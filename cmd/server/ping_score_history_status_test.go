package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// E. Concurrent load/store: many goroutines hammering the setter and the
// accessor at once, under -race, must never produce a torn read -- every
// observed tuple must be exactly one of the complete tuples a writer
// actually stored, never a mix of State/Code/LastCycleAt from different
// calls. This is guaranteed by construction (a single atomic.Value.Store
// per write, of a plain string-only struct value), but is worth pinning
// down as an explicit regression test.
func TestPingScoreHistoryStatus_ConcurrentLoadStore_NoTornReads(t *testing.T) {
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
	s.setPingScoreHistoryStatus(tuples[0].State, tuples[0].Code, tuples[0].LastCycleAt)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var badReads atomic.Int64

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				tup := tuples[n%len(tuples)]
				s.setPingScoreHistoryStatus(tup.State, tup.Code, tup.LastCycleAt)
				n++
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := s.pingScoreHistoryStatusView(); !valid[got] {
					badReads.Add(1)
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if n := badReads.Load(); n != 0 {
		t.Errorf("%d reads observed a tuple that was never one complete write (torn read)", n)
	}
}
