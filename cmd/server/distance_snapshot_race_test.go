package main

import (
	"fmt"
	"io"
	"log"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for computeAnalyticsDistance reading distHops/distPaths outside
// s.mu while ingest (updateDistanceIndexForTxs) and eviction change them
// under s.mu.Lock.
//
// The deterministic tests park the compute after its s.mu.RUnlock() by
// holding one tx's decodedOnce, the same seam as
// TestComputeAnalyticsDistanceReleasesLockBeforeCompute: the compute blocks
// in tx.ParsedDecoded() inside the area filter. While it is parked the test
// runs a real writer under s.mu.Lock, then lets the compute finish.

const (
	distSnapArea   = "SNAP-AREA"
	distSnapRegion = "SNAP-REGION"
)

// newDistSnapStore builds a store with makeTestStore (n packets, starting at
// start, intervalMin apart) and gives every distance record a recognisable
// value: tx i has one hop and one path record with Dist/TotalDist = i+1 and
// Hash = hash%04d. Every sender is in distSnapArea and every observer in
// distSnapRegion. The node cache is seeded empty, so updateDistanceIndexForTxs
// and buildDistanceIndex touch no DB and recompute zero records.
func newDistSnapStore(tb testing.TB, n int, start time.Time, intervalMin int) *PacketStore {
	tb.Helper()
	s := makeTestStore(n, start, intervalMin)
	for i := range s.distHops {
		tx := s.distHops[i].tx
		s.distHops[i] = distHopRecord{
			FromName: "F" + tx.Hash, FromPk: "from-" + tx.Hash,
			ToName: "T" + tx.Hash, ToPk: "to-" + tx.Hash,
			Dist: float64(i + 1), Type: "R↔R", Hash: tx.Hash,
			Timestamp: "2024-01-01T00:00:00Z", HourBucket: "2024-01-01-00", tx: tx,
		}
		s.distPaths[i] = distPathRecord{Hash: tx.Hash, TotalDist: float64(i + 1), HopCount: 1, tx: tx}
	}
	s.nodeCache = []nodeInfo{}
	s.nodePM = buildPrefixMap(nil)
	s.nodeCacheTime = time.Now().Add(time.Hour)

	s.config = &Config{Areas: map[string]AreaEntry{distSnapArea: {Label: "snap"}}}
	inArea := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		inArea[fmt.Sprintf("pk%04d", i)] = true
	}
	s.areaNodeCache = map[string]map[string]bool{distSnapArea: inArea}
	s.areaNodeCacheTimes = map[string]time.Time{distSnapArea: time.Now().Add(time.Hour)}
	s.regionObsCache = map[string]map[string]bool{distSnapRegion: {"obs0": true, "obs1": true}}
	s.regionObsCacheTime = time.Now().Add(time.Hour)
	return s
}

// distSnapParkDecode makes tx.ParsedDecoded() block until release is called.
func distSnapParkDecode(tx *StoreTx) (release func()) {
	held := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	go tx.decodedOnce.Do(func() {
		tx.parsedDecoded = map[string]interface{}{"pubKey": strings.Replace(tx.Hash, "hash", "pk", 1)}
		close(held)
		<-rel
	})
	<-held
	return func() { once.Do(func() { close(rel) }) }
}

type distSnapOutcome struct {
	res      map[string]interface{}
	panicVal string
}

// distSnapRunCompute runs computeAnalyticsDistance in its own goroutine and
// reports its result or recovered panic.
func distSnapRunCompute(s *PacketStore, region, area string) <-chan distSnapOutcome {
	ch := make(chan distSnapOutcome, 1)
	go func() {
		var out distSnapOutcome
		defer func() {
			if r := recover(); r != nil {
				out.panicVal = fmt.Sprint(r)
			}
			ch <- out
		}()
		out.res = s.computeAnalyticsDistance(region, area)
	}()
	return ch
}

// distSnapWaitParked yields until a compute started by distSnapRunCompute is
// blocked in tx.ParsedDecoded's sync.Once. Same runtime.Stack markers as
// distanceComputeWaiting; re-verify them when changing the Go version.
func distSnapWaitParked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, ".(*PacketStore).computeAnalyticsDistance(") &&
				strings.Contains(g, ".(*StoreTx).ParsedDecoded(") &&
				strings.Contains(g, "sync.(*Once).doSlow(") &&
				strings.Contains(g, ".distSnapRunCompute in goroutine ") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for computeAnalyticsDistance to park in tx.ParsedDecoded")
		}
		runtime.Gosched()
	}
}

// distSnapAssertSnapshotState checks the result of a compute that captured
// its snapshot (5 records, Dist 1..5) before the writer ran: it must report
// exactly that snapshot, not a mix of snapshot and post-write state.
func distSnapAssertSnapshotState(t *testing.T, out distSnapOutcome) {
	t.Helper()
	if out.panicVal != "" {
		t.Fatalf("computeAnalyticsDistance panicked: %s", out.panicVal)
	}
	sum := out.res["summary"].(map[string]interface{})
	paths := distSnapTopPathHashes(out.res)
	want := []string{"hash0000", "hash0001", "hash0002", "hash0003", "hash0004"}
	if sum["totalHops"] != 5 || sum["totalPaths"] != 5 || sum["avgDist"] != 3.0 ||
		fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("summary=%v topPaths=%v, want the snapshot: totalHops=5 totalPaths=5 avgDist=3 topPaths=%v",
			sum, paths, want)
	}
}

func distSnapTopPathHashes(res map[string]interface{}) []string {
	tp, _ := res["topPaths"].([]map[string]interface{})
	out := make([]string, 0, len(tp))
	for _, p := range tp {
		out = append(out, p["hash"].(string))
	}
	sort.Strings(out)
	return out
}

// The area-only branch must not index the live s.distHops after RUnlock.
// Here updateDistanceIndexForTxs (IngestNewObservations after a best-path
// change) drops every record while the area loop is parked at i=2. Before
// the fix the loop panics: "index out of range [...] with length 0".
//
// Every tx loses its path, so the writer recomputes nothing and, with no
// prefix map, never decodes a tx (buildHopContextPubkeys returns early);
// it would otherwise block on the parked tx. Removing every record also
// means no element of the snapshot is overwritten.
func TestDistanceSnapshot_AreaOnly_UpdateIndexMidLoop(t *testing.T) {
	s := newDistSnapStore(t, 5, time.Now().Add(-time.Hour), 1)
	s.nodePM = nil
	release := distSnapParkDecode(s.packets[2])
	defer release()
	done := distSnapRunCompute(s, "", distSnapArea)
	distSnapWaitParked(t)

	s.mu.Lock()
	for _, tx := range s.packets {
		tx.PathJSON, tx.parsedPath, tx.pathParsed = "", nil, false
	}
	s.updateDistanceIndexForTxs(s.packets)
	if len(s.distHops) != 0 || len(s.distPaths) != 0 {
		s.mu.Unlock()
		t.Fatalf("writer left %d hops, %d paths; want 0", len(s.distHops), len(s.distPaths))
	}
	s.mu.Unlock()
	release()

	distSnapAssertSnapshotState(t, <-done)
}

// Same, with EvictStale evicting every packet while the area loop is parked
// at i=2 (eviction does not go through ParsedDecoded). Before the fix the
// loop panics: "index out of range [...] with length 0".
func TestDistanceSnapshot_AreaOnly_EvictMidLoop(t *testing.T) {
	s := newDistSnapStore(t, 5, time.Now().Add(-100*time.Hour), 60)
	s.retentionHours = 24
	release := distSnapParkDecode(s.packets[2])
	defer release()
	done := distSnapRunCompute(s, "", distSnapArea)
	distSnapWaitParked(t)

	s.mu.Lock()
	if ev := s.EvictStale(); ev != 5 {
		s.mu.Unlock()
		t.Fatalf("evicted %d, want 5", ev)
	}
	s.mu.Unlock()
	release()

	distSnapAssertSnapshotState(t, <-done)
}

// Area-only computes under -race against a writer that swaps distHops and
// distPaths for new slices (buildDistanceIndex, and a restore of the
// original records). Before the fix the race detector reports the area
// loop reading s.distHops without s.mu, and computes panic.
func TestDistanceSnapshot_AreaOnly_RaceStress(t *testing.T) {
	const n = 500
	s := newDistSnapStore(t, n, time.Now().Add(-time.Hour), 0)
	origHops := append([]distHopRecord(nil), s.distHops...)
	origPaths := append([]distPathRecord(nil), s.distPaths...)

	prev := log.Writer()
	log.SetOutput(io.Discard) // buildDistanceIndex logs every build
	defer log.SetOutput(prev)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			s.buildDistanceIndex() // empty node set: zero records
			s.mu.Unlock()
			runtime.Gosched()
			s.mu.Lock()
			s.distHops = append([]distHopRecord(nil), origHops...)
			s.distPaths = append([]distPathRecord(nil), origPaths...)
			s.mu.Unlock()
			runtime.Gosched()
		}
	}()
	panics := 0
	for i := 0; i < 30; i++ {
		if out := <-distSnapRunCompute(s, "", distSnapArea); out.panicVal != "" {
			panics++
		}
	}
	close(stop)
	wg.Wait()
	if panics > 0 {
		t.Fatalf("%d of 30 area-only computes panicked", panics)
	}
}
