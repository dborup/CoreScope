package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/copdem/copdemtest"
	"github.com/meshcore-analyzer/dhm"
)

// #738 M1 — the foreign fill in GET /api/terrain/profile. DHM is a fake
// ElevationSource (or the real WCS client against a fake upstream);
// Copernicus is copdemtest's fake bucket behind an injected transport.
// Nothing reaches the network, and no real token exists here.

const (
	fillDHMHeight = 5 // the fake DHM: every covered sample at 5 m
	cphPt         = "55.67610,12.56830"
	malmoPt       = "55.60500,13.00380" // just east of 13° E: tile N55E013
	sonderborgPt  = "54.90980,9.79220"
	kielPt        = "54.32330,10.12280" // south of DHM's raster extent
	lubeckPt      = "53.86550,10.68660"
	silkeborgPt   = "56.16970,9.54510"
)

// fillHeight is the synthetic GLO-30 terrain: a plane in lon/lat, 145 m
// and more on the Swedish side, never DHM's 5 m.
func fillHeight(lon, lat float64) float32 { return float32(100 + 50*(lon-12) + 30*(lat-55)) }

var (
	fillTenth = copdemtest.Layout{W: 240, H: 360, Block: 64}
	fillDense = copdemtest.Layout{W: 480, H: 720, Block: 16}
)

func newFillBucket(t *testing.T, l copdemtest.Layout) *copdemtest.Bucket {
	b := copdemtest.New()
	t.Cleanup(b.Close)
	b.Generate = copdemtest.Synthetic(l, fillHeight)
	return b
}

func fillClient(b *copdemtest.Bucket) *copdem.Client {
	o := copdem.DefaultOptions()
	o.Transport = b.Transport()
	return copdem.NewClient(o)
}

// newFillEnv is the profile with the fill on, DHM a flat 5 m.
func newFillEnv(t *testing.T, b *copdemtest.Bucket, pc dhm.ProfileConfig) (*terrainEnv, *fakeElevation) {
	t.Helper()
	src := newFakeElevation(t, flatElev(fillDHMHeight))
	pc.ForeignFill.Enabled = true
	env := newTerrainEnvWith(t, src, pc, nil, func(d *terrainProfilerDeps) { d.fill = fillClient(b) })
	if env.p.fill == nil {
		t.Fatal("fill not wired")
	}
	return env, src
}

// inFill reports whether d lies in one of the fill ranges.
func inFill(ranges []TerrainFillRange, d float64) bool {
	for _, r := range ranges {
		if d >= r.FromM && d <= r.ToM {
			return true
		}
	}
	return false
}

// checkMixed: inside a fill range every sample is GLO-30 and has no
// surface; outside, every sample is DHM's.
func checkMixed(t *testing.T, resp TerrainProfileResponse) {
	t.Helper()
	if len(resp.Coverage.Fill) == 0 {
		t.Fatal("no fill ranges")
	}
	for _, s := range resp.Samples {
		if s.DTM == nil {
			t.Fatalf("sample at %v m has no DTM", s.D)
		}
		filled := inFill(resp.Coverage.Fill, s.D)
		switch {
		case filled && (*s.DTM == fillDHMHeight || s.DSM != nil):
			t.Fatalf("sample at %v m is in a fill range but has DTM %v, DSM %v", s.D, *s.DTM, s.DSM)
		case !filled && *s.DTM != fillDHMHeight:
			t.Fatalf("sample at %v m is outside every fill range but has DTM %v", s.D, *s.DTM)
		}
	}
}

// Off by default: with map.dhm.profile.foreignFill absent, nothing is
// filled and the bucket is never asked, even with a client at hand.
func TestTerrainFill_OffByDefault(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	src := newFakeElevation(t, flatElev(fillDHMHeight))
	env := newTerrainEnvWith(t, src, dhm.ProfileConfig{}, nil, func(d *terrainProfilerDeps) { d.fill = fillClient(b) })
	if env.p.fill != nil || env.p.set.fill.enabled {
		t.Fatal("the fill is on without map.dhm.profile.foreignFill.enabled")
	}
	resp := decodeProfile(t, env.get(pathQuery(cphPt, malmoPt)))
	if resp.Coverage.State != "partial" || resp.Verdict.State != "unknown" || resp.Coverage.Mixed || resp.Coverage.Fill != nil || resp.AttributionFill != "" {
		t.Fatalf("coverage %+v, verdict %s, attributionFill %q", resp.Coverage, resp.Verdict.State, resp.AttributionFill)
	}
	if b.Requests.Load() != 0 {
		t.Fatalf("%d requests to the bucket with the fill off", b.Requests.Load())
	}
}

// Copenhagen → Malmö: the Swedish end is filled, the profile is full with
// a verdict on the mixed DTM, the surface is unknown, and the Copernicus
// credit appears.
func TestTerrainFill_CopenhagenMalmo(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, src := newFillEnv(t, b, dhm.ProfileConfig{})
	w := env.get(pathQuery(cphPt, malmoPt))
	resp := decodeProfile(t, w)
	if resp.Coverage.State != "full" || resp.Coverage.Pct != 100 || resp.Coverage.FirstGapM != nil || !resp.Coverage.Mixed {
		t.Fatalf("coverage %+v", resp.Coverage)
	}
	if resp.Verdict.State == "unknown" || resp.Verdict.WorstClearanceM == nil {
		t.Fatalf("no verdict on a full mixed profile: %+v", resp.Verdict)
	}
	if resp.Verdict.Surface == nil || resp.Verdict.Surface.State != "unknown" || resp.Verdict.Surface.WorstClearanceM != nil {
		t.Fatalf("surface %+v: abroad there is no surface model", resp.Verdict.Surface)
	}
	if resp.Attribution != dhm.AttributionDerived || resp.AttributionFill != copdem.AttributionDerived {
		t.Fatalf("attribution %q / %q", resp.Attribution, resp.AttributionFill)
	}
	checkMixed(t, resp)
	last := resp.Coverage.Fill[len(resp.Coverage.Fill)-1]
	if last.ToM != resp.DistanceM || last.Source != copdem.Source || last.ResolutionM != 30 {
		t.Fatalf("last fill range %+v, distance %v", last, resp.DistanceM)
	}
	if resp.Coverage.Fill[0].FromM <= 0 {
		t.Fatalf("Copenhagen is DHM's: first fill range %+v", resp.Coverage.Fill[0])
	}
	// Malmö's ground is GLO-30's.
	if g := resp.B.GroundM; g == nil || math.Abs(*g-float64(fillHeight(13.0038, 55.605))) > 0.5 {
		t.Fatalf("Malmö ground %v", g)
	}
	if g := resp.A.GroundM; g == nil || *g != fillDHMHeight {
		t.Fatalf("Copenhagen ground %v", g)
	}
	// Only the two tiles of the path, and only range reads of them.
	for _, r := range b.Log() {
		if r.Path != "/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif" &&
			r.Path != "/Copernicus_DSM_COG_10_N55_00_E013_00_DEM/Copernicus_DSM_COG_10_N55_00_E013_00_DEM.tif" {
			t.Fatalf("request for %s", r.Path)
		}
	}
	if b.Requests.Load() < 3 || b.Requests.Load() > dhm.MaxCallsPerModel {
		t.Fatalf("%d requests", b.Requests.Load())
	}
	if src.calls.Load() == 0 {
		t.Fatal("DHM was not asked for the Danish part")
	}
}

// A path DHM covers makes no Copernicus request and says nothing about a
// fill: no mixed, no ranges, no second credit.
func TestTerrainFill_PureDHMPathMakesNoRequest(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{})
	w := env.get(pathQuery(aarhusPt, silkeborgPt))
	resp := decodeProfile(t, w)
	if resp.Coverage.State != "full" || resp.Coverage.Mixed || resp.AttributionFill != "" || b.Requests.Load() != 0 {
		t.Fatalf("coverage %+v, attributionFill %q, %d requests", resp.Coverage, resp.AttributionFill, b.Requests.Load())
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &raw)
	var cov map[string]json.RawMessage
	json.Unmarshal(raw["coverage"], &cov)
	if _, ok := cov["fill"]; ok {
		t.Fatalf("coverage %s carries fill ranges", raw["coverage"])
	}
	if _, ok := raw["attributionFill"]; ok {
		t.Fatal("attributionFill on a DHM-only profile")
	}
}

// A path with no DHM sample still makes no upstream call at all: the fill
// only completes a profile that DHM is part of.
func TestTerrainFill_PathOutsideDenmarkStillMakesNoCall(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, src := newFillEnv(t, b, dhm.ProfileConfig{})
	resp := decodeProfile(t, env.get(pathQuery(kielPt, lubeckPt)))
	if resp.Coverage.State != "none" || resp.Coverage.Mixed || src.calls.Load() != 0 || b.Requests.Load() != 0 {
		t.Fatalf("coverage %+v, %d DHM calls, %d bucket requests", resp.Coverage, src.calls.Load(), b.Requests.Load())
	}
}

// Sønderborg → Kiel: Kiel is south of DHM's raster extent, not merely on
// foreign land, and is filled too.
func TestTerrainFill_OutOfExtent(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{})
	resp := decodeProfile(t, env.get(pathQuery(sonderborgPt, kielPt)))
	if resp.Coverage.State != "full" || !resp.Coverage.Mixed || resp.Verdict.State == "unknown" {
		t.Fatalf("coverage %+v, verdict %s", resp.Coverage, resp.Verdict.State)
	}
	checkMixed(t, resp)
}

// A failed fill degrades the profile to its DHM part and is logged once
// by category. The DHM part is cached as usual and only the fill is tried
// again (#773 review finding 1): five views of a path whose fill keeps
// failing cost one DHM fetch, not five.
func TestTerrainFill_FailedFillKeepsDHMCached(t *testing.T) {
	b := copdemtest.New() // empty: every listed tile is a 404
	t.Cleanup(b.Close)
	env, src := newFillEnv(t, b, dhm.ProfileConfig{})
	var dhmCalls int64
	var states []string
	for i := 0; i < 5; i++ {
		w := env.get(pathQuery(cphPt, malmoPt, "model=both"))
		resp := decodeProfile(t, w)
		if resp.Coverage.State != "partial" || resp.Verdict.State != "unknown" || resp.Coverage.Mixed || resp.Coverage.Fill != nil || resp.AttributionFill != "" {
			t.Fatalf("request %d: coverage %+v, verdict %s", i, resp.Coverage, resp.Verdict.State)
		}
		if i == 0 {
			dhmCalls = src.calls.Load()
		}
		states = append(states, w.Header().Get("X-CoreScope-Cache"))
	}
	if dhmCalls == 0 || src.calls.Load() != dhmCalls {
		t.Fatalf("%d DHM raster calls after five requests, want the first request's %d", src.calls.Load(), dhmCalls)
	}
	if got := strings.Join(states, ","); got != "miss,hit,hit,hit,hit" {
		t.Fatalf("cache states %s", got)
	}
	if env.p.cache.Len() != 1 {
		t.Fatalf("%d cache entries, want the degraded profile's DHM part", env.p.cache.Len())
	}
	if b.Requests.Load() < 5 {
		t.Fatalf("%d bucket requests: a cached partial profile did not try the fill again", b.Requests.Load())
	}
	logs := env.logs.String()
	if !strings.Contains(logs, "[dhm] terrain profile foreign fill failed (status 404)") {
		t.Fatalf("log %q", logs)
	}
	if strings.Contains(logs, "amazonaws") || strings.Contains(logs, "Copernicus_DSM") || strings.Count(logs, "foreign fill failed") != 1 {
		t.Fatalf("log names the upstream, or is not throttled: %q", logs)
	}
}

// A fill that succeeds on a retry completes the cached profile: the next
// view is mixed and makes no upstream call at all.
func TestTerrainFill_RetriedFillCompletesCachedProfile(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	ok := everywhere(999)
	env, f := newScriptedEnv(t, func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
		if failing.Load() {
			return copdem.Result{}, dhm.FetchError{Reason: "status 503"}
		}
		return ok(ctx, p)
	})
	src := env.p.src.(*fakeElevation)
	if resp := decodeProfile(t, env.get(pathQuery(cphPt, malmoPt))); resp.Coverage.Mixed {
		t.Fatalf("failed fill served as mixed: %+v", resp.Coverage)
	}
	dhmCalls := src.calls.Load()
	failing.Store(false)
	for i := 0; i < 2; i++ {
		w := env.get(pathQuery(cphPt, malmoPt))
		resp := decodeProfile(t, w)
		if w.Header().Get("X-CoreScope-Cache") != "hit" || resp.Coverage.State != "full" || !resp.Coverage.Mixed || resp.AttributionFill == "" {
			t.Fatalf("view %d: cache %q, coverage %+v", i, w.Header().Get("X-CoreScope-Cache"), resp.Coverage)
		}
		checkMixed(t, resp)
	}
	if f.count() != 2 || src.calls.Load() != dhmCalls {
		t.Fatalf("%d fills (want the failure and one retry), %d DHM calls (want %d)", f.count(), src.calls.Load(), dhmCalls)
	}
}

// The retry is upstream work, so it is charged to the per-client budget
// like a miss; a client over it still gets the cached partial profile,
// not a 429, and no fill is tried.
func TestTerrainFill_RetryOverBudgetServesCachedPartial(t *testing.T) {
	f := &scriptedFill{fn: func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
		return copdem.Result{}, dhm.FetchError{Reason: "status 503"}
	}}
	pc := dhm.ProfileConfig{RateLimitPerMin: intp(1), ForeignFill: dhm.ForeignFillConfig{Enabled: true}}
	env := newTerrainEnvWith(t, newFakeElevation(t, flatElev(fillDHMHeight)), pc, nil, func(d *terrainProfilerDeps) { d.fill = f })
	for i := 0; i < 2; i++ {
		w := env.get(pathQuery(cphPt, malmoPt))
		if resp := decodeProfile(t, w); w.Code != 200 || resp.Coverage.State != "partial" {
			t.Fatalf("request %d: %d, coverage %+v", i, w.Code, resp.Coverage)
		}
	}
	if f.count() != 1 {
		t.Fatalf("%d fills, want only the first request's", f.count())
	}
}

// A DHM failure still fails the profile as before; the fill does not
// rescue it.
func TestTerrainFill_DHMFailureStillFails(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	src := newFakeElevation(t, flatElev(fillDHMHeight))
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error { return dhm.FetchError{Reason: "status 500"} }
	pc := dhm.ProfileConfig{ForeignFill: dhm.ForeignFillConfig{Enabled: true}}
	env := newTerrainEnvWith(t, src, pc, nil, func(d *terrainProfilerDeps) { d.fill = fillClient(b) })
	if w := env.get(pathQuery(cphPt, malmoPt)); w.Code != http.StatusBadGateway {
		t.Fatalf("status %d", w.Code)
	}
}

// Over the budget even at the ½ overview: no fill, partial, and the
// bucket saw no more than the budget.
func TestTerrainFill_OverBudgetDegrades(t *testing.T) {
	b := newFillBucket(t, copdemtest.Layout{W: 1024, H: 1024, Block: 16})
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{})
	resp := decodeProfile(t, env.get(pathQuery("55.60000,12.70000", offsetPt(55.60, 12.70, 40000*math.Sin(80*math.Pi/180), 40000*math.Cos(80*math.Pi/180)))))
	if resp.Coverage.State != "partial" || resp.Coverage.Mixed {
		t.Fatalf("coverage %+v", resp.Coverage)
	}
	if got := b.Requests.Load(); got > dhm.MaxCallsPerModel {
		t.Fatalf("%d requests", got)
	}
	if !strings.Contains(env.logs.String(), "foreign fill failed (over budget)") {
		t.Fatalf("log %q", env.logs.String())
	}
}

// When full resolution is over budget the ½ overview is used, and the
// response says so (#738 decision 7).
func TestTerrainFill_OverviewResolution(t *testing.T) {
	b := newFillBucket(t, fillDense)
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{})
	end := offsetPt(55.60, 12.75, 40000*math.Sin(100*math.Pi/180), 40000*math.Cos(100*math.Pi/180))
	resp := decodeProfile(t, env.get(pathQuery("55.60000,12.75000", end)))
	if resp.Coverage.State != "full" || !resp.Coverage.Mixed {
		t.Fatalf("coverage %+v", resp.Coverage)
	}
	for _, r := range resp.Coverage.Fill {
		if r.ResolutionM != 60 {
			t.Fatalf("fill range %+v, want the 60 m overview", r)
		}
	}
	if got := b.Requests.Load(); got > dhm.MaxCallsPerModel {
		t.Fatalf("%d requests", got)
	}
}

// A cached mixed profile is served as such without any upstream call,
// and the fill state is part of the cache key.
func TestTerrainFill_CacheHit(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, src := newFillEnv(t, b, dhm.ProfileConfig{})
	first := env.get(pathQuery(cphPt, malmoPt))
	reqs, calls := b.Requests.Load(), src.calls.Load()
	second := env.get(pathQuery(cphPt, malmoPt, "ha=20"))
	if got := second.Header().Get("X-CoreScope-Cache"); got != "hit" {
		t.Fatalf("cache %q", got)
	}
	if b.Requests.Load() != reqs || src.calls.Load() != calls {
		t.Fatal("a cache hit went upstream")
	}
	r1, r2 := decodeProfile(t, first), decodeProfile(t, second)
	if !r2.Coverage.Mixed || r2.AttributionFill == "" || len(r2.Coverage.Fill) != len(r1.Coverage.Fill) || r2.Coverage.Fill[0] != r1.Coverage.Fill[0] {
		t.Fatalf("hit %+v, miss %+v", r2.Coverage, r1.Coverage)
	}
	k := terrainKey{A: [2]int64{1, 2}, B: [2]int64{3, 4}, Samples: 10, StepCm: 2500}
	kf := k
	kf.Fill = true
	if k.String() == kf.String() {
		t.Fatal("the fill is not part of the cache key")
	}
}

// Two different profiles that need the same blocks, fetched at once,
// read each block once.
func TestTerrainFill_ConcurrentProfilesShareBlocks(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{})
	var wg sync.WaitGroup
	for i, a := range []string{cphPt, "55.68000,12.60000", "55.66000,12.58000"} {
		wg.Add(1)
		go func(i int, a string) {
			defer wg.Done()
			w := env.get(pathQuery(a, malmoPt), func(r *http.Request) { r.RemoteAddr = "198.51.100." + string(rune('1'+i)) + ":1000" })
			if resp := decodeProfile(t, w); !resp.Coverage.Mixed {
				t.Errorf("profile %d not mixed", i)
			}
		}(i, a)
	}
	wg.Wait()
	seen := map[string]int{}
	for _, r := range b.Log() {
		seen[r.Path+" "+r.Header.Get("Range")]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("%s read %d times", k, n)
		}
	}
}

// A client that leaves cancels the fill with the profile: the bucket's
// request ends, and every admission slot is free again.
func TestTerrainFill_ClientGoneCancelsFill(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	hung := make(chan struct{}, 8)
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		hung <- struct{}{}
		<-r.Context().Done()
		return true
	}
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.get(pathQuery(cphPt, malmoPt), func(r *http.Request) { *r = *r.WithContext(ctx) })
	}()
	select {
	case <-hung:
	case <-time.After(5 * time.Second):
		t.Fatal("the fill never reached the bucket")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after the client left")
	}
	waitUntil(t, "profile admission free", func() bool { return len(env.p.admit) == 0 && env.p.flight.Pending() == 0 })
	if strings.Contains(env.logs.String(), "foreign fill failed") {
		t.Fatalf("a client leaving was logged as a fill failure: %q", env.logs.String())
	}
}

// The DHM token never reaches the Copernicus host: the real WCS client
// with a token, and a fill alongside it.
func TestTerrainFill_TokenNeverReachesCopernicus(t *testing.T) {
	var sawToken sync.Once
	gotToken := false
	up := wcsUpstream(t, func(w http.ResponseWriter, r *http.Request, width, height int) {
		if r.Header.Get("token") == dhmTestToken {
			sawToken.Do(func() { gotToken = true })
		}
		tiffAnswer(w, r, width, height)
	})
	logs := &dhmLogSink{}
	client := dhm.NewClient(dhm.Config{Enabled: true, Token: dhmTestToken}, testDHMOptions(up.srv.URL+"/", logs).client)
	b := newFillBucket(t, fillTenth)
	pc := dhm.ProfileConfig{ForeignFill: dhm.ForeignFillConfig{Enabled: true}}
	env := newTerrainEnvWith(t, dhm.NewWCSSource(client), pc, nil, func(d *terrainProfilerDeps) { d.fill = fillClient(b) })
	resp := decodeProfile(t, env.get(pathQuery(cphPt, malmoPt)))
	if !resp.Coverage.Mixed || !gotToken {
		t.Fatalf("mixed %v, DHM saw its token %v", resp.Coverage.Mixed, gotToken)
	}
	if len(b.Log()) == 0 {
		t.Fatal("no fill request")
	}
	for _, r := range b.Log() {
		for k, vs := range r.Header {
			if strings.EqualFold(k, "token") || strings.EqualFold(k, "authorization") {
				t.Fatalf("header %s sent to Copernicus", k)
			}
			for _, v := range vs {
				if strings.Contains(v, dhmTestToken) {
					t.Fatalf("the DHM token sent to Copernicus in %s", k)
				}
			}
		}
		if strings.Contains(r.Path+r.RawQuery, dhmTestToken) {
			t.Fatal("the DHM token in a Copernicus URL")
		}
	}
}

// The per-client budget is charged once per uncached profile, whatever
// the fill does.
func TestTerrainFill_ChargedOncePerProfile(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{RateLimitPerMin: intp(2)})
	for i, a := range []string{cphPt, "55.68000,12.60000"} {
		if w := env.get(pathQuery(a, malmoPt)); w.Code != 200 {
			t.Fatalf("profile %d: %d", i, w.Code)
		}
	}
	if w := env.get(pathQuery("55.66000,12.58000", malmoPt)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("third profile: %d, want 429", w.Code)
	}
}

// The response shape M2 builds on (AGENTS.md rule 4): named fields, as
// served.
func TestTerrainFill_ResponseShape(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	env, _ := newFillEnv(t, b, dhm.ProfileConfig{})
	w := env.get(pathQuery(cphPt, malmoPt, "model=dtm"))
	var raw struct {
		Coverage struct {
			State     string           `json:"state"`
			Pct       float64          `json:"pct"`
			FirstGapM *float64         `json:"firstGapM"`
			Mixed     *bool            `json:"mixed"`
			Fill      []map[string]any `json:"fill"`
		} `json:"coverage"`
		AttributionFill *string `json:"attributionFill"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Coverage.Mixed == nil || !*raw.Coverage.Mixed || raw.AttributionFill == nil || len(raw.Coverage.Fill) == 0 {
		t.Fatalf("shape %s", w.Body.String()[:400])
	}
	for _, r := range raw.Coverage.Fill {
		if len(r) != 4 || r["fromM"] == nil || r["toM"] == nil || r["source"] != "copdem-glo30" || r["resolutionM"] != float64(30) {
			t.Fatalf("fill range %v", r)
		}
	}
	var cov map[string]json.RawMessage
	var top map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &top)
	json.Unmarshal(top["coverage"], &cov)
	t.Logf("coverage %s", top["coverage"])
	t.Logf("attributionFill %s", top["attributionFill"])
	// A model=both profile has the same shape plus the unknown surface.
	if _, ok := cov["mixed"]; !ok {
		t.Fatal("coverage.mixed missing")
	}
}

func TestTerrainFill_Settings(t *testing.T) {
	logs := &dhmLogSink{}
	def := resolveTerrainSettings(dhm.ProfileConfig{}, logs.logf).fill
	if def.enabled || def.blockCacheBytes != 32<<20 || def.timeout != 10*time.Second {
		t.Fatalf("defaults %+v", def)
	}
	set := resolveTerrainSettings(dhm.ProfileConfig{ForeignFill: dhm.ForeignFillConfig{Enabled: true, BlockCacheMB: intp(64), TimeoutMs: intp(2500)}}, logs.logf)
	if !set.fill.enabled || set.fill.blockCacheBytes != 64<<20 || set.fill.timeout != 2500*time.Millisecond || logs.String() != "" {
		t.Fatalf("set %+v, logs %q", set.fill, logs.String())
	}
	for _, bad := range []dhm.ForeignFillConfig{{BlockCacheMB: intp(0)}, {BlockCacheMB: intp(257)}, {TimeoutMs: intp(499)}, {TimeoutMs: intp(10001)}} {
		l := &dhmLogSink{}
		got := resolveTerrainSettings(dhm.ProfileConfig{ForeignFill: bad}, l.logf).fill
		if got != def || !strings.Contains(l.String(), "map.dhm.profile.foreignFill.") {
			t.Fatalf("%+v: %+v, log %q", bad, got, l.String())
		}
	}
	// The client's limits follow: its admission holds every admitted
	// profile's whole budget.
	o := fillOptions(set)
	if o.BlockCacheBytes != 64<<20 || o.Timeout != 2500*time.Millisecond || o.MaxPending != set.maxInFlight*dhm.MaxCallsPerModel || o.Transport != nil {
		t.Fatalf("client options %+v", o)
	}
}

// The production wiring: the fill exists exactly when configured, as a
// copdem client on its own (no DHM token anywhere near it).
func TestTerrainFill_ProductionWiring(t *testing.T) {
	proxy := newDHMProxy(dhm.Config{Enabled: true, Token: dhmTestToken}, defaultDHMProxyOptions())
	cfg := &Config{}
	cfg.DHM = dhm.Config{Enabled: true, Token: dhmTestToken, Profile: dhm.ProfileConfig{Enabled: true}}
	if p := newTerrainProfilerFromConfig(cfg, proxy, nil); p == nil || p.fill != nil {
		t.Fatal("fill on without foreignFill.enabled")
	}
	cfg.DHM.Profile.ForeignFill.Enabled = true
	p := newTerrainProfilerFromConfig(cfg, proxy, nil)
	if p == nil {
		t.Fatal("no profiler")
	}
	if _, ok := p.fill.(*copdem.Client); !ok {
		t.Fatalf("fill %T", p.fill)
	}
}

// terrainFillRanges: runs of filled samples, merged across the smallest
// DHM gaps when there are more than terrainMaxFillRanges.
func TestTerrainFillRanges(t *testing.T) {
	const step = 10.0
	mk := func(filled ...[2]int) []bool {
		f := make([]bool, 200)
		for _, r := range filled {
			for i := r[0]; i <= r[1]; i++ {
				f[i] = true
			}
		}
		return f
	}
	got := terrainFillRanges(mk([2]int{3, 5}, [2]int{9, 9}, [2]int{190, 199}), step, 60)
	want := []TerrainFillRange{
		{FromM: 30, ToM: 50, Source: copdem.Source, ResolutionM: 60},
		{FromM: 90, ToM: 90, Source: copdem.Source, ResolutionM: 60},
		{FromM: 1900, ToM: 1990, Source: copdem.Source, ResolutionM: 60},
	}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d %+v, want %+v", i, got[i], want[i])
		}
	}
	if terrainFillRanges(mk(), step, 30) != nil {
		t.Fatal("ranges without a filled sample")
	}
	// Every other sample filled: 100 runs, merged to the cap; the merged
	// ranges still start at the first and end at the last filled sample.
	f := make([]bool, 200)
	for i := 0; i < 200; i += 2 {
		f[i] = true
	}
	got = terrainFillRanges(f, step, 30)
	if len(got) != terrainMaxFillRanges || got[0].FromM != 0 || got[len(got)-1].ToM != 1980 {
		t.Fatalf("%d ranges, %+v … %+v", len(got), got[0], got[len(got)-1])
	}
	// The smallest gaps are merged first: 3 + 64 runs, every gap 2 but
	// two (48 and 50), so the first stretches join and those two stay.
	f = make([]bool, 400)
	f[0], f[2], f[50] = true, true, true
	for i := 100; i < 100+2*terrainMaxFillRanges; i += 2 {
		f[i] = true
	}
	got = terrainFillRanges(f, step, 30)
	if len(got) != terrainMaxFillRanges || got[0] != (TerrainFillRange{FromM: 0, ToM: 20, Source: copdem.Source, ResolutionM: 30}) {
		t.Fatalf("%d ranges, first %+v", len(got), got[0])
	}
}

// The cached elevation carries the fill level; a flag it does not know is
// refused, like any entry that does not match its key.
func TestTerrainElevationEncodesFill(t *testing.T) {
	for _, level := range []int{fillNone, fillFull, fillOverview} {
		for _, both := range []bool{false, true} {
			e := terrainElevation{dtm: []float32{1, 2, 3}, fill: level}
			if both {
				e.dsm = []float32{4, 5, 6}
			}
			got, err := decodeTerrainElevation(e.encode(), 3, both, false)
			if err != nil || got.fill != level || got.dtm[2] != 3 || (both && got.dsm[0] != 4) {
				t.Fatalf("level %d both %v: %+v %v", level, both, got, err)
			}
		}
	}
	got, err := decodeTerrainElevation(terrainElevation{dtm: []float32{1, 2, 3}, fillFailed: true}.encode(), 3, false, false)
	if err != nil || !got.fillFailed || got.fill != fillNone {
		t.Fatalf("failed fill: %+v %v", got, err)
	}
	enc := terrainElevation{dtm: []float32{1, 2, 3}}.encode()
	// 6: fill level 3; 10, 12: a failed fill with a fill level; 0x10: the
	// classification (#730 M2), which this entry does not carry; 0x20,
	// 0x40, 0x80: unknown bits (#786 review F3).
	for _, flag := range []byte{6, 10, 12, 0x10, 0x20, 0x40, 0x80} {
		bad := append([]byte{}, enc...)
		bad[4] = flag
		if _, err := decodeTerrainElevation(bad, 3, false, false); err == nil {
			t.Fatalf("flag %#x accepted", flag)
		}
	}
}

// The fill runs beside the DHM rasters, not after them: a slow fill and a
// slow DHM overlap.
func TestTerrainFill_RunsAlongsideDHM(t *testing.T) {
	src := newFakeElevation(t, flatElev(fillDHMHeight))
	started := make(chan struct{}, 64)
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error {
		started <- struct{}{}
		return nil
	}
	f := &blockingFill{started: started}
	pc := dhm.ProfileConfig{ForeignFill: dhm.ForeignFillConfig{Enabled: true}}
	env := newTerrainEnvWith(t, src, pc, nil, func(d *terrainProfilerDeps) { d.fill = f })
	w := env.get(pathQuery(cphPt, malmoPt))
	if w.Code != 200 || !f.sawDHM {
		t.Fatalf("status %d, fill saw DHM running: %v", w.Code, f.sawDHM)
	}
}

// blockingFill waits until DHM has started before it answers.
type blockingFill struct {
	started chan struct{}
	sawDHM  bool
}

func (f *blockingFill) Fill(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
	select {
	case <-f.started:
		f.sawDHM = true
	case <-time.After(3 * time.Second):
	}
	return copdem.Result{}, errors.New("no data")
}

// scriptedFill answers every fill with fn's result and counts the calls.
type scriptedFill struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, p *dhm.Plan) (copdem.Result, error)
}

func (f *scriptedFill) Fill(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.fn(ctx, p)
}

func (f *scriptedFill) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newScriptedEnv(t *testing.T, fn func(ctx context.Context, p *dhm.Plan) (copdem.Result, error)) (*terrainEnv, *scriptedFill) {
	t.Helper()
	f := &scriptedFill{fn: fn}
	pc := dhm.ProfileConfig{ForeignFill: dhm.ForeignFillConfig{Enabled: true}}
	env := newTerrainEnvWith(t, newFakeElevation(t, flatElev(fillDHMHeight)), pc, nil, func(d *terrainProfilerDeps) { d.fill = f })
	return env, f
}

// everywhere is a fill that claims a height for every sample.
func everywhere(h float32) func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
	return func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
		v := make([]float32, len(p.E))
		for i := range v {
			v[i] = h
		}
		return copdem.Result{Values: v, Filled: len(v)}, nil
	}
}

// A fill can only ever complete the profile: a height it returns for a
// sample DHM covers is ignored.
func TestTerrainFill_NeverOverridesDHM(t *testing.T) {
	env, f := newScriptedEnv(t, everywhere(999))
	resp := decodeProfile(t, env.get(pathQuery(cphPt, malmoPt)))
	if f.count() != 1 || resp.Coverage.State != "full" || !resp.Coverage.Mixed {
		t.Fatalf("calls %d, coverage %+v", f.count(), resp.Coverage)
	}
	checkMixed(t, resp) // every non-fill sample is still DHM's 5 m
	if *resp.A.GroundM != fillDHMHeight || *resp.B.GroundM != 999 {
		t.Fatalf("grounds %v / %v", *resp.A.GroundM, *resp.B.GroundM)
	}
}

// A path DHM covers end to end never even starts the fill.
func TestTerrainFill_NotStartedWhenDHMCoversAll(t *testing.T) {
	env, f := newScriptedEnv(t, everywhere(999))
	resp := decodeProfile(t, env.get(pathQuery(aarhusPt, silkeborgPt)))
	if f.count() != 0 || resp.Coverage.Mixed {
		t.Fatalf("calls %d, coverage %+v", f.count(), resp.Coverage)
	}
}

// A fill whose answer does not match the plan is a failure, not a panic
// and not a half fill.
func TestTerrainFill_MismatchedResultIsAFailure(t *testing.T) {
	for _, extra := range []int{-1, 1} {
		env, _ := newScriptedEnv(t, func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
			return copdem.Result{Values: make([]float32, len(p.E)+extra)}, nil
		})
		resp := decodeProfile(t, env.get(pathQuery(cphPt, malmoPt)))
		if resp.Coverage.State != "partial" || resp.Coverage.Mixed || !strings.Contains(env.logs.String(), "foreign fill failed (fill mismatch)") {
			t.Fatalf("extra %d: coverage %+v, log %q", extra, resp.Coverage, env.logs.String())
		}
	}
}

// fetch always stops its fill on the way out, also when DHM fails and the
// caller's context lives on.
func TestTerrainFill_StoppedWhenDHMFails(t *testing.T) {
	stopped := make(chan struct{})
	f := &scriptedFill{fn: func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
		<-ctx.Done()
		close(stopped)
		return copdem.Result{}, ctx.Err()
	}}
	src := newFakeElevation(t, flatElev(fillDHMHeight))
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error { return dhm.FetchError{Reason: "status 500"} }
	p := newTerrainProfiler(dhm.ProfileConfig{Enabled: true, ForeignFill: dhm.ForeignFillConfig{Enabled: true}}, nil,
		terrainProfilerDeps{src: src, fill: f, logf: func(string, ...interface{}) {}})
	_, _, plan := fillBenchPath()
	if _, err := p.fetch(context.Background(), &plan, false); err == nil {
		t.Fatal("DHM failure not returned")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the fill kept running after fetch returned")
	}
}

// A mixed profile has no surface verdict, whatever the DSM holds: there is
// no surface model abroad, so a DSM value there could only be wrong.
func TestTerrainFill_SurfaceUnknownWhenMixed(t *testing.T) {
	pa, pb, plan := fillBenchPath()
	n := len(plan.E)
	e := terrainElevation{dtm: make([]float32, n), dsm: make([]float32, n), fill: fillFull}
	for i := range e.dsm {
		e.dsm[i] = 1 // complete and low: clear, if it were assessed
	}
	p := newTerrainProfiler(dhm.ProfileConfig{Enabled: true}, nil, terrainProfilerDeps{src: newFakeElevation(nil, flatElev(0))})
	ends := [2]terrainEnd{{pt: pa, kind: "point"}, {pt: pb, kind: "point"}}
	r := p.buildResponse(terrainQuery{both: true}, ends, pa, pb, &plan, e)
	if !r.Coverage.Mixed || r.Coverage.State != "full" || r.Verdict.Surface == nil || r.Verdict.Surface.State != "unknown" {
		t.Fatalf("coverage %+v, surface %+v", r.Coverage, r.Verdict.Surface)
	}
	e.fill = fillNone
	if r := p.buildResponse(terrainQuery{both: true}, ends, pa, pb, &plan, e); r.Coverage.Mixed || r.Verdict.Surface.State == "unknown" {
		t.Fatalf("not mixed: coverage %+v, surface %+v", r.Coverage, r.Verdict.Surface)
	}
}

// #738 decision 2: the Copernicus liability notice (licence Art. 6(c))
// is in the README and in the config docs, verbatim, beside both
// credits; and the example config documents the fill, off, at the
// defaults (TestTerrainProfile_ExampleConfigMatchesDefaults compares the
// values).
func TestTerrainFill_LicenceNoticeDocumented(t *testing.T) {
	for _, c := range []struct {
		file  string
		texts []string
	}{
		{"README.md", []string{copdem.LiabilityNotice, copdem.AttributionDerived, dhm.Attribution, "map.dhm.profile.foreignFill.enabled"}},
		{"config.example.json", []string{copdem.LiabilityNotice, copdem.Host}},
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", c.file))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range c.texts {
			if !strings.Contains(string(b), s) {
				t.Errorf("%s does not contain %q", c.file, s)
			}
		}
	}
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	ff := cfg.DHM.Profile.ForeignFill
	if ff.Enabled || ff.BlockCacheMB == nil || ff.TimeoutMs == nil {
		t.Fatalf("example map.dhm.profile.foreignFill %+v: must be documented and off", ff)
	}
}
