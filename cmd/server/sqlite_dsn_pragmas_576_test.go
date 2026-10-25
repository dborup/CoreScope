package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Issue #576: modernc.org/sqlite applies only _pragma, _time_format and
// _txlock from a DSN. The mattn-style _busy_timeout and _journal_mode the
// server's DSNs asked for were silently dropped, so every connection ran
// with busy_timeout=0 and failed with SQLITE_BUSY the moment it met a lock.
// These tests read the effective pragmas back instead of trusting the DSN.

// dsn576CreateDB creates path with one table in the given journal mode and
// checks that the mode really took.
func dsn576CreateDB(t *testing.T, path, journalMode string) {
	t.Helper()
	w, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode("+journalMode+")")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec(`CREATE TABLE t576 (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := w.QueryRow(`PRAGMA journal_mode`).Scan(&got); err != nil || got != journalMode {
		t.Fatalf("fixture journal_mode = %q (%v), want %q", got, err, journalMode)
	}
}

// dsn576RawJournalMode reads the file's journal mode through a fresh
// connection that sets nothing.
func dsn576RawJournalMode(t *testing.T, path string) string {
	t.Helper()
	c, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var mode string
	if err := c.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

// dsn576AssertPragmas checks busy_timeout and journal_mode on n distinct
// pooled connections, held at the same time so the pool cannot hand the
// same one out twice. The DSN pragmas run once per new connection, so a
// single read would not show that every connection got them.
func dsn576AssertPragmas(t *testing.T, db *sql.DB, n, wantBusy int, wantJournal string) {
	t.Helper()
	ctx := context.Background()
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, c)
		var busy int
		var journal string
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if busy != wantBusy || journal != wantJournal {
			t.Errorf("conn %d: busy_timeout=%d journal_mode=%q, want %d %q", i, busy, journal, wantBusy, wantJournal)
		}
	}
}

// dsn576HoldExclusive takes an EXCLUSIVE lock on path with an uncommitted
// INSERT and commits it after hold. While the lock is held no other
// connection can read the file. The returned channel yields the commit
// error.
func dsn576HoldExclusive(t *testing.T, path string, hold time.Duration) <-chan error {
	t.Helper()
	w, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	ctx := context.Background()
	c, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO t576 (x) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(hold)
		_, err := c.ExecContext(ctx, `COMMIT`)
		c.Close()
		done <- err
	}()
	return done
}

// dsn576AssertReaderWaits runs a COUNT(*) on reader while another
// connection holds an EXCLUSIVE lock for hold. With a busy timeout the
// reader waits for the commit and sees its row; with busy_timeout=0 it
// fails at once with "database is locked".
func dsn576AssertReaderWaits(t *testing.T, path string, reader *sql.DB) {
	t.Helper()
	const hold = 300 * time.Millisecond
	done := dsn576HoldExclusive(t, path, hold)
	start := time.Now()
	var n int
	err := reader.QueryRow(`SELECT COUNT(*) FROM t576`).Scan(&n)
	waited := time.Since(start)
	if cerr := <-done; cerr != nil {
		t.Fatalf("writer commit: %v", cerr)
	}
	if err != nil {
		t.Fatalf("reader failed after %v instead of waiting for the writer: %v", waited, err)
	}
	if n != 1 {
		t.Fatalf("reader saw %d rows, want the writer's committed row", n)
	}
	if waited < hold/2 {
		t.Fatalf("reader returned after %v; it cannot have waited for a %v lock", waited, hold)
	}
}

// OpenDB applies busy_timeout(5000) to every pooled connection and leaves
// the file's journal mode alone: the server is read-only (#1283), WAL is the
// ingestor's to set. A rollback-journal file must still open; asking a
// mode=ro connection for journal_mode(WAL) fails on one with "attempt to
// write a readonly database".
func TestOpenDB_EffectivePragmas_576(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "srv.db")
			dsn576CreateDB(t, path, mode)
			d, err := OpenDB(path)
			if err != nil {
				t.Fatalf("OpenDB on a %s-mode file: %v", mode, err)
			}
			dsn576AssertPragmas(t, d.conn, 4, 5000, mode)
			if _, err := d.conn.Exec(`INSERT INTO t576 (x) VALUES (1)`); err == nil {
				t.Fatal("server connection accepted a write; it must stay mode=ro")
			}
			d.Close()
			if got := dsn576RawJournalMode(t, path); got != mode {
				t.Fatalf("OpenDB changed the file's journal mode from %q to %q", mode, got)
			}
		})
	}
}

func TestOpenDB_ReaderWaitsForWriter_576(t *testing.T) {
	path := filepath.Join(t.TempDir(), "srv.db")
	dsn576CreateDB(t, path, "delete")
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dsn576AssertReaderWaits(t, path, d.conn)
}

// The ping-score history store is a separate, server-owned file with one
// connection. Both of its DSNs get busy_timeout(5000). Its journal mode
// stays the rollback journal it has always had in practice (the ignored
// _journal_mode=WAL never took): in WAL a mode=ro inspection open creates a
// -wal file, which TestOpenPingScoreHistoryStore_UnknownNewerVersionDoesNotTouchWALFile
// forbids.
func TestOpenPingScoreHistoryConn_EffectivePragmas_576(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, writable := range []bool{true, false} {
		conn, err := openPingScoreHistoryConn(path, writable)
		if err != nil {
			t.Fatalf("writable=%v: %v", writable, err)
		}
		dsn576AssertPragmas(t, conn, 1, 5000, "delete")
		conn.Close()
	}
	if got := dsn576RawJournalMode(t, path); got != "delete" {
		t.Fatalf("history file journal_mode = %q, want delete", got)
	}
}

func TestOpenPingScoreHistoryConn_ReaderWaitsForWriter_576(t *testing.T) {
	for _, writable := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "h.db")
		dsn576CreateDB(t, path, "delete")
		conn, err := openPingScoreHistoryConn(path, writable)
		if err != nil {
			t.Fatal(err)
		}
		dsn576AssertReaderWaits(t, path, conn)
		conn.Close()
	}
}

// mattnStyleDSNParam matches a DSN query parameter in the mattn/go-sqlite3
// style (_busy_timeout=, _journal_mode=, _synchronous=, ...). modernc
// ignores every one of them except _time_format and _txlock.
var mattnStyleDSNParam = regexp.MustCompile(`[?&]_([a-z_]+)=`)

// dsn576ScanIgnoredParams reports every DSN parameter under dir (non-recursive,
// .go files including tests) that modernc would silently drop.
func dsn576ScanIgnoredParams(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range mattnStyleDSNParam.FindAllStringSubmatch(line, -1) {
				switch m[1] {
				case "pragma", "time_format", "txlock":
					continue
				}
				bad = append(bad, fmt.Sprintf("%s:%d: %s", filepath.Join(dir, e.Name()), i+1, strings.TrimSpace(line)))
			}
		}
	}
	return bad
}

// No DSN in cmd/server, cmd/ingestor, the SQLite CLIs (cmd/anomaly,
// cmd/decrypt, cmd/migrate, #583) or internal/* (production or test
// fixture) may use a parameter modernc ignores: a fixture that asks for WAL
// and silently runs in the rollback journal with no busy timeout is how
// #567's SQLITE_BUSY flake came about.
func TestDSNsUseModerncPragmaSyntax_576(t *testing.T) {
	internal, err := filepath.Glob("../../internal/*")
	if err != nil || len(internal) == 0 {
		t.Fatalf("no internal/* packages found (%v)", err)
	}
	cmds := []string{".", "../ingestor", "../anomaly", "../decrypt", "../migrate"}
	for _, dir := range cmds {
		// A renamed command must not silently drop out of the scan.
		if goFiles, _ := filepath.Glob(filepath.Join(dir, "*.go")); len(goFiles) == 0 {
			t.Fatalf("%s has no .go files: the scan list is stale", dir)
		}
	}
	var bad []string
	for _, dir := range append(cmds, internal...) {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		bad = append(bad, dsn576ScanIgnoredParams(t, dir)...)
	}
	if len(bad) > 0 {
		t.Errorf("DSN parameters modernc.org/sqlite ignores (use _pragma=name(value)):\n  %s", strings.Join(bad, "\n  "))
	}
}
