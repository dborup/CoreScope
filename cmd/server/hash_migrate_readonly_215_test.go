package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Issue #215: the content-hash migration used to UPDATE/DELETE transmissions
// and observations on the server's mode=ro handle. Every write failed, was
// logged as a collision, and left in-memory duplicates ("ghosts") sharing one
// hash. The DB half of the migration now lives in the ingestor; the server
// only rehashes and merges in memory. These tests pin that.

const hm215Observers = 4

// Relay paths: hop i of an observation is the first byte of resolved pubkey i
// (acct113PK(k) starts with byte k), as the store assumes when it sweeps
// byPathHop by hop prefix.

// hm215Raw is a distinct, decodable raw packet per key. ComputeContentHash
// differs for every key, and never equals the "stale-*" hashes below.
func hm215Raw(key int) string {
	return fmt.Sprintf("0A00D69FD7A5A7475DB07337749AE61FA53A4788E9%02X", key)
}

func hm215Time(offset time.Duration) string {
	return time.Now().UTC().Add(offset).Format(time.RFC3339)
}

type hm215DB struct {
	conn *sql.DB
	path string
	obs  int
}

func hm215Create(t testing.TB) *hm215DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hm215.db")
	acct113CreateDBOpts(t, path, 0, 0, 0) // schema + observers obs0..obs3, no rows
	conn, err := sql.Open("sqlite", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &hm215DB{conn: conn, path: path}
}

func (d *hm215DB) tx(t testing.TB, id int, raw, hash, firstSeen string, payloadType int, decoded string) {
	t.Helper()
	if _, err := d.conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
		VALUES (?, ?, ?, ?, 1, ?, 1, ?)`, id, raw, hash, firstSeen, payloadType, decoded); err != nil {
		t.Fatal(err)
	}
}

func (d *hm215DB) observation(t testing.TB, txID, observer int, path, resolved, ts string) {
	t.Helper()
	d.obs++
	var rp interface{}
	if resolved != "" {
		rp = resolved
	}
	if _, err := d.conn.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp, resolved_path)
		VALUES (?, ?, ?, ?, 'RX', -5.0, -90.0, 5, ?, ?, ?)`,
		d.obs, txID, fmt.Sprintf("obs%d", observer%hm215Observers), fmt.Sprintf("Observer%d", observer%hm215Observers), path, ts, rp); err != nil {
		t.Fatal(err)
	}
}

func hm215Advert(pk string) string {
	return fmt.Sprintf(`{"type":"ADVERT","pubKey":%q,"name":"n"}`, pk)
}

// hm215Ballast is a recent, correctly hashed transmission that stays live. It
// shares relay keys with the stale rows so a shared byNode list is exercised.
func (d *hm215DB) ballast(t testing.TB) {
	raw := hm215Raw(250)
	ts := hm215Time(-time.Hour)
	d.tx(t, 1000, raw, ComputeContentHash(raw), ts, 4, hm215Advert(acct113PK(240)))
	// PK(18) is also the relay of the second row of every stale group, which
	// the survivor (the first row) does not have: removing that row leaves the
	// ballast alone in byNode[PK(18)], and its nodeHashes entry must then hold
	// the ballast's hash only.
	d.observation(t, 1000, 0, `["05","11","12"]`, fmt.Sprintf(`[%q,%q,%q]`, acct113PK(5), acct113PK(17), acct113PK(18)), ts)
	d.observation(t, 1000, 1, `["aa"]`, "", ts) // fallback relay
}

// hm215Stale adds the stale rows: groups of `dups` duplicate rows that share a
// raw packet (so one content hash), and `singles` rows that migrate without a
// collision. Group g runs its first_seen order with the ids for odd g and
// against them for even g, so both "the first row processed survives" and
// "a later row takes over" happen. Observation (obs0, ["aa"]) is identical
// across the rows of a group: the DB would reject it as a duplicate, so the
// merge must keep it once.
func (d *hm215DB) stale(t testing.TB, groups, dups, singles int) {
	old := -72 * time.Hour
	for g := 1; g <= groups; g++ {
		raw := hm215Raw(g)
		pk := acct113PK(100 + g)
		for k := 1; k <= dups; k++ {
			id := g*10 + k
			order := k
			if g%2 == 0 {
				order = dups - k + 1
			}
			ts := hm215Time(old + time.Duration(order)*time.Second)
			d.tx(t, id, raw, fmt.Sprintf("stale-%d-%d", g, k), ts, 4, hm215Advert(pk))
			d.observation(t, id, 0, `["aa"]`, "", ts) // same in every row: fallback relay
			// Observer 0 again, with a path of its own: not a duplicate of the
			// one above or of the other rows' (the dedup key is observer AND
			// path), so the merge must keep every one of them.
			d.observation(t, id, 0, fmt.Sprintf(`["%02x"]`, 32+k), "", ts)
			path, resolved := fmt.Sprintf(`["05","%02x"]`, 16+k), fmt.Sprintf(`[%q,%q]`, acct113PK(5), acct113PK(16+k))
			if k == dups {
				// The last row has the longest path: once its observations
				// move to the survivor, the survivor's best path changes.
				path = fmt.Sprintf(`["05","%02x","11"]`, 16+k)
				resolved = fmt.Sprintf(`[%q,%q,%q]`, acct113PK(5), acct113PK(16+k), acct113PK(17))
			}
			d.observation(t, id, k, path, resolved, ts)
		}
	}
	for s := 1; s <= singles; s++ {
		id := 500 + s
		ts := hm215Time(old + time.Duration(s)*time.Second)
		d.tx(t, id, hm215Raw(100+s), fmt.Sprintf("stale-single-%d", s), ts, 4, hm215Advert(acct113PK(200+s)))
		d.observation(t, id, s, fmt.Sprintf(`["05","%02x"]`, 30+s), fmt.Sprintf(`[%q,%q]`, acct113PK(5), acct113PK(30+s)), ts)
	}
}

func hm215Open(t testing.TB, d *hm215DB) *PacketStore { return hm215OpenIdx(t, d, true) }

// hm215OpenIdx opens the store with the resolved-pubkey index on or off (the
// feature flag; off is the conservative path).
func hm215OpenIdx(t testing.TB, d *hm215DB, resolvedIndex bool) *PacketStore {
	t.Helper()
	db, err := OpenDB(d.path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &PacketStoreConfig{}
	store := NewPacketStore(db, cfg)
	store.useResolvedPathIndex = resolvedIndex
	leak202SeedNodes(store, true) // hop "aa" resolves to a unique node
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(30 * time.Second) {
		t.Fatal("background index builds did not finish")
	}
	t.Cleanup(func() { db.conn.Close() })
	return store
}

// hm215Snapshot is the part of the store a leftover would show up in.
// spTotalPaths is left out on purpose: eviction never decrements it (master
// behaviour, unrelated to this change), so it cannot return to a baseline.
type hm215Snapshot struct {
	Packets       int
	TrackedBytes  int64
	ByNode        map[string][]int
	NodeHashes    map[string][]string
	ByPayloadType map[int]int
	ByObserver    map[string]int
	ByObsID       int
	ByTxID        int
	ByHash        int
	PathHopRecs   int
	FallbackRecs  int
	ResolvedIdx   int
	TotalObs      int
	AdvertKeys    map[string]int
	ByPathHop     map[string]int
	SpIndex       map[string]int
	SpTxIndex     map[string]int
}

func hm215Snap(s *PacketStore) hm215Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sn := hm215Snapshot{
		Packets: len(s.packets), TrackedBytes: s.trackedBytes,
		ByNode: map[string][]int{}, NodeHashes: map[string][]string{},
		ByPayloadType: map[int]int{}, ByObserver: map[string]int{},
		ByObsID: len(s.byObsID), ByTxID: len(s.byTxID), ByHash: len(s.byHash),
		PathHopRecs: len(s.pathHopResolved), FallbackRecs: len(s.fallbackByNode),
		ResolvedIdx: len(s.resolvedPubkeyIndex), TotalObs: s.totalObs,
		AdvertKeys: map[string]int{},
		ByPathHop:  map[string]int{}, SpIndex: map[string]int{}, SpTxIndex: map[string]int{},
	}
	for pk, list := range s.byNode {
		for _, tx := range list {
			sn.ByNode[pk] = append(sn.ByNode[pk], tx.ID)
		}
		sort.Ints(sn.ByNode[pk])
	}
	for pk, hashes := range s.nodeHashes {
		for h := range hashes {
			sn.NodeHashes[pk] = append(sn.NodeHashes[pk], h)
		}
		sort.Strings(sn.NodeHashes[pk])
	}
	for pt, list := range s.byPayloadType {
		sn.ByPayloadType[pt] = len(list)
	}
	for o, list := range s.byObserver {
		sn.ByObserver[o] = len(list)
	}
	for pk, n := range s.advertPubkeys {
		sn.AdvertKeys[pk] = n
	}
	for k, list := range s.byPathHop {
		sn.ByPathHop[k] = len(list)
	}
	for k, n := range s.spIndex {
		sn.SpIndex[k] = n
	}
	for k, list := range s.spTxIndex {
		sn.SpTxIndex[k] = len(list)
	}
	return sn
}

func hm215Migrate(s *PacketStore, batch int) string {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	migrateContentHashesAsync(s, batch, 0)
	return buf.String()
}

// stmtLog records every statement a connection prepares or every transaction it
// begins. database/sql falls back to Prepare/Begin for a driver connection that
// implements nothing else, so wrapping driver.Conn sees all of them.
type stmtLog struct {
	mu    sync.Mutex
	stmts []string
}

func (l *stmtLog) add(q string) {
	l.mu.Lock()
	l.stmts = append(l.stmts, q)
	l.mu.Unlock()
}

func (l *stmtLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.stmts...)
}

type recordingConn struct {
	driver.Conn
	log *stmtLog
}

func (c *recordingConn) Prepare(q string) (driver.Stmt, error) {
	c.log.add(q)
	return c.Conn.Prepare(q)
}

func (c *recordingConn) Begin() (driver.Tx, error) { //nolint:staticcheck // the fallback database/sql uses
	c.log.add("BEGIN")
	return c.Conn.Begin() //nolint:staticcheck
}

type recordingConnector struct {
	drv driver.Driver
	dsn string
	log *stmtLog
}

func (c *recordingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &recordingConn{Conn: conn, log: c.log}, nil
}

func (c *recordingConnector) Driver() driver.Driver { return c.drv }

// hm215OpenRecording opens the store the way the server does (mode=ro, same
// DSN as OpenDB) on a connection that records every statement.
func hm215OpenRecording(t testing.TB, d *hm215DB) (*PacketStore, *stmtLog) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL&_busy_timeout=5000", d.path)
	probe, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drv := probe.Driver()
	probe.Close()
	rec := &stmtLog{}
	conn := sql.OpenDB(&recordingConnector{drv: drv, dsn: dsn, log: rec})
	conn.SetMaxOpenConns(4)
	t.Cleanup(func() { conn.Close() })
	db := &DB{conn: conn, path: d.path, schemaHealerStop: make(chan struct{})}
	db.detectSchema()
	store := NewPacketStore(db, &PacketStoreConfig{})
	leak202SeedNodes(store, true)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(30 * time.Second) {
		t.Fatal("background index builds did not finish")
	}
	return store, rec
}

// A probe on a connection that records statements, not only a log line: the
// migration may run reads, but it must not prepare a single write statement or
// begin a transaction, whether or not the read-only handle would reject it.
func TestHashMigrate_IssuesNoWriteStatements_215(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 2, 3, 2)
	s, rec := hm215OpenRecording(t, d)
	loaded := len(rec.all())
	if loaded == 0 {
		t.Fatal("setup: the probe recorded no statements from the load, so it would see nothing of the migration either")
	}
	hm215Migrate(s, 2)
	if len(s.byHash) != 5 {
		t.Fatalf("setup: the migration did not run: %d transmissions by hash, want 5", len(s.byHash))
	}
	for _, q := range rec.all() {
		head := strings.ToUpper(strings.TrimSpace(q))
		if strings.HasPrefix(head, "SELECT") || strings.HasPrefix(head, "PRAGMA") || strings.HasPrefix(head, "WITH") {
			continue
		}
		t.Errorf("the server prepared a non-read statement: %q", q)
	}
}

// The migration must not try to write: no statement may fail on the mode=ro
// handle, so nothing may be logged as a collision or an error, and the DB
// stays exactly as the ingestor left it.
func TestHashMigrate_NeverWritesOnTheReadOnlyHandle_215(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 2, 3, 2)
	s := hm215Open(t, d)

	out := hm215Migrate(s, 2)
	for _, bad := range []string{"collides", "begin tx", "prepare", "commit", "readonly", "read-only"} {
		if strings.Contains(out, bad) {
			t.Errorf("the migration logged %q, which means it tried to write on the read-only handle:\n%s", bad, out)
		}
	}
	var rows int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE hash LIKE 'stale-%'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if want := 2*3 + 2; rows != want {
		t.Fatalf("%d stale rows left in the DB, want all %d untouched (the ingestor owns that rewrite)", rows, want)
	}
}

// After the migration there is exactly one transmission per content hash, it
// is the lowest id (the ingestor keeps the same one), it holds every
// observation once, and every index agrees.
func TestHashMigrate_MergesDuplicatesInMemory_215(t *testing.T) {
	for _, batch := range []int{1, 2, 100} {
		t.Run(fmt.Sprintf("batch%d", batch), func(t *testing.T) {
			d := hm215Create(t)
			d.ballast(t)
			d.stale(t, 2, 3, 2)
			s := hm215Open(t, d)
			hm215Migrate(s, batch)

			s.mu.RLock()
			defer s.mu.RUnlock()
			// 1 ballast + 2 merged groups + 2 singles.
			if len(s.packets) != 5 || len(s.byTxID) != 5 || len(s.byHash) != 5 {
				t.Fatalf("packets/byTxID/byHash = %d/%d/%d, want 5 each", len(s.packets), len(s.byTxID), len(s.byHash))
			}
			if got := len(s.byPayloadType[4]); got != 5 {
				t.Errorf("byPayloadType[ADVERT] holds %d transmissions, want 5 (ballast, 2 merged groups, 2 singles)", got)
			}
			seen := map[string]int{}
			for _, tx := range s.packets {
				seen[tx.Hash]++
				if s.byTxID[tx.ID] != tx || s.byHash[tx.Hash] != tx {
					t.Errorf("tx %d is not what byTxID/byHash hold for it", tx.ID)
				}
				if want := ComputeContentHash(tx.RawHex); tx.Hash != want {
					t.Errorf("tx %d hash = %s, want %s", tx.ID, tx.Hash, want)
				}
			}
			for h, n := range seen {
				if n != 1 {
					t.Errorf("hash %s is held by %d transmissions in s.packets", h, n)
				}
			}
			for g := 1; g <= 2; g++ {
				tx := s.byHash[ComputeContentHash(hm215Raw(g))]
				if tx == nil || tx.ID != g*10+1 {
					t.Fatalf("group %d survivor = %v, want the lowest id %d", g, tx, g*10+1)
				}
				// (obs0,["aa"]) is shared by the 3 rows; each row has one
				// observation of its own observer and one of observer 0 with a
				// path of its own.
				if want := 1 + 3 + 3; len(tx.Observations) != want || tx.ObservationCount != want {
					t.Errorf("group %d survivor holds %d observations (count %d), want %d", g, len(tx.Observations), tx.ObservationCount, want)
				}
				keys := map[string]bool{}
				for _, o := range tx.Observations {
					if o.TransmissionID != tx.ID {
						t.Errorf("observation %d still names tx %d, want %d", o.ID, o.TransmissionID, tx.ID)
					}
					k := o.ObserverID + "|" + o.PathJSON
					if keys[k] {
						t.Errorf("observation %s is held twice", k)
					}
					keys[k] = true
					if s.byObsID[o.ID] != o {
						t.Errorf("byObsID[%d] is not the observation of tx %d", o.ID, tx.ID)
					}
				}
				if int(tx.UniqueObserverCount) != len(tx.observerSet) || len(tx.observerSet) != 4 {
					t.Errorf("group %d survivor has %d unique observers (%d in the set), want 4", g, tx.UniqueObserverCount, len(tx.observerSet))
				}
				for k := 1; k <= 3; k++ {
					if loser := s.byTxID[g*10+k]; k != 1 && loser != nil {
						t.Errorf("duplicate tx %d is still in byTxID", g*10+k)
					}
				}
				pk := acct113PK(100 + g)
				if list := s.byNode[pk]; len(list) != 1 || list[0] != tx {
					t.Errorf("byNode[%s...] = %d entries, want exactly the survivor", pk[:4], len(list))
				}
				if got := s.nodeHashes[pk]; len(got) != 1 || !got[tx.Hash] {
					t.Errorf("nodeHashes[%s...] = %v, want exactly the new hash", pk[:4], got)
				}
				if n := s.advertPubkeys[pk]; n != 1 {
					t.Errorf("advertPubkeys[%s...] = %d, want 1 (one advert transmission)", pk[:4], n)
				}
			}
			for pk, hashes := range s.nodeHashes {
				for h := range hashes {
					if strings.HasPrefix(h, "stale-") {
						t.Errorf("nodeHashes[%s...] still holds the old hash %s", pk[:4], h)
					}
				}
			}
			// The subpath total follows the merge: it counts transmissions with a
			// path of 2+ hops, and a merged-away duplicate no longer counts.
			multiHop := 0
			for _, tx := range s.packets {
				if len(txGetParsedPath(tx)) >= 2 {
					multiHop++
				}
			}
			if s.spTotalPaths != multiHop {
				t.Errorf("spTotalPaths = %d, want %d (transmissions with 2+ hop paths)", s.spTotalPaths, multiHop)
			}
			// Every observation of the DB survives, except the 4 identical
			// (obs0, ["aa"]) ones that merge into 1 per group.
			if want := 2 /*ballast*/ + 2*(1+3+3) + 2; s.totalObs != want {
				t.Errorf("totalObs = %d, want %d", s.totalObs, want)
			}
			if n := len(s.byObsID); n != s.totalObs {
				t.Errorf("byObsID holds %d observations, totalObs = %d", n, s.totalObs)
			}
		})
	}
	t.Run("QueryPackets", func(t *testing.T) {
		d := hm215Create(t)
		d.ballast(t)
		d.stale(t, 2, 3, 2)
		s := hm215Open(t, d)
		if before := s.QueryPackets(PacketQuery{Limit: 100}).Total; before != 1+6+2 {
			t.Fatalf("setup: %d transmissions before the migration, want 9", before)
		}
		hm215Migrate(s, 3)
		if got := s.QueryPackets(PacketQuery{Limit: 100}).Total; got != 5 {
			t.Fatalf("QueryPackets total = %d after the merge, want one per hash (5)", got)
		}
		if got := s.QueryPackets(PacketQuery{Limit: 100, Hash: ComputeContentHash(hm215Raw(1))}).Total; got != 1 {
			t.Fatalf("QueryPackets by hash = %d, want 1", got)
		}
	})
}

// trackedBytes, every per-transmission charge and every index must account for
// exactly what the store holds after the migration.
func TestHashMigrate_AccountingIsExactAfterTheMerge_215(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 3, 3, 3)
	s := hm215Open(t, d)
	acct113Check(t, s, "before the migration") // stale hashes are a different length: charges are stale only after a rehash
	hm215Migrate(s, 2)
	acct113Check(t, s, "after the migration")
}

// After the migration and the eviction of every migrated row the store is
// what it would be had those rows never existed. Compared against a second
// store that never held them, so a double credit cannot hide behind the clamp
// at zero, and a leftover (a node key, a relay record, an advert refcount)
// shows up as a difference.
func TestHashMigrate_EvictionReturnsToBaseline_215(t *testing.T) {
	for _, resolvedIndex := range []bool{true, false} {
		for _, batch := range []int{1, 3, 1000} {
			t.Run(fmt.Sprintf("resolvedIndex=%v/batch%d", resolvedIndex, batch), func(t *testing.T) {
				base := hm215Create(t)
				base.ballast(t)
				baseline := hm215OpenIdx(t, base, resolvedIndex)
				want := hm215Snap(baseline)

				d := hm215Create(t)
				d.ballast(t)
				d.stale(t, 3, 3, 3)
				s := hm215OpenIdx(t, d, resolvedIndex)
				hm215Migrate(s, batch)
				s.mu.Lock()
				s.retentionHours = 24 // the stale rows are 3 days old; the ballast is an hour old
				s.mu.Unlock()
				if n := s.RunEviction(); n != 3+3 {
					t.Fatalf("evicted %d transmissions, want the 3 merged groups and 3 singles (6)", n)
				}
				acct113Check(t, s, "after evicting the migrated rows")
				got := hm215Snap(s)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("store after migrate+evict differs from a store that never had the rows:\n got  %+v\n want %+v", got, want)
				}
				s.mu.RLock()
				defer s.mu.RUnlock()
				if got.TrackedBytes != want.TrackedBytes {
					t.Errorf("trackedBytes = %d, want exactly the baseline %d (drift %d)", got.TrackedBytes, want.TrackedBytes, got.TrackedBytes-want.TrackedBytes)
				}
				if len(s.fallbackByNode) != len(baseline.fallbackByNode) {
					t.Errorf("fallbackByNode holds %d records, baseline %d", len(s.fallbackByNode), len(baseline.fallbackByNode))
				}
			})
		}
	}
}

// Renaming the nodeHashes keys of a batch must cost what the batch holds, not
// what the whole index holds: with the resolved-pubkey index on, the keys of a
// transmission are found through the transmission (its decoded pubkeys, its
// fallback relays, its resolved relays), and no pass over all of nodeHashes
// runs under the store's write lock (#215 review, N2).
func TestHashMigrate_RenameDoesNotWalkTheWholeIndex_215(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 3, 3, 3)
	s := hm215Open(t, d)
	before := hashRekeySweeps.Load()
	hm215Migrate(s, 2)
	if n := hashRekeySweeps.Load() - before; n != 0 {
		t.Errorf("the rename walked the whole of nodeHashes %d times, want 0 with the resolved-pubkey index on", n)
	}
	// With the index off the relays of a transmission are not recorded
	// anywhere, so the one pass per batch stays as the fallback.
	d2 := hm215Create(t)
	d2.ballast(t)
	d2.stale(t, 1, 3, 0)
	s2 := hm215OpenIdx(t, d2, false)
	before = hashRekeySweeps.Load()
	hm215Migrate(s2, 100)
	if n := hashRekeySweeps.Load() - before; n == 0 {
		t.Error("with the resolved-pubkey index off no pass ran: the relay keys would be left under the old hash")
	}
}

// A merge keeps what the duplicate knew: the earliest first_seen (the survivor
// stays where it is in s.packets, so the slice has to be put back in order) and
// the columns the survivor has no value for. The survivor's own values stay.
func TestHashMigrate_MergeKeepsEarliestFirstSeenAndFillsNulls_215(t *testing.T) {
	d := hm215Create(t)
	if _, err := d.conn.Exec(`ALTER TABLE transmissions ADD COLUMN scope_name TEXT`); err != nil {
		t.Fatal(err)
	}
	d.ballast(t)
	raw := hm215Raw(7)
	t0 := time.Now().UTC().Add(-72 * time.Hour)
	at := func(s int) string { return t0.Add(time.Duration(s) * time.Second).Format(time.RFC3339) }
	// 50 is the survivor (lowest id) with the LATER first_seen and no scope;
	// 51 is the duplicate with the earlier one and a scope. A bystander (52)
	// sits between the two in time.
	d.tx(t, 50, raw, "stale-r-50", at(30), 4, hm215Advert(acct113PK(210)))
	d.tx(t, 51, raw, "stale-r-51", at(10), 4, hm215Advert(acct113PK(210)))
	d.tx(t, 52, hm215Raw(8), "stale-r-52", at(20), 4, hm215Advert(acct113PK(211)))
	if _, err := d.conn.Exec(`UPDATE transmissions SET scope_name = '#x' WHERE id = 51`); err != nil {
		t.Fatal(err)
	}
	// Another pair where the survivor has its own scope.
	raw2 := hm215Raw(9)
	d.tx(t, 60, raw2, "stale-r-60", at(40), 4, hm215Advert(acct113PK(212)))
	d.tx(t, 61, raw2, "stale-r-61", at(41), 4, hm215Advert(acct113PK(212)))
	if _, err := d.conn.Exec(`UPDATE transmissions SET scope_name = CASE id WHEN 60 THEN '#keep' ELSE '#other' END WHERE id IN (60, 61)`); err != nil {
		t.Fatal(err)
	}
	s := hm215Open(t, d)
	hm215Migrate(s, 2)

	s.mu.RLock()
	w := s.byTxID[50]
	if w == nil || s.byTxID[51] != nil {
		s.mu.RUnlock()
		t.Fatal("tx 50 must survive and 51 be merged into it")
	}
	if w.FirstSeen != at(10) {
		t.Errorf("survivor first_seen = %s, want the duplicate's earlier %s", w.FirstSeen, at(10))
	}
	if w.ScopeName != "#x" {
		t.Errorf("survivor scope = %q, want %q filled from the duplicate", w.ScopeName, "#x")
	}
	if s.byTxID[60].ScopeName != "#keep" {
		t.Errorf("tx 60 scope = %q, want its own %q kept", s.byTxID[60].ScopeName, "#keep")
	}
	for i := 1; i < len(s.packets); i++ {
		if s.packets[i-1].FirstSeen > s.packets[i].FirstSeen {
			t.Errorf("s.packets is not sorted by first_seen at %d: %s > %s (eviction cuts from the head)", i, s.packets[i-1].FirstSeen, s.packets[i].FirstSeen)
			break
		}
	}
	s.mu.RUnlock()

	// Eviction cuts from the head by first_seen, so it reaches all of them.
	s.mu.Lock()
	s.retentionHours = 24
	s.mu.Unlock()
	if n := s.RunEviction(); n != 3 {
		t.Errorf("retention evicted %d transmissions, want the survivors of both pairs and the bystander (3)", n)
	}
	acct113Check(t, s, "after retention")
}

// The batch was chosen under a read lock: what was evicted or rehashed since is
// dropped when it is applied under the write lock, not resurrected.
func TestHashMigrate_BatchRechecksUnderTheWriteLock_215(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 2, 3, 2)
	s := hm215Open(t, d)
	s.mu.RLock()
	updates := staleContentHashes(s, s.packets)
	s.mu.RUnlock()
	if len(updates) == 0 {
		t.Fatal("setup: nothing stale")
	}
	// Eviction removes every stale row (they are 3 days old).
	s.mu.Lock()
	s.retentionHours = 24
	s.mu.Unlock()
	if n := s.RunEviction(); n != 2*3+2 {
		t.Fatalf("setup: evicted %d, want all %d stale rows", n, 2*3+2)
	}
	s.mu.Lock()
	r, m := s.applyContentHashUpdates(updates)
	s.mu.Unlock()
	if r != 0 || m != 0 {
		t.Errorf("applied %d rehashes and %d merges for transmissions that are gone", r, m)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.byHash) != 1 || len(s.byTxID) != 1 || len(s.packets) != 1 {
		t.Errorf("byHash/byTxID/packets = %d/%d/%d after applying a stale batch, want only the ballast", len(s.byHash), len(s.byTxID), len(s.packets))
	}
}

// The in-memory pass walks a snapshot of s.packets taken when it starts, so
// main must start it after the whole startup load (every chunk, the background
// fill included): in production it started right after the first chunk and saw
// almost none of the store (#215 review, N3).
func TestMain_StartsHashMigrationAfterStartupLoad_215(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	m := string(src)
	done := strings.Index(m, "<-store.StartupLoadDone()")
	start := strings.Index(m, "migrateContentHashesAsync(store")
	if done < 0 || start < 0 {
		t.Fatalf("markers not found: StartupLoadDone wait=%d, migrateContentHashesAsync=%d", done, start)
	}
	if !(done < start) {
		t.Error("migrateContentHashesAsync must start after <-store.StartupLoadDone()")
	}
	if strings.Contains(m[:done], "go migrateContentHashesAsync(") {
		t.Error("the migration is also started before the startup load has finished")
	}
}

// A second run, and a store that was already migrated, change nothing.
func TestHashMigrate_IsIdempotent_215(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 2, 3, 2)
	s := hm215Open(t, d)
	hm215Migrate(s, 4)
	first := hm215Snap(s)
	out := hm215Migrate(s, 4)
	if strings.Contains(out, "Migrated") {
		t.Errorf("a second run migrated again: %s", out)
	}
	if second := hm215Snap(s); !reflect.DeepEqual(first, second) {
		t.Errorf("a second run changed the store:\n first  %+v\n second %+v", first, second)
	}
	if !s.hashMigrationComplete.Load() {
		t.Error("hashMigrationComplete not set")
	}
}

// A late observation of a migrated transmission must find the node index
// already holding it: a stale nodeHashes key would index it a second time.
func TestHashMigrate_RekeysNodeHashesSoLateObservationsDoNotDuplicate_215(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	d.stale(t, 0, 0, 1)
	s := hm215Open(t, d)
	hm215Migrate(s, 10)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.byTxID[501]
	pk := acct113PK(201)
	if tx == nil || len(s.byNode[pk]) != 1 {
		t.Fatalf("setup: byNode[%s...] = %d entries", pk[:4], len(s.byNode[pk]))
	}
	s.addToByNode(tx, pk)
	s.addToByNode(tx, acct113PK(5)) // a relay the observation was resolved through
	if n := len(s.byNode[pk]); n != 1 {
		t.Errorf("a second index call added the migrated tx again: byNode has %d entries", n)
	}
	count := 0
	for _, t2 := range s.byNode[acct113PK(5)] {
		if t2 == tx {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the migrated tx is in byNode[relay] %d times, want 1", count)
	}
}

// perFallbackRelayBytes must stay a real allowance. The charge is derived, not
// measured on its own, so pin it from both sides in the default test run
// (the heap comparison is env-gated, #209 review): at least what a fallback
// relay structurally holds, at most what a resolved relay holds, of which the
// fallback relay is a strict subset.
func TestPerFallbackRelayBytes_IsPinned_215(t *testing.T) {
	// A byNode slot holds a pointer (8). The nodeHashes entry holds a key
	// string header (16) and a bool. The record in fallbackByNode holds the
	// pubkey string header (16).
	const floor = 8 + 16 + 16 + 1
	if got := fallbackRelayBytes(1) - fallbackRelayBytes(0); got < perFallbackRecordBytes+floor {
		t.Errorf("one fallback relay is charged %d bytes, want at least %d", got, perFallbackRecordBytes+floor)
	}
	if got := fallbackRelayBytes(2) - fallbackRelayBytes(1); got != perFallbackRelayBytes || got < floor {
		t.Errorf("a further fallback relay is charged %d bytes, want perFallbackRelayBytes=%d and at least %d", got, perFallbackRelayBytes, floor)
	}
	if perFallbackRelayBytes > perResolvedRelayBytes || perFallbackRecordBytes > perResolvedRecordBytes {
		t.Errorf("a fallback relay (%d+%d) is charged more than a resolved relay (%d+%d), which holds a superset",
			perFallbackRelayBytes, perFallbackRecordBytes, perResolvedRelayBytes, perResolvedRecordBytes)
	}
}
