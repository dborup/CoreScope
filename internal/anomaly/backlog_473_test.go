package anomaly

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Issue #473, phase 1: a key-free ingest-backlog flag on events and an
// opt-in rate-rule guard that leaves flagged events out of its counts.

// The flag of an event is the flag of its first observation (the one that
// sets Event.Time), in the streaming Normalizer and in the batch path.
func TestEventTakesTheBacklogFlagOfItsFirstObservation(t *testing.T) {
	mk := func(firstFlagged, laterFlagged bool) []Observation {
		a := obs(t, "tx", "a", at(time.Second), RouteClassFlood, 1, 0x11)
		b := obs(t, "tx", "b", at(2*time.Second), RouteClassFlood, 1, 0x22)
		a.IngestBacklog, b.IngestBacklog = firstFlagged, laterFlagged
		return []Observation{b, a} // input order must not matter
	}
	for _, c := range []struct{ first, later, want bool }{
		{true, false, true}, {false, true, false}, {true, true, true}, {false, false, false},
	} {
		os := mk(c.first, c.later)
		e, _, err := NormalizeTransmission(os, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if e.IngestBacklog != c.want {
			t.Fatalf("batch first=%v later=%v: flag %v, want %v", c.first, c.later, e.IngestBacklog, c.want)
		}
		n, err := NewNormalizer(normCfg())
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range []Observation{os[1], os[0]} { // arrival order
			if r := n.Add(o); r.Status != ObsAccepted {
				t.Fatalf("add: %v", r.Status)
			}
		}
		evs := n.Flush().Events
		if len(evs) != 1 || evs[0].IngestBacklog != c.want {
			t.Fatalf("stream first=%v later=%v: events %+v, want flag %v", c.first, c.later, evs, c.want)
		}
	}
}

// One observation (same ObsID) delivered twice with different flags is a
// conflict, not a duplicate: the flag is part of the observation.
func TestBacklogFlagIsPartOfTheObservation(t *testing.T) {
	n, err := NewNormalizer(normCfg())
	if err != nil {
		t.Fatal(err)
	}
	a := obs(t, "tx", "a", at(time.Second), RouteClassFlood, 1, 0x11)
	if r := n.Add(a); r.Status != ObsAccepted {
		t.Fatal(r.Status)
	}
	a.IngestBacklog = true
	if r := n.Add(a); r.Status != ObsConflict {
		t.Fatalf("same ObsID with another flag: %v, want conflict", r.Status)
	}
}

// The replay line carries the flag as "ingest_backlog": true, and omits it
// when false, so lines without it are byte-identical to the #457 format.
func TestReplayRecordCarriesTheBacklogFlag(t *testing.T) {
	line := `{"id":"x","time":"2030-01-07T12:00:00Z","payload_type":5,"channel":"0a","route_class":"flood",` +
		`"route_kind":"nopath","first_hop":"","full_route":"","wire_bytes":30,"traffic":"unclassified",` +
		`"decryption":"unknown","ingest_backlog":true}`
	var r ReplayRecord
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	e, err := r.Event()
	if err != nil || !e.IngestBacklog {
		t.Fatalf("event %+v, err %v: flag lost", e, err)
	}
	b, _ := json.Marshal(RecordFromEvent(e))
	if !bytes.HasSuffix(b, []byte(`,"ingest_backlog":true}`)) {
		t.Fatalf("record %s does not end with the flag", b)
	}
	e.IngestBacklog = false
	b, _ = json.Marshal(RecordFromEvent(e))
	if bytes.Contains(b, []byte("ingest_backlog")) {
		t.Fatalf("unflagged record %s names the flag", b)
	}
	for _, bad := range []string{`"yes"`, `1`, `null`} {
		var r ReplayRecord
		l := strings.Replace(line, `"ingest_backlog":true`, `"ingest_backlog":`+bad, 1)
		if bad == `null` {
			if err := json.Unmarshal([]byte(l), &r); err != nil || r.IngestBacklog {
				t.Fatalf("null: %v %v", err, r.IngestBacklog)
			}
			continue
		}
		if err := json.Unmarshal([]byte(l), &r); err == nil {
			t.Fatalf("ingest_backlog %s accepted", bad)
		}
	}
}

// The replay config exposes the guard as an optional rate field; omitted
// means false (the #457 configs stay valid and unchanged), and it is only
// a rate-rule field.
func TestReplayConfigExposesExcludeIngestBacklog(t *testing.T) {
	cfg := func(extra string) string {
		return `{"limits": {"max_keys_per_scope": 10, "state_ttl": "1h", "dedup_horizon": "1h", "max_dedup_ids": 10,
  "reorder_delay": "0s", "max_reorder_buffer": 4, "max_window_entries": 4, "max_candidates_per_call": 4,
  "max_pending_candidates": 4}, "expected": "include",
 "rate": [{"name": "r", "scope": "global", "window": "5m", "threshold": 3` + extra + `,
  "state": {"confirm_after": "0s", "end_after_quiet": "10m", "cooldown": "0s"}}]}`
	}
	for extra, want := range map[string]bool{"": false, `, "exclude_ingest_backlog": false`: false,
		`, "exclude_ingest_backlog": true`: true} {
		c, err := ParseReplayConfig([]byte(cfg(extra)))
		if err != nil {
			t.Fatalf("%q: %v", extra, err)
		}
		if c.Rate[0].ExcludeIngestBacklog != want {
			t.Fatalf("%q: guard %v, want %v", extra, c.Rate[0].ExcludeIngestBacklog, want)
		}
	}
	if _, err := ParseReplayConfig([]byte(cfg(`, "exclude_ingest_backlog": "true"`))); err == nil {
		t.Fatal("a string guard was accepted")
	}
	b, err := os.ReadFile(filepath.Join("testdata", "replay_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), `"name": "periodic_example",`, `"name": "periodic_example", "exclude_ingest_backlog": true,`, 1)
	if _, err := ParseReplayConfig([]byte(s)); err == nil {
		t.Fatal("exclude_ingest_backlog accepted on a periodic rule")
	}
}

func flagged(e Event) Event { e.IngestBacklog = true; return e }

// A guarded rule does not count flagged events; the same rule unguarded
// does. The guard is per rule: a guarded and an unguarded rule with the
// same window on the same key keep separate counts.
func TestRateGuardLeavesFlaggedEventsOutOfTheCount(t *testing.T) {
	guarded := rateRule("guarded", ScopeGlobal, 5*time.Minute, 5)
	guarded.ExcludeIngestBacklog = true
	plain := rateRule("plain", ScopeGlobal, 5*time.Minute, 5)
	var evs []Event
	for i := 0; i < 8; i++ { // 4 live, 4 backlog, interleaved: 8 >= 5, but 4 < 5
		e := relayEvent(t, fmt.Sprintf("e%d", i), at(time.Duration(i)*10*time.Second), 0x0a, 0x11)
		if i%2 == 1 {
			e = flagged(e)
		}
		evs = append(evs, e)
	}
	cs := run(t, newDet(t, Config{Rate: []RateRule{guarded, plain}}), evs)
	if len(filter(cs, "plain", StateActive)) != 1 {
		t.Fatalf("unguarded rule did not fire on 8 events:\n%s", describe(cs))
	}
	if g := filter(cs, "guarded", StateActive); len(g) != 0 {
		t.Fatalf("guarded rule fired on 4 live events (threshold 5):\n%s", describe(cs))
	}
	// one more live event reaches the guarded threshold, and its evidence
	// counts live events only
	evs = append(evs, relayEvent(t, "e8", at(90*time.Second), 0x0a, 0x11))
	cs = run(t, newDet(t, Config{Rate: []RateRule{guarded, plain}}), evs)
	g := filter(cs, "guarded", StateActive)
	if len(g) != 1 || g[0].Rate == nil || g[0].Rate.Count != 5 || g[0].TriggerID != "e8" {
		t.Fatalf("guarded activation %+v:\n%s", g, describe(cs))
	}
	if p := filter(cs, "plain", StateActive); len(p) != 1 || p[0].Rate.Count != 5 || p[0].TriggerID != "e4" {
		t.Fatalf("plain activation %+v", p)
	}
}

// A flagged event is not a signal for a guarded rule: it cannot keep a
// guarded episode hot, so the episode ends EndAfterQuiet after the last
// live signal even while backlog events keep arriving. That holds alone
// and when an unguarded rule of the same scope makes the flagged events
// reach the key.
func TestRateGuardFlaggedEventsDoNotExtendAnEpisode(t *testing.T) {
	r := rateRule("g", ScopeGlobal, time.Minute, 2)
	r.ExcludeIngestBacklog = true
	evs := []Event{relayEvent(t, "a", at(0), 0x0a, 0x11), relayEvent(t, "b", at(time.Second), 0x0a, 0x11)}
	for i := 0; i < 30; i++ { // backlog for 15 minutes after the live pair
		evs = append(evs, flagged(relayEvent(t, fmt.Sprintf("bl%d", i), at(time.Duration(i+1)*30*time.Second), 0x0a, 0x11)))
	}
	for _, rules := range [][]RateRule{{r}, {r, rateRule("plain", ScopeGlobal, 2*time.Minute, 1000)}} {
		cs := run(t, newDet(t, Config{Rate: rules}), evs)
		ends := filter(cs, "g", StateEnded)
		if len(ends) != 1 || !ends[0].At.Equal(at(time.Second+10*time.Minute)) {
			t.Fatalf("%d rules: guarded episode end %v, want 10m after the last live signal:\n%s", len(rules), ends, describe(cs))
		}
	}
}

// strip returns the candidates without DecidedAt, which follows the
// decision clock (every offered event's FinalAt) and so legitimately moves
// with input that a rule ignores.
func strip(cs []Candidate) []Candidate {
	out := make([]Candidate, len(cs))
	for i, c := range cs {
		c.DecidedAt = time.Time{}
		out[i] = c
	}
	return out
}

// randomStorms builds a random event stream with storms on two channels,
// a random share of it flagged.
func randomStorms(t *testing.T, rng *rand.Rand) []Event {
	var evs []Event
	tt := at(0)
	for i := 0; i < 1500; i++ {
		if rng.Intn(40) == 0 {
			tt = tt.Add(time.Duration(rng.Intn(20)) * time.Minute) // a quiet gap
		}
		tt = tt.Add(time.Duration(rng.Intn(20)) * time.Second)
		e := relayEvent(t, fmt.Sprintf("r%d", i), tt, byte(0x0a+rng.Intn(2)), byte(0x11+rng.Intn(3)))
		if rng.Intn(3) == 0 {
			e.PayloadType = 4
			e.Channel = ChannelHash{}
		}
		e.IngestBacklog = rng.Intn(3) == 0
		evs = append(evs, e)
	}
	return evs
}

func guardConfig(guard bool) Config {
	var rs []RateRule
	for _, r := range []RateRule{
		rateRule("g_5m", ScopeGlobal, 5*time.Minute, 12), rateRule("g_1m", ScopeGlobal, time.Minute, 5),
		rateRule("s_5m", ScopeStream, 5*time.Minute, 6), rateRule("c_2m", ScopeChannel, 2*time.Minute, 4),
	} {
		r.ExcludeIngestBacklog = guard
		if r.Name == "g_1m" {
			r.PayloadTypes = []PayloadType{5}
		}
		rs = append(rs, r)
	}
	return Config{Rate: rs}
}

// A config of guarded rate rules behaves exactly as the same rules
// unguarded on the input with every flagged event removed: same
// candidates (up to DecidedAt), same keys and episodes.
func TestRateGuardEqualsReplayWithoutFlaggedEvents(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		evs := randomStorms(t, rng)
		var live []Event
		for _, e := range evs {
			if !e.IngestBacklog {
				live = append(live, e)
			}
		}
		got := run(t, newDet(t, guardConfig(true)), evs)
		want := run(t, newDet(t, guardConfig(false)), live)
		if len(want) < 10 {
			t.Fatalf("seed %d: only %d candidates; the scenario is too weak", seed, len(want))
		}
		if !reflect.DeepEqual(strip(got), strip(want)) {
			t.Fatalf("seed %d: guarded run differs from the run without flagged events\n got:\n%s\nwant:\n%s",
				seed, describe(got), describe(want))
		}
	}
}

// Without the guard the flag changes nothing: the same input with and
// without flags gives identical candidates, for rate, new-stream and
// periodic rules alike (the replay fixture config).
func TestUnguardedRulesIgnoreTheBacklogFlag(t *testing.T) {
	cb, err := os.ReadFile(filepath.Join("testdata", "replay_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseReplayConfig(cb)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "replay_events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	marked := markEveryOtherLine(src)
	opt := ReplayOptions{Start: t0, End: t0.Add(24 * time.Hour), MaxLineBytes: 1 << 16}
	a, err := Replay(bytes.NewReader(src), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Replay(bytes.NewReader(marked), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.MarshalIndent(a, "", " ")
	jb, _ := json.MarshalIndent(b, "", " ")
	if len(a.Candidates) == 0 || !bytes.Equal(ja, jb) {
		t.Fatalf("flags changed an unguarded replay (%d candidates)", len(a.Candidates))
	}
	// and the guard does change it, so the flags above were read
	for i := range cfg.Rate {
		cfg.Rate[i].ExcludeIngestBacklog = true
	}
	c, err := Replay(bytes.NewReader(marked), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	if jc, _ := json.MarshalIndent(c, "", " "); bytes.Equal(ja, jc) {
		t.Fatal("guarded replay of flagged input equals the unguarded one")
	}
}

// markEveryOtherLine adds "ingest_backlog": true to every other line.
func markEveryOtherLine(src []byte) []byte {
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(src))
	for i := 0; sc.Scan(); i++ {
		line := sc.Bytes()
		if i%2 == 0 && bytes.HasSuffix(line, []byte("}")) {
			line = append(append([]byte(nil), line[:len(line)-1]...), []byte(`,"ingest_backlog":true}`)...)
		}
		out.Write(append(line, '\n'))
	}
	return out.Bytes()
}

// Flagged events that only guarded rules would see do not touch the
// scope at all: no key is created for them.
func TestRateGuardFlaggedEventsCreateNoKeys(t *testing.T) {
	r := rateRule("g", ScopeStream, time.Minute, 100)
	r.ExcludeIngestBacklog = true
	d := newDet(t, Config{Rate: []RateRule{r}})
	run(t, d, []Event{flagged(relayEvent(t, "a", at(0), 0x0a, 0x11)), flagged(relayEvent(t, "b", at(time.Second), 0x0b, 0x12))})
	if s := d.Snapshot(); s.Keys[ScopeStream] != 0 || s.Accepted != 2 {
		t.Fatalf("stats %+v: flagged events created keys", s)
	}
	run(t, d, []Event{relayEvent(t, "c", at(2*time.Second), 0x0a, 0x11)})
	if s := d.Snapshot(); s.Keys[ScopeStream] != 1 {
		t.Fatalf("keys %v after one live event", s.Keys)
	}
}
