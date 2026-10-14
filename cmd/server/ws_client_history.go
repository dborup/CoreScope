package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// In-memory history of open WebSocket connections, shown on the Perf page.
// Memory only: cmd/server is read-only (#1283), so nothing is persisted and
// the history restarts empty with the process.
const (
	wsHistoryInterval = time.Minute
	wsHistoryCapacity = 24 * 60 // 24 h at one sample per minute
)

// WSClientSample is one point on the connection-count curve.
type WSClientSample struct {
	T       time.Time `json:"t"`
	Clients int       `json:"clients"`
	// Peak is the highest count seen since the previous sample, so a short
	// spike between two samples is not lost.
	Peak int `json:"peak"`
}

// WSClientHistoryResp is the /api/perf/ws-clients response.
type WSClientHistoryResp struct {
	IntervalSec int              `json:"intervalSec"`
	Samples     []WSClientSample `json:"samples"`
}

// wsClientHistory is a fixed-size ring of samples. Its own mutex is separate
// from Hub.mu and never held while taking it.
type wsClientHistory struct {
	mu   sync.Mutex
	buf  []WSClientSample
	head int // next write position
	n    int // samples stored, <= len(buf)
	peak int // highest count since the last sample
}

func newWSClientHistory(capacity int) *wsClientHistory {
	if capacity < 1 {
		capacity = 1
	}
	return &wsClientHistory{buf: make([]WSClientSample, capacity)}
}

// observe records a connection count; O(1), called on every connect.
func (h *wsClientHistory) observe(n int) {
	h.mu.Lock()
	if n > h.peak {
		h.peak = n
	}
	h.mu.Unlock()
}

// sample appends the current count and starts a new peak interval.
func (h *wsClientHistory) sample(current int, at time.Time) {
	h.mu.Lock()
	peak := h.peak
	if current > peak {
		peak = current
	}
	h.buf[h.head] = WSClientSample{T: at, Clients: current, Peak: peak}
	h.head = (h.head + 1) % len(h.buf)
	if h.n < len(h.buf) {
		h.n++
	}
	h.peak = current
	h.mu.Unlock()
}

// snapshot returns the samples oldest-first. Never nil, so it encodes as [].
func (h *wsClientHistory) snapshot() []WSClientSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]WSClientSample, h.n)
	start := (h.head - h.n + len(h.buf)) % len(h.buf)
	for i := 0; i < h.n; i++ {
		out[i] = h.buf[(start+i)%len(h.buf)]
	}
	return out
}

// StartHistorySampler samples the connection count every interval until the
// returned stop function is called. stop blocks until the goroutine has
// exited and is safe to call more than once.
func (h *Hub) StartHistorySampler(interval time.Duration) (stop func()) {
	h.mu.Lock()
	h.historyInterval = interval
	h.mu.Unlock()

	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				h.history.sample(h.ClientCount(), time.Now().UTC())
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

func (s *Server) handlePerfWSClients(w http.ResponseWriter, r *http.Request) {
	resp := WSClientHistoryResp{IntervalSec: int(wsHistoryInterval / time.Second), Samples: []WSClientSample{}}
	if s.hub != nil {
		s.hub.mu.RLock()
		if s.hub.historyInterval > 0 {
			resp.IntervalSec = int(s.hub.historyInterval / time.Second)
		}
		s.hub.mu.RUnlock()
		resp.Samples = s.hub.history.snapshot()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
