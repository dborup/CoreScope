package main

import (
	"math"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// #490: StoreObs caches its parsed Timestamp in one atomic 64-bit word
// instead of a sync.Once, a time.Time and a bool. These tests hold the new
// cache to the parser it replaced.

// legacyParsedTime490 is the body of the sync.Once ParsedTime this cache
// replaced, kept verbatim as the oracle of the differential tests below.
func legacyParsedTime490(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", ts); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// unixNanoTS490 formats the instant n nanoseconds after the epoch, plus d.
func unixNanoTS490(n int64, d time.Duration) string {
	return time.Unix(0, n).UTC().Add(d).Format(time.RFC3339Nano)
}

// The inputs of the differential tests, with the state the cache must be in
// after the first call (tsCacheInvalid, tsCacheReparse, or 0 for "holds the
// time").
var obsTimeCases490 = []struct {
	name, ts string
	state    uint64
}{
	{"rfc3339", "2026-10-24T12:34:56Z", 0},
	{"rfc3339nano", "2026-10-24T12:34:56.123456789Z", 0},
	{"rfc3339 millis", "2026-10-24T12:34:56.789Z", 0},
	{"rfc3339 one digit", "2026-10-24T12:34:56.1Z", 0},
	{"sql", "2026-10-24 12:34:56", 0},
	{"sql epoch", "1970-01-01 00:00:00", 0},
	{"epoch", "1970-01-01T00:00:00Z", 0},
	{"one ns after epoch", "1970-01-01T00:00:00.000000001Z", 0},
	{"one ns before epoch", "1969-12-31T23:59:59.999999999Z", 0},
	{"negative", "1900-01-01T00:00:00Z", 0},
	{"offset", "2026-10-24T13:34:56+01:00", tsCacheReparse},
	{"half-hour offset", "2026-10-24T07:04:56-05:30", tsCacheReparse},
	{"nano offset", "2026-10-24T13:34:56.000000007+01:00", tsCacheReparse},
	{"plus zero offset", "2026-10-24T12:34:56+00:00", tsCacheReparse},
	{"zero time", "0001-01-01T00:00:00Z", tsCacheReparse},
	{"year zero", "0000-01-01T00:00:00Z", tsCacheReparse},
	{"far future", "9999-12-31T23:59:59.999999999Z", tsCacheReparse},
	// The exact ends of what UnixNano holds.
	{"unix-nano max", unixNanoTS490(math.MaxInt64, 0), 0},
	{"unix-nano max + 1ns", unixNanoTS490(math.MaxInt64, time.Nanosecond), tsCacheReparse},
	{"unix-nano min - 1ns", unixNanoTS490(math.MinInt64, -time.Nanosecond), tsCacheReparse},
	// The three instants whose encoding is a sentinel.
	{"collides with not-parsed", unixNanoTS490(math.MinInt64, 0), tsCacheReparse},
	{"collides with invalid", unixNanoTS490(math.MinInt64+1, 0), tsCacheReparse},
	{"collides with reparse", unixNanoTS490(math.MinInt64+2, 0), tsCacheReparse},
	{"first after the sentinels", unixNanoTS490(math.MinInt64+3, 0), 0},
	{"empty", "", tsCacheInvalid},
	{"space", " ", tsCacheInvalid},
	{"garbage", "not a time", tsCacheInvalid},
	{"date only", "2026-10-24", tsCacheInvalid},
	{"month 13", "2026-13-01T00:00:00Z", tsCacheInvalid},
	{"leap second", "2026-06-30T23:59:60Z", tsCacheInvalid},
	{"no zone", "2026-10-24T12:34:56", tsCacheInvalid},
}

// sameParsedTime490 reports how got differs from want, or "" when it does
// not: the same success, instant, location, zone and rendering, and for a
// time in UTC or Local the identical value.
func sameParsedTime490(got time.Time, gotOK bool, want time.Time, wantOK bool) string {
	gotName, gotOff := got.Zone()
	wantName, wantOff := want.Zone()
	switch {
	case gotOK != wantOK:
		return "ok differs"
	case !got.Equal(want):
		return "instant differs"
	case got.Location().String() != want.Location().String():
		return "location differs"
	case gotName != wantName || gotOff != wantOff:
		return "zone differs"
	case got.Format(time.RFC3339Nano) != want.Format(time.RFC3339Nano):
		return "rendering differs"
	case (want.Location() == time.UTC || want.Location() == time.Local) && got != want:
		return "value differs"
	}
	return ""
}

// Each input gives, on the first and on cached calls, what the replaced
// parser gave.
func TestStoreObsParsedTimeMatchesLegacyParser_490(t *testing.T) {
	for _, c := range obsTimeCases490 {
		t.Run(c.name, func(t *testing.T) {
			want, wantOK := legacyParsedTime490(c.ts)
			if got, ok := parseObsTimestamp(c.ts); sameParsedTime490(got, ok, want, wantOK) != "" {
				t.Fatalf("parseObsTimestamp(%q) = %v %v, want %v %v", c.ts, got, ok, want, wantOK)
			}
			o := &StoreObs{Timestamp: c.ts}
			for call := 1; call <= 3; call++ {
				got, ok := o.ParsedTime()
				if d := sameParsedTime490(got, ok, want, wantOK); d != "" {
					t.Fatalf("%q call %d: %s: got %v (%v) ok=%v, want %v (%v) ok=%v",
						c.ts, call, d, got, got.Location(), ok, want, want.Location(), wantOK)
				}
			}
		})
	}
}

// After the first call the cache holds the time, or says why it does not;
// it never stays "not parsed yet" and never holds a time it cannot give
// back exactly.
func TestStoreObsParsedTimeCacheStates_490(t *testing.T) {
	for _, c := range obsTimeCases490 {
		t.Run(c.name, func(t *testing.T) {
			o := &StoreObs{Timestamp: c.ts}
			if got := o.tsCache.Load(); got != tsCacheNotParsed {
				t.Fatalf("a new observation's cache is %d, want %d", got, tsCacheNotParsed)
			}
			want, _ := legacyParsedTime490(c.ts)
			o.ParsedTime()
			got := o.tsCache.Load()
			switch {
			case c.state != 0 && got != c.state:
				t.Fatalf("cache = %d after the first call, want sentinel %d", got, c.state)
			case c.state == 0 && got <= tsCacheReparse:
				t.Fatalf("cache = %d after the first call, want the time itself", got)
			case c.state == 0 && got != tsCacheEncode(want.UnixNano()):
				t.Fatalf("cache = %d, want %d (%v)", got, tsCacheEncode(want.UnixNano()), want)
			}
			o.ParsedTime()
			if again := o.tsCache.Load(); again != got {
				t.Fatalf("cache changed from %d to %d on a cached call", got, again)
			}
		})
	}
}

// A cached call reads the cache, not Timestamp (which never changes once the
// observation is built): replacing the string after the first call must not
// change a cached answer.
func TestStoreObsParsedTimeCachedCallDoesNotReparse_490(t *testing.T) {
	for _, ts := range []string{"2026-10-24T12:34:56.123456789Z", "1970-01-01T00:00:00Z", "1900-01-01 00:00:00", "garbage"} {
		want, wantOK := legacyParsedTime490(ts)
		o := &StoreObs{Timestamp: ts}
		o.ParsedTime()
		o.Timestamp = "2001-02-03T04:05:06Z"
		if got, ok := o.ParsedTime(); sameParsedTime490(got, ok, want, wantOK) != "" {
			t.Fatalf("%q: a cached call returned %v %v, want %v %v", ts, got, ok, want, wantOK)
		}
	}
}

// Readers under the store's read lock race to fill the cache: every first
// and repeated call, from any goroutine, gives the replaced parser's answer.
// Run with -race.
func TestStoreObsParsedTimeConcurrentFirstReads_490(t *testing.T) {
	const readers, rounds = 8, 20
	for round := 0; round < rounds; round++ {
		obs := make([]*StoreObs, len(obsTimeCases490))
		for i, c := range obsTimeCases490 {
			obs[i] = &StoreObs{Timestamp: c.ts}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func(r int) {
				defer wg.Done()
				<-start
				for k := range obs {
					i := (k + r) % len(obs) // readers start on different observations
					want, wantOK := legacyParsedTime490(obs[i].Timestamp)
					for call := 0; call < 2; call++ {
						got, ok := obs[i].ParsedTime()
						if d := sameParsedTime490(got, ok, want, wantOK); d != "" {
							t.Errorf("%q: %s: got %v %v, want %v %v", obs[i].Timestamp, d, got, ok, want, wantOK)
							return
						}
					}
				}
			}(r)
		}
		close(start)
		wg.Wait()
	}
}

// The cache is one 64-bit word in place of 40 bytes: StoreObs is 168 bytes
// on 64-bit, down from 200 (wireRoute's place: heard_by_test.go).
func TestStoreObsLayoutTimeCacheIsOneWord_490(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout budget is defined for 64-bit platforms")
	}
	var o StoreObs
	if got := unsafe.Sizeof(o.tsCache); got != 8 {
		t.Fatalf("unsafe.Sizeof(StoreObs.tsCache) = %d, want 8", got)
	}
	if got := unsafe.Sizeof(o); got != 168 {
		t.Fatalf("unsafe.Sizeof(StoreObs{}) = %d, want 168", got)
	}
}

// trackedBytes charges each observation's struct at the size the heap
// actually gives it, so the accounting follows the layout.
func TestStoreObsBaseBytesIsTheHeapSizeClass_490(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout budget is defined for 64-bit platforms")
	}
	class := heapSizeClassOf490(unsafe.Sizeof(StoreObs{}))
	if storeObsBaseBytes != int(class) {
		t.Fatalf("storeObsBaseBytes = %d, want %d: the heap size class of a %d-byte StoreObs",
			storeObsBaseBytes, class, unsafe.Sizeof(StoreObs{}))
	}
	// And the heap does allocate observations in that class.
	const n = 4096
	keep := make([]*StoreObs, n)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range keep {
		keep[i] = &StoreObs{}
	}
	runtime.ReadMemStats(&after)
	for i, c := range after.BySize {
		if c.Size == class {
			if got := c.Mallocs - before.BySize[i].Mallocs; got < n {
				t.Fatalf("%d allocations in the %d-byte class, want at least %d", got, class, n)
			}
		}
	}
	runtime.KeepAlive(keep)
}

// The differential property on any input: every table case seeds the
// corpus; `go test -fuzz FuzzStoreObsParsedTime_490` searches further.
func FuzzStoreObsParsedTime_490(f *testing.F) {
	for _, c := range obsTimeCases490 {
		f.Add(c.ts)
	}
	f.Fuzz(func(t *testing.T, ts string) {
		want, wantOK := legacyParsedTime490(ts)
		o := &StoreObs{Timestamp: ts}
		for call := 1; call <= 2; call++ {
			got, ok := o.ParsedTime()
			if d := sameParsedTime490(got, ok, want, wantOK); d != "" {
				t.Fatalf("%q call %d: %s: got %v %v, want %v %v", ts, call, d, got, ok, want, wantOK)
			}
		}
	})
}
