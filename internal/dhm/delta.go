package dhm

import "math"

// #941 M2 — the "complete method" for a general terrestrial path,
// ITU-R P.526-16 (11/2025) §4.5.2 ("delta-Bullington"), next to the
// Bullington method of §4.5.1 (diffraction.go):
//
//	L_d = L_bulla + max(0, L_sph − L_bulls)
//
//   - L_bulla is Bullington over the actual profile and antenna heights
//     (Link.Bullington, bullingtonFrom);
//   - L_bulls is Bullington again over the least-squares smooth surface:
//     every profile height set to zero, the antennas at h'ts = hts − hstd
//     and h'rs = hrs − hsrd above it (smoothBullington);
//   - L_sph is the spherical-earth diffraction loss over the path length
//     with the same effective heights (sphericalEarth.loss).
//
// It adds the smooth-earth diffraction that plain Bullington misses
// beyond the radio horizon over flat land and sea, and never lowers the
// Bullington loss: on a short, clear line of sight L_sph is 0 and L_d
// equals L_bulla.
//
// What the equations were checked against. The ITU-R site refused the
// P.526 PDF from this build environment, so the P.526 text itself was not
// read here (#941 M2 report, residual). The equation numbers below are
// those of ITU-R P.1812-6, which states the same method: §4.3.1 (eqs.
// 12–21, Bullington), §4.3.2 (eqs. 22–27, spherical earth), §4.3.3 (eqs.
// 29–36, its first term), §4.3.4 (eqs. 37–39, delta-Bullington) and
// Attachment 1 §5.6.1–5.6.2 (eqs. 85–91d, the smooth surface and its
// diffraction heights). The implementation follows OFCOM's reference
// implementation of P.1812-6 (Py1812: dl_delta_bull, dl_se, dl_se_ft,
// smooth_earth_heights), transcribed to plain Python in
// testdata/delta_bullington_941_vectors.py, whose output
// TestDeltaBullington941_MatchesP1812Reference pins.
//
// Choices:
//   - L_bulls is the Bullington loss of the zero-height profile itself,
//     not P.1812's optional closed form (its Attachment 4). It is found
//     exactly, sample for sample, in O(log n): every quantity it
//     maximises is unimodal along a smooth path (smoothBullington);
//   - the ground is land (ε_r 22, σ 0.003 S/m, P.1812's land constants)
//     and the polarisation vertical, as MeshCore antennas are. A sea path
//     would lose a little less; the validation harness says whether it
//     matters;
//   - the smooth surface comes from two running sums, so the sweep fits
//     it in O(1) per receiver (radialSweep.run).

// PathModel names a propagation model of the predicted reach.
type PathModel string

const (
	// PathBullington is free-space loss plus Bullington (P.526 §4.5.1),
	// the default.
	PathBullington PathModel = "bullington"
	// PathDelta is free-space loss plus the complete method (P.526
	// §4.5.2).
	PathDelta PathModel = "delta"
	// PathFreeSpace is free-space loss alone: the validation's baseline,
	// never served.
	PathFreeSpace PathModel = "fspl"
)

// CoverageModelDelta is PathDelta's id with its version (CoverageModel is
// Bullington's): part of the cache key and the response.
const CoverageModelDelta = "delta-bullington-p526-v1"

// ID is the model's versioned id; an unknown model is Bullington's.
func (m PathModel) ID() string {
	switch m {
	case PathDelta:
		return CoverageModelDelta
	case PathFreeSpace:
		return "fspl-v1"
	}
	return CoverageModel
}

// ParsePathModel reads a served model: "bullington" or "delta".
func ParsePathModel(s string) (PathModel, bool) {
	switch PathModel(s) {
	case PathBullington, PathDelta:
		return PathModel(s), true
	}
	return "", false
}

// Ground constants of the spherical-earth loss: P.1812's land path.
const (
	groundPermittivity  = 22.0
	groundConductivityS = 0.003
)

// DeltaBullington is the complete method's result for one path.
type DeltaBullington struct {
	Actual Bullington // L_bulla, over the actual profile
	// HeightTx, HeightRx are h'ts, h'rs: the antennas above the smooth
	// surface (eqs. 37a–b).
	HeightTx, HeightRx float64
	Smooth             float64 // L_bulls, dB
	Spherical          float64 // L_sph, dB
	Loss               float64 // L_d, dB
}

// smoothFit holds what the complete method needs of a profile beyond
// Bullington: the sums v1, v2 of the least-squares smooth surface (eqs.
// 85–86) and the obstruction terms of eqs. 89a–c, the largest height H_i
// of the profile above the straight line between the antennas, and its
// largest slopes seen from either end.
type smoothFit struct {
	v1, v2           float64
	hobs, aobt, aobr float64
}

// noObstruction is a smoothFit's obstruction terms before any sample.
func noObstruction() smoothFit {
	return smoothFit{hobs: math.Inf(-1), aobt: math.Inf(-1), aobr: math.Inf(-1)}
}

// diffractionHeights are hstd, hsrd (eqs. 87–91d): the smooth surface's
// heights at both ends, lowered by an obstruction above the straight line
// in proportion to its slopes, and never above the ground at that end
// (h0, hn). The path is dM long.
func (f smoothFit) diffractionHeights(dM, h0, hn float64) (hstd, hsrd float64) {
	hst := (2*f.v1*dM - f.v2) / (dM * dM)
	hsr := (f.v2 - f.v1*dM) / (dM * dM)
	if f.hobs > 0 {
		gt := f.aobt / (f.aobt + f.aobr)
		hst -= f.hobs * gt
		hsr -= f.hobs * (1 - gt)
	}
	return math.Min(hst, h0), math.Min(hsr, hn)
}

// deltaFrom finishes the complete method for a path dM long of n
// intervals step apart, from its Bullington result (L_bulla), its smooth
// fit, the ground at both ends and the antennas above sea level.
func deltaFrom(bl Bullington, fit smoothFit, n int, step, h0, hn, hts, hrs, k, lambda float64, se *sphericalEarth) DeltaBullington {
	dM := float64(n) * step
	hstd, hsrd := fit.diffractionHeights(dM, h0, hn)
	out := DeltaBullington{Actual: bl, HeightTx: hts - hstd, HeightRx: hrs - hsrd}
	out.Smooth = smoothBullington(out.HeightTx, out.HeightRx, n, step, k, lambda).Loss
	out.Spherical = se.loss(dM, out.HeightTx, out.HeightRx)
	out.Loss = bl.Loss + math.Max(0, out.Spherical-out.Smooth)
	return out
}

// DeltaBullington is the path's diffraction by the complete method (P.526
// §4.5.2) over surface, evenly spaced as in Assess, with the earth bulge
// for l.K. Like Bullington, ok is false when a sample or an antenna
// height is missing. It is the reference the sweep (coverage.go) is
// tested against, written for clarity rather than speed.
func (l Link) DeltaBullington(surface []float32) (DeltaBullington, bool) {
	bl, ok := l.Bullington(surface)
	if !ok {
		return DeltaBullington{}, false
	}
	n := len(surface) - 1
	step := l.DistanceM / float64(n)
	fit := noObstruction()
	for i := 1; i <= n; i++ {
		// Eqs. 85–86 with d_i = i·step.
		di, dp := float64(i)*step, float64(i-1)*step
		hi, hp := float64(surface[i]), float64(surface[i-1])
		fit.v1 += (di - dp) * (hi + hp)
		fit.v2 += (di - dp) * (hi*(2*di+dp) + hp*(di+2*dp))
		if i == n {
			break
		}
		// Eq. 89d: the height above the straight line between the antennas.
		hh := hi - (l.HeightA*(l.DistanceM-di)+l.HeightB*di)/l.DistanceM
		fit.hobs = math.Max(fit.hobs, hh)
		fit.aobt = math.Max(fit.aobt, hh/di)
		fit.aobr = math.Max(fit.aobr, hh/(l.DistanceM-di))
	}
	se := newSphericalEarth(l.LambdaM, l.K)
	return deltaFrom(bl, fit, n, step, float64(surface[0]), float64(surface[n]), l.HeightA, l.HeightB, l.K, l.LambdaM, &se), true
}

// PathLoss is the path's basic transmission loss in dB under model m:
// free-space loss plus the model's diffraction. ok is false without a
// complete profile. It is what a node in reach and the validation
// harness use, the point-to-point twin of the sweep.
func (l Link) PathLoss(surface []float32, m PathModel) (float64, bool) {
	fspl := FreeSpaceLoss(l.DistanceM, l.LambdaM)
	switch m {
	case PathFreeSpace:
		for _, s := range surface {
			if math.IsNaN(float64(s)) {
				return math.NaN(), false
			}
		}
		return fspl, len(surface) >= 2
	case PathDelta:
		if len(surface) < 2 {
			return math.NaN(), false
		}
		d, ok := l.DeltaBullington(surface)
		return fspl + d.Loss, ok
	}
	b, ok := l.Bullington(surface)
	return fspl + b.Loss, ok
}

// smoothBullington is Link.Bullington over a smooth path: n intervals
// step apart, every surface height zero, antennas hts and hrs above it,
// the earth bulge for k (P.526 §4.5.2's L_bulls). It finds each extreme
// exactly, on the same samples, without visiting them all: with the
// clearance c_i = hts + (hrs − hts)·i/n − β·i·(n−i), β = step²/(2kR), the
// slope from the transmitter, −c_i/(i·step), is concave in i with its
// continuous top at i = √(hts/β), so the largest sample slope is at one
// of the two integers around it; the slope from the receiver likewise at
// n − √(hrs/β). The clearance ratio is quasiconvex (a convex clearance
// over a concave Fresnel radius), so its minimum is a ternary search.
// O(log n) instead of O(n), with Link.Bullington's result
// (TestSmoothBullington941_MatchesTheProfile).
func smoothBullington(hts, hrs float64, n int, step, k, lambda float64) Bullington {
	if n < 2 {
		return bullingtonFrom(math.Inf(1), math.Inf(-1), math.Inf(-1), float64(n)*step, lambda)
	}
	l := Link{DistanceM: float64(n) * step, HeightA: hts, HeightB: hrs, LambdaM: lambda, K: k}
	beta := EarthBulge(step, step, k)
	a := maxConcave(1, n-1, math.Sqrt(hts/beta), func(i int) float64 {
		d1, c, _ := l.clearanceAt(0, i, step)
		return -c / d1
	})
	b := maxConcave(1, n-1, float64(n)-math.Sqrt(hrs/beta), func(i int) float64 {
		d1, c, _ := l.clearanceAt(0, i, step)
		return -c / (l.DistanceM - d1)
	})
	worst := math.Inf(1)
	if a < 0 {
		worst = -maxUnimodal(1, n-1, func(i int) float64 {
			_, _, ratio := l.clearanceAt(0, i, step)
			return -ratio
		})
	}
	return bullingtonFrom(worst, a, b, l.DistanceM, lambda)
}

// maxConcave is the largest f(i) for lo ≤ i ≤ hi, f concave with its
// continuous maximum at x: at ⌊x⌋ or ⌈x⌉, each clamped to the range.
func maxConcave(lo, hi int, x float64, f func(int) float64) float64 {
	at := func(v float64) int {
		if !(v > float64(lo)) { // NaN too
			return lo
		}
		if v > float64(hi) {
			return hi
		}
		return int(v)
	}
	return math.Max(f(at(math.Floor(x))), f(at(math.Ceil(x))))
}

// maxUnimodal is the largest f(i) for lo ≤ i ≤ hi, f rising then falling
// (a plateau only at the top): a ternary search on the integers.
func maxUnimodal(lo, hi int, f func(int) float64) float64 {
	for hi-lo > 2 {
		m1, m2 := lo+(hi-lo)/3, hi-(hi-lo)/3
		if f(m1) < f(m2) {
			lo = m1 + 1
		} else {
			hi = m2
		}
	}
	best := f(lo)
	for i := lo + 1; i <= hi; i++ {
		best = math.Max(best, f(i))
	}
	return best
}

// sphericalEarth is the spherical-earth diffraction loss of one frequency
// and k: P.1812-6 §4.3.2–4.3.3 in P.1812's units (km, m, GHz).
type sphericalEarth struct {
	fGHz, lambda, aeKm float64
	ae                 firstTerm // for aeKm, the common case
}

func newSphericalEarth(lambdaM, k float64) sphericalEarth {
	s := sphericalEarth{fGHz: SpeedOfLight / lambdaM / 1e9, lambda: lambdaM, aeKm: k * EarthRadiusM / 1000}
	s.ae = newFirstTerm(s.aeKm, s.fGHz)
	return s
}

// loss is L_sph in dB of a path dM long between antennas hte and hre
// metres above the smooth surface (eqs. 22–27): the first-term loss
// beyond the marginal line of sight; inside it zero when the path clears
// the curved earth by the required clearance, otherwise the first-term
// loss for the effective radius that puts the path at grazing, scaled by
// the missing clearance.
func (s *sphericalEarth) loss(dM, hte, hre float64) float64 {
	d := dM / 1000
	ap := s.aeKm
	dlos := math.Sqrt(2*ap) * (math.Sqrt(0.001*hte) + math.Sqrt(0.001*hre)) // eq. 22
	if d >= dlos {
		return s.ae.loss(d, hte, hre)
	}
	c := (hte - hre) / (hte + hre)        // eq. 24d
	m := 250 * d * d / (ap * (hte + hre)) // eq. 24e
	// Eq. 24c; the argument stays in [−1, 1] in exact arithmetic.
	arg := math.Max(-1, math.Min(1, 3*c/2*math.Sqrt(3*m/math.Pow(m+1, 3))))
	b := 2 * math.Sqrt((m+1)/(3*m)) * math.Cos(math.Pi/3+math.Acos(arg)/3)
	dse1 := d / 2 * (1 + b)                                                // eq. 24a
	dse2 := d - dse1                                                       // eq. 24b
	hse := ((hte-500*dse1*dse1/ap)*dse2 + (hre-500*dse2*dse2/ap)*dse1) / d // eq. 23
	hreq := 17.456 * math.Sqrt(dse1*dse2*s.lambda/d)                       // eq. 25
	if hse > hreq {
		return 0
	}
	aem := 500 * math.Pow(d/(math.Sqrt(hte)+math.Sqrt(hre)), 2) // eq. 26
	ldft := newFirstTerm(aem, s.fGHz).loss(d, hte, hre)
	return (1 - hse/hreq) * math.Max(0, ldft) // eq. 27
}

// firstTerm is the first-term spherical-earth loss of one effective
// radius aKm and frequency (eqs. 29–36), vertical polarisation, land.
type firstTerm struct {
	beta, xPerKm, yPerM, gMin float64
}

func newFirstTerm(aKm, fGHz float64) firstTerm {
	sig := 18 * groundConductivityS / fGHz
	kh := 0.036 * math.Pow(aKm*fGHz, -1.0/3) * math.Pow(math.Pow(groundPermittivity-1, 2)+sig*sig, -0.25) // eq. 29a
	kv := kh * math.Sqrt(groundPermittivity*groundPermittivity+sig*sig)                                   // eq. 29b
	k2 := kv * kv
	beta := (1 + 1.6*k2 + 0.67*k2*k2) / (1 + 4.5*k2 + 1.53*k2*k2) // eq. 30
	return firstTerm{
		beta:   beta,
		xPerKm: 21.88 * beta * math.Cbrt(fGHz/(aKm*aKm)), // eq. 31
		yPerM:  0.9575 * beta * math.Cbrt(fGHz*fGHz/aKm), // eqs. 32a–b
		gMin:   2 + 20*math.Log10(kv),                    // below eq. 34
	}
}

// loss is L_dft in dB over dKm between antennas hte and hre metres up
// (eq. 36).
func (t firstTerm) loss(dKm, hte, hre float64) float64 {
	x := t.xPerKm * dKm
	var fx float64
	if x >= 1.6 { // eq. 33
		fx = 11 + 10*math.Log10(x) - 17.6*x
	} else {
		fx = -20*math.Log10(x) - 5.6488*math.Pow(x, 1.425)
	}
	return -fx - t.heightGain(hte) - t.heightGain(hre)
}

// heightGain is G(Y) for an antenna h metres up (eqs. 34–35), at least
// gMin.
func (t firstTerm) heightGain(h float64) float64 {
	b := t.beta * t.yPerM * h
	g := math.Inf(-1)
	switch {
	case b > 2:
		g = 17.6*math.Sqrt(b-1.1) - 5*math.Log10(b-1.1) - 8
	case b > 0:
		g = 20 * math.Log10(b+0.1*b*b*b)
	}
	return math.Max(g, t.gMin)
}
