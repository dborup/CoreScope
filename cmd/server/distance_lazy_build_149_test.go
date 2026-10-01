package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Issue #149: two defects in the gate state of the lazy distance-index build
// (#1011).
//
//  1. distLazyOnce (a sync.Once) was reassigned to a zero value by the
//     background-load completion while Do could be running on it. Do holds
//     the Once's internal mutex for the whole build, so its deferred unlock
//     hit the zeroed mutex: "fatal error: sync: unlock of unlocked mutex",
//     which cannot be recovered — the server exits.
//  2. TriggerDistanceIndexBuild took s.mu.RLock while holding distLazyMu, and
//     the background-load completion took distLazyMu while holding s.mu.Lock.
//     Opposite lock order: both block forever.
//
// On a broken gate these scenarios crash or hang the process, so they run in
// a child process (runDistBuildChild). Every wait inside the child has a
// deadline and dumps all goroutines when it expires; the parent only reports
// what the child printed.

const distBuildChildEnv = "CORESCOPE_DIST_BUILD_149_CHILD"

// distBuildDeadline bounds each wait. The scenarios finish in milliseconds
// when the gate is correct; the deadline only turns a hang into a failure
// and leaves room for a loaded -race CI runner.
const distBuildDeadline = 10 * time.Second

// runDistBuildChild runs scenario in a child process that re-executes only
// the calling test, and fails the test if the child fails, crashes or hangs.
// In the child it runs scenario directly.
func runDistBuildChild(t *testing.T, scenario func(t *testing.T)) {
	t.Helper()
	if os.Getenv(distBuildChildEnv) == t.Name() {
		scenario(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v", "-test.timeout=90s")
	cmd.Env = append(os.Environ(), distBuildChildEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed (%v); child output (tail):\n%s", err, lastLines(string(out), 150))
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"..."}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

func distBuildGoroutines() string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// distBuildParked reports whether a goroutine with frame on its stack is
// blocked with the given runtime wait reason ("sync.Mutex.Lock",
// "sync.RWMutex.Lock", "sync.RWMutex.RLock").
//
// Maintenance: the match depends on the runtime.Stack text format (the wait
// reason in the goroutine header) and the method names passed as frame.
// Re-verify it when changing the Go version or renaming those methods.
func distBuildParked(waitReason, frame string) bool {
	return distBuildParkedCount(waitReason, frame) > 0
}

// distBuildParkedCount counts the goroutines distBuildParked matches; a
// waitReason of "sync." matches any mutex wait.
func distBuildParkedCount(waitReason, frame string) int {
	n := 0
	for _, g := range strings.Split(distBuildGoroutines(), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "["+waitReason) && strings.Contains(g, frame) {
			n++
		}
	}
	return n
}

const (
	triggerFrame       = ".(*PacketStore).TriggerDistanceIndexBuild("
	loaderFrame        = ".(*PacketStore).loadBackgroundChunks("
	buildFrame         = ".(*PacketStore).runDistanceIndexBuild("
	indexBuildsCreator = "github.com/corescope/server.(*PacketStore).startBackgroundIndexBuilds"
)

// distBuildWait yields until cond holds and fails with a goroutine dump when
// the deadline expires first.
func distBuildWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(distBuildDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s; goroutines:\n%s", distBuildDeadline, what, distBuildGoroutines())
		}
		runtime.Gosched()
	}
}

func distBuildWaitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(distBuildDeadline):
		t.Fatalf("timed out after %v waiting for %s; goroutines:\n%s", distBuildDeadline, what, distBuildGoroutines())
	}
}

// distBuildWaitCurrent waits until no build runs and the index is current.
func distBuildWaitCurrent(t *testing.T, store *PacketStore) {
	t.Helper()
	distBuildWait(t, "a finished build with a current distance index", func() bool {
		return !store.DistanceIndexBuilding() && store.DistanceIndexBuilt()
	})
}

// distBuildSettle waits, best effort, until the goroutine count is back at
// baseline, so a build goroutine that already did its bookkeeping has also
// returned. Before #149 the build goroutine died in sync.Once.Do's deferred
// unlock right after that bookkeeping; waiting for it to return makes the
// fatal error happen before the child exits instead of racing it.
func distBuildSettle(baseline int) {
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// newDistBuildStore returns a loaded store whose background loader goes
// straight to its completion step: retention is set, and oldestLoaded is
// already past the retention cutoff, so the chunk loop exits on its first
// check and the only s.mu.Lock the loader takes is the completion's. Load()'s
// own index-build goroutines (#1008) have finished and returned, so nothing
// else queues for s.mu or counts as a leftover goroutine.
func newDistBuildStore(t *testing.T) *PacketStore {
	t.Helper()
	db := setupRichTestDB(t)
	t.Cleanup(func() { db.Close() })
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 48})
	if err := store.Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !store.WaitIndexesReady(distBuildDeadline) {
		t.Fatal("Load()'s background index builds did not finish")
	}
	distBuildWait(t, "Load()'s index-build goroutines to return", func() bool {
		return !strings.Contains(distBuildGoroutines(), "created by "+indexBuildsCreator)
	})
	store.mu.Lock()
	store.oldestLoaded = time.Now().UTC().Add(-96 * time.Hour).Format(time.RFC3339)
	store.mu.Unlock()
	return store
}

// distBuildGate is installed as distanceBuildHook: it counts builds and holds
// the first one at its start (before it reads the dataset) until release is
// closed. Later builds only count.
type distBuildGate struct {
	builds  atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func installDistBuildGate(store *PacketStore) *distBuildGate {
	g := &distBuildGate{entered: make(chan struct{}), release: make(chan struct{})}
	store.distanceBuildHook = func() {
		if g.builds.Add(1) == 1 {
			close(g.entered)
			<-g.release
		}
	}
	return g
}

// distBuildLoadWhileQueued starts a build, holds it before it reads the
// dataset, runs the background-load completion to the end, then lets the
// build finish.
func distBuildLoadWhileQueued(t *testing.T) (*PacketStore, *distBuildGate) {
	t.Helper()
	store := newDistBuildStore(t)
	gate := installDistBuildGate(store)
	baseline := runtime.NumGoroutine()

	store.TriggerDistanceIndexBuild()
	distBuildWaitClosed(t, "the build to reach distanceBuildHook", gate.entered)
	// Before #149 the build was inside distLazyOnce.Do here, holding the
	// Once's mutex, and this completion overwrote distLazyOnce.
	store.loadBackgroundChunks()
	close(gate.release)

	distBuildWaitCurrent(t, store)
	distBuildSettle(baseline)
	return store, gate
}

// Crash reproduction: before #149 the child died with "fatal error: sync:
// unlock of unlocked mutex" from sync.(*Once).doSlow.
func TestDistanceBuild149_LoadCompletionDuringBuildDoesNotCrash(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		distBuildLoadWhileQueued(t)
	})
}

// The load completed before the queued build read the dataset, so that build
// already saw the full dataset: no second build.
func TestDistanceBuild149_LoadBeforeBuildSnapshotNoExtraBuild(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		_, gate := distBuildLoadWhileQueued(t)
		if n := gate.builds.Load(); n != 1 {
			t.Fatalf("builds = %d, want 1: the build read the dataset after the load completed, so a rebuild is redundant", n)
		}
	})
}

// The load completed after the build read the dataset: the index that build
// produces is stale, so exactly one more build must follow, and the gate must
// not report the stale index as built.
func TestDistanceBuild149_LoadAfterBuildSnapshotRebuildsOnce(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		store := newDistBuildStore(t)
		gate := installDistBuildGate(store)
		baseline := runtime.NumGoroutine()

		store.TriggerDistanceIndexBuild()
		distBuildWaitClosed(t, "the build to reach distanceBuildHook", gate.entered)

		// Park the build between reading the dataset and its bookkeeping:
		// the bookkeeping needs distLazyMu. The build has read the dataset
		// once distHops is set and s.mu is free again.
		store.distLazyMu.Lock()
		close(gate.release)
		distBuildWait(t, "the build to read the dataset", func() bool {
			store.mu.RLock()
			defer store.mu.RUnlock()
			return store.distHops != nil
		})

		// Complete the background load now, after the build's snapshot. If
		// the completion itself waits for distLazyMu, let both go and let
		// them race for it: the outcome must be the same either way.
		loaderDone := make(chan struct{})
		go func() {
			defer close(loaderDone)
			store.loadBackgroundChunks()
		}()
		distBuildWait(t, "the load completion to finish or wait for distLazyMu", func() bool {
			select {
			case <-loaderDone:
				return true
			default:
				return distBuildParked("sync.Mutex.Lock", loaderFrame)
			}
		})
		store.distLazyMu.Unlock()
		distBuildWaitClosed(t, "the load completion to finish", loaderDone)

		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)
		if n := gate.builds.Load(); n != 2 {
			t.Fatalf("builds = %d, want 2: the first build read the dataset before the load completed, so exactly one rebuild must follow", n)
		}
	})
}

// The build is already waiting for s.mu when the load completion takes it and
// bumps the generation. The build must read the generation inside its own s.mu
// section: a value read before it predates the bump, and would force a
// redundant rebuild.
func TestDistanceBuild149_LoadHoldsMuWhileBuildQueuedNoExtraBuild(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		store := newDistBuildStore(t)
		gate := installDistBuildGate(store)
		baseline := runtime.NumGoroutine()

		store.TriggerDistanceIndexBuild()
		distBuildWaitClosed(t, "the build to reach distanceBuildHook", gate.entered)

		// Queue the load completion as a writer behind a held read lock, then
		// queue the build behind the load completion.
		store.mu.RLock()
		loaderDone := make(chan struct{})
		go func() {
			defer close(loaderDone)
			store.loadBackgroundChunks()
		}()
		distBuildWait(t, "the load completion to queue for s.mu.Lock", func() bool {
			return distBuildParked("sync.RWMutex.Lock", loaderFrame)
		})
		close(gate.release)
		distBuildWait(t, "the build to queue behind the load completion", func() bool {
			return distBuildParkedCount("sync.", buildFrame) == 1
		})
		store.mu.RUnlock()
		distBuildWaitClosed(t, "the load completion to finish", loaderDone)

		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)
		if n := gate.builds.Load(); n != 1 {
			t.Fatalf("builds = %d, want 1: the build took s.mu after the load completed", n)
		}
	})
}

// A build whose dataset went stale during its first pass runs a second pass.
// When that pass starts, distLazyBuilding is still true, so triggers during it
// start no build of their own, and the index built by the first pass does not
// count as built.
func TestDistanceBuild149_TriggersDuringStaleRepassStartNoBuild(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		store := newDistBuildStore(t)
		var builds atomic.Int32
		entered1, release1 := make(chan struct{}), make(chan struct{})
		entered2, release2 := make(chan struct{}), make(chan struct{})
		store.distanceBuildHook = func() {
			switch builds.Add(1) {
			case 1:
				close(entered1)
				<-release1
			case 2:
				close(entered2)
				<-release2
			}
		}
		baseline := runtime.NumGoroutine()

		store.TriggerDistanceIndexBuild()
		distBuildWaitClosed(t, "pass 1 to reach distanceBuildHook", entered1)
		// Park pass 1 before its bookkeeping (it needs distLazyMu), once it
		// has read the dataset, and complete the load behind its snapshot.
		store.distLazyMu.Lock()
		close(release1)
		distBuildWait(t, "pass 1 to read the dataset", func() bool {
			store.mu.RLock()
			defer store.mu.RUnlock()
			return store.distHops != nil
		})
		// If the completion itself takes distLazyMu (allowed: s.mu →
		// distLazyMu), let both go; it bumps the generation first either way.
		loaderDone := make(chan struct{})
		go func() {
			defer close(loaderDone)
			store.loadBackgroundChunks()
		}()
		distBuildWait(t, "the load completion to finish or wait for distLazyMu", func() bool {
			select {
			case <-loaderDone:
				return true
			default:
				return distBuildParked("sync.Mutex.Lock", loaderFrame)
			}
		})
		store.distLazyMu.Unlock()
		distBuildWaitClosed(t, "the load completion to finish", loaderDone)
		distBuildWaitClosed(t, "the stale second pass to reach distanceBuildHook", entered2)

		building, built := store.DistanceIndexBuilding(), store.DistanceIndexBuilt()
		for i := 0; i < 8; i++ {
			store.TriggerDistanceIndexBuild()
		}
		close(release2)
		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)
		if !building {
			t.Error("DistanceIndexBuilding() = false during the stale second pass")
		}
		if built {
			t.Error("DistanceIndexBuilt() = true during the stale second pass: the first pass's index predates the load")
		}
		if n := builds.Load(); n != 2 {
			t.Fatalf("builds = %d, want 2 (pass 1 and one second pass; triggers during the second pass must not start another)", n)
		}
	})
}

// Deadlock reproduction: a trigger on the debounce path holds distLazyMu and
// waits for s.mu.RLock while the load completion holds s.mu.Lock and waits for
// distLazyMu. Before #149 both blocked forever and the child timed out with
// both goroutines in the dump.
func TestDistanceBuild149_TriggerAndLoadCompletionDoNotDeadlock(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		store := newDistBuildStore(t)
		// A completed, current build puts the next trigger on the debounce
		// path, which reads totalObs under s.mu.
		store.TriggerDistanceIndexBuild()
		distBuildWaitCurrent(t, store)

		// Hold a read lock so the load completion queues as a writer.
		store.mu.RLock()
		loaderDone := make(chan struct{})
		go func() {
			defer close(loaderDone)
			store.loadBackgroundChunks()
		}()
		distBuildWait(t, "the load completion to queue for s.mu.Lock", func() bool {
			return distBuildParked("sync.RWMutex.Lock", loaderFrame)
		})

		// With a writer queued, RLock blocks: the trigger parks in RLock.
		triggerDone := make(chan struct{})
		go func() {
			defer close(triggerDone)
			store.TriggerDistanceIndexBuild()
		}()
		distBuildWait(t, "the trigger to wait for s.mu.RLock", func() bool {
			return distBuildParked("sync.RWMutex.RLock", triggerFrame)
		})

		// Let the load completion take s.mu. If the trigger holds distLazyMu
		// while it waits, and the completion needs distLazyMu, neither can
		// continue.
		store.mu.RUnlock()
		distBuildWaitClosed(t, "the load completion to finish (lock-order deadlock)", loaderDone)
		distBuildWaitClosed(t, "the trigger to finish (lock-order deadlock)", triggerDone)
		distBuildWait(t, "no build in flight", func() bool { return !store.DistanceIndexBuilding() })
	})
}

// distLazyBuilding must be set before the build goroutine is started, or
// triggers that run before that goroutine is scheduled start builds of their
// own. With one P the goroutine cannot run until the test goroutine blocks or
// yields, which makes that window deterministic.
func TestDistanceBuild149_BuildingIsSetBeforeTheBuildGoroutineRuns(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		store := newDistBuildStore(t)
		gate := installDistBuildGate(store)
		baseline := runtime.NumGoroutine()

		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		runtime.Gosched() // start a fresh time slice: no preemption below
		store.TriggerDistanceIndexBuild()
		building := store.DistanceIndexBuilding()
		for i := 0; i < 20; i++ {
			store.TriggerDistanceIndexBuild()
		}
		if !building {
			t.Error("DistanceIndexBuilding() = false right after the first trigger: distLazyBuilding is not set before the build goroutine starts")
		}

		distBuildWaitClosed(t, "the build to reach distanceBuildHook", gate.entered)
		close(gate.release)
		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)
		if n := gate.builds.Load(); n != 1 {
			t.Fatalf("builds = %d after 21 back-to-back triggers, want 1", n)
		}
	})
}

// Triggers on the debounce path read totalObs after releasing distLazyMu and
// re-check the gate before they start a build. When a rebuild is due and many
// of them get past the first check together, exactly one starts it.
func TestDistanceBuild149_DebouncedRebuildStartsOnce(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		store := newDistBuildStore(t)
		var builds atomic.Int32
		release := make(chan struct{})
		store.distanceBuildHook = func() {
			if builds.Add(1) > 1 {
				<-release // hold every rebuild until all triggers returned
			}
		}
		baseline := runtime.NumGoroutine()
		store.TriggerDistanceIndexBuild()
		distBuildWaitCurrent(t, store)
		store.distLazyMu.Lock()
		store.distLazyLastBuilt = time.Now().Add(-6 * time.Minute) // rebuild due
		store.distLazyMu.Unlock()

		// Park every trigger behind a held write lock, then let them all go.
		const N = 16
		store.mu.Lock()
		var wg sync.WaitGroup
		wg.Add(N)
		for i := 0; i < N; i++ {
			go func() {
				defer wg.Done()
				store.TriggerDistanceIndexBuild()
			}()
		}
		distBuildWait(t, "every trigger to wait for a lock", func() bool {
			return distBuildParkedCount("sync.", triggerFrame) == N
		})
		store.mu.Unlock()
		wg.Wait()
		close(release)

		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)
		if n := builds.Load(); n != 2 {
			t.Fatalf("builds = %d, want 2 (the first build and one debounced rebuild for %d triggers)", n, N)
		}
	})
}

// A debounced trigger re-checks the gate in its second section: when a build
// finished while it read totalObs, its own rebuild is redundant.
func TestDistanceBuild149_DebouncedTriggerSkipsWhenABuildFinishedMeanwhile(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		store := newDistBuildStore(t)
		var builds atomic.Int32
		store.distanceBuildHook = func() { builds.Add(1) }
		baseline := runtime.NumGoroutine()
		store.TriggerDistanceIndexBuild()
		distBuildWaitCurrent(t, store)
		store.distLazyMu.Lock()
		store.distLazyLastBuilt = store.distLazyLastBuilt.Add(-6 * time.Minute) // rebuild due
		store.distLazyMu.Unlock()

		// Park the trigger in its s.mu read, after its first section ...
		store.mu.Lock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			store.TriggerDistanceIndexBuild()
		}()
		distBuildWait(t, "the trigger to wait for s.mu.RLock", func() bool {
			return distBuildParked("sync.RWMutex.RLock", triggerFrame)
		})
		// ... then at its second section (s.mu → distLazyMu is allowed).
		store.distLazyMu.Lock()
		store.mu.Unlock()
		distBuildWait(t, "the trigger to wait for distLazyMu", func() bool {
			return distBuildParked("sync.Mutex.Lock", triggerFrame)
		})
		// A build completes in between: this is what runDistanceIndexBuild records.
		store.distLazyLastBuilt = time.Now()
		store.distLazyMu.Unlock()
		distBuildWaitClosed(t, "the trigger to return", done)

		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)
		if n := builds.Load(); n != 1 {
			t.Fatalf("builds = %d, want 1: a build finished between the trigger's sections, so its rebuild is redundant", n)
		}
	})
}

// Many concurrent triggers during the first build start exactly one build, and
// triggers after it are debounced.
func TestDistanceBuild149_ConcurrentTriggersStartOneBuild(t *testing.T) {
	store := newDistBuildStore(t)
	gate := installDistBuildGate(store)

	const N = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			store.TriggerDistanceIndexBuild()
		}()
	}
	close(start)
	wg.Wait()
	distBuildWaitClosed(t, "the build to reach distanceBuildHook", gate.entered)
	if !store.DistanceIndexBuilding() {
		t.Fatal("DistanceIndexBuilding() = false while the build is held open")
	}
	close(gate.release)
	distBuildWaitCurrent(t, store)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			store.TriggerDistanceIndexBuild()
		}()
	}
	wg.Wait()
	distBuildWaitCurrent(t, store)
	if n := gate.builds.Load(); n != 1 {
		t.Fatalf("builds = %d, want 1 (%d concurrent triggers, then %d debounced ones)", n, N, N)
	}
}

// The HTTP contract is unchanged: 202 + Retry-After while the index is not
// built (and while the build runs), 200 once it is, and 202 again with one
// rebuild after the background load completes.
func TestDistanceBuild149_HandlerContractAndLoadInvalidation(t *testing.T) {
	store := newDistBuildStore(t)
	gate := installDistBuildGate(store)
	srv := NewServer(store.db, &Config{Port: 3000}, NewHub())
	srv.store = store
	r := mux.NewRouter()
	srv.RegisterRoutes(r)
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/distance", nil))
		return w
	}
	want202 := func(when string) {
		t.Helper()
		w := get()
		if w.Code != 202 {
			t.Fatalf("%s: status %d, want 202 (body=%s)", when, w.Code, w.Body.String())
		}
		if ra := w.Header().Get("Retry-After"); ra != "5" {
			t.Fatalf("%s: Retry-After = %q, want \"5\"", when, ra)
		}
		if !strings.Contains(w.Body.String(), `"status":"building"`) {
			t.Fatalf("%s: body %s, want status building", when, w.Body.String())
		}
	}
	want200 := func(when string) {
		t.Helper()
		if w := get(); w.Code != 200 {
			t.Fatalf("%s: status %d, want 200 (body=%s)", when, w.Code, w.Body.String())
		}
	}

	want202("first request")
	distBuildWaitClosed(t, "the build to reach distanceBuildHook", gate.entered)
	want202("request during the build")
	close(gate.release)
	distBuildWaitCurrent(t, store)
	want200("after the build")
	if n := gate.builds.Load(); n != 1 {
		t.Fatalf("builds = %d after the first build, want 1", n)
	}

	store.loadBackgroundChunks()
	if store.DistanceIndexBuilt() {
		t.Fatal("DistanceIndexBuilt() = true after the background load completed; the index predates the load")
	}
	want202("first request after the background load")
	distBuildWaitCurrent(t, store)
	want200("after the rebuild")
	if n := gate.builds.Load(); n != 2 {
		t.Fatalf("builds = %d, want 2 (one rebuild after the background load)", n)
	}
}

// The debounce policy is unchanged: once a build completed, a trigger only
// rebuilds when Δobs since that build reached 5 % or 5 minutes have passed.
// While a debounced rebuild runs, the index reports not built, so the
// handler answers 202 as for any other build.
func TestDistanceBuild149_DebounceUnchanged(t *testing.T) {
	store := newDistBuildStore(t)
	var builds atomic.Int32
	var hold sync.Mutex // held by the test to keep a rebuild in flight
	store.distanceBuildHook = func() {
		builds.Add(1)
		hold.Lock()
		hold.Unlock()
	}
	store.TriggerDistanceIndexBuild()
	distBuildWaitCurrent(t, store)
	lastObsIs := func(when string, want int) {
		t.Helper()
		store.distLazyMu.Lock()
		got := store.distLazyLastObs
		store.distLazyMu.Unlock()
		if got != want {
			t.Errorf("%s: distLazyLastObs = %d, want totalObs %d: a build records the observation count it read", when, got, want)
		}
	}
	store.mu.RLock()
	loadedObs := store.totalObs
	store.mu.RUnlock()
	lastObsIs("after the first build", loadedObs)

	cases := []struct {
		name     string
		lastObs  int
		curObs   int
		age      time.Duration
		rebuilds bool
	}{
		{name: "just built, no new observations", lastObs: 1000, curObs: 1000},
		{name: "Δobs 4 %", lastObs: 1000, curObs: 1040, age: time.Minute},
		{name: "Δobs 6 %", lastObs: 1000, curObs: 1060, age: time.Minute, rebuilds: true},
		{name: "4 minutes old", lastObs: 1000, curObs: 1000, age: 4 * time.Minute},
		{name: "6 minutes old", lastObs: 1000, curObs: 1000, age: 6 * time.Minute, rebuilds: true},
		{name: "no Δobs baseline, just built", lastObs: 0, curObs: 1000},
	}
	for _, c := range cases {
		store.distLazyMu.Lock()
		store.distLazyLastObs = c.lastObs
		store.distLazyLastBuilt = time.Now().Add(-c.age)
		store.distLazyMu.Unlock()
		store.mu.Lock()
		store.totalObs = c.curObs
		store.mu.Unlock()

		before := builds.Load()
		hold.Lock()
		store.TriggerDistanceIndexBuild()
		built := store.DistanceIndexBuilt()
		hold.Unlock()
		if built == c.rebuilds {
			t.Errorf("%s: DistanceIndexBuilt() = %v right after the trigger, want %v", c.name, built, !c.rebuilds)
		}
		distBuildWaitCurrent(t, store)
		want := before
		if c.rebuilds {
			want++
		}
		if got := builds.Load(); got != want {
			t.Errorf("%s: builds %d → %d, want %d", c.name, before, got, want)
		}
		if c.rebuilds {
			lastObsIs(c.name, c.curObs)
		}
	}
}

// The debounce boundaries are master's: its suppression condition was
// elapsed < 5 min && Δobs < 5 %, so exactly 5 minutes or exactly 5 % rebuilds.
func TestDistanceBuild149_RebuildDueBoundaries(t *testing.T) {
	cases := []struct {
		elapsed         time.Duration
		lastObs, curObs int
		want            bool
	}{
		{5 * time.Minute, 1000, 1000, true},
		{5*time.Minute - time.Nanosecond, 1000, 1000, false},
		{0, 1000, 1050, true},
		{0, 1000, 1049, false},
		{0, 1000, 900, false}, // fewer observations (eviction)
		{0, 0, 1000, false},   // no Δobs baseline
	}
	for _, c := range cases {
		if got := distanceRebuildDue(c.elapsed, c.lastObs, c.curObs); got != c.want {
			t.Errorf("distanceRebuildDue(%v, %d, %d) = %v, want %v", c.elapsed, c.lastObs, c.curObs, got, c.want)
		}
	}
}

// BenchmarkTriggerDistanceIndexBuild covers the two trigger paths a burst of
// requests hits repeatedly: a build already in flight (early return under
// distLazyMu) and a current index inside the debounce window (reads totalObs
// under s.mu.RLock, suppressed). Neither starts a build, so a bare store is
// enough.
func BenchmarkTriggerDistanceIndexBuild(b *testing.B) {
	b.Run("building", func(b *testing.B) {
		s := &PacketStore{}
		s.distLazyBuilding = true
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.TriggerDistanceIndexBuild()
		}
	})
	b.Run("debounced", func(b *testing.B) {
		s := &PacketStore{totalObs: 1000}
		s.distLazyBuilt = true
		s.distLazyLastBuilt = time.Now()
		s.distLazyLastObs = 1000
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.TriggerDistanceIndexBuild()
		}
		if s.DistanceIndexBuilding() {
			b.Fatal("the debounced path started a build")
		}
	})
}
