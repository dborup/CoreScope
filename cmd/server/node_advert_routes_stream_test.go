package main

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// PR #97 review P2-1: GetNodeAdvertRoutes is one index-ordered pass over the
// node's ADVERT rows, classified and limited per class in Go. The window
// query it replaced (ROW_NUMBER() per class in SQL) stays here as a
// test-only oracle, and so does its SQL classifier; the stream must agree
// with both on randomized data, and its flood_advert_count_7d must agree bit
// for bit with CountFloodAdvertsForNode.

// advertRouteClassSQL is classifyAdvertRoute as a SQL expression over the
// unaliased transmissions columns, yielding the route class name. Without
// the route_mask column every row takes the route_type fallback.
// TestAdvertRouteClassSQL_MatchesGoClassifier pins it to the Go rule.
func advertRouteClassSQL(hasRouteMask bool) string {
	fallback := fmt.Sprintf(`WHEN route_type IN (%d, %d) THEN '%s' WHEN route_type IN (%d, %d) THEN '%s' ELSE '%s'`,
		RouteTransportFlood, RouteFlood, advertClassFlood,
		RouteDirect, RouteTransportDirect, advertClassZeroHop, advertClassUnknown)
	if !hasRouteMask {
		return "(CASE " + fallback + " END)"
	}
	flood, direct := packetpath.RouteMaskFlood, packetpath.RouteMaskDirect
	return fmt.Sprintf(`(CASE WHEN route_mask & %d <> 0 THEN (CASE WHEN route_mask & %d <> 0 AND route_mask & %d <> 0 THEN '%s' WHEN route_mask & %d <> 0 THEN '%s' ELSE '%s' END) %s END)`,
		packetpath.RouteMaskAll, flood, direct, advertClassMixed, flood, advertClassFlood, advertClassZeroHop, fallback)
}

// oracleNodeAdvertRoutesWindow is the replaced implementation: the class
// computed in SQL and ROW_NUMBER() per class, then the same lean-row fetch.
// oracleScanWindow is its query and count loop, verbatim but for returning
// the listed ids instead of fetching them.
func oracleNodeAdvertRoutesWindow(db *DB, pubkey string, perClass int, now time.Time, rowCap int) (NodeAdvertsByRoute, NodeAdvertCounts, error) {
	byRoute := NodeAdvertsByRoute{Limit: perClass, Flood: NodeAdvertRows{}, ZeroHop: NodeAdvertRows{}, Mixed: NodeAdvertRows{}}
	sc, err := oracleScanWindow(db, pubkey, perClass, now, rowCap)
	if err != nil || len(sc.ids) == 0 {
		return byRoute, sc.counts, err
	}
	listed := map[int]string{}
	var ids []interface{}
	for i, id := range sc.ids {
		listed[id] = sc.classes[i]
		ids = append(ids, id)
	}
	full, err := db.queryNodeAdvertRows(false, false, "t.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+") ORDER BY t.id DESC", ids...)
	if err != nil {
		return byRoute, sc.counts, err
	}
	for _, p := range full {
		id, _ := p["id"].(int)
		p["route_class"] = listed[id]
		switch listed[id] {
		case advertClassFlood:
			byRoute.Flood = append(byRoute.Flood, p)
		case advertClassZeroHop:
			byRoute.ZeroHop = append(byRoute.ZeroHop, p)
		case advertClassMixed:
			byRoute.Mixed = append(byRoute.Mixed, p)
		default:
			byRoute.Unknown = append(byRoute.Unknown, p)
		}
	}
	return byRoute, sc.counts, nil
}

func oracleScanWindow(db *DB, pubkey string, perClass int, now time.Time, rowCap int) (nodeAdvertScan, error) {
	var sc nodeAdvertScan
	floor := advertDateFloor(now, 7*24)
	class := advertRouteClassSQL(db.hasRouteMask())
	rows, err := db.conn.Query(`SELECT id, cls, rn, first_seen, hash FROM (
			SELECT id, cls, COALESCE(first_seen, '') AS first_seen, COALESCE(hash, '') AS hash,
				ROW_NUMBER() OVER (PARTITION BY cls ORDER BY id DESC) AS rn
			FROM (SELECT id, first_seen, hash, `+class+` AS cls FROM transmissions WHERE from_pubkey = ? AND payload_type = ?)
		) WHERE rn <= ? OR first_seen >= ?
		ORDER BY id DESC`, pubkey, payloadTypeAdvert, perClass, floor)
	if err != nil {
		return sc, err
	}
	defer rows.Close()
	var entries []advertRouteEntry
	for rows.Next() {
		var id, rn int
		var cls, ts, hash string
		if err := rows.Scan(&id, &cls, &rn, &ts, &hash); err != nil {
			return sc, err
		}
		if rn <= perClass {
			sc.ids = append(sc.ids, id)
			sc.classes = append(sc.classes, cls)
		}
		if ts >= floor {
			if len(entries) < rowCap {
				entries = append(entries, advertRouteEntry{ts: ts, hash: hash, class: cls})
			} else {
				sc.counts.Truncated = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return sc, err
	}
	sc.counts.H24 = countAdvertRoutes(entries, now, 24)
	sc.counts.D7 = countAdvertRoutes(entries, now, 7*24)
	return sc, nil
}

// narRelaxTransmissions rebuilds transmissions without NOT NULL / UNIQUE, so
// the randomized data can hold what the parity must also survive: duplicate
// and NULL hashes (the dedup paths) and NULL first_seen. It leaves out
// idx_transmissions_payload_type, which would put the verbatim oracle query
// on a scan of every ADVERT (TestNodeAdvertRoutes_StreamQueryPlan covers the
// stream with that index present).
func narRelaxTransmissions(t *testing.T, db *DB) {
	t.Helper()
	rows, err := db.conn.Query(`SELECT name, type FROM pragma_table_info('transmissions') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		if name == "id" {
			cols = append(cols, "id INTEGER PRIMARY KEY AUTOINCREMENT")
		} else {
			cols = append(cols, name+" "+typ)
		}
	}
	rows.Close()
	for _, q := range []string{
		`DROP TABLE transmissions`,
		`CREATE TABLE transmissions (` + strings.Join(cols, ", ") + `)`,
		`CREATE INDEX idx_transmissions_from_pubkey ON transmissions(from_pubkey)`,
		`CREATE INDEX idx_transmissions_first_seen ON transmissions(first_seen)`,
	} {
		if _, err := db.conn.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

type narParityNode struct {
	pubkey   string
	perClass int
	rowCap   int
}

type narParityVariant struct {
	name     string
	withMask bool // the route_mask column exists
	relaxed  bool // duplicate/NULL hashes and NULL first_seen allowed
	nodes    int
}

// 1,000 random nodes over three schemas: migrated with and without the
// relaxed constraints, and a legacy database without route_mask.
var narParityVariants = []narParityVariant{
	{"relaxed, route_mask", true, true, 500},
	{"relaxed, no route_mask column", false, true, 250},
	{"production schema, route_mask", true, false, 250},
}

// narParityTS returns a first_seen value around now in one of the formats
// seen in the wild (plus garbage, empty, date-only and window-edge values).
func narParityTS(rng *rand.Rand, now time.Time) interface{} {
	age := time.Duration(rng.Int63n(int64(10*24*time.Hour))) - time.Hour
	switch rng.Intn(12) {
	case 0:
		return now.Add(-24*time.Hour + time.Duration(rng.Intn(3)-1)*time.Second).Format(time.RFC3339)
	case 1:
		return now.Add(-7*24*time.Hour + time.Duration(rng.Intn(3)-1)*time.Second).Format(time.RFC3339)
	case 2:
		return advertDateFloor(now, 7*24) + []string{"", "T00:00:00Z", " 00:00:00", "T23:59:59.999Z"}[rng.Intn(4)]
	case 3:
		return now.Add(-age).Format(time.RFC3339Nano)
	case 4:
		return now.Add(-age).Format("2006-01-02T15:04:05.000Z")
	case 5:
		return now.Add(-age).In(time.FixedZone("CEST", 2*3600)).Format(time.RFC3339)
	case 6:
		return now.Add(-age).Format("2006-01-02 15:04:05")
	case 7:
		return []interface{}{"garbage", "", "zzzz", nil}[rng.Intn(4)]
	default:
		return now.Add(-age).Format(time.RFC3339)
	}
}

// narSeedParity seeds v.nodes random nodes (ids interleaved across nodes)
// and returns them with a per-class limit and row cap each.
func narSeedParity(t *testing.T, db *DB, rng *rand.Rand, v narParityVariant, now time.Time) []narParityNode {
	t.Helper()
	routeTypes := []interface{}{nil, 0, 1, 1, 1, 2, 2, 3, 4, 99, -1, 1.5, "x", "1"}
	masks := []interface{}{nil, nil, 0, -1, 16, "6", "abc"}
	for m := 1; m < 32; m++ {
		masks = append(masks, m)
	}
	type row struct {
		pubkey, hash interface{}
		firstSeen    interface{}
		routeType    interface{}
		payloadType  interface{}
		mask         interface{}
	}
	var all []row
	nodes := make([]narParityNode, v.nodes)
	serial := 0
	for n := range nodes {
		pk := fmt.Sprintf("%064x", rng.Uint64()) + fmt.Sprintf("-%s-%d", v.name, n)
		nodes[n] = narParityNode{pubkey: pk, perClass: nodeAdvertRouteLimit, rowCap: floodAdvertRowCap}
		if rng.Intn(4) == 0 {
			nodes[n].perClass = 1 + rng.Intn(5)
		}
		if rng.Intn(2) == 0 {
			nodes[n].rowCap = []int{1, 2, 3, 7, 15, 40}[rng.Intn(6)]
		}
		count := rng.Intn(70)
		switch rng.Intn(10) {
		case 0:
			count = rng.Intn(4)
		case 1:
			count = 150 + rng.Intn(50)
		}
		dupPool := 1 + rng.Intn(6)
		for i := 0; i < count; i++ {
			serial++
			r := row{pubkey: pk, firstSeen: narParityTS(rng, now), routeType: routeTypes[rng.Intn(len(routeTypes))], payloadType: payloadTypeAdvert}
			if v.withMask {
				r.mask = masks[rng.Intn(len(masks))]
			}
			if rng.Intn(8) == 0 {
				r.payloadType = []interface{}{5, 2, nil}[rng.Intn(3)]
			}
			r.hash = fmt.Sprintf("u%d", serial)
			if v.relaxed {
				switch rng.Intn(6) {
				case 0:
					r.hash = fmt.Sprintf("%s-dup%d", pk, rng.Intn(dupPool))
				case 1:
					r.hash = []interface{}{"", nil}[rng.Intn(2)]
				}
			} else if r.firstSeen == nil {
				r.firstSeen = "garbage"
			}
			all = append(all, r)
		}
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if v.withMask {
			_, err = tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey, route_mask)
				VALUES ('1100', ?, ?, ?, ?, '{"type":"ADVERT"}', ?, ?)`, r.hash, r.firstSeen, r.routeType, r.payloadType, r.pubkey, r.mask)
		} else {
			_, err = tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
				VALUES ('1100', ?, ?, ?, ?, '{"type":"ADVERT"}', ?)`, r.hash, r.firstSeen, r.routeType, r.payloadType, r.pubkey)
		}
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nodes
}

func narParityDB(t *testing.T, v narParityVariant) *DB {
	t.Helper()
	db := setupTestDB(t)
	if v.relaxed {
		narRelaxTransmissions(t, db)
	}
	if v.withMask {
		narAddRouteMask(t, db)
	}
	return db
}

// The stream returns exactly what the window query returned - the same
// listed ids, order and classes and the same counts and truncation - for
// 1,000 random nodes with mixed, NULL, out-of-range and mistyped masks and
// route types, every first_seen format, duplicate and missing hashes,
// non-advert rows, small per-class limits and row caps. The scans are
// compared for every node; the full responses (same lean-row fetch) for
// every 25th.
func TestNodeAdvertRoutes_StreamMatchesWindowOracle(t *testing.T) {
	now := time.Now().UTC()
	var total, full, truncated, limited, withUnknown, withMixed int
	for vi, v := range narParityVariants {
		db := narParityDB(t, v)
		rng := rand.New(rand.NewSource(int64(2073 + vi)))
		for i, n := range narSeedParity(t, db, rng, v, now) {
			want, err := oracleScanWindow(db, n.pubkey, n.perClass, now, n.rowCap)
			if err != nil {
				t.Fatalf("%s: oracle: %v", v.name, err)
			}
			got, err := db.scanNodeAdvertRoutes(n.pubkey, n.perClass, now, n.rowCap)
			if err != nil {
				t.Fatalf("%s: stream: %v", v.name, err)
			}
			if !reflect.DeepEqual(got.ids, want.ids) || !reflect.DeepEqual(got.classes, want.classes) {
				t.Fatalf("%s node %s (perClass=%d): lists differ\nstream %v %v\noracle %v %v", v.name, n.pubkey, n.perClass, got.ids, got.classes, want.ids, want.classes)
			}
			if got.counts != want.counts {
				t.Fatalf("%s node %s (rowCap=%d): counts differ\nstream %+v\noracle %+v", v.name, n.pubkey, n.rowCap, got.counts, want.counts)
			}
			if i%25 == 0 {
				wantBR, wantCounts, err := oracleNodeAdvertRoutesWindow(db, n.pubkey, n.perClass, now, n.rowCap)
				if err != nil {
					t.Fatal(err)
				}
				gotBR, gotCounts, _, err := db.GetNodeAdvertRoutes(n.pubkey, n.perClass, now, n.rowCap)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotBR, wantBR) || gotCounts != wantCounts {
					t.Fatalf("%s node %s: responses differ\nstream flood=%v zero_hop=%v mixed=%v unknown=%v\noracle flood=%v zero_hop=%v mixed=%v unknown=%v",
						v.name, n.pubkey, narHashes(gotBR.Flood), narHashes(gotBR.ZeroHop), narHashes(gotBR.Mixed), narHashes(gotBR.Unknown),
						narHashes(wantBR.Flood), narHashes(wantBR.ZeroHop), narHashes(wantBR.Mixed), narHashes(wantBR.Unknown))
				}
				full++
			}
			total++
			perClass := map[string]int{}
			for _, c := range want.classes {
				perClass[c]++
			}
			if want.counts.Truncated {
				truncated++
			}
			if perClass[advertClassZeroHop] == n.perClass || perClass[advertClassFlood] == n.perClass {
				limited++
			}
			if perClass[advertClassUnknown] > 0 {
				withUnknown++
			}
			if perClass[advertClassMixed] > 0 {
				withMixed++
			}
		}
	}
	t.Logf("compared %d nodes (%d full responses): %d truncated, %d at a per-class limit, %d with unknown, %d with mixed", total, full, truncated, limited, withUnknown, withMixed)
	if total < 1000 || full < 40 || truncated == 0 || limited == 0 || withUnknown == 0 || withMixed == 0 {
		t.Fatalf("the random data must exercise every path (nodes=%d full=%d truncated=%d limited=%d unknown=%d mixed=%d)", total, full, truncated, limited, withUnknown, withMixed)
	}
}

// The stream's flood count is CountFloodAdvertsForNode(pubkey, 7d, rowCap)
// bit for bit at the same instant: route_type = 1 and the first_seen floor
// evaluated by SQL as there, the newest rowCap flood rows inside the floor,
// the same dedup and parseRelayTS window.
func TestNodeAdvertRoutes_FloodCountMatchesCountFloodAdvertsForNode(t *testing.T) {
	now := time.Now().UTC()
	floor := advertDateFloor(now, 7*24)
	var total, nonZero, capped, skipped, duplicated int
	for vi, v := range narParityVariants {
		db := narParityDB(t, v)
		rng := rand.New(rand.NewSource(int64(4073 + vi)))
		nodes := narSeedParity(t, db, rng, v, now)
		// Per node: route_type 1 rows inside the floor, and hash groups
		// (NULL and empty together) holding more than one of them.
		type stat struct{ rows, dupGroups int }
		stats := map[string]stat{}
		rows, err := db.conn.Query(`SELECT from_pubkey, SUM(n), SUM(n > 1) FROM (
				SELECT from_pubkey, COUNT(*) AS n FROM transmissions
				WHERE payload_type = ? AND route_type = 1 AND first_seen >= ?
				GROUP BY from_pubkey, COALESCE(hash, '')) GROUP BY from_pubkey`, payloadTypeAdvert, floor)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var pk string
			var st stat
			if err := rows.Scan(&pk, &st.rows, &st.dupGroups); err != nil {
				t.Fatal(err)
			}
			stats[pk] = st
		}
		rows.Close()
		for _, n := range nodes {
			want, err := db.countFloodAdvertsForNodeAt(n.pubkey, 7*24, n.rowCap, now)
			if err != nil {
				t.Fatalf("%s: CountFloodAdvertsForNode: %v", v.name, err)
			}
			got, err := db.scanNodeAdvertRoutes(n.pubkey, n.perClass, now, n.rowCap)
			if err != nil {
				t.Fatalf("%s: stream: %v", v.name, err)
			}
			if got.flood7d != want {
				t.Fatalf("%s node %s (rowCap=%d): flood_advert_count_7d stream=%d CountFloodAdvertsForNode=%d", v.name, n.pubkey, n.rowCap, got.flood7d, want)
			}
			total++
			st := stats[n.pubkey]
			if want > 0 {
				nonZero++
			}
			if st.rows > n.rowCap {
				capped++ // the row cap binds
			} else if st.rows > want {
				skipped++ // rows dropped by the exact window, parseRelayTS or dedup
			}
			if st.dupGroups > 0 {
				duplicated++ // same (or missing) hash on several flood rows: the dedup path
			}
		}
	}
	t.Logf("compared %d nodes: %d with flood adverts, %d where the row cap binds, %d with window/parse/dedup skips, %d with duplicate hashes", total, nonZero, capped, skipped, duplicated)
	if total < 1000 || nonZero == 0 || capped == 0 || skipped == 0 || duplicated == 0 {
		t.Fatalf("the random data must exercise every path (nodes=%d nonzero=%d capped=%d skipped=%d duplicated=%d)", total, nonZero, capped, skipped, duplicated)
	}
}

// The stream reads the node's rows in index order: from_pubkey index, newest
// id first, no temp B-tree sort and no window - also without ANALYZE
// statistics and with idx_transmissions_payload_type created after the
// from_pubkey index, where SQLite would otherwise walk every ADVERT via the
// payload_type index (the unary + in nodeAdvertScanSQL).
func TestNodeAdvertRoutes_StreamQueryPlan(t *testing.T) {
	for _, withMask := range []bool{true, false} {
		db := setupTestDB(t)
		if withMask {
			narAddRouteMask(t, db)
		}
		if _, err := db.conn.Exec(`CREATE INDEX idx_transmissions_payload_type ON transmissions(payload_type)`); err != nil {
			t.Fatal(err)
		}
		floor := advertDateFloor(time.Now(), 7*24)
		rows, err := db.conn.Query(`EXPLAIN QUERY PLAN `+nodeAdvertScanSQL(withMask), floor, advertRouteTypeFlood, narNode, payloadTypeAdvert)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, " | ")
		if !strings.Contains(joined, "idx_transmissions_from_pubkey") || strings.Contains(joined, "TEMP B-TREE") {
			t.Fatalf("withMask=%v: plan = %s, want an index-ordered from_pubkey scan", withMask, joined)
		}
		if strings.Contains(strings.ToUpper(nodeAdvertScanSQL(withMask)), "OVER") {
			t.Fatal("the stream must not use a window function")
		}
	}
}
