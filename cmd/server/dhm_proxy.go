package main

// #680 — DHM (Danmarks Højdemodel) terrain hillshade overlay proxy.
//
// The Map and Live pages can draw the DHM terrain hillshade as an optional
// overlay. The browser requests WMS tiles from /api/map/dhm/wms; this
// handler validates the request against one fixed, allow-listed shape and
// fetches the tile through the shared DHM client (internal/dhm), which
// adds the token server-side in the `token` request header.
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
//
// The endpoint never touches SQLite.

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

const (
	dhmClientPath    = "/api/map/dhm/wms"
	dhmBrowserMaxAge = 24 * time.Hour
)

// DHMConfig is config.json `map.dhm` (Config.DHM).
type DHMConfig = dhm.Config

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

// extractDHMConfig moves map.dhm into the typed DHM field, because the
// free-form Map block is shipped verbatim by /api/config/client. The raw
// block is deleted even when it is malformed: it must never reach clients.
func (c *Config) extractDHMConfig() {
	if c.Map == nil {
		return
	}
	raw, ok := c.Map["dhm"]
	if !ok {
		return
	}
	delete(c.Map, "dhm")
	c.DHM = dhm.Config{}
	b, err := json.Marshal(raw)
	if err == nil {
		err = json.Unmarshal(b, &c.DHM)
	}
	if err != nil {
		c.DHM = dhm.Config{}
		log.Printf("[dhm] config map.dhm is malformed; terrain overlay disabled")
	}
}

// ── Proxy ──────────────────────────────────────────────────────────────────

type dhmProxyOptions struct {
	client          dhm.ClientOptions
	cacheMaxEntries int
	cacheMaxBytes   int
	cacheTTL        time.Duration
}

func defaultDHMProxyOptions() dhmProxyOptions {
	return dhmProxyOptions{
		client:          dhm.DefaultClientOptions(),
		cacheMaxEntries: 2048,
		cacheMaxBytes:   64 << 20,
		cacheTTL:        24 * time.Hour,
	}
}

type dhmProxy struct {
	client *dhm.Client
	cache  *dhm.Cache[dhm.TileKey]
	now    func() time.Time
}

// newDHMProxy returns nil unless map.dhm is enabled with a token: an
// inactive proxy is not offered to clients and its endpoint returns 404.
func newDHMProxy(cfg dhm.Config, opts dhmProxyOptions) *dhmProxy {
	client := dhm.NewClient(cfg, opts.client)
	if client == nil {
		return nil
	}
	return &dhmProxy{
		client: client,
		cache:  dhm.NewCache[dhm.TileKey](opts.cacheMaxEntries, opts.cacheMaxBytes, opts.cacheTTL),
		now:    opts.client.Now,
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
	key, err := dhm.ParseTileRequest(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	if !key.IntersectsCoverage() {
		writeDHMTile(w, dhmTransparentPNG, true, "outside")
		return
	}
	if data, ok := p.cache.Get(key, p.now()); ok {
		writeDHMTile(w, data, true, "hit")
		return
	}
	data, err := p.client.FetchHillshadeTile(key)
	if err != nil {
		p.client.LogFailure("hillshade tile", err)
		writeDHMTile(w, dhmTransparentPNG, false, "unavailable")
		return
	}
	p.cache.Put(key, data, p.now())
	writeDHMTile(w, data, true, "miss")
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
