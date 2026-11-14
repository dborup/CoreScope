package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #744 — cold hillshade tiles are admitted against a global bound before a
// worker exists, a refusal is a quick transparent no-store tile that costs
// the client nothing, and a client that leaves takes its queued work with
// it, while coalesced callers still share one upstream call.
//
// Every test runs against a fake upstream; none reaches Dataforsyningen.

// heldUpstream answers a tile only once release is closed, and records
// requests whose client (our fetch) hung up first.
func heldUpstream(t *testing.T) (up *fakeUpstream, release chan struct{}, abandoned *atomic.Int64) {
	t.Helper()
	release = make(chan struct{})
	abandoned = &atomic.Int64{}
	up = newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngStub)
		case <-r.Context().Done():
			abandoned.Add(1)
		}
	})
	return up, release, abandoned
}

// goroutineStacks counts live goroutines whose stack contains frame. It
// observes the workers independently of the code under test (the
// #744 report counted Client.fetch stacks the same way).
func goroutineStacks(frame string) int {
	var buf bytes.Buffer
	pprof.Lookup("goroutine").WriteTo(&buf, 2)
	return bytes.Count(buf.Bytes(), []byte(frame))
}

const (
	fetchFrame  = "dhm.(*Client).fetch("
	flightFrame = "dhm.(*Flight[...]).DoAdmitted("
)

func eventually744(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// aarhusTile is tile i of a row of distinct, in-coverage z16 tiles.
func aarhusTile(i int) url.Values {
	const z = 16
	x, y := lonLatTile(z, 10.20, 56.16)
	return leafletQuery(z, x+i, y)
}

func tileRequest(ctx context.Context, q url.Values, remote string) *http.Request {
	r := httptest.NewRequest("GET", "/api/map/dhm/wms?"+q.Encode(), nil).WithContext(ctx)
	if remote != "" {
		r.RemoteAddr = remote
	}
	return r
}

func assertBusyTile(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), dhmTransparentPNG) ||
		w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-CoreScope-Tile") != "busy" {
		t.Fatalf("refusal: status %d, tile %q, cache %q, %d bytes; want 200 busy no-store transparent",
			w.Code, w.Header().Get("X-CoreScope-Tile"), w.Header().Get("Cache-Control"), w.Body.Len())
	}
}

func assertMissTile(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), pngStub) || w.Header().Get("X-CoreScope-Tile") != "miss" {
		t.Fatalf("tile: status %d, state %q, %d bytes; want 200 miss with the upstream tile",
			w.Code, w.Header().Get("X-CoreScope-Tile"), w.Body.Len())
	}
}

// A flood of distinct cold tiles holds at most tileMaxInFlight workers;
// the rest are refused at once, before any worker exists.
func TestDHMTileAdmission744_FloodStaysBounded(t *testing.T) {
	for _, layer := range dhmTestLayers {
		t.Run("layer="+layer, func(t *testing.T) {
			const limit, total = 8, 96
			up, release, _ := heldUpstream(t)
			p := newTestDHMProxyLayer(t, up, layer, &dhmLogSink{}, func(o *dhmProxyOptions) {
				o.tileMaxInFlight = limit
				o.client.Timeout = 5 * time.Second
			})
			results := make(chan *httptest.ResponseRecorder, total)
			for i := 0; i < total; i++ {
				go func(i int) { results <- doTile(p, aarhusTile(i)) }(i)
			}
			deadline := time.After(2 * time.Second)
			for refused := 0; refused < total-limit; refused++ {
				select {
				case w := <-results:
					assertBusyTile(t, w)
				case <-deadline:
					close(release)
					t.Fatalf("only %d of %d requests refused while upstream was held; %d fetch workers live",
						refused, total-limit, goroutineStacks(fetchFrame))
				}
			}
			if n := goroutineStacks(fetchFrame); n > limit {
				t.Fatalf("%d fetch workers live, bound is %d", n, limit)
			}
			if n := p.admit.InUse(); n != limit {
				t.Fatalf("admission holds %d, want %d", n, limit)
			}
			if h := up.hits.Load(); h > 4 {
				t.Fatalf("upstream saw %d requests, concurrency is 4", h)
			}
			close(release)
			for i := 0; i < limit; i++ {
				assertMissTile(t, <-results)
			}
			if h := up.hits.Load(); h != limit {
				t.Fatalf("upstream calls %d, want %d admitted", h, limit)
			}
			// Every slot comes back once its worker has returned.
			eventually744(t, "admission drained", func() bool { return p.admit.InUse() == 0 })
			assertMissTile(t, doTile(p, aarhusTile(total)))
		})
	}
}

// Callers that leave take their queued and running work with them: the
// workers end, their slots return, and no upstream request completes.
func TestDHMTileAdmission744_CancellationRemovesQueuedWork(t *testing.T) {
	const n = 12
	up, release, abandoned := heldUpstream(t)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) {
		o.tileMaxInFlight = 16
		o.client.Timeout = 5 * time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, tileRequest(ctx, aarhusTile(i), ""))
			done <- w
		}(i)
	}
	eventually744(t, "4 active and 8 queued workers", func() bool {
		return up.hits.Load() == 4 && goroutineStacks(fetchFrame) == n
	})
	cancel()
	for i := 0; i < n; i++ {
		select {
		case w := <-done:
			if w.Body.Len() != 0 {
				t.Fatalf("a client that left was answered: %q, %d bytes", w.Header().Get("X-CoreScope-Tile"), w.Body.Len())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("handler %d still waiting after its client left", i)
		}
	}
	eventually744(t, "workers gone and slots returned", func() bool {
		return goroutineStacks(fetchFrame) == 0 && p.admit.InUse() == 0
	})
	// #749 review F1: during the mass cancel, a queued worker whose caller
	// has not processed its own cancel yet may take an upstream slot freed
	// by a cancelled one and start a request, and a request the transport
	// had already written may reach upstream after its worker is gone. So
	// the invariant is not "exactly 4 requests" but: every request that
	// reached upstream is abandoned, and none completes for anyone — no
	// tile is cached even once upstream would answer. (Upstream's own
	// count of answered requests cannot show this: a handler whose client
	// is gone but not yet noticed may still pick release and write to the
	// dead connection.)
	eventually744(t, "every upstream request abandoned", func() bool {
		return up.inFlight.Load() == 0 && abandoned.Load() == up.hits.Load()
	})
	close(release)
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < n; i++ {
		k, err := dhm.ParseTileRequest(aarhusTile(i))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.cache.Get(dhmCacheKey{Layer: p.layer, Tile: k}, p.now()); ok {
			t.Fatalf("tile %d completed and was cached after its client left", i)
		}
	}
	if n := goroutineStacks(fetchFrame); n != 0 {
		t.Fatalf("%d fetch workers after upstream answered", n)
	}
}

// #749 review F2: the exaggerated → terrain fallback (#687) goes through
// the same bound. With the exaggerated layer failing at once and terrain
// held, each fallback holds a slot; once the bound is full a new tile is
// refused at once, and live workers never exceed the bound.
func TestDHMTileAdmission744_FallbackStaysWithinBound(t *testing.T) {
	const limit = 2
	u := newDHMUpstreams(t)
	release := make(chan struct{})
	defer close(release)
	u.exaggerated.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	u.terrain.set(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p := newLayerProxy(t, u, "", &dhmLogSink{}, func(o *dhmProxyOptions) {
		o.tileMaxInFlight = limit
		o.client.Timeout = 5 * time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < limit; i++ {
		go p.ServeHTTP(httptest.NewRecorder(), tileRequest(ctx, aarhusTile(i), ""))
		eventually744(t, "fallback held upstream", func() bool { return u.terrain.hits() == i+1 })
	}
	if n := p.admit.InUse(); n != limit {
		t.Fatalf("admission holds %d with %d fallbacks held, want %d", n, limit, limit)
	}
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, tileRequest(ctx, aarhusTile(limit), ""))
		got <- w
	}()
	select {
	case w := <-got:
		assertBusyTile(t, w)
	case <-time.After(2 * time.Second):
		t.Fatalf("a tile past the bound was not refused; %d fetch workers live, bound %d",
			goroutineStacks(fetchFrame), limit)
	}
	if n := goroutineStacks(fetchFrame); n > limit {
		t.Fatalf("%d fetch workers live, bound is %d", n, limit)
	}
	if e := u.exaggerated.hits(); e != limit {
		t.Fatalf("exaggerated upstream calls %d, want %d (the refused tile goes nowhere)", e, limit)
	}
}

// Callers of one tile share one upstream call. Joining takes no admission
// slot, and callers leaving — even the one that started it — do not cancel
// it while any caller still waits.
func TestDHMTileAdmission744_CoalescedCallersShareOneCall(t *testing.T) {
	const n, leaving = 8, 3
	up, release, abandoned := heldUpstream(t)
	p := newTestDHMProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) {
		o.tileMaxInFlight = 1 // a joiner that took a slot would be refused
		o.client.Timeout = 5 * time.Second
	})
	q := aarhusTile(0)
	type result struct {
		i int
		w *httptest.ResponseRecorder
	}
	results := make(chan result, n)
	cancels := make([]context.CancelFunc, n)
	start := func(i int) {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go func() {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, tileRequest(ctx, q, ""))
			results <- result{i, w}
		}()
	}
	defer func() {
		for _, c := range cancels {
			if c != nil {
				c()
			}
		}
	}()
	start(0) // the starter is among those that leave
	eventually744(t, "the shared call upstream", func() bool { return up.hits.Load() == 1 })
	for i := 1; i < n; i++ {
		start(i)
	}
	eventually744(t, "every caller waiting", func() bool { return goroutineStacks(flightFrame) == n })
	for i := 0; i < leaving; i++ {
		cancels[i]()
	}
	for i := 0; i < leaving; i++ {
		select {
		case r := <-results:
			if r.i >= leaving {
				t.Fatalf("caller %d returned before the tile was released: %q", r.i, r.w.Header().Get("X-CoreScope-Tile"))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a caller that left is still waiting")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if a := abandoned.Load(); a != 0 {
		t.Fatal("shared upstream call cancelled while callers still wait")
	}
	close(release)
	for i := leaving; i < n; i++ {
		assertMissTile(t, (<-results).w)
	}
	if h := up.hits.Load(); h != 1 {
		t.Fatalf("%d callers of one tile made %d upstream calls", n, h)
	}
}

// A refusal did no upstream work, so it is not charged to the client's
// per-IP budget: a client refused while the server is busy still has its
// whole budget afterwards, and the budget itself still holds.
func TestDHMTileAdmission744_RefusalIsNotCharged(t *testing.T) {
	const client = "198.51.100.44:1000"
	up, release, _ := heldUpstream(t)
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, func(o *dhmProxyOptions) {
		o.tileMaxInFlight = 1
		o.rateLimitPerMin = 2
		o.client.Timeout = 5 * time.Second
	})
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, tileRequest(context.Background(), aarhusTile(0), client))
		held <- w
	}()
	eventually744(t, "the first tile upstream", func() bool { return up.hits.Load() == 1 })
	for i := 1; i <= 5; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, tileRequest(context.Background(), aarhusTile(i), client))
		if w.Code == http.StatusTooManyRequests {
			close(release)
			t.Fatalf("refusal %d was charged: 429 while the client had used 1 of 2", i)
		}
		assertBusyTile(t, w)
	}
	close(release)
	assertMissTile(t, <-held)
	eventually744(t, "the slot back", func() bool { return p.admit.InUse() == 0 })
	w := httptest.NewRecorder()
	p.ServeHTTP(w, tileRequest(context.Background(), aarhusTile(6), client))
	assertMissTile(t, w)
	w = httptest.NewRecorder()
	p.ServeHTTP(w, tileRequest(context.Background(), aarhusTile(7), client))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third upstream-bound tile in the minute: status %d, want 429", w.Code)
	}
	if h := up.hits.Load(); h != 2 {
		t.Fatalf("upstream calls %d, want 2", h)
	}
	// #749 review F4: a refusal is not an upstream failure.
	if l := logs.String(); strings.Count(l, "[dhm] hillshade tile refused: admission full, no upstream request\n") != 5 ||
		strings.Contains(l, "failed") {
		t.Fatalf("refusal log lines:\n%s", l)
	}
}

// AGENTS rule 0: the cache-hit hot path. #744 adds nothing to it; this
// benchmark is the before/after proof (PR description).
func BenchmarkDHMProxyCacheHit(b *testing.B) {
	up := newFakeUpstreamB(b)
	o := testDHMOptions(up.URL+"/", &dhmLogSink{})
	p := newDHMProxy(DHMConfig{Enabled: true, Token: dhmTestToken, Layer: "terrain"}, o)
	z, x, y := dkTile()
	r := httptest.NewRequest("GET", "/api/map/dhm/wms?"+leafletQuery(z, x, y).Encode(), nil)
	if w := httptest.NewRecorder(); func() string { p.ServeHTTP(w, r); return w.Header().Get("X-CoreScope-Tile") }() != "miss" {
		b.Fatal("warm-up was not a miss")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Header().Get("X-CoreScope-Tile") != "hit" {
			b.Fatal("not a cache hit")
		}
	}
}

func newFakeUpstreamB(b *testing.B) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngStub)
	}))
	b.Cleanup(srv.Close)
	return srv
}
