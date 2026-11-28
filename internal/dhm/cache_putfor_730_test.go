package dhm

import (
	"testing"
	"time"
)

// TestCachePutFor (#730 M2): an entry stored with PutFor expires after its
// own shorter TTL, never after a longer one than the cache's; Put keeps
// the cache's TTL.
func TestCachePutFor(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	c := NewCache[string](8, 1<<10, time.Hour)
	c.Put("put", []byte("a"), now)
	c.PutFor("short", []byte("b"), now, 10*time.Minute)
	c.PutFor("long", []byte("c"), now, 48*time.Hour)
	c.PutFor("zero", []byte("d"), now, 0)
	at := func(d time.Duration, key string) bool {
		_, ok := c.Get(key, now.Add(d))
		return ok
	}
	if !at(10*time.Minute-time.Nanosecond, "short") || at(10*time.Minute, "short") {
		t.Fatal("PutFor's 10 minutes not kept")
	}
	if at(0, "zero") {
		t.Fatal("PutFor with no TTL served")
	}
	for _, key := range []string{"put", "long"} {
		if !at(time.Hour-time.Nanosecond, key) || at(time.Hour, key) {
			t.Fatalf("%s: not held to the cache's hour", key)
		}
	}
	if c.Len() != 0 || c.Size() != 0 {
		t.Fatalf("%d entries, %d bytes left after expiry", c.Len(), c.Size())
	}
}
