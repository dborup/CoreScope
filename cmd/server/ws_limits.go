package main

// Issue #1794: per-IP limits and a deny list for the /ws upgrade, as defence
// in depth behind the CheckOrigin allowlist from #1793.
//
// CheckOrigin only stops browsers. A Go, Python or curl client can omit the
// Origin header entirely or forge one, connect, and sit in the hub. These
// limits work on the transport instead: who is connecting, how often, and how
// many at once.
//
// THE THING THAT MAKES THIS DANGEROUS TO SHIP NAIVELY, stated up front:
// most CoreScope installs run behind nginx, Caddy, Traefik or a k8s ingress
// (cdn_detection.go says so in as many words). For those, r.RemoteAddr is the
// proxy, 127.0.0.1 for every visitor on earth. A per-IP cap keyed on that
// address protects nobody; it counts the whole internet as one client and
// hands the sixth legitimate browser tab a 403. That is a self-inflicted
// outage, not hardening.
//
// So enforcement is conditional on being able to tell clients apart:
//   - X-Forwarded-For is honoured ONLY when the request arrives from an
//     address the operator listed in trustedProxies. Otherwise it is an
//     attacker-supplied header, and trusting it would let anyone forge a
//     fresh source IP per connection, which is worse than no limit at all.
//   - If the peer looks like a local reverse proxy and no trustedProxies is
//     configured, clients cannot be distinguished, so the per-IP limits are
//     SKIPPED and one warning is logged telling the operator what to set.
//     The deny list still applies, because an explicit block is the
//     operator's own instruction rather than an inference.
//
// #369 adds the piece the per-IP design cannot provide: a GLOBAL ceiling on
// concurrent clients. Everything above is conditional on telling clients
// apart, so in the shipped Docker topology (bundled Caddy -> localhost:3000,
// no trustedProxies) it enforces nothing at all. The ceiling is unconditional
// — it counts connections, not clients — so it still bounds the resource that
// actually runs out: sockets, goroutines and 256-slot send queues. It is the
// last check before a slot is booked, so the deny list and the per-IP rules
// keep their reasons, their statuses and their precedence.
//
// Not shipped: a default deny list of hosting-provider ranges. It was
// proposed on the issue (44 CIDRs for one VPS host, after a single scraper
// was seen). Blanket-blocking a provider by default breaks legitimate
// operators who host there, is undiscoverable by the person locked out, and
// ages badly as ranges are reassigned. Operators who want it can configure it.

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// wsUpgradeWindow is the span the upgrade budget is counted over. One minute
// matches the config field name (upgradesPerMinPerIP) so an operator reading
// the config can predict the behaviour without reading this file.
const wsUpgradeWindow = time.Minute

// wsBucketIdleGC is how long an idle per-IP record is kept before collection.
// Longer than the window, so a client that pauses briefly cannot earn a fresh
// budget by being forgotten.
const wsBucketIdleGC = 10 * time.Minute

// wsRejectReason is the label used both in the log line and in the
// /api/stats counters, so an operator can match one to the other.
type wsRejectReason string

const (
	wsRejectDeny    wsRejectReason = "deny"
	wsRejectRate    wsRejectReason = "rate"
	wsRejectConnCap wsRejectReason = "conncap"
	// wsRejectGlobalCap is the server-wide ceiling from #369. Kept distinct
	// from conncap so /api/stats tells "this one client is greedy" apart from
	// "the server is full", which call for different operator responses.
	wsRejectGlobalCap wsRejectReason = "globalcap"
)

// wsDefaultMaxClients is the shipped global ceiling. See WebSocketConfig.MaxClients
// for the derivation; config.example.json carries the operator-facing version.
const wsDefaultMaxClients = 512

// wsGlobalCapRetryAfter is the Retry-After sent with a ceiling refusal, in
// seconds as the header requires. Three seconds matches the frontend's own
// reconnect delay (WS_RECONNECT_MS, public/app.js), so a refused browser tab
// and a refused script come back on the same cadence — and a slot freed by
// any disconnect is offered again almost immediately.
const wsGlobalCapRetryAfter = "3"

type wsIPState struct {
	conns      int
	upgrades   []time.Time // timestamps inside the current window
	lastSeenAt time.Time
}

// wsLimiter holds the parsed configuration and the live per-IP state. A nil
// *wsLimiter allows everything, so a server that never configures limits
// behaves exactly as it did before this change.
type wsLimiter struct {
	mu sync.Mutex

	maxConnsPerIP  int // 0 disables the concurrent-connection cap
	upgradesPerMin int // 0 disables the upgrade-rate limit
	maxClients     int // #369: global concurrent ceiling; 0 disables it

	// totalConns is the number of slots currently booked against maxClients.
	// It counts reservations, not registered hub clients: a slot is taken
	// before the handshake and returned by release(), so a connection that
	// dies between admission and Register can never be double-counted or
	// leaked. Guarded by mu.
	totalConns int

	trustedProxies []*net.IPNet
	denyNets       []*net.IPNet

	state map[string]*wsIPState

	rejectedDeny    atomic.Int64
	rejectedRate    atomic.Int64
	rejectedConnCap atomic.Int64
	rejectedGlobal  atomic.Int64

	// warnedIndistinct fires the "set trustedProxies" warning once, rather
	// than on every upgrade from behind an unconfigured proxy.
	warnedIndistinct sync.Once
}

func newWSLimiter() *wsLimiter {
	return &wsLimiter{state: make(map[string]*wsIPState)}
}

// parseCIDRList turns operator strings into networks. A bare address is
// accepted and treated as a single-host network, because "1.2.3.4" is what an
// operator naturally writes and silently ignoring it would be the worst
// possible failure mode for a deny list. An unparseable entry is logged and
// skipped rather than aborting startup: one typo must not take the server
// down, but it must not pass unnoticed either.
func parseCIDRList(entries []string, what string) []*net.IPNet {
	return parseCIDRs(entries, func(e string) {
		log.Printf("[ws] WARNING: %s entry %q is neither an IP nor a CIDR, ignored", what, e)
	})
}

// parseCIDRListQuiet is parseCIDRList for a second consumer of a list that
// the first one already warned about.
func parseCIDRListQuiet(entries []string) []*net.IPNet {
	return parseCIDRs(entries, nil)
}

func parseCIDRs(entries []string, warn func(entry string)) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(entries))
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			out = append(out, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		if warn != nil {
			warn(e)
		}
	}
	return out
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP resolves the address the limits are counted against, and reports
// whether that address actually identifies a client.
//
// distinct is false when the peer is a loopback or private address and no
// trustedProxies is configured: the request has almost certainly crossed a
// reverse proxy whose X-Forwarded-For we are not allowed to believe, so every
// visitor looks identical and per-IP limits would punish the wrong people.
func (l *wsLimiter) clientIP(r *http.Request) (ip net.IP, distinct bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(strings.TrimSpace(host))
	if peer == nil {
		return nil, false
	}
	if ipInAny(peer, l.trustedProxies) {
		// First hop is the client; later hops were appended by intermediaries
		// we have no reason to trust individually.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if fwd := net.ParseIP(first); fwd != nil {
				return fwd, true
			}
		}
		// A trusted proxy that forwarded no XFF still identifies itself.
		return peer, true
	}
	if peer.IsLoopback() || peer.IsPrivate() || peer.IsLinkLocalUnicast() {
		return peer, false
	}
	return peer, true
}

// allow decides whether an upgrade may proceed. On rejection it returns the
// reason. On success it returns a release func the caller MUST invoke when
// the connection ends, so the concurrent-connection counts come back down.
//
// Order of checks, and why: deny list (an explicit instruction) → per-IP rate
// → per-IP concurrent cap → global ceiling (#369). The ceiling goes last so
// every pre-existing refusal keeps the reason, status and counter it had
// before #369; it is still evaluated before anything is charged, so a client
// the ceiling turns away does not spend its own per-IP budget on a
// connection it never got.
func (l *wsLimiter) allow(r *http.Request) (ok bool, reason wsRejectReason, key string, release func()) {
	noop := func() {}
	if l == nil {
		return true, "", "", noop
	}
	ip, distinct := l.clientIP(r)

	// The deny list is an explicit operator instruction, so it applies even
	// when clients cannot be told apart. Behind an unconfigured proxy it
	// simply never matches, which is visible rather than silently wrong.
	if ipInAny(ip, l.denyNets) {
		l.rejectedDeny.Add(1)
		return false, wsRejectDeny, ip.String(), noop
	}

	perIPConfigured := l.maxConnsPerIP > 0 || l.upgradesPerMin > 0
	if perIPConfigured && !distinct {
		l.warnedIndistinct.Do(func() {
			// #369: say which protection is actually left. Claiming the
			// ceiling still applies when the operator set maxClients to 0
			// would be the one wrong thing to print here.
			fallback := "webSocket.maxClients is 0, so NOTHING bounds concurrent clients"
			if l.maxClients > 0 {
				fallback = fmt.Sprintf("the global webSocket.maxClients ceiling (%d) still applies", l.maxClients)
			}
			log.Printf("[ws] WARNING: per-IP limits are configured but every request arrives from %s, "+
				"so clients cannot be told apart and the limits are NOT enforced. "+
				"Set webSocket.trustedProxies to your reverse proxy's address to enable them. "+
				"Meanwhile %s.", ip, fallback)
		})
	}
	// perIP is what actually gets enforced: configured AND able to tell
	// clients apart. The ceiling does not need that distinction, so it is
	// enforced in both cases.
	perIP := perIPConfigured && distinct
	if !perIP && l.maxClients <= 0 {
		return true, "", "", noop
	}

	if ip != nil {
		key = ip.String()
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)

	var st *wsIPState
	if perIP {
		st = l.state[key]
		if st == nil {
			st = &wsIPState{}
			l.state[key] = st
		}
		st.lastSeenAt = now

		if l.upgradesPerMin > 0 {
			cutoff := now.Add(-wsUpgradeWindow)
			kept := st.upgrades[:0]
			for _, t := range st.upgrades {
				if t.After(cutoff) {
					kept = append(kept, t)
				}
			}
			st.upgrades = kept
			if len(st.upgrades) >= l.upgradesPerMin {
				l.rejectedRate.Add(1)
				return false, wsRejectRate, key, noop
			}
		}
		if l.maxConnsPerIP > 0 && st.conns >= l.maxConnsPerIP {
			l.rejectedConnCap.Add(1)
			return false, wsRejectConnCap, key, noop
		}
	}

	// #369: the server-wide ceiling. Checked under the same lock as the
	// per-IP state and the booking below, so concurrent handshakes cannot
	// both read "one slot left" and both take it.
	if l.maxClients > 0 && l.totalConns >= l.maxClients {
		l.rejectedGlobal.Add(1)
		return false, wsRejectGlobalCap, key, noop
	}

	// Every budget has room, so charge them together: a rejected upgrade
	// must never consume rate budget for a connection it did not get.
	if perIP {
		if l.upgradesPerMin > 0 {
			st.upgrades = append(st.upgrades, now)
		}
		st.conns++
	}
	l.totalConns++

	var once sync.Once
	return true, "", key, func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.totalConns > 0 {
				l.totalConns--
			}
			if !perIP {
				return
			}
			if s := l.state[key]; s != nil && s.conns > 0 {
				s.conns--
				s.lastSeenAt = time.Now()
			}
		})
	}
}

// reserved reports how many slots are currently booked against the global
// ceiling. Nil-safe, so callers need no special case for an unlimited hub.
func (l *wsLimiter) reserved() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalConns
}

// gcLocked drops records with no live connections and no recent activity, so
// the map cannot grow without bound when the server is scanned from many
// addresses. Caller must hold l.mu.
func (l *wsLimiter) gcLocked(now time.Time) {
	for k, st := range l.state {
		if st.conns == 0 && now.Sub(st.lastSeenAt) > wsBucketIdleGC {
			delete(l.state, k)
		}
	}
}

// counts reports the rejection tallies for /api/stats.
func (l *wsLimiter) counts() (deny, rate, connCap int64) {
	if l == nil {
		return 0, 0, 0
	}
	return l.rejectedDeny.Load(), l.rejectedRate.Load(), l.rejectedConnCap.Load()
}

// globalCapRejections reports how many upgrades the #369 ceiling turned away
// since boot. Kept separate from counts() so the #1794 call sites, and the
// tests that pin their three-value shape, stay untouched.
func (l *wsLimiter) globalCapRejections() int64 {
	if l == nil {
		return 0
	}
	return l.rejectedGlobal.Load()
}
