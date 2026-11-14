package dhm

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// #744 — Flight admits a new call only while a slot is free, before its
// worker exists; joiners take no slot; the slot returns when the worker
// has returned.

func TestFlightAdmissionRefusesNewCallWithoutWorker(t *testing.T) {
	var g Flight[int]
	adm := NewAdmission(1)
	release := make(chan struct{})
	var calls atomic.Int64
	fn := func(context.Context) (int, error) { calls.Add(1); <-release; return 1, nil }

	first := make(chan error, 1)
	go func() { _, _, err := g.DoAdmitted(context.Background(), "a", 0, adm, fn); first <- err }()
	waitFor(t, "call a running", func() bool { return calls.Load() == 1 })

	_, _, err := g.DoAdmitted(context.Background(), "b", 0, adm, func(context.Context) (int, error) {
		t.Error("refused call ran")
		return 0, nil
	})
	if !errors.Is(err, ErrAdmissionFull) {
		t.Fatalf("second key: err %v, want ErrAdmissionFull", err)
	}
	// Joining "a" adds no work and takes no slot.
	joined := make(chan error, 1)
	go func() {
		_, shared, err := g.DoAdmitted(context.Background(), "a", 0, adm, fn)
		if !shared {
			err = errors.New("did not join")
		}
		joined <- err
	}()
	waitFor(t, "joiner waiting", func() bool { return g.waiters("a") == 2 })
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-joined; err != nil {
		t.Fatalf("joiner: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times", calls.Load())
	}
	waitFor(t, "slot returned", func() bool { return adm.InUse() == 0 })
	if _, _, err := g.DoAdmitted(context.Background(), "b", 0, adm, func(context.Context) (int, error) { return 2, nil }); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// The slot bounds workers, so it is held until the worker has returned,
// not merely until every caller has left.
func TestFlightAdmissionHeldUntilWorkerReturns(t *testing.T) {
	var g Flight[int]
	adm := NewAdmission(1)
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		_, _, err := g.DoAdmitted(ctx, "a", 0, adm, func(context.Context) (int, error) {
			close(running)
			<-stop // ignores its context for a while
			return 0, nil
		})
		errc <- err
	}()
	<-running
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller: %v", err)
	}
	if adm.InUse() != 1 {
		t.Fatal("slot returned while the worker still runs")
	}
	close(stop)
	waitFor(t, "slot returned", func() bool { return adm.InUse() == 0 })
}

// #749 review F3: a call's slot is back before its waiters wake, so a
// caller at a full bound can start its next call (the terrain fallback)
// as soon as the first returns. The hook observes the worker at the last
// moment before close(done), which makes the order deterministic.
func TestFlightAdmissionReleasedBeforeWaitersWake(t *testing.T) {
	var g Flight[int]
	adm := NewAdmission(1)
	var inUseAtWake = -1
	g.beforeDone = func() { inUseAtWake = adm.InUse() }
	if _, _, err := g.DoAdmitted(context.Background(), "a", 0, adm, func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if inUseAtWake != 0 {
		t.Fatalf("waiters woken with %d slot(s) still taken, want 0", inUseAtWake)
	}
	if _, _, err := g.DoAdmitted(context.Background(), "b", 0, adm, func(context.Context) (int, error) { return 2, nil }); err != nil {
		t.Fatalf("next call right after the first returned: %v", err)
	}
}

// A tile caller waiting for a concurrency slot that leaves removes its
// queued worker: nothing reaches upstream and the admission slot returns.
func TestClientHillshadeCancelRemovesQueuedWorker(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	c.opts.Timeout = 5 * time.Second
	c.sem = make(chan struct{}, 1)
	c.sem <- struct{}{} // budget full: the fetch queues
	adm := NewAdmission(4)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.FetchHillshadeAdmitted(ctx, adm, HillshadeTerrain, someTile())
	if FailureReason(err) != "canceled" {
		t.Fatalf("err %v, want canceled", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("cancel took %v", el)
	}
	waitFor(t, "slot returned", func() bool { return adm.InUse() == 0 })
	<-c.sem
	time.Sleep(30 * time.Millisecond)
	if u.hits.Load() != 0 {
		t.Fatal("cancelled tile reached upstream")
	}
}

func TestClientHillshadeAdmissionFullIsNotFallbackEligible(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	c := newTestClient(t, u, &logSink{})
	adm := NewAdmission(1)
	adm.slots <- struct{}{}
	_, err := c.FetchHillshadeAdmitted(context.Background(), adm, HillshadeExaggerated, someTile())
	if !errors.Is(err, ErrAdmissionFull) || FallbackEligible(err) {
		t.Fatalf("err %v (fallback %v), want ErrAdmissionFull, not eligible", err, FallbackEligible(err))
	}
	if u.hits.Load() != 0 {
		t.Fatal("refused tile reached upstream")
	}
}

// #749 review F4: a refusal made no upstream request, so it is not logged
// as an upstream failure, and it is throttled on its own clock: a flood of
// refusals does not hide the next real upstream failure.
func TestClientLogRefusalHasItsOwnTextAndThrottle(t *testing.T) {
	logs := &logSink{}
	o := DefaultClientOptions()
	o.Logf = logs.logf
	o.LogEvery = time.Minute
	o.Now = func() time.Time { return time.Unix(1_000_000, 0) } // one instant
	c := NewClient(Config{Enabled: true, Token: testToken}, o)
	for i := 0; i < 100; i++ {
		c.LogRefusal("hillshade tile")
	}
	c.LogFailure("hillshade tile", FetchError{"timeout"})
	got := logs.String()
	want := "[dhm] hillshade tile refused: admission full, no upstream request\n" +
		"[dhm] upstream hillshade tile failed (timeout)\n"
	if got != want {
		t.Fatalf("log:\n%s\nwant:\n%s", got, want)
	}
}
