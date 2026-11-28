package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

// 30k transmissions, 1–5 observations each (90k joined observation rows).
// Fixture creation and connection opening are excluded. Every measured load
// starts with an empty store and includes its normal index-building work.
// Use -benchtime=1x in fresh processes for before/after heap/RSS comparisons.
func BenchmarkBulkScanLoaders486(b *testing.B) {
	path := filepath.Join(b.TempDir(), "scan-30k.db")
	createTestDBWithObs(b, path, 30000)
	db, err := OpenDB(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	for _, loader := range []string{"Load", "LoadChunked", "loadChunk"} {
		b.Run(loader, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(90000, "rows/op")
			var s *PacketStore
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s = NewPacketStore(db, nil)
				if err := bulkScanLoad486(s, loader); err != nil {
					b.Fatal(err)
				}
				if len(s.packets) != 30000 || s.totalObs != 90000 {
					b.Fatalf("loaded %d/%d, want 30000/90000", len(s.packets), s.totalObs)
				}
			}
			b.StopTimer()
			runtime.GC()
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			b.ReportMetric(float64(mem.HeapAlloc), "live-heap-B")
			runtime.KeepAlive(s)
		})
	}
}
