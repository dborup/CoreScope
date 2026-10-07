package main

import (
	"testing"
	"time"
)

// #329: the ingestor's table retention never deletes ping_triggers rows, so a
// Ping Scores history entry outlives the packetDays prune of its transmission.
// This pins the server side of that on the shared DB: an entry already marked
// data_pruned keeps rendering (the #329 acceptance criterion).

// A ping settled 40 days ago loses its transmission and observations the way
// the ingestor's packetDays prune removes them (the ping_triggers row stays).
// Its history entry is marked data_pruned and keeps rendering in the all-time
// records, cycle after cycle.
func TestPingScoresDataPrunedEntryRendersWhileTriggerKept(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	firstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	txID := setupSettledPingWithGoneData(t, fx, "keeptrigger329", firstSeen)
	if _, err := fx.srv.db.conn.Exec(`DELETE FROM transmissions WHERE id = ?`, txID); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(2 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	if e, _ := fx.engine.index.Get(txID); !e.DataPruned {
		t.Fatalf("sanity check failed: entry not data_pruned after its transmission was pruned: %+v", e)
	}
	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}

	snap, err := fx.srv.buildPingScoresSnapshotFromHistory(mustFetchTriggers(t, fx), fx.engine.index.Entries(), fx.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.MostHopsPing == nil || snap.MostHopsPing.Hash != "keeptrigger329" {
		t.Errorf("data_pruned entry not rendered in the all-time records: %+v", snap.MostHopsPing)
	}
}
