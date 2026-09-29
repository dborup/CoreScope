package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Issue #116: HTTP binds after the first chunk and the analytics
// recomputers start then, but LoadComplete() flips at the end of the hot
// window, before loadBackgroundChunks fills the retention window. A
// one-shot "startup load terminated" signal (StartupLoadDone) now fires on
// every RunStartupLoad exit path, every default-shape recomputer runs once
// after it, and the #1659 warm-up gate only opens on a pass that STARTED
// after it. backgroundLoadDone/Failed keep their health meaning.

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// hotAndBackgroundDB seeds hot rows (last 30 min) and older rows (1–5 days)
// so RunStartupLoad with HotStartupHours=1 loads the older ones in the
// background fill.
func hotAndBackgroundDB(t *testing.T, hot, older int) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	now := time.Now().UTC()
	seedTestDBRows(t, dbPath, hot+older, 2, func(i int) (string, int64) {
		ago := 30 * time.Minute
		if i >= hot {
			ago = time.Duration(24+(i-hot)%96) * time.Hour
		}
		ts := now.Add(-ago)
		return ts.Format(time.RFC3339), ts.Unix()
	})
	return dbPath
}

func openStartupStore(t *testing.T, dbPath string, retention, hot float64) *PacketStore {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.conn.Close() })
	return NewPacketStore(db, &PacketStoreConfig{RetentionHours: retention, HotStartupHours: hot})
}

func TestStartupLoadDoneFiresOnEveryPath_116(t *testing.T) {
	type want struct{ done, failed bool }
	cases := []struct {
		name  string
		setup func(t *testing.T) *PacketStore
		err   bool
		want  want // backgroundLoadDone/Failed must keep their meaning
	}{
		{"hot window + background fill", func(t *testing.T) *PacketStore {
			return openStartupStore(t, hotAndBackgroundDB(t, 20, 30), 168, 1)
		}, false, want{true, false}},
		{"hot window disabled (hotStartupHours=0)", func(t *testing.T) *PacketStore {
			return openStartupStore(t, hotAndBackgroundDB(t, 5, 5), 168, 0)
		}, false, want{true, false}},
		{"retention disabled", func(t *testing.T) *PacketStore {
			return openStartupStore(t, hotAndBackgroundDB(t, 5, 5), 0, 1)
		}, false, want{true, false}},
		{"empty database", func(t *testing.T) *PacketStore {
			return openStartupStore(t, hotAndBackgroundDB(t, 0, 0), 168, 1)
		}, false, want{true, false}},
		{"LoadChunked error", func(t *testing.T) *PacketStore {
			s := openStartupStore(t, hotAndBackgroundDB(t, 5, 5), 168, 1)
			s.db.conn.Close()
			return s
		}, true, want{true, true}},
		{"background fill failure", func(t *testing.T) *PacketStore {
			s := openStartupStore(t, hotAndBackgroundDB(t, 5, 30), 168, 1)
			s.bgLoaderEntryHook = func() { s.db.conn.Close() } // every background chunk fails
			return s
		}, false, want{false, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.setup(t)
			if isClosed(s.StartupLoadDone()) {
				t.Fatal("StartupLoadDone closed before RunStartupLoad")
			}
			err := s.RunStartupLoad(7)
			if (err != nil) != c.err {
				t.Fatalf("RunStartupLoad err = %v, want error %v", err, c.err)
			}
			if !isClosed(s.StartupLoadDone()) {
				t.Fatal("StartupLoadDone not closed after RunStartupLoad returned")
			}
			if got := (want{s.backgroundLoadDone.Load(), s.backgroundLoadFailed.Load()}); got != c.want {
				t.Fatalf("health semantics changed: done/failed = %+v, want %+v", got, c.want)
			}
			s.signalStartupLoadDone() // a second signal is a no-op, not a panic
		})
	}
}

// The signal is separate from LoadComplete(): it stays open while the
// background fill runs, after the hot window has already reported complete.
func TestStartupLoadDoneWaitsForBackgroundFill_116(t *testing.T) {
	s := openStartupStore(t, hotAndBackgroundDB(t, 20, 40), 168, 1)
	var sawComplete, sawDone atomic.Bool
	s.bgLoaderEntryHook = func() {
		sawComplete.Store(s.LoadComplete())
		sawDone.Store(isClosed(s.StartupLoadDone()))
	}
	if err := s.RunStartupLoad(7); err != nil {
		t.Fatal(err)
	}
	if !sawComplete.Load() {
		t.Fatal("fixture: LoadComplete() was false when the background fill started")
	}
	if sawDone.Load() {
		t.Fatal("StartupLoadDone was closed while the background fill was still to run")
	}
}

func TestStartupLoadDoneDropsDependentCaches_116(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewPacketStore(db, nil)
	s.hashSizeInfoMu.Lock()
	s.hashSizeInfoCache = map[string]*hashSizeNodeInfo{"x": {}}
	s.hashSizeInfoAt = time.Now()
	s.hashSizeInfoMu.Unlock()
	s.clockSkew.mu.Lock()
	s.clockSkew.lastComputed = time.Now()
	s.clockSkew.mu.Unlock()
	s.cacheMu.Lock()
	s.rfCache["AAR|"] = &cachedResult{expiresAt: time.Now().Add(time.Hour)}
	s.cacheMu.Unlock()

	s.signalStartupLoadDone()

	s.hashSizeInfoMu.Lock()
	hs := s.hashSizeInfoCache
	s.hashSizeInfoMu.Unlock()
	s.clockSkew.mu.Lock()
	last := s.clockSkew.lastComputed
	s.clockSkew.mu.Unlock()
	s.cacheMu.Lock()
	rf := len(s.rfCache)
	s.cacheMu.Unlock()
	if hs != nil {
		t.Error("the hash-size info cache (15 s TTL) survived the startup-load signal")
	}
	if !last.IsZero() {
		t.Error("the clock-skew recompute throttle survived the startup-load signal")
	}
	if rf != 0 {
		t.Error("region-keyed analytics TTL caches computed on the partial store survived the signal")
	}
}

// RecomputeNow runs a pass that starts after the call, on the recomputer's
// own goroutine (never concurrently with a periodic pass), and restarts the
// ticker so no periodic pass follows right behind it.
func TestRecomputeNowRunsOnLoopAndResetsTicker_116(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	var running, maxRunning atomic.Int32
	rc := newAnalyticsRecomputer("t", 400*time.Millisecond, func() interface{} {
		if n := running.Add(1); n > maxRunning.Load() {
			maxRunning.Store(n)
		}
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		return 1
	})
	rc.Start()
	defer rc.Stop()
	time.Sleep(300 * time.Millisecond)
	before := time.Now()
	rc.RecomputeNow()
	mu.Lock()
	n := len(starts)
	last := starts[n-1]
	mu.Unlock()
	if n != 2 || last.Before(before) {
		t.Fatalf("RecomputeNow: %d passes, last started %v before the call", n, before.Sub(last))
	}
	// Without the reset the periodic tick at ~400 ms would run now.
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	n = len(starts)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("a periodic pass ran %d time(s) right after RecomputeNow: the ticker was not reset", n-2)
	}
	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	n = len(starts)
	mu.Unlock()
	if n < 3 {
		t.Fatal("the periodic loop stopped after RecomputeNow")
	}
	if maxRunning.Load() != 1 {
		t.Fatalf("passes overlapped (%d at once)", maxRunning.Load())
	}
	rc.Stop()
	done := make(chan struct{})
	go func() { rc.RecomputeNow(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecomputeNow blocked on a stopped recomputer")
	}
}

// A pass that started before the startup load finished must not open the
// warm-up gate, even if the load finishes while it runs.
func TestWarmupGateIgnoresPassStartedBeforeLoad_116(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewPacketStore(db, nil)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var calls atomic.Int32
	rc := newAnalyticsRecomputer("gated", time.Hour, func() interface{} {
		if calls.Add(1) == 2 {
			entered <- struct{}{}
			<-release
		}
		return 1
	})
	loaded := s.StartupLoadDone()
	rc.setWarmupReadyGate_1659(func() bool { return isClosed(loaded) })
	rc.Start() // pass 1: before the load, does not count
	if !rc.FirstPassDoneAt_1659().IsZero() {
		t.Fatal("a pass before the load opened the gate")
	}
	go rc.RecomputeNow() // pass 2 starts before the load ...
	<-entered
	s.signalStartupLoadDone() // ... and the load finishes while it runs
	close(release)
	time.Sleep(50 * time.Millisecond)
	if !rc.FirstPassDoneAt_1659().IsZero() {
		t.Fatal("a pass that STARTED before the load finished opened the warm-up gate")
	}
	rc.RecomputeNow() // pass 3 starts after: opens it
	if rc.FirstPassDoneAt_1659().IsZero() {
		t.Fatal("a pass started after the load did not open the gate")
	}
	rc.Stop()
}

func recomputerByName(s *PacketStore) map[string]*analyticsRecomputer {
	s.analyticsRecomputerMu.RLock()
	defer s.analyticsRecomputerMu.RUnlock()
	out := map[string]*analyticsRecomputer{}
	for _, rc := range []*analyticsRecomputer{s.recompTopology, s.recompRF, s.recompDistance, s.recompChannels,
		s.recompHashCollisions, s.recompHashSizes, s.recompRoles, s.recompObserversClockSkew, s.recompNodesClockSkew} {
		out[rc.name] = rc
	}
	return out
}

// Every default-shape recomputer runs exactly once more, promptly, after
// the signal; the gated three first and roles after nodes-clock-skew (roles
// reads that snapshot).
func TestEveryRecomputerRunsOnceAfterLoad_116(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewPacketStore(db, nil)
	stop := s.StartAnalyticsRecomputers(time.Hour)
	defer stop()
	rcs := recomputerByName(s)
	before := map[string]int64{}
	for n, rc := range rcs {
		before[n] = rc.ComputeRuns()
	}
	time.Sleep(50 * time.Millisecond)
	for n, rc := range rcs {
		if rc.ComputeRuns() != before[n] {
			t.Fatalf("%s recomputed before the load finished", n)
		}
	}
	signalled := time.Now()
	s.signalStartupLoadDone()
	deadline := time.Now().Add(10 * time.Second)
	for n, rc := range rcs {
		for rc.ComputeRuns() < before[n]+1 {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not recompute after the startup load finished (runs %d)", n, rc.ComputeRuns())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(100 * time.Millisecond)
	type started struct {
		name string
		at   time.Time
	}
	var order []started
	for n, rc := range rcs {
		if got := rc.ComputeRuns(); got != before[n]+1 {
			t.Errorf("%s ran %d extra passes after the load, want 1", n, got-before[n])
		}
		if rc.LastStartAt().Before(signalled) {
			t.Errorf("%s's post-load pass started before the signal", n)
		}
		order = append(order, started{n, rc.LastStartAt()})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].at.Before(order[j].at) })
	pos := map[string]int{}
	for i, o := range order {
		pos[o.name] = i
	}
	for _, g := range []string{"rf", "topology", "channels"} {
		if pos[g] > 2 {
			t.Errorf("gated recomputer %s ran at position %d; the gated three go first", g, pos[g])
		}
	}
	if pos["roles"] < pos["nodes-clock-skew"] {
		t.Error("roles recomputed before nodes-clock-skew, whose snapshot it reads")
	}
}

func analyticsRouter(t *testing.T, s *PacketStore) *mux.Router {
	t.Helper()
	srv := NewServer(s.db, &Config{Port: 3000}, NewHub())
	srv.store = s
	r := mux.NewRouter()
	srv.RegisterRoutes(r)
	return r
}

func get(r http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// End to end the way main.go runs it: recomputers start at the first chunk,
// the hot window completes (LoadComplete()==true), the background fill is
// held. RF stays 503 (on master it opened on the hot-window snapshot);
// ungated endpoints keep answering 200; after the fill RF serves a snapshot
// of the full store.
func TestGatedRFWaitsForBackgroundFill_116(t *testing.T) {
	s := openStartupStore(t, hotAndBackgroundDB(t, 20, 60), 168, 1)
	hold := make(chan struct{})
	inBg := make(chan struct{})
	s.bgLoaderEntryHook = func() { close(inBg); <-hold }
	loadErr := make(chan error, 1)
	go func() { loadErr <- s.RunStartupLoad(7) }()
	<-s.FirstChunkReady()
	<-inBg
	if !s.LoadComplete() {
		t.Fatal("fixture: hot window not complete when the background fill started")
	}
	stop := s.StartAnalyticsRecomputers(time.Hour)
	defer stop()
	r := analyticsRouter(t, s)
	if w := get(r, "/api/analytics/rf"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("RF during the background fill: %d, want 503 (the hot-window snapshot must not open the gate)", w.Code)
	}
	for _, p := range []string{"/api/analytics/hash-sizes", "/api/analytics/hash-collisions", "/api/analytics/roles"} {
		if w := get(r, p); w.Code != http.StatusOK {
			t.Errorf("ungated %s during the load: %d, want 200 (no new 503s)", p, w.Code)
		}
	}
	close(hold)
	if err := <-loadErr; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for get(r, "/api/analytics/rf").Code != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("RF never opened after the startup load")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := s.recompRF.Load().(map[string]interface{})["totalTransmissions"]
	want := s.computeAnalyticsRF("", "", TimeWindow{})["totalTransmissions"]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RF snapshot totalTransmissions=%v, full store gives %v", got, want)
	}
}

// When the force timeout opened the gate on partial data, the post-load
// recompute still replaces that snapshot promptly.
func TestForcedOpenSnapshotReplacedAfterLoad_116(t *testing.T) {
	old := warmupForceTimeout
	warmupForceTimeout = time.Millisecond
	defer func() { warmupForceTimeout = old }()
	db := setupTestDB(t)
	defer db.Close()
	s := NewPacketStore(db, nil)
	stop := s.StartAnalyticsRecomputers(time.Hour)
	defer stop()
	time.Sleep(5 * time.Millisecond)
	if s.recompRF.IsWarmingUp_1659() {
		t.Fatal("fixture: the force timeout did not open the gate")
	}
	runs := s.recompRF.ComputeRuns()
	s.signalStartupLoadDone()
	deadline := time.Now().Add(5 * time.Second)
	for s.recompRF.ComputeRuns() == runs {
		if time.Now().After(deadline) {
			t.Fatal("the forced-open RF snapshot was not replaced after the load")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.recompRF.FirstPassDoneAt_1659().IsZero() {
		t.Fatal("the post-load pass did not mark the first real pass")
	}
}

// The lazy distance build refreshes the distance snapshot before the
// index reports built, so the handler never goes from 202 to an older
// snapshot.
func TestDistanceSnapshotRefreshedBeforeReady_116(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	stop := s.StartAnalyticsRecomputers(time.Hour)
	defer stop()
	runs := s.recompDistance.ComputeRuns()
	var runsAtReady int64 = -1
	var snapAtReady interface{}
	s.TriggerDistanceIndexBuild()
	deadline := time.Now().Add(5 * time.Second)
	for runsAtReady < 0 {
		if s.DistanceIndexBuilt() {
			runsAtReady = s.recompDistance.ComputeRuns()
			snapAtReady = s.recompDistance.Load()
		}
		if time.Now().After(deadline) {
			t.Fatal("distance index never built")
		}
		time.Sleep(time.Millisecond)
	}
	if runsAtReady <= runs {
		t.Fatal("the distance index reported built before the distance snapshot was refreshed")
	}
	if !reflect.DeepEqual(snapAtReady, s.computeAnalyticsDistance("", "")) {
		t.Fatal("the distance snapshot at ready does not match the built index")
	}
}
