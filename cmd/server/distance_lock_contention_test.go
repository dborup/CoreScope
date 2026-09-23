package main

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestComputeAnalyticsDistanceReleasesLockBeforeCompute asserts the #1239
// invariant: computeAnalyticsDistance holds s.mu.RLock() only while it
// snapshots distHops/distPaths (and builds the region match set), and has
// released it before the post-snapshot work starts. Held across the compute,
// the read lock serializes every ingest writer (s.mu.Lock) behind analytics
// readers, which turned a 3s analytics call into 15s under heavy ingest.
//
// The test checks the lock state directly instead of timing writers. It
// parks the compute at a known point after the snapshot, asks the mutex
// whether a writer can get in, and then lets the compute finish while the
// writer still holds the lock:
//
//  1. The test enters tx.decodedOnce.Do first and blocks inside it, so the
//     compute's tx.ParsedDecoded() call has to wait. With a region and an
//     area set, that call is in the area filter, the first step after
//     s.mu.RUnlock().
//  2. It waits until the compute goroutine's stack shows it parked there.
//     That proves the compute has passed the snapshot and cannot move on.
//  3. s.mu.TryLock() must then succeed. No other goroutine touches this
//     store's mu (NewPacketStore starts none), so a failure can only mean
//     the compute still holds the read lock.
//  4. Still holding the write lock, the test points distHops/distPaths at
//     fresh slices and releases the park. Any s.mu.RLock() the compute takes
//     from here on blocks behind the test, so the compute either returns
//     (pass) or shows up parked in RLock (fail). The result must come from
//     the snapshot alone; under -race this also covers the post-snapshot
//     reads.
//
// A regression that still holds the read lock at the park point fails at
// step 3; one that takes it again later in the compute fails at step 4. Both
// fail on any machine, core count or race mode, because nothing is measured.
// The deadlines only turn a broken setup into a failure instead of a hang.
//
// Scope: only the region+area path is driven. The default (region="",
// area="") path has no call the test can block. It shares the lock
// acquisition with this path, so a change to that shared code is caught,
// but a lock taken on the default or area-only path alone is not. Step 4
// assigns fresh slices; it does not model ingest and eviction compacting
// distHops/distPaths in place.
//
// This replaces a writer-latency threshold (150µs, then 5ms) that overlapped
// both healthy CI runs and a deliberate regression on fast machines. The
// measurement lives on as BenchmarkComputeAnalyticsDistanceWriterCycle.
func TestComputeAnalyticsDistanceReleasesLockBeforeCompute(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewPacketStore(db, nil)

	const (
		region   = "SEAM"
		area     = "SEAM-AREA"
		observer = "obs-seam"
	)
	obs := []*StoreObs{{ObserverID: observer}}
	// Both transmissions pass the region filter; only inArea passes the
	// area filter, so the result also shows the area filter really ran.
	inArea := &StoreTx{DecodedJSON: `{"public_key":"aa"}`, Observations: obs}
	outOfArea := &StoreTx{DecodedJSON: `{"public_key":"zz"}`, Observations: obs}

	hop := func(from string, dist float64, tx *StoreTx) distHopRecord {
		return distHopRecord{
			FromName: from, FromPk: from, ToName: "B", ToPk: "bb",
			Dist: dist, Type: "R↔R", Hash: "h-" + from,
			Timestamp: "2024-01-01T00:00:00Z", HourBucket: "2024-01-01-00",
			tx: tx,
		}
	}
	store.mu.Lock()
	store.distHops = []distHopRecord{hop("aa", 10, inArea), hop("zz", 99, outOfArea)}
	store.distPaths = []distPathRecord{{Hash: "p", TotalDist: 10, HopCount: 1, tx: inArea}}
	store.mu.Unlock()

	store.config = &Config{Areas: map[string]AreaEntry{area: {Label: "seam"}}}
	store.regionObsMu.Lock()
	store.regionObsCache = map[string]map[string]bool{region: {observer: true}}
	store.regionObsCacheTime = time.Now()
	store.regionObsMu.Unlock()
	store.areaNodeMu.Lock()
	store.areaNodeCache[area] = map[string]bool{"aa": true}
	store.areaNodeCacheTimes[area] = time.Now()
	store.areaNodeMu.Unlock()

	// Step 1: occupy inArea's decode so the compute blocks on it.
	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	go inArea.decodedOnce.Do(func() {
		inArea.parsedDecoded = map[string]interface{}{"public_key": "aa"}
		close(held)
		<-release
	})
	<-held

	done := make(chan map[string]interface{}, 1)
	go func() { done <- store.computeAnalyticsDistance(region, area) }()

	// Step 2: the compute is past the snapshot and parked in the area filter.
	distanceWaitFor(t, "computeAnalyticsDistance to park in tx.ParsedDecoded", func() bool {
		return distanceComputeWaiting(".(*StoreTx).ParsedDecoded(", "sync.(*Once).doSlow(")
	})

	// Step 3: the invariant.
	if !store.mu.TryLock() {
		unblock()
		<-done
		t.Fatal("s.mu.TryLock() failed while computeAnalyticsDistance was parked after its snapshot: " +
			"the main RLock is still held across the compute, so ingest writers queue behind analytics readers (issue #1239)")
	}
	// Step 4: new data while the compute is mid-flight, then let it finish
	// with the write lock still held.
	store.distHops = []distHopRecord{hop("xx", 500, inArea)}
	store.distPaths = nil
	unblock()
	var r map[string]interface{}
	distanceWaitFor(t, "computeAnalyticsDistance to return or block in s.mu.RLock() "+
		"(a timeout here means it is stuck on s.mu some other way, e.g. Lock())", func() bool {
		select {
		case r = <-done:
			return true
		default:
			return distanceComputeWaiting("[sync.RWMutex.RLock")
		}
	})
	store.mu.Unlock()
	if r == nil {
		<-done
		t.Fatal("computeAnalyticsDistance blocked in s.mu.RLock() after its snapshot: " +
			"it takes the main lock again for the compute, so ingest writers queue behind analytics readers (issue #1239)")
	}

	topHops, _ := r["topHops"].([]map[string]interface{})
	if len(topHops) != 1 || topHops[0]["fromPk"] != "aa" {
		t.Fatalf("topHops = %v, want exactly the in-area snapshot hop (fromPk aa)", topHops)
	}
	topPaths, _ := r["topPaths"].([]map[string]interface{})
	if len(topPaths) != 1 {
		t.Fatalf("topPaths = %v, want the one snapshot path", topPaths)
	}
}

// distanceComputeWaiting reports whether a goroutine started by
// TestComputeAnalyticsDistanceReleasesLockBeforeCompute and inside
// computeAnalyticsDistance has every marker in its runtime.Stack dump. The
// markers are frames (sync.(*Once).doSlow under tx.ParsedDecoded) or the
// wait reason in the goroutine header ([sync.RWMutex.RLock]). Requiring the
// creator keeps a compute leaked by another test from matching.
//
// Maintenance: the match depends on the runtime.Stack text format, those
// runtime frame names and wait reasons, and the method names used here.
// Re-verify it when changing the Go version or renaming those methods.
func distanceComputeWaiting(markers ...string) bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, ".(*PacketStore).computeAnalyticsDistance(") ||
			!strings.Contains(g, ".TestComputeAnalyticsDistanceReleasesLockBeforeCompute in goroutine ") {
			continue
		}
		all := true
		for _, m := range markers {
			all = all && strings.Contains(g, m)
		}
		if all {
			return true
		}
	}
	return false
}

// distanceWaitFor yields until cond holds. The deadline only turns a broken
// test into a failure instead of a hang; it is not part of the coordination.
func distanceWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// BenchmarkComputeAnalyticsDistanceWriterCycle times one bare
// s.mu.Lock/Unlock (ns/op) while 8 goroutines run computeAnalyticsDistance
// on 20k hops, the setup the lock test above used to time and gate on. The
// number depends on core count, scheduler and -race, so it is only for
// comparing builds on one machine and is never asserted.
func BenchmarkComputeAnalyticsDistanceWriterCycle(b *testing.B) {
	store := NewPacketStore(nil, nil) // the default path never touches the DB

	const N = 20000
	hops := make([]distHopRecord, N)
	for i := range hops {
		hops[i] = distHopRecord{
			FromName: "A", FromPk: "aa", ToName: "B", ToPk: "bb",
			Dist: float64(i%500) + 0.5, Type: []string{"R↔R", "C↔R", "C↔C"}[i%3],
			Hash: "h", Timestamp: "2024-01-01T00:00:00Z", HourBucket: "2024-01-01-00",
		}
	}
	paths := make([]distPathRecord, 200)
	for i := range paths {
		paths[i] = distPathRecord{Hash: "p", TotalDist: float64(i), HopCount: 3, Timestamp: "2024-01-01T00:00:00Z"}
	}
	store.mu.Lock()
	store.distHops = hops
	store.distPaths = paths
	store.mu.Unlock()

	const Readers = 8
	var stop atomic.Bool
	var wg, ready sync.WaitGroup
	wg.Add(Readers)
	ready.Add(Readers)
	for i := 0; i < Readers; i++ {
		go func() {
			defer wg.Done()
			store.computeAnalyticsDistance("", "")
			ready.Done()
			for !stop.Load() {
				store.computeAnalyticsDistance("", "")
			}
		}()
	}
	ready.Wait() // time only while every reader is in its compute loop
	defer func() {
		stop.Store(true)
		wg.Wait()
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.mu.Lock()
		store.mu.Unlock()
	}
	b.StopTimer()
}
