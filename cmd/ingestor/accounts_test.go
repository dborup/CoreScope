package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/account"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// #1018 M1 — owner accounts, the writer side. Every advert is
// firmware-shaped (AdvertDataBuilder::encodeTo) and signed with a TEST key
// from testKey; no real node key is used.

const (
	loginCode = "3HQ7ZB"
	loginA    = "e000000000000001"
	loginB    = "e000000000000002"
	writeA    = "f000000000000001"
	writeB    = "f000000000000002"
	writeC    = "f000000000000003"
)

// Advert types (firmware src/helpers/AdvertDataHelpers.h:7-11).
const (
	advChat     byte = 0x01
	advRepeater byte = 0x02
)

type accountRig struct {
	*claimRig
	acc   *accountRunner
	aq    *channelregistry.Queue
	hash  string
	nodeA string // a node key for My-nodes entries
}

func newAccountRig(t *testing.T, cfg *account.Config) *accountRig {
	t.Helper()
	cr := newClaimRig(t, nil)
	r := &accountRig{claimRig: cr, hash: account.HashRedeemSecret(strings.Repeat("7", 32)), nodeA: strings.Repeat("a1", 32)}
	r.acc = newAccountRunner(cr.store, cfg)
	r.acc.now = func() time.Time { return r.clock }
	cr.store.accounts = r.acc
	r.aq = channelregistry.NewQueue(account.QueueDir(cr.store.path))
	return r
}

// companionAdvert sends a companion (chat) advert, without a position by
// default (the companion default, examples/companion_radio/NodePrefs.h:33).
func (r *accountRig) typedAdvert(role byte, name string, withPos bool) string {
	r.t.Helper()
	r.ts++
	var lat, lon *float64
	if withPos {
		lat, lon = &claimLat, &claimLon
	}
	raw := buildAdvertHex(r.pub, r.priv, r.ts, firmwareAppdata(role, lat, lon, name))
	r.ingest(raw)
	return raw
}

func (r *accountRig) send(c account.Command) {
	r.t.Helper()
	if c.CreatedAt == 0 {
		c.CreatedAt = r.clock.UnixMilli()
	}
	if err := r.aq.EnqueueValue(c.RequestID, c, 100); err != nil {
		r.t.Fatal(err)
	}
	r.acc.RunOnce(context.Background())
}

func (r *accountRig) login(id, code string) {
	r.t.Helper()
	r.send(account.Command{RequestID: id, Op: account.OpLogin, PubKey: r.pk, Code: code, RedeemHash: r.hash})
}

func (r *accountRig) res(id string) account.Result {
	r.t.Helper()
	var res account.Result
	if queued, err := r.aq.LookupValue(id, &res); err != nil || queued {
		r.t.Fatalf("result %s: queued=%v err=%v", id, queued, err)
	}
	return res
}

func (r *accountRig) proof(id string) (status, aid string) {
	r.t.Helper()
	var a sql.NullString
	if err := r.store.db.QueryRow(`SELECT status, account_id FROM account_proofs WHERE request_id = ?`, id).Scan(&status, &a); err != nil {
		r.t.Fatalf("proof %s: %v", id, err)
	}
	return status, a.String
}

func (r *accountRig) gen(aid string) int64 {
	r.t.Helper()
	var g int64
	if err := r.store.db.QueryRow(`SELECT session_gen FROM accounts WHERE account_id = ?`, aid).Scan(&g); err != nil {
		r.t.Fatalf("gen %s: %v", aid, err)
	}
	return g
}

func (r *accountRig) count(q string, args ...interface{}) int {
	r.t.Helper()
	var n int
	if err := r.store.db.QueryRow(q, args...).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// signIn proves a fresh sign-in and returns the account id.
func (r *accountRig) signIn(id string) string {
	r.t.Helper()
	r.login(id, loginCode)
	r.typedAdvert(advChat, "Anna "+loginCode, false)
	st, aid := r.proof(id)
	if st != account.StatusProven || !account.ValidAccountID(aid) {
		r.t.Fatalf("sign-in not proven: %s %q", st, aid)
	}
	return aid
}

func TestAccount1018_CompanionAdvertSignsIn(t *testing.T) {
	r := newAccountRig(t, nil)
	r.login(loginA, loginCode)
	if res := r.res(loginA); res.Status != account.StatusPending {
		t.Fatalf("login result %+v", res)
	}
	raw := r.typedAdvert(advChat, "Anna T-Echo "+loginCode, false)
	st, aid := r.proof(loginA)
	if st != account.StatusProven || !account.ValidAccountID(aid) {
		t.Fatalf("status %s account %q", st, aid)
	}
	if strings.Contains(aid, r.pk[:8]) {
		t.Fatal("the account id must not derive from the companion key")
	}
	var hash string
	var provenAt int64
	r.store.db.QueryRow(`SELECT proof_hash, proven_at FROM account_proofs WHERE request_id = ?`, loginA).Scan(&hash, &provenAt)
	if hash != ComputeContentHash(raw) || provenAt != r.clock.UnixMilli() {
		t.Fatalf("proof hash %s at %d", hash, provenAt)
	}
	if n := r.count(`SELECT COUNT(*) FROM accounts WHERE identity_pubkey = ?`, r.pk); n != 1 {
		t.Fatalf("%d accounts for the companion", n)
	}
	// A second sign-in of the same companion reuses the account.
	r.clock = r.clock.Add(time.Minute)
	r.login(loginB, "4JR8ZC")
	r.typedAdvert(advChat, "Anna 4JR8ZC", false)
	if _, aid2 := r.proof(loginB); aid2 != aid {
		t.Fatalf("second sign-in made account %q, want %q", aid2, aid)
	}
	var last int64
	r.store.db.QueryRow(`SELECT last_login_at FROM accounts WHERE account_id = ?`, aid).Scan(&last)
	if last != r.clock.UnixMilli() {
		t.Fatalf("last_login_at %d", last)
	}
}

// The 31-byte budget of a companion without a position, the 23-byte one
// with (firmware src/helpers/AdvertDataHelpers.cpp:4-29).
func TestAccount1018_NameBudget(t *testing.T) {
	r := newAccountRig(t, nil)
	r.login(loginA, loginCode)
	name := strings.Repeat("x", 24) + " " + loginCode // 31 bytes
	r.typedAdvert(advChat, name, true)                // with a position: cut at 23
	if st, _ := r.proof(loginA); st != account.StatusPending {
		t.Fatalf("a code past the 23-byte budget proved a sign-in: %s", st)
	}
	r.typedAdvert(advChat, name, false) // no position: 31 bytes on air
	if st, _ := r.proof(loginA); st != account.StatusProven {
		t.Fatalf("a code within the 31-byte budget did not prove: %s", st)
	}
}

func TestAccount1018_RefusedAdverts(t *testing.T) {
	otherPub, otherPriv := testKey(2)
	cases := []struct {
		name string
		send func(r *accountRig)
	}{
		// Owner decision 2: a repeater cannot be an account, whatever its
		// name carries.
		{"repeater advert with the code", func(r *accountRig) { r.typedAdvert(advRepeater, "Roof "+loginCode, true) }},
		{"wrong pubkey", func(r *accountRig) {
			r.ts++
			r.ingest(buildAdvertHex(otherPub, otherPriv, r.ts, firmwareAppdata(advChat, nil, nil, "Anna "+loginCode)))
		}},
		{"forged signature", func(r *accountRig) {
			r.ts++
			r.ingest(buildAdvertHex(r.pub, otherPriv, r.ts, firmwareAppdata(advChat, nil, nil, "Anna "+loginCode)))
		}},
		{"substring, not a token", func(r *accountRig) { r.typedAdvert(advChat, "Anna x"+loginCode, false) }},
		{"expired code", func(r *accountRig) {
			r.clock = r.clock.Add(16 * time.Minute)
			r.typedAdvert(advChat, "Anna "+loginCode, false)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newAccountRig(t, nil)
			r.login(loginA, loginCode)
			c.send(r)
			if st, aid := r.proof(loginA); st != account.StatusPending || aid != "" {
				t.Fatalf("status %s account %q, want pending", st, aid)
			}
			if n := r.count(`SELECT COUNT(*) FROM accounts`); n != 0 {
				t.Fatalf("%d accounts created", n)
			}
		})
	}
}

// Code replay: a re-received copy of the proving advert is not new, and a
// used code proves nothing again.
func TestAccount1018_CodeReplay(t *testing.T) {
	r := newAccountRig(t, nil)
	r.login(loginA, loginCode)
	raw := buildAdvertHex(r.pub, r.priv, r.ts+1, firmwareAppdata(advChat, nil, nil, "Anna "+loginCode))
	// The proving copy arrives first as a re-received one: never new.
	r.acc.observeAdvert(&Payload{PubKey: r.pk, Name: "Anna " + loginCode, SignatureValid: boolPtr(true), Flags: &AdvertFlags{Chat: true, Type: 1}}, "h", false)
	if st, _ := r.proof(loginA); st != account.StatusPending {
		t.Fatalf("a non-new advert proved a sign-in: %s", st)
	}
	r.ingest(raw)
	if st, _ := r.proof(loginA); st != account.StatusProven {
		t.Fatalf("the advert did not prove: %s", st)
	}
	// A second request, then the same advert again (identical content hash).
	r.login(loginB, loginCode)
	r.ingest(raw)
	if st, _ := r.proof(loginB); st != account.StatusPending {
		t.Fatalf("a replayed advert proved a new sign-in: %s", st)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestAccount1018_PendingCaps(t *testing.T) {
	r := newAccountRig(t, &account.Config{MaxPendingPerKey: 2, MaxPendingLogins: 3})
	r.login("e000000000000011", loginCode)
	r.login("e000000000000012", "4JR8ZC")
	r.login("e000000000000013", "5KS9ZD")
	if res := r.res("e000000000000013"); res.Status != account.RequestError || !strings.Contains(res.Error, "maximum") {
		t.Fatalf("per-key cap: %+v", res)
	}
	r.pk = strings.Repeat("b2", 32)
	r.login("e000000000000014", "6MT0ZE")
	r.pk = strings.Repeat("c3", 32)
	r.login("e000000000000015", "7NV1ZF")
	if res := r.res("e000000000000015"); res.Status != account.RequestError || !strings.Contains(res.Error, "too many") {
		t.Fatalf("global cap: %+v", res)
	}
}

func TestAccount1018_WritesAndGeneration(t *testing.T) {
	r := newAccountRig(t, &account.Config{MaxMyNodes: 2})
	aid := r.signIn(loginA)
	nodeB := strings.Repeat("b1", 32)
	nodeC := strings.Repeat("c1", 32)
	r.send(account.Command{RequestID: writeA, Op: account.OpMyNodesSet, AccountID: aid, Gen: 1,
		Nodes: []account.MyNode{{PubKey: r.nodeA, Name: "Roof", AddedAt: 1}, {PubKey: nodeB}}})
	if res := r.res(writeA); res.Status != account.RequestDone {
		t.Fatalf("set: %+v", res)
	}
	if n := r.count(`SELECT COUNT(*) FROM account_my_nodes WHERE account_id = ?`, aid); n != 2 {
		t.Fatalf("%d nodes after set", n)
	}
	r.send(account.Command{RequestID: writeB, Op: account.OpMyNodeAdd, AccountID: aid, Gen: 1, Nodes: []account.MyNode{{PubKey: nodeC}}})
	if res := r.res(writeB); res.Status != account.RequestError || !strings.Contains(res.Error, "maximum") {
		t.Fatalf("add past the cap: %+v", res)
	}
	r.send(account.Command{RequestID: writeC, Op: account.OpMyNodeRemove, AccountID: aid, Gen: 1, PubKey: nodeB})
	if n := r.count(`SELECT COUNT(*) FROM account_my_nodes WHERE account_id = ?`, aid); n != 1 {
		t.Fatalf("%d nodes after remove", n)
	}
	name := "Anna"
	r.send(account.Command{RequestID: "f000000000000004", Op: account.OpProfileSet, AccountID: aid, Gen: 1, DisplayName: &name})
	var dn sql.NullString
	r.store.db.QueryRow(`SELECT display_name FROM accounts WHERE account_id = ?`, aid).Scan(&dn)
	if dn.String != "Anna" {
		t.Fatalf("display name %q", dn.String)
	}

	// Sign out everywhere: the generation moves on, and a command queued
	// under the old one is refused (a mutant without the check applies it).
	r.send(account.Command{RequestID: "f000000000000005", Op: account.OpSignOutAll, AccountID: aid, Gen: 1})
	if g := r.gen(aid); g != 2 {
		t.Fatalf("gen %d after sign-out-all", g)
	}
	r.send(account.Command{RequestID: "f000000000000006", Op: account.OpMyNodeRemove, AccountID: aid, Gen: 1, PubKey: r.nodeA})
	if res := r.res("f000000000000006"); res.Status != account.RequestError || !strings.Contains(res.Error, "session has ended") {
		t.Fatalf("stale gen: %+v", res)
	}
	if n := r.count(`SELECT COUNT(*) FROM account_my_nodes WHERE account_id = ?`, aid); n != 1 {
		t.Fatal("a stale-generation command changed the list")
	}

	// Delete: the account, its list and its proofs go.
	r.send(account.Command{RequestID: "f000000000000007", Op: account.OpDelete, AccountID: aid, Gen: 2})
	if res := r.res("f000000000000007"); res.Status != account.RequestDone {
		t.Fatalf("delete: %+v", res)
	}
	for _, tbl := range []string{"accounts", "account_my_nodes", "account_proofs"} {
		if n := r.count(`SELECT COUNT(*) FROM ` + tbl); n != 0 {
			t.Fatalf("%d rows left in %s", n, tbl)
		}
	}
	r.send(account.Command{RequestID: "f000000000000008", Op: account.OpSignOutAll, AccountID: aid, Gen: 2})
	if res := r.res("f000000000000008"); res.Status != account.RequestError || res.Error != errAccountNotFound.Error() {
		t.Fatalf("write after delete: %+v", res)
	}
}

func TestAccount1018_InvalidAndExpiredCommands(t *testing.T) {
	r := newAccountRig(t, nil)
	// A command file that breaks the shared rules (an upper-case key).
	r.send(account.Command{RequestID: loginA, Op: account.OpLogin, PubKey: strings.ToUpper(r.pk), Code: loginCode, RedeemHash: r.hash})
	if res := r.res(loginA); res.Status != account.RequestError {
		t.Fatalf("invalid: %+v", res)
	}
	r.send(account.Command{RequestID: loginB, Op: account.OpLogin, PubKey: r.pk, Code: loginCode, RedeemHash: r.hash,
		CreatedAt: r.clock.Add(-20 * time.Minute).UnixMilli()})
	if res := r.res(loginB); res.Status != account.RequestError || !strings.Contains(res.Error, "expired") {
		t.Fatalf("stale login command: %+v", res)
	}
}

// Owner decision 9: no sign-in for 24 months deletes the account.
func TestAccount1018_InactiveAccountsDeleted(t *testing.T) {
	r := newAccountRig(t, nil)
	aid := r.signIn(loginA)
	r.send(account.Command{RequestID: writeA, Op: account.OpMyNodesSet, AccountID: aid, Gen: 1, Nodes: []account.MyNode{{PubKey: r.nodeA}}})
	r.clock = r.clock.Add(23 * 30 * 24 * time.Hour)
	r.acc.Prune(context.Background())
	if n := r.count(`SELECT COUNT(*) FROM accounts`); n != 1 {
		t.Fatal("an account 23 months old was deleted")
	}
	if n := r.count(`SELECT COUNT(*) FROM account_proofs`); n != 0 {
		t.Fatal("the finished sign-in row outlived its retention")
	}
	r.clock = r.clock.Add(2 * 31 * 24 * time.Hour)
	r.acc.Prune(context.Background())
	if n := r.count(`SELECT COUNT(*) FROM accounts`) + r.count(`SELECT COUNT(*) FROM account_my_nodes`); n != 0 {
		t.Fatalf("%d rows left after 25 months without a sign-in", n)
	}
}

func TestAccount1018_KeyFileIs0600AndKept(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	if err := accountLoadOrCreateKey(db); err != nil {
		t.Fatal(err)
	}
	p := account.KeyPath(db)
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != account.KeyLen {
		t.Fatalf("key file %v %v", info, err)
	}
	first, _ := os.ReadFile(p)
	os.Chmod(p, 0o644)
	if err := accountLoadOrCreateKey(db); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(p)
	info, _ = os.Stat(p)
	if string(first) != string(again) || info.Mode().Perm() != 0o600 {
		t.Fatal("an existing key must be kept and tightened to 0600")
	}
	if k, err := account.ReadKey(db); err != nil || string(k) != string(first) {
		t.Fatalf("the server reads it back: %v", err)
	}
}

// The existing antenna-claim ownership rules are unchanged with the account
// hook beside them: a repeater's advert carrying its claim code still proves
// the claim, and a sign-in code proves no claim.
func TestAccount1018_AntennaClaimsUnchanged(t *testing.T) {
	r := newAccountRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	r.login(loginA, loginCode)
	r.typedAdvert(advRepeater, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusProven {
		t.Fatalf("claim %s, want proven", row.status)
	}
	if st, _ := r.proof(loginA); st != account.StatusPending {
		t.Fatalf("a repeater's claim advert signed in: %s", st)
	}
	r.claim(claimB, 20, "8PW2ZG")
	r.typedAdvert(advChat, "Anna "+loginCode, true)
	if row := r.row(claimB); row.status != antennaclaim.StatusPending {
		t.Fatalf("a sign-in advert touched a claim: %s", row.status)
	}
}

// Perf note for the advert hook: one map lookup per companion advert. The
// benchmark runs the hook with 512 pending sign-ins (the default cap) on
// other keys.
func BenchmarkAccount1018_ObserveAdvert(b *testing.B) {
	r := &accountRunner{now: time.Now, pending: map[string][]pendingLogin{}}
	for i := 0; i < 512; i++ {
		pk := strings.Repeat("0", 60) + strings.Repeat(string("0123456789abcdef"[i%16]), 1) + "000"
		pk = pk[:56] + string("0123456789abcdef"[i/16%16]) + pk[57:]
		r.pending[pk] = append(r.pending[pk], pendingLogin{id: "x", code: loginCode, expiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}
	p := &Payload{PubKey: strings.Repeat("f", 64), Name: "Anna " + loginCode, SignatureValid: boolPtr(true), Flags: &AdvertFlags{Chat: true, Type: 1}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.observeAdvert(p, "h", true)
	}
}
