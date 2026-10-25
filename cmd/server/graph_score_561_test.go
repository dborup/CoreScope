package main

// Issue #561: semantic pins for the Bridge (weighted Brandes betweenness) and
// Coverage (harmonic reach) graph scores, written BEFORE the indexed-graph
// optimization so the optimization cannot silently change what is computed.
//
// The reference oracle below is deliberately a DIFFERENT algorithm from
// production: Floyd–Warshall all-pairs distances, shortest-path counts by a
// distance-ordered DP, and betweenness from the pair-dependency definition
// Σ σ_st(v)/σ_st. Production (Dijkstra + Brandes back-propagation) is never
// used as its own oracle.
//
// Float tolerance: scores live in [0, 1]; equality is |got-want| <= 1e-9
// (graphScoreTol). Fixtures with ties use weights that are powers of two so
// tie distances are exact in float64.

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const graphScoreTol = 1e-9

// ─── independent oracle ──────────────────────────────────────────────────────

type oracleGraph struct {
	keys []string    // sorted lowercase pubkeys
	d    [][]float64 // direct edge distance (Inf = no edge)
	sp   [][]float64 // all-pairs shortest distance (Inf = unreachable)
}

// newOracleGraph applies the documented input contract independently of
// weightedDistanceAdjacency: trim+lowercase endpoints, drop empty endpoints,
// self-loops, NaN/±Inf and weight < 1e-9, distance = 1/weight, and keep the
// cheapest (highest-weight) duplicate.
func newOracleGraph(edges []BridgeEdge) *oracleGraph {
	type kept struct {
		a, b string
		dist float64
	}
	var ks []kept
	set := map[string]bool{}
	for _, e := range edges {
		a := strings.ToLower(strings.TrimSpace(e.A))
		b := strings.ToLower(strings.TrimSpace(e.B))
		if a == "" || b == "" || a == b || !(e.Weight >= 1e-9) || math.IsInf(e.Weight, 1) {
			continue
		}
		ks = append(ks, kept{a, b, 1 / e.Weight})
		set[a], set[b] = true, true
	}
	g := &oracleGraph{}
	for k := range set {
		g.keys = append(g.keys, k)
	}
	sort.Strings(g.keys)
	idx := map[string]int{}
	for i, k := range g.keys {
		idx[k] = i
	}
	n := len(g.keys)
	g.d = make([][]float64, n)
	g.sp = make([][]float64, n)
	for i := range g.d {
		g.d[i] = make([]float64, n)
		g.sp[i] = make([]float64, n)
		for j := range g.d[i] {
			g.d[i][j] = math.Inf(1)
		}
	}
	for _, k := range ks {
		i, j := idx[k.a], idx[k.b]
		if k.dist < g.d[i][j] {
			g.d[i][j], g.d[j][i] = k.dist, k.dist
		}
	}
	for i := 0; i < n; i++ {
		copy(g.sp[i], g.d[i])
		g.sp[i][i] = 0
	}
	for m := 0; m < n; m++ {
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				if alt := g.sp[i][m] + g.sp[m][j]; alt < g.sp[i][j] {
					g.sp[i][j] = alt
				}
			}
		}
	}
	return g
}

// oracleTie uses an ABSOLUTE tolerance: a relative one would, at the 1e9
// distances produced by epsilon weights, merge paths that differ by whole
// units. Tie fixtures use power-of-two weights, so real ties are exact.
func oracleTie(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}

// pathCounts returns σ[s][t], the number of shortest s→t paths, via a DP over
// targets ordered by distance from s: σ[s][t] = Σ σ[s][u] over neighbors u
// with sp[s][u] + d[u][t] == sp[s][t].
func (g *oracleGraph) pathCounts() [][]float64 {
	n := len(g.keys)
	sigma := make([][]float64, n)
	for s := 0; s < n; s++ {
		sigma[s] = make([]float64, n)
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(x, y int) bool { return g.sp[s][order[x]] < g.sp[s][order[y]] })
		sigma[s][s] = 1
		for _, t := range order {
			if t == s || math.IsInf(g.sp[s][t], 1) {
				continue
			}
			for u := 0; u < n; u++ {
				if !math.IsInf(g.d[u][t], 1) && !math.IsInf(g.sp[s][u], 1) &&
					g.sp[s][u] < g.sp[s][t] && oracleTie(g.sp[s][u]+g.d[u][t], g.sp[s][t]) {
					sigma[s][t] += sigma[s][u]
				}
			}
		}
	}
	return sigma
}

func oracleNormalize(keys []string, raw []float64) map[string]float64 {
	maxV := 0.0
	for _, v := range raw {
		maxV = math.Max(maxV, v)
	}
	out := make(map[string]float64, len(keys))
	for i, k := range keys {
		if maxV > 0 {
			out[k] = raw[i] / maxV
		} else {
			out[k] = 0
		}
	}
	return out
}

// oracleBridge: BC(v) = ½ Σ_{s≠v≠t, s≠t} σ_sv·σ_vt/σ_st over ordered pairs
// with sp[s][v]+sp[v][t] == sp[s][t], then normalized by the max.
func oracleBridge(edges []BridgeEdge) map[string]float64 {
	g := newOracleGraph(edges)
	n := len(g.keys)
	sigma := g.pathCounts()
	raw := make([]float64, n)
	for v := 0; v < n; v++ {
		for s := 0; s < n; s++ {
			for t := 0; t < n; t++ {
				if s == v || t == v || s == t || math.IsInf(g.sp[s][t], 1) ||
					math.IsInf(g.sp[s][v], 1) || math.IsInf(g.sp[v][t], 1) {
					continue
				}
				if oracleTie(g.sp[s][v]+g.sp[v][t], g.sp[s][t]) {
					raw[v] += sigma[s][v] * sigma[v][t] / sigma[s][t]
				}
			}
		}
		raw[v] /= 2
	}
	return oracleNormalize(g.keys, raw)
}

// oracleCoverage: harmonic reach Σ_{t≠s, reachable} 1/sp[s][t], normalized.
func oracleCoverage(edges []BridgeEdge) map[string]float64 {
	g := newOracleGraph(edges)
	n := len(g.keys)
	raw := make([]float64, n)
	for s := 0; s < n; s++ {
		for t := 0; t < n; t++ {
			if s != t && !math.IsInf(g.sp[s][t], 1) {
				raw[s] += 1 / g.sp[s][t]
			}
		}
	}
	return oracleNormalize(g.keys, raw)
}

// ─── fixtures ────────────────────────────────────────────────────────────────

func be(a, b string, w float64) BridgeEdge { return BridgeEdge{A: a, B: b, Weight: w} }

// tieWeightedRandomEdges: random graph whose weights are powers of two, so
// many equal-length shortest paths exist and their sums are exact.
func tieWeightedRandomEdges(nodes, edges int, seed int64) []BridgeEdge {
	rng := rand.New(rand.NewSource(seed))
	ws := []float64{1, 0.5, 0.25}
	out := make([]BridgeEdge, 0, edges)
	for len(out) < edges {
		a, b := rng.Intn(nodes), rng.Intn(nodes)
		out = append(out, be(string(rune('a'+a%26))+string(rune('a'+a/26)),
			string(rune('a'+b%26))+string(rune('a'+b/26)), ws[rng.Intn(len(ws))]))
	}
	return out // may contain self-loops and duplicates on purpose
}

func gridEdges(w, h int) []BridgeEdge {
	name := func(x, y int) string { return string(rune('a'+x)) + string(rune('a'+y)) }
	var out []BridgeEdge
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			if x+1 < w {
				out = append(out, be(name(x, y), name(x+1, y), 1))
			}
			if y+1 < h {
				out = append(out, be(name(x, y), name(x, y+1), 1))
			}
		}
	}
	return out
}

func graphScore561Fixtures() map[string][]BridgeEdge {
	return map[string][]BridgeEdge{
		"empty":    nil,
		"single":   {be("a", "b", 1)},
		"line":     {be("a", "b", 1), be("b", "c", 1), be("c", "d", 1)},
		"triangle": {be("a", "b", 1), be("b", "c", 1), be("a", "c", 1)},
		"star":     {be("h", "a", 1), be("h", "b", 1), be("h", "c", 1), be("h", "d", 1)},
		// Two equal shortest a→d paths through b and c.
		"diamond-tie": {be("a", "b", 1), be("a", "c", 1), be("b", "d", 1), be("c", "d", 1)},
		// Unequal weights break the diamond tie: a-b-d is shorter.
		"diamond-weighted": {be("a", "b", 1), be("a", "c", 0.5), be("b", "d", 1), be("c", "d", 0.5)},
		"disconnected": {be("a", "b", 1), be("b", "c", 1), be("x", "y", 1),
			be("p", "q", 0.5), be("q", "r", 0.5), be("r", "s", 0.5)},
		// Duplicates keep the cheapest distance (highest weight), in either
		// direction and either case.
		"duplicates": {be("a", "b", 0.25), be("B", "A", 1), be("a", "b", 0.5),
			be("b", "c", 1), be("c", "b", 0.125), be("a", "c", 0.25), be("a", "c", 0.25)},
		// Invalid inputs are dropped; boundary weights are kept.
		"invalid-and-boundary": {be("a", "b", 1), be("b", "c", 1), be("a", "a", 1),
			be("", "c", 1), be("c", " ", 1), be("a", "c", 0), be("a", "c", -1),
			be("a", "c", math.NaN()), be("a", "c", math.Inf(1)), be("a", "c", math.Inf(-1)),
			be("a", "c", 0.999e-9), be("c", "zz", 0.999e-9), be("c", "d", 1e-9),
			be("d", "e", 4), be("e", "f", 2)},
		"case-and-space": {be(" AbC ", "dEf", 1), be("def", "GHI", 0.5), be("ghi ", "abc", 0.25),
			be("ghi", "jkl", 1)},
		"grid-4x4":       gridEdges(4, 4),
		"random-ties-a":  tieWeightedRandomEdges(18, 45, 1),
		"random-ties-b":  tieWeightedRandomEdges(30, 70, 2),
		"random-sparse":  tieWeightedRandomEdges(40, 35, 3),
		"random-float-a": syntheticBridgeEdges(60, 180, 4),
		"random-float-b": syntheticBridgeEdges(150, 600, 5),
	}
}

func assertScoresClose(t *testing.T, label string, got, want map[string]float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nil map, want non-nil", label)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d keys, want %d (got=%v want=%v)", label, len(got), len(want), got, want)
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Fatalf("%s: missing key %q", label, k)
		}
		if math.IsNaN(g) || g < 0 || g > 1 || math.Abs(g-w) > graphScoreTol {
			t.Errorf("%s: score[%s] = %.17g, want %.17g (tol %g)", label, k, g, w, graphScoreTol)
		}
	}
}

// ─── semantic pins ───────────────────────────────────────────────────────────

func TestGraphScore561_BridgeMatchesOracle(t *testing.T) {
	for name, edges := range graphScore561Fixtures() {
		t.Run(name, func(t *testing.T) {
			assertScoresClose(t, "bridge", ComputeBridgeScores(edges), oracleBridge(edges))
		})
	}
}

func TestGraphScore561_CoverageMatchesOracle(t *testing.T) {
	for name, edges := range graphScore561Fixtures() {
		t.Run(name, func(t *testing.T) {
			assertScoresClose(t, "coverage", ComputeCoverageScores(edges), oracleCoverage(edges))
		})
	}
}

// TestGraphScore561_OracleSanity pins hand-derived values so a broken oracle
// cannot agree with a broken production implementation.
func TestGraphScore561_OracleSanity(t *testing.T) {
	fx := graphScore561Fixtures()
	// Line a-b-c-d: b and c each carry 2 of the pairs → both 1.0, ends 0.
	assertScoresClose(t, "oracle bridge line", oracleBridge(fx["line"]),
		map[string]float64{"a": 0, "b": 1, "c": 1, "d": 0})
	// Diamond tie: b and c split a↔d equally; a and d split b↔c equally.
	assertScoresClose(t, "oracle bridge diamond", oracleBridge(fx["diamond-tie"]),
		map[string]float64{"a": 1, "b": 1, "c": 1, "d": 1})
	// Weighted diamond (distances a-b 1, a-c 2, b-d 1, c-d 2): b alone carries
	// a↔d (2 vs 4); a and d split the b↔c tie (3 vs 3); c carries nothing.
	assertScoresClose(t, "oracle bridge weighted diamond", oracleBridge(fx["diamond-weighted"]),
		map[string]float64{"a": 0.5, "b": 1, "c": 0, "d": 0.5})
	// Line harmonic reach: ends 1+1/2+1/3, middles 1+1+1/2.
	assertScoresClose(t, "oracle coverage line", oracleCoverage(fx["line"]),
		map[string]float64{"a": (11.0 / 6) / 2.5, "b": 1, "c": 1, "d": (11.0 / 6) / 2.5})
	// Duplicates: a-b kept at weight 1 (dist 1), b-c weight 1, a-c 0.25 (dist 4
	// > 2 via b) → b is the only bridge.
	assertScoresClose(t, "oracle bridge duplicates", oracleBridge(fx["duplicates"]),
		map[string]float64{"a": 0, "b": 1, "c": 0})
}

// TestGraphScore561_GoldenBeforeAfter compares against scores captured from
// the pre-#561 map-based implementation (master 2e6fd9b0) on a fixed 400/2000
// graph. The golden file must not be regenerated from the new implementation.
func TestGraphScore561_GoldenBeforeAfter(t *testing.T) {
	raw, err := os.ReadFile("testdata/graph_scores_561_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Nodes, Edges int
		Seed         int64
		Keys         []string
		Bridge       []float64
		Coverage     []float64
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	edges := syntheticBridgeEdges(golden.Nodes, golden.Edges, golden.Seed)
	want := func(vals []float64) map[string]float64 {
		m := make(map[string]float64, len(vals))
		for i, k := range golden.Keys {
			m[k] = vals[i]
		}
		return m
	}
	assertScoresClose(t, "bridge golden", ComputeBridgeScores(edges), want(golden.Bridge))
	assertScoresClose(t, "coverage golden", ComputeCoverageScores(edges), want(golden.Coverage))
}

// ─── determinism ─────────────────────────────────────────────────────────────

func assertScoresBitwise(t *testing.T, label string, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d keys, want %d", label, len(got), len(want))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || math.Float64bits(g) != math.Float64bits(w) {
			t.Fatalf("%s: score[%s] = %.17g, want bit-identical %.17g", label, k, g, w)
		}
	}
}

// permuteEdges shuffles edge order, swaps endpoints and flips case — none of
// which changes the graph.
func permuteEdges(edges []BridgeEdge, rng *rand.Rand) []BridgeEdge {
	out := append([]BridgeEdge(nil), edges...)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	for i := range out {
		if rng.Intn(2) == 0 {
			out[i].A, out[i].B = out[i].B, out[i].A
		}
		if rng.Intn(2) == 0 {
			out[i].A = strings.ToUpper(out[i].A)
		}
	}
	return out
}

// TestGraphScore561_DeterministicAcrossRunsAndPermutations: identical graphs
// must produce bit-identical scores regardless of repetition or input order.
// The tie-weighted graph matters: with many equal shortest paths, Brandes'
// fractional dependencies are summed in an order set by vertex numbering.
func TestGraphScore561_DeterministicAcrossRunsAndPermutations(t *testing.T) {
	for name, edges := range map[string][]BridgeEdge{
		"continuous": syntheticBridgeEdges(120, 480, 7),
		"ties":       tieWeightedRandomEdges(90, 300, 7),
	} {
		t.Run(name, func(t *testing.T) {
			refBridge := ComputeBridgeScores(edges)
			refCov := ComputeCoverageScores(edges)
			rng := rand.New(rand.NewSource(561))
			for i := 0; i < 12; i++ {
				in := edges
				if i%2 == 1 {
					in = permuteEdges(edges, rng)
				}
				assertScoresBitwise(t, "bridge", ComputeBridgeScores(in), refBridge)
				assertScoresBitwise(t, "coverage", ComputeCoverageScores(in), refCov)
			}
		})
	}
}

// TestGraphScore561_NoStateAcrossCallsOrSizes: alternating large/small/large
// graphs (and repeating each) must reproduce the first fresh result exactly —
// catches any workspace or buffer reused across calls or graph sizes.
func TestGraphScore561_NoStateAcrossCallsOrSizes(t *testing.T) {
	big := syntheticBridgeEdges(200, 800, 8)
	small := graphScore561Fixtures()["diamond-tie"]
	mid := syntheticBridgeEdges(90, 200, 9)
	type ref struct{ b, c map[string]float64 }
	refs := map[string]ref{}
	inputs := map[string][]BridgeEdge{"big": big, "small": small, "mid": mid}
	for _, name := range []string{"big", "small", "mid", "small", "big", "mid", "big"} {
		b, c := ComputeBridgeScores(inputs[name]), ComputeCoverageScores(inputs[name])
		if r, ok := refs[name]; ok {
			assertScoresBitwise(t, name+" bridge", b, r.b)
			assertScoresBitwise(t, name+" coverage", c, r.c)
		} else {
			refs[name] = ref{b, c}
		}
	}
	assertScoresClose(t, "small bridge vs oracle", refs["small"].b, oracleBridge(small))
	assertScoresClose(t, "small coverage vs oracle", refs["small"].c, oracleCoverage(small))
}

// TestGraphScore561_ConcurrentComputations: concurrent callers sharing one
// input slice must neither race (run with -race) nor perturb each other.
func TestGraphScore561_ConcurrentComputations(t *testing.T) {
	edgesA := syntheticBridgeEdges(80, 240, 10)
	edgesB := graphScore561Fixtures()["grid-4x4"]
	wantA, wantB := oracleBridge(edgesA), oracleBridge(edgesB)
	wantCA, wantCB := oracleCoverage(edgesA), oracleCoverage(edgesB)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in, wb, wc := edgesA, wantA, wantCA
			if i%2 == 1 {
				in, wb, wc = edgesB, wantB, wantCB
			}
			for r := 0; r < 3; r++ {
				for k, v := range ComputeBridgeScores(in) {
					if math.Abs(v-wb[k]) > graphScoreTol {
						errs <- "bridge " + k
					}
				}
				for k, v := range ComputeCoverageScores(in) {
					if math.Abs(v-wc[k]) > graphScoreTol {
						errs <- "coverage " + k
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent mismatch: %s", e)
	}
}

// ─── allocation budget ───────────────────────────────────────────────────────

// TestGraphScore561_AllocationBudget bounds allocations per computation below
// V/2 on a 600/2000 graph: any allocation made once per source vertex costs
// at least V and fails, so per-computation state must be O(V+E) slices that
// are reused across sources. Before #561 the map-per-source implementation
// allocated ~1.46M (bridge) / ~1.11M (coverage) objects here.
func TestGraphScore561_AllocationBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation budget runs a 600/2000 all-sources computation")
	}
	const v, e = 600, 2000
	edges := syntheticBridgeEdges(v, e, 561)
	budget := float64(v / 2)
	if got := testing.AllocsPerRun(2, func() { _ = ComputeBridgeScores(edges) }); got > budget {
		t.Errorf("ComputeBridgeScores: %.0f allocs/op, budget %.0f (< 1 per source)", got, budget)
	}
	if got := testing.AllocsPerRun(2, func() { _ = ComputeCoverageScores(edges) }); got > budget {
		t.Errorf("ComputeCoverageScores: %.0f allocs/op, budget %.0f (< 1 per source)", got, budget)
	}
}

// ─── reusable workspace isolation ────────────────────────────────────────────

// poisonBrandesWorkspace fills every reusable slot with garbage that a
// missing reset would turn into wrong scores (or an out-of-row index).
func poisonBrandesWorkspace(ws *brandesWorkspace) {
	for i := range ws.dist {
		ws.dist[i], ws.sigma[i], ws.delta[i] = -1, math.NaN(), math.NaN()
		ws.settled[i] = true
		ws.predLen[i] = 1
	}
	for i := range ws.predBuf {
		ws.predBuf[i] = 0
	}
	ws.stack = append(ws.stack, 0)
	ws.heap = append(ws.heap, distHeapItem{node: 0, dist: -1})
}

func bridgeRawWith(g *indexedDistanceGraph, ws *brandesWorkspace, poison bool) []float64 {
	bc := make([]float64, g.numVertices())
	for s := range bc {
		ws.accumulate(g, int32(s), bc)
		if poison {
			poisonBrandesWorkspace(ws)
		}
	}
	return bc
}

func coverageRawWith(g *indexedDistanceGraph, ws *ssspWorkspace, poison bool) []float64 {
	out := make([]float64, g.numVertices())
	for s := range out {
		out[s] = ws.harmonicReach(g, int32(s))
		if poison {
			for i := range ws.dist {
				ws.dist[i], ws.settled[i] = -1, true
			}
			ws.heap = append(ws.heap, distHeapItem{node: 0, dist: -1})
		}
	}
	return out
}

func assertRawBitwise(t *testing.T, label string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len %d, want %d", label, len(got), len(want))
	}
	for i := range want {
		if math.Float64bits(got[i]) != math.Float64bits(want[i]) {
			t.Fatalf("%s: [%d] = %.17g, want %.17g", label, i, got[i], want[i])
		}
	}
}

// TestGraphScore561_WorkspaceReuseIsStateless reuses ONE workspace across
// sources and across graphs of different sizes (shrink and grow), poisoning
// it after every source, and requires bit-identical results to a fresh
// workspace per graph — and, so that a reset bug shared by both cannot pass,
// the fresh result must match the independent oracle.
func TestGraphScore561_WorkspaceReuseIsStateless(t *testing.T) {
	inputs := [][]BridgeEdge{
		syntheticBridgeEdges(120, 400, 11),
		graphScore561Fixtures()["diamond-tie"],
		graphScore561Fixtures()["disconnected"],
		syntheticBridgeEdges(200, 900, 12),
	}
	var bws brandesWorkspace
	var cws ssspWorkspace
	for round := 0; round < 2; round++ {
		for i, edges := range inputs {
			g := newIndexedDistanceGraph(edges)
			var freshB brandesWorkspace
			var freshC ssspWorkspace
			wantB, wantC := bridgeRawWith(g, &freshB, false), coverageRawWith(g, &freshC, false)
			assertRawBitwise(t, fmt.Sprintf("bridge graph %d round %d", i, round),
				bridgeRawWith(g, &bws, true), wantB)
			assertRawBitwise(t, fmt.Sprintf("coverage graph %d round %d", i, round),
				coverageRawWith(g, &cws, true), wantC)
			if round == 0 {
				normalizeByMax(wantB)
				normalizeByMax(wantC)
				assertScoresClose(t, fmt.Sprintf("fresh bridge graph %d vs oracle", i), g.scoreMap(wantB), oracleBridge(edges))
				assertScoresClose(t, fmt.Sprintf("fresh coverage graph %d vs oracle", i), g.scoreMap(wantC), oracleCoverage(edges))
			}
		}
	}
}

// TestGraphScore561_IndexedGraphStructure pins the CSR build: sorted ids,
// symmetric rows sorted by neighbor, cheapest duplicate kept, invalid edges
// dropped.
func TestGraphScore561_IndexedGraphStructure(t *testing.T) {
	g := newIndexedDistanceGraph(graphScore561Fixtures()["duplicates"])
	if fmt.Sprint(g.keys) != "[a b c]" {
		t.Fatalf("keys = %v, want [a b c]", g.keys)
	}
	type arc struct {
		from, to string
		cost     float64
	}
	var got []arc
	for v := range g.keys {
		for k := g.off[v]; k < g.off[v+1]; k++ {
			got = append(got, arc{g.keys[v], g.keys[g.nbr[k]], g.cost[k]})
		}
	}
	want := []arc{{"a", "b", 1}, {"a", "c", 4}, {"b", "a", 1}, {"b", "c", 1}, {"c", "a", 4}, {"c", "b", 1}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("arcs = %v, want %v", got, want)
	}
	if g := newIndexedDistanceGraph(graphScore561Fixtures()["invalid-and-boundary"]); fmt.Sprint(g.keys) != "[a b c d e f]" {
		t.Fatalf("invalid-and-boundary keys = %v, want [a b c d e f]", g.keys)
	}
}

// ─── readiness (unchanged contract) ──────────────────────────────────────────

// TestGraphScore561_StartInstallsScoresBeforeReturn pins the readiness
// contract #561 must not weaken: both Start* recomputers return only after a
// populated snapshot is installed, so the first /api/nodes after
// ListenAndServe never sees zero scores presented as finished analytics.
func TestGraphScore561_StartInstallsScoresBeforeReturn(t *testing.T) {
	line := []BridgeEdge{be("aa", "bb", 1), be("bb", "cc", 1), be("cc", "dd", 1)}
	s := &PacketStore{}
	s.graph.Store(neighborGraphFromBridgeEdges(line, time.Now()))

	resetBridgeRecomputerForTest()
	stopBridge := s.StartBridgeScoreRecomputer(time.Hour)
	bridgeSnap := s.bridgeScoreMap.Load()
	stopBridge()
	resetBridgeRecomputerForTest()
	if bridgeSnap == nil {
		t.Fatal("StartBridgeScoreRecomputer returned before installing a bridge snapshot")
	}
	if (*bridgeSnap)["bb"] != 1 || (*bridgeSnap)["cc"] != 1 || (*bridgeSnap)["aa"] != 0 {
		t.Fatalf("initial bridge snapshot = %v, want bb=cc=1 aa=0", *bridgeSnap)
	}

	stopAxes := s.StartUsefulnessAxesRecomputer(time.Hour)
	computed := s.UsefulnessAxesComputed()
	covSnap := s.coverageScoreMap.Load()
	stopAxes()
	if !computed || covSnap == nil {
		t.Fatal("StartUsefulnessAxesRecomputer returned before installing axis snapshots")
	}
	if (*covSnap)["bb"] != 1 || (*covSnap)["aa"] <= 0 {
		t.Fatalf("initial coverage snapshot = %v, want bb=1 aa>0", *covSnap)
	}
}
