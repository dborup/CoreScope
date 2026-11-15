package buildings

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// fakeSource stands in for the Client. Every call is counted; block, if
// set, holds a call until it is closed or the call's context ends.
type fakeSource struct {
	calls   atomic.Int32
	running atomic.Int32
	peak    atomic.Int32
	block   chan struct{}
	fail    atomic.Bool
	page    Page
}

func (f *fakeSource) FetchSegment(ctx context.Context, s Segment) (Page, error) {
	f.calls.Add(1)
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Page{}, dhm.FetchError{Reason: "canceled"}
		}
	}
	if f.fail.Load() {
		return Page{}, dhm.FetchError{Reason: "status 500"}
	}
	return f.page, nil
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testFetcher(t *testing.T, src SegmentSource, edit func(*FetcherOptions)) (*Fetcher, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	o := DefaultFetcherOptions()
	o.Now = clock.Now
	if edit != nil {
		edit(&o)
	}
	return NewFetcher(src, o), clock
}

func fixturePage(t *testing.T) Page {
	t.Helper()
	p, err := Parse(readFixture(t, "dokk1_cell.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFetcherCachesASegment: a segment is fetched once; again, or from the
// path's reverse, it is a cache hit that decodes to the same footprints.
func TestFetcherCachesASegment(t *testing.T) {
	src := &fakeSource{page: fixturePage(t)}
	src.page.Skipped, src.page.Truncated = 2, true
	f, _ := testFetcher(t, src, nil)
	a, b := [2]float64{575000, 6223800}, [2]float64{576000, 6223800}
	seg := mustCorridor(t, a, b).Segments()[0]
	first, err := f.Fetch(context.Background(), seg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Segment{seg, mustCorridor(t, b, a).Segments()[0]} {
		got, err := f.Fetch(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) || !reflect.DeepEqual(got, src.page) {
			t.Fatal("cached page differs from the fetched one")
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("%d upstream calls, want 1", n)
	}
	other := mustCorridor(t, a, [2]float64{576000, 6223900}).Segments()[0]
	if _, err := f.Fetch(context.Background(), other); err != nil || src.calls.Load() != 2 {
		t.Fatalf("another segment: %v, %d calls", err, src.calls.Load())
	}
}

// TestFetcherCacheExpires: after CacheTTL the segment is fetched again.
func TestFetcherCacheExpires(t *testing.T) {
	src := &fakeSource{page: fixturePage(t)}
	f, clock := testFetcher(t, src, nil)
	seg := dokk1Segment(t)
	f.Fetch(context.Background(), seg)
	clock.Add(DefaultFetcherOptions().CacheTTL - time.Second)
	f.Fetch(context.Background(), seg)
	if src.calls.Load() != 1 {
		t.Fatalf("%d calls before the TTL", src.calls.Load())
	}
	clock.Add(2 * time.Second)
	f.Fetch(context.Background(), seg)
	if src.calls.Load() != 2 {
		t.Fatalf("%d calls after the TTL, want 2", src.calls.Load())
	}
}

// TestFetcherFailureIsNotCached: a failure is returned as is and the next
// Fetch asks again.
func TestFetcherFailureIsNotCached(t *testing.T) {
	src := &fakeSource{page: fixturePage(t)}
	src.fail.Store(true)
	f, _ := testFetcher(t, src, nil)
	seg := dokk1Segment(t)
	_, err := f.Fetch(context.Background(), seg)
	wantReason(t, err, "status 500")
	src.fail.Store(false)
	if p, err := f.Fetch(context.Background(), seg); err != nil || len(p.Footprints) != 25 {
		t.Fatalf("retry: %d footprints, %v", len(p.Footprints), err)
	}
	if src.calls.Load() != 2 {
		t.Fatalf("%d calls, want 2", src.calls.Load())
	}
}

func TestFetcherInvalidSegmentNeverCallsUpstream(t *testing.T) {
	src := &fakeSource{}
	f, _ := testFetcher(t, src, nil)
	if _, err := f.Fetch(context.Background(), Segment{}); !errors.Is(err, ErrBadSegment) {
		t.Fatalf("err %v", err)
	}
	if src.calls.Load() != 0 {
		t.Fatal("upstream called")
	}
}

// TestFetcherCoalesces: concurrent fetches of one segment share one
// upstream call, from either direction of the path.
func TestFetcherCoalesces(t *testing.T) {
	src := &fakeSource{page: fixturePage(t), block: make(chan struct{})}
	f, _ := testFetcher(t, src, nil)
	a, b := [2]float64{575000, 6223800}, [2]float64{576000, 6223800}
	segs := []Segment{mustCorridor(t, a, b).Segments()[0], mustCorridor(t, b, a).Segments()[0]}
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(seg Segment) {
			defer wg.Done()
			p, err := f.Fetch(context.Background(), seg)
			if err == nil && len(p.Footprints) != 25 {
				err = errors.New("short page")
			}
			errs <- err
		}(segs[i%2])
	}
	waitFor(t, func() bool { return f.flight.Pending() == 5 })
	close(src.block)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("%d upstream calls, want 1", n)
	}
}

// TestFetcherAdmission: at most MaxInFlight distinct segment calls exist
// at once; a further one is refused with dhm.ErrAdmissionFull at once and
// makes no upstream call. Joining a running call needs no slot.
func TestFetcherAdmission(t *testing.T) {
	src := &fakeSource{page: fixturePage(t), block: make(chan struct{})}
	f, _ := testFetcher(t, src, func(o *FetcherOptions) { o.MaxInFlight, o.Concurrency = 1, 1 })
	segs := mustCorridor(t, [2]float64{550000, 6223000}, [2]float64{575000, 6223000}).Segments()
	done := make(chan error, 2)
	go func() { _, err := f.Fetch(context.Background(), segs[0]); done <- err }()
	waitFor(t, func() bool { return src.calls.Load() == 1 })
	go func() { _, err := f.Fetch(context.Background(), segs[0]); done <- err }() // joins
	waitFor(t, func() bool { return f.flight.Pending() == 2 })
	start := time.Now()
	if _, err := f.Fetch(context.Background(), segs[1]); !errors.Is(err, dhm.ErrAdmissionFull) {
		t.Fatalf("second segment: err %v, want ErrAdmissionFull", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("refusal took %v", d)
	}
	close(src.block)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("%d upstream calls, want 1", n)
	}
}

// TestFetcherConcurrency: of the admitted calls, at most Concurrency run
// upstream at once; the rest wait their turn and all succeed.
func TestFetcherConcurrency(t *testing.T) {
	src := &fakeSource{page: fixturePage(t), block: make(chan struct{})}
	f, _ := testFetcher(t, src, func(o *FetcherOptions) { o.MaxInFlight, o.Concurrency = 8, 2 })
	segs := mustCorridor(t, [2]float64{500000, 6223000}, [2]float64{575000, 6223000}).Segments() // 8
	var wg sync.WaitGroup
	errs := make(chan error, len(segs))
	for _, s := range segs {
		wg.Add(1)
		go func(s Segment) {
			defer wg.Done()
			_, err := f.Fetch(context.Background(), s)
			errs <- err
		}(s)
	}
	waitFor(t, func() bool { return f.admit.InUse() == len(segs) && src.running.Load() == 2 })
	time.Sleep(20 * time.Millisecond) // a third would have started by now
	if n := src.running.Load(); n != 2 {
		t.Fatalf("%d running, want 2", n)
	}
	close(src.block)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if src.peak.Load() != 2 || src.calls.Load() != int32(len(segs)) {
		t.Fatalf("peak %d, calls %d", src.peak.Load(), src.calls.Load())
	}
}

// TestFetcherCallerGivesUp: a caller whose context ends returns at once;
// once every caller has gone the shared call is cancelled and its slot
// freed, so the next Fetch starts afresh.
func TestFetcherCallerGivesUp(t *testing.T) {
	src := &fakeSource{page: fixturePage(t), block: make(chan struct{})}
	f, _ := testFetcher(t, src, func(o *FetcherOptions) { o.MaxInFlight = 1 })
	seg := dokk1Segment(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, err := f.Fetch(ctx, seg); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	waitFor(t, func() bool { return f.admit.InUse() == 0 && src.running.Load() == 0 })
	close(src.block)
	if _, err := f.Fetch(context.Background(), seg); err != nil {
		t.Fatal(err)
	}
}

// TestFetcherTimeout: the shared call has its own deadline, whoever waits.
func TestFetcherTimeout(t *testing.T) {
	src := &fakeSource{page: fixturePage(t), block: make(chan struct{})}
	defer close(src.block)
	f, _ := testFetcher(t, src, func(o *FetcherOptions) { o.Timeout = 50 * time.Millisecond })
	done := make(chan error, 1)
	go func() { _, err := f.Fetch(context.Background(), dokk1Segment(t)); done <- err }()
	select {
	case err := <-done:
		wantReason(t, err, "canceled") // the fake's answer to its context ending
	case <-time.After(2 * time.Second):
		t.Fatal("the shared call outlived its timeout")
	}
	if DefaultFetcherOptions().Timeout != 5*time.Second {
		t.Fatalf("default timeout %v", DefaultFetcherOptions().Timeout)
	}
}

// TestFetcherWithClient: end to end against a fake OGC API, two fetches
// make one request.
func TestFetcherWithClient(t *testing.T) {
	srv, hits := fakeOGC(t, answer(200, "application/json", readFixture(t, "dokk1_cell.json")))
	f, _ := testFetcher(t, testClient(srv.URL), nil)
	for i := 0; i < 2; i++ {
		p, err := f.Fetch(context.Background(), dokk1Segment(t))
		if err != nil || len(p.Footprints) != 25 {
			t.Fatalf("fetch %d: %d footprints, %v", i, len(p.Footprints), err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("%d requests, want 1", hits.Load())
	}
}

// TestFetcherCacheIsBounded: the cache holds at most CacheEntries pages
// and CacheBytes; the oldest go first.
func TestFetcherCacheIsBounded(t *testing.T) {
	src := &fakeSource{page: fixturePage(t)}
	f, _ := testFetcher(t, src, func(o *FetcherOptions) { o.CacheEntries = 2 })
	segs := mustCorridor(t, [2]float64{550000, 6223000}, [2]float64{575000, 6223000}).Segments()
	for _, s := range segs {
		f.Fetch(context.Background(), s)
	}
	if f.cache.Len() != 2 {
		t.Fatalf("%d entries", f.cache.Len())
	}
	f.Fetch(context.Background(), segs[0]) // evicted: fetched again
	if src.calls.Load() != 4 {
		t.Fatalf("%d calls, want 4", src.calls.Load())
	}
	if o := DefaultFetcherOptions(); o.CacheBytes > 32<<20 || o.CacheEntries > 1024 {
		t.Fatalf("defaults %+v", o)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
