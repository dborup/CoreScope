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
// the range instead of being dropped before refinement.
func TestPeriodicNarrowRangeFindsPeriodBehindOutOfRangeSeeds(t *testing.T) {
	rule := experimentalPeriodic()
	rule.MinPeriod, rule.MaxPeriod = 299*time.Second, 301*time.Second
	rule.JitterAbs, rule.JitterRel, rule.MaxJitterFraction = 6*time.Second, 0, 1
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
		t.Fatal("5m train with gaps 294s/306s was not detected with MinPeriod=299s, MaxPeriod=301s, tolerance=6s")
	}
	ev := a[0].Periodic
	if ev == nil || ev.Period != 5*time.Minute {
		t.Fatalf("detected period %+v, want 5m0s", ev)
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
// as k periods with its own tolerance, under absolute and relative jitter,
// and its bounds are tight.
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
		lo, hi := int64(k)*minP-tolNanos(&r, minP), int64(k)*maxP+tolNanos(&r, maxP)
		if !seedInReach(lo, k, &r) || !seedInReach(hi, k, &r) || seedInReach(lo-1, k, &r) || seedInReach(hi+1, k, &r) {
			t.Fatalf("trial %d: reach of k=%d is not exactly [%v, %v]", trial, k, time.Duration(lo), time.Duration(hi))
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
