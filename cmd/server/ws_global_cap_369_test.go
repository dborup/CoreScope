package main

// Issue #369: a global ceiling on concurrent /ws clients.
//
// The per-IP caps from #1794 are skipped whenever clients cannot be told
// apart — which is exactly the shipped Docker topology, where the bundled
// Caddy proxies to localhost and every visitor arrives as 127.0.0.1. These
// tests therefore drive real handshakes through an httptest server (so the
// peer really is loopback) rather than calling allow() directly: a limit that
// only holds in a unit test is not the limit this issue asks for.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// capHub builds a hub whose only configured limit is the global ceiling,
// unless the caller asks for per-IP limits too, and serves it over a real
// listener. Every dial to it arrives from 127.0.0.1, i.e. the
// indistinguishable-client case.
func capHub(t *testing.T, maxClients, maxPerIP, perMin int, trusted []string) (*Hub, *httptest.Server) {
	t.Helper()
	hub := NewHub()
	hub.pingInterval = time.Hour // no ping traffic during these tests
	hub.ConfigureLimits(maxClients, maxPerIP, perMin, trusted, nil)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(srv.Close)
	return hub, srv
}

// dialCap performs a real handshake. headers may carry Origin or
// X-Forwarded-For. The returned response is the server's, so a refusal can be
// inspected for its status and Retry-After.
func dialCap(t *testing.T, srv *httptest.Server, headers http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:], headers)
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, resp, err
}

func waitClients(t *testing.T, hub *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hub has %d clients, want %d", hub.ClientCount(), want)
}

func waitReserved(t *testing.T, hub *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.limits.reserved() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("limiter holds %d reserved slots, want %d", hub.limits.reserved(), want)
}

// Acceptance 1: only the configured number of clients can connect at once,
// including many connections through one proxy address.
func TestWSGlobalCap_CeilingHoldsBehindOneProxyAddress_369(t *testing.T) {
	// Per-IP caps are generous AND unenforceable here (loopback peer, no
	// trustedProxies), so anything that refuses the third client can only be
	// the global ceiling.
	hub, srv := capHub(t, 2, 50, 50, nil)
	for i := 1; i <= 2; i++ {
		if _, _, err := dialCap(t, srv, nil); err != nil {
			t.Fatalf("client %d of 2 was refused: %v", i, err)
		}
	}
	waitClients(t, hub, 2)
	_, resp, err := dialCap(t, srv, nil)
	if err == nil {
		t.Fatal("the third client connected: the global ceiling did not hold")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third client status = %v, want %d", resp, http.StatusServiceUnavailable)
	}
	if hub.ClientCount() != 2 {
		t.Errorf("hub holds %d clients, want 2", hub.ClientCount())
	}
	if _, _, c := hub.limits.counts(); c != 0 {
		t.Errorf("rejectedConnCap = %d: the refusal must be attributed to the ceiling, not the per-IP cap", c)
	}
	if g := hub.limits.rejectedGlobal.Load(); g != 1 {
		t.Errorf("rejectedGlobalCap = %d, want 1", g)
	}
}

// Distinct clients behind ONE trusted proxy: each is well inside its own
// per-IP budget, and the ceiling is still what bounds the total.
func TestWSGlobalCap_DistinctClientsBehindTrustedProxy_369(t *testing.T) {
	hub, srv := capHub(t, 2, 5, 30, []string{"127.0.0.1", "::1"})
	for i := 1; i <= 2; i++ {
		h := http.Header{}
		h.Set("X-Forwarded-For", "203.0.113."+strconv.Itoa(i))
		if _, _, err := dialCap(t, srv, h); err != nil {
			t.Fatalf("client %d of 2 was refused: %v", i, err)
		}
	}
	waitClients(t, hub, 2)
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.3") // a third, entirely fresh client
	_, resp, err := dialCap(t, srv, h)
	if err == nil {
		t.Fatal("a fresh client address got in past a full server")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %v, want %d", resp, http.StatusServiceUnavailable)
	}
}

// Acceptance 2: excess clients are refused BEFORE the upgrade, with a retry
// response rather than a protocol switch.
func TestWSGlobalCap_RetryResponseBeforeUpgrade_369(t *testing.T) {
	hub, srv := capHub(t, 1, 0, 0, nil)
	if _, _, err := dialCap(t, srv, nil); err != nil {
		t.Fatalf("first client refused: %v", err)
	}
	waitClients(t, hub, 1)
	_, resp, err := dialCap(t, srv, nil)
	if err == nil {
		t.Fatal("the second client was upgraded")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (temporary, retryable)", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := resp.Header.Get("Retry-After"); got != wsGlobalCapRetryAfter {
		t.Errorf("Retry-After = %q, want %q so a non-browser client knows when to come back", got, wsGlobalCapRetryAfter)
	}
	if got := resp.Header.Get("Upgrade"); got != "" {
		t.Errorf("Upgrade header = %q: the refusal must not look like a handshake", got)
	}
}

// Acceptance 3a: a slot reserved for an upgrade that then fails comes back.
// Origin rejection is the real-world way that happens, and it must keep its
// 403 — the ceiling must not change that verdict.
func TestWSGlobalCap_SlotReleasedOnFailedUpgrade_369(t *testing.T) {
	hub, srv := capHub(t, 1, 0, 0, nil)
	h := http.Header{}
	h.Set("Origin", "https://evil.example.com")
	_, resp, err := dialCap(t, srv, h)
	if err == nil {
		t.Fatal("a foreign origin was upgraded")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403 (origin rejection keeps precedence)", resp.StatusCode)
	}
	waitReserved(t, hub, 0)
	if _, _, err := dialCap(t, srv, nil); err != nil {
		t.Fatalf("the only slot stayed leaked after the failed upgrade: %v", err)
	}
}

// Acceptance 3b: a disconnect returns the slot.
func TestWSGlobalCap_SlotReleasedOnDisconnect_369(t *testing.T) {
	hub, srv := capHub(t, 1, 0, 0, nil)
	conn, _, err := dialCap(t, srv, nil)
	if err != nil {
		t.Fatalf("first client refused: %v", err)
	}
	waitClients(t, hub, 1)
	conn.Close()
	waitClients(t, hub, 0)
	waitReserved(t, hub, 0)
	if _, _, err := dialCap(t, srv, nil); err != nil {
		t.Fatalf("the slot was not returned on disconnect: %v", err)
	}
}

// Acceptance 3c: hub shutdown returns every slot. Without this a server that
// closed the hub (graceful shutdown, or a test harness reusing the limiter)
// keeps the reservations forever.
func TestWSGlobalCap_SlotsReleasedOnHubShutdown_369(t *testing.T) {
	hub, srv := capHub(t, 2, 0, 0, nil)
	for i := 1; i <= 2; i++ {
		if _, _, err := dialCap(t, srv, nil); err != nil {
			t.Fatalf("client %d refused: %v", i, err)
		}
	}
	waitClients(t, hub, 2)
	hub.Close()
	if n := hub.limits.reserved(); n != 0 {
		t.Fatalf("after hub shutdown %d slots are still reserved, want 0", n)
	}
	for i := 1; i <= 2; i++ {
		if _, _, err := dialCap(t, srv, nil); err != nil {
			t.Fatalf("post-shutdown client %d refused: the ceiling is permanently full: %v", i, err)
		}
	}
}

// Acceptance: slots are returned exactly once. A double release would credit
// capacity the server never regained.
func TestWSGlobalCap_ReleaseIsExactlyOnce_369(t *testing.T) {
	l := limiter(0, 0, nil, nil)
	l.maxClients = 1
	_, _, _, release := l.allow(req("127.0.0.1:1", ""))
	release()
	release()
	if n := l.reserved(); n != 0 {
		t.Fatalf("reserved = %d after a double release, want 0", n)
	}
	if ok, _, _, _ := l.allow(req("127.0.0.1:1", "")); !ok {
		t.Fatal("the released slot must be reusable")
	}
	if ok, _, _, _ := l.allow(req("127.0.0.1:1", "")); ok {
		t.Error("ceiling 1 must still hold: the double release must not have credited two slots")
	}
}

// Acceptance: race coverage for concurrent admission. Run under -race.
func TestWSGlobalCap_ConcurrentAdmission_369(t *testing.T) {
	const cap369, racers = 5, 40
	hub, srv := capHub(t, cap369, 0, 0, nil)
	var (
		mu       sync.Mutex
		accepted int
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:], nil)
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			t.Cleanup(func() { conn.Close() })
		}()
	}
	close(start)
	wg.Wait()
	mu.Lock()
	got := accepted
	mu.Unlock()
	if got != cap369 {
		t.Fatalf("%d of %d racers were admitted, want exactly %d", got, racers, cap369)
	}
	waitClients(t, hub, cap369)
	if n := hub.limits.reserved(); n != cap369 {
		t.Errorf("reserved = %d, want %d", n, cap369)
	}
}

// Precedence: the deny list is an explicit operator instruction and is still
// evaluated first, so a denied address gets 403 and never books a slot.
func TestWSGlobalCap_DenyKeepsPrecedence_369(t *testing.T) {
	l := limiter(0, 0, nil, []string{"127.0.0.1"})
	l.maxClients = 5
	ok, reason, _, _ := l.allow(req("127.0.0.1:1", ""))
	if ok || reason != wsRejectDeny {
		t.Fatalf("ok=%v reason=%q, want the deny list to win", ok, reason)
	}
	if n := l.reserved(); n != 0 {
		t.Errorf("reserved = %d, want 0: a denied request must not book a slot", n)
	}
}

// Precedence: a per-IP refusal keeps its own reason and status, so an
// operator reading the counters can still tell the two apart.
func TestWSGlobalCap_PerIPReasonSurvives_369(t *testing.T) {
	l := limiter(1, 0, []string{"10.0.0.0/8"}, nil)
	l.maxClients = 10
	l.allow(req("10.0.0.2:1", "203.0.113.5"))
	ok, reason, _, _ := l.allow(req("10.0.0.2:1", "203.0.113.5"))
	if ok || reason != wsRejectConnCap {
		t.Fatalf("ok=%v reason=%q, want %q", ok, reason, wsRejectConnCap)
	}
}

// A refusal by the ceiling must not charge the per-IP upgrade budget, the
// same invariant #1794 holds for its own refusals.
func TestWSGlobalCap_RefusalDoesNotChargeRateBudget_369(t *testing.T) {
	l := limiter(0, 5, []string{"10.0.0.0/8"}, nil)
	l.maxClients = 1
	l.allow(req("10.0.0.2:1", "203.0.113.6")) // takes the only slot
	for i := 0; i < 3; i++ {
		if ok, reason, _, _ := l.allow(req("10.0.0.2:1", "203.0.113.6")); ok || reason != wsRejectGlobalCap {
			t.Fatalf("attempt %d: ok=%v reason=%q, want %q", i, ok, reason, wsRejectGlobalCap)
		}
	}
	l.mu.Lock()
	used := len(l.state["203.0.113.6"].upgrades)
	l.mu.Unlock()
	if used != 1 {
		t.Errorf("upgrades charged = %d, want 1: a refusal must not spend budget", used)
	}
}

// A nil limiter (a Hub built without ConfigureLimits, as several tests do)
// must still allow everything.
func TestWSGlobalCap_NilLimiterUnbounded_369(t *testing.T) {
	var l *wsLimiter
	if n := l.reserved(); n != 0 {
		t.Errorf("nil limiter reserved = %d, want 0", n)
	}
	if ok, _, _, release := l.allow(req("203.0.113.9:1", "")); !ok {
		t.Error("a nil limiter must allow the upgrade")
	} else {
		release()
	}
}

func TestWSGlobalCap_ConfigDefault_369(t *testing.T) {
	var c Config
	if got := c.WSMaxClients(); got != wsDefaultMaxClients {
		t.Errorf("default maxClients = %d, want %d", got, wsDefaultMaxClients)
	}
	if wsDefaultMaxClients <= 0 {
		t.Fatalf("the shipped default must be a real ceiling, got %d", wsDefaultMaxClients)
	}
	zero := 0
	c.WebSocket = &WebSocketConfig{MaxClients: &zero}
	if got := c.WSMaxClients(); got != 0 {
		t.Errorf("explicit 0 = %d, want 0: an operator must be able to turn the ceiling off", got)
	}
	n := 64
	c.WebSocket = &WebSocketConfig{MaxClients: &n}
	if got := c.WSMaxClients(); got != 64 {
		t.Errorf("configured ceiling = %d, want 64", got)
	}
	neg := -1
	c.WebSocket = &WebSocketConfig{MaxClients: &neg}
	if got := c.WSMaxClients(); got != wsDefaultMaxClients {
		t.Errorf("negative ceiling = %d, want the default %d: a typo must not remove the ceiling", got, wsDefaultMaxClients)
	}
}

// The counter reaches /api/stats, where an operator can see the server is
// turning clients away.
func TestWSGlobalCap_StatsReportsRejections_369(t *testing.T) {
	hub, srv := capHub(t, 1, 0, 0, nil)
	if _, _, err := dialCap(t, srv, nil); err != nil {
		t.Fatalf("first client refused: %v", err)
	}
	waitClients(t, hub, 1)
	if _, _, err := dialCap(t, srv, nil); err == nil {
		t.Fatal("the second client was admitted")
	}
	s := &Server{hub: hub, perfStats: &PerfStats{}, startedAt: time.Now()}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/stats: %v", err)
	}
	if got.WebSocket.RejectedGlobalCap != 1 {
		t.Errorf("websocket.rejectedGlobalCap = %d, want 1", got.WebSocket.RejectedGlobalCap)
	}
	if got.WebSocket.Clients != 1 {
		t.Errorf("websocket.clients = %d, want 1", got.WebSocket.Clients)
	}
}

// The shipped config file must document the same default the code uses, or
// an operator reading config.example.json is told a number that is not true.
func TestWSGlobalCap_ExampleConfigMatchesDefault_369(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	// Only the webSocket block: the file's _comment_* keys sit inside typed
	// maps elsewhere, so the whole file does not unmarshal into Config.
	var file struct {
		WebSocket *WebSocketConfig `json:"webSocket"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("config.example.json webSocket block does not parse: %v", err)
	}
	c := Config{WebSocket: file.WebSocket}
	if c.WebSocket == nil || c.WebSocket.MaxClients == nil {
		t.Fatal("config.example.json does not document webSocket.maxClients")
	}
	if got := c.WSMaxClients(); got != wsDefaultMaxClients {
		t.Errorf("config.example.json ships maxClients = %d, code default is %d", got, wsDefaultMaxClients)
	}
}

// The ceiling is only real if main actually passes the configured value in.
// Every other test here builds its own hub, so this is the one check that the
// production wiring is not hardcoded to 0 (= unbounded).
func TestWSGlobalCap_MainWiresTheCeiling_369(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), "hub.ConfigureLimits(cfg.WSMaxClients()") {
		t.Error("main.go must pass cfg.WSMaxClients() as ConfigureLimits' ceiling; " +
			"any literal there silently un-bounds every deployment")
	}
}
