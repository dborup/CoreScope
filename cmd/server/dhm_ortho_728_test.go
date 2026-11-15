package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #728 M2 — the Danish orthophoto through the DHM proxy:
// GET /api/map/dhm/ortho/{z}/{x}/{y}. It is the same dhmProxy on the
// same token, behind a second route, with the same per-client budget,
// #744 admission bound and coverage gate, and its own cache instance.
// Every test runs against a fake upstream (httptest); none reaches
// Dataforsyningen.

// orthoJPEG is a real 256×256 JPEG standing in for an upstream ortho tile.
var orthoJPEG = dhmtest.JPEG(256, 256, 7)

const orthoUpstreamPath = "/orto_foraar_webm_DAF"

// orthoAwareUpstream answers the ortho service with a JPEG and every other
// service with the hillshade PNG, like the real services do.
func orthoAwareUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	return newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == orthoUpstreamPath {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(orthoJPEG)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngStub)
	})
}

func orthoTestConfig() DHMConfig {
	return DHMConfig{Enabled: true, Token: dhmTestToken, Layer: "terrain", Ortho: dhm.OrthoConfig{Enabled: true}}
}

func newTestOrthoProxy(t *testing.T, up *fakeUpstream, logs *dhmLogSink, tweak func(*dhmProxyOptions)) *dhmProxy {
	t.Helper()
	o := testDHMOptions(up.srv.URL+"/", logs)
	if tweak != nil {
		tweak(&o)
	}
	p := newDHMProxy(orthoTestConfig(), o)
	if p == nil {
		t.Fatal("newDHMProxy returned nil for an enabled config with a token")
	}
	return p
}

// orthoRouter routes the ortho path the way RegisterRoutes does, so the
// proxy sees mux's path variables.
func orthoRouter(p *dhmProxy) http.Handler {
	r := mux.NewRouter()
	r.HandleFunc(dhmOrthoRoute, p.serveOrtho)
	r.HandleFunc(dhmClientPath, p.ServeHTTP)
	return r
}

func orthoPath(z, x, y int) string { return fmt.Sprintf("/api/map/dhm/ortho/%d/%d/%d", z, x, y) }

func orthoRequest(ctx context.Context, target, remote string) *http.Request {
	r := httptest.NewRequest("GET", target, nil).WithContext(ctx)
	if remote != "" {
		r.RemoteAddr = remote
	}
	return r
}

func doOrtho(h http.Handler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, orthoRequest(context.Background(), target, ""))
	return w
}

// aarhusOrtho is tile i of a row of distinct, in-coverage z16 tiles.
func aarhusOrtho(i int) string {
	const z = 16
	x, y := lonLatTile(z, 10.20, 56.16)
	return orthoPath(z, x+i, y)
}

func assertOrthoTile(t *testing.T, where string, w *httptest.ResponseRecorder, state string) {
	t.Helper()
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), orthoJPEG) {
		t.Fatalf("%s: status %d, %d bytes; want 200 with the upstream JPEG", where, w.Code, w.Body.Len())
	}
	h := w.Header()
	if h.Get("Content-Type") != "image/jpeg" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("X-CoreScope-Tile") != state || h.Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("%s: headers %v; want image/jpeg, nosniff, %s, max-age=86400", where, h, state)
	}
}

func assertTransparentOrtho(t *testing.T, where string, w *httptest.ResponseRecorder, state, cacheControl string) {
	t.Helper()
	h := w.Header()
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), dhmTransparentPNG) ||
		h.Get("Content-Type") != "image/png" || h.Get("X-CoreScope-Tile") != state || h.Get("Cache-Control") != cacheControl {
		t.Fatalf("%s: status %d, %d bytes, headers %v; want transparent PNG, %s, %s",
			where, w.Code, w.Body.Len(), h, state, cacheControl)
	}
}

// ── Valid request shape ────────────────────────────────────────────────────

// A tile in Denmark is fetched once from the ortho service with the exact
// GetTile query, the token in the header, and then served from the cache.
func TestDHMOrtho728_ValidTileForwardsFixedShape(t *testing.T) {
	up := orthoAwareUpstream(t)
	logs := &dhmLogSink{}
	h := orthoRouter(newTestOrthoProxy(t, up, logs, nil))
	z, x, y := dkTile()

	w := doOrtho(h, orthoPath(z, x, y))
	assertOrthoTile(t, "first", w, "miss")
	assertNoToken(t, "first", w, logs)
	if up.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1", up.hits.Load())
	}
	want := url.Values{
		"service": {"WMTS"}, "request": {"GetTile"}, "version": {"1.0.0"},
		"layer": {"orto_foraar_webm"}, "style": {"default"},
		"tilematrixset": {"DFD_GoogleMapsCompatible"}, "format": {"image/jpeg"},
		"tilematrix": {strconv.Itoa(z)}, "tilerow": {strconv.Itoa(y)}, "tilecol": {strconv.Itoa(x)},
	}
	up.mu.Lock()
	got, token := up.queries[0], up.tokens[0]
	up.mu.Unlock()
	if got.Encode() != want.Encode() {
		t.Fatalf("upstream query\n got %s\nwant %s", got.Encode(), want.Encode())
	}
	if token != dhmTestToken {
		t.Fatalf("token header %q", token)
	}

	w = doOrtho(h, orthoPath(z, x, y))
	assertOrthoTile(t, "second", w, "hit")
	if up.hits.Load() != 1 {
		t.Fatalf("cache hit went upstream: %d hits", up.hits.Load())
	}
}

// Everything but a bare canonical z/x/y path in range is 400, and nothing
// is fetched or charged.
func TestDHMOrtho728_RejectsNonAllowListedRequests(t *testing.T) {
	up := orthoAwareUpstream(t)
	logs := &dhmLogSink{}
	p := newTestOrthoProxy(t, up, logs, func(o *dhmProxyOptions) { o.rateLimitPerMin = 1 })
	h := orthoRouter(p)
	z, x, y := dkTile()
	base := orthoPath(z, x, y)
	for name, target := range map[string]string{
		"any query":         base + "?a=1",
		"token query":       base + "?token=" + dhmTestToken,
		"TOKEN query":       base + "?TOKEN=x",
		"empty query":       base + "?",
		"layer query":       base + "?layer=quickorto",
		"z below min":       orthoPath(6, 33, 19),
		"z above max":       orthoPath(21, x<<11, y<<11),
		"x off grid":        orthoPath(10, 1024, y),
		"y off grid":        orthoPath(10, x, 1024),
		"leading zero":      fmt.Sprintf("/api/map/dhm/ortho/%d/0%d/%d", z, x, y),
		"plus sign":         fmt.Sprintf("/api/map/dhm/ortho/%d/+%d/%d", z, x, y),
		"negative":          fmt.Sprintf("/api/map/dhm/ortho/%d/-1/%d", z, y),
		"extension":         base + ".jpg",
		"decimal":           fmt.Sprintf("/api/map/dhm/ortho/%d/%d.0/%d", z, x, y),
		"huge":              fmt.Sprintf("/api/map/dhm/ortho/%d/99999999999999999999/%d", z, y),
		"encoded plus sign": fmt.Sprintf("/api/map/dhm/ortho/%d/%%2B%d/%d", z, x, y),
	} {
		w := doOrtho(h, target)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s (%s): status %d, want 400", name, target, w.Code)
		}
		if strings.Contains(w.Body.String(), "quickorto") || strings.Contains(w.Body.String(), dhmTestToken) {
			t.Errorf("%s: client input echoed: %q", name, w.Body.String())
		}
		assertNoToken(t, name, w, logs)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(m, base, nil))
		if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", m, w.Code)
		}
	}
	if n := up.hits.Load(); n != 0 {
		t.Fatalf("%d upstream requests for refused requests", n)
	}
	// Nothing above was charged: the budget of 1 is still whole.
	assertOrthoTile(t, "after refusals", doOrtho(h, base), "miss")
}

// Outside DHM coverage the tile is transparent, browser-cacheable and
// costs neither an upstream call nor the client's budget.
func TestDHMOrtho728_OutsideCoverageIsTransparentWithoutUpstream(t *testing.T) {
	up := orthoAwareUpstream(t)
	h := orthoRouter(newTestOrthoProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.rateLimitPerMin = 1 }))
	for _, at := range [][2]float64{{13.40, 52.52}, {-0.12, 51.50}, {18.07, 59.33}} { // Berlin, London, Stockholm
		x, y := lonLatTile(12, at[0], at[1])
		assertTransparentOrtho(t, fmt.Sprint(at), doOrtho(h, orthoPath(12, x, y)), "outside", "public, max-age=86400")
	}
	if n := up.hits.Load(); n != 0 {
		t.Fatalf("%d upstream requests outside coverage", n)
	}
	assertOrthoTile(t, "inside after outside", doOrtho(h, aarhusOrtho(0)), "miss")
}

// ── Response checks: only a decoded JPEG is served or cached ──────────────

func TestDHMOrtho728_InvalidTileIsTransparentNoStoreAndNeverCached(t *testing.T) {
	sos := bytes.Index(orthoJPEG, []byte{0xff, 0xda})
	for name, body := range map[string][]byte{
		"not a JPEG":    []byte("<html>error</html>"),
		"PNG body":      pngStub,
		"truncated":     orthoJPEG[:len(orthoJPEG)/2],
		"headers only":  orthoJPEG[:sos],
		"no EOI":        orthoJPEG[:len(orthoJPEG)-2],
		"wrong size":    dhmtest.JPEG(512, 512, 7),
		"one pixel row": dhmtest.JPEG(256, 1, 7),
	} {
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/jpeg")
				w.Write(body)
			})
			logs := &dhmLogSink{}
			p := newTestOrthoProxy(t, up, logs, nil)
			h := orthoRouter(p)
			for i := 0; i < 2; i++ {
				assertTransparentOrtho(t, name, doOrtho(h, aarhusOrtho(0)), "unavailable", "no-store")
			}
			if n := up.hits.Load(); n != 2 {
				t.Fatalf("upstream hits %d, want 2: a bad tile was cached", n)
			}
			if n := p.orthoCache.Len(); n != 0 {
				t.Fatalf("ortho cache holds %d entries after bad tiles", n)
			}
			if !strings.Contains(logs.String(), "[dhm] upstream ortho tile failed (") {
				t.Fatalf("failure not logged by category: %q", logs.String())
			}
		})
	}
}

// Upstream errors are reported by category only; the token, which a hostile
// or broken upstream may echo, never reaches the response or the log, and
// no redirect is followed.
func TestDHMOrtho728_UpstreamErrorsDoNotLeak(t *testing.T) {
	elsewhere := newFakeUpstream(t, nil)
	for name, hf := range map[string]func(w http.ResponseWriter, r *http.Request){
		"403 echoing the token": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "User not authorized: %s %s", r.Header.Get("token"), r.URL)
		},
		"500 XML": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "<ExceptionReport>%s</ExceptionReport>", r.Header.Get("token"))
		},
		"JPEG as PNG": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(orthoJPEG)
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.srv.URL+"/steal", http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &dhmLogSink{}
			h := orthoRouter(newTestOrthoProxy(t, newFakeUpstream(t, hf), logs, nil))
			w := doOrtho(h, aarhusOrtho(0))
			assertTransparentOrtho(t, name, w, "unavailable", "no-store")
			assertNoToken(t, name, w, logs)
		})
	}
	if n := elsewhere.hits.Load(); n != 0 {
		t.Fatalf("redirect followed: %d requests", n)
	}
}

// ── Cache ──────────────────────────────────────────────────────────────────

// The plan's ortho cache bounds until staging measures real tiles
// (#728 M2): 2048 entries, 96 MB, 7 days, apart from the hillshade cache.
func TestDHMOrtho728_DefaultCacheBounds(t *testing.T) {
	o := defaultDHMProxyOptions()
	if o.orthoCacheMaxEntries != 2048 || o.orthoCacheMaxBytes != 96<<20 || o.orthoCacheTTL != 7*24*time.Hour {
		t.Fatalf("ortho cache %d entries, %d bytes, %v", o.orthoCacheMaxEntries, o.orthoCacheMaxBytes, o.orthoCacheTTL)
	}
	// The hillshade cache keeps its own bounds.
	if o.cacheMaxEntries != 2048 || o.cacheMaxBytes != 64<<20 || o.cacheTTL != 24*time.Hour {
		t.Fatalf("hillshade cache %d entries, %d bytes, %v", o.cacheMaxEntries, o.cacheMaxBytes, o.cacheTTL)
	}
}

// The same tile as hillshade and as ortho is two products: two upstream
// calls, each served its own body, each in its own cache, so ortho JPEGs
// never evict or replace hillshade tiles.
func TestDHMOrtho728_OrthoAndHillshadeCachesAreSeparate(t *testing.T) {
	up := orthoAwareUpstream(t)
	p := newTestOrthoProxy(t, up, &dhmLogSink{}, nil)
	h := orthoRouter(p)
	z, x, y := dkTile()
	hill := "/api/map/dhm/wms?" + leafletQuery(z, x, y).Encode()
	for i, state := range []string{"miss", "hit"} {
		if w := doOrtho(h, hill); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pngStub) || w.Header().Get("X-CoreScope-Tile") != state {
			t.Fatalf("hillshade %d: status %d, state %q", i, w.Code, w.Header().Get("X-CoreScope-Tile"))
		}
		assertOrthoTile(t, "ortho "+state, doOrtho(h, orthoPath(z, x, y)), state)
	}
	if n := up.hits.Load(); n != 2 {
		t.Fatalf("upstream hits %d, want one per product", n)
	}
	if p.cache.Len() != 1 || p.cache.Size() != len(pngStub) {
		t.Fatalf("hillshade cache %d entries / %d bytes, want only the PNG", p.cache.Len(), p.cache.Size())
	}
	if p.orthoCache.Len() != 1 || p.orthoCache.Size() != len(orthoJPEG) {
		t.Fatalf("ortho cache %d entries / %d bytes, want only the JPEG", p.orthoCache.Len(), p.orthoCache.Size())
	}
	// The keys differ too, so even one shared cache could not collide.
	tile := dhm.TileKey{Z: z, X: x, Y: y}
	if (dhmCacheKey{Product: dhmProductOrtho, Tile: tile}) == (dhmCacheKey{Product: hillshadeProduct(p.layer), Tile: tile}) {
		t.Fatal("ortho and hillshade cache keys collide")
	}
	if hillshadeProduct(dhm.HillshadeTerrain) == hillshadeProduct(dhm.HillshadeExaggerated) {
		t.Fatal("terrain and exaggerated cache keys collide (#687)")
	}
}

func TestDHMOrtho728_CacheEntryByteAndTTLBounds(t *testing.T) {
	t.Run("entries", func(t *testing.T) {
		up := orthoAwareUpstream(t)
		p := newTestOrthoProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.orthoCacheMaxEntries = 2 })
		h := orthoRouter(p)
		for i := 0; i < 5; i++ {
			assertOrthoTile(t, "fill", doOrtho(h, aarhusOrtho(i)), "miss")
		}
		if n := p.orthoCache.Len(); n != 2 {
			t.Fatalf("ortho cache %d entries, want 2", n)
		}
		assertOrthoTile(t, "newest", doOrtho(h, aarhusOrtho(4)), "hit")
		assertOrthoTile(t, "evicted", doOrtho(h, aarhusOrtho(0)), "miss")
	})
	t.Run("bytes", func(t *testing.T) {
		up := orthoAwareUpstream(t)
		p := newTestOrthoProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.orthoCacheMaxBytes = 2*len(orthoJPEG) + 1 })
		h := orthoRouter(p)
		for i := 0; i < 5; i++ {
			doOrtho(h, aarhusOrtho(i))
		}
		if p.orthoCache.Size() > 2*len(orthoJPEG)+1 || p.orthoCache.Len() != 2 {
			t.Fatalf("ortho cache %d bytes / %d entries over its byte bound", p.orthoCache.Size(), p.orthoCache.Len())
		}
	})
	t.Run("ttl", func(t *testing.T) {
		up := orthoAwareUpstream(t)
		var mu sync.Mutex
		now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		h := orthoRouter(newTestOrthoProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.client.Now = clock }))
		assertOrthoTile(t, "cold", doOrtho(h, aarhusOrtho(0)), "miss")
		mu.Lock()
		now = now.Add(7*24*time.Hour - time.Second)
		mu.Unlock()
		assertOrthoTile(t, "within 7 days", doOrtho(h, aarhusOrtho(0)), "hit")
		mu.Lock()
		now = now.Add(time.Second)
		mu.Unlock()
		assertOrthoTile(t, "after 7 days", doOrtho(h, aarhusOrtho(0)), "miss")
	})
}

// Identical concurrent cold requests share one upstream call.
func TestDHMOrtho728_CoalescesSameTile(t *testing.T) {
	up, release, _ := heldUpstream(t)
	up.handler = func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(orthoJPEG)
	}
	h := orthoRouter(newTestOrthoProxy(t, up, &dhmLogSink{}, nil))
	const n = 8
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = doOrtho(h, aarhusOrtho(0))
		}(i)
	}
	eventually744(t, "one upstream call", func() bool { return up.hits.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if hits := up.hits.Load(); hits != 1 {
		t.Fatalf("upstream hits %d, want 1", hits)
	}
	for i, w := range results {
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), orthoJPEG) {
			t.Fatalf("caller %d: status %d", i, w.Code)
		}
	}
}

// ── Shared budgets ─────────────────────────────────────────────────────────

// One client's ortho and hillshade misses draw on one per-client budget;
// cache hits and outside tiles are not charged.
func TestDHMOrtho728_PerClientBudgetIsSharedWithHillshade(t *testing.T) {
	const client = "198.51.100.28:1000"
	up := orthoAwareUpstream(t)
	h := orthoRouter(newTestOrthoProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.rateLimitPerMin = 2 }))
	do := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, orthoRequest(context.Background(), target, client))
		return w
	}
	z, x, y := dkTile()
	if w := do("/api/map/dhm/wms?" + leafletQuery(z, x, y).Encode()); w.Code != 200 {
		t.Fatalf("hillshade miss: %d", w.Code)
	}
	assertOrthoTile(t, "ortho miss", do(aarhusOrtho(0)), "miss")
	// Cached and outside tiles are free.
	for i := 0; i < 5; i++ {
		assertOrthoTile(t, "ortho hit", do(aarhusOrtho(0)), "hit")
	}
	bx, by := lonLatTile(12, 13.40, 52.52)
	assertTransparentOrtho(t, "outside", do(orthoPath(12, bx, by)), "outside", "public, max-age=86400")
	// The budget of 2 is spent: the next miss of either product is 429.
	for _, target := range []string{aarhusOrtho(1), "/api/map/dhm/wms?" + aarhusTile(1).Encode()} {
		w := do(target)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: status %d, Retry-After %q; want 429", target, w.Code, w.Header().Get("Retry-After"))
		}
	}
	if n := up.hits.Load(); n != 2 {
		t.Fatalf("upstream hits %d, want 2", n)
	}
	// Another address has its own budget.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, orthoRequest(context.Background(), aarhusOrtho(1), "198.51.100.29:1000"))
	assertOrthoTile(t, "other client", w, "miss")
}

// Cold ortho and hillshade tiles share the #744 admission bound: while
// the only slot is held by a hillshade tile, a cold ortho tile is refused
// at once (busy, no-store), without an upstream call or a charge.
func TestDHMOrtho728_AdmissionIsSharedWithHillshade(t *testing.T) {
	const client = "198.51.100.30:1000"
	up, release, _ := heldUpstream(t)
	logs := &dhmLogSink{}
	p := newTestOrthoProxy(t, up, logs, func(o *dhmProxyOptions) {
		o.tileMaxInFlight = 1
		o.rateLimitPerMin = 2
		o.client.Timeout = 5 * time.Second
	})
	h := orthoRouter(p)
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, orthoRequest(context.Background(), "/api/map/dhm/wms?"+aarhusTile(0).Encode(), client))
		held <- w
	}()
	eventually744(t, "the hillshade tile upstream", func() bool { return up.hits.Load() == 1 })
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, orthoRequest(context.Background(), aarhusOrtho(i), client))
		assertTransparentOrtho(t, "refused", w, "busy", "no-store")
	}
	close(release)
	assertMissTile(t, <-held)
	if n := up.hits.Load(); n != 1 {
		t.Fatalf("upstream hits %d, want only the held hillshade tile", n)
	}
	eventually744(t, "the slot back", func() bool { return p.admit.InUse() == 0 })
	// The refusals were not charged: the second of the 2 is still free.
	up.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(orthoJPEG)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, orthoRequest(context.Background(), aarhusOrtho(0), client))
	assertOrthoTile(t, "after refusals", w, "miss")
	if l := logs.String(); strings.Count(l, "[dhm] ortho tile refused: admission full, no upstream request\n") != 3 ||
		strings.Contains(l, "failed") {
		t.Fatalf("refusal log lines:\n%s", l)
	}
}

// A client that leaves stops waiting; nothing is written or logged for it.
func TestDHMOrtho728_ClientGoneWritesNothing(t *testing.T) {
	up, release, abandoned := heldUpstream(t)
	defer close(release)
	logs := &dhmLogSink{}
	p := newTestOrthoProxy(t, up, logs, func(o *dhmProxyOptions) { o.client.Timeout = 5 * time.Second })
	h := orthoRouter(p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, orthoRequest(ctx, aarhusOrtho(0), ""))
		done <- w
	}()
	eventually744(t, "the tile upstream", func() bool { return up.hits.Load() == 1 })
	cancel()
	w := <-done
	if w.Body.Len() != 0 || w.Header().Get("X-CoreScope-Tile") != "" {
		t.Fatalf("wrote %d bytes / state %q to a client that left", w.Body.Len(), w.Header().Get("X-CoreScope-Tile"))
	}
	eventually744(t, "the upstream request abandoned", func() bool { return abandoned.Load() == 1 })
	eventually744(t, "the slot back", func() bool { return p.admit.InUse() == 0 })
	if l := logs.String(); l != "" {
		t.Fatalf("logged for a client that left: %q", l)
	}
}

// ── Offering: off unless map.dhm is active and map.dhm.ortho.enabled ───────

func clientConfigBody(t *testing.T, router http.Handler) map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if strings.Contains(w.Body.String(), dhmTestToken) {
		t.Fatalf("token in /api/config/client: %s", w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDHMOrtho728_OffIs404AndNotOffered(t *testing.T) {
	up := orthoAwareUpstream(t)
	z, x, y := dkTile()
	for _, tc := range []struct {
		name      string
		cfg       DHMConfig
		hillshade bool // the dhm block (and the hillshade) stays on
	}{
		{"ortho not enabled", DHMConfig{Enabled: true, Token: dhmTestToken}, true},
		{"ortho enabled, dhm disabled", DHMConfig{Token: dhmTestToken, Ortho: dhm.OrthoConfig{Enabled: true}}, false},
		{"ortho enabled, no token", DHMConfig{Enabled: true, Ortho: dhm.OrthoConfig{Enabled: true}}, false},
		{"ortho enabled, blank token", DHMConfig{Enabled: true, Token: "  ", Ortho: dhm.OrthoConfig{Enabled: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.DHM = tc.cfg
			srv.dhm = newDHMProxy(tc.cfg, testDHMOptions(up.srv.URL+"/", &dhmLogSink{}))
			w := doOrtho(router, orthoPath(z, x, y))
			if w.Code != http.StatusNotFound {
				t.Fatalf("ortho endpoint status %d, want 404", w.Code)
			}
			assertNoToken(t, tc.name, w, nil)
			body := clientConfigBody(t, router)
			raw, ok := body["dhm"]
			if ok != tc.hillshade {
				t.Fatalf("dhm block present %v, want %v", ok, tc.hillshade)
			}
			if ok && strings.Contains(string(raw), `"ortho"`) {
				t.Fatalf("client config offers ortho while it is off: %s", raw)
			}
		})
	}
	if n := up.hits.Load(); n != 0 {
		t.Fatalf("%d upstream requests while ortho is off", n)
	}
}

// When on, /api/config/client carries exactly the ortho block the M3 base
// layer needs, and the advertised URL template is the routed endpoint.
func TestDHMOrtho728_OfferedAndRouted(t *testing.T) {
	up := orthoAwareUpstream(t)
	srv, router := setupTestServer(t)
	srv.cfg.DHM = orthoTestConfig()
	srv.dhm = newTestOrthoProxy(t, up, &dhmLogSink{}, nil)
	body := clientConfigBody(t, router)
	var block struct {
		Ortho map[string]json.RawMessage `json:"ortho"`
	}
	if err := json.Unmarshal(body["dhm"], &block); err != nil || block.Ortho == nil {
		t.Fatalf("no dhm.ortho block: %s (%v)", body["dhm"], err)
	}
	keys := make([]string, 0, len(block.Ortho))
	for k := range block.Ortho {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "attribution,bounds,datasetName,datasetUrl,licenseName,licenseUrl,maxZoom,minZoom,url" {
		t.Fatalf("dhm.ortho keys %s", got)
	}
	var o DHMOrthoClientConfig
	if err := json.Unmarshal(body["dhm"], &struct {
		Ortho *DHMOrthoClientConfig `json:"ortho"`
	}{&o}); err != nil {
		t.Fatal(err)
	}
	c := dhm.Coverage
	want := DHMOrthoClientConfig{
		URL: "/api/map/dhm/ortho/{z}/{x}/{y}", MinZoom: 7, MaxZoom: 20,
		Bounds:      [2][2]float64{{c.South, c.West}, {c.North, c.East}},
		Attribution: dhm.OrthoAttribution,
		DatasetName: dhm.OrthoDatasetName, DatasetURL: dhm.OrthoDatasetURL,
		LicenseName: dhm.LicenseName, LicenseURL: dhm.LicenseURL,
	}
	if o != want {
		t.Fatalf("dhm.ortho\n got %+v\nwant %+v", o, want)
	}
	// The hillshade part of the block is unchanged by ortho.
	var hill DHMClientConfig
	json.Unmarshal(body["dhm"], &hill)
	if hill.URL != dhmClientPath || hill.Attribution != dhm.Attribution {
		t.Fatalf("hillshade block changed: %+v", hill)
	}

	z, x, y := dkTile()
	target := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y)).Replace(o.URL)
	w := doOrtho(router, target)
	// The /api no-store default must not override the tile's own caching.
	assertOrthoTile(t, "routed", w, "miss")
	if w := doOrtho(router, target+"?x=1"); w.Code != http.StatusBadRequest {
		t.Fatalf("routed tile with a query: status %d, want 400", w.Code)
	}
}

// A config that puts the token, or anything else, into map.dhm.ortho
// ships none of it: the block is lifted out of the client-visible map, and
// dhm.ortho is built from constants only.
func TestDHMOrtho728_HostileConfigShipsNoSecrets(t *testing.T) {
	const secret = "ortho-728-HOSTILE-secret"
	dir := t.TempDir()
	cfgJSON := `{"map": {"dhm": {"enabled": true, "token": "` + dhmTestToken + `", "layer": "terrain",
		"ortho": {"enabled": true, "token": "` + secret + `", "url": "https://` + secret + `/", "attribution": "` + secret + `",
		"apikey": "` + secret + `", "layer": "` + secret + `"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DHM.Ortho.Enabled {
		t.Fatal("map.dhm.ortho.enabled not read")
	}
	srv, router := setupTestServer(t)
	srv.cfg = cfg
	srv.dhm = newTestOrthoProxy(t, orthoAwareUpstream(t), &dhmLogSink{}, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	for _, s := range []string{dhmTestToken, secret} {
		if strings.Contains(w.Body.String(), s) {
			t.Fatalf("%q in /api/config/client: %s", s, w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), `"url":"/api/map/dhm/ortho/{z}/{x}/{y}"`) {
		t.Fatalf("ortho block missing: %s", w.Body.String())
	}
}

// The shipped example keeps ortho off.
func TestDHMOrtho728_ExampleConfigIsOff(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Map struct {
			DHM struct {
				Ortho *struct {
					Enabled *bool `json:"enabled"`
				} `json:"ortho"`
			} `json:"dhm"`
		} `json:"map"`
	}
	if err := json.Unmarshal(data, &ex); err != nil {
		t.Fatal(err)
	}
	if o := ex.Map.DHM.Ortho; o == nil || o.Enabled == nil || *o.Enabled {
		t.Fatalf("config.example.json map.dhm.ortho.enabled must be present and false: %+v", o)
	}
}

// ── OpenAPI ────────────────────────────────────────────────────────────────

func TestDHMOrtho728_OpenAPIDescribesTheRoute(t *testing.T) {
	meta, ok := routeDescriptions()["GET "+dhmOrthoRoute]
	if !ok {
		t.Fatal("route not described")
	}
	if dhmOrthoRoute != "/api/map/dhm/ortho/{z}/{x}/{y}" {
		t.Fatalf("route %q", dhmOrthoRoute)
	}
	if len(meta.QueryParams) != 0 {
		t.Fatalf("the route takes no query parameters, OpenAPI declares %v", meta.QueryParams)
	}
	if got := extractPathParams(dhmOrthoRoute); strings.Join(got, ",") != "z,x,y" {
		t.Fatalf("path params %v", got)
	}
	for _, want := range []string{"orto_foraar_webm", "7-20", "map.dhm.ortho.enabled", "hit", "miss", "outside", "unavailable", "busy", "query string"} {
		if !strings.Contains(meta.Description, want) {
			t.Errorf("description lacks %q", want)
		}
	}
	if len(meta.Errors) == 0 || meta.Errors[0].Status != "429" {
		t.Fatalf("errors %+v, want the shared 429", meta.Errors)
	}
}

// AGENTS rule 0: the ortho cache-hit hot path.
func BenchmarkDHMOrthoCacheHit(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(orthoJPEG)
	}))
	b.Cleanup(srv.Close)
	p := newDHMProxy(orthoTestConfig(), testDHMOptions(srv.URL+"/", &dhmLogSink{}))
	h := orthoRouter(p)
	z, x, y := dkTile()
	r := httptest.NewRequest("GET", orthoPath(z, x, y), nil)
	if w := httptest.NewRecorder(); func() string { h.ServeHTTP(w, r); return w.Header().Get("X-CoreScope-Tile") }() != "miss" {
		b.Fatal("warm-up was not a miss")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("X-CoreScope-Tile") != "hit" {
			b.Fatal("not a cache hit")
		}
	}
}
