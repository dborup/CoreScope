package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Issue #622: audit of upstream pull request 2284 ("preserve distance
// invalidation during an active build", upstream issue 2281).
//
// Upstream's background-fill completion reset an in-use sync.Once and a build
// that started before the fill published `built = true` afterwards, so the
// gate reported an index built from part-way through the fill as current.
// Our fork replaced the Once with a dataset generation in #149: the fill bumps
// distDataGen under s.mu after the last chunk is merged, every build records
// the generation it read inside its own s.mu section, and the gate reports the
// index current only while the two match.
//
// #149 covers the gate state exhaustively, but every one of its scenarios (and
// #558's) runs the background loader with an empty chunk loop — oldestLoaded
// is already past the retention cutoff — so no test merged a packet during an
// invalidation, and none asserted what the rebuilt index then contains. The
// three tests below close that gap with the real chunk loader: a fill that
// merges transmissions the hot window did not have, and an assertion that the
// ready response carries them.
//
// The fixture keeps the merged observations below the 5 % Δobs debounce
// threshold, as upstream's regression does, so the rebuild proves the
// generation invalidation survived rather than the growth policy firing.

const (
	// dist622HotTx transmissions sit inside the 2 h hot startup window and
	// dist622ColdTx older ones inside the 48 h retention window, where only
	// the background chunk loader reaches them. One observation each, so the
	// fill grows totalObs by dist622ColdTx/dist622HotTx = 3.3 %, under the
	// debounce threshold (see dist622RequireBelowDebounce).
	dist622HotTx  = 60
	dist622ColdTx = 2
	// Chunk size of the startup load; small enough to exercise its loop.
	dist622ChunkSize = 25
)

// dist622Raw is a distinct, decodable raw packet per transmission id, so every
// transmission gets its own content hash and the cold ones are identifiable in
// the distance analytics.
func dist622Raw(id int) string {
	return fmt.Sprintf("0A00D69FD7A5A7475DB07337749AE61FA53A4788E9%04X", id)
}

// dist622Seed returns a database with the hot and cold transmissions described
// above and the content hashes of the cold ones. The four nodes it writes carry
// GPS a few km apart and cover the sender plus every path hop, so a
// transmission over those paths yields distance records. They live in the nodes
// table rather than in the store's node cache, so a cache refresh during the
// test keeps them.
func dist622Seed(t *testing.T) (*DB, []string) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	for _, n := range []struct {
		pk  string
		lat float64
	}{
		{acct113PK(150), 55.00}, // sender of every transmission
		{acct113PK(5), 55.02},   // hop "05"
		{acct113PK(6), 55.04},   // hop "06"
		{acct113PK(7), 55.06},   // hop "07", only on the cold path
	} {
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key,name,role,lat,lon,last_seen,first_seen,advert_count)
			VALUES (?,?,'repeater',?,12.00,?,?,3)`, n.pk, "n"+n.pk[:2], n.lat,
			now.Format(time.RFC3339), now.Add(-72*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(id int, at time.Time, path string) string {
		t.Helper()
		raw := dist622Raw(id)
		hash := ComputeContentHash(raw)
		if _, err := db.conn.Exec(`INSERT INTO transmissions (id,raw_hex,hash,first_seen,route_type,payload_type,payload_version,decoded_json)
			VALUES (?,?,?,?,1,4,1,?)`, id, raw, hash, at.Format(time.RFC3339),
			fmt.Sprintf(`{"type":"ADVERT","pubKey":%q,"name":"n"}`, acct113PK(150))); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id,observer_idx,direction,snr,rssi,score,path_json,timestamp)
			VALUES (?,1,'RX',-5.0,-90.0,5,?,?)`, id, path, at.Unix()); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	for i := 1; i <= dist622HotTx; i++ {
		insert(i, now.Add(-time.Hour-time.Duration(i)*time.Second), `["05","06"]`)
	}
	cold := make([]string, 0, dist622ColdTx)
	for i := 0; i < dist622ColdTx; i++ {
		// 30 h back: outside the hot window, inside retention, and not in the
		// first chunk the background loader walks, so the merge happens with
		// chunks of the fill already behind it.
		cold = append(cold, insert(500+i, now.Add(-30*time.Hour-time.Duration(i)*time.Minute), `["05","06","07"]`))
	}
	return db, cold
}

// dist622Store runs the production startup loader on db with hook installed as
// the distance build seam, and checks that the cold transmissions stayed in
// the database: the fill below has to be the thing that merges them.
func dist622Store(t *testing.T, db *DB, hook func()) *PacketStore {
	t.Helper()
	store := NewPacketStore(db, &PacketStoreConfig{HotStartupHours: 2, RetentionHours: 48})
	store.distanceBuildHook = hook
	if err := store.LoadChunked(dist622ChunkSize); err != nil {
		t.Fatalf("LoadChunked: %v", err)
	}
	if !store.WaitIndexesReady(distBuildDeadline) {
		t.Fatal("LoadChunked's background index builds did not finish")
	}
	store.mu.RLock()
	packets, obs := len(store.packets), store.totalObs
	store.mu.RUnlock()
	if packets != dist622HotTx || obs != dist622HotTx {
		t.Fatalf("after LoadChunked: %d packets, %d observations; want %d and %d (the cold rows must stay for the background fill)",
			packets, obs, dist622HotTx, dist622HotTx)
	}
	return store
}

// dist622RequireBelowDebounce pins the premise both tests rest on: with the
// index just built, the Δobs the fill adds does not reach the debounce
// threshold, so only the generation invalidation can drive a rebuild.
func dist622RequireBelowDebounce(t *testing.T) {
	t.Helper()
	if distanceRebuildDue(0, dist622HotTx, dist622HotTx+dist622ColdTx) {
		t.Fatalf("fixture premise broken: %d observations added to %d already reach the debounce threshold, so a rebuild would not prove the invalidation survived",
			dist622ColdTx, dist622HotTx)
	}
}

// dist622Fill runs the real background chunk loader and checks it merged the
// cold transmissions.
func dist622Fill(t *testing.T, store *PacketStore) {
	t.Helper()
	store.loadBackgroundChunks()
	store.mu.RLock()
	packets, obs := len(store.packets), store.totalObs
	store.mu.RUnlock()
	want := dist622HotTx + dist622ColdTx
	if packets != want || obs != want {
		t.Fatalf("after the background fill: %d packets, %d observations; want %d of each", packets, obs, want)
	}
	if !store.backgroundLoadDone.Load() {
		t.Fatal("the background fill did not report done")
	}
}

// dist622Router serves the real /api/analytics/distance handler on store.
func dist622Router(t *testing.T, db *DB, store *PacketStore) func() *httptest.ResponseRecorder {
	t.Helper()
	srv := NewServer(db, &Config{Port: 3000}, NewHub())
	srv.store = store
	r := mux.NewRouter()
	srv.RegisterRoutes(r)
	return func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/distance", nil))
		return w
	}
}

// dist622PathHashes decodes the content hashes of topPaths out of a ready
// distance response.
func dist622PathHashes(t *testing.T, w *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var body struct {
		TopPaths []struct {
			Hash      string  `json:"hash"`
			TotalDist float64 `json:"totalDist"`
		} `json:"topPaths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode distance response: %v (body=%s)", err, w.Body.String())
	}
	out := make(map[string]bool, len(body.TopPaths))
	for _, p := range body.TopPaths {
		if p.TotalDist > 0 {
			out[p.Hash] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("the ready response carries no measured path at all: %s", w.Body.String())
	}
	return out
}

func dist622RequireCold(t *testing.T, what string, got map[string]bool, cold []string, want bool) {
	t.Helper()
	for _, h := range cold {
		if got[h] != want {
			t.Fatalf("%s: cold path %s present=%v, want %v (paths: %v)", what, h, got[h], want, got)
		}
	}
}

// TestDistanceFill622RebuildBelowTheDebounceThreshold: a background fill that
// merges transmissions the hot window did not have invalidates an index that
// was just built, even though the observations it adds stay under the Δobs
// debounce threshold. Concurrent callers coalesce into one rebuild and all get
// 202 + Retry-After, and the 200 that follows carries the merged paths.
func TestDistanceFill622RebuildBelowTheDebounceThreshold(t *testing.T) {
	dist622RequireBelowDebounce(t)
	db, cold := dist622Seed(t)
	var builds atomic.Int32
	rebuild, releaseRebuild := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRebuild) }) }
	// Hold the rebuild (pass 2) open, so the 202 window after the fill is
	// deterministic; the first build runs to completion unheld.
	store := dist622Store(t, db, func() {
		if builds.Add(1) == 2 {
			close(rebuild)
			<-releaseRebuild
		}
	})
	t.Cleanup(release)
	stop := store.StartAnalyticsRecomputers(time.Hour)
	t.Cleanup(stop)
	get := dist622Router(t, db, store)

	if w := get(); w.Code != 202 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("first request: status %d, Retry-After %q; want 202 with a Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	distBuildWaitCurrent(t, store)
	w := get()
	if w.Code != 200 {
		t.Fatalf("after the first build: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	dist622RequireCold(t, "the hot-window index", dist622PathHashes(t, w), cold, false)
	if n := builds.Load(); n != 1 {
		t.Fatalf("builds before the fill = %d, want 1", n)
	}

	dist622Fill(t, store)
	if store.DistanceIndexBuilt() {
		t.Fatal("the background fill's invalidation was lost: the index built before it merged the cold chunk is reported current")
	}

	const callers = 10
	codes := make([]int, callers)
	retry := make([]string, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			w := get()
			codes[i], retry[i] = w.Code, w.Header().Get("Retry-After")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range codes {
		if codes[i] != 202 || retry[i] == "" {
			t.Fatalf("caller %d after the fill: status %d, Retry-After %q; want 202 with a Retry-After", i, codes[i], retry[i])
		}
	}
	distBuildWaitClosed(t, "the rebuild to reach distanceBuildHook", rebuild)
	if n := builds.Load(); n != 2 {
		t.Fatalf("builds = %d, want 2: the %d callers must share one rebuild", n, callers)
	}
	release()
	distBuildWaitCurrent(t, store)

	w = get()
	if w.Code != 200 {
		t.Fatalf("after the rebuild: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	dist622RequireCold(t, "the rebuilt index", dist622PathHashes(t, w), cold, true)
	if n := builds.Load(); n != 2 {
		t.Fatalf("builds = %d, want 2 after the rebuild finished", n)
	}
}

// TestDistanceFill622StaleCompletionKeepsTheInvalidation: the window upstream
// 2281 describes. A build reads the hot-window dataset, the fill merges the
// cold chunk and bumps the generation, and only then does that build publish
// its result. The index it produced predates the merge, so the gate must not
// report it current; exactly one rebuild follows and its 200 carries the
// merged paths. Runs in a child process because a gate that takes s.mu under
// distLazyMu would hang the whole package here (#149).
func TestDistanceFill622StaleCompletionKeepsTheInvalidation(t *testing.T) {
	runDistBuildChild(t, func(t *testing.T) {
		dist622RequireBelowDebounce(t)
		db, cold := dist622Seed(t)
		var builds atomic.Int32
		pass1, release1 := make(chan struct{}), make(chan struct{})
		pass2, release2 := make(chan struct{}), make(chan struct{})
		var release2Once sync.Once
		releasePass2 := func() { release2Once.Do(func() { close(release2) }) }
		store := dist622Store(t, db, func() {
			switch builds.Add(1) {
			case 1:
				close(pass1)
				<-release1
			case 2:
				close(pass2)
				<-release2
			}
		})
		t.Cleanup(releasePass2)
		baseline := runtime.NumGoroutine()
		get := dist622Router(t, db, store)

		if w := get(); w.Code != 202 {
			t.Fatalf("first request: status %d, want 202", w.Code)
		}
		distBuildWaitClosed(t, "pass 1 to reach distanceBuildHook", pass1)
		// Park pass 1 at its bookkeeping: it needs distLazyMu, which the test
		// holds, and by then it has read the hot-window dataset.
		store.distLazyMu.Lock()
		close(release1)
		distBuildWait(t, "pass 1 to read the dataset and wait for distLazyMu", func() bool {
			return distBuildParked("sync.Mutex.Lock", buildFrame)
		})
		dist622Fill(t, store)
		store.distLazyMu.Unlock()

		distBuildWaitClosed(t, "the stale-generation rebuild to start", pass2)
		if store.DistanceIndexBuilt() {
			t.Fatal("the build that read the pre-fill dataset consumed the invalidation: its index is reported current")
		}
		if !store.DistanceIndexBuilding() {
			t.Fatal("DistanceIndexBuilding() = false while the rebuild runs")
		}
		if w := get(); w.Code != 202 {
			t.Fatalf("request during the rebuild: status %d, want 202", w.Code)
		}
		releasePass2()
		distBuildWaitCurrent(t, store)
		distBuildSettle(baseline)

		w := get()
		if w.Code != 200 {
			t.Fatalf("after the rebuild: status %d, want 200 (body=%s)", w.Code, w.Body.String())
		}
		dist622RequireCold(t, "the rebuilt index", dist622PathHashes(t, w), cold, true)
		if n := builds.Load(); n != 2 {
			t.Fatalf("builds = %d, want 2 (the pass on the hot window and one rebuild)", n)
		}
	})
}

// TestDistanceFill622MidFillSnapshotIsNotCurrent: upstream 2281's core
// complaint — "the index then reflects the data from part-way through the
// fill, and the gate says it is current". A build takes its snapshot while the
// chunk loader is between scanning a chunk and merging it, so the dataset it
// reads is missing transmissions the fill goes on to add. The generation that
// build records must be the pre-fill one, so its index is stale the moment the
// fill completes; one rebuild follows and carries the merged paths.
func TestDistanceFill622MidFillSnapshotIsNotCurrent(t *testing.T) {
	dist622RequireBelowDebounce(t)
	db, cold := dist622Seed(t)
	var builds atomic.Int32
	pass1, release1 := make(chan struct{}), make(chan struct{})
	store := dist622Store(t, db, func() {
		if builds.Add(1) == 1 {
			close(pass1)
			<-release1
		}
	})
	// Park the chunk loader between scanning its first chunk and merging it.
	// It holds no store lock there, so the build can take s.mu meanwhile.
	scanned, releaseChunk := make(chan struct{}), make(chan struct{})
	var chunks atomic.Int32
	store.loadChunkScannedHook = func() {
		if chunks.Add(1) == 1 {
			close(scanned)
			<-releaseChunk
		}
	}
	get := dist622Router(t, db, store)

	if w := get(); w.Code != 202 {
		t.Fatalf("first request: status %d, want 202", w.Code)
	}
	distBuildWaitClosed(t, "the build to reach distanceBuildHook", pass1)
	filled := make(chan struct{})
	go func() {
		defer close(filled)
		store.loadBackgroundChunks()
	}()
	distBuildWaitClosed(t, "the chunk loader to scan its first chunk", scanned)
	store.mu.RLock()
	packets := len(store.packets)
	store.mu.RUnlock()
	if packets != dist622HotTx {
		t.Fatalf("the chunk loader merged %d packets before its scan hook, want %d still pending", packets-dist622HotTx, dist622HotTx)
	}

	// Let the build read and publish the half-filled dataset while the fill
	// is still parked.
	close(release1)
	distBuildWait(t, "the mid-fill build pass to finish", func() bool {
		return !store.DistanceIndexBuilding()
	})
	close(releaseChunk)
	distBuildWaitClosed(t, "the background fill to finish", filled)
	store.mu.RLock()
	packets = len(store.packets)
	store.mu.RUnlock()
	if want := dist622HotTx + dist622ColdTx; packets != want {
		t.Fatalf("after the background fill: %d packets, want %d", packets, want)
	}

	if store.DistanceIndexBuilt() {
		t.Fatal("the index built from part-way through the background fill is reported current: the fill's invalidation did not cover the chunks it merged after the build's snapshot")
	}
	if w := get(); w.Code != 202 {
		t.Fatalf("request after the fill: status %d, want 202", w.Code)
	}
	distBuildWaitCurrent(t, store)
	w := get()
	if w.Code != 200 {
		t.Fatalf("after the rebuild: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	dist622RequireCold(t, "the rebuilt index", dist622PathHashes(t, w), cold, true)
	if n := builds.Load(); n != 2 {
		t.Fatalf("builds = %d, want 2 (the mid-fill pass and one rebuild)", n)
	}
}
