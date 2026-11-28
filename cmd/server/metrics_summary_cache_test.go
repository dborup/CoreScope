package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func summaryRows(ids ...string) []MetricsSummaryRow {
	out := make([]MetricsSummaryRow, 0, len(ids))
	for _, id := range ids {
		out = append(out, MetricsSummaryRow{ObserverID: id})
	}
	return out
}

// Concurrent misses for one key must run the computation once (#506).
func TestMetricsSummaryCacheSingleflight(t *testing.T) {
	var c metricsSummaryCache
	var calls atomic.Int32
	release := make(chan struct{})
	var started sync.WaitGroup
	const n = 20
	started.Add(n)
	var done sync.WaitGroup
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			started.Done()
			rows, err := c.load("1h", func() ([]MetricsSummaryRow, error) {
				calls.Add(1)
				<-release
				return summaryRows("a"), nil
			})
			if err != nil || len(rows) != 1 {
				t.Errorf("load = %v, %v", rows, err)
			}
		}()
	}
	started.Wait()
	time.Sleep(50 * time.Millisecond) // let every goroutine reach the flight
	close(release)
	done.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("computations = %d, want 1", got)
	}
}

func TestMetricsSummaryCacheHitAndTTL(t *testing.T) {
	var c metricsSummaryCache
	var calls int
	q := func() ([]MetricsSummaryRow, error) { calls++; return summaryRows("a"), nil }
	c.load("1h", q)
	c.load("1h", q)
	if calls != 1 {
		t.Fatalf("second call within TTL recomputed (calls=%d)", calls)
	}
	// Expire the entry: the next call recomputes.
	c.mu.Lock()
	c.entries["1h"].expires = time.Now().Add(-time.Second)
	c.mu.Unlock()
	c.load("1h", q)
	if calls != 2 {
		t.Fatalf("expired entry not recomputed (calls=%d)", calls)
	}
	// Different keys do not share an entry.
	c.load("24h", q)
	if calls != 3 {
		t.Fatalf("distinct key served from another key's entry (calls=%d)", calls)
	}
}

func TestMetricsSummaryCacheErrorNotStored(t *testing.T) {
	var c metricsSummaryCache
	var calls int
	_, err := c.load("1h", func() ([]MetricsSummaryRow, error) { calls++; return nil, errors.New("boom") })
	if err == nil {
		t.Fatal("expected error")
	}
	rows, err := c.load("1h", func() ([]MetricsSummaryRow, error) { calls++; return summaryRows("a"), nil })
	if err != nil || len(rows) != 1 || calls != 2 {
		t.Fatalf("error was cached: rows=%v err=%v calls=%d", rows, err, calls)
	}
}

// The window comes from a query parameter, so the key space is caller
// controlled and must stay bounded.
func TestMetricsSummaryCacheBounded(t *testing.T) {
	var c metricsSummaryCache
	for i := 1; i <= metricsSummaryMaxKeys*3; i++ {
		k := fmt.Sprintf("%dh", i)
		c.load(k, func() ([]MetricsSummaryRow, error) { return summaryRows("a"), nil })
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > metricsSummaryMaxKeys {
		t.Fatalf("entries = %d, want <= %d", n, metricsSummaryMaxKeys)
	}
}

// The handler serves the second request from the cache, equivalent windows
// share one entry, and the region filter never leaks into the cached rows.
func TestMetricsSummaryHandlerCached(t *testing.T) {
	srv, router := setupTestServer(t)
	now := time.Now().UTC()
	ins := func(id string) {
		t.Helper()
		if _, err := srv.db.conn.Exec(`INSERT INTO observer_metrics (observer_id, timestamp, noise_floor) VALUES (?, ?, -100)`,
			id, now.Add(-5*time.Minute).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	get := func(q string) []MetricsSummaryRow {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/observers/metrics/summary?"+q, nil))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var resp MetricsSummaryResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Observers
	}
	ins("obs-cache-1")
	first := len(get("window=1h"))
	if first == 0 {
		t.Fatal("expected at least one observer")
	}
	ins("obs-cache-2") // new data appears only after the TTL
	if got := len(get("window=1h")); got != first {
		t.Fatalf("second request not served from cache: %d rows, want %d", got, first)
	}
	// "60m" is the same 1 h window and must share the entry.
	if got := len(get("window=60m")); got != first {
		t.Fatalf("equivalent window did not share the entry: %d rows, want %d", got, first)
	}
	// A region filter applies to a copy; the unfiltered request is unchanged.
	if got := len(get("window=1h&region=NOPE")); got != 0 {
		t.Fatalf("region filter returned %d rows", got)
	}
	if got := len(get("window=1h")); got != first {
		t.Fatalf("region filter leaked into the cache: %d rows, want %d", got, first)
	}
	// After expiry the new observer shows up.
	srv.db.metricsSummary.mu.Lock()
	for _, e := range srv.db.metricsSummary.entries {
		e.expires = time.Now().Add(-time.Second)
	}
	srv.db.metricsSummary.mu.Unlock()
	if got := len(get("window=1h")); got != first+1 {
		t.Fatalf("expired entry not refreshed: %d rows, want %d", got, first+1)
	}
}

// Distinct windows must not share an entry, and filtering by a region that
// matches only some rows must not rewrite the cached rows (#506).
func TestMetricsSummaryHandlerWindowsAndRegionIsolated(t *testing.T) {
	srv, router := setupTestServer(t)
	now := time.Now().UTC()
	for _, o := range []struct{ id, iata string }{{"iso-a", "AAA"}, {"iso-b", "BBB"}} {
		if _, err := srv.db.conn.Exec(`INSERT OR REPLACE INTO observers (id, name, iata) VALUES (?, ?, ?)`, o.id, o.id, o.iata); err != nil {
			t.Fatal(err)
		}
		// One sample 3 h ago: inside 24h, outside 1h.
		if _, err := srv.db.conn.Exec(`INSERT INTO observer_metrics (observer_id, timestamp, noise_floor) VALUES (?, ?, -100)`,
			o.id, now.Add(-3*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(q string) map[string]bool {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/observers/metrics/summary?"+q, nil))
		var resp MetricsSummaryResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, r := range resp.Observers {
			m[r.ObserverID] = true
		}
		return m
	}
	if got := ids("window=1h"); got["iso-a"] || got["iso-b"] {
		t.Fatalf("1h window shows 3 h old samples: %v", got)
	}
	if got := ids("window=24h"); !got["iso-a"] || !got["iso-b"] {
		t.Fatalf("24h window served from the 1h entry: %v", got)
	}
	if got := ids("window=24h&region=bbb"); len(got) != 1 || !got["iso-b"] {
		t.Fatalf("region filter = %v, want only iso-b", got)
	}
	if got := ids("window=24h"); !got["iso-a"] || !got["iso-b"] {
		t.Fatalf("region filter rewrote the cached rows: %v", got)
	}
}

// Realistic fixture: cold vs warm latency of the handler.
func BenchmarkMetricsSummaryHandler(b *testing.B) {
	db := setupTestDB(b)
	now := time.Now().UTC()
	tx, _ := db.conn.Begin()
	for o := 0; o < 60; o++ {
		for m := 0; m < 720; m++ { // 1 sample / 5 min over 60 h
			tx.Exec(`INSERT INTO observer_metrics (observer_id, timestamp, noise_floor, tx_air_secs, rx_air_secs) VALUES (?, ?, ?, ?, ?)`,
				fmt.Sprintf("obs%03d", o), now.Add(-time.Duration(m)*5*time.Minute).Format(time.RFC3339), -100+m%7, 1000-m, 2000-m)
		}
	}
	tx.Commit()
	b.Run("cold", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			db.GetMetricsSummary(now.Add(-24*time.Hour).Format(time.RFC3339), now)
		}
	})
	b.Run("warm", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			db.metricsSummary.load("24h", func() ([]MetricsSummaryRow, error) {
				return db.GetMetricsSummary(now.Add(-24*time.Hour).Format(time.RFC3339), now)
			})
		}
	})
}
