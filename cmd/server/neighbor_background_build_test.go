package main

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func neighborBackgroundTestStore() *PacketStore {
	return ngTestStore([]nodeInfo{
		{PublicKey: "aaaa1111", Role: "repeater"},
		{PublicKey: "bbbb2222", Role: "repeater"},
		{PublicKey: "cccc3333", Role: "repeater"},
	}, []*StoreTx{ngMakeTx(1, PayloadADVERT, ngFromNodeJSON("aaaa1111"), []*StoreObs{
		ngMakeObs("cccc3333", `["bbbb"]`, "2026-10-01T12:00:00Z", nil),
	})})
}

func TestNeighborGraphExistingStaleSnapshotDoesNotRebuild(t *testing.T) {
	old := NewNeighborGraph()
	old.builtAt = time.Now().Add(-2 * neighborGraphTTL)
	srv := &Server{store: neighborBackgroundTestStore(), neighborGraph: old}
	if got := srv.getNeighborGraph(); got != old {
		t.Fatal("request rebuilt an existing stale graph instead of serving it")
	}
}

func TestNeighborGraphTickRebuildsEvenFreshSnapshot(t *testing.T) {
	old := NewNeighborGraph()
	old.builtAt = time.Now()
	srv := &Server{
		store:         neighborBackgroundTestStore(),
		neighborGraph: old,
		computeNeighborGraphResponseFn: func(int, float64, string, string) NeighborGraphResponse {
			return NeighborGraphResponse{}
		},
	}
	srv.recomputeNeighborGraphCache()
	if got := srv.getNeighborGraph(); got == old || len(got.AllEdges()) == 0 {
		t.Fatal("tick did not replace a fresh graph with a newly built graph")
	}
}

// Hold the real SQL pool so the expensive build remains in flight. Serving
// the old graph must not wait for that build or hold the pointer mutex for it.
func TestNeighborGraphReadersContinueDuringBlockedRebuild(t *testing.T) {
	srv, _ := setupTestServer(t)
	old := NewNeighborGraph()
	old.builtAt = time.Now().Add(-2 * neighborGraphTTL)
	srv.neighborGraph = old
	srv.store.InvalidateNodeCache()
	conn, err := srv.db.conn.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	buildDone := make(chan struct{})
	readDone := make(chan *NeighborGraph, 1)
	readerStarted := false
	t.Cleanup(func() {
		conn.Close()
		select {
		case <-buildDone:
		case <-time.After(5 * time.Second):
			t.Error("build did not finish after releasing SQL")
		}
		if readerStarted {
			select {
			case <-readDone:
			case <-time.After(5 * time.Second):
				t.Error("reader did not finish after releasing SQL")
			}
		}
	})
	waits := srv.db.conn.Stats().WaitCount
	go func() {
		defer close(buildDone)
		srv.recomputeNeighborGraphCache()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for srv.db.conn.Stats().WaitCount == waits {
		select {
		case <-buildDone:
			t.Fatal("recompute did not attempt a graph build")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("build never reached the occupied SQL pool")
		}
		runtime.Gosched()
	}
	readerStarted = true
	go func() { readDone <- srv.getNeighborGraph() }()
	select {
	case got := <-readDone:
		readerStarted = false
		if got != old {
			t.Fatal("reader did not receive the old graph during the blocked build")
		}
	case <-time.After(time.Second):
		t.Fatal("reader waited for the background graph build")
	}
}

func TestNeighborGraphConcurrentFirstReadersShareBuild(t *testing.T) {
	srv := &Server{store: neighborBackgroundTestStore()}
	const readers = 16
	start := make(chan struct{})
	results := make(chan *NeighborGraph, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- srv.getNeighborGraph()
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first *NeighborGraph
	for got := range results {
		if first == nil {
			first = got
		}
		if got == nil || got != first || len(got.AllEdges()) == 0 {
			t.Fatal("concurrent initial requests did not share one populated graph")
		}
	}
}

func TestNeighborGraphRecomputeWithoutStoreKeepsInjectedGraph(t *testing.T) {
	injected := NewNeighborGraph()
	srv := &Server{
		neighborGraph: injected,
		computeNeighborGraphResponseFn: func(int, float64, string, string) NeighborGraphResponse {
			return NeighborGraphResponse{}
		},
	}
	srv.recomputeNeighborGraphCache()
	if srv.getNeighborGraph() != injected {
		t.Fatal("recompute discarded an injected graph without a packet store")
	}
}

func TestNeighborGraphFailedRebuildKeepsPublishedSnapshot(t *testing.T) {
	old := NewNeighborGraph()
	old.builtAt = time.Now()
	srv := &Server{
		store:         ngTestStore(nil, []*StoreTx{nil}), // fault injection: graph build panics
		neighborGraph: old,
	}
	srv.recomputeNeighborGraphCache()
	if got := srv.getNeighborGraph(); got != old {
		t.Fatal("failed background build replaced the last published graph")
	}
	if failures := atomic.LoadUint64(&srv.neighborGraphCacheRebuildFailures); failures != 1 {
		t.Fatalf("rebuild failures = %d, want 1", failures)
	}
}

func BenchmarkNeighborGraphStaleRead30K(b *testing.B) {
	store := neighborBackgroundTestStore()
	packet := store.packets[0]
	store.packets = make([]*StoreTx, 30000)
	for i := range store.packets {
		store.packets[i] = ngMakeTx(i+1, PayloadADVERT, packet.DecodedJSON, packet.Observations)
	}
	old := NewNeighborGraph()
	old.builtAt = time.Now().Add(-2 * neighborGraphTTL)
	srv := &Server{store: store, neighborGraph: old}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		srv.neighborMu.Lock()
		srv.neighborGraph = old
		srv.neighborMu.Unlock()
		srv.getNeighborGraph()
	}
}
