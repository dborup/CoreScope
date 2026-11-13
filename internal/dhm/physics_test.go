package dhm

import (
	"math"
	"testing"
)

// #681 M3 — the physics table against the plan's hand-computed values
// (§4.2–4.3), the verdict boundaries and decimation.

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s = %.6f, want %.6f ±%g", what, got, want, tol)
	}
}

func TestWavelengthAtFirmwareDefault(t *testing.T) {
	if FirmwareFreqMHz != 869.618 {
		t.Fatalf("FirmwareFreqMHz %v, want 869.618 (firmware/platformio.ini LORA_FREQ)", FirmwareFreqMHz)
	}
	near(t, "λ(869.618 MHz)", Wavelength(869.618), 0.344740, 5e-7)
	near(t, "λ(910.525 MHz)", Wavelength(910.525), 0.329252, 5e-7)
}

func TestFresnelAndBulgeTable(t *testing.T) {
	lambda := Wavelength(FirmwareFreqMHz)
	for _, c := range []struct {
		km                  float64
		f1, bulge43, bulge1 float64
	}{
		{5, 20.76, 0.37, 0.49},
		{10, 29.36, 1.47, 1.96},
		{20, 41.52, 5.89, 7.85},
		{40, 58.71, 23.54, 31.39},
	} {
		d := c.km * 1000 / 2 // midpoint: d1 = d2
		near(t, "F1 max", FresnelRadius(lambda, d, d), c.f1, 0.006)
		near(t, "bulge k=4/3", EarthBulge(d, d, 4.0/3), c.bulge43, 0.006)
		near(t, "bulge k=1", EarthBulge(d, d, 1), c.bulge1, 0.006)
	}
	// At an endpoint the zone and the bulge vanish.
	if FresnelRadius(lambda, 0, 1000) != 0 || EarthBulge(0, 1000, 4.0/3) != 0 {
		t.Fatal("non-zero at the endpoint")
	}
}

func TestClassifyClearanceBoundaries(t *testing.T) {
	const eps = 1e-12
	for _, c := range []struct {
		ratio float64
		want  VerdictState
	}{
		{-1, VerdictBlocked},
		{-eps, VerdictBlocked},
		{0, VerdictFresnel60}, // exactly grazing: the LoS itself is clear
		{0.6 - eps, VerdictFresnel60},
		{0.6, VerdictFresnel100}, // exactly 60 %: the criterion holds
		{1 - eps, VerdictFresnel100},
		{1, VerdictClear}, // exactly 100 %: F1 fully clear
		{7, VerdictClear},
		{math.NaN(), VerdictUnknown},
	} {
		if got := ClassifyClearance(c.ratio); got != c.want {
			t.Errorf("ClassifyClearance(%v) = %q, want %q", c.ratio, got, c.want)
		}
	}
}

// flatLink is a 20 km link over flat ground at 0 m with 10 m antennas,
// sampled every 20 m.
func flatLink() (Link, []float32) {
	const n = 1001
	return Link{DistanceM: 20000, HeightA: 10, HeightB: 10, LambdaM: Wavelength(FirmwareFreqMHz), K: 4.0 / 3},
		make([]float32, n)
}

func TestAssessFlatLink(t *testing.T) {
	l, dtm := flatLink()
	a := l.Assess(dtm)
	// Worst point is mid-path: clearance 10 − 5.886 m against F1 41.52 m.
	near(t, "worst at", a.WorstAtM, 10000, 0.001)
	near(t, "worst clearance", a.WorstClearanceM, 10-5.886, 0.01)
	near(t, "worst ratio", a.WorstRatio, (10-5.886)/41.52, 0.001)
	if a.State != VerdictFresnel60 || a.WorstIndex != 500 {
		t.Fatalf("assessment %+v", a)
	}
}

// TestAssessVerdictBoundaries raises one obstacle mid-path to just inside
// and just outside each boundary, by a millimetre.
func TestAssessVerdictBoundaries(t *testing.T) {
	l, dtm := flatLink()
	const mid = 500
	r := FresnelRadius(l.LambdaM, 10000, 10000)
	bulge := EarthBulge(10000, 10000, l.K)
	for _, c := range []struct {
		name      string
		clearance float64 // wanted clearance below the LoS at mid-path
		want      VerdictState
	}{
		{"above LoS", -0.001, VerdictBlocked},
		{"grazing", 0.001, VerdictFresnel60},
		{"inside 60 %", 0.6*r - 0.001, VerdictFresnel60},
		{"at 60 %", 0.6*r + 0.001, VerdictFresnel100},
		{"inside F1", r - 0.001, VerdictFresnel100},
		{"clear of F1", r + 0.001, VerdictClear},
	} {
		obst := append([]float32(nil), dtm...)
		// Ground so low that only the mid obstacle matters.
		for i := range obst {
			obst[i] = -500
		}
		obst[0], obst[len(obst)-1] = 0, 0
		obst[mid] = float32(10 - bulge - c.clearance)
		a := l.Assess(obst)
		if a.State != c.want || a.WorstIndex != mid {
			t.Errorf("%s: state %q at %d (ratio %.6f), want %q at %d", c.name, a.State, a.WorstIndex, a.WorstRatio, c.want, mid)
		}
	}
}

func TestAssessMissingDataIsUnknown(t *testing.T) {
	l, dtm := flatLink()
	dtm[300] = float32(math.NaN())
	if a := l.Assess(dtm); a.State != VerdictUnknown || a.WorstIndex != -1 {
		t.Fatalf("assessment with a gap %+v", a)
	}
	l.HeightA = math.NaN() // no ground under endpoint A
	if a := l.Assess(make([]float32, 11)); a.State != VerdictUnknown {
		t.Fatalf("assessment without endpoint ground %+v", a)
	}
}

func TestDecimateKeepsEndpointsAndWorst(t *testing.T) {
	if got := Decimate(10, 1024); len(got) != 10 || got[9] != 9 {
		t.Fatalf("short profile decimated: %v", got)
	}
	for _, worst := range []int{1, 1777, 1778, 3198} {
		idx := Decimate(3200, 1024, worst)
		if len(idx) > 1024 {
			t.Fatalf("worst %d: %d indices", worst, len(idx))
		}
		if idx[0] != 0 || idx[len(idx)-1] != 3199 {
			t.Fatalf("worst %d: endpoints missing", worst)
		}
		found := false
		for i, v := range idx {
			if i > 0 && v <= idx[i-1] {
				t.Fatalf("worst %d: not strictly increasing at %d", worst, i)
			}
			found = found || v == worst
		}
		if !found {
			t.Fatalf("worst sample %d dropped by decimation", worst)
		}
	}
	// A negative keep index (no worst sample) is ignored.
	if idx := Decimate(3200, 1024, -1); len(idx) > 1024 {
		t.Fatal("negative keep broke the bound")
	}
}

// TestAssessEndpointNodataIsUnknown: a NaN at either end of the series
// being assessed makes the verdict unknown (#696 finding 2). Assess is
// the same for both models; the endpoint heights alone do not catch it,
// since they come from the DTM, so a DSM gap at an endpoint used to pass
// as "clear". The handler side is TestTerrainProfile_EndpointNodata.
func TestAssessEndpointNodataIsUnknown(t *testing.T) {
	nan := float32(math.NaN())
	l := Link{DistanceM: 20, HeightA: 10, HeightB: 10, LambdaM: Wavelength(FirmwareFreqMHz), K: DefaultKFactor}
	if a := l.Assess([]float32{0, 0, 0}); a.State != VerdictClear {
		t.Fatalf("control: %+v", a)
	}
	for _, s := range [][]float32{{nan, 0, 0}, {0, 0, nan}} {
		if a := l.Assess(s); a.State != VerdictUnknown || a.WorstIndex != -1 {
			t.Errorf("Assess(%v) = %+v, want unknown", s, a)
		}
	}
}

// TestAssessObstacleNextToEndpoint: the interior samples right next to
// each antenna are assessed, so an obstacle there blocks the link, in
// both directions (#696 finding 3).
func TestAssessObstacleNextToEndpoint(t *testing.T) {
	l, dtm := flatLink()
	n := len(dtm)
	for i := range dtm {
		dtm[i] = -500 // only the obstacle matters
	}
	dtm[0], dtm[n-1] = 0, 0
	for _, c := range []struct {
		name   string
		idx    int
		ha, hb float64
	}{
		{"next to A", 1, 10, 10},
		{"next to B", n - 2, 10, 10},
		{"next to A, B high", 1, 10, 200},
		{"next to B, A high", n - 2, 200, 10},
	} {
		obst := append([]float32(nil), dtm...)
		obst[c.idx] = 300 // far above any line of sight
		ll := l
		ll.HeightA, ll.HeightB = c.ha, c.hb
		if a := ll.Assess(obst); a.State != VerdictBlocked || a.WorstIndex != c.idx {
			t.Errorf("%s: %q at %d, want blocked at %d", c.name, a.State, a.WorstIndex, c.idx)
		}
	}
}
