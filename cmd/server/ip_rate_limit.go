package main

// #681 M3 / #682 review F3 — a per-client request budget for the two
// endpoints that spend the operator's Dataforsyningen token on anonymous
// visitors: /api/terrain/profile and the DHM tile proxy.
//
// Clients are identified exactly like the /ws limits (#1794, clientAddr):
// X-Forwarded-For only from a configured trusted proxy, walked from the
// right past trusted hops (#691), and no per-IP limit at all when every
// request comes from an unconfigured local proxy, because that would
// throttle every visitor together. That case logs one
// warning naming the setting to fix.
//
// The budget is a GCRA token bucket: perMin requests at once, refilled at
// perMin per minute. It keeps one timestamp per client, so the client table
// costs the same whatever the limit, and an entry whose budget is full
// again carries no state and is dropped. The table is capped: when it is
// full of active clients a new client is refused, not given a fresh budget
// by evicting someone else.

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ipLimiterMaxClients bounds the client table: ~100 bytes per entry, so
// well under a megabyte, and far above the distinct clients a regional
// instance sees within one minute.
const ipLimiterMaxClients = 4096

// clientAddr resolves the address per-IP limits are counted against, and
// reports whether that address actually identifies a client.
//
// distinct is false when the peer is a loopback or private address and is
// not a trusted proxy: the request has almost certainly crossed a reverse
// proxy whose X-Forwarded-For we are not allowed to believe, so every
// visitor looks identical and per-IP limits would punish the wrong people.
func clientAddr(r *http.Request, trustedProxies []*net.IPNet) (ip net.IP, distinct bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(strings.TrimSpace(host))
	if peer == nil {
		return nil, false
	}
	if ipInAny(peer, trustedProxies) {
		return forwardedClient(r.Header.Values("X-Forwarded-For"), peer, trustedProxies), true
	}
	if peer.IsLoopback() || peer.IsPrivate() || peer.IsLinkLocalUnicast() {
		return peer, false
	}
	return peer, true
}

// forwardedClient finds the client behind a trusted peer (#691). It walks
// X-Forwarded-For from the RIGHT, the end our own proxies write, and skips
// hops that are themselves trusted proxies; the first untrusted address is
// the client. The left-most entry is not believed: a proxy that appends to
// the header (nginx $proxy_add_x_forwarded_for) leaves whatever the client
// sent there, so it would let one client mint a fresh budget per request
// and fill the client table with forged addresses. A proxy that replaces
// the header writes one entry, which this walk returns as before.
//
// Several header lines count as one comma-joined line, in order (RFC 9110
// 5.3), so a client-sent line ahead of the proxy's own is still left of it.
//
// A malformed or empty entry fails closed: the walk stops and the result is
// the nearest address a trusted hop vouched for (peer, when the bad entry is
// right-most). A trusted proxy only ever writes the address it saw, so a
// bad entry there is not its work, and skipping past it would hand the
// identity to whatever lies further left — client-controlled behind an
// appending proxy. Stopping instead attributes the request to a known hop,
// which keeps the limit enforced rather than bypassable. "host:port" and
// "[v6]:port" entries are accepted, since some proxies append the port.
//
// When every entry is trusted, the left-most (furthest) trusted hop is the
// best we know. The walk does not allocate: it runs on every /ws upgrade
// and every limited DHM request.
func forwardedClient(lines []string, peer net.IP, trusted []*net.IPNet) net.IP {
	client := peer
	for li := len(lines) - 1; li >= 0; li-- {
		rest := lines[li]
		for {
			hop := rest
			cut := strings.LastIndexByte(rest, ',')
			if cut >= 0 {
				hop, rest = rest[cut+1:], rest[:cut]
			}
			ip := parseForwardedHop(hop)
			if ip == nil {
				return client
			}
			if !ipInAny(ip, trusted) {
				return ip
			}
			client = ip
			if cut < 0 {
				break
			}
		}
	}
	return client
}

// parseForwardedHop parses one X-Forwarded-For entry: a bare address, or
// one with a port. Anything else, including an empty entry, is nil.
func parseForwardedHop(s string) net.IP {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return net.ParseIP(host)
	}
	return nil
}

// ipRateLimiter is a per-client budget of perMin requests per minute. A nil
// *ipRateLimiter allows everything.
type ipRateLimiter struct {
	what       string // the config key, named in the warning
	interval   time.Duration
	burst      time.Duration // perMin × interval: one minute
	trusted    []*net.IPNet
	logf       func(format string, args ...interface{})
	now        func() time.Time
	maxClients int

	mu sync.Mutex
	// clients maps a client key to its theoretical arrival time: the
	// budget is exhausted while tat − now > burst − interval.
	clients map[string]time.Time

	warnIndistinct sync.Once
	rejected       atomic.Int64
}

// newIPRateLimiter returns nil when perMin ≤ 0 (off).
func newIPRateLimiter(what string, perMin int, trusted []*net.IPNet, logf func(string, ...interface{})) *ipRateLimiter {
	if perMin <= 0 {
		return nil
	}
	if logf == nil {
		logf = log.Printf
	}
	interval := time.Minute / time.Duration(perMin)
	return &ipRateLimiter{
		what: what, interval: interval, burst: interval * time.Duration(perMin),
		trusted: trusted, logf: logf, now: time.Now, maxClients: ipLimiterMaxClients,
		clients: make(map[string]time.Time),
	}
}

// clientKey is the address a budget belongs to. IPv6 is counted per /64,
// the smallest block a subscriber is given, so rotating addresses inside
// it buys nothing.
func clientKey(ip net.IP) string {
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

// allow charges one request to r's client. When the budget is spent it
// returns false and how long until the next request would be allowed.
func (l *ipRateLimiter) allow(r *http.Request) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	ip, distinct := clientAddr(r, l.trusted)
	if !distinct {
		l.warnIndistinct.Do(func() {
			l.logf("[dhm] WARNING: %s is set but requests arrive from a local or private address, "+
				"so clients cannot be told apart and the limit is NOT enforced. "+
				"Set webSocket.trustedProxies to your reverse proxy's address to enable it.", l.what)
		})
		return true, 0
	}
	key := clientKey(ip)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	tat, known := l.clients[key]
	if !known && len(l.clients) >= l.maxClients {
		l.collectLocked(now)
		if len(l.clients) >= l.maxClients {
			l.rejected.Add(1)
			return false, time.Minute
		}
	}
	if tat.Before(now) {
		tat = now
	}
	next := tat.Add(l.interval)
	if wait := next.Sub(now) - l.burst; wait > 0 {
		l.rejected.Add(1)
		return false, wait
	}
	l.clients[key] = next
	return true, 0
}

// refund gives back one charge made by allow for r's client, for a request
// that was refused before it did any upstream work (#707). It is a no-op
// for a nil limiter, an indistinct client (never charged) or an unknown one.
func (l *ipRateLimiter) refund(r *http.Request) {
	if l == nil {
		return
	}
	ip, distinct := clientAddr(r, l.trusted)
	if !distinct {
		return
	}
	key := clientKey(ip)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	tat, known := l.clients[key]
	if !known {
		return
	}
	if tat = tat.Add(-l.interval); tat.After(now) {
		l.clients[key] = tat
	} else {
		delete(l.clients, key) // budget full again: no state
	}
}

// collectLocked drops clients whose budget is full again: they carry no
// state. Caller holds l.mu.
func (l *ipRateLimiter) collectLocked(now time.Time) {
	for k, tat := range l.clients {
		if !tat.After(now) {
			delete(l.clients, k)
		}
	}
}

// writeRateLimited answers 429 with Retry-After in whole seconds, rounded
// up (a refusal always waits more than zero).
func writeRateLimited(w http.ResponseWriter, retry time.Duration) {
	setRetryAfter(w, retry)
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
}
