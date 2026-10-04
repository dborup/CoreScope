package main

import (
	"bytes"
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
