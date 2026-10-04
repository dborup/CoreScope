package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
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

// hm215Reopen opens the DB at path, lets fn seed it, and reopens it so the
// migration, scheduled by OpenStore, runs over what fn wrote. The migration is
// recorded as done on the first open, so its record is dropped before fn runs:
// the seeded DB then looks like one written by an ingestor from before the
// migration existed.
func hm215Reopen(t *testing.T, path string, fn func(db *sql.DB)) *Store {
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
	s.WaitForAsyncMigrations()
	t.Cleanup(func() { s.Close() })
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
