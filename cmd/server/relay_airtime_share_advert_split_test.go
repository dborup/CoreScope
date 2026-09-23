package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/lora"
)

// Route-class contract for the ADVERT split in Relay Airtime Share
// (Kpa-clawbot/CoreScope#2041). MeshCore firmware (src/Packet.h) defines the
// two low header bits as the route type: 0 transport-flood, 1 flood, 2 direct,
// 3 transport-direct. Every firmware advert sent over a direct route goes
// through Mesh::sendZeroHop (path_len 0), so direct adverts are zero-hop.
// NULL and any value outside 0..3 must fall back to the historical
// unsuffixed ADVERT bucket, and no other payload type may be split.

func TestRelayAirtimeKey_RouteClassification(t *testing.T) {
	cases := []struct {
		name        string
		payloadType int
		routeType   *int
		wantRoute   relayAirtimeAdvertRoute
		wantLabel   string
		wantClass   string // route_class; "" means null (non-ADVERT rows)
	}{
		{"advert transport flood", PayloadADVERT, intPtr(RouteTransportFlood), relayAirtimeAdvertFlood, "ADVERT (flood)", "flood"},
		{"advert flood", PayloadADVERT, intPtr(RouteFlood), relayAirtimeAdvertFlood, "ADVERT (flood)", "flood"},
		{"advert direct", PayloadADVERT, intPtr(RouteDirect), relayAirtimeAdvertZeroHop, "ADVERT (zero-hop)", "zero_hop"},
		{"advert transport direct", PayloadADVERT, intPtr(RouteTransportDirect), relayAirtimeAdvertZeroHop, "ADVERT (zero-hop)", "zero_hop"},
		{"advert NULL route", PayloadADVERT, nil, relayAirtimeAdvertUnknown, "ADVERT", "legacy"},
		{"advert route 4", PayloadADVERT, intPtr(4), relayAirtimeAdvertUnknown, "ADVERT", "legacy"},
		{"advert route 99", PayloadADVERT, intPtr(99), relayAirtimeAdvertUnknown, "ADVERT", "legacy"},
		{"advert route -1", PayloadADVERT, intPtr(-1), relayAirtimeAdvertUnknown, "ADVERT", "legacy"},
		{"advert route -2", PayloadADVERT, intPtr(-2), relayAirtimeAdvertUnknown, "ADVERT", "legacy"},
		{"unknown payload 12", 12, intPtr(RouteFlood), relayAirtimeAdvertUnknown, "UNK", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &StoreTx{PayloadType: intPtr(tc.payloadType), RouteType: tc.routeType}
			key := relayAirtimeKey(tx)
			want := relayAirtimeBucketKey{payloadType: tc.payloadType, advertRoute: tc.wantRoute}
			if key != want {
				t.Fatalf("relayAirtimeKey = %+v, want %+v", key, want)
			}
			if got := relayAirtimeBucketName(key); got != tc.wantLabel {
				t.Fatalf("relayAirtimeBucketName = %q, want %q", got, tc.wantLabel)
			}
			got := relayAirtimeRouteClass(key)
			if (got == nil) != (tc.wantClass == "") || (got != nil && *got != tc.wantClass) {
				t.Fatalf("relayAirtimeRouteClass = %v, want %q", got, tc.wantClass)
			}
		})
	}
}

// Every non-ADVERT payload type, including unnamed ones, must map to exactly
// one bucket regardless of route, keeping the pre-split grouping.
func TestRelayAirtimeKey_NonAdvertPayloadsIgnoreRoute(t *testing.T) {
	routes := []*int{nil, intPtr(0), intPtr(1), intPtr(2), intPtr(3), intPtr(7), intPtr(-1)}
	for pt := 0; pt <= 15; pt++ {
		if pt == PayloadADVERT {
			continue
		}
		want := relayAirtimeBucketKey{payloadType: pt}
		wantName := payloadTypeNames[pt]
		if wantName == "" {
			wantName = "UNK"
		}
		for _, rt := range routes {
			key := relayAirtimeKey(&StoreTx{PayloadType: intPtr(pt), RouteType: rt})
			if key != want {
				t.Fatalf("payload %d route %v: key = %+v, want %+v", pt, rt, key, want)
			}
			if got := relayAirtimeBucketName(key); got != wantName {
				t.Fatalf("payload %d route %v: name = %q, want %q", pt, rt, got, wantName)
			}
			if got := relayAirtimeRouteClass(key); got != nil {
				t.Fatalf("payload %d route %v: route_class = %q, want nil", pt, rt, *got)
			}
		}
	}
}

type relayAirtimeSplitRow struct {
	PayloadType string  `json:"payload_type"`
	Type        int     `json:"type"`
	Count       int     `json:"count"`
	CountPct    float64 `json:"count_pct"`
	Score       int64   `json:"score"`
	AirtimePct  float64 `json:"airtime_pct"`
	RouteClass  *string `json:"route_class"`
}

type relayAirtimeSplitResponse struct {
	Rows       []relayAirtimeSplitRow `json:"rows"`
	TotalCount int                    `json:"total_count"`
	TotalScore int64                  `json:"total_score"`
}

type relayAirtimeSplitFixture struct {
	payloadType int
	route       *int
	bytes       int
	relays      int
	hash        string
}

// relayAirtimeSplitStore builds a store whose transmissions each have the
// fixture's route type and number of distinct relays.
func relayAirtimeSplitStore(fixtures []relayAirtimeSplitFixture) *PacketStore {
	packets := make([]*StoreTx, 0, len(fixtures))
	for i, f := range fixtures {
		tx := makeRelayAirtimeTx(i+1, f.payloadType, f.bytes, f.relays, f.hash)
		tx.RouteType = f.route
		packets = append(packets, tx)
	}
	store := newRelayAirtimeShareTestStore(packets)
	for i, f := range fixtures {
		if f.relays == 0 {
			continue
		}
		pks := make([]string, f.relays)
		for r := range pks {
			pks[r] = "relay-" + zeroPad(r, 3)
		}
		store.addToResolvedPubkeyIndex(packets[i].ID, pks)
	}
	return store
}

func relayAirtimeSplitScore(bytes, relays int) int64 {
	return int64(lora.TimeOnAir(bytes, defaultLoRaPreset())) * int64(relays)
}

// mixedAdvertFixtures holds all three ADVERT buckets, ACKs on both route
// classes, a GRP_TXT, an unnamed payload type, a zero-score ADVERT and a
// re-observed hash that must only be counted once.
func mixedAdvertFixtures() []relayAirtimeSplitFixture {
	return []relayAirtimeSplitFixture{
		{PayloadADVERT, intPtr(0), 120, 3, "adv-tf"},
		{PayloadADVERT, intPtr(1), 110, 4, "adv-f"},
		{PayloadADVERT, intPtr(1), 110, 4, "adv-f"}, // re-observation: same hash
		{PayloadADVERT, intPtr(2), 100, 0, "adv-d"}, // zero-hop, never relayed
		{PayloadADVERT, intPtr(3), 100, 1, "adv-td"},
		{PayloadADVERT, nil, 90, 2, "adv-null"},
		{PayloadADVERT, intPtr(7), 90, 1, "adv-unk"},
		{PayloadACK, intPtr(0), 10, 2, "ack-tf"},
		{PayloadACK, intPtr(2), 10, 1, "ack-d"},
		{PayloadGRP_TXT, intPtr(1), 40, 2, "grp"},
		{13, intPtr(1), 30, 1, "pt13"},
	}
}

func TestRelayAirtimeShare_AdvertSplitTotalsAndDedup(t *testing.T) {
	store := relayAirtimeSplitStore(mixedAdvertFixtures())
	result := store.computeRelayAirtimeShare(TimeWindow{})
	rows, ok := result["rows"].([]map[string]interface{})
	if !ok {
		t.Fatalf("rows has type %T", result["rows"])
	}

	type want struct {
		typ   int
		count int
		score int64
	}
	wants := map[string]want{
		"ADVERT (flood)":    {PayloadADVERT, 2, relayAirtimeSplitScore(120, 3) + relayAirtimeSplitScore(110, 4)},
		"ADVERT (zero-hop)": {PayloadADVERT, 2, relayAirtimeSplitScore(100, 1)},
		"ADVERT":            {PayloadADVERT, 2, relayAirtimeSplitScore(90, 2) + relayAirtimeSplitScore(90, 1)},
		"ACK":               {PayloadACK, 2, relayAirtimeSplitScore(10, 2) + relayAirtimeSplitScore(10, 1)},
		"GRP_TXT":           {PayloadGRP_TXT, 1, relayAirtimeSplitScore(40, 2)},
		"UNK":               {13, 1, relayAirtimeSplitScore(30, 1)},
	}
	var wantTotalCount int
	var wantTotalScore int64
	for _, w := range wants {
		wantTotalCount += w.count
		wantTotalScore += w.score
	}
	if wantTotalCount != 10 {
		t.Fatalf("fixture sanity: expected 10 distinct hashes, got %d", wantTotalCount)
	}

	if got := result["total_count"].(int); got != wantTotalCount {
		t.Errorf("total_count = %d, want %d (re-observed hash must count once)", got, wantTotalCount)
	}
	if got := result["total_score"].(int64); got != wantTotalScore {
		t.Errorf("total_score = %d, want %d", got, wantTotalScore)
	}
	if len(rows) != len(wants) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(wants), rows)
	}

	seen := make(map[string]bool, len(rows))
	var sumCount int
	var sumScore int64
	var sumCountPct, sumAirtimePct float64
	for _, r := range rows {
		label := r["payload_type"].(string)
		if seen[label] {
			t.Fatalf("duplicate payload_type label %q", label)
		}
		seen[label] = true
		w, ok := wants[label]
		if !ok {
			t.Fatalf("unexpected row %q", label)
		}
		count, score := r["count"].(int), r["score"].(int64)
		countPct, airtimePct := r["count_pct"].(float64), r["airtime_pct"].(float64)
		if r["type"].(int) != w.typ || count != w.count || score != w.score {
			t.Errorf("%s: type=%v count=%d score=%d, want type=%d count=%d score=%d",
				label, r["type"], count, score, w.typ, w.count, w.score)
		}
		if want := float64(w.count) / float64(wantTotalCount) * 100; math.Abs(countPct-want) > 1e-9 {
			t.Errorf("%s: count_pct = %v, want %v", label, countPct, want)
		}
		if want := float64(w.score) / float64(wantTotalScore) * 100; math.Abs(airtimePct-want) > 1e-9 {
			t.Errorf("%s: airtime_pct = %v, want %v", label, airtimePct, want)
		}
		sumCount += count
		sumScore += score
		sumCountPct += countPct
		sumAirtimePct += airtimePct
	}
	if sumCount != wantTotalCount || sumScore != wantTotalScore {
		t.Errorf("row sums count=%d score=%d do not match totals %d/%d", sumCount, sumScore, wantTotalCount, wantTotalScore)
	}
	if math.Abs(sumCountPct-100) > 1e-9 || math.Abs(sumAirtimePct-100) > 1e-9 {
		t.Errorf("percentages sum to count=%v airtime=%v, want 100", sumCountPct, sumAirtimePct)
	}

	// Rows are sorted airtime desc, count desc, label asc, so repeated
	// computations over the same data must return identical ordering.
	firstOrder := relayAirtimeRowLabels(rows)
	for i := 0; i < 20; i++ {
		again := store.computeRelayAirtimeShare(TimeWindow{})["rows"].([]map[string]interface{})
		if got := relayAirtimeRowLabels(again); !reflect.DeepEqual(got, firstOrder) {
			t.Fatalf("row order not deterministic: %v vs %v", got, firstOrder)
		}
	}
	if !sort.SliceIsSorted(rows, func(i, j int) bool {
		return rows[i]["airtime_pct"].(float64) > rows[j]["airtime_pct"].(float64)
	}) {
		t.Errorf("rows not sorted by airtime_pct desc: %v", firstOrder)
	}
}

func relayAirtimeRowLabels(rows []map[string]interface{}) []string {
	labels := make([]string, len(rows))
	for i, r := range rows {
		labels[i] = r["payload_type"].(string)
	}
	return labels
}

func TestRelayAirtimeShare_AdvertSplitSingleClassAndEmpty(t *testing.T) {
	cases := []struct {
		name     string
		fixtures []relayAirtimeSplitFixture
		want     []string
	}{
		{"empty", nil, []string{}},
		{"flood only", []relayAirtimeSplitFixture{
			{PayloadADVERT, intPtr(0), 100, 1, "a"},
			{PayloadADVERT, intPtr(1), 100, 1, "b"},
		}, []string{"ADVERT (flood)"}},
		{"zero-hop only", []relayAirtimeSplitFixture{
			{PayloadADVERT, intPtr(2), 100, 1, "a"},
			{PayloadADVERT, intPtr(3), 100, 1, "b"},
		}, []string{"ADVERT (zero-hop)"}},
		{"legacy only", []relayAirtimeSplitFixture{
			{PayloadADVERT, nil, 100, 1, "a"},
			{PayloadADVERT, intPtr(42), 100, 1, "b"},
		}, []string{"ADVERT"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := relayAirtimeSplitStore(tc.fixtures).computeRelayAirtimeShare(TimeWindow{})
			rows := result["rows"].([]map[string]interface{})
			if got := relayAirtimeRowLabels(rows); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
			if result["total_count"].(int) != len(tc.fixtures) {
				t.Errorf("total_count = %v, want %d", result["total_count"], len(tc.fixtures))
			}
			if len(rows) == 1 {
				if rows[0]["count_pct"].(float64) != 100 || rows[0]["airtime_pct"].(float64) != 100 {
					t.Errorf("single bucket should hold 100%%: %+v", rows[0])
				}
			}
		})
	}
}

// The serialized API response is the contract the frontend and external
// clients see: the three ADVERT rows share numeric type 4, several unnamed
// payload types share the "UNK" label, and (type, route_class) identifies
// every row. Existing fields are unchanged; route_class is always present.
func TestRelayAirtimeShareAPI_AdvertRowsShareNumericType(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	srv := NewServer(db, &Config{Port: 3000}, NewHub())
	fixtures := append(mixedAdvertFixtures(), relayAirtimeSplitFixture{14, intPtr(1), 30, 1, "pt14"})
	srv.store = relayAirtimeSplitStore(fixtures)
	router := mux.NewRouter()
	srv.RegisterRoutes(router)

	req := httptest.NewRequest("GET", "/api/analytics/relay-airtime-share", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var raw struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	wantFields := []string{"airtime_pct", "count", "count_pct", "payload_type", "route_class", "score", "type"}
	for _, r := range raw.Rows {
		fields := make([]string, 0, len(r))
		for k := range r {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		if !reflect.DeepEqual(fields, wantFields) {
			t.Fatalf("row fields = %v, want %v", fields, wantFields)
		}
	}

	// route_class is the machine-readable ADVERT identity; it is JSON null on
	// every other row, never absent.
	wantClass := map[string]string{
		"ADVERT (flood)":    `"flood"`,
		"ADVERT (zero-hop)": `"zero_hop"`,
		"ADVERT":            `"legacy"`,
		"ACK":               `null`,
		"GRP_TXT":           `null`,
		"UNK":               `null`,
	}
	for _, r := range raw.Rows {
		var label string
		if err := json.Unmarshal(r["payload_type"], &label); err != nil {
			t.Fatalf("decode payload_type: %v", err)
		}
		if got := string(r["route_class"]); got != wantClass[label] {
			t.Errorf("%s: route_class = %s, want %s", label, got, wantClass[label])
		}
	}

	var resp relayAirtimeSplitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	advertLabels := []string{}
	unknownTypes := []int{}
	identities := make(map[string]bool, len(resp.Rows))
	for _, r := range resp.Rows {
		class := "<null>"
		if r.RouteClass != nil {
			class = *r.RouteClass
		}
		id := fmt.Sprintf("%d/%s", r.Type, class)
		if identities[id] {
			t.Fatalf("(type, route_class) %s repeated; it must identify each row", id)
		}
		identities[id] = true
		if r.Type == PayloadADVERT {
			advertLabels = append(advertLabels, r.PayloadType)
		}
		if r.PayloadType == "UNK" {
			unknownTypes = append(unknownTypes, r.Type)
		}
	}
	sort.Strings(advertLabels)
	if want := []string{"ADVERT", "ADVERT (flood)", "ADVERT (zero-hop)"}; !reflect.DeepEqual(advertLabels, want) {
		t.Fatalf("rows with type %d = %v, want %v", PayloadADVERT, advertLabels, want)
	}
	sort.Ints(unknownTypes)
	if !reflect.DeepEqual(unknownTypes, []int{13, 14}) {
		t.Fatalf("UNK rows have types %v, want [13 14]", unknownTypes)
	}
	if resp.TotalCount != 11 {
		t.Errorf("total_count = %d, want 11", resp.TotalCount)
	}
}

// Payload Type Mix (RF analytics) must keep a single combined ADVERT entry;
// the route split is specific to Relay Airtime Share.
func TestPayloadTypeMix_AdvertStaysCombined(t *testing.T) {
	store := relayAirtimeSplitStore(mixedAdvertFixtures())
	result := store.computeAnalyticsRF("", "", TimeWindow{})
	encoded, err := json.Marshal(result["payloadTypes"])
	if err != nil {
		t.Fatalf("marshal payloadTypes: %v", err)
	}
	var entries []struct {
		Type  int    `json:"type"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(encoded, &entries); err != nil {
		t.Fatalf("decode payloadTypes: %v", err)
	}
	var adverts []string
	for _, e := range entries {
		if e.Type == PayloadADVERT {
			adverts = append(adverts, e.Name)
			if e.Name != "ADVERT" || e.Count != 6 {
				t.Errorf("ADVERT entry = %+v, want name ADVERT count 6", e)
			}
		}
	}
	if len(adverts) != 1 {
		t.Fatalf("Payload Type Mix has %d ADVERT entries %v, want exactly 1", len(adverts), adverts)
	}
}

// Unnamed payload types share the "UNK" label, so equal-airtime UNK rows
// need the numeric type as the final tiebreak to keep a stable order.
func TestRelayAirtimeShare_UnknownPayloadTieOrderIsStable(t *testing.T) {
	store := relayAirtimeSplitStore([]relayAirtimeSplitFixture{
		{14, intPtr(1), 30, 1, "pt14"},
		{12, intPtr(1), 30, 1, "pt12"},
		{13, intPtr(1), 30, 1, "pt13"},
	})
	for i := 0; i < 50; i++ {
		rows := store.computeRelayAirtimeShare(TimeWindow{})["rows"].([]map[string]interface{})
		got := make([]int, len(rows))
		for j, r := range rows {
			got[j] = r["type"].(int)
		}
		if !reflect.DeepEqual(got, []int{12, 13, 14}) {
			t.Fatalf("run %d: UNK row types = %v, want [12 13 14]", i, got)
		}
	}
}

// A contact re-shared as a zero-hop advert (shareContactZeroHop) has the same
// content hash as the original flood advert, so both land on one transmission
// whose route_type is that of the first observation the ingestor inserted
// (pinned in cmd/ingestor TestInsertTransmission_AdvertRouteIsFirstIngested).
// This test models the stored result of that contract: a single transmission
// with the first-inserted route_type whose relays come from the flood
// observations. Relay Airtime Share classifies by the stored route alone, so
// a relayed transmission stored as zero-hop stays on the zero_hop row rather
// than being reclassified from its relay count.
func TestRelayAirtimeShare_MixedRouteHashFollowsStoredRoute(t *testing.T) {
	cases := []struct {
		name      string
		stored    int
		wantLabel string
		wantClass string
	}{
		{"flood observation inserted first", RouteFlood, "ADVERT (flood)", "flood"},
		{"zero-hop re-share inserted first", RouteTransportDirect, "ADVERT (zero-hop)", "zero_hop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Relays come from the flood observations' resolved paths; the
			// zero-hop observation contributes none.
			store := relayAirtimeSplitStore([]relayAirtimeSplitFixture{
				{PayloadADVERT, intPtr(tc.stored), 110, 2, "mixed"},
			})
			rows := store.computeRelayAirtimeShare(TimeWindow{})["rows"].([]map[string]interface{})
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
			}
			r := rows[0]
			class, _ := r["route_class"].(*string)
			if r["payload_type"] != tc.wantLabel || class == nil || *class != tc.wantClass {
				t.Fatalf("row = %v/%v, want %s/%s", r["payload_type"], class, tc.wantLabel, tc.wantClass)
			}
			if r["count"].(int) != 1 || r["score"].(int64) != relayAirtimeSplitScore(110, 2) {
				t.Errorf("count=%v score=%v, want 1 and the flood relays' score", r["count"], r["score"])
			}
		})
	}
}
