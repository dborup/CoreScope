package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGetStoreStats_CacheHit verifies that a second call within 30s returns
// the cached observation counts without re-querying the database.
func TestGetStoreStats_CacheHit(t *testing.T) {
	srv, _ := setupTestServer(t)
	store := srv.store

	store.statsCacheMu.Lock()
	store.statsCacheTime = time.Now()
	store.statsLastHour = 42
	store.statsLast24h = 777
	store.statsCacheMu.Unlock()

	st, err := store.GetStoreStats()
	if err != nil {
		t.Fatalf("GetStoreStats: %v", err)
	}
	if st.PacketsLastHour != 42 {
		t.Errorf("cache hit: PacketsLastHour want 42 got %d", st.PacketsLastHour)
	}
	if st.PacketsLast24h != 777 {
		t.Errorf("cache hit: PacketsLast24h want 777 got %d", st.PacketsLast24h)
	}
}

// TestGetStoreStats_CacheExpiry verifies that a cache older than 30s is
// discarded and the database query re-runs to refresh the values.
func TestGetStoreStats_CacheExpiry(t *testing.T) {
	srv, _ := setupTestServer(t)
	store := srv.store

	store.statsCacheMu.Lock()
	store.statsCacheTime = time.Now().Add(-35 * time.Second)
	store.statsLastHour = 9999
	store.statsLast24h = 9999
	store.statsCacheMu.Unlock()

	st, err := store.GetStoreStats()
	if err != nil {
		t.Fatalf("GetStoreStats: %v", err)
	}
	if st.PacketsLastHour == 9999 || st.PacketsLast24h == 9999 {
		t.Errorf("stale cache not expired: got PacketsLastHour=%d PacketsLast24h=%d — DB values expected, not sentinel",
			st.PacketsLastHour, st.PacketsLast24h)
	}

	store.statsCacheMu.Lock()
	age := time.Since(store.statsCacheTime)
	store.statsCacheMu.Unlock()
	if age > 5*time.Second {
		t.Errorf("cache not refreshed after expiry: statsCacheTime age=%v", age)
	}
}

// TestGetStoreStats_CacheConcurrentReaders verifies that 100 concurrent
// callers produce no data race on the stats cache fields.
// Run with: go test -race ./... -run TestGetStoreStats_CacheConcurrentReaders
func TestGetStoreStats_CacheConcurrentReaders(t *testing.T) {
	srv, _ := setupTestServer(t)
	store := srv.store

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.GetStoreStats(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent GetStoreStats: %v", err)
	}
}

// Upstream #1963 step 1: concurrent misses of either stats cache share one
// computation, and misses still refresh synchronously.
//
// The tests below hold that computation open through the test-only statsHook
// fields and learn that the other callers have queued behind it by finding
// their goroutines parked in singleflight.(*Group).Do, so no step waits on a
// sleep. A recomputation is counted at "obs-scan" (store) or "stats-build"
// (handler), which only a caller that really recomputes reaches. Scan failures
// are real SQLite errors from renaming the observations table away.

const (
	obsCountsFlightFrame = ".(*PacketStore).loadObsCounts("
	statsFlightFrame     = ".(*Server).handleStats("
)

// statsFlightWaiters counts goroutines parked in singleflight.(*Group).Do
// behind a call already in progress, with frame on their stack. The goroutine
// running the call is excluded: its stack also holds doCall.
//
// Maintenance: the match depends on the runtime.Stack text format, the
// singleflight-internal frames (Do, doCall, WaitGroup.Wait) and the named
// callers in obsCountsFlightFrame and statsFlightFrame. Re-verify it when
// changing the Go version, golang.org/x/sync, or those function names.
func statsFlightWaiters(frame string) int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	waiters := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, frame) &&
			strings.Contains(g, "singleflight.(*Group).Do(") &&
			strings.Contains(g, "sync.(*WaitGroup).Wait(") &&
			!strings.Contains(g, "singleflight.(*Group).doCall(") {
			waiters++
		}
	}
	return waiters
}

// allJoinedOrRunning reports whether n concurrent callers have all either
// queued behind one held computation or, without single-flight, started their
// own.
func allJoinedOrRunning(n int, computations *atomic.Int64, frame string) func() bool {
	return func() bool {
		c := computations.Load()
		return c == int64(n) || (c == 1 && statsFlightWaiters(frame) == n-1)
	}
}

// statsWaitFor yields until cond holds. The deadline only turns a broken test
// into a failure instead of a hang; it is not part of the coordination.
func statsWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// statsGate returns a blocking wait and its release. The release also runs at
// cleanup, so a failed assertion cannot leave goroutines stuck on the gate.
func statsGate(t *testing.T) (wait, release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return func() { <-ch }, release
}

func setObsStatsCache(store *PacketStore, at time.Time, lastHour, last24h int) {
	store.statsCacheMu.Lock()
	store.statsCacheTime = at
	store.statsLastHour = lastHour
	store.statsLast24h = last24h
	store.statsCacheMu.Unlock()
}

func obsStatsCache(store *PacketStore) (at time.Time, lastHour, last24h int) {
	store.statsCacheMu.Lock()
	defer store.statsCacheMu.Unlock()
	return store.statsCacheTime, store.statsLastHour, store.statsLast24h
}

func setStatsResponseCache(srv *Server, resp *StatsResponse, at time.Time) {
	srv.statsMu.Lock()
	srv.statsCache = resp
	srv.statsCachedAt = at
	srv.statsMu.Unlock()
}

// hideObservationsTable renames observations away so the scan fails with a
// real SQLite error until restore runs (also at cleanup).
func hideObservationsTable(t *testing.T, db *DB) (restore func()) {
	t.Helper()
	if _, err := db.conn.Exec(`ALTER TABLE observations RENAME TO observations_hidden`); err != nil {
		t.Fatalf("hide observations: %v", err)
	}
	var once sync.Once
	restore = func() {
		once.Do(func() {
			if _, err := db.conn.Exec(`ALTER TABLE observations_hidden RENAME TO observations`); err != nil {
				t.Errorf("restore observations: %v", err)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

func serveStats(h http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/stats", nil))
	return w
}

func decodeStats(t *testing.T, w *httptest.ResponseRecorder) StatsResponse {
	t.Helper()
	var resp StatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /api/stats: %v (body %q)", err, w.Body.String())
	}
	return resp
}

type storeStatsResult struct {
	st  *Stats
	err error
}

// A fresh cache is served without any refresh being attempted.
func TestGetStoreStats_FreshCacheSkipsRefresh(t *testing.T) {
	srv, _ := setupTestServer(t)
	store := srv.store
	var hookCalls atomic.Int64
	store.statsHook = func(string) { hookCalls.Add(1) }
	setObsStatsCache(store, time.Now(), 42, 777)

	st, err := store.GetStoreStats()
	if err != nil {
		t.Fatalf("GetStoreStats: %v", err)
	}
	if st.PacketsLastHour != 42 || st.PacketsLast24h != 777 {
		t.Errorf("got (%d, %d), want the cached (42, 777)", st.PacketsLastHour, st.PacketsLast24h)
	}
	if n := hookCalls.Load(); n != 0 {
		t.Errorf("fresh cache still reached the refresh path %d times", n)
	}
}

// Concurrent callers on a cold or expired cache share one observations scan,
// and each of them gets its result synchronously.
func TestGetStoreStats_ConcurrentMissesShareOneScan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cachedAt time.Time
	}{
		{"cold", time.Time{}},
		{"expired", time.Now().Add(-35 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := setupTestServer(t)
			store := srv.store
			const callers, expiredValue = 8, 9999
			setObsStatsCache(store, tc.cachedAt, expiredValue, expiredValue)

			var scans atomic.Int64
			wait, release := statsGate(t)
			store.statsHook = func(stage string) {
				if stage == "obs-scan" {
					scans.Add(1)
					wait()
				}
			}

			type counts struct{ lastHour, last24h int }
			results := make(chan storeStatsResult, callers)
			for range callers {
				go func() {
					st, err := store.GetStoreStats()
					results <- storeStatsResult{st, err}
				}()
			}
			statsWaitFor(t, "every caller to queue behind the scan or start its own",
				allJoinedOrRunning(callers, &scans, obsCountsFlightFrame))
			release()

			var first counts
			for i := range callers {
				r := <-results
				if r.err != nil {
					t.Fatalf("GetStoreStats: %v", r.err)
				}
				got := counts{r.st.PacketsLastHour, r.st.PacketsLast24h}
				if got.lastHour == expiredValue || got.last24h == expiredValue {
					t.Errorf("caller got the expired cached value %+v", got)
				}
				if i == 0 {
					first = got
				} else if got != first {
					t.Errorf("callers disagree: %+v vs %+v", got, first)
				}
			}
			if n := scans.Load(); n != 1 {
				t.Errorf("%d concurrent callers ran %d observations scans, want 1", callers, n)
			}
		})
	}
}

// A caller that missed the cache just before another caller's scan filled it
// takes that result from the re-check inside the flight instead of scanning.
func TestGetStoreStats_LateMissRechecksCache(t *testing.T) {
	srv, _ := setupTestServer(t)
	store := srv.store
	setObsStatsCache(store, time.Time{}, 0, 0)

	var misses, scans atomic.Int64
	wait, release := statsGate(t)
	store.statsHook = func(stage string) {
		switch stage {
		case "obs-miss":
			if misses.Add(1) == 1 {
				wait() // the late caller has missed the cache but not yet joined a flight
			}
		case "obs-scan":
			scans.Add(1)
		}
	}

	late := make(chan storeStatsResult, 1)
	go func() {
		st, err := store.GetStoreStats()
		late <- storeStatsResult{st, err}
	}()
	statsWaitFor(t, "the late caller to miss the cache", func() bool { return misses.Load() == 1 })

	st, err := store.GetStoreStats() // fills the cache with a complete scan
	if err != nil {
		t.Fatalf("GetStoreStats: %v", err)
	}
	if n := scans.Load(); n != 1 {
		t.Fatalf("the filling call ran %d scans, want 1", n)
	}

	release()
	r := <-late
	if r.err != nil {
		t.Fatalf("late GetStoreStats: %v", r.err)
	}
	if r.st.PacketsLastHour != st.PacketsLastHour || r.st.PacketsLast24h != st.PacketsLast24h {
		t.Errorf("late caller got (%d, %d), want the cached (%d, %d)",
			r.st.PacketsLastHour, r.st.PacketsLast24h, st.PacketsLastHour, st.PacketsLast24h)
	}
	if n := scans.Load(); n != 1 {
		t.Errorf("late caller scanned again: %d scans, want 1", n)
	}
}

// Scans never overlap, so an older scan result cannot be written over a newer
// one that a caller has already been given.
func TestGetStoreStats_NoOverlappingScanWriteBack(t *testing.T) {
	srv, _ := setupTestServer(t)
	store := srv.store
	setObsStatsCache(store, time.Time{}, 0, 0)

	var scans, writes atomic.Int64
	wait, release := statsGate(t)
	store.statsHook = func(stage string) {
		switch stage {
		case "obs-scan":
			scans.Add(1)
		case "obs-write":
			if writes.Add(1) == 1 {
				wait() // the first scan holds its counts but has not stored them
			}
		}
	}

	first := make(chan storeStatsResult, 1)
	go func() {
		st, err := store.GetStoreStats()
		first <- storeStatsResult{st, err}
	}()
	statsWaitFor(t, "the first scan to read its counts", func() bool { return writes.Load() == 1 })

	// An observation lands after the first scan read the table.
	if _, err := srv.db.conn.Exec(
		`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		 VALUES ((SELECT MIN(id) FROM transmissions), 1, 1.0, -90, '[]', ?)`, time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert observation: %v", err)
	}

	second := make(chan storeStatsResult, 1)
	go func() {
		st, err := store.GetStoreStats()
		second <- storeStatsResult{st, err}
	}()
	// Without single-flight the second caller scans the newer table and stores
	// its counts while the first is still held; with it, it queues behind.
	var b storeStatsResult
	bDone := false
	statsWaitFor(t, "the second caller to finish or queue behind the first", func() bool {
		select {
		case b = <-second:
			bDone = true
			return true
		default:
			return statsFlightWaiters(obsCountsFlightFrame) == 1
		}
	})
	release()
	a := <-first
	if !bDone {
		b = <-second
	}
	if a.err != nil || b.err != nil {
		t.Fatalf("first err=%v, second err=%v", a.err, b.err)
	}

	after, err := store.GetStoreStats()
	if err != nil {
		t.Fatalf("GetStoreStats: %v", err)
	}
	for _, given := range []*Stats{a.st, b.st} {
		if after.PacketsLastHour < given.PacketsLastHour || after.PacketsLast24h < given.PacketsLast24h {
			t.Errorf("cache went back to (%d, %d) after a caller was given (%d, %d)",
				after.PacketsLastHour, after.PacketsLast24h, given.PacketsLastHour, given.PacketsLast24h)
		}
	}
	if n := scans.Load(); n != 1 {
		t.Errorf("second caller ran an overlapping scan: %d scans, want 1", n)
	}
}

// A failed scan returns its error to every caller that shared it, is not
// cached, and the next call scans again.
func TestGetStoreStats_ConcurrentMissesShareErrorThenRecover(t *testing.T) {
	srv, _ := setupTestServer(t)
	store := srv.store
	setObsStatsCache(store, time.Time{}, 0, 0)
	restore := hideObservationsTable(t, srv.db)

	const callers = 8
	var scans atomic.Int64
	wait, release := statsGate(t)
	store.statsHook = func(stage string) {
		if stage == "obs-scan" {
			scans.Add(1)
			wait()
		}
	}
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := store.GetStoreStats()
			errs <- err
		}()
	}
	statsWaitFor(t, "every caller to queue behind the scan or start its own",
		allJoinedOrRunning(callers, &scans, obsCountsFlightFrame))
	release()

	for range callers {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "no such table") {
			t.Errorf("caller got err=%v, want the scan's no-such-table error", err)
		}
	}
	if n := scans.Load(); n != 1 {
		t.Errorf("%d concurrent callers ran %d observations scans, want 1", callers, n)
	}
	if at, _, _ := obsStatsCache(store); !at.IsZero() {
		t.Errorf("failed scan stamped the cache at %v", at)
	}

	restore()
	st, err := store.GetStoreStats()
	if err != nil {
		t.Fatalf("GetStoreStats after recovery: %v", err)
	}
	if n := scans.Load(); n != 2 {
		t.Errorf("recovery ran %d scans in total, want 2: the error must not be cached", n)
	}
	if at, lastHour, last24h := obsStatsCache(store); at.IsZero() || lastHour != st.PacketsLastHour || last24h != st.PacketsLast24h {
		t.Errorf("recovery did not refill the cache: cache=(%v, %d, %d), result=(%d, %d)",
			at, lastHour, last24h, st.PacketsLastHour, st.PacketsLast24h)
	}
}

// After a long idle period the first call gets fresh counts or an error, never
// the old cached value as a fallback.
func TestGetStoreStats_FirstCallAfterIdle(t *testing.T) {
	const idleValue = 7777
	idleSince := time.Now().Add(-2 * time.Hour)

	t.Run("fresh", func(t *testing.T) {
		srv, _ := setupTestServer(t)
		store := srv.store
		setObsStatsCache(store, idleSince, idleValue, idleValue)

		st, err := store.GetStoreStats()
		if err != nil {
			t.Fatalf("GetStoreStats: %v", err)
		}
		if st.PacketsLastHour == idleValue || st.PacketsLast24h == idleValue {
			t.Errorf("got the 2h-old cached value (%d, %d)", st.PacketsLastHour, st.PacketsLast24h)
		}
		if at, lastHour, last24h := obsStatsCache(store); !at.After(idleSince) || lastHour != st.PacketsLastHour || last24h != st.PacketsLast24h {
			t.Errorf("the call did not refresh the cache before returning: cache=(%v, %d, %d)", at, lastHour, last24h)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv, _ := setupTestServer(t)
		store := srv.store
		setObsStatsCache(store, idleSince, idleValue, idleValue)
		hideObservationsTable(t, srv.db)

		st, err := store.GetStoreStats()
		if err == nil {
			t.Fatalf("got (%d, %d) and no error while the scan cannot run", st.PacketsLastHour, st.PacketsLast24h)
		}
		if st != nil && (st.PacketsLastHour == idleValue || st.PacketsLast24h == idleValue) {
			t.Errorf("error came back with the 2h-old cached value (%d, %d)", st.PacketsLastHour, st.PacketsLast24h)
		}
		if at, _, _ := obsStatsCache(store); !at.Equal(idleSince) {
			t.Errorf("failed refresh re-stamped the cache at %v", at)
		}
	})
}

// A fresh /api/stats cache is served without any rebuild being attempted.
func TestHandleStats_FreshCacheSkipsRebuild(t *testing.T) {
	srv, router := setupTestServer(t)
	var hookCalls atomic.Int64
	srv.statsHook = func(string) { hookCalls.Add(1) }
	setStatsResponseCache(srv, &StatsResponse{PacketsLastHour: 4242}, time.Now())

	w := serveStats(router)
	if w.Code != 200 {
		t.Fatalf("GET /api/stats: %d %s", w.Code, w.Body.String())
	}
	if got := decodeStats(t, w).PacketsLastHour; got != 4242 {
		t.Errorf("packetsLastHour = %d, want the cached 4242", got)
	}
	if n := hookCalls.Load(); n != 0 {
		t.Errorf("fresh cache still reached the rebuild path %d times", n)
	}
}

// Concurrent requests on a cold or expired /api/stats cache share one rebuild.
func TestHandleStats_ConcurrentMissesShareOneBuild(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cached   *StatsResponse
		cachedAt time.Time
	}{
		{"cold", nil, time.Time{}},
		{"expired", &StatsResponse{PacketsLastHour: 9999}, time.Now().Add(-11 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			setStatsResponseCache(srv, tc.cached, tc.cachedAt)

			const requests = 8
			var builds atomic.Int64
			wait, release := statsGate(t)
			srv.statsHook = func(stage string) {
				if stage == "stats-build" {
					builds.Add(1)
					wait()
				}
			}
			responses := make(chan *httptest.ResponseRecorder, requests)
			for range requests {
				go func() { responses <- serveStats(router) }()
			}
			statsWaitFor(t, "every request to queue behind the rebuild or start its own",
				allJoinedOrRunning(requests, &builds, statsFlightFrame))
			release()

			var first string
			for i := range requests {
				w := <-responses
				if w.Code != 200 {
					t.Fatalf("GET /api/stats: %d %s", w.Code, w.Body.String())
				}
				if i == 0 {
					first = w.Body.String()
					if got := decodeStats(t, w).PacketsLastHour; got == 9999 {
						t.Errorf("request got the expired cached response")
					}
				} else if w.Body.String() != first {
					t.Errorf("responses differ:\n%s\n%s", w.Body.String(), first)
				}
			}
			if n := builds.Load(); n != 1 {
				t.Errorf("%d concurrent requests ran %d rebuilds, want 1", requests, n)
			}
		})
	}
}

// A request that missed the cache just before another request's rebuild
// filled it takes that response from the re-check inside the flight.
func TestHandleStats_LateMissRechecksCache(t *testing.T) {
	srv, router := setupTestServer(t)
	var misses, builds atomic.Int64
	wait, release := statsGate(t)
	srv.statsHook = func(stage string) {
		switch stage {
		case "stats-miss":
			if misses.Add(1) == 1 {
				wait() // the late request has missed the cache but not yet joined a flight
			}
		case "stats-build":
			builds.Add(1)
		}
	}

	late := make(chan *httptest.ResponseRecorder, 1)
	go func() { late <- serveStats(router) }()
	statsWaitFor(t, "the late request to miss the cache", func() bool { return misses.Load() == 1 })

	w := serveStats(router) // fills the cache with a complete rebuild
	if w.Code != 200 {
		t.Fatalf("GET /api/stats: %d %s", w.Code, w.Body.String())
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("the filling request ran %d rebuilds, want 1", n)
	}

	release()
	lw := <-late
	if lw.Code != 200 {
		t.Fatalf("late GET /api/stats: %d %s", lw.Code, lw.Body.String())
	}
	if lw.Body.String() != w.Body.String() {
		t.Errorf("late request got a different response than the cached one:\n%s\n%s", lw.Body.String(), w.Body.String())
	}
	if n := builds.Load(); n != 1 {
		t.Errorf("late request rebuilt again: %d rebuilds, want 1", n)
	}
}

// A failed rebuild returns 500 to every request that shared it, is not cached,
// and the next request rebuilds.
func TestHandleStats_ConcurrentMissesShareErrorThenRecover(t *testing.T) {
	srv, router := setupTestServer(t)
	restore := hideObservationsTable(t, srv.db)

	const requests = 8
	var builds atomic.Int64
	wait, release := statsGate(t)
	srv.statsHook = func(stage string) {
		if stage == "stats-build" {
			builds.Add(1)
			wait()
		}
	}
	responses := make(chan *httptest.ResponseRecorder, requests)
	for range requests {
		go func() { responses <- serveStats(router) }()
	}
	statsWaitFor(t, "every request to queue behind the rebuild or start its own",
		allJoinedOrRunning(requests, &builds, statsFlightFrame))
	release()

	for range requests {
		if w := <-responses; w.Code != 500 || !strings.Contains(w.Body.String(), "no such table") {
			t.Errorf("request got %d %s, want 500 with the scan's no-such-table error", w.Code, w.Body.String())
		}
	}
	if n := builds.Load(); n != 1 {
		t.Errorf("%d concurrent requests ran %d rebuilds, want 1", requests, n)
	}
	srv.statsMu.Lock()
	cached := srv.statsCache
	srv.statsMu.Unlock()
	if cached != nil {
		t.Errorf("failed rebuild was cached")
	}

	restore()
	if w := serveStats(router); w.Code != 200 {
		t.Fatalf("GET /api/stats after recovery: %d %s", w.Code, w.Body.String())
	}
	if n := builds.Load(); n != 2 {
		t.Errorf("recovery ran %d rebuilds in total, want 2: the error must not be cached", n)
	}
}

// After a long idle period the first request gets fresh stats or an error,
// never an old cached response or old observation counts as a fallback.
func TestHandleStats_FirstRequestAfterIdle(t *testing.T) {
	const idleValue = 7777
	idleSince := time.Now().Add(-2 * time.Hour)
	seedIdle := func(srv *Server) {
		setStatsResponseCache(srv, &StatsResponse{PacketsLastHour: idleValue, PacketsLast24h: idleValue}, idleSince)
		setObsStatsCache(srv.store, idleSince, idleValue, idleValue)
	}

	t.Run("fresh", func(t *testing.T) {
		srv, router := setupTestServer(t)
		seedIdle(srv)

		w := serveStats(router)
		if w.Code != 200 {
			t.Fatalf("GET /api/stats: %d %s", w.Code, w.Body.String())
		}
		if resp := decodeStats(t, w); resp.PacketsLastHour == idleValue || resp.PacketsLast24h == idleValue {
			t.Errorf("got the 2h-old value (%d, %d)", resp.PacketsLastHour, resp.PacketsLast24h)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv, router := setupTestServer(t)
		seedIdle(srv)
		hideObservationsTable(t, srv.db)

		if w := serveStats(router); w.Code != 500 {
			t.Errorf("got %d %s, want 500 while the scan cannot run", w.Code, w.Body.String())
		}
	})
}
