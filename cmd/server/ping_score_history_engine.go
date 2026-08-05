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
	"strings"
	"time"
)

// pingScoreHistoryEngineConfig holds the engine's configurable thresholds.
// No default is baked into engine code for RetentionDuration in particular
// -- per the Phase 4D review, the (currently 30-day, staging-specific)
// packet retention window must be injected by the caller, never
// hardcoded here. The zero value (RetentionDuration: 0) safely disables
// DataPruned detection entirely (see maybeMarkDataPruned) rather than
// guessing a number.
//
// Defaults are applied in exactly ONE place, defaultPingScoreHistoryEngineConfig
// below, meant for the future production-wiring phase -- newPingScoreHistoryEngine
// itself never substitutes a default for a zero value; every zero value
// documented here is a real, deliberate, tested behavior in its own right,
// not "unset". Test fixtures must not apply hidden defaults either (see
// setupEngineFixture in ping_score_history_engine_test.go) -- every test
// passes the literal config it means to exercise.
type pingScoreHistoryEngineConfig struct {
	// SettleDebounce is how long an entry's fingerprint must stay
	// unchanged (measured from StableSince) before it transitions to
	// Settled=true and stops being fingerprint-checked every cycle.
	//
	// Zero is a valid, deliberate value: an entry settles at the very
	// next cycle that finds its fingerprint unchanged (now.Sub(stableSince)
	// >= 0 is always true) -- "settle at the next unchanged check", not
	// "never settle". Must not be negative (rejected by
	// newPingScoreHistoryEngine): a negative debounce has no sensible
	// interpretation.
	SettleDebounce time.Duration

	// DeepSweepBatchSize caps how many Settled=true, DataPruned=false,
	// not-permanently-unreconstructable entries get a full independent-
	// of-fingerprint recompute each cycle.
	//
	// Zero is a valid, deliberate value: deep-sweep is disabled entirely
	// (the eligible-candidates slice is truncated to length 0 every
	// cycle) -- a real, supported "off" switch, not an oversight. Must
	// not be negative (rejected by newPingScoreHistoryEngine): a negative
	// size would panic the slice-truncation operation in Cycle.
	DeepSweepBatchSize int

	// RetentionDuration is the packet-data retention window (how long the
	// ingestor keeps the underlying transmissions/observations rows) --
	// used for DataPruned detection, permanent-unreconstructability
	// detection, and initial-backfill-incomplete bootstrap-integrity
	// detection. Zero is a valid, deliberate value: all three are
	// disabled (we simply don't know the retention window, so none of
	// them can be judged safely). Must not be negative (rejected by
	// newPingScoreHistoryEngine): a negative duration has no real-world
	// meaning here.
	RetentionDuration time.Duration

	// MaxEdgeKm is threaded through to GetPacketPathsBulk's geo-sanity
	// filter, matching GetPacketPath's/nearestPositionedNeighbor's own
	// existing parameter and its own documented convention: <=0
	// deliberately DISABLES the geo-sanity filter (not an error) -- so
	// unlike the other three fields, a non-positive MaxEdgeKm is NOT
	// rejected by newPingScoreHistoryEngine. This mirrors an existing,
	// already-relied-upon convention rather than inventing a new one.
	MaxEdgeKm float64
}

// defaultPingScoreHistoryEngineConfig returns the Phase 4 design's default
// thresholds (10-minute settle debounce, 100-entry deep-sweep batches) --
// RetentionDuration is deliberately left at zero (disabled); a caller
// wiring this up for a real server must set it explicitly. This is the
// ONE place production defaults live -- see the type's own doc comment.
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
// store: validates every input and config field FIRST (before touching the
// store at all), then loads every persisted entry, builds the in-memory
// index, and verifies the store's integrity record loads cleanly -- all
// read-only, no persistent state is ever changed by construction. Returns
// a contextual error (never a partially-usable engine, and never a panic)
// if anything fails, including:
//   - nil server, nil server.db, nil store, or nil now (a nil now would
//     panic the first time Cycle called it)
//   - SettleDebounce < 0, DeepSweepBatchSize < 0, or RetentionDuration < 0
//     (zero is valid and meaningful for all three -- see
//     pingScoreHistoryEngineConfig's own doc comment for what each zero
//     value means; only negative is rejected). MaxEdgeKm is deliberately
//     NOT validated this way -- <=0 is an existing, meaningful "disable
//     the geo-filter" convention borrowed from nearestPositionedNeighbor,
//     not an error condition here either.
//   - a non-positive tx_id in a loaded entry (the same invariant the
//     schema's own CHECK constraint enforces), a duplicate tx_id within
//     the loaded slice (LoadAll's tx_id PRIMARY KEY makes this exactly as
//     impossible as a same-DB duplicate row, but this stays a belt-and-
//     suspenders check rather than an assumption), or relay_pubkeys_json
//     that fails to parse (would otherwise surface much later, deep
//     inside a Cycle, as a materialization error instead of an init-time
//     one)
func newPingScoreHistoryEngine(server *Server, store *PingScoreHistoryStore, now func() time.Time, config pingScoreHistoryEngineConfig) (*pingScoreHistoryEngine, error) {
	if server == nil {
		return nil, fmt.Errorf("ping score history engine: init: server is nil")
	}
	if server.db == nil {
		return nil, fmt.Errorf("ping score history engine: init: server.db is nil")
	}
	if store == nil {
		return nil, fmt.Errorf("ping score history engine: init: store is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("ping score history engine: init: now is nil")
	}
	if config.SettleDebounce < 0 {
		return nil, fmt.Errorf("ping score history engine: init: SettleDebounce is negative (%s) -- zero is valid (settle at the next unchanged check), negative is not", config.SettleDebounce)
	}
	if config.DeepSweepBatchSize < 0 {
		return nil, fmt.Errorf("ping score history engine: init: DeepSweepBatchSize is negative (%d) -- zero is valid (deep-sweep disabled), negative is not", config.DeepSweepBatchSize)
	}
	if config.RetentionDuration < 0 {
		return nil, fmt.Errorf("ping score history engine: init: RetentionDuration is negative (%s) -- zero is valid (DataPruned/permanent-unreconstructable/bootstrap-integrity detection disabled), negative is not", config.RetentionDuration)
	}

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
	// LoadIntegrity/LoadGap/HistoryInitializedAt are called here purely so
	// construction fails fast on a broken table rather than succeeding and
	// failing later inside the first Cycle -- Cycle() re-reads whichever of
	// these it actually needs itself (see the isGenuineBootstrap check and
	// the gap computation near the top and bottom of Cycle respectively),
	// so nothing from these calls is retained on the engine.
	if _, err := store.LoadIntegrity(); err != nil {
		return nil, fmt.Errorf("ping score history engine: init: load integrity: %w", err)
	}
	if _, err := store.LoadGap(); err != nil {
		return nil, fmt.Errorf("ping score history engine: init: load gap: %w", err)
	}
	if _, _, err := store.HistoryInitializedAt(); err != nil {
		return nil, fmt.Errorf("ping score history engine: init: load history-initialized marker: %w", err)
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

	// isGenuineBootstrap captures the ONLY signal for "is this cycle the
	// real, first-ever bootstrap": whether the persistent
	// history_initialized_at marker has EVER been written before (see
	// HistoryInitializedAt). Deliberately NOT derived from e.index.Len()==0
	// -- an empty in-memory index is not a permanent fact: every tracked
	// trigger could simply have been pruned/deleted by a LATER cycle (see
	// planPingScoreHistoryReconcile's ToDelete), and brand-new triggers
	// arriving after that point are normal operation resuming, not a new
	// bootstrap (fix-round-2 review of a1c3022d: "en senere tom index
	// bliver fejlklassificeret som ny bootstrap"). The marker itself is
	// only ever written ONCE, by the genuine first successful cycle (see
	// the historyInitializedAt persist call below), so this check is
	// reliable no matter how many times the index later becomes empty
	// again. HistoryInitializedAt is fallible and must run before any
	// mutation/persistence -- exactly like the fetch above, an error here
	// aborts the whole cycle with nothing touched yet.
	_, alreadyInitialized, err := e.store.HistoryInitializedAt()
	if err != nil {
		return nil, fmt.Errorf("ping score history cycle: load history-initialized marker: %w", err)
	}
	isGenuineBootstrap := !alreadyInitialized

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
		if entry.PermanentlyUnreconstructable {
			// Excluded PERMANENTLY -- but only once REAL evidence already
			// exists (this flag is only ever set by
			// maybeMarkPermanentlyUnreconstructable, after an ACTUAL
			// deep-sweep attempt found nothing while past retention -- see
			// its own doc comment). Age past retention alone is
			// deliberately NOT checked here (fix-round-2 review of
			// a1c3022d: "alder alene er ikke bevis for permanent
			// unreconstructability") -- an Unscorable entry that has JUST
			// crossed retention, or crossed it long ago but was never
			// actually re-attempted, REMAINS eligible below and gets
			// exactly one real GetPacketPathsBulk-backed attempt; only
			// THAT attempt's outcome (via maybeMarkPermanentlyUnreconstructable,
			// called from the deep-sweep merge loop) can ever set this
			// flag. It still settles normally (this check runs strictly
			// after the Settled gate above, never before), still counts
			// toward TotalPings via the live trigger list, stays
			// Unscorable and DataPruned=false, and its explanation lives
			// in the ping_score_history_gaps record (see
			// buildPingScoreHistoryGap), not here.
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
	// pathResultFor normalizes the lookup key ONLY -- GetPacketPathsBulk's
	// result map is always keyed by the lowercased hash (see its own
	// implementation), but ping_triggers.hash (and therefore trigger.hash)
	// may not be. This must NEVER be used to change what gets PERSISTED:
	// entry.Hash is still set verbatim from trigger.hash everywhere in this
	// file (see 7a/7b/7d below and pingScoreHistoryEntryFromScore/
	// mergePingScoreHistoryEntry), preserving whatever casing
	// ping_triggers itself stores as the identity ping_triggers-vs-entry
	// reconciliation and mismatch detection depend on -- only the map
	// lookup into pathResults is case-normalized.
	pathResultFor := func(hash string) *PacketPathResponse {
		return pathResults[strings.ToLower(hash)]
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
		score := e.server.buildPingScoreFromPath(trigger, pathResultFor(trigger.hash))
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
		score := e.server.buildPingScoreFromPath(trigger, pathResultFor(trigger.hash))
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
		score := e.server.buildPingScoreFromPath(trigger, pathResultFor(trigger.hash))
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
		maybeMarkPermanentlyUnreconstructable(&merged, existing, trigger, score, now, e.config.RetentionDuration)

		candidateIndex.Upsert(merged)
		upserts = append(upserts, merged)
	}

	// --- 8. history snapshot built + names enriched ---
	snapshot, err := e.server.buildPingScoresSnapshotFromHistory(triggers, candidateIndex.Entries(), now)
	if err != nil {
		return nil, fmt.Errorf("ping score history cycle: build snapshot: %w", err)
	}

	// Bootstrap-integrity: computed ONLY on the genuine first bootstrap
	// (isGenuineBootstrap, captured at the very top of this function,
	// before anything existed to compute over) -- every other cycle
	// passes nil, so UpsertDeleteAndIntegrity leaves whatever integrity
	// record already exists (including one written by an earlier
	// bootstrap, or none at all) completely untouched. See
	// buildInitialBootstrapIntegrity's own doc comment for the full
	// semantics and why this is scoped to the WHOLE first triggers list,
	// not just whatever a later cycle's ToCompute happens to contain.
	var integrity *PingScoreHistoryIntegrity
	if isGenuineBootstrap {
		integrity = e.buildInitialBootstrapIntegrity(triggers, candidateIndex, now)
	}

	// Gap tracking: a GLOBAL count of every entry in the post-cycle
	// candidateIndex (the complete population, not just entries touched
	// THIS cycle) that carries evidence-confirmed PermanentlyUnreconstructable
	// -- distinct from, and computed on EVERY cycle regardless of
	// isGenuineBootstrap (unlike the bootstrap-integrity record above,
	// this can newly become true at ANY point in the store's life; see
	// PingScoreHistoryGap's own doc comment). existingGap is loaded fresh
	// each cycle (fallible, must run before persistence) so
	// buildPingScoreHistoryGap can preserve FirstDetectedAt verbatim
	// rather than resetting it every time new evidence is found.
	permanentCount := 0
	for _, ce := range candidateIndex.Entries() {
		if ce.PermanentlyUnreconstructable {
			permanentCount++
		}
	}
	existingGap, err := e.store.LoadGap()
	if err != nil {
		return nil, fmt.Errorf("ping score history cycle: load gap: %w", err)
	}
	gap := buildPingScoreHistoryGap(existingGap, permanentCount, len(triggers), e.config.RetentionDuration, now)

	// History-initialized marker: written EXACTLY once, ever, the very
	// first time isGenuineBootstrap is true -- see HistoryInitializedAt's
	// own doc comment for why this must be a persistent marker rather than
	// re-derived from index size on every cycle.
	var historyInitializedAt *string
	if isGenuineBootstrap {
		v := nowStr
		historyInitializedAt = &v
	}

	// --- 9. UpsertAndDelete (via UpsertDeleteAndMetadata) in one
	// persistent transaction ---
	if err := e.store.UpsertDeleteAndMetadata(upserts, plan.ToDelete, integrity, gap, historyInitializedAt); err != nil {
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

// maybeMarkPermanentlyUnreconstructable sets
// merged.PermanentlyUnreconstructable = true in place, but ONLY when a
// REAL deep-sweep attempt THIS cycle actually produced the evidence -- age
// past retention is necessary but NEVER sufficient on its own (fix-round-2
// review of a1c3022d: "alder alene er ikke bevis for permanent
// unreconstructability" -- age alone is not proof of permanent
// unreconstructability). Requires ALL of:
//   - score == nil: THIS cycle's real GetPacketPathsBulk-backed recompute
//     attempt (not a prediction) found nothing.
//   - existingEntry.Unscorable == true (the PRE-cycle state, before this
//     merge): this tx_id has never had a successful computation, ever --
//     by construction of pingScoreHistoryEntryFromScore/
//     mergePingScoreHistoryEntry this field already IS exactly that, so
//     there is no separate condition to derive here. Mutually exclusive
//     with DataPruned by construction (maybeMarkDataPruned itself requires
//     the OPPOSITE: !existingEntry.Unscorable).
//   - trigger.firstSeen parses as a valid timestamp (can't judge age
//     otherwise -- an invalid timestamp is never treated as "old").
//   - now - firstSeen >= retentionDuration: the underlying packet data is
//     ALREADY gone by definition of the retention window, so THIS cycle's
//     empty result is not just "data hasn't arrived yet".
//   - retentionDuration > 0 -- the same explicit opt-in convention as
//     maybeMarkDataPruned: retention<=0 means the caller hasn't told us
//     the retention window, so nothing can be safely judged permanent.
//
// Only ever called from the deep-sweep merge loop (7d) in Cycle -- deep-
// sweep is the ONLY code path that attempts a real, fingerprint-
// independent recompute for an entry that is ALREADY Settled AND not yet
// flagged (the deep-sweep eligibility filter's whole job, see Cycle). A
// YOUNG Unscorable entry (not yet past retention) never reaches a state
// where this can fire with a true result -- it keeps getting real attempts
// via deep-sweep every cycle until either a score arrives or it crosses
// retention with a real attempt still coming up empty.
//
// Once true, the eligibility filter in Cycle excludes this entry from
// ALL FUTURE deep-sweep attempts -- there is nothing left to find. See
// mergePingScoreHistoryEntry's score != nil branch for the defensive
// clear that keeps this from ever going permanently stale if a real score
// somehow does arrive later anyway.
//
// Distinct from DataPruned: DataPruned means "had a valid score once, its
// raw data has SINCE been pruned" (existing facts are preserved and
// merged going forward). Permanently unreconstructable means "never had
// anything to preserve in the first place, AND a real attempt already
// proved it" -- these entries stay Unscorable and DataPruned=false
// forever. Their explanation lives in the ping_score_history_gaps record
// (see buildPingScoreHistoryGap), not in the one-time bootstrap-integrity
// record -- they still count toward TotalPings via the live trigger list
// and are still visible in the history database, just never re-attempted
// again after this.
func maybeMarkPermanentlyUnreconstructable(merged *PingScoreHistoryEntry, existingEntry PingScoreHistoryEntry, trigger pingTriggerRow, score *PingScore, now time.Time, retentionDuration time.Duration) {
	if score != nil || !existingEntry.Unscorable || retentionDuration <= 0 {
		return
	}
	firstSeen, err := time.Parse(time.RFC3339, trigger.firstSeen)
	if err != nil {
		return
	}
	if now.Sub(firstSeen) >= retentionDuration {
		merged.PermanentlyUnreconstructable = true
	}
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

// buildInitialBootstrapIntegrity computes the "initial-backfill-incomplete"
// integrity record, or nil when there is nothing abnormal to report. Only
// ever called by Cycle when isGenuineBootstrap is true -- i.e. exactly
// once, ever, for a given history store's lifetime (the index was
// completely empty before this cycle, and no integrity record has ever
// been written before). A later cycle, even one whose OWN ToCompute set
// contains a brand-new or newly-invalidated trigger that happens to
// already be past retention, is NOT a bootstrap and never calls this
// function -- Cycle passes nil for integrity on every other cycle, which
// by UpsertDeleteAndIntegrity's own contract leaves whatever integrity
// record already exists (written by the real bootstrap, or none at all)
// completely untouched. That is the fix for the review's finding: this
// function itself has no "is this really the first time" logic anymore --
// isGenuineBootstrap is the only gate, checked once, before this function
// is ever reached.
//
// Computed over triggers (the WHOLE first live trigger list), not just
// some subset -- on a genuine bootstrap this is also exactly what
// reconciliation's ToCompute contains (an empty starting index means
// every trigger is new), but this function deliberately doesn't rely on
// that coincidence: it walks triggers directly.
//   - TotalTriggers = len(triggers)
//   - ScoredCount = triggers with a candidateIndex entry that is NOT
//     Unscorable. A trigger with NO candidateIndex entry at all (would
//     indicate a bug elsewhere, since every trigger in a bootstrap's
//     ToCompute gets an entry from Cycle's own build step) is counted as
//     NEITHER scored nor unreconstructable -- never silently counted as
//     scored.
//   - UnreconstructableCount = triggers whose entry is Unscorable AND
//     provably already older than RetentionDuration -- an AGE ESTIMATE
//     for this one-time reporting snapshot only, deliberately NOT the
//     same evidence-based test as PermanentlyUnreconstructable/
//     maybeMarkPermanentlyUnreconstructable (an age estimate is fine for
//     an informational count at bootstrap time; it is NOT fine as the
//     basis for permanently excluding an entry from ever being
//     re-attempted, which is exactly the distinction the fix-round-2
//     review drew). An unparseable firstSeen is never counted as
//     unreconstructable (can't prove it).
//   - DetectedAt is set once, here, at bootstrap time, and never revised
//     (there is no later call site that could revise it).
//
// Returns nil (write nothing) whenever RetentionDuration<=0 (the
// permanence question can't be judged at all) or UnreconstructableCount
// is 0 -- a normal empty/new database with no historical loss must never
// be marked degraded.
func (e *pingScoreHistoryEngine) buildInitialBootstrapIntegrity(triggers []pingTriggerRow, candidateIndex *pingScoreHistoryIndex, now time.Time) *PingScoreHistoryIntegrity {
	if e.config.RetentionDuration <= 0 {
		return nil
	}
	scoredCount, unreconstructableCount := 0, 0
	for _, trigger := range triggers {
		entry, ok := candidateIndex.Get(trigger.txID)
		if !ok {
			continue // missing entry -- never counted as scored (or anything else)
		}
		if !entry.Unscorable {
			scoredCount++
			continue
		}
		firstSeen, err := time.Parse(time.RFC3339, trigger.firstSeen)
		if err != nil {
			continue // can't judge age -- not counted as unreconstructable
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
			"%d of %d triggers from the initial bootstrap could not be reconstructed (already older than the %s retention window)",
			unreconstructableCount, len(triggers), e.config.RetentionDuration,
		),
	}
}

// buildPingScoreHistoryGap decides what (if anything) to persist to the
// ping_score_history_gaps table THIS cycle, given permanentCount (the
// GLOBAL count of entries with PermanentlyUnreconstructable=true across
// the WHOLE post-cycle candidateIndex -- see the gap computation in Cycle,
// step 9) and existing (whatever gap record, if any, was already there
// before this cycle -- nil if none). Called on EVERY cycle, unlike
// buildInitialBootstrapIntegrity (which only ever runs once) -- new
// evidence can arrive at any point in the store's life, long after a
// perfectly healthy bootstrap.
//
//   - permanentCount == 0, existing == nil: nil (nothing to write) -- a
//     healthy history has no gap row at all, same "nil means healthy"
//     convention as LoadIntegrity.
//   - permanentCount == 0, existing != nil: nil (no write) -- a
//     previously recorded abnormal status is NEVER silently reverted to
//     "ok" by this function (fix-round-2 review: "en tidligere abnormal
//     status må ikke stiltiende blive 'ok'"); the existing record is left
//     completely untouched rather than downgraded.
//   - permanentCount > 0, existing == nil: a brand-new record,
//     FirstDetectedAt = LastDetectedAt = now.
//   - permanentCount > 0, existing != nil: FirstDetectedAt is PRESERVED
//     verbatim from existing (never revised once set -- "DetectedAt for
//     det første observerede tab skal bevares"), LastDetectedAt and the
//     counts are refreshed to this cycle's GLOBAL values.
func buildPingScoreHistoryGap(existing *PingScoreHistoryGap, permanentCount, totalTriggers int, retention time.Duration, now time.Time) *PingScoreHistoryGap {
	if permanentCount == 0 {
		return nil
	}
	nowStr := now.UTC().Format(time.RFC3339)
	firstDetectedAt := nowStr
	if existing != nil && existing.FirstDetectedAt != "" {
		firstDetectedAt = existing.FirstDetectedAt
	}
	return &PingScoreHistoryGap{
		Status:                            "history-incomplete",
		FirstDetectedAt:                   firstDetectedAt,
		LastDetectedAt:                    nowStr,
		PermanentlyUnreconstructableCount: permanentCount,
		TotalTriggers:                     totalTriggers,
		Detail: fmt.Sprintf(
			"%d of %d live triggers are permanently unreconstructable (proven by an empty deep-sweep already past the %s retention window)",
			permanentCount, totalTriggers, retention,
		),
	}
}
