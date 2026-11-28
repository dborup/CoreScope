package anomaly

import (
	"testing"
	"time"
)

// A new-stream episode opened from left-censored evidence is reported with
// ConfidenceLow ("never as a certain new stream", newstream.go). Every later
// transition of that episode, including those without a trigger event (quiet
// end, eviction end) and a reopen by an uncensored signal within the
// cooldown, keeps that confidence.

// episodes groups the candidates of rule by episode, in emission order.
func episodes(cs []Candidate, rule string) (ids []string, by map[string][]Candidate) {
	by = map[string][]Candidate{}
	for _, c := range cs {
		if c.Rule != rule {
			continue
		}
		if _, ok := by[c.EventID]; !ok {
			ids = append(ids, c.EventID)
		}
		by[c.EventID] = append(by[c.EventID], c)
	}
	return ids, by
}

// censoredEpisode returns the only episode of rule "ns", after checking that
// it was opened from censored evidence with ConfidenceLow and that it
// contains a transition to `to` for reason `reason`.
func censoredEpisode(t *testing.T, cs []Candidate, to State, reason Reason) []Candidate {
	t.Helper()
	ids, by := episodes(cs, "ns")
	if len(ids) != 1 {
		t.Fatalf("fixture: %d new-stream episodes, want 1:\n%s", len(ids), describe(cs))
	}
	ep := by[ids[0]]
	if open := ep[0]; open.NewStream == nil || !open.NewStream.Censored || open.Confidence != ConfidenceLow {
		t.Fatalf("fixture: episode opened %s->%s with confidence %v, evidence %+v; want a censored, low-confidence open",
			open.From, open.To, open.Confidence, open.NewStream)
	}
	for _, c := range ep {
		if c.To == to && c.Reason == reason {
			return ep
		}
	}
	t.Fatalf("fixture: episode has no ->%v (%v) transition:\n%s", to, reason, describe(ep))
	return nil
}

func assertAllLow(t *testing.T, ep []Candidate) {
	t.Helper()
	for _, c := range ep {
		if c.Confidence != ConfidenceLow {
			t.Errorf("%s->%s (%s) at %v carries confidence %v, want low (the episode opened from censored evidence)",
				c.From, c.To, c.Reason, c.At.Sub(t0), c.Confidence)
		}
	}
}

// censoredStream is a stream that starts one minute after the history start
// (inside CensorWindow) and reaches MinCount 30 after 14m30s.
func censoredStream(t *testing.T, prefix string, start time.Time) []Event {
	return series(t, prefix, start, 30, 30*time.Second, nil, 0x11, 0x22)
}

func TestCensoredNewStreamKeepsLowConfidenceOnQuietEnd(t *testing.T) {
	d := newDet(t, Config{NewStream: []NewStreamRule{nsRule(30)}, HistoryStart: t0})
	cs := run(t, d, censoredStream(t, "n", t0.Add(time.Minute)))
	cs = append(cs, d.Advance(t0.Add(3*time.Hour)).Candidates...)
	assertAllLow(t, censoredEpisode(t, cs, StateEnded, ReasonQuiet))
}

func TestCensoredNewStreamKeepsLowConfidenceOnEvictionEnd(t *testing.T) {
	cfg := Config{NewStream: []NewStreamRule{nsRule(30)}, HistoryStart: t0, Limits: experimentalLimits()}
	cfg.Limits.MaxKeysPerScope = 1
	evs := append(censoredStream(t, "n", t0.Add(time.Minute)), relayEvent(t, "other", t0.Add(16*time.Minute), 0x33, 0x44))
	cs := run(t, newDet(t, cfg), evs)
	assertAllLow(t, censoredEpisode(t, cs, StateEnded, ReasonEvicted))
}

// The stream goes quiet for more than QuietPeriod and comes back: the new
// stream state is reactivated (never censored) and fires again within the
// cooldown, which reopens the censored episode.
func TestCensoredNewStreamKeepsLowConfidenceOnReopen(t *testing.T) {
	r := nsRule(30)
	r.State.Cooldown = 24 * time.Hour
	d := newDet(t, Config{NewStream: []NewStreamRule{r}, HistoryStart: t0})
	evs := append(censoredStream(t, "a", t0.Add(time.Minute)), censoredStream(t, "b", t0.Add(13*time.Hour))...)
	cs := run(t, d, evs)
	cs = append(cs, d.Advance(t0.Add(15*time.Hour)).Candidates...)
	ep := censoredEpisode(t, cs, StateActive, ReasonReopened)
	for _, c := range ep {
		if c.Reason == ReasonReopened && (c.NewStream == nil || c.NewStream.Censored) {
			t.Fatalf("fixture: the reopening signal must be uncensored, got %+v", c.NewStream)
		}
	}
	assertAllLow(t, ep)
}

// Controls: an uncensored stream is never lowered, and the low confidence
// does not outlive its episode (after the cooldown, a reactivated stream
// opens a new episode with the key's confidence).
func TestNewStreamConfidenceIsNotLoweredWithoutCensoring(t *testing.T) {
	t.Run("uncensored stream", func(t *testing.T) {
		d := newDet(t, historyThen(nsRule(30)))
		cs := run(t, d, series(t, "n", t0, 30, 30*time.Second, nil, 0x11, 0x22))
		cs = append(cs, d.Advance(t0.Add(3*time.Hour)).Candidates...)
		ids, by := episodes(cs, "ns")
		if len(ids) != 1 || len(by[ids[0]]) < 3 {
			t.Fatalf("fixture: want one episode with an end:\n%s", describe(cs))
		}
		for _, c := range by[ids[0]] {
			if c.Confidence != ConfidenceRouteObserved {
				t.Errorf("%s->%s (%s) carries %v, want route_observed", c.From, c.To, c.Reason, c.Confidence)
			}
		}
	})
	t.Run("new episode after the cooldown", func(t *testing.T) {
		d := newDet(t, Config{NewStream: []NewStreamRule{nsRule(30)}, HistoryStart: t0})
		evs := append(censoredStream(t, "a", t0.Add(time.Minute)), censoredStream(t, "b", t0.Add(13*time.Hour))...)
		cs := run(t, d, evs)
		cs = append(cs, d.Advance(t0.Add(15*time.Hour)).Candidates...)
		ids, by := episodes(cs, "ns")
		if len(ids) != 2 {
			t.Fatalf("fixture: want two episodes:\n%s", describe(cs))
		}
		if first := by[ids[0]][0]; first.Confidence != ConfidenceLow || first.NewStream == nil || !first.NewStream.Censored {
			t.Fatalf("fixture: first episode opened with %v", first.Confidence)
		}
		for _, c := range by[ids[1]] {
			if c.Confidence != ConfidenceRouteObserved {
				t.Errorf("second episode %s->%s (%s) carries %v, want route_observed", c.From, c.To, c.Reason, c.Confidence)
			}
		}
	})
}
