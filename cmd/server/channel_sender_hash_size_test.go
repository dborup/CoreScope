package main

import (
	"fmt"
	"testing"
	"time"
)

// Both channel-message backends must describe the stored frame, including a
// flood heard before any relay has appended a hop.
func TestChannelMessagesSenderPathHashSize(t *testing.T) {
	for _, makeDB := range []struct {
		name string
		open func(*testing.T) *DB
	}{
		{"v3", func(t *testing.T) *DB { return setupTestDB(t) }},
		{"v2", setupTestDBV2},
	} {
		t.Run(makeDB.name, func(t *testing.T) {
			db := makeDB.open(t)
			defer db.Close()
			if _, err := db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obs-hash', 'Observer', 'AAR')`); err != nil {
				t.Fatal(err)
			}
			for i, tc := range []struct {
				raw  string
				want int
			}{
				{"1540DEADBEEF", 2}, // zero-hop flood
				{"1580DEADBEEF", 3}, // zero-hop flood
				{"1600DEADBEEF", 0}, // direct marker, no encoded width
			} {
				ts := time.Now().UTC().Add(time.Duration(i-5) * time.Minute)
				res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
					VALUES (?, ?, ?, ?, 5, '{"type":"CHAN","channel":"#sender-size","text":"Alice: message"}', '#sender-size')`,
					tc.raw, fmt.Sprintf("sender-size-%d", i), ts.Format(time.RFC3339), 1)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				if makeDB.name == "v3" {
					_, err = db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, 1, '[]', ?)`, id, ts.Unix())
				} else {
					_, err = db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, path_json, timestamp) VALUES (?, 'obs-hash', 'Observer', '[]', ?)`, id, ts.Unix())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			check := func(source string, messages []map[string]interface{}) {
				t.Helper()
				if len(messages) != 3 {
					t.Fatalf("%s: got %d messages, want 3", source, len(messages))
				}
				for _, message := range messages {
					hash, _ := message["packetHash"].(string)
					var n int
					if _, err := fmt.Sscanf(hash, "sender-size-%d", &n); err != nil {
						t.Fatalf("%s: unexpected hash %q", source, hash)
					}
					want := []int{2, 3, 0}[n]
					if got := message["senderPathHashSize"]; got != want {
						t.Errorf("%s %s: senderPathHashSize=%v, want %d", source, hash, got, want)
					}
				}
			}
			messages, _, err := db.GetChannelMessages("#sender-size", 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			check("db", messages)
			store := NewPacketStore(db, nil)
			if err := store.Load(); err != nil {
				t.Fatal(err)
			}
			messages, _ = store.GetChannelMessages("#sender-size", 100, 0)
			check("store", messages)
		})
	}
}
