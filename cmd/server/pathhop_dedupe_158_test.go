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

// Issue #158: traffic_share_score grew with server uptime. The score is
// |non-advert txs in byPathHop[pubkey]| / |non-advert txs in byPayloadType|,
// but every observation of a transmission that resolved to the same relays
// appended the transmission to the relay's byPathHop bucket again, while the
// denominator counts it once. Load and the background chunk load rebuild the
// index (retainResolvedPathHops dedups by *StoreTx), so values looked sane
// right after a restart and then drifted upwards with live observations.

// Four relays with distinct first bytes, so a raw 1-byte hop resolves to
// exactly one of them (unique prefix) on every ingest path.
var relays158 = []string{
	"a1" + strings.Repeat("11", 31),
	"b2" + strings.Repeat("22", 31),
	"c3" + strings.Repeat("33", 31),
	"d4" + strings.Repeat("44", 31),
}

// paths158 is the fixed transmission set: indexes into relays158. The
// longest path has 3 hops, so the sum of all relays' share is at most 3.
var paths158 = [][]int{
	{0, 1, 2}, {1, 2}, {3}, {0, 3}, {2},
	{0, 1}, {1}, {2, 3}, {0}, {1, 3},
	{0, 1, 2}, {3}, {2, 1}, {0, 2}, {1},
	{3, 2, 0}, {2}, {0, 1}, {1, 2, 3}, {3},
}

const (
	maxPathLen158    = 3
	lateObs158       = 10 // extra observations per transmission (acceptance N)
	observerCount158 = 1 + lateObs158
)

func rawHopsJSON158(path []int) string {
	hops := make([]string, len(path))
	for i, r := range path {
		hops[i] = `"` + strings.ToUpper(relays158[r][:2]) + `"`
	}
	return "[" + strings.Join(hops, ",") + "]"
}

func resolvedJSON158(path []int) string {
	pks := make([]string, len(path))
	for i, r := range path {
		pks[i] = `"` + relays158[r] + `"`
	}
	return "[" + strings.Join(pks, ",") + "]"
}

// seed158 creates the relays (as repeater nodes) and the observers.
func seed158(t testing.TB, db *DB, observers int) {
	t.Helper()
	ts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for i, pk := range relays158 {
		exec158(t, db, `INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', ?, '2026-01-01', 1)`,
			pk, fmt.Sprintf("Relay158-%d", i), ts)
	}
	for i := 1; i <= observers; i++ {
		exec158(t, db, `INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, 'TST')`,
			i, fmt.Sprintf("obs158-%02d", i), fmt.Sprintf("Observer %d", i))
	}
}

func exec158(t testing.TB, db *DB, q string, args ...any) {
	t.Helper()
	if _, err := db.conn.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// insertTx158 inserts transmission i of paths158 (non-advert TXT_MSG, flood).
func insertTx158(t testing.TB, db *DB, i int, firstSeen time.Time) int {
	t.Helper()
	id := i + 1
	exec158(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES (?, 'CAFE', ?, ?, 1, 2, '{"type":"TXT_MSG"}')`, id, fmt.Sprintf("tx158-%03d", i), firstSeen.Format(time.RFC3339))
	return id
}

// insertObs158 inserts one observation of transmission i through its path,
// heard by observer obsIdx, the way the ingestor writes it: with its
// resolved_path in the same row (#1547). Load and live ingest both index
// that resolved_path (indexObservationRelayHops).
func insertObs158(t testing.TB, db *DB, i, obsIdx int, ts time.Time) {
	t.Helper()
	exec158(t, db, `INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path)
		VALUES (?, ?, 5, -90, ?, ?, ?)`, i+1, obsIdx, rawHopsJSON158(paths158[i]), ts.Unix(), resolvedJSON158(paths158[i]))
}

func maxObsID158(t testing.TB, db *DB) int {
	t.Helper()
	var id int
	if err := db.conn.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM observations`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func loadedStore158(t testing.TB, db *DB) *PacketStore {
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

func countTxIn158(store *PacketStore, key string, tx *StoreTx) int {
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

func assertOncePerRelay158(t *testing.T, store *PacketStore, i int) {
	t.Helper()
	tx := store.byTxID[i+1]
	if tx == nil {
		t.Fatalf("tx %d not in store", i+1)
	}
	if got := len(tx.Observations); got != observerCount158 {
		t.Fatalf("fixture: tx %d has %d observations, want %d", tx.ID, got, observerCount158)
	}
	for _, r := range paths158[i] {
		if n := countTxIn158(store, relays158[r], tx); n != 1 {
			t.Errorf("tx %d is in byPathHop[%s…] %d times, want exactly 1", tx.ID, relays158[r][:8], n)
		}
	}
}

// Late-observation path: the transmission is ingested with its first
// observation, then 10 more observers hear it through the same relays.
func TestPathHopIndexOncePerTx_LateObservations_158(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	seed158(t, db, observerCount158)
	store := loadedStore158(t, db)

	now := time.Now().UTC().Add(-10 * time.Minute)
	insertTx158(t, db, 0, now)
	insertObs158(t, db, 0, 1, now)
	store.IngestNewFromDB(0, 100)
	for _, r := range paths158[0] {
		if n := countTxIn158(store, relays158[r], store.byTxID[1]); n != 1 {
			t.Fatalf("fixture: first observation must index tx once under %s…, got %d", relays158[r][:8], n)
		}
	}

	for k := 1; k <= lateObs158; k++ {
		since := maxObsID158(t, db)
		insertObs158(t, db, 0, 1+k, now.Add(time.Duration(k)*time.Second))
		store.IngestNewObservations(since, 100)
	}
	assertOncePerRelay158(t, store, 0)
}

// Live-ingest path: the transmission arrives together with all of its
// observations in one IngestNewFromDB batch.
func TestPathHopIndexOncePerTx_LiveIngestBatch_158(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	seed158(t, db, observerCount158)
	store := loadedStore158(t, db)

	now := time.Now().UTC().Add(-10 * time.Minute)
	insertTx158(t, db, 0, now)
	for o := 1; o <= observerCount158; o++ {
		insertObs158(t, db, 0, o, now.Add(time.Duration(o)*time.Second))
	}
	store.IngestNewFromDB(0, 100)
	assertOncePerRelay158(t, store, 0)
}

// shares158 returns every relay's traffic share as seen by the three score
// functions, failing if they disagree.
func shares158(t *testing.T, store *PacketStore) map[string]float64 {
	t.Helper()
	bulk := store.computeRepeaterUsefulnessScoreMap()
	batch := store.GetRepeaterNodeStatsBatch(relays158, 24)
	out := make(map[string]float64, len(relays158))
	for _, pk := range relays158 {
		single := store.GetRepeaterUsefulnessScore(pk)
		if bulk[pk] != single || batch[pk].Score != single {
			t.Fatalf("score functions disagree for %s…: bulk=%v single=%v batch=%v", pk[:8], bulk[pk], single, batch[pk].Score)
		}
		out[pk] = single
	}
	return out
}

// expectedShares158 is the definition applied to the fixture: the fraction
// of the (all non-advert) transmissions whose path contains the relay.
func expectedShares158() map[string]float64 {
	out := make(map[string]float64, len(relays158))
	for _, path := range paths158 {
		for _, r := range path {
			out[relays158[r]] += 1 / float64(len(paths158))
		}
	}
	return out
}

func assertShares158(t *testing.T, label string, got map[string]float64) {
	t.Helper()
	want := expectedShares158()
	sum := 0.0
	for _, pk := range relays158 {
		sum += got[pk]
		if math.Abs(got[pk]-want[pk]) > 1e-9 {
			t.Errorf("%s: traffic share of %s… = %.4f, want %.4f", label, pk[:8], got[pk], want[pk])
		}
		if got[pk] >= 1 {
			t.Errorf("%s: traffic share of %s… reached the 1.0 clamp", label, pk[:8])
		}
	}
	if sum > maxPathLen158+1e-9 {
		t.Errorf("%s: sum of all relays' traffic share = %.3f, exceeds the maximum path length %d", label, sum, maxPathLen158)
	}
}

// The traffic share of a fixed set of transmissions is the same right after
// a load and after any number of further observations of them.
func TestTrafficShareStableAcrossLateObservations_158(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)

	// Restart: every observation is already persisted (resolved_path set by
	// the ingestor) and comes in through Load.
	loadDB := setupTestDB(t)
	defer loadDB.conn.Close()
	seed158(t, loadDB, observerCount158)
	for i := range paths158 {
		insertTx158(t, loadDB, i, base.Add(time.Duration(i)*time.Second))
		for o := 1; o <= observerCount158; o++ {
			insertObs158(t, loadDB, i, o, base.Add(time.Duration(i*100+o)*time.Second))
		}
	}
	loaded := loadedStore158(t, loadDB)
	afterLoad := shares158(t, loaded)
	assertShares158(t, "after load", afterLoad)

	// Live: the same transmissions arrive with one observation each, then
	// the late observations arrive in rounds.
	liveDB := setupTestDB(t)
	defer liveDB.conn.Close()
	seed158(t, liveDB, observerCount158)
	live := loadedStore158(t, liveDB)
	for i := range paths158 {
		insertTx158(t, liveDB, i, base.Add(time.Duration(i)*time.Second))
		insertObs158(t, liveDB, i, 1, base.Add(time.Duration(i*100+1)*time.Second))
	}
	live.IngestNewFromDB(0, 1000)
	afterIngest := shares158(t, live)
	assertShares158(t, "after live ingest", afterIngest)

	for round := 0; round < 2; round++ {
		since := maxObsID158(t, liveDB)
		for i := range paths158 {
			for k := 0; k < lateObs158/2; k++ {
				o := 2 + round*(lateObs158/2) + k
				insertObs158(t, liveDB, i, o, base.Add(time.Duration(i*100+o)*time.Second))
			}
		}
		live.IngestNewObservations(since, 10000)
		got := shares158(t, live)
		label := fmt.Sprintf("after late-observation round %d", round+1)
		assertShares158(t, label, got)
		if !reflect.DeepEqual(got, afterIngest) {
			t.Errorf("%s: shares changed without new transmissions:\n got %v\nwant %v", label, got, afterIngest)
		}
	}
	for i := range paths158 {
		assertOncePerRelay158(t, live, i)
	}
	if !reflect.DeepEqual(shares158(t, live), afterLoad) {
		t.Errorf("live shares differ from a fresh load of the same data:\nlive %v\nload %v", shares158(t, live), afterLoad)
	}
}

// Index size is a function of the transmissions, not of how many times they
// were heard: further observations through the same relays add nothing.
func TestPathHopIndexSizeBoundedByTransmissions_158(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	seed158(t, db, observerCount158)
	store := loadedStore158(t, db)
	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := range paths158 {
		insertTx158(t, db, i, base.Add(time.Duration(i)*time.Second))
		insertObs158(t, db, i, 1, base.Add(time.Duration(i*100+1)*time.Second))
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
	for o := 2; o <= observerCount158; o++ {
		since := maxObsID158(t, db)
		for i := range paths158 {
			insertObs158(t, db, i, o, base.Add(time.Duration(i*100+o)*time.Second))
		}
		store.IngestNewObservations(since, 10000)
		if got := entries(); got != want {
			t.Fatalf("after observation %d of each tx: byPathHop holds %d entries, want %d (one per relay key per tx)", o, got, want)
		}
	}
}

// Defence in depth: whatever the index holds, the score counts distinct
// transmissions, not bucket entries.
func TestTrafficShareCountsDistinctTransmissions_158(t *testing.T) {
	store := makeTestStore(0, time.Now().UTC(), 0)
	store.byPathHop = make(map[string][]*StoreTx)
	pt, advert := 2, payloadTypeAdvert
	relay := relays158[0]
	var txs []*StoreTx
	for i := 0; i < 10; i++ {
		tx := &StoreTx{ID: i + 1, Hash: fmt.Sprintf("d158-%d", i), PayloadType: &pt}
		txs = append(txs, tx)
		store.packets = append(store.packets, tx)
		store.byTxID[tx.ID] = tx
		store.byPayloadType[pt] = append(store.byPayloadType[pt], tx)
	}
	ad := &StoreTx{ID: 99, Hash: "d158-advert", PayloadType: &advert}
	store.byPayloadType[advert] = append(store.byPayloadType[advert], ad)
	// Two of the ten transmissions, each present 6 times (the pre-#158 index
	// shape after 6 observations), plus a duplicated advert.
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

// The per-transmission record behind the dedupe holds one hash per relay key
// of a live transmission: it does not grow with observations, and eviction
// removes it together with the transmission's byPathHop entries (#115).
func TestPathHopResolvedRecordBounded_158(t *testing.T) {
	store, old, young := evict115Store(t, 40, true)
	hopsSeen := map[string]bool{}
	for _, tx := range young {
		for k := 0; k < lateObs158; k++ {
			store.indexResolvedPathHops(tx, []string{evict115PK1, evict115PK2}, hopsSeen)
		}
	}
	if got := len(store.pathHopResolved); got != len(old)+len(young) {
		t.Fatalf("record holds %d transmissions, want %d", got, len(old)+len(young))
	}
	for _, tx := range young {
		if got := len(store.pathHopResolved[tx]); got != 2 {
			t.Fatalf("tx %d record holds %d keys after %d observations, want 2", tx.ID, got, 2+lateObs158)
		}
	}
	store.EvictStale()
	for _, tx := range old {
		if _, ok := store.pathHopResolved[tx]; ok {
			t.Fatalf("evicted tx %d is still in the indexed-key record", tx.ID)
		}
	}
	if got := len(store.pathHopResolved); got != len(young) {
		t.Fatalf("record holds %d transmissions after eviction, want %d", got, len(young))
	}
	assertEvictedGone115(t, store, old, young, young115Refs)
}

// A rebuild keeps the record of live transmissions (their resolved entries
// are carried over), so observations after it still add nothing; it drops
// the record of transmissions that are no longer in the store.
func TestPathHopResolvedRecordAcrossRebuild_158(t *testing.T) {
	store, _, young := evict115Store(t, 40, true)
	gone := store.packets[0]
	store.packets = store.packets[1:]
	delete(store.byTxID, gone.ID)
	delete(store.byHash, gone.Hash)
	store.buildPathHopIndex()
	if _, ok := store.pathHopResolved[gone]; ok {
		t.Fatal("rebuild kept the record of a transmission no longer in the store")
	}
	hopsSeen := map[string]bool{}
	for _, tx := range young {
		store.indexResolvedPathHops(tx, []string{evict115PK1, evict115PK2}, hopsSeen)
		if n := countIn(store.byPathHop, tx); n != young115Refs {
			t.Fatalf("tx %d has %d entries after a rebuild and another observation, want %d", tx.ID, n, young115Refs)
		}
	}
}

// Restart: Load indexes every persisted observation and rebuilds the index;
// a live observation after that must not add the transmission again.
func TestPathHopIndexOncePerTx_AfterLoad_158(t *testing.T) {
	db := setupTestDB(t)
	defer db.conn.Close()
	seed158(t, db, observerCount158)
	base := time.Now().UTC().Add(-30 * time.Minute)
	insertTx158(t, db, 0, base)
	for o := 1; o < observerCount158; o++ {
		insertObs158(t, db, 0, o, base.Add(time.Duration(o)*time.Second))
	}
	store := loadedStore158(t, db)
	since := maxObsID158(t, db)
	insertObs158(t, db, 0, observerCount158, base.Add(time.Minute))
	store.IngestNewObservations(since, 100)
	assertOncePerRelay158(t, store, 0)
}

func TestCountDistinctNonAdvert_158(t *testing.T) {
	pt, advert := 2, payloadTypeAdvert
	tx := func(id int) *StoreTx { return &StoreTx{ID: id, PayloadType: &pt} }
	t1, t2, t3 := tx(1), tx(2), tx(3)
	ad := &StoreTx{ID: 4, PayloadType: &advert}
	cases := []struct {
		name string
		list []*StoreTx
		want int
	}{
		{"empty", nil, 0},
		{"ascending", []*StoreTx{t1, t2, t3}, 3},
		{"descending (chunk load order)", []*StoreTx{t3, t2, t1}, 3},
		{"adjacent duplicate", []*StoreTx{t1, t1}, 1},
		{"ascending then duplicate", []*StoreTx{t1, t2, t3, t3}, 3},
		{"interleaved duplicates", []*StoreTx{t1, t2, t1, t3, t2}, 3},
		{"adverts and nils skipped", []*StoreTx{nil, ad, t2, ad, nil, t2}, 1},
	}
	var ids []int
	for _, c := range cases {
		var got int
		got, ids = countDistinctNonAdvert(c.list, ids)
		if got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// duplicateFullKeyEntries158 reproduces the pre-#158 index shape: every
// transmission in a full-pubkey bucket is present `extra` more times.
func duplicateFullKeyEntries158(store *PacketStore, extra int) {
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
// their results. This pins that, so a refactor cannot reintroduce #158 there.
func TestPathHopConsumersIgnoreDuplicateEntries_158(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)
	db := setupTestDB(t)
	defer db.conn.Close()
	seed158(t, db, observerCount158)
	for i := range paths158 {
		insertTx158(t, db, i, base.Add(time.Duration(i)*time.Second))
		for o := 1; o <= 2; o++ {
			insertObs158(t, db, i, o, base.Add(time.Duration(i*100+o)*time.Second))
		}
	}
	store := loadedStore158(t, db)

	type snapshot struct {
		hops  map[string][]HopAnalyticsPacket
		info  map[string]RepeaterRelayInfo
		multi []MultiByteCapEntry
	}
	take := func() snapshot {
		s := snapshot{hops: map[string][]HopAnalyticsPacket{}, info: map[string]RepeaterRelayInfo{}}
		batch := store.GetRepeaterNodeStatsBatch(relays158, 24)
		for _, pk := range relays158 {
			resp, err := store.GetNodeHopAnalytics(pk, 7)
			if err != nil || resp == nil {
				t.Fatalf("GetNodeHopAnalytics(%s…): %v, %v", pk[:8], resp, err)
			}
			s.hops[pk] = resp.Packets
			s.info[pk] = batch[pk].Info
		}
		s.multi = store.computeMultiByteCapability(nil)
		return s
	}
	before := take()
	if len(before.hops[relays158[0]]) == 0 || before.info[relays158[0]].RelayCount24h == 0 {
		t.Fatalf("fixture: relay 0 must have hop analytics and relay counts: %+v / %+v", before.hops[relays158[0]], before.info[relays158[0]])
	}
	duplicateFullKeyEntries158(store, lateObs158)
	after := take()
	for _, pk := range relays158 {
		if !reflect.DeepEqual(before.hops[pk], after.hops[pk]) {
			t.Errorf("GetNodeHopAnalytics(%s…) changed with duplicate entries:\nbefore %+v\n after %+v", pk[:8], before.hops[pk], after.hops[pk])
		}
		if !reflect.DeepEqual(before.info[pk], after.info[pk]) {
			t.Errorf("relay info of %s… changed with duplicate entries:\nbefore %+v\n after %+v", pk[:8], before.info[pk], after.info[pk])
		}
	}
	if !reflect.DeepEqual(before.multi, after.multi) {
		t.Errorf("computeMultiByteCapability changed with duplicate entries:\nbefore %+v\n after %+v", before.multi, after.multi)
	}
}

// computeMultiByteCapability reads raw prefix buckets only and keeps a
// maximum hash size per node, so duplicates there cannot inflate it either.
func TestMultiByteCapabilityIgnoresDuplicateEntries_158(t *testing.T) {
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
	for k := 0; k < lateObs158; k++ {
		store.byPathHop["aabb"] = append(store.byPathHop["aabb"], pkt)
	}
	store.mu.Unlock()
	after := store.computeMultiByteCapability(nil)
	if len(before) != 1 || !reflect.DeepEqual(before, after) {
		t.Fatalf("duplicates changed multi-byte capability:\nbefore %+v\n after %+v", before, after)
	}
}

// lateObsBenchStore158: n transmissions one second apart, each with 3 raw
// one-byte hops of which 2 resolve to one of 4000 relays (the shape of
// evict115BenchStore), indexed once as by their first observation.
func lateObsBenchStore158(n int) (*PacketStore, [][]string) {
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

// BenchmarkLateObservationIndex_158 measures the store side of
// IngestNewObservations for one late observation (indexResolvedPathHops
// under the write lock, as the ingest loop calls it) on stores of realistic
// size, and reports how many byPathHop entries a transmission ends up with.
// Before #158 that number grows with every observation; after, it stays at
// 3 raw + 2 resolved.
func BenchmarkLateObservationIndex_158(b *testing.B) {
	for _, n := range []int{20000, 100000} {
		b.Run(fmt.Sprintf("txs=%d", n), func(b *testing.B) {
			store, pks := lateObsBenchStore158(n)
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

// BenchmarkIngestNewObservations_158 drives the real late-observation ingest
// (SQL scan + index) for a batch of one new observation per transmission.
func BenchmarkIngestNewObservations_158(b *testing.B) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)
	db := setupTestDB(b)
	defer db.conn.Close()
	const observers = 512
	seed158(b, db, observers)
	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := range paths158 {
		insertTx158(b, db, i, base.Add(time.Duration(i)*time.Second))
		insertObs158(b, db, i, 1, base.Add(time.Duration(i*1000+1)*time.Second))
	}
	store := loadedStore158(b, db)
	store.IngestNewFromDB(0, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		b.StopTimer()
		o := 2 + it%(observers-1)
		since := maxObsID158(b, db)
		for i := range paths158 {
			insertObs158(b, db, i, o, base.Add(time.Duration(i*1000+o)*time.Second))
		}
		b.StartTimer()
		store.IngestNewObservations(since, 10000)
	}
}

// BenchmarkTrafficShareScoreMap_158 measures the bulk traffic-share pass
// (cache-miss path of GetRepeaterUsefulnessScoreMap) on realistic sizes.
// order=reversed reverses every bucket, as when background chunks load
// older transmissions after newer ones: no bucket is in ascending ID order.
func BenchmarkTrafficShareScoreMap_158(b *testing.B) {
	for _, c := range []struct {
		n        int
		reversed bool
	}{{20000, false}, {100000, false}, {20000, true}, {100000, true}} {
		order := "ingest"
		if c.reversed {
			order = "reversed"
		}
		b.Run(fmt.Sprintf("txs=%d/order=%s", c.n, order), func(b *testing.B) {
			store, _ := lateObsBenchStore158(c.n)
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
