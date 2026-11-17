package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/mastedb"
	"github.com/meshcore-analyzer/mastedb/mastedbtest"
)

// #872 — the circuit breaker in front of Mastedatabasen. Every test talks
// to the httptest fake of masts_proxy_724_test.go; nothing here contacts
// the real service. Time is a fake clock (opts.now), so a cool-down is a
// clock step, not a sleep.

type breakerClock struct {
	mu sync.Mutex
	t  time.Time
}

func newBreakerClock() *breakerClock {
	return &breakerClock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
}

func (c *breakerClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *breakerClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// breakerProxy is a proxy on the fake with a fake clock and no per-IP
// limit (each test sets what it needs).
func breakerProxy(t *testing.T, f *fakeWFS, logs *safeLog, clk *breakerClock) *mastsProxy {
	t.Helper()
	opts := testMastsOptions(f, logs)
	opts.rateLimitPerMin = 0
	opts.now = clk.now
	return mustMastsProxy(t, opts)
}

// cellN is the i-th distinct valid cell, so no two requests share a cache
// entry or a flight.
func cellN(i int) mastedb.Cell { return mastedb.Cell{E: 263 + i, N: 3127} }

func fail500(w http.ResponseWriter, _ mastedb.Cell) { w.WriteHeader(http.StatusInternalServerError) }

func isSourceDown(w interface {
	Header() http.Header
}) bool {
	return w.Header().Get(mastsSourceHeader) == "down"
}

// assertSourceDown checks the distinct "source unavailable" answer.
func assertSourceDown(t *testing.T, code int, h http.Header, body string, wantRetry string) {
	t.Helper()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	if h.Get(mastsSourceHeader) != "down" {
		t.Fatalf("%s = %q, want down", mastsSourceHeader, h.Get(mastsSourceHeader))
	}
	if !strings.Contains(body, `"`+errMastsSourceDown.Error()+`"`) {
		t.Fatalf("body %q is not the source-down error", body)
	}
	if wantRetry != "" && h.Get("Retry-After") != wantRetry {
		t.Fatalf("Retry-After %q, want %q", h.Get("Retry-After"), wantRetry)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", h.Get("Cache-Control"))
	}
}

// Requirement 1: N consecutive 5xx open the breaker; the next uncached
// cell answers at once without an upstream call, with Retry-After = the
// remaining cool-down, and one log line says so.
func TestMastsBreakerOpensAfterConsecutive5xx(t *testing.T) {
	f := newFakeWFS(t)
	f.respond = fail500
	var logs safeLog
	clk := newBreakerClock()
	p := breakerProxy(t, f, &logs, clk)

	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		if w := mastsGet(p, cellQuery(cellN(i)), clientA); w.Code != http.StatusBadGateway {
			t.Fatalf("failure %d: status %d, want 502", i, w.Code)
		}
	}
	if n := f.hits.Load(); n != int32(mastsBreakerFailuresDefault) {
		t.Fatalf("upstream hits %d, want %d", n, mastsBreakerFailuresDefault)
	}
	clk.advance(10 * time.Second)
	for i := 0; i < 20; i++ {
		start := time.Now()
		w := mastsGet(p, cellQuery(cellN(10+i)), clientA)
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("open breaker answered after %s, want < 100ms", d)
		}
		assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "290")
	}
	if n := f.hits.Load(); n != int32(mastsBreakerFailuresDefault) {
		t.Fatalf("open breaker reached upstream: %d hits", n)
	}
	if c := strings.Count(logs.String(), "[masts] circuit open"); c != 1 {
		t.Fatalf("%d open log lines, want 1:\n%s", c, logs.String())
	}
}

// Timeouts count like 5xx.
func TestMastsBreakerOpensAfterTimeouts(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{}) // never answers
	t.Cleanup(func() { close(f.gate) })
	var logs safeLog
	clk := newBreakerClock()
	opts := testMastsOptions(f, &logs)
	opts.rateLimitPerMin = 0
	opts.now = clk.now
	opts.client.Timeout = 30 * time.Millisecond
	p := mustMastsProxy(t, opts)

	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		if w := mastsGet(p, cellQuery(cellN(i)), clientA); w.Code != http.StatusGatewayTimeout {
			t.Fatalf("timeout %d: status %d, want 504", i, w.Code)
		}
	}
	w := mastsGet(p, cellQuery(cellN(9)), clientA)
	assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "300")
	if n := f.hits.Load(); n != int32(mastsBreakerFailuresDefault) {
		t.Fatalf("upstream hits %d, want %d", n, mastsBreakerFailuresDefault)
	}
}

// Only consecutive failures count: a success resets the streak, and an
// answer that is not "down" (a 4xx, a malformed page) neither counts nor
// resets.
func TestMastsBreakerCountsOnlyConsecutiveDownFailures(t *testing.T) {
	f := newFakeWFS(t)
	var mu sync.Mutex
	mode := "500"
	f.respond = func(w http.ResponseWriter, c mastedb.Cell) {
		mu.Lock()
		m := mode
		mu.Unlock()
		switch m {
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		case "404":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
			w.Write(okGML(c))
		}
	}
	set := func(m string) { mu.Lock(); mode = m; mu.Unlock() }
	var logs safeLog
	clk := newBreakerClock()
	p := breakerProxy(t, f, &logs, clk)

	i := 0
	get := func() int { i++; return mastsGet(p, cellQuery(cellN(i)), clientA).Code }
	for k := 0; k < mastsBreakerFailuresDefault-1; k++ {
		get()
	}
	set("ok")
	if c := get(); c != http.StatusOK {
		t.Fatalf("success: %d", c)
	}
	set("500")
	for k := 0; k < mastsBreakerFailuresDefault-1; k++ {
		if c := get(); c != http.StatusBadGateway {
			t.Fatalf("after a success the breaker opened early: %d", c)
		}
	}
	set("404")
	for k := 0; k < 5; k++ {
		if c := get(); c != http.StatusBadGateway {
			t.Fatalf("a 4xx counted as down: %d", c)
		}
	}
	set("500")
	if c := get(); c != http.StatusBadGateway {
		t.Fatalf("the Nth consecutive 5xx: %d", c)
	}
	if c := get(); c != http.StatusServiceUnavailable {
		t.Fatalf("breaker not open after %d consecutive 5xx (4xx between): %d", mastsBreakerFailuresDefault, c)
	}
}

// Cached cells are served while the breaker is open.
func TestMastsBreakerStillServesCachedCells(t *testing.T) {
	f := newFakeWFS(t)
	var mu sync.Mutex
	down := false
	f.respond = func(w http.ResponseWriter, c mastedb.Cell) {
		mu.Lock()
		d := down
		mu.Unlock()
		if d {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
		w.Write(okGML(c))
	}
	var logs safeLog
	clk := newBreakerClock()
	p := breakerProxy(t, f, &logs, clk)

	cached := cellN(40)
	if w := mastsGet(p, cellQuery(cached), clientA); w.Code != http.StatusOK {
		t.Fatalf("warm: %d", w.Code)
	}
	mu.Lock()
	down = true
	mu.Unlock()
	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		mastsGet(p, cellQuery(cellN(i)), clientA)
	}
	if w := mastsGet(p, cellQuery(cellN(50)), clientA); !isSourceDown(w) {
		t.Fatalf("breaker not open: %d", w.Code)
	}
	hits := f.hits.Load()
	w := mastsGet(p, cellQuery(cached), clientA)
	if w.Code != http.StatusOK || w.Header().Get("X-CoreScope-Cache") != "hit" {
		t.Fatalf("cached cell while open: %d %q", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
	if f.hits.Load() != hits {
		t.Fatal("cached cell reached upstream")
	}
}

// After the cool-down one probe goes upstream; other requests meanwhile
// are refused at once. A failed probe re-opens for a full cool-down; a
// successful one closes the breaker.
func TestMastsBreakerProbeAfterCooldown(t *testing.T) {
	f := newFakeWFS(t)
	var mu sync.Mutex
	down := true
	var gate chan struct{}
	f.respond = func(w http.ResponseWriter, c mastedb.Cell) {
		mu.Lock()
		d, g := down, gate
		mu.Unlock()
		if g != nil {
			<-g
		}
		if d {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
		w.Write(okGML(c))
	}
	var logs safeLog
	clk := newBreakerClock()
	p := breakerProxy(t, f, &logs, clk)

	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		mastsGet(p, cellQuery(cellN(i)), clientA)
	}
	// Just before the end of the cool-down: still refused.
	clk.advance(mastsBreakerCooldownDefault - time.Second)
	w := mastsGet(p, cellQuery(cellN(20)), clientA)
	assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "1")
	hits := f.hits.Load()

	// Cool-down over: one probe, and it fails → open again for a full
	// cool-down.
	clk.advance(time.Second)
	if w := mastsGet(p, cellQuery(cellN(21)), clientA); w.Code != http.StatusBadGateway {
		t.Fatalf("probe: status %d, want 502 (it should go upstream)", w.Code)
	}
	if f.hits.Load() != hits+1 {
		t.Fatalf("probe hits %d, want 1", f.hits.Load()-hits)
	}
	w = mastsGet(p, cellQuery(cellN(22)), clientA)
	assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "300")
	if f.hits.Load() != hits+1 {
		t.Fatal("re-opened breaker reached upstream")
	}

	// Next cool-down: the probe is held upstream; concurrent requests are
	// refused at once without an upstream call.
	clk.advance(mastsBreakerCooldownDefault)
	mu.Lock()
	down = false
	gate = make(chan struct{})
	g := gate
	mu.Unlock()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(g) }) })
	probeDone := make(chan int, 1)
	go func() { probeDone <- mastsGet(p, cellQuery(cellN(23)), clientA).Code }()
	waitUntil872(t, func() bool { return f.hits.Load() == hits+2 })
	for i := 0; i < 5; i++ {
		start := time.Now()
		// Bounded: a second probe would wait on the held gate (VM3 7).
		w := getWithin(t, p, cellN(30+i), 2*time.Second)
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("request during the probe waited %s", d)
		}
		if !isSourceDown(w) {
			t.Fatalf("request during the probe: %d, want source down", w.Code)
		}
		if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
			t.Fatalf("request during the probe: Retry-After %q", ra)
		}
	}
	if f.hits.Load() != hits+2 {
		t.Fatalf("a second probe went upstream: %d", f.hits.Load()-hits)
	}
	mu.Lock()
	gate = nil
	mu.Unlock()
	release.Do(func() { close(g) })
	select {
	case c := <-probeDone:
		if c != http.StatusOK {
			t.Fatalf("successful probe: %d", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe did not answer")
	}
	// Closed: uncached cells go upstream again.
	for i := 0; i < 3; i++ {
		if w := mastsGet(p, cellQuery(cellN(40+i)), clientA); w.Code != http.StatusOK {
			t.Fatalf("after a successful probe: %d", w.Code)
		}
	}
	out := logs.String()
	if c := strings.Count(out, "[masts] circuit open"); c != 2 {
		t.Fatalf("%d open lines, want 2 (open, re-open):\n%s", c, out)
	}
	if c := strings.Count(out, "[masts] circuit closed"); c != 1 {
		t.Fatalf("%d closed lines, want 1:\n%s", c, out)
	}
}

// An open breaker is not charged to the client's budget: refusals never
// turn into 429.
func TestMastsBreakerRefusalIsNotCharged(t *testing.T) {
	f := newFakeWFS(t)
	f.respond = fail500
	var logs safeLog
	clk := newBreakerClock()
	opts := testMastsOptions(f, &logs)
	opts.now = clk.now
	opts.rateLimitPerMin = mastsBreakerFailuresDefault + 1
	p := mustMastsProxy(t, opts)
	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		mastsGet(p, cellQuery(cellN(i)), clientA)
	}
	for i := 0; i < 10; i++ {
		if w := mastsGet(p, cellQuery(cellN(10+i)), clientA); !isSourceDown(w) {
			t.Fatalf("refusal %d: %d", i, w.Code)
		}
	}
}

// The breaker's state is a few scalars: nothing grows with refusals.
func TestMastsBreakerStateIsBounded(t *testing.T) {
	b := newMastsBreaker(3, time.Minute)
	now := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		gen, _ := b.start(false)
		b.record(now, gen, mastsOutcomeDown)
	}
	for i := 0; i < 100000; i++ {
		if _, _, ok := b.admit(now); ok {
			t.Fatal("admitted while open")
		}
	}
	if b.fails > 3 {
		t.Fatalf("failure count grew to %d", b.fails)
	}
}

// Requirement 3: map.masts.breakerFailures and breakerCooldownSec are
// bounded; out of range warns and takes the default.
func TestMastsBreakerConfigBounds(t *testing.T) {
	ip := func(v int) *int { return &v }
	cases := []struct {
		name         string
		cfg          MastsConfig
		wantN        int
		wantCooldown time.Duration
		warn         bool
	}{
		{"defaults", MastsConfig{}, 3, 5 * time.Minute, false},
		{"set", MastsConfig{BreakerFailures: ip(5), BreakerCooldownSec: ip(120)}, 5, 2 * time.Minute, false},
		{"low edge", MastsConfig{BreakerFailures: ip(1), BreakerCooldownSec: ip(30)}, 1, 30 * time.Second, false},
		{"high edge", MastsConfig{BreakerFailures: ip(20), BreakerCooldownSec: ip(3600)}, 20, time.Hour, false},
		{"failures 0", MastsConfig{BreakerFailures: ip(0)}, 3, 5 * time.Minute, true},
		{"failures 21", MastsConfig{BreakerFailures: ip(21)}, 3, 5 * time.Minute, true},
		{"cooldown 29", MastsConfig{BreakerCooldownSec: ip(29)}, 3, 5 * time.Minute, true},
		{"cooldown 3601", MastsConfig{BreakerCooldownSec: ip(3601)}, 3, 5 * time.Minute, true},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		lg := log.New(&buf, "", 0)
		n, cd := mastsBreakerSettings(c.cfg, lg.Printf)
		if n != c.wantN || cd != c.wantCooldown {
			t.Errorf("%s: got %d/%s, want %d/%s", c.name, n, cd, c.wantN, c.wantCooldown)
		}
		if got := strings.Contains(buf.String(), "map.masts.breaker"); got != c.warn {
			t.Errorf("%s: warned=%v, want %v (%q)", c.name, got, c.warn, buf.String())
		}
	}
}

// The config reaches the proxy: breakerFailures 1 opens on the first 5xx.
func TestMastsBreakerConfigReachesProxy(t *testing.T) {
	cfg := writeMastsConfig(t, `{"map":{"masts":{"enabled":true,"breakerFailures":1,"breakerCooldownSec":60}}}`)
	p := newMastsProxyFromConfig(cfg)
	if p == nil {
		t.Fatal("nil proxy")
	}
	if p.breaker.threshold != 1 || p.breaker.cooldown != time.Minute {
		t.Fatalf("breaker %d/%s, want 1/1m", p.breaker.threshold, p.breaker.cooldown)
	}
}

func TestMastsBreakerConfigExampleDocumentsDefaults(t *testing.T) {
	data := readExampleConfig(t)
	var doc struct {
		Map struct {
			Masts struct {
				BreakerFailures    *int   `json:"breakerFailures"`
				BreakerCooldownSec *int   `json:"breakerCooldownSec"`
				C1                 string `json:"_comment_breakerFailures"`
				C2                 string `json:"_comment_breakerCooldownSec"`
			} `json:"masts"`
		} `json:"map"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	m := doc.Map.Masts
	if m.BreakerFailures == nil || *m.BreakerFailures != mastsBreakerFailuresDefault ||
		m.BreakerCooldownSec == nil || *m.BreakerCooldownSec != int(mastsBreakerCooldownDefault/time.Second) {
		t.Fatalf("example breaker values %v/%v are not the defaults", m.BreakerFailures, m.BreakerCooldownSec)
	}
	if m.C1 == "" || m.C2 == "" {
		t.Fatal("breaker keys undocumented")
	}
}

func okGML(c mastedb.Cell) []byte { return mastedbtest.GML(c.E, c.N, 2, "2") }

func waitUntil872(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

func readExampleConfig(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A fetch admitted before the breaker opened, still waiting for one of the
// upstream slots, does not go upstream once it opens.
func TestMastsBreakerStopsQueuedFetches(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	f.respond = fail500
	var logs safeLog
	clk := newBreakerClock()
	opts := testMastsOptions(f, &logs)
	opts.rateLimitPerMin = 0
	opts.now = clk.now
	// One failure opens it: a fetch records its outcome before it frees
	// its slot, so the queued fetch always sees the breaker open.
	opts.breakerFailures = 1
	p := mustMastsProxy(t, opts)

	codes := make(chan int, testMastsSlots+1)
	for i := 0; i < testMastsSlots; i++ {
		go func(i int) { codes <- mastsGet(p, cellQuery(cellN(i)), clientA).Code }(i)
	}
	waitUntil872(t, func() bool { return f.inFlight.Load() == testMastsSlots })
	queued := make(chan *httptest.ResponseRecorder, 1)
	go func() { queued <- mastsGet(p, cellQuery(cellN(9)), clientA) }()
	time.Sleep(50 * time.Millisecond) // the third fetch is waiting for a slot
	close(f.gate)
	for i := 0; i < testMastsSlots; i++ {
		if c := <-codes; c != http.StatusBadGateway {
			t.Fatalf("held fetch: %d", c)
		}
	}
	w := <-queued
	// It waits out the remaining cool-down, not the busy retry (VM3 3).
	assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "300")
	if n := f.hits.Load(); n != int32(testMastsSlots) {
		t.Fatalf("upstream hits %d, want %d (the queued fetch went upstream)", n, testMastsSlots)
	}
}

// getWithin runs one request and fails the test if it has not answered
// within d, so a breaker regression fails fast instead of hanging until
// go test's own timeout (#878 review VM3 7).
func getWithin(t *testing.T, p *mastsProxy, c mastedb.Cell, d time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- mastsGet(p, cellQuery(c), clientA) }()
	select {
	case w := <-done:
		return w
	case <-time.After(d):
		t.Fatalf("request for cell %v did not answer within %s", c, d)
		return nil
	}
}

// heldCellUpstream answers the cells in held only once their channel is
// closed, with the answer given; every other cell gets a 500.
func heldCellUpstream(held map[mastedb.Cell]chan struct{}, answer func(http.ResponseWriter, mastedb.Cell)) func(http.ResponseWriter, mastedb.Cell) {
	return func(w http.ResponseWriter, c mastedb.Cell) {
		if ch, ok := held[c]; ok {
			<-ch
			answer(w, c)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func answerOK(w http.ResponseWriter, c mastedb.Cell) {
	w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
	w.Write(okGML(c))
}

// #878 external review finding 5: a success from a fetch that went
// upstream before the breaker opened does not close it in the middle of
// its cool-down. The next cold cell still gets the source-down 503 and
// upstream is not asked.
func TestMastsBreakerPreOpenSuccessDoesNotClose(t *testing.T) {
	f := newFakeWFS(t)
	early := cellN(100)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.respond = heldCellUpstream(map[mastedb.Cell]chan struct{}{early: release}, answerOK)
	var logs safeLog
	clk := newBreakerClock()
	p := breakerProxy(t, f, &logs, clk)

	earlyDone := make(chan int, 1)
	go func() { earlyDone <- mastsGet(p, cellQuery(early), clientA).Code }()
	waitUntil872(t, func() bool { return f.hits.Load() == 1 })
	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		if w := getWithin(t, p, cellN(i), 5*time.Second); w.Code != http.StatusBadGateway {
			t.Fatalf("failure %d: status %d, want 502", i, w.Code)
		}
	}
	clk.advance(10 * time.Second)
	hits := f.hits.Load()

	once.Do(func() { close(release) })
	select {
	case c := <-earlyDone:
		if c != http.StatusOK {
			t.Fatalf("pre-open fetch: %d, want 200", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pre-open fetch did not answer")
	}

	w := getWithin(t, p, cellN(50), 5*time.Second)
	assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "290")
	if n := f.hits.Load(); n != hits {
		t.Fatalf("a pre-open success closed the breaker: %d extra upstream hit(s)", n-hits)
	}
	if out := logs.String(); strings.Contains(out, "[masts] circuit closed") {
		t.Fatalf("a pre-open success was logged as a closing probe:\n%s", out)
	}
}

// #878 review VM3 5 (mutant M1): a failure from a fetch that went upstream
// before the breaker opened neither extends the cool-down (it ends when
// it would have) nor, arriving after the cool-down, re-opens it as if a
// probe had failed.
func TestMastsBreakerPreOpenFailureDoesNotExtend(t *testing.T) {
	f := newFakeWFS(t)
	during, after := cellN(100), cellN(101)
	relDuring, relAfter := make(chan struct{}), make(chan struct{})
	var o1, o2 sync.Once
	t.Cleanup(func() { o1.Do(func() { close(relDuring) }); o2.Do(func() { close(relAfter) }) })
	f.respond = heldCellUpstream(map[mastedb.Cell]chan struct{}{during: relDuring, after: relAfter}, fail500)
	var logs safeLog
	clk := newBreakerClock()
	opts := testMastsOptions(f, &logs)
	opts.rateLimitPerMin = 0
	opts.now = clk.now
	opts.upstreamSlots = 4 // two held fetches and room for the failures
	opts.maxInFlight = 4
	p := mustMastsProxy(t, opts)

	held := make(chan int, 2)
	for _, c := range []mastedb.Cell{during, after} {
		go func(c mastedb.Cell) { held <- mastsGet(p, cellQuery(c), clientA).Code }(c)
	}
	waitUntil872(t, func() bool { return f.hits.Load() == 2 })
	for i := 0; i < mastsBreakerFailuresDefault; i++ {
		getWithin(t, p, cellN(i), 5*time.Second)
	}
	waitHeld := func() {
		t.Helper()
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			t.Fatal("held pre-open fetch did not answer")
		}
	}

	// A late failure inside the cool-down changes nothing.
	clk.advance(mastsBreakerCooldownDefault / 2)
	o1.Do(func() { close(relDuring) })
	waitHeld()
	w := getWithin(t, p, cellN(50), 5*time.Second)
	assertSourceDown(t, w.Code, w.Header(), w.Body.String(), "150")

	// Another one after the cool-down does not re-open it.
	clk.advance(mastsBreakerCooldownDefault / 2)
	o2.Do(func() { close(relAfter) })
	waitHeld()
	hits := f.hits.Load()
	if w := getWithin(t, p, cellN(51), 5*time.Second); w.Code != http.StatusBadGateway {
		t.Fatalf("after the original cool-down: %d (source down %v), want the probe's 502", w.Code, isSourceDown(w))
	}
	if n := f.hits.Load(); n != hits+1 {
		t.Fatalf("probe hits %d, want 1", n-hits)
	}
	if c := strings.Count(logs.String(), "[masts] circuit open again"); c != 1 {
		t.Fatalf("%d re-open lines, want 1 (the probe's):\n%s", c, logs.String())
	}
}

// #878 review VM3 6 (mutant M4): clients that leave cannot open the
// breaker. Their fetch ends "canceled", which says nothing about upstream.
func TestMastsBreakerClientCancelDoesNotOpen(t *testing.T) {
	for _, err := range []error{dhm.FetchError{Reason: "canceled"}, context.Canceled} {
		if o := mastsOutcomeOf(err); o != mastsOutcomeNeutral {
			t.Fatalf("mastsOutcomeOf(%v) = %d, want neutral", err, o)
		}
	}
	f := newFakeWFS(t)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	held := map[mastedb.Cell]chan struct{}{}
	for i := 0; i < mastsBreakerFailuresDefault+2; i++ {
		held[cellN(i)] = hang
	}
	f.respond = func(w http.ResponseWriter, c mastedb.Cell) {
		if ch, ok := held[c]; ok {
			<-ch
		}
		answerOK(w, c)
	}
	var logs safeLog
	clk := newBreakerClock()
	p := breakerProxy(t, f, &logs, clk)

	for i := 0; i < mastsBreakerFailuresDefault+2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		r := httptest.NewRequest("GET", "/api/masts?"+cellQuery(cellN(i)), nil).WithContext(ctx)
		r.RemoteAddr = clientA
		done := make(chan struct{})
		go func() { p.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
		waitUntil872(t, func() bool { return f.hits.Load() == int32(i+1) })
		cancel()
		<-done
		// The fetch records its outcome before it frees its slot.
		waitUntil872(t, func() bool { return len(p.upstream) == 0 })
	}
	w := getWithin(t, p, cellN(40), 5*time.Second)
	if w.Code != http.StatusOK {
		t.Fatalf("after %d clients left: %d (source down %v), want 200", mastsBreakerFailuresDefault+2, w.Code, isSourceDown(w))
	}
	if strings.Contains(logs.String(), "[masts] circuit open") {
		t.Fatalf("clients that left opened the breaker:\n%s", logs.String())
	}
}

// A probe whose client left frees the probe for the next request; if the
// second probe closes the breaker, the first one's late failure belongs to
// the half-open generation and does not count against the closed breaker.
func TestMastsBreakerAbandonedProbeOutcomeIsIgnored(t *testing.T) {
	b := newMastsBreaker(1, time.Minute)
	now := time.Unix(0, 0)
	gen, _ := b.start(false)
	if b.record(now, gen, mastsOutcomeDown) != "open" {
		t.Fatal("not open")
	}
	now = now.Add(time.Minute)
	if probe, _, ok := b.admit(now); !probe || !ok {
		t.Fatal("first probe not admitted")
	}
	first, _ := b.start(true)
	b.endProbe() // its client left; the fetch runs on
	if probe, _, ok := b.admit(now); !probe || !ok {
		t.Fatal("second probe not admitted")
	}
	second, _ := b.start(true)
	if s := b.record(now, second, mastsOutcomeOK); s != "closed" {
		t.Fatalf("second probe's success: %q, want closed", s)
	}
	b.endProbe()
	if s := b.record(now, first, mastsOutcomeDown); s != "" {
		t.Fatalf("the abandoned probe's late failure: %q, want no change", s)
	}
	if _, _, ok := b.admit(now); !ok {
		t.Fatal("closed breaker refused a fetch")
	}
}
