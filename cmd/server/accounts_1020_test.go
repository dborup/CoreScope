package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/account"
)

// #1020 review fixes on the #1018 M1 server side:
//   F1 — sign out everywhere and delete never wait on a write budget, and
//        one account cannot spend the global window for everyone;
//   F2 — a proven sign-in is redeemed once, and never after a sign-out
//        everywhere (the proof is bound to its generation).

const accOtherAID = "8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b"

// signedInAs signs in a second account (aid, its own companion and proof).
func (e *acc1018Env) signedInAs(pk, aid, proofID string) string {
	e.t.Helper()
	e.addNode(pk, "Bo T-Deck", "companion", false)
	c := e.srv.db.conn
	ms := e.now.UnixMilli()
	if _, err := c.Exec(`INSERT INTO accounts (account_id, identity_pubkey, created_at, last_login_at) VALUES (?, ?, ?, ?)`, aid, pk, ms, ms); err != nil {
		e.t.Fatal(err)
	}
	if _, err := c.Exec(`INSERT INTO account_proofs (request_id, kind, account_id, pubkey, code, redeem_hash, status, created_at, expires_at, proven_at, proof_hash, proven_gen)
		VALUES (?, 'login', ?, ?, '3HQ7ZB', ?, 'proven', ?, ?, ?, 'abcd', 1)`, proofID, aid, pk, account.HashRedeemSecret(accSecret), ms, ms+15*60000, ms); err != nil {
		e.t.Fatal(err)
	}
	w := e.redeem(proofID, accSecret)
	if w.Code != http.StatusOK {
		e.t.Fatalf("redeem %s: %d %s", proofID, w.Code, w.Body)
	}
	return sessionCookie(w).Value
}

func acceptedID(t *testing.T, body string) string {
	t.Helper()
	var got AccountAcceptedResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.RequestID == "" {
		t.Fatalf("accepted body %q: %v", body, err)
	}
	return got.RequestID
}

func (e *acc1018Env) countOp(op string) int {
	n := 0
	for _, c := range e.allQueued() {
		if c.Op == op {
			n++
		}
	}
	return n
}

// F1: the reviewer's probe — five profile writes spend writesPerHour=5,
// then sign-out-all and DELETE /api/account must still be accepted.
func TestAccounts1020_RevocationNeverWaitsOnWriteBudget(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.WritesPerHour = 5 }))
	tok := e.signedIn(pk)
	for i := 0; i < 5; i++ {
		if w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tok)); w.Code != http.StatusAccepted {
			t.Fatalf("profile %d: %d", i, w.Code)
		}
	}
	if w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tok)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth write: %d, want 429", w.Code)
	}
	w := e.req("POST", "/api/account/sign-out-all", "", withCookie(tok))
	if w.Code != http.StatusAccepted {
		t.Fatalf("sign-out-all with the window spent: %d %s", w.Code, w.Body)
	}
	first := acceptedID(t, w.Body.String())
	// A stolen copy of the token repeats it: the same request, no new file.
	for i := 0; i < 20; i++ {
		w := e.req("POST", "/api/account/sign-out-all", "", withCookie(tok))
		if w.Code != http.StatusAccepted || acceptedID(t, w.Body.String()) != first {
			t.Fatalf("repeat %d: %d %s, want 202 with %s", i, w.Code, w.Body, first)
		}
	}
	if n := e.countOp(account.OpSignOutAll); n != 1 {
		t.Fatalf("%d sign-out-all commands queued, want 1", n)
	}
	if w := e.req("DELETE", "/api/account", "", withCookie(tok)); w.Code != http.StatusAccepted {
		t.Fatalf("delete with the window spent: %d %s", w.Code, w.Body)
	}
	// Revocations were never charged: the window is still exactly spent.
	if w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tok)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("profile after revocations: %d, want 429", w.Code)
	}

	// The first one failed at the ingestor: a retry queues afresh.
	if err := e.svc.queue.CompleteValue(first, account.Result{RequestID: first, Status: account.RequestError, Error: "the request could not be saved"}); err != nil {
		t.Fatal(err)
	}
	w = e.req("POST", "/api/account/sign-out-all", "", withCookie(tok))
	if w.Code != http.StatusAccepted || acceptedID(t, w.Body.String()) == first {
		t.Fatalf("retry after a failure: %d %s", w.Code, w.Body)
	}
}

// F1: a queue that ordinary writes filled does not stop a revocation (it
// has headroom), and a stolen session cannot fill the headroom either.
func TestAccounts1020_RevocationHasQueueHeadroom(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.MaxQueuedRequests = 2 }))
	tok := e.signedIn(pk) // queues one OpRedeem
	if w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tok)); w.Code != http.StatusAccepted {
		t.Fatalf("profile: %d", w.Code)
	}
	if w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tok)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("ordinary write into a full queue: %d, want 503", w.Code)
	}
	for i := 0; i < 5; i++ {
		if w := e.req("POST", "/api/account/sign-out-all", "", withCookie(tok)); w.Code != http.StatusAccepted {
			t.Fatalf("sign-out-all %d into a full queue: %d %s", i, w.Code, w.Body)
		}
	}
	if w := e.req("DELETE", "/api/account", "", withCookie(tok)); w.Code != http.StatusAccepted {
		t.Fatalf("delete into a full queue: %d", w.Code)
	}
	if n := len(e.allQueued()); n != 4 {
		t.Fatalf("%d commands queued, want 4 (redeem, profile, one sign-out-all, delete)", n)
	}
}

// F1: the per-account window stops one session from spending the global
// one; another account keeps writing.
func TestAccounts1020_PerAccountWriteWindow(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.WritesPerHourPerAcct = 3; c.WritesPerHour = 10 }))
	tokA := e.signedIn(pk789("a"))
	tokB := e.signedInAs(pk789("b"), accOtherAID, "e000000000000009")
	for i := 0; i < 3; i++ {
		if w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tokA)); w.Code != http.StatusAccepted {
			t.Fatalf("A write %d: %d", i, w.Code)
		}
	}
	w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tokA))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("A past its own window: %d", w.Code)
	}
	for i := 0; i < 3; i++ {
		if w := e.req("POST", "/api/account/profile", `{"displayName":"Bo"}`, withCookie(tokB)); w.Code != http.StatusAccepted {
			t.Fatalf("B write %d while A is refused: %d", i, w.Code)
		}
	}
	// A refused write charged neither window: 6 of the global 10 are used.
	tokC := e.signedInAs(pk789("c"), "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c", "e00000000000000a")
	for i := 0; i < 3; i++ {
		if w := e.req("POST", "/api/account/profile", `{"displayName":"Cy"}`, withCookie(tokC)); w.Code != http.StatusAccepted {
			t.Fatalf("C write %d: %d", i, w.Code)
		}
	}
	// A write the global window refuses does not spend the account's own
	// budget: C is refused twice with the global window full, then the
	// global window opens (the limit raised in place) and C still has its
	// three (a mutant without the refund leaves C at 429).
	e2 := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.WritesPerHourPerAcct = 2; c.WritesPerHour = 1 }))
	tokX := e2.signedIn(pk789("a"))
	tokY := e2.signedInAs(pk789("b"), accOtherAID, "e000000000000009")
	if w := e2.req("POST", "/api/account/profile", `{"displayName":"X"}`, withCookie(tokX)); w.Code != http.StatusAccepted {
		t.Fatalf("X: %d", w.Code)
	}
	for i := 0; i < 2; i++ {
		if w := e2.req("POST", "/api/account/profile", `{"displayName":"Y"}`, withCookie(tokY)); w.Code != http.StatusTooManyRequests {
			t.Fatalf("Y %d with the global window full: %d", i, w.Code)
		}
	}
	e2.svc.limits.WritesPerHour = 100
	for i := 0; i < 2; i++ {
		if w := e2.req("POST", "/api/account/profile", `{"displayName":"Y"}`, withCookie(tokY)); w.Code != http.StatusAccepted {
			t.Fatalf("Y %d once the global window opened: %d, want its own budget intact", i, w.Code)
		}
	}

	// An hour on, A's window is full again.
	e.now = e.now.Add(time.Hour)
	if w := e.req("POST", "/api/account/profile", `{"displayName":"Anna"}`, withCookie(tokA)); w.Code != http.StatusAccepted {
		t.Fatalf("A an hour later: %d", w.Code)
	}
}

// F2: a proof is redeemed once; the redeem is recorded for the ingestor,
// and a recorded redeem stays refused after a server restart.
func TestAccounts1020_RedeemOnce(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	e.addNode(pk, "Anna", "companion", false)
	e.proven("e000000000000001", pk, e.now)
	if w := e.redeem("e000000000000001", accSecret); w.Code != http.StatusOK {
		t.Fatalf("first redeem: %d %s", w.Code, w.Body)
	}
	if w := e.redeem("e000000000000001", accSecret); w.Code != http.StatusUnauthorized || sessionCookie(w) != nil {
		t.Fatalf("second redeem: %d, want 401 without a cookie", w.Code)
	}
	var marks []account.Command
	for _, c := range e.allQueued() {
		if c.Op == account.OpRedeem {
			marks = append(marks, c)
		}
	}
	if len(marks) != 1 || marks[0].ProofID != "e000000000000001" || marks[0].AccountID != accAID || marks[0].Gen != 1 {
		t.Fatalf("redeem records %+v", marks)
	}

	// A restarted server has no memory of it; the ingestor's record holds.
	e.srv.db.conn.Exec(`UPDATE account_proofs SET redeemed_at = ? WHERE request_id = 'e000000000000001'`, e.now.UnixMilli())
	fresh := newAccountService(e.srv.cfg, e.srv.db.conn, filepath.Join(t.TempDir(), "meshcore.db"))
	fresh.now = e.svc.now
	fresh.readKey = e.svc.readKey
	e.srv.accounts, e.svc = fresh, fresh
	if w := e.redeem("e000000000000001", accSecret); w.Code != http.StatusUnauthorized {
		t.Fatalf("redeem after a restart: %d, want 401", w.Code)
	}
}

// F2, the race: many redeems of one proof at once, exactly one session.
func TestAccounts1020_RedeemRaceOneWinner(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	e.addNode(pk, "Anna", "companion", false)
	e.proven("e000000000000001", pk, e.now)
	const n = 32
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = e.redeem("e000000000000001", accSecret).Code
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent redeems succeeded, want exactly 1", ok, n)
	}
	if got := e.countOp(account.OpRedeem); got != 1 {
		t.Fatalf("%d redeem records queued, want 1", got)
	}
}

// F2: the reviewer's probe — a proof made before a sign-out everywhere
// cannot be redeemed after it, even inside its 10-minute window. A redeem
// that raced ahead of the sign-out still dies with its generation.
func TestAccounts1020_NoRedeemAfterSignOutAll(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	tok := e.signedIn(pk) // proof e…01 at gen 1, redeemed
	e.proven("e000000000000003", pk, e.now)
	// The race: e…03 is redeemed while the sign-out-all is still queued.
	if w := e.req("POST", "/api/account/sign-out-all", "", withCookie(tok)); w.Code != http.StatusAccepted {
		t.Fatalf("sign-out-all: %d", w.Code)
	}
	w := e.redeem("e000000000000003", accSecret)
	raced := sessionCookie(w)
	if w.Code != http.StatusOK || raced == nil {
		t.Fatalf("redeem before the ingestor applied it: %d", w.Code)
	}
	e.proven("e000000000000004", pk, e.now)
	// The ingestor applies it (test fixture).
	e.srv.db.conn.Exec(`UPDATE accounts SET session_gen = session_gen + 1 WHERE account_id = ?`, accAID)
	if w := e.req("GET", "/api/account", "", withCookie(raced.Value)); w.Code != http.StatusUnauthorized {
		t.Fatalf("the raced session after the sign-out applied: %d, want 401", w.Code)
	}
	// e…04 was proven under gen 1; five minutes later, still in its window.
	e.now = e.now.Add(5 * time.Minute)
	if w := e.redeem("e000000000000004", accSecret); w.Code != http.StatusUnauthorized || sessionCookie(w) != nil {
		t.Fatalf("redeem of a pre-sign-out proof: %d, want 401", w.Code)
	}
	// A proof without a generation (a row from before the column) is refused.
	e.srv.db.conn.Exec(`INSERT INTO account_proofs (request_id, kind, account_id, pubkey, code, redeem_hash, status, created_at, expires_at, proven_at, proof_hash)
		VALUES ('e000000000000005', 'login', ?, ?, '3HQ7ZB', ?, 'proven', ?, ?, ?, 'abcd')`, accAID, pk, account.HashRedeemSecret(accSecret),
		e.now.UnixMilli(), e.now.Add(15*time.Minute).UnixMilli(), e.now.UnixMilli())
	if w := e.redeem("e000000000000005", accSecret); w.Code != http.StatusUnauthorized {
		t.Fatalf("redeem of a proof without proven_gen: %d, want 401", w.Code)
	}
	// A proof made under the new generation works.
	e.srv.db.conn.Exec(`INSERT INTO account_proofs (request_id, kind, account_id, pubkey, code, redeem_hash, status, created_at, expires_at, proven_at, proof_hash, proven_gen)
		VALUES ('e000000000000006', 'login', ?, ?, '3HQ7ZB', ?, 'proven', ?, ?, ?, 'abcd', 2)`, accAID, pk, account.HashRedeemSecret(accSecret),
		e.now.UnixMilli(), e.now.Add(15*time.Minute).UnixMilli(), e.now.UnixMilli())
	w = e.redeem("e000000000000006", accSecret)
	if c := sessionCookie(w); w.Code != http.StatusOK || c == nil {
		t.Fatalf("redeem under the new generation: %d", w.Code)
	} else if got := e.req("GET", "/api/account", "", withCookie(c.Value)); got.Code != http.StatusOK {
		t.Fatalf("new-generation session: %d", got.Code)
	}
}

// F2: when the redeem cannot be recorded (the queue is full even with its
// headroom) it is refused and not used up, so the owner can try again.
func TestAccounts1020_RedeemNotUsedUpWhenUnrecorded(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.MaxQueuedRequests = 1 }))
	e.addNode(pk, "Anna", "companion", false)
	e.proven("e000000000000001", pk, e.now)
	for _, id := range []string{"d000000000000001", "d000000000000002"} {
		if err := e.svc.queue.EnqueueValue(id, account.Command{RequestID: id}, 10); err != nil {
			t.Fatal(err)
		}
	}
	if w := e.redeem("e000000000000001", accSecret); w.Code != http.StatusServiceUnavailable || sessionCookie(w) != nil {
		t.Fatalf("redeem into a full queue: %d, want 503 without a cookie", w.Code)
	}
	e.svc.queue = channelregistry.NewQueue(filepath.Join(t.TempDir(), "q"))
	if w := e.redeem("e000000000000001", accSecret); w.Code != http.StatusOK {
		t.Fatalf("redeem once the queue drained: %d %s", w.Code, w.Body)
	}
}

// claimRedeem stays bounded: past its bound it prunes ended windows and
// refuses only while every entry is live.
func TestAccounts1020_RedeemClaimsBounded(t *testing.T) {
	a := &accountService{limits: account.Limits{MaxPendingLogins: 2}, redeemed: map[string]int64{}}
	if a.claimRedeem("a", 100, 0) != claimOK || a.claimRedeem("b", 100, 0) != claimOK {
		t.Fatal("first two claims")
	}
	if a.claimRedeem("a", 100, 0) != claimTaken {
		t.Fatal("a repeat claim")
	}
	if a.claimRedeem("c", 100, 50) != claimFull {
		t.Fatal("a third live claim past the bound")
	}
	if a.claimRedeem("c", 300, 200) != claimOK || len(a.redeemed) != 1 {
		t.Fatalf("after the windows ended: %v", a.redeemed)
	}
}

func TestAccounts1020_KeyedRateLimiter(t *testing.T) {
	if newKeyedRateLimiter(0, time.Hour) != nil {
		t.Fatal("perPeriod 0 is off (nil)")
	}
	var off *ipRateLimiter
	if ok, _ := off.allowKey("x"); !ok {
		t.Fatal("a nil limiter allows")
	}
	off.refundKey("x")
	now := time.Unix(1000, 0)
	l := newKeyedRateLimiter(2, time.Hour)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := l.allowKey("a"); !ok {
			t.Fatalf("charge %d", i)
		}
	}
	if ok, wait := l.allowKey("a"); ok || wait <= 0 || wait > 30*time.Minute {
		t.Fatalf("third: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allowKey("b"); !ok {
		t.Fatal("another key has its own budget")
	}
	l.refundKey("a")
	if ok, _ := l.allowKey("a"); !ok {
		t.Fatal("after a refund")
	}
}
