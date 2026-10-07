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
// render the notice + inject the nav link; field ABSENT => feature off.
//
// The notice itself is a FIXED document in public/privacy.js. The server
// therefore publishes a flag and nothing else: there is no operator text in
// the payload, and no config value can put wording on the page. These tests
// pin that emptiness as hard as they pin the on/off contract, because a
// field reappearing here is a field that could ship unreviewed text.

// removedPrivacyFields are the operator-text keys the privacy block used to
// carry. None may ever come back: public/privacy.js has no renderer for
// them, so publishing one again would ship dead data to every browser and
// re-impose a required field an operator cannot influence the page with.
var removedPrivacyFields = []string{
	"controllerName", "contactEmail", "effectiveDate", "purposesText",
	"legalBasisType", "legalBasisText", "legitimateInterestsText",
	"retentionText", "recipientsText", "dataSourcesText",
	"thirdPartyServicesText", "internationalTransfersText",
	"browserStorageText", "serverLogsText", "automatedDecisionMakingText",
	"rightsRequestText", "supervisoryAuthorityName", "supervisoryAuthorityUrl",
	"dpoName", "dpoContact", "hiddenNamePrefixes",
}

func privacyBlock(t *testing.T, cfg *PrivacyConfig) (map[string]interface{}, bool) {
	t.Helper()
	srv, router := setupTestServer(t)
	srv.cfg.Privacy = cfg

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
	if !present {
		return nil, false
	}
	p, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("expected privacy to be a JSON object, got %T: %+v", raw, raw)
	}
	return p, true
}

func TestConfigClientExposesPrivacyWhenEnabled(t *testing.T) {
	p, present := privacyBlock(t, &PrivacyConfig{Enabled: true})
	if !present {
		t.Fatal("expected privacy to be published when enabled")
	}
	if enabled, ok := p["enabled"].(bool); !ok || !enabled {
		t.Errorf("privacy[enabled] = %T(%v), want true", p["enabled"], p["enabled"])
	}
}

// The whole payload is one flag. Asserted by key count, not by a checklist,
// so a NEW field added later fails this too -- not just the removed ones.
func TestConfigClientPublishesOnlyTheEnabledFlag(t *testing.T) {
	p, present := privacyBlock(t, &PrivacyConfig{Enabled: true})
	if !present {
		t.Fatal("expected privacy to be published when enabled")
	}
	if len(p) != 1 {
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		t.Errorf("privacy payload must carry ONLY \"enabled\", got %d keys: %v", len(p), keys)
	}
}

func TestConfigClientOmitsAllRemovedPrivacyFields(t *testing.T) {
	p, present := privacyBlock(t, &PrivacyConfig{Enabled: true})
	if !present {
		t.Fatal("expected privacy to be published when enabled")
	}
	for _, f := range removedPrivacyFields {
		if _, found := p[f]; found {
			t.Errorf("privacy[%q] was removed with the operator-text model but is still published", f)
		}
	}
}

func TestConfigClientOmitsPrivacyWhenDisabled(t *testing.T) {
	if _, present := privacyBlock(t, &PrivacyConfig{Enabled: false}); present {
		t.Error("expected privacy to be omitted from /api/config/client when disabled")
	}
}

func TestConfigClientOmitsPrivacyWhenUnconfigured(t *testing.T) {
	if _, present := privacyBlock(t, nil); present {
		t.Error("expected privacy to be omitted from /api/config/client when unconfigured")
	}
}

// config.json parsing: the privacy section round-trips into Config with the
// documented json field name (see config.example.json).
func TestPrivacyConfigParsing(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"privacy":{"enabled":true}}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Privacy == nil {
		t.Fatal("privacy section did not parse")
	}
	if !c.Privacy.Enabled {
		t.Error("privacy.enabled did not parse as true")
	}

	// Omitted section stays nil so the feature defaults off.
	var c2 Config
	if err := json.Unmarshal([]byte(`{}`), &c2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c2.Privacy != nil {
		t.Errorf("an omitted privacy section must stay nil, got %+v", c2.Privacy)
	}
}

// A deployment upgrading in place still has the old operator-text keys in
// its config.json. They must be ignored -- not rejected, and above all never
// published, because there is no longer any renderer that would show them.
func TestPrivacyStaleConfigKeysAreIgnored(t *testing.T) {
	raw := []byte(`{
	  "enabled": true,
	  "controllerName": "STALE-controller",
	  "contactEmail": "stale@example.invalid",
	  "effectiveDate": "STALE-date",
	  "purposesText": "STALE-purposes",
	  "legalBasisType": "not_a_real_basis",
	  "legalBasisText": "STALE-basis",
	  "retentionText": "STALE-retention",
	  "recipientsText": "STALE-recipients",
	  "dataSourcesText": "STALE-sources",
	  "thirdPartyServicesText": "STALE-third-party",
	  "internationalTransfersText": "STALE-transfers",
	  "browserStorageText": "STALE-storage",
	  "serverLogsText": "STALE-logs",
	  "automatedDecisionMakingText": "STALE-automated",
	  "rightsRequestText": "STALE-rights",
	  "supervisoryAuthorityName": "STALE-authority",
	  "supervisoryAuthorityUrl": "javascript:alert(1)",
	  "dpoName": "STALE-dpo",
	  "dpoContact": "STALE-dpo-contact"
	}`)
	var p PrivacyConfig
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("a config.json written before the removal must still parse: %v", err)
	}
	if !p.Enabled {
		t.Fatal("enabled must still parse from a pre-removal config")
	}

	srv, router := setupTestServer(t)
	srv.cfg.Privacy = &p
	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("a stale privacy config must not break the endpoint, got %d", w.Code)
	}
	body := w.Body.String()
	for _, leak := range []string{"STALE-", "stale@example.invalid", "javascript:alert(1)", "not_a_real_basis"} {
		if strings.Contains(body, leak) {
			t.Errorf("a stale removed key leaked into /api/config/client: %q", leak)
		}
	}
}

// ActiveHiddenNamePrefixes must reflect what IsNameHidden actually enforces,
// including after a SIGHUP-style replacement, and must not alias live config.
// The privacy page no longer consumes it, but the invariant it pins belongs
// to node hiding, not to the notice.
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
