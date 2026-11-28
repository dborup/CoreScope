package dbschema

import "testing"

// PR #93 review: route_mask_changes is the durable change log from the
// ingestor to running servers. A new route bit on an existing transmission
// whose observation row is only upserted (same observer and path, same id)
// is otherwise invisible to a server that polls new ids.

func TestApplyCreatesRouteMaskChangesTable(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type) VALUES ('11', 'h1', '2026-09-24T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := Apply(db, nil); err != nil {
			t.Fatalf("Apply run %d: %v", i+1, err)
		}
	}
	for _, col := range []string{"id", "transmission_id", "route_mask", "created_at"} {
		if has, err := TableHasColumn(db, "route_mask_changes", col); err != nil || !has {
			t.Fatalf("route_mask_changes.%s missing (err=%v)", col, err)
		}
	}
	var idx int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, RouteMaskChangesTxIndex).Scan(&idx)
	if idx != 1 {
		t.Fatalf("index %s missing", RouteMaskChangesTxIndex)
	}
	var marker string
	if err := db.QueryRow(`SELECT name FROM _migrations WHERE name = ?`, RouteMaskChangesMigration).Scan(&marker); err != nil {
		t.Errorf("migration marker %q not recorded: %v", RouteMaskChangesMigration, err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM route_mask_changes`).Scan(&n)
	if n != 0 {
		t.Errorf("Apply wrote %d change rows; the table must start empty (no backfill of events)", n)
	}
	var mask interface{}
	db.QueryRow(`SELECT route_mask FROM transmissions WHERE hash = 'h1'`).Scan(&mask)
	if mask != nil {
		t.Errorf("Apply changed an existing transmission: route_mask = %v", mask)
	}
}

// Servers poll the log with an id cursor, so an id must never be reused
// after retention deletes the newest rows.
func TestRouteMaskChangesIDsAreNeverReused(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	ins := func() int64 {
		res, err := db.Exec(`INSERT INTO route_mask_changes (transmission_id, route_mask, created_at) VALUES (1, 2, 1790244000)`)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	ins()
	ins()
	last := ins()
	if _, err := db.Exec(`DELETE FROM route_mask_changes`); err != nil {
		t.Fatal(err)
	}
	if next := ins(); next <= last {
		t.Fatalf("id %d reused after deleting up to %d; a server cursor at %d would skip it", next, last, last)
	}
}

func TestAssertReady_RequiresRouteMaskChangesTable(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady after Apply: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE route_mask_changes`); err != nil {
		t.Fatal(err)
	}
	if err := AssertReady(db); err == nil || !contains(err.Error(), "table:route_mask_changes") {
		t.Fatalf("AssertReady must name the missing route_mask_changes table, got %v", err)
	}
}
