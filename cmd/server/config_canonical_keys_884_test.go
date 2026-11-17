package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/configkey"
)

// Issue #884: encoding/json matches object keys case-insensitively, so a
// feature gate spelled `Enabled` (or a whole block spelled `Share`, `Map`,
// …) used to switch a feature on, even next to a canonical `"enabled":
// false`. Gates and the client-visible base-map token now read only the
// canonical spellings; every ignored spelling logs one warning. Dummy
// values only.

const canonicalKeysTestToken = "dummy-basemap-884"

// loadConfigLogged loads cfgJSON through LoadConfig and returns the config
// and what it logged.
func loadConfigLogged(t *testing.T, cfgJSON string) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", cfgJSON)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	cfg, err := LoadConfig(dir)
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, buf.String()
}

// ignoredWarnings counts the #884 warnings in a log.
func ignoredWarnings(logged string) int {
	return strings.Count(logged, configkey.IgnoredNote)
}

func TestShareGatesReadOnlyCanonicalKeys884(t *testing.T) {
	cases := []struct {
		name                string
		cfg                 string
		wantNodes, wantPkts bool
		warnings            int
	}{
		{"canonical on", `{"share":{"nodes":{"enabled":true},"packets":{"enabled":true}}}`, true, true, 0},
		{"Enabled after canonical false", `{"share":{"nodes":{"enabled":false,"Enabled":true},"packets":{"enabled":false,"ENABLED":true}}}`, false, false, 2},
		{"Enabled before canonical false", `{"share":{"nodes":{"Enabled":true,"enabled":false}}}`, false, false, 1},
		{"Enabled alone", `{"share":{"nodes":{"Enabled":true},"packets":{"Enabled":true}}}`, false, false, 2},
		{"top-level Share", `{"Share":{"nodes":{"enabled":true},"packets":{"enabled":true}}}`, false, false, 1},
		{"share.Nodes", `{"share":{"Nodes":{"enabled":true}}}`, false, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logged := loadConfigLogged(t, tc.cfg)
			if cfg.Share.Nodes.Enabled != tc.wantNodes || cfg.Share.Packets.Enabled != tc.wantPkts {
				t.Fatalf("share nodes=%v packets=%v, want %v %v", cfg.Share.Nodes.Enabled, cfg.Share.Packets.Enabled, tc.wantNodes, tc.wantPkts)
			}
			if n := ignoredWarnings(logged); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
			}
		})
	}
}

func TestAntennaClaimGatesReadOnlyCanonicalKeys884(t *testing.T) {
	cases := []struct {
		name                  string
		cfg                   string
		wantEnabled, wantAuto bool
		warnings              int
	}{
		{"canonical on", `{"antennaClaims":{"enabled":true,"autoApproveProven":true}}`, true, true, 0},
		{"Enabled after canonical false", `{"antennaClaims":{"enabled":false,"Enabled":true}}`, false, false, 1},
		{"Enabled alone", `{"antennaClaims":{"Enabled":true}}`, false, false, 1},
		{"AutoApproveProven after canonical false", `{"antennaClaims":{"enabled":true,"autoApproveProven":false,"AutoApproveProven":true}}`, true, false, 1},
		{"autoapproveproven alone", `{"antennaClaims":{"enabled":true,"autoapproveproven":true}}`, true, false, 1},
		{"top-level AntennaClaims", `{"AntennaClaims":{"enabled":true,"autoApproveProven":true}}`, false, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logged := loadConfigLogged(t, tc.cfg)
			if got := cfg.AntennaClaims.SubmissionsRequested(); got != tc.wantEnabled {
				t.Fatalf("SubmissionsRequested = %v, want %v", got, tc.wantEnabled)
			}
			if got := cfg.AntennaClaims.AutoApprove(); got != tc.wantAuto {
				t.Fatalf("AutoApprove = %v, want %v", got, tc.wantAuto)
			}
			if n := ignoredWarnings(logged); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
			}
		})
	}
}

func TestLiftedMapBlockGatesReadOnlyCanonicalKeys884(t *testing.T) {
	blocks := []struct {
		key     string
		enabled func(*Config) bool
	}{
		{"skraafoto", func(c *Config) bool { return c.Skraafoto.Enabled }},
		{"masts", func(c *Config) bool { return c.Masts.Enabled }},
		{"heritage", func(c *Config) bool { return c.Heritage.Enabled }},
	}
	for _, b := range blocks {
		cases := []struct {
			name, body string
			want       bool
			warnings   int
		}{
			{"canonical on", `{"enabled":true}`, true, 0},
			{"Enabled alone", `{"Enabled":true}`, false, 1},
			{"Enabled after canonical false", `{"enabled":false,"Enabled":true}`, false, 1},
			{"ENABLED before canonical false", `{"ENABLED":true,"enabled":false}`, false, 1},
		}
		for _, tc := range cases {
			t.Run(b.key+"/"+tc.name, func(t *testing.T) {
				cfg, logged := loadConfigLogged(t, `{"map":{"`+b.key+`":`+tc.body+`}}`)
				if got := b.enabled(cfg); got != tc.want {
					t.Fatalf("map.%s enabled = %v, want %v", b.key, got, tc.want)
				}
				if n := ignoredWarnings(logged); n != tc.warnings {
					t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
				}
			})
		}
	}
}

func TestNonCanonicalTopLevelMapDoesNotShipTheBaseMapToken884(t *testing.T) {
	on := `{"tiles":{"providers":{"dataforsyningen":{"enabled":true,"token":"` + canonicalKeysTestToken + `"}}}}`
	off := `{"tiles":{"providers":{"dataforsyningen":{"enabled":false,"token":"` + canonicalKeysTestToken + `"}}}}`
	cases := []struct {
		name, cfg string
		wantToken bool
	}{
		{"canonical map on", `{"map":` + on + `}`, true},
		{"canonical map off", `{"map":` + off + `}`, false},
		{"top-level Map on, canonical map off", `{"map":` + off + `,"Map":` + on + `}`, false},
		{"top-level Map on before canonical map off", `{"Map":` + on + `,"map":` + off + `}`, false},
		{"top-level Map on alone", `{"Map":` + on + `}`, false},
		{"top-level MAP on, canonical map without tiles", `{"map":{"dhm":{"enabled":false}},"MAP":` + on + `}`, false},
		// The legacy top-level tiles block is migrated into map.tiles, so
		// its spelling counts the same way.
		{"legacy top-level Tiles on", `{"Tiles":` + strings.TrimSuffix(strings.TrimPrefix(on, `{"tiles":`), `}`) + `}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := clientConfigFor(t, tc.cfg)
			if got := strings.Contains(body, canonicalKeysTestToken); got != tc.wantToken {
				t.Fatalf("/api/config/client ships the base-map token: %v, want %v\n%s", got, tc.wantToken, body)
			}
		})
	}
}

func TestNonCanonicalTopLevelMapIsWarnedOnce884(t *testing.T) {
	cfg, logged := loadConfigLogged(t, `{"map":{"tiles":{}},"Map":{"tiles":{"providers":{"dataforsyningen":{"enabled":true,"token":"`+canonicalKeysTestToken+`"}}}}}`)
	if n := ignoredWarnings(logged); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, logged)
	}
	if strings.Contains(logged, canonicalKeysTestToken) {
		t.Fatalf("the warning must not log the token:\n%s", logged)
	}
	if got := jsonObjectAt(cfg.Map, "tiles", "providers"); got != nil {
		t.Fatalf("map.tiles.providers = %v, want nothing from the top-level Map", got)
	}
}
