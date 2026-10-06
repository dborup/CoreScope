package main

import (
	"strings"
	"testing"
	"time"
)

// Issue #301: Load() starts the two background index builds, and each builder
// logs one more line ("index build complete") AFTER it has flipped its ready
// flag. WaitIndexesReady returning therefore does not mean the builders are
// done logging, and a builder of an earlier store can log at any later time.
// The log capture of the hash-migrate harness must take those writes without a
// data race: here the subpath builder is released inside the capture, so its
// last line lands in the captured output while nothing orders it before the
// read at the end of the capture. Red under -race with an unsynchronized
// buffer.
func TestHashMigrate_LogCaptureToleratesIndexBuilderLogs_301(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	db, err := OpenDB(d.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	store := NewPacketStore(db, &PacketStoreConfig{})
	release := make(chan struct{})
	store.subpathBuildGate = func() { <-release }
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}

	out := hm215CaptureLog(func() {
		close(release)
		if !store.WaitIndexesReady(30 * time.Second) {
			t.Error("background index builds did not finish")
		}
		// The builder logs its completion line after the ready flag that
		// woke us; give it time to land in the capture. Sleeping orders
		// nothing, so the race detector still sees the two accesses.
		time.Sleep(200 * time.Millisecond)
	})
	if !strings.Contains(out, "index build complete: subpath") {
		t.Fatalf("the subpath builder's last line was not captured:\n%s", out)
	}
}
