package main

import (
	"fmt"
	"io"
	"log"
	"math"
	"strings"
	"testing"
	"time"
)

// Issue #158 follow-up: after #162 the traffic share no longer grew from
// duplicate byPathHop entries, but its sum still rose with uptime on
// staging (14.3 at 2.6 h, 20.7 at 18 h). Load and the background chunk load
// index the resolved_path the ingestor persisted (#1547), which leaves a hop
// unresolved when its prefix is ambiguous (#1560). Live ingest
// re-resolved every observation with the server's resolver instead, which
// always picks a candidate (affinity, geo proximity or observation count)
// and can pick a different one per observer. So the same transmissions
// counted for more relays when they arrived live than after a restart, and
// the sum climbed as live traffic replaced the loaded part of the store.

// Two repeaters share the 1-byte prefix E5, one in Jutland and one on
// Zealand; the ingestor cannot tell them apart and persists null for that
// hop. A1 and B2 are unique. The two observers are repeaters near each E5
// node, so the server's geo-proximity tier resolves E5 differently per
// observer.
var (
	shareA1  = relays158[0]
	shareB2  = relays158[1]
	shareE5W = "e5aa" + strings.Repeat("55", 30)
	shareE5E = "e5bb" + strings.Repeat("66", 30)
	shareObW = "f1" + strings.Repeat("77", 31)
	shareObE = "f2" + strings.Repeat("88", 31)
)

const shareTxs = 20 // even: through A1,E5; odd: through B2 only

func seedShareGrowth(t testing.TB, db *DB) {
	t.Helper()
	ts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for _, n := range []struct {
		pk       string
		lat, lon any
	}{
		{shareA1, nil, nil},
		{shareB2, nil, nil},
		{shareE5W, 56.15, 9.55},
		{shareE5E, 55.65, 12.10},
		{shareObW, 56.10, 9.50},
		{shareObE, 55.60, 12.20},
	} {
		exec158(t, db, `INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', ?, ?, ?, '2026-01-01', 1)`,
			n.pk, "Share-"+n.pk[:4], n.lat, n.lon, ts)
	}
	for i, pk := range []string{shareObW, shareObE} {
		exec158(t, db, `INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, 'TST')`, i+1, pk, "Obs-"+pk[:4])
	}
}

// insertShareObs inserts observation obsIdx (1 or 2) of transmission i the
// way the ingestor writes it: path_json and, in the same row, its own
// resolved_path, with null for the ambiguous E5 hop.
func insertShareObs(t testing.TB, db *DB, i, obsIdx int, ts time.Time) {
	t.Helper()
	path, rp := `["B2"]`, `["`+shareB2+`"]`
	if i%2 == 0 {
		path, rp = `["A1","E5"]`, `["`+shareA1+`",null]`
	}
	exec158(t, db, `INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path)
		VALUES (?, ?, 5, -90, ?, ?, ?)`, i+1, obsIdx, path, ts.Unix(), rp)
}

func insertShareTx(t testing.TB, db *DB, i int, ts time.Time) {
	t.Helper()
	exec158(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES (?, 'CAFE', ?, ?, 1, 2, '{"type":"TXT_MSG"}')`, i+1, fmt.Sprintf("share158-%03d", i), ts.Format(time.RFC3339))
}

var shareKeys = []string{shareA1, shareB2, shareE5W, shareE5E}

func shareSnapshot(store *PacketStore) (map[string]float64, float64) {
	bulk := store.computeRepeaterUsefulnessScoreMap()
	out := make(map[string]float64, len(shareKeys))
	sum := 0.0
	for _, pk := range shareKeys {
		out[pk] = bulk[pk]
	}
	for key, v := range bulk {
		if len(key) == 64 { // full pubkeys only, as /api/nodes reads them
			sum += v
		}
	}
	return out, sum
}

// The traffic share of a fixed set of transmissions must not depend on
// whether they were loaded at startup or ingested live: otherwise the sum
// drifts with uptime as the retention window turns over.
func TestTrafficShareLiveMatchesLoad_AmbiguousPrefix_158(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)

	// Restart: everything comes in through Load.
	loadDB := setupTestDB(t)
	defer loadDB.conn.Close()
	seedShareGrowth(t, loadDB)
	for i := 0; i < shareTxs; i++ {
		insertShareTx(t, loadDB, i, base.Add(time.Duration(i)*time.Second))
		insertShareObs(t, loadDB, i, 1, base.Add(time.Duration(i*10+1)*time.Second))
		insertShareObs(t, loadDB, i, 2, base.Add(time.Duration(i*10+2)*time.Second))
	}
	loaded := loadedStore158(t, loadDB)
	loadShares, loadSum := shareSnapshot(loaded)
	if math.Abs(loadShares[shareA1]-0.5) > 1e-9 || math.Abs(loadShares[shareB2]-0.5) > 1e-9 {
		t.Fatalf("fixture: after load A1=%v B2=%v, want 0.5 each", loadShares[shareA1], loadShares[shareB2])
	}

	cases := []struct {
		name   string
		ingest func(t *testing.T, db *DB, store *PacketStore)
	}{
		{"live batch", func(t *testing.T, db *DB, store *PacketStore) {
			for i := 0; i < shareTxs; i++ {
				insertShareTx(t, db, i, base.Add(time.Duration(i)*time.Second))
				insertShareObs(t, db, i, 1, base.Add(time.Duration(i*10+1)*time.Second))
				insertShareObs(t, db, i, 2, base.Add(time.Duration(i*10+2)*time.Second))
			}
			store.IngestNewFromDB(0, 1000)
		}},
		{"late observation", func(t *testing.T, db *DB, store *PacketStore) {
			for i := 0; i < shareTxs; i++ {
				insertShareTx(t, db, i, base.Add(time.Duration(i)*time.Second))
				insertShareObs(t, db, i, 1, base.Add(time.Duration(i*10+1)*time.Second))
			}
			store.IngestNewFromDB(0, 1000)
			since := maxObsID158(t, db)
			for i := 0; i < shareTxs; i++ {
				insertShareObs(t, db, i, 2, base.Add(time.Duration(i*10+2)*time.Second))
			}
			store.IngestNewObservations(since, 1000)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			defer db.conn.Close()
			seedShareGrowth(t, db)
			store := loadedStore158(t, db)
			tc.ingest(t, db, store)

			got, sum := shareSnapshot(store)
			for _, pk := range shareKeys {
				if math.Abs(got[pk]-loadShares[pk]) > 1e-9 {
					t.Errorf("traffic share of %s… = %.3f live, %.3f after load of the same rows", pk[:6], got[pk], loadShares[pk])
				}
			}
			if math.Abs(sum-loadSum) > 1e-9 {
				t.Errorf("sum of traffic shares = %.3f live, %.3f after load of the same rows", sum, loadSum)
			}
			// One hop is one relay: no transmission may sit under more
			// resolved full keys than it has distinct hops.
			store.mu.RLock()
			defer store.mu.RUnlock()
			for i := 0; i < shareTxs; i += 2 {
				tx := store.byTxID[i+1]
				n := 0
				for _, pk := range shareKeys {
					if countTxInLocked(store.byPathHop[pk], tx) > 0 {
						n++
					}
				}
				if n > 2 {
					t.Errorf("tx %d (2 hops) is indexed under %d relays", tx.ID, n)
					break
				}
			}
		})
	}
}

func countTxInLocked(list []*StoreTx, tx *StoreTx) int {
	n := 0
	for _, t := range list {
		if t == tx {
			n++
		}
	}
	return n
}

// An observation the ingestor could not resolve at all is stored with a
// NULL resolved_path. Load then re-resolves path_json on unique prefixes
// into byNode only (#1352, PR #1643); live ingest must index it the same
// way rather than resolve it itself.
func TestLiveIngestIndexesLikeLoad_NullResolvedPath_158(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)
	insert := func(t *testing.T, db *DB) {
		insertShareTx(t, db, 0, base)
		exec158(t, db, `INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path)
			VALUES (1, 1, 5, -90, '["A1","E5"]', ?, NULL)`, base.Unix())
	}
	type view struct{ pathHopA1, pathHopE5, byNodeA1, byNodeE5 bool }
	snapshot := func(store *PacketStore) view {
		store.mu.RLock()
		defer store.mu.RUnlock()
		tx := store.byTxID[1]
		if tx == nil {
			t.Fatal("tx 1 not in store")
		}
		e5 := countTxInLocked(store.byPathHop[shareE5W], tx)+countTxInLocked(store.byPathHop[shareE5E], tx) > 0
		e5n := countTxInLocked(store.byNode[shareE5W], tx)+countTxInLocked(store.byNode[shareE5E], tx) > 0
		return view{
			pathHopA1: countTxInLocked(store.byPathHop[shareA1], tx) > 0,
			pathHopE5: e5,
			byNodeA1:  countTxInLocked(store.byNode[shareA1], tx) > 0,
			byNodeE5:  e5n,
		}
	}

	loadDB := setupTestDB(t)
	defer loadDB.conn.Close()
	seedShareGrowth(t, loadDB)
	insert(t, loadDB)
	want := snapshot(loadedStore158(t, loadDB))
	if want != (view{byNodeA1: true}) {
		t.Fatalf("fixture: after load %+v, want only byNode under the unique A1", want)
	}

	liveDB := setupTestDB(t)
	defer liveDB.conn.Close()
	seedShareGrowth(t, liveDB)
	live := loadedStore158(t, liveDB)
	insert(t, liveDB)
	live.IngestNewFromDB(0, 100)
	if got := snapshot(live); got != want {
		t.Errorf("live ingest indexed %+v, load of the same row %+v", got, want)
	}
}

// insertShareObsPath inserts one observation with an explicit path and
// persisted resolved_path (rp == "" writes NULL), the shape of an
// observation another observer reports later through different relays.
func insertShareObsPath(t testing.TB, db *DB, i, obsIdx int, path, rp string, ts time.Time) {
	t.Helper()
	var rpArg any
	if rp != "" {
		rpArg = rp
	}
	exec158(t, db, `INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path)
		VALUES (?, ?, 5, -90, ?, ?, ?)`, i+1, obsIdx, path, ts.Unix(), rpArg)
}

// A late observation from ANOTHER observer can carry a persisted
// resolved_path with relays the first observation did not have. The first
// observation of the late-observation case above reuses the same path, so
// there is nothing new to index; here the late observation brings a relay
// (B2 for even transmissions, A1 for odd ones) that only its resolved_path
// names. IngestNewObservations must index it exactly as Load does, or live
// traffic share drifts from the loaded one (mutant: ignore resolved_path
// and index "" instead).
func TestTrafficShareLiveMatchesLoad_LateObserverNewRelay_158(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)
	first := func(t *testing.T, db *DB, i int) {
		insertShareObs(t, db, i, 1, base.Add(time.Duration(i*10+1)*time.Second))
	}
	late := func(t *testing.T, db *DB, i int) {
		// Observer 2 heard it through A1 and B2: whichever relay the first
		// observation lacked is new here.
		insertShareObsPath(t, db, i, 2, `["A1","B2"]`, `["`+shareA1+`","`+shareB2+`"]`, base.Add(time.Duration(i*10+2)*time.Second))
	}

	loadDB := setupTestDB(t)
	defer loadDB.conn.Close()
	seedShareGrowth(t, loadDB)
	for i := 0; i < shareTxs; i++ {
		insertShareTx(t, loadDB, i, base.Add(time.Duration(i)*time.Second))
		first(t, loadDB, i)
		late(t, loadDB, i)
	}
	loaded := loadedStore158(t, loadDB)
	loadShares, loadSum := shareSnapshot(loaded)
	if math.Abs(loadShares[shareA1]-1.0) > 1e-9 || math.Abs(loadShares[shareB2]-1.0) > 1e-9 {
		t.Fatalf("fixture: after load A1=%v B2=%v, want 1.0 each (every tx passes both through some observation)", loadShares[shareA1], loadShares[shareB2])
	}

	db := setupTestDB(t)
	defer db.conn.Close()
	seedShareGrowth(t, db)
	store := loadedStore158(t, db)
	for i := 0; i < shareTxs; i++ {
		insertShareTx(t, db, i, base.Add(time.Duration(i)*time.Second))
		first(t, db, i)
	}
	store.IngestNewFromDB(0, 1000)
	since := maxObsID158(t, db)
	for i := 0; i < shareTxs; i++ {
		late(t, db, i)
	}
	store.IngestNewObservations(since, 1000)

	got, sum := shareSnapshot(store)
	for _, pk := range shareKeys {
		if math.Abs(got[pk]-loadShares[pk]) > 1e-9 {
			t.Errorf("traffic share of %s… = %.3f live, %.3f after load of the same rows", pk[:6], got[pk], loadShares[pk])
		}
	}
	if math.Abs(sum-loadSum) > 1e-9 {
		t.Errorf("sum of traffic shares = %.3f live, %.3f after load of the same rows", sum, loadSum)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, pk := range shareKeys {
		if g, w := len(store.byPathHop[pk]), len(loaded.byPathHop[pk]); g != w {
			t.Errorf("byPathHop[%s…] holds %d transmissions live, %d after load", pk[:6], g, w)
		}
	}
}

// The same NULL case as TestLiveIngestIndexesLikeLoad_NullResolvedPath_158,
// for a late observation of a transmission already in the store: an
// observation the ingestor could not resolve must be re-resolved on unique
// prefixes into byNode only, never into byPathHop, exactly as Load does.
func TestLateObservationIndexesLikeLoad_NullResolvedPath_158(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute)
	type view struct{ pathHopA1, pathHopB2, byNodeA1, byNodeB2 bool }
	snapshot := func(store *PacketStore) view {
		store.mu.RLock()
		defer store.mu.RUnlock()
		tx := store.byTxID[1]
		if tx == nil {
			t.Fatal("tx 1 not in store")
		}
		return view{
			pathHopA1: countTxInLocked(store.byPathHop[shareA1], tx) > 0,
			pathHopB2: countTxInLocked(store.byPathHop[shareB2], tx) > 0,
			byNodeA1:  countTxInLocked(store.byNode[shareA1], tx) > 0,
			byNodeB2:  countTxInLocked(store.byNode[shareB2], tx) > 0,
		}
	}
	// First observation: resolved by the ingestor (A1). Late observation
	// from the other observer: NULL, path through the unique B2.
	insertFirst := func(t *testing.T, db *DB) {
		insertShareTx(t, db, 0, base)
		insertShareObsPath(t, db, 0, 1, `["A1"]`, `["`+shareA1+`"]`, base.Add(time.Second))
	}
	insertLate := func(t *testing.T, db *DB) {
		insertShareObsPath(t, db, 0, 2, `["B2"]`, "", base.Add(2*time.Second))
	}

	loadDB := setupTestDB(t)
	defer loadDB.conn.Close()
	seedShareGrowth(t, loadDB)
	insertFirst(t, loadDB)
	insertLate(t, loadDB)
	want := snapshot(loadedStore158(t, loadDB))
	if want != (view{pathHopA1: true, byNodeA1: true, byNodeB2: true}) {
		t.Fatalf("fixture: after load %+v, want byPathHop only under A1 and byNode under A1 and B2", want)
	}

	liveDB := setupTestDB(t)
	defer liveDB.conn.Close()
	seedShareGrowth(t, liveDB)
	live := loadedStore158(t, liveDB)
	insertFirst(t, liveDB)
	live.IngestNewFromDB(0, 100)
	since := maxObsID158(t, liveDB)
	insertLate(t, liveDB)
	live.IngestNewObservations(since, 100)
	if got := snapshot(live); got != want {
		t.Errorf("late observation indexed %+v, load of the same rows %+v", got, want)
	}
}

// decodePersistedRelayPath is what the live ingest paths call before taking
// s.mu. A NULL/empty column must stay distinguishable from a persisted path
// whose hops are all null: the first takes the path_json fallback, the
// second is indexed as given (nothing).
func TestDecodePersistedRelayPath_158(t *testing.T) {
	for _, c := range []struct {
		name, in string
		want     persistedRelayPath
	}{
		{"NULL column", "", persistedRelayPath{}},
		{"all hops null", `[null,null]`, persistedRelayPath{present: true}},
		{"partly resolved", `["` + shareA1 + `",null,"` + shareB2 + `"]`, persistedRelayPath{pubkeys: []string{shareA1, shareB2}, present: true}},
		{"empty list", `[]`, persistedRelayPath{present: true}},
		{"corrupt json", `[`, persistedRelayPath{present: true}},
		{"non-string hop", `[1,"` + shareA1 + `"]`, persistedRelayPath{present: true}},
		{"empty string hop", `["","` + shareB2 + `"]`, persistedRelayPath{pubkeys: []string{shareB2}, present: true}},
	} {
		got := decodePersistedRelayPath(c.in)
		if got.present != c.want.present || strings.Join(got.pubkeys, ",") != strings.Join(c.want.pubkeys, ",") {
			t.Errorf("%s: decodePersistedRelayPath(%q) = %+v, want %+v", c.name, c.in, got, c.want)
		}
		// Oracle: the two functions the decode replaces.
		if c.in != "" {
			if o := extractResolvedPubkeys(unmarshalResolvedPath(c.in)); strings.Join(o, ",") != strings.Join(got.pubkeys, ",") {
				t.Errorf("%s: %q decodes to %v, old extractResolvedPubkeys(unmarshalResolvedPath) gives %v", c.name, c.in, got.pubkeys, o)
			}
		}
	}
}

// BenchmarkIngestNewFromDB_158 drives the real live ingest (SQL scan +
// index + broadcast build) of a batch of 20 new transmissions with 11
// observations each, as the ingestor writes them (resolved_path set).
func BenchmarkIngestNewFromDB_158(b *testing.B) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)
	db := setupTestDB(b)
	defer db.conn.Close()
	seed158(b, db, observerCount158)
	store := loadedStore158(b, db)
	base := time.Now().UTC().Add(-30 * time.Minute)
	nextID := 1
	b.ReportAllocs()
	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		b.StopTimer()
		since := nextID - 1
		for i := range paths158 {
			ts := base.Add(time.Duration(nextID) * time.Millisecond)
			exec158(b, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
				VALUES (?, 'CAFE', ?, ?, 1, 2, '{"type":"TXT_MSG"}')`, nextID, fmt.Sprintf("bench158-%07d", nextID), ts.Format(time.RFC3339))
			for o := 1; o <= observerCount158; o++ {
				exec158(b, db, `INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path)
					VALUES (?, ?, 5, -90, ?, ?, ?)`, nextID, o, rawHopsJSON158(paths158[i]), ts.Unix()+int64(o), resolvedJSON158(paths158[i]))
			}
			nextID++
		}
		b.StartTimer()
		store.IngestNewFromDB(since, 1000)
	}
}
