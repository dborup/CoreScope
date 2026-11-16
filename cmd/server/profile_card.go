package main

// #727 M3 — GET /api/terrain/profile.png: the 1200×630 share card of a
// terrain profile (internal/profilecard), and the og:image of /p.
//
// A request meets, in order (each pinned by profile_card_test.go):
//   - the share switch: 404 unless map.dhm.profile.share.enabled;
//   - the API grammar (parseTerrainQuery) plus v, a preview cache-bust
//     that is accepted and ignored;
//   - resolveTerrainRequest, the prelude of /api/terrain/profile and /p:
//     hidden (#743/#747), unknown and unpositioned nodes are the same 404,
//     before any charge, upstream call or render, so a name reaches the
//     card only through that gate;
//   - the card cache (hit: 200, no charge), bounded by entries and bytes;
//   - the per-IP budget (map.dhm.profile.rateLimitPerMin), charged on a
//     card miss: a render costs CPU even when the elevations are cached;
//   - the elevation cache; on a miss the server-wide share budget
//     (share.upstreamPerMin) and then the shared fetch (p.flight under
//     p.admit, the same maxInFlight as the panel's profiles). A spent
//     budget, an upstream failure or a slow upstream gives a degraded
//     card: 200, max-age 60, no verdict. A slow fetch is not cut short: it
//     runs on to the profile deadline, held in its maxInFlight slot, and
//     its profile is cached for the next fetch. A crawler that got an error would
//     keep "no preview" for a long time; a degraded card refreshes itself;
//   - the render: one per card key at a time (c.flight), at most
//     cardRenderSlots at once (c.admit).
//
// maxInFlight or render admission full is 503 with Retry-After, and gives
// back both the per-IP charge and the share token: a refusal did no work.
// A request that joins a fetch already running gives its token back too.
//
// /p reads the same caches but never fetches (share_profile.go): one
// shared link costs at most one upstream profile, spent by the card.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/profilecard"
)

const (
	// cardRoute is the share card's path.
	cardRoute = "/api/terrain/profile.png"
	// cardRendererVersion is part of every card key and v: bump it when
	// the renderer draws differently, so no stale card is served or kept
	// by a crawler.
	cardRendererVersion = "846.1"
	// cardBytesPerEntry is the byte bound per cached card. Real cards are
	// 80–94 KB (#755 review); 128 KiB leaves room for an unusual one, and
	// the default 64 entries keep the plan's ~8 MiB (#727 plan §3.2).
	cardBytesPerEntry       = 128 << 10
	cardCacheEntriesDefault = 64
	cardCacheEntriesMax     = 1024
	cardCacheTTL            = 24 * time.Hour
	// cardShareWait is how long a card waits for upstream before it is
	// served degraded: an image fetch is more patient than an HTML one,
	// but a crawler's is not endless (#727 plan §3.3). The fetch itself
	// runs on to the profile deadline and is cached (cardElevation).
	cardShareWait = 10 * time.Second
	// cardRenderSlotsMax bounds concurrent renders (~10 ms of CPU each).
	cardRenderSlotsMax = 4
	// cardDegradedLogEvery throttles the spent-budget log line.
	cardDegradedLogEvery = time.Minute

	cardCacheComplete = "public, max-age=86400, immutable"
	cardCacheDegraded = "public, max-age=60"
)

// cardSettings are map.dhm.profile.card, resolved.
type cardSettings struct {
	dark         bool // theme "dark" (default) or "light"
	names        bool // node names on the card (default true)
	cacheEntries int
}

func cardTheme(v string, logf func(string, ...interface{})) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "dark":
		return true
	case "light":
		return false
	}
	// The value is not echoed: config content does not belong in a log.
	logf("[dhm] config map.dhm.profile.card.theme must be dark or light; using dark")
	return true
}

// cardRenderSlots is the render admission: one per CPU, at most
// cardRenderSlotsMax.
func cardRenderSlots() int {
	n := runtime.GOMAXPROCS(0)
	if n > cardRenderSlotsMax {
		n = cardRenderSlotsMax
	}
	if n < 1 {
		n = 1
	}
	return n
}

// cardKey is a card's identity: the elevations it was drawn from, and a
// digest of everything else the picture depends on (cardInputs).
type cardKey struct {
	terrainKey
	Inputs [16]byte
}

func (k cardKey) String() string {
	return k.terrainKey.String() + "|" + hex.EncodeToString(k.Inputs[:])
}

// newCardCache is the rendered-card cache: entries and bytes bounded.
func newCardCache(entries int, ttl time.Duration) *dhm.Cache[cardKey] {
	return dhm.NewCache[cardKey](entries, entries*cardBytesPerEntry, ttl)
}

// profileCards is the share card's state. pal, palFP and site are set at
// startup, before the server serves.
type profileCards struct {
	set    cardSettings
	cache  *dhm.Cache[cardKey]
	flight dhm.Flight[[]byte] // one render per card key at a time
	admit  *dhm.Admission     // concurrent renders
	budget *shareBudget
	wait   time.Duration
	render func(profilecard.TerrainProfileResponse, [2]string, profilecard.Palette, profilecard.Options) ([]byte, error)
	logf   func(string, ...interface{})

	degraded    atomic.Int64 // degraded cards for a spent budget, since start
	degradedLog logThrottle

	pal   profilecard.Palette
	palFP string
	site  string // host shown after the credit; "" without publicBaseUrl
}

func newProfileCards(set terrainSettings, theme func() (light, dark map[string]string), logf func(string, ...interface{})) *profileCards {
	c := &profileCards{
		set:    set.card,
		cache:  newCardCache(set.card.cacheEntries, cardCacheTTL),
		admit:  dhm.NewAdmission(cardRenderSlots()),
		budget: newShareBudget(set.share.upstreamPerMin),
		wait:   cardShareWait,
		render: profilecard.Render,
		logf:   logf,
	}
	var light, dark map[string]string
	if theme != nil {
		light, dark = theme()
	}
	c.usePalette(light, dark)
	return c
}

// usePalette resolves the card palette from theme sections, as the
// browser does for the card's theme (profilecard.ResolveTheme), and warns
// once per unparseable key, naming the key only.
func (c *profileCards) usePalette(light, dark map[string]string) {
	pal, bad := profilecard.ResolveTheme(light, dark, c.set.dark)
	for _, k := range bad {
		c.logf("[dhm] share card: theme colour %q is not a colour the card can read; using the default", k)
	}
	c.pal = pal
	sum := sha256.Sum256([]byte(fmt.Sprintf("%+v", pal)))
	c.palFP = hex.EncodeToString(sum[:8])
}

// cardThemeSections are the theme and themeDark sections that
// /api/config/theme serves (shipped defaults, then config.json, then
// theme.json), as strings: theme sections are untyped JSON, and only a
// string can be a colour, so the conversion happens here, at the boundary.
func cardThemeSections(cfg *Config, theme *ThemeFile) (light, dark map[string]string) {
	if cfg == nil {
		cfg = &Config{}
	}
	if theme == nil {
		theme = &ThemeFile{}
	}
	light, dark = map[string]string{}, map[string]string{}
	putStrings(light, themeLightDefaults)
	putStrings(light, cfg.Theme)
	putStrings(light, theme.Theme)
	putStrings(dark, themeDarkDefaults)
	putStrings(dark, cfg.ThemeDark)
	putStrings(dark, theme.ThemeDark)
	return light, dark
}

// putStrings copies the string values of src into dst, overwriting.
func putStrings[V any](dst map[string]string, src map[string]V) {
	for k, v := range src {
		if s, ok := any(v).(string); ok {
			dst[k] = s
		}
	}
}

func (c *profileCards) options() profilecard.Options {
	return profilecard.Options{HideNames: !c.set.names, Site: c.site}
}

// cardNames are the names the card may draw: a node's, which
// resolveTerrainRequest only sets for an identity that passed the
// visibility gate; never a point's.
func cardNames(req terrainRequest) [2]string {
	var out [2]string
	for i, e := range req.ends {
		if e.kind == "node" {
			out[i] = e.name
		}
	}
	return out
}

// cardInputs digests everything but the elevations that the picture
// depends on: the renderer and palette, the display options, both
// endpoints as drawn, the resolved antenna heights and radio. A cached
// dsm-estimate (#789) is part of the resolved height, so v changes when
// the estimate changes what is drawn. classes is the building
// classification the card is drawn with (cardClasses, #846): its classes
// and whether the footprints are credited can change under the same
// elevation key, when a building retry ends differently (#797).
func (p *terrainProfiler) cardInputs(req terrainRequest, names [2]string, classes []byte) [16]byte {
	c := p.card
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%+v|%t|%x\n", cardRendererVersion, c.palFP, c.options(), req.q.both, classes)
	overrides := [2]*float64{req.q.ha, req.q.hb}
	for i, e := range req.ends {
		ant, src, assumed := p.antenna(e, overrides[i])
		fmt.Fprintf(h, "%s|%q|%s|%v|%s|%t\n", e.kind, names[i], e.role, ant, src, assumed)
	}
	freq, freqSrc, k, kSrc := p.linkParams(req.q)
	fmt.Fprintf(h, "%v|%s|%v|%s\n", freq, freqSrc, k, kSrc)
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// parseCardQuery is parseTerrainQuery plus v: one value of at most 64
// bytes, accepted and ignored.
func parseCardQuery(q url.Values) (terrainQuery, error) {
	api := url.Values{}
	for k, vs := range q {
		if k == "v" {
			if len(vs) != 1 || len(vs[0]) > 64 {
				return terrainQuery{}, errTerrainQuery
			}
			continue
		}
		api[k] = vs
	}
	return parseTerrainQuery(api)
}

// cardKey is the card's identity for a resolved request, so only for
// ends that passed the visibility gate, drawn with classes (cardClasses).
func (p *terrainProfiler) cardKey(req terrainRequest, classes []byte) cardKey {
	return cardKey{terrainKey: p.elevationKey(req), Inputs: p.cardInputs(req, cardNames(req), classes)}
}

// cardClasses is the classification part of a card's identity (#846):
// the classification as cached (terrainClasses.appendTo: state, flags and
// one class per sample), nil for a profile without one. Every class and
// so whether the footprints are credited is in it, and no position.
func (e terrainElevation) cardClasses() []byte {
	if e.classes == nil {
		return nil
	}
	return e.classes.appendTo(nil)
}

// cachedCardClasses is cardClasses of the profile a card for req would be
// drawn from, read off the elevation cache without decoding it, so the
// card cache can be asked before any charge. A profile this profiler does
// not classify needs no read: its classes are nil. known is false when a
// classified profile is not cached: then the card is drawn from what the
// fetch returns, and keyed by that.
func (p *terrainProfiler) cachedCardClasses(req terrainRequest) (classes []byte, known bool) {
	n, both := len(req.plan.E), req.q.both
	if !p.classifies(both) || req.plan.Calls() == 0 {
		return nil, true // cardElevation draws a path outside DHM unclassified
	}
	data, ok := p.cache.Get(p.elevationKey(req), p.now())
	if !ok {
		return nil, false
	}
	if !terrainEntryOK(data, n, both, true) {
		return nil, true // drawn degraded from none, never cached
	}
	return data[len(data)-(2+n):], true
}

// cardPath is the card's path and query for a resolved request, built
// from parsed values only. v is a short hash of the card key and the
// readiness of the profile: it changes whenever the card would be drawn
// differently under the same query (the renderer, palette and options,
// a node's name, position, antenna or the radio, the building classes and
// credit), so the immutable URL
// never serves a crawler stale bytes, and a crawler that cached the
// degraded card sees a new URL once the profile is measured. The key's
// inputs are hashed, so v carries no name or position.
func (p *terrainProfiler) cardPath(req terrainRequest, classes []byte, ready bool) string {
	q := "a=" + shareRefParam(req.q.a) + "&b=" + shareRefParam(req.q.b) + physicsQuery(req.q)
	v := ogContentHashHex([]byte(fmt.Sprintf("%s|%t", p.cardKey(req, classes), ready)))
	return cardRoute + "?" + q + "&v=" + v
}

func (s *Server) handleTerrainProfileCard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	p := s.terrain
	if p == nil || p.card == nil {
		refuseCard(w, http.StatusNotFound, "not found")
		return
	}
	tq, err := parseCardQuery(r.URL.Query())
	if err != nil {
		refuseCard(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	req, rerr := s.resolveTerrainRequest(r.Context(), p, tq)
	if rerr != nil {
		refuseCard(w, rerr.status, rerr.msg)
		return
	}
	c := p.card
	names := cardNames(req)
	if classes, known := p.cachedCardClasses(req); known {
		if b, ok := c.cache.Get(p.cardKey(req, classes), p.now()); ok {
			writeCard(w, b, "hit")
			return
		}
	}
	if ok, retry := p.limiter.allow(r); !ok {
		markCardRefused(w)
		writeRateLimited(w, retry)
		return
	}
	elev, ready, err := p.cardElevation(r, req)
	if err != nil {
		p.refuseCardBusy(w, r, err)
		return
	}
	resp := p.buildResponse(tq, req.ends, req.a, req.b, &req.plan, elev)
	key := p.cardKey(req, elev.cardClasses())
	opt := c.options()
	opt.Pending = !ready
	flightKey := key.String()
	if !ready {
		flightKey += "|pending"
	}
	b, _, err := c.flight.DoAdmitted(r.Context(), flightKey, 0, c.admit, func(context.Context) ([]byte, error) {
		out, err := c.render(resp, names, c.pal, opt)
		if err == nil && ready {
			c.cache.Put(key, out, p.now())
		}
		return out, err
	})
	if err != nil {
		p.refuseCardBusy(w, r, err)
		return
	}
	state := "miss"
	if !ready {
		state = "degraded"
	}
	writeCard(w, b, state)
}

// cardElevation is the elevations a card is drawn from, and whether they
// are real. ready false is a degraded card: the share budget was spent,
// or upstream failed or was slow. err is only ever an admission refusal
// or the client having gone.
func (p *terrainProfiler) cardElevation(r *http.Request, req terrainRequest) (terrainElevation, bool, error) {
	n, both := len(req.plan.E), req.q.both
	none := terrainElevation{dtm: nanSlice(n)}
	if both {
		none.dsm = nanSlice(n)
	}
	if req.plan.Calls() == 0 {
		// Nothing of the path is inside DHM: complete, and free.
		return none, true, nil
	}
	key := p.elevationKey(req)
	if data, ok := p.cache.Get(key, p.now()); ok {
		e, ready := p.cardDecode(data, n, both, none)
		return e, ready, nil
	}
	c := p.card
	if !c.budget.take(p.now()) {
		c.noteDegraded(p.now())
		return none, false, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), c.wait)
	defer cancel()
	// Held: when the card stops waiting, the fetch its token bought runs
	// on to p.deadline under its maxInFlight slot and is cached, so the
	// crawler's re-fetch gets the complete card instead of spending another
	// token on another fetch that is cut short (#759 review finding 3).
	data, shared, err := p.flight.DoAdmittedHeld(ctx, key.String(), p.deadline, p.admit, p.elevationCall(key, &req.plan, both))
	if shared || isTerrainBusy(err) {
		// Joining a running fetch, or being refused, did no upstream work.
		c.budget.refund()
	}
	switch {
	case err == nil:
		e, ready := p.cardDecode(data, n, both, none)
		return e, ready, nil
	case isTerrainBusy(err):
		return none, false, err
	case r.Context().Err() != nil:
		return none, false, r.Context().Err()
	}
	if p.logFailure != nil && !errors.Is(err, context.DeadlineExceeded) {
		p.logFailure("terrain share card raster", err)
	}
	return none, false, nil
}

// cardDecode decodes a cached profile for a card, classified exactly when
// this profiler classifies the model. An entry that does not decode is
// drawn as the degraded card from none, never from the zero value, whose
// empty slices buildResponse cannot index (#727 M4).
func (p *terrainProfiler) cardDecode(data []byte, n int, both bool, none terrainElevation) (terrainElevation, bool) {
	e, err := decodeTerrainElevation(data, n, both, p.classifies(both))
	if err != nil {
		return none, false
	}
	return e, cardReady(e, nil)
}

// cardReady reports whether decoded elevations make a complete card. A
// profile whose foreign fill failed (#738) is cached for its DHM part, and
// the JSON route tries the fill again on a hit; a card of it would be
// served immutable while the profile can still change, so it is degraded
// instead, and refreshes once the fill has been retried.
func cardReady(e terrainElevation, err error) bool {
	return err == nil && !e.fillFailed
}

// noteDegraded counts a degraded card served for a spent share budget and
// reports it at most once per cardDegradedLogEvery, naming the setting.
func (c *profileCards) noteDegraded(now time.Time) {
	if n, due := c.degradedLog.note(now, c.degraded.Add(1), cardDegradedLogEvery); due {
		c.logf("[dhm] terrain share card: served %d degraded card(s) since the last report because map.dhm.profile.share.upstreamPerMin (%d) is spent; they refresh within a minute, raise it if previews stay incomplete", n, c.budget.perMin)
	}
}

// refuseCardBusy answers a failed card: nothing for a client that left,
// 503 for an admission refusal (refunded), 500 otherwise.
func (p *terrainProfiler) refuseCardBusy(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	if isTerrainBusy(err) {
		p.limiter.refund(r)
		p.noteRefusal()
		setRetryAfter(w, terrainBusyRetry)
		refuseCard(w, http.StatusServiceUnavailable, errTerrainBusy.Error())
		return
	}
	refuseCard(w, http.StatusInternalServerError, "internal error")
}

func markCardRefused(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-CoreScope-Card", "refused")
}

// refuseCard writes a refusal: the fixed JSON error, never cached.
func refuseCard(w http.ResponseWriter, code int, msg string) {
	markCardRefused(w)
	writeError(w, code, msg)
}

// writeCard writes a card. A complete card never changes for its URL; a
// degraded one is re-fetched within a minute.
func writeCard(w http.ResponseWriter, b []byte, state string) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("X-CoreScope-Card", state)
	if state == "degraded" {
		h.Set("Cache-Control", cardCacheDegraded)
	} else {
		h.Set("Cache-Control", cardCacheComplete)
	}
	w.Write(b)
}

// ── The share budget ───────────────────────────────────────────────────────

// shareBudget is map.dhm.profile.share.upstreamPerMin: a server-wide token
// bucket of upstream profile fetches that sharing may cause, refilled at
// perMin per minute with a burst of perMin. Per-IP limits do not bound a
// crawler fleet that fetches from many addresses; this does. 0: sharing
// never goes upstream.
type shareBudget struct {
	mu     sync.Mutex
	perMin int
	tokens float64
	at     time.Time
}

func newShareBudget(perMin int) *shareBudget {
	return &shareBudget{perMin: perMin, tokens: float64(perMin)}
}

func (b *shareBudget) refillLocked(now time.Time) {
	if !b.at.IsZero() && now.After(b.at) {
		b.tokens = math.Min(float64(b.perMin), b.tokens+now.Sub(b.at).Minutes()*float64(b.perMin))
	}
	if b.at.IsZero() || now.After(b.at) {
		b.at = now
	}
}

// take spends one token, if there is one.
func (b *shareBudget) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.perMin <= 0 {
		return false
	}
	b.refillLocked(now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// refund gives back a token that bought no upstream work.
func (b *shareBudget) refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = math.Min(float64(b.perMin), b.tokens+1)
}

// available is the number of tokens at now.
func (b *shareBudget) available(now time.Time) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now)
	return b.tokens
}

// ── What /p reads ──────────────────────────────────────────────────────────

// cachedElevation is the profile for req if it is to hand without any
// upstream call: from the elevation cache, or for a path outside DHM.
func (p *terrainProfiler) cachedElevation(req terrainRequest) (terrainElevation, bool) {
	n, both := len(req.plan.E), req.q.both
	if req.plan.Calls() == 0 {
		e := terrainElevation{dtm: nanSlice(n)}
		if both {
			e.dsm = nanSlice(n)
		}
		return e, true
	}
	data, ok := p.cache.Get(p.elevationKey(req), p.now())
	if !ok {
		return terrainElevation{}, false
	}
	e, err := decodeTerrainElevation(data, n, both, p.classifies(both))
	if !cardReady(e, err) {
		return terrainElevation{}, false
	}
	return e, true
}

// pendingResponse is the profile for req with no elevations: what a
// degraded card shows.
func (p *terrainProfiler) pendingResponse(req terrainRequest) TerrainProfileResponse {
	n := len(req.plan.E)
	e := terrainElevation{dtm: nanSlice(n)}
	if req.q.both {
		e.dsm = nanSlice(n)
	}
	return p.buildResponse(req.q, req.ends, req.a, req.b, &req.plan, e)
}
