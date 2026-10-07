package main

// Tests for the opt-in retention of the tables nothing else prunes (#329):
// retention.inactiveNodeDays and nodeChangeDays here, and observerPurgeDays in
// observer_purge_test.go. ping_triggers is deliberately not among them; see
// TestTableRetentionLeavesPingTriggers.

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
		{"set", `{"retention":{"inactiveNodeDays":30,"nodeChangeDays":31,"observerPurgeDays":33}}`,
			TableRetention{InactiveNodeDays: 30, NodeChangeDays: 31, ObserverPurgeDays: 33}},
		{"negative disables", `{"retention":{"inactiveNodeDays":-1,"nodeChangeDays":-1,"observerPurgeDays":-1}}`, TableRetention{}},
		// An earlier draft of #329 had this knob; ping_triggers is never pruned.
		{"pingTriggerDays ignored", `{"retention":{"pingTriggerDays":30}}`, TableRetention{}},
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
	for _, k := range []string{"inactiveNodeDays", "nodeChangeDays", "observerPurgeDays"} {
		if v, ok := raw.Retention[k]; !ok || v != float64(0) {
			t.Errorf("retention.%s = %v (present %v), want 0", k, v, ok)
		}
		if !strings.Contains(doc, k+":") {
			t.Errorf("retention._comment_tableRetention does not document %s", k)
		}
	}
	// ping_triggers is kept forever (operator decision on #329), so neither
	// the example nor the user guide offers a knob for it.
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "user-guide", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"config.example.json": string(data), "configuration.md": string(guide)} {
		if strings.Contains(text, "pingTriggerDays") {
			t.Errorf("%s documents pingTriggerDays; ping_triggers is never pruned", name)
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

// ─── ping_triggers: never pruned ───────────────────────────────────────────

func insertPingTrigger(t *testing.T, s *Store, txID int64, ageDays int) {
	t.Helper()
	mustExec(t, s, `INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen) VALUES (?, ?, '#ping', 'Alice', ?)`,
		txID, fmt.Sprintf("hash%d", txID), daysAgo(ageDays))
}

// pingTriggerRows dumps every ping_triggers row, so a test can tell that none
// was deleted or rewritten.
func pingTriggerRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT tx_id || '|' || hash || '|' || IFNULL(channel_hash, '') || '|' ||
		IFNULL(sender, '') || '|' || first_seen FROM ping_triggers ORDER BY tx_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ping_triggers keeps every row (operator decision on #329): the all-time Ping
// Scores records join it with the history sidecar (#349, #241). With every
// other retention knob set, and the transmission prune run first, no trigger
// is deleted or changed, however old it is or whether its transmission is
// gone. pingTriggerDays, a knob in an earlier draft of #329, is ignored.
func TestTableRetentionLeavesPingTriggers(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"retention":{"packetDays":30,"inactiveNodeDays":30,`+
		`"nodeChangeDays":30,"observerPurgeDays":30,"pingTriggerDays":30}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	insertChanTx(t, store, "pingpruned03", "Carol: ping", "#ping")
	mustExec(t, store, `UPDATE transmissions SET first_seen = ? WHERE hash = 'pingpruned03'`, daysAgo(90))
	mustExec(t, store, `UPDATE ping_triggers SET first_seen = ? WHERE hash = 'pingpruned03'`, daysAgo(90))
	insertPingTrigger(t, store, 101, 90)
	insertPingTrigger(t, store, 102, 1)
	seedTableRetentionRows(t, store)
	before := pingTriggerRows(t, store)
	if len(before) != 3 {
		t.Fatalf("seeded %d ping_triggers, want 3", len(before))
	}

	if n, err := store.PruneOldPackets(cfg.PacketDaysOrZero()); err != nil || n != 1 {
		t.Fatalf("PruneOldPackets = %d, %v; want the ping's transmission pruned", n, err)
	}
	runTableRetention(store, cfg.TableRetention(), "test")

	for _, table := range []string{"inactive_nodes", "node_changes", "observers"} {
		if got := countRows(t, store, table); got != 1 {
			t.Errorf("%s rows = %d, want 1 (its knob is set)", table, got)
		}
	}
	after := pingTriggerRows(t, store)
	if strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("ping_triggers changed by retention:\nbefore %q\nafter  %q", before, after)
	}
}

// ─── wiring ────────────────────────────────────────────────────────────────

// seedTableRetentionRows gives each of the three tables one 90-day-old row
// and one 1-day-old row.
func seedTableRetentionRows(t *testing.T, s *Store) {
	t.Helper()
	insertInactiveNode(t, s, "cc01", 90)
	insertInactiveNode(t, s, "cc02", 1)
	insertNodeChange(t, s, "cc01", 90)
	insertNodeChange(t, s, "cc02", 1)
	staleObserver(t, s, "obs-cc01")
	if err := s.UpsertObserver("obs-cc02", "obs-cc02", "LAX", nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE observers SET inactive = 1, last_seen = ? WHERE id = 'obs-cc02'`, daysAgo(1))
}

// Every knob unset: nothing is deleted from any of the three tables.
func TestRunTableRetentionUnsetChangesNothing(t *testing.T) {
	store := newTestStore(t)
	seedTableRetentionRows(t, store)

	runTableRetention(store, TableRetention{}, "test")

	for _, table := range []string{"inactive_nodes", "node_changes", "observers"} {
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
		{"observers", TableRetention{ObserverPurgeDays: 30}},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			store := newTestStore(t)
			seedTableRetentionRows(t, store)

			runTableRetention(store, c.r, "test")

			for _, table := range []string{"inactive_nodes", "node_changes", "observers"} {
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
