package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/regions"
)

func scopeReviewServer(t *testing.T, cfg *Config, scopes string) (*mux.Router, *DB) {
	t.Helper()
	db := setupTestDB(t)
	db.path = filepath.Join(t.TempDir(), "scope.db")
	seedTestData(t, db)
	for _, stmt := range []string{
		`CREATE TABLE approved_region_scopes (name TEXT PRIMARY KEY, status TEXT, created_at INTEGER, reviewed_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS observer_neighbors (observer_id TEXT,neighbor_pubkey TEXT,scopes TEXT,status TEXT,reported_at TEXT)`,
	} {
		if _, err := db.conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.conn.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node',?,'responded')`, scopes); err != nil {
		t.Fatal(err)
	}
	cfg.APIKey = strongTestKey
	router := mux.NewRouter()
	NewServer(db, cfg, NewHub()).RegisterRoutes(router)
	return router, db
}

func postScope(router *mux.Router, action, name string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/admin/region-scopes/"+action, bytes.NewBufferString(`{"name":"`+name+`"}`))
	req.Header.Set("X-API-Key", strongTestKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func queuedScopeRequests(t *testing.T, db *DB) int {
	t.Helper()
	pending, err := channelregistry.NewQueue(filepath.Join(filepath.Dir(db.path), "region-scope-requests")).Pending()
	if err != nil {
		t.Fatal(err)
	}
	return len(pending)
}

// Review of #355, gap 3: once the overlay is full, an approval is refused
// synchronously, so the admin sees the documented cap instead of a queued
// request that the ingestor later fails, and nothing is queued.
func TestRegionScopeApproveAtCapIsRefusedWithoutQueueing(t *testing.T) {
	router, db := scopeReviewServer(t, &Config{}, "#candidate")
	for i := 0; i < regions.MaxApprovedScopes; i++ {
		if _, err := db.conn.Exec(`INSERT INTO approved_region_scopes VALUES (?, 'approved', 1, 1)`, fmt.Sprintf("#full%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	w := postScope(router, "approve", "#candidate")
	if w.Code != http.StatusConflict {
		t.Fatalf("approve at cap = %d %s, want 409", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), fmt.Sprint(regions.MaxApprovedScopes)) {
		t.Errorf("409 body should name the cap: %s", w.Body)
	}
	if n := queuedScopeRequests(t, db); n != 0 {
		t.Fatalf("%d request(s) queued at cap", n)
	}
	list := httptest.NewRequest("GET", "/api/admin/region-scopes", nil)
	list.Header.Set("X-API-Key", strongTestKey)
	lw := httptest.NewRecorder()
	router.ServeHTTP(lw, list)
	if !strings.Contains(lw.Body.String(), fmt.Sprintf(`"limit":%d`, regions.MaxApprovedScopes)) {
		t.Errorf("decision list does not report the cap: %.200s", lw.Body)
	}
	// Rejecting is still possible at the cap: it activates nothing.
	if w := postScope(router, "reject", "#candidate"); w.Code != http.StatusAccepted {
		t.Fatalf("reject at cap = %d %s", w.Code, w.Body)
	}
}

// Review of #355, smaller point: configured names are the baseline. One
// cannot be approved (it is not an unknown scope), and revoking an approval
// of a name that is also configured leaves it known.
func TestRegionScopeConfiguredNamePrecedence(t *testing.T) {
	router, db := scopeReviewServer(t, &Config{HashRegions: []string{"#cfg"}}, "#cfg,#both")
	if w := postScope(router, "approve", "#cfg"); w.Code != http.StatusConflict {
		t.Fatalf("approving a configured scope = %d %s, want 409", w.Code, w.Body)
	}
	// #both was approved and has since been added to config.json as well.
	if _, err := db.conn.Exec(`INSERT INTO approved_region_scopes VALUES ('#both', 'revoked', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest("GET", "/api/observers/neighbors", nil))
	if !strings.Contains(get.Body.String(), `"scope":"#both"`) {
		t.Fatalf("fixture: #both should be unknown before it is configured: %s", get.Body)
	}
	router2, _ := scopeReviewServerWithDB(t, db, &Config{HashRegions: []string{"#cfg", "#both"}})
	get = httptest.NewRecorder()
	router2.ServeHTTP(get, httptest.NewRequest("GET", "/api/observers/neighbors", nil))
	if strings.Contains(get.Body.String(), `"scope":"#both"`) || strings.Contains(get.Body.String(), `"scope":"#cfg"`) {
		t.Fatalf("a configured scope is listed as unknown after revocation: %s", get.Body)
	}
}

func scopeReviewServerWithDB(t *testing.T, db *DB, cfg *Config) (*mux.Router, *DB) {
	t.Helper()
	cfg.APIKey = strongTestKey
	router := mux.NewRouter()
	NewServer(db, cfg, NewHub()).RegisterRoutes(router)
	return router, db
}

// Review of #355, gap 2 and 3: the API reference states that decisions apply
// to packets received afterwards, that stored packets keep their scope, and
// the approval cap.
func TestRegionScopeOpenAPIDocumentsHistoryAndCap(t *testing.T) {
	d := routeDescriptions()
	for _, route := range []string{"POST /api/admin/region-scopes/approve", "POST /api/admin/region-scopes/revoke"} {
		desc := d[route].Description
		if !strings.Contains(desc, "received afterwards") || !strings.Contains(desc, "Stored packets keep") {
			t.Errorf("%s does not document future-only matching: %q", route, desc)
		}
	}
	if desc := d["POST /api/admin/region-scopes/approve"].Description; !strings.Contains(desc, fmt.Sprint(regions.MaxApprovedScopes)) {
		t.Errorf("approve does not document the cap: %q", desc)
	}
}
