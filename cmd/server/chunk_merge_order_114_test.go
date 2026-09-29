package main

import (
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Issue #114: PacketStore.packets must stay ordered by first_seen ASC,
// because eviction walks it from the head and stops at the first
// transmission inside the retention window. Background chunks are windowed
// on last_seen (#1690), so a chunk can hold transmissions whose first_seen
// is NEWER than transmissions already in the store (an old transmission that
// was heard again recently is in the hot set). loadChunk published a chunk
// with s.packets = append(localPackets, s.packets...), which then leaves an
// out-of-order store and makes eviction stop early.

type fixtureTx114 struct {
	id        int
	hash      string
	firstSeen time.Time
	lastSeen  time.Time
}

// createChunkOrderDB writes transmissions with explicit first_seen and
// last_seen (unix seconds, as the live schema stores it) and one observation
// each.
func createChunkOrderDB(t *testing.T, txs []fixtureTx114) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "chunk-order.db")
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(q string, args ...interface{}) {
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatalf("setup: %v\n%s", err, q)
		}
	}
	// PREFLIGHT: async=true reason="test fixture: t.TempDir SQLite created from empty, never a real DB"
	exec(`CREATE TABLE transmissions (
		id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT, first_seen TEXT,
		route_type INTEGER, payload_type INTEGER, payload_version INTEGER,
		decoded_json TEXT, last_seen INTEGER)`)
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE observations (
		id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_id TEXT, observer_name TEXT,
		direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT, timestamp TEXT,
		raw_hex TEXT, resolved_path TEXT)`)
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE observers (rowid INTEGER PRIMARY KEY, id TEXT, name TEXT, iata TEXT)`)
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL,
		last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0)`)
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE schema_version (version INTEGER)`)
	exec(`INSERT INTO schema_version (version) VALUES (1)`)
	for _, tx := range txs {
		exec(`INSERT INTO transmissions VALUES (?,?,?,?,0,5,1,'{}',?)`,
			tx.id, "aa", tx.hash, tx.firstSeen.UTC().Format(time.RFC3339), tx.lastSeen.Unix())
		exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp, raw_hex, resolved_path)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL)`,
			tx.id, tx.id, "obs1", "Obs1", "RX", -10.0, -80.0, 5, `["aa"]`, tx.lastSeen.UTC().Format(time.RFC3339), "")
	}
	return dbPath
}

func assertSortedByFirstSeen(t *testing.T, s *PacketStore) {
	t.Helper()
	if !sort.SliceIsSorted(s.packets, func(i, j int) bool { return s.packets[i].FirstSeen < s.packets[j].FirstSeen }) {
		var order []string
		for _, tx := range s.packets {
			order = append(order, tx.Hash+"@"+tx.FirstSeen)
		}
		t.Fatalf("s.packets is not ordered by first_seen: %v", order)
	}
}

func assertEveryPacketIndexedOnce(t *testing.T, s *PacketStore, want int) {
	t.Helper()
	if len(s.packets) != want {
		t.Fatalf("%d packets in the store, want %d", len(s.packets), want)
	}
	seen := map[int]bool{}
	for _, tx := range s.packets {
		if seen[tx.ID] {
			t.Fatalf("tx %d is in s.packets twice", tx.ID)
		}
		seen[tx.ID] = true
		if s.byHash[tx.Hash] != tx || s.byTxID[tx.ID] != tx {
			t.Fatalf("tx %d is in s.packets without its byHash/byTxID entries", tx.ID)
		}
	}
}

// chunkOrderFixture: two transmissions in the hot set (heard in the last
// hour), one of them first seen 30h ago, and a background chunk whose
// transmissions straddle it, with a first_seen tie on either side.
func chunkOrderFixture(now time.Time) []fixtureTx114 {
	h := func(d time.Duration) time.Time { return now.Add(-d).Truncate(time.Second) }
	return []fixtureTx114{
		{1, "hot-old-first-seen", h(30 * time.Hour), h(10 * time.Minute)},
		{2, "hot-new", h(20 * time.Minute), h(5 * time.Minute)},
		{3, "chunk-40h", h(40 * time.Hour), h(20 * time.Hour)},
		{4, "chunk-20h", h(20 * time.Hour), h(5 * time.Hour)},
		{5, "chunk-35h", h(35 * time.Hour), h(30 * time.Hour)},
		{6, "chunk-tie-30h", h(30 * time.Hour), h(3 * time.Hour)},
		{7, "chunk-2h", h(2 * time.Hour), h(90 * time.Minute)},
	}
}

func loadHotThenChunk(t *testing.T, now time.Time, txs []fixtureTx114) *PacketStore {
	t.Helper()
	db, err := OpenDB(createChunkOrderDB(t, txs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	if !db.hasLastSeen() {
		t.Fatal("fixture: last_seen column not detected; chunks would not be windowed on last_seen")
	}
	s := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72, HotStartupHours: 1})
	s.graph.Store(NewNeighborGraph())
	if err := s.LoadChunked(0); err != nil {
		t.Fatalf("LoadChunked: %v", err)
	}
	if len(s.packets) != 2 {
		t.Fatalf("fixture: hot load has %d packets, want the 2 heard in the last hour", len(s.packets))
	}
	if err := s.loadChunk(now.Add(-72*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("loadChunk: %v", err)
	}
	return s
}

func TestBackgroundChunkMergeKeepsFirstSeenOrder_114(t *testing.T) {
	now := time.Now().UTC()
	s := loadHotThenChunk(t, now, chunkOrderFixture(now))
	assertSortedByFirstSeen(t, s)
	assertEveryPacketIndexedOnce(t, s, 7)
}

// Time-based eviction must remove every transmission older than retention,
// not stop at the first in-window one it meets at the head.
func TestEvictionAfterChunkMergeRemovesAllExpired_114(t *testing.T) {
	now := time.Now().UTC()
	s := loadHotThenChunk(t, now, chunkOrderFixture(now))
	s.retentionHours = 25
	s.mu.Lock()
	n := s.EvictStale()
	s.mu.Unlock()
	cutoff := now.Add(-25 * time.Hour).Format(time.RFC3339)
	for _, tx := range s.packets {
		if tx.FirstSeen < cutoff {
			t.Errorf("tx %s (first_seen %s) is older than retention but survived eviction", tx.Hash, tx.FirstSeen)
		}
	}
	if n != 4 {
		t.Errorf("evicted %d, want 4 (40h, 35h and both 30h)", n)
	}
	assertEveryPacketIndexedOnce(t, s, 3)
}

// Memory-based eviction also evicts from the head: after a merge, the head
// must be the oldest transmissions.
func TestMemoryEvictionAfterChunkMergeTakesOldest_114(t *testing.T) {
	now := time.Now().UTC()
	s := loadHotThenChunk(t, now, chunkOrderFixture(now))
	s.retentionHours = 0
	s.maxMemoryMB = 1
	s.trackedBytes = 2 * 1048576 // over the watermark; the 25% cap evicts 1 of 7
	s.mu.Lock()
	n := s.EvictStale()
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if _, ok := s.byHash["chunk-40h"]; ok {
		t.Error("memory eviction kept the oldest transmission (chunk-40h)")
	}
}

// A second chunk merged later (repeated last_seen activity moves rows
// between windows) keeps the order too.
func TestRepeatedChunkMergesKeepOrder_114(t *testing.T) {
	now := time.Now().UTC()
	txs := chunkOrderFixture(now)
	txs = append(txs,
		fixtureTx114{8, "late-chunk-25h", now.Add(-25 * time.Hour).Truncate(time.Second), now.Add(-80 * time.Hour)},
		fixtureTx114{9, "late-chunk-50h", now.Add(-50 * time.Hour).Truncate(time.Second), now.Add(-90 * time.Hour)},
	)
	s := loadHotThenChunk(t, now, txs)
	if err := s.loadChunk(now.Add(-100*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatalf("second loadChunk: %v", err)
	}
	assertSortedByFirstSeen(t, s)
	assertEveryPacketIndexedOnce(t, s, 9)
}
