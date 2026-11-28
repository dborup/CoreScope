package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Follow-ups to the #265 client-RX source allowlist (#278).

// TestLoadConfigClientRxCoverageSources pins the documented JSON key
// clientRxCoverage.sources through the real LoadConfig. Every other allowlist
// test builds ClientRxCoverageConfig directly, so a typo in the struct tag
// would leave them green while the key silently did nothing — and an ignored
// allowlist fails open (every source accepted).
func TestLoadConfigClientRxCoverageSources(t *testing.T) {
	t.Setenv("MQTT_BROKER", "")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
		"mqttSources": [
			{"name": "a", "broker": "tcp://a.invalid:1883", "topics": ["meshcore/#"]},
			{"name": "b", "broker": "tcp://b.invalid:1883", "topics": ["meshcore/#"]}
		],
		"clientRxCoverage": { "enabled": true, "sources": ["a"] }
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientRxCoverage == nil || !cfg.ClientRxCoverageEnabled() {
		t.Fatalf("clientRxCoverage.enabled not loaded: %+v", cfg.ClientRxCoverage)
	}
	if got := cfg.ClientRxCoverage.Sources; len(got) != 1 || got[0] != "a" {
		t.Fatalf("clientRxCoverage.sources = %q, want [a]", got)
	}
	if !cfg.ClientRxSourceAllowed("a") {
		t.Error("listed source a must be allowed")
	}
	if cfg.ClientRxSourceAllowed("b") {
		t.Error("unlisted source b must be rejected once sources is loaded")
	}
}

// TestCheckClientRxSourcesDuplicateName: matching is case-insensitive and
// mqttSources[].name is not required to be unique, so one allowlisted name can
// admit several brokers. That widens a trust boundary without the operator
// noticing, so startup warns once per such name and lists the matching sources.
func TestCheckClientRxSourcesDuplicateName(t *testing.T) {
	sources := []MQTTSource{{Name: "auth"}, {Name: " Auth "}, {Name: "legacy"}, {Name: "LEGACY"}}

	cfg := coverageCfgWithSources("AUTH")
	var unknown []string
	out := captureLog(t, func() { unknown = checkClientRxSources(cfg, sources) })

	if len(unknown) != 0 {
		t.Fatalf("a duplicated name is not unknown: unknown = %v", unknown)
	}
	var warn []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WARNING") {
			warn = append(warn, l)
		}
	}
	if len(warn) != 1 {
		t.Fatalf("expected exactly one warning line, got %d:\n%s", len(warn), out)
	}
	for _, want := range []string{"AUTH", `"auth"`, `" Auth "`, "2"} {
		if !strings.Contains(warn[0], want) {
			t.Errorf("warning must mention %s:\n%s", want, warn[0])
		}
	}
	if strings.Contains(strings.ToLower(warn[0]), "legacy") {
		t.Errorf("legacy is duplicated but not allowlisted, it must not be reported:\n%s", warn[0])
	}
}

// TestCheckClientRxSourcesUniqueNamesNoDuplicateWarning: one match per
// allowlisted name ⇒ no warning at all.
func TestCheckClientRxSourcesUniqueNamesNoDuplicateWarning(t *testing.T) {
	sources := []MQTTSource{{Name: "auth"}, {Name: "legacy"}}
	out := captureLog(t, func() { checkClientRxSources(coverageCfgWithSources("Auth", "legacy"), sources) })
	if strings.Contains(out, "WARNING") {
		t.Fatalf("unique names must not warn:\n%s", out)
	}
}

// TestClientRxCoverageDocListsDropLogLimits: AGENTS.md rule 8 — the hardcoded
// drop-warning interval and line cap are listed under "Configurable values
// (future customizer)", with their current values taken from the constants so
// the docs cannot drift from the code.
func TestClientRxCoverageDocListsDropLogLimits(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "client-rx-coverage.md"))
	if err != nil {
		t.Fatal(err)
	}
	const heading = "## Configurable values (future customizer)"
	doc := string(data)
	i := strings.Index(doc, heading)
	if i < 0 {
		t.Fatalf("docs/client-rx-coverage.md has no %q section", heading)
	}
	section := doc[i+len(heading):]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}
	section = strings.Join(strings.Fields(section), " ") // tolerate line wrapping

	for _, want := range []string{
		fmt.Sprintf("`clientRxSourceWarnInterval`, %d minutes", int(clientRxSourceWarnInterval.Minutes())),
		fmt.Sprintf("`clientRxSourceWarnMax`, %d lines", clientRxSourceWarnMax),
	} {
		if !strings.Contains(section, want) {
			t.Errorf("configurable-values section must list %q; got:\n%s", want, section)
		}
	}
}
