package main

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// #534: byNode's de-dup sets hold transmission IDs, not hashes. These tests
// pin that the sets list exactly what byNode lists on every path that feeds or
// trims it (Load, background chunks, live transmissions and observations,
// hash migration and its merges, eviction), that nothing stale is left behind,
// and the few places where the hash keys behaved differently.

// bn534AssertConsistent fails unless every byNode list holds live
// transmissions, each once, and its de-dup set holds exactly their IDs, with
// no set left for a pubkey byNode does not list. Must not hold s.mu.
func bn534AssertConsistent(t *testing.T, s *PacketStore, when string) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for pk, list := range s.byNode {
		ids := make([]int, 0, len(list))
		seen := map[*StoreTx]bool{}
		for _, tx := range list {
			if seen[tx] {
				t.Errorf("%s: byNode[%.8s] lists tx %d twice", when, pk, tx.ID)
			}
			seen[tx] = true
			if s.byTxID[tx.ID] != tx {
				t.Errorf("%s: byNode[%.8s] lists tx %d, which is not live", when, pk, tx.ID)
			}
			ids = append(ids, tx.ID)
		}
		sort.Ints(ids)
		if got := bn534SetIDs(s, pk); !reflect.DeepEqual(got, ids) {
			t.Errorf("%s: de-dup set of %.8s = %v, byNode lists %v", when, pk, got, ids)
		}
	}
	for _, pk := range bn534SetKeys(s) {
		if _, ok := s.byNode[pk]; !ok {
			t.Errorf("%s: de-dup set left for %.8s, which byNode no longer lists: %v", when, pk, bn534SetIDs(s, pk))
		}
	}
}

// bn534Members returns pubkey → sorted IDs of what byNode lists, for the given
// pubkeys. Must not hold s.mu.
func bn534Members(s *PacketStore, pks ...string) map[string][]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string][]int{}
	for _, pk := range pks {
		for _, tx := range s.byNode[pk] {
			out[pk] = append(out[pk], tx.ID)
		}
		sort.Ints(out[pk])
	}
	return out
}

func bn534RP(pks ...string) string {
	q := make([]string, len(pks))
	for i, pk := range pks {
		q[i] = fmt.Sprintf("%q", pk)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// bn534Scenario is a store fed through every path that adds to byNode, with
// the expected membership written out by hand (the oracle), plus the pubkeys
// it covers. PK(5) is a relay many transmissions share; "aa" resolves to
// PK(170) through the path_json fallback (leak202SeedNodes).
type bn534Scenario struct {
	d    *hm215DB
	s    *PacketStore
	pks  []string
	want map[string][]int
}

func bn534PKs() (p1, p5, p6, p7, p8, p9, pAA string) {
	return acct113PK(1), acct113PK(5), acct113PK(6), acct113PK(7), acct113PK(8), acct113PK(9), acct113PK(0xaa)
}

func bn534Build(t *testing.T) *bn534Scenario {
	t.Helper()
	p1, p5, p6, p7, p8, _, pAA := bn534PKs()
	d := hm215Create(t)
	// The scenario writes while the store's read-only handle reads
	// (concurrently in TestByNodeConcurrentReaders_534): wait for the lock
	// like the ingestor does instead of failing on SQLITE_BUSY.
	d.conn.SetMaxOpenConns(1)
	if _, err := d.conn.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		t.Fatal(err)
	}
	hot, old := hm215Time(-30*time.Minute), hm215Time(-48*time.Hour)
	grp := `{"type":"GRP_TXT","channel":"#t","text":"x"}`
	// 1: an advert from PK(1), relayed by 5 and 6, heard again later via 5,
	// and once more on an observer/path it was already heard on (dropped by
	// the #411 dedup).
	d.tx(t, 1, hm215Raw(1), ComputeContentHash(hm215Raw(1)), hot, 4, hm215Advert(p1))
	d.observation(t, 1, 0, `["05","06"]`, bn534RP(p5, p6), hot)
	d.observation(t, 1, 1, `["05"]`, bn534RP(p5), hot)
	d.observation(t, 1, 0, `["05","06"]`, bn534RP(p5, p6), hot)
	// 2: a second hash under PK(5), with PK(7).
	d.tx(t, 2, hm215Raw(2), ComputeContentHash(hm215Raw(2)), hot, 5, grp)
	d.observation(t, 2, 2, `["05","07"]`, bn534RP(p5, p7), hot)
	// 3: resolved_path NULL: the fallback resolves "aa" to PK(170).
	d.tx(t, 3, hm215Raw(3), ComputeContentHash(hm215Raw(3)), hot, 5, grp)
	d.observation(t, 3, 3, `["aa"]`, "", hot)
	// 4 and 5: older than the hot window, loaded by a background chunk.
	d.tx(t, 4, hm215Raw(4), ComputeContentHash(hm215Raw(4)), old, 5, grp)
	d.observation(t, 4, 0, `["05","08"]`, bn534RP(p5, p8), old)
	d.observation(t, 4, 1, `["08"]`, bn534RP(p8), old)
	d.tx(t, 5, hm215Raw(5), ComputeContentHash(hm215Raw(5)), old, 4, hm215Advert(p1))
	d.observation(t, 5, 2, `["aa"]`, "", old)

	db, err := OpenDB(d.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	s := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72, HotStartupHours: 1})
	leak202SeedNodes(s, true)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if !s.WaitIndexesReady(30 * time.Second) {
		t.Fatal("index builds did not finish")
	}
	sc := &bn534Scenario{d: d, s: s, pks: []string{p1, p5, p6, p7, p8, pAA}}
	sc.want = map[string][]int{p1: {1}, p5: {1, 2}, p6: {1}, p7: {2}, pAA: {3}}
	sc.check(t, "hot load")

	if err := s.loadChunk(time.Now().UTC().Add(-72*time.Hour), time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	sc.want = map[string][]int{p1: {1, 5}, p5: {1, 2, 4}, p6: {1}, p7: {2}, p8: {4}, pAA: {3, 5}}
	sc.check(t, "background chunk")
	return sc
}

func (sc *bn534Scenario) check(t *testing.T, when string) {
	t.Helper()
	bn534AssertConsistent(t, sc.s, when)
	got := bn534Members(sc.s, sc.pks...)
	for _, pk := range sc.pks {
		if !reflect.DeepEqual(got[pk], sc.want[pk]) {
			t.Errorf("%s: byNode[%.8s] = %v, want %v", when, pk, got[pk], sc.want[pk])
		}
	}
}

// Live traffic after the load: later observations (a new relay, the same
// relays again, a duplicate) and a new transmission; then eviction, and a
// transmission that comes back with an evicted one's hash.
func (sc *bn534Scenario) live(t *testing.T) {
	t.Helper()
	p1, p5, p6, p7, p8, p9, pAA := bn534PKs()
	d, s := sc.d, sc.s
	hot := hm215Time(-10 * time.Minute)
	d.observation(t, 1, 2, `["05","09"]`, bn534RP(p5, p9), hot) // later, new relay 9
	d.observation(t, 1, 0, `["05","06"]`, bn534RP(p5, p6), hot) // duplicate
	d.observation(t, 4, 2, `["05","08"]`, bn534RP(p5, p8), hot) // chunk-loaded tx, same relays
	d.observation(t, 3, 1, `["aa"]`, "", hot)                   // fallback again
	s.IngestNewObservations(0, 1000)
	sc.pks = append(sc.pks, p9)
	sc.want = map[string][]int{p1: {1, 5}, p5: {1, 2, 4}, p6: {1}, p7: {2}, p8: {4}, p9: {1}, pAA: {3, 5}}
	sc.check(t, "later observations")

	d.tx(t, 6, hm215Raw(6), ComputeContentHash(hm215Raw(6)), hot, 4, hm215Advert(p1))
	d.observation(t, 6, 0, `["05"]`, bn534RP(p5), hot)
	s.IngestNewFromDB(5, 100)
	sc.want = map[string][]int{p1: {1, 5, 6}, p5: {1, 2, 4, 6}, p6: {1}, p7: {2}, p8: {4}, p9: {1}, pAA: {3, 5}}
	sc.check(t, "new transmission")

	// The two older transmissions leave the retention window.
	s.mu.Lock()
	s.retentionHours = 24
	s.mu.Unlock()
	if n := s.RunEviction(); n != 2 {
		t.Fatalf("evicted %d transmissions, want 2", n)
	}
	sc.want = map[string][]int{p1: {1, 6}, p5: {1, 2, 6}, p6: {1}, p7: {2}, p9: {1}, pAA: {3}}
	sc.check(t, "eviction")

	// The DB's retention removes row 4; the same packet is stored again as a
	// new row and comes back under the relays it was heard through.
	if _, err := d.conn.Exec(`DELETE FROM observations WHERE transmission_id = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.conn.Exec(`DELETE FROM transmissions WHERE id = 4`); err != nil {
		t.Fatal(err)
	}
	d.tx(t, 7, hm215Raw(4), ComputeContentHash(hm215Raw(4)), hot, 5, `{"type":"GRP_TXT","channel":"#t","text":"x"}`)
	d.observation(t, 7, 0, `["05","08"]`, bn534RP(p5, p8), hot)
	s.IngestNewFromDB(6, 100)
	sc.want = map[string][]int{p1: {1, 6}, p5: {1, 2, 6, 7}, p6: {1}, p7: {2}, p8: {7}, p9: {1}, pAA: {3}}
	sc.check(t, "re-added after eviction")
}

// Membership through every feeding and trimming path matches the oracle, and
// the sets hold exactly what byNode lists: no duplicate, no missing member, no
// stale ID.
func TestByNodeMembershipAcrossPaths_534(t *testing.T) {
	sc := bn534Build(t)
	sc.live(t)
}

// Readers that walk byNode and its sets (bulk health with a region, node
// health, node analytics, the node filter of the packets query) run while
// every writer path does: chunk load, live transmissions and observations,
// eviction and the hash migration. Meaningful under -race.
func TestByNodeConcurrentReaders_534(t *testing.T) {
	_, p5, _, _, _, _, _ := bn534PKs()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var sc *bn534Scenario
	ready := make(chan *PacketStore, 1)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			s := <-ready
			ready <- s
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch r {
				case 0:
					s.GetBulkHealth(50, "TST", "")
				case 1:
					s.GetNodeHealth(p5, time.Now().UTC())
				case 2:
					s.GetNodeAnalytics(p5, 7)
				case 3:
					s.QueryPackets(PacketQuery{Node: p5, Limit: 50})
				}
			}
		}(r)
	}
	sc = bn534Build(t)
	ready <- sc.s
	sc.live(t)
	migrateContentHashesAsync(sc.s, 2, 0)
	close(stop)
	wg.Wait()
	bn534AssertConsistent(t, sc.s, "after concurrent readers")
}

// A rehash (#215) keeps a transmission's ID, so the sets need no change: no
// pass over byNode or the sets, with the resolved-pubkey index on or off. The
// hash keys had to be renamed in every set the transmission was in, and with
// the index off that took one pass over every set per batch.
func TestHashMigrateRehashLeavesNodeSetsAlone_534(t *testing.T) {
	for _, idx := range []bool{true, false} {
		t.Run(fmt.Sprintf("resolvedIndex=%v", idx), func(t *testing.T) {
			d := hm215Create(t)
			d.ballast(t)
			d.stale(t, 0, 0, 3) // three rehashes, no collision
			s := hm215OpenIdx(t, d, idx)
			before := hashRekeySweeps.Load()
			hm215Migrate(s, 100)
			if n := hashRekeySweeps.Load() - before; n != 0 {
				t.Errorf("a rehash without merges made %d passes over the node index, want 0", n)
			}
			bn534AssertConsistent(t, s, "after rehash")
			// A later observation of a rehashed transmission, under the
			// relay it was indexed through, must not list it again.
			s.mu.Lock()
			tx := s.byTxID[501]
			if tx == nil {
				s.mu.Unlock()
				t.Fatal("setup: tx 501 missing")
			}
			s.addToByNode(tx, acct113PK(5))
			s.addToByNode(tx, acct113PK(201))
			s.mu.Unlock()
			bn534AssertConsistent(t, s, "late observation after rehash")
		})
	}
}

// Merges (#215): the duplicates leave every list and set, the survivor stays
// listed where it was, and nothing points at a merged-away ID.
func TestHashMigrateMergeKeepsNodeSetsExact_534(t *testing.T) {
	for _, idx := range []bool{true, false} {
		t.Run(fmt.Sprintf("resolvedIndex=%v", idx), func(t *testing.T) {
			d := hm215Create(t)
			d.ballast(t)
			d.stale(t, 3, 3, 2)
			s := hm215OpenIdx(t, d, idx)
			hm215Migrate(s, 2)
			bn534AssertConsistent(t, s, "after merge")
			s.mu.RLock()
			for _, gone := range []int{12, 13, 22, 23, 32, 33} {
				if s.byTxID[gone] != nil {
					t.Errorf("tx %d survived the merge", gone)
				}
			}
			s.mu.RUnlock()
		})
	}
}

// Eviction rebuilt the relays of a transmission from a SQL prefetch, and one
// it missed (the per-chunk query error was skipped, or the DB's resolved_path
// no longer named it) left that relay's entry behind; keyed by hash, the stale
// entry kept a later transmission with the same content hash out of byNode for
// good. Since #621 eviction sweeps the actual memberships, and the later
// transmission is listed either way.
func TestByNodeStaleEntryDoesNotBlockSameHash_534(t *testing.T) {
	s := NewPacketStore(nil, &PacketStoreConfig{RetentionHours: 24})
	relay := acct113PK(5)
	old := &StoreTx{ID: 1, Hash: "c0ffee0000534534", FirstSeen: hm215Time(-48 * time.Hour)}
	s.mu.Lock()
	s.packets = append(s.packets, old)
	s.byHash[old.Hash], s.byTxID[old.ID] = old, old
	s.addToByNode(old, relay)
	if n := s.EvictStale(); n != 1 {
		s.mu.Unlock()
		t.Fatalf("evicted %d, want 1", n)
	}
	again := &StoreTx{ID: 2, Hash: old.Hash, FirstSeen: hm215Time(-time.Minute)}
	s.packets = append(s.packets, again)
	s.byHash[again.Hash], s.byTxID[again.ID] = again, again
	s.addToByNode(again, relay)
	listed := false
	for _, tx := range s.byNode[relay] {
		listed = listed || tx == again
	}
	s.mu.Unlock()
	if !listed {
		t.Fatal("a transmission with an evicted one's hash is not listed under the relay it was heard through")
	}
}

// The regional bulk-health filter keeps a node when one of its transmissions
// was heard by an observer in the region. It matches the region's
// observations to the sets by transmission ID; observation IDs differ from
// transmission IDs here, so matching on the wrong one fails. Relay d's
// transmission was evicted, and the same packet was later heard again in EDI
// only. Before #621 an eviction prefetch that missed d left d's entry behind,
// and the hash keys matched it and kept d in EDI.
func TestGetBulkHealthRegionMatchesByTransmission_534(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	now := time.Now().UTC()
	ts, epoch := now.Add(-time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Unix()
	oldTS, oldEpoch := now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(-48*time.Hour).Unix()
	const (
		a = "aaaa000000000001" // heard only in EDI
		b = "bbbb000000000002" // heard only in GLA
		c = "cccc000000000003" // adverts in GLA, relays a packet heard in EDI
		d = "dddd000000000004" // relayed an old packet in GLA
	)
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('obsE', 'Edinburgh', 'EDI', ?, ?, 1)`, ts, ts)
	exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('obsG', 'Glasgow', 'GLA', ?, ?, 1)`, ts, ts)
	for _, pk := range []string{a, b, c, d} {
		exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', 56.0, -3.5, ?, ?, 1)`, pk, "R-"+pk[:4], ts, ts)
	}
	advert := func(id int, pk string) {
		exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey) VALUES (?, 'AA00', ?, ?, 1, 4, ?, ?)`,
			id, "h"+pk, ts, `{"pubKey":"`+pk+`","name":"R","type":"ADVERT"}`, pk)
	}
	advert(10, a)
	advert(20, b)
	advert(30, c)
	exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES (40, 'CC00', 'hchan', ?, 1, 5, '{"type":"CHAN","text":"x"}')`, ts)
	exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES (50, 'DD00', 'hagain', ?, 1, 5, '{"type":"CHAN","text":"y"}')`, oldTS)
	// observer_idx is the observers rowid: 1 = obsE (EDI), 2 = obsG (GLA).
	// Observation IDs 1-5 never equal a transmission ID.
	exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (1, 10, 1, 5, -90, '[]', ?)`, epoch)
	exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (2, 20, 2, 5, -90, '[]', ?)`, epoch)
	exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (3, 30, 2, 5, -90, '[]', ?)`, epoch)
	exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path) VALUES (4, 40, 1, 5, -90, '["cc"]', ?, ?)`, epoch, `["`+c+`"]`)
	exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path) VALUES (5, 50, 2, 5, -90, '["dd"]', ?, ?)`, oldEpoch, `["`+d+`"]`)

	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72})
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	keys := func(region string) []string {
		var out []string
		for _, r := range store.GetBulkHealth(50, region, "") {
			out = append(out, r["public_key"].(string))
		}
		sort.Strings(out)
		return out
	}
	check := func(when string, want map[string][]string) {
		t.Helper()
		for region, w := range want {
			// The region→observers cache is not what is under test.
			store.regionObsMu.Lock()
			store.regionObsCache = nil
			store.regionObsMu.Unlock()
			if got := keys(region); !reflect.DeepEqual(got, w) {
				t.Errorf("%s: region %s: got %v, want %v", when, region, got, w)
			}
		}
	}
	check("load", map[string][]string{"EDI": {a, c}, "GLA": {b, c, d}})

	store.mu.Lock()
	store.retentionHours = 24
	evicted := store.EvictStale()
	store.mu.Unlock()
	if evicted != 1 {
		t.Fatalf("evicted %d, want 1", evicted)
	}
	exec(`DELETE FROM observations WHERE transmission_id = 50`)
	exec(`DELETE FROM transmissions WHERE id = 50`)
	exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES (60, 'DD00', 'hagain', ?, 1, 5, '{"type":"CHAN","text":"y"}')`, ts)
	exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (6, 60, 1, 5, -90, '[]', ?)`, epoch)
	store.IngestNewFromDB(40, 100)
	check("same packet heard again in EDI", map[string][]string{"EDI": {a, c}, "GLA": {b, c}})
}

// The one way two live transmissions share a hash: the ingestor's #215
// migration merges a row the server already loaded hot into an older row
// while the background fill is still loading (the in-memory migration only
// starts after the fill). The chunk then brings the older row with the same
// hash; byHash keeps the hot one, byTxID, s.packets and byPayloadType hold
// both. The hash keys left the chunk's transmission out of byNode wherever
// the hot one was listed; keyed by ID it is listed like in every other index,
// and evicting either leaves the other listed.
func TestByNodeChunkGhostListedLikeOtherIndexes_534(t *testing.T) {
	p1, p5, _, _, _, _, _ := bn534PKs()
	d := hm215Create(t)
	raw := hm215Raw(9)
	h := ComputeContentHash(raw)
	hot, old := hm215Time(-30*time.Minute), hm215Time(-48*time.Hour)
	d.tx(t, 20, raw, h, hot, 4, hm215Advert(p1))
	d.observation(t, 20, 0, `["05"]`, bn534RP(p5), hot)
	d.tx(t, 10, raw, h, old, 4, hm215Advert(p1))
	d.observation(t, 10, 1, `["05"]`, bn534RP(p5), old)
	db, err := OpenDB(d.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	s := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72, HotStartupHours: 1})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if err := s.loadChunk(time.Now().UTC().Add(-72*time.Hour), time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	both := s.byTxID[10] != nil && s.byTxID[20] != nil && len(s.packets) == 2 && len(s.byPayloadType[4]) == 2
	s.mu.RUnlock()
	if !both {
		t.Fatal("setup: the chunk did not leave two live transmissions with one hash")
	}
	bn534AssertConsistent(t, s, "chunk with a held hash")
	want := map[string][]int{p1: {10, 20}, p5: {10, 20}}
	if got := bn534Members(s, p1, p5); !reflect.DeepEqual(got, want) {
		t.Errorf("byNode = %v, want %v", got, want)
	}
	s.mu.Lock()
	s.retentionHours = 24
	s.mu.Unlock()
	if n := s.RunEviction(); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	bn534AssertConsistent(t, s, "after evicting the older one")
	want = map[string][]int{p1: {20}, p5: {20}}
	if got := bn534Members(s, p1, p5); !reflect.DeepEqual(got, want) {
		t.Errorf("after eviction byNode = %v, want %v", got, want)
	}
}

// The sets cost what the large-instance shape says they cost: built through
// addToByNode from the call stream of a load (decoded pubkeys plus every
// observation's resolved relays, de-dup hits included) at a fifth of the large
// instance's per-node degree, they keep at most 24 bytes per entry. Measured
// here: hash keys (a 16-byte string header and a bool per slot) 46, int keys
// (a slot padded to 16 bytes) 32, uint32 keys 17. The heap is
// read in a child process: goroutines earlier tests leave running (index
// builders, recomputers) allocate while it is read and make an in-process
// figure meaningless.
func TestNodeTxSetsRetainedHeap_534(t *testing.T) {
	if os.Getenv("CORESCOPE_534_HEAP_CHILD") == "1" {
		stream := bn534Stream(bn534Data(30000, 2000, 490000, 534))
		seen := map[bn534Pair]bool{}
		pairs := make([]bn534Pair, 0, len(stream)/5)
		for _, p := range stream {
			if !seen[p] {
				seen[p] = true
				pairs = append(pairs, p)
			}
		}
		seen = nil
		var got []float64
		for i := 0; i < 3; i++ {
			got = append(got, float64(bn534SetsRetained(pairs))/float64(len(pairs)))
		}
		sort.Float64s(got)
		fmt.Printf("BN534 entries=%d perEntry=%.2f runs=%v\n", len(pairs), got[1], got)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNodeTxSetsRetainedHeap_534$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CORESCOPE_534_HEAP_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	var entries int
	var perEntry float64
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "BN534 ") {
			line = l
		}
	}
	if _, err := fmt.Sscanf(line, "BN534 entries=%d perEntry=%f", &entries, &perEntry); err != nil {
		t.Fatalf("no measurement in the child's output (%v):\n%s", err, out)
	}
	t.Log(line)
	const maxPerEntry = 24
	if perEntry > maxPerEntry {
		t.Fatalf("the sets keep %.1f B per entry over %d entries, limit %d", perEntry, entries, maxPerEntry)
	}
}

// Transmission IDs are int64 rowids. One that does not fit a uint32 is kept
// whole, so two IDs that share their low 32 bits stay two members, and
// removing one leaves the other.
func TestNodeTxSetKeepsIDsBeyondUint32_534(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int is 32 bits: every ID fits")
	}
	var big int64 = 1<<32 + 5
	s := NewPacketStore(nil, nil)
	low := &StoreTx{ID: 5, Hash: "low"}
	high := &StoreTx{ID: int(big), Hash: "high"}
	s.addToByNode(low, "pk")
	s.addToByNode(high, "pk")
	s.addToByNode(high, "pk")
	if n := len(s.byNode["pk"]); n != 2 {
		t.Fatalf("byNode lists %d transmissions, want 2 (IDs 5 and 2^32+5)", n)
	}
	if got := bn534SetIDs(s, "pk"); !reflect.DeepEqual(got, []int{5, int(big)}) {
		t.Fatalf("set = %v, want [5 %d]", got, big)
	}
	bn534Unlist(s, "pk", high)
	if got := bn534SetIDs(s, "pk"); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("after removing 2^32+5 the set is %v, want [5]", got)
	}
	bn534Unlist(s, "pk", low)
	if keys := bn534SetKeys(s); len(keys) != 0 {
		t.Fatalf("an emptied set is kept: %v", keys)
	}
}

// perResolvedRelayBytes is what a resolved relay key costs: a byPathHop slot,
// a pathHopResolved hash, a byNode slot, a de-dup set entry and a
// resolved-pubkey index entry. The set entry is the part #534 shrank, so the
// charge must follow it or trackedBytes overstates the store and eviction
// runs early. Measured in a child process (see TestNodeTxSetsRetainedHeap_534)
// as the marginal heap per key between stores whose transmissions are indexed
// under 1 and under 4 resolved relays, through indexResolvedPathHops as Load
// does per observation (no SQL, so it stays cheap under the CI's -race and
// coverage): the charge must stay within a third of it either way, the band
// the calibration of #164 sat in at 100k and 500k transmissions.
func TestResolvedRelayChargeTracksHeap_534(t *testing.T) {
	if os.Getenv("CORESCOPE_534_CHARGE_CHILD") == "1" {
		const n = 40000
		run := func(perTx int) (heap int64, keys int) {
			s, h := heapOf(func() *PacketStore {
				s := NewPacketStore(nil, &PacketStoreConfig{})
				hopsSeen := map[string]bool{}
				pks := make([]string, 0, perTx)
				for i := 1; i <= n; i++ {
					tx := &StoreTx{ID: i, Hash: fmt.Sprintf("h%06d", i)}
					s.packets = append(s.packets, tx)
					s.byHash[tx.Hash], s.byTxID[tx.ID] = tx, tx
					pks = pks[:0]
					for h := 0; h < perTx; h++ {
						pks = append(pks, acct113PK(i+h*7))
					}
					s.indexResolvedPathHops(tx, pks, hopsSeen)
				}
				return s
			})
			for _, hs := range s.pathHopResolved {
				keys += len(hs)
			}
			runtime.KeepAlive(s)
			return h, keys
		}
		h1, k1 := run(1)
		h4, k4 := run(4)
		fmt.Printf("BN534CHARGE perKey=%.2f keys=%d,%d\n", float64(h4-h1)/float64(k4-k1), k1, k4)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestResolvedRelayChargeTracksHeap_534$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CORESCOPE_534_CHARGE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	var perKey float64
	var k1, k4 int
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "BN534CHARGE ") {
			line = l
		}
	}
	if _, err := fmt.Sscanf(line, "BN534CHARGE perKey=%f keys=%d,%d", &perKey, &k1, &k4); err != nil {
		t.Fatalf("no measurement in the child's output (%v):\n%s", err, out)
	}
	t.Logf("%s; charged %d B per key", line, perResolvedRelayBytes)
	if lo, hi := perKey*0.75, perKey*4/3; float64(perResolvedRelayBytes) < lo || float64(perResolvedRelayBytes) > hi {
		t.Fatalf("a resolved relay key is charged %d B and costs %.1f B: outside [%.0f, %.0f]", perResolvedRelayBytes, perKey, lo, hi)
	}
}
