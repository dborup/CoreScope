package main

import (
	"testing"
	"time"
)

// TestMarkBridgeRepeaters_NilStore covers the graceful no-op when the
// in-memory store isn't available (s.store can legitimately be nil --
// see handlePacketPath). Must not panic and must leave every IsBridge
// at its zero value (false).
func TestMarkBridgeRepeaters_NilStore(t *testing.T) {
	resp := &PacketPathResponse{
		Hash: "deadbeef",
		Branches: []PacketPathBranch{
			{Points: []PacketPathPoint{{PublicKey: "pk1"}}, Observer: &PacketPathObserver{PublicKey: "obs1"}},
		},
	}
	markBridgeRepeaters(resp, nil, &Config{})
	if resp.Branches[0].Points[0].IsBridge {
		t.Errorf("Points[0].IsBridge = true, want false -- store is nil, nothing should be marked")
	}
	if resp.Branches[0].Observer.IsBridge {
		t.Errorf("Observer.IsBridge = true, want false -- store is nil, nothing should be marked")
	}
}

// TestMarkBridgeRepeaters_TwoScopesIsBridge covers the actual
// determination: a pubkey relaying 2+ distinct region scopes (the same
// definition/data source as the Foreign Traffic tab's "Bridge" badge,
// ScopeStatsResponse.bridgeRepeaters) gets IsBridge=true on every point
// and observer entry referencing it; a pubkey with 0 or 1 scope, or one
// missing from the relay map entirely, stays false.
func TestMarkBridgeRepeaters_TwoScopesIsBridge(t *testing.T) {
	store := &PacketStore{
		repeaterRelayCache: map[string]RepeaterRelayInfo{
			"bridgepk":       {TransportedScopes: []string{"#dk", "#se"}},
			"regionalpk":     {TransportedScopes: []string{"#dk"}},
			"unknownscopepk": {TransportedScopes: nil},
		},
		repeaterRelayCacheWin: 24,
		repeaterRelayAt:       time.Now(),
	}
	resp := &PacketPathResponse{
		Hash: "deadbeef",
		Branches: []PacketPathBranch{
			{
				Points: []PacketPathPoint{
					{PublicKey: "bridgepk"},
					{PublicKey: "regionalpk"},
					{PublicKey: "unknownscopepk"},
					{PublicKey: "notinrelaymap"},
				},
				Observer: &PacketPathObserver{PublicKey: "BridgePK"}, // case-insensitive match
			},
		},
	}
	markBridgeRepeaters(resp, store, &Config{})

	pts := resp.Branches[0].Points
	if !pts[0].IsBridge {
		t.Errorf("Points[0] (bridgepk, 2 scopes) IsBridge = false, want true")
	}
	if pts[1].IsBridge {
		t.Errorf("Points[1] (regionalpk, 1 scope) IsBridge = true, want false")
	}
	if pts[2].IsBridge {
		t.Errorf("Points[2] (unknownscopepk, 0 scopes) IsBridge = true, want false")
	}
	if pts[3].IsBridge {
		t.Errorf("Points[3] (notinrelaymap, absent from relay map) IsBridge = true, want false")
	}
	if !resp.Branches[0].Observer.IsBridge {
		t.Errorf("Observer (BridgePK, case-insensitive match on bridgepk) IsBridge = false, want true")
	}
}
