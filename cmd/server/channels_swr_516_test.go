package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #516: stale-while-revalidate for the channel-list cache. An expired
// entry is served at once while one background refresh runs; only an empty
// key waits for the query. Tests drive the cache with a fake clock.

type swrClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *swrClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *swrClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newSWRCache() (*channelListCache, *swrClock) {
	clk := &swrClock{t: time.Unix(1_700_000_000, 0)}
	return &channelListCache{clock: clk.now}, clk
}

func swrEntry(v int) *channelListEntry {
	return &channelListEntry{channels: []map[string]interface{}{{"v": v}}}
}

func swrVal(e *channelListEntry) int { return e.channels[0]["v"].(int) }

func TestSWRExpiredEntryServedImmediatelyWithOneRefresh_516(t *testing.T) {
	c, clk := newSWRCache()
	var queries atomic.Int32
	release := make(chan struct{})
	q := func() (*channelListEntry, error) {
		n := int(queries.Add(1))
		if n > 1 {
			<-release // the refresh blocks until released
		}
		return swrEntry(n), nil
	}
	if e, err := c.load("AAR", nil, q); err != nil || swrVal(e) != 1 {
		t.Fatalf("first load: %v %v", e, err)
	}
	clk.add(channelListTTL + time.Second)

	done := make(chan struct{})
	var wrong atomic.Int32
	go func() {
		defer close(done)
		callConcurrently(32, func(int) (interface{}, error) {
			e, err := c.load("AAR", nil, q)
			if err != nil || swrVal(e) != 1 {
				wrong.Add(1)
			}
			return nil, nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callers waited for the refresh instead of getting the stale entry")
	}
	if wrong.Load() != 0 {
		t.Fatalf("%d callers did not get the stale entry", wrong.Load())
	}
	close(release)
	c.wait()
	if n := queries.Load(); n != 2 {
		t.Fatalf("queries = %d, want 2 (initial + exactly one refresh)", n)
	}
	if e, _ := c.load("AAR", nil, q); swrVal(e) != 2 {
		t.Fatalf("after refresh the entry should be the new one, got %d", swrVal(e))
	}
	if n := queries.Load(); n != 2 {
		t.Fatalf("fresh hit queried again: %d", n)
	}
}

func TestSWRStalenessIsBounded_516(t *testing.T) {
	c, clk := newSWRCache()
	var queries atomic.Int32
	q := func() (*channelListEntry, error) { return swrEntry(int(queries.Add(1))), nil }
	c.load("AAR", nil, q)

	clk.add(channelListTTL + channelListMaxStale - time.Second) // still servable
	if e, _ := c.load("AAR", nil, q); swrVal(e) != 1 {
		t.Fatalf("inside the stale bound the old entry must be served, got %d", swrVal(e))
	}
	c.wait()

	c2, clk2 := newSWRCache()
	queries.Store(0)
	c2.load("AAR", nil, q)
	clk2.add(channelListTTL + channelListMaxStale + time.Second) // too old
	e, _ := c2.load("AAR", nil, q)
	if swrVal(e) != 2 {
		t.Fatalf("beyond the stale bound the caller must wait for a fresh query, got %d", swrVal(e))
	}
}

func TestSWRFailedRefreshKeepsStaleWithoutStampede_516(t *testing.T) {
	c, clk := newSWRCache()
	var queries atomic.Int32
	var fail atomic.Bool
	q := func() (*channelListEntry, error) {
		n := int(queries.Add(1))
		if fail.Load() {
			return nil, errors.New("boom")
		}
		return swrEntry(n), nil
	}
	c.load("AAR", nil, q)
	fail.Store(true)
	clk.add(channelListTTL + time.Second)

	for i := 0; i < 20; i++ {
		e, err := c.load("AAR", nil, q)
		if err != nil || swrVal(e) != 1 {
			t.Fatalf("failed refresh must keep serving stale: %v %v", e, err)
		}
		c.wait()
	}
	if n := queries.Load(); n != 2 {
		t.Fatalf("queries = %d, want 2: a failing refresh must back off, not retry per request", n)
	}
	// After the retry delay a new refresh is attempted and can succeed.
	fail.Store(false)
	clk.add(channelListRetryDelay + time.Second)
	c.load("AAR", nil, q)
	c.wait()
	if e, _ := c.load("AAR", nil, q); swrVal(e) != 3 {
		t.Fatalf("refresh after the retry delay should replace the entry, got %d", swrVal(e))
	}
}

func TestSWREmptyKeyWaitsAndErrorsAreNotCached_516(t *testing.T) {
	c, _ := newSWRCache()
	var queries atomic.Int32
	q := func() (*channelListEntry, error) { queries.Add(1); return nil, errors.New("boom") }
	if _, err := c.load("AAR", nil, q); err == nil {
		t.Fatal("empty cache: the error must reach the caller")
	}
	if _, err := c.load("AAR", nil, q); err == nil || queries.Load() != 2 {
		t.Fatalf("errors must not be cached (queries=%d)", queries.Load())
	}
}

func TestSWRBackgroundRefreshesAreBounded_516(t *testing.T) {
	c, clk := newSWRCache()
	ok := func() (*channelListEntry, error) { return swrEntry(1), nil }
	keys := []string{"A", "B", "C", "D", "E", "F"}
	for _, k := range keys {
		c.load(k, nil, ok)
	}
	clk.add(channelListTTL + time.Second)
	var running, peak atomic.Int32
	release := make(chan struct{})
	slow := func() (*channelListEntry, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return swrEntry(2), nil
	}
	for _, k := range keys {
		if e, err := c.load(k, nil, slow); err != nil || swrVal(e) != 1 {
			t.Fatalf("stale expected for %s", k)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if p := peak.Load(); p > channelListMaxRefreshes {
		t.Fatalf("%d concurrent background refreshes, cap is %d", p, channelListMaxRefreshes)
	}
	close(release)
	c.wait()
}

func TestSWRResetDropsStaleEntries_516(t *testing.T) {
	c, clk := newSWRCache()
	var queries atomic.Int32
	q := func() (*channelListEntry, error) { return swrEntry(int(queries.Add(1))), nil }
	c.load("AAR", nil, q)
	clk.add(channelListTTL + time.Second)
	c.reset()
	if e, _ := c.load("AAR", nil, q); swrVal(e) != 2 {
		t.Fatalf("after reset the caller must wait for a fresh query, got %d", swrVal(e))
	}
}

// DB level: GetChannels(region) and GetEncryptedChannels(region) both use it.
func TestSWRGetChannelsServesExpiredWhileRefreshing_516(t *testing.T) {
	db := singleflightDB(t)
	clk := &swrClock{t: time.Now()}
	db.channelsCache.clock = clk.now
	db.encChannelsCache.clock = clk.now
	var queries atomic.Int32
	block := make(chan struct{})
	db.channelsQueryHook = func(kind, _ string) error {
		if queries.Add(1) > 2 {
			<-block
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		if _, err := db.GetChannels("AAR"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.GetEncryptedChannels("AAR"); err != nil {
		t.Fatal(err)
	}
	// queries: GetChannels once (2nd call cached) + encrypted once = 2
	clk.add(channelListTTL + time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		db.GetChannels("AAR")
		db.GetEncryptedChannels("AAR")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expired region list blocked on the cold query")
	}
	close(block)
	db.channelsCache.wait()
	db.encChannelsCache.wait()
}

// BenchmarkExpiredRegionList_516 measures what a caller sees when the cached
// region list has just expired: with stale-while-revalidate it is a map
// lookup, without it the full cold query.
func BenchmarkExpiredRegionList_516(b *testing.B) {
	db := pinTestDB(b, true, false)
	pinTestSeed(b, db, true, 30000, 516)
	clk := &swrClock{t: time.Now()}
	db.channelsCache.clock = clk.now
	if _, err := db.GetChannels("AAR"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clk.add(channelListTTL + time.Second)
		if _, err := db.GetChannels("AAR"); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		db.channelsCache.wait() // let the refresh finish outside the timed region
		b.StartTimer()
	}
}
