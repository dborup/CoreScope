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
	type tc struct {
		name  string
		setup func(t *testing.T, path string)
		want  string
	}
	foreign := func(t *testing.T) {
		old := statsFileEUID
		t.Cleanup(func() { statsFileEUID = old })
		statsFileEUID = func() int { return os.Geteuid() + 1 }
	}
	cases := []tc{
		{"FIFO without a reader", func(t *testing.T, path string) { mkStatsTmpFIFO(t, path) }, "not a regular file"},
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"foreign owner", func(t *testing.T, path string) {
			if err := os.WriteFile(path+".tmp", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			foreign(t)
		}, "remove it or fix its owner"},
		{"foreign owner, unopenable", func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root opens a 0400 file for writing")
			}
			if err := os.WriteFile(path+".tmp", nil, 0o400); err != nil {
				t.Fatal(err)
			}
			foreign(t)
		}, "remove it or fix its owner"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "stats.json")
			c.setup(t, path)
			err := writeStatsAtomic(path, []byte(`{}`))
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
	mkStatsTmpFIFO(t, path)
	logs := captureStdLog(t)
	store := openStatsTestStore(t, dir)
	stop := StartStatsFileWriter(store, 5*time.Millisecond)
	t.Cleanup(stop)

	deadline := time.Now().Add(3 * time.Second)
	for len(logs.linesWith("[stats-file] write")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no failure line")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	line := logs.linesWith("[stats-file] write")[0]
	if n := strings.Count(line, dir); n != 1 {
		t.Errorf("path named %d times in %q, want once", n, line)
	}
}
