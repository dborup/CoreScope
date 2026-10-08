package main

import "time"

// Bounds and validators for observer /neighbors report evidence (#337).
//
// # Trust model — read this before relying on anything below
//
// Everything these helpers accept is still UNAUTHENTICATED. A well-formed
// public key proves only that the string is a syntactically valid 32-byte
// MeshCore key; a timestamp inside the tolerance window proves only that the
// publisher's clock is plausible. NEITHER authenticates the publisher, and
// neither proves that the publisher owns the `origin_id` it claims or that it
// ever heard the neighbors it lists. Any party that can publish to
// meshcore/<region>/<observer_id>/neighbors can assert any origin_id and any
// neighbor set; the only binding between a topic and a publisher is whatever
// ACL the broker enforces, which is an operator deployment property, not
// something the ingestor can verify. The observer blacklist and the IATA
// whitelist in handleMessage are the policy knobs for that, not these bounds.
//
// What these bounds DO buy is damage containment: malformed keys cannot
// accumulate unjoinable rows in observer_neighbors / observer_neighbor_metrics,
// an oversized string cannot be stored or normalized, an unbounded neighbor
// array cannot multiply per-report write work, and a publisher-chosen
// far-future timestamp cannot win the lexicographic last-write-wins comparison
// against every genuine report for years to come. They bound the blast radius
// of a hostile or broken publisher. They do not make the data trustworthy.
//
// # Configurability
//
// All four values are deliberately hardcoded here. Per AGENTS rule 8 they are
// candidates for the customizer/config surface in a later milestone —
// maxReportFutureSkew especially, since it is the one an operator running
// deliberately-skewed test hardware might want to widen. Nothing in this file
// reads config today.
const (
	// nodePubkeyHexLen is the hex length of a full MeshCore node public key
	// (32 bytes). nodes.public_key, inactive_nodes.public_key and
	// observer_neighbors.neighbor_pubkey all hold the FULL key, never a
	// prefix: the hash-prefix disambiguation work (#215, docs/HASH-PREFIX-
	// DISAMBIGUATION.md) exists precisely because short prefixes collide, so
	// validation here must reject anything that is not a full key rather than
	// shorten one to fit. Every one of the 200 nodes in the CI fixture
	// (test-fixtures/e2e-fixture.db) carries a 64-char lowercase-hex key.
	nodePubkeyHexLen = 64

	// maxReportScopeBytes bounds a report's scope string, measured on the RAW
	// field BEFORE normalization.
	//
	// Derivation: a neighbor's scope list reaches the observer as the response
	// to an over-the-air scope query, and a MeshCore frame cannot carry more
	// than MAX_PACKET_PAYLOAD = 184 bytes (pinned as maxPacketPayload in
	// decoder.go, from firmware MeshCore.h:19). 256 bytes is ~39% headroom
	// over that physical ceiling, so no scope list the radio can actually
	// deliver is ever rejected. self.scopes comes from the observer's own
	// local config rather than the air, but it is the same field shape and is
	// held to the same bound.
	//
	// Why raw and not normalized (which is how upstream frames the same
	// limit): normalizeConfiguredScopeList ADDS a "#" per comma-separated
	// part, so the normalized string is always at least as long as what the
	// radio delivered. Measuring after normalization would reject input the
	// firmware can legitimately produce — a 184-byte list of 92 one-character
	// scopes normalizes to 276 bytes, past a 256-byte normalized limit.
	// Measuring the raw field avoids that false rejection while still
	// bounding what gets stored (at most 256+128 = 384 bytes in the
	// pathological one-character-scope case, at most 257 for any list whose
	// parts are two characters or more) and bounding the work normalization
	// itself does on a hostile string.
	maxReportScopeBytes = 256

	// maxReportNeighbors bounds how many neighbor entries one report may
	// contribute. The firmware's own report is 10 KB-capped and truncates by
	// ordering (see handleNeighborsReport); a minimal entry
	// {"pubkey":"<64 hex>","status":"timeout"}, is ~95 bytes, so a legitimate
	// 10 KB report carries at most ~110 entries. 256 is over twice that, and
	// caps per-report write work at 256 conditional-UPDATE transactions for
	// configured_scope plus 256 observer_neighbors and 256
	// observer_neighbor_metrics inserts. Entries past the cap are dropped from
	// the tail, matching how the firmware already shortens an oversized
	// report.
	maxReportNeighbors = 256

	// maxReportFutureSkew is how far ahead of ingest time a report timestamp
	// may be and still count as evidence.
	//
	// Upstream proposes 5 minutes. We use 15 because that is already this
	// repo's threshold for "this observer's clock is wrong": resolveRxTime's
	// naiveTolerance (#1463) is 15 minutes, and the clock-skew chip/banner
	// RecordNaiveSkew feeds (#1478) is calibrated to the same number. One
	// number for operators to reason about beats importing a second one.
	//
	// Why not resolveRxTime's 14-hour hard reject: that bound exists only
	// because naive (zone-less) timestamps are parsed as UTC, which makes a
	// UTC+14 observer look 14 hours ahead. normalizeReportTS accepts ONLY
	// zone-aware RFC3339, so that entire class is already rejected before
	// this check — nothing legitimate reaches this path more than clock jitter
	// ahead of now.
	//
	// The cost of 15 over 5 is bounded and small: a report accepted at the
	// edge of the window dominates last-write-wins for at most 15 minutes
	// before real time passes it. Without any bound it dominates until the
	// claimed timestamp arrives, which for a year-9999 report is forever.
	maxReportFutureSkew = 15 * time.Minute
)

// normalizeNodePubkey validates a report-supplied node public key and returns
// it in the lowercase-hex form nodes.public_key uses. Report keys are
// uppercase on the wire; the FULL 64 characters are preserved — the key is
// lowercased, never truncated to a prefix, so node identity is unchanged by
// validation.
//
// Anything that is not exactly nodePubkeyHexLen hex characters is rejected
// (ok=false): the empty string, a short legacy prefix, a 65-character key, and
// any value carrying a non-hex byte. Such a key can only ever name a node that
// cannot exist, so rejecting it costs nothing and keeps junk out of
// observer_neighbors / observer_neighbor_metrics, whose rows are inserted
// verbatim with no foreign key back to nodes.
//
// Note the asymmetry with observer ids, which are deliberately NOT validated
// this way: an observer id is an operator-chosen MQTT topic segment, and the
// CI fixture contains a real one ("kpabap") that is not a public key at all.
func normalizeNodePubkey(raw string) (string, bool) {
	if len(raw) != nodePubkeyHexLen {
		return "", false
	}
	out := []byte(raw)
	for i, c := range out {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			// already canonical
		case c >= 'A' && c <= 'F':
			out[i] = c + ('a' - 'A')
		default:
			return "", false
		}
	}
	return string(out), true
}

// reportScopeTooLong reports whether a raw scope string exceeds
// maxReportScopeBytes. Length is measured in bytes, not runes: the bound
// exists to cap storage and normalization work, and region names are ASCII in
// every deployment we have (the "#" prefix regions.Normalize adds is itself
// one byte).
func reportScopeTooLong(raw string) bool {
	return len(raw) > maxReportScopeBytes
}
