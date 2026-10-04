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

	// A new failure inside the interval of the last failure line (120 s)
	// is counted, not logged, and its recovery is not logged either; the
	// next failure line, an interval after the last one, reports it.
	lines = nil
	l.failed("x", errTmp, t0.Add(151*time.Second))
	l.succeeded("x")
	if len(lines) != 0 {
		t.Fatalf("episode inside the interval logged %q", lines)
	}
	l.failed("x", errTmp, t0.Add(181*time.Second))
	l.succeeded("x")
	want = []string{
		"[stats-file] write x: " + errTmp.Error() + " (1 more failed writes since the last report)",
		"[stats-file] write x: ok again after 1 failed writes",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("second episode lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// PR #216 review, F2: a failure that alternates with successes (flapping)
// is rate-limited too. At 1 Hz, 120 s gave 120 lines (a failure line and an
// "ok again" line every time); now the failure line and the recovery line
// each come at most once per interval, and the failures in between are
// counted.
func TestStatsWriteLogFlappingAtMostOncePerInterval_160(t *testing.T) {
	var lines []string
	l := statsWriteLog{every: time.Minute, logf: func(f string, a ...any) {
		lines = append(lines, fmt.Sprintf(f, a...))
	}}
	errTmp := errors.New("x.tmp: broken")
	t0 := time.Unix(1_700_000_000, 0)
	for s := 0; s < 120; s++ {
		at := t0.Add(time.Duration(s) * time.Second)
		if s%2 == 0 {
			l.failed("x", errTmp, at)
		} else {
			l.succeeded("x")
		}
	}
	want := []string{
		"[stats-file] write x: " + errTmp.Error(),
		"[stats-file] write x: ok again after 1 failed writes",
		"[stats-file] write x: " + errTmp.Error() + " (29 more failed writes since the last report)",
		"[stats-file] write x: ok again after 1 failed writes",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%d lines:\n%s\nwant:\n%s", len(lines), strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// writeUnopenableStatsTmp plants a regular tmp the ingestor cannot open for
// writing (0400), the way a 0600 tmp of another service user looks to a
// non-root ingestor: open(2) fails with EACCES before any owner check.
func writeUnopenableStatsTmp(t *testing.T) (path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no Unix file owners")
	}
	if os.Geteuid() == 0 {
		t.Skip("root opens a 0400 file for writing")
	}
	path = filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(path+".tmp", nil, 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

// PR #216 review, F1: the usual #160 case is a foreign 0600 tmp and a non-root
// ingestor, where open itself fails with EACCES. The hint must be there too,
// with the owner the Lstat finds.
func TestWriteStatsAtomicUnopenableForeignTmpNamesTheFix_160(t *testing.T) {
	path := writeUnopenableStatsTmp(t)
	old := statsFileEUID
	t.Cleanup(func() { statsFileEUID = old })
	statsFileEUID = func() int { return os.Geteuid() + 1 } // the tmp is someone else's

	err := writeStatsAtomic(path, []byte(`{}`))
	if err == nil {
		t.Fatal("unopenable tmp accepted")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error %q does not wrap the permission error", err)
	}
	for _, want := range []string{"or fix its owner", fmt.Sprintf("uid %d", os.Geteuid()), fmt.Sprintf("uid %d", os.Geteuid()+1)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// An unopenable tmp of the ingestor's own user is a mode problem, not an
// owner one; the hint says so.
func TestWriteStatsAtomicUnopenableOwnTmpNamesTheFix_160(t *testing.T) {
	path := writeUnopenableStatsTmp(t)
	err := writeStatsAtomic(path, []byte(`{}`))
	if err == nil {
		t.Fatal("unopenable tmp accepted")
	}
	if !strings.Contains(err.Error(), "or fix its permissions") || strings.Contains(err.Error(), "owner") {
		t.Errorf("error %q: want the permissions hint, not the owner one", err)
	}
}

// PR #216 review, F3: a backward wall-clock step must not silence a
// persisting failure. The limiter compared now with the last failure line,
// and a negative difference counted as "inside the interval", so after a
// 1 h step back nothing was logged for an hour. A step back now counts as
// an interval passed (the writer also passes a monotonic time.Now()).
func TestStatsWriteLogBackwardClockStepStillLogs_160(t *testing.T) {
	var lines []string
	l := statsWriteLog{every: time.Minute, logf: func(f string, a ...any) {
		lines = append(lines, fmt.Sprintf(f, a...))
	}}
	errTmp := errors.New("x.tmp: broken")
	t0 := time.Unix(1_700_000_000, 0)
	for s := 0; s < 10; s++ {
		l.failed("x", errTmp, t0.Add(time.Duration(s)*time.Second))
	}
	back := t0.Add(-time.Hour) // the wall clock steps back 1 h
	for s := 0; s < 180; s++ {
		l.failed("x", errTmp, back.Add(time.Duration(s)*time.Second))
	}
	want := []string{
		"[stats-file] write x: " + errTmp.Error(),
		"[stats-file] write x: " + errTmp.Error() + " (9 more failed writes since the last report)",
		"[stats-file] write x: " + errTmp.Error() + " (59 more failed writes since the last report)",
		"[stats-file] write x: " + errTmp.Error() + " (59 more failed writes since the last report)",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%d lines:\n%s\nwant:\n%s", len(lines), strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
