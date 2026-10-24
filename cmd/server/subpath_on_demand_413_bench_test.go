package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Benchmarks for #413 on 30,000 transmissions over 2,000 nodes
// (subpath413Data). The on-demand benchmarks time what a request does now;
// the legacy ones time what the removed index cost on every ingest, eviction
// and rebuild, and how much heap it held, using its frozen copy
// (legacy413Index). Run with:
//
//	go test -run '^$' -bench '_413' -benchtime 2s .

const (
	bench413Tx    = 30000
	bench413Nodes = 2000
)

func bench413Store(b *testing.B) *PacketStore {
	b.Helper()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	nodes, packets := subpath413Data(bench413Tx, bench413Nodes, 413)
	s := newSubpath413Store(nodes, packets)
	for _, tx := range packets {
		txGetParsedPath(tx)
		tx.ParsedDecoded()
	}
	return s
}

// The Route Patterns tab's four length groups.
var bench413Groups = []subpathGroup{{2, 2, 50}, {3, 3, 30}, {4, 4, 20}, {5, 8, 15}}

func BenchmarkSubpathsOnDemand_413(b *testing.B) {
	s := bench413Store(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.computeAnalyticsSubpaths("", 2, 8, 100)
	}
}

func BenchmarkSubpathsBulkOnDemand_413(b *testing.B) {
	s := bench413Store(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.GetAnalyticsSubpathsBulk("", bench413Groups)
		clearSubpathCache413(s)
	}
}

// The part of a subpaths request that holds PacketStore.mu.
func BenchmarkSubpathsLockedSnapshot_413(b *testing.B) {
	s := bench413Store(b)
	_, pm := s.getCachedNodesAndPM()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.snapshotSubpathPaths(nil, pm)
	}
}

func BenchmarkSubpathDetailOnDemand_413(b *testing.B) {
	s := bench413Store(b)
	hot, cold := subpath413DetailKeys(s.packets)
	for _, c := range []struct {
		name string
		hops []string
	}{{"hot", hot}, {"cold", cold}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s.GetSubpathDetail(c.hops)
			}
		})
	}
}

// BenchmarkLegacySubpathIndex_413 times the removed index's upkeep: adding one
// transmission (ingest), removing one (eviction, a linear scan of each of its
// subpaths' transmission lists) and the full rebuild of Load and the chunk
// loader. heap-MB is the heap the built index held.
func BenchmarkLegacySubpathIndex_413(b *testing.B) {
	s := bench413Store(b)
	b.Run("ingest", func(b *testing.B) {
		x := newLegacy413Index(s.packets)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tx := s.packets[i%len(s.packets)]
			b.StopTimer()
			x.remove(tx, txGetParsedPath(tx))
			b.StartTimer()
			x.add(tx, txGetParsedPath(tx))
		}
	})
	b.Run("evict", func(b *testing.B) {
		x := newLegacy413Index(s.packets)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tx := s.packets[i%len(s.packets)]
			x.remove(tx, txGetParsedPath(tx))
			b.StopTimer()
			x.add(tx, txGetParsedPath(tx))
			b.StartTimer()
		}
	})
	b.Run("build", func(b *testing.B) {
		var before, after runtime.MemStats
		var x *legacy413Index
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			x = nil
			b.StopTimer()
			runtime.GC()
			runtime.ReadMemStats(&before)
			b.StartTimer()
			x = newLegacy413Index(s.packets)
		}
		b.StopTimer()
		runtime.GC()
		runtime.ReadMemStats(&after)
		entries := 0
		for _, l := range x.txs {
			entries += len(l)
		}
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), "heap-MB")
		b.ReportMetric(float64(len(x.counts)), "keys")
		b.ReportMetric(float64(entries), "tx-refs")
	})
}

// BenchmarkSubpathsColdConcurrent_413 sends n concurrent cold requests for
// the same key (PR #529 review F1). peak-heap-MB is the highest heap held by
// live and unswept objects while they run (sampled every 200 µs), counts the
// number of store counts they ran. Run with -benchtime 1x -count 3.
func BenchmarkSubpathsColdConcurrent_413(b *testing.B) {
	s := bench413Store(b)
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	for _, n := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			var counts atomic.Int32
			s.subpathHook = func(stage string) {
				if stage == "count" {
					counts.Add(1)
				}
			}
			defer func() { s.subpathHook = nil }()
			var peak uint64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				clearSubpathCache413(s)
				runtime.GC()
				stop := make(chan struct{})
				sampled := make(chan uint64)
				go func() {
					var max uint64
					tick := time.NewTicker(200 * time.Microsecond)
					defer tick.Stop()
					for {
						metrics.Read(sample)
						if v := sample[0].Value.Uint64(); v > max {
							max = v
						}
						select {
						case <-stop:
							sampled <- max
							return
						case <-tick.C:
						}
					}
				}()
				start := make(chan struct{})
				var wg sync.WaitGroup
				for j := 0; j < n; j++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						s.GetAnalyticsSubpaths("", 2, 8, 100)
					}()
				}
				b.StartTimer()
				close(start)
				wg.Wait()
				b.StopTimer()
				close(stop)
				if p := <-sampled; p > peak {
					peak = p
				}
			}
			b.ReportMetric(float64(peak)/(1<<20), "peak-heap-MB")
			b.ReportMetric(float64(counts.Load())/float64(b.N), "counts/op")
		})
	}
}

// BenchmarkSubpathDetailWriterWait_413 runs a store writer that takes
// PacketStore.mu in a loop while subpath-detail requests run (PR #529 review
// F2). max-writer-wait-µs is the longest it waited for the lock. GC is off
// while it runs, so a collection's pause does not count as lock time.
func BenchmarkSubpathDetailWriterWait_413(b *testing.B) {
	s := bench413Store(b)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	hot, cold := subpath413DetailKeys(s.packets)
	for _, c := range []struct {
		name string
		hops []string
	}{{"hot", hot}, {"cold", cold}} {
		b.Run(c.name, func(b *testing.B) {
			stop := make(chan struct{})
			waited := make(chan time.Duration)
			go func() {
				var max time.Duration
				for {
					select {
					case <-stop:
						waited <- max
						return
					default:
					}
					t0 := time.Now()
					s.mu.Lock()
					if w := time.Since(t0); w > max {
						max = w
					}
					s.mu.Unlock()
					runtime.Gosched()
				}
			}()
			for i := 0; i < b.N; i++ {
				s.GetSubpathDetail(c.hops)
			}
			b.StopTimer()
			close(stop)
			b.ReportMetric(float64((<-waited).Microseconds()), "max-writer-wait-µs")
		})
	}
}
