package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Issue #117: browsers cannot see WebSocket ping frames, so the server also
// writes a small application-level heartbeat on the existing ping tick, from
// the single writer goroutine. public/app.js replaces a socket that has been
// silent for longer than its threshold.

func dialHeartbeatHub(t *testing.T, interval time.Duration) (*Hub, *websocket.Conn, *atomic.Int32) {
	t.Helper()
	hub := NewHub()
	hub.pingInterval = interval
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	var pings atomic.Int32
	conn.SetPingHandler(func(data string) error {
		pings.Add(1)
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	return hub, conn, &pings
}

func TestHubDefaultPingInterval_117(t *testing.T) {
	// public/app.js WS_STALE_MS is derived from this interval (two missed
	// heartbeats plus slack); TestClientStaleThresholdCoversTwoHeartbeats_117
	// checks the pair.
	if got := NewHub().pingInterval; got != 30*time.Second {
		t.Fatalf("default ping interval = %v, want 30s", got)
	}
}

func TestWritePumpSendsHeartbeatOnEveryPingTick_117(t *testing.T) {
	_, conn, pings := dialHeartbeatHub(t, 25*time.Millisecond)
	const want = 4
	got := 0
	deadline := time.Now().Add(5 * time.Second)
	for got < want {
		conn.SetReadDeadline(deadline)
		typ, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("after %d heartbeats: read: %v (no application heartbeat reaches the page)", got, err)
		}
		if typ != websocket.TextMessage || !bytes.Equal(msg, wsHeartbeat) {
			t.Fatalf("unexpected frame %d %q", typ, msg)
		}
		got++
	}
	// The protocol-level ping is still sent on the same tick; it is written
	// before the heartbeat, so by now at least as many pings have arrived.
	if p := int(pings.Load()); p < want {
		t.Fatalf("%d pings for %d heartbeats: the protocol ping must stay", p, want)
	}
}

// The heartbeat bytes are what app.js matches exactly, and a plain JSON
// object with a type other than "packet" for anything that parses it.
func TestHeartbeatFrameShape_117(t *testing.T) {
	var v struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(wsHeartbeat, &v); err != nil || v.Type != "heartbeat" {
		t.Fatalf("heartbeat %q: type=%q err=%v", wsHeartbeat, v.Type, err)
	}
	if len(wsHeartbeat) > 32 {
		t.Fatalf("heartbeat is %d bytes; keep it small", len(wsHeartbeat))
	}
}

// Broadcasts still arrive byte-for-byte, interleaved with heartbeats.
func TestBroadcastsUnchangedAlongsideHeartbeats_117(t *testing.T) {
	hub, conn, _ := dialHeartbeatHub(t, 20*time.Millisecond)
	for hub.ClientCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	type packetMsg struct {
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}
	want, _ := json.Marshal(packetMsg{Type: "packet", Data: map[string]string{"hash": "abc"}})
	hub.Broadcast(packetMsg{Type: "packet", Data: map[string]string{"hash": "abc"}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if bytes.Equal(msg, wsHeartbeat) {
			continue
		}
		if !bytes.Equal(msg, want) {
			t.Fatalf("broadcast changed: got %q, want %q", msg, want)
		}
		return
	}
}

// Dead-client detection is unchanged: the read deadline is refreshed only by
// pongs (a client never sends heartbeats), and all writes, the heartbeat
// included, stay on the one writer goroutine per client.
func TestHeartbeatDoesNotChangeDeadClientHandling_117(t *testing.T) {
	src, err := os.ReadFile("websocket.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if strings.Count(s, "SetReadDeadline(time.Now().Add(60 * time.Second))") != 2 {
		t.Fatal("readPump's 60s read deadline / pong refresh changed")
	}
	if strings.Count(s, "go client.writePump(") != 1 {
		t.Fatal("there must be exactly one writer goroutine per client")
	}
}

// app.js must treat two missed heartbeats (plus slack) as stale, never less,
// and match the server's heartbeat bytes exactly.
func TestClientStaleThresholdCoversTwoHeartbeats_117(t *testing.T) {
	app, err := os.ReadFile("../../public/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const WS_STALE_MS = (\d+);`).FindSubmatch(app)
	if m == nil {
		t.Fatal("WS_STALE_MS not found in public/app.js")
	}
	staleMs, _ := strconv.Atoi(string(m[1]))
	if stale := time.Duration(staleMs) * time.Millisecond; stale <= 2*NewHub().pingInterval {
		t.Fatalf("WS_STALE_MS %v does not tolerate one lost heartbeat at %v", stale, NewHub().pingInterval)
	}
	h := regexp.MustCompile("const WS_HEARTBEAT = '([^']*)';").FindSubmatch(app)
	if h == nil || !bytes.Equal(h[1], wsHeartbeat) {
		t.Fatalf("app.js WS_HEARTBEAT %q != server %q", h, wsHeartbeat)
	}
}
