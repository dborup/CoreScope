package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Bounds and normalizers for observer metadata (#338).
//
// # Two different contracts: identity vs. display
//
// Observer rows are keyed on an observer id (observers.id, the MQTT topic
// segment parts[2]). That id is an IDENTITY: every observer_neighbors,
// observer_neighbor_metrics, observer_metrics and clock-skew row joins back to
// it, and the row the firmware metadata lands on is chosen by it. An id must
// therefore never be rewritten to fit a bound — truncating "AAAA…X" and
// "AAAA…Y" to a shared prefix would MERGE two distinct observers into one row.
// That is precisely the upstream behaviour this issue rejects: over-limit or
// malformed ids are DROPPED (observerIDRejectReason), never truncated.
//
// Everything else an observer publishes — its display name, region, model,
// firmware, client version and radio line — is DISPLAY TEXT. None of it keys a
// row, so it is safe to normalize: strip control bytes, then truncate on a rune
// boundary (normalizeObserverText). A multi-byte name is never split mid-rune,
// and a stray control byte (the CI fixture carries a radio value ending in a
// bare CR) is removed rather than stored.
//
// # What counts as a control character
//
// Both contracts share one definition, in observerRuneIsControl and
// observerRuneIsBidiControl:
//
//   - C0 controls (U+0000-U+001F) and DEL (U+007F) — the classic case, and the
//     one the CI fixture exercises with its trailing-CR radio value.
//   - C1 controls (U+0080-U+009F). These are controls that encode as TWO bytes
//     in UTF-8, so the obvious byte-oriented "< 0x20" test misses them
//     completely.
//   - Every codepoint with Unicode's Bidi_Control property — all twelve: the
//     marks U+061C, U+200E and U+200F, the embeddings/overrides U+202A-U+202E
//     and the isolates U+2066-U+2069. Not controls in the Unicode sense but
//     invisible formatting codepoints that change how the text around them
//     renders, so an id or name can DISPLAY as a different string than the one
//     the row actually holds. For identity that is the sharper case than
//     reordering: an id differing from another only by an invisible mark
//     renders identically to it while keying its own row, so an operator
//     cannot tell the two apart in the UI. The diagnostic below quotes its
//     sample with %q, which escapes all of these, so this is a UI-spoofing
//     concern rather than a log-injection sink.
//
// Everything else is left alone. Ordinary non-ASCII text passes through
// untouched — Danish letters and emoji both appear in real observer names — as
// do the codepoints bordering the ranges above (NBSP U+00A0, U+202F, U+2065,
// U+206A). An id carrying a forbidden codepoint is REJECTED; display text has
// it STRIPPED.
//
// Deliberately NOT covered: invisible codepoints that are not Bidi_Control —
// ZWSP U+200B, ZWNJ U+200C, ZWJ U+200D, WJ U+2060, BOM U+FEFF, SHY U+00AD and
// the variation selectors. They raise the same "two ids render the same"
// question, but ZWJ and the variation selectors are load-bearing inside
// legitimate text (emoji sequences, Indic scripts), so excluding them is a
// separate decision from this one, with its own blast radius. The boundary is
// pinned by test rather than left implicit.
//
// # These bounds are damage containment, not authentication
//
// Observer ids and metadata arrive unauthenticated over MQTT; the only binding
// between a topic and a publisher is whatever ACL the broker enforces, which is
// an operator deployment property. Bounding field lengths caps the blast radius
// of a hostile or broken publisher (storage per row, log volume, and the chance
// of an id collision); it does not make the data trustworthy, and — see the PR
// text — it does NOT by itself stop a publisher from minting unlimited DISTINCT
// (in-bounds) observer ids. The observer blacklist and IATA whitelist in
// handleMessage remain the policy knobs for that.
//
// # Configurability
//
// All limits are hardcoded here. Per AGENTS rule 8 they are candidates for the
// customizer/config surface in a later milestone; nothing in this file reads
// config today.
const (
	// maxObserverIDBytes bounds the observer id. Measured in BYTES because the
	// id is an opaque key, not rendered text, and the bound exists to cap the
	// primary-key/foreign-key storage it drives across observers,
	// observer_neighbors, observer_neighbor_metrics and observer_metrics.
	//
	// Derivation: the longest structured id is a full MeshCore node public key,
	// 64 lowercase-hex characters (nodePubkeyHexLen in neighbor_report_bounds.go
	// once #337 lands; 64 regardless). Non-pubkey operator ids in the CI fixture
	// (test-fixtures/e2e-fixture.db) are far shorter — "kpabap" is 6 bytes, and
	// the longest id in the fixture is a 64-char pubkey. 128 is 2x the longest
	// real id: comfortably above anything legitimate, while still bounding a
	// hostile segment. An id over this is rejected, not shortened.
	maxObserverIDBytes = 128

	// Display-text rune caps. Truncation is on a rune boundary, so a cap of N
	// runes stores at most 4*N bytes. All are measured in RUNES, not bytes,
	// because these fields are rendered and may legitimately carry multi-byte
	// characters (the fixture has observer names with emoji).

	// maxObserverNameRunes bounds observers.name (the status `origin` /
	// per-packet `origin`). Fixture display names reach 27 runes and carry emoji
	// ("Key Route Inspector-BRK \U0001F5DD"); 64 runes is >2x the longest real
	// name with rune-safe truncation so a multi-byte name is never split.
	maxObserverNameRunes = 64

	// maxObserverRegionRunes bounds observers.iata (topic region parts[1] and
	// the payload `region` override). Fixture iata values are 3-letter IATA
	// codes (OAK/SJC/MRY/SFO); 32 runes is ~10x that. Operator-configured
	// MQTTSource.Region values are the same IATA shape and sit far below the
	// cap, so configured-source behaviour is unchanged — the cap only bites the
	// untrusted topic/payload region segments. (Same 32 the #110 region-warn
	// path already uses for a topic segment: iataWarnMaxKeyLen.)
	maxObserverRegionRunes = 32

	// maxObserverModelRunes bounds observers.model. The fixture carries no model
	// strings; known MeshCore model labels ("Heltec V3", "RAK4631", "L1") are a
	// handful of characters. 48 runes is generous headroom.
	maxObserverModelRunes = 48

	// maxObserverFirmwareRunes bounds observers.firmware (status `firmware` /
	// `firmware_version`). The fixture carries none, but firmware strings share
	// the version-string shape of client_version (observed up to 52 bytes), so
	// 64 runes covers them with headroom.
	maxObserverFirmwareRunes = 64

	// maxObserverClientVersionRunes bounds observers.client_version. The longest
	// in the fixture is 52 bytes
	// ("meshcore/1.14.1-letsmesh.net-dev-2026-03-20-abe6e046"); 64 runes covers
	// the longest observed with headroom.
	maxObserverClientVersionRunes = 64

	// maxObserverRadioRunes bounds observers.radio — the single free-text radio
	// line ("freq,bw,sf,cr"), the whole of what this codebase stores as "radio
	// metadata". The longest in the fixture is 21 bytes ("910.5250244,62.5,7,5"),
	// and one fixture value ends in a stray CR that control-stripping removes;
	// 64 runes is 3x the longest observed.
	maxObserverRadioRunes = 64

	// observerIDDropLogInterval rate-limits the invalid-id diagnostic. One
	// aggregated line per interval is enough to make a drop visible without
	// letting a publisher that sprays malformed ids drive unbounded log volume.
	observerIDDropLogInterval = 5 * time.Minute
)

// Reject reasons returned by observerIDRejectReason. A small closed set so the
// drop throttle can count per reason in O(1) state: a single counter cannot say
// which of several reasons a batch of drops carried, and labelling the whole
// batch with the last one seen mislabels the rest.
const (
	observerIDRejectTooLong     = "too-long"
	observerIDRejectInvalidUTF8 = "invalid-utf8"
	observerIDRejectControlChar = "control-char"
	observerIDRejectBidiControl = "bidi-control"
	// observerIDRejectOther is never returned by observerIDRejectReason. It is
	// the throttle's catch-all slot, so a reason added to the gate without a
	// counter still surfaces in the aggregate instead of being miscounted as
	// one of the reasons above.
	observerIDRejectOther = "other"

	// observerIDDropMixedSources stands in for the source tag when the
	// aggregated batch came from more than one MQTT source. See
	// observerIDDropThrottle.record.
	observerIDDropMixedSources = "multiple sources"
)

// observerIDRejectReasonOrder fixes both the throttle's counter slots and the
// order reasons appear in the aggregated log line. observerIDRejectOther is
// last and acts as the catch-all.
var observerIDRejectReasonOrder = [...]string{
	observerIDRejectTooLong,
	observerIDRejectInvalidUTF8,
	observerIDRejectControlChar,
	observerIDRejectBidiControl,
	observerIDRejectOther,
}

// observerIDRejectSlot maps a reason to its counter slot — O(1) over a
// five-element array. An unrecognised reason lands in the catch-all.
func observerIDRejectSlot(reason string) int {
	for i, r := range observerIDRejectReasonOrder {
		if r == reason {
			return i
		}
	}
	return len(observerIDRejectReasonOrder) - 1
}

// observerRuneIsControl reports whether r is a control character: a C0 control
// (U+0000-U+001F), DEL (U+007F), or a C1 control (U+0080-U+009F). C1 is the
// easy one to miss — those codepoints are two bytes in UTF-8, so a byte-wise
// "< 0x20" check never sees them. See this file's header.
func observerRuneIsControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// observerRuneIsBidiControl reports whether r is one of the twelve codepoints
// Unicode gives the Bidi_Control property: the marks U+061C (ALM), U+200E
// (LRM) and U+200F (RLM), the embeddings/overrides U+202A-U+202E, and the
// isolates U+2066-U+2069. All twelve are invisible and all twelve affect how
// the text around them renders, so they can make a stored id or name display
// as something it is not. See this file's header.
//
// The three marks are listed individually because they do not sit in a range
// with the others — which is exactly why they were missed first time round.
func observerRuneIsBidiControl(r rune) bool {
	switch r {
	case 0x061c, 0x200e, 0x200f:
		return true
	}
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// observerIDRejectReason returns a short machine reason an observer id must be
// rejected, or "" if the id is acceptable. Acceptable ids are stored verbatim —
// never lowercased, truncated or otherwise rewritten — so identity is preserved
// exactly, including for non-pubkey operator ids like the fixture's "kpabap".
//
// Rejection reasons, in priority order:
//   - "too-long": more than maxObserverIDBytes bytes. Dropped, not truncated,
//     so two long ids sharing a prefix can never collapse to one row.
//   - "invalid-utf8": not valid UTF-8. A topic segment the broker delivered as
//     raw bytes that are not UTF-8 cannot name a real observer. Checked before
//     the rune scan below so that scan only ever sees real codepoints.
//   - "control-char": contains a C0 control, DEL, or a C1 control. Real ids are
//     hex pubkeys or printable operator labels; a control character signals a
//     malformed segment.
//   - "bidi-control": contains a Bidi_Control codepoint (mark, embedding/
//     override or isolate), which would render the id as a different string
//     than the one rows are keyed on.
//
// The empty id is intentionally NOT rejected here: that is an existing,
// orthogonal edge (a malformed "meshcore//status" topic) whose behaviour this
// change deliberately leaves untouched.
func observerIDRejectReason(id string) string {
	if id == "" {
		return ""
	}
	if len(id) > maxObserverIDBytes {
		return observerIDRejectTooLong
	}
	if !utf8.ValidString(id) {
		return observerIDRejectInvalidUTF8
	}
	for _, r := range id {
		switch {
		case observerRuneIsControl(r):
			return observerIDRejectControlChar
		case observerRuneIsBidiControl(r):
			return observerIDRejectBidiControl
		}
	}
	return ""
}

// normalizeObserverText is the display-text contract for observer metadata: it
// strips every control and bidi-formatting codepoint this file's header lists,
// then truncates to maxRunes on a rune boundary. Stripping happens before
// counting, so a field padded with control characters is not counted against
// the visible cap. An empty input (or one that is entirely stripped) returns "".
//
// Truncation iterates runes and stops at the cap, so a multi-byte rune is never
// split — the invariant the Unicode test pins. maxRunes <= 0 means "strip only,
// do not truncate".
//
// The builder is pre-sized from the cap rather than from len(s): see the
// comment at the Grow call.
func normalizeObserverText(s string, maxRunes int) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	// Size the builder from what this call can RETURN, not from what it was
	// handed. The output is at most maxRunes runes, i.e. 4*maxRunes bytes in
	// UTF-8, so a hostile 4 MiB display field no longer buys a 4 MiB
	// allocation to produce 64 runes — there is no payload size cap upstream
	// (handleMessage hands json.Unmarshal the raw payload), so the field length
	// is a broker deployment property. min() keeps the single-allocation intent
	// for the common short field. maxRunes <= 0 is "strip only, do not
	// truncate", where the input length IS the bound on the output.
	if maxRunes > 0 {
		b.Grow(min(len(s), 4*maxRunes)) // 4 bytes is the widest UTF-8 rune
	} else {
		b.Grow(len(s))
	}
	n := 0
	for _, r := range s {
		if observerRuneIsControl(r) || observerRuneIsBidiControl(r) {
			continue // see the control-character policy in this file's header
		}
		if maxRunes > 0 && n == maxRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// boundObserverMeta returns a copy of meta with its four free-text fields
// (model, firmware, client_version, radio) run through the display-text
// contract. Numeric fields and CanRelay are carried through unchanged. A field
// that normalizes to "" is set back to nil so the COALESCE in the upsert leaves
// the stored column untouched (the #1290 "field absent -> keep prior value"
// contract), rather than overwriting it with an empty string.
func boundObserverMeta(meta *ObserverMeta) *ObserverMeta {
	if meta == nil {
		return nil
	}
	out := *meta
	out.Model = boundObserverMetaField(meta.Model, maxObserverModelRunes)
	out.Firmware = boundObserverMetaField(meta.Firmware, maxObserverFirmwareRunes)
	out.ClientVersion = boundObserverMetaField(meta.ClientVersion, maxObserverClientVersionRunes)
	out.Radio = boundObserverMetaField(meta.Radio, maxObserverRadioRunes)
	return &out
}

// boundObserverMetaField applies normalizeObserverText to an optional string
// field, collapsing an empty result back to nil (see boundObserverMeta).
func boundObserverMetaField(p *string, maxRunes int) *string {
	if p == nil {
		return nil
	}
	v := normalizeObserverText(*p, maxRunes)
	if v == "" {
		return nil
	}
	return &v
}

// normalizeObserverRegion is the display-text contract for the region that
// lands in observers.iata: strip controls and truncate, preserving the existing
// TrimSpace+ToUpper the upsert paths already applied. Split out so the two
// upsert methods share one definition.
func normalizeObserverRegion(iata string) string {
	return strings.TrimSpace(strings.ToUpper(normalizeObserverText(iata, maxObserverRegionRunes)))
}

// observerIDDropThrottle aggregates invalid-observer-id drops into one
// rate-limited log line. It holds O(1) state — one counter and one bounded
// sample PER REJECT REASON plus a timestamp, never a per-id table — on
// purpose: a publisher spraying distinct malformed ids must not be able to grow
// this throttle's memory, only its (capped) drop counts. The full id is never
// retained or logged (PII / fingerprinting, per #1211); record stores only the
// short prefix sample, and the line quotes it with %q.
//
// Counting per reason, rather than one counter plus whichever reason happened
// to arrive last, is what keeps the aggregated line honest: a batch that mixes
// too-long, invalid-utf8 and control-char drops cannot be labelled with any
// single one of them without mislabelling the others.
//
// The SOURCE tag has the same problem and gets the same treatment. This
// throttle is process-wide across every MQTT source, so printing whichever
// source happened to trigger the flush attributes one source's drops to
// another. Per-source counters would make the state grow with the source list,
// which is the property this throttle exists to keep; instead the batch
// remembers one tag and whether a second, different one contributed, and the
// line names the source only when that is true — otherwise
// observerIDDropMixedSources. Same rule as omitting a reason that did not
// occur: say what is known, do not invent the rest.
type observerIDDropThrottle struct {
	mu      sync.Mutex
	dropped [len(observerIDRejectReasonOrder)]int64
	samples [len(observerIDRejectReasonOrder)]string
	// tag is the source tag of the batch accumulated since the last line, and
	// tagMixed records that a different source also contributed to it. Both
	// are reset per batch by flushLocked.
	tag        string
	tagMixed   bool
	lastLogged time.Time
}

// record notes one dropped message and logs an aggregated line at most once per
// observerIDDropLogInterval. The first drop logs immediately (lastLogged zero),
// so a drop is never invisible; subsequent drops within the interval accumulate
// silently and are reported in the next line. now is passed in (not read from
// the wall clock) so tests can drive the throttle deterministically.
func (t *observerIDDropThrottle) record(tag, sampleID, reason string, now time.Time) {
	slot := observerIDRejectSlot(reason)
	// Bound the sample on the way IN, so the throttle never holds a full id
	// between lines. strings.Clone is load-bearing, not decoration:
	// observerIDLogSample returns a SLICE of the id, which in Go keeps the
	// whole backing array (the id, and the MQTT topic string it was cut from)
	// reachable for as long as the slice is retained. Copying is what actually
	// makes the retained state the 8-rune prefix and nothing more.
	sample := strings.Clone(observerIDLogSample(sampleID))

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pendingLocked() == 0 {
		// First drop of a new batch: it owns the tag until another source
		// contributes to the same batch.
		t.tag, t.tagMixed = tag, false
	} else if tag != t.tag {
		t.tagMixed = true
	}
	t.dropped[slot]++
	t.samples[slot] = sample
	if !t.lastLogged.IsZero() && now.Sub(t.lastLogged) < observerIDDropLogInterval {
		return
	}
	total, source, breakdown := t.flushLocked()
	t.lastLogged = now
	log.Printf("MQTT [%s] [observer-id] dropped %d message(s) with invalid observer id (%s); further invalid ids suppressed for %s",
		source, total, breakdown, observerIDDropLogInterval)
}

// flushLocked zeroes the per-reason counters and samples and the batch's
// source attribution, and returns the total drop count, the source label for
// the line, and a breakdown naming only the reasons that actually occurred,
// each with its own count and its own most recent sample. A reason with no
// drops is omitted entirely rather than printed as "reason=0", and a batch
// several sources contributed to is labelled observerIDDropMixedSources rather
// than with any one of them. Caller holds t.mu.
func (t *observerIDDropThrottle) flushLocked() (int64, string, string) {
	source := t.tag
	if t.tagMixed {
		source = observerIDDropMixedSources
	}
	t.tag, t.tagMixed = "", false
	var total int64
	var b strings.Builder
	for i, reason := range observerIDRejectReasonOrder {
		n := t.dropped[i]
		if n == 0 {
			continue
		}
		total += n
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d sample=%q", reason, n, t.samples[i])
		t.dropped[i] = 0
		t.samples[i] = ""
	}
	return total, source, b.String()
}

// pendingDropped reports how many drops have accumulated across all reasons
// since the last logged line. Lets tests assert the accumulate-then-flush
// behaviour without reaching into the locked counters.
func (t *observerIDDropThrottle) pendingDropped() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pendingLocked()
}

// pendingLocked is pendingDropped without the lock, for callers that already
// hold t.mu. A zero total means the next drop starts a fresh batch — which is
// what record uses it for.
func (t *observerIDDropThrottle) pendingLocked() int64 {
	var n int64
	for _, c := range t.dropped {
		n += c
	}
	return n
}

// observerIDLogSample returns a bounded, log-safe prefix of a rejected id: at
// most 8 runes. The log line quotes it with %q, so any control or bidi
// codepoint that this prefix carries (a control-char or bidi-control rejection
// samples the offending value) is escaped and cannot forge or reorder a log
// line; capping the length keeps even a huge id's diagnostic short, and the
// throttle calls this on the way in so it never retains the full id.
func observerIDLogSample(id string) string {
	n := 0
	for i := range id {
		if n == 8 {
			return id[:i]
		}
		n++
	}
	return id
}

// observerIDDrops is the process-wide throttle for the handleMessage id gate.
var observerIDDrops = &observerIDDropThrottle{}
