package main

// PR #38 (port of upstream #1882): the /api/nodes region filter reads
// transmissions.from_pubkey instead of JSON_EXTRACT(decoded_json, '$.pubKey').
// These tests lock the claim that justifies the swap — the region filter
// returns exactly the node set the JSON_EXTRACT expression returned — and the
// one behaviour that changed on purpose: a corrupt decoded_json no longer
// fails the whole query.
//
// The helpers in db_test.go install trigger test_from_pubkey_advert, which
// copies decoded_json.pubKey into from_pubkey on every ADVERT insert. That
// makes the two expressions equal by construction, so a test built on them
// cannot tell the variants apart. The tests here drop the trigger and write
// from_pubkey the way each real write path does.

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// regionNodesByJSONExtract is the oracle: the node set the region filter
// returned before the swap, built with the pre-#38 expression. It is written
// out here on purpose — it is the reference the real GetNodes is compared to,
// not a copy of the code under test.
func regionNodesByJSONExtract(t *testing.T, db *DB, region string) []string {
	t.Helper()
	codes := normalizeRegionCodes(region)
	if len(codes) == 0 {
		t.Fatalf("region %q normalizes to no codes", region)
	}
	joinCond := "obs.rowid = o.observer_idx"
	if !db.isV3() {
		joinCond = "obs.id = o.observer_id"
	}
	args := make([]interface{}, len(codes))
	for i, c := range codes {
		args[i] = c
	}
	q := `SELECT public_key FROM nodes WHERE public_key IN (
		SELECT DISTINCT JSON_EXTRACT(t.decoded_json, '$.pubKey')
		FROM transmissions t
		JOIN observations o ON o.transmission_id = t.id
		JOIN observers obs ON ` + joinCond + `
		WHERE t.payload_type = 4
		AND UPPER(TRIM(obs.iata)) IN (?` + strings.Repeat(",?", len(codes)-1) + `)
	)`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		t.Fatalf("oracle query for %q: %v", region, err)
	}
	defer rows.Close()
	var out []string
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

// regionNodesByGetNodes runs the real region filter and returns the sorted
// public keys. The limit is far above any test's node count, and total must
// agree with the page, so nothing is hidden by paging.
func regionNodesByGetNodes(t *testing.T, db *DB, region string) []string {
	t.Helper()
	nodes, total, _, err := db.GetNodes(100000, 0, "", "", "", "", "", region)
	if err != nil {
		t.Fatalf("GetNodes(region=%q): %v", region, err)
	}
	if total != len(nodes) {
		t.Fatalf("GetNodes(region=%q): total=%d but %d rows returned", region, total, len(nodes))
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		pk, _ := n["public_key"].(string)
		out = append(out, pk)
	}
	sort.Strings(out)
	return out
}

// assertSameNodeSet reports only the difference, so a failure on the fixture
// names the lost nodes instead of printing every key twice.
func assertSameNodeSet(t *testing.T, region string, got, want []string) {
	t.Helper()
	inGot := make(map[string]bool, len(got))
	for _, pk := range got {
		inGot[pk] = true
	}
	var missing []string
	for _, pk := range want {
		if !inGot[pk] {
			missing = append(missing, pk)
		}
		delete(inGot, pk)
	}
	var extra []string
	for pk := range inGot {
		extra = append(extra, pk)
	}
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("region %q: GetNodes %d nodes, JSON_EXTRACT oracle %d; missing %v, extra %v",
			region, len(got), len(want), missing, extra)
	}
}

// On the committed e2e fixture (real staging data) every region, alone and
// combined, returns the same node set as the JSON_EXTRACT expression did.
func TestGetNodesRegionFilter_FixtureMatchesJSONExtract_PR38(t *testing.T) {
	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "fixture.db")
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.conn.Query(`SELECT DISTINCT UPPER(TRIM(iata)) FROM observers WHERE iata IS NOT NULL AND TRIM(iata) <> '' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var regions []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		regions = append(regions, r)
	}
	rows.Close()
	if len(regions) < 2 {
		t.Fatalf("fixture has %d regions, want at least 2 to exercise the filter", len(regions))
	}
	// Multi-region, lower-case with padding (normalizeRegionCodes), and a
	// code no observer carries.
	cases := append([]string{}, regions...)
	cases = append(cases, strings.Join(regions, ","), " "+strings.ToLower(regions[0])+" , "+regions[1], "ZZZ")

	nonEmpty := 0
	for _, region := range cases {
		got := regionNodesByGetNodes(t, db, region)
		assertSameNodeSet(t, region, got, regionNodesByJSONExtract(t, db, region))
		if len(got) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty < 2 {
		t.Fatalf("only %d region cases matched any node; the comparison is vacuous", nonEmpty)
	}
}

// setupRegionFromPubkeyDB is the v3 test schema without the auto-populate
// trigger, so from_pubkey holds exactly what the test writes.
func setupRegionFromPubkeyDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	if _, err := db.conn.Exec(`DROP TRIGGER test_from_pubkey_advert`); err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO observers (rowid, id, name, iata) VALUES (1, 'obs-sjc', 'SJC obs', 'SJC'), (2, 'obs-sfo', 'SFO obs', ' sfo ')`)
	for _, pk := range []string{"aa00000000000001", "bb00000000000002", "cc00000000000003", "dd00000000000004"} {
		mustExec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen) VALUES (?, ?, 'repeater', '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`, pk, "n-"+pk[:2])
	}
	return db
}

// regionTx inserts one transmission and its observations, one per observer rowid.
func regionTx(t *testing.T, db *DB, hash string, payloadType int, decoded string, fromPubkey sql.NullString, observerIdx ...int) {
	t.Helper()
	res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES ('00', ?, '2026-10-01T00:00:00Z', 1, ?, ?, ?)`, hash, payloadType, decoded, fromPubkey)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for i, idx := range observerIdx {
		if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp) VALUES (?, ?, ?)`, id, idx, 1790000000+i); err != nil {
			t.Fatal(err)
		}
	}
}

func advertJSON(pk string) string {
	return `{"type":"ADVERT","pubKey":"` + pk + `","name":"n"}`
}

// The row shapes production writes, side by side: ADVERTs the ingestor
// stamped with their pubkey, an ADVERT that carried no pubkey (left NULL by
// the guard in cmd/ingestor/db.go — the 25 such rows on staging), a legacy
// pubkey-less ADVERT the backfill marked with the "" sentinel, and a
// non-ADVERT. The region filter must return the JSON_EXTRACT node set for
// every region, and the explicit expected set besides.
func TestGetNodesRegionFilter_WritePathShapesMatchJSONExtract_PR38(t *testing.T) {
	db := setupRegionFromPubkeyDB(t)
	defer db.Close()
	pk := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	null := sql.NullString{}

	regionTx(t, db, "adv-a-sjc", 4, advertJSON("aa00000000000001"), pk("aa00000000000001"), 1)
	regionTx(t, db, "adv-b-sfo", 4, advertJSON("bb00000000000002"), pk("bb00000000000002"), 2)
	regionTx(t, db, "adv-c-both", 4, advertJSON("cc00000000000003"), pk("cc00000000000003"), 1, 2)
	regionTx(t, db, "adv-nopk-null", 4, `{"type":"ADVERT","name":"no key"}`, null, 1, 2)
	regionTx(t, db, "adv-nopk-backfilled", 4, `{"type":"ADVERT"}`, pk(""), 1, 2)
	// dd is heard in SJC only through a non-ADVERT, which never places a
	// node in a region either way.
	regionTx(t, db, "txt-d-sjc", 2, `{"type":"TXT_MSG","pubKey":"dd00000000000004"}`, null, 1)

	for _, tc := range []struct {
		region string
		want   []string
	}{
		{"SJC", []string{"aa00000000000001", "cc00000000000003"}},
		{"SFO", []string{"bb00000000000002", "cc00000000000003"}},
		{"sjc, SFO", []string{"aa00000000000001", "bb00000000000002", "cc00000000000003"}},
		{"LAX", nil},
	} {
		got := regionNodesByGetNodes(t, db, tc.region)
		assertSameNodeSet(t, tc.region, got, regionNodesByJSONExtract(t, db, tc.region))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("region %q: got %v, want %v", tc.region, got, tc.want)
		}
	}
}

// One ADVERT whose decoded_json is not valid JSON made the JSON_EXTRACT
// expression raise "malformed JSON", failing every region query that joined
// it — /api/nodes?region= then answered 500. The row is left with a NULL
// from_pubkey, the state the backfill has not reached yet.
func TestGetNodesRegionFilter_MalformedAdvertJSONDoesNotFailQuery_PR38(t *testing.T) {
	db := setupRegionFromPubkeyDB(t)
	defer db.Close()

	regionTx(t, db, "adv-a-sjc", 4, advertJSON("aa00000000000001"), sql.NullString{String: "aa00000000000001", Valid: true}, 1)
	regionTx(t, db, "adv-corrupt-sjc", 4, `NOT JSON`, sql.NullString{}, 1)

	nodes, total, _, err := db.GetNodes(100, 0, "", "", "", "", "", "SJC")
	if err != nil {
		t.Fatalf("GetNodes(region=SJC) with one corrupt ADVERT in the region: %v", err)
	}
	if total != 1 || len(nodes) != 1 || nodes[0]["public_key"] != "aa00000000000001" {
		t.Fatalf("GetNodes(region=SJC) = total %d, %d nodes %v; want only aa00000000000001", total, len(nodes), nodes)
	}
}
