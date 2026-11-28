package main

import (
	"strings"
	"testing"
)

// The same ADVERT can reach the ingestor on both route classes: MeshCore's
// BaseChatMesh::shareContactZeroHop re-sends a stored advert unchanged as a
// TRANSPORT_DIRECT zero-hop packet. ComputeContentHash ignores the route bits,
// transport codes and path, so both variants share one transmissions row, and
// InsertTransmission never rewrites route_type or raw_hex for an existing hash.
// The stored route is therefore that of the first observation *inserted*, not
// the earliest received. Relay Airtime Share's ADVERT route classes are built
// on this contract; these tests pin it so any change to it is deliberate.

var firstIngestedAdvertPayload = "" +
	strings.Repeat("86", 32) + // public key
	"00e1f565" + // timestamp
	strings.Repeat("00", 64) + // signature (not validated here)
	"81" + "50523836" // app data: flags + name "PR86"

var (
	// ROUTE_TYPE_FLOOD (header 0x11 = ADVERT<<2 | 1), two 1-byte relay hops.
	firstIngestedFloodRaw = "11" + "02" + "a1b2" + firstIngestedAdvertPayload
	// ROUTE_TYPE_TRANSPORT_DIRECT (0x13), transport codes {0,0}, path_len 0:
	// exactly what shareContactZeroHop sends.
	firstIngestedZeroHopRaw = "13" + "00000000" + "00" + firstIngestedAdvertPayload
)

type advertObservation struct {
	raw, observer, rxTime string
}

func advertPacketData(t *testing.T, o advertObservation) *PacketData {
	t.Helper()
	decoded, err := DecodePacket(o.raw, nil, false)
	if err != nil {
		t.Fatalf("decode %s: %v", o.raw[:10], err)
	}
	pd := BuildPacketData(&MQTTPacketMessage{Raw: o.raw}, decoded, o.observer, "", nil)
	pd.Timestamp = o.rxTime
	return pd
}

type storedAdvert struct {
	rows         int
	routeType    int
	rawHex       string
	firstSeen    string
	observations int
}

func ingestAdverts(t *testing.T, obs ...advertObservation) (storedAdvert, string) {
	t.Helper()
	s := newTestStore(t)
	var hash string
	for i, o := range obs {
		pd := advertPacketData(t, o)
		if pd.PayloadType != PayloadADVERT {
			t.Fatalf("observation %d decoded as payload %d, want ADVERT", i, pd.PayloadType)
		}
		if i == 0 {
			hash = pd.Hash
		} else if pd.Hash != hash {
			t.Fatalf("observation %d hash %s differs from %s; route variants must share a content hash", i, pd.Hash, hash)
		}
		if _, err := s.InsertTransmission(pd); err != nil {
			t.Fatalf("insert observation %d: %v", i, err)
		}
	}
	var got storedAdvert
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(route_type), MAX(raw_hex), MAX(first_seen) FROM transmissions WHERE hash = ?`, hash).
		Scan(&got.rows, &got.routeType, &got.rawHex, &got.firstSeen); err != nil {
		t.Fatalf("query transmissions: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations o JOIN transmissions t ON t.id = o.transmission_id WHERE t.hash = ?`, hash).
		Scan(&got.observations); err != nil {
		t.Fatalf("query observations: %v", err)
	}
	return got, hash
}

func TestAdvertRouteVariantsShareContentHash(t *testing.T) {
	flood := advertPacketData(t, advertObservation{firstIngestedFloodRaw, "obs-a", "2026-09-23T10:00:00Z"})
	zeroHop := advertPacketData(t, advertObservation{firstIngestedZeroHopRaw, "obs-b", "2026-09-23T10:05:00Z"})
	if flood.RouteType != RouteFlood || zeroHop.RouteType != RouteTransportDirect {
		t.Fatalf("route types = %d/%d, want %d/%d", flood.RouteType, zeroHop.RouteType, RouteFlood, RouteTransportDirect)
	}
	if flood.Hash != zeroHop.Hash {
		t.Fatalf("hashes differ: flood %s, zero-hop %s", flood.Hash, zeroHop.Hash)
	}
}

func TestInsertTransmission_AdvertRouteIsFirstIngested(t *testing.T) {
	cases := []struct {
		name          string
		obs           []advertObservation
		wantRoute     int
		wantRaw       string
		wantFirstSeen string
	}{
		{
			name: "flood then zero-hop",
			obs: []advertObservation{
				{firstIngestedFloodRaw, "obs-a", "2026-09-23T10:00:00Z"},
				{firstIngestedZeroHopRaw, "obs-b", "2026-09-23T10:05:00Z"},
			},
			wantRoute: RouteFlood, wantRaw: firstIngestedFloodRaw, wantFirstSeen: "2026-09-23T10:00:00Z",
		},
		{
			name: "zero-hop then flood",
			obs: []advertObservation{
				{firstIngestedZeroHopRaw, "obs-b", "2026-09-23T10:00:00Z"},
				{firstIngestedFloodRaw, "obs-a", "2026-09-23T10:05:00Z"},
			},
			wantRoute: RouteTransportDirect, wantRaw: firstIngestedZeroHopRaw, wantFirstSeen: "2026-09-23T10:00:00Z",
		},
		{
			// A delayed zero-hop observation inserted first keeps its route even
			// though the flood observation was received earlier: first_seen moves
			// back, route_type and raw_hex do not.
			name: "zero-hop inserted first but received later",
			obs: []advertObservation{
				{firstIngestedZeroHopRaw, "obs-b", "2026-09-23T10:05:00Z"},
				{firstIngestedFloodRaw, "obs-a", "2026-09-23T10:00:00Z"},
			},
			wantRoute: RouteTransportDirect, wantRaw: firstIngestedZeroHopRaw, wantFirstSeen: "2026-09-23T10:00:00Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ingestAdverts(t, tc.obs...)
			if got.rows != 1 || got.observations != 2 {
				t.Fatalf("rows=%d observations=%d, want one transmission with two observations", got.rows, got.observations)
			}
			if got.routeType != tc.wantRoute {
				t.Errorf("route_type = %d, want %d (first inserted observation)", got.routeType, tc.wantRoute)
			}
			if got.rawHex != tc.wantRaw {
				t.Errorf("raw_hex header %s, want %s: raw_hex must stay consistent with route_type", got.rawHex[:2], tc.wantRaw[:2])
			}
			if got.firstSeen != tc.wantFirstSeen {
				t.Errorf("first_seen = %s, want %s (earliest rx time)", got.firstSeen, tc.wantFirstSeen)
			}
		})
	}
}
