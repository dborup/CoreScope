package main

import (
	"log"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Round-2 review tests for the observer-metadata bounds (#338, PR #386).
//
// observer_metadata_bounds_test.go pinned the raw-packet path, the
// UpsertObserverAt display bounds and a C0+DEL control policy. The reviewer's
// mutation run showed three of those pins were too narrow and one diagnostic
// was inaccurate:
//
//   - N1: moving the handleMessage id gate below the neighbors+status dispatch
//     left the whole suite green — only the packet path was pinned.
//   - N2: stripping every bound from UpsertObserverRetained (the path a
//     retained status takes, which is the common one on subscribe) left the
//     whole suite green.
//   - N3: "control characters" meant C0+DEL only; C1 controls (U+0080-U+009F)
//     and bidi overrides/isolates (U+202A-U+202E, U+2066-U+2069) passed both
//     the id gate and the display normalizer untouched.
//   - N4: the aggregated drop line carried one reason/sample for a count that
//     could span several reasons, so it could mislabel what was suppressed.
//
// Everything below closes those gaps. Each group is written so the matching
// mutant fails here. countRows is the shared COUNT(*) helper from
// prune_chunked_test.go.

// --- N1: pin WHERE the id gate sits ----------------------------------------

// currentReportTS is the report-envelope timestamp the neighbors payloads
// below carry. It MUST be derived from the clock rather than hardcoded: #374
// added a future bound to normalizeReportTS (maxReportFutureSkew, 15 minutes),
// and ReplaceObserverNeighbors drops a report whose envelope timestamp is past
// it. A fixed literal therefore has an expiry date — before it, the valid-path
// test fails outright, and worse, the INVALID-path neighbors subtest goes
// vacuous: the report is rejected for the timestamp instead of the id, so the
// subtest passes even with the id gate moved below the dispatch, which is the
// exact mutant it exists to kill. Relative to now, both subtests stay honest
// on any clock. There is no past bound, so "now" needs no skew of its own.
func currentReportTS() string {
	return time.Now().UTC().Format(time.RFC3339)
}

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
	reportTS := currentReportTS()

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
			payload: `{"timestamp":"` + reportTS + `","neighbors":[{"pubkey":"` + neighborPubkey + `","status":"responded","scopes":"SJC","snr":-7.5,"heard_secs_ago":12}]}`,
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
	reportTS := currentReportTS()

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
			payload: []byte(`{"timestamp":"` + reportTS + `","neighbors":[{"pubkey":"` + neighborPubkey + `","status":"responded","scopes":"SJC","snr":-7.5,"heard_secs_ago":12}]}`),
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

// --- N3: the control policy covers C1 and bidi overrides/isolates ----------

// C1 controls (U+0080-U+009F) are controls that happen to be two bytes in
// UTF-8, so a byte-oriented "< 0x20" check misses them entirely. In an id they
// must be rejected exactly like C0.
func TestObserverIDRejectReason_C1Controls(t *testing.T) {
	for _, r := range []rune{'\u0080', '\u0085', '\u009b', '\u009f'} {
		id := "obs" + string(r) + "01"
		if got := observerIDRejectReason(id); got != observerIDRejectControlChar {
			t.Errorf("observerIDRejectReason(%q) = %q, want %q (U+%04X is a C1 control)", id, got, observerIDRejectControlChar, r)
		}
	}
	// U+007E and U+00A0 bracket the policy: the last printable ASCII before
	// DEL, and the first codepoint past the C1 block (NBSP). Both are text.
	for _, r := range []rune{'~', '\u00a0'} {
		id := "obs" + string(r) + "01"
		if got := observerIDRejectReason(id); got != "" {
			t.Errorf("observerIDRejectReason(%q) = %q, want accepted (U+%04X is outside the control ranges)", id, got, r)
		}
	}
}

// bidiControlRunes is the COMPLETE set of codepoints Unicode gives the
// Bidi_Control property — all twelve. Round 2 covered only the embedding/
// override block (U+202A-U+202E) and the isolates (U+2066-U+2069); the marks
// U+061C/U+200E/U+200F carry the same property and the same invisibility, and
// they are the sharper case for #338's identity contract: two ids that differ
// only by an invisible mark render identically, so an operator reading the UI
// cannot tell the two stored rows apart.
//
// Kept as one table used by every bidi assertion below, so the policy and its
// tests cannot drift apart the way they did for these three.
var bidiControlRunes = []rune{
	'\u061c', // ALM, Arabic letter mark
	'\u200e', // LRM, left-to-right mark
	'\u200f', // RLM, right-to-left mark
	'\u202a', // LRE
	'\u202b', // RLE
	'\u202c', // PDF
	'\u202d', // LRO
	'\u202e', // RLO
	'\u2066', // LRI
	'\u2067', // RLI
	'\u2068', // FSI
	'\u2069', // PDI
}

// bidiControlBorderRunes are the codepoints bracketing the ranges above that
// are NOT Bidi_Control and must stay text. U+200B/U+200C/U+200D and U+2060 are
// the interesting ones: they are invisible too, but they are not Bidi_Control
// and bounding them is a separate question from this policy (see the PR's
// residuals), so the boundary is pinned here rather than left implicit.
var bidiControlBorderRunes = []rune{
	'\u061b', // Arabic semicolon, just below ALM
	'\u061d', // just above ALM
	'\u200b', // ZWSP — invisible, not Bidi_Control
	'\u200c', // ZWNJ
	'\u200d', // ZWJ, just below LRM
	'\u2010', // hyphen, just above RLM
	'\u2029', // paragraph separator
	'\u202f', // narrow no-break space, just above RLO
	'\u2060', // word joiner — invisible, not Bidi_Control
	'\u2065', // just below LRI
	'\u206a', // just above PDI
}

// The three Bidi_Control codepoints round 2 missed, called out on their own so
// the gap is named by a failing test and not only by the table above.
func TestObserverIDRejectReason_BidiMarks(t *testing.T) {
	for _, r := range []rune{'\u200e', '\u200f', '\u061c'} {
		id := "obs" + string(r) + "01"
		if got := observerIDRejectReason(id); got != observerIDRejectBidiControl {
			t.Errorf("observerIDRejectReason(%q) = %q, want %q (U+%04X is Bidi_Control)", id, got, observerIDRejectBidiControl, r)
		}
		if got := normalizeObserverText("Observer"+string(r)+"01", 64); got != "Observer01" {
			t.Errorf("U+%04X survived normalizeObserverText: %q", r, got)
		}
	}
	// The identity case the marks create: without the policy these two ids
	// render the same and are stored as two distinct observers.
	plain, marked := "obs01", "obs\u200e01"
	if plain == marked {
		t.Fatal("test setup: the two ids must differ in bytes")
	}
	if observerIDRejectReason(plain) != "" {
		t.Fatalf("the unmarked id must stay acceptable")
	}
	if got := observerIDRejectReason(marked); got == "" {
		t.Errorf("observerIDRejectReason(%q) accepted it — it renders identically to %q, so the two rows cannot be told apart", marked, plain)
	}
}

// End-to-end: a bidi-mark-bearing id is dropped at the gate, like the override
// and isolate cases above it.
func TestHandleMessage_BidiMarkObserverIDDropped(t *testing.T) {
	for _, r := range []rune{'\u200e', '\u200f', '\u061c'} {
		store, source := newTestContext(t)
		msg := &mockMessage{
			topic:   "meshcore/SJC/obs" + string(r) + "01/status",
			payload: []byte(`{"origin":"Hostile","noise_floor":-100}`),
		}
		handleMessage(store, "test", source, msg, nil, nil, &Config{})
		if n := countRows(t, store, "observers"); n != 0 {
			t.Errorf("U+%04X: observers = %d, want 0 (a Bidi_Control id must be dropped)", r, n)
		}
	}
}

// Bidi embedding/override (U+202A-U+202E) and isolate (U+2066-U+2069)
// characters reorder everything after them when rendered, so an id carrying
// one displays as a different id than the one rows are keyed on. Rejected.
func TestObserverIDRejectReason_BidiControls(t *testing.T) {
	for _, r := range bidiControlRunes {
		id := "obs" + string(r) + "01"
		if got := observerIDRejectReason(id); got != observerIDRejectBidiControl {
			t.Errorf("observerIDRejectReason(%q) = %q, want %q (U+%04X)", id, got, observerIDRejectBidiControl, r)
		}
	}
	// The codepoints immediately outside every covered range are ordinary
	// characters.
	for _, r := range bidiControlBorderRunes {
		id := "obs" + string(r) + "01"
		if got := observerIDRejectReason(id); got != "" {
			t.Errorf("observerIDRejectReason(%q) = %q, want accepted (U+%04X is not a Bidi_Control codepoint)", id, got, r)
		}
	}
}

// Ids made of ordinary non-ASCII text stay acceptable: the policy targets
// control and bidi-formatting codepoints, not "anything non-ASCII".
func TestObserverIDRejectReason_ValidUnicodeAccepted(t *testing.T) {
	for _, id := range []string{
		"kpabap",
		"obs-æøå",
		"OBS-ÆØÅ",
		"observatør-københavn",
		"obs-\U0001F5DD",         // emoji
		strings.Repeat("ab", 32), // 64-char pubkey
	} {
		if got := observerIDRejectReason(id); got != "" {
			t.Errorf("observerIDRejectReason(%q) = %q, want accepted", id, got)
		}
	}
}

// Display text: the same two groups are stripped rather than rejected.
func TestNormalizeObserverText_StripsC1Controls(t *testing.T) {
	in := "a\u0080b\u0085c\u009bd\u009fef"
	if got := normalizeObserverText(in, 64); got != "abcdef" {
		t.Fatalf("got %q, want %q (C1 controls must be stripped)", got, "abcdef")
	}
	// Stripping happens before counting, so C1 padding does not eat the cap.
	padded := strings.Repeat("\u0085", 100) + "name"
	if got := normalizeObserverText(padded, 4); got != "name" {
		t.Fatalf("padded got %q, want %q", got, "name")
	}
	// NBSP survives — it is text, not a control.
	if got := normalizeObserverText("a\u00a0b", 64); got != "a\u00a0b" {
		t.Fatalf("NBSP stripped: %q", got)
	}
}

func TestNormalizeObserverText_StripsBidiControls(t *testing.T) {
	// A right-to-left override inside a display name makes the rendered string
	// read differently from the stored bytes — the UI-spoofing case.
	in := "Observer\u202e01\u202c-SJC"
	if got := normalizeObserverText(in, 64); got != "Observer01-SJC" {
		t.Fatalf("got %q, want %q (bidi overrides must be stripped)", got, "Observer01-SJC")
	}
	for _, r := range bidiControlRunes {
		if got := normalizeObserverText("a"+string(r)+"b", 64); got != "ab" {
			t.Errorf("U+%04X survived normalizeObserverText: %q", r, got)
		}
	}
	for _, r := range bidiControlBorderRunes {
		want := "a" + string(r) + "b"
		if got := normalizeObserverText(want, 64); got != want {
			t.Errorf("U+%04X stripped but is not a Bidi_Control codepoint: %q", r, got)
		}
	}
}

// Valid Unicode display text is preserved verbatim: Danish letters and emoji
// are exactly what the fixture's observer names carry.
func TestNormalizeObserverText_PreservesValidUnicode(t *testing.T) {
	for _, s := range []string{
		"Københavns Observatør",
		"Ærø Øst Ångström",
		"Key Route Inspector-BRK \U0001F5DD",
		"Nørrebro \U0001F5DD Relæ",
	} {
		if got := normalizeObserverText(s, maxObserverNameRunes); got != s {
			t.Fatalf("normalizeObserverText(%q) = %q, want unchanged", s, got)
		}
	}
}

// End-to-end: a bidi-bearing id is dropped at the gate, never stored.
func TestHandleMessage_BidiObserverIDDropped(t *testing.T) {
	store, source := newTestContext(t)
	msg := &mockMessage{
		topic:   "meshcore/SJC/obs\u202e01/status",
		payload: []byte(`{"origin":"Hostile","noise_floor":-100}`),
	}
	handleMessage(store, "test", source, msg, nil, nil, &Config{})
	if n := countRows(t, store, "observers"); n != 0 {
		t.Fatalf("observers = %d, want 0 (a bidi-bearing id must be dropped)", n)
	}
}

// End-to-end: a C1-bearing id is dropped at the gate too.
func TestHandleMessage_C1ControlObserverIDDropped(t *testing.T) {
	store, source := newTestContext(t)
	msg := &mockMessage{
		topic:   "meshcore/SJC/obs\u009b01/status",
		payload: []byte(`{"origin":"Hostile","noise_floor":-100}`),
	}
	handleMessage(store, "test", source, msg, nil, nil, &Config{})
	if n := countRows(t, store, "observers"); n != 0 {
		t.Fatalf("observers = %d, want 0 (a C1-bearing id must be dropped)", n)
	}
}

// Display bounds end-to-end: a C1/bidi-bearing display name reaches the stored
// row stripped, while the valid Unicode around it survives.
func TestUpsertObserverStripsC1AndBidiFromDisplayText(t *testing.T) {
	s := newTestStore(t)
	id := "obs-display-unicode"
	radio := "910.5250244,62.5,7,5\u0085"
	meta := &ObserverMeta{Radio: &radio}
	if err := s.UpsertObserverAt(id, "Nørrebro\u202e Relæ \U0001F5DD", "SJC", meta, ""); err != nil {
		t.Fatalf("UpsertObserverAt: %v", err)
	}
	name, _, _, _, _, gotRadio := observerStrings(t, s, id)
	if name != "Nørrebro Relæ \U0001F5DD" {
		t.Errorf("stored name = %q, want the bidi override stripped and the rest intact", name)
	}
	if gotRadio != "910.5250244,62.5,7,5" {
		t.Errorf("stored radio = %q, want the C1 control stripped", gotRadio)
	}
}

// --- N4: the aggregated drop line must not mislabel the batch --------------

// The throttle aggregates several drops into one line. With a single counter
// plus the LAST reason/sample, a line reading "dropped 3 ... reason=too-long"
// can actually be summarising two control-char drops and one too-long drop.
// The line must account for each reason separately.
func TestObserverIDDropThrottleLineCountsPerReason(t *testing.T) {
	th := &observerIDDropThrottle{}
	base := time.Unix(1_700_000_000, 0).UTC()
	buf := captureObserverDropLog(t)

	// The first drop logs immediately and is reported on its own.
	th.record("test", strings.Repeat("a", 200), observerIDRejectTooLong, base)
	buf.Reset()

	// A mixed batch inside the interval: 1 too-long, 2 control-char,
	// 1 bidi-control, 1 invalid-utf8.
	th.record("test", strings.Repeat("b", 200), observerIDRejectTooLong, base.Add(10*time.Second))
	th.record("test", "obs\x01aa", observerIDRejectControlChar, base.Add(20*time.Second))
	th.record("test", "obs\x02aa", observerIDRejectControlChar, base.Add(30*time.Second))
	th.record("test", "obs\u202eaa", observerIDRejectBidiControl, base.Add(40*time.Second))
	th.record("test", "obs\xffaa", observerIDRejectInvalidUTF8, base.Add(50*time.Second))
	if got := buf.String(); got != "" {
		t.Fatalf("within-interval drops logged early: %q", got)
	}
	if got := th.pendingDropped(); got != 5 {
		t.Fatalf("pendingDropped = %d, want 5", got)
	}

	// The next drop past the interval flushes the batch (5 accumulated + itself).
	th.record("test", "obs\x03aa", observerIDRejectControlChar, base.Add(observerIDDropLogInterval))
	line := buf.String()
	if !strings.Contains(line, "dropped 6 message(s)") {
		t.Errorf("line does not report the batch total:\n%s", line)
	}
	for _, want := range []string{"too-long=1", "control-char=3", "bidi-control=1", "invalid-utf8=1"} {
		if !strings.Contains(line, want) {
			t.Errorf("line is missing %q — a single last-reason label mislabels the batch:\n%s", want, line)
		}
	}
	if got := th.pendingDropped(); got != 0 {
		t.Errorf("pendingDropped after flush = %d, want 0", got)
	}
}

// A reason that did not occur is not named at all, so the line stays readable
// and never implies a drop that did not happen.
func TestObserverIDDropThrottleLineOmitsUnusedReasons(t *testing.T) {
	th := &observerIDDropThrottle{}
	base := time.Unix(1_700_000_000, 0).UTC()
	buf := captureObserverDropLog(t)

	th.record("test", strings.Repeat("a", 200), observerIDRejectTooLong, base)
	line := buf.String()
	if !strings.Contains(line, "too-long=1") {
		t.Fatalf("line is missing the reason that did occur:\n%s", line)
	}
	for _, unwanted := range []string{"control-char", "bidi-control", "invalid-utf8", "=0"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("line names %q but no such drop occurred:\n%s", unwanted, line)
		}
	}
}

// A sample is carried per reason, so the sample printed next to a reason
// belongs to that reason — and it stays bounded to the 8-rune prefix, never
// the full id.
func TestObserverIDDropThrottleSamplePerReason(t *testing.T) {
	th := &observerIDDropThrottle{}
	base := time.Unix(1_700_000_000, 0).UTC()
	buf := captureObserverDropLog(t)

	longID := strings.Repeat("L", 200)
	th.record("test", longID, observerIDRejectTooLong, base) // first drop: logs now
	buf.Reset()
	th.record("test", longID, observerIDRejectTooLong, base.Add(10*time.Second))
	th.record("test", "CTRLxxxx-tail", observerIDRejectControlChar, base.Add(20*time.Second))
	th.record("test", "x", observerIDRejectTooLong, base.Add(observerIDDropLogInterval))

	line := buf.String()
	if !strings.Contains(line, `too-long=2 sample="x"`) {
		t.Errorf("too-long sample not carried with its own reason:\n%s", line)
	}
	if !strings.Contains(line, `control-char=1 sample="CTRLxxxx"`) {
		t.Errorf("control-char sample not carried with its own reason:\n%s", line)
	}
	if strings.Contains(line, longID) {
		t.Errorf("the full oversized id leaked into the log line:\n%s", line)
	}
	if strings.Contains(line, "CTRLxxxx-tail") {
		t.Errorf("sample was not bounded to 8 runes:\n%s", line)
	}
}

// --- F4: the builder is sized by the cap, not by the untrusted input -------

// normalizeObserverText is damage containment, so its own allocation must be
// bounded by what it can RETURN, not by what it was handed. There is no MQTT
// payload size cap anywhere in cmd/ingestor — the only reader is
// json.Unmarshal(m.Payload(), ...) in handleMessage — so a display field's
// length is a broker deployment property, and b.Grow(len(s)) turns a 4 MiB
// field into a 4 MiB allocation to produce 64 runes.
//
// Measured rather than argued: TotalAlloc across the call, lowest of several
// attempts so a background goroutine's allocation cannot inflate the verdict.
// The threshold is a quarter of the input, far above the few hundred bytes the
// bounded builder needs and far below the full-input allocation.
func TestNormalizeObserverText_BuilderSizedByCapNotInput(t *testing.T) {
	const inputBytes = 4 << 20
	in := strings.Repeat("x", inputBytes)

	var lowest uint64 = math.MaxUint64
	var out string
	for i := 0; i < 5; i++ {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		out = normalizeObserverText(in, maxObserverNameRunes)
		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew < lowest {
			lowest = grew
		}
	}
	// Keep the result reachable so nothing above can be optimised away.
	if n := utf8.RuneCountInString(out); n != maxObserverNameRunes {
		t.Fatalf("runes = %d, want %d", n, maxObserverNameRunes)
	}
	if limit := uint64(inputBytes / 4); lowest > limit {
		t.Errorf("normalizeObserverText allocated %d B for a %d B input capped at %d runes (want <= %d B): the builder is sized from the input, not the cap",
			lowest, inputBytes, maxObserverNameRunes, limit)
	}
}

// The other direction: bounding the builder must not change what the function
// returns for any input shape, including one whose runes are multi-byte (where
// 4 bytes per rune is the headroom the cap-derived size has to allow) and one
// shorter than the cap (where the input is the smaller of the two).
func TestNormalizeObserverText_CapSizedBuilderKeepsOutput(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
	}{
		{"shorter than the cap", "Nørrebro Relæ", 64, "Nørrebro Relæ"},
		{"multi-byte at the cap", strings.Repeat("ø", 100), 8, strings.Repeat("ø", 8)},
		{"4-byte runes at the cap", strings.Repeat("\U0001F5DD", 40), 8, strings.Repeat("\U0001F5DD", 8)},
		{"controls then text", strings.Repeat("\x01", 1000) + "name", 4, "name"},
		{"strip only, no cap", "a\u202eb\u0085c", 0, "abc"},
		{"strip only, long input", strings.Repeat("ø", 500), -1, strings.Repeat("ø", 500)},
		{"empty", "", 64, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeObserverText(tc.in, tc.maxRunes); got != tc.want {
				t.Errorf("normalizeObserverText(%d runes in, maxRunes=%d) = %q, want %q",
					utf8.RuneCountInString(tc.in), tc.maxRunes, got, tc.want)
			}
		})
	}
}

// captureObserverDropLog redirects the standard logger into a buffer for the
// duration of the test. Flags are cleared so assertions can match the line
// text without a timestamp prefix in the way.
func captureObserverDropLog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	return &buf
}
