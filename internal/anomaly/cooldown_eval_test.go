package anomaly

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// Phase A of issue #130: evaluate StateParams.Cooldown on a deterministic
// synthetic pulse train built from the documented ground truth of a 5-minute
// channel bot. No staging export is used; every number below is a named test
// fixture, not a recommendation.

type botTrain struct {
	events    []Event
	lastPulse time.Duration // offset of the bot's last pulse
	holes     []time.Duration
}

// syntheticBot builds hours of a 5 min bot: per-pulse jitter sd 3 s (gap
// residual median ~3 s, p95 ~8 s), ~30 % multi-part pulses (1-2 extra parts,
// 0-55 s after the first), ~5 % single missed pulses and one 35 min hole.
func syntheticBot(t testing.TB, seed int64, hours float64, holeAt, holeLen time.Duration) botTrain {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	total := time.Duration(hours * float64(time.Hour))
	var bt botTrain
	n := 0
	for k := 0; ; k++ {
		base := time.Duration(k) * 5 * time.Minute
		if base > total {
			break
		}
		if base >= holeAt && base < holeAt+holeLen {
			continue
		}
		if k > 0 && rng.Float64() < 0.05 { // single missed pulse
			continue
		}
		first := base + time.Duration(rng.NormFloat64()*3*float64(time.Second))
		if first < 0 {
			first = 0
		}
		parts := 1
		if rng.Float64() < 0.30 {
			parts += 1 + rng.Intn(2)
		}
		for j := 0; j < parts; j++ {
			off := time.Duration(0)
			if j > 0 {
				off = time.Duration(rng.Float64() * 55 * float64(time.Second))
			}
			bt.events = append(bt.events, relayEvent(t, fmt.Sprintf("bot-%05d", n), at(first+off), 0x11, 0x22))
			n++
		}
		bt.lastPulse = first
	}
	sort.SliceStable(bt.events, func(i, j int) bool { return bt.events[i].Time.Before(bt.events[j].Time) })
	bt.holes = []time.Duration{holeAt}
	return bt
}

// botRule is the issue's evidence-section variant (burst_gap 15 s,
// jitter_abs 10 s) with the given end_after_quiet and cooldown.
func botRule(quiet, cooldown time.Duration, missed int) PeriodicRule {
	r := experimentalPeriodic()
	r.EndAfterMissedPulses = missed
	r.BurstGap = 15 * time.Second
	r.JitterAbs = 10 * time.Second
	r.State = StateParams{EndAfterQuiet: quiet, Cooldown: cooldown}
	return r
}

type incidentStats struct {
	eventIDs     int
	activations  int
	reopens      int
	ends         int
	flaps        int // ends followed by more bot pulses (false stops)
	firstAlarm   time.Duration
	finalEnd     time.Duration // -1: never ended in the run
	finalEndID   string
	stopLatency  time.Duration // finalEnd - last pulse; -1 if none
	activeCovers time.Duration
}

func evalCooldown(t testing.TB, bt botTrain, quiet, cooldown time.Duration, missed int) incidentStats {
	t.Helper()
	d := newDet(t, Config{Periodic: []PeriodicRule{botRule(quiet, cooldown, missed)}})
	// advance past the bot's stop so that the final end is produced
	cs := runUntil(t, d, bt.events, bt.lastPulse+quiet+cooldown+time.Hour)
	st := incidentStats{finalEnd: -1, stopLatency: -1, firstAlarm: -1}
	ids := map[string]bool{}
	var openAt time.Duration = -1
	for _, c := range cs {
		if c.Rule != "per" {
			continue
		}
		switch {
		case c.To == StateActive && c.Reason == ReasonConfirmed:
			st.activations++
			ids[c.EventID] = true
			if st.firstAlarm < 0 {
				st.firstAlarm = c.At.Sub(t0)
			}
			openAt = c.At.Sub(t0)
		case c.To == StateActive && c.Reason == ReasonReopened:
			st.reopens++
			ids[c.EventID] = true
			openAt = c.At.Sub(t0)
		case c.To == StateEnded:
			st.ends++
			if at := c.At.Sub(t0); at < bt.lastPulse-time.Minute {
				st.flaps++
			}
			st.finalEnd, st.finalEndID = c.At.Sub(t0), c.EventID
			if openAt >= 0 {
				st.activeCovers += c.At.Sub(t0) - openAt
			}
			openAt = -1
		}
	}
	st.eventIDs = len(ids)
	if st.finalEnd >= 0 {
		st.stopLatency = st.finalEnd - bt.lastPulse
	}
	return st
}

func TestCooldownEvalTable(t *testing.T) {
	bt := syntheticBot(t, 1, 85.2, 40*time.Hour, 35*time.Minute)
	t.Logf("events=%d lastPulse=%v", len(bt.events), bt.lastPulse)
	for _, q := range []time.Duration{30 * time.Minute, time.Hour} {
		for _, cd := range []time.Duration{0, time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour} {
			s := evalCooldown(t, bt, q, cd, 0)
			t.Logf("quiet=%v cooldown=%v ids=%d act=%d reopen=%d ends=%d flaps=%d first=%v finalEnd=%v stopLat=%v finalKnownAfterStop=%v cover=%.0f%%",
				q, cd, s.eventIDs, s.activations, s.reopens, s.ends, s.flaps, s.firstAlarm, s.finalEnd, s.stopLatency, s.stopLatency+cd,
				100*float64(s.activeCovers)/float64(bt.lastPulse))
		}
	}
	for _, k := range []int{3, 4, 6} {
		s := evalCooldown(t, bt, time.Hour, 0, k)
		t.Logf("TRACKING K=%d quiet=1h ids=%d act=%d ends=%d flaps=%d first=%v finalEnd=%v stopLat=%v cover=%.0f%%",
			k, s.eventIDs, s.activations, s.ends, s.flaps, s.firstAlarm, s.finalEnd, s.stopLatency,
			100*float64(s.activeCovers)/float64(bt.lastPulse))
	}
}

// Phase A conclusion, pinned: no Cooldown value holds the bot as one incident
// AND reports its stop. Cooldown only merges EventIDs; every end it merges
// is still an end/reopen transition, the final end lands before the bot's
// last pulse (a false stop), and the real stop is never reported.
func TestCooldownAloneCannotReportTheStop(t *testing.T) {
	bt := syntheticBot(t, 1, 85.2, 40*time.Hour, 35*time.Minute)
	for _, cd := range []time.Duration{0, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour} {
		s := evalCooldown(t, bt, time.Hour, cd, 0)
		if s.stopLatency >= 0 {
			t.Fatalf("cooldown %v: final end %v is after the last pulse %v; the fixture no longer shows the problem", cd, s.finalEnd, bt.lastPulse)
		}
		if cd >= 4*time.Hour && s.eventIDs > 2 {
			t.Fatalf("cooldown %v: %d event IDs, want the cooldown to merge them", cd, s.eventIDs)
		}
		if cd >= 4*time.Hour && s.flaps < 20 {
			t.Fatalf("cooldown %v: only %d false stops", cd, s.flaps)
		}
	}
}

func TestTrackingHoldsTheBotAndReportsTheStop(t *testing.T) {
	bt := syntheticBot(t, 1, 85.2, 40*time.Hour, 35*time.Minute)
	s := evalCooldown(t, bt, time.Hour, 0, 3)
	if s.stopLatency <= 0 || s.stopLatency > 4*5*time.Minute {
		t.Fatalf("stop latency %v, want within (K+1)*P after the last pulse", s.stopLatency)
	}
	cd := evalCooldown(t, bt, time.Hour, 6*time.Hour, 0)
	if s.flaps*3 > cd.flaps {
		t.Fatalf("tracking has %d false stops, cooldown 6h has %d", s.flaps, cd.flaps)
	}
	if s.activations > 8 {
		t.Fatalf("%d activations", s.activations)
	}
}
