package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Issue #411: a transmission's observations are deduplicated by scanning
// them until it has obsIndexThreshold of them; only then does it carry the
// obsKeys/observerSet maps. Every load and update path must keep the same
// observations, unique observer counts, relay indexes and charges as before,
// and no transmission below the threshold may hold the maps.

// obs411Want is what a transmission must hold after dedup, computed from the
// rows alone: the distinct observer/path pairs and non-empty observers.
type obs411Want struct {
	keys      []string
	observers int
}

func obs411Expected(txs []obs411Tx) map[string]obs411Want {
	out := make(map[string]obs411Want, len(txs))
	for _, tx := range txs {
		keys, observers := map[string]bool{}, map[string]bool{}
		for _, r := range tx.rows {
			pj, _ := json.Marshal(r.path)
			keys[r.observer+"|"+string(pj)] = true
			if r.observer != "" {
				observers[r.observer] = true
			}
		}
		w := obs411Want{observers: len(observers)}
		for k := range keys {
			w.keys = append(w.keys, k)
		}
		sort.Strings(w.keys)
		out[tx.hash] = w
	}
	return out
}

// obs411Snapshot is the per-transmission dedup state and the relay indexes.
type obs411Snapshot struct {
	Tx           map[string]obs411TxState
	ByPathHop    map[string][]string
	ByNode       map[string][]string
	Resolved     map[string][]uint64
	Fallback     map[string][]string
	TrackedBytes int64
	TotalObs     int
}

type obs411TxState struct {
	Count, Unique int
	Keys          []string
	Indexed       bool
}

func obs411Hashes(list []*StoreTx) []string {
	out := make([]string, 0, len(list))
	for _, tx := range list {
		out = append(out, tx.Hash)
	}
	sort.Strings(out)
	return out
}

func obs411Snap(s *PacketStore) obs411Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sn := obs411Snapshot{Tx: map[string]obs411TxState{}, ByPathHop: map[string][]string{}, ByNode: map[string][]string{},
		Resolved: map[string][]uint64{}, Fallback: map[string][]string{}, TrackedBytes: s.trackedBytes, TotalObs: s.totalObs}
	for _, tx := range s.packets {
		st := obs411TxState{Count: tx.ObservationCount, Unique: int(tx.UniqueObserverCount), Indexed: tx.obsKeys != nil}
		for _, o := range tx.Observations {
			st.Keys = append(st.Keys, o.ObserverID+"|"+o.PathJSON)
		}
		sort.Strings(st.Keys)
		sn.Tx[tx.Hash] = st
	}
	for k, list := range s.byPathHop {
		sn.ByPathHop[k] = obs411Hashes(list)
	}
	for k, list := range s.byNode {
		sn.ByNode[k] = obs411Hashes(list)
	}
	for tx, h := range s.pathHopResolved {
		sn.Resolved[tx.Hash] = append([]uint64(nil), h...)
		sort.Slice(sn.Resolved[tx.Hash], func(i, j int) bool { return sn.Resolved[tx.Hash][i] < sn.Resolved[tx.Hash][j] })
	}
	for tx, pks := range s.fallbackByNode {
		sn.Fallback[tx.Hash] = append([]string(nil), pks...)
		sort.Strings(sn.Fallback[tx.Hash])
	}
	return sn
}

// obs411CheckTx asserts the dedup state of every live transmission: the
// observations are exactly the distinct pairs of its rows, the counts agree,
// and the maps exist exactly from obsIndexThreshold on and mirror the list.
func obs411CheckTx(t *testing.T, s *PacketStore, want map[string]obs411Want, where string) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	bad := 0
	fail := func(format string, args ...interface{}) {
		t.Helper()
		if bad++; bad <= 5 {
			t.Errorf(where+": "+format, args...)
		}
	}
	for _, tx := range s.packets {
		var keys []string
		observers := map[string]bool{}
		for _, o := range tx.Observations {
			keys = append(keys, o.ObserverID+"|"+o.PathJSON)
			if o.ObserverID != "" {
				observers[o.ObserverID] = true
			}
		}
		sort.Strings(keys)
		if w, ok := want[tx.Hash]; ok {
			if !reflect.DeepEqual(keys, w.keys) {
				fail("tx %s holds %d observations, want the %d distinct observer/path pairs", tx.Hash, len(keys), len(w.keys))
			}
			if int(tx.UniqueObserverCount) != w.observers {
				fail("tx %s UniqueObserverCount = %d, want %d", tx.Hash, tx.UniqueObserverCount, w.observers)
			}
		}
		if tx.ObservationCount != len(tx.Observations) || int(tx.UniqueObserverCount) != len(observers) {
			fail("tx %s counts %d/%d, list holds %d observations by %d observers", tx.Hash, tx.ObservationCount, tx.UniqueObserverCount, len(tx.Observations), len(observers))
		}
		indexed := len(tx.Observations) >= obsIndexThreshold
		if (tx.obsKeys != nil) != indexed || (tx.observerSet != nil) != indexed {
			fail("tx %s with %d observations: maps present = %v/%v, want %v (threshold %d)", tx.Hash, len(tx.Observations), tx.obsKeys != nil, tx.observerSet != nil, indexed, obsIndexThreshold)
		}
		if indexed && (len(tx.obsKeys) != len(tx.Observations) || len(tx.observerSet) != len(observers)) {
			fail("tx %s maps hold %d keys and %d observers, list %d and %d", tx.Hash, len(tx.obsKeys), len(tx.observerSet), len(tx.Observations), len(observers))
		}
		for _, o := range tx.Observations {
			if !tx.hasObservation(o.ObserverID, o.PathJSON) {
				fail("tx %s: hasObservation misses its own observation %d", tx.Hash, o.ID)
				break
			}
		}
	}
}

// obs411Paths are the load and update paths a store can be filled through.
type obs411Path struct {
	name  string
	build func(t *testing.T, txs []obs411Tx) *PacketStore
}

func obs411Open(t *testing.T, dbPath string, cfg *PacketStoreConfig) *PacketStore {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	return NewPacketStore(db, cfg)
}

func obs411Ready(t *testing.T, s *PacketStore) *PacketStore {
	t.Helper()
	if !s.WaitIndexesReady(60 * time.Second) {
		t.Fatal("background index builds did not finish")
	}
	return s
}

// obs411IngestAll drains IngestNewFromDB from the store's watermark.
func obs411IngestAll(s *PacketStore) {
	for since := s.MaxTransmissionID(); ; {
		_, next := s.IngestNewFromDB(since, 97)
		if next <= since {
			return
		}
		since = next
	}
}

func obs411PathList() []obs411Path {
	return []obs411Path{
		{"hot Load", func(t *testing.T, txs []obs411Tx) *PacketStore {
			s := obs411Open(t, obs411DB(t, txs), &PacketStoreConfig{})
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			return obs411Ready(t, s)
		}},
		{"LoadChunked", func(t *testing.T, txs []obs411Tx) *PacketStore {
			s := obs411Open(t, obs411DB(t, txs), &PacketStoreConfig{RetentionHours: 168})
			if err := s.RunStartupLoad(53); err != nil {
				t.Fatal(err)
			}
			return obs411Ready(t, s)
		}},
		{"background fill", func(t *testing.T, txs []obs411Tx) *PacketStore {
			s := obs411Open(t, obs411DB(t, txs), &PacketStoreConfig{RetentionHours: 168, HotStartupHours: 2})
			if err := s.RunStartupLoad(53); err != nil {
				t.Fatal(err)
			}
			return obs411Ready(t, s)
		}},
		{"ingest", func(t *testing.T, txs []obs411Tx) *PacketStore {
			s := obs411Open(t, obs411DB(t, txs), &PacketStoreConfig{})
			obs411IngestAll(s)
			return s
		}},
		{"upsert", func(t *testing.T, txs []obs411Tx) *PacketStore {
			// Each transmission arrives with its first observation; the rest
			// reach the store as observations of transmissions it holds.
			dbPath := filepath.Join(t.TempDir(), "obs411.db")
			conn := obs411Create(t, dbPath)
			defer conn.Close()
			obs411Write(t, conn, txs, true, func(j int) bool { return j == 0 })
			s := obs411Open(t, dbPath, &PacketStoreConfig{})
			obs411IngestAll(s)
			obs411Write(t, conn, txs, false, func(j int) bool { return j > 0 })
			for since := s.MaxObservationID(); ; {
				s.IngestNewObservations(since, 101)
				next := s.MaxObservationID()
				if next <= since {
					break
				}
				since = next
			}
			return s
		}},
	}
}

// obs411Set is ordinary transmissions (the first half 3 days old) and a few
// busy ones far past the threshold, with exact repeats after the threshold.
func obs411Set() []obs411Tx {
	txs := obs411Data(600, 3, 300, 411)
	for i := 600; i < len(txs); i++ {
		tx := &txs[i]
		next := tx.rows[len(tx.rows)-1].id
		for j := 0; j < 40; j++ { // repeats of rows already past the threshold
			r := tx.rows[obsIndexThreshold+j*7]
			next += 1_000_000
			r.id = next
			tx.rows = append(tx.rows, r)
		}
	}
	return txs
}

func TestObsDedup411_PathsKeepTheSameObservationsAndIndexes(t *testing.T) {
	txs := obs411Set()
	want := obs411Expected(txs)
	indexed := 0
	for _, w := range want {
		if len(w.keys) >= obsIndexThreshold {
			indexed++
		}
	}
	if indexed < 3 || indexed == len(want) {
		t.Fatalf("setup: %d of %d transmissions reach the threshold", indexed, len(want))
	}
	var ref obs411Snapshot
	for i, p := range obs411PathList() {
		s := p.build(t, txs)
		if len(s.packets) != len(txs) {
			t.Fatalf("%s: %d transmissions in the store, want %d", p.name, len(s.packets), len(txs))
		}
		obs411CheckTx(t, s, want, p.name)
		acct113Check(t, s, p.name)
		sn := obs411Snap(s)
		if len(sn.Resolved) == 0 || len(sn.ByPathHop) == 0 {
			t.Fatalf("%s: setup: no relay indexes", p.name)
		}
		if i == 0 {
			ref = sn
			continue
		}
		if !reflect.DeepEqual(sn.Tx, ref.Tx) {
			t.Errorf("%s: per-transmission observations differ from hot Load", p.name)
		}
		if !reflect.DeepEqual(sn.Resolved, ref.Resolved) || !reflect.DeepEqual(sn.ByPathHop, ref.ByPathHop) ||
			!reflect.DeepEqual(sn.ByNode, ref.ByNode) || !reflect.DeepEqual(sn.Fallback, ref.Fallback) {
			t.Errorf("%s: relay indexes differ from hot Load (resolved %d/%d, byPathHop %d/%d, byNode %d/%d)", p.name,
				len(sn.Resolved), len(ref.Resolved), len(sn.ByPathHop), len(ref.ByPathHop), len(sn.ByNode), len(ref.ByNode))
		}
		if sn.TotalObs != ref.TotalObs || sn.TrackedBytes != ref.TrackedBytes {
			t.Errorf("%s: totalObs/trackedBytes = %d/%d, hot Load %d/%d", p.name, sn.TotalObs, sn.TrackedBytes, ref.TotalObs, ref.TrackedBytes)
		}
	}
}

// Retention eviction of the old half, then of everything, credits exactly
// what each path charged, the dedup maps of busy transmissions included.
func TestObsDedup411_EvictionCreditsWhatEachPathCharged(t *testing.T) {
	txs := obs411Set()
	want := obs411Expected(txs)
	var ref obs411Snapshot
	for i, p := range obs411PathList() {
		s := p.build(t, txs)
		s.mu.Lock()
		s.retentionHours = 24
		s.mu.Unlock()
		if n := s.RunEviction(); n != 300 {
			t.Fatalf("%s: evicted %d, want the 300 old transmissions", p.name, n)
		}
		obs411CheckTx(t, s, want, p.name+" after eviction")
		acct113Check(t, s, p.name+" after eviction")
		sn := obs411Snap(s)
		if i == 0 {
			ref = sn
		} else if !reflect.DeepEqual(sn, ref) {
			t.Errorf("%s: store after evicting the old half differs from hot Load (trackedBytes %d vs %d)", p.name, sn.TrackedBytes, ref.TrackedBytes)
		}
		s.mu.Lock()
		for _, tx := range s.packets {
			tx.FirstSeen = time.Now().UTC().Add(-100 * time.Hour).Format(time.RFC3339)
		}
		s.mu.Unlock()
		s.RunEviction()
		if s.trackedBytes != 0 || len(s.packets) != 0 {
			t.Errorf("%s: emptied store: trackedBytes=%d packets=%d, want 0/0", p.name, s.trackedBytes, len(s.packets))
		}
	}
}

// The memory budget (packetStore.maxMemoryMB) evicts the same transmissions
// whichever path filled the store.
func TestObsDedup411_MemoryBudgetEvictsTheSameOnEveryPath(t *testing.T) {
	txs := obs411Set()
	want := obs411Expected(txs)
	var ref []string
	for i, p := range obs411PathList() {
		s := p.build(t, txs)
		s.mu.Lock()
		budget := int(s.trackedBytes/2) / 1048576
		if budget < 1 {
			budget = 1
		}
		s.maxMemoryMB = budget
		s.mu.Unlock()
		// A pass evicts at most a quarter of the store.
		evicted := 0
		for pass := 0; pass < 10; pass++ {
			n := s.RunEviction()
			if n == 0 {
				break
			}
			evicted += n
		}
		if evicted == 0 {
			t.Fatalf("%s: a %d MB budget evicted nothing (trackedBytes %d)", p.name, budget, s.trackedBytes)
		}
		acct113Check(t, s, p.name+" after the memory pass")
		obs411CheckTx(t, s, want, p.name+" after the memory pass")
		if s.trackedMemoryMB() > float64(budget) {
			t.Errorf("%s: %.2f MB tracked after the pass, budget %d MB", p.name, s.trackedMemoryMB(), budget)
		}
		got := obs411Hashes(s.packets)
		if i == 0 {
			ref = got
		} else if !reflect.DeepEqual(got, ref) {
			t.Errorf("%s: the memory pass kept %d transmissions, hot Load kept %d (or different ones)", p.name, len(got), len(ref))
		}
	}
}

// Rebuilding the path-hop index reads every observation; it must neither
// change the relay indexes nor the charges, on any path.
func TestObsDedup411_RebuildKeepsIndexesAndCharges(t *testing.T) {
	txs := obs411Set()
	want := obs411Expected(txs)
	for _, p := range obs411PathList() {
		s := p.build(t, txs)
		before := obs411Snap(s)
		s.mu.Lock()
		s.buildPathHopIndex()
		s.mu.Unlock()
		obs411CheckTx(t, s, want, p.name+" after the rebuild")
		acct113Check(t, s, p.name+" after the rebuild")
		if after := obs411Snap(s); !reflect.DeepEqual(after, before) {
			t.Errorf("%s: the rebuild changed the store (trackedBytes %d -> %d, byPathHop %d -> %d keys)", p.name,
				before.TrackedBytes, after.TrackedBytes, len(before.ByPathHop), len(after.ByPathHop))
		}
	}
}

// The in-memory hash merge (#215) moves a duplicate's observations onto the
// survivor through the same dedup: shared pairs are dropped once, the
// survivor's maps appear when the merged list reaches the threshold, and the
// charges of both sides stay exact through the merge and the eviction.
func TestObsDedup411_HashMergeDedupsAcrossTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths int // distinct paths per observer per duplicate row
	}{
		{"merged list stays below", 2}, // 8 per row, 12 merged
		{"merged list crosses", 7}, // 28 per row, 32 merged
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := hm215Create(t)
			base.ballast(t)
			baseline := hm215Open(t, base)
			wantBase := hm215Snap(baseline)

			d := hm215Create(t)
			d.ballast(t)
			raw := hm215Raw(7)
			old := -72 * time.Hour
			keys, observers := map[string]bool{}, map[string]bool{}
			for k := 1; k <= 2; k++ {
				id := 70 + k
				ts := hm215Time(old + time.Duration(k)*time.Second)
				d.tx(t, id, raw, fmt.Sprintf("stale-7-%d", k), ts, 4, hm215Advert(acct113PK(107)))
				for o := 0; o < hm215Observers; o++ {
					for p := 0; p < tc.paths; p++ {
						// Path p of row 2 is path p+1 of row 1: shared pairs.
						path := fmt.Sprintf(`["05","%02x"]`, 40+p+k)
						d.observation(t, id, o, path, fmt.Sprintf(`[%q,%q]`, acct113PK(5), acct113PK(40+p+k)), ts)
						keys[fmt.Sprintf("obs%d|%s", o, path)] = true
						observers[fmt.Sprintf("obs%d", o)] = true
					}
				}
			}
			s := hm215Open(t, d)
			acct113Check(t, s, "before the merge")
			hm215Migrate(s, 1)
			acct113Check(t, s, "after the merge")
			s.mu.RLock()
			w := s.byHash[ComputeContentHash(raw)]
			if w == nil {
				s.mu.RUnlock()
				t.Fatal("no survivor")
			}
			got := map[string]bool{}
			for _, o := range w.Observations {
				got[o.ObserverID+"|"+o.PathJSON] = true
			}
			n, unique, indexed := len(w.Observations), int(w.UniqueObserverCount), w.obsKeys != nil
			s.mu.RUnlock()
			if !reflect.DeepEqual(got, keys) || n != len(keys) {
				t.Errorf("survivor holds %d observations (%d distinct), want the %d distinct pairs", n, len(got), len(keys))
			}
			if unique != len(observers) {
				t.Errorf("survivor UniqueObserverCount = %d, want %d", unique, len(observers))
			}
			if wantIdx := len(keys) >= obsIndexThreshold; indexed != wantIdx {
				t.Errorf("survivor with %d observations: maps present = %v, want %v", n, indexed, wantIdx)
			}
			s.mu.Lock()
			s.retentionHours = 24
			s.mu.Unlock()
			if ev := s.RunEviction(); ev != 1 {
				t.Fatalf("evicted %d, want the merged transmission", ev)
			}
			acct113Check(t, s, "after evicting the merged transmission")
			if gotBase := hm215Snap(s); gotBase.TrackedBytes != wantBase.TrackedBytes {
				t.Errorf("trackedBytes = %d after the eviction, baseline %d", gotBase.TrackedBytes, wantBase.TrackedBytes)
			}
		})
	}
}

// An observation upserted into its existing row (same observer and path)
// keeps its id; the new route bit reaches the store through the change log
// (#89). Neither the observation list nor the dedup state changes, below
// the threshold or past it.
func TestObsDedup411_SameRowRouteUpdateKeepsObservations(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	fs := time.Now().UTC().Add(-time.Hour)
	rmInsertTx(t, db, 2, "busy", 1, 0b0010, fs.Format(time.RFC3339))
	rmInsertObs(t, db, 2, 2, 1, `[]`, "110000", fs.Unix())
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	for i := 0; i < 2; i++ { // "adv": 3 observations, "busy": 41
		rmIngestNewBit(t, db, 1, 0b0010, 1+i, fmt.Sprintf(`["%02x"]`, i), "110000")
	}
	for i := 0; i < 40; i++ {
		rmIngestNewBit(t, db, 2, 0b0010, 1+i%2, fmt.Sprintf(`["%02x"]`, i), "110000")
	}
	for i := 0; i < 3; i++ {
		poll.tick()
	}
	state := func() map[string]obs411TxState { return obs411Snap(s).Tx }
	before := state()
	if before["adv"].Count != 3 || before["busy"].Count != 41 || before["busy"].Unique != 2 {
		t.Fatalf("setup: adv %+v busy %+v", before["adv"].Count, before["busy"].Count)
	}
	if before["adv"].Indexed || !before["busy"].Indexed {
		t.Fatalf("maps present: adv %v (3 observations), busy %v (41)", before["adv"].Indexed, before["busy"].Indexed)
	}
	maxObs := s.MaxObservationID()
	rmIngestNewBit(t, db, 1, 0b1000, 1, `[]`, "130000000000") // same row
	rmIngestNewBit(t, db, 2, 0b1000, 2, `["05"]`, "130000000000")
	for i := 0; i < 2; i++ {
		poll.tick()
	}
	if got := rmSnapshot(s); got["adv"] != (rmView{0b1010, true}) || got["busy"] != (rmView{0b1010, true}) {
		t.Fatalf("route masks after the same-row upserts = %+v, want 1010 for both", got)
	}
	if s.MaxObservationID() != maxObs {
		t.Fatalf("a same-row upsert reached the store as a new observation (max id %d -> %d)", maxObs, s.MaxObservationID())
	}
	if after := state(); !reflect.DeepEqual(after, before) {
		t.Errorf("observations changed with the route bit:\n before %+v\n after  %+v", before, after)
	}
	acct113Check(t, s, "after the same-row upserts")
}

// Below the threshold a transmission carries no maps and a duplicate check
// allocates nothing; at the threshold the maps take over with the same
// answers, unknown observers are never counted, and the charge of the maps
// is exactly obsIndexBytes.
func TestObsDedup411_ThresholdAndAnswers(t *testing.T) {
	tx := &StoreTx{ID: 1, Hash: "h"}
	add := func(observer, path string) (bool, int64) {
		dup, known := tx.lookupObservation(observer, path)
		if dup {
			return false, 0
		}
		return true, tx.addObservation(&StoreObs{ID: len(tx.Observations) + 1, ObserverID: observer, PathJSON: path}, known)
	}
	observer := func(k int) string { return obs411ObserverID(k % 5) }
	var charged int64
	for i := 0; i < obsIndexThreshold-1; i++ {
		ok, d := add(observer(i), fmt.Sprintf(`["%02x"]`, i))
		charged += d
		if !ok {
			t.Fatalf("observation %d was taken for a duplicate", i)
		}
	}
	if tx.obsKeys != nil || tx.observerSet != nil || charged != 0 {
		t.Fatalf("%d observations: maps %v/%v and %d bytes charged, want none", len(tx.Observations), tx.obsKeys != nil, tx.observerSet != nil, charged)
	}
	if ok, _ := add(observer(3), `["03"]`); ok {
		t.Fatal("an exact repeat was kept below the threshold")
	}
	if ok, _ := add("", `["03"]`); !ok {
		t.Fatal("an unknown observer on a known path was taken for a duplicate")
	}
	if tx.UniqueObserverCount != 5 {
		t.Fatalf("UniqueObserverCount = %d, want 5 (the unknown observer is not counted)", tx.UniqueObserverCount)
	}
	if tx.obsKeys == nil || len(tx.obsKeys) != obsIndexThreshold || len(tx.observerSet) != 5 {
		t.Fatalf("at %d observations: maps %d keys / %d observers, want %d / 5", len(tx.Observations), len(tx.obsKeys), len(tx.observerSet), obsIndexThreshold)
	}
	if want := int64(obsIndexRecordBytes + obsIndexThreshold*obsIndexKeyBytes + 5*obsIndexObserverBytes); obsIndexBytes(tx) != want {
		t.Fatalf("obsIndexBytes = %d, want %d", obsIndexBytes(tx), want)
	}
	charged = obsIndexBytes(tx)
	for _, q := range []struct {
		observer, path string
		dup, known     bool
	}{
		{observer(3), `["03"]`, true, true},
		{"", `["03"]`, true, false},
		{observer(3), `["99"]`, false, true},
		{obs411ObserverID(9), `["03"]`, false, false},
	} {
		if dup, known := tx.lookupObservation(q.observer, q.path); dup != q.dup || known != q.known {
			t.Errorf("lookup(%.6s…, %s) = %v/%v, want %v/%v", q.observer, q.path, dup, known, q.dup, q.known)
		}
	}
	ok, d := add(obs411ObserverID(9), `["03"]`)
	charged += d
	if !ok || tx.UniqueObserverCount != 6 || len(tx.observerSet) != 6 || len(tx.obsKeys) != obsIndexThreshold+1 {
		t.Fatalf("past the threshold: kept=%v unique=%d maps %d/%d", ok, tx.UniqueObserverCount, len(tx.obsKeys), len(tx.observerSet))
	}
	if ok, _ := add(obs411ObserverID(9), `["03"]`); ok {
		t.Fatal("an exact repeat was kept past the threshold")
	}
	if charged != obsIndexBytes(tx) {
		t.Fatalf("the returned deltas sum to %d, obsIndexBytes is %d", charged, obsIndexBytes(tx))
	}

	small := &StoreTx{ID: 2}
	for i := 0; i < 8; i++ {
		_, known := small.lookupObservation(observer(i), `["aa"]`)
		small.addObservation(&StoreObs{ObserverID: observer(i), PathJSON: fmt.Sprintf(`["%02x"]`, i)}, known)
	}
	o, p := strings.Clone(observer(2)), `["zz"]`
	if allocs := testing.AllocsPerRun(100, func() { small.lookupObservation(o, p) }); allocs != 0 {
		t.Errorf("a duplicate check on a small transmission allocates %.0f times, want 0", allocs)
	}
}

// The memory estimate charges what is allocated: no per-transmission maps
// and no concatenated key per observation below the threshold.
func TestObsDedup411_EstimateHasNoEagerMapTerms(t *testing.T) {
	if got, want := estimateStoreTxBytes(&StoreTx{}), int64(storeTxBaseBytes+numIndexesPerTx*indexEntryBytes); got != want {
		t.Errorf("estimateStoreTxBytes(empty tx) = %d, want %d (no per-transmission map allowance)", got, want)
	}
	short := &StoreObs{ObserverID: "o", PathJSON: `[]`}
	long := &StoreObs{ObserverID: strings.Repeat("o", 65), PathJSON: `["aa","bb"]`}
	if d := estimateStoreObsBytes(long) - estimateStoreObsBytes(short); d != 64+9 {
		t.Errorf("64 more observer bytes and 9 more path bytes cost %d, want 73 (charged once, no dedup key copy)", d)
	}
}
