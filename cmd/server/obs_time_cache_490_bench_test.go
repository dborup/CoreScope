package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"unsafe"
)

// The measurements of #490. They use only ParsedTime, Load and the
// accounting constants, so the same file runs on the branch before it.

// BenchmarkStoreObsParsedTime_490 measures the first call (the parse and the
// cache fill) and a cached call, for a UTC timestamp, one with an offset (a
// reparse on every call since #490) and an invalid one.
func BenchmarkStoreObsParsedTime_490(b *testing.B) {
	for _, c := range []struct{ name, ts string }{
		{"utc", "2026-10-24T12:34:56.123456789Z"},
		{"sql", "2026-10-24 12:34:56"},
		{"offset", "2026-10-24T13:34:56+01:00"},
		{"invalid", "not a time"},
	} {
		b.Run(c.name+"/first", func(b *testing.B) {
			const chunk = 4096
			obs := make([]StoreObs, chunk)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%chunk == 0 {
					b.StopTimer()
					obs = make([]StoreObs, chunk)
					for j := range obs {
						obs[j].Timestamp = c.ts
					}
					b.StartTimer()
				}
				obs[i%chunk].ParsedTime()
			}
		})
		b.Run(c.name+"/cached", func(b *testing.B) {
			o := &StoreObs{Timestamp: c.ts}
			o.ParsedTime()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				o.ParsedTime()
			}
		})
	}
}

// TestStoreObsHeapAfterLoad_490 reports the layout, the accounting and the
// heap a loaded store retains, with every observation's time parsed the way
// the analytics parse it. Set CORESCOPE_MEASURE_490 to the number of
// transmissions to generate (about 2.5 observations each), or to "fixture"
// for test-fixtures/e2e-fixture.db.
func TestStoreObsHeapAfterLoad_490(t *testing.T) {
	arg := os.Getenv("CORESCOPE_MEASURE_490")
	if arg == "" {
		t.Skip("measurement: set CORESCOPE_MEASURE_490")
	}
	dbPath := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	if arg != "fixture" {
		n, err := strconv.Atoi(arg)
		if err != nil {
			t.Fatal(err)
		}
		dbPath = filepath.Join(t.TempDir(), "measure.db")
		acct113CreateDB(t, dbPath, n, 0)
	}
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	store := NewPacketStore(db, &PacketStoreConfig{})
	store.graph.Store(loadNeighborEdgesFromDB(db.conn)) // before Load, as main.go does
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	nObs := 0
	for _, tx := range store.packets {
		for _, o := range tx.Observations {
			o.ParsedTime()
			nObs++
		}
	}
	db.Close() // the SQLite page cache is Go heap too, and not the store's
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m1)
	heap := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	t.Logf("Sizeof(StoreObs)=%d B, size class=%d B, storeObsBaseBytes=%d B",
		unsafe.Sizeof(StoreObs{}), heapSizeClassOf490(unsafe.Sizeof(StoreObs{})), storeObsBaseBytes)
	t.Logf("transmissions=%d observations=%d heap after load=%.2f MiB (%.1f B/obs) trackedBytes=%.2f MiB ratio=%.3f",
		len(store.packets), nObs, float64(heap)/1048576, float64(heap)/float64(nObs),
		float64(store.trackedBytes)/1048576, float64(store.trackedBytes)/float64(heap))
	runtime.KeepAlive(store)
}

// heapSizeClassOf490 is the size the heap allocates for an n-byte object:
// the smallest of the runtime's own size classes that holds it.
func heapSizeClassOf490(n uintptr) uint32 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	for _, c := range ms.BySize {
		if uintptr(c.Size) >= n {
			return c.Size
		}
	}
	return 0
}
