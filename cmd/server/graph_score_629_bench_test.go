package main

// Issue #629 (upstream follow-up 2293): measure-first benchmark for the
// Bridge and Coverage scores on the bounded 2 000-node / 10 000-edge graph,
// used to decide whether upstream's decrease-key heap and reached-only reset
// are a measurable win on top of the indexed workspaces from #587 (#561).
// Reported memory is ALLOCATED bytes per op (B/op), not retained heap.
//
//	go test -run '^$' -bench 'GraphScore629' -benchmem -count 6 .

import (
	"fmt"
	"testing"
)

// graphScore629Seed fixes the synthetic inputs so runs on master and on any
// candidate port measure identical graphs.
const graphScore629Seed = 629

// syntheticFragmentedBridgeEdges builds `components` disjoint connected
// components of nodes/components vertices and edges/components unique edges
// each (see syntheticBridgeEdges). Fragmented meshes are the case upstream's
// reached-only workspace reset targets: a source reaches only its own
// component, so an O(V) reset per source is the most work it can save.
func syntheticFragmentedBridgeEdges(nodes, edges, components int, seed int64) []BridgeEdge {
	out := make([]BridgeEdge, 0, edges)
	for c := 0; c < components; c++ {
		out = append(out, syntheticBridgeEdges(nodes/components, edges/components, seed+int64(c))...)
	}
	return out
}

// graphScore629Shapes: the issue's connected 2 000 / 10 000 graph plus the
// same size split into 20 components.
func graphScore629Shapes() []struct {
	name  string
	nodes int
	edges []BridgeEdge
} {
	return []struct {
		name  string
		nodes int
		edges []BridgeEdge
	}{
		{"mesh_V2000_E10000", 2000, syntheticBridgeEdges(2000, 10000, graphScore629Seed)},
		{"fragmented20_V2000_E10000", 2000, syntheticFragmentedBridgeEdges(2000, 10000, 20, graphScore629Seed)},
	}
}

func BenchmarkGraphScore629(b *testing.B) {
	axes := []struct {
		name    string
		compute func([]BridgeEdge) map[string]float64
	}{{"bridge", ComputeBridgeScores}, {"coverage", ComputeCoverageScores}}
	for _, shape := range graphScore629Shapes() {
		for _, axis := range axes {
			b.Run(fmt.Sprintf("%s/%s", shape.name, axis.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if got := axis.compute(shape.edges); len(got) != shape.nodes {
						b.Fatalf("got %d scores, want %d", len(got), shape.nodes)
					}
				}
			})
		}
	}
}
