package dhm

import "math"

// #941 M1 — diffraction for the predicted reach: the knife-edge loss J(ν)
// and the Bullington method of ITU-R P.526 §4.5.1 (the same equations as
// P.1812-6 §4.3.1, eqs. 12–21), next to the profile's link physics.
//
// The equations were checked against the reference implementation of
// P.1812-6 (OFCOM's Py1812, dl_bull) on synthetic profiles;
// TestBullington941_MatchesP1812Reference pins its results.
//
// Bullington, written with the profile's own clearance (Link.clearanceAt:
// the line of sight minus the surface and the earth bulge, c_i, at
// distance d_i of a path of length d):
//
//   - the path is in line of sight when every c_i > 0 (P.526's
//     S_tim < S_tr). ν is then the largest −c_i·√2 / F1_i, i.e. −√2 times
//     the worst clearance ratio Link.Assess reports: one formula for the
//     profile verdict and the reach prediction;
//   - otherwise the Bullington point is where the steepest slopes from
//     both ends meet. With A = max(−c_i / d_i) and B = max(−c_i / (d−d_i)),
//     P.526's eqs. reduce to ν_b = √(2·A·B·d / λ), independent of the
//     antenna heights' difference (the clearances already carry it);
//   - L_uc = J(ν), and the path loss L_b = L_uc + (1 − e^(−L_uc/6))·(10 +
//     0.02·d_km).

// KnifeEdgeLoss is J(ν) in dB (P.526 eq. 31, P.1812 eq. 12): 0 for
// ν ≤ −0.78, else 6.9 + 20·log10(√((ν−0.1)² + 1) + ν − 0.1).
func KnifeEdgeLoss(nu float64) float64 {
	if !(nu > -0.78) {
		return 0
	}
	x := nu - 0.1
	return 6.9 + 20*math.Log10(math.Sqrt(x*x+1)+x)
}

// bullingtonLoss is L_b of a path dM metres long whose uncorrected loss is
// luc (P.1812 eq. 21).
func bullingtonLoss(luc, dM float64) float64 {
	return luc + (1-math.Exp(-luc/6))*(10+0.02*dM/1000)
}

// FreeSpaceLoss is the free-space basic transmission loss in dB over dM
// metres at wavelength lambdaM: 20·log10(4π·d / λ).
func FreeSpaceLoss(dM, lambdaM float64) float64 {
	return 20 * math.Log10(4*math.Pi*dM/lambdaM)
}

// Bullington is the diffraction of one path.
type Bullington struct {
	LoS  bool    // every interior sample clears the line of sight
	Nu   float64 // ν_max on the line-of-sight branch, ν_b beyond it
	Luc  float64 // J(Nu), dB
	Loss float64 // L_b, dB: Luc with P.526's distance correction
}

// Bullington is the path's diffraction by P.526 §4.5.1 over surface,
// evenly spaced as in Assess, with the earth bulge for l.K. ok is false
// when any sample or an antenna height is missing: like Assess, a partial
// profile gets no number. With fewer than three samples there is no
// interior point and no diffraction.
//
// It is the reference the reach sweep (coverage.go) is tested against:
// O(n) per path, written for clarity rather than speed.
func (l Link) Bullington(surface []float32) (Bullington, bool) {
	n := len(surface)
	if n < 2 || math.IsNaN(l.HeightA) || math.IsNaN(l.HeightB) {
		return Bullington{}, false
	}
	for _, s := range surface {
		if math.IsNaN(float64(s)) {
			return Bullington{}, false
		}
	}
	step := l.DistanceM / float64(n-1)
	worstRatio := math.Inf(1)
	a, b := math.Inf(-1), math.Inf(-1)
	for i := 1; i < n-1; i++ {
		d1, c, ratio := l.clearanceAt(float64(surface[i]), i, step)
		worstRatio = math.Min(worstRatio, ratio)
		a = math.Max(a, -c/d1)
		b = math.Max(b, -c/(l.DistanceM-d1))
	}
	return bullingtonFrom(worstRatio, a, b, l.DistanceM, l.LambdaM), true
}

// bullingtonFrom finishes a path from its worst clearance ratio and its
// two slope maxima (see the file comment). With no interior sample the
// ratio is +Inf: line of sight, ν = −Inf, no loss.
func bullingtonFrom(worstRatio, a, b, dM, lambdaM float64) Bullington {
	out := Bullington{LoS: a < 0}
	if out.LoS {
		out.Nu = -math.Sqrt2 * worstRatio
	} else {
		out.Nu = math.Sqrt(2 * a * b * dM / lambdaM)
	}
	out.Luc = KnifeEdgeLoss(out.Nu)
	out.Loss = bullingtonLoss(out.Luc, dM)
	return out
}
