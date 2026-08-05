package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

// A. Zero-value accessor: a *Server built without ever calling the
// setter (e.g. &Server{}, or the fixture healthz's own tests already
// use) must return the documented "initializing"/""/"" default, and must
// never panic on a zero-value atomic.Value.
func TestPingScoreHistoryStatusView_ZeroValue_ReturnsInitializingNoPanic(t *testing.T) {
	s := &Server{}
	got := s.pingScoreHistoryStatusView()
	want := pingScoreHistoryStatusSnapshot{State: "initializing"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// B. Setter round-trip: the accessor returns exactly the tuple just
// stored.
func TestPingScoreHistoryStatus_SetterRoundTrip(t *testing.T) {
	s := &Server{}
	s.setPingScoreHistoryStatus("ok", "", "2026-08-05T12:00:00Z")

	got := s.pingScoreHistoryStatusView()
	want := pingScoreHistoryStatusSnapshot{State: "ok", Code: "", LastCycleAt: "2026-08-05T12:00:00Z"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// C. Overwrite: a later Store fully replaces the earlier one -- the
// accessor always returns the single latest complete tuple, never a mix.
func TestPingScoreHistoryStatus_OverwriteReplacesEntireTuple(t *testing.T) {
	s := &Server{}
	s.setPingScoreHistoryStatus("ok", "", "2026-08-05T12:00:00Z")
	s.setPingScoreHistoryStatus("degraded", "cycle_failed", "2026-08-05T12:00:00Z")

	got := s.pingScoreHistoryStatusView()
	want := pingScoreHistoryStatusSnapshot{State: "degraded", Code: "cycle_failed", LastCycleAt: "2026-08-05T12:00:00Z"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// D. Server isolation: status set on one *Server must never be visible
// on another -- this is per-instance state, not a package global.
func TestPingScoreHistoryStatus_ServerIsolation(t *testing.T) {
	s1 := &Server{}
	s2 := &Server{}

	s1.setPingScoreHistoryStatus("degraded", "cycle_failed", "x")

	if got := s2.pingScoreHistoryStatusView(); got != (pingScoreHistoryStatusSnapshot{State: "initializing"}) {
		t.Errorf("s2.pingScoreHistoryStatusView() = %+v, want the untouched default (s1's Store leaked across instances)", got)
	}
	want1 := pingScoreHistoryStatusSnapshot{State: "degraded", Code: "cycle_failed", LastCycleAt: "x"}
	if got := s1.pingScoreHistoryStatusView(); got != want1 {
		t.Errorf("s1.pingScoreHistoryStatusView() = %+v, want %+v", got, want1)
	}
}

// E. Concurrent load/store: a fixed number of writer and reader
// goroutines, each doing a fixed number of operations, released together
// off one start gate (so none of them can begin -- or finish -- before
// every goroutine has actually been created). Deterministic in shape (no
// time.Sleep, no time-based stop): the test asserts the EXACT operation
// counts completed, not just "some work happened". Every observed tuple
// must still be exactly one of the complete tuples a writer actually
// stored, never a mix of State/Code/LastCycleAt from different calls --
// guaranteed by construction (a single atomic.Value.Store per write, of
// a plain string-only struct value), pinned down here as an explicit
// regression test.
func TestPingScoreHistoryStatus_ConcurrentLoadStore_NoTornReads(t *testing.T) {
	const (
		numWriters         = 8
		numReaders         = 8
		writesPerGoroutine = 500
		readsPerGoroutine  = 500
	)

	s := &Server{}
	tuples := []pingScoreHistoryStatusSnapshot{
		{State: "initializing"},
		{State: "ok", LastCycleAt: "A"},
		{State: "degraded", Code: "cycle_failed", LastCycleAt: "A"},
		{State: "ok", LastCycleAt: "B"},
		{State: "read_only", Code: "read_only"},
		{State: "corrupt", Code: "corrupt"},
	}
	valid := make(map[pingScoreHistoryStatusSnapshot]bool, len(tuples))
	for _, tup := range tuples {
		valid[tup] = true
	}
	// A valid tuple is in place before any goroutine starts, so every
	// read -- including the very first one -- is guaranteed to observe a
	// member of `valid`, never the (also valid, but distinct) zero-value
	// default this same accessor returns before any Store at all.
	s.setPingScoreHistoryStatus(tuples[0].State, tuples[0].Code, tuples[0].LastCycleAt)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var writeCount, readCount, badReads atomic.Int64

	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for n := 0; n < writesPerGoroutine; n++ {
				tup := tuples[(i+n)%len(tuples)]
				s.setPingScoreHistoryStatus(tup.State, tup.Code, tup.LastCycleAt)
				writeCount.Add(1)
			}
		}(i)
	}
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < readsPerGoroutine; n++ {
				if got := s.pingScoreHistoryStatusView(); !valid[got] {
					badReads.Add(1)
				}
				readCount.Add(1)
			}
		}()
	}

	// Every goroutine now exists and is blocked on <-start; releasing it
	// is the only thing that lets any of them begin.
	close(start)
	wg.Wait()

	wantWrites := int64(numWriters * writesPerGoroutine)
	wantReads := int64(numReaders * readsPerGoroutine)
	if got := writeCount.Load(); got != wantWrites {
		t.Errorf("writeCount = %d, want exactly %d", got, wantWrites)
	}
	if got := readCount.Load(); got != wantReads {
		t.Errorf("readCount = %d, want exactly %d", got, wantReads)
	}
	if n := badReads.Load(); n != 0 {
		t.Errorf("%d of %d reads observed a tuple that was never one complete write (torn read)", n, wantReads)
	}
}
