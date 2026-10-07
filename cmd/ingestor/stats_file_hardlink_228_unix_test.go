//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A hard link at the tmp path is a regular file of the ingestor's user, so
// it passed the type and owner checks: the writer truncated and overwrote
// the link target and published it by the rename. It is now refused and
// left in place, and the target is untouched (#228).
func TestWriteStatsAtomicRefusesHardLinkedTmp_228(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	target := filepath.Join(dir, "precious")
	const content = "precious content"
	if err := os.WriteFile(target, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil { // past the umask
		t.Fatal(err)
	}
	if err := os.Link(target, path+".tmp"); err != nil {
		t.Skipf("link: %v", err)
	}

	err := writeStatsAtomic(path, []byte(`{}`))
	if err == nil {
		t.Fatal("hard-linked tmp accepted")
	}
	want := path + ".tmp: hard-linked (nlink 2); remove it"
	if err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != content {
		t.Errorf("link target changed: %q, %v", got, err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("link target mode changed: %v, %v", fi.Mode(), err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("stats file published from a hard link: lstat err %v", err)
	}
	if _, err := os.Lstat(path + ".tmp"); err != nil {
		t.Errorf("hard-linked tmp not left in place: %v", err)
	}
}
