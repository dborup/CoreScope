package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWSClientHistory_ThirtyDayCapacity(t *testing.T) {
	if wsHistoryCapacity != 30*24*60 {
		t.Fatalf("capacity = %d, want 43200", wsHistoryCapacity)
	}
}

func TestWSClientHistory_PersistencePathOverrideAndDisable(t *testing.T) {
	db := filepath.Join(t.TempDir(), "meshcore.db")
	if got := wsHistoryPersistencePath("", db); got != filepath.Join(filepath.Dir(db), wsHistoryFileName) {
		t.Fatalf("default path = %s", got)
	}
	if got := wsHistoryPersistencePath("custom.jsonl", db); got != "custom.jsonl" {
		t.Fatalf("override = %s", got)
	}
	if got := wsHistoryPersistencePath("off", db); got != "" {
		t.Fatalf("disabled path = %s", got)
	}
}

func TestWSClientHistory_LazyGrowthPreservesOrderAndBound(t *testing.T) {
	h := newWSClientHistory(wsHistoryCapacity)
	if len(h.buf) > 1440 {
		t.Fatalf("eager allocation = %d", len(h.buf))
	}
	base := time.Now().UTC().Add(-wsHistoryRetention)
	for i := 0; i < wsHistoryCapacity+100; i++ {
		h.sample(i, base.Add(time.Duration(i)*time.Minute))
	}
	got := h.snapshot()
	if len(got) != wsHistoryCapacity || got[0].Clients != 100 || got[len(got)-1].Clients != wsHistoryCapacity+99 {
		t.Fatalf("growth lost order/bound: len=%d first=%d last=%d", len(got), got[0].Clients, got[len(got)-1].Clients)
	}
}

func TestWSClientHistory_ClockRollbackDoesNotPoisonDurableOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	h := newWSClientHistory(20)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	h.sampleAndPersist(1, now)
	h.sampleAndPersist(2, now.Add(-time.Minute))
	h.sampleAndPersist(3, now.Add(time.Minute))
	h.closePersistence()
	reopen := newWSClientHistory(20)
	if err := reopen.enablePersistence(path, "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	defer reopen.closePersistence()
	if got := reopen.snapshot(); len(got) != 2 || got[1].Clients != 3 {
		t.Fatalf("samples = %+v", got)
	}
}

func TestWSClientHistory_RestartWithSmallClockRollbackPreservesSavedFuturePoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	h.sampleAndPersist(3, now.Add(-time.Minute))
	h.sampleAndPersist(4, now.Add(time.Minute))
	h.closePersistence()
	reopen := newWSClientHistory(10)
	if err := reopen.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	defer reopen.closePersistence()
	r, _ := wsHistoryRange("24h")
	if got := reopen.snapshotRange(now, r); len(got) != 1 {
		t.Fatalf("future point exposed: %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 2 {
		t.Fatal("future saved point silently discarded during startup compaction")
	}
}

func TestWSClientHistory_ClockSkewBeyondToleranceDoesNotDisablePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	h.sampleAndPersist(3, now.Add(-time.Minute))
	// A later NTP correction can step the wall clock back by more than the
	// loader's 2-minute tolerance, leaving the last saved sample looking far
	// in the future relative to the corrected clock.
	h.sampleAndPersist(4, now.Add(10*time.Minute))
	h.closePersistence()

	reopen := newWSClientHistory(10)
	if err := reopen.enablePersistence(path, "", now); err != nil {
		t.Fatalf("clock skew beyond 2 minutes must not disable persistence: %v", err)
	}
	defer reopen.closePersistence()
	if !reopen.persistenceStatus().Healthy {
		t.Fatal("persistence must stay healthy across a large clock step-back")
	}
	r, _ := wsHistoryRange("24h")
	if got := reopen.snapshotRange(now.Add(11*time.Minute), r); len(got) != 1 || got[0].Clients != 3 {
		t.Fatalf("the far-future sample must be skipped at load, not reject the whole journal or block sampling: %+v", got)
	}
}

// #463: a journal written by a process whose clock ran ahead must not stall
// sampling after the clock steps back.
func TestWSClientHistory_FutureDatedJournalDoesNotStallSampling(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	a := newWSClientHistory(100)
	if err := a.enablePersistence(path, "", now.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a.sampleAndPersist(1, now.Add(72*time.Hour))
	a.sampleAndPersist(2, now.Add(72*time.Hour+time.Minute))
	a.closePersistence()

	b := newWSClientHistory(100)
	if err := b.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	b.sampleAndPersist(5, now.Add(time.Minute))
	b.sampleAndPersist(6, now.Add(2*time.Minute))
	b.closePersistence()
	got := b.snapshot()
	if len(got) != 2 || got[0].Clients != 5 || got[1].Clients != 6 {
		t.Fatalf("B must keep sampling after loading future-dated records: %+v", got)
	}
	// The next restart must still load the journal (order stays valid).
	c := newWSClientHistory(100)
	if err := c.enablePersistence(path, "", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("journal unreadable after new samples: %v", err)
	}
	defer c.closePersistence()
	if got := c.snapshot(); len(got) != 2 || got[1].Clients != 6 {
		t.Fatalf("reload = %+v", got)
	}
}

// #463: equal timestamps are not strictly increasing and must be rejected.
func TestWSClientHistory_EqualJournalTimestampsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC().Truncate(time.Second)
	ts := now.Add(-time.Minute).Format(time.RFC3339Nano)
	line := `{"t":"` + ts + `","clients":1,"peak":1}` + "\n"
	if err := os.WriteFile(path, []byte(line+line), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err == nil {
		h.closePersistence()
		t.Fatal("two records with the same timestamp must be rejected")
	}
	if got, _ := os.ReadFile(path); string(got) != line+line {
		t.Fatal("rejected journal must be preserved byte-for-byte")
	}
}

func TestWSClientHistory_RestoredBackupWithGroupReadableModeIsAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	b, _ := json.Marshal(WSClientSample{T: now.Add(-time.Minute), Clients: 2, Peak: 2})
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// A backup restored via tar/cp under umask 022 comes back as 0644: still
	// safe, since group/other can only read it, never write it. Bypass the
	// process umask with an explicit Chmod so the test is deterministic.
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatalf("restored backup at mode 0644 must still load: %v", err)
	}
	defer h.closePersistence()
	if !h.persistenceStatus().Healthy {
		t.Fatal("restored backup at mode 0644 must be healthy")
	}
	if got := h.snapshot(); len(got) != 1 || got[0].Clients != 2 {
		t.Fatalf("samples = %+v", got)
	}
}

func TestWSClientHistory_GroupWritableJournalStillRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	b, _ := json.Marshal(WSClientSample{T: now.Add(-time.Minute), Clients: 2, Peak: 2})
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err == nil {
		t.Fatal("group-writable journal accepted")
	}
}

func TestWSClientHistory_OnlyOneWriterAndSamplerStopReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	first := NewHub()
	if err := first.history.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	defer first.history.closePersistence()
	first.history.sampleAndPersist(7, now)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := newWSClientHistory(20)
	if err := second.enablePersistence(path, "", now); err == nil {
		t.Fatal("concurrent writer accepted")
	}
	second.sampleAndPersist(99, now)
	if after, _ := os.ReadFile(path); string(before) != string(after) {
		t.Fatal("second writer changed active journal")
	}
	if status := second.persistenceStatus(); !status.Enabled || status.Healthy {
		t.Fatalf("second status = %+v", status)
	}
	stop := first.StartHistorySampler(time.Hour)
	stop()
	stop()
	third := newWSClientHistory(20)
	if err := third.enablePersistence(path, "", now); err != nil {
		t.Fatalf("stop did not release lock: %v", err)
	}
	defer third.closePersistence()
	if got := third.snapshot(); len(got) != 1 || got[0].Clients != 7 {
		t.Fatalf("first writer data lost: %+v", got)
	}
}

func TestWSClientHistory_TornFinalAppendRecoversSyncedPrefix(t *testing.T) {
	for _, tail := range []string{"{\"t\":\"20", "{\"t\":\"2026-10-15T12:00:00Z\",\"clients\":8,\"peak\":8}"} {
		t.Run(tail[:8], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), wsHistoryFileName)
			now := time.Now().UTC()
			var valid strings.Builder
			for i := 0; i < 2; i++ {
				b, _ := json.Marshal(WSClientSample{T: now.Add(time.Duration(i-2) * time.Minute), Clients: i + 3, Peak: i + 6})
				valid.Write(b)
				valid.WriteByte('\n')
			}
			if err := os.WriteFile(path, []byte(valid.String()+tail), 0600); err != nil {
				t.Fatal(err)
			}
			h := newWSClientHistory(10)
			if err := h.enablePersistence(path, "", now); err != nil {
				t.Fatal(err)
			}
			defer h.closePersistence()
			if got := h.snapshot(); len(got) != 2 || got[0].Clients != 3 || got[1].Peak != 7 {
				t.Fatalf("synced prefix lost: %+v", got)
			}
			if data, _ := os.ReadFile(path); string(data) != valid.String() {
				t.Fatalf("atomic recovery did not preserve prefix: %q", data)
			}
		})
	}
}

func TestWSClientHistory_LockMustNotAliasPacketDatabaseOrSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	db := path + ".lock"
	if err := os.WriteFile(db, []byte("packet database"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, db, time.Now()); err == nil {
		t.Fatal("lock aliases packet database")
	}
	if got, _ := os.ReadFile(db); string(got) != "packet database" {
		t.Fatal("database changed")
	}
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, db); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := h.enablePersistence(path, "", time.Now()); err == nil {
		t.Fatal("symlink lock accepted")
	}
	if got, _ := os.ReadFile(target); string(got) != "private" {
		t.Fatal("lock target changed")
	}
}

func TestWSClientHistory_WriteFailureReportedAndNoRetryOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wsHistoryFileName)
	now := time.Now().UTC()
	h := newWSClientHistory(20)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	defer h.closePersistence()
	// Deterministically simulate a permissions/storage replacement failure,
	// even when tests run as root: the target becomes a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	h.sampleAndPersist(1, now)
	if status := h.persistenceStatus(); !status.Enabled || status.Healthy {
		t.Fatalf("status = %+v", status)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	const sentinel = "operator replacement; do not rewrite"
	if err := os.WriteFile(path, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}
	h.sampleAndPersist(2, now.Add(time.Minute))
	if data, _ := os.ReadFile(path); string(data) != sentinel {
		t.Fatal("failed journal retried and changed replacement")
	}
	if len(h.snapshot()) != 2 {
		t.Fatal("RAM sampling stopped on disk error")
	}
}

func TestWSClientHistory_DailyCompactionAndRestartRetainPeaks(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC().Add(-25 * time.Hour)
	h := newWSClientHistory(wsHistoryCapacity)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < wsHistoryCompactEvery+5; i++ {
		h.observe(i + 7)
		h.sampleAndPersist(i, now.Add(time.Duration(i)*time.Minute))
	}
	h.closePersistence()
	reopen := newWSClientHistory(wsHistoryCapacity)
	if err := reopen.enablePersistence(path, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	defer reopen.closePersistence()
	got := reopen.snapshot()
	if len(got) != wsHistoryCompactEvery+5 || got[len(got)-1].Peak != wsHistoryCompactEvery+11 {
		t.Fatalf("reopened points=%d last=%+v", len(got), got[len(got)-1])
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".ws-client-history-*.tmp")); len(matches) != 0 {
		t.Fatalf("temporary files leaked: %v", matches)
	}
}

func TestWSClientHistory_RangesAndAggregation(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	h := newWSClientHistory(wsHistoryCapacity)
	for _, age := range []time.Duration{31 * 24 * time.Hour, 29 * 24 * time.Hour, 6 * 24 * time.Hour, 23 * time.Hour} {
		h.sample(1, now.Add(-age))
	}
	for _, tc := range []struct {
		rangeName string
		want      int
		interval  int
	}{{"24h", 1, 60}, {"7d", 2, 300}, {"30d", 3, 900}} {
		t.Run(tc.rangeName, func(t *testing.T) {
			r, ok := wsHistoryRange(tc.rangeName)
			if !ok {
				t.Fatal("range rejected")
			}
			got := h.snapshotRange(now, r)
			if len(got) != tc.want || int(r.interval/time.Second) != tc.interval {
				t.Fatalf("got %d samples at %v", len(got), r.interval)
			}
		})
	}
	if _, ok := wsHistoryRange("1y"); ok {
		t.Fatal("invalid range accepted")
	}
	if r, ok := wsHistoryRange(""); !ok || r.name != "24h" {
		t.Fatal("default must be 24h")
	}
}

func TestWSClientHistory_AggregationKeepsLastCountMaximumPeakAndGaps(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	h := newWSClientHistory(20)
	h.observe(90)
	h.sample(3, now.Add(-4*time.Minute))
	h.sample(8, now.Add(-3*time.Minute))
	h.markRestartGap()
	h.sample(4, now.Add(-2*time.Minute))
	h.sample(5, now.Add(-time.Minute))
	r, _ := wsHistoryRange("7d")
	got := h.snapshotRange(now, r)
	if len(got) != 2 || got[0].Clients != 8 || got[0].Peak != 90 || got[1].Clients != 5 || !got[1].GapBefore {
		t.Fatalf("aggregates = %+v", got)
	}
	if !got[0].T.Equal(now.Add(-3*time.Minute)) || !got[1].T.Equal(now.Add(-time.Minute)) {
		t.Fatalf("timestamps must describe last actual sample: %+v", got)
	}
}

func TestWSClientHistory_RestartRestoresHistoryAndMarksGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws-client-history.jsonl")
	now := time.Now().UTC().Truncate(time.Second)
	h := newWSClientHistory(wsHistoryCapacity)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	h.sampleAndPersist(4, now.Add(-time.Minute))
	h.observe(12)
	h.sampleAndPersist(2, now)
	h.closePersistence()
	reopened := newWSClientHistory(wsHistoryCapacity)
	if err := reopened.enablePersistence(path, "", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	defer reopened.closePersistence()
	got := reopened.snapshot()
	if len(got) != 2 || got[1].Clients != 2 || got[1].Peak != 12 {
		t.Fatalf("restored = %+v", got)
	}
	reopened.sampleAndPersist(1, now.Add(time.Minute))
	got = reopened.snapshot()
	if !got[2].GapBefore || got[2].Peak != 1 {
		t.Fatalf("restart must not bridge gap or carry prior peak: %+v", got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}

func TestWSClientHistory_CompactionDropsExpiredAndBoundsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws-client-history.jsonl")
	now := time.Now().UTC()
	var lines strings.Builder
	for i := 0; i < wsHistoryCapacity+20; i++ {
		b, _ := json.Marshal(WSClientSample{T: now.Add(time.Duration(i-wsHistoryCapacity-19) * time.Minute), Clients: 2, Peak: 3})
		lines.Write(b)
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	h := newWSClientHistory(wsHistoryCapacity)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	defer h.closePersistence()
	if len(h.snapshot()) > wsHistoryCapacity {
		t.Fatal("history exceeds bound")
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), "\n") > wsHistoryCapacity {
		t.Fatal("compacted file exceeds retention")
	}
	if !h.persistenceStatus().Healthy {
		t.Fatal("valid persistence not healthy")
	}
}

func TestWSClientHistory_CorruptOversizedAndUnsafeFilesPreserved(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"malformed", "{not-json}\n"},
		{"unrecognized-without-newline", "operator file; do not erase"},
		{"negative", "{\"t\":\"2026-10-15T12:00:00Z\",\"clients\":-1,\"peak\":1}\n"},
		{"peak-below-current", "{\"t\":\"2026-10-15T12:00:00Z\",\"clients\":2,\"peak\":1}\n"},
		{"oversized", strings.Repeat("x", wsHistoryMaxFileBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			h := newWSClientHistory(10)
			if err := h.enablePersistence(path, "", time.Now()); err == nil {
				t.Fatal("bad file accepted")
			}
			h.sampleAndPersist(5, time.Now())
			got, _ := os.ReadFile(path)
			if string(got) != tc.content {
				t.Fatal("bad file changed")
			}
			if status := h.persistenceStatus(); !status.Enabled || status.Healthy {
				t.Fatalf("status = %+v", status)
			}
			if len(h.snapshot()) != 1 {
				t.Fatal("RAM history must keep working")
			}
		})
	}
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	h := newWSClientHistory(10)
	if err := h.enablePersistence(link, "", time.Now()); err == nil {
		t.Fatal("symlink accepted")
	}
	if got, _ := os.ReadFile(target); string(got) != "private" {
		t.Fatal("symlink target changed")
	}
}

func TestWSClientHistory_RejectsPacketDatabasePathsAndHardlinks(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "meshcore.db")
	if err := os.WriteFile(db, []byte("packet database"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Link(db, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{db, alias} {
		h := newWSClientHistory(10)
		if err := h.enablePersistence(path, db, time.Now()); err == nil {
			t.Fatalf("packet DB path accepted: %s", path)
		}
	}
	if got, _ := os.ReadFile(db); string(got) != "packet database" {
		t.Fatal("packet DB changed")
	}
}

func TestWSClientHistory_TimeGapMarkedAndFutureSamplesExcluded(t *testing.T) {
	now := time.Now().UTC()
	h := newWSClientHistory(10)
	h.sample(1, now.Add(-4*time.Minute))
	h.sample(2, now)
	h.sample(3, now.Add(time.Hour))
	r, _ := wsHistoryRange("24h")
	got := h.snapshotRange(now, r)
	if len(got) != 2 || !got[1].GapBefore {
		t.Fatalf("samples = %+v", got)
	}
}

func TestHandlePerfWSClients_InvalidRangeAndExtendedShape(t *testing.T) {
	srv, router := setupNoStoreServer(t)
	srv.hub.history.sample(4, time.Now().UTC())
	for _, rangeName := range []string{"24h", "7d", "30d", "invalid"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/perf/ws-clients?range="+rangeName, nil))
		if rangeName == "invalid" {
			if w.Code != 400 {
				t.Fatalf("invalid range status = %d", w.Code)
			}
			continue
		}
		var resp WSClientHistoryResp
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || resp.Range != rangeName || len(resp.Samples) != 1 {
			t.Fatalf("response = %s", w.Body.String())
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &raw)
		if _, ok := raw["persistence"]; !ok {
			t.Fatal("missing persistence status")
		}
	}
}

// writeMinimalWSHistoryRecord is the smallest valid journal line: strictly
// after prev, non-negative, peak >= clients.
func writeMinimalWSHistoryRecord(w *strings.Builder, at time.Time) {
	b, _ := json.Marshal(WSClientSample{T: at, Clients: 0, Peak: 0})
	w.Write(b)
	w.WriteByte('\n')
}

func TestWSClientHistory_EightMiBLoadLimitRejectsManyValidRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	// Many small, individually valid records whose combined size crosses the
	// 8 MiB load cap — distinct from a single oversized/corrupt line, which
	// a smaller per-record buffer would reject on its own.
	var buf strings.Builder
	const n = 170000
	start := now.Add(-time.Duration(n) * time.Millisecond)
	for i := 0; i < n; i++ {
		writeMinimalWSHistoryRecord(&buf, start.Add(time.Duration(i)*time.Millisecond))
	}
	if buf.Len() <= wsHistoryMaxFileBytes {
		t.Fatalf("fixture too small to exercise the cap: %d bytes", buf.Len())
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0600); err != nil {
		t.Fatal(err)
	}
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err == nil {
		t.Fatal("file over the 8 MiB load cap accepted")
	}
	if got, _ := os.ReadFile(path); string(got) != buf.String() {
		t.Fatal("rejected oversized journal must not be modified")
	}
}

func TestWSClientHistory_PerRecordBufferLimitRejectsAnOverlongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	// Padding whitespace is insignificant in JSON, so this line decodes fine
	// under a larger buffer but must overflow the 4 KiB per-record reader,
	// well below the 8 MiB whole-file cap (isolating the two checks).
	valid, _ := json.Marshal(WSClientSample{T: now.Add(-time.Minute), Clients: 1, Peak: 1})
	line := "{" + strings.Repeat(" ", 8192) + string(valid[1:])
	if len(line) <= 4096 || len(line) >= wsHistoryMaxFileBytes {
		t.Fatalf("fixture line length = %d, must sit strictly between the two caps", len(line))
	}
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err == nil {
		t.Fatal("line longer than the 4 KiB per-record buffer accepted")
	}
}

func TestWSClientHistory_AppendCapRejectsAGrownJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", now); err != nil {
		t.Fatal(err)
	}
	defer h.closePersistence()
	// Simulate a journal that has grown close to the cap over many appends
	// (enablePersistence's own startup compaction rewrote it small, so
	// overwrite it directly rather than looping thousands of real appends).
	filler := make([]byte, wsHistoryMaxFileBytes-10)
	for i := range filler {
		filler[i] = 'x'
	}
	filler[len(filler)-1] = '\n'
	if err := os.WriteFile(path, filler, 0600); err != nil {
		t.Fatal(err)
	}
	h.sampleAndPersist(1, now.Add(time.Minute))
	if status := h.persistenceStatus(); !status.Enabled || status.Healthy {
		t.Fatalf("append over the 8 MiB cap must degrade persistence: %+v", status)
	}
	if got, _ := os.ReadFile(path); string(got) != string(filler) {
		t.Fatal("rejected append must not modify the journal")
	}
}

func TestWSClientHistory_CompactionRejectsOversizedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), wsHistoryFileName)
	now := time.Now().UTC()
	// One minimal sample encodes to roughly 25 bytes; this many cross the
	// 8 MiB compacted-output cap even though the live 30-day ring (43200
	// samples, ~1 MB at this size) never reaches it in practice.
	const n = 350000
	samples := make([]WSClientSample, n)
	for i := range samples {
		samples[i] = WSClientSample{T: now.Add(time.Duration(i-n) * time.Second), Clients: 0, Peak: 0}
	}
	j := &wsHistoryJournal{path: path}
	if err := j.compact(samples, now); err == nil {
		t.Fatal("oversized compaction output accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a rejected compaction must not replace the journal")
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".ws-client-history-*.tmp")); len(matches) != 0 {
		t.Fatalf("temporary file leaked: %v", matches)
	}
}

func TestWSClientHistory_HardlinkWithoutDBAliasStillRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wsHistoryFileName)
	if err := os.WriteFile(path, []byte("{\"t\":\"2026-10-15T12:00:00Z\",\"clients\":1,\"peak\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "unrelated-hardlink")
	if err := os.Link(path, other); err != nil {
		t.Fatal(err)
	}
	// No packet database path at all, so the alias check in
	// checkWSHistoryPath cannot be what rejects this — only the generic
	// Nlink != 1 guard in openWSHistoryFile can catch a hardlink that is
	// not a database alias.
	h := newWSClientHistory(10)
	if err := h.enablePersistence(path, "", time.Now()); err == nil {
		t.Fatal("hardlinked journal (not a database alias) accepted")
	}
}

func TestWSClientHistory_AggregationPeakUpdatesFromLaterSampleInBucket(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 10, 0, 0, time.UTC)
	h := newWSClientHistory(20)
	h.sample(2, time.Date(2026, 10, 15, 12, 0, 30, 0, time.UTC))
	h.observe(9)
	// Still within the first sample's 5-minute bucket (12:00-12:05), but not
	// the bucket's first sample: only an explicit max-peak update carries 9
	// into the aggregate, since this sample's own Peak starts above 2.
	h.sample(5, time.Date(2026, 10, 15, 12, 2, 30, 0, time.UTC))
	h.sample(1, time.Date(2026, 10, 15, 12, 6, 0, 0, time.UTC))
	r, _ := wsHistoryRange("7d")
	got := h.snapshotRange(now, r)
	if len(got) != 2 {
		t.Fatalf("expected 2 buckets, got %+v", got)
	}
	if got[0].Peak != 9 || got[0].Clients != 5 {
		t.Fatalf("bucket must keep the max peak from its later sample: %+v", got[0])
	}
}
