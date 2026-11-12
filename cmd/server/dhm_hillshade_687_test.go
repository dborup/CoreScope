package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #687 — the exaggerated hillshade (wms/dhm_temp) by default, with the #680
// terrain hillshade (dhm_DAF) as per-tile fallback. Every test runs against
// fake upstreams (httptest); none reaches Dataforsyningen.

const (
	exaggeratedPath  = "/wms/dhm_temp"
	terrainPath      = "/dhm_DAF"
	exaggeratedLayer = "quickDHM_skyggekort_terraen_overdrevet"
	terrainLayer     = "dhm_terraen_skyggekort"
)

var (
	exaggeratedStub = append([]byte("\x89PNG\r\n\x1a\n"), []byte("exaggerated-tile")...)
	terrainStub     = append([]byte("\x89PNG\r\n\x1a\n"), []byte("terrain-tile")...)
)

// dhmService is one fake upstream WMS: it records what it was asked and
// answers with handler, or a valid PNG stub.
type dhmService struct {
	mu      sync.Mutex
	stub    []byte
	handler func(w http.ResponseWriter, r *http.Request)
	queries []url.Values
	tokens  []string
}

func (s *dhmService) set(h func(w http.ResponseWriter, r *http.Request)) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

func (s *dhmService) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
}

func (s *dhmService) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.queries = append(s.queries, r.URL.Query())
	s.tokens = append(s.tokens, r.Header.Get("token"))
	h := s.handler
	s.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(s.stub)
}

// dhmUpstreams is Dataforsyningen faked: an exaggerated and a terrain WMS
// under one API root, plus a count of requests to any other path.
type dhmUpstreams struct {
	srv         *httptest.Server
	exaggerated *dhmService
	terrain     *dhmService
	mu          sync.Mutex
	stray       []string
}

func newDHMUpstreams(t *testing.T) *dhmUpstreams {
	t.Helper()
	u := &dhmUpstreams{
		exaggerated: &dhmService{stub: exaggeratedStub},
		terrain:     &dhmService{stub: terrainStub},
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exaggeratedPath:
			u.exaggerated.serve(w, r)
		case terrainPath:
			u.terrain.serve(w, r)
		default:
			u.mu.Lock()
			u.stray = append(u.stray, r.URL.Path)
			u.mu.Unlock()
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func newLayerProxy(t *testing.T, u *dhmUpstreams, layer string, logs *dhmLogSink, tweak func(*dhmProxyOptions)) *dhmProxy {
	t.Helper()
	o := testDHMOptions(u.srv.URL+"/", logs)
	if tweak != nil {
		tweak(&o)
	}
	p := newDHMProxy(DHMConfig{Enabled: true, Token: dhmTestToken, Layer: layer}, o)
	if p == nil {
		t.Fatal("newDHMProxy returned nil for an enabled config with a token")
	}
	return p
}

// assertUpstreamShape checks one upstream query: the fixed #680 GetMap
// shape with the given layer, and never a token.
func assertUpstreamShape(t *testing.T, where string, got url.Values, layer string) {
	t.Helper()
	want := map[string]string{
		"service": "WMS", "version": "1.3.0", "request": "GetMap",
		"layers": layer, "styles": "", "format": "image/png",
		"transparent": "TRUE", "crs": "EPSG:3857", "width": "256", "height": "256",
	}
	for k, v := range want {
		if vs, ok := got[k]; !ok || len(vs) != 1 || vs[0] != v {
			t.Errorf("%s: upstream %s = %v, want %q", where, k, got[k], v)
		}
	}
	if len(got) != len(want)+1 || got.Get("bbox") == "" {
		t.Errorf("%s: upstream params %v", where, got)
	}
	for k, vs := range got {
		if strings.EqualFold(k, "token") || strings.Contains(strings.Join(vs, ","), dhmTestToken) {
			t.Errorf("%s: token in the upstream URL", where)
		}
	}
}

func assertTokensInHeader(t *testing.T, s *dhmService) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, tok := range s.tokens {
		if tok != dhmTestToken {
			t.Errorf("upstream request %d: token header %q", i, tok)
		}
	}
}

func assertTile(t *testing.T, where string, w *httptest.ResponseRecorder, body []byte, state, cacheControl string) {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("%s: status %d content-type %q", where, w.Code, w.Header().Get("Content-Type"))
	}
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("%s: body %q, want %q", where, w.Body.Bytes(), body)
	}
	if got := w.Header().Get("X-CoreScope-Tile"); got != state {
		t.Fatalf("%s: X-CoreScope-Tile %q, want %q", where, got, state)
	}
	if got := w.Header().Get("Cache-Control"); got != cacheControl {
		t.Fatalf("%s: Cache-Control %q, want %q", where, got, cacheControl)
	}
}

const (
	ccTile     = "public, max-age=86400"
	ccFallback = "public, max-age=600"
	ccNoStore  = "no-store"
)

// ── Layer choice ───────────────────────────────────────────────────────────

func TestDHM687_DefaultAndExplicitExaggerated(t *testing.T) {
	for _, layer := range []string{"", "exaggerated"} {
		t.Run("layer="+layer, func(t *testing.T) {
			u := newDHMUpstreams(t)
			logs := &dhmLogSink{}
			p := newLayerProxy(t, u, layer, logs, nil)
			z, x, y := dkTile()
			assertTile(t, "first", doTile(p, leafletQuery(z, x, y)), exaggeratedStub, "miss", ccTile)
			assertTile(t, "second", doTile(p, leafletQuery(z, x, y)), exaggeratedStub, "hit", ccTile)
			if u.exaggerated.hits() != 1 || u.terrain.hits() != 0 || len(u.stray) != 0 {
				t.Fatalf("hits exaggerated=%d terrain=%d stray=%v", u.exaggerated.hits(), u.terrain.hits(), u.stray)
			}
			assertUpstreamShape(t, "exaggerated", u.exaggerated.queries[0], exaggeratedLayer)
			assertTokensInHeader(t, u.exaggerated)
			if logs.String() != "" {
				t.Fatalf("unexpected log: %q", logs.String())
			}
		})
	}
}

func TestDHM687_TerrainLayer(t *testing.T) {
	u := newDHMUpstreams(t)
	logs := &dhmLogSink{}
	p := newLayerProxy(t, u, "terrain", logs, nil)
	z, x, y := dkTile()
	assertTile(t, "first", doTile(p, leafletQuery(z, x, y)), terrainStub, "miss", ccTile)
	assertTile(t, "second", doTile(p, leafletQuery(z, x, y)), terrainStub, "hit", ccTile)
	if u.exaggerated.hits() != 0 || u.terrain.hits() != 1 {
		t.Fatalf("hits exaggerated=%d terrain=%d", u.exaggerated.hits(), u.terrain.hits())
	}
	assertUpstreamShape(t, "terrain", u.terrain.queries[0], terrainLayer)
	assertTokensInHeader(t, u.terrain)

	// A failing terrain upstream has no fallback: the #680 transparent tile.
	u.terrain.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	assertTile(t, "terrain down", doTile(p, leafletQuery(z, x+1, y)), dhmTransparentPNG, "unavailable", ccNoStore)
	if u.exaggerated.hits() != 0 {
		t.Fatalf("terrain config called the exaggerated service")
	}
}

func TestDHM687_InvalidLayerWarnsAndUsesTerrain(t *testing.T) {
	for _, layer := range []string{"Exaggerated", "TERRAIN", " terrain", "hillshade", exaggeratedLayer, dhmTestToken} {
		t.Run(layer, func(t *testing.T) {
			u := newDHMUpstreams(t)
			logs := &dhmLogSink{}
			p := newLayerProxy(t, u, layer, logs, nil)
			if n := strings.Count(logs.String(), "map.dhm.layer"); n != 1 {
				t.Fatalf("want one map.dhm.layer warning, log %q", logs.String())
			}
			if !strings.Contains(logs.String(), `using "terrain"`) {
				t.Fatalf("warning does not name the fallback: %q", logs.String())
			}
			z, x, y := dkTile()
			w := doTile(p, leafletQuery(z, x, y))
			assertTile(t, "tile", w, terrainStub, "miss", ccTile)
			if u.exaggerated.hits() != 0 || u.terrain.hits() != 1 {
				t.Fatalf("hits exaggerated=%d terrain=%d", u.exaggerated.hits(), u.terrain.hits())
			}
			// The warning never echoes the configured value: it could be a
			// misplaced token.
			assertNoToken(t, "invalid layer", w, logs)
		})
	}
}

// map.dhm.layer is read from config.json, and stays out of what clients get.
func TestDHM687_LayerFromConfigFile(t *testing.T) {
	for layer, want := range map[string]string{`"terrain"`: "terrain", `"exaggerated"`: "exaggerated"} {
		dir := t.TempDir()
		cfgJSON := `{"map":{"dhm":{"enabled":true,"token":"` + dhmTestToken + `","layer":` + layer + `}}}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DHM.Layer != want || !cfg.DHM.Active() {
			t.Fatalf("layer %s: loaded %q active=%v", layer, cfg.DHM.Layer, cfg.DHM.Active())
		}
		if _, ok := cfg.Map["dhm"]; ok {
			t.Fatalf("map.dhm left in the client-visible map block")
		}
	}
}

// The browser contract is #680's: the client config still names the
// terrain layer, whatever upstream the server uses.
func TestDHM687_ClientConfigUnchanged(t *testing.T) {
	c := dhmClientConfig()
	if c.Layer != terrainLayer || c.URL != "/api/map/dhm/wms" {
		t.Fatalf("client config %+v", c)
	}
	if got := dhm.TileParams()["layers"]; got != terrainLayer {
		t.Fatalf("browser layers param %q", got)
	}
}

// ── Fallback ───────────────────────────────────────────────────────────────

func TestDHM687_FallbackToTerrain(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		h      func(w http.ResponseWriter, r *http.Request)
		// net/http retries a GET once when a reused keep-alive connection
		// dies, so a transport error may reach the fake upstream twice.
		retried bool
	}{
		{"404", "status 404", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			io.WriteString(w, "gone "+r.URL.String()+" "+r.Header.Get("token"))
		}, false},
		{"500", "status 500", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			io.WriteString(w, r.Header.Get("token"))
		}, false},
		{"transport", "transport", func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.(*net.TCPConn).SetLinger(0)
				conn.Close()
			}
		}, true},
		{"content type", "unexpected content type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.ogc.se_xml")
			io.WriteString(w, "<ServiceExceptionReport>"+r.Header.Get("token"))
		}, false},
		{"invalid png", "non-image response", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "not a png "+r.Header.Get("token"))
		}, false},
		{"redirect", "status 302", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newDHMUpstreams(t)
			logs := &dhmLogSink{}
			p := newLayerProxy(t, u, "", logs, nil)
			u.exaggerated.set(tc.h)
			z, x, y := dkTile()
			q := leafletQuery(z, x, y)
			// exaggeratedHits checks the exaggerated upstream saw want
			// requests (for a transport error: at most one retry each).
			exaggeratedHits := func(want int) {
				t.Helper()
				n := u.exaggerated.hits()
				if n != want && !(tc.retried && n > want && n <= 2*want) {
					t.Fatalf("exaggerated hits %d, want %d", n, want)
				}
			}

			// 1. Exaggerated fails → the terrain tile, marked and short-lived.
			w := doTile(p, q)
			assertTile(t, "fallback", w, terrainStub, "fallback", ccFallback)
			assertNoToken(t, "fallback", w, logs)
			exaggeratedHits(1)
			if u.terrain.hits() != 1 {
				t.Fatalf("terrain hits %d", u.terrain.hits())
			}
			assertUpstreamShape(t, "terrain fallback", u.terrain.queries[0], terrainLayer)
			assertTokensInHeader(t, u.terrain)
			if want := "[dhm] upstream exaggerated hillshade tile failed (" + tc.reason + "); served terrain fallback\n"; logs.String() != want {
				t.Fatalf("log %q, want %q", logs.String(), want)
			}

			// 2. Exaggerated still fails → retried, terrain from its own
			// cache entry.
			assertTile(t, "fallback again", doTile(p, q), terrainStub, "fallback-hit", ccFallback)
			exaggeratedHits(2)
			if u.terrain.hits() != 1 {
				t.Fatalf("terrain hits %d", u.terrain.hits())
			}

			// 3. Exaggerated recovers → its own tile, never the cached
			// terrain one, then a hit on the exaggerated entry.
			u.exaggerated.set(nil)
			assertTile(t, "recovered", doTile(p, q), exaggeratedStub, "miss", ccTile)
			assertTile(t, "recovered hit", doTile(p, q), exaggeratedStub, "hit", ccTile)
			exaggeratedHits(3)
			if u.terrain.hits() != 1 || len(u.stray) != 0 {
				t.Fatalf("terrain hits %d stray %v", u.terrain.hits(), u.stray)
			}
			if p.cache.Len() != 2 {
				t.Fatalf("cache entries %d, want one per layer", p.cache.Len())
			}
			assertTokensInHeader(t, u.exaggerated)
			assertNoToken(t, "after recovery", w, logs)
		})
	}
}

func TestDHM687_BothFailIsTransparent(t *testing.T) {
	u := newDHMUpstreams(t)
	logs := &dhmLogSink{}
	p := newLayerProxy(t, u, "", logs, nil)
	u.exaggerated.set(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, r.Header.Get("token"))
	})
	u.terrain.set(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, r.Header.Get("token"))
	})
	z, x, y := dkTile()
	w := doTile(p, leafletQuery(z, x, y))
	assertTile(t, "both down", w, dhmTransparentPNG, "unavailable", ccNoStore)
	assertNoToken(t, "both down", w, logs)
	if u.exaggerated.hits() != 1 || u.terrain.hits() != 1 {
		t.Fatalf("hits exaggerated=%d terrain=%d", u.exaggerated.hits(), u.terrain.hits())
	}
	if want := "[dhm] upstream hillshade tile failed (exaggerated status 404, terrain status 503)\n"; logs.String() != want {
		t.Fatalf("log %q, want %q", logs.String(), want)
	}
	if p.cache.Len() != 0 {
		t.Fatalf("a failure was cached")
	}
	// Nothing was cached: the next request tries both again.
	doTile(p, leafletQuery(z, x, y))
	if u.exaggerated.hits() != 2 || u.terrain.hits() != 2 {
		t.Fatalf("hits exaggerated=%d terrain=%d", u.exaggerated.hits(), u.terrain.hits())
	}
}

// Fallback log lines share the client's throttle: at most one per LogEvery.
func TestDHM687_FallbackLogIsThrottled(t *testing.T) {
	u := newDHMUpstreams(t)
	logs := &dhmLogSink{}
	now := time.Unix(1_800_000_000, 0)
	p := newLayerProxy(t, u, "", logs, func(o *dhmProxyOptions) {
		o.client.LogEvery = time.Minute
		o.client.Now = func() time.Time { return now }
	})
	u.exaggerated.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	z, x, y := dkTile()
	for i := 0; i < 5; i++ {
		assertTile(t, "fallback", doTile(p, leafletQuery(z, x+i, y)), terrainStub, "fallback", ccFallback)
	}
	if n := strings.Count(logs.String(), "\n"); n != 1 {
		t.Fatalf("%d log lines within LogEvery, want 1: %q", n, logs.String())
	}
	now = now.Add(time.Minute)
	doTile(p, leafletQuery(z, x+5, y))
	if n := strings.Count(logs.String(), "\n"); n != 2 {
		t.Fatalf("%d log lines after LogEvery, want 2: %q", n, logs.String())
	}
}

// hang is an upstream that never answers: it returns when the proxy gives
// up (the request context ends) or the test ends.
func hang(t *testing.T) func(w http.ResponseWriter, r *http.Request) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

// #688 review F1: a timeout is not a fallback trigger. Under the default
// config a hanging exaggerated upstream costs one Timeout per tile request,
// never a second one on the terrain service.
func TestDHM687_TimeoutDoesNotFallBack(t *testing.T) {
	const timeout = 300 * time.Millisecond
	u := newDHMUpstreams(t)
	u.exaggerated.set(hang(t))
	u.terrain.set(hang(t))
	logs := &dhmLogSink{}
	p := newLayerProxy(t, u, "", logs, func(o *dhmProxyOptions) { o.client.Timeout = timeout })
	z, x, y := dkTile()
	start := time.Now()
	w := doTile(p, leafletQuery(z, x, y))
	el := time.Since(start)
	assertTile(t, "timeout", w, dhmTransparentPNG, "unavailable", ccNoStore)
	if u.exaggerated.hits() != 1 || u.terrain.hits() != 0 {
		t.Fatalf("hits exaggerated=%d terrain=%d, want 1 and 0", u.exaggerated.hits(), u.terrain.hits())
	}
	if el < timeout || el > timeout*3/2 {
		t.Fatalf("tile request took %v, want about one Timeout (%v)", el, timeout)
	}
	if want := "[dhm] upstream exaggerated hillshade tile failed (timeout)\n"; logs.String() != want {
		t.Fatalf("log %q, want %q", logs.String(), want)
	}
	assertNoToken(t, "timeout", w, logs)
}

// #688 review F1: with the shared upstream budget saturated, a tile request
// under the default config waits at most one Timeout and is not retried on
// the terrain service.
func TestDHM687_SaturatedBudgetNoExtraWait(t *testing.T) {
	const timeout = 300 * time.Millisecond
	u := newDHMUpstreams(t)
	u.exaggerated.set(hang(t))
	u.terrain.set(hang(t))
	logs := &dhmLogSink{}
	p := newLayerProxy(t, u, "", logs, func(o *dhmProxyOptions) {
		o.client.Timeout = timeout
		o.client.Concurrency = 1
	})
	z, x, y := dkTile()
	// Tile A takes the only slot and hangs upstream.
	done := make(chan struct{})
	go func() { defer close(done); doTile(p, leafletQuery(z, x, y)) }()
	for deadline := time.Now().Add(2 * time.Second); u.exaggerated.hits() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("tile A never reached upstream")
		}
		runtime.Gosched()
	}
	// Tile B waits for the budget.
	start := time.Now()
	w := doTile(p, leafletQuery(z, x+1, y))
	el := time.Since(start)
	<-done
	assertTile(t, "tile B", w, dhmTransparentPNG, "unavailable", ccNoStore)
	if u.terrain.hits() != 0 {
		t.Fatalf("terrain attempts %d, want 0", u.terrain.hits())
	}
	if el > timeout*3/2 {
		t.Fatalf("tile B took %v with a saturated budget, want at most about one Timeout (%v)", el, timeout)
	}
	if strings.Contains(logs.String(), "fallback") {
		t.Fatalf("fallback logged: %q", logs.String())
	}
}

// #688 review F2: a terrain-only config makes exactly one upstream attempt
// per tile and never reports a fallback, also when terrain fails.
func TestDHM687_TerrainLayerOneAttempt(t *testing.T) {
	cases := []struct {
		name   string
		h      func(w http.ResponseWriter, r *http.Request)
		state  string
		reason string
	}{
		{"ok", nil, "miss", ""},
		{"500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, "unavailable", "status 500"},
		{"bad png", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "not a png")
		}, "unavailable", "non-image response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newDHMUpstreams(t)
			u.terrain.set(tc.h)
			logs := &dhmLogSink{}
			p := newLayerProxy(t, u, "terrain", logs, nil)
			z, x, y := dkTile()
			w := doTile(p, leafletQuery(z, x, y))
			if got := w.Header().Get("X-CoreScope-Tile"); got != tc.state {
				t.Fatalf("X-CoreScope-Tile %q, want %q", got, tc.state)
			}
			if u.terrain.hits() != 1 || u.exaggerated.hits() != 0 {
				t.Fatalf("hits terrain=%d exaggerated=%d, want 1 and 0", u.terrain.hits(), u.exaggerated.hits())
			}
			want := ""
			if tc.reason != "" {
				want = "[dhm] upstream hillshade tile failed (" + tc.reason + ")\n"
			}
			if logs.String() != want {
				t.Fatalf("log %q, want %q", logs.String(), want)
			}
		})
	}
}

// #688 review N1: the API docs name the default upstream and say what each
// X-CoreScope-Tile value means; fallback-hit still made an upstream call.
func TestDHM687_OpenAPIDescribesLayersAndTileStates(t *testing.T) {
	meta, ok := routeDescriptions()["GET /api/map/dhm/wms"]
	if !ok {
		t.Fatal("route not described")
	}
	if !strings.Contains(meta.Summary, "exaggerated by default") || strings.Contains(meta.Summary, "terrain hillshade tile") {
		t.Errorf("summary %q does not name the default layer", meta.Summary)
	}
	for _, want := range []string{
		exaggeratedLayer, "wms/dhm_temp", terrainLayer, "map.dhm.layer",
		"hit / miss", "fallback = ", "outside = ", "unavailable = ",
		"fallback-hit = the exaggerated attempt still went upstream and failed, and the terrain tile came from the cache",
		"not on a timeout or a full upstream budget",
	} {
		if !strings.Contains(meta.Description, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}
