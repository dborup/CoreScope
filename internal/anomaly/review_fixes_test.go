package anomaly

import (
	"encoding/json"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Boundaries and paths around the six independent-review findings that the
// reproductions in independent_review_test.go do not cover on their own.

// ---- periodic candidate search (finding 1) ----

// The same alternating pattern with a purely relative tolerance, the jitter
// exactly at tol(P): P=320s, JitterRel=1/32 (exact in binary), so tol(P) is
// exactly 10s and the gaps are 310s and 330s. Seeds below P have a smaller
// tolerance than P itself, so the refined period must be checked with its
// own tolerance, not the seed's or the range's lowest.
func TestPeriodicAlternatingJitterAtARelativeTolerance(t *testing.T) {
	rule := experimentalPeriodic()
	rule.JitterAbs, rule.JitterRel, rule.MaxJitterFraction = 0, 1.0/32, 1
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{rule}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	const period = 320 * time.Second
	p := newPeriodicState(rule.HistoryLen)
	var sc periodicScratch
	var events []Event
	for i := 0; i < 32; i++ {
		when := time.Duration(i) * period
		if i%2 == 1 {
			when -= 10 * time.Second
		}
		events = append(events, relayEvent(t, "pulse-"+string(rune('A'+i)), at(when), 1, 2))
		p.observe(at(when).UnixNano(), TrafficUnclassified, &rule, &sc)
	}
	ideal := p.chainFor(int64(period), &rule, &sc)
	if ideal.tol != int64(10*time.Second) || !ideal.meets(&rule) || ideal.gaps != 31 || p.chance(ideal, &rule) > rule.MaxChance {
		t.Fatalf("invalid repro: ideal=%+v chance=%g", ideal, p.chance(ideal, &rule))
	}
	cs := run(t, newDet(t, cfg), events)
	a := filter(cs, rule.Name, StateActive)
	if len(a) == 0 {
		t.Fatalf("not detected: period=320s, gaps=310s/330s, relative tolerance 10s")
	}
	if a[0].Periodic.Period != period {
		t.Fatalf("detected period %v, want %v", a[0].Periodic.Period, period)
	}
}

// A refined cell must not hide its seed. Pulse 1 is a stray start 305s
// before a steady 294s train, so the seed 294s explains a clean chain that
// meets every criterion once the train has MinPulses pulses (pulse
// 1+MinPulses, as on ad3ab30f), while the refinement, pulled by the 305s
// gap, explains a longer chain that fails MaxJitterFraction. Both are
// candidates, and the one that meets wins.
func TestRefinementDoesNotHideAMeetingSeedChain(t *testing.T) {
	rule := experimentalPeriodic()
	p := newPeriodicState(rule.HistoryLen)
	var sc periodicScratch
	when := 305 * time.Second
	first := 0
	for i := 1; i <= 24 && first == 0; i++ {
		ts := time.Duration(0)
		if i > 1 {
			ts = when + time.Duration(i-2)*294*time.Second
		}
		if _, ok := p.observe(at(ts).UnixNano(), TrafficUnclassified, &rule, &sc); ok {
			first = i
		}
	}
	if want := 1 + rule.MinPulses; first != want {
		t.Fatalf("first signal at pulse %d, want %d (the first sufficient pulse)", first, want)
	}
}

// A refined candidate replaces the seeds' estimate only once it would
// signal. With a tight MaxChance a refined chain can meet the criteria and
// outrank the seeds while its Chance is still too high; kept as the
// hypothesis, it would stop the search while a seed chain that passes
// MaxChance exists. The gaps are two trains from an independent Monte Carlo
// (rule: JitterAbs 1s, JitterRel 0.03, MaxChance 1e-9); without the Chance
// condition the first signalled 27 pulses late and the second never.
// ad3ab30f's signals are the reference.
func TestRefinedCandidateReplacesOnlyWhenItWouldSignal(t *testing.T) {
	for _, c := range []struct {
		name               string
		gaps               []int64 // ns
		first, count, last int
	}{
		{"train 740 (period 4m16s)", []int64{
			253473301057, 258989191336, 248808871358, 260765027015, 259220274542, 260339684107,
			262600378762, 258738291871, 248944813011, 258606895992, 256467098643, 241784077500,
			257070483133, 258859646680, 250520765238, 257227581530, 249266780471, 252126136834,
			250445492269, 263258476954, 249429701539, 255178245932, 260308587824, 262717898963,
			253196559566, 248241298572, 261418029655, 248770098153, 255363287212, 258491054663,
			258376829668, 249750285464, 260387262917, 257399278175, 263913884834, 254618106104,
			263253087435, 261094534382, 261639049616, 251513354176, 258897024009, 262539896680,
			258308071838, 261975360249, 262945608084, 257866559143, 253898288909, 252200311541,
			252505890454, 248219752799, 260187439605, 256025054915, 255771405888, 263583781023,
			258520832693, 250677715026, 254379206898, 250134628599, 248673262343, 253559894187,
			260685391656, 244190473175, 259409506745, 263956802350, 249234009113, 262085624807,
			252595914023, 268807348304, 263888676328, 257006252683,
		}, 10, 14, 59},
		{"train 1240 (period 2m53s)", []int64{
			174171937807, 173259145459, 174531540411, 340444992491, 168600002187, 172130714765,
			178185967610, 167672548385, 175582782198, 178930825973, 170987808172, 178812895590,
			169727494636, 175341588223, 170041727849, 173205068154, 170345765780, 175075327296,
			175235169903, 169332171444, 188333639312, 167408995031, 168616014872, 176732465155,
			178045458259, 174315631381, 178741743767, 174044112872, 167394578413, 175381847267,
			172583705123, 168739362204, 174321044684, 168217101116, 167035424717, 170618088704,
			165346946332, 176915627713, 176029405703, 173136516273, 168960152899, 175535230297,
			169200234897, 177781560705, 177969402460, 172228240331, 169581467830, 172286294966,
			168458815458, 178829076893, 168522218864, 173637838896, 175170616840, 171330601291,
			174342474654, 169534334907, 175681725697, 173915952191, 187517102381, 174359296774,
			169014698377, 174123854812, 175505387461, 175828494557, 173758510591, 168915340027,
			171752353392, 167303471723, 175039029128, 174776307324,
		}, 57, 1, 57},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := experimentalPeriodic()
			r.JitterAbs, r.JitterRel, r.MaxChance = time.Second, 0.03, 1e-9
			p := newPeriodicState(r.HistoryLen)
			var sc periodicScratch
			first, count, last := -1, 0, -1
			var cur int64
			for i, g := range c.gaps {
				cur += g
				if _, ok := p.observe(t0.UnixNano()+cur, TrafficUnclassified, &r, &sc); ok {
					if first < 0 {
						first = i
					}
					count, last = count+1, i
				}
			}
			if first != c.first || count != c.count || last != c.last {
				t.Fatalf("signals first=%d count=%d last=%d, want %d %d %d (as on ad3ab30f)", first, count, last, c.first, c.count, c.last)
			}
		})
	}
}

// ringOf returns a periodic state holding pulses at the given offsets from t0.
func ringOf(r *PeriodicRule, offsets ...time.Duration) *periodicState {
	p := newPeriodicState(r.HistoryLen)
	for i, o := range offsets {
		p.times[i] = at(o).UnixNano()
	}
	p.n = len(offsets)
	return p
}

// The refined period is checked with its own tolerance. Here the common
// range, built with the seed's larger relative tolerance, reaches 319.69s,
// but a period there misses even the newest gap (330s) with its own 9.99s
// tolerance, so the cell keeps the seed.
func TestRefinedPeriodNeverExplainsFewerGapsThanTheSeed(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel = 0, 1.0/32
	p := ringOf(&r, 0, 400*time.Second, 709700*time.Millisecond, 1039700*time.Millisecond)
	var sc periodicScratch
	seed := int64(330 * time.Second)
	c, ref := p.chainRefined(seed, &r, &sc)
	if c.gaps != 1 {
		t.Fatalf("fixture: seed explains %d gaps, want 1", c.gaps)
	}
	if got := p.recentExplained(ref, &r, &sc); got < c.gaps {
		t.Fatalf("cell tests %v, which explains %d newest gaps; the seed explains %d", time.Duration(ref), got, c.gaps)
	}
}

// The common range is exact to the nanosecond: its bounds are the ceiling
// of (g-tol)/k and the floor of (g+tol)/k, and the phase estimate is clamped
// into it. With k=2, tol=6s and X=300s+1ns, the gaps allow only P=X, and
// span/slots falls below X (first case) or above it (second case). The cell
// must test exactly X. The oldest gap, 1000s (k=3), ends the range.
func TestRefinementRangeIsExactAndClamped(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel = 6*time.Second, 0
	const x = 300*time.Second + 1
	for _, c := range []struct {
		name    string
		gaps    []time.Duration // oldest first
		seedGap time.Duration   // the seed is this gap / 2
		own     int
	}{
		// 606s+1ns sets the lower bound X (ceil), 594s+2ns the upper bound X;
		// span/slots = 300s
		{"estimate below the range", []time.Duration{1000 * time.Second, 594*time.Second + 2, 606*time.Second + 1}, 594*time.Second + 2, 0},
		// 594s+3ns sets the upper bound X (floor), 606s+1ns and 606s+2ns keep
		// the lower bound X; span/slots = 301s+1ns
		{"estimate above the range", []time.Duration{1000 * time.Second, 606*time.Second + 2, 606*time.Second + 1, 594*time.Second + 3}, 594*time.Second + 3, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			offs := []time.Duration{0}
			for _, g := range c.gaps {
				offs = append(offs, offs[len(offs)-1]+g)
			}
			p := ringOf(&r, offs...)
			var sc periodicScratch
			chain, ref := p.chainRefined(int64(c.seedGap)/2, &r, &sc)
			if chain.gaps != c.own {
				t.Fatalf("fixture: the seed explains %d gaps, want %d", chain.gaps, c.own)
			}
			if ref != int64(x) {
				t.Fatalf("refined period %v, want %v", time.Duration(ref), x)
			}
			if got := p.chainFor(ref, &r, &sc); got.gaps != len(c.gaps)-1 {
				t.Fatalf("refined period explains %d gaps, want %d", got.gaps, len(c.gaps)-1)
			}
		})
	}
}

// The refinement costs only what its rules say: one pass over the gaps the
// seed's chain or the recentGaps window needs, plus one verification walk
// when (and only when) a refinement is proposed.
func TestRefinementWorkIsExact(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel = 6*time.Second, 0
	alternating := func(n int) *periodicState {
		var offs []time.Duration
		for i := 0; i < n; i++ {
			o := time.Duration(i) * 5 * time.Minute
			if i%2 == 1 {
				o -= 6 * time.Second
			}
			offs = append(offs, o)
		}
		return ringOf(&r, offs...)
	}
	periodic := func(n int) *periodicState {
		var offs []time.Duration
		for i := 0; i < n; i++ {
			offs = append(offs, time.Duration(i)*5*time.Minute)
		}
		return ringOf(&r, offs...)
	}
	for _, c := range []struct {
		name      string
		p         *periodicState
		seed      int64
		steps     uint64
		refined   bool
		wantGaps  int
		wantRange string
	}{
		// the seed explains all 16 gaps: no verification walk
		{"seed explains every walked gap", periodic(17), int64(5 * time.Minute), 16, false, 16, ""},
		// the range stops after one gap: no refinement, no verification walk
		{"range of one gap", ringOf(&r, 0, 1000*time.Second, 1300*time.Second), int64(1000 * time.Second), 2, false, 0, ""},
		// 31 gaps: the range walk stops at the 16-gap window, then one
		// verification walk of 16 gaps
		{"window of recentGaps", alternating(32), int64(294 * time.Second), recentGaps + recentGaps, true, 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var sc periodicScratch
			got, ref := c.p.chainRefined(c.seed, &r, &sc)
			if sc.steps != c.steps || (ref != c.seed) != c.refined || got.gaps != c.wantGaps {
				t.Fatalf("steps %d refined %v gaps %d, want %d %v %d", sc.steps, ref != c.seed, got.gaps, c.steps, c.refined, c.wantGaps)
			}
		})
	}
}

// Each distinct period is measured once per search: many seeds of the
// alternating train refine to the same 300s.
func TestRefinedPeriodsAreMeasuredOnce(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel, r.MaxJitterFraction = 6*time.Second, 0, 1
	var offs []time.Duration
	for i := 0; i < 32; i++ {
		o := time.Duration(i) * 5 * time.Minute
		if i%2 == 1 {
			o -= 6 * time.Second
		}
		offs = append(offs, o)
	}
	p := ringOf(&r, offs...)
	var sc periodicScratch
	best := p.estimate(&r, &sc)
	if best.period != int64(5*time.Minute) {
		t.Fatalf("fixture: estimate %v, want 5m", time.Duration(best.period))
	}
	distinct := map[int64]bool{}
	for _, v := range append(append([]int64(nil), sc.seen...), sc.seenRef...) {
		distinct[v] = true
	}
	// one measurement per distinct period, plus at most the final refinement
	if n := len(sc.seen) + len(sc.seenRef); n != len(distinct) || sc.chains > uint64(n)+1 {
		t.Fatalf("%d chains measured, %d periods recorded, %d distinct", sc.chains, n, len(distinct))
	}
}

// estimateSeedsOnly is estimate as of ad3ab30f, before refined candidates:
// the reference the search must never fall below.
func estimateSeedsOnly(p *periodicState, r *PeriodicRule, sc *periodicScratch) chainInfo {
	var best chainInfo
	minP, maxP := int64(r.MinPeriod), int64(r.MaxPeriod)
	maxK := r.MaxMissing + 1
	newest := p.at(p.n-1) - p.at(p.n-2)
	if newest+tolNanos(r, maxP) < minP-tolNanos(r, minP) || newest > int64(maxK)*maxP+tolNanos(r, maxP) {
		return best
	}
	var seen []int64
	for i := p.n - 1; i >= 1 && i >= p.n-recentGaps; i-- {
		g := p.at(i) - p.at(i-1)
		for k := 1; k <= maxK; k++ {
			cand := g / int64(k)
			if cand < minP || cand > maxP || slices.Contains(seen, cand) {
				continue
			}
			seen = append(seen, cand)
			if c := p.chainFor(cand, r, sc); c.gaps > 0 && (best.gaps == 0 || c.preferred(best, r)) {
				best = c
			}
		}
	}
	if best.gaps == 0 {
		return best
	}
	span := p.at(p.n-1) - p.at(p.n-1-best.gaps)
	if ref := span / int64(best.slots); ref >= minP && ref <= maxP && ref != best.period {
		if c := p.chainFor(ref, r, sc); c.gaps >= best.gaps && c.coverage >= best.coverage && (c.meets(r) || !best.meets(r)) {
			best = c
		}
	}
	return best
}

// Over random rings, rule shapes and both tolerance kinds, the search
// returns exactly what the seeds alone return, or a refined chain that
// would signal (meets the criteria, Chance within MaxChance) and strictly
// outranks it.
func TestEstimateNeverRanksBelowTheSeedsAlone(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	var sc periodicScratch
	improved := 0
	for trial := 0; trial < 3000; trial++ {
		r := experimentalPeriodic()
		r.MaxMissing = rng.Intn(3)
		r.MaxJitterFraction = []float64{0.25, 0.5, 1}[rng.Intn(3)]
		r.MinCoverage = []float64{0.6, 0.8, 1}[rng.Intn(3)]
		if trial%2 == 0 {
			r.JitterAbs, r.JitterRel = 6*time.Second, 0
		} else {
			r.JitterAbs, r.JitterRel = 0, 1.0/32
		}
		base := time.Duration(60+rng.Intn(900)) * time.Second
		jitter := time.Duration(1+rng.Intn(8)) * time.Second
		offs := []time.Duration{0}
		for i := 1; i < r.HistoryLen; i++ {
			k := time.Duration(1)
			if rng.Intn(10) == 0 {
				k = 2
			}
			d := k*base + time.Duration(rng.Int63n(int64(2*jitter))) - jitter
			if rng.Intn(15) == 0 {
				d = time.Duration(rng.Int63n(int64(base))) + 10*time.Second
			}
			offs = append(offs, offs[i-1]+d)
		}
		p := ringOf(&r, offs...)
		want := estimateSeedsOnly(p, &r, &sc)
		got := p.estimate(&r, &sc)
		if got != want {
			if !got.meets(&r) || p.chance(got, &r) > r.MaxChance || (want.gaps > 0 && !got.outranks(want, &r)) {
				t.Fatalf("trial %d: estimate %+v replaced the seeds' %+v without being able to signal and outranking it", trial, got, want)
			}
			improved++
		}
	}
	if improved == 0 {
		t.Fatal("fixture: refined candidates never changed the estimate")
	}
	t.Logf("%d of 3000 estimates improved by a refined candidate", improved)
}

// The seeds are ranked exactly as without refined candidates, even when a
// seed equals a period an earlier refinement already measured: here the
// newest seed (306s) refines to 300s, and the oldest gap in the window is
// exactly 300s. Every distinct in-range gap/k of the window is a seed.
func TestSeedsAreRankedAsWithoutRefinement(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel, r.MaxJitterFraction = 6*time.Second, 0, 1
	offs := []time.Duration{0, 300 * time.Second}
	for j := 2; j <= recentGaps; j++ {
		g := 294 * time.Second
		if j%2 == 0 {
			g = 306 * time.Second
		}
		offs = append(offs, offs[len(offs)-1]+g)
	}
	p := ringOf(&r, offs...)
	var sc periodicScratch
	p.estimate(&r, &sc)
	want := map[int64]bool{}
	for i := p.n - 1; i >= 1 && i >= p.n-recentGaps; i-- {
		g := p.at(i) - p.at(i-1)
		for k := 1; k <= r.MaxMissing+1; k++ {
			if s := g / int64(k); s >= int64(r.MinPeriod) && s <= int64(r.MaxPeriod) {
				want[s] = true
			}
		}
	}
	if !slices.Contains(sc.seenRef, int64(300*time.Second)) {
		t.Fatalf("fixture: 300s was not measured as a refined period (refined %v)", sc.seenRef)
	}
	got := map[int64]bool{}
	for _, s := range sc.seen {
		got[s] = true
	}
	if len(sc.seen) != len(want) || !reflect.DeepEqual(got, want) {
		t.Fatalf("seed pool %d values %v, want the %d distinct seeds %v", len(sc.seen), sc.seen, len(want), want)
	}
}

// Over random rings and both tolerance kinds: the fused pass measures the
// seed's chain exactly as chainFor does, and the period a cell tests never
// explains fewer of the newest gaps than its seed.
func TestChainRefinedMatchesChainForAndNeverLosesGaps(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	abs := experimentalPeriodic()
	abs.JitterAbs, abs.JitterRel, abs.MaxMissing = 6*time.Second, 0, 2
	rel := experimentalPeriodic()
	rel.JitterAbs, rel.JitterRel, rel.MaxMissing = 0, 1.0/32, 2
	var sc periodicScratch
	refined := 0
	for trial := 0; trial < 2000; trial++ {
		r := abs
		if trial%2 == 1 {
			r = rel
		}
		base := time.Duration(60+rng.Intn(600)) * time.Second
		offs := []time.Duration{0}
		for i := 1; i < r.HistoryLen; i++ {
			k := time.Duration(1 + rng.Intn(2))
			jit := time.Duration(rng.Int63n(int64(12*time.Second))) - 6*time.Second
			if rng.Intn(8) == 0 {
				jit = time.Duration(rng.Int63n(int64(base)))
			}
			offs = append(offs, offs[i-1]+k*base+jit)
		}
		p := ringOf(&r, offs...)
		for i := p.n - 1; i >= p.n-recentGaps; i-- {
			g := p.at(i) - p.at(i-1)
			for k := 1; k <= r.MaxMissing+1; k++ {
				seed := g / int64(k)
				if seed < int64(r.MinPeriod) || seed > int64(r.MaxPeriod) {
					continue
				}
				c, ref := p.chainRefined(seed, &r, &sc)
				if want := p.chainFor(seed, &r, &sc); c != want {
					t.Fatalf("trial %d seed %v: fused chain %+v, chainFor %+v", trial, time.Duration(seed), c, want)
				}
				if ref != seed {
					refined++
					if got, own := p.recentExplained(ref, &r, &sc), p.recentExplained(seed, &r, &sc); got <= own {
						t.Fatalf("trial %d seed %v: refined %v explains %d newest gaps, seed %d", trial, time.Duration(seed), time.Duration(ref), got, own)
					}
				}
			}
		}
	}
	if refined == 0 {
		t.Fatal("fixture: no cell was refined")
	}
	t.Logf("%d refined cells", refined)
}

// ---- union bound over the refined candidates (cloud review of 5b816f66) ----

// scenarioA is the loose rule of the cloud review's scenario a): a wide
// absolute tolerance, no missing pulses and weak shape criteria, so under a
// null only the Chance condition stops chance chains.
func scenarioA(maxChance float64) PeriodicRule {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel, r.MaxJitterFraction, r.MinPulses, r.MinCoverage, r.MaxMissing = 20*time.Second, 0, 1, 4, 0.1, 0
	r.MaxChance = maxChance
	return r
}

// poissonTrain returns 64 pulse offsets of a Poisson train (mean gap 2m to
// 22m, pulses at least 10s apart), generated in full from its seed.
func poissonTrain(seed int64) []time.Duration {
	rng := rand.New(rand.NewSource(seed))
	mean := time.Duration(120+rng.Intn(1200)) * time.Second
	out := make([]time.Duration, 0, 64)
	ts := time.Duration(0)
	for len(out) < 64 {
		ts += 10*time.Second + time.Duration(rng.ExpFloat64()*float64(mean))
		out = append(out, ts)
	}
	return out
}

// Chance is a union bound over every period the search can report, so the
// cells it charges must cover every period estimate measures: up to
// recentGaps*(MaxMissing+1) seeds, as many refined periods and the phase
// refinement. Some of these trains need more than the seeds and the phase
// refinement alone.
func TestSearchCellsCoverEveryPeriodTheSearchMeasures(t *testing.T) {
	b := experimentalPeriodic()
	b.JitterRel, b.MaxJitterFraction, b.MinPulses, b.MinCoverage = 0.1, 1, 4, 0.1
	beyondSeeds := false
	for _, r := range []PeriodicRule{experimentalPeriodic(), scenarioA(0.1), b} {
		for _, shape := range benchShapes {
			p := newPeriodicState(r.HistoryLen)
			var sc periodicScratch
			for i, ts := range pulseTimes(shape, 4*r.HistoryLen) {
				p.observe(ts, TrafficUnclassified, &r, &sc)
				if p.n < 2 {
					continue
				}
				var own periodicScratch
				p.estimate(&r, &own)
				if own.chains > uint64(searchCells(&r)) {
					t.Fatalf("%s MaxMissing=%d pulse %d: estimate measured %d periods, Chance charges %d cells",
						shape, r.MaxMissing, i, own.chains, searchCells(&r))
				}
				beyondSeeds = beyondSeeds || own.chains > uint64(recentGaps*(r.MaxMissing+1)+1)
			}
		}
	}
	if !beyondSeeds {
		t.Fatal("fixture: no search measured more periods than the seeds and the phase refinement")
	}
}

// Refined candidates are hypotheses the search tries, so they must not make
// chance chains fire more often than the seeds alone. On these fixed Poisson
// trains, with the rule of scenario a) and MaxChance 0.1, ad3ab30f (seeds
// only) fires on 60; 5b816f66, whose Chance did not charge the refined
// candidates, fired on 107.
func TestRefinedCandidatesDoNotRaiseTheNullFalseAlarmRate(t *testing.T) {
	const trains, seedsOnly = 10000, 60
	fired := 0
	for trial := 0; trial < trains; trial++ {
		d := newDet(t, Config{Periodic: []PeriodicRule{scenarioA(0.1)}})
		var evs []Event
		for i, off := range poissonTrain(int64(trial)) {
			evs = append(evs, relayEvent(t, "n"+strconv.Itoa(i), at(off), 1, 2))
		}
		if len(run(t, d, evs)) > 0 {
			fired++
		}
	}
	if fired > seedsOnly+seedsOnly/10 {
		t.Fatalf("%d of %d null trains fired, seeds alone %d (+10%% allowed)", fired, trains, seedsOnly)
	}
	t.Logf("%d of %d null trains fired, seeds alone %d", fired, trains, seedsOnly)
}

// A gap that bridges a missing pulse takes the nearest multiple of the
// seed, not the one below: with a seed of 303s (from the 606s gap), 594s is
// two periods although 594/303 < 2. The range then runs through both
// missing-pulse gaps to exactly 300s, which explains four newest gaps where
// the seed explains two.
func TestRefinementRoundsTheMultipleOfAGap(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel, r.MaxMissing = 6*time.Second, 0, 1
	p := ringOf(&r, 0, 1000*time.Second, 1606*time.Second, 2200*time.Second, 2502*time.Second, 2800*time.Second)
	var sc periodicScratch
	c, ref := p.chainRefined(int64(303*time.Second), &r, &sc)
	if c.gaps != 2 {
		t.Fatalf("fixture: the seed explains %d gaps, want 2", c.gaps)
	}
	if ref != int64(300*time.Second) {
		t.Fatalf("refined period %v, want 5m0s", time.Duration(ref))
	}
	if got := p.recentExplained(ref, &r, &sc); got != 4 {
		t.Fatalf("refined period explains %d newest gaps, want 4", got)
	}
}

// A gap that needs more than MaxMissing+1 periods ends the common range:
// with MaxMissing 0, the oldest gap (590s, two periods) must neither narrow
// the range nor enter the phase estimate, which stays 902s/3 over the three
// newest gaps.
func TestRefinementRangeEndsAtAGapBeyondMaxMissing(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel, r.MaxMissing = 6*time.Second, 0, 0
	p := ringOf(&r, 0, 590*time.Second, 890*time.Second, 1192*time.Second, 1492*time.Second)
	var sc periodicScratch
	c, ref := p.chainRefined(int64(294*time.Second), &r, &sc)
	if c.gaps != 1 {
		t.Fatalf("fixture: the seed explains %d gaps, want 1", c.gaps)
	}
	if want := int64(902*time.Second) / 3; ref != want {
		t.Fatalf("refined period %v, want %v", time.Duration(ref), time.Duration(want))
	}
}

// A refined period equal to a seed already measured is not measured again.
// Newest first, the gaps are 300s, 306s and 294s: the seed 300s explains all
// three, and the seeds 306s and 294s both refine to exactly 300s. The search
// measures three chains: one pass of three gaps per seed, plus a
// verification walk of three gaps for each of the two refined cells.
func TestRefinedPeriodEqualToASeedIsNotMeasuredAgain(t *testing.T) {
	r := experimentalPeriodic()
	r.JitterAbs, r.JitterRel, r.MaxMissing = 6*time.Second, 0, 0
	p := ringOf(&r, 0, 294*time.Second, 600*time.Second, 900*time.Second)
	var sc periodicScratch
	best := p.estimate(&r, &sc)
	if best.period != int64(300*time.Second) || best.gaps != 3 {
		t.Fatalf("fixture: estimate %v over %d gaps, want 5m0s over 3", time.Duration(best.period), best.gaps)
	}
	if sc.chains != 3 || sc.steps != 15 {
		t.Fatalf("%d chains and %d steps, want 3 and 15", sc.chains, sc.steps)
	}
}

// ---- dedup horizon (finding 2) ----

func dedupDet(t *testing.T, horizon, reorder time.Duration) *Detector {
	t.Helper()
	lim := experimentalLimits()
	lim.DedupHorizon, lim.ReorderDelay = horizon, reorder
	return newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeGlobal, time.Minute, 1)}})
}

func observeStatus(t *testing.T, d *Detector, id string, when time.Time) Status {
	t.Helper()
	return d.Observe(relayEvent(t, id, when, 1, 2)).Status
}

// A repeat within the horizon (inclusive) is a duplicate; one ns beyond it
// counts again, with or without another ID expiring the first copy.
func TestDedupHorizonBoundaryIsInclusive(t *testing.T) {
	const h = 2 * time.Minute
	for _, c := range []struct {
		name  string
		after time.Duration
		want  Status
	}{
		{"one ns before", h - time.Nanosecond, StatusDuplicate},
		{"at the horizon", h, StatusDuplicate},
		{"one ns after", h + time.Nanosecond, StatusAccepted},
	} {
		for _, other := range []bool{false, true} {
			name := c.name
			if other {
				name += " after another ID"
			}
			t.Run(name, func(t *testing.T) {
				d := dedupDet(t, h, 0)
				if got := observeStatus(t, d, "same", t0); got != StatusAccepted {
					t.Fatal(got)
				}
				if other {
					if got := observeStatus(t, d, "other", at(c.after)); got != StatusAccepted {
						t.Fatal(got)
					}
				}
				if got := observeStatus(t, d, "same", at(c.after)); got != c.want {
					t.Fatalf("repeat %v after the first copy: %v, want %v", c.after, got, c.want)
				}
			})
		}
	}
}

// An older copy arriving out of order stays a duplicate and neither deletes
// nor revives the newer remembered copy.
func TestDedupOlderCopyOutOfOrderKeepsTheNewerCopy(t *testing.T) {
	const h, rd = 2 * time.Minute, 30 * time.Second
	d := dedupDet(t, h, rd)
	if got := observeStatus(t, d, "same", at(3*time.Minute)); got != StatusAccepted {
		t.Fatal(got)
	}
	if got := observeStatus(t, d, "same", at(2*time.Minute+45*time.Second)); got != StatusDuplicate {
		t.Fatalf("older copy within the reorder delay: %v, want duplicate", got)
	}
	// still measured from the newer copy at 3m
	if got := observeStatus(t, d, "same", at(3*time.Minute+h)); got != StatusDuplicate {
		t.Fatalf("repeat at the horizon of the newer copy: %v, want duplicate", got)
	}
	if got := observeStatus(t, d, "same", at(3*time.Minute+h+time.Nanosecond)); got != StatusAccepted {
		t.Fatalf("repeat beyond the horizon of the newer copy: %v, want accepted", got)
	}
	if s := d.Snapshot(); s.Accepted != 2 || s.Duplicates != 2 {
		t.Fatalf("accepted=%d duplicates=%d, want 2 and 2", s.Accepted, s.Duplicates)
	}
}

// An older copy more than the horizon before the remembered one is still a
// duplicate: only a copy beyond the horizon after the remembered one counts
// again. The remembered copy (10m) is still held for the reorder delay, so
// nothing has been processed and the older copy is not late.
func TestDedupOlderCopyBeyondTheHorizonIsStillADuplicate(t *testing.T) {
	const h, rd = 6 * time.Minute, 5 * time.Minute
	d := dedupDet(t, h, rd)
	if got := observeStatus(t, d, "same", at(10*time.Minute)); got != StatusAccepted {
		t.Fatal(got)
	}
	if got := observeStatus(t, d, "same", at(10*time.Minute-h-time.Nanosecond)); got != StatusDuplicate {
		t.Fatalf("older copy beyond the horizon: %v, want duplicate", got)
	}
	if s := d.Snapshot(); s.Accepted != 1 || s.Duplicates != 1 {
		t.Fatalf("accepted=%d duplicates=%d, want 1 and 1", s.Accepted, s.Duplicates)
	}
}

// ---- automatic history start (finding 3) ----

func historyCfg(reorder time.Duration, buffer int) Config {
	lim := experimentalLimits()
	lim.ReorderDelay, lim.MaxReorderBuffer = reorder, buffer
	return Config{Limits: lim, NewStream: []NewStreamRule{{Name: "ns", Scope: ScopeStream, Window: time.Minute, MinCount: 1,
		CensorWindow: 10 * time.Second, QuietPeriod: 10 * time.Second, State: quickState()}}}
}

// censoredByTrigger returns, per trigger ID, whether its new-stream
// activation was left-censored.
func censoredByTrigger(cs []Candidate) map[TxID]bool {
	out := map[TxID]bool{}
	for _, c := range filter(cs, "ns", StateActive) {
		out[c.TriggerID] = c.NewStream.Censored
	}
	return out
}

// Whatever releases the first event from the reorder buffer (a later
// Observe, Advance, Flush or a forced release), the automatic history start
// is that event's time, so the arrival order within the delay does not
// change the result.
func TestAutoHistoryStartIsTheFirstProcessedEventOnEveryReleasePath(t *testing.T) {
	early := func(t *testing.T) Event { return relayEvent(t, "early", t0, 1, 2) }
	later := func(t *testing.T) Event { return relayEvent(t, "later", at(20*time.Second), 1, 3) }
	want := map[TxID]bool{"early": true, "later": false}
	for _, c := range []struct {
		name string
		feed func(t *testing.T, d *Detector) []Candidate
	}{
		{"watermark", func(t *testing.T, d *Detector) []Candidate {
			out := d.Observe(later(t)).Candidates
			out = append(out, d.Observe(early(t)).Candidates...)
			return append(out, d.Advance(at(20*time.Second+30*time.Second)).Candidates...)
		}},
		{"flush", func(t *testing.T, d *Detector) []Candidate {
			out := d.Observe(later(t)).Candidates
			out = append(out, d.Observe(early(t)).Candidates...)
			return append(out, d.Flush().Candidates...)
		}},
		{"later observe", func(t *testing.T, d *Detector) []Candidate {
			out := d.Observe(later(t)).Candidates
			out = append(out, d.Observe(early(t)).Candidates...)
			return append(out, d.Observe(relayEvent(t, "far", at(10*time.Minute), 1, 4)).Candidates...)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := censoredByTrigger(c.feed(t, newDet(t, historyCfg(30*time.Second, 1024))))
			for id, cens := range want {
				if g, ok := got[id]; !ok || g != cens {
					t.Fatalf("%s: censored=%v present=%v, want censored=%v (all: %v)", id, g, ok, cens, got)
				}
			}
		})
	}
	// forced release: the buffer holds one event, so the second Observe forces
	// the oldest (early) out first
	t.Run("forced release", func(t *testing.T) {
		d := newDet(t, historyCfg(30*time.Second, 1))
		out := d.Observe(later(t)).Candidates
		out = append(out, d.Observe(early(t)).Candidates...)
		out = append(out, d.Flush().Candidates...)
		if got := censoredByTrigger(out); got["later"] {
			t.Fatalf("later stream censored after a forced release: %v", got)
		}
	})
}

// An explicit HistoryStart is used as given, never replaced by the first
// processed event.
func TestExplicitHistoryStartIsKept(t *testing.T) {
	for _, c := range []struct {
		name  string
		start time.Time
		want  map[TxID]bool
	}{
		{"well before the data", at(-time.Hour), map[TxID]bool{"early": false, "later": false}},
		{"between the events", at(15 * time.Second), map[TxID]bool{"later": true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := historyCfg(30*time.Second, 1024)
			cfg.HistoryStart = c.start
			evs := []Event{relayEvent(t, "later", at(20*time.Second), 1, 3)}
			if c.start.Before(t0) {
				evs = append(evs, relayEvent(t, "early", t0, 1, 2))
			}
			got := censoredByTrigger(run(t, newDet(t, cfg), evs))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("censored %v, want %v", got, c.want)
			}
		})
	}
}

// End of replay input: sorted and reversed lines within the reorder delay
// give the same report. Stats.EffectiveDedupHorizon is left out: it is
// measured from the dedup log in acceptance order and so reads up to
// ReorderDelay low for out-of-order input (unchanged since ad3ab30f).
func TestReplayResultDoesNotDependOnOrderWithinTheDelay(t *testing.T) {
	cfg := historyCfg(30*time.Second, 1024)
	cfg.Expected.Mode = ExpectedInclude
	lines := []string{replayLineAt("early", "2030-01-07T12:00:00Z", "02"), replayLineAt("later", "2030-01-07T12:00:20Z", "03")}
	opt := ReplayOptions{Start: t0, End: at(time.Hour), MaxLineBytes: 4096}
	var reports []string
	for _, in := range [][]string{lines, {lines[1], lines[0]}} {
		rep, err := Replay(strings.NewReader(strings.Join(in, "\n")), cfg, opt)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Candidates) == 0 {
			t.Fatal("fixture: no candidates")
		}
		rep.Stats.EffectiveDedupHorizon = 0
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		reports = append(reports, string(b))
	}
	if reports[0] != reports[1] {
		t.Fatalf("sorted and reversed input differ:\n%s\n%s", reports[0], reports[1])
	}
}

// ---- config ownership (finding 4) ----

func payloadCfg(types []PayloadType) Config {
	per := experimentalPeriodic()
	per.PayloadTypes = types
	return Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude},
		HistoryStart: at(-time.Hour),
		Rate:         []RateRule{{Name: "r", Scope: ScopeGlobal, Window: time.Minute, Threshold: 1, PayloadTypes: types, State: quickState()}},
		NewStream: []NewStreamRule{{Name: "ns", Scope: ScopeStream, Window: time.Minute, MinCount: 1,
			CensorWindow: 10 * time.Second, QuietPeriod: 10 * time.Second, PayloadTypes: types, State: quickState()}},
		Periodic: []PeriodicRule{per}}
}

// New copies every PayloadTypes slice: nil stays nil, empty stays empty, and
// a non-empty filter never shares its backing array with the caller.
func TestNewCopiesEveryPayloadFilter(t *testing.T) {
	for _, c := range []struct {
		name  string
		types []PayloadType
	}{{"nil", nil}, {"empty", make([]PayloadType, 0, 4)}, {"nonempty", []PayloadType{5, 6}}} {
		t.Run(c.name, func(t *testing.T) {
			cfg := payloadCfg(c.types)
			d, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string][2][]PayloadType{
				"rate":      {cfg.Rate[0].PayloadTypes, d.cfg.Rate[0].PayloadTypes},
				"newstream": {cfg.NewStream[0].PayloadTypes, d.cfg.NewStream[0].PayloadTypes},
				"periodic":  {cfg.Periodic[0].PayloadTypes, d.cfg.Periodic[0].PayloadTypes},
			}
			for kind, p := range got {
				caller, own := p[0], p[1]
				if (caller == nil) != (own == nil) || !reflect.DeepEqual(caller, own) {
					t.Errorf("%s: copy %v (nil=%v), caller %v (nil=%v)", kind, own, own == nil, caller, caller == nil)
				}
				if cap(caller) > 0 && cap(own) > 0 && &caller[:1][0] == &own[:1][0] {
					t.Errorf("%s: PayloadTypes shares its backing array with the caller", kind)
				}
			}
		})
	}
}

// After New, changing the caller's filters changes nothing for any rule
// kind, and running the detector leaves the caller's config as it was.
func TestCallerConfigAndDetectorAreIndependent(t *testing.T) {
	cfg := payloadCfg([]PayloadType{5})
	before := payloadCfg([]PayloadType{5})
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Rate[0].PayloadTypes[0] = 4
	cfg.NewStream[0].PayloadTypes[0] = 4
	cfg.Periodic[0].PayloadTypes[0] = 4
	var evs []Event
	for i := 0; i < experimentalPeriodic().MinPulses; i++ {
		evs = append(evs, relayEvent(t, "p"+string(rune('a'+i)), at(time.Duration(i)*5*time.Minute), 1, 2))
	}
	cs := run(t, d, evs)
	for _, rule := range []string{"r", "ns", "per"} {
		if len(filter(cs, rule, StateActive)) == 0 {
			t.Errorf("rule %s: no activation after the caller changed its filter:\n%s", rule, describe(cs))
		}
	}
	cfg.Rate[0].PayloadTypes[0] = 5
	cfg.NewStream[0].PayloadTypes[0] = 5
	cfg.Periodic[0].PayloadTypes[0] = 5
	if !reflect.DeepEqual(cfg, before) {
		t.Fatalf("the detector changed the caller's config:\n got %+v\nwant %+v", cfg, before)
	}
}

// ---- replay route invariants (finding 5) ----

// Route and first hop parse with NewRoute's invariants: width 0 only with no
// bytes, the width matching the bytes, hex only; nothing is dropped silently.
func TestReplayRouteWidthInvariants(t *testing.T) {
	for _, c := range []struct {
		name            string
		kind, hop, full string
		ok              bool
		hopW, fullW     int
	}{
		{"route width 0 with bytes", "relay", "1:5c", "0:deadbeef", false, 0, 0},
		{"hop width 0 with bytes (relay)", "relay", "0:5c", "", false, 0, 0},
		{"hop width 0 with bytes (nopath)", "nopath", "0:5c", "", false, 0, 0},
		{"hop width without bytes", "relay", "1:", "", false, 0, 0},
		{"route width without bytes", "relay", "1:5c", "2:", false, 0, 0},
		{"hop bytes without width", "relay", ":5c", "", false, 0, 0},
		{"route bytes without width", "relay", "1:5c", ":5c00", false, 0, 0},
		{"hop invalid hex", "relay", "1:zz", "", false, 0, 0},
		{"route invalid hex", "relay", "1:5c", "1:5cz0", false, 0, 0},
		{"hop length does not match width", "relay", "2:5c", "", false, 0, 0},
		{"route length not a multiple of width", "relay", "2:5c01", "2:5c01aa", false, 0, 0},
		{"hop wider than MaxHopWidth", "relay", "4:5c010203", "", false, 0, 0},
		{"empty route (\"\")", "relay", "1:5c", "", true, 1, 0},
		{"empty route (\"-\")", "relay", "1:5c", "-", true, 1, 0},
		{"empty route (\"0:\")", "relay", "1:5c", "0:", true, 1, 0},
		{"empty hop (\"0:\", nopath)", "nopath", "0:", "", true, 0, 0},
		{"1-byte route", "relay", "1:5c", "1:5c11", true, 1, 1},
		{"2-byte route", "relay", "2:5c01", "2:5c01aa02", true, 2, 2},
		{"3-byte route", "relay", "3:5c0102", "3:5c0102aabbcc", true, 3, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			var rec ReplayRecord
			if err := json.Unmarshal([]byte(replayLine("x")), &rec); err != nil {
				t.Fatal(err)
			}
			rec.RouteKind, rec.FirstHop, rec.FullRoute = c.kind, c.hop, c.full
			e, err := rec.Event()
			if c.ok != (err == nil) {
				t.Fatalf("first_hop %q full_route %q: err %v, want valid=%v", c.hop, c.full, err, c.ok)
			}
			if c.ok && (e.FirstHop.Width() != c.hopW || e.FullRoute.Width() != c.fullW) {
				t.Fatalf("widths hop=%d route=%d, want %d and %d", e.FirstHop.Width(), e.FullRoute.Width(), c.hopW, c.fullW)
			}
		})
	}
}

// A replay line whose first hop has width 0 but bytes is counted invalid.
func TestReplayRejectsZeroWidthFirstHopWithBytes(t *testing.T) {
	var rec ReplayRecord
	if err := json.Unmarshal([]byte(replayLine("x")), &rec); err != nil {
		t.Fatal(err)
	}
	rec.RouteKind, rec.FirstHop = "nopath", "0:5c"
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Replay(strings.NewReader(string(line)), replayFixtureConfig(t), ReplayOptions{Start: t0, End: at(time.Hour), MaxLineBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Input.Accepted != 0 || rep.Input.Invalid != 1 {
		t.Fatalf("first hop with width 0 and bytes: %+v", rep.Input)
	}
}

// ---- BurstGap validation (finding 6) ----

// BurstGap must stay below MinPeriod/2 for every value, with no overflow.
func TestBurstGapValidationBoundaries(t *testing.T) {
	const m = time.Minute
	for _, c := range []struct {
		name      string
		minPeriod time.Duration
		gap       time.Duration
		ok        bool
	}{
		{"zero", m, 0, true},
		{"largest allowed (even MinPeriod)", m, m/2 - 1, true},
		{"half (even MinPeriod)", m, m / 2, false},
		{"half plus 1ns (even MinPeriod)", m, m/2 + 1, false},
		{"largest allowed (odd MinPeriod)", m + 1, m / 2, true},
		{"one ns over (odd MinPeriod)", m + 1, m/2 + 1, false},
		{"MaxInt64/2 (2x fits)", m, math.MaxInt64 / 2, false},
		{"MaxInt64/2+1 (2x overflows)", m, math.MaxInt64/2 + 1, false},
		{"MaxInt64", m, math.MaxInt64, false},
		{"negative", m, -1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := experimentalPeriodic()
			r.MinPeriod, r.BurstGap = c.minPeriod, c.gap
			cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{r}}
			if err := cfg.Validate(); (err == nil) != c.ok {
				t.Fatalf("BurstGap %d with MinPeriod %d: err %v, want valid=%v", c.gap, c.minPeriod, err, c.ok)
			}
		})
	}
}

// ---- fuzz targets for the changed parsing and search ----

// Any first_hop / full_route text either is rejected or yields exactly the
// width and bytes written: nothing is dropped or normalised silently.
func FuzzReplayRouteParsing(f *testing.F) {
	for _, s := range [][3]string{{"relay", "1:5c", "1:5c11"}, {"relay", "1:5c", "0:deadbeef"},
		{"nopath", "0:5c", ""}, {"relay", "2:5c01", "2:5c01aa"}, {"relay", "3:5c0102", "-"}, {"nopath", ":", "0:"}} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, kind, hop, full string) {
		var rec ReplayRecord
		if err := json.Unmarshal([]byte(replayLine("x")), &rec); err != nil {
			t.Fatal(err)
		}
		rec.RouteKind, rec.FirstHop, rec.FullRoute = kind, hop, full
		e, err := rec.Event()
		if err != nil {
			return
		}
		for _, c := range []struct {
			text  string
			width int
			bytes []byte
		}{{hop, e.FirstHop.Width(), e.FirstHop.Bytes()}, {full, e.FullRoute.Width(), e.FullRoute.Bytes()}} {
			w, b, perr := parseWidthHex(c.text)
			if perr != nil {
				t.Fatalf("%q parses with error %v but the event was accepted", c.text, perr)
			}
			if len(b) == 0 {
				if c.width != 0 || w != 0 {
					t.Fatalf("%q: empty bytes, width %d (parsed %d)", c.text, c.width, w)
				}
			} else if c.width != w || string(c.bytes) != string(b) {
				t.Fatalf("%q: event has width %d bytes %x, text says %d %x", c.text, c.width, c.bytes, w, b)
			}
		}
	})
}

// Arbitrary pulse gaps and rule parameters: every pulse stays within
// periodicWorkBound, the fused pass equals chainFor, a refined cell never
// explains fewer of the newest gaps than its seed, and the search never
// ranks below the seeds alone.
func FuzzPeriodicSearch(f *testing.F) {
	f.Add(uint8(2), uint8(6), uint8(0), []byte{0, 12, 0, 12, 0, 12, 0, 12, 0, 12, 0, 12, 0, 12, 0, 12, 0, 12})
	f.Add(uint8(8), uint8(0), uint8(8), []byte{5, 200, 40, 7, 90, 3, 250, 17, 64, 64, 64, 1, 2, 3})
	f.Fuzz(func(t *testing.T, missing, jitAbs, jitRel uint8, gaps []byte) {
		r := experimentalPeriodic()
		r.MaxMissing = int(missing % (maxMissingLimit + 1))
		r.JitterAbs = time.Duration(jitAbs%30) * time.Second
		r.JitterRel = float64(jitRel%9) / 100
		r.MaxJitterFraction = 1
		cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{r}}
		if cfg.Validate() != nil {
			return
		}
		maxC, maxS := periodicWorkBound(&r)
		p := newPeriodicState(r.HistoryLen)
		var sc periodicScratch
		now := t0.UnixNano()
		for i, b := range gaps {
			if i >= 4*r.HistoryLen {
				break
			}
			// gaps around 5 minutes: base 240s plus 0..127s in 1s steps, or up
			// to 4x for missing pulses
			now += int64(240*time.Second) + int64(b&0x7f)*int64(time.Second)
			if b&0x80 != 0 {
				now += int64(b&0x3) * int64(5*time.Minute)
			}
			c0, s0 := sc.chains, sc.steps
			p.observe(now, TrafficUnclassified, &r, &sc)
			if dc, ds := sc.chains-c0, sc.steps-s0; dc > maxC || ds > maxS {
				t.Fatalf("pulse %d: %d chains, %d steps, bound %d and %d", i, dc, ds, maxC, maxS)
			}
		}
		for i := p.n - 1; i >= 1 && i >= p.n-recentGaps; i-- {
			g := p.at(i) - p.at(i-1)
			for k := 1; k <= r.MaxMissing+1; k++ {
				seed := g / int64(k)
				if seed < int64(r.MinPeriod) || seed > int64(r.MaxPeriod) {
					continue
				}
				c, ref := p.chainRefined(seed, &r, &sc)
				if want := p.chainFor(seed, &r, &sc); c != want {
					t.Fatalf("seed %v: fused %+v, chainFor %+v", time.Duration(seed), c, want)
				}
				if ref != seed && p.recentExplained(ref, &r, &sc) <= p.recentExplained(seed, &r, &sc) {
					t.Fatalf("seed %v refined to %v without explaining more newest gaps", time.Duration(seed), time.Duration(ref))
				}
			}
		}
		if p.n >= 2 {
			want, got := estimateSeedsOnly(p, &r, &sc), p.estimate(&r, &sc)
			if got != want && (!got.meets(&r) || p.chance(got, &r) > r.MaxChance || (want.gaps > 0 && !got.outranks(want, &r))) {
				t.Fatalf("estimate %+v replaced the seeds' %+v without being able to signal and outranking it", got, want)
			}
		}
	})
}
