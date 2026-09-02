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

// validPrivacy returns a fully-populated, publishable config. Tests blank
// ONE field at a time from this baseline, so a new required field
// automatically gains coverage in TestPrivacyEachRequiredFieldIsMandatory.
func validPrivacy() *PrivacyConfig {
	return &PrivacyConfig{
		Enabled:                     true,
		ControllerName:              "Example Mesh Community",
		ContactEmail:                "privacy@example.org",
		EffectiveDate:               "2026-09-01",
		PurposesText:                "Operating and troubleshooting the community network.",
		LegalBasisType:              "public_task",
		LegalBasisText:              "Processing is necessary for our community task.",
		RetentionText:               "Packet data is deleted after 30 days.",
		RecipientsText:              "Website and API visitors; our hosting provider.",
		DataSourcesText:             "Observer nodes, radio packets and derived measurements.",
		ThirdPartyServicesText:      "Map tiles are loaded from a third-party provider.",
		InternationalTransfersText:  "No transfers outside the EU/EEA.",
		BrowserStorageText:          "Interface settings are stored in your browser.",
		ServerLogsText:              "Our proxy keeps access logs for 14 days.",
		AutomatedDecisionMakingText: "No automated decision-making is used.",
	}
}

func TestConfigClientExposesPrivacyWhenEnabled(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = validPrivacy()

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
		"controllerName":              "Example Mesh Community",
		"contactEmail":                "privacy@example.org",
		"effectiveDate":               "2026-09-01",
		"legalBasisType":              "public_task",
		"retentionText":               "Packet data is deleted after 30 days.",
		"automatedDecisionMakingText": "No automated decision-making is used.",
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
	srv.cfg.Privacy = &PrivacyConfig{Enabled: false, ControllerName: "Example"}

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
			"controllerName": "Example Mesh Community",
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
	if cfg.Privacy.ControllerName != "Example Mesh Community" {
		t.Errorf("ControllerName = %q", cfg.Privacy.ControllerName)
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
// Every REQUIRED field, blanked one at a time from a known-good baseline.
// Driven by a table so adding a required field to PrivacyConfig without
// adding it here shows up as a gap rather than passing silently.
func TestPrivacyEachRequiredFieldIsMandatory(t *testing.T) {
	if errs := validPrivacy().Validate(); len(errs) != 0 {
		t.Fatalf("the baseline must validate, got %v", errs)
	}
	blank := map[string]func(*PrivacyConfig){
		"controllerName":              func(p *PrivacyConfig) { p.ControllerName = "" },
		"contactEmail":                func(p *PrivacyConfig) { p.ContactEmail = "" },
		"effectiveDate":               func(p *PrivacyConfig) { p.EffectiveDate = "" },
		"purposesText":                func(p *PrivacyConfig) { p.PurposesText = "" },
		"legalBasisType":              func(p *PrivacyConfig) { p.LegalBasisType = "" },
		"legalBasisText":              func(p *PrivacyConfig) { p.LegalBasisText = "" },
		"retentionText":               func(p *PrivacyConfig) { p.RetentionText = "" },
		"recipientsText":              func(p *PrivacyConfig) { p.RecipientsText = "" },
		"dataSourcesText":             func(p *PrivacyConfig) { p.DataSourcesText = "" },
		"thirdPartyServicesText":      func(p *PrivacyConfig) { p.ThirdPartyServicesText = "" },
		"internationalTransfersText":  func(p *PrivacyConfig) { p.InternationalTransfersText = "" },
		"browserStorageText":          func(p *PrivacyConfig) { p.BrowserStorageText = "" },
		"serverLogsText":              func(p *PrivacyConfig) { p.ServerLogsText = "" },
		"automatedDecisionMakingText": func(p *PrivacyConfig) { p.AutomatedDecisionMakingText = "" },
	}
	for field, clear := range blank {
		t.Run(field, func(t *testing.T) {
			p := validPrivacy()
			clear(p)
			errs := p.Validate()
			if len(errs) == 0 {
				t.Fatalf("missing %s must be a validation error", field)
			}
			var named bool
			for _, e := range errs {
				if strings.Contains(e, field) {
					named = true
				}
			}
			if !named {
				t.Errorf("the error for %s should name the field; got %v", field, errs)
			}
		})
	}

	// Whitespace-only is empty for every text field.
	p := validPrivacy()
	p.RetentionText = "   \t "
	if len(p.Validate()) == 0 {
		t.Error("whitespace-only retentionText must not satisfy the requirement")
	}

	// controllerName specifically has NO fallback anywhere in the stack.
	p = validPrivacy()
	p.ControllerName = "   "
	if len(p.Validate()) == 0 {
		t.Error("controllerName must have no fallback — blank is a hard error")
	}

	// DPO fields stay optional.
	p = validPrivacy()
	p.DPOName, p.DPOContact = "", ""
	if errs := p.Validate(); len(errs) != 0 {
		t.Errorf("DPO fields are optional, got %v", errs)
	}

	// Disabled or absent is never an error.
	if errs := (&PrivacyConfig{Enabled: false}).Validate(); len(errs) != 0 {
		t.Errorf("a disabled block must not report errors, got %v", errs)
	}
	var nilCfg *PrivacyConfig
	if errs := nilCfg.Validate(); len(errs) != 0 {
		t.Errorf("an absent block must not report errors, got %v", errs)
	}
}

// legalBasisType is structured, not inferred from prose, and
// legitimate_interests additionally requires the specific interests.
func TestPrivacyLegalBasisTypeContract(t *testing.T) {
	for _, bt := range PrivacyLegalBasisTypes() {
		p := validPrivacy()
		p.LegalBasisType = bt
		if bt == "legitimate_interests" {
			if errs := p.Validate(); len(errs) == 0 {
				t.Error("legitimate_interests without legitimateInterestsText must fail")
			}
			p.LegitimateInterestsText = "We rely on X to keep the mesh operable; see our assessment."
		}
		if errs := p.Validate(); len(errs) != 0 {
			t.Errorf("legalBasisType %q should validate, got %v", bt, errs)
		}
	}

	// Unrecognised values are rejected rather than passed through.
	for _, bad := range []string{"legitimate interest", "LegitimateInterests", "art6f", "other"} {
		p := validPrivacy()
		p.LegalBasisType = bad
		if errs := p.Validate(); len(errs) == 0 {
			t.Errorf("legalBasisType %q should be rejected", bad)
		}
	}

	// legitimateInterestsText is NOT required for other bases.
	p := validPrivacy()
	p.LegalBasisType = "consent"
	p.LegitimateInterestsText = ""
	if errs := p.Validate(); len(errs) != 0 {
		t.Errorf("legitimateInterestsText should only be required for legitimate_interests, got %v", errs)
	}
}

func TestPrivacyValidateEmailShape(t *testing.T) {
	base := func(email string) *PrivacyConfig {
		p := validPrivacy()
		p.ContactEmail = email
		return p
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
// An enabled-but-invalid block must NOT reach the browser: the page would
// otherwise have to invent the missing text, or name no controller at all.
// One subtest per required field, so every one of them is proven to gate
// publication end-to-end, not just in Validate().
func TestConfigClientWithholdsPrivacyWhenInvalid(t *testing.T) {
	blank := map[string]func(*PrivacyConfig){
		"controllerName":              func(p *PrivacyConfig) { p.ControllerName = "" },
		"contactEmail":                func(p *PrivacyConfig) { p.ContactEmail = "" },
		"effectiveDate":               func(p *PrivacyConfig) { p.EffectiveDate = "" },
		"purposesText":                func(p *PrivacyConfig) { p.PurposesText = "" },
		"legalBasisType":              func(p *PrivacyConfig) { p.LegalBasisType = "" },
		"legalBasisText":              func(p *PrivacyConfig) { p.LegalBasisText = "" },
		"retentionText":               func(p *PrivacyConfig) { p.RetentionText = "" },
		"recipientsText":              func(p *PrivacyConfig) { p.RecipientsText = "" },
		"dataSourcesText":             func(p *PrivacyConfig) { p.DataSourcesText = "" },
		"thirdPartyServicesText":      func(p *PrivacyConfig) { p.ThirdPartyServicesText = "" },
		"internationalTransfersText":  func(p *PrivacyConfig) { p.InternationalTransfersText = "" },
		"browserStorageText":          func(p *PrivacyConfig) { p.BrowserStorageText = "" },
		"serverLogsText":              func(p *PrivacyConfig) { p.ServerLogsText = "" },
		"automatedDecisionMakingText": func(p *PrivacyConfig) { p.AutomatedDecisionMakingText = "" },
		"malformed email":             func(p *PrivacyConfig) { p.ContactEmail = "nope" },
		"legitimate_interests without interests": func(p *PrivacyConfig) {
			p.LegalBasisType = "legitimate_interests"
			p.LegitimateInterestsText = ""
		},
		"dpoName without dpoContact": func(p *PrivacyConfig) {
			p.DPOName = "Jane Doe"
			p.DPOContact = ""
		},
	}
	for name, clear := range blank {
		t.Run(name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			p := validPrivacy()
			clear(p)
			srv.cfg.Privacy = p
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

// The happy path: a complete config publishes every documented field, so the
// page and the nav link both light up.
func TestConfigClientPublishesCompletePrivacy(t *testing.T) {
	srv, router := setupTestServer(t)
	p := validPrivacy()
	p.LegalBasisType = "legitimate_interests"
	p.LegitimateInterestsText = "Keeping the community mesh operable; see our assessment."
	p.DPOName = "Jane Doe"
	p.DPOContact = "dpo@example.org"
	srv.cfg.Privacy = p

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	pc, ok := body["privacy"].(map[string]interface{})
	if !ok {
		t.Fatalf("privacy missing from a complete config: %+v", body["privacy"])
	}
	for _, f := range []string{
		"enabled", "controllerName", "contactEmail", "effectiveDate", "purposesText",
		"legalBasisType", "legalBasisText", "legitimateInterestsText", "retentionText",
		"recipientsText", "dataSourcesText", "thirdPartyServicesText",
		"internationalTransfersText", "browserStorageText", "serverLogsText",
		"automatedDecisionMakingText", "dpoName", "dpoContact",
	} {
		if _, present := pc[f]; !present {
			t.Errorf("privacy[%q] missing from the published payload", f)
		}
	}
}

// The "Your rights" section was deleted from the privacy page outright, so
// the three fields that fed it are gone from the config, the validator and
// the client DTO. These tests fail if any of them is reintroduced: the page
// has no renderer for them, so publishing them again would ship dead data to
// every browser and re-impose a required field operators cannot use.
func TestConfigClientOmitsRemovedRightsFields(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = validPrivacy()

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	pc, ok := body["privacy"].(map[string]interface{})
	if !ok {
		t.Fatalf("privacy missing from a complete config: %+v", body["privacy"])
	}
	for _, f := range []string{"rightsRequestText", "supervisoryAuthorityName", "supervisoryAuthorityUrl"} {
		if _, present := pc[f]; present {
			t.Errorf("privacy[%q] was removed with the rights section but is still published", f)
		}
	}
}

// The removed fields are no longer required, so a config that omits them
// entirely must still publish the notice.
func TestPrivacyValidatesWithoutRemovedRightsFields(t *testing.T) {
	if errs := validPrivacy().Validate(); len(errs) != 0 {
		t.Fatalf("a config without the removed rights fields must validate, got %v", errs)
	}
	for _, e := range validPrivacy().Validate() {
		if strings.Contains(e, "rightsRequestText") || strings.Contains(e, "supervisoryAuthority") {
			t.Errorf("validator still demands a removed field: %s", e)
		}
	}
}

// An existing deployment upgrades in place: its config.json still carries the
// three deleted keys. They must be ignored, not rejected, and must not reach
// the browser.
func TestPrivacyStaleRemovedKeysAreIgnored(t *testing.T) {
	raw := []byte(`{
	  "enabled": true,
	  "controllerName": "Example Mesh Community",
	  "contactEmail": "privacy@example.org",
	  "effectiveDate": "2026-09-01",
	  "purposesText": "x",
	  "legalBasisType": "public_task",
	  "legalBasisText": "x",
	  "retentionText": "x",
	  "recipientsText": "x",
	  "dataSourcesText": "x",
	  "thirdPartyServicesText": "x",
	  "internationalTransfersText": "x",
	  "browserStorageText": "x",
	  "serverLogsText": "x",
	  "automatedDecisionMakingText": "x",
	  "rightsRequestText": "STALE",
	  "supervisoryAuthorityName": "STALE",
	  "supervisoryAuthorityUrl": "https://stale.example"
	}`)
	var p PrivacyConfig
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("a config.json written before the removal must still parse: %v", err)
	}
	if errs := p.Validate(); len(errs) != 0 {
		t.Fatalf("stale keys must be ignored, not rejected, got %v", errs)
	}

	srv, router := setupTestServer(t)
	srv.cfg.Privacy = &p
	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "STALE") || strings.Contains(w.Body.String(), "stale.example") {
		t.Error("a stale removed key leaked into /api/config/client")
	}
}

// The page names the deployment's REAL hide prefixes instead of hardcoding a
// character, so the server has to publish the active list.
func TestConfigClientExposesActiveHiddenNamePrefixes(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = validPrivacy()
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
	srv.cfg.Privacy = validPrivacy()
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

// The DPO block is optional, but not divisible: a named DPO the reader cannot
// reach is an incomplete disclosure, while a contact route without a name
// still says where to write. All four combinations are pinned here, at both
// the Validate() layer and the /api/config/client publishing gate, because a
// withheld privacy block also removes the page from the navigation.
func TestPrivacyDPONameRequiresContact(t *testing.T) {
	cases := []struct {
		name        string
		dpoName     string
		dpoContact  string
		wantValid   bool
		wantName    bool // dpoName present in the published payload
		wantContact bool // dpoContact present in the published payload
	}{
		{"both blank", "", "", true, false, false},
		{"name without contact", "Jane Doe", "", false, false, false},
		{"contact without name", "", "dpo@example.org", true, false, true},
		{"both present", "Jane Doe", "dpo@example.org", true, true, true},
		// Trimming must match the rest of the contract: a whitespace-only
		// contact is blank, not a contact route.
		{"name with whitespace-only contact", "Jane Doe", "   \t  ", false, false, false},
		{"whitespace-only name is not a name", "   ", "", true, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Layer 1: Validate() itself.
			p := validPrivacy()
			p.DPOName = tc.dpoName
			p.DPOContact = tc.dpoContact
			errs := p.Validate()
			if tc.wantValid && len(errs) != 0 {
				t.Fatalf("expected a valid config, got errors: %v", errs)
			}
			if !tc.wantValid {
				if len(errs) == 0 {
					t.Fatal("expected a validation error for a half-filled DPO block, got none")
				}
				var found bool
				for _, e := range errs {
					if strings.Contains(e, "privacy.dpoContact is required") {
						found = true
					}
				}
				if !found {
					t.Errorf("expected the dpoContact error, got: %v", errs)
				}
			}

			// Layer 2: the end-to-end publishing gate. An invalid DPO block
			// must withhold the WHOLE privacy payload, not just the section.
			srv, router := setupTestServer(t)
			srv.cfg.Privacy = p
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
			raw, present := body["privacy"]
			if !tc.wantValid {
				if present {
					t.Fatal("a half-filled DPO block must withhold the entire privacy payload")
				}
				return
			}
			pc, ok := raw.(map[string]interface{})
			if !ok {
				t.Fatalf("privacy missing from a valid config: %+v", raw)
			}
			if _, got := pc["dpoName"]; got != tc.wantName {
				t.Errorf("dpoName present = %v, want %v", got, tc.wantName)
			}
			if _, got := pc["dpoContact"]; got != tc.wantContact {
				t.Errorf("dpoContact present = %v, want %v", got, tc.wantContact)
			}
		})
	}
}
