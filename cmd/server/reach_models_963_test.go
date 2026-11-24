package main

// #963 M0 — candidate propagation models for the predicted reach, as
// test-only prototypes next to the #941 harness: research code, in no
// binary. Each candidate is a basic transmission loss in dB for one path,
// given the bare-earth (DTM) and the surface (DSM) profiles along it,
// evenly spaced, both ends included. Today's model is delta-Bullington over
// the DTM (P.526 §4.5.2, dhm.Link.PathLoss).
//
// Candidates (ids as in the report):
//   - delta: today, raw.
//   - delta+env25: today as served, with the 25 dB environment loss of
//     #962 (a constant, so the same ranking as delta).
//   - tworay: the larger of delta and the plane-earth (two-ray asymptote)
//     loss 40·log10(d) − 20·log10(h1·h2), never below free space, with the
//     antenna heights above the smooth-earth surface delta fits (P.1812
//     eqs. 37a–b; the antenna heights on flat ground).
//   - tworay+p2108, delta+p2108: ITU-R P.2108 §3.1 by the book (P.526-16
//     §4.5): a terminal below its clutter's representative height is
//     raised to it for the path, plus the terminal correction, by a
//     clutter class per end (open/rural land included).
//   - delta+p2108c: the same for suburban and urban/forest ends only; an
//     end in open land is left to delta, whose smooth-earth term already
//     carries the ground's height gain (eq. 2b's effect) and knows when
//     the ground falls away below a node on high ground.
//   - tworay+dsm: the larger of delta over the DSM and the plane-earth loss.
//   - hata-open, hata-suburban: Okumura–Hata (Hata 1980), terrain-blind,
//     with the higher effective height as the base station; outside its
//     validity (hb ≥ 30 m, d ≥ 1 km) here, never below free space.
//   - dsm: delta over the DSM, with the bare earth within 150 m of either
//     end (antennas stand in a clearing or above their own roof).
//   - dsm-raw: delta over the DSM everywhere between the ends.
//   - mm-proxy: map.meshcore.io's approach (src/coverage-model.js, MIT):
//     25 m of forest on the bare earth outside 150 m of the ends, P.2108
//     clutter at both ends and a 10 dB fade margin. Forest and clutter
//     classes come from the DHM canopy (DSM − DTM), not from OSM, and the
//     diffraction is delta-Bullington, not their Deygout (A: a proxy).

import (
	"math"
	"sort"

	"github.com/meshcore-analyzer/dhm"
)

const (
	cand963Delta       = "delta"
	cand963DeltaEnv    = "delta+env25"
	cand963TwoRay      = "tworay"
	cand963TwoRayP2108 = "tworay+p2108"
	cand963TwoRayDSM   = "tworay+dsm"
	cand963HataOpen    = "hata-open"
	cand963HataSub     = "hata-suburban"
	cand963DeltaP2108  = "delta+p2108"
	cand963DeltaP2108C = "delta+p2108c"
	cand963DSM         = "dsm"
	cand963DSMRaw      = "dsm-raw"
	cand963MM          = "mm-proxy"
)

// candidates963 are the scored candidates, today first.
var candidates963 = []string{cand963Delta, cand963DeltaEnv, cand963TwoRay, cand963TwoRayP2108, cand963TwoRayDSM,
	cand963HataOpen, cand963HataSub, cand963DeltaP2108, cand963DeltaP2108C, cand963DSM, cand963DSMRaw, cand963MM}

const (
	envLoss963Db      = 25   // map.coverage.environmentLossDb's default (#962)
	fadeMargin963Db   = 10   // map.meshcore.io's FADE_MARGIN_DB
	forestHeight963M  = 25   // map.meshcore.io's FOREST_HEIGHT
	clearance963M     = 150  // map.meshcore.io's FOREST_CLEARANCE, also the DSM's
	forestCanopy963M  = 10   // A: a DSM − DTM this high is forest (Danish houses are lower)
	clutterTall963M   = 3    // A: a DSM − DTM this high is clutter, not ground noise
	clutterShare963   = 0.3  // A: below this share of tall cells an end is open
	clutterUrban963M  = 12   // A: tall cells' 75th percentile from here is urban / forest
	clutterWindow963M = 100  // A: the clutter class's window radius around an end
	streetWidth963M   = 27.0 // P.2108 §3.1 w_s
	minAntenna963M    = 1.0  // A: heights below this are 1 m in the log terms
)

// clutterClass963 is a P.2108 clutter type with its representative height
// (ITU-R P.2108-2 §3.1, Table 3): open/rural 10 m, suburban 10 m, urban /
// trees / forest 15 m.
type clutterClass963 int

const (
	clutterOpen963 clutterClass963 = iota
	clutterSuburban963
	clutterUrban963
)

var clutterHeight963 = [...]float64{10, 10, 15}

// planeEarthLoss963 is the plane-earth loss over dM metres between
// antennas h1, h2 metres above a flat reflecting ground: the two-ray
// asymptote 40·log10(d) − 20·log10(h1·h2) beyond the breakpoint, never
// below free space (Rappaport, Wireless Communications 2nd ed., §4.6).
func planeEarthLoss963(dM, h1, h2, lambda float64) float64 {
	h1, h2 = math.Max(h1, minAntenna963M), math.Max(h2, minAntenna963M)
	return math.Max(dhm.FreeSpaceLoss(dM, lambda), 40*math.Log10(dM)-20*math.Log10(h1*h2))
}

// p2108TerminalLoss963 is ITU-R P.2108-2 §3.1's height-gain terminal
// correction (eqs. 2a–2g, 0.03–3 GHz) for an antenna hM above the ground
// in clutter class cl: 0 at or above the class's representative height R;
// below it K_h2·log10(R/h) in open/rural land (2b, K_h2 = 21.8 +
// 6.2·log10(f GHz)), and J(ν) − 6.03 among suburban and urban clutter
// (2a, ν = 0.342·√f·√(h_dif·θ_clut), θ_clut = arctan(h_dif / 27 m) in
// degrees), never negative. It corrects a path computed with the terminal
// at R (P.526-16 §4.5), not one at hM.
func p2108TerminalLoss963(hM float64, cl clutterClass963, freqMHz float64) float64 {
	r := clutterHeight963[cl]
	if hM >= r {
		return 0
	}
	fGHz := freqMHz / 1000
	if cl == clutterOpen963 {
		h := math.Max(hM, minAntenna963M)
		return (21.8 + 6.2*math.Log10(fGHz)) * math.Log10(r/h)
	}
	hdif := r - hM
	theta := math.Atan(hdif/streetWidth963M) * 180 / math.Pi
	nu := 0.342 * math.Sqrt(fGHz) * math.Sqrt(hdif*theta)
	return math.Max(0, dhm.KnifeEdgeLoss(nu)-6.03)
}

// mmClutterLoss963 is map.meshcore.io's clutterLoss: no clutter in open
// land (their representative height 0 m), eq. (2a) among suburban and
// urban clutter, added to the path at the antenna's own height.
func mmClutterLoss963(hM float64, cl clutterClass963, freqMHz float64) float64 {
	if cl == clutterOpen963 {
		return 0
	}
	return p2108TerminalLoss963(hM, cl, freqMHz)
}

type hataEnv963 int

const (
	hataOpen963 hataEnv963 = iota
	hataSuburban963
)

// hataLoss963 is Okumura–Hata (Hata 1980) for a small or medium city's
// mobile correction, then the suburban or open-area correction; hb is the
// base station's effective height, hm the mobile's height above ground.
// Never below free space.
func hataLoss963(dM, hb, hm, freqMHz float64, env hataEnv963) float64 {
	hb = math.Max(hb, minAntenna963M)
	hm = math.Min(math.Max(hm, minAntenna963M), 10)
	lf := math.Log10(freqMHz)
	a := (1.1*lf-0.7)*hm - (1.56*lf - 0.8)
	l := 69.55 + 26.16*lf - 13.82*math.Log10(hb) - a + (44.9-6.55*math.Log10(hb))*math.Log10(dM/1000)
	switch env {
	case hataSuburban963:
		x := math.Log10(freqMHz / 28)
		l -= 2*x*x + 5.4
	default:
		l -= 4.78*lf*lf - 18.33*lf + 40.94
	}
	return math.Max(l, dhm.FreeSpaceLoss(dM, dhm.Wavelength(freqMHz)))
}

// classifyClutter963 is the clutter class of an end from the canopy
// heights (DSM − DTM) around it; missing cells are left out.
func classifyClutter963(canopy []float64) clutterClass963 {
	var v []float64
	tall := 0
	for _, h := range canopy {
		if math.IsNaN(h) {
			continue
		}
		v = append(v, h)
		if h >= clutterTall963M {
			tall++
		}
	}
	if len(v) == 0 || float64(tall)/float64(len(v)) < clutterShare963 {
		return clutterOpen963
	}
	sort.Float64s(v)
	if v[int(math.Ceil(0.75*float64(len(v))))-1] >= clutterUrban963M {
		return clutterUrban963
	}
	return clutterSuburban963
}

// clutterAt963 is the clutter class around (e, n) from a DTM and a DSM
// grid.
func clutterAt963(tg, sg *dhm.CoverageGrid, e, n, res float64) clutterClass963 {
	k := int(clutterWindow963M / res)
	var canopy []float64
	for dy := -k; dy <= k; dy++ {
		for dx := -k; dx <= k; dx++ {
			x, y := float64(dx)*res, float64(dy)*res
			if math.Hypot(x, y) > clutterWindow963M {
				continue
			}
			canopy = append(canopy, float64(sg.Sample(e+x, n+y)-tg.Sample(e+x, n+y)))
		}
	}
	return classifyClutter963(canopy)
}

// path963 is one path's input to the candidates.
type path963 struct {
	dtm, dsm     []float32 // evenly spaced, both ends included
	distanceM    float64
	txAGL, rxAGL float64 // antennas above the ground
	freqMHz, k   float64
	endClass     [2]clutterClass963
}

// candidateLosses963 is every candidate's loss for one path; NaN without a
// complete profile.
func candidateLosses963(p path963) map[string]float64 {
	out := make(map[string]float64, len(candidates963))
	for _, c := range candidates963 {
		out[c] = math.NaN()
	}
	n := len(p.dtm) - 1
	if n < 1 || len(p.dsm) != n+1 {
		return out
	}
	lambda := dhm.Wavelength(p.freqMHz)
	link := dhm.Link{DistanceM: p.distanceM, HeightA: float64(p.dtm[0]) + p.txAGL, HeightB: float64(p.dtm[n]) + p.rxAGL,
		LambdaM: lambda, K: p.k}
	db, ok := link.DeltaBullington(p.dtm)
	if !ok {
		return out
	}
	fspl := dhm.FreeSpaceLoss(p.distanceM, lambda)
	delta := fspl + db.Loss
	pe := planeEarthLoss963(p.distanceM, db.HeightTx, db.HeightRx, lambda)
	// P.2108 by the book: the path with each terminal below its clutter's
	// height raised to it, then the terminal corrections; openToo false
	// leaves ends in open land as they are.
	byTheBook := func(openToo bool) (dl, pl, cl float64) {
		h := [2]float64{p.txAGL, p.rxAGL}
		raised := false
		for k, c := range p.endClass {
			if c == clutterOpen963 && !openToo {
				continue
			}
			cl += p2108TerminalLoss963(h[k], c, p.freqMHz)
			if r := clutterHeight963[c]; h[k] < r {
				h[k], raised = r, true
			}
		}
		if !raised {
			return delta, pe, cl
		}
		rl := link
		rl.HeightA, rl.HeightB = float64(p.dtm[0])+h[0], float64(p.dtm[n])+h[1]
		rdb, _ := rl.DeltaBullington(p.dtm)
		return fspl + rdb.Loss, planeEarthLoss963(p.distanceM, rdb.HeightTx, rdb.HeightRx, lambda), cl
	}
	raisedDelta, raisedPE, clutter := byTheBook(true)
	clutterDelta, _, clutterOnly := byTheBook(false)
	mmClutter := mmClutterLoss963(p.txAGL, p.endClass[0], p.freqMHz) + mmClutterLoss963(p.rxAGL, p.endClass[1], p.freqMHz)
	hb, hm := db.HeightTx, p.rxAGL
	if db.HeightRx > db.HeightTx {
		hb, hm = db.HeightRx, p.txAGL
	}

	step := p.distanceM / float64(n)
	clear := func(i int) bool { d := float64(i) * step; return d < clearance963M || p.distanceM-d < clearance963M }
	dsm, raw, forest := make([]float32, n+1), make([]float32, n+1), make([]float32, n+1)
	for i := range p.dtm {
		dsm[i], raw[i], forest[i] = p.dsm[i], p.dsm[i], p.dtm[i]
		if i == 0 || i == n {
			raw[i] = p.dtm[i]
		}
		if clear(i) {
			dsm[i] = p.dtm[i]
			continue
		}
		if p.dsm[i]-p.dtm[i] >= forestCanopy963M {
			forest[i] = p.dtm[i] + forestHeight963M
		}
	}
	lossOver := func(s []float32) float64 {
		l, ok := link.PathLoss(s, dhm.PathDelta)
		if !ok {
			return math.NaN()
		}
		return l
	}
	lossDSM := lossOver(dsm)

	out[cand963Delta] = delta
	out[cand963DeltaEnv] = delta + envLoss963Db
	out[cand963TwoRay] = math.Max(delta, pe)
	out[cand963TwoRayP2108] = math.Max(raisedDelta, raisedPE) + clutter
	out[cand963TwoRayDSM] = math.Max(lossDSM, pe)
	out[cand963HataOpen] = hataLoss963(p.distanceM, hb, hm, p.freqMHz, hataOpen963)
	out[cand963HataSub] = hataLoss963(p.distanceM, hb, hm, p.freqMHz, hataSuburban963)
	out[cand963DeltaP2108] = raisedDelta + clutter
	out[cand963DeltaP2108C] = clutterDelta + clutterOnly
	out[cand963DSM] = lossDSM
	out[cand963DSMRaw] = lossOver(raw)
	out[cand963MM] = lossOver(forest) + mmClutter + fadeMargin963Db
	return out
}

// reachLosses963 is every candidate's loss along `bearings` radials from
// (e0, n0), every res metres out to maxM: [candidate][bearing][i] is the
// loss at (i+1)·res, NaN without terrain. The transmitter is txH above
// the ground at the centre, the receiver rxH everywhere; the clutter
// classes come from the grids (tg the DTM, sg the DSM).
func reachLosses963(tg, sg *dhm.CoverageGrid, e0, n0, txH, rxH, freqMHz, k, maxM, res float64, bearings int) map[string][][]float64 {
	out := map[string][][]float64{}
	for _, c := range candidates963 {
		out[c] = make([][]float64, bearings)
	}
	ns := int(maxM / res)
	txClass := clutterAt963(tg, sg, e0, n0, res)
	for b := 0; b < bearings; b++ {
		th := 2 * math.Pi * float64(b) / float64(bearings)
		se, sn := math.Sin(th), math.Cos(th)
		dtm, dsm := make([]float32, ns+1), make([]float32, ns+1)
		for i := 0; i <= ns; i++ {
			e, n := e0+float64(i)*res*se, n0+float64(i)*res*sn
			dtm[i], dsm[i] = tg.Sample(e, n), sg.Sample(e, n)
		}
		for _, c := range candidates963 {
			out[c][b] = make([]float64, ns)
		}
		for i := 1; i <= ns; i++ {
			rxClass := clutterAt963(tg, sg, e0+float64(i)*res*se, n0+float64(i)*res*sn, res)
			l := candidateLosses963(path963{dtm: dtm[:i+1], dsm: dsm[:i+1], distanceM: float64(i) * res,
				txAGL: txH, rxAGL: rxH, freqMHz: freqMHz, k: k, endClass: [2]clutterClass963{txClass, rxClass}})
			for _, c := range candidates963 {
				out[c][b][i-1] = l[c]
			}
		}
	}
	return out
}

// reachRadius963 is, per bearing, the farthest distance reached (a margin
// of at least the marginal class, 3 dB, as the #941 report counts) at a
// link budget; 0 when nothing is.
func reachRadius963(losses [][]float64, res, budgetDb float64) []float64 {
	out := make([]float64, len(losses))
	for b, ls := range losses {
		for i, l := range ls {
			if budgetDb-l >= coverageClasses[1].minDb {
				out[b] = float64(i+1) * res
			}
		}
	}
	return out
}
