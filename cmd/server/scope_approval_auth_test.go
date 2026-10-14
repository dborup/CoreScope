package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

// #355 AC2: a non-admin cannot approve, reject or revoke, and cannot read
// the decision list, the audit or a request's status. Every route is tried
// without a key and with a wrong key, and nothing may be queued. The same
// requests with the admin key succeed, so a route that lost requireAPIKey
// would answer 2xx here instead of 401/403.
func TestRegionScopeAdminRoutesRefuseNonAdmin(t *testing.T) {
	router, db := scopeReviewServer(t, &Config{}, "#candidate")
	if _, err := db.conn.Exec(`INSERT INTO approved_region_scopes VALUES ('#approved', 'approved', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`CREATE TABLE region_scope_audit (request_id TEXT PRIMARY KEY, name TEXT, operation TEXT, previous_status TEXT, new_status TEXT, decided_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	// A real queued request, so the status route has something to reveal.
	queue := channelregistry.NewQueue(filepath.Join(filepath.Dir(db.path), "region-scope-requests"))
	existing := channelregistry.Command{RequestID: channelregistry.NewID(), Op: channelregistry.OpScopeApprove, Name: "#candidate", CreatedAt: time.Now().UnixMilli()}
	if err := queue.Enqueue(existing, 100); err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		method, path, body string
		ok                 int
	}{
		{"GET", "/api/admin/region-scopes", "", http.StatusOK},
		{"GET", "/api/admin/region-scopes/audit", "", http.StatusOK},
		{"GET", "/api/admin/region-scopes/requests/" + existing.RequestID, "", http.StatusOK},
		{"POST", "/api/admin/region-scopes/approve", `{"name":"#candidate"}`, http.StatusAccepted},
		{"POST", "/api/admin/region-scopes/reject", `{"name":"#candidate"}`, http.StatusAccepted},
		{"POST", "/api/admin/region-scopes/revoke", `{"name":"#approved"}`, http.StatusAccepted},
	}
	do := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, rt := range routes {
		for _, key := range []struct{ label, value string }{{"no key", ""}, {"wrong key", "not-the-admin-key-but-long-enough"}} {
			w := do(rt.method, rt.path, rt.body, key.value)
			if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
				t.Errorf("%s %s with %s = %d %s, want 401/403", rt.method, rt.path, key.label, w.Code, w.Body)
			}
			if n := queuedScopeRequests(t, db); n != 1 {
				t.Fatalf("%s %s with %s queued a decision: %d pending, want 1", rt.method, rt.path, key.label, n)
			}
		}
	}
	// Positive control: the admin key reaches every handler.
	for _, rt := range routes {
		if w := do(rt.method, rt.path, rt.body, strongTestKey); w.Code != rt.ok {
			t.Errorf("%s %s with admin key = %d %s, want %d", rt.method, rt.path, w.Code, w.Body, rt.ok)
		}
	}
}
