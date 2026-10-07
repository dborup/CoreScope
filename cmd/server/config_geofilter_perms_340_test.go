package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Tests for #340: SaveGeoFilter must replace config.json atomically without
// widening its permissions and without ever writing through a pre-existing
// temporary path. config.json can hold credentials, so a deliberate 0600 must
// survive a geo-filter save.

const geoPerm340Config = `{
  "_comment": "keep me",
  "port": 3000,
  "mqtt": {"username": "redacted-test-user", "password": "redacted-test-secret"},
  "areas": [{"label": "A", "polygon": [[1, 2], [3, 4], [5, 6]]}]
}
`

func geoPerm340Filter() *GeoFilterConfig {
	return &GeoFilterConfig{
		Polygon:  [][2]float64{{51.0, 4.0}, {51.0, 5.0}, {50.5, 4.0}},
		BufferKm: 20,
	}
}

// writeGeoPerm340Config writes the fixture config at path with mode and
// returns path. The explicit chmod defeats the test process umask so the
// recorded starting mode is exact.
func writeGeoPerm340Config(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(geoPerm340Config), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func geoPerm340Mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// assertNoStrayTemp fails if dir holds any entry whose name looks like a
// temporary file left behind by SaveGeoFilter. Entries listed in keep are
// allowed (pre-existing files the save must not touch).
func assertNoStrayTemp(t *testing.T, dir string, keep ...string) {
	t.Helper()
	allowed := map[string]bool{"config.json": true}
	for _, k := range keep {
		allowed[k] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if allowed[e.Name()] {
			continue
		}
		if strings.Contains(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), "config.json.") {
			t.Errorf("leftover temp file %q in %s", e.Name(), dir)
		}
	}
}

// TestSaveGeoFilterPreservesMode is the core #340 regression: the saved file
// keeps the permission bits it had, both for a locked-down 0600 config and for
// an intentionally group/world-readable one.
func TestSaveGeoFilterPreservesMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o644, 0o664} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), mode)

			if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
				t.Fatalf("SaveGeoFilter: %v", err)
			}
			if got := geoPerm340Mode(t, path); got != mode {
				t.Errorf("mode after save = %04o, want %04o", got, mode)
			}

			// A removal save must preserve the mode too.
			if err := SaveGeoFilter(dir, nil); err != nil {
				t.Fatalf("SaveGeoFilter(nil): %v", err)
			}
			if got := geoPerm340Mode(t, path); got != mode {
				t.Errorf("mode after removal save = %04o, want %04o", got, mode)
			}
			assertNoStrayTemp(t, dir)
		})
	}
}

// TestSaveGeoFilterFailsSafelyOnUnreadableConfig covers the "handle metadata /
// read failures safely rather than silently choosing permissive defaults"
// criterion: an unreadable config must surface an error and stay untouched.
func TestSaveGeoFilterFailsSafelyOnUnreadableConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	dir := t.TempDir()
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o000)

	err := SaveGeoFilter(dir, geoPerm340Filter())
	if err == nil {
		t.Fatal("expected an error for an unreadable config, got nil")
	}
	if got := geoPerm340Mode(t, path); got != 0o000 {
		t.Errorf("mode after failed save = %04o, want 0000", got)
	}
	assertNoStrayTemp(t, dir)
}

// TestSaveGeoFilterDanglingSymlinkConfigFails: when the only config.json is a
// dangling symlink, the lookup must report "not found" instead of creating a
// fresh file through the link with default permissions.
func TestSaveGeoFilterDanglingSymlinkConfigFails(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(filepath.Join(dir, "missing.json"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := SaveGeoFilter(dir, geoPerm340Filter()); err == nil {
		t.Fatal("expected an error for a dangling config.json symlink, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink target was created: stat err = %v", err)
	}
}

// TestSaveGeoFilterIgnoresPreexistingTempPath: a stale config.json.tmp — here a
// directory, which no write can ever succeed through — must not block the save,
// because the temp file is created under a unique name.
func TestSaveGeoFilterIgnoresPreexistingTempPath(t *testing.T) {
	dir := t.TempDir()
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
	stale := path + ".tmp"
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
		t.Fatalf("SaveGeoFilter: %v", err)
	}
	if got := geoPerm340Mode(t, path); got != 0o600 {
		t.Errorf("mode after save = %04o, want 0600", got)
	}
	fi, err := os.Stat(stale)
	if err != nil || !fi.IsDir() {
		t.Errorf("pre-existing %s was disturbed: dir=%v err=%v", filepath.Base(stale), err == nil && fi.IsDir(), err)
	}
	assertNoStrayTemp(t, dir, "config.json.tmp")
}

// TestSaveGeoFilterDoesNotFollowTempSymlink: writing through a planted
// config.json.tmp symlink would copy config contents — credentials included —
// to an attacker-chosen path.
func TestSaveGeoFilterDoesNotFollowTempSymlink(t *testing.T) {
	dir := t.TempDir()
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim.txt")
	const victimBody = "victim-untouched\n"
	if err := os.WriteFile(victim, []byte(victimBody), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
	link := path + ".tmp"
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
		t.Fatalf("SaveGeoFilter: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != victimBody {
		t.Errorf("symlink target was written through: %q", string(got))
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("planted symlink was replaced: mode=%v err=%v", fi, err)
	}
	if got := geoPerm340Mode(t, path); got != 0o600 {
		t.Errorf("mode after save = %04o, want 0600", got)
	}
}

// TestSaveGeoFilterDoesNotFollowTempHardLink is the same leak via a hard link,
// which no O_NOFOLLOW flag can catch — only a unique temp name can.
func TestSaveGeoFilterDoesNotFollowTempHardLink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	const victimBody = "victim-untouched\n"
	if err := os.WriteFile(victim, []byte(victimBody), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
	if err := os.Link(victim, path+".tmp"); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}

	if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
		t.Fatalf("SaveGeoFilter: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != victimBody {
		t.Errorf("hard-linked file was written through: %q", string(got))
	}
}

// TestSaveGeoFilterCreateTempFailure: when the destination directory is not
// writable the save must fail loudly and leave the config untouched.
func TestSaveGeoFilterCreateTempFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "cfg")
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := SaveGeoFilter(dir, geoPerm340Filter()); err == nil {
		t.Fatal("expected an error when the config directory is read-only, got nil")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != geoPerm340Config {
		t.Error("config was modified despite the failed save")
	}
}

// TestSaveGeoFilterWriteFailureCleansUpOwnTemp exercises a failure after the
// temp file exists on disk but before the rename, injected through the
// create-temp seam: the temp file must be removed, a pre-existing
// config.json.tmp must survive, and the config must be unchanged.
func TestSaveGeoFilterWriteFailureCleansUpOwnTemp(t *testing.T) {
	dir := t.TempDir()
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
	stale := path + ".tmp"
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := configCreateTemp
	t.Cleanup(func() { configCreateTemp = orig })
	configCreateTemp = func(d, pattern string) (*os.File, error) {
		f, err := orig(d, pattern)
		if err != nil {
			return nil, err
		}
		// A closed descriptor makes the first operation on it fail with
		// ErrClosed while the temp file itself stays on disk, so cleanup is
		// observable.
		_ = f.Close()
		return f, nil
	}

	err := SaveGeoFilter(dir, geoPerm340Filter())
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("SaveGeoFilter error = %v, want it to wrap %v", err, os.ErrClosed)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read config: %v", rerr)
	}
	if string(data) != geoPerm340Config {
		t.Error("config was modified despite the failed save")
	}
	if got := geoPerm340Mode(t, path); got != 0o600 {
		t.Errorf("mode after failed save = %04o, want 0600", got)
	}
	if got, rerr := os.ReadFile(stale); rerr != nil || string(got) != "stale\n" {
		t.Errorf("pre-existing temp file was touched: %q err=%v", string(got), rerr)
	}
	assertNoStrayTemp(t, dir, "config.json.tmp")
}

// TestSaveGeoFilterRenameFailureCleansUpOwnTemp: a failing rename must not
// leave the half-written replacement behind.
func TestSaveGeoFilterRenameFailureCleansUpOwnTemp(t *testing.T) {
	dir := t.TempDir()
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
	stale := path + ".tmp"
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := configRename
	t.Cleanup(func() { configRename = orig })
	sentinel := errors.New("rename refused by test")
	configRename = func(string, string) error { return sentinel }

	err := SaveGeoFilter(dir, geoPerm340Filter())
	if !errors.Is(err, sentinel) {
		t.Fatalf("SaveGeoFilter error = %v, want it to wrap %v", err, sentinel)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read config: %v", rerr)
	}
	if string(data) != geoPerm340Config {
		t.Error("config was modified despite the failed rename")
	}
	if got, rerr := os.ReadFile(stale); rerr != nil || string(got) != "stale\n" {
		t.Errorf("pre-existing temp file was touched: %q err=%v", string(got), rerr)
	}
	assertNoStrayTemp(t, dir, "config.json.tmp")
}

// geoPerm340Decode decodes JSON into a shape comparable with
// reflect.DeepEqual, so assertions are about values and not about indentation.
func geoPerm340Decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, b)
	}
	return m
}

// TestSaveGeoFilterPreservesUnrelatedFields: only geo_filter may change, and
// the permission-preserving rewrite must not cost us any of the other keys —
// including ones the Config struct does not model, such as _comment.
func TestSaveGeoFilterPreservesUnrelatedFields(t *testing.T) {
	dir := t.TempDir()
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)

	if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
		t.Fatalf("SaveGeoFilter: %v", err)
	}
	if got := geoPerm340Mode(t, path); got != 0o600 {
		t.Errorf("mode after save = %04o, want 0600", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := geoPerm340Decode(t, raw)
	if _, ok := got["geo_filter"]; !ok {
		t.Error("geo_filter missing after save")
	}
	want := geoPerm340Decode(t, []byte(geoPerm340Config))
	for key, wantVal := range want {
		gotVal, ok := got[key]
		if !ok {
			t.Errorf("key %q missing after save", key)
			continue
		}
		if !reflect.DeepEqual(gotVal, wantVal) {
			t.Errorf("key %q changed: got %#v, want %#v", key, gotVal, wantVal)
		}
	}

	// A removal save must keep the same keys minus geo_filter.
	if err := SaveGeoFilter(dir, nil); err != nil {
		t.Fatalf("SaveGeoFilter(nil): %v", err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after := geoPerm340Decode(t, raw)
	if _, ok := after["geo_filter"]; ok {
		t.Error("geo_filter still present after removal save")
	}
	delete(got, "geo_filter")
	if !reflect.DeepEqual(after, got) {
		t.Errorf("removal save changed unrelated fields:\ngot  %#v\nwant %#v", after, got)
	}
	if got := geoPerm340Mode(t, path); got != 0o600 {
		t.Errorf("mode after removal save = %04o, want 0600", got)
	}
}

// TestSaveGeoFilterStatFailureIsFatal pins the fail-safe half of the mode
// preservation: if the config's metadata cannot be read, the save must report
// the failure rather than fall back to a permissive default mode.
func TestSaveGeoFilterStatFailureIsFatal(t *testing.T) {
	dir := t.TempDir()
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)

	orig := configStatFile
	t.Cleanup(func() { configStatFile = orig })
	sentinel := errors.New("stat refused by test")
	first := true
	configStatFile = func(f *os.File) (os.FileInfo, error) {
		if first {
			first = false
			return nil, sentinel
		}
		return orig(f)
	}

	err := SaveGeoFilter(dir, geoPerm340Filter())
	if !errors.Is(err, sentinel) {
		t.Fatalf("SaveGeoFilter error = %v, want it to wrap %v", err, sentinel)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read config: %v", rerr)
	}
	if string(data) != geoPerm340Config {
		t.Error("config was modified despite unreadable metadata")
	}
	if got := geoPerm340Mode(t, path); got != 0o600 {
		t.Errorf("mode after failed save = %04o, want 0600", got)
	}
	assertNoStrayTemp(t, dir)
}

// TestSaveGeoFilterConfigLocations covers both supported lookup locations and
// their precedence, asserting mode preservation in each.
func TestSaveGeoFilterConfigLocations(t *testing.T) {
	t.Run("top level", func(t *testing.T) {
		dir := t.TempDir()
		path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
		if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
			t.Fatalf("SaveGeoFilter: %v", err)
		}
		if got := geoPerm340Mode(t, path); got != 0o600 {
			t.Errorf("mode = %04o, want 0600", got)
		}
		assertNoStrayTemp(t, dir)
	})

	t.Run("data subdirectory", func(t *testing.T) {
		dir := t.TempDir()
		path := writeGeoPerm340Config(t, filepath.Join(dir, "data", "config.json"), 0o600)
		if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
			t.Fatalf("SaveGeoFilter: %v", err)
		}
		if got := geoPerm340Mode(t, path); got != 0o600 {
			t.Errorf("mode = %04o, want 0600", got)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "geo_filter") {
			t.Error("geo_filter not written to data/config.json")
		}
		// The temp file must land next to the config it replaces, not in the
		// parent directory, or the rename would cross filesystems on a mount.
		assertNoStrayTemp(t, filepath.Join(dir, "data"))
		assertNoStrayTemp(t, dir, "data")
	})

	t.Run("top level wins over data subdirectory", func(t *testing.T) {
		dir := t.TempDir()
		top := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)
		nested := writeGeoPerm340Config(t, filepath.Join(dir, "data", "config.json"), 0o644)
		if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
			t.Fatalf("SaveGeoFilter: %v", err)
		}
		if got := geoPerm340Mode(t, top); got != 0o600 {
			t.Errorf("top-level mode = %04o, want 0600", got)
		}
		body, err := os.ReadFile(nested)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != geoPerm340Config {
			t.Error("data/config.json was written when the top-level config exists")
		}
	})
}
