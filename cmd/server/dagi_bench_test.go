package main

// #740 M1 — DAGI perf proof (AGENTS.md rule 0): a full rebuild of the
// membership cache over all four layers at 2K and 20K nodes, a refresh
// with nothing moved, one lookup, and the per-packet membership check.
//
//	go test -run '^$' -bench Dagi -benchmem
//
// TestDagiRefreshWithinBudget enforces the rebuild cost in every run.

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// dagiSyntheticNodes spreads n nodes uniformly over Denmark's bbox (lat
// 54.55–57.80, lon 8.05–15.15): about half on land, the rest at sea or
// abroad, which is the expensive case (every candidate is tested and none
// matches). Seeded, so runs compare.
func dagiSyntheticNodes(n int, seed int64) []nodeInfo {
	r := rand.New(rand.NewSource(seed))
	out := make([]nodeInfo, n)
	for i := range out {
		out[i] = nodeInfo{
			PublicKey: fmt.Sprintf("%064x", i),
			Lat:       54.55 + r.Float64()*3.25,
			Lon:       8.05 + r.Float64()*7.1,
			HasGPS:    true,
		}
	}
	return out
}

func benchmarkDagiRefreshFull(b *testing.B, n int) {
	idx := dagiTestIndex(b)
	nodes := dagiSyntheticNodes(n, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		newDagiMembership(idx).Refresh(nodes)
	}
}

func BenchmarkDagiRefreshFull2K(b *testing.B)  { benchmarkDagiRefreshFull(b, 2000) }
func BenchmarkDagiRefreshFull20K(b *testing.B) { benchmarkDagiRefreshFull(b, 20000) }

// Nothing moved: every node reuses its membership, no lookup runs.
func BenchmarkDagiRefreshUnchanged20K(b *testing.B) {
	m := newDagiMembership(dagiTestIndex(b))
	nodes := dagiSyntheticNodes(20000, 1)
	m.Refresh(nodes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Refresh(nodes)
	}
}

// One position, all four layers.
func BenchmarkDagiLookup(b *testing.B) {
	idx := dagiTestIndex(b)
	nodes := dagiSyntheticNodes(1024, 2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := &nodes[i&1023]
		idx.locate(n.Lat, n.Lon)
	}
}

// The per-packet check a filter makes: is the sender in kommune X.
func BenchmarkDagiContains(b *testing.B) {
	m := newDagiMembership(dagiTestIndex(b))
	nodes := dagiSyntheticNodes(20000, 1)
	m.Refresh(nodes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Contains(DagiKommune, "0751", nodes[i%len(nodes)].PublicKey)
	}
}

// Budgets: the best of five full rebuilds. Measured on a 4-core dev VM
// (Go 1.27, plain binary): 2K ≈ 7 ms, 20K ≈ 70 ms; the caps are ~7x that,
// so a constant-factor regression or an accidental O(n²) fails and runner
// noise does not. -race instruments every memory access and gets its own
// multiple. The scaling bound catches a superlinear rebuild even under
// the cap: 10x the nodes must cost under 20x the time.
const (
	dagiBudget2K       = 50 * time.Millisecond
	dagiBudget20K      = 500 * time.Millisecond
	dagiRaceFactor     = 12
	dagiMaxScaleFactor = 20.0
)

func dagiBestRefresh(t *testing.T, idx *dagiIndex, nodes []nodeInfo) time.Duration {
	best := time.Duration(1<<63 - 1)
	for round := 0; round < 5; round++ {
		m := newDagiMembership(idx)
		start := time.Now()
		st := m.Refresh(nodes)
		if d := time.Since(start); d < best {
			best = d
		}
		if st.Located != len(nodes) {
			t.Fatalf("located %d of %d", st.Located, len(nodes))
		}
	}
	return best
}

func TestDagiRefreshWithinBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	idx := dagiTestIndex(t)
	factor := time.Duration(1)
	if raceBuild {
		factor = dagiRaceFactor
	}
	small := dagiBestRefresh(t, idx, dagiSyntheticNodes(2000, 1))
	big := dagiBestRefresh(t, idx, dagiSyntheticNodes(20000, 1))
	t.Logf("full rebuild, 4 layers: 2K nodes %v, 20K nodes %v (race %v)", small, big, raceBuild)
	if small > dagiBudget2K*factor {
		t.Errorf("2K rebuild %v, budget %v", small, dagiBudget2K*factor)
	}
	if big > dagiBudget20K*factor {
		t.Errorf("20K rebuild %v, budget %v", big, dagiBudget20K*factor)
	}
	if scale := float64(big) / float64(small); scale > dagiMaxScaleFactor {
		t.Errorf("10x the nodes cost %.1fx the time, bound %.0fx", scale, dagiMaxScaleFactor)
	}
}
