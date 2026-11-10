package main

// #680 — DHM (Danmarks Højdemodel) terrain hillshade overlay proxy.
//
// The Map and Live pages can draw the DHM terrain hillshade from the Danish
// public data service (Dataforsyningen / Klimadatastyrelsen) as an optional
// overlay. That service needs a personal access token, so the browser never
// talks to it: it requests WMS tiles from /api/map/dhm/wms, and this proxy
// validates the request against one fixed, allow-listed shape and fetches the
// tile upstream with the token added server-side.
//
// Upstream (verified against the public GetCapabilities of dhm_DAF, WMS
// 1.3.0): layer dhm_terraen_skyggekort, image/png, EPSG:3857 inherited from
// the root layer, MaxWidth/MaxHeight 10000, token as the lowercase `token`
// query parameter.
//
// Security properties, each pinned by dhm_proxy_test.go:
//   - Not an open proxy. The upstream URL is a constant, every client
//     parameter is checked against an exact allow-list, unknown parameters
//     are rejected, and the upstream query is rebuilt from the validated
//     tile coordinates rather than copied from the client.
//   - The token is never written to a response, an error or a log line.
//     net/http transport errors quote the full request URL (token
//     included), so failures are logged by fixed category only.
//   - Upstream bodies are never forwarded unless they are a PNG within the
//     size cap; anything else becomes a transparent tile marked no-store.
//   - Bounded LRU cache (entries + bytes + TTL), a per-request timeout,
//     a concurrency limit towards upstream and coalescing of identical
//     in-flight tiles.
//
// The endpoint never touches SQLite.

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	dhmUpstreamURL    = "https://api.dataforsyningen.dk/dhm_DAF"
	dhmLayer          = "dhm_terraen_skyggekort"
	dhmTileSize       = 256
	dhmMinZoom        = 7
	dhmMaxZoom        = 19
	dhmClientPath     = "/api/map/dhm/wms"
	dhmAttribution    = "Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel"
	dhmMaxTileBytes   = 1 << 20
	dhmBrowserMaxAge  = 24 * time.Hour
	webMercatorHalf   = math.Pi * 6378137 // 20037508.342789244
	dhmBBoxTolerance  = 1e-6              // fraction of a tile edge
	dhmMaxBBoxDecimal = 64                // longest accepted bbox component
)

// dhmCoverage is the EX_GeographicBoundingBox of dhm_terraen_skyggekort in
// the dhm_DAF capabilities. Tiles wholly outside it are answered with a
// transparent tile and never sent upstream.
var dhmCoverage = struct{ South, West, North, East float64 }{54.4265, 7.99125, 57.778, 15.5995}

// DHMConfig is config.json `map.dhm`. LoadConfig moves it out of the
// free-form Map block (which /api/config/client ships verbatim) into
// Config.DHM, so the token cannot reach the browser through that block.
type DHMConfig struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
}

func (c DHMConfig) active() bool {
	return c.Enabled && strings.TrimSpace(c.Token) != ""
}

// DHMClientConfig is the `dhm` block of /api/config/client. Present only
// while the proxy is active; it carries what the overlay needs and nothing
// secret.
type DHMClientConfig struct {
	Enabled     bool          `json:"enabled"`
	URL         string        `json:"url"`
	Layer       string        `json:"layer"`
	MinZoom     int           `json:"minZoom"`
	MaxZoom     int           `json:"maxZoom"`
	Bounds      [2][2]float64 `json:"bounds"` // [[south, west], [north, east]]
	Attribution string        `json:"attribution"`
}

func dhmClientConfig() *DHMClientConfig {
	return &DHMClientConfig{
		Enabled:     true,
		URL:         dhmClientPath,
		Layer:       dhmLayer,
		MinZoom:     dhmMinZoom,
		MaxZoom:     dhmMaxZoom,
		Bounds:      [2][2]float64{{dhmCoverage.South, dhmCoverage.West}, {dhmCoverage.North, dhmCoverage.East}},
		Attribution: dhmAttribution,
	}
}

// extractDHMConfig moves map.dhm into the typed DHM field. The raw block is
// deleted even when it is malformed: it must never be shipped to clients.
func (c *Config) extractDHMConfig() {
	if c.Map == nil {
		return
	}
	raw, ok := c.Map["dhm"]
	if !ok {
		return
	}
	delete(c.Map, "dhm")
	c.DHM = DHMConfig{}
	b, err := json.Marshal(raw)
	if err == nil {
		err = json.Unmarshal(b, &c.DHM)
	}
	if err != nil {
		c.DHM = DHMConfig{}
		log.Printf("[dhm] config map.dhm is malformed; terrain overlay disabled")
	}
}

// ── Request validation ─────────────────────────────────────────────────────

type dhmTileKey struct{ Z, X, Y int }

// dhmFixedParams is the exact client request shape (Leaflet 1.9.4
// L.tileLayer.wms with the overlay options in map-tile-providers.js), apart
// from bbox. Any other key, a missing key or a repeated key is rejected.
var dhmFixedParams = map[string]string{
	"service":     "WMS",
	"request":     "GetMap",
	"version":     "1.3.0",
	"layers":      dhmLayer,
	"styles":      "",
	"format":      "image/png",
	"transparent": "true",
	"width":       strconv.Itoa(dhmTileSize),
	"height":      strconv.Itoa(dhmTileSize),
	"crs":         "EPSG:3857",
}

var errDHMBadRequest = errors.New("invalid terrain tile request")

// parseDHMTileRequest validates the client query and returns the Web
// Mercator tile it names. Errors are one fixed value: nothing from the
// client is echoed back.
func parseDHMTileRequest(q url.Values) (dhmTileKey, error) {
	if len(q) != len(dhmFixedParams)+1 {
		return dhmTileKey{}, errDHMBadRequest
	}
	for k, vs := range q {
		if len(vs) != 1 {
			return dhmTileKey{}, errDHMBadRequest
		}
		if k == "bbox" {
			continue
		}
		want, ok := dhmFixedParams[k]
		if !ok || vs[0] != want {
			return dhmTileKey{}, errDHMBadRequest
		}
	}
	bbox := q.Get("bbox")
	if bbox == "" {
		return dhmTileKey{}, errDHMBadRequest
	}
	return dhmTileFromBBox(bbox)
}

// dhmTileFromBBox accepts a bbox only if it is exactly one Web Mercator
// tile (within float noise) at an allowed zoom.
func dhmTileFromBBox(bbox string) (dhmTileKey, error) {
	parts := strings.Split(bbox, ",")
	if len(parts) != 4 {
		return dhmTileKey{}, errDHMBadRequest
	}
	var v [4]float64
	for i, s := range parts {
		if len(s) == 0 || len(s) > dhmMaxBBoxDecimal {
			return dhmTileKey{}, errDHMBadRequest
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return dhmTileKey{}, errDHMBadRequest
		}
		v[i] = f
	}
	minx, miny, maxx, maxy := v[0], v[1], v[2], v[3]
	span := maxx - minx
	if !(span > 0) || !(maxy-miny > 0) {
		return dhmTileKey{}, errDHMBadRequest
	}
	zf := math.Log2(2 * webMercatorHalf / span)
	z := int(math.Round(zf))
	if z < dhmMinZoom || z > dhmMaxZoom {
		return dhmTileKey{}, errDHMBadRequest
	}
	size := 2 * webMercatorHalf / math.Exp2(float64(z))
	x := int(math.Round((minx + webMercatorHalf) / size))
	y := int(math.Round((webMercatorHalf - maxy) / size))
	n := 1 << z
	if x < 0 || y < 0 || x >= n || y >= n {
		return dhmTileKey{}, errDHMBadRequest
	}
	key := dhmTileKey{z, x, y}
	want := key.bbox()
	tol := size * dhmBBoxTolerance
	for i := range v {
		if math.Abs(v[i]-want[i]) > tol {
			return dhmTileKey{}, errDHMBadRequest
		}
	}
	return key, nil
}

// bbox is the tile's exact EPSG:3857 extent: minx, miny, maxx, maxy.
func (k dhmTileKey) bbox() [4]float64 {
	size := 2 * webMercatorHalf / math.Exp2(float64(k.Z))
	return [4]float64{
		float64(k.X)*size - webMercatorHalf,
		webMercatorHalf - float64(k.Y+1)*size,
		float64(k.X+1)*size - webMercatorHalf,
		webMercatorHalf - float64(k.Y)*size,
	}
}

func (k dhmTileKey) String() string { return fmt.Sprintf("%d/%d/%d", k.Z, k.X, k.Y) }

func mercatorToLonLat(x, y float64) (lon, lat float64) {
	lon = x / webMercatorHalf * 180
	lat = (2*math.Atan(math.Exp(y/6378137)) - math.Pi/2) * 180 / math.Pi
	return
}

func (k dhmTileKey) intersectsCoverage() bool {
	b := k.bbox()
	west, south := mercatorToLonLat(b[0], b[1])
	east, north := mercatorToLonLat(b[2], b[3])
	return east > dhmCoverage.West && west < dhmCoverage.East &&
		north > dhmCoverage.South && south < dhmCoverage.North
}

// ── Bounded tile cache ─────────────────────────────────────────────────────

type dhmCacheEntry struct {
	key     dhmTileKey
	data    []byte
	expires time.Time
}

// dhmTileCache is an LRU bounded by entry count and total bytes, with a TTL
// checked on read.
type dhmTileCache struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int
	ttl        time.Duration
	bytes      int
	order      *list.List // front = most recently used
	items      map[dhmTileKey]*list.Element
}

func newDHMTileCache(maxEntries, maxBytes int, ttl time.Duration) *dhmTileCache {
	return &dhmTileCache{
		maxEntries: maxEntries, maxBytes: maxBytes, ttl: ttl,
		order: list.New(), items: make(map[dhmTileKey]*list.Element),
	}
}

func (c *dhmTileCache) get(k dhmTileKey, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	e := el.Value.(*dhmCacheEntry)
	if !now.Before(e.expires) {
		c.removeLocked(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.data, true
}

func (c *dhmTileCache) put(k dhmTileKey, data []byte, now time.Time) {
	if len(data) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.removeLocked(el)
	}
	c.items[k] = c.order.PushFront(&dhmCacheEntry{key: k, data: data, expires: now.Add(c.ttl)})
	c.bytes += len(data)
	for c.order.Len() > c.maxEntries || c.bytes > c.maxBytes {
		c.removeLocked(c.order.Back())
	}
}

func (c *dhmTileCache) removeLocked(el *list.Element) {
	e := c.order.Remove(el).(*dhmCacheEntry)
	delete(c.items, e.key)
	c.bytes -= len(e.data)
}

func (c *dhmTileCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *dhmTileCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// ── Proxy ──────────────────────────────────────────────────────────────────

type dhmProxyOptions struct {
	upstream        string
	timeout         time.Duration
	concurrency     int
	cacheMaxEntries int
	cacheMaxBytes   int
	cacheTTL        time.Duration
	now             func() time.Time
	logf            func(format string, args ...interface{})
	logEvery        time.Duration // throttle for upstream-failure log lines
}

func defaultDHMProxyOptions() dhmProxyOptions {
	return dhmProxyOptions{
		upstream:        dhmUpstreamURL,
		timeout:         10 * time.Second,
		concurrency:     4,
		cacheMaxEntries: 2048,
		cacheMaxBytes:   64 << 20,
		cacheTTL:        24 * time.Hour,
		now:             time.Now,
		logf:            log.Printf,
		logEvery:        time.Minute,
	}
}

type dhmProxy struct {
	token   string
	opts    dhmProxyOptions
	client  *http.Client
	sem     chan struct{}
	cache   *dhmTileCache
	flight  singleflight.Group
	lastLog atomic.Int64 // unix nanos of the last failure log line
}

// newDHMProxy returns nil unless map.dhm is enabled with a token: an
// inactive proxy is not offered to clients and its endpoint returns 404.
func newDHMProxy(cfg DHMConfig, opts dhmProxyOptions) *dhmProxy {
	if !cfg.active() {
		return nil
	}
	return &dhmProxy{
		token: strings.TrimSpace(cfg.Token),
		opts:  opts,
		client: &http.Client{
			Timeout: opts.timeout,
			// Never follow a redirect: it would resend the token-bearing
			// request to wherever upstream points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sem:   make(chan struct{}, opts.concurrency),
		cache: newDHMTileCache(opts.cacheMaxEntries, opts.cacheMaxBytes, opts.cacheTTL),
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

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

func writeDHMTile(w http.ResponseWriter, data []byte, cacheable bool, state string) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-CoreScope-Tile", state)
	if cacheable {
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(dhmBrowserMaxAge/time.Second)))
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
	key, err := parseDHMTileRequest(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, errDHMBadRequest.Error())
		return
	}
	if !key.intersectsCoverage() {
		writeDHMTile(w, dhmTransparentPNG, true, "outside")
		return
	}
	if data, ok := p.cache.get(key, p.opts.now()); ok {
		writeDHMTile(w, data, true, "hit")
		return
	}
	v, err, _ := p.flight.Do(key.String(), func() (interface{}, error) {
		data, ferr := p.fetch(key)
		if ferr == nil {
			p.cache.put(key, data, p.opts.now())
		}
		return data, ferr
	})
	if err != nil {
		p.logFailure(err)
		writeDHMTile(w, dhmTransparentPNG, false, "unavailable")
		return
	}
	writeDHMTile(w, v.([]byte), true, "miss")
}

// dhmFetchError is an upstream failure reduced to a fixed category. It holds
// no URL and no upstream text, so logging it cannot leak the token.
type dhmFetchError struct{ reason string }

func (e dhmFetchError) Error() string { return e.reason }

func (p *dhmProxy) upstreamURL(key dhmTileKey) string {
	b := key.bbox()
	q := url.Values{}
	q.Set("service", "WMS")
	q.Set("version", "1.3.0")
	q.Set("request", "GetMap")
	q.Set("layers", dhmLayer)
	q.Set("styles", "")
	q.Set("format", "image/png")
	q.Set("transparent", "TRUE")
	q.Set("crs", "EPSG:3857")
	q.Set("width", strconv.Itoa(dhmTileSize))
	q.Set("height", strconv.Itoa(dhmTileSize))
	q.Set("bbox", strings.Join([]string{
		strconv.FormatFloat(b[0], 'f', -1, 64), strconv.FormatFloat(b[1], 'f', -1, 64),
		strconv.FormatFloat(b[2], 'f', -1, 64), strconv.FormatFloat(b[3], 'f', -1, 64),
	}, ","))
	q.Set("token", p.token)
	return p.opts.upstream + "?" + q.Encode()
}

func (p *dhmProxy) fetch(key dhmTileKey) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.opts.timeout)
	defer cancel()
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return nil, dhmFetchError{"busy"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.upstreamURL(key), nil)
	if err != nil {
		return nil, dhmFetchError{"request"}
	}
	req.Header.Set("Accept", "image/png")
	resp, err := p.client.Do(req)
	if err != nil {
		// err is a *url.Error quoting the token-bearing URL: classify it
		// and drop it.
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, dhmFetchError{"timeout"}
		}
		return nil, dhmFetchError{"transport"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, dhmMaxTileBytes))
		return nil, dhmFetchError{"status " + strconv.Itoa(resp.StatusCode)}
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "image/png") {
		return nil, dhmFetchError{"non-image response"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, dhmMaxTileBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, dhmFetchError{"timeout"}
		}
		return nil, dhmFetchError{"read"}
	}
	if len(data) > dhmMaxTileBytes {
		return nil, dhmFetchError{"oversize response"}
	}
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, dhmFetchError{"non-image response"}
	}
	return data, nil
}

func isTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// logFailure logs at most once per logEvery, by category only.
func (p *dhmProxy) logFailure(err error) {
	var fe dhmFetchError
	reason := "unknown"
	if errors.As(err, &fe) {
		reason = fe.reason
	}
	now := p.opts.now().UnixNano()
	last := p.lastLog.Load()
	if p.opts.logEvery > 0 && now-last < int64(p.opts.logEvery) {
		return
	}
	if !p.lastLog.CompareAndSwap(last, now) {
		return
	}
	p.opts.logf("[dhm] upstream tile fetch failed (%s); serving a transparent tile", reason)
}

func newDHMProxyFromConfig(cfg *Config) *dhmProxy {
	if cfg == nil {
		return nil
	}
	return newDHMProxy(cfg.DHM, defaultDHMProxyOptions())
}

func (s *Server) dhmClientConfig() *DHMClientConfig {
	if s.dhm == nil {
		return nil
	}
	return dhmClientConfig()
}

func (s *Server) handleDHMTile(w http.ResponseWriter, r *http.Request) {
	if s.dhm == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.dhm.ServeHTTP(w, r)
}
