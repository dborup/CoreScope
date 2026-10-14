package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gorilla/mux"
)

// #76 fixture identities. hbNode and hbTwin share the 1-byte prefix a1, so
// only hbNode's 2-byte prefix a1b2 is evidence.
var (
	hbNode     = "a1b2" + strings.Repeat("11", 30)
	hbTwin     = "a1ff" + strings.Repeat("22", 30)
	hbOther    = "c3d4" + strings.Repeat("33", 30)
	hbListener = "e5f6" + strings.Repeat("44", 30)
	hbComp     = "0707" + strings.Repeat("55", 30)
)

func hbRoute(v int) *int { return &v }

func hbPrefixMap() *prefixMap {
	pm := buildPrefixMap([]nodeInfo{
		{PublicKey: hbNode, Role: "repeater"}, {PublicKey: hbTwin, Role: "repeater"},
		{PublicKey: hbOther, Role: "repeater"}, {PublicKey: hbListener, Role: "repeater"},
		{PublicKey: hbComp, Role: "companion"},
	})
	pm.markNonRelay([]string{hbListener})
	return pm
}

func TestHeardDirect_Rule_76(t *testing.T) {
	pm := hbPrefixMap()
	// wire is StoreObs.wireRoute: frame route + 1, 0 = no frame stored.
	cases := []struct {
		name    string
		key     string
		own     bool
		txRoute *int
		wire    uint8
		path    string
		want    bool
	}{
		{"flood, empty path: the advertiser", hbNode, true, hbRoute(RouteFlood), 0, `[]`, true},
		{"transport flood, empty path: the advertiser", hbNode, true, hbRoute(RouteTransportFlood), 0, `[ ]`, true},
		{"flood, empty path of another advertiser", hbNode, false, hbRoute(RouteFlood), 0, `[]`, false},
		{"flood, last hop is the node (unique 2-byte prefix)", hbNode, false, hbRoute(RouteFlood), 0, `["C3D4","A1B2"]`, true},
		{"flood, last hop lower case", hbNode, false, hbRoute(RouteFlood), 0, `["a1b2"]`, true},
		{"flood, last hop colliding 1-byte prefix", hbNode, false, hbRoute(RouteFlood), 0, `["A1"]`, false},
		{"flood, node earlier in the path", hbNode, false, hbRoute(RouteFlood), 0, `["A1B2","C3D4"]`, false},
		{"flood, own advert relayed by another node", hbNode, true, hbRoute(RouteFlood), 0, `["C3D4"]`, false},
		{"flood, listener as last hop", hbListener, false, hbRoute(RouteFlood), 0, `["E5F6"]`, false},
		{"flood, companion as last hop", hbComp, false, hbRoute(RouteFlood), 0, `["0707"]`, false},
		{"zero-hop direct: the advertiser", hbNode, true, hbRoute(RouteDirect), 0, `[]`, true},
		{"zero-hop transport direct: the advertiser", hbNode, true, hbRoute(RouteTransportDirect), 0, `[]`, true},
		{"zero-hop direct of another advertiser", hbNode, false, hbRoute(RouteDirect), 0, `[]`, false},
		{"direct with a path: remaining route, own advert", hbNode, true, hbRoute(RouteDirect), 0, `["A1B2"]`, false},
		{"direct path ending in the node", hbNode, false, hbRoute(RouteTransportDirect), 0, `["C3D4","A1B2"]`, false},
		{"unknown route", hbNode, true, nil, 0, `[]`, false},
		{"out-of-range route", hbNode, true, hbRoute(7), 0, `[]`, false},
		{"missing path is not zero hop", hbNode, true, hbRoute(RouteFlood), 0, ``, false},
		{"malformed path is not zero hop", hbNode, true, hbRoute(RouteFlood), 0, `["A1B2"`, false},
		{"escaped path still parses", hbNode, false, hbRoute(RouteFlood), 0, `["A1B2"]`, true},
		{"frame wins: direct frame under a flood tx", hbNode, false, hbRoute(RouteFlood), RouteDirect + 1, `["A1B2"]`, false},
		{"frame wins: flood frame under a direct tx", hbNode, false, hbRoute(RouteDirect), RouteFlood + 1, `["A1B2"]`, true},
		{"frame wins: zero-hop frame under a flood tx", hbNode, true, hbRoute(RouteFlood), RouteDirect + 1, `[]`, true},
		{"frame wins: transport flood frame under a direct tx", hbNode, true, hbRoute(RouteDirect), RouteTransportFlood + 1, `[]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &StoreTx{RouteType: tc.txRoute}
			o := &StoreObs{PathJSON: tc.path, wireRoute: tc.wire}
			if got := heardDirect(o, tx, tc.own, newRelayKeyMatcher(tc.key, pm)); got != tc.want {
				t.Fatalf("heardDirect = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestObservationRouteType_PerObservation_76(t *testing.T) {
	cases := []struct {
		name   string
		wire   uint8
		route  *int
		mask   uint8
		known  bool
		want   int
		wantOK bool
	}{
		{"frame header wins over the tx route", RouteDirect + 1, hbRoute(RouteFlood), 0b0110, true, RouteDirect, true},
		{"frame header route 0", RouteTransportFlood + 1, hbRoute(RouteDirect), 0, false, RouteTransportFlood, true},
		{"no frame, mixed mask: unknown", 0, hbRoute(RouteFlood), 0b0110, true, 0, false},
		{"no frame, all four routes: unknown", 0, hbRoute(RouteDirect), 0b1111, true, 0, false},
		{"no frame, flood family only", 0, hbRoute(RouteFlood), 0b0011, true, RouteFlood, true},
		{"no frame, direct family only", 0, hbRoute(RouteDirect), 0b1100, true, RouteDirect, true},
		{"no frame, mask not backfilled", 0, hbRoute(RouteDirect), 0b0110, false, RouteDirect, true},
		{"no frame, NULL route", 0, nil, 0, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &StoreTx{RouteType: tc.route, routeMask: tc.mask, routeMaskKnown: tc.known}
			got, ok := observationRouteType(&StoreObs{wireRoute: tc.wire}, tx)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("observationRouteType = %d,%v want %d,%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	for raw, want := range map[string]uint8{"12": RouteDirect + 1, "1100": RouteFlood + 1, "13ab": RouteTransportDirect + 1, "10": RouteTransportFlood + 1, "": 0, "1": 0, "zz": 0} {
		if got := wireRouteFromRawHex(raw); got != want {
			t.Errorf("wireRouteFromRawHex(%q) = %d, want %d", raw, got, want)
		}
	}
}

// The route field must live in padding: one byte per observation, never a
// new word (StoreObs is the most numerous struct in the store).
func TestStoreObsLayoutFitsWireRouteInPadding_76(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout budget is defined for 64-bit platforms")
	}
	var o StoreObs
	if got := unsafe.Sizeof(o); got != 200 {
		t.Fatalf("unsafe.Sizeof(StoreObs{}) = %d, want 200: wireRoute must use existing padding", got)
	}
	if unsafe.Offsetof(o.wireRoute) != unsafe.Offsetof(o.tsParsedOK)+1 {
		t.Fatalf("wireRoute at %d, want the padding byte after tsParsedOK (%d)", unsafe.Offsetof(o.wireRoute), unsafe.Offsetof(o.tsParsedOK))
	}
}

var hbTxSeq int

func hbObs(observer, name, path string, wire uint8, snr, rssi float64) *StoreObs {
	return &StoreObs{ObserverID: observer, ObserverName: name, PathJSON: path, wireRoute: wire, SNR: &snr, RSSI: &rssi,
		Timestamp: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}
}

func hbTx(pt, route int, decoded string, obs ...*StoreObs) *StoreTx {
	hbTxSeq++
	tx := &StoreTx{ID: hbTxSeq, Hash: fmt.Sprintf("hb%06d", hbTxSeq),
		PayloadType: &pt, RouteType: &route, DecodedJSON: decoded,
		FirstSeen: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), Observations: obs, ObservationCount: len(obs)}
	if len(obs) > 0 {
		tx.ObserverID, tx.ObserverName, tx.SNR, tx.RSSI, tx.PathJSON = obs[0].ObserverID, obs[0].ObserverName, obs[0].SNR, obs[0].RSSI, obs[0].PathJSON
	}
	return tx
}

func hbAdvert(origin string, route int, obs ...*StoreObs) *StoreTx {
	return hbTx(PayloadADVERT, route, `{"pubKey":"`+origin+`"}`, obs...)
}

func hbStore(t *testing.T) (*DB, *PacketStore) {
	t.Helper()
	db := setupTestDB(t)
	seen := time.Now().UTC().Format(time.RFC3339)
	for _, n := range []struct{ key, name, role string }{
		{hbNode, "Node A1B2", "repeater"}, {hbTwin, "Twin A1FF", "repeater"}, {hbOther, "Other C3D4", "repeater"},
		{hbListener, "Listener", "repeater"}, {hbComp, "Companion", "companion"},
	} {
		if _, err := db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen) VALUES (?, ?, ?, ?)", n.key, n.name, n.role, seen); err != nil {
			t.Fatal(err)
		}
	}
	return db, NewPacketStore(db, nil)
}

func hbIndex(s *PacketStore, key string, txs ...*StoreTx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tx := range txs {
		s.addToByNode(tx, key)
	}
}

type hbHealth struct {
	Stats struct {
		AvgSnr *float64 `json:"avgSnr"`
	} `json:"stats"`
	Direct   []map[string]interface{} `json:"directObservers"`
	Relay    []map[string]interface{} `json:"relayObservers"`
	Observer interface{}              `json:"observers"`
}

func hbDecode(t *testing.T, v interface{}) hbHealth {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var h hbHealth
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func hbIDs(rows []map[string]interface{}) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r["observer_id"].(string))
	}
	sort.Strings(ids)
	return ids
}

func hbRow(t *testing.T, rows []map[string]interface{}, id string) map[string]interface{} {
	t.Helper()
	for _, r := range rows {
		if r["observer_id"] == id {
			return r
		}
	}
	t.Fatalf("observer %s not in %v", id, rows)
	return nil
}

// One content hash heard on several route types must be classified per
// observation (#308), never by the first-ingested route of the transmission.
func TestHeardBy_MixedRouteTypesPerHash_76(t *testing.T) {
	_, s := hbStore(t)
	// Own advert, first ingested as FLOOD.
	own := hbAdvert(hbNode, RouteFlood,
		hbObs("obs-flood0", "Flood zero", `[]`, RouteFlood+1, 4, -90),
		hbObs("obs-zero", "Zero hop", `[]`, RouteDirect+1, 6, -80),
		hbObs("obs-relayed", "Relayed", `["C3D4"]`, RouteFlood+1, 9, -70))
	// Another node's advert, first ingested as DIRECT (a zero-hop copy).
	other := hbAdvert(hbOther, RouteDirect,
		hbObs("obs-zero-other", "Zero hop other", `[]`, RouteDirect+1, 11, -60),
		hbObs("obs-hears-node", "Hears node", `["A1B2"]`, RouteFlood+1, 2, -95),
		hbObs("obs-direct-path", "Direct path", `["A1B2"]`, RouteDirect+1, 12, -50))
	hbIndex(s, hbNode, own, other)
	health, err := s.GetNodeHealth(hbNode, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	h := hbDecode(t, health)
	if got, want := hbIDs(h.Direct), []string{"obs-flood0", "obs-hears-node", "obs-zero"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("direct = %v, want %v", got, want)
	}
	if got, want := hbIDs(h.Relay), []string{"obs-direct-path", "obs-relayed", "obs-zero-other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("relay = %v, want %v", got, want)
	}
	if h.Observer != nil {
		t.Fatalf("the mixed observers list must be gone, got %v", h.Observer)
	}

	// Without frames, a transmission seen on both route families is unknown:
	// no credit, rather than the first-ingested route's.
	_, s2 := hbStore(t)
	mixed := hbAdvert(hbNode, RouteFlood, hbObs("obs-x", "X", `[]`, 0, 1, -99))
	mixed.routeMask, mixed.routeMaskKnown = 0b0110, true
	floodOnly := hbAdvert(hbNode, RouteFlood, hbObs("obs-y", "Y", `[]`, 0, 1, -99))
	floodOnly.routeMask, floodOnly.routeMaskKnown = 0b0010, true
	hbIndex(s2, hbNode, mixed, floodOnly)
	health2, _ := s2.GetNodeHealth(hbNode, time.Now().UTC())
	h2 := hbDecode(t, health2)
	if got := hbIDs(h2.Direct); !reflect.DeepEqual(got, []string{"obs-y"}) {
		t.Fatalf("direct without frames = %v, want [obs-y]", got)
	}
	if got := hbIDs(h2.Relay); !reflect.DeepEqual(got, []string{"obs-x"}) {
		t.Fatalf("relay without frames = %v, want [obs-x]", got)
	}
}

// Relay observers come from every observation, not the representative one;
// packetCount counts transmissions, not copies.
func TestHeardBy_RelayObserversFromAllObservations_76(t *testing.T) {
	_, s := hbStore(t)
	msg := hbTx(PayloadTXT_MSG, RouteFlood, `{"srcPubKey":"`+hbNode+`"}`,
		hbObs("obs-rep", "Representative", `["C3D4"]`, RouteFlood+1, 1, -99),
		hbObs("obs-extra", "Extra", `["C3D4"]`, RouteFlood+1, 1, -99),
		hbObs("obs-extra", "Extra", `["C3D4","A1FF"]`, RouteFlood+1, 1, -99))
	msg2 := hbTx(PayloadTXT_MSG, RouteFlood, `{"srcPubKey":"`+hbNode+`"}`,
		hbObs("obs-extra", "Extra", `[]`, RouteFlood+1, 1, -99))
	hbIndex(s, hbNode, msg, msg2)
	health, _ := s.GetNodeHealth(hbNode, time.Now().UTC())
	h := hbDecode(t, health)
	if len(h.Direct) != 0 {
		t.Fatalf("a non-advert never proves a direct reception: %v", h.Direct)
	}
	if got := hbIDs(h.Relay); !reflect.DeepEqual(got, []string{"obs-extra", "obs-rep"}) {
		t.Fatalf("relay = %v, want both observers", got)
	}
	if got := hbRow(t, h.Relay, "obs-extra")["packetCount"]; got != float64(2) {
		t.Fatalf("obs-extra packetCount = %v, want 2 transmissions", got)
	}
	bulk := s.GetBulkHealth(10, "", "")
	for _, row := range bulk {
		if row["public_key"] == hbNode {
			if b := hbDecode(t, row); !reflect.DeepEqual(hbIDs(b.Relay), hbIDs(h.Relay)) {
				t.Fatalf("bulk relay %v != node relay %v", hbIDs(b.Relay), hbIDs(h.Relay))
			}
		}
	}
}

// Signal values are reported only for direct receptions, and only those
// receptions' values: a direct observer's relayed copies carry another
// node's SNR.
func TestHeardBy_SignalOnlyFromDirectReceptions_76(t *testing.T) {
	_, s := hbStore(t)
	advert := hbAdvert(hbNode, RouteFlood,
		hbObs("obs-d", "Direct", `[]`, RouteFlood+1, -3, -110),
		hbObs("obs-d", "Direct", `["C3D4"]`, RouteFlood+1, 9, -60),
		hbObs("obs-r", "Relay only", `["C3D4"]`, RouteFlood+1, 12, -50))
	zero := hbAdvert(hbNode, RouteDirect, hbObs("obs-d", "Direct", `[]`, RouteDirect+1, -1, -100))
	forged := hbAdvert(hbNode, RouteFlood, hbObs("obs-forged", "Forged", `[]`, RouteFlood+1, 20, -40))
	forged.DecodedJSON = `{"pubKey":"` + hbNode + `","signatureValid":false}`
	self := hbAdvert(hbNode, RouteFlood, hbObs(strings.ToUpper(hbNode), "Itself", `[]`, RouteFlood+1, 30, -30))
	hbIndex(s, hbNode, advert, zero, forged, self)
	health, _ := s.GetNodeHealth(hbNode, time.Now().UTC())
	h := hbDecode(t, health)
	if got := hbIDs(h.Direct); !reflect.DeepEqual(got, []string{"obs-d"}) {
		t.Fatalf("direct = %v, want [obs-d]", got)
	}
	d := hbRow(t, h.Direct, "obs-d")
	if d["packetCount"] != float64(2) || d["avgSnr"] != float64(-2) || d["avgRssi"] != float64(-105) {
		t.Fatalf("direct row = %v, want 2 receptions, SNR -2, RSSI -105", d)
	}
	if h.Stats.AvgSnr == nil || *h.Stats.AvgSnr != -2 {
		t.Fatalf("stats.avgSnr = %v, want -2 (direct receptions only)", h.Stats.AvgSnr)
	}
	for _, r := range h.Relay {
		if _, has := r["avgSnr"]; has {
			t.Fatalf("relay row carries SNR: %v", r)
		}
		if _, has := r["avgRssi"]; has {
			t.Fatalf("relay row carries RSSI: %v", r)
		}
	}
	if got := hbIDs(h.Relay); !reflect.DeepEqual(got, []string{strings.ToUpper(hbNode), "obs-forged", "obs-r"}) {
		t.Fatalf("relay = %v: a forged advert and the node's own upload are not direct receptions", got)
	}

	// No direct reception: no node SNR at all, in single and bulk health.
	_, s2 := hbStore(t)
	hbIndex(s2, hbNode, hbAdvert(hbNode, RouteFlood, hbObs("obs-r", "Relay only", `["C3D4"]`, RouteFlood+1, 12, -50)))
	health2, _ := s2.GetNodeHealth(hbNode, time.Now().UTC())
	if h2 := hbDecode(t, health2); h2.Stats.AvgSnr != nil {
		t.Fatalf("stats.avgSnr = %v without a direct reception", *h2.Stats.AvgSnr)
	}
	for _, row := range s2.GetBulkHealth(10, "", "") {
		if row["public_key"] == hbNode && hbDecode(t, row).Stats.AvgSnr != nil {
			t.Fatal("bulk stats.avgSnr set without a direct reception")
		}
	}
}

// Single and bulk health share one classifier, so their lists are equal.
func TestHeardBy_BulkMatchesNodeHealth_76(t *testing.T) {
	_, s := hbStore(t)
	hbIndex(s, hbNode,
		hbAdvert(hbNode, RouteFlood, hbObs("obs-1", "One", `[]`, RouteFlood+1, 1, -91), hbObs("obs-2", "Two", `["C3D4"]`, RouteFlood+1, 2, -92)),
		hbAdvert(hbOther, RouteFlood, hbObs("obs-3", "Three", `["A1B2"]`, 0, 3, -93), hbObs("obs-4", "Four", `["A1B2","C3D4"]`, 0, 4, -94)))
	health, _ := s.GetNodeHealth(hbNode, time.Now().UTC())
	h := hbDecode(t, health)
	found := false
	for _, row := range s.GetBulkHealth(10, "", "") {
		if row["public_key"] != hbNode {
			continue
		}
		found = true
		b := hbDecode(t, row)
		if !reflect.DeepEqual(b.Direct, h.Direct) || !reflect.DeepEqual(b.Relay, h.Relay) || !reflect.DeepEqual(b.Stats.AvgSnr, h.Stats.AvgSnr) {
			t.Fatalf("bulk %+v != node %+v", b, h)
		}
	}
	if !found || !reflect.DeepEqual(hbIDs(h.Direct), []string{"obs-1", "obs-3"}) {
		t.Fatalf("found=%v direct=%v", found, hbIDs(h.Direct))
	}
}

// Hidden observer identities (#1181 rules) leave both lists on both
// endpoints: observer blacklist, recorded name and current name.
func TestHeardBy_HiddenObserversRemoved_76(t *testing.T) {
	db, s := hbStore(t)
	if _, err := db.conn.Exec(`INSERT INTO observers (id, name) VALUES ('obs-renamed', 'HIDE renamed')`); err != nil {
		t.Fatal(err)
	}
	hbIndex(s, hbNode, hbAdvert(hbNode, RouteFlood,
		hbObs("obs-visible", "Visible", `[]`, RouteFlood+1, 1, -91),
		hbObs("OBS-BLACK", "Blacklisted", `[]`, RouteFlood+1, 1, -91),
		hbObs("obs-named", "HIDE recorded", `["C3D4"]`, RouteFlood+1, 1, -91),
		hbObs("obs-renamed", "Was visible", `["C3D4"]`, RouteFlood+1, 1, -91),
		hbObs("obs-relay", "Relay", `["C3D4"]`, RouteFlood+1, 1, -91)))
	cfg := &Config{Port: 3000, ObserverBlacklist: []string{"obs-black"}}
	cfg.SetHiddenNamePrefixes([]string{"HIDE"})
	srv := NewServer(db, cfg, NewHub())
	srv.store = s
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	get := func(path string) []byte {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	var single hbHealth
	if err := json.Unmarshal(get("/api/nodes/"+hbNode+"/health"), &single); err != nil {
		t.Fatal(err)
	}
	var bulk []hbHealth
	if err := json.Unmarshal(get("/api/nodes/bulk-health?limit=10"), &bulk); err != nil {
		t.Fatal(err)
	}
	var bulkNode *hbHealth
	var keys []map[string]interface{}
	raw := get("/api/nodes/bulk-health?limit=10")
	_ = json.Unmarshal(raw, &keys)
	for i, k := range keys {
		if k["public_key"] == hbNode {
			bulkNode = &bulk[i]
		}
	}
	if bulkNode == nil {
		t.Fatal("node missing from bulk-health")
	}
	for name, h := range map[string]hbHealth{"health": single, "bulk-health": *bulkNode} {
		if got := hbIDs(h.Direct); !reflect.DeepEqual(got, []string{"obs-visible"}) {
			t.Errorf("%s direct = %v, want [obs-visible]", name, got)
		}
		if got := hbIDs(h.Relay); !reflect.DeepEqual(got, []string{"obs-relay"}) {
			t.Errorf("%s relay = %v, want [obs-relay]", name, got)
		}
	}
}

// Every path that materialises an observation keeps its frame's route type:
// cold Load, chunked load, loadChunk, IngestNewFromDB, IngestNewObservations.
func TestWireRoute_SetOnEveryObservationPath_76(t *testing.T) {
	snapshot := func(s *PacketStore) map[int]uint8 {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := map[int]uint8{}
		for _, tx := range s.packets {
			for _, o := range tx.Observations {
				out[o.ID] = o.wireRoute
			}
		}
		return out
	}
	// rmSeed frames: 1102.. flood, 1300.. transport direct, 1101.. flood, 1200 direct.
	want := map[int]uint8{1: RouteFlood + 1, 2: RouteTransportDirect + 1, 3: RouteFlood + 1, 4: RouteDirect + 1}
	db := routeMaskServerDB(t)
	rmSeed(t, db)
	load := NewPacketStore(db, nil)
	if err := load.Load(); err != nil {
		t.Fatal(err)
	}
	chunked := NewPacketStore(db, nil)
	if err := chunked.LoadChunked(1); err != nil {
		t.Fatal(err)
	}
	window := NewPacketStore(db, nil)
	if err := window.loadChunk(time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*PacketStore{"Load": load, "LoadChunked": chunked, "loadChunk": window} {
		if got := snapshot(s); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: wireRoute %v, want %v", name, got, want)
		}
	}
	ts := time.Now().UTC().Add(-30 * time.Minute)
	rmInsertTx(t, db, 4, "new-tx", 1, 0b0010, ts.Format(time.RFC3339))
	rmInsertObs(t, db, 5, 4, 1, `[]`, "1100", ts.Unix())
	load.IngestNewFromDB(3, 100)
	rmInsertObs(t, db, 6, 2, 2, `[]`, "1200", ts.Unix())
	rmInsertObs(t, db, 7, 2, 1, `[]`, "", ts.Unix())
	load.IngestNewObservations(5, 100)
	want[5], want[6], want[7] = RouteFlood+1, RouteDirect+1, 0
	if got := snapshot(load); !reflect.DeepEqual(got, want) {
		t.Fatalf("live ingest: wireRoute %v, want %v", got, want)
	}
}

// GetBulkHealth stops reading the node rows at limit; it must release that
// connection before relayPrefixMapLocked queries the nodes again, or a
// single-connection pool (setupTestDB, the #76 benchmark) blocks forever.
func TestGetBulkHealth_ReleasesNodeRowsBeforeStoreWork_76(t *testing.T) {
	_, s := hbStore(t) // 5 nodes, one connection
	done := make(chan int, 1)
	go func() { done <- len(s.GetBulkHealth(2, "", "")) }()
	select {
	case n := <-done:
		if n != 2 {
			t.Fatalf("bulk rows = %d, want 2", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("GetBulkHealth blocked: node rows still hold the only connection")
	}
}

// The OpenAPI spec documents both health endpoints with the Heard By schemas,
// and each schema lists exactly the JSON fields of its Go struct, so the
// published contract cannot drift from the response.
func TestOpenAPI_HeardBySchemas_76(t *testing.T) {
	spec := fetchSpec(t)
	paths := asMap(t, spec["paths"], "paths")
	for path, want := range map[string]string{"/api/nodes/{pubkey}/health": "NodeHealthResponse", "/api/nodes/bulk-health": "BulkHealthResponse"} {
		get := asMap(t, asMap(t, paths[path], path)["get"], path+".get")
		resp := asMap(t, asMap(t, get["responses"], path)["200"], path+".200")
		schema := asMap(t, asMap(t, asMap(t, resp["content"], path)["application/json"], path)["schema"], path+".schema")
		if ref, _ := schema["$ref"].(string); ref != "#/components/schemas/"+want {
			t.Errorf("%s: 200 schema = %q, want %s", path, ref, want)
		}
	}
	schemas := asMap(t, asMap(t, spec["components"], "components")["schemas"], "schemas")
	props := func(name string) []string {
		p := asMap(t, asMap(t, schemas[name], name)["properties"], name+".properties")
		out := make([]string, 0, len(p))
		for k := range p {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	jsonFields := func(v interface{}) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			out = append(out, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
		}
		sort.Strings(out)
		return out
	}
	if got, want := props("HeardDirectObserver"), jsonFields(HeardDirectObserver{}); !reflect.DeepEqual(got, want) {
		t.Errorf("HeardDirectObserver properties %v, struct fields %v", got, want)
	}
	if got, want := props("HeardRelayObserver"), jsonFields(HeardRelayObserver{}); !reflect.DeepEqual(got, want) {
		t.Errorf("HeardRelayObserver properties %v, struct fields %v", got, want)
	}
	for _, name := range []string{"NodeHealthResponse", "BulkHealthEntry"} {
		p := props(name)
		joined := "," + strings.Join(p, ",") + ","
		if !strings.Contains(joined, ",directObservers,") || !strings.Contains(joined, ",relayObservers,") || strings.Contains(joined, ",observers,") {
			t.Errorf("%s properties %v: want directObservers and relayObservers, no observers", name, p)
		}
	}
	if got, want := props("BulkHealthEntry"), jsonFields(BulkHealthEntry{}); !reflect.DeepEqual(got, want) {
		t.Errorf("BulkHealthEntry properties %v, struct fields %v", got, want)
	}
}
