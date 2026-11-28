package main

import (
	"fmt"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Benchmarks for the #470 round-2 readers at the documented scale (30K
// transmissions). Besides ns/op they report how long a writer waits for
// s.mu.Lock while the reader runs: that is the stall ingest sees, and what
// moving work under the read lock could make worse. A writer that polls the
// lock waits about once per read, so the median of the b.N longest waits
// (writer-stall-µs) is the typical stall one read causes; the longest wait
// (writer-wait-max-µs) also catches GC and scheduler pauses.

const bench470Txs = 30000

// benchWriterWait470 runs read b.N times while a writer takes s.mu.Lock in a
// loop, and reports the writer's wait for the lock.
func benchWriterWait470(b *testing.B, s *PacketStore, read func()) {
	b.Helper()
	var stop atomic.Bool
	var waits []time.Duration
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			t0 := time.Now()
			s.mu.Lock()
			waits = append(waits, time.Since(t0))
			s.mu.Unlock()
			time.Sleep(50 * time.Microsecond)
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		read()
	}
	b.StopTimer()
	stop.Store(true)
	wg.Wait()
	if len(waits) == 0 {
		return
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] > waits[j] })
	top := waits[:min(b.N, len(waits))]
	b.ReportMetric(float64(top[len(top)/2].Microseconds()), "writer-stall-µs")
	b.ReportMetric(float64(waits[0].Microseconds()), "writer-wait-max-µs")
}

func BenchmarkNeighborGraphBuild30K470(b *testing.B) {
	nodes := []nodeInfo{
		{Role: "repeater", PublicKey: "aaaa1111", Name: "NodeA"},
		{Role: "repeater", PublicKey: "bbbb2222", Name: "NodeB"},
		{Role: "repeater", PublicKey: "cccc3333", Name: "NodeC"},
	}
	pt := PayloadADVERT
	packets := make([]*StoreTx, bench470Txs)
	for i := range packets {
		packets[i] = &StoreTx{ID: i + 1, PayloadType: &pt, DecodedJSON: `{"pubKey":"aaaa1111"}`, Observations: []*StoreObs{
			{ObserverID: "bbbb2222", PathJSON: `["cccc"]`, Timestamp: nowStr, SNR: ngFloatPtr(-5)},
			{ObserverID: "cccc3333", PathJSON: `["aaaa","bbbb"]`, Timestamp: nowStr, SNR: ngFloatPtr(-7)},
			{ObserverID: "bbbb2222", PathJSON: `[]`, Timestamp: nowStr},
		}}
	}
	store := ngTestStore(nodes, packets)
	benchWriterWait470(b, store, func() { BuildFromStoreWithOptions(store, BuildOptions{}) })
}

func BenchmarkAudioLabBuckets30K470(b *testing.B) {
	store := NewPacketStore(nil, nil)
	types := []string{"ADVERT", "GRP_TXT", "TXT_MSG", "REQ", "ACK"}
	for i := 0; i < bench470Txs; i++ {
		pt := i % len(types)
		store.packets = append(store.packets, &StoreTx{
			ID: i + 1, Hash: fmt.Sprintf("%016x", i+1), FirstSeen: nowStr, PayloadType: &pt,
			RawHex:      fmt.Sprintf("%0*x", 20+2*(i%40), i),
			DecodedJSON: fmt.Sprintf(`{"type":%q,"text":"benchmark payload %d"}`, types[pt], i),
			PathJSON:    `["aa","bb"]`, ObserverID: "OBS1", ObservationCount: 2,
		})
	}
	srv := &Server{store: store}
	benchWriterWait470(b, store, func() {
		srv.handleAudioLabBuckets(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/audio-lab/buckets", nil))
	})
}

func BenchmarkObserverAnalytics30K470(b *testing.B) {
	store := NewPacketStore(nil, nil)
	now := time.Now().UTC()
	rt := 1
	for i := 0; i < bench470Txs; i++ {
		pt := i % 6
		tx := &StoreTx{ID: i + 1, Hash: fmt.Sprintf("%016x", i+1), RawHex: "aabb", PayloadType: &pt, RouteType: &rt,
			DecodedJSON: `{"pubKey":"aaaa1111"}`}
		obs := &StoreObs{ID: i + 1, TransmissionID: tx.ID, ObserverID: "OBS1", PathJSON: `["aa","bb"]`,
			Timestamp: now.Add(-time.Duration(i) * 10 * time.Second).Format(time.RFC3339), SNR: ngFloatPtr(float64(i%20 - 10))}
		tx.Observations = []*StoreObs{obs}
		store.packets = append(store.packets, tx)
		store.byTxID[tx.ID] = tx
		store.byObserver["OBS1"] = append(store.byObserver["OBS1"], obs)
	}
	srv := &Server{store: store}
	benchWriterWait470(b, store, func() {
		req := mux.SetURLVars(httptest.NewRequest("GET", "/api/observers/OBS1/analytics?days=7", nil), map[string]string{"id": "OBS1"})
		srv.handleObserverAnalytics(httptest.NewRecorder(), req)
	})
}
