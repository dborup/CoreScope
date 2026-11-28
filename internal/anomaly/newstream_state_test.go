package anomaly

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func nsRule(minCount int) NewStreamRule {
	return NewStreamRule{Name: "ns", Scope: ScopeStream, Window: time.Hour, MinCount: minCount,
		CensorWindow: 12 * time.Hour, QuietPeriod: 12 * time.Hour, State: quickState()}
}

// historyThen returns a config whose history starts a day before t0, so
// streams appearing at t0 are not censored.
func historyThen(r NewStreamRule) Config {
	return Config{NewStream: []NewStreamRule{r}, HistoryStart: t0.Add(-24 * time.Hour)}
}

func TestNewStreamGenuinelyNew(t *testing.T) {
	evs := series(t, "n", t0, 30, 30*time.Second, nil, 0x11, 0x22)
	cs := run(t, newDet(t, historyThen(nsRule(30))), evs)
	a := filter(cs, "ns", StateActive)
	if len(a) != 1 {
		t.Fatalf("new stream must fire once:\n%s", describe(cs))
	}
	ev := a[0].NewStream
	if ev.Censored || ev.Reactivated || ev.Count != 30 || ev.Age != 29*30*time.Second || !a[0].At.Equal(evs[29].Time) {
		t.Fatalf("evidence %+v at %v", ev, a[0].At)
	}
	if a[0].Confidence != ConfidenceRouteObserved {
		t.Fatalf("confidence %v", a[0].Confidence)
	}
}

func TestNewStreamLeftCensoredIsNeverCertainlyNew(t *testing.T) {
	// history starts at t0: the stream may predate the data
	cfg := Config{NewStream: []NewStreamRule{nsRule(30)}, HistoryStart: t0}
	evs := series(t, "n", t0.Add(time.Minute), 30, 30*time.Second, nil, 0x11, 0x22)
	cs := run(t, newDet(t, cfg), evs)
	a := filter(cs, "ns", StateActive)
	if len(a) != 1 || !a[0].NewStream.Censored || a[0].Confidence != ConfidenceLow {
		t.Fatalf("left-censored stream must be marked censored / low confidence:\n%s", describe(cs))
	}
	// zero HistoryStart = first processed event: the same holds
	cs = run(t, newDet(t, Config{NewStream: []NewStreamRule{nsRule(30)}}), evs)
	if a := filter(cs, "ns", StateActive); len(a) != 1 || !a[0].NewStream.Censored {
		t.Fatal("stream at the start of the data must be censored")
	}
}

func TestNewStreamRestartReplayDoesNotCallOldStreamsNew(t *testing.T) {
	// A stream active for days; a restarted detector replays history from a
	// start point in the middle of it. It must be censored, not new.
	replayStart := t0.Add(48 * time.Hour)
	evs := series(t, "old", replayStart, 40, 20*time.Second, nil, 0x11, 0x22)
	cs := run(t, newDet(t, Config{NewStream: []NewStreamRule{nsRule(30)}, HistoryStart: replayStart}), evs)
	for _, c := range filter(cs, "ns", StateActive) {
		if !c.NewStream.Censored {
			t.Fatalf("restart replay called an old stream new: %+v", c.NewStream)
		}
	}
}

func TestNewStream29Versus30(t *testing.T) {
	for _, n := range []int{29, 30} {
		cs := run(t, newDet(t, historyThen(nsRule(30))), series(t, "n", t0, n, time.Minute, nil, 1, 2))
		if got := len(filter(cs, "ns", StateActive)); (n == 30) != (got == 1) {
			t.Fatalf("%d events in the first hour: %d activations", n, got)
		}
	}
}

func TestNewStreamOnlyCountsTheFirstWindow(t *testing.T) {
	// 29 events in the first hour, the 30th just after it: not new-volume
	evs := series(t, "n", t0, 29, 2*time.Minute, nil, 1, 2) // last at 56 min
	evs = append(evs, relayEvent(t, "late", t0.Add(time.Hour), 1, 2))
	cs := run(t, newDet(t, historyThen(nsRule(30))), evs)
	if len(cs) != 0 {
		t.Fatalf("an event at exactly start+Window is outside [start, start+Window):\n%s", describe(cs))
	}
}

func TestNewStreamReactivationAfterSilenceAndNoRepeats(t *testing.T) {
	r := nsRule(5)
	var evs []Event
	evs = append(evs, series(t, "a", t0, 10, time.Minute, nil, 1, 2)...) // fires once at #5
	evs = append(evs, series(t, "b", t0.Add(13*time.Hour), 10, time.Minute, nil, 1, 2)...)
	cs := run(t, newDet(t, historyThen(r)), evs)
	act := filter(cs, "ns", StateActive)
	if len(act) != 2 {
		t.Fatalf("one activation per episode expected:\n%s", describe(cs))
	}
	if act[0].NewStream.Reactivated || !act[1].NewStream.Reactivated {
		t.Fatalf("reactivation flags: %+v %+v", act[0].NewStream, act[1].NewStream)
	}
	if act[0].EventID == act[1].EventID {
		t.Fatal("a reactivation after the cooldown is a new episode with a new EventID")
	}
	// no reactivation for a gap shorter than QuietPeriod
	evs = append(series(t, "a", t0, 10, time.Minute, nil, 1, 2), series(t, "c", t0.Add(6*time.Hour), 10, time.Minute, nil, 1, 2)...)
	cs = run(t, newDet(t, historyThen(r)), evs)
	if len(filter(cs, "ns", StateActive)) != 1 {
		t.Fatalf("gap below QuietPeriod must not reactivate:\n%s", describe(cs))
	}
}

// ---- state machine ----

func TestLegalTransitions(t *testing.T) {
	legal := map[[3]int]bool{
		{int(StateNormal), int(StateSuspicious), int(ReasonThresholdCrossed)}: true,
		{int(StateSuspicious), int(StateActive), int(ReasonConfirmed)}:        true,
		{int(StateSuspicious), int(StateNormal), int(ReasonNotConfirmed)}:     true,
		{int(StateSuspicious), int(StateNormal), int(ReasonEvicted)}:          true,
		{int(StateActive), int(StateEnded), int(ReasonQuiet)}:                 true,
		{int(StateActive), int(StateEnded), int(ReasonEvicted)}:               true,
		{int(StateEnded), int(StateActive), int(ReasonReopened)}:              true,
	}
	for from := StateNormal; from <= StateEnded; from++ {
		for to := StateNormal; to <= StateEnded; to++ {
			for r := Reason(0); r <= ReasonEvicted+1; r++ {
				want := legal[[3]int{int(from), int(to), int(r)}]
				if got := legalTransition(from, to, r); got != want {
					t.Fatalf("%v->%v (%v): legal=%v want %v", from, to, r, got, want)
				}
			}
		}
	}
}

func TestStateMachineFullCycle(t *testing.T) {
	p := StateParams{ConfirmAfter: 2 * time.Minute, EndAfterQuiet: 10 * time.Minute, Cooldown: 30 * time.Minute}
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: p}
	var evs []Event
	add := func(id string, d time.Duration) { evs = append(evs, relayEvent(t, id, at(d), 1, 2)) }
	add("a", 0)                  // suspicious
	add("b", time.Minute)        // still suspicious (1 min < confirm 2 min)
	add("c", 3*time.Minute)      // active
	add("d", 20*time.Minute)     // ended at 13m (quiet), reopened at 20m (within cooldown)
	add("e", 100*time.Minute)    // ended at 30m; cooldown over at 60m; new episode
	add("f", 100*time.Minute+30) // same second-ish, same episode
	d := newDet(t, Config{Rate: []RateRule{r}})
	cs := run(t, d, evs)
	cs = append(cs, d.Advance(at(10*time.Hour)).Candidates...)
	type tr struct {
		from, to State
		reason   Reason
		at       time.Duration
	}
	var got []tr
	for _, c := range cs {
		got = append(got, tr{c.From, c.To, c.Reason, c.At.Sub(t0)})
	}
	want := []tr{
		{StateNormal, StateSuspicious, ReasonThresholdCrossed, 0},
		{StateSuspicious, StateActive, ReasonConfirmed, 3 * time.Minute},
		{StateActive, StateEnded, ReasonQuiet, 13 * time.Minute},
		{StateEnded, StateActive, ReasonReopened, 20 * time.Minute},
		{StateActive, StateEnded, ReasonQuiet, 30 * time.Minute},
		{StateNormal, StateSuspicious, ReasonThresholdCrossed, 100 * time.Minute},
	}
	want = append(want, tr{StateSuspicious, StateNormal, ReasonNotConfirmed, 110*time.Minute + 30})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions:\n%s", describe(cs))
	}
	if cs[0].EventID != cs[3].EventID || cs[3].EventID == cs[5].EventID {
		t.Fatal("reopen within cooldown keeps the EventID; a new episode after cooldown gets a new one")
	}
	if cs[2].Summary == nil || cs[2].Summary.HotSignals != 3 {
		t.Fatalf("ended candidate summary %+v", cs[2].Summary)
	}
	// a quiet end is decided no earlier than its own data time and never
	// before the transitions that opened the episode
	if cs[2].DecidedAt.Before(cs[2].At) || cs[2].DecidedAt.Before(cs[1].DecidedAt) {
		t.Fatalf("quiet end DecidedAt %v (At %v, opened %v)", cs[2].DecidedAt, cs[2].At, cs[1].DecidedAt)
	}
}

func TestSuspiciousWithoutConfirmationReturnsToNormal(t *testing.T) {
	p := StateParams{ConfirmAfter: 5 * time.Minute, EndAfterQuiet: 2 * time.Minute}
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: p}
	cs := run(t, newDet(t, Config{Rate: []RateRule{r}}), []Event{
		relayEvent(t, "a", t0, 1, 2), relayEvent(t, "b", at(10*time.Minute), 1, 2)})
	if len(cs) < 2 || cs[1].From != StateSuspicious || cs[1].To != StateNormal || cs[1].Reason != ReasonNotConfirmed ||
		!cs[1].At.Equal(at(2*time.Minute)) {
		t.Fatalf("unconfirmed episode:\n%s", describe(cs))
	}
}

func TestCooldownSuppressesRepeatedAlerts(t *testing.T) {
	p := StateParams{EndAfterQuiet: time.Minute, Cooldown: time.Hour}
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: p}
	var evs []Event
	for i := 0; i < 5; i++ { // bursts every 10 minutes
		evs = append(evs, relayEvent(t, fmt.Sprint(i), at(time.Duration(i)*10*time.Minute), 1, 2))
	}
	cs := run(t, newDet(t, Config{Rate: []RateRule{r}}), evs)
	if n := len(filter(cs, "r", StateSuspicious)); n != 1 {
		t.Fatalf("cooldown must keep one episode, got %d openings:\n%s", n, describe(cs))
	}
	ids := map[string]bool{}
	for _, c := range cs {
		ids[c.EventID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("one stable EventID expected, got %d", len(ids))
	}
}

func TestEventIDsAreStableAcrossReplays(t *testing.T) {
	evs := series(t, "s", t0, 40, 10*time.Second, nil, 1, 2)
	cfg := Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 5)}}
	a := run(t, newDet(t, cfg), evs)
	b := run(t, newDet(t, cfg), evs)
	if len(a) == 0 || !reflect.DeepEqual(a, b) {
		t.Fatal("identical input and config must give identical candidates")
	}
}

func TestObserveDoesNotAliasCallerData(t *testing.T) {
	e := relayEvent(t, "a", t0, 1, 2)
	hops := e.FullRoute.hops
	d := newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 1)}})
	cs := run(t, d, []Event{e})
	hops[0] ^= 0xFF // caller mutates its own buffer afterwards
	if cs[0].TriggerRoute.hops[0] == hops[0] {
		t.Fatal("candidate aliases the caller's route buffer")
	}
}

func TestEpisodeTimeTransitionsUseTheirOwnDataTime(t *testing.T) {
	// the transition time must not depend on when time was advanced
	p := StateParams{EndAfterQuiet: 10 * time.Minute, Cooldown: 5 * time.Minute}
	var ep episode
	start := t0.UnixNano()
	ep.onHot(start, &p, nil)
	trs := ep.onTime(start+int64(3*time.Hour), &p, nil)
	if len(trs) != 1 || trs[0].reason != ReasonQuiet || trs[0].at != start+int64(10*time.Minute) {
		t.Fatalf("quiet end stamped %v, want lastHot+quiet", trs)
	}
	if ep.state != StateNormal {
		t.Fatalf("cooldown also expired: state %v", ep.state)
	}
}

func TestQuietEndAtExactlyStateTTLIsNotAnEviction(t *testing.T) {
	lim := experimentalLimits()
	lim.StateTTL = 10 * time.Minute
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: StateParams{EndAfterQuiet: 10 * time.Minute}}
	cs := run(t, newDet(t, Config{Limits: lim, Rate: []RateRule{r}}),
		[]Event{relayEvent(t, "a", t0, 1, 2), relayEvent(t, "b", at(10*time.Minute), 3, 4)})
	e := filter(cs, "r", StateEnded)
	if len(e) == 0 || e[0].Reason != ReasonQuiet {
		t.Fatalf("an episode whose quiet end coincides with StateTTL must end as quiet:\n%s", describe(cs))
	}
}

func TestNewStreamSignalsOncePerEpisode(t *testing.T) {
	// 20 events a minute apart: the rule is hot once (at the 5th); later
	// events in the same episode must not keep the episode alive
	evs := series(t, "n", t0, 20, time.Minute, nil, 1, 2)
	d := newDet(t, historyThen(nsRule(5)))
	cs := run(t, d, evs)
	cs = append(cs, d.Advance(at(3*time.Hour)).Candidates...)
	e := filter(cs, "ns", StateEnded)
	if len(e) != 1 || !e[0].At.Equal(evs[4].Time.Add(10*time.Minute)) || e[0].Summary.HotSignals != 1 {
		t.Fatalf("new-stream episode must end quiet after its single signal:\n%s", describe(cs))
	}
}
