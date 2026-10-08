//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestSaveGeoFilterModeIsUmaskIndependent: the preserved mode must be applied
// explicitly, not left to the creation mode masked by the process umask. With
// umask 0077 a deliberately world-readable 0644 config would otherwise come
// back as 0600 and break anything reading it as another user; with umask 0022
// a 0600 config would come back as 0644 (#340).
//
// cmd/server tests never call t.Parallel, so flipping the process-wide umask
// for the duration of one save is safe here.
func TestSaveGeoFilterModeIsUmaskIndependent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		umask int
		mode  os.FileMode
	}{
		{"restrictive umask keeps 0644", 0o077, 0o644},
		{"permissive umask keeps 0600", 0o000, 0o600},
		{"default umask keeps 0600", 0o022, 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), tc.mode)

			prev := syscall.Umask(tc.umask)
			err := SaveGeoFilter(dir, geoPerm340Filter())
			syscall.Umask(prev)
			if err != nil {
				t.Fatalf("SaveGeoFilter: %v", err)
			}
			if got := geoPerm340Mode(t, path); got != tc.mode {
				t.Errorf("mode after save under umask %04o = %04o, want %04o", tc.umask, got, tc.mode)
			}
		})
	}
}

// TestSaveGeoFilterPreservesOwner documents and pins the owner/group behavior
// relevant to the non-root container migration (#339): the replacement keeps
// the original uid/gid whenever the saving process already owns the file, which
// is the only arrangement a non-root server can write in anyway. An
// unprivileged process cannot chown to a foreign uid, so the save must not try
// to and must not fail over it.
func TestSaveGeoFilterPreservesOwner(t *testing.T) {
	dir := t.TempDir()
	path := writeGeoPerm340Config(t, filepath.Join(dir, "config.json"), 0o600)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wantUID, wantGID, ok := configFileOwnerIDs(before)
	if !ok {
		t.Skip("owner IDs unavailable on this platform")
	}

	if err := SaveGeoFilter(dir, geoPerm340Filter()); err != nil {
		t.Fatalf("SaveGeoFilter: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	gotUID, gotGID, ok := configFileOwnerIDs(after)
	if !ok {
		t.Fatal("owner IDs unavailable after save")
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("owner after save = %d:%d, want %d:%d", gotUID, gotGID, wantUID, wantGID)
	}
}
