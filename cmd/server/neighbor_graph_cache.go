package main

import (
	"bytes"
	"encoding/json"
	"log"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"
)

// #1481 P0-1: cached default-filter neighbor-graph response.
//
// The /api/analytics/neighbor-graph handler does graph build + per-edge
// score + filter + ~900KB JSON marshal on every request. The default
// (no-region, no-role, minCount=5, minScore=0.1) shape covers the
// overwhelming majority of organic traffic; cache the fully-built AND
// pre-marshaled response so warm reads are a single Write. Recomputed
// every 5 minutes in the background — never on the hot path.

const neighborGraphCacheInterval = 5 * time.Minute

// neighborGraphRebuildEvery returns how many recompute ticks make up one graph
// rebuild: ceil(neighborGraphTTL / interval), at least 1. The response cache is
// refreshed on every tick, but the ~seconds-long graph build runs at most once
// per neighborGraphTTL, so cacheRecomputeIntervalSeconds neither multiplies the
// rebuild CPU nor changes the graph's age below the TTL. Intervals at or above
// the TTL rebuild on every tick (graph age == interval).
func neighborGraphRebuildEvery(interval time.Duration) int {
	if interval <= 0 || interval >= neighborGraphTTL {
		return 1
	}
	return int((neighborGraphTTL + interval - 1) / interval)
}

// neighborGraphCacheEntry holds both the response struct (kept for
// tests / structured access) and the pre-marshaled bytes that the
// handler writes verbatim.
type neighborGraphCacheEntry struct {
	resp NeighborGraphResponse
	json []byte
	at   time.Time
}

type neighborGraphCacheField struct {
	ptr atomic.Pointer[neighborGraphCacheEntry]
	// unfiltered = the (minCount=1, minScore=0, no region/role) shape
	// the analytics tab actually hits. Cached separately so the UI
	// tab also benefits from the warm path; client-side sliders then
	// filter from full data. #1483 follow-up to perf claim.
	unfilteredPtr atomic.Pointer[neighborGraphCacheEntry]
}

// startNeighborGraphRecomputer launches a background goroutine that
// rebuilds the default-shape response every interval. Returns when
// the stop channel is closed.
func (s *Server) startNeighborGraphRecomputer(interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = neighborGraphCacheInterval
	}
	every := neighborGraphRebuildEvery(interval)
	go func() {
		s.recomputeNeighborGraphCache()
		t := time.NewTicker(interval)
		defer t.Stop()
		runNeighborGraphLoop(t.C, stop, every, s.recomputeNeighborGraphCacheTick)
	}()
}

// shouldRebuildNeighborGraph reports whether recompute tick (1-based) also
// rebuilds the graph, given neighborGraphRebuildEvery's cadence. With every ==
// 1 (the default 300 s interval) every tick rebuilds.
func shouldRebuildNeighborGraph(tick, every int) bool {
	if every <= 1 {
		return true
	}
	return tick%every == 0
}

// runNeighborGraphLoop calls step once per tick until stop closes, telling it
// whether this tick rebuilds the graph. Split out so tests can drive it with a
// fake tick channel.
func runNeighborGraphLoop(ticks <-chan time.Time, stop <-chan struct{}, every int, step func(rebuildGraph bool)) {
	for tick := 1; ; tick++ {
		select {
		case <-ticks:
			step(shouldRebuildNeighborGraph(tick, every))
		case <-stop:
			return
		}
	}
}

// recomputeNeighborGraphCache builds and pre-marshals the default-shape
// response and atomically swaps it in. Panic-defensive so a single bad
// rebuild doesn't kill the background goroutine — but logs the panic
// and increments a counter so operators see the failure (#1483 follow-up).
func (s *Server) recomputeNeighborGraphCache() {
	s.recomputeNeighborGraphCacheTick(true)
}

// recomputeNeighborGraphCacheTick refreshes the cached responses, first
// rebuilding the graph when rebuildGraph is set. The responses are always built
// from the graph published by this call (or the existing one when skipped).
func (s *Server) recomputeNeighborGraphCacheTick(rebuildGraph bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[neighbor-graph-cache] rebuild panic: %v\n%s", r, debug.Stack())
			atomic.AddUint64(&s.neighborGraphCacheRebuildFailures, 1)
		}
	}()
	start := time.Now()
	if rebuildGraph {
		s.rebuildNeighborGraph()
	}
	resp := s.buildDefaultNeighborGraphResponse()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		log.Printf("[neighbor-graph-cache] marshal error: %v", err)
		atomic.AddUint64(&s.neighborGraphCacheRebuildFailures, 1)
		return
	}
	s.neighborGraphCache.ptr.Store(&neighborGraphCacheEntry{
		resp: resp,
		json: buf.Bytes(),
		at:   time.Now(),
	})
	log.Printf("[neighbor-graph-cache] rebuild ok in %v, nodes=%d", time.Since(start), len(resp.Nodes))

	// Build + cache the analytics-tab shape (minCount=1, minScore=0).
	// This is what the UI actually fetches so it can slider client-side.
	// Cached separately so its TTL stays aligned with the default cache.
	uStart := time.Now()
	uResp := s.computeNeighborGraphResponseDispatch(1, 0, "", "")
	var uBuf bytes.Buffer
	if err := json.NewEncoder(&uBuf).Encode(uResp); err != nil {
		log.Printf("[neighbor-graph-cache] unfiltered marshal error: %v", err)
		atomic.AddUint64(&s.neighborGraphCacheRebuildFailures, 1)
		return
	}
	s.neighborGraphCache.unfilteredPtr.Store(&neighborGraphCacheEntry{
		resp: uResp,
		json: uBuf.Bytes(),
		at:   time.Now(),
	})
	log.Printf("[neighbor-graph-cache] unfiltered rebuild ok in %v, nodes=%d", time.Since(uStart), len(uResp.Nodes))
}

// loadNeighborGraphCache returns the cached default response if present.
func (s *Server) loadNeighborGraphCache() (NeighborGraphResponse, bool) {
	e := s.neighborGraphCache.ptr.Load()
	if e == nil {
		return NeighborGraphResponse{}, false
	}
	return e.resp, true
}

// loadNeighborGraphCacheBytes returns the pre-marshaled JSON for the
// cached default response if present, along with the age of the
// snapshot (zero when no entry is present).
func (s *Server) loadNeighborGraphCacheBytes() ([]byte, time.Duration, bool) {
	e := s.neighborGraphCache.ptr.Load()
	if e == nil || len(e.json) == 0 {
		return nil, 0, false
	}
	age := time.Duration(0)
	if !e.at.IsZero() {
		age = time.Since(e.at)
	}
	return e.json, age, true
}

// loadNeighborGraphCacheBytesUnfiltered returns the pre-marshaled JSON
// for the (minCount=1, minScore=0) cache shape used by the analytics
// tab. #1483 follow-up.
func (s *Server) loadNeighborGraphCacheBytesUnfiltered() ([]byte, time.Duration, bool) {
	e := s.neighborGraphCache.unfilteredPtr.Load()
	if e == nil || len(e.json) == 0 {
		return nil, 0, false
	}
	age := time.Duration(0)
	if !e.at.IsZero() {
		age = time.Since(e.at)
	}
	return e.json, age, true
}

// cacheAgeSecondsHeader formats a time.Duration as integer seconds for
// the X-Cache-Age-Seconds response header.
func cacheAgeSecondsHeader(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(int64(d/time.Second), 10)
}
