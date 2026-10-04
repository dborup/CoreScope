package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncLogBuffer collects the standard logger's output; the stats writer
// logs from its own goroutine.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// linesWith returns the logged lines that contain sub.
func (b *syncLogBuffer) linesWith(sub string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// captureStdLog sends the standard logger to a buffer until the test ends.
// Register it before the writer's stop, so the stop runs first (LIFO).
func captureStdLog(t *testing.T) *syncLogBuffer {
	t.Helper()
	b := &syncLogBuffer{}
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return b
}

// openStatsTestStore opens a Store for a stats-writer test; it is closed
// after the writer's stop, which the caller registers later (LIFO).
func openStatsTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// A tmp path the writer cannot use fails every tick (1 Hz in production).
// The failure is logged once, not once per tick, and the first successful
// write after it is logged once (#160).
func TestStatsFileWriterLogsWriteFailureOnce_160(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	t.Setenv("CORESCOPE_INGESTOR_STATS", path)
	// A directory at the tmp path makes every write fail.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	logs := captureStdLog(t)
	store := openStatsTestStore(t, dir)
	stop := StartStatsFileWriter(store, 5*time.Millisecond)
	t.Cleanup(stop)

	time.Sleep(300 * time.Millisecond) // dozens of failing ticks
	if got := logs.linesWith("[stats-file] write "); len(got) != 1 {
		t.Fatalf("%d failure lines while the tmp stayed broken, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}

	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no stats file after the tmp path was freed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // more successful ticks
	stop()

	if got := logs.linesWith("[stats-file] write "); len(got) != 2 {
		t.Fatalf("%d write lines, want the failure and one recovery:\n%s", len(got), strings.Join(got, "\n"))
	}
	if got := logs.linesWith("ok again"); len(got) != 1 {
		t.Fatalf("%d recovery lines, want 1", len(got))
	}
}

// A tmp file that belongs to another user cannot be fixed by the ingestor;
// the error says what the operator has to do (#160).
func TestWriteStatsAtomicForeignTmpErrorNamesTheFix_160(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix file owners")
	}
	old := statsFileEUID
	t.Cleanup(func() { statsFileEUID = old })
	statsFileEUID = func() int { return os.Geteuid() + 1 } // the tmp is someone else's

	path := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(path+".tmp", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeStatsAtomic(path, []byte(`{}`))
	if err == nil {
		t.Fatal("foreign tmp accepted")
	}
	if want := "remove " + path + ".tmp or fix its owner"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q lacks the hint %q", err, want)
	}
}

// A failure that persists is logged at most once per interval, with the
// failures in between counted; recovery is logged once, and a success
// without a failure before it logs nothing (#160).
func TestStatsWriteLogAtMostOncePerInterval_160(t *testing.T) {
	var lines []string
	l := statsWriteLog{every: time.Minute, logf: func(f string, a ...any) {
		lines = append(lines, fmt.Sprintf(f, a...))
	}}
	errTmp := errors.New("x.tmp belongs to uid 1, not 2; remove x.tmp or fix its owner")
	t0 := time.Unix(1_700_000_000, 0)

	l.succeeded("x")           // healthy start: silent
	for s := 0; s < 150; s++ { // 2.5 minutes of 1 Hz failures
		l.failed("x", errTmp, t0.Add(time.Duration(s)*time.Second))
	}
	want := []string{
		"[stats-file] write x: " + errTmp.Error(),
		"[stats-file] write x: " + errTmp.Error() + " (59 more failed writes since the last report)",
		"[stats-file] write x: " + errTmp.Error() + " (59 more failed writes since the last report)",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("failure lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	lines = nil
	l.succeeded("x")
	l.succeeded("x")
	if len(lines) != 1 || lines[0] != "[stats-file] write x: ok again after 150 failed writes" {
		t.Fatalf("recovery lines %q", lines)
	}

	// A new failure after recovery is logged at once, counted afresh.
	lines = nil
	l.failed("x", errTmp, t0.Add(151*time.Second))
	l.succeeded("x")
	if len(lines) != 2 || !strings.HasSuffix(lines[1], "ok again after 1 failed writes") {
		t.Fatalf("second episode lines %q", lines)
	}
}
