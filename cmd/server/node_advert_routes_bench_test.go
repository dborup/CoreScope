package main

import (
	"os"
	"testing"
	"time"
)

// Benchmarks for the #2073 node-detail advert route breakdown: the stream
// (which also yields flood_advert_count_7d), the window query it replaced,
// and the queries a node-detail request runs anyway. They need a
// large database, so they run only against NODE_ADVERT_BENCH_DB (a migrated
// CoreScope SQLite file) with NODE_ADVERT_BENCH_PUBKEY as the node to read:
//
//	NODE_ADVERT_BENCH_DB=/path/bench.db NODE_ADVERT_BENCH_PUBKEY=<hex> \
//	  go test -run '^$' -bench NodeAdvertRoutes -benchtime 50x .
func nodeAdvertBenchDB(b *testing.B) (*DB, string) {
	b.Helper()
	path, pubkey := os.Getenv("NODE_ADVERT_BENCH_DB"), os.Getenv("NODE_ADVERT_BENCH_PUBKEY")
	if path == "" || pubkey == "" {
		b.Skip("set NODE_ADVERT_BENCH_DB and NODE_ADVERT_BENCH_PUBKEY")
	}
	db, err := OpenDB(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.conn.Close() })
	return db, pubkey
}

func BenchmarkNodeAdvertRoutes(b *testing.B) {
	db, pubkey := nodeAdvertBenchDB(b)
	for i := 0; i < b.N; i++ {
		if _, _, _, err := db.GetNodeAdvertRoutes(pubkey, nodeAdvertRouteLimit, time.Now(), floodAdvertRowCap); err != nil {
			b.Fatal(err)
		}
	}
}

// The ROW_NUMBER() window query GetNodeAdvertRoutes replaced (PR #97 review
// P2-1), kept as the test-only oracle.
func BenchmarkNodeAdvertRoutes_WindowOracle(b *testing.B) {
	db, pubkey := nodeAdvertBenchDB(b)
	for i := 0; i < b.N; i++ {
		if _, _, err := oracleNodeAdvertRoutesWindow(db, pubkey, nodeAdvertRouteLimit, time.Now(), floodAdvertRowCap); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNodeAdvertRoutes_FloodAdvertCount7d(b *testing.B) {
	db, pubkey := nodeAdvertBenchDB(b)
	for i := 0; i < b.N; i++ {
		if _, err := db.CountFloodAdvertsForNode(pubkey, 7*24, floodAdvertRowCap); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNodeAdvertRoutes_RecentAdverts(b *testing.B) {
	db, pubkey := nodeAdvertBenchDB(b)
	for i := 0; i < b.N; i++ {
		if _, err := db.GetRecentTransmissionsForNode(pubkey, 20, true); err != nil {
			b.Fatal(err)
		}
	}
}
