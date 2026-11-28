// Package main: coverage axis of repeater usefulness score (issue #672,
// axis 3 of 4). The "Coverage" signal is the normalized harmonic reach
// centrality of a node in the (undirected, weighted) neighbor graph: how
// well a repeater can reach the rest of the mesh. A node that sits close
// (in affinity-distance) to many other nodes covers more of the network;
// a peripheral or weakly-connected node covers little.
//
// Why harmonic (Σ 1/d) and not plain closeness (1 / Σ d): harmonic reach
// is well-defined on a DISCONNECTED graph — an unreachable node simply
// contributes 1/∞ = 0 — whereas closeness blows up. Real meshes fragment
// into components, so this matters. (Boldi & Vigna, "Axioms for
// Centrality", 2014.)
//
// It is deliberately distinct from the other axes: Traffic is empirical
// (observed relayed load), Bridge is betweenness (being ON shortest
// paths), Redundancy is removal-impact (criticality). Coverage is REACH
// breadth — a hub that can get a packet close to anyone, regardless of
// whether it currently carries that traffic.
//
// Edge weight and distance follow the bridge convention (#1235): weight =
// affinity Score(now) · observer-diversity Confidence(); Dijkstra needs a
// DISTANCE (lower = better) so cost = 1/weight. The input is the shared
// BridgeEdge weighted-edge primitive, and the shared indexed graph and
// reusable Dijkstra workspace (graph_weighted.go, #561) drive the per-source
// shortest paths.
//
// Algorithm: one Dijkstra single-source shortest-path computation per
// vertex, accumulating Σ 1/d over reachable targets, then normalize by
// the max observed reach so per-node scores live in [0, 1]. Complexity
// O(V · (E + V log V)) — identical to the bridge axis, comfortably a
// background-cadence cost.
package main

// ComputeCoverageScores returns a map pubkey → coverage score in [0, 1]
// computed as normalized harmonic reach centrality on the undirected
// weighted graph defined by `edges`. Keys are the lowercase pubkey form
// (matching the byPathHop / persisted-edge convention).
//
// The graph and per-source shortest paths come from the shared
// indexedDistanceGraph / ssspWorkspace helpers (graph_weighted.go). When
// Coverage and the Bridge axis are computed from the same edge snapshot within
// one recomputeUsefulnessAxes call they therefore see byte-identical structure;
// the bridge map surfaced independently by the bridge recomputer (different
// cadence/snapshot) is NOT guaranteed to match. Self-loops and edges with
// weight < epsilon are skipped there; nodes unable to reach anyone score 0.
//
// Pure (no global state, no locks); safe to call concurrently.
func ComputeCoverageScores(edges []BridgeEdge) map[string]float64 {
	g := newIndexedDistanceGraph(edges)
	n := g.numVertices()
	if n == 0 {
		return map[string]float64{}
	}

	// Unreachable peers contribute nothing (harmonic reach treats them as
	// 1/∞ = 0). Normalize by the max so the best-reaching repeater is 1.0;
	// if max is 0 (no reachable pair) leave zeros.
	harmonic := make([]float64, n)
	var ws ssspWorkspace
	for s := 0; s < n; s++ {
		harmonic[s] = ws.harmonicReach(g, int32(s))
	}
	normalizeByMax(harmonic)
	return g.scoreMap(harmonic)
}
