package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #474, phase 3: injection-based evaluation. Every bot, channel byte
// and time here is synthetic.

var injT = time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)

func injAt(d time.Duration) string { return injT.Add(d).UTC().Format(time.RFC3339) }

// specOpts are the knobs the tests vary; everything else is fixed below.
type specOpts struct {
	seed              uint64
	channel           string
	period            string
	stopAfter         time.Duration
	jitterSD          string
	missedFraction    float64
	multipartFraction float64
	payloadSpread     int
	holes             string // JSON array body, e.g. {"start":..,"duration":"35m"}
	extraBots         string // JSON for further bots, with a leading comma
	leadBots          string // JSON for bots before bot-a, with a trailing comma
	minPulses         int
}

func (o specOpts) with() specOpts {
	if o.channel == "" {
		o.channel = "7f"
	}
	if o.period == "" {
		o.period = "5m"
	}
	if o.stopAfter == 0 {
		o.stopAfter = 4 * time.Hour
	}
	if o.jitterSD == "" {
		o.jitterSD = "0s"
	}
	if o.holes == "" {
		o.holes = "[]"
	}
	if o.minPulses == 0 {
		o.minPulses = 8
	}
	return o
}

// specJSON is the one spec shape the tests use, with the September bot's
// shape as its default (5 min, 3 s jitter, multi-part pulses).
func specJSON(o specOpts) string {
	o = o.with()
	return fmt.Sprintf(`{
 "seed": %d,
 "final_after": "30s",
 "labelling": {"pulse_merge_gap": "60s", "min_period": "1m", "max_period": "1h",
  "min_pulses": %d, "max_harmonic": 3, "tolerance_abs": "30s", "tolerance_rel": 0.1},
 "bots": [%s{
  "name": "bot-a",
  "channel": %q,
  "payload_type": 5,
  "route_class": "flood",
  "route_kind": "relay",
  "first_hop": "1:fd",
  "full_route": "1:fd00",
  "wire_bytes": 60,
  "payload_bytes": 35,
  "payload_bytes_spread": %d,
  "start": %q,
  "stop": %q,
  "period": %q,
  "jitter_sd": %q,
  "jitter_max": "10s",
  "missed_fraction": %v,
  "multipart_fraction": %v,
  "multipart_max_extra": 2,
  "multipart_max_delay": "55s",
  "holes": %s
 }%s]
}`, o.seed, o.minPulses, o.leadBots, o.channel, o.payloadSpread, injAt(0), injAt(o.stopAfter), o.period, o.jitterSD,
		o.missedFraction, o.multipartFraction, o.holes, o.extraBots)
}

// backgroundJSONL builds a sparse, non-periodic background: n events on
// channel 11 at irregular gaps, through two first hops.
func backgroundJSONL(n int) string {
	var b strings.Builder
	step := []time.Duration{37 * time.Second, 4 * time.Minute, 71 * time.Second, 13 * time.Minute}
	hops := []string{"1:0b", "2:0c01"}
	t := injT
	for i := 0; i < n; i++ {
		t = t.Add(step[i%len(step)])
		pb := 23
		rec := anomaly.ReplayRecord{ID: fmt.Sprintf("tx%d", i+1), Time: t, FinalAt: t.Add(30 * time.Second),
			PayloadType: 5, Channel: "11", RouteClass: "flood", RouteKind: "relay",
			FirstHop: hops[i%len(hops)], FullRoute: hops[i%len(hops)] + "00", WireBytes: 55,
			PayloadBytes: &pb, Traffic: "unclassified", Decryption: "unknown"}
		j, err := json.Marshal(rec)
		if err != nil {
			panic(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

// injectRun writes the spec and the background into a fresh directory, runs
// inject and returns the output lines, the labels and the summary.
type injectResult struct {
	dir     string
	outPath string
	labPath string
	lines   []string
	labels  InjectLabels
	summary injectSummary
	stderr  string
}

func injectInto(t *testing.T, spec, background string, extra ...string) injectResult {
	t.Helper()
	res, code := tryInject(t, spec, background, extra...)
	if code != 0 {
		t.Fatalf("inject exit %d: %s", code, res.stderr)
	}
	return res
}

func tryInject(t *testing.T, spec, background string, extra ...string) (injectResult, int) {
	t.Helper()
	dir := t.TempDir()
	r := injectResult{dir: dir, outPath: filepath.Join(dir, "events.jsonl"), labPath: filepath.Join(dir, "labels.json")}
	specPath := filepath.Join(dir, "spec.json")
	bgPath := filepath.Join(dir, "background.jsonl")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bgPath, []byte(background), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"inject", "-spec", specPath, "-input", bgPath, "-out", r.outPath,
		"-labels", r.labPath, "-max-injected", "100000", "-max-line-bytes", "65536"}, extra...)
	var stdout, stderr bytes.Buffer
	code := run(args, nil, &stdout, &stderr)
	r.stderr = stderr.String()
	if stdout.Len() != 0 {
		t.Fatalf("inject wrote %d bytes to stdout", stdout.Len())
	}
	if code != 0 {
		return r, code
	}
	out, err := os.ReadFile(r.outPath)
	if err != nil {
		t.Fatal(err)
	}
	r.lines = strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	lb, err := os.ReadFile(r.labPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lb, &r.labels); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(r.stderr), &r.summary); err != nil {
		t.Fatalf("summary %q: %v", r.stderr, err)
	}
	return r, code
}

// injectedLines returns the injected lines of a run, parsed.
func injectedLines(t *testing.T, r injectResult) []anomaly.ReplayRecord {
	t.Helper()
	var out []anomaly.ReplayRecord
	for _, l := range r.lines {
		var rec anomaly.ReplayRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		if strings.HasPrefix(rec.ID, injPrefix) {
			out = append(out, rec)
		}
	}
	return out
}

// Every injected line is marked, unique, valid as an event, and carries
// exactly the configured wire fields. The background is copied through byte
// for byte, and the merged output is in data-time order.
func TestInjectMarksAndMergesWithoutTouchingTheBackground(t *testing.T) {
	bg := backgroundJSONL(40)
	r := injectInto(t, specJSON(specOpts{seed: 1, multipartFraction: 0.3}), bg)
	ids := map[string]bool{}
	var last time.Time
	injected, passed := 0, 0
	var bgLines []string
	for _, l := range r.lines {
		var rec anomaly.ReplayRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		if ids[rec.ID] {
			t.Fatalf("duplicate id %s", rec.ID)
		}
		ids[rec.ID] = true
		if rec.Time.Before(last) {
			t.Fatalf("output is not in data-time order at %s", rec.ID)
		}
		last = rec.Time
		if !strings.HasPrefix(rec.ID, injPrefix) {
			passed++
			bgLines = append(bgLines, l)
			continue
		}
		injected++
		// The marker is the literal "inj-<bot>-<pulse>-<part>": an export
		// numbers its lines tx1, tx2, ..., so no real line can carry it.
		if !strings.HasPrefix(rec.ID, "inj-bot-a-") {
			t.Fatalf("injected id %q is not marked inj-<bot>-<pulse>-<part>", rec.ID)
		}
		if _, err := rec.Event(); err != nil {
			t.Fatalf("injected %s is not a valid event: %v", rec.ID, err)
		}
		if rec.Channel != "7f" || rec.FirstHop != "1:fd" || rec.FullRoute != "1:fd00" ||
			rec.PayloadType != 5 || rec.WireBytes != 60 || rec.PayloadBytes == nil || *rec.PayloadBytes != 35 {
			t.Fatalf("injected %s has unexpected wire fields: %+v", rec.ID, rec)
		}
		if got := rec.FinalAt.Sub(rec.Time); got != 30*time.Second {
			t.Fatalf("injected %s final_at offset %v", rec.ID, got)
		}
	}
	if passed != 40 || injected != r.summary.InjectedEvents || injected < 48 {
		t.Fatalf("passed %d background lines, injected %d (summary %d)", passed, injected, r.summary.InjectedEvents)
	}
	if strings.Join(bgLines, "\n")+"\n" != bg {
		t.Fatal("the background lines were not copied through byte for byte")
	}
	if r.summary.BackgroundLines != 40 || r.summary.LinesWritten != 40+injected || r.summary.BackgroundOutOfOrder != 0 {
		t.Fatalf("summary %+v", r.summary)
	}
}

// The same spec, seed and background give byte-identical output and labels.
func TestInjectIsDeterministic(t *testing.T) {
	spec := specJSON(specOpts{seed: 7, jitterSD: "3s", missedFraction: 0.05, multipartFraction: 0.3})
	bg := backgroundJSONL(20)
	a := injectInto(t, spec, bg)
	b := injectInto(t, spec, bg)
	ab, _ := os.ReadFile(a.outPath)
	bb, _ := os.ReadFile(b.outPath)
	if !bytes.Equal(ab, bb) {
		t.Fatal("two runs of the same spec differ")
	}
	al, _ := os.ReadFile(a.labPath)
	bl, _ := os.ReadFile(b.labPath)
	if !bytes.Equal(al, bl) {
		t.Fatal("two runs of the same spec produced different labels")
	}
	// A different seed moves the pulses but keeps the schedule.
	c := injectInto(t, specJSON(specOpts{seed: 8, jitterSD: "3s", missedFraction: 0.05, multipartFraction: 0.3}), bg)
	cb, _ := os.ReadFile(c.outPath)
	if bytes.Equal(ab, cb) {
		t.Fatal("a different seed gave identical output")
	}
	if a.summary.Bots[0].PulsesScheduled != c.summary.Bots[0].PulsesScheduled {
		t.Fatalf("the schedule changed with the seed: %d vs %d",
			a.summary.Bots[0].PulsesScheduled, c.summary.Bots[0].PulsesScheduled)
	}
}

// A bot's events depend only on the seed and its own fields, so adding a
// second bot leaves the first one's events untouched.
func TestInjectBotsAreIndependent(t *testing.T) {
	botB := `{
  "name": "bot-b", "channel": "22", "payload_type": 5, "route_class": "flood", "route_kind": "relay",
  "first_hop": "1:ee", "full_route": "1:ee00", "wire_bytes": 60, "payload_bytes": 35, "payload_bytes_spread": 0,
  "start": "` + injAt(0) + `", "stop": "` + injAt(time.Hour) + `", "period": "7m",
  "jitter_sd": "2s", "jitter_max": "10s", "missed_fraction": 0, "multipart_fraction": 0,
  "multipart_max_extra": 2, "multipart_max_delay": "55s", "holes": []}`
	linesOf := func(r injectResult, prefix string) string {
		var out []string
		for _, rec := range injectedLines(t, r) {
			if strings.HasPrefix(rec.ID, prefix) {
				b, _ := json.Marshal(rec)
				out = append(out, string(b))
			}
		}
		if len(out) == 0 {
			t.Fatalf("no injected lines with prefix %s", prefix)
		}
		return strings.Join(out, "\n")
	}
	alone := injectInto(t, specJSON(specOpts{seed: 3, jitterSD: "3s"}), "")
	after := injectInto(t, specJSON(specOpts{seed: 3, jitterSD: "3s", extraBots: "," + botB}), "")
	// bot-a's events must not depend on its position in the spec either.
	before := injectInto(t, specJSON(specOpts{seed: 3, jitterSD: "3s", leadBots: botB + ","}), "")
	want := linesOf(alone, injPrefix+"bot-a-")
	if got := linesOf(after, injPrefix+"bot-a-"); got != want {
		t.Fatal("adding a second bot after bot-a changed bot-a's events")
	}
	if got := linesOf(before, injPrefix+"bot-a-"); got != want {
		t.Fatal("adding a bot before bot-a changed bot-a's events")
	}
	if len(after.labels.Bots) != 2 || after.labels.Bots[1].Name != "bot-b" {
		t.Fatalf("labels %+v", after.labels.Bots)
	}
}

// The labelling recipe is applied to the emitted pulses: a clean train is
// one incident, a hole longer than max_harmonic periods splits it, and a
// train shorter than min_pulses is no incident at all.
func TestInjectLabelsIncidentsFromThePulses(t *testing.T) {
	clean := injectInto(t, specJSON(specOpts{seed: 11}), "")
	if len(clean.labels.Incidents) != 1 {
		t.Fatalf("clean train: %d incidents, want 1", len(clean.labels.Incidents))
	}
	inc := clean.labels.Incidents[0]
	if inc.Channel != "7f" || inc.Bot != "bot-a" || time.Duration(inc.Period) != 5*time.Minute || inc.Pulses != 48 {
		t.Fatalf("incident %+v", inc)
	}
	if !inc.Start.Equal(injT) || !inc.Stop.Equal(injT.Add(235*time.Minute)) {
		t.Fatalf("incident span %s..%s", inc.Start, inc.Stop)
	}
	// A 35-minute hole is 7 periods: more than max_harmonic, so the gap
	// across it fits no k*P and the run is split in two.
	holed := injectInto(t, specJSON(specOpts{seed: 11,
		holes: `[{"start": "` + injAt(100*time.Minute) + `", "duration": "35m"}]`}), "")
	if len(holed.labels.Incidents) != 2 {
		t.Fatalf("holed train: %d incidents, want 2: %+v", len(holed.labels.Incidents), holed.labels.Incidents)
	}
	if holed.summary.Bots[0].PulsesInHole != 7 {
		t.Fatalf("pulses in hole: %d, want 7", holed.summary.Bots[0].PulsesInHole)
	}
	// Seven pulses are fewer than min_pulses 8: no incident, but the events
	// are still injected.
	short := injectInto(t, specJSON(specOpts{seed: 11, stopAfter: 35 * time.Minute}), "")
	if len(short.labels.Incidents) != 0 || short.summary.Bots[0].PulsesEmitted != 7 {
		t.Fatalf("short train: %d incidents, %d pulses", len(short.labels.Incidents), short.summary.Bots[0].PulsesEmitted)
	}
}

// Multi-part pulses are extra events inside the merge gap, so they add
// events without adding pulses or incidents.
func TestInjectMultipartPulsesStayOnePulse(t *testing.T) {
	r := injectInto(t, specJSON(specOpts{seed: 5, multipartFraction: 1}), "")
	st := r.summary.Bots[0]
	if st.MultipartPulses != st.PulsesEmitted || st.Events <= st.PulsesEmitted {
		t.Fatalf("stats %+v", st)
	}
	if len(r.labels.Incidents) != 1 || r.labels.Incidents[0].Pulses != st.PulsesEmitted {
		t.Fatalf("incidents %+v for %d pulses", r.labels.Incidents, st.PulsesEmitted)
	}
	// Every extra part lands within multipart_max_delay of its pulse.
	byPulse := map[string][]time.Time{}
	for _, rec := range injectedLines(t, r) {
		p := rec.ID[:strings.LastIndex(rec.ID, "-")]
		byPulse[p] = append(byPulse[p], rec.Time)
	}
	for p, ts := range byPulse {
		if len(ts) < 2 {
			t.Fatalf("pulse %s has %d parts", p, len(ts))
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
		if d := ts[len(ts)-1].Sub(ts[0]); d > 55*time.Second {
			t.Fatalf("pulse %s spans %v", p, d)
		}
	}
	// With the fraction at 0 there are no extra parts at all.
	none := injectInto(t, specJSON(specOpts{seed: 5}), "")
	if st := none.summary.Bots[0]; st.MultipartPulses != 0 || st.Events != st.PulsesEmitted {
		t.Fatalf("multipart_fraction 0 produced parts: %+v", st)
	}
}

// missed_fraction drops pulses, and never the first one.
func TestInjectMissedPulses(t *testing.T) {
	all := injectInto(t, specJSON(specOpts{seed: 9, missedFraction: 1}), "")
	st := all.summary.Bots[0]
	if st.PulsesEmitted != 1 || st.PulsesMissed != st.PulsesScheduled-1 {
		t.Fatalf("missed_fraction 1: %+v", st)
	}
	if len(all.labels.Incidents) != 0 {
		t.Fatalf("a single pulse is no incident: %+v", all.labels.Incidents)
	}
	some := injectInto(t, specJSON(specOpts{seed: 9, missedFraction: 0.5}), "")
	if st := some.summary.Bots[0]; st.PulsesMissed == 0 || st.PulsesEmitted == st.PulsesScheduled {
		t.Fatalf("missed_fraction 0.5: %+v", st)
	}
}

// Jitter stays inside jitter_max and does not accumulate: a pulse is always
// within the clamp of its nominal grid time.
func TestInjectJitterIsClampedAndDoesNotDrift(t *testing.T) {
	r := injectInto(t, specJSON(specOpts{seed: 13, jitterSD: "8s"}), "")
	seen := false
	for _, rec := range injectedLines(t, r) {
		var pulse, part int
		if _, err := fmt.Sscanf(strings.TrimPrefix(rec.ID, injPrefix+"bot-a-"), "%d-%d", &pulse, &part); err != nil {
			t.Fatalf("id %s: %v", rec.ID, err)
		}
		off := rec.Time.Sub(injT.Add(time.Duration(pulse) * 5 * time.Minute))
		if off > 10*time.Second || off < -10*time.Second {
			t.Fatalf("pulse %d is %v off its nominal time", pulse, off)
		}
		if off != 0 {
			seen = true
		}
	}
	if !seen {
		t.Fatal("jitter_sd 8s produced no offset at all")
	}
}

// The tool writes only the two files it was given, refuses to overwrite
// either of them, and leaves nothing behind when it fails.
func TestInjectWritesOnlyItsTwoFiles(t *testing.T) {
	spec := specJSON(specOpts{seed: 2})
	r := injectInto(t, spec, backgroundJSONL(5))
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"background.jsonl", "events.jsonl", "labels.json", "spec.json"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("directory holds %v", names)
	}
	// Running again in the same directory refuses to overwrite.
	args := []string{"inject", "-spec", filepath.Join(r.dir, "spec.json"), "-input", filepath.Join(r.dir, "background.jsonl"),
		"-out", r.outPath, "-labels", r.labPath, "-max-injected", "100000", "-max-line-bytes", "65536"}
	var stdout, stderr bytes.Buffer
	if code := run(args, nil, &stdout, &stderr); code != exitFail {
		t.Fatalf("overwriting -out: exit %d", code)
	}
	// A labels file that already exists leaves no half-written output.
	out2 := filepath.Join(r.dir, "events2.jsonl")
	args[6] = out2
	if code := run(args, nil, &stdout, &stderr); code != exitFail {
		t.Fatalf("overwriting -labels: exit %d", code)
	}
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Fatalf("a failed run left %s behind (%v)", out2, err)
	}
}

// -max-injected is a hard bound: exceeding it fails the run and writes
// nothing.
func TestInjectMaxInjectedIsEnforced(t *testing.T) {
	r, code := tryInject(t, specJSON(specOpts{seed: 4}), "", "-max-injected", "10")
	if code != exitFail {
		t.Fatalf("exit %d, want %d", code, exitFail)
	}
	if !strings.Contains(r.stderr, "max-injected") {
		t.Fatalf("stderr %q", r.stderr)
	}
	if _, err := os.Stat(r.outPath); !os.IsNotExist(err) {
		t.Fatal("output file written despite the bound")
	}
	if _, err := os.Stat(r.labPath); !os.IsNotExist(err) {
		t.Fatal("labels file written despite the bound")
	}
}

// A background line that already carries an injected id is refused, so the
// marker stays unambiguous.
func TestInjectRefusesPreInjectedBackground(t *testing.T) {
	bg := backgroundJSONL(2) + `{"id":"inj-bot-a-0-0","time":"` + injAt(time.Hour) +
		`","payload_type":5,"channel":"7f","route_class":"flood","route_kind":"relay","first_hop":"1:fd","full_route":"1:fd00","wire_bytes":60,"traffic":"unclassified","decryption":"unknown"}` + "\n"
	r, code := tryInject(t, specJSON(specOpts{seed: 4}), bg)
	if code != exitFail || !strings.Contains(r.stderr, "injected id") {
		t.Fatalf("exit %d, stderr %q", code, r.stderr)
	}
}

// A background line without a usable time is an error, not a silently
// misplaced line.
func TestInjectRefusesUnreadableBackgroundLine(t *testing.T) {
	for _, bad := range []string{"not json", `{"id":"tx9"}`, `{"id":"tx9","time":"yesterday"}`} {
		r, code := tryInject(t, specJSON(specOpts{seed: 4}), bad+"\n")
		if code != exitFail {
			t.Fatalf("%q: exit %d, stderr %q", bad, code, r.stderr)
		}
	}
}

// Required flags are required; -spec, -input, -out, -labels, -max-injected
// and -max-line-bytes all have no default.
func TestInjectRequiresExplicitFlags(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.json")
	bgPath := filepath.Join(dir, "bg.jsonl")
	if err := os.WriteFile(specPath, []byte(specJSON(specOpts{seed: 1})), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bgPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	full := []string{"inject", "-spec", specPath, "-input", bgPath, "-out", filepath.Join(dir, "o.jsonl"),
		"-labels", filepath.Join(dir, "l.json"), "-max-injected", "1000", "-max-line-bytes", "4096"}
	for i := 1; i < len(full); i += 2 {
		args := append(append([]string{}, full[:i]...), full[i+2:]...)
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != exitUsage {
			t.Fatalf("without %s: exit %d", full[i], code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(append(append([]string{}, full...), "extra"), nil, &stdout, &stderr); code != exitUsage {
		t.Fatalf("with a positional argument: exit %d", code)
	}
}

// The spec has no defaults and no room for anything the tool does not know:
// a missing required field, a field whose zero value would be silently
// assumed, and an unknown field are all errors.
func TestInjectSpecRejectsMissingAndUnknownFields(t *testing.T) {
	base := specJSON(specOpts{seed: 1, multipartFraction: 0.3})
	for _, key := range []string{`"seed": 1,`, `"final_after": "30s",`, `"payload_type": 5,`,
		`"jitter_sd": "0s",`, `"missed_fraction": 0,`, `"multipart_fraction": 0.3,`,
		`"multipart_max_extra": 2,`, `"wire_bytes": 60,`, `"period": "5m",`} {
		if !strings.Contains(base, key) {
			t.Fatalf("test fixture no longer contains %s", key)
		}
		if _, code := tryInject(t, strings.Replace(base, key, "", 1), ""); code != exitFail {
			t.Errorf("without %s: exit %d", key, code)
		}
	}
	for name, spec := range map[string]string{
		"unknown field": strings.Replace(base, `"seed": 1,`, `"seed": 1, "db": "/tmp/meshcore.db",`, 1),
		"trailing data": base + "\n{}",
		"no bots":       strings.Replace(base, `"bots": [{`, `"bots2": [], "bots": [{`, 1),
		"bad channel":   strings.Replace(base, `"channel": "7f"`, `"channel": "7F"`, 1),
		"bad bot name":  strings.Replace(base, `"name": "bot-a"`, `"name": "Bot A"`, 1),
		"stop before start": strings.Replace(base, `"stop": "`+injAt(4*time.Hour)+`"`,
			`"stop": "`+injAt(-time.Hour)+`"`, 1),
		"jitter at half the period": strings.Replace(base, `"jitter_max": "10s"`, `"jitter_max": "150s"`, 1),
		"bad labelling":             strings.Replace(base, `"min_pulses": 8`, `"min_pulses": 1`, 1),
		"duplicate bot": strings.Replace(base, `"holes": []
 }]`, `"holes": []
 }, {"name": "bot-a", "channel": "22", "payload_type": 5, "route_class": "flood", "route_kind": "relay",
  "first_hop": "1:ee", "full_route": "1:ee00", "wire_bytes": 60, "payload_bytes": 35, "payload_bytes_spread": 0,
  "start": "`+injAt(0)+`", "stop": "`+injAt(time.Hour)+`", "period": "7m", "jitter_sd": "0s",
  "jitter_max": "10s", "missed_fraction": 0, "multipart_fraction": 0, "multipart_max_extra": 2,
  "multipart_max_delay": "55s", "holes": []}]`, 1),
	} {
		if _, code := tryInject(t, spec, ""); code != exitFail {
			t.Errorf("%s: exit %d", name, code)
		}
	}
}

// The labels file carries the ground truth and nothing else: it is marked
// synthetic, and it holds no ids, routes, hops, observers or text.
func TestInjectLabelsAreKeyFreeAndMarkedSynthetic(t *testing.T) {
	r := injectInto(t, specJSON(specOpts{seed: 6, multipartFraction: 0.3}), backgroundJSONL(5))
	if !r.labels.Experimental || !r.labels.Synthetic || r.labels.Seed != 6 {
		t.Fatalf("labels header %+v", r.labels)
	}
	raw, err := os.ReadFile(r.labPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"fd00", "observer", "tx1", injPrefix, "raw_hex", "public_key"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Fatalf("labels file carries %q:\n%s", banned, raw)
		}
	}
	// It does carry the channel hash, the times and the period.
	if !bytes.Contains(raw, []byte(`"channel": "7f"`)) || !bytes.Contains(raw, []byte(`"period": "5m0s"`)) {
		t.Fatalf("labels file is missing the ground truth:\n%s", raw)
	}
	// Since #474 round 2 it also carries the bot's stream key, which
	// scoring needs to tell a bot apart from real traffic on the same
	// channel hash. The key holds the payload type, the channel hash, the
	// route class and kind and the first hop - fields the export itself
	// carries - and nothing else: no whole route, no ids, no observers.
	if !bytes.Contains(raw, []byte(`"key": "`+scoreKey+`"`)) {
		t.Fatalf("labels file is missing the stream key:\n%s", raw)
	}
}
