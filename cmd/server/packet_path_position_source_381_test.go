package main

import (
	"testing"
	"time"
)

// TestPacketPathObserver_PositionSource covers the packet-path/hop
// disambiguation consumer audited for issue #381 (upstream #2098).
// buildPacketPathResponseFromReduction already preferred the observer's real
// node position over its IATA code internally (obs.iataFallback), but never
// exposed that distinction on the wire -- API/UI consumers could not tell a
// real fix from an airport centroid. This checks the now-exported
// PositionSource field for all three cases: real position, IATA-only
// fallback, and neither.
func TestPacketPathObserver_PositionSource(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)

	insertTx := func(hash string) int64 {
		res, err := srv.db.conn.Exec(
			`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AA', ?, ?, 1, 5, '{}')`,
			hash, now,
		)
		if err != nil {
			t.Fatalf("insert tx %s: %v", hash, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	insertObserver := func(id, name, iata string) {
		if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?,?,?)`, id, name, iata); err != nil {
			t.Fatalf("insert observer %s: %v", id, err)
		}
	}
	insertObs := func(txID int64, observerID string, ts int64) {
		var observerIdx int64
		if err := srv.db.conn.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, observerID).Scan(&observerIdx); err != nil {
			t.Fatalf("lookup observer %s: %v", observerID, err)
		}
		if _, err := srv.db.conn.Exec(
			`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp) VALUES (?,?,?,?,?,?,?)`,
			txID, observerIdx, 5.0, -80.0, `[]`, `[]`, ts,
		); err != nil {
			t.Fatalf("insert observation: %v", err)
		}
	}

	ts := time.Now().UTC().Add(-time.Hour).Unix()

	// Case 1: real node position, IATA code deliberately absent from the
	// static table -- the position must still come from the real fix.
	const realPubkey = "obspathreal381"
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, lat, lon) VALUES (?,?,?,?)`, realPubkey, "RealPathObs", 55.4, 10.4); err != nil {
		t.Fatal(err)
	}
	insertObserver(realPubkey, "RealPathObs", "ZXY")
	txReal := insertTx("pp381real000001")
	insertObs(txReal, realPubkey, ts)

	// Case 2: no node position, mapped IATA code.
	insertObserver("obspathiata381", "IataPathObs", "AAR")
	txIata := insertTx("pp381iata000001")
	insertObs(txIata, "obspathiata381", ts)

	// Case 3: no node position, unmapped IATA code -- neither source.
	insertObserver("obspathnone381", "NonePathObs", "ZZZ")
	txNone := insertTx("pp381none000001")
	insertObs(txNone, "obspathnone381", ts)

	check := func(t *testing.T, hash string, wantSource PositionSource, wantLat, wantLon *float64) {
		t.Helper()
		path, err := srv.db.testPacketPath(hash, EstimateMaxEdgeKm)
		if err != nil {
			t.Fatalf("testPacketPath(%s): %v", hash, err)
		}
		if path == nil || path.First == nil || path.First.Observer == nil {
			t.Fatalf("testPacketPath(%s): no observer in response: %+v", hash, path)
		}
		obs := path.First.Observer
		if obs.PositionSource != wantSource {
			t.Errorf("%s: PositionSource = %q, want %q", hash, obs.PositionSource, wantSource)
		}
		if wantLat == nil {
			if obs.Lat != nil || obs.Lon != nil {
				t.Errorf("%s: Lat/Lon = %v,%v, want nil,nil", hash, obs.Lat, obs.Lon)
			}
			return
		}
		if obs.Lat == nil || obs.Lon == nil || *obs.Lat != *wantLat || *obs.Lon != *wantLon {
			t.Errorf("%s: Lat/Lon = %v,%v, want %v,%v", hash, obs.Lat, obs.Lon, *wantLat, *wantLon)
		}
	}

	realLat, realLon := 55.4, 10.4
	check(t, "pp381real000001", PositionSourceReal, &realLat, &realLon)

	wantAAR := iataCoords["AAR"]
	check(t, "pp381iata000001", PositionSourceIATA, &wantAAR.Lat, &wantAAR.Lon)

	check(t, "pp381none000001", PositionSourceNone, nil, nil)
}
