package main

import (
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// Measurement harness for #533 (in-place scanning of persisted resolved_path
// pubkeys). It compiles against master too, so the same benchmarks give the
// before and after numbers.
//
// Inputs come from obs489Data, the large-instance shape used for #489: 1 to
// 3 hops per row, each a 64-hex pubkey.

// obs533Values returns the resolved_path column values obs489Data writes for
// about n observations, marshalled exactly as the ingestor persists them.
func obs533Values(n int) []string {
	txs := obs489Data(n, 533)
	out := make([]string, 0, n)
	for _, tx := range txs {
		for _, r := range tx.rows {
			out = append(out, marshalResolvedPath(stringPtrs(r.resolved)))
		}
	}
	return out
}

func stringPtrs(ss []string) []*string {
	out := make([]*string, len(ss))
	for i := range ss {
		out[i] = &ss[i]
	}
	return out
}

// BenchmarkDecodePersistedRelayPath_533 is the per-observation decode the
// bulk-load and live-ingest paths run (decodePersistedRelayPath).
//
//	go test -run '^$' -bench DecodePersistedRelayPath_533 -count 6 .
func BenchmarkDecodePersistedRelayPath_533(b *testing.B) {
	vals := obs533Values(100000)
	b.ReportAllocs()
	b.ResetTimer()
	var n int
	for i := 0; i < b.N; i++ {
		n += len(decodePersistedRelayPath(vals[i%len(vals)]).pubkeys)
	}
	runtime.KeepAlive(n)
}

// TestResolvedPathScan533_Measure loads a CORESCOPE_533_MEASURE-observation
// synthetic store (2500000 is the large instance: about 170K transmissions)
// and reports Load wall time, allocations, heap after load and peak RSS
// proxy (Sys).
// Run it with -cpuprofile/-memprofile to attribute the cost.
//
//	CORESCOPE_533_MEASURE=2500000 go test -run ResolvedPathScan533_Measure -v -timeout 60m
func TestResolvedPathScan533_Measure(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("CORESCOPE_533_MEASURE"))
	if n <= 0 {
		t.Skip("set CORESCOPE_533_MEASURE=2500000 to run")
	}
	txs := obs489Data(n, 533)
	dbPath := obs489DB(t, txs)
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.conn.Close()

	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	t0 := time.Now()
	s := NewPacketStore(db, &PacketStoreConfig{})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	load := time.Since(t0)
	s.WaitIndexesReady(10 * time.Minute)
	runtime.ReadMemStats(&m1)
	allocs, bytes := m1.Mallocs-m0.Mallocs, m1.TotalAlloc-m0.TotalAlloc
	runtime.GC()
	runtime.ReadMemStats(&m1)
	t.Logf("load: tx=%d obs=%d wall=%v allocs=%d allocMiB=%.1f heapMiB=%.1f sysMiB=%.0f",
		len(s.packets), s.totalObs, load, allocs, float64(bytes)/1048576,
		float64(int64(m1.HeapAlloc)-int64(m0.HeapAlloc))/1048576, float64(m1.Sys)/1048576)
	runtime.KeepAlive(s)
}
