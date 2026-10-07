package main

import (
	"io"
	"log"
	"sync"
	"testing"
)

// Issue #310 (follow-up to #301 / #307): a test that points the standard
// logger at a plain bytes.Buffer and then reads that buffer races with every
// goroutine that is still logging. log.Logger serializes its writers against
// each other under outMu, but nothing orders a write against the harness's
// own read -- and goroutines routinely outlive the test that started them:
// OpenDB's healSchemaFlags, the two background index builds Load() starts,
// runDistanceIndexBuild, (*DB).Close's WAL checkpoint, the recomputers. Such
// a goroutine can land in a *later* test's capture.
//
// logCapture closes the hazard twice over:
//
//  1. the buffer is a syncBuffer (#307), so every write and every read take
//     the same mutex. This is the load-bearing fix and the one the guard
//     tests kill mutants on;
//  2. stop restores the previous writer *before* reading. log.SetOutput and
//     (*Logger).output share the logger's outMu, so the restore orders every
//     in-flight write before the read. Defence in depth: with (1) in place no
//     test can distinguish it, so no mutant pins it.
type logCapture struct {
	buf    *syncBuffer
	prev   io.Writer
	flags  int
	prefix string
	once   sync.Once
}

// startLogCapture redirects the standard logger into a locked buffer and
// records the writer, flags and prefix to put back. A caller that wants
// log.SetFlags(0) sets it after this returns; restoreWriter undoes it.
func startLogCapture() *logCapture {
	c := &logCapture{
		buf:    newSyncBuffer(),
		prev:   log.Writer(),
		flags:  log.Flags(),
		prefix: log.Prefix(),
	}
	log.SetOutput(c.buf)
	return c
}

// read returns everything captured so far. Safe to call while the logger
// still points at the capture.
func (c *logCapture) read() string { return c.buf.String() }

// restoreWriter puts the previous writer, flags and prefix back. Idempotent,
// so it can be deferred and still be followed by stop.
func (c *logCapture) restoreWriter() {
	c.once.Do(func() {
		log.SetOutput(c.prev)
		log.SetFlags(c.flags)
		log.SetPrefix(c.prefix)
	})
}

// stop restores the previous writer and then returns the captured output.
func (c *logCapture) stop() string {
	c.restoreWriter()
	return c.buf.String()
}

// captureLog returns what the standard logger printed while fn ran. The
// writer is restored even if fn calls t.Fatal or panics.
func captureLog(fn func()) string {
	return captureLogRead(func(func() string) { fn() })
}

// captureLogRead is captureLog for a body that has to read the capture while
// the logger is still pointing at it -- polling for a line a background
// goroutine is about to write, say. read is safe to call concurrently with
// any logger write.
func captureLogRead(fn func(read func() string)) string {
	c := startLogCapture()
	defer c.restoreWriter()
	fn(c.read)
	return c.stop()
}

// newLogCapture installs a capture for the rest of the test and returns a
// locked reader. Use it where the assertions have to see the log while the
// capture is still installed; captureLog is the simpler choice otherwise.
func newLogCapture(t testing.TB) func() string {
	t.Helper()
	c := startLogCapture()
	t.Cleanup(c.restoreWriter)
	return c.read
}
