package main

// Tests for the #488 research prototype (region_placement_proto_488_test.go)
// and characterizations of the current code that the analysis on #488
// relies on. Characterization tests are marked as such: they pin today's
// behaviour so the claim is executable, and must be flipped by the
// milestone that changes it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func rp488Null() sql.NullString        { return sql.NullString{} }
func rp488Str(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
func rp488Ev(k rp488Kind, h int) rp488Evidence {
	return rp488Evidence{Kind: k, Hops: h, Verified: true}
}

// TestRP488_ClassifyRawRouteTypes covers every route type, both transport
// variants' 4-byte offset, hash sizes, and the malformed encodings.
func TestRP488_ClassifyRawRouteTypes(t *testing.T) {
	const pl = "aabbccddeeff0011" // stand-in payload bytes
	for _, tc := range []struct {
		name, raw string
		want      rp488Evidence
	}{
		{"flood zero-hop", "1100" + pl, rp488Ev(rp488Direct, 0)},
		{"flood zero-hop 2-byte hashes", "1140" + pl, rp488Ev(rp488Direct, 0)},
		{"flood 3 hops 1-byte", "1103a1a2a3" + pl, rp488Ev(rp488Relayed, 3)},
		{"flood 2 hops 2-byte", "1142a1a1a2a2" + pl, rp488Ev(rp488Relayed, 2)},
		{"flood 1 hop 3-byte", "1181a1a1a1" + pl, rp488Ev(rp488Relayed, 1)},
		{"transport flood zero-hop", "101a2b3c4d00" + pl, rp488Ev(rp488Direct, 0)},
		{"transport flood 2 hops", "101a2b3c4d02a1a2" + pl, rp488Ev(rp488Relayed, 2)},
		{"direct zero-hop", "1200" + pl, rp488Ev(rp488Direct, 0)},
		{"direct zero-hop 2-byte mode", "1240" + pl, rp488Ev(rp488Direct, 0)},
		{"direct with a path (hops to go, not travelled)", "1202a1a2" + pl, rp488Ev(rp488Unknown, 0)},
		{"transport direct, codes {0,0}: shared contact", "130000000000" + pl, rp488Ev(rp488Shared, 0)},
		// Byte 1 is 0xC0: read as a path byte it would be the reserved
		// hash size; at the real offset (5) the path byte is 0x00.
		{"transport direct zero-hop, non-zero codes", "13c000000000" + pl, rp488Ev(rp488Unknown, 0)},
		{"transport direct with path", "130000000001a1" + pl, rp488Ev(rp488Unknown, 0)},
		{"reserved hash size", "11c0" + pl, rp488Ev(rp488Unknown, 0)},
		{"path over MAX_PATH_SIZE", "117f" + strings.Repeat("a1", 126) + pl, rp488Ev(rp488Unknown, 0)},
		{"63 one-byte hops is valid", "113f" + strings.Repeat("a1", 63) + pl, rp488Ev(rp488Relayed, 63)},
		{"truncated path", "1103a1a2", rp488Ev(rp488Unknown, 0)},
		{"no payload after path", "1102a1a2", rp488Ev(rp488Unknown, 0)},
		{"truncated transport codes", "101a2b", rp488Ev(rp488Unknown, 0)},
		{"not an advert (GRP_TXT)", "1500" + pl, rp488Ev(rp488Unknown, 0)},
		{"bad hex", "zz00" + pl, rp488Ev(rp488Unknown, 0)},
		{"one byte", "11", rp488Ev(rp488Unknown, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rp488ClassifyRaw(tc.raw); got != tc.want {
				t.Errorf("rp488ClassifyRaw(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestRP488_ClassifyPathJSONNullVsEmpty: a missing path is unknown, an
// explicit empty path is a (legacy, unverified) direct hearing, and an
// observation's own raw bytes win over its path_json.
func TestRP488_ClassifyPathJSONNullVsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw, path sql.NullString
		want      rp488Evidence
	}{
		{"NULL path, no raw", rp488Null(), rp488Null(), rp488Evidence{Kind: rp488Unknown}},
		{"empty string path", rp488Null(), rp488Str(""), rp488Evidence{Kind: rp488Unknown}},
		{"json null path", rp488Null(), rp488Str("null"), rp488Evidence{Kind: rp488Unknown}},
		{"malformed path", rp488Null(), rp488Str("[\"A1\""), rp488Evidence{Kind: rp488Unknown}},
		{"explicit empty path", rp488Null(), rp488Str("[]"), rp488Evidence{Kind: rp488Direct}},
		{"two hops", rp488Null(), rp488Str(`["A1","B2"]`), rp488Evidence{Kind: rp488Relayed, Hops: 2}},
		{"empty raw falls back to path", rp488Str("  "), rp488Str("[]"), rp488Evidence{Kind: rp488Direct}},
		{"raw shared beats '[]'", rp488Str("130000000000aabbccdd"), rp488Str("[]"), rp488Ev(rp488Shared, 0)},
		{"raw relayed beats '[]' (backfill sentinel)", rp488Str("1102a1a2aabbccdd"), rp488Str("[]"), rp488Ev(rp488Relayed, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rp488ClassifyAdvert(tc.raw, tc.path); got != tc.want {
				t.Errorf("rp488ClassifyAdvert(%v, %v) = %+v, want %+v", tc.raw, tc.path, got, tc.want)
			}
		})
	}
}

const rp488Day = int64(24 * 3600)

// rp488Pattern builds per-region stats from (region, ts, evidence) triples.
type rp488Hearing struct {
	region string
	ts     int64
	ev     rp488Evidence
}

func rp488Stats1(hs ...rp488Hearing) map[string]*rp488Stats {
	m := map[string]*rp488Stats{}
	for _, h := range hs {
		if m[h.region] == nil {
			m[h.region] = &rp488Stats{}
		}
		m[h.region].add(h.ts, h.ev)
	}
	return m
}

func rp488Daily(region string, from, to int64, ev rp488Evidence) []rp488Hearing {
	var out []rp488Hearing
	for d := from; d <= to; d++ {
		out = append(out, rp488Hearing{region, 1_000*rp488Day + d*rp488Day, ev})
	}
	return out
}

func rp488Cat(parts ...[]rp488Hearing) []rp488Hearing {
	var out []rp488Hearing
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestRP488_PlaceRule(t *testing.T) {
	direct := rp488Ev(rp488Direct, 0)
	hop := func(n int) rp488Evidence { return rp488Ev(rp488Relayed, n) }
	base := int64(1_000 * rp488Day)
	for _, tc := range []struct {
		name string
		hs   []rp488Hearing
		p    rp488Params
		want rp488Placement
	}{
		{"relayed-only region does not leak",
			rp488Cat(rp488Daily("AAA", 0, 9, direct), rp488Daily("BBB", 0, 9, hop(2))),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"AAA"}}},
		{"directly heard flood advert counts as direct",
			rp488Cat(rp488Daily("AAA", 0, 9, rp488Ev(rp488Direct, 0)), rp488Daily("BBB", 0, 9, hop(1))),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"AAA"}}},
		{"two persistent direct regions are a set",
			rp488Cat(rp488Daily("AAA", 0, 6, direct), rp488Daily("BBB", 0, 6, direct)),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"AAA", "BBB"}}},
		{"one-off long-distance direct hearing is transient",
			rp488Cat(rp488Daily("AAA", 0, 9, direct), []rp488Hearing{{"DDD", base + 5*rp488Day, direct}}),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"AAA"}, Transient: []string{"DDD"}}},
		{"sustained second region is not transient",
			rp488Cat(rp488Daily("AAA", 0, 9, direct), []rp488Hearing{{"DDD", base + 2*rp488Day, direct}, {"DDD", base + 5*rp488Day, direct}}),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"AAA", "DDD"}}},
		{"no persistent region: all direct regions kept",
			[]rp488Hearing{{"AAA", base, direct}, {"BBB", base + 3600, direct}},
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"AAA", "BBB"}}},
		{"fewest hops wins, ties keep both",
			rp488Cat(rp488Daily("AAA", 0, 3, hop(2)), rp488Daily("BBB", 0, 3, hop(2)), rp488Daily("CCC", 0, 3, hop(3))),
			rp488DefaultParams, rp488Placement{Basis: "fewest_hops", Regions: []string{"AAA", "BBB"}, Hops: 2}},
		{"exactly the cap is placed",
			rp488Daily("AAA", 0, 3, hop(3)),
			rp488DefaultParams, rp488Placement{Basis: "fewest_hops", Regions: []string{"AAA"}, Hops: 3}},
		{"past the cap is unknown",
			rp488Cat(rp488Daily("AAA", 0, 3, hop(4)), rp488Daily("BBB", 0, 3, hop(9))),
			rp488DefaultParams, rp488Placement{Basis: "unknown", Hops: -1}},
		{"cap 0 places only direct evidence",
			rp488Daily("AAA", 0, 3, hop(1)),
			rp488Params{Lifetime: 30 * 24 * time.Hour, MaxHops: 0, PersistSpan: 24 * time.Hour},
			rp488Placement{Basis: "unknown", Hops: -1}},
		{"shared contacts and unknown paths never place",
			[]rp488Hearing{{"AAA", base, rp488Ev(rp488Shared, 0)}, {"BBB", base, rp488Evidence{Kind: rp488Unknown}}},
			rp488DefaultParams, rp488Placement{Basis: "unknown", Hops: -1}},
		{"quiet node keeps its placement (window anchored at its newest hearing)",
			rp488Daily("AAA", -200, -190, direct),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"AAA"}}},
		{"moved node: evidence older than the lifetime before its newest is dropped",
			rp488Cat(rp488Daily("AAA", -100, -60, direct), rp488Daily("BBB", -19, 0, direct)),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"BBB"}}},
		{"stale fallback does not outrank fresher, longer one",
			rp488Cat(rp488Daily("AAA", -100, -60, hop(1)), rp488Daily("BBB", -10, 0, hop(3))),
			rp488DefaultParams, rp488Placement{Basis: "fewest_hops", Regions: []string{"BBB"}, Hops: 3}},
		{"direct anywhere beats a closer-looking fallback here",
			rp488Cat(rp488Daily("AAA", 0, 9, hop(1)), rp488Daily("BBB", 0, 9, direct)),
			rp488DefaultParams, rp488Placement{Basis: "direct", Regions: []string{"BBB"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rp488Place(rp488Stats1(tc.hs...), tc.p)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rp488Place = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRP488_MembersUnionOfCodes: ?region=A,B returns each node once, placed
// in either region; a transient region alone does not make a member.
func TestRP488_MembersUnionOfCodes(t *testing.T) {
	pl := map[string]rp488Placement{
		"n1": {Basis: rp488BasisDirect, Regions: []string{"AAA", "BBB"}},
		"n2": {Basis: rp488BasisFewest, Regions: []string{"BBB"}, Hops: 2},
		"n3": {Basis: rp488BasisDirect, Regions: []string{"CCC"}, Transient: []string{"AAA"}},
		"n4": {Basis: rp488BasisUnknown, Hops: -1},
	}
	if got, want := rp488Members(pl, []string{"AAA", "BBB"}), []string{"n1", "n2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("members(AAA,BBB) = %v, want %v", got, want)
	}
	if got := rp488Members(pl, []string{"DDD"}); len(got) != 0 {
		t.Errorf("members(DDD) = %v, want none", got)
	}
}

func TestRP488_StatsAreBounded(t *testing.T) {
	s := &rp488Stats{}
	for i := 0; i < 10_000; i++ {
		s.add(int64(i), rp488Ev(rp488Relayed, 1+i%70))
	}
	if s.Far == 0 || s.LastAt[rp488HopSlots-1] == 0 {
		t.Fatalf("hops past the slots must be counted, not stored: %+v", s)
	}
	// The struct is fixed-size: one pair costs the same after 10k hearings.
	if got := len(s.LastAt); got != rp488HopSlots {
		t.Fatalf("LastAt grew to %d", got)
	}
	// Slot 0 is direct-only: a malformed "relayed at 0 hops" is unknown.
	z := &rp488Stats{}
	z.add(42, rp488Ev(rp488Relayed, 0))
	if z.LastAt[0] != 0 || z.Unknown != 1 {
		t.Fatalf("relayed at 0 hops reached the direct slot: %+v", z)
	}
}

// ---- DB-backed tests: the aggregate behind a watermark ----

const rp488PK = "aaaa000000000001"

// rp488EmptyTestDB is the shared in-memory test schema with observations.raw_hex.
func rp488EmptyTestDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	db.hasObsRawHexFlag.forceTrue()
	t.Cleanup(func() { db.Close() })
	return db
}

// rp488OpenTestDB adds one observer per region AAA, BBB, CCC (rowids 1-3).
func rp488OpenTestDB(t *testing.T) *DB {
	t.Helper()
	db := rp488EmptyTestDB(t)
	for i, iata := range []string{"AAA", "BBB", "CCC"} {
		if _, err := db.conn.Exec(`INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen)
			VALUES (?, ?, ?, ?, '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			i+1, "obs-"+iata, "Obs "+iata, iata); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// rp488Insert writes one ADVERT transmission by pk heard once by observer
// rowid obs, with the given per-observation raw and path. Returns the
// observation id.
func rp488Insert(t *testing.T, db *DB, pk string, seq, obs int, ts int64, raw, path sql.NullString) int64 {
	t.Helper()
	decoded := `{"type":"ADVERT","pubKey":"` + pk + `"}`
	res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES ('1100aabb', ?, '2026-10-01T00:00:00Z', 1, 4, ?)`, fmt.Sprintf("h-%s-%d", pk, seq), decoded)
	if err != nil {
		t.Fatal(err)
	}
	txID, _ := res.LastInsertId()
	res, err = db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
		VALUES (?, ?, ?, ?, ?)`, txID, obs, path, ts, raw)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func rp488Build(t *testing.T, db *DB, lo, hi int64, agg *rp488Agg) *rp488Agg {
	t.Helper()
	if agg == nil {
		agg = newRP488Agg()
	}
	if _, err := rp488ScanAgg(context.Background(), db, lo, hi, lo > 0, agg); err != nil {
		t.Fatal(err)
	}
	return agg
}

func rp488MaxID(t *testing.T, db *DB) int64 {
	t.Helper()
	wm, err := db.maxObservationID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return wm
}

const rp488T0 = int64(1_790_000_000) // 2026-09-21, fixed so tests are deterministic

func TestRP488_DeltaEqualsFullBuild(t *testing.T) {
	db := rp488EmptyTestDB(t)
	truth, _ := rp488SeedScenario(t, db.conn, 1, rp488T0)
	if len(truth) == 0 {
		t.Fatal("scenario seeded nothing")
	}
	wm := rp488MaxID(t, db)
	full := rp488Build(t, db, 0, wm, nil)
	if len(full.Nodes) != len(truth) {
		t.Fatalf("full build saw %d nodes, scenario has %d", len(full.Nodes), len(truth))
	}
	for _, mid := range []int64{1, wm / 3, wm / 2, wm - 1} {
		inc := rp488Build(t, db, 0, mid, nil)
		inc = rp488Build(t, db, mid, wm, inc)
		if !reflect.DeepEqual(rp488Placements(inc, rp488DefaultParams), rp488Placements(full, rp488DefaultParams)) {
			t.Fatalf("delta from %d differs from the full build", mid)
		}
		if inc.Watermark != wm {
			t.Fatalf("watermark %d, want %d", inc.Watermark, wm)
		}
	}
}

// TestRP488_NewObservationCanRemoveRegion: under the candidate rule a new
// direct hearing can take a node OUT of a region (its fallback regions are
// dropped). The current cache's extend step only unions keys ("membership
// only grows"), so the candidate must re-derive placements from the
// aggregate on every refresh. The second half characterizes the current
// cache doing exactly that union.
func TestRP488_NewObservationCanRemoveRegion(t *testing.T) {
	db := rp488OpenTestDB(t)
	rp488Insert(t, db, rp488PK, 1, 1, rp488T0, rp488Str("1101a1aabbccdd"), rp488Str(`["A1"]`)) // AAA, 1 hop
	agg := rp488Build(t, db, 0, rp488MaxID(t, db), nil)
	if got := rp488Members(rp488Placements(agg, rp488DefaultParams), []string{"AAA"}); !reflect.DeepEqual(got, []string{rp488PK}) {
		t.Fatalf("before: AAA members %v", got)
	}
	// Characterization: the current cache serves the node in AAA too.
	if !rp488CurrentHas(t, db, "AAA", rp488PK) {
		t.Fatal("current cache: node missing from AAA before the new observation")
	}

	wm := agg.Watermark
	rp488Insert(t, db, rp488PK, 2, 2, rp488T0+3600, rp488Str("1200aabbccdd"), rp488Str("[]")) // BBB, direct
	agg = rp488Build(t, db, wm, rp488MaxID(t, db), agg)
	pl := rp488Placements(agg, rp488DefaultParams)
	if got := rp488Members(pl, []string{"AAA"}); len(got) != 0 {
		t.Errorf("after: AAA members %v, want none (direct evidence elsewhere)", got)
	}
	if got := rp488Members(pl, []string{"BBB"}); !reflect.DeepEqual(got, []string{rp488PK}) {
		t.Errorf("after: BBB members %v", got)
	}

	// Characterization: a delta refresh of the current AAA entry keeps it.
	key := nodeRegionKey([]string{"AAA"})
	e := db.getNodeRegionEntry(key)
	stale := *e
	stale.refreshed = time.Now().Add(-2 * nodeRegionFreshTTL)
	db.setNodeRegionEntry(key, &stale)
	if _, err := db.refreshNodeRegion(key, []string{"AAA"}); err != nil {
		t.Fatal(err)
	}
	if !rp488CurrentHas(t, db, "AAA", rp488PK) {
		t.Error("current cache dropped the node on a delta: the union assumption changed, update #488's analysis")
	}
}

// TestRP488_RebuildNeededForEditsPruneBackfill: observer IATA edits,
// retention pruning and late from_pubkey backfills change rows BELOW the
// watermark, so a delta scan cannot see them and only the periodic full
// rebuild does — for the candidate exactly as for the current cache.
func TestRP488_RebuildNeededForEditsPruneBackfill(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, db *DB)
		before []string // AAA members before
		after  []string // AAA members after a full rebuild
	}{
		{"observer region edit", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`UPDATE observers SET iata = 'CCC' WHERE id = 'obs-AAA'`); err != nil {
				t.Fatal(err)
			}
		}, []string{rp488PK}, nil},
		{"retention prune", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`DELETE FROM observations`); err != nil {
				t.Fatal(err)
			}
		}, []string{rp488PK}, nil},
		{"late from_pubkey backfill", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`UPDATE transmissions SET from_pubkey = 'bbbb000000000002' WHERE from_pubkey IS NULL`); err != nil {
				t.Fatal(err)
			}
		}, []string{rp488PK}, []string{rp488PK, "bbbb000000000002"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := rp488OpenTestDB(t)
			rp488Insert(t, db, rp488PK, 1, 1, rp488T0, rp488Str("1200aabbccdd"), rp488Str("[]"))
			// An advert whose from_pubkey the ingestor has not filled yet.
			if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
				VALUES ('1200ccdd', 'h-late', '2026-10-01T00:00:00Z', 2, 4, '{}')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
				VALUES (2, 1, '[]', ?, '1200ccddeeff')`, rp488T0); err != nil {
				t.Fatal(err)
			}
			wm := rp488MaxID(t, db)
			agg := rp488Build(t, db, 0, wm, nil)
			if got := rp488Members(rp488Placements(agg, rp488DefaultParams), []string{"AAA"}); !reflect.DeepEqual(got, tc.before) {
				t.Fatalf("before: %v, want %v", got, tc.before)
			}
			tc.mutate(t, db)
			agg = rp488Build(t, db, wm, rp488MaxID(t, db), agg) // delta: nothing new
			if got := rp488Members(rp488Placements(agg, rp488DefaultParams), []string{"AAA"}); !reflect.DeepEqual(got, tc.before) {
				t.Fatalf("delta saw a change below the watermark: %v", got)
			}
			rebuilt := rp488Build(t, db, 0, rp488MaxID(t, db), nil)
			if got := rp488Members(rp488Placements(rebuilt, rp488DefaultParams), []string{"AAA"}); !reflect.DeepEqual(got, tc.after) {
				t.Fatalf("rebuild: %v, want %v", got, tc.after)
			}
		})
	}
}

// TestRP488_QuietNodePlacedFromSQLHistory: a node last heard long before any
// RAM window is placed by a cold build, which reads SQLite only.
func TestRP488_QuietNodePlacedFromSQLHistory(t *testing.T) {
	db := rp488OpenTestDB(t)
	old := rp488T0 - 120*rp488Day
	rp488Insert(t, db, rp488PK, 1, 2, old, rp488Str("1200aabbccdd"), rp488Str("[]"))
	rp488Insert(t, db, rp488PK, 2, 2, old+2*rp488Day, rp488Str("1100aabbccdd"), rp488Str("[]"))
	rp488Insert(t, db, rp488PK, 3, 1, old+2*rp488Day, rp488Str("1102a1a2aabbccdd"), rp488Str(`["A1","A2"]`))
	agg := rp488Build(t, db, 0, rp488MaxID(t, db), nil)
	got := rp488Placements(agg, rp488DefaultParams)[rp488PK]
	want := rp488Placement{Basis: "direct", Regions: []string{"BBB"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("quiet node placement %+v, want %+v", got, want)
	}
}

// ---- characterization of the current code ----

// rp488CurrentMembers is what /api/nodes?region= filters on today: the real
// membership cache.
func rp488CurrentMembers(t testing.TB, db *DB, codes []string) []string {
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

func rp488CurrentHas(t testing.TB, db *DB, region, pk string) bool {
	t.Helper()
	for _, k := range rp488CurrentMembers(t, db, []string{region}) {
		if k == pk {
			return true
		}
	}
	return false
}

// TestRP488_CurrentFilterIncludesRelayedOnly (characterization): today a
// node heard only through relays in a region is a member of it — the leak
// #488 is about. Flip in the milestone that switches the filter.
func TestRP488_CurrentFilterIncludesRelayedOnly(t *testing.T) {
	db := rp488OpenTestDB(t)
	rp488Insert(t, db, rp488PK, 1, 1, rp488T0, rp488Str("1200aabbccdd"), rp488Str("[]"))
	rp488Insert(t, db, rp488PK, 2, 2, rp488T0, rp488Str("1105a1a2a3a4a5aabbccdd"), rp488Str(`["A1","A2","A3","A4","A5"]`))
	if !rp488CurrentHas(t, db, "BBB", rp488PK) {
		t.Fatal("current filter no longer includes a relayed-only node: update #488's analysis")
	}
	agg := rp488Build(t, db, 0, rp488MaxID(t, db), nil)
	pl := rp488Placements(agg, rp488DefaultParams)
	if got := rp488Members(pl, []string{"BBB"}); len(got) != 0 {
		t.Fatalf("candidate places the relayed-only node in BBB: %v", got)
	}
	if got := rp488Members(pl, []string{"AAA"}); !reflect.DeepEqual(got, []string{rp488PK}) {
		t.Fatalf("candidate AAA members %v, want the directly heard node", got)
	}
}

// TestRP488_CurrentHomeRegionParsing (characterization): computeNodeHomeRegions
// (#804 analytics attribution) reads the path byte at offset 1 for
// TRANSPORT_DIRECT and decides on the transmission's first raw_hex, so
//   - a TRANSPORT_DIRECT advert with transport codes {0,0} (a contact
//     re-shared by a companion) counts as the advertised node's home;
//   - a zero-hop FLOOD advert does not count at all.
//
// The prototype classifier gets both right. Flip in milestone M1.
func TestRP488_CurrentHomeRegionParsing(t *testing.T) {
	db := rp488OpenTestDB(t)
	shared := "130000000000aabbccdd"
	floodZero := "1100aabbccdd"
	for i, c := range []struct {
		pk, raw string
		obs     int
	}{
		{"cccc000000000003", shared, 1},    // shared into AAA
		{"dddd000000000004", floodZero, 2}, // heard directly in BBB
	} {
		decoded := `{"type":"ADVERT","pubKey":"` + c.pk + `"}`
		if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
			VALUES (?, ?, '2026-10-01T00:00:00Z', ?, 4, ?)`, c.raw, fmt.Sprintf("h-char-%d", i), int(c.raw[1]-'0')&3, decoded); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
			VALUES (?, ?, '[]', ?, ?)`, i+1, c.obs, rp488T0, c.raw); err != nil {
			t.Fatal(err)
		}
	}
	store := NewPacketStore(db, nil)
	store.Load()
	obsIATA := store.observerIATAs()
	store.mu.RLock()
	home := store.computeNodeHomeRegions(obsIATA)
	store.mu.RUnlock()

	if home["cccc000000000003"] != "AAA" {
		t.Errorf("current: shared contact home = %q; the analysis says it is counted as AAA", home["cccc000000000003"])
	}
	if _, ok := home["dddd000000000004"]; ok {
		t.Errorf("current: zero-hop flood advert now counts (%q); update the analysis", home["dddd000000000004"])
	}
	if got := rp488ClassifyRaw(shared); got.Kind != rp488Shared {
		t.Errorf("prototype: shared contact classified %v", got.Kind)
	}
	if got := rp488ClassifyRaw(floodZero); got.Kind != rp488Direct {
		t.Errorf("prototype: zero-hop flood classified %v", got.Kind)
	}
}
