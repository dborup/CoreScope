package main

import (
	"testing"
	"time"
)

// #199: MoveStaleNodes must not retire a node whose pubkey is an observer
// seen within nodeDays. An observer never hears its own adverts, so an
// online observer can go a week without one reaching another observer.

const (
	issue199ObserverKey = "a199000000000000000000000000000000000000000000000000000000000001"
	issue199QuietKey    = "a199000000000000000000000000000000000000000000000000000000000002"
	issue199PlainKey    = "a199000000000000000000000000000000000000000000000000000000000003"
	issue199OldAdvert   = "2020-01-01T00:00:00Z"
)

func issue199Upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

func issue199NodeIn(t *testing.T, store *Store, table, pubkey string) (bool, string) {
	t.Helper()
	var lastSeen string
	err := store.db.QueryRow(`SELECT last_seen FROM `+table+` WHERE public_key = ?`, pubkey).Scan(&lastSeen)
	if err != nil {
		return false, ""
	}
	return true, lastSeen
}

func issue199Observer(t *testing.T, store *Store, id, lastSeen string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO observers (id, name, last_seen, first_seen) VALUES (?, 'obs', ?, ?)`, id, lastSeen, lastSeen); err != nil {
		t.Fatal(err)
	}
}

func TestMoveStaleNodesKeepsActiveObserverNode(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertNode(issue199ObserverKey, "ObsRepeater", "repeater", nil, nil, issue199OldAdvert); err != nil {
		t.Fatal(err)
	}
	// Observer ids are upper-case pubkeys from the MQTT topic; node keys are lower-case.
	issue199Observer(t, store, issue199Upper(issue199ObserverKey), time.Now().UTC().Format(time.RFC3339))

	moved, err := store.MoveStaleNodes(7)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Errorf("moved=%d, want 0: an observer seen within nodeDays must stay a node", moved)
	}
	in, lastSeen := issue199NodeIn(t, store, "nodes", issue199ObserverKey)
	if !in {
		t.Fatal("active observer's node was removed from nodes")
	}
	if lastSeen != issue199OldAdvert {
		t.Errorf("last_seen=%q, want %q: last_seen is the last advert and must not be faked", lastSeen, issue199OldAdvert)
	}
	if in, _ := issue199NodeIn(t, store, "inactive_nodes", issue199ObserverKey); in {
		t.Error("active observer's node was copied to inactive_nodes")
	}
}

func TestMoveStaleNodesRetiresQuietObserverAndPlainNode(t *testing.T) {
	store := newTestStore(t)
	for _, pk := range []string{issue199QuietKey, issue199PlainKey} {
		if err := store.UpsertNode(pk, "Stale", "repeater", nil, nil, issue199OldAdvert); err != nil {
			t.Fatal(err)
		}
	}
	// Last heard as an observer 8 days ago: outside the 7-day window.
	issue199Observer(t, store, issue199Upper(issue199QuietKey), time.Now().UTC().AddDate(0, 0, -8).Format(time.RFC3339))

	moved, err := store.MoveStaleNodes(7)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Errorf("moved=%d, want 2", moved)
	}
	for _, pk := range []string{issue199QuietKey, issue199PlainKey} {
		if in, _ := issue199NodeIn(t, store, "nodes", pk); in {
			t.Errorf("%s still in nodes, want retired", pk[:8])
		}
		if in, _ := issue199NodeIn(t, store, "inactive_nodes", pk); !in {
			t.Errorf("%s missing from inactive_nodes", pk[:8])
		}
	}
}

// A legacy observers row with a NULL id must not stop retention: with a bare
// NOT IN, one NULL in the subquery makes the predicate NULL for every node.
func TestMoveStaleNodesNullObserverIDDoesNotBlockRetention(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertNode(issue199PlainKey, "Stale", "companion", nil, nil, issue199OldAdvert); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO observers (id, name, last_seen) VALUES (NULL, 'legacy', ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	moved, err := store.MoveStaleNodes(7)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Errorf("moved=%d, want 1", moved)
	}
}
