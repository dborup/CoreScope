package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPutCachedResultLocked_BoundsEntries proves the ceiling: inserting far
// more distinct keys than analyticsCacheMaxEntries leaves the map at exactly
// the ceiling, never above it.
func TestPutCachedResultLocked_BoundsEntries(t *testing.T) {
	m := make(map[string]*cachedResult)
	const n = analyticsCacheMaxEntries * 4
	for i := 0; i < n; i++ {
		putCachedResultLocked(m, fmt.Sprintf("k%06d", i), map[string]interface{}{"i": i}, time.Minute)
		if len(m) > analyticsCacheMaxEntries {
			t.Fatalf("after %d inserts len=%d exceeds ceiling %d", i+1, len(m), analyticsCacheMaxEntries)
		}
	}
	if len(m) != analyticsCacheMaxEntries {
		t.Fatalf("len=%d want ceiling %d", len(m), analyticsCacheMaxEntries)
	}
}

// TestPutCachedResultLocked_RefreshDoesNotEvict proves a refresh of an existing
// key never evicts a different live key, even at the ceiling.
func TestPutCachedResultLocked_RefreshDoesNotEvict(t *testing.T) {
	m := make(map[string]*cachedResult)
	for i := 0; i < analyticsCacheMaxEntries; i++ {
		putCachedResultLocked(m, fmt.Sprintf("k%06d", i), map[string]interface{}{"i": i}, time.Minute)
	}
	// All entries live; map is exactly full.
	if len(m) != analyticsCacheMaxEntries {
		t.Fatalf("precondition len=%d want %d", len(m), analyticsCacheMaxEntries)
	}
	// Snapshot the keys that must survive a refresh.
	before := make(map[string]bool, len(m))
	for k := range m {
		before[k] = true
	}
	// Refresh an existing key many times. No other live key may disappear.
	for r := 0; r < 100; r++ {
		putCachedResultLocked(m, "k000000", map[string]interface{}{"r": r}, time.Minute)
		if len(m) != analyticsCacheMaxEntries {
			t.Fatalf("refresh %d changed size to %d (want %d) — refresh evicted a live key", r, len(m), analyticsCacheMaxEntries)
		}
	}
	for k := range before {
		if _, ok := m[k]; !ok {
			t.Fatalf("refresh evicted live key %q", k)
		}
	}
}

// TestEvictCachedResultLocked_ExpiredFirst proves expired entries are dropped
// before any live entry: a map full of expired entries plus a new key must not
// evict the newest live entry, and the stale ones go.
func TestEvictCachedResultLocked_ExpiredFirst(t *testing.T) {
	m := make(map[string]*cachedResult)
	past := time.Now().Add(-time.Hour)
	for i := 0; i < analyticsCacheMaxEntries; i++ {
		m[fmt.Sprintf("old%06d", i)] = &cachedResult{data: map[string]interface{}{}, expiresAt: past}
	}
	putCachedResultLocked(m, "fresh", map[string]interface{}{"x": 1}, time.Minute)
	if _, ok := m["fresh"]; !ok {
		t.Fatal("fresh key was not stored")
	}
	if len(m) != 1 {
		t.Fatalf("expected expired sweep to leave only the fresh key, len=%d", len(m))
	}
}

// TestEvictCachedResultLocked_DeterministicVictim proves the victim at the
// ceiling is the entry nearest expiry, ties broken by smallest key, regardless
// of map iteration order.
func TestEvictCachedResultLocked_DeterministicVictim(t *testing.T) {
	base := time.Now().Add(time.Hour)
	for trial := 0; trial < 20; trial++ {
		m := make(map[string]*cachedResult)
		// Fill to the ceiling with entries whose expiry increases with the key,
		// so the smallest key is the soonest to expire = the victim.
		for i := 0; i < analyticsCacheMaxEntries; i++ {
			m[fmt.Sprintf("k%06d", i)] = &cachedResult{
				data:      map[string]interface{}{},
				expiresAt: base.Add(time.Duration(i) * time.Second),
			}
		}
		putCachedResultLocked(m, "zzz-new", map[string]interface{}{}, time.Hour)
		if _, ok := m["k000000"]; ok {
			t.Fatalf("trial %d: soonest-expiry entry k000000 should have been evicted", trial)
		}
		if _, ok := m["zzz-new"]; !ok {
			t.Fatalf("trial %d: new key not stored", trial)
		}
		if len(m) != analyticsCacheMaxEntries {
			t.Fatalf("trial %d: len=%d want %d", trial, len(m), analyticsCacheMaxEntries)
		}
	}
}

// TestEvictCachedResultLocked_TieBreakSmallestKey proves that among entries
// sharing the soonest expiry, the lexicographically smallest key is evicted.
func TestEvictCachedResultLocked_TieBreakSmallestKey(t *testing.T) {
	soon := time.Now().Add(time.Minute)
	later := time.Now().Add(time.Hour)
	for trial := 0; trial < 20; trial++ {
		m := make(map[string]*cachedResult)
		// Two entries tie for soonest expiry: "aaa" and "bbb". "aaa" must go.
		m["aaa"] = &cachedResult{data: map[string]interface{}{}, expiresAt: soon}
		m["bbb"] = &cachedResult{data: map[string]interface{}{}, expiresAt: soon}
		for i := 0; i < analyticsCacheMaxEntries-2; i++ {
			m[fmt.Sprintf("c%06d", i)] = &cachedResult{data: map[string]interface{}{}, expiresAt: later}
		}
		putCachedResultLocked(m, "new", map[string]interface{}{}, time.Hour)
		if _, ok := m["aaa"]; ok {
			t.Fatalf("trial %d: tie should evict smallest key 'aaa'", trial)
		}
		if _, ok := m["bbb"]; !ok {
			t.Fatalf("trial %d: 'bbb' should have survived the tie", trial)
		}
	}
}

// TestAnalyticsFilterError covers the handler-side rejection of overlong
// filters and unknown areas, and that legitimate selections pass.
func TestAnalyticsFilterError(t *testing.T) {
	areas := map[string]AreaEntry{"sjaelland": {}, "fyn": {}}
	long := make([]byte, analyticsFilterMaxLen+1)
	for i := range long {
		long[i] = 'A'
	}
	ok := string(make([]byte, analyticsFilterMaxLen))
	cases := []struct {
		name          string
		region, area  string
		wantNonEmpty  bool
		wantSubstring string
	}{
		{"empty global", "", "", false, ""},
		{"valid region", "EDI,GLA", "", false, ""},
		{"valid area", "EDI", "fyn", false, ""},
		{"region at limit", ok, "", false, ""},
		{"overlong region", string(long), "", true, "region"},
		{"overlong area", "EDI", string(long), true, "area"},
		{"unknown area", "EDI", "atlantis", true, "unknown area"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := analyticsFilterError(c.region, c.area, areas)
			if c.wantNonEmpty && got == "" {
				t.Fatalf("expected a rejection message, got none")
			}
			if !c.wantNonEmpty && got != "" {
				t.Fatalf("expected no rejection, got %q", got)
			}
		})
	}
}

// TestAnalyticsFilterError_NilAreasRejectsArea proves that with no configured
// areas, any non-empty area is unknown (and empty area still passes).
func TestAnalyticsFilterError_NilAreasRejectsArea(t *testing.T) {
	if msg := analyticsFilterError("EDI", "", nil); msg != "" {
		t.Fatalf("empty area with nil areas should pass, got %q", msg)
	}
	if msg := analyticsFilterError("EDI", "fyn", nil); msg == "" {
		t.Fatal("non-empty area with nil areas should be rejected")
	}
}

// BenchmarkPutCachedResultLocked_FullMapMiss measures the worst case: a cache
// pinned at the ceiling with all-live entries, where every insert is a new key
// and so pays the deterministic single-victim scan (O(ceiling), bounded). This
// is the per-request cost the eviction adds on a cold miss.
func BenchmarkPutCachedResultLocked_FullMapMiss(b *testing.B) {
	m := make(map[string]*cachedResult, analyticsCacheMaxEntries)
	for i := 0; i < analyticsCacheMaxEntries; i++ {
		m[fmt.Sprintf("seed%06d", i)] = &cachedResult{data: map[string]interface{}{}, expiresAt: time.Now().Add(time.Hour)}
	}
	res := map[string]interface{}{"x": 1}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		putCachedResultLocked(m, fmt.Sprintf("new%08d", i), res, time.Hour)
	}
}

// BenchmarkPutCachedResultLocked_Refresh measures the common refresh path (key
// already present): the guard returns before any scan, so it must match a
// plain map assignment.
func BenchmarkPutCachedResultLocked_Refresh(b *testing.B) {
	m := make(map[string]*cachedResult, analyticsCacheMaxEntries)
	for i := 0; i < analyticsCacheMaxEntries; i++ {
		m[fmt.Sprintf("seed%06d", i)] = &cachedResult{data: map[string]interface{}{}, expiresAt: time.Now().Add(time.Hour)}
	}
	res := map[string]interface{}{"x": 1}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		putCachedResultLocked(m, "seed000000", res, time.Hour)
	}
}

// BenchmarkPlainMapAssign_Baseline is the pre-#370 unbounded write, for a delta
// against the eviction cost above.
func BenchmarkPlainMapAssign_Baseline(b *testing.B) {
	m := make(map[string]*cachedResult, analyticsCacheMaxEntries)
	res := map[string]interface{}{"x": 1}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m[fmt.Sprintf("new%08d", i)] = &cachedResult{data: res, expiresAt: time.Now().Add(time.Hour)}
	}
}

// TestEvictStatsCacheLocked_Bounds covers the routes.go per-window stats cache
// evictor (wardrivingStatsCache): bounded size, refresh does not evict, expired
// entries go first, and the deterministic smallest-key victim on a tie.
func TestEvictStatsCacheLocked_Bounds(t *testing.T) {
	const ttl = 30 * time.Second
	max := wardrivingStatsMaxEntries
	cache := map[string]*int{}
	at := map[string]time.Time{}
	now := time.Now()

	// Bounded: inserting far more than max distinct keys never exceeds max.
	for i := 0; i < max*3; i++ {
		k := fmt.Sprintf("k%06d", i)
		evictStatsCacheLocked(cache, at, k, now, ttl, max)
		v := i
		cache[k] = &v
		at[k] = now
		if len(cache) > max {
			t.Fatalf("insert %d: len=%d exceeds ceiling %d", i, len(cache), max)
		}
	}
	if len(cache) != max {
		t.Fatalf("len=%d want %d", len(cache), max)
	}

	// Refresh an existing key: no eviction, size unchanged.
	anyKey := ""
	for k := range cache {
		anyKey = k
		break
	}
	evictStatsCacheLocked(cache, at, anyKey, now, ttl, max)
	if len(cache) != max {
		t.Fatalf("refresh changed size to %d", len(cache))
	}

	// Expired-first: a full cache of stale entries plus a new key collapses to
	// just the survivors of the expiry sweep.
	cache = map[string]*int{}
	at = map[string]time.Time{}
	stale := now.Add(-ttl - time.Hour)
	for i := 0; i < max; i++ {
		k := fmt.Sprintf("old%06d", i)
		v := i
		cache[k] = &v
		at[k] = stale
	}
	evictStatsCacheLocked(cache, at, "fresh", now, ttl, max)
	if len(cache) != 0 {
		t.Fatalf("expired sweep should have emptied the cache, len=%d", len(cache))
	}
}

// TestEvictInspectCacheLocked_Bounds covers the path-inspect cache evictor:
// expired-first, bounded size, refresh-no-evict, deterministic victim.
func TestEvictInspectCacheLocked_Bounds(t *testing.T) {
	now := time.Now()
	m := map[string]*inspectCachedResult{}
	// Fill with entries whose expiry increases with the key, all live.
	for i := 0; i < inspectCacheMaxEntries; i++ {
		m[fmt.Sprintf("k%06d", i)] = &inspectCachedResult{expiresAt: now.Add(time.Duration(i+1) * time.Minute)}
	}
	// New key at the cap: the helper makes room (the handler then inserts), so
	// the soonest-expiry entry (smallest key here) is evicted and the insert
	// lands at exactly the ceiling.
	evictInspectCacheLocked(m, "znew", now)
	m["znew"] = &inspectCachedResult{expiresAt: now.Add(time.Hour)}
	if _, ok := m["k000000"]; ok {
		t.Fatal("soonest-expiry entry should have been evicted")
	}
	if len(m) != inspectCacheMaxEntries {
		t.Fatalf("len=%d want %d", len(m), inspectCacheMaxEntries)
	}
	// Refresh of an existing key never evicts (and never grows the map).
	before := len(m)
	evictInspectCacheLocked(m, "znew", now)
	if len(m) != before {
		t.Fatalf("refresh changed size %d -> %d", before, len(m))
	}
	// Expired entries are always dropped, even below the cap.
	m2 := map[string]*inspectCachedResult{
		"a": {expiresAt: now.Add(-time.Hour)},
		"b": {expiresAt: now.Add(time.Hour)},
	}
	evictInspectCacheLocked(m2, "c", now)
	if _, ok := m2["a"]; ok {
		t.Fatal("expired entry 'a' should have been dropped")
	}
	if _, ok := m2["b"]; !ok {
		t.Fatal("live entry 'b' should remain")
	}
}

// TestEvictRegionObsLocked_Bounds covers the region→observer-id cache bound:
// bounded size and the deterministic smallest-key victim.
func TestEvictRegionObsLocked_Bounds(t *testing.T) {
	s := NewPacketStore(nil, nil)
	s.regionObsCache = map[string]map[string]bool{}
	for i := 0; i < regionObsCacheMaxEntries*3; i++ {
		k := fmt.Sprintf("R%06d", i)
		s.evictRegionObsLocked(k)
		s.regionObsCache[k] = map[string]bool{}
		if len(s.regionObsCache) > regionObsCacheMaxEntries {
			t.Fatalf("insert %d: len=%d exceeds ceiling %d", i, len(s.regionObsCache), regionObsCacheMaxEntries)
		}
	}
	if len(s.regionObsCache) != regionObsCacheMaxEntries {
		t.Fatalf("len=%d want %d", len(s.regionObsCache), regionObsCacheMaxEntries)
	}
	// The smallest keys were evicted first; the earliest key must be gone.
	if _, ok := s.regionObsCache["R000000"]; ok {
		t.Fatal("earliest region key should have been evicted")
	}
}

// TestHandleAnalyticsRF_FilterRejection proves the handler rejects overlong
// filters and unknown areas with 400 before any store work, while legitimate
// region/area selections pass (store is nil, so a passing request returns the
// empty 200 response — the point is only that it is NOT a 400).
func TestHandleAnalyticsRF_FilterRejection(t *testing.T) {
	srv := &Server{cfg: &Config{Areas: map[string]AreaEntry{"fyn": {}}}}
	long := strings.Repeat("A", analyticsFilterMaxLen+1)
	call := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/analytics/rf?"+q, nil)
		w := httptest.NewRecorder()
		srv.handleAnalyticsRF(w, req)
		return w
	}
	reject := []struct{ name, query string }{
		{"overlong region", "region=" + url.QueryEscape(long)},
		{"overlong area", "area=" + url.QueryEscape(long)},
		{"unknown area", "area=atlantis"},
	}
	for _, c := range reject {
		if w := call(c.query); w.Code != 400 {
			t.Errorf("%s: want 400, got %d", c.name, w.Code)
		}
	}
	pass := []struct{ name, query string }{
		{"global", ""},
		{"valid region", "region=EDI,GLA"},
		{"valid area", "area=fyn"},
	}
	for _, c := range pass {
		if w := call(c.query); w.Code == 400 {
			t.Errorf("%s: should not be rejected, got 400", c.name)
		}
	}
}

// TestHandleAnalyticsSubpathsBulk_RegionRejection proves the region-only
// handlers reject an overlong region before work and accept a legitimate one.
func TestHandleAnalyticsSubpathsBulk_RegionRejection(t *testing.T) {
	srv := &Server{cfg: &Config{}}
	long := strings.Repeat("A", analyticsFilterMaxLen+1)
	call := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/analytics/subpaths-bulk?"+q, nil)
		w := httptest.NewRecorder()
		srv.handleAnalyticsSubpathsBulk(w, req)
		return w
	}
	if w := call("region=" + url.QueryEscape(long)); w.Code != 400 {
		t.Errorf("overlong region: want 400, got %d", w.Code)
	}
	// A legitimate region passes validation (missing groups then yields a
	// non-400 groups-required response).
	if w := call("region=EDI"); w.Code == 400 {
		t.Errorf("valid region should not be rejected, got 400")
	}
}

// TestHandleWardrivingStats_RejectsOverlongChannel proves the free-text channel
// filter is length-rejected before GetWardrivingStats, and a normal channel
// still works end-to-end against a real store.
func TestHandleWardrivingStats_RejectsOverlongChannel(t *testing.T) {
	_, router := setupTestServer(t)
	long := strings.Repeat("x", analyticsFilterMaxLen+1)

	req := httptest.NewRequest(http.MethodGet, "/api/analytics/wardriving?window=24h&channel="+url.QueryEscape(long), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("overlong channel: want 400, got %d", w.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if msg, _ := body["error"].(string); !strings.Contains(msg, "channel") {
		t.Fatalf("expected channel-length error, got %q", msg)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/analytics/wardriving?window=24h&channel="+url.QueryEscape("#wardriving"), nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code == 400 {
		t.Fatalf("normal channel should not be rejected, got 400")
	}
}

// TestGetAnalyticsRF_CacheBounded is the wiring proof: driving the real cached
// wrapper with far more distinct absolute time-windows than the ceiling must
// leave rfCache bounded. Red before the write site goes through the helper.
func TestGetAnalyticsRF_CacheBounded(t *testing.T) {
	s := NewPacketStore(nil, nil)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = analyticsCacheMaxEntries * 3
	for i := 0; i < n; i++ {
		// Distinct absolute windows => distinct cache keys, region/area empty so
		// no DB work is needed.
		w := TimeWindow{Since: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)}
		s.GetAnalyticsRFWithWindow("", "", w)
	}
	s.cacheMu.RLock()
	got := len(s.rfCache)
	s.cacheMu.RUnlock()
	if got > analyticsCacheMaxEntries {
		t.Fatalf("rfCache grew to %d entries, exceeding ceiling %d", got, analyticsCacheMaxEntries)
	}
}

// TestGetAnalyticsRF_ConcurrentBounded drives the real cached wrapper from many
// goroutines with overlapping distinct windows. Run under -race it proves the
// bounded insertion adds no data race on cacheMu, and the map stays bounded.
func TestGetAnalyticsRF_ConcurrentBounded(t *testing.T) {
	s := NewPacketStore(nil, nil)
	base := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	const workers = 16
	const perWorker = analyticsCacheMaxEntries * 2
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				w := TimeWindow{Since: base.Add(time.Duration((g*perWorker + i)) * time.Minute).Format(time.RFC3339)}
				s.GetAnalyticsRFWithWindow("", "", w)
			}
		}(g)
	}
	wg.Wait()
	s.cacheMu.RLock()
	got := len(s.rfCache)
	s.cacheMu.RUnlock()
	if got > analyticsCacheMaxEntries {
		t.Fatalf("rfCache grew to %d entries under concurrency, exceeding ceiling %d", got, analyticsCacheMaxEntries)
	}
}
