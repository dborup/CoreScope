package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
)

func TestRegionScopeAdminRouteRequiresKeyAndObservedCandidate(t *testing.T) {
	db := setupTestDB(t)
	db.path = filepath.Join(t.TempDir(), "scope.db")
	seedTestData(t, db)
	if _, err := db.conn.Exec(`CREATE TABLE approved_region_scopes (name TEXT PRIMARY KEY, status TEXT, created_at INTEGER, reviewed_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`CREATE TABLE IF NOT EXISTS observer_neighbors (observer_id TEXT,neighbor_pubkey TEXT,scopes TEXT,status TEXT,reported_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node','#candidate','responded')`); err != nil {
		t.Fatal(err)
	}
	s := NewServer(db, &Config{APIKey: strongTestKey}, NewHub())
	router := mux.NewRouter()
	s.RegisterRoutes(router)
	request := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, bytes.NewBufferString(`{"name":"#candidate"}`))
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := request("/api/admin/region-scopes/approve", ""); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Fatalf("anonymous = %d %s", w.Code, w.Body)
	}
	// A candidate may be queued only when the server has a real DB path.
	if w := request("/api/admin/region-scopes/approve", strongTestKey); w.Code != http.StatusAccepted {
		t.Fatalf("observed = %d %s", w.Code, w.Body)
	} else if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("accepted Content-Type = %q", got)
	}
	if _, err := db.conn.Exec(`INSERT INTO approved_region_scopes(name,status,created_at,reviewed_at) VALUES('#candidate','approved',1,2)`); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest("GET", "/api/observers/neighbors", nil))
	if get.Code != 200 {
		t.Fatalf("neighbors = %d %s", get.Code, get.Body)
	}
	if bytes.Contains(get.Body.Bytes(), []byte(`"scope":"#candidate"`)) {
		t.Fatalf("approved scope still listed as unknown: %s", get.Body)
	}
	if w := request("/api/admin/region-scopes/revoke", strongTestKey); w.Code != http.StatusAccepted {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
}
