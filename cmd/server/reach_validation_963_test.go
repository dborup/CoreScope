package main

// #963 M0 — tests and opt-in runs of the candidate-model harness
// (reach_validation_963_harness_test.go). CI runs the fixture mechanics on
// synthetic terrain; the real-data and GLO-30 runs are opt-in:
//
//	CORESCOPE_REACH_VALIDATION_DB / _CONFIG / _OUT  the instance run (#941's variables)
//	CORESCOPE_REACH_VALIDATION_963_GLO30=1           the fixture and the reach radius over real GLO-30
//	CORESCOPE_REACH_VALIDATION_963_OUT=<dir>         where the fixture and GLO-30 runs write reports
//	CORESCOPE_REACH_VALIDATION_963_POINT=lat,lon     the reach-radius point (default 56.136,8.975)
//	CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT     one DHM tile attempt (default 60s; also the DHM client's timeout)
//	CORESCOPE_REACH_VALIDATION_963_FETCH_RETRIES     retries of a failed tile before it has no terrain (default 3)
//	CORESCOPE_REACH_VALIDATION_963_PROGRESS_EVERY    a progress line every this many disks (default 25; 0: none)

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
)

// syntheticCanopy963 is a synthetic land cover anywhere: forest 20 m tall
// and villages of 7 m on open land, a function of the coordinates alone.
func syntheticCanopy963(e, n float64) float32 {
	x, y := e/1000, n/1000
	switch {
	case math.Sin(x/2.3)*math.Cos(y/3.1) > 0.6:
		return 20
	case math.Cos(x/7.9+0.5)*math.Cos(y/5.3) > 0.85:
		return 7
	}
	return 0
}

func reach963OutDir(t *testing.T) string {
	if d := os.Getenv("CORESCOPE_REACH_VALIDATION_963_OUT"); d != "" {
		return d
	}
	return t.TempDir()
}

// TestReachValidation963_FixtureMechanics runs the candidate harness once
// on the fixture, over a synthetic DTM and a DSM with synthetic forest and
// villages on it, and checks the report: the database copy unchanged,
// every candidate scored over the same pairs as #941, today's delta
// identical to the #941 harness's, today's 25 dB a pure shift, the bands
// and height bands adding up, every clutter class seen, and each DTM and
// DSM tile fetched once.
func TestReachValidation963_FixtureMechanics(t *testing.T) {
	sc := reachFixtureScaleFromEnv()
	path := prepareReachFixture(t, sc)
	before := fileSHA(t, path)
	srv, c, db, src := reachValidationEnv(t, path, sc.resolutionM)
	src.surface = func(e, n float64) float32 { return rollingElev(e, n) + syntheticCanopy963(e, n) }
	// DTM and DSM share one per-minute upstream budget, and the full scale
	// needs more than the default 240 calls a minute for both.
	c.budget = newUpstreamBudget(4000, c.now)
	cs := surfaceCoverage963(c, reachValidationCoverageConfig(sc.resolutionM))
	if cs.budget != c.budget {
		t.Fatal("the surface coverage has its own upstream budget")
	}
	out := reach963OutDir(t)
	o := fixtureValidationOptions(t, src, sc.radiusM)
	o.outDir = out
	o.upstream = countReach963Upstream(c, cs)
	rep, err := runReachValidation963(context.Background(), srv, c, cs, db.conn, o, defaultGather963(o.sleep))
	if err != nil {
		t.Fatal(err)
	}
	if fileSHA(t, path) != before {
		t.Fatal("the harness changed the database copy")
	}
	if rep.Terrain.Upstream != src.calls.Load() {
		t.Fatalf("report counts %d upstream calls, the fake upstream saw %d", rep.Terrain.Upstream, src.calls.Load())
	}
	dtmCalls, dsmCalls := src.model["dtm"], src.model["dsm"]
	if dtmCalls == 0 || dtmCalls != dsmCalls || int64(dtmCalls+dsmCalls) != src.calls.Load() || dtmCalls > rep.Terrain.Tiles {
		t.Fatalf("upstream: %d DTM and %d DSM rasters for %d tiles", dtmCalls, dsmCalls, rep.Terrain.Tiles)
	}
	if rep.Terrain.SurfaceSource != "dhm surface (dhm_overflade)" || rep.Terrain.SurfaceTiles != rep.Terrain.Tiles {
		t.Fatalf("terrain %+v", rep.Terrain)
	}
	// Every upstream raster went through the harness's own fetch, once.
	if f := rep.Terrain.Fetch; f.DTM.Fetched != dtmCalls || f.DSM.Fetched != dsmCalls || f.failed() || f.DTM.Retried+f.DSM.Retried != 0 || rep.Terrain.Partial {
		t.Fatalf("fetches %+v for %d DTM and %d DSM rasters", f, dtmCalls, dsmCalls)
	}
	ec := rep.EndClasses
	if ec.Open == 0 || ec.Suburban == 0 || ec.Urban == 0 {
		t.Fatalf("end classes %+v: the synthetic cover has all three", ec)
	}
	if len(rep.Candidates) != len(candidates963) {
		t.Fatalf("%d candidates", len(rep.Candidates))
	}
	p := rep.Pairs
	// The CI scale's disks hold every partner: no pair without terrain.
	if !sc.full && p.NoTerrain != 0 {
		t.Fatalf("%d pairs without terrain", p.NoTerrain)
	}
	if p.Positives < sc.seeds || p.Negatives < 100 || p.Positives+p.Negatives != p.Scored {
		t.Fatalf("pairs %+v", p)
	}
	byID := map[string]reachValidation963Candidate{}
	for i, cand := range rep.Candidates {
		if cand.ID != candidates963[i] {
			t.Fatalf("candidate %d is %q", i, cand.ID)
		}
		byID[cand.ID] = cand
		o := cand.Overall
		if o.Positives != p.Positives || o.Negatives != p.Negatives || o.AUC == nil || o.HitRate == nil || o.MedianSNROffsetDb == nil {
			t.Fatalf("%s overall %+v", cand.ID, o)
		}
		for _, split := range [][]reachValidation963Band{cand.Bands, cand.Heights} {
			sp, sn := 0, 0
			for _, b := range split {
				sp += b.Positives
				sn += b.Negatives
			}
			if sp != o.Positives || sn != o.Negatives {
				t.Fatalf("%s: a split holds %d/%d of %d/%d", cand.ID, sp, sn, o.Positives, o.Negatives)
			}
		}
	}
	if len(byID[cand963Delta].Heights) != len(reachValidation963Heights) {
		t.Fatalf("height bands %+v", byID[cand963Delta].Heights)
	}

	// Today's delta is the #941 harness's delta, pair for pair.
	o941 := fixtureValidationOptions(t, src, sc.radiusM)
	rep941, err := runReachValidation(context.Background(), srv, c, db.conn, o941)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := byID[cand963Delta].Overall, rep941.Models[2].Overall; !metricsEqual963(got, want) {
		t.Fatalf("963 delta %+v, 941 delta %+v", got, want)
	}
	// Today's 25 dB shifts every margin: the same ranking, fewer reached,
	// the SNR offset 25 dB higher.
	d, e := byID[cand963Delta].Overall, byID[cand963DeltaEnv].Overall
	if *d.AUC != *e.AUC || *e.HitRate > *d.HitRate || *e.FalseReachRate > *d.FalseReachRate ||
		math.Abs(*e.MedianSNROffsetDb-*d.MedianSNROffsetDb-envLoss963Db) > 1e-3 {
		t.Fatalf("delta %+v, delta+env25 %+v", d, e)
	}

	md, err := os.ReadFile(filepath.Join(out, "reach-validation-963.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range append([]string{"# Predicted reach candidates (#963 M0)", "## Overall", "## ROC-AUC per distance band",
		"## Median SNR offset per distance band (raw / capped)", "## Observed SNR", "## Hit rate per lower antenna height", "## Clutter classes at the ends"}, candidates963...) {
		if !strings.Contains(string(md), h) {
			t.Fatalf("markdown lacks %q:\n%s", h, md)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "reach-validation-963.json")); err != nil {
		t.Fatal(err)
	}
}

// metricsEqual963 compares two metric rows value for value.
func metricsEqual963(a, b reachValidationMetrics) bool {
	eq := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Positives == b.Positives && a.Negatives == b.Negatives && a.SNRPairs == b.SNRPairs && eq(a.HitRate, b.HitRate) &&
		eq(a.FalseReachRate, b.FalseReachRate) && eq(a.AUC, b.AUC) && eq(a.Spearman, b.Spearman) && eq(a.MedianSNROffsetDb, b.MedianSNROffsetDb)
}

// TestReachValidation963_ReachRadiusReport gathers a reach-radius disk
// through the harness's coverages (synthetic terrain and canopy) and
// checks the table: every candidate at both budgets, the radius never
// growing as the budget shrinks, today's 25 dB the same as delta at 25 dB
// less, the markdown naming every candidate.
func TestReachValidation963_ReachRadiusReport(t *testing.T) {
	c, src := reachValidationCoverage(t, 60)
	src.surface = func(e, n float64) float32 { return rollingElev(e, n) + syntheticCanopy963(e, n) }
	cs := surfaceCoverage963(c, reachValidationCoverageConfig(60))
	rep, err := reachRadiusAt963(context.Background(), c, cs, reachFixtureOrigin, 10, 2, 10000, 8, defaultGather963(func(time.Duration) { t.Fatal("the upstream budget ran out") }))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != len(candidates963) || rep.Bearings != 8 || rep.TxClass == "" {
		t.Fatalf("report %+v", rep)
	}
	byID := map[string]reachRadius963Row{}
	for i, r := range rep.Rows {
		if r.Candidate != candidates963[i] {
			t.Fatalf("row %d is %q", i, r.Candidate)
		}
		if !(r.MedianKm[1] <= r.MedianKm[0] && r.MinKm[0] <= r.MedianKm[0] && r.MedianKm[0] <= r.MaxKm[0]) {
			t.Fatalf("row %+v", r)
		}
		byID[r.Candidate] = r
	}
	if byID[cand963DeltaEnv].MedianKm[0] != byID[cand963Delta].MedianKm[1] {
		t.Fatalf("delta+env25 %+v, delta %+v", byID[cand963DeltaEnv], byID[cand963Delta])
	}
	md := rep.markdown()
	for _, c := range candidates963 {
		if !strings.Contains(md, "| "+c+" |") {
			t.Fatalf("markdown lacks %s:\n%s", c, md)
		}
	}
}

// TestReachValidation963_RealDB runs the candidate harness against an
// instance's database copy with #941's variables
// (docs/predicted-reach-validation.md), over DHM's terrain and surface
// models (or GLO-30 alone), and writes reach-validation-963.{json,md} and
// the reach radius of CORESCOPE_REACH_VALIDATION_963_POINT,
// reach-radius-963.{json,md}, to CORESCOPE_REACH_VALIDATION_OUT. It skips
// without CORESCOPE_REACH_VALIDATION_DB.
func TestReachValidation963_RealDB(t *testing.T) {
	g, err := gather963FromEnv(t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	// The DHM client's own per-request timeout is the attempt's too.
	srv, c, db, cfg, o := openRealReachValidationTimeout(t, g.timeout)
	pt, err := reachPoint963()
	if err != nil {
		t.Fatal(err)
	}
	cs := surfaceCoverage963(c, cfg.Coverage)
	o.upstream = countReach963Upstream(c, cs)
	g.upstream = o.upstream
	rep, err := runReachValidation963(context.Background(), srv, c, cs, db.conn, o, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("report in %s: %d scored pairs (%d positives), %d without terrain, %s; tile fetches %s",
		o.outDir, rep.Pairs.Scored, rep.Pairs.Positives, rep.Pairs.NoTerrain, upstreamText(o.upstream()), rep.Terrain.Fetch.text())
	writeReachRadius963(t, c, cs, pt, o.terrainSource+map[bool]string{true: " + dhm surface", false: ""}[cs != nil], o.upstream, o.outDir, g)
}

// TestReachValidation963_GLO30 is the opt-in run over real terrain without
// a token (CORESCOPE_REACH_VALIDATION_963_GLO30=1): the Copernicus GLO-30
// tiles the foreign fill reads, keyless, for the reach radius at
// CORESCOPE_REACH_VALIDATION_963_POINT and for the fixture moved to
// Jutland (at its CI scale, or the full one with
// CORESCOPE_REACH_VALIDATION_FULL_FIXTURE=1). GLO-30 is a surface model,
// so there is no separate DSM: the DSM candidates equal delta and every end
// is open. Reports go to CORESCOPE_REACH_VALIDATION_963_OUT.
func TestReachValidation963_GLO30(t *testing.T) {
	if os.Getenv("CORESCOPE_REACH_VALIDATION_963_GLO30") != "1" {
		t.Skip("CORESCOPE_REACH_VALIDATION_963_GLO30 not set: reads public Copernicus tiles, opt-in")
	}
	pt, err := reachPoint963()
	if err != nil {
		t.Fatal(err)
	}
	g, err := gather963FromEnv(t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	sc := reachFixtureScaleFromEnv()
	cc := reachValidationCoverageConfig(sc.resolutionM)
	set := resolveTerrainSettings(dhm.ProfileConfig{}, quietLogf)
	fo := fillOptions(set)
	fo.MaxPending = 64
	c := newTerrainCoverage(cc, nil, terrainCoverageDeps{fill: copdem.NewClient(fo), logf: quietLogf,
		observers: func() ([]Observer, error) { return nil, nil }})
	upstream := countReachUpstream(c)
	out := reach963OutDir(t)
	// The reach radius on the reach's default 30 m grid.
	cr := newTerrainCoverage(reachValidationCoverageConfig(30), nil, terrainCoverageDeps{fill: c.fill, logf: quietLogf,
		observers: func() ([]Observer, error) { return nil, nil }})
	writeReachRadius963(t, cr, nil, pt, "glo30", upstream, out, g)

	path := prepareReachFixture(t, sc)
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := &Server{cfg: &Config{}, db: db}
	srv.claims = newAntennaClaimService(srv.cfg, db.conn, "")
	o := defaultReachValidationOptions(sc.radiusM)
	o.minEdgeCount = 1
	o.terrainSource = "glo30 (fixture moved to Jutland)"
	o.outDir = out
	o.upstream = upstream
	rep, err := runReachValidation963(context.Background(), srv, c, nil, db.conn, o, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture over GLO-30: %d scored pairs, %d without terrain, %s", rep.Pairs.Scored, rep.Pairs.NoTerrain, upstreamText(upstream()))
}

// writeReachRadius963 computes the 10 m / 2 m reach radius at pt to 40 km
// over 72 bearings, gathering as g does, and writes
// reach-radius-963.{json,md} to dir.
func writeReachRadius963(t *testing.T, c, cs *terrainCoverage, pt dhm.LatLon, terrain string, upstream func() int64, dir string, g gather963) {
	t.Helper()
	start := time.Now()
	rr, err := reachRadiusAt963(context.Background(), c, cs, pt, 10, 2, 40000, 72, g)
	if err != nil {
		t.Fatal(err)
	}
	rr.Terrain, rr.Upstream = terrain, upstream()
	t.Logf("reach radius in %v:\n%s", time.Since(start).Round(time.Millisecond), rr.markdown())
	if err := writeReport963(dir, "reach-radius-963", rr, rr.markdown()); err != nil {
		t.Fatal(err)
	}
}

// TestReachValidation963_CappedSNROffset: the censored offset compares
// observed and predicted SNR both capped at the receiver's saturation, so
// a strong short link that reports the cap is no longer counted as tens of
// dB optimistic, and the observed SNR summary says how many sit at it.
func TestReachValidation963_CappedSNROffset(t *testing.T) {
	radio := CoverageRadio{TxPowerDbm: 22, AntennaGainDbi: 3, RxGainDbi: 3, SensitivityDbm: -130}
	noise := -115.0
	mk := func(loss, snr float64) *valPair {
		s := snr
		return &valPair{positive: true, snr: &s, noiseDbm: &noise, loss: map[dhm.PathModel]float64{"m": loss}}
	}
	// Predicted SNRs (28 − loss + 115): 63, 43, 23, 3 dB; observed 12, 11,
	// 10 (at the cap), −2.
	ps := []*valPair{mk(80, 12), mk(100, 11), mk(120, 10), mk(140, -2)}
	got := cappedSNROffset963(ps, "m", radio, 10)
	// Capped at 10: offsets 0, 0, 0, −5 → median 0. Uncapped: −51, −32,
	// −13, −5 → median −22.5.
	if got == nil || *got != 0 {
		t.Fatalf("capped offset %v, want 0", got)
	}
	if un := scoreMetrics(ps, "m", radio).MedianSNROffsetDb; un == nil || *un != -22.5 {
		t.Fatalf("uncapped offset %v, want -22.5", un)
	}
	st := observedSNR963(ps, 10)
	if st.Pairs != 4 || *st.MaxDb != 12 || *st.P50Db != 10.5 || *st.P95Db != 12 || st.AtOrAboveCap != 3 {
		t.Fatalf("observed SNR %+v", st)
	}
	if cappedSNROffset963(nil, "m", radio, 10) != nil {
		t.Fatal("no pairs, no offset")
	}
}

// TestReachValidation963_ScoredPairs: a pair without a loss under any
// scored model is counted as without terrain and left out; the rest are
// counted by label and source (the helper both harnesses share).
func TestReachValidation963_ScoredPairs(t *testing.T) {
	snr := 3.0
	mk := func(loss float64, positive, firmware bool, s *float64) *valPair {
		return &valPair{positive: positive, firmware: firmware, edge: positive && !firmware, snr: s,
			loss: map[dhm.PathModel]float64{"a": loss, "b": 120}}
	}
	all := []*valPair{mk(120, true, true, &snr), mk(math.NaN(), true, false, nil), mk(120, false, false, nil), mk(120, true, false, nil)}
	var c reachValidationPairCounts
	ps := scoredPairs(all, []dhm.PathModel{"a", "b"}, &c)
	want := reachValidationPairCounts{Scored: 3, Positives: 2, FromFirmware: 1, FromEdges: 1, Negatives: 1, NoTerrain: 1, WithSNR: 1}
	if len(ps) != 3 || c != want {
		t.Fatalf("%d scored, counts %+v, want %+v", len(ps), c, want)
	}
}

// tileOf963 is the coverage tile a raster request of the harness fetches.
func tileOf963(r dhm.RasterRequest, res float64) dhm.CoverageTile {
	span := dhm.CoverageTilePx * res
	return dhm.CoverageTile{X: int(math.Floor((r.BBox[0] + r.BBox[2]) / 2 / span)), Y: int(math.Floor((r.BBox[1] + r.BBox[3]) / 2 / span))}
}

// TestReachValidation963_TileTimeoutsDoNotAbort is the coordinator's
// failed run in small: the fake upstream lets the DSM tile under the
// network's origin time out on every attempt and every other DSM tile time
// out once. The run completes, retries with a doubling backoff, gives up
// on that one tile, counts it per source in the report's header, scores
// the pairs it does not touch and counts the rest as without terrain, and
// logs progress per disk. A harness that aborts on the first timeout
// fails here.
func TestReachValidation963_TileTimeoutsDoNotAbort(t *testing.T) {
	sc := reachFixtureScaleFromEnv()
	path := prepareReachFixture(t, sc)
	srv, c, db, src := reachValidationEnv(t, path, sc.resolutionM)
	src.surface = func(e, n float64) float32 { return rollingElev(e, n) + syntheticCanopy963(e, n) }
	c.budget = newUpstreamBudget(4000, c.now)
	cs := surfaceCoverage963(c, reachValidationCoverageConfig(sc.resolutionM))
	res := float64(sc.resolutionM)
	e0, n0 := dhm.ToUTM32(reachFixtureOrigin.Lat, reachFixtureOrigin.Lon)
	bad := dhm.CoverageTile{X: int(math.Floor(e0 / (dhm.CoverageTilePx * res))), Y: int(math.Floor(n0 / (dhm.CoverageTilePx * res)))}
	var mu sync.Mutex
	seen := map[dhm.CoverageTile]int{}
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error {
		if r.Model != dhm.ModelSurface {
			return nil
		}
		tile := tileOf963(r, res)
		mu.Lock()
		seen[tile]++
		first := seen[tile] == 1
		mu.Unlock()
		if tile == bad || first {
			<-ctx.Done() // a raster that never arrives: the attempt's own timeout
			return dhm.ContextFailure(ctx.Err())
		}
		return nil
	}
	out := t.TempDir()
	o := fixtureValidationOptions(t, src, sc.radiusM)
	o.outDir = out
	o.upstream = countReach963Upstream(c, cs)
	var slept []time.Duration
	var lines []string
	g := defaultGather963(func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() })
	g.timeout, g.retries, g.backoff = 50*time.Millisecond, 2, time.Second
	g.logf = func(f string, a ...interface{}) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	g.every, g.upstream = 1, o.upstream
	rep, err := runReachValidation963(context.Background(), srv, c, cs, db.conn, o, g)
	if err != nil {
		t.Fatalf("one slow DSM tile aborted the run: %v", err)
	}
	if seen[bad] != 3 || len(seen) < 2 {
		t.Fatalf("DSM attempts per tile %v: want 3 at the bad tile %v and others", seen, bad)
	}
	others := len(seen) - 1
	f := rep.Terrain.Fetch
	wantDSM := reachFetch963Stats{Fetched: others, Retried: 2 + others, Failed: 1, Reasons: map[string]int{"timeout": 3 + others}}
	if f.DSM.Fetched != wantDSM.Fetched || f.DSM.Retried != wantDSM.Retried || f.DSM.Failed != 1 || f.DSM.Reasons["timeout"] != wantDSM.Reasons["timeout"] || len(f.DSM.Reasons) != 1 {
		t.Fatalf("DSM fetches %+v, want %+v", f.DSM, wantDSM)
	}
	if f.DTM.Failed != 0 || f.DTM.Retried != 0 || f.DTM.Fetched != src.model[dhm.ModelTerrain] || len(f.DTM.Reasons) != 0 {
		t.Fatalf("DTM fetches %+v for %d DTM rasters", f.DTM, src.model[dhm.ModelTerrain])
	}
	if !rep.Terrain.Partial || rep.Terrain.FetchRetries != 2 || rep.Terrain.FetchTimeoutS != 0.05 {
		t.Fatalf("terrain %+v", rep.Terrain)
	}
	// The pairs over the failed tile have no terrain; the rest are scored,
	// and together they are every pair a clean run scores.
	p := rep.Pairs
	if p.NoTerrain == 0 || p.Scored == 0 || p.Positives == 0 || p.Scored+p.NoTerrain != p.Candidates {
		t.Fatalf("pairs %+v", p)
	}
	// Backoff 1 s, then 2 s, for the bad tile; 1 s for each flaky one.
	ones, twos := 0, 0
	for _, d := range slept {
		switch d {
		case time.Second:
			ones++
		case 2 * time.Second:
			twos++
		default:
			t.Fatalf("slept %v: %v", d, slept)
		}
	}
	if ones != 1+others || twos != 1 {
		t.Fatalf("backoffs %v", slept)
	}
	md, err := os.ReadFile(filepath.Join(out, "reach-validation-963.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "DSM " + f.DSM.text(); !strings.Contains(string(md), "DHM tile fetches (0.05 s timeout, up to 2 retries; a failed tile has no terrain): DTM ") ||
		!strings.Contains(string(md), want) || !strings.Contains(string(md), "1 failed (timeout ×") {
		t.Fatalf("header lacks the fetch counts %q:\n%.600s", want, md)
	}
	if _, err := os.Stat(filepath.Join(out, "reach-validation-963.json")); err != nil {
		t.Fatal(err)
	}
	progress, gaveUp := 0, 0
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "963: disk "):
			progress++
		case strings.Contains(l, fmt.Sprintf("DSM tile %d,%d given up after 3 attempts (timeout)", bad.X, bad.Y)):
			gaveUp++
		}
	}
	if gaveUp != 1 || progress == 0 || !strings.Contains(lines[len(lines)-1], "upstream calls") {
		t.Fatalf("%d progress lines, %d given up:\n%s", progress, gaveUp, strings.Join(lines, "\n"))
	}
}

// TestReachValidation963_FatalErrorSaysWhere: a failure no retry mends (a
// refused token) ends the run at once, with the stage, the disk, the tile,
// its bounding box and the upstream status in the error.
func TestReachValidation963_FatalErrorSaysWhere(t *testing.T) {
	sc := reachFixtureScaleFromEnv()
	path := prepareReachFixture(t, sc)
	srv, c, db, src := reachValidationEnv(t, path, sc.resolutionM)
	c.budget = newUpstreamBudget(4000, c.now)
	cs := surfaceCoverage963(c, reachValidationCoverageConfig(sc.resolutionM))
	src.hook = func(ctx context.Context, r dhm.RasterRequest) error {
		if r.Model == dhm.ModelSurface {
			return dhm.FetchError{Reason: "status 403"}
		}
		return nil
	}
	o := fixtureValidationOptions(t, src, sc.radiusM)
	g := defaultGather963(func(time.Duration) { t.Error("a refused token was retried") })
	_, err := runReachValidation963(context.Background(), srv, c, cs, db.conn, o, g)
	var te tileError963
	if !errors.As(err, &te) || te.stage != stageDSM963 || te.attempts != 1 {
		t.Fatalf("error %v", err)
	}
	for _, want := range []string{"disk 1/", "DSM fetch, disk of ", "DSM tile ", "(EPSG:25832 bbox ", "attempt 1: status 403"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

// TestReachValidation963_Retryable: every upstream failure is retried and
// then left without terrain, except a refused token and a request the
// client refuses, which are the harness's or the operator's to fix.
func TestReachValidation963_Retryable(t *testing.T) {
	for err, want := range map[error]bool{
		dhm.FetchError{Reason: "timeout"}: true, dhm.FetchError{Reason: "status 503"}: true, dhm.FetchError{Reason: "busy"}: true,
		dhm.FetchError{Reason: "invalid raster"}: true, dhm.FetchError{Reason: "status 401"}: false, dhm.FetchError{Reason: "status 403"}: false,
		dhm.ErrBadRequest: false, fmt.Errorf("wrapped: %w", dhm.FetchError{Reason: "transport"}): true,
	} {
		if got := retryable963(err); got != want {
			t.Errorf("retryable963(%v) = %v, want %v", err, got, want)
		}
	}
}

// TestReachValidation963_Gather963FromEnv: the opt-in runs' fetch
// settings, their defaults, and a bad value refused with its name.
func TestReachValidation963_Gather963FromEnv(t *testing.T) {
	g, err := gather963FromEnv(nil)
	if err != nil || g.timeout != 60*time.Second || g.retries != 3 || g.backoff != 5*time.Second || g.every != 25 {
		t.Fatalf("defaults %+v, %v", g, err)
	}
	t.Setenv("CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT", "90")
	t.Setenv("CORESCOPE_REACH_VALIDATION_963_FETCH_RETRIES", "0")
	t.Setenv("CORESCOPE_REACH_VALIDATION_963_PROGRESS_EVERY", "5")
	if g, err = gather963FromEnv(nil); err != nil || g.timeout != 90*time.Second || g.retries != 0 || g.every != 5 {
		t.Fatalf("seconds: %+v, %v", g, err)
	}
	t.Setenv("CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT", "2m")
	if g, err = gather963FromEnv(nil); err != nil || g.timeout != 2*time.Minute {
		t.Fatalf("duration: %+v, %v", g, err)
	}
	for name, v := range map[string]string{"CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT": "soon", "CORESCOPE_REACH_VALIDATION_963_FETCH_RETRIES": "-1"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, v)
			if _, err := gather963FromEnv(nil); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s=%s: %v", name, v, err)
			}
		})
	}
}
