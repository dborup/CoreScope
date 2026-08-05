// Package main: Ping Scores history conversion -- Phase 4C of the
// recompute redesign (see reviews/CoreScope-code-review-2026-08-04.md and
// the accompanying design discussion).
//
// This file holds the pure, I/O-free translation layer between a
// computed *PingScore (ping_scores.go) and a persisted
// PingScoreHistoryEntry (ping_score_history.go): converting a fresh
// computation into a brand-new entry, merging a fresh computation into an
// existing entry, and materializing a persisted entry back into a
// *PingScore for snapshot building. None of this is wired into
// computeAllPingScores, StartPingScoresRecomputer, or main.go yet -- that
// is Phase 4D+.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// marshalRelayPubkeysJSON serializes a set of relay pubkeys
// deterministically: deduplicated and sorted ascending before encoding, so
// two entries built from the same underlying relay SET always produce
// byte-identical JSON regardless of map iteration order (score.relayPubkeys
// is built from map iteration in buildPingScoreFromPath). No error return:
// json.Marshal of a []string cannot fail (no channels/functions/cycles/
// invalid floats involved), so there is no scenario here to handle.
func marshalRelayPubkeysJSON(pubkeys []string) string {
	if len(pubkeys) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(pubkeys))
	unique := make([]string, 0, len(pubkeys))
	for _, pk := range pubkeys {
		if seen[pk] {
			continue
		}
		seen[pk] = true
		unique = append(unique, pk)
	}
	sort.Strings(unique)
	b, _ := json.Marshal(unique) // infallible for []string, see doc comment above
	return string(b)
}

// unmarshalRelayPubkeysJSON is marshalRelayPubkeysJSON's inverse. Unlike
// the write side, this CAN fail -- the column holds whatever was last
// written, and a corrupt/truncated file, a hand-edited row, or a future
// schema mistake could all produce a value that isn't valid JSON. Returns
// an error rather than silently treating invalid content as an empty
// list: a relay set silently going empty would permanently and invisibly
// erase that ping's contribution to the relay leaderboard.
func unmarshalRelayPubkeysJSON(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("invalid relay_pubkeys_json %q: %w", raw, err)
	}
	return out, nil
}

// PingScoreHistoryEntryState bundles the settle/deep-sweep state fields
// pingScoreHistoryEntryFromScore and mergePingScoreHistoryEntry carry
// through verbatim -- neither function computes or derives them. That
// state machine (when a ping becomes "settled", when a deep-sweep last
// ran) is Phase 4D+; Phase 4C's job is only the conversion/merge
// mechanics, so callers pass through whatever state they already have
// (e.g. all zero values for a ping's very first conversion).
type PingScoreHistoryEntryState struct {
	StableSince     string
	Settled         bool
	DataPruned      bool
	LastDeepSweptAt string
}

// pingScoreHistoryEntryFromScore converts a trigger + freshly computed
// score (nil when GetPacketPath/GetPacketPathsBulk produced no usable path
// this cycle -- see buildPingScoreFromPath's own nil contract) + observation
// fingerprint + carried-through state into a brand-new PingScoreHistoryEntry
// ready to persist. Used for a tx_id that has no existing entry yet; see
// mergePingScoreHistoryEntry for updating one that already does.
//
// Display names (DeepestName/FarthestName/firstName, and every leaderboard
// Name) are never persisted -- PingScoreHistoryEntry deliberately excludes
// them (see its own doc comment); materializePingScoreFromHistoryEntry
// leaves them empty, and buildPingScoresSnapshotFromHistory re-resolves
// them fresh at snapshot-build time instead, so a later node/observer
// rename is never frozen into stale history.
func pingScoreHistoryEntryFromScore(
	trigger pingTriggerRow,
	score *PingScore,
	fingerprint observationFingerprint,
	state PingScoreHistoryEntryState,
	computedAt time.Time,
) PingScoreHistoryEntry {
	e := PingScoreHistoryEntry{
		TxID:             trigger.txID,
		Hash:             trigger.hash,
		Sender:           trigger.sender,
		ChannelHash:      trigger.channelHash,
		Timestamp:        trigger.firstSeen,
		FingerprintCount: fingerprint.Count,
		FingerprintMaxID: fingerprint.MaxID,
		StableSince:      state.StableSince,
		Settled:          state.Settled,
		DataPruned:       state.DataPruned,
		LastDeepSweptAt:  state.LastDeepSweptAt,
		ComputedAt:       computedAt.UTC().Format(time.RFC3339),
	}
	if score == nil {
		e.Unscorable = true
		return e
	}
	e.StationCount = score.StationCount
	e.DeepestHops = score.DeepestHops
	e.DeepestPubkey = score.DeepestPubkey
	e.FarthestKm = score.FarthestKm
	e.FarthestPubkey = score.FarthestPubkey
	e.SpreadSeconds = score.SpreadSeconds
	e.AirtimeMs = score.AirtimeMs
	e.RelayCount = score.RelayCount
	e.RelayPubkeysJSON = marshalRelayPubkeysJSON(score.relayPubkeys)
	e.FirstPubkey = score.firstPubkey
	return e
}

// mergePingScoreHistoryEntry combines an EXISTING persisted entry with a
// freshly computed score for the SAME tx_id (score is nil when this
// cycle's computation produced no usable path -- see
// buildPingScoreFromPath's nil contract), producing the entry to persist
// next. Pure function: no I/O, no *DB/*Server, safe to unit-test
// exhaustively with a table. For a tx_id with NO existing entry, use
// pingScoreHistoryEntryFromScore instead -- this function assumes existing
// reflects genuine prior state (in particular, existing.Unscorable already
// correctly reflects "has this tx_id EVER been successfully scored",
// which a synthetic zero-value PingScoreHistoryEntry would not).
//
// Rules (all deliberate, not incidental -- see the Phase 4 design report
// for the full rationale):
//   - score != nil ("a successful computation") replaces every path fact
//     (StationCount, DeepestHops, DeepestPubkey, FarthestKm, FarthestPubkey,
//     SpreadSeconds, RelayPubkeysJSON, FirstPubkey) with this cycle's
//     values, and clears Unscorable.
//   - score == nil ("an empty result") leaves every path fact AND
//     Unscorable exactly as they were on entry -- an empty cycle can never
//     downgrade an entry that was ever successfully scored, and Unscorable
//     only stays true when there has NEVER been a successful computation
//     for this tx_id (i.e. it was already true on entry).
//   - AirtimeMs locks at the first non-nil, POSITIVE value it ever sees
//     (existing OR new) and is never overwritten again, including by a
//     later nil/non-positive value. RelayCount locks in the very same
//     step, as a pair -- it never changes independently of AirtimeMs.
//   - Fingerprint/StableSince/Settled/LastDeepSweptAt are taken verbatim
//     from update -- this function never derives them from score.
//   - DataPruned is always carried through from existing, unmodified --
//     setting it is retention/evidence logic for a later phase, not this
//     general-purpose merge.
//   - KmPerSecondAirtime is never touched (PingScoreHistoryEntry doesn't
//     even have the field -- it's derived fresh at materialization time,
//     see materializePingScoreFromHistoryEntry).
func mergePingScoreHistoryEntry(
	existing PingScoreHistoryEntry,
	trigger pingTriggerRow,
	score *PingScore,
	update PingScoreHistoryEntryState,
	fingerprint observationFingerprint,
	computedAt time.Time,
) PingScoreHistoryEntry {
	merged := existing
	merged.TxID = trigger.txID
	merged.Hash = trigger.hash
	merged.Sender = trigger.sender
	merged.ChannelHash = trigger.channelHash
	merged.Timestamp = trigger.firstSeen

	if score != nil {
		merged.Unscorable = false
		merged.StationCount = score.StationCount
		merged.DeepestHops = score.DeepestHops
		merged.DeepestPubkey = score.DeepestPubkey
		merged.FarthestKm = score.FarthestKm
		merged.FarthestPubkey = score.FarthestPubkey
		merged.SpreadSeconds = score.SpreadSeconds
		merged.FirstPubkey = score.firstPubkey
		merged.RelayPubkeysJSON = marshalRelayPubkeysJSON(score.relayPubkeys)

		airtimeAlreadyLocked := existing.AirtimeMs != nil && *existing.AirtimeMs > 0
		newAirtimeIsValid := score.AirtimeMs != nil && *score.AirtimeMs > 0
		if !airtimeAlreadyLocked && newAirtimeIsValid {
			merged.AirtimeMs = score.AirtimeMs
			merged.RelayCount = score.RelayCount
		}
		// airtimeAlreadyLocked: merged.AirtimeMs/RelayCount already equal
		// existing's (copied via `merged := existing` above) -- left
		// untouched deliberately, not by omission.
	}
	// score == nil: merged's path facts and Unscorable already equal
	// existing's (copied via `merged := existing` above) -- an empty
	// result never downgrades a prior valid score, and never marks a
	// never-scored entry as scorable either. Left untouched deliberately.

	merged.FingerprintCount = fingerprint.Count
	merged.FingerprintMaxID = fingerprint.MaxID
	merged.StableSince = update.StableSince
	merged.Settled = update.Settled
	merged.LastDeepSweptAt = update.LastDeepSweptAt
	merged.DataPruned = existing.DataPruned
	merged.ComputedAt = computedAt.UTC().Format(time.RFC3339)
	return merged
}

// materializePingScoreFromHistoryEntry reconstructs a *PingScore from a
// persisted history entry -- the inverse of pingScoreHistoryEntryFromScore/
// mergePingScoreHistoryEntry, used at snapshot-build time
// (buildPingScoresSnapshotFromHistory). Returns (nil, nil) for an
// Unscorable entry, matching buildPingScoreFromPath's own nil-means-
// nothing-to-show contract.
//
// Display names (DeepestName, FarthestName, firstName) are intentionally
// left empty here -- never sourced from history. buildPingScoresSnapshotFromHistory's
// name-enrichment pass fills them in afterward from a fresh bulk lookup,
// so a node/observer rename since this ping was last computed is always
// reflected, never frozen at persist time.
//
// KmPerSecondAirtime is NEVER read from the entry (it isn't persisted) --
// always derived fresh here from FarthestKm/AirtimeMs, matching
// buildPingScoreFromPath's own derivation exactly: nil unless both
// FarthestKm and a positive AirtimeMs are present.
//
// relayPubkeysJSON must be valid JSON (a []string) if non-empty -- an
// invalid value is a data-integrity problem, not an absent-value case (see
// unmarshalRelayPubkeysJSON), so it is propagated as an error rather than
// silently degrading to an empty relay list.
func materializePingScoreFromHistoryEntry(e PingScoreHistoryEntry) (*PingScore, error) {
	if e.Unscorable {
		return nil, nil
	}
	relayPubkeys, err := unmarshalRelayPubkeysJSON(e.RelayPubkeysJSON)
	if err != nil {
		return nil, fmt.Errorf("materialize tx_id=%d: %w", e.TxID, err)
	}
	score := &PingScore{
		Hash:           e.Hash,
		Sender:         e.Sender,
		ChannelHash:    e.ChannelHash,
		Timestamp:      e.Timestamp,
		StationCount:   e.StationCount,
		DeepestHops:    e.DeepestHops,
		DeepestPubkey:  e.DeepestPubkey,
		FarthestKm:     e.FarthestKm,
		FarthestPubkey: e.FarthestPubkey,
		SpreadSeconds:  e.SpreadSeconds,
		AirtimeMs:      e.AirtimeMs,
		RelayCount:     e.RelayCount,
		relayPubkeys:   relayPubkeys,
		firstPubkey:    e.FirstPubkey,
	}
	if e.FarthestKm != nil && e.AirtimeMs != nil && *e.AirtimeMs > 0 {
		kmPerSec := *e.FarthestKm / (*e.AirtimeMs / 1000.0)
		score.KmPerSecondAirtime = &kmPerSec
	}
	return score, nil
}
