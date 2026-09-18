package main

import (
	"sync"
	"testing"
	"time"
)

// setupWatchdogTestLoop spawns runLivenessWatchdogLoop with a panic
// safety net so an unrecovered panic in the loop (the historical bug
// shape #1749 / #1810 round-1 gates against) does NOT crash the test
// binary. Returns:
//   - tick: send fabricated timestamps to drive the loop
//   - done: caller closes to ask the loop to exit cleanly
//   - exited: closed by the helper after the loop returns (whether by
//     done-signal, normal channel close, or panic propagation)
//
// Extracted as part of #1810 round-1 (adv #4) — four tests in this
// package previously open-coded the same scaffolding with subtle
// variations.
func setupWatchdogTestLoop(t *testing.T, threshold time.Duration, emit func(...any)) (tick chan time.Time, done chan struct{}, exited chan struct{}) {
	t.Helper()
	tick = make(chan time.Time)
	done = make(chan struct{})
	exited = make(chan struct{})
	go func() {
		defer func() {
			_ = recover() // safety net; the production loop is what we are asserting on
			close(exited)
		}()
		runLivenessWatchdogLoop(tick, done, threshold, emit)
	}()
	return tick, done, exited
}

// sendTickOrFail pushes one fabricated timestamp into the loop's tick
// channel and fails the test if the loop does not consume it within
// timeout. Use this everywhere a test wants to step the watchdog by
// exactly one tick — open-coded select-default-Fatal patterns are
// repetitive and easy to get wrong (off-by-one timeouts).
func sendTickOrFail(t *testing.T, tick chan<- time.Time, stamp time.Time, timeout time.Duration, label string) {
	t.Helper()
	select {
	case tick <- stamp:
	case <-time.After(timeout):
		t.Fatalf("%s: tick blocked after %s — loop dead?", label, timeout)
	}
}

// startWatchdogTestLoop is setupWatchdogTestLoop for the common case: a test
// that wants the loop running for its own duration and nothing more. The
// returned stop closes done AND waits for the loop goroutine to return. It is
// safe to call more than once.
//
// Waiting is the part that must not be skipped. Closing done only asks the
// loop to stop; the goroutine can still be inside a scan, and that scan walks
// the package-level livenessRegistry. The next test registers its own source
// there, so a loop that has not returned yet will process that source on a
// tick carrying the previous test's fabricated clock.
//
// That is measured, not theoretical. With four call sites closing done and
// walking away, TestMQTTStallWatchdog_DisconnectedEscalationThrottled_1749
// failed intermittently when run with the other watchdog tests and not when
// run on its own. A trace of one failure showed the loop from
// TestMQTTStallWatchdog_EscalateOnPersistentDisconnect_1749, whose last tick
// carried a clock 420s ahead, still running after its test returned. The
// throttle test's own loop had already forced a reconnect and stamped
// LastForceReconnectUnix. The old loop then read that non-zero stamp, measured
// it against its own clock, saw more than forceReconnectThrottle elapse and
// forced a second reconnect. The throttle itself was correct; it was given two
// clocks for one source.
func startWatchdogTestLoop(t *testing.T, threshold time.Duration, emit func(...any)) (tick chan time.Time, stop func()) {
	t.Helper()
	tick, done, exited := setupWatchdogTestLoop(t, threshold, emit)
	return tick, joiningWatchdogStop(done, exited)
}

// joiningWatchdogStop returns the stop function used by startWatchdogTestLoop:
// close done once, then wait for exited. It is separate only so
// TestStartWatchdogTestLoop_StopJoinsLoop can hold done and exited itself.
func joiningWatchdogStop(done, exited chan struct{}) func() {
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// TestStartWatchdogTestLoop_StopIsIdempotent turns the "safe to call more
// than once" claim in startWatchdogTestLoop's doc comment above into an
// executable assertion. joiningWatchdogStop's returned func uses sync.Once
// to guard close(done), so a second call skips that close and falls
// straight to <-exited — which returns immediately once exited is closed,
// on every subsequent read. If either the once-guard or that closed-channel
// behaviour ever regressed, a second stop() call would hang instead of
// returning immediately, and this test would time out.
func TestStartWatchdogTestLoop_StopIsIdempotent(t *testing.T) {
	_, stop := startWatchdogTestLoop(t, time.Minute, func(args ...any) {})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
		stop()
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("calling stop twice blocked; stop must remain idempotent")
	}
}
