// Package main: bridge axis of repeater usefulness score (issue #672,
// axis 2 of 4). The "Bridge" signal is the betweenness centrality of a
// node in the (undirected, weighted) neighbor graph: a high value means
// the node lies on many shortest paths between other pairs and is hence
// structurally important — removing it would force traffic around or
// fragment the mesh.
//
// Algorithm: Brandes' algorithm (1) with Dijkstra for weighted
// shortest paths. Complexity O(V · (E + V log V)). For the staging
// graph (~600 nodes, ~2 000 edges) this is ~4.8M ops — trivial,
// completes in milliseconds. We accumulate raw betweenness across all
// sources, halve (an undirected pair is counted from each endpoint
// once), then normalize by the max observed value so the per-node
// score is in [0, 1].
//
// Edge weight follows the convention established by #1235: the
// affinity score (count + recency decay) is multiplied by the
// observer-diversity confidence — stronger, more corroborated
// neighborships are preferred when there is a choice of paths.
// Geo-rejected edges are already excluded from the input graph at
// build time (#1230) so we don't have to re-filter here.
//
// For Dijkstra we need a DISTANCE (lower = better) not an affinity
// (higher = better): cost = 1/weight. That conversion (plus the
// epsilon/non-finite-weight filtering and self-loop/dedup handling) lives
// in the shared indexedDistanceGraph (graph_weighted.go), used by both this
// axis and Coverage so they see byte-identical graph structure.
//
// Issue #561: the per-source state (distance, path count, dependency,
// predecessor lists, heap) lives in one brandesWorkspace of flat,
// vertex-indexed slices that is reset for every source instead of
// allocating string-keyed maps per source.
//
// (1) Brandes, "A Faster Algorithm for Betweenness Centrality" (2001).
package main

import "math"

// BridgeEdge is the algorithm-facing edge tuple consumed by
// ComputeBridgeScores. Endpoints A and B are pubkeys (case preserved
// by caller; we lowercase internally for stable keying). Weight is
// the affinity (higher = stronger connection). Edges with zero or
// negative weight are skipped — they would break Dijkstra's
// relaxation invariant.
type BridgeEdge struct {
	A, B   string
	Weight float64
}

// bridgeMinWeightEpsilon is the floor applied to weights before we
// invert them into Dijkstra distances. 1e-9 is small enough that any
// real weight (Score in [0,1] times Confidence in [0,1]) dominates,
// but large enough to avoid Inf when weight is exactly zero.
const bridgeMinWeightEpsilon = 1e-9

// ComputeBridgeScores returns a map pubkey → bridge score in [0, 1]
// computed via Brandes' weighted betweenness centrality on the
// undirected graph defined by `edges`. Returned map is keyed by the
// lowercase pubkey form (matching the byPathHop / persisted-edge
// convention). Nodes appearing in the graph but with zero betweenness
// are still present in the map with value 0.0.
//
// Self-loops (A == B) and edges with weight < epsilon are silently
// skipped. Duplicate edges between the same pair keep the cheapest
// (= the highest-weight) version — consistent with shortest-path
// semantics.
//
// Pure (no global state, no locks); safe to call concurrently.
// Cost: O(V · (E + V log V)).
func ComputeBridgeScores(edges []BridgeEdge) map[string]float64 {
	g := newIndexedDistanceGraph(edges)
	n := g.numVertices()
	if n == 0 {
		return map[string]float64{}
	}

	// Brandes outer loop: one Dijkstra-based single-source shortest-path
	// computation per source vertex, accumulating dependencies into bc.
	bc := make([]float64, n)
	var ws brandesWorkspace
	for s := 0; s < n; s++ {
		ws.accumulate(g, int32(s), bc)
	}

	// Undirected graphs double-count each (s,t) pair, so halve. Then
	// normalize by max so scores live in [0, 1]; if max is 0 (clique or
	// single edge) everything stays at zero.
	for i := range bc {
		bc[i] /= 2.0
	}
	normalizeByMax(bc)
	return g.scoreMap(bc)
}

// brandesWorkspace extends the Dijkstra workspace with Brandes' path counts
// (sigma), dependencies (delta), settle order (stack) and predecessor lists.
// A vertex's predecessors are distinct neighbors, so they are stored in its
// own CSR row of predBuf: pred(w) = predBuf[off[w] : off[w]+predLen[w]].
type brandesWorkspace struct {
	ssspWorkspace
	sigma, delta []float64
	stack        []int32
	predBuf      []int32 // len 2E, parallel to the graph's nbr
	predLen      []int32
}

func (ws *brandesWorkspace) reset(g *indexedDistanceGraph, src int32) {
	ws.ssspWorkspace.reset(g, src)
	n := g.numVertices()
	ws.sigma = resized(ws.sigma, n)
	ws.delta = resized(ws.delta, n)
	ws.predLen = resized(ws.predLen, n)
	ws.predBuf = resized(ws.predBuf, len(g.nbr))
	for i := 0; i < n; i++ {
		ws.sigma[i] = 0
		ws.delta[i] = 0
		ws.predLen[i] = 0
	}
	ws.stack = ws.stack[:0]
	ws.sigma[src] = 1
}

// accumulate runs one Brandes single-source pass from src and adds the
// resulting dependencies of every other vertex into bc.
func (ws *brandesWorkspace) accumulate(g *indexedDistanceGraph, src int32, bc []float64) {
	ws.reset(g, src)
	dist, sigma, delta := ws.dist, ws.sigma, ws.delta
	for len(ws.heap) > 0 {
		v := ws.heap.pop().node
		if ws.settled[v] {
			continue
		}
		ws.settled[v] = true
		ws.stack = append(ws.stack, v)

		for k := g.off[v]; k < g.off[v+1]; k++ {
			w := g.nbr[k]
			alt := dist[v] + g.cost[k]
			if alt < dist[w]-1e-12 {
				dist[w] = alt
				sigma[w] = sigma[v]
				ws.predBuf[g.off[w]] = v
				ws.predLen[w] = 1
				ws.heap.push(distHeapItem{node: w, dist: alt})
			} else if math.Abs(alt-dist[w]) <= 1e-12 {
				sigma[w] += sigma[v]
				ws.predBuf[g.off[w]+ws.predLen[w]] = v
				ws.predLen[w]++
			}
		}
	}

	// Back-propagation: walk the stack in reverse settle order.
	for i := len(ws.stack) - 1; i >= 0; i-- {
		w := ws.stack[i]
		if sigma[w] != 0 {
			for _, v := range ws.predBuf[g.off[w] : g.off[w]+ws.predLen[w]] {
				delta[v] += (sigma[v] / sigma[w]) * (1.0 + delta[w])
			}
		}
		if w != src {
			bc[w] += delta[w]
		}
	}
}
