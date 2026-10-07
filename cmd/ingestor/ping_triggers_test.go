package main

// Tests for the ping-score highscore/leaderboard feature's detection side:
// isPingTrigger/pingTriggerSenderAndText mirror cmd/server/db.go's copies
// exactly, and InsertTransmission writes exactly one ping_triggers row per
// new ping-triggering CHAN transmission.

import (
	"testing"
	"time"
)

func TestIsPingTrigger(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"ping", true},
		{"Ping", true},
		{"PING", true},
		{"/ping", true},
		{"@CoreScopeBot ping", true},
		{"@CoreScopeBot /ping", true},
		{"  ping  ", true},
		{"ping there", false},
		{"pingpong", false},
		{"pong", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isPingTrigger(c.text); got != c.want {
			t.Errorf("isPingTrigger(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestPingTriggerSenderAndText(t *testing.T) {
	sender, text, ok := pingTriggerSenderAndText(`{"type":"CHAN","channel":"#test","text":"Alice: ping"}`)
	if !ok {
		t.Fatal("expected ok=true for valid JSON")
	}
	if sender != "Alice" {
		t.Errorf("sender = %q, want Alice", sender)
	}
	if text != "ping" {
		t.Errorf("text = %q, want ping", text)
	}

	if _, _, ok := pingTriggerSenderAndText("not json"); ok {
		t.Error("expected ok=false for invalid JSON")
	}
}

func insertChanTx(t *testing.T, s *Store, hash, text, channelHash string) {
	t.Helper()
	data := &PacketData{
		RawHex:      "AABB",
		Timestamp:   "2026-03-25T00:00:00Z",
		ObserverID:  "obs1",
		Hash:        hash,
		RouteType:   1,
		PayloadType: 5,
		DecodedJSON: `{"type":"CHAN","channel":"` + channelHash + `","text":"` + text + `"}`,
		ChannelHash: channelHash,
		PathJSON:    "[]",
	}
	if _, err := s.InsertTransmission(data); err != nil {
		t.Fatalf("InsertTransmission: %v", err)
	}
}

func countPingTriggers(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ping_triggers`).Scan(&n); err != nil {
		t.Fatalf("count ping_triggers: %v", err)
	}
	return n
}

// Packet retention removes raw data, but the ping index must remain so
// previously computed all-time scores can still be joined to their trigger.
func TestPruneOldPacketsKeepsPingTriggers(t *testing.T) {
	s := openPruneStore(t, "ping-retention.db")
	old := time.Now().UTC().AddDate(0, 0, -40).Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)

	seed := func(hash, channel, sender, seen string) int64 {
		t.Helper()
		result, err := s.db.Exec(`INSERT INTO transmissions
			(raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
			VALUES ('AA', ?, ?, 1, 5, 1, '{}')`, hash, seen)
		if err != nil {
			t.Fatalf("seed transmission %s: %v", hash, err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("transmission ID %s: %v", hash, err)
		}
		if _, err := s.db.Exec(`INSERT INTO observations
			(transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp)
			VALUES (?, 1, 'rx', 1.0, -100, 0, '[]', ?)`, id, time.Now().Unix()); err != nil {
			t.Fatalf("seed observation %s: %v", hash, err)
		}
		if _, err := s.db.Exec(`INSERT INTO ping_triggers
			(tx_id, hash, channel_hash, sender, first_seen) VALUES (?, ?, ?, ?, ?)`,
			id, hash, channel, sender, seen); err != nil {
			t.Fatalf("seed ping trigger %s: %v", hash, err)
		}
		return id
	}

	oldID := seed("old-ping", "#old", "Old sender", old)
	freshID := seed("fresh-ping", "#fresh", "Fresh sender", fresh)
	deleted, err := s.PruneOldPackets(30)
	if err != nil {
		t.Fatalf("PruneOldPackets: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted transmissions = %d, want 1", deleted)
	}

	for _, tc := range []struct {
		id      int64
		hash    string
		channel string
		sender  string
		seen    string
		present int
	}{
		{oldID, "old-ping", "#old", "Old sender", old, 0},
		{freshID, "fresh-ping", "#fresh", "Fresh sender", fresh, 1},
	} {
		var txCount, obsCount int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE id = ?`, tc.id).Scan(&txCount); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE transmission_id = ?`, tc.id).Scan(&obsCount); err != nil {
			t.Fatal(err)
		}
		if txCount != tc.present || obsCount != tc.present {
			t.Errorf("tx %d: transmissions=%d observations=%d, want %d each", tc.id, txCount, obsCount, tc.present)
		}
		var hash, channel, sender, seen string
		if err := s.db.QueryRow(`SELECT hash, channel_hash, sender, first_seen FROM ping_triggers WHERE tx_id = ?`, tc.id).
			Scan(&hash, &channel, &sender, &seen); err != nil {
			t.Fatalf("trigger for tx %d: %v", tc.id, err)
		}
		if hash != tc.hash || channel != tc.channel || sender != tc.sender || seen != tc.seen {
			t.Errorf("trigger for tx %d changed: (%q, %q, %q, %q)", tc.id, hash, channel, sender, seen)
		}
	}
}

// With channelDays longer than packetDays, a ping remains a channel message
// until channelDays; its trigger remains even after that raw message is pruned.
func TestPruneTransmissionsChannelRetentionKeepsPingTriggers(t *testing.T) {
	s := openPruneStore(t, "ping-channel-retention.db")
	seenByID := make(map[int64]string)
	seedPing := func(hash, channel, sender string, ageDays int) int64 {
		t.Helper()
		id := seedRetentionTx(t, s, hash, intPtr(payloadTypeGrpTxt), ageDays, 1)
		var seen string
		if err := s.db.QueryRow(`SELECT first_seen FROM transmissions WHERE id = ?`, id).Scan(&seen); err != nil {
			t.Fatalf("first_seen for %s: %v", hash, err)
		}
		seenByID[id] = seen
		if _, err := s.db.Exec(`INSERT INTO ping_triggers
			(tx_id, hash, channel_hash, sender, first_seen) VALUES (?, ?, ?, ?, ?)`,
			id, hash, channel, sender, seen); err != nil {
			t.Fatalf("seed trigger %s: %v", hash, err)
		}
		return id
	}

	oldID := seedPing("old-channel-ping", "#old", "Old sender", 100)
	keptID := seedPing("kept-channel-ping", "#kept", "Kept sender", 30)
	freshID := seedPing("fresh-channel-ping", "#fresh", "Fresh sender", 5)
	seedRetentionTx(t, s, "old-advert", intPtr(4), 30, 1)

	result, err := s.PruneTransmissions(14, 90)
	if err != nil {
		t.Fatalf("PruneTransmissions: %v", err)
	}
	if result.Packets != 1 || result.ChannelMessages != 1 {
		t.Fatalf("pruned = %+v, want one ordinary packet and one old channel message", result)
	}

	for _, tc := range []struct {
		id      int64
		hash    string
		channel string
		sender  string
		present int
	}{
		{oldID, "old-channel-ping", "#old", "Old sender", 0},
		{keptID, "kept-channel-ping", "#kept", "Kept sender", 1},
		{freshID, "fresh-channel-ping", "#fresh", "Fresh sender", 1},
	} {
		var txCount, obsCount int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE id = ?`, tc.id).Scan(&txCount); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE transmission_id = ?`, tc.id).Scan(&obsCount); err != nil {
			t.Fatal(err)
		}
		if txCount != tc.present || obsCount != tc.present {
			t.Errorf("tx %d: transmissions=%d observations=%d, want %d each", tc.id, txCount, obsCount, tc.present)
		}
		var hash, channel, sender, triggerSeen string
		if err := s.db.QueryRow(`SELECT hash, channel_hash, sender, first_seen FROM ping_triggers WHERE tx_id = ?`, tc.id).
			Scan(&hash, &channel, &sender, &triggerSeen); err != nil {
			t.Fatalf("trigger for tx %d: %v", tc.id, err)
		}
		wantSeen := seenByID[tc.id]
		if hash != tc.hash || channel != tc.channel || sender != tc.sender || triggerSeen != wantSeen {
			t.Errorf("trigger for tx %d changed: (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				tc.id, hash, channel, sender, triggerSeen, tc.hash, tc.channel, tc.sender, wantSeen)
		}
	}
}

func TestInsertTransmission_PingTriggerRecorded(t *testing.T) {
	s := openNeighborsStore(t) // reuses the OpenStore+t.Cleanup helper from issue1865_test.go

	insertChanTx(t, s, "pinghash00000001", "Alice: ping", "#test")

	if got := countPingTriggers(t, s); got != 1 {
		t.Fatalf("ping_triggers count = %d, want 1", got)
	}
	var hash, channelHash, sender, firstSeen string
	if err := s.db.QueryRow(
		`SELECT hash, channel_hash, sender, first_seen FROM ping_triggers WHERE tx_id = (SELECT id FROM transmissions WHERE hash = ?)`,
		"pinghash00000001",
	).Scan(&hash, &channelHash, &sender, &firstSeen); err != nil {
		t.Fatalf("read ping_triggers row: %v", err)
	}
	if hash != "pinghash00000001" || channelHash != "#test" || sender != "Alice" {
		t.Errorf("ping_triggers row = (hash=%q channel=%q sender=%q), want (pinghash00000001, #test, Alice)", hash, channelHash, sender)
	}
}

func TestInsertTransmission_NonPingNotRecorded(t *testing.T) {
	s := openNeighborsStore(t)

	insertChanTx(t, s, "chatmsg00000001", "Alice: hello everyone", "#test")

	if got := countPingTriggers(t, s); got != 0 {
		t.Errorf("ping_triggers count = %d, want 0 for a non-ping message", got)
	}
}

func TestInsertTransmission_RepeatObservationDoesNotDuplicate(t *testing.T) {
	s := openNeighborsStore(t)

	insertChanTx(t, s, "pinghash00000002", "Bob: /ping", "#test")
	// A second observation of the SAME hash (e.g. heard by another
	// observer) must not add a second ping_triggers row -- the
	// transmissions table's own find-or-create semantics mean
	// InsertTransmission's isNew branch (where the ping check lives)
	// only ever runs once per hash.
	insertChanTx(t, s, "pinghash00000002", "Bob: /ping", "#test")

	if got := countPingTriggers(t, s); got != 1 {
		t.Errorf("ping_triggers count = %d, want 1 (no duplicate on repeat observation)", got)
	}
}

func TestInsertTransmission_NonChanPayloadNotChecked(t *testing.T) {
	s := openNeighborsStore(t)

	// PayloadType 2 (not 5/CHAN) with "ping"-looking text must never be
	// mistaken for a channel ping trigger.
	data := &PacketData{
		RawHex:      "AABB",
		Timestamp:   "2026-03-25T00:00:00Z",
		ObserverID:  "obs1",
		Hash:        "advhash00000001",
		RouteType:   1,
		PayloadType: 2,
		DecodedJSON: `{"type":"ADVERT","text":"ping"}`,
		PathJSON:    "[]",
	}
	if _, err := s.InsertTransmission(data); err != nil {
		t.Fatalf("InsertTransmission: %v", err)
	}

	if got := countPingTriggers(t, s); got != 0 {
		t.Errorf("ping_triggers count = %d, want 0 for a non-CHAN payload type", got)
	}
}
