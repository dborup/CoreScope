package main

// #740 M1 — the per-node DAGI membership cache: node → {kommune, landsdel,
// region, postnummer}, and per layer code → set(pubkey), so a filter (M2)
// costs one map lookup per packet's sender and never a point-in-polygon
// test (AGENTS.md rule 0).
//
// Refresh takes the in-memory node list (getCachedNodesAndPM's snapshot)
// and builds everything in one pass. M2 calls RefreshIfMoved each time the
// store publishes a new node snapshot, so a refresh happens once at startup
// and then only when a node was added, removed, moved or renamed. A node whose position is unchanged
// since the previous refresh keeps its membership without a lookup; only
// new and moved nodes are located. The result is published as an immutable
// snapshot, so readers never lock and never see a half-built one. Nodes
// that are gone from the list are gone from the snapshot, so it is bounded
// by the node count.

import (
	"encoding/binary"
	"hash/maphash"
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

// dagiMembershipSnapshot is one published state; never changed after. A
// request reads one (dagiMembership.Snapshot), so everything it filters and
// counts comes from the same refresh.
type dagiMembershipSnapshot struct {
	idx   *dagiIndex
	nodes map[string]dagiNodePlace
	// members is the filter's view, per layer by filter key (the code; a
	// grouped postnummer's group key). Postnummer holds only nodes that are
	// also in a kommune: DAGI's postnummer polygons reach out to sea, and a
	// node out there is not in the postal district (#740).
	members      [dagiLayerCount]map[string]map[string]struct{}
	unpositioned []dagiUnplaced
}

// dagiUnplaced is a node of the refresh without a usable position: no DAGI
// filter can match it, and the filters report how many they hid.
type dagiUnplaced struct{ pubkey, name string }

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
	seed      maphash.Seed
	// Guarded by refreshMu: the fingerprint of the last refresh's nodes.
	refreshed bool
	lastPrint uint64
	done      atomic.Bool // set by the first refresh; readable without the lock
}

func newDagiMembership(idx *dagiIndex) *dagiMembership {
	m := &dagiMembership{idx: idx, seed: maphash.MakeSeed()}
	m.snap.Store(&dagiMembershipSnapshot{idx: idx, nodes: map[string]dagiNodePlace{}})
	return m
}

// fingerprint is an order-independent hash of what a refresh reads from
// nodes: each node's key, name, position and HasGPS.
func (m *dagiMembership) fingerprint(nodes []nodeInfo) uint64 {
	var h maphash.Hash
	h.SetSeed(m.seed)
	var buf [17]byte
	sum := uint64(len(nodes))
	for i := range nodes {
		n := &nodes[i]
		h.Reset()
		h.WriteString(n.PublicKey)
		h.WriteByte(0)
		h.WriteString(n.Name)
		binary.LittleEndian.PutUint64(buf[0:], math.Float64bits(n.Lat))
		binary.LittleEndian.PutUint64(buf[8:], math.Float64bits(n.Lon))
		buf[16] = 0
		if n.HasGPS {
			buf[16] = 1
		}
		h.Write(buf[:])
		sum += h.Sum64() // a sum, so the order does not matter and duplicates do not cancel
	}
	return sum
}

// RefreshIfMoved refreshes unless nodes is, but for its order, what the last
// refresh read (same keys, names, positions and HasGPS). It reports whether
// it refreshed; the fingerprint costs one hash per node, no lookup and no
// map.
func (m *dagiMembership) RefreshIfMoved(nodes []nodeInfo) (dagiRefreshStats, bool) {
	fp := m.fingerprint(nodes)
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if m.refreshed && fp == m.lastPrint {
		return dagiRefreshStats{}, false
	}
	return m.refreshLocked(nodes, fp), true
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
	fp := m.fingerprint(nodes)
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	return m.refreshLocked(nodes, fp)
}

func (m *dagiMembership) refreshLocked(nodes []nodeInfo, fp uint64) dagiRefreshStats {
	prev := m.snap.Load()
	next := &dagiMembershipSnapshot{idx: m.idx, nodes: make(map[string]dagiNodePlace, len(nodes))}
	for l := range next.members {
		next.members[l] = make(map[string]map[string]struct{}, len(m.idx.layers[l].features))
	}
	st := dagiRefreshStats{Nodes: len(nodes)}
	for i := range nodes {
		n := &nodes[i]
		if !n.HasGPS || !isValidNodePosition(n.Lat, n.Lon) {
			st.Unpositioned++
			next.unpositioned = append(next.unpositioned, dagiUnplaced{n.PublicKey, n.Name})
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
			if fi < 0 || (DagiLayer(l) == DagiPostnummer && p.at[DagiKommune] < 0) {
				continue
			}
			key := m.idx.layers[l].features[fi].key
			set := next.members[l][key]
			if set == nil {
				set = make(map[string]struct{})
				next.members[l][key] = set
			}
			set[n.PublicKey] = struct{}{}
		}
	}
	m.snap.Store(next)
	m.refreshed, m.lastPrint = true, fp
	m.done.Store(true)
	return st
}

// Snapshot is the current published state. Read it once per request.
func (m *dagiMembership) Snapshot() *dagiMembershipSnapshot { return m.snap.Load() }

// Place is the cached membership of a node, and whether the node had a
// usable position at the last refresh. It is the lookup's answer: a node at
// sea keeps its sea postnummer here, though the postnummer filter skips it.
func (s *dagiMembershipSnapshot) Place(pubkey string) (DagiPlace, bool) {
	p, ok := s.nodes[pubkey]
	if !ok {
		return DagiPlace{}, false
	}
	return s.idx.place(p.at), true
}

// Members is the set of pubkeys the filter for key in layer l matches, nil
// when none. The set is shared and read-only.
func (s *dagiMembershipSnapshot) Members(l DagiLayer, key string) map[string]struct{} {
	if l < 0 || l >= dagiLayerCount {
		return nil
	}
	return s.members[l][key]
}

// Contains reports whether the filter for key in layer l matches pubkey:
// one map lookup.
func (s *dagiMembershipSnapshot) Contains(l DagiLayer, key, pubkey string) bool {
	_, ok := s.Members(l, key)[pubkey]
	return ok
}

// Unpositioned is how many nodes of the refresh had no usable position.
func (s *dagiMembershipSnapshot) Unpositioned() int { return len(s.unpositioned) }

// The cache's own readers each read the current snapshot.

func (m *dagiMembership) Place(pubkey string) (DagiPlace, bool) { return m.Snapshot().Place(pubkey) }

func (m *dagiMembership) Members(l DagiLayer, key string) map[string]struct{} {
	return m.Snapshot().Members(l, key)
}

func (m *dagiMembership) Contains(l DagiLayer, key, pubkey string) bool {
	return m.Snapshot().Contains(l, key, pubkey)
}

func (m *dagiMembership) Unpositioned() int { return m.Snapshot().Unpositioned() }
