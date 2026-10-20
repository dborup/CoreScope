package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// #509: retained-history coverage (node activity24h / My Mesh, Regions and the
// #494 analytics coverage) must not turn unknown just because retention or
// eviction removed rows while the startup load was running, and must still
// refuse to claim coverage for rows that were genuinely never loaded.

const coverage509RetentionHours = 48

// coverage509Rows seeds `regular` rows spread over the last 47h (id order =
// FirstSeen order, first_seen == last_seen) after `reheard` long-lived
// transmissions whose FirstSeen is 60h old but whose last_seen falls inside
// the 48h window. The loader selects rows by last_seen, retention evicts by
// FirstSeen: the re-heard rows are loaded and then evicted at the next tick.
func coverage509Rows(t *testing.T, now time.Time, reheard, regular int, reheardLastSeen time.Duration) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "coverage509.db")
	seedTestDBRows(t, dbPath, reheard+regular, 1, func(i int) (string, int64) {
		if i <= reheard {
			return now.Add(-60 * time.Hour).Format(time.RFC3339), now.Add(-reheardLastSeen).Unix()
		}
		step := 47 * time.Hour / time.Duration(regular)
		ts := now.Add(-47*time.Hour + time.Duration(i-reheard-1)*step)
		return ts.Format(time.RFC3339), ts.Unix()
	})
	return dbPath
}

func openCoverage509Store(t *testing.T, dbPath string, cfg *PacketStoreConfig) *PacketStore {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.conn.Close() })
	return NewPacketStore(db, cfg)
}

func coverage509Starts(store *PacketStore, now time.Time) (retained, activity time.Time, analytics NodeAnalyticsCoverage) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.retainedHistoryCoverageStartLocked(now),
		store.nodeActivityCoverageStartLocked(now),
		store.nodeAnalyticsCoverageLocked(2, now)
}

func assertCoverage509Known(t *testing.T, store *PacketStore, now time.Time) {
	t.Helper()
	if !store.backgroundLoadDone.Load() || store.backgroundLoadFailed.Load() {
		t.Fatalf("load did not finish cleanly: done=%v failed=%v err=%q",
			store.backgroundLoadDone.Load(), store.backgroundLoadFailed.Load(), store.BackgroundLoadError())
	}
	retained, activity, _ := coverage509Starts(store, now)
	floor := now.Add(-coverage509RetentionHours * time.Hour)
	if retained.IsZero() || retained.Before(floor) || retained.After(floor.Add(time.Hour)) {
		t.Fatalf("retained coverage start=%v, want known near the retention floor %v (ratio=%.4f)",
			retained, floor, store.LoadCoverageRatio())
	}
	if activity.IsZero() || activity.After(now.Add(-24*time.Hour)) {
		t.Fatalf("activity24h coverage start=%v, want known and before %v", activity, now.Add(-24*time.Hour))
	}
}

// A retention tick between two background chunks evicts the re-heard rows
// the first chunk loaded. They were counted in totalInDB and are gone from
// s.packets, so the old exact-ratio gate stayed unknown until a restart.
func TestCoverage509RetentionEvictionDuringBackgroundLoad(t *testing.T) {
	now := time.Now().UTC()
	dbPath := coverage509Rows(t, now, 5, 200, 30*time.Hour)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: 12})
	chunks := 0
	store.loadChunkScannedHook = func() {
		if chunks++; chunks == 2 {
			if n := store.RunEviction(); n != 5 {
				t.Errorf("retention tick evicted %d rows, want the 5 re-heard rows", n)
			}
		}
	}
	if err := store.RunStartupLoad(50); err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	if ratio := store.LoadCoverageRatio(); ratio >= 1 {
		t.Fatalf("fixture did not reproduce #509: loadCoverageRatio=%.4f", ratio)
	}
	assertCoverage509Known(t, store, now)
	// #494: analytics coverage is not a failed load either, and a retention
	// eviction of 60h-old rows must not shrink it to the 12h hot window.
	_, _, analytics := coverage509Starts(store, now)
	from, err := time.Parse(time.RFC3339Nano, analytics.From)
	if err != nil || analytics.Reason != "available_history" || from.After(now.Add(-46*time.Hour)) {
		t.Fatalf("analytics coverage=%+v, want available_history from ~47h ago", analytics)
	}
}

// A retention tick right before the background count evicts re-heard rows
// that the hot window loaded; they are still counted in totalInDB.
func TestCoverage509RetentionEvictionAtBackgroundLoadStart(t *testing.T) {
	now := time.Now().UTC()
	dbPath := coverage509Rows(t, now, 5, 200, time.Hour)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: 12})
	store.bgLoaderEntryHook = func() { store.RunEviction() }
	if err := store.RunStartupLoad(50); err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	if ratio := store.LoadCoverageRatio(); ratio >= 1 {
		t.Fatalf("fixture did not reproduce #509: loadCoverageRatio=%.4f", ratio)
	}
	assertCoverage509Known(t, store, now)
}

// hotStartupHours=0: the eviction ticker starts at the first chunk, while
// LoadChunked is still loading the rest of the retention window.
func TestCoverage509RetentionEvictionDuringSynchronousLoad(t *testing.T) {
	now := time.Now().UTC()
	dbPath := coverage509Rows(t, now, 5, 200, 30*time.Hour)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours})
	first := true
	store.OnChunkLoaded(func(int, int) {
		if first {
			first = false
			store.RunEviction()
		}
	})
	if err := store.RunStartupLoad(50); err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	if ratio := store.LoadCoverageRatio(); ratio >= 1 {
		t.Fatalf("fixture did not reproduce #509: loadCoverageRatio=%.4f", ratio)
	}
	assertCoverage509Known(t, store, now)
}

// Database retention (the ingestor prunes by first_seen) deletes counted
// rows before the background loader reaches their window. Rows that no
// longer exist are not missing from memory.
func TestCoverage509DatabaseRetentionDuringBackgroundLoad(t *testing.T) {
	now := time.Now().UTC()
	dbPath := coverage509Rows(t, now, 5, 200, 40*time.Hour)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: 12})
	writer, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	chunks := 0
	store.loadChunkScannedHook = func() {
		if chunks++; chunks == 1 {
			// PREFLIGHT: async=true reason="unit-test writer standing in for ingestor retention on an ephemeral DB"
			if _, err := writer.Exec(`DELETE FROM observations WHERE transmission_id <= 5`); err != nil {
				t.Error(err)
			}
			// PREFLIGHT: async=true reason="unit-test writer standing in for ingestor retention on an ephemeral DB"
			if _, err := writer.Exec(`DELETE FROM transmissions WHERE id <= 5`); err != nil {
				t.Error(err)
			}
		}
	}
	if err := store.RunStartupLoad(50); err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	if ratio := store.LoadCoverageRatio(); ratio >= 1 {
		t.Fatalf("fixture did not reproduce #509: loadCoverageRatio=%.4f", ratio)
	}
	assertCoverage509Known(t, store, now)
}

// A memory-cap eviction during the background load removes rows inside the
// retention window; older chunks merged afterwards put older rows back at
// s.packets[0]. Coverage may only start after the newest evicted row.
func TestCoverage509MemoryEvictionDuringBackgroundLoad(t *testing.T) {
	now := time.Now().UTC()
	dbPath := coverage509Rows(t, now, 0, 200, 0)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: 12, MaxMemoryMB: 1})
	var lastEvicted time.Time
	chunks := 0
	store.loadChunkScannedHook = func() {
		if chunks++; chunks != 1 {
			return
		}
		store.mu.Lock()
		before := append([]*StoreTx(nil), store.packets...)
		tracked := store.trackedBytes
		store.trackedBytes = 1 << 30
		store.mu.Unlock()
		if store.RunEviction() == 0 {
			t.Error("memory eviction did not run")
		}
		store.mu.Lock()
		store.trackedBytes = tracked
		for _, tx := range before {
			if store.byTxID[tx.ID] == nil {
				if at, _ := time.Parse(time.RFC3339, tx.FirstSeen); at.After(lastEvicted) {
					lastEvicted = at
				}
			}
		}
		store.mu.Unlock()
	}
	if err := store.RunStartupLoad(50); err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	if lastEvicted.IsZero() {
		t.Fatal("fixture did not record the eviction edge")
	}
	store.mu.RLock()
	first, _ := time.Parse(time.RFC3339, store.packets[0].FirstSeen)
	store.mu.RUnlock()
	if !first.Before(lastEvicted.Add(-time.Hour)) {
		t.Fatalf("fixture did not merge older rows below the eviction edge: first=%v edge=%v", first, lastEvicted)
	}
	retained, _, _ := coverage509Starts(store, now)
	if retained.IsZero() || !retained.After(lastEvicted) {
		t.Fatalf("coverage start=%v, want known and after the newest evicted row %v", retained, lastEvicted)
	}
}

// A row inside the window that the loader never read (an un-backfilled
// last_seen=0 row) is genuinely missing: coverage must stay unknown.
func TestCoverage509UnloadedRowKeepsCoverageUnknown(t *testing.T) {
	now := time.Now().UTC()
	dbPath := filepath.Join(t.TempDir(), "coverage509.db")
	seedTestDBRows(t, dbPath, 200, 1, func(i int) (string, int64) {
		ts := now.Add(-47*time.Hour + time.Duration(i)*14*time.Minute)
		if i == 100 {
			return ts.Format(time.RFC3339), 0
		}
		return ts.Format(time.RFC3339), ts.Unix()
	})
	for _, hot := range []float64{0, 12} {
		store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: hot})
		if err := store.RunStartupLoad(50); err != nil {
			t.Fatalf("RunStartupLoad: %v", err)
		}
		retained, activity, analytics := coverage509Starts(store, now)
		if !retained.IsZero() || !activity.IsZero() {
			t.Fatalf("hot=%g: unloaded row was certified: retained=%v activity=%v", hot, retained, activity)
		}
		if hot > 0 && analytics.Reason != "load_failed" {
			t.Fatalf("hot=%g: analytics certified an incomplete background load: %+v", hot, analytics)
		}
	}
}

// A retention tick between the SQL count and the in-memory count evicts rows
// the SQL count included. The edge moved, so the check is repeated against
// the new edge instead of reporting those rows as missing.
func TestCoverage509EvictionBetweenCountsRetries(t *testing.T) {
	now := time.Now().UTC()
	dbPath := coverage509Rows(t, now, 0, 200, 0)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: 12})
	counted := 0
	var lastEvicted time.Time
	store.retainedHistoryCountedHook = func() {
		if counted++; counted == 1 {
			// Stand-in for a later tick: the oldest rows aged out.
			store.mu.Lock()
			evicted := append([]*StoreTx(nil), store.packets[:4]...)
			store.invalidateNodeAnalyticsEvictionLocked(evicted)
			store.packets = store.packets[4:]
			for _, tx := range evicted {
				delete(store.byTxID, tx.ID)
			}
			store.mu.Unlock()
			lastEvicted, _ = time.Parse(time.RFC3339, evicted[3].FirstSeen)
		}
	}
	if err := store.RunStartupLoad(50); err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	if counted < 2 {
		t.Fatalf("verification was not repeated after the edge moved (%d counts)", counted)
	}
	retained, _, _ := coverage509Starts(store, now)
	if retained.IsZero() || !retained.After(lastEvicted) || retained.After(lastEvicted.Add(time.Hour)) {
		t.Fatalf("coverage start=%v, want known just after the newest evicted row %v", retained, lastEvicted)
	}
}

// Eviction ticks and coverage readers run concurrently with the startup
// load, as in production. Run with -race: the verification takes s.mu only
// around the in-memory reads and never across its SQL count (#495).
func TestCoverage509ConcurrentEvictionAndReaders(t *testing.T) {
	now := time.Now().UTC()
	dbPath := coverage509Rows(t, now, 20, 400, 30*time.Hour)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: 12})
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
				store.RunEviction()
				coverage509Starts(store, now)
			}
		}
	}()
	err := store.RunStartupLoad(25)
	close(done)
	<-stopped
	if err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	// Ticks may land between the two counts often enough to exhaust the
	// retries; the result must then be unknown, never an earlier start.
	if retained, _, _ := coverage509Starts(store, now); !retained.IsZero() &&
		retained.Before(now.Add(-coverage509RetentionHours*time.Hour)) {
		t.Fatalf("coverage start %v precedes the retention floor", retained)
	}
}

// Capped and failed loads stay unknown however the counts compare: a capped
// load omits rows above the store's highest id, and a failed load is
// reported as failed by the #494 analytics coverage as well.
func TestCoverage509EdgeRefusesCappedOrFailedLoad(t *testing.T) {
	now := time.Now().UTC()
	s := &PacketStore{loaded: true, maxTxID: 7, oldestLoaded: now.Add(-time.Hour).Format(time.RFC3339Nano),
		packets: []*StoreTx{{ID: 7, FirstSeen: now.Format(time.RFC3339)}}}
	s.analyticsHistory.evictedFrom = now.Add(-30*time.Minute + time.Nanosecond)
	from, maxID, ok := s.retainedHistoryEdgeLocked(now)
	if !ok || maxID != 7 || !from.Equal(now.Add(-30*time.Minute).Truncate(time.Second).Add(time.Second)) {
		t.Fatalf("edge=%v maxID=%d ok=%v, want the eviction edge rounded up to a whole second", from, maxID, ok)
	}
	s.analyticsHistory.cap = true
	if _, _, ok := s.retainedHistoryEdgeLocked(now); ok {
		t.Fatal("capped load must not be verifiable")
	}
	s.analyticsHistory.cap, s.analyticsHistory.failed = false, true
	if _, _, ok := s.retainedHistoryEdgeLocked(now); ok {
		t.Fatal("failed load must not be verifiable")
	}
}

// coverage509BelowFloorRows seeds the default-config shape (#351 F1): the DB
// keeps more history than the server's retentionHours. `reheard` long-lived
// transmissions have a 60h-old first_seen and a last_seen 30h ago, `plain`
// rows at 55h ago and `edge` rows two minutes either side of the 48h
// retention floor sit in the DB, followed by `regular` rows over the last
// 47h. Only rows with last_seen inside the window are loaded.
func coverage509BelowFloorRows(t *testing.T, now time.Time, reheard, plain, edge, regular int) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "coverage509.db")
	floor := now.Add(-coverage509RetentionHours * time.Hour)
	seedTestDBRows(t, dbPath, reheard+plain+2*edge+regular, 1, func(i int) (string, int64) {
		var ts time.Time
		switch {
		case i <= reheard:
			return now.Add(-60 * time.Hour).Format(time.RFC3339), now.Add(-30 * time.Hour).Unix()
		case i <= reheard+plain:
			ts = now.Add(-55 * time.Hour)
		case i <= reheard+plain+edge:
			ts = floor.Add(-2 * time.Minute)
		case i <= reheard+plain+2*edge:
			ts = floor.Add(2 * time.Minute)
		default:
			ts = now.Add(-47*time.Hour + time.Duration(i-reheard-plain-2*edge-1)*14*time.Minute)
		}
		return ts.Format(time.RFC3339), ts.Unix()
	})
	return dbPath
}

// coverage509InsertTx stands in for the ingestor writing a new transmission
// while the server is starting.
func coverage509InsertTx(t *testing.T, writer *sql.DB, id int, at time.Time) {
	t.Helper()
	// PREFLIGHT: async=true reason="unit-test writer standing in for the ingestor on an ephemeral DB"
	if _, err := writer.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, last_seen) VALUES (?, 'aabb', ?, ?, 0, 4, 1, '{}', ?)`,
		id, fmt.Sprintf("live%06d", id), at.Format(time.RFC3339), at.Unix()); err != nil {
		t.Error(err)
	}
	// PREFLIGHT: async=true reason="unit-test writer standing in for the ingestor on an ephemeral DB"
	if _, err := writer.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp) VALUES (?, ?, 'obs1', 'Obs1', 'RX', -10.0, -80.0, 5, '[]', ?)`,
		1000000+id, id, at.Format(time.RFC3339)); err != nil {
		t.Error(err)
	}
}

// With hotStartupHours=0 the load floor is s.packets[0].FirstSeen, which a
// loaded re-heard row pulls below the retention floor. DB rows between it
// and the floor were never meant to be loaded (their last_seen is outside
// the window) and must not count as missing. No retention tick runs during
// the load, the common case for a fast startup.
func TestCoverage509DBRowsBelowRetentionFloorNoTick(t *testing.T) {
	for _, hot := range []float64{0, 12} {
		now := time.Now().UTC()
		dbPath := coverage509BelowFloorRows(t, now, 5, 3, 0, 200)
		store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: hot})
		if err := store.RunStartupLoad(50); err != nil {
			t.Fatalf("hot=%g: RunStartupLoad: %v", hot, err)
		}
		t.Logf("hot=%g: loadCoverageRatio=%.4f", hot, store.LoadCoverageRatio())
		assertCoverage509Known(t, store, now)
	}
}

// The ingestor keeps writing while the server starts and the poller ingests
// concurrently with the load and with the verification, while DB rows sit
// on both sides of the retention floor. Rows the ingestor wrote after the
// store's highest id are not yet evidence either way: in the DB (not polled
// yet) or in memory (polled between the two counts), they must not turn a
// complete history into a mismatch. Rows just below the floor were never
// loaded and must not count as missing; rows just above it were.
func TestCoverage509ConcurrentIngestAtRetentionEdge(t *testing.T) {
	for _, hot := range []float64{0, 12} {
		now := time.Now().UTC()
		const reheard, plain, edge, regular = 5, 3, 4, 200
		dbPath := coverage509BelowFloorRows(t, now, reheard, plain, edge, regular)
		store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: hot})
		writer, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
		if err != nil {
			t.Fatal(err)
		}
		nextID := reheard + plain + 2*edge + regular
		insertDuringLoad := func() {
			for i := 0; i < 3; i++ {
				nextID++
				coverage509InsertTx(t, writer, nextID, time.Now().UTC())
			}
		}
		// The background fill only reads windows older than the hot
		// window, so rows written during it stay above the store's
		// highest id until the poller picks them up.
		if hot > 0 {
			chunks := 0
			store.loadChunkScannedHook = func() {
				if chunks++; chunks == 1 {
					insertDuringLoad()
				}
			}
		} else {
			first := true
			store.OnChunkLoaded(func(int, int) {
				if first {
					first = false
					insertDuringLoad()
				}
			})
		}
		polled := 0
		store.retainedHistoryCountedHook = func() {
			if polled++; polled != 1 {
				return
			}
			nextID++
			coverage509InsertTx(t, writer, nextID, time.Now().UTC())
			// Poller tick between the SQL and the in-memory count.
			if _, newMax := store.IngestNewFromDB(store.MaxTransmissionID(), 100); newMax != nextID {
				t.Errorf("hot=%g: poller reached id %d, want %d", hot, newMax, nextID)
			}
		}
		if err := store.RunStartupLoad(50); err != nil {
			t.Fatalf("hot=%g: RunStartupLoad: %v", hot, err)
		}
		writer.Close()
		if polled == 0 {
			t.Fatalf("hot=%g: verification never reached the poller tick", hot)
		}
		t.Logf("hot=%g: loadCoverageRatio=%.4f", hot, store.LoadCoverageRatio())
		assertCoverage509Known(t, store, now)
	}
}

// The edge never sits below the retention floor: every row with first_seen
// at or after the floor has last_seen after it too and was selected by the
// load, while older rows were never meant to be in memory.
func TestCoverage509EdgeFlooredAtRetention(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 500, time.UTC)
	s := &PacketStore{loaded: true, maxTxID: 7, retentionHours: 48,
		oldestLoaded: now.Add(-60 * time.Hour).Format(time.RFC3339Nano),
		packets:      []*StoreTx{{ID: 7, FirstSeen: now.Format(time.RFC3339)}}}
	floor := now.Add(-48 * time.Hour)
	from, _, ok := s.retainedHistoryEdgeLocked(now)
	if !ok || !from.Equal(floor.Truncate(time.Second).Add(time.Second)) {
		t.Fatalf("edge=%v ok=%v, want the retention floor %v rounded up", from, ok, floor)
	}
	// An eviction edge above the floor still wins.
	s.analyticsHistory.evictedFrom = now.Add(-47 * time.Hour)
	if from, _, _ := s.retainedHistoryEdgeLocked(now); !from.Equal(now.Add(-47 * time.Hour).Truncate(time.Second).Add(time.Second)) {
		t.Fatalf("edge=%v, want the eviction edge above the floor", from)
	}
	// Unlimited retention has no floor.
	s.retentionHours, s.analyticsHistory.evictedFrom = 0, time.Time{}
	if from, _, _ := s.retainedHistoryEdgeLocked(now); !from.Equal(now.Add(-60 * time.Hour).Truncate(time.Second).Add(time.Second)) {
		t.Fatalf("edge=%v, want the load floor with unlimited retention", from)
	}
}

// coverage509Reheard stands in for the ingestor hearing an existing
// transmission again: a new observation row, then the last_seen bump
// (ingestor InsertTransmission, #1690). last_seen moves to `at`, out of any
// background window that has not been read yet.
func coverage509Reheard(t *testing.T, writer *sql.DB, txID, obsID int, at time.Time) {
	t.Helper()
	// PREFLIGHT: async=true reason="unit-test writer standing in for the ingestor on an ephemeral DB"
	if _, err := writer.Exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp) VALUES (?, ?, 'obs2', 'Obs2', 'RX', -9.0, -79.0, 5, '[]', ?)`,
		obsID, txID, at.Format(time.RFC3339)); err != nil {
		t.Error(err)
	}
	// PREFLIGHT: async=true reason="unit-test writer standing in for the ingestor on an ephemeral DB"
	if _, err := writer.Exec(`UPDATE transmissions SET last_seen = ? WHERE id = ? AND last_seen < ?`, at.Unix(), txID, at.Unix()); err != nil {
		t.Error(err)
	}
}

// The staging configuration (#509 round 3): retentionHours 48,
// hotStartupHours 6, a memory cap and the background fill, while the
// ingestor keeps writing. Retention evicts the re-heard rows the hot window
// loaded, a memory-cap tick evicts the oldest rows mid-load, the poller
// runs, and new transmissions arrive. With reheard, the ingestor also hears
// transmissions again whose last_seen sat in a window not read yet: one
// after the hot cursor passed it, three before the second background
// window. Their last_seen moves to now, out of every window the loader
// will still read, and the poller skips observations of transmissions that
// are not in memory. They must still end up in memory, and coverage must be
// known. The control (no re-hearing) isolates the remaining gates:
// eviction, retention, oldestLoaded at the background start and the 90%
// done gate.
func TestCoverage509StagingShapeBackgroundLoad(t *testing.T) {
	for _, reheard := range []bool{false, true} {
		t.Run(fmt.Sprintf("reheard=%v", reheard), func(t *testing.T) {
			now := time.Now().UTC()
			const reheardOld, regular = 5, 2000
			dbPath := coverage509Rows(t, now, reheardOld, regular, time.Hour)
			store := openCoverage509Store(t, dbPath, &PacketStoreConfig{
				RetentionHours: coverage509RetentionHours, HotStartupHours: 6, MaxMemoryMB: 1})
			writer, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			// Regular row i has first_seen == last_seen ≈ now − 47h + (i−6)·step.
			rowAt := func(hoursAgo float64) int {
				return reheardOld + 1 + int((47-hoursAgo)/47*regular)
			}
			var hearAgain []int
			nextID, nextObs := reheardOld+regular, 5000000
			hear := func(id int) {
				nextObs++
				coverage509Reheard(t, writer, id, nextObs, time.Now().UTC())
				hearAgain = append(hearAgain, id)
			}
			first := true
			store.OnChunkLoaded(func(int, int) {
				if first && reheard {
					first = false
					hear(rowAt(20)) // first background window, behind the hot cursor
				}
			})
			var lastEvicted time.Time
			sinceObs := store.MaxObservationID()
			chunks := 0
			store.loadChunkScannedHook = func() {
				// [now−30h, now−6h) scanned, [now−48h, now−30h) not read yet.
				if chunks++; chunks != 1 {
					return
				}
				if reheard {
					for _, ago := range []float64{40, 38, 33} {
						hear(rowAt(ago))
					}
				}
				for i := 0; i < 3; i++ {
					nextID++
					coverage509InsertTx(t, writer, nextID, time.Now().UTC())
				}
				if n := store.RunEviction(); n != reheardOld {
					t.Errorf("retention tick evicted %d rows, want the %d re-heard rows", n, reheardOld)
				}
				// Poller tick: observations of transmissions that are not in
				// memory are skipped.
				store.IngestNewFromDB(store.MaxTransmissionID(), 100)
				store.IngestNewObservations(sinceObs, 500)
			}
			// A memory-cap tick once the oldest window is merged, when the
			// store is fullest: it lands between the two counts, so the
			// check also repeats against the moved edge.
			counted := 0
			store.retainedHistoryCountedHook = func() {
				if counted++; counted != 1 {
					return
				}
				store.mu.Lock()
				before := append([]*StoreTx(nil), store.packets...)
				tracked := store.trackedBytes
				store.trackedBytes = int64(store.maxMemoryMB)*1048576 + 1
				store.mu.Unlock()
				if store.RunEviction() == 0 {
					t.Error("memory eviction did not run")
				}
				store.mu.Lock()
				store.trackedBytes = tracked
				for _, tx := range before {
					if store.byTxID[tx.ID] == nil {
						if at, _ := time.Parse(time.RFC3339, tx.FirstSeen); at.After(lastEvicted) {
							lastEvicted = at
						}
					}
				}
				store.mu.Unlock()
			}
			if err := store.RunStartupLoad(100); err != nil {
				t.Fatalf("RunStartupLoad: %v", err)
			}
			t.Logf("loadCoverageRatio=%.4f lastEvicted=%v", store.LoadCoverageRatio(), lastEvicted)
			if !store.backgroundLoadDone.Load() || store.backgroundLoadFailed.Load() {
				t.Fatalf("load did not finish cleanly: err=%q", store.BackgroundLoadError())
			}
			floor := now.Add(-coverage509RetentionHours * time.Hour)
			if lastEvicted.Before(floor) || lastEvicted.After(now.Add(-25*time.Hour)) {
				t.Fatalf("fixture: memory eviction edge %v, want inside the window and before 25h ago", lastEvicted)
			}
			store.mu.RLock()
			for _, id := range hearAgain {
				if store.byTxID[id] == nil {
					t.Errorf("re-heard transmission %d is not in memory", id)
				}
			}
			store.mu.RUnlock()
			retained, activity, analytics := coverage509Starts(store, now)
			if retained.IsZero() || !retained.After(lastEvicted) || retained.After(lastEvicted.Add(time.Hour)) {
				t.Fatalf("retained coverage start=%v, want known just after the memory eviction edge %v", retained, lastEvicted)
			}
			if activity.IsZero() || activity.After(now.Add(-24*time.Hour)) {
				t.Fatalf("activity24h coverage start=%v, want known and before %v", activity, now.Add(-24*time.Hour))
			}
			if analytics.Reason == "load_failed" {
				t.Fatalf("analytics coverage=%+v, want a verified load", analytics)
			}
		})
	}
}

// N1 (round 2): the retention floor also bounds the edge on the background
// path. There oldestLoaded ends at the retention floor taken when the
// background fill started, which is just below the floor at verification.
// The ingestor prunes by first_seen, so it can delete rows in that sliver
// while they are still in memory, before a server tick evicts them. They
// were never meant to be covered and must not count as surplus.
func TestCoverage509BackgroundEdgeFlooredAtRetention(t *testing.T) {
	now := time.Now().UTC()
	const regular, sliver = 200, 3
	dbPath := coverage509Rows(t, now, 0, regular, 0)
	store := openCoverage509Store(t, dbPath, &PacketStoreConfig{RetentionHours: coverage509RetentionHours, HotStartupHours: 12})
	writer, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var sliverAt time.Time
	store.bgLoaderEntryHook = func() {
		// Start early in a second, so the background floor taken next is
		// in the same second; the sliver rows sit one second above it.
		if frac := time.Duration(time.Now().Nanosecond()); frac > 500*time.Millisecond {
			time.Sleep(time.Second - frac)
		}
		sliverAt = time.Now().UTC().Truncate(time.Second).Add(time.Second - coverage509RetentionHours*time.Hour)
		for i := 1; i <= sliver; i++ {
			// PREFLIGHT: async=true reason="unit-test writer standing in for the ingestor on an ephemeral DB"
			if _, err := writer.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, last_seen) VALUES (?, 'aabb', ?, ?, 0, 4, 1, '{}', ?)`,
				regular+i, fmt.Sprintf("sliver%d", i), sliverAt.Format(time.RFC3339), sliverAt.Unix()); err != nil {
				t.Error(err)
			}
		}
	}
	chunks := 0
	store.loadChunkScannedHook = func() {
		if chunks++; chunks != 2 { // the oldest window, sliver rows scanned
			return
		}
		// Wait until the retention floor has passed the sliver, then let the
		// ingestor prune it; the scanned rows are still merged.
		for !time.Now().UTC().Add(-coverage509RetentionHours * time.Hour).After(sliverAt) {
			time.Sleep(20 * time.Millisecond)
		}
		// PREFLIGHT: async=true reason="unit-test writer standing in for ingestor retention on an ephemeral DB"
		if _, err := writer.Exec(`DELETE FROM transmissions WHERE id > ?`, regular); err != nil {
			t.Error(err)
		}
	}
	if err := store.RunStartupLoad(50); err != nil {
		t.Fatalf("RunStartupLoad: %v", err)
	}
	store.mu.RLock()
	inMemory := store.byTxID[regular+1] != nil
	store.mu.RUnlock()
	if !inMemory || chunks < 2 {
		t.Fatalf("fixture: sliver row in memory=%v after %d chunks", inMemory, chunks)
	}
	retained, _, _ := coverage509Starts(store, now)
	if retained.IsZero() || !retained.After(sliverAt) {
		t.Fatalf("retained coverage start=%v, want known and above the pruned sliver at %v", retained, sliverAt)
	}
}
