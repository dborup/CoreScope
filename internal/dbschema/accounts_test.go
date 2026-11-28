package dbschema

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// #1020 review F2: account_proofs gains proven_gen and redeemed_at, also on
// a table created by the earlier #1018 M1 shape, and EnsureAccounts stays
// idempotent.
func TestEnsureAccountsAddsProofColumns(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE account_proofs (request_id TEXT PRIMARY KEY, kind TEXT NOT NULL, account_id TEXT NULL,
		pubkey TEXT NOT NULL, code TEXT NOT NULL, redeem_hash TEXT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL, proven_at INTEGER NULL, proof_hash TEXT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := EnsureAccounts(db); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	for _, col := range []string{"proven_gen", "redeemed_at"} {
		if has, err := TableHasColumn(db, "account_proofs", col); err != nil || !has {
			t.Fatalf("%s: has=%v err=%v", col, has, err)
		}
	}
	fresh, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := EnsureAccounts(fresh); err != nil {
		t.Fatal(err)
	}
	if has, _ := TableHasColumn(fresh, "account_proofs", "redeemed_at"); !has {
		t.Fatal("a fresh table has no redeemed_at")
	}
}
