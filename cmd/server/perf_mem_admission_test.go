package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Admission control for the opt-in /api/perf?mem=1 store memory scan (#371).
// Every test drives a fake clock and parks the admitted scan in the limiter's
// acquire hook, so nothing here depends on wall-clock timing.

const perfMemTestCooldown = 5 * time.Second

// perfMemClock is a manually advanced clock for the limiter.
type perfMemClock struct {
	mu sync.Mutex
	t  time.Time
}

func newPerfMemClock() *perfMemClock {
	return &perfMemClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *perfMemClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *perfMemClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// perfMemBody decodes just the fields these tests look at.
type perfMemBody struct {
	MemoryBreakdown *StoreMemoryBreakdown `json:"memoryBreakdown"`
	Error           string                `json:"error"`
	RetryAfter      int                   `json:"retryAfter"`
}

func getPerf(t *testing.T, router *mux.Router, url string) (*httptest.ResponseRecorder, perfMemBody) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	var b perfMemBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("GET %s: decode %q: %v", url, w.Body.String(), err)
	}
	return w, b
}

// assertPerfMemBusy checks the full shed response: 429, Retry-After header
// and body agree, JSON content type, no breakdown.
func assertPerfMemBusy(t *testing.T, w *httptest.ResponseRecorder, b perfMemBody, wantRetry string) {
	t.Helper()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != wantRetry {
		t.Errorf("Retry-After = %q, want %q", got, wantRetry)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if b.Error == "" {
		t.Error("429 body has no error message")
	}
	if got := strconv.Itoa(b.RetryAfter); got != wantRetry {
		t.Errorf("body retryAfter = %s, want %s (same as the header)", got, wantRetry)
	}
	if b.MemoryBreakdown != nil {
		t.Error("a rejected request must not carry a memory breakdown")
	}
}

func assertPerfMemServed(t *testing.T, w *httptest.ResponseRecorder, b perfMemBody) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if b.MemoryBreakdown == nil {
		t.Fatal("admitted mem=1 request returned no memoryBreakdown")
	}
}

// parkedScan starts one mem=1 request and parks it in the limiter's acquire
// hook (after admission, before any store access). unpark lets it finish and
// returns its response.
type parkedScan struct {
	unpark func() (*httptest.ResponseRecorder, perfMemBody)
}

func startParkedScan(t *testing.T, srv *Server, router *mux.Router, clk *perfMemClock, scanTime time.Duration) parkedScan {
	t.Helper()
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	srv.perfMem.configure(perfMemTestCooldown, clk.now, func() {
		once.Do(func() {
			close(entered)
			<-gate
			clk.advance(scanTime) // the scan "takes" scanTime on the fake clock
		})
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/perf?mem=1", nil))
		done <- w
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first mem=1 request was never admitted")
	}
	return parkedScan{unpark: func() (*httptest.ResponseRecorder, perfMemBody) {
		close(gate)
		select {
		case w := <-done:
			var b perfMemBody
			if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
				t.Fatalf("decode parked response %q: %v", w.Body.String(), err)
			}
			return w, b
		case <-time.After(10 * time.Second):
			t.Fatal("parked mem=1 request never finished")
		}
		return nil, perfMemBody{}
	}}
}

// While one scan is in flight, a concurrent burst of mem=1 requests is shed
// with 429 + Retry-After — and shed before the packet-store lock: the test
// holds the store's write lock for the whole burst, so any rejected request
// that reached RLock (or the traversal) would block and time out.
func TestPerfMemAdmission_ConcurrentRejectedBeforeStoreLock(t *testing.T) {
	srv, router := setupTestServer(t)
	clk := newPerfMemClock()
	scan := startParkedScan(t, srv, router, clk, 0)

	const burst = 16
	srv.store.mu.Lock()
	type result struct {
		w *httptest.ResponseRecorder
	}
	results := make(chan result, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/perf?mem=1", nil))
			results <- result{w}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		srv.store.mu.Unlock()
		t.Fatal("rejected mem=1 requests blocked on the packet-store lock")
	}
	srv.store.mu.Unlock()
	close(results)
	for r := range results {
		var b perfMemBody
		if err := json.Unmarshal(r.w.Body.Bytes(), &b); err != nil {
			t.Fatalf("decode %q: %v", r.w.Body.String(), err)
		}
		// While a scan is active the remaining time is unknown; the limiter
		// advertises the cooldown, the earliest a new scan could be admitted.
		assertPerfMemBusy(t, r.w, b, "5")
	}
	if got := srv.perfMem.rejectedCount(); got != burst {
		t.Errorf("rejected = %d, want %d", got, burst)
	}

	w, b := scan.unpark()
	assertPerfMemServed(t, w, b)
	if got := srv.perfMem.acquiredCount(); got != 1 {
		t.Errorf("acquired = %d, want exactly 1 scan", got)
	}
}

// An immediate repeat after a completed scan is shed for the cooldown;
// Retry-After rounds the remaining time up, and the request at exactly the
// end of the cooldown is admitted again (release happened).
func TestPerfMemAdmission_RepeatCooldownBoundary(t *testing.T) {
	srv, router := setupTestServer(t)
	clk := newPerfMemClock()
	srv.perfMem.configure(perfMemTestCooldown, clk.now, nil)

	w, b := getPerf(t, router, "/api/perf?mem=1")
	assertPerfMemServed(t, w, b)

	w, b = getPerf(t, router, "/api/perf?mem=1")
	assertPerfMemBusy(t, w, b, "5")

	clk.advance(perfMemTestCooldown - 1500*time.Millisecond) // 1.5 s left
	w, b = getPerf(t, router, "/api/perf?mem=1")
	assertPerfMemBusy(t, w, b, "2")

	clk.advance(1500*time.Millisecond - time.Nanosecond) // 1 ns left
	w, b = getPerf(t, router, "/api/perf?mem=1")
	assertPerfMemBusy(t, w, b, "1")

	clk.advance(time.Nanosecond) // cooldown boundary
	w, b = getPerf(t, router, "/api/perf?mem=1")
	assertPerfMemServed(t, w, b)

	if got := srv.perfMem.acquiredCount(); got != 2 {
		t.Errorf("acquired = %d, want 2", got)
	}
	if got := srv.perfMem.rejectedCount(); got != 3 {
		t.Errorf("rejected = %d, want 3", got)
	}
}

// The cooldown runs from scan completion, not from admission: a scan that
// takes 30 s on the fake clock still leaves the full cooldown afterwards.
func TestPerfMemAdmission_CooldownStartsAtCompletion(t *testing.T) {
	srv, router := setupTestServer(t)
	clk := newPerfMemClock()
	scan := startParkedScan(t, srv, router, clk, 30*time.Second)
	w, b := scan.unpark()
	assertPerfMemServed(t, w, b)

	clk.advance(perfMemTestCooldown - time.Nanosecond)
	w, b = getPerf(t, router, "/api/perf?mem=1")
	assertPerfMemBusy(t, w, b, "1")

	clk.advance(time.Nanosecond)
	w, b = getPerf(t, router, "/api/perf?mem=1")
	assertPerfMemServed(t, w, b)
}

// blockingWriter stalls the first Write until released, like a client that
// stops reading the response.
type blockingWriter struct {
	*httptest.ResponseRecorder
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.writing)
		<-b.release
	})
	return b.ResponseRecorder.Write(p)
}

// The slot is released when the scan completes, not when the response has been
// written: a client that stalls reading its body cannot hold the scan slot.
func TestPerfMemAdmission_ReleasedBeforeResponseWrite(t *testing.T) {
	srv, router := setupTestServer(t)
	clk := newPerfMemClock()
	srv.perfMem.configure(perfMemTestCooldown, clk.now, nil)

	bw := &blockingWriter{
		ResponseRecorder: httptest.NewRecorder(),
		writing:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(bw, httptest.NewRequest("GET", "/api/perf?mem=1", nil))
		close(done)
	}()
	select {
	case <-bw.writing:
	case <-time.After(5 * time.Second):
		t.Fatal("slow client never reached the response write")
	}
	clk.advance(perfMemTestCooldown)
	w, b := getPerf(t, router, "/api/perf?mem=1")
	close(bw.release)
	<-done
	assertPerfMemServed(t, w, b)
}

// Ordinary /api/perf (no mem, mem=0, mem=true) is never gated — not during an
// active scan and not during the cooldown — and carries no breakdown.
func TestPerfMemAdmission_OrdinaryPerfUnaffected(t *testing.T) {
	srv, router := setupTestServer(t)
	clk := newPerfMemClock()
	plain := func(phase string) {
		t.Helper()
		for _, u := range []string{"/api/perf", "/api/perf?mem=0", "/api/perf?mem=true"} {
			w, b := getPerf(t, router, u)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: GET %s = %d, want 200", phase, u, w.Code)
			}
			if b.MemoryBreakdown != nil {
				t.Errorf("%s: GET %s carried a memory breakdown", phase, u)
			}
			if ra := w.Header().Get("Retry-After"); ra != "" {
				t.Errorf("%s: GET %s set Retry-After %q", phase, u, ra)
			}
		}
	}
	scan := startParkedScan(t, srv, router, clk, 0)
	plain("active scan")
	w, b := scan.unpark()
	assertPerfMemServed(t, w, b)
	plain("cooldown")
	if got := srv.perfMem.rejectedCount(); got != 0 {
		t.Errorf("ordinary /api/perf counted %d rejections", got)
	}
}

// Without a packet store mem=1 has nothing to scan: it keeps returning 200
// with no breakdown and never consults the limiter, even while it is busy.
func TestPerfMemAdmission_StorelessUnaffected(t *testing.T) {
	srv, router := setupNoStoreServer(t)
	clk := newPerfMemClock()
	srv.perfMem.configure(perfMemTestCooldown, clk.now, nil)
	release, _, ok := srv.perfMem.acquire()
	if !ok {
		t.Fatal("fresh limiter refused the first acquire")
	}
	defer release()

	for i := 0; i < 3; i++ {
		w, b := getPerf(t, router, "/api/perf?mem=1")
		if w.Code != http.StatusOK {
			t.Fatalf("storeless mem=1 = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if b.MemoryBreakdown != nil {
			t.Error("storeless mem=1 carried a memory breakdown")
		}
	}
	if got := srv.perfMem.rejectedCount(); got != 0 {
		t.Errorf("storeless mem=1 counted %d rejections", got)
	}
}

// A release func is single-use: calling it again (the handler releases right
// after the scan and again from its defer) must not free a later holder's
// slot or restart the cooldown.
func TestPerfMemLimiter_StaleReleaseIsNoop(t *testing.T) {
	var l perfMemLimiter
	clk := newPerfMemClock()
	l.configure(perfMemTestCooldown, clk.now, nil)

	relA, _, ok := l.acquire()
	if !ok {
		t.Fatal("first acquire refused")
	}
	relA()
	clk.advance(perfMemTestCooldown)
	relB, _, ok := l.acquire()
	if !ok {
		t.Fatal("acquire after the cooldown refused")
	}
	relA() // stale: must not touch B's scan
	if _, wait, ok := l.acquire(); ok {
		t.Fatal("stale release freed the active scan's slot")
	} else if wait != perfMemTestCooldown {
		t.Errorf("busy wait = %v, want the cooldown %v", wait, perfMemTestCooldown)
	}
	relB()
	relB()
}

// The zero value works (production Servers never call configure) and uses
// the package default cooldown.
func TestPerfMemLimiter_ZeroValueDefaults(t *testing.T) {
	var l perfMemLimiter
	rel, _, ok := l.acquire()
	if !ok {
		t.Fatal("zero-value limiter refused the first acquire")
	}
	rel()
	_, wait, ok := l.acquire()
	if ok {
		t.Fatal("zero-value limiter admitted a repeat inside the cooldown")
	}
	if wait <= 0 || wait > perfMemScanCooldown {
		t.Errorf("wait = %v, want within (0, %v]", wait, perfMemScanCooldown)
	}
}
