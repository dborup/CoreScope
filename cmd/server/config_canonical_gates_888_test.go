package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #888: the remaining opt-in gates read only their canonical,
// exact-case keys, like the #884 gates. encoding/json would take `Enabled`
// (or a block spelled `Privacy`) and let the last spelling win.

func TestRemainingGatesReadOnlyCanonicalKeys888(t *testing.T) {
	gates := []struct {
		block string
		on    func(*Config) bool
	}{
		{"privacy", func(c *Config) bool { return c.Privacy != nil && c.Privacy.Enabled }},
		{"clientRegions", func(c *Config) bool { return c.ClientRegionsEnabled() }},
		{"clientRxCoverage", func(c *Config) bool { return c.ClientRxCoverageEnabled() }},
		{"channelProposals", func(c *Config) bool { return c.ChannelProposals.SubmissionsRequested() }},
	}
	for _, g := range gates {
		cases := []struct {
			name, cfg string
			want      bool
			warnings  int
		}{
			{"canonical on", `{"` + g.block + `":{"enabled":true}}`, true, 0},
			{"Enabled after canonical false", `{"` + g.block + `":{"enabled":false,"Enabled":true}}`, false, 1},
			{"ENABLED before canonical false", `{"` + g.block + `":{"ENABLED":true,"enabled":false}}`, false, 1},
			{"Enabled alone", `{"` + g.block + `":{"Enabled":true}}`, false, 1},
			{"block spelled in another case", `{"` + capitalize(g.block) + `":{"enabled":true}}`, false, 1},
		}
		for _, tc := range cases {
			t.Run(g.block+"/"+tc.name, func(t *testing.T) {
				cfg, logged := loadConfigLogged(t, tc.cfg)
				if got := g.on(cfg); got != tc.want {
					t.Fatalf("%s on = %v, want %v", g.block, got, tc.want)
				}
				if n := ignoredWarnings(logged); n != tc.warnings {
					t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
				}
			})
		}
	}
}

func capitalize(s string) string { return string(s[0]-32) + s[1:] }

func TestCompressionGatesReadOnlyCanonicalKeys888(t *testing.T) {
	cases := []struct {
		name, cfg      string
		wantGZ, wantWS bool
		warnings       int
	}{
		{"canonical on", `{"compression":{"gzip":true,"websocket":true}}`, true, true, 0},
		{"GZip after canonical false", `{"compression":{"gzip":false,"GZip":true,"websocket":false,"WebSocket":true}}`, false, false, 2},
		{"alone", `{"compression":{"GZIP":true,"Websocket":true}}`, false, false, 2},
		{"block spelled in another case", `{"Compression":{"gzip":true}}`, false, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logged := loadConfigLogged(t, tc.cfg)
			if cfg.GZipEnabled() != tc.wantGZ || cfg.WSCompressionEnabled() != tc.wantWS {
				t.Fatalf("gzip=%v ws=%v, want %v %v", cfg.GZipEnabled(), cfg.WSCompressionEnabled(), tc.wantGZ, tc.wantWS)
			}
			if n := ignoredWarnings(logged); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
			}
		})
	}
}

// estimatedPositions is on by default, so a stray `Enabled: true` must not
// override a canonical false, and a non-canonical block must not turn it
// off or on.
func TestEstimatedPositionsReadsOnlyCanonicalKeys888(t *testing.T) {
	cases := []struct {
		name, cfg string
		want      bool
		warnings  int
	}{
		{"absent", `{}`, true, 0},
		{"canonical true", `{"estimatedPositions":{"enabled":true}}`, true, 0},
		{"canonical false", `{"estimatedPositions":{"enabled":false}}`, false, 0},
		{"canonical false plus Enabled true", `{"estimatedPositions":{"enabled":false,"Enabled":true}}`, false, 1},
		{"Enabled true before canonical false", `{"estimatedPositions":{"Enabled":true,"enabled":false}}`, false, 1},
		{"Enabled false alone keeps the default", `{"estimatedPositions":{"Enabled":false}}`, true, 1},
		{"block spelled in another case keeps the default", `{"EstimatedPositions":{"enabled":false}}`, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logged := loadConfigLogged(t, tc.cfg)
			if got := cfg.estimatedPositionsEnabled(); got != tc.want {
				t.Fatalf("estimatedPositionsEnabled = %v, want %v", got, tc.want)
			}
			if n := ignoredWarnings(logged); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
			}
		})
	}
}

// config.example.json uses canonical keys only: it loads with no warning and
// the same gate values as before this change.
func TestExampleConfigLoadsIdentically888(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, logged := loadConfigLogged(t, string(data))
	if n := ignoredWarnings(logged); n != 0 {
		t.Fatalf("example config logged %d ignored-key warnings:\n%s", n, logged)
	}
	if !cfg.estimatedPositionsEnabled() {
		t.Error("estimatedPositions must stay on")
	}
	if cfg.ClientRegionsEnabled() || cfg.ClientRxCoverageEnabled() || cfg.GZipEnabled() || cfg.WSCompressionEnabled() ||
		(cfg.Privacy != nil && cfg.Privacy.Enabled) || cfg.ChannelProposals.SubmissionsRequested() {
		t.Error("every other opt-in gate in the example config is off")
	}
}

// Each ignored key of a lifted block is reported once, and the exact-key
// reader is not kept after load.
func TestLiftedBlockVariantsWarnOnceAndKeysReleased888(t *testing.T) {
	cfg, logged := loadConfigLogged(t, `{"map":{"Dhm":{"enabled":true},"DHM":{"enabled":true},"masts":{"enabled":true},"Masts":{"enabled":true},"heritage":{"Enabled":true}}}`)
	for _, spelling := range []string{"map.Dhm ", "map.DHM ", "map.Masts ", "map.heritage.Enabled "} {
		if n := strings.Count(logged, spelling); n != 1 {
			t.Fatalf("%q logged %d times, want 1:\n%s", spelling, n, logged)
		}
	}
	if cfg.keys != nil {
		t.Fatal("cfg.keys must be nil once the typed blocks are extracted")
	}
	logged2 := captureLog(func() { cfg.extractTypedMapBlocks() })
	if logged2 != "" {
		t.Fatalf("a second extraction logged again:\n%s", logged2)
	}
}
