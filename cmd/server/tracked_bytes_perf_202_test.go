package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Issue #202 perf check. Not a pass/fail test: with CORESCOPE_PERF_202=<runs>
// it times the two paths this change touches and prints one RESULT line per
// measurement, so the same file can be compiled against master
// (go test -c, then run the two binaries alternately):
//
//	CORESCOPE_PERF_202=7 ./server.test -test.run TestPerf_202 -test.v
//
// It uses only functions that exist on master.
func perf202Runs(t *testing.T) int {
	n, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_202"))
	if n <= 0 {
		t.Skip("set CORESCOPE_PERF_202=<runs> to measure")
	}
	return n
}

func perf202Median(d []time.Duration) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}

// The path_json fallback: Load of 5000 transmissions whose hops all resolve
// through it, and then the eviction of all of them.
func TestPerf_FallbackLoadAndEvict_202(t *testing.T) {
	runs := perf202Runs(t)
	dbPath := leak202Fixture(t, 5000, 5000)
	var load, evict []time.Duration
	var tracked int64
	for i := 0; i < runs; i++ {
		s := leak202Open(t, dbPath, true, &PacketStoreConfig{})
		t0 := time.Now()
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		load = append(load, time.Since(t0))
		leak202Ready(t, s)
		tracked = s.trackedBytes
		s.mu.Lock()
		s.retentionHours = 24
		s.mu.Unlock()
		t0 = time.Now()
		if n := s.RunEviction(); n != 5000 {
			t.Fatalf("evicted %d, want 5000", n)
		}
		evict = append(evict, time.Since(t0))
	}
	fmt.Printf("RESULT fallback-load-5000 median_ns=%d trackedBytes=%d\n", perf202Median(load).Nanoseconds(), tracked)
	fmt.Printf("RESULT fallback-evict-5000 median_ns=%d\n", perf202Median(evict).Nanoseconds())
}

// The fallback's charge against the heap it really holds: the marginal heap
// of a store whose hops resolve through the fallback, over the same store
// whose hops resolve nothing, and the marginal trackedBytes. On master the
// second number is 0.
func TestPerf_FallbackChargeVsHeap_202(t *testing.T) {
	runs := perf202Runs(t)
	dbPath := leak202Fixture(t, 5000, 0)
	build := func(withNodes bool) func() *PacketStore {
		return func() *PacketStore {
			s := leak202Open(t, dbPath, withNodes, &PacketStoreConfig{})
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			leak202Ready(t, s)
			return s
		}
	}
	for i := 0; i < runs; i++ {
		with, hWith := heapOf(build(true))
		without, hWithout := heapOf(build(false))
		fmt.Printf("RESULT fallback-charge-vs-heap-5000 run=%d heap_delta=%d charge_delta=%d\n",
			i, hWith-hWithout, with.trackedBytes-without.trackedBytes)
	}
}

// Hash migration with 1000 duplicate pairs (2000 transmissions, 2 observations
// each) that all collide and merge.
func TestPerf_HashMigrateMerge_202(t *testing.T) {
	runs := perf202Runs(t)
	var migrate []time.Duration
	packets := 0
	for i := 0; i < runs; i++ {
		db := setupTestDBv2(t)
		tx, err := db.conn.Begin()
		if err != nil {
			t.Fatal(err)
		}
		obsID := 0
		for id := 1; id <= 2000; id++ {
			raw := fmt.Sprintf("0A00D69FD7A5A7475DB07337749AE61FA53A4788%04X", (id+1)/2)
			if _, err := tx.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
				VALUES (?, ?, ?, '2026-01-01T00:00:00Z', 1, 5, '{}')`, id, raw, fmt.Sprintf("old-%d", id)); err != nil {
				t.Fatal(err)
			}
			for k := 0; k < 2; k++ {
				obsID++
				if _, err := tx.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, path_json, timestamp)
					VALUES (?, ?, 'observer', 'Observer', '["AA","BB"]', ?)`, obsID, id, obsID); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		store := NewPacketStore(db, nil)
		if err := store.Load(); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		migrateContentHashesAsync(store, 500, 0)
		migrate = append(migrate, time.Since(t0))
		// Master leaves the 2000 ghosts in memory; since #215 the duplicates
		// are merged (1000 remain). Printed, not asserted, so the same file
		// compiles and runs against both.
		packets = len(store.packets)
		db.conn.Close()
	}
	fmt.Printf("RESULT hash-migrate-merge-2000 median_ns=%d packets_after=%d\n", perf202Median(migrate).Nanoseconds(), packets)
}

// Hash migration of CORESCOPE_PERF_215_ROWS (default 5000) transmissions that all carry a stale hash and do not
// collide, each indexed under three node keys (#215). It measures the rename of
// the nodeHashes keys that the migration now does per batch. Compiles against
// master too (where the migration also writes the DB, on this in-memory
// handle).
func TestPerf_HashMigrateRehash_215(t *testing.T) {
	runs := perf202Runs(t)
	rows, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_215_ROWS"))
	if rows <= 0 {
		rows = 5000
	}
	var migrate []time.Duration
	for i := 0; i < runs; i++ {
		db := setupTestDBv2(t)
		tx, err := db.conn.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for id := 1; id <= rows; id++ {
			raw := fmt.Sprintf("0A00D69FD7A5A7475DB07337749AE61FA53A478%05X", id)
			decoded := fmt.Sprintf(`{"type":"ADVERT","pubKey":"%064x","destPubKey":"%064x","srcPubKey":"%064x"}`, id%800, (id+1)%800, (id+2)%800)
			if _, err := tx.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
				VALUES (?, ?, ?, '2026-01-01T00:00:00Z', 1, 4, ?)`, id, raw, fmt.Sprintf("old-%d", id), decoded); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, path_json, timestamp)
				VALUES (?, ?, 'observer', 'Observer', '["AA","BB"]', ?)`, id, id, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		store := NewPacketStore(db, nil)
		if err := store.Load(); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		migrateContentHashesAsync(store, 500, 0)
		migrate = append(migrate, time.Since(t0))
		if len(store.packets) != rows {
			t.Fatalf("%d packets after the migration, want %d", len(store.packets), rows)
		}
		db.conn.Close()
	}
	fmt.Printf("RESULT hash-migrate-rehash-%d median_ns=%d\n", rows, perf202Median(migrate).Nanoseconds())
}
