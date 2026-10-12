package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Round-2 review tests for the observer-metadata bounds (#338, PR #386).
//
// observer_metadata_bounds_test.go pinned the raw-packet path and the
// UpsertObserverAt display bounds. The reviewer's mutation run showed both
// pins were too narrow:
//
//   - N1: moving the handleMessage id gate below the neighbors+status dispatch
//     left the whole suite green — only the packet path was pinned.
//   - N2: stripping every bound from UpsertObserverRetained (the path a
//     retained status takes, which is the common one on subscribe) left the
//     whole suite green.
//
// Both are test gaps, not code defects: the production behaviour is already
// correct on every path. These tests pin it so a regression cannot pass.
// countRows is the shared COUNT(*) helper from prune_chunked_test.go.

// --- N1: pin WHERE the id gate sits ----------------------------------------

// The gate is deliberately placed above the neighbors/status/packet dispatch,
// so one check covers every observer-bearing topic. A packet-only test cannot
// see that: moving the gate below the neighbors+status branches keeps it green
// while those two paths write rows under an unvalidated identity. Each subtest
// drives one dispatch branch and asserts the rows that branch would otherwise
// write, so the gate's position — not just its existence — is pinned.
func TestHandleMessage_InvalidObserverIDDroppedOnEveryDispatchPath(t *testing.T) {
	badID := strings.Repeat("a", maxObserverIDBytes+1)
	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	neighborPubkey := strings.Repeat("cd", 32)

	cases := []struct {
		name    string
		topic   string
		payload string
		// tables that must stay empty: the rows this branch writes keyed on
		// the observer id.
		tables []string
	}{
		{
			// A live status (no `status`, no `timestamp`, so statusIsLiveness
			// is true) reaches UpsertObserverAt, which INSERTs the observer
			// row, plus InsertMetrics for the noise-floor sample.
			name:    "status",
			topic:   "meshcore/SJC/" + badID + "/status",
			payload: `{"origin":"Hostile Observer","noise_floor":-100}`,
			tables:  []string{"observers", "observer_metrics"},
		},
		{
			// The neighbors branch never INSERTs an observers row
			// (TouchObserverNeighborsReport is UPDATE-only), so counting
			// observers alone would pass even with the gate gone. The rows it
			// does write keyed on the id are the neighbor snapshot and its
			// SNR history.
			name:    "neighbors",
			topic:   "meshcore/SJC/" + badID + "/neighbors",
			payload: `{"timestamp":"2026-10-13T10:00:00Z","neighbors":[{"pubkey":"` + neighborPubkey + `","status":"responded","scopes":"SJC","snr":-7.5,"heard_secs_ago":12}]}`,
			tables:  []string{"observer_neighbors", "observer_neighbor_metrics"},
		},
		{
			// The path the original test covered, kept here so all three
			// branches are asserted side by side.
			name:    "packets",
			topic:   "meshcore/SJC/" + badID + "/packets",
			payload: `{"raw":"` + rawHex + `"}`,
			tables:  []string{"observers", "observations"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, source := newTestContext(t)
			msg := &mockMessage{topic: tc.topic, payload: []byte(tc.payload)}
			handleMessage(store, "test", source, msg, nil, nil, &Config{})
			for _, table := range tc.tables {
				if n := countRows(t, store, table); n != 0 {
					t.Errorf("%s = %d rows, want 0 (an invalid observer id must be dropped before the %s dispatch)", table, n, tc.name)
				}
			}
		})
	}
}

// Counterpart to the above: a VALID id must still reach each branch, so the
// gate cannot be "fixed" by dropping everything.
func TestHandleMessage_ValidObserverIDReachesEveryDispatchPath(t *testing.T) {
	id := "kpabap" // the fixture's shortest real, non-pubkey operator id
	neighborPubkey := strings.Repeat("cd", 32)

	t.Run("status", func(t *testing.T) {
		store, source := newTestContext(t)
		msg := &mockMessage{
			topic:   "meshcore/SJC/" + id + "/status",
			payload: []byte(`{"origin":"Kings Island Control","noise_floor":-100}`),
		}
		handleMessage(store, "test", source, msg, nil, nil, &Config{})
		if n := countRows(t, store, "observers"); n != 1 {
			t.Fatalf("observers = %d, want 1", n)
		}
		name, iata, _, _, _, _ := observerStrings(t, store, id)
		if name != "Kings Island Control" || iata != "SJC" {
			t.Fatalf("stored name=%q iata=%q, want both verbatim", name, iata)
		}
	})

	t.Run("neighbors", func(t *testing.T) {
		store, source := newTestContext(t)
		msg := &mockMessage{
			topic:   "meshcore/SJC/" + id + "/neighbors",
			payload: []byte(`{"timestamp":"2026-10-13T10:00:00Z","neighbors":[{"pubkey":"` + neighborPubkey + `","status":"responded","scopes":"SJC","snr":-7.5,"heard_secs_ago":12}]}`),
		}
		handleMessage(store, "test", source, msg, nil, nil, &Config{})
		if n := countRows(t, store, "observer_neighbors"); n != 1 {
			t.Fatalf("observer_neighbors = %d, want 1", n)
		}
	})
}

// --- N2: UpsertObserverRetained carries the same display bounds ------------

// The retained/replayed status path is the common one — the broker replays a
// retained status for every observer on every subscribe — and it goes through
// UpsertObserverRetained, not UpsertObserverAt. So that method gets its own
// assertion rather than relying on the UpsertObserverAt test.
func TestUpsertObserverRetainedBoundsDisplayMetadata(t *testing.T) {
	s := newTestStore(t)
	id := "obs-retained-bounds"
	// UpsertObserverRetained is UPDATE-only by design, so the row has to exist
	// first — in production a live message is what creates it.
	if err := s.UpsertObserverAt(id, "seed", "SJC", nil, ""); err != nil {
		t.Fatalf("seed UpsertObserverAt: %v", err)
	}

	bigModel := strings.Repeat("m", 200)
	bigFirmware := strings.Repeat("f", 200)
	bigClient := strings.Repeat("c", 200)
	bigRadio := strings.Repeat("r", 200) + "\r"
	bigName := strings.Repeat("n", 200) + "\x00"
	// Lowercase on purpose: the assertions below also pin that the pre-existing
	// ToUpper region semantics survive the normalization. No space padding —
	// normalizeObserverRegion truncates before it trims, so padding would eat
	// runes out of the cap (the reviewer's N5 note, deliberately unchanged).
	bigRegion := strings.Repeat("z", 200)

	meta := &ObserverMeta{
		Model:         &bigModel,
		Firmware:      &bigFirmware,
		ClientVersion: &bigClient,
		Radio:         &bigRadio,
	}
	if err := s.UpsertObserverRetained(id, bigName, bigRegion, meta); err != nil {
		t.Fatalf("UpsertObserverRetained: %v", err)
	}

	name, iata, model, firmware, client, radio := observerStrings(t, s, id)
	for _, c := range []struct {
		field string
		got   string
		want  int
	}{
		{"name", name, maxObserverNameRunes},
		{"iata", iata, maxObserverRegionRunes},
		{"model", model, maxObserverModelRunes},
		{"firmware", firmware, maxObserverFirmwareRunes},
		{"client_version", client, maxObserverClientVersionRunes},
		{"radio", radio, maxObserverRadioRunes},
	} {
		if n := utf8.RuneCountInString(c.got); n != c.want {
			t.Errorf("retained %s runes = %d, want %d (%q)", c.field, n, c.want, c.got)
		}
	}
	if iata != strings.Repeat("Z", maxObserverRegionRunes) {
		t.Errorf("retained iata = %q, want %d uppercase Z (TrimSpace+ToUpper preserved)", iata, maxObserverRegionRunes)
	}
	if strings.ContainsAny(name, "\x00") || strings.ContainsAny(radio, "\r") {
		t.Errorf("retained control bytes survived: name=%q radio=%q", name, radio)
	}

	// TrimSpace on a realistic (short) region, kept separate from the cap
	// assertion above so the two semantics are pinned independently.
	if err := s.UpsertObserverRetained(id, "ok", "  mry  ", nil); err != nil {
		t.Fatalf("UpsertObserverRetained (trim): %v", err)
	}
	if _, iata, _, _, _, _ := observerStrings(t, s, id); iata != "MRY" {
		t.Errorf("retained iata = %q, want MRY (TrimSpace+ToUpper preserved)", iata)
	}
}

// The retained path must not mangle valid metadata either.
func TestUpsertObserverRetainedValidMetadataUnchanged(t *testing.T) {
	s := newTestStore(t)
	id := strings.Repeat("ab", 32) // 64-char pubkey id
	if err := s.UpsertObserverAt(id, "seed", "SJC", nil, ""); err != nil {
		t.Fatalf("seed UpsertObserverAt: %v", err)
	}
	model := "Heltec V3"
	firmware := "1.12.0-test"
	client := "meshcore/1.14.1-letsmesh.net-dev-2026-03-20-abe6e046"
	radio := "910.5250244,62.5,7,5"
	meta := &ObserverMeta{Model: &model, Firmware: &firmware, ClientVersion: &client, Radio: &radio}
	if err := s.UpsertObserverRetained(id, "Key Route Inspector-BRK \U0001F5DD", "mry", meta); err != nil {
		t.Fatalf("UpsertObserverRetained: %v", err)
	}
	name, iata, gotModel, gotFirmware, gotClient, gotRadio := observerStrings(t, s, id)
	if name != "Key Route Inspector-BRK \U0001F5DD" {
		t.Errorf("retained name = %q, changed", name)
	}
	if iata != "MRY" {
		t.Errorf("retained iata = %q, want MRY", iata)
	}
	if gotModel != model || gotFirmware != firmware || gotClient != client || gotRadio != radio {
		t.Errorf("retained metadata changed: %q %q %q %q", gotModel, gotFirmware, gotClient, gotRadio)
	}
}
