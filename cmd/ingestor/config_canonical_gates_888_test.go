package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/configkey"
)

// Issue #888: the ingestor reads clientRxCoverage.enabled,
// clientRegions.enabled and channelProposals.enabled by their canonical
// spellings only, like the server.
func TestIngestorRemainingGatesReadOnlyCanonicalKeys888(t *testing.T) {
	gates := []struct {
		block, upper string
		on           func(*Config) bool
	}{
		{"clientRxCoverage", "ClientRxCoverage", func(c *Config) bool { return c.ClientRxCoverageEnabled() }},
		{"clientRegions", "ClientRegions", func(c *Config) bool { return c.ClientRegionsEnabled() }},
		{"channelProposals", "ChannelProposals", func(c *Config) bool { return c.ChannelProposals.SubmissionsRequested() }},
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
			{"block spelled in another case", `{"` + g.upper + `":{"enabled":true}}`, false, 1},
		}
		for _, tc := range cases {
			t.Run(g.block+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, []byte(tc.cfg), 0o644); err != nil {
					t.Fatal(err)
				}
				var buf bytes.Buffer
				log.SetOutput(&buf)
				cfg, err := LoadConfig(path)
				log.SetOutput(os.Stderr)
				if err != nil {
					t.Fatal(err)
				}
				if got := g.on(cfg); got != tc.want {
					t.Fatalf("%s on = %v, want %v", g.block, got, tc.want)
				}
				if n := strings.Count(buf.String(), configkey.IgnoredNote); n != tc.warnings {
					t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, buf.String())
				}
			})
		}
	}
}

// config.example.json loads with no ignored-key warning and every opt-in gate off.
func TestIngestorExampleConfigLoadsIdentically888(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	cfg, err := LoadConfig(filepath.Join("..", "..", "config.example.json"))
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), configkey.IgnoredNote) {
		t.Fatalf("example config logged an ignored-key warning:\n%s", buf.String())
	}
	if cfg.ClientRxCoverageEnabled() || cfg.ClientRegionsEnabled() || cfg.ChannelProposals.SubmissionsRequested() {
		t.Error("every opt-in gate in the example config is off")
	}
}
