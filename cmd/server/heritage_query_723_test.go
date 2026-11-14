package main

// #723 M2 — GET /api/terrain/heritage. Every test runs against a fake
// source or an httptest fake WFS; the agency's service is never contacted.

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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/heritage"
)

// fakeHeritage stands in for the WFS client: it serves page for every
// cell, counts calls per cell, and can be made to fail or to block.
type fakeHeritage struct {
	mu    sync.Mutex
	page  heritage.Page
	err   error
	calls map[heritage.Cell]int
	total atomic.Int32
	gate  chan struct{} // non-nil: block until closed or ctx is done
	ctxs  chan error    // receives ctx.Err() of a blocked call that gave up
}

func (f *fakeHeritage) FetchCell(ctx context.Context, c heritage.Cell) (heritage.Page, error) {
	f.total.Add(1)
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[heritage.Cell]int{}
	}
	f.calls[c]++
	f.mu.Unlock()
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			if f.ctxs != nil {
				f.ctxs <- ctx.Err()
			}
			return heritage.Page{}, dhm.FetchError{Reason: "canceled"}
		}
	}
	return f.page, f.err
}

// The synthetic fixture zones (internal/heritage/testdata): a "Rundhøj"
// zone centred on (526400, 6179600), 111.5 m east-west, and a "Vandfyldt
// grav" zone centred on (526900, 6179600), 136 m east-west.
func heritageFixturePage(t *testing.T) heritage.Page {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "heritage", "testdata", "zones_two.json"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := heritage.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

type heritageEnv struct {
	srv    *Server
	router *mux.Router
	q      *heritageQuery
	src    *fakeHeritage
	logs   *dhmLogSink
}

func newHeritageEnv(t *testing.T, hc HeritageConfig, src *fakeHeritage) *heritageEnv {
	t.Helper()
	srv, router := setupTestServer(t)
	logs := &dhmLogSink{}
	hc.Enabled = true
	srv.cfg.Heritage = hc
	srv.heritage = newHeritageQuery(resolveHeritageSettings(hc, logs.logf), heritageQueryDeps{src: src, logf: logs.logf})
	if srv.heritage == nil {
		t.Fatal("newHeritageQuery returned nil")
	}
	return &heritageEnv{srv: srv, router: router, q: srv.heritage, src: src, logs: logs}
}

func (e *heritageEnv) get(query string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/terrain/heritage?"+query, nil)
	for _, m := range mod {
		m(req)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// at is the query for the EPSG:25832 point (e, n).
func at(e, n float64) string {
	lat, lon := dhm.FromUTM32(e, n)
	return fmt.Sprintf("lat=%.7f&lon=%.7f", lat, lon)
}

func decodeHeritage(t *testing.T, w *httptest.ResponseRecorder) HeritageInfoResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp HeritageInfoResponse
	dec := json.NewDecoder(strings.NewReader(w.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return resp
}

func fromIP(ip string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = ip + ":1000" }
}

// ── Enablement and grammar ─────────────────────────────────────────────────

func TestHeritageQuery_DisabledIs404(t *testing.T) {
	_, router := setupTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/terrain/heritage?"+at(526400, 6179600), nil))
	wantError(t, w, http.StatusNotFound, "not found")
	if newHeritageQueryFromConfig(&Config{}) != nil || newHeritageQueryFromConfig(nil) != nil {
		t.Fatal("query offered without map.heritage.enabled")
	}
	if newHeritageQueryFromConfig(&Config{Heritage: HeritageConfig{Enabled: true}}) == nil {
		t.Fatal("query not offered with map.heritage.enabled")
	}
}

// Every malformed request is the same fixed 400 that echoes nothing, and
// none reaches upstream.
func TestHeritageQuery_RejectsJunkWithoutEcho(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t)}
	env := newHeritageEnv(t, HeritageConfig{}, src)
	for _, q := range []string{
		"",
		"lat=55.75",
		"lon=9.42",
		"lat=55.75&lon=9.42&x=1",
		"lat=55.75&lat=55.76&lon=9.42",
		"lat=55.75&lon=9.42&lon=9.43",
		"lat=5.575e1&lon=9.42",
		"lat=NaN&lon=9.42",
		"lat=Inf&lon=9.42",
		"lat=0x1p3&lon=9.42",
		"lat=+55.75&lon=9.42",
		"lat=%2055.75&lon=9.42",
		"lat=91&lon=9.42",
		"lat=55.75&lon=181",
		"lat=55.123456789012&lon=9.42",
		"lat=<script>&lon=9.42",
		"LAT=55.75&lon=9.42",
		"lat=55.75&lon=9.42&cql_filter=INCLUDE",
	} {
		w := env.get(q)
		wantError(t, w, http.StatusBadRequest, "invalid heritage request")
		if strings.Contains(w.Body.String(), "script") || strings.Contains(w.Body.String(), "cql") {
			t.Fatalf("%q echoed: %s", q, w.Body.String())
		}
	}
	if n := src.total.Load(); n != 0 {
		t.Fatalf("%d upstream calls for invalid requests", n)
	}
}

// A point outside the layers' extent is answered without an upstream call
// or a charge: there is nothing to look up.
func TestHeritageQuery_OutsideDenmarkMakesNoUpstreamCall(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t)}
	env := newHeritageEnv(t, HeritageConfig{RateLimitPerMin: intp(1)}, src)
	// 55.5,15.6 is east of the layers' box but inside the projected cell
	// extent, so the box itself must be checked.
	for _, q := range []string{"lat=52.52&lon=13.405", "lat=59.91&lon=10.75", "lat=-33.9&lon=151.2", "lat=55.75&lon=7.5", "lat=55.5&lon=15.6"} {
		w := env.get(q, fromIP("198.51.100.9"))
		resp := decodeHeritage(t, w)
		if resp.Nearest != nil || resp.Count != 0 || w.Header().Get("X-CoreScope-Cache") != "none" {
			t.Fatalf("%s: %+v %q", q, resp, w.Header().Get("X-CoreScope-Cache"))
		}
	}
	if n := src.total.Load(); n != 0 {
		t.Fatalf("%d upstream calls outside the extent", n)
	}
}

// ── Answer ─────────────────────────────────────────────────────────────────

func TestHeritageQuery_InsideAZone(t *testing.T) {
	env := newHeritageEnv(t, HeritageConfig{}, &fakeHeritage{page: heritageFixturePage(t)})
	resp := decodeHeritage(t, env.get(at(526400, 6179600)))
	want := HeritageMonument{Type: "Rundhøj", Dating: "Oldtid", Status: "Fredet", ProtectionNumber: "3624:15",
		SystemNumber: "190001", DistanceM: 0, InsideZone: true}
	if resp.Nearest == nil || *resp.Nearest != want {
		t.Fatalf("nearest %+v, want %+v", resp.Nearest, want)
	}
	if resp.Count != 1 || resp.RadiusM != 100 || resp.Truncated || resp.Attribution != heritageAttribution {
		t.Fatalf("%+v", resp)
	}
}

func TestHeritageQuery_NearestWithinRadius(t *testing.T) {
	env := newHeritageEnv(t, HeritageConfig{}, &fakeHeritage{page: heritageFixturePage(t)})
	// 40 m east of the Rundhøj zone's eastern edge.
	resp := decodeHeritage(t, env.get(at(526551.5, 6179600)))
	if resp.Nearest == nil || resp.Nearest.Type != "Rundhøj" || resp.Nearest.InsideZone || resp.Count != 1 {
		t.Fatalf("%+v", resp.Nearest)
	}
	// The lat/lon round trip at 7 decimals is good to a centimetre.
	if resp.Nearest.DistanceM != 40 {
		t.Fatalf("distance %d", resp.Nearest.DistanceM)
	}
	// Between the zones: 138.5 m and 114 m away, nothing within 100 m.
	resp = decodeHeritage(t, env.get(at(526650, 6179600)))
	if resp.Nearest != nil || resp.Count != 0 {
		t.Fatalf("%+v", resp)
	}
	// With a 150 m radius both count, and the grave is nearer.
	env = newHeritageEnv(t, HeritageConfig{RadiusM: intp(150)}, &fakeHeritage{page: heritageFixturePage(t)})
	resp = decodeHeritage(t, env.get(at(526650, 6179600)))
	if resp.Nearest == nil || resp.Nearest.Type != "Vandfyldt grav" || resp.Nearest.DistanceM != 114 ||
		resp.Nearest.ProtectionNumber != "" || resp.Count != 2 || resp.RadiusM != 150 {
		t.Fatalf("%+v %+v", resp, resp.Nearest)
	}
}

// Only fields that come from the dataset, and nothing from the request:
// the body has exactly the documented keys, no coordinates, and none of
// the dataset fields the response does not carry (url, stednr, ...).
func TestHeritageQuery_FieldsShapeAndNothingEchoed(t *testing.T) {
	env := newHeritageEnv(t, HeritageConfig{}, &fakeHeritage{page: heritageFixturePage(t)})
	q := at(526400, 6179600)
	w := env.get(q)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	var nearest map[string]json.RawMessage
	if err := json.Unmarshal(top["nearest"], &nearest); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]json.RawMessage) string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	if got := keys(top); got != "attribution,count,nearest,radiusM,truncated" {
		t.Fatalf("top-level keys %s", got)
	}
	if got := keys(nearest); got != "dating,distanceM,insideZone,protectionNumber,status,systemNumber,type" {
		t.Fatalf("nearest keys %s", got)
	}
	vals, _ := url.ParseQuery(q)
	body := w.Body.String()
	for _, leak := range []string{vals.Get("lat"), vals.Get("lon"), vals.Get("lat")[:6], "kulturarv.dk", "Lokalitet", "170109"} {
		if strings.Contains(body, leak) {
			t.Fatalf("body carries %q: %s", leak, body)
		}
	}
}

func TestHeritageQuery_TruncatedPassesThrough(t *testing.T) {
	page := heritageFixturePage(t)
	page.Truncated = true
	env := newHeritageEnv(t, HeritageConfig{}, &fakeHeritage{page: page})
	if resp := decodeHeritage(t, env.get(at(526400, 6179600))); !resp.Truncated {
		t.Fatal("truncated page not reported")
	}
}

// ── Cache, coalescing, limits ──────────────────────────────────────────────

// Clicks inside one 250 m cell share one upstream call; another cell is a
// new call. The header says which.
func TestHeritageQuery_CachePerCell(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t)}
	env := newHeritageEnv(t, HeritageConfig{}, src)
	// (526400, 6179600) and (526260, 6179740) are both in cell 2105/24718.
	for i, c := range []struct {
		e, n  float64
		state string
	}{{526400, 6179600, "miss"}, {526260, 6179740, "hit"}, {526400, 6179600, "hit"}, {526551.5, 6179600, "miss"}} {
		w := env.get(at(c.e, c.n))
		decodeHeritage(t, w)
		if got := w.Header().Get("X-CoreScope-Cache"); got != c.state {
			t.Fatalf("request %d: cache %q, want %q", i, got, c.state)
		}
	}
	if n := src.total.Load(); n != 2 {
		t.Fatalf("%d upstream calls, want 2", n)
	}
}

func TestHeritageQuery_CacheTTL(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t)}
	env := newHeritageEnv(t, HeritageConfig{CacheTTLHours: floatp(1)}, src)
	clock := time.Unix(1_700_000_000, 0)
	env.q.now = func() time.Time { return clock }
	env.get(at(526400, 6179600))
	clock = clock.Add(59 * time.Minute)
	env.get(at(526400, 6179600))
	clock = clock.Add(2 * time.Minute)
	env.get(at(526400, 6179600))
	if n := src.total.Load(); n != 2 {
		t.Fatalf("%d upstream calls across the TTL, want 2", n)
	}
}

func TestHeritageQuery_CoalescesConcurrentClicks(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t), gate: make(chan struct{})}
	env := newHeritageEnv(t, HeritageConfig{RateLimitPerMin: intp(0)}, src)
	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = env.get(at(526400+float64(i), 6179600)).Code
		}(i)
	}
	waitUntil(t, "20 waiters on one fetch", func() bool { return env.q.flight.Pending() == 20 })
	close(src.gate)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	if n := src.total.Load(); n != 1 {
		t.Fatalf("%d upstream calls for 20 clicks in one cell", n)
	}
}

// Only a click that would go upstream is charged; beyond the budget it is
// 429 with Retry-After, and cache hits stay free.
func TestHeritageQuery_PerIPLimit(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t)}
	env := newHeritageEnv(t, HeritageConfig{RateLimitPerMin: intp(2)}, src)
	client := fromIP("198.51.100.7")
	decodeHeritage(t, env.get(at(526400, 6179600), client))
	decodeHeritage(t, env.get(at(527400, 6179600), client))
	w := env.get(at(528400, 6179600), client)
	wantError(t, w, http.StatusTooManyRequests, "rate limit exceeded")
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After %q", ra)
	}
	for i := 0; i < 5; i++ {
		decodeHeritage(t, env.get(at(526400, 6179600), client))
	}
	decodeHeritage(t, env.get(at(528400, 6179600), fromIP("198.51.100.8")))
	if n := src.total.Load(); n != 3 {
		t.Fatalf("%d upstream calls", n)
	}
}

// At most maxInFlight distinct upstream fetches run at once, server-wide,
// through the shared dhm.Admission (#744): beyond that a click is 503 with
// Retry-After before a worker starts or any upstream call, the per-IP
// charge is given back, and refusals are logged throttled. A click on a
// cell already being fetched joins that fetch and takes no slot.
func TestHeritageQuery_GlobalAdmissionBound(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t), gate: make(chan struct{})}
	env := newHeritageEnv(t, HeritageConfig{RateLimitPerMin: intp(3), MaxInFlight: intp(1)}, src)
	if env.q.admit.Cap() != 1 {
		t.Fatalf("admission cap %d, want maxInFlight 1", env.q.admit.Cap())
	}
	held := at(540400, 6179600)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, ip := range []string{"198.51.100.8", "198.51.100.9"} {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			codes <- env.get(held, fromIP(ip)).Code
		}(ip)
		// The first holds the only slot; the second joins its fetch.
		waitUntil(t, "fetch admitted", func() bool { return env.q.admit.InUse() == 1 && src.total.Load() == 1 })
	}
	client := fromIP("198.51.100.7")
	for i := 0; i < 10; i++ {
		w := env.get(at(526400, 6179600), client)
		wantError(t, w, http.StatusServiceUnavailable, "heritage lookups busy")
		if w.Header().Get("Retry-After") != "5" {
			t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
		}
	}
	if n := src.total.Load(); n != 1 {
		t.Fatalf("%d upstream calls while full, want only the held one", n)
	}
	if got := env.q.refused.Load(); got != 10 {
		t.Fatalf("refused %d", got)
	}
	if n := strings.Count(env.logs.String(), "admission"); n != 1 || !strings.Contains(env.logs.String(), "map.heritage.maxInFlight (1)") {
		t.Fatalf("refusal log %q", env.logs.String())
	}
	close(src.gate)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("held or joined click got %d", c)
		}
	}
	waitUntil(t, "slot released", func() bool { return env.q.admit.InUse() == 0 })
	// The per-IP budget of 3 is intact.
	for i := 0; i < 3; i++ {
		decodeHeritage(t, env.get(at(526400+float64(i)*1000, 6179600), client))
	}
	wantError(t, env.get(at(530400, 6179600), client), http.StatusTooManyRequests, "rate limit exceeded")
}

// A click that leaves cancels the upstream call once nobody waits for it,
// and its admission slot is free again.
func TestHeritageQuery_CancelFreesTheSlot(t *testing.T) {
	src := &fakeHeritage{page: heritageFixturePage(t), gate: make(chan struct{}), ctxs: make(chan error, 1)}
	env := newHeritageEnv(t, HeritageConfig{MaxInFlight: intp(1)}, src)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("GET", "/api/terrain/heritage?"+at(526400, 6179600), nil).WithContext(ctx)
		env.router.ServeHTTP(httptest.NewRecorder(), req)
	}()
	waitUntil(t, "fetch admitted", func() bool { return env.q.admit.InUse() == 1 })
	cancel()
	<-done
	select {
	case err := <-src.ctxs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("upstream ctx %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream call not cancelled")
	}
	waitUntil(t, "slot released", func() bool { return env.q.admit.InUse() == 0 })
	close(src.gate)
	decodeHeritage(t, env.get(at(526400, 6179600)))
}

// Upstream failures are 502, timeouts 504, never with upstream text, and
// are logged by category at most once a minute.
func TestHeritageQuery_UpstreamFailuresAreClean(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		msg  string
	}{
		{dhm.FetchError{Reason: "status 500"}, http.StatusBadGateway, "heritage data unavailable"},
		{dhm.FetchError{Reason: "bad response"}, http.StatusBadGateway, "heritage data unavailable"},
		{errors.New("secret upstream text"), http.StatusBadGateway, "heritage data unavailable"},
		{dhm.FetchError{Reason: "timeout"}, http.StatusGatewayTimeout, "heritage data timed out"},
	} {
		src := &fakeHeritage{err: c.err}
		env := newHeritageEnv(t, HeritageConfig{}, src)
		for i := 0; i < 3; i++ {
			w := env.get(at(526400+float64(i)*1000, 6179600))
			wantError(t, w, c.code, c.msg)
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("upstream text in the response")
			}
		}
		logs := env.logs.String()
		if strings.Count(logs, "[heritage] upstream") != 1 || strings.Contains(logs, "secret") {
			t.Fatalf("%v: logs %q", c.err, logs)
		}
		// A failure is not cached.
		src.err = nil
		src.page = heritageFixturePage(t)
		if w := env.get(at(526400, 6179600)); w.Header().Get("X-CoreScope-Cache") != "miss" {
			t.Fatalf("failure was cached: %q", w.Header().Get("X-CoreScope-Cache"))
		}
	}
}

// End to end through the real WFS client against an httptest fake WFS:
// one request per cell, a bbox around it in EPSG:25832, never a filter.
func TestHeritageQuery_AgainstFakeWFS(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "heritage", "testdata", "zones_two.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []url.Values
	var mu sync.Mutex
	wfs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Query())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		w.Write(body)
	}))
	defer wfs.Close()
	srv, router := setupTestServer(t)
	hc := HeritageConfig{Enabled: true}
	set := resolveHeritageSettings(hc, nil)
	opts := heritage.DefaultClientOptions()
	opts.BaseURL, opts.MarginM, opts.Timeout = wfs.URL, set.radiusM, set.upstreamTimeout
	client, err := heritage.NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.heritage = newHeritageQuery(set, heritageQueryDeps{src: client})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/terrain/heritage?"+at(526400, 6179600), nil))
	resp := decodeHeritage(t, w)
	if resp.Nearest == nil || !resp.Nearest.InsideZone || resp.Nearest.Type != "Rundhøj" {
		t.Fatalf("%+v", resp)
	}
	if len(got) != 1 {
		t.Fatalf("%d WFS requests", len(got))
	}
	q := got[0]
	if q.Get("bbox") != "526150,6179400,526600,6179850,urn:ogc:def:crs:EPSG::25832" || q.Get("typeNames") != heritage.TypeName {
		t.Fatalf("WFS query %v", q)
	}
	for k := range q {
		if strings.Contains(strings.ToLower(k), "filter") {
			t.Fatalf("WFS query carries %s", k)
		}
	}
}

// ── Settings, config, docs ─────────────────────────────────────────────────

func TestHeritageQuery_SettingsDefaultsAndClamps(t *testing.T) {
	logs := &dhmLogSink{}
	s := resolveHeritageSettings(HeritageConfig{}, logs.logf)
	want := heritageSettings{radiusM: 100, cacheEntries: 256, cacheTTL: 168 * time.Hour, upstreamTimeout: 10 * time.Second,
		rateLimitPerMin: 20, maxInFlight: 4}
	if s != want {
		t.Fatalf("defaults %+v", s)
	}
	if logs.String() != "" {
		t.Fatalf("defaults warn: %q", logs.String())
	}
	s = resolveHeritageSettings(HeritageConfig{RadiusM: intp(251), CacheEntries: intp(0), CacheTTLHours: floatp(math.NaN()),
		UpstreamTimeoutMs: intp(499), RateLimitPerMin: intp(-1), MaxInFlight: intp(17)}, logs.logf)
	if s != want {
		t.Fatalf("clamped %+v", s)
	}
	for _, k := range []string{"radiusM", "cacheEntries", "cacheTTLHours", "upstreamTimeoutMs", "rateLimitPerMin", "maxInFlight"} {
		if !strings.Contains(logs.String(), "map.heritage."+k) {
			t.Fatalf("out-of-range %s not warned: %q", k, logs.String())
		}
	}
	s = resolveHeritageSettings(HeritageConfig{RadiusM: intp(0), CacheEntries: intp(4096), CacheTTLHours: floatp(0.5),
		UpstreamTimeoutMs: intp(500), RateLimitPerMin: intp(0), MaxInFlight: intp(16)}, logs.logf)
	if s != (heritageSettings{radiusM: 0, cacheEntries: 4096, cacheTTL: 30 * time.Minute, upstreamTimeout: 500 * time.Millisecond,
		rateLimitPerMin: 0, maxInFlight: 16}) {
		t.Fatalf("edges %+v", s)
	}
}

// The knobs load from config.json, never reach the browser, and the
// example documents exactly the defaults.
func TestHeritageQuery_ConfigLoadsStaysServerSideAndExampleMatches(t *testing.T) {
	cfg := writeHeritageConfig(t, `{"map":{"heritage":{"enabled":true,"radiusM":50,"rateLimitPerMin":5}}}`)
	if cfg.Heritage.RadiusM == nil || *cfg.Heritage.RadiusM != 50 || *cfg.Heritage.RateLimitPerMin != 5 {
		t.Fatalf("%+v", cfg.Heritage)
	}
	srv, router := setupTestServer(t)
	srv.cfg.Heritage = cfg.Heritage
	raw := string(clientHeritageRaw(t, router))
	for _, k := range []string{"radius", "rateLimit", "cache", "maxInFlight", "upstream"} {
		if strings.Contains(raw, k) {
			t.Fatalf("client block carries %s: %s", k, raw)
		}
	}

	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	ex := writeHeritageConfig(t, string(data))
	if ex.Heritage.Enabled || newHeritageQueryFromConfig(ex) != nil {
		t.Fatal("example config enables the heritage query")
	}
	if ex.Heritage.RadiusM == nil || ex.Heritage.MaxInFlight == nil {
		t.Fatal("example config does not document the heritage knobs")
	}
	logs := &dhmLogSink{}
	if got, want := resolveHeritageSettings(ex.Heritage, logs.logf), resolveHeritageSettings(HeritageConfig{}, logs.logf); got != want {
		t.Fatalf("example %+v, defaults %+v", got, want)
	}
	if logs.String() != "" {
		t.Fatalf("example warns: %q", logs.String())
	}
}

func TestHeritageQuery_OpenAPI(t *testing.T) {
	m, ok := routeDescriptions()["GET /api/terrain/heritage"]
	if !ok {
		t.Fatal("route not in OpenAPI")
	}
	req := map[string]bool{}
	for _, p := range m.QueryParams {
		req[p.Name] = p.Required
	}
	if len(req) != 2 || !req["lat"] || !req["lon"] {
		t.Fatalf("params %v", req)
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

// BenchmarkHeritageQuery_CacheHit is the hot path of a click in a cached
// cell: parse, project, decode the cached page, Nearest, encode.
func BenchmarkHeritageQuery_CacheHit(b *testing.B) {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "heritage", "testdata", "zones_two.json"))
	if err != nil {
		b.Fatal(err)
	}
	page, err := heritage.Parse(data)
	if err != nil {
		b.Fatal(err)
	}
	q := newHeritageQuery(resolveHeritageSettings(HeritageConfig{}, nil), heritageQueryDeps{src: &fakeHeritage{page: page}})
	s := &Server{heritage: q}
	query := at(526400, 6179600)
	s.handleHeritageInfo(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/terrain/heritage?"+query, nil))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		s.handleHeritageInfo(w, httptest.NewRequest("GET", "/api/terrain/heritage?"+query, nil))
		if w.Header().Get("X-CoreScope-Cache") != "hit" {
			b.Fatal("not a hit")
		}
	}
}
