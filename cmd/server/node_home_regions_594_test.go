package main

import (
	"fmt"
	"reflect"
	"testing"
)

// #594 (port of upstream Kpa-clawbot/CoreScope#2234): computeNodeHomeRegions
// counts adverts heard zero-hop. Transport routes carry 4 transport-code
// bytes before path_length (firmware/docs/packet_format.md; src/Packet.cpp
// Packet::readFrom reads path_len after them when hasTransportCodes(),
// src/Packet.h routes 0x00/0x03), so a transport frame's path byte is at
// offset 5, not 1. One golden advert per route type, each heard by its own
// observer region so a wrong offset shows up as a missing or extra pubkey.
//
// #488 M1 classifies each observation by its own frame: zero-hop FLOOD and
// TRANSPORT_FLOOD adverts count, and a TRANSPORT_DIRECT advert does not (the
// only firmware sender is a contact re-share, BaseChatMesh::shareContactZeroHop;
// other codes have no sender, so neither proves the originator's position).
func TestNodeHomeRegionsReadTransportDirectPathByte(t *testing.T) {
	store := NewPacketStore(nil, nil)
	pt := PayloadADVERT
	add := func(id int, raw, pk, observer string) {
		store.packets = append(store.packets, &StoreTx{
			ID:           id,
			RawHex:       raw,
			PayloadType:  &pt,
			DecodedJSON:  `{"type":"ADVERT","pubKey":"` + pk + `"}`,
			Observations: []*StoreObs{{ObserverID: observer, advertEv: advertEvidenceFromRawHex(raw)}},
		})
	}
	// Header 0x10|route: payload type 4 (ADVERT) in bits 2-5.
	// Route 0, zero hops: path byte 0x00 at offset 5; the first code byte
	// 0xAA at offset 1 is 42 three-byte hops, over MAX_PATH_SIZE.
	add(1, "10AABBCCDD00DEADBEEF", "pk_transport_flood", "obs_tf")
	add(2, "1100DEADBEEF", "pk_flood", "obs_f")             // route 1: zero-hop flood
	add(3, "1200DEADBEEF", "pk_direct", "obs_d")            // route 2: zero hops at offset 1
	add(4, "1241EEDEADBEEF", "pk_direct_relayed", "obs_dr") // route 2: one hop at offset 1
	// Route 3, zero hops, codes non-zero: no firmware sender, no evidence.
	add(5, "13AABBCCDD00DEADBEEF", "pk_transport_direct", "obs_td")
	// Route 3, one 2-byte hop: path byte 0x41 at offset 5, first code byte
	// 0x00 at offset 1.
	add(6, "1300445566"+"41"+"EEFF"+"DEADBEEF", "pk_transport_direct_relayed", "obs_tdr")

	got := store.computeNodeHomeRegions(map[string]string{
		"obs_tf": "TFL", "obs_f": "FLD", "obs_d": "DIR", "obs_dr": "DRL", "obs_td": "TDR", "obs_tdr": "TDX",
	})
	want := map[string]string{"pk_transport_flood": "TFL", "pk_flood": "FLD", "pk_direct": "DIR"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("home regions = %v, want %v", got, want)
	}
}

// BenchmarkComputeNodeHomeRegions594 runs computeNodeHomeRegions over 30,000
// adverts (all four route types, two observations each) to show reading the
// path byte through packetpath.PathByte adds no cost or allocations. Since
// #488 M1 each observation carries its own frame's class (advertEv), set
// here as the store's load paths set it.
func BenchmarkComputeNodeHomeRegions594(b *testing.B) {
	store := NewPacketStore(nil, nil)
	pt := PayloadADVERT
	frames := []string{"10AABBCCDD00DEADBEEF", "1100DEADBEEF", "1200DEADBEEF", "1300BBCCDD00DEADBEEF"}
	obsIATA := map[string]string{"obs_a": "AAA", "obs_b": "BBB"}
	for i := 0; i < 30000; i++ {
		store.packets = append(store.packets, &StoreTx{
			ID:          i,
			RawHex:      frames[i%len(frames)],
			PayloadType: &pt,
			DecodedJSON: fmt.Sprintf(`{"type":"ADVERT","pubKey":"pk%04d"}`, i%2000),
			Observations: []*StoreObs{
				{ObserverID: "obs_a", advertEv: advertEvidenceFromRawHex(frames[i%len(frames)])},
				{ObserverID: "obs_b", advertEv: advertEvidenceFromRawHex(frames[i%len(frames)])},
			},
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.computeNodeHomeRegions(obsIATA)
	}
}
