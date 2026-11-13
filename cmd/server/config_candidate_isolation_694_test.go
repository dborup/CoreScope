package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #694: LoadConfig walks several candidate files and used to decode
// every one of them into the same *Config. encoding/json fills fields as it
// goes and keeps going past a type error, so a candidate that was rejected
// still left its fields -- and its maps -- behind for the next candidate to
// be merged into. The DHM block made that security-relevant: a rejected file
// could switch the terrain overlay and the profile proxy on, with its own
// token, while the config the operator actually meant to serve has no
// map.dhm at all.
//
// The fixture below is the reproduction from the issue: map.dhm comes
// *before* the broken key in the document, so the decoder has already taken
// it when it hits the error. Key order is load-bearing in every fixture
// here, which is why they are literal JSON rather than marshalled maps --
// json.Marshal sorts map keys and would put "areas" first, aborting the
// decode before it ever reaches "map".
const rejectedDHMCandidate = `{
  "port": 8080,
  "branding": {"siteName": "Leaked"},
  "regions": {"SJC": "Leaked Region"},
  "map": {"dhm": {"enabled": true, "token": "leaked-dhm-token", "profile": {"enabled": true}}},
  "areas": 5
}`

func writeConfigFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if d := filepath.Dir(filepath.Join(dir, name)); d != dir {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadConfigRejectedCandidateDoesNotEnableDHM is the issue's own
// reproduction: an enabled map.dhm with a token in a file that fails to
// decode, and a valid file without any DHM block. DHM must stay off.
func TestLoadConfigRejectedCandidateDoesNotEnableDHM(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", rejectedDHMCandidate)
	writeConfigFile(t, dir, "data/config.json", `{"port": 9090}`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 {
		t.Fatalf("expected the valid candidate to be served, got port %d", cfg.Port)
	}
	if cfg.DHM.Enabled {
		t.Error("map.dhm from the rejected candidate enabled the terrain overlay")
	}
	if cfg.DHM.Token != "" {
		t.Error("map.dhm token from the rejected candidate survived into the served config")
	}
	if cfg.DHM.Profile.Enabled {
		t.Error("map.dhm.profile from the rejected candidate enabled the profile proxy")
	}
	if cfg.DHM.Active() {
		t.Error("DHM is active although the served config has no map.dhm")
	}
}

// TestLoadConfigRejectedCandidateDoesNotLeakFields covers the general leak:
// one map field (branding) and one scalar (port).
func TestLoadConfigRejectedCandidateDoesNotLeakFields(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", rejectedDHMCandidate)
	writeConfigFile(t, dir, "data/config.json", `{"dbPath": "/valid/path.db"}`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != "/valid/path.db" {
		t.Fatalf("expected the valid candidate to be served, got dbPath %q", cfg.DBPath)
	}
	if cfg.Branding != nil {
		t.Errorf("branding leaked from the rejected candidate: %v", cfg.Branding)
	}
	// The second candidate has no "port", so the served value must be the
	// default the fresh struct starts at -- not the 8080 of the rejected one.
	if cfg.Port != 3000 {
		t.Errorf("port leaked from the rejected candidate: got %d, want the default 3000", cfg.Port)
	}
	if cfg.Regions != nil {
		t.Errorf("regions leaked from the rejected candidate: %v", cfg.Regions)
	}
	// migrateDeprecatedConfig always seeds map.tiles, so the probe here is
	// that nothing *else* reached Map.
	for k, v := range cfg.Map {
		if k != "tiles" {
			t.Errorf("map.%s leaked from the rejected candidate", k)
			continue
		}
		if tiles, ok := v.(map[string]interface{}); !ok || len(tiles) != 0 {
			t.Errorf("map.tiles leaked content from the rejected candidate: %v", v)
		}
	}
}

// TestLoadConfigValidFirstCandidateWins pins the precedence the loop has
// always had: the first readable, decodable file is the one served, and the
// later candidate is never merged on top of it.
func TestLoadConfigValidFirstCandidateWins(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", `{"port": 8081, "branding": {"siteName": "First"}}`)
	writeConfigFile(t, dir, "data/config.json", `{"port": 9091, "dbPath": "/second.db"}`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8081 {
		t.Errorf("expected port 8081 from the first candidate, got %d", cfg.Port)
	}
	if cfg.DBPath != "" {
		t.Errorf("the second candidate was merged in: dbPath %q", cfg.DBPath)
	}
	if cfg.Branding == nil || cfg.Branding["siteName"] != "First" {
		t.Errorf("expected the first candidate's branding, got %v", cfg.Branding)
	}
}

// TestLoadConfigRejectionIsLoggedWithoutValues: the operator has to be able
// to see which file was dropped and roughly why, and the log must never
// carry config values -- the rejected file is exactly the one that may hold
// a token.
func TestLoadConfigRejectionIsLoggedWithoutValues(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", rejectedDHMCandidate)
	writeConfigFile(t, dir, "data/config.json", `{"port": 9090}`)

	var cfg *Config
	var err error
	out := captureLog(func() { cfg, err = LoadConfig(dir) })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 {
		t.Fatalf("expected the valid candidate to be served, got port %d", cfg.Port)
	}
	if !strings.Contains(out, filepath.Join(dir, "config.json")) {
		t.Errorf("rejection log does not name the rejected file; got %q", out)
	}
	if !strings.Contains(out, "ignored") {
		t.Errorf("rejection log does not say the file was ignored; got %q", out)
	}
	if !strings.Contains(out, "areas") {
		t.Errorf("rejection log does not name the offending field; got %q", out)
	}
	for _, secret := range []string{"leaked-dhm-token", "Leaked", "Leaked Region", "8080"} {
		if strings.Contains(out, secret) {
			t.Errorf("rejection log leaked a config value %q; got %q", secret, out)
		}
	}
}

// TestLoadConfigSyntaxErrorRejectionLogsOffset: a syntax error has no field
// to name, so the offset is what the operator gets. Still no values.
func TestLoadConfigSyntaxErrorRejectionLogsOffset(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", `{"port": 8080, "branding": {"siteName": "Leaked"`)
	writeConfigFile(t, dir, "data/config.json", `{"port": 9090}`)

	var cfg *Config
	var err error
	out := captureLog(func() { cfg, err = LoadConfig(dir) })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 {
		t.Fatalf("expected the valid candidate to be served, got port %d", cfg.Port)
	}
	if !strings.Contains(out, "offset") {
		t.Errorf("syntax rejection log does not carry an offset; got %q", out)
	}
	if strings.Contains(out, "Leaked") {
		t.Errorf("syntax rejection log leaked a config value; got %q", out)
	}
}

// TestLoadConfigEstimatedPositionsStillFailsStartup: the per-candidate
// rewrite must not turn the explicit startup failure for a malformed
// estimatedPositions policy into a silent fall-through to the next file.
func TestLoadConfigEstimatedPositionsStillFailsStartup(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", `{"port": 8080, "estimatedPositions": {"enabled": "false"}}`)
	writeConfigFile(t, dir, "data/config.json", `{"port": 9090}`)

	cfg, err := LoadConfig(dir)
	if err == nil {
		t.Fatalf("expected startup to fail on estimatedPositions.enabled, got cfg %+v", cfg)
	}
	if cfg != nil {
		t.Errorf("expected no config alongside the error, got %+v", cfg)
	}
	if !strings.Contains(err.Error(), "estimatedPositions.enabled") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestConfigExampleDecodesCleanly: the shipped example is what manage.sh
// copies into place on a fresh deployment, so it has to survive the loader
// as a whole. Before #694 it did not: `areas` is a map of area objects and
// the block carried a string "_comment", so the file failed to decode and
// only reached the server through the partial decode this issue removes.
// Every documentation note therefore has to sit where it is either a
// top-level `_comment_*` key or a field of a struct, never a value in a map
// of typed entries.
func TestConfigExampleDecodesCleanly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, newDefaultConfig()); err != nil {
		t.Fatalf("config.example.json does not decode into Config: %v", err)
	}

	// And end to end: the example must come back as itself, not as defaults.
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", string(data))
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Areas) == 0 {
		t.Error("example areas did not survive the load")
	}
	if _, ok := cfg.Areas["_comment"]; ok {
		t.Error("the areas documentation note is still an area entry")
	}
	if cfg.Branding == nil {
		t.Error("example branding did not survive the load")
	}
}

// TestLoadConfigRejectedSoleCandidateFallsBackToDefaults is the one-file
// variant, and the sharpest one: with nothing valid behind it, the rejected
// file used to BE the served config -- every field it had got as far as,
// map.dhm included. Now it is dropped whole and the shipped defaults are
// served instead.
func TestLoadConfigRejectedSoleCandidateFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", rejectedDHMCandidate)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3000 {
		t.Errorf("rejected sole candidate set the port: got %d, want the default 3000", cfg.Port)
	}
	if cfg.Branding != nil {
		t.Errorf("rejected sole candidate set branding: %v", cfg.Branding)
	}
	if cfg.Regions != nil {
		t.Errorf("rejected sole candidate set regions: %v", cfg.Regions)
	}
	if cfg.DHM.Enabled || cfg.DHM.Token != "" || cfg.DHM.Active() {
		t.Error("rejected sole candidate enabled DHM with its own token")
	}
}
