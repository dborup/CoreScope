package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// Synthetic Route Patterns data for #413: numNodes nodes, a quarter of them
// repeaters wired into a sparse neighbour mesh, and numTx transmissions whose
// paths are random walks over that mesh. Path lengths follow the hop-count
// distribution of test-fixtures/e2e-fixture.db (median 7, tail to 37), hops
// are upper-case prefixes like the decoder writes, 1-byte for most packets and
// 2-byte for the rest, so prefixes collide the way they do on a real mesh.
// Seeded, so every run (and the before/after benchmark) sees the same data.

// subpath413PathLens is the fixture's path-length histogram: weight per length.
var subpath413PathLens = map[int]int{
	0: 101, 1: 28, 2: 9, 3: 2, 4: 16, 5: 49, 6: 65, 7: 63, 8: 57, 9: 33,
	10: 28, 11: 13, 12: 9, 13: 9, 14: 4, 15: 3, 16: 1, 17: 1, 18: 1, 19: 3,
	20: 1, 22: 2, 31: 1, 37: 1,
}

func subpath413Data(numTx, numNodes int, seed int64) ([]nodeInfo, []*StoreTx) {
	rng := rand.New(rand.NewSource(seed))
	nodes := make([]nodeInfo, numNodes)
	for i := range nodes {
		pk := make([]byte, 32)
		rng.Read(pk)
		role := "companion"
		if i%4 == 0 {
			role = "repeater"
		}
		nodes[i] = nodeInfo{
			PublicKey: fmt.Sprintf("%x", pk), Name: fmt.Sprintf("Node %04d", i), Role: role,
			Lat: 55 + rng.Float64(), Lon: 12 + rng.Float64(), HasGPS: true,
			ObservationCount: rng.Intn(500),
		}
	}
	var repeaters []int
	for i, n := range nodes {
		if n.Role == "repeater" {
			repeaters = append(repeaters, i)
		}
	}
	// Each repeater has 3-8 neighbours.
	neigh := make(map[int][]int, len(repeaters))
	for _, r := range repeaters {
		for k := 3 + rng.Intn(6); k > 0; k-- {
			o := repeaters[rng.Intn(len(repeaters))]
			if o != r {
				neigh[r] = append(neigh[r], o)
				neigh[o] = append(neigh[o], r)
			}
		}
	}

	var lens []int
	for l, w := range subpath413PathLens {
		for ; w > 0; w-- {
			lens = append(lens, l)
		}
	}
	// Map iteration above is random; sort so the seed alone fixes the data.
	sort.Ints(lens)

	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	packets := make([]*StoreTx, numTx)
	for i := range packets {
		hopBytes := 1
		if rng.Intn(5) == 0 {
			hopBytes = 2
		}
		n := lens[rng.Intn(len(lens))]
		hops := make([]string, n)
		cur := repeaters[rng.Intn(len(repeaters))]
		for h := range hops {
			hops[h] = strings.ToUpper(nodes[cur].PublicKey[:2*hopBytes])
			if nb := neigh[cur]; len(nb) > 0 {
				cur = nb[rng.Intn(len(nb))]
			}
		}
		path, _ := json.Marshal(hops)
		sender := nodes[rng.Intn(numNodes)]
		observer := nodes[repeaters[rng.Intn(len(repeaters))]]
		pt := PayloadTXT_MSG
		if rng.Intn(4) == 0 {
			pt = PayloadADVERT
		}
		snr := float64(rng.Intn(40)-20) / 2
		rssi := float64(-60 - rng.Intn(60))
		packets[i] = &StoreTx{
			ID: i + 1, Hash: fmt.Sprintf("%016x", rng.Uint64()),
			FirstSeen:   t0.Add(time.Duration(i) * 20 * time.Second).Format(time.RFC3339),
			PayloadType: &pt, PathJSON: string(path),
			DecodedJSON: fmt.Sprintf(`{"pubKey":%q}`, sender.PublicKey),
			ObserverID:  observer.PublicKey, ObserverName: observer.Name,
			SNR: &snr, RSSI: &rssi,
		}
	}
	return nodes, packets
}

// newSubpath413Store is a DB-less store holding packets, with a node cache
// that does not expire during a long benchmark.
func newSubpath413Store(nodes []nodeInfo, packets []*StoreTx) *PacketStore {
	s := newTieOrderStore(packets)
	for _, tx := range packets {
		s.byHash[tx.Hash] = tx
		s.byTxID[tx.ID] = tx
	}
	s.nodeCache = nodes
	s.nodePM = buildPrefixMap(nodes)
	s.nodeCacheTime = time.Now().Add(time.Hour)
	s.rfCacheTTL = time.Minute
	return s
}

// subpath413DetailKeys picks two subpath-detail queries from the data: the
// most frequent 2-hop subpath (a hot repeater pair, ties to the smallest key)
// and a 5-hop subpath taken from the first packet long enough to hold one.
func subpath413DetailKeys(packets []*StoreTx) (hot, cold []string) {
	counts := map[string]int{}
	for _, tx := range packets {
		hops := txGetParsedPath(tx)
		for i := 0; i+2 <= len(hops); i++ {
			counts[strings.ToLower(hops[i]+","+hops[i+1])]++
		}
		if cold == nil && len(hops) >= 5 {
			cold = append([]string(nil), hops[:5]...)
		}
	}
	best := ""
	for k, n := range counts {
		if n > counts[best] || (n == counts[best] && k < best) {
			best = k
		}
	}
	return strings.Split(best, ","), cold
}
