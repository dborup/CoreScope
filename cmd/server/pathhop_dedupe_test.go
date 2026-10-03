package main

import (
	"fmt"
	"io"
	"log"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// traffic_share_score grew with server uptime. The score is
// |non-advert txs in byPathHop[pubkey]| / |non-advert txs in byPayloadType|,
// but every observation of a transmission that resolved to the same relays
// appended the transmission to the relay's byPathHop bucket again, while the
// denominator counts it once. A rebuild of the index (retainResolvedPathHops
// dedups by *StoreTx) made values look sane right after a restart; they then
// drifted upwards with live observations.

// Four relays with distinct first bytes, so a raw 1-byte hop resolves to
// exactly one of them (unique prefix) on every ingest path.
var phdRelays = []string{
	"a1" + strings.Repeat("11", 31),
	"b2" + strings.Repeat("22", 31),
	"c3" + strings.Repeat("33", 31),
	"d4" + strings.Repeat("44", 31),
}

// phdPaths is the fixed transmission set: indexes into phdRelays. The
// longest path has 3 hops, so the sum of all relays' share is at most 3.
var phdPaths = [][]int{
	{0, 1, 2}, {1, 2}, {3}, {0, 3}, {2},
	{0, 1}, {1}, {2, 3}, {0}, {1, 3},
	{0, 1, 2}, {3}, {2, 1}, {0, 2}, {1},
	{3, 2, 0}, {2}, {0, 1}, {1, 2, 3}, {3},
}

const (
	phdMaxPathLen    = 3
	phdLateObs       = 10 // extra observations per transmission (acceptance N)
	phdObserverCount = 1 + phdLateObs
)

func phdRawHopsJSON(path []int) string {
	hops := make([]string, len(path))
	for i, r := range path {
		hops[i] = `"` + strings.ToUpper(phdRelays[r][:2]) + `"`
	}
	return "[" + strings.Join(hops, ",") + "]"
}

func phdResolvedJSON(path []int) string {
	pks := make([]string, len(path))
	for i, r := range path {
		pks[i] = `"` + phdRelays[r] + `"`
	}
	return "[" + strings.Join(pks, ",") + "]"
}

// phdSeed creates the relays (as repeater nodes) and the observers.
func phdSeed(t testing.TB, db *DB, observers int) {
	t.Helper()
	ts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for i, pk := range phdRelays {
		phdExec(t, db, `INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', ?, '2026-01-01', 1)`,
			pk, fmt.Sprintf("phdRelay-%d", i), ts)
	}
	for i := 1; i <= observers; i++ {
		phdExec(t, db, `INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, 'TST')`,
			i, fmt.Sprintf("phdObs-%02d", i), fmt.Sprintf("Observer %d", i))
	}
}

func phdExec(t testing.TB, db *DB, q string, args ...any) {
	t.Helper()
	if _, err := db.conn.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// phdInsertTx inserts transmission i of phdPaths (non-advert TXT_MSG, flood).
func phdInsertTx(t testing.TB, db *DB, i int, firstSeen time.Time) int {
	t.Helper()
	id := i + 1
	phdExec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES (?, 'CAFE', ?, ?, 1, 2, '{"type":"TXT_MSG"}')`, id, fmt.Sprintf("phdTx-%03d", i), firstSeen.Format(time.RFC3339))
	return id
}

// phdInsertObs inserts one observation of transmission i through its path,
// heard by observer obsIdx. persisted controls resolved_path (the ingestor
// path) vs NULL (resolved fresh by the server on ingest).
func phdInsertObs(t testing.TB, db *DB, i, obsIdx int, ts time.Time, persisted bool) {
	t.Helper()
	var rp any
	if persisted {
		rp = phdResolvedJSON(phdPaths[i])
	}
	phdExec(t, db, `INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path)
		VALUES (?, ?, 5, -90, ?, ?, ?)`, i+1, obsIdx, phdRawHopsJSON(phdPaths[i]), ts.Unix(), rp)
}

func phdMaxObsID(t testing.TB, db *DB) int {
	t.Helper()
	var id int
	if err := db.conn.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM observations`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func phdLoadedStore(t testing.TB, db *DB) *PacketStore {
	t.Helper()
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes not ready")
	}
	return store
}

func phdCountTxIn(store *PacketStore, key string, tx *StoreTx) int {
	store.mu.RLock()
	defer store.mu.RUnlock()
	n := 0
	for _, t := range store.byPathHop[key] {
		if t == tx {
			n++
		}
	}
	return n
}

func phdAssertOncePerRelay(t *testing.T, store *PacketStore, i int) {
	t.Helper()
	tx := store.byTxID[i+1]
	if tx == nil {
		t.Fatalf("tx %d not in store", i+1)
	}
	if got := len(tx.Observations); got != phdObserverCount {
		t.Fatalf("fixture: tx %d has %d observations, want %d", tx.ID, got, phdObserverCount)
	}
	for _, r := range phdPaths[i] {
		if n := phdCountTxIn(store, phdRelays[r], tx); n != 1 {
			t.Errorf("tx %d is in byPathHop[%s…] %d times, want exactly 1", tx.ID, phdRelays[r][:8], n)
		}
	}
}

// Late-observation path: the transmission is ingested with its first
// observation, then 10 more observers hear it through the same relays.
func TestPathHopIndexOncePerTx_LateObservations_PathHopDedupe(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	phdSeed(t, db, phdObserverCount)
	store := phdLoadedStore(t, db)

	now := time.Now().UTC().Add(-10 * time.Minute)
	phdInsertTx(t, db, 0, now)
	phdInsertObs(t, db, 0, 1, now, false)
	store.IngestNewFromDB(0, 100)
	for _, r := range phdPaths[0] {
		if n := phdCountTxIn(store, phdRelays[r], store.byTxID[1]); n != 1 {
			t.Fatalf("fixture: first observation must index tx once under %s…, got %d", phdRelays[r][:8], n)
		}
	}

	for k := 1; k <= phdLateObs; k++ {
		since := phdMaxObsID(t, db)
		phdInsertObs(t, db, 0, 1+k, now.Add(time.Duration(k)*time.Second), false)
		store.IngestNewObservations(since, 100)
	}
	phdAssertOncePerRelay(t, store, 0)
}

// Live-ingest path: the transmission arrives together with all of its
// observations in one IngestNewFromDB batch.
func TestPathHopIndexOncePerTx_LiveIngestBatch_PathHopDedupe(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	phdSeed(t, db, phdObserverCount)
	store := phdLoadedStore(t, db)

	now := time.Now().UTC().Add(-10 * time.Minute)
	phdInsertTx(t, db, 0, now)
	for o := 1; o <= phdObserverCount; o++ {
		phdInsertObs(t, db, 0, o, now.Add(time.Duration(o)*time.Second), false)
	}
	store.IngestNewFromDB(0, 100)
	phdAssertOncePerRelay(t, store, 0)
}

// phdShares returns every relay's traffic share as seen by the three score
// functions, failing if they disagree.
func phdShares(t *testing.T, store *PacketStore) map[string]float64 {
	t.Helper()
	bulk := store.computeRepeaterUsefulnessScoreMap()
	batch := store.GetRepeaterNodeStatsBatch(phdRelays, 24)
	out := make(map[string]float64, len(phdRelays))
	for _, pk := range phdRelays {
		single := store.GetRepeaterUsefulnessScore(pk)
		if bulk[pk] != single || batch[pk].Score != single {
			t.Fatalf("score functions disagree for %s…: bulk=%v single=%v batch=%v", pk[:8], bulk[pk], single, batch[pk].Score)
		}
		out[pk] = single
	}
	return out
}

// phdExpectedShares is the definition applied to the fixture: the fraction
// of the (all non-advert) transmissions whose path contains the relay.
func phdExpectedShares() map[string]float64 {
	out := make(map[string]float64, len(phdRelays))
	for _, path := range phdPaths {
		for _, r := range path {
			out[phdRelays[r]] += 1 / float64(len(phdPaths))
		}
	}
	return out
}

func phdAssertShares(t *testing.T, label string, got map[string]float64) {
	t.Helper()
	want := phdExpectedShares()
	sum := 0.0
	for _, pk := range phdRelays {
		sum += got[pk]
		if math.Abs(got[pk]-want[pk]) > 1e-9 {
			t.Errorf("%s: traffic share of %s… = %.4f, want %.4f", label, pk[:8], got[pk], want[pk])
		}
		if got[pk] >= 1 {
			t.Errorf("%s: traffic share of %s… reached the 1.0 clamp", label, pk[:8])
		}
	}
	if sum > phdMaxPathLen+1e-9 {
		t.Errorf("%s: sum of all relays' traffic share = %.3f, exceeds the maximum path length %d", label, sum, phdMaxPathLen)
	}
}

// The traffic share of a fixed set of transmissions is the same right after
// a load and after any number of further observations of them.
func TestTrafficShareStableAcrossLateObservations_PathHopDedupe(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)

	// Restart: every observation is already persisted (resolved_path set by
	// the ingestor) and comes in through Load.
	loadDB := setupTestDB(t)
	defer loadDB.conn.Close()
	phdSeed(t, loadDB, phdObserverCount)
	for i := range phdPaths {
		phdInsertTx(t, loadDB, i, base.Add(time.Duration(i)*time.Second))
		for o := 1; o <= phdObserverCount; o++ {
			phdInsertObs(t, loadDB, i, o, base.Add(time.Duration(i*100+o)*time.Second), true)
		}
	}
	loaded := phdLoadedStore(t, loadDB)
	afterLoad := phdShares(t, loaded)
	phdAssertShares(t, "after load", afterLoad)

	// Live: the same transmissions arrive with one observation each, then
	// the late observations arrive in rounds.
	liveDB := setupTestDB(t)
	defer liveDB.conn.Close()
	phdSeed(t, liveDB, phdObserverCount)
	live := phdLoadedStore(t, liveDB)
	for i := range phdPaths {
		phdInsertTx(t, liveDB, i, base.Add(time.Duration(i)*time.Second))
		phdInsertObs(t, liveDB, i, 1, base.Add(time.Duration(i*100+1)*time.Second), false)
	}
	live.IngestNewFromDB(0, 1000)
	afterIngest := phdShares(t, live)
	phdAssertShares(t, "after live ingest", afterIngest)

	for round := 0; round < 2; round++ {
		since := phdMaxObsID(t, liveDB)
		for i := range phdPaths {
			for k := 0; k < phdLateObs/2; k++ {
				o := 2 + round*(phdLateObs/2) + k
				phdInsertObs(t, liveDB, i, o, base.Add(time.Duration(i*100+o)*time.Second), false)
			}
		}
		live.IngestNewObservations(since, 10000)
		got := phdShares(t, live)
		label := fmt.Sprintf("after late-observation round %d", round+1)
		phdAssertShares(t, label, got)
		if !reflect.DeepEqual(got, afterIngest) {
			t.Errorf("%s: shares changed without new transmissions:\n got %v\nwant %v", label, got, afterIngest)
		}
	}
	for i := range phdPaths {
		phdAssertOncePerRelay(t, live, i)
	}
	if !reflect.DeepEqual(phdShares(t, live), afterLoad) {
		t.Errorf("live shares differ from a fresh load of the same data:\nlive %v\nload %v", phdShares(t, live), afterLoad)
	}
}

// Index size is a function of the transmissions, not of how many times they
// were heard: further observations through the same relays add nothing.
func TestPathHopIndexSizeBoundedByTransmissions_PathHopDedupe(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	phdSeed(t, db, phdObserverCount)
	store := phdLoadedStore(t, db)
	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := range phdPaths {
		phdInsertTx(t, db, i, base.Add(time.Duration(i)*time.Second))
		phdInsertObs(t, db, i, 1, base.Add(time.Duration(i*100+1)*time.Second), false)
	}
	store.IngestNewFromDB(0, 1000)
	entries := func() int {
		store.mu.RLock()
		defer store.mu.RUnlock()
		n := 0
		for _, list := range store.byPathHop {
			n += len(list)
		}
		return n
	}
	want := entries()
	for o := 2; o <= phdObserverCount; o++ {
		since := phdMaxObsID(t, db)
		for i := range phdPaths {
			phdInsertObs(t, db, i, o, base.Add(time.Duration(i*100+o)*time.Second), false)
		}
		store.IngestNewObservations(since, 10000)
		if got := entries(); got != want {
			t.Fatalf("after observation %d of each tx: byPathHop holds %d entries, want %d (one per relay key per tx)", o, got, want)
		}
	}
}

// Defence in depth: whatever the index holds, the score counts distinct
// transmissions, not bucket entries.
func TestTrafficShareCountsDistinctTransmissions_PathHopDedupe(t *testing.T) {
	store := makeTestStore(0, time.Now().UTC(), 0)
	store.byPathHop = make(map[string][]*StoreTx)
	pt, advert := 2, payloadTypeAdvert
	relay := phdRelays[0]
	var txs []*StoreTx
	for i := 0; i < 10; i++ {
		tx := &StoreTx{ID: i + 1, Hash: fmt.Sprintf("phd-%d", i), PayloadType: &pt}
		txs = append(txs, tx)
		store.packets = append(store.packets, tx)
		store.byTxID[tx.ID] = tx
		store.byPayloadType[pt] = append(store.byPayloadType[pt], tx)
	}
	ad := &StoreTx{ID: 99, Hash: "phd-advert", PayloadType: &advert}
	store.byPayloadType[advert] = append(store.byPayloadType[advert], ad)
	// Two of the ten transmissions, each present 6 times (the index shape the
	// bug produced after 6 observations), plus a duplicated advert.
	for k := 0; k < 6; k++ {
		store.byPathHop[relay] = append(store.byPathHop[relay], txs[0], txs[1], ad)
	}

	const want = 2.0 / 10
	if got := store.GetRepeaterUsefulnessScore(relay); got != want {
		t.Errorf("GetRepeaterUsefulnessScore = %v, want %v", got, want)
	}
	if got := store.computeRepeaterUsefulnessScoreMap()[relay]; got != want {
		t.Errorf("computeRepeaterUsefulnessScoreMap = %v, want %v", got, want)
	}
	if got := store.GetRepeaterNodeStatsBatch([]string{relay}, 24)[relay].Score; got != want {
		t.Errorf("GetRepeaterNodeStatsBatch score = %v, want %v", got, want)
	}
}

// Restart: Load indexes every persisted observation and rebuilds the index;
// a live observation after that must not add the transmission again.
func TestPathHopIndexOncePerTx_AfterLoad_PathHopDedupe(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	phdSeed(t, db, phdObserverCount)
	base := time.Now().UTC().Add(-30 * time.Minute)
	phdInsertTx(t, db, 0, base)
	for o := 1; o < phdObserverCount; o++ {
		phdInsertObs(t, db, 0, o, base.Add(time.Duration(o)*time.Second), true)
	}
	store := phdLoadedStore(t, db)
	since := phdMaxObsID(t, db)
	phdInsertObs(t, db, 0, phdObserverCount, base.Add(time.Minute), false)
	store.IngestNewObservations(since, 100)
	phdAssertOncePerRelay(t, store, 0)
}

// phdDuplicateFullKeyEntries reproduces the index shape the bug produced: every
// transmission in a full-pubkey bucket is present `extra` more times.
func phdDuplicateFullKeyEntries(store *PacketStore, extra int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for key, list := range store.byPathHop {
		if len(key) != 64 {
			continue
		}
		orig := append([]*StoreTx(nil), list...)
		for k := 0; k < extra; k++ {
			store.byPathHop[key] = append(store.byPathHop[key], orig...)
		}
	}
	store.invalidateRelayStatsCache()
}

// The other byPathHop consumers already deduplicate by transmission (or only
// read raw prefix keys and take a maximum): duplicate entries do not change
// their results. This pins that, so a refactor cannot reintroduce the inflation there.
func TestPathHopConsumersIgnoreDuplicateEntries_PathHopDedupe(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)
	db := setupTestDB(t)
	defer db.conn.Close()
	phdSeed(t, db, phdObserverCount)
	for i := range phdPaths {
		phdInsertTx(t, db, i, base.Add(time.Duration(i)*time.Second))
		for o := 1; o <= 2; o++ {
			phdInsertObs(t, db, i, o, base.Add(time.Duration(i*100+o)*time.Second), true)
		}
	}
	store := phdLoadedStore(t, db)

	type snapshot struct {
		info  map[string]RepeaterRelayInfo
		bulk  map[string]RepeaterRelayInfo
		multi []MultiByteCapEntry
	}
	take := func() snapshot {
		s := snapshot{info: map[string]RepeaterRelayInfo{}}
		batch := store.GetRepeaterNodeStatsBatch(phdRelays, 24)
		for _, pk := range phdRelays {
			s.info[pk] = batch[pk].Info
		}
		s.bulk = store.computeRepeaterRelayInfoMap(24)
		s.multi = store.computeMultiByteCapability(nil)
		return s
	}
	before := take()
	if before.info[phdRelays[0]].RelayCount24h == 0 || before.bulk[phdRelays[0]].RelayCount24h == 0 {
		t.Fatalf("fixture: relay 0 must have relay counts: %+v / %+v", before.info[phdRelays[0]], before.bulk[phdRelays[0]])
	}
	phdDuplicateFullKeyEntries(store, phdLateObs)
	after := take()
	for _, pk := range phdRelays {
		if !reflect.DeepEqual(before.info[pk], after.info[pk]) {
			t.Errorf("relay info of %s… changed with duplicate entries:\nbefore %+v\n after %+v", pk[:8], before.info[pk], after.info[pk])
		}
		if !reflect.DeepEqual(before.bulk[pk], after.bulk[pk]) {
			t.Errorf("bulk relay info of %s… changed with duplicate entries:\nbefore %+v\n after %+v", pk[:8], before.bulk[pk], after.bulk[pk])
		}
	}
	if !reflect.DeepEqual(before.multi, after.multi) {
		t.Errorf("computeMultiByteCapability changed with duplicate entries:\nbefore %+v\n after %+v", before.multi, after.multi)
	}
}

// computeMultiByteCapability reads raw prefix buckets only and keeps a
// maximum hash size per node, so duplicates there cannot inflate it either.
func TestMultiByteCapabilityIgnoresDuplicateEntries_PathHopDedupe(t *testing.T) {
	db := setupCapabilityTestDB(t)
	defer db.conn.Close()
	db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen) VALUES (?, ?, ?, ?)",
		"aabbccdd11223344", "RepB", "repeater", recentTS(48))
	store := NewPacketStore(db, nil)
	pt := 1
	pkt := &StoreTx{RawHex: "01" + buildPathByte(2, 1) + "aabb", PayloadType: &pt, PathJSON: `["aabb"]`, FirstSeen: recentTS(48)}
	addTestPacket(store, pkt)
	before := store.computeMultiByteCapability(nil)
	store.mu.Lock()
	for k := 0; k < phdLateObs; k++ {
		store.byPathHop["aabb"] = append(store.byPathHop["aabb"], pkt)
	}
	store.mu.Unlock()
	after := store.computeMultiByteCapability(nil)
	if len(before) != 1 || !reflect.DeepEqual(before, after) {
		t.Fatalf("duplicates changed multi-byte capability:\nbefore %+v\n after %+v", before, after)
	}
}

// phdLateObsBenchStore: n transmissions one second apart, each with 3 raw
// one-byte hops of which 2 resolve to one of 4000 relays (the shape of a
// relay-heavy store), indexed once as by their first observation.
func phdLateObsBenchStore(n int) (*PacketStore, [][]string) {
	start := time.Now().UTC().Add(-time.Duration(n+10) * time.Second)
	store := makeTestStore(0, start, 0)
	store.byPathHop = make(map[string][]*StoreTx)
	hopsSeen := map[string]bool{}
	pks := make([][]string, n)
	for i := 0; i < n; i++ {
		r1, r2 := i%2000, 2000+(5000+i*31)%2000
		h1, h2, h3 := r1%256, r2%256, (i*13+5)%256
		pt := 2
		tx := &StoreTx{
			ID:          i + 1,
			Hash:        fmt.Sprintf("late%07d", i),
			FirstSeen:   start.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			PathJSON:    fmt.Sprintf(`["%02x","%02x","%02x"]`, h1, h2, h3),
			PayloadType: &pt,
		}
		store.packets = append(store.packets, tx)
		store.byHash[tx.Hash] = tx
		store.byTxID[tx.ID] = tx
		store.byPayloadType[pt] = append(store.byPayloadType[pt], tx)
		addTxToPathHopIndex(store.byPathHop, tx)
		pks[i] = []string{fmt.Sprintf("%02x%062x", h1, r1), fmt.Sprintf("%02x%062x", h2, r2)}
		store.indexResolvedPathHops(tx, pks[i], hopsSeen)
	}
	return store, pks
}

// BenchmarkLateObservationIndex_PathHopDedupe measures the store side of
// IngestNewObservations for one late observation (indexResolvedPathHops
// under the write lock, as the ingest loop calls it) on stores of realistic
// size, and reports how many byPathHop entries a transmission ends up with.
// Without the dedupe that number grows with every observation; with it, it
// stays at 3 raw + 2 resolved.
func BenchmarkLateObservationIndex_PathHopDedupe(b *testing.B) {
	for _, n := range []int{20000, 100000} {
		b.Run(fmt.Sprintf("txs=%d", n), func(b *testing.B) {
			store, pks := phdLateObsBenchStore(n)
			hopsSeen := map[string]bool{}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				j := (i * 7919) % n // spread late observations over the store
				tx := store.packets[j]
				store.mu.Lock()
				store.indexResolvedPathHops(tx, pks[j], hopsSeen)
				store.mu.Unlock()
			}
			b.StopTimer()
			entries := 0
			for _, list := range store.byPathHop {
				entries += len(list)
			}
			b.ReportMetric(float64(entries)/float64(n), "pathhop-entries/tx")
		})
	}
}

// BenchmarkIngestNewObservations_PathHopDedupe drives the real late-observation ingest
// (SQL scan + index) for a batch of one new observation per transmission.
func BenchmarkIngestNewObservations_PathHopDedupe(b *testing.B) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)
	db := setupTestDB(b)
	defer db.conn.Close()
	const observers = 512
	phdSeed(b, db, observers)
	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := range phdPaths {
		phdInsertTx(b, db, i, base.Add(time.Duration(i)*time.Second))
		phdInsertObs(b, db, i, 1, base.Add(time.Duration(i*1000+1)*time.Second), false)
	}
	store := phdLoadedStore(b, db)
	store.IngestNewFromDB(0, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		b.StopTimer()
		o := 2 + it%(observers-1)
		since := phdMaxObsID(b, db)
		for i := range phdPaths {
			phdInsertObs(b, db, i, o, base.Add(time.Duration(i*1000+o)*time.Second), false)
		}
		b.StartTimer()
		store.IngestNewObservations(since, 10000)
	}
}

// BenchmarkTrafficShareScoreMap_PathHopDedupe measures the bulk traffic-share pass
// (cache-miss path of GetRepeaterUsefulnessScoreMap) on realistic sizes.
// order=reversed reverses every bucket, as when background chunks load
// older transmissions after newer ones: no bucket is in ascending ID order.
func BenchmarkTrafficShareScoreMap_PathHopDedupe(b *testing.B) {
	for _, c := range []struct {
		n        int
		reversed bool
	}{{20000, false}, {100000, false}, {20000, true}, {100000, true}} {
		order := "ingest"
		if c.reversed {
			order = "reversed"
		}
		b.Run(fmt.Sprintf("txs=%d/order=%s", c.n, order), func(b *testing.B) {
			store, _ := phdLateObsBenchStore(c.n)
			if c.reversed {
				for _, list := range store.byPathHop {
					slices.Reverse(list)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.computeRepeaterUsefulnessScoreMap()
			}
		})
	}
}
