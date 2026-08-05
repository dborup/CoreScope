package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- fase 5D test infrastructure -----------------------------------------
//
// Every fake below is deterministic: no time.Sleep anywhere, no mutable
// package-globals. Rendezvous points (unbuffered/gated channels) and
// explicit signal channels replace wall-clock waits, and every recorder
// is its own small mutex-protected type rather than a shared global.

// eventLog is a mutex-protected append-only log, used to prove ordering
// (e.g. "store closed before the backoff wait began") across goroutines.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) record(s string) {
	l.mu.Lock()
	l.events = append(l.events, s)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

// waitFor blocks until ch yields a value (works for both a closed "done"
// channel and a buffered one-shot signal channel), or fails the test
// after a generous timeout -- a safety net against a hung test, not a
// substitute for real synchronization.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// statusRecorderSpy records every pingScoreHistorySetStatus call in order.
// setStatus is called exactly once by every runOneCycle invocation
// (success, failure, or nil-snapshot) and is always preceded by that
// call's publish (when there is one) -- so notify, if set, is a reliable
// "one full runOneCycle call has finished" signal for tests to drain,
// with no dependence on timing inside the fake cycler itself.
type statusRecorderSpy struct {
	mu     sync.Mutex
	calls  []pingScoreHistoryStatusSnapshot
	notify chan struct{} // optional; buffered non-blocking signal after each record
}

func (s *statusRecorderSpy) record(state, code, lastCycleAt string) {
	s.mu.Lock()
	s.calls = append(s.calls, pingScoreHistoryStatusSnapshot{State: state, Code: code, LastCycleAt: lastCycleAt})
	s.mu.Unlock()
	if s.notify != nil {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// waitForStatusCalls drains n notify signals from a statusRecorderSpy
// created with a buffered notify channel, proving n setStatus calls (and
// everything that necessarily preceded each of them within runOneCycle,
// e.g. its publish call) have completed.
func waitForStatusCalls(t *testing.T, notify chan struct{}, n int, what string) {
	t.Helper()
	for i := 0; i < n; i++ {
		waitFor(t, notify, what)
	}
}

func (s *statusRecorderSpy) all() []pingScoreHistoryStatusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pingScoreHistoryStatusSnapshot, len(s.calls))
	copy(out, s.calls)
	return out
}

func (s *statusRecorderSpy) last() pingScoreHistoryStatusSnapshot {
	all := s.all()
	if len(all) == 0 {
		return pingScoreHistoryStatusSnapshot{}
	}
	return all[len(all)-1]
}

// errorCall is one recorded pingScoreHistoryErrorReporter call.
type errorCall struct {
	code string
	err  error
}

type errorRecorderSpy struct {
	mu    sync.Mutex
	calls []errorCall
}

func (e *errorRecorderSpy) record(code string, err error) {
	e.mu.Lock()
	e.calls = append(e.calls, errorCall{code: code, err: err})
	e.mu.Unlock()
}

func (e *errorRecorderSpy) all() []errorCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]errorCall, len(e.calls))
	copy(out, e.calls)
	return out
}

func (e *errorRecorderSpy) hasCode(code string) bool {
	for _, c := range e.all() {
		if c.code == code {
			return true
		}
	}
	return false
}

// publishRecorderSpy records every pingScoreHistoryPublish call in order.
type publishRecorderSpy struct {
	mu    sync.Mutex
	calls []*PingScoresSnapshot
}

func (p *publishRecorderSpy) record(snap *PingScoresSnapshot) {
	p.mu.Lock()
	p.calls = append(p.calls, snap)
	p.mu.Unlock()
}

func (p *publishRecorderSpy) all() []*PingScoresSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*PingScoresSnapshot, len(p.calls))
	copy(out, p.calls)
	return out
}

// snap is a small helper building an identifiable snapshot -- GeneratedAt
// doubles as the test's identity marker for "which snapshot was this".
func snap(id string) *PingScoresSnapshot {
	return &PingScoresSnapshot{GeneratedAt: id, TotalPings: 1}
}

// cycleResult is one canned Cycle() outcome consumed in order by
// fakeCycler; the last entry repeats once the queue is exhausted.
type cycleResult struct {
	snap *PingScoresSnapshot
	err  error
}

// fakeCycler implements pingScoreHistoryCycler deterministically:
// QuickSnapshot returns one canned result; Cycle consumes a queue of
// canned results in order, optionally rendezvousing on entry (entered)
// and blocking until released (gate) so a test can hold a Cycle call
// open, and optionally panicking instead of returning. running/
// overlapDetected structurally prove no two Cycle calls are ever
// in flight at once.
type fakeCycler struct {
	quickResult *PingScoresSnapshot
	quickErr    error

	mu           sync.Mutex
	cycleResults []cycleResult
	cycleCalls   int
	quickCalls   int

	entered    chan struct{} // signaled (non-blocking send skipped if nil) when Cycle is entered
	gate       chan struct{} // if non-nil, Cycle blocks here after signaling entered
	panicValue interface{}   // if non-nil, Cycle panics with this instead of returning

	running         int32
	overlapDetected int32
	eventLog        *eventLog
}

func (f *fakeCycler) QuickSnapshot() (*PingScoresSnapshot, error) {
	f.mu.Lock()
	f.quickCalls++
	f.mu.Unlock()
	if f.eventLog != nil {
		f.eventLog.record("quick_snapshot")
	}
	return f.quickResult, f.quickErr
}

func (f *fakeCycler) Cycle() (*PingScoresSnapshot, error) {
	if !atomic.CompareAndSwapInt32(&f.running, 0, 1) {
		atomic.StoreInt32(&f.overlapDetected, 1)
	} else {
		defer atomic.StoreInt32(&f.running, 0)
	}

	f.mu.Lock()
	idx := f.cycleCalls
	f.cycleCalls++
	f.mu.Unlock()

	if f.eventLog != nil {
		f.eventLog.record("cycle_start")
	}

	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}

	if f.panicValue != nil {
		panic(f.panicValue)
	}

	f.mu.Lock()
	var result cycleResult
	if len(f.cycleResults) > 0 {
		i := idx
		if i >= len(f.cycleResults) {
			i = len(f.cycleResults) - 1
		}
		result = f.cycleResults[i]
	}
	f.mu.Unlock()

	if f.eventLog != nil {
		f.eventLog.record("cycle_end")
	}
	return result.snap, result.err
}

func (f *fakeCycler) callCounts() (quick, cycle int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quickCalls, f.cycleCalls
}

// fakeStoreHandle implements pingScoreHistoryStoreHandle.
type fakeStoreHandle struct {
	readOnly bool
	closeErr error

	mu         sync.Mutex
	closeCount int
	eventLog   *eventLog
}

func (f *fakeStoreHandle) ReadOnly() bool { return f.readOnly }

func (f *fakeStoreHandle) Close() error {
	f.mu.Lock()
	f.closeCount++
	f.mu.Unlock()
	if f.eventLog != nil {
		f.eventLog.record("store_close")
	}
	return f.closeErr
}

func (f *fakeStoreHandle) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCount
}

// fakeTicker implements pingScoreHistoryTicker over a plain channel the
// test controls directly -- no real timer involved.
type fakeTicker struct {
	ch chan time.Time

	mu      sync.Mutex
	stopped bool
}

func (f *fakeTicker) C() <-chan time.Time { return f.ch }

func (f *fakeTicker) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}

func (f *fakeTicker) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// newTickerSignaling returns a pingScoreHistoryNewTicker that hands back
// ft and, on every call, sends (non-blocking) to constructed -- tests use
// this to know precisely when the worker core has finished its first
// Cycle and moved on to steady-state ticking, without any sleep.
func newTickerSignaling(ft *fakeTicker, constructed chan struct{}) pingScoreHistoryNewTicker {
	return func(time.Duration) pingScoreHistoryTicker {
		select {
		case constructed <- struct{}{}:
		default:
		}
		return ft
	}
}

// failIfCalledNewTicker fails the test if the worker core ever tries to
// construct a ticker -- used to prove a code path (read-only, an
// already-cancelled context) never reaches that point.
func failIfCalledNewTicker(t *testing.T) pingScoreHistoryNewTicker {
	return func(time.Duration) pingScoreHistoryTicker {
		t.Fatal("newTicker must not be called on this path")
		return nil
	}
}

// failIfCalledOpener fails the test if the lifecycle core ever tries to
// open a store -- used to prove the loop never even reaches open() (e.g.
// ctx already cancelled before the first iteration).
func failIfCalledOpener(t *testing.T) pingScoreHistoryOpener {
	return func(string) (pingScoreHistoryStoreHandle, error) {
		t.Fatal("open must not be called on this path")
		return nil, nil
	}
}

// failIfCalledWaiter fails the test if the lifecycle core ever waits --
// used to prove a terminal classification (corrupt) never retries.
func failIfCalledWaiter(t *testing.T) pingScoreHistoryWaiter {
	return func(context.Context, time.Duration) bool {
		t.Fatal("wait must not be called on this path")
		return false
	}
}

// startWorkerCore launches pingScoreHistoryWorkerCore in its own
// goroutine (it can block indefinitely in its ticker select loop) and
// returns a channel closed when it returns.
func startWorkerCore(
	ctx context.Context,
	cycler pingScoreHistoryCycler,
	readOnly bool,
	newTicker pingScoreHistoryNewTicker,
	pub *publishRecorderSpy,
	status *statusRecorderSpy,
	errs *errorRecorderSpy,
	lastSuccessfulCycleAt *string,
) chan struct{} {
	done := make(chan struct{})
	go func() {
		pingScoreHistoryWorkerCore(ctx, cycler, readOnly, newTicker, time.Minute, pub.record, status.record, errs.record, lastSuccessfulCycleAt)
		close(done)
	}()
	return done
}

// --- Worker tests (pingScoreHistoryWorkerCore / runOneCycle) -------------

// 1. QuickSnapshot is published before the first Cycle runs.
func TestWorkerCore_QuickSnapshotPublishedBeforeFirstCycle(t *testing.T) {
	cycler := &fakeCycler{quickResult: snap("quick"), cycleResults: []cycleResult{{snap: snap("cycle1")}}}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, new(string))
	waitFor(t, constructed, "ticker construction")

	calls := pub.all()
	if len(calls) != 2 || calls[0].GeneratedAt != "quick" || calls[1].GeneratedAt != "cycle1" {
		t.Fatalf("publish order = %+v, want [quick, cycle1]", calls)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 2. A QuickSnapshot failure is non-fatal: it's reported, nothing is
// published for it, and the worker still proceeds to its first Cycle.
func TestWorkerCore_QuickSnapshotFailure_NonFatalWorkerProceeds(t *testing.T) {
	cycler := &fakeCycler{quickErr: errors.New("quick boom"), cycleResults: []cycleResult{{snap: snap("cycle1")}}}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, new(string))
	waitFor(t, constructed, "ticker construction")

	if !errs.hasCode("quick_snapshot_failed") {
		t.Errorf("errs = %+v, want a quick_snapshot_failed entry", errs.all())
	}
	calls := pub.all()
	if len(calls) != 1 || calls[0].GeneratedAt != "cycle1" {
		t.Fatalf("publish calls = %+v, want exactly [cycle1]", calls)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 3. A defensive case: QuickSnapshot returns (nil, nil). Nothing is
// published for it (nothing to publish) and no error is reported, but the
// worker still proceeds normally.
func TestWorkerCore_QuickSnapshotNilNil_DefensiveNoPublish(t *testing.T) {
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("cycle1")}}}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, new(string))
	waitFor(t, constructed, "ticker construction")

	if len(errs.all()) != 0 {
		t.Errorf("errs = %+v, want none", errs.all())
	}
	calls := pub.all()
	if len(calls) != 1 || calls[0].GeneratedAt != "cycle1" {
		t.Fatalf("publish calls = %+v, want exactly [cycle1]", calls)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 4. Status becomes "initializing" right after QuickSnapshot, strictly
// before any ok/degraded status from the first Cycle.
func TestWorkerCore_StatusInitializing_SetBeforeFirstCycle(t *testing.T) {
	cycler := &fakeCycler{quickResult: snap("quick"), cycleResults: []cycleResult{{snap: snap("cycle1")}}}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, new(string))
	waitFor(t, constructed, "ticker construction")

	all := status.all()
	if len(all) < 2 {
		t.Fatalf("status calls = %+v, want at least 2", all)
	}
	if all[0].State != "initializing" {
		t.Errorf("status[0] = %+v, want state=initializing", all[0])
	}
	if all[1].State != "ok" {
		t.Errorf("status[1] = %+v, want state=ok (from the first Cycle)", all[1])
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 5. Against a read-only store, QuickSnapshot is attempted (and
// published if it succeeds), but Cycle is never called and no ticker is
// ever constructed -- status ends at read_only/read_only.
func TestWorkerCore_ReadOnly_QuickSnapshotAttemptedNoCycleNoTicker(t *testing.T) {
	cycler := &fakeCycler{quickResult: snap("quick"), cycleResults: []cycleResult{{snap: snap("should-not-run")}}}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}

	done := startWorkerCore(context.Background(), cycler, true, failIfCalledNewTicker(t), pub, status, errs, new(string))
	waitFor(t, done, "worker core shutdown (read-only returns promptly)")

	if quick, cycle := cycler.callCounts(); quick != 1 || cycle != 0 {
		t.Fatalf("callCounts = (quick=%d, cycle=%d), want (1, 0)", quick, cycle)
	}
	calls := pub.all()
	if len(calls) != 1 || calls[0].GeneratedAt != "quick" {
		t.Fatalf("publish calls = %+v, want exactly [quick]", calls)
	}
	if last := status.last(); last.State != "read_only" || last.Code != "read_only" {
		t.Errorf("status.last() = %+v, want {read_only read_only}", last)
	}
}

// 6. The first Cycle completes strictly before the ticker is
// constructed -- proven via eventLog ordering, not just call counts.
func TestWorkerCore_FirstCycleStrictlyBeforeTickerConstruction(t *testing.T) {
	elog := &eventLog{}
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("cycle1")}}, eventLog: elog}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)
	newTicker := func(time.Duration) pingScoreHistoryTicker {
		elog.record("ticker_constructed")
		select {
		case constructed <- struct{}{}:
		default:
		}
		return ft
	}

	done := startWorkerCore(ctx, cycler, false, newTicker, pub, status, errs, new(string))
	waitFor(t, constructed, "ticker construction")

	events := elog.snapshot()
	cycleEndIdx, tickerIdx := -1, -1
	for i, e := range events {
		switch e {
		case "cycle_end":
			if cycleEndIdx == -1 {
				cycleEndIdx = i
			}
		case "ticker_constructed":
			tickerIdx = i
		}
	}
	if cycleEndIdx == -1 || tickerIdx == -1 || cycleEndIdx > tickerIdx {
		t.Fatalf("events = %v, want the first cycle_end strictly before ticker_constructed", events)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 7. A successful Cycle publishes the snapshot and reports ok status
// carrying the snapshot's own GeneratedAt as LastCycleAt.
func TestWorkerCore_CycleSuccess_PublishesAndSetsOkStatus(t *testing.T) {
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("cycle-X")}}}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)
	lastGood := new(string)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, lastGood)
	waitFor(t, constructed, "ticker construction")

	calls := pub.all()
	if len(calls) != 1 || calls[0].GeneratedAt != "cycle-X" {
		t.Fatalf("publish calls = %+v, want exactly [cycle-X]", calls)
	}
	if last := status.last(); last.State != "ok" || last.Code != "" || last.LastCycleAt != "cycle-X" {
		t.Errorf("status.last() = %+v, want {ok  cycle-X}", last)
	}
	if *lastGood != "cycle-X" {
		t.Errorf("*lastSuccessfulCycleAt = %q, want cycle-X", *lastGood)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 8. A later Cycle failure never publishes anything new and reports
// degraded/cycle_failed while PRESERVING the last known good snapshot's
// timestamp -- LastCycleAt must not be cleared or advanced by a failure.
func TestWorkerCore_CycleFailure_PreservesLastGoodStatusAndSnapshot(t *testing.T) {
	cycler := &fakeCycler{
		cycleResults: []cycleResult{{snap: snap("A")}, {err: errors.New("cycle boom")}},
	}
	pub := &publishRecorderSpy{}
	status := &statusRecorderSpy{notify: make(chan struct{}, 64)}
	errs := &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)
	lastGood := new(string)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, lastGood)
	waitFor(t, constructed, "ticker construction")
	waitForStatusCalls(t, status.notify, 2, "initializing + cycle 1 status") // both already queued by the time the ticker exists

	if last := status.last(); last.State != "ok" || last.LastCycleAt != "A" {
		t.Fatalf("after cycle 1, status.last() = %+v, want {ok ... A}", last)
	}

	ft.ch <- time.Now()
	waitForStatusCalls(t, status.notify, 1, "cycle 2 (failure) status")

	if calls := pub.all(); len(calls) != 1 {
		t.Fatalf("publish calls = %+v, want still exactly [A] (no new publish on failure)", calls)
	}
	if last := status.last(); last.State != "degraded" || last.Code != "cycle_failed" || last.LastCycleAt != "A" {
		t.Errorf("status.last() = %+v, want {degraded cycle_failed A}", last)
	}
	if !errs.hasCode("cycle_failed") {
		t.Errorf("errs = %+v, want a cycle_failed entry", errs.all())
	}
	if *lastGood != "A" {
		t.Errorf("*lastSuccessfulCycleAt = %q, want still A", *lastGood)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 9. Cycle() returning (nil, nil) is treated as a cycle failure -- never
// a panic and never a silent no-op.
func TestWorkerCore_CycleNilNil_TreatedAsFailureNotPanic(t *testing.T) {
	cycler := &fakeCycler{cycleResults: []cycleResult{{}}} // zero-value cycleResult: nil snap, nil err
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, new(string))
	waitFor(t, constructed, "ticker construction")

	if calls := pub.all(); len(calls) != 0 {
		t.Fatalf("publish calls = %+v, want none", calls)
	}
	if last := status.last(); last.State != "degraded" || last.Code != "cycle_failed" {
		t.Errorf("status.last() = %+v, want {degraded cycle_failed ...}", last)
	}
	if !errs.hasCode("cycle_failed") {
		t.Errorf("errs = %+v, want a cycle_failed entry for the nil/nil result", errs.all())
	}

	cancel()
	waitFor(t, done, "worker core shutdown (no panic)")
}

// 10. A failed tick never stops the loop: subsequent ticks keep calling
// Cycle, and a later success is published and reported normally.
func TestWorkerCore_TickerLoop_FailedCycleNonTerminal_KeepsTicking(t *testing.T) {
	cycler := &fakeCycler{
		cycleResults: []cycleResult{{snap: snap("A")}, {err: errors.New("boom")}, {snap: snap("B")}},
	}
	pub := &publishRecorderSpy{}
	status := &statusRecorderSpy{notify: make(chan struct{}, 64)}
	errs := &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 2)}
	constructed := make(chan struct{}, 1)
	lastGood := new(string)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, lastGood)
	waitFor(t, constructed, "ticker construction")
	waitForStatusCalls(t, status.notify, 2, "initializing + cycle 1 (A) status")

	ft.ch <- time.Now()
	waitForStatusCalls(t, status.notify, 1, "cycle 2 (failure) status")
	ft.ch <- time.Now()
	waitForStatusCalls(t, status.notify, 1, "cycle 3 (B) status")

	calls := pub.all()
	if len(calls) != 2 || calls[0].GeneratedAt != "A" || calls[1].GeneratedAt != "B" {
		t.Fatalf("publish calls = %+v, want [A, B] (failure skipped, loop kept going)", calls)
	}
	if last := status.last(); last.State != "ok" || last.LastCycleAt != "B" {
		t.Errorf("status.last() = %+v, want {ok  B}", last)
	}
	if *lastGood != "B" {
		t.Errorf("*lastSuccessfulCycleAt = %q, want B", *lastGood)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 11. Cycle calls are never overlapping: while one Cycle is deliberately
// held open, a second (and third) tick already queued on the ticker
// channel cannot cause a second Cycle call until the first one returns.
func TestWorkerCore_NoOverlappingCycleCalls(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	cycler := &fakeCycler{
		cycleResults: []cycleResult{{snap: snap("A")}, {snap: snap("B")}, {snap: snap("C")}},
		entered:      entered,
		gate:         gate,
	}
	pub := &publishRecorderSpy{}
	status := &statusRecorderSpy{notify: make(chan struct{}, 64)}
	errs := &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 2)}
	constructed := make(chan struct{}, 1)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, new(string))

	// First (pre-ticker) Cycle call: gated immediately.
	waitFor(t, entered, "first Cycle entry")
	gate <- struct{}{}                             // release A
	waitFor(t, constructed, "ticker construction") // implies cycle A fully processed
	waitForStatusCalls(t, status.notify, 2, "initializing + cycle A status")

	// Queue two ticks before releasing the second Cycle call, so a bug
	// that let the loop race ahead would have every opportunity to do so.
	ft.ch <- time.Now()
	ft.ch <- time.Now()

	waitFor(t, entered, "second Cycle entry")
	// The second tick must still be sitting unread: the loop cannot have
	// come back around to select while Cycle #2 is still blocked on gate.
	if _, cycleCalls := cycler.callCounts(); cycleCalls != 2 {
		t.Fatalf("cycleCalls = %d while second Cycle is gated, want 2 (no third call started)", cycleCalls)
	}
	if len(ft.ch) != 1 {
		t.Fatalf("len(ft.ch) = %d while second Cycle is gated, want 1 (second tick still unread)", len(ft.ch))
	}
	gate <- struct{}{} // release B
	waitForStatusCalls(t, status.notify, 1, "cycle B status")

	waitFor(t, entered, "third Cycle entry")
	gate <- struct{}{} // release C
	waitForStatusCalls(t, status.notify, 1, "cycle C status")

	if atomic.LoadInt32(&cycler.overlapDetected) != 0 {
		t.Error("overlapDetected = true, want Cycle calls to never overlap")
	}
	calls := pub.all()
	if len(calls) != 3 || calls[0].GeneratedAt != "A" || calls[1].GeneratedAt != "B" || calls[2].GeneratedAt != "C" {
		t.Fatalf("publish calls = %+v, want [A, B, C]", calls)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// 12. The ticker is always stopped (via defer) once the worker shuts
// down.
func TestWorkerCore_TickerStoppedOnShutdown(t *testing.T) {
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("A")}}}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, new(string))
	waitFor(t, constructed, "ticker construction")

	if ft.isStopped() {
		t.Fatal("ticker stopped before shutdown was even requested")
	}
	cancel()
	waitFor(t, done, "worker core shutdown")
	if !ft.isStopped() {
		t.Error("ticker.Stop() was not called on shutdown")
	}
}

// 13. If shutdown is requested during QuickSnapshot (observed once
// QuickSnapshot returns, before the first Cycle would be attempted),
// runOneCycle's own leading ctx check means Cycle is never called.
func TestWorkerCore_CancellationBetweenQuickSnapshotAndFirstCycle_PreventsCycleStarting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}

	// Cancel ctx as a side effect of QuickSnapshot returning, simulating a
	// shutdown signal that arrives in the narrow window right after
	// QuickSnapshot and before the first Cycle call.
	wrapped := &fakeCycler{quickResult: snap("quick"), cycleResults: []cycleResult{{snap: snap("should-not-run")}}}
	adapter := &cancelingQuickSnapshotCycler{inner: wrapped, cancel: cancel}

	done := startWorkerCore(ctx, adapter, false, failIfCalledNewTicker(t), pub, status, errs, new(string))
	waitFor(t, done, "worker core shutdown")

	if _, cycleCalls := wrapped.callCounts(); cycleCalls != 0 {
		t.Fatalf("cycleCalls = %d, want 0 (Cycle must never start once ctx is already cancelled)", cycleCalls)
	}
}

// cancelingQuickSnapshotCycler wraps a fakeCycler and cancels ctx the
// instant QuickSnapshot returns, precisely simulating a shutdown signal
// that arrives between QuickSnapshot and the first Cycle attempt.
type cancelingQuickSnapshotCycler struct {
	inner  *fakeCycler
	cancel context.CancelFunc
}

func (c *cancelingQuickSnapshotCycler) QuickSnapshot() (*PingScoresSnapshot, error) {
	snap, err := c.inner.QuickSnapshot()
	c.cancel()
	return snap, err
}

func (c *cancelingQuickSnapshotCycler) Cycle() (*PingScoresSnapshot, error) {
	return c.inner.Cycle()
}

// 14. Model A: an in-flight Cycle is never abandoned when shutdown is
// requested mid-call -- it's let run to completion -- but a result that
// raced the shutdown signal is never published.
func TestWorkerCore_CancellationDuringActiveCycle_WaitsAndNeverPublishes(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("A")}}, entered: entered, gate: gate}
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())

	done := startWorkerCore(ctx, cycler, false, failIfCalledNewTicker(t), pub, status, errs, new(string))

	waitFor(t, entered, "first Cycle entry")
	cancel()           // shutdown requested WHILE Cycle is still blocked inside the call
	gate <- struct{}{} // now let Cycle actually return successfully

	waitFor(t, done, "worker core shutdown")

	if calls := pub.all(); len(calls) != 0 {
		t.Fatalf("publish calls = %+v, want none (result raced the shutdown signal)", calls)
	}
	for _, s := range status.all() {
		if s.State == "ok" {
			t.Errorf("status.all() = %+v, must never report ok for a cycle that finished after shutdown", status.all())
		}
	}
}

// 15. lastSuccessfulCycleAt is threaded correctly across several cycles:
// it advances on each success and is externally observable at each step.
func TestWorkerCore_LastSuccessfulCycleAtThreadedAcrossMultipleCycles(t *testing.T) {
	cycler := &fakeCycler{
		cycleResults: []cycleResult{{snap: snap("A")}, {snap: snap("B")}, {snap: snap("C")}},
	}
	pub := &publishRecorderSpy{}
	status := &statusRecorderSpy{notify: make(chan struct{}, 64)}
	errs := &errorRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTicker{ch: make(chan time.Time, 2)}
	constructed := make(chan struct{}, 1)
	lastGood := new(string)

	done := startWorkerCore(ctx, cycler, false, newTickerSignaling(ft, constructed), pub, status, errs, lastGood)
	waitFor(t, constructed, "ticker construction")
	waitForStatusCalls(t, status.notify, 2, "initializing + cycle 1 status")
	if *lastGood != "A" {
		t.Fatalf("*lastSuccessfulCycleAt = %q after cycle 1, want A", *lastGood)
	}

	ft.ch <- time.Now()
	waitForStatusCalls(t, status.notify, 1, "cycle 2 status")
	if *lastGood != "B" {
		t.Fatalf("*lastSuccessfulCycleAt = %q after cycle 2, want B", *lastGood)
	}

	ft.ch <- time.Now()
	waitForStatusCalls(t, status.notify, 1, "cycle 3 status")
	if *lastGood != "C" {
		t.Fatalf("*lastSuccessfulCycleAt = %q after cycle 3, want C", *lastGood)
	}

	cancel()
	waitFor(t, done, "worker core shutdown")
}

// --- Lifecycle/store tests (pingScoreHistoryAttempt / pingScoreHistoryLifecycleCore / backoffPolicy / closeAndReport / runPingScoreHistoryLifecycleRecovered) ---

// 16. pingScoreHistoryAttempt, unit-tested directly: when newEngine
// fails, the store is closed (via defer) before the function returns,
// and the classification is attemptRetryAfterWait.
func TestAttempt_EngineInitFailure_ClosesStoreBeforeReturningRetryClassification(t *testing.T) {
	store := &fakeStoreHandle{}
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) {
		return nil, errors.New("engine init boom")
	}
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}

	result := pingScoreHistoryAttempt(context.Background(), store, newEngine, failIfCalledNewTicker(t), time.Minute, pub.record, status.record, errs.record, new(string))

	if result != attemptRetryAfterWait {
		t.Errorf("result = %v, want attemptRetryAfterWait", result)
	}
	if store.closes() != 1 {
		t.Errorf("store.closes() = %d, want 1", store.closes())
	}
	if last := status.last(); last.State != "degraded" || last.Code != "engine_init_failed" {
		t.Errorf("status.last() = %+v, want {degraded engine_init_failed ...}", last)
	}
	if !errs.hasCode("engine_init_failed") {
		t.Errorf("errs = %+v, want an engine_init_failed entry", errs.all())
	}
}

// 17. At the lifecycle level, a repeated engine_init_failed closes the
// store before EACH backoff wait, and genuinely reopens the store on
// retry (not the same handle reused).
func TestLifecycleCore_EngineInitFailed_ClosesStoreBeforeEachWaitAndReopensOnRetry(t *testing.T) {
	elog := &eventLog{}
	openCalls := 0
	open := func(string) (pingScoreHistoryStoreHandle, error) {
		openCalls++
		return &fakeStoreHandle{eventLog: elog}, nil
	}
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) {
		return nil, errors.New("engine init boom")
	}
	waitCalls := 0
	waiter := func(context.Context, time.Duration) bool {
		waitCalls++
		elog.record("wait_called")
		return waitCalls >= 2 // stop after the second wait
	}
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}

	pingScoreHistoryLifecycleCore(context.Background(), open, "path", newEngine, failIfCalledNewTicker(t), time.Minute,
		backoffPolicy{initial: time.Millisecond, max: time.Second}, waiter, pub.record, status.record, errs.record)

	if openCalls != 2 {
		t.Errorf("openCalls = %d, want 2", openCalls)
	}
	want := []string{"store_close", "wait_called", "store_close", "wait_called"}
	if got := elog.snapshot(); !equalStrings(got, want) {
		t.Errorf("eventLog = %v, want %v", got, want)
	}
	if calls := errs.all(); len(calls) != 2 || calls[0].code != "engine_init_failed" || calls[1].code != "engine_init_failed" {
		t.Errorf("errs = %+v, want two engine_init_failed entries", calls)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 19. A plain (non-typed) open error is classified open_failed and the
// loop retries.
func TestLifecycleCore_GenericOpenError_ClassifiedOpenFailedRetries(t *testing.T) {
	openCalls := 0
	open := func(string) (pingScoreHistoryStoreHandle, error) {
		openCalls++
		return nil, errors.New("plain open error")
	}
	waitCalls := 0
	waiter := func(context.Context, time.Duration) bool {
		waitCalls++
		return waitCalls >= 2
	}
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}

	pingScoreHistoryLifecycleCore(context.Background(), open, "path", nil, failIfCalledNewTicker(t), time.Minute,
		backoffPolicy{initial: time.Millisecond, max: time.Second}, waiter, pub.record, status.record, errs.record)

	if openCalls != 2 {
		t.Errorf("openCalls = %d, want 2 (retried after open_failed)", openCalls)
	}
	if last := status.last(); last.State != "degraded" || last.Code != "open_failed" {
		t.Errorf("status.last() = %+v, want {degraded open_failed ...}", last)
	}
	if !errs.hasCode("open_failed") {
		t.Errorf("errs = %+v, want an open_failed entry", errs.all())
	}
}

// 20. A *PingScoreHistoryCorruptError is terminal: no retry, and wait is
// never even called.
func TestLifecycleCore_CorruptOpenError_TerminalNoRetryNoWait(t *testing.T) {
	openCalls := 0
	open := func(string) (pingScoreHistoryStoreHandle, error) {
		openCalls++
		return nil, &PingScoreHistoryCorruptError{Path: "x.db", Detail: "integrity check failed"}
	}
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}

	pingScoreHistoryLifecycleCore(context.Background(), open, "path", nil, failIfCalledNewTicker(t), time.Minute,
		backoffPolicy{initial: time.Millisecond, max: time.Second}, failIfCalledWaiter(t), pub.record, status.record, errs.record)

	if openCalls != 1 {
		t.Errorf("openCalls = %d, want exactly 1 (corrupt is terminal, no retry)", openCalls)
	}
	if last := status.last(); last.State != "degraded" || last.Code != "corrupt" {
		t.Errorf("status.last() = %+v, want {degraded corrupt ...}", last)
	}
	if !errs.hasCode("corrupt") {
		t.Errorf("errs = %+v, want a corrupt entry", errs.all())
	}
}

// 21. A *PingScoreHistoryMigrationError is degraded/migration_failed and
// the loop retries (a transient cause may resolve).
func TestLifecycleCore_MigrationOpenError_DegradedRetries(t *testing.T) {
	openCalls := 0
	open := func(string) (pingScoreHistoryStoreHandle, error) {
		openCalls++
		return nil, &PingScoreHistoryMigrationError{Path: "x.db", FromVersion: 1, ToVersion: 2, Err: errors.New("mig fail")}
	}
	waitCalls := 0
	waiter := func(context.Context, time.Duration) bool {
		waitCalls++
		return waitCalls >= 2
	}
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}

	pingScoreHistoryLifecycleCore(context.Background(), open, "path", nil, failIfCalledNewTicker(t), time.Minute,
		backoffPolicy{initial: time.Millisecond, max: time.Second}, waiter, pub.record, status.record, errs.record)

	if openCalls != 2 {
		t.Errorf("openCalls = %d, want 2 (retried after migration_failed)", openCalls)
	}
	if last := status.last(); last.State != "degraded" || last.Code != "migration_failed" {
		t.Errorf("status.last() = %+v, want {degraded migration_failed ...}", last)
	}
	if !errs.hasCode("migration_failed") {
		t.Errorf("errs = %+v, want a migration_failed entry", errs.all())
	}
}

// 22. backoffPolicy doubles, caps at max, and guards against overflow
// when doubling a duration already near time.Duration's maximum.
func TestBackoffPolicy_DoublesAndCapsWithOverflowGuard(t *testing.T) {
	b := backoffPolicy{initial: 100 * time.Millisecond, max: 1 * time.Second}
	steps := []struct{ cur, want time.Duration }{
		{0, 100 * time.Millisecond},
		{100 * time.Millisecond, 200 * time.Millisecond},
		{200 * time.Millisecond, 400 * time.Millisecond},
		{400 * time.Millisecond, 800 * time.Millisecond},
		{800 * time.Millisecond, 1 * time.Second}, // would be 1.6s, capped
		{1 * time.Second, 1 * time.Second},        // already at max
	}
	for _, s := range steps {
		if got := b.next(s.cur); got != s.want {
			t.Errorf("next(%s) = %s, want %s", s.cur, got, s.want)
		}
	}

	overflowGuard := backoffPolicy{initial: time.Second, max: 30 * time.Second}
	nearMax := time.Duration(math.MaxInt64) - time.Second
	if got := overflowGuard.next(nearMax); got != overflowGuard.max {
		t.Errorf("next(near-MaxInt64) = %s, want max %s (overflow guard)", got, overflowGuard.max)
	}
}

// 23. closeAndReport reports a close failure as close_failed and never
// touches status at all -- its signature has no setStatus parameter,
// which is itself the structural guarantee; this proves the error is at
// least surfaced somewhere. A nil close error reports nothing.
func TestCloseAndReport_ErrorReportedNoErrorReportsNothing(t *testing.T) {
	errs := &errorRecorderSpy{}
	failing := &fakeStoreHandle{closeErr: errors.New("close boom")}
	closeAndReport(failing, errs.record)
	if failing.closes() != 1 {
		t.Errorf("closes() = %d, want 1", failing.closes())
	}
	if calls := errs.all(); len(calls) != 1 || calls[0].code != "close_failed" {
		t.Errorf("errs = %+v, want exactly one close_failed entry", calls)
	}

	errs2 := &errorRecorderSpy{}
	ok := &fakeStoreHandle{}
	closeAndReport(ok, errs2.record)
	if calls := errs2.all(); len(calls) != 0 {
		t.Errorf("errs = %+v, want none for a successful close", calls)
	}
}

// 24. Cancellation during the backoff wait itself returns immediately --
// no real sleep is needed to prove this: the fake waiter genuinely
// blocks on ctx.Done().
func TestLifecycleCore_CancellationDuringBackoffWait_ReturnsImmediately(t *testing.T) {
	open := func(string) (pingScoreHistoryStoreHandle, error) {
		return nil, errors.New("open boom")
	}
	waiter := func(ctx context.Context, d time.Duration) bool {
		<-ctx.Done()
		return true
	}
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		pingScoreHistoryLifecycleCore(ctx, open, "path", nil, failIfCalledNewTicker(t), time.Minute,
			backoffPolicy{initial: time.Hour, max: time.Hour}, waiter, pub.record, status.record, errs.record)
		close(done)
	}()

	cancel()
	waitFor(t, done, "lifecycle core shutdown during backoff wait")
}

// 25. Once a worker actually starts (engine construction succeeds), the
// lifecycle loop never retries, no matter why the worker later returns.
func TestLifecycleCore_NoRetryOnceWorkerStarted(t *testing.T) {
	openCalls := 0
	open := func(string) (pingScoreHistoryStoreHandle, error) {
		openCalls++
		return &fakeStoreHandle{readOnly: true}, nil
	}
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) {
		return &fakeCycler{quickResult: snap("quick")}, nil
	}
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}

	pingScoreHistoryLifecycleCore(context.Background(), open, "path", newEngine, failIfCalledNewTicker(t), time.Minute,
		backoffPolicy{initial: time.Millisecond, max: time.Second}, failIfCalledWaiter(t), pub.record, status.record, errs.record)

	if openCalls != 1 {
		t.Errorf("openCalls = %d, want exactly 1 (a started worker is never retried)", openCalls)
	}
	if last := status.last(); last.State != "read_only" {
		t.Errorf("status.last() = %+v, want state=read_only", last)
	}
}

// 26. A panic deep inside Cycle() unwinds correctly: ticker.Stop() and
// store.Close() (deferred at inner frames) still fire before the single
// outermost recover() in runPingScoreHistoryLifecycleRecovered, which
// reports the panic and sets status to panic/panic carrying the LAST
// KNOWN LastCycleAt (via the local recorder, not any *Server field), and
// returns normally instead of re-panicking.
func TestRunPingScoreHistoryLifecycleRecovered_PanicRecoveredWithLastKnownCycleAt(t *testing.T) {
	elog := &eventLog{}
	store := &fakeStoreHandle{eventLog: elog}
	open := func(string) (pingScoreHistoryStoreHandle, error) { return store, nil }

	cycler := &fakeCycler{
		cycleResults: []cycleResult{{snap: snap("A")}}, // first (pre-ticker) cycle succeeds
	}
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) { return cycler, nil }

	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)
	newTicker := newTickerSignaling(ft, constructed)

	status := &statusRecorderSpy{notify: make(chan struct{}, 64)}
	errs, pub := &errorRecorderSpy{}, &publishRecorderSpy{}
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		runPingScoreHistoryLifecycleRecovered(ctx, open, "path", newEngine, newTicker, time.Minute,
			backoffPolicy{initial: time.Millisecond, max: time.Second}, failIfCalledWaiter(t), pub.record, status.record, errs.record)
		close(done)
	}()

	waitFor(t, constructed, "ticker construction") // implies the first cycle (A) fully processed
	waitForStatusCalls(t, status.notify, 2, "initializing + cycle 1 (A) status")
	if last := status.last(); last.State != "ok" || last.LastCycleAt != "A" {
		t.Fatalf("status.last() before panic = %+v, want {ok  A}", last)
	}

	// Now arm the panic for the NEXT Cycle call and fire a tick.
	cycler.mu.Lock()
	cycler.panicValue = "boom"
	cycler.mu.Unlock()
	ft.ch <- time.Now()

	waitFor(t, done, "recovered goroutine to return normally after the panic")

	if !ft.isStopped() {
		t.Error("ticker.Stop() was not called during panic unwind")
	}
	if store.closes() != 1 {
		t.Errorf("store.closes() = %d, want 1 (closed during panic unwind)", store.closes())
	}
	if !errs.hasCode("panic") {
		t.Errorf("errs = %+v, want a panic entry", errs.all())
	}
	for _, c := range errs.all() {
		if c.code == "panic" && !strings.Contains(c.err.Error(), "boom") {
			t.Errorf("panic error = %q, want it to mention the panic value %q", c.err.Error(), "boom")
		}
	}
	if last := status.last(); last.State != "panic" || last.Code != "panic" || last.LastCycleAt != "A" {
		t.Errorf("status.last() = %+v, want {panic panic A} (last known cycle time preserved)", last)
	}
}

// 27. If ctx is already cancelled before the lifecycle loop's very first
// iteration, open() is never called and nothing is reported.
func TestLifecycleCore_ContextAlreadyCancelledBeforeFirstIteration_SkipsOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, errs, pub := &statusRecorderSpy{}, &errorRecorderSpy{}, &publishRecorderSpy{}

	pingScoreHistoryLifecycleCore(ctx, failIfCalledOpener(t), "path", nil, failIfCalledNewTicker(t), time.Minute,
		backoffPolicy{initial: time.Millisecond, max: time.Second}, failIfCalledWaiter(t), pub.record, status.record, errs.record)

	if len(status.all()) != 0 {
		t.Errorf("status calls = %+v, want none", status.all())
	}
	if len(errs.all()) != 0 {
		t.Errorf("error calls = %+v, want none", errs.all())
	}
	if len(pub.all()) != 0 {
		t.Errorf("publish calls = %+v, want none", pub.all())
	}
}
