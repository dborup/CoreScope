package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Issue #576: modernc.org/sqlite applies only _pragma, _time_format and
// _txlock from a DSN, so the mattn-style _busy_timeout/_journal_mode in the
// dry-run DSN were dropped and it ran with busy_timeout=0. These tests read
// the effective pragmas back.

func dsn576Pragmas(t *testing.T, db *sql.DB) (busy int, journal string) {
	t.Helper()
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	return busy, journal
}

// dsn576IngestorDB creates an ingestor database (OpenStore: WAL) and
// closes it. journalMode "delete" then switches the file to the rollback
// journal, the one mode in which a writer's lock blocks every reader.
func dsn576IngestorDB(t *testing.T, journalMode string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ingest.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if journalMode == "wal" {
		return path
	}
	c, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode("+journalMode+")")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, journal := dsn576Pragmas(t, c); journal != journalMode {
		t.Fatalf("fixture journal_mode = %q, want %q", journal, journalMode)
	}
	return path
}

// The ingestor's own handle already used the modernc syntax; pinned so it
// stays that way.
func TestOpenStore_EffectivePragmas_576(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	busy, journal := dsn576Pragmas(t, s.db)
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if busy != 5000 || journal != "wal" || fk != 1 {
		t.Fatalf("OpenStore: busy_timeout=%d journal_mode=%q foreign_keys=%d, want 5000 wal 1", busy, journal, fk)
	}
}

// The dry-run DSN gets busy_timeout(5000), stays mode=ro, and leaves the
// journal mode alone: on a rollback-journal file a mode=ro connection
// asked for journal_mode(WAL) fails to open.
func TestClientRxReadOnlyDSN_EffectivePragmas_576(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			path := dsn576IngestorDB(t, mode)
			db, err := sql.Open("sqlite", clientRxReadOnlyDSN(path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if busy, journal := dsn576Pragmas(t, db); busy != 5000 || journal != mode {
				t.Fatalf("busy_timeout=%d journal_mode=%q, want 5000 %q", busy, journal, mode)
			}
			if _, err := db.Exec(`DELETE FROM client_receptions`); err == nil {
				t.Fatal("dry-run connection accepted a write; it must stay mode=ro")
			}
		})
	}
}

// A dry-run that meets a write in progress waits for it instead of failing
// with SQLITE_BUSY ("database is locked").
func TestAdminDeleteClientRxDryRun_WaitsForWriter_576(t *testing.T) {
	path := dsn576IngestorDB(t, "delete")
	w, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()
	c, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx,
		`INSERT INTO client_receptions (rx_pubkey, heard_key, heard_keylen, lat, lon, rx_at, ingested_at, src)
		 VALUES (?, 'aabb01', 1, 0, 0, '2026-02-01T00:00:00Z', '2026-02-01T00:00:00Z', 'mqtt')`, clientRxTarget); err != nil {
		t.Fatal(err)
	}
	const hold = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		time.Sleep(hold)
		_, err := c.ExecContext(ctx, `COMMIT`)
		c.Close()
		done <- err
	}()
	start := time.Now()
	code := runAdminDeleteClientRxDryRun(path, clientRxTarget)
	waited := time.Since(start)
	if err := <-done; err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	if code != 0 {
		t.Fatalf("dry-run exit code %d after %v; it must wait for the writer, not fail", code, waited)
	}
	if waited < hold/2 {
		t.Fatalf("dry-run returned after %v; it cannot have waited for a %v lock", waited, hold)
	}
}
