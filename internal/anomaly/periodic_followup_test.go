package anomaly

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// Follow-up to the periodic detector: a period search range narrower than
// the jitter, and the traffic label of bursts that mix expected and
// unclassified events.

// A period range narrower than the tolerance: gaps alternate 294s and 306s,
// so every seed g/k lies outside [299s, 301s], yet 300s explains every gap
// within 6s and meets every criterion. The seeds must still be refined into
// the range instead of being dropped before refinement, with an absolute and
// with a relative tolerance.
func TestPeriodicNarrowRangeFindsPeriodBehindOutOfRangeSeeds(t *testing.T) {
	for _, j := range []struct {
		name string
		abs  time.Duration
		rel  float64
	}{{"absolute 6s", 6 * time.Second, 0}, {"relative 2%", 0, 0.02}} {
		t.Run(j.name, func(t *testing.T) {
			rule := experimentalPeriodic()
			rule.MinPeriod, rule.MaxPeriod = 299*time.Second, 301*time.Second
			rule.JitterAbs, rule.JitterRel, rule.MaxJitterFraction = j.abs, j.rel, 1
			cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{rule}}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			const pulses = 32
			offs := make([]time.Duration, pulses)
			var events []Event
			for i := range offs {
				offs[i] = time.Duration(i) * 5 * time.Minute
				if i%2 == 1 {
					offs[i] -= 6 * time.Second
				}
				events = append(events, relayEvent(t, fmt.Sprintf("narrow-%02d", i), at(offs[i]), 1, 2))
			}

			// The fixture is valid: 300s explains the whole chain, meets the
			// criteria and passes MaxChance, and no seed g/k is in range.
			p := ringOf(&rule, offs...)
			p.evaluated = pulses
			var sc periodicScratch
			ideal := p.chainFor(int64(300*time.Second), &rule, &sc)
			if ideal.gaps != pulses-1 || !ideal.meets(&rule) || p.chance(ideal, &rule) > rule.MaxChance {
				t.Fatalf("invalid repro: ideal=%+v chance=%g", ideal, p.chance(ideal, &rule))
			}
			for _, g := range []time.Duration{294 * time.Second, 306 * time.Second} {
				for k := 1; k <= rule.MaxMissing+1; k++ {
					if s := g / time.Duration(k); s >= rule.MinPeriod && s <= rule.MaxPeriod {
						t.Fatalf("invalid repro: seed %v/%d = %v is in range", g, k, s)
					}
				}
			}

			a := filter(run(t, newDet(t, cfg), events), rule.Name, StateActive)
			if len(a) == 0 {
				t.Fatal("5m train with gaps 294s/306s was not detected with MinPeriod=299s, MaxPeriod=301s")
			}
			ev := a[0].Periodic
			if ev == nil || ev.Period != 5*time.Minute {
				t.Fatalf("detected period %+v, want 5m0s", ev)
			}
		})
	}
}

// The traffic label of a periodic chain follows the events of its pulses:
// a burst that merges an expected and an unclassified event is mixed, in
// either order, exactly as two single-event pulses with those classes are.
//
// A chain is evaluated when its newest pulse starts, so that pulse holds only
// its first event then; the older pulses are complete.
func TestPeriodicBurstTrafficLabel(t *testing.T) {
	x, u, m := TrafficExpected, TrafficUnclassified, TrafficMonitored
	every := func(cls ...TrafficClass) func(int) []TrafficClass {
		return func(int) []TrafficClass { return cls }
	}
	oneAmong := func(burst []TrafficClass, other TrafficClass) func(int) []TrafficClass {
		return func(i int) []TrafficClass {
			if i == 3 {
				return burst
			}
			return []TrafficClass{other}
		}
	}
	for _, c := range []struct {
		name  string
		pulse func(i int) []TrafficClass // classes of the events in pulse i
		want  TrafficLabel
	}{
		{"bursts unclassified then expected", every(u, x), LabelMixed},
		{"one expected-then-unclassified burst among unclassified pulses", oneAmong([]TrafficClass{x, u}, u), LabelMixed},
		{"one unclassified-then-expected burst among unclassified pulses", oneAmong([]TrafficClass{u, x}, u), LabelMixed},
		// controls
		// (mixed already before the fix, only because the newest pulse
		// holds just its expected first event when the chain is evaluated)
		{"bursts expected then unclassified", every(x, u), LabelMixed},
		{"one mixed burst among expected pulses", oneAmong([]TrafficClass{x, u}, x), LabelMixed},
		{"single events alternating", func(i int) []TrafficClass { return []TrafficClass{[]TrafficClass{x, u}[i%2]} }, LabelMixed},
		{"expected bursts", every(x, x), LabelExpected},
		{"unclassified bursts", every(u, u), LabelUnclassified},
		{"mixed bursts with a monitored event", every(x, u, m), LabelMonitored},
	} {
		t.Run(c.name, func(t *testing.T) {
			var evs []Event
			for i := 0; i < 12; i++ {
				for j, cls := range c.pulse(i) {
					e := relayEvent(t, fmt.Sprintf("p%02d-%d", i, j), at(time.Duration(i)*5*time.Minute+time.Duration(j)*time.Second), 1, 2)
					e.Traffic = cls
					evs = append(evs, e)
				}
			}
			a := firstActive(t, run(t, perDet(t, ExpectedInclude), evs))
			if a.Traffic != c.want {
				t.Fatalf("traffic label %v, want %v", a.Traffic, c.want)
			}
		})
	}
}

// seedInReach keeps every seed g/k for which some period in range explains g
// as k periods with its own tolerance, under absolute, relative and combined
// jitter, at realistic magnitudes (sampled).
func TestSeedInReachKeepsEverySeedAPeriodInRangeExplains(t *testing.T) {
	rng := rand.New(rand.NewSource(94))
	checked := 0
	for trial := 0; trial < 20000; trial++ {
		r := experimentalPeriodic()
		r.MinPeriod = time.Duration(30+rng.Intn(7200)) * time.Second
		r.MaxPeriod = r.MinPeriod + time.Duration(1+rng.Int63n(int64(2*r.MinPeriod)))
		r.JitterAbs, r.JitterRel = 0, 0
		switch trial % 3 {
		case 0:
			r.JitterAbs = time.Duration(1 + rng.Int63n(int64(r.MinPeriod/4)))
		case 1:
			r.JitterRel = 0.25 * (1 - rng.Float64())
		default:
			r.JitterAbs, r.JitterRel = time.Duration(1+rng.Int63n(int64(r.MinPeriod/8))), 0.25*rng.Float64()
		}
		minP, maxP := int64(r.MinPeriod), int64(r.MaxPeriod)
		k := 1 + rng.Intn(maxMissingLimit+1)
		// periods and residuals at the edges are where a bound that is off
		// (or uses the wrong period's tolerance) drops a seed
		per := []int64{minP, maxP, minP + rng.Int63n(maxP-minP+1)}[rng.Intn(3)]
		tol := tolNanos(&r, per)
		g := []int64{int64(k)*per - tol, int64(k)*per + tol, int64(k)*per - tol + rng.Int63n(2*tol+1)}[rng.Intn(3)]
		if kk, _ := explain(g, per, tol, k); kk != k {
			continue // tol >= per/2: another k explains g
		}
		checked++
		if !seedInReach(g, k, &r) {
			t.Fatalf("trial %d: %v explains gap %v as %d periods (tol %v), but the seed is dropped; rule %v..%v abs %v rel %g",
				trial, time.Duration(per), time.Duration(g), k, time.Duration(tol), r.MinPeriod, r.MaxPeriod, r.JitterAbs, r.JitterRel)
		}
	}
	if checked < 19000 {
		t.Fatalf("only %d of 20000 trials checked", checked)
	}
}

// A seed outside the range is measured for its refinement but never
// reported: a steady 294s or 306s train is measured (its seed is in the
// seed pool) and never yields a period outside [299s, 301s].
func TestPeriodicOutOfRangeSeedIsNeverReported(t *testing.T) {
	rule := experimentalPeriodic()
	rule.MinPeriod, rule.MaxPeriod = 299*time.Second, 301*time.Second
	rule.JitterAbs, rule.JitterRel, rule.MaxJitterFraction = 6*time.Second, 0, 1
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{rule}}
	for _, step := range []time.Duration{294 * time.Second, 306 * time.Second} {
		offs := make([]time.Duration, 32)
		for i := range offs {
			offs[i] = time.Duration(i) * step
		}
		p := ringOf(&rule, offs...)
		p.evaluated = uint64(len(offs))
		var sc periodicScratch
		est := p.estimate(&rule, &sc)
		if !slices.Contains(sc.seen, int64(step)) {
			t.Fatalf("%v: fixture: the out-of-range seed was not measured (seeds %v)", step, sc.seen)
		}
		if est.gaps > 0 && (est.period < int64(rule.MinPeriod) || est.period > int64(rule.MaxPeriod)) {
			t.Fatalf("%v: estimate %v is outside the range", step, time.Duration(est.period))
		}
		evs := series(t, "steady", t0, len(offs), step, nil, 1, 2)
		for _, c := range run(t, newDet(t, cfg), evs) {
			if c.Periodic != nil && (c.Periodic.Period < rule.MinPeriod || c.Periodic.Period > rule.MaxPeriod) {
				t.Fatalf("%v: reported period %v outside [%v, %v]", step, c.Periodic.Period, rule.MinPeriod, rule.MaxPeriod)
			}
		}
	}
}

// The same, exhaustively over small integer periods, where the truncation of
// tolNanos and the rounding of explain matter most: every gap that some
// period in range explains as k periods is in reach.
func TestSeedInReachIsSoundExhaustively(t *testing.T) {
	checked := 0
	for minP := int64(2); minP <= 40; minP++ {
		for width := int64(1); width <= 12; width++ {
			for _, abs := range []int64{0, 1, 2, 3} {
				for _, rel := range []float64{0, 0.1, 0.13, 0.25} {
					r := PeriodicRule{MinPeriod: time.Duration(minP), MaxPeriod: time.Duration(minP + width),
						JitterAbs: time.Duration(abs), JitterRel: rel}
					for k := 1; k <= maxMissingLimit+1; k++ {
						for per := minP; per <= minP+width; per++ {
							tol := tolNanos(&r, per)
							for g := int64(k)*per - tol; g <= int64(k)*per+tol; g++ {
								if kk, _ := explain(g, per, tol, k); kk != k {
									continue
								}
								checked++
								if !seedInReach(g, k, &r) {
									t.Fatalf("period %d explains gap %d as %d periods (tol %d) but the seed is dropped; range %d..%d abs %d rel %g",
										per, g, k, tol, minP, minP+width, abs, rel)
								}
							}
						}
					}
				}
			}
		}
	}
	if checked < 100000 {
		t.Fatalf("only %d cases checked", checked)
	}
}

// Every seed in range is measured, also where seedInReach alone would drop
// it: with a 1ns tolerance the gap 3*60s+2ns gives the in-range seed 60s
// (floor of g/3), beyond k*MaxPeriod+tol. That seed explains the three
// newest gaps with full coverage and must stay the estimate.
func TestPeriodicInRangeSeedIsAlwaysMeasured(t *testing.T) {
	r := experimentalPeriodic()
	r.MinPeriod, r.MaxPeriod = 30*time.Second, time.Minute
	r.JitterAbs, r.JitterRel = 1, 0
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{r}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	const m = time.Minute
	g0 := 3*m + 2
	if seedInReach(int64(g0), 3, &r) {
		t.Fatal("fixture: seedInReach alone keeps the seed")
	}
	p := ringOf(&r, 0, g0, g0+m-1, g0+2*m, g0+3*m-1)
	p.evaluated = uint64(p.n)
	var sc periodicScratch
	est := p.estimate(&r, &sc)
	if est.period != int64(m) || est.gaps != 3 || est.coverage != 1 {
		t.Fatalf("estimate %v over %d gaps (coverage %g), want 1m0s over 3 (coverage 1)", time.Duration(est.period), est.gaps, est.coverage)
	}
	for i := p.n - 1; i >= 1; i-- {
		g := p.at(i) - p.at(i-1)
		for k := 1; k <= r.MaxMissing+1; k++ {
			if s := g / int64(k); s >= int64(r.MinPeriod) && s <= int64(r.MaxPeriod) && !slices.Contains(sc.seen, s) {
				t.Fatalf("in-range seed %v (gap %v / %d) was not measured", time.Duration(s), time.Duration(g), k)
			}
		}
	}
}

// Relative jitter: the refinement must use the tolerance of the period it
// refines to, not the seed's. JitterRel 2% gives tol(300s) = 6s; gaps repeat
// 305.95s, 305.95s, 294.05s, which 300s explains within 5.95s. With the
// seed's tolerance, the low seed's range (tol 5.881s) is empty, and the high
// seed's range (tol 6.119s) reaches 300.169s, which misses the 294.05s gaps
// with its own tolerance, so no seed refined to a period that explains the
// train and it was never detected.
func TestPeriodicRelativeJitterRefinesWithTheCandidateTolerance(t *testing.T) {
	rule := experimentalPeriodic()
	rule.JitterAbs, rule.JitterRel, rule.MaxJitterFraction = 0, 0.02, 1
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{rule}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	hi, lo := 305950*time.Millisecond, 294050*time.Millisecond
	offs := []time.Duration{0}
	for i := 1; i < 32; i++ {
		g := hi
		if i%3 == 0 {
			g = lo
		}
		offs = append(offs, offs[i-1]+g)
	}
	p := ringOf(&rule, offs...)
	p.evaluated = uint64(len(offs))
	var sc periodicScratch
	ideal := p.chainFor(int64(300*time.Second), &rule, &sc)
	if ideal.gaps != len(offs)-1 || !ideal.meets(&rule) || p.chance(ideal, &rule) > rule.MaxChance {
		t.Fatalf("invalid repro: ideal=%+v chance=%g", ideal, p.chance(ideal, &rule))
	}

	var events []Event
	for i, o := range offs {
		events = append(events, relayEvent(t, fmt.Sprintf("rel-%02d", i), at(o), 1, 2))
	}
	a := filter(run(t, newDet(t, cfg), events), rule.Name, StateActive)
	if len(a) == 0 {
		t.Fatal("train with gaps 305.95s, 305.95s, 294.05s was not detected; 300s explains every gap with JitterRel 2%")
	}
	ev := a[0].Periodic
	if ev == nil {
		t.Fatal("activation without periodic evidence")
	}
	// the detected period explains every gap of the chain with its own
	// tolerance, and is within that tolerance of 300s
	c := p.chainFor(int64(ev.Period), &rule, &sc)
	if c.gaps < int(ev.Support)-1 || ev.Tolerance != time.Duration(tolNanos(&rule, int64(ev.Period))) ||
		(ev.Period-300*time.Second).Abs() > ev.Tolerance {
		t.Fatalf("detected %v (tolerance %v, support %d), whose chain explains %d gaps", ev.Period, ev.Tolerance, ev.Support, c.gaps)
	}
}

// oldAbsRange is the refinement range chainRefined used before periodRange,
// for one gap g as k periods with tolerance tol (then always the seed's):
// [ceil((g-tol)/k), floor((g+tol)/k)], with no lower end while g-tol <= 0.
func oldAbsRange(g, k, tol int64) (lo, hi int64) {
	lo, hi = 1, (g+tol)/k
	if g-tol > 0 {
		lo = max(lo, (g-tol+k-1)/k)
	}
	return lo, hi
}

// With absolute jitter only, periodRange gives exactly the integers of the
// old computation: exhaustively over small gaps and tolerances, and sampled
// at nanosecond magnitudes up to the largest gaps a rule can bridge.
func TestPeriodRangeWithAbsoluteJitterIsTheOldRange(t *testing.T) {
	check := func(g, k, abs int64) {
		r := PeriodicRule{JitterAbs: time.Duration(abs)}
		lo, hi := periodRange(g, k, &r)
		if olo, ohi := oldAbsRange(g, k, abs); lo != olo || hi != ohi {
			t.Fatalf("g=%d k=%d JitterAbs=%d: periodRange [%d, %d], old range [%d, %d]", g, k, abs, lo, hi, olo, ohi)
		}
	}
	for g := int64(1); g <= 2000; g++ {
		for k := int64(1); k <= maxMissingLimit+1; k++ {
			for abs := int64(0); abs <= 40; abs++ {
				check(g, k, abs)
			}
		}
	}
	rng := rand.New(rand.NewSource(108))
	for i := 0; i < 200000; i++ {
		k := 1 + rng.Int63n(maxMissingLimit+1)
		g := 1 + rng.Int63n(k*int64(maxWindow))
		check(g, k, rng.Int63n(int64(maxWindow/2)))
	}
}

// With relative (and combined) jitter, periodRange is exactly the set of P
// with |g - k*P| <= tol(P): by brute force over small integers, and by its
// defining ends at nanosecond magnitudes.
func TestPeriodRangeIsExactlyThePeriodsThatExplainTheGap(t *testing.T) {
	for _, abs := range []int64{0, 1, 3, 7} {
		for _, rel := range []float64{0.01, 0.1, 0.13, 0.25} {
			r := PeriodicRule{JitterAbs: time.Duration(abs), JitterRel: rel}
			for k := int64(1); k <= maxMissingLimit+1; k++ {
				for g := int64(1); g <= 600; g++ {
					lo, hi := periodRange(g, k, &r)
					for per := int64(1); per <= g+abs+1; per++ {
						d := g - k*per
						in := max(d, -d) <= tolNanos(&r, per)
						if in != (per >= lo && per <= hi) {
							t.Fatalf("abs=%d rel=%g k=%d g=%d: P=%d explains=%v, but periodRange is [%d, %d]", abs, rel, k, g, per, in, lo, hi)
						}
					}
				}
			}
		}
	}
	rng := rand.New(rand.NewSource(1081))
	for i := 0; i < 200000; i++ {
		r := PeriodicRule{JitterAbs: time.Duration(rng.Int63n(int64(10 * time.Second))), JitterRel: 0.25 * rng.Float64()}
		k := 1 + rng.Int63n(maxMissingLimit+1)
		g := int64(time.Second) + rng.Int63n(k*int64(maxWindow))
		lo, hi := periodRange(g, k, &r)
		f := func(per int64) int64 { return k*per - tolNanos(&r, per) }
		h := func(per int64) int64 { return k*per + tolNanos(&r, per) }
		if lo > hi || f(hi) > g || f(hi+1) <= g || h(lo) < g || (lo > 1 && h(lo-1) >= g) {
			t.Fatalf("abs=%v rel=%g k=%d g=%d: [%d, %d] are not the exact ends", r.JitterAbs, r.JitterRel, k, g, lo, hi)
		}
	}
}

// The refined period is clamped into the part of the common range that lies
// in [MinPeriod, MaxPeriod]. Both trains have a true period of 300.98s in
// the range [299s, 301s], residuals of 0.90..0.99 tol and two of three gaps
// long, so their phase estimate (mean gap) lies beyond MaxPeriod. The common
// range straddles MaxPeriod: clamped only into the common range, the refined
// period would be above 301s and rejected, although the periods from its
// lower end up to 301s explain every gap.
func TestPeriodicRefinementIsClampedIntoTheSearchRange(t *testing.T) {
	for _, c := range []struct {
		name   string
		abs    time.Duration
		rel    float64
		period int64
		gaps   []int64
	}{
		{"relative 2%", 0, 0.02, 300983680322, []int64{
			295433727560, 295081034588, 306545473781, 306861519212, 306631836381, 306839668100,
			295481269048, 306684537919, 306797954449, 306605636817, 306836785762, 306717842243,
			295064704066, 306841202094, 306918734081, 306475916213, 306738216837, 306599618627,
			295439541190, 306701648665, 306655004215, 295172194564, 295422882548, 306816575831,
		}},
		{"absolute 6s", 6 * time.Second, 0, 300983815273, []int64{
			306831828975, 306822681220, 295457880758, 306702249706, 306592244168, 306845246623,
			295130477268, 306753857882, 306756812033, 306408553938, 295364746775, 306677382170,
			306511141502, 306590311144, 295567381355, 306481296874, 306868730214, 306614050442,
			295348035963, 306797789043, 306408756668, 295272100140, 295171304764, 306667020981,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := experimentalPeriodic()
			r.MinPeriod, r.MaxPeriod = 299*time.Second, 301*time.Second
			r.JitterAbs, r.JitterRel, r.MaxJitterFraction = c.abs, c.rel, 1
			if err := (&Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{r}}).Validate(); err != nil {
				t.Fatal(err)
			}
			// fixture: the true period explains the train and would signal,
			// while the mean gap is above MaxPeriod
			offs := []time.Duration{0}
			var sum int64
			for _, g := range c.gaps {
				offs = append(offs, offs[len(offs)-1]+time.Duration(g))
				sum += g
			}
			ring := ringOf(&r, offs...)
			ring.evaluated = uint64(len(offs))
			var sc periodicScratch
			ideal := ring.chainFor(c.period, &r, &sc)
			if ideal.gaps != len(c.gaps) || !ideal.meets(&r) || ring.chance(ideal, &r) > r.MaxChance || sum/int64(len(c.gaps)) <= int64(r.MaxPeriod) {
				t.Fatalf("invalid fixture: ideal=%+v chance=%g mean gap %v", ideal, ring.chance(ideal, &r), time.Duration(sum/int64(len(c.gaps))))
			}

			p := newPeriodicState(r.HistoryLen)
			cur := t0.UnixNano()
			for i, g := range c.gaps {
				cur += g
				s, ok := p.observe(cur, TrafficUnclassified, &r, &sc)
				if !ok {
					continue
				}
				per := int64(s.ev.Period)
				if per < int64(r.MinPeriod) || per > int64(r.MaxPeriod) || max(per-c.period, c.period-per) > tolNanos(&r, c.period) {
					t.Fatalf("gap %d: signalled %v, want a period in range within tol of %v", i, s.ev.Period, time.Duration(c.period))
				}
				return
			}
			t.Fatalf("no signal over %d gaps; %v and periods up to 301s explain every gap", len(c.gaps), time.Duration(c.period))
		})
	}
}

// The periodRange memo is never stale: one scratch answers interleaved
// queries for different gaps, multiples, rules (the Detector shares its
// scratch between rules) and pulse numbers that share a slot, always with
// periodRange's result.
func TestRangeMemoIsNeverStale(t *testing.T) {
	rules := []PeriodicRule{
		{JitterAbs: 5 * time.Second, JitterRel: 0.02},
		{JitterAbs: 1 * time.Second, JitterRel: 0.02},
		{JitterAbs: 5 * time.Second, JitterRel: 0.03},
		{JitterAbs: 6 * time.Second},
	}
	gaps := []int64{int64(294 * time.Second), int64(300 * time.Second), int64(306 * time.Second), int64(600 * time.Second), int64(887 * time.Second)}
	rng := rand.New(rand.NewSource(1082))
	var sc periodicScratch
	hits := 0
	for i := 0; i < 200000; i++ {
		r := &rules[rng.Intn(len(rules))]
		g, k := gaps[rng.Intn(len(gaps))], int64(1+rng.Intn(maxMissingLimit+1))
		pulse := uint64(rng.Intn(3 * recentGaps))
		if sc.ranges != nil && r.JitterRel != 0 {
			if m := sc.ranges[int(pulse%recentGaps)*(maxMissingLimit+1)+int(k-1)]; m.g == g && m.abs == int64(r.JitterAbs) && m.rel == r.JitterRel {
				hits++
			}
		}
		lo, hi := sc.memoRange(pulse, g, k, r)
		if wlo, whi := periodRange(g, k, r); lo != wlo || hi != whi {
			t.Fatalf("query %d: memo [%d, %d], periodRange [%d, %d] (g=%d k=%d rule %+v pulse %d)", i, lo, hi, wlo, whi, g, k, *r, pulse)
		}
	}
	if hits < 1000 || hits > 199000 {
		t.Fatalf("fixture: %d memo hits of 200000 queries; want both hits and misses", hits)
	}
	if len(sc.ranges) != recentGaps*(maxMissingLimit+1) {
		t.Fatalf("memo holds %d entries, want the fixed %d", len(sc.ranges), recentGaps*(maxMissingLimit+1))
	}
}
