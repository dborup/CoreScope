package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fase 5G: synchronous validation ---------------------------------------

func validPingScoreHistoryStartArgs() (*Server, string, pingScoreHistoryEngineConfig, time.Duration, backoffPolicy) {
	return &Server{}, "/tmp/ping_scores_history_test.db", defaultPingScoreHistoryEngineConfig(), time.Minute, pingScoreHistoryProductionBackoff()
}

func TestValidatePingScoreHistoryStartConfig_AllValidAccepted(t *testing.T) {
	s, path, cfg, interval, backoff := validPingScoreHistoryStartArgs()
	if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, backoff); err != nil {
		t.Errorf("want no error for a fully valid config, got %v", err)
	}
}

func TestValidatePingScoreHistoryStartConfig_NilServerRejected(t *testing.T) {
	_, path, cfg, interval, backoff := validPingScoreHistoryStartArgs()
	if err := validatePingScoreHistoryStartConfig(nil, path, cfg, interval, backoff); err == nil {
		t.Fatal("want an error for a nil Server")
	}
}

func TestValidatePingScoreHistoryStartConfig_EmptyOrWhitespacePathRejected(t *testing.T) {
	s, _, cfg, interval, backoff := validPingScoreHistoryStartArgs()
	for _, path := range []string{"", "   ", "\t\n"} {
		if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, backoff); err == nil {
			t.Errorf("path=%q: want an error", path)
		}
	}
}

func TestValidatePingScoreHistoryStartConfig_NonPositiveIntervalRejected(t *testing.T) {
	s, path, cfg, _, backoff := validPingScoreHistoryStartArgs()
	for _, interval := range []time.Duration{0, -time.Second} {
		if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, backoff); err == nil {
			t.Errorf("interval=%s: want an error", interval)
		}
	}
}

func TestValidatePingScoreHistoryStartConfig_NonPositiveBackoffInitialRejected(t *testing.T) {
	s, path, cfg, interval, _ := validPingScoreHistoryStartArgs()
	for _, initial := range []time.Duration{0, -time.Minute} {
		b := backoffPolicy{initial: initial, max: 30 * time.Minute}
		if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, b); err == nil {
			t.Errorf("backoff.initial=%s: want an error", initial)
		}
	}
}

func TestValidatePingScoreHistoryStartConfig_MaxLessThanInitialRejected(t *testing.T) {
	s, path, cfg, interval, _ := validPingScoreHistoryStartArgs()
	b := backoffPolicy{initial: time.Minute, max: 30 * time.Second}
	if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, b); err == nil {
		t.Fatal("want an error when backoff.max < backoff.initial")
	}
}

func TestValidatePingScoreHistoryStartConfig_NegativeEngineConfigFieldsRejected(t *testing.T) {
	s, path, _, interval, backoff := validPingScoreHistoryStartArgs()
	base := defaultPingScoreHistoryEngineConfig()

	cfg := base
	cfg.SettleDebounce = -time.Second
	if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, backoff); err == nil {
		t.Error("negative SettleDebounce: want an error")
	}

	cfg = base
	cfg.DeepSweepBatchSize = -1
	if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, backoff); err == nil {
		t.Error("negative DeepSweepBatchSize: want an error")
	}

	cfg = base
	cfg.RetentionDuration = -time.Hour
	if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, backoff); err == nil {
		t.Error("negative RetentionDuration: want an error")
	}
}

func TestValidatePingScoreHistoryStartConfig_NonPositiveMaxEdgeKmAccepted(t *testing.T) {
	s, path, cfg, interval, backoff := validPingScoreHistoryStartArgs()
	for _, maxEdgeKm := range []float64{0, -1, -30} {
		cfg.MaxEdgeKm = maxEdgeKm
		if err := validatePingScoreHistoryStartConfig(s, path, cfg, interval, backoff); err != nil {
			t.Errorf("MaxEdgeKm=%v: want no error (deliberate disable-geo-filter convention), got %v", maxEdgeKm, err)
		}
	}
}

func TestDefaultPingScoreHistoryEngineConfig_ExactValues(t *testing.T) {
	got := defaultPingScoreHistoryEngineConfig()
	want := pingScoreHistoryEngineConfig{
		SettleDebounce:     10 * time.Minute,
		DeepSweepBatchSize: 100,
		MaxEdgeKm:          EstimateMaxEdgeKm,
		RetentionDuration:  0,
	}
	if got != want {
		t.Errorf("defaultPingScoreHistoryEngineConfig() = %+v, want %+v", got, want)
	}
}

func TestPingScoreHistoryProductionBackoff_ExactValues(t *testing.T) {
	got := pingScoreHistoryProductionBackoff()
	want := backoffPolicy{initial: time.Minute, max: 30 * time.Minute}
	if got != want {
		t.Errorf("pingScoreHistoryProductionBackoff() = %+v, want %+v", got, want)
	}
}

func TestStartPingScoreHistoryEngineWithDependencies_ValidationFailure_NeverOpensOrStartsGoroutine(t *testing.T) {
	stop, err := startPingScoreHistoryEngineWithDependencies(
		nil, // nil server -- guaranteed synchronous validation failure
		"/tmp/x.db", defaultPingScoreHistoryEngineConfig(), time.Minute, pingScoreHistoryProductionBackoff(),
		failIfCalledOpener(t), nil, failIfCalledNewTicker(t), failIfCalledWaiter(t),
		func(*PingScoresSnapshot) { t.Fatal("publish must not be called") },
		func(state, code, lastCycleAt string) { t.Fatal("setStatus must not be called") },
		func(code string, err error) { t.Fatal("reportError must not be called") },
	)
	if err == nil {
		t.Fatal("want an error for invalid config")
	}
	if stop != nil {
		t.Fatal("want a nil stop func on validation failure")
	}
}

func TestStartPingScoreHistoryEngine_ValidationFailurePropagates(t *testing.T) {
	stop, err := StartPingScoreHistoryEngine(nil, "/tmp/x.db", defaultPingScoreHistoryEngineConfig(), time.Minute, pingScoreHistoryProductionBackoff())
	if err == nil {
		t.Fatal("want an error for a nil Server")
	}
	if stop != nil {
		t.Fatal("want a nil stop func on validation failure")
	}
}

// --- fase 5G: Start/Stop semantics (via the injectable core, using the
// SAME deterministic fase 5D fakes -- no wall-clock waiting for
// synchronization, only bounded safety-net timeouts against a hung test) --

// startFixture builds a ready-to-use, immediately-successful (read-only,
// so the worker finishes its own lifecycle almost instantly without
// needing Stop to force cancellation) startPingScoreHistoryEngineWithDependencies
// call, returning openCalls for tests that want to assert on it.
func startPingScoreHistoryReadOnlyFixture(t *testing.T) (stop func(), openCalls *int, store *fakeStoreHandle, status *statusRecorderSpy, errs *errorRecorderSpy) {
	t.Helper()
	calls := 0
	store = &fakeStoreHandle{readOnly: true}
	open := func(string) (pingScoreHistoryStoreHandle, error) {
		calls++
		return store, nil
	}
	cycler := &fakeCycler{quickResult: snap("quick")}
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) { return cycler, nil }
	pub := &publishRecorderSpy{}
	status = &statusRecorderSpy{notify: make(chan struct{}, 64)}
	errs = &errorRecorderSpy{}

	stop, err := startPingScoreHistoryEngineWithDependencies(
		&Server{}, "/tmp/x.db", defaultPingScoreHistoryEngineConfig(), time.Minute, pingScoreHistoryProductionBackoff(),
		open, newEngine, failIfCalledNewTicker(t), failIfCalledWaiter(t),
		pub.record, status.record, errs.record,
	)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	// Draining 2 notify signals is enough to prove open() has already
	// happened, race-free, with no atomic needed: signal 1 is the
	// synchronous "initializing" startPingScoreHistoryEngineWithDependencies
	// itself sets before ever starting the background goroutine; signal
	// 2 is that goroutine's OWN first setStatus call (from
	// pingScoreHistoryWorkerCore, after QuickSnapshot) -- which, on that
	// SAME goroutine, is unconditionally preceded by pingScoreHistoryLifecycleCore's
	// open()+newEngine() call in program order, regardless of which
	// exact state string signal 2 turns out to carry.
	waitForStatusCalls(t, status.notify, 2, "open()-preceding status calls")
	return stop, &calls, store, status, errs
}

func TestStartPingScoreHistoryEngineWithDependencies_ReturnsWithoutWaitingForFirstCycle(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("A")}}, entered: entered, gate: gate}
	store := &fakeStoreHandle{}
	open := func(string) (pingScoreHistoryStoreHandle, error) { return store, nil }
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) { return cycler, nil }
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}
	ft := &fakeTicker{ch: make(chan time.Time, 1)}
	constructed := make(chan struct{}, 1)

	type result struct {
		stop func()
		err  error
	}
	startDone := make(chan result, 1)
	go func() {
		stop, err := startPingScoreHistoryEngineWithDependencies(
			&Server{}, "/tmp/x.db", defaultPingScoreHistoryEngineConfig(), time.Minute, pingScoreHistoryProductionBackoff(),
			open, newEngine, newTickerSignaling(ft, constructed), failIfCalledWaiter(t),
			pub.record, status.record, errs.record,
		)
		startDone <- result{stop, err}
	}()

	var stop func()
	select {
	case r := <-startDone:
		if r.err != nil {
			t.Fatalf("Start returned an error: %v", r.err)
		}
		stop = r.stop
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return promptly -- it must never block on the first Cycle (which is still gated open)")
	}

	waitFor(t, entered, "first Cycle entry")
	gate <- struct{}{} // let the first Cycle finish; the worker then constructs a ticker and enters steady-state
	stop()
}

func TestStartPingScoreHistoryEngineWithDependencies_ExactlyOneLifecycleGoroutine(t *testing.T) {
	stop, openCalls, _, _, _ := startPingScoreHistoryReadOnlyFixture(t)
	stop() // a read-only worker finishes on its own; Stop just waits for that

	if *openCalls != 1 {
		t.Errorf("openCalls = %d, want exactly 1 (a single lifecycle instance/goroutine)", *openCalls)
	}
}

func TestStartPingScoreHistoryEngineWithDependencies_DoubleStopSafe(t *testing.T) {
	stop, _, _, _, _ := startPingScoreHistoryReadOnlyFixture(t)
	stop()
	stop() // must not panic, hang, or double-close a channel
}

func TestStartPingScoreHistoryEngineWithDependencies_ConcurrentStopSafe(t *testing.T) {
	stop, _, _, _, _ := startPingScoreHistoryReadOnlyFixture(t)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stop()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Stop calls did not all return -- possible deadlock")
	}
}

func TestStartPingScoreHistoryEngineWithDependencies_StopWaitsForActiveCycle(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("A")}}, entered: entered, gate: gate}
	store := &fakeStoreHandle{}
	open := func(string) (pingScoreHistoryStoreHandle, error) { return store, nil }
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) { return cycler, nil }
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}

	stop, err := startPingScoreHistoryEngineWithDependencies(
		&Server{}, "/tmp/x.db", defaultPingScoreHistoryEngineConfig(), time.Minute, pingScoreHistoryProductionBackoff(),
		open, newEngine, failIfCalledNewTicker(t), failIfCalledWaiter(t),
		pub.record, status.record, errs.record,
	)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	waitFor(t, entered, "first Cycle entry")

	stopReturned := make(chan struct{})
	go func() {
		stop()
		close(stopReturned)
	}()

	select {
	case <-stopReturned:
		t.Fatal("stop() returned while the first Cycle was still gated open -- it must wait for the active Cycle to finish")
	case <-time.After(200 * time.Millisecond):
		// expected: stop() is still blocked on the active Cycle.
	}

	gate <- struct{}{} // release the Cycle
	waitFor(t, stopReturned, "stop() to return once the Cycle finishes")

	// done closing after cleanup means the store is already closed by
	// the time stop() returns.
	if n := store.closes(); n != 1 {
		t.Errorf("store.closes() = %d, want exactly 1 (closed as part of cleanup before stop() returned)", n)
	}
}

func TestStartPingScoreHistoryEngineWithDependencies_PanicBecomesStatusPanicAndStopReturns(t *testing.T) {
	entered := make(chan struct{})
	cycler := &fakeCycler{cycleResults: []cycleResult{{snap: snap("A")}}, entered: entered, panicValue: "boom"}
	store := &fakeStoreHandle{}
	open := func(string) (pingScoreHistoryStoreHandle, error) { return store, nil }
	newEngine := func(pingScoreHistoryStoreHandle) (pingScoreHistoryCycler, error) { return cycler, nil }
	pub, status, errs := &publishRecorderSpy{}, &statusRecorderSpy{}, &errorRecorderSpy{}

	stop, err := startPingScoreHistoryEngineWithDependencies(
		&Server{}, "/tmp/x.db", defaultPingScoreHistoryEngineConfig(), time.Minute, pingScoreHistoryProductionBackoff(),
		open, newEngine, failIfCalledNewTicker(t), failIfCalledWaiter(t),
		pub.record, status.record, errs.record,
	)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	waitFor(t, entered, "first Cycle entry (about to panic, ungated)")

	stop() // must return normally even though the worker panicked internally

	if last := status.last(); last.State != "panic" || last.Code != "panic" {
		t.Errorf("status.last() = %+v, want {panic panic ...}", last)
	}
	if !errs.hasCode("panic") {
		t.Errorf("errs = %+v, want a panic entry", errs.all())
	}
	if n := store.closes(); n != 1 {
		t.Errorf("store.closes() = %d, want 1 (closed during panic unwind)", n)
	}
}

// --- fase 5G: production adapters (A, B, C, D, E, G) -----------------------

func TestPingScoreHistoryRealNewTicker_TicksAndStopsCleanly(t *testing.T) {
	ticker := pingScoreHistoryRealNewTicker(5 * time.Millisecond)
	select {
	case <-ticker.C():
		// tick observed
	case <-time.After(2 * time.Second):
		t.Fatal("real ticker never ticked within 2s")
	}
	ticker.Stop() // must not panic
}

func TestPingScoreHistoryRealWaiter_ReturnsTrueOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- pingScoreHistoryRealWaiter(ctx, time.Hour) // would never fire on its own
	}()
	cancel()
	select {
	case cancelled := <-done:
		if !cancelled {
			t.Error("want true (cancelled) when ctx is cancelled before the duration elapses")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return promptly after ctx cancellation")
	}
}

func TestPingScoreHistoryRealWaiter_ReturnsFalseWhenDurationElapses(t *testing.T) {
	got := pingScoreHistoryRealWaiter(context.Background(), 5*time.Millisecond)
	if got {
		t.Error("want false (not cancelled) when the duration elapses on its own")
	}
}

func TestPingScoreHistoryRealOpener_OpensAndCloses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")
	handle, err := pingScoreHistoryRealOpener(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, ok := handle.(*PingScoreHistoryStore); !ok {
		t.Fatalf("handle is %T, want *PingScoreHistoryStore", handle)
	}
	if handle.ReadOnly() {
		t.Error("a freshly created store should not be read-only")
	}
	if err := handle.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPingScoreHistoryRealEngineFactory_UnexpectedStoreHandleType_ClearError(t *testing.T) {
	factory := pingScoreHistoryRealEngineFactory(&Server{}, defaultPingScoreHistoryEngineConfig())
	_, err := factory(&fakeStoreHandle{}) // NOT a *PingScoreHistoryStore
	if err == nil {
		t.Fatal("want an error for an unexpected store handle type")
	}
	if !strings.Contains(err.Error(), "unexpected store handle type") {
		t.Errorf("err = %q, want it to mention the unexpected type", err.Error())
	}
}

func TestPingScoreHistoryRealPublisher_StoresIntoServerPingScoresCache(t *testing.T) {
	s := &Server{}
	publish := pingScoreHistoryRealPublisher(s)
	snapshot := &PingScoresSnapshot{GeneratedAt: "2026-08-05T00:00:00Z", TotalPings: 3}
	publish(snapshot)

	if got := s.pingScores.Load(); got != snapshot {
		t.Errorf("s.pingScores.Load() = %+v, want the exact snapshot pointer that was published", got)
	}
}

// G. The production reporter must log the FULL internal error with the
// stable "[ping-scores-history] <code>: <error>" prefix -- this is the
// ONE place that full detail is allowed to appear anywhere. The mirror
// guarantee -- that /api/healthz NEVER sees this raw detail, only the
// sanitized state/code/lastCycleAt tuple -- is proven exhaustively by
// fase 5F's own healthz tests (TestHealthzReady_DegradedPingScoresHistoryStatus_NoRawErrorText
// et al.) and structurally by pingScoreHistorySetStatus's own signature,
// which has no error parameter to leak through in the first place.
func TestPingScoreHistoryRealReportError_LogsWithStablePrefix(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(orig)
		log.SetFlags(origFlags)
	}()

	pingScoreHistoryRealReportError("cycle_failed", errors.New("secret detail: /var/lib/corescope/meshcore.db"))

	got := buf.String()
	if !strings.Contains(got, "[ping-scores-history] cycle_failed:") {
		t.Errorf("log output = %q, want the stable [ping-scores-history] <code>: prefix", got)
	}
	if !strings.Contains(got, "secret detail: /var/lib/corescope/meshcore.db") {
		t.Errorf("log output = %q, want the FULL underlying error text (this is the one place it's allowed to appear)", got)
	}
}
