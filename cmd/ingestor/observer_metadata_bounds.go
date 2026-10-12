package main

import (
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

// observerIDRejectReason returns a short machine reason an observer id must be
// rejected, or "" if the id is acceptable. Acceptable ids are stored verbatim —
// never lowercased, truncated or otherwise rewritten — so identity is preserved
// exactly, including for non-pubkey operator ids like the fixture's "kpabap".
//
// Rejection reasons, in priority order:
//   - "too-long": more than maxObserverIDBytes bytes. Dropped, not truncated,
//     so two long ids sharing a prefix can never collapse to one row.
//   - "invalid-utf8": not valid UTF-8. A topic segment the broker delivered as
//     raw bytes that are not UTF-8 cannot name a real observer.
//   - "control-char": contains a C0 control byte (< 0x20) or DEL (0x7f). Real
//     ids are hex pubkeys or printable operator labels; a control byte signals
//     a malformed segment and is a log-injection vector.
//
// The empty id is intentionally NOT rejected here: that is an existing,
// orthogonal edge (a malformed "meshcore//status" topic) whose behaviour this
// change deliberately leaves untouched.
func observerIDRejectReason(id string) string {
	if id == "" {
		return ""
	}
	if len(id) > maxObserverIDBytes {
		return "too-long"
	}
	if !utf8.ValidString(id) {
		return "invalid-utf8"
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return "control-char"
		}
	}
	return ""
}

// normalizeObserverText is the display-text contract for observer metadata: it
// strips C0 control bytes and DEL, then truncates to maxRunes on a rune
// boundary. Stripping happens before counting, so a field padded with control
// bytes is not counted against the visible cap. An empty input (or one that is
// entirely control bytes) returns "".
//
// Truncation iterates runes and stops at the cap, so a multi-byte rune is never
// split — the invariant the Unicode test pins. maxRunes <= 0 means "strip only,
// do not truncate".
func normalizeObserverText(s string, maxRunes int) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue // strip C0 controls + DEL
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
// rate-limited log line. It holds O(1) state — a single counter and timestamp,
// not a per-id table — on purpose: a publisher spraying distinct malformed ids
// must not be able to grow this throttle's memory, only its (capped) drop
// count. The full id is never logged (PII / fingerprinting, per #1211); only a
// short, control-escaped prefix sample accompanies the aggregate count.
type observerIDDropThrottle struct {
	mu         sync.Mutex
	dropped    int64
	lastLogged time.Time
}

// record notes one dropped message and logs an aggregated line at most once per
// observerIDDropLogInterval. The first drop logs immediately (lastLogged zero),
// so a drop is never invisible; subsequent drops within the interval accumulate
// silently and are reported in the next line. now is passed in (not read from
// the wall clock) so tests can drive the throttle deterministically.
func (t *observerIDDropThrottle) record(tag, sampleID, reason string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropped++
	if !t.lastLogged.IsZero() && now.Sub(t.lastLogged) < observerIDDropLogInterval {
		return
	}
	n := t.dropped
	t.dropped = 0
	t.lastLogged = now
	log.Printf("MQTT [%s] [observer-id] dropped %d message(s) with invalid observer id (reason=%s, sample=%q); further invalid ids suppressed for %s",
		tag, n, reason, observerIDLogSample(sampleID), observerIDDropLogInterval)
}

// observerIDLogSample returns a bounded, log-safe prefix of a rejected id: at
// most 8 runes. The caller quotes it with %q, so any control bytes that slipped
// through (a control-char rejection logs the offending value) are escaped and
// cannot forge log lines; capping the length keeps even a huge id's diagnostic
// short.
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
