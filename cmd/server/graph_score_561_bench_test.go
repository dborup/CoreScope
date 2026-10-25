package main

// Issue #561: before/after benchmarks for the graph-score warmup (Bridge
// betweenness + Coverage harmonic reach). The inputs are deterministic
// (seeded) so runs on master and on the optimized branch measure identical
// graphs. Reported memory is ALLOCATED bytes per op (-benchmem B/op), not
// retained heap.
//
//	go test -run '^$' -bench 'GraphScore561' -benchmem -count 6 .

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// syntheticBridgeEdges builds a deterministic connected graph with exactly
// `nodes` vertices and `edges` unique undirected edges: a random spanning tree
// (so every vertex is reachable — worst case for all-sources SSSP) plus
// uniformly random extra pairs. Weights mirror production Score·Confidence in
// (0, 1]. Pubkeys are 64-hex lowercase, like NeighborGraph keys.
func syntheticBridgeEdges(nodes, edges int, seed int64) []BridgeEdge {
	rng := rand.New(rand.NewSource(seed))
	keys := make([]string, nodes)
	for i := range keys {
		keys[i] = fmt.Sprintf("%016x%048x", uint64(rng.Int63()), i)
	}
	type pair struct{ a, b int }
	seen := make(map[pair]bool, edges)
	out := make([]BridgeEdge, 0, edges)
	add := func(a, b int) {
		if a == b {
			return
		}
		if a > b {
			a, b = b, a
		}
		if seen[pair{a, b}] {
			return
		}
		seen[pair{a, b}] = true
		out = append(out, BridgeEdge{A: keys[a], B: keys[b], Weight: 0.01 + 0.99*rng.Float64()})
	}
	for i := 1; i < nodes && len(out) < edges; i++ {
		add(i, rng.Intn(i))
	}
	for len(out) < edges {
		add(rng.Intn(nodes), rng.Intn(nodes))
	}
	return out
}

// graphScore561Sizes: the issue's bounded representative graph (2 000 / 10 000)
// plus the staging-scale graph quoted in bridge_score.go (~600 / ~2 000).
var graphScore561Sizes = []struct{ v, e int }{
	{600, 2000},
	{2000, 10000},
}

func BenchmarkGraphScore561_Bridge(b *testing.B) {
	for _, sz := range graphScore561Sizes {
		edges := syntheticBridgeEdges(sz.v, sz.e, 561)
		b.Run(fmt.Sprintf("V%d_E%d", sz.v, sz.e), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = ComputeBridgeScores(edges)
			}
		})
	}
}

func BenchmarkGraphScore561_Coverage(b *testing.B) {
	for _, sz := range graphScore561Sizes {
		edges := syntheticBridgeEdges(sz.v, sz.e, 561)
		b.Run(fmt.Sprintf("V%d_E%d", sz.v, sz.e), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = ComputeCoverageScores(edges)
			}
		})
	}
}

// BenchmarkGraphScore561_Warmup measures exactly what the two Start*
// recomputers do synchronously before ListenAndServe: one
// recomputeBridgeScoresSafe plus one recomputeUsefulnessAxes (Coverage +
// Redundancy) over a NeighborGraph, including the bridgeEdgesFromGraph
// snapshot. Redundancy is unchanged by #561 but is part of the warmup cost.
func BenchmarkGraphScore561_Warmup(b *testing.B) {
	for _, sz := range graphScore561Sizes {
		g := neighborGraphFromBridgeEdges(syntheticBridgeEdges(sz.v, sz.e, 561), time.Now())
		b.Run(fmt.Sprintf("V%d_E%d", sz.v, sz.e), func(b *testing.B) {
			s := &PacketStore{}
			s.graph.Store(g)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				recomputeBridgeScoresSafe(s)
				s.recomputeUsefulnessAxes()
			}
			if len(s.GetBridgeScoreMap()) != sz.v || len(s.GetCoverageScoreMap()) != sz.v {
				b.Fatalf("warmup produced %d bridge / %d coverage scores, want %d",
					len(s.GetBridgeScoreMap()), len(s.GetCoverageScoreMap()), sz.v)
			}
		})
	}
}

// neighborGraphFromBridgeEdges materializes edges as a NeighborGraph via the
// production upsert path. Each edge is observed affinitySaturationCount times
// by one observer so its Score(now)·Confidence() is a positive constant.
func neighborGraphFromBridgeEdges(edges []BridgeEdge, now time.Time) *NeighborGraph {
	g := NewNeighborGraph()
	snr := 5.0
	for _, e := range edges {
		for i := 0; i < affinitySaturationCount; i++ {
			g.upsertEdge(e.A, e.B, e.B[:2], "obs-561", &snr, now)
		}
	}
	return g
}
