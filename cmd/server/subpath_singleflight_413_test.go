package main

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An uncached Route Patterns request scans the whole store and allocates
// about as much as the removed index held (#413). N concurrent misses for
// one key must share one count, or the heap the index saved is spent again
// as soon as a few overlap (review F1 on PR #529).
//
// The first count waits until all n callers have missed, so without a
// shared flight every one of them counts; with it the other n-1 join the
// first, and a caller that reaches the flight after it stored its result
// finds that result when the cache is checked again inside the flight.
func TestSubpathColdMissesShareOneCount_413(t *testing.T) {
	const n = 8
	window := TimeWindow{Since: "2000-01-01T00:00:00Z"}
	cases := []struct {
		name string
		call func(s *PacketStore) interface{}
	}{
		{"list", func(s *PacketStore) interface{} { return s.GetAnalyticsSubpaths("", 2, 8, 100) }},
		{"window", func(s *PacketStore) interface{} {
			return s.GetAnalyticsSubpathsWithWindow("", 2, 5, 50, window)
		}},
		{"bulk", func(s *PacketStore) interface{} { return s.GetAnalyticsSubpathsBulk("", bench413Groups) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodes, packets := subpath413Data(1500, 150, 7)
			s := newSubpath413Store(nodes, packets)

			var missed sync.WaitGroup
			missed.Add(n)
			var counts atomic.Int32
			s.subpathHook = func(stage string) {
				switch stage {
				case "miss":
					missed.Done()
				case "count":
					if counts.Add(1) != 1 {
						return
					}
					done := make(chan struct{})
					go func() { missed.Wait(); close(done) }()
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("not every caller missed the cache")
					}
				}
			}

			results := make([]interface{}, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i] = c.call(s)
				}(i)
			}
			wg.Wait()

			if got := counts.Load(); got != 1 {
				t.Fatalf("%d concurrent misses ran %d counts, want 1", n, got)
			}
			for i := 1; i < n; i++ {
				if !reflect.DeepEqual(results[i], results[0]) {
					t.Fatalf("caller %d got a different result", i)
				}
			}
			if again := c.call(s); !reflect.DeepEqual(again, results[0]) || counts.Load() != 1 {
				t.Fatalf("the shared result was not cached (counts=%d)", counts.Load())
			}
		})
	}
}
