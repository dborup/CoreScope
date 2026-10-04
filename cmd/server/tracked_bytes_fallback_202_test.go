package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Issue #202, leak 2: the relays indexObservationRelayHops resolves from
// path_json when an observation has no persisted resolved_path.

// leak202SeedNodes gives the store a node cache with one repeater per
// acct113PK key (acct113PK(k) starts with the byte k), so every one-byte hop
// resolves to a unique node. withNodes=false installs an empty cache, so the
// same rows resolve nothing: a twin store that differs from the first only by
// the fallback's relays.
func leak202SeedNodes(s *PacketStore, withNodes bool) {
	var nodes []nodeInfo
	if withNodes {
		for k := 0; k < 256; k++ {
			nodes = append(nodes, nodeInfo{PublicKey: acct113PK(k), Name: fmt.Sprintf("n%d", k), Role: "repeater"})
		}
	}
	s.cacheMu.Lock()
	s.nodeCache = nodes
	s.nodePM = buildPrefixMap(nodes)
	s.nodeCacheTime = time.Now()
	s.cacheMu.Unlock()
}

// leak202Fixture is a DB whose observations all have a NULL resolved_path, so
// every relay comes from the path_json fallback.
func leak202Fixture(t *testing.T, n, oldUntil int) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "leak202.db")
	acct113CreateDBOpts(t, dbPath, n, oldUntil, 0)
	return dbPath
}

func leak202Open(t *testing.T, dbPath string, withNodes bool, cfg *PacketStoreConfig) *PacketStore {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	store := NewPacketStore(db, cfg)
	leak202SeedNodes(store, withNodes)
	return store
}

func leak202Ready(t *testing.T, s *PacketStore) {
	t.Helper()
	if !s.WaitIndexesReady(30 * time.Second) {
		t.Fatal("background index builds did not finish")
	}
}

// leak202ByNodeEntries counts the (node, tx) pairs byNode holds.
func leak202ByNodeEntries(s *PacketStore) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, l := range s.byNode {
		n += len(l)
	}
	return n
}

// leak202Insert adds rows after the store was loaded, as the ingestor would.
func leak202Insert(t *testing.T, dbPath string, fn func(conn *sql.DB, maxObs int)) {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var maxObs int
	if err := conn.QueryRow(`SELECT MAX(id) FROM observations`).Scan(&maxObs); err != nil {
		t.Fatal(err)
	}
	fn(conn, maxObs)
}

// Every way a fallback relay reaches the store. Each builds the same rows
// twice, once with nodes the hops resolve to and once without.
var leak202Scenarios = []struct {
	name  string
	build func(t *testing.T, withNodes bool) *PacketStore
}{
	{"Load", func(t *testing.T, withNodes bool) *PacketStore {
		s := leak202Open(t, leak202Fixture(t, 200, 0), withNodes, &PacketStoreConfig{})
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		leak202Ready(t, s)
		return s
	}},
	{"LoadChunked", func(t *testing.T, withNodes bool) *PacketStore {
		s := leak202Open(t, leak202Fixture(t, 200, 0), withNodes, &PacketStoreConfig{RetentionHours: 168})
		if err := s.RunStartupLoad(100); err != nil {
			t.Fatal(err)
		}
		leak202Ready(t, s)
		return s
	}},
	{"BackgroundFill", func(t *testing.T, withNodes bool) *PacketStore {
		s := leak202Open(t, leak202Fixture(t, 200, 100), withNodes, &PacketStoreConfig{RetentionHours: 168, HotStartupHours: 2})
		if err := s.RunStartupLoad(100); err != nil {
			t.Fatal(err)
		}
		leak202Ready(t, s)
		if len(s.packets) != 200 {
			t.Fatalf("setup: %d packets after the startup load, want 200 (hot window plus background fill)", len(s.packets))
		}
		return s
	}},
	{"IngestNewFromDB", func(t *testing.T, withNodes bool) *PacketStore {
		dbPath := leak202Fixture(t, 20, 0)
		s := leak202Open(t, dbPath, withNodes, &PacketStoreConfig{})
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		leak202Ready(t, s)
		leak202Insert(t, dbPath, func(conn *sql.DB, maxObs int) {
			now := time.Now().UTC().Format(time.RFC3339)
			for k := 1; k <= 5; k++ {
				if _, err := conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
					VALUES (?, 'aabb', ?, ?, 1, 5, 1, '{"type":"GRP_TXT","text":"live"}')`, 20+k, fmt.Sprintf("live%06d", 20+k), now); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, path_json, timestamp)
					VALUES (?, ?, 'obs0', 'Alpha', 'RX', ?, ?)`, maxObs+k, 20+k, fmt.Sprintf(`["%02x","%02x"]`, 0xa0+k, 0xb0+k), now); err != nil {
					t.Fatal(err)
				}
			}
		})
		s.IngestNewFromDB(20, 100)
		if len(s.packets) != 25 {
			t.Fatalf("setup: %d packets after the live ingest, want 25", len(s.packets))
		}
		return s
	}},
	{"IngestNewObservations", func(t *testing.T, withNodes bool) *PacketStore {
		dbPath := leak202Fixture(t, 20, 0)
		s := leak202Open(t, dbPath, withNodes, &PacketStoreConfig{})
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		leak202Ready(t, s)
		var sinceObs int
		leak202Insert(t, dbPath, func(conn *sql.DB, maxObs int) {
			sinceObs = maxObs
			now := time.Now().UTC().Format(time.RFC3339)
			for k := 1; k <= 5; k++ { // late observations of existing transmissions, through new relays
				if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, path_json, timestamp)
					VALUES (?, ?, 'obs3', 'Delta', 'RX', ?, ?)`, maxObs+k, k, fmt.Sprintf(`["%02x","%02x","%02x"]`, 0xc0+k, 0xd0+k, 0xe0+k), now); err != nil {
					t.Fatal(err)
				}
			}
		})
		s.IngestNewObservations(sinceObs, 100)
		return s
	}},
}

// The fallback puts a transmission into byNode under each relay it resolves,
// so it is memory the store holds and must be charged. The twin without
// nodes holds none of it.
func TestFallbackRelays_AreCharged_202(t *testing.T) {
	for _, sc := range leak202Scenarios {
		t.Run(sc.name, func(t *testing.T) {
			with, without := sc.build(t, true), sc.build(t, false)
			if got, base := leak202ByNodeEntries(with), leak202ByNodeEntries(without); got <= base {
				t.Fatalf("setup: the fallback indexed nothing (byNode entries %d with nodes, %d without)", got, base)
			}
			if with.trackedBytes <= without.trackedBytes {
				t.Fatalf("trackedBytes = %d with the fallback's byNode entries, %d without them: they are not charged",
					with.trackedBytes, without.trackedBytes)
			}
		})
	}
}

// Eviction removes what the fallback indexed (byNode, nodeHashes) and credits
// what it charged: an emptied store holds nothing and is charged nothing.
func TestFallbackRelays_EvictionRemovesAndCredits_202(t *testing.T) {
	for _, sc := range leak202Scenarios {
		t.Run(sc.name, func(t *testing.T) {
			s := sc.build(t, true)
			s.mu.Lock()
			for _, tx := range s.packets { // live tx carry current timestamps
				tx.FirstSeen = time.Now().UTC().Add(-100 * time.Hour).Format(time.RFC3339)
			}
			s.retentionHours = 24
			s.mu.Unlock()
			if n := s.RunEviction(); n == 0 {
				t.Fatal("setup: eviction removed nothing")
			}
			s.mu.RLock()
			defer s.mu.RUnlock()
			if len(s.packets) != 0 {
				t.Fatalf("setup: %d transmissions left after eviction", len(s.packets))
			}
			if s.trackedBytes != 0 {
				t.Errorf("trackedBytes = %d after every transmission was evicted, want 0", s.trackedBytes)
			}
			if len(s.byNode) != 0 || len(s.nodeHashes) != 0 {
				t.Errorf("an emptied store still indexes %d nodes in byNode and %d in nodeHashes: evicted transmissions are pinned",
					len(s.byNode), len(s.nodeHashes))
			}
		})
	}
}
