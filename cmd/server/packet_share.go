package main

// #812 M2 — packet share previews: GET /p?packet=<16-hex hash> and its card,
// GET /api/packets/{hash}/card.png (internal/packetcard), the third card
// type after the terrain profile (#727) and the node (#811).
//
// Properties, each pinned by packet_share_test.go:
//   - Off unless share.packets.enabled (owner decision Q1). Off, /p?packet=
//     is the plain site document, as /p?node= is when node sharing is off,
//     and the card route is 404.
//   - The key is exactly 16 hex characters, either case; a numeric packet
//     id is refused. The only other keys are v, accepted and ignored,
//     viewPath (#812 M3), and the click-tracking keys /p already ignores;
//     anything else is a 400.
//   - #812 M3: viewPath=1 (exactly that) makes it a View Path link, the
//     link of the View Path modal (#/packets/<hash>?viewPath=1). It is the
//     same page with the same gate, lookup, visibility rule, tags and card
//     (one v, one cache entry); only og:url carries viewPath=1, and the
//     index.html handoff opens the modal.
//   - The packet is looked up as the packet page does: the in-memory store
//     first, then the DB. Unknown packets, and packets gone from both, are
//     the plain site document with a 404; nothing from the request is
//     echoed. The ping-history archive is not consulted.
//   - Every payload type gets a card (owner decision Q2, revised), named by
//     its firmware type only. Nothing of the payload is read except, for an
//     ADVERT, the origin's key and name: the origin is checked with
//     identityHidden over hiddenIdentityNames, and a hidden origin is the
//     plain site document with a 404. No message text, channel or sender
//     ever reaches a tag or the card.
//   - The path is getPacketPath's, then filterPingScorePath, the rule the
//     ping-score path uses: a branch through a hidden node or observer is
//     removed whole, a hidden first observation is dropped with the
//     measurements relative to it, approximate positions are stripped under
//     an active visibility policy, and the touched areas are recomputed from
//     what is left. A first observation whose observer has no visible branch
//     is dropped too (#852 review, finding 5): naming an observer the count
//     does not include would tell that one of its branches was removed.
//     Every count is computed from what is drawn.
//   - The first observer is named only through that filter (owner decision
//     Q3, revised); without it the card falls back to counts, and no other
//     observer is named in its place.
//   - v is a content hash of the card's input, which is exactly what the
//     card draws, plus the palette, the options and the renderer version.
//     The card cache is keyed by it, so a changed value is never served
//     from the cache.
//   - Observations keep arriving for minutes, then the packet is final: the
//     path cache and the card cache keep an entry 2 min while the packet was
//     first heard less than 10 min ago, 24 h after that, and the card's
//     Cache-Control says the same. The path cache keeps the result of the
//     one expensive step (getPacketPath); the visibility filter runs on every
//     request, so a blacklist or hidden-name change applies at once.
//   - Both caches are bounded (packetCardCacheEntries, and by bytes); a card
//     miss is charged to the per-IP limiter and drawn in the render slots of
//     the terrain card, or of the node card when only that one is on, so all
//     card types draw from one budget.
//   - #885: the budget is checked before the cold path lookup, on the card
//     and on /p alike. A path-cache miss is charged to the per-IP limiter,
//     then run once for every identical request in flight, in a render slot;
//     slots full is a 503 with the charge refunded. The same request's card
//     render is not charged again. A cached path costs nothing, and the
//     visibility filter still runs on every request, after the lookup.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/packetcard"
)

const (
	// packetCardRendererVersion is part of every v: bump it when the packet
	// card draws differently, so no crawler keeps a stale card.
	packetCardRendererVersion = "812m2.1"
	// packetCardCacheEntries bounds the card cache and the path cache
	// (#812 plan: about 64 each).
	packetCardCacheEntries = 64
	// packetPathCacheEntryMax bounds one cached path's JSON; a larger path
	// (a flood heard by hundreds of observers) is not cached and costs its
	// query again.
	packetPathCacheEntryMax = 256 << 10
	// packetFreshAge is how long after it was first heard a packet may
	// still gain observations (#812 plan, freshness).
	packetFreshAge     = 10 * time.Minute
	packetCardTTLFresh = 2 * time.Minute
	packetCardTTLFinal = 24 * time.Hour
)

// packetCards is the packet card's state, built once by initPacketCards.
type packetCards struct {
	names bool         // map.dhm.profile.card.names
	look  cardSettings // map.dhm.profile.card, published as share.card (#812 M4)
	pal   packetcard.Palette
	palFP string
	site  string // set by registerShareRoutes, before the server serves

	cards   *dhm.Cache[packetCardKey]
	paths   *dhm.Cache[string]
	flight  dhm.Flight[[]byte]
	admit   *dhm.Admission
	limiter *ipRateLimiter
	render  func(packetcard.PacketCardInput, packetcard.Options) ([]byte, error)
	now     func() time.Time
	// coldPath is the one expensive step of a path-cache miss (#885).
	coldPath func(hash string) (*PacketPathResponse, error)
}

// packetCardKey is a card's identity: the packet and what the card shows.
type packetCardKey struct {
	Hash, V string
}

func (k packetCardKey) String() string { return k.Hash + "|" + k.V }

// initPacketCards sets s.packetCards from the config: nil unless
// share.packets.enabled. The per-IP limiter and the render slots are the
// terrain card's when it exists, else the node card's, so every card type
// draws from one budget; call it again after replacing s.terrain or
// s.nodeCards.
func (s *Server) initPacketCards() {
	s.packetCards = nil
	if s.cfg == nil || !s.cfg.Share.Packets.Enabled {
		return
	}
	var set terrainSettings
	if s.terrain != nil {
		set = s.terrain.set
	} else {
		set = resolveTerrainSettings(s.cfg.DHM.Profile, log.Printf)
	}
	c := &packetCards{
		names:  set.card.names,
		look:   set.card,
		cards:  dhm.NewCache[packetCardKey](packetCardCacheEntries, packetCardCacheEntries*cardBytesPerEntry, packetCardTTLFinal),
		paths:  dhm.NewCache[string](packetCardCacheEntries, packetCardCacheEntries*packetPathCacheEntryMax, packetCardTTLFinal),
		render: packetcard.Render,
		now:    time.Now,
	}
	c.coldPath = s.coldPacketPath
	switch {
	case s.terrain != nil && s.terrain.card != nil:
		c.limiter, c.admit = s.terrain.limiter, s.terrain.card.admit
	case s.nodeCards != nil:
		c.limiter, c.admit = s.nodeCards.limiter, s.nodeCards.admit
	default:
		c.limiter = newIPRateLimiter("share", "map.dhm.profile.rateLimitPerMin", set.rateLimitPerMin, parseCIDRListQuiet(s.cfg.WSTrustedProxies()), log.Printf)
		c.admit = dhm.NewAdmission(cardRenderSlots())
	}
	// Read once, like the other cards' palettes and the og: tags.
	light, dark := cardThemeSections(s.cfg, LoadTheme("."))
	c.usePalette(light, dark, set.card.dark)
	s.packetCards = c
}

// usePalette resolves the card palette and warns once per unparseable key,
// naming the key only.
func (c *packetCards) usePalette(light, dark map[string]string, darkMode bool) {
	pal, bad := packetcard.ResolveTheme(light, dark, darkMode)
	for _, k := range bad {
		log.Printf("[share] packet card: theme colour %q is not a colour the card can read; using the default", k)
	}
	c.pal = pal
	sum := sha256.Sum256([]byte(fmt.Sprintf("%+v", pal)))
	c.palFP = hex.EncodeToString(sum[:8])
}

func (c *packetCards) options() packetcard.Options {
	return packetcard.Options{Palette: &c.pal, HideNames: !c.names, Site: c.site}
}

// packetCardTTL is how long a packet's path and card are kept: briefly
// while observations may still arrive, a day once the packet is final. An
// unknown or future first-heard time counts as fresh.
func packetCardTTL(firstHeard, now time.Time) time.Duration {
	if firstHeard.IsZero() || now.Sub(firstHeard) < packetFreshAge {
		return packetCardTTLFresh
	}
	return packetCardTTLFinal
}

func packetCardCacheControl(ttl time.Duration) string {
	return "public, max-age=" + strconv.Itoa(int(ttl/time.Second))
}

// ── The packet itself ──────────────────────────────────────────────────────

// packetShareMeta is all the share reads from the packet itself. Decoded is
// filled for an ADVERT only, for its origin; no other payload is read.
type packetShareMeta struct {
	PayloadType, RouteType int // -1 when unknown
	FirstSeen              string
	Decoded                string
}

// packetShareMeta is hash's entry in the store, copied under the read lock.
func (s *PacketStore) packetShareMeta(hash string) (packetShareMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tx := s.byHash[hash]
	if tx == nil {
		return packetShareMeta{}, false
	}
	m := packetShareMeta{PayloadType: -1, RouteType: -1, FirstSeen: tx.FirstSeen}
	if tx.PayloadType != nil {
		m.PayloadType = *tx.PayloadType
	}
	if tx.RouteType != nil {
		m.RouteType = *tx.RouteType
	}
	if m.PayloadType == payloadTypeAdvert {
		m.Decoded = tx.DecodedJSON
	}
	return m, true
}

// packetShareMeta is hash's transmission row: one indexed lookup.
func (db *DB) packetShareMeta(hash string) (packetShareMeta, bool, error) {
	var firstSeen, decoded sql.NullString
	var pt, rt sql.NullInt64
	err := db.conn.QueryRow(`SELECT first_seen, payload_type, route_type,
		CASE WHEN payload_type = ? THEN decoded_json END
		FROM transmissions WHERE hash = ?`, payloadTypeAdvert, hash).Scan(&firstSeen, &pt, &rt, &decoded)
	if errors.Is(err, sql.ErrNoRows) {
		return packetShareMeta{}, false, nil
	}
	if err != nil {
		return packetShareMeta{}, false, err
	}
	m := packetShareMeta{PayloadType: -1, RouteType: -1, FirstSeen: firstSeen.String, Decoded: decoded.String}
	if pt.Valid {
		m.PayloadType = int(pt.Int64)
	}
	if rt.Valid {
		m.RouteType = int(rt.Int64)
	}
	return m, true, nil
}

// lookupPacketShareMeta is the packet page's lookup: store first, then DB.
func (s *Server) lookupPacketShareMeta(hash string) (packetShareMeta, bool) {
	if s.store != nil {
		if m, ok := s.store.packetShareMeta(hash); ok {
			return m, true
		}
	}
	if s.db == nil {
		return packetShareMeta{}, false
	}
	m, ok, err := s.db.packetShareMeta(hash)
	if err != nil {
		log.Printf("[share] packet card: packet lookup failed; refused")
		return packetShareMeta{}, false
	}
	return m, ok
}

// sharedAdvertOrigin is the part of an ADVERT's decoded payload the share
// reads: the origin's key and name.
type sharedAdvertOrigin struct {
	PubKey string `json:"pubKey"`
	Name   string `json:"name"`
}

// ── Resolving a shared packet ──────────────────────────────────────────────

// packetShare is one visible packet, ready to describe.
type packetShare struct {
	in packetcard.PacketCardInput
	// areas are the touched areas' labels, from visible positions only.
	areas []string
	ttl   time.Duration
	// charged: this request's share of the per-IP budget was spent on a cold
	// path lookup, so the card render must not charge it again (#885).
	charged bool
}

// shareRefusal is why a request got no packet: status is 404 for an unknown
// or hidden packet (and a path that cannot be read, or a failed visibility
// lookup: fail closed), 429 with retry for a spent budget, 503 with retry
// for full slots, 0 for a client that left.
type shareRefusal struct {
	status int
	retry  time.Duration
}

var shareNotFound = &shareRefusal{status: http.StatusNotFound}

// resolvePacketShare looks a lower-case 16-hex hash up. A cold path lookup
// is charged to r's client and coalesced before it runs (#885); a cached
// path is free. The visibility filter runs on every request.
func (s *Server) resolvePacketShare(r *http.Request, c *packetCards, hash string) (packetShare, *shareRefusal) {
	ctx := r.Context()
	meta, ok := s.lookupPacketShareMeta(hash)
	if !ok || s.db == nil {
		return packetShare{}, shareNotFound
	}
	now := c.now()
	firstHeard, _ := parseObsTimestamp(meta.FirstSeen)
	in := packetcard.PacketCardInput{PayloadType: meta.PayloadType, RouteType: meta.RouteType, FirstHeard: firstHeard}
	if meta.PayloadType == payloadTypeAdvert {
		var o sharedAdvertOrigin
		if json.Unmarshal([]byte(meta.Decoded), &o) != nil || !isHexPubkey(strings.ToLower(o.PubKey)) {
			return packetShare{}, shareNotFound
		}
		pk := strings.ToLower(o.PubKey)
		names, err := s.hiddenIdentityNames(ctx, []string{pk})
		if err != nil || identityHidden(s.cfg, pk, append(names[pk], o.Name)...) {
			if err != nil {
				log.Printf("[share] packet card: origin visibility lookup failed; refused")
			}
			return packetShare{}, shareNotFound
		}
		if c.names {
			in.AdvertName = o.Name
		}
	}
	ttl := packetCardTTL(firstHeard, now)
	path, charged, refused := s.sharedPacketPath(r, c, hash, now, ttl)
	if refused != nil {
		return packetShare{}, refused
	}
	if _, err := s.filterPingScorePath(ctx, path); err != nil {
		log.Printf("[share] packet card: path visibility lookup failed; refused")
		return packetShare{}, shareNotFound
	}
	dropUncountedFirst(path)
	s.annotatePacketPathTouchedAreas(path)
	fillPacketCardPath(&in, path, c.names)
	ps := packetShare{in: in, ttl: ttl, charged: charged}
	for _, a := range path.TouchedAreas {
		ps.areas = append(ps.areas, a.Label)
	}
	return ps, nil
}

// coldPacketPath is hash's path as getPacketPath and the airtime annotation
// give it, before any filter: the work a path-cache miss costs.
func (s *Server) coldPacketPath(hash string) (*PacketPathResponse, error) {
	p, err := s.db.getPacketPath(hash, EstimateMaxEdgeKm, s.estimatedPositionsEnabled())
	if err != nil {
		return nil, err
	}
	s.annotatePacketPathAirtime(p)
	p.TouchedAreas = nil // config projections, recomputed after the filter
	return p, nil
}

// sharedPacketPath is hash's unfiltered path, from the path cache when it is
// there. On a miss it first charges r's client and takes a render slot, then
// runs the lookup once for every identical request in flight (#885), and
// gives the charge back when no slot is free. It returns a private copy:
// the caller filters it in place. charged says the request was charged.
func (s *Server) sharedPacketPath(r *http.Request, c *packetCards, hash string, now time.Time, ttl time.Duration) (path *PacketPathResponse, charged bool, refused *shareRefusal) {
	b, ok := c.paths.Get(hash, now)
	if !ok {
		if allowed, retry := c.limiter.allow(r); !allowed {
			return nil, false, &shareRefusal{status: http.StatusTooManyRequests, retry: retry}
		}
		charged = true
		var err error
		b, _, err = c.flight.DoAdmitted(r.Context(), "path|"+hash, 0, c.admit, func(context.Context) ([]byte, error) {
			if b, ok := c.paths.Get(hash, c.now()); ok { // filled while this call queued
				return b, nil
			}
			p, err := c.coldPath(hash)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(p)
			if err == nil && len(b) <= packetPathCacheEntryMax {
				c.paths.PutFor(hash, b, c.now(), ttl)
			}
			return b, err
		})
		switch {
		case err == nil:
		case r.Context().Err() != nil:
			return nil, charged, &shareRefusal{}
		case isTerrainBusy(err):
			c.limiter.refund(r)
			return nil, false, &shareRefusal{status: http.StatusServiceUnavailable, retry: terrainBusyRetry}
		default:
			log.Printf("[share] packet card: path unavailable; refused")
			return nil, charged, shareNotFound
		}
	}
	var p PacketPathResponse
	if err := json.Unmarshal(b, &p); err != nil {
		log.Printf("[share] packet card: path unavailable; refused")
		return nil, charged, shareNotFound
	}
	return &p, charged, nil
}

// dropUncountedFirst drops First, and the measurements relative to it, when
// its observer is not among the visible branches' observers. Every observer
// has a branch (its deepest observation), so a First without one means the
// filter removed that branch, for instance through a hidden relay, while the
// first observation itself was visible. Naming that observer while the count
// leaves it out would tell that a branch is missing (#852 review, finding
// 5); the card falls back to counts instead, and no other observer is
// promoted (owner decision Q3, revised).
func dropUncountedFirst(path *PacketPathResponse) {
	if path.First == nil {
		return
	}
	for _, b := range path.Branches {
		if sameObserver(path.First.Observer, b.Observer) {
			return
		}
	}
	path.First = nil
	for i := range path.Branches {
		path.Branches[i].SecondsAfterFirst, path.Branches[i].DistanceFromFirstKm = nil, nil
	}
}

// sameObserver: the same station, by key when both have one, else by name
// and IATA. An unknown observer matches nothing.
func sameObserver(a, b *PacketPathObserver) bool {
	if a == nil || b == nil {
		return false
	}
	if a.PublicKey != "" || b.PublicKey != "" {
		return strings.EqualFold(a.PublicKey, b.PublicKey)
	}
	return a.Name == b.Name && a.IATA == b.IATA
}

// fillPacketCardPath copies the filtered path into the card input: only
// positions, hop counts, the measurements from First and the area rings.
// No relay or observer name crosses, except First's observer when names are
// on.
func fillPacketCardPath(in *packetcard.PacketCardInput, path *PacketPathResponse, names bool) {
	for _, b := range path.Branches {
		in.Branches = append(in.Branches, cardBranch(b))
	}
	if f := path.First; f != nil {
		b := cardBranch(*f)
		in.First = &b
		if names && f.Observer != nil {
			in.FirstObserverName = f.Observer.Name
		}
	}
	for _, a := range path.TouchedAreas {
		var ring []packetcard.LatLon
		switch {
		case len(a.Polygon) > 0:
			for _, v := range a.Polygon {
				ring = append(ring, packetcard.LatLon{Lat: v[0], Lon: v[1]})
			}
		case a.LatMin != nil && a.LatMax != nil && a.LonMin != nil && a.LonMax != nil:
			ring = packetcard.BoxRing(*a.LatMin, *a.LatMax, *a.LonMin, *a.LonMax)
		}
		if len(ring) > 0 {
			in.Areas = append(in.Areas, packetcard.Area{Ring: ring})
		}
	}
	in.AirtimeMs = path.EstimatedAirtimeMs
}

func cardBranch(b PacketPathBranch) packetcard.Branch {
	out := packetcard.Branch{Hops: b.Hops, SecondsAfterFirst: b.SecondsAfterFirst, DistanceFromFirstKm: b.DistanceFromFirstKm}
	for _, p := range b.Points {
		if p.Lat != nil && p.Lon != nil {
			out.Points = append(out.Points, packetcard.LatLon{Lat: *p.Lat, Lon: *p.Lon})
		}
	}
	if o := b.Observer; o != nil && o.Lat != nil && o.Lon != nil {
		out.Observer = &packetcard.LatLon{Lat: *o.Lat, Lon: *o.Lon}
	}
	return out
}

// ── What the card and the page say ─────────────────────────────────────────

// packetTitle is "GRP_TXT · Flood · 6 hops · heard by 12": the card's title
// and its observer count.
func packetTitle(in packetcard.PacketCardInput) string {
	n := packetcard.Observers(in)
	if n == 0 {
		return packetcard.Title(in) + " · no visible observers"
	}
	return packetcard.Title(in) + " · heard by " + strconv.Itoa(n)
}

// packetNames are the card's names as text: "Advert from X" and "First
// heard by Y", each only when the card draws it.
func packetNames(in packetcard.PacketCardInput) []string {
	var out []string
	if in.PayloadType == payloadTypeAdvert {
		if n := ogClip(in.AdvertName, shareNameMaxRunes); n != "" {
			out = append(out, "Advert from "+n)
		}
	}
	if in.First != nil {
		if n := ogClip(in.FirstObserverName, shareNameMaxRunes); n != "" {
			out = append(out, "First heard by "+n)
		}
	}
	return out
}

// packetStats are the card's stat tiles as text ("Spread: 4.2s"). The
// airtime "≥" stays: it is a lower bound.
func packetStats(in packetcard.PacketCardInput) []string {
	var out []string
	for _, st := range packetcard.Stats(in) {
		out = append(out, st.Label+": "+st.Value)
	}
	return out
}

func packetAreasClause(areas []string) string {
	if len(areas) == 0 {
		return ""
	}
	return "Areas: " + strings.Join(areas, ", ")
}

// positioned reports whether the card draws a map: some visible point or
// observer has a position.
func positioned(in packetcard.PacketCardInput) bool {
	has := func(b packetcard.Branch) bool { return len(b.Points) > 0 || b.Observer != nil }
	for _, b := range in.Branches {
		if has(b) {
			return true
		}
	}
	return in.First != nil && has(*in.First)
}

// version is v: a short hash of the card's input, which is exactly what it
// draws, and of everything else the drawing depends on.
func (c *packetCards) version(in packetcard.PacketCardInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%t|%q\n", packetCardRendererVersion, c.palFP, c.names, c.site)
	b, _ := json.Marshal(in)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// packetCardPath is the card's path and query, built from the parsed hash
// only.
func packetCardPath(hash, v string) string {
	return "/api/packets/" + hash + "/card.png?v=" + v
}

// page is the preview of a shared packet. A View Path link differs only in
// og:url: it shares the card, its v and its cache entry.
func (c *packetCards) page(sp sharedPacket, ps packetShare) ogPage {
	hash := sp.Hash
	link := sharePath + "?packet=" + hash
	if sp.ViewPath {
		link += "&viewPath=1"
	}
	in := ps.in
	title := packetTitle(in)
	desc := append(packetNames(in), packetStats(in)...)
	if a := packetAreasClause(ps.areas); a != "" {
		desc = append(desc, a)
	}
	alt := "Packet path schematic: " + title + "."
	if !positioned(in) {
		alt = "Packet card: " + title + ". No station on this path has an advertised position."
	}
	if n := packetNames(in); len(n) > 0 {
		alt += " " + strings.Join(n, ". ") + "."
	}
	if st := packetStats(in); len(st) > 0 {
		alt += " " + strings.Join(st, ". ") + "."
	}
	if a := packetAreasClause(ps.areas); a != "" {
		alt += " " + a + "."
	}
	return ogPage{
		Title:       title,
		Description: strings.Join(desc, " · "),
		Path:        link,
		Image:       packetCardPath(hash, c.version(in)),
		ImageAlt:    alt,
	}
}

// ── Requests ───────────────────────────────────────────────────────────────

// sharedPacket is a parsed /p?packet= link.
type sharedPacket struct {
	Hash string // lower case
	// ViewPath marks the View Path modal's link (#812 M3): the same
	// preview, handed off to #/packets/<hash>?viewPath=1.
	ViewPath bool
}

// parseSharedPacket is the /p?packet= grammar: packet, exactly 16 hex
// characters of either case, once; viewPath, exactly "1", once; v once, at
// most 64 bytes, ignored; tracking keys ignored; nothing else.
func parseSharedPacket(q url.Values) (sharedPacket, bool) {
	for k, vs := range q {
		if isShareTrackingParam(k) {
			continue
		}
		if (k != "packet" && k != "v" && k != "viewPath") || len(vs) != 1 || len(vs[0]) > 64 {
			return sharedPacket{}, false
		}
	}
	if vp, ok := q["viewPath"]; ok && vp[0] != "1" {
		return sharedPacket{}, false
	}
	sp := sharedPacket{Hash: strings.ToLower(q.Get("packet")), ViewPath: q.Has("viewPath")}
	return sp, hashPattern.MatchString(sp.Hash)
}

// handleSharePacket is /p with a packet hash.
func (s *Server) handleSharePacket(doc *indexDoc, w http.ResponseWriter, r *http.Request) {
	c := s.packetCards
	if c == nil {
		doc.serveSite(w, r, http.StatusOK)
		return
	}
	sp, ok := parseSharedPacket(r.URL.Query())
	if !ok {
		doc.serveSite(w, r, http.StatusBadRequest)
		return
	}
	ps, refused := s.resolvePacketShare(r, c, sp.Hash)
	if refused != nil {
		if refused.status == 0 {
			return
		}
		if refused.retry > 0 {
			setRetryAfter(w, refused.retry)
		}
		doc.serveSite(w, r, refused.status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(doc.og.renderProfile(doc.profile, r, c.page(sp, ps)))
}

// handlePacketCard is GET /api/packets/{hash}/card.png.
func (s *Server) handlePacketCard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	c := s.packetCards
	if c == nil {
		refuseCard(w, http.StatusNotFound, "not found")
		return
	}
	hash := strings.ToLower(mux.Vars(r)["hash"])
	if !hashPattern.MatchString(hash) || !nodeCardQueryOK(r.URL.Query()) {
		refuseCard(w, http.StatusBadRequest, "bad request")
		return
	}
	ps, refused := s.resolvePacketShare(r, c, hash)
	if refused != nil {
		switch refused.status {
		case 0:
		case http.StatusTooManyRequests:
			markCardRefused(w)
			writeRateLimited(w, refused.retry)
		case http.StatusServiceUnavailable:
			setRetryAfter(w, refused.retry)
			refuseCard(w, refused.status, "card renders busy")
		default:
			refuseCard(w, refused.status, "not found")
		}
		return
	}
	cc := packetCardCacheControl(ps.ttl)
	key := packetCardKey{Hash: hash, V: c.version(ps.in)}
	if b, ok := c.cards.Get(key, c.now()); ok {
		writeCardAs(w, b, "hit", cc)
		return
	}
	if !ps.charged { // a cold path lookup was this request's charge (#885)
		if ok, retry := c.limiter.allow(r); !ok {
			markCardRefused(w)
			writeRateLimited(w, retry)
			return
		}
	}
	b, _, err := c.flight.DoAdmitted(r.Context(), key.String(), 0, c.admit, func(context.Context) ([]byte, error) {
		out, err := c.render(ps.in, c.options())
		if err == nil {
			c.cards.PutFor(key, out, c.now(), ps.ttl)
		}
		return out, err
	})
	switch {
	case err == nil:
		writeCardAs(w, b, "miss", cc)
	case r.Context().Err() != nil:
	case isTerrainBusy(err):
		c.limiter.refund(r)
		setRetryAfter(w, terrainBusyRetry)
		refuseCard(w, http.StatusServiceUnavailable, "card renders busy")
	default:
		refuseCard(w, http.StatusInternalServerError, "internal error")
	}
}
