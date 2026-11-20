package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
)

// #941 M1 — GET /api/terrain/coverage and its raster. Every test runs on a
// fake ElevationSource, a fake WCS upstream (httptest) or a fake foreign
// fill; none reaches Dataforsyningen or AWS, and no real token exists here.

type coverageEnv struct {
	srv    *Server
	router *mux.Router
	c      *terrainCoverage
	logs   *dhmLogSink
}

// newCoverageEnv is a test server with the predicted reach enabled on src
// (the DHM WCS stand-in; nil for none) and cc.
func newCoverageEnv(t *testing.T, src dhm.ElevationSource, cc CoverageConfig, radio *RadioConfig, edit func(*terrainCoverageDeps)) *coverageEnv {
	t.Helper()
	srv, router := setupTestServer(t)
	logs := &dhmLogSink{}
	cc.Enabled = true
	if cc.RateLimitPerMin == nil {
		// Off unless a test is about it: most send many misses.
		cc.RateLimitPerMin = intp(0)
	}
	srv.cfg.Radio = radio
	d := terrainCoverageDeps{
		src:        src,
		logf:       logs.logf,
		logFailure: func(what string, err error) { logs.logf("[dhm] upstream %s failed (%s)", what, dhm.FailureReason(err)) },
		observers:  func() ([]Observer, error) { return nil, nil },
	}
	if edit != nil {
		edit(&d)
	}
	srv.coverage = newTerrainCoverage(cc, radio, d)
	if srv.coverage == nil {
		t.Fatal("newTerrainCoverage returned nil for an enabled reach with a source")
	}
	return &coverageEnv{srv: srv, router: router, c: srv.coverage, logs: logs}
}

func (e *coverageEnv) get(query string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/terrain/coverage?"+query, nil)
	for _, m := range mod {
		m(req)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *coverageEnv) raster(path string, acceptGzip bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if acceptGzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func reachQuery(at string, extra ...string) string {
	q := "at=" + url.QueryEscape(at)
	for _, x := range extra {
		q += "&" + x
	}
	return q
}

func decodeCoverage(t *testing.T, w *httptest.ResponseRecorder) CoverageResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp CoverageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return resp
}

// hillyElev is rolling terrain around Aarhus with a ridge 3 km east.
func hillyElev(e, n float64) float32 {
	e0, n0 := dhm.ToUTM32(56.1629, 10.2039)
	x, y := (e-e0)/1000, (n-n0)/1000
	v := 30 + 15*math.Sin(x/2.3)*math.Cos(y/3.1) + 6*math.Sin(x/0.7+y/1.3)
	if math.Abs(x-3) < 0.06 {
		v += 70
	}
	return float32(math.Max(0, v))
}

// ── Enablement ─────────────────────────────────────────────────────────────

func TestCoverage941_DisabledIs404(t *testing.T) {
	srv, router := setupTestServer(t)
	for _, p := range []string{"/api/terrain/coverage?at=56.1,10.2", "/api/terrain/coverage/raster?key=" + strings.Repeat("a", 32)} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		wantError(t, w, 404, "not found")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if strings.Contains(w.Body.String(), `"coverage"`) {
		t.Fatalf("client config offers the reach while it is off: %s", w.Body.String())
	}
	if srv.coverage != nil {
		t.Fatal("coverage built without config")
	}
}

// Without DHM and without the foreign fill there is no terrain to use:
// the reach stays off, and the operator is told why.
func TestCoverage941_OffWithoutASource(t *testing.T) {
	logs := &dhmLogSink{}
	if c := newTerrainCoverage(CoverageConfig{Enabled: true}, nil, terrainCoverageDeps{logf: logs.logf}); c != nil {
		t.Fatal("reach on with no terrain source")
	}
	if !strings.Contains(logs.String(), "map.coverage") || !strings.Contains(logs.String(), "foreignFill") {
		t.Fatalf("not explained: %q", logs.String())
	}
	if c := newTerrainCoverage(CoverageConfig{}, nil, terrainCoverageDeps{src: newFakeElevation(t, flatElev(0))}); c != nil {
		t.Fatal("reach on while map.coverage.enabled is false")
	}
}

// ── Grammar ────────────────────────────────────────────────────────────────

func TestCoverage941_RejectsJunkWithoutEcho(t *testing.T) {
	src := newFakeElevation(t, flatElev(5))
	env := newCoverageEnv(t, src, CoverageConfig{}, nil, nil)
	bad := []string{
		"",
		"at=",
		"at=56.1629",
		"at=56.1629,10.2039,1",
		"at=1e1,10",
		"at=0x38,10",
		"at=NaN,10",
		"at=91,10",
		"at=56.1,181",
		"at=" + strings.Repeat("g", 64),
		"at=56.1,10.2&at=56.2,10.2",
		"at=56.1,10.2&ha=101",
		"at=56.1,10.2&ha=-1",
		"at=56.1,10.2&hr=1e2",
		"at=56.1,10.2&r=0",
		"at=56.1,10.2&r=0.5",
		"at=56.1,10.2&r=abc",
		"at=56.1,10.2&freq=100",
		"at=56.1,10.2&freq=Infinity",
		// Power, gains and sensitivity never reach the server: the
		// browser applies them to the cached loss.
		"at=56.1,10.2&p=22",
		"at=56.1,10.2&g=3",
		"at=56.1,10.2&s=-130",
		"at=56.1,10.2&k=1.33",
		"at=56.1,10.2&evil=<script>",
	}
	for _, q := range bad {
		w := env.get(q)
		wantError(t, w, 400, "invalid terrain request")
		if strings.Contains(w.Body.String(), "script") || strings.Contains(w.Body.String(), "56.1") {
			t.Fatalf("%q echoed: %s", q, w.Body.String())
		}
	}
	// A radius over the limit names the limit, like a profile path.
	w := env.get(reachQuery("56.1629,10.2039", "r=41"))
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"radius too large"`) || !strings.Contains(w.Body.String(), `"maxRadiusKm":40`) {
		t.Fatalf("over-radius: %d %s", w.Code, w.Body.String())
	}
	// Outside every source: refused before any upstream call.
	wantError(t, env.get(reachQuery("37.77,-122.42")), 400, "point not covered")
	wantError(t, env.get(reachQuery("55.6,13.2")), 400, "point not covered") // Skåne: foreign land
	if src.calls.Load() != 0 {
		t.Fatalf("%d upstream calls for refused requests", src.calls.Load())
	}
}

// ── Compute, serve, cache ──────────────────────────────────────────────────

func TestCoverage941_ComputesAndServesTheRaster(t *testing.T) {
	src := newFakeElevation(t, hillyElev)
	env := newCoverageEnv(t, src, CoverageConfig{}, nil, nil)
	w := env.get(reachQuery("56.1629,10.2039", "r=8", "ha=10", "hr=2"))
	if w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("cache %q", w.Header().Get("X-CoreScope-Cache"))
	}
	resp := decodeCoverage(t, w)
	if len(resp.Key) != 32 || resp.Raster.URL != "/api/terrain/coverage/raster?key="+resp.Key {
		t.Fatalf("key %q url %q", resp.Key, resp.Raster.URL)
	}
	if resp.Center.Kind != "point" || resp.Center.Lat != 56.1629 || resp.Center.Lon != 10.2039 || resp.Center.GroundM == nil {
		t.Fatalf("center %+v", resp.Center)
	}
	p := resp.Params
	if p.RadiusKm != 8 || p.ResolutionM != 30 || p.TxHeightM != 10 || p.TxHeightSource != "request" || p.RxHeightM != 2 ||
		p.FreqMHz != dhm.FirmwareFreqMHz || p.FreqSource != "firmware" || p.K != dhm.DefaultKFactor || p.Model != dhm.CoverageModel {
		t.Fatalf("params %+v", p)
	}
	r := resp.Raster
	if r.Encoding != "loss-u8" || r.LossBaseDb != dhm.CoverageLossBaseDb || r.LossStepDb != dhm.CoverageLossStepDb ||
		r.NoData != dhm.CoverageNoData || r.Width < 100 || r.Height < 100 || r.CoveredPct < 99 {
		t.Fatalf("raster %+v", r)
	}
	if r.Bounds[0][0] >= 56.1629 || r.Bounds[1][0] <= 56.1629 || r.Bounds[0][1] >= 10.2039 || r.Bounds[1][1] <= 10.2039 {
		t.Fatalf("bounds %v do not hold the centre", r.Bounds)
	}
	if len(resp.Sources) != 1 || resp.Sources[0].ID != "dhm-dtm" || resp.Attribution != dhm.AttributionDerived || resp.AttributionFill != "" {
		t.Fatalf("sources %+v attribution %q fill %q", resp.Sources, resp.Attribution, resp.AttributionFill)
	}

	// The raster, gzip as stored, and plain for a client that asks so.
	gz := env.raster(r.URL, true)
	if gz.Code != 200 || gz.Header().Get("Content-Encoding") != "gzip" || gz.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("raster %d %v", gz.Code, gz.Header())
	}
	if gz.Body.Len() != r.Bytes {
		t.Fatalf("raster %d bytes, response says %d", gz.Body.Len(), r.Bytes)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if len(raw) != r.Width*r.Height {
		t.Fatalf("raster %d bytes, want %d", len(raw), r.Width*r.Height)
	}
	plain := env.raster(r.URL, false)
	if plain.Code != 200 || plain.Header().Get("Content-Encoding") != "" || !bytes.Equal(plain.Body.Bytes(), raw) {
		t.Fatalf("plain raster %d %v", plain.Code, plain.Header())
	}
	// Somewhere the ridge shadows and somewhere is open.
	lo, hi := 255, 0
	for _, v := range raw {
		if v != dhm.CoverageNoData {
			lo, hi = min(lo, int(v)), max(hi, int(v))
		}
	}
	if hi-lo < 40 {
		t.Fatalf("loss spans only %d steps", hi-lo)
	}

	// A hit: no upstream call, the same answer.
	calls := src.calls.Load()
	w2 := env.get(reachQuery("56.1629,10.2039", "r=8", "ha=10", "hr=2"))
	if w2.Header().Get("X-CoreScope-Cache") != "hit" || src.calls.Load() != calls {
		t.Fatalf("second request: cache %q, %d new calls", w2.Header().Get("X-CoreScope-Cache"), src.calls.Load()-calls)
	}
	if !bytes.Equal(w.Body.Bytes(), w2.Body.Bytes()) {
		t.Fatal("a hit answers differently")
	}
	// Advert jitter under the 1e-4° snap is the same reach.
	if got := decodeCoverage(t, env.get(reachQuery("56.16292,10.20388", "r=8", "ha=10", "hr=2"))); got.Key != resp.Key {
		t.Fatal("jitter under the snap missed the cache")
	}
	// The raster endpoint serves cache hits only.
	wantError(t, env.raster("/api/terrain/coverage/raster?key="+strings.Repeat("0", 32), true), 404, "not found")
	for _, q := range []string{"", "key=abc", "key=" + strings.Repeat("G", 32), "key=" + resp.Key + "&x=1"} {
		wantError(t, env.raster("/api/terrain/coverage/raster?"+q, true), 400, "invalid terrain request")
	}
}

// TestCoverage941_UpstreamCallsWithinBudget: a cold 40 km reach fetches
// each planned tile once, at most two at a time (the sub-budget of the
// shared client); a second reach over the same area with other heights
// fetches nothing (the tile cache), and a cache hit nothing at all.
func TestCoverage941_UpstreamCallsWithinBudget(t *testing.T) {
	src := newFakeElevation(t, hillyElev)
	var inFlight, peak atomic.Int64
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		return nil
	}
	env := newCoverageEnv(t, src, CoverageConfig{}, nil, nil)
	e0, n0 := dhm.ToUTM32(56.1629, 10.2039)
	planned := 0
	for _, tl := range dhm.PlanCoverageTiles(e0, n0, 40000, 30) {
		if _, ok := tl.Raster(30); ok {
			planned++
		}
	}
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039")))
	cold := int(src.calls.Load())
	if cold == 0 || cold > planned || cold > 49 {
		t.Fatalf("cold reach: %d calls, %d tiles planned", cold, planned)
	}
	if peak.Load() > coverageUpstreamSlots || coverageUpstreamSlots != 2 {
		t.Fatalf("peak %d concurrent upstream calls, sub-budget %d", peak.Load(), coverageUpstreamSlots)
	}
	if src.model[dhm.ModelSurface] != 0 {
		t.Fatal("the reach fetched the surface model")
	}
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "ha=25", "hr=1.5")))
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039")))
	if got := int(src.calls.Load()); got != cold {
		t.Fatalf("warm reaches made %d more calls", got-cold)
	}
	t.Logf("cold 40 km reach: %d WCS rasters (%d tiles planned), peak concurrency %d", cold, planned, peak.Load())
}

// ── Cache key ──────────────────────────────────────────────────────────────

// Power, gains, cable loss and sensitivity are not in the cache key: they
// only shift the margin, which the browser applies.
func TestCoverage941_RadioBudgetIsNotInTheCacheKey(t *testing.T) {
	typ := reflect.TypeOf(coverageKey{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, banned := range []string{"power", "gain", "sens", "loss", "dbm", "dbi"} {
			if strings.Contains(name, banned) {
				t.Fatalf("cache key field %s carries the link budget", typ.Field(i).Name)
			}
		}
	}
	keys := map[string]bool{}
	for _, radio := range []*RadioConfig{nil, {TxPowerDbm: floatp(14), AntennaGainDbi: floatp(8), RxGainDbi: floatp(0), LossDb: floatp(3), SensitivityDbm: floatp(-120)}} {
		env := newCoverageEnv(t, newFakeElevation(t, flatElev(4)), CoverageConfig{}, radio, nil)
		resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5")))
		keys[resp.Key] = true
	}
	if len(keys) != 1 {
		t.Fatalf("the link budget changed the key: %v", keys)
	}
	// What does change the prediction changes the key.
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(4)), CoverageConfig{}, nil, nil)
	seen := map[string]string{}
	for _, q := range []string{"r=5", "r=6", "r=5&ha=12", "r=5&hr=3", "r=5&freq=433.5", "r=5&ha=12.5"} {
		k := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", q))).Key
		if prev, dup := seen[k]; dup {
			t.Fatalf("%q and %q share key %s", q, prev, k)
		}
		seen[k] = q
	}
	// Heights snap to 0.5 m, in the key and in the computation alike: 12.2
	// is 12, so a cached 12 m reach is exactly what 12.2 m would compute.
	snapped := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5&ha=12.2&hr=2.3")))
	if seen[snapped.Key] != "" || snapped.Params.TxHeightM != 12 || snapped.Params.RxHeightM != 2.5 {
		t.Fatalf("12.2/2.3 m: key seen as %q, computed at %v/%v m", seen[snapped.Key], snapped.Params.TxHeightM, snapped.Params.RxHeightM)
	}
	if k := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5&ha=12.2"))).Key; seen[k] != "r=5&ha=12" {
		t.Fatalf("12.2 m did not snap to 12 m")
	}
}

// ── Limits and failures ────────────────────────────────────────────────────

// Cache misses are charged to the client's budget; hits are not.
func TestCoverage941_PerIPLimitOnMissesOnly(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{RateLimitPerMin: intp(2)}, nil, nil)
	ip := func(r *http.Request) { r.RemoteAddr = "203.0.113.7:4000" }
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=4"), ip))
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5"), ip))
	w := env.get(reachQuery("56.1629,10.2039", "r=6"), ip)
	wantError(t, w, 429, "rate limit exceeded")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	for i := 0; i < 5; i++ {
		decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=4"), ip))
	}
	// Another client is not affected.
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=6"), func(r *http.Request) { r.RemoteAddr = "198.51.100.2:1" }))
}

// A full admission is 503 with Retry-After before any upstream call, and
// gives the per-IP charge back.
func TestCoverage941_AdmissionFullIs503(t *testing.T) {
	src := newFakeElevation(t, flatElev(3))
	env := newCoverageEnv(t, src, CoverageConfig{RateLimitPerMin: intp(1)}, nil, nil)
	release := fillSlots(env.c.admit)
	ip := func(r *http.Request) { r.RemoteAddr = "203.0.113.8:4000" }
	for i := 0; i < 3; i++ {
		w := env.get(reachQuery("56.1629,10.2039", fmt.Sprintf("r=%d", 4+i)), ip)
		wantError(t, w, 503, "predicted reach busy")
		if w.Header().Get("Retry-After") != "5" {
			t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
		}
	}
	if src.calls.Load() != 0 {
		t.Fatal("a refused reach went upstream")
	}
	release()
	// The refusals were refunded: the one-per-minute budget is intact.
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=4"), ip))
}

// The server-wide upstream budget (map.coverage.upstreamPerMin) refuses a
// cold reach with 503 and Retry-After once spent, without an upstream
// call, and refills with time.
func TestCoverage941_UpstreamPerMinuteBudget(t *testing.T) {
	src := newFakeElevation(t, flatElev(3))
	env := newCoverageEnv(t, src, CoverageConfig{UpstreamPerMin: intp(4)}, nil, nil)
	now := time.Unix(1_800_000_000, 0)
	env.c.now = func() time.Time { return now }
	env.c.budget.now = env.c.now
	// A full budget admits a reach larger than itself (12 tiles at 20 km)…
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=20")))
	calls := src.calls.Load()
	if calls <= 4 {
		t.Fatalf("expected a reach larger than the budget, got %d calls", calls)
	}
	// …and the next cold one waits until it has refilled.
	w := env.get(reachQuery("57.0488,9.9217", "r=5"))
	wantError(t, w, 503, "predicted reach busy")
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After %q", ra)
	}
	if src.calls.Load() != calls {
		t.Fatal("a refused reach went upstream")
	}
	// Hits and warm tiles cost nothing.
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=20")))
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=19")))
	now = now.Add(5 * time.Minute)
	decodeCoverage(t, env.get(reachQuery("57.0488,9.9217", "r=5")))
}

// A refusal by the spent upstream budget gives the per-IP charge back,
// like a full admission: with one reach per minute per IP, three
// refusals in a row stay 503 (not 429), and the IP's first reach after
// the refill is served.
func TestCoverage941_UpstreamBudgetRefusalRefundsPerIP(t *testing.T) {
	src := newFakeElevation(t, flatElev(3))
	env := newCoverageEnv(t, src, CoverageConfig{UpstreamPerMin: intp(4), RateLimitPerMin: intp(1)}, nil, nil)
	now := time.Unix(1_800_000_000, 0)
	env.c.now = func() time.Time { return now }
	env.c.budget.now = env.c.now
	// Another IP spends the budget with a reach larger than it.
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=20"), func(r *http.Request) { r.RemoteAddr = "198.51.100.3:1" }))
	calls := src.calls.Load()
	ip := func(r *http.Request) { r.RemoteAddr = "203.0.113.9:4000" }
	for i := 0; i < 3; i++ {
		w := env.get(reachQuery("57.0488,9.9217", fmt.Sprintf("r=%d", 5+i)), ip)
		wantError(t, w, 503, "predicted reach busy")
	}
	if src.calls.Load() != calls {
		t.Fatal("a refused reach went upstream")
	}
	// The budget refills; the per-IP limiter's minute has not passed.
	now = now.Add(5 * time.Minute)
	decodeCoverage(t, env.get(reachQuery("57.0488,9.9217", "r=5"), ip))
}

// coverageWCSEnv is the reach on the real WCS source and client, against up.
func coverageWCSEnv(t *testing.T, up *fakeUpstream, timeout time.Duration) *coverageEnv {
	t.Helper()
	logs := &dhmLogSink{}
	o := testDHMOptions(up.srv.URL+"/", logs).client
	o.Timeout = timeout
	client := dhm.NewClient(dhm.Config{Enabled: true, Token: dhmTestToken}, o)
	env := newCoverageEnv(t, dhm.NewWCSSource(client), CoverageConfig{}, nil, func(d *terrainCoverageDeps) {
		d.logf = logs.logf
		d.logFailure = client.LogFailure
	})
	env.logs = logs
	return env
}

// Upstream failures are 502, timeouts 504; neither carries upstream text,
// the token is never in a response or a log line, and nothing is cached.
func TestCoverage941_UpstreamFailuresAreClean(t *testing.T) {
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
		{"xml 500", func(w http.ResponseWriter, r *http.Request, _, _ int) {
			w.Header().Set("Content-Type", "application/vnd.ogc.se_xml")
			w.WriteHeader(500)
			w.Write([]byte("<ServiceException>boom " + r.URL.String() + " " + r.Header.Get("token") + "</ServiceException>"))
		}, 502, "terrain data unavailable", "status 500"},
		{"timeout", func(w http.ResponseWriter, r *http.Request, _, _ int) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, 504, "terrain data timed out", "timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := coverageWCSEnv(t, wcsUpstream(t, c.h), 150*time.Millisecond)
			w := env.get(reachQuery("56.1629,10.2039", "r=5"))
			wantError(t, w, c.code, c.msg)
			assertNoToken(t, c.name, w, env.logs)
			for _, leak := range []string{"User not authorized", "ServiceException", "boom", "dhm_wcs_DAF", "127.0.0.1"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Fatalf("upstream text %q in response", leak)
				}
			}
			if !strings.Contains(env.logs.String(), "("+c.log+")") {
				t.Fatalf("log %q lacks category %q", env.logs.String(), c.log)
			}
			if env.c.results.Len() != 0 || env.c.tiles.Len() != 0 {
				t.Fatal("a failure was cached")
			}
		})
	}
	// And a success through the same client leaks nothing either.
	env := coverageWCSEnv(t, wcsUpstream(t, tiffAnswer), 5*time.Second)
	w := env.get(reachQuery("56.1629,10.2039", "r=5"))
	decodeCoverage(t, w)
	assertNoToken(t, "success", w, env.logs)
	assertNoToken(t, "raster", env.raster(decodeCoverage(t, w).Raster.URL, true), env.logs)
}

// The whole computation has a deadline: a source that never answers is
// 504 within it.
func TestCoverage941_Deadline(t *testing.T) {
	src := newFakeElevation(t, flatElev(3))
	src.hook = func(ctx context.Context, _ dhm.RasterRequest) error {
		<-ctx.Done()
		return dhm.ContextFailure(ctx.Err())
	}
	env := newCoverageEnv(t, src, CoverageConfig{}, nil, nil)
	env.c.deadline = 200 * time.Millisecond
	start := time.Now()
	wantError(t, env.get(reachQuery("56.1629,10.2039", "r=5")), 504, "terrain data timed out")
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v", el)
	}
}

// Both caches are bounded: the results by entries and bytes, the tiles
// by map.coverage.elevationCacheMB, evicting the least recently used.
func TestCoverage941_CachesAreBounded(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, hillyElev), CoverageConfig{CacheEntries: intp(2), ElevationCacheMB: intp(8)}, nil, nil)
	for _, r := range []string{"r=4", "r=5", "r=6", "r=7"} {
		decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", r)))
	}
	if env.c.results.Len() != 2 || env.c.results.Size() > 2*coverageResultMaxBytes {
		t.Fatalf("results: %d entries, %d bytes", env.c.results.Len(), env.c.results.Size())
	}
	// The oldest is gone, so its raster is too.
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=20")))
	if env.c.tiles.Size() > 8<<20 || env.c.tiles.Len() > (8<<20)/dhm.CoverageTileBytes {
		t.Fatalf("tiles: %d entries, %d bytes over 8 MiB", env.c.tiles.Len(), env.c.tiles.Size())
	}
	if env.c.tiles.Len() == 0 {
		t.Fatal("no tile cached")
	}
}

// ── Nodes ──────────────────────────────────────────────────────────────────

func TestCoverage941_NodeCentre(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(7)), CoverageConfig{}, nil, nil)
	rep, hidden, unplaced := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, 'mast', 'repeater', 56.16291, 10.20389, datetime('now'), datetime('now'), 1),
		       (?, 'h', 'repeater', 56.2, 10.2, datetime('now'), datetime('now'), 1),
		       (?, 'u', 'repeater', NULL, NULL, datetime('now'), datetime('now'), 1)`, rep, hidden, unplaced)
	env.srv.store.InvalidateNodeCache()
	env.srv.cfg.SetNodeBlacklist([]string{hidden})
	w := env.get(reachQuery(rep, "r=5"))
	resp := decodeCoverage(t, w)
	if resp.Center.Kind != "node" || resp.Center.Role != "repeater" || resp.Center.Lat != 56.1629 || resp.Center.Lon != 10.2039 {
		t.Fatalf("centre %+v", resp.Center)
	}
	if resp.Params.TxHeightM != 10 || resp.Params.TxHeightSource != "role-default" || !resp.Params.TxAssumed {
		t.Fatalf("tx height %+v", resp.Params)
	}
	if strings.Contains(w.Body.String(), rep) || strings.Contains(w.Body.String(), "mast") {
		t.Fatal("the node's key or name is in the response")
	}
	for _, pk := range []string{hidden, unplaced, strings.Repeat("d", 64)} {
		wantError(t, env.get(reachQuery(pk, "r=5")), 404, "node not found")
	}
}

// ── Foreign fill ───────────────────────────────────────────────────────────

type fakeFill struct {
	calls, points atomic.Int64
	fail          atomic.Bool
	h             float32
}

func (f *fakeFill) Fill(ctx context.Context, plan *dhm.Plan) (copdem.Result, error) {
	f.calls.Add(1)
	f.points.Add(int64(len(plan.E)))
	if f.fail.Load() {
		return copdem.Result{}, dhm.FetchError{Reason: "status 503"}
	}
	v := make([]float32, len(plan.E))
	for i := range v {
		if plan.Inside(i) {
			v[i] = float32(math.NaN())
		} else {
			v[i] = f.h
		}
	}
	return copdem.Result{Values: v, Filled: len(v)}, nil
}

// With DHM, the fill takes only the cells DHM does not cover: at the
// border the reach crosses into Germany on GLO-30, and says so.
func TestCoverage941_FillsAbroadBesideDHM(t *testing.T) {
	src := newFakeElevation(t, flatElev(4))
	fill := &fakeFill{h: 6}
	env := newCoverageEnv(t, src, CoverageConfig{}, nil, func(d *terrainCoverageDeps) { d.fill = fill })
	resp := decodeCoverage(t, env.get(reachQuery("54.9098,9.7922", "r=12"))) // Sønderborg
	if fill.calls.Load() == 0 || src.calls.Load() == 0 {
		t.Fatalf("fill %d calls, dhm %d", fill.calls.Load(), src.calls.Load())
	}
	ids := []string{}
	for _, s := range resp.Sources {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "dhm-dtm,copdem-glo30" || resp.AttributionFill != copdem.AttributionDerived || resp.Attribution != dhm.AttributionDerived {
		t.Fatalf("sources %v, fill credit %q", ids, resp.AttributionFill)
	}
	if resp.Raster.CoveredPct < 99 {
		t.Fatalf("covered %.1f %% with the fill", resp.Raster.CoveredPct)
	}
	// Inside Denmark nothing is filled.
	before := fill.calls.Load()
	resp = decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5")))
	if fill.calls.Load() != before || len(resp.Sources) != 1 || resp.AttributionFill != "" {
		t.Fatalf("an inland reach was filled: %d calls, sources %+v", fill.calls.Load()-before, resp.Sources)
	}
}

// Without DHM the fill is the only terrain, and a failed fill leaves the
// reach partial, kept only briefly so a later request tries again.
func TestCoverage941_FillOnly(t *testing.T) {
	fill := &fakeFill{h: 5}
	env := newCoverageEnv(t, nil, CoverageConfig{}, nil, func(d *terrainCoverageDeps) { d.fill = fill })
	resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5")))
	if len(resp.Sources) != 1 || resp.Sources[0].ID != "copdem-glo30" || resp.Attribution != "" || resp.AttributionFill != copdem.AttributionDerived {
		t.Fatalf("sources %+v, credits %q / %q", resp.Sources, resp.Attribution, resp.AttributionFill)
	}
	if resp.Raster.CoveredPct < 99 {
		t.Fatalf("covered %.1f %%", resp.Raster.CoveredPct)
	}
	// Outside the GLO-30 region nothing can be computed.
	wantError(t, env.get(reachQuery("37.77,-122.42", "r=5")), 400, "point not covered")

	fill.fail.Store(true)
	resp = decodeCoverage(t, env.get(reachQuery("56.5,9.5", "r=5")))
	if resp.Raster.CoveredPct != 0 || resp.Center.GroundM != nil {
		t.Fatalf("failed fill: covered %.1f %%, ground %v", resp.Raster.CoveredPct, resp.Center.GroundM)
	}
	if !strings.Contains(env.logs.String(), "fill failed (status 503)") {
		t.Fatalf("fill failure not logged by category: %q", env.logs.String())
	}
	now := time.Now()
	env.c.now = func() time.Time { return now.Add(coveragePartialTTL + time.Second) }
	fill.fail.Store(false)
	resp = decodeCoverage(t, env.get(reachQuery("56.5,9.5", "r=5")))
	if resp.Raster.CoveredPct < 99 {
		t.Fatalf("the retry after the partial TTL is still partial: %.1f %%", resp.Raster.CoveredPct)
	}
}

// ── Perf budget ────────────────────────────────────────────────────────────

// TestCoverage941_PerfBudget measures the M0 §6 budget on synthetic
// terrain: the cache hit (JSON + raster) ≤ 300 ms, the warm-elevation
// 40 km / 30 m computation ≤ 3 s, the raster ≤ 400 KB. Run with -v for
// the numbers.
func TestCoverage941_PerfBudget(t *testing.T) {
	if testing.Short() || raceBuild {
		t.Skip("timing budget: not under -short or -race")
	}
	env := newCoverageEnv(t, newFakeElevation(t, func(e, n float64) float32 {
		x, y := e/1000, n/1000
		return float32(math.Max(0, 40+25*math.Sin(x/3.1)*math.Cos(y/4.3)+15*math.Sin(x/0.9+y/1.7)+8*math.Cos(x/0.37-y/0.53)))
	}), CoverageConfig{}, nil, nil)
	start := time.Now()
	resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039")))
	cold := time.Since(start)
	start = time.Now()
	resp = decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "ha=12")))
	warm := time.Since(start)
	start = time.Now()
	hit := env.get(reachQuery("56.1629,10.2039", "ha=12"))
	ras := env.raster(resp.Raster.URL, true)
	hitTime := time.Since(start)
	if hit.Code != 200 || ras.Code != 200 {
		t.Fatal("hit failed")
	}
	t.Logf("40 km / 30 m, %dx%d raster: cold (fake source, no latency) %v, warm compute %v, cache hit %v, payload %d B gzip (%.0f %% of raw), JSON %d B",
		resp.Raster.Width, resp.Raster.Height, cold.Round(time.Millisecond), warm.Round(time.Millisecond), hitTime.Round(time.Microsecond),
		ras.Body.Len(), 100*float64(ras.Body.Len())/float64(resp.Raster.Width*resp.Raster.Height), hit.Body.Len())
	if hitTime > 300*time.Millisecond {
		t.Fatalf("cache hit %v > 300 ms", hitTime)
	}
	if warm > 3*time.Second {
		t.Fatalf("warm compute %v > 3 s", warm)
	}
	if ras.Body.Len() > 400<<10 {
		t.Fatalf("payload %d B > 400 KB", ras.Body.Len())
	}
}

// ── Config ─────────────────────────────────────────────────────────────────

func TestCoverage941_SettingsDefaultsAndClamps(t *testing.T) {
	logs := &dhmLogSink{}
	s := resolveCoverageSettings(CoverageConfig{}, logs.logf)
	want := coverageSettings{maxRadiusM: 40000, resolutionM: 30, maxInFlight: 1, rateLimitPerMin: 2, upstreamPerMin: 240,
		cacheEntries: 32, cacheTTL: 24 * time.Hour, elevationCacheBytes: 64 << 20}
	if s != want || logs.String() != "" {
		t.Fatalf("defaults %+v (log %q)", s, logs.String())
	}
	s = resolveCoverageSettings(CoverageConfig{MaxRadiusKm: floatp(61), ResolutionM: intp(10), MaxInFlight: intp(5),
		RateLimitPerMin: intp(-1), UpstreamPerMin: intp(-1), CacheEntries: intp(0), CacheTTLHours: floatp(math.NaN()),
		ElevationCacheMB: intp(4)}, logs.logf)
	if s != want {
		t.Fatalf("clamped %+v", s)
	}
	for _, k := range []string{"maxRadiusKm", "resolutionM", "maxInFlight", "rateLimitPerMin", "upstreamPerMin", "cacheEntries", "cacheTTLHours", "elevationCacheMB"} {
		if !strings.Contains(logs.String(), "map.coverage."+k) {
			t.Fatalf("out-of-range %s not warned: %q", k, logs.String())
		}
	}
	r := resolveCoverageRadio(nil, logs.logf)
	if r != (CoverageRadio{TxPowerDbm: 22, AntennaGainDbi: 3, RxGainDbi: 3, LossDb: 0, SensitivityDbm: -130}) {
		t.Fatalf("radio defaults %+v", r)
	}
	logs = &dhmLogSink{}
	r = resolveCoverageRadio(&RadioConfig{TxPowerDbm: floatp(40), AntennaGainDbi: floatp(-20), RxGainDbi: floatp(25),
		LossDb: floatp(-1), SensitivityDbm: floatp(-60)}, logs.logf)
	if r != (CoverageRadio{TxPowerDbm: 22, AntennaGainDbi: 3, RxGainDbi: 3, LossDb: 0, SensitivityDbm: -130}) {
		t.Fatalf("radio clamped %+v", r)
	}
	for _, k := range []string{"txPowerDbm", "antennaGainDbi", "rxGainDbi", "lossDb", "sensitivityDbm"} {
		if !strings.Contains(logs.String(), "radio."+k) {
			t.Fatalf("out-of-range radio.%s not warned: %q", k, logs.String())
		}
	}
}

func TestCoverage941_ConfigLoadsAndStaysServerSide(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{"map":{"dhm":{"enabled":true,"token":"` + dhmTestToken + `"},
		"coverage":{"enabled":true,"maxRadiusKm":25,"maxInFlight":2,"upstreamPerMin":120,"elevationCacheMB":32}},
		"radio":{"txPowerDbm":20,"antennaGainDbi":5,"sensitivityDbm":-125}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Coverage.Enabled || *cfg.Coverage.MaxRadiusKm != 25 || *cfg.Coverage.UpstreamPerMin != 120 {
		t.Fatalf("coverage config %+v", cfg.Coverage)
	}
	srv := NewServer(setupTestDB(t), cfg, NewHub())
	if srv.coverage == nil || srv.coverage.set.maxRadiusM != 25000 || srv.coverage.admit.Cap() != 2 || srv.coverage.dhm == nil || srv.coverage.fill != nil {
		t.Fatal("coverage not wired from config")
	}
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	var cc struct {
		Coverage *CoverageClientConfig `json:"coverage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cc); err != nil || cc.Coverage == nil {
		t.Fatalf("client config: %v %s", err, w.Body.String())
	}
	got := *cc.Coverage
	if !got.Enabled || got.MaxRadiusKm != 25 || got.RadiusKm != 25 || got.RxHeightM != 2 || !got.DHM || got.Fill != nil ||
		got.Radio != (CoverageRadio{TxPowerDbm: 20, AntennaGainDbi: 5, RxGainDbi: 3, LossDb: 0, SensitivityDbm: -125}) ||
		got.PointAntenna == nil || got.PointAntenna.M != 2 {
		t.Fatalf("client coverage block %+v", got)
	}
	for _, leak := range []string{dhmTestToken, "upstreamPerMin", "elevationCacheMB", "maxInFlight"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("%q in /api/config/client", leak)
		}
	}
}

// #884: only the canonical `enabled` switches the reach on, and the block
// is lifted out of the client-visible map whatever its spelling.
func TestCoverage941_GateReadsOnlyTheCanonicalKey(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  string
		on   bool
	}{
		{"canonical", `{"map":{"coverage":{"enabled":true},"dhm":{"enabled":true,"token":"t"}}}`, true},
		{"Enabled", `{"map":{"coverage":{"Enabled":true},"dhm":{"enabled":true,"token":"t"}}}`, false},
		{"Enabled after false", `{"map":{"coverage":{"enabled":false,"Enabled":true},"dhm":{"enabled":true,"token":"t"}}}`, false},
		{"Coverage block", `{"map":{"Coverage":{"enabled":true},"dhm":{"enabled":true,"token":"t"}}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := loadConfigLogged(t, c.cfg)
			if cfg.Coverage.Enabled != c.on {
				t.Fatalf("enabled %v, want %v", cfg.Coverage.Enabled, c.on)
			}
			for k := range cfg.Map {
				if strings.EqualFold(k, "coverage") {
					t.Fatalf("map.%s left in the client-visible map", k)
				}
			}
		})
	}
}

// The documented example values are the code defaults, and the example
// leaves the reach off.
func TestCoverage941_ExampleConfigMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, logged := loadConfigLogged(t, string(data))
	if cfg.Coverage.Enabled {
		t.Fatal("the example enables the predicted reach")
	}
	if cfg.Coverage.MaxRadiusKm == nil || cfg.Coverage.UpstreamPerMin == nil || cfg.Coverage.ElevationCacheMB == nil {
		t.Fatal("the example does not document map.coverage")
	}
	logs := &dhmLogSink{}
	if got, want := resolveCoverageSettings(cfg.Coverage, logs.logf), resolveCoverageSettings(CoverageConfig{}, logs.logf); got != want {
		t.Fatalf("example %+v, defaults %+v", got, want)
	}
	rc := cfg.Radio
	if rc == nil || rc.TxPowerDbm == nil || rc.AntennaGainDbi == nil || rc.RxGainDbi == nil || rc.LossDb == nil || rc.SensitivityDbm == nil {
		t.Fatalf("the example does not document the link budget: %+v", rc)
	}
	if got, want := resolveCoverageRadio(rc, logs.logf), resolveCoverageRadio(nil, logs.logf); got != want {
		t.Fatalf("example radio %+v, defaults %+v", got, want)
	}
	if logs.String() != "" || strings.Contains(logged, "[coverage]") {
		t.Fatalf("example config warns: %q %q", logs.String(), logged)
	}
}

func TestCoverage941_OpenAPI(t *testing.T) {
	routes := routeDescriptions()
	m, ok := routes["GET /api/terrain/coverage"]
	if !ok {
		t.Fatal("route not in OpenAPI")
	}
	have := map[string]bool{}
	for _, p := range m.QueryParams {
		have[p.Name] = p.Required
	}
	for _, p := range []string{"at", "ha", "hr", "r", "freq"} {
		if _, ok := have[p]; !ok {
			t.Fatalf("param %s undocumented", p)
		}
	}
	if !have["at"] {
		t.Fatal("at not required")
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
	if _, ok := routes["GET /api/terrain/coverage/raster"]; !ok {
		t.Fatal("raster route not in OpenAPI")
	}
}
