package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #734 M1 — the tile proxy serves DHM overlays by id: terrain (the #687
// chain), surface and contours. Every test runs against fake upstreams
// (httptest); none reaches Dataforsyningen, and no real token is used.

const (
	surfaceLayer = "dhm_overflade_skyggekort"
	contourLayer = "dhm_kurve_traditionel"
)

var (
	surfaceStub = dhmtest.PNG(256, 256, 4)
	contourStub = dhmtest.PNG(256, 256, 5)
)

// overlayUpstreams is Dataforsyningen faked per upstream layer: dhm_DAF
// answers the terrain, surface and contour layers, wms/dhm_temp the
// exaggerated one. A request to any other path or layer is stray.
type overlayUpstreams struct {
	srv    *httptest.Server
	layers map[string]*dhmService
	mu     sync.Mutex
	stray  []string
}

func newOverlayUpstreams(t *testing.T) *overlayUpstreams {
	t.Helper()
	u := &overlayUpstreams{layers: map[string]*dhmService{
		terrainLayer:     {stub: terrainStub},
		exaggeratedLayer: {stub: exaggeratedStub},
		surfaceLayer:     {stub: surfaceStub},
		contourLayer:     {stub: contourStub},
	}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layer := r.URL.Query().Get("layers")
		wantPath := terrainPath
		if layer == exaggeratedLayer {
			wantPath = exaggeratedPath
		}
		if s := u.layers[layer]; s != nil && r.URL.Path == wantPath {
			s.serve(w, r)
			return
		}
		u.mu.Lock()
		u.stray = append(u.stray, r.URL.Path+"?"+r.URL.RawQuery)
		u.mu.Unlock()
		w.WriteHeader(404)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *overlayUpstreams) total() int {
	n := 0
	for _, s := range u.layers {
		n += s.hits()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return n + len(u.stray)
}

func (u *overlayUpstreams) assertNoStray(t *testing.T) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.stray) != 0 {
		t.Fatalf("stray upstream requests: %v", u.stray)
	}
}

func newOverlayProxy(t *testing.T, u *overlayUpstreams, layer string, logs *dhmLogSink, tweak func(*dhmProxyOptions)) *dhmProxy {
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

// overlayTile is the browser's query for one Aarhus tile of overlay id
// (the `layers` value) at zoom z.
func overlayTile(id string, z, dx int) url.Values {
	x, y := lonLatTile(z, 10.2039, 56.1629)
	q := leafletQuery(z, x+dx, y)
	q.Set("layers", id)
	return q
}

func overlayRequest(q url.Values, remote string) *http.Request {
	return tileRequest(context.Background(), q, remote)
}

// ── Each id, its own upstream layer ─────────────────────────────────────────

func TestDHM734_EachIdServesItsOwnLayer(t *testing.T) {
	for _, cfg := range []struct{ layer, terrainUp string }{
		{"terrain", terrainLayer},
		{"", exaggeratedLayer}, // the shipped default (#687)
	} {
		t.Run("map.dhm.layer="+cfg.layer, func(t *testing.T) {
			u := newOverlayUpstreams(t)
			logs := &dhmLogSink{}
			p := newOverlayProxy(t, u, cfg.layer, logs, nil)
			for _, c := range []struct {
				id, upLayer string
				z           int
				stub        []byte
			}{
				{"terrain", cfg.terrainUp, 10, u.layers[cfg.terrainUp].stub},
				{"surface", surfaceLayer, 10, surfaceStub},
				{"contours", contourLayer, 14, contourStub},
				{"contours", contourLayer, 19, contourStub},
			} {
				s := u.layers[c.upLayer]
				before := s.hits()
				w := doTile(p, overlayTile(c.id, c.z, 0))
				assertTile(t, c.id, w, c.stub, "miss", ccTile)
				if s.hits() != before+1 {
					t.Fatalf("%s z%d: %s hits %d, want %d", c.id, c.z, c.upLayer, s.hits(), before+1)
				}
				assertUpstreamShape(t, c.id, s.queries[len(s.queries)-1], c.upLayer)
				assertNoToken(t, c.id, w, logs)
			}
			for _, s := range u.layers {
				assertTokensInHeader(t, s)
			}
			u.assertNoStray(t)
			if logs.String() != "" {
				t.Fatalf("unexpected log: %q", logs.String())
			}
		})
	}
}

// ── The allow-list ──────────────────────────────────────────────────────────

func TestDHM734_RejectsUnknownIdsRawNamesAndOffRangeZooms(t *testing.T) {
	u := newOverlayUpstreams(t)
	logs := &dhmLogSink{}
	p := newOverlayProxy(t, u, "", logs, nil)
	set := func(v ...string) func(url.Values) { return func(q url.Values) { q["layers"] = v } }
	cases := []struct {
		name string
		q    url.Values
	}{
		{"unknown id", withLayers(overlayTile("terrain", 15, 0), set("elevation"))},
		{"exaggerated is no id", withLayers(overlayTile("terrain", 15, 0), set("exaggerated"))},
		{"surface layer name", withLayers(overlayTile("terrain", 15, 0), set(surfaceLayer))},
		{"contour layer name", withLayers(overlayTile("terrain", 15, 0), set(contourLayer))},
		{"quick DHM surface", withLayers(overlayTile("terrain", 15, 0), set("quickDHM_skyggekort_overflade"))},
		{"exaggerated layer name", withLayers(overlayTile("terrain", 15, 0), set(exaggeratedLayer))},
		{"case variant", withLayers(overlayTile("terrain", 15, 0), set("Surface"))},
		{"comma list", withLayers(overlayTile("terrain", 15, 0), set("terrain,surface"))},
		{"repeated layers", withLayers(overlayTile("terrain", 15, 0), set("surface", "surface"))},
		{"empty layers", withLayers(overlayTile("terrain", 15, 0), set(""))},
		{"surface with a style", withLayers(overlayTile("surface", 15, 0), func(q url.Values) { q.Set("styles", "gul") })},
		{"contours below z14", overlayTile("contours", 13, 0)},
		{"contours at z7", overlayTile("contours", 7, 0)},
		{"surface below z7", overlayTile("surface", 6, 0)},
		{"client token", withLayers(overlayTile("surface", 15, 0), func(q url.Values) { q.Set("token", "attacker") })},
	}
	for _, tc := range cases {
		w := doTile(p, tc.q)
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) == "" {
			t.Fatalf("%s: status %d (body %s), want 400", tc.name, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "attacker") || strings.Contains(w.Body.String(), "elevation") {
			t.Fatalf("%s: error echoes client input: %s", tc.name, w.Body.String())
		}
		assertNoToken(t, tc.name, w, logs)
	}
	if n := u.total(); n != 0 {
		t.Fatalf("rejected requests reached upstream %d times", n)
	}
	if p.cache.Len() != 0 {
		t.Fatalf("rejected requests left %d cache entries", p.cache.Len())
	}
}

func withLayers(q url.Values, f func(url.Values)) url.Values {
	f(q)
	return q
}

// ── Per-overlay cache keys ─────────────────────────────────────────────────

// The same z/x/y of two overlays are two upstream fetches and two cache
// entries, and neither is ever served for the other.
func TestDHM734_PerOverlayCacheKeys(t *testing.T) {
	for _, cfg := range []struct {
		layer     string
		terrainUp string
		product   dhmProduct
	}{
		{"terrain", terrainLayer, dhmProductTerrain},
		{"", exaggeratedLayer, dhmProductExaggerated},
	} {
		t.Run("map.dhm.layer="+cfg.layer, func(t *testing.T) {
			u := newOverlayUpstreams(t)
			p := newOverlayProxy(t, u, cfg.layer, &dhmLogSink{}, nil)
			const z = 15
			terrainBody := u.layers[cfg.terrainUp].stub
			assertTile(t, "terrain miss", doTile(p, overlayTile("terrain", z, 0)), terrainBody, "miss", ccTile)
			assertTile(t, "surface miss", doTile(p, overlayTile("surface", z, 0)), surfaceStub, "miss", ccTile)
			assertTile(t, "contours miss", doTile(p, overlayTile("contours", z, 0)), contourStub, "miss", ccTile)
			for i := 0; i < 3; i++ {
				assertTile(t, "surface hit", doTile(p, overlayTile("surface", z, 0)), surfaceStub, "hit", ccTile)
				assertTile(t, "terrain hit", doTile(p, overlayTile("terrain", z, 0)), terrainBody, "hit", ccTile)
				assertTile(t, "contours hit", doTile(p, overlayTile("contours", z, 0)), contourStub, "hit", ccTile)
			}
			if u.layers[cfg.terrainUp].hits() != 1 || u.layers[surfaceLayer].hits() != 1 || u.layers[contourLayer].hits() != 1 {
				t.Fatalf("upstream hits terrain %d surface %d contours %d, want 1 each",
					u.layers[cfg.terrainUp].hits(), u.layers[surfaceLayer].hits(), u.layers[contourLayer].hits())
			}
			u.assertNoStray(t)
			// Three entries, one per product, each holding its own bytes.
			x, y := lonLatTile(z, 10.2039, 56.1629)
			tile := dhm.TileKey{Z: z, X: x, Y: y}
			for _, e := range []struct {
				product dhmProduct
				body    []byte
			}{{cfg.product, terrainBody}, {dhmProductSurface, surfaceStub}, {dhmProductContours, contourStub}} {
				got, ok := p.cache.Get(dhmCacheKey{Product: e.product, Tile: tile}, p.now())
				if !ok || !bytes.Equal(got, e.body) {
					t.Fatalf("cache entry for product %d: %v, %d bytes", e.product, ok, len(got))
				}
			}
			if p.cache.Len() != 3 {
				t.Fatalf("cache holds %d entries, want 3", p.cache.Len())
			}
		})
	}
}

// Every way internal/dhm's table fetches an overlay — each id under both
// map.dhm.layer values, and each fallback — is a product of its own, so
// no two upstream layers ever share a cache entry. A table entry without
// a product panics here rather than in a request.
func TestDHM734_EveryOverlayHasItsOwnProduct(t *testing.T) {
	seen := map[dhmProduct]string{dhmProductOrtho: "ortho"}
	for _, id := range dhm.OverlayIDs() {
		o, _ := dhm.LookupOverlay(id)
		for _, h := range []dhm.Hillshade{dhm.HillshadeTerrain, dhm.HillshadeExaggerated} {
			chain := []dhm.Overlay{o.Resolve(h)}
			if fb, ok := chain[0].Fallback(); ok {
				chain = append(chain, fb)
			}
			for _, up := range chain {
				where := up.Service + "/" + up.Layer
				pr := overlayProduct(up)
				if prev, dup := seen[pr]; dup && prev != where {
					t.Fatalf("product %d is both %s and %s", pr, prev, where)
				}
				seen[pr] = where
			}
		}
	}
	if len(seen) != 5 {
		t.Fatalf("products %v, want terrain, exaggerated, ortho, surface and contours", seen)
	}
	// The #687/#728 tests' helper agrees with the table.
	terrain, _ := dhm.LookupOverlay("terrain")
	for _, h := range []dhm.Hillshade{dhm.HillshadeTerrain, dhm.HillshadeExaggerated} {
		if hillshadeProduct(h) != overlayProduct(terrain.Resolve(h)) {
			t.Fatalf("hillshadeProduct(%v) disagrees with the overlay table", h)
		}
	}
}

// #734 Q7: the #680 layer name is a fixed alias of terrain for one
// release: the same bytes and the same cache entry, never forwarded.
func TestDHM734_LegacyAliasIsTerrain(t *testing.T) {
	for _, cfg := range []struct{ layer, terrainUp string }{{"terrain", terrainLayer}, {"", exaggeratedLayer}} {
		t.Run("map.dhm.layer="+cfg.layer, func(t *testing.T) {
			u := newOverlayUpstreams(t)
			p := newOverlayProxy(t, u, cfg.layer, &dhmLogSink{}, nil)
			body := u.layers[cfg.terrainUp].stub
			assertTile(t, "alias miss", doTile(p, overlayTile(terrainLayer, 12, 0)), body, "miss", ccTile)
			assertTile(t, "id after alias", doTile(p, overlayTile("terrain", 12, 0)), body, "hit", ccTile)
			assertTile(t, "id miss", doTile(p, overlayTile("terrain", 12, 1)), body, "miss", ccTile)
			assertTile(t, "alias after id", doTile(p, overlayTile(terrainLayer, 12, 1)), body, "hit", ccTile)
			if n := u.layers[cfg.terrainUp].hits(); n != 2 || u.total() != 2 {
				t.Fatalf("upstream hits %d of %d, want 2 of 2", n, u.total())
			}
			if p.cache.Len() != 2 {
				t.Fatalf("cache holds %d entries, want 2", p.cache.Len())
			}
			// The alias is the browser's word only: upstream is asked for
			// its layer by the rebuilt query.
			for _, q := range u.layers[cfg.terrainUp].queries {
				assertUpstreamShape(t, "alias", q, cfg.terrainUp)
			}
		})
	}
}

// ── One shared budget ──────────────────────────────────────────────────────

// Terrain, surface and contour tiles from one address drain one budget;
// cached and outside-coverage tiles are not charged.
func TestDHM734_SharedRateLimit(t *testing.T) {
	const client = "198.51.100.34:1000"
	u := newOverlayUpstreams(t)
	p := newOverlayProxy(t, u, "terrain", &dhmLogSink{}, func(o *dhmProxyOptions) { o.rateLimitPerMin = 2 })
	do := func(q url.Values, remote string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, overlayRequest(q, remote))
		return w
	}
	assertTile(t, "terrain miss", do(overlayTile("terrain", 15, 0), client), terrainStub, "miss", ccTile)
	assertTile(t, "surface miss", do(overlayTile("surface", 15, 0), client), surfaceStub, "miss", ccTile)
	// Cached tiles are free, under any id that names them.
	for i := 0; i < 5; i++ {
		assertTile(t, "terrain hit", do(overlayTile("terrain", 15, 0), client), terrainStub, "hit", ccTile)
		assertTile(t, "alias hit", do(overlayTile(terrainLayer, 15, 0), client), terrainStub, "hit", ccTile)
		assertTile(t, "surface hit", do(overlayTile("surface", 15, 0), client), surfaceStub, "hit", ccTile)
	}
	// Outside coverage is free for every overlay.
	bx, by := lonLatTile(15, 13.40, 52.52) // Berlin
	for _, id := range []string{"terrain", "surface", "contours"} {
		q := leafletQuery(15, bx, by)
		q.Set("layers", id)
		assertTile(t, id+" outside", do(q, client), dhmTransparentPNG, "outside", ccTile)
	}
	// The budget of 2 is spent: the next miss of any overlay is 429.
	for _, q := range []url.Values{overlayTile("surface", 15, 1), overlayTile("terrain", 15, 1), overlayTile("contours", 15, 0), overlayTile(terrainLayer, 15, 2)} {
		w := do(q, client)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
			t.Fatalf("layers=%s: status %d, Retry-After %q; want 429", q.Get("layers"), w.Code, w.Header().Get("Retry-After"))
		}
	}
	if n := u.total(); n != 2 {
		t.Fatalf("upstream hits %d, want 2", n)
	}
	// Another address has its own budget.
	assertTile(t, "other client", do(overlayTile("contours", 15, 0), "198.51.100.35:1000"), contourStub, "miss", ccTile)
}

// ── Admission ──────────────────────────────────────────────────────────────

// With the admission bound full, a cold surface tile is refused at once
// (busy, no-store), without an upstream call, and its charge is given back.
func TestDHM734_AdmissionFullSurfaceIsBusyAndRefunded(t *testing.T) {
	const client = "198.51.100.36:1000"
	up, release, _ := heldUpstream(t)
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, func(o *dhmProxyOptions) {
		o.tileMaxInFlight = 1
		o.rateLimitPerMin = 2
		o.client.Timeout = 5 * time.Second
	})
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, overlayRequest(overlayTile("terrain", 16, 0), client))
		held <- w
	}()
	eventually744(t, "the terrain tile upstream", func() bool { return up.hits.Load() == 1 })
	for i := 1; i <= 5; i++ {
		for _, id := range []string{"surface", "contours"} {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, overlayRequest(overlayTile(id, 16, i), client))
			if w.Code == http.StatusTooManyRequests {
				close(release)
				t.Fatalf("%s refusal %d was charged: 429 while the client had used 1 of 2", id, i)
			}
			assertBusyTile(t, w)
		}
	}
	if n := up.hits.Load(); n != 1 {
		close(release)
		t.Fatalf("upstream hits %d, want only the held terrain tile", n)
	}
	close(release)
	assertMissTile(t, <-held)
	eventually744(t, "the slot back", func() bool { return p.admit.InUse() == 0 })
	// One unit is left of the budget: one surface miss, then 429.
	w := httptest.NewRecorder()
	p.ServeHTTP(w, overlayRequest(overlayTile("surface", 16, 1), client))
	assertMissTile(t, w)
	w = httptest.NewRecorder()
	p.ServeHTTP(w, overlayRequest(overlayTile("surface", 16, 2), client))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("after the refunds: status %d, want 429", w.Code)
	}
	l := logs.String()
	if strings.Count(l, "[dhm] surface hillshade tile refused: admission full, no upstream request\n") != 5 ||
		strings.Count(l, "[dhm] contour tile refused: admission full, no upstream request\n") != 5 {
		t.Fatalf("refusals not logged per overlay: %q", l)
	}
}

// ── Fallback ───────────────────────────────────────────────────────────────

// Surface and contours have no fallback: a failure is a transparent
// `unavailable` tile, never a terrain tile, and is never cached. Terrain
// keeps the exaggerated → terrain fallback (#687) on the same proxy.
func TestDHM734_NoFallbackForSurfaceOrContours(t *testing.T) {
	failures := []struct {
		name, reason string
		h            func(w http.ResponseWriter, r *http.Request)
	}{
		{"500", "status 500", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			io.WriteString(w, "boom "+r.Header.Get("token"))
		}},
		{"xml", "unexpected content type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.ogc.se_xml")
			io.WriteString(w, "<ServiceExceptionReport>"+r.Header.Get("token"))
		}},
		{"not a png", "non-image response", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "not a png "+r.Header.Get("token"))
		}},
		{"truncated png", "invalid image", servePNGBody(surfaceStub[:len(surfaceStub)/2])},
	}
	for _, cfg := range []string{"", "terrain"} {
		for _, f := range failures {
			for _, o := range []struct{ id, upLayer, what string }{
				{"surface", surfaceLayer, "surface hillshade tile"},
				{"contours", contourLayer, "contour tile"},
			} {
				t.Run("layer="+cfg+"/"+o.id+"/"+f.name, func(t *testing.T) {
					u := newOverlayUpstreams(t)
					logs := &dhmLogSink{}
					p := newOverlayProxy(t, u, cfg, logs, nil)
					u.layers[o.upLayer].set(f.h)
					for i := 0; i < 2; i++ {
						w := doTile(p, overlayTile(o.id, 15, 0))
						assertTile(t, o.id, w, dhmTransparentPNG, "unavailable", ccNoStore)
						assertNoToken(t, o.id, w, logs)
					}
					// Never cached: the second request went upstream again,
					// and nothing else was asked.
					if n := u.layers[o.upLayer].hits(); n != 2 || u.total() != 2 {
						t.Fatalf("%s hits %d of %d, want 2 of 2", o.upLayer, n, u.total())
					}
					if want := strings.Repeat("[dhm] upstream "+o.what+" failed ("+f.reason+")\n", 2); logs.String() != want {
						t.Fatalf("log %q, want %q", logs.String(), want)
					}
				})
			}
		}
	}
	// Side by side on one default proxy: terrain still falls back.
	u := newOverlayUpstreams(t)
	logs := &dhmLogSink{}
	p := newOverlayProxy(t, u, "", logs, nil)
	u.layers[exaggeratedLayer].set(failures[0].h)
	u.layers[surfaceLayer].set(failures[0].h)
	assertTile(t, "terrain", doTile(p, overlayTile("terrain", 15, 0)), terrainStub, "fallback", ccFallback)
	assertTile(t, "surface", doTile(p, overlayTile("surface", 15, 0)), dhmTransparentPNG, "unavailable", ccNoStore)
	if u.layers[terrainLayer].hits() != 1 {
		t.Fatalf("terrain hits %d: surface fell back to terrain, or terrain did not", u.layers[terrainLayer].hits())
	}
	u.assertNoStray(t)
	if want := "[dhm] upstream exaggerated hillshade tile failed (status 500); served terrain fallback\n" +
		"[dhm] upstream surface hillshade tile failed (status 500)\n"; logs.String() != want {
		t.Fatalf("log %q, want %q", logs.String(), want)
	}
}

// A timeout on a surface tile is unavailable too (and there is nothing to
// fall back to), and the token reaches no response or log line.
func TestDHM734_SurfaceTimeoutIsUnavailable(t *testing.T) {
	u := newOverlayUpstreams(t)
	logs := &dhmLogSink{}
	p := newOverlayProxy(t, u, "", logs, func(o *dhmProxyOptions) { o.client.Timeout = 100 * time.Millisecond })
	u.layers[surfaceLayer].set(hang(t))
	w := doTile(p, overlayTile("surface", 15, 0))
	assertTile(t, "surface", w, dhmTransparentPNG, "unavailable", ccNoStore)
	assertNoToken(t, "surface timeout", w, logs)
	if want := "[dhm] upstream surface hillshade tile failed (timeout)\n"; logs.String() != want {
		t.Fatalf("log %q, want %q", logs.String(), want)
	}
	if u.layers[terrainLayer].hits() != 0 || u.layers[exaggeratedLayer].hits() != 0 {
		t.Fatal("a surface timeout asked a terrain layer")
	}
}

// ── OpenAPI ────────────────────────────────────────────────────────────────

// layers is documented as the enum of overlay ids plus the legacy alias,
// in the built spec a generated client reads, and the description names
// each id's upstream and zoom range.
func TestDHM734_OpenAPIDocumentsLayersAsOverlayIDs(t *testing.T) {
	router := mux.NewRouter()
	srv, _ := setupNoStoreServer(t)
	srv.RegisterRoutes(router)
	raw, err := json.Marshal(buildOpenAPISpec(router, "test"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Description string `json:"description"`
			Parameters  []struct {
				Name   string `json:"name"`
				Schema struct {
					Type string   `json:"type"`
					Enum []string `json:"enum"`
				} `json:"schema"`
				Description string `json:"description"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	op := spec.Paths["/api/map/dhm/wms"]["get"]
	found := false
	for _, p := range op.Parameters {
		if p.Name != "layers" {
			if len(p.Schema.Enum) != 0 {
				t.Errorf("param %s carries an enum %v", p.Name, p.Schema.Enum)
			}
			continue
		}
		found = true
		want := []string{"terrain", "surface", "contours", terrainLayer}
		if !reflect.DeepEqual(p.Schema.Enum, want) || p.Schema.Type != "string" {
			t.Errorf("layers schema %+v, want string enum %v", p.Schema, want)
		}
		for _, s := range []string{"overlay id", "alias", "terrain", "one release"} {
			if !strings.Contains(p.Description, s) {
				t.Errorf("layers description %q lacks %q", p.Description, s)
			}
		}
	}
	if !found {
		t.Fatal("layers not declared")
	}
	for _, s := range []string{
		"layers=terrain, surface or contours", surfaceLayer + " from dhm_DAF, zoom 7-19",
		contourLayer + " from dhm_DAF, zoom 14-19", "no fallback", "alias",
	} {
		if !strings.Contains(op.Description, s) {
			t.Errorf("route description lacks %q", s)
		}
	}
	// Nothing in the spec still claims layers is the one fixed value.
	if strings.Contains(op.Description, "layers="+terrainLayer+", styles=") {
		t.Errorf("route description still pins layers=%s", terrainLayer)
	}
}

// ── /api/config/client ─────────────────────────────────────────────────────

// The dhm block lists the overlays the page may offer, terrain and surface,
// each with its zoom range and its own dataset link (#734 Q5, in the #733
// credit's parts). contours is served but not offered until M2. Nothing
// else in the block changes.
func TestDHM734_ClientConfigOffersTerrainAndSurface(t *testing.T) {
	u := newOverlayUpstreams(t)
	srv, router := setupTestServer(t)
	srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
	srv.dhm = newOverlayProxy(t, u, "", &dhmLogSink{}, nil)

	raw := clientDHMRaw(t, router)
	if raw == nil {
		t.Fatal("no dhm block while the proxy is active")
	}
	var b struct {
		Overlays    []map[string]json.RawMessage `json:"overlays"`
		Layer       string                       `json:"layer"`
		Attribution string                       `json:"attribution"`
		DatasetName string                       `json:"datasetName"`
		DatasetURL  string                       `json:"datasetUrl"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	want := []DHMOverlayClientConfig{
		{ID: "terrain", MinZoom: 7, MaxZoom: 19, DatasetName: "Danmarks Højdemodel", DatasetURL: "https://dataforsyningen.dk/data/930"},
		{ID: "surface", MinZoom: 7, MaxZoom: 19, DatasetName: "Danmarks Højdemodel", DatasetURL: "https://dataforsyningen.dk/data/928"},
	}
	if len(b.Overlays) != len(want) {
		t.Fatalf("overlays %s, want %d entries", raw, len(want))
	}
	for i, entry := range b.Overlays {
		keys := make([]string, 0, len(entry))
		for k := range entry {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if want := []string{"datasetName", "datasetUrl", "id", "maxZoom", "minZoom"}; !reflect.DeepEqual(keys, want) {
			t.Fatalf("overlay %d keys %v, want %v", i, keys, want)
		}
		buf, _ := json.Marshal(entry)
		var got DHMOverlayClientConfig
		if err := json.Unmarshal(buf, &got); err != nil {
			t.Fatal(err)
		}
		if got != want[i] {
			t.Errorf("overlay %d = %+v, want %+v", i, got, want[i])
		}
		// The dataset mark is in the unchanged credit text, so the page
		// can link it (#733).
		if !strings.Contains(b.Attribution, got.DatasetName) {
			t.Errorf("attribution %q lacks the mark %q", b.Attribution, got.DatasetName)
		}
		// Every offered id is served at both ends of its range and
		// refused just outside it.
		p := srv.dhm
		if w := doTile(p, overlayTile(got.ID, got.MinZoom, 0)); w.Code != 200 {
			t.Errorf("%s at its minZoom %d: status %d", got.ID, got.MinZoom, w.Code)
		}
		if w := doTile(p, overlayTile(got.ID, got.MaxZoom, 0)); w.Code != 200 {
			t.Errorf("%s at its maxZoom %d: status %d", got.ID, got.MaxZoom, w.Code)
		}
		if w := doTile(p, overlayTile(got.ID, got.MinZoom-1, 0)); w.Code != http.StatusBadRequest {
			t.Errorf("%s below its minZoom: status %d", got.ID, w.Code)
		}
	}
	// The rest of the block is #680/#733's, unchanged.
	if b.Layer != terrainLayer || b.Attribution != dhm.Attribution || b.DatasetName != dhm.DatasetName || b.DatasetURL != dhm.DatasetURL {
		t.Fatalf("block changed: layer %q attribution %q dataset %q %q", b.Layer, b.Attribution, b.DatasetName, b.DatasetURL)
	}
	// Not offered: contours, and any upstream name beyond the #680 layer.
	for _, s := range []string{"contours", contourLayer, surfaceLayer, exaggeratedLayer, "dhm_temp", "/data/926", dhmTestToken} {
		if strings.Contains(string(raw), s) {
			t.Errorf("dhm block carries %q: %s", s, raw)
		}
	}
}

// The offered ids are a subset of the table's, in its order, so the page
// never offers an id the proxy refuses.
func TestDHM734_OfferedOverlaysAreTableIDs(t *testing.T) {
	table := dhm.OverlayIDs()
	next := 0
	for _, c := range dhmOverlayClientConfigs() {
		for next < len(table) && table[next] != c.ID {
			next++
		}
		if next == len(table) {
			t.Fatalf("offered overlay %q is not in the table %v (or out of order)", c.ID, table)
		}
	}
}
