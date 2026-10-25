package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #492: opt-in analytics.pauseWhenIdle. A scheduled recompute is
// skipped when nothing read the snapshot since the previous pass; the
// next read is served the existing snapshot at once and wakes ONE
// coalesced refresh.
//
// Every test here is driven by a manual ticker and the loop's event hook,
// never by sleeping: a tick is delivered by a channel send, and the test
// waits for the loop to report what it did with it.

type manualRecomputeTicker struct {
	c      chan time.Time
	resets atomic.Int64
	// resetC gets one (non-blocking) send per Reset. The loop calls Reset
	// after a kick or RecomputeNow pass, so a receive means that pass is
	// done.
	resetC chan struct{}
}

func newManualRecomputeTicker() *manualRecomputeTicker {
	return &manualRecomputeTicker{c: make(chan time.Time), resetC: make(chan struct{}, 16)}
}

func (m *manualRecomputeTicker) C() <-chan time.Time { return m.c }
func (m *manualRecomputeTicker) Stop()               {}
func (m *manualRecomputeTicker) Reset(time.Duration) {
	m.resets.Add(1)
	select {
	case m.resetC <- struct{}{}:
	default:
	}
}

// snapshotClock is an injectable analyticsRecomputer.now.
type snapshotClock struct{ ns atomic.Int64 }

func newSnapshotClock() *snapshotClock {
	c := &snapshotClock{}
	c.ns.Store(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano())
	return c
}

func (c *snapshotClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *snapshotClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

const pauseTestDeadline = 5 * time.Second

type pauseHarness struct {
	t      *testing.T
	r      *analyticsRecomputer
	tk     *manualRecomputeTicker
	events chan string

	logMu sync.Mutex
	log   []string // every loop event and tick, in order; printed if the test fails
}

func (h *pauseHarness) record(ev string) {
	h.logMu.Lock()
	h.log = append(h.log, ev)
	h.logMu.Unlock()
}

// newPauseHarness builds an unstarted recomputer whose compute is fn
// (called with the 1-based pass number) and whose loop reports every
// event on h.events.
func newPauseHarness(t *testing.T, name string, pause bool, fn func(pass int64) interface{}) *pauseHarness {
	t.Helper()
	h := &pauseHarness{t: t, tk: newManualRecomputeTicker(), events: make(chan string, 256)}
	var passes atomic.Int64
	h.r = newAnalyticsRecomputer(name, time.Hour, func() interface{} {
		return fn(passes.Add(1))
	})
	h.r.pauseWhenIdle = pause
	h.r.newTicker = func(string, time.Duration) recomputeTicker { return h.tk }
	h.r.onLoopEvent = func(ev string) { h.record(ev); h.events <- ev }
	t.Cleanup(func() {
		if t.Failed() {
			h.logMu.Lock()
			defer h.logMu.Unlock()
			t.Logf("%s: loop events before failure: %v", name, h.log)
		}
	})
	return h
}

func (h *pauseHarness) start() *pauseHarness {
	h.r.Start()
	h.t.Cleanup(h.r.Stop)
	return h
}

func countingPass(pass int64) interface{} { return pass }

func (h *pauseHarness) next() string {
	h.t.Helper()
	select {
	case ev := <-h.events:
		return ev
	case <-time.After(pauseTestDeadline):
		h.t.Fatalf("%s: no loop event within %s", h.r.name, pauseTestDeadline)
		return ""
	}
}

func (h *pauseHarness) tick() string {
	h.t.Helper()
	select {
	case h.tk.c <- time.Now():
		h.record("tick-sent")
	case <-time.After(pauseTestDeadline):
		h.t.Fatalf("%s: loop did not accept a tick", h.r.name)
	}
	return h.next()
}

func (h *pauseHarness) expect(want string) {
	h.t.Helper()
	if got := h.next(); got != want {
		h.t.Fatalf("%s: loop event = %q, want %q", h.r.name, got, want)
	}
}

func (h *pauseHarness) expectTick(want string) {
	h.t.Helper()
	if got := h.tick(); got != want {
		h.t.Fatalf("%s: tick event = %q, want %q", h.r.name, got, want)
	}
}

// wakeWinsRace forces the worst interleaving of a Load that wakes a paused
// recomputer: the wake pass stores its snapshot before that Load fetches
// one (#610). Only for passes that succeed and are not gated.
func (h *pauseHarness) wakeWinsRace() {
	h.r.testHookLoad = func() {
		old := h.r.cache.Load()
		deadline := time.Now().Add(pauseTestDeadline)
		for h.r.cache.Load() == old {
			if time.Now().After(deadline) {
				h.t.Errorf("%s: wake pass did not store a snapshot", h.r.name)
				return
			}
			time.Sleep(50 * time.Microsecond)
		}
	}
}

// peek returns the stored snapshot without counting as a read, so it can
// never wake a paused recomputer.
func (h *pauseHarness) peek() interface{} {
	if snap, _ := h.r.cache.Load().(*recomputerSnapshot); snap != nil {
		return snap.v
	}
	return nil
}

// wakeRead is the one read that wakes a paused recomputer. Whether that
// Load returns the old or the refreshed snapshot depends on scheduling, so
// its value is not asserted; the harness waits for the wake pass and checks
// that exactly one ran. Assert snapshots with peek before and after.
func (h *pauseHarness) wakeRead() {
	h.t.Helper()
	before := h.r.ComputeRuns()
	h.r.Load()
	h.expect("kick-run")
	if got := h.r.ComputeRuns(); got != before+1 {
		h.t.Fatalf("%s: ComputeRuns = %d after the wake read, want %d", h.r.name, got, before+1)
	}
}

func (h *pauseHarness) noPendingEvent() {
	h.t.Helper()
	select {
	case ev := <-h.events:
		h.t.Fatalf("%s: unexpected loop event %q", h.r.name, ev)
	default:
	}
}

// --- config -------------------------------------------------------------

func TestPauseWhenIdle_ConfigDefaultsOff(t *testing.T) {
	if (*Config)(nil).AnalyticsPauseWhenIdle() || (*Config)(nil).AnalyticsRecomputeIntervals().PauseWhenIdle {
		t.Fatal("nil config must leave pauseWhenIdle off")
	}
	for _, raw := range []string{
		`{}`,
		`{"analytics":{}}`,
		`{"analytics":{"defaultIntervalSeconds":300}}`,
		`{"analytics":{"pauseWhenIdle":false,"recomputeIntervalSeconds":{"rf":60}}}`,
	} {
		var c Config
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		if c.AnalyticsPauseWhenIdle() || c.AnalyticsRecomputeIntervals().PauseWhenIdle {
			t.Errorf("%s: pauseWhenIdle must default off", raw)
		}
	}
}

// main.go passes cfg.AnalyticsRecomputeIntervals() straight to
// StartAnalyticsRecomputers, so the flag must survive the early return
// for a config without per-endpoint intervals.
func TestPauseWhenIdle_ConfigTrueReachesIntervals(t *testing.T) {
	for _, raw := range []string{
		`{"analytics":{"pauseWhenIdle":true}}`,
		`{"analytics":{"pauseWhenIdle":true,"recomputeIntervalSeconds":{"rf":60}}}`,
	} {
		var c Config
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		if !c.AnalyticsPauseWhenIdle() {
			t.Errorf("%s: AnalyticsPauseWhenIdle = false", raw)
		}
		if !c.AnalyticsRecomputeIntervals().PauseWhenIdle {
			t.Errorf("%s: AnalyticsRecomputeIntervals().PauseWhenIdle = false", raw)
		}
	}
}

// storeRecomputerFields returns every *analyticsRecomputer field on
// PacketStore by reflection, so a recomputer added later without the
// knob fails here.
func storeRecomputerFields(t *testing.T, s *PacketStore) map[string]*analyticsRecomputer {
	t.Helper()
	s.analyticsRecomputerMu.RLock()
	defer s.analyticsRecomputerMu.RUnlock()
	out := map[string]*analyticsRecomputer{}
	v := reflect.ValueOf(s).Elem()
	rt := reflect.TypeOf((*analyticsRecomputer)(nil))
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Type() != rt {
			continue
		}
		rc := (*analyticsRecomputer)(f.UnsafePointer())
		out[v.Type().Field(i).Name] = rc
	}
	return out
}

func TestPauseWhenIdle_ReachesEveryRecomputer(t *testing.T) {
	for _, on := range []bool{false, true} {
		db := setupTestDB(t)
		s := NewPacketStore(db, nil)
		stop := s.StartAnalyticsRecomputers(time.Hour, AnalyticsRecomputeIntervals{PauseWhenIdle: on})
		fields := storeRecomputerFields(t, s)
		if len(fields) != 10 {
			t.Fatalf("expected 10 analytics recomputer fields, found %d: %v", len(fields), fields)
		}
		s.analyticsRecomputerMu.RLock()
		listed := s.analyticsRecomputersLocked()
		s.analyticsRecomputerMu.RUnlock()
		if len(listed) != len(fields) {
			t.Fatalf("analyticsRecomputersLocked lists %d, store has %d", len(listed), len(fields))
		}
		for name, rc := range fields {
			if rc == nil {
				t.Fatalf("%s not started", name)
			}
			if rc.pauseWhenIdle != on {
				t.Errorf("pause=%v: %s.pauseWhenIdle = %v", on, name, rc.pauseWhenIdle)
			}
		}
		// roles reads the nodes-clock-skew snapshot (GetFleetClockSkew):
		// that dependency must be declared so a paused clock-skew is
		// refreshed before roles computes over it.
		if deps := fields["recompRoles"].deps; len(deps) != 1 || deps[0] != fields["recompNodesClockSkew"] {
			t.Errorf("roles deps = %v, want [nodes-clock-skew]", deps)
		}
		stop()
		db.Close()
	}
}

// --- loop behaviour -----------------------------------------------------

func TestPauseWhenIdle_DefaultOffComputesEveryUnreadTick(t *testing.T) {
	h := newPauseHarness(t, "off", false, countingPass).start()
	for i := 0; i < 5; i++ {
		h.expectTick("tick-run")
	}
	if got := h.r.ComputeRuns(); got != 6 {
		t.Fatalf("ComputeRuns = %d, want 6 (start + 5 ticks)", got)
	}
	if h.r.paused.Load() {
		t.Fatal("pause flag set with pauseWhenIdle off")
	}
}

func TestPauseWhenIdle_UnreadTicksSkip(t *testing.T) {
	h := newPauseHarness(t, "idle", true, countingPass).start()
	for i := 0; i < 5; i++ {
		h.expectTick("tick-skip")
	}
	if got := h.r.ComputeRuns(); got != 1 {
		t.Fatalf("ComputeRuns = %d, want 1 (only the start pass)", got)
	}
	if !h.r.paused.Load() {
		t.Fatal("an unread recomputer is not marked paused")
	}
}

func TestPauseWhenIdle_ContinuousReadsKeepComputing(t *testing.T) {
	h := newPauseHarness(t, "read", true, countingPass).start()
	for i := 0; i < 5; i++ {
		if v := h.r.Load(); v != int64(i+1) {
			t.Fatalf("read %d got %v, want the latest pass %d", i, v, i+1)
		}
		h.expectTick("tick-run")
	}
	if got := h.r.ComputeRuns(); got != 6 {
		t.Fatalf("ComputeRuns = %d, want 6", got)
	}
	if len(h.r.kick) != 0 {
		t.Fatal("a recomputer that was never paused queued a wake-up")
	}
	h.noPendingEvent()
}

// A read while a pass is computing may have got the previous snapshot, so
// it counts towards the next tick: the pass clears the read flag before
// computing, not after.
func TestPauseWhenIdle_ReadDuringPassCounts(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	h := newPauseHarness(t, "read-during-pass", true, func(pass int64) interface{} {
		if pass == 2 {
			close(started)
			<-release
		}
		return pass
	}).start()
	h.r.Load() // so the next tick computes
	select {
	case h.tk.c <- time.Now():
	case <-time.After(pauseTestDeadline):
		t.Fatal("loop did not accept a tick")
	}
	<-started
	if v := h.r.Load(); v != int64(1) { // read while pass 2 runs
		t.Fatalf("read during the pass got %v, want the previous snapshot", v)
	}
	close(release)
	h.expect("tick-run")
	h.expectTick("tick-run") // the read during pass 2 is not forgotten
	h.expectTick("tick-skip")
}

// The first read after idle returns the existing snapshot while the wake
// pass is still blocked, a burst of concurrent readers coalesces into ONE
// pass, and later reads see the refreshed snapshot.
func TestPauseWhenIdle_WakeServesSnapshotAndCoalesces(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	h := newPauseHarness(t, "wake", true, func(pass int64) interface{} {
		if pass > 1 {
			started <- struct{}{}
			<-release
		}
		return pass
	}).start()
	h.expectTick("tick-skip")

	// 64 concurrent readers, each must return the old snapshot without
	// waiting for the blocked pass.
	const readers = 64
	var wg sync.WaitGroup
	var wrong atomic.Int64
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.r.Load() != int64(1) {
				wrong.Add(1)
			}
		}()
	}
	readersDone := make(chan struct{})
	go func() { wg.Wait(); close(readersDone) }()
	select {
	case <-readersDone:
	case <-time.After(pauseTestDeadline):
		t.Fatal("readers waited for the heavy computation")
	}
	if wrong.Load() != 0 {
		t.Fatalf("%d readers did not get the existing snapshot", wrong.Load())
	}
	select {
	case <-started:
	case <-time.After(pauseTestDeadline):
		t.Fatal("a read on a paused recomputer did not wake it")
	}
	if c := cap(h.r.kick); c != 1 {
		t.Fatalf("wake-up channel capacity = %d, want 1", c)
	}
	close(release)
	h.expect("kick-run")
	// The wake pass restarts the schedule, so the next tick comes a full
	// interval after it instead of marking the new snapshot stale at once.
	if got := h.tk.resets.Load(); got != 1 {
		t.Fatalf("ticker resets after the wake pass = %d, want 1", got)
	}

	// Barrier: RecomputeNow is served after any wake-up still queued, so
	// every event before it belongs to the burst.
	h.r.RecomputeNow()
	runs, ignored := 1, 0
	for ev := h.next(); ev != "recompute-now"; ev = h.next() {
		switch ev {
		case "kick-run":
			runs++
		case "kick-ignored":
			ignored++
		default:
			t.Fatalf("unexpected event %q during the burst", ev)
		}
	}
	if runs != 1 || ignored > 1 {
		t.Fatalf("burst of %d reads gave %d wake passes and %d ignored wake-ups, want 1 and <=1", readers, runs, ignored)
	}
	if got := h.r.ComputeRuns(); got != 3 {
		t.Fatalf("ComputeRuns = %d, want 3 (start + 1 wake + barrier)", got)
	}
	if v := h.r.Load(); v != int64(3) {
		t.Fatalf("read after the refresh got %v, want the refreshed snapshot", v)
	}
}

// A read landing between "pause armed" and "read checked" on the loop
// goroutine must still be served: either the tick computes, or the read
// queues a wake-up.
func TestPauseWhenIdle_NoLostWakeupInPauseWindow(t *testing.T) {
	h := newPauseHarness(t, "window", true, countingPass)
	var once sync.Once
	h.r.testHookPauseWindow = func() { once.Do(func() { h.r.Load() }) }
	h.start()
	switch ev := h.tick(); ev {
	case "tick-run":
	case "tick-skip":
		if next := h.next(); next != "kick-run" {
			t.Fatalf("read in the pause window was lost: skip followed by %q", next)
		}
	default:
		t.Fatalf("unexpected tick event %q", ev)
	}
	if got := h.r.ComputeRuns(); got != 2 {
		t.Fatalf("ComputeRuns = %d, want 2", got)
	}
}

func TestPauseWhenIdle_FirstComputationNeverPauses(t *testing.T) {
	// Start computes synchronously even with the opt-in on.
	h := newPauseHarness(t, "first", true, func(pass int64) interface{} {
		if pass <= 3 {
			return nil // no snapshot yet
		}
		return pass
	}).start()
	if h.r.ComputeRuns() != 1 {
		t.Fatalf("Start did not run the initial compute")
	}
	// Without a successful first snapshot unread ticks keep computing.
	h.expectTick("tick-run")
	h.expectTick("tick-run")
	if h.r.Load() != nil {
		t.Fatal("snapshot present before the first successful pass")
	}
	h.expectTick("tick-run") // pass 4 succeeds
	h.r.cache.Load()         // not a read: only Load counts
	h.expectTick("tick-skip")
	h.wakeWinsRace()
	if v := h.peek(); v != int64(4) {
		t.Fatalf("snapshot = %v, want pass 4", v)
	}
	h.wakeRead() // the read wakes the paused recomputer: pass 5
	if v := h.peek(); v != int64(5) {
		t.Fatalf("snapshot after the wake = %v, want pass 5", v)
	}
	h.noPendingEvent()
}

// Until a successful pass starts after the startup load, unread ticks keep
// computing; the post-load RecomputeNow runs even on a paused recomputer.
func TestPauseWhenIdle_StartupLoad(t *testing.T) {
	loaded := make(chan struct{})
	h := newPauseHarness(t, "startup", true, countingPass)
	h.r.pauseReadyGate = func() bool { return isClosed(loaded) }
	h.start()
	h.expectTick("tick-run") // load not done: no pause
	h.expectTick("tick-run")
	close(loaded)
	h.expectTick("tick-run") // first pass started after the load: settles
	h.expectTick("tick-skip")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); recomputeWhenLoaded(loaded, stop, []*analyticsRecomputer{h.r}) }()
	h.expect("recompute-now")
	<-done
	if got := h.r.ComputeRuns(); got != 5 {
		t.Fatalf("ComputeRuns = %d, want 5: the post-load pass was skipped", got)
	}
	h.expectTick("tick-skip")
}

// A failed pass is a panic or a nil result; both keep the recomputer from
// pausing.
func TestPauseWhenIdle_FailedComputationRecovers(t *testing.T) {
	for _, mode := range []string{"panic", "nil"} {
		t.Run(mode, func(t *testing.T) {
			fail := atomic.Bool{}
			h := newPauseHarness(t, "fail-"+mode, true, func(pass int64) interface{} {
				if !fail.Load() {
					return pass
				}
				if mode == "panic" {
					panic("compute failed")
				}
				return nil
			}).start()
			h.expectTick("tick-skip")

			// A failed wake pass keeps the old snapshot, still marked
			// stale, and the next tick retries even though nothing read it.
			fail.Store(true)
			if v := h.r.Load(); v != int64(1) {
				t.Fatalf("read got %v, want the existing snapshot", v)
			}
			h.expect("kick-run")
			if _, stale := h.r.SnapshotInfo(); !stale {
				t.Fatal("a failed refresh made the old snapshot look fresh")
			}
			h.expectTick("tick-run")
			h.expectTick("tick-run")
			if v := h.r.Load(); v != int64(1) {
				t.Fatalf("read after failed passes got %v, want the existing snapshot", v)
			}
			fail.Store(false)
			h.expectTick("tick-run") // recovers
			h.expectTick("tick-skip")
			h.wakeWinsRace()
			if v := h.peek(); v != int64(5) {
				t.Fatalf("snapshot = %v, want pass 5", v)
			}
			h.wakeRead()
			if v := h.peek(); v != int64(6) {
				t.Fatalf("snapshot after the wake = %v, want pass 6", v)
			}
			h.noPendingEvent()
		})
	}
}

func TestPauseWhenIdle_Shutdown(t *testing.T) {
	t.Run("while paused", func(t *testing.T) {
		h := newPauseHarness(t, "stop-paused", true, countingPass).start()
		h.expectTick("tick-skip")
		h.r.Stop()
		if !isClosed(h.r.done) {
			t.Fatal("Stop returned before the loop exited")
		}
		for i := 0; i < 10; i++ {
			if h.r.Load() != int64(1) {
				t.Fatal("Load after Stop lost the snapshot")
			}
		}
		if len(h.r.kick) > 1 {
			t.Fatal("wake-up queue grew past its bound")
		}
		h.r.RecomputeNow() // returns, does not hang
	})
	t.Run("during a wake pass", func(t *testing.T) {
		release := make(chan struct{})
		started := make(chan struct{})
		h := newPauseHarness(t, "stop-wake", true, func(pass int64) interface{} {
			if pass == 2 {
				close(started)
				<-release
			}
			return pass
		}).start()
		h.expectTick("tick-skip")
		h.r.Load()
		<-started
		stopped := make(chan struct{})
		go func() { h.r.Stop(); close(stopped) }()
		close(release)
		<-stopped
		if !isClosed(h.r.done) {
			t.Fatal("Stop did not join the loop")
		}
		if h.r.ComputeRuns() != 2 {
			t.Fatalf("ComputeRuns = %d, want 2", h.r.ComputeRuns())
		}
	})
	t.Run("dependency already stopped", func(t *testing.T) {
		dep := newPauseHarness(t, "dep", true, countingPass).start()
		r := newPauseHarness(t, "dependent", true, countingPass)
		r.r.deps = []*analyticsRecomputer{dep.r}
		r.start()
		dep.expectTick("tick-skip")
		r.expectTick("tick-skip")
		dep.r.Stop()
		r.r.Load()
		r.expect("kick-run")
	})
}

// A dependent that wakes refreshes its paused dependency FIRST, and the
// dependent reading the dependency's snapshot keeps the dependency awake.
func TestPauseWhenIdle_DependencyNotStarved(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(n string) { mu.Lock(); order = append(order, n); mu.Unlock() }

	dep := newPauseHarness(t, "nodes-clock-skew", true, func(pass int64) interface{} {
		note("dep")
		return pass
	}).start()
	var depSeen atomic.Int64
	rh := newPauseHarness(t, "roles", true, func(pass int64) interface{} {
		note("roles")
		depSeen.Store(dep.r.Load().(int64))
		return pass
	})
	rh.r.deps = []*analyticsRecomputer{dep.r}
	rh.start()

	dep.expectTick("tick-run") // roles' start pass read it
	dep.expectTick("tick-skip")
	rh.expectTick("tick-skip")
	mu.Lock()
	order = nil
	mu.Unlock()

	rh.r.Load() // a reader of roles only
	dep.expect("recompute-now")
	rh.expect("kick-run")
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"dep", "roles"}) {
		t.Fatalf("compute order = %v, want [dep roles]", got)
	}
	if want := dep.r.ComputeRuns(); depSeen.Load() != want {
		t.Fatalf("roles computed over dependency pass %d, want the refreshed pass %d", depSeen.Load(), want)
	}

	// roles keeps being read; its computes count as reads of the
	// dependency, so the dependency keeps ticking too.
	for i := 0; i < 3; i++ {
		rh.r.Load()
		rh.expectTick("tick-run")
		dep.expectTick("tick-run")
	}
}

// --- freshness ----------------------------------------------------------

func TestPauseWhenIdle_SnapshotInfoNeverFalselyFresh(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	h := newPauseHarness(t, "fresh", true, func(pass int64) interface{} {
		if pass == 2 {
			close(started)
			<-release
		}
		return pass
	}).start()
	at1, stale := h.r.SnapshotInfo()
	if stale || at1.IsZero() {
		t.Fatalf("start snapshot: at=%v stale=%v, want a fresh dated snapshot", at1, stale)
	}
	h.expectTick("tick-skip")
	if _, stale := h.r.SnapshotInfo(); !stale {
		t.Fatal("snapshot that missed a scheduled refresh is reported fresh")
	}
	h.r.Load()
	<-started // the wake pass is running, the snapshot is still the old one
	if at, stale := h.r.SnapshotInfo(); !stale || !at.Equal(at1) {
		t.Fatalf("during the wake pass: at=%v stale=%v, want the old snapshot reported stale", at, stale)
	}
	close(release)
	h.expect("kick-run")
	if at, stale := h.r.SnapshotInfo(); stale || !at.After(at1) && !at.Equal(at1) {
		t.Fatalf("after the refresh: at=%v stale=%v, want a fresh snapshot", at, stale)
	}
}

func TestPauseWhenIdle_DefaultOffNeverStale(t *testing.T) {
	h := newPauseHarness(t, "off-fresh", false, countingPass).start()
	h.expectTick("tick-run")
	if _, stale := h.r.SnapshotInfo(); stale {
		t.Fatal("stale with pauseWhenIdle off")
	}
}

// startRecomputersWithManualTickers starts s's analytics recomputers with
// one manual ticker per recomputer name (names must list all of them), so
// no scheduled tick fires on its own.
func startRecomputersWithManualTickers(t *testing.T, s *PacketStore, names []string, ov AnalyticsRecomputeIntervals) (func(), map[string]*manualRecomputeTicker) {
	t.Helper()
	tickers := make(map[string]*manualRecomputeTicker, len(names))
	for _, n := range names {
		tickers[n] = newManualRecomputeTicker()
	}
	prev := newRecomputeTicker
	// Called later on each loop goroutine; tickers is read-only by then.
	newRecomputeTicker = func(name string, _ time.Duration) recomputeTicker {
		tk, ok := tickers[name]
		if !ok {
			panic("no manual ticker for recomputer " + name)
		}
		return tk
	}
	// Each recomputer copies the factory when it is constructed.
	stop := s.StartAnalyticsRecomputers(time.Hour, ov)
	newRecomputeTicker = prev
	return stop, tickers
}

// Every default-shape endpoint served from a recomputer carries the
// snapshot headers when the opt-in is on, reports a skipped refresh as
// stale and gives the snapshot's exact age; non-default shapes (region,
// area, window) carry none. With the opt-in off no response has them.
func TestPauseWhenIdle_SnapshotHeaders(t *testing.T) {
	endpoints := []struct {
		path, field, name string
		other             []string // non-default shapes, not recomputer-served
	}{
		{"/api/analytics/rf", "recompRF", "rf", []string{"region=SJC", "area=fyn", "window=1h"}},
		{"/api/analytics/topology", "recompTopology", "topology", []string{"region=SJC", "area=fyn", "window=1h"}},
		{"/api/analytics/channels", "recompChannels", "channels", []string{"region=SJC", "area=fyn", "window=1h"}},
		{"/api/analytics/distance", "recompDistance", "distance", []string{"region=SJC", "area=fyn"}},
		{"/api/analytics/hash-sizes", "recompHashSizes", "hash-sizes", []string{"region=SJC", "area=fyn"}},
		{"/api/analytics/hash-collisions", "recompHashCollisions", "hash-collisions", []string{"region=SJC", "area=fyn"}},
		{"/api/analytics/roles", "recompRoles", "roles", nil},
		{"/api/observers/clock-skew", "recompObserversClockSkew", "observers-clock-skew", nil},
		{"/api/nodes/clock-skew", "recompNodesClockSkew", "nodes-clock-skew", []string{"area=fyn"}},
		// #245 M2: filtered shapes also read the snapshot, so none is listed.
		{"/api/analytics/advert-intervals", "recompAdvertIntervals", "advert-intervals", nil},
	}
	names := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		names = append(names, e.name)
	}
	for _, on := range []bool{false, true} {
		srv, router := setupTestServer(t)
		srv.cfg.Areas = map[string]AreaEntry{"fyn": {}}
		stop, tickers := startRecomputersWithManualTickers(t, srv.store, names, AnalyticsRecomputeIntervals{PauseWhenIdle: on})
		srv.store.signalStartupLoadDone()
		// Wait for the post-load pass of every recomputer (#116): it
		// stores a new snapshot, which would clear a stale mark set below.
		// It is the first Reset on each ticker, and with manual tickers
		// nothing else runs a pass from here on except this test.
		for _, n := range names {
			select {
			case <-tickers[n].resetC:
			case <-time.After(pauseTestDeadline):
				t.Fatalf("pause=%v %s: no post-load pass", on, n)
			}
		}
		srv.store.distLazyMu.Lock()
		srv.store.distLazyBuilding = true
		srv.store.distLazyMu.Unlock()
		srv.store.runDistanceIndexBuild() // synchronous lazy build (#1011)
		if !srv.store.DistanceIndexBuilt() {
			t.Fatal("distance index not built")
		}
		fields := storeRecomputerFields(t, srv.store)
		if len(fields) != len(endpoints) {
			t.Fatalf("%d recomputers, %d endpoints under test", len(fields), len(endpoints))
		}
		// Every loop is idle (its last pass is done), so swapping the clock
		// does not race; the passes below store snapshots dated clock.now().
		clock := newSnapshotClock()
		for _, rc := range fields {
			rc.now = clock.now
			rc.RecomputeNow()
		}
		clock.advance(125*time.Second + 900*time.Millisecond)
		get := func(path string) *httptest.ResponseRecorder {
			t.Helper()
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("pause=%v %s: status %d", on, path, w.Code)
			}
			return w
		}
		for _, e := range endpoints {
			for _, q := range e.other {
				w := get(e.path + "?" + q)
				if h := w.Header(); h.Get(analyticsSnapshotHeader) != "" || h.Get(analyticsSnapshotAgeHeader) != "" {
					t.Errorf("pause=%v %s?%s: snapshot headers on a shape not served from the recomputer", on, e.path, q)
				}
			}
			w := get(e.path)
			state, age := w.Header().Get(analyticsSnapshotHeader), w.Header().Get(analyticsSnapshotAgeHeader)
			if !on {
				if state != "" || age != "" {
					t.Errorf("%s: snapshot headers set with pauseWhenIdle off", e.path)
				}
				continue
			}
			if state != "current" || age != "125" {
				t.Errorf("%s: %s = %q, %s = %q, want current, 125", e.path, analyticsSnapshotHeader, state, analyticsSnapshotAgeHeader, age)
			}
			fields[e.field].missedRefresh.Store(true) // as after a skipped tick
			w = get(e.path)
			state, age = w.Header().Get(analyticsSnapshotHeader), w.Header().Get(analyticsSnapshotAgeHeader)
			if state != "stale" || age != "125" {
				t.Errorf("%s after a skipped refresh: %s = %q, %s = %q, want stale, 125", e.path, analyticsSnapshotHeader, state, analyticsSnapshotAgeHeader, age)
			}
		}
		stop()
	}
}

// --- measurement (manual) -----------------------------------------------

// TestPauseWhenIdle_Measure prints computation counts, allocations, CPU and
// read latency for idle vs visible traffic with the opt-in off and on. It
// sleeps by design (real tickers), so it only runs when asked:
//
//	PAUSE_IDLE_MEASURE=1 go test -run TestPauseWhenIdle_Measure -v ./cmd/server
func TestPauseWhenIdle_Measure(t *testing.T) {
	if testing.Short() || os.Getenv("PAUSE_IDLE_MEASURE") != "1" {
		t.Skip("set PAUSE_IDLE_MEASURE=1 to measure")
	}
	measurePauseWhenIdle(t)
}
