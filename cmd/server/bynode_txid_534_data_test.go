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

// Synthetic data and measurement harness for #534 (byNode de-dup sets keyed
// by transmission ID instead of hash). Everything here reaches the sets only
// through Load, addToByNode and the byNode lists, so the same file compiles
// against master and gives the "before" numbers.
//
// The shape follows the large fork instance: about 150K transmissions, 2K
// nodes and 2.5M observations over 65 observers. A quarter of the
// transmissions are adverts (one decoded pubkey); every transmission is
// relayed by a small pool of nodes picked with a Zipf skew (a few hubs relay
// most traffic), and each observation's resolved_path names 1 to 3 of them.
// That gives about 4.5 node-set entries per transmission, what the e2e
// fixture holds (2247 entries for 499 transmissions).

const (
	bn534Observers = 65
	bn534ObsSigma  = 0.8
)

// bn534PK is the 64-hex pubkey of node k. The top byte varies with k, so the
// 2-hex path prefixes spread over the whole range like real ones do.
func bn534PK(k int) string {
	return fmt.Sprintf("%016x", uint64(k+1)*0x9e3779b97f4a7c15) + strings.Repeat("5a", 24)
}

type bn534Obs struct {
	id       int
	observer int
	relays   []int // node indexes, in path order
	ts       time.Time
}

type bn534Tx struct {
	id        int
	hash      string
	firstSeen time.Time
	origin    int // advert origin node, -1 for a non-advert
	obs       []bn534Obs
}

// bn534Data builds numTx transmissions with about numObs observations in
// total, over numNodes nodes. Deterministic for a seed.
func bn534Data(numTx, numNodes, numObs int, seed int64) []bn534Tx {
	rng := rand.New(rand.NewSource(seed))
	zipf := rand.NewZipf(rng, 1.15, 8, uint64(numNodes-1))
	// Log-normal observations per transmission with the production spread;
	// the median is chosen so the mean hits numObs/numTx.
	median := float64(numObs) / float64(numTx) / math.Exp(bn534ObsSigma*bn534ObsSigma/2)
	now := time.Now().UTC().Truncate(time.Second)
	span := 6 * 24 * time.Hour
	txs := make([]bn534Tx, numTx)
	obsID := 0
	for i := range txs {
		tx := &txs[i]
		tx.id = i + 1
		tx.hash = fmt.Sprintf("%016x", uint64(i+1)*0x2545f4914f6cdd1d)
		tx.firstSeen = now.Add(-span + time.Duration(float64(span)*float64(i)/float64(numTx)))
		tx.origin = -1
		if i%4 == 0 {
			tx.origin = rng.Intn(numNodes)
		}
		pool := 1 + int(math.Round(math.Exp(math.Log(2.9)+0.7*rng.NormFloat64())))
		if pool > 12 {
			pool = 12
		}
		relays := make([]int, 0, pool)
		seen := map[int]bool{}
		for len(relays) < pool {
			k := int(zipf.Uint64())
			if !seen[k] {
				seen[k] = true
				relays = append(relays, k)
			}
		}
		rows := int(math.Round(math.Exp(math.Log(median) + bn534ObsSigma*rng.NormFloat64())))
		if rows < 1 {
			rows = 1
		}
		if rows > bn534Observers {
			rows = bn534Observers // one row per observer: none is dropped by the #411 dedup
		}
		first := rng.Intn(bn534Observers)
		for j := 0; j < rows; j++ {
			hops := 1 + rng.Intn(3)
			if hops > len(relays) {
				hops = len(relays)
			}
			path := make([]int, hops)
			for h, p := range rng.Perm(len(relays))[:hops] {
				path[h] = relays[p]
			}
			obsID++
			tx.obs = append(tx.obs, bn534Obs{id: obsID, observer: (first + j) % bn534Observers,
				relays: path, ts: tx.firstSeen.Add(time.Duration(j) * time.Second)})
		}
	}
	return txs
}

// bn534WriteDB writes the data in the fork's v3 layout (observer_idx joined to
// observers, which carry an IATA code so the region filter has observers to
// match) plus a nodes table GetBulkHealth reads.
func bn534WriteDB(tb testing.TB, dbPath string, txs []bn534Tx, numNodes int) {
	tb.Helper()
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		tb.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT NOT NULL UNIQUE, first_seen TEXT,
			route_type INTEGER, payload_type INTEGER, payload_version INTEGER, decoded_json TEXT)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER,
			direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT,
			timestamp INTEGER, resolved_path TEXT)`,
		`CREATE TABLE observers (rowid INTEGER PRIMARY KEY, id TEXT, name TEXT, iata TEXT)`,
		`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL,
			last_seen TEXT, first_seen TEXT)`,
		`CREATE TABLE schema_version (version INTEGER)`,
		`INSERT INTO schema_version (version) VALUES (3)`,
		`CREATE INDEX idx_tx_first_seen ON transmissions(first_seen)`,
		`CREATE INDEX idx_obs_tx ON observations(transmission_id)`,
	} {
		if _, err := conn.Exec(q); err != nil {
			tb.Fatalf("setup: %v\n%s", err, q)
		}
	}
	dbtx, err := conn.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	for k := 0; k < bn534Observers; k++ {
		if _, err := dbtx.Exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, ?)`,
			k+1, obs489ObserverID(k), obs489ObserverName(k), obs489IATA(k)); err != nil {
			tb.Fatal(err)
		}
	}
	seen := time.Now().UTC().Format(time.RFC3339)
	for k := 0; k < numNodes; k++ {
		if _, err := dbtx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?, ?, 'repeater', 55.6, 12.5, ?, ?)`,
			bn534PK(k), fmt.Sprintf("R%04d", k), seen, seen); err != nil {
			tb.Fatal(err)
		}
	}
	txStmt, err := dbtx.Prepare(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
		VALUES (?, ?, ?, ?, 1, ?, 1, ?)`)
	if err != nil {
		tb.Fatal(err)
	}
	obsStmt, err := dbtx.Prepare(`INSERT INTO observations (id, transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp, resolved_path)
		VALUES (?, ?, ?, 'RX', -5.0, -90.0, 5, ?, ?, ?)`)
	if err != nil {
		tb.Fatal(err)
	}
	for _, tx := range txs {
		pt, decoded := 5, fmt.Sprintf(`{"type":"GRP_TXT","channel":"#test","text":"message %d","sender":"node%d"}`, tx.id, tx.id%50)
		if tx.origin >= 0 {
			pt, decoded = 4, fmt.Sprintf(`{"type":"ADVERT","pubKey":"%s","name":"R%04d"}`, bn534PK(tx.origin), tx.origin)
		}
		if _, err := txStmt.Exec(tx.id, fmt.Sprintf("1500%036d", tx.id), tx.hash, tx.firstSeen.Format(time.RFC3339), pt, decoded); err != nil {
			tb.Fatal(err)
		}
		for _, o := range tx.obs {
			path := make([]string, len(o.relays))
			resolved := make([]string, len(o.relays))
			for h, k := range o.relays {
				resolved[h] = bn534PK(k)
				path[h] = resolved[h][:2]
			}
			pj, _ := json.Marshal(path)
			rj, _ := json.Marshal(resolved)
			if _, err := obsStmt.Exec(o.id, tx.id, o.observer+1, string(pj), o.ts.Unix(), string(rj)); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := dbtx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// bn534Scale reads "tx,nodes,obs" (default the large instance).
func bn534Scale(tb testing.TB, env string) (numTx, numNodes, numObs int) {
	numTx, numNodes, numObs = 150000, 2000, 2500000
	if v := os.Getenv(env); v != "" && v != "1" {
		parts := strings.Split(v, ",")
		if len(parts) != 3 {
			tb.Fatalf("%s=%q: want tx,nodes,obs", env, v)
		}
		var err error
		if numTx, err = strconv.Atoi(parts[0]); err == nil {
			if numNodes, err = strconv.Atoi(parts[1]); err == nil {
				numObs, err = strconv.Atoi(parts[2])
			}
		}
		if err != nil {
			tb.Fatalf("%s=%q: %v", env, v, err)
		}
	}
	return
}

// bn534DBPath returns a database for the scale, reusing the file named by
// CORESCOPE_534_DB when it exists (so before and after read the same rows),
// else writing it there or to a temp dir.
func bn534DBPath(tb testing.TB, numTx, numNodes, numObs int) string {
	tb.Helper()
	path := os.Getenv("CORESCOPE_534_DB")
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	} else {
		path = filepath.Join(tb.TempDir(), "bn534.db")
	}
	t0 := time.Now()
	bn534WriteDB(tb, path, bn534Data(numTx, numNodes, numObs, 534), numNodes)
	tb.Logf("wrote %s in %s", path, time.Since(t0).Round(time.Millisecond))
	return path
}

// bn534Pair is one (pubkey, transmission) membership of byNode.
type bn534Pair struct {
	pk string
	tx *StoreTx
}

// bn534Pairs returns every membership of s.byNode, in a deterministic order.
// Must hold s.mu.
func bn534Pairs(s *PacketStore) []bn534Pair {
	pks := make([]string, 0, len(s.byNode))
	for pk := range s.byNode {
		pks = append(pks, pk)
	}
	sort.Strings(pks)
	var out []bn534Pair
	for _, pk := range pks {
		for _, tx := range s.byNode[pk] {
			out = append(out, bn534Pair{pk, tx})
		}
	}
	return out
}

// bn534SetsRetained builds the de-dup sets for pairs on an empty store (the
// way the store builds them, through addToByNode), drops the byNode lists and
// returns the heap the sets keep.
func bn534SetsRetained(pairs []bn534Pair) int64 {
	_, heap := heapOf(func() *PacketStore {
		s := NewPacketStore(nil, nil)
		for _, p := range pairs {
			s.addToByNode(p.tx, p.pk)
		}
		s.byNode = nil
		return s
	})
	empty, base := heapOf(func() *PacketStore {
		s := NewPacketStore(nil, nil)
		s.byNode = nil
		return s
	})
	runtime.KeepAlive(empty)
	return heap - base
}

// TestByNodeTxIDs534_Measure loads the large-instance shape and prints what
// the byNode de-dup sets cost and what the store does with them: entries,
// retained heap of the sets alone, heap and trackedBytes after Load, Load
// time and allocations, the regional bulk-health call and one eviction pass.
// The same file runs against master for the before numbers:
//
//	CORESCOPE_534_MEASURE=1 CORESCOPE_534_DB=/tmp/bn534.db go test -run ByNodeTxIDs534_Measure -v -timeout 60m
//	CORESCOPE_534_MEASURE=30000,600,500000 ... (tx,nodes,obs)
func TestByNodeTxIDs534_Measure(t *testing.T) {
	if os.Getenv("CORESCOPE_534_MEASURE") == "" {
		t.Skip("set CORESCOPE_534_MEASURE=1 to run")
	}
	numTx, numNodes, numObs := bn534Scale(t, "CORESCOPE_534_MEASURE")
	dbPath := bn534DBPath(t, numTx, numNodes, numObs)
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.conn.Close()

	var loadDur time.Duration
	var m0, m1 runtime.MemStats
	store, heap := heapOf(func() *PacketStore {
		s := NewPacketStore(db, &PacketStoreConfig{})
		runtime.ReadMemStats(&m0)
		t0 := time.Now()
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		loadDur = time.Since(t0)
		runtime.ReadMemStats(&m1)
		s.WaitIndexesReady(10 * time.Minute)
		return s
	})
	mib := func(b int64) float64 { return float64(b) / 1048576 }

	store.mu.RLock()
	pairs := bn534Pairs(store)
	sizes := make([]int, 0, len(store.byNode))
	for _, list := range store.byNode {
		sizes = append(sizes, len(list))
	}
	small := 0
	for _, n := range sizes {
		if n <= 8 {
			small++
		}
	}
	sort.Ints(sizes)
	tracked := store.trackedBytes
	store.mu.RUnlock()

	t.Logf("data: tx=%d obs=%d nodes=%d set entries=%d (%.2f per tx); per-node set size p50=%d p90=%d p99=%d max=%d; sets with <=8 entries: %d",
		len(store.packets), store.totalObs, len(sizes), len(pairs), float64(len(pairs))/float64(len(store.packets)),
		obs411Pct(sizes, .5), obs411Pct(sizes, .9), obs411Pct(sizes, .99), sizes[len(sizes)-1], small)
	t.Logf("load: %s, %d mallocs, %.1f MiB allocated; heap after load %.1f MiB, trackedBytes %.1f MiB",
		loadDur.Round(time.Millisecond), m1.Mallocs-m0.Mallocs, mib(int64(m1.TotalAlloc-m0.TotalAlloc)), mib(heap), mib(tracked))

	var retained []int64
	for i := 0; i < 3; i++ {
		retained = append(retained, bn534SetsRetained(pairs))
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i] < retained[j] })
	r := retained[1]
	t.Logf("node sets retained (median of 3): %.2f MiB, %.1f B per entry", mib(r), float64(r)/float64(len(pairs)))

	// Regional bulk health: the region filter walks every set once.
	var health []time.Duration
	got := 0
	for i := 0; i < 15; i++ {
		t0 := time.Now()
		got = len(store.GetBulkHealth(50, "CPH", ""))
		health = append(health, time.Since(t0))
	}
	t.Logf("GetBulkHealth(50, CPH): median %s over %d runs (%d nodes)", perf202Median(health).Round(time.Microsecond), len(health), got)

	// One eviction pass over the oldest sixth (a retention cut placed relative
	// to the data, so a later run on the same DB evicts the same rows), as
	// RunEviction runs it.
	store.mu.Lock()
	cut, err := time.Parse(time.RFC3339, store.packets[len(store.packets)/6].FirstSeen)
	if err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.retentionHours = time.Since(cut).Hours()
	store.mu.Unlock()
	t0 := time.Now()
	evicted := store.RunEviction()
	evictDur := time.Since(t0)
	store.mu.RLock()
	left, stale := 0, 0
	live := make(map[*StoreTx]bool, len(store.packets))
	for _, tx := range store.packets {
		live[tx] = true
	}
	for _, list := range store.byNode {
		for _, tx := range list {
			left++
			if !live[tx] {
				stale++
			}
		}
	}
	store.mu.RUnlock()
	t.Logf("RunEviction: %d tx in %s; byNode keeps %d memberships, %d of evicted transmissions", evicted, evictDur.Round(time.Millisecond), left, stale)
	runtime.KeepAlive(store)
}

// bn534Stream is the sequence of addToByNode calls the store makes while it
// loads data: one per decoded pubkey, then one per resolved relay of every
// observation, most of them de-dup hits.
func bn534Stream(txs []bn534Tx) []bn534Pair {
	var out []bn534Pair
	for i := range txs {
		tx := &StoreTx{ID: txs[i].id, Hash: txs[i].hash}
		if txs[i].origin >= 0 {
			out = append(out, bn534Pair{bn534PK(txs[i].origin), tx})
		}
		for _, o := range txs[i].obs {
			for _, k := range o.relays {
				out = append(out, bn534Pair{bn534PK(k), tx})
			}
		}
	}
	return out
}

// BenchmarkByNodeIndex534 replays the addToByNode calls of a load (decoded
// pubkeys and every observation's resolved relays, de-dup hits included) into
// an empty store: CPU and allocations of building byNode and its sets.
// Default 30K tx / 600 nodes / 500K obs; CORESCOPE_534_BENCH=tx,nodes,obs.
//
//	go test -run '^$' -bench ByNodeIndex534 -benchmem -count 6 .
func BenchmarkByNodeIndex534(b *testing.B) {
	numTx, numNodes, numObs := 30000, 600, 500000
	if os.Getenv("CORESCOPE_534_BENCH") != "" {
		numTx, numNodes, numObs = bn534Scale(b, "CORESCOPE_534_BENCH")
	}
	stream := bn534Stream(bn534Data(numTx, numNodes, numObs, 534))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := NewPacketStore(nil, nil)
		for _, p := range stream {
			s.addToByNode(p.tx, p.pk)
		}
	}
	b.ReportMetric(float64(len(stream)), "calls/op")
}

// bn534EvictionStore builds a store from data without a DB (byNode through
// addToByNode, as Load feeds it).
func bn534EvictionStore(txs []bn534Tx) *PacketStore {
	s := NewPacketStore(nil, &PacketStoreConfig{})
	pt := 5
	for i := range txs {
		d := &txs[i]
		tx := &StoreTx{ID: d.id, Hash: d.hash, FirstSeen: d.firstSeen.Format(time.RFC3339), PayloadType: &pt}
		if d.origin >= 0 {
			tx.DecodedJSON = fmt.Sprintf(`{"type":"ADVERT","pubKey":"%s","name":"R%04d"}`, bn534PK(d.origin), d.origin)
			s.addToByNode(tx, bn534PK(d.origin))
		}
		for _, o := range d.obs {
			for _, k := range o.relays {
				s.addToByNode(tx, bn534PK(k))
			}
		}
		s.packets = append(s.packets, tx)
		s.byHash[tx.Hash], s.byTxID[tx.ID] = tx, tx
		s.byPayloadType[pt] = append(s.byPayloadType[pt], tx)
	}
	return s
}

// BenchmarkEvictNodeSets534 is one eviction pass over the oldest sixth of a
// 30K-transmission store (default; CORESCOPE_534_BENCH=tx,nodes,obs): what
// the pass costs with byNode and its sets at the large instance's shape.
//
//	go test -run '^$' -bench EvictNodeSets534 -benchmem -count 6 .
func BenchmarkEvictNodeSets534(b *testing.B) {
	numTx, numNodes, numObs := 30000, 600, 500000
	if os.Getenv("CORESCOPE_534_BENCH") != "" {
		numTx, numNodes, numObs = bn534Scale(b, "CORESCOPE_534_BENCH")
	}
	data := bn534Data(numTx, numNodes, numObs, 534)
	cut := data[len(data)/6].firstSeen
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s := bn534EvictionStore(data)
		s.retentionHours = time.Since(cut).Hours()
		runtime.GC()
		b.StartTimer()
		s.mu.Lock()
		n := s.EvictStale()
		s.mu.Unlock()
		if n == 0 {
			b.Fatal("nothing evicted")
		}
	}
}

// BenchmarkBulkHealthRegion534 is GetBulkHealth with a region on a loaded
// 30K-transmission store (default; CORESCOPE_534_BENCH=tx,nodes,obs). The
// region filter walks every de-dup set once.
//
//	go test -run '^$' -bench BulkHealthRegion534 -benchmem -count 6 .
func BenchmarkBulkHealthRegion534(b *testing.B) {
	numTx, numNodes, numObs := 30000, 600, 500000
	if os.Getenv("CORESCOPE_534_BENCH") != "" {
		numTx, numNodes, numObs = bn534Scale(b, "CORESCOPE_534_BENCH")
	}
	dbPath := filepath.Join(b.TempDir(), "bn534.db")
	bn534WriteDB(b, dbPath, bn534Data(numTx, numNodes, numObs, 534), numNodes)
	db, err := OpenDB(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer db.conn.Close()
	s := NewPacketStore(db, &PacketStoreConfig{})
	if err := s.Load(); err != nil {
		b.Fatal(err)
	}
	s.WaitIndexesReady(10 * time.Minute)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(s.GetBulkHealth(50, "CPH", "")) == 0 {
			b.Fatal("no nodes in the region")
		}
	}
}
