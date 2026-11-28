package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// #339: once the Go services can run without root, config.json is read-only
// to the server (it never gets write access to /app, and a read-only config
// mount is supported). A geo-filter save must then fail with a message that
// names the arrangement — not the generic "failed to save config" — and must
// not apply the filter in memory while the file on disk says otherwise.

const geoReadOnly339Key = "a-strong-api-key-for-testing"
const geoReadOnly339Body = `{"polygon":[[51.0,4.0],[51.0,5.0],[50.5,5.0],[50.5,4.0]],"bufferKm":15}`

func putGeoFilter339(t *testing.T, router http.Handler) (int, string) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/config/geo-filter", strings.NewReader(geoReadOnly339Body))
	req.Header.Set("X-API-Key", geoReadOnly339Key)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Error
}

func assertReadOnlyGeoFailure339(t *testing.T, srv *Server, code int, msg string) {
	t.Helper()
	if code != http.StatusConflict {
		t.Fatalf("status = %d (%q), want 409", code, msg)
	}
	for _, want := range []string{"geo filter not saved", "config.json", "read-only"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if srv.cfg.GeoFilter != nil {
		t.Error("geo filter was applied in memory although config.json was not written")
	}
}

func TestPutGeoFilterReadOnlyConfigDirIsClear339(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	srv, router, dir := setupGeoFilterServer(t, geoReadOnly339Key)
	before, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	code, msg := putGeoFilter339(t, router)
	assertReadOnlyGeoFailure339(t, srv, code, msg)
	after, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("config.json changed despite the failed save")
	}
}

func TestPutGeoFilterReadOnlyErrnosAreClear339(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
		stage string
	}{
		{"EROFS on create", syscall.EROFS, "create"},
		{"EACCES on create", syscall.EACCES, "create"},
		{"EPERM on create", syscall.EPERM, "create"},
		{"EROFS on rename", syscall.EROFS, "rename"},
		{"EACCES on rename", syscall.EACCES, "rename"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router, _ := setupGeoFilterServer(t, geoReadOnly339Key)
			origCreate, origRename := configCreateTemp, configRename
			t.Cleanup(func() { configCreateTemp, configRename = origCreate, origRename })
			if tc.stage == "create" {
				configCreateTemp = func(dir, pattern string) (*os.File, error) {
					return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, pattern), Err: tc.errno}
				}
			} else {
				configRename = func(oldpath, newpath string) error {
					return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: tc.errno}
				}
			}
			code, msg := putGeoFilter339(t, router)
			assertReadOnlyGeoFailure339(t, srv, code, msg)
		})
	}
}

// Failures that are not about the arrangement keep the historical response.
func TestPutGeoFilterOtherSaveErrorsUnchanged339(t *testing.T) {
	srv, router, _ := setupGeoFilterServer(t, geoReadOnly339Key)
	orig := configCreateTemp
	t.Cleanup(func() { configCreateTemp = orig })
	configCreateTemp = func(dir, pattern string) (*os.File, error) {
		return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, pattern), Err: syscall.ENOSPC}
	}
	code, msg := putGeoFilter339(t, router)
	if code != http.StatusInternalServerError || msg != "failed to save config" {
		t.Fatalf("got %d %q, want 500 \"failed to save config\"", code, msg)
	}
	if srv.cfg.GeoFilter != nil {
		t.Error("geo filter was applied in memory although the save failed")
	}
}

// An unreadable config is not a read-only arrangement: it fails before any
// write and keeps the generic response.
func TestPutGeoFilterUnreadableConfigNotReportedAsReadOnly339(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	_, router, dir := setupGeoFilterServer(t, geoReadOnly339Key)
	path := filepath.Join(dir, "config.json")
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	code, msg := putGeoFilter339(t, router)
	if code != http.StatusInternalServerError || msg != "failed to save config" {
		t.Fatalf("got %d %q, want 500 \"failed to save config\"", code, msg)
	}
}

// In the container's non-root mode config.json belongs to the host or root
// and the services must never replace it (#339 review, finding 2). The 409
// must therefore not depend on where the file lives or on which directory
// happens to be writable: here every path is writable for the test user, the
// config is reached through a symlink into a writable data directory (the
// container layout, and what #422 resolves), and the save must still be
// refused without creating, renaming or changing anything.
func TestPutGeoFilterNonRootModeRefusesWritableConfig339(t *testing.T) {
	for _, setting := range []string{"1", "true"} {
		t.Run(setting, func(t *testing.T) {
			t.Setenv("CORESCOPE_RUN_AS_NONROOT", setting)
			srv, router, dir := setupGeoFilterServer(t, geoReadOnly339Key)
			data := filepath.Join(dir, "data")
			if err := os.Mkdir(data, 0o777); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(data, "config.json")
			if err := os.Rename(filepath.Join(dir, "config.json"), target); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o666); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "config.json")); err != nil {
				t.Fatal(err)
			}
			before := snapshotDir339(t, dir, data)
			origCreate, origRename := configCreateTemp, configRename
			t.Cleanup(func() { configCreateTemp, configRename = origCreate, origRename })
			configCreateTemp = func(string, string) (*os.File, error) {
				t.Error("SaveGeoFilter created a temp file in non-root mode")
				return nil, syscall.EIO
			}
			configRename = func(string, string) error {
				t.Error("SaveGeoFilter renamed in non-root mode")
				return syscall.EIO
			}

			code, msg := putGeoFilter339(t, router)
			assertReadOnlyGeoFailure339(t, srv, code, msg)
			if after := snapshotDir339(t, dir, data); after != before {
				t.Errorf("files changed despite the refused save:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// The default (setting off) keeps saving a writable config exactly as before.
func TestPutGeoFilterSettingOffStillSaves339(t *testing.T) {
	for _, setting := range []string{"", "0", "false"} {
		t.Run("value="+setting, func(t *testing.T) {
			t.Setenv("CORESCOPE_RUN_AS_NONROOT", setting)
			srv, router, dir := setupGeoFilterServer(t, geoReadOnly339Key)
			code, msg := putGeoFilter339(t, router)
			if code != http.StatusOK {
				t.Fatalf("status = %d (%q), want 200", code, msg)
			}
			if srv.cfg.GeoFilter == nil {
				t.Error("geo filter not applied")
			}
			b, err := os.ReadFile(filepath.Join(dir, "config.json"))
			if err != nil || !strings.Contains(string(b), "geo_filter") {
				t.Errorf("config.json not saved: %v %s", err, b)
			}
		})
	}
}

// snapshotDir339 lists name, mode, size, inode and content of every entry in
// the given directories, without following symlinks.
func snapshotDir339(t *testing.T, dirs ...string) string {
	t.Helper()
	var sb strings.Builder
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p := filepath.Join(d, e.Name())
			fi, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			st := fi.Sys().(*syscall.Stat_t)
			fmt.Fprintf(&sb, "%s %v %d %d;", p, fi.Mode(), fi.Size(), st.Ino)
			if fi.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				sb.Write(b)
			}
		}
	}
	return sb.String()
}
