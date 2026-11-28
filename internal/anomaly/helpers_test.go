package anomaly

import (
	"fmt"
	"testing"
	"time"
)

// All fixtures are synthetic: an arbitrary base time in 2030, made-up
// channel and hop bytes, and thresholds that are named experimental test
// values, not recommendations.

var t0 = time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func mustHop(t testing.TB, b ...byte) Hop {
	t.Helper()
	h, err := NewHop(b)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustChan(t testing.TB, b ...byte) ChannelHash {
	t.Helper()
	c, err := NewChannelHash(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustRoute(t testing.TB, w int, b ...byte) Route {
	t.Helper()
	r, err := NewRoute(w, b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// relayEvent builds a final flood event on channel ch via first hop hop.
func relayEvent(t testing.TB, id string, when time.Time, ch byte, hop ...byte) Event {
	t.Helper()
	return Event{ID: TxID(id), Time: when, PayloadType: 5, Channel: mustChan(t, ch),
		RouteClass: RouteClassFlood, RouteKind: RouteKindRelay, FirstHop: mustHop(t, hop...),
		FullRoute: mustRoute(t, len(hop), append(append([]byte(nil), hop...), make([]byte, len(hop))...)...),
		WireBytes: 60}
}

// experimentalLimits are generous test limits (not recommendations).
func experimentalLimits() Limits {
	return Limits{
		MaxKeysPerScope:      10_000,
		StateTTL:             48 * time.Hour,
		DedupHorizon:         6 * time.Hour,
		MaxDedupIDs:          1_000_000,
		ReorderDelay:         0,
		MaxReorderBuffer:     1024,
		MaxWindowEntries:     100_000,
		MaxCandidatesPerCall: 100_000,
		MaxPendingCandidates: 1_000_000,
	}
}

func quickState() StateParams {
	return StateParams{ConfirmAfter: 0, EndAfterQuiet: 10 * time.Minute, Cooldown: 0}
}

func rateRule(name string, s Scope, w time.Duration, threshold int) RateRule {
	return RateRule{Name: name, Scope: s, Window: w, Threshold: threshold, State: quickState()}
}

func newDet(t testing.TB, cfg Config) *Detector {
	t.Helper()
	if cfg.Limits == (Limits{}) {
		cfg.Limits = experimentalLimits()
	}
	if cfg.Expected.Mode == 0 {
		cfg.Expected.Mode = ExpectedInclude
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// run feeds events in order, then flushes, and returns every candidate.
func run(t testing.TB, d *Detector, evs []Event) []Candidate {
	t.Helper()
	var out []Candidate
	for i, e := range evs {
		r := d.Observe(e)
		if r.Status != StatusAccepted && r.Status != StatusDuplicate {
			t.Fatalf("event %d: status %v err %v", i, r.Status, r.Err)
		}
		out = append(out, r.Candidates...)
	}
	for {
		r := d.Flush()
		out = append(out, r.Candidates...)
		if !r.More {
			break
		}
	}
	return out
}

func filter(cs []Candidate, rule string, to State) []Candidate {
	var out []Candidate
	for _, c := range cs {
		if c.Rule == rule && c.To == to {
			out = append(out, c)
		}
	}
	return out
}

func describe(cs []Candidate) string {
	s := ""
	for _, c := range cs {
		s += fmt.Sprintf("  %s %s %s->%s %s at=%s\n", c.Rule, c.Key.Scope, c.From, c.To, c.Reason, c.At.Sub(t0))
	}
	return s
}

// series builds n events on one stream at start + i*step (+ jitter[i]).
func series(t testing.TB, prefix string, start time.Time, n int, step time.Duration, jitter func(i int) time.Duration, ch byte, hop ...byte) []Event {
	t.Helper()
	evs := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		j := time.Duration(0)
		if jitter != nil {
			j = jitter(i)
		}
		evs = append(evs, relayEvent(t, fmt.Sprintf("%s-%04d", prefix, i), start.Add(time.Duration(i)*step+j), ch, hop...))
	}
	return evs
}
