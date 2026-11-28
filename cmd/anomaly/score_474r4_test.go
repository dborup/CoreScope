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

// Issue #474, round 4: arm A scored against real-traffic labels from
// `label`. A false alarm is an episode that matches no labelled real sender
// (same channel hash, [activation, end] overlapping [start, stop + 4P], P̂
// within max(30 s, 0.1 P) of k*P, k in 1..3). Round 3's strict count, every
// episode a false alarm, is reported beside it.

// r4Metrics holds the round-4 fields of the metrics file. It is declared
// here so the test pins their names.
type r4Metrics struct {
	RealLabels              string   `json:"real_labels"`
	Labels                  string   `json:"labels"`
	Episodes                int      `json:"episodes"`
	TruePositives           int      `json:"true_positives"`
	FalsePositives          int      `json:"false_positives"`
	FalseAlarmsPerDay       float64  `json:"false_alarms_per_day"`
	StrictFalsePositives    *int     `json:"strict_false_positives"`
	StrictFalseAlarmsPerDay *float64 `json:"strict_false_alarms_per_day"`
	Incidents               int      `json:"incidents"`
	IncidentsDetected       int      `json:"incidents_detected"`
	Recall                  float64  `json:"recall"`
	PeriodMismatches        int      `json:"period_mismatches"`
	PerIncident             []struct {
		Bot      string `json:"bot"`
		Channel  string `json:"channel"`
		Detected bool   `json:"detected"`
		Episodes int    `json:"episodes"`
	} `json:"per_incident"`
}

// r4Sender is one crafted real-sender tuple, times relative to injT.
type r4Sender struct {
	channel     string
	start, stop time.Duration
	period      time.Duration
}

// r4RealLabels writes a label file as `label` writes it, over
// [injT + from, injT + to).
func r4RealLabels(t *testing.T, dir string, from, to time.Duration, senders ...r4Sender) string {
	t.Helper()
	var ss []string
	for _, s := range senders {
		ss = append(ss, fmt.Sprintf(`{"channel":%q,"start":%q,"stop":%q,"period":%q,"size_share":1}`,
			s.channel, injAt(s.start), injAt(s.stop), s.period.String()))
	}
	body := fmt.Sprintf(`{"experimental":true,"real":true,"payload_type":5,"exclude_channels":["81"],
 "start":%q,"end":%q,
 "labelling":{"pulse_merge_gap":"1m0s","min_period":"1m0s","max_period":"1h0m0s","min_pulses":8,
  "max_harmonic":3,"tolerance_abs":"30s","tolerance_rel":0.1},
 "min_size_share":0.9,"recipe":"crafted","input_lines":0,"considered":0,"skipped_payload_type":0,
 "skipped_no_channel":0,"skipped_excluded_channel":0,"skipped_out_of_window":0,"channels":0,"segments":0,
 "timing_runs":0,"size_rejected":0,"senders":[%s]}`, injAt(from), injAt(to), strings.Join(ss, ","))
	p := filepath.Join(dir, fmt.Sprintf("labels-real-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// r4Score runs score with -real-labels and returns the exit code, the
// parsed round-4 fields and the raw output.
func r4Score(t *testing.T, report string, extra ...string) (int, r4Metrics, string, string) {
	t.Helper()
	args := append([]string{"score", "-report", report, "-rule", "p474",
		"-start", injAt(0), "-end", injAt(24 * time.Hour)}, extra...)
	var stdout, stderr bytes.Buffer
	code := run(args, nil, &stdout, &stderr)
	var m r4Metrics
	if code == exitOK {
		if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
			t.Fatalf("metrics %q: %v", stdout.String(), err)
		}
	}
	return code, m, stdout.String(), stderr.String()
}

const r4Other = "v1|s=stream|pt=5|ch=1:11|rc=flood|rk=relay|hop=1:fd"

// An episode on a labelled real sender is not a false alarm; every other
// episode is. The match is the §8 rule: same channel hash (any route
// group), overlap with [start, stop + 4P] closed at both ends, and the
// period within the tolerance of k*P. The strict count is every episode.
func TestScoreRealLabelsExcuseOnlyMatchingEpisodes(t *testing.T) {
	dir := t.TempDir()
	p := time.Minute
	labels := r4RealLabels(t, dir, 0, 24*time.Hour, r4Sender{"7f", time.Hour, 2 * time.Hour, p})
	report := craftReport(t, dir,
		craftedEpisode{id: "inside", active: 70 * time.Minute, ended: 80 * time.Minute, periodNS: int64(p)},
		craftedEpisode{id: "other-route-group", key: "v1|s=stream|pt=5|ch=1:7f|rc=flood|rk=relay|hop=2:a1b2",
			active: 90 * time.Minute, ended: 95 * time.Minute, periodNS: int64(3 * p)},
		craftedEpisode{id: "at-stop+4P", active: 2*time.Hour + 4*p, ended: 3 * time.Hour, periodNS: int64(p)},
		craftedEpisode{id: "after-stop+4P", active: 2*time.Hour + 4*p + time.Second, ended: 3 * time.Hour, periodNS: int64(p)},
		// At P = 1 min the k = 1..3 windows of +/- 30 s are contiguous from
		// 30 s to 210 s; 4P is outside all of them.
		craftedEpisode{id: "fourth-harmonic", active: 100 * time.Minute, ended: 110 * time.Minute, periodNS: int64(4 * p)},
		craftedEpisode{id: "other-channel", key: r4Other, active: 70 * time.Minute, ended: 80 * time.Minute, periodNS: int64(p)},
		craftedEpisode{id: "no-period", active: 75 * time.Minute, ended: 80 * time.Minute},
	)
	code, m, _, stderr := r4Score(t, report, "-real-labels", labels)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if m.Episodes != 7 || m.TruePositives != 3 || m.FalsePositives != 4 || m.FalseAlarmsPerDay != 4 {
		t.Errorf("episodes %d, tp %d, fp %d, fa/day %v; want 7, 3, 4, 4", m.Episodes, m.TruePositives,
			m.FalsePositives, m.FalseAlarmsPerDay)
	}
	if m.PeriodMismatches != 1 {
		t.Errorf("period mismatches %d, want 1", m.PeriodMismatches)
	}
	if m.StrictFalsePositives == nil || *m.StrictFalsePositives != 7 ||
		m.StrictFalseAlarmsPerDay == nil || *m.StrictFalseAlarmsPerDay != 7 {
		t.Errorf("strict count %v / %v, want 7 and 7/day", m.StrictFalsePositives, m.StrictFalseAlarmsPerDay)
	}
	if m.Incidents != 1 || m.IncidentsDetected != 1 || m.Recall != 1 || len(m.PerIncident) != 1 ||
		m.PerIncident[0].Channel != "7f" || m.PerIncident[0].Episodes != 3 {
		t.Errorf("real-sender recall %+v", m)
	}
	if m.RealLabels != filepath.Base(labels) || m.Labels != "" {
		t.Errorf("provenance real_labels %q labels %q", m.RealLabels, m.Labels)
	}
}

// Real-sender recall is the share of labelled real senders with at least
// one episode. It carries no bar; a sender no episode found is reported,
// not penalised.
func TestScoreRealSenderRecall(t *testing.T) {
	dir := t.TempDir()
	labels := r4RealLabels(t, dir, 0, 24*time.Hour,
		r4Sender{"7f", time.Hour, 2 * time.Hour, 5 * time.Minute},
		r4Sender{"11", 10 * time.Hour, 12 * time.Hour, 5 * time.Minute})
	report := craftReport(t, dir, craftedEpisode{id: "e1", active: 90 * time.Minute, ended: 100 * time.Minute,
		periodNS: int64(5 * time.Minute)})
	code, m, _, stderr := r4Score(t, report, "-real-labels", labels)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if m.Incidents != 2 || m.IncidentsDetected != 1 || m.Recall != 0.5 || m.FalsePositives != 0 {
		t.Fatalf("recall %+v", m)
	}
}

// With no labelled sender, arm A is round 3's: every episode is a false
// alarm, and the strict count equals it. The real-sender recall is 0/0 and
// is left out rather than written as 0.
func TestScoreRealLabelsWithNoSenderIsTheStrictCount(t *testing.T) {
	dir := t.TempDir()
	labels := r4RealLabels(t, dir, 0, 24*time.Hour)
	report := craftReport(t, dir,
		craftedEpisode{id: "a", active: time.Hour, ended: 2 * time.Hour, periodNS: int64(time.Minute)},
		craftedEpisode{id: "b", key: r4Other, active: 3 * time.Hour, ended: 4 * time.Hour, periodNS: int64(time.Minute)})
	code, m, out, stderr := r4Score(t, report, "-real-labels", labels)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if m.FalsePositives != 2 || m.StrictFalsePositives == nil || *m.StrictFalsePositives != 2 {
		t.Fatalf("%+v", m)
	}
	if _, ok := r4Fields(t, out)["recall"]; ok {
		t.Errorf("recall written with no labelled sender: %s", out)
	}
}

// With labelled senders and no episode, the real-sender recall is 0/S, not
// 0/0: it is written as 0, and every sender is an undetected per_incident
// row. Only "no labelled sender" leaves the field out (round-4 draft §6).
func TestScoreRealLabelsWithSendersAndNoEpisodeWritesRecall(t *testing.T) {
	dir := t.TempDir()
	labels := r4RealLabels(t, dir, 0, 24*time.Hour, r4Sender{"7f", time.Hour, 2 * time.Hour, time.Minute})
	report := craftReport(t, dir)
	code, m, out, stderr := r4Score(t, report, "-real-labels", labels)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if m.Episodes != 0 || m.FalsePositives != 0 || m.StrictFalsePositives == nil || *m.StrictFalsePositives != 0 {
		t.Fatalf("%+v", m)
	}
	recall, ok := r4Fields(t, out)["recall"]
	if !ok || string(recall) != "0" {
		t.Errorf("recall with 1 labelled sender and 0 episodes: %q (present %v), want 0: %s", recall, ok, out)
	}
	if m.Incidents != 1 || m.IncidentsDetected != 0 || len(m.PerIncident) != 1 ||
		m.PerIncident[0].Bot != "real-1" || m.PerIncident[0].Detected {
		t.Errorf("per_incident %+v, want one undetected real-1", m.PerIncident)
	}
}

// r4Fields returns the top-level fields of a metrics file.
func r4Fields(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		t.Fatalf("metrics %q: %v", out, err)
	}
	return f
}

// A run without -real-labels writes exactly the fields it wrote before
// round 4: arm B and round 3's arm A are unchanged byte for byte.
func TestScoreWithoutRealLabelsIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	report := craftReport(t, dir, craftedEpisode{id: "a", active: time.Hour, ended: 2 * time.Hour, periodNS: int64(time.Minute)})
	labels := craftLabels(t, dir, time.Hour, 2*time.Hour, time.Minute)
	for _, extra := range [][]string{nil, {"-labels", labels}, {"-labels", labels, "-baseline", report}} {
		code, _, out, stderr := r4Score(t, report, extra...)
		if code != exitOK {
			t.Fatalf("%v: exit %d %s", extra, code, stderr)
		}
		for _, k := range []string{"real_labels", "strict_false_positives", "strict_false_alarms_per_day"} {
			if strings.Contains(out, k) {
				t.Errorf("%v: output carries %q", extra, k)
			}
		}
		// Recall stays written, 0 included, as round 3 wrote it.
		if _, ok := r4Fields(t, out)["recall"]; !ok {
			t.Errorf("%v: recall left out: %s", extra, out)
		}
	}
}

// -real-labels is arm A's ground truth and nothing else: it cannot be
// combined with injected labels, a baseline or a bot selection.
func TestScoreRealLabelsFlagCombinations(t *testing.T) {
	dir := t.TempDir()
	report := craftReport(t, dir, craftedEpisode{id: "a", active: time.Hour, ended: 2 * time.Hour, periodNS: int64(time.Minute)})
	real := r4RealLabels(t, dir, 0, 24*time.Hour)
	inj := craftLabels(t, dir, time.Hour, 2*time.Hour, time.Minute)
	if code, _, _, stderr := r4Score(t, report, "-real-labels", real); code != exitOK {
		t.Fatalf("-real-labels alone: exit %d %s", code, stderr)
	}
	for _, extra := range [][]string{
		{"-real-labels", real, "-labels", inj},
		{"-real-labels", real, "-baseline", report},
		{"-real-labels", real, "-bots", "bot-a"},
	} {
		if code, _, _, _ := r4Score(t, report, extra...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", extra, code, exitUsage)
		}
	}
}

// Each labels file is accepted by its own flag only, and the real labels
// must cover the whole score window: a sender outside the labelled window
// could not have been labelled, so its episodes would be miscounted.
func TestScoreRealLabelsFileIsValidated(t *testing.T) {
	dir := t.TempDir()
	report := craftReport(t, dir, craftedEpisode{id: "a", active: time.Hour, ended: 2 * time.Hour, periodNS: int64(time.Minute)})
	real := r4RealLabels(t, dir, 0, 24*time.Hour, r4Sender{"7f", time.Hour, 2 * time.Hour, time.Minute})
	inj := craftLabels(t, dir, time.Hour, 2*time.Hour, time.Minute)
	short := r4RealLabels(t, dir, time.Hour, 24*time.Hour)
	early := r4RealLabels(t, dir, 0, 23*time.Hour)
	b, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, from, to string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, bytes.Replace(b, []byte(from), []byte(to), 1), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, extra := range map[string][]string{
		"injected labels as real":   {"-real-labels", inj},
		"real labels as injected":   {"-labels", real},
		"labels start after -start": {"-real-labels", short},
		"labels end before -end":    {"-real-labels", early},
		"not real":                  {"-real-labels", write("notreal.json", `"real":true`, `"real":false`)},
		"unknown field":             {"-real-labels", write("unknown.json", `"recipe":`, `"bogus":1,"recipe":`)},
		"empty channel":             {"-real-labels", write("nochan.json", `"channel":"7f"`, `"channel":""`)},
		"zero period":               {"-real-labels", write("noperiod.json", `"period":"1m0s"`, `"period":"0s"`)},
		"share below the bar":       {"-real-labels", write("share.json", `"size_share":1`, `"size_share":0.5`)},
		"stop before start":         {"-real-labels", write("order.json", `"stop":"`+injAt(2*time.Hour), `"stop":"`+injAt(0))},
	} {
		if code, _, _, _ := r4Score(t, report, extra...); code != exitFail {
			t.Errorf("%s: exit %d, want %d", name, code, exitFail)
		}
	}
}

// End to end on a synthetic background shaped like round 3's arm A: a
// constant-size one-minute sender on a hash that was silent before, entering
// through two route groups in turn, plus a varied-size five-minute train on
// another hash and human chatter on a third. label -> replay -> score: the
// sender's episodes are excused, the varied-size train's are false alarms,
// and the strict count is every episode.
func TestPipelineLabelReplayScoreOnRealShapes(t *testing.T) {
	dir := t.TempDir()
	day := 24 * time.Hour
	var lines []r4Line
	lines = append(lines, r4Line{at: injT.Add(2 * time.Hour), channel: "56", payload: 51})
	lines = append(lines, r4Bot(r4BotOpts{channel: "56", start: injT.Add(6 * time.Hour), period: time.Minute,
		pulses: 600, jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, missed: 0.05, multipart: 0.1,
		sizes: []int{35}, hops: []string{"1:a1", "1:b2"}, hopEvery: 300, seed: 56})...)
	lines = append(lines, r4Bot(r4BotOpts{channel: "a2", start: injT.Add(2 * time.Hour), period: 5 * time.Minute,
		pulses: 144, jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, sizes: []int{35, 51, 67, 83},
		hops: []string{"1:c3"}, seed: 2})...)
	lines = append(lines, r4Chatter("d9", injT, day, 1, 2*time.Minute, r4HumanMix, 3)...)
	bg := filepath.Join(dir, "bg.jsonl")
	if err := os.WriteFile(bg, []byte(r4JSONL(t, lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	labels := filepath.Join(dir, "labels-real.json")
	var stdout, stderr bytes.Buffer
	args := r4LabelArgs(bg, "-out", labels)
	for i := range args {
		if args[i] == "-end" {
			args[i+1] = injAt(day)
		}
	}
	if code := run(args, nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("label exit %d: %s", code, stderr.String())
	}
	report := replayInto(t, dir, "a", replayConfigJSON(""), bg)
	code, m, _, errs := r4Score(t, report, "-real-labels", labels)
	if code != exitOK {
		t.Fatalf("score exit %d: %s", code, errs)
	}
	if m.StrictFalsePositives == nil {
		t.Fatal("no strict count")
	}
	t.Logf("real senders %d, episodes %d, tp %d, fp %d (%.2f/day), strict %d", m.Incidents, m.Episodes,
		m.TruePositives, m.FalsePositives, m.FalseAlarmsPerDay, *m.StrictFalsePositives)
	if m.Incidents != 1 || m.PerIncident[0].Channel != "56" || !m.PerIncident[0].Detected {
		t.Fatalf("real senders %+v", m.PerIncident)
	}
	if m.TruePositives == 0 || m.StrictFalsePositives == nil || *m.StrictFalsePositives != m.Episodes ||
		m.Episodes != m.TruePositives+m.FalsePositives {
		t.Fatalf("episodes %d, tp %d, fp %d, strict %v", m.Episodes, m.TruePositives, m.FalsePositives, m.StrictFalsePositives)
	}
	// Round 3's strict reading of the same report counts the sender's
	// episodes too.
	plain := scoreInto(t, report, "")
	if plain.FalsePositives != m.Episodes || m.FalsePositives >= plain.FalsePositives {
		t.Fatalf("strict fp %d, round-4 fp %d, episodes %d", plain.FalsePositives, m.FalsePositives, m.Episodes)
	}
	// Every remaining false alarm is the varied-size train's or the
	// chatter's: none is on the labelled sender's hash.
	var full scoreResult
	if err := json.Unmarshal([]byte(scoreOut(t, report, labels)), &full); err != nil {
		t.Fatal(err)
	}
	for _, c := range full.FalsePositivesByChannel {
		if c.Channel == "56" {
			t.Errorf("a false alarm on the labelled sender's hash: %+v", full.FalsePositivesByChannel)
		}
	}
	if m.FalsePositives == 0 {
		t.Errorf("the varied-size train raised no false alarm, so the pipeline did not test the size criterion")
	}
}

func scoreOut(t *testing.T, report, realLabels string) string {
	t.Helper()
	code, _, out, stderr := r4Score(t, report, "-real-labels", realLabels)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	return out
}
