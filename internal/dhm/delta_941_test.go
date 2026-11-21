package dhm

import (
	"context"
	"math"
	"math/rand"
	"testing"
)

// #941 M2 — the complete method of ITU-R P.526-16 §4.5.2
// ("delta-Bullington", delta.go) and its place in the sweep.

// deltaProfile is the profile of a reference vector: the hilly
// p1812Profile extended to n samples, or flat ground at a height.
func deltaProfile(hilly bool, phase float64, ridge bool, base float64, n int) []float32 {
	g := make([]float32, n)
	for i := range g {
		if !hilly {
			g[i] = float32(base)
			continue
		}
		v := 20 + 15*math.Sin(float64(i)/17.0) + 8*math.Sin(float64(i)/5.3+phase) + base
		if ridge && i == 120 {
			v += 60
		}
		g[i] = float32(v)
	}
	return g
}

// TestDeltaBullington941_MatchesP1812Reference compares
// Link.DeltaBullington with the reference implementation of the same
// method (OFCOM's Py1812 dl_delta_bull, P.1812-6 §4.3.4, transcribed in
// testdata/delta_bullington_941_vectors.py, which prints the table) on
// hilly and flat profiles from 1 to 40 km, inside and beyond the radio
// horizon. Every part is checked: L_bulla, L_bulls, L_sph and L_d.
func TestDeltaBullington941_MatchesP1812Reference(t *testing.T) {
	lambda := Wavelength(FirmwareFreqMHz)
	type prof struct {
		hilly bool
		phase float64
		ridge bool
		base  float64
	}
	for _, c := range []struct {
		name                     string
		p                        prof
		n, step                  int
		ta, ra                   float64
		bulla, bulls, sph, delta float64
	}{
		{"hilly nlos 10/2", prof{true, 0, false, 0}, 201, 30, 10, 2, 33.482780255911536, 1.1958171138898552, 1.5663273860556157, 33.853290528077295},
		{"hilly ridge", prof{true, 1, true, 7}, 201, 30, 10, 2, 34.02601123874618, 0.0, 0.0, 34.02601123874618},
		{"hilly los 40/22", prof{true, 3, false, 0}, 201, 30, 40, 22, 11.964441255844807, 0.0, 0.0, 11.964441255844807},
		{"hilly los clear", prof{true, 0, false, 0}, 201, 30, 120, 100, 0.0, 0.0, 0.0, 0.0},
		{"hilly 30 km", prof{true, 2, false, 14}, 1001, 30, 10, 2, 35.538792572182814, 9.655892463619846, 14.23728838115729, 40.120188489720256},
		{"flat 1 km 10/10", prof{false, 0, false, 5}, 34, 30, 10, 10, 0.0, 0.0, 0.0, 0.0},
		{"flat 5 km los", prof{false, 0, false, 0}, 168, 30, 10, 2, 8.215529229760861, 8.215529229760861, 16.271072826900824, 16.271072826900824},
		{"flat 15 km los", prof{false, 0, false, 0}, 501, 30, 10, 2, 11.583632626976378, 11.583632626976378, 29.07326699160348, 29.07326699160348},
		{"flat 25 km", prof{false, 0, false, 0}, 834, 30, 10, 2, 14.496591481107405, 14.496591481107405, 38.33872305734073, 38.33872305734073},
		{"flat 40 km", prof{false, 0, false, 12}, 1334, 30, 10, 2, 20.067113736758564, 20.06711373675856, 49.8609237659423, 49.8609237659423},
		{"flat 30 km 30/5", prof{false, 0, false, 3}, 1001, 30, 30, 5, 12.167363509636921, 12.167363509636921, 23.324510246730952, 23.324510246730952},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := deltaProfile(c.p.hilly, c.p.phase, c.p.ridge, c.p.base, c.n)
			link := Link{DistanceM: float64((c.n - 1) * c.step), HeightA: float64(g[0]) + c.ta, HeightB: float64(g[c.n-1]) + c.ra,
				LambdaM: lambda, K: DefaultKFactor}
			d, ok := link.DeltaBullington(g)
			if !ok {
				t.Fatal("no result")
			}
			// float32 heights against the reference's float64.
			nearly(t, "L_bulla", d.Actual.Loss, c.bulla, 2e-3)
			nearly(t, "L_bulls", d.Smooth, c.bulls, 2e-3)
			nearly(t, "L_sph", d.Spherical, c.sph, 2e-3)
			nearly(t, "L_d", d.Loss, c.delta, 2e-3)
			loss, ok := link.PathLoss(g, PathDelta)
			nearly(t, "PathLoss", loss, FreeSpaceLoss(link.DistanceM, lambda)+c.delta, 2e-3)
			if !ok {
				t.Fatal("PathLoss not ok")
			}
		})
	}
}

// flatLink is a path over flat ground at sea level: n intervals of step,
// antennas ta and tb above it.
func flatSeaLink(n int, step, ta, tb float64) (Link, []float32) {
	return Link{DistanceM: float64(n) * step, HeightA: ta, HeightB: tb, LambdaM: Wavelength(FirmwareFreqMHz), K: DefaultKFactor},
		make([]float32, n+1)
}

// TestDeltaBullington941_SmoothEarthBeyondTheHorizon: over flat ground
// plain Bullington misses the smooth earth's diffraction, which is where
// the complete method differs. With 10 m and 2 m antennas the horizon is
// at √(2kR·10) + √(2kR·2) = 18.9 km; beyond it the delta loss is far above
// Bullington's and keeps growing with distance, and it is never below it.
func TestDeltaBullington941_SmoothEarthBeyondTheHorizon(t *testing.T) {
	prev := 0.0
	for _, km := range []float64{20, 25, 30, 40} {
		n := int(km * 1000 / 30)
		link, g := flatSeaLink(n, 30, 10, 2)
		bull, _ := link.PathLoss(g, PathBullington)
		delta, ok := link.PathLoss(g, PathDelta)
		if !ok {
			t.Fatal("no result")
		}
		if delta < bull+15 {
			t.Fatalf("%v km: delta %.2f dB, Bullington %.2f dB: want the smooth-earth term (≥ 15 dB more)", km, delta, bull)
		}
		if delta <= prev {
			t.Fatalf("%v km: delta %.2f dB not above %.2f dB at a shorter distance", km, delta, prev)
		}
		prev = delta
	}
	// Property: on any terrain the correction is never negative.
	rng := rand.New(rand.NewSource(9412))
	for trial := 0; trial < 300; trial++ {
		n := 2 + rng.Intn(800)
		g := make([]float32, n+1)
		ph := rng.Float64() * 10
		for i := range g {
			g[i] = float32(20 + 20*math.Sin(float64(i)/(4+ph)) + 10*math.Sin(float64(i)/1.7+ph) + rng.Float64()*4)
		}
		link := Link{DistanceM: float64(n) * (20 + rng.Float64()*40), HeightA: float64(g[0]) + rng.Float64()*40,
			HeightB: float64(g[n]) + rng.Float64()*20, LambdaM: Wavelength(400 + rng.Float64()*600), K: []float64{1, DefaultKFactor, 4}[trial%3]}
		b, _ := link.Bullington(g)
		d, ok := link.DeltaBullington(g)
		if !ok || math.IsNaN(d.Loss) || d.Loss < b.Loss-1e-12 || d.Actual != b {
			t.Fatalf("trial %d: delta %+v, Bullington %+v", trial, d, b)
		}
		if d.HeightTx < link.HeightA-float64(g[0])-1e-9 || d.HeightRx < link.HeightB-float64(g[n])-1e-9 {
			t.Fatalf("trial %d: effective heights %v/%v below the antennas above the ground", trial, d.HeightTx, d.HeightRx)
		}
	}
}

// TestDeltaBullington941_EqualOnShortLineOfSight: on a short path whose
// line of sight clears the curved earth by the required clearance, the
// spherical-earth loss is zero and the complete method is Bullington,
// partly obstructed first Fresnel zone or not.
func TestDeltaBullington941_EqualOnShortLineOfSight(t *testing.T) {
	lambda := Wavelength(FirmwareFreqMHz)
	for _, c := range []struct {
		name   string
		g      []float32
		ta, tb float64
	}{
		{"flat 1 km 10/10", make([]float32, 34), 10, 10},
		{"flat 2 km 30/30", make([]float32, 68), 30, 30},
		{"hilly 6 km 40/22", deltaProfile(true, 3, false, 0, 201), 40, 22},
		{"hilly 6 km 34/30", deltaProfile(true, 1, false, 0, 201), 34, 30},
	} {
		n := len(c.g) - 1
		link := Link{DistanceM: float64(n) * 30, HeightA: float64(c.g[0]) + c.ta, HeightB: float64(c.g[n]) + c.tb, LambdaM: lambda, K: DefaultKFactor}
		b, _ := link.Bullington(c.g)
		d, _ := link.DeltaBullington(c.g)
		if !b.LoS || d.Spherical != 0 || d.Loss != b.Loss {
			t.Fatalf("%s: Bullington %+v, delta %+v", c.name, b, d)
		}
		bl, _ := link.PathLoss(c.g, PathBullington)
		dl, _ := link.PathLoss(c.g, PathDelta)
		if bl != dl {
			t.Fatalf("%s: path loss %v vs %v", c.name, bl, dl)
		}
	}
	// The partly obstructed case really is: a nonzero Bullington loss.
	g := deltaProfile(true, 3, false, 0, 201)
	b, _ := (Link{DistanceM: 6000, HeightA: float64(g[0]) + 40, HeightB: float64(g[200]) + 22, LambdaM: lambda, K: DefaultKFactor}).Bullington(g)
	if b.Loss < 5 {
		t.Fatalf("the hilly line of sight is not obstructed: %+v", b)
	}
}

// TestSmoothBullington941_MatchesTheProfile: the O(log n) smooth-path
// Bullington is Link.Bullington over an all-zero surface, sample for
// sample, on both branches.
func TestSmoothBullington941_MatchesTheProfile(t *testing.T) {
	rng := rand.New(rand.NewSource(9413))
	los, nlos := 0, 0
	for trial := 0; trial < 2000; trial++ {
		n := 2 + rng.Intn(1500)
		step := 10 + rng.Float64()*50
		hts, hrs := rng.Float64()*60, rng.Float64()*60
		if trial%7 == 0 {
			hts = 0
		}
		k := []float64{0.7, DefaultKFactor, 4, 1e9}[trial%4]
		lambda := Wavelength(150 + rng.Float64()*2000)
		ref, _ := (Link{DistanceM: float64(n) * step, HeightA: hts, HeightB: hrs, LambdaM: lambda, K: k}).Bullington(make([]float32, n+1))
		got := smoothBullington(hts, hrs, n, step, k, lambda)
		if got.LoS != ref.LoS || math.Abs(got.Loss-ref.Loss) > 1e-9*math.Max(1, ref.Loss) ||
			math.Abs(got.Nu-ref.Nu) > 1e-9*math.Max(1, math.Abs(ref.Nu)) {
			t.Fatalf("trial %d (n %d step %.1f h %.2f/%.2f k %g): got %+v, profile %+v", trial, n, step, hts, hrs, k, got, ref)
		}
		if ref.LoS {
			los++
		} else {
			nlos++
		}
	}
	if los < 200 || nlos < 200 {
		t.Fatalf("branches not both exercised: %d LoS, %d beyond", los, nlos)
	}
}

// TestMaxUnimodal941: the ternary search finds the top of a rising then
// falling sequence, including at either end and on a plateau.
func TestMaxUnimodal941(t *testing.T) {
	rng := rand.New(rand.NewSource(9414))
	for trial := 0; trial < 3000; trial++ {
		lo := rng.Intn(5)
		hi := lo + rng.Intn(400)
		peak := lo + rng.Intn(hi-lo+1)
		width := rng.Intn(4)
		f := func(i int) float64 {
			d := math.Abs(float64(i - peak))
			if d <= float64(width) {
				return 7
			}
			return 7 - d*d
		}
		if got := maxUnimodal(lo, hi, f); got != 7 {
			t.Fatalf("lo %d hi %d peak %d: %v", lo, hi, peak, got)
		}
	}
}

// TestSphericalEarth941_Continuous: the spherical-earth loss is zero on a
// well-cleared path, grows with distance and is continuous across the
// marginal line of sight (P.1812 eqs. 22–27 join there).
func TestSphericalEarth941_Continuous(t *testing.T) {
	se := newSphericalEarth(Wavelength(FirmwareFreqMHz), DefaultKFactor)
	if v := se.loss(1000, 30, 30); v != 0 {
		t.Fatalf("1 km 30/30: %v dB", v)
	}
	dlos := math.Sqrt(2*se.aeKm) * (math.Sqrt(0.001*10) + math.Sqrt(0.001*2)) * 1000
	below, above := se.loss(dlos-1, 10, 2), se.loss(dlos+1, 10, 2)
	if math.Abs(below-above) > 0.05 {
		t.Fatalf("jump at the marginal line of sight (%.0f m): %.3f → %.3f dB", dlos, below, above)
	}
	prev := -1.0
	for d := 2000.0; d <= 60000; d += 500 {
		v := se.loss(d, 10, 2)
		if math.IsNaN(v) || v < prev-1e-9 {
			t.Fatalf("not monotonic at %v m: %v after %v", d, v, prev)
		}
		prev = v
	}
	// A zero-height antenna is a defined, finite loss.
	if v := se.loss(30000, 0, 2); math.IsNaN(v) || math.IsInf(v, 0) {
		t.Fatalf("zero antenna: %v", v)
	}
}

// TestPathModel941_IDs: the served models parse, nothing else does, and
// each model's id is versioned and distinct (the cache key carries it).
func TestPathModel941_IDs(t *testing.T) {
	for _, s := range []string{"bullington", "delta"} {
		if m, ok := ParsePathModel(s); !ok || string(m) != s {
			t.Fatalf("%q: %v %v", s, m, ok)
		}
	}
	for _, s := range []string{"", "fspl", "Delta", "itm", "delta "} {
		if _, ok := ParsePathModel(s); ok {
			t.Fatalf("%q parsed", s)
		}
	}
	ids := map[string]bool{PathBullington.ID(): true, PathDelta.ID(): true, PathFreeSpace.ID(): true}
	if len(ids) != 3 || PathBullington.ID() != CoverageModel || PathDelta.ID() != CoverageModelDelta || PathModel("").ID() != CoverageModel {
		t.Fatalf("ids %v", ids)
	}
}

// sweepForModel is sweepFor with a model.
func sweepForModel(h []float32, res, txH, rxH, freq, k float64, rxEvery int, m PathModel) []float32 {
	s := newRadialSweep(res, freq, k, len(h)-1, rxEvery, m)
	nRx := (len(h) - 1) / rxEvery
	loss := make([]float32, nRx)
	s.run(h, txH, rxH, loss, nil)
	return loss
}

// TestCoverageSweep941_DeltaMatchesLinkReference is the DRY property of
// the complete method: on random terrain, every receiver of the sweep
// (running sums, one pass) has the loss of Link.PathLoss with PathDelta
// over the same samples (direct sums, the reference).
func TestCoverageSweep941_DeltaMatchesLinkReference(t *testing.T) {
	rng := rand.New(rand.NewSource(9415))
	for trial := 0; trial < 60; trial++ {
		res := []float64{20, 30, 60}[trial%3]
		n := 50 + rng.Intn(600)
		h := make([]float32, n+1)
		ph := rng.Float64() * 10
		base := rng.Float64() * 150
		for i := range h {
			h[i] = float32(base + 25*math.Sin(float64(i)/(5+ph)) + 15*math.Sin(float64(i)/2.1+ph) + rng.Float64()*3)
			if trial%4 == 0 {
				h[i] = float32(base) // flat: the smooth-earth term dominates
			}
		}
		txH, rxH := 1+rng.Float64()*40, 1+rng.Float64()*10
		freq := 433 + rng.Float64()*500
		k := []float64{DefaultKFactor, 1, 4}[trial%3]
		rxEvery := 1 + trial%3
		loss := sweepForModel(h, res, txH, rxH, freq, k, rxEvery, PathDelta)
		lambda := Wavelength(freq)
		for r := range loss {
			j := (r + 1) * rxEvery
			link := Link{DistanceM: float64(j) * res, HeightA: float64(h[0]) + txH, HeightB: float64(h[j]) + rxH, LambdaM: lambda, K: k}
			want, ok := link.PathLoss(h[:j+1], PathDelta)
			if !ok {
				t.Fatal("reference failed")
			}
			if math.Abs(float64(loss[r])-want) > 2e-3 {
				t.Fatalf("trial %d rx %d (j=%d): sweep %v, reference %v", trial, r, j, loss[r], want)
			}
		}
		// The Bullington sweep is unchanged by the model switch.
		bl := sweepForModel(h, res, txH, rxH, freq, k, rxEvery, PathBullington)
		ref, _ := sweepFor(h, res, txH, rxH, freq, k, rxEvery)
		for r := range bl {
			if bl[r] != ref[r] || loss[r] < bl[r]-1e-3 {
				t.Fatalf("trial %d rx %d: Bullington %v/%v, delta %v", trial, r, bl[r], ref[r], loss[r])
			}
		}
	}
}

// TestComputeCoverage941_DeltaOnFlatGround: the complete method in the
// whole pipeline. Over flat ground with 10 m antennas at both ends the
// delta raster is the Bullington raster near the centre, where the path
// clears the earth by the required clearance, and far lossier past the
// 26 km horizon (√(2kR·10) twice).
func TestComputeCoverage941_DeltaOnFlatGround(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	g := covGridFrom(e0, n0, 32000, 30, func(float64, float64) float32 { return 0 })
	spec := CoverageSpec{Center: LatLon{Lat: covAarhusLat, Lon: covAarhusLon}, RadiusM: 32000, ResolutionM: 30,
		TxHeightM: 10, RxHeightM: 10, FreqMHz: FirmwareFreqMHz, K: DefaultKFactor, OutPixelM: 100, Workers: 2}
	bull, err := ComputeCoverage(context.Background(), g, spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Model = PathDelta
	delta, err := ComputeCoverage(context.Background(), g, spec)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Width != bull.Width || delta.Height != bull.Height || delta.Iterations != bull.Iterations {
		t.Fatalf("shape differs: %dx%d/%d vs %dx%d/%d", delta.Width, delta.Height, delta.Iterations, bull.Width, bull.Height, bull.Iterations)
	}
	at := func(r CoverageResult, dN float64) int {
		lat, lon := FromUTM32(e0, n0+dN)
		x, y := lonLatToMercator(lon, lat)
		w, s := r.Bounds[0][1], r.Bounds[0][0]
		e, nn := r.Bounds[1][1], r.Bounds[1][0]
		x0, y0 := lonLatToMercator(w, s)
		x1, y1 := lonLatToMercator(e, nn)
		px := int((x - x0) / (x1 - x0) * float64(r.Width))
		py := int((y1 - y) / (y1 - y0) * float64(r.Height))
		return int(r.Loss[py*r.Width+px])
	}
	if b, d := at(bull, 800), at(delta, 800); b != d {
		t.Fatalf("at 800 m: Bullington byte %d, delta %d", b, d)
	}
	if b, d := at(bull, 30000), at(delta, 30000); d < b+20 {
		t.Fatalf("at 30 km: Bullington byte %d, delta %d: want ≥ 10 dB more", b, d)
	}
}

// BenchmarkCoverage40km30mDelta is BenchmarkCoverage40km30m with the
// complete method: the M2 perf budget (warm compute ≤ 3 s).
func BenchmarkCoverage40km30mDelta(b *testing.B) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	spec := CoverageSpec{Center: LatLon{Lat: covAarhusLat, Lon: covAarhusLon}, RadiusM: 40000, ResolutionM: 30,
		TxHeightM: 10, RxHeightM: 2, FreqMHz: FirmwareFreqMHz, K: DefaultKFactor, OutPixelM: 100,
		Workers: CoverageWorkers(), Model: PathDelta}
	g := covGridFrom(e0, n0, spec.RadiusM, spec.ResolutionM, covSyntheticDEM(e0, n0))
	b.ResetTimer()
	var iters int64
	for i := 0; i < b.N; i++ {
		res, err := ComputeCoverage(context.Background(), g, spec)
		if err != nil {
			b.Fatal(err)
		}
		iters = res.Iterations
	}
	b.ReportMetric(float64(iters), "inner-iters")
	b.ReportMetric(float64(spec.Workers), "workers")
}

// TestMaxConcave941: the top of a concave sequence is next to its
// continuous maximum, clamped at both ends.
func TestMaxConcave941(t *testing.T) {
	rng := rand.New(rand.NewSource(9416))
	for trial := 0; trial < 3000; trial++ {
		lo := 1 + rng.Intn(3)
		hi := lo + rng.Intn(300)
		x := float64(lo) - 20 + rng.Float64()*float64(hi-lo+40)
		f := func(i int) float64 { return -(float64(i) - x) * (float64(i) - x) }
		want := math.Inf(-1)
		for i := lo; i <= hi; i++ {
			want = math.Max(want, f(i))
		}
		if got := maxConcave(lo, hi, x, f); got != want {
			t.Fatalf("lo %d hi %d x %.3f: %v, want %v", lo, hi, x, got, want)
		}
	}
	if got := maxConcave(1, 9, math.NaN(), func(i int) float64 { return float64(-i) }); got != -1 {
		t.Fatalf("NaN optimum: %v", got)
	}
}
