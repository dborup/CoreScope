package main

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const coverage485PK = "c048500000000000000000000000000000000000000000000000000000000000"

func TestNodeAnalyticsCoverage485_RequestedMonthDoesNotDiluteRetainedTwoDays(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, 'Coverage test', 'repeater')`, coverage485PK); err != nil {
		t.Fatal(err)
	}
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 48})
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store.loaded = true
	store.oldestLoaded = now.Add(-48 * time.Hour).Format(time.RFC3339)
	store.analyticsHistory = nodeAnalyticsHistory{known: true, from: now.Add(-48 * time.Hour)}
	for h := 0; h < 48; h++ {
		tx := &StoreTx{ID: h + 1, Hash: fmt.Sprintf("coverage-%d", h), FirstSeen: now.Add(-48*time.Hour + time.Duration(h)*time.Hour).Format(time.RFC3339)}
		store.byNode[coverage485PK] = append(store.byNode[coverage485PK], tx)
		store.packets = append(store.packets, tx)
	}
	full, err := store.getNodeAnalyticsAt(coverage485PK, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if full.ComputedStats.AvgPacketsPerDay != 24 {
		t.Fatalf("packets/day=%v, want 24 over the supported two days, not the requested month", full.ComputedStats.AvgPacketsPerDay)
	}
	if full.ComputedStats.AvailabilityPct != 100 {
		t.Fatalf("availability=%v, want 100 for all 48 supported hour bins", full.ComputedStats.AvailabilityPct)
	}
}

func coverage485Store(t *testing.T, now time.Time, floor time.Time) *PacketStore {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, 'Coverage test', 'repeater')`, coverage485PK); err != nil {
		t.Fatal(err)
	}
	s := NewPacketStore(db, nil)
	s.loaded = true
	s.analyticsHistory = nodeAnalyticsHistory{known: !floor.IsZero(), from: floor}
	return s
}

func coverage485Packet(s *PacketStore, first string, observations ...string) {
	id := len(s.packets) + 1
	tx := &StoreTx{ID: id, Hash: fmt.Sprintf("cov-%d", id), FirstSeen: first}
	for _, at := range observations {
		tx.Observations = append(tx.Observations, &StoreObs{Timestamp: at})
	}
	s.packets = append(s.packets, tx)
	s.byNode[coverage485PK] = append(s.byNode[coverage485PK], tx)
}

func coverage485Pair(t *testing.T, s *PacketStore, days int, now time.Time) *NodeAnalyticsResponse {
	t.Helper()
	f, err := s.getNodeAnalyticsAt(coverage485PK, days, now)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.getNodeAnalyticsSummaryAt(coverage485PK, days, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.ComputedStats, l.ComputedStats) || f.Coverage != l.Coverage || f.TimeRange != l.TimeRange {
		t.Fatalf("full/summary differ: %+v / %+v", f, l)
	}
	if math.IsNaN(f.ComputedStats.AvailabilityPct) || math.IsInf(f.ComputedStats.AvgPacketsPerDay, 0) {
		t.Fatal("non-finite rates")
	}
	return f
}

func TestNodeAnalyticsCoverage485_BoundariesUTCAndPartialHour(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 15, 0, 0, time.FixedZone("east", 2*3600)) // 10:15 UTC
	floor := now.UTC().Add(-90 * time.Minute)                                    // 08:45
	s := coverage485Store(t, now, floor)
	for _, raw := range []string{"2026-10-16T08:44:59Z", "2026-10-16T10:45:00+02:00", "2026-10-16T09:59:00Z", "2026-10-16T10:15:00Z", "2026-10-16T10:15:01Z", "malformed"} {
		coverage485Packet(s, raw)
	}
	f := coverage485Pair(t, s, 30, now)
	if f.ComputedStats.TotalPackets != 3 || f.ComputedStats.AvgPacketsPerDay != 48 || f.ComputedStats.AvailabilityPct != 100 {
		t.Fatalf("stats=%+v", f.ComputedStats)
	}
	if f.Coverage.From != "2026-10-16T08:45:00Z" || f.Coverage.Seconds != 5400 || !f.Coverage.Partial {
		t.Fatalf("coverage=%+v", f.Coverage)
	}
	if f.TimeRange.To != "2026-10-16T10:15:00Z" {
		t.Fatalf("non UTC range=%+v", f.TimeRange)
	}
	for _, b := range f.ActivityTimeline {
		at, _ := time.Parse(time.RFC3339Nano, *b.Bucket)
		if at.Before(floor) {
			t.Fatalf("bucket precedes supported history: %v", b)
		}
	}
}

func TestNodeAnalyticsCoverage485_CompleteAndInactiveNode(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := coverage485Store(t, now, now.Add(-48*time.Hour))
	f := coverage485Pair(t, s, 1, now)
	if f.Coverage.Partial || f.Coverage.Seconds != 86400 || f.ComputedStats.TotalPackets != 0 || f.ComputedStats.AvailabilityPct != 0 {
		t.Fatalf("complete empty node=%+v", f)
	}
}

func TestNodeAnalyticsCoverage485_EmptyLoadingCappedFailedAndZeroDuration(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		history nodeAnalyticsHistory
		loaded  bool
	}{
		{"empty", nodeAnalyticsHistory{}, true},
		{"loading", nodeAnalyticsHistory{loading: true}, false},
		{"capped", nodeAnalyticsHistory{known: true, from: now, cap: true, reason: "load_capped"}, true},
		{"failed", nodeAnalyticsHistory{known: true, from: now, failed: true, reason: "load_failed"}, true},
		{"future", nodeAnalyticsHistory{known: true, from: now.Add(time.Second)}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := coverage485Store(t, now, time.Time{})
			s.analyticsHistory = tt.history
			s.loaded = tt.loaded
			coverage485Packet(s, now.Add(-time.Hour).Format(time.RFC3339))
			f := coverage485Pair(t, s, 7, now)
			if !f.Coverage.Partial || f.Coverage.Seconds != 0 || f.ComputedStats.TotalPackets != 0 || f.ComputedStats.AvailabilityPct != 0 || f.ComputedStats.AvgPacketsPerDay != 0 {
				t.Fatalf("unknown/zero history=%+v", f)
			}
		})
	}
}

func TestNodeAnalyticsCoverage485_ReheardOldDoesNotProveOldHistory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := coverage485Store(t, now, time.Time{})
	coverage485Packet(s, now.Add(-365*24*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
	coverage485Packet(s, now.Add(-30*time.Minute).Format(time.RFC3339), now.Add(-30*time.Minute).Format(time.RFC3339))
	s.beginNodeAnalyticsLoadLocked(now.Add(-30 * 24 * time.Hour).Format(time.RFC3339))
	s.analyticsHistory.observedFrom = now.Add(-time.Hour)
	s.finishNodeAnalyticsLoadLocked(s.analyticsHistory.loadCutoff, false, false)
	f := coverage485Pair(t, s, 30, now)
	if f.Coverage.Seconds != 3600 || f.ComputedStats.TotalPackets != 1 || f.ComputedStats.AvgPacketsPerDay != 24 {
		t.Fatalf("reheard=%+v", f)
	}
}

func TestNodeAnalyticsCoverage485_LoadLifecycleShortDBAndEviction(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := coverage485Store(t, now, time.Time{})
	coverage485Packet(s, now.Add(-40*24*time.Hour).Format(time.RFC3339), now.Add(-24*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
	coverage485Packet(s, now.Add(-time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
	s.beginNodeAnalyticsLoadLocked(now.Add(-30 * 24 * time.Hour).Format(time.RFC3339))
	s.analyticsHistory.observedFrom = now.Add(-24 * time.Hour)
	s.finishNodeAnalyticsLoadLocked(s.analyticsHistory.loadCutoff, false, true)
	f := coverage485Pair(t, s, 30, now)
	if !f.Coverage.Loading || f.Coverage.Seconds != 86400 {
		t.Fatalf("short DB=%+v", f.Coverage)
	}
	s.analyticsHistory.loading = false
	s.invalidateNodeAnalyticsEvictionLocked(s.packets[:1])
	// The evicted tx was first seen 40 days ago; its late observations must
	// not move the edge (F1). Coverage stays at the 24 h loaded edge.
	f = coverage485Pair(t, s, 30, now)
	if f.Coverage.Reason != "memory_eviction" || f.Coverage.Seconds != 86400 || f.ComputedStats.TotalPackets != 1 {
		t.Fatalf("reheard eviction=%+v", f)
	}
	// A later background completion cannot undo the invalidated edge.
	s.finishNodeAnalyticsLoadLocked(now.Add(-30*24*time.Hour), false, false)
	if s.analyticsHistory.from.Before(now.Add(-24 * time.Hour)) {
		t.Fatal("background completion restored evicted coverage")
	}
}

func TestNodeAnalyticsCoverage485_EvictingReheardOldTxDoesNotJumpFrom(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	floor := now.Add(-72 * time.Hour)
	s := coverage485Store(t, now, floor)
	// Old tx first seen at the edge, re-heard ~2 days later (production max).
	coverage485Packet(s, floor.Add(time.Minute).Format(time.RFC3339), floor.Add(time.Minute).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), now.Add(-30*time.Minute).Format(time.RFC3339))
	coverage485Packet(s, now.Add(-10*time.Hour).Format(time.RFC3339), now.Add(-10*time.Hour).Format(time.RFC3339))
	s.invalidateNodeAnalyticsEvictionLocked(s.packets[:1])
	want := floor.Add(time.Minute).Add(time.Nanosecond)
	if got := s.analyticsHistory.from; !got.Equal(want) {
		t.Fatalf("from=%v want eviction boundary %v (late observation must not move it)", got, want)
	}
	cov := s.nodeAnalyticsCoverageLocked(30, now)
	if cov.LateEvictedObservations != 2 {
		t.Fatalf("late evicted observations=%d want 2", cov.LateEvictedObservations)
	}
	if want := now.Sub(floor.Add(time.Minute)).Seconds(); cov.Seconds < want-1 || cov.Seconds > want {
		t.Fatalf("seconds=%v want ~%v", cov.Seconds, want)
	}
}

func TestNodeAnalyticsCoverage485_InitiallyEmptyGainsLiveEvidence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := coverage485Store(t, now, time.Time{})
	s.observeNodeAnalyticsLiveLocked("invalid")
	if s.analyticsHistory.known {
		t.Fatal("invalid timestamp proved history")
	}
	s.observeNodeAnalyticsLiveLocked(now.Add(-time.Minute).Format(time.RFC3339))
	if !s.analyticsHistory.known || s.nodeAnalyticsCoverageLocked(1, now).Seconds != 60 {
		t.Fatalf("first live evidence=%+v", s.analyticsHistory)
	}
}

func TestNodeAnalyticsCoverage485_LiveEvidenceSurvivesEmptyBackgroundHandoff(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := coverage485Store(t, now, time.Time{})
	s.beginNodeAnalyticsLoadLocked(now.Add(-48 * time.Hour).Format(time.RFC3339))
	s.finishNodeAnalyticsLoadLocked(s.analyticsHistory.loadCutoff, false, true)
	at := now.Add(-time.Minute)
	coverage485Packet(s, at.Format(time.RFC3339), at.Format(time.RFC3339))
	s.observeNodeAnalyticsLiveLocked(at.Format(time.RFC3339))
	if !s.nodeAnalyticsCoverageLocked(1, now).Loading {
		t.Fatal("unfinished background load claimed completion")
	}
	s.finishNodeAnalyticsLoadLocked(s.analyticsHistory.loadCutoff, false, false)
	f := coverage485Pair(t, s, 1, now)
	if f.Coverage.Loading || f.Coverage.Seconds != 60 || f.ComputedStats.TotalPackets != 1 || !s.analyticsHistory.known {
		t.Fatalf("accepted live observation was lost at handoff: %+v", f)
	}
}

func BenchmarkNodeAnalyticsCoverage485Snapshot(b *testing.B) {
	now := time.Now().UTC()
	s := &PacketStore{loaded: true, analyticsHistory: nodeAnalyticsHistory{known: true, from: now.Add(-48 * time.Hour)}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = s.nodeAnalyticsCoverageLocked(30, now)
	}
}

func TestNodeAnalyticsCoverage485_RealStartupLoadersUseObservationEvidence(t *testing.T) {
	for _, mode := range []string{"legacy", "chunked", "background"} {
		t.Run(mode, func(t *testing.T) {
			db := setupTestDB(t)
			t.Cleanup(func() { db.Close() })
			seedTestData(t, db)
			if _, err := db.conn.Exec(`ALTER TABLE transmissions ADD COLUMN last_seen INTEGER; UPDATE transmissions SET last_seen=COALESCE((SELECT MAX(timestamp) FROM observations o WHERE o.transmission_id=transmissions.id),0); UPDATE transmissions SET first_seen='2020-01-01T00:00:00Z' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			db.hasLastSeenFlag.forceTrue()
			cfg := &PacketStoreConfig{RetentionHours: 30 * 24}
			if mode == "background" {
				cfg.HotStartupHours = 2
			}
			s := NewPacketStore(db, cfg)
			if mode == "legacy" {
				if err := s.Load(); err != nil {
					t.Fatal(err)
				}
			} else {
				s.OnChunkLoaded(func(_, _ int) {
					s.mu.RLock()
					c := s.nodeAnalyticsCoverageLocked(30, time.Now())
					s.mu.RUnlock()
					if !c.Partial || !c.Loading {
						t.Error("in-flight loader claimed complete coverage")
					}
				})
				if err := s.RunStartupLoad(1); err != nil {
					t.Fatal(err)
				}
			}
			if !s.WaitIndexesReady(5 * time.Second) {
				t.Fatal("indexes not ready")
			}
			now := time.Now().UTC()
			full, err := s.getNodeAnalyticsAt(nodeAnalyticsSummaryTestPK, 30, now)
			if err != nil {
				t.Fatal(err)
			}
			if !full.Coverage.Partial || full.Coverage.Seconds < 23*3600 || full.Coverage.Seconds > 25*3600 || full.ComputedStats.TotalPackets != 1 {
				t.Fatalf("%s coverage=%+v stats=%+v", mode, full.Coverage, full.ComputedStats)
			}
			if full.Coverage.Loading {
				t.Fatal("finished loader still reports loading")
			}
		})
	}
}

func TestNodeAnalyticsCoverage485_BudgetOnlyCapsWhenRowsOmitted(t *testing.T) {
	for _, mode := range []string{"legacy", "chunked"} {
		for _, n := range []int{1000, 1001} {
			t.Run(fmt.Sprintf("%s/%d", mode, n), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "coverage.db")
				createTestDBAt(t, path, n)
				db, err := OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				s := NewPacketStore(db, &PacketStoreConfig{MaxMemoryMB: 1})
				if mode == "legacy" {
					err = s.Load()
				} else {
					err = s.LoadChunked(1000)
				}
				if err != nil {
					t.Fatal(err)
				}
				if !s.WaitIndexesReady(5 * time.Second) {
					t.Fatal("indexes not ready")
				}
				wantCap := n > 1000
				if s.analyticsHistory.cap != wantCap {
					t.Fatalf("loaded=%d/%d cap=%v want%v", len(s.packets), n, s.analyticsHistory.cap, wantCap)
				}
				if !wantCap && s.nodeAnalyticsCoverageLocked(365, time.Now()).Seconds < 24*3600 {
					t.Fatal("complete 1000-row history was discarded")
				}
			})
		}
	}
}

func TestNodeAnalyticsCoverage485_ExcludedRetentionRowsAreNotMemoryCap(t *testing.T) {
	for _, mode := range []string{"legacy", "chunked"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "retained.db")
			createTestDBAt(t, path, 1001)
			db, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			recent := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
			writer, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = writer.Exec(`UPDATE transmissions SET first_seen=?; UPDATE observations SET timestamp=?; UPDATE transmissions SET first_seen='2020-01-01T00:00:00Z' WHERE id=1001; UPDATE observations SET timestamp='2020-01-01T00:00:00Z' WHERE transmission_id=1001`, recent, recent)
			writer.Close()
			if err != nil {
				t.Fatal(err)
			}
			s := NewPacketStore(db, &PacketStoreConfig{MaxMemoryMB: 1, RetentionHours: 24})
			if mode == "legacy" {
				err = s.Load()
			} else {
				err = s.LoadChunked(1000)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !s.WaitIndexesReady(5 * time.Second) {
				t.Fatal("indexes not ready")
			}
			if len(s.packets) != 1000 || s.analyticsHistory.cap {
				t.Fatalf("retained population incorrectly capped: loaded=%d history=%+v", len(s.packets), s.analyticsHistory)
			}
			if s.nodeAnalyticsCoverageLocked(30, time.Now()).Seconds < 3599 {
				t.Fatal("eligible historical interval discarded")
			}
		})
	}
}

// F2: the canonical-UTC fast path must be indistinguishable from the
// time.Parse fallback for every input shape, valid or not.
func TestParseNodeAnalyticsTime485_FastPathMatchesFallback(t *testing.T) {
	slow := func(raw string) (time.Time, bool) {
		tm, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			tm, err = time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC)
		}
		return tm.UTC(), err == nil && tm.Unix() > 0
	}
	for _, raw := range []string{
		"2026-10-16T12:00:00Z", "2024-02-29T23:59:59Z", "2025-02-29T00:00:00Z",
		"2026-13-01T00:00:00Z", "2026-00-10T00:00:00Z", "2026-04-31T00:00:00Z",
		"2026-10-16T24:00:00Z", "2026-10-16T12:60:00Z", "2026-10-16T12:00:60Z",
		"2026-10-16T12:00:00z", "2026-10-16T12:00:0aZ", "2026-10-16 12:00:00",
		"2026-10-16T12:00:00+02:00", "2026-10-16T12:00:00.123456789Z",
		"1970-01-01T00:00:00Z", "1969-12-31T23:59:59Z", "0000-01-01T00:00:00Z",
		"2026-10-16T12:00:00Z ", "", "x", "2026-10-16T12:00:00ZZ", "2026+10-16T12:00:00Z",
	} {
		gt, gok := parseNodeAnalyticsTime(raw)
		wt, wok := slow(raw)
		if gok != wok || (gok && !gt.Equal(wt)) {
			t.Errorf("%q: got (%v,%v) want (%v,%v)", raw, gt, gok, wt, wok)
		}
	}
}

// #504 item 2: pin every separator position of the fast-path parser. Dropping
// any single separator check (mutant M7 dropped raw[13]) must change the
// result relative to the time.Parse fallback.
func TestParseNodeAnalyticsTime504_EverySeparatorPositionMatchesFallback(t *testing.T) {
	const canonical = "2026-10-16T12:30:45Z"
	slow := func(raw string) (time.Time, bool) {
		tm, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			tm, err = time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC)
		}
		return tm.UTC(), err == nil && tm.Unix() > 0
	}
	for _, pos := range []int{4, 7, 10, 13, 16, 19} {
		for _, c := range []byte{'X', ' ', '-', ':', 'T', 'Z', '0', '.'} {
			if canonical[pos] == c {
				continue
			}
			raw := []byte(canonical)
			raw[pos] = c
			if _, ok := parseCanonicalUTCSeconds(string(raw)); ok {
				t.Errorf("fast path accepted %q (separator %d replaced by %q)", raw, pos, c)
			}
			gt, gok := parseNodeAnalyticsTime(string(raw))
			wt, wok := slow(string(raw))
			if gok != wok || (gok && !gt.Equal(wt)) {
				t.Errorf("%q: got (%v,%v) want (%v,%v)", raw, gt, gok, wt, wok)
			}
		}
	}
}

// #504 item 1: late_evicted_observations counts only observations heard more
// than lateEvictedObservationThreshold after the evicted tx's FirstSeen.
// Ordinary second-observer hearings seconds later must not count.
func TestNodeAnalyticsCoverage504_LateEvictedCountsOnlyGenuinelyLate(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	first := now.Add(-60 * time.Hour)
	s := coverage485Store(t, now, now.Add(-72*time.Hour))
	f := func(d time.Duration) string { return first.Add(d).Format(time.RFC3339) }
	coverage485Packet(s, f(0),
		f(0), f(2*time.Second), f(30*time.Second), f(10*time.Minute), // normal multi-observer hearings
		f(time.Hour),                     // exactly at the threshold: not late
		f(time.Hour+time.Second),         // late
		f(30*time.Hour), f(48*time.Hour)) // late
	s.invalidateNodeAnalyticsEvictionLocked(s.packets[:1])
	if got := s.nodeAnalyticsCoverageLocked(30, now).LateEvictedObservations; got != 3 {
		t.Fatalf("late evicted observations=%d want 3 (only >%v after FirstSeen)", got, lateEvictedObservationThreshold)
	}
	if lateEvictedObservationThreshold != time.Hour {
		t.Fatalf("threshold=%v want 1h", lateEvictedObservationThreshold)
	}
}
