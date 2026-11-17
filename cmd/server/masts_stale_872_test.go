package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/mastedb"
	"github.com/meshcore-analyzer/mastedb/mastedbtest"
)

// #872 owner additions: the upstream slot count is map.masts.upstreamSlots
// (default 16, 1-256, maxInFlight never below it), and stale-if-error: a
// cell past its 7-day TTL but within map.masts.maxStaleDays (default 90)
// is served, marked stale, when Mastedatabasen fails or the breaker is
// open. Everything talks to the httptest fake; time is a fake clock.

// ── Upstream slots and maxInFlight ─────────────────────────────────────────

func TestMastsUpstreamSlotsSetting(t *testing.T) {
	if mastsUpstreamSlotsDefault != 16 || mastsUpstreamSlotsMax != 256 {
		t.Fatalf("slots default %d max %d, want 16 and 256", mastsUpstreamSlotsDefault, mastsUpstreamSlotsMax)
	}
	if mastsMaxInFlightCap != 256 {
		t.Fatalf("maxInFlight cap %d, want 256", mastsMaxInFlightCap)
	}
	iv := func(v int) *int { return &v }
	cases := []struct {
		name               string
		slots, inFlight    *int
		wantSlots, wantMax int
		warn               string
	}{
		{"defaults", nil, nil, 16, 16, ""},
		{"lowest", iv(1), nil, 1, 16, ""},
		{"highest", iv(256), iv(256), 256, 256, ""},
		{"zero", iv(0), nil, 16, 16, "upstreamSlots"},
		{"over", iv(257), nil, 16, 16, "upstreamSlots"},
		{"negative", iv(-3), nil, 16, 16, "upstreamSlots"},
		{"maxInFlight at cap", nil, iv(256), 16, 256, ""},
		{"maxInFlight over cap", nil, iv(257), 16, 16, "maxInFlight"},
		// maxInFlight always admits at least the configured slot count.
		{"maxInFlight below slots", iv(64), iv(16), 64, 64, "maxInFlight"},
		{"maxInFlight default below slots", iv(100), nil, 100, 100, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs safeLog
			slots, max := mastsConcurrency(MastsConfig{UpstreamSlots: tc.slots, MaxInFlight: tc.inFlight}, logs.printf)
			if slots != tc.wantSlots || max != tc.wantMax {
				t.Fatalf("slots %d maxInFlight %d, want %d and %d", slots, max, tc.wantSlots, tc.wantMax)
			}
			if tc.warn == "" && logs.String() != "" {
				t.Fatalf("unexpected warning: %s", logs.String())
			}
			if tc.warn != "" && !strings.Contains(logs.String(), tc.warn) {
				t.Fatalf("warning %q does not name %s", logs.String(), tc.warn)
			}
		})
	}
}

// The configured slot count is what bounds the open upstream requests.
func TestMastsConfiguredUpstreamSlotsHonoured(t *testing.T) {
	for _, slots := range []int{1, 5} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			f := newFakeWFS(t)
			f.gate = make(chan struct{})
			var logs safeLog
			opts := testMastsOptions(f, &logs)
			opts.rateLimitPerMin = 0
			opts.upstreamSlots = slots
			opts.maxInFlight = 16
			p := mustMastsProxy(t, opts)

			const n = 8
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) { defer wg.Done(); mastsGet(p, cellQuery(cellN(i)), clientA) }(i)
			}
			waitMasts(t, func() bool { return f.inFlight.Load() == int32(slots) && p.flight.Pending() == n })
			time.Sleep(50 * time.Millisecond)
			if m := f.maxSeen.Load(); m != int32(slots) {
				t.Fatalf("%d upstream requests open at once, want %d", m, slots)
			}
			close(f.gate)
			wg.Wait()
			if m := f.maxSeen.Load(); m != int32(slots) {
				t.Fatalf("%d upstream requests open at once, want %d", m, slots)
			}
		})
	}
}

// The breaker opens and closes as before with a non-default slot count.
func TestMastsBreakerWithConfiguredSlots(t *testing.T) {
	f := newFakeWFS(t)
	var mode atomic.Int32
	f.respond = switchable(&mode)
	var logs safeLog
	clk := newBreakerClock()
	opts := testMastsOptions(f, &logs)
	opts.rateLimitPerMin = 0
	opts.now = clk.now
	opts.upstreamSlots = 7
	p := mustMastsProxy(t, opts)

	mode.Store(modeFail)
	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		mastsGet(p, cellQuery(cellN(i)), clientA)
	}
	if w := mastsGet(p, cellQuery(cellN(20)), clientA); !isSourceDown(w) {
		t.Fatalf("breaker not open with 7 slots: %d", w.Code)
	}
	clk.advance(mastsBreakerCooldownDefault)
	mode.Store(modeOK)
	if w := mastsGet(p, cellQuery(cellN(21)), clientA); w.Code != http.StatusOK {
		t.Fatalf("probe: %d, want 200", w.Code)
	}
	if w := mastsGet(p, cellQuery(cellN(22)), clientA); w.Code != http.StatusOK {
		t.Fatalf("after close: %d, want 200", w.Code)
	}
}

// ── Stale-if-error ─────────────────────────────────────────────────────────

const (
	modeOK int32 = iota
	modeFail
	modeHang
	modeMalformed
)

// switchable answers per *mode, read atomically so a test may flip it
// between requests without a data race with the fake's handler.
func switchable(mode *atomic.Int32) func(http.ResponseWriter, mastedb.Cell) {
	return func(w http.ResponseWriter, c mastedb.Cell) {
		switch mode.Load() {
		case modeFail:
			w.WriteHeader(http.StatusServiceUnavailable)
		case modeHang:
			time.Sleep(300 * time.Millisecond)
		case modeMalformed:
			w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
			w.Write([]byte("<not gml"))
		default:
			w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
			w.Write(mastedbtest.GML(c.E, c.N, 3, "3"))
		}
	}
}

func staleProxy(t *testing.T, f *fakeWFS, mode *atomic.Int32, logs *safeLog, clk *breakerClock) *mastsProxy {
	t.Helper()
	f.respond = switchable(mode)
	opts := testMastsOptions(f, logs)
	opts.rateLimitPerMin = 0
	opts.now = clk.now
	opts.client.Timeout = 50 * time.Millisecond
	return mustMastsProxy(t, opts)
}

func assertStale(t *testing.T, w interface {
	Header() http.Header
}, code int, body, want string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200 with the stale copy", code)
	}
	h := w.Header()
	if h.Get(mastsStaleHeader) != "1" {
		t.Fatalf("%s = %q, want 1", mastsStaleHeader, h.Get(mastsStaleHeader))
	}
	if h.Get("X-CoreScope-Cache") != "stale" {
		t.Fatalf("X-CoreScope-Cache %q, want stale", h.Get("X-CoreScope-Cache"))
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store (the browser must not keep a stale copy)", h.Get("Cache-Control"))
	}
	if body != want {
		t.Fatalf("stale body differs from the cached copy:\n%s\n%s", body, want)
	}
}

func TestMastsStaleServedOnUpstreamFailure(t *testing.T) {
	for _, m := range []struct {
		name string
		mode int32
	}{{"5xx", modeFail}, {"timeout", modeHang}} {
		t.Run(m.name, func(t *testing.T) {
			f := newFakeWFS(t)
			var mode atomic.Int32
			var logs safeLog
			clk := newBreakerClock()
			p := staleProxy(t, f, &mode, &logs, clk)

			cell := cellQuery(cellN(0))
			first := mastsGet(p, cell, clientA)
			if first.Code != http.StatusOK || first.Header().Get(mastsStaleHeader) != "" {
				t.Fatalf("first fetch %d %q", first.Code, first.Header().Get(mastsStaleHeader))
			}
			clk.advance(mastsCacheTTL + 24*time.Hour)
			mode.Store(m.mode)
			w := mastsGet(p, cell, clientA)
			assertStale(t, w, w.Code, w.Body.String(), first.Body.String())
			if n := f.hits.Load(); n != 2 {
				t.Fatalf("upstream hits %d, want 2 (the expired cell is asked first)", n)
			}
			if !strings.Contains(logs.String(), "stale") {
				t.Fatalf("serving stale was not logged:\n%s", logs.String())
			}
		})
	}
}

func TestMastsStaleServedWhileBreakerOpen(t *testing.T) {
	f := newFakeWFS(t)
	var mode atomic.Int32
	var logs safeLog
	clk := newBreakerClock()
	p := staleProxy(t, f, &mode, &logs, clk)

	cell := cellQuery(cellN(0))
	first := mastsGet(p, cell, clientA)
	clk.advance(30 * 24 * time.Hour)
	mode.Store(modeFail)
	for i := 1; i <= mastsBreakerFailuresDefault; i++ {
		mastsGet(p, cellQuery(cellN(i)), clientA)
	}
	if w := mastsGet(p, cellQuery(cellN(40)), clientA); !isSourceDown(w) {
		t.Fatalf("breaker not open: %d", w.Code)
	}
	hits := f.hits.Load()
	w := mastsGet(p, cell, clientA)
	assertStale(t, w, w.Code, w.Body.String(), first.Body.String())
	if f.hits.Load() != hits {
		t.Fatal("the open breaker went upstream for a stale cell")
	}
}

// Past map.masts.maxStaleDays a cell answers as it did before #872.
func TestMastsPastMaxStaleAgeAnswersAsBefore(t *testing.T) {
	t.Run("upstream failure", func(t *testing.T) {
		f := newFakeWFS(t)
		var mode atomic.Int32
		var logs safeLog
		clk := newBreakerClock()
		p := staleProxy(t, f, &mode, &logs, clk)
		cell := cellQuery(cellN(0))
		mastsGet(p, cell, clientA)
		clk.advance(mastsMaxStaleDaysDefault * 24 * time.Hour)
		mode.Store(modeFail)
		w := mastsGet(p, cell, clientA)
		if w.Code != http.StatusBadGateway || w.Header().Get(mastsStaleHeader) != "" {
			t.Fatalf("past max stale age: %d %q, want 502 and no stale mark", w.Code, w.Header().Get(mastsStaleHeader))
		}
	})
	t.Run("breaker open", func(t *testing.T) {
		f := newFakeWFS(t)
		var mode atomic.Int32
		var logs safeLog
		clk := newBreakerClock()
		p := staleProxy(t, f, &mode, &logs, clk)
		cell := cellQuery(cellN(0))
		mastsGet(p, cell, clientA)
		clk.advance(mastsMaxStaleDaysDefault * 24 * time.Hour)
		mode.Store(modeFail)
		for i := 1; i <= mastsBreakerFailuresDefault; i++ {
			mastsGet(p, cellQuery(cellN(i)), clientA)
		}
		w := mastsGet(p, cell, clientA)
		assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "")
	})
	t.Run("configured max stale age", func(t *testing.T) {
		f := newFakeWFS(t)
		var mode atomic.Int32
		var logs safeLog
		clk := newBreakerClock()
		f.respond = switchable(&mode)
		opts := testMastsOptions(f, &logs)
		opts.rateLimitPerMin = 0
		opts.now = clk.now
		opts.maxStale = 10 * 24 * time.Hour
		p := mustMastsProxy(t, opts)
		cell := cellQuery(cellN(0))
		mastsGet(p, cell, clientA)
		mode.Store(modeFail)
		clk.advance(9 * 24 * time.Hour)
		if w := mastsGet(p, cell, clientA); w.Header().Get(mastsStaleHeader) != "1" {
			t.Fatalf("day 9 of 10: %d, want stale", w.Code)
		}
		clk.advance(24 * time.Hour)
		if w := mastsGet(p, cell, clientA); w.Code != http.StatusBadGateway {
			t.Fatalf("day 10 of 10: %d, want 502", w.Code)
		}
	})
}

// Only "source down" failures fall back: a malformed page is still 502,
// and a healthy upstream refreshes an expired cell with no stale mark.
func TestMastsStaleOnlyOnSourceDown(t *testing.T) {
	f := newFakeWFS(t)
	var mode atomic.Int32
	var logs safeLog
	clk := newBreakerClock()
	p := staleProxy(t, f, &mode, &logs, clk)
	cell := cellQuery(cellN(0))
	mastsGet(p, cell, clientA)
	clk.advance(mastsCacheTTL)

	mode.Store(modeMalformed)
	if w := mastsGet(p, cell, clientA); w.Code != http.StatusBadGateway || w.Header().Get(mastsStaleHeader) != "" {
		t.Fatalf("malformed page: %d %q, want 502 without stale", w.Code, w.Header().Get(mastsStaleHeader))
	}
	mode.Store(modeOK)
	w := mastsGet(p, cell, clientA)
	if w.Code != http.StatusOK || w.Header().Get(mastsStaleHeader) != "" || w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("refresh: %d stale=%q cache=%q", w.Code, w.Header().Get(mastsStaleHeader), w.Header().Get("X-CoreScope-Cache"))
	}
}

func TestMastsMaxStaleDaysSetting(t *testing.T) {
	if mastsMaxStaleDaysDefault != 90 {
		t.Fatalf("default %d, want 90", mastsMaxStaleDaysDefault)
	}
	if d := defaultMastsProxyOptions().maxStale; d != 90*24*time.Hour {
		t.Fatalf("default option %s, want 90 days", d)
	}
	iv := func(v int) *int { return &v }
	for _, tc := range []struct {
		in   *int
		want int
		warn bool
	}{
		{nil, 90, false}, {iv(7), 7, false}, {iv(365), 365, false},
		{iv(6), 90, true}, {iv(366), 90, true}, {iv(0), 90, true},
	} {
		var logs safeLog
		got := mastsMaxStale(MastsConfig{MaxStaleDays: tc.in}, logs.printf)
		if got != time.Duration(tc.want)*24*time.Hour {
			t.Fatalf("maxStaleDays %v: %s, want %d days", tc.in, got, tc.want)
		}
		if tc.warn != strings.Contains(logs.String(), "maxStaleDays") {
			t.Fatalf("maxStaleDays %v: warning %q", tc.in, logs.String())
		}
	}
}
