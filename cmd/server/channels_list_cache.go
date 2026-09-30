package main

import (
	"crypto/sha256"
	"encoding/hex"
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
}

const (
	channelListTTL     = 60 * time.Second
	channelListMaxKeys = 64
	// channelListMaxKeyBytes caps a stored key. A longer one becomes
	// "sha256:" + 64 hex digits (71 bytes); no normalized region key
	// contains ':', so a digest never equals a short key.
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
}

// reset drops every entry (tests).
func (c *channelListCache) reset() {
	c.mu.Lock()
	c.entries = nil
	c.mu.Unlock()
}

// load returns the cached entry for key or, on a miss, the result of one
// query shared by every concurrent caller of key. missed runs after the
// first cache check fails (a test seam; nil in production).
func (c *channelListCache) load(key string, missed func(), query func() (*channelListEntry, error)) (*channelListEntry, error) {
	key = channelListStoreKey(key)
	if e, ok := c.get(key, time.Now()); ok {
		return e, nil
	}
	if missed != nil {
		missed()
	}
	v, err, _ := c.flights.Do(key, func() (interface{}, error) {
		if e, ok := c.get(key, time.Now()); ok {
			return e, nil
		}
		e, err := query()
		if err != nil {
			return nil, err
		}
		e.expires = time.Now().Add(channelListTTL)
		c.put(key, e, time.Now())
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*channelListEntry), nil
}
