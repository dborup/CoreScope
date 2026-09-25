package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

type proposalFixture struct {
	t      *testing.T
	dbPath string
	store  *Store
	keys   *hotKeys
	runner *channelProposalRunner
	clock  time.Time
}

func newProposalFixture(t *testing.T, cfg *channelregistry.Config) *proposalFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "meshcore.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &proposalFixture{t: t, dbPath: dbPath, store: store, clock: time.UnixMilli(1_700_000_000_000)}
	f.keys = newHotKeys(map[string]string{"Public": "8b3387e9c5cdea6ac9e5edbaa115cd72"}, nil)
	f.runner = newChannelProposalRunner(store, f.keys, cfg)
	f.runner.now = func() time.Time { return f.clock }
	return f
}

// enqueue writes a command the way the server does and returns its request id.
func (f *proposalFixture) enqueue(op, nameOrID string) string {
	f.t.Helper()
	cmd := channelregistry.Command{RequestID: channelregistry.NewID(), Op: op, CreatedAt: f.clock.UnixMilli()}
	if op == channelregistry.OpSubmit {
		cmd.Name = nameOrID
	} else {
		cmd.ProposalID = nameOrID
	}
	if err := f.runner.queue.Enqueue(cmd, 1000); err != nil {
		f.t.Fatal(err)
	}
	return cmd.RequestID
}

// run applies the queue and returns the status of request id.
func (f *proposalFixture) run(id string) channelregistry.RequestStatus {
	f.t.Helper()
	f.runner.RunOnce(context.Background())
	st, err := f.runner.queue.Lookup(id)
	if err != nil {
		f.t.Fatalf("lookup %s: %v", id, err)
	}
	return st
}

func (f *proposalFixture) count(where string) int {
	f.t.Helper()
	var n int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM channel_proposals WHERE ` + where).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestChannelProposalSubmitDuplicatesAndCase(t *testing.T) {
	f := newProposalFixture(t, nil)
	st := f.run(f.enqueue(channelregistry.OpSubmit, "  Test "))
	if st.Status != channelregistry.RequestPending || st.Proposal == nil || st.Proposal.Name != "#Test" {
		t.Fatalf("submit = %+v", st)
	}
	if st.Proposal.CreatedAt != f.clock.UnixMilli() || st.Proposal.ReviewedAt != nil {
		t.Fatalf("timestamps = %+v", st.Proposal)
	}
	first := st.Proposal.ID

	// Same name from another request: reported, not duplicated.
	dup := f.run(f.enqueue(channelregistry.OpSubmit, "#Test"))
	if dup.Status != channelregistry.RequestPending || dup.Proposal == nil || dup.Proposal.ID != first {
		t.Fatalf("duplicate = %+v", dup)
	}
	// Different case is a different channel (different key).
	lower := f.run(f.enqueue(channelregistry.OpSubmit, "#test"))
	if lower.Status != channelregistry.RequestPending || lower.Proposal.ID == first {
		t.Fatalf("#test must be its own proposal: %+v", lower)
	}
	if n := f.count("1=1"); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	// Invalid names are rejected by the ingestor too (the queue is not trusted).
	bad := f.run(f.enqueue(channelregistry.OpSubmit, "#"+strings.Repeat("x", 40)))
	if bad.Status != channelregistry.RequestError || bad.Error != channelregistry.ErrNameTooLong.Error() {
		t.Fatalf("too long = %+v", bad)
	}
}

func TestChannelProposalLimits(t *testing.T) {
	f := newProposalFixture(t, &channelregistry.Config{MaxPending: 2, MaxApproved: 1})
	a := f.run(f.enqueue(channelregistry.OpSubmit, "#a")).Proposal.ID
	b := f.run(f.enqueue(channelregistry.OpSubmit, "#b")).Proposal.ID
	over := f.run(f.enqueue(channelregistry.OpSubmit, "#c"))
	if over.Status != channelregistry.RequestError || over.Error != errTooManyPending.Error() {
		t.Fatalf("pending limit = %+v", over)
	}
	if st := f.run(f.enqueue(channelregistry.OpApprove, a)); st.Status != channelregistry.RequestApproved {
		t.Fatalf("approve a = %+v", st)
	}
	full := f.run(f.enqueue(channelregistry.OpApprove, b))
	if full.Status != channelregistry.RequestError || full.Error != errTooManyApproved.Error() {
		t.Fatalf("approved limit = %+v", full)
	}
	if _, ok := f.keys.Channels()["#b"]; ok {
		t.Fatal("#b must not be keyed when its approval failed")
	}
}

func TestChannelProposalReviewTransitions(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#test")).Proposal.ID
	f.clock = f.clock.Add(time.Minute)

	st := f.run(f.enqueue(channelregistry.OpApprove, id))
	if st.Status != channelregistry.RequestApproved || st.Proposal.ReviewedAt == nil || *st.Proposal.ReviewedAt != f.clock.UnixMilli() {
		t.Fatalf("approve = %+v", st)
	}
	// Firmware docs: hashtag channel "#test" has key 9cd8fcf22a47333b591d96a2b848b73f.
	if got := f.keys.Channels()["#test"]; got != "9cd8fcf22a47333b591d96a2b848b73f" {
		t.Fatalf("approved key = %q", got)
	}
	if f.keys.Channels()["Public"] == "" {
		t.Fatal("approval dropped a configured key")
	}
	// Repeating the same decision is idempotent.
	if again := f.run(f.enqueue(channelregistry.OpApprove, id)); again.Status != channelregistry.RequestApproved || again.Error != "" {
		t.Fatalf("repeat approve = %+v", again)
	}
	// A conflicting decision fails and reports the stored state.
	conflict := f.run(f.enqueue(channelregistry.OpReject, id))
	if conflict.Status != channelregistry.RequestError || conflict.Error != "suggestion was already approved" ||
		conflict.Proposal == nil || conflict.Proposal.Status != channelregistry.StatusApproved {
		t.Fatalf("conflicting reject = %+v", conflict)
	}
	if missing := f.run(f.enqueue(channelregistry.OpApprove, "ffffffffffffffff")); missing.Error != errProposalNotFound.Error() {
		t.Fatalf("unknown id = %+v", missing)
	}

	other := f.run(f.enqueue(channelregistry.OpSubmit, "#other")).Proposal.ID
	rej := f.run(f.enqueue(channelregistry.OpReject, other))
	if rej.Status != channelregistry.RequestRejected || rej.Proposal.ReviewedAt == nil {
		t.Fatalf("reject = %+v", rej)
	}
	if _, ok := f.keys.Channels()["#other"]; ok {
		t.Fatal("rejected channel must not be keyed")
	}
	if late := f.run(f.enqueue(channelregistry.OpApprove, other)); late.Error != "suggestion was already rejected" {
		t.Fatalf("approve after reject = %+v", late)
	}
}

// A crash after the commit but before the result is written leaves the
// command in the queue. Applying it again must not duplicate or fail.
func TestChannelProposalCrashReplayIsIdempotent(t *testing.T) {
	f := newProposalFixture(t, nil)
	ctx := context.Background()

	submitID := f.enqueue(channelregistry.OpSubmit, "#Replay")
	pending, _ := f.runner.queue.Pending()
	f.runner.apply(ctx, pending[0].Command) // committed, "crash" before Complete
	st := f.run(submitID)
	if st.Status != channelregistry.RequestPending || st.Proposal.ID != submitID {
		t.Fatalf("replayed submit = %+v", st)
	}
	if n := f.count("1=1"); n != 1 {
		t.Fatalf("rows after replay = %d", n)
	}

	approveID := f.enqueue(channelregistry.OpApprove, submitID)
	pending, _ = f.runner.queue.Pending()
	f.runner.apply(ctx, pending[0].Command) // approved in the DB, keys not updated yet
	if _, ok := f.keys.Channels()["#Replay"]; ok {
		t.Fatal("precondition: keys must not be updated by apply alone")
	}
	st = f.run(approveID)
	if st.Status != channelregistry.RequestApproved || st.Error != "" {
		t.Fatalf("replayed approve = %+v", st)
	}
	if _, ok := f.keys.Channels()["#Replay"]; !ok {
		t.Fatal("replayed approval must add the key")
	}
}

func TestChannelProposalExpiredAndMalformedCommands(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.enqueue(channelregistry.OpSubmit, "#late")
	f.clock = f.clock.Add(25 * time.Hour)
	if st := f.run(id); st.Status != channelregistry.RequestError || st.Error != errRequestExpired.Error() {
		t.Fatalf("expired = %+v", st)
	}
	if n := f.count("1=1"); n != 0 {
		t.Fatalf("expired command wrote %d row(s)", n)
	}
	junk := filepath.Join(f.runner.queue.Dir(), "cmd-0123456789abcdef.json")
	if err := os.WriteFile(junk, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runner.RunOnce(context.Background())
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatal("malformed command must be discarded")
	}
}

func TestChannelProposalRetention(t *testing.T) {
	f := newProposalFixture(t, &channelregistry.Config{RetentionDays: 30})
	day := int64(24 * 60 * 60 * 1000)
	now := f.clock.UnixMilli()
	rows := []struct {
		id, name, status string
		created          int64
		reviewed         interface{}
	}{
		{"1111111111111111", "#oldRejected", "rejected", now - 40*day, now - 31*day},
		{"2222222222222222", "#newRejected", "rejected", now - 40*day, now - 1*day},
		{"3333333333333333", "#oldPending", "pending", now - 31*day, nil},
		{"4444444444444444", "#newPending", "pending", now - 1*day, nil},
		{"5555555555555555", "#oldApproved", "approved", now - 400*day, now - 399*day},
	}
	for _, r := range rows {
		if _, err := f.store.db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES (?, ?, ?, ?, ?)`,
			r.id, r.name, r.status, r.created, r.reviewed); err != nil {
			t.Fatal(err)
		}
	}
	f.runner.Prune(context.Background())
	var names []string
	got, err := channelregistry.ListProposals(context.Background(), f.store.db, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		names = append(names, p.Name)
	}
	// Newest first; the old rejected and old pending rows are gone, the
	// approved one is kept however old it is.
	if got := strings.Join(names, ","); got != "#newPending,#newRejected,#oldApproved" {
		t.Fatalf("kept = %s", got)
	}
}

// Approvals survive a restart: a fresh process loads them into the keys.
func TestChannelProposalRestartPersistence(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#Persist")).Proposal.ID
	pendingID := f.run(f.enqueue(channelregistry.OpSubmit, "#StillPending")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))
	f.store.Close()

	store, err := OpenStore(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys := newHotKeys(map[string]string{}, nil)
	runner := newChannelProposalRunner(store, keys, nil)
	n, err := runner.LoadApproved(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("LoadApproved = %d, %v", n, err)
	}
	if keys.Channels()["#Persist"] != deriveHashtagChannelKey("#Persist") {
		t.Fatal("approved channel not keyed after restart")
	}
	p, found, err := channelregistry.GetProposal(context.Background(), store.db, pendingID)
	if err != nil || !found || p.Status != channelregistry.StatusPending {
		t.Fatalf("pending proposal after restart = %+v, %v, %v", p, found, err)
	}
}

// SIGHUP rebuilds the config-derived keys; approved channels must survive
// it, and a manually configured key for the same name keeps priority.
func TestChannelProposalKeysSurviveSIGHUPAndConfigWins(t *testing.T) {
	configPath := writeTestConfig(t, []string{"#alpha"}, nil)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	hk := newHotKeys(loadChannelKeys(cfg, configPath), loadRegionKeys(cfg))
	if hk.AddApproved("#Shared", "#Manual") != 2 || hk.AddApproved("#Shared") != 0 {
		t.Fatal("AddApproved must report only new names")
	}

	manualKey := "00112233445566778899aabbccddeeff"
	if err := os.WriteFile(configPath, mustJSON(t, struct {
		HashChannels []string          `json:"hashChannels"`
		ChannelKeys  map[string]string `json:"channelKeys"`
	}{HashChannels: []string{"#alpha", "#beta"}, ChannelKeys: map[string]string{"#Manual": manualKey}}), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := startSIGHUPReload(hk, configPath)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := hk.Channels()["#beta"]; ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ch := hk.Channels()
	if _, ok := ch["#beta"]; !ok {
		t.Fatalf("SIGHUP did not reload: %v", ch)
	}
	if ch["#Shared"] != deriveHashtagChannelKey("#Shared") {
		t.Fatal("SIGHUP dropped an approved channel")
	}
	if ch["#Manual"] != manualKey {
		t.Fatalf("configured key must win over the approved one, got %q", ch["#Manual"])
	}
}

// The merged snapshot handed to readers is never mutated afterwards.
func TestHotKeysSnapshotIsImmutable(t *testing.T) {
	hk := newHotKeys(map[string]string{"#a": "1"}, nil)
	snap := hk.Channels()
	hk.AddApproved("#b")
	if _, ok := snap["#b"]; ok {
		t.Fatal("AddApproved mutated a published snapshot")
	}
	if _, ok := hk.Channels()["#b"]; !ok {
		t.Fatal("new snapshot lacks #b")
	}
}

// A database without the table (a server-side rolling-upgrade state) reads
// as empty rather than failing.
func TestChannelProposalMissingTableReadsEmpty(t *testing.T) {
	f := newProposalFixture(t, nil)
	if _, err := f.store.db.Exec(`DROP TABLE channel_proposals`); err != nil {
		t.Fatal(err)
	}
	if n, err := f.runner.LoadApproved(context.Background()); err != nil || n != 0 {
		t.Fatalf("LoadApproved on missing table = %d, %v", n, err)
	}
	list, err := channelregistry.ListProposals(context.Background(), f.store.db, "", 0)
	if err != nil || len(list) != 0 {
		t.Fatalf("ListProposals on missing table = %v, %v", list, err)
	}
	if _, found, err := channelregistry.GetProposal(context.Background(), f.store.db, "0123456789abcdef"); err != nil || found {
		t.Fatalf("GetProposal on missing table = %v, %v", found, err)
	}
	f.runner.Prune(context.Background()) // must not panic or log-spam
}

// ── revoke ──────────────────────────────────────────────────────────────

// Approve, then revoke: the row moves to revoked, reviewed_at is updated
// again, and the key drops out of the merged map handleMessage actually
// reads for decoding (main.go: handleMessage(..., keys.Channels(), ...)) —
// unless a manually configured base key for the same name exists, in which
// case decoding must keep working.
func TestChannelProposalRevokeRemovesKeyUnlessBaseConfigured(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#Revocable")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))
	if _, ok := f.keys.Channels()["#Revocable"]; !ok {
		t.Fatal("precondition: approved channel must be keyed")
	}
	f.clock = f.clock.Add(time.Minute)

	st := f.run(f.enqueue(channelregistry.OpRevoke, id))
	if st.Status != channelregistry.RequestRevoked || st.Proposal == nil ||
		st.Proposal.ReviewedAt == nil || *st.Proposal.ReviewedAt != f.clock.UnixMilli() {
		t.Fatalf("revoke = %+v", st)
	}
	if _, ok := f.keys.Channels()["#Revocable"]; ok {
		t.Fatal("revoked channel must no longer be keyed — traffic on it must not decrypt")
	}
	p, found, err := channelregistry.GetProposal(context.Background(), f.store.db, id)
	if err != nil || !found || p.Status != channelregistry.StatusRevoked {
		t.Fatalf("row after revoke = %+v, %v, %v", p, found, err)
	}

	// Now with a manually configured base key for the same name: base must
	// win, so the channel keeps decrypting even after the approval that
	// added it via `approved` is revoked.
	f2 := newProposalFixture(t, nil)
	f2.keys.setBase(map[string]string{"#Revocable": "deadbeefdeadbeefdeadbeefdeadbeef"})
	id2 := f2.run(f2.enqueue(channelregistry.OpSubmit, "#Revocable")).Proposal.ID
	f2.run(f2.enqueue(channelregistry.OpApprove, id2))
	f2.run(f2.enqueue(channelregistry.OpRevoke, id2))
	if got := f2.keys.Channels()["#Revocable"]; got != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Fatalf("base key must survive revoking the approval for the same name, got %q", got)
	}
}

// Replaying the exact same revoke command twice is a no-op success both
// times: no error, no further key-map mutation, no row change on the second
// application.
func TestChannelProposalRevokeIsIdempotent(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#Idem")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))

	reqID := f.enqueue(channelregistry.OpRevoke, id)
	first := f.run(reqID)
	if first.Status != channelregistry.RequestRevoked || first.Error != "" {
		t.Fatalf("first revoke = %+v", first)
	}
	// Re-run the queue again (nothing pending, harmless) then replay a fresh
	// revoke command for the same, already-revoked proposal.
	second := f.run(f.enqueue(channelregistry.OpRevoke, id))
	if second.Status != channelregistry.RequestRevoked || second.Error != "" {
		t.Fatalf("second revoke (already revoked) = %+v", second)
	}
	if n := f.count(`id = '` + id + `' AND status = 'revoked'`); n != 1 {
		t.Fatalf("row count after double revoke = %d", n)
	}
}

// A crash after the commit but before the result is written must not double
// -apply or corrupt state when the revoke command is replayed.
func TestChannelProposalRevokeCrashReplayIsIdempotent(t *testing.T) {
	f := newProposalFixture(t, nil)
	ctx := context.Background()
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#CrashRevoke")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))

	revokeID := f.enqueue(channelregistry.OpRevoke, id)
	pending, _ := f.runner.queue.Pending()
	f.runner.apply(ctx, pending[0].Command) // committed, "crash" before Complete
	if _, ok := f.keys.Channels()["#CrashRevoke"]; !ok {
		t.Fatal("precondition: apply alone must not touch keys yet")
	}
	st := f.run(revokeID) // replays the same command via RunOnce
	if st.Status != channelregistry.RequestRevoked || st.Error != "" {
		t.Fatalf("replayed revoke = %+v", st)
	}
	if _, ok := f.keys.Channels()["#CrashRevoke"]; ok {
		t.Fatal("replayed revoke must still remove the key")
	}
	if n := f.count(`id = '` + id + `' AND status = 'revoked'`); n != 1 {
		t.Fatalf("rows after crash-replay = %d", n)
	}
}

// approve → revoke → approve again: the second approve is a fresh decision
// on a row that is no longer pending (it is revoked), so it must be a
// conflict, not a silent re-approval.
func TestChannelProposalApproveRevokeApproveIsConflictNotSilentReapproval(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#Cycle")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))
	f.run(f.enqueue(channelregistry.OpRevoke, id))

	again := f.run(f.enqueue(channelregistry.OpApprove, id))
	if again.Status != channelregistry.RequestError || again.Error != "suggestion was already revoked" {
		t.Fatalf("approve of a revoked row = %+v, want conflict", again)
	}
	p, _, err := channelregistry.GetProposal(context.Background(), f.store.db, id)
	if err != nil || p.Status != channelregistry.StatusRevoked {
		t.Fatalf("status must stay revoked after the rejected re-approval attempt: %+v, %v", p, err)
	}
	if _, ok := f.keys.Channels()["#Cycle"]; ok {
		t.Fatal("the conflicting approve attempt must not have re-added the key")
	}
}

// Revoking a pending, a rejected, or an unknown id are all conflicts/not-
// found: no DB mutation beyond what already existed, no key-map mutation.
func TestChannelProposalRevokeOfNonApprovedProducesConflict(t *testing.T) {
	f := newProposalFixture(t, nil)
	pendingID := f.run(f.enqueue(channelregistry.OpSubmit, "#StillPending")).Proposal.ID
	rejectedID := f.run(f.enqueue(channelregistry.OpSubmit, "#WillReject")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpReject, rejectedID))

	if st := f.run(f.enqueue(channelregistry.OpRevoke, pendingID)); st.Status != channelregistry.RequestError || st.Error != "suggestion was already pending" {
		t.Fatalf("revoke pending = %+v", st)
	}
	if st := f.run(f.enqueue(channelregistry.OpRevoke, rejectedID)); st.Status != channelregistry.RequestError || st.Error != "suggestion was already rejected" {
		t.Fatalf("revoke rejected = %+v", st)
	}
	if st := f.run(f.enqueue(channelregistry.OpRevoke, "ffffffffffffffff")); st.Status != channelregistry.RequestError || st.Error != errProposalNotFound.Error() {
		t.Fatalf("revoke unknown = %+v", st)
	}
	p, _, _ := channelregistry.GetProposal(context.Background(), f.store.db, pendingID)
	if p.Status != channelregistry.StatusPending {
		t.Fatalf("pending row mutated by a failed revoke: %+v", p)
	}
	r, _, _ := channelregistry.GetProposal(context.Background(), f.store.db, rejectedID)
	if r.Status != channelregistry.StatusRejected {
		t.Fatalf("rejected row mutated by a failed revoke: %+v", r)
	}
	if _, ok := f.keys.Channels()["#StillPending"]; ok {
		t.Fatal("a failed revoke must never add a key")
	}
}

// A revoke must not be resurrected by a reload (the SIGHUP handler's
// config-reread path — exercised here by calling hk.reload directly, which
// is exactly what startSIGHUPReload calls, so this is not a weaker test)
// unless the operator actually configured the channel; when they did, base
// wins (same rule as for a still-approved channel, already covered by
// TestChannelProposalKeysSurviveSIGHUPAndConfigWins).
func TestChannelProposalRevokedKeySurvivesReloadUnlessBaseConfigured(t *testing.T) {
	configPath := writeTestConfig(t, nil, nil)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	hk := newHotKeys(loadChannelKeys(cfg, configPath), loadRegionKeys(cfg))
	hk.AddApproved("#Revocable", "#StillApproved")
	if !hk.RemoveApproved("#Revocable") {
		t.Fatal("RemoveApproved must report the name was present")
	}
	if hk.RemoveApproved("#Revocable") {
		t.Fatal("RemoveApproved must report false the second time")
	}
	if _, ok := hk.Channels()["#Revocable"]; ok {
		t.Fatal("precondition: must be unkeyed right after RemoveApproved")
	}

	// A config reload that does not mention the channel at all: it must stay
	// absent, not come back from anywhere, and an unrelated still-approved
	// channel must survive the reload untouched (reload must never wipe or
	// otherwise mutate the approved layer, only base).
	if err := hk.reload(configPath); err != nil {
		t.Fatal(err)
	}
	if _, ok := hk.Channels()["#Revocable"]; ok {
		t.Fatal("reload must not resurrect a revoked channel that is not configured")
	}
	if _, ok := hk.Channels()["#StillApproved"]; !ok {
		t.Fatal("reload must not wipe unrelated approved channels")
	}

	// Now #Revocable is configured under hashChannels: base wins, same as
	// for any other manually configured key.
	if err := os.WriteFile(configPath, mustJSON(t, struct {
		HashChannels []string `json:"hashChannels"`
	}{HashChannels: []string{"#Revocable"}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := hk.reload(configPath); err != nil {
		t.Fatal(err)
	}
	if _, ok := hk.Channels()["#Revocable"]; !ok {
		t.Fatal("a channel later configured under hashChannels must decrypt again, even though it was revoked")
	}
}

// Process restart after a revoke: LoadApproved only ever selects
// status = 'approved' (internal/channelregistry/sql.go), so a fresh process
// never re-adds a revoked channel to the keys.
func TestChannelProposalRevokeSurvivesRestart(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#RestartRevoke")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))
	f.run(f.enqueue(channelregistry.OpRevoke, id))
	f.store.Close()

	store, err := OpenStore(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys := newHotKeys(map[string]string{}, nil)
	runner := newChannelProposalRunner(store, keys, nil)
	n, err := runner.LoadApproved(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("LoadApproved after restart = %d, %v — a revoked channel must not be reloaded", n, err)
	}
	if _, ok := keys.Channels()["#RestartRevoke"]; ok {
		t.Fatal("revoked channel reappeared after restart")
	}
}

// Revoke against a database without the channel_proposals table behaves
// like the other missing-table paths: no panic, a sane not-found error.
func TestChannelProposalRevokeMissingTable(t *testing.T) {
	f := newProposalFixture(t, nil)
	if _, err := f.store.db.Exec(`DROP TABLE channel_proposals`); err != nil {
		t.Fatal(err)
	}
	st := f.run(f.enqueue(channelregistry.OpRevoke, "0123456789abcdef"))
	if st.Status != channelregistry.RequestError {
		t.Fatalf("revoke on missing table = %+v", st)
	}
}

// Re-suggesting the same name after a revoke lands as pending again (never
// auto-approved), reuses the SAME row id (UNIQUE(name)), and a replay of
// that exact resubmission request is itself idempotent.
func TestChannelProposalResubmitAfterRevokeLandsPendingWithSameID(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#Resuggest")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))
	f.run(f.enqueue(channelregistry.OpRevoke, id))
	if _, ok := f.keys.Channels()["#Resuggest"]; ok {
		t.Fatal("precondition: must be revoked (unkeyed) before resuggesting")
	}

	resubmitReqID := f.enqueue(channelregistry.OpSubmit, "#Resuggest")
	st := f.run(resubmitReqID)
	if st.Status != channelregistry.RequestPending {
		t.Fatalf("resubmit after revoke = %+v, want pending (never auto-approved)", st)
	}
	if st.Proposal.ID != id {
		t.Fatalf("resubmit after revoke got a new id %q, want the original %q", st.Proposal.ID, id)
	}
	if n := f.count("1=1"); n != 1 {
		t.Fatalf("resubmit after revoke must reuse the row, got %d rows", n)
	}
	if _, ok := f.keys.Channels()["#Resuggest"]; ok {
		t.Fatal("resubmission must not re-add the key: it is pending, not approved")
	}

	// Replay of the exact same resubmission request (e.g. after a crash
	// before its result was written) must be idempotent: same id, no new row.
	pending, err := channelregistry.ListProposals(context.Background(), f.store.db, "", 0)
	if err != nil || len(pending) != 1 {
		t.Fatalf("listing before replay: %v, %v", pending, err)
	}
	replayCmd := channelregistry.Command{RequestID: resubmitReqID, Op: channelregistry.OpSubmit, Name: "#Resuggest", CreatedAt: f.clock.UnixMilli()}
	replayed, err := f.store.submitChannelProposal(context.Background(), replayCmd, 100, f.clock.UnixMilli())
	if err != nil {
		t.Fatalf("replayed resubmit: %v", err)
	}
	if replayed.ID != id || replayed.Status != channelregistry.StatusPending {
		t.Fatalf("replayed resubmit = %+v, want id=%s status=pending", replayed, id)
	}
	if n := f.count("1=1"); n != 1 {
		t.Fatalf("rows after replayed resubmit = %d, want 1 (still idempotent)", n)
	}
}

// Retention: a revoked row older than the cutoff is pruned exactly like a
// rejected one; a newer one is kept; an approved row is never pruned.
func TestChannelProposalRevokeRetention(t *testing.T) {
	f := newProposalFixture(t, &channelregistry.Config{RetentionDays: 30})
	day := int64(24 * 60 * 60 * 1000)
	now := f.clock.UnixMilli()
	rows := []struct {
		id, name, status string
		created          int64
		reviewed         interface{}
	}{
		{"6666666666666666", "#oldRevoked", "revoked", now - 400*day, now - 31*day},
		{"7777777777777777", "#newRevoked", "revoked", now - 400*day, now - 1*day},
		{"8888888888888888", "#stillApproved", "approved", now - 400*day, now - 399*day},
	}
	for _, r := range rows {
		if _, err := f.store.db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES (?, ?, ?, ?, ?)`,
			r.id, r.name, r.status, r.created, r.reviewed); err != nil {
			t.Fatal(err)
		}
	}
	f.runner.Prune(context.Background())
	var names []string
	got, err := channelregistry.ListProposals(context.Background(), f.store.db, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "#newRevoked,#stillApproved" {
		t.Fatalf("kept = %s, want #newRevoked,#stillApproved", got)
	}
}
