package main

import (
	"context"
	"math"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
)

// #738 M2 — the two retryFill properties the #773 delta review found
// unpinned: concurrent viewers of a cached partial profile share one
// retry, and the retry writes into a copy of the caller's DTM.

// Concurrent viewers of a profile whose fill failed share one retry: one
// Fill call, however many hits arrive while it runs.
func TestTerrainFill_ConcurrentViewersShareOneRetry(t *testing.T) {
	var attempt atomic.Int32
	release := make(chan struct{})
	ok := everywhere(999)
	env, f := newScriptedEnv(t, func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
		if attempt.Add(1) == 1 {
			return copdem.Result{}, dhm.FetchError{Reason: "status 503"}
		}
		select {
		case <-release:
		case <-ctx.Done():
			return copdem.Result{}, ctx.Err()
		}
		return ok(ctx, p)
	})
	if resp := decodeProfile(t, env.get(pathQuery(cphPt, malmoPt))); resp.Coverage.State != "partial" {
		t.Fatalf("setup: the failed fill served %+v", resp.Coverage)
	}
	const viewers = 4
	var wg sync.WaitGroup
	resps := make([]TerrainProfileResponse, viewers)
	for i := 0; i < viewers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := env.get(pathQuery(cphPt, malmoPt))
			if w.Code == 200 {
				resps[i] = decodeProfile(t, w)
			}
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for env.p.flight.Pending() != viewers {
		if time.Now().After(deadline) {
			close(release)
			wg.Wait()
			t.Fatalf("%d callers waiting on the retry, want all %d viewers on one call (fills: %d)", env.p.flight.Pending(), viewers, f.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if f.count() != 2 {
		t.Fatalf("%d fills, want the failure and one shared retry", f.count())
	}
	for i, resp := range resps {
		if !resp.Coverage.Mixed || resp.Coverage.State != "full" {
			t.Fatalf("viewer %d: coverage %+v", i, resp.Coverage)
		}
	}
}

// The retry fills a copy: the caller's partial elevation e, which it
// serves if it leaves before the retry ends, is never written to.
func TestTerrainFill_RetryWritesIntoACopy(t *testing.T) {
	env, f := newScriptedEnv(t, everywhere(999))
	q, err := url.ParseQuery(pathQuery(cphPt, malmoPt))
	if err != nil {
		t.Fatal(err)
	}
	tq, err := parseTerrainQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	req, rerr := env.srv.resolveTerrainRequest(context.Background(), env.p, tq)
	if rerr != nil {
		t.Fatalf("resolve: %+v", rerr)
	}
	n := len(req.plan.E)
	e := terrainElevation{dtm: nanSlice(n), fillFailed: true}
	uncovered := 0
	for i := range e.dtm {
		if req.plan.Inside(i) {
			e.dtm[i] = fillDHMHeight
		} else {
			uncovered++
		}
	}
	if uncovered == 0 {
		t.Fatal("setup: the path has no uncovered sample")
	}
	key := terrainKey{A: req.ka, B: req.kb, Samples: n, Fill: true}
	got := env.p.retryFill(httptest.NewRequest("GET", "/api/terrain/profile", nil), key, &req.plan, e)
	if f.count() != 1 || got.fillFailed || got.fill == fillNone {
		t.Fatalf("retry: %d fills, result fill %d failed %v", f.count(), got.fill, got.fillFailed)
	}
	for i, v := range e.dtm {
		if req.plan.Inside(i) != !math.IsNaN(float64(v)) {
			t.Fatalf("the caller's DTM was written at sample %d: %v", i, v)
		}
	}
}
