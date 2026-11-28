package main

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Issue #102: while paho is in its initial ConnectRetry loop (status
// connecting), the watchdog's force-reconnect must not log the expected
// Connect() status error as a failure. Issue #103: a force-reconnect still in
// flight when the watchdog stops must not panic with "send on closed channel".

const (
	connectFailedLine102 = "WATCHDOG force-reconnect Connect() failed"
	retryPending102      = "paho reports a retry pending"
	// retryPendingErr102 is the start of the retry-pending line: the line keeps
	// paho's error, because IsConnected() is a hint and not a guarantee.
	retryPendingErr102 = "WATCHDOG force-reconnect: Connect() returned " + pahoErrStatusMustBeDisconnected + "; " + retryPending102
)

// newNeverConnectedTestClient builds the client as main does, against a
// broker that is down from the start, so paho stays in its initial
// ConnectRetry loop (status connecting). It waits for the first CONNECT.
func newNeverConnectedTestClient(t *testing.T, b *forceReconnectTestBroker, tag string, retryInterval time.Duration) mqtt.Client {
	t.Helper()
	b.goDown()
	opts := buildMQTTOpts(MQTTSource{Broker: b.url(), Name: tag}).
		SetConnectTimeout(500 * time.Millisecond).
		SetWriteTimeout(500 * time.Millisecond).
		SetMaxReconnectInterval(forceReconnectTestRetryInterval).
		SetConnectRetryInterval(retryInterval)
	client := mqtt.NewClient(opts)
	client.Connect()
	t.Cleanup(func() { client.Disconnect(0) })
	waitForConnects(t, b, 0, "test setup: initial connect")
	return client
}

// #102: five triggers during the initial ConnectRetry loop (the #29 review
// measured 5/5). Connect() returns paho's errStatusMustBeDisconnected each
// time; paho keeps retrying, so this is not a failure. The client must not
// log "Connect() failed", must log paho's error with the retry it reports
// pending, and paho's loop must keep going and connect once the broker is up.
func TestForceReconnect_RealPaho_InitialRetryLoopIsNotAConnectFailure_102(t *testing.T) {
	b := newForceReconnectTestBroker(t)
	logs := captureLog118(t)
	client := newNeverConnectedTestClient(t, b, "force-reconnect-initial", 200*time.Millisecond)

	// Premise: status connecting looks like reconnecting to the watchdog.
	if !client.IsConnected() || client.IsConnectionOpen() {
		t.Fatalf("test setup: IsConnected=%v IsConnectionOpen=%v, want true/false (initial ConnectRetry loop)", client.IsConnected(), client.IsConnectionOpen())
	}
	fn := buildForceReconnectFn(client, "force-reconnect-initial")
	for i := 0; i < 5; i++ {
		fn()
	}
	if n := strings.Count(logs.String(), connectFailedLine102); n != 0 {
		t.Errorf("%d %q lines while paho was in its initial retry loop:\n%s", n, connectFailedLine102, logs)
	}
	if n := strings.Count(logs.String(), retryPendingErr102); n != 5 {
		t.Errorf("%d %q lines, want 5:\n%s", n, retryPendingErr102, logs)
	}

	n, _ := b.counts()
	waitForConnects(t, b, n, "paho's retry loop after the triggers")
	b.goUp()
	if !pollUntil(forceReconnectTestDeadline, client.IsConnectionOpen) {
		t.Fatal("client did not connect after the broker came back")
	}
}

// #102: the same paho error is a genuine failure when nothing will retry.
// Disconnect() during the initial retry loop moves paho to disconnecting
// without a reconnect, and blocks there until the retry sleep ends. A
// force-reconnect in that window gets errStatusMustBeDisconnected too, but
// IsConnected() is false: no attempt will follow, so it stays an error.
func TestForceReconnect_RealPaho_ConnectErrorWhileDisconnectingIsLogged_102(t *testing.T) {
	b := newForceReconnectTestBroker(t)
	logs := captureLog118(t)
	client := newNeverConnectedTestClient(t, b, "force-reconnect-disconnecting", 3*time.Second)

	go client.Disconnect(0)
	if !pollUntil(time.Second, func() bool { return !client.IsConnected() }) {
		t.Fatal("test setup: client did not leave status connecting")
	}
	buildForceReconnectFn(client, "force-reconnect-disconnecting")()

	if n := strings.Count(logs.String(), connectFailedLine102); n != 1 {
		t.Errorf("%d %q lines, want 1:\n%s", n, connectFailedLine102, logs)
	}
	if n := strings.Count(logs.String(), retryPending102); n != 0 {
		t.Errorf("a Connect() error with no retry pending was logged as %q:\n%s", retryPending102, logs)
	}
}

// #102 review F1: IsConnected() in status disconnecting reflects paho's
// willReconnect, which a Disconnect() leaves set. Here paho is reconnecting
// after a connection loss (willReconnect=true) when Disconnect() runs; status
// is then disconnecting, IsConnected() stays true, and Connect() returns
// errStatusMustBeDisconnected, yet no retry follows: paho ends disconnected.
// So the retry-pending line must keep paho's error and must not claim more.
func TestForceReconnect_RealPaho_DisconnectWhileReconnectingKeepsConnectError_102(t *testing.T) {
	b := newForceReconnectTestBroker(t)
	logs := captureLog118(t)
	// A 3s cap keeps paho's sleep between attempts (1s, then 2s) far above
	// the 5ms polling below, so the disconnecting window cannot be missed.
	opts := buildMQTTOpts(MQTTSource{Broker: b.url(), Name: "force-reconnect-sticky"}).
		SetConnectTimeout(500 * time.Millisecond).
		SetWriteTimeout(500 * time.Millisecond).
		SetMaxReconnectInterval(3 * time.Second)
	client := mqtt.NewClient(opts)
	client.Connect()
	t.Cleanup(func() { client.Disconnect(0) })
	if !pollUntil(forceReconnectTestDeadline, client.IsConnectionOpen) {
		t.Fatal("test setup: client never connected to the test broker")
	}

	// Connection lost: paho's AutoReconnect loop starts (willReconnect=true)
	// and sleeps after its first failed attempt.
	n, _ := b.counts()
	b.goDown()
	waitForConnects(t, b, n, "test setup: paho's reconnect loop")

	// Disconnect() moves reconnecting to disconnecting and waits for the
	// loop's sleep to end; willReconnect stays true. Connect() is a no-op
	// probe here: a success token while reconnecting, the status error once
	// disconnecting.
	client.Disconnect(0)
	if !pollUntil(time.Second, func() bool { return client.Connect().Error() != nil }) {
		t.Fatal("test setup: paho never reached status disconnecting")
	}
	if !client.IsConnected() || client.IsConnectionOpen() {
		t.Fatalf("test setup: IsConnected=%v IsConnectionOpen=%v, want true/false (disconnecting, willReconnect set)", client.IsConnected(), client.IsConnectionOpen())
	}

	buildForceReconnectFn(client, "force-reconnect-sticky")()
	if n := strings.Count(logs.String(), retryPendingErr102); n != 1 {
		t.Errorf("%d %q lines, want 1 (the line must keep paho's error):\n%s", n, retryPendingErr102, logs)
	}

	// The reported retry never comes: with the broker back up, paho still
	// ends disconnected rather than connected.
	b.goUp()
	if !pollUntil(forceReconnectTestDeadline, func() bool { return !client.IsConnected() }) {
		t.Fatal("paho did not settle to disconnected after Disconnect(); the scenario no longer shows that IsConnected() is only a hint")
	}
}

// #102: only paho's status error is recognised. Any other Connect() error
// is logged as a failure, whatever IsConnected() reports.
func TestBuildForceReconnectFn_OtherConnectErrorIsLogged_102(t *testing.T) {
	logs := captureLog118(t)
	c := &fakeClient{isConnected: true, connectErr: errors.New("network unreachable")}
	buildForceReconnectFn(c, "other-error-source")()
	if n := strings.Count(logs.String(), connectFailedLine102+": network unreachable"); n != 1 {
		t.Errorf("Connect() error not logged as a failure:\n%s", logs)
	}
	if strings.Count(logs.String(), retryPending102) != 0 {
		t.Errorf("an unrelated Connect() error was logged as %q:\n%s", retryPending102, logs)
	}
}

// #103: emit after stop is a no-op. The force-reconnect goroutine can emit
// after the watchdog has stopped; that must not panic.
func TestNewAsyncEmit_EmitAfterStopDoesNotPanic_103(t *testing.T) {
	var delivered atomic.Int64
	emit, stop := newAsyncEmit(func(...any) { delivered.Add(1) })
	emit("before stop")
	stop()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("emit after stop panicked: %v", r)
			}
		}()
		emit("after stop")
	}()
	if got := delivered.Load(); got != 1 {
		t.Fatalf("delivered %d lines, want 1 (the line before stop)", got)
	}
}

// #103 review F2: a line dropped after stop is not a "queue full" drop, so it
// must not count in watchdogLogDropCount.
func TestNewAsyncEmit_EmitAfterStopIsNotCountedAsDrop_103(t *testing.T) {
	emit, stop := newAsyncEmit(func(...any) {})
	stop()
	before := WatchdogLogDropCount()
	for i := 0; i < asyncEmitQueueSize+10; i++ {
		emit("after stop", i)
	}
	if got := WatchdogLogDropCount() - before; got != 0 {
		t.Fatalf("emit after stop counted %d drops in watchdogLogDropCount, want 0", got)
	}
}

// #103: emits racing stop neither panic nor race (run with -race). Every
// emitter is already emitting when stop runs and keeps going until after it
// has returned. A single round can miss a narrow window in stop (review F3:
// a stop that closes the queue before it marks itself stopped panics in
// about half the rounds without -race and fewer with it), so it runs many.
func TestNewAsyncEmit_ConcurrentEmitDuringStop_103(t *testing.T) {
	for round := 0; round < 200; round++ {
		if r := concurrentEmitDuringStopRound(); r != nil {
			t.Fatalf("round %d: emit racing stop panicked: %v", round, r)
		}
	}
}

// concurrentEmitDuringStopRound runs one round of
// TestNewAsyncEmit_ConcurrentEmitDuringStop_103 and returns the first panic.
func concurrentEmitDuringStopRound() any {
	emit, stop := newAsyncEmit(func(...any) {})
	stopped := make(chan struct{})
	panics := make(chan any, 8)
	var running, wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		running.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			emit("first line")
			running.Done()
			for {
				select {
				case <-stopped:
					for i := 0; i < 100; i++ {
						emit("after stop", i)
					}
					return
				default:
					emit("line")
				}
			}
		}()
	}
	running.Wait()
	stop()
	close(stopped)
	wg.Wait()
	close(panics)
	return <-panics
}

// #103, the scenario from the issue: the production watchdog fires a
// force-reconnect whose ForceReconnectFn blocks, the watchdog is stopped
// (SIGTERM path), then ForceReconnectFn returns and its goroutine emits
// "reconnect attempt issued". No panic; stop does not wait for the blocked
// reconnect.
func TestRunLivenessWatchdog_ForceReconnectInFlightAtShutdown_103(t *testing.T) {
	defer snapshotAndResetRegistry(t)()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s := &SourceLivenessState{
		Tag:           "shutdown-103",
		Broker:        "test",
		IsConnectedFn: func() bool { return true },
		ForceReconnectFn: func() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		},
	}
	atomic.StoreInt64(&s.LastMessageUnix, time.Now().Add(-time.Hour).Unix())
	if err := registerLivenessState(s); err != nil {
		t.Fatal(err)
	}

	stop := runLivenessWatchdog(5*time.Millisecond, time.Second)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		stop()
		close(release)
		t.Fatal("the watchdog never called ForceReconnectFn")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("stop() waited for a blocked ForceReconnectFn")
	}
	close(release)
	if !pollUntil(2*time.Second, func() bool { return !goroutineRunning("maybeForceReconnect") }) {
		t.Fatal("the force-reconnect goroutine did not finish")
	}
}

// goroutineRunning reports whether any goroutine's stack mentions fn.
func goroutineRunning(fn string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), fmt.Sprintf(".%s.", fn))
}
