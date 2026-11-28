package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestRP488_FixtureHomeRegionAttribution reports analytics region attribution
// (#804) on a copy of the e2e fixture: home regions per region, the
// multi-byte nodes each region's hash-size analytics lists, and the median
// cost of computeNodeHomeRegions and of the per-region analytics build. It
// uses only APIs that predate #488 M1, so the same file measures master.
// It asserts invariants, not counts, so refreshing the fixture keeps it green.
//
// The committed fixture predates the observer columns GetObservers reads, so
// without CORESCOPE_488_FIXTURE_DB (a copy prepared as deploy.yml prepares
// it: freshen, seed, corescope-migrate, seeds) no region is readable and the
// test skips.
func TestRP488_FixtureHomeRegionAttribution(t *testing.T) {
	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	if p := os.Getenv("CORESCOPE_488_FIXTURE_DB"); p != "" {
		src = p
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "fixture.db")
	copyFileForMeasure(t, src, dbPath)
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewPacketStore(db, nil)
	s.graph.Store(loadNeighborEdgesFromDB(db.conn)) // main.go order: graph before packets
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	obsIATA := s.observerIATAs()
	if len(obsIATA) == 0 {
		t.Skip("no observer regions readable; set CORESCOPE_488_FIXTURE_DB to a migrated fixture copy")
	}
	home := func() map[string]string {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.computeNodeHomeRegions(obsIATA)
	}
	median := func(n int, f func()) time.Duration {
		d := make([]time.Duration, n)
		for i := range d {
			start := time.Now()
			f()
			d[i] = time.Since(start)
		}
		sort.Slice(d, func(a, b int) bool { return d[a] < d[b] })
		return d[n/2]
	}

	homes := home()
	regionSet := map[string]bool{}
	for _, iata := range obsIATA {
		regionSet[iata] = true
	}
	regions := make([]string, 0, len(regionSet))
	for r := range regionSet {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	perRegion := map[string]int{}
	for pk, r := range homes {
		if !regionSet[r] {
			t.Errorf("node %s placed in %q, which no observer reports", pk, r)
		}
		perRegion[r]++
	}

	t.Logf("e2e fixture: obs raw_hex column %v, %d nodes with a home region", db.hasObsRawHex(), len(homes))
	placed := make([]string, 0, len(homes))
	for pk, r := range homes {
		placed = append(placed, pk+"="+r)
	}
	sort.Strings(placed)
	t.Logf("home regions: %v", placed)
	t.Logf("| region | home nodes | multiByteNodes | attributionMethod |")
	for _, r := range regions {
		res := s.computeAnalyticsHashSizes(r, "")
		mb, _ := res["multiByteNodes"].([]map[string]interface{})
		method, _ := res["attributionMethod"].(string)
		t.Logf("| %s | %d | %d | %s |", r, perRegion[r], len(mb), method)
	}
	homeCost := median(51, func() { home() })
	buildCost := median(15, func() {
		for _, r := range regions {
			s.computeAnalyticsHashSizes(r, "")
		}
	})
	t.Logf("median computeNodeHomeRegions %v; median hash-size analytics build, all %d regions: %v", homeCost, len(regions), buildCost)
}
