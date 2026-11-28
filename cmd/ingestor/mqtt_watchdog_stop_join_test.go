package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartWatchdogTestLoop_StopJoinsLoop guards the test harness, not
// production code. The stop returned by startWatchdogTestLoop must not return
// while the loop goroutine is still inside a tick, because the caller restores
// livenessRegistry right after it and the next test registers its own sources
// there. A stop that only closes done lets the old loop finish that tick, and
// possibly scan the next test's registry, with its own fabricated clock. That
// is how TestMQTTStallWatchdog_DisconnectedEscalationThrottled_1749 saw two
// forced reconnects.
//
// The loop is held inside a tick by an emit callback that blocks until the
// test releases it. Everything is ordered by channels:
//
//   - emitEntered proves the loop is inside the tick before stop is called.
//   - done being closed proves the stop goroutine has actually run stop and
//     got past close(done). Without this, "stop has not returned" could only
//     mean the goroutine had not been scheduled yet.
//   - stopReturned is then checked while emit is still blocked. A close-only
//     stop returns right after close(done) and is caught here. A joining stop
//     cannot return at all until release, so nothing timing-based can make
//     the correct helper fail. notReturnedWindow only bounds how long a
//     close-only stop gets to show itself.
//   - As a second check, the stop goroutine records whether exited was
//     already closed at the moment stop returned. A joining stop always sees
//     it closed.
//
// The time.After bounds are safety nets that turn a hang into a failure.
// They do not order anything.
func TestStartWatchdogTestLoop_StopJoinsLoop(t *testing.T) {
	defer snapshotAndResetRegistry(t)()

	const (
		safetyTimeout     = 5 * time.Second
		notReturnedWindow = 100 * time.Millisecond
	)

	// Connected but silent for 10m against a 1m threshold: the first tick
	// classifies it as LivenessStalled and calls emit.
	s := &SourceLivenessState{
		Tag:           "stop-join-harness",
		Broker:        "tcp://x:1883",
		IsConnectedFn: func() bool { return true },
	}
	atomic.StoreInt64(&s.LastMessageUnix, time.Now().Add(-10*time.Minute).Unix())
	if err := registerLivenessState(s); err != nil {
		t.Fatalf("setup: %v", err)
	}

	emitEntered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseEmit := func() { releaseOnce.Do(func() { close(release) }) }
	emit := func(...any) {
		enteredOnce.Do(func() { close(emitEntered) })
		<-release
	}

	tick, done, exited := setupWatchdogTestLoop(t, time.Minute, emit)
	stop := joiningWatchdogStop(done, exited)
	// Deferred calls run last-in first-out. On every path, including
	// t.Fatal, emit is released first, then the stop goroutine (if started)
	// is joined, then stop joins the loop, then the registry is restored.
	defer stop()

	sendTickOrFail(t, tick, time.Now(), safetyTimeout, "tick into stalled source")

	var stopStarted bool
	stopReturned := make(chan struct{})
	var exitedWhenStopReturned atomic.Bool
	defer func() {
		if !stopStarted {
			return
		}
		select {
		case <-stopReturned:
		case <-time.After(safetyTimeout):
			t.Errorf("stop goroutine still running %s after release", safetyTimeout)
		}
	}()
	defer releaseEmit()

	select {
	case <-emitEntered:
	case <-time.After(safetyTimeout):
		t.Fatalf("loop never called emit within %s", safetyTimeout)
	}

	stopStarted = true
	go func() {
		defer close(stopReturned)
		stop()
		select {
		case <-exited:
			exitedWhenStopReturned.Store(true)
		default:
		}
	}()

	select {
	case <-done:
	case <-time.After(safetyTimeout):
		t.Fatalf("stop did not close done within %s", safetyTimeout)
	}

	select {
	case <-stopReturned:
		t.Fatal("stop returned while the loop was still blocked inside a tick; it must wait for the loop to exit")
	case <-exited:
		t.Fatal("loop exited while emit was still blocked")
	case <-time.After(notReturnedWindow):
	}

	releaseEmit()

	select {
	case <-exited:
	case <-time.After(safetyTimeout):
		t.Fatalf("loop did not exit within %s after emit was released", safetyTimeout)
	}
	select {
	case <-stopReturned:
	case <-time.After(safetyTimeout):
		t.Fatalf("stop did not return within %s after the loop exited", safetyTimeout)
	}
	if !exitedWhenStopReturned.Load() {
		t.Fatal("stop returned before the loop goroutine had exited")
	}
}
