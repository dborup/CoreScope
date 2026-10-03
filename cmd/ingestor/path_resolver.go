package main

import (
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
)

// Context-aware hop resolver — full restore of pre-#1289 hop
// disambiguation semantics, ported into the ingestor (where the
// neighbor graph + node directory now live, per #1283).
//
// Why this exists (issues #1547 / #1560):
//   The naive `resolvePath` only resolves hops whose prefix is unique
//   in the node table. On a >2K-node mesh the dominant case is 1-byte
//   prefix collisions (multiple candidates per prefix). Without
//   adjacency disambiguation those hops always serialize as `nil`
//   and the resolved_path remains effectively empty for the largest
//   meshes — the very deployments that need it most.
//
// Algorithm (ported from cmd/server/store.go @ commit 450236d5
// `pm.resolveWithContext`, intersected with the disambiguation gating
// from PR #1144 / #1352):
//
//   For each hop:
//     1. Collect candidate pubkeys by prefix-match (existing prefixIndex).
//     2. len==0 → nil.
//     3. len==1 → that pubkey.
//     4. len>1 → filter by NeighborGraph adjacency to the anchor:
//          - hop 0 anchor = fromPubkey (ADVERT originator) if known;
//          - hop i (i>0) anchor = previous resolved hop's pubkey;
//            if the previous hop did not resolve, the chain breaks
//            and subsequent >1-candidate hops fall to nil.
//        Surviving candidates after filter:
//          - exactly 1 → use it
//          - 0 or >1   → nil (cannot disambiguate further)
//
// This is the conservative tier-1 variant. Pre-#1289 also carried
// tier-2 (geo proximity), tier-3 (GPS preference), tier-4 (obs-count
// fallback) — those were noisy in practice and are intentionally NOT
// ported here; this PR is a regression restore, not an enhancement.

// NeighborGraph is the in-memory adjacency snapshot used by the
// context-aware resolver. Internally lowercased.
type NeighborGraph struct {
	adj map[string]map[string]struct{}
}

// NewNeighborGraph returns an empty graph.
func NewNeighborGraph() *NeighborGraph {
	return &NeighborGraph{adj: make(map[string]map[string]struct{})}
}

// AddEdge adds an undirected adjacency a↔b. Self-loops and empty
// endpoints are ignored.
func (g *NeighborGraph) AddEdge(a, b string) {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	if a == "" || b == "" || a == b {
		return
	}
	if g.adj[a] == nil {
		g.adj[a] = make(map[string]struct{})
	}
	if g.adj[b] == nil {
		g.adj[b] = make(map[string]struct{})
	}
	g.adj[a][b] = struct{}{}
	g.adj[b][a] = struct{}{}
}

// IsAdjacent reports whether a and b appear together in any neighbor edge.
func (g *NeighborGraph) IsAdjacent(a, b string) bool {
	if g == nil {
		return false
	}
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	if a == "" || b == "" {
		return false
	}
	nbrs, ok := g.adj[a]
	if !ok {
		return false
	}
	_, present := nbrs[b]
	return present
}

// empty reports whether the graph has no edge.
func (g *NeighborGraph) empty() bool {
	return g == nil || len(g.adj) == 0
}

// neighborGraphHolder caches the graph for the InsertTransmission hot
// path. atomic.Value lets the 60s rebuild publish without a read-side
// lock.
//
// It also records whether a snapshot loaded after a successful
// neighbor_edges build has been published (storeBuilt). Before that, the
// snapshot may predate the edges the warm-up build derives; the
// resolved_path backfill waits for it (#188, PR #190 review).
type neighborGraphHolder struct {
	v atomic.Value // holds *NeighborGraph

	mu    sync.Mutex
	built bool          // a post-build snapshot has been published
	next  chan struct{} // closed when the next post-build snapshot is published
}

func (h *neighborGraphHolder) load() *NeighborGraph {
	if v := h.v.Load(); v != nil {
		return v.(*NeighborGraph)
	}
	return nil
}

func (h *neighborGraphHolder) store(g *NeighborGraph) {
	h.v.Store(g)
}

// storeBuilt publishes g, loaded after a successful neighbor_edges build,
// and wakes everyone waiting on buildState's channel.
func (h *neighborGraphHolder) storeBuilt(g *NeighborGraph) {
	h.store(g)
	h.mu.Lock()
	h.built = true
	if h.next != nil {
		close(h.next)
		h.next = nil
	}
	h.mu.Unlock()
}

// buildState reports whether a post-build snapshot has been published, and
// returns a channel that is closed when the next one is.
func (h *neighborGraphHolder) buildState() (built bool, next <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.next == nil {
		h.next = make(chan struct{})
	}
	return h.built, h.next
}

// loadNeighborGraph reads neighbor_edges and returns an in-memory
// adjacency snapshot. Safe to call against a fresh DB (returns an
// empty graph).
func loadNeighborGraph(db *sql.DB) (*NeighborGraph, error) {
	rows, err := db.Query(`SELECT node_a, node_b FROM neighbor_edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	g := NewNeighborGraph()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			continue
		}
		g.AddEdge(a, b)
	}
	return g, nil
}

// resolveHopWithContext resolves a single hop using NeighborGraph
// adjacency to the anchor. Returns nil when the hop cannot be
// disambiguated.
//
// exclude is a set of pubkeys to discard from the candidate pool
// (typically the prior hops already resolved on the path — a packet
// does not revisit a node).
//
// Behavior matrix:
//
//	len(candidates) | anchor       | graph | result
//	0               | —            | —     | nil
//	1               | —            | —     | candidates[0]
//	>1              | "" or no graph|—     | nil
//	>1              | non-empty    | set   | unique adjacent candidate
//	                                         (or nil if 0 or >1 survive)
func resolveHopWithContext(hop string, anchor string, graph *NeighborGraph, idx prefixIndex, exclude map[string]struct{}) *string {
	if idx == nil {
		return nil
	}
	h := strings.ToLower(hop)
	candidates := idx[h]
	switch len(candidates) {
	case 0:
		return nil
	case 1:
		pk := candidates[0]
		if _, skip := exclude[pk]; skip {
			return nil
		}
		return &pk
	}
	if graph == nil || anchor == "" {
		return nil
	}
	var match string
	survivors := 0
	for _, cand := range candidates {
		if _, skip := exclude[cand]; skip {
			continue
		}
		if graph.IsAdjacent(anchor, cand) {
			survivors++
			if survivors > 1 {
				return nil
			}
			match = cand
		}
	}
	if survivors == 1 {
		return &match
	}
	return nil
}

// resolvePathWithContext walks the hop list, anchoring hop 0 on
// fromPubkey (for ADVERTs) and each subsequent hop on the previous
// resolved hop. Previously-resolved pubkeys (plus the originator) are
// excluded from later candidate pools so the walk doesn't revisit a
// node. Returns a `[]*string` shape compatible with
// marshalResolvedPath (and the all-nil clobber-guard from PR #1548).
func resolvePathWithContext(hops []string, fromPubkey string, graph *NeighborGraph, idx prefixIndex) []*string {
	return resolvePathForward(hops, fromPubkey, "", graph, idx)
}

// resolvePathForward is the forward walk of resolvePathWithContext. notLast
// (lower-case), when set, is not a candidate for the last hop (#188: the
// observer of a flood path, see resolveObservationPath).
func resolvePathForward(hops []string, fromPubkey, notLast string, graph *NeighborGraph, idx prefixIndex) []*string {
	if len(hops) == 0 {
		return nil
	}
	out := make([]*string, len(hops))
	if idx == nil {
		return out
	}
	prevAnchor := strings.ToLower(fromPubkey)
	seen := make(map[string]struct{}, len(hops)+2)
	if prevAnchor != "" {
		seen[prevAnchor] = struct{}{}
	}
	for i, hop := range hops {
		if notLast != "" && i == len(hops)-1 {
			seen[notLast] = struct{}{}
		}
		r := resolveHopWithContext(hop, prevAnchor, graph, idx, seen)
		out[i] = r
		if r != nil {
			lc := strings.ToLower(*r)
			seen[lc] = struct{}{}
			prevAnchor = lc
		} else {
			prevAnchor = ""
		}
	}
	return out
}

// RefreshNeighborGraph loads the latest neighbor_edges snapshot and
// publishes it atomically. Called on startup and once per neighbor-
// edges builder tick (60s) alongside RefreshPrefixIndex.
func (s *Store) RefreshNeighborGraph() error {
	g, err := loadNeighborGraph(s.db)
	if err != nil {
		return err
	}
	s.neighborGraph.store(g)
	return nil
}

// refreshBuiltNeighborGraph is RefreshNeighborGraph for a caller that has
// just completed a successful buildAndPersistNeighborEdges: it marks the
// snapshot as post-build (neighborGraphHolder.storeBuilt).
func (s *Store) refreshBuiltNeighborGraph() error {
	g, err := loadNeighborGraph(s.db)
	if err != nil {
		return err
	}
	s.neighborGraph.storeBuilt(g)
	return nil
}

// Observer anchor (#188)
//
// Firmware basis (MeshCore src/): a flood forwarder appends its own hash
// before it retransmits (Mesh.cpp routeRecvPacket: copyHashTo(&path[n*sz])),
// and an observer logs a packet on reception, before its own routing step
// (Dispatcher.cpp: logRx(pkt, ...) runs before processRecvPacket). So the
// last hash of a FLOOD path an observer reports is the node it heard the
// packet from: a direct neighbour of the observer. This does not hold for
// DIRECT routes (the path is the remaining planned route and each forwarder
// strips itself from the front, Mesh.cpp removeSelfFromPath), nor for TRACE
// (the path carries SNR bytes), so only flood route types are anchored.
//
// Observer identity: the observer is the <observer_id> segment of the MQTT
// topic meshcore/<iata>/<observer_id>/packets, which is the observer node's
// pubkey. The neighbour builder writes observer<->last-hop edges keyed by
// that id lower-cased (neighbor_builder.go), and NeighborGraph lower-cases
// on lookup, so an id that is not a node (e.g. "companion") has no edges and
// anchors nothing.

// Route types (MeshCore src/Packet.h ROUTE_TYPE_*).
const (
	routeTypeTransportFlood = 0
	routeTypeFlood          = 1
)

func isFloodRoute(routeType int) bool {
	return routeType == routeTypeTransportFlood || routeType == routeTypeFlood
}

// resolveObservationPath resolves one observation's hops for
// observations.resolved_path. It runs the forward chain from fromPubkey
// (resolvePathForward, the walk behind resolvePathWithContext); for a flood
// packet whose forward chain left a hop nil, it also runs a backward chain
// from the observer and merges the two (mergeForwardBackward). Both chains
// use resolveHopWithContext, so a hop resolves only to a unique candidate:
// no geo, GPS or count tie-break.
//
// In a flood path, the observer is never the last hop: a radio does not
// receive its own transmission. Both chains leave that hop nil rather than
// name the observer, even when its prefix is unique (PR #190 review).
func resolveObservationPath(hops []string, fromPubkey, observer string, routeType int, graph *NeighborGraph, idx prefixIndex) []*string {
	if !isFloodRoute(routeType) {
		return resolvePathWithContext(hops, fromPubkey, graph, idx)
	}
	observer = strings.ToLower(observer)
	fwd := resolvePathForward(hops, fromPubkey, observer, graph, idx)
	if graph == nil || idx == nil || observer == "" || !hasNil(fwd) {
		return fwd
	}
	return mergeForwardBackward(fwd, resolvePathBackward(hops, fromPubkey, observer, graph, idx))
}

// resolvePathBackward walks the hops from last to first. The last hop is
// anchored on the observer, each earlier hop on the hop after it; a hop that
// does not resolve breaks the chain, as in the forward walk. fromPubkey and
// already resolved hops are excluded from later candidate pools.
//
// The observer (lower-case) is excluded from the last hop only. It stays a
// candidate for earlier hops: it logs a packet before de-duplication
// (Dispatcher.cpp logRx runs before processRecvPacket), so it can report the
// echo of a flood it forwarded itself.
func resolvePathBackward(hops []string, fromPubkey, observer string, graph *NeighborGraph, idx prefixIndex) []*string {
	out := make([]*string, len(hops))
	anchor := observer
	seen := make(map[string]struct{}, len(hops)+2)
	if fp := strings.ToLower(fromPubkey); fp != "" {
		seen[fp] = struct{}{}
	}
	_, observerSeen := seen[observer]
	seen[observer] = struct{}{}
	for i := len(hops) - 1; i >= 0; i-- {
		r := resolveHopWithContext(hops[i], anchor, graph, idx, seen)
		if i == len(hops)-1 && !observerSeen {
			delete(seen, observer)
		}
		out[i] = r
		if r != nil {
			seen[*r] = struct{}{}
			anchor = *r
		} else {
			anchor = ""
		}
	}
	return out
}

// mergeForwardBackward combines the two chains. A hop keeps the forward
// result when there is one and takes the backward result otherwise. The two
// must never contradict each other in one row: if they resolve a hop to
// different nodes, or the merge would put one node at two positions, the
// backward evidence is dropped for the whole row and fwd is returned as is.
func mergeForwardBackward(fwd, bwd []*string) []*string {
	out := make([]*string, len(fwd))
	seen := make(map[string]struct{}, len(fwd))
	for i, f := range fwd {
		b := bwd[i]
		if f != nil && b != nil && *f != *b {
			return fwd
		}
		r := f
		if r == nil {
			r = b
		}
		if r == nil {
			continue
		}
		if _, dup := seen[*r]; dup {
			return fwd
		}
		seen[*r] = struct{}{}
		out[i] = r
	}
	return out
}

func hasNil(rp []*string) bool {
	for _, p := range rp {
		if p == nil {
			return true
		}
	}
	return false
}
