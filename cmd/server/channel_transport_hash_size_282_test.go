package main

import (
	"fmt"
	"testing"
	"time"
)

// #282 (6): GetChannelMessages selects `substr(t.raw_hex, 1, 12)` -- the first
// six bytes -- and feeds it to packetpath.SenderHashSize(). Six bytes is exactly
// the minimum a TRANSPORT route needs: the header byte, four transport-code
// bytes (next/last hop), and then the path-length byte at offset 5. The existing
// sender-hash-size test only sends FLOOD frames, whose path-length byte is at
// offset 1, so it never exercised the transport layout and nothing proved the
// 12-hex-char slice reaches byte 5. Shortening the slice to `substr(…, 1, 11)`
// truncates that byte and this test goes red, while the FLOOD test stays green.
func TestChannelMessagesSenderPathHashSize_TransportRoute_282(t *testing.T) {
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
			if _, err := db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obs-tr', 'Observer', 'AAR')`); err != nil {
				t.Fatal(err)
			}
			// header byte 0x14 = route 0 (TRANSPORT_FLOOD), path-type 5 (hops);
			// 0x17 = route 3 (TRANSPORT_DIRECT). Four transport-code bytes follow
			// (AABBCCDD), then the path-length byte at offset 5 (hex chars 11-12),
			// which only survives because the slice keeps all 12 characters.
			for i, tc := range []struct {
				raw       string
				routeType int
				want      int
			}{
				{"14AABBCCDD40", 0, 2}, // transport flood, width bits 1 -> 2 bytes
				{"14AABBCCDD80", 0, 3}, // transport flood, width bits 2 -> 3 bytes
				{"17AABBCCDD00", 3, 0}, // transport direct, 0x00 marker -> no width
			} {
				ts := time.Now().UTC().Add(time.Duration(i-5) * time.Minute)
				res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
					VALUES (?, ?, ?, ?, 5, '{"type":"CHAN","channel":"#transport-size","text":"Alice: message"}', '#transport-size')`,
					tc.raw, fmt.Sprintf("transport-size-%d", i), ts.Format(time.RFC3339), tc.routeType)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				if makeDB.name == "v3" {
					_, err = db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, 1, '[]', ?)`, id, ts.Unix())
				} else {
					_, err = db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, path_json, timestamp) VALUES (?, 'obs-tr', 'Observer', '[]', ?)`, id, ts.Unix())
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
					if _, err := fmt.Sscanf(hash, "transport-size-%d", &n); err != nil {
						t.Fatalf("%s: unexpected hash %q", source, hash)
					}
					want := []int{2, 3, 0}[n]
					if got := message["senderPathHashSize"]; got != want {
						t.Errorf("%s %s: senderPathHashSize=%v, want %d (the path-length byte at offset 5 must survive the raw_hex slice)", source, hash, got, want)
					}
				}
			}
			messages, _, err := db.GetChannelMessages("#transport-size", 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			check("db", messages)
			store := NewPacketStore(db, nil)
			if err := store.Load(); err != nil {
				t.Fatal(err)
			}
			messages, _ = store.GetChannelMessages("#transport-size", 100, 0)
			check("store", messages)
		})
	}
}
