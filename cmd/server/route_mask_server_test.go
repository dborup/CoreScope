package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dbschema"
)

// Issue #89: the server reads transmissions.route_mask (every raw route type
// observed for the content hash) in all load paths, classifies ADVERTs as
// flood / zero_hop / mixed from it, and reports the backfill status read-only.

func TestStoreTxLayoutFitsRouteMaskInPadding(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout budget is defined for 64-bit platforms")
	}
	if got := unsafe.Sizeof(StoreTx{}); got != 320 {
		t.Fatalf("unsafe.Sizeof(StoreTx{}) = %d, want 320: route mask fields must use existing padding", got)
	}
}

func TestAdvertRouteClassFromMask(t *testing.T) {
	rt := func(v int) *int { return &v }
	cases := []struct {
		name  string
		mask  uint8
		known bool
		route *int
		want  relayAirtimeAdvertRoute
	}{
		{"route 0 only", 0b0001, true, rt(0), relayAirtimeAdvertFlood},
		{"routes 0 and 1", 0b0011, true, rt(1), relayAirtimeAdvertFlood},
		{"route 2 only", 0b0100, true, rt(2), relayAirtimeAdvertZeroHop},
		{"routes 2 and 3", 0b1100, true, rt(3), relayAirtimeAdvertZeroHop},
		{"flood and zero-hop", 0b1010, true, rt(3), relayAirtimeAdvertMixed},
		{"all four", 0b1111, true, rt(0), relayAirtimeAdvertMixed},
		{"mask wins over stored route", 0b0010, true, rt(3), relayAirtimeAdvertFlood},
		{"unknown mask falls back to flood route", 0, false, rt(1), relayAirtimeAdvertFlood},
		{"unknown mask falls back to zero-hop route", 0, false, rt(2), relayAirtimeAdvertZeroHop},
		{"unknown mask, NULL route", 0, false, nil, relayAirtimeAdvertUnknown},
		{"unknown mask, out-of-range route", 0, false, rt(99), relayAirtimeAdvertUnknown},
		{"known empty mask falls back to route", 0, true, rt(2), relayAirtimeAdvertZeroHop},
		{"known empty mask, invalid route", 0, true, rt(7), relayAirtimeAdvertUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &StoreTx{RouteType: tc.route, routeMask: tc.mask, routeMaskKnown: tc.known}
			if got := advertRouteClass(tx); got != tc.want {
				t.Fatalf("advertRouteClass = %v, want %v", got, tc.want)
			}
		})
	}
}

func routeMaskServerDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	if _, err := db.conn.Exec(`ALTER TABLE transmissions ADD COLUMN route_mask INTEGER`); err != nil {
		t.Fatal(err)
	}
	db.hasRouteMaskFlag.forceTrue()
	db.hasObsRawHexFlag.forceTrue()
	for i, id := range []string{"obs-a", "obs-b"} {
		if _, err := db.conn.Exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, 'TST')`, i+1, id, id); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func rmInsertTx(t *testing.T, db *DB, id int, hash string, routeType, mask interface{}, firstSeen string) {
	t.Helper()
	if _, err := db.conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, route_mask)
		VALUES (?, '11020a0b', ?, ?, ?, 4, '{}', ?)`, id, hash, firstSeen, routeType, mask); err != nil {
		t.Fatal(err)
	}
}

func rmInsertObs(t *testing.T, db *DB, id, txID, observer int, pathJSON, rawHex string, ts int64) {
	t.Helper()
	if _, err := db.conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, path_json, timestamp, raw_hex) VALUES (?, ?, ?, ?, ?, ?)`,
		id, txID, observer, pathJSON, ts, rawHex); err != nil {
		t.Fatal(err)
	}
}

type rmView struct {
	Mask  uint8
	Known bool
}

func rmSnapshot(s *PacketStore) map[string]rmView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]rmView{}
	for _, tx := range s.packets {
		out[tx.Hash] = rmView{tx.routeMask, tx.routeMaskKnown}
	}
	return out
}

func rmSeed(t *testing.T, db *DB) {
	t.Helper()
	now := time.Now().UTC()
	fs := now.Add(-time.Hour).Format(time.RFC3339)
	ts := now.Add(-time.Hour).Unix()
	rmInsertTx(t, db, 1, "mixed", 3, 0b1010, fs)
	rmInsertObs(t, db, 1, 1, 1, `["A1","B2"]`, "1102a1b2", ts)
	rmInsertObs(t, db, 2, 1, 2, `[]`, "130000000000", ts)
	rmInsertTx(t, db, 2, "flood", 1, 0b0010, fs)
	rmInsertObs(t, db, 3, 2, 1, `["A1"]`, "1101a1", ts)
	rmInsertTx(t, db, 3, "legacy", 2, nil, fs) // not backfilled yet
	rmInsertObs(t, db, 4, 3, 2, `[]`, "1200", ts)
}

var rmWantSeed = map[string]rmView{
	"mixed":  {0b1010, true},
	"flood":  {0b0010, true},
	"legacy": {0, false},
}

func TestRouteMask_ColdAndChunkedLoadKeepMask(t *testing.T) {
	db := routeMaskServerDB(t)
	rmSeed(t, db)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := rmSnapshot(s); !reflect.DeepEqual(got, rmWantSeed) {
		t.Fatalf("Load: %v, want %v", got, rmWantSeed)
	}
	c := NewPacketStore(db, nil)
	if err := c.LoadChunked(1); err != nil {
		t.Fatal(err)
	}
	if got := rmSnapshot(c); !reflect.DeepEqual(got, rmWantSeed) {
		t.Fatalf("LoadChunked: %v, want %v", got, rmWantSeed)
	}
	l := NewPacketStore(db, nil)
	if err := l.loadChunk(time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := rmSnapshot(l); !reflect.DeepEqual(got, rmWantSeed) {
		t.Fatalf("loadChunk: %v, want %v", got, rmWantSeed)
	}
}

// Live ingest must end with the same masks a cold load of the same database
// produces: new transmissions carry their mask, and a new observation of an
// existing transmission brings its route bit even when the server polls
// before the ingestor's OR has committed.
func TestRouteMask_IncrementalIngestMatchesColdLoad(t *testing.T) {
	db := routeMaskServerDB(t)
	rmSeed(t, db)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Add(-30 * time.Minute)
	// New transmission, then a zero-hop observation of the known flood tx
	// whose OR has not reached transmissions.route_mask yet.
	rmInsertTx(t, db, 4, "new-zero-hop", 3, 0b1000, ts.Format(time.RFC3339))
	rmInsertObs(t, db, 5, 4, 2, `[]`, "130000000000", ts.Unix())
	s.IngestNewFromDB(3, 100)
	rmInsertObs(t, db, 6, 2, 2, `[]`, "130000000000", ts.Unix())
	s.IngestNewObservations(5, 100)
	got := rmSnapshot(s)
	if got["flood"] != (rmView{0b1010, true}) || got["new-zero-hop"] != (rmView{0b1000, true}) {
		t.Fatalf("after incremental ingest: %v", got)
	}
	// The ingestor's OR lands; a cold load must agree with the live view.
	if _, err := db.conn.Exec(`UPDATE transmissions SET route_mask = route_mask | 8 WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	cold := NewPacketStore(db, nil)
	if err := cold.Load(); err != nil {
		t.Fatal(err)
	}
	if want := rmSnapshot(cold); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental %v != cold load %v", got, want)
	}
	// A legacy (NULL) row stays unknown until the backfill fills it, even
	// when a new observation arrives.
	rmInsertObs(t, db, 7, 3, 1, `["C3"]`, "1101c3", ts.Unix())
	s.IngestNewObservations(6, 100)
	if v := rmSnapshot(s)["legacy"]; v.Known {
		t.Fatalf("legacy row became known from a single observation: %v", v)
	}
	if _, err := db.conn.Exec(`UPDATE transmissions SET route_mask = 6 WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	rmInsertObs(t, db, 8, 3, 2, `["D4"]`, "1101d4", ts.Unix())
	s.IngestNewObservations(7, 100)
	if v := rmSnapshot(s)["legacy"]; v != (rmView{0b0110, true}) {
		t.Fatalf("backfilled legacy row after a new observation = %v, want 0110 known", v)
	}
}

func TestRelayAirtimeShare_MixedAdvertCountsOnceOnMixedRow(t *testing.T) {
	mk := func(pt int, route interface{}, mask uint8, known bool, relays int, hash string) relayAirtimeSplitFixture {
		f := relayAirtimeSplitFixture{payloadType: pt, bytes: 100, relays: relays, hash: hash}
		if r, ok := route.(int); ok {
			f.route = intPtr(r)
		}
		return f
	}
	fixtures := []relayAirtimeSplitFixture{
		mk(PayloadADVERT, 3, 0b1010, true, 2, "mixed"),
		mk(PayloadADVERT, 1, 0b0010, true, 3, "flood"),
		mk(PayloadADVERT, 2, 0b0100, true, 0, "zero"),
		mk(PayloadADVERT, 2, 0, false, 0, "legacy-null-route2"),
		mk(PayloadADVERT, 99, 0, false, 1, "legacy-unknown"),
		mk(PayloadACK, 0, 0b1010, true, 1, "ack-mixed"),
	}
	masks := map[string][2]interface{}{
		"mixed": {uint8(0b1010), true}, "flood": {uint8(0b0010), true}, "zero": {uint8(0b0100), true},
		"legacy-null-route2": {uint8(0), false}, "legacy-unknown": {uint8(0), false}, "ack-mixed": {uint8(0b1010), true},
	}
	store := relayAirtimeSplitStore(fixtures)
	for _, tx := range store.packets {
		m := masks[tx.Hash]
		tx.routeMask, tx.routeMaskKnown = m[0].(uint8), m[1].(bool)
	}
	res := store.computeRelayAirtimeShare(TimeWindow{})
	rows := res["rows"].([]map[string]interface{})
	type row struct {
		label, class string
		count        int
		score        int64
	}
	got := map[string]row{}
	for _, r := range rows {
		class := "null"
		if p, ok := r["route_class"].(*string); ok && p != nil {
			class = *p
		}
		key := r["payload_type"].(string)
		if _, dup := got[key]; dup {
			t.Fatalf("duplicate row %s", key)
		}
		got[key] = row{key, class, r["count"].(int), r["score"].(int64)}
	}
	want := map[string]row{
		"ADVERT (mixed)":    {"ADVERT (mixed)", "mixed", 1, relayAirtimeSplitScore(100, 2)},
		"ADVERT (flood)":    {"ADVERT (flood)", "flood", 1, relayAirtimeSplitScore(100, 3)},
		"ADVERT (zero-hop)": {"ADVERT (zero-hop)", "zero_hop", 2, 0},
		"ADVERT":            {"ADVERT", "legacy", 1, relayAirtimeSplitScore(100, 1)},
		"ACK":               {"ACK", "null", 1, relayAirtimeSplitScore(100, 1)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows:\n got  %v\n want %v", got, want)
	}
	if res["total_count"].(int) != 6 {
		t.Fatalf("total_count = %v, want 6 (a mixed hash counts once)", res["total_count"])
	}
}

func rmStatus(t *testing.T, db *DB) RouteMaskBackfillStatus {
	t.Helper()
	return db.computeRouteMaskBackfillStatus()
}

func TestRouteMaskBackfillStatus_NeverReportsFalseComplete(t *testing.T) {
	db := routeMaskServerDB(t)
	rmSeed(t, db) // one NULL row
	if st := rmStatus(t, db); st.Status != "pending" || st.Remaining != nil {
		t.Fatalf("without the pending index: %+v, want pending with unknown remaining", st)
	}
	if _, err := db.conn.Exec(dbschema.CreateRouteMaskPendingIndexSQL); err != nil {
		t.Fatal(err)
	}
	if st := rmStatus(t, db); st.Status != "pending" || st.Remaining == nil || *st.Remaining != 1 {
		t.Fatalf("index present, backfill not started: %+v, want pending remaining=1", st)
	}
	db.conn.Exec(`CREATE TABLE _async_migrations (name TEXT PRIMARY KEY, status TEXT, started_at TEXT, ended_at TEXT, error TEXT)`)
	db.conn.Exec(`INSERT INTO _async_migrations (name, status) VALUES (?, 'pending_async')`, dbschema.RouteMaskBackfillMigration)
	if st := rmStatus(t, db); st.Status != "backfilling" || st.Remaining == nil || *st.Remaining != 1 {
		t.Fatalf("backfill running: %+v, want backfilling remaining=1", st)
	}
	// Marked done while a NULL row still exists (e.g. an older ingestor wrote
	// it after completion): must not claim complete.
	db.conn.Exec(`UPDATE _async_migrations SET status = 'done'`)
	if st := rmStatus(t, db); st.Status == "complete" {
		t.Fatalf("reported complete with a NULL row left: %+v", st)
	}
	db.conn.Exec(`UPDATE transmissions SET route_mask = 4 WHERE route_mask IS NULL`)
	if st := rmStatus(t, db); st.Status != "complete" || st.Remaining == nil || *st.Remaining != 0 {
		t.Fatalf("no NULL rows: %+v, want complete remaining=0", st)
	}
}

func TestRouteMaskBackfillStatus_OldSchemaIsPending(t *testing.T) {
	db := setupTestDB(t) // no route_mask column
	if st := db.computeRouteMaskBackfillStatus(); st.Status != "pending" || st.Remaining != nil {
		t.Fatalf("old schema: %+v, want pending with unknown remaining", st)
	}
}

func TestHealthz_ReportsRouteMaskBackfill(t *testing.T) {
	db := routeMaskServerDB(t)
	rmSeed(t, db)
	db.conn.Exec(dbschema.CreateRouteMaskPendingIndexSQL)
	srv := NewServer(db, &Config{Port: 3000}, NewHub())
	srv.store = NewPacketStore(db, nil)
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	prev := readiness.Load()
	readiness.Store(1)
	defer readiness.Store(prev)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("healthz status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		RouteMask *RouteMaskBackfillStatus `json:"route_mask_backfill"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RouteMask == nil || body.RouteMask.Status != "pending" || body.RouteMask.Remaining == nil || *body.RouteMask.Remaining != 1 {
		t.Fatalf("healthz route_mask_backfill = %+v, want pending remaining=1", body.RouteMask)
	}
}

// Payload Type Mix stays one combined ADVERT entry even when the masks mark
// transmissions as flood, zero-hop and mixed.
func TestPayloadTypeMix_AdvertStaysCombinedWithRouteMasks(t *testing.T) {
	store := relayAirtimeSplitStore(mixedAdvertFixtures())
	masks := []uint8{0b1010, 0b0010, 0b1000, 0b0001}
	i := 0
	for _, tx := range store.packets {
		if tx.PayloadType != nil && *tx.PayloadType == PayloadADVERT {
			tx.routeMask, tx.routeMaskKnown = masks[i%len(masks)], true
			i++
		}
	}
	encoded, err := json.Marshal(store.computeAnalyticsRF("", "", TimeWindow{})["payloadTypes"])
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Type  int    `json:"type"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(encoded, &entries); err != nil {
		t.Fatal(err)
	}
	var adverts int
	for _, e := range entries {
		if e.Type == PayloadADVERT {
			adverts++
			if e.Name != "ADVERT" || e.Count != 6 {
				t.Errorf("ADVERT entry = %+v, want name ADVERT count 6", e)
			}
		}
	}
	if adverts != 1 {
		t.Fatalf("Payload Type Mix has %d ADVERT entries, want exactly 1", adverts)
	}
}
