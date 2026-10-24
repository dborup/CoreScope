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
	"unsafe"
)

// stringDataPtr is the address of s's backing bytes. Two strings share one
// copy exactly when their data pointers are equal, which is what the sharing
// pool must achieve and what a clone must break.
func stringDataPtr(s string) uintptr {
	if s == "" {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.StringData(s)))
}

// Synthetic data and measurement harness for #489 (bounded sharing of the
// observer metadata strings). It compiles against master too, so the same
// generator produces the "before" and the "after" numbers.
//
// The shape follows the large fork instance: about 65 observers and 2.5M
// observations, i.e. a handful of distinct observer IDs, names, IATA codes
// and directions repeated across hundreds of thousands of rows. Observations
// per transmission follow a log-normal distribution with the production
// median and spread (median 15, p90 ~35, p99 ~70).

const (
	obs489Observers = 65
	obs489Median    = 15.0
	obs489Sigma     = 0.8
)

// obs489ObserverID is a 64-hex pubkey, the dominant per-observation string.
func obs489ObserverID(k int) string {
	return fmt.Sprintf("%064x", uint64(k+1)*0x9e3779b97f4a7c15)
}

// obs489ObserverName returns a station name of a realistic length (8 to 26
// bytes), so the name term of the measurement is not a single fixed size.
func obs489ObserverName(k int) string {
	towns := []string{"Nord", "Soenderbro", "Havnen", "Vestervig", "Oesterskov", "Midtby", "Lufthavn"}
	kinds := []string{"Repeater", "Room", "Companion", "Sensor"}
	return fmt.Sprintf("%s %s %02d", towns[k%len(towns)], kinds[(k/len(towns))%len(kinds)], k)
}

// obs489IATA returns a 3-letter code, empty for every seventh observer (the
// fixture has observers without one).
func obs489IATA(k int) string {
	if k%7 == 6 {
		return ""
	}
	codes := []string{"CPH", "AAR", "BLL", "AAL", "ODE", "RNN", "EBJ", "SGD"}
	return codes[k%len(codes)]
}

// obs489Direction is "RX" or "TX"; a tenth of the rows carry neither.
func obs489Direction(j int) string {
	switch {
	case j%10 == 9:
		return ""
	case j%3 == 0:
		return "TX"
	default:
		return "RX"
	}
}

type obs489Row struct {
	id       int
	observer int // index into the observer pool
	dir      string
	path     []string
	resolved []string
	ts       time.Time
}

type obs489Tx struct {
	id        int
	hash      string
	firstSeen time.Time
	rows      []obs489Row
}

// obs489Data builds transmissions until it holds at least numObs observation
// rows, each row one observer on one path. Every row of a transmission gets a
// distinct observer/path pair, so none of them is dropped by the #411 dedup
// and the observation count is the row count.
//
// The first half of the transmissions are 3 days old and the second half one
// hour old, so a store with HotStartupHours=2 loads the recent half hot and
// the old half through the background fill, and a retention window below an
// hour evicts every one of them.
//
// Observation ids are assigned in two phases, as for #411: every
// transmission's first row gets an id before any later row, so the upsert
// path can ingest the first rows as new transmissions and the rest as
// observations of transmissions it already holds.
func obs489Data(numObs int, seed int64) []obs489Tx {
	rng := rand.New(rand.NewSource(seed))
	now := time.Now().UTC().Truncate(time.Second)
	var txs []obs489Tx
	total, i := 0, 0
	for total < numObs {
		i++
		rows := int(math.Round(math.Exp(math.Log(obs489Median) + obs489Sigma*rng.NormFloat64())))
		if rows < 1 {
			rows = 1
		}
		if total+rows > numObs {
			rows = numObs - total
		}
		fs := now.Add(-time.Hour + time.Duration(total)*time.Millisecond)
		if total*2 < numObs {
			fs = now.Add(-72*time.Hour + time.Duration(total)*time.Millisecond)
		}
		tx := obs489Tx{id: i, hash: fmt.Sprintf("%016x", uint64(i)*0x2545f4914f6cdd1d), firstSeen: fs}
		for j := 0; j < rows; j++ {
			obs := (j + rng.Intn(obs489Observers)) % obs489Observers
			hops := 1 + (i+j)%3
			path := make([]string, hops)
			resolved := make([]string, hops)
			for h := range path {
				path[h] = acct113Hop(i, h, j)
				resolved[h] = acct113PK(i + h*7 + j)
			}
			tx.rows = append(tx.rows, obs489Row{observer: obs, dir: obs489Direction(j),
				path: path, resolved: resolved, ts: fs.Add(time.Duration(j) * time.Millisecond)})
		}
		total += rows
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

// obs489MaxObsID is the highest observation id obs489Data assigned.
func obs489MaxObsID(txs []obs489Tx) int {
	max := 0
	for _, tx := range txs {
		for _, r := range tx.rows {
			if r.id > max {
				max = r.id
			}
		}
	}
	return max
}

// obs489Create writes the v3 schema (observer_idx + observers join, what the
// fork instances run) and the observer pool.
func obs489Create(tb testing.TB, dbPath string) *sql.DB {
	tb.Helper()
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		tb.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT, first_seen TEXT,
			route_type INTEGER, payload_type INTEGER, payload_version INTEGER, decoded_json TEXT)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER,
			direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT,
			timestamp INTEGER, resolved_path TEXT)`,
		`CREATE TABLE observers (rowid INTEGER PRIMARY KEY, id TEXT, name TEXT, iata TEXT)`,
		`CREATE TABLE nodes (pubkey TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL,
			last_seen TEXT, first_seen TEXT, frequency REAL)`,
		`CREATE TABLE schema_version (version INTEGER)`,
		`INSERT INTO schema_version (version) VALUES (3)`,
		`CREATE INDEX idx_tx_first_seen ON transmissions(first_seen)`,
		`CREATE INDEX idx_obs_tx ON observations(transmission_id)`,
	} {
		if _, err := conn.Exec(q); err != nil {
			tb.Fatalf("setup: %v\n%s", err, q)
		}
	}
	for k := 0; k < obs489Observers; k++ {
		if _, err := conn.Exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, ?)`,
			k+1, obs489ObserverID(k), obs489ObserverName(k), obs489IATA(k)); err != nil {
			tb.Fatal(err)
		}
	}
	return conn
}

// obs489Write inserts txs; rows selects which observation rows of each are
// written (nil: all of them).
func obs489Write(tb testing.TB, conn *sql.DB, txs []obs489Tx, withTx bool, rows func(j int) bool) {
	tb.Helper()
	dbtx, err := conn.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	txStmt, err := dbtx.Prepare(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
		VALUES (?, ?, ?, ?, 1, 5, 1, ?)`)
	if err != nil {
		tb.Fatal(err)
	}
	obsStmt, err := dbtx.Prepare(`INSERT INTO observations (id, transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp, resolved_path)
		VALUES (?, ?, ?, ?, -5.0, -90.0, 5, ?, ?, ?)`)
	if err != nil {
		tb.Fatal(err)
	}
	for _, tx := range txs {
		if withTx {
			decoded := fmt.Sprintf(`{"type":"GRP_TXT","channel":"#test","text":"message number %d","sender":"node%d"}`, tx.id, tx.id%50)
			if _, err := txStmt.Exec(tx.id, fmt.Sprintf("1500%036d", tx.id), tx.hash, tx.firstSeen.Format(time.RFC3339), decoded); err != nil {
				tb.Fatal(err)
			}
		}
		for j, r := range tx.rows {
			if rows != nil && !rows(j) {
				continue
			}
			pj, _ := json.Marshal(r.path)
			rj, _ := json.Marshal(r.resolved)
			if _, err := obsStmt.Exec(r.id, tx.id, r.observer+1, r.dir, string(pj), r.ts.Unix(), string(rj)); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := dbtx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// obs489DB writes txs in full to a new database file and returns its path.
func obs489DB(tb testing.TB, txs []obs489Tx) string {
	tb.Helper()
	dbPath := filepath.Join(tb.TempDir(), "obs489.db")
	conn := obs489Create(tb, dbPath)
	obs489Write(tb, conn, txs, true, nil)
	conn.Close()
	return dbPath
}

// obs489StringStats is what a store's observations hold in the four observer
// metadata fields: the logical bytes (one copy per observation) against the
// bytes of the distinct values, which is what a perfect sharing pool retains.
type obs489StringStats struct {
	obs             int
	logicalBytes    int64
	distinctBytes   int64
	distinctValues  int
	pointerDistinct int // distinct string data pointers (actual copies held)
	perField        map[string]int
}

// obs489StatsOfStore walks every observation of s and returns its string
// stats. It counts distinct VALUES and distinct string data POINTERS: with no
// sharing the pointer count is one per observation, with sharing it collapses
// to the value count.
func obs489StatsOfStore(s *PacketStore) obs489StringStats {
	out := obs489StringStats{perField: map[string]int{}}
	values := map[string]bool{}
	ptrs := map[uintptr]bool{}
	fieldValues := map[string]map[string]bool{
		"observerID": {}, "observerName": {}, "observerIATA": {}, "direction": {},
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, tx := range s.packets {
		for _, o := range tx.Observations {
			out.obs++
			for name, v := range map[string]string{
				"observerID": o.ObserverID, "observerName": o.ObserverName,
				"observerIATA": o.ObserverIATA, "direction": o.Direction,
			} {
				if v == "" {
					continue
				}
				out.logicalBytes += int64(len(v))
				fieldValues[name][v] = true
				if !values[v] {
					values[v] = true
					out.distinctBytes += int64(len(v))
				}
				ptrs[stringDataPtr(v)] = true
			}
		}
	}
	out.distinctValues = len(values)
	out.pointerDistinct = len(ptrs)
	for name, vs := range fieldValues {
		out.perField[name] = len(vs)
	}
	return out
}

// TestObsStrings489_MeasureFixture reports the observer metadata strings held
// after loading test-fixtures/e2e-fixture.db. It compiles against master too.
//
//	CORESCOPE_OBS489_MEASURE=fixture go test -run ObsStrings489_MeasureFixture -v
func TestObsStrings489_MeasureFixture(t *testing.T) {
	if os.Getenv("CORESCOPE_OBS489_MEASURE") == "" {
		t.Skip("set CORESCOPE_OBS489_MEASURE=fixture to run")
	}
	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "fixture.db")
	if err := os.WriteFile(dbPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.conn.Close()
	store, heap := heapOf(func() *PacketStore {
		s := NewPacketStore(db, &PacketStoreConfig{})
		// main.go's startup order: the neighbour graph is loaded before the
		// packet store (Load panics otherwise, see the #1643 invariant).
		s.graph.Store(loadNeighborEdgesFromDB(db.conn))
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		s.WaitIndexesReady(60 * time.Second)
		return s
	})
	st := obs489StatsOfStore(store)
	t.Logf("fixture: tx=%d obs=%d heap=%.2f MiB trackedBytes=%.2f MiB",
		len(store.packets), st.obs, float64(heap)/1048576, float64(store.trackedBytes)/1048576)
	t.Logf("fixture: observer strings logical=%d B distinct=%d B (%d values, %.1f%% duplicate) copies=%d perField=%v",
		st.logicalBytes, st.distinctBytes, st.distinctValues,
		100*float64(st.logicalBytes-st.distinctBytes)/float64(st.logicalBytes), st.pointerDistinct, st.perField)
	runtime.KeepAlive(store)
}

// TestObsStrings489_Measure loads a synthetic store for each observation count
// in CORESCOPE_OBS489_MEASURE and reports heap after load, trackedBytes, the
// retained observer-string bytes and the Load wall time.
//
//	CORESCOPE_OBS489_MEASURE=250000,1000000,2500000 go test -run ObsStrings489_Measure$ -v -timeout 60m
func TestObsStrings489_Measure(t *testing.T) {
	sizes := os.Getenv("CORESCOPE_OBS489_MEASURE")
	if sizes == "" {
		t.Skip("set CORESCOPE_OBS489_MEASURE=250000 to run")
	}
	for _, field := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil {
			continue
		}
		txs := obs489Data(n, 489)
		dist := make([]int, 0, len(txs))
		for _, tx := range txs {
			dist = append(dist, len(tx.rows))
		}
		sort.Ints(dist)
		t.Logf("n=%d tx=%d obs/tx mean=%.1f p50=%d p90=%d p99=%d max=%d",
			n, len(txs), float64(n)/float64(len(txs)),
			obs411Pct(dist, .5), obs411Pct(dist, .9), obs411Pct(dist, .99), dist[len(dist)-1])

		dbPath := obs489DB(t, txs)
		db, err := OpenDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		var t0 time.Time
		var loadMS int64
		store, heap := heapOf(func() *PacketStore {
			s := NewPacketStore(db, &PacketStoreConfig{})
			t0 = time.Now()
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			loadMS = time.Since(t0).Milliseconds()
			s.WaitIndexesReady(10 * time.Minute)
			return s
		})
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		st := obs489StatsOfStore(store)
		t.Logf("n=%d heap=%.1f MiB trackedBytes=%.1f MiB sysMB=%.0f load=%d ms obs=%d",
			n, float64(heap)/1048576, float64(store.trackedBytes)/1048576,
			float64(ms.Sys)/1048576, loadMS, st.obs)
		t.Logf("n=%d observer strings logical=%.2f MiB distinct=%d B (%d values, %.2f%% duplicate) copies=%d perField=%v",
			n, float64(st.logicalBytes)/1048576, st.distinctBytes, st.distinctValues,
			100*float64(st.logicalBytes-st.distinctBytes)/float64(st.logicalBytes), st.pointerDistinct, st.perField)
		runtime.KeepAlive(store)
		db.conn.Close()
		store = nil
		runtime.GC()
	}
}

// BenchmarkLoad_Obs489 is a hot Load of obs489Bench observations with the
// production observation-per-transmission distribution, plus heap after load
// as a custom metric. It compiles against master and against the red commit,
// so the same benchmark gives the before and after numbers.
//
//	go test -run '^$' -bench Load_Obs489 -benchtime 3x -count 6 .
const obs489Bench = 250000

func BenchmarkLoad_Obs489(b *testing.B) {
	dbPath := obs489DB(b, obs489Data(obs489Bench, 489))
	db, err := OpenDB(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer db.conn.Close()
	b.ReportAllocs()
	b.ResetTimer()
	var heap, tracked int64
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		runtime.GC()
		runtime.GC()
		var m0 runtime.MemStats
		runtime.ReadMemStats(&m0)
		b.StartTimer()
		s := NewPacketStore(db, &PacketStoreConfig{})
		if err := s.Load(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		s.WaitIndexesReady(10 * time.Minute)
		runtime.GC()
		runtime.GC()
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)
		heap += int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
		tracked += s.trackedBytes
		runtime.KeepAlive(s)
		b.StartTimer()
	}
	b.ReportMetric(float64(heap)/float64(b.N)/1048576, "heapMiB")
	b.ReportMetric(float64(tracked)/float64(b.N)/1048576, "trackedMiB")
}
