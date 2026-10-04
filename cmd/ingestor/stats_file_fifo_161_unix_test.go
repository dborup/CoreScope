//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mkStatsTmpFIFO plants a named pipe at path+".tmp".
func mkStatsTmpFIFO(t *testing.T, path string) string {
	t.Helper()
	tmp := path + ".tmp"
	if err := syscall.Mkfifo(tmp, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	return tmp
}

// openFIFOReader opens the read end without blocking. A writer blocked in
// open(2) on the FIFO then gets through.
func openFIFOReader(t *testing.T, fifo string) *os.File {
	t.Helper()
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open FIFO reader: %v", err)
	}
	return r
}

// writeStatsAtomicOrRelease runs writeStatsAtomic and returns its error. If
// it blocks for 2s, the FIFO gets a reader so the goroutine can end, and the
// test fails.
func writeStatsAtomicOrRelease(t *testing.T, path, fifo string) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- writeStatsAtomic(path, []byte(`{}`)) }()
	select {
	case err := <-errc:
		return err
	case <-time.After(2 * time.Second):
		r := openFIFOReader(t, fifo)
		defer r.Close()
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("writeStatsAtomic blocked on a FIFO at the tmp path")
		return nil
	}
}

// assertNotRegularRefused checks the error and that nothing was published.
func assertNotRegularRefused(t *testing.T, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatal("FIFO at the tmp path accepted")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error %q does not say the tmp is not a regular file", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("stats file published from a FIFO: lstat err %v", err)
	}
}

// Opening a FIFO for writing waits for a reader. Without one the writer
// must fail at once, not block forever (#161).
func TestWriteStatsAtomicFIFOWithoutReaderFailsFast_161(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	fifo := mkStatsTmpFIFO(t, path)
	assertNotRegularRefused(t, writeStatsAtomicOrRelease(t, path, fifo), path)
}

// With a reader the open succeeds; the writer must still refuse the FIFO
// rather than write into it and rename it over the stats file (#161).
func TestWriteStatsAtomicFIFOWithReaderRefused_161(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	fifo := mkStatsTmpFIFO(t, path)
	r := openFIFOReader(t, fifo)
	defer r.Close()
	assertNotRegularRefused(t, writeStatsAtomicOrRelease(t, path, fifo), path)
}

// The writer's stop waits for its goroutine. A FIFO at the tmp path must
// not keep that goroutine, and so the stop, from returning (#161).
func TestStatsFileWriterStopsWithFIFOAtTmp_161(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	t.Setenv("CORESCOPE_INGESTOR_STATS", path)
	fifo := mkStatsTmpFIFO(t, path)
	captureStdLog(t) // keep the failure line out of the test output
	store := openStatsTestStore(t, dir)
	stop := StartStatsFileWriter(store, 5*time.Millisecond)

	time.Sleep(100 * time.Millisecond) // several ticks against the FIFO
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		r := openFIFOReader(t, fifo)
		defer r.Close()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("stop hung: the writer is blocked on the FIFO")
	}
}
