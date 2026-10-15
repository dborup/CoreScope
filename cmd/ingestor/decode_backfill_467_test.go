package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Issue #467: follow-ups to the historical decode backfill (#459).

// A revoke and a re-approve that both land after the watcher armed the pass
// and before the transaction that records it, with a row ingested in between,
// must not leave that row unresolved: the ceiling is read in the recording
// transaction, so the row is inside the pass.
func TestDecodeBackfill_StartWindowRevokeReapproveRescansTheGap(t *testing.T) {
	s := decodeBFStore(t, "start-window.db")
	key := deriveHashtagChannelKey(decodeBFChannel)
	decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: before", 0x31), nil, nil)
	hk := newIngestorHotKeys(s, nil, nil)
	hk.AddApproved(decodeBFChannel)

	var gap int64
	decodeBackfillPreSnapshotHook = func() {
		decodeBackfillPreSnapshotHook = nil
		hk.RemoveApproved(decodeBFChannel)
		gap = decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: in the gap", 0x32), hk.Channels(), nil)
		hk.AddApproved(decodeBFChannel)
	}
	t.Cleanup(func() { decodeBackfillPreSnapshotHook = nil })
	set := decodeBFSettings(10, 1000)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	if gap == 0 {
		t.Fatal("the pre-snapshot hook never ran")
	}
	// A second pass runs only if the first one left the key uncovered, which
	// is the correct outcome when it did not reach the gap row.
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, gap); got.channelHash.String != decodeBFChannel {
		t.Errorf("gap row channel_hash = %q, want %q: a revoke and re-approve inside the start window left the row unresolved", got.channelHash.String, decodeBFChannel)
	}
}

// A hashRegions entry dropped by one reload and restored by the next is a
// removal, even though it goes through setBaseRegions and not through the
// approval path.
func TestDecodeBackfill_RegionConfigRemoveReaddAcrossTwoReloadsRescansTheGap(t *testing.T) {
	s := decodeBFStore(t, "region-reload.db")
	const scope = "#bf-reload-region"
	decodeBFIngest(t, s, decodeBFTransportFrame(t, scope, []byte{0x01}), nil, nil)
	hk := newIngestorHotKeys(s, nil, decodeBFRegionKeys(scope))
	set := decodeBFSettings(10, 1000)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindScope)
	if err != nil {
		t.Fatal(err)
	}
	if !covered[decodeBackfillKeyIdentity(scope, decodeBFRegionKeys(scope)[scope])] {
		t.Fatal("precondition: the first pass did not cover the region")
	}

	hk.setBaseRegions(map[string][]byte{})
	gap := decodeBFIngest(t, s, decodeBFTransportFrame(t, scope, []byte{0x02}), nil, hk.Regions())
	hk.setBaseRegions(decodeBFRegionKeys(scope))

	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, gap); got.scopeName.String != scope {
		t.Errorf("scope gap row = %q, want %q: a region removed and restored by two reloads stayed credited", got.scopeName.String, scope)
	}
}

// A region revoked after a batch decoded and before it writes must not have
// its rows persisted: the in-transaction re-check covers regions as well as
// channels.
func TestDecodeBackfill_RegionRevocationInsideTheWriteWindowIsHonoured(t *testing.T) {
	s := decodeBFStore(t, "region-write-window.db")
	const scope = "#bf-window-region"
	row := decodeBFIngest(t, s, decodeBFTransportFrame(t, scope, []byte{0x01}), nil, nil)
	hk := newHotKeys(nil, nil)
	hk.AddApprovedRegions(scope)

	fired := false
	decodeBackfillPreWriteHook = func() {
		if !fired {
			fired = true
			hk.RemoveApprovedRegion(scope)
		}
	}
	t.Cleanup(func() { decodeBackfillPreWriteHook = nil })
	res, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	decodeBackfillPreWriteHook = nil
	if !fired {
		t.Fatal("the pre-write hook never ran")
	}
	if got := decodeBFReadRow(t, s, row); got.scopeName.String != "" {
		t.Errorf("scope_name = %q: the batch persisted a row matched with a region revoked before its write", got.scopeName.String)
	}
	if res.ScopeUpdated != 0 {
		t.Errorf("ScopeUpdated = %d, want 0", res.ScopeUpdated)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(covered) != 0 {
		t.Errorf("covered = %v: the revoked region was credited", covered)
	}
}

// The key sets main builds forget a removed key on the event itself. The
// builder is the one main calls, so dropping the wiring fails here.
func TestNewIngestorHotKeys_ForgetsARevokedKey(t *testing.T) {
	s := decodeBFStore(t, "wired.db")
	decodeBFIngest(t, s, decodeBFGrpTxt(t, deriveHashtagChannelKey(decodeBFChannel), "A: one", 0x31), nil, nil)
	hk := newIngestorHotKeys(s, nil, nil)
	hk.AddApproved(decodeBFChannel)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000)); err != nil {
		t.Fatal(err)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if !covered[decodeBFIdentity(decodeBFChannel)] {
		t.Fatal("precondition: the pass did not cover the key")
	}

	hk.RemoveApproved(decodeBFChannel)
	covered, err = s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if covered[decodeBFIdentity(decodeBFChannel)] {
		t.Error("a revoked key is still recorded as covered: the removal hook is not wired")
	}
}

// main must build its key sets with newIngestorHotKeys; a plain newHotKeys
// there would leave the hook unwired and every Go test green.
func TestMain_BuildsItsKeysWithTheWiredBuilder(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, "newIngestorHotKeys(store, channelKeys, regionKeys)") {
		t.Error("main.go does not build its key sets with newIngestorHotKeys")
	}
	if strings.Contains(text, "newHotKeys(") {
		t.Error("main.go builds a key set with newHotKeys, bypassing the removal hook")
	}
}
