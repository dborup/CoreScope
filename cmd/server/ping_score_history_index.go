// Package main: Ping Scores in-memory history index -- Phase 4C of the
// recompute redesign (see reviews/CoreScope-code-review-2026-08-04.md).
//
// pingScoreHistoryIndex is the working, in-memory state a future recompute
// cycle (Phase 4D+) will read from and write back to the
// PingScoreHistoryStore. Phase 4C only builds the pure data structure and
// its pure operations (New/Load, Clone, Get/Upsert/Delete, and a reconcile
// PLAN against a fresh trigger list) -- nothing here performs I/O, and
// nothing here is wired into the scheduler yet.
package main

import "sort"

// pingScoreHistoryIndex is an in-memory index over PingScoreHistoryEntry
// rows keyed by tx_id.
//
// NOT safe for concurrent use -- deliberately no mutex. Exactly one
// goroutine (the ping-scores recomputer, once Phase 4D wires this in) is
// ever expected to hold and mutate a given instance, matching
// PingScoreHistoryStore's own single-owner contract (see its doc comment:
// LoadAll/UpsertAndDelete aren't safe for concurrent access either).
// Adding a mutex here would paper over that design rather than actually
// make cross-goroutine sharing safe -- if a future phase needs concurrent
// access, that needs its own explicit design (e.g. swapping a
// *pingScoreHistoryIndex behind an atomic.Value the way pingScoresCache
// already does for *PingScoresSnapshot), not a lock bolted onto this type.
type pingScoreHistoryIndex struct {
	byTxID map[int64]PingScoreHistoryEntry
}

// newPingScoreHistoryIndex builds an index from a freshly loaded entry
// slice (e.g. PingScoreHistoryStore.LoadAll's result).
func newPingScoreHistoryIndex(entries []PingScoreHistoryEntry) *pingScoreHistoryIndex {
	idx := &pingScoreHistoryIndex{byTxID: make(map[int64]PingScoreHistoryEntry, len(entries))}
	for _, e := range entries {
		idx.byTxID[e.TxID] = e
	}
	return idx
}

// Clone returns an independent copy: a future recompute cycle can build up
// changes against the clone and only Upsert them into the original index
// (or discard the clone entirely) once the cycle actually succeeds,
// without a failed cycle ever having mutated the original's state.
//
// Copying the map (which copies each PingScoreHistoryEntry by value,
// including its pointer fields FarthestKm/SpreadSeconds/AirtimeMs) is
// sufficient depth: nothing anywhere in this package ever mutates a
// PingScoreHistoryEntry's pointer fields IN PLACE (through the pointer) --
// every update (pingScoreHistoryEntryFromScore, mergePingScoreHistoryEntry)
// replaces a field with a freshly allocated pointer instead of writing
// through an old one. So even though the clone's pointers may alias the
// original's for fields neither side has touched yet, neither side can
// ever observe a mutation through them.
func (idx *pingScoreHistoryIndex) Clone() *pingScoreHistoryIndex {
	clone := &pingScoreHistoryIndex{byTxID: make(map[int64]PingScoreHistoryEntry, len(idx.byTxID))}
	for k, v := range idx.byTxID {
		clone.byTxID[k] = v
	}
	return clone
}

// Get returns the entry for txID and whether it was present.
func (idx *pingScoreHistoryIndex) Get(txID int64) (PingScoreHistoryEntry, bool) {
	e, ok := idx.byTxID[txID]
	return e, ok
}

// Upsert inserts or replaces the entry for e.TxID.
func (idx *pingScoreHistoryIndex) Upsert(e PingScoreHistoryEntry) {
	idx.byTxID[e.TxID] = e
}

// Delete removes txID from the index, if present. A no-op if absent.
func (idx *pingScoreHistoryIndex) Delete(txID int64) {
	delete(idx.byTxID, txID)
}

// Len reports how many entries are currently in the index.
//
// NOT the right source for a PingScoresSnapshot's TotalPings -- see
// buildPingScoresSnapshotFromHistory, which uses len(triggers) (a fresh
// ping_triggers read) instead. The index can legitimately lag the live
// trigger table (a trigger not yet computed) or lead it (a trigger since
// removed from ping_triggers but not yet reconciled out of the index) --
// TotalPings must reflect what ping_triggers says exists right now, not
// how many entries this index happens to currently hold.
func (idx *pingScoreHistoryIndex) Len() int { return len(idx.byTxID) }

// TxIDs returns every tx_id currently in the index, sorted ascending --
// deterministic order for callers walking the whole index reproducibly
// (Go's own map iteration order is not).
func (idx *pingScoreHistoryIndex) TxIDs() []int64 {
	out := make([]int64, 0, len(idx.byTxID))
	for k := range idx.byTxID {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Entries returns every entry currently in the index, ordered by tx_id
// ascending -- matching PingScoreHistoryStore.LoadAll's own
// `ORDER BY tx_id`, so round-tripping LoadAll -> newPingScoreHistoryIndex
// -> Entries reproduces the same order LoadAll itself would.
func (idx *pingScoreHistoryIndex) Entries() []PingScoreHistoryEntry {
	ids := idx.TxIDs()
	out := make([]PingScoreHistoryEntry, len(ids))
	for i, id := range ids {
		out[i] = idx.byTxID[id]
	}
	return out
}

// pingScoreHistoryReconcilePlan is the PURE result of comparing an index
// against a fresh ping_triggers read: what a recompute cycle needs to do,
// with no I/O performed and nothing mutated. Phase 4C only builds this
// plan (planPingScoreHistoryReconcile); Phase 4D+ is what actually acts on
// it (calling GetPacketPathsBulk for ToCompute, deleting ToDelete from the
// store, etc).
type pingScoreHistoryReconcilePlan struct {
	// ToCompute is every trigger needing a fresh computation this cycle:
	// tx_ids absent from the index entirely, PLUS tx_ids whose existing
	// entry's hash or timestamp no longer matches the fresh trigger row
	// (see Invalidated). Ordered the same as the triggers slice passed in
	// (fetchPingTriggers itself orders by tx_id, so this is ascending
	// tx_id order for the normal caller).
	ToCompute []pingTriggerRow

	// Invalidated holds the tx_ids from ToCompute that are being
	// recomputed specifically because their hash/timestamp CHANGED for the
	// same tx_id (a subset of ToCompute's tx_ids) -- a changed hash/
	// timestamp on an INTEGER PRIMARY KEY row should not happen in normal
	// operation, but if it ever does, the stale entry must never be
	// blindly reused as if nothing changed; surfaced separately so a
	// caller can log/count this distinctly from an ordinary first-time
	// computation. Sorted ascending (built from map lookups, not
	// inherently ordered like ToCompute is).
	Invalidated []int64

	// ToDelete is every tx_id present in the index but absent from the
	// fresh trigger list -- ping_triggers no longer has a row for it, so
	// the history entry should be removed too. Sorted ascending (built
	// from map iteration, not inherently ordered).
	ToDelete []int64
}

// planPingScoreHistoryReconcile compares idx against a fresh triggers read
// and returns what a recompute cycle needs to do. Pure function: does not
// read or write idx, does not perform any I/O.
func planPingScoreHistoryReconcile(idx *pingScoreHistoryIndex, triggers []pingTriggerRow) pingScoreHistoryReconcilePlan {
	var plan pingScoreHistoryReconcilePlan
	seen := make(map[int64]bool, len(triggers))
	for _, trigger := range triggers {
		seen[trigger.txID] = true
		existing, ok := idx.byTxID[trigger.txID]
		if !ok {
			plan.ToCompute = append(plan.ToCompute, trigger)
			continue
		}
		if existing.Hash != trigger.hash || existing.Timestamp != trigger.firstSeen {
			plan.ToCompute = append(plan.ToCompute, trigger)
			plan.Invalidated = append(plan.Invalidated, trigger.txID)
		}
	}
	for txID := range idx.byTxID {
		if !seen[txID] {
			plan.ToDelete = append(plan.ToDelete, txID)
		}
	}
	sort.Slice(plan.Invalidated, func(i, j int) bool { return plan.Invalidated[i] < plan.Invalidated[j] })
	sort.Slice(plan.ToDelete, func(i, j int) bool { return plan.ToDelete[i] < plan.ToDelete[j] })
	return plan
}
