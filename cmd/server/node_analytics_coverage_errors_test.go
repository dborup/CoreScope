package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// Wrap the real SQLite driver to force count, row-iteration and conversion
// failures independently, without production hooks or timing assumptions.
var coverage485RegisterFailureDriver sync.Once

type coverage485FailureDriver struct{}
type coverage485FailureConn struct {
	driver.Conn
	failure string
}
type coverage485FailureRows struct {
	driver.Rows
	failure string
	n       int
}

func (coverage485FailureDriver) Open(dsn string) (driver.Conn, error) {
	parts := strings.SplitN(dsn, "|", 2)
	conn, err := (&sqlite.Driver{}).Open(parts[0])
	if err != nil {
		return nil, err
	}
	return coverage485FailureConn{Conn: conn, failure: parts[1]}, nil
}

func (c coverage485FailureConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.failure == "count" && strings.HasPrefix(q, "SELECT COUNT(*) FROM transmissions") {
		return nil, errors.New("injected eligible-count failure")
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(q, "SELECT t.id, t.raw_hex") {
		return &coverage485FailureRows{Rows: rows, failure: c.failure}, nil
	}
	return rows, nil
}

func (r *coverage485FailureRows) Next(values []driver.Value) error {
	if r.failure == "cursor" && r.n == 1 {
		return errors.New("injected cursor I/O failure")
	}
	err := r.Rows.Next(values)
	if err == nil && r.failure == "scan" && r.n == 0 {
		values[0] = "not an integer transmission id"
	}
	r.n++
	return err
}

func TestNodeAnalyticsCoverage485_LoadErrorsNeverCertifyFullHistory(t *testing.T) {
	coverage485RegisterFailureDriver.Do(func() { sql.Register("coverage485-failure", coverage485FailureDriver{}) })
	for _, mode := range []string{"legacy", "chunked"} {
		for _, failure := range []string{"count", "cursor", "scan"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "failure.db")
				createTestDBAt(t, path, 2)
				db, err := OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				original := db.conn
				t.Cleanup(func() { original.Close() })
				db.conn, err = sql.Open("coverage485-failure", path+"|"+failure)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				s := NewPacketStore(db, nil)
				if mode == "legacy" {
					err = s.Load()
				} else {
					err = s.LoadChunked(10000)
				}
				if err == nil && !s.WaitIndexesReady(5*time.Second) {
					t.Fatal("indexes not ready")
				}
				s.mu.RLock()
				c := s.nodeAnalyticsCoverageLocked(1, time.Now())
				failed := s.analyticsHistory.failed
				s.mu.RUnlock()
				if !c.Partial || (err == nil && !failed) {
					t.Fatalf("%s error silently certified: err=%v failed=%v coverage=%+v", failure, err, failed, c)
				}
				if err == nil && c.Seconds > 1 {
					t.Fatalf("failed historical subset used as supported history: %+v", c)
				}
			})
		}
	}
}
