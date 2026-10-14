package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

// #355 replay protection, TTL half: a queued decision older than five
// minutes is refused without touching the decision table, the audit or the
// live keys; a decision just inside the window is applied. Both go through
// the real queue, as the server's commands do.
func TestScopeApprovalRefusesExpiredCommands(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "scope-ttl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node','#fresh,#stale,#live','responded')`); err != nil {
		t.Fatal(err)
	}
	keys := newHotKeys(nil, nil)
	runner := newScopeApprovalRunner(store, keys)
	ctx := context.Background()
	const ttl = 5 * time.Minute
	run := func(op, name string, age time.Duration) channelregistry.RequestStatus {
		t.Helper()
		cmd := channelregistry.Command{RequestID: channelregistry.NewID(), Op: op, Name: name, CreatedAt: time.Now().Add(-age).UnixMilli()}
		if err := runner.queue.Enqueue(cmd, 100); err != nil {
			t.Fatal(err)
		}
		runner.RunOnce(ctx)
		st, err := runner.queue.Lookup(cmd.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	rows := func(name string) (decisions, audit int) {
		t.Helper()
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM approved_region_scopes WHERE name = ?`, name).Scan(&decisions); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM region_scope_audit WHERE name = ?`, name).Scan(&audit); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Just inside the window: applied.
	if st := run(channelregistry.OpScopeApprove, "#fresh", ttl-2*time.Second); st.Status != "approved" {
		t.Fatalf("approve at TTL-2s = %+v, want approved", st)
	}
	if _, ok := keys.Regions()["#fresh"]; !ok {
		t.Fatal("fresh approval not live")
	}
	if st := run(channelregistry.OpScopeReject, "#stale", ttl-2*time.Second); st.Status != "rejected" {
		t.Fatalf("reject at TTL-2s = %+v, want rejected", st)
	}

	// Older than the window: refused for every operation, with no effect.
	for _, c := range []struct {
		op, name string
		age      time.Duration
	}{
		{channelregistry.OpScopeApprove, "#live", ttl + time.Second},
		{channelregistry.OpScopeApprove, "#live", 24 * time.Hour},
		{channelregistry.OpScopeReject, "#live", ttl + time.Second},
	} {
		st := run(c.op, c.name, c.age)
		if st.Status != channelregistry.RequestError || st.Error != "request expired" {
			t.Fatalf("%s %s aged %s = %+v, want request expired", c.op, c.name, c.age, st)
		}
		if d, a := rows(c.name); d != 0 || a != 0 {
			t.Fatalf("expired %s %s wrote %d decision and %d audit row(s)", c.op, c.name, d, a)
		}
		if _, ok := keys.Regions()[c.name]; ok {
			t.Fatalf("expired %s %s made the scope live", c.op, c.name)
		}
	}
	_, auditBefore := rows("#fresh")
	if st := run(channelregistry.OpScopeRevoke, "#fresh", ttl+time.Second); st.Status != channelregistry.RequestError || st.Error != "request expired" {
		t.Fatalf("revoke aged TTL+1s = %+v, want request expired", st)
	}
	if _, ok := keys.Regions()["#fresh"]; !ok {
		t.Fatal("expired revoke removed a live approval")
	}
	if _, auditAfter := rows("#fresh"); auditAfter != auditBefore {
		t.Fatalf("expired revoke wrote audit: %d -> %d", auditBefore, auditAfter)
	}
	if st := run(channelregistry.OpScopeRevoke, "#fresh", ttl-2*time.Second); st.Status != "revoked" {
		t.Fatalf("revoke at TTL-2s = %+v, want revoked", st)
	}
}
