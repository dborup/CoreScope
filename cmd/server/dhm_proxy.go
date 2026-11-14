package main

// #680 — DHM (Danmarks Højdemodel) terrain hillshade overlay proxy.
//
// The Map and Live pages can draw the DHM terrain hillshade as an optional
// overlay. The browser requests WMS tiles from /api/map/dhm/wms; this
// handler validates the request against one fixed, allow-listed shape and
// fetches the tile through the shared DHM client (internal/dhm), which
// adds the token server-side in the `token` request header.
//
// #687: the server chooses the upstream layer (map.dhm.layer). By default
// it is the exaggerated hillshade (wms/dhm_temp); if that fails for a
// tile, the #680 terrain hillshade (dhm_DAF) is served instead. Browsers
// keep requesting the #680 shape either way.
//
// Security properties, each pinned by dhm_proxy_test.go and the
// internal/dhm tests:
//   - Not an open proxy. The upstream service is a constant, every client
//     parameter is checked against an exact allow-list, unknown parameters
//     are rejected, and the upstream query is rebuilt from the validated
//     tile coordinates rather than copied from the client.
//   - The token is never written to a response, an error, a URL or a log
//     line. Failures are logged by fixed category only.
//   - Upstream bodies are never forwarded unless they are a PNG within the
//     size cap; anything else becomes a transparent tile marked no-store.
//   - Bounded LRU cache (entries + bytes + TTL), a per-request timeout,
//     a concurrency limit towards upstream and coalescing of identical
//     in-flight tiles.
//   - A global admission bound on distinct cold tiles (#744): at most
//     dhmTileMaxInFlight fetch workers exist, running or queued for an
//     upstream slot. Beyond it a tile is refused at once (transparent,
//     no-store, X-CoreScope-Tile: busy) and the per-client charge is given
//     back. A client that leaves stops waiting, and the fetch stops once
//     no client waits for it, so queued work does not outlive its callers.
//   - A per-client budget for tiles that would go upstream
//     (map.dhm.tileRateLimitPerMin, #682 review F3): 429 with Retry-After
//     beyond it. Cached tiles and tiles outside Denmark are not charged.
//
// The endpoint never touches SQLite.

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

const (
	dhmClientPath    = "/api/map/dhm/wms"
	dhmBrowserMaxAge = 24 * time.Hour
	// dhmFallbackMaxAge is the browser cache lifetime of a terrain tile
	// served in place of a failed exaggerated one (#687), so the browser
	// asks again soon rather than keeping a mixed map for a day.
	dhmFallbackMaxAge = 10 * time.Minute
)

// DHMConfig is config.json `map.dhm` (Config.DHM).
type DHMConfig = dhm.Config

// DHMClientConfig is the `dhm` block of /api/config/client. Present only
// while the proxy is active; it carries what the overlay needs and nothing
// secret.
type DHMClientConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Layer   string `json:"layer"`
	MinZoom int    `json:"minZoom"`
	MaxZoom int    `json:"maxZoom"`
	// Profile reports whether GET /api/terrain/profile is served (#681 M4).
	// The overlay only needs map.dhm; the Path profile tool also needs
	// map.dhm.profile.enabled, so the Map page cannot infer it from
	// Enabled and must be told. No limit or parameter is published with
	// it: the endpoint validates its own request.
	Profile     bool          `json:"profile"`
	Bounds      [2][2]float64 `json:"bounds"` // [[south, west], [north, east]]
	Attribution string        `json:"attribution"`
}

func dhmClientConfig() *DHMClientConfig {
	c := dhm.Coverage
	return &DHMClientConfig{
		Enabled:     true,
		URL:         dhmClientPath,
		Layer:       dhm.HillshadeLayer,
		MinZoom:     dhm.MinZoom,
		MaxZoom:     dhm.MaxZoom,
		Bounds:      [2][2]float64{{c.South, c.West}, {c.North, c.East}},
		Attribution: dhm.Attribution,
	}
}

// dhmTileQueryParams documents the exact tile request shape for OpenAPI:
// every fixed key with its only accepted value, plus bbox, all required.
func dhmTileQueryParams() []paramMeta {
	fixed := dhm.TileParams()
	names := make([]string, 0, len(fixed))
	for k := range fixed {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]paramMeta, 0, len(names)+1)
	for _, k := range names {
		desc := "must be exactly " + strconv.Quote(fixed[k])
		if fixed[k] == "" {
			desc = "must be present and empty"
		}
		out = append(out, paramMeta{Name: k, Description: desc, Type: "string", Required: true})
	}
	return append(out, paramMeta{Name: "bbox", Description: "minx,miny,maxx,maxy of one EPSG:3857 tile at zoom 7-19", Type: "string", Required: true})
}

// extractDHMConfig moves map.dhm into the typed DHM field, because the
// free-form Map block is shipped verbatim by /api/config/client. The raw
// block is deleted even when it is malformed: it must never reach clients.
func (c *Config) extractDHMConfig() {
	liftMapBlock(c, "dhm", "terrain overlay", &c.DHM)
}

// extractTypedMapBlocks lifts every typed block out of the client-visible
// Map: map.dhm (#680) and map.heritage (#723).
func (c *Config) extractTypedMapBlocks() {
	c.extractDHMConfig()
	c.extractHeritageConfig()
}

// ── Proxy ──────────────────────────────────────────────────────────────────

type dhmProxyOptions struct {
	client          dhm.ClientOptions
	cacheMaxEntries int
	cacheMaxBytes   int
	cacheTTL        time.Duration
	// rateLimitPerMin is the per-client budget of upstream-bound tiles;
	// 0 is off. newDHMProxyFromConfig sets it from the config.
	rateLimitPerMin int
	trustedProxies  []*net.IPNet
	// tileMaxInFlight bounds distinct cold-tile fetches, running or
	// queued (#744); defaultDHMProxyOptions sets dhmTileMaxInFlight.
	tileMaxInFlight int
}

// dhmTileMaxInFlight is the shipped bound on cold-tile fetch workers
// (#744): 16x the upstream concurrency of 4, like the profile's
// maxInFlight cap is a multiple of the same budget. A cold 1920x1080 Map
// view is 8x5 visible tiles plus Leaflet's edge buffer, so at least one
// full view always fits, while a flood holds at most 64 goroutines that
// read no body until they have an upstream slot. An admitted tile waits
// behind at most 15 others for one of the 4 slots, and the 10 s Timeout
// still bounds that wait.
const dhmTileMaxInFlight = 64

// dhmTileRateLimitDefault is the shipped per-client budget of tiles that
// miss the server cache, per minute. Measured with
// test-issue-681-dhm-tile-budget-e2e.js on a 1920×1080 Map with the HTTP
// cache off (so upper bounds): a normal session — load, three zoom steps
// in, four pans, two out — requested 308 tiles; an aggressive one through
// the whole zoom range 8→16→8 with sixteen pans requested 812 in 18 s.
// 1200 is 3.9× the normal session and covers the aggressive one, so a
// person using the map never meets it, while a script walking the ~97 M
// tiles of DHM coverage is held to 20 per second per address.
const dhmTileRateLimitDefault = 1200

// dhmTileRateLimitMax bounds map.dhm.tileRateLimitPerMin like
// profile.rateLimitPerMin bounds its own key (0-6000, 600x its default of
// 10): about 50x the shipped tile default, comfortably under the point
// (perMin above ~6e10, where time.Minute/perMin truncates to zero) where
// the limiter would silently stop limiting (#690 review N2).
const dhmTileRateLimitMax = 60000

// dhmTileRateLimit is map.dhm.tileRateLimitPerMin: unset is the default, 0
// is off, and anything outside [0, dhmTileRateLimitMax] warns and falls
// back to the default too, like profile.rateLimitPerMin.
func dhmTileRateLimit(c dhm.Config, logf func(string, ...interface{})) int {
	if logf == nil {
		logf = log.Printf
	}
	if c.TileRateLimitPerMin == nil {
		return dhmTileRateLimitDefault
	}
	v := *c.TileRateLimitPerMin
	if v < 0 || v > dhmTileRateLimitMax {
		logf("[dhm] config map.dhm.tileRateLimitPerMin is out of range; using %d", dhmTileRateLimitDefault)
		return dhmTileRateLimitDefault
	}
	return v
}

func defaultDHMProxyOptions() dhmProxyOptions {
	return dhmProxyOptions{
		client:          dhm.DefaultClientOptions(),
		cacheMaxEntries: 2048,
		cacheMaxBytes:   64 << 20,
		cacheTTL:        24 * time.Hour,
		tileMaxInFlight: dhmTileMaxInFlight,
	}
}

type dhmProxy struct {
	client  *dhm.Client
	layer   dhm.Hillshade // configured upstream layer
	cache   *dhm.Cache[dhmCacheKey]
	limiter *ipRateLimiter // nil: no per-client limit
	admit   *dhm.Admission // bound on cold-tile fetch workers (#744)
	now     func() time.Time
}

// dhmCacheKey is a tile of the upstream layer it was actually fetched
// from, so a terrain fallback is never served as an exaggerated tile.
type dhmCacheKey struct {
	Layer dhm.Hillshade
	Tile  dhm.TileKey
}

// newDHMProxy returns nil unless map.dhm is enabled with a token: an
// inactive proxy is not offered to clients and its endpoint returns 404.
func newDHMProxy(cfg dhm.Config, opts dhmProxyOptions) *dhmProxy {
	client := dhm.NewClient(cfg, opts.client)
	if client == nil {
		return nil
	}
	layer, ok := dhm.ParseHillshade(cfg.Layer)
	if !ok {
		// The value is not echoed: it could be a misplaced token.
		opts.client.Logf(`[dhm] config map.dhm.layer must be "exaggerated" or "terrain"; using "terrain"`)
	}
	return &dhmProxy{
		client: client,
		layer:  layer,
		cache:  dhm.NewCache[dhmCacheKey](opts.cacheMaxEntries, opts.cacheMaxBytes, opts.cacheTTL),
		limiter: newIPRateLimiter("map.dhm.tileRateLimitPerMin", opts.rateLimitPerMin,
			opts.trustedProxies, opts.client.Logf),
		admit: dhm.NewAdmission(opts.tileMaxInFlight),
		now:   opts.client.Now,
	}
}

// dhmTransparentPNG is served outside coverage and on any upstream failure,
// so the overlay stays silent instead of drawing broken tiles.
var dhmTransparentPNG = func() []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// writeDHMTile writes a tile the browser may cache for maxAge; 0 is
// no-store.
func writeDHMTile(w http.ResponseWriter, data []byte, maxAge time.Duration, state string) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-CoreScope-Tile", state)
	if maxAge > 0 {
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(maxAge/time.Second)))
	} else {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (p *dhmProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	key, err := dhm.ParseTileRequest(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	if !key.IntersectsCoverage() {
		writeDHMTile(w, dhmTransparentPNG, dhmBrowserMaxAge, "outside")
		return
	}
	// Only a tile that will go upstream is charged to the client.
	if _, cached := p.cache.Get(dhmCacheKey{Layer: p.layer, Tile: key}, p.now()); !cached {
		if ok, retry := p.limiter.allow(r); !ok {
			writeRateLimited(w, retry)
			return
		}
	}
	ctx := r.Context()
	data, hit, err := p.layerTile(ctx, p.layer, key)
	if err == nil {
		writeDHMTile(w, data, dhmBrowserMaxAge, tileState("", hit))
		return
	}
	if ctx.Err() != nil {
		return // the client left: nobody to answer, nothing to log
	}
	if errors.Is(err, dhm.ErrAdmissionFull) {
		// #744: refused before any upstream work, so the charge is given
		// back, as for a refused profile (#705/#707).
		p.limiter.refund(r)
		p.client.LogRefusal("hillshade tile")
		writeDHMTile(w, dhmTransparentPNG, 0, "busy")
		return
	}
	if p.layer == dhm.HillshadeTerrain {
		p.client.LogFailure("hillshade tile", err)
		writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
		return
	}
	if !dhm.FallbackEligible(err) {
		// A timeout or a full budget: retrying on terrain would only wait
		// again (#688 review F1).
		p.client.LogFailure("exaggerated hillshade tile", err)
		writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
		return
	}
	// #687: the exaggerated tile failed; serve the terrain tile instead.
	data, hit, fallbackErr := p.layerTile(ctx, dhm.HillshadeTerrain, key)
	if ctx.Err() != nil {
		return
	}
	if fallbackErr != nil {
		p.client.LogFailure("hillshade tile", dhm.FetchError{Reason: "exaggerated " + dhm.FailureReason(err) +
			", terrain " + dhm.FailureReason(fallbackErr)})
		writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
		return
	}
	p.client.LogFallback("exaggerated hillshade tile", err)
	writeDHMTile(w, data, dhmFallbackMaxAge, tileState("fallback", hit))
}

// layerTile returns the tile of one upstream layer, from the cache or
// upstream, and whether it was a cache hit. Only successes are cached. A
// cold tile is admitted against p.admit and waited for until ctx is done.
func (p *dhmProxy) layerTile(ctx context.Context, h dhm.Hillshade, k dhm.TileKey) ([]byte, bool, error) {
	ck := dhmCacheKey{Layer: h, Tile: k}
	if data, ok := p.cache.Get(ck, p.now()); ok {
		return data, true, nil
	}
	data, err := p.client.FetchHillshadeAdmitted(ctx, p.admit, h, k)
	if err != nil {
		return nil, false, err
	}
	p.cache.Put(ck, data, p.now())
	return data, false, nil
}

// tileState is the X-CoreScope-Tile value: hit/miss, or with a prefix
// fallback/fallback-hit.
func tileState(prefix string, hit bool) string {
	switch {
	case prefix == "" && hit:
		return "hit"
	case prefix == "":
		return "miss"
	case hit:
		return prefix + "-hit"
	}
	return prefix
}

func newDHMProxyFromConfig(cfg *Config) *dhmProxy {
	if cfg == nil {
		return nil
	}
	opts := defaultDHMProxyOptions()
	opts.rateLimitPerMin = dhmTileRateLimit(cfg.DHM, opts.client.Logf)
	opts.trustedProxies = parseCIDRListQuiet(cfg.WSTrustedProxies())
	return newDHMProxy(cfg.DHM, opts)
}

func (s *Server) dhmClientConfig() *DHMClientConfig {
	if s.dhm == nil {
		return nil
	}
	c := dhmClientConfig()
	c.Profile = s.terrain != nil
	return c
}

func (s *Server) handleDHMTile(w http.ResponseWriter, r *http.Request) {
	if s.dhm == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.dhm.ServeHTTP(w, r)
}
