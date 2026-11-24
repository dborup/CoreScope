package main

// #963 M0 — pins of the candidate propagation models (reach_models_963_test.go).
// The expected numbers were computed independently (python, closed forms) and
// are quoted in the M0 research comment.

import (
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

const freq963 = 869.618

func near963(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f ± %g", what, got, want, tol)
	}
}

func TestReachModels963_PlaneEarth(t *testing.T) {
	lambda := dhm.Wavelength(freq963)
	// Inside the breakpoint 4·h1·h2/λ (232 m at 10 m / 2 m) it is free space.
	near963(t, "plane earth at 100 m", planeEarthLoss963(100, 10, 2, lambda), dhm.FreeSpaceLoss(100, lambda), 1e-9)
	// Beyond it 40·log10(d) − 20·log10(h1·h2).
	near963(t, "plane earth at 5 km", planeEarthLoss963(5000, 10, 2, lambda), 121.938, 0.01)
	near963(t, "plane earth at 20 km", planeEarthLoss963(20000, 10, 2, lambda), 146.021, 0.01)
	near963(t, "excess over free space at 5 km", planeEarthLoss963(5000, 10, 2, lambda)-dhm.FreeSpaceLoss(5000, lambda), 16.724, 0.01)
	// Higher antennas push the breakpoint out: 10 m / 10 m is free space at 1 km.
	near963(t, "plane earth 10/10 at 1 km", planeEarthLoss963(1000, 10, 10, lambda), dhm.FreeSpaceLoss(1000, lambda), 1e-9)
}

func TestReachModels963_P2108TerminalClutter(t *testing.T) {
	// ITU-R P.2108-2 §3.1: open/rural R = 10 m by eq. (2b), suburban R = 10 m
	// and urban/trees/forest R = 15 m by eq. (2a); nothing at or above R.
	for _, c := range []struct {
		h    float64
		cl   clutterClass963
		want float64
	}{{2, clutterOpen963, 14.974}, {5, clutterOpen963, 6.449}, {10, clutterOpen963, 0}, {2, clutterSuburban963, 18.097},
		{2, clutterUrban963, 22.120}, {5, clutterSuburban963, 14.192}, {10, clutterUrban963, 14.192}, {10, clutterSuburban963, 0}, {16, clutterUrban963, 0}} {
		near963(t, fmt.Sprintf("P.2108 §3.1 loss, %g m in class %d", c.h, c.cl), p2108TerminalLoss963(c.h, c.cl, freq963), c.want, 0.01)
	}
	// map.meshcore.io's clutterLoss: no clutter in the open, eq. (2a) elsewhere.
	near963(t, "map.meshcore.io, open", mmClutterLoss963(2, clutterOpen963, freq963), 0, 1e-12)
	near963(t, "map.meshcore.io, urban", mmClutterLoss963(2, clutterUrban963, freq963), 22.120, 0.01)
}

func TestReachModels963_Hata(t *testing.T) {
	near963(t, "Hata open, 1 km", hataLoss963(1000, 10, 2, freq963, hataOpen963), 102.982, 0.01)
	near963(t, "Hata open, 20 km", hataLoss963(20000, 10, 2, freq963, hataOpen963), 152.877, 0.01)
	near963(t, "Hata suburban, 1 km", hataLoss963(1000, 10, 2, freq963, hataSuburban963), 121.489, 0.01)
	near963(t, "Hata suburban, 10 km", hataLoss963(10000, 10, 2, freq963, hataSuburban963), 159.839, 0.01)
	// Never below free space (Hata is not defined under 1 km).
	lambda := dhm.Wavelength(freq963)
	if got, fs := hataLoss963(50, 30, 2, freq963, hataOpen963), dhm.FreeSpaceLoss(50, lambda); got < fs-1e-9 {
		t.Errorf("Hata open at 50 m = %.2f, below free space %.2f", got, fs)
	}
}

func TestReachModels963_ClutterClass(t *testing.T) {
	open := []float64{0, 0, 0.5, 0, 4, 0, 0, 0, 0, 0}
	village := []float64{0, 6, 7, 5, 0, 6, 4, 0, 7, 5}
	forest := []float64{18, 20, 22, 19, 0, 21, 20, 23, 19, 18}
	// The edge of a wood: the median is open ground, the 75th percentile
	// the canopy.
	edge := []float64{0, 0, 0, 5, 5, 6, 14, 15, 16, 18}
	gaps := []float64{math.NaN(), math.NaN()}
	for _, c := range []struct {
		name string
		in   []float64
		want clutterClass963
	}{{"open", open, clutterOpen963}, {"village", village, clutterSuburban963}, {"forest", forest, clutterUrban963}, {"edge of a wood", edge, clutterUrban963}, {"no data", gaps, clutterOpen963}} {
		if got := classifyClutter963(c.in); got != c.want {
			t.Errorf("%s: class %d, want %d", c.name, got, c.want)
		}
	}
}

// flatProfile963 is n+1 samples of ground at h.
func flatProfile963(n int, h float32) []float32 {
	s := make([]float32, n+1)
	for i := range s {
		s[i] = h
	}
	return s
}

func pathIn963(dtm, dsm []float32, dM, txH, rxH float64) path963 {
	return path963{dtm: dtm, dsm: dsm, distanceM: dM, txAGL: txH, rxAGL: rxH, freqMHz: freq963, k: 4.0 / 3}
}

func TestReachModels963_Candidates(t *testing.T) {
	const n, d = 400, 10000.0 // 25 m steps
	dtm := flatProfile963(n, 50)
	p := pathIn963(dtm, dtm, d, 10, 2)
	l := candidateLosses963(p)
	lambda := dhm.Wavelength(freq963)
	delta, ok := dhm.Link{DistanceM: d, HeightA: 60, HeightB: 52, LambdaM: lambda, K: 4.0 / 3}.PathLoss(dtm, dhm.PathDelta)
	if !ok {
		t.Fatal("no delta loss")
	}
	near963(t, "delta", l[cand963Delta], delta, 1e-9)
	near963(t, "today's budget term", l[cand963DeltaEnv], delta+25, 1e-9)
	// Flat ground: the effective heights are the antenna heights.
	near963(t, "two-ray", l[cand963TwoRay], math.Max(delta, planeEarthLoss963(d, 10, 2, lambda)), 1e-6)
	near963(t, "Hata open", l[cand963HataOpen], hataLoss963(d, 10, 2, freq963, hataOpen963), 1e-6)
	near963(t, "Hata suburban", l[cand963HataSub], hataLoss963(d, 10, 2, freq963, hataSuburban963), 1e-6)
	// No clutter anywhere: every surface model is the bare-earth one.
	for _, c := range []string{cand963DSM, cand963DSMRaw} {
		near963(t, c+" without clutter", l[c], delta, 1e-9)
	}
	// P.2108 by the book (P.526-16 §4.5): the 2 m receiver in open land is
	// raised to R = 10 m and corrected by eq. (2b); on flat ground that is
	// within 1.5 dB of delta at 2 m, the same ground-reflection height gain.
	delta1010, _ := dhm.Link{DistanceM: d, HeightA: 60, HeightB: 60, LambdaM: lambda, K: 4.0 / 3}.PathLoss(dtm, dhm.PathDelta)
	near963(t, "delta + P.2108, open", l[cand963DeltaP2108], delta1010+14.974, 0.01)
	if math.Abs(l[cand963DeltaP2108]-delta) > 1.5 {
		t.Errorf("delta + P.2108 in the open %.2f, delta %.2f: more than 1.5 dB apart", l[cand963DeltaP2108], delta)
	}
	near963(t, "two-ray + P.2108, open", l[cand963TwoRayP2108], math.Max(delta1010, planeEarthLoss963(d, 10, 10, lambda))+14.974, 0.01)
	// The clutter-only variant leaves open land to delta's own ground term.
	near963(t, "delta + P.2108 clutter classes, open", l[cand963DeltaP2108C], delta, 1e-9)
	near963(t, "two-ray + DSM without clutter", l[cand963TwoRayDSM], l[cand963TwoRay], 1e-9)
	near963(t, "map.meshcore.io proxy without clutter: the fade margin only", l[cand963MM], delta+10, 1e-9)

	// A 20 m forest band across the middle of the path.
	dsm := flatProfile963(n, 50)
	for i := 180; i <= 220; i++ {
		dsm[i] = 70
	}
	l = candidateLosses963(pathIn963(dtm, dsm, d, 10, 2))
	link := dhm.Link{DistanceM: d, HeightA: 60, HeightB: 52, LambdaM: lambda, K: 4.0 / 3}
	wantDSM, _ := link.PathLoss(dsm, dhm.PathDelta)
	near963(t, "DSM as obstacles: delta over the DSM", l[cand963DSM], wantDSM, 1e-9)
	// On flat ground at 10 km the smooth-earth term already dominates, so a
	// 20 m canopy band mid-path adds little (1.5 dB), but it adds.
	if !(l[cand963DSM] > delta+1) {
		t.Errorf("DSM as obstacles: %.2f, want above the bare-earth %.2f", l[cand963DSM], delta)
	}
	near963(t, "DSM, no clearance, same as with it here", l[cand963DSMRaw], l[cand963DSM], 1e-9)
	near963(t, "P.2108 ignores the middle of the path", l[cand963DeltaP2108], delta1010+14.974, 0.01)
	// map.meshcore.io's proxy: 25 m on the bare earth where the canopy is ≥ 10 m.
	mm := append([]float32(nil), dtm...)
	for i := 180; i <= 220; i++ {
		mm[i] = 75
	}
	want, _ := dhm.Link{DistanceM: d, HeightA: 60, HeightB: 52, LambdaM: lambda, K: 4.0 / 3}.PathLoss(mm, dhm.PathDelta)
	near963(t, "map.meshcore.io proxy, forest band", l[cand963MM], want+10, 1e-9)

	// The same forest only within 150 m of the receiver: the clearance keeps
	// it out of DSM and the proxy, the raw DSM blocks.
	dsm = flatProfile963(n, 50)
	for i := n - 5; i < n; i++ { // 125 m to 25 m from the receiver
		dsm[i] = 70
	}
	l = candidateLosses963(pathIn963(dtm, dsm, d, 10, 2))
	near963(t, "DSM, clutter inside the 150 m clearance", l[cand963DSM], delta, 1e-9)
	if !(l[cand963DSMRaw] > delta+5) {
		t.Errorf("raw DSM: %.2f, want the receiver's own trees to block (> %.2f)", l[cand963DSMRaw], delta+5)
	}
	near963(t, "map.meshcore.io proxy, trees in the clearance", l[cand963MM], delta+10, 1e-9)
}

func TestReachModels963_EndClutter(t *testing.T) {
	const n, d = 400, 10000.0
	dtm := flatProfile963(n, 50)
	p := pathIn963(dtm, dtm, d, 10, 2)
	p.endClass = [2]clutterClass963{clutterSuburban963, clutterUrban963}
	l := candidateLosses963(p)
	lambda := dhm.Wavelength(freq963)
	// 10 m among suburban clutter (10 m): none. 2 m among urban/forest
	// (15 m): raised to 15 m, plus eq. (2a)'s 22.1 dB.
	raised, _ := dhm.Link{DistanceM: d, HeightA: 60, HeightB: 65, LambdaM: lambda, K: 4.0 / 3}.PathLoss(dtm, dhm.PathDelta)
	near963(t, "delta + P.2108", l[cand963DeltaP2108], raised+22.120, 0.01)
	near963(t, "two-ray + P.2108", l[cand963TwoRayP2108], math.Max(raised, planeEarthLoss963(d, 10, 15, lambda))+22.120, 0.01)
	near963(t, "delta + P.2108 clutter classes", l[cand963DeltaP2108C], raised+22.120, 0.01)
	// map.meshcore.io keeps the receiver at 2 m and adds the same 22.1 dB.
	near963(t, "map.meshcore.io proxy", l[cand963MM]-l[cand963Delta], 22.120+10, 0.01)

	// A 2 m transmitter in open land: by the book it is raised to 10 m and
	// corrected by eq. (2b); the clutter-only variant leaves it at 2 m.
	p = pathIn963(dtm, dtm, d, 2, 2)
	p.endClass = [2]clutterClass963{clutterOpen963, clutterUrban963}
	l = candidateLosses963(p)
	both, _ := dhm.Link{DistanceM: d, HeightA: 60, HeightB: 65, LambdaM: lambda, K: 4.0 / 3}.PathLoss(dtm, dhm.PathDelta)
	rxOnly, _ := dhm.Link{DistanceM: d, HeightA: 52, HeightB: 65, LambdaM: lambda, K: 4.0 / 3}.PathLoss(dtm, dhm.PathDelta)
	near963(t, "delta + P.2108, open 2 m transmitter", l[cand963DeltaP2108], both+14.974+22.120, 0.01)
	near963(t, "delta + P.2108 clutter classes, open 2 m transmitter", l[cand963DeltaP2108C], rxOnly+22.120, 0.01)
}

// flatGrid963 is a coverage grid of ground at h around (e0, n0).
func flatGrid963(e0, n0, r, res float64, h float32) *dhm.CoverageGrid {
	g := dhm.NewCoverageGrid(e0, n0, r, res)
	for i := range g.Values {
		g.Values[i] = h
	}
	return g
}

func TestReachModels963_ReachRadiusFlat(t *testing.T) {
	e0, n0 := dhm.ToUTM32(56.136, 8.975)
	const res, r = 30.0, 40000.0
	g := flatGrid963(e0, n0, r+2*res, res, 50)
	rr := reachLosses963(g, g, e0, n0, 10, 2, freq963, 4.0/3, r, res, 1)
	radius := func(c string, budget float64) float64 { return reachRadius963(rr[c], res, budget)[0] }
	// Reached is a margin of at least 3 dB, as in the #941 report. Hata open
	// reaches 158 − 3 dB at 22.72 km and 133 − 3 dB at 5.06 km (closed form).
	near963(t, "Hata open radius at 158 dB", radius(cand963HataOpen, 158), 22720, res)
	near963(t, "Hata open radius at 133 dB", radius(cand963HataOpen, 133), 5064, res)
	// Finding: on flat ground delta-Bullington is within 1.5 dB of the plane-
	// earth loss out to 10 km and above it beyond, so two-ray reaches no
	// shorter than delta here: 7.92 km at 133 dB (plane earth alone, closed
	// form: 7.95 km) and 22.80 km at 158 dB.
	near963(t, "delta radius at 133 dB", radius(cand963Delta, 133), 7920, 1e-9)
	near963(t, "delta radius at 158 dB", radius(cand963Delta, 158), 22800, 1e-9)
	for _, b := range []float64{133, 158} {
		near963(t, "two-ray radius", radius(cand963TwoRay, b), radius(cand963Delta, b), 1e-9)
	}
	// Open land: the clutter-only P.2108 variant is delta.
	near963(t, "delta + P.2108 clutter classes", radius(cand963DeltaP2108C, 158), radius(cand963Delta, 158), 1e-9)
	// Today's 25 dB is delta at 158 − 25 dB.
	near963(t, "today's budget term", radius(cand963DeltaEnv, 158), radius(cand963Delta, 133), 1e-9)

	rep := reachRadius963Report{Lat: 56.136, Lon: 8.975, TxHeightM: 10, RxHeightM: 2, RadiusKm: r / 1000, ResolutionM: int(res), Bearings: 1,
		BudgetsDb: reachRadius963Budgets, Terrain: "flat (synthetic), no clutter", TxClass: clutterNames963[clutterOpen963], Rows: reachRadiusRows963(rr, res)}
	t.Logf("\n%s", rep.markdown())
	if dir := os.Getenv("CORESCOPE_REACH_VALIDATION_963_OUT"); dir != "" {
		if err := writeReport963(dir, "reach-radius-963-flat", rep, rep.markdown()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReachModels963_ReachRadiusReceiverClutter: the receiver's clutter
// class is taken where the receiver is, not at the transmitter: a node in a
// clearing among 20 m of canopy reaches less far with P.2108 at the
// receiver's end than delta, and its own end stays open.
func TestReachModels963_ReachRadiusReceiverClutter(t *testing.T) {
	e0, n0 := dhm.ToUTM32(56.136, 8.975)
	const res, r = 30.0, 10000.0
	tg := flatGrid963(e0, n0, r+2*res, res, 50)
	sg := flatGrid963(e0, n0, r+2*res, res, 70)
	for i := range sg.Values {
		row, col := i/sg.Width, i%sg.Width
		e := sg.OriginE + (float64(col)+0.5)*res
		n := sg.OriginN - (float64(row)+0.5)*res
		if math.Hypot(e-e0, n-n0) < 400 {
			sg.Values[i] = 50 // the clearing
		}
	}
	if c := clutterAt963(tg, sg, e0, n0, res); c != clutterOpen963 {
		t.Fatalf("the clearing is class %d", c)
	}
	rr := reachLosses963(tg, sg, e0, n0, 10, 2, freq963, 4.0/3, r, res, 1)
	clutter := reachRadius963(rr[cand963DeltaP2108C], res, 133)[0]
	bare := reachRadius963(rr[cand963Delta], res, 133)[0]
	if !(clutter < bare-500) {
		t.Fatalf("P.2108 at the receiver's end reaches %.0f m, delta %.0f m", clutter, bare)
	}
}
