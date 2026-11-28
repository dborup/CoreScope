package main

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// The columns a merge fills with COALESCE are an explicit allow-list (#215
// review, NEW-4). A nullable column added to transmissions later, whose NULL
// may mean "pending" and not "unknown", must not be filled from a duplicate
// unless someone adds it here on purpose.
func TestFillableColumns_IsAnExplicitAllowList_215(t *testing.T) {
	want := []string{"channel_hash", "decoded_json", "from_pubkey", "payload_type", "payload_version", "route_type", "scope_name"}
	s := hm215Prepare(t, filepath.Join(t.TempDir(), "fill.db"), func(db *sql.DB) {})
	got, err := fillableColumns(s.db)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fillableColumns on today's schema = %v, want exactly %v", got, want)
	}

	// A nullable column nobody listed is not filled.
	hm215Exec(t, s.db, `ALTER TABLE transmissions ADD COLUMN zz_pending_marker TEXT`)
	got, err = fillableColumns(s.db)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fillableColumns after adding a nullable column = %v, want it unchanged: %v", got, want)
	}

	// A listed column the table does not have (an older schema) is left out, not
	// an error.
	hm215Exec(t, s.db, `ALTER TABLE transmissions DROP COLUMN zz_pending_marker`)
	hm215Exec(t, s.db, `DROP INDEX IF EXISTS idx_tx_scope_name`)
	hm215Exec(t, s.db, `ALTER TABLE transmissions DROP COLUMN scope_name`)
	got, err = fillableColumns(s.db)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want2 := []string{"channel_hash", "decoded_json", "from_pubkey", "payload_type", "payload_version", "route_type"}; !reflect.DeepEqual(got, want2) {
		t.Errorf("fillableColumns without scope_name = %v, want %v", got, want2)
	}
}

// NULL (not transport-scoped) and "" (transport-scoped, region unknown) are
// different values of scope_name (scopeNameForDB), and the merge keeps them
// apart: only NULL is filled from the duplicate (#215 review, NEW-4). The
// server's in-memory merge follows the same rule
// (TestHashMigrate_ScopeFillMatchesTheIngestorCoalesce_215).
func TestContentHashMigration_ScopeFillOnlyFillsNull_215(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "scope.db"), func(db *sql.DB) {
		ins := func(id int, raw, hash string, scope interface{}) {
			hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, last_seen, route_mask, scope_name)
				VALUES (?, ?, ?, '2026-01-01T00:00:00Z', 1, 4, '{}', 1, 1, ?)`, id, raw, hash, scope)
		}
		ins(70, hm215Raw(70), "stale-s-70", "") // transport-scoped, region unknown
		ins(71, hm215Raw(70), "stale-s-71", "#x")
		ins(72, hm215Raw(72), "stale-s-72", nil) // not transport-scoped
		ins(73, hm215Raw(72), "stale-s-73", "")
		ins(74, hm215Raw(74), "stale-s-74", nil)
		ins(75, hm215Raw(74), "stale-s-75", "#y")
		ins(76, hm215Raw(76), "stale-s-76", nil)
		ins(77, hm215Raw(76), "stale-s-77", nil)
	})
	get := func(id int) (v sql.NullString) {
		if err := s.db.QueryRow(`SELECT scope_name FROM transmissions WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatalf("tx %d: %v", id, err)
		}
		return v
	}
	for _, c := range []struct {
		id   int
		want sql.NullString
		why  string
	}{
		{70, sql.NullString{String: "", Valid: true}, `"" is a value: the duplicate's "#x" must not replace it`},
		{72, sql.NullString{String: "", Valid: true}, `NULL is filled with the duplicate's "", which stays "" and not NULL`},
		{74, sql.NullString{String: "#y", Valid: true}, "NULL is filled with the duplicate's value"},
		{76, sql.NullString{}, "NULL and NULL stay NULL"},
	} {
		if got := get(c.id); got != c.want {
			t.Errorf("survivor %d scope_name = %+v, want %+v: %s", c.id, got, c.want, c.why)
		}
	}
}
