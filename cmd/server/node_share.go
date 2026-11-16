package main

// #811 M3 — node share previews: GET /p?node=<64-hex pubkey> and its card,
// GET /api/nodes/{pubkey}/card.png (internal/nodecard), the second card
// type after the terrain profile (#727).
//
// Properties, each pinned by node_share_test.go:
//   - Off unless share.nodes.enabled (owner decision Q2). Off, /p?node= is
//     the plain site document, as /p is when profile sharing is off, and
//     the card route is 404.
//   - The key is exactly 64 hex characters, either case (Q4: no #772
//     prefixes). The only other keys are v, accepted and ignored, and the
//     click-tracking keys /p already ignores; anything else is a 400.
//   - Visibility is identityHidden over hiddenIdentityNames, the rule the
//     terrain profile's node endpoints use (blacklist, observer blacklist,
//     hidden-name prefixes on node, inactive-node and observer names). A
//     hidden, unknown or inactive node is the plain site document with a
//     404 and the card route's 404; nothing from the request is echoed.
//   - Cards for repeaters, rooms and sensors only (Q3). A companion is the
//     plain site document (200: the browser still lands on its page) and
//     has no card.
//   - The area is the configured area the node's position is in, the same
//     matching as the rest of the site (AreaForPoint); without a position
//     or outside every area there is no area clause (Q5). No coordinate
//     reaches a tag or the card.
//   - Every value comes from a snapshot the server already keeps: the node
//     snapshot (getCachedNodesAndPM, indexed here by key), the published
//     relay snapshot and the published neighbour graph. A card never scans
//     the DB or the packet store and never builds a snapshot inline: a
//     snapshot that is not there yet drops its clauses. The work per
//     uncached card is O(1) in the size of the network, plus the render.
//   - v is a content hash of what the card shows: last heard as its
//     relative wording ("3h ago"), every count as drawn. The card cache is
//     keyed by it, so a changed value is never served from the cache.
//   - The card cache is bounded (nodeCardCacheEntries, and by bytes); a
//     miss is charged to the per-IP limiter and drawn in the render slots
//     of the terrain card when that card exists (shared budget), else in
//     an equivalent pair of its own.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/nodecard"
)

const (
	// nodeCardRendererVersion is part of every v: bump it when the node
	// card draws differently, so no crawler keeps a stale card.
	nodeCardRendererVersion = "811m3.1"
	// nodeCardCacheEntries bounds the node card cache (#811 plan: about
	// 64); the byte bound is cardBytesPerEntry per entry, like the terrain
	// card's.
	nodeCardCacheEntries = 64
	// nodeCardCacheTTL is short: a node's stats move, a terrain does not.
	nodeCardCacheTTL     = 10 * time.Minute
	nodeCardCacheControl = "public, max-age=600"
)

// nodeCards is the node card's state, built once by initNodeCards.
type nodeCards struct {
	names bool // map.dhm.profile.card.names
	pal   nodecard.Palette
	palFP string
	site  string // set by registerShareRoutes, before the server serves

	cache   *dhm.Cache[nodeCardKey]
	flight  dhm.Flight[[]byte]
	admit   *dhm.Admission
	limiter *ipRateLimiter
	render  func(nodecard.NodeCardInput, nodecard.Options) ([]byte, error)
	now     func() time.Time

	index nodeKeyIndex
}

// nodeCardKey is a card's identity: the node and what the card shows.
type nodeCardKey struct {
	PubKey, V string
}

func (k nodeCardKey) String() string { return k.PubKey + "|" + k.V }

// initNodeCards sets s.nodeCards from the config: nil unless
// share.nodes.enabled. With the terrain card on, the per-IP limiter and
// the render slots are that card's, so both card types draw from one
// budget; call it again after replacing s.terrain.
func (s *Server) initNodeCards() {
	s.nodeCards = nil
	if s.cfg == nil || !s.cfg.Share.Nodes.Enabled {
		return
	}
	var set terrainSettings
	if s.terrain != nil {
		set = s.terrain.set
	} else {
		set = resolveTerrainSettings(s.cfg.DHM.Profile, log.Printf)
	}
	c := &nodeCards{
		names:  set.card.names,
		cache:  dhm.NewCache[nodeCardKey](nodeCardCacheEntries, nodeCardCacheEntries*cardBytesPerEntry, nodeCardCacheTTL),
		render: nodecard.Render,
		now:    time.Now,
	}
	if p := s.terrain; p != nil && p.card != nil {
		c.limiter, c.admit = p.limiter, p.card.admit
	} else {
		c.limiter = newIPRateLimiter("share", "map.dhm.profile.rateLimitPerMin", set.rateLimitPerMin, parseCIDRListQuiet(s.cfg.WSTrustedProxies()), log.Printf)
		c.admit = dhm.NewAdmission(cardRenderSlots())
	}
	// Read once, like the terrain card's palette and the og: tags.
	theme := LoadTheme(".")
	light, dark := cardThemeSections(s.cfg, theme)
	roles := map[string]string{}
	putStrings(roles, s.cfg.NodeColors)
	putStrings(roles, theme.NodeColors)
	c.usePalette(light, dark, roles, set.card.dark)
	s.nodeCards = c
}

// usePalette resolves the card palette and warns once per unparseable key,
// naming the key only.
func (c *nodeCards) usePalette(light, dark, roles map[string]string, darkMode bool) {
	pal, bad := nodecard.ResolveTheme(light, dark, roles, darkMode)
	for _, k := range bad {
		log.Printf("[share] node card: theme colour %q is not a colour the card can read; using the default", k)
	}
	c.pal = pal
	sum := sha256.Sum256([]byte(fmt.Sprintf("%+v", pal)))
	c.palFP = hex.EncodeToString(sum[:8])
}

func (c *nodeCards) options() nodecard.Options {
	return nodecard.Options{Palette: &c.pal, HideNames: !c.names, Site: c.site}
}

// ── The node snapshot, by key ──────────────────────────────────────────────

// nodeKeyIndex maps a lower-case key to its position in one node snapshot.
// It is rebuilt when the snapshot changes (at most once per snapshot
// refresh, every 30 s), so a lookup is O(1) between refreshes. It holds
// one entry per node of the snapshot it indexes and nothing else.
type nodeKeyIndex struct {
	mu   sync.Mutex
	base *nodeInfo
	n    int
	m    map[string]int
}

func (x *nodeKeyIndex) find(nodes []nodeInfo, pk string) (nodeInfo, bool) {
	if len(nodes) == 0 {
		return nodeInfo{}, false
	}
	x.mu.Lock()
	if x.base != &nodes[0] || x.n != len(nodes) {
		m := make(map[string]int, len(nodes))
		for i := range nodes {
			m[strings.ToLower(nodes[i].PublicKey)] = i
		}
		x.base, x.n, x.m = &nodes[0], len(nodes), m
	}
	i, ok := x.m[pk]
	x.mu.Unlock()
	if !ok {
		return nodeInfo{}, false
	}
	return nodes[i], true
}

// ── Resolving a shared node ────────────────────────────────────────────────

// nodeShare is one visible node, ready to describe.
type nodeShare struct {
	in nodecard.NodeCardInput
	// carded is false for a role without a card (a companion).
	carded bool
}

// resolveNodeShare looks a lower-case 64-hex key up. found is false for a
// hidden, unknown or inactive node, and for a failed visibility lookup
// (fail closed). The visibility lookup runs for every key, known or not,
// so the work done does not tell a hidden node from a missing one.
func (s *Server) resolveNodeShare(ctx context.Context, c *nodeCards, pk string) (nodeShare, bool) {
	names, err := s.hiddenIdentityNames(ctx, []string{pk})
	if err != nil || identityHidden(s.cfg, pk, names[pk]...) {
		if err != nil {
			log.Printf("[share] node card: node visibility lookup failed; refused")
		}
		return nodeShare{}, false
	}
	if s.store == nil {
		return nodeShare{}, false
	}
	nodes, _ := s.store.getCachedNodesAndPM()
	n, ok := c.index.find(nodes, pk)
	if !ok || identityHidden(s.cfg, pk, n.Name) {
		return nodeShare{}, false
	}
	in := nodecard.NodeCardInput{PubKey: pk, Name: n.Name, Role: n.Role, LastHeard: n.LastSeen, AsOf: c.now()}
	if !nodecard.Carded(n.Role) {
		return nodeShare{in: in}, true
	}
	if n.HasGPS && isValidNodePosition(n.Lat, n.Lon) {
		if label, ok := AreaForPoint(n.Lat, n.Lon, s.cfg.Areas); ok {
			in.Area = label
		}
	}
	if n.Role == "repeater" || n.Role == "room" {
		if info, ok := s.store.publishedRelayInfo(pk); ok {
			in.Relay = &nodecard.RelayStats{
				Relayed24h: &info.RelayCount24h, Relayed1h: &info.RelayCount1h,
				AirtimePct24h: &info.EstAirtimePct24h, AirtimePeakPct: &info.EstAirtimePeakPct,
			}
		}
	}
	if g := s.neighborGraph.Load(); g != nil {
		k := nodeNeighborCount(g, pk, in.AsOf)
		in.Neighbors = &k
	}
	return nodeShare{in: in, carded: true}, true
}

// publishedRelayInfo is pk's entry in the published relay snapshot (the
// one /api/nodes reads), without ever computing it: before the first
// snapshot there is none.
func (s *PacketStore) publishedRelayInfo(pk string) (RepeaterRelayInfo, bool) {
	s.repeaterEnrichMu.Lock()
	info, ok := s.repeaterRelayCache[pk]
	s.repeaterEnrichMu.Unlock()
	return info, ok
}

// nodeNeighborCount is the number of rows the node page's neighbour list
// shows: GET /api/nodes/{pk}/neighbors with its defaults (every edge,
// ambiguous ones included), after the same prefix de-duplication. It is
// O(degree²) in the node's own edges, never in the network.
func nodeNeighborCount(g *NeighborGraph, pk string, now time.Time) int {
	var entries []NeighborEntry
	for _, e := range g.Neighbors(pk) {
		if e.Count < 1 || e.Score(now) < 0 {
			continue
		}
		entry := NeighborEntry{Prefix: e.Prefix}
		other := e.NodeA
		if strings.EqualFold(other, pk) {
			other = e.NodeB
		}
		if !e.Ambiguous && other != "" {
			o := other
			entry.Pubkey = &o
		}
		entries = append(entries, entry)
	}
	return len(dedupPrefixEntries(entries))
}

// ── What the card and the page say ─────────────────────────────────────────

// title is the node's name as the card draws it (or its fallback) and its
// role: "DK_7800_Kjeldbjerg · Repeater".
func (c *nodeCards) title(in nodecard.NodeCardInput) string {
	name := ""
	if c.names {
		name = ogClip(in.Name, shareNameMaxRunes)
	}
	if name == "" {
		name = nodecard.FallbackTitle(in.PubKey)
	}
	return name + " · " + nodecard.RoleLabel(in.Role)
}

// clauses are the area and the card's stat clauses as text ("Last heard:
// 3h ago"), in the card's order. A trace airtime, "<0.1%" on the card, is
// "under 0.1%" here: ogClip drops angle brackets, and "0.1%" would
// overstate it.
func clauses(in nodecard.NodeCardInput) []string {
	var out []string
	if in.Area != "" {
		out = append(out, in.Area)
	}
	for _, cl := range nodecard.Clauses(in) {
		v := cl.Value
		if strings.HasPrefix(v, "<") {
			v = "under " + v[1:]
		}
		out = append(out, cl.Label+": "+v)
	}
	return out
}

// version is v: a short hash of everything the card shows except the
// absolute times (the as-of footer and last heard's UTC detail), which
// follow the relative wording. It changes exactly when a displayed value,
// the palette, the options or the renderer change.
func (c *nodeCards) version(in nodecard.NodeCardInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%t|%q\n", nodeCardRendererVersion, c.palFP, c.names, c.site)
	fmt.Fprintf(h, "%s|%q|%s|%q\n", in.PubKey, in.Name, in.Role, in.Area)
	for _, cl := range nodecard.Clauses(in) {
		fmt.Fprintf(h, "%q|%q|%t\n", cl.Label, cl.Value, cl.Over)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// cardPath is the card's path and query, built from the parsed key only.
func nodeCardPath(pk, v string) string {
	return "/api/nodes/" + pk + "/card.png?v=" + v
}

// page is the preview of a carded node.
func (c *nodeCards) page(in nodecard.NodeCardInput) ogPage {
	title := c.title(in)
	cl := clauses(in)
	desc := nodecard.RoleLabel(in.Role)
	if len(cl) > 0 {
		desc = strings.Join(cl, " · ")
	}
	alt := "Node card: " + title
	if len(cl) > 0 {
		alt += ". " + strings.Join(cl, ". ")
	}
	return ogPage{
		Title:       title,
		Description: desc,
		Path:        sharePath + "?node=" + in.PubKey,
		Image:       nodeCardPath(in.PubKey, c.version(in)),
		ImageAlt:    alt,
	}
}

// ── Requests ───────────────────────────────────────────────────────────────

// parseSharedNode is the /p?node= grammar: node, exactly 64 hex characters
// of either case, once; v once, at most 64 bytes, ignored; tracking keys
// ignored; nothing else. It returns the key in lower case.
func parseSharedNode(q url.Values) (string, bool) {
	for k, vs := range q {
		if isShareTrackingParam(k) {
			continue
		}
		if (k != "node" && k != "v") || len(vs) != 1 || len(vs[0]) > 64 {
			return "", false
		}
	}
	pk := strings.ToLower(q.Get("node"))
	return pk, isHexPubkey(pk)
}

// handleShareNode is /p with a node key.
func (s *Server) handleShareNode(doc *indexDoc, w http.ResponseWriter, r *http.Request) {
	c := s.nodeCards
	if c == nil {
		doc.serveSite(w, r, http.StatusOK)
		return
	}
	pk, ok := parseSharedNode(r.URL.Query())
	if !ok {
		doc.serveSite(w, r, http.StatusBadRequest)
		return
	}
	ns, found := s.resolveNodeShare(r.Context(), c, pk)
	if !found {
		doc.serveSite(w, r, http.StatusNotFound)
		return
	}
	if !ns.carded {
		doc.serveSite(w, r, http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(doc.og.renderProfile(doc.profile, r, c.page(ns.in)))
}

// handleNodeCard is GET /api/nodes/{pubkey}/card.png.
func (s *Server) handleNodeCard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	c := s.nodeCards
	if c == nil {
		refuseCard(w, http.StatusNotFound, "not found")
		return
	}
	pk := strings.ToLower(mux.Vars(r)["pubkey"])
	if !isHexPubkey(pk) || !nodeCardQueryOK(r.URL.Query()) {
		refuseCard(w, http.StatusBadRequest, "bad request")
		return
	}
	ns, found := s.resolveNodeShare(r.Context(), c, pk)
	if !found || !ns.carded {
		refuseCard(w, http.StatusNotFound, "not found")
		return
	}
	key := nodeCardKey{PubKey: pk, V: c.version(ns.in)}
	if b, ok := c.cache.Get(key, c.now()); ok {
		writeCardAs(w, b, "hit", nodeCardCacheControl)
		return
	}
	if ok, retry := c.limiter.allow(r); !ok {
		markCardRefused(w)
		writeRateLimited(w, retry)
		return
	}
	b, _, err := c.flight.DoAdmitted(r.Context(), key.String(), 0, c.admit, func(context.Context) ([]byte, error) {
		out, err := c.render(ns.in, c.options())
		if err == nil {
			c.cache.Put(key, out, c.now())
		}
		return out, err
	})
	switch {
	case err == nil:
		writeCardAs(w, b, "miss", nodeCardCacheControl)
	case r.Context().Err() != nil:
	case isTerrainBusy(err):
		c.limiter.refund(r)
		setRetryAfter(w, terrainBusyRetry)
		refuseCard(w, http.StatusServiceUnavailable, "card renders busy")
	default:
		refuseCard(w, http.StatusInternalServerError, "internal error")
	}
}

// nodeCardQueryOK: the card URL carries v only (at most once, 64 bytes),
// which is accepted and ignored.
func nodeCardQueryOK(q url.Values) bool {
	for k, vs := range q {
		if k != "v" || len(vs) != 1 || len(vs[0]) > 64 {
			return false
		}
	}
	return true
}
