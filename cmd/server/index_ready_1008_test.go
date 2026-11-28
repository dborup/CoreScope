// Issue #1008: the pathHop index build must move off the synchronous
// Load() critical path into a background goroutine.
//
// Contract:
//   1. Immediately after Load() returns, PathHopIndexReady() reports
//      false (the goroutine has not finished).
//   2. Handlers that depend on the index respond 503 with Retry-After: 5
//      until the ready flag flips true.
//   3. After the background build completes (waitable via a helper),
//      the flag flips true and handlers respond 200.
//
// Route Patterns (subpaths) had their own index and gate until #413; they
// are counted on demand and answer as soon as Load() returns.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// TestIssue1008_PathHopIndexReadyFalseImmediatelyAfterLoad asserts the
// path-hop ready flag is false the instant Load() returns: the flag is owned
// by the background goroutine, which has not yet run.
func TestIssue1008_PathHopIndexReadyFalseImmediatelyAfterLoad(t *testing.T) {
	db := setupRichTestDB(t)
	defer db.Close()
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if store.PathHopIndexReady() {
		t.Fatal("expected PathHopIndexReady()==false immediately after Load(); want background-deferred build (#1008)")
	}
}

// TestIssue1008_HandlerReturns503WhilePathHopIndexLoading asserts that
// /api/nodes/{pubkey}/paths returns 503 + Retry-After: 5 + a JSON body
// matching the triage spec while the path-hop index is still building, and
// 200 once it is built — and that Route Patterns, which keep no index
// (#413), are served with their counts throughout.
//
// The background build of the small fixture used to finish before the
// request under CPU load, so the 503 was a race the test could lose (#227).
// pathHopBuildGate now holds the build until the 503 has been asserted.
func TestIssue1008_HandlerReturns503WhilePathHopIndexLoading(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)
	store := NewPacketStore(db, nil)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBuild := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseBuild) // a failed assertion must not strand the builder
	building := make(chan struct{})
	store.pathHopBuildGate = func() { close(building); <-release }
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	select {
	case <-building: // the build has started and is held
	case <-time.After(5 * time.Second):
		t.Fatal("background path-hop index build never started")
	}
	cfg := &Config{}
	cfg.applyListLimitsDefaults()
	srv := NewServer(db, cfg, NewHub())
	srv.store = store
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	get := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		return rec
	}
	const nodePaths = "/api/nodes/aabbccdd11223344/paths"

	rec := get(nodePaths)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (path-hop index loading, #1008)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After header = %q, want %q", got, "5")
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not valid JSON: %v (body=%s)", err, rec.Body.String())
	}
	if body["error"] != "index loading" {
		t.Errorf(`body["error"] = %v, want "index loading"`, body["error"])
	}

	// Route Patterns do not wait for any build (#413).
	for _, url := range []string{
		"/api/analytics/subpaths?minLen=2&maxLen=4&limit=10",
		"/api/analytics/subpaths-bulk?groups=2-2:10",
		"/api/analytics/subpath-detail?hops=aa,bb",
	} {
		if rec := get(url); rec.Code != http.StatusOK {
			t.Errorf("%s while the path-hop index builds: status %d, want 200", url, rec.Code)
		}
	}
	var subpaths SubpathsResponse
	if err := json.Unmarshal(get("/api/analytics/subpaths?minLen=2&maxLen=4&limit=10").Body.Bytes(), &subpaths); err != nil {
		t.Fatal(err)
	}
	if subpaths.TotalPaths == 0 || len(subpaths.Subpaths) == 0 {
		t.Errorf("subpaths while the path-hop index builds = %+v, want the counted paths", subpaths)
	}

	// Released, the build completes and the same request is served.
	releaseBuild()
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("index not ready within 5s of releasing the build")
	}
	if rec := get(nodePaths); rec.Code != http.StatusOK {
		t.Fatalf("status after the build = %d, want 200", rec.Code)
	}
}

// TestIssue1008_HandlerRecoversAfterIndexReady asserts that, once the
// background build completes, the handler returns 200.
func TestIssue1008_HandlerRecoversAfterIndexReady(t *testing.T) {
	db := setupRichTestDB(t)
	defer db.Close()
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Wait up to 5s for the background build to finish on this small
	// fixture (rich test DB has ~3 packets; build is sub-millisecond).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store.PathHopIndexReady() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !store.PathHopIndexReady() {
		t.Fatal("PathHopIndexReady() never flipped true within 5s")
	}

	cfg := &Config{}
	cfg.applyListLimitsDefaults()
	srv := &Server{store: store, cfg: cfg}
	req := httptest.NewRequest("GET", "/api/analytics/subpaths?minLen=2&maxLen=4&limit=10", nil)
	rec := httptest.NewRecorder()
	srv.handleAnalyticsSubpaths(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status after ready = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestIssue1008_m7_BothFlagsSetAfterParallelStart verifies that
// startBackgroundIndexBuilds sets the ready flag after a bounded wait.
// Review m7 split the subpath and pathHop builds into two goroutines;
// #413 removed the subpath build, and this is the sanity check that the
// pathHop flag flip survived both changes.
func TestIssue1008_m7_BothFlagsSetAfterParallelStart(t *testing.T) {
	db := setupRichTestDB(t)
	defer db.Close()
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes never ready after parallel start (#1008 m7)")
	}
	if !store.PathHopIndexReady() {
		t.Error("pathHop flag not set after WaitIndexesReady returned true")
	}
}

// loadStoreIndexed loads store and waits for the background path-hop index
// (#1008), so a test that queries /api/nodes/{pk}/paths right after Load()
// asserts against the built index instead of racing it into a 503 (#567).
// WaitIndexesReady wakes on the ready channel; the timeout only bounds a
// build that never finishes.
func loadStoreIndexed(t testing.TB, store *PacketStore) {
	t.Helper()
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !store.WaitIndexesReady(30 * time.Second) {
		t.Fatal("path-hop index not ready within 30s after Load (#1008)")
	}
}
