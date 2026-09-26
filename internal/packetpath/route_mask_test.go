package packetpath

import "testing"

func TestRouteTypeFromHeader(t *testing.T) {
	cases := map[byte]int{
		0x10: RouteTransportFlood, // ADVERT, transport flood
		0x11: RouteFlood,          // ADVERT, flood
		0x12: RouteDirect,         // ADVERT, direct
		0x13: RouteTransportDirect,
		0x0c: RouteTransportFlood, // ACK
		0xff: RouteTransportDirect,
	}
	for header, want := range cases {
		if got := RouteTypeFromHeader(header); got != want {
			t.Errorf("RouteTypeFromHeader(0x%02x) = %d, want %d", header, got, want)
		}
	}
}

func TestRouteTypeFromRawHex(t *testing.T) {
	cases := []struct {
		raw    string
		want   int
		wantOK bool
	}{
		{"11", RouteFlood, true},
		{"1302aabb", RouteTransportDirect, true},
		{"1A00", RouteDirect, true}, // upper-case hex
		{"10", RouteTransportFlood, true},
		{"", 0, false},   // empty
		{"1", 0, false},  // truncated header byte
		{"zz", 0, false}, // not hex
		{"g1", 0, false},
	}
	for _, tc := range cases {
		got, ok := RouteTypeFromRawHex(tc.raw)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("RouteTypeFromRawHex(%q) = (%d, %v), want (%d, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestRouteMaskBit(t *testing.T) {
	want := map[int]int64{0: 1, 1: 2, 2: 4, 3: 8, -1: 0, 4: 0, 99: 0}
	for rt, bit := range want {
		if got := RouteMaskBit(rt); got != bit {
			t.Errorf("RouteMaskBit(%d) = %d, want %d", rt, got, bit)
		}
	}
	if RouteMaskFlood != RouteMaskBit(RouteTransportFlood)|RouteMaskBit(RouteFlood) {
		t.Errorf("RouteMaskFlood = %b, want routes 0 and 1", RouteMaskFlood)
	}
	if RouteMaskDirect != RouteMaskBit(RouteDirect)|RouteMaskBit(RouteTransportDirect) {
		t.Errorf("RouteMaskDirect = %b, want routes 2 and 3", RouteMaskDirect)
	}
	if RouteMaskFlood&RouteMaskDirect != 0 || RouteMaskFlood|RouteMaskDirect != RouteMaskAll {
		t.Errorf("flood %b and direct %b must partition all %b", RouteMaskFlood, RouteMaskDirect, RouteMaskAll)
	}
}
