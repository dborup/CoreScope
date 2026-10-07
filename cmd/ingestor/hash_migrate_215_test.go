package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Issue #215: the content-hash migration (rehash with the current formula,
// merge rows that collide, delete the duplicate) is a DB write and belongs to
// the ingestor. The server used to run it on its read-only handle, where every
// statement failed and was retried at every start.

func hm215Raw(key int) string {
	return fmt.Sprintf("0A00D69FD7A5A7475DB07337749AE61FA53A4788E9%02X", key)
}

// hm215Prepare opens the DB at path, drops the migration's record, lets fn seed
// it, and reopens it WITHOUT starting the migration. The record is dropped
// because it is marked done on the first open: the seeded DB then looks like one
// written by an ingestor from before the migration existed.
func hm215Prepare(t *testing.T, path string, fn func(db *sql.DB)) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.WaitForAsyncMigrations()
	if _, err := s.db.Exec(`DELETE FROM _async_migrations WHERE name LIKE '%content_hash%'`); err != nil {
		t.Fatal(err)
	}
	fn(s.db)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// hm215Reopen is hm215Prepare followed by running the migration to the end.
func hm215Reopen(t *testing.T, path string, fn func(db *sql.DB)) *Store {
	t.Helper()
	s := hm215Prepare(t, path, fn)
	s.StartContentHashMigration(context.Background())
	s.WaitForAsyncMigrations()
	return s
}

func hm215Exec(t *testing.T, db *sql.DB, q string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

func hm215Tx(t *testing.T, db *sql.DB, id int, raw, hash string, lastSeen, mask int) {
	t.Helper()
	hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, last_seen, route_mask)
		VALUES (?, ?, ?, ?, 1, 4, '{}', ?, ?)`, id, raw, hash, fmt.Sprintf("2026-01-01T00:00:%02dZ", id%60), lastSeen, mask)
}

func hm215Obs(t *testing.T, db *sql.DB, txID, observer int, path string) {
	t.Helper()
	hm215Exec(t, db, `INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp)
		VALUES (?, ?, 'RX', -5.0, -90.0, 5, ?, 1767225600)`, txID, observer, path)
}

func hm215Count(t *testing.T, db *sql.DB, q string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	return n
}

// hm215Seed writes the rows every test below migrates:
//
//	A: ids 11,12,13 share raw A. Rows overlap in observations (12 repeats an
//	   observation of 11, 13 one of 12), which the observations dedup index
//	   would reject when re-parented.
//	B: id 20 is stale, id 25 already carries the current hash of the same raw
//	   packet. The lowest id (20) must survive although 25 holds the hash.
//	C: id 30 is stale and has no duplicate.
//	D: id 40 is current and must not be touched.
func hm215Seed(t *testing.T, db *sql.DB) {
	t.Helper()
	for i := 1; i <= 3; i++ {
		hm215Exec(t, db, `INSERT OR IGNORE INTO observers (id, name, first_seen, last_seen) VALUES (?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, fmt.Sprintf("o%d", i), fmt.Sprintf("O%d", i))
	}
	rowid := func(id string) int {
		return hm215Count(t, db, `SELECT rowid FROM observers WHERE id = ?`, id)
	}
	o1, o2, o3 := rowid("o1"), rowid("o2"), rowid("o3")

	hm215Tx(t, db, 11, hm215Raw(1), "stale-a-1", 100, 1)
	hm215Tx(t, db, 12, hm215Raw(1), "stale-a-2", 300, 2)
	hm215Tx(t, db, 13, hm215Raw(1), "stale-a-3", 200, 4)
	hm215Obs(t, db, 11, o1, `["aa"]`)
	hm215Obs(t, db, 11, o1, `["bb"]`)
	hm215Obs(t, db, 12, o1, `["aa"]`) // same as 11's: rejected by the dedup index once re-parented
	hm215Obs(t, db, 12, o2, `["cc"]`)
	hm215Obs(t, db, 13, o2, `["cc"]`) // same as 12's
	hm215Obs(t, db, 13, o2, `["dd"]`)
	hm215Exec(t, db, `INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen) VALUES (12, 'stale-a-2', 'c', 's', '2026-01-01T00:00:12Z')`)
	hm215Exec(t, db, `INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen) VALUES (13, 'stale-a-3', 'c', 's', '2026-01-01T00:00:13Z')`)
	hm215Exec(t, db, `INSERT INTO route_mask_changes (transmission_id, route_mask, created_at) VALUES (13, 4, 1)`)

	hm215Tx(t, db, 20, hm215Raw(2), "stale-b-20", 50, 1)
	hm215Tx(t, db, 25, hm215Raw(2), ComputeContentHash(hm215Raw(2)), 60, 2)
	hm215Obs(t, db, 20, o1, `["ee"]`)
	hm215Obs(t, db, 25, o3, `["ff"]`)
	hm215Obs(t, db, 25, o1, `["ee"]`) // same as 20's

	hm215Tx(t, db, 30, hm215Raw(3), "stale-c-30", 10, 1)
	hm215Obs(t, db, 30, o1, `["01"]`)

	hm215Tx(t, db, 40, hm215Raw(4), ComputeContentHash(hm215Raw(4)), 10, 1)
	hm215Obs(t, db, 40, o2, `["02"]`)
}

func hm215Open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hm215.db")
	return hm215Reopen(t, path, func(db *sql.DB) { hm215Seed(t, db) }), path
}

func TestContentHashMigration_ConvergesInTheIngestor_215(t *testing.T) {
	for _, batch := range []int{1, 2, 3, 1000} {
		t.Run(fmt.Sprintf("batch%d", batch), func(t *testing.T) {
			oldBatch, oldYield := contentHashMigrationBatchSize, contentHashMigrationYield
			contentHashMigrationBatchSize, contentHashMigrationYield = batch, 0
			t.Cleanup(func() { contentHashMigrationBatchSize, contentHashMigrationYield = oldBatch, oldYield })

			s, _ := hm215Open(t)
			db := s.db

			// One row per content hash, none of them stale.
			rows, err := db.Query(`SELECT id, raw_hex, hash FROM transmissions ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var ids []int
			seen := map[string]int{}
			for rows.Next() {
				var id int
				var raw, hash string
				if err := rows.Scan(&id, &raw, &hash); err != nil {
					t.Fatal(err)
				}
				if want := ComputeContentHash(raw); hash != want {
					t.Errorf("tx %d hash = %s, want %s", id, hash, want)
				}
				seen[hash]++
				ids = append(ids, id)
			}
			rows.Close()
			// A survives as its lowest id, B as 20 (not 25, the current holder).
			if got, want := fmt.Sprint(ids), "[11 20 30 40]"; got != want {
				t.Fatalf("surviving transmissions = %s, want %s", got, want)
			}
			for h, n := range seen {
				if n != 1 {
					t.Errorf("hash %s is held by %d rows", h, n)
				}
			}

			// Every observation survives once: A has 4 distinct of 6, B 2 of 3.
			for id, want := range map[int]int{11: 4, 20: 2, 30: 1, 40: 1} {
				if got := hm215Count(t, db, `SELECT COUNT(*) FROM observations WHERE transmission_id = ?`, id); got != want {
					t.Errorf("tx %d holds %d observations, want %d", id, got, want)
				}
			}
			if got := hm215Count(t, db, `SELECT COUNT(*) FROM observations WHERE transmission_id NOT IN (SELECT id FROM transmissions)`); got != 0 {
				t.Errorf("%d observations point at a transmission that no longer exists", got)
			}
			if got := hm215Count(t, db, `SELECT COUNT(*) FROM pragma_foreign_key_check`); got != 0 {
				t.Errorf("foreign_key_check reports %d violations", got)
			}

			// The survivor carries what the duplicates knew: the latest
			// last_seen and every route type seen. first_seen stays its own.
			if got := hm215Count(t, db, `SELECT last_seen FROM transmissions WHERE id = 11`); got != 300 {
				t.Errorf("survivor last_seen = %d, want the latest of the duplicates (300)", got)
			}
			if got := hm215Count(t, db, `SELECT route_mask FROM transmissions WHERE id = 11`); got != 7 {
				t.Errorf("survivor route_mask = %d, want the union 7", got)
			}
			if got := hm215Count(t, db, `SELECT route_mask FROM transmissions WHERE id = 20`); got != 3 {
				t.Errorf("tx 20 route_mask = %d, want the union with the deleted row 25 (3)", got)
			}
			var firstSeen string
			if err := db.QueryRow(`SELECT first_seen FROM transmissions WHERE id = 11`).Scan(&firstSeen); err != nil || firstSeen != "2026-01-01T00:00:11Z" {
				t.Errorf("survivor first_seen = %q (%v), want its own", firstSeen, err)
			}

			// Rows hung off the transmission follow the survivor.
			if got := hm215Count(t, db, `SELECT COUNT(*) FROM ping_triggers`); got != 1 {
				t.Errorf("ping_triggers holds %d rows, want one for the merged ping", got)
			}
			var pingTx int
			var pingHash string
			if err := db.QueryRow(`SELECT tx_id, hash FROM ping_triggers`).Scan(&pingTx, &pingHash); err != nil {
				t.Fatal(err)
			}
			if pingTx != 11 || pingHash != ComputeContentHash(hm215Raw(1)) {
				t.Errorf("ping trigger = tx %d hash %s, want tx 11 with the new hash", pingTx, pingHash)
			}
			if got := hm215Count(t, db, `SELECT COUNT(*) FROM route_mask_changes WHERE transmission_id NOT IN (SELECT id FROM transmissions)`); got != 0 {
				t.Errorf("%d route_mask_changes rows name a deleted transmission", got)
			}
			// A running server that holds the survivor learns the grown mask.
			if got := hm215Count(t, db, `SELECT COALESCE(MAX(route_mask), 0) FROM route_mask_changes WHERE transmission_id = 11`); got != 7 {
				t.Errorf("route_mask_changes announces mask %d for the survivor, want 7", got)
			}
		})
	}
}

// The migration runs once. The next start must not scan the table again, and
// a database that has converged is left byte for byte as it is.
func TestContentHashMigration_RunsOnce_215(t *testing.T) {
	s, path := hm215Open(t)
	var status string
	if err := s.db.QueryRow(`SELECT status FROM _async_migrations WHERE name LIKE '%content_hash%'`).Scan(&status); err != nil {
		t.Fatalf("migration is not recorded: %v", err)
	}
	if status != "done" {
		t.Fatalf("migration status = %q, want done", status)
	}
	before := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A stale row appearing after convergence is not touched on the next
	// start: the ingestor writes the current formula, so none can appear, and
	// the migration does not pay a table scan on every boot to look.
	s2 := hm215Reopen(t, path, func(db *sql.DB) {
		hm215Exec(t, db, `INSERT INTO _async_migrations (name, status) VALUES ('content_hash_formula_v1', 'done') ON CONFLICT(name) DO UPDATE SET status='done'`)
		hm215Tx(t, db, 90, hm215Raw(9), "stale-after", 1, 1)
	})
	_ = s2
	if hm215Count(t, s2.db, `SELECT COUNT(*) FROM transmissions WHERE hash = 'stale-after'`) != 1 {
		t.Error("a completed migration ran again at the next start")
	}
	if got := hm215Count(t, s2.db, `SELECT COUNT(*) FROM transmissions`); got != before+1 {
		t.Errorf("transmissions = %d, want %d", got, before+1)
	}
}

// New rows are written with the current formula, so the migration is only
// needed for rows older than the formula (#786/#787), never for new ones.
func TestInsertWritesTheCurrentContentHash_215(t *testing.T) {
	s := newTestStore(t)
	raw := hm215Raw(7)
	pd := &PacketData{RawHex: raw, Hash: ComputeContentHash(raw), Timestamp: time.Now().UTC().Format(time.RFC3339), PayloadType: 4, RouteType: 1, DecodedJSON: "{}", PathJSON: "[]"}
	if _, err := s.InsertTransmission(pd); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := s.db.QueryRow(`SELECT hash FROM transmissions WHERE raw_hex = ?`, raw).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if want := ComputeContentHash(raw); hash != want || strings.HasPrefix(hash, "stale") {
		t.Fatalf("inserted hash = %s, want %s", hash, want)
	}
}

// hm215Dump is the migrated state in a comparable form: the rows and their
// foreign rows, without the autoincrement ids and clock values that differ
// between two runs of the same data.
func hm215Dump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	dump := func(title, q string) {
		b.WriteString("== " + title + "\n")
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, vals...)
		}
	}
	dump("transmissions", `SELECT id, raw_hex, hash, first_seen, last_seen, COALESCE(route_mask, -1), COALESCE(scope_name, ''), COALESCE(channel_hash, ''), COALESCE(from_pubkey, '') FROM transmissions ORDER BY id`)
	dump("observations", `SELECT transmission_id, observer_idx, COALESCE(path_json, ''), timestamp, COALESCE(snr, 0) FROM observations ORDER BY transmission_id, observer_idx, COALESCE(path_json, ''), timestamp`)
	dump("ping_triggers", `SELECT tx_id, hash FROM ping_triggers ORDER BY tx_id`)
	dump("route_mask_changes", `SELECT transmission_id, route_mask FROM route_mask_changes ORDER BY transmission_id, route_mask`)
	return b.String()
}

func hm215Status(t *testing.T, s *Store) string {
	t.Helper()
	var status string
	if err := s.db.QueryRow(`SELECT status FROM _async_migrations WHERE name = ?`, contentHashMigration).Scan(&status); err != nil {
		t.Fatalf("migration is not recorded: %v", err)
	}
	return status
}

// A run that is stopped half way (a deploy restart) must not be recorded as
// done, and the restarted run must end in exactly the state an uninterrupted run
// ends in. The migration deletes rows, so a stop that was recorded as done would
// leave the rest unconverted for good.
func TestContentHashMigration_InterruptedRunResumesToTheSameState_215(t *testing.T) {
	oldBatch, oldYield := contentHashMigrationBatchSize, contentHashMigrationYield
	contentHashMigrationBatchSize, contentHashMigrationYield = 1, 0
	t.Cleanup(func() { contentHashMigrationBatchSize, contentHashMigrationYield = oldBatch, oldYield })

	ref, _ := hm215Open(t)
	want := hm215Dump(t, ref.db)

	path := filepath.Join(t.TempDir(), "interrupted.db")
	s := hm215Prepare(t, path, func(db *sql.DB) { hm215Seed(t, db) })
	seeded := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE hash LIKE 'stale-%'`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Stop after the fourth row was scanned: rows 11, 12, 13 and 20 are done,
	// 30 is still stale.
	contentHashMigrationHook = func(stage string, n int) {
		if stage == "batch" && n == 4 {
			cancel()
		}
	}
	t.Cleanup(func() { contentHashMigrationHook = nil })
	s.StartContentHashMigration(ctx)
	s.WaitForAsyncMigrations()
	contentHashMigrationHook = nil

	if got := hm215Status(t, s); got == "done" {
		t.Fatal("a run that was cancelled half way is recorded as done: the rest is never converted")
	}
	left := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE hash LIKE 'stale-%'`)
	if left == 0 || left >= seeded {
		t.Fatalf("setup: %d of %d stale rows left, the cancel did not land half way", left, seeded)
	}
	if got := hm215Count(t, s.db, `SELECT COUNT(*) FROM pragma_foreign_key_check`); got != 0 {
		t.Errorf("foreign_key_check reports %d violations at the cancel point", got)
	}
	if got := hm215Count(t, s.db, `SELECT COUNT(*) FROM observations WHERE transmission_id NOT IN (SELECT id FROM transmissions)`); got != 0 {
		t.Errorf("%d orphan observations at the cancel point", got)
	}

	s.StartContentHashMigration(context.Background())
	s.WaitForAsyncMigrations()
	if got := hm215Status(t, s); got != "done" {
		t.Fatalf("restarted run status = %q, want done", got)
	}
	if got := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE hash LIKE 'stale-%'`); got != 0 {
		t.Errorf("%d stale rows left after the restart", got)
	}
	if got := hm215Dump(t, s.db); got != want {
		t.Errorf("the restarted run ends in a different state than an uninterrupted one:\n got:\n%s\n want:\n%s", got, want)
	}
}

// A row the scan saw can be gone (retention) or changed by the time its batch
// runs, because the scan is outside writerMu. Neither may fail the migration.
func TestContentHashMigration_RowsChangedSinceTheScanAreSkipped_215(t *testing.T) {
	oldBatch, oldYield := contentHashMigrationBatchSize, contentHashMigrationYield
	contentHashMigrationBatchSize, contentHashMigrationYield = 1000, 0
	t.Cleanup(func() { contentHashMigrationBatchSize, contentHashMigrationYield = oldBatch, oldYield })

	path := filepath.Join(t.TempDir(), "race.db")
	s := hm215Prepare(t, path, func(db *sql.DB) { hm215Seed(t, db) })
	fired := false
	contentHashMigrationHook = func(stage string, n int) {
		if stage != "scanned" || fired {
			return
		}
		fired = true
		// Retention removes row 30 and something rewrites the hash of row 11
		// after the scan and before the batch.
		hm215Exec(t, s.db, `DELETE FROM observations WHERE transmission_id = 30`)
		hm215Exec(t, s.db, `DELETE FROM transmissions WHERE id = 30`)
		hm215Exec(t, s.db, `UPDATE transmissions SET hash = 'changed-11' WHERE id = 11`)
	}
	t.Cleanup(func() { contentHashMigrationHook = nil })
	s.StartContentHashMigration(context.Background())
	s.WaitForAsyncMigrations()
	contentHashMigrationHook = nil

	if got := hm215Status(t, s); got != "done" {
		t.Fatalf("status = %q: a row that changed since the scan failed the migration", got)
	}
	if got := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE id = 30`); got != 0 {
		t.Errorf("deleted row 30 is back")
	}
	var hash string
	if err := s.db.QueryRow(`SELECT hash FROM transmissions WHERE id = 11`).Scan(&hash); err != nil || hash != "changed-11" {
		t.Errorf("row 11 hash = %q (%v): a row whose hash changed since the scan must be left for the next run, not rewritten from stale data", hash, err)
	}
	// The next run picks the skipped row up.
	if err := s.migrateContentHashes(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
	if got := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE hash NOT LIKE '________________'`); got != 0 {
		t.Errorf("%d rows still carry a non-current hash after the next run", got)
	}
}

// A stop at shutdown is not a failure: it resumes at the next start, and the log
// must not read like one during a deploy restart. A real failure still does.
func TestAsyncMigration_CancelIsLoggedAsResumable_215(t *testing.T) {
	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RunAsyncMigration(ctx, "cancel_probe_v1", func(ctx context.Context, d *sql.DB) error { return ctx.Err() }); err != nil {
		t.Fatal(err)
	}
	if err := s.RunAsyncMigration(context.Background(), "failure_probe_v1", func(ctx context.Context, d *sql.DB) error { return fmt.Errorf("boom") }); err != nil {
		t.Fatal(err)
	}
	s.WaitForAsyncMigrations()
	out := buf.String()
	if !strings.Contains(out, `"cancel_probe_v1" cancelled (will resume)`) {
		t.Errorf("a cancelled migration is not logged as resumable:\n%s", out)
	}
	if strings.Contains(out, `"cancel_probe_v1" FAILED`) {
		t.Errorf("a cancelled migration is logged as FAILED:\n%s", out)
	}
	if !strings.Contains(out, `"failure_probe_v1" FAILED: boom`) {
		t.Errorf("a real failure is no longer logged as FAILED:\n%s", out)
	}
	if got, _ := s.AsyncMigrationStatus("cancel_probe_v1"); got == "done" {
		t.Error("a cancelled migration is recorded as done")
	}
}

// Rows that merge keep what the duplicate knew: the earliest first_seen and
// every column the survivor has no value for. The survivor's own non-null values
// stay. A dropped observation (same observer and path) keeps the survivor's
// copy, as ingest does for a repeat reception.
func TestContentHashMigration_MergeKeepsEarliestFirstSeenAndFillsNulls_215(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rpi3.db"), func(db *sql.DB) {
		hm215Exec(t, db, `INSERT OR IGNORE INTO observers (id, name, first_seen, last_seen) VALUES ('o1', 'O1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		o1 := hm215Count(t, db, `SELECT rowid FROM observers WHERE id = 'o1'`)
		ins := func(id int, raw, hash, first string, scope, channel, from interface{}) {
			hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, last_seen, route_mask, scope_name, channel_hash, from_pubkey)
				VALUES (?, ?, ?, ?, 1, 4, '{}', 1, 1, ?, ?, ?)`, id, raw, hash, first, scope, channel, from)
		}
		// Pair 1: the survivor (lowest id, later first_seen) has nothing set.
		ins(50, hm215Raw(5), "stale-r-50", "2026-01-02T00:00:00Z", nil, nil, nil)
		ins(51, hm215Raw(5), "stale-r-51", "2026-01-01T00:00:00Z", "#x", "ch", "pk")
		// Pair 2: the survivor has its own values; the duplicate's differ.
		ins(60, hm215Raw(6), "stale-r-60", "2026-01-01T00:00:00Z", "#keep", "keepch", "keeppk")
		ins(61, hm215Raw(6), "stale-r-61", "2026-01-02T00:00:00Z", "#other", "otherch", "otherpk")
		// The same observation in both rows of pair 1: the survivor's copy
		// (SNR -10, later) stays although the duplicate's (SNR 5) is earlier.
		hm215Exec(t, db, `INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, path_json, timestamp) VALUES (50, ?, 'RX', -10.0, -100, '["aa"]', 1767312000)`, o1)
		hm215Exec(t, db, `INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, path_json, timestamp) VALUES (51, ?, 'RX', 5.0, -60, '["aa"]', 1767225600)`, o1)
	})
	type row struct{ first, scope, channel, from string }
	get := func(id int) row {
		var r row
		if err := s.db.QueryRow(`SELECT first_seen, COALESCE(scope_name, ''), COALESCE(channel_hash, ''), COALESCE(from_pubkey, '') FROM transmissions WHERE id = ?`, id).Scan(&r.first, &r.scope, &r.channel, &r.from); err != nil {
			t.Fatalf("tx %d: %v", id, err)
		}
		return r
	}
	if got, want := get(50), (row{"2026-01-01T00:00:00Z", "#x", "ch", "pk"}); got != want {
		t.Errorf("survivor 50 = %+v, want the earliest first_seen and the duplicate's values for what it lacked: %+v", got, want)
	}
	if got, want := get(60), (row{"2026-01-01T00:00:00Z", "#keep", "keepch", "keeppk"}); got != want {
		t.Errorf("survivor 60 = %+v, want its own non-null values kept: %+v", got, want)
	}
	for _, id := range []int{51, 61} {
		if hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE id = ?`, id) != 0 {
			t.Errorf("duplicate %d was not deleted", id)
		}
	}
	var snr float64
	if err := s.db.QueryRow(`SELECT snr FROM observations WHERE transmission_id = 50`).Scan(&snr); err != nil || snr != -10.0 {
		t.Errorf("the surviving observation has SNR %v (%v), want the survivor's own -10 (documented rule)", snr, err)
	}
}

// Perf check, not a pass/fail test: with CORESCOPE_PERF_215=<runs> it times
// migrateContentHashes over 2000 stale rows in 1000 colliding pairs (the shape
// of the server's TestPerf_HashMigrateMerge_202) and over 5000 stale rows that
// do not collide (TestPerf_HashMigrateRehash_215), printing one RESULT line each.
func TestPerf_ContentHashMigration_215(t *testing.T) {
	runs, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_215"))
	if runs <= 0 {
		t.Skip("set CORESCOPE_PERF_215=<runs> to measure")
	}
	measure := func(name string, rows int, paired bool) {
		var times []time.Duration
		for r := 0; r < runs; r++ {
			s := newTestStore(t)
			db := s.db
			hm215Exec(t, db, `INSERT OR IGNORE INTO observers (id, name, first_seen, last_seen) VALUES ('o1', 'O1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
			o1 := hm215Count(t, db, `SELECT rowid FROM observers WHERE id = 'o1'`)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			for id := 1; id <= rows; id++ {
				key := id
				if paired {
					key = (id + 1) / 2
				}
				if _, err := tx.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, last_seen, route_mask)
					VALUES (?, ?, ?, '2026-01-01T00:00:00Z', 1, 4, '{}', ?, 1)`, id, fmt.Sprintf("0A00D69FD7A5A7475DB07337749AE61FA53A4788%04X", key), fmt.Sprintf("old-%d", id), id); err != nil {
					t.Fatal(err)
				}
				for k := 0; k < 2; k++ {
					if _, err := tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, direction, path_json, timestamp) VALUES (?, ?, 'RX', ?, 1767225600)`,
						id, o1, fmt.Sprintf(`["%02X","%02X"]`, id%256, k)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			oldBatch, oldYield := contentHashMigrationBatchSize, contentHashMigrationYield
			contentHashMigrationBatchSize, contentHashMigrationYield = 500, 0
			t0 := time.Now()
			if err := s.migrateContentHashes(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			times = append(times, time.Since(t0))
			contentHashMigrationBatchSize, contentHashMigrationYield = oldBatch, oldYield
			if got := hm215Count(t, db, `SELECT COUNT(*) FROM transmissions`); paired && got != rows/2 || !paired && got != rows {
				t.Fatalf("%s: %d transmissions after the migration", name, got)
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		fmt.Printf("RESULT %s median_ns=%d\n", name, times[len(times)/2].Nanoseconds())
	}
	measure("ingestor-hash-migrate-merge-2000", 2000, true)
	measure("ingestor-hash-migrate-rehash-5000", 5000, false)
}

// OpenStore does not start the migration: callers that seed their own rows
// (tests, tools) would have them rehashed under them. main starts it, once the
// ingest buffer is draining, and cancels it on shutdown, like the route_mask
// backfill.
func TestContentHashMigration_StartsAfterBufferReady_215(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitForAsyncMigrations()
	if hm215Count(t, s.db, `SELECT COUNT(*) FROM _async_migrations WHERE name LIKE '%content_hash%'`) != 0 {
		t.Error("OpenStore scheduled the content-hash migration")
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	m := string(src)
	ready := strings.Index(m, "ingestBuffer.Ready()")
	start := strings.Index(m, "store.StartContentHashMigration(")
	shutdown := strings.Index(m, `log.Println("Shutting down...")`)
	stop := strings.LastIndex(m, "stopContentHashMigration()")
	disconnect := strings.LastIndex(m, "c.Disconnect(5000)")
	if ready < 0 || start < 0 || shutdown < 0 || stop < 0 || disconnect < 0 {
		t.Fatalf("markers not found: ready=%d start=%d shutdown=%d stop=%d disconnect=%d", ready, start, shutdown, stop, disconnect)
	}
	if !(ready < start) {
		t.Errorf("StartContentHashMigration must come after ingestBuffer.Ready()")
	}
	if !(shutdown < stop && stop < disconnect) {
		t.Errorf("shutdown must cancel the content-hash migration before disconnecting MQTT clients")
	}
}
