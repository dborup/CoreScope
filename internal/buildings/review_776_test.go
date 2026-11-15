package buildings

// #730 M2 — the follow-ups the #776 review carried over from M1b (N1-N4),
// each reproduced here before it is fixed.

import (
	"context"
	"testing"
	"time"
)

// TestFetcherWithoutNowUsesTheClock (#776 review N1): FetcherOptions.Now
// had no default, so a Fetcher built from options that leave it nil
// panicked on its first Fetch. It must read the real clock instead.
func TestFetcherWithoutNowUsesTheClock(t *testing.T) {
	src := &fakeSource{page: fixturePage(t)}
	o := DefaultFetcherOptions()
	o.Now = nil
	f := NewFetcher(src, o)
	seg := dokk1Segment(t)
	for i := 0; i < 2; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Fetch %d with a nil Now panicked: %v", i+1, r)
				}
			}()
			if _, err := f.Fetch(context.Background(), seg); err != nil {
				t.Fatalf("Fetch %d: %v", i+1, err)
			}
		}()
	}
	// The second Fetch is a cache hit on the real clock: the page's
	// expiry is a day ahead of time.Now, not of the zero time.
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("%d upstream calls, want 1 (the second Fetch is a cache hit)", n)
	}
	if got := f.now(); time.Since(got) > time.Minute || time.Until(got) > time.Minute {
		t.Fatalf("now() = %v, want the wall clock", got)
	}
}
