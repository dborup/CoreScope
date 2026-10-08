package main

// Perf proof for PR #176 (AGENTS.md rule 0: a perf claim needs data).
//
// Not a pass/fail test. It uses only identifiers that exist on master
// (OpenDB, DB.GetNodes, DB.Close), so the same file compiles against master
// and against this branch and the two binaries can be run alternately — the
// before/after difference is then the region cache itself, not a rewritten
// measurement:
//
//	# one large database, reused by both binaries
//	CORESCOPE_PERF_176_GEN=/path/big.db CORESCOPE_PERF_176_OBS=1500000 \
//	  go test -run TestPerf_GenerateRegionDB_176 -timeout 2h .
//
//	go test -c -o after.test .            # on this branch
//	go test -c -o before.test .           # on master, same file copied in
//	for i in 1 2 3 4 5 6 7; do
//	  CORESCOPE_PERF_176=1 CORESCOPE_PERF_176_DB=/path/big.db ./before.test -test.run TestPerf_NodesRegionPageLoad_176
//	  CORESCOPE_PERF_176=1 CORESCOPE_PERF_176_DB=/path/big.db ./after.test  -test.run TestPerf_NodesRegionPageLoad_176
//	done
//
// The measured unit is one Nodes page load with a region selected: what
// public/nodes.js does in fetchAllNodes(), four pages of 500. GetNodes
// evaluates the region predicate twice per call (COUNT(*) and the page), so
// the uncached shape pays for it eight times per page load.

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

const (
	perf176PageSize = 500
	perf176Pages    = 4
	perf176Region   = "SJC"
)

func perf176Median(d []time.Duration) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}

// perf176Regions are the observer regions the generated database uses. SJC
// (the measured one) holds a quarter of the observers.
var perf176Regions = []string{"SJC", "SJC", "SJC", "SJC", "SFO", "OAK", "MRY", "LAX", "PDX", "SEA", "BUR", "SAN", "SMF", "RNO", "LAS", "PHX"}

// TestPerf_GenerateRegionDB_176 writes a database shaped like a busy
// instance: many observations per ADVERT, most transmissions not ADVERTs,
// and ANALYZE run at the end so the planner sees sqlite_stat1 the way a live
// database does (#2058 — that is what makes the uncached subquery drive off
// the region's observers).
func TestPerf_GenerateRegionDB_176(t *testing.T) {
	path := os.Getenv("CORESCOPE_PERF_176_GEN")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_176_GEN=<db path> to generate the perf database")
	}
	obsTarget, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_176_OBS"))
	if obsTarget <= 0 {
		obsTarget = 1500000
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", "file:"+path+"?_journal_mode=WAL&_synchronous=OFF")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// The v3 schema of the columns this query touches, with the indexes
	// dbschema.go creates for them.
	for _, stmt := range []string{
		`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL,
			last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0, battery_mv INTEGER,
			temperature_c REAL, foreign_advert INTEGER DEFAULT 0)`,
		`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT, iata TEXT, last_seen TEXT,
			first_seen TEXT, packet_count INTEGER DEFAULT 0, inactive INTEGER DEFAULT 0,
			last_packet_at TEXT DEFAULT NULL)`,
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY AUTOINCREMENT, raw_hex TEXT NOT NULL,
			hash TEXT NOT NULL UNIQUE, first_seen TEXT NOT NULL, route_type INTEGER,
			payload_type INTEGER, payload_version INTEGER, decoded_json TEXT,
			channel_hash TEXT DEFAULT NULL, from_pubkey TEXT DEFAULT NULL,
			created_at TEXT DEFAULT (datetime('now')))`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY AUTOINCREMENT,
			transmission_id INTEGER NOT NULL REFERENCES transmissions(id), observer_idx INTEGER,
			direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT,
			timestamp INTEGER NOT NULL, resolved_path TEXT, raw_hex TEXT)`,
		`CREATE INDEX idx_transmissions_payload_type ON transmissions(payload_type)`,
		`CREATE INDEX idx_transmissions_from_pubkey ON transmissions(from_pubkey)`,
		`CREATE INDEX idx_transmissions_hash ON transmissions(hash)`,
		`CREATE INDEX idx_observations_timestamp ON observations(timestamp)`,
		`CREATE INDEX idx_observations_transmission_id ON observations(transmission_id)`,
		`CREATE INDEX idx_observations_observer_idx ON observations(observer_idx)`,
		`CREATE INDEX idx_observations_tx_ts ON observations(transmission_id, timestamp)`,
		`CREATE INDEX idx_nodes_last_seen ON nodes(last_seen)`,
	} {
		mustExec(stmt)
	}

	const observers = 64
	const nodes = 4000
	rng := rand.New(rand.NewSource(1764))
	now := time.Now().UTC()
	begin := func() *sql.Tx {
		tx, err := conn.Begin()
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	tx := begin()
	for i := range observers {
		iata := perf176Regions[i%len(perf176Regions)]
		if _, err := tx.Exec(`INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen)
			VALUES (?, ?, ?, ?, ?, ?)`, i+1, fmt.Sprintf("obs-%03d", i), fmt.Sprintf("Observer %03d", i),
			iata, now.Format(time.RFC3339), now.Add(-90*24*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	pubkeys := make([]string, nodes)
	for i := range nodes {
		pubkeys[i] = fmt.Sprintf("%016x", 0x1000000000000000+i)
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count)
			VALUES (?, ?, 'repeater', ?, ?, 10)`, pubkeys[i], fmt.Sprintf("node-%04d", i),
			now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339),
			now.Add(-90*24*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Four fifths of the transmissions are not ADVERTs, which is what makes
	// the payload_type index worth anything, and each ADVERT is heard by a
	// handful of observers.
	rawHex := make([]byte, 240)
	for i := range rawHex {
		rawHex[i] = "0123456789abcdef"[i%16]
	}
	start := time.Now()
	obs, txCount := 0, 0
	tx = begin()
	for obs < obsTarget {
		isAdvert := txCount%5 == 0
		payload := 2
		var from any
		var decoded string
		if isAdvert {
			payload = 4
			pk := pubkeys[rng.Intn(nodes)]
			from = pk
			decoded = `{"type":"ADVERT","pubKey":"` + pk + `","name":"n"}`
		} else {
			decoded = `{"type":"TXT_MSG","text":"x"}`
		}
		res, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
			VALUES (?, ?, ?, 1, ?, ?, ?)`, string(rawHex), fmt.Sprintf("h%012d", txCount),
			now.Add(-time.Duration(txCount)*time.Second).Format(time.RFC3339), payload, decoded, from)
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		for range 1 + rng.Intn(3) {
			if _, err := tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
				VALUES (?, ?, 8.5, -95, '[]', ?)`, txID, 1+rng.Intn(observers),
				now.Add(-time.Duration(txCount)*time.Second).Unix()); err != nil {
				t.Fatal(err)
			}
			obs++
		}
		txCount++
		if txCount%20000 == 0 {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			t.Logf("… %d transmissions, %d observations (%v)", txCount, obs, time.Since(start).Round(time.Second))
			tx = begin()
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExec(`ANALYZE`)
	mustExec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RESULT generated db=%s transmissions=%d observations=%d nodes=%d observers=%d bytes=%d in %v\n",
		path, txCount, obs, nodes, observers, st.Size(), time.Since(start).Round(time.Second))
}

// TestPerf_NodesRegionPageLoad_176 times one Nodes page load with a region
// selected: four GetNodes pages of 500. It prints the cold load (the first
// one on a freshly opened database, which on this branch includes the one
// full membership scan) and then the median of the following loads, which is
// what a reload or a second tab pays.
func TestPerf_NodesRegionPageLoad_176(t *testing.T) {
	runs, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_176"))
	if runs <= 0 {
		t.Skip("set CORESCOPE_PERF_176=<page loads> and CORESCOPE_PERF_176_DB=<db path> to measure")
	}
	path := os.Getenv("CORESCOPE_PERF_176_DB")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_176_DB=<db path> to measure")
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pageLoad := func() (time.Duration, int) {
		t0 := time.Now()
		total := 0
		for page := range perf176Pages {
			_, n, _, err := db.GetNodes(perf176PageSize, page*perf176PageSize,
				"", "", "", "", "", perf176Region)
			if err != nil {
				t.Fatal(err)
			}
			total = n
		}
		return time.Since(t0), total
	}

	cold, total := pageLoad()
	var warm []time.Duration
	for range runs {
		d, got := pageLoad()
		if got != total {
			t.Fatalf("region total changed between page loads: %d then %d", total, got)
		}
		warm = append(warm, d)
	}
	fmt.Printf("RESULT nodes-region-pageload region=%s pages=%dx%d matched_nodes=%d cold_ns=%d warm_median_ns=%d runs=%d\n",
		perf176Region, perf176Pages, perf176PageSize, total, cold.Nanoseconds(),
		perf176Median(warm).Nanoseconds(), runs)
}
