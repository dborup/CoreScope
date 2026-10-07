package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Benchmarks for the accounting added in #113 / #164: the cold-load path
// (every tx and observation is charged and recharged), live ingest of new
// transmissions and of new observations on known ones, and an eviction pass.
// They use production entry points only, so they also run on master for the
// "before" numbers (see the PR for the interleaved comparison). The three
// DB-backed ingest benchmarks spend most of their time in SQLite, so the two
// pure-Go ones at the end isolate the accounting itself.

const bench113Tx = 20000

func bench113DB(b *testing.B) string {
	b.Helper()
	dbPath := filepath.Join(b.TempDir(), "bench.db")
	acct113CreateDB(b, dbPath, bench113Tx, bench113Tx/2)
	return dbPath
}

func BenchmarkTrackedBytes_Load_113(b *testing.B) {
	dbPath := bench113DB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db, err := OpenDB(dbPath)
		if err != nil {
			b.Fatal(err)
		}
		s := NewPacketStore(db, &PacketStoreConfig{})
		b.StartTimer()
		if err := s.Load(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		db.conn.Close()
	}
}

// New transmissions, one observation each, 200 per iteration.
func BenchmarkTrackedBytes_IngestNewTx_113(b *testing.B) {
	dbPath := bench113DB(b)
	store := acct113Load(b, dbPath)
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	maxTx := bench113Tx
	maxObs := 0
	if err := conn.QueryRow(`SELECT COALESCE(MAX(id),0) FROM observations`).Scan(&maxObs); err != nil {
		b.Fatal(err)
	}
	ts := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		first := maxTx
		for k := 0; k < 200; k++ {
			maxTx++
			maxObs++
			conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
				VALUES (?, 'aabb', ?, ?, 1, 5, 1, '{"type":"GRP_TXT","text":"live"}')`, maxTx, fmt.Sprintf("bn%08d", maxTx), ts)
			conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, path_json, timestamp)
				VALUES (?, ?, 'obs0', 'Alpha', 'RX', '["aa","bb","cc"]', ?)`, maxObs, maxTx, ts)
		}
		b.StartTimer()
		store.IngestNewFromDB(first, 1000)
	}
}

// New observations with a longer path on known transmissions, 200 per iteration.
func BenchmarkTrackedBytes_IngestNewObs_113(b *testing.B) {
	dbPath := bench113DB(b)
	store := acct113Load(b, dbPath)
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	maxObs := 0
	if err := conn.QueryRow(`SELECT COALESCE(MAX(id),0) FROM observations`).Scan(&maxObs); err != nil {
		b.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		first := maxObs
		for k := 0; k < 200; k++ {
			maxObs++
			conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, path_json, timestamp)
				VALUES (?, ?, 'obs3', 'Delta', 'RX', '["01","02","03","04","05","06"]', ?)`, maxObs, 1+(maxObs*7)%bench113Tx, ts)
		}
		b.StartTimer()
		store.IngestNewObservations(first, 1000)
	}
}

// One retention pass that evicts half of a 20k store.
func BenchmarkTrackedBytes_Evict_113(b *testing.B) {
	dbPath := bench113DB(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s := acct113Load(b, dbPath)
		s.retentionHours = 24
		b.StartTimer()
		if n := s.RunEviction(); n != bench113Tx/2 {
			b.Fatalf("evicted %d, want %d", n, bench113Tx/2)
		}
	}
}

// Adding relays to a tx that already has records: the steady state (the same
// relays again, nothing appended) and the growth case (a fresh tx each time,
// three new keys). Runs on master too; there it does no accounting.
func BenchmarkAddResolvedPubkeys_Steady_113(b *testing.B) {
	s := &PacketStore{byPathHop: map[string][]*StoreTx{}, pathHopResolved: map[*StoreTx][]uint64{}}
	tx := &StoreTx{ID: 1, PathJSON: `["aa","bb","cc"]`}
	keys := []string{acct113PK(1), acct113PK(2), acct113PK(3)}
	hops := map[string]bool{}
	s.addResolvedPubkeysToPathHopIndex(tx, keys, hops)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.addResolvedPubkeysToPathHopIndex(tx, keys, hops)
	}
}

func BenchmarkAddResolvedPubkeys_Grow_113(b *testing.B) {
	s := &PacketStore{byPathHop: map[string][]*StoreTx{}, pathHopResolved: map[*StoreTx][]uint64{}}
	keys := []string{acct113PK(1), acct113PK(2), acct113PK(3)}
	hops := map[string]bool{}
	txs := make([]*StoreTx, 1024)
	for i := range txs {
		txs[i] = &StoreTx{ID: i, PathJSON: `["aa","bb","cc"]`}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := txs[i%len(txs)]
		if i%len(txs) == 0 {
			clear(s.pathHopResolved)
			clear(s.byPathHop)
		}
		s.addResolvedPubkeysToPathHopIndex(tx, keys, hops)
	}
}
