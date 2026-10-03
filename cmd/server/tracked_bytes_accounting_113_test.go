package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Issues #113 and #164: trackedBytes (which drives packetStore.maxMemoryMB
// eviction) must equal what the store was charged, exactly.
//
// The invariant every test below checks:
//
//	trackedBytes == Σ over live tx of ( tx.chargedBytes
//	                                   + resolvedRelayBytes(len(pathHopResolved[tx]))
//	                                   + Σ estimateStoreObsBytes(obs) )
//
// and tx.chargedBytes == estimateStoreTxBytes(tx) for every live tx, i.e. no
// charge is stale. Master charged a tx before pickBestObservation gave it a
// path and never charged resolved relay hops, then re-estimated at eviction.

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
			if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp, resolved_path)
				VALUES (?, ?, ?, ?, 'RX', -5.0, -90.0, 5, ?, ?, ?)`,
				obsID, i, fmt.Sprintf("obs%d", j), names[j], string(pj), ts.Format(time.RFC3339), string(rj)); err != nil {
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

// acct113Sum is what trackedBytes must equal: the charges of the live store.
func acct113Sum(s *PacketStore) (sum int64, stale int) {
	for _, tx := range s.packets {
		if int64(tx.chargedBytes) != estimateStoreTxBytes(tx) {
			stale++
		}
		sum += int64(tx.chargedBytes) + resolvedRelayBytes(len(s.pathHopResolved[tx]))
		for _, o := range tx.Observations {
			sum += estimateStoreObsBytes(o)
		}
	}
	return sum, stale
}

// acct113Check asserts the accounting invariant for the whole store.
func acct113Check(t *testing.T, s *PacketStore, where string) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	sum, stale := acct113Sum(s)
	if stale > 0 {
		t.Errorf("%s: %d of %d tx carry a stale charge (chargedBytes != estimateStoreTxBytes)", where, stale, len(s.packets))
	}
	if s.trackedBytes != sum {
		t.Errorf("%s: trackedBytes = %d, charges of the live store sum to %d (drift %d)", where, s.trackedBytes, sum, s.trackedBytes-sum)
	}
}

func TestTrackedBytes_LoadChargesPathAndResolvedRelays_113(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 400, 0)
	store := acct113Load(t, dbPath)
	if len(store.pathHopResolved) == 0 {
		t.Fatal("setup: no tx has resolved relay hops; the test would not cover #164")
	}
	multiHop := 0
	for _, tx := range store.packets {
		if len(txGetParsedPath(tx)) > 1 {
			multiHop++
		}
	}
	if multiHop == 0 {
		t.Fatal("setup: no tx has a multi-hop path")
	}
	acct113Check(t, store, "after Load")
}

func TestTrackedBytes_EvictionSubtractsExactlyWhatWasCharged_113(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 400, 200)
	store := acct113Load(t, dbPath)
	store.retentionHours = 24
	acct113Check(t, store, "after Load")

	if n := store.RunEviction(); n != 200 {
		t.Fatalf("evicted %d tx, want the 200 older than the retention window", n)
	}
	// 200 tx stay: the remaining total must still be exactly their charges.
	acct113Check(t, store, "after evicting the old half")
	if store.trackedBytes <= 0 {
		t.Fatal("trackedBytes hit zero with half of the store still live")
	}

	store.retentionHours = 0.0001 // everything is now older than the window
	if n := store.RunEviction(); n != 200 {
		t.Fatalf("second pass evicted %d tx, want 200", n)
	}
	if store.trackedBytes != 0 || len(store.packets) != 0 || len(store.pathHopResolved) != 0 {
		t.Fatalf("empty store: trackedBytes=%d packets=%d pathHopResolved=%d, want 0/0/0",
			store.trackedBytes, len(store.packets), len(store.pathHopResolved))
	}
}

// A tx whose estimate changed after it was charged (a path assigned later,
// or any other charged allocation) must still be removed at the amount it
// was charged: re-estimating at eviction is what made master drift.
func TestTrackedBytes_EvictionIgnoresAFreshEstimate_113(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 100, 50)
	store := acct113Load(t, dbPath)
	store.retentionHours = 24

	victim := store.packets[0] // oldest: will be evicted
	victim.PathJSON = `["aa","bb","cc","dd","ee","ff","11","22","33","44","55","66","77","88","99","00"]`
	victim.pathParsed = false
	if estimateStoreTxBytes(victim) == int64(victim.chargedBytes) {
		t.Fatal("setup: the fresh estimate did not change")
	}

	store.RunEviction()
	// The live remainder: 50 tx that were never touched.
	store.mu.RLock()
	live, _ := acct113Sum(store)
	got := store.trackedBytes
	store.mu.RUnlock()
	if got != live {
		t.Fatalf("after eviction trackedBytes=%d, live charges=%d: eviction subtracted a fresh estimate, not the charge", got, live)
	}
}

func TestTrackedBytes_RechargeCoversALaterPath_113(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 40, 0)
	store := acct113Load(t, dbPath)

	tx := store.packets[0]
	long := &StoreObs{ID: 9_000_001, TransmissionID: tx.ID, ObserverID: "obs-late", ObserverName: "Late Observer",
		PathJSON: `["01","02","03","04","05","06","07","08"]`, Timestamp: tx.FirstSeen}
	store.mu.Lock()
	tx.Observations = append(tx.Observations, long)
	tx.ObservationCount++
	store.trackedBytes += estimateStoreObsBytes(long)
	pickBestObservation(tx)
	d := rechargeTx(tx)
	store.trackedBytes += d
	store.mu.Unlock()

	if d <= 0 {
		t.Fatalf("an 8-hop path replaced a shorter one but the recharge delta is %d", d)
	}
	acct113Check(t, store, "after the longer path was assigned")
	store.mu.Lock()
	if d2 := rechargeTx(tx); d2 != 0 {
		t.Errorf("a second recharge with nothing changed returned %d, want 0", d2)
	}
	store.mu.Unlock()
}

func TestTrackedBytes_ResolvedRelaysAreChargedIncrementally_164(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 60, 0)
	store := acct113Load(t, dbPath)

	// Start from a tx with no record of resolved relays.
	tx := store.packets[0]
	store.mu.Lock()
	store.trackedBytes -= resolvedRelayBytes(len(store.pathHopResolved[tx]))
	delete(store.pathHopResolved, tx)
	store.mu.Unlock()

	hops := map[string]bool{}
	step := func(keys ...string) int64 {
		store.mu.Lock()
		defer store.mu.Unlock()
		before := store.trackedBytes
		store.addResolvedPubkeysToPathHopIndex(tx, keys, hops)
		return store.trackedBytes - before
	}
	k1, k2, k3 := acct113PK(201), acct113PK(202), acct113PK(203)

	d1 := step(k1, k2)
	if d1 <= 0 {
		t.Fatalf("two new resolved relays added %d bytes to trackedBytes", d1)
	}
	if want := resolvedRelayBytes(2); d1 != want {
		t.Errorf("first two relays charged %d, want resolvedRelayBytes(2) = %d (slot + hash per key, entry overhead once)", d1, want)
	}
	if d := step(k1, k2); d != 0 {
		t.Errorf("the same relays seen again (a later observation) charged %d, want 0", d)
	}
	d3 := step(k3)
	if want := resolvedRelayBytes(3) - resolvedRelayBytes(2); d3 != want || d3 < 16 {
		t.Errorf("a third relay charged %d, want %d (at least a byPathHop slot and a hash: 16)", d3, want)
	}
	acct113Check(t, store, "after adding resolved relays")

	// A rebuild keeps the records, so it must not move the total.
	store.mu.Lock()
	before := store.trackedBytes
	store.buildPathHopIndex()
	after := store.trackedBytes
	store.mu.Unlock()
	if after != before {
		t.Errorf("buildPathHopIndex moved trackedBytes %d -> %d although every record was carried over", before, after)
	}
	acct113Check(t, store, "after a rebuild that retains the records")

	// A rebuild with no previous resolved entries clears the records: their
	// charge must go with them.
	store.mu.Lock()
	store.byPathHop = map[string][]*StoreTx{}
	store.buildPathHopIndex()
	store.mu.Unlock()
	if len(store.pathHopResolved) != 0 {
		t.Fatalf("setup: the rebuild kept %d records", len(store.pathHopResolved))
	}
	acct113Check(t, store, "after a rebuild that clears the records")
}

// Insert, extra observations, path changes, resolved relays and eviction in a
// deterministic mix: the invariant holds after every step, and an emptied
// store is exactly zero.
func TestTrackedBytes_MixedSequenceDoesNotDrift_113(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 200, 0)
	store := acct113Load(t, dbPath)
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	rng := rand.New(rand.NewSource(113))
	maxTx, maxObs := 200, 0
	if err := conn.QueryRow(`SELECT COALESCE(MAX(id),0) FROM observations`).Scan(&maxObs); err != nil {
		t.Fatal(err)
	}
	hops := map[string]bool{}
	now := time.Now().UTC()
	for step := 0; step < 60; step++ {
		switch rng.Intn(4) {
		case 0: // new transmissions
			firstNew, firstNewObs := maxTx, maxObs
			for k := 0; k < 3; k++ {
				maxTx++
				ts := now.Add(time.Duration(step*10+k) * time.Second).Format(time.RFC3339)
				if _, err := conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
					VALUES (?, 'aabb', ?, ?, 1, 5, 1, '{"type":"GRP_TXT","text":"live"}')`, maxTx, fmt.Sprintf("live%06d", maxTx), ts); err != nil {
					t.Fatal(err)
				}
				maxObs++
				if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, path_json, timestamp)
					VALUES (?, ?, 'obs0', 'Alpha', 'RX', ?, ?)`, maxObs, maxTx, `["aa","bb"]`, ts); err != nil {
					t.Fatal(err)
				}
			}
			_ = firstNewObs
			store.IngestNewFromDB(firstNew, 100)
		case 1: // a longer path heard on an existing transmission
			id := 1 + rng.Intn(200)
			maxObs++
			p := make([]string, 2+rng.Intn(6))
			for i := range p {
				p[i] = acct113Hop(id, i, step)
			}
			pj, _ := json.Marshal(p)
			if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, path_json, timestamp)
				VALUES (?, ?, 'obs3', 'Delta', 'RX', ?, ?)`, maxObs, id, string(pj), now.Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			store.IngestNewObservations(maxObs-1, 10)
		case 2: // relays resolved after insertion
			store.mu.Lock()
			if len(store.packets) > 0 {
				tx := store.packets[rng.Intn(len(store.packets))]
				store.addResolvedPubkeysToPathHopIndex(tx, []string{acct113PK(rng.Intn(256)), acct113PK(rng.Intn(256))}, hops)
			}
			store.mu.Unlock()
		case 3: // age the head of the store out
			store.mu.Lock()
			for i := 0; i < len(store.packets) && i < 5; i++ {
				store.packets[i].FirstSeen = now.Add(-100 * time.Hour).Format(time.RFC3339)
			}
			store.retentionHours = 24
			store.mu.Unlock()
			store.RunEviction()
		}
		acct113Check(t, store, fmt.Sprintf("after step %d", step))
		if t.Failed() {
			return
		}
	}
	store.mu.Lock()
	store.retentionHours = 0.0001
	store.mu.Unlock()
	store.RunEviction()
	if store.trackedBytes != 0 || len(store.packets) != 0 {
		t.Fatalf("emptied store: trackedBytes=%d packets=%d, want 0/0", store.trackedBytes, len(store.packets))
	}
}

// A memory-triggered pass must start above the high watermark and finish at
// or below the low one, without taking much more than needed.
func TestMemoryEviction_CrossesHighAndReachesLow_113(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 3000, 0)
	store := acct113Load(t, dbPath)
	acct113Check(t, store, "after Load")

	tracked := store.trackedBytes
	store.maxMemoryMB = int(float64(tracked) * 0.9 / 1048576)
	high := int64(store.maxMemoryMB) * 1048576
	low := int64(float64(high) * 0.85)
	if tracked <= high {
		t.Fatalf("setup: trackedBytes %d does not exceed the high watermark %d", tracked, high)
	}
	before := len(store.packets)
	store.RunEviction()
	if store.trackedBytes > low {
		t.Errorf("after the pass trackedBytes=%d is above the low watermark %d", store.trackedBytes, low)
	}
	if slack := int64(float64(tracked)/float64(before)) * 8; store.trackedBytes < low-slack {
		t.Errorf("trackedBytes=%d is far below the low watermark %d: the pass evicted too much", store.trackedBytes, low)
	}
	if len(store.packets) >= before {
		t.Fatal("nothing was evicted")
	}
	acct113Check(t, store, "after the memory pass")
}

// The cold-load budget is the estimate of a typical tx; it must stay in line
// with what the store really charges for one.
func TestEstimateStoreTxBytesTypical_TracksTheRealCharge_113(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 400, 0)
	store := acct113Load(t, dbPath)
	var total int64
	obs := 0
	for _, tx := range store.packets {
		total += int64(tx.chargedBytes) + resolvedRelayBytes(len(store.pathHopResolved[tx]))
		for _, o := range tx.Observations {
			total += estimateStoreObsBytes(o)
			obs++
		}
	}
	perTx := float64(total) / float64(len(store.packets))
	avgObs := int(float64(obs)/float64(len(store.packets)) + 0.5)
	typical := float64(estimateStoreTxBytesTypical(avgObs))
	if typical < perTx*0.8 || typical > perTx*1.25 {
		t.Errorf("estimateStoreTxBytesTypical(%d) = %.0f, the store charges %.0f per tx: more than 20-25%% apart", avgObs, typical, perTx)
	}
}

// trackedBytes against the real heap after a GC, on a store loaded through
// Load(). The store's own maps and slices are what trackedBytes is meant to
// bound; the ratio must be close to 1 (master: well under half).
func TestTrackedBytesTracksTheHeap_113(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a 20k-transmission store")
	}
	dbPath := filepath.Join(t.TempDir(), "acct.db")
	acct113CreateDB(t, dbPath, 20000, 0)
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store := NewPacketStore(db, &PacketStoreConfig{})
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m1)
	heap := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	ratio := float64(store.trackedBytes) / float64(heap)
	t.Logf("trackedBytes = %.1f MB, heap growth = %.1f MB, ratio = %.2f", float64(store.trackedBytes)/1048576, float64(heap)/1048576, ratio)
	if ratio < 0.8 || ratio > 1.3 {
		t.Errorf("trackedBytes is %.2f of the real heap growth, want 0.8-1.3", ratio)
	}
	runtime.KeepAlive(store)
}
