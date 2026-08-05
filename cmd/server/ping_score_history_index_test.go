package main

import "testing"

func TestPingScoreHistoryIndex_NewFromEntries(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{
		{TxID: 3, Hash: "h3"},
		{TxID: 1, Hash: "h1"},
		{TxID: 2, Hash: "h2"},
	})
	if idx.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", idx.Len())
	}
	e, ok := idx.Get(2)
	if !ok || e.Hash != "h2" {
		t.Errorf("Get(2) = %+v, %v, want h2, true", e, ok)
	}
	if _, ok := idx.Get(999); ok {
		t.Error("Get(999) = true, want false (absent)")
	}
}

func TestPingScoreHistoryIndex_EntriesAndTxIDsAreSortedAscending(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{
		{TxID: 30}, {TxID: 10}, {TxID: 20},
	})
	ids := idx.TxIDs()
	want := []int64{10, 20, 30}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("TxIDs() = %v, want %v", ids, want)
		}
	}
	entries := idx.Entries()
	for i, e := range entries {
		if e.TxID != want[i] {
			t.Fatalf("Entries()[%d].TxID = %d, want %d", i, e.TxID, want[i])
		}
	}
}

func TestPingScoreHistoryIndex_UpsertAndDelete(t *testing.T) {
	idx := newPingScoreHistoryIndex(nil)
	idx.Upsert(PingScoreHistoryEntry{TxID: 1, Hash: "first"})
	if e, ok := idx.Get(1); !ok || e.Hash != "first" {
		t.Fatalf("after Upsert: Get(1) = %+v, %v", e, ok)
	}
	idx.Upsert(PingScoreHistoryEntry{TxID: 1, Hash: "second"})
	if e, ok := idx.Get(1); !ok || e.Hash != "second" {
		t.Fatalf("after re-Upsert: Get(1) = %+v, %v, want second", e, ok)
	}
	idx.Delete(1)
	if _, ok := idx.Get(1); ok {
		t.Error("after Delete: Get(1) still present")
	}
	idx.Delete(999) // no-op, must not panic
}

// TestPingScoreHistoryIndex_CloneIndependence proves a failed future
// recompute cycle working against a clone can never mutate the original --
// the core guarantee Clone exists for.
func TestPingScoreHistoryIndex_CloneIndependence(t *testing.T) {
	original := newPingScoreHistoryIndex([]PingScoreHistoryEntry{
		{TxID: 1, Hash: "orig1", AirtimeMs: f64(100)},
		{TxID: 2, Hash: "orig2"},
	})
	clone := original.Clone()

	// Mutate the clone: upsert a changed entry, delete another, add a new one.
	clone.Upsert(PingScoreHistoryEntry{TxID: 1, Hash: "mutated1", AirtimeMs: f64(999)})
	clone.Delete(2)
	clone.Upsert(PingScoreHistoryEntry{TxID: 3, Hash: "new3"})

	// Original must be completely unaffected.
	if e, ok := original.Get(1); !ok || e.Hash != "orig1" || e.AirtimeMs == nil || *e.AirtimeMs != 100 {
		t.Errorf("original.Get(1) = %+v, %v, want unchanged orig1/AirtimeMs=100", e, ok)
	}
	if _, ok := original.Get(2); !ok {
		t.Error("original.Get(2) missing -- clone's Delete leaked into original")
	}
	if _, ok := original.Get(3); ok {
		t.Error("original.Get(3) present -- clone's Upsert of a new entry leaked into original")
	}
	if original.Len() != 2 {
		t.Errorf("original.Len() = %d, want 2 (unaffected by clone mutations)", original.Len())
	}

	// Clone independently reflects its own mutations.
	if e, ok := clone.Get(1); !ok || e.Hash != "mutated1" {
		t.Errorf("clone.Get(1) = %+v, %v, want mutated1", e, ok)
	}
	if _, ok := clone.Get(2); ok {
		t.Error("clone.Get(2) still present after Delete")
	}
	if clone.Len() != 2 { // 1 (mutated) + 3 (new), 2 deleted
		t.Errorf("clone.Len() = %d, want 2", clone.Len())
	}
}

// --- planPingScoreHistoryReconcile ---------------------------------------

func TestPlanPingScoreHistoryReconcile_NewTrigger(t *testing.T) {
	idx := newPingScoreHistoryIndex(nil)
	triggers := []pingTriggerRow{{txID: 1, hash: "h1", firstSeen: "t1"}}
	plan := planPingScoreHistoryReconcile(idx, triggers)
	if len(plan.ToCompute) != 1 || plan.ToCompute[0].txID != 1 {
		t.Errorf("ToCompute = %+v, want [{txID:1}]", plan.ToCompute)
	}
	if len(plan.Invalidated) != 0 {
		t.Errorf("Invalidated = %v, want empty (a brand-new trigger isn't an invalidation)", plan.Invalidated)
	}
	if len(plan.ToDelete) != 0 {
		t.Errorf("ToDelete = %v, want empty", plan.ToDelete)
	}
}

func TestPlanPingScoreHistoryReconcile_RemovedTrigger(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{{TxID: 1, Hash: "h1", Timestamp: "t1"}})
	plan := planPingScoreHistoryReconcile(idx, nil)
	if len(plan.ToDelete) != 1 || plan.ToDelete[0] != 1 {
		t.Errorf("ToDelete = %v, want [1]", plan.ToDelete)
	}
	if len(plan.ToCompute) != 0 {
		t.Errorf("ToCompute = %+v, want empty", plan.ToCompute)
	}
}

func TestPlanPingScoreHistoryReconcile_UnchangedTriggerNeedsNothing(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{{TxID: 1, Hash: "h1", Timestamp: "t1"}})
	triggers := []pingTriggerRow{{txID: 1, hash: "h1", firstSeen: "t1"}}
	plan := planPingScoreHistoryReconcile(idx, triggers)
	if len(plan.ToCompute) != 0 || len(plan.Invalidated) != 0 || len(plan.ToDelete) != 0 {
		t.Errorf("plan = %+v, want a completely empty plan for an unchanged trigger", plan)
	}
}

func TestPlanPingScoreHistoryReconcile_ChangedHashIsInvalidated(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{{TxID: 1, Hash: "oldhash", Timestamp: "t1"}})
	triggers := []pingTriggerRow{{txID: 1, hash: "newhash", firstSeen: "t1"}}
	plan := planPingScoreHistoryReconcile(idx, triggers)
	if len(plan.ToCompute) != 1 || plan.ToCompute[0].hash != "newhash" {
		t.Errorf("ToCompute = %+v, want the trigger with the new hash", plan.ToCompute)
	}
	if len(plan.Invalidated) != 1 || plan.Invalidated[0] != 1 {
		t.Errorf("Invalidated = %v, want [1]", plan.Invalidated)
	}
}

func TestPlanPingScoreHistoryReconcile_ChangedTimestampIsInvalidated(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{{TxID: 1, Hash: "h1", Timestamp: "oldts"}})
	triggers := []pingTriggerRow{{txID: 1, hash: "h1", firstSeen: "newts"}}
	plan := planPingScoreHistoryReconcile(idx, triggers)
	if len(plan.Invalidated) != 1 || plan.Invalidated[0] != 1 {
		t.Errorf("Invalidated = %v, want [1]", plan.Invalidated)
	}
}

func TestPlanPingScoreHistoryReconcile_MixedScenario(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{
		{TxID: 1, Hash: "h1", Timestamp: "t1"}, // unchanged
		{TxID: 2, Hash: "h2", Timestamp: "t2"}, // changed hash -> invalidated
		{TxID: 3, Hash: "h3", Timestamp: "t3"}, // removed -> ToDelete
	})
	triggers := []pingTriggerRow{
		{txID: 1, hash: "h1", firstSeen: "t1"},
		{txID: 2, hash: "h2-changed", firstSeen: "t2"},
		{txID: 4, hash: "h4", firstSeen: "t4"}, // brand new
	}
	plan := planPingScoreHistoryReconcile(idx, triggers)

	computeIDs := map[int64]bool{}
	for _, tr := range plan.ToCompute {
		computeIDs[tr.txID] = true
	}
	if !computeIDs[2] || !computeIDs[4] || computeIDs[1] {
		t.Errorf("ToCompute = %+v, want exactly {2 (invalidated), 4 (new)}", plan.ToCompute)
	}
	if len(plan.Invalidated) != 1 || plan.Invalidated[0] != 2 {
		t.Errorf("Invalidated = %v, want [2]", plan.Invalidated)
	}
	if len(plan.ToDelete) != 1 || plan.ToDelete[0] != 3 {
		t.Errorf("ToDelete = %v, want [3]", plan.ToDelete)
	}
}

func TestPlanPingScoreHistoryReconcile_DoesNotMutateIndex(t *testing.T) {
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{{TxID: 1, Hash: "h1", Timestamp: "t1"}})
	before := idx.Len()
	planPingScoreHistoryReconcile(idx, nil) // ToDelete=[1], but planning must not delete anything itself
	if idx.Len() != before {
		t.Errorf("index Len() changed from %d to %d -- planning must be pure, not mutate", before, idx.Len())
	}
	if _, ok := idx.Get(1); !ok {
		t.Error("entry 1 removed from index by planning alone -- planning must be pure")
	}
}
