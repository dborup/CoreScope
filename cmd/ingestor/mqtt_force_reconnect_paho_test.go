package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Behavioural tests for buildForceReconnectFn (upstream #1897) against a REAL
// paho client and a small in-test broker.
//
// The fake-client tests in mqtt_force_reconnect_race_test.go pin the call
// sequence. They cannot see what the fix is actually for: whether paho's own
// retry loop is still alive after the watchdog has fired. A Disconnect moved
// into a goroutine keeps the call sequence intact and kills the retry loop
// all the same (review of PR #29: mutants M15/M18). These tests assert the
// outcome instead, counted at the broker:
//
//   - broker down, paho retrying, watchdog fires repeatedly: CONNECT packets
//     keep arriving, and the client reconnects on its own once the broker is
//     back (no further trigger);
//   - genuine half-open socket (IsConnectionOpen()==true, broker silent): the
//     force-reconnect drops the old socket and a new session comes up;
//   - paho has given up (status disconnected, the #1749 escalation case):
//     the force-reconnect starts a fresh connect without blocking.
//
// Timings are shortened from buildMQTTOpts' production values so the whole
// file runs in a few seconds. The one ratio that matters is kept:
// forceReconnectTestRetryInterval is well above Disconnect's 250ms quiesce, as
// production's 30s MaxReconnectInterval is, so a Disconnect issued while paho
// sleeps between attempts is still blocked in "disconnecting" when Connect()
// runs — the exact #1897 failure.

const (
	forceReconnectTestRetryInterval = 800 * time.Millisecond
	forceReconnectTestDeadline      = 4 * time.Second
)

type testBrokerMode int32

const (
	testBrokerUp   testBrokerMode = iota // CONNECT → CONNACK accepted
	testBrokerDown                       // CONNECT is read and counted, then the socket is closed
)

// forceReconnectTestBroker speaks just enough MQTT 3.1.1 for paho: CONNECT →
// CONNACK, PINGREQ → PINGRESP, DISCONNECT → close.
type forceReconnectTestBroker struct {
	ln   net.Listener
	mode atomic.Int32

	mu       sync.Mutex
	connects int // CONNECT packets received, in any mode
	accepted int // sessions that got a CONNACK
	sessions []*testBrokerSession
	wg       sync.WaitGroup
}

type testBrokerSession struct {
	conn   net.Conn
	silent atomic.Bool   // half-open: read everything, answer nothing
	closed chan struct{} // closed once the client side has gone away
}

func newForceReconnectTestBroker(t *testing.T) *forceReconnectTestBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("test broker listen: %v", err)
	}
	b := &forceReconnectTestBroker{ln: ln}
	b.wg.Add(1)
	go b.acceptLoop()
	t.Cleanup(b.close)
	return b
}

func (b *forceReconnectTestBroker) url() string { return "tcp://" + b.ln.Addr().String() }

func (b *forceReconnectTestBroker) acceptLoop() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go b.serve(conn)
	}
}

func (b *forceReconnectTestBroker) serve(conn net.Conn) {
	defer b.wg.Done()
	defer conn.Close()
	pkt, err := packets.ReadPacket(conn)
	if err != nil {
		return
	}
	if _, ok := pkt.(*packets.ConnectPacket); !ok {
		return
	}
	b.mu.Lock()
	b.connects++
	b.mu.Unlock()
	if testBrokerMode(b.mode.Load()) == testBrokerDown {
		return
	}
	connack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	connack.ReturnCode = packets.Accepted
	if connack.Write(conn) != nil {
		return
	}
	s := &testBrokerSession{conn: conn, closed: make(chan struct{})}
	defer close(s.closed)
	b.mu.Lock()
	b.accepted++
	b.sessions = append(b.sessions, s)
	b.mu.Unlock()
	for {
		pkt, err := packets.ReadPacket(conn)
		if err != nil {
			return
		}
		if s.silent.Load() {
			continue
		}
		switch pkt.(type) {
		case *packets.PingreqPacket:
			if packets.NewControlPacket(packets.Pingresp).Write(conn) != nil {
				return
			}
		case *packets.DisconnectPacket:
			return
		}
	}
}

func (b *forceReconnectTestBroker) counts() (connects, accepted int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects, b.accepted
}

func (b *forceReconnectTestBroker) session(i int) *testBrokerSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[i]
}

// goDown drops every live session (paho sees EOF and starts its reconnect
// loop) and makes later CONNECTs fail.
func (b *forceReconnectTestBroker) goDown() {
	b.mode.Store(int32(testBrokerDown))
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sessions {
		s.conn.Close()
	}
}

func (b *forceReconnectTestBroker) goUp() { b.mode.Store(int32(testBrokerUp)) }

// silenceSessions turns every live session half-open: the socket stays up
// but the broker never sends another byte.
func (b *forceReconnectTestBroker) silenceSessions() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sessions {
		s.silent.Store(true)
	}
}

func (b *forceReconnectTestBroker) close() {
	b.ln.Close()
	b.mu.Lock()
	for _, s := range b.sessions {
		s.conn.Close()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// newForceReconnectTestClient builds the client exactly as main does
// (buildMQTTOpts: AutoReconnect, ConnectRetry, 30s keepalive) with shortened
// timeouts, and waits for the first session.
func newForceReconnectTestClient(t *testing.T, b *forceReconnectTestBroker, tag string) mqtt.Client {
	t.Helper()
	opts := buildMQTTOpts(MQTTSource{Broker: b.url(), Name: tag}).
		SetConnectTimeout(500 * time.Millisecond).
		SetWriteTimeout(500 * time.Millisecond).
		SetMaxReconnectInterval(forceReconnectTestRetryInterval).
		SetConnectRetryInterval(200 * time.Millisecond)
	client := mqtt.NewClient(opts)
	client.Connect()
	t.Cleanup(func() { client.Disconnect(0) })
	if !pollUntil(forceReconnectTestDeadline, client.IsConnectionOpen) {
		t.Fatal("test setup: client never connected to the test broker")
	}
	return client
}

// forceReconnectTestWatchdog drives the production watchdog action
// (maybeForceReconnect → ForceReconnectFn in a goroutine) with a synthetic
// clock that steps past forceReconnectThrottle, so each trigger really fires.
type forceReconnectTestWatchdog struct {
	state *SourceLivenessState
	now   time.Time

	mu    sync.Mutex
	emits []string
}

func newForceReconnectTestWatchdog(client mqtt.Client, tag string) *forceReconnectTestWatchdog {
	s := &SourceLivenessState{
		Tag:              tag,
		Broker:           "test",
		IsConnectedFn:    client.IsConnected,
		ForceReconnectFn: buildForceReconnectFn(client, tag),
	}
	atomic.StoreInt64(&s.LastMessageUnix, time.Now().Add(-time.Hour).Unix())
	return &forceReconnectTestWatchdog{state: s, now: time.Now()}
}

func (w *forceReconnectTestWatchdog) emit(args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(args) > 0 {
		if s, ok := args[0].(string); ok {
			w.emits = append(w.emits, s)
		}
	}
}

func (w *forceReconnectTestWatchdog) count(substr string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, e := range w.emits {
		if strings.Contains(e, substr) {
			n++
		}
	}
	return n
}

func (w *forceReconnectTestWatchdog) classify() LivenessKind {
	_, kind := checkSourceLiveness(w.state, 5*time.Minute, time.Now())
	return kind
}

// trigger fires one forced reconnect and waits for ForceReconnectFn to
// return. It must return promptly in every state: the watchdog issues it from
// a goroutine, and one that never returns leaks per trigger.
func (w *forceReconnectTestWatchdog) trigger(t *testing.T) {
	t.Helper()
	issued := w.count("reconnect attempt issued")
	maybeForceReconnect(w.state, w.now, w.emit)
	w.now = w.now.Add(forceReconnectThrottle + time.Second)
	if !pollUntil(2*time.Second, func() bool { return w.count("reconnect attempt issued") > issued }) {
		t.Fatal("ForceReconnectFn did not return within 2s")
	}
}

func pollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForConnects waits until the broker has seen more than `than` CONNECT
// packets and returns the new count.
func waitForConnects(t *testing.T, b *forceReconnectTestBroker, than int, why string) int {
	t.Helper()
	var n int
	if !pollUntil(forceReconnectTestDeadline, func() bool { n, _ = b.counts(); return n > than }) {
		t.Fatalf("%s: no CONNECT attempt within %s (still %d) — paho's retry loop is dead", why, forceReconnectTestDeadline, n)
	}
	return n
}

// The #1897 production failure. Old code: after the first trigger the broker
// never sees another CONNECT and the client never comes back.
func TestForceReconnect_RealPaho_RetryLoopSurvivesRepeatedTriggers(t *testing.T) {
	b := newForceReconnectTestBroker(t)
	client := newForceReconnectTestClient(t, b, "force-reconnect-retrying")
	w := newForceReconnectTestWatchdog(client, "force-reconnect-retrying")

	before, _ := b.counts()
	b.goDown()
	n := waitForConnects(t, b, before, "after broker went down")

	// Premise: while paho retries, IsConnected() stays true, so the watchdog
	// classifies the silent source as Stalled and fires; IsConnectionOpen()
	// is what tells the two cases apart.
	if client.IsConnectionOpen() {
		t.Fatal("IsConnectionOpen()==true while the broker is down")
	}
	if kind := w.classify(); kind != LivenessStalled {
		t.Fatalf("watchdog classified a retrying client as %v, want LivenessStalled", kind)
	}

	// Fire right after each observed attempt, i.e. while paho sleeps
	// forceReconnectTestRetryInterval before the next one. A Disconnect here
	// blocks in "disconnecting" until that sleep ends.
	for i := 1; i <= 3; i++ {
		w.trigger(t)
		n = waitForConnects(t, b, n, fmt.Sprintf("after force-reconnect #%d", i))
	}
	waitForConnects(t, b, n, "second attempt after the last force-reconnect")

	// Broker back: paho's own loop must reconnect, with no further trigger.
	_, acceptedBefore := b.counts()
	b.goUp()
	if !pollUntil(forceReconnectTestDeadline, client.IsConnectionOpen) {
		c, a := b.counts()
		t.Fatalf("client did not reconnect within %s after the broker came back (CONNECTs %d, sessions %d)", forceReconnectTestDeadline, c, a)
	}
	if _, a := b.counts(); a <= acceptedBefore {
		t.Fatalf("IsConnectionOpen()==true but the broker accepted no new session (%d)", a)
	}
}

// The #1335 case the force-reconnect exists for: paho thinks the socket is
// open, the broker is silent. The old socket must be dropped and replaced.
func TestForceReconnect_RealPaho_ReplacesHalfOpenSocket(t *testing.T) {
	b := newForceReconnectTestBroker(t)
	client := newForceReconnectTestClient(t, b, "force-reconnect-half-open")
	w := newForceReconnectTestWatchdog(client, "force-reconnect-half-open")

	b.silenceSessions()
	if !client.IsConnectionOpen() {
		t.Fatal("test setup: half-open session must still look open to paho")
	}
	if kind := w.classify(); kind != LivenessStalled {
		t.Fatalf("watchdog classified a half-open client as %v, want LivenessStalled", kind)
	}

	w.trigger(t)

	if !pollUntil(forceReconnectTestDeadline, func() bool { _, a := b.counts(); return a >= 2 && client.IsConnectionOpen() }) {
		c, a := b.counts()
		t.Fatalf("no replacement session within %s (CONNECTs %d, sessions %d, open %v)", forceReconnectTestDeadline, c, a, client.IsConnectionOpen())
	}
	select {
	case <-b.session(0).closed:
	case <-time.After(forceReconnectTestDeadline):
		t.Fatal("client never dropped the half-open socket")
	}
}

// The #1749 escalation case: paho has stopped retrying (status disconnected).
// The force-reconnect must start a fresh connect — and must not block on it
// while the broker is still down.
func TestForceReconnect_RealPaho_RestartsClientPahoGaveUpOn(t *testing.T) {
	b := newForceReconnectTestBroker(t)
	client := newForceReconnectTestClient(t, b, "force-reconnect-gave-up")
	w := newForceReconnectTestWatchdog(client, "force-reconnect-gave-up")

	client.Disconnect(250)
	b.goDown()
	if !pollUntil(forceReconnectTestDeadline, func() bool { return !client.IsConnected() }) {
		t.Fatal("test setup: client did not settle to disconnected")
	}
	if kind := w.classify(); kind != LivenessDisconnected {
		t.Fatalf("watchdog classified a stopped client as %v, want LivenessDisconnected", kind)
	}

	before, _ := b.counts()
	w.trigger(t)
	n := waitForConnects(t, b, before, "after force-reconnect of a stopped client")
	waitForConnects(t, b, n, "ConnectRetry after force-reconnect of a stopped client")

	b.goUp()
	if !pollUntil(forceReconnectTestDeadline, client.IsConnectionOpen) {
		t.Fatalf("client did not connect within %s after the broker came back", forceReconnectTestDeadline)
	}
}
