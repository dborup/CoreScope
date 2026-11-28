package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

// Wiring tests for #370: every write site of a bounded cache is driven through
// its real entry point (Get… wrapper or HTTP handler) with more distinct keys
// than the ceiling. Each asserts the ceiling holds and that re-filling an
// existing key at the ceiling evicts no other live key. The helper unit tests
// in analytics_cache_bounds_test.go cannot catch a call site reverted to a
// plain map assignment; these do.

// cachedResultFamily is one write site of a map[string]*cachedResult cache.
type cachedResultFamily struct {
	name  string
	cache func(s *PacketStore) map[string]*cachedResult
	// call drives the real wrapper with the i-th distinct key. Keys must sort
	// in insertion order so the newest key is never the smallest.
	call func(s *PacketStore, i int)
}

func wiringWindow(i int) TimeWindow {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	return TimeWindow{Since: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)}
}

// wiringArea is a distinct, unconfigured area key: resolveAreaNodes returns
// nil for it, so the cache key is distinct while the compute stays cheap.
func wiringArea(i int) string { return fmt.Sprintf("a%06d", i) }

// newWiringStore is the seeded test store, so every compute path runs for
// real; region stays empty so no key needs a configured region.
func newWiringStore(t *testing.T) *PacketStore {
	srv, _ := setupTestServer(t)
	return srv.store
}

func cachedResultFamilies() []cachedResultFamily {
	return []cachedResultFamily{
		{"rf", func(s *PacketStore) map[string]*cachedResult { return s.rfCache },
			func(s *PacketStore, i int) { s.GetAnalyticsRFWithWindow("", "", wiringWindow(i)) }},
		{"relay-airtime-share", func(s *PacketStore) map[string]*cachedResult { return s.rfCache },
			func(s *PacketStore, i int) { s.GetRelayAirtimeShareWithWindow(wiringWindow(i)) }},
		{"topology", func(s *PacketStore) map[string]*cachedResult { return s.topoCache },
			func(s *PacketStore, i int) { s.GetAnalyticsTopologyWithWindow("", "", wiringWindow(i)) }},
		{"channels", func(s *PacketStore) map[string]*cachedResult { return s.chanCache },
			func(s *PacketStore, i int) { s.GetAnalyticsChannelsWithWindow("", "", wiringWindow(i)) }},
		{"distance", func(s *PacketStore) map[string]*cachedResult { return s.distCache },
			func(s *PacketStore, i int) { s.GetAnalyticsDistance("", wiringArea(i)) }},
		{"hash-sizes", func(s *PacketStore) map[string]*cachedResult { return s.hashCache },
			func(s *PacketStore, i int) { s.GetAnalyticsHashSizes("", wiringArea(i)) }},
		{"hash-collisions", func(s *PacketStore) map[string]*cachedResult { return s.collisionCache },
			func(s *PacketStore, i int) { s.GetAnalyticsHashCollisions("", wiringArea(i)) }},
		{"subpaths", func(s *PacketStore) map[string]*cachedResult { return s.subpathCache },
			func(s *PacketStore, i int) { s.GetAnalyticsSubpaths("", 2, 2, 1000+i) }},
		{"subpaths-window", func(s *PacketStore) map[string]*cachedResult { return s.subpathCache },
			func(s *PacketStore, i int) { s.GetAnalyticsSubpathsWithWindow("", 2, 2, 10, wiringWindow(i)) }},
		{"subpaths-bulk", func(s *PacketStore) map[string]*cachedResult { return s.subpathCache },
			func(s *PacketStore, i int) {
				s.GetAnalyticsSubpathsBulk("", []subpathGroup{{MinLen: 2, MaxLen: 2, Limit: 1000 + i}})
			}},
	}
}

func TestAnalyticsCacheWiring_EachFamilyBounded(t *testing.T) {
	const n = analyticsCacheMaxEntries + 32
	for _, f := range cachedResultFamilies() {
		f := f
		t.Run(f.name, func(t *testing.T) {
			s := newWiringStore(t)
			for i := 0; i < n; i++ {
				f.call(s, i)
				s.cacheMu.Lock()
				got := len(f.cache(s))
				s.cacheMu.Unlock()
				if got > analyticsCacheMaxEntries {
					t.Fatalf("after %d distinct keys len=%d exceeds ceiling %d", i+1, got, analyticsCacheMaxEntries)
				}
			}
			s.cacheMu.Lock()
			m := f.cache(s)
			if len(m) != analyticsCacheMaxEntries {
				s.cacheMu.Unlock()
				t.Fatalf("len=%d want exactly the ceiling %d (keys not distinct?)", len(m), analyticsCacheMaxEntries)
			}
			// Re-fill the newest key: expire it so the wrapper misses and
			// writes it again at a full cache. No other live key may go.
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			newest := keys[len(keys)-1]
			m[newest].expiresAt = time.Now().Add(-time.Second)
			s.cacheMu.Unlock()

			f.call(s, n-1)

			s.cacheMu.Lock()
			defer s.cacheMu.Unlock()
			m = f.cache(s)
			for _, k := range keys {
				if _, ok := m[k]; !ok {
					t.Fatalf("refreshing %q evicted live key %q", newest, k)
				}
			}
			if e := m[newest]; e == nil || !time.Now().Before(e.expiresAt) {
				t.Fatalf("refreshed key %q not re-stored live", newest)
			}
			if len(m) != analyticsCacheMaxEntries {
				t.Fatalf("len=%d after refresh, want %d", len(m), analyticsCacheMaxEntries)
			}
		})
	}
}

func TestWardrivingStatsCacheWiring_Bounded(t *testing.T) {
	srv, router := setupTestServer(t)
	get := func(i int) {
		req := httptest.NewRequest(http.MethodGet, "/api/analytics/wardriving?window=24h&channel="+url.QueryEscape(fmt.Sprintf("#c%06d", i)), nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("channel %d: status %d: %s", i, w.Code, w.Body.String())
		}
	}
	const n = wardrivingStatsMaxEntries + 16
	for i := 0; i < n; i++ {
		get(i)
		srv.wardrivingStatsMu.Lock()
		got := len(srv.wardrivingStatsCache)
		srv.wardrivingStatsMu.Unlock()
		if got > wardrivingStatsMaxEntries {
			t.Fatalf("after %d channels len=%d exceeds ceiling %d", i+1, got, wardrivingStatsMaxEntries)
		}
	}
	srv.wardrivingStatsMu.Lock()
	if len(srv.wardrivingStatsCache) != wardrivingStatsMaxEntries {
		srv.wardrivingStatsMu.Unlock()
		t.Fatalf("len=%d want %d", len(srv.wardrivingStatsCache), wardrivingStatsMaxEntries)
	}
	keys := make([]string, 0, wardrivingStatsMaxEntries)
	for k := range srv.wardrivingStatsCache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	newest := keys[len(keys)-1]
	srv.wardrivingStatsCachedAt[newest] = time.Now().Add(-time.Hour) // older than the handler's 30s TTL
	srv.wardrivingStatsMu.Unlock()

	get(n - 1)

	srv.wardrivingStatsMu.Lock()
	defer srv.wardrivingStatsMu.Unlock()
	for _, k := range keys {
		if _, ok := srv.wardrivingStatsCache[k]; !ok {
			t.Fatalf("refreshing %q evicted live key %q", newest, k)
		}
	}
	if len(srv.wardrivingStatsCache) != wardrivingStatsMaxEntries {
		t.Fatalf("len=%d after refresh, want %d", len(srv.wardrivingStatsCache), wardrivingStatsMaxEntries)
	}
}

func TestPathInspectCacheWiring_Bounded(t *testing.T) {
	srv := newTestServerForInspect(t)
	srv.store.nodeCache = []nodeInfo{{PublicKey: "aabb1234", Name: "A", Role: "repeater"}}
	srv.store.nodePM = buildPrefixMap(srv.store.nodeCache)
	srv.store.nodeCacheTime = time.Now()
	post := func(i int) {
		body := fmt.Sprintf(`{"prefixes":["aa"],"context":{"observerId":"o%06d"}}`, i)
		if rr := doInspectRequest(srv, body); rr.Code != 200 {
			t.Fatalf("request %d: status %d: %s", i, rr.Code, rr.Body.String())
		}
	}
	const n = inspectCacheMaxEntries + 32
	for i := 0; i < n; i++ {
		post(i)
		srv.store.inspectMu.RLock()
		got := len(srv.store.inspectCache)
		srv.store.inspectMu.RUnlock()
		if got > inspectCacheMaxEntries {
			t.Fatalf("after %d keys len=%d exceeds ceiling %d", i+1, got, inspectCacheMaxEntries)
		}
	}
	srv.store.inspectMu.Lock()
	if len(srv.store.inspectCache) != inspectCacheMaxEntries {
		srv.store.inspectMu.Unlock()
		t.Fatalf("len=%d want %d", len(srv.store.inspectCache), inspectCacheMaxEntries)
	}
	keys := make([]string, 0, inspectCacheMaxEntries)
	for k := range srv.store.inspectCache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	newest := keys[len(keys)-1]
	srv.store.inspectCache[newest].expiresAt = time.Now().Add(-time.Second)
	srv.store.inspectMu.Unlock()

	post(n - 1)

	srv.store.inspectMu.RLock()
	defer srv.store.inspectMu.RUnlock()
	for _, k := range keys {
		if _, ok := srv.store.inspectCache[k]; !ok {
			t.Fatalf("refreshing %q evicted live key %q", newest, k)
		}
	}
}

func TestRegionObsCacheWiring_Bounded(t *testing.T) {
	srv, _ := setupTestServer(t)
	s := srv.store
	const n = regionObsCacheMaxEntries + 32
	for i := 0; i < n; i++ {
		s.resolveRegionObservers(fmt.Sprintf("R%06d", i))
		s.regionObsMu.Lock()
		got := len(s.regionObsCache)
		s.regionObsMu.Unlock()
		if got > regionObsCacheMaxEntries {
			t.Fatalf("after %d regions len=%d exceeds ceiling %d", i+1, got, regionObsCacheMaxEntries)
		}
	}
	s.regionObsMu.Lock()
	got := len(s.regionObsCache)
	keys := make([]string, 0, got)
	for k := range s.regionObsCache {
		keys = append(keys, k)
	}
	s.regionObsMu.Unlock()
	if got != regionObsCacheMaxEntries {
		t.Fatalf("len=%d want %d", got, regionObsCacheMaxEntries)
	}
	// A repeat lookup of a present region is served from the map and never
	// evicts another key.
	sort.Strings(keys)
	s.resolveRegionObservers(keys[len(keys)-1])
	s.regionObsMu.Lock()
	defer s.regionObsMu.Unlock()
	for _, k := range keys {
		if _, ok := s.regionObsCache[k]; !ok {
			t.Fatalf("repeat lookup evicted live key %q", k)
		}
	}
}

// analyticsWorkProbe snapshots every observable side effect of the expensive
// analytics paths: analytics cache hits/misses (every Get… wrapper bumps one),
// region observer resolution, the lazy distance-index build, the wardriving
// stats query and the neighbor-graph compute. A rejected request must leave
// it unchanged; the positive controls prove each handler moves it.
type analyticsWorkProbe struct {
	hits, misses   int64
	regionObs      int
	distTriggered  bool
	wardriving     int
	neighborGraphs int
}

func probeAnalyticsWork(srv *Server, neighborCalls *int) analyticsWorkProbe {
	s := srv.store
	var p analyticsWorkProbe
	s.cacheMu.Lock()
	p.hits, p.misses = s.cacheHits, s.cacheMisses
	s.cacheMu.Unlock()
	s.regionObsMu.Lock()
	p.regionObs = len(s.regionObsCache)
	s.regionObsMu.Unlock()
	s.distLazyMu.Lock()
	p.distTriggered = s.distLazyBuilding || s.distLazyBuilt
	s.distLazyMu.Unlock()
	srv.wardrivingStatsMu.Lock()
	p.wardriving = len(srv.wardrivingStatsCache)
	srv.wardrivingStatsMu.Unlock()
	p.neighborGraphs = *neighborCalls
	return p
}

func TestAnalyticsHandlers_RejectBeforeExpensiveWork(t *testing.T) {
	long := url.QueryEscape(strings.Repeat("A", analyticsFilterMaxLen+1))
	regionArea := []string{"region=" + long, "area=" + long, "area=atlantis"}
	regionOnly := []string{"region=" + long}
	cases := []struct {
		name   string
		path   string
		reject []string
		valid  string // positive control: a legitimate request that does work
	}{
		{"rf", "/api/analytics/rf", regionArea, "region=SJC"},
		{"topology", "/api/analytics/topology", regionArea, "region=SJC"},
		{"channels", "/api/analytics/channels", regionArea, "region=SJC"},
		{"distance", "/api/analytics/distance", regionArea, "region=SJC"},
		{"hash-sizes", "/api/analytics/hash-sizes", regionArea, "region=SJC"},
		{"hash-collisions", "/api/analytics/hash-collisions", regionArea, "region=SJC"},
		{"subpaths", "/api/analytics/subpaths", regionOnly, "region=SJC"},
		{"subpaths-bulk", "/api/analytics/subpaths-bulk", []string{"region=" + long + "&groups=2-2:5"}, "region=SJC&groups=2-2:5"},
		{"wardriving", "/api/analytics/wardriving", []string{"window=24h&channel=" + url.QueryEscape(strings.Repeat("x", analyticsFilterMaxLen+1))}, "window=24h&channel=%23wardriving"},
		{"neighbor-graph", "/api/analytics/neighbor-graph", regionOnly, "region=SJC"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.Areas = map[string]AreaEntry{"fyn": {}}
			neighborCalls := 0
			srv.computeNeighborGraphResponseFn = func(minCount int, minScore float64, region, role string) NeighborGraphResponse {
				neighborCalls++
				return NeighborGraphResponse{}
			}
			do := func(q string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, c.path+"?"+q, nil)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				return w
			}
			for _, q := range c.reject {
				before := probeAnalyticsWork(srv, &neighborCalls)
				w := do(q)
				if w.Code != 400 {
					t.Fatalf("%.40s…: want 400, got %d", q, w.Code)
				}
				if after := probeAnalyticsWork(srv, &neighborCalls); after != before {
					t.Fatalf("%.40s…: rejected request still did work: before %+v after %+v", q, before, after)
				}
			}
			before := probeAnalyticsWork(srv, &neighborCalls)
			if w := do(c.valid); w.Code == 400 {
				t.Fatalf("valid %q rejected: %s", c.valid, w.Body.String())
			}
			if after := probeAnalyticsWork(srv, &neighborCalls); after == before {
				t.Fatalf("positive control %q moved no probe (%+v): the probe cannot see this handler's work", c.valid, after)
			}
		})
	}
}

// TestHandleAnalyticsSubpaths_MaxLenFirmwareCeiling: the firmware encodes the
// hop count in bits 0-5 of path_length (firmware/docs/packet_format.md), so no
// path exceeds 63 hops. A 63-hop subpath must stay reachable, and any larger
// ?maxLen= must give the same result through the same cache key.
func TestHandleAnalyticsSubpaths_MaxLenFirmwareCeiling(t *testing.T) {
	srv, router := setupTestServer(t)
	hops := make([]string, 63)
	for i := range hops {
		hops[i] = fmt.Sprintf("%02x", i)
	}
	// A time window counts every subpath length of each transmission (the
	// unfiltered list stops at subpathMaxHops, #413), so the 63-hop path is
	// asked for through ?window=.
	pathJSON, _ := json.Marshal(hops)
	srv.store.mu.Lock()
	srv.store.packets = append(srv.store.packets, &StoreTx{
		ID: 1 << 30, Hash: "ceiling63", PathJSON: string(pathJSON),
		FirstSeen: time.Now().UTC().Format(time.RFC3339),
	})
	srv.store.mu.Unlock()

	get := func(maxLen string) []interface{} {
		req := httptest.NewRequest(http.MethodGet, "/api/analytics/subpaths?minLen=63&window=24h&maxLen="+maxLen, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("maxLen=%s: status %d", maxLen, w.Code)
		}
		var body struct {
			Subpaths []interface{} `json:"subpaths"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Subpaths
	}
	for _, ml := range []string{"63", "64", "1000"} {
		if got := get(ml); len(got) != 1 {
			t.Fatalf("maxLen=%s: want the 63-hop subpath, got %d subpaths", ml, len(got))
		}
	}
	srv.store.cacheMu.Lock()
	defer srv.store.cacheMu.Unlock()
	n := 0
	for k := range srv.store.subpathCache {
		if strings.HasPrefix(k, "|63|") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("maxLen 63/64/1000 produced %d cache keys, want 1 (clamped)", n)
	}
}

// TestEvictStatsCacheLocked_TieBreakSmallestKey pins the deterministic victim:
// with every entry stored at the same instant, the smallest key goes.
func TestEvictStatsCacheLocked_TieBreakSmallestKey(t *testing.T) {
	const max = 4
	now := time.Now()
	cache := map[string]*int{}
	at := map[string]time.Time{}
	for _, k := range []string{"d", "b", "a", "c"} {
		v := 0
		cache[k] = &v
		at[k] = now
	}
	evictStatsCacheLocked(cache, at, "z", now, time.Hour, max)
	if _, ok := cache["a"]; ok {
		t.Fatalf("tie: smallest key %q should be the victim, cache=%v", "a", at)
	}
	if len(cache) != max-1 || len(at) != max-1 {
		t.Fatalf("want one victim, len(cache)=%d len(at)=%d", len(cache), len(at))
	}
}
