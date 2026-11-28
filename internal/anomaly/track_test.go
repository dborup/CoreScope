package anomaly

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Phase B of issue #130: train tracking for active periodic episodes. All
// values are named test fixtures, not recommendations.

const sec = int64(time.Second)

// trackRule: P ~ 5 min, tolerance 10 s, MaxMissing 2, K = 3 missed pulses.
func trackRule() PeriodicRule {
	r := experimentalPeriodic()
	r.BurstGap = 15 * time.Second
	r.JitterAbs = 10 * time.Second
	r.State = StateParams{EndAfterQuiet: 2 * time.Hour}
	r.EndAfterMissedPulses = 3
	return r
}

func armed(r *PeriodicRule) *pulseTracker {
	tr := &pulseTracker{}
	tr.arm(0, int64(5*time.Minute), tolNanos(r, int64(5*time.Minute)))
	return tr
}

func TestTrackerMatchesPulseWithinToleranceAndMovesAnchor(t *testing.T) {
	r := trackRule()
	tr := armed(&r)
	if got := tr.pulse(300*sec+9*sec, &r); got != trackMatched || tr.anchor != 304*sec+sec/2 {
		t.Fatalf("got %v anchor %d ns; the phase must move halfway to the observation", got, tr.anchor)
	}
	if got := tr.pulse(tr.anchor+300*sec-11*sec, &r); got != trackIgnored {
		t.Fatalf("11 s off the grid must not match, got %v", got)
	}
}

func TestTrackerIgnoresStraysAndMatchesAgainstTheAnchor(t *testing.T) {
	r := trackRule()
	tr := armed(&r)
	// later parts of the pulse at t=0, 9 s and 40 s after it, are neither
	// matches nor breaks, and must not move the anchor
	for _, off := range []int64{9, 40, 55} {
		if got := tr.pulse(off*sec, &r); got != trackIgnored || tr.anchor != 0 {
			t.Fatalf("stray +%ds: got %v anchor %d", off, got, tr.anchor/sec)
		}
	}
	if got := tr.pulse(303*sec, &r); got != trackMatched {
		t.Fatalf("the next real pulse must still match the original anchor, got %v", got)
	}
}

func TestTrackerBridgesAtMostMaxMissing(t *testing.T) {
	r := trackRule() // MaxMissing 2: k up to 3
	for k, want := range map[int64]trackResult{1: trackMatched, 2: trackMatched, 3: trackMatched, 4: trackIgnored} {
		tr := armed(&r)
		if got := tr.pulse(k*300*sec+4*sec, &r); got != want {
			t.Fatalf("k=%d: got %v want %v", k, got, want)
		}
	}
}

func TestTrackerPeriodDriftIsBounded(t *testing.T) {
	r := trackRule()
	tr := armed(&r)
	p0, tol := tr.period, tr.tol
	// a train that is 6 s slower than activation: the estimate follows it,
	// but only up to half a tolerance
	now := int64(0)
	for i := 0; i < 2000; i++ {
		now += 306 * sec
		if got := tr.pulse(now, &r); got != trackMatched {
			t.Fatalf("pulse %d of a slightly slower train was lost", i)
		}
	}
	if tr.period != p0+tol/2 {
		t.Fatalf("period %d s, want the bound %d s", tr.period/sec, (p0+tol/2)/sec)
	}
	// a train that has clearly changed period (gaps 317 s: more than the
	// bound plus a tolerance) is not followed
	for i := 0; i < 5; i++ {
		now += 317 * sec
		if got := tr.pulse(now, &r); got != trackIgnored {
			t.Fatalf("a changed period was followed at pulse %d", i)
		}
	}
}

func TestTrackerLosesATrainThatDriftsFasterThanItFollows(t *testing.T) {
	r := trackRule()
	tr := armed(&r)
	now := int64(0)
	matched := 0
	for i := 0; i < 50; i++ {
		now += 312 * sec // 12 s slower than the activation estimate
		if tr.pulse(now, &r) == trackMatched {
			matched++
		}
	}
	if matched == 50 {
		t.Fatal("a train 12 s off the activation period was followed indefinitely")
	}
}

func TestTrackerFollowsSmallDrift(t *testing.T) {
	r := trackRule()
	tr := armed(&r)
	now := int64(0)
	for i := 0; i < 20; i++ {
		now += 304 * sec
		tr.pulse(now, &r)
	}
	if tr.period <= int64(5*time.Minute) {
		t.Fatalf("period did not follow a +4 s drift: %d", tr.period/sec)
	}
}

func TestTrackerQuietIsKMissedPulsesPlusTolerance(t *testing.T) {
	r := trackRule()
	tr := armed(&r)
	want := int64(3*300+10) * sec
	if got := tr.quiet(&r); got != want {
		t.Fatalf("quiet %d s want %d s", got/sec, want/sec)
	}
	r.State.EndAfterQuiet = 10 * time.Minute // the outer bound wins
	if got := tr.quiet(&r); got != int64(10*time.Minute) {
		t.Fatalf("outer bound: %d s", got/sec)
	}
}

func TestTrackingValidation(t *testing.T) {
	for _, tc := range []struct {
		k    int
		ok   bool
		name string
	}{{0, true, "off"}, {3, true, "on"}, {-1, false, "negative"}, {1000, false, "huge"}} {
		r := trackRule()
		r.EndAfterMissedPulses = tc.k
		cfg := Config{Periodic: []PeriodicRule{r}, Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}}
		err := cfg.Validate()
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err %v", tc.name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "EndAfterMissedPulses") {
			t.Fatalf("%s: unhelpful error %v", tc.name, err)
		}
	}
}

// ---- detector integration ----

// pulseTrain: n pulses of period P from start, each with up to two later
// parts 20-55 s after the first (never within BurstGap), no jitter.
func pulseTrain(t testing.TB, prefix string, start time.Duration, n int, parts bool) []Event {
	var evs []Event
	for i := 0; i < n; i++ {
		base := start + time.Duration(i)*5*time.Minute
		evs = append(evs, relayEvent(t, fmt.Sprintf("%s-%03d-0", prefix, i), at(base), 0x11, 0x22))
		if parts && i >= 10 && i%3 == 0 {
			evs = append(evs, relayEvent(t, fmt.Sprintf("%s-%03d-1", prefix, i), at(base+40*time.Second), 0x11, 0x22))
		}
	}
	return evs
}

// runUntil is run followed by advancing data time to until, so that
// deadlines after the last event (the stop of a train) fire.
func runUntil(t testing.TB, d *Detector, evs []Event, until time.Duration) []Candidate {
	t.Helper()
	out := run(t, d, evs)
	out = append(out, d.Advance(at(until)).Candidates...)
	for {
		r := d.Drain()
		out = append(out, r.Candidates...)
		if !r.More {
			return out
		}
	}
}

func trackedDet(t testing.TB, r PeriodicRule) *Detector {
	return newDet(t, Config{Periodic: []PeriodicRule{r}})
}

func TestTrackingEndsKMissedPulsesAfterTheLastPulse(t *testing.T) {
	r := trackRule()
	evs := pulseTrain(t, "a", 0, 60, true)
	last := at(59 * 5 * time.Minute)
	cs := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	ends := filter(cs, "per", StateEnded)
	if len(ends) != 1 {
		t.Fatalf("want one end:\n%s", describe(cs))
	}
	want := last.Add(3*5*time.Minute + 10*time.Second)
	if !ends[0].At.Equal(want) {
		t.Fatalf("ended at %v, want last pulse + K*P + tol = %v", ends[0].At.Sub(t0), want.Sub(t0))
	}
	if lat := ends[0].At.Sub(last); lat > 4*5*time.Minute {
		t.Fatalf("stop latency %v > (K+1)*P", lat)
	}
}

func TestTrackingDisabledEndsByQuietOnly(t *testing.T) {
	r := trackRule()
	r.EndAfterMissedPulses = 0
	evs := pulseTrain(t, "a", 0, 60, false)
	last := at(59 * 5 * time.Minute)
	ends := filter(runUntil(t, trackedDet(t, r), evs, 24*time.Hour), "per", StateEnded)
	if len(ends) != 1 || !ends[0].At.Equal(last.Add(2*time.Hour)) {
		t.Fatalf("disabled tracking must end EndAfterQuiet after the last hot signal: %s", describe(ends))
	}
}

// A chain that detection alone loses (a stray right before each pulse spoils
// the clean-chain requirement) is held while tracking follows the train.
func TestTrackingHoldsATrainThatDetectionLoses(t *testing.T) {
	r := trackRule()
	r.State.EndAfterQuiet = 30 * time.Minute
	evs := pulseTrain(t, "a", 0, 20, false) // 20 clean pulses: activation at pulse 8
	// afterwards the train continues, but every pulse is preceded by a stray
	// 100 s earlier, so no clean 8-pulse chain exists
	for i := 20; i < 80; i++ {
		base := time.Duration(i) * 5 * time.Minute
		evs = append(evs, relayEvent(t, fmt.Sprintf("s-%03d", i), at(base-100*time.Second), 0x11, 0x22))
		evs = append(evs, relayEvent(t, fmt.Sprintf("a-%03d", i), at(base), 0x11, 0x22))
	}
	sortEvents(evs)
	tracked := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	r.EndAfterMissedPulses = 0
	plain := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	if n := len(filter(plain, "per", StateEnded)); n < 2 {
		t.Fatalf("fixture too easy: plain detection ended only %d times", n)
	}
	if act := filter(tracked, "per", StateActive); len(act) != 1 {
		t.Fatalf("tracking must hold one episode, got %d activations:\n%s", len(act), describe(tracked))
	}
}

func TestTrackingEndsAtARealHole(t *testing.T) {
	r := trackRule()
	evs := pulseTrain(t, "a", 0, 30, false)
	evs = append(evs, pulseTrain(t, "b", 29*5*time.Minute+35*time.Minute, 30, false)...) // 35 min hole
	cs := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	ends := filter(cs, "per", StateEnded)
	if len(ends) != 2 {
		t.Fatalf("want an end at the hole and one at the stop:\n%s", describe(cs))
	}
	lastBefore := at(29 * 5 * time.Minute)
	if !ends[0].At.Equal(lastBefore.Add(3*5*time.Minute + 10*time.Second)) {
		t.Fatalf("hole end at %v", ends[0].At.Sub(t0))
	}
}

func TestTrackingShortGapKeepsTheEpisode(t *testing.T) {
	r := trackRule()
	evs := pulseTrain(t, "a", 0, 30, false)
	// 2 consecutive pulses missing (< K) in the middle
	evs = append(evs[:15], evs[17:]...)
	cs := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	if n := len(filter(cs, "per", StateActive)); n != 1 {
		t.Fatalf("a gap of 2 missed pulses split the episode:\n%s", describe(cs))
	}
}

func TestTrackingChangedPeriodIsEnded(t *testing.T) {
	r := trackRule()
	evs := pulseTrain(t, "a", 0, 20, false)
	lastOld := at(19 * 5 * time.Minute)
	// afterwards a 6 min train starts
	for i := 1; i < 40; i++ {
		evs = append(evs, relayEvent(t, fmt.Sprintf("n-%03d", i), lastOld.Add(time.Duration(i)*6*time.Minute).Add(0), 0x11, 0x22))
	}
	sortEvents(evs)
	cs := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	ends := filter(cs, "per", StateEnded)
	if len(ends) == 0 || ends[0].At.Sub(lastOld) > 4*5*time.Minute {
		t.Fatalf("the old train was stretched over the new period:\n%s", describe(cs))
	}
}

// Tracking never creates activations, and on non-periodic traffic it only
// extends an episode that detection opened.
func TestTrackingNullTrafficAddsNoActivations(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var evs []Event
		cur := time.Duration(0)
		for i := 0; i < 600; i++ {
			cur += time.Duration(rng.ExpFloat64() * float64(5*time.Minute)) // Poisson, same mean rate
			evs = append(evs, relayEvent(t, fmt.Sprintf("p-%d-%04d", seed, i), at(cur), 0x11, 0x22))
		}
		r := trackRule()
		r.State.EndAfterQuiet = 30 * time.Minute
		on := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
		r.EndAfterMissedPulses = 0
		off := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
		if a, b := len(filter(on, "per", StateActive)), len(filter(off, "per", StateActive)); a != b {
			t.Fatalf("seed %d: tracking changed activations %d -> %d", seed, b, a)
		}
	}
}

// After a genuine episode, random traffic at the same mean rate extends the
// episode only briefly: measured and bounded.
func TestTrackingExtensionOnNullTrafficIsBounded(t *testing.T) {
	worst := time.Duration(0)
	for seed := int64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		evs := pulseTrain(t, "a", 0, 12, false)
		cur := 12 * 5 * time.Minute
		for i := 0; i < 200; i++ {
			cur += time.Duration(rng.ExpFloat64() * float64(5*time.Minute))
			evs = append(evs, relayEvent(t, fmt.Sprintf("p-%d-%04d", seed, i), at(cur), 0x11, 0x22))
		}
		r := trackRule()
		r.State.EndAfterQuiet = 30 * time.Minute
		on := filter(runUntil(t, trackedDet(t, r), evs, 24*time.Hour), "per", StateEnded)
		r.EndAfterMissedPulses = 0
		off := filter(runUntil(t, trackedDet(t, r), evs, 24*time.Hour), "per", StateEnded)
		if len(on) == 0 || len(off) == 0 {
			continue
		}
		if ext := on[0].At.Sub(off[0].At); ext > worst {
			worst = ext
		}
	}
	t.Logf("worst extension of an episode on null traffic: %v", worst)
	if worst > 2*time.Hour {
		t.Fatalf("extension on null traffic %v", worst)
	}
}

func TestTrackerPeriodDriftIsBoundedBelow(t *testing.T) {
	r := trackRule()
	tr := armed(&r)
	p0, tol := tr.period, tr.tol
	now := int64(0)
	for i := 0; i < 2000; i++ {
		now += 294 * sec
		if got := tr.pulse(now, &r); got != trackMatched {
			t.Fatalf("pulse %d of a slightly faster train was lost", i)
		}
	}
	if tr.period != p0-tol/2 {
		t.Fatalf("period %d s, want the bound %d s", tr.period/sec, (p0-tol/2)/sec)
	}
}

// Tracking only applies to an active episode: a suspicious one (waiting for
// ConfirmAfter) keeps its normal deadline.
func TestTrackingLeavesUnconfirmedEpisodesAlone(t *testing.T) {
	r := trackRule()
	r.State = StateParams{ConfirmAfter: 3 * time.Hour, EndAfterQuiet: 30 * time.Minute}
	evs := pulseTrain(t, "a", 0, 12, false)
	on := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	r.EndAfterMissedPulses = 0
	off := runUntil(t, trackedDet(t, r), evs, 24*time.Hour)
	if len(filter(off, "per", StateNormal)) != 1 {
		t.Fatalf("fixture: want one unconfirmed episode:\n%s", describe(off))
	}
	if describe(on) != describe(off) {
		t.Fatalf("tracking changed an unconfirmed episode:\n%s\nvs\n%s", describe(on), describe(off))
	}
}
