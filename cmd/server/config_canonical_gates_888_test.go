package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/configkey"
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

// map.dhm and its sub-gates read only canonical keys too (#888 review):
// `Enabled` alone must not switch the overlay on, and a canonical false wins.
func TestDHMGatesReadOnlyCanonicalKeys888(t *testing.T) {
	gates := []struct {
		name string
		path string // the JSON object that holds the gate, inside map.dhm
		on   func(*Config) bool
	}{
		{"dhm", ``, func(c *Config) bool { return c.DHM.Enabled }},
		{"ortho", `"ortho"`, func(c *Config) bool { return c.DHM.Ortho.Enabled }},
		{"profile", `"profile"`, func(c *Config) bool { return c.DHM.Profile.Enabled }},
		{"share", `"profile":{"share"`, func(c *Config) bool { return c.DHM.Profile.Share.Enabled }},
		{"foreignFill", `"profile":{"foreignFill"`, func(c *Config) bool { return c.DHM.Profile.ForeignFill.Enabled }},
		{"buildings", `"profile":{"buildings"`, func(c *Config) bool { return c.DHM.Profile.Buildings.Enabled }},
	}
	// wrap builds map.dhm around a gate body, e.g. {"enabled":true}.
	wrap := func(path, body string) string {
		switch {
		case path == "":
			return `{"map":{"dhm":` + body + `}}`
		case strings.HasPrefix(path, `"profile":{`):
			return `{"map":{"dhm":{"profile":{` + strings.TrimPrefix(path, `"profile":{`) + `:` + body + `}}}}`
		default:
			return `{"map":{"dhm":{` + path + `:` + body + `}}}`
		}
	}
	for _, g := range gates {
		cases := []struct {
			name, body string
			want       bool
			warnings   int
		}{
			{"canonical on", `{"enabled":true}`, true, 0},
			{"Enabled after canonical false", `{"enabled":false,"Enabled":true}`, false, 1},
			{"ENABLED before canonical false", `{"ENABLED":true,"enabled":false}`, false, 1},
			{"Enabled alone", `{"Enabled":true}`, false, 1},
		}
		for _, tc := range cases {
			t.Run(g.name+"/"+tc.name, func(t *testing.T) {
				cfg, logged := loadConfigLogged(t, wrap(g.path, tc.body))
				if got := g.on(cfg); got != tc.want {
					t.Fatalf("%s on = %v, want %v", g.name, got, tc.want)
				}
				if n := strings.Count(logged, configkey.IgnoredNote); n != tc.warnings {
					t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
				}
			})
		}
	}
}

// A malformed map.dhm stays disabled even when it holds a canonical true.
func TestDHMMalformedStaysOff888(t *testing.T) {
	cfg, _ := loadConfigLogged(t, `{"map":{"dhm":{"enabled":true,"token":5}}}`)
	if cfg.DHM.Enabled {
		t.Fatal("malformed map.dhm must stay disabled")
	}
}

// The estimatedPositions default is kept, and the operator is told so: a
// non-canonical key that used to switch it off must produce a warning that
// names the feature, beyond the generic ignored-key line.
func TestEstimatedPositionsIgnoredKeyWarnsItStaysOn888(t *testing.T) {
	for _, cfgJSON := range []string{
		`{"estimatedPositions":{"Enabled":false}}`,
		`{"EstimatedPositions":{"enabled":false}}`,
	} {
		cfg, logged := loadConfigLogged(t, cfgJSON)
		if !cfg.estimatedPositionsEnabled() {
			t.Fatalf("%s: must stay on", cfgJSON)
		}
		var named int
		for _, line := range strings.Split(logged, "\n") {
			if strings.Contains(line, "estimatedPositions") && !strings.Contains(line, configkey.IgnoredNote) {
				named++
			}
		}
		if named != 1 {
			t.Fatalf("%s: %d stays-on warnings, want 1:\n%s", cfgJSON, named, logged)
		}
	}
	// Canonical false, or no block at all, needs no extra warning.
	for _, cfgJSON := range []string{`{}`, `{"estimatedPositions":{"enabled":false}}`} {
		_, logged := loadConfigLogged(t, cfgJSON)
		if strings.Contains(logged, "estimatedPositions") {
			t.Fatalf("%s: unexpected warning:\n%s", cfgJSON, logged)
		}
	}
}
