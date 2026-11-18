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

// Issue #884: the ingestor reads the antenna-claim gates by their canonical
// spellings only, like the server. encoding/json would accept `Enabled` or
// `AutoApproveProven`, even after a canonical false in the same object.
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
		{"AutoApproveProven after canonical false", `{"antennaClaims":{"autoApproveProven":false,"AutoApproveProven":true}}`, false, false, 1},
		{"AutoApproveProven before canonical false", `{"antennaClaims":{"AutoApproveProven":true,"autoApproveProven":false}}`, false, false, 1},
		{"AUTOAPPROVEPROVEN alone", `{"antennaClaims":{"AUTOAPPROVEPROVEN":true}}`, false, false, 1},
		{"top-level AntennaClaims", `{"AntennaClaims":{"enabled":true,"autoApproveProven":true}}`, false, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			if got := cfg.AntennaClaims.SubmissionsRequested(); got != tc.wantEnabled {
				t.Fatalf("SubmissionsRequested = %v, want %v", got, tc.wantEnabled)
			}
			if got := cfg.AntennaClaims.AutoApprove(); got != tc.wantAuto {
				t.Fatalf("AutoApprove = %v, want %v", got, tc.wantAuto)
			}
			if n := strings.Count(buf.String(), configkey.IgnoredNote); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, buf.String())
			}
		})
	}
}

// #898 M3: the ingestor reads antennaClaims.detailsEnabled by its canonical
// spelling only, like the server, and it is off by default.
func TestAntennaClaimDetailsGateReadsOnlyCanonicalKey898(t *testing.T) {
	cases := []struct {
		name        string
		cfg         string
		wantDetails bool
		warnings    int
	}{
		{"absent is off", `{"antennaClaims":{"enabled":true}}`, false, 0},
		{"canonical on", `{"antennaClaims":{"enabled":true,"detailsEnabled":true}}`, true, 0},
		{"canonical false", `{"antennaClaims":{"enabled":true,"detailsEnabled":false}}`, false, 0},
		{"DetailsEnabled alone", `{"antennaClaims":{"enabled":true,"DetailsEnabled":true}}`, false, 1},
		{"DetailsEnabled after canonical false", `{"antennaClaims":{"detailsEnabled":false,"DetailsEnabled":true}}`, false, 1},
		{"DETAILSENABLED before canonical false", `{"antennaClaims":{"DETAILSENABLED":true,"detailsEnabled":false}}`, false, 1},
		{"top-level AntennaClaims", `{"AntennaClaims":{"enabled":true,"detailsEnabled":true}}`, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			if got := cfg.AntennaClaims.DetailsRequested(); got != tc.wantDetails {
				t.Fatalf("DetailsRequested = %v, want %v", got, tc.wantDetails)
			}
			if n := strings.Count(buf.String(), configkey.IgnoredNote); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, buf.String())
			}
		})
	}
}
