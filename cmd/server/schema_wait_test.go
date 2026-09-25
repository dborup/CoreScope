package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// PR #93 staging gate: with the table missing, the old server exited on
// every start attempt; supervisord gave up after 64.6 s and the server stayed
// FATAL although the ingestor created the table 4 s later. The server now
// waits in-process, bounded and cancellable, and only for the ingestor's
// in-progress migration or SQLite BUSY/LOCKED.

// fakeClock advances only when waitForSchema waits on it.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
	block func(n int) bool // return true to make wait n never fire
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	if c.block != nil && c.block(len(c.waits)) {
		return ch, func() bool { return true }
	}
	c.now = c.now.Add(d)
	ch <- c.now
	return ch, func() bool { return false }
}

type busyErr int

func (b busyErr) Error() string { return fmt.Sprintf("sqlite code %d", int(b)) }
func (b busyErr) Code() int     { return int(b) }

var (
	missingTable = &dbschema.NotReadyError{Missing: []string{"table:route_mask_changes"}}
	busyProbe    = &dbschema.NotReadyError{Probes: []dbschema.ProbeFailure{{Item: "table:route_mask_changes", Err: busyErr(5)}}}
	permanent    = &dbschema.NotReadyError{Missing: []string{"observations.raw_hex"}}
)

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// checks returns a check that yields errs in order, then nil forever. It
// fails the test instead of looping without bound.
func checks(t *testing.T, errs ...error) (func() error, *int) {
	n := 0
	return func() error {
		n++
		if n > 10000 {
			t.Fatalf("check called %d times: the wait is unbounded", n)
		}
		if n <= len(errs) {
			return errs[n-1]
		}
		return nil
	}, &n
}

func always(t *testing.T, err error) (func() error, *int) {
	n := 0
	return func() error {
		n++
		if n > 10000 {
			t.Fatalf("check called %d times: the wait is unbounded", n)
		}
		return err
	}, &n
}

func TestWaitForSchema_TableAppearsAfterSeveralAttempts(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	check, n := checks(t, missingTable, missingTable, missingTable, missingTable)
	var log logSink
	attempts, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf)
	if err != nil || attempts != 5 || *n != 5 {
		t.Fatalf("waitForSchema = (%d, %v) after %d checks, want success on attempt 5", attempts, err, *n)
	}
	want := []time.Duration{250 * time.Millisecond, 375 * time.Millisecond, 562500 * time.Microsecond, 843750 * time.Microsecond}
	if fmt.Sprint(clk.waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", clk.waits, want)
	}
}

func TestWaitForSchema_BusyThenSuccess(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	check, _ := checks(t, busyProbe, busyProbe)
	var log logSink
	if attempts, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf); err != nil || attempts != 3 {
		t.Fatalf("waitForSchema = (%d, %v), want success on attempt 3", attempts, err)
	}
}

// Backoff grows from 250 ms by 1.5x to a 2 s cap, never waits past the
// deadline, and the deadline is measured from the first attempt.
func TestWaitForSchema_BackoffAndDeadline(t *testing.T) {
	start := time.Unix(0, 0)
	clk := &fakeClock{now: start}
	check, n := always(t, missingTable)
	var log logSink
	attempts, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf)
	var timeout *SchemaWaitTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("waitForSchema = (%d, %v), want *SchemaWaitTimeoutError", attempts, err)
	}
	if !errors.Is(err, missingTable) {
		t.Fatalf("timeout error does not wrap the last check error: %v", err)
	}
	elapsed := clk.now.Sub(start)
	if elapsed != defaultSchemaWaitPolicy.Deadline || timeout.Elapsed != elapsed {
		t.Fatalf("gave up after %v (reported %v), want exactly the %v deadline", elapsed, timeout.Elapsed, defaultSchemaWaitPolicy.Deadline)
	}
	if defaultSchemaWaitPolicy.Deadline < 120*time.Second {
		t.Fatalf("deadline %v, want at least 120 s", defaultSchemaWaitPolicy.Deadline)
	}
	var sum time.Duration
	for i, w := range clk.waits {
		if w <= 0 || w > 2*time.Second {
			t.Fatalf("wait %d = %v, want 0 < wait <= 2s", i, w)
		}
		if i > 0 && w < clk.waits[i-1] && i != len(clk.waits)-1 {
			t.Fatalf("wait %d = %v shrank from %v before the deadline", i, w, clk.waits[i-1])
		}
		sum += w
	}
	if clk.waits[0] != 250*time.Millisecond || clk.waits[6] != 2*time.Second {
		t.Fatalf("waits start %v, 7th %v; want 250ms growing to the 2s cap", clk.waits[0], clk.waits[6])
	}
	if *n != attempts || attempts != len(clk.waits)+1 {
		t.Fatalf("attempts %d, checks %d, waits %d: every wait must be followed by exactly one check", attempts, *n, len(clk.waits))
	}
}

func TestWaitForSchema_SuccessJustBeforeTheDeadline(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	n := 0
	check := func() error {
		n++
		if clk.now.Sub(time.Unix(0, 0)) < defaultSchemaWaitPolicy.Deadline-3*time.Second {
			return missingTable
		}
		return nil
	}
	var log logSink
	if _, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf); err != nil {
		t.Fatalf("schema ready %v before the deadline: %v", 3*time.Second, err)
	}
	if n < 60 {
		t.Fatalf("only %d checks over ~117 s", n)
	}
}

func TestWaitForSchema_CancellationIsNotATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clk := &fakeClock{now: time.Unix(0, 0), block: func(n int) bool {
		if n == 3 {
			cancel()
			return true
		}
		return false
	}}
	check, n := always(t, missingTable)
	var log logSink
	_, err := waitForSchema(ctx, check, defaultSchemaWaitPolicy, clk, log.logf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForSchema after cancellation = %v, want context.Canceled", err)
	}
	var timeout *SchemaWaitTimeoutError
	if errors.As(err, &timeout) {
		t.Fatal("cancellation reported as a timeout")
	}
	if *n != 3 {
		t.Fatalf("checks after cancellation: %d, want 3", *n)
	}
	for _, l := range log.lines {
		if strings.Contains(strings.ToLower(l), "timed out") || strings.Contains(strings.ToLower(l), "fatal") {
			t.Fatalf("cancellation logged a failure: %q", l)
		}
	}
}

// A shutdown signal that lands while the last check runs is still a
// shutdown: not a start (the check succeeded) and not a timeout (the check
// was the last before the deadline).
func TestWaitForSchema_CancellationDuringTheLastCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start time.Duration // clock offset of the cancelled check
		err   error         // what that check returns
	}{
		{"during a successful check", 0, nil},
		{"during the check at the deadline", defaultSchemaWaitPolicy.Deadline, missingTable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clk := &fakeClock{now: time.Unix(0, 0)}
			start := clk.now
			check := func() error {
				if clk.now.Sub(start) >= tc.start {
					cancel()
					return tc.err
				}
				return missingTable
			}
			var log logSink
			_, err := waitForSchema(ctx, check, defaultSchemaWaitPolicy, clk, log.logf)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waitForSchema = %v, want context.Canceled", err)
			}
		})
	}
}

func TestWaitForSchema_PermanentErrorsFailAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"another missing column", permanent},
		{"malformed change log", &dbschema.NotReadyError{Missing: []string{"index:idx_route_mask_changes_tx"}}},
		{"I/O error while probing", &dbschema.NotReadyError{Probes: []dbschema.ProbeFailure{{Item: "observers.iata", Err: busyErr(10)}}}},
		{"read-only / permission", &dbschema.NotReadyError{Probes: []dbschema.ProbeFailure{{Item: "observers.iata", Err: busyErr(8)}}}},
		{"marker recorded but table gone", &dbschema.NotReadyError{Missing: []string{"table:route_mask_changes (recorded in _migrations)"}}},
		{"not a NotReadyError", errors.New("sql: database is closed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			check, n := always(t, tc.err)
			var log logSink
			attempts, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf)
			if !errors.Is(err, tc.err) || attempts != 1 || *n != 1 || len(clk.waits) != 0 {
				t.Fatalf("waitForSchema = (%d, %v) after %d checks and %d waits, want the error at once", attempts, err, *n, len(clk.waits))
			}
		})
	}
}

// One line when the wait starts, one every LogEvery, one on success or
// timeout; never one per attempt. Lines name the missing items only.
func TestWaitForSchema_LoggingIsBounded(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	check, n := always(t, missingTable)
	var log logSink
	waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf)
	maxLines := int(defaultSchemaWaitPolicy.Deadline/defaultSchemaWaitPolicy.LogEvery) + 2
	if len(log.lines) > maxLines || len(log.lines) >= *n {
		t.Fatalf("%d log lines for %d attempts over %v, want at most %d", len(log.lines), *n, defaultSchemaWaitPolicy.Deadline, maxLines)
	}
	if !strings.Contains(log.lines[0], "waiting") || !strings.Contains(log.lines[0], "table:route_mask_changes") || strings.Contains(log.lines[0], "restart ingestor") {
		t.Fatalf("first line %q must say what is awaited, without telling the operator to restart", log.lines[0])
	}
	if !strings.Contains(log.lines[1], "still waiting") || !strings.Contains(log.lines[1], "table:route_mask_changes") {
		t.Fatalf("status line %q does not say what is still missing", log.lines[1])
	}
	for _, l := range log.lines {
		if strings.Contains(l, "SELECT") || strings.Contains(l, "password") {
			t.Fatalf("log line leaks SQL or credentials: %q", l)
		}
	}
	clk2 := &fakeClock{now: time.Unix(0, 0)}
	check2, _ := checks(t, missingTable, missingTable)
	var log2 logSink
	waitForSchema(context.Background(), check2, defaultSchemaWaitPolicy, clk2, log2.logf)
	if len(log2.lines) != 2 || !strings.Contains(log2.lines[1], "ready after") {
		t.Fatalf("short wait logged %q, want the start and the success line", log2.lines)
	}
	clk3 := &fakeClock{now: time.Unix(0, 0)}
	check3, _ := checks(t)
	var log3 logSink
	waitForSchema(context.Background(), check3, defaultSchemaWaitPolicy, clk3, log3.logf)
	if len(log3.lines) != 0 {
		t.Fatalf("a schema that is ready at once logged %q", log3.lines)
	}
}

// --- real SQLite ---

var testSchemaPolicy = schemaWaitPolicy{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond, Factor: 1.5, Deadline: 3 * time.Second, LogEvery: time.Second}

// fixtureSchemaDB copies the e2e fixture, migrates it and removes the
// change log (and its marker), returning the path and a writer handle.
func fixtureSchemaDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	src, err := os.Open(filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	path := filepath.Join(t.TempDir(), "schema.db")
	dst, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	rw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	rw.SetMaxOpenConns(1)
	t.Cleanup(func() { rw.Close() })
	if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE route_mask_changes`, `DELETE FROM _migrations WHERE name = 'route_mask_changes_v1'`} {
		if _, err := rw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path, rw
}

func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

// Real SQLite, fake clock: the ingestor's migration is simulated by a hook
// in the check, so the tests are deterministic and never wait in real time.

func TestWaitForServerSchema_AlreadyReadyDoesNotWait(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	ro := openRO(t, path)
	defer ro.Close()
	clk := &fakeClock{now: time.Unix(0, 0)}
	var log logSink
	attempts, err := waitForServerSchema(context.Background(), ro, defaultSchemaWaitPolicy, clk, log.logf)
	if err != nil || attempts != 1 || len(clk.waits) != 0 || len(log.lines) != 0 {
		t.Fatalf("ready schema: (%d, %v), %d waits, logs %q; want one check, no wait, no log", attempts, err, len(clk.waits), log.lines)
	}
}

// ingestorAfter returns a check that runs AssertReady and, right before
// attempt n, lets the "ingestor" run fn (e.g. its Apply).
func ingestorAfter(t *testing.T, ro *sql.DB, n int, fn func()) (func() error, *int) {
	calls := 0
	return func() error {
		calls++
		if calls > 1000 {
			t.Fatalf("check called %d times", calls)
		}
		if calls == n {
			fn()
		}
		return dbschema.AssertReady(ro)
	}, &calls
}

func TestWaitForServerSchema_ChangeLogCreatedLater(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	ro := openRO(t, path)
	defer ro.Close()
	check, calls := ingestorAfter(t, ro, 4, func() { dbschema.Apply(rw, func(string, ...interface{}) {}) })
	clk := &fakeClock{now: time.Unix(0, 0)}
	var log logSink
	attempts, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf)
	if err != nil || attempts != 4 || *calls != 4 || len(clk.waits) != 3 {
		t.Fatalf("waitForSchema = (%d, %v) after %d checks and %d waits, want success on attempt 4", attempts, err, *calls, len(clk.waits))
	}
}

// A pre-#93 database lacks the route_mask column and the change log; both
// appear in the same ingestor Apply.
func TestWaitForServerSchema_SeveralMissingItemsCreatedLater(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	for _, q := range []string{`ALTER TABLE transmissions DROP COLUMN route_mask`, `DELETE FROM _migrations WHERE name = 'transmissions_route_mask_v1'`} {
		if _, err := rw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ro := openRO(t, path)
	defer ro.Close()
	var nr *dbschema.NotReadyError
	if err := dbschema.AssertReady(ro); !errors.As(err, &nr) || len(nr.Missing) != 2 || !nr.Transient() {
		t.Fatalf("setup: want the column and the table missing (transient), got %v", err)
	}
	check, _ := ingestorAfter(t, ro, 3, func() { dbschema.Apply(rw, func(string, ...interface{}) {}) })
	clk := &fakeClock{now: time.Unix(0, 0)}
	var log logSink
	if attempts, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf); err != nil || attempts != 3 {
		t.Fatalf("waitForSchema = (%d, %v), want success on attempt 3", attempts, err)
	}
}

// holdWriterLock holds the SQLite write lock on the ingestor's (single)
// writer connection, as a long migration or the staging gate's external lock
// does, until the returned release func is called.
func holdWriterLock(t *testing.T, rw *sql.DB) func() {
	t.Helper()
	conn, err := rw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`BEGIN IMMEDIATE`, `INSERT INTO _migrations (name) VALUES ('lock-probe')`} {
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			conn.ExecContext(context.Background(), `ROLLBACK`)
			conn.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// The ingestor holds the writer lock; only after it releases the lock can it
// create the table, and only then is the schema verified.
func TestWaitForServerSchema_RealWriterLockReleasedLater(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	release := holdWriterLock(t, rw)
	ro := openRO(t, path)
	defer ro.Close()
	// The ingestor is a separate process: its own connection, short busy wait.
	ingestor, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(20)")
	if err != nil {
		t.Fatal(err)
	}
	defer ingestor.Close()
	ingestor.SetMaxOpenConns(1)
	var releasedAt int
	check, calls := ingestorAfter(t, ro, 5, func() {
		if err := dbschema.Apply(ingestor, func(string, ...interface{}) {}); err == nil {
			t.Error("Apply succeeded while the writer lock was held")
		}
		release()
		releasedAt = 5
		if err := dbschema.Apply(ingestor, func(string, ...interface{}) {}); err != nil {
			t.Errorf("Apply after the lock was released: %v", err)
		}
	})
	clk := &fakeClock{now: time.Unix(0, 0)}
	var log logSink
	attempts, err := waitForSchema(context.Background(), check, defaultSchemaWaitPolicy, clk, log.logf)
	if err != nil || attempts != 5 || releasedAt != 5 || *calls != 5 {
		t.Fatalf("waitForSchema = (%d, %v), released at %d; want success on the attempt after the release", attempts, err, releasedAt)
	}
	if err := dbschema.AssertReady(ro); err != nil {
		t.Fatalf("schema not verified afterwards: %v", err)
	}
}

func TestWaitForServerSchema_WrongDefinitionIsRejectedAtOnce(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	if _, err := rw.Exec(dbschema.CreateRouteMaskChangesTableSQL); err != nil { // no index
		t.Fatal(err)
	}
	ro := openRO(t, path)
	defer ro.Close()
	clk := &fakeClock{now: time.Unix(0, 0)}
	var log logSink
	attempts, err := waitForServerSchema(context.Background(), ro, defaultSchemaWaitPolicy, clk, log.logf)
	if err == nil || attempts != 1 || len(clk.waits) != 0 {
		t.Fatalf("waitForServerSchema = (%d, %v), want an immediate error for a malformed change log", attempts, err)
	}
}

func TestWaitForServerSchema_CancelledUnderRealLock(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	holdWriterLock(t, rw)
	ro := openRO(t, path)
	defer ro.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clk := &fakeClock{now: time.Unix(0, 0), block: func(n int) bool {
		if n == 2 {
			cancel()
			return true
		}
		return false
	}}
	var log logSink
	if _, err := waitForServerSchema(ctx, ro, defaultSchemaWaitPolicy, clk, log.logf); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForServerSchema = %v, want context.Canceled", err)
	}
}

// Real clock and real SQLite with a tiny policy: after success, timeout,
// permanent failure and cancellation the wait leaves no goroutine and no
// timer behind (it starts none), and the caller's connections close cleanly.
func TestWaitForServerSchema_NoLeaks(t *testing.T) {
	tiny := schemaWaitPolicy{Initial: time.Millisecond, Max: 2 * time.Millisecond, Factor: 1.5, Deadline: 20 * time.Millisecond, LogEvery: time.Second}
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, rw *sql.DB)
		cancel bool
		ok     func(error) bool
	}{
		{"success", func(t *testing.T, rw *sql.DB) { dbschema.Apply(rw, func(string, ...interface{}) {}) }, false, func(err error) bool { return err == nil }},
		{"timeout", func(t *testing.T, rw *sql.DB) {}, false, func(err error) bool { var e *SchemaWaitTimeoutError; return errors.As(err, &e) }},
		{"permanent", func(t *testing.T, rw *sql.DB) { rw.Exec(dbschema.CreateRouteMaskChangesTableSQL) }, false, func(err error) bool { return err != nil }},
		{"cancelled", func(t *testing.T, rw *sql.DB) {}, true, func(err error) bool { return errors.Is(err, context.Canceled) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, rw := fixtureSchemaDB(t)
			tc.setup(t, rw)
			ro := openRO(t, path)
			ro.Ping()
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancel {
				cancel()
			}
			before := runtime.NumGoroutine()
			var log logSink
			_, err := waitForServerSchema(ctx, ro, tiny, realClock{}, log.logf)
			cancel()
			if !tc.ok(err) {
				t.Fatalf("unexpected result %v", err)
			}
			// database/sql ends a read transaction's watcher goroutine just
			// after Rollback returns: yield until it is gone, bounded. Counted
			// before Close, which also stops the pool's own goroutine.
			after := runtime.NumGoroutine()
			for settle := time.Now().Add(5 * time.Second); after > before && time.Now().Before(settle); after = runtime.NumGoroutine() {
				runtime.Gosched()
			}
			if after > before {
				t.Fatalf("goroutines %d -> %d after the wait", before, after)
			}
			if err := ro.Close(); err != nil {
				t.Fatal(err)
			}
			if open := ro.Stats().OpenConnections; open != 0 {
				t.Fatalf("%d connections still open after Close", open)
			}
		})
	}
}

// Regression (review B1): the server reads its optional-column flags when it
// opens the database. On a pre-#93 database the route_mask column appears
// only during the wait, so the flags must be refreshed after it: otherwise
// the start-up load leaves route_mask out and never queues the loaded rows
// for the backfill refresh.
func TestWaitForDBSchema_RefreshesSchemaFlagsAfterWaiting(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	for _, q := range []string{`ALTER TABLE transmissions DROP COLUMN route_mask`, `DELETE FROM _migrations WHERE name = 'transmissions_route_mask_v1'`} {
		if _, err := rw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.hasRouteMask() {
		t.Fatal("setup: route_mask detected before the ingestor added it")
	}
	calls := 0
	check := func() error {
		calls++
		if calls == 3 {
			dbschema.Apply(rw, func(string, ...interface{}) {}) // the ingestor migrates
		}
		return dbschema.AssertReady(db.conn)
	}
	clk := &fakeClock{now: time.Unix(0, 0)}
	var log logSink
	if _, err := waitForDBSchemaWith(context.Background(), db, check, defaultSchemaWaitPolicy, clk, log.logf); err != nil {
		t.Fatal(err)
	}
	if !db.hasRouteMask() {
		t.Fatal("route_mask flag still false after the wait: the start-up load would skip route masks")
	}
	s := NewPacketStore(db, nil)
	if neighborEdgesTableExists(db.conn) { // main.go loads the graph before the packets
		s.graph.Store(loadNeighborEdgesFromDB(db.conn))
	}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if n := s.routeMaskPendingLen(); n == 0 {
		t.Fatal("loaded rows with a NULL route_mask were not queued for the backfill refresh")
	}
}
