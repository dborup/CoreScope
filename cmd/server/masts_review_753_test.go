package main

// #724 M3 — the #753 review nits on GET /api/masts (N1–N3, N8).

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/mastedb"
)

// N1: admission is the shared dhm.Admission (#744), so a refused call takes
// no slot and starts no worker; the slots in use are what Flight holds.
func TestMastsAdmissionIsSharedHelper(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.maxInFlight = 2
	p := mustMastsProxy(t, opts)
	if p.admit.Cap() != 2 {
		t.Fatalf("admission cap %d, want 2", p.admit.Cap())
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mastsGet(p, cellQuery(mastedb.Cell{E: 263 + i, N: 3127}), clientB)
		}(i)
	}
	waitMasts(t, func() bool { return p.admit.InUse() == 2 && f.inFlight.Load() == 2 })
	for i := 0; i < 10; i++ {
		if w := mastsGet(p, cellQuery(mastedb.Cell{E: 300 + i, N: 3127}), clientA); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("refusal %d: status %d, want 503", i, w.Code)
		}
	}
	// A refused call never registered a flight: only the two held fetches
	// have waiters.
	if n := p.flight.Pending(); n != 2 {
		t.Fatalf("%d flight waiters after refusals, want 2 (a refused call joined or started a flight)", n)
	}
	close(f.gate)
	wg.Wait()
	waitMasts(t, func() bool { return p.admit.InUse() == 0 })
}

// N2: an admitted fetch that never gets one of the upstream slots before
// the deadline did no upstream work. It is a 503 like an admission
// refusal, refunded and logged as busy, not a charged 504 logged as an
// upstream timeout.
func TestMastsStarvedFetchIsBusyAndRefunded(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.maxInFlight = 4
	opts.rateLimitPerMin = 2
	p := mustMastsProxy(t, opts)

	// Each held fetch comes from its own client: two concurrent requests
	// from one client on a budget of exactly 2 can race inside the limiter
	// (it reads the clock before its lock), which is not what this tests.
	var wg sync.WaitGroup
	for i := 0; i < testMastsSlots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mastsGet(p, cellQuery(mastedb.Cell{E: 263 + i, N: 3127}), fmt.Sprintf("198.51.100.%d:40000", 20+i))
		}(i)
	}
	waitMasts(t, func() bool { return f.inFlight.Load() == testMastsSlots })
	// The two held fetches keep the default deadline; only the third, which
	// can never get a slot, runs out.
	p.deadline = 150 * time.Millisecond

	w := mastsGet(p, cellQuery(mastedb.Cell{E: 300, N: 3127}), clientA)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"antenna data busy"`) {
		t.Fatalf("starved fetch: status %d body %q, want 503 busy", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("starved fetch headers %v", w.Header())
	}
	if got := logs.String(); strings.Contains(got, "upstream cell failed") || !strings.Contains(got, "upstream slots") {
		t.Fatalf("starved fetch logged as %q, want the busy line and no upstream failure", got)
	}
	if n := f.hits.Load(); int(n) != testMastsSlots {
		t.Fatalf("upstream hits %d, want %d (the starved fetch reached upstream)", n, testMastsSlots)
	}
	close(f.gate)
	wg.Wait()
	// The starved request was refunded: clientA still has its budget of 2.
	for i := 0; i < 2; i++ {
		if w := mastsGet(p, cellQuery(mastedb.Cell{E: 310 + i, N: 3127}), clientA); w.Code != 200 {
			t.Fatalf("after the starved fetch, request %d: status %d (the starved fetch was charged)", i, w.Code)
		}
	}
}

// N3: the indistinct-client warning carries the limiter's own log prefix.
func TestMastsLimiterWarningIsTaggedMasts(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	p := mustMastsProxy(t, testMastsOptions(f, &logs))
	mastsGet(p, cellQuery(testMastsCell), "127.0.0.1:5000")
	got := logs.String()
	if !strings.Contains(got, "[masts] WARNING: map.masts.cellRateLimitPerMin") || strings.Contains(got, "[dhm]") {
		t.Fatalf("limiter warning %q, want a [masts] line and no [dhm]", got)
	}
}

func TestIPRateLimiterWarningTag(t *testing.T) {
	for _, tag := range []string{"dhm", "masts"} {
		var logs safeLog
		l := newIPRateLimiter(tag, "some.key", 5, nil, logs.printf)
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.1:1"
		l.allow(r)
		if got := logs.String(); !strings.HasPrefix(got, "["+tag+"] WARNING: some.key") {
			t.Fatalf("tag %s: warning %q", tag, got)
		}
	}
}

// N8: one throttle for the masts and terrain log lines. allow is the
// count-free form the masts failure line uses.
func TestLogThrottle(t *testing.T) {
	var th logThrottle
	t0 := time.Unix(1000, 0)
	if n, ok := th.note(t0, 3, time.Minute); !ok || n != 3 {
		t.Fatalf("first note: %d %v, want 3 true", n, ok)
	}
	if _, ok := th.note(t0.Add(59*time.Second), 7, time.Minute); ok {
		t.Fatal("note inside the interval went through")
	}
	if n, ok := th.note(t0.Add(time.Minute), 9, time.Minute); !ok || n != 6 {
		t.Fatalf("note after the interval: %d %v, want 6 true", n, ok)
	}
	var a logThrottle
	if !a.allow(t0, time.Minute) || a.allow(t0.Add(time.Second), time.Minute) || !a.allow(t0.Add(time.Minute), time.Minute) {
		t.Fatal("allow does not let one line through per interval")
	}
}
