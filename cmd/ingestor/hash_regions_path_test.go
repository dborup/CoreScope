package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"
	"time"

	regions "github.com/meshcore-analyzer/regions"
)

// parityFixtureDir is the single config.json + hash-regions.json pair that
// BOTH cmd/ingestor and cmd/server assert against, so the two processes can
// be shown to agree on the same bytes rather than on two hand-copied
// expectations. See internal/regions/testdata/parity/config.json.
const parityFixtureDir = "../../internal/regions/testdata/parity"

// parityWantNames is the effective set that fixture must produce.
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

// writeRegionsConfig writes a config.json with the given inline hashRegions
// and hashRegionsPath into dir and returns its path.
func writeRegionsConfig(t *testing.T, dir string, inline []string, path string) string {
	t.Helper()
	cfg := struct {
		HashRegions     []string `json:"hashRegions"`
		HashRegionsPath string   `json:"hashRegionsPath,omitempty"`
	}{HashRegions: inline, HashRegionsPath: path}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
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

// regionNames returns the sorted key names of a region-key map.
func regionNames(keys map[string][]byte) []string {
	out := make([]string, 0, len(keys))
	for name := range keys {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

// chdirTest moves the process into dir for the duration of the test: the
// relative-path rule is "relative to the config file", which only means
// something when the working directory is somewhere else entirely.
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

// Inline-only configs must keep today's behaviour exactly: no file read, no
// error, same keys.
func TestLoadRegionKeys_InlineOnlyUnchanged(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	// A file that looks like the obvious default must NOT be picked up.
	writeRegionsFile(t, dir, "hash-regions.json", "sneaky")
	cfgPath := writeRegionsConfig(t, dir, []string{"#belgium", "eu"}, "")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	keys, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	if want := []string{"#belgium", "#eu"}; !reflect.DeepEqual(regionNames(keys), want) {
		t.Errorf("region keys = %v, want %v (no auto-discovered file)", regionNames(keys), want)
	}
}

func TestLoadRegionKeys_EnvOverrideWinsOverConfigKey(t *testing.T) {
	dir := t.TempDir()
	writeRegionsFile(t, dir, "from-cfg.json", "cfg-region")
	envFile := writeRegionsFile(t, dir, "from-env.json", "env-region")
	cfgPath := writeRegionsConfig(t, dir, []string{"#dk"}, "from-cfg.json")
	t.Setenv(regions.PathEnvVar, envFile)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	keys, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	if want := []string{"#dk", "#env-region"}; !reflect.DeepEqual(regionNames(keys), want) {
		t.Errorf("region keys = %v, want %v (HASH_REGIONS_PATH must win)", regionNames(keys), want)
	}
}

func TestLoadRegionKeys_MissingFileFallsBackToInline(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	cfgPath := writeRegionsConfig(t, dir, []string{"#dk", "eu"}, "not-there.json")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	keys, err := loadRegionKeys(cfg, cfgPath)
	if err == nil {
		t.Fatal("loadRegionKeys should report an error for a missing hashRegionsPath file")
	}
	if want := []string{"#dk", "#eu"}; !reflect.DeepEqual(regionNames(keys), want) {
		t.Errorf("region keys = %v, want the inline list %v as startup fallback", regionNames(keys), want)
	}
}

func TestLoadRegionKeys_InvalidJSONFallsBackToInline(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "regions.json"), []byte("[\"dk\","), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeRegionsConfig(t, dir, []string{"#dk"}, "regions.json")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	keys, err := loadRegionKeys(cfg, cfgPath)
	if err == nil {
		t.Fatal("loadRegionKeys should report an error for a malformed hashRegionsPath file")
	}
	if want := []string{"#dk"}; !reflect.DeepEqual(regionNames(keys), want) {
		t.Errorf("region keys = %v, want the inline list %v as startup fallback", regionNames(keys), want)
	}
}

// The merge: inline + file, deduplicated after normalization. "dk" and
// "#dk" collapse; "#DK" does not, because the HMAC key is SHA256 of the
// exact name.
func TestLoadRegionKeys_MergesInlineAndFile(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk", "#DK", "dk-aarhus", "", "  ")
	cfgPath := writeRegionsConfig(t, dir, []string{"#dk", "", "eu"}, "regions.json")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	keys, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	want := []string{"#DK", "#dk", "#dk-aarhus", "#eu"}
	if !reflect.DeepEqual(regionNames(keys), want) {
		t.Errorf("region keys = %v, want %v", regionNames(keys), want)
	}
	// Distinct names must get distinct keys — a case-folding "dedupe"
	// would have dropped one of these.
	if reflect.DeepEqual(keys["#dk"], keys["#DK"]) {
		t.Error("#dk and #DK must derive different keys")
	}
}

func TestLoadRegionKeys_RelativePathResolvesAgainstConfigDir(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk-aarhus")
	cfgPath := writeRegionsConfig(t, dir, []string{"#dk"}, "regions.json")
	chdirTest(t, t.TempDir()) // CWD has no regions.json at all

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	keys, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	if want := []string{"#dk", "#dk-aarhus"}; !reflect.DeepEqual(regionNames(keys), want) {
		t.Errorf("region keys = %v, want %v (path is relative to the config file, not the CWD)", regionNames(keys), want)
	}
}

// SIGHUP must re-read the file even when config.json itself is untouched.
func TestHotKeys_ReloadRereadsHashRegionsFile(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk")
	cfgPath := writeRegionsConfig(t, dir, nil, "regions.json")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	rk, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	hk := newHotKeys(loadChannelKeys(cfg, cfgPath), rk)
	if _, ok := hk.Regions()["#dk-fyn"]; ok {
		t.Fatal("#dk-fyn present before the file was changed")
	}

	// Only the external file changes — config.json stays byte-identical.
	writeRegionsFile(t, dir, "regions.json", "dk", "dk-fyn")
	if err := hk.reload(cfgPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if want := []string{"#dk", "#dk-fyn"}; !reflect.DeepEqual(regionNames(hk.Regions()), want) {
		t.Errorf("region keys after reload = %v, want %v", regionNames(hk.Regions()), want)
	}
}

// An unusable file on reload keeps the previous keys — same contract as a
// malformed config.json.
func TestHotKeys_ReloadKeepsPreviousKeysOnInvalidRegionsFile(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk", "dk-fyn")
	cfgPath := writeRegionsConfig(t, dir, nil, "regions.json")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	rk, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	hk := newHotKeys(loadChannelKeys(cfg, cfgPath), rk)
	before := regionNames(hk.Regions())

	if err := os.WriteFile(filepath.Join(dir, "regions.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := hk.reload(cfgPath); err == nil {
		t.Fatal("reload with a malformed hashRegions file should return an error")
	}
	if got := regionNames(hk.Regions()); !reflect.DeepEqual(got, before) {
		t.Errorf("region keys after failed reload = %v, want the previous %v", got, before)
	}

	// A deleted file is just as unusable, and must also change nothing.
	if err := os.Remove(filepath.Join(dir, "regions.json")); err != nil {
		t.Fatal(err)
	}
	if err := hk.reload(cfgPath); err == nil {
		t.Fatal("reload with a missing hashRegions file should return an error")
	}
	if got := regionNames(hk.Regions()); !reflect.DeepEqual(got, before) {
		t.Errorf("region keys after failed reload = %v, want the previous %v", got, before)
	}
}

// End-to-end: a real SIGHUP re-reads the external file.
func TestStartSIGHUPReload_RereadsHashRegionsFile(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "dk")
	cfgPath := writeRegionsConfig(t, dir, nil, "regions.json")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	rk, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	hk := newHotKeys(loadChannelKeys(cfg, cfgPath), rk)
	stop := startSIGHUPReload(hk, cfgPath)
	defer stop()

	writeRegionsFile(t, dir, "regions.json", "dk", "dk-gamma")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := hk.Regions()["#dk-gamma"]; ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("region keys after SIGHUP = %v, want #dk-gamma within 2s", regionNames(hk.Regions()))
}

// The ingestor's region keys must cover exactly the shared loader's names
// for the shared fixture — the server asserts the same list against the
// same files, which is what keeps the two processes from disagreeing.
func TestLoadRegionKeys_ParityFixtureMatchesSharedLoader(t *testing.T) {
	t.Setenv(regions.PathEnvVar, "")
	cfgPath := copyParityFixture(t)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	keys, err := loadRegionKeys(cfg, cfgPath)
	if err != nil {
		t.Fatalf("loadRegionKeys: %v", err)
	}
	if want := sortedCopy(parityWantNames); !reflect.DeepEqual(regionNames(keys), want) {
		t.Errorf("region keys = %v, want %v", regionNames(keys), want)
	}

	res, err := regions.Load(cfg.HashRegions, cfg.HashRegionsPath, cfgPath, nil)
	if err != nil {
		t.Fatalf("regions.Load: %v", err)
	}
	if !reflect.DeepEqual(res.Names, parityWantNames) {
		t.Errorf("shared loader names = %v, want %v", res.Names, parityWantNames)
	}
}
