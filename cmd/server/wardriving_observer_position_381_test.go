package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHandleWardrivingStats_PositionSource covers issue #381 (upstream
// #2098): GetWardrivingStats must prefer an observer's real node position
// over its static IATA centroid, and must never fabricate a position when
// neither source is available.
//
// Three observers, one per source:
//   - obsReal381 has a real `nodes` row AND an IATA code that is NOT in the
//     static iataCoords table. Before the fix, the unmapped code meant Lat/
//     Lon stayed nil even though a real position was available -- this is
//     the case this test is red for before the fix and green after.
//   - obsIataOnly381 has no `nodes` row but a mapped IATA code -- the
//     existing fallback, must keep working unchanged.
//   - obsNone381 has neither a `nodes` row nor a usable IATA code -- Lat/Lon
//     must stay nil, never fabricated.
func TestHandleWardrivingStats_PositionSource(t *testing.T) {
	srv, router := setupTestServer(t)
	if _, err := srv.db.conn.Exec(`DELETE FROM transmissions`); err != nil {
		t.Fatalf("clear transmissions: %v", err)
	}
	if _, err := srv.db.conn.Exec(`DELETE FROM observations`); err != nil {
		t.Fatalf("clear observations: %v", err)
	}
	if _, err := srv.db.conn.Exec(`DELETE FROM observers`); err != nil {
		t.Fatalf("clear observers: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	insertTx := func(hash string) int64 {
		res, err := srv.db.conn.Exec(
			`INSERT INTO transmissions (raw_hex,hash,first_seen,route_type,payload_type,channel_hash,decoded_json) VALUES (?,?,?,1,5,'#wardriving',?)`,
			"aa", hash, now, `{"sender":"Carol","text":"Carol: MM:xyz000"}`,
		)
		if err != nil {
			t.Fatalf("insert tx %s: %v", hash, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	// obsReal381: a real node position (pubkey matches observers.id) AND an
	// IATA code deliberately absent from the static iataCoords table --
	// same shape as the Danish/Swedish deployment's unmapped codes the
	// issue describes. ZXY is not a real airport code.
	// nodes.public_key is stored lowercase in production; both
	// GetNodeLocationsByKeys and GetNodePositionsByKeys rely on that and
	// lowercase their query args accordingly, so the fixture must match.
	const realPubkey = "obsreal381pubkey"
	if _, err := srv.db.conn.Exec(
		`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?,?,?,?,?,?,?)`,
		realPubkey, "Real Node", "repeater", 56.1629, 10.2039, now, now,
	); err != nil {
		t.Fatalf("insert real-position node: %v", err)
	}
	insertObserver := func(id, name, iata string) int64 {
		res, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?,?,?)`, id, name, iata)
		if err != nil {
			t.Fatalf("insert observer %s: %v", id, err)
		}
		rowid, _ := res.LastInsertId()
		return rowid
	}
	realIdx := insertObserver(realPubkey, "RealObs", "ZXY")
	iataIdx := insertObserver("obsIataOnly381", "IataOnlyObs", "AAR")
	noneIdx := insertObserver("obsNone381", "NoneObs", "ZZZ")

	insertObs := func(txID, observerIdx int64) {
		if _, err := srv.db.conn.Exec(
			`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?,?,?,?,?,?)`,
			txID, observerIdx, 1.0, -80.0, `[]`, time.Now().Unix(),
		); err != nil {
			t.Fatalf("insert observation: %v", err)
		}
	}
	tx1 := insertTx("wd381-1")
	tx2 := insertTx("wd381-2")
	tx3 := insertTx("wd381-3")
	insertObs(tx1, realIdx)
	insertObs(tx2, iataIdx)
	insertObs(tx3, noneIdx)

	req := httptest.NewRequest("GET", "/api/analytics/wardriving?window=24h", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp WardrivingStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}

	byName := map[string]WardrivingObserverCoverage{}
	for _, oc := range resp.Observers {
		byName[oc.ObserverName] = oc
	}
	if len(byName) != 3 {
		t.Fatalf("Observers = %+v, want 3 distinct observers", resp.Observers)
	}

	real, ok := byName["RealObs"]
	if !ok {
		t.Fatal("RealObs missing from observer coverage")
	}
	if real.Lat == nil || real.Lon == nil {
		t.Fatal("RealObs has a real node position and an unmapped IATA code -- it must still get placed on the map (#381)")
	}
	if *real.Lat != 56.1629 || *real.Lon != 10.2039 {
		t.Errorf("RealObs lat/lon = %v,%v, want the real node position 56.1629,10.2039 (not an IATA centroid)", *real.Lat, *real.Lon)
	}
	if real.PositionSource != PositionSourceReal {
		t.Errorf("RealObs PositionSource = %q, want %q", real.PositionSource, PositionSourceReal)
	}

	iataOnly, ok := byName["IataOnlyObs"]
	if !ok {
		t.Fatal("IataOnlyObs missing from observer coverage")
	}
	if iataOnly.Lat == nil || iataOnly.Lon == nil {
		t.Fatal("IataOnlyObs has a mapped IATA code and no real position -- the fallback must still place it")
	}
	wantAAR := iataCoords["AAR"]
	if *iataOnly.Lat != wantAAR.Lat || *iataOnly.Lon != wantAAR.Lon {
		t.Errorf("IataOnlyObs lat/lon = %v,%v, want the AAR centroid %v,%v", *iataOnly.Lat, *iataOnly.Lon, wantAAR.Lat, wantAAR.Lon)
	}
	if iataOnly.PositionSource != PositionSourceIATA {
		t.Errorf("IataOnlyObs PositionSource = %q, want %q", iataOnly.PositionSource, PositionSourceIATA)
	}

	none, ok := byName["NoneObs"]
	if !ok {
		t.Fatal("NoneObs missing from observer coverage")
	}
	if none.Lat != nil || none.Lon != nil {
		t.Errorf("NoneObs has no real position and an unmapped IATA code -- Lat/Lon must stay nil, not fabricated, got lat=%v lon=%v", none.Lat, none.Lon)
	}
	if none.PositionSource != PositionSourceNone {
		t.Errorf("NoneObs PositionSource = %q, want %q", none.PositionSource, PositionSourceNone)
	}
}
