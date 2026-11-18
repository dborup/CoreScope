package main

import (
	"context"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// #789 M2b carry-over N2 (#866 review): an idle 2 s tick — no command, no
// in-memory claim expired, the table already loaded — takes no write and
// re-reads nothing. A zero-row UPDATE changes no data, so the test plants
// rows behind the runner's back: an already expired pending row that only
// the expiry UPDATE would touch, and a live pending row that only a reload
// would pick up.
func TestAntennaClaim789_IdleTickTakesNoWrite(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode) // loads the table; claimA lives 24 h
	if n := r.runner.pendingCount(); n != 1 {
		t.Fatalf("%d pending after the first claim, want 1", n)
	}
	plant := func(id, code string, expiresAt time.Time) {
		t.Helper()
		if _, err := r.store.db.Exec(`INSERT INTO node_antenna_claims
			(request_id, pubkey, height_m, code, status, created_at, expires_at)
			VALUES (?, ?, 12, ?, 'pending', ?, ?)`,
			id, r.pk, code, r.clock.UnixMilli(), expiresAt.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	plant(claimB, "BBBBBB", r.clock.Add(-time.Minute)) // already expired
	plant(claimC, "CCCCCC", r.clock.Add(time.Hour))    // live

	for i := 0; i < 3; i++ {
		r.clock = r.clock.Add(2 * time.Second)
		r.runner.RunOnce(context.Background())
	}
	if row := r.row(claimB); row.status != antennaclaim.StatusPending {
		t.Fatalf("an idle tick ran the expiry UPDATE: planted row is %q", row.status)
	}
	if n := r.runner.pendingCount(); n != 1 {
		t.Fatalf("an idle tick reloaded the table: %d pending in memory, want 1", n)
	}

	// Control: once an in-memory claim expires the same tick does sweep
	// and reload, so the assertions above can see a write when one runs.
	r.clock = r.clock.Add(25 * time.Hour)
	r.runner.RunOnce(context.Background())
	for _, id := range []string{claimA, claimB, claimC} {
		if row := r.row(id); row.status != antennaclaim.StatusExpired {
			t.Fatalf("claim %s after its expiry tick: %q", id, row.status)
		}
	}
	if n := r.runner.pendingCount(); n != 0 {
		t.Fatalf("%d pending in memory after the sweep, want 0", n)
	}
}
