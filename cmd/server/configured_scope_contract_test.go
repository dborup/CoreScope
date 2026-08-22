package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for issue #7: HTTP-boundary contract for configured_scope /
// configured_scope_at on GET /api/nodes and GET /api/nodes/{pubkey}.
//
// setupTestServer's base test schema (setupTestDB) predates #1865 and has no
// configured_scope/configured_scope_at columns at all, so these tests ALTER
// the columns onto the shared in-memory test DB and force a schema
// re-detection (mirroring TestSchemaFlagSelfHealsAfterMigrationLandsLate's
// established pattern) before seeding dedicated, real 64-char-pubkey
// fixtures -- seedTestData's existing nodes use short, non-representative
// keys and carry no scope data at all.

const (
	scopeContractMultiPK = "e100000000000000000000000000000000000000000000000000000000000001"
	scopeContractWildPK  = "e200000000000000000000000000000000000000000000000000000000000002"
	scopeContractEmptyPK = "e300000000000000000000000000000000000000000000000000000000000003"
	scopeContractNonePK  = "e400000000000000000000000000000000000000000000000000000000000004"
)

// setupConfiguredScopeContractServer builds the standard test server, adds
// the configured_scope columns to its nodes table, and seeds four fixture
// nodes covering every state the API contract must expose.
func setupConfiguredScopeContractServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv, router := setupTestServer(t)

	if _, err := srv.db.conn.Exec(`ALTER TABLE nodes ADD COLUMN configured_scope TEXT`); err != nil {
		t.Fatalf("alter nodes add configured_scope: %v", err)
	}
	if _, err := srv.db.conn.Exec(`ALTER TABLE nodes ADD COLUMN configured_scope_at TEXT`); err != nil {
		t.Fatalf("alter nodes add configured_scope_at: %v", err)
	}
	srv.db.detectSchema()
	if !srv.db.hasConfiguredScope() {
		t.Fatal("hasConfiguredScope should be true after detectSchema() following the ALTER")
	}

	seed := func(pk, name string, scope, scopeAt interface{}) {
		if _, err := srv.db.conn.Exec(
			`INSERT INTO nodes (public_key, name, role, last_seen, first_seen, configured_scope, configured_scope_at)
			 VALUES (?, ?, 'repeater', '2026-07-25T12:00:00Z', '2026-01-01T00:00:00Z', ?, ?)`,
			pk, name, scope, scopeAt); err != nil {
			t.Fatalf("seed node %s: %v", pk, err)
		}
	}
	seed(scopeContractMultiPK, "ScopeMulti", "#dk,#eu", "2026-07-25T12:00:00Z")
	seed(scopeContractWildPK, "ScopeWildcard", "*", "2026-07-25T12:00:00Z")
	seed(scopeContractEmptyPK, "ScopeEmpty", "", "2026-07-25T12:00:00Z")
	seed(scopeContractNonePK, "ScopeNone", nil, nil)

	return srv, router
}

// decodeJSONBody decodes an httptest.ResponseRecorder body into a
// map[string]interface{} (or a slice, per T), failing the test on error.
func decodeJSONBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode JSON response: %v\nbody: %s", err, w.Body.String())
	}
	return v
}

func nodeByPubkey(t *testing.T, nodes []interface{}, pk string) map[string]interface{} {
	t.Helper()
	for _, n := range nodes {
		nm, ok := n.(map[string]interface{})
		if !ok {
			continue
		}
		if nm["public_key"] == pk {
			return nm
		}
	}
	t.Fatalf("node %s not found in response list", pk)
	return nil
}

// ─── GET /api/nodes ──────────────────────────────────────────────────────────

func TestGetNodes_ConfiguredScope_NonEmptyValue(t *testing.T) {
	_, router := setupConfiguredScopeContractServer(t)
	req := httptest.NewRequest("GET", "/api/nodes?limit=500", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/nodes: want 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeJSONBody[map[string]interface{}](t, w)
	nodes, ok := resp["nodes"].([]interface{})
	if !ok {
		t.Fatalf("response.nodes is not an array: %T", resp["nodes"])
	}
	n := nodeByPubkey(t, nodes, scopeContractMultiPK)
	sc, ok := n["configured_scope"].(string)
	if !ok || sc != "#dk,#eu" {
		t.Errorf("configured_scope = %#v, want \"#dk,#eu\" (string)", n["configured_scope"])
	}
	at, ok := n["configured_scope_at"].(string)
	if !ok || at != "2026-07-25T12:00:00Z" {
		t.Errorf("configured_scope_at = %#v, want \"2026-07-25T12:00:00Z\" (string, UTC RFC3339)", n["configured_scope_at"])
	}
	if pk, _ := n["public_key"].(string); pk != scopeContractMultiPK {
		t.Errorf("public_key = %q, want full untruncated key %q", pk, scopeContractMultiPK)
	}
}

func TestGetNodes_ConfiguredScope_Wildcard(t *testing.T) {
	_, router := setupConfiguredScopeContractServer(t)
	req := httptest.NewRequest("GET", "/api/nodes?limit=500", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := decodeJSONBody[map[string]interface{}](t, w)
	nodes := resp["nodes"].([]interface{})
	n := nodeByPubkey(t, nodes, scopeContractWildPK)
	if sc, _ := n["configured_scope"].(string); sc != "*" {
		t.Errorf("configured_scope = %#v, want literal \"*\", not rewritten to \"#*\"", n["configured_scope"])
	}
}

func TestGetNodes_ConfiguredScope_ValidEmpty(t *testing.T) {
	_, router := setupConfiguredScopeContractServer(t)
	req := httptest.NewRequest("GET", "/api/nodes?limit=500", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := decodeJSONBody[map[string]interface{}](t, w)
	nodes := resp["nodes"].([]interface{})
	n := nodeByPubkey(t, nodes, scopeContractEmptyPK)
	sc, ok := n["configured_scope"]
	if !ok {
		t.Fatal("configured_scope key missing, want present with value \"\"")
	}
	if scStr, isStr := sc.(string); !isStr || scStr != "" {
		t.Errorf("configured_scope = %#v, want \"\" (a successful empty result must not collapse to null)", sc)
	}
	at, ok := n["configured_scope_at"].(string)
	if !ok || at == "" {
		t.Errorf("configured_scope_at = %#v, want a real non-empty timestamp accompanying the confirmed-empty scope", n["configured_scope_at"])
	}
}

func TestGetNodes_ConfiguredScope_Unavailable(t *testing.T) {
	_, router := setupConfiguredScopeContractServer(t)
	req := httptest.NewRequest("GET", "/api/nodes?limit=500", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := decodeJSONBody[map[string]interface{}](t, w)
	nodes := resp["nodes"].([]interface{})
	n := nodeByPubkey(t, nodes, scopeContractNonePK)
	sc, hasScope := n["configured_scope"]
	at, hasAt := n["configured_scope_at"]
	if !hasScope || sc != nil {
		t.Errorf("configured_scope = %#v (present=%v), want key present with value null", sc, hasScope)
	}
	if !hasAt || at != nil {
		t.Errorf("configured_scope_at = %#v (present=%v), want key present with value null", at, hasAt)
	}
}

// ─── GET /api/nodes/{pubkey} ─────────────────────────────────────────────────

func TestGetNodeByPubkey_ConfiguredScope_FullKeyNoAmbiguity(t *testing.T) {
	_, router := setupConfiguredScopeContractServer(t)

	cases := []struct {
		name          string
		pk            string
		wantScope     interface{}
		wantScopeAt   interface{}
		scopeIsString bool
	}{
		{"multi", scopeContractMultiPK, "#dk,#eu", "2026-07-25T12:00:00Z", true},
		{"wildcard", scopeContractWildPK, "*", "2026-07-25T12:00:00Z", true},
		{"valid-empty", scopeContractEmptyPK, "", "2026-07-25T12:00:00Z", true},
		{"unavailable", scopeContractNonePK, nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/nodes/"+tc.pk, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/nodes/%s: want 200, got %d: %s", tc.pk, w.Code, w.Body.String())
			}
			resp := decodeJSONBody[map[string]interface{}](t, w)
			node, ok := resp["node"].(map[string]interface{})
			if !ok {
				t.Fatalf("response.node is not an object: %#v", resp["node"])
			}

			if pk, _ := node["public_key"].(string); pk != tc.pk {
				t.Errorf("public_key = %q, want the exact full 64-char key %q (no prefix/short-ID resolution should have been needed)", pk, tc.pk)
			}
			if tc.scopeIsString {
				sc, ok := node["configured_scope"].(string)
				if !ok || sc != tc.wantScope {
					t.Errorf("configured_scope = %#v, want %q (string)", node["configured_scope"], tc.wantScope)
				}
				at, ok := node["configured_scope_at"].(string)
				if !ok || at != tc.wantScopeAt {
					t.Errorf("configured_scope_at = %#v, want %q (string)", node["configured_scope_at"], tc.wantScopeAt)
				}
			} else {
				scVal, hasScope := node["configured_scope"]
				atVal, hasAt := node["configured_scope_at"]
				if !hasScope || scVal != nil {
					t.Errorf("configured_scope = %#v (present=%v), want key present with value null", scVal, hasScope)
				}
				if !hasAt || atVal != nil {
					t.Errorf("configured_scope_at = %#v (present=%v), want key present with value null", atVal, hasAt)
				}
			}
		})
	}
}
