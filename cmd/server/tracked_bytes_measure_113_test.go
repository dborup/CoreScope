package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Synthetic store for the trackedBytes accounting tests (#113, #164) and the
// tracked-vs-heap measurement below. It is built from standard Load()
// inputs only, so it also runs against master for the "before" numbers.

// acct113PK is a 64-hex relay pubkey from a fixed pool.
func acct113PK(k int) string {
	return fmt.Sprintf("%02x", k%256) + strings.Repeat("c4", 31)
}

func acct113Hop(i, h, j int) string { return fmt.Sprintf("%02x", (i*7+h*13+j)%256) }

// acct113CreateDB writes n transmissions with 1..4 observations each. The
// observations of one transmission carry paths of different lengths (the
// longest wins pickBestObservation, after the tx was created) and a
// resolved_path of full pubkeys. oldUntil of them are 3 days old, the rest
// one hour.
func acct113CreateDB(tb testing.TB, dbPath string, n, oldUntil int) {
	acct113CreateDBOpts(tb, dbPath, n, oldUntil, 99)
}

// acct113CreateDBOpts is acct113CreateDB with the resolved_path of every
// observation limited to its first maxResolved hops (0: left NULL), so the
// measurement can vary the resolved relays per transmission.
func acct113CreateDBOpts(tb testing.TB, dbPath string, n, oldUntil, maxResolved int) {
	tb.Helper()
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		tb.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT, first_seen TEXT,
			route_type INTEGER, payload_type INTEGER, payload_version INTEGER, decoded_json TEXT)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_id TEXT,
			observer_name TEXT, direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT,
			timestamp TEXT, raw_hex TEXT, resolved_path TEXT)`,
		`CREATE TABLE observers (rowid INTEGER PRIMARY KEY, id TEXT, name TEXT, iata TEXT)`,
		`CREATE TABLE nodes (pubkey TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL,
			last_seen TEXT, first_seen TEXT, frequency REAL)`,
		`CREATE TABLE schema_version (version INTEGER)`,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`CREATE INDEX idx_tx_first_seen ON transmissions(first_seen)`,
	} {
		if _, err := conn.Exec(q); err != nil {
			tb.Fatalf("setup: %v\n%s", err, q)
		}
	}
	names := []string{"Alpha", "Bravo", "Charlie", "Delta"}
	for j, name := range names {
		if _, err := conn.Exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, 'TST')`, j+1, fmt.Sprintf("obs%d", j), name); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := conn.Exec("BEGIN"); err != nil {
		tb.Fatal(err)
	}
	now := time.Now().UTC()
	obsID := 0
	for i := 1; i <= n; i++ {
		ts := now.Add(-time.Hour + time.Duration(i)*time.Second)
		if i <= oldUntil {
			ts = now.Add(-72*time.Hour + time.Duration(i)*time.Second)
		}
		decoded := fmt.Sprintf(`{"type":"GRP_TXT","channel":"#test","text":"message number %d with some body text","sender":"node%d"}`, i, i%50)
		if _, err := conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
			VALUES (?, ?, ?, ?, 1, 5, 1, ?)`, i, fmt.Sprintf("1500%036d", i), fmt.Sprintf("h%06d", i), ts.Format(time.RFC3339), decoded); err != nil {
			tb.Fatal(err)
		}
		for j := 0; j < 1+i%4; j++ {
			obsID++
			hops := 1 + (i+j)%5
			path := make([]string, hops)
			resolved := make([]string, hops)
			for h := range path {
				path[h] = acct113Hop(i, h, j)
				resolved[h] = acct113PK(i + h*7)
			}
			pj, _ := json.Marshal(path)
			rj, _ := json.Marshal(resolved)
			var rp interface{}
			if maxResolved > 0 {
				if len(resolved) > maxResolved {
					rj, _ = json.Marshal(resolved[:maxResolved])
				}
				rp = string(rj)
			}
			if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp, resolved_path)
				VALUES (?, ?, ?, ?, 'RX', -5.0, -90.0, 5, ?, ?, ?)`,
				obsID, i, fmt.Sprintf("obs%d", j), names[j], string(pj), ts.Format(time.RFC3339), rp); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if _, err := conn.Exec("COMMIT"); err != nil {
		tb.Fatal(err)
	}
}

func acct113Load(tb testing.TB, dbPath string) *PacketStore {
	tb.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		tb.Fatal(err)
	}
	store := NewPacketStore(db, &PacketStoreConfig{})
	if err := store.Load(); err != nil {
		tb.Fatal(err)
	}
	return store
}

// heapOf returns the heap growth of building a store with build(), after a
// forced GC on both sides.
func heapOf(build func() *PacketStore) (*PacketStore, int64) {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m0)
	s := build()
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m1)
	return s, int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
}

// TestTrackedBytesVsHeap_Measure_113 prints trackedBytes against the real heap
// for synthetic stores. It runs only with CORESCOPE_TRACKED_BYTES_MEASURE set
// to a comma-separated list of transmission counts, e.g. "100000,500000":
//
//	CORESCOPE_TRACKED_BYTES_MEASURE=100000,500000 go test -run Measure_113 -v -timeout 30m
//
// For each size it loads the store with no resolved relays and with 1, 2 and
// all resolved hops per observation, so the marginal heap per resolved relay
// key and per pathHopResolved record can be read off. It compiles against
// master too, for the "before" numbers.
func TestTrackedBytesVsHeap_Measure_113(t *testing.T) {
	sizes := os.Getenv("CORESCOPE_TRACKED_BYTES_MEASURE")
	if sizes == "" {
		t.Skip("set CORESCOPE_TRACKED_BYTES_MEASURE=100000,500000 to run")
	}
	type result struct {
		tracked, heap int64
		keys, records int
		obs           int
	}
	for _, field := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil {
			t.Fatal(err)
		}
		run := func(maxResolved int, warm bool) result {
			dbPath := filepath.Join(t.TempDir(), "m.db")
			acct113CreateDBOpts(t, dbPath, n, 0, maxResolved)
			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			store, heap := heapOf(func() *PacketStore {
				s := NewPacketStore(db, &PacketStoreConfig{})
				if err := s.Load(); err != nil {
					t.Fatal(err)
				}
				if warm { // what analytics do: parse every transmission's JSON
					for _, tx := range s.packets {
						tx.ParsedDecoded()
					}
				}
				return s
			})
			r := result{tracked: store.trackedBytes, heap: heap, records: len(store.pathHopResolved), obs: store.totalObs}
			for _, h := range store.pathHopResolved {
				r.keys += len(h)
			}
			runtime.KeepAlive(store)
			db.conn.Close()
			return r
		}
		mb := func(b int64) float64 { return float64(b) / 1048576 }
		base := run(0, false)
		t.Logf("N=%d tx, %d obs", n, base.obs)
		t.Logf("  no resolved relays, decode cache cold: tracked %.1f MB  heap %.1f MB  tracked/heap %.2f", mb(base.tracked), mb(base.heap), float64(base.tracked)/float64(base.heap))
		warm := run(0, true)
		t.Logf("  no resolved relays, decode cache warm: tracked %.1f MB  heap %.1f MB  tracked/heap %.2f", mb(warm.tracked), mb(warm.heap), float64(warm.tracked)/float64(warm.heap))
		base = warm // the resolved-relay rows below are measured warm
		for _, m := range []int{1, 2, 99} {
			r := run(m, true)
			t.Logf("  resolved hops <= %d per obs: %d keys in %d records (%.1f keys/record): tracked %.1f MB  heap %.1f MB  tracked/heap %.2f",
				m, r.keys, r.records, float64(r.keys)/float64(r.records), mb(r.tracked), mb(r.heap), float64(r.tracked)/float64(r.heap))
			t.Logf("    marginal per record: heap %.1f B, tracked %.1f B  (per key: heap %.1f B, tracked %.1f B)",
				float64(r.heap-base.heap)/float64(r.records), float64(r.tracked-base.tracked)/float64(r.records),
				float64(r.heap-base.heap)/float64(r.keys), float64(r.tracked-base.tracked)/float64(r.keys))
		}
	}
}
