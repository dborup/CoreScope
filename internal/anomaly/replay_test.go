package anomaly

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	updateGolden = flag.Bool("anomaly.update", false, "rewrite testdata/replay_golden.json")
	// Offline harness (EXPERIMENTAL): run a replay over local files, e.g.
	//
	//	go test -run TestOfflineReplay -anomaly.replay.events=/path/events.jsonl \
	//	  -anomaly.replay.config=/path/config.json -anomaly.replay.start=2030-01-01T00:00:00Z \
	//	  -anomaly.replay.end=2030-02-01T00:00:00Z -anomaly.replay.out=/path/report.json
	//
	// It reads only the given files and writes only the given output file.
	replayEvents = flag.String("anomaly.replay.events", "", "JSON Lines of normalized events")
	replayConfig = flag.String("anomaly.replay.config", "", "replay config JSON")
	replayStart  = flag.String("anomaly.replay.start", "", "RFC3339 start (inclusive)")
	replayEnd    = flag.String("anomaly.replay.end", "", "RFC3339 end (exclusive)")
	replayOut    = flag.String("anomaly.replay.out", "", "report output path")
)

func loadFixtureReplay(t *testing.T) ReplayReport {
	t.Helper()
	cb, err := os.ReadFile(filepath.Join("testdata", "replay_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseReplayConfig(cb)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join("testdata", "replay_events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rep, err := Replay(f, cfg, ReplayOptions{Start: t0, End: t0.Add(24 * time.Hour), MaxLineBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestReplayFixtureIsDeterministicAndMatchesGolden(t *testing.T) {
	a, _ := json.MarshalIndent(loadFixtureReplay(t), "", " ")
	b, _ := json.MarshalIndent(loadFixtureReplay(t), "", " ")
	if !bytes.Equal(a, b) {
		t.Fatal("replay output is not deterministic")
	}
	golden := filepath.Join("testdata", "replay_golden.json")
	if *updateGolden {
		if err := os.WriteFile(golden, append(a, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden file (run with -anomaly.update): %v", err)
	}
	if got := append(a, '\n'); !bytes.Equal(got, want) {
		// name the first differing line so a CI log shows the actual diff
		gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("replay output differs from testdata/replay_golden.json at line %d:\n got: %s\nwant: %s\n(run with -anomaly.update after reviewing)", i+1, g, w)
			}
		}
	}
}

func TestReplayFixtureContent(t *testing.T) {
	rep := loadFixtureReplay(t)
	in := rep.Input
	if in.Duplicates != 1 || in.Invalid != 1 || in.Late != 1 || in.OutOfRange != 1 || in.Accepted != 50 {
		t.Fatalf("input accounting %+v", in)
	}
	got := map[string]int{}
	for _, c := range rep.Candidates {
		if c.To == StateActive {
			got[c.Rule]++
		}
	}
	// the global burst activates once; the spread burst never reaches 10 per
	// stream in 5 minutes. The new-stream rule fires twice: for the periodic
	// stream that starts at the start of the data (left-censored) and for the
	// stream that appears later (not censored). The periodic rule fires for
	// the 5-minute series and for the later stream, which is regular too.
	want := map[string]int{"periodic_example": 2, "global_5m_example": 1, "new_stream_example": 2}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("activations %v, want %v", got, want)
		}
	}
	if got["stream_5m_example"] != 0 {
		t.Fatalf("stream rule fired on a spread burst: %v", got)
	}
	var censored, fresh int
	for _, c := range rep.Candidates {
		if c.Rule == "new_stream_example" && c.To == StateActive {
			if c.NewStream.Censored {
				censored++
				if !c.At.Before(t0.Add(time.Hour)) || c.Confidence != ConfidenceLow {
					t.Fatalf("censored candidate %+v", c)
				}
			} else {
				fresh++
			}
		}
	}
	if censored != 1 || fresh != 1 {
		t.Fatalf("censored %d fresh %d", censored, fresh)
	}
	for _, c := range rep.Candidates {
		if c.Rule == "global_5m_example" && (c.Key.Scope != ScopeGlobal || !c.Key.FirstHop.IsZero()) {
			t.Fatal("global candidate attributed to a route")
		}
	}
}

func TestReplayRejectsMissingBoundsAndLongLines(t *testing.T) {
	cb, _ := os.ReadFile(filepath.Join("testdata", "replay_config.json"))
	cfg, _ := ParseReplayConfig(cb)
	if _, err := Replay(strings.NewReader(""), cfg, ReplayOptions{MaxLineBytes: 100}); err == nil {
		t.Fatal("replay without explicit Start/End accepted")
	}
	long := strings.Repeat("x", 500) + "\n"
	if _, err := Replay(strings.NewReader(long), cfg, ReplayOptions{Start: t0, End: t0.Add(time.Hour), MaxLineBytes: 100}); err == nil {
		t.Fatal("over-long line accepted")
	}
	if _, err := ParseReplayConfig([]byte(`{"limits":{},"unknown":1}`)); err == nil {
		t.Fatal("unknown config field accepted")
	}
	if _, err := ParseReplayConfig([]byte(`{}`)); err == nil {
		t.Fatal("empty replay config accepted")
	}
}

func TestOfflineReplay(t *testing.T) {
	if *replayEvents == "" {
		t.Skip("offline harness: pass -anomaly.replay.events/-config/-start/-end/-out to run")
	}
	cb, err := os.ReadFile(*replayConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseReplayConfig(cb)
	if err != nil {
		t.Fatal(err)
	}
	start, err1 := time.Parse(time.RFC3339, *replayStart)
	end, err2 := time.Parse(time.RFC3339, *replayEnd)
	if err1 != nil || err2 != nil {
		t.Fatal("start and end are required RFC3339 times")
	}
	f, err := os.Open(*replayEvents)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rep, err := Replay(f, cfg, ReplayOptions{Start: start, End: end, MaxLineBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(rep, "", " ")
	if *replayOut == "" {
		t.Fatal("-anomaly.replay.out is required")
	}
	if err := os.WriteFile(*replayOut, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("replay: %+v, %d candidates", rep.Input, len(rep.Candidates))
}

func replayFixtureConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := ParseReplayConfig([]byte(`{"limits":{"max_keys_per_scope":100,"state_ttl":"2h","dedup_horizon":"2h",
		"max_dedup_ids":1000,"reorder_delay":"0s","max_reorder_buffer":10,"max_window_entries":100,
		"max_candidates_per_call":1,"max_pending_candidates":3},"expected":"include",
		"rate":[{"name":"r","scope":"stream","window":"1m","threshold":1,"state":{"confirm_after":"0s","end_after_quiet":"1m","cooldown":"0s"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func replayLine(id string) string {
	return replayLineAt(id, "2030-01-07T12:00:00Z", "5c")
}

func replayLineAt(id, when, hop string) string {
	return `{"id":"` + id + `","time":"` + when + `","payload_type":5,"channel":"ab","route_class":"flood","route_kind":"relay","first_hop":"1:` + hop + `","wire_bytes":60}`
}

func TestReplayLineLimitIsExact(t *testing.T) {
	cfg := replayFixtureConfig(t)
	line := replayLine("a")
	opt := ReplayOptions{Start: t0, End: t0.Add(time.Hour), MaxLineBytes: len(line)}
	for _, in := range []string{line + "\n", line + "\r\n", line} {
		rep, err := Replay(strings.NewReader(in), cfg, opt)
		if err != nil || rep.Input.Accepted != 1 {
			t.Fatalf("line of exactly MaxLineBytes (%q terminator): %v %+v", in[len(line):], err, rep.Input)
		}
	}
	opt.MaxLineBytes = len(line) - 1
	if _, err := Replay(strings.NewReader(line+"\n"), cfg, opt); err == nil {
		t.Fatal("line longer than MaxLineBytes accepted")
	}
}

func TestReplayRejectsTrailingData(t *testing.T) {
	cfg := replayFixtureConfig(t)
	in := replayLine("a") + ` {"x":1}` + "\n" + replayLine("b") + "garbage\n" + replayLine("c") + "]\n" + replayLine("d") + "}\n"
	rep, err := Replay(strings.NewReader(in), cfg, ReplayOptions{Start: t0, End: t0.Add(time.Hour), MaxLineBytes: 4096})
	if err != nil || rep.Input.Invalid != 4 || rep.Input.Accepted != 0 {
		t.Fatalf("trailing data: %v %+v", err, rep.Input)
	}
	for _, tail := range []string{` {"limits":{}}`, `]`, `}`} {
		if _, err := ParseReplayConfig([]byte(`{"limits":{}}` + tail)); err == nil || !strings.Contains(err.Error(), "trailing") {
			t.Fatalf("config with trailing %q: %v", tail, err)
		}
	}
	if _, err := Replay(strings.NewReader(""), cfg, ReplayOptions{Start: t0, End: t0.Add(time.Hour), MaxLineBytes: math.MaxInt}); err == nil {
		t.Fatal("MaxLineBytes = MaxInt accepted")
	}
}

func TestReplayRejectsAnEndOutsideTheValidRange(t *testing.T) {
	cfg := replayFixtureConfig(t)
	_, err := Replay(strings.NewReader(""), cfg, ReplayOptions{Start: t0, End: time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), MaxLineBytes: 100})
	if err == nil {
		t.Fatal("End beyond the valid range accepted (its deadlines would silently not run)")
	}
}

// The harness drains after every call: each line yields up to 3
// candidates (a quiet end and two opening transitions) with a per-call cap
// of 1 and a pending buffer of 3, so without draining the backlog would
// grow by 2 per line and overflow.
func TestReplayDrainsSoTheCandidateBufferDoesNotOverflow(t *testing.T) {
	cfg := replayFixtureConfig(t)
	var in strings.Builder
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		when := t0.Add(time.Duration(i) * 2 * time.Minute).Format(time.RFC3339)
		in.WriteString(replayLineAt(id, when, "0"+string(rune('1'+i))) + "\n")
	}
	rep, err := Replay(strings.NewReader(in.String()), cfg, ReplayOptions{Start: t0, End: t0.Add(time.Hour), MaxLineBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stats.CandidatesDropped != 0 || rep.CoverageReduced {
		t.Fatalf("harness dropped candidates: %+v", rep.Stats)
	}
	// 5 streams x (threshold_crossed, confirmed, quiet end)
	if len(rep.Candidates) != 15 {
		t.Fatalf("candidates %d", len(rep.Candidates))
	}
}
