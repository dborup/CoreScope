package main

import (
	"net/http"
	"time"
)

// Fixed entry ceilings for the public analytics response caches whose keys
// incorporate caller-controlled filters or time-window variants (issue #370).
//
// TTL expiry alone does not cap how many distinct keys a cache retains inside
// one TTL window: `/api/analytics/*?region=…&area=…&from=…&to=…` lets a client
// mint an unbounded number of distinct keys (absolute from/to ranges are not a
// closed alias set like ?window=), and each miss stored one more entry. That
// is the AGENTS.md rule 0 "no unbounded data structures" surface this file
// closes, mirroring the eviction already used by channelListCache.put and
// evictReachLocked: on a new key past the ceiling, drop expired entries first,
// then — if still full — a single deterministic victim. A refresh of an
// existing key never evicts a different live entry.

const (
	// analyticsCacheMaxEntries bounds each analytics response cache keyed by
	// region/area/window. Legitimate use touches a small set of region×area×
	// window combinations (the UI drives a handful); this is far above that
	// yet a hard cap against a client cycling distinct absolute windows. Each
	// entry holds one already-computed response map, so the bound is on count,
	// not per-entry size.
	analyticsCacheMaxEntries = 256

	// subpathMaxLenCeiling clamps the ?maxLen= hop count on the subpath
	// endpoints. It is part of the subpathCache key, and queryInt places no
	// upper bound on it, so an unclamped value is a second key-cardinality
	// vector beyond region. Real relay paths are a handful of hops; this is far
	// above any legitimate request yet stops unbounded distinct keys. The
	// subpathCache ceiling still backstops whatever gets through.
	subpathMaxLenCeiling = 32

	// analyticsFilterMaxLen caps a region/area/channel filter value, rejected
	// before any cache/index/graph work. A normalized region set is comma-
	// joined upper-case IATA codes and an area/channel key is short, so no
	// legitimate selection approaches this; it bounds both the wasted compute
	// and the stored key size. Mirrors reachRankMaxQueryLen's handler-side
	// length guard.
	analyticsFilterMaxLen = 128

	// wardrivingStatsMaxEntries bounds wardrivingStatsCache, whose key is
	// window|channel with a free-text channel. The window is a 3-value
	// whitelist, so legitimate use needs only a few entries per real channel;
	// this caps a client cycling distinct channel filters.
	wardrivingStatsMaxEntries = 64

	// regionObsCacheMaxEntries bounds regionObsCache (region → observer-id set).
	// The whole map already resets every 30s, but within one window a client
	// cycling distinct region strings accumulated entries without limit.
	regionObsCacheMaxEntries = 256

	// inspectCacheMaxEntries bounds inspectCache, keyed by caller path prefixes
	// plus observer/since/until. It already drops expired entries on write;
	// this adds the entry ceiling that TTL alone does not provide.
	inspectCacheMaxEntries = 256
)

// evictCachedResultLocked makes room for a NEW key in m. It is a no-op when key
// already exists (refreshing a live entry must never evict a different one) or
// m is below the ceiling. Otherwise every expired entry is dropped; if that
// still leaves m at the ceiling, the single entry nearest expiry is evicted,
// ties broken by the smallest key so the victim is deterministic regardless of
// Go map iteration order. Caller holds the cache's write lock.
func evictCachedResultLocked(m map[string]*cachedResult, key string) {
	if _, exists := m[key]; exists || len(m) < analyticsCacheMaxEntries {
		return
	}
	now := time.Now()
	for k, v := range m {
		if now.After(v.expiresAt) {
			delete(m, k)
		}
	}
	if len(m) < analyticsCacheMaxEntries {
		return
	}
	victim := ""
	var soonest time.Time
	first := true
	for k, v := range m {
		switch {
		case first, v.expiresAt.Before(soonest):
			victim, soonest, first = k, v.expiresAt, false
		case v.expiresAt.Equal(soonest) && k < victim:
			victim = k
		}
	}
	if !first {
		delete(m, victim)
	}
}

// evictStatsCacheLocked bounds a per-window stats cache that tracks expiry in a
// sibling `at` map (the routes.go pattern), mirroring evictCachedResultLocked:
// a no-op when newKey already exists (a refresh never evicts a live key) or the
// cache is below max; otherwise entries older than ttl are dropped first, then —
// if still at the ceiling — the single oldest, ties broken by the smallest key.
// Deletes from both maps together. Caller holds the cache's lock.
func evictStatsCacheLocked[T any](cache map[string]*T, at map[string]time.Time, newKey string, now time.Time, ttl time.Duration, max int) {
	if _, exists := cache[newKey]; exists || len(cache) < max {
		return
	}
	for k, t := range at {
		if now.Sub(t) >= ttl {
			delete(cache, k)
			delete(at, k)
		}
	}
	if len(cache) < max {
		return
	}
	victim := ""
	var oldest time.Time
	first := true
	for k, t := range at {
		switch {
		case first, t.Before(oldest):
			victim, oldest, first = k, t, false
		case t.Equal(oldest) && k < victim:
			victim = k
		}
	}
	if !first {
		delete(cache, victim)
		delete(at, victim)
	}
}

// evictInspectCacheLocked bounds inspectCache. It always drops expired entries
// (relative to now), then — only when inserting a NEW key would exceed
// inspectCacheMaxEntries — evicts the single entry nearest expiry, ties broken
// by the smallest key for a deterministic victim. Refreshing an existing key
// never evicts another entry. Caller holds inspectMu.
func evictInspectCacheLocked(m map[string]*inspectCachedResult, key string, now time.Time) {
	for k, v := range m {
		if now.After(v.expiresAt) {
			delete(m, k)
		}
	}
	if _, exists := m[key]; exists || len(m) < inspectCacheMaxEntries {
		return
	}
	victim := ""
	var soonest time.Time
	first := true
	for k, v := range m {
		switch {
		case first, v.expiresAt.Before(soonest):
			victim, soonest, first = k, v.expiresAt, false
		case v.expiresAt.Equal(soonest) && k < victim:
			victim = k
		}
	}
	if !first {
		delete(m, victim)
	}
}

// evictRegionObsLocked bounds regionObsCache for a new region key. The cache
// has no per-entry timestamp (the whole map shares regionObsCacheTime and is
// reset wholesale every 30s), so the deterministic victim is the smallest key.
// A no-op when newKey already exists or the map is below the ceiling. Caller
// holds regionObsMu.
func (s *PacketStore) evictRegionObsLocked(newKey string) {
	if _, exists := s.regionObsCache[newKey]; exists || len(s.regionObsCache) < regionObsCacheMaxEntries {
		return
	}
	victim := ""
	first := true
	for k := range s.regionObsCache {
		if first || k < victim {
			victim, first = k, false
		}
	}
	if !first {
		delete(s.regionObsCache, victim)
	}
}

// putCachedResultLocked stores result under key in m with a ttl-bounded expiry,
// enforcing analyticsCacheMaxEntries. Caller holds the cache's write lock
// (s.cacheMu for the store.go analytics caches).
func putCachedResultLocked(m map[string]*cachedResult, key string, result map[string]interface{}, ttl time.Duration) {
	evictCachedResultLocked(m, key)
	m[key] = &cachedResult{data: result, expiresAt: time.Now().Add(ttl)}
}

// analyticsFilterError validates the region/area query parameters shared by the
// analytics handlers before any cache/index/graph work. It returns a non-empty
// 400 message when a filter is too long or names an area that is not a
// configured area; "" means the selection is legitimate (including the empty
// region/area that means "global"). areas is s.cfg.Areas (nil when none are
// configured, in which case any non-empty area is unknown).
func analyticsFilterError(region, area string, areas map[string]AreaEntry) string {
	if len(region) > analyticsFilterMaxLen {
		return "region filter too long"
	}
	if len(area) > analyticsFilterMaxLen {
		return "area filter too long"
	}
	if area != "" {
		if _, ok := areas[area]; !ok {
			return "unknown area"
		}
	}
	return ""
}

// rejectAnalyticsFilters writes a 400 and returns true when the region/area
// query parameters are invalid, so the analytics handlers can bail out before
// any cache/index/graph work. It returns false (and writes nothing) for a
// legitimate selection.
func (s *Server) rejectAnalyticsFilters(w http.ResponseWriter, region, area string) bool {
	var areas map[string]AreaEntry
	if s.cfg != nil {
		areas = s.cfg.Areas
	}
	if msg := analyticsFilterError(region, area, areas); msg != "" {
		writeError(w, 400, msg)
		return true
	}
	return false
}
