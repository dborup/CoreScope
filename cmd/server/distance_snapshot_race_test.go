package main

import (
	"fmt"
	"io"
	"log"
	"reflect"
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

// The same mid-loop writers, but removing only some records: the writer
// then moves the survivors down in place, over snapshot elements the compute
// has yet to copy. The compute must still report its snapshot.
func TestDistanceSnapshot_AreaOnly_UpdateIndexMidLoop_Partial(t *testing.T) {
	s := newDistSnapStore(t, 5, time.Now().Add(-time.Hour), 1)
	release := distSnapParkDecode(s.packets[2])
	defer release()
	done := distSnapRunCompute(s, "", distSnapArea)
	distSnapWaitParked(t)

	s.mu.Lock()
	// Not the parked tx: the writer decodes every tx it recomputes.
	s.updateDistanceIndexForTxs([]*StoreTx{s.packets[0], s.packets[1], s.packets[3], s.packets[4]})
	s.mu.Unlock()
	release()

	distSnapAssertSnapshotState(t, <-done)
}

func TestDistanceSnapshot_AreaOnly_EvictMidLoop_Partial(t *testing.T) {
	// Ages 74h, 50h, 26h, 2h, -22h: retention 24h evicts the first three
	// and the parked packets[3] survives.
	s := newDistSnapStore(t, 5, time.Now().Add(-74*time.Hour), 24*60)
	s.retentionHours = 24
	release := distSnapParkDecode(s.packets[3])
	defer release()
	done := distSnapRunCompute(s, "", distSnapArea)
	distSnapWaitParked(t)

	s.mu.Lock()
	if ev := s.EvictStale(); ev != 3 {
		s.mu.Unlock()
		t.Fatalf("evicted %d, want 3", ev)
	}
	s.mu.Unlock()
	release()

	distSnapAssertSnapshotState(t, <-done)
}

// Region+area: the compute parks in the area filter over matchSet, after
// RUnlock and before its filter loops copy hopsSnap/pathsSnap. Before the
// fix, eviction compacted the shared backing array to
// [h4 h1 h2 h3 h4]: totalHops 5, avgDist 3.8, hash0004 twice.
func TestDistanceSnapshot_RegionArea_EvictAfterSnapshot(t *testing.T) {
	s := newDistSnapStore(t, 5, time.Now().Add(-100*time.Hour), 24*60)
	s.retentionHours = 24
	release := distSnapParkDecode(s.packets[4])
	defer release()
	done := distSnapRunCompute(s, distSnapRegion, distSnapArea)
	distSnapWaitParked(t)

	s.mu.Lock()
	if ev := s.EvictStale(); ev != 4 {
		s.mu.Unlock()
		t.Fatalf("evicted %d, want 4", ev)
	}
	s.mu.Unlock()
	release()

	distSnapAssertSnapshotState(t, <-done)
}

// Same with updateDistanceIndexForTxs dropping tx1's records. Before the
// fix: [h0 h2 h3 h4 h4], avgDist 3.6, hash0004 twice.
func TestDistanceSnapshot_RegionArea_UpdateIndexAfterSnapshot(t *testing.T) {
	s := newDistSnapStore(t, 5, time.Now().Add(-time.Hour), 1)
	release := distSnapParkDecode(s.packets[4])
	defer release()
	done := distSnapRunCompute(s, distSnapRegion, distSnapArea)
	distSnapWaitParked(t)

	s.mu.Lock()
	s.updateDistanceIndexForTxs([]*StoreTx{s.packets[1]})
	s.mu.Unlock()
	release()

	distSnapAssertSnapshotState(t, <-done)
}

func distSnapHopHashes(h []distHopRecord) []string {
	out := make([]string, len(h))
	for i := range h {
		out[i] = h[i].Hash
	}
	return out
}

func distSnapPathHashes(p []distPathRecord) []string {
	out := make([]string, len(p))
	for i := range p {
		out[i] = p[i].Hash
	}
	return out
}

// Without a pinned snapshot compaction stays in place (no new backing array,
// so ingest pays no allocation). With one pinned, compaction plus a later
// ingest append must leave the pinned snapshot untouched.
func TestCompactDistIndex_InPlaceUnlessPinned(t *testing.T) {
	s := newDistSnapStore(t, 5, time.Now().Add(-time.Hour), 1)

	s.mu.Lock()
	hops0, paths0 := &s.distHops[0], &s.distPaths[0]
	s.updateDistanceIndexForTxs([]*StoreTx{s.packets[0]})
	if &s.distHops[0] != hops0 || &s.distPaths[0] != paths0 {
		s.mu.Unlock()
		t.Fatal("unpinned compaction allocated new distHops/distPaths")
	}
	s.mu.Unlock()

	s.mu.RLock()
	hopsSnap, pathsSnap := s.distHops, s.distPaths
	s.distSnapReaders.Add(1)
	s.mu.RUnlock()
	wantHops, wantPaths := distSnapHopHashes(hopsSnap), distSnapPathHashes(pathsSnap)

	s.mu.Lock()
	s.updateDistanceIndexForTxs([]*StoreTx{s.packets[1]})
	s.distHops = append(s.distHops, distHopRecord{Hash: "new"}) // as IngestNewFromDB
	s.distPaths = append(s.distPaths, distPathRecord{Hash: "new"})
	gotLive := distSnapHopHashes(s.distHops)
	s.mu.Unlock()
	s.distSnapReaders.Add(-1)

	if got := distSnapHopHashes(hopsSnap); fmt.Sprint(got) != fmt.Sprint(wantHops) {
		t.Errorf("pinned hop snapshot changed: %v, want %v", got, wantHops)
	}
	if got := distSnapPathHashes(pathsSnap); fmt.Sprint(got) != fmt.Sprint(wantPaths) {
		t.Errorf("pinned path snapshot changed: %v, want %v", got, wantPaths)
	}
	if want := "[hash0002 hash0003 hash0004 new]"; fmt.Sprint(gotLive) != want {
		t.Errorf("distHops = %v, want %s", gotLive, want)
	}
}

// After an unpinned in-place compaction the vacated tail of each backing
// array (between the new and the old length) must be zeroed, so it no
// longer keeps removed or evicted *StoreTx (and their observations)
// reachable until a later append happens to overwrite it.
func TestCompactDistIndex_ClearsVacatedTail(t *testing.T) {
	check := func(t *testing.T, s *PacketStore, oldHops, oldPaths int, wantLive string) {
		t.Helper()
		if got := fmt.Sprint(distSnapHopHashes(s.distHops)); got != wantLive {
			t.Fatalf("live distHops = %s, want %s", got, wantLive)
		}
		if got := fmt.Sprint(distSnapPathHashes(s.distPaths)); got != wantLive {
			t.Fatalf("live distPaths = %s, want %s", got, wantLive)
		}
		for i, r := range s.distHops[len(s.distHops):oldHops] {
			if !reflect.ValueOf(r).IsZero() {
				t.Errorf("distHops tail[%d] not zeroed: hash=%q tx=%p", i, r.Hash, r.tx)
			}
		}
		for i, r := range s.distPaths[len(s.distPaths):oldPaths] {
			if !reflect.ValueOf(r).IsZero() {
				t.Errorf("distPaths tail[%d] not zeroed: hash=%q tx=%p", i, r.Hash, r.tx)
			}
		}
	}

	t.Run("updateDistanceIndexForTxs", func(t *testing.T) {
		s := newDistSnapStore(t, 5, time.Now().Add(-time.Hour), 1)
		s.mu.Lock()
		defer s.mu.Unlock()
		oldHops, oldPaths := len(s.distHops), len(s.distPaths)
		s.updateDistanceIndexForTxs([]*StoreTx{s.packets[0], s.packets[2]})
		check(t, s, oldHops, oldPaths, "[hash0001 hash0003 hash0004]")
	})
	t.Run("EvictStale", func(t *testing.T) {
		// Ages 100h, 76h, 52h, 28h, 4h: retention 24h evicts four.
		s := newDistSnapStore(t, 5, time.Now().Add(-100*time.Hour), 24*60)
		s.retentionHours = 24
		s.mu.Lock()
		defer s.mu.Unlock()
		oldHops, oldPaths := len(s.distHops), len(s.distPaths)
		if ev := s.EvictStale(); ev != 4 {
			t.Fatalf("evicted %d, want 4", ev)
		}
		check(t, s, oldHops, oldPaths, "[hash0004]")
	})
}

// Every compute must release its pin; a leaked pin would turn every later
// compaction into a full copy.
func TestDistanceSnapshot_PinReleased(t *testing.T) {
	s := newDistSnapStore(t, 20, time.Now().Add(-time.Hour), 1)
	for _, q := range [][2]string{{"", ""}, {"", distSnapArea}, {distSnapRegion, ""}, {distSnapRegion, distSnapArea}} {
		s.computeAnalyticsDistance(q[0], q[1])
		if n := s.distSnapReaders.Load(); n != 0 {
			t.Fatalf("distSnapReaders=%d after compute(region=%q, area=%q), want 0", n, q[0], q[1])
		}
	}
}

// Default computes (the recomputer's call) under -race against the real
// updateDistanceIndexForTxs plus an IngestNewFromDB-style append. Before the
// fix the race detector reports the filter loop reading snapshot elements
// that the compaction overwrites.
func TestDistanceSnapshot_DefaultCompute_RaceStress(t *testing.T) {
	const n = 2000
	s := newDistSnapStore(t, n, time.Now().Add(-time.Hour), 0)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			tx := s.packets[k%n]
			s.updateDistanceIndexForTxs([]*StoreTx{tx})
			s.distHops = append(s.distHops, distHopRecord{Hash: tx.Hash, Dist: 1, Type: "R↔R", tx: tx})
			s.distPaths = append(s.distPaths, distPathRecord{Hash: tx.Hash, TotalDist: 1, tx: tx})
			s.mu.Unlock()
			runtime.Gosched()
		}
	}()
	for i := 0; i < 30; i++ {
		if out := <-distSnapRunCompute(s, "", ""); out.panicVal != "" {
			t.Errorf("compute panicked: %s", out.panicVal)
		}
	}
	close(stop)
	wg.Wait()
}

// BenchmarkUpdateDistanceIndexForTxs measures one ingest-cycle recompute
// (one tx) at ~30K transmissions: 3.6 hop records per tx as in the e2e
// fixture, ~110K hops and ~21.6K paths. "pinned" is the rare case of a
// compute holding a snapshot, where compaction copies instead.
func BenchmarkUpdateDistanceIndexForTxs(b *testing.B) {
	for _, pinned := range []bool{false, true} {
		name := "unpinned"
		if pinned {
			name = "pinned"
		}
		b.Run(name, func(b *testing.B) {
			s := newDistSnapStore(b, 30000, time.Now().Add(-time.Hour), 0)
			base := s.distHops
			hops := make([]distHopRecord, 0, 110000)
			for len(hops) < cap(hops) {
				r := base[len(hops)%len(base)]
				r.ToPk = fmt.Sprintf("to-%d", len(hops)%5000)
				hops = append(hops, r)
			}
			s.distHops = hops
			s.distPaths = s.distPaths[:21600]
			if pinned {
				s.distSnapReaders.Add(1)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.mu.Lock()
				tx := s.packets[i%len(s.packets)]
				s.updateDistanceIndexForTxs([]*StoreTx{tx})
				s.distHops = append(s.distHops, distHopRecord{tx: tx})
				s.distPaths = append(s.distPaths, distPathRecord{tx: tx})
				s.mu.Unlock()
			}
		})
	}
}
