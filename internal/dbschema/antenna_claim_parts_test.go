package dbschema

import (
	"database/sql"
	"strings"
	"testing"
)

// #898 M1 T1 — the combined-claim migration on a real pre-#898 table:
// ADD COLUMN + a one-time backfill (N1), the column CHECKs, the admin
// contact table's cascade, and AssertReady.

// legacyClaimsDB is a database with the pre-#898 claims table and one row in
// every status, plus an approved clear and a pending clear.
func legacyClaimsDB(t *testing.T) *sql.DB {
	t.Helper()
	db := minimalDB(t)
	t.Cleanup(func() { db.Close() })
	if err := ensureNodeAntennaClaimsTable(db); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id     string
		h      interface{}
		status string
	}{
		{"h-pending", 10.0, "pending"}, {"h-proven", 11.0, "proven"}, {"h-approved", 12.0, "approved"},
		{"h-rejected", 13.0, "rejected"}, {"h-revoked", 14.0, "revoked"}, {"h-expired", 15.0, "expired"},
		{"h-superseded", 16.0, "superseded"}, {"c-approved", nil, "approved"}, {"c-pending", nil, "pending"},
	} {
		if _, err := db.Exec(`INSERT INTO node_antenna_claims (request_id, pubkey, height_m, code, status, created_at, expires_at)
			VALUES (?, 'ab', ?, '7KX9QM', ?, 1, 2)`, r.id, r.h, r.status); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

type partsRow struct {
	heightOp, detailsOp sql.NullString
	hIn, dIn            int
}

func readParts(t *testing.T, db *sql.DB) map[string]partsRow {
	t.Helper()
	rows, err := db.Query(`SELECT request_id, height_op, details_op, height_in_force, details_in_force FROM node_antenna_claims`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]partsRow{}
	for rows.Next() {
		var id string
		var p partsRow
		if err := rows.Scan(&id, &p.heightOp, &p.detailsOp, &p.hIn, &p.dIn); err != nil {
			t.Fatal(err)
		}
		out[id] = p
	}
	return out
}

func TestAntennaClaimParts898_LegacyBackfill(t *testing.T) {
	db := legacyClaimsDB(t)
	var logs []string
	logf := Logger(func(f string, a ...interface{}) { logs = append(logs, f) })
	if err := Apply(db, logf); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readParts(t, db)
	for id, p := range got {
		wantOp := "set"
		if strings.HasPrefix(id, "c-") {
			wantOp = "clear"
		}
		wantIn := 0
		if strings.HasSuffix(id, "-approved") {
			wantIn = 1
		}
		if p.heightOp.String != wantOp || p.detailsOp.Valid || p.hIn != wantIn || p.dIn != 0 {
			t.Errorf("%s: %+v, want height_op=%s height_in_force=%d, no details part", id, p, wantOp, wantIn)
		}
	}
	if len(got) != 9 {
		t.Fatalf("rows = %d, want 9", len(got))
	}
	if countLogs(logs, "#898") != 1 {
		t.Errorf("migration log lines = %d, want 1: %v", countLogs(logs, "#898"), logs)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady after the migration: %v", err)
	}

	// Idempotent: a second Apply changes nothing and logs nothing, even for
	// a row a test plants without an op afterwards (the backfill never runs
	// again, so it cannot turn a later NULL into a clear).
	mustExec(t, db, `INSERT INTO node_antenna_claims (request_id, pubkey, height_m, code, status, created_at, expires_at)
		VALUES ('planted', 'ab', NULL, '7KX9QM', 'approved', 1, 2)`)
	logs = nil
	if err := Apply(db, logf); err != nil {
		t.Fatal(err)
	}
	after := readParts(t, db)
	if p := after["planted"]; p.heightOp.Valid || p.detailsOp.Valid {
		t.Errorf("second Apply backfilled a new row: %+v", p)
	}
	delete(after, "planted")
	for id, p := range after {
		if got[id] != p {
			t.Errorf("second Apply changed %s: %+v → %+v", id, got[id], p)
		}
	}
	if countLogs(logs, "#898") != 0 {
		t.Errorf("second Apply logged the migration again: %v", logs)
	}
	// AssertReady refuses a row without a part, permanently.
	nr := notReady(t, AssertReady(db))
	if !strings.Contains(strings.Join(nr.Missing, ","), "rows without a part") || nr.Transient() {
		t.Errorf("planted partless row: missing=%v transient=%v", nr.Missing, nr.Transient())
	}
}

func TestAntennaClaimParts898_Checks(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	ins := func(cols, vals string) error {
		_, err := db.Exec(`INSERT INTO node_antenna_claims (request_id, pubkey, code, status, created_at, expires_at, ` + cols + `)
			VALUES (lower(hex(randomblob(8))), 'ab', '7KX9QM', 'approved', 1, 2, ` + vals + `)`)
		return err
	}
	for name, tc := range map[string]struct {
		cols, vals string
		ok         bool
	}{
		"height set":                    {"height_m, height_op", "12.5, 'set'", true},
		"height clear":                  {"height_m, height_op", "NULL, 'clear'", true},
		"height set without a value":    {"height_m, height_op", "NULL, 'set'", false},
		"height clear with a value":     {"height_m, height_op", "3, 'clear'", false},
		"bad height op":                 {"height_m, height_op", "3, 'merge'", false},
		"details set, all fields":       {"details_op, antenna_type, gain_dbi, power_mask, installation, contact_public", "'set', 'omni', 5.8, 7, 'mast', 'OZ1ABC'", true},
		"details clear":                 {"details_op", "'clear'", true},
		"details clear with a type":     {"details_op, antenna_type", "'clear', 'omni'", false},
		"type without a details part":   {"height_m, height_op, antenna_type", "3, 'set', 'omni'", false},
		"contact_public on a clear":     {"details_op, contact_public", "'clear', 'x'", false},
		"bad antenna type":              {"details_op, antenna_type", "'set', 'dipole'", false},
		"gain over 20":                  {"details_op, gain_dbi", "'set', 20.5", false},
		"power mask 0":                  {"details_op, power_mask", "'set', 0", false},
		"power mask 8":                  {"details_op, power_mask", "'set', 8", false},
		"bad installation":              {"details_op, installation", "'set', 'tower'", false},
		"contact_public 33 chars":       {"details_op, contact_public", "'set', '" + strings.Repeat("x", 33) + "'", false},
		"height in force without op":    {"details_op, height_in_force", "'clear', 1", false},
		"details in force without op":   {"height_m, height_op, details_in_force", "3, 'set', 1", false},
		"both in force, both carried":   {"height_m, height_op, details_op, height_in_force, details_in_force", "3, 'set', 'clear', 1, 1", true},
		"in force flag not boolean":     {"height_m, height_op, height_in_force", "3, 'set', 2", false},
		"details op not set or clear":   {"details_op", "'merge'", false},
		"height set with clear details": {"height_m, height_op, details_op", "4, 'set', 'clear'", true},
	} {
		if err := ins(tc.cols, tc.vals); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
	}
}

// TestAntennaClaimParts898_AdminContactCascade: retention deletes a claim
// and, with foreign keys on (the ingestor's connection), its admin contact.
func TestAntennaClaimParts898_AdminContactCascade(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	mustExec(t, db, `INSERT INTO node_antenna_claims (request_id, pubkey, code, status, created_at, expires_at, details_op, antenna_type)
		VALUES ('c1', 'ab', '7KX9QM', 'pending', 1, 2, 'set', 'omni')`)
	mustExec(t, db, `INSERT INTO node_claim_admin_contacts (request_id, contact) VALUES ('c1', 'OZ1ABC')`)
	if _, err := db.Exec(`INSERT INTO node_claim_admin_contacts (request_id, contact) VALUES ('nope', 'x')`); err == nil {
		t.Error("a contact without a claim was accepted")
	}
	if _, err := db.Exec(`INSERT INTO node_claim_admin_contacts (request_id, contact) VALUES ('c1', '')`); err == nil {
		t.Error("an empty contact was accepted")
	}
	mustExec(t, db, `DELETE FROM node_antenna_claims WHERE request_id = 'c1'`)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_claim_admin_contacts`).Scan(&n); err != nil || n != 0 {
		t.Errorf("contacts after the claim was deleted = %d (%v), want 0", n, err)
	}
}

// TestAntennaClaimParts898_AssertReady: an unmigrated claims table is the
// migration still to come (transient); a recorded migration with a column
// gone is permanent; no claims table at all is still fine.
func TestAntennaClaimParts898_AssertReady(t *testing.T) {
	db := legacyClaimsDB(t)
	// Everything else migrated, the claims table not yet: apply everything,
	// then roll the claims table back to its pre-#898 form.
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DROP TABLE node_claim_admin_contacts`)
	mustExec(t, db, `DROP TABLE node_antenna_claims`)
	mustExec(t, db, `DELETE FROM _migrations WHERE name = '`+AntennaClaimPartsMigration+`'`)
	if err := ensureNodeAntennaClaimsTable(db); err != nil {
		t.Fatal(err)
	}
	nr := notReady(t, AssertReady(db))
	if len(nr.Missing) != 1 || nr.Missing[0] != AntennaClaimPartsItem || !nr.Transient() {
		t.Errorf("legacy claims table: missing=%v transient=%v, want only %s, transient", nr.Missing, nr.Transient(), AntennaClaimPartsItem)
	}
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("after Apply: %v", err)
	}
	mustExec(t, db, `DROP TABLE node_claim_admin_contacts`)
	nr = notReady(t, AssertReady(db))
	if !strings.Contains(strings.Join(nr.Missing, ","), "table:node_claim_admin_contacts") || nr.Transient() {
		t.Errorf("recorded migration, contact table gone: missing=%v transient=%v, want permanent", nr.Missing, nr.Transient())
	}
}
