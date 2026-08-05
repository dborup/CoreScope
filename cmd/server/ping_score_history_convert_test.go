package main

import (
	"encoding/json"
	"testing"
	"time"
)

// f64 is defined in store_tophops_test.go (same package).

// --- marshal/unmarshal relay pubkeys -----------------------------------

func TestMarshalRelayPubkeysJSON_DeterministicDedupSort(t *testing.T) {
	got := marshalRelayPubkeysJSON([]string{"zzz", "aaa", "mmm", "aaa", "zzz"})
	want := `["aaa","mmm","zzz"]`
	if got != want {
		t.Errorf("marshalRelayPubkeysJSON = %q, want %q", got, want)
	}
	// Different input order, same SET -- must produce byte-identical JSON.
	got2 := marshalRelayPubkeysJSON([]string{"mmm", "zzz", "aaa"})
	if got2 != want {
		t.Errorf("marshalRelayPubkeysJSON (reordered input) = %q, want %q", got2, want)
	}
}

func TestMarshalRelayPubkeysJSON_Empty(t *testing.T) {
	if got := marshalRelayPubkeysJSON(nil); got != "" {
		t.Errorf("marshalRelayPubkeysJSON(nil) = %q, want empty string", got)
	}
	if got := marshalRelayPubkeysJSON([]string{}); got != "" {
		t.Errorf("marshalRelayPubkeysJSON([]) = %q, want empty string", got)
	}
}

func TestUnmarshalRelayPubkeysJSON_RoundTrip(t *testing.T) {
	raw := marshalRelayPubkeysJSON([]string{"pk2", "pk1", "pk1"})
	got, err := unmarshalRelayPubkeysJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pk1", "pk2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func TestUnmarshalRelayPubkeysJSON_Empty(t *testing.T) {
	got, err := unmarshalRelayPubkeysJSON("")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestUnmarshalRelayPubkeysJSON_InvalidReturnsError(t *testing.T) {
	for _, raw := range []string{"not json", "{}", `["unterminated`, `123`, `"just a string"`} {
		if _, err := unmarshalRelayPubkeysJSON(raw); err == nil {
			t.Errorf("unmarshalRelayPubkeysJSON(%q): want error, got nil", raw)
		}
	}
}

// --- pingScoreHistoryEntryFromScore --------------------------------------

func TestPingScoreHistoryEntryFromScore_NilScoreIsUnscorable(t *testing.T) {
	trigger := pingTriggerRow{txID: 42, hash: "h1", channelHash: "#c", sender: "Alice", firstSeen: "2026-01-01T00:00:00Z"}
	fp := observationFingerprint{Count: 3, MaxID: 99}
	state := PingScoreHistoryEntryState{StableSince: "2026-01-02T00:00:00Z", Settled: true, DataPruned: false, LastDeepSweptAt: "2026-01-03T00:00:00Z"}
	computedAt := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)

	e := pingScoreHistoryEntryFromScore(trigger, nil, fp, state, computedAt)

	if !e.Unscorable {
		t.Error("Unscorable = false, want true")
	}
	if e.TxID != 42 || e.Hash != "h1" || e.ChannelHash != "#c" || e.Sender != "Alice" || e.Timestamp != "2026-01-01T00:00:00Z" {
		t.Errorf("trigger fields not carried through: %+v", e)
	}
	if e.StationCount != 0 || e.DeepestHops != 0 || e.DeepestPubkey != "" || e.FarthestKm != nil || e.AirtimeMs != nil || e.RelayPubkeysJSON != "" {
		t.Errorf("path facts should be zero-valued for an unscorable entry: %+v", e)
	}
	if e.FingerprintCount != 3 || e.FingerprintMaxID != 99 {
		t.Errorf("fingerprint not carried through: %+v", e)
	}
	if e.StableSince != state.StableSince || e.Settled != state.Settled || e.DataPruned != state.DataPruned || e.LastDeepSweptAt != state.LastDeepSweptAt {
		t.Errorf("state not carried through: %+v", e)
	}
	if e.ComputedAt != "2026-01-04T00:00:00Z" {
		t.Errorf("ComputedAt = %q, want 2026-01-04T00:00:00Z", e.ComputedAt)
	}
}

func TestPingScoreHistoryEntryFromScore_ScoredEntry(t *testing.T) {
	trigger := pingTriggerRow{txID: 7, hash: "h2", firstSeen: "2026-02-01T00:00:00Z"}
	score := &PingScore{
		StationCount:   3,
		DeepestHops:    2,
		DeepestPubkey:  "deep1",
		FarthestKm:     f64(123.4),
		FarthestPubkey: "far1",
		SpreadSeconds:  f64(60),
		AirtimeMs:      f64(500),
		RelayCount:     2,
		relayPubkeys:   []string{"r2", "r1"},
		firstPubkey:    "first1",
		// Names deliberately populated -- must NOT be persisted.
		DeepestName:  "Deep One",
		FarthestName: "Far One",
		firstName:    "First One",
	}
	computedAt := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)

	e := pingScoreHistoryEntryFromScore(trigger, score, observationFingerprint{}, PingScoreHistoryEntryState{}, computedAt)

	if e.Unscorable {
		t.Error("Unscorable = true, want false")
	}
	if e.StationCount != 3 || e.DeepestHops != 2 || e.DeepestPubkey != "deep1" {
		t.Errorf("path facts wrong: %+v", e)
	}
	if e.FarthestKm == nil || *e.FarthestKm != 123.4 || e.FarthestPubkey != "far1" {
		t.Errorf("farthest facts wrong: %+v", e)
	}
	if e.AirtimeMs == nil || *e.AirtimeMs != 500 || e.RelayCount != 2 {
		t.Errorf("airtime facts wrong: %+v", e)
	}
	if e.FirstPubkey != "first1" {
		t.Errorf("FirstPubkey = %q, want first1", e.FirstPubkey)
	}
	if e.RelayPubkeysJSON != `["r1","r2"]` {
		t.Errorf("RelayPubkeysJSON = %q, want deterministic sorted JSON", e.RelayPubkeysJSON)
	}
}

// --- materializePingScoreFromHistoryEntry --------------------------------

func TestMaterializePingScoreFromHistoryEntry_Unscorable(t *testing.T) {
	score, err := materializePingScoreFromHistoryEntry(PingScoreHistoryEntry{TxID: 1, Unscorable: true})
	if err != nil {
		t.Fatal(err)
	}
	if score != nil {
		t.Errorf("score = %+v, want nil", score)
	}
}

func TestMaterializePingScoreFromHistoryEntry_FullRoundTrip(t *testing.T) {
	e := PingScoreHistoryEntry{
		TxID: 5, Hash: "hh", Sender: "Bob", ChannelHash: "#c", Timestamp: "2026-03-01T00:00:00Z",
		StationCount: 4, DeepestHops: 3, DeepestPubkey: "dp",
		FarthestKm: f64(200), FarthestPubkey: "fp", SpreadSeconds: f64(45),
		AirtimeMs: f64(1000), RelayCount: 5, RelayPubkeysJSON: `["r1","r2"]`,
		FirstPubkey: "fip",
	}
	score, err := materializePingScoreFromHistoryEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if score == nil {
		t.Fatal("score = nil, want a value")
	}
	if score.Hash != "hh" || score.Sender != "Bob" || score.ChannelHash != "#c" || score.Timestamp != e.Timestamp {
		t.Errorf("trigger-ish fields wrong: %+v", score)
	}
	if score.StationCount != 4 || score.DeepestHops != 3 || score.DeepestPubkey != "dp" {
		t.Errorf("path facts wrong: %+v", score)
	}
	if score.FarthestKm == nil || *score.FarthestKm != 200 || score.FarthestPubkey != "fp" {
		t.Errorf("farthest facts wrong: %+v", score)
	}
	if score.AirtimeMs == nil || *score.AirtimeMs != 1000 || score.RelayCount != 5 {
		t.Errorf("airtime facts wrong: %+v", score)
	}
	if score.firstPubkey != "fip" {
		t.Errorf("firstPubkey = %q, want fip", score.firstPubkey)
	}
	if len(score.relayPubkeys) != 2 || score.relayPubkeys[0] != "r1" || score.relayPubkeys[1] != "r2" {
		t.Errorf("relayPubkeys = %v, want [r1 r2]", score.relayPubkeys)
	}
	// Names never sourced from history.
	if score.DeepestName != "" || score.FarthestName != "" || score.firstName != "" {
		t.Errorf("names should be empty after materialization, got Deepest=%q Farthest=%q first=%q", score.DeepestName, score.FarthestName, score.firstName)
	}
	// 200km / (1000ms/1000) = 200 km/s.
	if score.KmPerSecondAirtime == nil || *score.KmPerSecondAirtime != 200 {
		t.Errorf("KmPerSecondAirtime = %v, want 200", score.KmPerSecondAirtime)
	}
}

func TestMaterializePingScoreFromHistoryEntry_KmPerSecondAirtimeNilCases(t *testing.T) {
	base := PingScoreHistoryEntry{TxID: 1, StationCount: 1}
	cases := []struct {
		name       string
		farthestKm *float64
		airtimeMs  *float64
	}{
		{"nil FarthestKm", nil, f64(1000)},
		{"nil AirtimeMs", f64(50), nil},
		{"zero AirtimeMs", f64(50), f64(0)},
		{"negative AirtimeMs", f64(50), f64(-5)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := base
			e.FarthestKm = c.farthestKm
			e.AirtimeMs = c.airtimeMs
			score, err := materializePingScoreFromHistoryEntry(e)
			if err != nil {
				t.Fatal(err)
			}
			if score.KmPerSecondAirtime != nil {
				t.Errorf("KmPerSecondAirtime = %v, want nil", *score.KmPerSecondAirtime)
			}
		})
	}
}

func TestMaterializePingScoreFromHistoryEntry_InvalidRelayJSONReturnsError(t *testing.T) {
	e := PingScoreHistoryEntry{TxID: 9, StationCount: 1, RelayPubkeysJSON: "not valid json"}
	score, err := materializePingScoreFromHistoryEntry(e)
	if err == nil {
		t.Fatal("want an error for invalid relay_pubkeys_json")
	}
	if score != nil {
		t.Errorf("score = %+v, want nil alongside the error", score)
	}
}

// --- mergePingScoreHistoryEntry: table-driven ----------------------------

func TestMergePingScoreHistoryEntry(t *testing.T) {
	trigger := pingTriggerRow{txID: 10, hash: "hmerge", channelHash: "#c", sender: "Carol", firstSeen: "2026-04-01T00:00:00Z"}
	computedAt := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	update := PingScoreHistoryEntryState{StableSince: "2026-04-03T00:00:00Z", Settled: true, LastDeepSweptAt: "2026-04-04T00:00:00Z"}
	fp := observationFingerprint{Count: 5, MaxID: 500}

	successScore := &PingScore{
		StationCount: 3, DeepestHops: 2, DeepestPubkey: "d1",
		FarthestKm: f64(100), FarthestPubkey: "f1", SpreadSeconds: f64(30),
		AirtimeMs: f64(200), RelayCount: 4, relayPubkeys: []string{"r1", "r2"}, firstPubkey: "fi1",
	}

	t.Run("new entry, success replaces every path fact and clears Unscorable", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, Unscorable: true} // seeded via pingScoreHistoryEntryFromScore(nil)
		got := mergePingScoreHistoryEntry(existing, trigger, successScore, update, fp, computedAt)
		if got.Unscorable {
			t.Error("Unscorable = true, want false")
		}
		if got.StationCount != 3 || got.DeepestHops != 2 || got.DeepestPubkey != "d1" {
			t.Errorf("path facts wrong: %+v", got)
		}
		if got.AirtimeMs == nil || *got.AirtimeMs != 200 || got.RelayCount != 4 {
			t.Errorf("airtime should lock on first success: %+v", got)
		}
		if got.RelayPubkeysJSON != `["r1","r2"]` {
			t.Errorf("RelayPubkeysJSON = %q", got.RelayPubkeysJSON)
		}
	})

	t.Run("existing valid entry, success REPLACES path facts", func(t *testing.T) {
		existing := PingScoreHistoryEntry{
			TxID: 10, StationCount: 1, DeepestHops: 0, DeepestPubkey: "old",
			FarthestKm: f64(5), FarthestPubkey: "oldfar", AirtimeMs: f64(999), RelayCount: 1,
		}
		got := mergePingScoreHistoryEntry(existing, trigger, successScore, update, fp, computedAt)
		if got.StationCount != 3 || got.DeepestHops != 2 || got.DeepestPubkey != "d1" {
			t.Errorf("path facts not replaced: %+v", got)
		}
		if got.FarthestKm == nil || *got.FarthestKm != 100 || got.FarthestPubkey != "f1" {
			t.Errorf("farthest facts not replaced: %+v", got)
		}
		// AirtimeMs was already locked (999 > 0) -- must NOT change to 200.
		if got.AirtimeMs == nil || *got.AirtimeMs != 999 || got.RelayCount != 1 {
			t.Errorf("AirtimeMs/RelayCount must stay locked at the existing value: %+v", got)
		}
	})

	t.Run("existing valid entry, empty result NEVER downgrades path facts or Unscorable", func(t *testing.T) {
		existing := PingScoreHistoryEntry{
			TxID: 10, Unscorable: false, StationCount: 5, DeepestHops: 4, DeepestPubkey: "keep",
			FarthestKm: f64(77), FarthestPubkey: "keepfar", AirtimeMs: f64(50), RelayCount: 2,
			RelayPubkeysJSON: `["keep1","keep2"]`, FirstPubkey: "keepfirst",
		}
		got := mergePingScoreHistoryEntry(existing, trigger, nil, update, fp, computedAt)
		if got.Unscorable {
			t.Error("Unscorable = true, want false (was already successfully scored)")
		}
		if got.StationCount != 5 || got.DeepestHops != 4 || got.DeepestPubkey != "keep" {
			t.Errorf("path facts changed on an empty result: %+v", got)
		}
		if got.FarthestKm == nil || *got.FarthestKm != 77 || got.FarthestPubkey != "keepfar" {
			t.Errorf("farthest facts changed on an empty result: %+v", got)
		}
		if got.AirtimeMs == nil || *got.AirtimeMs != 50 || got.RelayCount != 2 {
			t.Errorf("airtime changed on an empty result: %+v", got)
		}
		if got.RelayPubkeysJSON != `["keep1","keep2"]` || got.FirstPubkey != "keepfirst" {
			t.Errorf("relay/first pubkeys changed on an empty result: %+v", got)
		}
	})

	t.Run("never-scored entry, empty result stays Unscorable", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, Unscorable: true}
		got := mergePingScoreHistoryEntry(existing, trigger, nil, update, fp, computedAt)
		if !got.Unscorable {
			t.Error("Unscorable = false, want true (never successfully scored, and this cycle also failed)")
		}
	})

	t.Run("AirtimeMs locked, later nil airtime never resets it", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, StationCount: 1, AirtimeMs: f64(300), RelayCount: 3}
		scoreNoAirtime := &PingScore{StationCount: 1, relayPubkeys: nil}
		got := mergePingScoreHistoryEntry(existing, trigger, scoreNoAirtime, update, fp, computedAt)
		if got.AirtimeMs == nil || *got.AirtimeMs != 300 || got.RelayCount != 3 {
			t.Errorf("AirtimeMs/RelayCount reset by a nil-airtime score: %+v", got)
		}
	})

	t.Run("AirtimeMs locked, later non-positive airtime never resets it", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, StationCount: 1, AirtimeMs: f64(300), RelayCount: 3}
		scoreZeroAirtime := &PingScore{StationCount: 1, AirtimeMs: f64(0), RelayCount: 9}
		got := mergePingScoreHistoryEntry(existing, trigger, scoreZeroAirtime, update, fp, computedAt)
		if got.AirtimeMs == nil || *got.AirtimeMs != 300 || got.RelayCount != 3 {
			t.Errorf("AirtimeMs/RelayCount reset by a non-positive-airtime score: %+v", got)
		}
	})

	t.Run("AirtimeMs unset, first positive value locks it", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, StationCount: 1, AirtimeMs: nil}
		got := mergePingScoreHistoryEntry(existing, trigger, successScore, update, fp, computedAt)
		if got.AirtimeMs == nil || *got.AirtimeMs != 200 || got.RelayCount != 4 {
			t.Errorf("AirtimeMs should lock to the first positive value: %+v", got)
		}
	})

	t.Run("AirtimeMs unset, non-positive new value does NOT lock", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, StationCount: 1, AirtimeMs: nil, RelayCount: 0}
		scoreZero := &PingScore{StationCount: 1, AirtimeMs: f64(-1), RelayCount: 7}
		got := mergePingScoreHistoryEntry(existing, trigger, scoreZero, update, fp, computedAt)
		if got.AirtimeMs != nil {
			t.Errorf("AirtimeMs = %v, want nil (never locked from a non-positive value)", *got.AirtimeMs)
		}
		if got.RelayCount != 0 {
			t.Errorf("RelayCount = %d, want 0 (must not change independently of AirtimeMs locking)", got.RelayCount)
		}
	})

	t.Run("DataPruned always carried through from existing, unmodified", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, StationCount: 1, DataPruned: true}
		got := mergePingScoreHistoryEntry(existing, trigger, successScore, update, fp, computedAt)
		if !got.DataPruned {
			t.Error("DataPruned = false, want true (must be carried through unchanged)")
		}
		existing2 := PingScoreHistoryEntry{TxID: 10, StationCount: 1, DataPruned: false}
		got2 := mergePingScoreHistoryEntry(existing2, trigger, nil, update, fp, computedAt)
		if got2.DataPruned {
			t.Error("DataPruned = true, want false (must be carried through unchanged)")
		}
	})

	t.Run("fingerprint/state fields always taken from parameters, never derived", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, FingerprintCount: 1, FingerprintMaxID: 1, StableSince: "old", Settled: false, LastDeepSweptAt: "old"}
		got := mergePingScoreHistoryEntry(existing, trigger, successScore, update, fp, computedAt)
		if got.FingerprintCount != 5 || got.FingerprintMaxID != 500 {
			t.Errorf("fingerprint not taken from parameter: %+v", got)
		}
		if got.StableSince != update.StableSince || !got.Settled || got.LastDeepSweptAt != update.LastDeepSweptAt {
			t.Errorf("state not taken from parameter: %+v", got)
		}
	})

	t.Run("trigger fields (hash/sender/channelHash/timestamp) always refreshed from trigger", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, Hash: "stale", Sender: "stale", ChannelHash: "stale", Timestamp: "stale"}
		got := mergePingScoreHistoryEntry(existing, trigger, nil, update, fp, computedAt)
		if got.Hash != trigger.hash || got.Sender != trigger.sender || got.ChannelHash != trigger.channelHash || got.Timestamp != trigger.firstSeen {
			t.Errorf("trigger fields not refreshed: %+v", got)
		}
	})

	t.Run("ComputedAt always set from the computedAt parameter", func(t *testing.T) {
		existing := PingScoreHistoryEntry{TxID: 10, ComputedAt: "old"}
		got := mergePingScoreHistoryEntry(existing, trigger, nil, update, fp, computedAt)
		if got.ComputedAt != "2026-04-02T00:00:00Z" {
			t.Errorf("ComputedAt = %q, want 2026-04-02T00:00:00Z", got.ComputedAt)
		}
	})
}

// TestMergePingScoreHistoryEntry_ResultIsJSONStable is a cheap sanity check
// that merged entries remain marshalable (used indirectly by
// UpsertAndDelete's callers) -- not a real behavioral test, just guards
// against an accidental field type change breaking persistence silently.
func TestMergePingScoreHistoryEntry_ResultIsJSONStable(t *testing.T) {
	trigger := pingTriggerRow{txID: 1, hash: "h", firstSeen: "2026-01-01T00:00:00Z"}
	score := &PingScore{StationCount: 1, AirtimeMs: f64(10), relayPubkeys: []string{"a"}}
	got := mergePingScoreHistoryEntry(PingScoreHistoryEntry{TxID: 1}, trigger, score, PingScoreHistoryEntryState{}, observationFingerprint{}, time.Now())
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("merged entry is not marshalable: %v", err)
	}
}
