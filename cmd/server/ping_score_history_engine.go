// Package main: Ping Scores incremental recompute engine -- Phase 4D of
// the recompute redesign (see reviews/CoreScope-code-review-2026-08-04.md).
//
// pingScoreHistoryEngine is the state machine that ties together everything
// built in Phases 4A-4C: the history store (4A), the bulk query helpers
// (4B), and the conversion/merge/snapshot layer (4C). It is deliberately
// NOT constructed or driven by anything in main.go/StartPingScoresRecomputer
// yet -- Phase 4D builds and tests it in isolation so production wiring
// (a later phase) is a small, low-risk cutover rather than the place where
// all of this logic first gets exercised.
package main

import (
	"fmt"
	"sort"
	"time"
)

// pingScoreHistoryEngineConfig holds the engine's configurable thresholds.
// No default is baked into engine code for RetentionDuration in particular
// -- per the Phase 4D review, the (currently 30-day, staging-specific)
// packet retention window must be injected by the caller, never
// hardcoded here. The zero value (RetentionDuration: 0) safely disables
// DataPruned detection entirely (see maybeMarkDataPruned) rather than
// guessing a number.
type pingScoreHistoryEngineConfig struct {
	// SettleDebounce is how long an entry's fingerprint must stay
	// unchanged (measured from StableSince) before it transitions to
	// Settled=true and stops being fingerprint-checked every cycle.
	SettleDebounce time.Duration

	// DeepSweepBatchSize caps how many Settled=true, DataPruned=false
	// entries get a full independent-of-fingerprint recompute each cycle.
	DeepSweepBatchSize int

	// RetentionDuration is the packet-data retention window (how long the
	// ingestor keeps the underlying transmissions/observations rows) --
	// used both for DataPruned detection and initial-backfill-incomplete
	// bootstrap-integrity detection. <=0 disables both.
	RetentionDuration time.Duration

	// MaxEdgeKm is threaded through to GetPacketPathsBulk's geo-sanity
	// filter, matching GetPacketPath's own existing parameter.
	MaxEdgeKm float64
}

// defaultPingScoreHistoryEngineConfig returns the Phase 4 design's default
// thresholds (10-minute settle debounce, 100-entry deep-sweep batches) --
// RetentionDuration is deliberately left at zero (disabled); a caller
// wiring this up for a real server must set it explicitly.
func defaultPingScoreHistoryEngineConfig() pingScoreHistoryEngineConfig {
	return pingScoreHistoryEngineConfig{
		SettleDebounce:     10 * time.Minute,
		DeepSweepBatchSize: 100,
		MaxEdgeKm:          EstimateMaxEdgeKm,
	}
}

// pingScoreHistoryEngine is the incremental recompute state machine. NOT
// safe for concurrent use -- single-owner, matching pingScoreHistoryIndex's
// and PingScoreHistoryStore's own documented contracts (see their doc
// comments). Not registered on *Server or driven from main.go yet.
type pingScoreHistoryEngine struct {
	server *Server
	store  *PingScoreHistoryStore
	index  *pingScoreHistoryIndex
	now    func() time.Time
	config pingScoreHistoryEngineConfig
}

// newPingScoreHistoryEngine constructs an engine against an ALREADY-OPEN
// store: loads every persisted entry, builds the in-memory index, and
// verifies the store's integrity record loads cleanly -- all read-only, no
// persistent state is ever changed by construction. Returns a contextual
// error (never a partially-usable engine) if anything fails, including
// safely rejecting entries that can't be trusted: a non-positive tx_id (the
// same invariant the schema's own CHECK constraint enforces), a duplicate
// tx_id within the loaded slice (LoadAll's tx_id PRIMARY KEY makes this
// exactly as impossible as a same-DB duplicate row, but this stays a
// belt-and-suspenders check rather than an assumption), or relay_pubkeys_json
// that fails to parse (would otherwise surface much later, deep inside a
// Cycle, as a materialization error instead of an init-time one).
func newPingScoreHistoryEngine(server *Server, store *PingScoreHistoryStore, now func() time.Time, config pingScoreHistoryEngineConfig) (*pingScoreHistoryEngine, error) {
	entries, err := store.LoadAll()
	if err != nil {
		return nil, fmt.Errorf("ping score history engine: init: load entries: %w", err)
	}
	seen := make(map[int64]bool, len(entries))
	for _, e := range entries {
		if e.TxID <= 0 {
			return nil, fmt.Errorf("ping score history engine: init: entry with invalid tx_id=%d", e.TxID)
		}
		if seen[e.TxID] {
			return nil, fmt.Errorf("ping score history engine: init: duplicate tx_id=%d in loaded entries", e.TxID)
		}
		seen[e.TxID] = true
		if _, err := unmarshalRelayPubkeysJSON(e.RelayPubkeysJSON); err != nil {
			return nil, fmt.Errorf("ping score history engine: init: tx_id=%d: %w", e.TxID, err)
		}
	}
	// LoadIntegrity is called here purely so construction fails fast on a
	// broken integrity table rather than succeeding and failing later
	// inside the first Cycle -- Cycle() re-reads it itself when it actually
	// needs the current value (see maybeBuildBootstrapIntegrity), so
	// nothing from this call is retained on the engine.
	if _, err := store.LoadIntegrity(); err != nil {
		return nil, fmt.Errorf("ping score history engine: init: load integrity: %w", err)
	}

	return &pingScoreHistoryEngine{
		server: server,
		store:  store,
		index:  newPingScoreHistoryIndex(entries),
		now:    now,
		config: config,
	}, nil
}

// needsFingerprintCheck reports whether entry should be fingerprint-checked
// this cycle: any entry that isn't (yet) Settled, PLUS any entry that
// claims to be Settled but whose StableSince doesn't parse -- an entry with
// an invalid/missing StableSince is conservatively treated as unsettled
// (see the Phase 4D review's own wording) rather than trusted at face
// value, since StableSince is exactly the field that would prove settling
// actually happened.
func needsFingerprintCheck(e PingScoreHistoryEntry) bool {
	if !e.Settled {
		return true
	}
	_, err := time.Parse(time.RFC3339, e.StableSince)
	return err != nil
}

// Cycle runs exactly one incremental recompute pass. All-or-nothing: any
// error returned means e.index, the persisted history database, and
// whatever *PingScoresSnapshot a caller already has published are ALL left
// completely unchanged -- no partial upserts, no partial settle/
// data_pruned state, nothing. This is achieved by building every change
// into candidateIndex (a Clone of e.index) and a plain upserts slice,
// touching neither e.index nor the store until step 9's single
// UpsertDeleteAndIntegrity transaction has actually committed; e.index is
// only reassigned to candidateIndex in step 10, strictly after that.
//
// Cycle does NOT publish its result anywhere (no s.pingScores.Store call --
// there is no s.pingScores write in this file at all): the returned
// snapshot is a CANDIDATE for a future caller (the scheduler-wiring phase)
// to decide what to do with, once this engine has been reviewed and cut
// over.
//
// Step order follows the approved design exactly:
//  1. fetchPingTriggers
//  2. plan reconciliation (planPingScoreHistoryReconcile)
//  3. select fingerprint- and deep-sweep candidates
//  4. observationFingerprintsBulk
//  5. GetPacketPathsBulk
//  6. buildPingScoreFromPath
//  7. merge into a cloned candidate index
//  8. history snapshot built + names enriched (buildPingScoresSnapshotFromHistory)
//  9. UpsertAndDelete (via UpsertDeleteAndIntegrity) in one persistent transaction
//  10. only after commit: replace engine.index
//  11. return the candidate snapshot
func (e *pingScoreHistoryEngine) Cycle() (*PingScoresSnapshot, error) {
	now := e.now()
	nowStr := now.UTC().Format(time.RFC3339)

	// --- 1. fetchPingTriggers ---
	triggers, err := e.server.db.fetchPingTriggers()
	if err != nil {
		return nil, fmt.Errorf("ping score history cycle: fetch triggers: %w", err)
	}
	triggerByID := make(map[int64]pingTriggerRow, len(triggers))
	for _, t := range triggers {
		triggerByID[t.txID] = t
	}

	// --- 2. plan reconciliation ---
	plan := planPingScoreHistoryReconcile(e.index, triggers)
	toComputeSet := make(map[int64]bool, len(plan.ToCompute))
	for _, t := range plan.ToCompute {
		toComputeSet[t.txID] = true
	}
	invalidatedSet := make(map[int64]bool, len(plan.Invalidated))
	for _, id := range plan.Invalidated {
		invalidatedSet[id] = true
	}

	// --- 3. select fingerprint candidates (existing, unsettled, present in
	// triggers, not already in ToCompute) and deep-sweep candidates
	// (existing, Settled, DataPruned=false, present in triggers, not
	// already selected above) ---
	var fingerprintCandidateIDs []int64
	for _, entry := range e.index.Entries() {
		if toComputeSet[entry.TxID] {
			continue // already getting a full recompute via reconciliation
		}
		if _, present := triggerByID[entry.TxID]; !present {
			continue // being deleted this cycle -- not a candidate for anything
		}
		if needsFingerprintCheck(entry) {
			fingerprintCandidateIDs = append(fingerprintCandidateIDs, entry.TxID)
		}
	}
	fingerprintCandidateSet := make(map[int64]bool, len(fingerprintCandidateIDs))
	for _, id := range fingerprintCandidateIDs {
		fingerprintCandidateSet[id] = true
	}

	var deepSweepEligible []PingScoreHistoryEntry
	for _, entry := range e.index.Entries() {
		if toComputeSet[entry.TxID] || fingerprintCandidateSet[entry.TxID] {
			continue
		}
		if _, present := triggerByID[entry.TxID]; !present {
			continue
		}
		if !entry.Settled || entry.DataPruned {
			continue
		}
		deepSweepEligible = append(deepSweepEligible, entry)
	}
	// Deterministic: oldest/empty LastDeepSweptAt first (empty sorts before
	// any real RFC3339 timestamp), tx_id as the tie-break.
	sort.Slice(deepSweepEligible, func(i, j int) bool {
		a, b := deepSweepEligible[i], deepSweepEligible[j]
		if a.LastDeepSweptAt != b.LastDeepSweptAt {
			return a.LastDeepSweptAt < b.LastDeepSweptAt
		}
		return a.TxID < b.TxID
	})
	if len(deepSweepEligible) > e.config.DeepSweepBatchSize {
		deepSweepEligible = deepSweepEligible[:e.config.DeepSweepBatchSize]
	}

	// --- 4. observationFingerprintsBulk ---
	// Fingerprinted this cycle: every ToCompute tx_id (so a brand-new/
	// invalidated entry's PERSISTED fingerprint reflects reality
	// immediately, rather than starting at a fake zero value that would
	// force one wasted extra recompute next cycle purely to "discover" a
	// fingerprint that was already knowable now), every ordinary
	// fingerprint candidate, AND every deep-sweep-selected entry -- deep-
	// sweep doubles as an opportunistic fingerprint refresh for entries
	// settling would otherwise never check again (see the deep-sweep
	// merge loop below for what happens when THAT check reveals a change).
	fingerprintIDs := make([]int64, 0, len(plan.ToCompute)+len(fingerprintCandidateIDs)+len(deepSweepEligible))
	for _, t := range plan.ToCompute {
		fingerprintIDs = append(fingerprintIDs, t.txID)
	}
	fingerprintIDs = append(fingerprintIDs, fingerprintCandidateIDs...)
	for _, entry := range deepSweepEligible {
		fingerprintIDs = append(fingerprintIDs, entry.TxID)
	}
	var fingerprints map[int64]observationFingerprint
	if len(fingerprintIDs) > 0 {
		fingerprints, err = e.server.db.observationFingerprintsBulk(fingerprintIDs)
		if err != nil {
			return nil, fmt.Errorf("ping score history cycle: fingerprint query: %w", err)
		}
	}
	fingerprintOf := func(txID int64) observationFingerprint {
		return fingerprints[txID] // zero value (Count:0, MaxID:0) if genuinely absent (no observation rows yet)
	}

	// Partition fingerprint candidates into changed (needs a full
	// recompute) vs settling-eligible (fingerprint unchanged -- may
	// transition to Settled this cycle, no path recompute needed).
	changedFingerprintIDs := map[int64]bool{}
	settlingCandidateIDs := map[int64]bool{}
	for _, txID := range fingerprintCandidateIDs {
		entry, _ := e.index.Get(txID)
		fp := fingerprintOf(txID)
		if fp.Count != entry.FingerprintCount || fp.MaxID != entry.FingerprintMaxID {
			changedFingerprintIDs[txID] = true
		} else {
			settlingCandidateIDs[txID] = true
		}
	}

	// --- 5. GetPacketPathsBulk: the union of every hash needing a real
	// path recompute this cycle (reconciliation + fingerprint-changed +
	// deep-sweep) ---
	txIDsNeedingPath := map[int64]bool{}
	for _, t := range plan.ToCompute {
		txIDsNeedingPath[t.txID] = true
	}
	for txID := range changedFingerprintIDs {
		txIDsNeedingPath[txID] = true
	}
	for _, entry := range deepSweepEligible {
		txIDsNeedingPath[entry.TxID] = true
	}
	hashesList := make([]string, 0, len(txIDsNeedingPath))
	seenHash := map[string]bool{}
	for txID := range txIDsNeedingPath {
		h := triggerByID[txID].hash
		if !seenHash[h] {
			seenHash[h] = true
			hashesList = append(hashesList, h)
		}
	}
	var pathResults map[string]*PacketPathResponse
	if len(hashesList) > 0 {
		pathResults, err = e.server.db.GetPacketPathsBulk(hashesList, e.config.MaxEdgeKm)
		if err != nil {
			return nil, fmt.Errorf("ping score history cycle: bulk path query: %w", err)
		}
	}

	// --- 6 & 7: buildPingScoreFromPath + merge into a CLONED candidate
	// index. Nothing here touches e.index or the store. ---
	candidateIndex := e.index.Clone()
	var upserts []PingScoreHistoryEntry

	// 7a. Reconciliation: brand-new AND invalidated tx_ids both use
	// pingScoreHistoryEntryFromScore (fresh-entry semantics), never
	// mergePingScoreHistoryEntry -- an invalidated entry's OLD path facts
	// describe a DIFFERENT packet (the hash/timestamp changed), so they
	// must never be carried forward even on a failed recompute this cycle.
	for _, trigger := range plan.ToCompute {
		score := e.server.buildPingScoreFromPath(trigger, pathResults[trigger.hash])
		state := PingScoreHistoryEntryState{StableSince: nowStr, Settled: false}
		entry := pingScoreHistoryEntryFromScore(trigger, score, fingerprintOf(trigger.txID), state, now)
		candidateIndex.Upsert(entry)
		upserts = append(upserts, entry)
	}

	// 7b. Fingerprint-changed (hash/timestamp UNCHANGED -- the same real
	// ping, new observations arrived): merge against the existing entry,
	// restart the settle clock.
	for txID := range changedFingerprintIDs {
		trigger := triggerByID[txID]
		existing, _ := candidateIndex.Get(txID)
		score := e.server.buildPingScoreFromPath(trigger, pathResults[trigger.hash])
		state := PingScoreHistoryEntryState{
			StableSince: nowStr, Settled: false,
			DataPruned: existing.DataPruned, LastDeepSweptAt: existing.LastDeepSweptAt,
		}
		entry := mergePingScoreHistoryEntry(existing, trigger, score, state, fingerprintOf(txID), now)
		candidateIndex.Upsert(entry)
		upserts = append(upserts, entry)
	}

	// 7c. Settling candidates (fingerprint unchanged): no path recompute
	// happened for these, so there is no *PingScore to merge -- a direct,
	// narrow field update instead of mergePingScoreHistoryEntry (which
	// would misinterpret "not attempted" as "attempted and empty").
	// Fingerprint change and settling can never happen for the SAME entry
	// in the SAME cycle: an entry only reaches this branch because its
	// fingerprint was found UNCHANGED; settling here just means enough
	// wall-clock time has now passed since the LAST time it changed
	// (StableSince). If it changes again, that happens in 7b instead
	// (Settled resets to false, StableSince resets to now) -- settling
	// can then only be re-evaluated in a LATER cycle, never the one where
	// the change was detected.
	for txID := range settlingCandidateIDs {
		trigger := triggerByID[txID]
		existing, _ := candidateIndex.Get(txID)
		fp := fingerprintOf(txID)
		updated := existing
		updated.Sender = trigger.sender
		updated.ChannelHash = trigger.channelHash
		updated.FingerprintCount = fp.Count
		updated.FingerprintMaxID = fp.MaxID
		if !updated.Settled {
			if stableSince, perr := time.Parse(time.RFC3339, existing.StableSince); perr == nil && now.Sub(stableSince) >= e.config.SettleDebounce {
				updated.Settled = true
			} else if perr != nil {
				// Invalid/missing StableSince -- conservatively unsettled
				// (see needsFingerprintCheck); give it a fresh, valid
				// StableSince now so it has a real chance to settle in a
				// future cycle instead of being stuck unsettled forever.
				updated.StableSince = nowStr
			}
		}
		updated.ComputedAt = nowStr
		candidateIndex.Upsert(updated)
		upserts = append(upserts, updated)
	}

	// 7d. Deep-sweep: a real, fingerprint-independent recompute for
	// Settled entries, bounding staleness for blind spots the fingerprint
	// can't see (in-place resolved_path/snr updates, nodes.lat/lon
	// changes, neighbor_edges changes -- see the Phase 4 design report).
	// If deep-sweep's OWN fingerprint check (fetched in step 4 above)
	// reveals a change too, the entry is ALSO un-settled here (Settled
	// reset to false, StableSince reset to now) -- this is the "fingerprint
	// changes after settle, because it was selected for deep-sweep"
	// scenario: deep-sweep is the only mechanism that ever re-examines a
	// Settled entry's fingerprint at all, so this is precisely where such
	// a change becomes visible again. LastDeepSweptAt is updated
	// unconditionally for every entry in this batch once the shared
	// GetPacketPathsBulk call above has succeeded (Cycle is all-or-nothing,
	// so reaching this loop at all means it did) -- an empty per-hash
	// result for one entry doesn't mean the SWEEP failed, only that this
	// particular attempt found nothing; the rotation must still advance so
	// this entry doesn't get picked as "oldest" again next cycle.
	for _, entry := range deepSweepEligible {
		trigger := triggerByID[entry.TxID]
		existing, _ := candidateIndex.Get(entry.TxID)
		score := e.server.buildPingScoreFromPath(trigger, pathResults[trigger.hash])
		fp := fingerprintOf(entry.TxID)
		fingerprintChanged := fp.Count != existing.FingerprintCount || fp.MaxID != existing.FingerprintMaxID

		state := PingScoreHistoryEntryState{
			StableSince: existing.StableSince, Settled: existing.Settled,
			DataPruned: existing.DataPruned, LastDeepSweptAt: nowStr,
		}
		if fingerprintChanged {
			state.Settled = false
			state.StableSince = nowStr
		}
		merged := mergePingScoreHistoryEntry(existing, trigger, score, state, fp, now)
		maybeMarkDataPruned(&merged, existing, trigger, score, now, e.config.RetentionDuration)

		candidateIndex.Upsert(merged)
		upserts = append(upserts, merged)
	}

	// --- 8. history snapshot built + names enriched ---
	snapshot, err := e.server.buildPingScoresSnapshotFromHistory(triggers, candidateIndex.Entries(), now)
	if err != nil {
		return nil, fmt.Errorf("ping score history cycle: build snapshot: %w", err)
	}

	// Bootstrap-integrity: among THIS cycle's reconciliation set (new or
	// invalidated tx_ids -- the only ones being reconstructed for the
	// first time under their current hash), count how many ended up
	// Unscorable AND are already older than the retention window --
	// permanently, not just "not yet", unreconstructable. See
	// maybeBuildBootstrapIntegrity's own doc comment for why this is
	// scoped to ToCompute specifically, distinct from DataPruned.
	integrity := e.maybeBuildBootstrapIntegrity(plan.ToCompute, candidateIndex, triggers, now)

	// --- 9. UpsertAndDelete (via UpsertDeleteAndIntegrity) in one
	// persistent transaction ---
	if err := e.store.UpsertDeleteAndIntegrity(upserts, plan.ToDelete, integrity); err != nil {
		return nil, fmt.Errorf("ping score history cycle: persist: %w", err)
	}

	// --- 10. only after commit: replace engine.index ---
	for _, txID := range plan.ToDelete {
		candidateIndex.Delete(txID)
	}
	e.index = candidateIndex

	// --- 11. return the candidate snapshot ---
	return snapshot, nil
}

// maybeMarkDataPruned sets merged.DataPruned = true in place, but ONLY
// when every one of these holds (see the Phase 4D review's DataPruned
// proof requirements):
//   - existingEntry previously had a genuinely valid score (!Unscorable) --
//     a brand-new or never-successfully-scored entry can NEVER become
//     DataPruned from a single empty result; there is nothing to preserve.
//   - THIS deep-sweep attempt found nothing (score == nil).
//   - trigger.firstSeen parses as a valid timestamp.
//   - trigger is older than retentionDuration (now - firstSeen >= retentionDuration).
//   - retentionDuration > 0 (a zero/negative value disables DataPruned
//     detection entirely -- an explicit opt-in, never inferred).
//
// merged must already have gone through mergePingScoreHistoryEntry before
// this is called, so its path facts already correctly preserve
// existingEntry's prior valid values (mergePingScoreHistoryEntry's own
// "empty result never downgrades" rule) -- this function only ever ADDS
// the DataPruned=true flag on top, never touches any path fact itself.
func maybeMarkDataPruned(merged *PingScoreHistoryEntry, existingEntry PingScoreHistoryEntry, trigger pingTriggerRow, score *PingScore, now time.Time, retentionDuration time.Duration) {
	if score != nil || existingEntry.Unscorable || retentionDuration <= 0 {
		return
	}
	firstSeen, err := time.Parse(time.RFC3339, trigger.firstSeen)
	if err != nil {
		return
	}
	if now.Sub(firstSeen) >= retentionDuration {
		merged.DataPruned = true
	}
}

// maybeBuildBootstrapIntegrity computes the "initial-backfill-incomplete"
// integrity record for THIS cycle's reconciliation set (toCompute), or nil
// when there is nothing abnormal to report.
//
// Scoped to toCompute specifically -- NOT deep-sweep, NOT the whole index --
// because this describes a DIFFERENT situation than DataPruned: a trigger
// that never had a chance to be scored in the first place (first seen this
// cycle, whether via true bootstrap -- the history store starting empty
// while ping_triggers already has rows -- or an ordinary later cycle
// seeing a brand-new/invalidated trigger that happens to already be past
// retention) and can never be reconstructed, versus DataPruned's "this
// ping WAS scored once, its raw data has since legitimately aged out."
//
// TotalTriggers is the overall live trigger count (len(triggers)), giving
// scale; ScoredCount/UnreconstructableCount describe only toCompute's
// outcome this cycle. Returns nil (write nothing) whenever
// UnreconstructableCount is 0 -- a normal empty/new database with no
// historical loss must never be marked degraded, and this function never
// clears a PREVIOUSLY recorded abnormal status either: it only ever
// returns a non-nil *PingScoreHistoryIntegrity when THIS cycle detects a
// genuine new instance of the condition; Cycle passes nil straight through
// to UpsertDeleteAndIntegrity otherwise, which by its own contract leaves
// whatever integrity record already existed completely untouched.
func (e *pingScoreHistoryEngine) maybeBuildBootstrapIntegrity(toCompute []pingTriggerRow, candidateIndex *pingScoreHistoryIndex, triggers []pingTriggerRow, now time.Time) *PingScoreHistoryIntegrity {
	if e.config.RetentionDuration <= 0 || len(toCompute) == 0 {
		return nil
	}
	scoredCount, unreconstructableCount := 0, 0
	for _, trigger := range toCompute {
		entry, ok := candidateIndex.Get(trigger.txID)
		if !ok || !entry.Unscorable {
			scoredCount++
			continue
		}
		firstSeen, err := time.Parse(time.RFC3339, trigger.firstSeen)
		if err != nil {
			continue // can't judge age -- not counted as unreconstructable by this pass
		}
		if now.Sub(firstSeen) >= e.config.RetentionDuration {
			unreconstructableCount++
		}
	}
	if unreconstructableCount == 0 {
		return nil
	}
	return &PingScoreHistoryIntegrity{
		Status:                 "initial-backfill-incomplete",
		DetectedAt:             now.UTC().Format(time.RFC3339),
		TotalTriggers:          len(triggers),
		ScoredCount:            scoredCount,
		UnreconstructableCount: unreconstructableCount,
		Detail: fmt.Sprintf(
			"%d of %d newly-seen trigger(s) this cycle could not be reconstructed (already older than the %s retention window)",
			unreconstructableCount, len(toCompute), e.config.RetentionDuration,
		),
	}
}
