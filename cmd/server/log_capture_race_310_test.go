package main

import (
	"log"
	"testing"
	"time"
)

// Issue #310 (follow-up to #301 / #307): a guard for the neighbor-graph geo
// capture, one of the seven cmd/server sites that redirected the standard
// logger into a plain bytes.Buffer.
//
// captureNeighborGraphRejectLog installs the capture for the whole test and
// hands back a reader the test calls while the logger still points at that
// buffer. log.Logger orders its writers against each other under outMu, but
// nothing orders a write against the harness's own read. Plenty of goroutines
// log at moments the test does not control -- OpenDB's healSchemaFlags, the
// two background index builds Load() starts, runDistanceIndexBuild,
// (*DB).Close's WAL checkpoint, the recomputers -- and one left over from an
// earlier test can write into a later test's capture.
//
// The hazard is reproduced here without a store: one goroutine logs reject
// lines while the test reads them back, with no happens-before edge between
// the write and the read. Red under -race while the capture buffer is
// unlocked, green once every access takes the capture's mutex.
func TestNeighborGraphRejectLogCapture_ReadDuringConcurrentLogging_310(t *testing.T) {
	read := captureNeighborGraphRejectLog(t)

	const lines = 500
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Do not race ahead of the reader: the overlap is the point.
		<-started
		for i := 0; i < lines; i++ {
			log.Printf("[neighbor-graph] reject geo-far edge marker-310 %d", i)
		}
	}()

	deadline := time.Now().Add(30 * time.Second)
	got := read()
	close(started)
	for len(got) < lines {
		if time.Now().After(deadline) {
			t.Fatalf("captured %d of %d reject lines before the deadline", len(got), lines)
		}
		got = read()
	}
	<-done

	if n := len(read()); n != lines {
		t.Fatalf("captured %d reject lines, want %d", n, lines)
	}
}
