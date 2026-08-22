package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for the #7 default_scope follow-up: HTTP-boundary contract for
// default_scope / default_scope_confirmed_at on GET /api/nodes and
// GET /api/nodes/{pubkey}.
//
// setupTestServer's base test schema predates both columns, so these tests
// ALTER them onto the shared in-memory test DB and force a schema
// re-detection (same established pattern as
// configured_scope_contract_test.go / TestSchemaFlagSelfHealsAfterMigrationLandsLate)
// before seeding dedicated, full 64-char-pubkey fixtures.

const (
	defaultScopeContractNullPK      = "a100000000000000000000000000000000000000000000000000000000000001"
	defaultScopeContractInferredPK  = "a200000000000000000000000000000000000000000000000000000000000002"
	defaultScopeContractConfirmedPK = "a300000000000000000000000000000000000000000000000000000000000003"
	defaultScopeContractWildcardPK  = "a400000000000000000000000000000000000000000000000000000000000004"
)

// setupDefaultScopeContractServer builds the standard test server, adds the
// default_scope columns to its nodes table, and seeds four fixture nodes
// covering every state the API contract must expose: unknown (null), packet
// -inferred only, firmware-confirmed, and the "*" wildcard sentinel.
func setupDefaultScopeContractServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv, router := setupTestServer(t)

	if _, err := srv.db.conn.Exec(`ALTER TABLE nodes ADD COLUMN default_scope TEXT`); err != nil {
		t.Fatalf("alter nodes add default_scope: %v", err)
	}
	if _, err := srv.db.conn.Exec(`ALTER TABLE nodes ADD COLUMN default_scope_confirmed_at TEXT`); err != nil {
		t.Fatalf("alter nodes add default_scope_confirmed_at: %v", err)
	}
	srv.db.detectSchema()
	if !srv.db.hasDefaultScope() {
		t.Fatal("hasDefaultScope should be true after detectSchema() following the ALTER")
	}
	if !srv.db.hasDefaultScopeConfirmedAt() {
		t.Fatal("hasDefaultScopeConfirmedAt should be true after detectSchema() following the ALTER")
	}

	seed := func(pk, name string, scope, scopeAt interface{}) {
		if _, err := srv.db.conn.Exec(
			`INSERT INTO nodes (public_key, name, role, last_seen, first_seen, default_scope, default_scope_confirmed_at)
			 VALUES (?, ?, 'repeater', '2026-07-25T12:00:00Z', '2026-01-01T00:00:00Z', ?, ?)`,
			pk, name, scope, scopeAt); err != nil {
			t.Fatalf("seed node %s: %v", pk, err)
		}
	}
	seed(defaultScopeContractNullPK, "DefaultScopeNone", nil, nil)
	seed(defaultScopeContractInferredPK, "DefaultScopeInferred", "#eu", nil)
	seed(defaultScopeContractConfirmedPK, "DefaultScopeConfirmed", "#dk", "2026-07-29T22:40:00Z")
	seed(defaultScopeContractWildcardPK, "DefaultScopeWildcard", "*", "2026-07-29T22:40:00Z")

	return srv, router
}

// ─── GET /api/nodes ──────────────────────────────────────────────────────────

func TestGetNodes_DefaultScope_Unknown(t *testing.T) {
	_, router := setupDefaultScopeContractServer(t)
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
	n := nodeByPubkey(t, nodes, defaultScopeContractNullPK)
	sc, hasScope := n["default_scope"]
	at, hasAt := n["default_scope_confirmed_at"]
	if !hasScope || sc != nil {
		t.Errorf("default_scope = %#v (present=%v), want key present with value null", sc, hasScope)
	}
	if !hasAt || at != nil {
		t.Errorf("default_scope_confirmed_at = %#v (present=%v), want key present with value null", at, hasAt)
	}
	if pk, _ := n["public_key"].(string); pk != defaultScopeContractNullPK {
		t.Errorf("public_key = %q, want full untruncated key %q", pk, defaultScopeContractNullPK)
	}
}

func TestGetNodes_DefaultScope_Inferred(t *testing.T) {
	_, router := setupDefaultScopeContractServer(t)
	req := httptest.NewRequest("GET", "/api/nodes?limit=500", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := decodeJSONBody[map[string]interface{}](t, w)
	nodes := resp["nodes"].([]interface{})
	n := nodeByPubkey(t, nodes, defaultScopeContractInferredPK)
	sc, ok := n["default_scope"].(string)
	if !ok || sc != "#eu" {
		t.Errorf("default_scope = %#v, want \"#eu\" (string)", n["default_scope"])
	}
	if at, hasAt := n["default_scope_confirmed_at"]; !hasAt || at != nil {
		t.Errorf("default_scope_confirmed_at = %#v (present=%v), want key present with value null (packet-inferred, not confirmed)", at, hasAt)
	}
}

func TestGetNodes_DefaultScope_Confirmed(t *testing.T) {
	_, router := setupDefaultScopeContractServer(t)
	req := httptest.NewRequest("GET", "/api/nodes?limit=500", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := decodeJSONBody[map[string]interface{}](t, w)
	nodes := resp["nodes"].([]interface{})
	n := nodeByPubkey(t, nodes, defaultScopeContractConfirmedPK)
	sc, ok := n["default_scope"].(string)
	if !ok || sc != "#dk" {
		t.Errorf("default_scope = %#v, want \"#dk\" (string)", n["default_scope"])
	}
	at, ok := n["default_scope_confirmed_at"].(string)
	if !ok || at != "2026-07-29T22:40:00Z" {
		t.Errorf("default_scope_confirmed_at = %#v, want \"2026-07-29T22:40:00Z\" (string, UTC RFC3339)", n["default_scope_confirmed_at"])
	}
}

func TestGetNodes_DefaultScope_Wildcard(t *testing.T) {
	_, router := setupDefaultScopeContractServer(t)
	req := httptest.NewRequest("GET", "/api/nodes?limit=500", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := decodeJSONBody[map[string]interface{}](t, w)
	nodes := resp["nodes"].([]interface{})
	n := nodeByPubkey(t, nodes, defaultScopeContractWildcardPK)
	if sc, _ := n["default_scope"].(string); sc != "*" {
		t.Errorf("default_scope = %#v, want literal \"*\" (firmware's \"no default region\" sentinel)", n["default_scope"])
	}
	if at, ok := n["default_scope_confirmed_at"].(string); !ok || at != "2026-07-29T22:40:00Z" {
		t.Errorf("default_scope_confirmed_at = %#v, want \"2026-07-29T22:40:00Z\" (wildcard can still be confirmed)", n["default_scope_confirmed_at"])
	}
}

// ─── GET /api/nodes/{pubkey} ─────────────────────────────────────────────────

func TestGetNodeByPubkey_DefaultScope_FullKeyNoAmbiguity(t *testing.T) {
	_, router := setupDefaultScopeContractServer(t)

	cases := []struct {
		name        string
		pk          string
		wantScope   interface{}
		wantScopeAt interface{}
		isString    bool
	}{
		{"unknown", defaultScopeContractNullPK, nil, nil, false},
		{"inferred", defaultScopeContractInferredPK, "#eu", nil, true},
		{"confirmed", defaultScopeContractConfirmedPK, "#dk", "2026-07-29T22:40:00Z", true},
		{"wildcard", defaultScopeContractWildcardPK, "*", "2026-07-29T22:40:00Z", true},
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
			if tc.isString {
				sc, ok := node["default_scope"].(string)
				if !ok || sc != tc.wantScope {
					t.Errorf("default_scope = %#v, want %q (string)", node["default_scope"], tc.wantScope)
				}
			} else {
				scVal, hasScope := node["default_scope"]
				if !hasScope || scVal != nil {
					t.Errorf("default_scope = %#v (present=%v), want key present with value null", scVal, hasScope)
				}
			}
			atVal, hasAt := node["default_scope_confirmed_at"]
			if tc.wantScopeAt == nil {
				if !hasAt || atVal != nil {
					t.Errorf("default_scope_confirmed_at = %#v (present=%v), want key present with value null", atVal, hasAt)
				}
			} else {
				atStr, ok := atVal.(string)
				if !ok || atStr != tc.wantScopeAt {
					t.Errorf("default_scope_confirmed_at = %#v, want %q (string, UTC RFC3339)", atVal, tc.wantScopeAt)
				}
			}
		})
	}
}

// TestGetNodeByPubkey_DefaultScope_ResurrectionFixture is the HTTP-layer
// half of the resurrection cross-table protection proof (see
// cmd/ingestor/issue1865_test.go's
// TestUpdateNodeDefaultScopeConfirmed_ThenInferredWriteAcrossTablesIsBlocked
// for the write-side half). cmd/ingestor and cmd/server are separate Go
// modules with no shared package, so this cannot be one test spanning both
// -- instead: the ingestor-side test proves the write-path guard correctly
// leaves the active nodes row at default_scope=NULL when a sibling
// inactive_nodes row is confirmed; this test proves that when the active
// row IS left at NULL (seeded directly here, matching the state that guard
// produces), the server never exposes anything other than null for it --
// specifically, it must never read the confirmed value from the OTHER
// (inactive_nodes) table. Together the two tests cover the full chain:
// confirmed evidence in inactive_nodes -> inference never writes nodes ->
// API never shows a downgraded or borrowed value.
func TestGetNodeByPubkey_DefaultScope_ResurrectionFixture(t *testing.T) {
	srv, router := setupTestServer(t)
	pk := "a500000000000000000000000000000000000000000000000000000000005"

	// setupTestDB's shared schema predates inactive_nodes entirely; reuse
	// new_nodes_test.go's helper rather than duplicating the CREATE TABLE.
	ensureInactiveNodesTable(t, srv)

	if _, err := srv.db.conn.Exec(`ALTER TABLE nodes ADD COLUMN default_scope TEXT`); err != nil {
		t.Fatalf("alter nodes add default_scope: %v", err)
	}
	if _, err := srv.db.conn.Exec(`ALTER TABLE nodes ADD COLUMN default_scope_confirmed_at TEXT`); err != nil {
		t.Fatalf("alter nodes add default_scope_confirmed_at: %v", err)
	}
	if _, err := srv.db.conn.Exec(`ALTER TABLE inactive_nodes ADD COLUMN default_scope TEXT`); err != nil {
		t.Fatalf("alter inactive_nodes add default_scope: %v", err)
	}
	if _, err := srv.db.conn.Exec(`ALTER TABLE inactive_nodes ADD COLUMN default_scope_confirmed_at TEXT`); err != nil {
		t.Fatalf("alter inactive_nodes add default_scope_confirmed_at: %v", err)
	}
	srv.db.detectSchema()

	// The resurrected active row: fresh, unconfirmed -- the exact state the
	// ingestor-side cross-table guard leaves it in.
	if _, err := srv.db.conn.Exec(
		`INSERT INTO nodes (public_key, name, role, last_seen, first_seen, default_scope, default_scope_confirmed_at)
		 VALUES (?, 'Resurrected', 'repeater', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', NULL, NULL)`,
		pk); err != nil {
		t.Fatalf("seed active row: %v", err)
	}
	// The stale, still-confirmed sibling row a real server would never read
	// default_scope from for this pubkey.
	if _, err := srv.db.conn.Exec(
		`INSERT INTO inactive_nodes (public_key, name, role, last_seen, first_seen, default_scope, default_scope_confirmed_at)
		 VALUES (?, 'Resurrected', 'repeater', '2026-07-01T00:00:00Z', '2026-01-01T00:00:00Z', '#dk', '2026-07-29T22:40:00Z')`,
		pk); err != nil {
		t.Fatalf("seed inactive sibling row: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/nodes/"+pk, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/nodes/%s: want 200, got %d: %s", pk, w.Code, w.Body.String())
	}
	resp := decodeJSONBody[map[string]interface{}](t, w)
	node, ok := resp["node"].(map[string]interface{})
	if !ok {
		t.Fatalf("response.node is not an object: %#v", resp["node"])
	}

	scVal, hasScope := node["default_scope"]
	if !hasScope || scVal != nil {
		t.Errorf("default_scope = %#v (present=%v), want null -- must reflect the active row's own (correctly unconfirmed) state, never borrow '#dk' from inactive_nodes", scVal, hasScope)
	}
	atVal, hasAt := node["default_scope_confirmed_at"]
	if !hasAt || atVal != nil {
		t.Errorf("default_scope_confirmed_at = %#v (present=%v), want null", atVal, hasAt)
	}
}
