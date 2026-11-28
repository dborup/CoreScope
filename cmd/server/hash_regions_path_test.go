package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	regionutil "github.com/meshcore-analyzer/regions"
)

// parityFixtureDir is the single config.json + hash-regions.json pair that
// BOTH cmd/server and cmd/ingestor assert against, so parity is shown on
// the same bytes rather than on two hand-copied expectations. See
// internal/regions/testdata/parity/config.json.
const parityFixtureDir = "../../internal/regions/testdata/parity"

// parityWantNames is the effective set that fixture must produce. The
// ingestor's TestLoadRegionKeys_ParityFixtureMatchesSharedLoader asserts
// the same list.
var parityWantNames = []string{"#dk", "#eu", "#dk-aarhus", "#NO"}

// copyParityFixture copies the shared parity fixture into a temp directory
// as config.json + hash-regions.json and returns the config path. The
// committed fixture cannot itself be named config.json (the repo
// .gitignore excludes that name everywhere), and copying keeps both
// processes asserting against the same committed bytes.
func copyParityFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for src, dst := range map[string]string{
		"config.json.fixture": "config.json",
		"hash-regions.json":   "hash-regions.json",
	} {
		data, err := os.ReadFile(filepath.Join(parityFixtureDir, src))
		if err != nil {
			t.Fatalf("read fixture %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}
	return filepath.Join(dir, "config.json")
}

// writeRegionsConfig writes a config.json carrying the given inline
// hashRegions and hashRegionsPath into dir.
func writeRegionsConfig(t *testing.T, dir string, inline []string, path string) {
	t.Helper()
	cfg := struct {
		Port            int      `json:"port"`
		HashRegions     []string `json:"hashRegions"`
		HashRegionsPath string   `json:"hashRegionsPath,omitempty"`
	}{Port: 3000, HashRegions: inline, HashRegionsPath: path}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// writeRegionsFile writes a JSON array of region names into dir/name.
func writeRegionsFile(t *testing.T, dir, name string, names ...string) string {
	t.Helper()
	data, err := json.Marshal(names)
	if err != nil {
		t.Fatalf("marshal names: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// chdirTest moves the process into dir for the duration of the test: the
// relative-path rule is "relative to the config file", which only means
// something from an unrelated working directory.
func chdirTest(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// An inline-only config must behave exactly as before: no file read, and a
// file that merely looks like a default is ignored.
func TestEffectiveHashRegions_InlineOnlyUnchanged(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "hash-regions.json", "sneaky")
	writeRegionsConfig(t, dir, []string{"#belgium", "eu", "", "#belgium"}, "")

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := []string{"#belgium", "#eu"}; !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want %v (no auto-discovered file)", cfg.EffectiveHashRegions(), want)
	}
}

func TestEffectiveHashRegions_EnvOverrideWinsOverConfigKey(t *testing.T) {
	dir := t.TempDir()
	writeRegionsFile(t, dir, "from-cfg.json", "cfg-region")
	envFile := writeRegionsFile(t, dir, "from-env.json", "env-region")
	writeRegionsConfig(t, dir, []string{"#dk"}, "from-cfg.json")
	t.Setenv(regionutil.PathEnvVar, envFile)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := []string{"#dk", "#env-region"}; !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want %v (HASH_REGIONS_PATH must win)", cfg.EffectiveHashRegions(), want)
	}
}

func TestEffectiveHashRegions_MissingFileFallsBackToInline(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsConfig(t, dir, []string{"#dk", "eu"}, "not-there.json")

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig must not fail on an unusable hashRegionsPath: %v", err)
	}
	if want := []string{"#dk", "#eu"}; !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want the inline list %v as fallback", cfg.EffectiveHashRegions(), want)
	}
}

func TestEffectiveHashRegions_InvalidJSONFallsBackToInline(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "regions.json"), []byte(`{"dk":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRegionsConfig(t, dir, []string{"#dk"}, "regions.json")

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig must not fail on a malformed hashRegionsPath: %v", err)
	}
	if want := []string{"#dk"}; !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want the inline list %v as fallback", cfg.EffectiveHashRegions(), want)
	}
}

// Inline + file merge: blank entries dropped, "dk"/"#dk" collapsed, "#DK"
// kept distinct (the ingestor's key is SHA256 of the exact name).
func TestEffectiveHashRegions_MergesInlineAndFile(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk", "#DK", "dk-aarhus", "", "  ", "#eu")
	writeRegionsConfig(t, dir, []string{"#dk", "", "eu"}, "regions.json")

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"#dk", "#eu", "#DK", "#dk-aarhus"}
	if !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want %v", cfg.EffectiveHashRegions(), want)
	}
}

func TestEffectiveHashRegions_RelativePathResolvesAgainstConfigDir(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk-aarhus")
	writeRegionsConfig(t, dir, []string{"#dk"}, "regions.json")
	chdirTest(t, t.TempDir()) // CWD has no regions.json at all

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := []string{"#dk", "#dk-aarhus"}; !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want %v (relative to the config file, not the CWD)", cfg.EffectiveHashRegions(), want)
	}
}

// LoadConfig also searches <dir>/data/config.json; a relative path must
// anchor at the config file that was actually used, not at <dir>.
func TestEffectiveHashRegions_RelativePathAnchorsAtDataConfig(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRegionsFile(t, dataDir, "regions.json", "dk-aarhus")
	writeRegionsConfig(t, dataDir, []string{"#dk"}, "regions.json")

	cfg, err := LoadConfig(base)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := []string{"#dk", "#dk-aarhus"}; !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want %v (anchored at data/config.json)", cfg.EffectiveHashRegions(), want)
	}
}

// An absolute path is used as-is, from any config location.
func TestEffectiveHashRegions_AbsolutePath(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	dir := t.TempDir()
	abs := writeRegionsFile(t, t.TempDir(), "regions.json", "dk-aarhus")
	writeRegionsConfig(t, dir, []string{"#dk"}, abs)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := []string{"#dk", "#dk-aarhus"}; !reflect.DeepEqual(cfg.EffectiveHashRegions(), want) {
		t.Errorf("EffectiveHashRegions = %v, want %v", cfg.EffectiveHashRegions(), want)
	}
}

// A nil *Config must not panic: handlers call this through s.cfg.
func TestEffectiveHashRegions_NilConfig(t *testing.T) {
	var cfg *Config
	if got := cfg.EffectiveHashRegions(); len(got) != 0 {
		t.Errorf("EffectiveHashRegions on nil cfg = %v, want empty", got)
	}
}

// The server's effective set must equal the shared loader's names for the
// shared parity fixture — the same assertion the ingestor makes.
func TestEffectiveHashRegions_ParityFixtureMatchesSharedLoader(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	cfgPath := copyParityFixture(t)
	cfg, err := LoadConfig(filepath.Dir(cfgPath))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg.EffectiveHashRegions(), parityWantNames) {
		t.Errorf("EffectiveHashRegions = %v, want %v", cfg.EffectiveHashRegions(), parityWantNames)
	}

	res, err := regionutil.Load(cfg.HashRegions, cfg.HashRegionsPath, cfgPath, nil)
	if err != nil {
		t.Fatalf("regions.Load: %v", err)
	}
	if !reflect.DeepEqual(res.Names, parityWantNames) {
		t.Errorf("shared loader names = %v, want %v", res.Names, parityWantNames)
	}
}

// A scope that only exists in the external file must not be reported as
// unknown by Observer Neighbors (#360 acceptance criterion).
func TestHandleAllObserverNeighbors_FileScopesAreNotUnknown(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	srv, router := setupTestServer(t)
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk-storkbh")
	writeRegionsConfig(t, dir, []string{"dk"}, "regions.json")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	srv.cfg.HashRegions = cfg.HashRegions
	srv.cfg.HashRegionsPath = cfg.HashRegionsPath
	srv.cfg.hashRegionsFile = cfg.hashRegionsFile

	if _, err := srv.db.conn.Exec(`INSERT INTO observer_neighbors (observer_id, neighbor_pubkey, scopes, status, reported_at) VALUES
		('obs1', ?, '*,#dk,#dk-storkbh,#dk-unknown', 'responded', '2026-07-28T14:00:00Z')`,
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"); err != nil {
		t.Fatalf("seed observer_neighbors: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/observers/neighbors", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var body struct {
		UnknownScopes []struct {
			Scope string `json:"scope"`
		} `json:"unknownScopes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(body.UnknownScopes) != 1 || body.UnknownScopes[0].Scope != "#dk-unknown" {
		t.Errorf("unknownScopes = %+v, want only #dk-unknown (#dk-storkbh comes from hashRegionsPath)", body.UnknownScopes)
	}
}

// Scopes from the external file count as configured in /api/scope-stats, so
// they show up in unusedRegions/usedRegions (#360 acceptance criterion).
func TestHandleScopeStats_FileScopesCountAsConfigured(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	srv, _ := setupTestServer(t)
	if _, err := srv.db.conn.Exec(`ALTER TABLE transmissions ADD COLUMN scope_name TEXT DEFAULT NULL`); err != nil {
		t.Fatalf("add scope_name column: %v", err)
	}
	srv.db.hasScopeNameFlag.forceTrue()
	if _, err := srv.db.conn.Exec(`DELETE FROM transmissions`); err != nil {
		t.Fatalf("clear transmissions: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := srv.db.conn.Exec(
		`INSERT INTO transmissions (raw_hex,hash,first_seen,route_type,payload_type,scope_name) VALUES (?,?,?,?,5,?)`,
		"aa", "h1", now, 0, "#from-file",
	); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "from-file", "unused-from-file", "#belgium")
	writeRegionsConfig(t, dir, []string{"#belgium"}, "regions.json")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	srv.cfg.HashRegions = cfg.HashRegions
	srv.cfg.HashRegionsPath = cfg.HashRegionsPath
	srv.cfg.hashRegionsFile = cfg.hashRegionsFile

	req := httptest.NewRequest("GET", "/api/scope-stats?window=24h", nil)
	w := httptest.NewRecorder()
	srv.handleScopeStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp ScopeStatsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ConfiguredRegions != 3 {
		t.Errorf("configuredRegions = %d, want 3 (#belgium deduped across inline+file)", resp.ConfiguredRegions)
	}
	if want := []string{"#from-file"}; !reflect.DeepEqual(resp.UsedRegions, want) {
		t.Errorf("usedRegions = %v, want %v", resp.UsedRegions, want)
	}
	if want := []string{"#belgium", "#unused-from-file"}; !reflect.DeepEqual(resp.UnusedRegions, want) {
		t.Errorf("unusedRegions = %v, want %v", resp.UnusedRegions, want)
	}
}

// With no inline hashRegions at all, a file-only config must still produce
// region stats — the old `len(cfg.HashRegions) > 0` gate would skip them.
func TestHandleScopeStats_FileOnlyConfigIsNotSkipped(t *testing.T) {
	t.Setenv(regionutil.PathEnvVar, "")
	srv, _ := setupTestServer(t)
	if _, err := srv.db.conn.Exec(`ALTER TABLE transmissions ADD COLUMN scope_name TEXT DEFAULT NULL`); err != nil {
		t.Fatalf("add scope_name column: %v", err)
	}
	srv.db.hasScopeNameFlag.forceTrue()

	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "only-in-file")
	writeRegionsConfig(t, dir, nil, "regions.json")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	srv.cfg.HashRegions = nil
	srv.cfg.HashRegionsPath = cfg.HashRegionsPath
	srv.cfg.hashRegionsFile = cfg.hashRegionsFile

	req := httptest.NewRequest("GET", "/api/scope-stats?window=24h", nil)
	w := httptest.NewRecorder()
	srv.handleScopeStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp ScopeStatsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ConfiguredRegions != 1 {
		t.Errorf("configuredRegions = %d, want 1 (file-only config)", resp.ConfiguredRegions)
	}
	if want := []string{"#only-in-file"}; !reflect.DeepEqual(resp.UnusedRegions, want) {
		t.Errorf("unusedRegions = %v, want %v", resp.UnusedRegions, want)
	}
}
