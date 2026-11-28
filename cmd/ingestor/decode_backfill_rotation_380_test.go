package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
)

// Issue #380, review round 3: a key's identity is its name *and* its value,
// a removal is forgotten when it happens rather than when it is next sampled,
// and a revocation that lands inside a batch is honoured by that batch.

// Two pre-shared keys for one non-hashtag name, as channelKeys and the
// rainbow file allow: an operator rotating or correcting "Ops" swaps K1 for K2.
const (
	decodeBFOps   = "Ops"
	decodeBFOpsK1 = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	decodeBFOpsK2 = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
)

// decodeBFIdentity is the bookkeeping identity of a hashtag channel name with
// its derived key.
func decodeBFIdentity(name string) string {
	return decodeBackfillKeyIdentity(name, channelKeyBytes(deriveHashtagChannelKey(name)))
}

// decodeBFWatchRemovals wires hk's removal events to s, as main does at start-up.
func decodeBFWatchRemovals(s *Store, hk *hotKeys) {
	hk.onRemoved(s.forgetDecodeBackfillKeys)
}

// decodeBFCaptureLog redirects the standard logger for the rest of the test.
func decodeBFCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// ---------------------------------------------------------------------------
// F1: a new value under the same name
// ---------------------------------------------------------------------------

// A value replaced under the same name mid-pass is a revocation of the old
// value: the pass must not decrypt any further row with it.
func TestDecodeBackfill_KeyValueRotatedMidRunIsNotUsed(t *testing.T) {
	s := decodeBFStore(t, "rotate-midrun.db")
	logs := decodeBFCaptureLog(t)
	id1 := decodeBFIngest(t, s, decodeBFGrpTxt(t, decodeBFOpsK1, "O: one", 0x41), nil, nil)
	id2 := decodeBFIngest(t, s, decodeBFGrpTxt(t, decodeBFOpsK1, "O: two", 0x42), nil, nil)
	id3 := decodeBFIngest(t, s, decodeBFGrpTxt(t, decodeBFOpsK2, "O: three", 0x43), nil, nil)

	hk := newHotKeys(map[string]string{decodeBFOps: decodeBFOpsK1}, nil)
	set := decodeBFSettings(1, 1)
	rotated := false
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) {
		if !rotated {
			rotated = true
			hk.setBase(map[string]string{decodeBFOps: decodeBFOpsK2}) // what hotKeys.reload does
		}
	}
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	res, err := s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	decodeBackfillBatchHook = nil

	if got := decodeBFReadRow(t, s, id1); got.channelHash.String != decodeBFOps {
		t.Fatalf("precondition: id %d channel_hash = %q, want %q (written before the rotation)", id1, got.channelHash.String, decodeBFOps)
	}
	if got := decodeBFReadRow(t, s, id2); got.channelHash.String != "enc_42" {
		t.Errorf("id %d channel_hash = %q: the pass kept decrypting with the value that was replaced", id2, got.channelHash.String)
	}
	if res.ChannelUpdated != 1 {
		t.Errorf("ChannelUpdated = %d, want 1 (only the row written before the rotation)", res.ChannelUpdated)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if covered[decodeBackfillKeyIdentity(decodeBFOps, channelKeyBytes(decodeBFOpsK1))] {
		t.Error("the replaced value was credited by the pass it was replaced under")
	}

	// The new value arms the next pass, which starts at id 0.
	res, err = s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "channel" || res.From != 0 {
		t.Fatalf("follow-up run = %+v, want a channel pass from id 0", res)
	}
	if got := decodeBFReadRow(t, s, id3); got.channelHash.String != decodeBFOps {
		t.Errorf("id %d channel_hash = %q: the new value never armed a pass", id3, got.channelHash.String)
	}
	if got := decodeBFReadRow(t, s, id2); got.channelHash.String != "enc_42" {
		t.Errorf("id %d channel_hash = %q: nothing configured can decrypt it any more", id2, got.channelHash.String)
	}

	for _, secret := range []string{decodeBFOpsK1, decodeBFOpsK2, strings.ToUpper(decodeBFOpsK1), strings.ToUpper(decodeBFOpsK2), `"` + decodeBFOps + `"`} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log names a key or its name (%q):\n%s", secret, logs.String())
		}
	}
}

// A value replaced under the same name after a pass covered the name must arm
// a new pass: history encrypted with the new value is otherwise never read.
func TestDecodeBackfill_KeyValueRotatedAfterRunArmsAPass(t *testing.T) {
	s := decodeBFStore(t, "rotate-after.db")
	id1 := decodeBFIngest(t, s, decodeBFGrpTxt(t, decodeBFOpsK1, "O: one", 0x41), nil, nil)
	id2 := decodeBFIngest(t, s, decodeBFGrpTxt(t, decodeBFOpsK2, "O: two", 0x42), nil, nil)
	set := decodeBFSettings(10, 1000)

	hk := newHotKeys(map[string]string{decodeBFOps: decodeBFOpsK1}, nil)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, id1); got.channelHash.String != decodeBFOps {
		t.Fatalf("precondition: id %d channel_hash = %q", id1, got.channelHash.String)
	}

	hk.setBase(map[string]string{decodeBFOps: decodeBFOpsK2})
	res, err := s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "channel" {
		t.Fatalf("a new value under a covered name armed %q, want channel", res.Kinds)
	}
	if got := decodeBFReadRow(t, s, id2); got.channelHash.String != decodeBFOps {
		t.Errorf("id %d channel_hash = %q, want %q after the new value's pass", id2, got.channelHash.String, decodeBFOps)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if !covered[decodeBackfillKeyIdentity(decodeBFOps, channelKeyBytes(decodeBFOpsK2))] {
		t.Error("the new value is not covered after its pass")
	}
	if _, present := covered[decodeBackfillKeyIdentity(decodeBFOps, channelKeyBytes(decodeBFOpsK1))]; present {
		t.Error("the replaced value is still recorded")
	}
}

// The stored identity is a digest of name and value: different for two values
// of one name, and holding neither the key nor the name in clear.
func TestDecodeBackfillKeyIdentity_IsADigestOfNameAndValue(t *testing.T) {
	k1, k2 := channelKeyBytes(decodeBFOpsK1), channelKeyBytes(decodeBFOpsK2)
	a, b := decodeBackfillKeyIdentity(decodeBFOps, k1), decodeBackfillKeyIdentity(decodeBFOps, k2)
	if a == b {
		t.Fatal("two values under one name share an identity")
	}
	if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("identity %q is not a hex sha256", a)
	}
	for _, id := range []string{a, b} {
		for _, secret := range []string{decodeBFOpsK1, decodeBFOpsK2, decodeBFOps} {
			if strings.Contains(strings.ToLower(id), strings.ToLower(secret)) {
				t.Errorf("identity %q contains %q", id, secret)
			}
		}
	}
	// The hex spelling of a key does not change its identity.
	if decodeBackfillKeyIdentity(decodeBFOps, channelKeyBytes(strings.ToUpper(decodeBFOpsK1))) != a {
		t.Error("an upper-case spelling of the same key gave a different identity")
	}
	// Name and value are length-delimited: moving bytes between them changes it.
	if decodeBackfillKeyIdentity("ab", []byte("c")) == decodeBackfillKeyIdentity("a", []byte("bc")) {
		t.Error("name/value boundary is ambiguous")
	}
}

// ---------------------------------------------------------------------------
// F2: forgetting is driven by the removal, not by the next sample
// ---------------------------------------------------------------------------

// A key revoked and re-approved inside one batch: rows that arrived in the gap
// were stored as enc_<HEX>, so the key must not be credited by this pass.
func TestDecodeBackfill_FastRevokeReapproveMidRunRescansTheGap(t *testing.T) {
	s := decodeBFStore(t, "fast-midrun.db")
	key := deriveHashtagChannelKey(decodeBFChannel)
	decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: one", 0x31), nil, nil)
	decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: two", 0x32), nil, nil)
	hk := newHotKeys(nil, nil)
	decodeBFWatchRemovals(s, hk)
	hk.AddApproved(decodeBFChannel)

	var gap int64
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) {
		if gap != 0 {
			return
		}
		hk.RemoveApproved(decodeBFChannel)
		gap = decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: in the gap", 0x33), hk.Channels(), nil)
		hk.AddApproved(decodeBFChannel)
	}
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	if _, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(1, 1)); err != nil {
		t.Fatal(err)
	}
	decodeBackfillBatchHook = nil
	if got := decodeBFReadRow(t, s, gap); got.channelHash.String != "enc_33" {
		t.Fatalf("precondition: gap row channel_hash = %q, want enc_33", got.channelHash.String)
	}

	if _, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(1, 1)); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, gap); got.channelHash.String != decodeBFChannel {
		t.Errorf("gap row channel_hash = %q: a revoke and re-approve between two samples left the key credited", got.channelHash.String)
	}
}

// The same between two polls of an idle watcher.
func TestDecodeBackfill_RevokeReapproveBetweenPollsRescansTheGap(t *testing.T) {
	s := decodeBFStore(t, "fast-idle.db")
	key := deriveHashtagChannelKey(decodeBFChannel)
	decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: before", 0x31), nil, nil)
	hk := newHotKeys(nil, nil)
	decodeBFWatchRemovals(s, hk)
	hk.AddApproved(decodeBFChannel)
	set := decodeBFSettings(10, 1000)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}

	hk.RemoveApproved(decodeBFChannel)
	gap := decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: in the gap", 0x32), hk.Channels(), nil)
	hk.AddApproved(decodeBFChannel)

	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, gap); got.channelHash.String != decodeBFChannel {
		t.Errorf("gap row channel_hash = %q, want %q", got.channelHash.String, decodeBFChannel)
	}
}

// A hashChannels entry removed by one SIGHUP and restored by the next, and a
// region scope revoked and re-approved, are the same event.
func TestDecodeBackfill_ConfigRemoveReaddAcrossTwoReloadsRescansTheGap(t *testing.T) {
	s := decodeBFStore(t, "fast-reload.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	const scope = "#bf-reload"
	decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: before", 0x31), nil, nil)
	hk := newHotKeys(keys, nil)
	decodeBFWatchRemovals(s, hk)
	hk.AddApprovedRegions(scope)
	set := decodeBFSettings(10, 1000)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}

	hk.setBase(map[string]string{})
	hk.RemoveApprovedRegion(scope)
	chanGap := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: in the gap", 0x32), hk.Channels(), hk.Regions())
	scopeGap := decodeBFIngest(t, s, decodeBFTransportFrame(t, scope, []byte{0x09}), hk.Channels(), hk.Regions())
	hk.setBase(keys)
	hk.AddApprovedRegions(scope)

	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, chanGap); got.channelHash.String != decodeBFChannel {
		t.Errorf("channel gap row = %q, want %q", got.channelHash.String, decodeBFChannel)
	}
	if got := decodeBFReadRow(t, s, scopeGap); got.scopeName.String != scope {
		t.Errorf("scope gap row = %q, want %q", got.scopeName.String, scope)
	}
}

// While decodeBackfill.disabled is set no pass runs, but a revoke and
// re-approve made while that ingestor runs is still forgotten, so turning the
// pass back on re-reads the gap.
func TestDecodeBackfill_DisabledIngestorStillForgetsARevocation(t *testing.T) {
	s := decodeBFStore(t, "disabled-revoke.db")
	key := deriveHashtagChannelKey(decodeBFChannel)
	decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: before", 0x31), nil, nil)
	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(decodeBFChannelKeys(decodeBFChannel), nil), decodeBFSettings(10, 1000)); err != nil {
		t.Fatal(err)
	}

	// Disabled process. A key it carries but no pass covered must not be
	// backfilled: no pass runs.
	const unrelated = "#bf-unrelated"
	untouched := decodeBFIngest(t, s, decodeBFGrpTxt(t, deriveHashtagChannelKey(unrelated), "U: stays", 0x3F), nil, nil)
	hk := newHotKeys(decodeBFChannelKeys(unrelated), nil)
	decodeBFWatchRemovals(s, hk)
	hk.AddApproved(decodeBFChannel)
	off := decodeBFSettings(10, 1000)
	off.Enabled = false
	s.StartDecodeBackfill(hk, off)()
	hk.RemoveApproved(decodeBFChannel)
	gap := decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: in the gap", 0x32), hk.Channels(), nil)
	hk.AddApproved(decodeBFChannel)
	if got := decodeBFReadRow(t, s, untouched); got.channelHash.String != "enc_3F" {
		t.Fatalf("a disabled ingestor ran a pass: id %d channel_hash = %q", untouched, got.channelHash.String)
	}

	// Enabled again.
	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(decodeBFChannelKeys(decodeBFChannel), nil), decodeBFSettings(10, 1000)); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, gap); got.channelHash.String != decodeBFChannel {
		t.Errorf("gap row = %q, want %q", got.channelHash.String, decodeBFChannel)
	}
}

// A key dropped from the config across the restart into a disabled ingestor
// is forgotten at that start: no in-process event exists for it.
func TestDecodeBackfill_DisabledIngestorForgetsAKeyGoneAtStart(t *testing.T) {
	s := decodeBFStore(t, "disabled-restart.db")
	key := deriveHashtagChannelKey(decodeBFChannel)
	decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: before", 0x31), nil, nil)
	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(decodeBFChannelKeys(decodeBFChannel), nil), decodeBFSettings(10, 1000)); err != nil {
		t.Fatal(err)
	}

	hk := newHotKeys(nil, nil) // the entry was removed while the ingestor was down
	decodeBFWatchRemovals(s, hk)
	off := decodeBFSettings(10, 1000)
	off.Enabled = false
	s.StartDecodeBackfill(hk, off)()
	gap := decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: in the gap", 0x32), hk.Channels(), nil)

	// Enabled again with the entry restored.
	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(decodeBFChannelKeys(decodeBFChannel), nil), decodeBFSettings(10, 1000)); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, gap); got.channelHash.String != decodeBFChannel {
		t.Errorf("gap row = %q, want %q", got.channelHash.String, decodeBFChannel)
	}
}

// A disabled ingestor on a database no pass has touched must not create the
// bookkeeping tables (healthz keeps reporting "absent").
func TestDecodeBackfill_DisabledIngestorCreatesNoTables(t *testing.T) {
	s := decodeBFStore(t, "disabled-fresh.db")
	hk := newHotKeys(decodeBFChannelKeys(decodeBFChannel), nil)
	decodeBFWatchRemovals(s, hk)
	off := decodeBFSettings(10, 1000)
	off.Enabled = false
	s.StartDecodeBackfill(hk, off)()
	hk.setBase(map[string]string{})
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'decode_backfill_%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d decode_backfill table(s) created by a disabled ingestor", n)
	}
}

// ---------------------------------------------------------------------------
// F3: a revocation inside the last batch
// ---------------------------------------------------------------------------

// The revocation lands between the batch's decode and its write: the batch
// must not persist what it decrypted with the revoked key, and the pass must
// not credit it. Another key's rows in the same batch are still written.
func TestDecodeBackfill_RevocationInsideTheLastBatchIsHonoured(t *testing.T) {
	s := decodeBFStore(t, "revoke-last.db")
	const kept = "#bf-kept"
	revokedRow := decodeBFIngest(t, s, decodeBFGrpTxt(t, deriveHashtagChannelKey(decodeBFChannel), "A: late", 0x31), nil, nil)
	keptRow := decodeBFIngest(t, s, decodeBFGrpTxt(t, deriveHashtagChannelKey(kept), "K: stays", 0x32), nil, nil)
	hk := newHotKeys(decodeBFChannelKeys(kept), nil)
	hk.AddApproved(decodeBFChannel)

	fired := false
	decodeBackfillPreWriteHook = func() {
		if !fired {
			fired = true
			hk.RemoveApproved(decodeBFChannel)
		}
	}
	t.Cleanup(func() { decodeBackfillPreWriteHook = nil })
	res, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000)) // one batch: the last
	if err != nil {
		t.Fatal(err)
	}
	decodeBackfillPreWriteHook = nil
	if !fired {
		t.Fatal("the pre-write hook never ran")
	}

	if got := decodeBFReadRow(t, s, revokedRow); got.channelHash.String != "enc_31" || strings.Contains(got.decodedJSON.String, "A: late") {
		t.Errorf("id %d = %+v: the batch persisted a row decrypted with a key revoked before its write", revokedRow, got)
	}
	if got := decodeBFReadRow(t, s, keptRow); got.channelHash.String != kept {
		t.Errorf("id %d channel_hash = %q, want %q: a revocation of one key must not drop another key's rows", keptRow, got.channelHash.String, kept)
	}
	if res.ChannelUpdated != 1 {
		t.Errorf("ChannelUpdated = %d, want 1", res.ChannelUpdated)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := covered[decodeBFIdentity(decodeBFChannel)]; present {
		t.Error("the key revoked inside the last batch was credited")
	}
	if !covered[decodeBFIdentity(kept)] {
		t.Error("the key that stayed was not credited")
	}
}

// A revocation after the last batch committed but before the pass finished
// must not be credited either (no removal hook wired here, so only the final
// re-read can catch it).
func TestDecodeBackfill_RevocationAfterTheLastBatchIsNotCredited(t *testing.T) {
	s := decodeBFStore(t, "revoke-finish.db")
	decodeBFIngest(t, s, decodeBFGrpTxt(t, deriveHashtagChannelKey(decodeBFChannel), "A: one", 0x31), nil, nil)
	hk := newHotKeys(nil, nil)
	hk.AddApproved(decodeBFChannel)
	decodeBackfillBatchHook = func(b decodeBackfillBatchResult) {
		if b.Done {
			hk.RemoveApproved(decodeBFChannel)
		}
	}
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	if _, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000)); err != nil {
		t.Fatal(err)
	}
	decodeBackfillBatchHook = nil
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := covered[decodeBFIdentity(decodeBFChannel)]; present {
		t.Error("a key revoked after the last batch was credited by finish")
	}
}

// ---------------------------------------------------------------------------
// F4: a kind whose keys are all revoked stops selecting its candidates
// ---------------------------------------------------------------------------

func TestDecodeBackfill_RevokedKindStopsSelectingItsCandidates(t *testing.T) {
	s := decodeBFStore(t, "revoke-narrow.db")
	const scope = "#bf-narrow"
	key := deriveHashtagChannelKey(decodeBFChannel)
	c1 := decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: one", 0x31), nil, nil)
	s1 := decodeBFIngest(t, s, decodeBFTransportFrame(t, scope, []byte{0x11}), nil, nil)
	c2 := decodeBFIngest(t, s, decodeBFGrpTxt(t, key, "A: two", 0x32), nil, nil)
	s2 := decodeBFIngest(t, s, decodeBFTransportFrame(t, scope, []byte{0x12}), nil, nil)

	hk := newHotKeys(nil, decodeBFRegionKeys(scope))
	hk.AddApproved(decodeBFChannel)
	revoked := false
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) {
		if !revoked {
			revoked = true
			hk.RemoveApproved(decodeBFChannel)
		}
	}
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	res, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	decodeBackfillBatchHook = nil

	if got := decodeBFReadRow(t, s, c1); got.channelHash.String != decodeBFChannel {
		t.Fatalf("precondition: id %d = %q", c1, got.channelHash.String)
	}
	if got := decodeBFReadRow(t, s, c2); got.channelHash.String != "enc_32" {
		t.Errorf("id %d = %q after its key was revoked", c2, got.channelHash.String)
	}
	for _, id := range []int64{s1, s2} {
		if got := decodeBFReadRow(t, s, id); got.scopeName.String != scope {
			t.Errorf("id %d scope_name = %q, want %q", id, got.scopeName.String, scope)
		}
	}
	// c1 before the revocation, then only the two scope rows: c2 is no longer a
	// candidate once no channel key is left.
	if res.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3: the pass kept selecting channel rows nothing could resolve", res.Scanned)
	}
	st, err := s.DecodeBackfillState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Scanned != 3 {
		t.Errorf("persisted scanned = %d, want 3", st.Scanned)
	}
}

// ---------------------------------------------------------------------------
// hotKeys reports effective removals only
// ---------------------------------------------------------------------------

func TestHotKeys_OnRemovedReportsEffectiveRemovalsOnly(t *testing.T) {
	hk := newHotKeys(map[string]string{"#a": deriveHashtagChannelKey("#a"), decodeBFOps: decodeBFOpsK1}, nil)
	var got []removedKey
	hk.onRemoved(func(r []removedKey) { got = append(got, r...) })

	hk.AddApproved("#a", "#b")
	if hk.RemoveApproved("#a"); len(got) != 0 {
		t.Errorf("#a is still in the config layer, so revoking its approval removed nothing: %+v", got)
	}
	hk.RemoveApproved("#b")
	if len(got) != 1 || got[0].kind != decodeBackfillKindChannel || got[0].name != "#b" {
		t.Fatalf("revoking #b reported %+v", got)
	}
	got = nil
	hk.setBase(map[string]string{"#a": deriveHashtagChannelKey("#a"), decodeBFOps: decodeBFOpsK2})
	if len(got) != 1 || got[0].name != decodeBFOps || !bytes.Equal(got[0].key, channelKeyBytes(decodeBFOpsK1)) {
		t.Fatalf("replacing Ops' value reported %+v, want the old value", got)
	}
	got = nil
	hk.AddApprovedRegions("#r")
	hk.RemoveApprovedRegion("#r")
	if len(got) != 1 || got[0].kind != decodeBackfillKindScope || got[0].name != "#r" {
		t.Fatalf("revoking region #r reported %+v", got)
	}
}

// A keys table from a build that recorded sha256(name) is replaced, so its
// digests (which match no identity) cannot make a resumed run use no key.
func TestEnsureDecodeBackfillTables_ReplacesTheNameOnlyKeysTable(t *testing.T) {
	s := decodeBFStore(t, "legacy.db")
	if _, err := s.db.Exec(`CREATE TABLE decode_backfill_keys (kind TEXT NOT NULL, name_hash TEXT NOT NULL, state TEXT NOT NULL, PRIMARY KEY (kind, name_hash))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO decode_backfill_keys VALUES ('channel', 'deadbeef', 'covered')`); err != nil {
		t.Fatal(err)
	}
	if err := ensureDecodeBackfillTables(s.db); err != nil {
		t.Fatal(err)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatalf("keys table not migrated: %v", err)
	}
	if len(covered) != 0 {
		t.Errorf("legacy rows survived: %v", covered)
	}
	// Idempotent on the new layout.
	if err := ensureDecodeBackfillTables(s.db); err != nil {
		t.Fatal(err)
	}
}
