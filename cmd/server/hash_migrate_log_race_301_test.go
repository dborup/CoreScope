package main

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// hm301InstallSubpathGate holds the subpath index build until the returned
// release runs. release is idempotent, so the caller can defer it right after
// the gate goes in and still release it on purpose later: without that
// deferred net a t.Fatal between the two (store.Load returning an error, say)
// would leave the build goroutine blocked on the channel for the rest of the
// run (#310, nit 3 of the #307 review).
func hm301InstallSubpathGate(store *PacketStore) (release func()) {
	gate := make(chan struct{})
	store.subpathBuildGate = func() { <-gate }
	return sync.OnceFunc(func() { close(gate) })
}

// The deferred net and the deliberate release are the same func, so releasing
// twice must be a no-op rather than a "close of closed channel" panic.
func TestHashMigrateSubpathGateReleaseIsIdempotent_310(t *testing.T) {
	store := &PacketStore{}
	release := hm301InstallSubpathGate(store)

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		store.subpathBuildGate()
	}()

	release()
	release()

	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		t.Fatal("the gated build goroutine was never released")
	}
}

// Issue #301: Load() starts the two background index builds, and each builder
// logs one more line ("index build complete") AFTER it has flipped its ready
// flag. WaitIndexesReady returning therefore does not mean the builders are
// done logging, and a builder of an earlier store can log at any later time.
// The log capture of the hash-migrate harness must take those writes without a
// data race: here the subpath builder is released inside the capture, so its
// last line lands in the captured output while nothing orders it before the
// harness's reads. Red under -race with an unsynchronized buffer.
func TestHashMigrate_LogCaptureToleratesIndexBuilderLogs_301(t *testing.T) {
	d := hm215Create(t)
	d.ballast(t)
	db, err := OpenDB(d.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	store := NewPacketStore(db, &PacketStoreConfig{})
	release := hm301InstallSubpathGate(store)
	defer release() // a t.Fatal below must not strand the build goroutine
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}

	const want = "index build complete: subpath"
	out := captureLogRead(func(read func() string) {
		release()
		if !store.WaitIndexesReady(30 * time.Second) {
			t.Error("background index builds did not finish")
			return
		}
		// The builder logs its completion line after the ready flag that
		// woke us. Poll the capture for it rather than sleeping a fixed
		// 200 ms (#310, nit 2 of the #307 review): the poll reads the
		// buffer while the builder can still be writing to it and shares
		// no lock with the builder's own progress, so an unsynchronized
		// capture is still a race the detector reports — proven by the
		// mutant in the PR report.
		deadline := time.Now().Add(30 * time.Second)
		for !strings.Contains(read(), want) {
			if time.Now().After(deadline) {
				t.Error("the subpath builder's last line did not arrive")
				return
			}
			runtime.Gosched()
		}
	})
	if !strings.Contains(out, want) {
		t.Fatalf("the subpath builder's last line was not captured:\n%s", out)
	}
}
