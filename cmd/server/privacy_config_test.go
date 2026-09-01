package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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
		Enabled:        true,
		OperatorName:   "Example Mesh Community",
		ContactEmail:   "privacy@example.org",
		RetentionText:  "Packet data is deleted after 30 days.",
		LegalBasisText: "Legitimate interest in operating the mesh.",
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
		"operatorName":   "Example Mesh Community",
		"contactEmail":   "privacy@example.org",
		"retentionText":  "Packet data is deleted after 30 days.",
		"legalBasisText": "Legitimate interest in operating the mesh.",
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
			"retentionText": "Deleted after 30 days.",
			"legalBasisText": "Legitimate interest."
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
	if cfg.Privacy.LegalBasisText != "Legitimate interest." {
		t.Errorf("LegalBasisText = %q", cfg.Privacy.LegalBasisText)
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

// ─── Required-field contract (review round: no invented legal text) ─────────

// Enabling the notice makes contactEmail, retentionText and legalBasisText
// mandatory. CoreScope cannot infer a retention period, a lawful basis or a
// working contact, and must never invent them — so an incomplete block is a
// configuration error and the notice is withheld rather than published with
// made-up content.
func TestPrivacyValidateRequiresOperatorSuppliedFields(t *testing.T) {
	full := func() *PrivacyConfig {
		return &PrivacyConfig{
			Enabled:        true,
			ContactEmail:   "privacy@example.org",
			RetentionText:  "Deleted after 30 days.",
			LegalBasisText: "Legitimate interest.",
		}
	}
	if errs := full().Validate(); len(errs) != 0 {
		t.Fatalf("a fully-populated config must validate, got %v", errs)
	}

	cases := map[string]func(*PrivacyConfig){
		"contactEmail":   func(p *PrivacyConfig) { p.ContactEmail = "" },
		"retentionText":  func(p *PrivacyConfig) { p.RetentionText = "" },
		"legalBasisText": func(p *PrivacyConfig) { p.LegalBasisText = "" },
	}
	for field, blank := range cases {
		p := full()
		blank(p)
		errs := p.Validate()
		if len(errs) == 0 {
			t.Errorf("missing %s must be a validation error", field)
			continue
		}
		var mentioned bool
		for _, e := range errs {
			if strings.Contains(e, field) {
				mentioned = true
			}
		}
		if !mentioned {
			t.Errorf("validation error for %s should name the field; got %v", field, errs)
		}
	}

	// Whitespace-only is empty.
	p := full()
	p.RetentionText = "   \t "
	if len(p.Validate()) == 0 {
		t.Error("whitespace-only retentionText must not satisfy the requirement")
	}

	// Disabled or absent is never an error — the feature is simply off.
	if errs := (&PrivacyConfig{Enabled: false}).Validate(); len(errs) != 0 {
		t.Errorf("a disabled block must not report errors, got %v", errs)
	}
	var nilCfg *PrivacyConfig
	if errs := nilCfg.Validate(); len(errs) != 0 {
		t.Errorf("an absent block must not report errors, got %v", errs)
	}
}

func TestPrivacyValidateEmailShape(t *testing.T) {
	base := func(email string) *PrivacyConfig {
		return &PrivacyConfig{
			Enabled: true, ContactEmail: email,
			RetentionText: "x", LegalBasisText: "y",
		}
	}
	good := []string{"privacy@example.org", "a.b+c@sub.example.co.uk", "x_y@example.io"}
	for _, e := range good {
		if errs := base(e).Validate(); len(errs) != 0 {
			t.Errorf("%q should be accepted, got %v", e, errs)
		}
	}
	// Conservative rejects: anything that could forge a mailto header/query,
	// carry CR/LF, or is simply not an address. This proves shape only, never
	// deliverability.
	bad := []string{
		"", "   ", "no-at-sign", "two@@example.org", "a@b", "a@example",
		"a b@example.org", "a\r\nBcc:v@example.org", "a\n@example.org",
		"a?subject=x@example.org", "a&cc=x@example.org",
		"\"quoted\"@example.org", "<a@example.org>", "Name <a@example.org>",
		"a,b@example.org", "a;b@example.org",
	}
	for _, e := range bad {
		if errs := base(e).Validate(); len(errs) == 0 {
			t.Errorf("%q should be rejected as a contactEmail", e)
		}
	}
}

// An enabled-but-invalid block must NOT reach the browser: the page would
// otherwise have to invent the missing text.
func TestConfigClientWithholdsPrivacyWhenInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *PrivacyConfig
	}{
		{"no contact", &PrivacyConfig{Enabled: true, RetentionText: "x", LegalBasisText: "y"}},
		{"no retention", &PrivacyConfig{Enabled: true, ContactEmail: "a@b.co", LegalBasisText: "y"}},
		{"no legal basis", &PrivacyConfig{Enabled: true, ContactEmail: "a@b.co", RetentionText: "x"}},
		{"malformed email", &PrivacyConfig{Enabled: true, ContactEmail: "nope", RetentionText: "x", LegalBasisText: "y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.Privacy = tc.cfg
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
				t.Error("an enabled-but-invalid privacy block must be withheld, not published")
			}
		})
	}
}

// The page names the deployment's REAL hide prefixes instead of hardcoding a
// character, so the server has to publish the active list.
func TestConfigClientExposesActiveHiddenNamePrefixes(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = &PrivacyConfig{
		Enabled: true, ContactEmail: "a@b.co",
		RetentionText: "x", LegalBasisText: "y",
	}
	srv.cfg.SetHiddenNamePrefixes([]string{"##", "  ", "zz"})

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	p, ok := body["privacy"].(map[string]interface{})
	if !ok {
		t.Fatalf("privacy missing: %+v", body["privacy"])
	}
	raw, ok := p["hiddenNamePrefixes"].([]interface{})
	if !ok {
		t.Fatalf("hiddenNamePrefixes = %T(%v), want an array", p["hiddenNamePrefixes"], p["hiddenNamePrefixes"])
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		got = append(got, v.(string))
	}
	want := []string{"##", "zz"} // whitespace-only entries are not enforced, so not advertised
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("hiddenNamePrefixes = %v, want %v (blank entries dropped)", got, want)
	}
}

// No configured prefixes => the field is omitted entirely, so the page knows
// not to promise self-service hiding.
func TestConfigClientOmitsHiddenPrefixesWhenNoneConfigured(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = &PrivacyConfig{
		Enabled: true, ContactEmail: "a@b.co",
		RetentionText: "x", LegalBasisText: "y",
	}
	srv.cfg.SetHiddenNamePrefixes([]string{"   "})

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	p := body["privacy"].(map[string]interface{})
	if v, present := p["hiddenNamePrefixes"]; present {
		t.Errorf("hiddenNamePrefixes should be omitted when none are active, got %v", v)
	}
}

// ActiveHiddenNamePrefixes must reflect what IsNameHidden actually enforces,
// including after a SIGHUP-style replacement, and must not alias live config.
func TestActiveHiddenNamePrefixesMatchesEnforcement(t *testing.T) {
	c := &Config{HiddenNamePrefixes: []string{"##", "", "  "}}
	got := c.ActiveHiddenNamePrefixes()
	if len(got) != 1 || got[0] != "##" {
		t.Fatalf("ActiveHiddenNamePrefixes = %v, want [##]", got)
	}
	if !c.IsNameHidden("##node") {
		t.Error("IsNameHidden should enforce the advertised prefix")
	}
	// Mutating the returned slice must not corrupt live config.
	got[0] = "MUTATED"
	if again := c.ActiveHiddenNamePrefixes(); again[0] != "##" {
		t.Errorf("returned slice must be a copy; live config now %v", again)
	}
	// SIGHUP replacement is reflected.
	c.SetHiddenNamePrefixes([]string{"zz"})
	if again := c.ActiveHiddenNamePrefixes(); len(again) != 1 || again[0] != "zz" {
		t.Errorf("after SetHiddenNamePrefixes = %v, want [zz]", again)
	}
	var nilCfg *Config
	if nilCfg.ActiveHiddenNamePrefixes() != nil {
		t.Error("nil config must return nil")
	}
}
