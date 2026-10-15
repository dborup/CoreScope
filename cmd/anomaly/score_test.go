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
)

// Issue #474, phase 3: scoring a replay report against injected ground
// truth, and against no ground truth at all (the false-alarm arm).

const scoreKey = "v1|s=stream|pt=5|ch=1:7f|rc=flood|rk=relay|hop=1:fd"

// replayConfigJSON is the periodic rule the phase-3 runs use. exclude is
// spliced in, for example `"exclude_channels": ["7f"],`.
func replayConfigJSON(exclude string) string {
	return `{
 "limits": {"max_keys_per_scope": 200000, "state_ttl": "48h", "dedup_horizon": "6h", "max_dedup_ids": 2000000,
  "reorder_delay": "0s", "max_reorder_buffer": 64, "max_window_entries": 200000,
  "max_candidates_per_call": 10000, "max_pending_candidates": 100000},
 "expected": "include",
 "periodic": [{"name": "p474", "scope": "stream", "payload_types": [5],
  "min_period": "1m", "max_period": "1h", "burst_gap": "15s", "jitter_abs": "10s", "jitter_rel": 0.02,
  "max_missing": 2, "min_pulses": 8, "history_len": 32, "min_coverage": 0.6, "max_jitter_fraction": 0.5,
  "max_chance": 0.01, "end_after_missed_pulses": 3, ` + exclude + `
  "state": {"confirm_after": "0s", "end_after_quiet": "60m", "cooldown": "0s"}}]
}`
}

// replayInto runs the replay command over one input file and returns the
// report path.
func replayInto(t *testing.T, dir, name, config, input string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, name+"-config.json")
	if err := os.WriteFile(cfgPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, name+"-report.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{"replay", "-config", cfgPath, "-input", input,
		"-start", injAt(0), "-end", injAt(24 * time.Hour), "-max-line-bytes", "65536",
		"-out", report}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr.String())
	}
	return report
}

// scoreInto runs the score command and returns the parsed metrics.
func scoreInto(t *testing.T, report, labels string, extra ...string) scoreResult {
	t.Helper()
	args := []string{"score", "-report", report, "-rule", "p474",
		"-start", injAt(0), "-end", injAt(24 * time.Hour)}
	if labels != "" {
		args = append(args, "-labels", labels)
	}
	var stdout, stderr bytes.Buffer
	if code := run(append(args, extra...), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("score exit %d: %s", code, stderr.String())
	}
	var res scoreResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("metrics %q: %v", stdout.String(), err)
	}
	return res
}

// End to end: inject a bot into a background, replay it and score it. The
// injected incident is detected, with the time to detect and the stop
// latency the pre-registration asks for, and the background raises no false
// alarm.
func TestScoreRecallOnAnInjectedBot(t *testing.T) {
	bg := backgroundJSONL(300)
	r := injectInto(t, specJSON(specOpts{seed: 21, jitterSD: "3s"}), bg)
	if len(r.labels.Incidents) != 1 {
		t.Fatalf("labels %+v", r.labels.Incidents)
	}
	report := replayInto(t, r.dir, "b", replayConfigJSON(""), r.outPath)
	res := scoreInto(t, report, r.labPath)
	if res.Incidents != 1 || res.IncidentsDetected != 1 || res.Recall != 1 {
		t.Fatalf("recall: %+v", res)
	}
	if res.TruePositives == 0 || res.FalsePositives != 0 || res.FalseAlarmsPerDay != 0 {
		t.Fatalf("precision side: %+v", res)
	}
	inc := res.PerIncident[0]
	if inc.Channel != "7f" || !inc.Detected || inc.TimeToDetect == nil || inc.StopLatency == nil {
		t.Fatalf("per-incident %+v", inc)
	}
	// The rule needs 8 pulses, so the first alarm cannot come before 7
	// periods; on a clean train it comes at 7 or 8.
	if inc.TTDPeriods < 7 || inc.TTDPeriods > 12 {
		t.Fatalf("time to detect %v (%.1f P)", time.Duration(*inc.TimeToDetect), inc.TTDPeriods)
	}
	// Train tracking (K = 3) ends the episode (K+1)*P after the last pulse
	// at the latest.
	if sl := time.Duration(*inc.StopLatency); sl < 0 || sl > 20*time.Minute {
		t.Fatalf("stop latency %v", sl)
	}
	if res.MedianTimeToDetect == nil || res.MedianStopLatency == nil || res.MedianEpisodesPerIncident < 1 {
		t.Fatalf("medians: %+v", res)
	}
	if res.Days != 1 {
		t.Fatalf("days %v", res.Days)
	}
}

// A noisy bot of the September shape (5 % missed pulses, 30 % multi-part) is
// still found and still raises no false alarm, but how long that takes is a
// property of the rule, not of the scorer: this run only pins recall and
// logs the timing, which is what the evaluation measures.
func TestScoreRecallOnANoisyInjectedBot(t *testing.T) {
	r := injectInto(t, specJSON(specOpts{seed: 21, jitterSD: "3s", missedFraction: 0.05,
		multipartFraction: 0.3}), backgroundJSONL(300))
	res := scoreInto(t, replayInto(t, r.dir, "n", replayConfigJSON(""), r.outPath), r.labPath)
	if res.Incidents != 1 || res.IncidentsDetected != 1 || res.FalsePositives != 0 {
		t.Fatalf("noisy bot: %+v", res)
	}
	inc := res.PerIncident[0]
	t.Logf("noisy bot: %d episodes, time to detect %v (%.1f P), stop latency %v",
		inc.Episodes, time.Duration(*inc.TimeToDetect), inc.TTDPeriods, time.Duration(*inc.StopLatency))
}

// The same background without any injection is the false-alarm arm: no
// labels, and every episode would be a false alarm. This background raises
// none.
func TestScoreFalseAlarmArmWithoutLabels(t *testing.T) {
	dir := t.TempDir()
	bgPath := filepath.Join(dir, "bg.jsonl")
	if err := os.WriteFile(bgPath, []byte(backgroundJSONL(300)), 0o644); err != nil {
		t.Fatal(err)
	}
	report := replayInto(t, dir, "a", replayConfigJSON(""), bgPath)
	res := scoreInto(t, report, "")
	if res.Episodes != 0 || res.FalsePositives != 0 || res.FalseAlarmsPerDay != 0 || res.Incidents != 0 {
		t.Fatalf("background raised alarms: %+v", res)
	}
	if res.Recall != 0 || res.MedianTimeToDetect != nil {
		t.Fatalf("no labels must give no recall and no medians: %+v", res)
	}
}

// Phase 2 and phase 3 together: with the bot's channel hash excluded, the
// same injected traffic produces no periodic episode at all.
func TestScoreExcludedChannelDetectsNothing(t *testing.T) {
	r := injectInto(t, specJSON(specOpts{seed: 21, jitterSD: "3s"}), backgroundJSONL(300))
	plain := scoreInto(t, replayInto(t, r.dir, "p", replayConfigJSON(""), r.outPath), r.labPath)
	if plain.IncidentsDetected != 1 {
		t.Fatalf("without exclusions the bot must be detected: %+v", plain)
	}
	excl := scoreInto(t, replayInto(t, r.dir, "x", replayConfigJSON(`"exclude_channels": ["7f"],`), r.outPath), r.labPath)
	if excl.Episodes != 0 || excl.IncidentsDetected != 0 || excl.Recall != 0 {
		t.Fatalf("the excluded channel still produced episodes: %+v", excl)
	}
	// Excluding another channel leaves the bot alone.
	other := scoreInto(t, replayInto(t, r.dir, "o", replayConfigJSON(`"exclude_channels": ["11"],`), r.outPath), r.labPath)
	if other.IncidentsDetected != 1 {
		t.Fatalf("excluding channel 11 changed the bot's detection: %+v", other)
	}
}

// ---- crafted reports: the matching rules ----

// craftedReport writes a report with one episode per entry.
type craftedEpisode struct {
	id       string
	key      string
	active   time.Duration // activation, relative to injT
	ended    time.Duration // 0 = never ended
	periodNS int64         // 0 = no periodic evidence on the activation
}

func craftReport(t *testing.T, dir string, eps ...craftedEpisode) string {
	t.Helper()
	var cands []string
	for _, e := range eps {
		key := e.key
		if key == "" {
			key = scoreKey
		}
		per := ""
		if e.periodNS != 0 {
			per = fmt.Sprintf(`,"Periodic":{"Period":%d,"Tolerance":10000000000,"Support":8,"Coverage":1}`, e.periodNS)
		}
		cands = append(cands, fmt.Sprintf(`{"EventID":%q,"Rule":"p474","Kind":"periodic","Key":%q,`+
			`"From":"suspicious","To":"active","Reason":"confirmed","At":%q,"EpisodeStart":%q,`+
			`"Confidence":"route_observed","Traffic":"unclassified","Coverage":[]%s}`,
			e.id, key, injT.Add(e.active).Format(time.RFC3339), injT.Add(e.active).Format(time.RFC3339), per))
		if e.ended != 0 {
			cands = append(cands, fmt.Sprintf(`{"EventID":%q,"Rule":"p474","Kind":"periodic","Key":%q,`+
				`"From":"active","To":"ended","Reason":"quiet","At":%q,"EpisodeStart":%q,`+
				`"Confidence":"route_observed","Traffic":"unclassified","Coverage":[]}`,
				e.id, key, injT.Add(e.ended).Format(time.RFC3339), injT.Add(e.active).Format(time.RFC3339)))
		}
	}
	body := fmt.Sprintf(`{"Experimental":true,"Start":%q,"End":%q,"Input":{"Lines":1},"Candidates":[%s],"CoverageReduced":false}`,
		injT.Format(time.RFC3339), injT.Add(24*time.Hour).Format(time.RFC3339), strings.Join(cands, ","))
	p := filepath.Join(dir, fmt.Sprintf("crafted-%d.json", len(eps)+int(time.Now().UnixNano()%1000)))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// craftLabels writes a labels file with one incident on channel 7f.
func craftLabels(t *testing.T, dir string, start, stop time.Duration, period time.Duration) string {
	t.Helper()
	l := InjectLabels{Experimental: true, Synthetic: true, Seed: 1,
		Labelling: labellingSpec{PulseMergeGap: dur(time.Minute), MinPeriod: dur(time.Minute),
			MaxPeriod: dur(time.Hour), MinPulses: 8, MaxHarmonic: 3,
			ToleranceAbs: dur(30 * time.Second), ToleranceRel: 0.1},
		Incidents: []Incident{{Bot: "bot-a", Channel: "7f", Start: injT.Add(start), Stop: injT.Add(stop),
			Period: dur(period), Pulses: 20}}}
	b, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, fmt.Sprintf("labels-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// An episode is a true positive only if its period agrees with the
// incident's within the labelling tolerance of k*P, k in 1..max_harmonic.
func TestScorePeriodCheck(t *testing.T) {
	dir := t.TempDir()
	labels := craftLabels(t, dir, time.Hour, 2*time.Hour, 5*time.Minute)
	for _, c := range []struct {
		name     string
		periodNS int64
		tp       bool
		mismatch int
	}{
		{"exact", int64(5 * time.Minute), true, 0},
		{"inside the 30 s tolerance", int64(5*time.Minute + 25*time.Second), true, 0},
		{"outside the tolerance", int64(5*time.Minute + 35*time.Second), false, 1},
		{"second harmonic", int64(10 * time.Minute), true, 0},
		{"third harmonic", int64(15 * time.Minute), true, 0},
		{"fourth harmonic", int64(20 * time.Minute), false, 1},
		{"unrelated", int64(99 * time.Second), false, 1},
		{"no periodic evidence", 0, false, 0},
	} {
		report := craftReport(t, dir, craftedEpisode{id: "e1", active: 90 * time.Minute,
			ended: 110 * time.Minute, periodNS: c.periodNS})
		res := scoreInto(t, report, labels)
		tp := res.TruePositives == 1
		if tp != c.tp || res.PeriodMismatches != c.mismatch {
			t.Errorf("%s: tp=%v mismatches=%d, want tp=%v mismatches=%d (%+v)",
				c.name, tp, res.PeriodMismatches, c.tp, c.mismatch, res)
		}
		if c.periodNS == 0 && res.EpisodesWithoutPeriod != 1 {
			t.Errorf("%s: episodes_without_period %d", c.name, res.EpisodesWithoutPeriod)
		}
	}
}

// The match window is [incident start, stop + 4P] against
// [activation, end], and the channel hash must be equal.
func TestScoreOverlapAndChannel(t *testing.T) {
	dir := t.TempDir()
	p := 5 * time.Minute
	labels := craftLabels(t, dir, time.Hour, 2*time.Hour, p)
	for _, c := range []struct {
		name         string
		active, end  time.Duration
		key          string
		tp, mismatch bool
	}{
		{name: "inside", active: 90 * time.Minute, end: 110 * time.Minute, tp: true},
		{name: "ends just inside the start", active: 30 * time.Minute, end: time.Hour, tp: true},
		{name: "ends before the start", active: 10 * time.Minute, end: 50 * time.Minute},
		{name: "activates at stop + 4P", active: 2*time.Hour + 4*p, end: 3 * time.Hour, tp: true},
		{name: "activates after stop + 4P", active: 2*time.Hour + 4*p + time.Minute, end: 3 * time.Hour},
		{name: "never ended", active: 90 * time.Minute, tp: true},
		{name: "another channel", active: 90 * time.Minute, end: 110 * time.Minute,
			key: "v1|s=stream|pt=5|ch=1:11|rc=flood|rk=relay|hop=1:fd"},
	} {
		report := craftReport(t, dir, craftedEpisode{id: "e1", key: c.key, active: c.active,
			ended: c.end, periodNS: int64(p)})
		res := scoreInto(t, report, labels)
		if (res.TruePositives == 1) != c.tp {
			t.Errorf("%s: tp %d, want %v (%+v)", c.name, res.TruePositives, c.tp, res)
		}
		if res.Episodes != 1 {
			t.Errorf("%s: %d episodes", c.name, res.Episodes)
		}
	}
}

// Only episodes activated inside [-start, -end) are scored, and each
// EventID is one episode however many transitions it has.
func TestScoreWindowAndEpisodeGrouping(t *testing.T) {
	dir := t.TempDir()
	labels := craftLabels(t, dir, time.Hour, 2*time.Hour, 5*time.Minute)
	report := craftReport(t, dir,
		craftedEpisode{id: "in", active: 90 * time.Minute, ended: 110 * time.Minute, periodNS: int64(5 * time.Minute)},
		craftedEpisode{id: "late", active: 25 * time.Hour, ended: 26 * time.Hour, periodNS: int64(5 * time.Minute)},
		craftedEpisode{id: "early", active: -time.Hour, ended: 30 * time.Minute, periodNS: int64(5 * time.Minute)},
	)
	res := scoreInto(t, report, labels)
	if res.Episodes != 1 || res.TruePositives != 1 || res.PerIncident[0].Episodes != 1 {
		t.Fatalf("window: %+v", res)
	}
	// Two episodes on the same incident are two episodes and one detection.
	two := craftReport(t, dir,
		craftedEpisode{id: "e1", active: 90 * time.Minute, ended: 100 * time.Minute, periodNS: int64(5 * time.Minute)},
		craftedEpisode{id: "e2", active: 105 * time.Minute, ended: 115 * time.Minute, periodNS: int64(5 * time.Minute)},
	)
	res = scoreInto(t, two, labels)
	if res.Episodes != 2 || res.TruePositives != 2 || res.IncidentsDetected != 1 ||
		res.PerIncident[0].Episodes != 2 || res.MedianEpisodesPerIncident != 2 {
		t.Fatalf("fragmentation: %+v", res)
	}
	// The time to detect is the first matching activation and the stop
	// latency the last matching end.
	if got := time.Duration(*res.MedianTimeToDetect); got != 30*time.Minute {
		t.Fatalf("time to detect %v", got)
	}
	if got := time.Duration(*res.MedianStopLatency); got != -5*time.Minute {
		t.Fatalf("stop latency %v", got)
	}
}

// False positives are counted per day and listed per channel hash.
func TestScoreFalsePositivesPerDay(t *testing.T) {
	dir := t.TempDir()
	labels := craftLabels(t, dir, time.Hour, 2*time.Hour, 5*time.Minute)
	report := craftReport(t, dir,
		craftedEpisode{id: "tp", active: 90 * time.Minute, ended: 110 * time.Minute, periodNS: int64(5 * time.Minute)},
		craftedEpisode{id: "fp1", active: 10 * time.Hour, ended: 11 * time.Hour, periodNS: int64(5 * time.Minute)},
		craftedEpisode{id: "fp2", active: 12 * time.Hour, ended: 13 * time.Hour, periodNS: int64(5 * time.Minute),
			key: "v1|s=stream|pt=5|ch=1:11|rc=flood|rk=relay|hop=1:fd"},
	)
	res := scoreInto(t, report, labels)
	if res.FalsePositives != 2 || res.FalseAlarmsPerDay != 2 {
		t.Fatalf("false alarms: %+v", res)
	}
	if len(res.FalsePositivesByChannel) != 2 || res.FalsePositivesByChannel[0].Channel != "11" ||
		res.FalsePositivesByChannel[1].Channel != "7f" {
		t.Fatalf("per channel: %+v", res.FalsePositivesByChannel)
	}
}

// Another rule's episodes are not scored.
func TestScoreOnlyTheNamedRule(t *testing.T) {
	dir := t.TempDir()
	report := craftReport(t, dir, craftedEpisode{id: "e1", active: 90 * time.Minute,
		ended: 110 * time.Minute, periodNS: int64(5 * time.Minute)})
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.json")
	if err := os.WriteFile(other, bytes.ReplaceAll(b, []byte(`"Rule":"p474"`), []byte(`"Rule":"p999"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := scoreInto(t, other, ""); res.Episodes != 0 {
		t.Fatalf("another rule was scored: %+v", res)
	}
	if res := scoreInto(t, report, ""); res.Episodes != 1 {
		t.Fatalf("the named rule was not scored: %+v", res)
	}
}

func TestScoreRequiresExplicitFlagsAndValidInput(t *testing.T) {
	dir := t.TempDir()
	report := craftReport(t, dir, craftedEpisode{id: "e1", active: time.Hour, periodNS: int64(5 * time.Minute)})
	full := []string{"score", "-report", report, "-rule", "p474", "-start", injAt(0), "-end", injAt(24 * time.Hour)}
	for i := 1; i < len(full); i += 2 {
		args := append(append([]string{}, full[:i]...), full[i+2:]...)
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != exitUsage {
			t.Fatalf("without %s: exit %d", full[i], code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(append(append([]string{}, full...), "-end", injAt(-time.Hour)), nil, &stdout, &stderr); code != exitUsage {
		t.Fatalf("-end before -start: exit %d", code)
	}
	// A report that is not a replay report, and a labels file that is not an
	// injected labels file, are both errors.
	bad := filepath.Join(dir, "bad.json")
	for _, body := range []string{`{}`, `{"Experimental":true}`, `not json`,
		`{"Experimental":true,"Start":"2030-03-01T00:00:00Z","End":"2030-03-02T00:00:00Z","Candidates":[{"EventID":"e","Rule":"p474","Kind":"periodic","Key":"nonsense","To":"active","At":"2030-03-01T01:00:00Z"}]}`} {
		if err := os.WriteFile(bad, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		args := append([]string{}, full...)
		args[2] = bad
		if code := run(args, nil, &stdout, &stderr); code != exitFail {
			t.Fatalf("report %q: exit %d", body, code)
		}
	}
	badLabels := filepath.Join(dir, "badlabels.json")
	validLabelling := `"labelling":{"pulse_merge_gap":"60s","min_period":"1m","max_period":"1h","min_pulses":8,"max_harmonic":3,"tolerance_abs":"30s","tolerance_rel":0.1}`
	for _, body := range []string{`{}`,
		// a complete file that is not marked synthetic: scoring is only
		// defined against injected ground truth
		`{"experimental":true,"synthetic":false,` + validLabelling + `,"incidents":[]}`,
		`{"experimental":true,"synthetic":true,"labelling":{"pulse_merge_gap":"60s","min_period":"1m","max_period":"1h","min_pulses":8,"max_harmonic":3,"tolerance_abs":"30s","tolerance_rel":0.1},"incidents":[{"bot":"b","channel":"","start":"2030-03-01T00:00:00Z","stop":"2030-03-01T01:00:00Z","period":"5m","pulses":9}]}`,
		`{"experimental":true,"synthetic":true,"surprise":1}`} {
		if err := os.WriteFile(badLabels, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := run(append(append([]string{}, full...), "-labels", badLabels), nil, &stdout, &stderr); code != exitFail {
			t.Fatalf("labels %q: exit %d", body, code)
		}
	}
}

// -out writes the metrics to a new file and never overwrites one.
func TestScoreWritesOutFileOnce(t *testing.T) {
	dir := t.TempDir()
	report := craftReport(t, dir, craftedEpisode{id: "e1", active: time.Hour, periodNS: int64(5 * time.Minute)})
	out := filepath.Join(dir, "metrics.json")
	args := []string{"score", "-report", report, "-rule", "p474", "-start", injAt(0),
		"-end", injAt(24 * time.Hour), "-out", out}
	var stdout, stderr bytes.Buffer
	if code := run(args, nil, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("exit %d, stdout %d bytes: %s", code, stdout.Len(), stderr.String())
	}
	var res scoreResult
	b, err := os.ReadFile(out)
	if err != nil || json.Unmarshal(b, &res) != nil || res.Episodes != 1 {
		t.Fatalf("metrics file: %v %s", err, b)
	}
	if code := run(args, nil, &stdout, &stderr); code != exitFail {
		t.Fatal("an existing metrics file was overwritten")
	}
}
