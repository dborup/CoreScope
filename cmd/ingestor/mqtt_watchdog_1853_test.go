package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// PR #1853: newAsyncEmit must make a blocked/slow sink invisible to the
// watchdog loop. #1749/#1810 already recover a PANIC in emit; these tests
// cover the sibling failure mode where emit BLOCKS instead (a
// backpressured Docker JSON-file log driver, a stuck stderr pipe) —
// without this fix that hangs runLivenessWatchdogLoop's per-tick work
// forever, which looks identical to #1749's original symptom (WATCHDOG
// log lines simply stop).

// TestNewAsyncEmit_NeverBlocksWhenWriterStuck is the core regression: a
// realEmit that blocks forever must not make the returned emit block.
func TestNewAsyncEmit_NeverBlocksWhenWriterStuck(t *testing.T) {
	block := make(chan struct{}) // never closed — realEmit hangs on it
	var calls atomic.Int64
	realEmit := func(args ...any) {
		calls.Add(1)
		<-block
	}
	emit, _ := newAsyncEmit(realEmit)

	// The drain goroutine will pick up the first call and hang inside
	// realEmit. Give it a moment to actually enter the hang before we
	// start flooding the queue, so this test is deterministic about
	// which call is "the one that's stuck" vs "queued behind it".
	done := make(chan struct{})
	go func() {
		emit("first")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first emit call blocked — newAsyncEmit's send is not non-blocking")
	}

	// Fill the queue past capacity; every one of these must return
	// immediately regardless of the stuck drain goroutine.
	start := time.Now()
	for i := 0; i < asyncEmitQueueSize+50; i++ {
		emit("flood")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("flooding emit took %s — a blocked sink should never make emit block", elapsed)
	}

	if got := WatchdogLogDropCount(); got == 0 {
		t.Error("WatchdogLogDropCount = 0, want > 0 after overflowing the queue")
	}
}

// TestNewAsyncEmit_DeliversWhenSinkIsHealthy is the non-degenerate case:
// a fast realEmit receives every call, in order, with no drops.
func TestNewAsyncEmit_DeliversWhenSinkIsHealthy(t *testing.T) {
	var mu sync.Mutex
	var got []string
	realEmit := func(args ...any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, args[0].(string))
	}
	emit, stop := newAsyncEmit(realEmit)

	for _, s := range []string{"a", "b", "c"} {
		emit(s)
	}
	stop() // blocks until the queue is fully drained

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("got %v, want [a b c] in order", got)
	}
}

// TestNewAsyncEmit_StopWaitsForDrain proves stop() is a real flush, not a
// fire-and-forget close — a caller that logs the final state right after
// stop() must see everything that was queued before stop() was called.
func TestNewAsyncEmit_StopWaitsForDrain(t *testing.T) {
	var count atomic.Int64
	realEmit := func(args ...any) {
		time.Sleep(time.Millisecond) // enough to make a race visible
		count.Add(1)
	}
	emit, stop := newAsyncEmit(realEmit)

	const n = 20
	for i := 0; i < n; i++ {
		emit("x")
	}
	stop()

	if got := count.Load(); got != n {
		t.Errorf("count after stop() = %d, want %d — stop must wait for the drain goroutine", got, n)
	}
}

// TestRunLivenessWatchdog_StopDoesNotHangOnStuckSink is the integration
// regression: runLivenessWatchdog's stop() must return promptly even
// when the real-world emit sink (here, a stand-in for log.Print) is
// stalled, and must not panic with "send on closed channel" from the
// loop goroutine's in-flight emit call racing stopEmit's queue close.
func TestRunLivenessWatchdog_StopDoesNotHangOnStuckSink(t *testing.T) {
	// This exercises the production wiring in runLivenessWatchdog
	// directly (not runLivenessWatchdogLoop in isolation), so it needs
	// a real ticker on a short interval rather than the synthetic tick
	// channel setupWatchdogTestLoop uses.
	stop := runLivenessWatchdog(5*time.Millisecond, time.Second)

	// Let it tick a few times so the loop goroutine is definitely live
	// and has called emit via the async queue at least once.
	time.Sleep(30 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLivenessWatchdog's stop() hung — shutdown ordering regression")
	}
}
