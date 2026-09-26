package anomaly

import (
	"math"
	"slices"
	"time"
)

// Periodicity detection.
//
// Events of a key are first grouped into pulses: an event within BurstGap of
// the previous event joins the current pulse, whose time is its first event.
// Each new pulse is evaluated immediately, using only pulses at or before it
// (causal). The detector explains the chain of consecutive gaps that ends at
// the newest pulse with one period P: a gap g is explained if
// |g - k*P| <= tol(P) for some k in 1..MaxMissing+1 (k-1 missing pulses).
//
// Candidate periods come from recent gaps divided by k, and the best is
// refined with the phase estimator (chain span / periods spanned). Where a
// candidate misses some of the newest gaps that one common period explains,
// that period is measured too (chainRefined); the best such period replaces
// the estimate only if it would signal (meets the criteria, Chance within
// MaxChance) and strictly outranks it.
// Candidates are ranked by
// (meets MinPulses/MinCoverage/MaxJitterFraction first, then chain length
// desc, coverage desc, median residual asc, period asc). A multiple of the
// true period cannot explain the gaps of the true period, so its chain
// breaks; a sub-multiple explains the same chain only with lower coverage.
// A hypothesis is kept while it explains each new gap; when it no longer
// meets the criteria, the search runs again and a hypothesis that meets
// them wins over a longer one that does not. While it meets them, its
// multiples 2P..(MaxMissing+1)P are checked too, and one that meets the
// criteria on its own with higher coverage replaces it (a sub-multiple
// explains the true train only with lower coverage). So an extra (stray)
// pulse, which breaks the true period's chain and briefly favours a
// sub-multiple, costs at most MinPulses clean pulses before the true period
// is detected again, whatever MinCoverage is. Near-ties are reported as
// alternatives rather than hidden.
//
// The null model is a Poisson process with the key's local pulse rate over
// the pulse ring (causal): chance = evaluated * cells * p_gap^(chainGaps-1),
// where p_gap is the probability that one exponential gap falls within tol
// of some k*P. The exponent drops one gap because P is fitted to the chain
// (the gap it was derived from matches by construction). cells is the
// number of candidate periods the search may report per pulse (seeds,
// refined periods and the phase refinement; see searchCells) and evaluated
// is the number of pulses tested on the key so far: a union bound over the
// period search and over every pulse that was another chance to fire.

// recentGaps is how many of the newest gaps seed the period search.
const recentGaps = 16

type pulseFlags uint8

const (
	pulseAllExpected pulseFlags = 1 << iota
	pulseAnyExpected
	pulseAnyMonitored
)

type periodicState struct {
	times     []int64 // ring of pulse start times
	flags     []pulseFlags
	counts    []uint16 // events merged into each pulse (saturating)
	head, n   int
	lastEvent int64
	started   bool
	evaluated uint64 // pulses evaluated on this key so far (multiple-testing count)
	// current hypothesis for the chain ending at the newest pulse
	period       int64
	sufficientAt int64 // first pulse time at which the current chain met every criterion (0 = not yet)
	alts         []PeriodAlternative
}

func newPeriodicState(historyLen int) *periodicState {
	return &periodicState{times: make([]int64, historyLen), flags: make([]pulseFlags, historyLen), counts: make([]uint16, historyLen)}
}

func (p *periodicState) at(i int) int64 { // i-th oldest pulse
	return p.times[(p.head+i)%len(p.times)]
}

func (p *periodicState) flagAt(i int) pulseFlags { return p.flags[(p.head+i)%len(p.flags)] }

func (p *periodicState) countAt(i int) uint16 { return p.counts[(p.head+i)%len(p.counts)] }

func classFlags(c TrafficClass) pulseFlags {
	switch c {
	case TrafficExpected:
		return pulseAllExpected | pulseAnyExpected
	case TrafficMonitored:
		return pulseAnyMonitored
	}
	return 0
}

// periodicScratch holds reusable buffers owned by the Detector (one
// goroutine), so periodic evaluation neither allocates per event nor keeps
// buffers per key.
type periodicScratch struct {
	resid, med, seen, seenRef []int64
	// work counters (chains measured, gaps visited), for bound tests and
	// benchmarks
	chains, steps uint64
}

// periodicSignal is a hot evaluation result.
type periodicSignal struct {
	ev    PeriodicEvidence
	label TrafficLabel
}

// observe feeds one event. It returns a signal when the chain ending at a
// newly started pulse satisfies every criterion of r.
func (p *periodicState) observe(t int64, cls TrafficClass, r *PeriodicRule, sc *periodicScratch) (periodicSignal, bool) {
	f := classFlags(cls)
	if p.started && t-p.lastEvent <= int64(r.BurstGap) {
		// same burst: merge into the newest pulse without re-evaluating
		i := (p.head + p.n - 1) % len(p.times)
		if p.flags[i]&pulseAllExpected != 0 && f&pulseAllExpected == 0 {
			p.flags[i] &^= pulseAllExpected
		}
		p.flags[i] |= f &^ pulseAllExpected
		if p.counts[i] < math.MaxUint16 {
			p.counts[i]++
		}
		p.lastEvent = t
		return periodicSignal{}, false
	}
	p.started = true
	p.lastEvent = t
	if p.n == len(p.times) {
		p.head = (p.head + 1) % len(p.times)
		p.n--
	}
	i := (p.head + p.n) % len(p.times)
	p.times[i], p.flags[i], p.counts[i] = t, f, 1
	p.n++
	p.evaluated++
	return p.evaluate(t, r, sc)
}

// chainInfo describes the longest suffix chain explained by one period.
type chainInfo struct {
	period   int64
	tol      int64
	gaps     int     // explained consecutive gaps ending at the newest pulse
	slots    int     // sum of k over those gaps
	residual int64   // median |g - k*P|
	coverage float64 // gaps / slots
}

// meets reports whether a chain meets the rule's shape criteria (support,
// coverage, jitter); the null-model test comes on top.
func (c chainInfo) meets(r *PeriodicRule) bool {
	return c.gaps > 0 && c.gaps+1 >= r.MinPulses && c.coverage >= r.MinCoverage &&
		float64(c.residual) <= r.MaxJitterFraction*float64(c.tol)
}

// preferred ranks chains: meeting the criteria first, then better().
func (c chainInfo) preferred(o chainInfo, r *PeriodicRule) bool {
	if cm, om := c.meets(r), o.meets(r); cm != om {
		return cm
	}
	return c.better(o)
}

// outranks is preferred without the final tie-break on the period: an exact
// tie on everything that describes the chain keeps o.
func (c chainInfo) outranks(o chainInfo, r *PeriodicRule) bool {
	if cm, om := c.meets(r), o.meets(r); cm != om {
		return cm
	}
	if c.gaps != o.gaps {
		return c.gaps > o.gaps
	}
	if c.coverage != o.coverage {
		return c.coverage > o.coverage
	}
	return c.residual < o.residual
}

func (c chainInfo) better(o chainInfo) bool {
	if c.gaps != o.gaps {
		return c.gaps > o.gaps
	}
	if c.coverage != o.coverage {
		return c.coverage > o.coverage
	}
	if c.residual != o.residual {
		return c.residual < o.residual
	}
	return c.period < o.period
}

func tolNanos(r *PeriodicRule, period int64) int64 {
	rel := int64(r.JitterRel * float64(period))
	if int64(r.JitterAbs) > rel {
		return int64(r.JitterAbs)
	}
	return rel
}

// explain returns k (1..K) if gap g is explained by period P with tolerance
// tol, else 0.
func explain(g, period, tol int64, maxK int) (int, int64) {
	if period <= 0 {
		return 0, 0
	}
	k := (g + period/2) / period
	if k < 1 {
		k = 1
	}
	if k > int64(maxK) {
		return 0, 0
	}
	d := g - k*period
	if d < 0 {
		d = -d
	}
	if d > tol {
		return 0, 0
	}
	return int(k), d
}

// chainFor measures the suffix chain for one period.
func (p *periodicState) chainFor(period int64, r *PeriodicRule, sc *periodicScratch) chainInfo {
	c := chainInfo{period: period, tol: tolNanos(r, period)}
	scratch := sc.resid[:0]
	maxK := r.MaxMissing + 1
	sc.chains++
	for i := p.n - 1; i >= 1; i-- {
		sc.steps++
		g := p.at(i) - p.at(i-1)
		k, d := explain(g, period, c.tol, maxK)
		if k == 0 {
			break
		}
		c.gaps++
		c.slots += k
		scratch = append(scratch, d)
	}
	sc.resid = scratch
	if c.gaps > 0 {
		c.coverage = float64(c.gaps) / float64(c.slots)
		c.residual = medianInt64(scratch, sc)
	}
	return c
}

// medianInt64 returns the median of v without modifying it.
func medianInt64(v []int64, sc *periodicScratch) int64 {
	s := append(sc.med[:0], v...)
	slices.Sort(s)
	sc.med = s
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func (p *periodicState) evaluate(now int64, r *PeriodicRule, sc *periodicScratch) (periodicSignal, bool) {
	if p.n < r.MinPulses {
		p.period, p.sufficientAt, p.alts = 0, 0, nil
		return periodicSignal{}, false
	}
	var best chainInfo
	// Keep the current hypothesis if it still explains the newest gap; search
	// again when there is none or it no longer meets the criteria. A new
	// chain restarts SufficientAt and the alternatives.
	kept := false
	if p.period > 0 {
		newest := p.at(p.n-1) - p.at(p.n-2)
		if k, _ := explain(newest, p.period, tolNanos(r, p.period), r.MaxMissing+1); k > 0 {
			best, kept = p.chainFor(p.period, r, sc), true
		}
	}
	// A sub-multiple that absorbed a stray pulse keeps explaining the true
	// train with lower coverage. As soon as a multiple of the kept period
	// meets the criteria on its own with higher coverage, it is the
	// fundamental.
	if kept && best.meets(r) {
		base, fund := best.period, best
		for m := int64(2); m <= int64(r.MaxMissing+1); m++ {
			if mp := base * m; mp <= int64(r.MaxPeriod) {
				if c := p.chainFor(mp, r, sc); c.meets(r) && c.coverage > fund.coverage {
					fund = c
				}
			}
		}
		if fund.period != base {
			best = fund
			p.sufficientAt, p.alts = 0, nil
		}
	}
	if !best.meets(r) {
		if est := p.estimate(r, sc); !kept || (est.gaps > 0 && est.preferred(best, r)) {
			best = est
			p.sufficientAt, p.alts = 0, nil
		}
	}
	if best.gaps == 0 {
		p.period = 0
		return periodicSignal{}, false
	}
	p.period = best.period
	support := best.gaps + 1
	if !best.meets(r) {
		p.sufficientAt, p.alts = 0, nil
		return periodicSignal{}, false
	}
	chance := p.chance(best, r)
	if chance > r.MaxChance {
		p.sufficientAt, p.alts = 0, nil
		return periodicSignal{}, false
	}
	if p.sufficientAt == 0 {
		p.sufficientAt = now
		p.alts = p.alternatives(best, r, sc)
	}
	// traffic label and burst sizes over the chain's pulses
	var total, exp, mon uint64
	events := 0
	for i := p.n - 1 - best.gaps; i < p.n; i++ {
		fl := p.flagAt(i)
		total++
		if fl&pulseAllExpected != 0 {
			exp++
		}
		if fl&pulseAnyMonitored != 0 {
			mon++
		}
		events += int(p.countAt(i))
	}
	ev := PeriodicEvidence{
		Period:       time.Duration(best.period),
		Tolerance:    time.Duration(best.tol),
		JitterMedian: time.Duration(best.residual),
		Support:      support,
		Events:       events,
		Coverage:     best.coverage,
		Chance:       chance,
		SufficientAt: time.Unix(0, p.sufficientAt).UTC(),
		Alternatives: append([]PeriodAlternative(nil), p.alts...),
	}
	return periodicSignal{ev: ev, label: labelFromCounts(total, exp, mon)}, true
}

// estimate searches candidate periods derived from recent gaps.
func (p *periodicState) estimate(r *PeriodicRule, sc *periodicScratch) chainInfo {
	var best chainInfo
	minP, maxP := int64(r.MinPeriod), int64(r.MaxPeriod)
	maxK := r.MaxMissing + 1
	newest := p.at(p.n-1) - p.at(p.n-2)
	// Quick reject: a newest gap no period in range can explain.
	if newest+tolNanos(r, maxP) < minP-tolNanos(r, minP) || newest > int64(maxK)*maxP+tolNanos(r, maxP) {
		return best
	}
	// The seeds and the phase refinement below give exactly the estimate
	// they give without refined candidates; the best refined candidate
	// (ranked on its own) replaces that estimate only if it would signal and
	// strictly outranks it. So the search never returns a chain the seeds
	// alone would rank higher, and until a refined period would signal it
	// changes nothing.
	var bestRef chainInfo
	seen, seenRef := sc.seen[:0], sc.seenRef[:0]
	for i := p.n - 1; i >= 1 && i >= p.n-recentGaps; i-- {
		g := p.at(i) - p.at(i-1)
		for k := 1; k <= maxK; k++ {
			cand := g / int64(k)
			if cand < minP || cand > maxP {
				continue
			}
			if slices.Contains(seen, cand) {
				continue
			}
			seen = append(seen, cand)
			c, ref := p.chainRefined(cand, r, sc)
			if c.gaps > 0 && (best.gaps == 0 || c.preferred(best, r)) {
				best = c
			}
			if ref != cand && !slices.Contains(seenRef, ref) && !slices.Contains(seen, ref) {
				seenRef = append(seenRef, ref)
				if c := p.chainFor(ref, r, sc); c.gaps > 0 && (bestRef.gaps == 0 || c.preferred(bestRef, r)) {
					bestRef = c
				}
			}
		}
	}
	sc.seen, sc.seenRef = seen, seenRef
	if best.gaps > 0 {
		// Refine with the phase estimator span/slots over the chain. It is
		// robust to per-pulse jitter, whereas ranking by gap residual favours
		// a biased gap. It replaces the estimate whenever it explains a chain
		// at least as long with at least the same coverage.
		span := p.at(p.n-1) - p.at(p.n-1-best.gaps)
		if ref := span / int64(best.slots); ref >= minP && ref <= maxP && ref != best.period {
			if c := p.chainFor(ref, r, sc); c.gaps >= best.gaps && c.coverage >= best.coverage && (c.meets(r) || !best.meets(r)) {
				best = c
			}
		}
	}
	// only a refined candidate that would signal (meets the criteria and its
	// Chance is within MaxChance): until one does, the hypothesis kept from
	// pulse to pulse stays the one the seeds give, and a kept hypothesis
	// that meets the criteria stops the search, so it must not be one that
	// cannot signal yet. Only the best-ranked refined candidate is tried; a
	// lower-ranked one that would pass MaxChance is deliberately not taken
	// instead, so the estimate is always the best-ranked chain and never the
	// one picked because it is the most likely to fire.
	if bestRef.meets(r) && p.chance(bestRef, r) <= r.MaxChance && (best.gaps == 0 || bestRef.outranks(best, r)) {
		best = bestRef
	}
	return best
}

// chainRefined measures seed's chain exactly as chainFor does and returns it
// with the period this search cell tests: seed, or a refinement of it.
//
// A single gap fixes P only to within tol/k. When the jitter of neighbouring
// gaps cancels (gaps of 294s, 306s, 294s... around 300s with tol 6s) no seed
// taken from one gap explains the next one, so no chain forms. Every period
// in the intersection of the ranges [(g-tol)/k, (g+tol)/k] of the newest gaps
// explains all of them. In the same pass as the chain, over the recentGaps
// newest gaps (the window the seeds come from) and with the multiples k the
// seed assigns them, chainRefined intersects those ranges. If the common
// range covers at least two gaps and more than the seed's chain, it also
// returns the phase estimate span/slots clamped into it, which estimate()
// measures as a refined candidate.
//
// A refined period is fitted to up to recentGaps gaps, not one, so a chance
// chain fits it up to that many times more often than one seed; it is a
// hypothesis of its own, and searchCells charges a cell for it (see there
// for why one gap per cell is still what the fit costs).
//
// Cost: the chain pass as before, the range adds O(1) per gap in the window;
// a refined cell adds one walk of recentGaps gaps and one chainFor. No
// allocation; see BenchmarkPeriodicSearch and TestPeriodicSearchWorkIsBounded.
func (p *periodicState) chainRefined(seed int64, r *PeriodicRule, sc *periodicScratch) (chainInfo, int64) {
	c := chainInfo{period: seed, tol: tolNanos(r, seed)}
	resid := sc.resid[:0]
	maxK := r.MaxMissing + 1
	lo, hi := int64(1), int64(math.MaxInt64)
	var n int
	var span, slots int64
	inChain, inRange := true, true
	sc.chains++
	for i := p.n - 1; i >= 1; i-- {
		if inRange = inRange && i >= p.n-recentGaps; !inChain && !inRange {
			break
		}
		sc.steps++
		g := p.at(i) - p.at(i-1)
		if inChain {
			if k, d := explain(g, seed, c.tol, maxK); k > 0 {
				c.gaps++
				c.slots += k
				resid = append(resid, d)
			} else {
				inChain = false
			}
		}
		if inRange {
			k := max((g+seed/2)/seed, 1)
			l, h := lo, min(hi, (g+c.tol)/k)
			if g-c.tol > 0 {
				l = max(l, (g-c.tol+k-1)/k)
			}
			if k > int64(maxK) || l > h {
				inRange = false
			} else {
				lo, hi = l, h
				n++
				span += g
				slots += k
			}
		}
	}
	sc.resid = resid
	if c.gaps > 0 {
		c.coverage = float64(c.gaps) / float64(c.slots)
		c.residual = medianInt64(resid, sc)
	}
	// With one gap the phase estimate is newest/k, a seed the search tries
	// anyway.
	if n < 2 || n <= c.gaps {
		return c, seed
	}
	ref := min(max(span/slots, lo), hi)
	// The range uses the seed's tolerance, but the tolerance grows with the
	// period (JitterRel): keep ref only if, with its own tolerance, it
	// explains more of the newest gaps than the seed does.
	if ref < int64(r.MinPeriod) || ref > int64(r.MaxPeriod) || p.recentExplained(ref, r, sc) <= c.gaps {
		return c, seed
	}
	return c, ref
}

// recentExplained counts the consecutive newest gaps, at most recentGaps,
// that period explains with its own tolerance.
func (p *periodicState) recentExplained(period int64, r *PeriodicRule, sc *periodicScratch) int {
	tol, maxK, n := tolNanos(r, period), r.MaxMissing+1, 0
	for i := p.n - 1; i >= 1 && i >= p.n-recentGaps; i-- {
		sc.steps++
		if k, _ := explain(p.at(i)-p.at(i-1), period, tol, maxK); k == 0 {
			break
		}
		n++
	}
	return n
}

// alternatives reports multiples and sub-multiples of the chosen period
// whose own chain is at least half as long (ambiguous explanations).
func (p *periodicState) alternatives(best chainInfo, r *PeriodicRule, sc *periodicScratch) []PeriodAlternative {
	var out []PeriodAlternative
	for _, f := range []struct{ num, den int64 }{{2, 1}, {3, 1}, {1, 2}, {1, 3}} {
		cand := best.period * f.num / f.den
		if cand < int64(r.MinPeriod) || cand > int64(r.MaxPeriod) {
			continue
		}
		if c := p.chainFor(cand, r, sc); c.gaps*2 >= best.gaps && c.gaps > 0 {
			out = append(out, PeriodAlternative{Period: time.Duration(cand), Support: c.gaps + 1, Coverage: c.coverage})
		}
	}
	return out
}

func (p *periodicState) chance(c chainInfo, r *PeriodicRule) float64 {
	span := p.at(p.n-1) - p.at(0)
	if span <= 0 {
		return 1
	}
	lambda := float64(p.n-1) / float64(span)
	var pg float64
	for k := 1; k <= r.MaxMissing+1; k++ {
		lo := float64(int64(k)*c.period - c.tol)
		if lo < 0 {
			lo = 0
		}
		hi := float64(int64(k)*c.period + c.tol)
		// P(lo <= gap <= hi) = exp(-l*lo) - exp(-l*hi), written without the
		// subtraction of two nearly equal numbers: for small tolerances
		// that subtraction loses digits, which Pow then amplifies (and the
		// lost digits differ between CPU architectures).
		pg += math.Exp(-lambda*lo) * -math.Expm1(-lambda*(hi-lo))
	}
	if pg > 1 {
		pg = 1
	}
	// Union bound: every pulse evaluated so far and every candidate period the
	// search may try is another chance; one gap is spent on fitting P.
	return float64(p.evaluated) * float64(searchCells(r)) * math.Pow(pg, float64(c.gaps-1))
}

// searchCells is the number of candidate periods the search may report per
// pulse, each one cell of the union bound: a seed g/k for each of the
// recentGaps newest gaps and k = 1..MaxMissing+1, at most one refined period
// per seed, and the phase refinement of the best seed. (A kept hypothesis
// reports only when the search does not run, with at most MaxMissing+1
// periods: itself and its multiples.)
//
// Each cell spends one gap on its fit: chance() uses pg^(gaps-1). For a seed
// that is the gap it is taken from. A refined period lies in the common
// range of the m newest gaps it explains, the intersection of their ranges
// [(g-tol)/k, (g+tol)/k]. That intersection is non-empty exactly when its
// highest lower end, L = (g_j-tol)/k_j of one gap j, lies in every other
// range, that is when the period L explains each of the other m-1 gaps. So
// "some period explains these m gaps" is the union, over the
// recentGaps*(MaxMissing+1) pairs (j, k_j), of "(g_j-tol)/k_j explains the
// other m-1 gaps", each with one gap spent: as many cells as there are
// seeds. For one set of multiples, a common period exists up to m times as
// often as one given seed explains the other gaps, so the refined periods
// cannot share the seeds' cells. Gaps older than the window are tested
// against a period the window fixes, one independent chance pg each. As
// everywhere in this bound, pg is taken at the reported period.
//
// All of this assumes the Poisson null. For traffic more regular than
// Poisson (gaps spread evenly around their mean) Chance is not a bound, and
// the fitted refined periods find more of its chance chains than the seeds
// alone.
func searchCells(r *PeriodicRule) int { return 2*recentGaps*(r.MaxMissing+1) + 1 }
