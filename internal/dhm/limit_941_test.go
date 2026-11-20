package dhm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// #941 M1 — the reach's sub-budget of the shared DHM client.

type blockingSource struct {
	inFlight, peak, calls atomic.Int64
	release               chan struct{}
}

func (b *blockingSource) FetchRaster(ctx context.Context, r RasterRequest) (Grid, error) {
	b.calls.Add(1)
	n := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return Grid{}, ctx.Err()
	}
	return Grid{}, nil
}

// TestLimitSource941_BoundsConcurrency: however many fetches are asked
// for at once, at most n reach the source together, so the reach never
// holds more than its share of the client's four upstream slots.
func TestLimitSource941_BoundsConcurrency(t *testing.T) {
	src := &blockingSource{release: make(chan struct{})}
	lim := LimitSource(src, 2)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := lim.FetchRaster(context.Background(), RasterRequest{}); err != nil {
				t.Error(err)
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for src.inFlight.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if p := src.peak.Load(); p != 2 {
		t.Fatalf("peak concurrency %d, want 2", p)
	}
	close(src.release)
	wg.Wait()
	if src.calls.Load() != 12 || src.peak.Load() != 2 {
		t.Fatalf("calls %d, peak %d", src.calls.Load(), src.peak.Load())
	}
}

// TestLimitSource941_WaitingStopsWithTheContext: a fetch waiting for a
// slot gives up when its context ends, as a FetchError category.
func TestLimitSource941_WaitingStopsWithTheContext(t *testing.T) {
	src := &blockingSource{release: make(chan struct{})}
	lim := LimitSource(src, 1)
	go lim.FetchRaster(context.Background(), RasterRequest{})
	for src.inFlight.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := lim.FetchRaster(ctx, RasterRequest{})
	var fe FetchError
	if !errors.As(err, &fe) || fe.Reason != "timeout" {
		t.Fatalf("err %v, want FetchError timeout", err)
	}
	if src.calls.Load() != 1 {
		t.Fatalf("the waiting fetch reached the source")
	}
	close(src.release)
}
