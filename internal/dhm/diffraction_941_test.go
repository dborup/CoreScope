package dhm

import (
	"math"
	"math/rand"
	"testing"
)

// #941 M1 — J(ν) and the Bullington method (ITU-R P.526 §4.5.1, the same
// equations as P.1812-6 §4.3.1, eqs. 12–21).

func nearly(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s = %.12g, want %.12g ±%g", what, got, want, tol)
	}
}

// TestKnifeEdgeLoss941_ReferencePoints pins J(ν) at the points P.526
// quotes and at values computed with the reference implementation
// (OFCOM's Py1812 dl_bull, eq. 12): J is 0 at and below ν = −0.78, about
// 6.0 dB at grazing (ν = 0), and follows 6.9 + 20·log10(√((ν−0.1)²+1)+ν−0.1)
// above.
func TestKnifeEdgeLoss941_ReferencePoints(t *testing.T) {
	for _, c := range []struct{ nu, want float64 }{
		{-3, 0},
		{-0.78, 0},
		{-0.75, 0.20111676909217024}, // just above the cut-off
		{-0.5, 1.9592497062281762},
		{0, 6.032852208563606},
		{1, 13.925728934959924},
		{2.4, 20.53926612973203},
	} {
		nearly(t, "J", KnifeEdgeLoss(c.nu), c.want, 1e-12)
	}
	// Just above the cut-off the formula is already ~0: J is continuous.
	if j := KnifeEdgeLoss(-0.7799); j < 0 || j > 0.01 {
		t.Fatalf("J(-0.7799) = %g, want ~0", j)
	}
	// Monotonic: more obstruction never costs less.
	prev := -1.0
	for nu := -1.0; nu <= 5; nu += 0.01 {
		j := KnifeEdgeLoss(nu)
		if j < prev-1e-12 {
			t.Fatalf("J not monotonic at %g: %g < %g", nu, j, prev)
		}
		prev = j
	}
}

// p1812Profile is the synthetic profile the reference vectors were
// computed on: 201 samples 30 m apart.
func p1812Profile(phase float64, ridge bool, base float64) []float32 {
	g := make([]float32, 201)
	for i := range g {
		v := 20 + 15*math.Sin(float64(i)/17.0) + 8*math.Sin(float64(i)/5.3+phase) + base
		if ridge && i == 120 {
			v += 60
		}
		g[i] = float32(v)
	}
	return g
}

// TestBullington941_MatchesP1812Reference compares Link.Bullington with
// the reference implementation of the same equations (OFCOM's Py1812
// dl_bull, transcribed to plain Python; the script and its output are in
// the PR) on six profiles: four beyond the horizon, two in line of sight
// with the first Fresnel zone partly obstructed, and one fully clear.
// The heights are float32, as in the elevation grid; the reference ran on
// the float64 values, so the tolerance allows for the rounding.
func TestBullington941_MatchesP1812Reference(t *testing.T) {
	lambda := Wavelength(FirmwareFreqMHz)
	for _, c := range []struct {
		name       string
		phase      float64
		ridge      bool
		base       float64
		ta, ra     float64
		los        bool
		nu, luc, l float64
	}{
		{"nlos 10/2", 0, false, 0, 10, 2, false, 3.4312677090596244, 23.56216598364412, 33.482780255911536},
		{"nlos ridge", 1, true, 7, 10, 2, false, 3.6483390421866995, 24.088647115045568, 34.02601123874618},
		{"nlos 2/2", 2, false, 14, 2, 2, false, 2.4838853372485477, 20.82541086398213, 30.630790369781863},
		{"nlos 30/5", 3, false, 21, 30, 5, false, 0.903482531028743, 13.287475962261844, 22.30237761084539},
		{"los clear", 0, false, 0, 120, 100, true, -5.19935178699519, 0, 0},
		{"los 34/30", 1, false, 0, 34, 30, true, -0.49953297148681697, 1.9627285344579448, 4.7862470651803575},
		{"los 31/28", 2, false, 0, 31, 28, true, -0.3828451366374599, 2.8539766568595315, 6.684669669726345},
		{"los 40/22", 3, false, 0, 40, 22, true, -0.03449311882299629, 5.735301039481045, 11.964441255844807},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := p1812Profile(c.phase, c.ridge, c.base)
			// The reference generator used 20+15sin+8sin(+phase)+base with
			// phase = case index; reproduce its antenna heights the same way.
			link := Link{DistanceM: 6000, HeightA: float64(g[0]) + c.ta, HeightB: float64(g[200]) + c.ra, LambdaM: lambda, K: DefaultKFactor}
			b, ok := link.Bullington(g)
			if !ok {
				t.Fatal("no result")
			}
			if b.LoS != c.los {
				t.Fatalf("LoS = %v, want %v", b.LoS, c.los)
			}
			nearly(t, "nu", b.Nu, c.nu, 2e-4)
			nearly(t, "Luc", b.Luc, c.luc, 2e-3)
			nearly(t, "Lb", b.Loss, c.l, 2e-3)
		})
	}
}

// TestBullington941_SingleKnifeEdge: on flat ground with no curvature, a
// single ridge is the textbook knife edge, ν = h·√(2/λ·(1/d1 + 1/d2)),
// where h is the edge's height above the line of sight (negative below
// it), and the uncorrected loss is J(ν).
func TestBullington941_SingleKnifeEdge(t *testing.T) {
	lambda := Wavelength(FirmwareFreqMHz)
	const n, step = 401, 25.0
	d := (n - 1) * step
	for _, c := range []struct {
		at    int
		ridge float64
	}{
		{100, 40}, {200, 25}, {300, 12}, {150, 9}, {250, 5},
	} {
		g := make([]float32, n)
		g[c.at] = float32(c.ridge)
		link := Link{DistanceM: d, HeightA: 10, HeightB: 10, LambdaM: lambda, K: 1e12}
		b, ok := link.Bullington(g)
		if !ok {
			t.Fatal("no result")
		}
		d1 := float64(c.at) * step
		d2 := d - d1
		nu := (c.ridge - 10) * math.Sqrt(2/lambda*(1/d1+1/d2))
		nearly(t, "nu", b.Nu, nu, 1e-6)
		nearly(t, "Luc", b.Luc, KnifeEdgeLoss(nu), 1e-6)
		want := b.Luc + (1-math.Exp(-b.Luc/6))*(10+0.02*d/1000)
		nearly(t, "Lb", b.Loss, want, 1e-9)
		if b.LoS != (c.ridge < 10) {
			t.Fatalf("ridge %v: LoS = %v", c.ridge, b.LoS)
		}
	}
}

// TestBullington941_LoSBranchSharesTheProfileRatio is the DRY link with
// the terrain profile: on the line-of-sight branch, Bullington's ν is
// −√2 times the worst clearance ratio Link.Assess reports, so the profile
// verdict and the reach prediction read one formula.
func TestBullington941_LoSBranchSharesTheProfileRatio(t *testing.T) {
	rng := rand.New(rand.NewSource(941))
	lambda := Wavelength(FirmwareFreqMHz)
	checked := 0
	for trial := 0; trial < 400; trial++ {
		n := 3 + rng.Intn(400)
		g := make([]float32, n)
		for i := range g {
			g[i] = float32(30 + 20*math.Sin(float64(i)/(3+rng.Float64()*30)) + rng.Float64()*10)
		}
		link := Link{DistanceM: float64(n-1) * (10 + rng.Float64()*40),
			HeightA: float64(g[0]) + rng.Float64()*80, HeightB: float64(g[n-1]) + rng.Float64()*80,
			LambdaM: lambda, K: DefaultKFactor}
		a := link.Assess(g)
		b, ok := link.Bullington(g)
		if !ok || a.State == VerdictUnknown {
			t.Fatal("no result on a complete profile")
		}
		if b.LoS != (a.WorstRatio > 0) {
			t.Fatalf("trial %d: LoS %v but worst ratio %g", trial, b.LoS, a.WorstRatio)
		}
		if b.LoS {
			nearly(t, "nu", b.Nu, -math.Sqrt2*a.WorstRatio, 1e-9*math.Max(1, math.Abs(b.Nu)))
			checked++
		}
	}
	if checked < 50 {
		t.Fatalf("only %d line-of-sight trials", checked)
	}
}

// TestBullington941_UnknownWithGaps: like Assess, a gap anywhere makes no
// prediction.
func TestBullington941_UnknownWithGaps(t *testing.T) {
	g := []float32{1, 2, float32(math.NaN()), 4, 5}
	link := Link{DistanceM: 100, HeightA: 10, HeightB: 10, LambdaM: 0.3, K: DefaultKFactor}
	if _, ok := link.Bullington(g); ok {
		t.Fatal("a profile with a gap got a loss")
	}
	if _, ok := (Link{DistanceM: 100, HeightA: math.NaN(), HeightB: 10, LambdaM: 0.3, K: 1}).Bullington([]float32{1, 2, 3}); ok {
		t.Fatal("an unknown antenna height got a loss")
	}
	// Two samples: no interior point, line of sight, no diffraction.
	b, ok := link.Bullington([]float32{1, 1})
	if !ok || !b.LoS || b.Loss != 0 {
		t.Fatalf("two samples: %+v %v", b, ok)
	}
}

// TestFreeSpaceLoss941: FSPL = 20·log10(4πd/λ); 40 km at 869.618 MHz is
// the 123.3 dB of the M0 research.
func TestFreeSpaceLoss941(t *testing.T) {
	lambda := Wavelength(FirmwareFreqMHz)
	nearly(t, "fspl 40 km", FreeSpaceLoss(40000, lambda), 123.27, 0.01)
	nearly(t, "fspl 1 km", FreeSpaceLoss(1000, lambda), 91.236, 0.01)
	// 20 dB per decade.
	nearly(t, "decade", FreeSpaceLoss(10000, lambda)-FreeSpaceLoss(1000, lambda), 20, 1e-9)
}
