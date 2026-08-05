// Package main: ping-score-history production-wiring layer -- fase 5 of
// the recompute redesign (see reviews/CoreScope-code-review-2026-08-04.md
// and the accompanying fase 5A design discussion, approved as v5). This
// file is where the glue between the already-approved engine/store (fase
// 4A-4E) and a real running server lives.
//
// Fase 5C added the retention-configuration bridge (pingScoreHistoryRetentionDuration).
//
// Fase 5D added the single-owner worker/lifecycle core: every type is a
// small, internal (unexported) interface or function type satisfied
// structurally by the already-approved fase 4 engine/store types
// (*pingScoreHistoryEngine, *PingScoreHistoryStore) with zero adapter
// code required in production, and by fakes in tests. Every dependency
// that layer needs (publish/setStatus/reportError/open/newEngine/
// newTicker/wait) is a plain injected callback, so it is testable in
// complete isolation from *Server, main.go, healthz, and routes.
//
// Fase 5F added the Server-scoped, healthz-exposed status
// (setPingScoreHistoryStatus/pingScoreHistoryStatusView), additive-only.
//
// Fase 5G, at the bottom of this file, adds the thin production
// adapters around fase 5D's core and StartPingScoreHistoryEngine -- the
// actual main.go entry point that starts the worker for real.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// --- fase 5D: internal interfaces --------------------------------------
//
// Every type below is unexported and satisfied structurally -- no public
// API surface, no change to any existing exported type or function.
// *pingScoreHistoryEngine already has (or, for QuickSnapshot, fase 5B
// already added) both methods pingScoreHistoryCycler needs, with EXACTLY
// these signatures; *PingScoreHistoryStore already has both methods
// pingScoreHistoryStoreHandle needs (ReadOnly, Close, fase 4A). Neither
// needs a single line of adapter code to satisfy these interfaces.

// pingScoreHistoryCycler is the minimal engine surface the worker core
// needs.
type pingScoreHistoryCycler interface {
	QuickSnapshot() (*PingScoresSnapshot, error)
	Cycle() (*PingScoresSnapshot, error)
}

// pingScoreHistoryStoreHandle is the minimal store surface the worker
// core and lifecycle core need -- just enough to check read-only-ness and
// release the connection. Every real read/write happens INSIDE
// pingScoreHistoryCycler's methods; nothing else in this file ever calls
// anything else on the store.
type pingScoreHistoryStoreHandle interface {
	ReadOnly() bool
	Close() error
}

// pingScoreHistoryOpener abstracts "open (or reopen) the history store at
// path". A production adapter (a later phase) wraps OpenPingScoreHistoryStore;
// tests inject a closure -- never a package-level mutable var, so
// parallel tests can never stomp on each other.
type pingScoreHistoryOpener func(path string) (pingScoreHistoryStoreHandle, error)

// pingScoreHistoryTicker abstracts the worker's steady-state pacing
// source. A production adapter (a later phase) wraps *time.Ticker (which
// needs one line of glue: its C field isn't itself a method). Tests
// inject a fake backed by a plain channel they control directly, making
// pacing assertions deterministic instead of wall-clock-flaky.
type pingScoreHistoryTicker interface {
	C() <-chan time.Time
	Stop()
}

// pingScoreHistoryWaiter abstracts "wait for d, or return early if ctx is
// cancelled". Returns true if cancelled. A production adapter wraps
// time.After; tests inject a channel-controlled fake that can release
// exactly one retry, count calls, and prove cancellation-during-wait
// needs no real sleep at all.
type pingScoreHistoryWaiter func(ctx context.Context, d time.Duration) bool

// pingScoreHistoryErrorReporter is the sole channel for FULL error detail
// (including anything that might contain file paths or SQL) -- status
// (via setStatus) carries only a sanitized state/code string, never a raw
// error. A production adapter logs `[ping-scores-history] <code>: <full
// error>`; tests inject a spy that records calls deterministically.
type pingScoreHistoryErrorReporter func(code string, err error)

// pingScoreHistoryMaxRetentionPacketDays is the largest packetDays value
// that can be multiplied by 24*time.Hour without overflowing
// time.Duration's underlying int64 (nanoseconds). Anything larger would
// silently wrap to an incorrect (possibly negative) duration --
// pingScoreHistoryRetentionDuration rejects it explicitly instead of ever
// computing the overflowed value.
const pingScoreHistoryMaxRetentionPacketDays = math.MaxInt64 / int64(24*time.Hour)

// pingScoreHistoryRetentionDuration converts the server's existing
// retention.packetDays config into the RetentionDuration
// pingScoreHistoryEngineConfig expects, reusing the SAME authoritative
// semantics cmd/ingestor/config.go's own PacketDaysOrZero already
// enforces for the identical field: packetDays is the single source of
// truth for how long the ingestor keeps transmissions/observations
// before PruneOldPackets (cmd/ingestor/main.go) removes them, so the
// history engine's DataPruned/permanent-unreconstructable/gap detection
// must judge age against exactly this window -- never an invented
// default.
//
//   - cfg == nil, or cfg.Retention == nil: returns (0, nil) -- retention
//     is simply not configured. Matches PacketDaysOrZero's own "no
//     Retention block" branch.
//   - cfg.Retention.PacketDays == 0: returns (0, nil) -- PacketDays'
//     own doc comment says "0 disables"; the history engine treats
//     RetentionDuration<=0 as "DataPruned/permanent-unreconstructable/
//     gap detection disabled" (see pingScoreHistoryEngineConfig's own
//     doc comment in ping_score_history_engine.go) -- the exact same
//     meaning, reused unchanged, not a new default.
//   - cfg.Retention.PacketDays > 0 and safely convertible: returns
//     (time.Duration(PacketDays) * 24 * time.Hour, nil) -- the same
//     "N days" interpretation PruneOldPackets itself uses.
//   - cfg.Retention.PacketDays < 0: returns an error. This is a
//     DELIBERATE departure from PacketDaysOrZero's own behavior --
//     PacketDaysOrZero's `> 0` guard silently folds a negative value
//     into "disabled" (identical to zero), with no error and no log.
//     That silent equivalence is a reasonable choice for the ingestor's
//     own prune-or-don't-prune decision, but this bridge exists
//     specifically so a negative value -- almost certainly a config
//     mistake, since nothing legitimately means "prune after -5 days" --
//     never gets a chance to silently masquerade as either "disabled" or
//     a valid duration. The caller (a later production-wiring phase) is
//     expected to fail startup rather than silently run with wrong
//     retention-based detection. (There is no existing config-loader
//     guarantee that rules this out ahead of time -- LoadConfig performs
//     no range validation on retention.packetDays -- so this defensive
//     check is load-bearing, not redundant.)
//   - cfg.Retention.PacketDays > pingScoreHistoryMaxRetentionPacketDays:
//     returns an error rather than ever computing
//     time.Duration(PacketDays)*24*time.Hour, which would overflow
//     time.Duration's int64 and wrap to an incorrect value. (Also not
//     ruled out by the config loader -- packetDays is a plain JSON int
//     with no upper bound enforced anywhere.)
//
// Never mutates cfg or cfg.Retention -- a pure read with no side effects.
func pingScoreHistoryRetentionDuration(cfg *Config) (time.Duration, error) {
	if cfg == nil || cfg.Retention == nil {
		return 0, nil
	}
	packetDays := cfg.Retention.PacketDays
	if packetDays == 0 {
		return 0, nil
	}
	if packetDays < 0 {
		return 0, fmt.Errorf("ping score history: retention.packetDays is negative (%d) -- 0 means disabled, a negative value is not a valid retention window", packetDays)
	}
	if int64(packetDays) > pingScoreHistoryMaxRetentionPacketDays {
		return 0, fmt.Errorf("ping score history: retention.packetDays (%d) is too large to convert to a time.Duration without overflow (max %d)", packetDays, pingScoreHistoryMaxRetentionPacketDays)
	}
	return time.Duration(packetDays) * 24 * time.Hour, nil
}

// --- fase 5D: additional callback types ---------------------------------
//
// These are not among the six interfaces above (which mirror the
// already-existing engine/store surface); they're the plain injected
// callbacks the worker/lifecycle core uses to reach the outside world --
// every one of them is a function type so tests can inject a closure with
// zero adapter code, exactly like the six above.

// pingScoreHistoryEngineFactory constructs a pingScoreHistoryCycler from
// an already-open store. A production adapter (a later phase) closes over
// *Server, a time source and a pingScoreHistoryEngineConfig, and type-
// asserts store to *PingScoreHistoryStore before calling
// newPingScoreHistoryEngine -- this file never depends on either
// concrete type.
type pingScoreHistoryEngineFactory func(store pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error)

// pingScoreHistoryNewTicker constructs a fresh pingScoreHistoryTicker
// ticking every d. A production adapter (a later phase) wraps
// time.NewTicker; tests inject a constructor returning a fake backed by a
// plain channel.
type pingScoreHistoryNewTicker func(d time.Duration) pingScoreHistoryTicker

// pingScoreHistoryPublish publishes a freshly computed snapshot. A
// production adapter (a later phase) wraps s.pingScores.Store, which is
// already a no-op on nil (see pingScoresCache.Store in ping_scores.go) --
// this layer additionally guarantees it is never even called with nil.
type pingScoreHistoryPublish func(snap *PingScoresSnapshot)

// pingScoreHistorySetStatus reports a SANITIZED status: state (e.g.
// "initializing", "ok", "degraded", "read_only", "corrupt", "panic"), a
// short machine-readable code (e.g. "cycle_failed", ""  when state=="ok"), and
// the RFC3339 timestamp of the last successful cycle's GeneratedAt (""
// if none has ever succeeded yet). Never a raw error -- that is exactly
// what pingScoreHistoryErrorReporter is for. A production adapter (a
// later phase, healthz) stores this atomically for /api/healthz to read.
type pingScoreHistorySetStatus func(state, code, lastCycleAt string)

// --- fase 5D: single-owner worker core -----------------------------------

// runOneCycle runs exactly one Cycle() call and, on success, publishes
// the result and records the new lastSuccessfulCycleAt -- unless ctx is
// already done before Cycle() is even attempted, or becomes done while
// Cycle() is running. In the latter case (Model A: an in-flight Cycle()
// is never abandoned, and its store is never closed early -- see
// pingScoreHistoryWorkerCore/pingScoreHistoryAttempt), Cycle() is still
// let run to completion, but a result that raced the shutdown signal is
// never published and never recorded as the new lastSuccessfulCycleAt --
// the caller is shutting down and nothing will read status again anyway.
//
// A nil snapshot with a nil error (Cycle() should never do this, but
// nothing in its signature forbids it) is treated as a cycle failure,
// not a panic and not a silent no-op.
//
// On failure, status is reported as degraded/cycle_failed carrying
// *lastSuccessfulCycleAt UNCHANGED -- the last known good timestamp, not
// cleared -- so a transient failure never makes an already-published
// good snapshot look older or unknown than it is.
func runOneCycle(
	ctx context.Context,
	cycler pingScoreHistoryCycler,
	publish pingScoreHistoryPublish,
	setStatus pingScoreHistorySetStatus,
	reportError pingScoreHistoryErrorReporter,
	lastSuccessfulCycleAt *string,
) {
	if ctx.Err() != nil {
		return
	}

	snap, err := cycler.Cycle()

	if ctx.Err() != nil {
		return
	}

	if err != nil {
		setStatus("degraded", "cycle_failed", *lastSuccessfulCycleAt)
		reportError("cycle_failed", err)
		return
	}
	if snap == nil {
		setStatus("degraded", "cycle_failed", *lastSuccessfulCycleAt)
		reportError("cycle_failed", errors.New("ping score history cycle returned a nil snapshot with a nil error"))
		return
	}

	publish(snap)
	*lastSuccessfulCycleAt = snap.GeneratedAt
	setStatus("ok", "", *lastSuccessfulCycleAt)
}

// pingScoreHistoryWorkerCore is the single-owner sequential body run by
// exactly one goroutine for the lifetime of one successfully constructed
// engine: QuickSnapshot (best-effort, non-fatal) -> readOnly check -> one
// synchronous first Cycle -> ticker-paced Cycle loop. There is never more
// than one Cycle() in flight, because every Cycle() call -- the first one
// and every subsequent tick -- happens synchronously inline in this one
// goroutine's control flow; the next tick literally cannot be selected
// until the current runOneCycle call has returned.
//
// QuickSnapshot has three possible outcomes, all non-fatal to starting
// the worker -- state is always "initializing" (a status distinct from
// both "ok" and "degraded", since nothing about the WORKER itself has
// failed yet), but the code distinguishes what actually happened:
//   - it fails: reported via reportError("quick_snapshot_failed", err),
//     nothing is published (whatever handlePingScores' own nil-fallback
//     already shows keeps showing), status becomes "initializing"/
//     "quick_snapshot_failed" (LastCycleAt unchanged), and the worker
//     proceeds anyway.
//   - it succeeds with a snapshot: published immediately, so a restart
//     doesn't leave clients looking at an empty board for the full first
//     Cycle -- a snapshot built from already-loaded persisted state,
//     cheaper but not free (see QuickSnapshot's own doc comment in
//     ping_score_history_engine.go), not a substitute for a real Cycle.
//     Status becomes "initializing"/"" (empty code -- nothing failed).
//   - it returns (nil, nil) (QuickSnapshot's own contract says it never
//     does this, but nothing stops a future/faulty implementation):
//     nothing to publish, no error to report, status still becomes
//     "initializing"/"" exactly like the success case.
//
// Whichever of the three happened, this "initializing" status is only
// ever transient: runOneCycle's very next call (the first real Cycle,
// just below) unconditionally overwrites it with either "ok"/"" or
// "degraded"/"cycle_failed".
//
// If the store is read-only (a future-schema store opened by older
// code -- see PingScoreHistoryStore's own ReadOnly() doc comment),
// QuickSnapshot is still attempted (so a restart against a read-only
// store isn't worse off than one against a writable store), but no
// Cycle() is ever called and no ticker is ever constructed: a real
// Cycle() cannot publish anything against a read-only store (it fails at
// persistence and returns an error -- see Cycle()'s own step-by-step doc
// comment), so attempting one would just be a guaranteed cycle_failed on
// a loop, for no benefit. Status becomes "read_only"/"read_only" and the
// worker returns -- this snapshot is permanently frozen for this
// worker's lifetime, by design (see fase 5A v2's read-only decision).
//
// Cancellation is honored at every boundary: before QuickSnapshot is
// even attempted, before the first Cycle is attempted (runOneCycle's own
// leading ctx check), and via the ticker loop's select on ctx.Done().
// Once cancelled, the ticker (constructed only after the first Cycle) is
// always stopped via defer.
func pingScoreHistoryWorkerCore(
	ctx context.Context,
	cycler pingScoreHistoryCycler,
	readOnly bool,
	newTicker pingScoreHistoryNewTicker,
	interval time.Duration,
	publish pingScoreHistoryPublish,
	setStatus pingScoreHistorySetStatus,
	reportError pingScoreHistoryErrorReporter,
	lastSuccessfulCycleAt *string,
) {
	if ctx.Err() != nil {
		return
	}

	quick, err := cycler.QuickSnapshot()
	if err != nil {
		reportError("quick_snapshot_failed", err)
		setStatus("initializing", "quick_snapshot_failed", *lastSuccessfulCycleAt)
	} else {
		if quick != nil {
			publish(quick)
		}
		setStatus("initializing", "", *lastSuccessfulCycleAt)
	}

	if readOnly {
		setStatus("read_only", "read_only", *lastSuccessfulCycleAt)
		return
	}

	// The first Cycle runs synchronously, strictly before the ticker
	// exists -- runOneCycle's own leading ctx check means a shutdown
	// requested during/after QuickSnapshot but before this point
	// prevents it from ever starting.
	runOneCycle(ctx, cycler, publish, setStatus, reportError, lastSuccessfulCycleAt)

	if ctx.Err() != nil {
		return
	}

	ticker := newTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			// A failed tick is never terminal -- the loop keeps ticking
			// regardless of any single cycle's outcome.
			runOneCycle(ctx, cycler, publish, setStatus, reportError, lastSuccessfulCycleAt)
		}
	}
}

// --- fase 5D: single-attempt store ownership -----------------------------

// pingScoreHistoryAttemptResult classifies how pingScoreHistoryAttempt
// ended, for pingScoreHistoryLifecycleCore's retry decision.
type pingScoreHistoryAttemptResult int

const (
	// attemptDone means the lifecycle loop must not retry: either the
	// worker actually started (engine construction succeeded) and later
	// returned for ANY reason -- normal shutdown, read-only freeze, or a
	// panic that unwound past this attempt (in which case it will never
	// reach this return at all; see runPingScoreHistoryLifecycleRecovered) --
	// or a permanently terminal open failure was already handled by the
	// caller before an attempt was even made. Once a worker has started,
	// it owns its own lifetime; the lifecycle loop never second-guesses
	// that by reopening a new store and constructing a second engine.
	attemptDone pingScoreHistoryAttemptResult = iota
	// attemptRetryAfterWait means the store opened successfully but the
	// engine itself could not be constructed from it (engine_init_failed)
	// -- the worker never started, so it is safe (and necessary, since
	// this is the only path to a running worker) to back off and retry
	// with a freshly reopened store.
	attemptRetryAfterWait
)

// closeAndReport closes store and reports (never treats as a status
// change) any close error. Close failures are operationally interesting
// (a leaked fd, a stuck lock) but do not by themselves mean the data the
// worker already published is wrong, so they never touch setStatus.
func closeAndReport(store pingScoreHistoryStoreHandle, reportError pingScoreHistoryErrorReporter) {
	if err := store.Close(); err != nil {
		reportError("close_failed", err)
	}
}

// pingScoreHistoryAttempt owns store for exactly the duration of one
// attempt: it registers store's closure via defer BEFORE doing anything
// else, so store is closed no matter which return path is taken --
// including a panic unwinding through this frame (Go still runs this
// defer during unwind; see runPingScoreHistoryLifecycleRecovered) --
// and, critically, closed BEFORE this function returns to
// pingScoreHistoryLifecycleCore, which only performs its backoff wait
// AFTER pingScoreHistoryAttempt has fully returned. A store is thus never
// held open across a backoff sleep.
//
// If newEngine fails, the worker never starts: status becomes
// degraded/engine_init_failed, the full error is reported, and this
// attempt is classified attemptRetryAfterWait (with no wait performed
// here -- that is the lifecycle loop's job, after store is closed).
//
// If newEngine succeeds, control passes to pingScoreHistoryWorkerCore for
// the rest of this attempt's lifetime; whatever reason it eventually
// returns for, this attempt is unconditionally attemptDone -- the
// lifecycle loop never retries a worker that actually got to run.
func pingScoreHistoryAttempt(
	ctx context.Context,
	store pingScoreHistoryStoreHandle,
	newEngine pingScoreHistoryEngineFactory,
	newTicker pingScoreHistoryNewTicker,
	interval time.Duration,
	publish pingScoreHistoryPublish,
	setStatus pingScoreHistorySetStatus,
	reportError pingScoreHistoryErrorReporter,
	lastSuccessfulCycleAt *string,
) pingScoreHistoryAttemptResult {
	defer closeAndReport(store, reportError)

	cycler, err := newEngine(store)
	if err != nil {
		setStatus("degraded", "engine_init_failed", *lastSuccessfulCycleAt)
		reportError("engine_init_failed", err)
		return attemptRetryAfterWait
	}

	pingScoreHistoryWorkerCore(ctx, cycler, store.ReadOnly(), newTicker, interval, publish, setStatus, reportError, lastSuccessfulCycleAt)
	return attemptDone
}

// --- fase 5D: outer retry loop -------------------------------------------

// backoffPolicy computes doubling backoff delays capped at max, with an
// overflow guard: doubling a duration already close to time.Duration's
// maximum would otherwise wrap around to a small or negative value
// instead of saturating at max.
type backoffPolicy struct {
	initial time.Duration
	max     time.Duration
}

// next returns the delay to use after a failure, given the previous
// delay (0 for the very first failure).
func (b backoffPolicy) next(cur time.Duration) time.Duration {
	if cur <= 0 {
		return b.initial
	}
	n := cur * 2
	if n > b.max || n <= 0 {
		return b.max
	}
	return n
}

// pingScoreHistoryLifecycleCore is the outer retry loop: open the store,
// hand it to pingScoreHistoryAttempt for exactly one attempt, and --
// unless that attempt is attemptDone -- back off and retry with a freshly
// reopened store. It never validates production config (numeric fields,
// paths); that happens once, synchronously, before this loop's goroutine
// is even started, in a later thin production wrapper -- this core is
// purely the retry/backoff/classification machinery.
//
// open's error is classified via errors.As, matching the exact typed
// errors OpenPingScoreHistoryStore already returns (ping_score_history.go):
//   - *PingScoreHistoryCorruptError: terminal, and its OWN distinct
//     state -- "corrupt"/"corrupt", not "degraded" -- since this is not
//     a transient or retryable condition like the other failure classes
//     below, it is a permanently broken store. The file failed
//     PRAGMA integrity_check; nothing about waiting and reopening the
//     same bytes would ever fix that. The error is reported, and the
//     loop returns -- no retry, ever, no wait() call at all.
//   - *PingScoreHistoryMigrationError: an additive schema migration
//     failed partway; OpenPingScoreHistoryStore's own doc comment
//     guarantees this touches nothing on disk beyond what the failed
//     migration itself already did. Status becomes degraded/
//     migration_failed, the error is reported, and the loop falls
//     through to backoff-and-retry (a transient cause -- e.g. disk full
//     -- may resolve).
//   - anything else (a plain open/permission/lock error): status becomes
//     degraded/open_failed, reported, falls through to backoff-and-retry.
//
// A defensive case sits alongside the above: open returning (nil, nil)
// -- no error, but also no store. Nothing in pingScoreHistoryOpener's
// contract permits this, but a buggy or future implementation could still
// do it, and treating it as an ordinary "attempt" would crash on
// store.ReadOnly()/newEngine(store) with a nil receiver. It is instead
// classified exactly like a plain open error: status becomes
// degraded/open_failed, a clear synthetic error is reported, newEngine
// and Close are never called (there is no store to hand either one), and
// the loop falls through to the SAME backoff-and-retry as any other
// open_failed.
//
// The backoff wait only ever happens AFTER any store opened this
// iteration is already closed (pingScoreHistoryAttempt's defer has
// already run by the time it returns here) -- a store is never held open
// across a sleep. wait's own ctx-cancellation return short-circuits the
// loop immediately, so shutdown during a backoff sleep needs no real
// time to pass in tests.
func pingScoreHistoryLifecycleCore(
	ctx context.Context,
	open pingScoreHistoryOpener,
	path string,
	newEngine pingScoreHistoryEngineFactory,
	newTicker pingScoreHistoryNewTicker,
	interval time.Duration,
	backoff backoffPolicy,
	wait pingScoreHistoryWaiter,
	publish pingScoreHistoryPublish,
	setStatus pingScoreHistorySetStatus,
	reportError pingScoreHistoryErrorReporter,
) {
	lastSuccessfulCycleAt := new(string)
	delay := time.Duration(0)

	for {
		if ctx.Err() != nil {
			return
		}

		store, err := open(path)
		switch {
		case err != nil:
			var corruptErr *PingScoreHistoryCorruptError
			var migrationErr *PingScoreHistoryMigrationError
			switch {
			case errors.As(err, &corruptErr):
				setStatus("corrupt", "corrupt", *lastSuccessfulCycleAt)
				reportError("corrupt", err)
				return
			case errors.As(err, &migrationErr):
				setStatus("degraded", "migration_failed", *lastSuccessfulCycleAt)
				reportError("migration_failed", err)
			default:
				setStatus("degraded", "open_failed", *lastSuccessfulCycleAt)
				reportError("open_failed", err)
			}
		case store == nil:
			// Defensive: open returned (nil, nil). Never call newEngine or
			// Close on this -- there is no store -- just classify it as an
			// open failure and let the normal retry/backoff below handle it.
			setStatus("degraded", "open_failed", *lastSuccessfulCycleAt)
			reportError("open_failed", errors.New("ping score history: opener returned a nil store without an error"))
		default:
			result := pingScoreHistoryAttempt(ctx, store, newEngine, newTicker, interval, publish, setStatus, reportError, lastSuccessfulCycleAt)
			if result == attemptDone {
				return
			}
		}

		if ctx.Err() != nil {
			return
		}
		delay = backoff.next(delay)
		if wait(ctx, delay) {
			return
		}
	}
}

// --- fase 5D: panic-safe outermost wrapper -------------------------------

// pingScoreHistoryStatusSnapshot is the last (state, code, lastCycleAt)
// tuple pingScoreHistoryStatusRecorder observed. Reused unchanged as the
// Server-scoped, healthz-exposed status value (fase 5F, see
// setPingScoreHistoryStatus/pingScoreHistoryStatusView below and
// handleHealthz in healthz.go) -- the JSON tags below are load-bearing
// for that exposure and deliberately carry ONLY the sanitized
// state/code/timestamp strings pingScoreHistorySetStatus's own doc
// comment already promises: never a raw error, Detail, file path, or SQL
// fragment, because none of those ever get assigned to these fields in
// the first place (see runOneCycle/pingScoreHistoryLifecycleCore, which
// route anything like that through pingScoreHistoryErrorReporter
// instead).
type pingScoreHistoryStatusSnapshot struct {
	State       string `json:"state"`
	Code        string `json:"code"`
	LastCycleAt string `json:"lastCycleAt"`
}

// pingScoreHistoryStatusRecorder wraps a pingScoreHistorySetStatus
// callback and remembers the most recently reported status, purely so
// runPingScoreHistoryLifecycleRecovered's panic recovery can report the
// last-known LastCycleAt without any dependency on *Server or any other
// shared/global state -- a small, directly testable mechanism local to
// this file, per the fase 5D design constraint (prefer callbacks/fakes
// over a new *Server field in this phase). It needs no locking of its
// own: by construction (pingScoreHistoryWorkerCore's single-owner
// sequential design) every write happens on the one goroutine running the
// worker/lifecycle core, and the only read happens from that SAME
// goroutine's own deferred recover().
type pingScoreHistoryStatusRecorder struct {
	last pingScoreHistoryStatusSnapshot
}

// wrap returns a pingScoreHistorySetStatus that records every call before
// forwarding it unchanged to setStatus.
func (r *pingScoreHistoryStatusRecorder) wrap(setStatus pingScoreHistorySetStatus) pingScoreHistorySetStatus {
	return func(state, code, lastCycleAt string) {
		r.last = pingScoreHistoryStatusSnapshot{State: state, Code: code, LastCycleAt: lastCycleAt}
		setStatus(state, code, lastCycleAt)
	}
}

// runPingScoreHistoryLifecycleRecovered runs pingScoreHistoryLifecycleCore
// under a single outermost recover(). Go re-runs every deferred function
// at every stack frame during a panic's unwind, not just at the frame
// that eventually calls recover() -- so, on the way up through this
// single recover point, pingScoreHistoryWorkerCore's own
// `defer ticker.Stop()` fires FIRST (that frame is nested inside
// pingScoreHistoryAttempt's call), and only then, as the unwind
// continues outward, does pingScoreHistoryAttempt's own
// `defer closeAndReport(store, ...)` fire, closing the history store.
// No per-layer recover is needed anywhere else in this file.
//
// A panic here is terminal for this worker: it is reported via
// reportError("panic", ...) with a full stack trace, status becomes
// panic/panic carrying the LAST KNOWN lastSuccessfulCycleAt (via the
// local statusRecorder above, not any *Server field), and this function
// then returns normally -- it does NOT re-panic (so the panic is not
// fatal to the whole server process) and it does NOT restart the worker
// with the same engine (a later production wrapper deciding whether/how
// to ever call this function again is a separate, explicit choice, out
// of scope here). Returning normally also means that if a future caller
// wraps this call with `defer close(done)`, that close always still
// happens, so nothing can be left waiting on it forever.
func runPingScoreHistoryLifecycleRecovered(
	ctx context.Context,
	open pingScoreHistoryOpener,
	path string,
	newEngine pingScoreHistoryEngineFactory,
	newTicker pingScoreHistoryNewTicker,
	interval time.Duration,
	backoff backoffPolicy,
	wait pingScoreHistoryWaiter,
	publish pingScoreHistoryPublish,
	setStatus pingScoreHistorySetStatus,
	reportError pingScoreHistoryErrorReporter,
) {
	recorder := &pingScoreHistoryStatusRecorder{}
	wrappedSetStatus := recorder.wrap(setStatus)

	defer func() {
		if r := recover(); r != nil {
			reportError("panic", fmt.Errorf("ping score history worker panicked: %v\n%s", r, debug.Stack()))
			setStatus("panic", "panic", recorder.last.LastCycleAt)
		}
	}()

	pingScoreHistoryLifecycleCore(ctx, open, path, newEngine, newTicker, interval, backoff, wait, publish, wrappedSetStatus, reportError)
}

// --- fase 5F: Server-scoped healthz status -------------------------------
//
// Additive-only: this section adds a way to READ/WRITE a status on
// *Server and to SHOW it in /api/healthz. Nothing in fase 5F itself
// started the worker or called setPingScoreHistoryStatus in production --
// that wiring is fase 5G, immediately below. Before fase 5G's
// StartPingScoreHistoryEngine has ever been called (e.g. in a unit test
// building its own *Server), pingScoreHistoryStatusView's zero-value
// default ("initializing") is exactly what /api/healthz reports.

// setPingScoreHistoryStatus stores a complete pingScoreHistoryStatusSnapshot
// with a single atomic.Value.Store call, matching pingScoreHistorySetStatus's
// own signature so a later production adapter can pass this method
// directly wherever a pingScoreHistorySetStatus callback is expected.
// State, Code, and LastCycleAt are always written together as one
// value -- a concurrent reader can never observe a torn mix of fields
// from two different calls.
func (s *Server) setPingScoreHistoryStatus(state, code, lastCycleAt string) {
	s.pingScoreHistoryStatus.Store(pingScoreHistoryStatusSnapshot{
		State:       state,
		Code:        code,
		LastCycleAt: lastCycleAt,
	})
}

// pingScoreHistoryStatusView returns the most recently stored ping-score-
// history status. Safe to call on a zero-value *Server.pingScoreHistoryStatus
// (e.g. a test fixture built as &Server{}, or before any fase 5 worker
// wiring has ever called the setter): atomic.Value.Load on a field that
// was never Store'd returns a nil interface{}, not a panic, and that nil
// case is exactly what maps to the documented "initializing"/""/""
// default here.
func (s *Server) pingScoreHistoryStatusView() pingScoreHistoryStatusSnapshot {
	v := s.pingScoreHistoryStatus.Load()
	if v == nil {
		return pingScoreHistoryStatusSnapshot{State: "initializing"}
	}
	return v.(pingScoreHistoryStatusSnapshot)
}

// --- fase 5G: production adapters + Start wrapper ------------------------
//
// This is the first phase allowed to actually start the worker. Every
// adapter below is a thin, directly-testable wrapper around a single
// real dependency (a *time.Ticker, a *time.Timer, OpenPingScoreHistoryStore,
// newPingScoreHistoryEngine, s.pingScores, s.setPingScoreHistoryStatus, or
// log.Printf) -- none of them contain any lifecycle/retry logic of their
// own, which all still lives entirely in fase 5D's core above. Adapters
// with no state to close over (A, B, C, G) are plain package-level
// functions, directly assignable to their respective callback types with
// no wrapper needed; adapters that must close over *Server/engineConfig
// (D, E) are small constructors returning the closure.

// A. Real ticker adapter -- wraps *time.Ticker.
type pingScoreHistoryRealTicker struct{ t *time.Ticker }

func (r pingScoreHistoryRealTicker) C() <-chan time.Time { return r.t.C }
func (r pingScoreHistoryRealTicker) Stop()               { r.t.Stop() }

// pingScoreHistoryRealNewTicker is a pingScoreHistoryNewTicker backed by a
// real *time.Ticker. d must already have been validated > 0 by the
// caller (validatePingScoreHistoryStartConfig, synchronously, before any
// goroutine starts) -- time.NewTicker itself panics on d<=0, and this
// function performs no validation of its own.
func pingScoreHistoryRealNewTicker(d time.Duration) pingScoreHistoryTicker {
	return pingScoreHistoryRealTicker{t: time.NewTicker(d)}
}

// B. Real cancellable waiter -- waits for d or ctx.Done(), whichever
// comes first. Uses time.NewTimer (not a bare time.After in a select)
// so the timer is always explicitly Stop()'d rather than left to fire
// uselessly into an unread channel until it naturally elapses -- worth
// doing here specifically since backoff delays can be tens of minutes
// (see pingScoreHistoryProductionBackoff below). A fresh *time.Timer is
// created on every call and never reused/Reset, so there is no need to
// drain timer.C after Stop() -- the classic Reset-without-draining
// hazard doesn't apply to a single-shot, never-reused timer.
func pingScoreHistoryRealWaiter(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer.C:
		return false
	}
}

// C. Production opener -- wraps OpenPingScoreHistoryStore. Explicitly
// returns a nil INTERFACE value (not a nil *PingScoreHistoryStore boxed
// into a non-nil interface -- the classic Go typed-nil hazard) on error,
// even though pingScoreHistoryLifecycleCore's error-first switch never
// actually inspects store when err != nil, purely so this adapter is
// correct in isolation, not just correct given today's caller.
func pingScoreHistoryRealOpener(path string) (pingScoreHistoryStoreHandle, error) {
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// D. Production engine factory -- closes over server and engineConfig,
// using time.Now as the engine's clock. The production opener above is
// the ONLY thing that ever produces the store handle this factory
// receives, and it always produces a *PingScoreHistoryStore -- but this
// still type-asserts rather than blindly casting, returning a clear,
// descriptive error (not a panic) if some other pingScoreHistoryStoreHandle
// implementation ever reaches this adapter (e.g. a future refactor, or a
// miswired test that meant to use a fake). No separate engine_init_failed
// classification is needed here: pingScoreHistoryAttempt (fase 5D)
// already classifies ANY error this factory returns that way.
func pingScoreHistoryRealEngineFactory(server *Server, engineConfig pingScoreHistoryEngineConfig) pingScoreHistoryEngineFactory {
	return func(handle pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) {
		store, ok := handle.(*PingScoreHistoryStore)
		if !ok {
			return nil, fmt.Errorf("ping score history: engine factory: unexpected store handle type %T (want *PingScoreHistoryStore)", handle)
		}
		return newPingScoreHistoryEngine(server, store, time.Now, engineConfig)
	}
}

// E. Production publisher -- stores into s.pingScores, the SAME cache
// handlePingScores already reads (see routes.go and ping_scores.go).
// pingScoresCache.Store is itself already a no-op on a nil snapshot; the
// core (runOneCycle/pingScoreHistoryWorkerCore) additionally guarantees
// this is never even called with nil, so both layers agree.
func pingScoreHistoryRealPublisher(server *Server) pingScoreHistoryPublish {
	return func(snap *PingScoresSnapshot) {
		server.pingScores.Store(snap)
	}
}

// F. Production status -- *Server.setPingScoreHistoryStatus (fase 5F)
// already has exactly the pingScoreHistorySetStatus signature; it is
// passed directly as a method value wherever this is needed, with no
// adapter function required.

// G. Production error reporter -- the ONLY place any FULL internal error
// detail (which may contain file paths, SQL fragments, or a panic's
// stack trace -- see runPingScoreHistoryLifecycleRecovered's own doc
// comment) is ever written anywhere. /api/healthz (fase 5F) only ever
// sees the sanitized state/code/lastCycleAt tuple via
// pingScoreHistorySetStatus -- never routed through this function, and
// this function never touches status.
func pingScoreHistoryRealReportError(code string, err error) {
	log.Printf("[ping-scores-history] %s: %v", code, err)
}

// pingScoreHistoryProductionBackoff is the ONE place the production
// backoff policy is defined: 1 minute initial, doubling (see
// backoffPolicy.next), capped at 30 minutes. A function rather than a
// package-level var, so there is no mutable global for anything to
// accidentally reassign -- every call just returns the same value.
func pingScoreHistoryProductionBackoff() backoffPolicy {
	return backoffPolicy{initial: time.Minute, max: 30 * time.Minute}
}

// validatePingScoreHistoryStartConfig performs ALL synchronous, pre-
// goroutine validation for StartPingScoreHistoryEngine. A permanently-
// invalid configuration must fail immediately here, return a plain
// error, and never start a goroutine or fall into an async retry loop
// (see fase 5A v5's explicit requirement on this exact point -- an
// invalid engineConfig must not end in an infinite async retry).
//
// The engineConfig checks intentionally mirror newPingScoreHistoryEngine's
// own validation exactly (SettleDebounce/DeepSweepBatchSize/RetentionDuration
// must be >= 0; MaxEdgeKm <= 0 is a deliberate, valid "disable the
// geo-filter" convention and is never rejected) -- catching the same
// problem here means a bad config fails at Start time with a clear
// synchronous error, not many minutes later buried in a cycle_failed log
// line from the first opened attempt.
func validatePingScoreHistoryStartConfig(
	s *Server,
	historyPath string,
	engineConfig pingScoreHistoryEngineConfig,
	interval time.Duration,
	backoff backoffPolicy,
) error {
	if s == nil {
		return fmt.Errorf("ping score history: start: server is nil")
	}
	if strings.TrimSpace(historyPath) == "" {
		return fmt.Errorf("ping score history: start: historyPath is empty")
	}
	if interval <= 0 {
		return fmt.Errorf("ping score history: start: interval must be positive, got %s", interval)
	}
	if backoff.initial <= 0 {
		return fmt.Errorf("ping score history: start: backoff.initial must be positive, got %s", backoff.initial)
	}
	if backoff.max < backoff.initial {
		return fmt.Errorf("ping score history: start: backoff.max (%s) must be >= backoff.initial (%s)", backoff.max, backoff.initial)
	}
	if engineConfig.SettleDebounce < 0 {
		return fmt.Errorf("ping score history: start: engineConfig.SettleDebounce is negative (%s)", engineConfig.SettleDebounce)
	}
	if engineConfig.DeepSweepBatchSize < 0 {
		return fmt.Errorf("ping score history: start: engineConfig.DeepSweepBatchSize is negative (%d)", engineConfig.DeepSweepBatchSize)
	}
	if engineConfig.RetentionDuration < 0 {
		return fmt.Errorf("ping score history: start: engineConfig.RetentionDuration is negative (%s)", engineConfig.RetentionDuration)
	}
	return nil
}

// startPingScoreHistoryEngineWithDependencies is StartPingScoreHistoryEngine's
// fully-injectable core: every dependency the lifecycle needs is a plain
// parameter, so tests can exercise Start/Stop semantics (single-goroutine,
// double/concurrent Stop safety, Stop waiting for an active Cycle, panic
// recovery, Start never blocking on the first Cycle) with the SAME
// deterministic fakes fase 5D's own tests already use, with no wall-clock
// waiting anywhere. StartPingScoreHistoryEngine below is the thin,
// production-only entry point that supplies the real adapters (A-G)
// above.
func startPingScoreHistoryEngineWithDependencies(
	s *Server,
	historyPath string,
	engineConfig pingScoreHistoryEngineConfig,
	interval time.Duration,
	backoff backoffPolicy,
	open pingScoreHistoryOpener,
	newEngine pingScoreHistoryEngineFactory,
	newTicker pingScoreHistoryNewTicker,
	wait pingScoreHistoryWaiter,
	publish pingScoreHistoryPublish,
	setStatus pingScoreHistorySetStatus,
	reportError pingScoreHistoryErrorReporter,
) (stop func(), err error) {
	if err := validatePingScoreHistoryStartConfig(s, historyPath, engineConfig, interval, backoff); err != nil {
		return nil, err
	}

	setStatus("initializing", "", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		runPingScoreHistoryLifecycleRecovered(ctx, open, historyPath, newEngine, newTicker, interval, backoff, wait, publish, setStatus, reportError)
	}()

	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
	return stop, nil
}

// StartPingScoreHistoryEngine is the production Start wrapper around the
// approved fase 5A/5D worker/lifecycle core: it validates synchronously,
// sets the initial "initializing" status synchronously, then starts
// EXACTLY ONE goroutine running runPingScoreHistoryLifecycleRecovered
// with the real (production) adapters (A-G) above, and returns
// immediately -- it never waits for QuickSnapshot or a first Cycle, so
// the caller's HTTP listener is never blocked by this call (the exact
// problem this whole redesign replaces -- see StartPingScoresRecomputer's
// own doc comment in ping_scores.go for what the OLD synchronous
// behavior was, and why it stays in place, untouched, as tested rollback
// code rather than being deleted in this phase).
//
// stop is sync.Once-safe: calling it more than once, or concurrently
// from multiple goroutines, is safe, and every call returns only once
// the worker has FULLY shut down (Model A: an unbounded wait, no
// internal timeout -- see fase 5A v2's explicit rejection of a
// timeout-then-close-resources-anyway approach). Cancellation during an
// active steady-state Cycle unwinds in order: the active Cycle finishes
// naturally, the ticker is stopped (pingScoreHistoryWorkerCore's own
// defer), the history store is closed (pingScoreHistoryAttempt's own
// defer), any panic is caught by the single outermost recover, and only
// then does the lifecycle goroutine return and `done` close. If
// cancellation instead lands during the very first Cycle, the ticker was
// never constructed yet, so that step simply doesn't apply -- the store
// is still closed before `done` closes. There is no legacy-recomputer
// fallback anywhere in this call chain.
func StartPingScoreHistoryEngine(
	s *Server,
	historyPath string,
	engineConfig pingScoreHistoryEngineConfig,
	interval time.Duration,
	backoff backoffPolicy,
) (stop func(), err error) {
	return startPingScoreHistoryEngineWithDependencies(
		s, historyPath, engineConfig, interval, backoff,
		pingScoreHistoryRealOpener,
		pingScoreHistoryRealEngineFactory(s, engineConfig),
		pingScoreHistoryRealNewTicker,
		pingScoreHistoryRealWaiter,
		pingScoreHistoryRealPublisher(s),
		s.setPingScoreHistoryStatus,
		pingScoreHistoryRealReportError,
	)
}
