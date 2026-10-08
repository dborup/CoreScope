package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

func TestApprovedRegionsSurviveReloadAndRevokeKeepsConfigured(t *testing.T) {
	key := func(name string) []byte { h := sha256.Sum256([]byte(name)); return h[:16] }
	hk := newHotKeys(nil, map[string][]byte{"#base": key("#base")})
	hk.AddApprovedRegions("#extra", "#base")
	if len(hk.Regions()) != 2 {
		t.Fatalf("regions after approval: %v", hk.Regions())
	}
	hk.setBaseRegions(map[string][]byte{"#file": key("#file"), "#base": key("#base")})
	if len(hk.Regions()) != 3 {
		t.Fatalf("reload lost approved region: %v", hk.Regions())
	}
	hk.RemoveApprovedRegion("#base")
	if _, ok := hk.Regions()["#base"]; !ok {
		t.Fatal("revoking overlay removed configured region")
	}
	hk.RemoveApprovedRegion("#extra")
	if _, ok := hk.Regions()["#extra"]; ok {
		t.Fatal("revoked region remains live")
	}
}

func TestScopeDecisionRowsStayBounded(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "scope-bound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		_, err := tx.Exec(`INSERT INTO approved_region_scopes(name,status,created_at,reviewed_at) VALUES (?,'rejected',1,1)`, fmt.Sprintf("#old%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node','#fresh','responded')`)
	if err != nil {
		t.Fatal(err)
	}
	r := newScopeApprovalRunner(store, newHotKeys(nil, nil))
	res := r.apply(context.Background(), channelregistry.Command{RequestID: channelregistry.NewID(), Op: channelregistry.OpScopeReject, Name: "#fresh", CreatedAt: time.Now().UnixMilli()})
	if res.Status != "rejected" {
		t.Fatalf("rejection = %+v", res)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM approved_region_scopes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > 3968 {
		t.Fatalf("decision rows = %d, want <=3968", n)
	}
}

func TestScopeApprovalCapacityDoesNotActivateOverLimit(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "scope-cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxApprovedRegionScopes; i++ {
		_, err := tx.Exec(`INSERT INTO approved_region_scopes(name,status,created_at,reviewed_at) VALUES (?,'approved',1,1)`, fmt.Sprintf("#old%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node','#fresh','responded')`)
	if err != nil {
		t.Fatal(err)
	}
	keys := newHotKeys(nil, nil)
	r := newScopeApprovalRunner(store, keys)
	res := r.apply(context.Background(), channelregistry.Command{RequestID: channelregistry.NewID(), Op: channelregistry.OpScopeApprove, Name: "#fresh", CreatedAt: time.Now().UnixMilli()})
	if res.Status != "error" {
		t.Fatalf("over-cap approval = %+v", res)
	}
	if _, ok := keys.Regions()["#fresh"]; ok {
		t.Fatal("over-cap scope activated")
	}
}

func TestApprovedRegionOverlayWithExternalHashRegionsPath(t *testing.T) {
	dir := t.TempDir()
	writeRegionsFile(t, dir, "regions.json", "#file")
	configPath := writeRegionsConfig(t, dir, []string{"#inline"}, "regions.json")
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base, err := loadRegionKeys(cfg, configPath)
	if err != nil {
		t.Fatal(err)
	}
	hk := newHotKeys(nil, base)
	hk.AddApprovedRegions("#file", "#approved")
	writeRegionsFile(t, dir, "regions.json", "#file", "#later")
	if err := hk.reload(configPath); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"#inline", "#file", "#later", "#approved"} {
		if _, ok := hk.Regions()[name]; !ok {
			t.Errorf("reload lost %s", name)
		}
	}
	hk.RemoveApprovedRegion("#file")
	hk.RemoveApprovedRegion("#approved")
	if _, ok := hk.Regions()["#file"]; !ok {
		t.Fatal("revoking overlay removed external configured scope")
	}
	if _, ok := hk.Regions()["#approved"]; ok {
		t.Fatal("revoked overlay survived")
	}
}

func TestScopeApprovalRunnerObservedOnlyDurableAndRevocable(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys := newHotKeys(nil, nil)
	runner := newScopeApprovalRunner(store, keys)
	cmd := func(op, name string) channelregistry.Command {
		return channelregistry.Command{RequestID: channelregistry.NewID(), Op: op, Name: name, CreatedAt: time.Now().UnixMilli()}
	}
	ctx := context.Background()
	missing := runner.apply(ctx, cmd(channelregistry.OpScopeApprove, "#new"))
	if missing.Status != "error" {
		t.Fatalf("unobserved approval = %+v", missing)
	}
	_, err = store.db.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node','#new,#Other','responded')`)
	if err != nil {
		t.Fatal(err)
	}
	approveCmd := cmd(channelregistry.OpScopeApprove, "#new")
	if err := runner.queue.Enqueue(approveCmd, 100); err != nil {
		t.Fatal(err)
	}
	runner.RunOnce(ctx)
	queued, err := runner.queue.Lookup(approveCmd.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	approved := channelregistry.Result{Status: queued.Status, Proposal: queued.Proposal, Error: queued.Error}
	if approved.Status != "approved" {
		t.Fatalf("approval = %+v", approved)
	}
	if _, ok := keys.Regions()["#new"]; !ok {
		t.Fatal("approved scope not live")
	}
	if rejected := runner.apply(ctx, cmd(channelregistry.OpScopeReject, "#new")); rejected.Status != "error" {
		t.Fatalf("reject must not silently revoke an approved scope: %+v", rejected)
	}
	if invalid := runner.apply(ctx, cmd(channelregistry.OpScopeApprove, "#bad name")); invalid.Status != "error" {
		t.Fatalf("invalid scope accepted: %+v", invalid)
	}
	_, err = store.db.Exec(`DELETE FROM observer_neighbors`)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := newHotKeys(nil, nil)
	if err := newScopeApprovalRunner(store, reloaded).LoadApproved(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Regions()["#new"]; !ok {
		t.Fatal("approved scope lost on restart")
	}
	revoked := runner.apply(ctx, cmd(channelregistry.OpScopeRevoke, "#new"))
	if revoked.Status != "revoked" {
		t.Fatalf("revoke = %+v", revoked)
	}
	if _, ok := keys.Regions()["#new"]; ok {
		t.Fatal("revoked scope still live")
	}
	// Simulate a crash after the original approval DB commit but before its
	// result file was durably completed. Replaying that old request after a
	// newer revoke must report its original result without re-approving.
	replay := runner.apply(ctx, approveCmd)
	if replay.Status != "approved" {
		t.Fatalf("replay should report original approval: %+v", replay)
	}
	if _, ok := keys.Regions()["#new"]; ok {
		t.Fatal("old approval replay undid newer revoke")
	}
	if err := newScopeApprovalRunner(store, newHotKeys(nil, nil)).LoadApproved(ctx); err != nil {
		t.Fatal(err)
	}
	var audit int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM region_scope_audit WHERE name = '#new'`).Scan(&audit); err != nil || audit != 2 {
		t.Fatalf("audit count = %d, err=%v", audit, err)
	}
}
