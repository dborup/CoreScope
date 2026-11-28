package main

import (
	"strings"
	"testing"
)

func TestNeighborGraphIgnoresTransmittedObservations(t *testing.T) {
	origin, relay, observer := strings.Repeat("aa", 32), strings.Repeat("bb", 32), strings.Repeat("cc", 32)
	nodes := []nodeInfo{
		{PublicKey: origin, Role: "repeater"},
		{PublicKey: relay, Role: "repeater"},
		{PublicKey: observer, Role: "repeater"},
	}
	for _, tc := range []struct {
		name, direction, path string
		wantEdges             int
	}{
		{"tx zero hop", "tx", `[]`, 0},
		{"tx forwarded", "tx", `["bbbb","cccc"]`, 0},
		{"rx", "rx", `["bbbb"]`, 2},
		{"legacy empty", "", `["bbbb"]`, 2},
		{"unknown direction unchanged", "unknown", `["bbbb"]`, 2},
		{"uppercase remains unchanged", "TX", `["bbbb"]`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := ngMakeObs(observer, tc.path, "2026-10-01T12:00:00Z", nil)
			obs.Direction = tc.direction
			tx := ngMakeTx(1, PayloadADVERT, ngFromNodeJSON(origin), []*StoreObs{obs})
			g := BuildFromStore(ngTestStore(nodes, []*StoreTx{tx}))
			if edges := g.AllEdges(); len(edges) != tc.wantEdges {
				t.Fatalf("direction %q: got %d edges, want %d", tc.direction, len(edges), tc.wantEdges)
			}
		})
	}
}

func TestNeighborGraphForwardedTxDoesNotDoubleCountRx(t *testing.T) {
	origin, relay, observer := strings.Repeat("aa", 32), strings.Repeat("bb", 32), strings.Repeat("cc", 32)
	nodes := []nodeInfo{
		{PublicKey: origin, Role: "repeater"},
		{PublicKey: relay, Role: "repeater"},
		{PublicKey: observer, Role: "repeater"},
	}
	rx := ngMakeObs(observer, `["bbbb"]`, "2026-10-01T12:00:00Z", ngFloatPtr(3))
	rx.Direction = "rx"
	txCopy := ngMakeObs(observer, `["bbbb","cccc"]`, "2026-10-01T12:00:01Z", nil)
	txCopy.Direction = "tx"
	tx := ngMakeTx(1, PayloadADVERT, ngFromNodeJSON(origin), []*StoreObs{rx, txCopy})
	g := BuildFromStore(ngTestStore(nodes, []*StoreTx{tx}))
	edges := g.AllEdges()
	if len(edges) != 2 {
		t.Fatalf("got %d edges, want the two edges from the RX copy", len(edges))
	}
	for _, e := range edges {
		if e.Count != 1 {
			t.Errorf("edge %s--%s: count %d, want 1 RX observation", e.NodeA, e.NodeB, e.Count)
		}
		if e.SNRCount != 1 || e.SNRSum != 3 {
			t.Errorf("edge signal = %v/%d, want RX-only 3/1", e.SNRSum, e.SNRCount)
		}
	}
}
