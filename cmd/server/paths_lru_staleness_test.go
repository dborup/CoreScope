package main

import (
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
