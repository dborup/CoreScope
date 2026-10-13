package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWSClientHistory_RingWrapsAndStaysOrdered(t *testing.T) {
	h := newWSClientHistory(3)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		h.sample(i, base.Add(time.Duration(i)*time.Minute))
	}
	got := h.snapshot()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (capacity)", len(got))
	}
	for i, want := range []int{3, 4, 5} {
		if got[i].Clients != want {
			t.Errorf("sample %d clients = %d, want %d (oldest first)", i, got[i].Clients, want)
		}
	}
}

func TestWSClientHistory_PeakCapturesSpikeBetweenSamples(t *testing.T) {
	h := newWSClientHistory(10)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	h.observe(40) // a spike that is gone again by the time we sample
	h.sample(5, base)
	got := h.snapshot()
	if got[0].Clients != 5 || got[0].Peak != 40 {
		t.Fatalf("got clients=%d peak=%d, want 5/40", got[0].Clients, got[0].Peak)
	}
}

func TestWSClientHistory_PeakResetsPerInterval(t *testing.T) {
	h := newWSClientHistory(10)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	h.observe(40)
	h.sample(5, base)
	h.sample(6, base.Add(time.Minute))
	got := h.snapshot()
	if got[1].Peak != 6 {
		t.Fatalf("second peak = %d, want 6 (must not carry the earlier spike)", got[1].Peak)
	}
}

func TestWSClientHistory_PeakNeverBelowClients(t *testing.T) {
	h := newWSClientHistory(10)
	h.sample(7, time.Now())
	if got := h.snapshot(); got[0].Peak < got[0].Clients {
		t.Fatalf("peak %d < clients %d", got[0].Peak, got[0].Clients)
	}
}

func TestWSClientHistory_EmptySnapshotIsNonNil(t *testing.T) {
	// JSON must encode as [] not null so the frontend can .map() it.
	s := newWSClientHistory(3).snapshot()
	if s == nil || len(s) != 0 {
		t.Fatalf("snapshot = %#v, want empty non-nil slice", s)
	}
}

func TestWSClientHistory_ConcurrentRace(t *testing.T) {
	h := newWSClientHistory(50)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				switch g % 3 {
				case 0:
					h.observe(i)
				case 1:
					h.sample(i, time.Now())
				default:
					_ = h.snapshot()
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestHub_RegisterFeedsHistoryPeak(t *testing.T) {
	hub := NewHub()
	mk := func() *Client { return &Client{send: make(chan []byte, 1)} }
	a, b, c := mk(), mk(), mk()
	hub.Register(a)
	hub.Register(b)
	hub.Register(c)
	hub.Unregister(a)
	hub.Unregister(b)
	hub.Unregister(c)
	hub.history.sample(hub.ClientCount(), time.Now())
	got := hub.history.snapshot()
	if got[0].Clients != 0 || got[0].Peak != 3 {
		t.Fatalf("got clients=%d peak=%d, want 0/3", got[0].Clients, got[0].Peak)
	}
}

func TestHub_HistorySamplerSamplesAndStops(t *testing.T) {
	hub := NewHub()
	stop := hub.StartHistorySampler(5 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for len(hub.history.snapshot()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("sampler produced no samples")
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	stop() // idempotent
	n := len(hub.history.snapshot())
	time.Sleep(30 * time.Millisecond)
	if len(hub.history.snapshot()) != n {
		t.Fatal("sampler kept running after stop()")
	}
}

func TestHandlePerfWSClients_Shape(t *testing.T) {
	srv, router := setupNoStoreServer(t)
	srv.hub.history.sample(4, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	srv.hub.historyInterval = time.Minute

	req := httptest.NewRequest("GET", "/api/perf/ws-clients", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp WSClientHistoryResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.IntervalSec != 60 {
		t.Errorf("intervalSec = %d, want 60", resp.IntervalSec)
	}
	if len(resp.Samples) != 1 || resp.Samples[0].Clients != 4 {
		t.Errorf("samples = %#v", resp.Samples)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"intervalSec", "samples"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestHandlePerfWSClients_NilHubDoesNotPanic(t *testing.T) {
	srv, router := setupNoStoreServer(t)
	srv.hub = nil
	req := httptest.NewRequest("GET", "/api/perf/ws-clients", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

// Register runs on every connect, so the extra history bookkeeping must stay
// negligible next to the map insert it already does.
func BenchmarkHubRegisterUnregister(b *testing.B) {
	hub := NewHub()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := &Client{send: make(chan []byte, 1)}
		hub.Register(c)
		hub.Unregister(c)
	}
}
