package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// Issue #489: the observer ID, name, IATA code and direction of every
// observation come from a SQL join, so every row scan allocated its own copy.
// The store now shares one canonical copy of each value through a bounded
// pool. These tests cover every construction path, the merge, eviction and
// accounting, metadata changes, GC stability and the lock discipline.

func obs489Open(t *testing.T, dbPath string, cfg *PacketStoreConfig) *PacketStore {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	return NewPacketStore(db, cfg)
}

func obs489Ready(t *testing.T, s *PacketStore) *PacketStore {
	t.Helper()
	if !s.WaitIndexesReady(60 * time.Second) {
		t.Fatal("background index builds did not finish")
	}
	return s
}

func obs489IngestAll(s *PacketStore) {
	for since := s.MaxTransmissionID(); ; {
		_, next := s.IngestNewFromDB(since, 97)
		if next <= since {
			return
		}
		since = next
	}
}

func obs489IngestObsAll(s *PacketStore) {
	for since := s.MaxObservationID(); ; {
		s.IngestNewObservations(since, 101)
		next := s.MaxObservationID()
		if next <= since {
			return
		}
		since = next
	}
}

type obs489Path struct {
	name  string
	build func(t *testing.T, txs []obs489Tx) *PacketStore
}

// obs489PathList is every path that constructs a StoreObs: the hot cold load,
// the chunked load, the background fill, live transmission ingest and live
// observation upsert.
func obs489PathList() []obs489Path {
	return []obs489Path{
		{"hot Load", func(t *testing.T, txs []obs489Tx) *PacketStore {
			s := obs489Open(t, obs489DB(t, txs), &PacketStoreConfig{})
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			return obs489Ready(t, s)
		}},
		{"LoadChunked", func(t *testing.T, txs []obs489Tx) *PacketStore {
			s := obs489Open(t, obs489DB(t, txs), &PacketStoreConfig{RetentionHours: 168})
			if err := s.RunStartupLoad(53); err != nil {
				t.Fatal(err)
			}
			return obs489Ready(t, s)
		}},
		{"background fill", func(t *testing.T, txs []obs489Tx) *PacketStore {
			s := obs489Open(t, obs489DB(t, txs), &PacketStoreConfig{RetentionHours: 168, HotStartupHours: 2})
			if err := s.RunStartupLoad(53); err != nil {
				t.Fatal(err)
			}
			return obs489Ready(t, s)
		}},
		{"ingest", func(t *testing.T, txs []obs489Tx) *PacketStore {
			s := obs489Open(t, obs489DB(t, txs), &PacketStoreConfig{})
			obs489IngestAll(s)
			return s
		}},
		{"upsert", func(t *testing.T, txs []obs489Tx) *PacketStore {
			// Each transmission arrives with its first observation; the rest
			// reach the store as observations of transmissions it holds.
			dbPath := filepath.Join(t.TempDir(), "obs489.db")
			conn := obs489Create(t, dbPath)
			defer conn.Close()
			obs489Write(t, conn, txs, true, func(j int) bool { return j == 0 })
			s := obs489Open(t, dbPath, &PacketStoreConfig{})
			obs489IngestAll(s)
			obs489Write(t, conn, txs, false, func(j int) bool { return j > 0 })
			obs489IngestObsAll(s)
			return s
		}},
	}
}

// obs489Set is a set small enough for a unit test but wide enough to repeat
// every observer many times and to push a few transmissions past the #411
// dedup threshold.
func obs489Set() []obs489Tx { return obs489Data(4000, 489) }

// obs489Sharing walks the store and reports, per metadata field, how many
// distinct values it holds and how many distinct copies of them, plus the
// values themselves.
type obs489Sharing struct {
	obs    int
	values map[string]map[string]int // field → value → number of distinct copies
}

func obs489SharingOf(s *PacketStore) obs489Sharing {
	out := obs489Sharing{values: map[string]map[string]int{}}
	ptrs := map[string]map[string]map[uintptr]bool{}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, tx := range s.packets {
		for _, o := range tx.Observations {
			out.obs++
			for field, v := range map[string]string{
				"observerID": o.ObserverID, "observerName": o.ObserverName,
				"observerIATA": o.ObserverIATA, "direction": o.Direction,
			} {
				if v == "" {
					continue
				}
				if ptrs[field] == nil {
					ptrs[field] = map[string]map[uintptr]bool{}
				}
				if ptrs[field][v] == nil {
					ptrs[field][v] = map[uintptr]bool{}
				}
				ptrs[field][v][stringDataPtr(v)] = true
			}
		}
	}
	for field, byValue := range ptrs {
		out.values[field] = map[string]int{}
		for v, set := range byValue {
			out.values[field][v] = len(set)
		}
	}
	return out
}

// obs489WantValues is the set of non-empty metadata values the generator
// writes, per field.
func obs489WantValues(txs []obs489Tx) map[string]map[string]bool {
	out := map[string]map[string]bool{
		"observerID": {}, "observerName": {}, "observerIATA": {}, "direction": {},
	}
	for _, tx := range txs {
		for _, r := range tx.rows {
			add := func(field, v string) {
				if v != "" {
					out[field][v] = true
				}
			}
			add("observerID", obs489ObserverID(r.observer))
			add("observerName", obs489ObserverName(r.observer))
			add("observerIATA", obs489IATA(r.observer))
			add("direction", r.dir)
		}
	}
	return out
}

// obs489CheckShared asserts that every observation carries the value its row
// wrote and that the store holds exactly one copy of each distinct value.
func obs489CheckShared(t *testing.T, s *PacketStore, want map[string]map[string]bool, where string) {
	t.Helper()
	got := obs489SharingOf(s)
	for field, wantValues := range want {
		gotValues := got.values[field]
		if len(gotValues) != len(wantValues) {
			t.Errorf("%s: %s holds %d distinct values, want %d", where, field, len(gotValues), len(wantValues))
		}
		for v, copies := range gotValues {
			if !wantValues[v] {
				t.Errorf("%s: %s holds unexpected value %q", where, field, v)
			}
			if copies != 1 {
				t.Errorf("%s: %s value %q is held in %d separate copies, want 1 shared copy", where, field, v, copies)
			}
		}
	}
	// The pool holds exactly the distinct non-empty values, no more.
	distinct := map[string]bool{}
	for _, values := range want {
		for v := range values {
			distinct[v] = true
		}
	}
	if n := s.obsStrPool.entries(); n != len(distinct) {
		t.Errorf("%s: the pool holds %d entries for %d distinct values", where, n, len(distinct))
	}
}

func TestObsStringsSharedOnEveryConstructionPath_489(t *testing.T) {
	txs := obs489Set()
	want := obs489WantValues(txs)
	if len(want["observerID"]) != obs489Observers {
		t.Fatalf("setup: %d distinct observer IDs, want %d", len(want["observerID"]), obs489Observers)
	}
	// The background-fill path only exercises loadChunk when some rows sit
	// outside its 2h hot window and some inside it.
	old := 0
	for _, tx := range txs {
		if time.Since(tx.firstSeen) > 2*time.Hour {
			old++
		}
	}
	if old == 0 || old == len(txs) {
		t.Fatalf("setup: %d of %d transmissions lie outside the 2h hot window", old, len(txs))
	}
	type ref struct {
		tracked  int64
		pool     int64
		totalObs int
		keys     map[string][]string
	}
	var first ref
	for i, p := range obs489PathList() {
		s := p.build(t, txs)
		if got := obs489SharingOf(s).obs; got == 0 {
			t.Fatalf("%s: store holds no observations", p.name)
		}
		obs489CheckShared(t, s, want, p.name)
		acct113Check(t, s, p.name)

		// Every observation's charge must exclude the shared bytes.
		s.mu.RLock()
		keys := map[string][]string{}
		for _, tx := range s.packets {
			for _, o := range tx.Observations {
				if got := ownedObsStringBytes(o); got != 0 {
					t.Errorf("%s: observation %d is charged %d B for metadata the pool holds", p.name, o.ID, got)
				}
				keys[tx.Hash] = append(keys[tx.Hash], fmt.Sprintf("%s|%s|%s|%s|%s",
					o.ObserverID, o.ObserverName, o.ObserverIATA, o.Direction, o.PathJSON))
			}
			sort.Strings(keys[tx.Hash])
		}
		cur := ref{tracked: s.trackedBytes, pool: s.obsStrPool.retainedBytes(), totalObs: s.totalObs, keys: keys}
		s.mu.RUnlock()

		if cur.pool <= 0 {
			t.Errorf("%s: the pool reports no retention", p.name)
		}
		if i == 0 {
			first = cur
			continue
		}
		if !reflect.DeepEqual(cur.keys, first.keys) {
			t.Errorf("%s: the observations differ from hot Load", p.name)
		}
		if cur.tracked != first.tracked || cur.totalObs != first.totalObs || cur.pool != first.pool {
			t.Errorf("%s: trackedBytes/totalObs/pool = %d/%d/%d, hot Load %d/%d/%d",
				p.name, cur.tracked, cur.totalObs, cur.pool, first.tracked, first.totalObs, first.pool)
		}
	}
}

// trackedBytes must hold the actual retention, not one logical copy of the
// metadata per observation, and the pool's own retention must be accounted
// for on top of it.
func TestObsStringsTrackedBytesHoldsSharedBytesOnce_489(t *testing.T) {
	txs := obs489Set()
	s := obs489Open(t, obs489DB(t, txs), &PacketStoreConfig{})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	obs489Ready(t, s)

	s.mu.RLock()
	defer s.mu.RUnlock()
	// What the same store would be charged if every observation owned its
	// own copy of the four fields, i.e. the charge before #489.
	var logical int64
	obsCount := 0
	for _, tx := range s.packets {
		for _, o := range tx.Observations {
			obsCount++
			logical += int64(len(o.ObserverID) + len(o.ObserverName) + len(o.ObserverIATA) + len(o.Direction))
		}
	}
	if obsCount < 1000 {
		t.Fatalf("setup: only %d observations", obsCount)
	}
	if logical < 1<<17 {
		t.Fatalf("setup: only %d B of logical metadata; the test would not show the difference", logical)
	}
	// What trackedBytes really holds for the metadata: only the bytes the
	// observations own, i.e. nothing the pool holds for them.
	var owned int64
	for _, tx := range s.packets {
		for _, o := range tx.Observations {
			owned += ownedObsStringBytes(o)
		}
	}
	if owned*10 > logical {
		t.Errorf("observations are charged %d B of the %d B of metadata; the shared bytes are still charged per observation", owned, logical)
	}
	pool := s.obsStrPool.retainedBytes()
	if pool <= 0 {
		t.Fatal("the pool reports no retention of its own, so nothing is accounted for the shared copies")
	}
	if pool*10 > logical {
		t.Errorf("the pool retains %d B against %d B logical: the sharing is barely saving anything", pool, logical)
	}
	if got := s.accountedBytes(); got != s.trackedBytes+pool {
		t.Fatalf("accountedBytes = %d, want trackedBytes %d + pool %d", got, s.trackedBytes, pool)
	}
	if want := float64(s.accountedBytes()) / 1048576.0; s.trackedMemoryMB() != want {
		t.Fatalf("trackedMemoryMB = %f, want %f (the pool must be in the reported figure)", s.trackedMemoryMB(), want)
	}
	t.Logf("trackedBytes=%d pool=%d owned metadata=%d logical metadata=%d (saved %d B of charge)",
		s.trackedBytes, pool, owned, logical, logical-owned-pool)
}

// Eviction credits exactly what ingest charged: trackedBytes returns to zero
// and only the pool's bounded retention is left standing.
func TestObsStringsEvictionCreditsExactly_489(t *testing.T) {
	txs := obs489Data(2000, 4890)
	for _, p := range obs489PathList() {
		t.Run(p.name, func(t *testing.T) {
			s := p.build(t, txs)
			acct113Check(t, s, "after the fill")
			pool := s.obsStrPool.retainedBytes()
			if pool <= 0 {
				t.Fatal("setup: the pool reports no retention")
			}
			s.mu.Lock()
			s.retentionHours = 0.5 // every row is at least an hour old
			s.mu.Unlock()
			for i := 0; i < 40 && len(s.packets) > 0; i++ {
				if s.RunEviction() == 0 {
					break
				}
			}
			s.mu.RLock()
			defer s.mu.RUnlock()
			if len(s.packets) != 0 {
				t.Fatalf("%d transmissions survived eviction", len(s.packets))
			}
			if s.trackedBytes != 0 {
				t.Errorf("trackedBytes = %d after evicting everything, want 0", s.trackedBytes)
			}
			if got := s.obsStrPool.retainedBytes(); got != pool {
				t.Errorf("pool retention changed to %d on eviction, want the %d it held (the canonical copies outlive the observations)", got, pool)
			}
			if got := s.accountedBytes(); got != pool {
				t.Errorf("accountedBytes = %d with an empty store, want the pool's %d", got, pool)
			}
		})
	}
}

// A renamed observer is a new value: it gets its own entry, and the
// observations heard before the rename keep the name they were heard with.
func TestObsStringsMetadataChangeLeavesOlderObservations_489(t *testing.T) {
	txs := obs489Data(300, 4891)
	dbPath := filepath.Join(t.TempDir(), "obs489.db")
	conn := obs489Create(t, dbPath)
	defer conn.Close()
	obs489Write(t, conn, txs, true, nil)

	s := obs489Open(t, dbPath, &PacketStoreConfig{})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	obs489Ready(t, s)

	oldName := obs489ObserverName(0)
	before := map[int]string{}
	var oldCopy string
	s.mu.RLock()
	for _, o := range s.byObsID {
		if o.ObserverName == oldName {
			before[o.ID] = o.ObserverName
			oldCopy = o.ObserverName
		}
	}
	s.mu.RUnlock()
	if len(before) == 0 {
		t.Fatal("setup: no observation carries observer 0's name")
	}
	entriesBefore := s.obsStrPool.entries()

	// The ingestor renames observer 0 and a new observation arrives. The new
	// name shares a prefix with the old one, so a pool that matched values
	// loosely instead of exactly would hand the old copy back here.
	const newName = "Nord Repeater 00 (renamed)"
	if _, err := conn.Exec(`UPDATE observers SET name = ? WHERE rowid = 1`, newName); err != nil {
		t.Fatal(err)
	}
	nextObs := 0
	for _, tx := range txs {
		for _, r := range tx.rows {
			if r.id > nextObs {
				nextObs = r.id
			}
		}
	}
	nextObs++
	if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp, resolved_path)
		VALUES (?, ?, 1, 'RX', -5.0, -90.0, 5, '["zz"]', ?, NULL)`,
		nextObs, txs[0].id, time.Now().UTC().Unix()); err != nil {
		t.Fatal(err)
	}
	obs489IngestObsAll(s)

	s.mu.RLock()
	defer s.mu.RUnlock()
	fresh := s.byObsID[nextObs]
	if fresh == nil {
		t.Fatal("the new observation was not ingested")
	}
	if fresh.ObserverName != newName {
		t.Errorf("new observation name = %q, want %q", fresh.ObserverName, newName)
	}
	if fresh.sharedStrs&obsSharedObserverName == 0 {
		t.Error("the new name was not shared")
	}
	for id, want := range before {
		o := s.byObsID[id]
		if o == nil {
			t.Fatalf("observation %d disappeared", id)
		}
		if o.ObserverName != want {
			t.Errorf("observation %d name changed to %q, want the %q it was heard with", id, o.ObserverName, want)
		}
		if !sameCopy(o.ObserverName, oldCopy) {
			t.Errorf("observation %d no longer shares the old name's copy", id)
		}
	}
	if sameCopy(fresh.ObserverName, oldCopy) {
		t.Error("the renamed observer shares the old name's copy")
	}
	if got := s.obsStrPool.entries(); got != entriesBefore+1 {
		t.Errorf("pool holds %d entries after one rename, want %d (the old name stays for the older observations)", got, entriesBefore+1)
	}
	acct113Check(t, s, "after the rename")
}

// The sharing must survive garbage collections: an observation ingested after
// a GC shares the copies the cold load put in the pool.
func TestObsStringsSharingSurvivesGC_489(t *testing.T) {
	txs := obs489Data(300, 4892)
	dbPath := filepath.Join(t.TempDir(), "obs489.db")
	conn := obs489Create(t, dbPath)
	defer conn.Close()
	obs489Write(t, conn, txs, true, nil)
	s := obs489Open(t, dbPath, &PacketStoreConfig{})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	obs489Ready(t, s)

	var loaded *StoreObs
	s.mu.RLock()
	for _, o := range s.byObsID {
		if o.ObserverName == obs489ObserverName(0) && o.Direction == "RX" {
			loaded = o
			break
		}
	}
	s.mu.RUnlock()
	if loaded == nil {
		t.Fatal("setup: no RX observation by observer 0")
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
	}

	nextObs := 0
	for _, tx := range txs {
		for _, r := range tx.rows {
			if r.id > nextObs {
				nextObs = r.id
			}
		}
	}
	nextObs++
	if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp, resolved_path)
		VALUES (?, ?, 1, 'RX', -5.0, -90.0, 5, '["yy"]', ?, NULL)`,
		nextObs, txs[0].id, time.Now().UTC().Unix()); err != nil {
		t.Fatal(err)
	}
	obs489IngestObsAll(s)

	s.mu.RLock()
	defer s.mu.RUnlock()
	fresh := s.byObsID[nextObs]
	if fresh == nil {
		t.Fatal("the new observation was not ingested")
	}
	for _, f := range []struct {
		name        string
		got, loaded string
	}{
		{"observer ID", fresh.ObserverID, loaded.ObserverID},
		{"observer name", fresh.ObserverName, loaded.ObserverName},
		{"IATA", fresh.ObserverIATA, loaded.ObserverIATA},
		{"direction", fresh.Direction, loaded.Direction},
	} {
		if f.got != f.loaded {
			t.Fatalf("setup: %s differs (%q vs %q)", f.name, f.got, f.loaded)
		}
		if !sameCopy(f.got, f.loaded) {
			t.Errorf("%s: the observation ingested after the GC holds a second copy", f.name)
		}
	}
}

// The hash merge (#215) moves observations between transmissions. They keep
// their canonical copies and their charges, and a dropped duplicate is
// credited exactly.
func TestObsStringsHashMergeKeepsSharingAndCharges_489(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 3, 3, 3)
	s := hm215Open(t, d)

	before := map[string]uintptr{}
	s.mu.RLock()
	for _, tx := range s.packets {
		for _, o := range tx.Observations {
			before[o.ObserverID] = stringDataPtr(o.ObserverID)
			if o.sharedStrs&(obsSharedObserverID|obsSharedObserverName|obsSharedDirection) !=
				obsSharedObserverID|obsSharedObserverName|obsSharedDirection {
				t.Fatalf("setup: observation %d carries unshared metadata (mask %#b)", o.ID, o.sharedStrs)
			}
		}
	}
	s.mu.RUnlock()
	if len(before) == 0 {
		t.Fatal("setup: no observations")
	}
	poolBefore := s.obsStrPool.retainedBytes()

	hm215Migrate(s, 2)
	acct113Check(t, s, "after the merge")

	s.mu.RLock()
	defer s.mu.RUnlock()
	moved := 0
	for _, tx := range s.packets {
		for _, o := range tx.Observations {
			moved++
			if got := stringDataPtr(o.ObserverID); got != before[o.ObserverID] {
				t.Errorf("observation %d holds a new copy of %q after the merge", o.ID, o.ObserverID)
			}
			if got := ownedObsStringBytes(o); got != 0 {
				t.Errorf("observation %d is charged %d B for shared metadata after the merge", o.ID, got)
			}
		}
	}
	if moved == 0 {
		t.Fatal("no observations survived the merge")
	}
	if got := s.obsStrPool.retainedBytes(); got != poolBefore {
		t.Errorf("the merge changed the pool's retention from %d to %d", poolBefore, got)
	}
}

// A full pool must not break ingest: the observations are correct, unshared,
// and charged for the bytes they then own.
func TestObsStringsSaturatedPoolStillIngests_489(t *testing.T) {
	txs := obs489Data(600, 4893)
	s := obs489Open(t, obs489DB(t, txs), &PacketStoreConfig{})
	// Saturate before the load, so every observation takes the fallback.
	for i := 0; i < obsStringPoolMax; i++ {
		s.obsStrPool.intern(fmt.Sprintf("filler-%07d", i))
	}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	obs489Ready(t, s)

	want := obs489WantValues(txs)
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	charged := int64(0)
	for _, tx := range s.packets {
		for _, o := range tx.Observations {
			if !want["observerID"][o.ObserverID] {
				t.Fatalf("observation %d carries observer ID %q, which no row wrote", o.ID, o.ObserverID)
			}
			if o.sharedStrs != 0 {
				t.Fatalf("observation %d reports shared metadata (%#b) though the pool was full", o.ID, o.sharedStrs)
			}
			seen[o.ObserverID] = true
			charged += ownedObsStringBytes(o)
		}
	}
	if len(seen) != obs489Observers {
		t.Errorf("%d distinct observers survived a full pool, want %d", len(seen), obs489Observers)
	}
	if charged == 0 {
		t.Error("a full pool charged nothing for the bytes the observations own")
	}
	if s.obsStrPool.entries() != obsStringPoolMax {
		t.Errorf("pool holds %d entries, want the cap %d", s.obsStrPool.entries(), obsStringPoolMax)
	}
	acct113Check(t, s, "after a load through a full pool")
}

// Lock discipline (#495): the live paths intern while scanning rows, so a
// goroutine parked inside the pool does not hold s.mu. A store writer must
// get the lock while an ingest is stuck on a pool miss.
func TestObsStringPoolNeverBlocksStoreWriters_489(t *testing.T) {
	txs := obs489Data(200, 4894)
	dbPath := filepath.Join(t.TempDir(), "obs489.db")
	conn := obs489Create(t, dbPath)
	defer conn.Close()
	obs489Write(t, conn, txs, true, nil)
	s := obs489Open(t, dbPath, &PacketStoreConfig{})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	obs489Ready(t, s)

	nextTx := len(txs) + 1
	nextObs := obs489MaxObsID(txs) + 1
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		add  func(t *testing.T)
		run  func()
	}{
		{"IngestNewFromDB", func(t *testing.T) {
			raw := fmt.Sprintf("1500%036d", nextTx)
			if _, err := conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
				VALUES (?, ?, ?, ?, 1, 5, 1, '{"type":"GRP_TXT"}')`,
				nextTx, raw, fmt.Sprintf("fresh-%d", nextTx), now.Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp, resolved_path)
				VALUES (?, ?, 1, 'RX', -5.0, -90.0, 5, '["aa"]', ?, NULL)`,
				nextObs, nextTx, now.Unix()); err != nil {
				t.Fatal(err)
			}
			nextTx++
			nextObs++
		}, func() { obs489IngestAll(s) }},
		{"IngestNewObservations", func(t *testing.T) {
			if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp, resolved_path)
				VALUES (?, ?, 1, 'RX', -5.0, -90.0, 5, '["bb"]', ?, NULL)`,
				nextObs, txs[0].id, now.Unix()); err != nil {
				t.Fatal(err)
			}
			nextObs++
		}, func() { obs489IngestObsAll(s) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A value the pool does not hold yet, so the ingest must take the
			// pool's write lock to intern it.
			if _, err := conn.Exec(`UPDATE observers SET name = ? WHERE rowid = 1`,
				"Fresh Name "+tc.name); err != nil {
				t.Fatal(err)
			}
			tc.add(t)

			// Park every would-be interner inside the pool.
			for i := range s.obsStrPool.shards {
				s.obsStrPool.shards[i].mu.Lock()
			}
			unpark := func() {
				for i := range s.obsStrPool.shards {
					s.obsStrPool.shards[i].mu.Unlock()
				}
			}
			done := make(chan struct{})
			go func() { defer close(done); tc.run() }()

			// The ingest is now either finished or stuck in the pool. Either
			// way it must not be holding s.mu: a writer gets the lock.
			locked := make(chan struct{})
			go func() {
				s.mu.Lock()
				s.mu.Unlock()
				close(locked)
			}()
			select {
			case <-locked:
			case <-time.After(10 * time.Second):
				unpark()
				t.Fatal("a store writer waited on s.mu while an ingest was parked in the sharing pool: the pool is being used under the store lock")
			}
			unpark()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the ingest did not finish after the pool was released")
			}
		})
	}
	acct113Check(t, s, "after the parked ingests")
}

// Concurrency: live ingest, eviction and readers at once, under -race.
func TestObsStringsConcurrentIngestAndReaders_489(t *testing.T) {
	txs := obs489Data(1500, 4895)
	dbPath := filepath.Join(t.TempDir(), "obs489.db")
	conn := obs489Create(t, dbPath)
	defer conn.Close()
	obs489Write(t, conn, txs, true, func(j int) bool { return j == 0 })
	s := obs489Open(t, dbPath, &PacketStoreConfig{})
	obs489IngestAll(s)
	obs489Write(t, conn, txs, false, func(j int) bool { return j > 0 })

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.mu.RLock()
				for _, tx := range s.packets {
					for _, o := range tx.Observations {
						_ = o.ObserverID + o.ObserverName + o.ObserverIATA + o.Direction
						_ = ownedObsStringBytes(o)
					}
				}
				s.mu.RUnlock()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.obsStrPool.internObs(obs489ObserverID(3), "Concurrent Name", "ZZZ", "RX")
	}()
	obs489IngestObsAll(s)
	close(stop)
	wg.Wait()

	want := obs489WantValues(txs)
	obs489CheckSharedAllowingExtra(t, s, want)
	acct113Check(t, s, "after the concurrent run")
}

// obs489CheckSharedAllowingExtra is obs489CheckShared without the pool-entry
// count, for a store whose pool a test also interned into directly.
func obs489CheckSharedAllowingExtra(t *testing.T, s *PacketStore, want map[string]map[string]bool) {
	t.Helper()
	got := obs489SharingOf(s)
	for field, wantValues := range want {
		for v, copies := range got.values[field] {
			if !wantValues[v] {
				t.Errorf("%s holds unexpected value %q", field, v)
			}
			if copies != 1 {
				t.Errorf("%s value %q is held in %d separate copies, want 1", field, v, copies)
			}
		}
	}
}
