package dhm

import (
	"container/list"
	"sync"
	"time"
)

// Cache is an LRU of upstream responses, bounded by entry count and total
// bytes, with a TTL checked on read. It is safe for concurrent use.
//
// A cache made with NewCacheStale also keeps an entry past its TTL, until
// keep after it was stored, for GetStale (stale-if-error, #872). Such an
// entry is a miss for Get and stays inside the same entry and byte bounds.
type Cache[K comparable] struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int
	ttl        time.Duration
	keep       time.Duration // 0: nothing is kept past its TTL
	bytes      int
	order      *list.List // front = most recently used
	items      map[K]*list.Element
}

type cacheEntry[K comparable] struct {
	key     K
	data    []byte
	expires time.Time
	keep    time.Time // GetStale serves it until then; never before expires
}

func NewCache[K comparable](maxEntries, maxBytes int, ttl time.Duration) *Cache[K] {
	return &Cache[K]{
		maxEntries: maxEntries, maxBytes: maxBytes, ttl: ttl,
		order: list.New(), items: make(map[K]*list.Element),
	}
}

// NewCacheStale is NewCache whose entries GetStale still serves until keep
// after they were stored, even past the TTL (#872). keep at or below the
// TTL keeps nothing extra.
func NewCacheStale[K comparable](maxEntries, maxBytes int, ttl, keep time.Duration) *Cache[K] {
	c := NewCache[K](maxEntries, maxBytes, ttl)
	c.keep = keep
	return c
}

func (c *Cache[K]) Get(k K, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry[K])
	if !now.Before(e.expires) {
		if !now.Before(e.keep) {
			c.removeLocked(el)
		}
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.data, true
}

// GetStale returns k's data whether or not its TTL has passed, as long as
// it is within the keep window of NewCacheStale. It is the fallback when
// the upstream fails; a plain NewCache serves nothing past the TTL.
func (c *Cache[K]) GetStale(k K, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry[K])
	if !now.Before(e.keep) {
		c.removeLocked(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.data, true
}

// Put stores data under k for the cache's TTL.
func (c *Cache[K]) Put(k K, data []byte, now time.Time) { c.PutFor(k, data, now, c.ttl) }

// PutFor stores data under k for ttl, or for the cache's TTL if that is
// shorter: an entry that should be retried sooner, such as a profile whose
// building lookup failed (#730 M2).
func (c *Cache[K]) PutFor(k K, data []byte, now time.Time, ttl time.Duration) {
	if len(data) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.removeLocked(el)
	}
	expires := now.Add(min(ttl, c.ttl))
	keep := now.Add(c.keep)
	if keep.Before(expires) {
		keep = expires
	}
	c.items[k] = c.order.PushFront(&cacheEntry[K]{key: k, data: data, expires: expires, keep: keep})
	c.bytes += len(data)
	for c.order.Len() > 0 && (c.order.Len() > c.maxEntries || c.bytes > c.maxBytes) {
		c.removeLocked(c.order.Back())
	}
}

func (c *Cache[K]) removeLocked(el *list.Element) {
	e := c.order.Remove(el).(*cacheEntry[K])
	delete(c.items, e.key)
	c.bytes -= len(e.data)
}

// Len is the number of cached entries.
func (c *Cache[K]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Size is the total cached bytes.
func (c *Cache[K]) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
