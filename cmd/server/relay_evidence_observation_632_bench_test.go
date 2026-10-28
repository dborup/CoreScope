package main

import "testing"

// relayBench632Store is the #76 fixture (2,000 repeaters, 30K transmissions,
// 150K observations) with every observation carrying its own frame route,
// one in five DIRECT, so relay evidence takes the per-observation path (#632).
func relayBench632Store(b *testing.B) (*PacketStore, []string) {
	store, keys := heardByBenchStore(b, 2000, 30000, 5)
	for _, tx := range store.packets {
		for k, o := range tx.Observations {
			o.wireRoute = RouteFlood + 1
			if (tx.ID+k)%5 == 0 {
				o.wireRoute = RouteDirect + 1
			}
		}
	}
	return store, keys
}

// BenchmarkRelayEvidenceBulkRelayMap30K_632 is the /api/nodes relay map
// (last_relayed for every repeater) over the whole fixture.
func BenchmarkRelayEvidenceBulkRelayMap30K_632(b *testing.B) {
	store, keys := relayBench632Store(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := store.computeRepeaterRelayInfoMap(24); len(got) == 0 || got[keys[1]].LastRelayed == "" {
			b.Fatal("no relay evidence")
		}
	}
}

// BenchmarkRelayEvidenceNodeHealthHub30K_632 is the worst single node request:
// a hub indexed under every transmission.
func BenchmarkRelayEvidenceNodeHealthHub30K_632(b *testing.B) {
	store, keys := relayBench632Store(b)
	for _, tx := range store.packets {
		store.addToByNode(tx, keys[0])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := store.GetRepeaterRelayInfo(keys[0], 24); got.LastRelayed == "" {
			b.Fatal("no relay evidence")
		}
	}
}
