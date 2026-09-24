package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/meshcore-analyzer/dbschema"
)

// Issue #89 backfill: legacy rows (route_mask IS NULL) get the bit of their
// stored route_type plus every bit that can be read from the surviving
// observations.raw_hex headers. That is a lower bound: a variant whose frame
// no longer exists anywhere is not invented.

func routeMaskShrinkBackfill(t *testing.T, batch int) {
	t.Helper()
	oldBatch, oldYield := routeMaskBackfillBatchSize, routeMaskBackfillYield
	routeMaskBackfillBatchSize, routeMaskBackfillYield = batch, 0
	t.Cleanup(func() { routeMaskBackfillBatchSize, routeMaskBackfillYield = oldBatch, oldYield })
}

func routeMaskMakeLegacy(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE transmissions SET route_mask = NULL`); err != nil {
		t.Fatal(err)
	}
}

func routeMaskAll(t *testing.T, s *Store) map[string]sql.NullInt64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT hash, route_mask FROM transmissions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]sql.NullInt64{}
	for rows.Next() {
		var h string
		var m sql.NullInt64
		rows.Scan(&h, &m)
		out[h] = m
	}
	return out
}

func routeMaskInsertRaw(t *testing.T, s *Store, hash string, routeType interface{}, obsRaw ...interface{}) {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type) VALUES ('00', ?, ?, ?, 4)`, hash, routeMaskT0, routeType)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for i, raw := range obsRaw {
		if _, err := s.db.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex) VALUES (?, ?, ?, 1790244000, ?)`,
			id, i+100, fmt.Sprintf(`["%02X"]`, i), raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackfillTxRouteMask_LowerBoundFromRouteAndObservations(t *testing.T) {
	routeMaskShrinkBackfill(t, 2)
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "bf.db"))
	defer s.Close()
	mixed := routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0})
	routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT5})
	routeMaskMakeLegacy(t, s)

	// Staging tx 1000443: stored as zero-hop, the only surviving frame is the
	// 0-hop transport flood that overwrote it (same observer, same path).
	overwritten := "hash-overwritten"
	routeMaskInsertRaw(t, s, overwritten, 3, routeMaskFlood0HopRaw)
	// A -> B -> A by one observer and path: the flood variant is gone from
	// every frame. The backfill must not invent it.
	lost := "hash-lost"
	routeMaskInsertRaw(t, s, lost, 3, firstIngestedZeroHopRaw)
	routeMaskInsertRaw(t, s, "hash-null-route", nil, firstIngestedFloodRaw)
	routeMaskInsertRaw(t, s, "hash-invalid-no-obs", 7)
	routeMaskInsertRaw(t, s, "hash-bad-frames", 1, "zz", "1", nil)
	routeMaskInsertRaw(t, s, "hash-known", 1)
	if _, err := s.db.Exec(`UPDATE transmissions SET route_mask = 4 WHERE hash = 'hash-known'`); err != nil {
		t.Fatal(err)
	}

	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	got := routeMaskAll(t, s)
	want := map[string]int64{
		mixed:                 0b1010,
		overwritten:           0b1001,
		lost:                  0b1000,
		"hash-null-route":     0b0010,
		"hash-invalid-no-obs": 0,
		"hash-bad-frames":     0b0010,
		"hash-known":          0b0100, // already known rows are not touched
	}
	for h, w := range want {
		if m := got[h]; !m.Valid || m.Int64 != w {
			t.Errorf("%s: route_mask = %v, want %04b", h, m, w)
		}
	}
	var idx, left int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, dbschema.RouteMaskPendingIndex).Scan(&idx)
	s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_mask IS NULL`).Scan(&left)
	if idx != 1 || left != 0 {
		t.Fatalf("pending index present=%d, NULL rows left=%d; want 1 and 0", idx, left)
	}
}

func routeMaskLegacyFixture(t *testing.T, path string, n int) *Store {
	t.Helper()
	s := routeMaskStore(t, path)
	for i := 0; i < n; i++ {
		payload := fmt.Sprintf("%064x", i) + firstIngestedAdvertPayload[64:]
		routeMaskInsert(t, s, routeMaskObs{"11" + "02" + "a1b2" + payload, "obs-a", routeMaskT0})
		if i%3 == 0 {
			routeMaskInsert(t, s, routeMaskObs{"13" + "00000000" + "00" + payload, "obs-b", routeMaskT5})
		}
	}
	routeMaskMakeLegacy(t, s)
	return s
}

// Interrupted after two committed batches, the ingestor restarts and resumes;
// the result equals an uninterrupted run, and a second run changes nothing.
func TestBackfillTxRouteMask_ChunkedResumableIdempotent(t *testing.T) {
	routeMaskShrinkBackfill(t, 2)
	ref := routeMaskLegacyFixture(t, filepath.Join(t.TempDir(), "ref.db"), 9)
	if err := ref.backfillTxRouteMask(context.Background(), ref.db); err != nil {
		t.Fatal(err)
	}
	want := routeMaskAll(t, ref)
	ref.Close()

	path := filepath.Join(t.TempDir(), "resume.db")
	s := routeMaskLegacyFixture(t, path, 9)
	if _, err := s.db.Exec(dbschema.CreateRouteMaskPendingIndexSQL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if n, err := s.backfillTxRouteMaskBatch(context.Background(), s.db, routeMaskBackfillBatchSize); err != nil || n != 2 {
			t.Fatalf("batch %d: n=%d err=%v", i, n, err)
		}
	}
	var left int
	s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_mask IS NULL`).Scan(&left)
	if left != 5 {
		t.Fatalf("after two batches of 2, NULL rows left = %d, want 5", left)
	}
	s.Close()
	s = routeMaskStore(t, path) // restart
	defer s.Close()
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
	if got := routeMaskAll(t, s); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("resumed result differs from an uninterrupted run:\n got  %v\n want %v", got, want)
	}
	if n, err := s.backfillTxRouteMaskBatch(context.Background(), s.db, routeMaskBackfillBatchSize); err != nil || n != 0 {
		t.Fatalf("second run updated %d rows (err=%v), want 0", n, err)
	}
}

// A batch that is cancelled before it commits leaves no partial update.
func TestBackfillTxRouteMask_CancelledBatchCommitsNothing(t *testing.T) {
	s := routeMaskLegacyFixture(t, filepath.Join(t.TempDir(), "cancel.db"), 4)
	defer s.Close()
	if _, err := s.db.Exec(dbschema.CreateRouteMaskPendingIndexSQL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.backfillTxRouteMaskBatch(ctx, s.db, 100); err == nil {
		t.Fatal("cancelled batch returned no error")
	}
	if err := s.backfillTxRouteMask(ctx, s.db); err == nil {
		t.Fatal("cancelled backfill returned no error")
	}
	var left int
	s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_mask IS NULL`).Scan(&left)
	if left != 4 {
		t.Fatalf("NULL rows after a cancelled batch = %d, want all 4", left)
	}
}

// Live ingest racing the backfill (batch size 1, no yield) must never lose a
// bit: each payload gets its zero-hop variant while its flood row is being
// backfilled.
func TestBackfillTxRouteMask_ConcurrentLiveIngestLosesNoBits(t *testing.T) {
	routeMaskShrinkBackfill(t, 1)
	const n = 60
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "live.db"))
	defer s.Close()
	for i := 0; i < n; i++ {
		payload := fmt.Sprintf("%064x", i) + firstIngestedAdvertPayload[64:]
		routeMaskInsert(t, s, routeMaskObs{"11" + "02" + "a1b2" + payload, "obs-a", routeMaskT0})
	}
	routeMaskMakeLegacy(t, s)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		for i := n - 1; i >= 0; i-- {
			payload := fmt.Sprintf("%064x", i) + firstIngestedAdvertPayload[64:]
			routeMaskInsert(t, s, routeMaskObs{"13" + "00000000" + "00" + payload, "obs-b", routeMaskT5})
		}
	}()
	wg.Wait()
	// Rows the backfill finished before their zero-hop observation arrived got
	// the bit from the live OR; the others from the backfill's frame read.
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
	var wrong int
	s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_mask IS NOT 10`).Scan(&wrong)
	if wrong != 0 {
		t.Fatalf("%d of %d transmissions lost a route bit while live ingest raced the backfill", wrong, n)
	}
}

func routeMaskAsyncStatus(t *testing.T, s *Store) string {
	t.Helper()
	var st string
	if err := s.db.QueryRow(`SELECT status FROM _async_migrations WHERE name = ?`, dbschema.RouteMaskBackfillMigration).Scan(&st); err != nil {
		return "missing"
	}
	return st
}

func TestStartRouteMaskBackfill_CompletesAndRerunsForNewNullRows(t *testing.T) {
	s := routeMaskLegacyFixture(t, filepath.Join(t.TempDir(), "start.db"), 5)
	defer s.Close()
	s.StartRouteMaskBackfill(context.Background())
	s.WaitForAsyncMigrations()
	if st := routeMaskAsyncStatus(t, s); st != "done" {
		t.Fatalf("status after first run = %s, want done", st)
	}

	// An older ingestor (rollback) inserted a row without a mask; the next
	// start must backfill it even though the migration is marked done.
	routeMaskInsertRaw(t, s, "hash-after-rollback", 2, firstIngestedFloodRaw)
	s.StartRouteMaskBackfill(context.Background())
	s.WaitForAsyncMigrations()
	if m := routeMaskAll(t, s)["hash-after-rollback"]; !m.Valid || m.Int64 != 0b0110 {
		t.Fatalf("row inserted after completion: route_mask = %v, want 0110", m)
	}
	if st := routeMaskAsyncStatus(t, s); st != "done" {
		t.Fatalf("status after re-run = %s, want done", st)
	}

	// Complete and nothing left: a start is a no-op (no re-run recorded).
	var before string
	s.db.QueryRow(`SELECT IFNULL(started_at, '') FROM _async_migrations WHERE name = ?`, dbschema.RouteMaskBackfillMigration).Scan(&before)
	s.db.Exec(`UPDATE _async_migrations SET started_at = 'sentinel' WHERE name = ?`, dbschema.RouteMaskBackfillMigration)
	s.StartRouteMaskBackfill(context.Background())
	s.WaitForAsyncMigrations()
	var after string
	s.db.QueryRow(`SELECT started_at FROM _async_migrations WHERE name = ?`, dbschema.RouteMaskBackfillMigration).Scan(&after)
	if after != "sentinel" {
		t.Fatalf("complete backfill was restarted (started_at %q -> %q)", before, after)
	}
}

// Shutdown cancels a running backfill; it is recorded as failed and resumes
// on the next start.
func TestStartRouteMaskBackfill_CancelledRunResumesNextStart(t *testing.T) {
	s := routeMaskLegacyFixture(t, filepath.Join(t.TempDir(), "shutdown.db"), 5)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.StartRouteMaskBackfill(ctx)
	s.WaitForAsyncMigrations()
	if st := routeMaskAsyncStatus(t, s); st != "failed" {
		t.Fatalf("status after cancelled run = %s, want failed", st)
	}
	s.StartRouteMaskBackfill(context.Background())
	s.WaitForAsyncMigrations()
	var left int
	s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_mask IS NULL`).Scan(&left)
	if st := routeMaskAsyncStatus(t, s); st != "done" || left != 0 {
		t.Fatalf("after resume: status=%s NULL rows=%d, want done and 0", st, left)
	}
}

// The backfill must start only once the startup buffer is draining (so it
// neither delays Ready() via WaitForAsyncMigrations nor builds its index
// before MQTT subscribe), and shutdown must cancel it before disconnecting.
func TestMain_StartsRouteMaskBackfillAfterBufferReady(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	subscribe := strings.Index(s, "c.Subscribe(")
	ready := strings.Index(s, "ingestBuffer.Ready()")
	start := strings.Index(s, "store.StartRouteMaskBackfill(")
	shutdown := strings.Index(s, `log.Println("Shutting down...")`)
	stop := strings.LastIndex(s, "stopRouteMaskBackfill()")
	disconnect := strings.LastIndex(s, "c.Disconnect(5000)")
	if subscribe < 0 || ready < 0 || start < 0 || shutdown < 0 || stop < 0 || disconnect < 0 {
		t.Fatalf("markers not found: subscribe=%d ready=%d start=%d shutdown=%d stop=%d disconnect=%d", subscribe, ready, start, shutdown, stop, disconnect)
	}
	if !(subscribe < ready && ready < start) {
		t.Errorf("StartRouteMaskBackfill must come after MQTT subscribe and ingestBuffer.Ready()")
	}
	if !(shutdown < stop && stop < disconnect) {
		t.Errorf("shutdown must cancel the route_mask backfill before disconnecting MQTT clients")
	}
}

// The pending-index build is one long write (about 18 s cold on the staging
// copy). It holds writerMu and is recorded under its own component, so the
// stall it causes is not attributed to the MQTT handler waiting behind it.
func TestStartRouteMaskBackfill_IndexBuildIsAttributed(t *testing.T) {
	s := routeMaskLegacyFixture(t, filepath.Join(t.TempDir(), "attr.db"), 3)
	defer s.Close()
	ResetWriterStatsForTest()
	s.StartRouteMaskBackfill(context.Background())
	s.WaitForAsyncMigrations()
	snap := s.WriterStatsSnapshot()
	if got := snap["route_mask_index"].Count; got != 1 {
		t.Fatalf("route_mask_index writer samples = %d, want 1 (snapshot %v)", got, snap)
	}
	if got := snap["route_mask_backfill"].Count; got < 1 {
		t.Fatalf("route_mask_backfill writer samples = %d, want at least 1", got)
	}
}
