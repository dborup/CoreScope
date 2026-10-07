package main

// Tests for the opt-in retention of the tables nothing else prunes (#329):
// retention.inactiveNodeDays, nodeChangeDays and pingTriggerDays here, and
// observerPurgeDays in observer_purge_test.go.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// daysAgo is an RFC3339 timestamp n days before now, the format every
// retention column is written in.
func daysAgo(n int) string {
	return time.Now().UTC().AddDate(0, 0, -n).Format(time.RFC3339)
}

// setRetentionBatchRows lowers retentionBatchRows for one test, so a handful
// of rows spans several batches.
func setRetentionBatchRows(t *testing.T, n int) {
	t.Helper()
	old := retentionBatchRows
	retentionBatchRows = n
	t.Cleanup(func() { retentionBatchRows = old })
}

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func rowExists(t *testing.T, s *Store, query string, args ...any) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM (`+query+`)`, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n > 0
}

// ─── config ────────────────────────────────────────────────────────────────

func TestTableRetentionConfig(t *testing.T) {
	cases := []struct {
		name string
		json string
		want TableRetention
	}{
		{"no retention block", `{}`, TableRetention{}},
		{"unset", `{"retention":{"nodeDays":7,"packetDays":30}}`, TableRetention{}},
		{"set", `{"retention":{"inactiveNodeDays":30,"nodeChangeDays":31,"pingTriggerDays":32,"observerPurgeDays":33}}`,
			TableRetention{InactiveNodeDays: 30, NodeChangeDays: 31, PingTriggerDays: 32, ObserverPurgeDays: 33}},
		{"negative disables", `{"retention":{"inactiveNodeDays":-1,"nodeChangeDays":-1,"pingTriggerDays":-1,"observerPurgeDays":-1}}`, TableRetention{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(c.json), &cfg); err != nil {
				t.Fatal(err)
			}
			got := cfg.TableRetention()
			if got != c.want {
				t.Errorf("TableRetention() = %+v, want %+v", got, c.want)
			}
			if got.Enabled() != (c.want != TableRetention{}) {
				t.Errorf("Enabled() = %v for %+v", got.Enabled(), got)
			}
		})
	}
}

// config.example.json documents every window, set to 0 so that copying it as
// a live config changes nothing.
func TestConfigExampleDocumentsTableRetention(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.TableRetention(); got.Enabled() {
		t.Errorf("config.example.json enables table retention: %+v, want every window 0", got)
	}
	var raw struct {
		Retention map[string]any `json:"retention"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	doc, _ := raw.Retention["_comment_tableRetention"].(string)
	for _, k := range []string{"inactiveNodeDays", "nodeChangeDays", "pingTriggerDays", "observerPurgeDays"} {
		if v, ok := raw.Retention[k]; !ok || v != float64(0) {
			t.Errorf("retention.%s = %v (present %v), want 0", k, v, ok)
		}
		if !strings.Contains(doc, k+":") {
			t.Errorf("retention._comment_tableRetention does not document %s", k)
		}
	}
}

// ─── inactive_nodes ────────────────────────────────────────────────────────

func insertInactiveNode(t *testing.T, s *Store, pubkey string, ageDays int) {
	t.Helper()
	mustExec(t, s, `INSERT INTO inactive_nodes (public_key, name, last_seen, first_seen) VALUES (?, ?, ?, ?)`,
		pubkey, pubkey, daysAgo(ageDays), daysAgo(ageDays+10))
}

func inactiveNodeExists(t *testing.T, s *Store, pubkey string) bool {
	t.Helper()
	return rowExists(t, s, `SELECT 1 FROM inactive_nodes WHERE public_key = ?`, pubkey)
}

func TestPruneInactiveNodesDeletesOldKeepsRecent(t *testing.T) {
	store := newTestStore(t)
	insertInactiveNode(t, store, "aa01", 90)
	insertInactiveNode(t, store, "aa02", 20)

	n, err := store.PruneInactiveNodes(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted=%d, want 1", n)
	}
	if inactiveNodeExists(t, store, "aa01") {
		t.Error("aa01 (90 days) still present, want deleted")
	}
	if !inactiveNodeExists(t, store, "aa02") {
		t.Error("aa02 (20 days) deleted, want kept")
	}
}

// A node that came back has a nodes row next to its old inactive_nodes row.
// That row still carries the node's confirmed default_scope evidence and
// keeps the node out of New Nodes, so it stays while the node is active.
func TestPruneInactiveNodesKeepsReturnedNode(t *testing.T) {
	store := newTestStore(t)
	insertInactiveNode(t, store, "aa03", 90)
	if err := store.UpsertNode("aa03", "Back", "repeater", nil, nil, daysAgo(0)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PruneInactiveNodes(30); err != nil {
		t.Fatal(err)
	}
	if !inactiveNodeExists(t, store, "aa03") {
		t.Error("inactive row of a node that came back was deleted, want kept")
	}
}

// MoveStaleNodes keeps the node of an observer that is still uploading (#199);
// the purge does not delete what MoveStaleNodes would keep.
func TestPruneInactiveNodesKeepsActiveObserverNode(t *testing.T) {
	store := newTestStore(t)
	insertInactiveNode(t, store, "aa04", 90)
	if err := store.UpsertObserver("AA04", "Obs", "LAX", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PruneInactiveNodes(30); err != nil {
		t.Fatal(err)
	}
	if !inactiveNodeExists(t, store, "aa04") {
		t.Error("inactive row of an active observer was deleted, want kept")
	}
}

func TestPruneInactiveNodesBatches(t *testing.T) {
	setRetentionBatchRows(t, 2)
	store := newTestStore(t)
	for _, pk := range []string{"ab01", "ab02", "ab03", "ab04", "ab05"} {
		insertInactiveNode(t, store, pk, 90)
	}
	insertInactiveNode(t, store, "ab06", 1)
	ResetWriterStatsForTest()

	n, err := store.PruneInactiveNodes(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || countRows(t, store, "inactive_nodes") != 1 {
		t.Errorf("deleted=%d left=%d, want 5 and 1", n, countRows(t, store, "inactive_nodes"))
	}
	if got := store.WriterStatsSnapshot()["prune_inactive_nodes"].Count; got != 3 {
		t.Errorf("prune_inactive_nodes transactions = %d, want 3", got)
	}
}

// ─── node_changes ──────────────────────────────────────────────────────────

func insertNodeChange(t *testing.T, s *Store, pubkey string, ageDays int) {
	t.Helper()
	mustExec(t, s, `INSERT INTO node_changes (public_key, change_type, old_value, new_value, detected_at) VALUES (?, 'name', 'a', 'b', ?)`,
		pubkey, daysAgo(ageDays))
}

func TestPruneNodeChangesDeletesOldKeepsRecent(t *testing.T) {
	store := newTestStore(t)
	insertNodeChange(t, store, "old", 90)
	insertNodeChange(t, store, "new", 20)

	n, err := store.PruneNodeChanges(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted=%d, want 1", n)
	}
	if rowExists(t, store, `SELECT 1 FROM node_changes WHERE public_key = 'old'`) {
		t.Error("90-day change still present, want deleted")
	}
	if !rowExists(t, store, `SELECT 1 FROM node_changes WHERE public_key = 'new'`) {
		t.Error("20-day change deleted, want kept")
	}
}

func TestPruneNodeChangesBatches(t *testing.T) {
	setRetentionBatchRows(t, 2)
	store := newTestStore(t)
	for i := 0; i < 4; i++ {
		insertNodeChange(t, store, "old", 90+i)
	}
	insertNodeChange(t, store, "new", 1)
	ResetWriterStatsForTest()

	n, err := store.PruneNodeChanges(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || countRows(t, store, "node_changes") != 1 {
		t.Errorf("deleted=%d left=%d, want 4 and 1", n, countRows(t, store, "node_changes"))
	}
	if got := store.WriterStatsSnapshot()["prune_node_changes"].Count; got != 3 {
		t.Errorf("prune_node_changes transactions = %d, want 3 (batches of 2, 2, 0)", got)
	}
}

// ─── ping_triggers ─────────────────────────────────────────────────────────

func insertPingTrigger(t *testing.T, s *Store, txID int64, ageDays int) {
	t.Helper()
	mustExec(t, s, `INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen) VALUES (?, ?, '#ping', 'Alice', ?)`,
		txID, fmt.Sprintf("hash%d", txID), daysAgo(ageDays))
}

func pingTriggerExists(t *testing.T, s *Store, txID int64) bool {
	t.Helper()
	return rowExists(t, s, `SELECT 1 FROM ping_triggers WHERE tx_id = ?`, txID)
}

func TestPrunePingTriggersDeletesOldKeepsRecent(t *testing.T) {
	store := newTestStore(t)
	insertPingTrigger(t, store, 1, 90)
	insertPingTrigger(t, store, 2, 20)

	n, err := store.PrunePingTriggers(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted=%d, want 1", n)
	}
	if pingTriggerExists(t, store, 1) {
		t.Error("90-day trigger still present, want deleted")
	}
	if !pingTriggerExists(t, store, 2) {
		t.Error("20-day trigger deleted, want kept")
	}
}

// The Ping Scores history keeps an entry only while its ping_triggers row
// exists (planPingScoreHistoryReconcile drops the rest), and an entry is
// marked data_pruned exactly when its transmission is gone. So the trigger
// of a pruned transmission must survive until pingTriggerDays: following the
// transmission prune would remove every data_pruned entry at once.
func TestPrunePingTriggersKeepsTriggerOfPrunedTransmission(t *testing.T) {
	store := newTestStore(t)
	insertChanTx(t, store, "pingpruned01", "Alice: ping", "#ping")
	var txID int64
	if err := store.db.QueryRow(`SELECT tx_id FROM ping_triggers`).Scan(&txID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, `UPDATE ping_triggers SET first_seen = ? WHERE tx_id = ?`, daysAgo(20), txID)
	mustExec(t, store, `UPDATE transmissions SET first_seen = ? WHERE id = ?`, daysAgo(20), txID)
	if n, err := store.PruneOldPackets(10); err != nil || n != 1 {
		t.Fatalf("PruneOldPackets(10) = %d, %v; want the transmission pruned", n, err)
	}

	if _, err := store.PrunePingTriggers(30); err != nil {
		t.Fatal(err)
	}
	if !pingTriggerExists(t, store, txID) {
		t.Error("trigger of a pruned transmission, inside pingTriggerDays, was deleted; want kept (data_pruned entries need it)")
	}
}

// pingTriggerDays unset: the transmission prune leaves ping_triggers alone, as
// before #329.
func TestPruneOldPacketsLeavesPingTriggers(t *testing.T) {
	store := newTestStore(t)
	insertChanTx(t, store, "pingpruned02", "Bob: ping", "#ping")
	mustExec(t, store, `UPDATE transmissions SET first_seen = ?`, daysAgo(90))
	mustExec(t, store, `UPDATE ping_triggers SET first_seen = ?`, daysAgo(90))
	if n, err := store.PruneOldPackets(30); err != nil || n != 1 {
		t.Fatalf("PruneOldPackets(30) = %d, %v; want the transmission pruned", n, err)
	}
	runTableRetention(store, TableRetention{}, "test")
	if got := countPingTriggers(t, store); got != 1 {
		t.Errorf("ping_triggers = %d, want 1", got)
	}
}

// Triggers are walked by tx_id, a batch of ids per transaction, because no
// index covers first_seen. Old and recent rows interleave across batches.
func TestPrunePingTriggersBatches(t *testing.T) {
	setRetentionBatchRows(t, 2)
	store := newTestStore(t)
	ages := []int{90, 1, 90, 90, 1, 90, 90}
	for i, age := range ages {
		insertPingTrigger(t, store, int64(i+1), age)
	}
	ResetWriterStatsForTest()

	n, err := store.PrunePingTriggers(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("deleted=%d, want 5", n)
	}
	for _, id := range []int64{2, 5} {
		if !pingTriggerExists(t, store, id) {
			t.Errorf("recent trigger %d deleted, want kept", id)
		}
	}
	if got := store.WriterStatsSnapshot()["prune_ping_triggers"].Count; got != 4 {
		t.Errorf("prune_ping_triggers transactions = %d, want 4 (7 ids in ranges of 2)", got)
	}
}

// ─── wiring ────────────────────────────────────────────────────────────────

// seedTableRetentionRows gives each of the four tables one 90-day-old row
// and one 1-day-old row.
func seedTableRetentionRows(t *testing.T, s *Store) {
	t.Helper()
	insertInactiveNode(t, s, "cc01", 90)
	insertInactiveNode(t, s, "cc02", 1)
	insertNodeChange(t, s, "cc01", 90)
	insertNodeChange(t, s, "cc02", 1)
	insertPingTrigger(t, s, 1, 90)
	insertPingTrigger(t, s, 2, 1)
	staleObserver(t, s, "obs-cc01")
	if err := s.UpsertObserver("obs-cc02", "obs-cc02", "LAX", nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE observers SET inactive = 1, last_seen = ? WHERE id = 'obs-cc02'`, daysAgo(1))
}

// Every knob unset: nothing is deleted from any of the four tables.
func TestRunTableRetentionUnsetChangesNothing(t *testing.T) {
	store := newTestStore(t)
	seedTableRetentionRows(t, store)

	runTableRetention(store, TableRetention{}, "test")

	for _, table := range []string{"inactive_nodes", "node_changes", "ping_triggers", "observers"} {
		if got := countRows(t, store, table); got != 2 {
			t.Errorf("%s rows = %d, want 2 (all knobs unset)", table, got)
		}
	}
}

// Each knob prunes its own table and only that one.
func TestRunTableRetentionEachKnob(t *testing.T) {
	cases := []struct {
		table string
		r     TableRetention
	}{
		{"inactive_nodes", TableRetention{InactiveNodeDays: 30}},
		{"node_changes", TableRetention{NodeChangeDays: 30}},
		{"ping_triggers", TableRetention{PingTriggerDays: 30}},
		{"observers", TableRetention{ObserverPurgeDays: 30}},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			store := newTestStore(t)
			seedTableRetentionRows(t, store)

			runTableRetention(store, c.r, "test")

			for _, table := range []string{"inactive_nodes", "node_changes", "ping_triggers", "observers"} {
				want := 2
				if table == c.table {
					want = 1
				}
				if got := countRows(t, store, table); got != want {
					t.Errorf("%s rows = %d, want %d", table, got, want)
				}
			}
		})
	}
}

// tx_id is a transmissions id, and those can be 0 or negative (the CI E2E
// fixture seeds both). The walk starts below every key.
func TestPrunePingTriggersNonPositiveTxID(t *testing.T) {
	store := newTestStore(t)
	insertPingTrigger(t, store, -1000000, 90)
	insertPingTrigger(t, store, 0, 90)
	insertPingTrigger(t, store, 1, 90)

	n, err := store.PrunePingTriggers(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || countPingTriggers(t, store) != 0 {
		t.Errorf("deleted=%d left=%d, want 3 and 0", n, countPingTriggers(t, store))
	}
}
