package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// #277: /paths and /hop_analytics decide membership from the canonical
// resolved_path (fetchResolvedPathForTxBest), which is served from
// apiResolvedPathLRU first. The ingestor's observation upsert
// (ON CONFLICT ... resolved_path = COALESCE(excluded.resolved_path,
// resolved_path)) can replace a stored path in place, keeping the row id, so
// the server's poll loop (WHERE o.id > ?) never sees the change. Nothing
// invalidated the LRU entry either, so the endpoints answered from the old
// path for as long as the entry survived FIFO eviction.

// fakeLRUClock installs a controllable clock on the store's LRU and returns a
// function that moves it forward.
func fakeLRUClock(store *PacketStore) (advance func(time.Duration)) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	store.lruClock = func() time.Time { return time.Unix(0, now.Load()) }
	return func(d time.Duration) { now.Add(int64(d)) }
}

// nodeEndpointBody GETs /api/nodes/{target}/{endpoint} and returns the body.
func nodeEndpointBody(t *testing.T, router http.Handler, endpoint string) string {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+confirmTestTarget+"/"+endpoint, nil))
	if w.Code != 200 {
		t.Fatalf("%s: expected 200, got %d: %s", endpoint, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// rewriteStoredResolvedPath does what the ingestor's upsert does when a
// re-delivered observation resolves differently: the row keeps its id and
// only resolved_path changes.
func rewriteStoredResolvedPath(t *testing.T, srv *Server, txID int, rpJSON string) {
	t.Helper()
	if _, err := srv.db.conn.Exec(`UPDATE observations SET resolved_path = ? WHERE transmission_id = ?`, rpJSON, txID); err != nil {
		t.Fatalf("rewrite resolved_path: %v", err)
	}
}

func TestNodePaths_RewrittenResolvedPathNotServedFromStaleLRU(t *testing.T) {
	srv, router := setupTestServer(t)
	const hash = "lru_rewrite_hash"
	txID := seedConfirmTx(t, srv, hash, `["aa","bb"]`, `["`+confirmTestTarget+`","eeff00112233aabb"]`)
	store := reloadConfirmStore(t, srv)
	advance := fakeLRUClock(store)

	// Warm the LRU: the stored path goes through the target.
	for _, ep := range []string{"paths", "hop_analytics"} {
		if !strings.Contains(nodeEndpointBody(t, router, ep), hash) {
			t.Fatalf("%s: tx missing before the rewrite (setup)", ep)
		}
	}

	// The ingestor replaces the stored path with one that does not contain
	// the target. The in-memory hash index still lists the tx under the
	// target, so the canonical path is what decides.
	rewriteStoredResolvedPath(t, srv, txID, `["aacafe0000000000","eeff00112233aabb"]`)
	advance(time.Hour)

	for _, ep := range []string{"paths", "hop_analytics"} {
		if strings.Contains(nodeEndpointBody(t, router, ep), hash) {
			t.Errorf("%s: tx still attributed to the target an hour after its stored resolved_path stopped containing it (stale LRU entry)", ep)
		}
	}
}

// The bound is a trade-off, not an invalidation: inside resolvedPathLRUTTL the
// cached path is still served without touching SQLite. This pins that the
// cache keeps doing its job (a TTL of 0, or a lookup that ignores the entry,
// would make every /paths candidate a primary-key read again).
func TestNodePaths_ResolvedPathLRUServesCachedEntryWithinTTL(t *testing.T) {
	srv, router := setupTestServer(t)
	const hash = "lru_within_ttl_hash"
	txID := seedConfirmTx(t, srv, hash, `["aa","bb"]`, `["`+confirmTestTarget+`","eeff00112233aabb"]`)
	store := reloadConfirmStore(t, srv)
	advance := fakeLRUClock(store)

	if !strings.Contains(nodeEndpointBody(t, router, "paths"), hash) {
		t.Fatal("tx missing before the rewrite (setup)")
	}
	rewriteStoredResolvedPath(t, srv, txID, `["aacafe0000000000","eeff00112233aabb"]`)

	// An entry must live long enough to absorb repeated dashboard requests.
	advance(30 * time.Second)
	if !strings.Contains(nodeEndpointBody(t, router, "paths"), hash) {
		t.Fatal("cached resolved_path was re-read after 30 s; the LRU no longer saves repeat reads")
	}
	advance(resolvedPathLRUTTL - 31*time.Second)
	if !strings.Contains(nodeEndpointBody(t, router, "paths"), hash) {
		t.Error("cached resolved_path was re-read before resolvedPathLRUTTL elapsed")
	}
	advance(2 * time.Second)
	if strings.Contains(nodeEndpointBody(t, router, "paths"), hash) {
		t.Error("cached resolved_path still served after resolvedPathLRUTTL elapsed")
	}
}

func TestResolvedPathLRU_ExpiryAndInPlaceRefresh(t *testing.T) {
	store := &PacketStore{}
	store.initResolvedPathIndex()
	advance := fakeLRUClock(store)
	a, b := "aa", "bb"

	store.lruMu.Lock()
	store.lruPut(7, []*string{&a})
	store.lruMu.Unlock()
	if rp, ok := store.lruGet(7, store.lruNow()); !ok || *rp[0] != "aa" {
		t.Fatalf("fresh entry not served: ok=%v", ok)
	}

	advance(resolvedPathLRUTTL + time.Nanosecond)
	if _, ok := store.lruGet(7, store.lruNow()); ok {
		t.Fatal("entry older than resolvedPathLRUTTL served")
	}

	// A re-read after expiry must replace the value and restart its age,
	// without a second FIFO slot for the same id.
	store.lruMu.Lock()
	store.lruPut(7, []*string{&b})
	store.lruMu.Unlock()
	if rp, ok := store.lruGet(7, store.lruNow()); !ok || *rp[0] != "bb" {
		t.Fatalf("refreshed entry not served with the new value: ok=%v", ok)
	}
	if n := len(store.lruOrder); n != 1 {
		t.Errorf("lruOrder has %d slots for one id, want 1", n)
	}

	// /paths reads the clock once per request, so a lookup's now can be
	// older than an entry stored meanwhile: that entry is fresh.
	if _, ok := store.lruGet(7, store.lruNow()-int64(time.Second)); !ok {
		t.Error("entry stored after the lookup's clock read was treated as expired")
	}
}

// Every staleness test installs lruClock, so this is the only test of the
// production clock. Its unit must be nanoseconds: lruGet compares ages with
// int64(resolvedPathLRUTTL), so a clock in ms or µs would stretch the 60 s
// TTL to days or hours while the hooked tests stay green.
func TestResolvedPathLRU_DefaultClockIsMonotonicNanoseconds(t *testing.T) {
	store := &PacketStore{}
	start := time.Now()
	a := store.lruNow()
	time.Sleep(time.Millisecond)
	b := store.lruNow()
	outer := time.Since(start)
	if b <= a || b > int64(time.Hour) {
		t.Errorf("lruNow went %d -> %d; want an increasing offset from lruEpoch", a, b)
	}
	// [a, b] lies inside [start, start+outer] on the same monotonic clock.
	if d := b - a; d < int64(time.Millisecond) || d > int64(outer) {
		t.Errorf("lruNow advanced %d across a 1 ms sleep (outer interval %d ns); want nanoseconds", d, outer.Nanoseconds())
	}
}

// lruClock stands in for time.Now, so the hook and the default clock must
// measure in the same domain: installing the hook while entries exist must
// neither expire nor revive them.
func TestResolvedPathLRU_ClockHookSharesDefaultClockDomain(t *testing.T) {
	store := &PacketStore{}
	store.initResolvedPathIndex()
	a := "aa"

	store.lruMu.Lock()
	store.lruPut(7, []*string{&a}) // stored on the default clock
	store.lruMu.Unlock()

	store.lruClock = time.Now
	if _, ok := store.lruGet(7, store.lruNow()); !ok {
		t.Fatal("entry stored on the default clock expired once lruClock = time.Now was installed")
	}
	store.lruClock = func() time.Time { return time.Now().Add(resolvedPathLRUTTL + time.Second) }
	if _, ok := store.lruGet(7, store.lruNow()); ok {
		t.Error("entry stored on the default clock still served after the hook moved past resolvedPathLRUTTL")
	}
}

// Entry ages cost a clock read, which is not free on every host (about 30 ns
// per read on a kvm-clock VM, against 15 ns for the whole cache hit). /paths
// and /hop_analytics look up one entry per candidate, so they read the clock
// once per request. A warm request with many candidates must read it once.
func TestNodePaths_WarmRequestReadsLRUClockOnce(t *testing.T) {
	srv, router := setupTestServer(t)
	for i := 0; i < 20; i++ {
		seedConfirmTx(t, srv, fmt.Sprintf("lru_clock_%02d", i), `["aa","bb"]`, `["`+confirmTestTarget+`","eeff00112233aabb"]`)
	}
	store := reloadConfirmStore(t, srv)
	var reads atomic.Int64
	base := time.Now()
	store.lruClock = func() time.Time { reads.Add(1); return base }

	for _, ep := range []string{"paths", "hop_analytics"} {
		nodeEndpointBody(t, router, ep) // warm
		reads.Store(0)
		body := nodeEndpointBody(t, router, ep)
		// /paths groups identical paths (one sample hash per group), so
		// count rows/occurrences rather than look for each hash.
		if ep == "paths" {
			var resp confirmPathsResp
			if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.TotalTransmissions < 20 {
				t.Fatalf("paths: totalTransmissions = %d (err %v), want >= 20 (setup)", resp.TotalTransmissions, err)
			}
		} else if strings.Count(body, "lru_clock_") < 20 {
			t.Fatalf("%s: seeded txs missing (setup)", ep)
		}
		if n := reads.Load(); n != 1 {
			t.Errorf("%s: warm request read the LRU clock %d times for 20+ candidates, want 1", ep, n)
		}
	}
}
