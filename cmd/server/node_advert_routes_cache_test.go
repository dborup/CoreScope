package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The #2073 advert route breakdown is cached per pubkey: bounded in size,
// expired by TTL, and invalidated for that pubkey alone when the node has a
// newer transmission (debounced, so a spamming node recomputes at most once
// per nodeAdvertRouteDebounce).

func narCacheServer(t *testing.T) *Server {
	t.Helper()
	db := narDB(t)
	return NewServer(db, &Config{}, NewHub())
}

func TestNodeAdvertRouteCache_HitAndTTL(t *testing.T) {
	s := narCacheServer(t)
	id := narInsert(t, s.db, narNode, "c-a", payloadTypeAdvert, 1, 0b0010, narAgo(time.Hour), true)
	t0 := time.Now()
	br, _, err := s.nodeAdvertRoutes(narNode, t0)
	if err != nil || len(br.Flood) != 1 {
		t.Fatalf("first read: flood=%d err=%v", len(br.Flood), err)
	}
	// A route_mask update does not change the node's latest id: served from
	// the cache until the TTL expires.
	if _, err := s.db.conn.Exec(`UPDATE transmissions SET route_mask = 6 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	br, _, _ = s.nodeAdvertRoutes(narNode, t0.Add(nodeAdvertRouteTTL-time.Second))
	if len(br.Flood) != 1 || len(br.Mixed) != 0 {
		t.Fatalf("within TTL want cached flood=1 mixed=0, got flood=%d mixed=%d", len(br.Flood), len(br.Mixed))
	}
	br, counts, _ := s.nodeAdvertRoutes(narNode, t0.Add(nodeAdvertRouteTTL+time.Second))
	if len(br.Flood) != 0 || len(br.Mixed) != 1 {
		t.Fatalf("after TTL want recomputed flood=0 mixed=1, got flood=%d mixed=%d", len(br.Flood), len(br.Mixed))
	}
	if counts.RouteMaskBackfill.Status == "" {
		t.Fatal("route_mask_backfill must be set on every read")
	}
}

func TestNodeAdvertRouteCache_TargetedInvalidation(t *testing.T) {
	s := narCacheServer(t)
	const other = "2073bb0011223344556677889900aabbccddeeff00112233445566778899aabb"
	narInsert(t, s.db, narNode, "t-a", payloadTypeAdvert, 1, 0b0010, narAgo(time.Hour), true)
	narInsert(t, s.db, other, "t-b", payloadTypeAdvert, 1, 0b0010, narAgo(time.Hour), true)
	t0 := time.Now()
	s.nodeAdvertRoutes(narNode, t0)
	s.nodeAdvertRoutes(other, t0)

	narInsert(t, s.db, narNode, "t-a2", payloadTypeAdvert, 2, 0b0100, narAgo(time.Minute), true)
	// Inside the debounce window a newer advert does not force a rescan yet.
	br, _, _ := s.nodeAdvertRoutes(narNode, t0.Add(nodeAdvertRouteDebounce/2))
	if len(br.ZeroHop) != 0 {
		t.Fatalf("within debounce want cached result, got zero_hop=%d", len(br.ZeroHop))
	}
	// Past it, the node's newer transmission invalidates its entry only.
	if _, err := s.db.conn.Exec(`UPDATE transmissions SET route_mask = 6 WHERE hash = 't-b'`); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(nodeAdvertRouteDebounce + time.Second)
	br, counts, _ := s.nodeAdvertRoutes(narNode, later)
	if len(br.ZeroHop) != 1 || counts.H24.ZeroHop != 1 {
		t.Fatalf("after invalidation want zero_hop list=1 count=1, got %d/%d", len(br.ZeroHop), counts.H24.ZeroHop)
	}
	br, _, _ = s.nodeAdvertRoutes(other, later)
	if len(br.Flood) != 1 || len(br.Mixed) != 0 {
		t.Fatalf("other node must stay cached (flood=1 mixed=0), got flood=%d mixed=%d", len(br.Flood), len(br.Mixed))
	}
}

func TestNodeAdvertRouteCache_SizeBound(t *testing.T) {
	var c nodeAdvertRouteCache
	t0 := time.Now()
	for i := 0; i < nodeAdvertRouteCacheMax+10; i++ {
		at := t0.Add(time.Duration(i) * time.Millisecond)
		c.put(fmt.Sprintf("pk%03d", i), nodeAdvertRouteEntry{latestID: 1, at: at}, at)
	}
	if n := c.len(); n != nodeAdvertRouteCacheMax {
		t.Fatalf("cache holds %d entries, want the bound %d", n, nodeAdvertRouteCacheMax)
	}
	if _, ok := c.get("pk000", 1, t0); ok {
		t.Fatal("oldest entry should have been evicted")
	}
	if _, ok := c.get(fmt.Sprintf("pk%03d", nodeAdvertRouteCacheMax+9), 1, t0.Add(time.Second)); !ok {
		t.Fatal("newest entry should be cached")
	}
}

// put drops expired entries, so a cache that filled up once does not keep
// holding stale breakdowns until the same pubkeys are read again.
func TestNodeAdvertRouteCache_PutSweepsExpired(t *testing.T) {
	var c nodeAdvertRouteCache
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		c.put(fmt.Sprintf("old%02d", i), nodeAdvertRouteEntry{latestID: 1, at: t0}, t0)
	}
	later := t0.Add(nodeAdvertRouteTTL + time.Second)
	c.put("fresh", nodeAdvertRouteEntry{latestID: 1, at: later}, later)
	if n := c.len(); n != 1 {
		t.Fatalf("cache holds %d entries after the others expired, want 1", n)
	}
}

// Concurrent misses for one node share a single scan.
func TestNodeAdvertRouteCache_ConcurrentMissesShareOneScan(t *testing.T) {
	s := narCacheServer(t)
	narInsert(t, s.db, narNode, "sf-a", payloadTypeAdvert, 1, 0b0010, narAgo(time.Hour), true)
	var scans atomic.Int32
	release := make(chan struct{})
	s.advertRoutes.onScan = func() { scans.Add(1); <-release }
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if br, _, err := s.nodeAdvertRoutes(narNode, now); err != nil || len(br.Flood) != 1 {
				t.Errorf("flood=%d err=%v", len(br.Flood), err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond) // let the goroutines join the flight
	close(release)
	wg.Wait()
	if n := scans.Load(); n != 1 {
		t.Fatalf("%d scans for 8 concurrent misses, want 1", n)
	}
}
