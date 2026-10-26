package main

// #618: /api/nodes?region= is served from ONE per-IATA index built by one
// shared scan (region_index.go), so a region set is a union of per-code key
// sets instead of a full history scan of its own.
//
//   - equivalence: for every region set the union equals the per-set build
//     it replaces (idx618PerSetKeys, the query master ran per set), on a
//     generated database with awkward observer regions, on the committed
//     fixture, and (env-gated) on a large generated database;
//   - one full scan serves every set, cold or warm; the delta and the
//     periodic rebuild keep their #176 semantics;
//   - the background warmer builds the index before the first request,
//     keeps it from ageing past nodeRegionMaxStale, and needs no PacketStore;
//   - a cold request (no snapshot yet) is served by whichever finishes
//     first, the index build or its own per-set build; identical sets
//     share one build, per-set builds are serialised, and a request never
//     queues behind a running rebuild when a usable snapshot exists;
//   - memory grows with distinct (node, region) pairs only.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// idx618PerSetKeys is the oracle: the full per-set build master ran for every
// region set (scanNodeRegionKeys with lo = 0, hi = MAX(observations.id)),
// kept here verbatim so the index is compared with the code it replaces.
func idx618PerSetKeys(t testing.TB, db *DB, codes []string) []string {
	t.Helper()
	wm, err := db.maxObservationID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	placeholders := make([]string, len(codes))
	args := []any{payloadTypeAdvert}
	for i, c := range codes {
		placeholders[i] = "?"
		args = append(args, c)
	}
	args = append(args, int64(0), wm)
	joinCond := "obs.rowid = o.observer_idx"
	if !db.isV3() {
		joinCond = "obs.id = o.observer_id"
	}
	q := fmt.Sprintf(`SELECT DISTINCT t.from_pubkey
		FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id
		JOIN observers obs ON %[1]s
		WHERE t.payload_type = ?
		AND t.from_pubkey IS NOT NULL
		AND UPPER(TRIM(obs.iata)) IN (%[2]s)
		AND o.id > ? AND o.id <= ?`, joinCond, strings.Join(placeholders, ","))
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			t.Fatal(err)
		}
		out = append(out, pk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// idx618Keys is what GetNodes binds for a raw ?region= value.
func idx618Keys(t testing.TB, db *DB, region string) []string {
	t.Helper()
	js, err := db.nodeRegionKeysJSON(normalizeRegionCodes(region))
	if err != nil {
		t.Fatalf("region %q: %v", region, err)
	}
	keys := []string{}
	if err := json.Unmarshal([]byte(js), &keys); err != nil {
		t.Fatal(err)
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("region %q: keys not sorted: %v", region, keys)
	}
	return keys
}

// idx618Subsets returns every non-empty subset of codes as a ?region= value
// when there are few codes, else every single code, every pair and all of
// them: singles, multi-code sets and quick-pick groups alike.
func idx618Subsets(codes []string) []string {
	var out []string
	if len(codes) <= 6 {
		for mask := 1; mask < 1<<len(codes); mask++ {
			var set []string
			for i, c := range codes {
				if mask&(1<<i) != 0 {
					set = append(set, c)
				}
			}
			out = append(out, strings.Join(set, ","))
		}
		return out
	}
	for i, a := range codes {
		out = append(out, a)
		for _, b := range codes[i+1:] {
			out = append(out, a+","+b)
		}
	}
	return append(out, strings.Join(codes, ","))
}

// idx618AssertEquivalent compares the served set with the per-set build for
// every region value and returns how many sets were non-empty.
func idx618AssertEquivalent(t testing.TB, db *DB, stage string, regions []string) int {
	t.Helper()
	nonEmpty := 0
	for _, region := range regions {
		want := idx618PerSetKeys(t, db, normalizeRegionCodes(region))
		got := idx618Keys(t, db, region)
		if !slices.Equal(got, want) {
			t.Errorf("%s: region %q: union %d keys %v, per-set build %d keys %v",
				stage, region, len(got), got, len(want), want)
		}
		if len(want) > 0 {
			nonEmpty++
		}
	}
	return nonEmpty
}

// idx618CountScans counts the heard index's full and delta scans.
func idx618CountScans(db *DB) (full, delta *atomic.Int32) {
	full, delta = &atomic.Int32{}, &atomic.Int32{}
	db.regionHeard.scanHook = func(isFull bool) error {
		if isFull {
			full.Add(1)
		} else {
			delta.Add(1)
		}
		return nil
	}
	return full, delta
}

// idx618AgeIndex pushes the heard index's snapshot refresh (and, when
// builtAgo > 0, its last full build) back, as ageNodeRegionEntry does for an
// entry.
func idx618AgeIndex(db *DB, refreshedAgo, builtAgo time.Duration) {
	ageRegionIndex(&db.regionHeard, refreshedAgo, builtAgo)
}

// idx618SeedAwkward adds what a real observers table holds besides clean
// codes: a lower-case padded code that must fold into AAA, a code only it
// carries, NULL and blank regions that belong to no set, plus an ADVERT
// without from_pubkey and a non-ADVERT, which no set may contain.
func idx618SeedAwkward(t *testing.T, db *DB, now int64) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, o := range []struct {
		rowid int
		iata  any
	}{{100, " aaa "}, {101, "eee"}, {102, nil}, {103, "  "}} {
		exec(`INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen)
			VALUES (?, ?, ?, ?, '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			o.rowid, fmt.Sprintf("obs-x%d", o.rowid), fmt.Sprintf("Obs x%d", o.rowid), o.iata)
	}
	s := &rpm2Seeder{t: t, exec: db.conn, seq: 1 << 20}
	for i, obs := range []int{100, 101, 102, 103} {
		pk := fmt.Sprintf("ee%02x000000000000", i)
		s.directAdvert(pk, now-int64(i)*60, obs)
	}
	// An ADVERT heard in AAA whose from_pubkey was never stamped (the test
	// trigger needs a pubKey to stamp it, and there is none).
	exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES ('1200', 'idx618-nopk', '2026-10-01T00:00:00Z', 2, 4, '{}')`)
	exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp)
		VALUES ((SELECT id FROM transmissions WHERE hash = 'idx618-nopk'), 1, ?)`, now)
	// A GRP_TXT from a pubkey-shaped sender, heard in BBB.
	exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES ('1500', 'idx618-grp', '2026-10-01T00:00:00Z', 1, 5, '{}', 'ff00000000000000')`)
	exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp)
		VALUES ((SELECT id FROM transmissions WHERE hash = 'idx618-grp'), 4, ?)`, now)
}

var idx618Codes = []string{"AAA", "BBB", "CCC", "DDD", "EEE", "ZZZ"}

// idx618RefreshAll takes every set's entry (the ones the LRU still holds)
// and the index past the fresh TTL and runs each set's refresh
// synchronously, as its background refresh would.
func idx618RefreshAll(t *testing.T, db *DB, regions []string) {
	t.Helper()
	for _, region := range regions {
		codes := normalizeRegionCodes(region)
		key := nodeRegionKey(codes)
		if e := db.getNodeRegionEntry(key); e != nil {
			c := *e
			c.refreshed = time.Now().Add(-2 * nodeRegionFreshTTL)
			db.setNodeRegionEntry(key, &c)
		}
		idx618AgeIndex(db, 2*nodeRegionFreshTTL, 0)
		if _, err := db.refreshNodeRegion(key, codes, true); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNodeRegionIndex618_UnionEqualsPerSetBuild: property test over every
// subset of the codes (63 sets, including one no observer carries), cold,
// after new adverts (delta) and after a rebuild that has to drop a region.
// All 63 sets are served from ONE full scan.
func TestNodeRegionIndex618_UnionEqualsPerSetBuild(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 2, rpm2T0)
	idx618SeedAwkward(t, db, rpm2T0)
	full, delta := idx618CountScans(db)
	regions := idx618Subsets(idx618Codes)
	// Spellings the normaliser folds into sets above.
	regions = append(regions, " aaa , Bbb ", "ccc,CCC,ccc", "eee,all", "zzz")

	if n := idx618AssertEquivalent(t, db, "cold", regions); n < len(regions)/2 {
		t.Fatalf("cold: only %d of %d sets are non-empty; the comparison is too weak", n, len(regions))
	}
	// The first sets may have been served by their own per-set build while
	// the index built; wait for it, then every set is a cut of it.
	waitForHeardIndex(t, db, rpm2MaxID(t, db))
	idx618AssertEquivalent(t, db, "warm", regions)
	if full.Load() != 1 || delta.Load() != 0 {
		t.Fatalf("cold: %d sets cost %d full and %d delta scans, want 1 full", len(regions), full.Load(), delta.Load())
	}

	// New adverts arrive: a new node in DDD and an existing CCC node first
	// heard in EEE. Every entry and the index go stale; each set's refresh
	// (run synchronously, as its background refresh would) extends.
	s := &rpm2Seeder{t: t, exec: db.conn, seq: 1 << 21}
	s.directAdvert("dd99000000000001", rpm2T0, s.obs(3, 0))
	s.directAdvert("0201000000000000", rpm2T0, 101)
	idx618AgeIndex(db, 2*nodeRegionFreshTTL, 0)
	idx618RefreshAll(t, db, regions)
	idx618AssertEquivalent(t, db, "after delta", regions)
	if full.Load() != 1 || delta.Load() != 1 {
		t.Fatalf("delta: %d full and %d delta scans, want 1 and 1", full.Load(), delta.Load())
	}

	// Observer edits below the watermark: only the rebuild sees them.
	if _, err := db.conn.Exec(`UPDATE observers SET iata = 'ccc' WHERE rowid = 100`); err != nil {
		t.Fatal(err)
	}
	idx618AgeIndex(db, 2*nodeRegionFreshTTL, 2*nodeRegionRebuildInterval)
	idx618RefreshAll(t, db, regions)
	idx618AssertEquivalent(t, db, "after rebuild", regions)
	if full.Load() != 2 {
		t.Fatalf("rebuild: %d full scans, want 2", full.Load())
	}
}

// TestNodeRegionIndex618_OneScanServesEverySet_Fixture runs on the committed
// e2e fixture through GetNodes only, and counts the "full build" log lines,
// so it states the issue's acceptance in terms master can be measured by:
// every subset of the fixture's regions equals the per-set build, and all of
// them together cost one full scan (master: one per set).
func TestNodeRegionIndex618_OneScanServesEverySet_Fixture(t *testing.T) {
	path := rpm2CopyFile(t, filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var codes []string
	rows, err := db.conn.Query(`SELECT DISTINCT UPPER(TRIM(iata)) FROM observers
		WHERE iata IS NOT NULL AND TRIM(iata) != '' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c)
	}
	rows.Close()
	if len(codes) < 2 {
		t.Fatalf("fixture has %d regions; need at least 2", len(codes))
	}
	regions := idx618Subsets(append(codes, "ZZZ"))

	capture := startLogCapture()
	defer capture.restoreWriter()
	nonEmpty := 0
	for _, region := range regions {
		want := idx618PerSetKeys(t, db, normalizeRegionCodes(region))
		nodes, total, _, err := db.GetNodes(10000, 0, "", "", "", "", "", region)
		if err != nil {
			t.Fatal(err)
		}
		inNodes := map[string]bool{}
		for _, n := range nodes {
			pk, _ := n["public_key"].(string)
			inNodes[pk] = true
		}
		got := idx618Keys(t, db, region)
		if !slices.Equal(got, want) {
			t.Errorf("region %q: union %v, per-set build %v", region, got, want)
		}
		// GetNodes lists exactly the keys that are in the nodes table.
		listed := 0
		for _, pk := range want {
			if inNodes[pk] {
				listed++
			}
		}
		if total != listed || len(nodes) != listed {
			t.Errorf("region %q: GetNodes total %d, %d rows; %d of the set's keys are nodes", region, total, len(nodes), listed)
		}
		if len(want) > 0 {
			nonEmpty++
		}
	}
	waitForHeardIndex(t, db, rpm2MaxID(t, db))
	capture.restoreWriter()
	builds := 0
	for _, line := range strings.Split(capture.read(), "\n") {
		if strings.Contains(line, "[nodes-region]") && strings.Contains(line, "full build") {
			builds++
		}
	}
	t.Logf("fixture: %d regions, %d sets (%d non-empty), %d full builds", len(codes), len(regions), nonEmpty, builds)
	if nonEmpty < len(regions)/2 {
		t.Fatalf("only %d of %d sets are non-empty; the comparison is too weak", nonEmpty, len(regions))
	}
	if builds != 1 {
		t.Fatalf("%d region sets cost %d full builds, want 1 shared scan", len(regions), builds)
	}
}

// TestNodeRegionIndex618_DeltaEqualsFullBuild: the heard scan folded in two
// parts at any split point equals one full scan, pair for pair.
func TestNodeRegionIndex618_DeltaEqualsFullBuild(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	idx618SeedAwkward(t, db, rpm2T0)
	ctx := context.Background()
	wm := rpm2MaxID(t, db)
	full := newRegionEvidenceAgg()
	if _, err := db.scanHeardRegions(ctx, 0, wm, false, full); err != nil {
		t.Fatal(err)
	}
	if full.pairs == 0 || full.watermark != wm {
		t.Fatalf("full scan: %d pairs, watermark %d/%d", full.pairs, full.watermark, wm)
	}
	for _, mid := range []int64{1, wm / 3, wm / 2, wm - 1} {
		inc := newRegionEvidenceAgg()
		if _, err := db.scanHeardRegions(ctx, 0, mid, false, inc); err != nil {
			t.Fatal(err)
		}
		d := newRegionEvidenceAgg()
		if _, err := db.scanHeardRegions(ctx, mid, wm, true, d); err != nil {
			t.Fatal(err)
		}
		inc.merge(d)
		if !reflect.DeepEqual(inc.nodes, full.nodes) || inc.pairs != full.pairs || inc.watermark != wm {
			t.Fatalf("split at %d: %d pairs (watermark %d), full build %d pairs", mid, inc.pairs, inc.watermark, full.pairs)
		}
	}
}

// waitForHeardIndex waits until the heard index has published a snapshot
// with watermark >= wm.
func waitForHeardIndex(t *testing.T, db *DB, wm int64) *regionMembershipSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := db.regionHeard.snap.Load(); s != nil && s.watermark >= wm {
			return s
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("heard index never reached watermark %d", wm)
	return nil
}

// TestNodeRegionIndex618_WarmupServesFirstRequests: after the warmer's build
// the first requests for any set — single, multi, a quick-pick group — scan
// nothing. The DB has no PacketStore at all, so the warmup cannot depend on
// it (or on its lock).
func TestNodeRegionIndex618_WarmupServesFirstRequests(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	full, delta := idx618CountScans(db)
	stop := db.startRegionIndexWarmer(time.Hour)
	defer stop()
	waitForHeardIndex(t, db, rpm2MaxID(t, db))
	if full.Load() != 1 {
		t.Fatalf("warmup: %d full scans, want 1", full.Load())
	}
	for _, region := range []string{"AAA", "BBB,CCC", "AAA,BBB,CCC,DDD", "ddd"} {
		start := time.Now()
		got := idx618Keys(t, db, region)
		took := time.Since(start)
		if want := idx618PerSetKeys(t, db, normalizeRegionCodes(region)); !slices.Equal(got, want) {
			t.Errorf("region %q after warmup: %v, want %v", region, got, want)
		}
		t.Logf("first request for %q after warmup: %v", region, took)
	}
	if full.Load() != 1 || delta.Load() != 0 {
		t.Fatalf("first requests after warmup scanned: %d full, %d delta", full.Load(), delta.Load())
	}
	stop()
	stop() // idempotent
}

// TestNodeRegionIndex618_WarmerKeepsIndexCurrent: with no requests at all
// the warmer keeps refreshing — new adverts land in the snapshot and a due
// rebuild runs in the background — so the index never ages past
// nodeRegionMaxStale and no request ever pays the periodic full scan.
func TestNodeRegionIndex618_WarmerKeepsIndexCurrent(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	full, delta := idx618CountScans(db)
	stop := db.startRegionIndexWarmer(5 * time.Millisecond)
	defer stop()
	waitForHeardIndex(t, db, rpm2MaxID(t, db))

	s := &rpm2Seeder{t: t, exec: db.conn, seq: 1 << 21}
	s.directAdvert("dd99000000000001", rpm2T0, s.obs(3, 0))
	snap := waitForHeardIndex(t, db, rpm2MaxID(t, db))
	if !slices.Contains(snap.byRegion["DDD"], "dd99000000000001") {
		t.Fatalf("warmer refresh missed a new advert: DDD = %v", snap.byRegion["DDD"])
	}
	if delta.Load() == 0 {
		t.Fatal("the warmer picked up a new advert without a delta scan")
	}

	before := full.Load()
	idx618AgeIndex(db, 0, 2*nodeRegionRebuildInterval)
	deadline := time.Now().Add(5 * time.Second)
	for full.Load() == before && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if full.Load() == before {
		t.Fatal("a due rebuild never ran in the warmer")
	}
}

// TestNodeRegionIndex618_WarmerStopDoesNotWaitForForeignFlight: a warmer
// that joined an index refresh some request started (whose context it
// cannot cancel) stops at once instead of waiting for that whole scan —
// on a large database a minute, longer than the shutdown is given.
func TestNodeRegionIndex618_WarmerStopDoesNotWaitForForeignFlight(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var once sync.Once
	db.regionHeard.scanHook = func(bool) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	go db.refreshNodeRegion("AAA", []string{"AAA"}, true) // the request's flight
	<-entered
	stop := db.startRegionIndexWarmer(time.Hour)
	time.Sleep(20 * time.Millisecond) // let the warmer join the flight
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("warmer stop waited for a refresh it did not start")
	}
}

// TestNodeRegionIndex618_ColdRequestsDuringWarmup: requests that arrive
// while the warmup's full scan is still running (here: held) do not wait
// for it — each is served by its own per-set build, the scan a cold request
// ran before #618 — and identical sets share one build. None starts an
// index scan of its own, and once the warmup lands every new set is a cut
// of it.
func TestNodeRegionIndex618_ColdRequestsDuringWarmup(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	var full, delta atomic.Int32
	var once sync.Once
	db.regionHeard.scanHook = func(isFull bool) error {
		if isFull {
			full.Add(1)
		} else {
			delta.Add(1)
		}
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	var refreshes atomic.Int32
	db.nodeRegionQueryHook = func() error { refreshes.Add(1); return nil }
	stop := db.startRegionIndexWarmer(time.Hour)
	defer stop()
	<-entered
	// A row the held build did not cover; the per-set builds start later
	// and must include it.
	s := &rpm2Seeder{t: t, exec: db.conn, seq: 1 << 21}
	s.directAdvert("dd99000000000001", rpm2T0, s.obs(3, 0))

	sets := []string{"AAA", "AAA", "aaa", "BBB,CCC", "CCC,BBB", "DDD"}
	got := make([][]string, len(sets))
	var wg sync.WaitGroup
	for i, region := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = idx618Keys(t, db, region)
		}()
	}
	served := make(chan struct{})
	go func() { wg.Wait(); close(served) }()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		releaseAll()
		t.Fatal("cold requests waited for the warmup's build")
	}
	for i, region := range sets {
		if want := idx618PerSetKeys(t, db, normalizeRegionCodes(region)); !slices.Equal(got[i], want) {
			t.Errorf("%q during warmup: %v, want %v", region, got[i], want)
		}
	}
	if n := refreshes.Load(); n != 3 {
		t.Errorf("%d per-set refreshes for 3 distinct sets; identical sets must share one", n)
	}
	if full.Load() != 1 || delta.Load() != 0 {
		t.Fatalf("during warmup: %d full and %d delta index scans, want only the warmup's", full.Load(), delta.Load())
	}

	releaseAll()
	waitForHeardIndex(t, db, 1)
	// Cut from the warmup's (fresh) snapshot, which predates the DDD row;
	// the next refresh extends it, within nodeRegionFreshTTL.
	want := slices.DeleteFunc(idx618PerSetKeys(t, db, []string{"BBB", "DDD"}),
		func(pk string) bool { return pk == "dd99000000000001" })
	if got := idx618Keys(t, db, "BBB,DDD"); !slices.Equal(got, want) {
		t.Errorf("BBB,DDD after warmup: %v, want %v", got, want)
	}
	if full.Load() != 1 || delta.Load() != 0 {
		t.Fatalf("a new set after warmup scanned: %d full, %d delta", full.Load(), delta.Load())
	}
}

// TestNodeRegionIndex618_ColdRequestTakesIndexWhenFirst: the other side of
// the race — when the index build finishes before the set's per-set build
// (held here), the request is served from the index and does not wait for
// its per-set build. Concurrent cold requests for different sets share the
// one index build.
func TestNodeRegionIndex618_ColdRequestTakesIndexWhenFirst(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	full, delta := idx618CountScans(db)
	db.nodeRegionColdMu.Lock() // no per-set build can run
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(db.nodeRegionColdMu.Unlock) }
	defer unlock()
	sets := []string{"AAA", "BBB,CCC", "DDD", "aaa,ddd"}
	got := make([][]string, len(sets))
	var wg sync.WaitGroup
	for i, region := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = idx618Keys(t, db, region)
		}()
	}
	served := make(chan struct{})
	go func() { wg.Wait(); close(served) }()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		unlock()
		t.Fatal("cold requests waited for their per-set builds although the index was built")
	}
	unlock()
	for i, region := range sets {
		if want := idx618PerSetKeys(t, db, normalizeRegionCodes(region)); !slices.Equal(got[i], want) {
			t.Errorf("%q: %v, want %v", region, got[i], want)
		}
	}
	if full.Load() != 1 || delta.Load() != 0 {
		t.Fatalf("%d concurrent cold sets: %d full and %d delta index scans, want 1 full", len(sets), full.Load(), delta.Load())
	}
}

// TestNodeRegionIndex618_ColdPerSetBuildsAreSerialised: the per-set builds
// cold requests run take nodeRegionColdMu, as master's per-set builds took
// nodeRegionFullMu, so a burst of cold sets holds one pooled connection
// besides the index build's (the pool is 4).
func TestNodeRegionIndex618_ColdPerSetBuildsAreSerialised(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	db.regionHeard.scanHook = func(bool) error { <-release; return nil }
	db.nodeRegionColdMu.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(db.nodeRegionColdMu.Unlock) }
	defer unlock()
	var done atomic.Int32
	var wg sync.WaitGroup
	for _, region := range []string{"AAA", "BBB"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx618Keys(t, db, region)
			done.Add(1)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if n := done.Load(); n != 0 {
		t.Fatalf("%d cold requests were served by a per-set build that skipped nodeRegionColdMu", n)
	}
	unlock()
	wg.Wait()
}

// TestNodeRegionIndex618_RequestsDoNotQueueBehindRebuild: while a full
// rebuild runs, a new region set and an over-stale entry are cut from the
// existing (stale but usable) snapshot at once instead of waiting for it —
// that wait is the minute-long blank Nodes page the issue reports.
func TestNodeRegionIndex618_RequestsDoNotQueueBehindRebuild(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	wantAAA := idx618Keys(t, db, "AAA")
	if len(wantAAA) == 0 {
		t.Fatal("AAA is empty")
	}
	waitForHeardIndex(t, db, rpm2MaxID(t, db))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	db.regionHeard.scanHook = func(isFull bool) error {
		if isFull {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	// AAA's entry is past nodeRegionMaxStale; the index is stale, still
	// usable, and due a rebuild (aged second: the entry helper ages it too).
	ageNodeRegionEntry(t, db, "AAA", nodeRegionMaxStale, 0)
	idx618AgeIndex(db, 2*nodeRegionFreshTTL, 2*nodeRegionRebuildInterval)
	go db.refreshNodeRegion("BBB", []string{"BBB"}, true) // starts the rebuild
	<-entered

	for _, region := range []string{"AAA", "CCC,DDD"} {
		ch := make(chan []string, 1)
		go func() { ch <- idx618Keys(t, db, region) }()
		select {
		case got := <-ch:
			if region == "AAA" && !slices.Equal(got, wantAAA) {
				t.Errorf("AAA during the rebuild: %v, want %v", got, wantAAA)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("request for %q waited for the running rebuild", region)
		}
		// Cut from a stale snapshot, the entry is stale too: the next
		// request re-cuts it instead of serving it for a full TTL.
		if e := db.getNodeRegionEntry(nodeRegionKey(normalizeRegionCodes(region))); e == nil || e.fresh() {
			t.Errorf("entry for %q cut from a stale snapshot: %+v, want a stale entry", region, e)
		}
	}
	close(release)
}

// TestNodeRegionIndex618_ServerWarmsAtStartup: the real server main() builds
// the heard index on its own after startup — no region request is made —
// and its shutdown stops the warmer before closing the database.
func TestNodeRegionIndex618_ServerWarmsAtStartup(t *testing.T) {
	p := startServerProc(t, fullSchemaDB(t), time.Minute)
	p.waitReady(t)
	p.waitLog(t, "[nodes-region] heard: full build", 30*time.Second)
	p.cmd.Process.Signal(syscall.SIGTERM)
	p.waitExit(t, 30*time.Second)
	if out := p.out.String(); !strings.Contains(out, "[server] shutdown complete") {
		t.Fatalf("server did not shut down cleanly:\n%s", out)
	}
}

// TestNodeRegionIndex618_BoundedMemory: the index grows with distinct (node,
// region) pairs, never with hearings or refreshes; the snapshot holds one
// key per pair; the per-set cache stays capped however many sets are asked.
func TestNodeRegionIndex618_BoundedMemory(t *testing.T) {
	db := rpm2OpenTestDB(t)
	seq := 0
	hearAll := func(ts int64) {
		for n := range 5 {
			pk := fmt.Sprintf("%04x000000000000", n+1)
			for obs := 1; obs <= 3; obs++ {
				seq++
				rpm2Insert(t, db, pk, seq, obs, ts, rpm2Str("1200aabbccdd"), rpm2Str("[]"))
			}
		}
	}
	hearAll(rpm2T0)
	idx618Keys(t, db, "AAA")
	waitForHeardIndex(t, db, rpm2MaxID(t, db))
	for round := 1; round <= 20; round++ {
		hearAll(rpm2T0 + int64(round)*3600)
		idx618AgeIndex(db, 2*nodeRegionFreshTTL, 0)
		ageNodeRegionEntry(t, db, "AAA", 2*nodeRegionFreshTTL, 0)
		if _, err := db.refreshNodeRegion("AAA", []string{"AAA"}, true); err != nil {
			t.Fatal(err)
		}
	}
	db.regionHeard.mu.Lock()
	agg := db.regionHeard.agg
	db.regionHeard.mu.Unlock()
	if agg.pairs != 15 || len(agg.nodes) != 5 || agg.watermark != rpm2MaxID(t, db) {
		t.Fatalf("index grew with hearings: %d pairs, %d nodes, watermark %d; want 15, 5, %d",
			agg.pairs, len(agg.nodes), agg.watermark, rpm2MaxID(t, db))
	}
	members := 0
	for _, keys := range db.regionHeard.snap.Load().byRegion {
		members += len(keys)
	}
	if members != agg.pairs {
		t.Fatalf("snapshot holds %d keys for %d pairs", members, agg.pairs)
	}
	for i := range 2 * nodeRegionMaxEntries {
		idx618Keys(t, db, fmt.Sprintf("Z%02d,AAA", i))
	}
	db.nodeRegionCacheMu.Lock()
	n := len(db.nodeRegionCache)
	db.nodeRegionCacheMu.Unlock()
	if n > nodeRegionMaxEntries {
		t.Fatalf("cached %d region sets, cap %d", n, nodeRegionMaxEntries)
	}
}

// TestNodeRegionIndex618_V2Schema: the heard scan also runs on the legacy
// schema (observations.observer_id).
func TestNodeRegionIndex618_V2Schema(t *testing.T) {
	db := setupTestDBv2(t)
	t.Cleanup(func() { db.Close() })
	if db.isV3() {
		t.Fatal("setupTestDBv2 produced a v3 schema")
	}
	for _, o := range []string{"AAA", "BBB"} {
		if _, err := db.conn.Exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen) VALUES (?, ?, ?, '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			"v2-"+o, "V2 "+o, o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES ('1100aabb', 'h-v2', '2026-10-01T00:00:00Z', 1, 4, '{}', ?)`, rpm2PK); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, path_json, timestamp)
		VALUES (1, 'v2-AAA', 'v2-AAA', '[]', '2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{"AAA", "BBB", "AAA,BBB"} {
		want := idx618PerSetKeys(t, db, normalizeRegionCodes(region))
		if got := idx618Keys(t, db, region); !slices.Equal(got, want) {
			t.Errorf("v2 %q: %v, want %v", region, got, want)
		}
	}
	if got := idx618Keys(t, db, "AAA"); !slices.Equal(got, []string{rpm2PK}) {
		t.Errorf("v2 AAA: %v", got)
	}
}

// TestNodeRegionIndex618_SourceGuards: the region index reads SQLite only —
// never the PacketStore or its lock — and adds no untyped maps.
func TestNodeRegionIndex618_SourceGuards(t *testing.T) {
	for _, f := range []string{"region_index.go", "nodes_region_cache.go", "region_placement.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, banned := range []string{"PacketStore", "store.", "map[string]interface{}", "map[string]any"} {
				if strings.Contains(line, banned) {
					t.Errorf("%s:%d uses %q: %s", f, i+1, banned, strings.TrimSpace(line))
				}
			}
		}
	}
}

// ---- large-database equivalence and cost (env-gated) ----

// TestNodeRegionIndex618_UnionEqualsPerSetBuild_LargeDB runs the property
// on a generated database (TestPerf_RegionPlacementM2Generate_488 or
// TestPerf_GenerateRegionDB_176):
//
//	CORESCOPE_PERF_618_DB=/path/big.db go test -run 'LargeDB$' -v -timeout 2h .
func TestNodeRegionIndex618_UnionEqualsPerSetBuild_LargeDB(t *testing.T) {
	path := os.Getenv("CORESCOPE_PERF_618_DB")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_618_DB=<db path> to run the property on a large database")
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	full, _ := idx618CountScans(db)
	codes := idx618Regions(t, db)
	regions := idx618Subsets(codes)
	n := idx618AssertEquivalent(t, db, filepath.Base(path), regions)
	t.Logf("%s: %d regions, %d sets (%d non-empty) equal the per-set build; %d full scans",
		filepath.Base(path), len(codes), len(regions), n, full.Load())
	if full.Load() != 1 {
		t.Errorf("%d full scans, want 1", full.Load())
	}
}

func idx618Regions(t testing.TB, db *DB) []string {
	t.Helper()
	rows, err := db.conn.Query(`SELECT DISTINCT UPPER(TRIM(iata)) FROM observers
		WHERE iata IS NOT NULL AND TRIM(iata) != '' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// TestPerf_NodeRegionIndex618 times, on one database (medians of 5):
//
//   - per-set: master's full build for every single code and every
//     quick-pick-sized group, summed — what a cold instance paid;
//
//   - index: one cold full build of every code;
//
//   - warm: cutting a new set from the warm index (the first request for a
//     set after warmup) and a cached hit;
//
//   - heap of the index and its snapshot.
//
//     CORESCOPE_PERF_618=/path/big.db go test -run '^TestPerf_NodeRegionIndex618$' -v -timeout 2h .
func TestPerf_NodeRegionIndex618(t *testing.T) {
	path := os.Getenv("CORESCOPE_PERF_618")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_618=<db path> to measure")
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	codes := idx618Regions(t, db)
	sets := make([][]string, 0, len(codes)+3)
	for _, c := range codes {
		sets = append(sets, []string{c})
	}
	// Quick-pick-sized groups: halves and all.
	sets = append(sets, codes[:len(codes)/2], codes[len(codes)/2:], codes)
	const runs = 5
	median := func(f func()) time.Duration {
		d := make([]time.Duration, runs)
		for i := range d {
			start := time.Now()
			f()
			d[i] = time.Since(start)
		}
		slices.Sort(d)
		return d[runs/2]
	}
	var perSet time.Duration
	for _, codes := range sets {
		perSet += median(func() { idx618PerSetKeys(t, db, codes) })
	}
	clear := func() {
		db.nodeRegionCacheMu.Lock()
		db.nodeRegionCache, db.nodeRegionUsed = nil, nil
		db.nodeRegionCacheMu.Unlock()
		db.regionHeard.mu.Lock()
		db.regionHeard.agg = nil
		db.regionHeard.mu.Unlock()
		db.regionHeard.snap.Store(nil)
	}
	cold := median(func() {
		clear()
		if _, err := db.refreshRegionIndex(context.Background(), &db.regionHeard, db.heardRegionSpec()); err != nil {
			t.Fatal(err)
		}
	})
	snap := db.regionHeard.snap.Load()
	var cutMax time.Duration
	for _, codes := range sets {
		d := median(func() {
			if _, err := snap.entry(codes); err != nil {
				t.Fatal(err)
			}
		})
		cutMax = max(cutMax, d)
	}
	firstAll := median(func() {
		db.nodeRegionCacheMu.Lock()
		db.nodeRegionCache, db.nodeRegionUsed = nil, nil
		db.nodeRegionCacheMu.Unlock()
		if _, err := db.nodeRegionKeysJSON(codes); err != nil {
			t.Fatal(err)
		}
	})
	hit := median(func() {
		if _, err := db.nodeRegionKeysJSON(codes); err != nil {
			t.Fatal(err)
		}
	})
	// A cold request (no snapshot yet: warmup still running, or no warmer)
	// for the largest single code and for every code: the race between
	// the index build and the set's own per-set build. Each run waits for
	// the background index build, and a losing per-set build, to finish
	// before the next starts; only the request is timed.
	largest, largestN := codes[0], -1
	for _, c := range codes {
		if n := len(snap.byRegion[c]); n > largestN {
			largest, largestN = c, n
		}
	}
	coldFirst := func(set []string) time.Duration {
		d := make([]time.Duration, runs)
		for i := range d {
			clear()
			start := time.Now()
			if _, err := db.nodeRegionKeysJSON(set); err != nil {
				t.Fatal(err)
			}
			d[i] = time.Since(start)
			for db.regionHeard.snap.Load() == nil {
				time.Sleep(time.Millisecond)
			}
			db.nodeRegionColdMu.Lock()
			db.nodeRegionColdMu.Unlock()
		}
		slices.Sort(d)
		return d[runs/2]
	}
	coldOne, coldAll := coldFirst([]string{largest}), coldFirst(codes)
	perSetOne := median(func() { idx618PerSetKeys(t, db, []string{largest}) })
	clear()
	h0 := rpm2Heap()
	if _, err := db.refreshRegionIndex(context.Background(), &db.regionHeard, db.heardRegionSpec()); err != nil {
		t.Fatal(err)
	}
	heap := int64(rpm2Heap()) - int64(h0)
	db.regionHeard.mu.Lock()
	pairs, nodes := db.regionHeard.agg.pairs, len(db.regionHeard.agg.nodes)
	db.regionHeard.mu.Unlock()
	wm := rpm2MaxID(t, db)
	t.Logf("%s: %d observations, %d regions, %d nodes, %d (node, region) pairs", filepath.Base(path), wm, len(codes), nodes, pairs)
	t.Logf("per-set builds, %d sets (%d singles + 3 groups), summed: %v", len(sets), len(codes), perSet.Round(time.Millisecond))
	t.Logf("index cold build, all codes: %v (%.1fx less than the sum)", cold.Round(time.Millisecond), float64(perSet)/float64(cold))
	t.Logf("cold request before any snapshot (index build vs own per-set build, first wins): %s %v (its per-set build alone %v) | all codes %v",
		largest, coldOne.Round(time.Millisecond), perSetOne.Round(time.Millisecond), coldAll.Round(time.Millisecond))
	t.Logf("warm: worst cut of a set from the snapshot %v | first request for all codes (cut + encode) %v | cached hit %v | index heap %d KiB",
		cutMax.Round(time.Microsecond), firstAll.Round(time.Microsecond), hit, heap/1024)
}
