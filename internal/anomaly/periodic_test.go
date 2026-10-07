package anomaly

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// experimentalPeriodic is a named test fixture, not a recommendation.
func experimentalPeriodic() PeriodicRule {
	return PeriodicRule{Name: "per", Scope: ScopeStream, MinPeriod: time.Minute, MaxPeriod: time.Hour,
		BurstGap: 5 * time.Second, JitterAbs: 5 * time.Second, JitterRel: 0.02, MaxMissing: 2,
		MinPulses: 8, HistoryLen: 32, MinCoverage: 0.6, MaxJitterFraction: 0.5, MaxChance: 0.01,
		State: StateParams{EndAfterQuiet: 30 * time.Minute}}
}

func perDet(t testing.TB, mode ExpectedMode) *Detector {
	return newDet(t, Config{Periodic: []PeriodicRule{experimentalPeriodic()}, Expected: ExpectedPolicy{Mode: mode}})
}

func firstActive(t *testing.T, cs []Candidate) Candidate {
	t.Helper()
	a := filter(cs, "per", StateActive)
	if len(a) == 0 {
		t.Fatalf("no periodic activation:\n%s", describe(cs))
	}
	return a[0]
}

func TestPeriodicPerfectFiveMinutesAlertsAtFirstSufficientPulse(t *testing.T) {
	evs := series(t, "p", t0, 12, 5*time.Minute, nil, 0x11, 0x22)
	c := firstActive(t, run(t, perDet(t, ExpectedInclude), evs))
	ev := c.Periodic
	if ev.Period != 5*time.Minute || ev.Support != 8 || ev.Coverage != 1 || ev.JitterMedian != 0 {
		t.Fatalf("evidence %+v", ev)
	}
	if !c.At.Equal(evs[7].Time) || !ev.SufficientAt.Equal(evs[7].Time) {
		t.Fatalf("alert at %v / sufficient %v, want the 8th pulse %v", c.At, ev.SufficientAt, evs[7].Time)
	}
	if ev.Chance > 1e-6 {
		t.Fatalf("chance %g", ev.Chance)
	}
}

func TestPeriodicWithJitter(t *testing.T) {
	// Per-pulse jitter of up to 4 s gives gap residuals of up to 8 s (the
	// detector measures gaps), so this rule uses an explicit 10 s tolerance.
	pattern := []time.Duration{0, 3, -4, 2, -3, 4, -1, 1, -2, 3, -4, 2}
	evs := series(t, "p", t0, 12, 5*time.Minute, func(i int) time.Duration { return pattern[i] * time.Second }, 0x11, 0x22)
	// JitterMedian is measured on gaps, so a per-pulse jitter of +/-4 s gives
	// a gap-residual median around 5 s: the explicit fraction is 0.6.
	r := experimentalPeriodic()
	r.JitterAbs = 10 * time.Second
	r.MaxJitterFraction = 0.6
	ev := firstActive(t, run(t, newDet(t, Config{Periodic: []PeriodicRule{r}}), evs)).Periodic
	// the same series with a 6 s tolerance is (correctly) not accepted
	if a := filter(run(t, perDet(t, ExpectedInclude), evs), "per", StateActive); len(a) != 0 {
		t.Fatalf("gap residuals above the tolerance were accepted: %+v", a[0].Periodic)
	}
	if d := ev.Period - 5*time.Minute; d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("period %v", ev.Period)
	}
	if ev.JitterMedian == 0 || float64(ev.JitterMedian) > 0.6*float64(ev.Tolerance) {
		t.Fatalf("jitter median %v tol %v", ev.JitterMedian, ev.Tolerance)
	}
}

func TestPeriodicBurstsAroundEachPulse(t *testing.T) {
	var evs []Event
	for i := 0; i < 10; i++ {
		for j := 0; j < 3; j++ {
			evs = append(evs, relayEvent(t, fmt.Sprintf("b%02d-%d", i, j), at(time.Duration(i)*5*time.Minute+time.Duration(j)*time.Second), 0x11, 0x22))
		}
	}
	c := firstActive(t, run(t, perDet(t, ExpectedInclude), evs))
	if c.Periodic.Period != 5*time.Minute || c.Periodic.Support != 8 || c.Periodic.Events != 22 {
		// 7 full pulses x 3 events + the first event of the 8th pulse
		t.Fatalf("bursts: %+v", c.Periodic)
	}
	if !c.At.Equal(at(35 * time.Minute)) {
		t.Fatalf("alert must be at the start of the 8th burst, got %v", c.At.Sub(t0))
	}
}

func TestPeriodicSimultaneousTimestampsAreOnePulse(t *testing.T) {
	r := experimentalPeriodic()
	r.BurstGap = 0
	var evs []Event
	for i := 0; i < 10; i++ {
		for j := 0; j < 2; j++ {
			evs = append(evs, relayEvent(t, fmt.Sprintf("s%02d-%d", i, j), at(time.Duration(i)*5*time.Minute), 0x11, 0x22))
		}
	}
	cs := run(t, newDet(t, Config{Periodic: []PeriodicRule{r}}), evs)
	c := firstActive(t, cs)
	if c.Periodic.Support != 8 || c.Periodic.Period != 5*time.Minute {
		t.Fatalf("simultaneous events: %+v", c.Periodic)
	}
}

func TestPeriodicMissingPulses(t *testing.T) {
	var ks []int
	for k := 0; len(ks) < 14; k++ {
		if k%3 != 2 { // every third pulse missing
			ks = append(ks, k)
		}
	}
	var evs []Event
	for i, k := range ks {
		evs = append(evs, relayEvent(t, fmt.Sprintf("m%02d", i), at(time.Duration(k)*5*time.Minute), 0x11, 0x22))
	}
	ev := firstActive(t, run(t, perDet(t, ExpectedInclude), evs)).Periodic
	if ev.Period != 5*time.Minute || ev.Coverage >= 1 || ev.Coverage < 0.6 {
		t.Fatalf("missing pulses: %+v", ev)
	}
}

func TestPeriodicDoesNotPickTheWrongMultiple(t *testing.T) {
	// true period 10 min: the 5-min sub-multiple explains the same gaps only
	// with half the coverage and must lose; it is reported as an alternative
	evs := series(t, "h", t0, 12, 10*time.Minute, nil, 0x11, 0x22)
	c := firstActive(t, run(t, perDet(t, ExpectedInclude), evs))
	if c.Periodic.Period != 10*time.Minute {
		t.Fatalf("picked %v for a 10-minute series", c.Periodic.Period)
	}
	found := false
	for _, a := range c.Periodic.Alternatives {
		if a.Period == 5*time.Minute && a.Coverage == 0.5 {
			found = true
		}
		if a.Period == 20*time.Minute {
			t.Fatalf("20 min cannot explain 10-min gaps: %+v", a)
		}
	}
	if !found {
		t.Fatalf("ambiguous sub-multiple not reported: %+v", c.Periodic.Alternatives)
	}
	// with a coverage floor both explanations meet, the fundamental must
	// still win on coverage
	r := experimentalPeriodic()
	r.MinCoverage = 0.4
	c = firstActive(t, run(t, newDet(t, Config{Periodic: []PeriodicRule{r}}), series(t, "l", t0, 12, 10*time.Minute, nil, 0x11, 0x22)))
	if c.Periodic.Period != 10*time.Minute {
		t.Fatalf("picked %v for a 10-minute series when both explanations meet the criteria", c.Periodic.Period)
	}
	// true period 5 min: 10 min cannot explain the chain
	evs = series(t, "f", t0, 12, 5*time.Minute, nil, 0x11, 0x22)
	c = firstActive(t, run(t, perDet(t, ExpectedInclude), evs))
	if c.Periodic.Period != 5*time.Minute {
		t.Fatalf("picked %v for a 5-minute series", c.Periodic.Period)
	}
}

func TestPeriodicCompetingFiveAndTenMinutes(t *testing.T) {
	// pulses at 0, 5, 10, then 20, 25, 30, then 40...: gaps 5,5,10,5,5,10
	var evs []Event
	for i, cycle := 0, 0; len(evs) < 15; cycle++ {
		for _, off := range []int{0, 5, 10} {
			evs = append(evs, relayEvent(t, fmt.Sprintf("c%02d", i), at(time.Duration(cycle*20+off)*time.Minute), 0x11, 0x22))
			i++
		}
	}
	ev := firstActive(t, run(t, perDet(t, ExpectedInclude), evs)).Periodic
	// the chain at the 8th pulse spans gaps 5,5,10,5,5,10,5: 7 gaps over 9 periods
	if ev.Period != 5*time.Minute || ev.Coverage != 7.0/9.0 {
		t.Fatalf("5-min fundamental with 7/9 coverage expected, got %+v", ev)
	}
}

func TestPeriodicAlarmUsesNoFutureEvidence(t *testing.T) {
	pattern := []time.Duration{0, 2, -3, 1, 4, -2, 0, 3, -1, 2, 1, -4, 0, 2, 3, -3, 1, 0}
	evs := series(t, "c", t0, len(pattern), 5*time.Minute, func(i int) time.Duration { return pattern[i] * time.Second }, 0x11, 0x22)
	// add unrelated noise on the same stream
	evs = append(evs, relayEvent(t, "noise1", at(17*time.Minute), 0x11, 0x22), relayEvent(t, "noise2", at(52*time.Minute), 0x11, 0x22))
	sortEvents(evs)
	full := run(t, perDet(t, ExpectedInclude), evs)
	for cut := 1; cut <= len(evs); cut++ {
		prefix := run(t, perDet(t, ExpectedInclude), evs[:cut])
		var want []Candidate
		for _, c := range full {
			if !c.At.After(evs[cut-1].Time) {
				want = append(want, c)
			}
		}
		if !reflect.DeepEqual(prefix, want) {
			t.Fatalf("cut %d: candidates depend on later events\nprefix:\n%sfull:\n%s", cut, describe(prefix), describe(want))
		}
	}
}

func TestIrregularChatIsNotPeriodic(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var evs []Event
		cur := t0
		for i := 0; i < 200; i++ {
			cur = cur.Add(time.Duration(rng.ExpFloat64() * float64(2*time.Minute)))
			evs = append(evs, relayEvent(t, fmt.Sprintf("x%03d", i), cur, 0x11, 0x22))
		}
		if a := filter(run(t, perDet(t, ExpectedInclude), evs), "per", StateActive); len(a) != 0 {
			t.Fatalf("seed %d: random chat called periodic: %+v", seed, a[0].Periodic)
		}
	}
}

func TestPeriodicExpectedTrafficIsLabelledNotDeleted(t *testing.T) {
	evs := series(t, "e", t0, 12, 5*time.Minute, nil, 0x11, 0x22)
	for i := range evs {
		evs[i].Traffic = TrafficExpected
	}
	inc := firstActive(t, run(t, perDet(t, ExpectedInclude), evs))
	sup := firstActive(t, run(t, perDet(t, ExpectedSuppress), evs))
	if inc.Traffic != LabelExpected || inc.Suppressed {
		t.Fatalf("include mode: %v suppressed=%v", inc.Traffic, inc.Suppressed)
	}
	if sup.Traffic != LabelExpected || !sup.Suppressed {
		t.Fatalf("suppress mode must mark, not drop: %v suppressed=%v", sup.Traffic, sup.Suppressed)
	}
	// a single monitored pulse makes the label monitored, never suppressed
	evs[3].Traffic = TrafficMonitored
	m := firstActive(t, run(t, perDet(t, ExpectedSuppress), evs))
	if m.Traffic != LabelMonitored || m.Suppressed {
		t.Fatalf("monitored: %v suppressed=%v", m.Traffic, m.Suppressed)
	}
}

func TestPeriodicEndsAfterQuiet(t *testing.T) {
	evs := series(t, "q", t0, 12, 5*time.Minute, nil, 0x11, 0x22)
	d := perDet(t, ExpectedInclude)
	cs := run(t, d, evs)
	cs = append(cs, d.Advance(evs[11].Time.Add(2*time.Hour)).Candidates...)
	e := filter(cs, "per", StateEnded)
	if len(e) != 1 || !e[0].At.Equal(evs[11].Time.Add(30*time.Minute)) || e[0].Reason != ReasonQuiet {
		t.Fatalf("periodic episode must end 30 min after the last pulse:\n%s", describe(cs))
	}
}

func sortEvents(evs []Event) {
	for i := 1; i < len(evs); i++ {
		for j := i; j > 0 && evs[j].Time.Before(evs[j-1].Time); j-- {
			evs[j], evs[j-1] = evs[j-1], evs[j]
		}
	}
}

func TestPeriodicNullModelRejectsChanceChains(t *testing.T) {
	// Loose tolerance, coverage and jitter criteria: only the Poisson null
	// stands between dense random traffic and a periodic claim.
	r := PeriodicRule{Name: "per", Scope: ScopeStream, MinPeriod: time.Minute, MaxPeriod: 10 * time.Minute,
		BurstGap: 0, JitterAbs: 20 * time.Second, MaxMissing: 8, MinPulses: 4, HistoryLen: 32,
		MinCoverage: 0.1, MaxJitterFraction: 1, MaxChance: 0.01, State: StateParams{EndAfterQuiet: time.Hour}}
	for seed := int64(1); seed <= 10; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var evs []Event
		cur := t0
		for i := 0; i < 300; i++ {
			cur = cur.Add(time.Second + time.Duration(rng.ExpFloat64()*float64(time.Minute)))
			evs = append(evs, relayEvent(t, fmt.Sprintf("r%03d", i), cur, 0x11, 0x22))
		}
		if a := filter(run(t, newDet(t, Config{Periodic: []PeriodicRule{r}}), evs), "per", StateActive); len(a) != 0 {
			t.Fatalf("seed %d: chance chain accepted: %+v", seed, a[0].Periodic)
		}
	}
	// a genuine series still passes once its chain is long enough
	cs := run(t, newDet(t, Config{Periodic: []PeriodicRule{r}}), series(t, "g", t0, 20, time.Minute, nil, 0x11, 0x22))
	a := filter(cs, "per", StateActive)
	if len(a) != 1 || a[0].Periodic.Support <= 4 || a[0].Periodic.Chance > 0.01 {
		t.Fatalf("genuine series with a loose rule: %+v", a)
	}
}

// A stray extra pulse must not lock the detector onto a sub-harmonic: once
// the clean train continues, the true period is detected again within
// MinPulses pulses.
func TestPeriodicRecoversFromAStrayPulse(t *testing.T) {
	var evs []Event
	for i := 0; i <= 7; i++ { // 0..70 min
		evs = append(evs, relayEvent(t, fmt.Sprintf("a%02d", i), at(time.Duration(i)*10*time.Minute), 0x11, 0x22))
	}
	evs = append(evs, relayEvent(t, "stray", at(75*time.Minute), 0x11, 0x22))
	for i := 8; i <= 30; i++ { // 80..300 min
		evs = append(evs, relayEvent(t, fmt.Sprintf("a%02d", i), at(time.Duration(i)*10*time.Minute), 0x11, 0x22))
	}
	r := experimentalPeriodic()
	r.State.EndAfterQuiet = 11 * time.Minute // end the first episode at the stray pulse
	cs := run(t, newDet(t, Config{Periodic: []PeriodicRule{r}}), evs)
	a := filter(cs, "per", StateActive)
	if len(a) < 2 {
		t.Fatalf("clean 10-minute train after one stray pulse was not detected again:\n%s", describe(cs))
	}
	re := a[len(a)-1]
	if re.Periodic.Period != 10*time.Minute {
		t.Fatalf("re-detected period %v, want 10m", re.Periodic.Period)
	}
	// the chain restarts at 80 min; MinPulses=8 pulses end at 150 min
	if re.At.After(at(150 * time.Minute)) {
		t.Fatalf("re-detected at %v, want by 150m", re.At.Sub(t0))
	}
}

// Calibration of the null model: on Poisson traffic, the share of keys that
// ever fire must not exceed MaxChance (the reported chance is an upper
// bound on the expected number of chance chains per key).
func TestPeriodicNullModelIsCalibratedOnPoissonTraffic(t *testing.T) {
	r := PeriodicRule{Name: "cal", Scope: ScopeStream, MinPeriod: time.Minute, MaxPeriod: time.Hour,
		JitterRel: 0.02, MaxMissing: 1, MinPulses: 4, HistoryLen: 32, MinCoverage: 0.5,
		MaxJitterFraction: 1, MaxChance: 0.05, State: StateParams{EndAfterQuiet: time.Hour}}
	const keys, pulses = 400, 300
	fired := 0
	var sc periodicScratch
	for seed := 0; seed < keys; seed++ {
		rng := rand.New(rand.NewSource(int64(seed) + 1000))
		p := newPeriodicState(r.HistoryLen)
		tt := t0.UnixNano()
		for i := 0; i < pulses; i++ {
			tt += int64(rng.ExpFloat64() * float64(10*time.Minute))
			if _, ok := p.observe(tt, TrafficUnclassified, &r, &sc); ok {
				fired++
				break
			}
		}
	}
	if limit := int(r.MaxChance * keys); fired > limit {
		t.Fatalf("%d of %d Poisson keys fired, MaxChance %.2f allows about %d", fired, keys, r.MaxChance, limit)
	}
}

// The stray-pulse recovery must not depend on MinCoverage: with a floor of
// 0.5 or below the sub-multiple meets the criteria too, and a stray pulse
// before the first activation must not lock the period either. Within
// MinPulses clean pulses after the stray, signals report the true period.
func TestPeriodicStrayPulseRecoveryWithLowCoverageFloors(t *testing.T) {
	for _, cov := range []float64{0.4, 0.5} {
		for _, stray := range []time.Duration{75 * time.Minute, 25 * time.Minute} {
			r := experimentalPeriodic()
			r.MinCoverage = cov
			p := newPeriodicState(r.HistoryLen)
			var sc periodicScratch
			var sig periodicSignal
			var sigAt time.Duration
			for i := 0; i <= 30; i++ {
				when := time.Duration(i) * 10 * time.Minute
				if when > stray && when-10*time.Minute < stray {
					if s, ok := p.observe(at(stray).UnixNano(), TrafficUnclassified, &r, &sc); ok {
						sig, sigAt = s, stray
					}
				}
				if s, ok := p.observe(at(when).UnixNano(), TrafficUnclassified, &r, &sc); ok {
					sig, sigAt = s, when
				}
			}
			if sigAt != 300*time.Minute || sig.ev.Period != 10*time.Minute || sig.ev.Coverage != 1 {
				t.Fatalf("MinCoverage %.1f, stray at %v: last signal at %v with %+v", cov, stray, sigAt, sig.ev)
			}
			// recovered within MinPulses clean pulses of the stray
			if suff := sig.ev.SufficientAt.Sub(t0); suff > stray.Truncate(10*time.Minute)+time.Duration(r.MinPulses)*10*time.Minute {
				t.Fatalf("MinCoverage %.1f, stray at %v: 10m sufficient only at %v", cov, stray, suff)
			}
		}
	}
}
