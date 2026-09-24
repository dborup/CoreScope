package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dbschema"
)

// Issue #89 review: the ingestor backfills route_mask while the server is
// already running (both restart together on a deploy, and the backfill takes
// minutes). The server must pick the backfilled masks up without a restart,
// and must not report the backfill complete before its own view has them.

func rmIndexedDB(t *testing.T) *DB {
	t.Helper()
	db := routeMaskServerDB(t)
	rmSeed(t, db) // tx 3 "legacy": route_type 2, route_mask NULL
	if _, err := db.conn.Exec(dbschema.CreateRouteMaskPendingIndexSQL); err != nil {
		t.Fatal(err)
	}
	prev := routeMaskStatusTTL
	routeMaskStatusTTL = 0
	t.Cleanup(func() { routeMaskStatusTTL = prev })
	return db
}

func rmExec(t *testing.T, db *DB, q string, args ...interface{}) {
	t.Helper()
	if _, err := db.conn.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func rmAdvertRows(s *PacketStore) map[string]int {
	res := s.GetRelayAirtimeShareWithWindow(TimeWindow{})
	out := map[string]int{}
	for _, r := range res["rows"].([]map[string]interface{}) {
		if r["type"] == PayloadADVERT {
			out[r["payload_type"].(string)] = r["count"].(int)
		}
	}
	return out
}

func TestRouteMask_RunningServerPicksUpBackfilledMasks(t *testing.T) {
	db := rmIndexedDB(t)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got, want := rmAdvertRows(s), map[string]int{"ADVERT (mixed)": 1, "ADVERT (flood)": 1, "ADVERT (zero-hop)": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before the backfill: %v, want %v", got, want)
	}

	// The backfill fills the legacy row: its frames show flood and zero-hop.
	rmExec(t, db, `UPDATE transmissions SET route_mask = 6 WHERE id = 3`)
	if st := db.computeRouteMaskBackfillStatus(); st.Status != "complete" {
		t.Fatalf("database status = %+v, want complete", st)
	}
	if st := s.routeMaskBackfillStatus(); st.Status != "backfilling" || st.Remaining == nil || *st.Remaining != 1 {
		t.Fatalf("server status before it read the backfilled mask = %+v, want backfilling remaining=1", st)
	}

	if n := s.RefreshBackfilledRouteMasks(); n != 1 {
		t.Fatalf("RefreshBackfilledRouteMasks merged %d masks, want 1", n)
	}
	live := rmSnapshot(s)
	if live["legacy"] != (rmView{0b0110, true}) {
		t.Fatalf("legacy row after refresh = %v, want 0110 known", live["legacy"])
	}
	cold := NewPacketStore(db, nil)
	if err := cold.Load(); err != nil {
		t.Fatal(err)
	}
	if want := rmSnapshot(cold); !reflect.DeepEqual(live, want) {
		t.Fatalf("live %v != cold load %v", live, want)
	}
	if st := s.routeMaskBackfillStatus(); st.Status != "complete" || st.Remaining == nil || *st.Remaining != 0 {
		t.Fatalf("server status after refresh = %+v, want complete remaining=0", st)
	}
	// The cached Relay Airtime Share result is invalidated by the refresh.
	if got, want := rmAdvertRows(s), map[string]int{"ADVERT (mixed)": 2, "ADVERT (flood)": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the refresh: %v, want %v", got, want)
	}
	if n := s.RefreshBackfilledRouteMasks(); n != 0 {
		t.Fatalf("a second refresh merged %d masks, want 0", n)
	}
}

// The backfill fills NULL rows in ascending id order; the refresh follows it,
// keeps rows the backfill has not reached, and drops rows that are gone.
func TestRouteMask_RefreshFollowsTheBackfill(t *testing.T) {
	db := rmIndexedDB(t)
	fs := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	rmInsertTx(t, db, 5, "legacy-2", 1, nil, fs)
	rmInsertTx(t, db, 6, "legacy-gone", 1, nil, fs)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}

	s.RefreshBackfilledRouteMasks() // backfill not started: nothing to merge
	if got := s.routeMaskPendingLen(); got != 3 {
		t.Fatalf("pending = %d before the backfill, want 3", got)
	}

	rmExec(t, db, `UPDATE transmissions SET route_mask = 4 WHERE id = 3`)
	s.RefreshBackfilledRouteMasks()
	snap := rmSnapshot(s)
	if snap["legacy"] != (rmView{0b0100, true}) || snap["legacy-2"].Known {
		t.Fatalf("after the first batch: %v", snap)
	}
	if st := s.routeMaskBackfillStatus(); st.Status == "complete" {
		t.Fatalf("complete with NULL rows left: %+v", st)
	}

	// Retention removed tx 6 before the backfill reached it.
	rmExec(t, db, `DELETE FROM transmissions WHERE id = 6`)
	rmExec(t, db, `UPDATE transmissions SET route_mask = 2 WHERE id = 5`)
	s.RefreshBackfilledRouteMasks()
	if v := rmSnapshot(s)["legacy-2"]; v != (rmView{0b0010, true}) {
		t.Fatalf("legacy-2 = %v, want 0010 known", v)
	}
	if got := s.routeMaskPendingLen(); got != 0 {
		t.Fatalf("pending = %d after the backfill, want 0", got)
	}
	if st := s.routeMaskBackfillStatus(); st.Status != "complete" {
		t.Fatalf("status = %+v, want complete", st)
	}
}

// A NULL row that reaches the server through live ingest (written by an older
// ingestor after a rollback) is picked up once the backfill fills it.
func TestRouteMask_NullRowIngestedLiveIsPickedUp(t *testing.T) {
	db := rmIndexedDB(t)
	rmExec(t, db, `UPDATE transmissions SET route_mask = 4 WHERE id = 3`)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	rmInsertTx(t, db, 7, "rollback-null", 1, nil, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
	s.IngestNewFromDB(3, 100)
	if v := rmSnapshot(s)["rollback-null"]; v.Known {
		t.Fatalf("NULL row ingested as known: %v", v)
	}
	rmExec(t, db, `UPDATE transmissions SET route_mask = 2 WHERE id = 7`)
	if st := s.routeMaskBackfillStatus(); st.Status == "complete" {
		t.Fatalf("complete before the server read the mask: %+v", st)
	}
	s.RefreshBackfilledRouteMasks()
	if v := rmSnapshot(s)["rollback-null"]; v != (rmView{0b0010, true}) {
		t.Fatalf("rollback-null = %v, want 0010 known", v)
	}
}

func TestPoller_PicksUpBackfilledRouteMasks(t *testing.T) {
	db := rmIndexedDB(t)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poller := NewPoller(db, NewHub(), 20*time.Millisecond)
	poller.store = s
	go poller.Start()
	defer poller.Stop()
	rmExec(t, db, `UPDATE transmissions SET route_mask = 6 WHERE id = 3`)
	deadline := time.Now().Add(5 * time.Second)
	for rmSnapshot(s)["legacy"] != (rmView{0b0110, true}) {
		if time.Now().After(deadline) {
			t.Fatalf("poller did not pick up the backfilled mask: %v", rmSnapshot(s)["legacy"])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHealthz_RouteMaskBackfillWaitsForTheServer(t *testing.T) {
	db := rmIndexedDB(t)
	srv := NewServer(db, &Config{Port: 3000}, NewHub())
	srv.store = NewPacketStore(db, nil)
	if err := srv.store.Load(); err != nil {
		t.Fatal(err)
	}
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	prev := readiness.Load()
	readiness.Store(1)
	defer readiness.Store(prev)
	status := func() RouteMaskBackfillStatus {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/healthz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("healthz status %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			RouteMask *RouteMaskBackfillStatus `json:"route_mask_backfill"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.RouteMask == nil {
			t.Fatalf("healthz body %s: %v", w.Body.String(), err)
		}
		return *body.RouteMask
	}
	rmExec(t, db, `UPDATE transmissions SET route_mask = 6 WHERE id = 3`)
	if st := status(); st.Status != "backfilling" || st.Remaining == nil || *st.Remaining != 1 {
		t.Fatalf("healthz before the server caught up = %+v, want backfilling remaining=1", st)
	}
	srv.store.RefreshBackfilledRouteMasks()
	if st := status(); st.Status != "complete" {
		t.Fatalf("healthz after the refresh = %+v, want complete", st)
	}
}
