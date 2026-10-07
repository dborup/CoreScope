package main

import (
	"strconv"
	"strings"
	"testing"
)

// Follow-ups to the #290 client-RX source warnings (#302): quote every
// config-derived value, pin the ambiguous-name message shape, keep the
// warnings consistent with the state line while coverage is disabled, and
// collapse a duplicated allowlist entry before reporting it.

// clientRxLogRecords splits captured output into log records. captureLog sets
// the log flags to 0, so one log.Printf is exactly one line — unless a value
// interpolated into it carries a newline, which is the whole point of N1.
func clientRxLogRecords(out string) []string {
	var recs []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			recs = append(recs, l)
		}
	}
	return recs
}

func clientRxWarnings(out string) []string {
	var warn []string
	for _, l := range clientRxLogRecords(out) {
		if strings.Contains(l, "WARNING") {
			warn = append(warn, l)
		}
	}
	return warn
}

// TestCheckClientRxSourcesQuotesConfigValues (N1): the startup lines
// interpolate operator-supplied config values, so an entry with an embedded
// newline could split a warning and forge an extra "[client-rx] …" line. Every
// config-derived value must go through %q, which renders the newline as \n and
// keeps one log.Printf to one line.
func TestCheckClientRxSourcesQuotesConfigValues(t *testing.T) {
	const forged = "evil\n[client-rx] WARNING: forged line injected from config"

	t.Run("state line and unknown-name warning", func(t *testing.T) {
		cfg := coverageCfgWithSources(forged)
		out := captureLog(t, func() { checkClientRxSources(cfg, []MQTTSource{{Name: "auth"}}) })

		// The state line plus the unknown-name warning: two records, no more.
		if recs := clientRxLogRecords(out); len(recs) != 2 {
			t.Errorf("expected 2 log records (state + unknown warning), got %d:\n%s", len(recs), out)
		}
		if strings.Contains(out, forged) {
			t.Errorf("the raw config value reached the log unquoted:\n%s", out)
		}
		if n := strings.Count(out, strconv.Quote(forged)); n != 2 {
			t.Errorf("expected the %%q-quoted value in both records, found %d:\n%s", n, out)
		}
	})

	t.Run("ambiguous-name warning", func(t *testing.T) {
		// Two sources carrying the same odd name, so the entry is ambiguous and
		// its own spelling is interpolated into the warning.
		cfg := coverageCfgWithSources(forged)
		out := captureLog(t, func() {
			checkClientRxSources(cfg, []MQTTSource{{Name: forged}, {Name: forged}})
		})

		if recs := clientRxLogRecords(out); len(recs) != 2 {
			t.Errorf("expected 2 log records (state + ambiguous warning), got %d:\n%s", len(recs), out)
		}
		if strings.Contains(out, forged) {
			t.Errorf("the raw config value reached the log unquoted:\n%s", out)
		}
	})
}

// TestCheckClientRxSourcesAmbiguousMessageShape (N2): #290 left two mutants
// alive in this message — the "; " clause separator and the match count, which
// no test distinguished from the literal 2. One entry matching three sources
// and a second matching two pin both.
func TestCheckClientRxSourcesAmbiguousMessageShape(t *testing.T) {
	sources := []MQTTSource{
		{Name: "auth"}, {Name: "Auth"}, {Name: " AUTH "},
		{Name: "legacy"}, {Name: "LEGACY"},
	}
	cfg := coverageCfgWithSources("auth", "legacy")

	out := captureLog(t, func() { checkClientRxSources(cfg, sources) })
	warn := clientRxWarnings(out)
	if len(warn) != 1 {
		t.Fatalf("expected exactly one warning line, got %d:\n%s", len(warn), out)
	}

	const wantClauses = `"auth" matches 3 sources ("auth", "Auth", " AUTH "); ` +
		`"legacy" matches 2 sources ("legacy", "LEGACY")`
	if !strings.Contains(warn[0], wantClauses) {
		t.Errorf("ambiguous warning must read\n\t%s\ngot\n\t%s", wantClauses, warn[0])
	}
}

// TestCheckClientRxSourcesDisabledWordingAgrees (N3): with
// clientRxCoverage.enabled false the state line says no coverage is ingested at
// all, so the warnings must not claim in the present tense that coverage is (or
// will never be) accepted. They speak in the conditional instead.
func TestCheckClientRxSourcesDisabledWordingAgrees(t *testing.T) {
	// One ambiguous entry and one unknown entry, so both warnings fire.
	sources := []MQTTSource{{Name: "auth"}, {Name: "AUTH"}}

	t.Run("disabled", func(t *testing.T) {
		cfg := &Config{ClientRxCoverage: &ClientRxCoverageConfig{
			Enabled: false,
			Sources: []string{"auth", "typo-broker"},
		}}
		out := captureLog(t, func() { checkClientRxSources(cfg, sources) })

		if !strings.Contains(out, "enabled is false") {
			t.Fatalf("the state line must flag the disabled feature:\n%s", out)
		}
		if len(clientRxWarnings(out)) != 2 {
			t.Fatalf("expected both warnings, got:\n%s", out)
		}
		for _, want := range []string{
			"coverage would be accepted from every one of them",
			"coverage would never be accepted for those names",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("disabled coverage must be reported as %q:\n%s", want, out)
			}
		}
		for _, bad := range []string{
			"coverage is accepted from every one of them",
			"coverage will never be accepted for those names",
		} {
			if strings.Contains(out, bad) {
				t.Errorf("the state line says no coverage is ingested, so %q contradicts it:\n%s", bad, out)
			}
		}
	})

	t.Run("enabled", func(t *testing.T) {
		cfg := coverageCfgWithSources("auth", "typo-broker")
		out := captureLog(t, func() { checkClientRxSources(cfg, sources) })

		if strings.Contains(out, "enabled is false") {
			t.Fatalf("coverage is enabled here:\n%s", out)
		}
		for _, want := range []string{
			"coverage is accepted from every one of them",
			"coverage will never be accepted for those names",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("enabled coverage must be reported as %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "would") {
			t.Errorf("the conditional mood belongs to disabled coverage only:\n%s", out)
		}
	})
}

// TestCheckClientRxSourcesDeduplicatesAllowlist (N4): matching is
// case-insensitive, so ["auth","AUTH"] is one gate written twice — a plausible
// copy/paste. Reporting each spelling repeated the whole clause. The allowlist
// is collapsed case-insensitively before reporting, keeping the first spelling.
func TestCheckClientRxSourcesDeduplicatesAllowlist(t *testing.T) {
	t.Run("ambiguous clause once", func(t *testing.T) {
		cfg := coverageCfgWithSources("auth", "AUTH", " Auth ")
		out := captureLog(t, func() {
			checkClientRxSources(cfg, []MQTTSource{{Name: "auth"}, {Name: "Auth"}})
		})

		warn := clientRxWarnings(out)
		if len(warn) != 1 {
			t.Fatalf("expected exactly one warning line, got %d:\n%s", len(warn), out)
		}
		if n := strings.Count(warn[0], " matches "); n != 1 {
			t.Errorf("the clause must appear once, got %d:\n%s", n, warn[0])
		}
		if strings.Contains(warn[0], `"AUTH"`) {
			t.Errorf("only the first spelling is reported:\n%s", warn[0])
		}
		// The state line counts distinct names, not config entries.
		if !strings.Contains(out, `restricted to 1 MQTT source(s): "auth"`) {
			t.Errorf("state line must report one distinct allowlisted name:\n%s", out)
		}
	})

	t.Run("unknown name once", func(t *testing.T) {
		cfg := coverageCfgWithSources("typo-broker", "TYPO-BROKER")
		var unknown []string
		out := captureLog(t, func() {
			unknown = checkClientRxSources(cfg, []MQTTSource{{Name: "auth"}})
		})

		if len(unknown) != 1 || unknown[0] != "typo-broker" {
			t.Fatalf("unknown names = %q, want [typo-broker]", unknown)
		}
		warn := clientRxWarnings(out)
		if len(warn) != 1 {
			t.Fatalf("expected exactly one warning line, got %d:\n%s", len(warn), out)
		}
		if !strings.Contains(warn[0], `1 clientRxCoverage.sources name(s)`) {
			t.Errorf("the duplicate must not be counted twice:\n%s", warn[0])
		}
	})
}
