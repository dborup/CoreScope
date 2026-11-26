package main

// #740 M1 — the per-node DAGI membership cache: node → {kommune, landsdel,
// region, postnummer}, and per layer code → set(pubkey), so a filter (M2)
// costs one map lookup per packet's sender and never a point-in-polygon
// test (AGENTS.md rule 0).
//
// Refresh takes the in-memory node list (getCachedNodesAndPM's snapshot)
// and builds everything in one pass. A node whose position is unchanged
// since the previous refresh keeps its membership without a lookup; only
// new and moved nodes are located. The result is published as an immutable
// snapshot, so readers never lock and never see a half-built one. Nodes
// that are gone from the list are gone from the snapshot, so it is bounded
// by the node count.

import (
	"math"
	"sync"
	"sync/atomic"
)

// dagiNodePlace is one node's cached membership: the position it was
// computed for and the feature index per layer (-1 for none).
type dagiNodePlace struct {
	lat, lon float64
	at       [dagiLayerCount]int32
}

// dagiMembershipSnapshot is one published state; never changed after.
type dagiMembershipSnapshot struct {
	nodes        map[string]dagiNodePlace
	members      [dagiLayerCount]map[string]map[string]struct{}
	unpositioned int
}

// dagiRefreshStats is what one Refresh did.
type dagiRefreshStats struct {
	Nodes        int `json:"nodes"`
	Located      int `json:"located"`
	Reused       int `json:"reused"`
	Unpositioned int `json:"unpositioned"`
}

// dagiMembership is the cache. The zero value is not usable; use
// newDagiMembership.
type dagiMembership struct {
	idx       *dagiIndex
	refreshMu sync.Mutex // serialises refreshes; readers never take it
	snap      atomic.Pointer[dagiMembershipSnapshot]
}

func newDagiMembership(idx *dagiIndex) *dagiMembership {
	m := &dagiMembership{idx: idx}
	m.snap.Store(&dagiMembershipSnapshot{nodes: map[string]dagiNodePlace{}})
	return m
}

// samePosition compares bit patterns, so an unchanged NaN counts as
// unchanged too.
func samePosition(a dagiNodePlace, lat, lon float64) bool {
	return math.Float64bits(a.lat) == math.Float64bits(lat) && math.Float64bits(a.lon) == math.Float64bits(lon)
}

// Refresh rebuilds the cache from nodes in one pass. A node without a
// position (HasGPS false, or not isValidNodePosition) gets no membership
// and is counted as unpositioned.
func (m *dagiMembership) Refresh(nodes []nodeInfo) dagiRefreshStats {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	prev := m.snap.Load()
	next := &dagiMembershipSnapshot{nodes: make(map[string]dagiNodePlace, len(nodes))}
	for l := range next.members {
		next.members[l] = make(map[string]map[string]struct{}, len(m.idx.layers[l].features))
	}
	st := dagiRefreshStats{Nodes: len(nodes)}
	for i := range nodes {
		n := &nodes[i]
		if !n.HasGPS || !isValidNodePosition(n.Lat, n.Lon) {
			st.Unpositioned++
			continue
		}
		p, ok := prev.nodes[n.PublicKey]
		if ok && samePosition(p, n.Lat, n.Lon) {
			st.Reused++
		} else {
			p = dagiNodePlace{lat: n.Lat, lon: n.Lon, at: m.idx.locate(n.Lat, n.Lon)}
			st.Located++
		}
		next.nodes[n.PublicKey] = p
		for l, fi := range p.at {
			if fi < 0 {
				continue
			}
			code := m.idx.layers[l].features[fi].area.Code
			set := next.members[l][code]
			if set == nil {
				set = make(map[string]struct{})
				next.members[l][code] = set
			}
			set[n.PublicKey] = struct{}{}
		}
	}
	next.unpositioned = st.Unpositioned
	m.snap.Store(next)
	return st
}

// Place is the cached membership of a node, and whether the node had a
// usable position at the last refresh.
func (m *dagiMembership) Place(pubkey string) (DagiPlace, bool) {
	p, ok := m.snap.Load().nodes[pubkey]
	if !ok {
		return DagiPlace{}, false
	}
	return m.idx.place(p.at), true
}

// Members is the set of pubkeys in the feature code of layer l, nil when
// none. The set is shared and read-only.
func (m *dagiMembership) Members(l DagiLayer, code string) map[string]struct{} {
	if l < 0 || l >= dagiLayerCount {
		return nil
	}
	return m.snap.Load().members[l][code]
}

// Contains reports whether pubkey is in the feature code of layer l: the
// per-packet check of the filters, one map lookup.
func (m *dagiMembership) Contains(l DagiLayer, code, pubkey string) bool {
	_, ok := m.Members(l, code)[pubkey]
	return ok
}

// Unpositioned is how many nodes of the last refresh had no position, for
// the "N nodes without position hidden" notice (M2/M3).
func (m *dagiMembership) Unpositioned() int { return m.snap.Load().unpositioned }
