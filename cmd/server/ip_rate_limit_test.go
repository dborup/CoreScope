package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #681 M3 / #682 review F3 — the per-IP request limit shared by
// /api/terrain/profile and the DHM tile proxy. Both front the operator's
// Dataforsyningen token, so both are budgeted per client.

type limiterClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *limiterClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *limiterClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func limiterReq(remote string, xff string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestIPRateLimiter_BudgetRefillAndRetryAfter(t *testing.T) {
	clock := &limiterClock{t: time.Unix(1_800_000_000, 0)}
	logs := &dhmLogSink{}
	l := newIPRateLimiter("test", 6, nil, logs.logf)
	l.now = clock.now
	for i := 0; i < 6; i++ {
		if ok, _ := l.allow(limiterReq("192.0.2.7:1", "")); !ok {
			t.Fatalf("request %d of 6 refused", i+1)
		}
	}
	ok, retry := l.allow(limiterReq("192.0.2.7:2", ""))
	if ok || retry != 10*time.Second {
		t.Fatalf("7th: ok=%v retry=%v, want refused, 10s (one token per 10 s)", ok, retry)
	}
	// Another client has its own budget.
	if ok, _ := l.allow(limiterReq("198.51.100.1:1", "")); !ok {
		t.Fatal("second client refused")
	}
	// One token back after 10 s, not before.
	clock.add(9 * time.Second)
	if ok, retry := l.allow(limiterReq("192.0.2.7:1", "")); ok || retry != time.Second {
		t.Fatalf("after 9 s: ok=%v retry=%v", ok, retry)
	}
	clock.add(time.Second)
	if ok, _ := l.allow(limiterReq("192.0.2.7:1", "")); !ok {
		t.Fatal("token not refilled after 10 s")
	}
	if l.rejected.Load() != 2 {
		t.Fatalf("rejected %d", l.rejected.Load())
	}
	// Refills never exceed the budget: after an hour, still 6.
	clock.add(time.Hour)
	for i := 0; i < 6; i++ {
		l.allow(limiterReq("192.0.2.7:1", ""))
	}
	if ok, _ := l.allow(limiterReq("192.0.2.7:1", "")); ok {
		t.Fatal("budget grew past perMin while idle")
	}
}

func TestIPRateLimiter_OffAndNil(t *testing.T) {
	if newIPRateLimiter("test", 0, nil, nil) != nil {
		t.Fatal("perMin 0 built a limiter")
	}
	var l *ipRateLimiter
	if ok, _ := l.allow(limiterReq("192.0.2.7:1", "")); !ok {
		t.Fatal("nil limiter refused")
	}
}

// Behind an unconfigured local proxy every visitor shares one address, so
// a per-IP limit would throttle everyone together; it is skipped with one
// warning, exactly like the /ws limits (#1794).
func TestIPRateLimiter_IndistinctClientsAreNotLimited(t *testing.T) {
	logs := &dhmLogSink{}
	l := newIPRateLimiter("map.dhm.profile.rateLimitPerMin", 1, nil, logs.logf)
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow(limiterReq("127.0.0.1:"+strconv.Itoa(1000+i), "203.0.113.9")); !ok {
			t.Fatal("loopback peer limited without trustedProxies")
		}
	}
	if n := strings.Count(logs.String(), "trustedProxies"); n != 1 {
		t.Fatalf("warning logged %d times: %q", n, logs.String())
	}
	if !strings.Contains(logs.String(), "map.dhm.profile.rateLimitPerMin") {
		t.Fatal("warning does not name the limit")
	}
}

// With the proxy trusted, X-Forwarded-For names the client: the same
// resolution the /ws limiter uses (clientAddr).
func TestIPRateLimiter_TrustedProxyXFF(t *testing.T) {
	_, proxyNet, _ := net.ParseCIDR("127.0.0.1/32")
	l := newIPRateLimiter("test", 1, []*net.IPNet{proxyNet}, nil)
	if ok, _ := l.allow(limiterReq("127.0.0.1:1", "203.0.113.9")); !ok {
		t.Fatal("first request refused")
	}
	// #691: the walk is from the right past trusted hops, so the same
	// client behind a second trusted hop is still the same client ...
	if ok, _ := l.allow(limiterReq("127.0.0.1:2", "203.0.113.9, 127.0.0.1")); ok {
		t.Fatal("same forwarded client not limited")
	}
	// ... while an untrusted right-most hop is the client: everything left
	// of it may have been written by that client.
	if ok, _ := l.allow(limiterReq("127.0.0.1:2", "203.0.113.9, 10.0.0.1")); !ok {
		t.Fatal("untrusted right-most hop not treated as its own client")
	}
	if ok, _ := l.allow(limiterReq("127.0.0.1:3", "203.0.113.10")); !ok {
		t.Fatal("other forwarded client limited")
	}
	// An untrusted peer cannot pick its address with XFF.
	if ok, _ := l.allow(limiterReq("198.51.100.4:1", "203.0.113.77")); !ok {
		t.Fatal("first direct request refused")
	}
	if ok, _ := l.allow(limiterReq("198.51.100.4:1", "203.0.113.78")); ok {
		t.Fatal("forged XFF from an untrusted peer bought a fresh budget")
	}
}

// IPv6 clients are counted per /64, the smallest block a subscriber gets,
// so rotating addresses inside it buys no budget.
func TestIPRateLimiter_IPv6Per64(t *testing.T) {
	l := newIPRateLimiter("test", 1, nil, nil)
	if ok, _ := l.allow(limiterReq("[2001:db8:1:2::1]:1", "")); !ok {
		t.Fatal("first refused")
	}
	if ok, _ := l.allow(limiterReq("[2001:db8:1:2:ffff::9]:1", "")); ok {
		t.Fatal("same /64 not limited")
	}
	if ok, _ := l.allow(limiterReq("[2001:db8:1:3::1]:1", "")); !ok {
		t.Fatal("other /64 limited")
	}
}

// The client table is bounded: idle entries are dropped, and when it is
// full of active clients a new one is refused rather than given a fresh
// budget by evicting someone.
func TestIPRateLimiter_TableIsBounded(t *testing.T) {
	clock := &limiterClock{t: time.Unix(1_800_000_000, 0)}
	l := newIPRateLimiter("test", 60, nil, nil)
	l.now = clock.now
	l.maxClients = 3
	for i := 1; i <= 3; i++ {
		if ok, _ := l.allow(limiterReq("192.0.2."+strconv.Itoa(i)+":1", "")); !ok {
			t.Fatalf("client %d refused", i)
		}
	}
	if ok, retry := l.allow(limiterReq("192.0.2.4:1", "")); ok || retry <= 0 {
		t.Fatal("table over its bound")
	}
	if len(l.clients) != 3 {
		t.Fatalf("table size %d", len(l.clients))
	}
	// A minute later every budget is full again, so those entries carry
	// no state and make room.
	clock.add(time.Minute)
	if ok, _ := l.allow(limiterReq("192.0.2.4:1", "")); !ok {
		t.Fatal("idle entries not collected")
	}
	if len(l.clients) != 1 {
		t.Fatalf("table size after collection %d", len(l.clients))
	}
}

// N1 (#690 review): TestIPRateLimiter_TableIsBounded sets maxClients by
// hand, pinning the mechanism but not the shipped value — raising
// ipLimiterMaxClients passed the whole suite. This pins the constant and
// that newIPRateLimiter actually wires it in.
func TestIPRateLimiter_DefaultMaxClientsIsTheConstant(t *testing.T) {
	if ipLimiterMaxClients != 4096 {
		t.Fatalf("ipLimiterMaxClients = %d, want 4096", ipLimiterMaxClients)
	}
	l := newIPRateLimiter("test", 60, nil, nil)
	if l.maxClients != ipLimiterMaxClients {
		t.Fatalf("newIPRateLimiter maxClients = %d, want ipLimiterMaxClients (%d)", l.maxClients, ipLimiterMaxClients)
	}
}

// ── Wiring: both endpoints answer 429 with Retry-After ─────────────────────

func TestTerrainProfile_RateLimit429(t *testing.T) {
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{RateLimitPerMin: intp(2)}, nil)
	paths := []string{
		pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 3000, 0), "model=dtm"),
		pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 0, 3000), "model=dtm"),
		pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, -3000, 0), "model=dtm"),
	}
	decodeProfile(t, env.get(paths[0]))
	decodeProfile(t, env.get(paths[1]))
	calls := src.calls.Load()
	w := env.get(paths[2])
	wantError(t, w, 429, "rate limit exceeded")
	// 2 per minute: the next one in 30 s (the test runs well inside a second).
	if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 29 || ra > 30 {
		t.Fatalf("Retry-After %q, want 30", w.Header().Get("Retry-After"))
	}
	if src.calls.Load() != calls {
		t.Fatal("refused request went upstream")
	}
	// Cache hits cost no upstream quota and are not charged.
	for i := 0; i < 5; i++ {
		decodeProfile(t, env.get(paths[0]))
	}
	// Invalid and outside-coverage requests never reach the limiter either.
	wantError(t, env.get("a=1"), 400, "invalid terrain request")
	decodeProfile(t, env.get(pathQuery("53.55000,9.99000", "53.60000,10.10000")))
	// Another client has its own budget.
	w = env.get(paths[2], func(r *http.Request) { r.RemoteAddr = "198.51.100.20:4000" })
	decodeProfile(t, w)
}

func TestDHMTile_RateLimit429(t *testing.T) {
	up := newFakeUpstream(t, nil)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.rateLimitPerMin = 3 })
	z, x, y := dkTile()
	for i := 0; i < 3; i++ {
		if w := doTile(p, leafletQuery(z, x+i, y)); w.Code != 200 {
			t.Fatalf("tile %d: %d", i, w.Code)
		}
	}
	hits := up.hits.Load()
	w := doTile(p, leafletQuery(z, x+3, y))
	wantError(t, w, 429, "rate limit exceeded")
	// 3 per minute: the next one in 20 s.
	if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 19 || ra > 20 {
		t.Fatalf("Retry-After %q, want 20", w.Header().Get("Retry-After"))
	}
	if up.hits.Load() != hits {
		t.Fatal("refused tile went upstream")
	}
	// Cached tiles and tiles outside Denmark cost nothing upstream: served.
	if w := doTile(p, leafletQuery(z, x, y)); w.Code != 200 || w.Header().Get("X-CoreScope-Tile") != "hit" {
		t.Fatalf("cached tile: %d %q", w.Code, w.Header().Get("X-CoreScope-Tile"))
	}
	ox, oy := lonLatTile(10, -3.7, 40.4) // Madrid
	if w := doTile(p, leafletQuery(10, ox, oy)); w.Code != 200 || w.Header().Get("X-CoreScope-Tile") != "outside" {
		t.Fatalf("outside tile: %d", w.Code)
	}
	// The browser's own query shape is still validated first.
	q := leafletQuery(z, x+4, y)
	q.Set("layers", "x")
	if w := doTile(p, q); w.Code != 400 {
		t.Fatalf("bad tile: %d", w.Code)
	}
}

// The shipped tile default is far above a heavy interactive session (see
// dhmTileRateLimitDefault) and is off only when set to 0.
func TestDHMTile_RateLimitConfig(t *testing.T) {
	inRange := &dhmLogSink{}
	if got := dhmTileRateLimit(dhm.Config{}, inRange.logf); got != dhmTileRateLimitDefault {
		t.Fatalf("default %d", got)
	}
	if got := dhmTileRateLimit(dhm.Config{TileRateLimitPerMin: intp(0)}, inRange.logf); got != 0 {
		t.Fatalf("explicit 0 → %d", got)
	}
	if got := dhmTileRateLimit(dhm.Config{TileRateLimitPerMin: intp(50)}, inRange.logf); got != 50 {
		t.Fatalf("50 → %d", got)
	}
	if got := dhmTileRateLimit(dhm.Config{TileRateLimitPerMin: intp(dhmTileRateLimitMax)}, inRange.logf); got != dhmTileRateLimitMax {
		t.Fatalf("at the bound → %d, want %d", got, dhmTileRateLimitMax)
	}
	if inRange.String() != "" {
		t.Fatalf("in-range values warned: %q", inRange.String())
	}
	// N2 (#690 review): out-of-range, like profile.rateLimitPerMin
	// (integer(..., 0, 6000)), warns and falls back to the default instead
	// of being accepted verbatim.
	for _, v := range []int{-4, dhmTileRateLimitMax + 1} {
		logs := &dhmLogSink{}
		if got := dhmTileRateLimit(dhm.Config{TileRateLimitPerMin: intp(v)}, logs.logf); got != dhmTileRateLimitDefault {
			t.Fatalf("%d → %d, want the default %d", v, got, dhmTileRateLimitDefault)
		}
		if !strings.Contains(logs.String(), "map.dhm.tileRateLimitPerMin") {
			t.Fatalf("%d: no warning for an out-of-range tileRateLimitPerMin: %q", v, logs.String())
		}
	}
	if defaultDHMProxyOptions().rateLimitPerMin != 0 {
		t.Fatal("the bare proxy options must not limit; the config path sets it")
	}
}

// Both limiters built from config identify clients through
// webSocket.trustedProxies, like the /ws limits.
func TestDHMRateLimits_TrustedProxiesFromConfig(t *testing.T) {
	cfg := &Config{
		DHM: dhm.Config{Enabled: true, Token: "t", TileRateLimitPerMin: intp(1),
			Profile: dhm.ProfileConfig{Enabled: true, RateLimitPerMin: intp(1)}},
		WebSocket: &WebSocketConfig{TrustedProxies: []string{"127.0.0.1"}},
	}
	proxy := newDHMProxyFromConfig(cfg)
	profile := newTerrainProfilerFromConfig(cfg, proxy, nil)
	for name, l := range map[string]*ipRateLimiter{"tile": proxy.limiter, "profile": profile.limiter} {
		if ok, _ := l.allow(limiterReq("127.0.0.1:1", "203.0.113.5")); !ok {
			t.Fatalf("%s: first request refused", name)
		}
		if ok, _ := l.allow(limiterReq("127.0.0.1:2", "203.0.113.5")); ok {
			t.Fatalf("%s: forwarded client behind the trusted proxy not limited", name)
		}
	}
}
