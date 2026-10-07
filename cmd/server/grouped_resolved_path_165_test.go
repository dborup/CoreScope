package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// #165 — the packets page's default (grouped) view resolves hop names from
// the grouped row. Without the server's resolved_path on that row, the client
// heuristic guessed every hop and the list flagged hops the server had a
// definite answer for (23 of 60 rows on the CI fixture). The grouped row must
// carry the resolved_path of the observation it displays: the same observer
// and path_json as the row's observer_id/path_json, since the client caches
// it under hop:observer.

const (
	rp165A = "aa11111111111111111111111111111111111111111111111111111111111111"
	rp165B = "bb22222222222222222222222222222222222222222222222222222222222222"
	rp165X = "aa99999999999999999999999999999999999999999999999999999999999999"
)

// seedGroupedRP165 inserts two transmissions:
//   - "165a…": observer A hears path [aa,bb] with resolved_path [A, null]
//     (the header: longest path); observer B hears [aa] resolved to X.
//   - "165b…": one observation with no resolved_path at all.
func seedGroupedRP165(t *testing.T, db *DB) {
	t.Helper()
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	epoch := now.Add(-time.Minute).Unix()
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	mustExec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('OBSA', 'A', 'SJC', ?, ?, 1)`, ts, ts)
	mustExec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('OBSB', 'B', 'SFO', ?, ?, 1)`, ts, ts)
	mustExec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES (1, 'AABB', '165a000000000001', ?, 1, 4, '{}')`, ts)
	mustExec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES (2, 'CCDD', '165b000000000002', ?, 1, 4, '{}')`, ts)
	mustExec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path) VALUES (1, 2, 5, -90, '["aa"]', ?, ?)`, epoch, `["`+rp165X+`"]`)
	mustExec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path) VALUES (1, 1, 6, -88, '["aa","bb"]', ?, ?)`, epoch, `["`+rp165A+`",null]`)
	mustExec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (2, 1, 7, -80, '["cc"]', ?)`, epoch)
}

func groupedRowsByHash165(t *testing.T, r *PacketResult) map[string]map[string]interface{} {
	t.Helper()
	out := map[string]map[string]interface{}{}
	for _, p := range r.Packets {
		h, _ := p["hash"].(string)
		out[h] = p
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 grouped rows, got %d: %#v", len(out), r.Packets)
	}
	return out
}

func assertGroupedRP165(t *testing.T, rows map[string]map[string]interface{}) {
	t.Helper()
	a := rows["165a000000000001"]
	if got := strings.ToUpper(a["observer_id"].(string)); got != "OBSA" {
		t.Fatalf("header observer = %q, want OBSA (longest path)", got)
	}
	rp, ok := a["resolved_path"]
	if !ok {
		t.Fatalf("grouped row carries no resolved_path; the client then guesses hops the server resolved (#165): %#v", a)
	}
	b, _ := json.Marshal(rp)
	if want := `["` + rp165A + `",null]`; string(b) != want {
		t.Fatalf("grouped resolved_path = %s, want the header observation's %s", b, want)
	}
	if _, ok := rows["165b000000000002"]["resolved_path"]; ok {
		t.Fatalf("a row whose observation has no resolved_path must omit the key, got %#v", rows["165b000000000002"]["resolved_path"])
	}
}

func TestGroupedPacketsCarryHeaderResolvedPath165(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedGroupedRP165(t, db)
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	assertGroupedRP165(t, groupedRowsByHash165(t, store.QueryGroupedPackets(PacketQuery{Limit: 50})))
	// Second call is served from the grouped sort cache; same contract.
	assertGroupedRP165(t, groupedRowsByHash165(t, store.QueryGroupedPackets(PacketQuery{Limit: 50})))
}

func TestGroupedPacketsSQLCarryHeaderResolvedPath165(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedGroupedRP165(t, db)
	r, err := db.QueryGroupedPackets(PacketQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	assertGroupedRP165(t, groupedRowsByHash165(t, r))
}
