package main

import (
	"testing"
	"time"
)

// Tests for retention.observerPurgeDays (#329), ported from upstream#1886
// (#34) and extended with the observer_neighbors / observer_neighbor_metrics
// rows that port did not cover.

// staleObserver inserts an observer already soft-deleted (inactive = 1) with
// last_seen 90 days in the past, and returns its rowid.
func staleObserver(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	if err := s.UpsertObserver(id, id, "LAX", nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE observers SET last_seen = ?, inactive = 1 WHERE id = ?`, old, id); err != nil {
		t.Fatal(err)
	}
	var rowid int64
	if err := s.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, id).Scan(&rowid); err != nil {
		t.Fatal(err)
	}
	return rowid
}

func observerExists(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observers WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestPurgeStaleObserversDeletesUnreferencedRow(t *testing.T) {
	store := newTestStore(t)
	staleObserver(t, store, "obs-gone")

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Errorf("purged=%d, want 1", purged)
	}
	if observerExists(t, store, "obs-gone") {
		t.Error("obs-gone still present, want hard-deleted")
	}
}

// Regression: an observer whose rowid is still referenced by observations must
// survive the purge — deleting it orphans observations.observer_idx, which
// breaks the packets_v join and mis-attributes historical packets.
func TestPurgeStaleObserversKeepsObserverWithObservations(t *testing.T) {
	store := newTestStore(t)
	rowid := staleObserver(t, store, "obs-referenced")

	if _, err := store.db.Exec(
		`INSERT INTO transmissions (id, raw_hex, hash, first_seen) VALUES (1, 'AA', 'h1', ?)`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO observations (transmission_id, observer_idx, timestamp) VALUES (1, ?, 0)`, rowid,
	); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Errorf("purged=%d, want 0 (row is referenced by observations)", purged)
	}
	if !observerExists(t, store, "obs-referenced") {
		t.Error("obs-referenced was deleted, want kept")
	}

	var orphans int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE observer_idx IS NOT NULL
		 AND observer_idx NOT IN (SELECT rowid FROM observers)`,
	).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("orphaned observations=%d, want 0", orphans)
	}
}

func TestPurgeStaleObserversKeepsObserverWithMetrics(t *testing.T) {
	store := newTestStore(t)
	staleObserver(t, store, "obs-metrics")

	if _, err := store.db.Exec(
		`INSERT INTO observer_metrics (observer_id, timestamp) VALUES ('obs-metrics', ?)`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Errorf("purged=%d, want 0 (row is referenced by observer_metrics)", purged)
	}
	if !observerExists(t, store, "obs-metrics") {
		t.Error("obs-metrics was deleted, want kept")
	}
}

func TestPurgeStaleObserversKeepsObserverWithDroppedPackets(t *testing.T) {
	store := newTestStore(t)
	staleObserver(t, store, "obs-dropped")

	if _, err := store.db.Exec(
		`INSERT INTO dropped_packets (reason, observer_id) VALUES ('bad-sig', 'obs-dropped')`,
	); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Errorf("purged=%d, want 0 (row is referenced by dropped_packets)", purged)
	}
	if !observerExists(t, store, "obs-dropped") {
		t.Error("obs-dropped was deleted, want kept")
	}
}

// An observer old enough to purge but not yet soft-deleted must be left to
// RemoveStaleObservers — the hard purge only ever finalises an inactive row.
func TestPurgeStaleObserversKeepsActiveObserver(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertObserver("obs-active", "Active", "LAX", nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE observers SET last_seen = ? WHERE id = ?`, old, "obs-active"); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Errorf("purged=%d, want 0 (inactive = 0)", purged)
	}
	if !observerExists(t, store, "obs-active") {
		t.Error("obs-active was deleted, want kept")
	}
}

func TestPurgeStaleObserversKeepsObserverInsideWindow(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertObserver("obs-recent", "Recent", "LAX", nil); err != nil {
		t.Fatal(err)
	}
	recent := time.Now().UTC().AddDate(0, 0, -20).Format(time.RFC3339)
	if _, err := store.db.Exec(
		`UPDATE observers SET last_seen = ?, inactive = 1 WHERE id = ?`, recent, "obs-recent",
	); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Errorf("purged=%d, want 0 (last_seen inside the 30-day window)", purged)
	}
	if !observerExists(t, store, "obs-recent") {
		t.Error("obs-recent was deleted, want kept")
	}
}

func TestPurgeStaleObserversDisabled(t *testing.T) {
	store := newTestStore(t)
	staleObserver(t, store, "obs-kept")

	for _, days := range []int{0, -1} {
		purged, err := store.PurgeStaleObservers(days)
		if err != nil {
			t.Fatal(err)
		}
		if purged != 0 {
			t.Errorf("PurgeStaleObservers(%d) purged=%d, want 0 (disabled)", days, purged)
		}
	}
	if !observerExists(t, store, "obs-kept") {
		t.Error("obs-kept was deleted, want kept while purge is disabled")
	}
}

// observer_neighbors is a current-only snapshot that only the observer itself
// replaces, so a purged observer would otherwise serve its last neighbour
// list forever (GetObserverNeighbors does not join observers). The purge
// deletes it in the same transaction as the observer row.
func TestPurgeStaleObserversDeletesNeighborSnapshot(t *testing.T) {
	store := newTestStore(t)
	staleObserver(t, store, "obs-nbr")
	staleObserver(t, store, "obs-nbr-kept")
	if _, err := store.db.Exec(
		`INSERT INTO observer_metrics (observer_id, timestamp) VALUES ('obs-nbr-kept', ?)`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"obs-nbr", "obs-nbr-kept"} {
		if _, err := store.db.Exec(
			`INSERT INTO observer_neighbors (observer_id, neighbor_pubkey, status, reported_at) VALUES (?, 'aabb', 'ok', ?)`,
			id, time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339),
		); err != nil {
			t.Fatal(err)
		}
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Errorf("purged=%d, want 1", purged)
	}
	if observerExists(t, store, "obs-nbr") {
		t.Error("obs-nbr still present, want hard-deleted")
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbors WHERE observer_id = 'obs-nbr'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("observer_neighbors rows of the purged observer = %d, want 0", n)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbors WHERE observer_id = 'obs-nbr-kept'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("observer_neighbors rows of a kept observer = %d, want 1", n)
	}
}

// observer_neighbor_metrics is history that ages out by metricsDays, like
// observer_metrics, so it guards the observer the same way.
func TestPurgeStaleObserversKeepsObserverWithNeighborMetrics(t *testing.T) {
	store := newTestStore(t)
	staleObserver(t, store, "obs-nbr-metrics")
	if _, err := store.db.Exec(
		`INSERT INTO observer_neighbor_metrics (observer_id, neighbor_pubkey, timestamp, snr) VALUES ('obs-nbr-metrics', 'aabb', ?, 5)`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Errorf("purged=%d, want 0 (row is referenced by observer_neighbor_metrics)", purged)
	}
	if !observerExists(t, store, "obs-nbr-metrics") {
		t.Error("obs-nbr-metrics was deleted, want kept")
	}
}

// More candidates than one batch: every one is purged, over several
// transactions.
func TestPurgeStaleObserversBatches(t *testing.T) {
	setRetentionBatchRows(t, 2)
	store := newTestStore(t)
	for _, id := range []string{"obs-b1", "obs-b2", "obs-b3", "obs-b4", "obs-b5"} {
		staleObserver(t, store, id)
	}
	ResetWriterStatsForTest()

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 5 {
		t.Errorf("purged=%d, want 5", purged)
	}
	if got := countRows(t, store, "observers"); got != 0 {
		t.Errorf("observers left = %d, want 0", got)
	}
	if got := store.WriterStatsSnapshot()["purge_observers"].Count; got != 3 {
		t.Errorf("purge_observers transactions = %d, want 3 (batches of 2, 2, 1)", got)
	}
}

// SQLite lets a TEXT PRIMARY KEY hold NULL. Such a row is never a candidate,
// and it does not stop the purge of the others.
func TestPurgeStaleObserversSkipsNullID(t *testing.T) {
	store := newTestStore(t)
	staleObserver(t, store, "obs-real")
	if _, err := store.db.Exec(`INSERT INTO observers (id, name, last_seen, inactive) VALUES (NULL, 'null-id', ?, 1)`,
		time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	purged, err := store.PurgeStaleObservers(30)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 || observerExists(t, store, "obs-real") {
		t.Errorf("purged=%d, obs-real present=%v; want 1 and false", purged, observerExists(t, store, "obs-real"))
	}
}
