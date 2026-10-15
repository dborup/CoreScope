package main

import (
	"strings"
	"testing"
	"time"
)

// Round-2 review of PR #478 (#474): the boundary cases the first round's
// mutants survived, plus the parity and key-sharing properties the
// pre-registration now depends on. Every bot, channel byte and time here is
// synthetic.

// r2Labelling is the labelling recipe of the pre-registration: a 60 s merge
// gap, P in [1 min, 1 h], k in 1..3 and a tolerance of max(30 s, 0.1 P).
func r2Labelling(minPulses int) labellingSpec {
	return labellingSpec{PulseMergeGap: dur(60 * time.Second), MinPeriod: dur(time.Minute),
		MaxPeriod: dur(time.Hour), MinPulses: minPulses, MaxHarmonic: 3,
		ToleranceAbs: dur(30 * time.Second), ToleranceRel: 0.1}
}

// r2Pulses builds one injected event per offset, which is all
// labelIncidents reads.
func r2Pulses(offsets ...time.Duration) []injEvent {
	evs := make([]injEvent, 0, len(offsets))
	for i, d := range offsets {
		evs = append(evs, injEvent{t: injT.Add(d).UnixNano(), pulse: i})
	}
	return evs
}

// grid returns n offsets exactly p apart.
func grid(n int, p time.Duration) []time.Duration {
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, time.Duration(i)*p)
	}
	return out
}

// F5: a run of exactly MinPulses pulses is an incident, and one pulse fewer
// is not. Without the exact-boundary case, labelling with ">" instead of
// ">=" (nine pulses required) passes every other test.
func TestLabelIncidentsAtExactlyMinPulses(t *testing.T) {
	const p = 300 * time.Second
	exact := grid(8, p)
	inc := labelIncidents("bot-a", "7f", r2Pulses(exact...), r2Labelling(8))
	if len(inc) != 1 || inc[0].Pulses != 8 {
		t.Fatalf("exactly 8 pulses must be one 8-pulse incident: %+v", inc)
	}
	if got := labelIncidents("bot-a", "7f", r2Pulses(exact[:7]...), r2Labelling(8)); len(got) != 0 {
		t.Fatalf("7 pulses must not be an incident: %+v", got)
	}
}

// F5: a gap exactly max(tolerance_abs, tolerance_rel*P) away from k*P is
// inside the tolerance. P = 300 s gives a tolerance of exactly 30 s, so a
// 330 s gap is the boundary and a 331 s gap is outside it.
func TestLabelIncidentsAtTheToleranceBoundary(t *testing.T) {
	const p = 300 * time.Second
	at := func(last time.Duration) []time.Duration {
		return append(grid(7, p), 6*p+last)
	}
	inc := labelIncidents("bot-a", "7f", r2Pulses(at(330*time.Second)...), r2Labelling(8))
	if len(inc) != 1 || inc[0].Pulses != 8 {
		t.Fatalf("a gap exactly at the tolerance must stay in the run: %+v", inc)
	}
	// One nanosecond further out and the run breaks before the last pulse,
	// which leaves 7 pulses: below min_pulses, so no incident at all.
	if got := labelIncidents("bot-a", "7f", r2Pulses(at(330*time.Second+1)...), r2Labelling(8)); len(got) != 0 {
		t.Fatalf("a gap past the tolerance must break the run: %+v", got)
	}
}

// F5: two events exactly pulse_merge_gap apart are two pulses, not one. The
// merge keeps events strictly closer than the gap together.
func TestLabelIncidentsAtThePulseMergeGapBoundary(t *testing.T) {
	l := r2Labelling(2)
	inc := labelIncidents("bot-a", "7f", r2Pulses(0, 60*time.Second), l)
	if len(inc) != 1 || inc[0].Pulses != 2 || time.Duration(inc[0].Period) != 60*time.Second {
		t.Fatalf("events exactly 60 s apart are two pulses: %+v", inc)
	}
	// One nanosecond closer and they merge, which leaves a single pulse and
	// no incident.
	if got := labelIncidents("bot-a", "7f", r2Pulses(0, 60*time.Second-1), l); len(got) != 0 {
		t.Fatalf("events closer than the gap are one pulse: %+v", got)
	}
}

// r2Cand builds one report candidate. period == 0 means no periodic
// evidence on that candidate.
func r2Cand(id, to, key string, at time.Time, period time.Duration) scoreCandidate {
	c := scoreCandidate{EventID: id, Rule: "p474", Kind: "periodic", Key: key, To: to, At: at}
	if period > 0 {
		c.Periodic = &struct{ Period time.Duration }{Period: period}
	}
	return c
}

func r2Report(cands ...scoreCandidate) scoreReport {
	return scoreReport{Experimental: true, Start: injT, End: injT.Add(24 * time.Hour), Candidates: cands}
}

// F5: an episode whose period is exactly the tolerance away from k*P is a
// true positive, not a period mismatch.
func TestScorePeriodFitsAtTheToleranceBoundary(t *testing.T) {
	const p = 300 * time.Second
	l := r2Labelling(8)
	if !periodFits(p+30*time.Second, p, l) {
		t.Fatal("a period exactly at the tolerance must fit")
	}
	if periodFits(p+30*time.Second+1, p, l) {
		t.Fatal("a period past the tolerance must not fit")
	}
	// The same through score: the boundary episode is a true positive and
	// raises no period mismatch.
	inc := Incident{Bot: "bot-a", Channel: "7f", Start: injT, Stop: injT.Add(time.Hour),
		Period: dur(p), Pulses: 8}
	lab := InjectLabels{Synthetic: true, Labelling: l, Incidents: []Incident{inc}}
	rep := r2Report(r2Cand("e1", "active", scoreKey, injT.Add(30*time.Minute), p+30*time.Second))
	res, err := score(rep, lab, "p474", "labels.json", nil, injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.TruePositives != 1 || res.PeriodMismatches != 0 || res.IncidentsDetected != 1 {
		t.Fatalf("boundary period: %+v", res)
	}
}

// F5: an episode's end is its last transition to ended, not its first. A
// reopened episode that ends twice must be measured to the later end, or the
// stop latency is understated.
func TestScoreTakesTheLastEnded(t *testing.T) {
	act := injT.Add(time.Hour)
	first := injT.Add(2 * time.Hour)
	last := injT.Add(3 * time.Hour)
	rep := r2Report(
		r2Cand("e1", "active", scoreKey, act, 300*time.Second),
		r2Cand("e1", "ended", scoreKey, first, 0),
		r2Cand("e1", "ended", scoreKey, last, 0),
	)
	eps, noPeriod, err := episodes(rep, "p474", injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || noPeriod != 0 {
		t.Fatalf("episodes %+v (no period %d)", eps, noPeriod)
	}
	if !eps[0].end.Equal(last) {
		t.Fatalf("end %v, want the last ended %v", eps[0].end, last)
	}
}

// F6: the export times every line from a whole-second observation time, so
// an injected line must too. A fractional second would make every injected
// line trivially distinguishable from an exported one.
func TestInjectTimesAreWholeSeconds(t *testing.T) {
	r := injectInto(t, specJSON(specOpts{seed: 31, jitterSD: "3s", multipartFraction: 0.5}),
		backgroundJSONL(40))
	recs := injectedLines(t, r)
	if len(recs) < 20 {
		t.Fatalf("only %d injected lines", len(recs))
	}
	for _, rec := range recs {
		if rec.Time.Nanosecond() != 0 || rec.FinalAt.Nanosecond() != 0 {
			t.Fatalf("%s: time %v final_at %v are not whole seconds", rec.ID, rec.Time, rec.FinalAt)
		}
	}
	// The same on the wire: no injected line carries a fractional second.
	for _, l := range r.lines {
		if !strings.Contains(l, `"id":"`+injPrefix) {
			continue
		}
		for _, f := range []string{`"time":"`, `"final_at":"`} {
			i := strings.Index(l, f)
			if i < 0 {
				t.Fatalf("line %q has no %s", l, f)
			}
			v := l[i+len(f):]
			v = v[:strings.IndexByte(v, '"')]
			if strings.Contains(v, ".") {
				t.Fatalf("%s %s carries a fractional second in %q", f, v, l)
			}
		}
	}
	// The labels are derived from the same whole-second pulses.
	for _, inc := range r.labels.Incidents {
		if inc.Start.Nanosecond() != 0 || inc.Stop.Nanosecond() != 0 {
			t.Fatalf("incident %+v is not on whole seconds", inc)
		}
	}
}

// F6: a real bot's payload length varies. payload_bytes_spread draws each
// event's size uniformly from payload_bytes +/- the spread, and a spread of
// 0 keeps the old constant size.
func TestInjectPayloadSizeVariesWithinTheSpread(t *testing.T) {
	const base, spread = 35, 6
	r := injectInto(t, specJSON(specOpts{seed: 31, payloadSpread: spread}), backgroundJSONL(40))
	seen := map[int]bool{}
	for _, rec := range injectedLines(t, r) {
		if rec.PayloadBytes == nil {
			t.Fatalf("%s has no payload_bytes", rec.ID)
		}
		pb := *rec.PayloadBytes
		if pb < base-spread || pb > base+spread {
			t.Fatalf("%s payload %d outside %d +/- %d", rec.ID, pb, base, spread)
		}
		seen[pb] = true
	}
	if len(seen) < 5 {
		t.Fatalf("payload size barely varies: %v", seen)
	}
	flat := injectInto(t, specJSON(specOpts{seed: 31}), backgroundJSONL(40))
	for _, rec := range injectedLines(t, flat) {
		if *rec.PayloadBytes != base {
			t.Fatalf("spread 0 must keep the size constant, got %d", *rec.PayloadBytes)
		}
	}
}

// F6: the spread is a required field with no default, and it may never push
// the payload size to zero or below.
func TestInjectPayloadSpreadIsValidated(t *testing.T) {
	bot := func(extra string) string {
		return `{"seed":7,"final_after":"30s",
 "labelling":{"pulse_merge_gap":"60s","min_period":"1m","max_period":"1h","min_pulses":8,
  "max_harmonic":3,"tolerance_abs":"30s","tolerance_rel":0.1},
 "bots":[{"name":"bot-a","channel":"7f","payload_type":5,"route_class":"flood","route_kind":"relay",
  "first_hop":"1:fd","full_route":"2:fd0a","wire_bytes":60,` + extra + `
  "start":"` + injAt(0) + `","stop":"` + injAt(time.Hour) + `","period":"5m","jitter_sd":"3s",
  "jitter_max":"10s","missed_fraction":0,"multipart_fraction":0,"multipart_max_extra":2,
  "multipart_max_delay":"55s","holes":[]}]}`
	}
	for _, tc := range []struct{ name, extra string }{
		{"missing", `"payload_bytes":35,`},
		{"negative", `"payload_bytes":35,"payload_bytes_spread":-1,`},
		{"as wide as the payload", `"payload_bytes":35,"payload_bytes_spread":35,`},
		{"without a payload size", `"payload_bytes_spread":3,`},
		{"zero payload size", `"payload_bytes":0,"payload_bytes_spread":0,`},
	} {
		if _, err := parseInjectSpec([]byte(bot(tc.extra))); err == nil {
			t.Fatalf("%s: want an error", tc.name)
		}
	}
	if _, err := parseInjectSpec([]byte(bot(`"payload_bytes":35,"payload_bytes_spread":34,`))); err != nil {
		t.Fatalf("a spread one below the payload size is legal: %v", err)
	}
}

// F6: final_after must be whole seconds, exactly as the export's -settle is.
func TestInjectFinalAfterIsWholeSeconds(t *testing.T) {
	spec := strings.Replace(specJSON(specOpts{seed: 7}), `"final_after": "30s"`, `"final_after": "30.5s"`, 1)
	if _, err := parseInjectSpec([]byte(spec)); err == nil {
		t.Fatal("a fractional final_after must be refused")
	}
}
