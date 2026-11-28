package main

import (
	"testing"
	"time"
)

// #632 (upstream 2266/2298): relay evidence must come from the observation
// that carries the hop. The content hash leaves the header route bits out
// (firmware Packet::calculatePacketHash), so one transmission can hold FLOOD
// and DIRECT receptions, and tx.RouteType is only the first one's. A FLOOD
// path lists hops that already forwarded (Mesh::routeRecvPacket appends
// self); a DIRECT path lists the hops still to come (Mesh::removeSelfFromPath).
//
// hbNode and hbTwin share the 1-byte prefix a1; only hbNode's 2-byte a1b2 is
// unique (heard_by_test.go fixtures).

type relayEvidence632 struct {
	confirmed   bool
	lastHeard   string // GetNodeHealth stats.lastHeard
	bulkHeard   string // GetBulkHealth stats.lastHeard
	lastRelayed string // GetRepeaterRelayInfo
	bulkRelayed string // computeRepeaterRelayInfoMap
}

func relayEvidenceFor632(t *testing.T, tx *StoreTx) relayEvidence632 {
	t.Helper()
	_, s := hbStore(t)
	hbIndex(s, hbNode, tx)
	_, pm := s.getCachedNodesAndPM()
	var got relayEvidence632
	got.confirmed = txHasConfirmedRelay(tx, hbNode, pm)
	health, err := s.GetNodeHealth(hbNode, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if p := health["stats"].(NodeHealthStats).LastHeard; p != nil {
		got.lastHeard = *p
	}
	for _, row := range s.GetBulkHealth(10, "", "") {
		if row["public_key"] == hbNode {
			if p := row["stats"].(NodeHealthStats).LastHeard; p != nil {
				got.bulkHeard = *p
			}
		}
	}
	got.lastRelayed = s.GetRepeaterRelayInfo(hbNode, 24).LastRelayed
	got.bulkRelayed = s.computeRepeaterRelayInfoMap(24)[hbNode].LastRelayed
	return got
}

func wantRelayEvidence632(t *testing.T, tx *StoreTx, credited bool) {
	t.Helper()
	got := relayEvidenceFor632(t, tx)
	want := relayEvidence632{}
	if credited {
		want = relayEvidence632{true, tx.FirstSeen, tx.FirstSeen, tx.FirstSeen, tx.FirstSeen}
	}
	if got != want {
		t.Fatalf("relay evidence = %+v, want %+v", got, want)
	}
}

// A silent repeater whose only recent appearances are a shared 1-byte FLOOD
// hop and a planned DIRECT hop must not look heard, whichever observation
// supplies the display path and the transmission's route type.
func TestRelayEvidence_PlannedDirectHopUnderFloodTx_632(t *testing.T) {
	for _, displayDirect := range []bool{false, true} {
		name := map[bool]string{false: "display=shared flood hop", true: "display=direct hop"}[displayDirect]
		t.Run(name, func(t *testing.T) {
			shared := hbObs("obs-flood", "Flood", `["C3D4","A1"]`, RouteFlood+1, 1, -99)
			direct := hbObs("obs-direct", "Direct", `["A1B2"]`, RouteDirect+1, 1, -99)
			tx := hbTx(PayloadTXT_MSG, RouteFlood, `{"srcPubKey":"`+hbOther+`"}`, shared, direct)
			if displayDirect {
				tx.PathJSON = direct.PathJSON
			}
			wantRelayEvidence632(t, tx, false)
		})
	}
	// Positive control: the same unique hop on a FLOOD reception is evidence.
	tx := hbTx(PayloadTXT_MSG, RouteFlood, `{"srcPubKey":"`+hbOther+`"}`,
		hbObs("obs-flood", "Flood", `["C3D4","A1"]`, RouteFlood+1, 1, -99),
		hbObs("obs-flood2", "Flood 2", `["A1B2"]`, RouteFlood+1, 1, -99))
	wantRelayEvidence632(t, tx, true)
}

// The converse: a transmission first ingested as DIRECT keeps a later FLOOD
// reception's observed hop as evidence.
func TestRelayEvidence_ObservedFloodHopUnderDirectTx_632(t *testing.T) {
	for _, route := range []int{RouteFlood, RouteTransportFlood} {
		tx := hbTx(PayloadTXT_MSG, RouteDirect, `{"srcPubKey":"`+hbOther+`"}`,
			hbObs("obs-direct", "Direct", `[]`, RouteDirect+1, 1, -99),
			hbObs("obs-flood", "Flood", `["A1B2"]`, uint8(route)+1, 1, -99))
		wantRelayEvidence632(t, tx, true)
	}
}

// Without a stored frame the observation's route follows the ingestor's #308
// rule (observationRouteType): unknown when the hash was heard on both route
// families, else the transmission's route.
func TestRelayEvidence_FramelessObservationRoute_632(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mask     uint8
		credited bool
	}{
		{"flood and direct seen", 0b0110, false},
		{"flood only", 0b0010, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := hbTx(PayloadTXT_MSG, RouteFlood, `{"srcPubKey":"`+hbOther+`"}`,
				hbObs("obs-x", "X", `["A1B2"]`, 0, 1, -99))
			tx.routeMask, tx.routeMaskKnown = tc.mask, true
			wantRelayEvidence632(t, tx, tc.credited)
		})
	}
}
