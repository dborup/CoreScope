package main

import (
	"database/sql"
	"testing"
)

// Issue #576 left the anomaly tool's DSNs alone: they already used the
// modernc _pragma syntax. Pinned by reading the effective pragmas back:
// the live-file opener waits up to 5 s on a lock, and the -immutable
// snapshot opener takes no locks and so has no busy timeout to set.
func TestOpenSQLite_EffectivePragmas_576(t *testing.T) {
	f := buildExportFixture(t, false)
	for name, c := range map[string]struct {
		open     func(string) (*sql.DB, error)
		wantBusy int
	}{
		"openReadOnly":  {openReadOnly, 5000},
		"openImmutable": {openImmutable, 0},
	} {
		t.Run(name, func(t *testing.T) {
			db, err := c.open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var busy int
			if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
				t.Fatal(err)
			}
			if busy != c.wantBusy {
				t.Fatalf("busy_timeout = %d, want %d", busy, c.wantBusy)
			}
		})
	}
}
