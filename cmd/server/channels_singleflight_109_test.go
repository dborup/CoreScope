package main

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #109: concurrent cache misses for one region must run one SQLite
// query, for GetChannels and GetEncryptedChannels, without mixing regions,
// caching errors or losing #107's index pinning and #98's slice protection.
// The counts come from channelsQueryHook, which runs once per real query.

func singleflightDB(t *testing.T) *DB {
	t.Helper()
	db := pinTestDB(t, true, false)
	pinTestSeed(t, db, true, 2000, 109)
	return db
}

// queryCounter counts real queries per kind. The first query of a flight
// waits until either `want` queries are running at once (the no-coalescing
// failure mode: every caller queries) or a short grace period passes, so
// concurrent callers really overlap it.
type queryCounter struct {
	mu      sync.Mutex
	n       map[string]int
	running atomic.Int32
	want    int32
	failAll error
}

func (q *queryCounter) hook(kind, _ string) error {
	q.mu.Lock()
	if q.n == nil {
		q.n = map[string]int{}
	}
	q.n[kind]++
	q.mu.Unlock()
	q.running.Add(1)
	deadline := time.Now().Add(300 * time.Millisecond)
	for q.running.Load() < q.want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return q.failAll
}

func (q *queryCounter) count(kind string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.n[kind]
}

// callConcurrently starts n callers at once and returns their results.
func callConcurrently(n int, fn func(i int) (interface{}, error)) ([]interface{}, []error) {
	res := make([]interface{}, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res[i], errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return res, errs
}

func TestGetChannelsCoalescesConcurrentMisses_109(t *testing.T) {
	db := singleflightDB(t)
	q := &queryCounter{want: 16}
	db.channelsQueryHook = q.hook
	res, errs := callConcurrently(16, func(int) (interface{}, error) { return db.GetChannels("AAR") })
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := q.count("channels"); got != 1 {
		t.Fatalf("16 concurrent cold GetChannels(AAR) ran %d queries, want 1", got)
	}
	for i := 1; i < len(res); i++ {
		if !reflect.DeepEqual(res[i], res[0]) {
			t.Fatalf("caller %d got a different result", i)
		}
	}
}

// " aar ", "AAR" and "aar,AAR" are one normalized region.
func TestGetChannelsCoalescesOnTheNormalizedRegion_109(t *testing.T) {
	db := singleflightDB(t)
	q := &queryCounter{want: 12}
	db.channelsQueryHook = q.hook
	forms := []string{"AAR", " aar ", "aar,AAR", "Aar"}
	_, errs := callConcurrently(12, func(i int) (interface{}, error) { return db.GetChannels(forms[i%len(forms)]) })
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := q.count("channels"); got != 1 {
		t.Fatalf("12 concurrent cold calls for one normalized region ran %d queries, want 1", got)
	}
}

func TestGetEncryptedChannelsCoalescesConcurrentMisses_109(t *testing.T) {
	db := singleflightDB(t)
	q := &queryCounter{want: 16}
	db.channelsQueryHook = q.hook
	res, errs := callConcurrently(16, func(int) (interface{}, error) { return db.GetEncryptedChannels("CPH") })
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := q.count("encrypted"); got != 1 {
		t.Fatalf("16 concurrent cold GetEncryptedChannels(CPH) ran %d queries, want 1", got)
	}
	if len(res[0].([]map[string]interface{})) == 0 {
		t.Fatal("fixture: no encrypted channels for CPH")
	}
	// and within the TTL a later call is served from the cache
	if _, err := db.GetEncryptedChannels("CPH"); err != nil {
		t.Fatal(err)
	}
	if got := q.count("encrypted"); got != 1 {
		t.Fatalf("a call within the TTL ran a query again (%d queries)", got)
	}
}

// Different regions are separate flights and each gets its own result,
// identical to what an uncoalesced call returns.
func TestDifferentRegionsAreNeverMixed_109(t *testing.T) {
	regions := []string{"AAR", "CPH", "", "OSL"}
	want := map[string][2]interface{}{}
	for _, r := range regions {
		fresh := singleflightDB(t)
		ch, err := fresh.GetChannels(r)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := fresh.GetEncryptedChannels(r)
		if err != nil {
			t.Fatal(err)
		}
		want[r] = [2]interface{}{ch, enc}
	}
	if reflect.DeepEqual(want["AAR"][0], want["CPH"][0]) || reflect.DeepEqual(want["AAR"][1], want["CPH"][1]) {
		t.Fatal("fixture: AAR and CPH give the same channels; the test could not tell them apart")
	}

	db := singleflightDB(t)
	q := &queryCounter{want: 32}
	db.channelsQueryHook = q.hook
	res, errs := callConcurrently(32, func(i int) (interface{}, error) {
		r := regions[(i/2)%len(regions)] // every region with both kinds
		if i%2 == 0 {
			ch, err := db.GetChannels(r)
			return [2]interface{}{r, ch}, err
		}
		enc, err := db.GetEncryptedChannels(r)
		return [2]interface{}{r, enc}, err
	})
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		got := res[i].([2]interface{})
		r := got[0].(string)
		if !reflect.DeepEqual(got[1], want[r][i%2]) {
			t.Fatalf("caller %d (region %q, kind %d) got another region's result", i, r, i%2)
		}
	}
	if c, e := q.count("channels"), q.count("encrypted"); c != len(regions) || e != len(regions) {
		t.Fatalf("queries: channels=%d encrypted=%d, want %d each (one per region)", c, e, len(regions))
	}
}

// An error is shared only by the callers of that flight and is not cached:
// the next call queries again and succeeds.
func TestChannelErrorsAreSharedButNotCached_109(t *testing.T) {
	for _, kind := range []string{"channels", "encrypted"} {
		t.Run(kind, func(t *testing.T) {
			db := singleflightDB(t)
			boom := errors.New("injected query failure")
			q := &queryCounter{want: 8, failAll: boom}
			db.channelsQueryHook = q.hook
			call := func() (interface{}, error) {
				if kind == "channels" {
					return db.GetChannels("AAR")
				}
				return db.GetEncryptedChannels("AAR")
			}
			_, errs := callConcurrently(8, func(int) (interface{}, error) { return call() })
			for i, err := range errs {
				if !errors.Is(err, boom) {
					t.Fatalf("caller %d: err %v, want the flight's error", i, err)
				}
			}
			if got := q.count(kind); got != 1 {
				t.Fatalf("the failing flight ran %d queries, want 1", got)
			}
			q.failAll = nil
			res, err := call()
			if err != nil {
				t.Fatalf("after the error: %v", err)
			}
			if res == nil {
				t.Fatal("after the error: nil result")
			}
			if got := q.count(kind); got != 2 {
				t.Fatalf("the error was cached: %d queries after a failed and a fresh call, want 2", got)
			}
		})
	}
}

// A caller that missed the cache before a flight finished, and reaches
// singleflight only after it has, must find the fresh result on the second
// cache check inside its own flight instead of querying again.
func TestSecondCacheCheckInsideTheFlight_109(t *testing.T) {
	for _, kind := range []string{"channels", "encrypted"} {
		t.Run(kind, func(t *testing.T) {
			db := singleflightDB(t)
			q := &queryCounter{want: 1}
			db.channelsQueryHook = q.hook
			var misses atomic.Int32
			release := make(chan struct{})
			db.channelsMissHook = func(k, _ string) {
				if k == kind && misses.Add(1) == 1 {
					<-release // the first caller is held right after its miss
				}
			}
			call := func() error {
				var err error
				if kind == "channels" {
					_, err = db.GetChannels("OSL")
				} else {
					_, err = db.GetEncryptedChannels("OSL")
				}
				return err
			}
			held := make(chan error, 1)
			go func() { held <- call() }()
			for misses.Load() < 1 {
				time.Sleep(time.Millisecond)
			}
			// a second caller misses too, runs the query and fills the cache
			if err := call(); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-held; err != nil {
				t.Fatal(err)
			}
			if got := q.count(kind); got != 1 {
				t.Fatalf("%d queries: the held caller queried again instead of re-checking the cache inside its flight", got)
			}
		})
	}
}

// The region comes from a query parameter, so the cache must stay bounded
// however many distinct regions callers send.
func TestChannelListCacheIsBounded_109(t *testing.T) {
	var c channelListCache
	now := time.Now()
	for i := 0; i < 1000; i++ {
		key := channelsRegionKey(strings.Repeat("X", 1+i%5) + strconv.Itoa(i))
		c.put(key, &channelListEntry{expires: now.Add(channelListTTL)}, now)
		if len(c.entries) > channelListMaxKeys {
			t.Fatalf("cache grew to %d entries", len(c.entries))
		}
	}
	// expired entries go first: a fresh key replaces an expired one, not a live one
	c.reset()
	for i := 0; i < channelListMaxKeys; i++ {
		exp := now.Add(channelListTTL)
		if i == 7 {
			exp = now.Add(-time.Second)
		}
		c.put(strconv.Itoa(i), &channelListEntry{expires: exp}, now)
	}
	c.put("new", &channelListEntry{expires: now.Add(channelListTTL)}, now)
	if _, ok := c.entries["7"]; ok {
		t.Error("the expired entry survived")
	}
	if len(c.entries) != channelListMaxKeys {
		t.Errorf("%d entries, want %d", len(c.entries), channelListMaxKeys)
	}
}

func TestChannelsRegionKey_109(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "all": "", " aar ": "AAR", "cph,AAR,aar": "AAR,CPH", "AAR, ,CPH": "AAR,CPH",
	} {
		if got := channelsRegionKey(in); got != want {
			t.Errorf("channelsRegionKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// failingRows passes `ok` rows through, then stops as a failed SQLite step
// does: Next returns false and Err reports the failure.
type failingRows struct {
	channelRows
	ok  int
	err error
}

func (r *failingRows) Next() bool {
	if r.ok == 0 {
		return false
	}
	r.ok--
	return r.channelRows.Next()
}

func (r *failingRows) Err() error {
	if r.ok == 0 {
		return r.err
	}
	return r.channelRows.Err()
}

// A failure part-way through the rows must fail the call, not return (and
// cache for the TTL) the truncated list as a success.
func TestChannelRowsErrorIsNotCachedAsSuccess_109(t *testing.T) {
	for _, kind := range []string{"channels", "encrypted"} {
		t.Run(kind, func(t *testing.T) {
			db := singleflightDB(t)
			call := func() ([]map[string]interface{}, error) {
				if kind == "channels" {
					return db.GetChannels("")
				}
				return db.GetEncryptedChannels("")
			}
			full, err := call()
			if err != nil {
				t.Fatal(err)
			}
			if len(full) < 2 {
				t.Fatalf("fixture gives %d rows, need at least 2 to truncate", len(full))
			}
			db.channelsCache.reset()
			db.encChannelsCache.reset()

			q := &queryCounter{want: 1}
			db.channelsQueryHook = q.hook
			boom := errors.New("injected step failure")
			db.channelsRowsHook = func(_ string, rows channelRows) channelRows {
				return &failingRows{channelRows: rows, ok: 1, err: boom}
			}
			res, err := call()
			if !errors.Is(err, boom) {
				t.Fatalf("iteration failed after 1 of %d rows: got %d rows, err %v; want the step error", len(full), len(res), err)
			}
			db.channelsRowsHook = nil
			res, err = call()
			if err != nil {
				t.Fatalf("after the failed iteration: %v", err)
			}
			if got := q.count(kind); got != 2 {
				t.Fatalf("%d queries after a failed and a fresh call, want 2: the truncated list was cached", got)
			}
			if !reflect.DeepEqual(res, full) {
				t.Fatalf("after the failed iteration: %d rows, want the full %d", len(res), len(full))
			}
		})
	}
}

// BenchmarkColdConcurrentGetChannels_109: 16 callers hit a cold (reset)
// cache for one region at once; reports the real queries per round.
func BenchmarkColdConcurrentGetChannels_109(b *testing.B) {
	db := pinTestDB(b, true, false)
	pinTestSeed(b, db, true, 20000, 109)
	var queries atomic.Int64
	db.channelsQueryHook = func(string, string) error { queries.Add(1); return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.channelsCache.reset()
		db.encChannelsCache.reset()
		callConcurrently(16, func(j int) (interface{}, error) {
			if j%2 == 0 {
				return db.GetChannels("AAR")
			}
			return db.GetEncryptedChannels("AAR")
		})
	}
	b.ReportMetric(float64(queries.Load())/float64(b.N), "queries/round")
}
