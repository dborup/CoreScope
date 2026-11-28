package main

// Issue #629 (upstream follow-up 2293): pinning tests for the Bridge and
// Coverage scores. Upstream's decrease-key heap / reached-only reset was
// measured against the #587 workspaces and not ported (see the PR), so these
// tests pin the current output instead: any future change to the
// shortest-path machinery must reproduce every score bit for bit, in the same
// ranking, on the benchmark graphs and on a real-shaped fixture graph.
//
// The golden file is captured from the current implementation. Do NOT
// regenerate it to make a refactor pass — a difference means the refactor
// changed the scores. (Regeneration, for a deliberate semantic change only:
// GRAPH_SCORE_629_WRITE_GOLDEN=1 go test -run TestGraphScore629_Golden .)

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const graphScore629GoldenPath = "testdata/graph_scores_629_golden.json"

// graphScore629TopN is how many leading ranks are stored verbatim (for a
// readable failure) next to the digest over all ranks.
const graphScore629TopN = 20

// rankedScore is one entry of a score map in ranking order. Bits is the
// IEEE-754 bit pattern in hex so the comparison is exact, not decimal.
type rankedScore struct {
	Key  string `json:"key"`
	Bits string `json:"bits"`
}

// scoreRanking orders a score map by score descending, ties by key ascending:
// the order in which the UI ranks repeaters.
func scoreRanking(scores map[string]float64) []rankedScore {
	keys := make([]string, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if scores[a] != scores[b] {
			if scores[a] > scores[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	out := make([]rankedScore, len(keys))
	for i, k := range keys {
		out[i] = rankedScore{Key: k, Bits: fmt.Sprintf("%016x", math.Float64bits(scores[k]))}
	}
	return out
}

func rankingDigest(r []rankedScore) string {
	h := sha256.New()
	for _, e := range r {
		fmt.Fprintf(h, "%s %s\n", e.Key, e.Bits)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pinnedAxis is the golden record of one score axis on one graph.
type pinnedAxis struct {
	Count  int           `json:"count"`
	Digest string        `json:"sha256"`
	Top    []rankedScore `json:"top"`
	All    []rankedScore `json:"all,omitempty"` // small graphs only
}

func pinAxis(scores map[string]float64, keepAll bool) pinnedAxis {
	r := scoreRanking(scores)
	p := pinnedAxis{Count: len(r), Digest: rankingDigest(r), Top: r[:min(graphScore629TopN, len(r))]}
	if keepAll {
		p.All = r
	}
	return p
}

// pinnedEdge stores a BridgeEdge with its weight as exact bits.
type pinnedEdge struct {
	A          string `json:"a"`
	B          string `json:"b"`
	WeightBits string `json:"w"`
}

type pinnedGraph struct {
	Name     string       `json:"name"`
	Source   string       `json:"source"`
	Edges    []pinnedEdge `json:"edges,omitempty"` // real-shaped fixture only
	Bridge   pinnedAxis   `json:"bridge"`
	Coverage pinnedAxis   `json:"coverage"`
}

type graphScore629Golden struct {
	Graphs []pinnedGraph `json:"graphs"`
}

func (g pinnedGraph) bridgeEdges(t testing.TB) []BridgeEdge {
	out := make([]BridgeEdge, len(g.Edges))
	for i, e := range g.Edges {
		bits, err := strconv.ParseUint(e.WeightBits, 16, 64)
		if err != nil {
			t.Fatalf("%s: edge %d weight %q: %v", g.Name, i, e.WeightBits, err)
		}
		out[i] = BridgeEdge{A: e.A, B: e.B, Weight: math.Float64frombits(bits)}
	}
	return out
}

// graphScore629FixtureNow is the fixed evaluation time for the real-shaped
// fixture's affinity weights (only used when regenerating the golden file).
var graphScore629FixtureNow = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

// realShapedFixtureEdges derives the weighted edge list the recomputers would
// see for test-fixtures/e2e-fixture.db (a staging snapshot) at a fixed time.
// Only the golden writer calls it: the test reads the stored edges so the pin
// does not move when the decay model changes.
func realShapedFixtureEdges(t *testing.T) []BridgeEdge {
	path := rpm2CopyFile(t, filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	edges := bridgeEdgesFromGraph(loadNeighborEdgesFromDB(db.conn), graphScore629FixtureNow)
	slices.SortFunc(edges, func(x, y BridgeEdge) int {
		if c := strings.Compare(x.A, y.A); c != 0 {
			return c
		}
		return strings.Compare(x.B, y.B)
	})
	return edges
}

// graphScore629PinnedGraph is a synthetic graph pinned by the golden file.
type graphScore629PinnedGraph struct {
	name, source string
	edges        []BridgeEdge
}

// graphScore629SyntheticGraphs: the two benchmark graphs plus two tie-heavy
// graphs (exactly equal path lengths everywhere). Ties are where a different
// heap could change the settle order among equal distances, and with it the
// floating-point summation order of Brandes' back-propagation.
func graphScore629SyntheticGraphs() []graphScore629PinnedGraph {
	var out []graphScore629PinnedGraph
	for _, s := range graphScore629Shapes() {
		out = append(out, graphScore629PinnedGraph{s.name, fmt.Sprintf("graphScore629Shapes (seed %d)", graphScore629Seed), s.edges})
	}
	return append(out,
		graphScore629PinnedGraph{"ties_V600_E3000", fmt.Sprintf("tieWeightedRandomEdges(600, 3000, %d)", graphScore629Seed),
			tieWeightedRandomEdges(600, 3000, graphScore629Seed)},
		graphScore629PinnedGraph{"grid_20x20", "gridEdges(20, 20)", gridEdges(20, 20)},
	)
}

func writeGraphScore629Golden(t *testing.T) {
	var golden graphScore629Golden
	for _, s := range graphScore629SyntheticGraphs() {
		golden.Graphs = append(golden.Graphs, pinnedGraph{
			Name:     s.name,
			Source:   s.source,
			Bridge:   pinAxis(ComputeBridgeScores(s.edges), false),
			Coverage: pinAxis(ComputeCoverageScores(s.edges), false),
		})
	}
	real := realShapedFixtureEdges(t)
	pg := pinnedGraph{
		Name:     "e2e_fixture_neighbor_edges",
		Source:   "test-fixtures/e2e-fixture.db neighbor_edges via bridgeEdgesFromGraph at " + graphScore629FixtureNow.Format(time.RFC3339),
		Bridge:   pinAxis(ComputeBridgeScores(real), true),
		Coverage: pinAxis(ComputeCoverageScores(real), true),
	}
	for _, e := range real {
		pg.Edges = append(pg.Edges, pinnedEdge{A: e.A, B: e.B, WeightBits: fmt.Sprintf("%016x", math.Float64bits(e.Weight))})
	}
	golden.Graphs = append(golden.Graphs, pg)
	raw, err := json.MarshalIndent(golden, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graphScore629GoldenPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", graphScore629GoldenPath)
}

func assertPinnedAxis(t *testing.T, label string, got map[string]float64, want pinnedAxis) {
	t.Helper()
	r := scoreRanking(got)
	if len(r) != want.Count {
		t.Fatalf("%s: %d scores, want %d", label, len(r), want.Count)
	}
	// Report the first differing rank so a failure says what moved.
	ref := want.All
	if ref == nil {
		ref = want.Top
	}
	for i, w := range ref {
		if r[i] != w {
			t.Fatalf("%s: rank %d = %s %s, want %s %s (scores must stay bit-identical in the same order)",
				label, i, r[i].Key, r[i].Bits, w.Key, w.Bits)
		}
	}
	if d := rankingDigest(r); d != want.Digest {
		t.Fatalf("%s: ranking digest %s, want %s (a score below rank %d changed bits or order)",
			label, d, want.Digest, len(ref))
	}
}

// TestGraphScore629_GoldenBitIdentical pins every Bridge and Coverage score,
// bit for bit and in ranking order, on the 2 000 / 10 000 benchmark graphs
// (connected and 20-component), on two tie-heavy graphs and on the
// real-shaped e2e fixture graph.
func TestGraphScore629_GoldenBitIdentical(t *testing.T) {
	if os.Getenv("GRAPH_SCORE_629_WRITE_GOLDEN") == "1" {
		writeGraphScore629Golden(t)
	}
	raw, err := os.ReadFile(graphScore629GoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden graphScore629Golden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	synthetic := make(map[string][]BridgeEdge)
	for _, s := range graphScore629SyntheticGraphs() {
		synthetic[s.name] = s.edges
	}
	if len(golden.Graphs) != len(synthetic)+1 {
		t.Fatalf("golden has %d graphs, want %d synthetic + 1 fixture", len(golden.Graphs), len(synthetic))
	}
	for _, g := range golden.Graphs {
		edges, ok := synthetic[g.Name]
		if !ok {
			if len(g.Edges) == 0 {
				t.Fatalf("golden graph %q is neither synthetic nor carries edges", g.Name)
			}
			edges = g.bridgeEdges(t)
		}
		t.Run(g.Name, func(t *testing.T) {
			assertPinnedAxis(t, "bridge", ComputeBridgeScores(edges), g.Bridge)
			assertPinnedAxis(t, "coverage", ComputeCoverageScores(edges), g.Coverage)
		})
	}
}

// TestGraphScore629_RealShapedFixtureIsNonTrivial guards the fixture pin
// against silently degenerating (e.g. every weight decayed below epsilon):
// it must have a real graph with several distinct nonzero scores per axis.
func TestGraphScore629_RealShapedFixtureIsNonTrivial(t *testing.T) {
	raw, err := os.ReadFile(graphScore629GoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden graphScore629Golden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	g := golden.Graphs[len(golden.Graphs)-1]
	if len(g.Edges) < 50 || g.Bridge.Count < 50 {
		t.Fatalf("fixture graph has %d edges / %d vertices, want ≥ 50 each", len(g.Edges), g.Bridge.Count)
	}
	for label, axis := range map[string]pinnedAxis{"bridge": g.Bridge, "coverage": g.Coverage} {
		distinct := map[string]bool{}
		for _, e := range axis.All {
			if e.Bits != fmt.Sprintf("%016x", math.Float64bits(0)) {
				distinct[e.Bits] = true
			}
		}
		if len(distinct) < 10 {
			t.Errorf("%s: only %d distinct nonzero scores", label, len(distinct))
		}
	}
}

// TestGraphScore629_FixtureShapes pins the benchmark inputs: exactly 2 000
// vertices and 10 000 unique undirected edges, in 1 and 20 connected
// components respectively, so before/after numbers measure the issue's graph.
func TestGraphScore629_FixtureShapes(t *testing.T) {
	want := map[string]int{"mesh_V2000_E10000": 1, "fragmented20_V2000_E10000": 20}
	for _, s := range graphScore629Shapes() {
		g := newIndexedDistanceGraph(s.edges)
		if g.numVertices() != 2000 || s.nodes != 2000 {
			t.Errorf("%s: %d vertices (declared %d), want 2000", s.name, g.numVertices(), s.nodes)
		}
		if len(g.nbr) != 2*10000 || len(s.edges) != 10000 {
			t.Errorf("%s: %d edges / %d unique, want 10000 unique", s.name, len(s.edges), len(g.nbr)/2)
		}
		if c := indexedComponents(g); c != want[s.name] {
			t.Errorf("%s: %d components, want %d", s.name, c, want[s.name])
		}
	}
}

func indexedComponents(g *indexedDistanceGraph) int {
	seen := make([]bool, g.numVertices())
	comps := 0
	for s := range seen {
		if seen[s] {
			continue
		}
		comps++
		stack := []int32{int32(s)}
		seen[s] = true
		for len(stack) > 0 {
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, w := range g.nbr[g.off[v]:g.off[v+1]] {
				if !seen[w] {
					seen[w] = true
					stack = append(stack, w)
				}
			}
		}
	}
	return comps
}

// TestGraphScore629_RecomputeDoesNotHoldStoreMu: both recomputers compute
// from the atomic graph snapshot without PacketStore.mu, so they finish while
// another goroutine holds the write lock.
func TestGraphScore629_RecomputeDoesNotHoldStoreMu(t *testing.T) {
	s := &PacketStore{}
	s.graph.Store(neighborGraphFromBridgeEdges(syntheticBridgeEdges(200, 800, graphScore629Seed), time.Now()))
	s.mu.Lock()
	defer s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		recomputeBridgeScoresSafe(s)
		s.recomputeUsefulnessAxes()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recompute blocked while PacketStore.mu was held")
	}
	if len(s.GetBridgeScoreMap()) != 200 || len(s.GetCoverageScoreMap()) != 200 {
		t.Fatalf("got %d bridge / %d coverage scores, want 200", len(s.GetBridgeScoreMap()), len(s.GetCoverageScoreMap()))
	}
}
