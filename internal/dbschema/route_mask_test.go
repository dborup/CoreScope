package dbschema

import "testing"

// Issue #89: transmissions.route_mask records every raw route type (0..3)
// observed for a content hash. Apply only adds the column (metadata-only
// ALTER). The partial index that tracks un-backfilled rows is built by the
// ingestor's async backfill after MQTT subscribe, never inside Apply: on a
// staging-sized DB the index build takes 18-19.5 s cold and would otherwise
// run before the subscription exists.

func TestApplyAddsRouteMaskColumn(t *testing.T) {
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
	has, err := TableHasColumn(db, "transmissions", "route_mask")
	if err != nil || !has {
		t.Fatalf("transmissions.route_mask missing after Apply (err=%v)", err)
	}
	var mask interface{}
	if err := db.QueryRow(`SELECT route_mask FROM transmissions WHERE hash = 'h1'`).Scan(&mask); err != nil {
		t.Fatal(err)
	}
	if mask != nil {
		t.Errorf("existing row route_mask = %v, want NULL (not yet backfilled; Apply must not invent bits)", mask)
	}
	var marker string
	if err := db.QueryRow(`SELECT name FROM _migrations WHERE name = ?`, RouteMaskColumnMigration).Scan(&marker); err != nil {
		t.Errorf("migration marker %q not recorded: %v", RouteMaskColumnMigration, err)
	}
	var idx int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, RouteMaskPendingIndex).Scan(&idx)
	if idx != 0 {
		t.Errorf("Apply built %s; the partial index must be built by the async backfill, not at start-up", RouteMaskPendingIndex)
	}
}

func TestAssertReady_RequiresRouteMaskColumn(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady after Apply: %v", err)
	}
	// A DB whose ingestor never added the column: AssertReady must refuse.
	if _, err := db.Exec(`ALTER TABLE transmissions DROP COLUMN route_mask`); err != nil {
		t.Fatal(err)
	}
	err := AssertReady(db)
	if err == nil || !contains(err.Error(), "transmissions.route_mask") {
		t.Fatalf("AssertReady must name the missing transmissions.route_mask, got %v", err)
	}
}

func TestRouteMaskPendingIndexSQL(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // IF NOT EXISTS
		if _, err := db.Exec(CreateRouteMaskPendingIndexSQL); err != nil {
			t.Fatalf("create pending index run %d: %v", i+1, err)
		}
	}
	var plan string
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT id FROM transmissions WHERE route_mask IS NULL ORDER BY id LIMIT 100`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		plan += detail + ";"
	}
	rows.Close()
	if !contains(plan, RouteMaskPendingIndex) {
		t.Errorf("pending-row lookup does not use %s: %s", RouteMaskPendingIndex, plan)
	}
}
