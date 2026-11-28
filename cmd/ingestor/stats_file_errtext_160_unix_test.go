//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PR #216 review, F4: a stats write error named the path up to three times,
// e.g. "<tmp> is not a regular file (mode …); remove it: open <tmp>: …" in
// a "[stats-file] write <path>: …" line. Each error now names the tmp once,
// at the start, and the writer's failure line adds no second copy.
func TestStatsWriteErrorNamesThePathOnce_160(t *testing.T) {
	// setup returns the FIFO it planted at the tmp path, if any, so a write
	// that blocks on it can be released (#161).
	type tc struct {
		name  string
		setup func(t *testing.T, path string) (fifo string)
		want  string
	}
	foreign := func(t *testing.T) {
		old := statsFileEUID
		t.Cleanup(func() { statsFileEUID = old })
		statsFileEUID = func() int { return os.Geteuid() + 1 }
	}
	cases := []tc{
		{"FIFO without a reader", func(t *testing.T, path string) string { return mkStatsTmpFIFO(t, path) }, "not a regular file"},
		{"directory", func(t *testing.T, path string) string {
			if err := os.Mkdir(path+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
			return ""
		}, "not a regular file"},
		{"foreign owner", func(t *testing.T, path string) string {
			if err := os.WriteFile(path+".tmp", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			foreign(t)
			return ""
		}, "remove it or fix its owner"},
		{"foreign owner, unopenable", func(t *testing.T, path string) string {
			if os.Geteuid() == 0 {
				t.Skip("root opens a 0400 file for writing")
			}
			if err := os.WriteFile(path+".tmp", nil, 0o400); err != nil {
				t.Fatal(err)
			}
			foreign(t)
			return ""
		}, "remove it or fix its owner"},
		// The rename fails on a non-empty directory at the stats path; its
		// *os.LinkError names both paths (#228).
		{"rename failure", func(t *testing.T, path string) string {
			if err := os.MkdirAll(filepath.Join(path, "keep"), 0o700); err != nil {
				t.Fatal(err)
			}
			return ""
		}, "rename to stats.json: "},
		{"hard link", func(t *testing.T, path string) string {
			target := filepath.Join(filepath.Dir(path), "other")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, path+".tmp"); err != nil {
				t.Skipf("link: %v", err)
			}
			return ""
		}, "hard-linked (nlink 2); remove it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "stats.json")
			var err error
			if fifo := c.setup(t, path); fifo != "" {
				err = writeStatsAtomicOrRelease(t, path, fifo)
			} else {
				err = writeStatsAtomic(path, []byte(`{}`))
			}
			if err == nil {
				t.Fatal("accepted")
			}
			msg := err.Error()
			if n := strings.Count(msg, dir); n != 1 {
				t.Errorf("path named %d times in %q, want once", n, msg)
			}
			if !strings.HasPrefix(msg, path+".tmp: ") {
				t.Errorf("%q does not start with the tmp path", msg)
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("%q lacks %q", msg, c.want)
			}
		})
	}
}

// The writer's failure line names the path once too.
func TestStatsFileWriterFailureLineNamesThePathOnce_160(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	t.Setenv("CORESCOPE_INGESTOR_STATS", path)
	fifo := mkStatsTmpFIFO(t, path)
	logs := captureStdLog(t)
	store := openStatsTestStore(t, dir)
	stop := StartStatsFileWriter(store, 5*time.Millisecond)
	// Guarded: a writer blocked on the FIFO (#161) fails the test, not
	// hangs it, also when the test ends early.
	t.Cleanup(func() { stopStatsWriterOrRelease(t, stop, fifo) })

	deadline := time.Now().Add(3 * time.Second)
	for len(logs.linesWith("[stats-file] write")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no failure line")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopStatsWriterOrRelease(t, stop, fifo)
	line := logs.linesWith("[stats-file] write")[0]
	if n := strings.Count(line, dir); n != 1 {
		t.Errorf("path named %d times in %q, want once", n, line)
	}
}
