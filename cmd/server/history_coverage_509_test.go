package main

import (
	"database/sql"
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
	from, maxID, ok := s.retainedHistoryEdgeLocked()
	if !ok || maxID != 7 || !from.Equal(now.Add(-30*time.Minute).Truncate(time.Second).Add(time.Second)) {
		t.Fatalf("edge=%v maxID=%d ok=%v, want the eviction edge rounded up to a whole second", from, maxID, ok)
	}
	s.analyticsHistory.cap = true
	if _, _, ok := s.retainedHistoryEdgeLocked(); ok {
		t.Fatal("capped load must not be verifiable")
	}
	s.analyticsHistory.cap, s.analyticsHistory.failed = false, true
	if _, _, ok := s.retainedHistoryEdgeLocked(); ok {
		t.Fatal("failed load must not be verifiable")
	}
}
