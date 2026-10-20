package main

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// metricsSummaryCache backs GET /api/observers/metrics/summary (#506).
//
//   - Keyed by the parsed window duration (metricsSummaryKey), so "1h" and
//     "60m" share an entry. The region filter is applied by the handler on a
//     copy, so it is not part of the key.
//   - Concurrent misses for one key run the query once (singleflight); the
//     flight checks the cache again first.
//   - Only successful results are stored.
//   - Bounded: at most metricsSummaryMaxKeys entries. The window comes from
//     a query parameter, so when full, expired entries go first, then the one
//     closest to expiry.
//   - No lock is held while the query runs.
//
// A stored slice is shared by every caller and must not be modified.
type metricsSummaryCache struct {
	mu      sync.Mutex
	entries map[string]*metricsSummaryEntry
	flights singleflight.Group
}

type metricsSummaryEntry struct {
	rows    []MetricsSummaryRow
	expires time.Time
}

const (
	metricsSummaryTTL     = 30 * time.Second
	metricsSummaryMaxKeys = 16
)

// metricsSummaryKey is the cache and flight key for a window duration.
func metricsSummaryKey(d time.Duration) string { return d.String() }

func (c *metricsSummaryCache) get(key string, now time.Time) ([]MetricsSummaryRow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || !now.Before(e.expires) {
		return nil, false
	}
	return e.rows, true
}

func (c *metricsSummaryCache) put(key string, rows []MetricsSummaryRow, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*metricsSummaryEntry)
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= metricsSummaryMaxKeys {
		for k, old := range c.entries {
			if !now.Before(old.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= metricsSummaryMaxKeys {
			victim, first := "", true
			var soonest time.Time
			for k, old := range c.entries {
				if first || old.expires.Before(soonest) {
					victim, soonest, first = k, old.expires, false
				}
			}
			delete(c.entries, victim)
		}
	}
	c.entries[key] = &metricsSummaryEntry{rows: rows, expires: now.Add(metricsSummaryTTL)}
}

// load returns the cached rows for key or, on a miss, the result of one
// query shared by every concurrent caller of key.
func (c *metricsSummaryCache) load(key string, query func() ([]MetricsSummaryRow, error)) ([]MetricsSummaryRow, error) {
	if rows, ok := c.get(key, time.Now()); ok {
		return rows, nil
	}
	v, err, _ := c.flights.Do(key, func() (interface{}, error) {
		if rows, ok := c.get(key, time.Now()); ok {
			return rows, nil
		}
		rows, err := query()
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []MetricsSummaryRow{}
		}
		c.put(key, rows, time.Now())
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]MetricsSummaryRow), nil
}
