package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #621 (port of upstream PR 2327): an evicted transmission must not stay
// reachable from any secondary index. Eviction used to rebuild the byNode keys
// of a transmission from its decoded pubkeys, the path_json fallback record
// and a SQL prefetch of resolved_path. The prefetch is not the record of what
// was indexed: the ingestor prunes the rows on its own schedule, and its
// upsert can rewrite resolved_path after the server indexed the old value. The
// memberships it misses keep the transmission, its observations and its
// decoded JSON alive after its bytes were credited.

func ev621PK(tag string) string { return tag + strings.Repeat("62", 32-len(tag)/2) }

var (
	ev621Relay     = ev621PK("e621")   // relays every transmission
	ev621OldRelay  = ev621PK("e622")   // relays only the old one
	ev621Rewritten = ev621PK("e623")   // what the rewritten resolved_path names
	ev621Sender    = ev621PK("5e4d01") // advertises
)

type ev621DB struct {
	t  *testing.T
	db *DB
}

func (d ev621DB) exec(q string, args ...interface{}) {
	d.t.Helper()
	if _, err := d.db.conn.Exec(q, args...); err != nil {
		d.t.Fatalf("%v\n%s", err, q)
	}
}

// tx inserts transmission id, first seen at at, heard by observer 1 through
// the given resolved relays.
func (d ev621DB) tx(id int, at time.Time, relays ...string) {
	d.t.Helper()
	ts := at.UTC().Format(time.RFC3339)
	d.exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES (?, 'AA00', ?, ?, 1, 4, ?)`,
		id, fmt.Sprintf("%016x", 0x6210000+id), ts, `{"type":"ADVERT","pubKey":"`+ev621Sender+`"}`)
	hops := make([]string, len(relays))
	for i, r := range relays {
		hops[i] = `"` + r[:2] + `"`
	}
	d.exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path) VALUES (?, ?, 1, 5, -90, ?, ?, ?)`,
		id, id, "["+strings.Join(hops, ",")+"]", at.Unix(), `["`+strings.Join(relays, `","`)+`"]`)
}

func newEv621DB(t *testing.T) ev621DB {
	d := ev621DB{t: t, db: setupTestDB(t)}
	t.Cleanup(func() { d.db.Close() })
	ts := time.Now().UTC().Format(time.RFC3339)
	d.exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('obs621', 'O', 'CPH', ?, ?, 1)`, ts, ts)
	return d
}

// ev621Refs counts the references the store's secondary indexes hold to tx,
// including the vacated slots of their backing arrays. Must hold s.mu.
func ev621Refs(s *PacketStore, tx *StoreTx) (refs []string) {
	short := func(k string) string { return k[:min(8, len(k))] }
	inSlice := func(name string, list []*StoreTx) {
		for i, t := range list[:cap(list)] {
			if t == tx {
				refs = append(refs, fmt.Sprintf("%s[%d] (len %d)", name, i, len(list)))
			}
		}
	}
	inSlice("packets", s.packets)
	for k, list := range s.byNode {
		inSlice("byNode["+short(k)+"]", list)
	}
	for k, list := range s.byPayloadType {
		inSlice(fmt.Sprintf("byPayloadType[%d]", k), list)
	}
	for k, list := range s.byPathHop {
		inSlice("byPathHop["+short(k)+"]", list)
	}
	for k, set := range s.nodeTxIDs {
		if set.has(tx) {
			refs = append(refs, "nodeTxIDs["+short(k)+"]")
		}
	}
	s.groupedCacheMu.Lock()
	inSlice("groupedCacheTxs", s.groupedCacheTxs)
	s.groupedCacheMu.Unlock()
	return refs
}

// The relays of an evicted transmission are released however the database
// row has changed since the server indexed it.
func TestEvictReleasesResolvedRelayMemberships_621(t *testing.T) {
	cases := []struct {
		name   string
		change func(d ev621DB)
	}{
		{"rows pruned by the ingestor first", func(d ev621DB) {
			d.exec(`DELETE FROM observations WHERE transmission_id = 1`)
			d.exec(`DELETE FROM transmissions WHERE id = 1`)
		}},
		{"resolved_path rewritten by the ingestor's upsert", func(d ev621DB) {
			d.exec(`UPDATE observations SET resolved_path = ? WHERE transmission_id = 1`, `["`+ev621Rewritten+`"]`)
		}},
		{"rows unchanged", func(ev621DB) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newEv621DB(t)
			now := time.Now().UTC()
			d.tx(1, now.Add(-48*time.Hour), ev621Relay, ev621OldRelay)
			d.tx(2, now.Add(-time.Hour), ev621Relay)
			s := NewPacketStore(d.db, &PacketStoreConfig{RetentionHours: 72})
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			s.WaitIndexesReady(10 * time.Second)
			s.mu.Lock()
			old, young := s.byTxID[1], s.byTxID[2]
			if old == nil || young == nil || len(s.byNode[ev621OldRelay]) != 1 || len(s.byNode[ev621Relay]) != 2 {
				s.mu.Unlock()
				t.Fatalf("fixture: relays not indexed: %v / %v", s.byNode[ev621OldRelay], s.byNode[ev621Relay])
			}
			s.retentionHours = 24
			s.mu.Unlock()

			c.change(d)
			if n := s.RunEviction(); n != 1 {
				t.Fatalf("evicted %d, want 1", n)
			}

			s.mu.RLock()
			refs := ev621Refs(s, old)
			_, oldBucket := s.byNode[ev621OldRelay]
			_, oldSet := s.nodeTxIDs[ev621OldRelay]
			kept := s.byNode[ev621Relay]
			s.mu.RUnlock()
			if len(refs) > 0 {
				t.Errorf("evicted tx still referenced from %v", refs)
			}
			if oldBucket || oldSet {
				t.Errorf("relay heard only by the evicted tx kept its byNode bucket (%v) or de-dup set (%v)", oldBucket, oldSet)
			}
			if len(kept) != 1 || kept[0] != young {
				t.Errorf("shared relay lists %d tx, want only the live one", len(kept))
			}
			bn534AssertConsistent(t, s, "after eviction")
		})
	}
}

// The slices eviction filters keep no evicted pointer in the slots past their
// new length, where the GC still sees them until an append overwrites them.
func TestEvictClearsVacatedSliceTails_621(t *testing.T) {
	now := time.Now().UTC()
	s := makeTestStore(4, now.Add(-48*time.Hour), 0)
	s.packets[3].FirstSeen = now.Add(-time.Hour).Format(time.RFC3339)
	s.retentionHours = 24
	for _, tx := range s.packets {
		s.addToByNode(tx, ev621Relay)
	}
	evicted := append([]*StoreTx(nil), s.packets[:3]...)
	evictedObs := map[*StoreObs]bool{}
	for _, tx := range evicted {
		for _, o := range tx.Observations {
			evictedObs[o] = true
		}
	}

	if n := s.EvictStale(); n != 3 {
		t.Fatalf("evicted %d, want 3", n)
	}
	for _, tx := range evicted {
		if refs := ev621Refs(s, tx); len(refs) > 0 {
			t.Errorf("evicted tx %d still referenced from %v", tx.ID, refs)
		}
	}
	for id, list := range s.byObserver {
		for i, o := range list[:cap(list)] {
			if evictedObs[o] {
				t.Errorf("byObserver[%s] keeps evicted observation %d in slot %d (len %d)", id, o.ID, i, len(list))
			}
		}
	}
}

// Eviction drops the grouped-query cache, which holds the transmissions of
// the last result, and a sort that started before an eviction does not
// publish what that eviction removed.
func TestEvictReleasesGroupedCache_621(t *testing.T) {
	now := time.Now().UTC()
	s := makeTestStore(4, now.Add(-48*time.Hour), 0)
	s.packets[3].FirstSeen = now.Add(-time.Hour).Format(time.RFC3339)
	s.retentionHours = 24
	if r := s.QueryGroupedPackets(PacketQuery{Limit: 10}); r.Total != 4 {
		t.Fatalf("grouped total %d, want 4", r.Total)
	}
	s.mu.Lock()
	n := s.EvictStale()
	s.mu.Unlock()
	if n != 3 {
		t.Fatalf("evicted %d, want 3", n)
	}
	s.groupedCacheMu.Lock()
	cached := len(s.groupedCacheTxs)
	s.groupedCacheMu.Unlock()
	if cached != 0 {
		t.Errorf("grouped cache still holds %d transmissions after eviction", cached)
	}

	// A sorted result snapshotted before an eviction is not published.
	stale := []*StoreTx{{ID: 99, Hash: "stale"}}
	before := atomic.LoadInt64(&s.evicted)
	s.mu.Lock()
	s.packets[0].FirstSeen = now.Add(-48 * time.Hour).Format(time.RFC3339)
	s.EvictStale()
	s.mu.Unlock()
	s.cacheGroupedPackets(stale, "k", before)
	s.groupedCacheMu.Lock()
	published := s.groupedCacheTxs != nil
	s.groupedCacheMu.Unlock()
	if published {
		t.Error("a grouped result sorted before an eviction was published after it")
	}
	s.cacheGroupedPackets(stale, "k", atomic.LoadInt64(&s.evicted))
	s.groupedCacheMu.Lock()
	published = s.groupedCacheTxs != nil
	s.groupedCacheMu.Unlock()
	if !published {
		t.Error("a grouped result with no eviction since its snapshot was not published")
	}
}

// Long-run retention, the issue's measurement: generations of transmissions
// ingested through the real paths, each evicted after the ingestor pruned its
// rows, with the packets page querying in between. After a GC no evicted
// transmission may still be reachable.
func TestEvictionLongRunRetainsNoEvictedTx_621(t *testing.T) {
	const gens, perGen = 6, 200
	d := newEv621DB(t)
	now := time.Now().UTC()
	at := func(g int) time.Time { return now.Add(-time.Duration(gens-g) * time.Hour) }
	s := NewPacketStore(d.db, &PacketStoreConfig{RetentionHours: 1000})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	s.WaitIndexesReady(10 * time.Second)

	var evicted int
	var collected atomic.Int64
	lastID := 0
	for g := 0; g < gens; g++ {
		genRelay := ev621PK(fmt.Sprintf("%02x%02x", 0x70+g, g))
		for i := 0; i < perGen; i++ {
			d.tx(lastID+1+i, at(g), ev621Relay, genRelay)
		}
		s.IngestNewFromDB(lastID, perGen*2)
		lastID += perGen
		s.QueryGroupedPackets(PacketQuery{Limit: 50})
		if g == 0 {
			continue
		}
		// The ingestor pruned the previous generation's rows first.
		d.exec(`DELETE FROM observations WHERE transmission_id <= ?`, lastID-perGen)
		d.exec(`DELETE FROM transmissions WHERE id <= ?`, lastID-perGen)
		s.mu.Lock()
		for _, tx := range s.packets {
			if tx.FirstSeen < at(g).Format(time.RFC3339) {
				evicted++
				runtime.SetFinalizer(tx, func(*StoreTx) { collected.Add(1) })
			}
		}
		s.retentionHours = float64(gens-g) + 0.5
		s.mu.Unlock()
		if n := s.RunEviction(); n != perGen {
			t.Fatalf("generation %d: evicted %d, want %d", g, n, perGen)
		}
	}

	// Finalizers run after the GC that found the object unreachable.
	for i := 0; i < 10 && int(collected.Load()) < evicted; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	reachable := evicted - int(collected.Load())
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("evicted %d, still reachable %d, live %d, heap %.1f MiB", evicted, reachable, len(s.packets), float64(ms.HeapAlloc)/1048576)
	if reachable != 0 {
		t.Errorf("%d of %d evicted transmissions are still reachable after GC", reachable, evicted)
	}
	runtime.KeepAlive(s)
}

// The in-memory content-hash migration recovers a panic so the server keeps
// serving; it must not leave s.mu held when it does, or every reader and the
// next eviction block for good (upstream PR 2327: "releases locks reliably").
func TestHashMigrateReleasesLockOnPanic_621(t *testing.T) {
	cases := []struct {
		name   string
		inject func(s *PacketStore)
	}{
		// a nil transmission panics while the batch is read under RLock
		{"read batch", func(s *PacketStore) { s.packets[1] = nil }},
		// a nil byHash panics while the batch is applied under Lock
		{"apply batch", func(s *PacketStore) { s.byHash = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeTestStore(2, time.Now().UTC(), 0)
			s.pathHopReady.Store(true)
			s.packets[0].RawHex = strings.Repeat("z", 32) // hashes to something else than hash0000
			c.inject(s)
			done := make(chan struct{})
			go func() {
				captureLog(func() { migrateContentHashesAsync(s, 10, 0) })
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("migration did not return")
			}
			if !s.mu.TryLock() {
				t.Fatal("s.mu is still held after the migration recovered a panic")
			}
			s.mu.Unlock()
			if !s.hashMigrationComplete.Load() {
				t.Error("migration did not report completion")
			}
		})
	}
}

// Pinning (upstream PR 2327, "cannot republish evicted packets"): a batch the
// migration chose before an eviction does not put the evicted transmission
// back into byHash when it is applied after it. Here the batch is re-checked
// under the write lock (applyContentHashUpdates), so this holds already.
func TestHashMigrateDoesNotRepublishEvicted_621(t *testing.T) {
	now := time.Now().UTC()
	s := makeTestStore(2, now.Add(-48*time.Hour), 0)
	s.packets[1].FirstSeen = now.Add(-time.Hour).Format(time.RFC3339)
	s.retentionHours = 24
	old := s.packets[0]
	old.RawHex = strings.Repeat("z", 32)
	s.mu.RLock()
	updates := staleContentHashes(s, s.packets)
	s.mu.RUnlock()
	if len(updates) != 1 {
		t.Fatalf("fixture: %d stale hashes, want 1", len(updates))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.EvictStale(); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if r, _ := s.applyContentHashUpdates(updates, pathNodeSnapshot{}); r != 0 {
		t.Errorf("rehashed %d evicted transmissions, want 0", r)
	}
	for h, tx := range s.byHash {
		if tx == old {
			t.Errorf("evicted transmission republished in byHash under %q", h)
		}
	}
	if s.byTxID[old.ID] != nil {
		t.Error("evicted transmission republished in byTxID")
	}
}
