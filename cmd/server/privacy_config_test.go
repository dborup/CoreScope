package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// The opt-in privacy-notice page (#/privacy) is driven entirely by the
// `privacy` field on /api/config/client. Contract with the frontend
// (public/roles.js + public/privacy.js): field PRESENT => feature on,
// render notice + inject nav link; field ABSENT => feature off. These
// tests pin both directions plus the disabled-but-configured case.

func TestConfigClientExposesPrivacyWhenEnabled(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = &PrivacyConfig{
		Enabled:       true,
		OperatorName:  "Example Mesh Community",
		ContactEmail:  "privacy@example.org",
		RetentionText: "Packet data is deleted after 30 days.",
	}

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	pRaw, present := body["privacy"]
	if !present {
		t.Fatal("expected privacy in /api/config/client response when enabled")
	}
	p, ok := pRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("expected privacy to be a JSON object, got %T: %+v", pRaw, pRaw)
	}
	// Explicit type assertions so a shape change (field renamed, value
	// becoming a different type) fails loudly — mirrors the geoFilter
	// contract tests in config_client_geofilter_test.go.
	if enabled, ok := p["enabled"].(bool); !ok || !enabled {
		t.Errorf("privacy[enabled] = %T(%v), want true", p["enabled"], p["enabled"])
	}
	wantStrings := map[string]string{
		"operatorName":  "Example Mesh Community",
		"contactEmail":  "privacy@example.org",
		"retentionText": "Packet data is deleted after 30 days.",
	}
	for field, want := range wantStrings {
		got, ok := p[field].(string)
		if !ok {
			t.Fatalf("privacy[%q] = %T(%v), want a string", field, p[field], p[field])
		}
		if got != want {
			t.Errorf("privacy[%q] = %q, want %q", field, got, want)
		}
	}
}

// A configured-but-disabled privacy section must behave exactly like an
// unconfigured one: the field is omitted entirely (not present-but-false),
// so the frontend's single presence check gates the whole feature.
func TestConfigClientOmitsPrivacyWhenDisabled(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = &PrivacyConfig{Enabled: false, OperatorName: "Example"}

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := body["privacy"]; present {
		t.Error("expected privacy to be omitted from /api/config/client when disabled")
	}
}

func TestConfigClientOmitsPrivacyWhenUnconfigured(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = nil

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := body["privacy"]; present {
		t.Error("expected privacy to be omitted from /api/config/client when unconfigured")
	}
}

// config.json parsing: the privacy section round-trips into Config with
// the documented json field names (see config.example.json).
func TestPrivacyConfigParsing(t *testing.T) {
	raw := `{
		"privacy": {
			"enabled": true,
			"operatorName": "Example Mesh Community",
			"contactEmail": "privacy@example.org",
			"retentionText": "Deleted after 30 days."
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Privacy == nil {
		t.Fatal("expected cfg.Privacy to be set")
	}
	if !cfg.Privacy.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.Privacy.OperatorName != "Example Mesh Community" {
		t.Errorf("OperatorName = %q", cfg.Privacy.OperatorName)
	}
	if cfg.Privacy.ContactEmail != "privacy@example.org" {
		t.Errorf("ContactEmail = %q", cfg.Privacy.ContactEmail)
	}
	if cfg.Privacy.RetentionText != "Deleted after 30 days." {
		t.Errorf("RetentionText = %q", cfg.Privacy.RetentionText)
	}
	// Omitted section must stay nil so the feature defaults off.
	var empty Config
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if empty.Privacy != nil {
		t.Error("expected nil Privacy for a config without the section")
	}
}
