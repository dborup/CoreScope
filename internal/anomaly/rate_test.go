package anomaly

import (
	"fmt"
	"testing"
	"time"
)

func TestDedupSameIDCountsOnce(t *testing.T) {
	d := newDet(t, Config{Rate: []RateRule{rateRule("r2", ScopeGlobal, time.Minute, 2)}})
	e := relayEvent(t, "same", t0, 0x11, 0x22)
	if r := d.Observe(e); r.Status != StatusAccepted {
		t.Fatal(r.Status)
	}
	e2 := e
	e2.FullRoute = mustRoute(t, 1, 0x22, 0x77) // another observation's route: still the same transmission
	if r := d.Observe(e2); r.Status != StatusDuplicate {
		t.Fatalf("second copy of one transmission: %v", r.Status)
	}
	if cs := run(t, d, nil); len(cs) != 0 {
		t.Fatalf("one transmission counted twice:\n%s", describe(cs))
	}
	if s := d.Snapshot(); s.Duplicates != 1 || s.Accepted != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestReplayOfSameBatchDoesNotDoubleCount(t *testing.T) {
	evs := series(t, "b", t0, 5, 10*time.Second, nil, 0x11, 0x22)
	d := newDet(t, Config{Rate: []RateRule{rateRule("r6", ScopeStream, time.Minute, 6)}})
	cs := run(t, d, evs)
	// replaying the same batch: all duplicates, still 5 < 6
	for _, e := range evs {
		r := d.Observe(e)
		if r.Status != StatusDuplicate {
			t.Fatalf("replayed event: %v", r.Status)
		}
		cs = append(cs, r.Candidates...)
	}
	if len(cs) != 0 {
		t.Fatalf("replay double-counted:\n%s", describe(cs))
	}
}

func TestByteIdenticalButDistinctTransmissionsCountTwice(t *testing.T) {
	a := relayEvent(t, "tx-a", t0, 0x11, 0x22)
	b := a
	b.ID = "tx-b" // identical content, different logical transmission
	d := newDet(t, Config{Rate: []RateRule{rateRule("r2", ScopeStream, time.Minute, 2)}})
	cs := run(t, d, []Event{a, b})
	if len(filter(cs, "r2", StateActive)) != 1 {
		t.Fatalf("two logical transmissions must count twice:\n%s", describe(cs))
	}
}

func TestDedupHorizonIsExplicit(t *testing.T) {
	lim := experimentalLimits()
	lim.DedupHorizon = 2 * time.Minute
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeGlobal, time.Minute, 1)}})
	e := relayEvent(t, "x", t0, 0x11, 0x22)
	d.Observe(e)
	d.Observe(relayEvent(t, "later", at(3*time.Minute), 0x11, 0x22))
	again := e
	again.Time = at(3 * time.Minute)
	if r := d.Observe(again); r.Status != StatusAccepted {
		t.Fatalf("an ID repeated after the dedup horizon is counted again (documented): %v", r.Status)
	}
}

func TestDedupCapacityEvictionIsReported(t *testing.T) {
	lim := experimentalLimits()
	lim.MaxDedupIDs = 3
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeGlobal, time.Minute, 1)}})
	var reduced bool
	for i := 0; i < 5; i++ {
		r := d.Observe(relayEvent(t, fmt.Sprint(i), at(time.Duration(i)*time.Second), 0x11, 0x22))
		reduced = reduced || r.CoverageReduced
	}
	s := d.Snapshot()
	if s.DedupIDs != 3 || s.DedupEvictedEarly != 2 || !reduced || !s.CoverageReduced() {
		t.Fatalf("dedup cap: %+v reduced=%v", s, reduced)
	}
	if s.EffectiveDedupHorizon >= lim.DedupHorizon {
		t.Fatalf("effective horizon must show the capacity cut: %v", s.EffectiveDedupHorizon)
	}
	// a repeat of an evicted ID is no longer caught: this is the reported limitation
	if r := d.Observe(relayEvent(t, "0", at(5*time.Second), 0x11, 0x22)); r.Status != StatusAccepted {
		t.Fatalf("evicted ID: %v", r.Status)
	}
}

func TestRateWindowBoundariesAreExact(t *testing.T) {
	for _, w := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
		t.Run(w.String(), func(t *testing.T) {
			// exactly W apart: the old event is outside (t-W, t]
			d := newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, w, 2)}})
			cs := run(t, d, []Event{relayEvent(t, "a", t0, 1, 2), relayEvent(t, "b", t0.Add(w), 1, 2)})
			if len(cs) != 0 {
				t.Fatalf("events exactly %v apart must not share a window:\n%s", w, describe(cs))
			}
			// one nanosecond less: both inside
			d = newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, w, 2)}})
			cs = run(t, d, []Event{relayEvent(t, "a", t0, 1, 2), relayEvent(t, "b", t0.Add(w-time.Nanosecond), 1, 2)})
			if len(filter(cs, "r", StateActive)) != 1 {
				t.Fatalf("events %v-1ns apart must share a window:\n%s", w, describe(cs))
			}
			if got := filter(cs, "r", StateActive)[0].Rate.Count; got != 2 {
				t.Fatalf("count %d", got)
			}
		})
	}
}

func TestSignalHitsOnlyTheMatchingWindows(t *testing.T) {
	rules := []RateRule{
		rateRule("w1m", ScopeStream, time.Minute, 5),
		rateRule("w5m", ScopeStream, 5*time.Minute, 12),
		rateRule("w15m", ScopeStream, 15*time.Minute, 30),
		rateRule("w60m", ScopeStream, time.Hour, 100),
	}
	// 12 events spread 20 s apart: 4 minutes. Max per minute = 3 (<5); per 5 min = 12 (hits);
	// per 15 min = 12 (<30); per 60 min = 12 (<100).
	evs := series(t, "s", t0, 12, 20*time.Second, nil, 0x11, 0x22)
	cs := run(t, newDet(t, Config{Rate: rules}), evs)
	for _, name := range []string{"w1m", "w15m", "w60m"} {
		if len(filter(cs, name, StateSuspicious)) != 0 {
			t.Fatalf("%s fired:\n%s", name, describe(cs))
		}
	}
	a := filter(cs, "w5m", StateActive)
	if len(a) != 1 || !a[0].At.Equal(evs[11].Time) {
		t.Fatalf("w5m must fire exactly at the 12th event:\n%s", describe(cs))
	}
}

func TestSlidingWindowEvictsOldEvents(t *testing.T) {
	// 3 events, then a gap longer than the window, then 3 more: never 4 in (t-W, t]
	w := 5 * time.Minute
	var evs []Event
	evs = append(evs, series(t, "a", t0, 3, time.Minute, nil, 1, 2)...)
	evs = append(evs, series(t, "b", t0.Add(2*time.Minute+w), 3, time.Minute, nil, 1, 2)...)
	cs := run(t, newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, w, 4)}}), evs)
	if len(cs) != 0 {
		t.Fatalf("evicted events counted:\n%s", describe(cs))
	}
	// the 4th event exactly inside the window does fire
	evs = append(series(t, "a", t0, 3, time.Minute, nil, 1, 2), relayEvent(t, "x", t0.Add(w-time.Second), 1, 2))
	cs = run(t, newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, w, 4)}}), evs)
	if len(filter(cs, "r", StateActive)) != 1 {
		t.Fatalf("4 events within %v must fire:\n%s", w, describe(cs))
	}
}

func TestThirtyPerFiveMinutesOnlyAsExplicitConfig(t *testing.T) {
	// The phase-1 example "30 per 5 minutes" is an explicit test config here,
	// never a default.
	rule := rateRule("example_30_per_5m", ScopeStream, 5*time.Minute, 30)
	for _, n := range []int{29, 30} {
		evs := series(t, "e", t0, n, 5*time.Second, nil, 0x11, 0x22)
		cs := run(t, newDet(t, Config{Rate: []RateRule{rule}}), evs)
		got := len(filter(cs, rule.Name, StateActive))
		if (n == 30) != (got == 1) {
			t.Fatalf("%d events: %d activations", n, got)
		}
	}
}

func TestGlobalDetectsActivitySpreadOverStreamsWithoutAttribution(t *testing.T) {
	rules := []RateRule{
		rateRule("per_stream", ScopeStream, 5*time.Minute, 10),
		rateRule("per_route", ScopeRouteGroup, 5*time.Minute, 10),
		rateRule("global", ScopeGlobal, 5*time.Minute, 30),
	}
	var evs []Event
	// 5 channels x 2 first hops x 3 events = 30 events in 5 minutes
	i := 0
	for ch := byte(1); ch <= 5; ch++ {
		for hop := byte(0x60); hop <= 0x61; hop++ {
			for k := 0; k < 3; k++ {
				evs = append(evs, relayEvent(t, fmt.Sprintf("g%03d", i), at(time.Duration(i)*9*time.Second), ch, hop))
				i++
			}
		}
	}
	cs := run(t, newDet(t, Config{Rate: rules}), evs)
	if len(filter(cs, "per_stream", StateSuspicious)) != 0 || len(filter(cs, "per_route", StateSuspicious)) != 2 {
		t.Fatalf("stream must stay quiet, both route groups (15 each) must fire:\n%s", describe(cs))
	}
	g := filter(cs, "global", StateActive)
	if len(g) != 1 {
		t.Fatalf("global must fire once:\n%s", describe(cs))
	}
	k := g[0].Key
	if k.Scope != ScopeGlobal || !k.Channel.IsZero() || !k.FirstHop.IsZero() || g[0].Confidence != ConfidenceAggregate {
		t.Fatalf("global candidate attributed to a stream/route: %s %v", k, g[0].Confidence)
	}
	if g[0].Kind != KindRate || g[0].Rate.Count != 30 {
		t.Fatalf("global evidence %+v", g[0].Rate)
	}
}

func TestPayloadTypeFilterAndScopes(t *testing.T) {
	r := rateRule("only5", ScopeGlobal, time.Minute, 2)
	r.PayloadTypes = []PayloadType{5}
	adv := relayEvent(t, "adv1", t0, 0x11, 0x22)
	adv.PayloadType, adv.Channel = 4, ChannelHash{}
	adv2 := adv
	adv2.ID, adv2.Time = "adv2", at(time.Second)
	cs := run(t, newDet(t, Config{Rate: []RateRule{r}}), []Event{adv, adv2})
	if len(cs) != 0 {
		t.Fatal("payload-type filter ignored")
	}
}

func TestWindowSaturationIsFlaggedAndNeverUndercounts(t *testing.T) {
	lim := experimentalLimits()
	lim.MaxWindowEntries = 4
	rule := rateRule("r", ScopeStream, time.Hour, 20)
	evs := series(t, "s", t0, 20, time.Second, nil, 1, 2) // 20 distinct timestamps
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rule}})
	cs := run(t, d, evs)
	a := filter(cs, "r", StateActive)
	if len(a) != 1 || a[0].Rate.Count != 20 || !a[0].Rate.Saturated || a[0].Coverage&CoverageWindowSaturated == 0 {
		t.Fatalf("saturated window: %+v", a)
	}
	if s := d.Snapshot(); s.WindowMerges == 0 || !s.CoverageReduced() {
		t.Fatalf("merges not counted: %+v", s)
	}
	for _, tb := range d.scopes {
		if tb == nil {
			continue
		}
		for _, k := range tb.m {
			if k.win.live() > lim.MaxWindowEntries {
				t.Fatalf("window holds %d entries > cap", k.win.live())
			}
		}
	}
}

func TestWindowCountsMatchBruteForce(t *testing.T) {
	widths := []int64{int64(time.Minute), int64(5 * time.Minute)}
	w := newWindowSet(len(widths))
	var times []int64
	cur := t0.UnixNano()
	for i := 0; i < 400; i++ {
		cur += int64((i*7919)%90) * int64(time.Second) / 3
		w.add(cur, TrafficUnclassified, widths, 1_000_000)
		times = append(times, cur)
		for wi, width := range widths {
			n := uint64(0)
			for _, x := range times {
				if x > cur-width && x <= cur {
					n++
				}
			}
			if w.sums[wi].n != n {
				t.Fatalf("i=%d width=%d: sliding %d, brute %d", i, width, w.sums[wi].n, n)
			}
		}
	}
}

func TestDecidedAtIsNeverBeforeTheEvidence(t *testing.T) {
	// the triggering event was only final (all route evidence in) 30 s after
	// its data time: the candidate must not claim an earlier decision
	a := relayEvent(t, "a", t0, 1, 2)
	b := relayEvent(t, "b", at(time.Second), 1, 2)
	b.FinalAt = at(31 * time.Second)
	cs := run(t, newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 2)}}), []Event{a, b})
	act := filter(cs, "r", StateActive)
	if len(act) != 1 || !act[0].At.Equal(b.Time) || !act[0].DecidedAt.Equal(b.FinalAt) {
		t.Fatalf("At %v DecidedAt %v, want At=data time and DecidedAt=FinalAt", act[0].At.Sub(t0), act[0].DecidedAt.Sub(t0))
	}
}

func TestSaturatedWindowRecoversAfterMergedEntriesExpire(t *testing.T) {
	// overflow merges keep counts (overstated while inside the window) and
	// must subtract them fully once they expire
	lim := experimentalLimits()
	lim.MaxWindowEntries = 4
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, 10*time.Second, 1000)}})
	var evs []Event
	for i := 0; i < 30; i++ {
		evs = append(evs, relayEvent(t, fmt.Sprint(i), at(time.Duration(i)*time.Second), 1, 2))
	}
	evs = append(evs, relayEvent(t, "after", at(5*time.Minute), 1, 2))
	run(t, d, evs)
	k := d.scopes[ScopeStream].m[StreamKeyOf(evs[0])]
	if got := k.win.sums[0].n; got != 1 {
		t.Fatalf("after every merged entry expired the window must hold exactly 1 event, got %d", got)
	}
	if d.Snapshot().WindowMerges == 0 {
		t.Fatal("fixture should saturate the window")
	}
}
