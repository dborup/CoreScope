package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestNeighborBuilderIgnoresTransmittedObservations(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		directions []any
		wantCount  int
	}{
		{"tx zero hop", `[]`, []any{"tx"}, 0},
		{"tx forwarded", `["bbbb","cccc"]`, []any{"tx"}, 0},
		{"rx plus forwarded tx", `["bbbb"]`, []any{"rx", "tx"}, 1},
		{"legacy NULL", `["bbbb"]`, []any{nil}, 1},
		{"legacy empty", `["bbbb"]`, []any{""}, 1},
		{"unknown direction unchanged", `["bbbb"]`, []any{"unknown"}, 1},
		{"uppercase remains unchanged", `["bbbb"]`, []any{"TX"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := OpenStore(filepath.Join(t.TempDir(), "neighbors.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.WaitForAsyncMigrations()
			origin, relay, observer := strings.Repeat("aa", 32), strings.Repeat("bb", 32), strings.Repeat("cc", 32)
			for _, key := range []string{origin, relay, observer} {
				if _, err := s.db.Exec(`INSERT INTO nodes (public_key, role) VALUES (?, 'repeater')`, key); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(`INSERT INTO observers (id) VALUES (?)`, observer); err != nil {
				t.Fatal(err)
			}
			var observerRow int
			if err := s.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, observer).Scan(&observerRow); err != nil {
				t.Fatal(err)
			}
			res, err := s.db.Exec(`INSERT INTO transmissions
				(raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
				VALUES ('', 'direction-test', '2026-10-01T12:00:00Z', 1, ?, '{}', ?)`, payloadADVERT, origin)
			if err != nil {
				t.Fatal(err)
			}
			txID, err := res.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			for i, direction := range tc.directions {
				path := tc.path
				if tc.name == "rx plus forwarded tx" && direction == "tx" {
					path = `["bbbb","cccc"]`
				}
				if _, err := s.db.Exec(`INSERT INTO observations
					(transmission_id, observer_idx, direction, path_json, timestamp)
					VALUES (?, ?, ?, ?, ?)`, txID, observerRow, direction, path, 1790856000+i); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.buildAndPersistNeighborEdges(); err != nil {
				t.Fatal(err)
			}
			rows, err := s.db.Query(`SELECT node_a, node_b, count FROM neighbor_edges ORDER BY node_a, node_b`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got := make(map[string]int)
			for rows.Next() {
				var a, b string
				var count int
				if err := rows.Scan(&a, &b, &count); err != nil {
					t.Fatal(err)
				}
				got[fmt.Sprintf("%s--%s", a, b)] = count
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if tc.wantCount == 0 {
				if len(got) != 0 {
					t.Fatalf("TX observation generated %d neighbor edges", len(got))
				}
				return
			}
			if len(got) != 2 {
				t.Fatalf("got %d edges, want origin--relay and relay--observer", len(got))
			}
			for _, pair := range [][2]string{{origin, relay}, {relay, observer}} {
				key := pair[0] + "--" + pair[1]
				if got[key] != tc.wantCount {
					t.Errorf("%s count = %d, want %d", key, got[key], tc.wantCount)
				}
			}
		})
	}
}

// The filter governs future delta scans, not a destructive repair of old counts.
func TestNeighborBuilderKeepsPersistedHistory(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitForAsyncMigrations()
	const lastSeen = "2026-09-30T12:00:00Z"
	if _, err := s.db.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen)
		VALUES ('historical-a', 'historical-b', 7, ?)`, lastSeen); err != nil {
		t.Fatal(err)
	}
	if n, err := s.buildAndPersistNeighborEdges(); err != nil || n != 0 {
		t.Fatalf("empty delta: upserts = %d, err = %v", n, err)
	}
	var count, rows int
	var at string
	if err := s.db.QueryRow(`SELECT count, last_seen FROM neighbor_edges
		WHERE node_a = 'historical-a' AND node_b = 'historical-b'`).Scan(&count, &at); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM neighbor_edges`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 7 || at != lastSeen {
		t.Fatalf("persisted history changed: rows=%d count=%d last_seen=%q", rows, count, at)
	}
}
