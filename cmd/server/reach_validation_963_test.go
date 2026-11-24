package main

// #963 M0 — tests and opt-in runs of the candidate-model harness
// (reach_validation_963_harness_test.go). CI runs the fixture mechanics on
// synthetic terrain; the real-data and GLO-30 runs are opt-in:
//
//	CORESCOPE_REACH_VALIDATION_DB / _CONFIG / _OUT  the instance run (#941's variables)
//	CORESCOPE_REACH_VALIDATION_963_GLO30=1           the fixture and the reach radius over real GLO-30
//	CORESCOPE_REACH_VALIDATION_963_OUT=<dir>         where the fixture and GLO-30 runs write reports
//	CORESCOPE_REACH_VALIDATION_963_POINT=lat,lon     the reach-radius point (default 56.136,8.975)

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
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
	rep, err := runReachValidation963(context.Background(), srv, c, cs, db.conn, o)
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
	rep, err := reachRadiusAt963(context.Background(), c, cs, reachFixtureOrigin, 10, 2, 10000, 8, func(time.Duration) { t.Fatal("the upstream budget ran out") })
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
	srv, c, db, cfg, o := openRealReachValidation(t)
	pt, err := reachPoint963()
	if err != nil {
		t.Fatal(err)
	}
	cs := surfaceCoverage963(c, cfg.Coverage)
	o.upstream = countReach963Upstream(c, cs)
	rep, err := runReachValidation963(context.Background(), srv, c, cs, db.conn, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("report in %s: %d scored pairs (%d positives), %s", o.outDir, rep.Pairs.Scored, rep.Pairs.Positives, upstreamText(o.upstream()))
	writeReachRadius963(t, c, cs, pt, o.terrainSource+map[bool]string{true: " + dhm surface", false: ""}[cs != nil], o.upstream, o.outDir)
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
	writeReachRadius963(t, cr, nil, pt, "glo30", upstream, out)

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
	rep, err := runReachValidation963(context.Background(), srv, c, nil, db.conn, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture over GLO-30: %d scored pairs, %d without terrain, %s", rep.Pairs.Scored, rep.Pairs.NoTerrain, upstreamText(upstream()))
}

// writeReachRadius963 computes the 10 m / 2 m reach radius at pt to 40 km
// over 72 bearings and writes reach-radius-963.{json,md} to dir.
func writeReachRadius963(t *testing.T, c, cs *terrainCoverage, pt dhm.LatLon, terrain string, upstream func() int64, dir string) {
	t.Helper()
	start := time.Now()
	rr, err := reachRadiusAt963(context.Background(), c, cs, pt, 10, 2, 40000, 72, time.Sleep)
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
