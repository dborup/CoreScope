package main

import (
	"strings"
	"testing"
	"time"
)

// Tests for the #265 client-RX source allowlist: when clientRxCoverage.sources
// is set and non-empty, meshcore/client/... is only handled for messages that
// arrived on a listed mqttSources[].name. All ingest assertions drive the real
// handleMessage path with a named source.

// clientObserverCount counts the mobile-client name rows the coverage path
// writes (handleClientPacket upserts one per message carrying an "origin").
func clientObserverCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM client_observers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// dispatchFrom drives handleMessage with a named MQTT source and returns the
// row-count snapshots either side of it, so a drop can be asserted across EVERY
// table: no client_receptions, no client_observers, and no observer row from a
// fall-through to the observer path.
func dispatchFrom(t *testing.T, s *Store, cfg *Config, sourceName string, m *mockMessage) (before, after map[string]int) {
	t.Helper()
	src := MQTTSource{Name: sourceName, Broker: "tcp://broker.invalid:1883"}
	tag := sourceName
	if tag == "" {
		tag = src.Broker
	}
	before = dataTableCounts(t, s)
	handleMessage(s, tag, src, m, nil, nil, cfg)
	after = dataTableCounts(t, s)
	return before, after
}

// coverageCfgWithSources builds an enabled coverage config with the given
// source allowlist (no arguments ⇒ no allowlist).
func coverageCfgWithSources(sources ...string) *Config {
	return &Config{ClientRxCoverage: &ClientRxCoverageConfig{Enabled: true, Sources: sources}}
}

// TestClientRxSourceAllowedWithoutList pins the upstream-compatible default:
// an absent, empty or blank-only list accepts every source.
func TestClientRxSourceAllowedWithoutList(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
	}{
		{"nil clientRxCoverage", &Config{}},
		{"absent sources", coverageCfgWithSources()},
		{"blank-only sources", coverageCfgWithSources(" ", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, src := range []string{"device-auth", "legacy", ""} {
				if !tc.cfg.ClientRxSourceAllowed(src) {
					t.Errorf("source %q must be allowed when no allowlist is configured", src)
				}
			}
		})
	}
}

// TestClientRxSourceAllowedWithList covers the matching rule: listed names
// (including case and whitespace variants) pass, everything else is rejected —
// an unnamed source included, since it can never appear on the list.
func TestClientRxSourceAllowedWithList(t *testing.T) {
	cfg := coverageCfgWithSources("device-auth", " Secondary ")
	for _, name := range []string{"device-auth", "DEVICE-AUTH", " device-auth ", "secondary", "Secondary"} {
		if !cfg.ClientRxSourceAllowed(name) {
			t.Errorf("source %q must be allowed", name)
		}
	}
	for _, name := range []string{"legacy", "device-auth2", "device", "", " "} {
		if cfg.ClientRxSourceAllowed(name) {
			t.Errorf("source %q must be rejected", name)
		}
	}
}

// TestClientRxCoverageListedSourceIngests: the packet that is dropped from an
// unlisted source (next test) is ingested when the source is on the allowlist.
func TestClientRxCoverageListedSourceIngests(t *testing.T) {
	store := newTestStore(t)
	cfg := coverageCfgWithSources("device-auth", "other")

	dispatchFrom(t, store, cfg, "device-auth", clientCoverageMsg())

	if n := clientReceptionCount(t, store); n != 1 {
		t.Fatalf("listed source: expected 1 client_receptions row, got %d", n)
	}
	if n := clientObserverCount(t, store); n != 1 {
		t.Fatalf("listed source: expected 1 client_observers row, got %d", n)
	}
}

// TestClientRxCoverageUnlistedSourceDropsEverything is the mutant killer for the
// source check: remove the check in handleMessage and these counts change.
// Nothing may be written anywhere — not client_receptions, not client_observers,
// and no observer row (the client namespace still returns).
func TestClientRxCoverageUnlistedSourceDropsEverything(t *testing.T) {
	store := newTestStore(t)
	cfg := coverageCfgWithSources("device-auth")

	before, after := dispatchFrom(t, store, cfg, "legacy", clientCoverageMsg())

	assertOnlyDeltas(t, before, after, nil)
	if n := clientReceptionCount(t, store); n != 0 {
		t.Fatalf("unlisted source: expected 0 client_receptions rows, got %d", n)
	}
	if n := clientObserverCount(t, store); n != 0 {
		t.Fatalf("unlisted source: expected 0 client_observers rows, got %d", n)
	}
}

// TestClientRxCoverageUnnamedSourceDropped: a source with no name cannot be on
// the allowlist, so with one configured it must not contribute coverage.
func TestClientRxCoverageUnnamedSourceDropped(t *testing.T) {
	store := newTestStore(t)
	cfg := coverageCfgWithSources("device-auth")

	before, after := dispatchFrom(t, store, cfg, "", clientCoverageMsg())

	assertOnlyDeltas(t, before, after, nil)
}

// TestClientRxCoverageNoAllowlistUnchanged: without sources, coverage is
// accepted from an arbitrary source name, exactly as before #265.
func TestClientRxCoverageNoAllowlistUnchanged(t *testing.T) {
	store := newTestStore(t)
	cfg := coverageCfgWithSources() // enabled, no allowlist

	dispatchFrom(t, store, cfg, "any-broker", clientCoverageMsg())

	if n := clientReceptionCount(t, store); n != 1 {
		t.Fatalf("no allowlist: expected 1 client_receptions row, got %d", n)
	}
}

// TestClientRxCoverageBlacklistBeatsAllowlist: the blacklist rule holds whatever
// the source — a blacklisted companion on a LISTED source is still dropped.
func TestClientRxCoverageBlacklistBeatsAllowlist(t *testing.T) {
	store := newTestStore(t)
	cfg := coverageCfgWithSources("device-auth")
	cfg.ObserverBlacklist = []string{testCompanionPK}

	before, after := dispatchFrom(t, store, cfg, "device-auth", clientCoverageMsg())

	assertOnlyDeltas(t, before, after, nil)
}

// TestClientRxCoverageUnlistedSourceOtherSubtopicDropped: a non-"packets" client
// sub-topic from an unlisted source must also write nothing — the "client
// namespace always returns" rule applies whatever the source.
func TestClientRxCoverageUnlistedSourceOtherSubtopicDropped(t *testing.T) {
	store := newTestStore(t)
	cfg := coverageCfgWithSources("device-auth")
	msg := &mockMessage{
		topic:   "meshcore/client/" + testCompanionPK + "/status",
		payload: []byte(`{"origin":"MyMob","noise_floor":-100}`),
	}

	before, after := dispatchFrom(t, store, cfg, "legacy", msg)

	assertOnlyDeltas(t, before, after, nil)
}

// clientRxDropLines returns the per-source drop warnings in captured log output.
func clientRxDropLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "not in clientRxCoverage.sources") {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestClientRxSourceDropLogThrottled: a burst of drops inside the throttle
// window produces one line, not one per message; the window reopens afterwards.
func TestClientRxSourceDropLogThrottled(t *testing.T) {
	cfg := coverageCfgWithSources("device-auth")
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	out := captureLog(t, func() {
		for i := 0; i < 500; i++ {
			cfg.warnClientRxSourceDrop("legacy", "legacy", t0.Add(time.Duration(i)*time.Millisecond))
		}
	})
	if n := len(clientRxDropLines(out)); n != 1 {
		t.Fatalf("burst inside the window: got %d lines, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, `"legacy"`) {
		t.Fatalf("the warning must name the rejected source:\n%s", out)
	}

	// A second source gets its own first line; the window is per source.
	out = captureLog(t, func() {
		cfg.warnClientRxSourceDrop("other", "other", t0.Add(time.Second))
	})
	if n := len(clientRxDropLines(out)); n != 1 {
		t.Fatalf("second source: got %d lines, want 1:\n%s", n, out)
	}

	// Past the interval the same source may warn again.
	out = captureLog(t, func() {
		cfg.warnClientRxSourceDrop("legacy", "legacy", t0.Add(clientRxSourceWarnInterval))
	})
	if n := len(clientRxDropLines(out)); n != 1 {
		t.Fatalf("after the interval: got %d lines, want 1:\n%s", n, out)
	}
}

// TestClientRxSourceDropLogCapped: the number of lines one source may ever emit
// is bounded, so a permanently misconfigured deployment cannot fill the log.
func TestClientRxSourceDropLogCapped(t *testing.T) {
	cfg := coverageCfgWithSources("device-auth")
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	out := captureLog(t, func() {
		// Well past the cap, every call a full interval apart.
		for i := 0; i < clientRxSourceWarnMax*5; i++ {
			cfg.warnClientRxSourceDrop("legacy", "legacy", t0.Add(time.Duration(i)*clientRxSourceWarnInterval))
		}
	})
	lines := clientRxDropLines(out)
	if len(lines) != clientRxSourceWarnMax {
		t.Fatalf("cap: got %d lines, want %d:\n%s", len(lines), clientRxSourceWarnMax, out)
	}
	if !strings.Contains(lines[len(lines)-1], "line cap reached") {
		t.Fatalf("the last allowed line must say the cap was reached:\n%s", lines[len(lines)-1])
	}
}

// TestCheckClientRxSourcesUnknownName: a name matching no configured source is
// reported once at startup; a configured name (any case) is not.
func TestCheckClientRxSourcesUnknownName(t *testing.T) {
	sources := []MQTTSource{{Name: "device-auth"}, {Name: "Legacy"}}

	cfg := coverageCfgWithSources("device-auth", "legacy", "typo-broker")
	var unknown []string
	out := captureLog(t, func() { unknown = checkClientRxSources(cfg, sources) })
	if len(unknown) != 1 || unknown[0] != "typo-broker" {
		t.Fatalf("unknown names = %v, want [typo-broker]", unknown)
	}
	if n := strings.Count(out, "WARNING"); n != 1 {
		t.Fatalf("expected exactly one warning line, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "typo-broker") {
		t.Fatalf("the warning must name the unknown entry:\n%s", out)
	}
}

// TestCheckClientRxSourcesSilentWithoutAllowlist: no allowlist ⇒ nothing to
// validate and nothing logged at startup.
func TestCheckClientRxSourcesSilentWithoutAllowlist(t *testing.T) {
	sources := []MQTTSource{{Name: "device-auth"}}
	for _, cfg := range []*Config{{}, coverageCfgWithSources(), coverageCfgWithSources(" ")} {
		var unknown []string
		out := captureLog(t, func() { unknown = checkClientRxSources(cfg, sources) })
		if len(unknown) != 0 {
			t.Fatalf("no allowlist: unknown = %v, want none", unknown)
		}
		if strings.TrimSpace(out) != "" {
			t.Fatalf("no allowlist: expected no log output, got:\n%s", out)
		}
	}
}
