package dhm

import (
	"testing"
	"time"
)

// TestCachePutHoldsTheTTL: an entry is served until the cache's TTL from
// its Put, and not from then on; a Put again starts it anew. (#730 M2's
// PutFor, a shorter TTL per entry, went with #730 M3: a profile whose
// building lookup failed is now cached like any other and retried on a
// hit.)
func TestCachePutHoldsTheTTL(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	c := NewCache[string](8, 1<<10, time.Hour)
	c.Put("a", []byte("a"), now)
	c.Put("b", []byte("b"), now)
	c.Put("b", []byte("b2"), now.Add(30*time.Minute))
	at := func(d time.Duration, key string) bool {
		_, ok := c.Get(key, now.Add(d))
		return ok
	}
	if !at(time.Hour-time.Nanosecond, "a") || at(time.Hour, "a") {
		t.Fatal("a: not held to the cache's hour")
	}
	if !at(90*time.Minute-time.Nanosecond, "b") || at(90*time.Minute, "b") {
		t.Fatal("b: a second Put did not start its hour anew")
	}
	if c.Len() != 0 || c.Size() != 0 {
		t.Fatalf("%d entries, %d bytes left after expiry", c.Len(), c.Size())
	}
}
