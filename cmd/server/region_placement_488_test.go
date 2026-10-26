package main

// Tests for the #488 M2 direct-evidence region placement
// (region_placement.go). The placement rule, the aggregate and the cache
// tests are ported from the research prototype in draft PR #601
// (region_placement_488_test.go there), renamed so the two never collide
// in package main, and run against the production code.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/meshcore-analyzer/packetpath"
)

func rpm2Null() sql.NullString        { return sql.NullString{} }
func rpm2Str(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

var (
	rpm2Direct = packetpath.AdvertHearing{Kind: packetpath.AdvertDirect, Verified: true}
	// rpm2Legacy is an explicit '[]' path on a row without a frame.
	rpm2Legacy = packetpath.AdvertHearing{Kind: packetpath.AdvertDirect}
	rpm2Shared = packetpath.AdvertHearing{Kind: packetpath.AdvertShared, Verified: true}
)

func rpm2Hop(n int) packetpath.AdvertHearing {
	return packetpath.AdvertHearing{Kind: packetpath.AdvertRelayed, Hops: n, Verified: true}
}

const rpm2Day = int64(24 * 3600)

type rpm2Hearing struct {
	region string
	ts     int64
	h      packetpath.AdvertHearing
}

func rpm2Stats(hs ...rpm2Hearing) map[string]*regionEvidenceStats {
	m := map[string]*regionEvidenceStats{}
	for _, h := range hs {
		if m[h.region] == nil {
			m[h.region] = &regionEvidenceStats{}
		}
		m[h.region].add(h.ts, h.h)
	}
	return m
}

func rpm2Daily(region string, from, to int64, h packetpath.AdvertHearing) []rpm2Hearing {
	var out []rpm2Hearing
	for d := from; d <= to; d++ {
		out = append(out, rpm2Hearing{region, 1_000*rpm2Day + d*rpm2Day, h})
	}
	return out
}

func rpm2Cat(parts ...[]rpm2Hearing) []rpm2Hearing {
	var out []rpm2Hearing
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestRegionPlacement488_PlaceRule(t *testing.T) {
	def := defaultRegionPlacementParams
	span := def
	span.PersistSpan = 24 * time.Hour
	cap0 := def
	cap0.MaxHops = 0
	noWindow := def
	noWindow.Lifetime = 0
	base := int64(1_000 * rpm2Day)
	direct := func(rs ...string) regionPlacement { return regionPlacement{Basis: placementBasisDirect, Regions: rs} }
	fewest := func(h int, rs ...string) regionPlacement {
		return regionPlacement{Basis: placementBasisFewest, Regions: rs, Hops: h}
	}
	unknown := regionPlacement{Basis: placementBasisUnknown, Hops: -1}
	for _, tc := range []struct {
		name string
		hs   []rpm2Hearing
		p    regionPlacementParams
		want regionPlacement
	}{
		{"relayed-only region does not leak",
			rpm2Cat(rpm2Daily("AAA", 0, 9, rpm2Direct), rpm2Daily("BBB", 0, 9, rpm2Hop(2))), def, direct("AAA")},
		{"two direct regions are a set",
			rpm2Cat(rpm2Daily("AAA", 0, 6, rpm2Direct), rpm2Daily("BBB", 0, 6, rpm2Direct)), def, direct("AAA", "BBB")},
		{"one-off long-distance direct hearing: kept while the transient rule is off (default)",
			rpm2Cat(rpm2Daily("AAA", 0, 9, rpm2Direct), []rpm2Hearing{{"DDD", base + 5*rpm2Day, rpm2Direct}}), def, direct("AAA", "DDD")},
		{"one-off long-distance direct hearing: transient with persistSpan",
			rpm2Cat(rpm2Daily("AAA", 0, 9, rpm2Direct), []rpm2Hearing{{"DDD", base + 5*rpm2Day, rpm2Direct}}), span,
			regionPlacement{Basis: placementBasisDirect, Regions: []string{"AAA"}, Transient: []string{"DDD"}}},
		{"sustained second region is not transient",
			rpm2Cat(rpm2Daily("AAA", 0, 9, rpm2Direct), []rpm2Hearing{{"DDD", base + 2*rpm2Day, rpm2Direct}, {"DDD", base + 5*rpm2Day, rpm2Direct}}), span,
			direct("AAA", "DDD")},
		{"no persistent region: all direct regions kept",
			[]rpm2Hearing{{"AAA", base, rpm2Direct}, {"BBB", base + 3600, rpm2Direct}}, span, direct("AAA", "BBB")},
		{"fewest hops wins, ties keep both",
			rpm2Cat(rpm2Daily("AAA", 0, 3, rpm2Hop(2)), rpm2Daily("BBB", 0, 3, rpm2Hop(2)), rpm2Daily("CCC", 0, 3, rpm2Hop(3))), def,
			fewest(2, "AAA", "BBB")},
		{"exactly the cap is placed", rpm2Daily("AAA", 0, 3, rpm2Hop(3)), def, fewest(3, "AAA")},
		{"past the cap is unknown",
			rpm2Cat(rpm2Daily("AAA", 0, 3, rpm2Hop(4)), rpm2Daily("BBB", 0, 3, rpm2Hop(9))), def, unknown},
		{"cap 0: no relayed fallback", rpm2Daily("AAA", 0, 3, rpm2Hop(1)), cap0, unknown},
		{"hops past the stored slots never place, whatever the cap",
			rpm2Daily("AAA", 0, 3, rpm2Hop(regionPlacementHopSlots)), regionPlacementParams{MaxHops: 63, Lifetime: def.Lifetime}, unknown},
		{"shared contacts and unknown paths never place",
			[]rpm2Hearing{{"AAA", base, rpm2Shared}, {"BBB", base, packetpath.AdvertHearing{Kind: packetpath.AdvertUnknown}}}, def, unknown},
		{"undated hearing never places", []rpm2Hearing{{"AAA", 0, rpm2Direct}}, def, unknown},
		{"quiet node keeps its placement (window anchored at its newest hearing)",
			rpm2Daily("AAA", -200, -190, rpm2Direct), def, direct("AAA")},
		{"moved node: evidence older than the lifetime before its newest is dropped",
			rpm2Cat(rpm2Daily("AAA", -100, -60, rpm2Direct), rpm2Daily("BBB", -19, 0, rpm2Direct)), def, direct("BBB")},
		{"lifetime 0 keeps all history",
			rpm2Cat(rpm2Daily("AAA", -100, -60, rpm2Direct), rpm2Daily("BBB", -19, 0, rpm2Direct)), noWindow, direct("AAA", "BBB")},
		// F1 (#612 review): with no window, relayed-only evidence must not
		// satisfy within() through an unheard (zero) direct timestamp.
		{"lifetime 0: relayed-only regions are not direct",
			rpm2Cat(rpm2Daily("AAA", 0, 3, rpm2Hop(1)), rpm2Daily("BBB", 0, 3, rpm2Hop(3))), noWindow, fewest(1, "AAA")},
		{"stale fallback does not outrank a fresher, longer one",
			rpm2Cat(rpm2Daily("AAA", -100, -60, rpm2Hop(1)), rpm2Daily("BBB", -10, 0, rpm2Hop(3))), def, fewest(3, "BBB")},
		{"direct anywhere beats a closer-looking fallback here",
			rpm2Cat(rpm2Daily("AAA", 0, 9, rpm2Hop(1)), rpm2Daily("BBB", 0, 9, rpm2Direct)), def, direct("BBB")},
		// Owner decision on #488: an explicit '[]' without a frame cannot be
		// told apart from a contact re-share, so it is fallback only.
		{"legacy '[]' never outranks verified direct evidence",
			rpm2Cat(rpm2Daily("AAA", 0, 9, rpm2Legacy), []rpm2Hearing{{"BBB", base, rpm2Direct}}), def, direct("BBB")},
		{"legacy-only node is a 0-hop fallback", rpm2Daily("AAA", 0, 3, rpm2Legacy), def, fewest(0, "AAA")},
		{"legacy '[]' beats a relayed hearing elsewhere in the fallback",
			rpm2Cat(rpm2Daily("AAA", 0, 3, rpm2Legacy), rpm2Daily("BBB", 0, 3, rpm2Hop(1))), def, fewest(0, "AAA")},
		{"legacy '[]' places under cap 0", rpm2Daily("AAA", 0, 3, rpm2Legacy), cap0, fewest(0, "AAA")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := placeNode(rpm2Stats(tc.hs...), tc.p)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("placeNode = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRegionPlacement488_StatsAreBounded: one (node, region) pair costs a
// fixed number of bytes however many hearings it folds in.
func TestRegionPlacement488_StatsAreBounded(t *testing.T) {
	if got := unsafe.Sizeof(regionEvidenceStats{}); got != 80 {
		t.Fatalf("regionEvidenceStats is %d bytes, want a fixed 80", got)
	}
	s := &regionEvidenceStats{}
	far := 0
	for i := 1; i <= 10_000; i++ {
		if s.add(int64(i), rpm2Hop(1+i%70)) == hearingFar {
			far++
		}
	}
	if far == 0 || s.LastAt[regionPlacementHopSlots-1] == 0 {
		t.Fatalf("hops past the slots must be counted, not stored: far %d, %+v", far, s)
	}
	// Slot 0 is verified-direct only: a malformed "relayed at 0 hops" is unknown.
	z := &regionEvidenceStats{}
	if k := z.add(42, rpm2Hop(0)); k != hearingUnknown || z.LastAt[0] != 0 {
		t.Fatalf("relayed at 0 hops reached the direct slot: %v %+v", k, z)
	}
	// A legacy '[]' goes to its own slot, never to the direct one.
	if k := z.add(42, rpm2Legacy); k != hearingLegacy || z.LastAt[0] != 0 || z.LegacyAt != 42 {
		t.Fatalf("legacy hearing: %v %+v", k, z)
	}
}

// ---- DB-backed: the aggregate behind the cache ----

const rpm2PK = "aaaa000000000001"

// rpm2T0 is fixed so the tests are deterministic (2026-09-21).
const rpm2T0 = int64(1_790_000_000)

func rpm2EmptyTestDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	db.hasObsRawHexFlag.forceTrue()
	t.Cleanup(func() { db.Close() })
	return db
}

// rpm2OpenTestDB adds one observer per region AAA, BBB, CCC (rowids 1-3).
func rpm2OpenTestDB(t *testing.T) *DB {
	t.Helper()
	db := rpm2EmptyTestDB(t)
	for i, iata := range []string{"AAA", "BBB", "CCC"} {
		if _, err := db.conn.Exec(`INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen)
			VALUES (?, ?, ?, ?, '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			i+1, "obs-"+iata, "Obs "+iata, iata); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func rpm2SetPlaced(db *DB, p regionPlacementParams) {
	db.regionPlacement = regionPlacementSettings{Mode: regionModePlaced, Params: p}
}

// rpm2Insert writes one ADVERT transmission by pk heard once by observer
// rowid obs, with the given per-observation frame and path.
func rpm2Insert(t *testing.T, db *DB, pk string, seq, obs int, ts int64, raw, path sql.NullString) {
	t.Helper()
	decoded := `{"type":"ADVERT","pubKey":"` + pk + `"}`
	res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES ('1100aabb', ?, '2026-10-01T00:00:00Z', 1, 4, ?, ?)`, fmt.Sprintf("h-%s-%d", pk, seq), decoded, pk)
	if err != nil {
		t.Fatal(err)
	}
	txID, _ := res.LastInsertId()
	if _, err = db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
		VALUES (?, ?, ?, ?, ?)`, txID, obs, path, ts, raw); err != nil {
		t.Fatal(err)
	}
}

func rpm2MaxID(t testing.TB, db *DB) int64 {
	t.Helper()
	wm, err := db.maxObservationID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return wm
}

func rpm2Scan(t testing.TB, db *DB, lo, hi int64, agg *regionEvidenceAgg) *regionEvidenceAgg {
	t.Helper()
	if agg == nil {
		agg = newRegionEvidenceAgg()
	}
	if _, err := db.scanRegionEvidence(context.Background(), lo, hi, lo > 0, agg); err != nil {
		t.Fatal(err)
	}
	return agg
}

func rpm2Placements(agg *regionEvidenceAgg, p regionPlacementParams) map[string]regionPlacement {
	out := make(map[string]regionPlacement, len(agg.nodes))
	for pk, per := range agg.nodes {
		out[pk] = placeNode(per, p)
	}
	return out
}

// rpm2Members is what /api/nodes?region= filters on: the real cache.
func rpm2Members(t testing.TB, db *DB, codes ...string) []string {
	t.Helper()
	js, err := db.nodeRegionKeysJSON(codes)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := json.Unmarshal([]byte(js), &keys); err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	return keys
}

// rpm2Refresh ages the snapshot and the entry for codes past the fresh TTL
// and runs that entry's refresh synchronously, as the stale-while-revalidate
// path does in the background.
func rpm2Refresh(t testing.TB, db *DB, codes ...string) []string {
	t.Helper()
	ageRegionIndex(&db.regionPlaced, 2*nodeRegionFreshTTL, 0)
	key := nodeRegionKey(codes)
	if e := db.getNodeRegionEntry(key); e != nil {
		c := *e
		c.refreshed = time.Now().Add(-2 * nodeRegionFreshTTL)
		db.setNodeRegionEntry(key, &c)
	}
	e, err := db.refreshNodeRegion(key, codes, true)
	if err != nil {
		t.Fatal(err)
	}
	keys := append([]string(nil), e.keys...)
	sort.Strings(keys)
	return keys
}

// rpm2RebuildDue makes the aggregate's next refresh a full rebuild.
func rpm2RebuildDue(db *DB) {
	ageRegionIndex(&db.regionPlaced, 0, 2*nodeRegionRebuildInterval)
}

// rpm2CountScans installs the scan hook and returns the full/delta counters.
func rpm2CountScans(db *DB) (full, delta *atomic.Int32) {
	full, delta = &atomic.Int32{}, &atomic.Int32{}
	db.regionPlaced.scanHook = func(isFull bool) error {
		if isFull {
			full.Add(1)
		} else {
			delta.Add(1)
		}
		return nil
	}
	return full, delta
}

func TestRegionPlacement488_DeltaEqualsFullBuild(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	truth, _ := rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	wm := rpm2MaxID(t, db)
	full := rpm2Scan(t, db, 0, wm, nil)
	if len(full.nodes) != len(truth) {
		t.Fatalf("full build saw %d nodes, scenario has %d", len(full.nodes), len(truth))
	}
	params := defaultRegionPlacementParams
	params.PersistSpan = 24 * time.Hour
	for _, mid := range []int64{1, wm / 3, wm / 2, wm - 1} {
		inc := rpm2Scan(t, db, 0, mid, nil)
		inc.merge(rpm2Scan(t, db, mid, wm, nil))
		if !reflect.DeepEqual(rpm2Placements(inc, params), rpm2Placements(full, params)) {
			t.Fatalf("delta from %d: placements differ from the full build", mid)
		}
		if !reflect.DeepEqual(inc.nodes, full.nodes) {
			t.Fatalf("delta from %d: per-pair stats differ from the full build", mid)
		}
		if inc.watermark != wm || inc.pairs != full.pairs || inc.hearings != full.hearings {
			t.Fatalf("delta from %d: watermark %d/%d, pairs %d/%d, hearings %v/%v",
				mid, inc.watermark, wm, inc.pairs, full.pairs, inc.hearings, full.hearings)
		}
	}
}

// TestRegionPlacement488_DefaultModeIsHeard: with no regionPlacement config
// the filter is today's rule (a relayed-only node is a member), and the
// aggregate is never built.
func TestRegionPlacement488_DefaultModeIsHeard(t *testing.T) {
	db := rpm2OpenTestDB(t)
	db.regionPlacement = (&Config{}).RegionPlacementSettings()
	full, delta := rpm2CountScans(db)
	rpm2Insert(t, db, rpm2PK, 1, 1, rpm2T0, rpm2Str("1200aabbccdd"), rpm2Str("[]"))
	rpm2Insert(t, db, rpm2PK, 2, 2, rpm2T0, rpm2Str("1105a1a2a3a4a5aabbccdd"), rpm2Str(`["A1","A2","A3","A4","A5"]`))
	if got := rpm2Members(t, db, "BBB"); !reflect.DeepEqual(got, []string{rpm2PK}) {
		t.Fatalf("default mode: BBB members %v, want the relayed-only node (heard rule)", got)
	}
	if db.regionPlaced.snap.Load() != nil || full.Load()+delta.Load() != 0 {
		t.Fatalf("default mode built the placement aggregate (%d full, %d delta scans)", full.Load(), delta.Load())
	}
}

// TestRegionPlacement488_PlacedModeDropsRelayedOnly: the leak #488 is
// about, closed in placed mode.
func TestRegionPlacement488_PlacedModeDropsRelayedOnly(t *testing.T) {
	db := rpm2OpenTestDB(t)
	rpm2SetPlaced(db, defaultRegionPlacementParams)
	rpm2Insert(t, db, rpm2PK, 1, 1, rpm2T0, rpm2Str("1200aabbccdd"), rpm2Str("[]"))
	rpm2Insert(t, db, rpm2PK, 2, 2, rpm2T0, rpm2Str("1105a1a2a3a4a5aabbccdd"), rpm2Str(`["A1","A2","A3","A4","A5"]`))
	if got := rpm2Members(t, db, "BBB"); len(got) != 0 {
		t.Fatalf("placed mode: BBB members %v, want none", got)
	}
	if got := rpm2Members(t, db, "AAA"); !reflect.DeepEqual(got, []string{rpm2PK}) {
		t.Fatalf("placed mode: AAA members %v", got)
	}
	if got := rpm2Members(t, db, "AAA", "BBB"); !reflect.DeepEqual(got, []string{rpm2PK}) {
		t.Fatalf("placed mode: AAA,BBB members %v (union of codes)", got)
	}
}

// TestRegionPlacement488_NewObservationCanRemoveRegion: a new direct
// hearing takes a node OUT of its fallback region, so a placed-mode refresh
// re-derives membership instead of extending it, and it does so from a
// delta scan, not a rebuild.
func TestRegionPlacement488_NewObservationCanRemoveRegion(t *testing.T) {
	db := rpm2OpenTestDB(t)
	rpm2SetPlaced(db, defaultRegionPlacementParams)
	full, delta := rpm2CountScans(db)
	rpm2Insert(t, db, rpm2PK, 1, 1, rpm2T0, rpm2Str("1101a1aabbccdd"), rpm2Str(`["A1"]`)) // AAA, 1 hop
	if got := rpm2Members(t, db, "AAA"); !reflect.DeepEqual(got, []string{rpm2PK}) {
		t.Fatalf("before: AAA members %v", got)
	}
	rpm2Members(t, db, "BBB")
	rpm2Insert(t, db, rpm2PK, 2, 2, rpm2T0+3600, rpm2Str("1200aabbccdd"), rpm2Str("[]")) // BBB, direct
	if got := rpm2Refresh(t, db, "AAA"); len(got) != 0 {
		t.Errorf("after: AAA members %v, want none (direct evidence elsewhere)", got)
	}
	if got := rpm2Refresh(t, db, "BBB"); !reflect.DeepEqual(got, []string{rpm2PK}) {
		t.Errorf("after: BBB members %v", got)
	}
	if full.Load() != 1 || delta.Load() != 1 {
		t.Errorf("scans: %d full, %d delta; want 1 full (cold) and 1 delta (the refresh)", full.Load(), delta.Load())
	}
	if s := db.regionPlaced.snap.Load(); s == nil || s.watermark != rpm2MaxID(t, db) {
		t.Errorf("snapshot watermark not advanced: %+v", s)
	}
}

// TestRegionPlacement488_RebuildNeededForEditsPruneBackfill: observer region
// edits, retention pruning and late from_pubkey backfills change rows BELOW
// the watermark. A delta refresh cannot see them; the periodic full rebuild
// does, exactly as for the heard cache.
func TestRegionPlacement488_RebuildNeededForEditsPruneBackfill(t *testing.T) {
	const late = "bbbb000000000002"
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, db *DB)
		after  []string // AAA members after the rebuild
	}{
		{"observer region edit", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`UPDATE observers SET iata = 'CCC' WHERE id = 'obs-AAA'`); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"retention prune", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`DELETE FROM observations`); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"late from_pubkey backfill", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`UPDATE transmissions SET from_pubkey = ? WHERE from_pubkey IS NULL`, late); err != nil {
				t.Fatal(err)
			}
		}, []string{rpm2PK, late}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := rpm2OpenTestDB(t)
			rpm2SetPlaced(db, defaultRegionPlacementParams)
			full, _ := rpm2CountScans(db)
			rpm2Insert(t, db, rpm2PK, 1, 1, rpm2T0, rpm2Str("1200aabbccdd"), rpm2Str("[]"))
			// An advert whose from_pubkey the ingestor has not filled yet.
			if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
				VALUES ('1200ccdd', 'h-late', '2026-10-01T00:00:00Z', 2, 4, '{}')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
				VALUES (2, 1, '[]', ?, '1200ccddeeff')`, rpm2T0); err != nil {
				t.Fatal(err)
			}
			before := []string{rpm2PK}
			if got := rpm2Members(t, db, "AAA"); !reflect.DeepEqual(got, before) {
				t.Fatalf("before: %v", got)
			}
			tc.mutate(t, db)
			if got := rpm2Refresh(t, db, "AAA"); !reflect.DeepEqual(got, before) {
				t.Fatalf("a delta refresh saw a change below the watermark: %v", got)
			}
			rpm2RebuildDue(db)
			if got := rpm2Refresh(t, db, "AAA"); !reflect.DeepEqual(got, tc.after) {
				t.Fatalf("after the rebuild: %v, want %v", got, tc.after)
			}
			if full.Load() != 2 {
				t.Fatalf("%d full scans, want 2 (cold + rebuild)", full.Load())
			}
		})
	}
}

// TestRegionPlacement488_QuietNodePlacedFromSQLHistory: a node last heard
// long before any RAM window is placed by a cold build, which reads SQLite
// only (no PacketStore exists here at all).
func TestRegionPlacement488_QuietNodePlacedFromSQLHistory(t *testing.T) {
	db := rpm2OpenTestDB(t)
	rpm2SetPlaced(db, defaultRegionPlacementParams)
	old := rpm2T0 - 120*rpm2Day
	rpm2Insert(t, db, rpm2PK, 1, 2, old, rpm2Str("1200aabbccdd"), rpm2Str("[]"))
	rpm2Insert(t, db, rpm2PK, 2, 2, old+2*rpm2Day, rpm2Str("1100aabbccdd"), rpm2Str("[]"))
	rpm2Insert(t, db, rpm2PK, 3, 1, old+2*rpm2Day, rpm2Str("1102a1a2aabbccdd"), rpm2Str(`["A1","A2"]`))
	if got := rpm2Members(t, db, "BBB"); !reflect.DeepEqual(got, []string{rpm2PK}) {
		t.Fatalf("quiet node: BBB members %v", got)
	}
	if got := rpm2Members(t, db, "AAA"); len(got) != 0 {
		t.Fatalf("quiet node: AAA members %v, want none", got)
	}
}

// TestRegionPlacement488_StartupSingleflight: concurrent first requests for
// different region sets share ONE full scan of the history, and all of them
// get the right membership.
func TestRegionPlacement488_StartupSingleflight(t *testing.T) {
	db := rpm2OpenTestDB(t)
	rpm2SetPlaced(db, defaultRegionPlacementParams)
	rpm2Insert(t, db, rpm2PK, 1, 1, rpm2T0, rpm2Str("1200aabbccdd"), rpm2Str("[]"))
	rpm2Insert(t, db, "bbbb000000000002", 2, 2, rpm2T0, rpm2Str("1100aabbccdd"), rpm2Str("[]"))
	release := make(chan struct{})
	var full, delta atomic.Int32
	db.regionPlaced.scanHook = func(isFull bool) error {
		if isFull {
			full.Add(1)
		} else {
			delta.Add(1)
		}
		<-release
		return nil
	}
	sets := [][]string{{"AAA"}, {"BBB"}, {"AAA", "BBB"}, {"CCC"}, {"AAA"}, {"BBB"}}
	want := [][]string{{rpm2PK}, {"bbbb000000000002"}, {rpm2PK, "bbbb000000000002"}, nil, {rpm2PK}, {"bbbb000000000002"}}
	got := make([][]string, len(sets))
	var wg sync.WaitGroup
	for i, codes := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = rpm2Members(t, db, codes...)
		}()
	}
	time.Sleep(50 * time.Millisecond) // let every request reach the flight
	close(release)
	wg.Wait()
	for i := range sets {
		if len(got[i]) == 0 && len(want[i]) == 0 {
			continue
		}
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%v: members %v, want %v", sets[i], got[i], want[i])
		}
	}
	if full.Load() != 1 || delta.Load() != 0 {
		t.Fatalf("startup: %d full and %d delta scans, want exactly 1 full", full.Load(), delta.Load())
	}
	// Warm: a fresh snapshot serves a new region set without scanning.
	rpm2Members(t, db, "CCC", "AAA")
	if full.Load() != 1 || delta.Load() != 0 {
		t.Fatalf("warm request scanned again: %d full, %d delta", full.Load(), delta.Load())
	}
}

// TestRegionPlacement488_FailedDeltaLeavesAggregateIntact: a delta scan
// that fails part-way must not fold half its rows in; the next refresh then
// equals a full build.
func TestRegionPlacement488_FailedDeltaLeavesAggregateIntact(t *testing.T) {
	db := rpm2OpenTestDB(t)
	rpm2SetPlaced(db, defaultRegionPlacementParams)
	rpm2Insert(t, db, rpm2PK, 1, 1, rpm2T0, rpm2Str("1101a1aabbccdd"), rpm2Str(`["A1"]`))
	rpm2Members(t, db, "AAA")
	rpm2Insert(t, db, rpm2PK, 2, 2, rpm2T0+60, rpm2Str("1200aabbccdd"), rpm2Str("[]"))
	db.regionPlaced.scanHook = func(bool) error { return errors.New("injected scan failure") }
	key := nodeRegionKey([]string{"AAA"})
	if e := db.getNodeRegionEntry(key); e != nil {
		c := *e
		c.refreshed = time.Now().Add(-2 * nodeRegionFreshTTL)
		db.setNodeRegionEntry(key, &c)
	}
	ageRegionIndex(&db.regionPlaced, 2*nodeRegionFreshTTL, 0)
	if _, err := db.refreshNodeRegion(key, []string{"AAA"}, true); err == nil {
		t.Fatal("refresh with a failing scan reported success")
	}
	db.regionPlaced.mu.Lock()
	wm, pairs := db.regionPlaced.agg.watermark, db.regionPlaced.agg.pairs
	db.regionPlaced.mu.Unlock()
	if wm != 1 || pairs != 1 {
		t.Fatalf("failed delta changed the aggregate: watermark %d, pairs %d", wm, pairs)
	}
	db.regionPlaced.scanHook = nil
	if got := rpm2Refresh(t, db, "AAA"); len(got) != 0 {
		t.Fatalf("after recovery: AAA members %v, want none", got)
	}
	db.regionPlaced.mu.Lock()
	inc := db.regionPlaced.agg
	db.regionPlaced.mu.Unlock()
	fresh := rpm2Scan(t, db, 0, rpm2MaxID(t, db), nil)
	if inc.pairs != fresh.pairs || inc.hearings != fresh.hearings ||
		!reflect.DeepEqual(rpm2Placements(inc, defaultRegionPlacementParams), rpm2Placements(fresh, defaultRegionPlacementParams)) {
		t.Fatalf("recovered aggregate differs from a full build: %v vs %v", inc.hearings, fresh.hearings)
	}
}

// TestRegionPlacement488_BoundedMemory: the aggregate grows with distinct
// (node, region) pairs, never with hearings or refreshes; the snapshot holds
// at most one key per placed membership; the per-set cache stays capped.
func TestRegionPlacement488_BoundedMemory(t *testing.T) {
	db := rpm2OpenTestDB(t)
	rpm2SetPlaced(db, defaultRegionPlacementParams)
	seq := 0
	hearAll := func(ts int64) {
		for n := range 5 {
			pk := fmt.Sprintf("%04x000000000000", n+1)
			for obs := 1; obs <= 3; obs++ {
				seq++
				rpm2Insert(t, db, pk, seq, obs, ts, rpm2Str(fmt.Sprintf("11%02xa1a2a3aabbccdd", obs%3)), rpm2Str("[]"))
			}
		}
	}
	hearAll(rpm2T0)
	rpm2Members(t, db, "AAA")
	for round := 1; round <= 20; round++ {
		hearAll(rpm2T0 + int64(round)*3600)
		rpm2Refresh(t, db, "AAA")
	}
	db.regionPlaced.mu.Lock()
	agg := db.regionPlaced.agg
	db.regionPlaced.mu.Unlock()
	if agg.pairs != 15 || len(agg.nodes) != 5 {
		t.Fatalf("aggregate grew with hearings: %d pairs, %d nodes; want 15 and 5", agg.pairs, len(agg.nodes))
	}
	if got := agg.hearings[hearingDirect] + agg.hearings[hearingRelayed] + agg.hearings[hearingFar]; got != int64(seq) {
		t.Fatalf("hearing counters %v, want %d hearings in all", agg.hearings, seq)
	}
	members := 0
	for _, keys := range db.regionPlaced.snap.Load().byRegion {
		members += len(keys)
	}
	if members > agg.pairs {
		t.Fatalf("snapshot holds %d keys for %d pairs", members, agg.pairs)
	}
	for i := range 2 * nodeRegionMaxEntries {
		rpm2Members(t, db, fmt.Sprintf("Z%02d", i))
	}
	db.nodeRegionCacheMu.Lock()
	n := len(db.nodeRegionCache)
	db.nodeRegionCacheMu.Unlock()
	if n > nodeRegionMaxEntries {
		t.Fatalf("placed mode cached %d region sets, cap %d", n, nodeRegionMaxEntries)
	}
}

// TestRegionPlacement488_V2Schema: the scan also runs on the legacy schema
// (observations.observer_id, ISO timestamps).
func TestRegionPlacement488_V2Schema(t *testing.T) {
	db := setupTestDBv2(t)
	t.Cleanup(func() { db.Close() })
	if db.isV3() {
		t.Fatal("setupTestDBv2 produced a v3 schema")
	}
	db.hasObsRawHexFlag.forceTrue()
	rpm2SetPlaced(db, defaultRegionPlacementParams)
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
	for _, o := range []struct{ obs, path, raw, ts string }{
		{"v2-AAA", "[]", "1100aabbccdd", "2026-10-01T00:00:00Z"},
		{"v2-BBB", `["A1","A2"]`, "1102a1a2aabbccdd", "2026-10-01T00:05:00Z"},
	} {
		if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, path_json, timestamp, raw_hex)
			VALUES (1, ?, ?, ?, ?, ?)`, o.obs, o.obs, o.path, o.ts, o.raw); err != nil {
			t.Fatal(err)
		}
	}
	if got := rpm2Members(t, db, "AAA"); !reflect.DeepEqual(got, []string{rpm2PK}) {
		t.Fatalf("v2: AAA members %v", got)
	}
	if got := rpm2Members(t, db, "BBB"); len(got) != 0 {
		t.Fatalf("v2: BBB members %v, want none", got)
	}
}

// ---- synthetic scenario with known ground truth ----

func TestRegionPlacement488_SyntheticAccuracy(t *testing.T) {
	db := rpm2EmptyTestDB(t)
	truth, transient := rpm2SeedScenario(t, db.conn, 1, rpm2T0)
	heard := rpm2RegionSets(func(r string) []string { return rpm2Members(t, db, r) })
	db.nodeRegionCacheMu.Lock()
	db.nodeRegionCache, db.nodeRegionUsed = nil, nil
	db.nodeRegionCacheMu.Unlock()

	params := defaultRegionPlacementParams
	params.PersistSpan = 24 * time.Hour
	rpm2SetPlaced(db, params)
	placed := rpm2RegionSets(func(r string) []string { return rpm2Members(t, db, r) })

	placedExact, placedWrong := rpm2Accuracy(truth, placed)
	heardExact, _ := rpm2Accuracy(truth, heard)
	t.Logf("synthetic: %d nodes; exact region set: placed %d, heard %d", len(truth), placedExact, heardExact)
	for _, pk := range placedWrong {
		t.Errorf("placed %s: got %v, want %v", pk, placed[pk], truth[pk])
	}
	db.regionPlaced.mu.Lock()
	pl := rpm2Placements(db.regionPlaced.agg, params)
	db.regionPlaced.mu.Unlock()
	for pk, want := range transient {
		if !reflect.DeepEqual(pl[pk].Transient, want) {
			t.Errorf("placed %s transient %v, want %v", pk, pl[pk].Transient, want)
		}
	}
	// Heard is right only for the 3 boundary nodes and the 4 scoped
	// repeaters, which are heard at home only.
	if len(truth) != 39 || heardExact != 7 {
		t.Errorf("pinned counts moved: nodes %d (want 39), heard exact %d (want 7)", len(truth), heardExact)
	}
	for pk, rs := range placed {
		for _, r := range rs {
			if !contains(heard[pk], r) {
				t.Errorf("placed %s in %s, where it was never heard", pk, r)
			}
		}
	}
}

// rpm2Regions lie on a line: AAA–BBB–CCC–DDD. A flood advert reaches a
// region d steps away after about 2d more relays.
var rpm2Regions = []string{"AAA", "BBB", "CCC", "DDD"}

const rpm2ObsPerRegion = 3

func rpm2RegionSets(members func(region string) []string) map[string][]string {
	out := map[string][]string{}
	for _, r := range rpm2Regions {
		for _, pk := range members(r) {
			out[pk] = append(out[pk], r)
		}
	}
	for pk := range out {
		sort.Strings(out[pk])
	}
	return out
}

func rpm2Accuracy(truth, got map[string][]string) (exact int, wrong []string) {
	for pk, want := range truth {
		g := got[pk]
		if len(g) == 0 && len(want) == 0 || reflect.DeepEqual(g, want) {
			exact++
		} else {
			wrong = append(wrong, pk)
		}
	}
	sort.Strings(wrong)
	return exact, wrong
}

// rpm2Exec is what the seeder writes through: a test connection or a
// transaction (the server's own handle is read-only in production).
type rpm2Exec interface {
	Exec(string, ...any) (sql.Result, error)
}

type rpm2Seeder struct {
	t    testing.TB
	exec rpm2Exec
	seq  int
}

func (s *rpm2Seeder) obs(region, i int) int { return region*rpm2ObsPerRegion + i + 1 }

func rpm2Payload(pk string, seq int) string { return pk + fmt.Sprintf("%08x", seq) + "00112233" }

func rpm2FloodRaw(header byte, codes string, pk string, seq, hops int) (raw, path string) {
	hs := make([]string, hops)
	b := strings.Builder{}
	fmt.Fprintf(&b, "%02x%s%02x", header, codes, hops)
	for i := range hops {
		hs[i] = fmt.Sprintf("%02X", 0xA0+i)
		b.WriteString(hs[i])
	}
	b.WriteString(rpm2Payload(pk, seq))
	if hops == 0 {
		return b.String(), "[]"
	}
	return b.String(), `["` + strings.Join(hs, `","`) + `"]`
}

func (s *rpm2Seeder) advert(pk string, ts int64, raw string) int64 {
	s.seq++
	decoded := `{"type":"ADVERT","pubKey":"` + pk + `"}`
	res, err := s.exec.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES (?, ?, ?, ?, 4, ?, ?)`, raw, fmt.Sprintf("rpm2-%d", s.seq),
		time.Unix(ts, 0).UTC().Format(time.RFC3339), int(raw[1]-'0')&3, decoded, pk)
	if err != nil {
		s.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (s *rpm2Seeder) hear(txID int64, obs int, ts int64, raw, path sql.NullString) {
	if _, err := s.exec.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex, snr, rssi)
		VALUES (?, ?, ?, ?, ?, 5, -100)`, txID, obs, path, ts, raw); err != nil {
		s.t.Fatal(err)
	}
}

// directAdvert: a zero-hop DIRECT advert heard by observers obs.
func (s *rpm2Seeder) directAdvert(pk string, ts int64, obs ...int) int64 {
	raw := "1200" + rpm2Payload(pk, s.seq+1)
	id := s.advert(pk, ts, raw)
	for _, o := range obs {
		s.hear(id, o, ts, rpm2Str(raw), rpm2Str("[]"))
	}
	return id
}

// floodAdvert: one flood advert; byRegion maps a region index to the hop
// count its observer 0 hears it at (header 0x11, or 0x10 with codes).
func (s *rpm2Seeder) floodAdvert(pk string, ts int64, header byte, codes string, byRegion map[int]int) {
	regions := make([]int, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Ints(regions)
	var id int64
	for i, r := range regions {
		raw, path := rpm2FloodRaw(header, codes, pk, s.seq+1, byRegion[r])
		if i == 0 {
			id = s.advert(pk, ts, raw)
		}
		s.hear(id, s.obs(r, 0), ts, rpm2Str(raw), rpm2Str(path))
	}
}

// rpm2Leak returns hop counts for a flood heard at h0 hops at home: +2 per
// region step away, as relays carry it down the line.
func rpm2Leak(home, h0 int) map[int]int {
	m := map[int]int{}
	for r := range rpm2Regions {
		d := r - home
		if d < 0 {
			d = -d
		}
		m[r] = h0 + 2*d
	}
	return m
}

// rpm2SeedScenario writes scale copies of each archetype per region and
// returns the expected home regions (nil = should be unknown) and the
// expected transient regions per node (with the transient rule on).
func rpm2SeedScenario(t testing.TB, exec rpm2Exec, scale int, now int64) (truth, transient map[string][]string) {
	t.Helper()
	s := &rpm2Seeder{t: t, exec: exec}
	for r, iata := range rpm2Regions {
		for i := range rpm2ObsPerRegion {
			if _, err := exec.Exec(`INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen)
				VALUES (?, ?, ?, ?, '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
				s.obs(r, i), fmt.Sprintf("obs-%s-%d", iata, i), fmt.Sprintf("Obs %s %d", iata, i), iata); err != nil {
				t.Fatal(err)
			}
		}
	}
	truth, transient = map[string][]string{}, map[string][]string{}
	at := func(d int64) int64 { return now - d*rpm2Day - 3600 }
	set := func(rs ...int) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = rpm2Regions[r]
		}
		sort.Strings(out)
		return out
	}
	for c := range scale {
		for r := range rpm2Regions {
			pk := func(arch int) string { return fmt.Sprintf("%02x%02x%012x", r, arch, c) }
			resident := func(p string, from, to int64) {
				for d := from; d <= to; d++ {
					s.directAdvert(p, at(d), s.obs(r, 0), s.obs(r, 1))
					s.floodAdvert(p, at(d)+60, 0x11, "", rpm2Leak(r, int(d%2)))
				}
			}
			// 1 resident repeater: zero-hop adverts at home, floods leak down the line.
			resident(pk(1), 0, 13)
			truth[pk(1)] = set(r)
			// 2 flood-only companion: never heard zero-hop; 1 hop at home.
			for d := int64(0); d < 5; d++ {
				s.floodAdvert(pk(2), at(d), 0x11, "", rpm2Leak(r, 1))
			}
			truth[pk(2)] = set(r)
			// 3 boundary node between r and r+1, heard directly in both.
			if r+1 < len(rpm2Regions) {
				for d := int64(0); d < 7; d++ {
					s.directAdvert(pk(3), at(d), s.obs(r, 0), s.obs(r+1, 0))
				}
				truth[pk(3)] = set(r, r+1)
			}
			// 4 resident with one long-distance direct hearing two regions away.
			far := (r + 2) % len(rpm2Regions)
			for d := int64(0); d < 10; d++ {
				id := s.directAdvert(pk(4), at(d), s.obs(r, 0))
				if d == 5 {
					raw := "1200" + rpm2Payload(pk(4), int(id))
					s.hear(id, s.obs(far, 1), at(d), rpm2Str(raw), rpm2Str("[]"))
				}
			}
			truth[pk(4)], transient[pk(4)] = set(r), set(far)
			// 5 resident whose advert a companion re-shares into another
			// region (TRANSPORT_DIRECT, codes {0,0}), as an extra observation
			// of the same transmission, on three days.
			other := (r + 3) % len(rpm2Regions)
			for d := int64(0); d < 10; d++ {
				id := s.directAdvert(pk(5), at(d), s.obs(r, 0))
				if d%3 == 1 {
					raw := "130000000000" + rpm2Payload(pk(5), int(id))
					s.hear(id, s.obs(other, 2), at(d)+600, rpm2Str(raw), rpm2Str("[]"))
				}
			}
			truth[pk(5)] = set(r)
			// 6 far-only: heard only after 5 relays.
			for d := int64(0); d < 5; d++ {
				s.floodAdvert(pk(6), at(d), 0x11, "", map[int]int{r: 5})
			}
			truth[pk(6)] = nil
			// 7 quiet resident: nothing in the last 90 days.
			resident(pk(7), 90, 99)
			truth[pk(7)] = set(r)
			// 8 moved from r to r+1 (mod 4) 60 days ago.
			next := (r + 1) % len(rpm2Regions)
			for d := int64(60); d < 100; d++ {
				s.directAdvert(pk(8), at(d), s.obs(r, 0))
			}
			for d := int64(0); d < 20; d++ {
				s.directAdvert(pk(8), at(d), s.obs(next, 0))
			}
			truth[pk(8)] = set(next)
			// 9 legacy rows only: NULL path_json, no frame.
			for d := int64(0); d < 5; d++ {
				id := s.advert(pk(9), at(d), "1100"+rpm2Payload(pk(9), int(d)))
				s.hear(id, s.obs(r, 2), at(d), rpm2Null(), rpm2Null())
			}
			truth[pk(9)] = nil
			// 10 scoped repeater: zero-hop TRANSPORT_FLOOD adverts at home.
			for d := int64(0); d < 10; d++ {
				s.floodAdvert(pk(10), at(d), 0x10, "1a2b3c4d", map[int]int{r: 0})
			}
			truth[pk(10)] = set(r)
		}
	}
	return truth, transient
}

// rpm2FileDB writes the scenario to a SQLite file the way the generator
// does (VACUUM INTO), for code that opens a path read-only.
func rpm2FileDB(t testing.TB, scale int, now int64) (string, map[string][]string) {
	t.Helper()
	db := setupTestDB(t)
	defer db.Close()
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	truth, _ := rpm2SeedScenario(t, tx, scale, now)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rpm2.db")
	if _, err := db.conn.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path, truth
}
