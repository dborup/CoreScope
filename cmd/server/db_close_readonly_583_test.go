package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #583: the server's DB is opened mode=ro (#1283), and a read-only handle
// cannot checkpoint the WAL (it fails with disk I/O error 778). Since #578
// busy_timeout=5000 applies, so DB.Close's wal_checkpoint(TRUNCATE) first
// waited up to 5 s for an in-flight writer transaction (the ingestor's) and
// then failed anyway. Close must not try.

// holdWriterLock583 starts a write transaction on rw (the ingestor's role)
// and keeps it open until the test ends.
func holdWriterLock583(t *testing.T, rw *sql.DB) {
	t.Helper()
	conn, err := rw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), `ROLLBACK`)
		conn.Close()
	})
}

// closeWaitBound583 is well below busy_timeout (5000 ms): closing a
// read-only handle takes microseconds, a checkpoint waits the full 5 s.
const closeWaitBound583 = 2 * time.Second

func TestDBCloseDoesNotWaitForAWriter_583(t *testing.T) {
	path := filepath.Join(t.TempDir(), "srv.db")
	dsn576CreateDB(t, path, "wal")
	rw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rw.Close() })
	if _, err := rw.Exec(`INSERT INTO t576 VALUES (1)`); err != nil { // frames in the WAL
		t.Fatal(err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM t576`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("server read = %d, %v", n, err)
	}
	holdWriterLock583(t, rw)

	var closeErr error
	var took time.Duration
	out := captureLog(func() {
		done := make(chan struct{})
		go func() { defer close(done); closeErr = db.Close() }()
		t0 := time.Now()
		select {
		case <-done:
		case <-time.After(closeWaitBound583):
			<-done
		}
		took = time.Since(t0)
	})
	if took >= closeWaitBound583 {
		t.Fatalf("Close waited %v for the writer's lock (busy_timeout); a read-only handle cannot checkpoint", took)
	}
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	if strings.Contains(out, "checkpoint") || !strings.Contains(out, dbClosedLog) {
		t.Fatalf("Close logged %q, want %q and no checkpoint", out, dbClosedLog)
	}
}

// The process view: SIGTERM while the ingestor holds a write transaction.
// The shutdown sequence (#577) still runs in order and exits 0, without the
// 5 s busy wait at dbClose.
func TestServerProcess_SIGTERMWithActiveWriterSkipsBusyWait_583(t *testing.T) {
	path, rw := migratedDB(t)
	p := startServerProc(t, path, time.Minute)
	p.waitReady(t)
	holdWriterLock583(t, rw)
	t0 := time.Now()
	p.cmd.Process.Signal(syscall.SIGTERM)
	code, _ := p.waitExit(t, 30*time.Second)
	took := time.Since(t0)
	out := p.out.String()
	t.Logf("shutdown with an active writer transaction took %v", took)
	if took >= 5*time.Second {
		t.Fatalf("shutdown took %v with an active writer: dbClose waited out busy_timeout:\n%s", took, out)
	}
	if code != 0 {
		t.Fatalf("exit code %d after SIGTERM:\n%s", code, out)
	}
	assertLogOrder(t, out, "received terminated", "[ws] all clients disconnected", dbClosedLog, "[server] shutdown complete")
	if strings.Contains(out, "WAL checkpoint") {
		t.Fatalf("the read-only server tried to checkpoint:\n%s", out)
	}
}
