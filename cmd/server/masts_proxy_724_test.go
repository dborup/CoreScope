package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/mastedb"
	"github.com/meshcore-analyzer/mastedb/mastedbtest"
)

// #724 M2 — GET /api/masts?cell=E,N. Every test talks to an httptest fake
// of the Mastedatabasen WFS; nothing here contacts the real service.

// testMastsCell is a valid cell inside the Danish extent.
var testMastsCell = mastedb.Cell{E: 263, N: 3127}

// fakeWFS is a fake Mastedatabasen WFS. By default it answers every
// GetFeature with mastedbtest.GML for the requested cell (rows rows).
type fakeWFS struct {
	srv      *httptest.Server
	hits     atomic.Int32
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	rows     int
	// gate, when set, holds every request until it is closed.
	gate chan struct{}
	// respond, when set, replaces the default answer.
	respond func(w http.ResponseWriter, cell mastedb.Cell)
}

func newFakeWFS(t *testing.T) *fakeWFS {
	t.Helper()
	f := &fakeWFS{rows: 12}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		n := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			m := f.maxSeen.Load()
			if n <= m || f.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		if f.gate != nil {
			select {
			case <-f.gate:
			case <-r.Context().Done():
				return
			}
		}
		cell, ok := cellOfBBox(r.URL.Query().Get("BBOX"))
		if !ok {
			t.Errorf("fake WFS: unexpected BBOX %q", r.URL.Query().Get("BBOX"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.respond != nil {
			f.respond(w, cell)
			return
		}
		w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
		w.Write(mastedbtest.GML(cell.E, cell.N, f.rows, strconv.Itoa(f.rows)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// cellOfBBox inverts mastedb's "minE,minN,maxE,maxN,EPSG:25832".
func cellOfBBox(s string) (mastedb.Cell, bool) {
	p := strings.Split(s, ",")
	if len(p) != 5 || p[4] != mastedb.CRS {
		return mastedb.Cell{}, false
	}
	var v [4]int
	for i := range v {
		n, err := strconv.Atoi(p[i])
		if err != nil {
			return mastedb.Cell{}, false
		}
		v[i] = n
	}
	c := mastedb.Cell{E: v[0] / mastedb.CellSizeM, N: v[1] / mastedb.CellSizeM}
	return c, c.BBox() == [4]float64{float64(v[0]), float64(v[1]), float64(v[2]), float64(v[3])}
}

// testMastsOptions are production options pointed at the fake, with logs
// captured instead of printed.
func testMastsOptions(f *fakeWFS, logs *safeLog) mastsProxyOptions {
	opts := defaultMastsProxyOptions()
	opts.client.BaseURL = f.srv.URL
	opts.client.Timeout = 5 * time.Second
	opts.logf = logs.printf
	return opts
}

type safeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *safeLog) printf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func mustMastsProxy(t *testing.T, opts mastsProxyOptions) *mastsProxy {
	t.Helper()
	p := newMastsProxy(MastsConfig{Enabled: true}, opts)
	if p == nil {
		t.Fatal("enabled masts proxy is nil")
	}
	return p
}

// mastsGet runs one request for rawQuery from remote (a public address, so
// the per-IP limit applies).
func mastsGet(p *mastsProxy, rawQuery, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/masts?"+rawQuery, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func cellQuery(c mastedb.Cell) string { return "cell=" + strconv.Itoa(c.E) + "," + strconv.Itoa(c.N) }

const (
	clientA = "203.0.113.5:40000"
	clientB = "198.51.100.7:40000"
)

// ── Validation ─────────────────────────────────────────────────────────────

// Every malformed or out-of-extent request is 400 with a fixed body, makes
// no upstream call and is not charged to the client's budget.
func TestMastsRejectsBadCellsBeforeAnyIO(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.rateLimitPerMin = 1
	p := mustMastsProxy(t, opts)

	bad := []string{
		"",
		"cell=",
		"cell=263",
		"cell=263,",
		"cell=,3127",
		"cell=263,3127,1",
		"cell=263;3127",
		"cell=263%203127",
		"cell=-263,3127",
		"cell=%2B263,3127",
		"cell=0263,3127",
		"cell=263,03127",
		"cell=263.0,3127",
		"cell=263,3127.5",
		"cell=%20263,3127",
		"cell=263,3127%20",
		"cell=0x107,3127",
		"cell=1e3,3127",
		"cell=%D9%A2%D9%A6%D9%A3,3127", // Arabic-Indic digits
		"cell=99999999999999999999,3127",
		"cell=218,3127", // west of the extent
		"cell=463,3127", // east
		"cell=263,3016", // south
		"cell=263,3210", // north
		"cell=0,0",
		"Cell=263,3127",
		"CELL=263,3127",
		"cell=263,3127&cell=263,3127",
		"cell=263,3127&bbox=1,2,3,4",
		"cell=263,3127&typeName=x",
		"cell=263,3127&callback=x",
		"cell=263,3127&=",
	}
	for _, q := range bad {
		w := mastsGet(p, q, clientA)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), `"invalid masts request"`) {
			t.Errorf("%q: body %q is not the fixed error", q, w.Body.String())
		}
		for _, echo := range []string{"263", "3127", "bbox", "typeName", "callback"} {
			if strings.Contains(w.Body.String(), echo) {
				t.Errorf("%q: body echoes %q: %s", q, echo, w.Body.String())
			}
		}
	}
	if n := f.hits.Load(); n != 0 {
		t.Fatalf("invalid requests reached upstream %d times", n)
	}
	// None of them was charged: the single budgeted request still passes.
	if w := mastsGet(p, cellQuery(testMastsCell), clientA); w.Code != http.StatusOK {
		t.Fatalf("valid request after %d invalid ones: status %d (budget charged by invalid requests?)", len(bad), w.Code)
	}
}

// The extent corners themselves are valid.
func TestMastsAcceptsExtentCorners(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	p := mustMastsProxy(t, testMastsOptions(f, &logs))
	for _, c := range []mastedb.Cell{
		{E: mastedb.MinCellE, N: mastedb.MinCellN},
		{E: mastedb.MaxCellE, N: mastedb.MaxCellN},
		{E: mastedb.MinCellE, N: mastedb.MaxCellN},
		{E: mastedb.MaxCellE, N: mastedb.MinCellN},
	} {
		if w := mastsGet(p, cellQuery(c), clientA); w.Code != http.StatusOK {
			t.Errorf("cell %v: status %d", c, w.Code)
		}
	}
}

// ── Response ───────────────────────────────────────────────────────────────

func TestMastsResponseShape(t *testing.T) {
	f := newFakeWFS(t)
	f.rows = 18 // three sites
	var logs safeLog
	p := mustMastsProxy(t, testMastsOptions(f, &logs))

	w := mastsGet(p, cellQuery(testMastsCell), clientA)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Fatalf("Cache-Control %q", cc)
	}
	if got := w.Header().Get("X-CoreScope-Cache"); got != "miss" {
		t.Fatalf("X-CoreScope-Cache %q, want miss", got)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if want := []string{"cell", "sites", "truncated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
	var resp MastsCellResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Cell != testMastsCell || resp.Truncated || len(resp.Sites) != 3 {
		t.Fatalf("cell %v truncated %v sites %d", resp.Cell, resp.Truncated, len(resp.Sites))
	}
	for _, s := range resp.Sites {
		if len(s.Entries) != 6 || !s.Interference || s.Lat < 54 || s.Lat > 58 || s.Lon < 8 || s.Lon > 16 {
			t.Fatalf("site %+v", s)
		}
	}
}

// An empty cell is an empty list, not null, and a full page is truncated.
func TestMastsEmptyAndTruncated(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	p := mustMastsProxy(t, testMastsOptions(f, &logs))

	f.rows = 0
	w := mastsGet(p, cellQuery(testMastsCell), clientA)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sites":[]`) {
		t.Fatalf("empty cell: %d %s", w.Code, w.Body.String())
	}
	f.rows = mastedb.PageLimit
	other := mastedb.Cell{E: 264, N: 3127}
	w = mastsGet(p, cellQuery(other), clientA)
	var resp MastsCellResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.Truncated {
		t.Fatalf("full page: err %v truncated %v", err, resp.Truncated)
	}
}

// ── Cache ──────────────────────────────────────────────────────────────────

func TestMastsCacheHitSkipsUpstream(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	p := mustMastsProxy(t, testMastsOptions(f, &logs))
	first := mastsGet(p, cellQuery(testMastsCell), clientA)
	second := mastsGet(p, cellQuery(testMastsCell), clientB)
	if first.Code != 200 || second.Code != 200 {
		t.Fatalf("status %d / %d", first.Code, second.Code)
	}
	if got := second.Header().Get("X-CoreScope-Cache"); got != "hit" {
		t.Fatalf("second request X-CoreScope-Cache %q, want hit", got)
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("cache hit body differs from the miss")
	}
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("upstream hits %d, want 1", n)
	}
}

// The cache holds at most cacheMaxEntries cells, evicting the least
// recently used.
func TestMastsCacheEntryBound(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.cacheMaxEntries = 2
	p := mustMastsProxy(t, opts)
	cells := []mastedb.Cell{{E: 263, N: 3127}, {E: 264, N: 3127}, {E: 265, N: 3127}}
	for _, c := range cells {
		mastsGet(p, cellQuery(c), clientA)
	}
	if n := p.cache.Len(); n != 2 {
		t.Fatalf("cache holds %d entries, want 2", n)
	}
	mastsGet(p, cellQuery(cells[0]), clientA) // evicted: upstream again
	if n := f.hits.Load(); n != 4 {
		t.Fatalf("upstream hits %d, want 4", n)
	}
	mastsGet(p, cellQuery(cells[2]), clientA) // still cached
	if n := f.hits.Load(); n != 4 {
		t.Fatalf("upstream hits %d after a cached cell, want 4", n)
	}
}

// The cache is also bounded by bytes, and a cell expires after the TTL.
func TestMastsCacheByteBoundAndTTL(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	opts.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	p := mustMastsProxy(t, opts)

	w := mastsGet(p, cellQuery(testMastsCell), clientA)
	size := w.Body.Len()
	if p.cache.Size() != size {
		t.Fatalf("cache size %d, want the body's %d", p.cache.Size(), size)
	}
	mu.Lock()
	now = now.Add(mastsCacheTTL - time.Second)
	mu.Unlock()
	mastsGet(p, cellQuery(testMastsCell), clientA)
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("upstream hits %d before the TTL, want 1", n)
	}
	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()
	mastsGet(p, cellQuery(testMastsCell), clientA)
	if n := f.hits.Load(); n != 2 {
		t.Fatalf("upstream hits %d after the TTL, want 2", n)
	}

	// Byte bound: room for one body only.
	opts.cacheMaxBytes = size + size/2
	p = mustMastsProxy(t, opts)
	mastsGet(p, cellQuery(mastedb.Cell{E: 263, N: 3127}), clientA)
	mastsGet(p, cellQuery(mastedb.Cell{E: 264, N: 3127}), clientA)
	if p.cache.Len() != 1 || p.cache.Size() > opts.cacheMaxBytes {
		t.Fatalf("byte-bounded cache holds %d entries, %d bytes (max %d)", p.cache.Len(), p.cache.Size(), opts.cacheMaxBytes)
	}
}

// The shipped bounds: a worst-case cell is well inside the byte bound, and
// the bound itself is modest.
func TestMastsDefaultCacheBounds(t *testing.T) {
	o := defaultMastsProxyOptions()
	if o.cacheMaxEntries <= 0 || o.cacheMaxBytes <= 0 || o.cacheMaxBytes > 64<<20 {
		t.Fatalf("cache bounds %d entries / %d bytes", o.cacheMaxEntries, o.cacheMaxBytes)
	}
	if o.cacheTTL != 7*24*time.Hour {
		t.Fatalf("cache TTL %v, want 7 days (#724 plan)", o.cacheTTL)
	}
	page, err := mastedb.Parse(mastedbtest.GML(testMastsCell.E, testMastsCell.N, mastedb.PageLimit, "500"), testMastsCell)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeMastsCell(testMastsCell, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(b)*16 > o.cacheMaxBytes {
		t.Fatalf("a full cell is %d bytes; the byte bound %d holds fewer than 16", len(b), o.cacheMaxBytes)
	}
}

// ── Coalescing ─────────────────────────────────────────────────────────────

// N concurrent misses for one cell make one upstream call and all get the
// same answer.
func TestMastsCoalescesConcurrentMisses(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	p := mustMastsProxy(t, opts)

	const n = 8
	var wg sync.WaitGroup
	res := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i] = mastsGet(p, cellQuery(testMastsCell), fmt.Sprintf("203.0.113.%d:4000", i+1))
		}(i)
	}
	waitMasts(t, func() bool { return p.flight.Pending() == n })
	close(f.gate)
	wg.Wait()
	if h := f.hits.Load(); h != 1 {
		t.Fatalf("%d concurrent misses made %d upstream calls, want 1", n, h)
	}
	for i, w := range res {
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), res[0].Body.Bytes()) {
			t.Fatalf("request %d: status %d, body differs", i, w.Code)
		}
	}
}

func waitMasts(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ── Per-IP limit ───────────────────────────────────────────────────────────

func TestMastsPerIPLimit(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.rateLimitPerMin = 2
	p := mustMastsProxy(t, opts)

	for i, c := range []mastedb.Cell{{E: 263, N: 3127}, {E: 264, N: 3127}} {
		if w := mastsGet(p, cellQuery(c), clientA); w.Code != 200 {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
	}
	w := mastsGet(p, cellQuery(mastedb.Cell{E: 265, N: 3127}), clientA)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third miss: status %d, want 429", w.Code)
	}
	if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	if n := f.hits.Load(); n != 2 {
		t.Fatalf("upstream hits %d, want 2 (the refused miss must not go upstream)", n)
	}
	// A cached cell is not charged.
	if w := mastsGet(p, cellQuery(mastedb.Cell{E: 263, N: 3127}), clientA); w.Code != 200 {
		t.Fatalf("cached cell over budget: status %d", w.Code)
	}
	// Another client has its own budget.
	if w := mastsGet(p, cellQuery(mastedb.Cell{E: 265, N: 3127}), clientB); w.Code != 200 {
		t.Fatalf("other client: status %d", w.Code)
	}
}

func TestMastsRateLimitSetting(t *testing.T) {
	neg, zero, big, ok := -1, 0, mastsCellRateLimitMax+1, 300
	for _, tc := range []struct {
		name string
		v    *int
		want int
		warn bool
	}{
		{"unset", nil, mastsCellRateLimitDefault, false},
		{"zero is off", &zero, 0, false},
		{"in range", &ok, 300, false},
		{"negative", &neg, mastsCellRateLimitDefault, true},
		{"too large", &big, mastsCellRateLimitDefault, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs safeLog
			got := mastsCellRateLimit(MastsConfig{Enabled: true, CellRateLimitPerMin: tc.v}, logs.printf)
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
			if warned := strings.Contains(logs.String(), "map.masts.cellRateLimitPerMin"); warned != tc.warn {
				t.Fatalf("warned %v, want %v: %q", warned, tc.warn, logs.String())
			}
		})
	}
	if mastsCellRateLimitDefault != 120 {
		t.Fatalf("default %d, want the plan's 120", mastsCellRateLimitDefault)
	}
}

// ── Global upstream bound and admission ────────────────────────────────────

// However many distinct cells are wanted at once, at most mastsUpstreamSlots
// requests are open towards the WFS.
func TestMastsGlobalUpstreamBound(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	p := mustMastsProxy(t, opts)

	const n = 6
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = mastsGet(p, cellQuery(mastedb.Cell{E: 263 + i, N: 3127}), clientA).Code
		}(i)
	}
	waitMasts(t, func() bool { return f.inFlight.Load() == mastsUpstreamSlots && p.flight.Pending() == n })
	time.Sleep(50 * time.Millisecond) // give a leaking request time to show
	if m := f.maxSeen.Load(); m != mastsUpstreamSlots {
		t.Fatalf("%d upstream requests open at once, want %d", m, mastsUpstreamSlots)
	}
	close(f.gate)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: status %d", i, c)
		}
	}
	if m := f.maxSeen.Load(); m > mastsUpstreamSlots {
		t.Fatalf("%d upstream requests open at once, want at most %d", m, mastsUpstreamSlots)
	}
	if mastsUpstreamSlots != 2 {
		t.Fatalf("upstream slots %d, want the plan's 2", mastsUpstreamSlots)
	}
}

// Beyond maxInFlight distinct fetches server-wide a miss is 503 with
// Retry-After before any upstream work, and is not charged (#705 pattern).
func TestMastsAdmissionRefusesAndRefunds(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.maxInFlight = 2
	opts.rateLimitPerMin = 3
	p := mustMastsProxy(t, opts)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mastsGet(p, cellQuery(mastedb.Cell{E: 263 + i, N: 3127}), clientB)
		}(i)
	}
	waitMasts(t, func() bool { return p.admit.InUse() == 2 })
	for i := 0; i < 5; i++ {
		w := mastsGet(p, cellQuery(mastedb.Cell{E: 300, N: 3127}), clientA)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("refusal %d: status %d, want 503", i, w.Code)
		}
		if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 1 {
			t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
		}
	}
	if n := f.hits.Load(); n != 2 {
		t.Fatalf("upstream hits %d, want 2 (a refused fetch must not go upstream)", n)
	}
	if !strings.Contains(logs.String(), "map.masts.maxInFlight") {
		t.Fatalf("no refusal log line: %q", logs.String())
	}
	close(f.gate)
	wg.Wait()
	// Five refusals were refunded: clientA still has its whole budget of 3.
	for i := 0; i < 3; i++ {
		if w := mastsGet(p, cellQuery(mastedb.Cell{E: 300 + i, N: 3127}), clientA); w.Code != 200 {
			t.Fatalf("after refusals, request %d: status %d (refusals charged?)", i, w.Code)
		}
	}
}

func TestMastsMaxInFlightSetting(t *testing.T) {
	zero, big, ok := 0, mastsMaxInFlightCap+1, 8
	for _, tc := range []struct {
		name string
		v    *int
		want int
		warn bool
	}{
		{"unset", nil, mastsMaxInFlightDefault, false},
		{"in range", &ok, 8, false},
		{"zero", &zero, mastsMaxInFlightDefault, true},
		{"too large", &big, mastsMaxInFlightDefault, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs safeLog
			got := mastsMaxInFlight(MastsConfig{Enabled: true, MaxInFlight: tc.v}, logs.printf)
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
			if warned := strings.Contains(logs.String(), "map.masts.maxInFlight"); warned != tc.warn {
				t.Fatalf("warned %v, want %v: %q", warned, tc.warn, logs.String())
			}
		})
	}
}

// ── Fail closed ────────────────────────────────────────────────────────────

const upstreamSecret = "UPSTREAM-DETAIL-must-not-leak"

// Anything but a well-formed GML page is 502 with a fixed body, carries no
// upstream text and is not cached: the next request asks again.
func TestMastsFailsClosedOnBadUpstream(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter, mastedb.Cell){
		"HTML 500": func(w http.ResponseWriter, _ mastedb.Cell) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(500)
			w.Write([]byte("<html>" + upstreamSecret + "</html>"))
		},
		"HTML 200": func(w http.ResponseWriter, _ mastedb.Cell) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>" + upstreamSecret + "</html>"))
		},
		"junk XML": func(w http.ResponseWriter, _ mastedb.Cell) {
			w.Header().Set("Content-Type", "application/gml+xml")
			w.Write([]byte("<x>" + upstreamSecret + "</x>"))
		},
		"OGC exception": func(w http.ResponseWriter, _ mastedb.Cell) {
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0"?><ows:ExceptionReport xmlns:ows="http://www.opengis.net/ows/1.1"><ows:Exception><ows:ExceptionText>` + upstreamSecret + `</ows:ExceptionText></ows:Exception></ows:ExceptionReport>`))
		},
		"cut off": func(w http.ResponseWriter, c mastedb.Cell) {
			w.Header().Set("Content-Type", "application/gml+xml")
			b := mastedbtest.GML(c.E, c.N, 12, "12")
			w.Write(b[:len(b)/2])
		},
		"trailing junk": func(w http.ResponseWriter, c mastedb.Cell) {
			w.Header().Set("Content-Type", "application/gml+xml")
			w.Write(append(mastedbtest.GML(c.E, c.N, 12, "12"), []byte("<"+upstreamSecret)...))
		},
		"redirect": func(w http.ResponseWriter, _ mastedb.Cell) {
			w.Header().Set("Location", "http://example.invalid/"+upstreamSecret)
			w.WriteHeader(http.StatusFound)
		},
		"oversize": func(w http.ResponseWriter, c mastedb.Cell) {
			w.Header().Set("Content-Type", "application/gml+xml")
			w.Write(mastedbtest.GML(c.E, c.N, 3000, "3000"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeWFS(t)
			f.respond = respond
			var logs safeLog
			opts := testMastsOptions(f, &logs)
			opts.client.MaxBodyBytes = 1 << 20
			p := mustMastsProxy(t, opts)
			for i := 0; i < 2; i++ {
				w := mastsGet(p, cellQuery(testMastsCell), clientA)
				if w.Code != http.StatusBadGateway {
					t.Fatalf("status %d, want 502", w.Code)
				}
				if !strings.Contains(w.Body.String(), `"antenna data unavailable"`) {
					t.Fatalf("body %q", w.Body.String())
				}
				if strings.Contains(w.Body.String(), upstreamSecret) || strings.Contains(w.Body.String(), f.srv.URL) {
					t.Fatalf("body leaks upstream detail: %q", w.Body.String())
				}
				if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
					t.Fatalf("failure Cache-Control %q, want no-store", cc)
				}
			}
			if n := f.hits.Load(); n != 2 {
				t.Fatalf("upstream hits %d, want 2 (a failure must not be cached)", n)
			}
			if p.cache.Len() != 0 {
				t.Fatal("a failure was cached")
			}
			if l := logs.String(); strings.Contains(l, upstreamSecret) || strings.Contains(l, f.srv.URL) || !strings.Contains(l, "[masts]") {
				t.Fatalf("log line %q", l)
			}
		})
	}
}

// Failures are logged by category at most once per minute.
func TestMastsFailureLogThrottled(t *testing.T) {
	f := newFakeWFS(t)
	f.respond = func(w http.ResponseWriter, _ mastedb.Cell) { w.WriteHeader(500) }
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	p := mustMastsProxy(t, opts)
	for i := 0; i < 5; i++ {
		mastsGet(p, cellQuery(mastedb.Cell{E: 263 + i, N: 3127}), clientA)
	}
	if n := strings.Count(logs.String(), "[masts] upstream cell failed (status 500)"); n != 1 {
		t.Fatalf("%d failure lines, want 1: %q", n, logs.String())
	}
}

func TestMastsUpstreamTimeoutIs504(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	t.Cleanup(func() { close(f.gate) })
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.client.Timeout = 50 * time.Millisecond
	p := mustMastsProxy(t, opts)
	w := mastsGet(p, cellQuery(testMastsCell), clientA)
	if w.Code != http.StatusGatewayTimeout || !strings.Contains(w.Body.String(), `"antenna data timed out"`) {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
}

// ── Wiring: route, config, client config ───────────────────────────────────

func TestMastsRouteDisabledIs404(t *testing.T) {
	_, router := setupTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/masts?"+cellQuery(testMastsCell), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 while map.masts is off", w.Code)
	}
}

func TestMastsRouteServedWhenEnabled(t *testing.T) {
	f := newFakeWFS(t)
	var logs safeLog
	srv, router := setupTestServer(t)
	srv.masts = mustMastsProxy(t, testMastsOptions(f, &logs))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/masts?"+cellQuery(testMastsCell), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/api/masts?"+cellQuery(testMastsCell), nil))
	if w.Code == http.StatusOK {
		t.Fatal("POST served")
	}
}

func TestNewMastsProxyOffByDefault(t *testing.T) {
	if p := newMastsProxy(MastsConfig{}, defaultMastsProxyOptions()); p != nil {
		t.Fatal("proxy built while map.masts.enabled is false")
	}
	if p := newMastsProxyFromConfig(&Config{}); p != nil {
		t.Fatal("proxy built from an empty config")
	}
	if p := newMastsProxyFromConfig(&Config{Masts: MastsConfig{Enabled: true}}); p == nil || p.client == nil {
		t.Fatal("no proxy from an enabled config")
	}
}

// The production client is pointed at the documented WFS and nowhere an
// operator could choose.
func TestMastsProductionUpstreamIsFixed(t *testing.T) {
	if got := defaultMastsProxyOptions().client.BaseURL; got != mastedb.DefaultBaseURL {
		t.Fatalf("base URL %q", got)
	}
	typ := reflect.TypeOf(MastsConfig{})
	for i := 0; i < typ.NumField(); i++ {
		if n := strings.ToLower(typ.Field(i).Name); strings.Contains(n, "url") || strings.Contains(n, "host") {
			t.Fatalf("map.masts has an upstream field %s", typ.Field(i).Name)
		}
	}
}

func clientMastsRaw(t *testing.T, router *mux.Router) json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body["masts"]
}

func TestMastsClientConfig(t *testing.T) {
	srv, router := setupTestServer(t)
	if raw := clientMastsRaw(t, router); raw != nil {
		t.Fatalf("masts block offered while off: %s", raw)
	}
	f := newFakeWFS(t)
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.rateLimitPerMin = 77
	srv.masts = mustMastsProxy(t, opts)
	raw := clientMastsRaw(t, router)
	if raw == nil {
		t.Fatal("no masts block while enabled")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if want := []string{"attribution", "cellExtent", "cellSizeM", "enabled", "minZoom", "sourceUrl", "url"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("masts block keys %v, want %v", got, want)
	}
	var c MastsClientConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	want := MastsClientConfig{Enabled: true, URL: "/api/masts", MinZoom: 13, CellSizeM: 2000,
		Attribution: mastedb.Attribution, SourceURL: mastedb.SourceURL, CellExtent: [4]int{219, 3017, 462, 3209}}
	if c != want {
		t.Fatalf("masts block %+v, want %+v", c, want)
	}
	if strings.Contains(string(raw), "77") || strings.Contains(string(raw), f.srv.URL) {
		t.Fatalf("masts block leaks server settings: %s", raw)
	}
}

func writeMastsConfig(t *testing.T, cfgJSON string) *Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMastsConfigLoadedAndStripped(t *testing.T) {
	cfg := writeMastsConfig(t, `{"map":{"tiles":{"darkDefault":"osm-dark"},"masts":{"enabled":true,"cellRateLimitPerMin":60,"maxInFlight":4}}}`)
	if !cfg.Masts.Enabled || cfg.Masts.CellRateLimitPerMin == nil || *cfg.Masts.CellRateLimitPerMin != 60 ||
		cfg.Masts.MaxInFlight == nil || *cfg.Masts.MaxInFlight != 4 {
		t.Fatalf("map.masts not loaded: %+v", cfg.Masts)
	}
	if _, ok := cfg.Map["masts"]; ok {
		t.Fatal("map.masts left in the client-visible map block")
	}
	if _, ok := cfg.Map["tiles"]; !ok {
		t.Fatal("map.tiles lost while extracting masts")
	}
	p := newMastsProxyFromConfig(cfg)
	if p == nil || p.admit.Cap() != 4 {
		t.Fatalf("proxy from config: %+v", p)
	}
}

func TestMastsConfigMalformedOrMisspelledIsOff(t *testing.T) {
	for _, doc := range []string{
		`{"map":{"masts":{"enabled":"yes"}}}`,
		`{"map":{"masts":true}}`,
		`{"map":{"Masts":{"enabled":true}}}`,
		`{"map":{"MASTS":{"enabled":true}}}`,
	} {
		var logBuf bytes.Buffer
		log.SetOutput(&logBuf)
		cfg := writeMastsConfig(t, doc)
		log.SetOutput(os.Stderr)
		if cfg.Masts.Enabled || newMastsProxyFromConfig(cfg) != nil {
			t.Fatalf("%s enabled the layer", doc)
		}
		for k := range cfg.Map {
			if strings.EqualFold(k, "masts") {
				t.Fatalf("%s: map.%s left in the map block", doc, k)
			}
		}
		if !strings.Contains(logBuf.String(), "[masts]") {
			t.Fatalf("%s: no warning: %q", doc, logBuf.String())
		}
	}
}

func TestMastsConfigExampleIsOffAndStripped(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Map map[string]json.RawMessage `json:"map"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	raw, ok := doc.Map["masts"]
	if !ok {
		t.Fatal("config.example.json does not document map.masts")
	}
	var ex struct {
		Enabled             bool `json:"enabled"`
		CellRateLimitPerMin *int `json:"cellRateLimitPerMin"`
		MaxInFlight         *int `json:"maxInFlight"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	if ex.CellRateLimitPerMin == nil || *ex.CellRateLimitPerMin != mastsCellRateLimitDefault ||
		ex.MaxInFlight == nil || *ex.MaxInFlight != mastsMaxInFlightDefault {
		t.Fatalf("example documents %+v, not the shipped defaults", ex)
	}
	cfg := writeMastsConfig(t, string(data))
	if cfg.Masts.Enabled {
		t.Fatal("example config enables the masts layer")
	}
	if _, ok := cfg.Map["masts"]; ok {
		t.Fatal("example map.masts left in the client-visible map block")
	}
}

// ── OpenAPI ────────────────────────────────────────────────────────────────

func TestMastsOpenAPIDocumented(t *testing.T) {
	m, ok := routeDescriptions()["GET /api/masts"]
	if !ok {
		t.Fatal("GET /api/masts not in routeDescriptions")
	}
	if len(m.QueryParams) != 1 || m.QueryParams[0].Name != "cell" || !m.QueryParams[0].Required {
		t.Fatalf("query params %+v", m.QueryParams)
	}
	have := map[string]bool{}
	for _, e := range m.Errors {
		have[e.Status] = true
	}
	for _, s := range []string{"429", "502", "503", "504"} {
		if !have[s] {
			t.Errorf("status %s undocumented", s)
		}
	}
}

// ── Perf ───────────────────────────────────────────────────────────────────

// BenchmarkMastsCacheHit is the whole request path for a cached full cell
// (500 rows): validation, cache lookup and the write.
func BenchmarkMastsCacheHit(b *testing.B) {
	opts := defaultMastsProxyOptions()
	opts.client.BaseURL = "http://127.0.0.1:1" // never reached
	p := newMastsProxy(MastsConfig{Enabled: true}, opts)
	page, err := mastedb.Parse(mastedbtest.GML(testMastsCell.E, testMastsCell.N, mastedb.PageLimit, "500"), testMastsCell)
	if err != nil {
		b.Fatal(err)
	}
	data, err := encodeMastsCell(testMastsCell, page)
	if err != nil {
		b.Fatal(err)
	}
	p.cache.Put(testMastsCell, data, p.now())
	u := &url.URL{Path: "/api/masts", RawQuery: cellQuery(testMastsCell)}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, &http.Request{Method: "GET", URL: u, RemoteAddr: clientA})
		if w.Code != 200 {
			b.Fatalf("status %d", w.Code)
		}
	}
}

// BenchmarkMastsMissFullCell is a miss's server-side work for a full cell
// against a local fake: fetch, parse 500 rows, group, encode, cache.
func BenchmarkMastsMissFullCell(b *testing.B) {
	body := mastedbtest.GML(testMastsCell.E, testMastsCell.N, mastedb.PageLimit, "500")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gml+xml")
		w.Write(body)
	}))
	defer up.Close()
	opts := defaultMastsProxyOptions()
	opts.client.BaseURL = up.URL
	opts.cacheTTL = time.Nanosecond // every request is a miss
	opts.rateLimitPerMin = 0        // one client making many misses
	p := newMastsProxy(MastsConfig{Enabled: true}, opts)
	u := &url.URL{Path: "/api/masts", RawQuery: cellQuery(testMastsCell)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, &http.Request{Method: "GET", URL: u, RemoteAddr: clientA})
		if w.Code != 200 {
			b.Fatalf("status %d", w.Code)
		}
	}
}

// TestMastsCacheHitFast is the perf guard: a cached full cell is served in
// well under a millisecond (measured ~µs; the bound leaves CI headroom).
func TestMastsCacheHitFast(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	r := testing.Benchmark(BenchmarkMastsCacheHit)
	if per := time.Duration(r.NsPerOp()); per > time.Millisecond {
		t.Fatalf("cache hit %v per request, want < 1ms", per)
	}
}
