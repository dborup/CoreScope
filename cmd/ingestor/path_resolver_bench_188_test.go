package main

import (
	"fmt"
	"math/rand"
	"testing"
)

// BenchmarkResolveObservationPath_188 measures resolveObservationPath, the
// per-observation call in InsertTransmission, on a synthetic mesh: 700
// relays, 60 observers, 1-byte hops, flood paths of 1-5 hops built as walks
// over the neighbour graph that end at a neighbour of the observer. One op is
// one observation.
func BenchmarkResolveObservationPath_188(b *testing.B) {
	rng := rand.New(rand.NewSource(188))
	key := func() string {
		buf := make([]byte, 32)
		rng.Read(buf)
		return fmt.Sprintf("%x", buf)
	}
	relays := make([]string, 700)
	for i := range relays {
		relays[i] = key()
	}
	observers := make([]string, 60)
	for i := range observers {
		observers[i] = key()
	}
	idx := idx188(relays...)
	g := NewNeighborGraph()
	nbrs := map[string][]string{}
	link := func(a, c string) {
		g.AddEdge(a, c)
		nbrs[a] = append(nbrs[a], c)
		nbrs[c] = append(nbrs[c], a)
	}
	for _, r := range relays {
		for k := 0; k < 3; k++ {
			link(r, relays[rng.Intn(len(relays))])
		}
	}
	for _, o := range observers {
		for k := 0; k < 8; k++ {
			link(o, relays[rng.Intn(len(relays))])
		}
	}

	type obs struct {
		hops     []string
		observer string
	}
	sample := make([]obs, 4096)
	for i := range sample {
		o := observers[rng.Intn(len(observers))]
		cur := nbrs[o][rng.Intn(len(nbrs[o]))]
		n := 1 + rng.Intn(5)
		hops := make([]string, n)
		for j := n - 1; j >= 0; j-- {
			hops[j] = cur[:2]
			next := nbrs[cur]
			cur = next[rng.Intn(len(next))]
		}
		sample[i] = obs{hops, o}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := sample[i%len(sample)]
		resolveObservationPath(s.hops, "", s.observer, routeFlood188, g, idx)
	}
}
