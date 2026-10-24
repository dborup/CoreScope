package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Synthetic data for #411 (observation dedup without eager per-transmission
// maps). It compiles against master too, so the same generator gives the
// "before" and "after" numbers.
//
// Observations per transmission follow a log-normal distribution (median
// obs411Median, sigma obs411Sigma): most transmissions are heard a handful of
// times, a few dozens of times. Each row is one observer (from a pool of
// obs411Observers 64-hex IDs, with an occasional unknown observer) on one of
// a few paths, so the same observer on a second path and exact repeats (which
// the store drops) occur naturally. adversarial transmissions carry
// obs411AdversarialRows distinct observer/path rows each.

const (
	obs411Observers       = 48
	obs411Median          = 4.0
	obs411Sigma           = 0.75
	obs411AdversarialRows = 600
)

type obs411Row struct {
	id       int
	observer string // "" for an unknown observer
	path     []string
	resolved []string
	ts       time.Time
}

type obs411Tx struct {
	id        int
	hash      string
	firstSeen time.Time
	rows      []obs411Row
}

func obs411ObserverID(k int) string { return fmt.Sprintf("%064x", uint64(k+1)*0x9e3779b97f4a7c15) }

// obs411Rows returns how many observation rows transmission i gets.
func obs411Rows(rng *rand.Rand) int {
	n := int(math.Round(math.Exp(math.Log(obs411Median) + obs411Sigma*rng.NormFloat64())))
	if n < 1 {
		n = 1
	}
	return n
}

// obs411Data builds n ordinary and adversarial busy transmissions. oldUntil of
// the ordinary ones are 3 days old (for the background fill), the rest are
// within the last hour. Observation ids are assigned in two phases: every
// transmission's first row gets an id before any later row, so the upsert
// path can ingest the first rows as new transmissions and the rest as
// observations of transmissions it already holds.
func obs411Data(n, adversarial, oldUntil int, seed int64) []obs411Tx {
	rng := rand.New(rand.NewSource(seed))
	now := time.Now().UTC().Truncate(time.Second)
	txs := make([]obs411Tx, 0, n+adversarial)
	for i := 1; i <= n+adversarial; i++ {
		fs := now.Add(-time.Hour + time.Duration(i)*time.Millisecond*100)
		if i <= oldUntil {
			fs = now.Add(-72*time.Hour + time.Duration(i)*time.Millisecond*100)
		}
		tx := obs411Tx{id: i, hash: fmt.Sprintf("%016x", uint64(i)*0x2545f4914f6cdd1d), firstSeen: fs}
		rows := obs411Rows(rng)
		busy := i > n
		if busy {
			rows = obs411AdversarialRows
		}
		for j := 0; j < rows; j++ {
			var obs, variant int
			if busy { // every row distinct: observer j%pool on path variant j/pool
				obs, variant = j%obs411Observers, j/obs411Observers
			} else {
				obs, variant = rng.Intn(obs411Observers), rng.Intn(3)
			}
			hops := 1 + (i+variant)%4
			path := make([]string, hops)
			resolved := make([]string, hops)
			for h := range path {
				path[h] = acct113Hop(i, h, variant)
				resolved[h] = acct113PK(i + h*7 + variant)
			}
			observer := obs411ObserverID(obs)
			if !busy && rng.Intn(100) == 0 {
				observer = ""
			}
			tx.rows = append(tx.rows, obs411Row{observer: observer, path: path, resolved: resolved,
				ts: fs.Add(time.Duration(j) * time.Millisecond)})
		}
		txs = append(txs, tx)
	}
	id := 0
	for i := range txs {
		id++
		txs[i].rows[0].id = id
	}
	for i := range txs {
		for j := 1; j < len(txs[i].rows); j++ {
			id++
			txs[i].rows[j].id = id
		}
	}
	return txs
}

// obs411Schema is the acct113 schema (observer_id column, resolved_path).
func obs411Create(tb testing.TB, dbPath string) *sql.DB {
	tb.Helper()
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		tb.Fatal(err)
	}
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
	for k := 0; k < obs411Observers; k++ {
		if _, err := conn.Exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, 'TST')`, k+1, obs411ObserverID(k), fmt.Sprintf("Obs%02d", k)); err != nil {
			tb.Fatal(err)
		}
	}
	return conn
}

// obs411Write inserts txs; rows selects which observation rows of each are
// written (nil: all of them).
func obs411Write(tb testing.TB, conn *sql.DB, txs []obs411Tx, withTx bool, rows func(j int) bool) {
	tb.Helper()
	dbtx, err := conn.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	for _, tx := range txs {
		if withTx {
			decoded := fmt.Sprintf(`{"type":"GRP_TXT","channel":"#test","text":"message number %d with some body text","sender":"node%d"}`, tx.id, tx.id%50)
			if _, err := dbtx.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
				VALUES (?, ?, ?, ?, 1, 5, 1, ?)`, tx.id, fmt.Sprintf("1500%036d", tx.id), tx.hash, tx.firstSeen.Format(time.RFC3339), decoded); err != nil {
				tb.Fatal(err)
			}
		}
		for j, r := range tx.rows {
			if rows != nil && !rows(j) {
				continue
			}
			pj, _ := json.Marshal(r.path)
			rj, _ := json.Marshal(r.resolved)
			var observer interface{}
			if r.observer != "" {
				observer = r.observer
			}
			if _, err := dbtx.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp, resolved_path)
				VALUES (?, ?, ?, 'Obs', 'RX', -5.0, -90.0, 5, ?, ?, ?)`,
				r.id, tx.id, observer, string(pj), r.ts.Format(time.RFC3339), string(rj)); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := dbtx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// obs411DB writes txs in full to a new database file and returns its path.
func obs411DB(tb testing.TB, txs []obs411Tx) string {
	tb.Helper()
	dbPath := filepath.Join(tb.TempDir(), "obs411.db")
	conn := obs411Create(tb, dbPath)
	defer conn.Close()
	obs411Write(tb, conn, txs, true, nil)
	return dbPath
}

// obs411Distribution is the per-transmission count of distinct
// observer/path pairs, i.e. what the store keeps after dedup.
func obs411Distribution(txs []obs411Tx) []int {
	out := make([]int, 0, len(txs))
	for _, tx := range txs {
		seen := map[string]bool{}
		for _, r := range tx.rows {
			pj, _ := json.Marshal(r.path)
			seen[r.observer+"|"+string(pj)] = true
		}
		out = append(out, len(seen))
	}
	sort.Ints(out)
	return out
}

func obs411Pct(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

// TestObsDedup411_Measure prints the distribution of the synthetic set and
// the heap and allocations of a hot Load of it. It runs only with
// CORESCOPE_OBS411_MEASURE set to a comma-separated list of transmission
// counts, e.g. "30000":
//
//	CORESCOPE_OBS411_MEASURE=30000 go test -run ObsDedup411_Measure -v
func TestObsDedup411_Measure(t *testing.T) {
	sizes := os.Getenv("CORESCOPE_OBS411_MEASURE")
	if sizes == "" {
		t.Skip("set CORESCOPE_OBS411_MEASURE=30000 to run")
	}
	for _, field := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil {
			t.Fatal(err)
		}
		txs := obs411Data(n, 0, 0, 411)
		d := obs411Distribution(txs)
		rows, kept := 0, 0
		for i, tx := range txs {
			rows += len(tx.rows)
			kept += d[i]
		}
		hist := map[string]int{}
		for _, k := range d {
			switch {
			case k <= 8:
				hist["1-8"]++
			case k <= 16:
				hist["9-16"]++
			case k <= 32:
				hist["17-32"]++
			case k <= 64:
				hist["33-64"]++
			default:
				hist[">64"]++
			}
		}
		t.Logf("n=%d rows=%d kept=%d mean=%.2f p50=%d p90=%d p99=%d p99.9=%d max=%d buckets 1-8:%d 9-16:%d 17-32:%d 33-64:%d >64:%d",
			n, rows, kept, float64(kept)/float64(n), obs411Pct(d, .5), obs411Pct(d, .9), obs411Pct(d, .99), obs411Pct(d, .999), d[len(d)-1],
			hist["1-8"], hist["9-16"], hist["17-32"], hist["33-64"], hist[">64"])

		dbPath := obs411DB(t, txs)
		db, err := OpenDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		store, heap := heapOf(func() *PacketStore {
			s := NewPacketStore(db, &PacketStoreConfig{})
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			if !s.WaitIndexesReady(60 * time.Second) {
				t.Fatal("index builds did not finish")
			}
			return s
		})
		runtime.ReadMemStats(&m1)
		t.Logf("n=%d heap after load=%.1f MiB (%.0f B/tx) mallocs=%d trackedBytes=%.1f MiB totalObs=%d",
			n, float64(heap)/1048576, float64(heap)/float64(n), m1.Mallocs-m0.Mallocs, float64(store.trackedBytes)/1048576, store.totalObs)
		runtime.KeepAlive(store)
		db.conn.Close()
	}
}

// BenchmarkLoad_Skewed30K_411 is a hot Load of 30K transmissions with the
// skewed distribution above, plus heap after load as a custom metric.
func BenchmarkLoad_Skewed30K_411(b *testing.B) {
	dbPath := obs411DB(b, obs411Data(30000, 0, 0, 411))
	db, err := OpenDB(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer db.conn.Close()
	b.ReportAllocs()
	b.ResetTimer()
	var heap int64
	for i := 0; i < b.N; i++ {
		var s *PacketStore
		b.StopTimer()
		runtime.GC()
		var m0 runtime.MemStats
		runtime.ReadMemStats(&m0)
		b.StartTimer()
		s = NewPacketStore(db, &PacketStoreConfig{})
		if err := s.Load(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		s.WaitIndexesReady(60 * time.Second)
		runtime.GC()
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)
		heap += int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
		runtime.KeepAlive(s)
		b.StartTimer()
	}
	b.ReportMetric(float64(heap)/float64(b.N)/1048576, "heapMiB")
}
