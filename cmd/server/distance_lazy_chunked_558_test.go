package main

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Issue #558: the production startup loader, LoadChunked, built the distance
// index eagerly under s.mu without publishing the lazy builder's ready /
// generation state (#1011, #149), so the first /api/analytics/distance request
// built it a second time. LoadChunked now leaves it to the lazy builder, as
// the legacy Load() already did (TestDistanceIndexNotBuiltOnLoad).

// distChunkedChunkSize splits setupRichTestDB's transmissions over several
// chunks, so the tests exercise the chunk loop and its post-load section.
const distChunkedChunkSize = 2

// distChunkedGate is a distBuildGate (it holds the first pass until release
// is closed and counts passes) that also records, at the start of every pass,
// how many distance records the index already held. A pass that starts on a
// non-empty index repeats work someone else (the startup loader) already did.
type distChunkedGate struct {
	distBuildGate
	recordsAtStart atomic.Int64
}

func newDistBuildGate() *distChunkedGate {
	return &distChunkedGate{distBuildGate: distBuildGate{entered: make(chan struct{}), release: make(chan struct{})}}
}

// loadChunkedDistStore runs the real LoadChunked on a store that already has
// gate installed as its distanceBuildHook (when non-nil), so a build started
// by the load itself is counted too.
func loadChunkedDistStore(t *testing.T, db *DB, cfg *PacketStoreConfig, gate *distChunkedGate) *PacketStore {
	t.Helper()
	store := NewPacketStore(db, cfg)
	if gate != nil {
		installDistBuildGateOn(store, gate)
	}
	if err := store.LoadChunked(distChunkedChunkSize); err != nil {
		t.Fatalf("LoadChunked: %v", err)
	}
	return store
}

func installDistBuildGateOn(store *PacketStore, g *distChunkedGate) {
	store.distanceBuildHook = func() {
		store.mu.RLock()
		n := len(store.distHops) + len(store.distPaths)
		store.mu.RUnlock()
		g.recordsAtStart.Store(int64(n))
		if g.builds.Add(1) == 1 {
			close(g.entered)
			<-g.release
		}
	}
}

func distIndexLen(store *PacketStore) (hops, paths int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return len(store.distHops), len(store.distPaths)
}

// TestDistanceIndexNotBuiltOnLoadChunked is TestDistanceIndexNotBuiltOnLoad
// for the production loader: right after LoadChunked the distance index is
// empty, not reported built, and no lazy build has started just because the
// startup load completed. A build triggered afterwards does produce records,
// so the empty index is not a property of the fixture.
func TestDistanceIndexNotBuiltOnLoadChunked(t *testing.T) {
	db := setupRichTestDB(t)
	defer db.Close()
	gate := newDistBuildGate()
	close(gate.release) // count builds only
	store := loadChunkedDistStore(t, db, nil, gate)

	if hops, paths := distIndexLen(store); hops != 0 || paths != 0 {
		t.Fatalf("distance index after LoadChunked: %d hops, %d paths, want 0 and 0 (lazy build, #558)", hops, paths)
	}
	if store.DistanceIndexBuilt() {
		t.Fatal("DistanceIndexBuilt() = true directly after LoadChunked")
	}
	if store.DistanceIndexBuilding() {
		t.Fatal("DistanceIndexBuilding() = true directly after LoadChunked: startup completion started a lazy build")
	}
	if n := gate.builds.Load(); n != 0 {
		t.Fatalf("lazy builds after LoadChunked = %d, want 0", n)
	}

	store.TriggerDistanceIndexBuild()
	distBuildWaitCurrent(t, store)
	if hops, paths := distIndexLen(store); hops == 0 || paths == 0 {
		t.Fatalf("lazy build produced %d hops, %d paths; the fixture must yield distance records for this test to mean anything", hops, paths)
	}
}

// TestLoadChunkedPathHopIndexReadyAtStartup558: removing the eager distance
// build must not take the path-hop index or its readiness with it. Startup
// dependants (StartRepeaterEnrichmentRecomputer through WaitIndexesReady)
// must find it ready without waiting.
func TestLoadChunkedPathHopIndexReadyAtStartup558(t *testing.T) {
	db := setupRichTestDB(t)
	defer db.Close()
	store := loadChunkedDistStore(t, db, nil, nil)

	if !store.LoadComplete() {
		t.Fatal("LoadComplete() = false after LoadChunked returned nil")
	}
	if !store.PathHopIndexReady() {
		t.Fatal("PathHopIndexReady() = false after LoadChunked: WaitIndexesReady callers would block until their timeout")
	}
	if !store.WaitIndexesReady(0) {
		t.Fatal("WaitIndexesReady(0) = false after LoadChunked")
	}
	store.mu.RLock()
	n := len(store.byPathHop)
	store.mu.RUnlock()
	if n == 0 {
		t.Fatal("byPathHop is empty after LoadChunked: the path-hop index was not built")
	}
}

// TestDistanceLoadChunkedFirstRequestBuildsOnce558: after a LoadChunked
// startup with the analytics recomputers running, concurrent first requests
// get 202 while the one lazy build is held open, then 200 with the distance
// analytics the eager startup build used to produce. The single build starts
// on an empty index (nothing built it at startup) and later requests do not
// build again.
func TestDistanceLoadChunkedFirstRequestBuildsOnce558(t *testing.T) {
	db := setupRichTestDB(t)
	defer db.Close()

	// Reference: what the eager startup build served, the old LoadChunked
	// post-load call followed by the analytics on that index.
	ref := loadChunkedDistStore(t, db, nil, nil)
	refNodes := ref.snapshotPathNodes()
	ref.mu.Lock()
	ref.buildDistanceIndex(refNodes)
	ref.mu.Unlock()
	want := jsonRoundTrip(t, ref.computeAnalyticsDistance("", ""))
	if s, _ := want["summary"].(map[string]interface{}); s == nil || s["totalHops"] == float64(0) {
		t.Fatalf("reference distance analytics has no hops (summary=%v); the fixture must yield distance data", want["summary"])
	}

	gate := newDistBuildGate()
	store := loadChunkedDistStore(t, db, nil, gate)
	stop := store.StartAnalyticsRecomputers(time.Hour)
	defer stop()
	srv := NewServer(db, &Config{Port: 3000}, NewHub())
	srv.store = store
	r := mux.NewRouter()
	srv.RegisterRoutes(r)
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/distance", nil))
		return w
	}

	const N = 16
	codes := make([]int, N)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = get().Code
		}(i)
	}
	close(start)
	wg.Wait()
	for i, c := range codes {
		if c != 202 {
			t.Fatalf("concurrent first request %d: status %d, want 202", i, c)
		}
	}
	distBuildWaitClosed(t, "the lazy build to reach distanceBuildHook", gate.entered)
	if w := get(); w.Code != 202 || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("request during the build: status %d, Retry-After %q, want 202 and \"5\"", w.Code, w.Header().Get("Retry-After"))
	}
	close(gate.release)
	distBuildWaitCurrent(t, store)

	w := get()
	if w.Code != 200 {
		t.Fatalf("after the build: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode 200 body: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("first 200 differs from the eager-build result:\n got %s\nwant %s", w.Body.String(), mustJSON(t, want))
	}
	for i := 0; i < 4; i++ {
		if c := get().Code; c != 200 {
			t.Fatalf("later request %d: status %d, want 200", i, c)
		}
	}
	if n := gate.builds.Load(); n != 1 {
		t.Fatalf("lazy builds = %d, want exactly 1 for %d concurrent first requests and the later ones", n, N)
	}
	if n := gate.recordsAtStart.Load(); n != 0 {
		t.Fatalf("the lazy build started on an index that already held %d records: startup built it too (double build)", n)
	}
}

// TestDistanceLoadChunkedLoadCompletionDuringBuild558: #149's generation gate
// on the production loader. The background fill completes after the lazy
// build read the LoadChunked dataset; that index belongs to the old generation
// and must not be reported ready. While the rebuild runs the handler keeps
// answering 202; once it finished, 200, after exactly two passes.
func TestDistanceLoadChunkedLoadCompletionDuringBuild558(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		db := setupRichTestDB(t)
		defer db.Close()
		gate := newDistBuildGate()
		// Retention set and oldestLoaded past its cutoff: the background
		// loader goes straight to its completion step (see newDistBuildStore).
		store := loadChunkedDistStore(t, db, &PacketStoreConfig{RetentionHours: 48}, gate)
		store.mu.Lock()
		store.oldestLoaded = time.Now().UTC().Add(-96 * time.Hour).Format(time.RFC3339)
		store.mu.Unlock()
		baseline := runtime.NumGoroutine()

		// Hold the second pass too, so the stale window can be observed.
		pass2 := make(chan struct{})
		release2 := make(chan struct{})
		first := store.distanceBuildHook
		store.distanceBuildHook = func() {
			if gate.builds.Load() >= 1 {
				gate.builds.Add(1)
				close(pass2)
				<-release2
				return
			}
			first()
		}
		srv := NewServer(db, &Config{Port: 3000}, NewHub())
		srv.store = store
		r := mux.NewRouter()
		srv.RegisterRoutes(r)
		code := func() int {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/distance", nil))
			return w.Code
		}

		if c := code(); c != 202 {
			t.Fatalf("first request: status %d, want 202", c)
		}
		distBuildWaitClosed(t, "pass 1 to reach distanceBuildHook", gate.entered)
		// Park pass 1 after it read the dataset, at its bookkeeping (distLazyMu).
		store.distLazyMu.Lock()
		close(gate.release)
		distBuildWait(t, "pass 1 to read the dataset and wait for distLazyMu", func() bool {
			return distBuildParked("sync.Mutex.Lock", buildFrame)
		})
		loaderDone := make(chan struct{})
		go func() {
			defer close(loaderDone)
			store.loadBackgroundChunks()
		}()
		distBuildWaitClosed(t, "the background-load completion", loaderDone)
		store.distLazyMu.Unlock()

		distBuildWaitClosed(t, "the stale-generation rebuild to start", pass2)
		if store.DistanceIndexBuilt() {
			t.Fatal("DistanceIndexBuilt() = true for the index built before the background load completed")
		}
		if !store.DistanceIndexBuilding() {
			t.Fatal("DistanceIndexBuilding() = false while the stale-generation rebuild runs")
		}
		if c := code(); c != 202 {
			t.Fatalf("request during the rebuild: status %d, want 202", c)
		}
		close(release2)
		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)
		if c := code(); c != 200 {
			t.Fatalf("after the rebuild: status %d, want 200", c)
		}
		if n := gate.builds.Load(); n != 2 {
			t.Fatalf("builds = %d, want 2 (the pass on the LoadChunked dataset and one rebuild)", n)
		}
	})
}

func jsonRoundTrip(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(mustJSON(t, v)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
