package anomaly

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestZeroValueConfigIsRejected(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("zero-value Config accepted")
	}
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}}
	if _, err := New(cfg); !errors.Is(err, errNoRules) {
		t.Fatalf("a config without rules must be rejected explicitly, got %v", err)
	}
	cfg.Rate = []RateRule{{Name: "r", Scope: ScopeGlobal, Window: time.Minute}}
	if _, err := New(cfg); err == nil {
		t.Fatal("a rule without threshold/state accepted")
	}
	cfg.Rate = []RateRule{rateRule("r", ScopeGlobal, time.Minute, 5)}
	cfg.Expected.Mode = 0
	if _, err := New(cfg); err == nil {
		t.Fatal("unset Expected.Mode accepted")
	}
	if _, err := NewNormalizer(NormalizerConfig{}); err == nil {
		t.Fatal("zero-value NormalizerConfig accepted")
	}
	// DedupHorizon must cover the longest window
	cfg.Expected.Mode = ExpectedInclude
	cfg.Limits.DedupHorizon = 30 * time.Second
	if _, err := New(cfg); err == nil {
		t.Fatal("dedup horizon shorter than the window accepted")
	}
}

func TestKeyCapacityIsBoundedAndReported(t *testing.T) {
	lim := experimentalLimits()
	lim.MaxKeysPerScope = 50
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 3)}})
	// an attacker mints 1000 unique route keys
	for i := 0; i < 1000; i++ {
		d.Observe(relayEvent(t, fmt.Sprint(i), at(time.Duration(i)*time.Millisecond), byte(i), byte(i>>8), byte(i), 0x01))
		if n := len(d.scopes[ScopeStream].m); n > lim.MaxKeysPerScope {
			t.Fatalf("keys %d > cap", n)
		}
	}
	s := d.Snapshot()
	if s.CapacityEvictions[ScopeStream] != 950 || !s.CoverageReduced() {
		t.Fatalf("capacity evictions not reported: %+v", s.CapacityEvictions)
	}
	// candidates in a scope under capacity pressure carry the flag
	cs := run(t, d, series(t, "hot", at(2*time.Second), 3, time.Second, nil, 0x99, 0x98))
	a := filter(cs, "r", StateActive)
	if len(a) != 1 || a[0].Coverage&CoverageKeyEvictions == 0 {
		t.Fatalf("capacity pressure not flagged: %+v", a)
	}
}

func TestEvictingAnOpenEpisodeClosesItExplicitly(t *testing.T) {
	lim := experimentalLimits()
	lim.MaxKeysPerScope = 1
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 1)}})
	cs := run(t, d, []Event{relayEvent(t, "a", t0, 1, 2), relayEvent(t, "b", at(time.Second), 3, 4)})
	var ev []Candidate
	for _, c := range cs {
		if c.Reason == ReasonEvicted {
			ev = append(ev, c)
		}
	}
	if len(ev) != 1 || ev[0].From != StateActive || ev[0].To != StateEnded || ev[0].Summary == nil {
		t.Fatalf("evicted open episode must emit active->ended(evicted):\n%s", describe(cs))
	}
	if d.Snapshot().EpisodesEvicted != 1 {
		t.Fatal("evicted episodes must be counted")
	}
}

func TestIdleKeysExpireByTTL(t *testing.T) {
	lim := experimentalLimits()
	lim.StateTTL = time.Hour
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 5)}})
	run(t, d, []Event{relayEvent(t, "a", t0, 1, 2), relayEvent(t, "b", at(2*time.Hour), 3, 4)})
	s := d.Snapshot()
	if s.Keys[ScopeStream] != 1 || s.TTLEvictions[ScopeStream] != 1 {
		t.Fatalf("TTL: %+v %+v", s.Keys, s.TTLEvictions)
	}
}

func TestReorderWithinDelayGivesTheSameOutput(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay = 30 * time.Second
	cfg := Config{Limits: lim,
		Rate:     []RateRule{rateRule("r", ScopeStream, 2*time.Minute, 6), rateRule("g", ScopeGlobal, time.Minute, 4)},
		Periodic: []PeriodicRule{experimentalPeriodic()}}
	var evs []Event
	evs = append(evs, series(t, "p", t0, 14, 5*time.Minute, nil, 0x11, 0x22)...)
	for i := 0; i < 40; i++ {
		evs = append(evs, relayEvent(t, fmt.Sprintf("n%02d", i), at(time.Duration(i)*17*time.Second), byte(i%3), 0x33))
	}
	sortEvents(evs)
	// DecidedAt records when the call sequence made a decision possible, so
	// it legitimately depends on arrival order; everything else must not.
	withoutDecidedAt := func(cs []Candidate) []Candidate {
		for i := range cs {
			if cs[i].DecidedAt.Before(cs[i].At) {
				t.Fatal("DecidedAt before At")
			}
			cs[i].DecidedAt = time.Time{}
		}
		return cs
	}
	want := withoutDecidedAt(run(t, newDet(t, cfg), evs))
	if len(want) == 0 {
		t.Fatal("fixture should produce candidates")
	}
	rng := rand.New(rand.NewSource(3))
	for trial := 0; trial < 20; trial++ {
		shuf := append([]Event(nil), evs...)
		// local shuffles that keep every event within 20 s of its sorted slot
		for i := 0; i+1 < len(shuf); i++ {
			if rng.Intn(2) == 0 && shuf[i+1].Time.Sub(shuf[i].Time) < 20*time.Second {
				shuf[i], shuf[i+1] = shuf[i+1], shuf[i]
			}
		}
		got := withoutDecidedAt(run(t, newDet(t, cfg), shuf))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: arrival order within ReorderDelay changed the output", trial)
		}
	}
}

func TestLateEventsAreRejectedNotMisplaced(t *testing.T) {
	d := newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 2)}})
	d.Observe(relayEvent(t, "a", at(time.Minute), 1, 2))
	r := d.Observe(relayEvent(t, "old", t0, 1, 2))
	if r.Status != StatusLate || d.Snapshot().Late != 1 {
		t.Fatalf("event behind the frontier: %v", r.Status)
	}
	if cs := run(t, d, nil); len(cs) != 0 {
		t.Fatal("a late event must not be counted")
	}
}

func TestReorderOverflowIsForcedAndReported(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay = time.Hour
	lim.MaxReorderBuffer = 3
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 50)}})
	var reduced bool
	for i := 0; i < 10; i++ {
		r := d.Observe(relayEvent(t, fmt.Sprint(i), at(time.Duration(i)*time.Second), 1, 2))
		reduced = reduced || r.CoverageReduced
		if d.reorder.Len() > lim.MaxReorderBuffer {
			t.Fatal("reorder buffer exceeded its cap")
		}
	}
	if !reduced || d.Snapshot().ReorderForced != 7 {
		t.Fatalf("forced releases: %d", d.Snapshot().ReorderForced)
	}
}

func TestCandidateBufferIsBoundedAndDrainable(t *testing.T) {
	lim := experimentalLimits()
	lim.MaxCandidatesPerCall = 2
	lim.MaxPendingCandidates = 5
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 1)}})
	var got []Candidate
	for i := 0; i < 4; i++ { // 4 streams x 2 transitions = 8 candidates
		r := d.Observe(relayEvent(t, fmt.Sprint(i), t0, byte(i), 0x01))
		if len(r.Candidates) > lim.MaxCandidatesPerCall {
			t.Fatal("per-call cap exceeded")
		}
		got = append(got, r.Candidates...)
	}
	for {
		r := d.Drain()
		got = append(got, r.Candidates...)
		if !r.More {
			break
		}
	}
	s := d.Snapshot()
	if s.CandidatesEmitted != 8 || int(s.CandidatesDropped)+len(got) != 8 || s.PendingCandidates != 0 {
		t.Fatalf("emitted %d dropped %d returned %d", s.CandidatesEmitted, s.CandidatesDropped, len(got))
	}
	// generous pending cap: nothing is dropped, everything comes out in order
	lim.MaxPendingCandidates = 100
	d = newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 1)}})
	got = got[:0]
	for i := 0; i < 4; i++ {
		got = append(got, d.Observe(relayEvent(t, fmt.Sprint(i), t0, byte(i), 0x01)).Candidates...)
	}
	for r := d.Drain(); ; r = d.Drain() {
		got = append(got, r.Candidates...)
		if !r.More {
			break
		}
	}
	if len(got) != 8 || d.Snapshot().CandidatesDropped != 0 {
		t.Fatalf("drain returned %d", len(got))
	}
}

func TestOutputHoldsOnlyCallerValues(t *testing.T) {
	// marker bytes the caller supplied; nothing else identifying may appear
	e := relayEvent(t, "caller-tx-0001", t0, 0xC3, 0x7A, 0x5B)
	evs := []Event{e}
	for i := 1; i < 3; i++ {
		x := e
		x.ID = TxID(fmt.Sprintf("caller-tx-%04d", i+1))
		x.Time = at(time.Duration(i) * time.Second)
		evs = append(evs, x)
	}
	cs := run(t, newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeStream, time.Minute, 3)}}), evs)
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, forbidden := range []string{"enc_", "#ping", "#wardriving", "sender", "observer", "pubkey"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(forbidden)) {
			t.Fatalf("output contains %q", forbidden)
		}
	}
	// every string value is an enum/rule name, a time, the stream key, the
	// caller's route, a caller ID or a 32-hex EventID
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	callerIDs := map[string]bool{"caller-tx-0001": true, "caller-tx-0002": true, "caller-tx-0003": true}
	eventID := regexp.MustCompile(`^[0-9a-f]{32}$`)
	enums := map[string]bool{"r": true, "rate": true, "stream": true, "normal": true, "suspicious": true, "active": true,
		"ended": true, "threshold_crossed": true, "confirmed": true, "quiet": true, "route_observed": true, "unclassified": true}
	var walk func(x interface{})
	walk = func(x interface{}) {
		switch y := x.(type) {
		case map[string]interface{}:
			for _, z := range y {
				walk(z)
			}
		case []interface{}:
			for _, z := range y {
				walk(z)
			}
		case string:
			if _, err := time.Parse(time.RFC3339Nano, y); err == nil {
				return
			}
			switch {
			case enums[y], callerIDs[y], eventID.MatchString(y),
				y == StreamKeyOf(e).String(), y == "2:7a5b0000":
			default:
				t.Fatalf("unexpected string value %q in output", y)
			}
		}
	}
	walk(v)
}

func TestStaleDeadlinesStayBounded(t *testing.T) {
	lim := experimentalLimits()
	lim.MaxKeysPerScope = 4
	st := StateParams{EndAfterQuiet: 24 * time.Hour}
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{{Name: "r", Scope: ScopeStream, Window: time.Minute, Threshold: 1, State: st}}})
	// every event opens an episode on a new key and evicts an old one, leaving
	// its deadline stale
	for i := 0; i < 5000; i++ {
		d.Observe(relayEvent(t, fmt.Sprint(i), at(time.Duration(i)*time.Second), byte(i), byte(i>>8), byte(i), 0x07))
		for r := d.Drain(); r.More; r = d.Drain() {
		}
		if d.deadlines.Len() > 4*d.liveEpisodes()+1024+1 {
			t.Fatalf("deadline heap %d grows without bound (live %d)", d.deadlines.Len(), d.liveEpisodes())
		}
	}
	if d.Snapshot().EpisodesEvicted < 4000 {
		t.Fatal("fixture should evict open episodes")
	}
}
