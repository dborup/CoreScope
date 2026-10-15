package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Bounded connection-count history. The optional journal is a separate file;
// the packet database remains read-only (#1283).
const (
	wsHistoryInterval  = time.Minute
	wsHistoryRetention = 30 * 24 * time.Hour
	wsHistoryCapacity  = 30 * 24 * 60
)

// WSClientSample is one point on the connection-count curve.
type WSClientSample struct {
	T       time.Time `json:"t"`
	Clients int       `json:"clients"`
	// Peak is the highest count seen since the previous sample, so a short
	// spike between two samples is not lost.
	Peak      int  `json:"peak"`
	GapBefore bool `json:"gapBefore,omitempty"`
}

type WSHistoryPersistence struct {
	Enabled bool `json:"enabled"`
	Healthy bool `json:"healthy"`
}

// WSClientHistoryResp is the /api/perf/ws-clients response.
type WSClientHistoryResp struct {
	IntervalSec int                  `json:"intervalSec"`
	Samples     []WSClientSample     `json:"samples"`
	Range       string               `json:"range"`
	Persistence WSHistoryPersistence `json:"persistence"`
}

// wsClientHistory is a fixed-size ring of samples. Its own mutex is separate
// from Hub.mu and never held while taking it.
type wsClientHistory struct {
	mu          sync.Mutex
	buf         []WSClientSample
	head        int // next write position
	n           int // samples stored, <= len(buf)
	capacity    int
	peak        int // highest count since the last sample
	nextGap     bool
	journal     *wsHistoryJournal // set before the sampler/listener starts
	persistence WSHistoryPersistence
}

func newWSClientHistory(capacity int) *wsClientHistory {
	if capacity < 1 {
		capacity = 1
	}
	initial := capacity
	if initial > 24*60 {
		initial = 24 * 60
	}
	return &wsClientHistory{buf: make([]WSClientSample, initial), capacity: capacity}
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
func (h *wsClientHistory) sample(current int, at time.Time) WSClientSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n > 0 && !at.After(h.buf[(h.head-1+len(h.buf))%len(h.buf)].T) {
		// Wall-clock rollback: do not fabricate timestamps or make the saved
		// series unordered. The next strictly newer sample resumes it.
		return WSClientSample{}
	}
	if h.n == len(h.buf) && len(h.buf) < h.capacity {
		next := 2 * len(h.buf)
		if next > h.capacity {
			next = h.capacity
		}
		grown := make([]WSClientSample, next)
		for i := 0; i < h.n; i++ {
			grown[i] = h.buf[(h.head+i)%len(h.buf)]
		}
		h.buf, h.head = grown, h.n
	}
	peak := h.peak
	if current > peak {
		peak = current
	}
	gap := h.nextGap && h.n > 0
	if h.n > 0 && at.Sub(h.buf[(h.head-1+len(h.buf))%len(h.buf)].T) > 2*wsHistoryInterval {
		gap = true
	}
	s := WSClientSample{T: at, Clients: current, Peak: peak, GapBefore: gap}
	h.buf[h.head] = s
	h.head = (h.head + 1) % len(h.buf)
	if h.n < len(h.buf) {
		h.n++
	}
	h.peak = current
	h.nextGap = false
	return s
}

func (h *wsClientHistory) markRestartGap() {
	h.mu.Lock()
	h.nextGap = true
	h.mu.Unlock()
}

type wsHistoryRangeSpec struct {
	name               string
	duration, interval time.Duration
}

func wsHistoryRange(name string) (wsHistoryRangeSpec, bool) {
	if name == "" {
		name = "24h"
	}
	switch name {
	case "24h":
		return wsHistoryRangeSpec{name, 24 * time.Hour, time.Minute}, true
	case "7d":
		return wsHistoryRangeSpec{name, 7 * 24 * time.Hour, 5 * time.Minute}, true
	case "30d":
		return wsHistoryRangeSpec{name, wsHistoryRetention, 15 * time.Minute}, true
	default:
		return wsHistoryRangeSpec{}, false
	}
}

// Aggregate outside the ring lock. Counts are the LAST actual sample in a
// bucket, never a fabricated average; maxima include every sample's peak.
// A restart gap starts a fresh aggregate even within the same bucket.
func (h *wsClientHistory) snapshotRange(now time.Time, r wsHistoryRangeSpec) []WSClientSample {
	samples := h.snapshot()
	capacity := int(r.duration/r.interval) + 1
	if capacity > len(samples) {
		capacity = len(samples)
	}
	out := make([]WSClientSample, 0, capacity)
	cutoff := now.Add(-r.duration)
	var bucket time.Time
	for _, s := range samples {
		if s.T.Before(cutoff) || s.T.After(now) {
			continue
		}
		b := s.T.Truncate(r.interval)
		if len(out) == 0 || !b.Equal(bucket) || s.GapBefore {
			out = append(out, s)
			bucket = b
			continue
		}
		last := &out[len(out)-1]
		if s.Peak > last.Peak {
			last.Peak = s.Peak
		}
		last.T, last.Clients = s.T, s.Clients
	}
	return out
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
				h.history.sampleAndPersist(h.ClientCount(), time.Now().UTC())
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
		h.history.closePersistence()
	}
}

func (s *Server) handlePerfWSClients(w http.ResponseWriter, r *http.Request) {
	rangeSpec, ok := wsHistoryRange(r.URL.Query().Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "range must be 24h, 7d or 30d")
		return
	}
	resp := WSClientHistoryResp{IntervalSec: int(rangeSpec.interval / time.Second), Samples: []WSClientSample{}, Range: rangeSpec.name}
	if s.hub != nil {
		resp.Samples = s.hub.history.snapshotRange(time.Now().UTC(), rangeSpec)
		resp.Persistence = s.hub.history.persistenceStatus()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
