package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// setupTestServerForObserverStats builds a Server+PacketStore harness like
// setupTestServer, but skips seedTestData so these observer-count
// assertions start from a clean observers table instead of having to
// account for the "obs1"/"obs2" baseline fixture rows.
func setupTestServerForObserverStats(t *testing.T) (*Server, *mux.Router) {
	t.Helper()
	db := setupTestDB(t)
	cfg := &Config{Port: 3000}
	hub := NewHub()
	srv := NewServer(db, cfg, hub)
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load failed: %v", err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatalf("background indexes never became ready")
	}
	store.config = cfg
	srv.store = store
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return srv, router
}

// seedObserver inserts a minimal observer row. Pass inactive as 0, 1, or
// nil (for SQL NULL) -- both 0 and NULL mean "active" per the
// `inactive IS NULL OR inactive = 0` predicate used throughout the code.
func seedObserver(t *testing.T, db *DB, id string, inactive interface{}) {
	t.Helper()
	_, err := db.conn.Exec(
		`INSERT INTO observers (id, name, iata, last_seen, inactive) VALUES (?, ?, 'SFO', datetime('now'), ?)`,
		id, id, inactive,
	)
	if err != nil {
		t.Fatalf("seed observer %s: %v", id, err)
	}
}

func fetchTotalObservers(t *testing.T, router *mux.Router) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/stats: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp StatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse /api/stats response: %v", err)
	}
	return resp.TotalObservers
}

func fetchObserversListLength(t *testing.T, router *mux.Router) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/observers", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/observers: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp ObserverListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse /api/observers response: %v", err)
	}
	return len(resp.Observers)
}

// A. TestGetStoreStats_ObserverCount verifies that PacketStore.GetStoreStats()
// -- the function underlying /api/stats in the production s.store != nil
// path -- counts only DB-active observers (inactive IS NULL OR inactive = 0),
// not every row ever written to the table (issue #1888).
func TestGetStoreStats_ObserverCount(t *testing.T) {
	srv, _ := setupTestServerForObserverStats(t)

	seedObserver(t, srv.db, "active1", 0)
	seedObserver(t, srv.db, "active2", nil) // NULL also means active
	seedObserver(t, srv.db, "inactive1", 1)

	st, err := srv.store.GetStoreStats()
	if err != nil {
		t.Fatalf("GetStoreStats: %v", err)
	}
	if st.TotalObservers != 2 {
		t.Errorf("TotalObservers = %d, want 2 (active1 + active2; inactive1 must be excluded)", st.TotalObservers)
	}
}

// B. TestHandleStats_StorePath_ActiveObserverCount proves two things at
// once about the production /api/stats handler:
//  1. totalObservers reflects only DB-active observers.
//  2. The response was actually produced by s.store.GetStoreStats() (the
//     s.store != nil branch), not by the s.db.GetStats() fallback.
//
// (2) can't be proven by the observer count alone once the fix lands,
// because both code paths apply the same active-observer filter and would
// then agree on that number. So this test also inserts a transmission row
// directly into the DB *after* store.Load() has already taken its
// in-memory snapshot. GetStoreStats() derives TotalTransmissions from that
// frozen snapshot (len(s.packets)) and cannot see the post-Load row, while
// DB.GetStats() runs a live "SELECT COUNT(*) FROM transmissions" and would
// see it. TotalTransmissions == 0 in the response is therefore direct
// evidence the store path served it, independent of the observer fix.
func TestHandleStats_StorePath_ActiveObserverCount(t *testing.T) {
	srv, router := setupTestServerForObserverStats(t)
	if srv.store == nil {
		t.Fatal("test setup invariant broken: srv.store must be non-nil to exercise the production s.store != nil branch")
	}

	seedObserver(t, srv.db, "active1", 0)
	seedObserver(t, srv.db, "active2", nil)
	seedObserver(t, srv.db, "inactive1", 1)

	if _, err := srv.db.conn.Exec(
		`INSERT INTO transmissions (raw_hex, hash, first_seen) VALUES ('aa', 'post-load-canary', datetime('now'))`,
	); err != nil {
		t.Fatalf("seed post-load transmission: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp StatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.TotalObservers != 2 {
		t.Errorf("totalObservers = %d, want 2 (active1 + active2; inactive1 must be excluded)", resp.TotalObservers)
	}

	if resp.TotalTransmissions == nil {
		t.Fatal("TotalTransmissions is nil")
	}
	if *resp.TotalTransmissions != 0 {
		t.Errorf("TotalTransmissions = %d, want 0 -- a nonzero count means handleStats served this response from "+
			"DB.GetStats() (the fallback, which re-queries the live transmissions table and would see the "+
			"post-Load row) instead of s.store.GetStoreStats() (the production path, which reflects the "+
			"in-memory snapshot frozen at store.Load() time, before that row existed)", *resp.TotalTransmissions)
	}
}

// C. TestObserverStatsParity locks down the corrected cross-endpoint
// invariant (design report v2, reviewfund #1): after the fix,
// stats.totalObservers and len(/api/observers) both draw from the same
// active-observer set, so without a blacklist they must match exactly.
// With a blacklist, buildObserversDefaultResponse()'s defense-in-depth
// filter can only ever *remove* rows on top of that shared active set, so
// stats.totalObservers may be strictly greater than the observers list --
// by precisely the number of active rows the blacklist filter removes,
// never by the number of inactive rows (which both endpoints already
// exclude identically).
func TestObserverStatsParity(t *testing.T) {
	t.Run("no blacklist: stats total equals observers list length exactly", func(t *testing.T) {
		srv, router := setupTestServerForObserverStats(t)

		seedObserver(t, srv.db, "active1", 0)
		seedObserver(t, srv.db, "active2", nil)
		seedObserver(t, srv.db, "active3", 0)
		seedObserver(t, srv.db, "inactive1", 1)
		seedObserver(t, srv.db, "inactive2", 1)

		statsTotal := fetchTotalObservers(t, router)
		observersLen := fetchObserversListLength(t, router)

		if statsTotal != 3 {
			t.Errorf("stats.totalObservers = %d, want 3 active rows (inactive1/2 excluded)", statsTotal)
		}
		if observersLen != 3 {
			t.Errorf("len(/api/observers) = %d, want 3 active rows (inactive1/2 excluded)", observersLen)
		}
		if statsTotal != observersLen {
			t.Errorf("without a blacklist, stats.totalObservers (%d) must equal len(/api/observers) (%d) exactly", statsTotal, observersLen)
		}
	})

	t.Run("with server blacklist: difference equals K active blacklisted rows, never the inactive count", func(t *testing.T) {
		srv, router := setupTestServerForObserverStats(t)
		srv.cfg.ObserverBlacklist = []string{"blacklisted1", "blacklisted2"}
		const K = 2 // number of active rows the blacklist filter removes below

		seedObserver(t, srv.db, "clean1", 0)
		seedObserver(t, srv.db, "clean2", nil)
		seedObserver(t, srv.db, "blacklisted1", 0)   // active AND blacklisted
		seedObserver(t, srv.db, "blacklisted2", nil) // active AND blacklisted
		// Inactive rows below are a distractor: if the difference were
		// (wrongly) computed against inactive rows instead of the
		// blacklist, these would inflate it past K.
		seedObserver(t, srv.db, "inactive1", 1)
		seedObserver(t, srv.db, "inactive2", 1)
		seedObserver(t, srv.db, "inactive3", 1)

		statsTotal := fetchTotalObservers(t, router)
		observersLen := fetchObserversListLength(t, router)

		if statsTotal != 4 {
			t.Errorf("stats.totalObservers = %d, want 4 active rows (clean1, clean2, blacklisted1, blacklisted2; inactive1-3 excluded)", statsTotal)
		}
		if observersLen != 2 {
			t.Errorf("len(/api/observers) = %d, want 2 (clean1, clean2; blacklisted1/2 removed by the defense-in-depth filter, inactive1-3 excluded)", observersLen)
		}
		if diff := statsTotal - observersLen; diff != K {
			t.Errorf("stats.totalObservers - len(/api/observers) = %d, want exactly K=%d -- the difference must equal "+
				"only the active rows the blacklist filter removes, never the inactive row count (3 inactive rows "+
				"were seeded specifically to catch that confusion)", diff, K)
		}
	})
}
