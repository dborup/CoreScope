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
// PositionSource field for the base three cases (real position, IATA-only
// fallback, and neither) plus the #381 review round 2 edge cases:
//   - a real position AND a *mapped* IATA code together (finding 1) --
//     real must win, not just when the code is unmapped;
//   - an out-of-range pubkey-matched position with a mapped IATA code
//     (finding 2) -- the invalid value must not count as real;
//   - an out-of-range name-matched position (bridge-style observer whose
//     id isn't a pubkey) with no mapped IATA code (finding 3) -- same
//     rejection must apply to the name-fallback path, not just pubkey.
//
// Coordinates are deliberately synthetic (round values like 10.0002,
// 20.0002), never a real place.
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
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, lat, lon) VALUES (?,?,?,?)`, realPubkey, "RealPathObs", 10.0002, 20.0002); err != nil {
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

	// Case 4 (review round 2, finding 1): real position AND a *mapped*
	// IATA code together -- real must still win. A swap mutant that checks
	// IATA before the pubkey/name position lookups turns this red.
	const bothPubkey = "obspathboth381"
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, lat, lon) VALUES (?,?,?,?)`, bothPubkey, "BothPathObs", 30.0002, 40.0002); err != nil {
		t.Fatal(err)
	}
	insertObserver(bothPubkey, "BothPathObs", "SEA")
	txBoth := insertTx("pp381both000001")
	insertObs(txBoth, bothPubkey, ts)

	// Case 5 (review round 2, finding 2): pubkey-matched position is
	// out of range (lat=97, outside ±90) with a mapped IATA code -- the
	// invalid value must not count as real; must fall back to IATA.
	const invalidPubkey = "obspathinvalid381"
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, lat, lon) VALUES (?,?,?,?)`, invalidPubkey, "InvalidPathObs", 97.0, 20.0003); err != nil {
		t.Fatal(err)
	}
	insertObserver(invalidPubkey, "InvalidPathObs", "AAR")
	txInvalid := insertTx("pp381invalid00001")
	insertObs(txInvalid, invalidPubkey, ts)

	// Case 6 (review round 2, finding 3): name-matched position (the
	// bridge-device-name shape from TestGetPacketPath_ObserverPositionFallsBackToNameMatch)
	// is out of range (lon=-185, outside ±180) with no mapped IATA code --
	// must not count as real on the name-fallback path either, and must
	// stay none since nothing else is available.
	const invalidNameObserverID = "InvalidNamePathObs" // observers.id is the device name, not a pubkey
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, lat, lon) VALUES (?,?,?,?)`, "obspathinvalidname381pubkey", invalidNameObserverID, 10.0004, -185.0); err != nil {
		t.Fatal(err)
	}
	insertObserver(invalidNameObserverID, invalidNameObserverID, "ZZZ")
	txInvalidName := insertTx("pp381invname00001")
	insertObs(txInvalidName, invalidNameObserverID, ts)

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

	realLat, realLon := 10.0002, 20.0002
	check(t, "pp381real000001", PositionSourceReal, &realLat, &realLon)

	wantAAR := iataCoords["AAR"]
	check(t, "pp381iata000001", PositionSourceIATA, &wantAAR.Lat, &wantAAR.Lon)

	check(t, "pp381none000001", PositionSourceNone, nil, nil)

	bothLat, bothLon := 30.0002, 40.0002
	check(t, "pp381both000001", PositionSourceReal, &bothLat, &bothLon)

	check(t, "pp381invalid00001", PositionSourceIATA, &wantAAR.Lat, &wantAAR.Lon)

	check(t, "pp381invname00001", PositionSourceNone, nil, nil)
}
