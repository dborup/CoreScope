package packetpath

import "testing"

// Golden frames for #594 (port of upstream Kpa-clawbot/CoreScope#2234).
// firmware/docs/packet_format.md: [header][transport_codes(optional)]
// [path_length][path][payload], transport_codes "Only present for
// ROUTE_TYPE_TRANSPORT_FLOOD and ROUTE_TYPE_TRANSPORT_DIRECT" (4 bytes).
// firmware/src/Packet.cpp Packet::readFrom reads path_len after those 4 bytes
// when hasTransportCodes() (src/Packet.h: route 0x00 or 0x03). Each transport
// frame's first code byte differs from its path byte, so reading offset 1
// instead of 5 returns the wrong byte.
func TestPathByte(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		wantRoute int
		wantByte  byte
		wantOK    bool
	}{
		{"transport flood reads offset 5", "10AABBCCDD41EEDEADBEEF", RouteTransportFlood, 0x41, true},
		{"flood reads offset 1", "1141EEDEADBEEF", RouteFlood, 0x41, true},
		{"direct reads offset 1", "1200DEADBEEF", RouteDirect, 0x00, true},
		{"transport direct zero hop reads offset 5", "13AABBCCDD00DEADBEEF", RouteTransportDirect, 0x00, true},
		{"transport direct relayed reads offset 5", "1300445566C1EEDEADBEEF", RouteTransportDirect, 0xC1, true},
		{"lower-case hex", "13aabbccdd80deadbeef", RouteTransportDirect, 0x80, true},
		{"flood header only", "11", RouteFlood, 0, false},
		{"transport header without path byte", "13AABBCCDD", RouteTransportDirect, 0, false},
		{"odd length before path byte", "13AABBCCD", RouteTransportDirect, 0, false},
		{"non-hex path byte", "11ZZ", RouteFlood, 0, false},
		{"non-hex header", "G100", 0, 0, false},
		{"empty", "", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, b, ok := PathByte(tc.raw)
			if ok != tc.wantOK || b != tc.wantByte || (tc.wantOK && route != tc.wantRoute) {
				t.Errorf("PathByte(%q) = (%d, 0x%02X, %v), want (%d, 0x%02X, %v)",
					tc.raw, route, b, ok, tc.wantRoute, tc.wantByte, tc.wantOK)
			}
		})
	}
}

// PathByte runs once per advert on every analytics/home-region recompute, so
// it must not allocate (upstream measured 17k → 457k allocs/op without this).
func TestPathByteDoesNotAllocate(t *testing.T) {
	raw := "13AABBCCDD00DEADBEEF"
	if n := testing.AllocsPerRun(1000, func() { PathByte(raw) }); n != 0 {
		t.Fatalf("PathByte allocates %.0f times per call, want 0", n)
	}
}
