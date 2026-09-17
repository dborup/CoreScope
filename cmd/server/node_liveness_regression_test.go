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

type activitySnapshot struct {
	relay       RepeaterRelayInfo
	bulkRelay   RepeaterRelayInfo
	health      NodeHealthStats
	bulkHealth  NodeHealthStats
	bulkPresent bool
}

func snapshotNodeActivity(t *testing.T, store *PacketStore, key string) activitySnapshot {
	t.Helper()
	snap := activitySnapshot{relay: store.GetRepeaterRelayInfo(key, 24)}
	bulkRelay, indexed := store.computeRepeaterRelayInfoMap(24)[key]
	if !indexed {
		// Keys never indexed are absent from the bulk map; enrichment skips them.
		bulkRelay = RepeaterRelayInfo{WindowHours: 24}
	}
	snap.bulkRelay = bulkRelay
	health, err := store.GetNodeHealth(key)
	if err != nil || health == nil {
		t.Fatalf("health for %s: %v %v", key, health, err)
	}
	snap.health = health["stats"].(NodeHealthStats)
	for _, row := range store.GetBulkHealth(100, "", "") {
		if row["public_key"] == key {
			snap.bulkHealth, snap.bulkPresent = row["stats"].(NodeHealthStats), true
		}
	}
	if !snap.bulkPresent {
		t.Fatalf("bulk health omitted %s", key)
	}
	if !reflect.DeepEqual(snap.relay, snap.bulkRelay) {
		t.Fatalf("single/bulk relay mismatch for %s: single %+v, bulk %+v", key, snap.relay, snap.bulkRelay)
	}
	if !reflect.DeepEqual(snap.health.LastHeard, snap.bulkHealth.LastHeard) || !reflect.DeepEqual(snap.health.LastAdvert, snap.bulkHealth.LastAdvert) {
		t.Fatalf("single/bulk health mismatch for %s: single %+v, bulk %+v", key, snap.health, snap.bulkHealth)
	}
	return snap
}

func derefTS(ts *string) string {
	if ts == nil {
		return "null"
	}
	return *ts
}

func activityTestStore(t *testing.T, repeaters ...string) (*DB, *PacketStore) {
	t.Helper()
	db := setupCapabilityTestDB(t)
	if _, err := db.conn.Exec("ALTER TABLE nodes ADD COLUMN foreign_advert INTEGER DEFAULT 0"); err != nil {
		t.Fatal(err)
	}
	for _, key := range repeaters {
		if _, err := db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen) VALUES (?, ?, 'repeater', ?)", key, "Synthetic "+key[:6], recentTS(0)); err != nil {
			t.Fatal(err)
		}
	}
	return db, NewPacketStore(db, nil)
}

// storeObservedTx builds a transmission the way ingest does: one StoreObs per
// observer path, with the longest path selected as the display observation.
func storeObservedTx(store *PacketStore, id, pt int, ts, decoded string, paths ...string) *StoreTx {
	rt := routeTypeFlood
	tx := &StoreTx{ID: id, Hash: fmt.Sprintf("synthetic-%d", id), PayloadType: &pt, RouteType: &rt, FirstSeen: ts, LatestSeen: ts, DecodedJSON: decoded}
	for i, path := range paths {
		tx.Observations = append(tx.Observations, &StoreObs{ID: id*10 + i, TransmissionID: id, ObserverID: fmt.Sprintf("observer-%d", i), PathJSON: path, Timestamp: ts})
	}
	tx.ObservationCount = len(tx.Observations)
	pickBestObservation(tx)
	store.packets = append(store.packets, tx)
	store.byHash[tx.Hash] = tx
	store.byTxID[tx.ID] = tx
	addTxToPathHopIndex(store.byPathHop, tx)
	store.indexByNode(tx)
	return tx
}

// Review fix A: the display observation is the longest path, but a shorter
// observation of the same flood can be the only raw evidence for a relay.
func TestNodeActivity_AlternateObservationPathConfirmsRelay(t *testing.T) {
	relay := "a1b2c3" + strings.Repeat("11", 29)
	hopA := "d4e5f6" + strings.Repeat("22", 29)
	hopB := "e7f8a9" + strings.Repeat("33", 29)
	guessed := "b0cafe" + strings.Repeat("44", 29)
	twin := "b0beef" + strings.Repeat("55", 29)
	db, store := activityTestStore(t, relay, hopA, hopB, guessed, twin)
	defer db.conn.Close()
	ts := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	hopsSeen := map[string]bool{}

	// Two observers heard the short route through relay (one lower-case),
	// the display observation took a longer route that does not name relay.
	confirmed := storeObservedTx(store, 1, 2, ts, `{"type":"TXT_MSG"}`, `["A1B2C3"]`, `["D4E5F6","E7F8A9"]`, `["a1b2c3"]`)
	if confirmed.PathJSON != `["D4E5F6","E7F8A9"]` {
		t.Fatalf("fixture must display the longer observation, got %s", confirmed.PathJSON)
	}
	store.indexResolvedPathHops(confirmed, mergeResolvedPubkeys([]*string{&relay}, []*string{&hopA, &hopB}), hopsSeen)
	// A heuristic resolution of a colliding alternate hop is not evidence.
	heuristic := storeObservedTx(store, 2, 2, ts, `{"type":"TXT_MSG"}`, `["B0"]`, `["D4E5F6","E7F8A9"]`)
	store.indexResolvedPathHops(heuristic, mergeResolvedPubkeys([]*string{&guessed}, []*string{&hopA, &hopB}), hopsSeen)
	// A planned DIRECT route in any observation is not observed relay activity.
	planned := storeObservedTx(store, 3, 2, ts, `{"type":"TXT_MSG"}`, `["A1B2C3"]`, `["D4E5F6","E7F8A9"]`)
	direct := 2
	planned.RouteType = &direct
	store.indexResolvedPathHops(planned, mergeResolvedPubkeys([]*string{&relay}, []*string{&hopA, &hopB}), hopsSeen)

	got := snapshotNodeActivity(t, store, relay)
	if !got.relay.RelayActive || got.relay.RelayCount1h != 1 || got.relay.RelayCount24h != 1 || got.relay.LastRelayed != ts {
		t.Errorf("alternate observation relay lost or not deduplicated per transmission: %+v", got.relay)
	}
	if got.health.LastHeard == nil || *got.health.LastHeard != ts || got.health.LastAdvert != nil {
		t.Errorf("health must reflect the alternate-observation relay without inventing an advert: lastHeard=%v lastAdvert=%v", derefTS(got.health.LastHeard), derefTS(got.health.LastAdvert))
	}
	for _, key := range []string{guessed, twin} {
		got := snapshotNodeActivity(t, store, key)
		if got.relay.RelayActive || got.relay.LastRelayed != "" || got.health.LastHeard != nil || got.health.LastAdvert != nil {
			t.Fatalf("colliding alternate hop became activity for %s: relay %+v health %+v", key[:6], got.relay, got.health)
		}
	}
	if got := snapshotNodeActivity(t, store, hopA); got.relay.RelayCount24h != 2 || got.health.LastHeard == nil {
		t.Fatalf("display-path positive control lost: relay %+v health %+v", got.relay, got.health)
	}
}

// Review fix B: relay status accepts a unique raw prefix bucket, so health
// must see the same evidence even without resolved full-key byNode membership.
func TestNodeHealth_RawPrefixOnlyRelayMatchesRelayActivity(t *testing.T) {
	relay := "c3d4e5" + strings.Repeat("66", 29)
	other := "f1a2b3" + strings.Repeat("77", 29)
	for _, width := range []int{2, 4, 6} {
		for _, ownAdvert := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dbyte/advert=%v", width/2, ownAdvert), func(t *testing.T) {
				db, store := activityTestStore(t, relay, other)
				defer db.conn.Close()
				relayTS := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
				path := `["` + strings.ToUpper(relay[:width]) + `","F1A2B3"]`
				relayed := storeObservedTx(store, 1, 2, relayTS, `{"type":"TXT_MSG"}`, path)
				var advert *StoreTx
				if ownAdvert {
					advertTS := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
					advert = storeObservedTx(store, 2, payloadTypeAdvert, advertTS, `{"type":"ADVERT","pubKey":"`+relay+`","signatureValid":true}`, `[]`)
				}
				for _, tx := range store.byNode[relay] {
					if tx == relayed {
						t.Fatal("fixture must not have resolved full-key byNode membership")
					}
				}
				got := snapshotNodeActivity(t, store, relay)
				if !got.relay.RelayActive || got.relay.LastRelayed != relayTS {
					t.Fatalf("precondition: unique raw prefix must be relay evidence: %+v", got.relay)
				}
				if got.health.LastHeard == nil || *got.health.LastHeard != relayTS {
					t.Fatalf("RelayActive=true (lastRelayed %s) but health lastHeard=%s", relayTS, derefTS(got.health.LastHeard))
				}
				if ownAdvert && (got.health.LastAdvert == nil || *got.health.LastAdvert != advert.FirstSeen) {
					t.Fatalf("own advert timestamp lost: %v", got.health.LastAdvert)
				}
				if !ownAdvert && got.health.LastAdvert != nil {
					t.Fatalf("relay became an advert: %v", *got.health.LastAdvert)
				}
			})
		}
	}
}

// Alternate observation paths use an allocation-free scanner; it must accept
// exactly what parsePathJSON accepts for the display path.
func TestVisitPathJSONHops_MatchesParsePathJSON(t *testing.T) {
	for _, path := range []string{
		``, `[]`, ` [ ] `, `null`, `["AA"]`, `["A1B2C3","d4"]`, " [ \"AA\" ,\n\t\"BB\" ] ",
		`["AA",]`, `["AA" "BB"]`, `["AA",1]`, `[1,"AA"]`, `["AA"`, `"AA"`, `["AA"]]`, `["AA"]x`,
		`[["AA"]]`, `{"a":"AA"}`, `["AA"]`, `["AA\"BB"]`, `["AA",null]`, "[\"A\nA\"]",
	} {
		var got []string
		visitPathJSONHops(path, func(token string) bool {
			got = append(got, token)
			return true
		})
		if want := parsePathJSON(path); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("path %q: scanner %q, parsePathJSON %q", path, got, want)
		}
	}
}
