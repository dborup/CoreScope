package main

import (
	"strings"
	"testing"
)

// Issue #952: the observer ↔ path[last] edge is a neighbour edge only on a
// flood route. Flood forwarders append their own hash (MeshCore Mesh.cpp
// routeRecvPacket), so the last hop is the node the observer heard. A direct
// path is the remaining planned route, from which each forwarder strips
// itself (removeSelfFromPath), so its last hop is the route's far end. TRACE
// is sent direct. The route type is the observation's own
// (observationRouteType); an unknown one adds no edge, as in the ingestor's
// neighbour builder.

var (
	ng952Origin   = strings.Repeat("aa", 32)
	ng952Relay    = strings.Repeat("bb", 32)
	ng952Observer = strings.Repeat("cc", 32)
	ng952Nodes    = []nodeInfo{
		{PublicKey: ng952Origin, Role: "repeater"},
		{PublicKey: ng952Relay, Role: "repeater"},
		{PublicKey: ng952Observer, Role: "repeater"},
	}
)

// ng952RouteMask is a transmissions.route_mask with bit r set per route r.
func ng952RouteMask(routes ...int) uint8 {
	var m uint8
	for _, r := range routes {
		m |= 1 << uint(r)
	}
	return m
}

func ng952HasEdge(g *NeighborGraph, a, b string) bool {
	for _, e := range g.Neighbors(a) {
		if (e.NodeA == a && e.NodeB == b) || (e.NodeA == b && e.NodeB == a) {
			return true
		}
	}
	return false
}

func TestNeighborGraphObserverEdgeFloodOnly(t *testing.T) {
	type obsCase struct {
		name         string
		payloadType  int
		txRoute      *int
		routeMask    *uint8 // nil: route_mask not known
		wireRoute    *int   // nil: no frame stored for the observation
		wantObserver bool
	}
	mask := func(routes ...int) *uint8 { m := ng952RouteMask(routes...); return &m }
	cases := []obsCase{
		{name: "flood", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteFlood), wantObserver: true},
		{name: "transport flood", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteTransportFlood), wantObserver: true},
		{name: "direct", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteDirect)},
		{name: "transport direct", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteTransportDirect)},
		{name: "trace", payloadType: PayloadTRACE, txRoute: ngIntPtr(RouteDirect)},
		{name: "direct ack", payloadType: PayloadACK, txRoute: ngIntPtr(RouteDirect)},
		// The observation's own frame wins over the transmission's first
		// reception (#289/#308).
		{name: "own frame direct, tx flood", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteFlood),
			routeMask: mask(RouteFlood, RouteDirect), wireRoute: ngIntPtr(RouteDirect)},
		{name: "own frame flood, tx direct", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteDirect),
			routeMask: mask(RouteFlood, RouteDirect), wireRoute: ngIntPtr(RouteFlood), wantObserver: true},
		{name: "own frame transport flood, tx direct", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteDirect),
			wireRoute: ngIntPtr(RouteTransportFlood), wantObserver: true},
		// Unknown route type: no edge, as in the ingestor
		// (observationRouteType -1 is not a flood route).
		{name: "unknown: no route type", payloadType: PayloadTXT_MSG},
		{name: "unknown: flood and direct seen, no frame", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteFlood),
			routeMask: mask(RouteFlood, RouteDirect)},
		{name: "mask flood only, no frame", payloadType: PayloadTXT_MSG, txRoute: ngIntPtr(RouteFlood),
			routeMask: mask(RouteFlood), wantObserver: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := ngMakeObs(ng952Observer, `["bbbb"]`, nowStr, nil)
			if tc.wireRoute != nil {
				obs.wireRoute = uint8(*tc.wireRoute) + 1
			}
			tx := ngMakeTx(1, tc.payloadType, `{}`, []*StoreObs{obs})
			tx.RouteType = tc.txRoute
			if tc.routeMask != nil {
				tx.routeMask, tx.routeMaskKnown = *tc.routeMask, true
			}
			g := BuildFromStore(ngTestStore(ng952Nodes, []*StoreTx{tx}))
			if got := ng952HasEdge(g, ng952Observer, ng952Relay); got != tc.wantObserver {
				t.Fatalf("observer ↔ last hop edge = %v, want %v", got, tc.wantObserver)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.wantObserver]; len(g.AllEdges()) != want {
				t.Fatalf("got %d edges, want %d", len(g.AllEdges()), want)
			}
		})
	}
}

// A direct observation's last hop is the route's far end: with a multi-hop
// path it must add neither the far-end edge nor raise a real flood edge.
func TestNeighborGraphDirectDoesNotRaiseFloodEdge(t *testing.T) {
	far := strings.Repeat("dd", 32)
	nodes := append(append([]nodeInfo{}, ng952Nodes...), nodeInfo{PublicKey: far, Role: "repeater"})
	flood := ngMakeTx(1, PayloadTXT_MSG, `{}`, []*StoreObs{ngMakeObs(ng952Observer, `["dddd","bbbb"]`, nowStr, nil)})
	flood.RouteType = ngIntPtr(RouteFlood)
	direct := ngMakeTx(2, PayloadTXT_MSG, `{}`, []*StoreObs{
		ngMakeObs(ng952Observer, `["bbbb","dddd"]`, nowStr, nil),
		ngMakeObs(ng952Observer, `["dddd","bbbb"]`, nowStr, nil),
	})
	direct.RouteType = ngIntPtr(RouteDirect)
	g := BuildFromStore(ngTestStore(nodes, []*StoreTx{flood, direct}))
	if ng952HasEdge(g, ng952Observer, far) {
		t.Fatal("direct observation added observer ↔ far-end edge")
	}
	for _, e := range g.Neighbors(ng952Observer) {
		if e.Count != 1 {
			t.Fatalf("observer edge %s--%s count %d, want the flood observation only (1)", e.NodeA, e.NodeB, e.Count)
		}
	}
}

// The advert edges stay as they are: zero hop and originator ↔ path[0] do not
// depend on the route type (adverts are flooded).
func TestNeighborGraphAdvertEdgesIgnoreRouteType(t *testing.T) {
	for _, tc := range []struct {
		name    string
		txRoute *int
	}{
		{"unknown", nil},
		{"direct", ngIntPtr(RouteDirect)},
		{"flood", ngIntPtr(RouteFlood)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zeroHop := ngMakeTx(1, PayloadADVERT, ngFromNodeJSON(ng952Origin), []*StoreObs{ngMakeObs(ng952Observer, `[]`, nowStr, nil)})
			zeroHop.RouteType = tc.txRoute
			g := BuildFromStore(ngTestStore(ng952Nodes, []*StoreTx{zeroHop}))
			if !ng952HasEdge(g, ng952Origin, ng952Observer) {
				t.Fatal("zero-hop advert edge missing")
			}

			relayed := ngMakeTx(2, PayloadADVERT, ngFromNodeJSON(ng952Origin), []*StoreObs{ngMakeObs(ng952Observer, `["bbbb"]`, nowStr, nil)})
			relayed.RouteType = tc.txRoute
			g = BuildFromStore(ngTestStore(ng952Nodes, []*StoreTx{relayed}))
			if !ng952HasEdge(g, ng952Origin, ng952Relay) {
				t.Fatal("advert originator ↔ path[0] edge missing")
			}
			wantObserver := tc.txRoute != nil && *tc.txRoute == RouteFlood
			if got := ng952HasEdge(g, ng952Observer, ng952Relay); got != wantObserver {
				t.Fatalf("observer ↔ last hop edge = %v, want %v", got, wantObserver)
			}
		})
	}
}
