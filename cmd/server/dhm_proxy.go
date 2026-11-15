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
// #734: the browser names the overlay by id in `layers` (terrain, surface
// or contours; internal/dhm/overlay.go), and the server maps it to the
// upstream layer. terrain is the #687 chain above, and the #680 layer name
// stays its alias for one release. surface (the surface hillshade) and
// contours (2.5 m contour lines, from z14) have no fallback. Every
// overlay keeps its own cache entries and shares the one per-client
// budget and admission bound.
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
// #728: with map.dhm.ortho.enabled, the same proxy also serves the Danish
// spring orthophoto as a base-map tile at /api/map/dhm/ortho/{z}/{x}/{y}
// (WMTS orto_foraar_webm_DAF, internal/dhm/ortho.go). It holds to every
// property above: the path is validated to a TileKey and nothing else, any
// query string is refused, the upstream query is rebuilt, only a decoded
// 256×256 JPEG is served, and the same coverage gate, per-client budget
// and admission bound apply. Its JPEGs live in their own cache instance,
// so they never evict hillshade tiles.
//
// The endpoints never touch SQLite.

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

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
)

const (
	dhmClientPath = "/api/map/dhm/wms"
	// dhmOrthoRoute is the ortho tile route (#728) and, as the browser's
	// Leaflet URL template, the url of dhm.ortho in /api/config/client.
	dhmOrthoRoute    = "/api/map/dhm/ortho/{z}/{x}/{y}"
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
	Profile bool `json:"profile"`
	// Share reports whether the /p share route serves per-profile link
	// previews (#727): the profile is served and map.dhm.profile.share.
	// enabled is set. Without it the copy button hands out the plain
	// #/map?profile= link, exactly as before.
	Share bool `json:"share"`
	// PointAntenna is how the profile resolves a free map point's antenna
	// height when the request names none (#746): radio.antennaHeightsM.point,
	// else the built-in default. A point has no per-node key, so this is the
	// whole rule, and the Map page can apply a customizer point default in
	// the one request a drop makes instead of asking twice. Present only
	// with Profile.
	PointAntenna *DHMPointAntenna `json:"pointAntenna,omitempty"`
	Bounds       [2][2]float64    `json:"bounds"` // [[south, west], [north, east]]
	Attribution  string           `json:"attribution"`
	// The credit's parts (#733). Dataforsyningen's terms require the
	// credit to link both the dataset and the licence, and Leaflet's
	// attribution control is the page's, not the server's, so the link
	// targets have to reach the browser. Attribution stays the plain text
	// it always was: the page turns these two marks inside it into
	// anchors, which is why each mark travels with its URL.
	DatasetName string `json:"datasetName"`
	DatasetURL  string `json:"datasetUrl"`
	LicenseName string `json:"licenseName"`
	LicenseURL  string `json:"licenseUrl"`
	// Ortho is present only while map.dhm.ortho.enabled is set (#728).
	Ortho *DHMOrthoClientConfig `json:"ortho,omitempty"`
}

// DHMOrthoClientConfig is `dhm.ortho` of /api/config/client (#728): what
// the orthophoto base layer needs, built from constants only. Its credit
// parts follow #733, like the hillshade's.
type DHMOrthoClientConfig struct {
	URL         string        `json:"url"` // Leaflet URL template
	MinZoom     int           `json:"minZoom"`
	MaxZoom     int           `json:"maxZoom"`
	Bounds      [2][2]float64 `json:"bounds"` // [[south, west], [north, east]]
	Attribution string        `json:"attribution"`
	DatasetName string        `json:"datasetName"`
	DatasetURL  string        `json:"datasetUrl"`
	LicenseName string        `json:"licenseName"`
	LicenseURL  string        `json:"licenseUrl"`
}

// dhmCoverageBounds is dhm.Coverage as Leaflet bounds.
func dhmCoverageBounds() [2][2]float64 {
	c := dhm.Coverage
	return [2][2]float64{{c.South, c.West}, {c.North, c.East}}
}

func dhmOrthoClientConfig() *DHMOrthoClientConfig {
	return &DHMOrthoClientConfig{
		URL:         dhmOrthoRoute,
		MinZoom:     dhm.OrthoMinZoom,
		MaxZoom:     dhm.OrthoMaxZoom,
		Bounds:      dhmCoverageBounds(),
		Attribution: dhm.OrthoAttribution,
		DatasetName: dhm.OrthoDatasetName,
		DatasetURL:  dhm.OrthoDatasetURL,
		LicenseName: dhm.LicenseName,
		LicenseURL:  dhm.LicenseURL,
	}
}

// DHMPointAntenna is a free point's antenna height without a request
// height, and its source: "operator-role" or "point-default", as in a
// profile response's antennaSource.
type DHMPointAntenna struct {
	M      float64 `json:"m"`
	Source string  `json:"source"`
}

func dhmClientConfig() *DHMClientConfig {
	return &DHMClientConfig{
		Enabled:     true,
		URL:         dhmClientPath,
		Layer:       dhm.HillshadeLayer,
		MinZoom:     dhm.MinZoom,
		MaxZoom:     dhm.MaxZoom,
		Bounds:      dhmCoverageBounds(),
		Attribution: dhm.Attribution,
		DatasetName: dhm.DatasetName,
		DatasetURL:  dhm.DatasetURL,
		LicenseName: dhm.LicenseName,
		LicenseURL:  dhm.LicenseURL,
	}
}

// dhmTileQueryParams documents the exact tile request shape for OpenAPI:
// every fixed key with its only accepted value, layers as the enum of
// overlay ids (#734), plus bbox, all required.
func dhmTileQueryParams() []paramMeta {
	fixed := dhm.TileParams()
	names := make([]string, 0, len(fixed))
	for k := range fixed {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]paramMeta, 0, len(names)+1)
	for _, k := range names {
		if k == "layers" {
			// TileParams' layers is the #680 value, the terrain alias.
			out = append(out, paramMeta{Name: k, Type: "string", Required: true,
				Enum: append(dhm.OverlayIDs(), fixed[k]),
				Description: "the overlay id: terrain, surface or contours; " + strconv.Quote(fixed[k]) +
					" is the #680 alias of terrain, accepted for one release"})
			continue
		}
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
// Map: map.dhm (#680), map.heritage (#723) and map.masts (#724).
func (c *Config) extractTypedMapBlocks() {
	c.extractDHMConfig()
	c.extractHeritageConfig()
	c.extractMastsConfig()
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
	// The ortho cache (#728), apart from the hillshade cache so large
	// JPEGs never evict hillshade tiles. Used only with map.dhm.ortho.
	orthoCacheMaxEntries int
	orthoCacheMaxBytes   int
	orthoCacheTTL        time.Duration
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
		// The #728 plan's proposal at ~25 KB a tile; staging measures real
		// ortho tile sizes before these are final. Aerial photos change
		// about once a year, hence the long TTL.
		orthoCacheMaxEntries: 2048,
		orthoCacheMaxBytes:   96 << 20,
		orthoCacheTTL:        7 * 24 * time.Hour,
	}
}

type dhmProxy struct {
	client *dhm.Client
	layer  dhm.Hillshade // configured upstream layer
	cache  *dhm.Cache[dhmCacheKey]
	// orthoCache holds ortho tiles (#728); nil while map.dhm.ortho is off,
	// and the ortho route then answers 404.
	orthoCache *dhm.Cache[dhmCacheKey]
	// limiter and admit are shared by every tile product: one token, one
	// fair-use budget.
	limiter *ipRateLimiter // nil: no per-client limit
	admit   *dhm.Admission // bound on cold-tile fetch workers (#744)
	now     func() time.Time
}

// dhmProduct is what a cached tile shows (#728).
type dhmProduct uint8

const (
	dhmProductTerrain     dhmProduct = iota // #680 hillshade
	dhmProductExaggerated                   // #687 hillshade
	dhmProductOrtho                         // #728 orthophoto
	dhmProductSurface                       // #734 surface hillshade
	dhmProductContours                      // #734 contour lines
)

// overlayProduct is the product an upstream overlay's tiles are, by its
// upstream layer: the terrain id is two products, one per #687 layer.
// Every overlay in internal/dhm's table has one (TestDHM734_
// EveryOverlayHasItsOwnProduct); one without would be a programming
// error, and must not share another product's cache entries.
func overlayProduct(o dhm.Overlay) dhmProduct {
	switch o.Layer {
	case dhm.HillshadeLayer:
		return dhmProductTerrain
	case dhm.ExaggeratedHillshadeLayer:
		return dhmProductExaggerated
	case dhm.SurfaceLayer:
		return dhmProductSurface
	case dhm.ContourLayer:
		return dhmProductContours
	}
	panic("dhm: overlay without a cache product")
}

// name is the product in a log line that names two of them.
func (pr dhmProduct) name() string {
	switch pr {
	case dhmProductExaggerated:
		return "exaggerated"
	case dhmProductOrtho:
		return "ortho"
	case dhmProductSurface:
		return "surface"
	case dhmProductContours:
		return "contours"
	}
	return "terrain"
}

// tileLabel is what a log line says failed or was refused.
func (pr dhmProduct) tileLabel() string {
	switch pr {
	case dhmProductExaggerated:
		return "exaggerated hillshade tile"
	case dhmProductOrtho:
		return "ortho tile"
	case dhmProductSurface:
		return "surface hillshade tile"
	case dhmProductContours:
		return "contour tile"
	}
	return "hillshade tile"
}

func hillshadeProduct(h dhm.Hillshade) dhmProduct {
	if h == dhm.HillshadeExaggerated {
		return dhmProductExaggerated
	}
	return dhmProductTerrain
}

// dhmCacheKey is a tile of the product it actually is, so a terrain
// fallback is never served as an exaggerated tile (#687), nor any tile as
// another product's.
type dhmCacheKey struct {
	Product dhmProduct
	Tile    dhm.TileKey
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
	p := &dhmProxy{
		client: client,
		layer:  layer,
		cache:  dhm.NewCache[dhmCacheKey](opts.cacheMaxEntries, opts.cacheMaxBytes, opts.cacheTTL),
		limiter: newIPRateLimiter("dhm", "map.dhm.tileRateLimitPerMin", opts.rateLimitPerMin,
			opts.trustedProxies, opts.client.Logf),
		admit: dhm.NewAdmission(opts.tileMaxInFlight),
		now:   opts.client.Now,
	}
	if cfg.Ortho.Enabled {
		p.orthoCache = dhm.NewCache[dhmCacheKey](opts.orthoCacheMaxEntries, opts.orthoCacheMaxBytes, opts.orthoCacheTTL)
	}
	return p
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

// writeDHMTile writes a PNG tile the browser may cache for maxAge; 0 is
// no-store.
func writeDHMTile(w http.ResponseWriter, data []byte, maxAge time.Duration, state string) {
	writeTileImage(w, "image/png", data, maxAge, state)
}

// writeTileImage is writeDHMTile for any image type.
func writeTileImage(w http.ResponseWriter, contentType string, data []byte, maxAge time.Duration, state string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
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
	overlay, key, err := dhm.ParseTileRequest(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	if !key.IntersectsCoverage() {
		writeDHMTile(w, dhmTransparentPNG, dhmBrowserMaxAge, "outside")
		return
	}
	src := overlay.Resolve(p.layer) // the terrain id is the configured chain (#687)
	// Only a tile that will go upstream is charged to the client.
	if _, cached := p.cache.Get(dhmCacheKey{Product: overlayProduct(src), Tile: key}, p.now()); !cached {
		if ok, retry := p.limiter.allow(r); !ok {
			writeRateLimited(w, retry)
			return
		}
	}
	ctx := r.Context()
	data, hit, err := p.layerTile(ctx, src, key)
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
		p.client.LogRefusal(overlayProduct(overlay).tileLabel())
		writeDHMTile(w, dhmTransparentPNG, 0, "busy")
		return
	}
	fallback, hasFallback := src.Fallback()
	if !hasFallback || !dhm.FallbackEligible(err) {
		// No fallback (#734: only the exaggerated terrain has one), or a
		// timeout or a full budget: retrying on the fallback would only
		// wait again (#688 review F1).
		p.client.LogFailure(overlayProduct(src).tileLabel(), err)
		writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
		return
	}
	// #687: the exaggerated tile failed; serve the terrain tile instead.
	data, hit, fallbackErr := p.layerTile(ctx, fallback, key)
	if ctx.Err() != nil {
		return
	}
	if fallbackErr != nil {
		first, second := overlayProduct(src), overlayProduct(fallback)
		p.client.LogFailure(second.tileLabel(), dhm.FetchError{Reason: first.name() + " " + dhm.FailureReason(err) +
			", " + second.name() + " " + dhm.FailureReason(fallbackErr)})
		writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
		return
	}
	p.client.LogFallback(overlayProduct(src).tileLabel(), err)
	writeDHMTile(w, data, dhmFallbackMaxAge, tileState("fallback", hit))
}

// layerTile returns the tile of one upstream overlay, from the cache or
// upstream, and whether it was a cache hit. Only successes are cached. A
// cold tile is admitted against p.admit and waited for until ctx is done.
func (p *dhmProxy) layerTile(ctx context.Context, o dhm.Overlay, k dhm.TileKey) ([]byte, bool, error) {
	ck := dhmCacheKey{Product: overlayProduct(o), Tile: k}
	if data, ok := p.cache.Get(ck, p.now()); ok {
		return data, true, nil
	}
	data, err := p.client.FetchOverlayAdmitted(ctx, p.admit, o, k)
	if err != nil {
		return nil, false, err
	}
	p.cache.Put(ck, data, p.now())
	return data, false, nil
}

// serveOrtho serves one orthophoto tile (#728) of
// GET /api/map/dhm/ortho/{z}/{x}/{y}, with the hillshade's coverage gate,
// per-client budget and admission bound. Failures are a transparent PNG,
// which a base layer shows as the map background.
func (p *dhmProxy) serveOrtho(w http.ResponseWriter, r *http.Request) {
	if p.orthoCache == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	v := mux.Vars(r)
	key, err := dhm.ParseOrthoTile(v["z"], v["x"], v["y"])
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	if !key.IntersectsCoverage() {
		writeDHMTile(w, dhmTransparentPNG, dhmBrowserMaxAge, "outside")
		return
	}
	ck := dhmCacheKey{Product: dhmProductOrtho, Tile: key}
	if data, ok := p.orthoCache.Get(ck, p.now()); ok {
		writeTileImage(w, "image/jpeg", data, dhmBrowserMaxAge, "hit")
		return
	}
	// Only a tile that will go upstream is charged, from the hillshade's
	// budget: one client, one token.
	if ok, retry := p.limiter.allow(r); !ok {
		writeRateLimited(w, retry)
		return
	}
	ctx := r.Context()
	data, err := p.client.FetchOrthoAdmitted(ctx, p.admit, key)
	if err == nil {
		p.orthoCache.Put(ck, data, p.now())
		writeTileImage(w, "image/jpeg", data, dhmBrowserMaxAge, "miss")
		return
	}
	if ctx.Err() != nil {
		return // the client left: nobody to answer, nothing to log
	}
	if errors.Is(err, dhm.ErrAdmissionFull) {
		p.limiter.refund(r)
		p.client.LogRefusal("ortho tile")
		writeDHMTile(w, dhmTransparentPNG, 0, "busy")
		return
	}
	p.client.LogFailure("ortho tile", err)
	writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
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
	c.Share = s.terrain != nil && s.terrain.set.share.enabled
	if s.terrain != nil {
		h, src, _ := s.terrain.antenna(terrainEnd{kind: "point"}, nil)
		c.PointAntenna = &DHMPointAntenna{M: h, Source: src}
	}
	if s.dhm.orthoCache != nil {
		c.Ortho = dhmOrthoClientConfig()
	}
	return c
}

func (s *Server) handleDHMTile(w http.ResponseWriter, r *http.Request) {
	if s.dhm == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.dhm.ServeHTTP(w, r)
}

// handleDHMOrtho is GET /api/map/dhm/ortho/{z}/{x}/{y} (#728): 404 unless
// map.dhm is active and map.dhm.ortho.enabled.
func (s *Server) handleDHMOrtho(w http.ResponseWriter, r *http.Request) {
	if s.dhm == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.dhm.serveOrtho(w, r)
}
