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
	retryInProgress102   = "retry already in progress"
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
// log "Connect() failed", must log that a retry is in progress, and paho's
// loop must keep going and connect once the broker is up.
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
	if n := strings.Count(logs.String(), retryInProgress102); n != 5 {
		t.Errorf("%d %q lines, want 5:\n%s", n, retryInProgress102, logs)
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
	if n := strings.Count(logs.String(), retryInProgress102); n != 0 {
		t.Errorf("a Connect() error with no retry pending was logged as %q:\n%s", retryInProgress102, logs)
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
	if strings.Count(logs.String(), retryInProgress102) != 0 {
		t.Errorf("an unrelated Connect() error was logged as %q:\n%s", retryInProgress102, logs)
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

// #103: emits racing stop neither panic nor race (run with -race). Every
// emitter is already emitting when stop runs and keeps going until after it
// has returned.
func TestNewAsyncEmit_ConcurrentEmitDuringStop_103(t *testing.T) {
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
	for r := range panics {
		t.Fatalf("emit racing stop panicked: %v", r)
	}
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
