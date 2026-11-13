package dhm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// #681 M3 / #683 review N3 — Flight coalesces like singleflight, but the
// shared call stops once every caller has given up.

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFlightCoalescesConcurrentCallers(t *testing.T) {
	var g Flight[int]
	var calls atomic.Int64
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 42, nil
	}
	const n = 20
	var wg sync.WaitGroup
	var shared atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, sh, err := g.Do(context.Background(), "k", 0, fn)
			if err != nil || v != 42 {
				t.Errorf("Do = %d, %v", v, err)
			}
			if sh {
				shared.Add(1)
			}
		}()
	}
	waitFor(t, "20 waiters", func() bool { return g.waiters("k") == n })
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times, want 1", calls.Load())
	}
	if shared.Load() != n-1 {
		t.Fatalf("%d callers joined, want %d", shared.Load(), n-1)
	}
	if g.waiters("k") != 0 {
		t.Fatal("finished call still registered")
	}
}

func TestFlightCancelsOnlyWhenEveryCallerLeft(t *testing.T) {
	var g Flight[int]
	started := make(chan struct{})
	stopped := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return 0, ctx.Err()
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	errs := make(chan error, 2)
	go func() { _, _, err := g.Do(ctx1, "k", 0, fn); errs <- err }()
	<-started
	go func() { _, _, err := g.Do(ctx2, "k", 0, fn); errs <- err }()
	waitFor(t, "2 waiters", func() bool { return g.waiters("k") == 2 })

	cancel1()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: %v", err)
	}
	select {
	case <-stopped:
		t.Fatal("shared call cancelled while a caller still waits")
	case <-time.After(50 * time.Millisecond):
	}
	cancel2()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("second caller: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("shared call kept running after every caller left")
	}
	// The abandoned call is gone: the next caller starts afresh.
	if g.waiters("k") != 0 {
		t.Fatal("abandoned call still registered")
	}
}

func TestFlightTimeoutBoundsTheSharedCall(t *testing.T) {
	var g Flight[int]
	start := time.Now()
	_, _, err := g.Do(context.Background(), "k", 50*time.Millisecond, func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want deadline", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("timeout not applied: %v", el)
	}
}

func TestFlightDistinctKeysRunSeparately(t *testing.T) {
	var g Flight[string]
	for _, k := range []string{"a", "b"} {
		v, shared, err := g.Do(context.Background(), k, 0, func(context.Context) (string, error) { return k, nil })
		if v != k || shared || err != nil {
			t.Fatalf("key %s: %q %v %v", k, v, shared, err)
		}
	}
}
