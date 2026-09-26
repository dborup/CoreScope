package anomaly

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestIndependentRejectZeroWidthRouteWithBytes(t *testing.T) {
	var rec ReplayRecord
	if err := json.Unmarshal([]byte(replayLine("badroute")), &rec); err != nil {
		t.Fatal(err)
	}
	rec.FullRoute = "0:deadbeef"
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Replay(strings.NewReader(string(line)), replayFixtureConfig(t), ReplayOptions{Start: t0, End: at(time.Hour), MaxLineBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Input.Accepted != 0 || rep.Input.Invalid != 1 {
		t.Fatalf("nonempty route with width zero was accepted: %+v", rep.Input)
	}
}

func TestIndependentRejectBurstGapOverflow(t *testing.T) {
	rule := experimentalPeriodic()
	rule.BurstGap = time.Duration(math.MaxInt64)
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{rule}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("BurstGap=MaxInt64 passed validation despite BurstGap >= MinPeriod/2")
	}
}

func TestIndependentDedupExpiresBeforeRepeatedID(t *testing.T) {
	lim := experimentalLimits()
	lim.DedupHorizon = 2 * time.Minute
	d := newDet(t, Config{Limits: lim, Rate: []RateRule{rateRule("r", ScopeGlobal, time.Minute, 1)}})
	if got := d.Observe(relayEvent(t, "same", t0, 1, 2)).Status; got != StatusAccepted {
		t.Fatal(got)
	}
	if got := d.Observe(relayEvent(t, "same", at(3*time.Minute), 1, 2)).Status; got != StatusAccepted {
		t.Fatalf("ID repeated after 3m with a 2m horizon: got %v, want accepted", got)
	}
}

func TestIndependentHistoryStartsAtFirstProcessedEvent(t *testing.T) {
	lim := experimentalLimits()
	lim.ReorderDelay = 30 * time.Second
	rule := NewStreamRule{Name: "ns", Scope: ScopeStream, Window: time.Minute, MinCount: 1,
		CensorWindow: 10 * time.Second, QuietPeriod: 10 * time.Second, State: quickState()}
	cfg := Config{Limits: lim, NewStream: []NewStreamRule{rule}}
	early := relayEvent(t, "early", t0, 1, 2)
	later := relayEvent(t, "later", at(20*time.Second), 1, 3)
	for _, tc := range []struct {
		name   string
		events []Event
	}{{"ordered", []Event{early, later}}, {"reordered", []Event{later, early}}} {
		t.Run(tc.name, func(t *testing.T) {
			cs := filter(run(t, newDet(t, cfg), tc.events), "ns", StateActive)
			for _, c := range cs {
				if c.TriggerID == later.ID {
					if c.NewStream.Censored {
						t.Fatalf("event 20s after history start is censored despite 10s censor window; confidence=%v", c.Confidence)
					}
					return
				}
			}
			t.Fatal("later stream candidate missing")
		})
	}
}

func TestIndependentConfigCopiesPayloadFilters(t *testing.T) {
	rule := rateRule("r", ScopeGlobal, time.Minute, 1)
	rule.PayloadTypes = []PayloadType{5}
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Rate: []RateRule{rule}}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Rate[0].PayloadTypes[0] = 4
	r := d.Observe(relayEvent(t, "type5", t0, 1, 2))
	if len(filter(r.Candidates, "r", StateActive)) != 1 {
		t.Fatalf("mutating caller's config removed the expected type-5 activation; candidates=%d", len(r.Candidates))
	}
}

func TestIndependentPeriodicAlternatingAllowedJitter(t *testing.T) {
	rule := experimentalPeriodic()
	rule.JitterAbs, rule.JitterRel, rule.MaxJitterFraction = 6*time.Second, 0, 1
	cfg := Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude}, Periodic: []PeriodicRule{rule}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p := newPeriodicState(rule.HistoryLen)
	var scratch periodicScratch
	var events []Event
	for i := 0; i < 32; i++ {
		when := time.Duration(i) * 5 * time.Minute
		if i%2 == 1 {
			when -= 6 * time.Second
		}
		events = append(events, relayEvent(t, fmt.Sprintf("pulse-%02d", i), at(when), 1, 2))
		p.observe(at(when).UnixNano(), TrafficUnclassified, &rule, &scratch)
	}
	ideal := p.chainFor(int64(5*time.Minute), &rule, &scratch)
	chance := p.chance(ideal, &rule)
	if !ideal.meets(&rule) || chance > rule.MaxChance {
		t.Fatalf("invalid repro: ideal=%+v chance=%g", ideal, chance)
	}
	cs := run(t, newDet(t, cfg), events)
	if len(filter(cs, rule.Name, StateActive)) == 0 {
		t.Fatalf("32 valid pulses were not detected: period=5m, gaps=294s/306s, tolerance=6s, coverage=%g, chance=%g", ideal.coverage, chance)
	}
}
