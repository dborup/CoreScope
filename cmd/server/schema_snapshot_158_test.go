package main

import (
	"testing"
	"time"
)

// IngestNewFromDB and IngestNewObservations build their SELECT from the
// optional-column schema flags (raw_hex, resolved_path, scope_name,
// route_mask) and then append one Scan destination per flag. The flags are
// atomics that the background healer (and main.go's forceTrue) can flip at
// any time, so reading them once for the query and again for the Scan can
// disagree: Scan gets the wrong number of destinations, the error is
// swallowed and the row is silently skipped. One snapshot per call keeps
// query and Scan consistent.
//
// The hooks flip the flag in the two windows a regression could use: after
// the query ran and before the rows are scanned (a Scan built from a fresh
// read), and after the snapshot and before the query is built (a SELECT
// built from a fresh read).

func TestIngestSchemaFlagFlipBetweenQueryAndScan_158(t *testing.T) {
	type flagCase struct {
		name, table string
		flag        func(*DB) *schemaFlag
	}
	cases := []flagCase{
		{"resolved_path", "observations", func(d *DB) *schemaFlag { return &d.hasResolvedPathFlag }},
		{"raw_hex", "observations", func(d *DB) *schemaFlag { return &d.hasObsRawHexFlag }},
		{"route_mask", "transmissions", func(d *DB) *schemaFlag { return &d.hasRouteMaskFlag }},
		{"scope_name", "transmissions", func(d *DB) *schemaFlag { return &d.hasScopeNameFlag }},
	}
	base := time.Now().UTC().Add(-30 * time.Minute)

	// The flag flips either right after the ingestCols snapshot (a query
	// built from a fresh flag read would not match the Scan) or between the
	// query and the Scan (a Scan built from a fresh read would not match).
	setFlip := func(store *PacketStore, at string, flip func()) {
		if at == "after_cols" {
			store.ingestAfterColsHook = flip
		} else {
			store.ingestAfterQueryHook = flip
		}
	}

	for _, c := range cases {
		for _, at := range []string{"after_cols", "after_query"} {
			for _, start := range []bool{true, false} {
				dir := at + "/true_to_false"
				if !start {
					dir = at + "/false_to_true"
				}

				t.Run("IngestNewFromDB/"+c.name+"/"+dir, func(t *testing.T) {
					db := setupTestDB(t)
					defer db.conn.Close()
					seedShareGrowth(t, db)
					ensureColumn158(t, db, c.table, c.name)
					store := loadedStore158(t, db)
					for i := 0; i < 3; i++ {
						insertShareTx(t, db, i, base.Add(time.Duration(i)*time.Second))
						insertShareObs(t, db, i, 1, base.Add(time.Duration(i*10+1)*time.Second))
					}
					f := c.flag(db)
					f.v.Store(start)
					setFlip(store, at, func() { f.v.Store(!start) })

					store.IngestNewFromDB(0, 100)

					store.mu.RLock()
					defer store.mu.RUnlock()
					for id := 1; id <= 3; id++ {
						if store.byTxID[id] == nil {
							t.Errorf("tx %d skipped: %s flag flipped (%s)", id, c.name, dir)
						}
					}
				})

				if c.name == "scope_name" {
					continue // IngestNewObservations does not select it
				}
				t.Run("IngestNewObservations/"+c.name+"/"+dir, func(t *testing.T) {
					db := setupTestDB(t)
					defer db.conn.Close()
					seedShareGrowth(t, db)
					ensureColumn158(t, db, c.table, c.name)
					for i := 0; i < 3; i++ {
						insertShareTx(t, db, i, base.Add(time.Duration(i)*time.Second))
						insertShareObs(t, db, i, 1, base.Add(time.Duration(i*10+1)*time.Second))
					}
					store := loadedStore158(t, db)
					since := maxObsID158(t, db)
					for i := 0; i < 3; i++ {
						insertShareObsPath(t, db, i, 2, `["A1","B2"]`, `["`+shareA1+`","`+shareB2+`"]`, base.Add(time.Duration(i*10+2)*time.Second))
					}
					f := c.flag(db)
					f.v.Store(start)
					setFlip(store, at, func() { f.v.Store(!start) })

					store.IngestNewObservations(since, 100)

					store.mu.RLock()
					defer store.mu.RUnlock()
					for id := 1; id <= 3; id++ {
						if n := len(store.byTxID[id].Observations); n != 2 {
							t.Errorf("tx %d has %d observations, want 2: row skipped when %s flag flipped (%s)", id, n, c.name, dir)
						}
					}
				})
			}
		}
	}
}

// ensureColumn158 adds an optional column the in-memory fixture lacks, so a
// schema flag can be latched true without the query failing on a missing
// column (which would mask the scan mismatch under test).
func ensureColumn158(t testing.TB, db *DB, table, col string) {
	t.Helper()
	rows, err := db.conn.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	have := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if name == col {
			have = true
		}
	}
	rows.Close()
	if !have {
		exec158(t, db, "ALTER TABLE "+table+" ADD COLUMN "+col+" TEXT")
	}
}
