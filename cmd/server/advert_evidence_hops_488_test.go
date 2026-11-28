package main

import (
	"strings"
	"testing"

	"github.com/meshcore-analyzer/packetpath"
)

// TestAdvertHearingKeepsRelayHops_488 (#488 M2, review N2 on #609): an
// observation's stored frame class must keep a relayed advert's hop count,
// which the capped fewest-hops fallback reads. M1 kept only the kind, so a
// relayed hearing came back as Hops 0, the same as a direct one.
func TestAdvertHearingKeepsRelayHops_488(t *testing.T) {
	const payload = "aabbccddeeff0011"
	for _, tc := range []struct {
		name string
		raw  string
		want packetpath.AdvertHearing
	}{
		{"flood, 1 hop", "1101a1" + payload, packetpath.AdvertHearing{Kind: packetpath.AdvertRelayed, Hops: 1, Verified: true}},
		{"flood, 3 hops", "1103a1a2a3" + payload, packetpath.AdvertHearing{Kind: packetpath.AdvertRelayed, Hops: 3, Verified: true}},
		{"transport flood, 2 hops of 2 bytes", "101a2b3c4d42a1a1a2a2" + payload, packetpath.AdvertHearing{Kind: packetpath.AdvertRelayed, Hops: 2, Verified: true}},
		{"flood, 63 hops (the most a path byte holds)", "113f" + strings.Repeat("a1", 63) + payload, packetpath.AdvertHearing{Kind: packetpath.AdvertRelayed, Hops: 63, Verified: true}},
		{"flood, zero hops", "1100" + payload, packetpath.AdvertHearing{Kind: packetpath.AdvertDirect, Verified: true}},
		{"direct with a path stays unknown", "1202a1a2" + payload, packetpath.AdvertHearing{Kind: packetpath.AdvertUnknown, Verified: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The hearing from the stored class equals classifying the frame.
			if direct := packetpath.ClassifyAdvertFrame(tc.raw); direct != tc.want {
				t.Fatalf("ClassifyAdvertFrame = %+v, want %+v", direct, tc.want)
			}
			o := &StoreObs{advertEv: advertEvidenceFromRawHex(tc.raw), PathJSON: "[]"}
			if got := o.advertHearing(); got != tc.want {
				t.Errorf("advertHearing() = %+v, want %+v", got, tc.want)
			}
		})
	}
	// A legacy row (no frame) still reads its path_json, unverified.
	o := &StoreObs{PathJSON: `["A1","B2"]`}
	if got, want := o.advertHearing(), (packetpath.AdvertHearing{Kind: packetpath.AdvertRelayed, Hops: 2}); got != want {
		t.Errorf("legacy advertHearing() = %+v, want %+v", got, want)
	}
}
