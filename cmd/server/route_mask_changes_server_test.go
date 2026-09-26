package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// PR #93 review: an observation that brings a new route bit through an
// upsert of an existing observation row (same observer and path) keeps its
// id, so the poller's new-observation query never sees it. The ingestor logs
// every actual mask change in route_mask_changes, in the same transaction;
// the server applies those rows to transmissions it holds, without a restart
// and without re-creating transmissions it does not hold.

const rmChangesDDL = dbschema.CreateRouteMaskChangesTableSQL

// rmChangesDB is a server test DB with route_mask, the change log and the
// ingestor's observation dedup index. tx 1 "adv" is an ADVERT seen once,
// by observer 1 on path [] with the given route and mask.
func rmChangesDB(t *testing.T, route int, mask int64, raw string) *DB {
	t.Helper()
	db := routeMaskServerDB(t)
	rmExec(t, db, rmChangesDDL)
	rmExec(t, db, dbschema.CreateRouteMaskChangesIndexSQL)
	rmExec(t, db, `CREATE UNIQUE INDEX idx_observations_dedup ON observations(transmission_id, observer_idx, COALESCE(path_json, ''))`)
	fs := time.Now().UTC().Add(-time.Hour)
	rmInsertTx(t, db, 1, "adv", route, mask, fs.Format(time.RFC3339))
	rmInsertObs(t, db, 1, 1, 1, `[]`, raw, fs.Unix())
	return db
}

// rmIngestNewBit does what the ingestor's insertObservationWithRouteBit does
// in one transaction: OR the bit, insert or upsert the observation, and log
// the resulting mask when it changed.
func rmIngestNewBit(t *testing.T, db *DB, txID int, bit int64, observer int, path, raw string) {
	t.Helper()
	if err := rmIngestNewBitErr(db, txID, bit, observer, path, raw); err != nil {
		t.Fatal(err)
	}
}

func rmIngestNewBitErr(db *DB, txID int, bit int64, observer int, path, raw string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE transmissions SET route_mask = route_mask | ? WHERE id = ? AND route_mask IS NOT NULL AND (route_mask & ?) = 0`, bit, txID, bit)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(transmission_id, observer_idx, COALESCE(path_json, '')) DO UPDATE SET raw_hex = COALESCE(excluded.raw_hex, raw_hex)`,
		txID, observer, path, time.Now().Unix(), raw); err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := tx.Exec(`INSERT INTO route_mask_changes (transmission_id, route_mask, created_at)
			SELECT id, route_mask, ? FROM transmissions WHERE id = ?`, time.Now().Unix(), txID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// rmPoller is the production poller driven one tick at a time.
type rmPoller struct {
	p              *Poller
	lastTx, lastOb int
}

func newRMPoller(db *DB, s *PacketStore) *rmPoller {
	p := NewPoller(db, NewHub(), time.Hour)
	p.store = s
	return &rmPoller{p: p, lastTx: s.MaxTransmissionID(), lastOb: s.MaxObservationID()}
}

func (r *rmPoller) tick() { r.lastTx, r.lastOb = r.p.pollStore(r.lastTx, r.lastOb) }

func rmRelayCached(s *PacketStore) bool {
	return s.GetRelayAirtimeShareWithWindow(TimeWindow{})["cached"] == true
}

func TestRouteMaskChanges_SameRowUpsertReachesRunningServer(t *testing.T) {
	for _, tc := range []struct {
		name            string
		route           int
		mask, bit, want int64
		firstRaw, raw   string
		before          string
	}{
		{"flood to mixed", 0, 0b0001, 0b1000, 0b1001, "100000000000", "130000000000", "ADVERT (flood)"},
		{"zero-hop to mixed", 3, 0b1000, 0b0001, 0b1001, "130000000000", "100000000000", "ADVERT (zero-hop)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := rmChangesDB(t, tc.route, tc.mask, tc.firstRaw)
			s := NewPacketStore(db, nil)
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			if got := rmAdvertRows(s); !reflect.DeepEqual(got, map[string]int{tc.before: 1}) {
				t.Fatalf("before: %v", got)
			}
			poll := newRMPoller(db, s)
			poll.tick()

			rmIngestNewBit(t, db, 1, tc.bit, 1, `[]`, tc.raw) // same observer, same path
			var n, id int
			db.conn.QueryRow(`SELECT COUNT(*), MAX(id) FROM observations WHERE transmission_id = 1`).Scan(&n, &id)
			if n != 1 || id != 1 {
				t.Fatalf("setup: observation rows=%d max id=%d, want the one row 1 upserted in place", n, id)
			}

			poll.tick()
			if v := rmSnapshot(s)["adv"]; v != (rmView{uint8(tc.want), true}) {
				t.Fatalf("in-memory mask after one tick = %04b known=%v, want %04b", v.Mask, v.Known, tc.want)
			}
			if got := rmAdvertRows(s); !reflect.DeepEqual(got, map[string]int{"ADVERT (mixed)": 1}) {
				t.Fatalf("Relay Airtime Share after one tick: %v, want ADVERT (mixed)=1", got)
			}
		})
	}
}

// A change row whose mask adds nothing (an identical route, or a change the
// load already read) changes nothing and keeps the cached Relay Airtime
// Share; a real change invalidates it.
func TestRouteMaskChanges_InvalidateOnlyOnRealChange(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	rmAdvertRows(s) // fill the cache
	if !rmRelayCached(s) {
		t.Fatal("setup: relay share not cached")
	}
	rmExec(t, db, `INSERT INTO route_mask_changes (transmission_id, route_mask, created_at) VALUES (1, 2, 1)`)
	poll.tick()
	if !rmRelayCached(s) {
		t.Fatal("a change row that adds no bit invalidated the Relay Airtime Share cache")
	}
	if n := s.RefreshRouteMaskChanges(); n != 0 {
		t.Fatalf("a refresh with nothing new changed %d masks", n)
	}
	rmIngestNewBit(t, db, 1, 0b1000, 1, `[]`, "130000000000") // same row
	poll.tick()
	if rmRelayCached(s) {
		t.Fatal("a real mask change kept the stale cached Relay Airtime Share")
	}
	if got := rmAdvertRows(s); !reflect.DeepEqual(got, map[string]int{"ADVERT (mixed)": 1}) {
		t.Fatalf("after the change: %v", got)
	}
}

// Several changes before the next tick, more than one refresh batch: all are
// applied in order over consecutive ticks and the final mask is complete.
func TestRouteMaskChanges_SeveralChangesAcrossBatches(t *testing.T) {
	prev := routeMaskChangesBatch
	routeMaskChangesBatch = 2
	defer func() { routeMaskChangesBatch = prev }()
	db := rmChangesDB(t, 1, 0b0010, "110000")
	fs := time.Now().UTC().Add(-time.Hour)
	rmInsertTx(t, db, 2, "adv-2", 1, 0b0010, fs.Format(time.RFC3339))
	rmInsertObs(t, db, 2, 2, 1, `[]`, "110000", fs.Unix())
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	// Every bit arrives through an upsert of the same row, so only the change
	// log can carry it.
	rmIngestNewBit(t, db, 1, 0b1000, 1, `[]`, "130000000000")
	rmIngestNewBit(t, db, 1, 0b0001, 1, `[]`, "100000000000")
	rmIngestNewBit(t, db, 2, 0b0100, 1, `[]`, "120000")
	rmIngestNewBit(t, db, 1, 0b0100, 1, `[]`, "120000")
	rmIngestNewBit(t, db, 2, 0b1000, 1, `[]`, "130000000000")
	for i := 0; i < 3; i++ {
		poll.tick()
	}
	snap := rmSnapshot(s)
	if snap["adv"] != (rmView{0b1111, true}) || snap["adv-2"] != (rmView{0b1110, true}) {
		t.Fatalf("after all batches: %v, want adv 1111 and adv-2 1110", snap)
	}
}

// A change for a transmission the server does not hold (evicted, outside its
// load window, or deleted) never re-creates it, and once the start-up load is
// settled the parked change is dropped.
func TestRouteMaskChanges_NeverRecreatesMissingTransmissions(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	fs := time.Now().UTC().Add(-time.Hour)
	rmInsertTx(t, db, 2, "evicted", 1, 0b0010, fs.Format(time.RFC3339))
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	s.mu.Lock()
	delete(s.byTxID, 2) // evicted
	s.mu.Unlock()
	rmIngestNewBit(t, db, 2, 0b1000, 1, `[]`, "130000000000")
	rmExec(t, db, `INSERT INTO route_mask_changes (transmission_id, route_mask, created_at) VALUES (999, 10, 1)`) // deleted tx
	poll.tick()
	s.mu.RLock()
	_, back := s.byTxID[2]
	_, ghost := s.byTxID[999]
	parked := len(s.routeMaskParked)
	s.mu.RUnlock()
	if back || ghost {
		t.Fatalf("refresh re-created a transmission: evicted=%v deleted=%v", back, ghost)
	}
	if parked != 1 {
		t.Fatalf("parked = %d, want only the change of tx 999 (above every id the store has seen, so it may still arrive)", parked)
	}
	// A newer transmission arrives: the store has now seen past 999, so the
	// change of the missing tx 999 is dropped, still without re-creating it.
	rmInsertTx(t, db, 1000, "newer", 1, 0b0010, fs.Format(time.RFC3339))
	poll.tick()
	s.mu.RLock()
	parked = len(s.routeMaskParked)
	_, ghost = s.byTxID[999]
	s.mu.RUnlock()
	if parked != 0 || ghost {
		t.Fatalf("after the store passed tx 999: parked=%d re-created=%v", parked, ghost)
	}
	if n := len(s.packets); n != 3 {
		t.Fatalf("store holds %d packets, want the 2 it loaded plus the new one", n)
	}
}

// Start-up: a change committed before the watermark is covered by the load
// itself; one committed after the watermark is applied (idempotently) by the
// poller; a restart with old changes in the table reads none of them.
func TestRouteMaskChanges_StartupWatermark(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	rmIngestNewBit(t, db, 1, 0b1000, 2, `[]`, "130000000000") // before the server starts
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if v := rmSnapshot(s)["adv"]; v != (rmView{0b1010, true}) {
		t.Fatalf("cold load = %v, want 1010 known", v)
	}
	read := s.routeMaskChangeRowsRead.Load()
	if n := s.RefreshRouteMaskChanges(); n != 0 || s.routeMaskChangeRowsRead.Load() != read {
		t.Fatalf("restart re-read old changes: changed %d, rows read %d", n, s.routeMaskChangeRowsRead.Load()-read)
	}

	// Watermark taken, then a change lands before the load reads the row.
	s2 := NewPacketStore(db, nil)
	s2.initRouteMaskChangeCursor()
	rmIngestNewBit(t, db, 1, 0b0001, 1, `[]`, "100000000000")
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if n := s2.RefreshRouteMaskChanges(); n != 0 {
		t.Fatalf("change already read by the load changed %d masks again", n)
	}
	if got := s2.routeMaskChangeRowsRead.Load(); got != 1 {
		t.Fatalf("rows read after the watermark = %d, want 1", got)
	}
	if v := rmSnapshot(s2)["adv"]; v != (rmView{0b1011, true}) {
		t.Fatalf("after load and refresh = %v, want 1011 known", v)
	}
}

// Chunked start-up: the background loader has scanned a transmission but not
// published it when a change lands and a poller tick runs. The change is
// parked, not dropped, and applied when the transmission is published.
func TestRouteMaskChanges_ParkedUntilTheChunkIsPublished(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	s := NewPacketStore(db, nil)
	s.initRouteMaskChangeCursor()
	s.loadChunkScannedHook = func() {
		rmIngestNewBit(t, db, 1, 0b1000, 1, `[]`, "130000000000")
		rmIngestNewBit(t, db, 1, 0b0001, 2, `[]`, "100000000000")
		s.RefreshRouteMaskChanges()
		s.mu.RLock()
		parked := s.routeMaskParked[1]
		s.mu.RUnlock()
		if parked != 0b1011 {
			t.Errorf("parked mask while the chunk is unpublished = %04b, want 1011", parked)
		}
	}
	if err := s.loadChunk(time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.loadChunkScannedHook = nil
	if v := rmSnapshot(s)["adv"]; v != (rmView{0b1011, true}) {
		t.Fatalf("published transmission = %v, want 1011 known", v)
	}
	s.mu.RLock()
	parked := len(s.routeMaskParked)
	s.mu.RUnlock()
	if parked != 0 {
		t.Fatalf("%d changes still parked after publication", parked)
	}
}

// The backfill refresh (rows loaded while NULL) and the change log are both
// monotonic merges; together they converge on the database value.
func TestRouteMaskChanges_BackfillAndLiveChangeConverge(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	rmExec(t, db, `UPDATE transmissions SET route_mask = NULL WHERE id = 1`)
	rmExec(t, db, `CREATE INDEX idx_tx_route_mask_null ON transmissions(id) WHERE route_mask IS NULL`)
	prev := routeMaskStatusTTL
	routeMaskStatusTTL = 0
	defer func() { routeMaskStatusTTL = prev }()
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	rmExec(t, db, `UPDATE transmissions SET route_mask = 2 WHERE id = 1`) // backfill, no change row
	rmIngestNewBit(t, db, 1, 0b1000, 2, `[]`, "130000000000")             // live change, logged
	poll.tick()
	poll.tick()
	if v := rmSnapshot(s)["adv"]; v != (rmView{0b1010, true}) {
		t.Fatalf("after backfill and live change = %v, want 1010 known", v)
	}
	if st := s.routeMaskBackfillStatus(); st.Status != "complete" {
		t.Fatalf("status = %+v, want complete", st)
	}
}

// Ingest and poll concurrently: after the writer stops and one more tick,
// every in-memory mask equals the database.
func TestRouteMaskChanges_ConcurrentIngestAndPoll(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	fs := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	const n = 40
	for i := 2; i <= n; i++ {
		rmInsertTx(t, db, i, fmt.Sprintf("adv-%d", i), 1, 0b0010, fs)
		rmInsertObs(t, db, i, i, 1, `[]`, "110000", time.Now().Unix())
	}
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 1; i <= n; i++ {
			for _, err := range []error{
				rmIngestNewBitErr(db, i, 0b1000, 1, `[]`, "130000000000"),
				rmIngestNewBitErr(db, i, 0b0001, 1, `[]`, "100000000000"),
			} {
				if err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			poll.tick()
		}
	}
	wg.Wait()
	for i := 0; i < 1+2*n/routeMaskChangesBatch; i++ {
		poll.tick()
	}
	cold := NewPacketStore(db, nil)
	if err := cold.Load(); err != nil {
		t.Fatal(err)
	}
	if live, want := rmSnapshot(s), rmSnapshot(cold); !reflect.DeepEqual(live, want) {
		t.Fatalf("live view differs from a cold load after concurrent ingest")
	}
	if v := rmSnapshot(s)["adv"]; v != (rmView{0b1011, true}) {
		t.Fatalf("adv = %v, want 1011", v)
	}
}

// Without the table (test databases; AssertReady requires it in production)
// the refresh is a no-op.
func TestRouteMaskChanges_NoTableIsNoOp(t *testing.T) {
	db := routeMaskServerDB(t)
	rmSeed(t, db)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if n := s.RefreshRouteMaskChanges(); n != 0 {
			t.Fatalf("refresh without the table changed %d", n)
		}
	}
}

// In production the poller starts only after the first chunk is loaded. A
// change committed between the load and the poller's first tick must still
// be applied: the watermark comes from before the load, not from the first
// tick.
func TestRouteMaskChanges_ChangeBeforeFirstTickIsApplied(t *testing.T) {
	for _, load := range []struct {
		name string
		fn   func(*PacketStore) error
	}{
		{"Load", func(s *PacketStore) error { return s.Load() }},
		{"LoadChunked", func(s *PacketStore) error { return s.LoadChunked(1) }},
	} {
		t.Run(load.name, func(t *testing.T) {
			db := rmChangesDB(t, 1, 0b0010, "110000")
			s := NewPacketStore(db, nil)
			if err := load.fn(s); err != nil {
				t.Fatal(err)
			}
			rmIngestNewBit(t, db, 1, 0b1000, 1, `[]`, "130000000000") // same row
			newRMPoller(db, s).tick()
			if v := rmSnapshot(s)["adv"]; v != (rmView{0b1010, true}) {
				t.Fatalf("after the first tick = %04b known=%v, want 1010", v.Mask, v.Known)
			}
		})
	}
}

// A background fill that does not reach full coverage never sets
// backgroundLoadDone, but the start-up load is still over: changes for
// transmissions the store will never hold must not stay parked forever.
func TestRouteMaskChanges_ParkedDroppedAfterPartialStartupLoad(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	rmExec(t, db, `UPDATE transmissions SET first_seen = ? WHERE id = 1`, time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339))
	old := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	for i := 2; i <= 19; i++ {
		rmInsertTx(t, db, i, fmt.Sprintf("old-%d", i), 1, 0b0010, old)
	}
	rmInsertTx(t, db, 20, "recent", 1, 0b0010, time.Now().UTC().Add(-30*time.Minute).Format(time.RFC3339))
	s := NewPacketStore(db, nil)
	s.hotStartupHours, s.retentionHours = 1, 2
	// A background chunk fails (its query cannot run), as a real chunk error
	// would: the fill is partial and backgroundLoadDone stays false.
	s.bgLoaderEntryHook = func() { rmExec(t, db, `ALTER TABLE observations RENAME TO observations_hidden`) }
	if err := s.RunStartupLoad(1); err != nil {
		t.Fatal(err)
	}
	rmExec(t, db, `ALTER TABLE observations_hidden RENAME TO observations`)
	if s.backgroundLoadDone.Load() || !s.backgroundLoadFailed.Load() {
		t.Fatalf("setup: want a partial background load (done=%v failed=%v)", s.backgroundLoadDone.Load(), s.backgroundLoadFailed.Load())
	}
	poll := newRMPoller(db, s)
	rmIngestNewBit(t, db, 5, 0b1000, 1, `[]`, "130000000000") // tx 5 is outside the loaded window
	poll.tick()
	s.mu.RLock()
	parked := len(s.routeMaskParked)
	_, back := s.byTxID[5]
	s.mu.RUnlock()
	if parked != 0 || back {
		t.Fatalf("after a partial start-up load: parked=%d re-created=%v, want the change dropped", parked, back)
	}
}

// An older transmission the background loader is still bringing in, below
// ids the store already holds, keeps its change parked until it is published.
func TestRouteMaskChanges_OlderChunkBelowMaxIDKeepsItsChange(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	rmExec(t, db, `UPDATE transmissions SET first_seen = ? WHERE id = 1`, time.Now().UTC().Add(-5*time.Hour).Format(time.RFC3339))
	rmInsertTx(t, db, 2, "recent", 1, 0b0010, time.Now().UTC().Add(-30*time.Minute).Format(time.RFC3339))
	s := NewPacketStore(db, nil)
	s.hotStartupHours = 1
	if err := s.LoadChunked(1); err != nil {
		t.Fatal(err)
	}
	if s.MaxTransmissionID() != 2 {
		t.Fatalf("setup: max tx id %d, want 2", s.MaxTransmissionID())
	}
	s.loadChunkScannedHook = func() {
		rmIngestNewBit(t, db, 1, 0b1000, 1, `[]`, "130000000000")
		s.RefreshRouteMaskChanges()
	}
	if err := s.loadChunk(time.Now().Add(-6*time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.loadChunkScannedHook = nil
	if v := rmSnapshot(s)["adv"]; v != (rmView{0b1010, true}) {
		t.Fatalf("older transmission published as %04b known=%v, want 1010", v.Mask, v.Known)
	}
}

// If the watermark cannot be read at load time, the server must not jump to
// the end of the log on its first tick: it reads from the start instead.
func TestRouteMaskChanges_WatermarkReadErrorReadsFromTheStart(t *testing.T) {
	db := rmChangesDB(t, 1, 0b0010, "110000")
	rmExec(t, db, `DROP TABLE route_mask_changes`)
	rmExec(t, db, `CREATE TABLE route_mask_changes (x INTEGER)`) // MAX(id) fails: no such column
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	rmExec(t, db, `DROP TABLE route_mask_changes`)
	rmExec(t, db, rmChangesDDL)
	rmIngestNewBit(t, db, 1, 0b1000, 1, `[]`, "130000000000")
	newRMPoller(db, s).tick()
	if v := rmSnapshot(s)["adv"]; v != (rmView{0b1010, true}) {
		t.Fatalf("after a failed watermark read = %04b known=%v, want 1010", v.Mask, v.Known)
	}
}

// rmFileDBs copies an in-memory test DB to a WAL file and returns a server
// view and a separate writer on it, so a test can commit a change while a
// load holds its read cursor open.
func rmFileDBs(t *testing.T, mem *DB) (server, writer *DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rm.db")
	rmExec(t, mem, `VACUUM INTO '`+path+`'`)
	open := func() *sql.DB {
		c, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	server = &DB{conn: open()}
	for _, f := range []*schemaFlag{&server.isV3Flag, &server.hasResolvedPathFlag, &server.hasRouteMaskFlag, &server.hasObsRawHexFlag} {
		f.forceTrue()
	}
	w := open()
	w.SetMaxOpenConns(1)
	return server, &DB{conn: w}
}

// The watermark is taken before the load reads any transmission: a change
// committed while the load is reading (after the row was read) is applied on
// the next tick, for both load paths.
func TestRouteMaskChanges_WatermarkTakenBeforeTheLoadReads(t *testing.T) {
	for _, load := range []struct {
		name string
		fn   func(*PacketStore) error
	}{
		{"Load", func(s *PacketStore) error { return s.Load() }},
		{"LoadChunked", func(s *PacketStore) error { return s.LoadChunked(1) }},
	} {
		t.Run(load.name, func(t *testing.T) {
			server, writer := rmFileDBs(t, rmChangesDB(t, 1, 0b0010, "110000"))
			s := NewPacketStore(server, nil)
			fired := false
			s.loadScannedRowHook = func() {
				fired = true
				rmIngestNewBit(t, writer, 1, 0b1000, 1, `[]`, "130000000000")
			}
			if err := load.fn(s); err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("setup: the load hook did not run")
			}
			if v := rmSnapshot(s)["adv"]; v.Mask != 0b0010 {
				t.Fatalf("setup: the load saw the change (mask %04b); the hook must run after the row was read", v.Mask)
			}
			newRMPoller(server, s).tick()
			if v := rmSnapshot(s)["adv"]; v != (rmView{0b1010, true}) {
				t.Fatalf("after the first tick = %04b known=%v, want 1010", v.Mask, v.Known)
			}
		})
	}
}
