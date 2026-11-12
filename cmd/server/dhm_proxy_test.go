package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #680 — DHM terrain hillshade proxy. Every test runs against a fake
// upstream (httptest); none reaches Dataforsyningen.

const dhmTestToken = "tok-680-SECRET-do-not-leak"

// pngStub is a minimal valid PNG signature + IHDR start; the proxy only
// checks the signature, so this stands in for an upstream tile.
var pngStub = append([]byte("\x89PNG\r\n\x1a\n"), []byte("stub-hillshade-tile-bytes")...)

// webMercatorTileBBox is the test's own, independent tile→EPSG:3857 bbox
// formula, so a bug in the proxy's tile math cannot also hide in the test.
func webMercatorTileBBox(z, x, y int) (minx, miny, maxx, maxy float64) {
	half := math.Pi * 6378137
	size := 2 * half / math.Exp2(float64(z))
	minx = float64(x)*size - half
	maxx = float64(x+1)*size - half
	maxy = half - float64(y)*size
	miny = half - float64(y+1)*size
	return
}

func lonLatTile(z int, lon, lat float64) (int, int) {
	n := math.Exp2(float64(z))
	x := int((lon + 180) / 360 * n)
	r := lat * math.Pi / 180
	y := int((1 - math.Log(math.Tan(r)+1/math.Cos(r))/math.Pi) / 2 * n)
	return x, y
}

func bboxParam(minx, miny, maxx, maxy float64) string {
	return fmt.Sprintf("%.10f,%.10f,%.10f,%.10f", minx, miny, maxx, maxy)
}

// leafletQuery is the query Leaflet 1.9.4's L.tileLayer.wms sends for the
// overlay options in public/map-tile-providers.js (lowercase keys, 1.3.0 →
// crs, width/height from tileSize).
func leafletQuery(z, x, y int) url.Values {
	minx, miny, maxx, maxy := webMercatorTileBBox(z, x, y)
	q := url.Values{}
	q.Set("service", "WMS")
	q.Set("request", "GetMap")
	q.Set("layers", "dhm_terraen_skyggekort")
	q.Set("styles", "")
	q.Set("format", "image/png")
	q.Set("transparent", "true")
	q.Set("version", "1.3.0")
	q.Set("width", "256")
	q.Set("height", "256")
	q.Set("crs", "EPSG:3857")
	q.Set("bbox", bboxParam(minx, miny, maxx, maxy))
	return q
}

// Aarhus, z10 — inside Denmark.
func dkTile() (int, int, int) {
	x, y := lonLatTile(10, 10.2039, 56.1629)
	return 10, x, y
}

type fakeUpstream struct {
	srv      *httptest.Server
	hits     atomic.Int64
	mu       sync.Mutex
	queries  []url.Values
	tokens   []string // `token` request header per upstream request
	handler  func(w http.ResponseWriter, r *http.Request)
	inFlight atomic.Int64
	maxSeen  atomic.Int64
}

func newFakeUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{handler: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		n := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			m := f.maxSeen.Load()
			if n <= m || f.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.Query())
		f.tokens = append(f.tokens, r.Header.Get("token"))
		f.mu.Unlock()
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngStub)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type dhmLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *dhmLogSink) logf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *dhmLogSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func testDHMOptions(baseURL string, logs *dhmLogSink) dhmProxyOptions {
	o := defaultDHMProxyOptions()
	o.client.BaseURL = baseURL
	o.client.Timeout = 2 * time.Second
	o.client.Logf = logs.logf
	o.client.LogEvery = 0 // tests want every failure logged
	return o
}

func newTestDHMProxy(t *testing.T, up *fakeUpstream, logs *dhmLogSink, tweak func(*dhmProxyOptions)) *dhmProxy {
	t.Helper()
	o := testDHMOptions(up.srv.URL+"/", logs)
	if tweak != nil {
		tweak(&o)
	}
	// The #680 tests specify the terrain upstream; #687's default
	// (exaggerated, with terrain as fallback) is dhm_hillshade_687_test.go.
	p := newDHMProxy(DHMConfig{Enabled: true, Token: dhmTestToken, Layer: "terrain"}, o)
	if p == nil {
		t.Fatal("newDHMProxy returned nil for an enabled config with a token")
	}
	return p
}

func doTile(p http.Handler, q url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/map/dhm/wms?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	return w
}

func assertNoToken(t *testing.T, where string, w *httptest.ResponseRecorder, logs *dhmLogSink) {
	t.Helper()
	if strings.Contains(w.Body.String(), dhmTestToken) {
		t.Fatalf("%s: token leaked into response body: %q", where, w.Body.String())
	}
	for k, vs := range w.Header() {
		for _, v := range vs {
			if strings.Contains(v, dhmTestToken) {
				t.Fatalf("%s: token leaked into response header %s", where, k)
			}
		}
	}
	if logs != nil && strings.Contains(logs.String(), dhmTestToken) {
		t.Fatalf("%s: token leaked into server log: %q", where, logs.String())
	}
}

// ── Valid request shape ────────────────────────────────────────────────────

func TestDHMProxy_ValidTileForwardsFixedUpstreamShape(t *testing.T) {
	up := newFakeUpstream(t, nil)
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, nil)
	z, x, y := dkTile()
	w := doTile(p, leafletQuery(z, x, y))
	if w.Code != 200 {
		t.Fatalf("valid tile: status %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type %q", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), pngStub) {
		t.Fatalf("body is not the upstream tile")
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") || strings.Contains(cc, "no-store") {
		t.Fatalf("tile Cache-Control %q, want a max-age", cc)
	}
	if up.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1", up.hits.Load())
	}
	got := up.queries[0]
	want := map[string]string{
		"service": "WMS", "version": "1.3.0", "request": "GetMap",
		"layers": "dhm_terraen_skyggekort", "styles": "", "format": "image/png",
		"transparent": "TRUE", "crs": "EPSG:3857", "width": "256", "height": "256",
	}
	for k, v := range want {
		if vs, ok := got[k]; !ok || len(vs) != 1 || vs[0] != v {
			t.Errorf("upstream %s = %v, want %q", k, got[k], v)
		}
	}
	// bbox is rebuilt from the tile, not copied from the client string.
	minx, miny, maxx, maxy := webMercatorTileBBox(z, x, y)
	parts := strings.Split(got.Get("bbox"), ",")
	if len(parts) != 4 {
		t.Fatalf("upstream bbox %q", got.Get("bbox"))
	}
	for i, wantV := range []float64{minx, miny, maxx, maxy} {
		var v float64
		fmt.Sscan(parts[i], &v)
		if math.Abs(v-wantV) > 1e-6 {
			t.Errorf("upstream bbox[%d]=%v want %v", i, v, wantV)
		}
	}
	if len(got) != len(want)+1 {
		t.Errorf("upstream got extra params: %v", got)
	}
	// The token travels in the `token` request header, never in the URL.
	if up.tokens[0] != dhmTestToken {
		t.Errorf("upstream token header %q, want the configured token", up.tokens[0])
	}
	for k := range got {
		if strings.EqualFold(k, "token") {
			t.Errorf("token sent in the upstream query string")
		}
	}
	assertNoToken(t, "valid tile", w, logs)
}

// ── Parameter validation ───────────────────────────────────────────────────

func TestDHMProxy_RejectsNonAllowListedRequests(t *testing.T) {
	up := newFakeUpstream(t, nil)
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, nil)
	z, x, y := dkTile()
	minx, miny, maxx, maxy := webMercatorTileBBox(z, x, y)
	size := maxx - minx

	set := func(k, v string) func(url.Values) { return func(q url.Values) { q.Set(k, v) } }
	del := func(k string) func(url.Values) { return func(q url.Values) { q.Del(k) } }
	cases := []struct {
		name string
		mut  func(url.Values)
	}{
		// layers
		{"other layer", set("layers", "dhm_overflade_skyggekort")},
		{"two layers", set("layers", "dhm_terraen_skyggekort,dhm_kurve_traditionel")},
		{"layer case variant", set("layers", "DHM_TERRAEN_SKYGGEKORT")},
		{"missing layers", del("layers")},
		// format
		{"jpeg format", set("format", "image/jpeg")},
		{"xml format", set("format", "text/xml")},
		// size
		{"width 512", set("width", "512")},
		{"height 512", set("height", "512")},
		{"width 10000", set("width", "10000")},
		{"width not a number", set("width", "256px")},
		// bbox
		{"bbox off-grid", set("bbox", bboxParam(minx+size/3, miny, maxx+size/3, maxy))},
		{"bbox huge", set("bbox", bboxParam(-2e7, -2e7, 2e7, 2e7))},
		{"bbox not square", set("bbox", bboxParam(minx, miny, maxx+size, maxy))},
		{"bbox inverted", set("bbox", bboxParam(maxx, maxy, minx, miny))},
		{"bbox three numbers", set("bbox", "1,2,3")},
		{"bbox NaN", set("bbox", "NaN,NaN,NaN,NaN")},
		{"bbox Inf", set("bbox", "-Inf,-Inf,Inf,Inf")},
		{"bbox text", set("bbox", "a,b,c,d")},
		{"bbox zoom too low", func(q url.Values) {
			a, b, c, d := webMercatorTileBBox(3, 4, 2)
			q.Set("bbox", bboxParam(a, b, c, d))
		}},
		{"bbox zoom too high", func(q url.Values) {
			a, b, c, d := webMercatorTileBBox(22, 2216000, 1286000)
			q.Set("bbox", bboxParam(a, b, c, d))
		}},
		{"missing bbox", del("bbox")},
		// fixed WMS shape
		{"GetCapabilities", set("request", "GetCapabilities")},
		{"GetFeatureInfo", set("request", "GetFeatureInfo")},
		{"wms 1.1.1", set("version", "1.1.1")},
		{"crs 25832", set("crs", "EPSG:25832")},
		{"service WFS", set("service", "WFS")},
		{"styles set", set("styles", "default")},
		{"transparent false", set("transparent", "false")},
		// no smuggled parameters
		{"client token", set("token", "attacker")},
		{"client TOKEN", set("TOKEN", "attacker")},
		{"url param", set("url", "http://metadata.internal/")},
		{"srs alongside crs", set("srs", "EPSG:3857")},
		{"uppercase key", func(q url.Values) { q.Set("LAYERS", q.Get("layers")); q.Del("layers") }},
		{"duplicate bbox", func(q url.Values) { q.Add("bbox", q.Get("bbox")) }},
		{"duplicate layers", func(q url.Values) { q.Add("layers", "dhm_overflade_skyggekort") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := leafletQuery(z, x, y)
			tc.mut(q)
			before := up.hits.Load()
			w := doTile(p, q)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if up.hits.Load() != before {
				t.Fatalf("rejected request reached upstream")
			}
			// The error text is fixed: it never echoes the client's values.
			if strings.Contains(w.Body.String(), "attacker") || strings.Contains(w.Body.String(), "metadata.internal") {
				t.Fatalf("error echoes client input: %s", w.Body.String())
			}
			assertNoToken(t, tc.name, w, logs)
		})
	}
}

func TestDHMProxy_RejectsNonGET(t *testing.T) {
	up := newFakeUpstream(t, nil)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, nil)
	z, x, y := dkTile()
	req := httptest.NewRequest("POST", "/api/map/dhm/wms?"+leafletQuery(z, x, y).Encode(), nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed || up.hits.Load() != 0 {
		t.Fatalf("POST: status %d, upstream hits %d", w.Code, up.hits.Load())
	}
}

// Outside DHM coverage the tile is transparent and upstream is never asked.
func TestDHMProxy_OutsideDenmarkIsTransparentWithoutUpstream(t *testing.T) {
	up := newFakeUpstream(t, nil)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, nil)
	for _, pt := range [][2]float64{{13.405, 52.52}, {-0.1276, 51.5072}, {18.0686, 59.3293}} {
		x, y := lonLatTile(10, pt[0], pt[1])
		w := doTile(p, leafletQuery(10, x, y))
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("outside tile %v: status %d ct %q", pt, w.Code, w.Header().Get("Content-Type"))
		}
		if !bytes.Equal(w.Body.Bytes(), dhmTransparentPNG) {
			t.Fatalf("outside tile %v is not the transparent tile", pt)
		}
	}
	if up.hits.Load() != 0 {
		t.Fatalf("outside-Denmark tiles hit upstream %d times", up.hits.Load())
	}
}

// ── Disabled / missing token ───────────────────────────────────────────────

func TestDHMProxy_DisabledOrMissingTokenIs404AndNotOffered(t *testing.T) {
	z, x, y := dkTile()
	for _, tc := range []struct {
		name string
		cfg  DHMConfig
	}{
		{"disabled with token", DHMConfig{Enabled: false, Token: dhmTestToken}},
		{"enabled without token", DHMConfig{Enabled: true}},
		{"enabled with blank token", DHMConfig{Enabled: true, Token: "   "}},
		{"unset", DHMConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if newDHMProxy(tc.cfg, defaultDHMProxyOptions()) != nil {
				t.Fatalf("proxy constructed for %+v", tc.cfg)
			}
			srv, router := setupTestServer(t)
			srv.cfg.DHM = tc.cfg
			srv.dhm = newDHMProxy(tc.cfg, defaultDHMProxyOptions())

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/map/dhm/wms?"+leafletQuery(z, x, y).Encode(), nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("tile endpoint status %d, want 404", w.Code)
			}
			assertNoToken(t, tc.name, w, nil)

			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["dhm"]; ok {
				t.Fatalf("client config offers dhm while inactive: %s", body["dhm"])
			}
		})
	}
}

func TestDHMProxy_EnabledIsOfferedAndRouted(t *testing.T) {
	up := newFakeUpstream(t, nil)
	srv, router := setupTestServer(t)
	srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
	srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if strings.Contains(w.Body.String(), dhmTestToken) {
		t.Fatalf("token in /api/config/client: %s", w.Body.String())
	}
	var body struct {
		DHM *DHMClientConfig `json:"dhm"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.DHM == nil || !body.DHM.Enabled || body.DHM.URL != "/api/map/dhm/wms" ||
		body.DHM.Layer != "dhm_terraen_skyggekort" {
		t.Fatalf("client dhm block %+v", body.DHM)
	}
	// The overlay credits the shared CC BY 4.0 attribution constant.
	if body.DHM.Attribution != dhm.Attribution || !strings.Contains(body.DHM.Attribution, "CC BY 4.0") {
		t.Fatalf("client dhm attribution %q, want dhm.Attribution with CC BY 4.0", body.DHM.Attribution)
	}

	z, x, y := dkTile()
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", body.DHM.URL+"?"+leafletQuery(z, x, y).Encode(), nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pngStub) {
		t.Fatalf("routed tile: status %d", w.Code)
	}
	// The /api no-store default must not override the tile's own caching.
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Fatalf("routed tile Cache-Control %q", cc)
	}
}

// ── Token never leaves the server ──────────────────────────────────────────

// LoadConfig moves map.dhm out of the free-form map block, so the block that
// /api/config/client ships verbatim can never carry the token.
func TestDHMConfig_TokenNeverInClientConfig(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{"map":{"tiles":{"darkDefault":"osm-dark"},"dhm":{"enabled":true,"token":"` + dhmTestToken + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DHM.Enabled || cfg.DHM.Token != dhmTestToken {
		t.Fatalf("typed DHM config not loaded: enabled=%v tokenLen=%d", cfg.DHM.Enabled, len(cfg.DHM.Token))
	}
	if _, ok := cfg.Map["dhm"]; ok {
		t.Fatalf("map.dhm still present in the client-visible map block")
	}
	if _, ok := cfg.Map["tiles"]; !ok {
		t.Fatalf("map.tiles lost while extracting dhm")
	}

	srv, router := setupTestServer(t)
	srv.cfg.Map = cfg.Map
	srv.cfg.DHM = cfg.DHM
	srv.dhm = newDHMProxy(cfg.DHM, defaultDHMProxyOptions())
	for _, path := range []string{"/api/config/client", "/api/config/map", "/api/config/cache", "/api/config/theme"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if strings.Contains(w.Body.String(), dhmTestToken) {
			t.Fatalf("token leaked via %s", path)
		}
	}
}

func TestDHMConfig_MalformedBlockIsDroppedNotShipped(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{"map":{"dhm":{"enabled":"yes","token":"` + dhmTestToken + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Map["dhm"]; ok {
		t.Fatalf("malformed map.dhm left in the client-visible map block")
	}
	if cfg.DHM.Enabled {
		t.Fatalf("malformed map.dhm enabled the proxy")
	}
}

// ── Upstream errors: transparent tile, no details, no token ────────────────

func TestDHMProxy_UpstreamErrorsDoNotLeak(t *testing.T) {
	const detail = "ORA-01017 internal upstream detail"
	cases := []struct {
		name string
		h    func(w http.ResponseWriter, r *http.Request)
	}{
		{"500 with echoed url", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			io.WriteString(w, detail+" "+r.URL.String()+" "+r.Header.Get("token"))
		}},
		{"401 json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"User not authorized","token":"`+r.Header.Get("token")+`"}`)
		}},
		// Dataforsyningen answers a missing or bad token with 403. The WMS
		// sends an OGC exception (verified against dhm_DAF); other services
		// send the bare text `User not authorized`.
		{"403 plain text", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(403)
			io.WriteString(w, "User not authorized "+detail+" "+r.Header.Get("token"))
		}},
		{"403 OGC exception", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(403)
			io.WriteString(w, `<ServiceExceptionReport><ServiceException>User not authorized: `+detail+` `+r.Header.Get("token")+`</ServiceException></ServiceExceptionReport>`)
		}},
		{"200 OGC exception", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			io.WriteString(w, `<ServiceExceptionReport><ServiceException>`+detail+` `+r.Header.Get("token")+`</ServiceException></ServiceExceptionReport>`)
		}},
		{"200 image/png header but not a png", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, detail+r.Header.Get("token"))
		}},
		{"oversize png", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngStub)
			w.Write(bytes.Repeat([]byte{0}, 2<<20))
		}},
	}
	z, x, y := dkTile()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, tc.h)
			logs := &dhmLogSink{}
			p := newTestDHMProxy(t, up, logs, nil)
			w := doTile(p, leafletQuery(z, x, y))
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), dhmTransparentPNG) {
				t.Fatalf("status %d body %q, want transparent tile", w.Code, w.Body.String())
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("error tile Cache-Control %q, want no-store", cc)
			}
			if strings.Contains(w.Body.String(), "ORA-") || strings.Contains(logs.String(), "ORA-") ||
				strings.Contains(w.Body.String(), "not authorized") || strings.Contains(logs.String(), "not authorized") {
				t.Fatalf("upstream detail leaked")
			}
			if logs.String() == "" {
				t.Fatalf("upstream failure was not logged at all")
			}
			assertNoToken(t, tc.name, w, logs)
			// An error is never cached: the next request asks upstream again.
			doTile(p, leafletQuery(z, x, y))
			if up.hits.Load() < 2 {
				t.Fatalf("failed tile was cached (upstream hits %d)", up.hits.Load())
			}
		})
	}
}

// A redirect would replay the token header to wherever upstream points;
// the proxy must not follow it.
func TestDHMProxy_DoesNotFollowRedirects(t *testing.T) {
	elsewhere := newFakeUpstream(t, nil)
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/steal?"+r.URL.RawQuery, http.StatusFound)
	})
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, nil)
	z, x, y := dkTile()
	w := doTile(p, leafletQuery(z, x, y))
	if elsewhere.hits.Load() != 0 {
		t.Fatalf("redirect followed: the token-bearing request reached another host")
	}
	if len(elsewhere.tokens) != 0 {
		t.Fatalf("token header reached the redirect target")
	}
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), dhmTransparentPNG) {
		t.Fatalf("status %d, want transparent tile", w.Code)
	}
	assertNoToken(t, "redirect", w, logs)
}

// A transport error (net/http's *url.Error) quotes the full upstream URL.
// The token is not in that URL, and the error must still never reach the
// response or the log.
func TestDHMProxy_TransportErrorDoesNotLogTokenURL(t *testing.T) {
	up := newFakeUpstream(t, nil)
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, nil)
	up.srv.Close() // connection refused
	z, x, y := dkTile()
	w := doTile(p, leafletQuery(z, x, y))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), dhmTransparentPNG) {
		t.Fatalf("status %d", w.Code)
	}
	if logs.String() == "" {
		t.Fatalf("transport failure not logged")
	}
	assertNoToken(t, "transport error", w, logs)
}

func TestDHMProxy_UpstreamTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, func(o *dhmProxyOptions) { o.client.Timeout = 150 * time.Millisecond })
	z, x, y := dkTile()
	start := time.Now()
	w := doTile(p, leafletQuery(z, x, y))
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("upstream timeout not enforced: took %v", el)
	}
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), dhmTransparentPNG) {
		t.Fatalf("status %d", w.Code)
	}
	assertNoToken(t, "timeout", w, logs)
}

// ── Concurrency limit towards upstream ─────────────────────────────────────

func TestDHMProxy_UpstreamConcurrencyLimit(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngStub)
	})
	p := newTestDHMProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.client.Concurrency = 2 })
	z, x0, y0 := dkTile()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doTile(p, leafletQuery(z, x0+i%4, y0+i/4))
		}(i)
	}
	wg.Wait()
	if m := up.maxSeen.Load(); m > 2 {
		t.Fatalf("upstream saw %d concurrent requests, limit is 2", m)
	}
	if up.hits.Load() != 12 {
		t.Fatalf("upstream hits %d, want 12 distinct tiles", up.hits.Load())
	}
}

// ── Bounded cache + TTL ────────────────────────────────────────────────────

func TestDHMProxy_CacheHitsAndEntryBound(t *testing.T) {
	up := newFakeUpstream(t, nil)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.cacheMaxEntries = 3 })
	z, x, y := dkTile()
	tile := func(i int) url.Values { return leafletQuery(z, x+i, y) }

	doTile(p, tile(0))
	if w := doTile(p, tile(0)); w.Header().Get("X-CoreScope-Tile") != "hit" {
		t.Fatalf("second request not a cache hit")
	}
	if up.hits.Load() != 1 {
		t.Fatalf("cached tile re-fetched: hits %d", up.hits.Load())
	}
	for i := 1; i <= 3; i++ {
		doTile(p, tile(i))
	}
	if n := p.cache.Len(); n > 3 {
		t.Fatalf("cache holds %d entries, bound is 3", n)
	}
	// tile 0 is the least recently used → evicted → upstream again.
	before := up.hits.Load()
	doTile(p, tile(0))
	if up.hits.Load() != before+1 {
		t.Fatalf("LRU tile 0 was not evicted")
	}
}

func TestDHMProxy_CacheByteBound(t *testing.T) {
	up := newFakeUpstream(t, nil)
	per := len(pngStub)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) {
		o.cacheMaxEntries = 100
		o.cacheMaxBytes = per*2 + per/2
	})
	z, x, y := dkTile()
	for i := 0; i < 5; i++ {
		doTile(p, leafletQuery(z, x+i, y))
	}
	if b := p.cache.Size(); b > per*2+per/2 {
		t.Fatalf("cache holds %d bytes, bound is %d", b, per*2+per/2)
	}
	if n := p.cache.Len(); n != 2 {
		t.Fatalf("cache holds %d entries, want 2 under the byte bound", n)
	}
}

func TestDHMProxy_CacheTTLEviction(t *testing.T) {
	up := newFakeUpstream(t, nil)
	var mu sync.Mutex
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) {
		o.cacheTTL = time.Hour
		o.client.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	z, x, y := dkTile()
	doTile(p, leafletQuery(z, x, y))
	mu.Lock()
	now = now.Add(59 * time.Minute)
	mu.Unlock()
	doTile(p, leafletQuery(z, x, y))
	if up.hits.Load() != 1 {
		t.Fatalf("tile re-fetched before TTL: hits %d", up.hits.Load())
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	doTile(p, leafletQuery(z, x, y))
	if up.hits.Load() != 2 {
		t.Fatalf("expired tile served from cache: hits %d", up.hits.Load())
	}
	if p.cache.Len() != 1 {
		t.Fatalf("expired entry not replaced: len %d", p.cache.Len())
	}
}

// Concurrent requests for one tile share one upstream fetch.
func TestDHMProxy_CoalescesSameTile(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngStub)
	})
	p := newTestDHMProxy(t, up, &dhmLogSink{}, nil)
	z, x, y := dkTile()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); doTile(p, leafletQuery(z, x, y)) }()
	}
	wg.Wait()
	if up.hits.Load() != 1 {
		t.Fatalf("8 concurrent requests for one tile made %d upstream calls", up.hits.Load())
	}
}

// Leaflet derives each bbox by unprojecting tile corners to lat/lng and
// projecting them back, so its numbers carry float noise. These are bbox
// strings recorded from Leaflet 1.9.4 in Chromium (test-issue-680 E2E); the
// proxy must accept them as the tiles they are.
func TestDHMProxy_AcceptsLeafletBBoxes(t *testing.T) {
	up := newFakeUpstream(t, nil)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, nil)
	for _, tc := range dhmLeafletRecordedBBoxes {
		q := leafletQuery(tc.z, tc.x, tc.y)
		q.Set("bbox", tc.bbox)
		w := doTile(p, q)
		if w.Code != 200 {
			t.Fatalf("Leaflet bbox %q (z%d/%d/%d): status %d", tc.bbox, tc.z, tc.x, tc.y, w.Code)
		}
		key, err := dhm.ParseTileRequest(q)
		if err != nil || key != (dhm.TileKey{Z: tc.z, X: tc.x, Y: tc.y}) {
			t.Fatalf("Leaflet bbox %q parsed as %+v (%v), want z%d/%d/%d", tc.bbox, key, err, tc.z, tc.x, tc.y)
		}
	}
}

// Recorded from Leaflet 1.9.4 (Chromium, L.tileLayer.wms with the overlay
// options) at zooms 7–19 over Denmark; across 98 recorded tiles the largest
// deviation from the exact tile edge was 3e-11 of a tile, far inside the
// proxy's 1e-6 tolerance.
var dhmLeafletRecordedBBoxes = []struct {
	z, x, y int
	bbox    string
}{
	{7, 68, 39, "1252344.2714243277,7514065.628545967,1565430.3392804097,7827151.696402045"},
	{7, 68, 38, "1252344.2714243277,7827151.696402045,1565430.3392804097,8140237.764258131"},
	{10, 538, 319, "1017529.7205322665,7514065.628545967,1056665.4790142768,7553201.38702798"},
	{10, 538, 318, "1017529.7205322665,7553201.38702798,1056665.4790142768,7592337.145509989"},
	{11, 1078, 636, "1056665.4790142768,7572769.266268986,1076233.3582552818,7592337.145509989"},
	{11, 1079, 637, "1076233.3582552818,7553201.38702798,1095801.2374962869,7572769.266268986"},
	{14, 8762, 5129, "1394211.395921615,7489605.779494709,1396657.3808267405,7492051.764399834"},
	{14, 8762, 5128, "1394211.395921615,7492051.764399834,1396657.3808267405,7494497.749304961"},
	{18, 138295, 80246, "1104209.3106076561,7769823.925188166,1104362.1846642266,7769976.799244742"},
	{18, 138295, 80245, "1104209.3106076561,7769976.799244742,1104362.1846642266,7770129.67330131"},
	{19, 273938, 166086, "901498.3115953689,7342311.625989176,901574.7486236541,7342388.06301746"},
	{19, 273938, 166085, "901498.3115953689,7342388.06301746,901574.7486236541,7342464.500045745"},
}

// encoding/json keeps decoding after a type error, so a config.json that
// LoadConfig rejects (and falls back from) can still have filled Map —
// token included. The fallback path must strip it as well.
func TestDHMConfig_PartiallyParsedConfigStillStripsToken(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{"map":{"dhm":{"enabled":true,"token":"` + dhmTestToken + `"}},"areas":["not-an-object"]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Map["dhm"]; ok {
		t.Fatalf("map.dhm (with token) left in the client-visible map block after a partial parse")
	}
	srv, router := setupTestServer(t)
	srv.cfg.Map = cfg.Map
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if strings.Contains(w.Body.String(), dhmTestToken) {
		t.Fatalf("token leaked via /api/config/client after a partial parse")
	}
}

// The shipped example documents map.dhm disabled; loading it must leave the
// proxy off and keep the block out of what clients receive.
func TestDHMConfig_ExampleConfigIsOffAndStripped(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Map["dhm"]; ok {
		t.Fatalf("example map.dhm left in the client-visible map block")
	}
	if cfg.DHM.Active() || newDHMProxyFromConfig(cfg) != nil {
		t.Fatalf("example config enables the DHM proxy")
	}
}

// #682 review F1: Config.Map is a map[string]interface{}, so a key spelled
// `DHM`, `Dhm` or `dHm` escaped the exact-key strip and its token shipped in
// /api/config/client. Every case variant must be stripped, must not enable
// the proxy, and must be warned about without the token.
func TestDHMConfig_CaseVariantKeysAreStripped(t *testing.T) {
	for _, key := range []string{"DHM", "Dhm", "dHm"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			cfgJSON := `{"map":{"tiles":{},"` + key + `":{"enabled":true,"token":"` + dhmTestToken + `"}}}`
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			var logBuf bytes.Buffer
			log.SetOutput(&logBuf)
			cfg, err := LoadConfig(dir)
			log.SetOutput(os.Stderr)
			if err != nil {
				t.Fatal(err)
			}
			for k := range cfg.Map {
				if strings.EqualFold(k, "dhm") {
					t.Fatalf("map.%s left in the client-visible map block", k)
				}
			}
			if cfg.DHM.Active() || newDHMProxyFromConfig(cfg) != nil {
				t.Fatalf("map.%s enabled the proxy; only the canonical map.dhm may", key)
			}
			if !strings.Contains(logBuf.String(), "map."+key) {
				t.Fatalf("no warning naming map.%s: %q", key, logBuf.String())
			}
			if strings.Contains(logBuf.String(), dhmTestToken) {
				t.Fatalf("token in the warning: %q", logBuf.String())
			}
			srv, router := setupTestServer(t)
			srv.cfg.Map = cfg.Map
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
			if strings.Contains(w.Body.String(), dhmTestToken) {
				t.Fatalf("token from map.%s leaked via /api/config/client", key)
			}
		})
	}
}

// The canonical key wins when a case variant sits next to it, and the
// variant is still stripped.
func TestDHMConfig_CanonicalKeyWinsOverCaseVariant(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{"map":{"DHM":{"enabled":true,"token":"other-` + dhmTestToken + `"},"dhm":{"enabled":true,"token":"` + dhmTestToken + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DHM.Token != dhmTestToken || !cfg.DHM.Active() {
		t.Fatalf("canonical map.dhm not used: active=%v", cfg.DHM.Active())
	}
	for k := range cfg.Map {
		if strings.EqualFold(k, "dhm") {
			t.Fatalf("map.%s left in the client-visible map block", k)
		}
	}
}

// #682 review N2: every mandatory query parameter is declared in OpenAPI,
// so a client generated from the schema sends a request the proxy accepts.
func TestDHMOpenAPIDeclaresEveryRequiredParam(t *testing.T) {
	meta, ok := routeDescriptions()["GET /api/map/dhm/wms"]
	if !ok {
		t.Fatal("route not described")
	}
	declared := map[string]bool{}
	for _, p := range meta.QueryParams {
		if !p.Required {
			t.Errorf("param %s not marked required", p.Name)
		}
		declared[p.Name] = true
	}
	z, x, y := dkTile()
	q := leafletQuery(z, x, y)
	for k := range q {
		if !declared[k] {
			t.Errorf("mandatory query param %q not declared in OpenAPI", k)
		}
	}
	if len(declared) != len(q) {
		t.Errorf("OpenAPI declares %d params, the proxy accepts exactly %d", len(declared), len(q))
	}
	if _, err := dhm.ParseTileRequest(q); err != nil {
		t.Fatalf("leafletQuery is not an accepted request: %v", err)
	}
}
