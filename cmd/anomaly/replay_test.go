package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #457, phase 1: the offline replay command.

var libTestdata = filepath.Join("..", "..", "internal", "anomaly", "testdata")

func goldenArgs(extra ...string) []string {
	args := []string{"replay", "-config", filepath.Join(libTestdata, "replay_config.json"),
		"-input", filepath.Join(libTestdata, "replay_events.jsonl"),
		"-start", "2030-01-07T12:00:00Z", "-end", "2030-01-08T12:00:00Z", "-max-line-bytes", "65536"}
	return append(args, extra...)
}

// The command prints exactly the library's golden report for the golden
// inputs (the format is the one TestReplayFixtureIsDeterministicAndMatchesGolden pins).
func TestReplayCommandMatchesTheLibraryGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(libTestdata, "replay_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(goldenArgs(), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("report differs from replay_golden.json:\n%s", stdout.String())
	}
}

func TestReplayCommandReadsStdinAndWritesOut(t *testing.T) {
	want, _ := os.ReadFile(filepath.Join(libTestdata, "replay_golden.json"))
	in, _ := os.ReadFile(filepath.Join(libTestdata, "replay_events.jsonl"))
	out := filepath.Join(t.TempDir(), "report.json")
	args := goldenArgs("-out", out)
	args[4] = "-" // -input
	var stdout, stderr bytes.Buffer
	if code := run(args, bytes.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, want) || stdout.Len() != 0 {
		t.Fatalf("report file differs (err %v, stdout %d bytes)", err, stdout.Len())
	}
	if code := run(args, bytes.NewReader(in), &stdout, &stderr); code == 0 {
		t.Fatal("existing report overwritten")
	}
}

func TestReplayCommandRequiresExplicitBounds(t *testing.T) {
	full := goldenArgs()
	for i := 1; i < len(full); i += 2 {
		args := append(append([]string{}, full[:i]...), full[i+2:]...)
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Fatalf("without %s: exit %d, %d bytes of output", full[i], code, stdout.Len())
		}
	}
	for name, extra := range map[string][]string{
		"end before start": {"-end", "2030-01-07T11:00:00Z"},
		"bad time":         {"-start", "now"},
		"line cap zero":    {"-max-line-bytes", "0"},
		"positional":       {"x"},
		"missing config":   {"-config", filepath.Join(t.TempDir(), "none.json")},
		"missing input":    {"-input", filepath.Join(t.TempDir(), "none.jsonl")},
		"invalid config":   {"-config", filepath.Join(libTestdata, "replay_events.jsonl")},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append(goldenArgs(), extra...), nil, &stdout, &stderr); code == 0 || stdout.Len() != 0 {
			t.Fatalf("%s: exit %d, %d bytes of output", name, code, stdout.Len())
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	for _, args := range [][]string{nil, {"help-me"}, {"-x"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

// botConfig: one periodic rule on stream keys with train tracking (K = 3).
// Named test values, not recommendations.
const botConfig = `{
 "limits": {"max_keys_per_scope": 10000, "state_ttl": "48h", "dedup_horizon": "6h", "max_dedup_ids": 1000000,
  "reorder_delay": "0s", "max_reorder_buffer": 64, "max_window_entries": 4096,
  "max_candidates_per_call": 1000, "max_pending_candidates": 10000},
 "expected": "include",
 "periodic": [{"name": "bot", "scope": "stream", "min_period": "1m", "max_period": "1h",
  "burst_gap": "15s", "jitter_abs": "10s", "jitter_rel": 0.02, "max_missing": 2, "min_pulses": 8,
  "history_len": 32, "min_coverage": 0.6, "max_jitter_fraction": 0.5, "max_chance": 0.01,
  "end_after_missed_pulses": 3,
  "state": {"confirm_after": "0s", "end_after_quiet": "2h", "cooldown": "0s"}}]
}`

// End to end on a synthetic database: export, then replay. The 5-minute
// train is found on its route group (channel 0a, flood, first hop 0b) and
// the tracked episode ends K*P + tol after the last pulse. The irregular
// traffic on another route group raises nothing.
func TestExportThenReplayFindsThePeriodicRouteGroup(t *testing.T) {
	f := newFixtureDB(t, false)
	grp := grpPayload(0x0a, 20)
	for i := 0; i < 24; i++ {
		at := fxAt(10*time.Minute + time.Duration(i)*5*time.Minute)
		f.tx(fxHash(i), frameHex(1, 5, 1, []byte{0x0b, 0x01}, grp), 5,
			relay(at, 1, 5, 1, []byte{0x0b, 0x01}, grp),
			relay(at.Add(2*time.Second), 1, 5, 1, []byte{0x0c}, grp))
	}
	other := grpPayload(0x0d, 20)
	for i, off := range []int{3, 4, 11, 29, 30, 52, 77, 78, 90, 131} {
		f.tx(fxHash(500+i), frameHex(1, 5, 1, []byte{0x0e}, other), 5,
			relay(fxAt(time.Duration(off)*time.Minute+17*time.Second), 1, 5, 1, []byte{0x0e}, other))
	}
	dir := t.TempDir()
	events, report, cfg := filepath.Join(dir, "events.jsonl"), filepath.Join(dir, "report.json"), filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(cfg, []byte(botConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"export", "-db", f.path, "-start", "2030-03-01T00:00:00Z", "-end", "2030-03-01T06:00:00Z",
		"-settle", "10s", "-max-rows", "1000", "-out", events}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("export exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"replay", "-config", cfg, "-input", events, "-start", "2030-03-01T00:00:00Z",
		"-end", "2030-03-01T06:00:00Z", "-max-line-bytes", "4096", "-out", report}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr.String())
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Input      anomaly.ReplayInputStats
		Candidates []struct {
			Rule, Key, To string
			At            time.Time
		}
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Input.Accepted != 34 || rep.Input.Invalid != 0 {
		t.Fatalf("input %+v", rep.Input)
	}
	const key = "v1|s=stream|pt=5|ch=1:0a|rc=flood|rk=relay|hop=1:0b"
	var active, ended int
	last := fxAt(10*time.Minute + 23*5*time.Minute)
	for _, c := range rep.Candidates {
		if c.Key != key {
			t.Fatalf("candidate on another key: %+v", c)
		}
		switch c.To {
		case "active":
			active++
		case "ended":
			ended++
			if want := last.Add(3*5*time.Minute + 10*time.Second); !c.At.Equal(want) {
				t.Fatalf("ended at %v, want %v", c.At, want)
			}
		}
	}
	if active != 1 || ended != 1 {
		t.Fatalf("active %d, ended %d:\n%s", active, ended, strings.TrimSpace(string(b)))
	}
}
