package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #681 M3 — GET /api/terrain/profile. Every test runs against a fake
// ElevationSource or a fake WCS upstream (httptest); none reaches
// Dataforsyningen, and no real token exists here.

// fakeElevation answers every raster with elev at the pixel centres,
// after checking the raster is inside the allow-list.
type fakeElevation struct {
	t     *testing.T
	elev  func(e, n float64) float32
	hook  func(ctx context.Context, r dhm.RasterRequest) error
	calls atomic.Int64
	mu    sync.Mutex
	model map[dhm.Model]int
}

func newFakeElevation(t *testing.T, elev func(e, n float64) float32) *fakeElevation {
	return &fakeElevation{t: t, elev: elev, model: map[dhm.Model]int{}}
}

func flatElev(h float32) func(e, n float64) float32 {
	return func(float64, float64) float32 { return h }
}

func (f *fakeElevation) FetchRaster(ctx context.Context, r dhm.RasterRequest) (dhm.Grid, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.model[r.Model]++
	f.mu.Unlock()
	if err := r.Validate(); err != nil {
		f.t.Errorf("invalid raster sent upstream: %+v", r)
		return dhm.Grid{}, err
	}
	if f.hook != nil {
		if err := f.hook(ctx, r); err != nil {
			return dhm.Grid{}, err
		}
	}
	g := dhm.Grid{Width: r.Width, Height: r.Height, Values: make([]float32, r.Width*r.Height),
		NoData: -9999, HasNoData: true,
		Georef: dhm.Georef{OriginE: r.BBox[0], OriginN: r.BBox[3],
			PixelE: (r.BBox[2] - r.BBox[0]) / float64(r.Width), PixelN: (r.BBox[3] - r.BBox[1]) / float64(r.Height)}}
	for row := 0; row < r.Height; row++ {
		for col := 0; col < r.Width; col++ {
			g.Values[row*r.Width+col] = f.elev(g.Georef.OriginE+(float64(col)+0.5)*g.Georef.PixelE,
				g.Georef.OriginN-(float64(row)+0.5)*g.Georef.PixelN)
		}
	}
	return g, nil
}

const aarhusPt = "56.16290,10.20390"

// offsetPt is the point dE, dN metres from lat,lon in UTM 32.
func offsetPt(lat, lon, dE, dN float64) string {
	e, n := dhm.ToUTM32(lat, lon)
	la, lo := dhm.FromUTM32(e+dE, n+dN)
	return fmt.Sprintf("%.5f,%.5f", la, lo)
}

func intp(v int) *int           { return &v }
func floatp(v float64) *float64 { return &v }

type terrainEnv struct {
	srv    *Server
	router *mux.Router
	p      *terrainProfiler
	logs   *dhmLogSink
}

// newTerrainEnv is a test server with the profile enabled on src.
func newTerrainEnv(t *testing.T, src dhm.ElevationSource, pc dhm.ProfileConfig, radio *RadioConfig) *terrainEnv {
	t.Helper()
	srv, router := setupTestServer(t)
	logs := &dhmLogSink{}
	pc.Enabled = true
	srv.cfg.Radio = radio
	srv.terrain = newTerrainProfiler(pc, radio, terrainProfilerDeps{
		src:        src,
		logf:       logs.logf,
		logFailure: func(what string, err error) { logs.logf("[dhm] upstream %s failed (%s)", what, dhm.FailureReason(err)) },
		observers:  func() ([]Observer, error) { return nil, nil },
	})
	if srv.terrain == nil {
		t.Fatal("newTerrainProfiler returned nil for an enabled profile")
	}
	return &terrainEnv{srv: srv, router: router, p: srv.terrain, logs: logs}
}

func (e *terrainEnv) get(query string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/terrain/profile?"+query, nil)
	for _, m := range mod {
		m(req)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func pathQuery(a, b string, extra ...string) string {
	q := "a=" + url.QueryEscape(a) + "&b=" + url.QueryEscape(b)
	for _, x := range extra {
		q += "&" + x
	}
	return q
}

func decodeProfile(t *testing.T, w *httptest.ResponseRecorder) TerrainProfileResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp TerrainProfileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return resp
}

func wantError(t *testing.T, w *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status %d, want %d: %s", w.Code, code, w.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error != msg {
		t.Fatalf("body %q, want error %q", w.Body.String(), msg)
	}
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s = %.6f, want %.6f ±%g", what, got, want, tol)
	}
}

// ── Enablement ─────────────────────────────────────────────────────────────

func TestTerrainProfile_DisabledIs404(t *testing.T) {
	_, router := setupTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/terrain/profile?"+pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 5000, 0)), nil))
	wantError(t, w, 404, "not found")

	proxy := &dhmProxy{} // an active tile proxy alone does not enable the profile
	for name, cfg := range map[string]*Config{
		"no dhm":              {},
		"dhm without profile": {DHM: dhm.Config{Enabled: true, Token: "t"}},
	} {
		if p := newTerrainProfilerFromConfig(cfg, proxy, nil); p != nil {
			t.Fatalf("%s: profile offered", name)
		}
	}
	on := &Config{DHM: dhm.Config{Enabled: true, Token: "t", Profile: dhm.ProfileConfig{Enabled: true}}}
	if p := newTerrainProfilerFromConfig(on, nil, nil); p != nil {
		t.Fatal("profile offered without an active DHM client")
	}
	if p := newTerrainProfilerFromConfig(on, newDHMProxy(on.DHM, defaultDHMProxyOptions()), nil); p == nil {
		t.Fatal("enabled profile with an active client not offered")
	}
}

// ── Validation ─────────────────────────────────────────────────────────────

func TestTerrainProfile_RejectsJunkWithoutEcho(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	b := offsetPt(56.1629, 10.2039, 5000, 0)
	long := strings.Repeat("1", 200)
	for _, q := range []string{
		"",
		"a=" + aarhusPt,
		"b=" + b,
		pathQuery(aarhusPt, b, "x=1"),
		pathQuery(aarhusPt, b, "token=abc"),
		pathQuery(aarhusPt, b) + "&a=" + aarhusPt,
		pathQuery(aarhusPt, b, "model=dsm"),
		pathQuery(aarhusPt, b, "model="),
		pathQuery(aarhusPt, b, "model=<script>"),
		pathQuery("NaN,10", b),
		pathQuery("56,Inf", b),
		pathQuery("0x1p5,10", b),
		pathQuery("5.6e1,10", b),
		pathQuery("91,10", b),
		pathQuery("56,181", b),
		pathQuery("56,-181", b),
		pathQuery("56,10,3", b),
		pathQuery("56", b),
		pathQuery(" 56,10", b),
		pathQuery("56.1234567890123,10", b),
		pathQuery(long, b),
		pathQuery(strings.Repeat("a", 63), b),
		pathQuery(strings.Repeat("g", 64), b),
		pathQuery(aarhusPt, aarhusPt),
		pathQuery(aarhusPt, b, "ha=-1"),
		pathQuery(aarhusPt, b, "ha=100.5"),
		pathQuery(aarhusPt, b, "hb=NaN"),
		pathQuery(aarhusPt, b, "hb="),
		pathQuery(aarhusPt, b, "k=0.1"),
		pathQuery(aarhusPt, b, "k=11"),
		pathQuery(aarhusPt, b, "k=Inf"),
		pathQuery(aarhusPt, b, "freq=100"),
		pathQuery(aarhusPt, b, "freq=2501"),
		pathQuery(aarhusPt, b, "freq=1e3"),
		pathQuery(aarhusPt, b, "freq="+long),
	} {
		w := env.get(q)
		wantError(t, w, 400, "invalid terrain request")
		for _, echo := range []string{"script", "token", "NaN", "Inf", "0x1p5", long[:20], "gggg"} {
			if strings.Contains(w.Body.String(), echo) {
				t.Fatalf("query %q: response echoes %q", q, echo)
			}
		}
	}
	if src.calls.Load() != 0 {
		t.Fatalf("%d upstream calls for invalid requests", src.calls.Load())
	}
	// Non-GET is not routed.
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest("POST", "/api/terrain/profile?"+pathQuery(aarhusPt, b), nil))
	if w.Code == 200 {
		t.Fatal("POST served")
	}
}

// ── Limits ─────────────────────────────────────────────────────────────────

func TestTerrainProfile_OverLengthIs400WithoutUpstream(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	wantError(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 0, -81000))), 400, "path too long")
	// Across the globe: rejected by length, not by the projection.
	wantError(t, env.get(pathQuery(aarhusPt, "-33.86000,151.20000")), 400, "path too long")

	short := newTerrainEnv(t, src, dhm.ProfileConfig{MaxPathKm: floatp(10)}, nil)
	wantError(t, short.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 0, -15000))), 400, "path too long")
	if src.calls.Load() != 0 {
		t.Fatalf("%d upstream calls for over-length paths", src.calls.Load())
	}
	// Under the limit it is served.
	decodeProfile(t, short.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 0, -9000))))
}

func TestTerrainProfile_BothModel20kmMakesAtMost16Calls(t *testing.T) {
	src := newFakeElevation(t, flatElev(3))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	// 45°: the most expensive bearing for axis-aligned rasters.
	b := offsetPt(56.1629, 10.2039, 20000/math.Sqrt2, 20000/math.Sqrt2)
	resp := decodeProfile(t, env.get(pathQuery(aarhusPt, b)))
	if n := src.calls.Load(); n > 16 || n == 0 {
		t.Fatalf("%d upstream calls for a 20 km both profile, want ≤ 16", n)
	}
	if src.model[dhm.ModelTerrain] != src.model[dhm.ModelSurface] || src.model[dhm.ModelTerrain] != 8 {
		t.Fatalf("calls per model %v, want 8 + 8", src.model)
	}
	if resp.Model != "both" || resp.Coverage.State != "full" || len(resp.Samples) > 1024 {
		t.Fatalf("model %q coverage %q samples %d", resp.Model, resp.Coverage.State, len(resp.Samples))
	}
	if resp.Samples[0].DSM == nil || resp.Verdict.Surface == nil {
		t.Fatal("both: DSM samples or surface info missing")
	}

	// model=dtm skips the surface fetch entirely.
	src2 := newFakeElevation(t, flatElev(3))
	env2 := newTerrainEnv(t, src2, dhm.ProfileConfig{}, nil)
	resp = decodeProfile(t, env2.get(pathQuery(aarhusPt, b, "model=dtm")))
	if src2.model[dhm.ModelSurface] != 0 || src2.model[dhm.ModelTerrain] != 8 {
		t.Fatalf("dtm calls per model %v", src2.model)
	}
	if resp.Samples[0].DSM != nil || resp.Verdict.Surface != nil {
		t.Fatal("dtm: DSM data in the response")
	}
}

// ── Cache + singleflight ───────────────────────────────────────────────────

func TestTerrainProfile_CacheHitMakesNoUpstreamCalls(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	b := offsetPt(56.1629, 10.2039, 8000, 3000)
	w := env.get(pathQuery(aarhusPt, b))
	cold := decodeProfile(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("first: X-CoreScope-Cache %q", w.Header().Get("X-CoreScope-Cache"))
	}
	calls := src.calls.Load()
	// Same path with other physics, and endpoints jittered below the
	// 5-decimal (~1 m) snap: the elevation data is the same.
	for _, q := range []string{
		pathQuery(aarhusPt, b),
		pathQuery(aarhusPt, b, "ha=20", "hb=15", "k=1", "freq=910.525"),
		pathQuery("56.162904,10.203896", b),
	} {
		w := env.get(q)
		warm := decodeProfile(t, w)
		if src.calls.Load() != calls {
			t.Fatalf("%q: cache hit made %d upstream calls", q, src.calls.Load()-calls)
		}
		if w.Header().Get("X-CoreScope-Cache") != "hit" || len(warm.Samples) != len(cold.Samples) {
			t.Fatalf("%q: X-CoreScope-Cache %q", q, w.Header().Get("X-CoreScope-Cache"))
		}
	}
	// The physics follow the request, not the cache.
	warm := decodeProfile(t, env.get(pathQuery(aarhusPt, b, "ha=20", "k=1", "freq=910.525")))
	if warm.A.AntennaM != 20 || warm.Physics.K != 1 || warm.Physics.FreqMHz != 910.525 {
		t.Fatalf("cached response kept stale physics: a %+v physics %+v", warm.A, warm.Physics)
	}
	// A different model is a different entry.
	decodeProfile(t, env.get(pathQuery(aarhusPt, b, "model=dtm")))
	if src.calls.Load() == calls {
		t.Fatal("model=dtm served from the both entry without a fetch")
	}
}

func TestTerrainProfile_CacheEvictionAndTTL(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{CacheEntries: intp(2), CacheTTLHours: floatp(1)}, nil)
	clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	env.p.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	paths := []string{
		pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 3000, 0), "model=dtm"),
		pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 0, 3000), "model=dtm"),
		pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, -3000, 0), "model=dtm"),
	}
	fetched := func(q string) bool {
		before := src.calls.Load()
		decodeProfile(t, env.get(q))
		return src.calls.Load() != before
	}
	for _, q := range paths {
		if !fetched(q) {
			t.Fatal("cold path served from cache")
		}
	}
	if fetched(paths[2]) || fetched(paths[1]) {
		t.Fatal("recent entries evicted")
	}
	if !fetched(paths[0]) {
		t.Fatal("cache holds more than cacheEntries=2")
	}
	if env.p.cache.Len() != 2 {
		t.Fatalf("cache len %d", env.p.cache.Len())
	}
	// TTL: an hour later the entry is gone.
	if fetched(paths[0]) {
		t.Fatal("fresh entry refetched")
	}
	mu.Lock()
	clock = clock.Add(time.Hour + time.Second)
	mu.Unlock()
	if !fetched(paths[0]) {
		t.Fatal("entry served past its TTL")
	}
}

// N3 (#689 review): the worst-case cache footprint at the documented maxima
// of cacheEntries and maxSamples must match config.example.json's
// _comment_cacheEntries (~105 MB); a mutant of the per-entry size formula,
// or a drift in either bound, should move this off the documented figure.
func TestTerrainProfile_CacheFootprintAtMaxima(t *testing.T) {
	warned := false
	set := resolveTerrainSettings(dhm.ProfileConfig{CacheEntries: intp(4096), MaxSamples: intp(3200)},
		func(string, ...interface{}) { warned = true })
	if warned {
		t.Fatal("4096/3200 are the documented maxima and should not warn")
	}
	if set.cacheEntries != 4096 || set.plan.MaxSamples != 3200 {
		t.Fatalf("resolved maxima %d/%d, want 4096/3200", set.cacheEntries, set.plan.MaxSamples)
	}
	maxBytes := terrainCacheMaxBytes(set.cacheEntries, set.plan.MaxSamples)
	const documentedMB = 105_000_000 // config.example.json _comment_cacheEntries
	if maxBytes > documentedMB || float64(documentedMB-maxBytes) > 2_000_000 {
		t.Fatalf("worst case %d bytes (%.1f MB) no longer matches the documented ~105 MB", maxBytes, float64(maxBytes)/1e6)
	}
}

func TestTerrainProfile_SingleflightCollapses20Requests(t *testing.T) {
	release := make(chan struct{})
	src := newFakeElevation(t, flatElev(0))
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return dhm.FetchError{Reason: "canceled"}
		}
	}
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	q := pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 20000/math.Sqrt2, 20000/math.Sqrt2))
	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 20 visitors: each has its own rate-limit budget.
			w := env.get(q, func(r *http.Request) { r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1000", i+1) })
			codes[i], bodies[i] = w.Code, w.Body.String()
		}(i)
	}
	deadline := time.Now().Add(3 * time.Second)
	for env.p.flight.Pending() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests joined", env.p.flight.Pending(), n)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	for i := range codes {
		if codes[i] != 200 || bodies[i] != bodies[0] {
			t.Fatalf("request %d: %d", i, codes[i])
		}
	}
	if got := src.calls.Load(); got != 16 {
		t.Fatalf("%d upstream calls for 20 identical requests, want one profile's 16", got)
	}
}

// #696 finding 1: uncached profiles in flight are bounded server-wide,
// whatever the number of client addresses. Each admitted profile runs
// 2 models × 8 rasters here; without the bound 40 distinct concurrent
// requests ran 40 such fetches at once (the review measured 681
// goroutines), all waiting on the shared upstream budget. Beyond the
// bound a request gets 503 with Retry-After at once, and cancellation
// still frees both the goroutines and the slots.
func TestTerrainProfile_GlobalInFlightBound(t *testing.T) {
	var blocked atomic.Int64
	var release atomic.Bool
	src := newFakeElevation(t, flatElev(0))
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error {
		if release.Load() {
			return nil
		}
		blocked.Add(1)
		defer blocked.Add(-1)
		<-ctx.Done()
		return dhm.FetchError{Reason: "canceled"}
	}
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	limit := env.p.set.maxInFlight
	if limit != 4 {
		t.Fatalf("default maxInFlight %d, want 4", limit)
	}
	// perProfile: a 20 km diagonal with chunkKm 2.5 and model=both.
	const n, perProfile = 40, 16
	// A distinct path per i, so no request joins another's fetch.
	queryFor := func(i int) string {
		return pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 20000/math.Sqrt2+float64(3*i), 20000/math.Sqrt2))
	}
	runtime.GC()
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	answered := make(chan *httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answered <- env.get(queryFor(i), func(r *http.Request) {
				*r = *r.WithContext(ctx)
				r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1000", i+1) // own per-IP budget each
			})
		}(i)
	}
	settled := func() bool { return blocked.Load() == int64(limit*perProfile) && len(answered) == n-limit }
	for deadline := time.Now().Add(3 * time.Second); !settled() && time.Now().Before(deadline); {
		time.Sleep(2 * time.Millisecond)
	}
	// Per admitted profile: its request, the flight, two model fetches
	// and perProfile rasters. Unbounded this is n×(4+perProfile) = 800.
	if got, max := runtime.NumGoroutine()-baseline, limit*(4+perProfile)+8; got > max {
		t.Fatalf("%d goroutines for %d concurrent profiles, want at most %d", got, n, max)
	}
	if !settled() {
		t.Fatalf("%d rasters blocked upstream, %d requests answered; want %d and %d",
			blocked.Load(), len(answered), limit*perProfile, n-limit)
	}
	if calls := src.calls.Load(); calls != int64(limit*perProfile) {
		t.Fatalf("%d upstream calls, want %d", calls, limit*perProfile)
	}
	for busy := 0; busy < n-limit; busy++ {
		w := <-answered
		wantError(t, w, http.StatusServiceUnavailable, "terrain profiles busy")
		if w.Header().Get("Retry-After") != "5" {
			t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
		}
	}
	if strings.Contains(env.logs.String(), "upstream") {
		t.Fatalf("a busy answer was logged as an upstream failure: %q", env.logs.String())
	}

	// The clients leave: every goroutine and every slot is released.
	cancel()
	wg.Wait()
	waitUntil(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= baseline+2 })
	if blocked.Load() != 0 || env.p.cache.Len() != 0 {
		t.Fatalf("blocked %d, cached %d after cancel", blocked.Load(), env.p.cache.Len())
	}
	release.Store(true)
	for i := 0; i < limit; i++ {
		decodeProfile(t, env.get(queryFor(100+i), func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1000" }))
	}
}

// ── Upstream failures ──────────────────────────────────────────────────────

// wcsUpstream is a fake dhm_wcs_DAF: h answers GetCoverage; tiles on
// dhm_DAF always succeed.
func wcsUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request, width, height int)) *fakeUpstream {
	return newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, dhm.WCSService) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngStub)
			return
		}
		q := r.URL.Query()
		width, _ := strconv.Atoi(q.Get("width"))
		height, _ := strconv.Atoi(q.Get("height"))
		h(w, r, width, height)
	})
}

func tiffAnswer(w http.ResponseWriter, _ *http.Request, width, height int) {
	vals := make([]float32, width*height)
	for i := range vals {
		vals[i] = float32(i%53) * 0.25
	}
	w.Header().Set("Content-Type", "image/tiff")
	w.Write(dhmtest.GeoTIFF(width, height, vals, nil, true))
}

// newWCSEnv is a profile on the real WCS source and client, against up.
func newWCSEnv(t *testing.T, up *fakeUpstream, tweak func(*dhm.ClientOptions)) (*terrainEnv, *dhm.Client) {
	t.Helper()
	logs := &dhmLogSink{}
	o := testDHMOptions(up.srv.URL+"/", logs).client
	if tweak != nil {
		tweak(&o)
	}
	client := dhm.NewClient(dhm.Config{Enabled: true, Token: dhmTestToken}, o)
	env := newTerrainEnv(t, dhm.NewWCSSource(client), dhm.ProfileConfig{}, nil)
	env.logs = logs
	env.p.logFailure = client.LogFailure
	return env, client
}

func TestTerrainProfile_UpstreamFailuresAreClean(t *testing.T) {
	for _, c := range []struct {
		name string
		h    func(w http.ResponseWriter, r *http.Request, width, height int)
		code int
		msg  string
		log  string
	}{
		{"403", func(w http.ResponseWriter, r *http.Request, _, _ int) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(403)
			w.Write([]byte("User not authorized " + r.Header.Get("token")))
		}, 502, "terrain data unavailable", "status 403"},
		{"500", func(w http.ResponseWriter, r *http.Request, _, _ int) {
			w.Header().Set("Content-Type", "application/vnd.ogc.se_xml")
			w.WriteHeader(500)
			w.Write([]byte("<ServiceException>boom " + r.URL.String() + " " + r.Header.Get("token") + "</ServiceException>"))
		}, 502, "terrain data unavailable", "status 500"},
		{"html 200", func(w http.ResponseWriter, r *http.Request, _, _ int) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>" + r.Header.Get("token") + "</html>"))
		}, 502, "terrain data unavailable", "unexpected content type"},
		{"timeout", func(w http.ResponseWriter, r *http.Request, _, _ int) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, 504, "terrain data timed out", "timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := wcsUpstream(t, c.h)
			env, _ := newWCSEnv(t, up, nil)
			env.p.set.upstreamTimeout = 150 * time.Millisecond
			start := time.Now()
			w := env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 4000, 0)))
			wantError(t, w, c.code, c.msg)
			if el := time.Since(start); el > 2*time.Second {
				t.Fatalf("took %v", el)
			}
			assertNoToken(t, c.name, w, env.logs)
			for _, leak := range []string{"User not authorized", "ServiceException", "boom", "html", "dhm_wcs_DAF", "127.0.0.1"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Fatalf("upstream text %q in response", leak)
				}
			}
			if !strings.Contains(env.logs.String(), "("+c.log+")") {
				t.Fatalf("log %q lacks category %q", env.logs.String(), c.log)
			}
			// A failure is not cached.
			if env.p.cache.Len() != 0 {
				t.Fatal("failure cached")
			}
		})
	}
}

func TestTerrainProfile_CancelledRequestReleasesSlots(t *testing.T) {
	var waiting atomic.Int64
	up := wcsUpstream(t, func(w http.ResponseWriter, r *http.Request, _, _ int) {
		waiting.Add(1)
		defer waiting.Add(-1)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	env, client := newWCSEnv(t, up, func(o *dhm.ClientOptions) {
		o.Concurrency = 2
		o.Timeout = 10 * time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		req := httptest.NewRequest("GET", "/api/terrain/profile?"+pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 20000, 0)), nil).WithContext(ctx)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		done <- w
	}()
	waitUntil(t, "both slots busy upstream", func() bool { return waiting.Load() == 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler kept waiting after the client left")
	}
	waitUntil(t, "upstream calls stopped", func() bool { return waiting.Load() == 0 })
	// Both slots are free again: a tile on the shared client is served at
	// once instead of after the 10 s Timeout.
	start := time.Now()
	if _, err := client.FetchHillshadeTile(dhm.TileKey{Z: 10, X: 540, Y: 316}); err != nil {
		t.Fatalf("tile after cancel: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("slots not released: tile took %v", el)
	}
}

func TestTerrainProfile_WholeProfileDeadline(t *testing.T) {
	up := wcsUpstream(t, func(w http.ResponseWriter, r *http.Request, _, _ int) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	env, _ := newWCSEnv(t, up, func(o *dhm.ClientOptions) { o.Timeout = 10 * time.Second })
	if env.p.deadline != 25*time.Second {
		t.Fatalf("whole-profile deadline %v, want 25s", env.p.deadline)
	}
	env.p.deadline = 150 * time.Millisecond
	env.p.set.upstreamTimeout = 10 * time.Second
	start := time.Now()
	wantError(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 6000, 0))), 504, "terrain data timed out")
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("deadline not applied: %v", el)
	}
	waitUntil(t, "upstream calls stopped", func() bool { return up.inFlight.Load() == 0 })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ── Coverage ───────────────────────────────────────────────────────────────

func TestTerrainProfile_CoverageGating(t *testing.T) {
	t.Run("outside", func(t *testing.T) {
		src := newFakeElevation(t, flatElev(0))
		env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
		w := env.get(pathQuery("53.55000,9.99000", "53.60000,10.10000"))
		resp := decodeProfile(t, w)
		if resp.Coverage.State != "none" || resp.Coverage.Pct != 0 || resp.Verdict.State != "unknown" ||
			resp.Samples == nil || len(resp.Samples) != 0 || resp.A.GroundM != nil {
			t.Fatalf("outside: %+v", resp)
		}
		if src.calls.Load() != 0 || w.Header().Get("X-CoreScope-Cache") != "none" {
			t.Fatalf("outside: %d calls, cache %q", src.calls.Load(), w.Header().Get("X-CoreScope-Cache"))
		}
		if !strings.Contains(w.Body.String(), `"samples":[]`) {
			t.Fatal("samples not an empty array")
		}
	})
	t.Run("crossing out", func(t *testing.T) {
		src := newFakeElevation(t, flatElev(0))
		env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
		resp := decodeProfile(t, env.get(pathQuery("54.65000,11.35000", "54.42000,11.20000")))
		if resp.Coverage.State != "partial" || resp.Verdict.State != "unknown" || resp.Verdict.WorstClearanceM != nil {
			t.Fatalf("crossing: coverage %+v verdict %+v", resp.Coverage, resp.Verdict)
		}
		if resp.Coverage.FirstGapM == nil || *resp.Coverage.FirstGapM <= 0 || resp.Coverage.Pct <= 0 || resp.Coverage.Pct >= 100 {
			t.Fatalf("crossing: coverage %+v", resp.Coverage)
		}
		if resp.B.GroundM != nil || resp.A.GroundM == nil {
			t.Fatal("crossing: endpoint ground wrong")
		}
		last := resp.Samples[len(resp.Samples)-1]
		if last.DTM != nil {
			t.Fatal("crossing: sample outside DK has a value")
		}
	})
	t.Run("nodata run", func(t *testing.T) {
		e0, _ := dhm.ToUTM32(56.1629, 10.2039)
		src := newFakeElevation(t, func(e, n float64) float32 {
			if e > e0+4500 && e < e0+5500 {
				return -9999
			}
			return 1
		})
		env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
		resp := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 10000, 0), "model=dtm")))
		if resp.Coverage.State != "partial" || resp.Verdict.State != "unknown" {
			t.Fatalf("nodata: %+v %+v", resp.Coverage, resp.Verdict)
		}
		near(t, "firstGapM", *resp.Coverage.FirstGapM, 4500, 30)
		near(t, "pct", resp.Coverage.Pct, 90, 1)
	})
	t.Run("full", func(t *testing.T) {
		src := newFakeElevation(t, flatElev(1))
		env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
		resp := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 5000, 0))))
		if resp.Coverage.State != "full" || resp.Coverage.Pct != 100 || resp.Coverage.FirstGapM != nil || resp.Verdict.State == "unknown" {
			t.Fatalf("full: %+v %+v", resp.Coverage, resp.Verdict)
		}
	})
}

// #692: the DHM WCS answers 0.0, not nodata, outside Denmark, so the fake
// source is flat 0 everywhere, as upstream is. A path onto foreign land
// must be partial with no verdict on either model, the gap starting at the
// foreign coast or border; a path over Danish land and any sea is full.
func TestTerrainProfile_LeavingDenmarkIsPartial(t *testing.T) {
	for _, c := range []struct {
		name, a, b string
		lo, hi     float64 // where the gap must start, metres along the path
	}{
		{"Copenhagen→Malmö", "55.67610,12.56830", "55.60500,13.00380", 24500, 27000},
		// #693 review round 2, F1: the likeliest cross-border path.
		{"Helsingør→Kärnan", "56.03610,12.61360", "56.04750,12.69690", 3500, 4900},
		{"Aabenraa→Flensburg", "55.04430,9.41740", "54.78360,9.43210", 24000, 27500},
		{"Rønne→Ystad", "55.10090,14.70670", "55.44000,13.83000", 62500, 65000},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := newFakeElevation(t, flatElev(0))
			env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
			resp := decodeProfile(t, env.get(pathQuery(c.a, c.b, "model=both", "ha=10", "hb=10")))
			if resp.Coverage.State != "partial" || resp.Verdict.State != "unknown" || resp.Verdict.WorstClearanceM != nil {
				t.Fatalf("coverage %+v verdict %+v", resp.Coverage, resp.Verdict)
			}
			if resp.Verdict.Surface == nil || resp.Verdict.Surface.State != "unknown" {
				t.Fatalf("surface verdict %+v, want unknown", resp.Verdict.Surface)
			}
			if g := resp.Coverage.FirstGapM; g == nil || *g < c.lo || *g > c.hi {
				t.Fatalf("first gap %v, want %v–%v m", resp.Coverage, c.lo, c.hi)
			}
			if resp.A.GroundM == nil || resp.B.GroundM != nil {
				t.Fatalf("ground A %v (want 0), B %v (want null)", resp.A.GroundM, resp.B.GroundM)
			}
			last := resp.Samples[len(resp.Samples)-1]
			if last.DTM != nil || last.DSM != nil {
				t.Fatalf("foreign sample has dtm %v dsm %v", last.DTM, last.DSM)
			}
		})
	}
	for _, c := range []struct{ name, a, b string }{
		{"Great Belt, Nyborg→Korsør", "55.31270,10.78970", "55.32970,11.13930"},
		{"Kattegat, Grenaa→Anholt", "56.41580,10.87830", "56.71670,11.51670"},
		{"Kattegat, Hesselø→Gilleleje", "56.19800,11.71000", "56.12100,12.31000"},
		{"Øresund, Copenhagen→Saltholm east", "55.67610,12.56830", "55.66000,12.86000"},
		{"Øresund, Helsingør→Copenhagen", "56.03610,12.61360", "55.67610,12.56830"},
		{"Øresund, Copenhagen→Saltholm", "55.67610,12.56830", "55.64000,12.76000"},
		// #704 — Okseøerne are Danish islets 2.1 km off the German shore
		// of Flensborg Fjord, outside the 1250 m buffer, so a path that
		// ends on one keeps its verdict. The coordinates are surveyed
		// (OpenStreetMap ways 4098190/4100737, agreeing with Wikidata
		// Q2013616); #704's remembered ones are German land on Holnis.
		{"Flensborg Fjord, Kollund→Store Okseø", "54.83500,9.46800", "54.85785,9.50905"},
		{"Flensborg Fjord, Kollund→Lille Okseø", "54.83500,9.46800", "54.85761,9.50235"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := newFakeElevation(t, flatElev(0))
			env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
			resp := decodeProfile(t, env.get(pathQuery(c.a, c.b, "model=both", "ha=10", "hb=10")))
			if resp.Coverage.State != "full" || resp.Coverage.FirstGapM != nil || resp.Verdict.State == "unknown" ||
				resp.Verdict.Surface == nil || resp.Verdict.Surface.State == "unknown" {
				t.Fatalf("coverage %+v verdict %+v", resp.Coverage, resp.Verdict)
			}
		})
	}
}

// ── Physics ────────────────────────────────────────────────────────────────

func TestTerrainProfile_PhysicsAndVerdict(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	resp := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 20000, 0), "ha=10", "hb=10", "model=dtm")))
	near(t, "distance", resp.DistanceM, 20006, 3)
	near(t, "lambda", resp.Physics.LambdaM, 0.344740, 1e-6)
	near(t, "f1Max", resp.Physics.F1MaxM, 41.53, 0.02)
	near(t, "k", resp.Physics.K, 4.0/3, 1e-4)
	if resp.Physics.FreqMHz != dhm.FirmwareFreqMHz || resp.Physics.FreqSource != "firmware" || resp.Physics.KSource != "default" {
		t.Fatalf("physics %+v", resp.Physics)
	}
	// Flat ground, 10 m masts: the bulge (5.89 m) leaves 4.1 m of a 41.5 m
	// zone at mid-path.
	if resp.Verdict.State != "fresnel60" || resp.Verdict.Model != "dtm" || resp.Verdict.Criterion != 0.6 {
		t.Fatalf("verdict %+v", resp.Verdict)
	}
	near(t, "worstAtM", *resp.Verdict.WorstAtM, 10003, 20)
	near(t, "worstClearanceM", *resp.Verdict.WorstClearanceM, 10-5.89, 0.05)
	// Higher masts clear the zone; ground-level antennas are blocked.
	if v := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 20000, 0), "ha=60", "hb=60", "model=dtm"))).Verdict; v.State != "clear" {
		t.Fatalf("60 m masts: %+v", v)
	}
	if v := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 20000, 0), "ha=0", "hb=0", "model=dtm"))).Verdict; v.State != "blocked" {
		t.Fatalf("0 m masts: %+v", v)
	}
}

// DTM decides; DSM is shown and assessed for information only.
func TestTerrainProfile_DTMDecidesDSMInforms(t *testing.T) {
	// Bare ground at 0 m under a 30 m canopy.
	split := modelSplit{dtm: newFakeElevation(t, flatElev(0)), dsm: newFakeElevation(t, flatElev(30))}
	env := newTerrainEnv(t, split, dhm.ProfileConfig{}, nil)
	resp := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 5000, 0), "ha=10", "hb=10")))
	if resp.Verdict.State == "blocked" || resp.Verdict.Model != "dtm" {
		t.Fatalf("verdict follows DSM: %+v", resp.Verdict)
	}
	if resp.Verdict.Surface == nil || resp.Verdict.Surface.State != "blocked" {
		t.Fatalf("surface info %+v", resp.Verdict.Surface)
	}
}

// N1 (#689 review): a nodata hole in the DTM makes the deciding verdict
// unknown, but the DSM is a separate, complete coverage and must not be
// allowed to report a confident "surface" verdict on a partial profile.
func TestTerrainProfile_SurfaceVerdictGatedByCoverage(t *testing.T) {
	e0, _ := dhm.ToUTM32(56.1629, 10.2039)
	dtmWithHole := newFakeElevation(t, func(e, n float64) float32 {
		if e > e0+4500 && e < e0+5500 {
			return -9999
		}
		return 0
	})
	split := modelSplit{dtm: dtmWithHole, dsm: newFakeElevation(t, flatElev(0))}
	env := newTerrainEnv(t, split, dhm.ProfileConfig{}, nil)
	resp := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 10000, 0), "ha=10", "hb=10")))
	if resp.Coverage.State != "partial" || resp.Verdict.State != "unknown" {
		t.Fatalf("expected a partial, unknown deciding verdict: %+v %+v", resp.Coverage, resp.Verdict)
	}
	if resp.Verdict.Surface == nil {
		t.Fatal("expected surface info to be present (DSM supplied)")
	}
	if resp.Verdict.Surface.State != "unknown" {
		t.Fatalf("surface verdict must be unknown on a partial profile, got %+v", resp.Verdict.Surface)
	}
	if resp.Verdict.Surface.WorstClearanceM != nil || resp.Verdict.Surface.WorstAtM != nil || resp.Verdict.Surface.WorstF1Fraction != nil {
		t.Fatalf("surface clearance numbers must be null when unknown: %+v", resp.Verdict.Surface)
	}
}

// #696 finding 2: a nodata endpoint in either model makes that model's
// verdict unknown. The DSM gap is the one that used to pass: the endpoint
// heights come from the DTM, so only the series itself shows it.
func TestTerrainProfile_EndpointNodata(t *testing.T) {
	e0, _ := dhm.ToUTM32(56.1629, 10.2039)
	gapAtA := func(e, n float64) float32 { // nodata west of A only
		if e < e0 {
			return -9999
		}
		return 0
	}
	// 60 m masts over flat ground: the whole F1 zone is clear.
	q := pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 5000, 0), "ha=60", "hb=60")
	t.Run("dsm", func(t *testing.T) {
		split := modelSplit{dtm: newFakeElevation(t, flatElev(0)), dsm: newFakeElevation(t, gapAtA)}
		resp := decodeProfile(t, newTerrainEnv(t, split, dhm.ProfileConfig{}, nil).get(q))
		if resp.Coverage.State != "full" || resp.Verdict.State != "clear" {
			t.Fatalf("DTM side should be full and clear: %+v %+v", resp.Coverage, resp.Verdict)
		}
		if resp.Samples[0].DSM != nil || resp.Samples[1].DSM == nil {
			t.Fatalf("fixture: want a DSM gap at A only, samples %+v %+v", resp.Samples[0], resp.Samples[1])
		}
		if s := resp.Verdict.Surface; s == nil || s.State != "unknown" || s.WorstClearanceM != nil {
			t.Fatalf("surface verdict with a DSM endpoint gap: %+v", s)
		}
	})
	t.Run("dtm", func(t *testing.T) {
		split := modelSplit{dtm: newFakeElevation(t, gapAtA), dsm: newFakeElevation(t, flatElev(0))}
		resp := decodeProfile(t, newTerrainEnv(t, split, dhm.ProfileConfig{}, nil).get(q))
		if resp.A.GroundM != nil || resp.Verdict.State != "unknown" || resp.Verdict.Surface.State != "unknown" {
			t.Fatalf("DTM endpoint gap: A %+v verdict %+v surface %+v", resp.A, resp.Verdict, resp.Verdict.Surface)
		}
	})
}

// #745: a surface (DSM) above the antenna at an endpoint makes the
// informational surface verdict blocked, at that endpoint, with a null F1
// fraction (the zone has no radius there), and says why: reason
// antennaBelowSurface and the endpoint (owner decision on #750). The
// deciding DTM verdict is the same as for the same path without a DSM.
func TestTerrainProfile_SurfaceAboveEndpointAntenna(t *testing.T) {
	x, _ := dhm.ToUTM32(56.1629, 10.2039)
	const distance = 1000
	for _, c := range []struct {
		name         string
		endpoint     int // the worst point: 0 = A, 1 = B
		atA, atB     bool
		wantEndpoint string
	}{
		{"A", 0, true, false, "A"},
		{"B", 1, false, true, "B"},
		// 45 m at B against 40 m at A makes B the deeper one.
		{"both", 1, true, true, "both"},
	} {
		t.Run(c.name, func(t *testing.T) {
			split := modelSplit{
				dtm: newFakeElevation(t, flatElev(0)),
				dsm: newFakeElevation(t, func(e, n float64) float32 {
					switch {
					case c.atA && e < x:
						return 40
					case c.atB && e > x+distance-1:
						return 45
					}
					return 0
				}),
			}
			env := newTerrainEnv(t, split, dhm.ProfileConfig{}, nil)
			q := pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, distance, 0), "ha=10", "hb=10")
			r := decodeProfile(t, env.get(q))
			end, ep := r.Samples[0], r.A
			if c.endpoint == 1 {
				end, ep = r.Samples[len(r.Samples)-1], r.B
			}
			if r.Coverage.State != "full" || end.DSM == nil || ep.GroundM == nil || *end.DSM <= *ep.GroundM+ep.AntennaM {
				t.Fatalf("fixture: want full coverage and DSM above the %s antenna: %+v endpoint %+v sample %+v", c.name, r.Coverage, ep, end)
			}
			for _, s := range r.Samples[1 : len(r.Samples)-1] {
				if s.DSM == nil || *s.DSM > 1 {
					t.Fatalf("fixture: the obstruction is not at the endpoints alone: %+v", s)
				}
			}
			sv := r.Verdict.Surface
			if sv == nil || sv.State != "blocked" || sv.WorstAtM == nil || sv.WorstClearanceM == nil || sv.WorstF1Fraction != nil {
				t.Fatalf("surface verdict %+v, want blocked at an endpoint with a null F1 fraction", sv)
			}
			if sv.Reason != "antennaBelowSurface" || sv.Endpoint != c.wantEndpoint {
				t.Fatalf("surface reason %q at %q, want antennaBelowSurface at %q", sv.Reason, sv.Endpoint, c.wantEndpoint)
			}
			near(t, "surface worstAtM", *sv.WorstAtM, float64(c.endpoint)*r.DistanceM, 0.01)
			near(t, "surface worstClearanceM", *sv.WorstClearanceM, *ep.GroundM+ep.AntennaM-*end.DSM, 0.01)

			// The DTM verdict is untouched: identical to the terrain-only profile.
			dtmOnly := decodeProfile(t, env.get(q+"&model=dtm"))
			r.Verdict.Surface = nil
			if !reflect.DeepEqual(r.Verdict, dtmOnly.Verdict) || r.Verdict.State != "clear" {
				t.Fatalf("deciding verdict changed: %+v, terrain-only %+v", r.Verdict, dtmOnly.Verdict)
			}
		})
	}
}

// #745 review F1, with the reviewer's numbers: 1 km flat, DSM 200 m at
// mid-path (about −190 m through the line of sight) and 10.01 m at A
// (−1 cm against a 10 m antenna). The worst point is the mid-path
// obstruction; the antenna below the surface at A is the reason. Without
// the endpoint obstruction there is no reason at all.
func TestTerrainProfile_InteriorObstructionOutranksEndpoint(t *testing.T) {
	x, y := dhm.ToUTM32(56.1629, 10.2039)
	la, lo := dhm.FromUTM32(x+1000, y)
	a, b := dhm.LatLon{Lat: 56.1629, Lon: 10.2039}, dhm.LatLon{Lat: la, Lon: lo}
	plan := dhm.PlanProfile(a, b, dhm.PlanOptions{MaxSamples: 3200, ChunkM: 2500})
	n := len(plan.E)
	if n%2 == 0 {
		t.Fatalf("fixture: %d samples have no mid-path sample", n)
	}
	p := newTerrainProfiler(dhm.ProfileConfig{Enabled: true}, nil, terrainProfilerDeps{src: newFakeElevation(t, flatElev(0))})
	q := terrainQuery{both: true, ha: floatp(10), hb: floatp(10)}
	for _, atA := range []bool{true, false} {
		e := terrainElevation{dtm: make([]float32, n), dsm: make([]float32, n)}
		e.dsm[n/2] = 200
		if atA {
			e.dsm[0] = 10.01
		}
		r := p.buildResponse(q, [2]terrainEnd{{pt: a, kind: "point"}, {pt: b, kind: "point"}}, a, b, &plan, e)
		sv := r.Verdict.Surface
		if sv == nil || sv.State != "blocked" || sv.WorstAtM == nil || sv.WorstClearanceM == nil || sv.WorstF1Fraction == nil {
			t.Fatalf("A below %t: surface verdict %+v, want blocked at an interior point with an F1 fraction", atA, sv)
		}
		near(t, "surface worstAtM", *sv.WorstAtM, r.DistanceM/2, 0.01)
		near(t, "surface worstClearanceM", *sv.WorstClearanceM, 10-200-dhm.EarthBulge(r.DistanceM/2, r.DistanceM/2, dhm.DefaultKFactor), 0.01)
		want := [2]string{}
		if atA {
			want = [2]string{"antennaBelowSurface", "A"}
		}
		if got := [2]string{sv.Reason, sv.Endpoint}; got != want {
			t.Fatalf("A below %t: reason %q at %q, want %q at %q", atA, got[0], got[1], want[0], want[1])
		}
		if r.Verdict.State != "clear" {
			t.Fatalf("the deciding DTM verdict is %q on flat ground", r.Verdict.State)
		}
	}
}

// #745 review F1, the chart: the worst DSM sample is a keep index, so a
// one-sample peak survives decimation even with an antenna below the
// surface at A. While the endpoint was the worst point, its keep index
// was dropped (endpoints are always in) and most peak positions were
// thinned out of the response.
func TestTerrainProfile_DecimationKeepsInteriorPeakWithEndpointBelow(t *testing.T) {
	a, b := dhm.LatLon{Lat: 56, Lon: 8.6}, dhm.LatLon{Lat: 56, Lon: 9.85}
	plan := dhm.PlanProfile(a, b, dhm.PlanOptions{MaxSamples: 3200, ChunkM: 2500})
	n := len(plan.E)
	if n <= terrainMaxReturned {
		t.Fatalf("fixture: %d samples need no decimation", n)
	}
	p := newTerrainProfiler(dhm.ProfileConfig{Enabled: true}, nil, terrainProfilerDeps{src: newFakeElevation(t, flatElev(0))})
	ends := [2]terrainEnd{{pt: a, kind: "point"}, {pt: b, kind: "point"}}
	lost := 0
	for peak := 3; peak < n-3; peak += 7 {
		e := terrainElevation{dtm: make([]float32, n), dsm: make([]float32, n)}
		e.dsm[0], e.dsm[peak] = 50, 400 // the point default antenna is 2 m
		r := p.buildResponse(terrainQuery{both: true}, ends, a, b, &plan, e)
		if sv := r.Verdict.Surface; sv == nil || sv.Reason != "antennaBelowSurface" || sv.Endpoint != "A" {
			t.Fatalf("peak %d: surface %+v, want the antenna below the surface at A", peak, sv)
		}
		found := false
		for _, s := range r.Samples {
			found = found || int(math.Round(s.D/r.StepM)) == peak
		}
		if !found {
			lost++
		}
	}
	if lost > 0 {
		t.Fatalf("the interior DSM peak was thinned out at %d positions", lost)
	}
}

// modelSplit serves DTM and DSM from different sources.
type modelSplit struct{ dtm, dsm dhm.ElevationSource }

func (m modelSplit) FetchRaster(ctx context.Context, r dhm.RasterRequest) (dhm.Grid, error) {
	if r.Model == dhm.ModelSurface {
		return m.dsm.FetchRaster(ctx, r)
	}
	return m.dtm.FetchRaster(ctx, r)
}

func TestTerrainProfile_DecimationKeepsWorstSample(t *testing.T) {
	e0, _ := dhm.ToUTM32(56.0, 8.6)
	spike := e0 + 51237
	src := newFakeElevation(t, func(e, n float64) float32 {
		if math.Abs(e-spike) < 30 {
			return 40
		}
		return 0
	})
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	resp := decodeProfile(t, env.get(pathQuery("56.00000,8.60000", offsetPt(56.0, 8.6, 79000, 0), "model=dtm")))
	if resp.SampleCount < 3000 || len(resp.Samples) > 1024 {
		t.Fatalf("computed %d, returned %d", resp.SampleCount, len(resp.Samples))
	}
	if resp.Verdict.WorstAtM == nil {
		t.Fatal("no worst sample")
	}
	found := false
	for _, s := range resp.Samples {
		found = found || s.D == *resp.Verdict.WorstAtM
	}
	if !found {
		t.Fatalf("worst sample at %v m dropped by decimation", *resp.Verdict.WorstAtM)
	}
	near(t, "worst near the spike", *resp.Verdict.WorstAtM, 51237, 60)
}

// #745: decimation must not hide a gap. A gap is hidden when two returned
// samples that are neighbours in the response both have a value while a
// sample between them is missing: the chart then draws a line across it.
// assertGapsVisible checks that for one series, and also that each gap
// whose edges fit (exact) starts and ends where the data does.
func assertGapsVisible(t *testing.T, name string, r TerrainProfileResponse, series []float32, value func(TerrainSample) *float64, exact bool) {
	t.Helper()
	if len(r.Samples) > terrainMaxReturned {
		t.Fatalf("%s: %d samples returned, more than %d", name, len(r.Samples), terrainMaxReturned)
	}
	returned := map[int]bool{}
	prev, prevValid := -1, false
	for j, s := range r.Samples {
		i := int(math.Round(s.D / r.StepM))
		if j > 0 && i <= prev {
			t.Fatalf("%s: samples not strictly increasing at %d", name, j)
		}
		if math.IsNaN(float64(series[i])) != (value(s) == nil) {
			t.Fatalf("%s: sample %d (index %d) misreports its value", name, j, i)
		}
		valid := value(s) != nil
		if valid && prevValid {
			for k := prev + 1; k < i; k++ {
				if math.IsNaN(float64(series[k])) {
					t.Fatalf("%s: missing sample %d hidden between returned samples %d and %d", name, k, prev, i)
				}
			}
		}
		returned[i] = true
		prev, prevValid = i, valid
	}
	if !exact {
		return
	}
	for i := 0; i < len(series); i++ {
		if !math.IsNaN(float64(series[i])) {
			continue
		}
		j := i
		for j+1 < len(series) && math.IsNaN(float64(series[j+1])) {
			j++
		}
		for _, k := range []int{i - 1, i, j, j + 1} {
			if k >= 0 && k < len(series) && !returned[k] {
				t.Fatalf("%s: edge %d of the gap %d–%d not returned", name, k, i, j)
			}
		}
		i = j
	}
}

func TestTerrainProfile_DecimationKeepsGaps(t *testing.T) {
	a := dhm.LatLon{Lat: 56, Lon: 8.6}
	nan := float32(math.NaN())
	for _, c := range []struct {
		name  string
		lonB  float64
		both  bool
		exact bool
		dtm   func(i, n int) bool // missing DTM sample
		dsm   func(i, n int) bool // missing DSM sample; nil: model=dtm
	}{
		// The reviewer's case: evenly thinning about 2000 samples to 1024
		// skips index 1.
		{name: "one sample next to A", lonB: 9.404, exact: true, dtm: func(i, n int) bool { return i == 1 }},
		{name: "a run and one next to B", lonB: 9.404, exact: true, dtm: func(i, n int) bool { return (i >= 700 && i <= 705) || i == n-2 }},
		{name: "DSM only", lonB: 9.404, both: true, exact: true,
			dtm: func(i, n int) bool { return false }, dsm: func(i, n int) bool { return i == 1501 || i == 3 }},
		{name: "both models, different gaps", lonB: 9.404, both: true, exact: true,
			dtm: func(i, n int) bool { return i >= 900 && i <= 903 }, dsm: func(i, n int) bool { return i >= 902 && i <= 907 }},
		// More gaps than the edges fit: every gap still shows.
		{name: "300 single gaps", lonB: 9.85, dtm: func(i, n int) bool { return i%7 == 3 && i < 2100 }},
		// More gaps than samples returned: none may hide behind a line.
		{name: "every other sample", lonB: 9.85, both: true,
			dtm: func(i, n int) bool { return i%2 == 1 }, dsm: func(i, n int) bool { return i%2 == 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := dhm.LatLon{Lat: 56, Lon: c.lonB}
			plan := dhm.PlanProfile(a, b, dhm.PlanOptions{MaxSamples: 3200, ChunkM: 2500})
			n := len(plan.E)
			if n <= terrainMaxReturned {
				t.Fatalf("fixture: %d samples need no decimation", n)
			}
			e := terrainElevation{dtm: make([]float32, n)}
			if c.dsm != nil {
				e.dsm = make([]float32, n)
			}
			for i := 0; i < n; i++ {
				if c.dtm(i, n) {
					e.dtm[i] = nan
				}
				if c.dsm != nil && c.dsm(i, n) {
					e.dsm[i] = nan
				}
			}
			p := newTerrainProfiler(dhm.ProfileConfig{Enabled: true}, nil, terrainProfilerDeps{src: newFakeElevation(t, flatElev(0))})
			r := p.buildResponse(terrainQuery{both: c.both}, [2]terrainEnd{{pt: a, kind: "point"}, {pt: b, kind: "point"}}, a, b, &plan, e)
			if len(r.Samples) == 0 || len(r.Samples) > terrainMaxReturned {
				t.Fatalf("%d of %d samples returned", len(r.Samples), n)
			}
			assertGapsVisible(t, "dtm", r, e.dtm, func(s TerrainSample) *float64 { return s.DTM }, c.exact)
			if e.dsm != nil {
				assertGapsVisible(t, "dsm", r, e.dsm, func(s TerrainSample) *float64 { return s.DSM }, c.exact)
			}
		})
	}
}

// terrainGapKeep with less room than gaps: bounded, and never a division
// by zero or a negative allocation.
func TestTerrainGapKeepTinyRoom(t *testing.T) {
	nan := float32(math.NaN())
	s := []float32{0, nan, 0, nan, 0, nan, 0}
	if got := terrainGapKeep(0, s); len(got) != 0 {
		t.Fatalf("room 0: %v", got)
	}
	if got := terrainGapKeep(1, s); len(got) != 1 || got[0] != 1 {
		t.Fatalf("room 1: %v", got)
	}
	if got := terrainGapKeep(2, s, nil); len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("room 2: %v", got)
	}
	if got := terrainGapKeep(3, s, nil); !reflect.DeepEqual(got, []int{1, 3, 5, 1, 0, 2, 3, 2, 4, 5, 4, 6}) {
		t.Fatalf("room 3, all gaps fit: %v", got)
	}
	// A run of missing samples is one gap: its interior takes no room.
	run := []float32{0, nan, nan, nan, 0}
	if got := terrainGapKeep(1, run); !reflect.DeepEqual(got, []int{1, 3, 0, 4}) {
		t.Fatalf("one run, room 1: %v", got)
	}
}

// BenchmarkTerrainBuildResponse is the per-request work after the
// elevations are in (cache hits included) at the 3200-sample maximum, with
// both models and gaps in each: coverage, physics, both assessments and
// the gap-keeping decimation to 1024 samples (#745).
func BenchmarkTerrainBuildResponse(b *testing.B) {
	pa, pb := dhm.LatLon{Lat: 56, Lon: 8.6}, dhm.LatLon{Lat: 56, Lon: 9.85}
	plan := dhm.PlanProfile(pa, pb, dhm.PlanOptions{MaxSamples: 3200, ChunkM: 2500})
	n := len(plan.E)
	e := terrainElevation{dtm: make([]float32, n), dsm: make([]float32, n)}
	for i := 0; i < n; i++ {
		e.dtm[i], e.dsm[i] = float32(i%37), float32(i%37+5)
		if i%80 < 5 {
			e.dtm[i] = float32(math.NaN())
		}
		if i%160 == 100 {
			e.dsm[i] = float32(math.NaN())
		}
	}
	p := newTerrainProfiler(dhm.ProfileConfig{Enabled: true}, nil, terrainProfilerDeps{src: newFakeElevation(nil, flatElev(0))})
	ends := [2]terrainEnd{{pt: pa, kind: "point"}, {pt: pb, kind: "point"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := p.buildResponse(terrainQuery{both: true}, ends, pa, pb, &plan, e); len(r.Samples) == 0 || len(r.Samples) > terrainMaxReturned {
			b.Fatalf("%d samples", len(r.Samples))
		}
	}
}

// ── Frequency ──────────────────────────────────────────────────────────────

func TestModalObserverFreq(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	stale := now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	obs := func(radio, seen string) Observer { return Observer{Radio: &radio, LastSeen: &seen} }
	list := []Observer{
		obs("910.5250244,62.5,7,5", recent),
		obs("910.525,62.5,7,5\r", recent),
		obs("910.525024,62.5,7,5", recent),
		obs("869.618,62.5,8,5", recent),
		obs("869.618,62.5,8,5", recent),
		obs("", recent),
		obs("abc,62.5", recent),
		obs("NaN,62.5", recent),
		obs("Inf", recent),
		obs("1e9,1", recent),
		obs("-5,1", recent),
		obs("0x1p9,1", recent),
		// A plain decimal outside 150-2500 MHz, more often than the answer.
		obs("5000.0,62.5", recent),
		obs("5000.0,62.5", recent),
		obs("5000.0,62.5", recent),
		obs("5000.0,62.5", recent),
		obs("12.5", recent),
		obs("433.175,62.5", stale),
		obs("433.175,62.5", stale),
		obs("433.175,62.5", stale),
		obs("433.175,62.5", "garbage"),
		{Radio: nil, LastSeen: &recent},
	}
	f, ok := modalObserverFreq(list, now)
	if !ok || f != 910.525 {
		t.Fatalf("modal %v %v, want 910.525", f, ok)
	}
	if _, ok := modalObserverFreq([]Observer{obs("", recent), obs("junk", recent)}, now); ok {
		t.Fatal("junk-only observers gave a frequency")
	}
	// A tie goes to the lower frequency, so the result is deterministic.
	if f, _ := modalObserverFreq([]Observer{obs("915.0", recent), obs("869.525", recent)}, now); f != 869.525 {
		t.Fatalf("tie: %v", f)
	}
}

func TestTerrainProfile_FrequencyPrecedence(t *testing.T) {
	q := pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 3000, 0))
	radio := func(r string) Observer {
		seen := time.Now().UTC().Format(time.RFC3339)
		return Observer{Radio: &r, LastSeen: &seen}
	}
	for _, c := range []struct {
		name   string
		cfg    *RadioConfig
		obs    []Observer
		extra  string
		want   float64
		source string
	}{
		{"firmware", nil, nil, "", 869.618, "firmware"},
		{"observers", nil, []Observer{radio("910.5250244,62.5,7,5"), radio("910.525,62.5,7,5\r")}, "", 910.525, "observers"},
		{"config beats observers", &RadioConfig{DefaultFreqMHz: floatp(868.1)}, []Observer{radio("910.525")}, "", 868.1, "config"},
		{"junk config ignored", &RadioConfig{DefaultFreqMHz: floatp(5)}, []Observer{radio("910.525")}, "", 910.525, "observers"},
		{"request beats all", &RadioConfig{DefaultFreqMHz: floatp(868.1)}, []Observer{radio("910.525")}, "freq=433.5", 433.5, "request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTerrainEnv(t, newFakeElevation(t, flatElev(0)), dhm.ProfileConfig{}, c.cfg)
			obs := c.obs
			env.p.observers = func() ([]Observer, error) { return obs, nil }
			qq := q
			if c.extra != "" {
				qq += "&" + c.extra
			}
			p := decodeProfile(t, env.get(qq)).Physics
			if p.FreqMHz != c.want || p.FreqSource != c.source {
				t.Fatalf("freq %v (%s), want %v (%s)", p.FreqMHz, p.FreqSource, c.want, c.source)
			}
			near(t, "lambda", p.LambdaM, dhm.Wavelength(c.want), 1e-6)
		})
	}
	// The observer lookup is cached, not repeated per request.
	env := newTerrainEnv(t, newFakeElevation(t, flatElev(0)), dhm.ProfileConfig{}, nil)
	var reads atomic.Int64
	env.p.observers = func() ([]Observer, error) { reads.Add(1); return nil, nil }
	for i := 0; i < 5; i++ {
		decodeProfile(t, env.get(q))
	}
	if reads.Load() != 1 {
		t.Fatalf("observers read %d times for 5 requests", reads.Load())
	}
}

func TestTerrainProfile_KFactor(t *testing.T) {
	q := pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 3000, 0))
	env := newTerrainEnv(t, newFakeElevation(t, flatElev(0)), dhm.ProfileConfig{}, &RadioConfig{KFactor: floatp(1)})
	if p := decodeProfile(t, env.get(q)).Physics; p.K != 1 || p.KSource != "config" {
		t.Fatalf("config k: %+v", p)
	}
	if p := decodeProfile(t, env.get(q+"&k=0.6")).Physics; p.K != 0.6 || p.KSource != "request" {
		t.Fatalf("request k: %+v", p)
	}
	env = newTerrainEnv(t, newFakeElevation(t, flatElev(0)), dhm.ProfileConfig{}, &RadioConfig{KFactor: floatp(-3)})
	if p := decodeProfile(t, env.get(q)).Physics; p.KSource != "default" {
		t.Fatalf("junk config k used: %+v", p)
	}
}

// ── Antenna heights ────────────────────────────────────────────────────────

func TestTerrainProfile_AntennaHeights(t *testing.T) {
	pk := func(c byte) string { return strings.Repeat(string(c), 64) }
	nodes := map[string]string{"repeater": pk('a'), "room": pk('b'), "companion": pk('c'), "sensor": pk('d')}
	b := offsetPt(56.1629, 10.2039, 4000, 0)
	radio := &RadioConfig{AntennaHeightsM: map[string]float64{
		"companion": 3,  // operator role default
		pk('d'):     18, // operator-known mast
		"point":     1.5,
		"bogus":     7,   // ignored with a warning
		pk('e'):     500, // out of range: ignored
	}}
	for _, withCfg := range []bool{false, true} {
		var cfg *RadioConfig
		if withCfg {
			cfg = radio
		}
		env := newTerrainEnv(t, newFakeElevation(t, flatElev(0)), dhm.ProfileConfig{}, cfg)
		for role, key := range nodes {
			env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
				VALUES (?, ?, ?, 56.1629, 10.2039, datetime('now'), datetime('now'), 1)`, key, "n-"+role, role)
		}
		env.srv.store.InvalidateNodeCache()
		want := map[string]struct {
			h      float64
			source string
		}{
			"repeater": {10, "role-default"}, "room": {5, "role-default"},
			"companion": {2, "role-default"}, "sensor": {2, "role-default"},
		}
		if withCfg {
			want["companion"] = struct {
				h      float64
				source string
			}{3, "operator-role"}
			want["sensor"] = struct {
				h      float64
				source string
			}{18, "operator-node"}
		}
		for role, key := range nodes {
			resp := decodeProfile(t, env.get(pathQuery(key, b)))
			if resp.A.Kind != "node" || resp.A.Role != role || resp.A.AntennaM != want[role].h || resp.A.AntennaSource != want[role].source {
				t.Fatalf("cfg=%v %s: endpoint %+v, want %+v", withCfg, role, resp.A, want[role])
			}
			if resp.A.Assumed != (want[role].source != "operator-node") {
				t.Fatalf("cfg=%v %s: assumed %v", withCfg, role, resp.A.Assumed)
			}
			if strings.Contains(env.get(pathQuery(key, b)).Body.String(), key) {
				t.Fatal("public key echoed")
			}
			// A URL override wins and is not an assumption.
			resp = decodeProfile(t, env.get(pathQuery(key, b, "ha=7.5")))
			if resp.A.AntennaM != 7.5 || resp.A.AntennaSource != "request" || resp.A.Assumed {
				t.Fatalf("override: %+v", resp.A)
			}
		}
		resp := decodeProfile(t, env.get(pathQuery(aarhusPt, b)))
		wantPoint, src := 2.0, "point-default"
		if withCfg {
			wantPoint, src = 1.5, "operator-role"
		}
		if resp.B.Kind != "point" || resp.B.AntennaM != wantPoint || resp.B.AntennaSource != src || !resp.B.Assumed {
			t.Fatalf("cfg=%v free point: %+v", withCfg, resp.B)
		}
		if withCfg && !strings.Contains(env.logs.String(), "antennaHeightsM") {
			t.Fatalf("bad antennaHeightsM entries not warned: %q", env.logs.String())
		}
	}
}

func TestTerrainProfile_NodeLookupIs404WhenHiddenOrUnplaced(t *testing.T) {
	env := newTerrainEnv(t, newFakeElevation(t, flatElev(0)), dhm.ProfileConfig{}, nil)
	placed, unplaced, hidden, private := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("5", 64)
	env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, 'p', 'repeater', 56.1, 10.1, datetime('now'), datetime('now'), 1),
		       (?, 'u', 'repeater', NULL, NULL, datetime('now'), datetime('now'), 1),
		       (?, 'h', 'repeater', 56.2, 10.2, datetime('now'), datetime('now'), 1),
		       (?, 'private-mast', 'repeater', 56.3, 10.3, datetime('now'), datetime('now'), 1)`, placed, unplaced, hidden, private)
	env.srv.store.InvalidateNodeCache()
	env.srv.cfg.SetNodeBlacklist([]string{hidden})
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private"})
	b := offsetPt(56.1629, 10.2039, 4000, 0)
	decodeProfile(t, env.get(pathQuery(placed, b)))
	decodeProfile(t, env.get(pathQuery(strings.ToUpper(placed), b)))
	for _, pk := range []string{unplaced, hidden, private, strings.Repeat("4", 64)} {
		w := env.get(pathQuery(pk, b))
		wantError(t, w, 404, "node not found")
		if strings.Contains(w.Body.String(), pk) {
			t.Fatal("pubkey echoed")
		}
	}
}

// ── Perf proof ─────────────────────────────────────────────────────────────

// TestTerrainProfile_ColdWarmLatency measures a 20 km `both` profile at the
// worst bearing through the real WCS client and decoder against a fake
// upstream serving DEFLATE GeoTIFFs: cold (16 upstream rasters) and warm
// (cache hit, 0 upstream calls). Run with -v for the numbers.
func TestTerrainProfile_ColdWarmLatency(t *testing.T) {
	up := wcsUpstream(t, tiffAnswer)
	env, _ := newWCSEnv(t, up, nil)
	q := pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 20000/math.Sqrt2, 20000/math.Sqrt2))
	start := time.Now()
	decodeProfile(t, env.get(q))
	cold := time.Since(start)
	hits := up.hits.Load()
	if hits != 16 {
		t.Fatalf("cold: %d upstream calls, want 16", hits)
	}
	const warmRuns = 50
	start = time.Now()
	for i := 0; i < warmRuns; i++ {
		decodeProfile(t, env.get(q))
	}
	warm := time.Since(start) / warmRuns
	if up.hits.Load() != hits {
		t.Fatalf("warm: %d upstream calls, want 0", up.hits.Load()-hits)
	}
	if warm >= cold {
		t.Fatalf("warm %v not faster than cold %v", warm, cold)
	}
	t.Logf("20 km both profile, 1025 samples, 16 rasters: cold %v, warm %v (mean of %d)", cold, warm, warmRuns)
}

// ── Config + docs ──────────────────────────────────────────────────────────

func TestTerrainProfile_SettingsDefaultsAndClamps(t *testing.T) {
	logs := &dhmLogSink{}
	s := resolveTerrainSettings(dhm.ProfileConfig{}, logs.logf)
	if s.maxPathM != 80000 || s.plan.MaxSamples != 3200 || s.plan.ChunkM != 2500 || s.cacheEntries != 256 ||
		s.cacheTTL != 24*time.Hour || s.upstreamTimeout != 10*time.Second || s.rateLimitPerMin != 10 || s.maxInFlight != 4 {
		t.Fatalf("defaults %+v", s)
	}
	s = resolveTerrainSettings(dhm.ProfileConfig{MaxPathKm: floatp(500), MaxSamples: intp(99999), ChunkKm: floatp(0),
		CacheEntries: intp(-1), CacheTTLHours: floatp(math.Inf(1)), UpstreamTimeoutMs: intp(60000), RateLimitPerMin: intp(0)}, logs.logf)
	if s.maxPathM != 80000 || s.plan.MaxSamples != 3200 || s.plan.ChunkM != 0 || s.cacheEntries != 256 ||
		s.cacheTTL != 24*time.Hour || s.upstreamTimeout != 10*time.Second || s.rateLimitPerMin != 0 {
		t.Fatalf("clamped %+v", s)
	}
	s = resolveTerrainSettings(dhm.ProfileConfig{RateLimitPerMin: intp(-1)}, logs.logf)
	if s.rateLimitPerMin != 10 {
		t.Fatalf("negative rateLimitPerMin → %d", s.rateLimitPerMin)
	}
	// maxInFlight is 1-16: 0 would refuse every uncached profile (#707).
	for v, want := range map[int]int{0: 4, 17: 4, 65: 4, 1: 1, 16: 16} {
		if got := resolveTerrainSettings(dhm.ProfileConfig{MaxInFlight: intp(v)}, logs.logf).maxInFlight; got != want {
			t.Fatalf("maxInFlight %d → %d, want %d", v, got, want)
		}
	}
	for _, k := range []string{"maxPathKm", "maxSamples", "cacheEntries", "cacheTTLHours", "upstreamTimeoutMs", "rateLimitPerMin", "maxInFlight"} {
		if !strings.Contains(logs.String(), k) {
			t.Fatalf("out-of-range %s not warned: %q", k, logs.String())
		}
	}
}

func TestTerrainProfile_ConfigLoadsAndStaysServerSide(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{"map":{"dhm":{"enabled":true,"token":"` + dhmTestToken + `","tileRateLimitPerMin":300,
		"profile":{"enabled":true,"maxPathKm":40,"chunkKm":5,"rateLimitPerMin":4,"maxInFlight":2}}},
		"radio":{"defaultFreqMHz":868.1,"kFactor":1,"antennaHeightsM":{"repeater":12}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.DHM.Profile
	if !pc.Enabled || *pc.MaxPathKm != 40 || *pc.ChunkKm != 5 || *pc.RateLimitPerMin != 4 || *cfg.DHM.TileRateLimitPerMin != 300 {
		t.Fatalf("profile config %+v", pc)
	}
	if cfg.Radio == nil || *cfg.Radio.DefaultFreqMHz != 868.1 || cfg.Radio.AntennaHeightsM["repeater"] != 12 {
		t.Fatalf("radio config %+v", cfg.Radio)
	}
	srv := NewServer(setupTestDB(t), cfg, NewHub())
	if srv.terrain == nil || srv.dhm == nil || srv.terrain.set.maxPathM != 40000 || cap(srv.terrain.admit) != 2 {
		t.Fatal("profile not wired from config")
	}
	if srv.dhm.limiter == nil || srv.terrain.limiter == nil {
		t.Fatal("rate limits not wired from config")
	}
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	for _, leak := range []string{dhmTestToken, "maxPathKm", "rateLimitPerMin", "antennaHeightsM"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("%q in /api/config/client", leak)
		}
	}
}

// The documented example values are the code defaults, and the example
// leaves the profile off.
func TestTerrainProfile_ExampleConfigMatchesDefaults(t *testing.T) {
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
	if cfg.DHM.Profile.Enabled || newTerrainProfilerFromConfig(cfg, newDHMProxy(dhm.Config{Enabled: true, Token: "t"}, defaultDHMProxyOptions()), nil) != nil {
		t.Fatal("example config enables the terrain profile")
	}
	if cfg.DHM.Profile.MaxPathKm == nil || cfg.DHM.TileRateLimitPerMin == nil {
		t.Fatal("example config does not document the profile limits")
	}
	logs := &dhmLogSink{}
	if got, want := resolveTerrainSettings(cfg.DHM.Profile, logs.logf), resolveTerrainSettings(dhm.ProfileConfig{}, logs.logf); got != want {
		t.Fatalf("example profile %+v, defaults %+v", got, want)
	}
	if got := dhmTileRateLimit(cfg.DHM, logs.logf); got != dhmTileRateLimitDefault {
		t.Fatalf("example tile limit %d, default %d", got, dhmTileRateLimitDefault)
	}
	if cfg.Radio == nil || cfg.Radio.DefaultFreqMHz != nil || cfg.Radio.KFactor != nil || len(cfg.Radio.AntennaHeightsM) != 0 {
		t.Fatalf("example radio %+v", cfg.Radio)
	}
	if logs.String() != "" {
		t.Fatalf("example config warns: %q", logs.String())
	}
}

func TestTerrainProfile_OpenAPI(t *testing.T) {
	m, ok := routeDescriptions()["GET /api/terrain/profile"]
	if !ok {
		t.Fatal("route not in OpenAPI")
	}
	have := map[string]bool{}
	for _, p := range m.QueryParams {
		have[p.Name] = p.Required
	}
	for _, p := range []string{"a", "b", "model", "ha", "hb", "k", "freq"} {
		if _, ok := have[p]; !ok {
			t.Fatalf("param %s undocumented", p)
		}
	}
	if !have["a"] || !have["b"] {
		t.Fatal("a and b not required")
	}
	tile := routeDescriptions()["GET /api/map/dhm/wms"]
	if len(tile.Errors) == 0 || tile.Errors[0].Status != "429" {
		t.Fatal("tile rate limit undocumented")
	}
	codes := map[string]bool{}
	for _, e := range m.Errors {
		codes[e.Status] = true
	}
	for _, c := range []string{"400", "404", "429", "502", "503", "504"} {
		if !codes[c] {
			t.Fatalf("error %s undocumented", c)
		}
	}
}

// ── #707: admission bound nits ─────────────────────────────────────────────

// fillAdmission occupies every admission slot, so the next uncached
// profile is refused with 503 without any upstream call or goroutine.
func fillAdmission(env *terrainEnv) {
	for i := 0; i < cap(env.p.admit); i++ {
		env.p.admit <- struct{}{}
	}
}

func busyQuery(i int) string {
	return pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 5000+float64(i), 0))
}

// #707 nit 1: the clamp's upper end is 4x the upstream budget (4), not 64.
// At 64 a blocked source held ~1280 goroutines above baseline. The value
// itself is pinned, so raising it is a deliberate, reviewed change.
func TestTerrainProfile_MaxInFlightClampIsPinned(t *testing.T) {
	if terrainMaxInFlightCap != 16 {
		t.Fatalf("terrainMaxInFlightCap = %d, want 16 (4x the upstream budget of 4)", terrainMaxInFlightCap)
	}
	logs := &dhmLogSink{}
	for v, want := range map[int]int{16: 16, 17: 4, 64: 4} {
		if got := resolveTerrainSettings(dhm.ProfileConfig{MaxInFlight: intp(v)}, logs.logf).maxInFlight; got != want {
			t.Fatalf("maxInFlight %d → %d, want %d", v, got, want)
		}
	}
}

// #707 nit 2: a 503 refusal does no upstream work, so it must not spend the
// client's per-IP budget. An obedient client retrying every Retry-After
// during a long refusal window must still get its fetch once a slot frees,
// not a 429. Default budget is 10/min; 40 refusals would exhaust it.
func TestTerrainProfile_AdmissionRefusalDoesNotChargePerIP(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	fillAdmission(env)
	client := func(r *http.Request) { r.RemoteAddr = "198.51.100.7:1000" }
	for i := 0; i < 40; i++ {
		w := env.get(busyQuery(0), client)
		wantError(t, w, http.StatusServiceUnavailable, "terrain profiles busy")
		if w.Header().Get("Retry-After") != "5" {
			t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
		}
	}
	for len(env.p.admit) > 0 {
		<-env.p.admit
	}
	// The budget is intact: the full 10 admitted fetches still succeed.
	for i := 0; i < 10; i++ {
		decodeProfile(t, env.get(busyQuery(i), client))
	}
	// ...and the budget still limits admitted fetches.
	wantError(t, env.get(busyQuery(99), client), http.StatusTooManyRequests, "rate limit exceeded")
}

// #707 nit 3: refusals are counted and logged throttled (first at once,
// then at most one line per interval carrying the count), so an operator
// sees the bound biting. The line names the setting, never the token.
func TestTerrainProfile_AdmissionRefusalsAreObservable(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{RateLimitPerMin: intp(0)}, nil)
	clock := time.Unix(1_700_000_000, 0)
	env.p.now = func() time.Time { return clock }
	fillAdmission(env)
	for i := 0; i < 30; i++ {
		wantError(t, env.get(busyQuery(i)), http.StatusServiceUnavailable, "terrain profiles busy")
	}
	if got := env.p.refused.Load(); got != 30 {
		t.Fatalf("refused counter %d, want 30", got)
	}
	lines := func() []string {
		var out []string
		for _, l := range strings.Split(env.logs.String(), "\n") {
			if strings.Contains(l, "admission") {
				out = append(out, l)
			}
		}
		return out
	}
	if got := lines(); len(got) != 1 {
		t.Fatalf("%d admission log lines for 30 refusals in one interval, want 1: %q", len(got), got)
	}
	clock = clock.Add(terrainRefusalLogEvery + time.Second)
	wantError(t, env.get(busyQuery(31)), http.StatusServiceUnavailable, "terrain profiles busy")
	got := lines()
	// 29 refused since the first line, plus this one.
	if len(got) != 2 || !strings.Contains(got[1], "30 request") {
		t.Fatalf("second report %q", got)
	}
	if !strings.Contains(got[0], "maxInFlight") || strings.Contains(strings.ToLower(env.logs.String()), "token") {
		t.Fatalf("log line must name maxInFlight and never the token: %q", env.logs.String())
	}
}

// #710 gap 2: an upstream failure (502, 504) did the upstream work, so it
// stays charged. Refunding it would let one client spend a full upstream
// timeout per request for free. With a budget of 3, three failing fetches
// exhaust it and the 4th is 429, not another upstream call.
func TestTerrainProfile_UpstreamFailuresStayCharged(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"502", errors.New("upstream broke"), http.StatusBadGateway, "terrain data unavailable"},
		{"504", context.DeadlineExceeded, http.StatusGatewayTimeout, "terrain data timed out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := newFakeElevation(t, flatElev(0))
			src.hook = func(context.Context, dhm.RasterRequest) error { return c.err }
			env := newTerrainEnv(t, src, dhm.ProfileConfig{RateLimitPerMin: intp(3)}, nil)
			client := func(r *http.Request) { r.RemoteAddr = "198.51.100.8:1000" }
			for i := 0; i < 3; i++ {
				wantError(t, env.get(busyQuery(i), client), c.code, c.msg)
			}
			calls := src.calls.Load()
			wantError(t, env.get(busyQuery(3), client), http.StatusTooManyRequests, "rate limit exceeded")
			if src.calls.Load() != calls {
				t.Fatal("a rate-limited request reached upstream")
			}
		})
	}
}

// #710 gap 3: the refusal log line names the configured maxInFlight, not a
// constant. 7 is neither the default (4) nor the clamp (16).
func TestTerrainProfile_AdmissionLogNamesConfiguredMaxInFlight(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{RateLimitPerMin: intp(0), MaxInFlight: intp(7)}, nil)
	if cap(env.p.admit) != 7 {
		t.Fatalf("admit capacity %d, want 7", cap(env.p.admit))
	}
	fillAdmission(env)
	wantError(t, env.get(busyQuery(0)), http.StatusServiceUnavailable, "terrain profiles busy")
	if !strings.Contains(env.logs.String(), "maxInFlight (7) fetches") {
		t.Fatalf("log line does not carry the configured value 7: %q", env.logs.String())
	}
}

// #707 nit 4: the leader is refused, so its joiners are refused with it.
func TestTerrainProfile_OpenAPI503DoesNotOverstateSharing(t *testing.T) {
	_, router := setupTestServer(t)
	spec := buildOpenAPISpec(router, "test")
	raw, _ := json.Marshal(spec)
	text := string(raw)
	if strings.Contains(text, "share it and are not refused") {
		t.Fatal("503 text still claims joiners are never refused")
	}
	if !strings.Contains(text, "refused with it") {
		t.Fatal("503 text must say joiners of a refused fetch are refused with it")
	}
}
