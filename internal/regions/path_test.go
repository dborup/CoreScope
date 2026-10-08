package regions

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeNames drops a JSON array of region names at dir/name and returns the
// full path.
func writeNames(t *testing.T, dir, name string, names ...string) string {
	t.Helper()
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("["+strings.Join(quoted, ",")+"]"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// chdir moves the process into dir for the duration of the test. The
// resolution rule is "relative to the config file", so a test has to prove
// it from a working directory that has nothing to do with the config.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// recorder collects the loader's log lines so a test can assert the
// "log once" promise instead of a per-entry flood.
type recorder struct{ lines []string }

func (r *recorder) logf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) count(substr string) int {
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func TestConfiguredPathEnvOverrideWins(t *testing.T) {
	t.Setenv(PathEnvVar, "/etc/corescope/regions.json")
	if got := ConfiguredPath("config-value.json"); got != "/etc/corescope/regions.json" {
		t.Errorf("ConfiguredPath = %q, want the HASH_REGIONS_PATH value", got)
	}
}

func TestConfiguredPathFallsBackToConfigKey(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	if got := ConfiguredPath("  regions.json  "); got != "regions.json" {
		t.Errorf("ConfiguredPath = %q, want the trimmed config key", got)
	}
	if got := ConfiguredPath("   "); got != "" {
		t.Errorf("ConfiguredPath(blank) = %q, want \"\" (no file)", got)
	}
}

// A blank env var must not shadow a real config key.
func TestConfiguredPathBlankEnvIsNotAnOverride(t *testing.T) {
	t.Setenv(PathEnvVar, "   ")
	if got := ConfiguredPath("regions.json"); got != "regions.json" {
		t.Errorf("ConfiguredPath = %q, want the config key to survive a blank env var", got)
	}
}

// The documented rule: a relative hashRegionsPath is anchored at the
// directory holding the config file it came from, never at the process CWD,
// so the ingestor and the server resolve it identically.
func TestResolvePathRelativeToConfigDir(t *testing.T) {
	if got := ResolvePath("regions.json", "/srv/app/config.json"); got != "/srv/app/regions.json" {
		t.Errorf("ResolvePath = %q, want /srv/app/regions.json", got)
	}
	if got := ResolvePath("lists/regions.json", "/srv/app/data/config.json"); got != "/srv/app/data/lists/regions.json" {
		t.Errorf("ResolvePath = %q, want /srv/app/data/lists/regions.json", got)
	}
	if got := ResolvePath("/abs/regions.json", "/srv/app/config.json"); got != "/abs/regions.json" {
		t.Errorf("ResolvePath = %q, want the absolute path untouched", got)
	}
	if got := ResolvePath("regions.json", "config.json"); got != "regions.json" {
		t.Errorf("ResolvePath = %q, want regions.json when the config has no directory", got)
	}
	if got := ResolvePath("", "/srv/app/config.json"); got != "" {
		t.Errorf("ResolvePath(\"\") = %q, want \"\" (no file configured)", got)
	}
}

// Relative resolution must actually find the file from an unrelated CWD.
func TestLoadRelativePathFoundFromAnyWorkingDir(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	writeNames(t, dir, "regions.json", "dk-aarhus")
	chdir(t, t.TempDir())

	res, err := Load([]string{"#dk"}, "regions.json", filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"#dk", "#dk-aarhus"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want %v", res.Names, want)
	}
}

// No path configured means no file is read — there is no auto-discovered
// default, so a file sitting next to config.json must be ignored.
func TestLoadWithoutPathReadsNoFile(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	writeNames(t, dir, "hash-regions.json", "sneaky")

	res, err := Load([]string{"dk"}, "", filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Path != "" {
		t.Errorf("Path = %q, want \"\" when no hashRegionsPath is configured", res.Path)
	}
	if res.FileNames != nil {
		t.Errorf("FileNames = %v, want nil", res.FileNames)
	}
	if want := []string{"#dk"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want %v (inline only)", res.Names, want)
	}
}

func TestLoadEnvOverrideWinsOverConfigKey(t *testing.T) {
	dir := t.TempDir()
	fromEnv := writeNames(t, dir, "env.json", "from-env")
	fromCfg := writeNames(t, dir, "cfg.json", "from-cfg")
	t.Setenv(PathEnvVar, fromEnv)

	res, err := Load(nil, fromCfg, filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"#from-env"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want %v", res.Names, want)
	}
	if res.Path != fromEnv {
		t.Errorf("Path = %q, want %q", res.Path, fromEnv)
	}
}

func TestLoadMissingFileFallsBackToInline(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")

	rec := &recorder{}
	res, err := Load([]string{"dk", "eu"}, missing, filepath.Join(dir, "config.json"), rec.logf)
	if err == nil {
		t.Fatal("Load should report an error for a missing hashRegionsPath file")
	}
	if want := []string{"#dk", "#eu"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want the inline list %v as fallback", res.Names, want)
	}
	if rec.count("nope.json") == 0 {
		t.Errorf("want a logged error naming the unusable file, got %v", rec.lines)
	}
}

func TestLoadInvalidJSONFallsBackToInline(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "regions.json")
	if err := os.WriteFile(path, []byte(`{"dk": true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	res, err := Load([]string{"dk"}, path, filepath.Join(dir, "config.json"), rec.logf)
	if err == nil {
		t.Fatal("Load should report an error for a JSON object (the file must be an array of names)")
	}
	if want := []string{"#dk"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want the inline list %v as fallback", res.Names, want)
	}
	if rec.count("regions.json") == 0 {
		t.Errorf("want a logged error naming the unusable file, got %v", rec.lines)
	}
}

func TestLoadNotAStringArrayIsInvalid(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "regions.json")
	if err := os.WriteFile(path, []byte(`[1, 2, 3]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(nil, path, filepath.Join(dir, "config.json"), nil); err == nil {
		t.Fatal("Load should reject a JSON array of non-strings")
	}
}

func TestLoadSkipsEmptyAndBlankEntries(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	path := writeNames(t, dir, "regions.json", "", "   ", "dk-fyn", "\t")

	rec := &recorder{}
	res, err := Load([]string{"", "  ", "dk"}, path, filepath.Join(dir, "config.json"), rec.logf)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"#dk", "#dk-fyn"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want %v", res.Names, want)
	}
	if res.Blank != 5 {
		t.Errorf("Blank = %d, want 5 (2 inline + 3 file)", res.Blank)
	}
	if got := rec.count("blank"); got != 1 {
		t.Errorf("blank entries logged %d times, want exactly 1 aggregate line: %v", got, rec.lines)
	}
}

// The merge rule: inline first, then the file, deduplicated after
// normalization. "dk" and "#dk" are the same scope; "#DK" is NOT — the
// ingestor's key is SHA256 of the exact name, so folding case here would
// silently drop a region whose key differs.
func TestLoadMergesInlineAndFileWithOverlap(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	path := writeNames(t, dir, "regions.json", "dk", "#eu", "dk-aarhus", "#DK")

	rec := &recorder{}
	res, err := Load([]string{"#dk", "eu"}, path, filepath.Join(dir, "config.json"), rec.logf)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"#dk", "#eu", "#dk-aarhus", "#DK"}
	if !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want %v", res.Names, want)
	}
	if !reflect.DeepEqual(res.Duplicates, []string{"#dk", "#eu"}) {
		t.Errorf("Duplicates = %v, want [#dk #eu]", res.Duplicates)
	}
	if got := rec.count("duplicate"); got != 1 {
		t.Errorf("duplicates logged %d times, want exactly 1 aggregate line: %v", got, rec.lines)
	}
	if !reflect.DeepEqual(res.FileNames, []string{"dk", "#eu", "dk-aarhus", "#DK"}) {
		t.Errorf("FileNames = %v, want the raw file entries", res.FileNames)
	}
}

// Merge is the one union rule both processes reuse, so an inline-only
// deployment keeps exactly today's behaviour.
func TestMergeInlineOnlyMatchesNormalizeNames(t *testing.T) {
	inline := []string{"#belgium", "#unused-region", "noHashPrefix", "#belgium", ""}
	if got, want := Merge(inline, nil), NormalizeNames(inline); !reflect.DeepEqual(got, want) {
		t.Errorf("Merge(inline, nil) = %v, want NormalizeNames(inline) = %v", got, want)
	}
}

func TestMergeFileOnly(t *testing.T) {
	if got, want := Merge(nil, []string{"dk", "dk"}), []string{"#dk"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

// Load's Names must be exactly Merge(inline, file): the server recomputes
// the union from the cached file entries, and it may not drift from what
// the ingestor derived its keys from.
func TestLoadNamesEqualMerge(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	path := writeNames(t, dir, "regions.json", "dk-fyn", "", "#eu")

	res, err := Load([]string{"dk", "eu"}, path, filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := Merge([]string{"dk", "eu"}, res.FileNames); !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want Merge(...) = %v", res.Names, want)
	}
}

// An empty JSON array is valid and simply contributes nothing.
func TestLoadEmptyArrayIsNotAnError(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	path := writeNames(t, dir, "regions.json")

	res, err := Load([]string{"dk"}, path, filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"#dk"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want %v", res.Names, want)
	}
}

// Load must tolerate a nil logf (callers that do their own reporting).
func TestLoadNilLogfIsSafe(t *testing.T) {
	t.Setenv(PathEnvVar, "")
	dir := t.TempDir()
	if _, err := Load([]string{"", "dk", "dk"}, filepath.Join(dir, "gone.json"), "", nil); err == nil {
		t.Fatal("want an error for the missing file")
	}
}
