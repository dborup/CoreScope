// Package main: shared weighted-graph primitives for the structural
// usefulness axes (issue #672). The Bridge (betweenness) and Coverage
// (harmonic reach) axes operate on the same undirected, affinity-weighted
// neighbor graph and previously each built the distance adjacency by hand —
// a line-for-line duplicate (#1762 review). These helpers are the single
// source of truth for that construction. Within a single recomputeUsefulnessAxes
// call the Bridge and Coverage axes are fed from the SAME bridgeEdgesFromGraph
// snapshot, so they see byte-identical structure there. This does NOT extend
// across recomputers: the bridge recomputer and the usefulness-axes recomputer
// run on independent cadences and take their own graph snapshots, so the bridge
// map surfaced by handleNodes need not match the Coverage axis's snapshot.
//
// Issue #561: both axes run one shortest-path computation per source vertex,
// so per-source cost dominates. The graph is therefore indexed once per
// computation (int32 vertex ids, CSR adjacency) and each computation owns one
// workspace whose flat slices are reset — not reallocated — for every source.
// Workspaces are never shared between computations or goroutines and never
// cached globally, so concurrent callers stay isolated and memory is bounded
// by O(V + E) per in-flight computation.
package main

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// indexedDistanceGraph is the symmetric distance adjacency of a weighted edge
// list in compressed-sparse-row form: cost = 1/weight (lower distance =
// stronger affinity), keeping the cheapest edge per pair. Self-loops, empty
// endpoints and edges with a non-finite weight (NaN/±Inf) or weight <
// bridgeMinWeightEpsilon are skipped — they would break Dijkstra's relaxation
// invariant. Note `w < epsilon` is false for NaN, so NaN must be rejected
// explicitly; otherwise 1/NaN = NaN would poison every reach computation
// downstream (#1762 review).
//
// Vertex ids follow the sorted lowercase pubkey order and each row is sorted
// by neighbor id, so traversal order — and hence float summation order — does
// not depend on input edge order or map iteration: results are bit-identical
// across runs and edge permutations.
type indexedDistanceGraph struct {
	keys []string  // vertex id → lowercase pubkey, ascending
	off  []int32   // row offsets: neighbors of v are nbr[off[v]:off[v+1]]
	nbr  []int32   // neighbor ids, ascending within a row
	cost []float64 // edge distance, parallel to nbr
}

func (g *indexedDistanceGraph) numVertices() int { return len(g.keys) }

// newIndexedDistanceGraph builds the indexed graph in O(V log V + E log E).
func newIndexedDistanceGraph(edges []BridgeEdge) *indexedDistanceGraph {
	type halfEdge struct {
		from, to int32
		cost     float64
	}
	ids := make(map[string]int32)
	var keys []string
	id := func(k string) int32 {
		if i, ok := ids[k]; ok {
			return i
		}
		i := int32(len(keys))
		ids[k] = i
		keys = append(keys, k)
		return i
	}
	half := make([]halfEdge, 0, 2*len(edges))
	for _, e := range edges {
		a := strings.ToLower(strings.TrimSpace(e.A))
		b := strings.ToLower(strings.TrimSpace(e.B))
		if a == "" || b == "" || a == b {
			continue
		}
		w := e.Weight
		if math.IsNaN(w) || math.IsInf(w, 0) || w < bridgeMinWeightEpsilon {
			continue
		}
		ia, ib := id(a), id(b)
		dist := 1.0 / w
		half = append(half, halfEdge{ia, ib, dist}, halfEdge{ib, ia, dist})
	}

	// Renumber ids into sorted-key order.
	order := make([]int32, len(keys))
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortFunc(order, func(x, y int32) int { return strings.Compare(keys[x], keys[y]) })
	remap := make([]int32, len(keys))
	sortedKeys := make([]string, len(keys))
	for newID, oldID := range order {
		remap[oldID] = int32(newID)
		sortedKeys[newID] = keys[oldID]
	}
	for i := range half {
		half[i].from, half[i].to = remap[half[i].from], remap[half[i].to]
	}

	// Sort by (from, to, cost) so the first of each duplicate run is the
	// cheapest; keep only that one.
	slices.SortFunc(half, func(x, y halfEdge) int {
		if c := cmp.Compare(x.from, y.from); c != 0 {
			return c
		}
		if c := cmp.Compare(x.to, y.to); c != 0 {
			return c
		}
		return cmp.Compare(x.cost, y.cost)
	})
	g := &indexedDistanceGraph{
		keys: sortedKeys,
		off:  make([]int32, len(keys)+1),
		nbr:  make([]int32, 0, len(half)),
		cost: make([]float64, 0, len(half)),
	}
	for i, h := range half {
		if i > 0 && h.from == half[i-1].from && h.to == half[i-1].to {
			continue
		}
		g.off[h.from+1]++
		g.nbr = append(g.nbr, h.to)
		g.cost = append(g.cost, h.cost)
	}
	for v := 0; v < len(keys); v++ {
		g.off[v+1] += g.off[v]
	}
	return g
}

// scoreMap pairs per-vertex values with their pubkeys.
func (g *indexedDistanceGraph) scoreMap(vals []float64) map[string]float64 {
	out := make(map[string]float64, len(vals))
	for v, x := range vals {
		out[g.keys[v]] = x
	}
	return out
}

// normalizeByMax scales vals in place so the maximum is 1. If the maximum is
// 0 (no signal) everything stays at zero.
func normalizeByMax(vals []float64) {
	maxV := 0.0
	for _, x := range vals {
		if x > maxV {
			maxV = x
		}
	}
	if maxV > 0 {
		for i := range vals {
			vals[i] /= maxV
		}
	}
}

// ─── reusable single-source workspace ─────────────────────────────────────────

type distHeapItem struct {
	node int32
	dist float64
}

// distHeap is a binary min-heap on dist with lazy deletion (stale entries are
// skipped by the settled check). Typed — unlike container/heap it does not box
// every push into an interface value.
type distHeap []distHeapItem

func (h *distHeap) push(it distHeapItem) {
	*h = append(*h, it)
	a := *h
	for i := len(a) - 1; i > 0; {
		p := (i - 1) / 2
		if a[p].dist <= a[i].dist {
			break
		}
		a[p], a[i] = a[i], a[p]
		i = p
	}
}

func (h *distHeap) pop() distHeapItem {
	a := *h
	top := a[0]
	last := len(a) - 1
	a[0] = a[last]
	a = a[:last]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(a) && a[l].dist < a[m].dist {
			m = l
		}
		if r < len(a) && a[r].dist < a[m].dist {
			m = r
		}
		if m == i {
			break
		}
		a[i], a[m] = a[m], a[i]
		i = m
	}
	*h = a
	return top
}

// ssspWorkspace holds the Dijkstra state for one source. reset sizes it for a
// graph (reusing capacity) and fully re-initializes every slot, so nothing
// leaks between sources, graphs or graph sizes.
type ssspWorkspace struct {
	dist    []float64
	settled []bool
	heap    distHeap // ≤ 2E+1 entries (lazy deletion), capacity reused
}

func resized[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n)
	}
	return s[:n]
}

func (ws *ssspWorkspace) reset(g *indexedDistanceGraph, src int32) {
	n := g.numVertices()
	ws.dist = resized(ws.dist, n)
	ws.settled = resized(ws.settled, n)
	inf := math.Inf(1)
	for i := range ws.dist {
		ws.dist[i] = inf
		ws.settled[i] = false
	}
	ws.heap = ws.heap[:0]
	ws.dist[src] = 0
	ws.heap.push(distHeapItem{node: src, dist: 0})
}

// harmonicReach runs Dijkstra from src and returns Σ 1/d over every other
// reachable vertex (unreachable vertices contribute 1/∞ = 0).
func (ws *ssspWorkspace) harmonicReach(g *indexedDistanceGraph, src int32) float64 {
	ws.reset(g, src)
	var reach float64
	for len(ws.heap) > 0 {
		top := ws.heap.pop()
		v := top.node
		if ws.settled[v] {
			continue
		}
		ws.settled[v] = true
		if d := ws.dist[v]; v != src && d > 0 {
			reach += 1.0 / d
		}
		for k := g.off[v]; k < g.off[v+1]; k++ {
			w := g.nbr[k]
			if alt := top.dist + g.cost[k]; alt < ws.dist[w] {
				ws.dist[w] = alt
				ws.heap.push(distHeapItem{node: w, dist: alt})
			}
		}
	}
	return reach
}
