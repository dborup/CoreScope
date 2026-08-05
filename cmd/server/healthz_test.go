package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzNotReady(t *testing.T) {
	// Ensure readiness is 0 (not ready)
	readiness.Store(0)
	defer readiness.Store(0)

	srv := &Server{store: &PacketStore{}}
	req := httptest.NewRequest("GET", "/api/healthz", nil)
	w := httptest.NewRecorder()

	srv.handleHealthz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["ready"] != false {
		t.Fatalf("expected ready=false, got %v", resp["ready"])
	}
	if resp["reason"] != "loading" {
		t.Fatalf("expected reason=loading, got %v", resp["reason"])
	}
}

func TestHealthzReady(t *testing.T) {
	readiness.Store(1)
	defer readiness.Store(0)

	srv := &Server{store: &PacketStore{}}
	req := httptest.NewRequest("GET", "/api/healthz", nil)
	w := httptest.NewRecorder()

	srv.handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["ready"] != true {
		t.Fatalf("expected ready=true, got %v", resp["ready"])
	}
	if _, ok := resp["loadedTx"]; !ok {
		t.Fatal("missing loadedTx field")
	}
	if _, ok := resp["loadedObs"]; !ok {
		t.Fatal("missing loadedObs field")
	}
}

func TestHealthzAntiTautology(t *testing.T) {
	// When readiness is 0, must NOT return 200
	readiness.Store(0)
	defer readiness.Store(0)

	srv := &Server{store: &PacketStore{}}
	req := httptest.NewRequest("GET", "/api/healthz", nil)
	w := httptest.NewRecorder()

	srv.handleHealthz(w, req)

	if w.Code == http.StatusOK {
		t.Fatal("anti-tautology: handler returned 200 when readiness=0; gating is broken")
	}
}

// F. Default ping_scores_history: a Server that never called
// setPingScoreHistoryStatus reports the documented initializing default,
// in the exact stable camelCase JSON shape, without affecting the
// existing 200 readiness status.
func TestHealthzReady_DefaultPingScoresHistoryStatus(t *testing.T) {
	readiness.Store(1)
	defer readiness.Store(0)

	srv := &Server{store: &PacketStore{}}
	req := httptest.NewRequest("GET", "/api/healthz", nil)
	w := httptest.NewRecorder()

	srv.handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	raw, ok := resp["ping_scores_history"]
	if !ok {
		t.Fatal("missing ping_scores_history field")
	}

	var got pingScoreHistoryStatusSnapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid ping_scores_history JSON: %v", err)
	}
	if want := (pingScoreHistoryStatusSnapshot{State: "initializing"}); got != want {
		t.Errorf("ping_scores_history = %+v, want %+v", got, want)
	}

	// Exact stable JSON shape, not just Go-side field equality.
	if raw := string(raw); raw != `{"state":"initializing","code":"","lastCycleAt":""}` {
		t.Errorf("ping_scores_history JSON = %s, want the exact stable camelCase shape", raw)
	}
}

// G. Degraded ping_scores_history: reflects a set status exactly, never
// leaks raw error text, and still leaves the existing 200 untouched.
func TestHealthzReady_DegradedPingScoresHistoryStatus_NoRawErrorText(t *testing.T) {
	readiness.Store(1)
	defer readiness.Store(0)

	srv := &Server{store: &PacketStore{}}
	srv.setPingScoreHistoryStatus("degraded", "cycle_failed", "2026-08-05T12:00:00Z")

	req := httptest.NewRequest("GET", "/api/healthz", nil)
	w := httptest.NewRecorder()
	srv.handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (readiness must not be affected by history status), got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"ping_scores_history":{"state":"degraded","code":"cycle_failed","lastCycleAt":"2026-08-05T12:00:00Z"}`) {
		t.Errorf("body = %s, want the exact ping_scores_history JSON shape", body)
	}
	for _, forbidden := range []string{"Detail", "SELECT", ".db", "/Users/", "panic:", "goroutine "} {
		if strings.Contains(body, forbidden) {
			t.Errorf("body contains %q -- raw error/path/SQL/stack detail must never leak through healthz", forbidden)
		}
	}
}

// H. corrupt/read_only/panic all serialize correctly and never change
// the readiness-driven HTTP status.
func TestHealthzReady_PingScoresHistoryStates_SerializeWithoutAffectingReadiness(t *testing.T) {
	cases := []struct {
		state, code, lastCycleAt string
	}{
		{"corrupt", "corrupt", ""},
		{"read_only", "read_only", "2026-08-01T00:00:00Z"},
		{"panic", "panic", "2026-08-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			readiness.Store(1)
			defer readiness.Store(0)

			srv := &Server{store: &PacketStore{}}
			srv.setPingScoreHistoryStatus(tc.state, tc.code, tc.lastCycleAt)

			req := httptest.NewRequest("GET", "/api/healthz", nil)
			w := httptest.NewRecorder()
			srv.handleHealthz(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("state=%s: expected 200 (history state must never gate readiness), got %d", tc.state, w.Code)
			}

			var resp map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			var got pingScoreHistoryStatusSnapshot
			if err := json.Unmarshal(resp["ping_scores_history"], &got); err != nil {
				t.Fatalf("invalid ping_scores_history JSON: %v", err)
			}
			want := pingScoreHistoryStatusSnapshot{State: tc.state, Code: tc.code, LastCycleAt: tc.lastCycleAt}
			if got != want {
				t.Errorf("ping_scores_history = %+v, want %+v", got, want)
			}
		})
	}
}

// I. Existing not-ready gate: even an "ok" ping-scores-history status
// cannot make healthz return 200 while the existing global readiness is
// still 0 -- proves history status cannot override the readiness gate.
func TestHealthzNotReady_PingScoresHistoryCannotOverrideReadinessGate(t *testing.T) {
	readiness.Store(0)
	defer readiness.Store(0)

	srv := &Server{store: &PacketStore{}}
	srv.setPingScoreHistoryStatus("ok", "", "2026-08-05T12:00:00Z")

	req := httptest.NewRequest("GET", "/api/healthz", nil)
	w := httptest.NewRecorder()
	srv.handleHealthz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 even with an ok ping-scores-history status, got %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "ping_scores_history") {
		t.Errorf("body = %s, want no ping_scores_history key at all in the not-ready response", body)
	}
}
