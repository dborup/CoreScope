package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// Issue #184 part 2: the start-up load must select observations.resolved_path
// once the schema gate has passed, because the gate (dbschema.AssertReady)
// requires that column. If it does not, the hot window is indexed through the
// NULL branch (byNode only) and its relays get no byPathHop credit until the
// next restart.
//
// Both cases start the way OpenDB does when the server probes before the
// ingestor has added the column: the flag is false. The ingestor then adds it,
// and the server runs its schema gate and the start-up load, in main.go's
// order. The DB is built without OpenDB's 2 s healer so the window is
// deterministic; in production the healer does not bound it either, because
// main.go only forced the flag after the first chunk was ready.
func TestStartupLoadIndexesResolvedPathAfterSchemaGate_184(t *testing.T) {
	cases := []struct {
		name     string
		probeErr error // returned by every detectSchema pass after the gate
	}{
		// The scenario in the issue: waitForDBSchema re-probes after the
		// gate, so the column the ingestor added in the meantime is seen.
		{name: "column added after the first probe", probeErr: nil},
		// detectSchema swallows a failed PRAGMA. The gate itself has proved
		// the column, so the flag must not depend on that probe.
		{name: "post-gate probe fails", probeErr: errors.New("simulated SQLITE_BUSY on PRAGMA table_info")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, rw := fixtureSchemaDB(t)
			for _, q := range []string{`DROP INDEX IF EXISTS idx_observations_resolved_path`, `ALTER TABLE observations DROP COLUMN resolved_path`} {
				if _, err := rw.Exec(q); err != nil {
					t.Fatal(err)
				}
			}

			conn, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
			if err != nil {
				t.Fatal(err)
			}
			db := &DB{conn: conn, path: path}
			defer db.Close()
			db.detectSchema() // OpenDB's probe, before the ingestor's migration
			if db.hasResolvedPath() {
				t.Fatal("setup: resolved_path detected before the ingestor added it")
			}

			// The ingestor migrates (adds the column) and writes a resolved
			// hop for one observation that carries a path.
			if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil {
				t.Fatal(err)
			}
			var obsID, txID int
			var pathJSON string
			if err := rw.QueryRow(`SELECT id, transmission_id, path_json FROM observations
				WHERE path_json IS NOT NULL AND path_json NOT IN ('', '[]') ORDER BY id LIMIT 1`).Scan(&obsID, &txID, &pathJSON); err != nil {
				t.Fatal(err)
			}
			relay := strings.Repeat("18", 31) + "4a" // matches no fixture node or hop
			rp := make([]*string, len(parsePathJSON(pathJSON)))
			rp[0] = &relay
			if _, err := rw.Exec(`UPDATE observations SET resolved_path = ? WHERE id = ?`, marshalResolvedPath(rp), obsID); err != nil {
				t.Fatal(err)
			}

			db.schemaProbeHook = func() error { return tc.probeErr }
			clk := &fakeClock{now: time.Unix(0, 0)}
			var log logSink
			if _, err := waitForDBSchema(context.Background(), db, testSchemaPolicy, clk, log.logf); err != nil {
				t.Fatal(err)
			}
			if !db.hasResolvedPath() {
				t.Error("hasResolvedPath() is false after the schema gate, which requires observations.resolved_path")
			}

			// main.go: graph before packets, then RunStartupLoad.
			s := NewPacketStore(db, nil)
			if neighborEdgesTableExists(db.conn) {
				s.graph.Store(loadNeighborEdgesFromDB(db.conn))
			}
			if err := s.RunStartupLoad(1000); err != nil {
				t.Fatal(err)
			}
			s.mu.RLock()
			defer s.mu.RUnlock()
			found := false
			for _, tx := range s.byPathHop[relay] {
				if tx.ID == txID {
					found = true
				}
			}
			if !found {
				t.Fatalf("byPathHop[%s…] does not hold tx %d: the start-up load did not read resolved_path (indexed %d tx)", relay[:8], txID, len(s.byPathHop[relay]))
			}
		})
	}
}
