package anomaly

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Issue #457, phase 1: the replay JSON config exposes
// end_after_missed_pulses (train tracking from #447), and RecordFromEvent
// writes an Event as a replay line, so an export can produce the input that
// Replay validates. Values are named test fixtures, not recommendations.

// trackReplayConfig is the JSON form of trackRule() with an optional
// end_after_missed_pulses field.
func trackReplayConfig(missed string) []byte {
	field := ""
	if missed != "" {
		field = `"end_after_missed_pulses": ` + missed + `,`
	}
	return []byte(`{
 "limits": {"max_keys_per_scope": 10000, "state_ttl": "48h", "dedup_horizon": "6h", "max_dedup_ids": 1000000,
  "reorder_delay": "0s", "max_reorder_buffer": 64, "max_window_entries": 4096,
  "max_candidates_per_call": 1000, "max_pending_candidates": 10000},
 "expected": "include",
 "periodic": [{"name": "per", "scope": "stream", "min_period": "1m", "max_period": "1h",
  "burst_gap": "15s", "jitter_abs": "10s", "jitter_rel": 0.02, "max_missing": 2, "min_pulses": 8,
  "history_len": 32, "min_coverage": 0.6, "max_jitter_fraction": 0.5, "max_chance": 0.01, ` + field + `
  "state": {"confirm_after": "0s", "end_after_quiet": "2h", "cooldown": "0s"}}]
}`)
}

func TestReplayConfigExposesEndAfterMissedPulses(t *testing.T) {
	cfg, err := ParseReplayConfig(trackReplayConfig("3"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Periodic[0].EndAfterMissedPulses; got != 3 {
		t.Fatalf("EndAfterMissedPulses = %d, want 3", got)
	}
	cfg, err = ParseReplayConfig(trackReplayConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Periodic[0].EndAfterMissedPulses; got != 0 {
		t.Fatalf("unset EndAfterMissedPulses = %d, want 0 (tracking off)", got)
	}
}

func TestReplayConfigEndAfterMissedPulsesIsValidated(t *testing.T) {
	for _, v := range []string{"-1", "65", "1.5", `"3"`} {
		if _, err := ParseReplayConfig(trackReplayConfig(v)); err == nil {
			t.Fatalf("end_after_missed_pulses %s accepted", v)
		}
	}
	if _, err := ParseReplayConfig(trackReplayConfig("64")); err != nil {
		t.Fatalf("64 rejected: %v", err)
	}
}

// replayLines writes events as replay JSON Lines with RecordFromEvent.
func replayLines(t testing.TB, evs []Event) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, e := range evs {
		line, err := json.Marshal(RecordFromEvent(e))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(line, '\n'))
	}
	return b.Bytes()
}

// The JSON field reaches the detector: with K = 3 the replayed train ends
// K*P + tol after its last pulse (as TestTrackingEndsKMissedPulsesAfterTheLastPulse
// does on the library API); unset, the episode ends by quiet after the last
// detection, which on this train (with multi-part pulses) is hours before
// the stop.
func TestReplayTrackingFromJSONConfigReportsTheStop(t *testing.T) {
	in := replayLines(t, pulseTrain(t, "a", 0, 60, true))
	last := at(59 * 5 * time.Minute)
	end := func(missed string) time.Time {
		cfg, err := ParseReplayConfig(trackReplayConfig(missed))
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Replay(bytes.NewReader(in), cfg, ReplayOptions{Start: t0, End: t0.Add(24 * time.Hour), MaxLineBytes: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Input.Accepted == 0 || rep.Input.Invalid != 0 {
			t.Fatalf("input %+v", rep.Input)
		}
		ends := filter(rep.Candidates, "per", StateEnded)
		if len(ends) == 0 {
			t.Fatalf("missed=%q: no end:\n%s", missed, describe(rep.Candidates))
		}
		return ends[len(ends)-1].At
	}
	if got, want := end("3"), last.Add(3*5*time.Minute+10*time.Second); !got.Equal(want) {
		t.Fatalf("tracked end at %v, want %v", got.Sub(t0), want.Sub(t0))
	}
	if got := end(""); !got.Before(last) {
		t.Fatalf("untracked end at %v: expected the quiet-based end before the last pulse (%v)", got.Sub(t0), last.Sub(t0))
	}
}

// RecordFromEvent is the inverse of ReplayRecord.Event: every valid line of
// the replay fixture survives the round trip unchanged.
func TestRecordFromEventRoundTripsTheFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "replay_events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var rec ReplayRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		e, err := rec.Event()
		if err != nil {
			continue
		}
		back, err := RecordFromEvent(e).Event()
		if err != nil {
			t.Fatalf("line %q: round trip invalid: %v", sc.Text(), err)
		}
		if e.FinalAt.IsZero() {
			e.FinalAt = e.Time // zero means FinalAt == Time
		}
		if !reflect.DeepEqual(back, e) {
			t.Fatalf("round trip changed the event:\n got %+v\nwant %+v", back, e)
		}
		n++
	}
	if n < 50 {
		t.Fatalf("only %d fixture lines round-tripped", n)
	}
}

func TestRecordFromEventFormat(t *testing.T) {
	pb := 35
	e := Event{ID: "tx", Time: t0, FinalAt: t0.Add(5 * time.Second), PayloadType: 5, Channel: mustChan(t, 0x0a),
		RouteClass: RouteClassTransportFlood, RouteKind: RouteKindRelay, FirstHop: mustHop(t, 0xfd, 0x12),
		FullRoute: mustRoute(t, 2, 0xfd, 0x12, 0x00, 0x01), WireBytes: 60, PayloadBytes: pb, PayloadKnown: true,
		Truncated: true, Observations: 3}
	got, err := json.Marshal(RecordFromEvent(e))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"tx","time":"2030-01-07T12:00:00Z","final_at":"2030-01-07T12:00:05Z","payload_type":5,` +
		`"channel":"0a","route_class":"tflood","route_kind":"relay","first_hop":"2:fd12","full_route":"2:fd120001",` +
		`"wire_bytes":60,"payload_bytes":35,"traffic":"unclassified","decryption":"unknown","truncated":true}`
	if string(got) != want {
		t.Fatalf("record\n got %s\nwant %s", got, want)
	}
	// no channel, no hop, no route, unknown payload size
	e = Event{ID: "d", Time: t0, PayloadType: 2, RouteClass: RouteClassDirect, RouteKind: RouteKindDirect}
	got, _ = json.Marshal(RecordFromEvent(e))
	for _, frag := range []string{`"channel":""`, `"first_hop":""`, `"full_route":""`, `"final_at":"2030-01-07T12:00:00Z"`} {
		if !strings.Contains(string(got), frag) {
			t.Fatalf("record %s lacks %s", got, frag)
		}
	}
	if strings.Contains(string(got), "payload_bytes") {
		t.Fatalf("unknown payload size written: %s", got)
	}
}
