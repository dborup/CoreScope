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
// Six observers, one per source/edge-case. Coordinates are deliberately
// synthetic round-number values (10.0001, 20.0001, ...), never a real
// place, per the #381 review round 2 privacy nit:
//   - obsReal381 has a real `nodes` row AND an IATA code that is NOT in the
//     static iataCoords table. Before the fix, the unmapped code meant Lat/
//     Lon stayed nil even though a real position was available -- this is
//     the case this test is red for before the fix and green after.
//   - obsIataOnly381 has no `nodes` row but a mapped IATA code -- the
//     existing fallback, must keep working unchanged.
//   - obsNone381 has neither a `nodes` row nor a usable IATA code -- Lat/Lon
//     must stay nil, never fabricated.
//   - obsBoth381 has BOTH a real `nodes` row AND a *mapped* IATA code
//     (review round 2, finding 1): the real position must win. A swap
//     mutant that checks iataCoords before the real position turns this
//     case's assertions red.
//   - obsZeroZero381 has a `nodes` row sitting at the (0,0) sentinel AND a
//     mapped IATA code (review round 2, finding 2): (0,0) must not count
//     as "real" -- it must fall back to the IATA centroid.
//   - obsInvalidRange381 has a `nodes` row with an out-of-range latitude
//     (95, outside ±90) and no mapped IATA code (review round 2, finding
//     2): the invalid value must not count as "real" -- it must fall
//     through to none, not be fabricated into the response.
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
	insertNode := func(pubkey string, lat, lon float64) {
		if _, err := srv.db.conn.Exec(
			`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?,?,?,?,?,?,?)`,
			pubkey, "Node "+pubkey, "repeater", lat, lon, now, now,
		); err != nil {
			t.Fatalf("insert node %s: %v", pubkey, err)
		}
	}
	const realPubkey = "obsreal381pubkey"
	insertNode(realPubkey, 10.0001, 20.0001)
	const bothPubkey = "obsboth381pubkey"
	insertNode(bothPubkey, 30.0001, 40.0001)
	const zeroPubkey = "obszero381pubkey"
	insertNode(zeroPubkey, 0, 0)
	const invalidPubkey = "obsinvalid381pubkey"
	insertNode(invalidPubkey, 95.0, 200.0) // out of range: |lat|>90, |lon|>180

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
	bothIdx := insertObserver(bothPubkey, "BothObs", "SEA")
	zeroIdx := insertObserver(zeroPubkey, "ZeroZeroObs", "CPH")
	invalidIdx := insertObserver(invalidPubkey, "InvalidRangeObs", "ZZZ")

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
	tx4 := insertTx("wd381-4")
	tx5 := insertTx("wd381-5")
	tx6 := insertTx("wd381-6")
	insertObs(tx1, realIdx)
	insertObs(tx2, iataIdx)
	insertObs(tx3, noneIdx)
	insertObs(tx4, bothIdx)
	insertObs(tx5, zeroIdx)
	insertObs(tx6, invalidIdx)

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
	if len(byName) != 6 {
		t.Fatalf("Observers = %+v, want 6 distinct observers", resp.Observers)
	}

	real, ok := byName["RealObs"]
	if !ok {
		t.Fatal("RealObs missing from observer coverage")
	}
	if real.Lat == nil || real.Lon == nil {
		t.Fatal("RealObs has a real node position and an unmapped IATA code -- it must still get placed on the map (#381)")
	}
	if *real.Lat != 10.0001 || *real.Lon != 20.0001 {
		t.Errorf("RealObs lat/lon = %v,%v, want the real node position 10.0001,20.0001 (not an IATA centroid)", *real.Lat, *real.Lon)
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

	// Review round 2, finding 1: real position must win even when a
	// *mapped* IATA code is also present. A swap mutant (check iataCoords
	// before positions) makes this fail because it would report the SEA
	// centroid instead.
	both, ok := byName["BothObs"]
	if !ok {
		t.Fatal("BothObs missing from observer coverage")
	}
	if both.Lat == nil || both.Lon == nil {
		t.Fatal("BothObs has a real position -- must not be left unplaced")
	}
	if *both.Lat != 30.0001 || *both.Lon != 40.0001 {
		t.Errorf("BothObs lat/lon = %v,%v, want the real node position 30.0001,40.0001 -- real must win over a mapped IATA code (SEA)", *both.Lat, *both.Lon)
	}
	if both.PositionSource != PositionSourceReal {
		t.Errorf("BothObs PositionSource = %q, want %q (real must win over a mapped IATA code)", both.PositionSource, PositionSourceReal)
	}

	// Review round 2, finding 2: a (0,0) node position is a "no fix"
	// sentinel, not a real position -- it must fall back to the mapped
	// IATA centroid (CPH), not be reported as real at 0,0.
	zeroZero, ok := byName["ZeroZeroObs"]
	if !ok {
		t.Fatal("ZeroZeroObs missing from observer coverage")
	}
	wantCPH := iataCoords["CPH"]
	if zeroZero.Lat == nil || zeroZero.Lon == nil || *zeroZero.Lat != wantCPH.Lat || *zeroZero.Lon != wantCPH.Lon {
		t.Errorf("ZeroZeroObs lat/lon = %v,%v, want the CPH centroid %v,%v -- (0,0) must not be treated as a real position", zeroZero.Lat, zeroZero.Lon, wantCPH.Lat, wantCPH.Lon)
	}
	if zeroZero.PositionSource != PositionSourceIATA {
		t.Errorf("ZeroZeroObs PositionSource = %q, want %q -- (0,0) must not count as real", zeroZero.PositionSource, PositionSourceIATA)
	}

	// Review round 2, finding 2: an out-of-range node position (lat=95,
	// lon=200) must not be treated as real either, and with no mapped IATA
	// code to fall back to, must stay none -- never fabricated.
	invalidRange, ok := byName["InvalidRangeObs"]
	if !ok {
		t.Fatal("InvalidRangeObs missing from observer coverage")
	}
	if invalidRange.Lat != nil || invalidRange.Lon != nil {
		t.Errorf("InvalidRangeObs lat/lon = %v,%v, want nil,nil -- an out-of-range position must not be fabricated into the response", invalidRange.Lat, invalidRange.Lon)
	}
	if invalidRange.PositionSource != PositionSourceNone {
		t.Errorf("InvalidRangeObs PositionSource = %q, want %q -- out-of-range coordinates must not count as real", invalidRange.PositionSource, PositionSourceNone)
	}
}
