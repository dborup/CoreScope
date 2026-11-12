package dhm

import (
	"container/list"
	"sync"
	"time"
)

// Cache is an LRU of upstream responses, bounded by entry count and total
// bytes, with a TTL checked on read. It is safe for concurrent use.
type Cache[K comparable] struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int
	ttl        time.Duration
	bytes      int
	order      *list.List // front = most recently used
	items      map[K]*list.Element
}

type cacheEntry[K comparable] struct {
	key     K
	data    []byte
	expires time.Time
}

func NewCache[K comparable](maxEntries, maxBytes int, ttl time.Duration) *Cache[K] {
	return &Cache[K]{
		maxEntries: maxEntries, maxBytes: maxBytes, ttl: ttl,
		order: list.New(), items: make(map[K]*list.Element),
	}
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
		c.removeLocked(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.data, true
}

func (c *Cache[K]) Put(k K, data []byte, now time.Time) {
	if len(data) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.removeLocked(el)
	}
	c.items[k] = c.order.PushFront(&cacheEntry[K]{key: k, data: data, expires: now.Add(c.ttl)})
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
