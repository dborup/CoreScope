package main

// Cost of the #488 M2 placement aggregate against the heard cache, and the
// dry-run on the committed e2e fixture. Ported from the measurement file of
// the research PR #601, run against the production code.
//
// The large-database runs are env-gated, like
// nodes_region_cache_perf_176_test.go:
//
//	CORESCOPE_PERF_488_GEN=/path/big.db CORESCOPE_PERF_488_SCALE=60 \
//	  go test -run '^TestPerf_RegionPlacementM2Generate_488$' -timeout 1h .
//	CORESCOPE_PERF_488_DB=/path/big.db \
//	  go test -run '^TestPerf_RegionPlacementM2_488$' -v -timeout 1h .

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rpm2CopyFile(t testing.TB, src string) string {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	defer in.Close()
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestRegionPlacement488_FixtureDryRun runs the dry-run on the committed e2e
// fixture, opened read-only through OpenDB, and checks the invariants (heard
// equals today's filter, placed never adds a membership, bases partition the
// nodes), not the counts, so refreshing the fixture does not break it. With
// -v it logs the report.
func TestRegionPlacement488_FixtureDryRun(t *testing.T) {
	path := rpm2CopyFile(t, filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	settings := regionPlacementSettings{Mode: regionModeHeard, Params: defaultRegionPlacementParams}
	r, err := buildRegionPlacementDryRun(context.Background(), db, settings, regionDryRunSweep)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := r.write(&b); err != nil {
		t.Fatal(err)
	}
	t.Logf("e2e fixture:\n%s", b.String())
	if len(r.Regions) == 0 {
		t.Fatal("the fixture's observers have regions and hear adverts, but the dry-run reports no region")
	}
	if r.Bases.Direct+r.Bases.Fewest+r.Bases.Unknown != r.Nodes {
		t.Errorf("bases do not partition the nodes: %+v", r.Bases)
	}
	all := map[string]bool{}
	for _, row := range r.Regions {
		cur := rpm2Members(t, db, row.Region)
		for _, pk := range cur {
			all[pk] = true
		}
		if row.Heard != len(cur) {
			t.Errorf("%s: heard %d, today's filter %d", row.Region, row.Heard, len(cur))
		}
		if row.Placed > row.Heard {
			t.Errorf("%s: placed %d > heard %d", row.Region, row.Placed, row.Heard)
		}
	}
	if len(all) != r.Nodes {
		t.Errorf("dry-run saw %d nodes, today's filter %d", r.Nodes, len(all))
	}
}

// ---- large-database cost (env-gated) ----

func TestPerf_RegionPlacementM2Generate_488(t *testing.T) {
	path := os.Getenv("CORESCOPE_PERF_488_GEN")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_488_GEN=<db path> to generate the #488 perf database")
	}
	scale, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_488_SCALE"))
	if scale <= 0 {
		scale = 60
	}
	filler, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_488_FILLER"))
	if filler <= 0 {
		filler = 1_000_000
	}
	db := setupTestDB(t)
	defer db.Close()
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	truth, _ := rpm2SeedScenario(t, tx, scale, time.Now().Unix())
	// Non-advert traffic around the adverts, as on a live instance, so the
	// scans pay for skipping it.
	for i := range filler {
		raw := fmt.Sprintf("1502a1a2%064x", i)
		res, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
			VALUES (?, ?, '2026-10-01T00:00:00Z', 1, 5, '{"type":"GRP_TXT"}')`, raw, fmt.Sprintf("fill-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
			VALUES (?, ?, '["A1","A2"]', ?, ?)`, id, 1+i%(len(rpm2Regions)*rpm2ObsPerRegion), time.Now().Unix()-int64(i), raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS idx_transmissions_payload_type ON transmissions(payload_type)`,
		`CREATE INDEX IF NOT EXISTS idx_observations_observer_idx ON observations(observer_idx)`,
		`ANALYZE`,
	} {
		if _, err := db.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(path)
	if _, err := db.conn.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	var obs int
	db.conn.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&obs)
	t.Logf("generated %s: %d nodes, %d observations in %v", path, len(truth), obs, time.Since(start).Round(time.Second))
}

// TestPerf_RegionPlacementM2_488 times, on one database, medians of 7:
//   - heard: cold build of every region (one shared index since #618), warm
//     hit, delta refresh over the last N observations;
//   - placed: cold global aggregate + placements serving every region,
//     placement-only recompute, delta scan + merge + recompute, warm hit;
//   - heap of the heard index and its entries vs the placement aggregate
//     and its snapshot.
func TestPerf_RegionPlacementM2_488(t *testing.T) {
	path := os.Getenv("CORESCOPE_PERF_488_DB")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_488_DB=<db path> (see TestPerf_RegionPlacementM2Generate_488)")
	}
	rpm2PerfReport(t, path, 5000)
}

// TestRegionPlacement488_FixtureCost runs the same timing on the e2e
// fixture, always (logs only).
func TestRegionPlacement488_FixtureCost(t *testing.T) {
	rpm2PerfReport(t, rpm2CopyFile(t, filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")), 100)
}

func rpm2Median(f func()) time.Duration {
	const runs = 7
	d := make([]time.Duration, runs)
	for i := range d {
		start := time.Now()
		f()
		d[i] = time.Since(start)
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[runs/2]
}

func rpm2Heap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func rpm2PerfReport(t *testing.T, path string, deltaRows int64) {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var regions []string
	rows, err := db.conn.Query(`SELECT DISTINCT UPPER(TRIM(iata)) FROM observers WHERE iata IS NOT NULL AND TRIM(iata) != '' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		regions = append(regions, r)
	}
	rows.Close()
	wm := rpm2MaxID(t, db)
	lo := max(wm-deltaRows, 0)
	// Second delta window: deltaRows ending at the newest ADVERT
	// observation, so every scanned row is classified (the worst case).
	var advHi int64
	if err := db.conn.QueryRow(`SELECT COALESCE(MAX(o.id), 0) FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id WHERE t.payload_type = ?`, payloadTypeAdvert).Scan(&advHi); err != nil {
		t.Fatal(err)
	}
	advLo := max(advHi-deltaRows, 0)

	clearEntries := func() {
		db.nodeRegionCacheMu.Lock()
		db.nodeRegionCache, db.nodeRegionUsed = nil, nil
		db.nodeRegionCacheMu.Unlock()
	}
	clearIndex := func(ix *regionIndex) {
		clearEntries()
		ix.mu.Lock()
		ix.agg = nil
		ix.mu.Unlock()
		ix.snap.Store(nil)
	}
	clearHeard := func() { clearIndex(&db.regionHeard) }
	clearPlaced := func() { clearIndex(&db.regionPlaced) }
	serveAll := func() {
		for _, r := range regions {
			if _, err := db.nodeRegionKeysJSON([]string{r}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// heard (today)
	db.regionPlacement = regionPlacementSettings{Mode: regionModeHeard, Params: defaultRegionPlacementParams}
	heardCold := rpm2Median(func() { clearHeard(); serveAll() })
	heardWarm := rpm2Median(serveAll)
	// #618: every region is cut from one heard index, so a delta is one
	// scan past the index's watermark for all of them.
	heardDelta := rpm2Median(func() {
		db.regionHeard.mu.Lock()
		db.regionHeard.agg.watermark = lo
		db.regionHeard.mu.Unlock()
		ageRegionIndex(&db.regionHeard, 2*nodeRegionFreshTTL, 0)
		for _, r := range regions {
			key := nodeRegionKey([]string{r})
			e := *db.getNodeRegionEntry(key)
			e.refreshed = time.Now().Add(-2 * nodeRegionFreshTTL)
			db.setNodeRegionEntry(key, &e)
			if _, err := db.refreshNodeRegion(key, []string{r}, true); err != nil {
				t.Fatal(err)
			}
		}
	})
	heardAdvScan := rpm2Median(func() {
		if _, err := db.scanHeardRegions(ctx, advLo, advHi, true, newRegionEvidenceAgg()); err != nil {
			t.Fatal(err)
		}
	})
	clearHeard()
	h0 := rpm2Heap()
	serveAll()
	heardHeap := int64(rpm2Heap()) - int64(h0)
	clearHeard()

	// placed
	rpm2SetPlaced(db, defaultRegionPlacementParams)
	placedCold := rpm2Median(func() { clearPlaced(); serveAll() })
	placedWarm := rpm2Median(serveAll)
	db.regionPlaced.mu.Lock()
	agg := db.regionPlaced.agg
	db.regionPlaced.mu.Unlock()
	placedSpec := db.placedRegionSpec()
	placeOnly := rpm2Median(func() { newRegionMembershipSnapshot(agg, placedSpec, time.Now()) })
	// The delta re-folds the same tail rows each run: the stats merge by
	// min/max, so the placement work is the same as for new rows.
	placedDeltaOver := func(lo, hi int64) time.Duration {
		return rpm2Median(func() {
			d := newRegionEvidenceAgg()
			if _, err := db.scanRegionEvidence(ctx, lo, hi, true, d); err != nil {
				t.Fatal(err)
			}
			agg.merge(d)
			snap := newRegionMembershipSnapshot(agg, placedSpec, time.Now())
			for _, r := range regions {
				if _, err := snap.entry([]string{r}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	placedDelta := placedDeltaOver(lo, wm)
	placedAdvDelta := placedDeltaOver(advLo, advHi)
	pairs, nodes := agg.pairs, len(agg.nodes)
	agg = nil
	clearPlaced()
	h1 := rpm2Heap()
	serveAll()
	placedHeap := int64(rpm2Heap()) - int64(h1)

	t.Logf("%s: %d observations, %d regions, %d nodes, %d (node, region) pairs", filepath.Base(path), wm, len(regions), nodes, pairs)
	t.Logf("heard  cold %v | warm %v | delta refresh, newest %d obs %v | delta scan, %d advert obs %v (all regions, one scan) | heap %d KiB",
		heardCold.Round(time.Microsecond), heardWarm, deltaRows, heardDelta.Round(time.Microsecond),
		deltaRows, heardAdvScan.Round(time.Microsecond), heardHeap/1024)
	t.Logf("placed cold %v | warm %v | delta scan+merge+placement+entries, newest %d obs %v, %d advert obs %v | placement only %v | heap %d KiB",
		placedCold.Round(time.Microsecond), placedWarm, deltaRows, placedDelta.Round(time.Microsecond),
		deltaRows, placedAdvDelta.Round(time.Microsecond), placeOnly.Round(time.Microsecond), placedHeap/1024)
}
