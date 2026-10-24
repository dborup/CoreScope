package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

// Issue #489: obsStringPool is the store's bounded, concurrency-safe table of
// canonical observer metadata strings. These are its unit properties; the
// load/ingest/merge paths are covered in obs_strings_paths_489_test.go.

// sameCopy reports whether a and b are literally the same bytes in memory,
// not merely equal strings. This is what "shared" means.
func sameCopy(a, b string) bool { return stringDataPtr(a) == stringDataPtr(b) }

func TestObsStringPoolSharesOneCopy_489(t *testing.T) {
	p := newObsStringPool()
	// Two equal values with different backing arrays, as two row scans give.
	a := strings.Repeat("ab", 32)
	b := strings.Repeat("ab", 32)
	if sameCopy(a, b) {
		t.Fatal("setup: the two inputs already share one copy")
	}
	ca, sharedA := p.intern(a)
	cb, sharedB := p.intern(b)
	if !sharedA || !sharedB {
		t.Fatalf("intern reported shared=%v/%v, want both shared", sharedA, sharedB)
	}
	if ca != a || cb != a {
		t.Fatalf("intern returned %q/%q, want %q", ca, cb, a)
	}
	if !sameCopy(ca, cb) {
		t.Fatal("the two observations hold two copies of the same value; the pool shared nothing")
	}
	if got := p.entries(); got != 1 {
		t.Fatalf("pool holds %d entries for one distinct value, want 1", got)
	}
}

func TestObsStringPoolClonesNewValues_489(t *testing.T) {
	p := newObsStringPool()
	// A row scan can hand over a short string that is a slice of a much
	// larger buffer. Retaining it as-is would pin the whole buffer.
	const big = 1 << 20
	buf := strings.Repeat("z", big-2) + "OK"
	sub := buf[big-2:]
	if len(sub) != 2 {
		t.Fatalf("setup: substring is %d bytes", len(sub))
	}
	c, shared := p.intern(sub)
	if !shared || c != "OK" {
		t.Fatalf("intern(sub) = %q, %v; want \"OK\", true", c, shared)
	}
	if sameCopy(c, sub) {
		t.Fatal("the pool retained the substring itself, pinning its 1 MiB backing buffer")
	}
	// The canonical copy must sit outside the big buffer entirely.
	lo := stringDataPtr(buf)
	if got := stringDataPtr(c); got >= lo && got < lo+uintptr(big) {
		t.Fatalf("canonical copy at %#x lies inside the 1 MiB buffer at %#x", got, lo)
	}
	// With the copy outside the buffer, dropping the buffer frees all 1 MiB
	// of it and the canonical copy still reads correctly.
	buf, sub = "", ""
	runtime.GC()
	runtime.GC()
	if c != "OK" {
		t.Fatalf("canonical copy changed to %q after the buffer was dropped", c)
	}
	if again, _ := p.intern("OK"); !sameCopy(again, c) {
		t.Fatal("the pool lost its copy when the buffer was dropped")
	}
}

func TestObsStringPoolSurvivesGC_489(t *testing.T) {
	p := newObsStringPool()
	first, _ := p.intern(strings.Repeat("c4", 32))
	ptr := stringDataPtr(first)
	// A weakly held table (unique.Make) hands back a second copy here.
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	again, shared := p.intern(strings.Repeat("c4", 32))
	if !shared {
		t.Fatal("a value interned before a GC came back unshared")
	}
	if stringDataPtr(again) != ptr {
		t.Fatal("a value interned again after a GC came back as a second copy; the sharing does not survive collection")
	}
	if got := p.entries(); got != 1 {
		t.Fatalf("pool holds %d entries after the GC, want 1", got)
	}
	runtime.KeepAlive(first)
}

func TestObsStringPoolSaturationFallsBackToPlainStrings_489(t *testing.T) {
	p := newObsStringPool()
	for i := 0; i < obsStringPoolMax; i++ {
		if _, shared := p.intern(fmt.Sprintf("value-%06d", i)); !shared {
			t.Fatalf("value %d was not shared while the pool had room", i)
		}
	}
	if got := p.entries(); got != obsStringPoolMax {
		t.Fatalf("pool holds %d entries, want the cap %d", got, obsStringPoolMax)
	}
	bytesAtCap := p.retainedBytes()

	// Past the cap: the value comes back exactly as it came in, unshared.
	fresh := strings.Repeat("ff", 32)
	c, shared := p.intern(fresh)
	if shared {
		t.Fatal("a full pool reported a value as shared")
	}
	if c != fresh || !sameCopy(c, fresh) {
		t.Fatalf("a full pool returned %q (same copy: %v), want the input unchanged", c, sameCopy(c, fresh))
	}
	if got := p.entries(); got != obsStringPoolMax {
		t.Fatalf("pool grew past the cap to %d entries", got)
	}
	if got := p.retainedBytes(); got != bytesAtCap {
		t.Fatalf("a full pool retained %d more bytes", got-bytesAtCap)
	}
	// Values already held keep being shared after saturation.
	held, shared := p.intern("value-000007")
	if !shared || held != "value-000007" {
		t.Fatalf("intern of a held value after saturation = %q, %v", held, shared)
	}
	if p.full.Load() != 1 {
		t.Fatalf("full counter = %d, want 1", p.full.Load())
	}
}

func TestObsStringPoolConcurrentCallsShareOneCopy_489(t *testing.T) {
	p := newObsStringPool()
	const goroutines, values = 16, 64
	got := make([][]string, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			out := make([]string, values)
			<-start
			for v := 0; v < values; v++ {
				// A fresh backing array per goroutine per value.
				s := fmt.Sprintf("%064x", v)
				c, shared := p.intern(s)
				if !shared {
					t.Errorf("goroutine %d: value %d unshared", g, v)
				}
				out[v] = c
			}
			got[g] = out
		}(g)
	}
	close(start)
	wg.Wait()
	for v := 0; v < values; v++ {
		want := got[0][v]
		for g := 1; g < goroutines; g++ {
			if got[g][v] != want {
				t.Fatalf("goroutine %d value %d = %q, want %q", g, v, got[g][v], want)
			}
			if !sameCopy(got[g][v], want) {
				t.Fatalf("value %d: goroutine %d holds a second copy", v, g)
			}
		}
	}
	if n := p.entries(); n != values {
		t.Fatalf("pool holds %d entries for %d distinct values", n, values)
	}
}

func TestObsStringPoolHitDoesNotAllocate_489(t *testing.T) {
	p := newObsStringPool()
	key := strings.Repeat("7e", 32)
	p.intern(key)
	if n := testing.AllocsPerRun(200, func() { p.intern(key) }); n != 0 {
		t.Fatalf("a pool hit allocates %.1f times per call, want 0", n)
	}
	m := obsStrings{}
	if n := testing.AllocsPerRun(200, func() {
		m = p.internObs(key, "Name", "CPH", "RX")
	}); n != 0 {
		t.Fatalf("internObs allocates %.1f times per call once warm, want 0", n)
	}
	if m.shared != obsSharedObserverID|obsSharedObserverName|obsSharedObserverIATA|obsSharedDirection {
		t.Fatalf("shared mask = %#b, want all four fields shared", m.shared)
	}
}

func TestObsStringPoolEmptyAndNil_489(t *testing.T) {
	var nilPool *obsStringPool
	if c, shared := nilPool.intern("abc"); c != "abc" || shared {
		t.Fatalf("nil pool intern = %q, %v; want the input unshared", c, shared)
	}
	if nilPool.retainedBytes() != 0 || nilPool.entries() != 0 {
		t.Fatal("nil pool reports retention")
	}
	m := nilPool.internObs("a", "b", "c", "d")
	if m.shared != 0 || m.observerID != "a" || m.direction != "d" {
		t.Fatalf("nil pool internObs = %+v", m)
	}

	p := newObsStringPool()
	if c, shared := p.intern(""); c != "" || shared {
		t.Fatalf("intern(\"\") = %q, %v; want the empty string unshared", c, shared)
	}
	if p.entries() != 0 {
		t.Fatal("the empty string took an entry")
	}
	// An observation with no IATA and no direction is charged nothing for
	// them and marks neither as shared.
	m = p.internObs("id", "name", "", "")
	if m.shared != obsSharedObserverID|obsSharedObserverName {
		t.Fatalf("shared mask = %#b, want only ID and name", m.shared)
	}
	obs := &StoreObs{}
	m.apply(obs)
	if got := ownedObsStringBytes(obs); got != 0 {
		t.Fatalf("ownedObsStringBytes = %d for a fully shared observation, want 0", got)
	}
}

// Accounting: the pool charges every entry its content plus one entry
// overhead, and that charge is an upper bound on what the table really
// retains at every fill (obsStringEntryBytes's doc comment).
func TestObsStringPoolRetainedBytes_489(t *testing.T) {
	p := newObsStringPool()
	var content int64
	for i := 0; i < 2000; i++ {
		v := fmt.Sprintf("%064x", i)
		p.intern(v)
		content += int64(len(v))
	}
	want := content + 2000*obsStringEntryBytes
	if got := p.retainedBytes(); got != want {
		t.Fatalf("retainedBytes = %d, want content %d + %d entries x %d = %d", got, content, 2000, obsStringEntryBytes, want)
	}
	// Re-interning held values must not charge again.
	for i := 0; i < 2000; i++ {
		p.intern(fmt.Sprintf("%064x", i))
	}
	if got := p.retainedBytes(); got != want {
		t.Fatalf("retainedBytes grew to %d on repeat interning, want %d", got, want)
	}

	// Upper bound against the real thing: the heap a second pool of the same
	// shape really costs, measured the way obsStringEntryBytes was.
	keys := make([]string, 2000)
	for i := range keys {
		keys[i] = fmt.Sprintf("%064x", i)
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m0)
	measured := newObsStringPool()
	for _, k := range keys {
		measured.intern(k)
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m1)
	actual := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	runtime.KeepAlive(measured)
	runtime.KeepAlive(keys)
	if actual <= 0 {
		t.Skipf("heap measurement unusable (%d B)", actual)
	}
	if want < actual {
		t.Fatalf("the pool charges %d B for what really costs %d B: the charge must be an upper bound", want, actual)
	}
	if want > 3*actual {
		t.Fatalf("the pool charges %d B for %d B of real retention; obsStringEntryBytes = %d is more than 3x off", want, actual, obsStringEntryBytes)
	}
}

func TestObsStringPoolChurnStaysBounded_489(t *testing.T) {
	p := newObsStringPool()
	// Twice the cap in distinct values: every one of them must come back
	// correct, and the table must stop at the cap.
	for i := 0; i < 2*obsStringPoolMax; i++ {
		v := fmt.Sprintf("rename-%07d", i)
		c, _ := p.intern(v)
		if c != v {
			t.Fatalf("value %d came back as %q", i, c)
		}
	}
	if got := p.entries(); got != obsStringPoolMax {
		t.Fatalf("pool holds %d entries after 2x the cap of distinct values, want %d", got, obsStringPoolMax)
	}
	maxBytes := int64(obsStringPoolMax) * (obsStringEntryBytes + 64)
	if got := p.retainedBytes(); got > maxBytes {
		t.Fatalf("pool retains %d B, above the %d B the cap allows for 64-byte values", got, maxBytes)
	}
}

// StoreObs must not grow: sharedStrs uses the padding wireRoute already sits
// in (see TestStoreObsLayoutFitsWireRouteInPadding_76).
func TestStoreObsLayoutFitsSharedStrsInPadding_489(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout pinned for 64-bit builds")
	}
	if got := unsafe.Sizeof(StoreObs{}); got != 200 {
		t.Fatalf("unsafe.Sizeof(StoreObs{}) = %d, want 200: sharedStrs must use existing padding", got)
	}
	// One shard per cache line, so a write on one does not invalidate the
	// next one's lock (see obsStringShard).
	if got := unsafe.Sizeof(obsStringShard{}); got != 64 {
		t.Fatalf("unsafe.Sizeof(obsStringShard{}) = %d, want 64: adjust the padding", got)
	}
}
