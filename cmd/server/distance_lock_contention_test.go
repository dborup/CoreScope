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
// parks the compute at a known point after the snapshot and asks the mutex
// whether a writer can get in:
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
//  4. Holding the write lock, the test replaces distHops/distPaths the way
//     ingest does. After release, the result must come from the snapshot
//     alone; under -race this also covers the post-snapshot reads.
//
// A regression that holds the lock past the snapshot fails at step 3 on any
// machine, core count or race mode, because nothing here is measured. The
// deadlines only turn a broken setup into a failure instead of a hang.
//
// Scope: the default (region="", area="") path has no call the test can
// block, so it is covered through the lock/unlock pair that all paths share,
// not by its own parking point. A later s.mu.RLock() added around one path's
// compute would not be caught here.
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
	distanceWaitFor(t, "computeAnalyticsDistance to park in tx.ParsedDecoded", distanceComputeParked)

	// Step 3: the invariant.
	if !store.mu.TryLock() {
		unblock()
		<-done
		t.Fatal("s.mu.TryLock() failed while computeAnalyticsDistance was parked after its snapshot: " +
			"the main RLock is still held across the compute, so ingest writers queue behind analytics readers (issue #1239)")
	}
	// Step 4: an ingest-style swap while the compute is mid-flight.
	store.distHops = []distHopRecord{hop("xx", 500, inArea)}
	store.distPaths = nil
	store.mu.Unlock()

	unblock()
	var r map[string]interface{}
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("computeAnalyticsDistance did not finish after the seam was released")
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

// distanceComputeParked reports whether a goroutine is inside
// computeAnalyticsDistance and blocked on a tx.ParsedDecoded sync.Once.
//
// Maintenance: the match depends on the runtime.Stack text format, the
// sync.(*Once).doSlow frame and the two method names below. Re-verify it when
// changing the Go version or renaming those methods.
func distanceComputeParked() bool {
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
		if strings.Contains(g, ".(*PacketStore).computeAnalyticsDistance(") &&
			strings.Contains(g, ".(*StoreTx).ParsedDecoded(") &&
			strings.Contains(g, "sync.(*Once).doSlow(") {
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
// on 20k hops. It is the measurement the lock test above used to gate on. The
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
	var wg sync.WaitGroup
	wg.Add(Readers)
	for i := 0; i < Readers; i++ {
		go func() {
			defer wg.Done()
			for !stop.Load() {
				store.computeAnalyticsDistance("", "")
			}
		}()
	}
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
