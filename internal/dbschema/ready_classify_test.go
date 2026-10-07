package dbschema

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// PR #93 staging gate: the server must wait (bounded) while the ingestor
// creates this PR's start-up migrations, instead of exiting until supervisord
// gives up. AssertReady therefore reports a typed *NotReadyError whose
// Transient() is true only for that in-progress migration or SQLite
// BUSY/LOCKED; every other defect is permanent and must fail at once.

func readyDB(t *testing.T) *sql.DB {
	t.Helper()
	db := minimalDB(t)
	t.Cleanup(func() { db.Close() })
	if err := Apply(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return db
}

func dbPath(t *testing.T, db *sql.DB) string {
	t.Helper()
	var seq int
	var name, file string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func notReady(t *testing.T, err error) *NotReadyError {
	t.Helper()
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		t.Fatalf("AssertReady = %v (%T), want *NotReadyError", err, err)
	}
	return nr
}

// dropChangeLog returns a migrated database to the state before this PR's
// change-log migration: no table, no index, no marker.
func dropChangeLog(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP TABLE route_mask_changes`)
	mustExec(t, db, `DELETE FROM _migrations WHERE name = 'route_mask_changes_v1'`)
}

func TestAssertReady_MissingChangeLogIsTransient(t *testing.T) {
	db := readyDB(t)
	dropChangeLog(t, db)
	nr := notReady(t, AssertReady(db))
	if !nr.Transient() {
		t.Fatalf("missing route_mask_changes is the ingestor's in-progress migration, want transient: %v", nr)
	}
	if !strings.Contains(nr.Error(), "table:route_mask_changes") {
		t.Fatalf("error does not name the missing table: %v", nr)
	}
}

// A pre-#93 database lacks the route_mask column too: the ingestor adds the
// column and then the table in the same start-up Apply.
func TestAssertReady_PreRouteMaskMigrationIsTransient(t *testing.T) {
	db := readyDB(t)
	dropChangeLog(t, db)
	mustExec(t, db, `ALTER TABLE transmissions DROP COLUMN route_mask`)
	if nr := notReady(t, AssertReady(db)); !nr.Transient() {
		t.Fatalf("pre-#93 schema (column and table not created yet) want transient: %v", nr)
	}
}

func TestAssertReady_OtherDefectsArePermanent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
		want  string
	}{
		{"another missing column", []string{`ALTER TABLE observations DROP COLUMN raw_hex`}, "observations.raw_hex"},
		{"another missing table", []string{`DROP TABLE node_changes`}, "table:node_changes"},
		{"missing change log plus another missing column",
			[]string{`DROP TABLE route_mask_changes`, `DELETE FROM _migrations WHERE name = 'route_mask_changes_v1'`, `ALTER TABLE observations DROP COLUMN raw_hex`}, "observations.raw_hex"},
		// The ingestor records the marker in the same transaction as the
		// table, and never recreates a table whose marker exists.
		{"change-log marker recorded but table gone", []string{`DROP TABLE route_mask_changes`}, "table:route_mask_changes (recorded in _migrations)"},
		{"route_mask column missing although the change log exists",
			[]string{`ALTER TABLE transmissions DROP COLUMN route_mask`}, "transmissions.route_mask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := readyDB(t)
			for _, q := range tc.stmts {
				mustExec(t, db, q)
			}
			nr := notReady(t, AssertReady(db))
			if nr.Transient() {
				t.Fatalf("want permanent: %v", nr)
			}
			if !strings.Contains(nr.Error(), tc.want) {
				t.Fatalf("error does not name %q: %v", tc.want, nr)
			}
		})
	}
}

// A change log that exists but does not match the approved definition is a
// broken schema, not a migration in progress: the ingestor creates the table,
// its index and its marker in one transaction.
func TestAssertReady_MalformedChangeLogIsPermanent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ddl  []string
		want string
	}{
		{"no index", []string{CreateRouteMaskChangesTableSQL}, "index:" + RouteMaskChangesTxIndex},
		{"missing column", []string{`CREATE TABLE route_mask_changes (id INTEGER PRIMARY KEY AUTOINCREMENT, transmission_id INTEGER NOT NULL, route_mask INTEGER NOT NULL)`,
			CreateRouteMaskChangesIndexSQL}, "route_mask_changes.created_at"},
		{"ids can be reused (no AUTOINCREMENT)", []string{`CREATE TABLE route_mask_changes (id INTEGER PRIMARY KEY, transmission_id INTEGER NOT NULL, route_mask INTEGER NOT NULL, created_at INTEGER NOT NULL)`,
			CreateRouteMaskChangesIndexSQL}, "route_mask_changes.id AUTOINCREMENT"},
		{"index on the wrong column", []string{CreateRouteMaskChangesTableSQL,
			`CREATE INDEX ` + RouteMaskChangesTxIndex + ` ON route_mask_changes(created_at)`}, "index:" + RouteMaskChangesTxIndex},
		{"composite index", []string{CreateRouteMaskChangesTableSQL,
			`CREATE INDEX ` + RouteMaskChangesTxIndex + ` ON route_mask_changes(transmission_id, created_at)`}, "index:" + RouteMaskChangesTxIndex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := readyDB(t)
			dropChangeLog(t, db)
			for _, q := range tc.ddl {
				mustExec(t, db, q)
			}
			nr := notReady(t, AssertReady(db))
			if nr.Transient() {
				t.Fatalf("malformed change log want permanent: %v", nr)
			}
			if !strings.Contains(nr.Error(), tc.want) {
				t.Fatalf("error does not name %q: %v", tc.want, nr)
			}
		})
	}
}

// A reader that hits SQLite BUSY while the writer holds an exclusive lock is
// waiting for the ingestor, not looking at a broken schema.
func TestAssertReady_BusyProbeIsTransient(t *testing.T) {
	db := readyDB(t)
	path := dbPath(t, db)
	writer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	mustExec(t, writer, `PRAGMA locking_mode=EXCLUSIVE`)
	mustExec(t, writer, `BEGIN EXCLUSIVE`)
	mustExec(t, writer, `INSERT INTO _migrations (name) VALUES ('busy-probe')`)
	reader, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(20)")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	nr := notReady(t, AssertReady(reader))
	if !nr.Transient() || len(nr.Probes) == 0 {
		t.Fatalf("probe under an exclusive writer lock want transient with probe errors: %v", nr)
	}
	mustExec(t, writer, `ROLLBACK`)
	mustExec(t, writer, `PRAGMA locking_mode=NORMAL`)
	mustExec(t, writer, `SELECT 1 FROM _migrations LIMIT 1`) // releases the exclusive lock
	writer.Close()
	if err := AssertReady(reader); err != nil {
		t.Fatalf("after the lock is released: %v", err)
	}
}

// The ingestor creates the route_mask column, the change log and its marker
// while the server checks. Every probe must read one snapshot: a check that
// sees half of a migration (table gone but marker recorded, or column missing
// but table present) calls the in-progress migration permanent, and the
// server exits.
func TestAssertReady_MigrationCommittingMidCheckStaysTransient(t *testing.T) {
	for _, tc := range []struct {
		name         string
		preRouteMask bool
		commitAfter  string
	}{
		{"change log committed between the table and marker probes", false, "table:route_mask_changes"},
		{"column and change log committed between the column and table probes", true, "transmissions.route_mask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := readyDB(t)
			// As in production (the ingestor opens with journal_mode(WAL)):
			// the writer commits while the server's read snapshot is open.
			var mode string
			if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil || mode != "wal" {
				t.Fatalf("journal_mode=WAL: %q, %v", mode, err)
			}
			dropChangeLog(t, db)
			if tc.preRouteMask {
				mustExec(t, db, `ALTER TABLE transmissions DROP COLUMN route_mask`)
				mustExec(t, db, `DELETE FROM _migrations WHERE name = 'transmissions_route_mask_v1'`)
			}
			reader, err := sql.Open("sqlite", "file:"+dbPath(t, db)+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			committed := false
			err = assertReady(reader, func(item string) {
				if item != tc.commitAfter || committed {
					return
				}
				committed = true
				if err := Apply(db, nil); err != nil { // the ingestor commits mid-check
					t.Fatalf("Apply during the check: %v", err)
				}
			})
			if !committed {
				t.Fatalf("probe %q never ran", tc.commitAfter)
			}
			if nr := notReady(t, err); !nr.Transient() {
				t.Fatalf("a migration committing during the check was called permanent: %v", nr)
			}
			if err := AssertReady(reader); err != nil {
				t.Fatalf("next check after the migration: %v", err)
			}
		})
	}
}

type codeErr int

func (c codeErr) Error() string { return fmt.Sprintf("sqlite code %d", int(c)) }
func (c codeErr) Code() int     { return int(c) }

func TestNotReadyError_ProbeErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{codeErr(5), true},                            // SQLITE_BUSY
		{codeErr(6), true},                            // SQLITE_LOCKED
		{codeErr(517), true},                          // SQLITE_BUSY_SNAPSHOT
		{codeErr(262), true},                          // SQLITE_LOCKED_SHAREDCACHE
		{fmt.Errorf("probe: %w", codeErr(5)), true},   // wrapped
		{codeErr(10), false},                          // SQLITE_IOERR
		{codeErr(8), false},                           // SQLITE_READONLY
		{codeErr(3), false},                           // SQLITE_PERM
		{codeErr(14), false},                          // SQLITE_CANTOPEN
		{codeErr(11), false},                          // SQLITE_CORRUPT
		{codeErr(26), false},                          // SQLITE_NOTADB
		{errors.New("database is locked (5)"), false}, // text only: never classified by string
		{sql.ErrConnDone, false},
	} {
		nr := &NotReadyError{Missing: []string{"table:route_mask_changes"}, Probes: []ProbeFailure{{Item: "observers.iata", Err: tc.err}}}
		if got := nr.Transient(); got != tc.want {
			t.Errorf("probe error %v: Transient() = %v, want %v", tc.err, got, tc.want)
		}
	}
	if (&NotReadyError{}).Transient() {
		t.Error("an empty NotReadyError must not be transient")
	}
	// BUSY while probing the change log, column not added yet: still the
	// ingestor's in-progress migration.
	nr := &NotReadyError{Missing: []string{"transmissions.route_mask"}, Probes: []ProbeFailure{{Item: "table:route_mask_changes", Err: codeErr(5)}}}
	if !nr.Transient() {
		t.Errorf("column missing and change-log probe BUSY: want transient: %v", nr)
	}
	// A probe failure is always reported with its item.
	if !strings.Contains(nr.Error(), "table:route_mask_changes (probe error: sqlite code 5)") {
		t.Errorf("error text %q does not report the probe failure", nr.Error())
	}
}

// A closed or unusable handle is a permanent failure, not a wait.
func TestAssertReady_ClosedDatabaseIsPermanent(t *testing.T) {
	db := readyDB(t)
	db.Close()
	if nr := notReady(t, AssertReady(db)); nr.Transient() {
		t.Fatalf("closed database want permanent: %v", nr)
	}
}

// The change log, its index and its marker appear together: a server never
// sees the table without its index while the ingestor is migrating, and a
// failed creation leaves nothing behind.
func TestApply_ChangeLogCreatedAtomically(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE `+RouteMaskChangesTxIndex+` (x INTEGER)`) // the index name is taken
	if err := Apply(db, nil); err == nil {
		t.Fatal("Apply succeeded although the change-log index could not be created")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'route_mask_changes'`).Scan(&n)
	if n != 0 {
		t.Fatal("route_mask_changes was left behind without its index")
	}
	db.QueryRow(`SELECT COUNT(*) FROM _migrations WHERE name = ?`, RouteMaskChangesMigration).Scan(&n)
	if n != 0 {
		t.Fatal("the change-log marker was recorded although creation failed")
	}
}
