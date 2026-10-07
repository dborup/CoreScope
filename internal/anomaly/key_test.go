package anomaly

import (
	"strings"
	"testing"
)

func TestHopWidthIsPartOfTheValue(t *testing.T) {
	one := mustHop(t, 0x5C)
	two := mustHop(t, 0x5C, 0x12)
	three := mustHop(t, 0x5C, 0x12, 0x34)
	if one == two || two == three || one == three {
		t.Fatal("hops of different width must differ")
	}
	e1 := relayEvent(t, "a", t0, 0x11, 0x5C)
	e2 := relayEvent(t, "b", t0, 0x11, 0x5C, 0x12)
	e3 := relayEvent(t, "c", t0, 0x11, 0x5C, 0x12, 0x34)
	k1, k2, k3 := StreamKeyOf(e1), StreamKeyOf(e2), StreamKeyOf(e3)
	if k1 == k2 || k2 == k3 || k1 == k3 {
		t.Fatal("1-byte hop must not share a stream key with a wider hop that starts with the same byte")
	}
	for _, k := range []Key{k1, k2, k3} {
		if strings.Count(k.String(), "|") != 6 {
			t.Fatalf("encoding shape: %s", k)
		}
	}
	if k1.String() == k2.String() || strings.HasPrefix(k2.String(), k1.String()) {
		t.Fatalf("encodings collide or prefix: %q %q", k1, k2)
	}
	r1, _ := RouteGroupKeyOf(e1)
	r2, _ := RouteGroupKeyOf(e2)
	if r1 == r2 {
		t.Fatal("route-group keys must keep the hop width")
	}
}

func TestKeyEncodingRoundTrip(t *testing.T) {
	e := relayEvent(t, "a", t0, 0xAB, 0x01, 0x02)
	nopath := Event{ID: "n", Time: t0, PayloadType: 5, Channel: mustChan(t, 0xAB), RouteClass: RouteClassFlood, RouteKind: RouteKindNoPath}
	direct := Event{ID: "d", Time: t0, PayloadType: 2, RouteClass: RouteClassDirect, RouteKind: RouteKindDirect}
	rg, _ := RouteGroupKeyOf(e)
	keys := []Key{StreamKeyOf(e), ChannelKeyOf(e), rg, GlobalKeyOf(e), StreamKeyOf(nopath), StreamKeyOf(direct), ChannelKeyOf(direct)}
	seen := map[string]bool{}
	for _, k := range keys {
		if err := k.Validate(); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		s := k.String()
		if seen[s] {
			t.Fatalf("duplicate encoding %s", s)
		}
		seen[s] = true
		back, err := ParseKey(s)
		if err != nil || back != k {
			t.Fatalf("round trip %s: %v", s, err)
		}
	}
	want := "v1|s=stream|pt=5|ch=1:ab|rc=flood|rk=relay|hop=2:0102"
	if got := StreamKeyOf(e).String(); got != want {
		t.Fatalf("encoding changed: %s", got)
	}
}

func TestParseKeyRejectsNonCanonical(t *testing.T) {
	good := "v1|s=stream|pt=5|ch=1:ab|rc=flood|rk=relay|hop=1:5c"
	if _, err := ParseKey(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"v1|s=stream|pt=5|ch=1:AB|rc=flood|rk=relay|hop=1:5c",  // upper-case hex
		"v1|s=stream|pt=05|ch=1:ab|rc=flood|rk=relay|hop=1:5c", // leading zero
		"v1|s=stream|pt=5|ch=2:ab|rc=flood|rk=relay|hop=1:5c",  // length mismatch
		"v1|s=stream|pt=5|ch=1:ab|rc=flood|rk=relay|hop=-",     // relay without hop
		"v1|s=stream|pt=5|ch=1:ab|rc=flood|rk=nopath|hop=1:5c", // hop on no-path
		"v1|s=global|pt=5|ch=1:ab|rc=unknown|rk=unknown|hop=-", // global with channel
		"v1|s=stream|pt=16|ch=1:ab|rc=flood|rk=relay|hop=1:5c", // pt out of range
		"v1|s=stream|pt=5|ch=1:ab|rk=relay|rc=flood|hop=1:5c",  // field order
		"v2|s=stream|pt=5|ch=1:ab|rc=flood|rk=relay|hop=1:5c",
		"v1|s=stream|pt=5|ch=1:ab|rc=flood|rk=relay|hop=4:5c000000",
		"",
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("accepted non-canonical %q", bad)
		}
	}
}

func TestFullRouteDoesNotFragmentStreamKey(t *testing.T) {
	a := relayEvent(t, "a", t0, 0x11, 0x22)
	b := a
	b.ID = "b"
	b.FullRoute = mustRoute(t, 1, 0x22, 0x33, 0x44, 0x55)
	if StreamKeyOf(a) != StreamKeyOf(b) {
		t.Fatal("different full routes with the same first hop must share the stream key")
	}
	if a.FullRoute.equal(b.FullRoute) {
		t.Fatal("fixture: full routes should differ")
	}
}

func TestNoPathAndDirectAreLowConfidence(t *testing.T) {
	nopath := Event{ID: "n", Time: t0, PayloadType: 5, Channel: mustChan(t, 0x11), RouteClass: RouteClassFlood, RouteKind: RouteKindNoPath}
	direct := Event{ID: "d", Time: t0, PayloadType: 5, Channel: mustChan(t, 0x11), RouteClass: RouteClassDirect, RouteKind: RouteKindDirect}
	for _, e := range []Event{nopath, direct} {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		if c := StreamKeyOf(e).confidence(); c != ConfidenceLow {
			t.Fatalf("%v: confidence %v, want low", e.RouteKind, c)
		}
		if _, ok := RouteGroupKeyOf(e); ok {
			t.Fatalf("%v must not form a route group", e.RouteKind)
		}
	}
	if c := StreamKeyOf(relayEvent(t, "r", t0, 0x11, 0x22)).confidence(); c != ConfidenceRouteObserved {
		t.Fatalf("relay confidence %v", c)
	}
	mixed := relayEvent(t, "m", t0, 0x11, 0x22)
	mixed.RouteClass = RouteClassMixed
	if c := StreamKeyOf(mixed).confidence(); c != ConfidenceLow {
		t.Fatalf("mixed route class must be low confidence, got %v", c)
	}
}

func TestGlobalKeyCarriesNoAttribution(t *testing.T) {
	k := GlobalKeyOf(relayEvent(t, "a", t0, 0x11, 0x22))
	if !k.Channel.IsZero() || !k.FirstHop.IsZero() || k.RouteKind != 0 || k.RouteClass != 0 {
		t.Fatalf("global key carries attribution: %s", k)
	}
	if k.confidence() != ConfidenceAggregate {
		t.Fatal("global key must be aggregate confidence")
	}
}

func TestChannelHashIsOpaqueAndNoChannelDiffersFromZeroByte(t *testing.T) {
	zero := mustChan(t, 0x00)
	if zero.IsZero() {
		t.Fatal("a 1-byte hash 0x00 is a channel, not 'no channel'")
	}
	e := Event{ID: "x", Time: t0, PayloadType: 2, RouteClass: RouteClassDirect, RouteKind: RouteKindDirect}
	f := e
	f.ID, f.Channel = "y", zero
	if ChannelKeyOf(e) == ChannelKeyOf(f) {
		t.Fatal("no-channel and channel 0x00 must differ")
	}
}
