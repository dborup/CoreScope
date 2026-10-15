package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
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
	inc := labelIncidents("bot-a", "7f", scoreKey, r2Pulses(exact...), r2Labelling(8))
	if len(inc) != 1 || inc[0].Pulses != 8 {
		t.Fatalf("exactly 8 pulses must be one 8-pulse incident: %+v", inc)
	}
	if got := labelIncidents("bot-a", "7f", scoreKey, r2Pulses(exact[:7]...), r2Labelling(8)); len(got) != 0 {
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
	inc := labelIncidents("bot-a", "7f", scoreKey, r2Pulses(at(330*time.Second)...), r2Labelling(8))
	if len(inc) != 1 || inc[0].Pulses != 8 {
		t.Fatalf("a gap exactly at the tolerance must stay in the run: %+v", inc)
	}
	// One nanosecond further out and the run breaks before the last pulse,
	// which leaves 7 pulses: below min_pulses, so no incident at all.
	if got := labelIncidents("bot-a", "7f", scoreKey, r2Pulses(at(330*time.Second+1)...), r2Labelling(8)); len(got) != 0 {
		t.Fatalf("a gap past the tolerance must break the run: %+v", got)
	}
}

// F5: two events exactly pulse_merge_gap apart are two pulses, not one. The
// merge keeps events strictly closer than the gap together.
func TestLabelIncidentsAtThePulseMergeGapBoundary(t *testing.T) {
	l := r2Labelling(2)
	inc := labelIncidents("bot-a", "7f", scoreKey, r2Pulses(0, 60*time.Second), l)
	if len(inc) != 1 || inc[0].Pulses != 2 || time.Duration(inc[0].Period) != 60*time.Second {
		t.Fatalf("events exactly 60 s apart are two pulses: %+v", inc)
	}
	// One nanosecond closer and they merge, which leaves a single pulse and
	// no incident.
	if got := labelIncidents("bot-a", "7f", scoreKey, r2Pulses(0, 60*time.Second-1), l); len(got) != 0 {
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
	inc := Incident{Bot: "bot-a", Channel: "7f", Key: scoreKey, Start: injT, Stop: injT.Add(time.Hour),
		Period: dur(p), Pulses: 8}
	lab := InjectLabels{Synthetic: true, Labelling: l, Incidents: []Incident{inc}}
	rep := r2Report(r2Cand("e1", "active", scoreKey, injT.Add(30*time.Minute), p+30*time.Second))
	res, err := score(rep, lab, "p474", "labels.json", "", nil, nil, injT, injT.Add(24*time.Hour))
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
  "first_hop":"1:fd","full_route":"1:fd0a","wire_bytes":60,` + extra + `
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
		// Round 3: the top of the draw range has to fit the frame too, not
		// only payload_bytes itself (see TestInjectSpreadEnvelopeMustFitTheFrame).
		{"wider than the frame allows", `"payload_bytes":35,"payload_bytes_spread":34,`},
	} {
		if _, err := parseInjectSpec([]byte(bot(tc.extra))); err == nil {
			t.Fatalf("%s: want an error", tc.name)
		}
	}
	// A spread just inside both bounds - below payload_bytes, and with its
	// top at exactly wire_bytes - 2 - is legal.
	if _, err := parseInjectSpec([]byte(bot(`"payload_bytes":35,"payload_bytes_spread":23,`))); err != nil {
		t.Fatalf("a spread whose envelope fits the frame is legal: %v", err)
	}
}

// F6: final_after must be whole seconds, exactly as the export's -settle is.
func TestInjectFinalAfterIsWholeSeconds(t *testing.T) {
	spec := strings.Replace(specJSON(specOpts{seed: 7}), `"final_after": "30s"`, `"final_after": "30.5s"`, 1)
	if _, err := parseInjectSpec([]byte(spec)); err == nil {
		t.Fatal("a fractional final_after must be refused")
	}
}

// keysOn runs the keys command over one background and returns the parsed
// ranking.
func keysOn(t *testing.T, background string, extra ...string) keysResult {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "bg.jsonl")
	if err := os.WriteFile(in, []byte(background), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"keys", "-input", in, "-payload-type", "5", "-top", "8",
		"-max-keys", "1000", "-max-line-bytes", "65536"}, extra...)
	var stdout, stderr bytes.Buffer
	if code := run(args, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("keys exit %d: %s", code, stderr.String())
	}
	var res keysResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("keys output %q: %v", stdout.String(), err)
	}
	return res
}

// r2Background writes n events spread over the given (channel, first hop,
// full route) series, with the share of each series fixed by weight.
type r2Series struct {
	channel, firstHop, fullRoute string
	weight                       int
	payload                      int
}

func r2Background(series []r2Series, rounds int) string {
	var b strings.Builder
	t := injT
	for i := 0; i < rounds; i++ {
		for _, s := range series {
			for w := 0; w < s.weight; w++ {
				t = t.Add(17 * time.Second)
				pb := s.payload + w
				rec := anomaly.ReplayRecord{ID: fmt.Sprintf("tx%d", b.Len()+w+1), Time: t,
					FinalAt: t.Add(30 * time.Second), PayloadType: 5, Channel: s.channel,
					RouteClass: "flood", RouteKind: "relay", FirstHop: s.firstHop,
					FullRoute: s.fullRoute, WireBytes: 55, PayloadBytes: &pb,
					Traffic: "unclassified", Decryption: "unknown"}
				j, err := json.Marshal(rec)
				if err != nil {
					panic(err)
				}
				b.Write(j)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// F2: the panel has to sit on stream keys real traffic uses, and those keys
// have to be picked mechanically. keys ranks the background's own stream
// keys by row count - nothing else - and reports the wire fields an
// injection spec needs to land on each one.
func TestKeysRanksBackgroundStreamKeysByRowCount(t *testing.T) {
	res := keysOn(t, r2Background([]r2Series{
		{channel: "0b", firstHop: "1:5c", fullRoute: "1:5c0a1c", weight: 5, payload: 30},
		{channel: "0b", firstHop: "2:7a01", fullRoute: "2:7a01", weight: 2, payload: 40},
		{channel: "81", firstHop: "1:5c", fullRoute: "1:5c", weight: 9, payload: 20},
	}, 6), "-exclude-channels", "81")
	if res.DistinctKeys != 2 || len(res.Top) != 2 {
		t.Fatalf("ranking %+v", res)
	}
	if res.SkippedExcluded != 54 {
		t.Fatalf("the excluded channel must be skipped, got %d", res.SkippedExcluded)
	}
	top, second := res.Top[0], res.Top[1]
	if top.Rank != 1 || top.Events != 30 || top.Channel != "0b" || top.FirstHop != "1:5c" {
		t.Fatalf("top key %+v", top)
	}
	if second.Rank != 2 || second.Events != 12 || second.FirstHop != "2:7a01" {
		t.Fatalf("second key %+v", second)
	}
	// The ranking is the key encoding the report writes, so a spec built
	// from it lands on exactly that key.
	if top.Key != "v1|s=stream|pt=5|ch=1:0b|rc=flood|rk=relay|hop=1:5c" {
		t.Fatalf("top key encoding %q", top.Key)
	}
	// The sizes and the route are the realistic values for a bot on the
	// key: the observed whole route, not the first hop repeated.
	if top.FullRoute != "1:5c0a1c" {
		t.Fatalf("full route %q", top.FullRoute)
	}
	if top.PayloadBytesMedian < 30 || top.PayloadBytesMedian > 34 || top.PayloadBytesSpread == 0 {
		t.Fatalf("payload figures %+v", top)
	}
	if top.WireBytesMedian != 55 {
		t.Fatalf("wire median %d", top.WireBytesMedian)
	}
	if top.First.IsZero() || top.Last.Before(top.First) {
		t.Fatalf("times %v..%v", top.First, top.Last)
	}
}

// F2: the filter is part of the selection rule, so it is restated in the
// output and every skipped line is counted. A line of another payload type,
// without a channel hash or not relayed cannot share a stream key with a
// channel bot and never enters the ranking.
func TestKeysFilterIsMechanicalAndCounted(t *testing.T) {
	lines := []string{
		`{"id":"tx1","time":"2030-03-01T00:00:01Z","payload_type":5,"channel":"0b","route_class":"flood","route_kind":"relay","first_hop":"1:5c","full_route":"1:5c","wire_bytes":55,"traffic":"unclassified","decryption":"unknown"}`,
		`{"id":"tx2","time":"2030-03-01T00:00:02Z","payload_type":4,"channel":"0b","route_class":"flood","route_kind":"relay","first_hop":"1:5c","full_route":"1:5c","wire_bytes":55,"traffic":"unclassified","decryption":"unknown"}`,
		`{"id":"tx3","time":"2030-03-01T00:00:03Z","payload_type":5,"channel":"","route_class":"flood","route_kind":"relay","first_hop":"1:5c","full_route":"1:5c","wire_bytes":55,"traffic":"unclassified","decryption":"unknown"}`,
		`{"id":"tx4","time":"2030-03-01T00:00:04Z","payload_type":5,"channel":"0b","route_class":"direct","route_kind":"direct","first_hop":"","full_route":"","wire_bytes":55,"traffic":"unclassified","decryption":"unknown"}`,
		`{"id":"tx5","time":"2030-03-01T00:00:05Z","payload_type":5,"channel":"81","route_class":"flood","route_kind":"relay","first_hop":"1:5c","full_route":"1:5c","wire_bytes":55,"traffic":"unclassified","decryption":"unknown"}`,
	}
	res := keysOn(t, strings.Join(lines, "\n")+"\n", "-exclude-channels", "81")
	if res.InputLines != 5 || res.Considered != 1 || res.DistinctKeys != 1 {
		t.Fatalf("counts %+v", res)
	}
	if res.SkippedType != 1 || res.SkippedNoChannel != 1 || res.SkippedNotRelay != 1 || res.SkippedExcluded != 1 {
		t.Fatalf("skip counters %+v", res)
	}
	if !strings.Contains(res.Filter, "route_kind == relay") || res.PayloadType != 5 {
		t.Fatalf("the output must restate the filter: %+v", res)
	}
	if len(res.ExcludeChannels) != 1 || res.ExcludeChannels[0] != "81" {
		t.Fatalf("exclusions %+v", res.ExcludeChannels)
	}
}

// F2: -max-keys is a hard bound, as the export's -max-rows is, so a run
// never silently reports a truncated ranking.
func TestKeysMaxKeysIsAHardBound(t *testing.T) {
	bg := r2Background([]r2Series{
		{channel: "0b", firstHop: "1:5c", fullRoute: "1:5c", weight: 1, payload: 30},
		{channel: "0c", firstHop: "1:5d", fullRoute: "1:5d", weight: 1, payload: 30},
	}, 2)
	dir := t.TempDir()
	in := filepath.Join(dir, "bg.jsonl")
	if err := os.WriteFile(in, []byte(bg), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"keys", "-input", in, "-payload-type", "5", "-top", "8",
		"-max-keys", "1", "-max-line-bytes", "65536"}, nil, &stdout, &stderr)
	if code != exitFail || !strings.Contains(stderr.String(), "-max-keys") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("a failed run must write no ranking, got %q", stdout.String())
	}
}

// otherHopKey is the same channel hash and payload type as scoreKey, but a
// different first hop: a second route group on one busy channel.
const otherHopKey = "v1|s=stream|pt=5|ch=1:7f|rc=flood|rk=relay|hop=1:ee"

// F2: once the panel shares a channel with real traffic, matching on the
// channel hash alone is wrong. An episode of a different route group on the
// same channel is a false positive, not a detection of the bot.
func TestScoreMatchesOnTheStreamKeyNotTheChannel(t *testing.T) {
	const p = 300 * time.Second
	l := r2Labelling(8)
	inc := Incident{Bot: "bot-a", Channel: "7f", Key: scoreKey, Start: injT,
		Stop: injT.Add(time.Hour), Period: dur(p), Pulses: 8}
	lab := InjectLabels{Synthetic: true, Labelling: l, Incidents: []Incident{inc}}
	// One episode, same channel hash, same period, overlapping - but
	// another route group.
	rep := r2Report(r2Cand("e1", "active", otherHopKey, injT.Add(30*time.Minute), p))
	res, err := score(rep, lab, "p474", "labels.json", "", nil, nil, injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.IncidentsDetected != 0 || res.Recall != 0 || res.TruePositives != 0 {
		t.Fatalf("another route group is not the bot: %+v", res)
	}
	if res.FalsePositives != 1 || res.PeriodMismatches != 0 {
		t.Fatalf("it is a false positive: %+v", res)
	}
	// On the bot's own key the same episode is the detection.
	rep = r2Report(r2Cand("e1", "active", scoreKey, injT.Add(30*time.Minute), p))
	res, err = score(rep, lab, "p474", "labels.json", "", nil, nil, injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.IncidentsDetected != 1 || res.TruePositives != 1 || res.FalsePositives != 0 {
		t.Fatalf("the bot's own key is the detection: %+v", res)
	}
	// A labels file from before round 2 carries no key; then the channel
	// hash is all there is, and the old behaviour stands.
	inc.Key = ""
	lab.Incidents = []Incident{inc}
	rep = r2Report(r2Cand("e1", "active", otherHopKey, injT.Add(30*time.Minute), p))
	res, err = score(rep, lab, "p474", "labels.json", "", nil, nil, injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.IncidentsDetected != 1 {
		t.Fatalf("without a key, matching falls back to the channel: %+v", res)
	}
}

// F2: a shared key needs a differential reading. An episode that the
// baseline arm (same window, no injection) already has on the same key and
// overlapping in time is the background's: it is neither a detection of the
// bot nor a false alarm of the injected arm.
func TestScoreBaselineDropsEpisodesPresentWithoutInjection(t *testing.T) {
	const p = 300 * time.Second
	l := r2Labelling(8)
	inc := Incident{Bot: "bot-a", Channel: "7f", Key: scoreKey, Start: injT,
		Stop: injT.Add(time.Hour), Period: dur(p), Pulses: 8}
	lab := InjectLabels{Synthetic: true, Labelling: l, Incidents: []Incident{inc}}
	rep := r2Report(
		r2Cand("e1", "active", scoreKey, injT.Add(30*time.Minute), p),
		r2Cand("e1", "ended", scoreKey, injT.Add(90*time.Minute), 0),
	)
	// Without a baseline the episode is the detection.
	res, err := score(rep, lab, "p474", "labels.json", "", nil, nil, injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.IncidentsDetected != 1 || res.BaselineEpisodes != 0 {
		t.Fatalf("no baseline: %+v", res)
	}
	// The same episode in the baseline arm, on the same key and overlapping:
	// the background already had it, so arm B learns nothing from it.
	baseRep := r2Report(
		r2Cand("a1", "active", scoreKey, injT.Add(20*time.Minute), p),
		r2Cand("a1", "ended", scoreKey, injT.Add(100*time.Minute), 0),
	)
	base, _, err := episodes(baseRep, "p474", injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	res, err = score(rep, lab, "p474", "labels.json", "report-a.json", base, nil, injT, injT.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.IncidentsDetected != 0 || res.Recall != 0 {
		t.Fatalf("a pre-existing episode is not a detection: %+v", res)
	}
	if res.BaselineEpisodes != 1 || res.TruePositives != 0 || res.FalsePositives != 0 {
		t.Fatalf("it is counted on its own: %+v", res)
	}
	if res.Baseline != "report-a.json" {
		t.Fatalf("baseline provenance %q", res.Baseline)
	}
	// A baseline episode on another key, or one that does not overlap,
	// leaves the detection alone.
	for _, other := range []scoreReport{
		r2Report(r2Cand("a1", "active", otherHopKey, injT.Add(20*time.Minute), p),
			r2Cand("a1", "ended", otherHopKey, injT.Add(100*time.Minute), 0)),
		r2Report(r2Cand("a1", "active", scoreKey, injT.Add(3*time.Hour), p),
			r2Cand("a1", "ended", scoreKey, injT.Add(4*time.Hour), 0)),
	} {
		b, _, err := episodes(other, "p474", injT, injT.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		res, err = score(rep, lab, "p474", "labels.json", "report-a.json", b, nil, injT, injT.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if res.IncidentsDetected != 1 || res.BaselineEpisodes != 0 {
			t.Fatalf("unrelated baseline episode changed the result: %+v", res)
		}
	}
}

// F2: -baseline works end to end from two report files.
func TestScoreBaselineFlagReadsAReportFile(t *testing.T) {
	bg := backgroundJSONL(300)
	r := injectInto(t, specJSON(specOpts{seed: 21, jitterSD: "3s"}), bg)
	bgPath := filepath.Join(r.dir, "bg.jsonl")
	if err := os.WriteFile(bgPath, []byte(bg), 0o644); err != nil {
		t.Fatal(err)
	}
	reportB := replayInto(t, r.dir, "b", replayConfigJSON(""), r.outPath)
	reportA := replayInto(t, r.dir, "a", replayConfigJSON(""), bgPath)
	res := scoreInto(t, reportB, r.labPath, "-baseline", reportA)
	// This background raises no episode of its own, so the baseline drops
	// nothing and the injected bot is still found.
	if res.Baseline != "a-report.json" || res.BaselineEpisodes != 0 || res.IncidentsDetected != 1 {
		t.Fatalf("baseline arm: %+v", res)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"score", "-report", reportB, "-labels", r.labPath, "-rule", "p474",
		"-baseline", filepath.Join(r.dir, "missing.json"),
		"-start", injAt(0), "-end", injAt(24 * time.Hour)}, nil, &stdout, &stderr)
	if code != exitFail || stdout.Len() != 0 {
		t.Fatalf("a missing baseline must fail: exit %d, stdout %q", code, stdout.String())
	}
}

// F3: five clean bots alarm at the earliest pulse the rule allows and the
// realistic ones take many times longer, so the median sits on the floor and
// hides them. The p75 and the maximum are reported beside every median.
func TestScoreReportsTheTailBesideTheMedian(t *testing.T) {
	const p = 300 * time.Second
	l := r2Labelling(8)
	var incs []Incident
	var cands []scoreCandidate
	// Four incidents detected at 1 P and four at 10, 20, 30 and 40 P.
	ttds := []time.Duration{p, p, p, p, 10 * p, 20 * p, 30 * p, 40 * p}
	for i, ttd := range ttds {
		start := injT.Add(time.Duration(i) * 12 * time.Hour)
		key := fmt.Sprintf("v1|s=stream|pt=5|ch=1:7f|rc=flood|rk=relay|hop=1:%02x", 0xa0+i)
		incs = append(incs, Incident{Bot: fmt.Sprintf("b%d", i), Channel: "7f", Key: key,
			Start: start, Stop: start.Add(60 * p), Period: dur(p), Pulses: 8})
		cands = append(cands,
			r2Cand(fmt.Sprintf("e%d", i), "active", key, start.Add(ttd), p),
			r2Cand(fmt.Sprintf("e%d", i), "ended", key, start.Add(ttd+p), 0))
	}
	lab := InjectLabels{Synthetic: true, Labelling: l, Incidents: incs}
	rep := scoreReport{Experimental: true, Start: injT, End: injT.Add(120 * time.Hour), Candidates: cands}
	res, err := score(rep, lab, "p474", "labels.json", "", nil, nil, injT, injT.Add(120*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.IncidentsDetected != 8 {
		t.Fatalf("detected %d of 8: %+v", res.IncidentsDetected, res)
	}
	// The median of 1, 1, 1, 1, 10, 20, 30, 40 is 5.5 P; the nearest-rank
	// p75 of eight values is the sixth, 20 P, and the maximum is 40 P.
	if res.MedianTTDPeriods > 6 {
		t.Fatalf("median %.1f P should sit low", res.MedianTTDPeriods)
	}
	if res.P75TTDPeriods != 20 || res.MaxTTDPeriods != 40 {
		t.Fatalf("p75 %.1f P, max %.1f P", res.P75TTDPeriods, res.MaxTTDPeriods)
	}
	if res.P75TimeToDetect == nil || time.Duration(*res.P75TimeToDetect) != 20*p {
		t.Fatalf("p75 time to detect %v", res.P75TimeToDetect)
	}
	if res.MaxTimeToDetect == nil || time.Duration(*res.MaxTimeToDetect) != 40*p {
		t.Fatalf("max time to detect %v", res.MaxTimeToDetect)
	}
	if res.MaxStopLatency == nil || res.P75StopLatency == nil || res.MaxEpisodesPerIncident != 1 {
		t.Fatalf("stop latency and episode tail: %+v", res)
	}
	// A bar on the noisy subset alone is the other half of the fix: -bots
	// narrows the panel, and the medians then describe that subset only.
	noisy, err := score(rep, lab, "p474", "labels.json", "", nil, []string{"b4", "b5", "b6", "b7"},
		injT, injT.Add(120*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if noisy.IncidentsSelected != 4 || noisy.MedianTTDPeriods != 25 || noisy.MaxTTDPeriods != 40 {
		t.Fatalf("noisy subset: selected %d, median %.1f P, max %.1f P",
			noisy.IncidentsSelected, noisy.MedianTTDPeriods, noisy.MaxTTDPeriods)
	}
}

// F8: the metrics record the file names of the labels and the baseline, not
// the paths they were read from. A path can carry a user or a host name, and
// a metrics file may be shared.
func TestScoreProvenanceIsFileNamesOnly(t *testing.T) {
	r := injectInto(t, specJSON(specOpts{seed: 21, jitterSD: "3s"}), backgroundJSONL(300))
	res := scoreInto(t, replayInto(t, r.dir, "b", replayConfigJSON(""), r.outPath), r.labPath)
	if res.Labels != "labels.json" {
		t.Fatalf("labels provenance %q must be a bare file name", res.Labels)
	}
	if strings.ContainsRune(res.Labels, filepath.Separator) || strings.Contains(res.Labels, r.dir) {
		t.Fatalf("labels provenance %q leaks a path", res.Labels)
	}
	// No labels, no provenance.
	none := scoreInto(t, replayInto(t, r.dir, "n", replayConfigJSON(""), r.outPath), "")
	if none.Labels != "" || none.Baseline != "" {
		t.Fatalf("provenance without files: %q / %q", none.Labels, none.Baseline)
	}
}
