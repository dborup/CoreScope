package main

// #724 M4 — the #758 review follow-ups on GET /api/masts (F5, F6).

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/mastedb"
)

// F5: a starved fetch (admitted, but no upstream slot before the deadline)
// names the upstream slots as the bottleneck, not maxInFlight, and an
// admission refusal keeps its own maxInFlight advice. Each case has its
// own line, so an operator raises the right knob.
func TestMastsStarvedAndRefusedLogsDiffer(t *testing.T) {
	f := newFakeWFS(t)
	f.gate = make(chan struct{})
	var logs safeLog
	opts := testMastsOptions(f, &logs)
	opts.maxInFlight = mastsUpstreamSlots + 1
	p := mustMastsProxy(t, opts)

	var wg sync.WaitGroup
	for i := 0; i < mastsUpstreamSlots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mastsGet(p, cellQuery(mastedb.Cell{E: 263 + i, N: 3127}), fmt.Sprintf("198.51.100.%d:40000", 30+i))
		}(i)
	}
	waitMasts(t, func() bool { return f.inFlight.Load() == mastsUpstreamSlots })
	p.deadline = 150 * time.Millisecond

	// Starved: admission has room (one slot left), the upstream slots do not.
	if w := mastsGet(p, cellQuery(mastedb.Cell{E: 300, N: 3127}), clientA); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("starved fetch: status %d, want 503", w.Code)
	}
	starved := logs.String()
	if !strings.Contains(starved, "upstream slots") || strings.Contains(starved, "raise maxInFlight") {
		t.Fatalf("starved fetch logged %q, want the upstream-slots line without the maxInFlight advice", starved)
	}

	// Refused: fill the last admission slot with a fetch that waits for a
	// slot until the gate opens, then ask for one more cell.
	p.deadline = mastsDeadline
	wg.Add(1)
	go func() {
		defer wg.Done()
		mastsGet(p, cellQuery(mastedb.Cell{E: 290, N: 3127}), "198.51.100.40:40000")
	}()
	waitMasts(t, func() bool { return p.admit.InUse() == p.admit.Cap() })
	if w := mastsGet(p, cellQuery(mastedb.Cell{E: 301, N: 3127}), clientB); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("refused fetch: status %d, want 503", w.Code)
	}
	refused := strings.TrimPrefix(logs.String(), starved)
	if !strings.Contains(refused, "raise maxInFlight") || strings.Contains(refused, "upstream slots") {
		t.Fatalf("admission refusal logged %q, want the maxInFlight line only", refused)
	}
	close(f.gate)
	wg.Wait()
}

// F6: allow carries no count, so it must not reset the running total a
// note on the same throttle reports from. A throttle used both ways
// reports the growth since the last note, not the whole total.
func TestLogThrottleAllowKeepsNoteCount(t *testing.T) {
	var th logThrottle
	t0 := time.Unix(1000, 0)
	if n, ok := th.note(t0, 5, time.Minute); !ok || n != 5 {
		t.Fatalf("first note: %d %v, want 5 true", n, ok)
	}
	if !th.allow(t0.Add(time.Minute), time.Minute) {
		t.Fatal("allow after the interval was refused")
	}
	if n, ok := th.note(t0.Add(2*time.Minute), 8, time.Minute); !ok || n != 3 {
		t.Fatalf("note after an allow: %d %v, want 3 true (allow reset the count)", n, ok)
	}
}
