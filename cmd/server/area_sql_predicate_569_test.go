package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestAreaFilterSQLPredicateBounded569: the area predicate binds ONE
// parameter whatever the member count, and drives off
// idx_transmissions_from_pubkey (no per-row JSON parse, no table scan).
func TestAreaFilterSQLPredicateBounded569(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	members := make(map[string]bool, 5000)
	for i := 0; i < 5000; i++ {
		members[fmt.Sprintf("%064x", i)] = true
	}
	where, args := db.buildTransmissionWhere(PacketQuery{Area: "BEL", AreaNodes: members})
	if len(where) != 1 || len(args) != 1 {
		t.Fatalf("want one predicate with one bound arg, got %d/%d: %v", len(where), len(args), where)
	}
	var keys []string
	if err := json.Unmarshal([]byte(args[0].(string)), &keys); err != nil || len(keys) != 5000 {
		t.Fatalf("arg must be a JSON array of all members: %d %v", len(keys), err)
	}
	if strings.Contains(where[0], "decoded_json") {
		t.Fatalf("predicate must not parse decoded_json per row: %s", where[0])
	}
	rows, err := db.conn.Query("EXPLAIN QUERY PLAN SELECT COUNT(*) FROM transmissions t WHERE "+where[0], args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_transmissions_from_pubkey") || strings.Contains(joined, "SCAN t") {
		t.Fatalf("area predicate must search idx_transmissions_from_pubkey:\n%s", joined)
	}

	// The empty-pubkey backfill sentinel ("") is never a member, matching
	// the in-memory `pk == ""` exclusion.
	_, args = db.buildTransmissionWhere(PacketQuery{Area: "BEL", AreaNodes: map[string]bool{"": true, "x": true}})
	if len(args) != 1 || args[0] != `["x"]` {
		t.Fatalf("empty key must be dropped, got %v", args)
	}
	// Unresolved / empty membership applies no filter (in-memory nil set).
	if where, _ := db.buildTransmissionWhere(PacketQuery{Area: "SEA"}); len(where) != 0 {
		t.Fatalf("nil membership must not filter: %v", where)
	}
}

// BenchmarkAreaSQL569 times the full SQL packet query (COUNT + page) over
// 30K ADVERTs from 2K nodes: no filter, a 1K-member area, and a since window
// (the shape of every history fallback) without and with that area.
func BenchmarkAreaSQL569(b *testing.B) {
	db := setupTestDB(b)
	defer db.Close()
	tx, _ := db.conn.Begin()
	for i := 0; i < 30000; i++ {
		ts := int64(1767225600 + i)
		res, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, payload_type, decoded_json) VALUES ('AA', ?, ?, 4, ?)`,
			fmt.Sprintf("%016x", i+1), fmt.Sprintf("2026-01-01T%08d", i), fmt.Sprintf(`{"pubKey":"%064x"}`, i%2000))
		if err != nil {
			b.Fatal(err)
		}
		id, _ := res.LastInsertId()
		tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp) VALUES (?, 1, ?)`, id, ts)
	}
	tx.Commit()
	members := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		members[fmt.Sprintf("%064x", i*2)] = true
	}
	for _, c := range []struct {
		name string
		q    PacketQuery
	}{
		{"none", PacketQuery{Limit: 50}},
		{"area1k", PacketQuery{Limit: 50, Area: "X", AreaNodes: members}},
		{"since", PacketQuery{Limit: 50, Since: "2026-01-01T08:00:00Z"}},
		{"area1k+since", PacketQuery{Limit: 50, Area: "X", AreaNodes: members, Since: "2026-01-01T08:00:00Z"}},
	} {
		for _, grouped := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/grouped=%t", c.name, grouped), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					var err error
					if grouped {
						_, err = db.QueryGroupedPackets(c.q)
					} else {
						_, err = db.QueryPackets(c.q)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
