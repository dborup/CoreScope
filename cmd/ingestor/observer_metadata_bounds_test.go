package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Tests for the observer-metadata bounds (#338). Grouped by the issue's
// acceptance criteria: identity rejection (never truncation), display-text
// normalization (controls stripped, rune-safe truncation), oversized fields,
// distinct long-prefix ids, and existing valid observers unchanged.

// --- Identity: observer ids are rejected, never rewritten -------------------

func TestObserverIDRejectReason(t *testing.T) {
	pubkey := strings.Repeat("ab", 32) // 64-char lowercase-hex node key
	cases := []struct {
		name string
		id   string
		want string
	}{
		{"empty is left alone (orthogonal edge)", "", ""},
		{"short non-pubkey operator id", "kpabap", ""},
		{"full 64-char pubkey", pubkey, ""},
		{"at the byte limit", strings.Repeat("a", maxObserverIDBytes), ""},
		{"one byte over the limit", strings.Repeat("a", maxObserverIDBytes+1), "too-long"},
		{"far over the limit", strings.Repeat("a", maxObserverIDBytes*4), "too-long"},
		{"control byte (newline)", "obs\n01", "control-char"},
		{"control byte (CR)", "obs\r01", "control-char"},
		{"DEL byte", "obs\x7f01", "control-char"},
		{"invalid utf-8", "obs\xff01", "invalid-utf8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := observerIDRejectReason(tc.id); got != tc.want {
				t.Fatalf("observerIDRejectReason(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// Two distinct ids that share a long prefix but differ in the last byte must
// both pass validation unchanged — the identity contract is "reject or keep
// verbatim", never "truncate to a shared cap".
func TestObserverIDLongPrefixStaysDistinct_Validation(t *testing.T) {
	a := strings.Repeat("a", maxObserverIDBytes-1) + "X"
	b := strings.Repeat("a", maxObserverIDBytes-1) + "Y"
	if a == b {
		t.Fatal("test setup: ids are not distinct")
	}
	if r := observerIDRejectReason(a); r != "" {
		t.Fatalf("id a rejected: %q", r)
	}
	if r := observerIDRejectReason(b); r != "" {
		t.Fatalf("id b rejected: %q", r)
	}
}

// --- Display text: strip controls, truncate on rune boundary ---------------

// Unicode: truncation must land on a rune boundary, never splitting a
// multi-byte character.
func TestNormalizeObserverText_UnicodeNeverSplitsRune(t *testing.T) {
	// 10 four-byte runes (U+1F5DD, the "old key" emoji used in the fixture).
	emoji := "\U0001F5DD"
	in := strings.Repeat(emoji, 10)
	got := normalizeObserverText(in, 4)
	if !utf8.ValidString(got) {
		t.Fatalf("normalizeObserverText produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("rune count = %d, want 4 (%q)", n, got)
	}
	if got != strings.Repeat(emoji, 4) {
		t.Fatalf("got %q, want 4 emoji", got)
	}
	// A cap that falls "inside" a rune's bytes must still not split it: a
	// mixed ASCII+multibyte string truncated to 5 runes keeps 5 whole runes.
	mixed := "ab" + strings.Repeat(emoji, 5) // 7 runes, 2+20 = 22 bytes
	got = normalizeObserverText(mixed, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("mixed produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("mixed rune count = %d, want 5", n)
	}
}

// Control characters are stripped (and do not count against the rune cap).
func TestNormalizeObserverText_StripsControls(t *testing.T) {
	in := "ab\x00c\td\ne\rf\x7fg"
	got := normalizeObserverText(in, 64)
	if got != "abcdefg" {
		t.Fatalf("got %q, want %q", got, "abcdefg")
	}
	// A radio line like the fixture's trailing-CR value is cleaned.
	if got := normalizeObserverText("910.5250244,62.5,7,5\r", 64); got != "910.5250244,62.5,7,5" {
		t.Fatalf("radio CR not stripped: %q", got)
	}
	// All-control input collapses to empty.
	if got := normalizeObserverText("\x01\x02\x03", 64); got != "" {
		t.Fatalf("all-control got %q, want empty", got)
	}
}

// Oversized ASCII text is truncated to the cap.
func TestNormalizeObserverText_TruncatesOversized(t *testing.T) {
	in := strings.Repeat("x", 500)
	got := normalizeObserverText(in, maxObserverNameRunes)
	if len(got) != maxObserverNameRunes {
		t.Fatalf("len = %d, want %d", len(got), maxObserverNameRunes)
	}
}

// Existing valid display text passes through untouched.
func TestNormalizeObserverText_ValidUnchanged(t *testing.T) {
	for _, s := range []string{
		"Kings Island Control",
		"Key Route Inspector-BRK \U0001F5DD", // fixture name with emoji
		"meshcoretomqtt/1.0.8.0",
		"910.5250244,62.5,7,5",
	} {
		if got := normalizeObserverText(s, maxObserverClientVersionRunes); got != s {
			t.Fatalf("normalizeObserverText(%q) = %q, want unchanged", s, got)
		}
	}
}

// --- Store integration: bounds hold in the stored row ----------------------

func observerStrings(t *testing.T, s *Store, id string) (name, iata, model, firmware, client, radio string) {
	t.Helper()
	err := s.db.QueryRow(`SELECT COALESCE(name,''), COALESCE(iata,''), COALESCE(model,''),
	                             COALESCE(firmware,''), COALESCE(client_version,''), COALESCE(radio,'')
	                      FROM observers WHERE id = ?`, id).Scan(
		&name, &iata, &model, &firmware, &client, &radio)
	if err != nil {
		t.Fatalf("scan observer %q: %v", id, err)
	}
	return
}

func TestUpsertObserverBoundsDisplayMetadata(t *testing.T) {
	s := newTestStore(t)
	id := "obs-bounds"

	bigModel := strings.Repeat("m", 200)
	bigFirmware := strings.Repeat("f", 200)
	bigClient := strings.Repeat("c", 200)
	bigRadio := strings.Repeat("r", 200) + "\r"
	bigName := strings.Repeat("n", 200) + "\x00"
	bigRegion := strings.Repeat("Z", 200)

	meta := &ObserverMeta{
		Model:         &bigModel,
		Firmware:      &bigFirmware,
		ClientVersion: &bigClient,
		Radio:         &bigRadio,
	}
	if err := s.UpsertObserverAt(id, bigName, bigRegion, meta, ""); err != nil {
		t.Fatalf("UpsertObserverAt: %v", err)
	}

	name, iata, model, firmware, client, radio := observerStrings(t, s, id)
	if utf8.RuneCountInString(name) != maxObserverNameRunes {
		t.Errorf("name runes = %d, want %d", utf8.RuneCountInString(name), maxObserverNameRunes)
	}
	if utf8.RuneCountInString(iata) != maxObserverRegionRunes {
		t.Errorf("iata runes = %d, want %d", utf8.RuneCountInString(iata), maxObserverRegionRunes)
	}
	if utf8.RuneCountInString(model) != maxObserverModelRunes {
		t.Errorf("model runes = %d, want %d", utf8.RuneCountInString(model), maxObserverModelRunes)
	}
	if utf8.RuneCountInString(firmware) != maxObserverFirmwareRunes {
		t.Errorf("firmware runes = %d, want %d", utf8.RuneCountInString(firmware), maxObserverFirmwareRunes)
	}
	if utf8.RuneCountInString(client) != maxObserverClientVersionRunes {
		t.Errorf("client runes = %d, want %d", utf8.RuneCountInString(client), maxObserverClientVersionRunes)
	}
	if utf8.RuneCountInString(radio) != maxObserverRadioRunes {
		t.Errorf("radio runes = %d, want %d", utf8.RuneCountInString(radio), maxObserverRadioRunes)
	}
	if strings.ContainsAny(radio, "\r") || strings.ContainsAny(name, "\x00") {
		t.Errorf("control bytes survived: radio=%q name=%q", radio, name)
	}
}

// Existing valid observers are stored byte-for-byte as before.
func TestUpsertObserverValidMetadataUnchanged(t *testing.T) {
	s := newTestStore(t)
	id := strings.Repeat("ab", 32) // 64-char pubkey id
	model := "Heltec V3"
	firmware := "1.12.0-test"
	client := "meshcoretomqtt/1.0.8.0"
	radio := "910.5250244,62.5,7,5"
	meta := &ObserverMeta{Model: &model, Firmware: &firmware, ClientVersion: &client, Radio: &radio}
	if err := s.UpsertObserverAt(id, "DntnMarina Rptr mrymesh.net", "mry", meta, ""); err != nil {
		t.Fatalf("UpsertObserverAt: %v", err)
	}
	name, iata, gotModel, gotFirmware, gotClient, gotRadio := observerStrings(t, s, id)
	if name != "DntnMarina Rptr mrymesh.net" {
		t.Errorf("name = %q, changed", name)
	}
	if iata != "MRY" { // existing TrimSpace+ToUpper behaviour preserved
		t.Errorf("iata = %q, want MRY", iata)
	}
	if gotModel != model || gotFirmware != firmware || gotClient != client || gotRadio != radio {
		t.Errorf("metadata changed: %q %q %q %q", gotModel, gotFirmware, gotClient, gotRadio)
	}
}

// --- End-to-end through handleMessage: the id gate ---------------------------

func countObservers(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observers`).Scan(&n); err != nil {
		t.Fatalf("count observers: %v", err)
	}
	return n
}

// A packet message whose observer id is over-limit is dropped whole: no
// observer row and no observation row — so an invalid id can never create a
// row that later work keys on.
func TestHandleMessage_OversizedObserverIDDropped(t *testing.T) {
	store, source := newTestContext(t)
	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	badID := strings.Repeat("a", maxObserverIDBytes+1)
	msg := &mockMessage{
		topic:   "meshcore/SJC/" + badID + "/packets",
		payload: []byte(`{"raw":"` + rawHex + `"}`),
	}
	handleMessage(store, "test", source, msg, nil, nil, &Config{})

	if n := countObservers(t, store); n != 0 {
		t.Fatalf("observers = %d, want 0 (oversized id must be dropped)", n)
	}
	var obs int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&obs); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if obs != 0 {
		t.Fatalf("observations = %d, want 0 (whole message dropped)", obs)
	}
}

// Two distinct ids sharing a long prefix each create their own row end-to-end:
// identities are not merged by any truncation.
func TestHandleMessage_LongPrefixIDsStayDistinct(t *testing.T) {
	store, source := newTestContext(t)
	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	prefix := strings.Repeat("a", maxObserverIDBytes-1)
	for _, suffix := range []string{"X", "Y"} {
		msg := &mockMessage{
			topic:   "meshcore/SJC/" + prefix + suffix + "/packets",
			payload: []byte(`{"raw":"` + rawHex + `"}`),
		}
		handleMessage(store, "test", source, msg, nil, nil, &Config{})
	}
	if n := countObservers(t, store); n != 2 {
		t.Fatalf("observers = %d, want 2 (distinct long-prefix ids)", n)
	}
	for _, suffix := range []string{"X", "Y"} {
		var got string
		if err := store.db.QueryRow(`SELECT id FROM observers WHERE id = ?`, prefix+suffix).Scan(&got); err != nil {
			t.Fatalf("id %s...%s not stored distinctly: %v", prefix[:4], suffix, err)
		}
	}
}

// --- Diagnostics: bounded / rate-limited -----------------------------------

func TestObserverIDDropThrottleRateLimited(t *testing.T) {
	th := &observerIDDropThrottle{}
	base := time.Unix(1_700_000_000, 0).UTC()
	// First drop logs immediately and resets the counters.
	th.record("test", "aaaaaaaaX", observerIDRejectTooLong, base)
	if got := th.pendingDropped(); got != 0 {
		t.Fatalf("after first record pendingDropped=%d, want 0 (logged+reset)", got)
	}
	// Drops inside the interval accumulate without logging.
	th.record("test", "aaaaaaaaY", observerIDRejectTooLong, base.Add(time.Minute))
	th.record("test", "aaaaaaaaZ", observerIDRejectTooLong, base.Add(2*time.Minute))
	if got := th.pendingDropped(); got != 2 {
		t.Fatalf("within-interval pendingDropped=%d, want 2 (accumulated, not logged)", got)
	}
	// After the interval the accumulated count is flushed (logged + reset).
	th.record("test", "aaaaaaaaW", observerIDRejectTooLong, base.Add(observerIDDropLogInterval+time.Second))
	if got := th.pendingDropped(); got != 0 {
		t.Fatalf("after interval pendingDropped=%d, want 0 (flushed)", got)
	}
}

func TestObserverIDLogSampleBounded(t *testing.T) {
	if got := observerIDLogSample(strings.Repeat("z", 100)); len(got) != 8 {
		t.Fatalf("sample len = %d, want 8", len(got))
	}
	if got := observerIDLogSample("short"); got != "short" {
		t.Fatalf("sample = %q, want short", got)
	}
}
