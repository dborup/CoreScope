package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Follow-ups to #386 (#395). Three gaps the reviews of #386 left open, each
// with its own mutant:
//
//   - Point 1: the id gate's own guard, len(parts) > 2, is unpinned. Every
//     dispatch-path subtest in observer_metadata_bounds_review_test.go uses a
//     FOUR-segment topic, so narrowing the guard to len(parts) > 3 leaves the
//     whole suite green while a three-segment raw-packet topic walks straight
//     past the gate.
//   - Point 2: F4 bounded normalizeObserverText's ALLOCATION but not its WALK.
//     The strip/continue sits above the cap check, so a run of control or bidi
//     runes is walked in full however long it is.
//   - Point 4: the mobile-RX path returns before the id gate and
//     client_observers.name was unbounded display text.
//
// Point 3 (the per-rune unicode.Is cost in the gate) needs no change, and
// point 5 (observer cardinality) is design-only — see the PR description.

// --- Point 1: pin the gate's len(parts) > 2 guard --------------------------

// threeSegmentPacketRaw is the raw-packet fixture the dispatch-path tests use.
// A raw packet does NOT need the /packets suffix: handleMessage reaches the
// "Format 1: Raw packet" branch for any observer topic carrying a `raw` field,
// so meshcore/<region>/<observer_id> — three segments — is a real ingestion
// topic that writes an observers row and an observation.
const threeSegmentPacketRaw = "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"

// The gate reads parts[2], so it must guard len(parts) > 2 — the smallest
// topic that HAS a parts[2]. Narrowing that to > 3 (an off-by-one a refactor
// can introduce without touching anything else) ungates every three-segment
// topic, and no existing test sees it: all three dispatch-path subtests publish
// four-segment topics (.../status, .../neighbors, .../packets), where > 3 still
// holds. This drives the three-segment form, so the guard's boundary is pinned
// and not just the branch below it.
func TestHandleMessage_InvalidObserverIDDroppedOnThreeSegmentTopic(t *testing.T) {
	badIDs := []struct {
		name string
		id   string
	}{
		{"too-long", strings.Repeat("a", maxObserverIDBytes+1)},
		{"control-char", "obs\x01id"},
		{"bidi-control", "obs\u202eid"},
	}
	for _, tc := range badIDs {
		t.Run(tc.name, func(t *testing.T) {
			store, source := newTestContext(t)
			msg := &mockMessage{
				topic:   "meshcore/SJC/" + tc.id,
				payload: []byte(`{"raw":"` + threeSegmentPacketRaw + `"}`),
			}
			handleMessage(store, "test", source, msg, nil, nil, &Config{})
			for _, table := range []string{"observers", "observations"} {
				if n := countRows(t, store, table); n != 0 {
					t.Errorf("%s = %d rows, want 0 (a three-segment topic has a parts[2] and must be gated)", table, n)
				}
			}
		})
	}
}

// Counterpart, same shape as the four-segment one: a VALID id on a
// three-segment topic must still reach the packet path, so the guard cannot be
// "pinned" by widening it until everything is dropped.
func TestHandleMessage_ValidObserverIDReachesThreeSegmentTopic(t *testing.T) {
	const id = "kpabap" // the fixture's shortest real, non-pubkey operator id
	store, source := newTestContext(t)
	msg := &mockMessage{
		topic:   "meshcore/SJC/" + id,
		payload: []byte(`{"raw":"` + threeSegmentPacketRaw + `"}`),
	}
	handleMessage(store, "test", source, msg, nil, nil, &Config{})
	if n := countRows(t, store, "observers"); n != 1 {
		t.Fatalf("observers = %d, want 1", n)
	}
	if n := countRows(t, store, "observations"); n != 1 {
		t.Fatalf("observations = %d, want 1", n)
	}
	name, iata, _, _, _, _ := observerStrings(t, store, id)
	if iata != "SJC" {
		t.Fatalf("stored iata = %q, want %q (name=%q)", iata, "SJC", name)
	}
}

// --- Point 2: the WALK is bounded, not only the allocation -----------------

// F4 sized the builder from the cap. That bounds what the function KEEPS, but
// the loop still visits every rune it was handed, because the strip/continue
// sits above the cap check: a field of N control runes costs N iterations of
// observerRuneIsControl + observerRuneIsBidiControl (the latter a binary search
// over the Bidi_Control table) to produce nothing at all. There is no MQTT
// payload size cap in cmd/ingestor, so N is a broker deployment property.
//
// Asserted on the scan count rather than on elapsed time, so the bound is
// exact and the test cannot flake on a loaded machine. An input made only of
// stripped runes produces the same "" either way, which is why the walk needs
// its own observable: normalizeObserverTextScan reports what the loop visited.
func TestNormalizeObserverText_BoundsTheWalkNotOnlyTheAllocation(t *testing.T) {
	const inputRunes = 1 << 20 // far above any cap, and cheap to build
	cases := []struct {
		name string
		in   string
	}{
		{"C0 controls", strings.Repeat("\x01", inputRunes)},
		{"C1 controls", strings.Repeat("\u0085", inputRunes)},
		{"bidi controls", strings.Repeat("\u202e", inputRunes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, scanned := normalizeObserverTextScan(tc.in, maxObserverNameRunes)
			if got != "" {
				t.Errorf("output = %q, want \"\" (every rune is stripped)", got)
			}
			if scanned > maxObserverTextScanRunes {
				t.Errorf("walked %d runes of a %d-rune all-stripped input (want <= %d): the cap bounds the allocation but not the walk",
					scanned, utf8.RuneCountInString(tc.in), maxObserverTextScanRunes)
			}
		})
	}
}

// The walk bound is also visible in the OUTPUT, without any instrumentation:
// text that sits beyond the scan cap behind a run of stripped runes is never
// reached. That is the deliberate trade — see the constant's comment — and
// pinning it here keeps the two halves (scan count, output) from drifting.
func TestNormalizeObserverText_StrippedRunRunsOutOfScanBudget(t *testing.T) {
	cases := []struct {
		name string
		pad  int
		want string
	}{
		{"padding under the scan cap keeps the text", maxObserverTextScanRunes - 8, "name"},
		{"padding over the scan cap drops it", maxObserverTextScanRunes + 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := strings.Repeat("\x01", tc.pad) + "name"
			if got := normalizeObserverText(in, maxObserverNameRunes); got != tc.want {
				t.Errorf("normalizeObserverText(%d-rune pad + %q) = %q, want %q", tc.pad, "name", got, tc.want)
			}
		})
	}
}

// Bounding the walk must not change what any legitimate field normalizes to,
// and must not change the scan count for an input that fits. The value the CI
// fixture actually carries (a radio line ending in a bare CR) is in here on
// purpose.
func TestNormalizeObserverText_WalkBoundLeavesShortInputsAlone(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
	}{
		{"fixture radio with trailing CR", "910.5250244,62.5,7,5\r", maxObserverRadioRunes, "910.5250244,62.5,7,5"},
		{"fixture name with emoji", "Key Route Inspector-BRK \U0001F5DD", maxObserverNameRunes, "Key Route Inspector-BRK \U0001F5DD"},
		{"danish name", "Københavns Observatør", maxObserverNameRunes, "Københavns Observatør"},
		{"strip only, no cap", "a\u202eb\u0085c", 0, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, scanned := normalizeObserverTextScan(tc.in, tc.maxRunes)
			if got != tc.want {
				t.Errorf("normalizeObserverText(%q, %d) = %q, want %q", tc.in, tc.maxRunes, got, tc.want)
			}
			if n := utf8.RuneCountInString(tc.in); scanned != n {
				t.Errorf("walked %d runes, want all %d: an input under the scan cap must be walked in full", scanned, n)
			}
		})
	}
}

// BenchmarkNormalizeObserverTextAllStripped measures the case the walk bound
// exists for: a megabyte-sized display field made only of runes that are
// stripped, so nothing short-circuits on the kept-rune cap. Before the bound
// this scales with the input; after it, it is flat.
func BenchmarkNormalizeObserverTextAllStripped(b *testing.B) {
	for _, bc := range []struct {
		name string
		in   string
	}{
		{"1KiB", strings.Repeat("\x01", 1<<10)},
		{"1MiB", strings.Repeat("\x01", 1<<20)},
		{"1MiB-bidi", strings.Repeat("\u202e", 1<<20)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(bc.in)))
			for i := 0; i < b.N; i++ {
				if out := normalizeObserverText(bc.in, maxObserverNameRunes); out != "" {
					b.Fatalf("output = %q, want \"\"", out)
				}
			}
		})
	}
}

// --- Point 4: the mobile-RX path's display name ---------------------------

// client_observers.name is the companion's SELF-REPORTED name, taken from the
// `origin` field of an untrusted payload — the same shape of value as
// observers.name, which #386 bounded. The mobile-RX path returns from
// handleMessage before the observer id gate, so it never saw that work.
// Identity on this path is a different contract and stays untouched: the
// companion pubkey is regex-validated (clientPubkeyRe) and never rewritten.
func TestUpsertClientObserverBoundsName(t *testing.T) {
	s := newTestStore(t)
	const ts = "2026-06-09T12:00:00Z"
	long := strings.Repeat("ø", maxObserverNameRunes+40) // multi-byte, over the cap

	if err := s.UpsertClientObserver(testCompanionPK, long+"\x01\u202e", ts); err != nil {
		t.Fatal(err)
	}
	got := clientObserverName(t, s, testCompanionPK)
	if n := utf8.RuneCountInString(got); n != maxObserverNameRunes {
		t.Errorf("stored name = %d runes, want %d (the mobile-RX display name is unbounded)", n, maxObserverNameRunes)
	}
	if !utf8.ValidString(got) {
		t.Errorf("stored name is not valid UTF-8: truncation split a rune")
	}
	for _, r := range got {
		if observerRuneIsControl(r) || observerRuneIsBidiControl(r) {
			t.Errorf("stored name kept a control/bidi rune U+%04X", r)
		}
	}
}

// Same bound, driven end-to-end through the real mobile-RX entry point, so the
// policy is pinned where the untrusted value actually arrives and not only at
// the Store method.
func TestHandleClientPacketBoundsObserverName(t *testing.T) {
	s := newTestStore(t)
	long := strings.Repeat("A", 4096) + "\x01"
	msg := map[string]interface{}{
		"raw":       "11451000D818206D3AAC152C8A91F89957E6D30CA51F36E28790228971C473B755F244F718754CF5EE4A2FD58D944466E42CDED140C66D0CC590183E32BAF40F112BE8F3F2BDF6012B4B2793C52F1D36F69EE054D9A05593286F78453E56C0EC4A3EB95DDA2A7543FCCC00B939CACC009278603902FC12BCF84B706120526F6F6620536F6C6172",
		"direction": "rx",
		"timestamp": "2026-06-09T12:00:00Z",
		"origin":    long,
		"SNR":       -7.0,
		"RSSI":      -92.0,
		"gps":       map[string]interface{}{"lat": 51.05, "lon": 3.72, "acc_m": 8.0},
	}
	handleClientPacket(s, "test", testCompanionPK, msg, nil)

	got := clientObserverName(t, s, testCompanionPK)
	if n := utf8.RuneCountInString(got); n != maxObserverNameRunes {
		t.Fatalf("stored name = %d runes, want %d", n, maxObserverNameRunes)
	}
	if strings.ContainsRune(got, '\x01') {
		t.Error("stored name kept a control byte")
	}
	// The identity side of this path is unchanged: the coverage row is still
	// written under the regex-validated pubkey.
	if n := countRows(t, s, "client_receptions"); n != 1 {
		t.Fatalf("client_receptions = %d, want 1: bounding the name must not drop the coverage row", n)
	}
}

// A name that is nothing but stripped runes normalizes to "", which the Store
// method already treats as "no name to record" — so it must not write a row
// with an empty name (the leaderboard reads this column).
func TestUpsertClientObserverSkipsFullyStrippedName(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertClientObserver(testCompanionPK, "\x01\u202e", "2026-06-09T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "client_observers"); n != 0 {
		t.Errorf("client_observers = %d rows, want 0 for a name that is entirely stripped", n)
	}
}

// The other direction: a real companion name is stored byte-for-byte, and the
// pubkey keeps its existing lowercasing. Identity validation on this path is
// explicitly out of scope for #395.
func TestUpsertClientObserverValidNameUnchanged(t *testing.T) {
	s := newTestStore(t)
	const want = "Nørrebro Mobil \U0001F5DD"
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := s.UpsertClientObserver(strings.ToUpper(testCompanionPK), want, ts); err != nil {
		t.Fatal(err)
	}
	if got := clientObserverName(t, s, testCompanionPK); got != want {
		t.Errorf("stored name = %q, want %q verbatim", got, want)
	}
}

// clientObserverName reads back the one column #395 bounds.
func clientObserverName(t *testing.T, s *Store, pubkey string) string {
	t.Helper()
	var name string
	if err := s.db.QueryRow(`SELECT name FROM client_observers WHERE pubkey=?`, strings.ToLower(pubkey)).Scan(&name); err != nil {
		t.Fatalf("read client_observers name: %v", err)
	}
	return name
}
