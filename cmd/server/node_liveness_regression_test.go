package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNodeHealth_LastAdvertIsOwnAdvertNotTraffic(t *testing.T) {
	db := setupCapabilityTestDB(t)
	defer db.conn.Close()
	if _, err := db.conn.Exec("ALTER TABLE nodes ADD COLUMN foreign_advert INTEGER DEFAULT 0"); err != nil {
		t.Fatal(err)
	}
	key := "aa" + strings.Repeat("11", 31)
	other := "bb" + strings.Repeat("22", 31)
	if _, err := db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen) VALUES (?, 'Synthetic', 'repeater', ?)", key, recentTS(0)); err != nil {
		t.Fatal(err)
	}
	for _, ownAdvert := range []bool{true, false} {
		t.Run(map[bool]string{true: "own advert", false: "no advert"}[ownAdvert], func(t *testing.T) {
			store := NewPacketStore(db, nil)
			mk := func(id, pt int, source string, hours float64) *StoreTx {
				return &StoreTx{ID: id, PayloadType: &pt, FirstSeen: time.Now().UTC().Add(-time.Duration(hours * float64(time.Hour))).Format(time.RFC3339), DecodedJSON: `{"pubKey":"` + source + `"}`, PathJSON: `[]`}
			}
			packets := []*StoreTx{mk(2, 4, other, 2), mk(3, 2, key, 1), mk(4, 3, key, 0.1)}
			advert := mk(1, 4, strings.ToUpper(key), 72)
			if ownAdvert {
				packets = append([]*StoreTx{advert}, packets...)
			}
			store.byNode[key] = packets
			health, err := store.GetNodeHealth(key)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(health)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Stats struct {
					LastAdvert   *string `json:"lastAdvert"`
					LastHeard    *string `json:"lastHeard"`
					TotalPackets int     `json:"totalPackets"`
				} `json:"stats"`
			}
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"lastAdvert":`) {
				t.Fatal("lastAdvert contract missing (must be explicitly null when unknown)")
			}
			if ownAdvert && (body.Stats.LastAdvert == nil || *body.Stats.LastAdvert != advert.FirstSeen) {
				t.Errorf("lastAdvert = %v, want own advert %s", body.Stats.LastAdvert, advert.FirstSeen)
			}
			if !ownAdvert && body.Stats.LastAdvert != nil {
				t.Errorf("traffic or relay-touched last_seen became an advert: %v", body.Stats.LastAdvert)
			}
			if !reflect.DeepEqual(body.Stats.LastHeard, body.Stats.LastAdvert) || body.Stats.TotalPackets != len(packets) {
				t.Fatalf("lastHeard must use certain activity while analytics counts stay intact: %+v", body.Stats)
			}
			bulk := store.GetBulkHealth(10, "", "")
			if len(bulk) != 1 || !reflect.DeepEqual(health["stats"].(NodeHealthStats).LastHeard, bulk[0]["stats"].(NodeHealthStats).LastHeard) || !reflect.DeepEqual(health["stats"].(NodeHealthStats).LastAdvert, bulk[0]["stats"].(NodeHealthStats).LastAdvert) {
				t.Fatalf("single/bulk health timestamp mismatch: %v", bulk)
			}
		})
	}
}

func TestRepeaterRelayActivity_CollisionAndHeuristicFullKeyFailClosed(t *testing.T) {
	db := setupCapabilityTestDB(t)
	defer db.conn.Close()
	a := "aa11" + strings.Repeat("11", 30)
	b := "aa22" + strings.Repeat("22", 30)
	for _, key := range []string{a, b} {
		if _, err := db.conn.Exec("INSERT INTO nodes (public_key, role) VALUES (?, 'repeater')", key); err != nil {
			t.Fatal(err)
		}
	}
	store := NewPacketStore(db, nil)
	pt, rt := 3, routeTypeFlood
	ambiguous := &StoreTx{ID: 1, PayloadType: &pt, RouteType: &rt, FirstSeen: recentTS(0), PathJSON: `["AA"]`, ScopeName: "ambiguous"}
	confirmed := &StoreTx{ID: 2, PayloadType: &pt, RouteType: &rt, FirstSeen: recentTS(0), PathJSON: `["AA22"]`, ScopeName: "confirmed"}
	store.byPathHop["aa"] = []*StoreTx{ambiguous}
	// A previous heuristic guess must not turn a colliding raw hop into certainty.
	store.byPathHop[a] = []*StoreTx{ambiguous}
	store.byPathHop[b] = []*StoreTx{confirmed}
	store.byPathHop["aa22"] = []*StoreTx{confirmed}
	bulk := store.computeRepeaterRelayInfoMap(24)
	for _, key := range []string{a, b} {
		one := store.GetRepeaterRelayInfo(key, 24)
		if !reflect.DeepEqual(one, bulk[key]) {
			t.Fatalf("bulk/single mismatch for %s: single %+v, bulk %+v", key, one, bulk[key])
		}
		if key == a && (one.RelayActive || one.LastRelayed != "" || one.RelayCount24h != 0 || len(one.TransportedScopes) != 0) {
			t.Fatalf("offline colliding node falsely active: %+v", one)
		}
		if key == b && (!one.RelayActive || one.RelayCount1h != 1 || one.RelayCount24h != 1 || one.LastRelayed != confirmed.FirstSeen || !reflect.DeepEqual(one.TransportedScopes, []string{"confirmed"})) {
			t.Fatalf("unique 2-byte relay lost or ambiguous traffic added: %+v", one)
		}
	}
}

func TestRepeaterRelayActivity_MissingEvidenceFailsClosed(t *testing.T) {
	key := "cc" + strings.Repeat("33", 31)
	pt, rt := 2, routeTypeFlood
	for _, path := range []string{"", `[]`, `["cc"]`} {
		store := &PacketStore{byPathHop: map[string][]*StoreTx{key: {{ID: 1, PayloadType: &pt, RouteType: &rt, FirstSeen: recentTS(0), PathJSON: path}}}}
		if got := store.GetRepeaterRelayInfo(key, 24); got.LastRelayed != "" || got.RelayActive {
			t.Fatalf("missing prefix map/path must fail closed, path=%q: %+v", path, got)
		}
	}
	// An exact full-key path remains certain without a prefix map.
	tx := &StoreTx{ID: 2, PayloadType: &pt, RouteType: &rt, FirstSeen: time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339), PathJSON: `["` + key + `"]`}
	store := &PacketStore{byPathHop: map[string][]*StoreTx{key: {tx}}}
	if got := store.GetRepeaterRelayInfo(key, 24); !got.RelayActive || got.RelayCount1h != 1 {
		t.Fatalf("exact key positive control lost: %+v", got)
	}
}

func TestNodeActivity_ObservedFloodNotPlannedDirectRoute(t *testing.T) {
	key := "cc" + strings.Repeat("33", 31)
	pt := 2
	for _, route := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("route%d", route), func(t *testing.T) {
			tx := &StoreTx{ID: 1, PayloadType: &pt, RouteType: &route, FirstSeen: recentTS(0), PathJSON: `["` + key + `"]`}
			store := &PacketStore{byPathHop: map[string][]*StoreTx{key: {tx}}}
			info := store.GetRepeaterRelayInfo(key, 24)
			var heard, advert string
			updateNodeActivity(tx, key, nil, &heard, &advert)
			flood := route == 0 || route == 1
			if info.RelayActive != flood || (heard != "") != flood || advert != "" {
				t.Fatalf("planned direct route became observed activity: route=%d relay=%+v heard=%q advert=%q", route, info, heard, advert)
			}
		})
	}
}

func TestNodeActivity_RejectsInvalidAdvertAndMalformedTimestamps(t *testing.T) {
	key := "dd" + strings.Repeat("44", 31)
	pt, direct := payloadTypeAdvert, 2
	makeAdvert := func(ts string, signature string) *StoreTx {
		return &StoreTx{PayloadType: &pt, RouteType: &direct, FirstSeen: ts, DecodedJSON: `{"pubKey":"` + key + `"` + signature + `}`}
	}
	var heard, advert string
	for _, tx := range []*StoreTx{makeAdvert("not-a-timestamp", ""), makeAdvert(recentTS(0), `,"signatureValid":false`)} {
		updateNodeActivity(tx, key, nil, &heard, &advert)
	}
	flood := routeTypeFlood
	forgedFlood := makeAdvert(recentTS(0), `,"signatureValid":false`)
	forgedFlood.RouteType, forgedFlood.PathJSON = &flood, `["`+key+`"]`
	updateNodeActivity(forgedFlood, key, nil, &heard, &advert)
	if heard != "" || advert != "" {
		t.Fatalf("invalid advert/timestamp became activity: heard=%q advert=%q", heard, advert)
	}
	// Lexicographically Z sorts after '.', but the fractional timestamp is newer.
	older := "2026-01-01T00:00:00Z"
	newer := "2026-01-01T00:00:00.500Z"
	updateNodeActivity(makeAdvert(older, ""), key, nil, &heard, &advert)
	updateNodeActivity(makeAdvert(newer, ""), key, nil, &heard, &advert)
	if heard != newer || advert != newer {
		t.Fatalf("mixed RFC3339 precision sorted incorrectly: heard=%q advert=%q", heard, advert)
	}
	for _, token := range []string{"dd444444", "dd4444444444", "gg", strings.Repeat("g", 64)} {
		if got := confirmedRelayKey(token, nil); got != "" {
			t.Fatalf("unsupported/malformed hop became identity: %q -> %q", token, got)
		}
	}
}

// Realistic indexed scale, with colliding short hashes and unique 3-byte hops.
// Bounds are inherited from packet-store eviction; no per-node SQL is needed.
func BenchmarkConfirmedRelayBulk30KPackets2KNodes(b *testing.B) {
	const nodeCount, packetCount = 2000, 30000
	nodes := make([]nodeInfo, nodeCount)
	for i := range nodes {
		nodes[i] = nodeInfo{PublicKey: fmt.Sprintf("%06x", i+1) + strings.Repeat("00", 29), Role: "repeater"}
	}
	store := &PacketStore{nodePM: buildPrefixMap(nodes), byPathHop: make(map[string][]*StoreTx)}
	pt, rt := 3, routeTypeFlood
	ts := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	for i := 0; i < packetCount; i++ {
		keys := []string{nodes[i%nodeCount].PublicKey, nodes[(i+1)%nodeCount].PublicKey, nodes[(i+2)%nodeCount].PublicKey}
		tx := &StoreTx{ID: i + 1, PayloadType: &pt, RouteType: &rt, FirstSeen: ts, PathJSON: fmt.Sprintf(`["%s","%s","%s"]`, keys[0][:6], keys[1][:6], keys[2][:6])}
		for _, key := range keys {
			store.byPathHop[key] = append(store.byPathHop[key], tx)
			store.byPathHop[key[:6]] = append(store.byPathHop[key[:6]], tx)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := store.computeRepeaterRelayInfoMap(24)
		if got[nodes[0].PublicKey].RelayCount24h != 45 {
			b.Fatal("unexpected dedup/attribution result")
		}
	}
}
