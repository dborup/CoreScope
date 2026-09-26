package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// Issue #89 / PR #93: the server and the ingestor start together (one
// supervisord), and the server requires route_mask_changes, which the
// ingestor creates during its start-up Apply. Exiting on a missing table made
// supervisord restart the server with growing back-off until it gave up
// (FATAL after 64.6 s in the staging gate) and left it stopped for good. The
// server now waits in-process, bounded and cancellable, but only while
// dbschema reports that migration as in progress or SQLite reports
// BUSY/LOCKED; every other schema defect still fails at once.

// schemaWaitPolicy is the internal start-up retry policy: Initial is the
// first wait, each later wait grows by Factor up to Max, and the whole wait
// ends Deadline after the first check. A status line is logged at most once
// per LogEvery.
type schemaWaitPolicy struct {
	Initial, Max time.Duration
	Factor       float64
	Deadline     time.Duration
	LogEvery     time.Duration
}

var defaultSchemaWaitPolicy = schemaWaitPolicy{
	Initial:  250 * time.Millisecond,
	Max:      2 * time.Second,
	Factor:   1.5,
	Deadline: 120 * time.Second,
	LogEvery: 10 * time.Second,
}

// schemaWaitClock lets tests replace real time.
type schemaWaitClock interface {
	Now() time.Time
	NewTimer(d time.Duration) (c <-chan time.Time, stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// SchemaWaitTimeoutError is returned when the schema is still not ready at
// the deadline. It wraps the last check error.
type SchemaWaitTimeoutError struct {
	Elapsed  time.Duration
	Attempts int
	Last     error
}

func (e *SchemaWaitTimeoutError) Error() string {
	return fmt.Sprintf("schema still not ready after %v (%d attempts): %v", e.Elapsed, e.Attempts, e.Last)
}

func (e *SchemaWaitTimeoutError) Unwrap() error { return e.Last }

// waitForSchema calls check until it succeeds, retrying only transient
// *dbschema.NotReadyError results with back-off. It returns the number of
// checks and nil on success, the check error at once when it is permanent,
// ctx.Err() when ctx is cancelled (never reported as a timeout), and a
// *SchemaWaitTimeoutError once p.Deadline has passed since the first check.
func waitForSchema(ctx context.Context, check func() error, p schemaWaitPolicy, clk schemaWaitClock, logf func(string, ...interface{})) (int, error) {
	start := clk.Now()
	delay := p.Initial
	var lastLog time.Time
	for attempts := 1; ; attempts++ {
		if err := ctx.Err(); err != nil {
			return attempts - 1, err
		}
		err := check()
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A shutdown that arrived during the check wins over its result.
			return attempts, ctxErr
		}
		now := clk.Now()
		elapsed := now.Sub(start)
		if err == nil {
			if attempts > 1 {
				logf("[db] schema ready after %v (%d attempts)", elapsed.Round(time.Millisecond), attempts)
			}
			return attempts, nil
		}
		var nr *dbschema.NotReadyError
		if !errors.As(err, &nr) || !nr.Transient() {
			return attempts, err
		}
		if elapsed >= p.Deadline {
			return attempts, &SchemaWaitTimeoutError{Elapsed: elapsed, Attempts: attempts, Last: err}
		}
		switch {
		case attempts == 1:
			logf("[db] waiting for the ingestor's start-up migrations (missing: %s); retrying for up to %v", strings.Join(nr.Items(), ", "), p.Deadline)
			lastLog = now
		case now.Sub(lastLog) >= p.LogEvery:
			logf("[db] still waiting for the schema (missing: %s): %d attempts, %v elapsed", strings.Join(nr.Items(), ", "), attempts, elapsed.Round(time.Second))
			lastLog = now
		}
		wait := delay
		if remaining := p.Deadline - elapsed; wait > remaining {
			wait = remaining
		}
		c, stop := clk.NewTimer(wait)
		select {
		case <-ctx.Done():
			stop()
			return attempts, ctx.Err()
		case <-c:
		}
		if next := time.Duration(float64(delay) * p.Factor); next < p.Max {
			delay = next
		} else {
			delay = p.Max
		}
	}
}

// waitForServerSchema waits until dbschema.AssertReady accepts the database.
func waitForServerSchema(ctx context.Context, db *sql.DB, p schemaWaitPolicy, clk schemaWaitClock, logf func(string, ...interface{})) (int, error) {
	return waitForSchema(ctx, func() error { return dbschema.AssertReady(db) }, p, clk, logf)
}

// waitForDBSchema is the server's start-up gate: it waits for the schema and
// then re-reads the optional-column flags OpenDB detected before the wait,
// because on a pre-#93 database the route_mask column only appears while
// the server waits (detectSchema only ever sets flags).
func waitForDBSchema(ctx context.Context, db *DB, p schemaWaitPolicy, clk schemaWaitClock, logf func(string, ...interface{})) (int, error) {
	return waitForDBSchemaWith(ctx, db, func() error { return dbschema.AssertReady(db.conn) }, p, clk, logf)
}

func waitForDBSchemaWith(ctx context.Context, db *DB, check func() error, p schemaWaitPolicy, clk schemaWaitClock, logf func(string, ...interface{})) (int, error) {
	attempts, err := waitForSchema(ctx, check, p, clk, logf)
	if err == nil {
		db.detectSchema()
	}
	return attempts, err
}
