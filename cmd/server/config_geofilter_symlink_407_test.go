package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func requireGeo407Symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
}

func assertGeo407LinkUnchanged(t *testing.T, link, want string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("config link no longer exists as a symlink: %v", err)
	}
	if got != want {
		t.Fatalf("link target = %q, want %q", got, want)
	}
}

// A container can expose config.json via a link to its persistent data mount.
// Both saving and removing the geo-filter must update that target, keeping all
// link entries intact so the next container start reads the persisted value.
func TestSaveGeoFilterPersistsThroughConfigSymlinks407(t *testing.T) {
	for _, kind := range []string{"absolute", "relative", "chained", "symlinked directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			persistedDir := t.TempDir()
			target := writeGeoPerm340Config(t, filepath.Join(persistedDir, "persistent.json"), 0o640)
			link := filepath.Join(dir, "config.json")
			links := map[string]string{}
			switch kind {
			case "absolute":
				links[link] = target
			case "relative":
				rel, err := filepath.Rel(dir, target)
				if err != nil {
					t.Fatal(err)
				}
				links[link] = rel
			case "chained":
				middle := filepath.Join(dir, "intermediate.json")
				links[middle] = target
				links[link] = "intermediate.json"
			case "symlinked directory":
				dataLink := filepath.Join(dir, "data")
				links[dataLink] = persistedDir
				links[link] = filepath.Join("data", "persistent.json")
			}
			for path, dest := range links {
				requireGeo407Symlink(t, dest, path)
			}

			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			wantFields := geoPerm340Decode(t, []byte(geoPerm340Config))
			for _, gf := range []*GeoFilterConfig{geoPerm340Filter(), nil} {
				if err := SaveGeoFilter(dir, gf); err != nil {
					t.Fatalf("SaveGeoFilter: %v", err)
				}
				for path, dest := range links {
					assertGeo407LinkUnchanged(t, path, dest)
				}
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				got := geoPerm340Decode(t, data)
				filter, present := got["geo_filter"]
				if gf == nil {
					if present {
						t.Fatal("geo_filter still present in persistent target after removal")
					}
				} else {
					// Use the same JSON conversion as the pre-existing assertions,
					// so the saved value, not just its presence, is pinned.
					wantFilter := geoPerm340Decode(t, []byte(`{"polygon":[[51,4],[51,5],[50.5,4]],"bufferKm":20}`))
					if !reflect.DeepEqual(filter, wantFilter) {
						t.Fatalf("geo_filter in persistent target = %#v, want %#v", filter, wantFilter)
					}
				}
				delete(got, "geo_filter")
				if !reflect.DeepEqual(got, wantFields) {
					t.Fatal("unrelated config fields changed")
				}
				if mode := geoPerm340Mode(t, target); mode != 0o640 {
					t.Fatalf("persistent target mode = %04o, want 0640", mode)
				}
				after, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				wantUID, wantGID, ok := configFileOwnerIDs(before)
				if gotUID, gotGID, available := configFileOwnerIDs(after); ok && available && (gotUID != wantUID || gotGID != wantGID) {
					t.Fatalf("target owner changed from %d:%d to %d:%d", wantUID, wantGID, gotUID, gotGID)
				}
				entries, err := os.ReadDir(persistedDir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Name() != "persistent.json" {
					t.Fatalf("unexpected target-directory entries: %v", entries)
				}
			}
		})
	}
}

func TestSaveGeoFilterSymlinkUsesTargetDirectory407(t *testing.T) {
	dir := t.TempDir()
	persistedDir := t.TempDir()
	target := writeGeoPerm340Config(t, filepath.Join(persistedDir, "persistent.json"), 0o600)
	link := filepath.Join(dir, "config.json")
	requireGeo407Symlink(t, target, link)
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	resolvedDir := filepath.Dir(resolvedTarget)

	origCreate, origRename := configCreateTemp, configRename
	t.Cleanup(func() { configCreateTemp, configRename = origCreate, origRename })
	created, renamed := false, false
	configCreateTemp = func(d, pattern string) (*os.File, error) {
		if d != resolvedDir || pattern != "persistent.json.*.tmp" {
			t.Fatalf("temp destination = %q / %q, want resolved config directory and basename", d, pattern)
		}
		created = true
		return origCreate(d, pattern)
	}
	configRename = func(old, dest string) error {
		if filepath.Dir(old) != resolvedDir || dest != resolvedTarget {
			t.Fatalf("rename %q -> %q is not confined to the target directory", old, dest)
		}
		renamed = true
		return origRename(old, dest)
	}
	if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
		t.Fatal(err)
	}
	if !created || !renamed {
		t.Fatal("atomic temp/rename path was not exercised")
	}
	assertGeo407LinkUnchanged(t, link, target)
}

func TestSaveGeoFilterSymlinkTargetRenameFailure407(t *testing.T) {
	dir := t.TempDir()
	persistedDir := t.TempDir()
	target := writeGeoPerm340Config(t, filepath.Join(persistedDir, "persistent.json"), 0o600)
	link := filepath.Join(dir, "config.json")
	requireGeo407Symlink(t, target, link)
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("target rename refused")
	orig := configRename
	t.Cleanup(func() { configRename = orig })
	configRename = func(_, dest string) error {
		if dest != resolvedTarget {
			t.Fatalf("rename destination = %q, want target %q", dest, resolvedTarget)
		}
		return sentinel
	}
	if err := SaveGeoFilter(dir, geoPerm340Filter()); !errors.Is(err, sentinel) {
		t.Fatalf("save error = %v, want wrapped rename error", err)
	}
	assertGeo407LinkUnchanged(t, link, target)
	data, err := os.ReadFile(target)
	if err != nil || string(data) != geoPerm340Config {
		t.Fatalf("target changed on failed save: %q, %v", data, err)
	}
	entries, err := os.ReadDir(persistedDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "persistent.json" {
		t.Fatalf("failed save left temporary files: %v, %v", entries, err)
	}
}

func TestSaveGeoFilterSymlinkUnwritableTargetDirectory407(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	dir := t.TempDir()
	persistedDir := t.TempDir()
	target := writeGeoPerm340Config(t, filepath.Join(persistedDir, "persistent.json"), 0o600)
	link := filepath.Join(dir, "config.json")
	requireGeo407Symlink(t, target, link)
	if err := os.Chmod(persistedDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(persistedDir, 0o700) })
	if err := SaveGeoFilter(dir, geoPerm340Filter()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("save error = %v, want target directory permission error", err)
	}
	assertGeo407LinkUnchanged(t, link, target)
	data, err := os.ReadFile(target)
	if err != nil || string(data) != geoPerm340Config {
		t.Fatalf("target changed despite read-only directory: %q, %v", data, err)
	}
}

func TestSaveGeoFilterDanglingAndLoopSymlinks407(t *testing.T) {
	for _, kind := range []string{"dangling", "loop"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			link := filepath.Join(dir, "config.json")
			dest := "missing.json"
			if kind == "loop" {
				dest = "config.json"
			}
			requireGeo407Symlink(t, dest, link)
			if err := SaveGeoFilter(dir, geoPerm340Filter()); err == nil {
				t.Fatal("expected invalid symlink to fail, got nil")
			}
			assertGeo407LinkUnchanged(t, link, dest)
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
				t.Fatalf("invalid link save created files: %v, %v", entries, err)
			}
		})
	}
}

func TestSaveGeoFilterDanglingSymlinkKeepsDataFallback407(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config.json")
	requireGeo407Symlink(t, "missing.json", link)
	target := writeGeoPerm340Config(t, filepath.Join(dir, "data", "config.json"), 0o600)
	if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
		t.Fatal(err)
	}
	assertGeo407LinkUnchanged(t, link, "missing.json")
	data, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(data), "geo_filter") {
		t.Fatalf("data fallback was not updated: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target was created: %v", err)
	}
}
