package dhm

import (
	"testing"
	"time"
)

// #872 — stale-if-error: a cache made with NewCacheStale keeps an entry past
// its TTL, up to keep after it was stored, for GetStale only. Get still
// misses after the TTL, and the entry and byte bounds cover stale entries.

var t872 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func TestCacheStaleServedPastTTLWithinKeep(t *testing.T) {
	c := NewCacheStale[string](4, 1<<20, time.Hour, 24*time.Hour)
	c.Put("a", []byte("A"), t872)

	at := t872.Add(2 * time.Hour)
	if _, ok := c.Get("a", at); ok {
		t.Fatal("Get after the TTL hit, want a miss")
	}
	if d, ok := c.GetStale("a", at); !ok || string(d) != "A" {
		t.Fatalf("GetStale after the TTL = %q %v, want A", d, ok)
	}
	// Get past the TTL must not have dropped it.
	if c.Len() != 1 {
		t.Fatalf("Len %d, want 1", c.Len())
	}
}

func TestCacheStaleGoneAfterKeep(t *testing.T) {
	c := NewCacheStale[string](4, 1<<20, time.Hour, 24*time.Hour)
	c.Put("a", []byte("A"), t872)
	at := t872.Add(24 * time.Hour)
	if _, ok := c.GetStale("a", at); ok {
		t.Fatal("GetStale at the keep limit hit, want a miss")
	}
	if _, ok := c.Get("a", at); ok {
		t.Fatal("Get at the keep limit hit")
	}
	if c.Len() != 0 || c.Size() != 0 {
		t.Fatalf("entry past keep not dropped: Len %d Size %d", c.Len(), c.Size())
	}
}

// A refreshed entry starts a new keep window.
func TestCacheStaleKeepRestartsOnPut(t *testing.T) {
	c := NewCacheStale[string](4, 1<<20, time.Hour, 24*time.Hour)
	c.Put("a", []byte("A"), t872)
	c.Put("a", []byte("B"), t872.Add(20*time.Hour))
	if d, ok := c.GetStale("a", t872.Add(30*time.Hour)); !ok || string(d) != "B" {
		t.Fatalf("GetStale = %q %v, want B", d, ok)
	}
}

// Stale entries count against the same entry and byte bounds (LRU).
func TestCacheStaleWithinBounds(t *testing.T) {
	c := NewCacheStale[string](2, 10, time.Hour, 24*time.Hour)
	c.Put("a", []byte("aaaa"), t872)
	c.Put("b", []byte("bbbb"), t872)
	at := t872.Add(2 * time.Hour) // both stale
	c.Put("c", []byte("cc"), at)  // entry bound: evicts the LRU stale "a"
	if _, ok := c.GetStale("a", at); ok {
		t.Fatal("stale entry outlived the entry bound")
	}
	if c.Len() != 2 {
		t.Fatalf("Len %d, want 2", c.Len())
	}
	c.Put("d", []byte("dddddddd"), at) // byte bound: 8+2 = 10 keeps c only if b goes
	if _, ok := c.GetStale("b", at); ok {
		t.Fatal("stale entry outlived the byte bound")
	}
	if c.Size() > 10 {
		t.Fatalf("Size %d over the byte bound", c.Size())
	}
}

// A plain NewCache keeps nothing past the TTL (unchanged behaviour).
func TestCachePlainHasNoStale(t *testing.T) {
	c := NewCache[string](4, 1<<20, time.Hour)
	c.Put("a", []byte("A"), t872)
	if _, ok := c.GetStale("a", t872.Add(time.Hour)); ok {
		t.Fatal("plain cache served a stale entry")
	}
	if _, ok := c.Get("a", t872.Add(time.Hour)); ok || c.Len() != 0 {
		t.Fatal("plain cache kept an expired entry")
	}
}
