package main

import (
	"testing"
	"time"
)

// #329: the ingestor's retention.pingTriggerDays deletes ping_triggers rows by
// their own age, not when their transmission is pruned. These tests pin the
// two halves of why, on the server side of the shared DB.

// dataPrunedFixture settles one ping 40 days old, removes its transmission
// and observations the way the ingestor's packetDays prune does (the
// ping_triggers row stays), and runs the cycle that marks it data_pruned.
func dataPrunedFixture(t *testing.T, hash string) (*engineFixture, int64) {
	t.Helper()
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	firstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	txID := setupSettledPingWithGoneData(t, fx, hash, firstSeen)
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
	return fx, txID
}

// A data_pruned entry whose trigger row is still there keeps rendering in the
// all-time records, cycle after cycle, with its transmission gone.
func TestPingScoresDataPrunedEntryRendersWhileTriggerKept(t *testing.T) {
	fx, _ := dataPrunedFixture(t, "keeptrigger329")
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

// Once its trigger row is deleted, the entry leaves the history store too
// (and with it the sender name). This is why ping_triggers cannot follow the
// transmission prune: every data_pruned entry has a pruned transmission.
func TestPingScoresDataPrunedEntryDroppedWithItsTrigger(t *testing.T) {
	fx, txID := dataPrunedFixture(t, "droptrigger329")
	if _, err := fx.srv.db.conn.Exec(`DELETE FROM ping_triggers WHERE tx_id = ?`, txID); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}

	if _, ok := fx.engine.index.Get(txID); ok {
		t.Error("history entry still indexed after its trigger was deleted")
	}
	stored, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stored {
		if e.TxID == txID {
			t.Errorf("history entry still stored after its trigger was deleted: %+v", e)
		}
	}
	snap, err := fx.srv.buildPingScoresSnapshotFromHistory(mustFetchTriggers(t, fx), fx.engine.index.Entries(), fx.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.MostHopsPing != nil && snap.MostHopsPing.Hash == "droptrigger329" {
		t.Error("entry of a deleted trigger still rendered")
	}
}
