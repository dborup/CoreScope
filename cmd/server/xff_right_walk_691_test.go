package main

// #691 — the per-IP client address (clientAddr, shared by /ws, the DHM
// profile and the DHM tile limits) must walk X-Forwarded-For from the RIGHT,
// skipping hops that are themselves trusted proxies. The left-most entry is
// whatever the client sent whenever the proxy appends to the header (nginx
// $proxy_add_x_forwarded_for), so believing it lets one client mint a fresh
// budget per request and fill the client table with forged addresses.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func trustedNets691(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// xffReq691 builds a request from peer carrying one X-Forwarded-For header
// line per element of lines (none when lines is empty).
func xffReq691(peer string, lines ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = peer
	for _, l := range lines {
		r.Header.Add("X-Forwarded-For", l)
	}
	return r
}

func wantClient691(t *testing.T, r *http.Request, trusted []*net.IPNet, want string) {
	t.Helper()
	ip, distinct := clientAddr(r, trusted)
	if !distinct {
		t.Fatalf("XFF %q: distinct=false, want true behind a trusted proxy", r.Header.Values("X-Forwarded-For"))
	}
	if ip == nil || !ip.Equal(net.ParseIP(want)) {
		t.Fatalf("XFF %q: client = %v, want %s", r.Header.Values("X-Forwarded-For"), ip, want)
	}
}

// Appending proxy: the client prefixes a fresh forged address on every
// request. The proxy-appended entry on the right is the client; the forged
// prefix must buy nothing.
func TestXFF691_AppendingProxyIgnoresForgedLeftmost(t *testing.T) {
	trusted := trustedNets691(t, "127.0.0.1/32")
	wantClient691(t, xffReq691("127.0.0.1:1", "198.51.100.66, 203.0.113.9"), trusted, "203.0.113.9")

	l := newIPRateLimiter("test", 1, trusted, nil)
	if ok, _ := l.allow(xffReq691("127.0.0.1:1", "198.51.100.1, 203.0.113.9")); !ok {
		t.Fatal("first request refused")
	}
	allowed := 0
	for i := 2; i <= 51; i++ {
		forged := "198.51.100." + strconv.Itoa(i)
		if ok, _ := l.allow(xffReq691("127.0.0.1:1", forged+", 203.0.113.9")); ok {
			allowed++
		}
	}
	if allowed != 0 {
		t.Fatalf("%d of 50 requests with a fresh forged left-most entry were allowed after the budget was spent", allowed)
	}
}

// Replacing proxy: X-Forwarded-For holds exactly the client the proxy saw,
// so the result is what it always was.
func TestXFF691_ReplacingProxyUnchanged(t *testing.T) {
	trusted := trustedNets691(t, "127.0.0.1/32")
	wantClient691(t, xffReq691("127.0.0.1:1", "203.0.113.9"), trusted, "203.0.113.9")
	wantClient691(t, xffReq691("127.0.0.1:1", "2001:db8:1:2::5"), trusted, "2001:db8:1:2::5")
	// Trusted proxy that forwards nothing still identifies itself.
	wantClient691(t, xffReq691("127.0.0.1:1"), trusted, "127.0.0.1")

	l := newIPRateLimiter("test", 1, trusted, nil)
	if ok, _ := l.allow(xffReq691("127.0.0.1:1", "203.0.113.9")); !ok {
		t.Fatal("first client refused")
	}
	if ok, _ := l.allow(xffReq691("127.0.0.1:2", "203.0.113.9")); ok {
		t.Fatal("same client not limited")
	}
	if ok, _ := l.allow(xffReq691("127.0.0.1:3", "203.0.113.10")); !ok {
		t.Fatal("other client limited")
	}
}

// A chain of trusted hops (CDN -> edge -> local proxy): every trusted hop is
// skipped and the first untrusted address from the right is the client.
func TestXFF691_ChainOfTrustedHops(t *testing.T) {
	trusted := trustedNets691(t, "127.0.0.1/32", "10.0.0.0/8", "192.0.2.0/24")
	wantClient691(t,
		xffReq691("127.0.0.1:1", "198.51.100.66, 203.0.113.9, 192.0.2.7, 10.1.1.1, 10.2.2.2"),
		trusted, "203.0.113.9")
	// The same chain spread over several header lines (RFC 9110 5.3: equal
	// to one comma-joined line, in order). A client-sent line comes first.
	wantClient691(t,
		xffReq691("127.0.0.1:1", "198.51.100.66", "203.0.113.9, 192.0.2.7", "10.1.1.1"),
		trusted, "203.0.113.9")
	// Every hop trusted: the furthest one is the best we know.
	wantClient691(t, xffReq691("127.0.0.1:1", "10.1.1.1, 10.2.2.2"), trusted, "10.1.1.1")
}

// Malformed and empty entries fail closed: the walk stops and the client is
// the nearest address a trusted hop vouched for (the peer when the bad entry
// is right-most). Skipping past one would hand the identity to whatever lies
// further left, which is client-controlled behind an appending proxy.
func TestXFF691_MalformedAndEmptyEntries(t *testing.T) {
	trusted := trustedNets691(t, "127.0.0.1/32", "10.0.0.0/8")
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"garbage right-most", []string{"203.0.113.9, not-an-ip"}, "127.0.0.1"},
		{"garbage behind a trusted hop", []string{"203.0.113.9, not-an-ip, 10.2.2.2"}, "10.2.2.2"},
		{"empty header", []string{""}, "127.0.0.1"},
		{"only separators", []string{" , ,"}, "127.0.0.1"},
		{"trailing empty entry", []string{"203.0.113.9,"}, "127.0.0.1"},
		{"empty entry behind a trusted hop", []string{"203.0.113.9,, 10.2.2.2"}, "10.2.2.2"},
		{"empty last line", []string{"203.0.113.9", ""}, "127.0.0.1"},
		{"untrusted client found before garbage", []string{"not-an-ip, 203.0.113.9"}, "203.0.113.9"},
		{"IPv4 with port", []string{"198.51.100.66, 203.0.113.9:4711"}, "203.0.113.9"},
		{"IPv6", []string{"198.51.100.66, 2001:db8:1:2::5"}, "2001:db8:1:2::5"},
		{"bracketed IPv6 with port", []string{"198.51.100.66, [2001:db8:1:2::5]:443"}, "2001:db8:1:2::5"},
		{"IPv6 zone is malformed", []string{"203.0.113.9, fe80::1%eth0"}, "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantClient691(t, xffReq691("127.0.0.1:1", tc.lines...), trusted, tc.want)
		})
	}
}

// IPv6 behind an appending proxy is still counted per /64, and a forged
// prefix does not escape it.
func TestXFF691_IPv6Per64BehindAppendingProxy(t *testing.T) {
	trusted := trustedNets691(t, "::1/128")
	l := newIPRateLimiter("test", 1, trusted, nil)
	if ok, _ := l.allow(xffReq691("[::1]:1", "2001:db8:ffff::1, 2001:db8:1:2::5")); !ok {
		t.Fatal("first refused")
	}
	if ok, _ := l.allow(xffReq691("[::1]:1", "2001:db8:eeee::1, 2001:db8:1:2:ffff::9")); ok {
		t.Fatal("same /64 with a fresh forged prefix not limited")
	}
	if ok, _ := l.allow(xffReq691("[::1]:1", "2001:db8:1:3::1")); !ok {
		t.Fatal("other /64 limited")
	}
}

// Table fill: one source sending more forged left-most entries than the
// shipped table holds must occupy one entry, and other visitors keep being
// served.
func TestXFF691_TableFillFromOneSourceDoesNotLockOutOthers(t *testing.T) {
	trusted := trustedNets691(t, "127.0.0.1/32")
	l := newIPRateLimiter("test", 1, trusted, nil)
	for i := 0; i < ipLimiterMaxClients+100; i++ {
		forged := "198.51." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256)
		l.allow(xffReq691("127.0.0.1:1", forged+", 203.0.113.9"))
	}
	if n := len(l.clients); n != 1 {
		t.Fatalf("client table holds %d entries after one source's flood, want 1", n)
	}
	if ok, _ := l.allow(xffReq691("127.0.0.1:1", "203.0.113.200")); !ok {
		t.Fatal("a genuine visitor was locked out by one source's forged entries")
	}
}

// /ws shares the helper: a forged left-most entry must not buy a fresh
// upgrade budget there either.
func TestXFF691_WSLimiterAppendingProxy(t *testing.T) {
	l := limiter(0, 1, []string{"127.0.0.1/32"}, nil)
	if ok, _, key, _ := l.allow(req("127.0.0.1:1", "198.51.100.1, 203.0.113.9")); !ok || key != "203.0.113.9" {
		t.Fatalf("key = %q (ok=%v), want the proxy-appended 203.0.113.9", key, ok)
	}
	if ok, _, _, _ := l.allow(req("127.0.0.1:1", "198.51.100.2, 203.0.113.9")); ok {
		t.Fatal("a fresh forged left-most entry bought a fresh /ws upgrade budget")
	}
}
