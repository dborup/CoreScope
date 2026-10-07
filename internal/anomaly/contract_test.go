package anomaly

import (
	"math"
	"testing"
	"time"
)

// Regression tests for the documented contracts: decision time, labels on
// every transition, route-group membership, tie order, config validation
// and normalization.

// An event counted in a window that became final later than the trigger
// must hold the decision back: DecidedAt covers every event behind it.
func TestDecidedAtCoversNonTriggerEvidence(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay = time.Minute
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeRouteGroup, time.Minute, 2)}})
	late := relayEvent(t, "a", t0, 1, 0x5c)
	late.FinalAt = at(5 * time.Minute) // its first observation arrived late
	trig := relayEvent(t, "b", at(time.Second), 1, 0x5c)
	// offered in finalization order: b first, a later (within ReorderDelay)
	var cs []Candidate
	cs = append(cs, d.Observe(trig).Candidates...)
	cs = append(cs, d.Observe(late).Candidates...)
	cs = append(cs, d.Flush().Candidates...)
	a := filter(cs, "r", StateActive)
	if len(a) != 1 || a[0].TriggerID != "b" {
		t.Fatalf("want one activation triggered by b:\n%s", describe(cs))
	}
	if a[0].DecidedAt.Before(late.FinalAt) {
		t.Fatalf("DecidedAt %v is before the FinalAt %v of evidence it used", a[0].DecidedAt.Sub(t0), late.FinalAt.Sub(t0))
	}
}

// A time-driven end is never decided before the transitions that opened the
// episode, and never before deadline + ReorderDelay (the earliest time an
// Advance could release it).
func TestQuietEndIsNotDecidedBeforeItsEpisodeOrItsDeadline(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay = 20 * time.Second
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: StateParams{EndAfterQuiet: 10 * time.Second}}
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{r}})
	e := relayEvent(t, "a", t0, 1, 2)
	e.FinalAt = at(60 * time.Second)
	var cs []Candidate
	cs = append(cs, d.Observe(e).Candidates...)
	cs = append(cs, d.Advance(at(2*time.Hour)).Candidates...)
	if len(cs) != 3 || cs[2].To != StateEnded {
		t.Fatalf("want open + active + ended:\n%s", describe(cs))
	}
	end := cs[2]
	if !end.At.Equal(at(10 * time.Second)) {
		t.Fatalf("quiet end At %v", end.At.Sub(t0))
	}
	if end.DecidedAt.Before(cs[1].DecidedAt) || end.DecidedAt.Before(end.At.Add(lim.ReorderDelay)) {
		t.Fatalf("quiet end DecidedAt %v, opened %v", end.DecidedAt.Sub(t0), cs[1].DecidedAt.Sub(t0))
	}
	// Advance models continuously passing time: not stamped with the final t
	if !end.DecidedAt.Equal(at(60 * time.Second)) {
		t.Fatalf("quiet end DecidedAt %v, want max(FinalAt 60s, deadline+delay 30s)", end.DecidedAt.Sub(t0))
	}
	// without a late FinalAt the deadline + ReorderDelay is what counts
	d = newDet(t, Config{Limits: lim, Rate: []RateRule{r}})
	d.Observe(relayEvent(t, "a", t0, 1, 2))
	ends := filter(d.Advance(at(2*time.Hour)).Candidates, "r", StateEnded)
	if len(ends) != 1 || !ends[0].DecidedAt.Equal(at(30*time.Second)) {
		t.Fatalf("quiet end %+v, want DecidedAt = deadline 10s + ReorderDelay 20s", ends)
	}
}

func TestSuppressModeMarksEveryTransitionOfAnExpectedEpisode(t *testing.T) {
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 2,
		State: StateParams{ConfirmAfter: time.Minute, EndAfterQuiet: 5 * time.Minute}}
	check := func(name string, cs []Candidate, wantTransitions int) {
		t.Helper()
		if len(cs) < wantTransitions {
			t.Fatalf("%s: too few transitions:\n%s", name, describe(cs))
		}
		for _, c := range cs {
			if !c.Suppressed || c.Traffic != LabelExpected {
				t.Fatalf("%s: %s->%s (%s) Suppressed=%v Traffic=%v", name, c.From, c.To, c.Reason, c.Suppressed, c.Traffic)
			}
		}
	}
	expected := func(evs []Event) []Event {
		for i := range evs {
			evs[i].Traffic = TrafficExpected
		}
		return evs
	}
	// opened, confirmed, ended by quiet
	d := newDet(t, Config{Rate: []RateRule{r}, Expected: ExpectedPolicy{Mode: ExpectedSuppress}})
	cs := run(t, d, expected(series(t, "e", t0, 12, 10*time.Second, nil, 1, 2)))
	cs = append(cs, d.Advance(at(time.Hour)).Candidates...)
	check("quiet", cs, 3)
	// opened, not confirmed
	d = newDet(t, Config{Rate: []RateRule{r}, Expected: ExpectedPolicy{Mode: ExpectedSuppress}})
	cs = run(t, d, expected(series(t, "n", t0, 2, 10*time.Second, nil, 1, 2)))
	cs = append(cs, d.Advance(at(time.Hour)).Candidates...)
	check("not_confirmed", cs, 2)
	// opened, evicted for capacity: also flagged as a key eviction
	lim := experimentalLimits()
	lim.MaxKeysPerScope = 1
	d = newDet(t, Config{Limits: lim, Rate: []RateRule{r}, Expected: ExpectedPolicy{Mode: ExpectedSuppress}})
	cs = run(t, d, expected(append(series(t, "x", t0, 2, time.Second, nil, 1, 2), relayEvent(t, "other", at(3*time.Second), 1, 3))))
	var ev []Candidate
	for _, c := range cs {
		if c.Reason == ReasonEvicted {
			ev = append(ev, c)
		}
	}
	if len(ev) != 1 || ev[0].Coverage&CoverageKeyEvictions == 0 {
		t.Fatalf("capacity eviction must carry key_evictions:\n%s", describe(cs))
	}
	check("evicted", cs, 2)
}

func TestTimeDrivenTransitionsKeepTheEpisodeCoverage(t *testing.T) {
	lim := experimentalLimits()
	lim.MaxDedupIDs = 1 // every new ID evicts the previous one early
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 2, State: StateParams{EndAfterQuiet: 5 * time.Minute}}
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{r}})
	cs := run(t, d, series(t, "c", t0, 3, 10*time.Second, nil, 1, 2))
	cs = append(cs, d.Advance(at(time.Hour)).Candidates...)
	end := filter(cs, "r", StateEnded)
	if len(end) != 1 || end[0].Coverage&CoverageDedupEvictedEarly == 0 {
		t.Fatalf("ended candidate lost the episode's coverage flags: %+v", end)
	}
}

func TestMixedAndNonFloodRelayEventsNeverFormARouteGroup(t *testing.T) {
	e, _, err := NormalizeTransmission([]Observation{
		obs(t, "tx", "a", t0, RouteClassFlood, 1, 0x11),
		obs(t, "tx", "b", at(time.Second), RouteClassTransportDirect, 0),
	}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if e.RouteClass != RouteClassMixed || e.RouteKind != RouteKindRelay {
		t.Fatalf("fixture: class %v kind %v", e.RouteClass, e.RouteKind)
	}
	if _, ok := RouteGroupKeyOf(e); ok {
		t.Fatal("a mixed transmission formed a route group")
	}
	cs := run(t, newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeRouteGroup, time.Minute, 1)}}), []Event{e})
	if len(cs) != 0 {
		t.Fatalf("route-group candidate from a mixed transmission:\n%s", describe(cs))
	}
	bad := relayEvent(t, "d", t0, 1, 0x5c)
	for _, rc := range []RouteClass{RouteClassDirect, RouteClassTransportDirect, RouteClassUnknown} {
		bad.RouteClass = rc
		if err := bad.Validate(); err == nil {
			t.Fatalf("relay kind with route class %v accepted", rc)
		}
	}
}

// Ties at the frontier are ordered as if every event had waited in the
// reorder buffer, whatever Advance calls came in between.
func TestTieOrderDoesNotDependOnAdvanceCalls(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay = 10 * time.Second
	cfg := Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 2)}}
	x, y := relayEvent(t, "b", t0, 1, 2), relayEvent(t, "a", t0, 1, 2)

	buffered := run(t, newDet(t, cfg), []Event{x, y})
	d := newDet(t, cfg)
	var cs []Candidate
	cs = append(cs, d.Observe(x).Candidates...)
	cs = append(cs, d.Advance(at(10*time.Second)).Candidates...)
	r := d.Observe(y)
	cs = append(cs, r.Candidates...)
	cs = append(cs, d.Flush().Candidates...)
	if r.Status != StatusLate {
		// accepting y after x was processed would reorder the tie
		t.Fatalf("tied event with a smaller ID after its tie was processed: %v", r.Status)
	}
	if len(buffered) == 0 || buffered[len(buffered)-1].TriggerID != "b" {
		t.Fatalf("buffered run:\n%s", describe(buffered))
	}
	// a tied event that sorts after the processed one is still accepted
	d = newDet(t, cfg)
	d.Observe(y)
	d.Advance(at(10 * time.Second))
	if st := d.Observe(x).Status; st != StatusAccepted {
		t.Fatalf("tied event that sorts later: %v", st)
	}
}

func TestConfigRejectsNewStreamRulesThatCannotWork(t *testing.T) {
	r := nsRule(5)
	r.CensorWindow = r.QuietPeriod - time.Minute
	if _, err := New(Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, NewStream: []NewStreamRule{r}}); err == nil {
		t.Fatal("CensorWindow < QuietPeriod accepted: a replay could call a continuing stream new")
	}
	r = nsRule(5)
	r.State.ConfirmAfter = time.Minute
	if _, err := New(Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, NewStream: []NewStreamRule{r}}); err == nil {
		t.Fatal("new-stream rule with ConfirmAfter accepted: it could never be confirmed")
	}
}

// A stream that was active shortly before the history start and reappears
// after the censor window is not reported as certainly new.
func TestNewStreamCensoringCoversTheQuietPeriod(t *testing.T) {
	histStart := t0
	evs := series(t, "old", histStart.Add(nsRule(30).CensorWindow-time.Minute), 30, 30*time.Second, nil, 0x11, 0x22)
	cs := run(t, newDet(t, Config{NewStream: []NewStreamRule{nsRule(30)}, HistoryStart: histStart}), evs)
	for _, c := range filter(cs, "ns", StateActive) {
		if !c.NewStream.Censored || c.Confidence != ConfidenceLow {
			t.Fatalf("stream within QuietPeriod of the history start reported as certainly new: %+v", c.NewStream)
		}
	}
}

func TestEpochZeroIsNotAValidTime(t *testing.T) {
	e := relayEvent(t, "z", time.Unix(0, 0), 1, 2)
	if err := e.Validate(); err == nil {
		t.Fatal("Unix epoch accepted as an event time")
	}
	e.Time = time.Unix(0, 1)
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizerPayloadConflictsAreCheckedAgainstEveryKnownSize(t *testing.T) {
	a := obs(t, "tx", "a", t0, RouteClassFlood, 1, 0x11)
	b := obs(t, "tx", "b", at(time.Second), RouteClassFlood, 1, 0x12)
	b.PayloadKnown, b.PayloadBytes = true, 10
	c := obs(t, "tx", "c", at(2*time.Second), RouteClassFlood, 1, 0x13)
	c.PayloadKnown, c.PayloadBytes = true, 20
	n, _ := NewNormalizer(normCfg())
	n.Add(a)
	n.Add(b)
	if st := n.Add(c).Status; st != ObsConflict {
		t.Fatalf("size 20 after a held size 10: %v", st)
	}
	if _, _, err := NormalizeTransmission([]Observation{a, b, c}, 30*time.Second); err == nil {
		t.Fatal("batch merged conflicting payload sizes")
	}
}

// The batch path excludes a late observation before any conflict check,
// exactly like the streaming Normalizer.
func TestBatchExcludesLateConflictsLikeStreaming(t *testing.T) {
	a := obs(t, "tx", "a", t0, RouteClassFlood, 1, 0x11)
	a.PayloadKnown, a.PayloadBytes = true, 30
	b := obs(t, "tx", "b", t0, RouteClassFlood, 1, 0x12)
	b.ArrivedAt = at(2 * time.Minute)
	b.PayloadKnown, b.PayloadBytes = true, 31
	e, rep, err := NormalizeTransmission([]Observation{a, b}, time.Minute)
	if err != nil || rep.ExcludedLate != 1 || rep.Included != 1 || e.PayloadBytes != 30 {
		t.Fatalf("batch: %v %+v %+v", err, rep, e)
	}
	n, _ := NewNormalizer(NormalizerConfig{SettleWindow: time.Minute, MaxPendingTransmissions: 10,
		MaxObservationsPerTransmission: 10, FinalizedHorizon: time.Hour, MaxFinalizedIDs: 10})
	n.Add(a)
	if st := n.Add(b).Status; st != ObsLate {
		t.Fatalf("streaming: %v", st)
	}
}

func TestNormalizerTruncationReachesTheEventAndTheCandidates(t *testing.T) {
	cfg := normCfg()
	cfg.MaxObservationsPerTransmission = 1
	n, _ := NewNormalizer(cfg)
	n.Add(obs(t, "tx", "a", at(2*time.Second), RouteClassFlood, 1, 0x11))
	b := obs(t, "tx", "b", t0, RouteClassFlood, 1, 0x22) // earlier data time, arrives later
	b.ArrivedAt = at(3 * time.Second)
	if st := n.Add(b).Status; st != ObsDroppedFanout {
		t.Fatalf("fixture: %v", st)
	}
	evs := n.Flush().Events
	if len(evs) != 1 || !evs[0].Truncated || evs[0].ExcludedObservations != 1 {
		t.Fatalf("truncated event: %+v", evs)
	}
	d := newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 1)}})
	r := d.Observe(evs[0])
	if !r.CoverageReduced || len(r.Candidates) == 0 || r.Candidates[0].Coverage&CoverageInputTruncated == 0 {
		t.Fatalf("candidate on truncated input not flagged: %+v", r)
	}
	if s := d.Snapshot(); s.TruncatedInputs != 1 || !s.CoverageReduced() {
		t.Fatalf("stats: %+v", s)
	}
	// forced early finalization is truncation too
	cfg = normCfg()
	cfg.MaxPendingTransmissions = 1
	n, _ = NewNormalizer(cfg)
	n.Add(obs(t, "t1", "a", t0, RouteClassFlood, 1, 0x11))
	res := n.Add(obs(t, "t2", "a", at(time.Second), RouteClassFlood, 1, 0x11))
	if len(res.Events) != 1 || !res.Events[0].Truncated {
		t.Fatalf("forced finalization: %+v", res.Events)
	}
}

func TestNormalizerRejectsUnboundedDurations(t *testing.T) {
	c := normCfg()
	c.SettleWindow, c.FinalizedHorizon = math.MaxInt64, math.MaxInt64
	if _, err := NewNormalizer(c); err == nil {
		t.Fatal("overflowing SettleWindow accepted")
	}
	c = normCfg()
	c.SettleWindow, c.FinalizedHorizon = 25*time.Hour, 25*time.Hour
	if _, err := NewNormalizer(c); err == nil {
		t.Fatal("SettleWindow above 24h accepted")
	}
	c = normCfg()
	c.FinalizedHorizon = 365 * 24 * time.Hour
	if _, err := NewNormalizer(c); err == nil {
		t.Fatal("FinalizedHorizon of a year accepted")
	}
}

func TestAdvanceOutsideTheValidRangeIsCounted(t *testing.T) {
	d := newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 1)}})
	d.Advance(time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC))
	if d.Snapshot().InvalidAdvance != 1 {
		t.Fatal("invalid Advance not counted")
	}
}

// Flush (end of input, as used by Replay) stamps released items no earlier
// than an Advance could have released them.
func TestFlushDecidesNoEarlierThanALiveDetector(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay = 20 * time.Second
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: StateParams{EndAfterQuiet: 10 * time.Second}}
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{r}})
	d.Observe(relayEvent(t, "a", t0, 1, 2))
	d.Observe(relayEvent(t, "b", at(15*time.Second), 1, 3))
	cs := d.Flush().Candidates
	var endX *Candidate
	for i := range cs {
		c := &cs[i]
		if c.DecidedAt.Before(c.At.Add(lim.ReorderDelay)) {
			t.Fatalf("%s->%s at %v decided at %v, before At+ReorderDelay", c.From, c.To, c.At.Sub(t0), c.DecidedAt.Sub(t0))
		}
		if c.To == StateEnded {
			endX = c
		}
	}
	if endX == nil || !endX.DecidedAt.Equal(at(30*time.Second)) {
		t.Fatalf("quiet end %+v, want DecidedAt = deadline 10s + ReorderDelay 20s", endX)
	}
}

// With ReorderDelay 0 every event is processed on arrival, so same-time
// events keep arrival order and none is rejected.
func TestZeroReorderDelayKeepsTiesInArrivalOrder(t *testing.T) {
	d := newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 2)}})
	if st := d.Observe(relayEvent(t, "b", t0, 1, 2)).Status; st != StatusAccepted {
		t.Fatal(st)
	}
	if st := d.Observe(relayEvent(t, "a", t0, 1, 2)).Status; st != StatusAccepted {
		t.Fatalf("same-time event with a smaller ID rejected with ReorderDelay 0: %v", st)
	}
}

func TestObservationPayloadMustFitEveryFrame(t *testing.T) {
	o := obs(t, "tx", "a", t0, RouteClassFlood, 1, 0x11)
	o.WireBytes, o.PayloadKnown, o.PayloadBytes = 10, true, 100
	if o.Validate() == nil {
		t.Fatal("payload larger than its own frame accepted")
	}
	// the small frame is not the first observation, so only the held-frame
	// check can see the mismatch
	first := obs(t, "tx", "u", t0, RouteClassFlood, 1, 0x10)
	first.WireBytes = 0
	small := obs(t, "tx", "s", t0, RouteClassFlood, 1, 0x11)
	small.WireBytes = 20
	big := obs(t, "tx", "b", at(time.Second), RouteClassFlood, 1, 0x12)
	big.WireBytes, big.PayloadKnown, big.PayloadBytes = 120, true, 100
	n, _ := NewNormalizer(normCfg())
	n.Add(first)
	n.Add(small)
	if st := n.Add(big).Status; st != ObsConflict {
		t.Fatalf("payload that cannot fit a held frame: %v", st)
	}
	for _, e := range n.Flush().Events {
		if err := e.Validate(); err != nil {
			t.Fatalf("Normalizer emitted an invalid event: %v", err)
		}
	}
}

func TestParseKeyAppliesTheKindClassContract(t *testing.T) {
	if _, err := ParseKey("v1|s=stream|pt=5|ch=1:ab|rc=direct|rk=relay|hop=1:5c"); err == nil {
		t.Fatal("stream key with relay kind on a direct class accepted")
	}
}

// Coverage flags look back from the newest capacity event: an older event
// accepted out of order must not move that point backwards.
func TestCoverageFlagTimesOnlyMoveForward(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay, lim.DedupHorizon = time.Hour, 2*time.Hour
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 1)}})
	a := relayEvent(t, "a", at(59*time.Minute), 1, 2)
	a.Truncated = true
	b := relayEvent(t, "b", t0, 1, 3)
	b.Truncated = true
	d.Observe(a)
	d.Observe(b) // accepted: within ReorderDelay
	c := relayEvent(t, "c", at(150*time.Minute), 1, 4)
	d.Observe(c)
	var got []Candidate
	for r := d.Flush(); ; r = d.Drain() {
		got = append(got, r.Candidates...)
		if !r.More {
			break
		}
	}
	for _, x := range got {
		if x.TriggerID == "c" && x.Coverage&CoverageInputTruncated == 0 {
			t.Fatal("candidate 91 minutes after truncated input lost CoverageInputTruncated")
		}
	}
}

// A time-driven end run early by a forced reorder release is flagged.
func TestForcedReleaseFlagsTimeDrivenEnds(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay, lim.MaxReorderBuffer = 10*time.Second, 2
	r := RateRule{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: StateParams{EndAfterQuiet: 5 * time.Second}}
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{r}})
	var cs []Candidate
	for _, e := range []Event{
		relayEvent(t, "a", at(100*time.Second), 1, 2), // released normally by d
		relayEvent(t, "d", at(111*time.Second), 1, 3),
		relayEvent(t, "e", at(112*time.Second), 1, 4),
		relayEvent(t, "f", at(113*time.Second), 1, 5), // overflow: forces d out, a's end runs first
	} {
		cs = append(cs, d.Observe(e).Candidates...)
	}
	for _, c := range cs {
		if c.To == StateEnded && c.Key.FirstHop == mustHop(t, 2) {
			if c.Coverage&CoverageReorderForced == 0 {
				t.Fatalf("early end without reorder_forced: %+v", c)
			}
			return
		}
	}
	t.Fatalf("no end for a:\n%s", describe(cs))
}
