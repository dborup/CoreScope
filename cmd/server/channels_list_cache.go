package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// channelListCache backs DB.GetChannels and DB.GetEncryptedChannels (#109).
//
//   - Keyed by the normalized region (channelsRegionKey), so " aar " and
//     "AAR,aar" share an entry and a flight; different regions never do.
//   - Concurrent misses for one key run the query once (singleflight). The
//     flight checks the cache again first: a caller that missed before
//     another flight stored a result must not query again.
//   - Stale-while-revalidate (#516): an expired entry is served at once and
//     one background refresh per key (singleflight, at most
//     channelListMaxRefreshes at a time) replaces it. Only an empty key, or
//     one expired more than channelListMaxStale ago, waits for the query. A
//     failed refresh keeps the stale entry and is retried after
//     channelListRetryDelay.
//   - Only successful results are stored; an error is returned to the
//     callers of that flight and the next call queries again.
//   - Bounded: at most channelListMaxKeys entries. The region comes from a
//     query parameter, so the key space is caller-controlled; when full,
//     expired entries are dropped first, then the one closest to expiry.
//     A key longer than channelListMaxKeyBytes is stored (and used as the
//     flight key) as its SHA-256 digest, so each stored key is small too.
//
// A stored entry is never modified. Its channels slice is shared by every
// caller, so callers must not write into it (handleChannels appends with a
// full slice expression, #98).
type channelListCache struct {
	mu      sync.Mutex
	entries map[string]*channelListEntry
	flights singleflight.Group
	// Stale-while-revalidate state (#516), all guarded by mu. refreshing is
	// the set of keys with a background refresh running; retryAt delays the
	// next refresh of a key whose last refresh failed. Both only hold keys
	// that have an entry (put/reset prune them), so they stay bounded.
	refreshing map[string]bool
	retryAt    map[string]time.Time
	bg         sync.WaitGroup
	// gen counts reset() calls (#519), guarded by mu. A refresh stores its
	// result only if gen is unchanged since it started.
	gen uint64
	// clock is a test seam; nil means time.Now.
	clock func() time.Time
}

const (
	channelListTTL     = 60 * time.Second
	channelListMaxKeys = 64
	// channelListMaxStale is how long past its TTL an entry may still be
	// served while a background refresh runs (#516). Beyond it callers wait
	// for a fresh query, so the data is never older than TTL + MaxStale.
	channelListMaxStale = 4 * channelListTTL
	// channelListRetryDelay is the wait before a failed refresh is retried,
	// so a failing database is not queried once per request.
	channelListRetryDelay = 10 * time.Second
	// channelListMaxRefreshes caps background refreshes running at once
	// across keys; a key over the cap keeps serving stale and tries again
	// on a later request.
	channelListMaxRefreshes = 2
	// channelListMaxKeyBytes caps a stored key. A longer one becomes
	// "sha256:" + 64 hex digits (71 bytes). Normalized region codes are
	// upper-case (normalizeRegionCodes), so no short key starts with the
	// lower-case "sha256:" and a digest never equals a short key.
	channelListMaxKeyBytes = 256
)

// channelListStoreKey is the key the cache and the flight use for key.
func channelListStoreKey(key string) string {
	if len(key) <= channelListMaxKeyBytes {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// channelsRegionKey is the cache and flight key for a region parameter: the
// normalized codes (normalizeRegionCodes), sorted and de-duplicated. The
// query runs with normalizeRegionCodes(key), the same set of codes.
func channelsRegionKey(region string) string {
	codes := normalizeRegionCodes(region)
	if len(codes) == 0 {
		return ""
	}
	sort.Strings(codes)
	return strings.Join(slices.Compact(codes), ",")
}

func (c *channelListCache) get(key string, now time.Time) (*channelListEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || !now.Before(e.expires) {
		return nil, false
	}
	return e, true
}

func (c *channelListCache) put(key string, e *channelListEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(key, e, now)
}

// putIfGen stores e unless reset() ran since gen was read (#519).
func (c *channelListCache) putIfGen(key string, e *channelListEntry, now time.Time, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen {
		c.putLocked(key, e, now)
	}
}

func (c *channelListCache) putLocked(key string, e *channelListEntry, now time.Time) {
	if c.entries == nil {
		c.entries = make(map[string]*channelListEntry)
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= channelListMaxKeys {
		for k, old := range c.entries {
			if !now.Before(old.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= channelListMaxKeys {
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
	c.entries[key] = e
	delete(c.retryAt, key)
	for k := range c.retryAt {
		if _, ok := c.entries[k]; !ok {
			delete(c.retryAt, k)
		}
	}
}

// reset drops every entry (decode backfill, tests).
func (c *channelListCache) reset() {
	c.mu.Lock()
	c.entries = nil
	c.retryAt = nil
	c.gen++
	c.mu.Unlock()
}

// wait blocks until the running background refreshes finish (tests).
func (c *channelListCache) wait() { c.bg.Wait() }

func (c *channelListCache) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// peek returns the entry for key even when expired, with whether it is
// still fresh.
func (c *channelListCache) peek(key string, now time.Time) (e *channelListEntry, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e = c.entries[key]
	return e, e != nil && now.Before(e.expires)
}

// startRefresh starts one background refresh of key unless one is running,
// the key is backing off after a failure, or channelListMaxRefreshes are
// already running. The query runs without holding mu.
func (c *channelListCache) startRefresh(key string, query func() (*channelListEntry, error)) {
	now := c.now()
	c.mu.Lock()
	if c.refreshing[key] || len(c.refreshing) >= channelListMaxRefreshes || now.Before(c.retryAt[key]) {
		c.mu.Unlock()
		return
	}
	if c.refreshing == nil {
		c.refreshing = make(map[string]bool)
	}
	c.refreshing[key] = true
	gen := c.gen
	c.bg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.bg.Done()
		_, err, _ := c.flights.Do(key, func() (e interface{}, err error) {
			// A panic here would kill the process (#519); report it as a
			// failed refresh so the stale entry stays and the retry backs off.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[channels] background refresh of %q panicked: %v", key, r)
					e, err = nil, fmt.Errorf("refresh panicked: %v", r)
				}
			}()
			if cur, ok := c.get(key, c.now()); ok {
				return cur, nil
			}
			ne, err := query()
			if err != nil {
				return nil, err
			}
			ne.expires = c.now().Add(channelListTTL)
			c.putIfGen(key, ne, c.now(), gen)
			return ne, nil
		})
		c.mu.Lock()
		delete(c.refreshing, key)
		if err != nil {
			if c.retryAt == nil {
				c.retryAt = make(map[string]time.Time)
			}
			if _, ok := c.entries[key]; ok {
				c.retryAt[key] = c.now().Add(channelListRetryDelay)
			}
		}
		c.mu.Unlock()
	}()
}

// load returns the cached entry for key or, on a miss, the result of one
// query shared by every concurrent caller of key. missed runs after the
// first cache check fails (a test seam; nil in production).
func (c *channelListCache) load(key string, missed func(), query func() (*channelListEntry, error)) (*channelListEntry, error) {
	key = channelListStoreKey(key)
	if e, fresh := c.peek(key, c.now()); e != nil {
		if fresh {
			return e, nil
		}
		// Expired but within the stale bound: serve it and refresh in the
		// background (#516).
		if c.now().Before(e.expires.Add(channelListMaxStale)) {
			c.startRefresh(key, query)
			return e, nil
		}
	}
	if missed != nil {
		missed()
	}
	v, err, _ := c.flights.Do(key, func() (interface{}, error) {
		if e, ok := c.get(key, c.now()); ok {
			return e, nil
		}
		e, err := query()
		if err != nil {
			return nil, err
		}
		e.expires = c.now().Add(channelListTTL)
		c.put(key, e, c.now())
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*channelListEntry), nil
}
