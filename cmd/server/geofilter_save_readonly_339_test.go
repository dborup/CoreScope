package main

import (
	"encoding/json"
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
