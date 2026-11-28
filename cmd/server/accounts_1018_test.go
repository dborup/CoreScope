package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/account"
	"github.com/meshcore-analyzer/dbschema"
)

// #1018 M1 — owner accounts, the read-only server side: the gate, issuing a
// sign-in (companions only, blacklisted refused like unknown, the hidden
// prefix allowed), the CSRF guard, the global and per-IP limits, redeeming a
// proven sign-in, the session (forged, expired, revoked, deleted), renewal,
// and the writes that carry the session's generation.

const accountAdminKey = "account-admin-key-strong-enough"

var (
	accKey      = []byte("0123456789abcdef0123456789abcdef")
	accOtherKey = []byte("fedcba9876543210fedcba9876543210")
	accSecret   = strings.Repeat("5", 32)
	accAID      = strings.Repeat("9a", 16)
)

type acc1018Env struct {
	t      *testing.T
	srv    *Server
	router *mux.Router
	svc    *accountService
	now    time.Time
}

func accountsOn(mod func(*account.Config)) *account.Config {
	on := true
	c := &account.Config{Enabled: &on}
	if mod != nil {
		mod(c)
	}
	return c
}

func newAcc1018Env(t *testing.T, apiKey string, ac *account.Config) *acc1018Env {
	t.Helper()
	srv, router := setupTestServerWithAPIKey(t, apiKey)
	srv.cfg.Accounts = ac
	// Test fixture only: the production server never writes these tables;
	// the ingestor creates them (internal/dbschema) and is their only writer.
	if err := dbschema.EnsureAccounts(srv.db.conn); err != nil {
		t.Fatal(err)
	}
	e := &acc1018Env{t: t, srv: srv, router: router, now: time.UnixMilli(1_800_000_000_000)}
	e.svc = newAccountService(srv.cfg, srv.db.conn, filepath.Join(t.TempDir(), "meshcore.db"))
	e.svc.now = func() time.Time { return e.now }
	e.svc.readKey = func() ([]byte, error) { return accKey, nil }
	srv.accounts = e.svc
	return e
}

func (e *acc1018Env) req(method, path, body string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	r.Host = "scope.example"
	r.Header.Set("Origin", "https://scope.example")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, r)
	return w
}

func withCookie(tok string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: account.CookieName, Value: tok}) }
}

func (e *acc1018Env) addNode(pk, name, role string, withPos bool) {
	e.t.Helper()
	var lat, lon interface{}
	if withPos {
		lat, lon = 55.6761, 12.5683
	}
	if _, err := e.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'), 1)`, pk, name, role, lat, lon); err != nil {
		e.t.Fatal(err)
	}
}

func (e *acc1018Env) queued() []account.Command {
	e.t.Helper()
	raw, err := e.svc.queue.PendingValues()
	if err != nil {
		e.t.Fatal(err)
	}
	var out []account.Command
	for _, rc := range raw {
		var c account.Command
		if err := json.Unmarshal(rc.Data, &c); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// proven writes, as the ingestor would (test fixture), an account for pk
// and a sign-in proven at provenAt with accSecret's hash.
func (e *acc1018Env) proven(id, pk string, provenAt time.Time) {
	e.t.Helper()
	c := e.srv.db.conn
	if _, err := c.Exec(`INSERT OR IGNORE INTO accounts (account_id, identity_pubkey, created_at, last_login_at) VALUES (?, ?, ?, ?)`,
		accAID, pk, provenAt.UnixMilli(), provenAt.UnixMilli()); err != nil {
		e.t.Fatal(err)
	}
	if _, err := c.Exec(`INSERT INTO account_proofs (request_id, kind, account_id, pubkey, code, redeem_hash, status, created_at, expires_at, proven_at, proof_hash)
		VALUES (?, 'login', ?, ?, '3HQ7ZB', ?, 'proven', ?, ?, ?, 'abcd')`, id, accAID, pk, account.HashRedeemSecret(accSecret),
		provenAt.UnixMilli(), provenAt.Add(15*time.Minute).UnixMilli(), provenAt.UnixMilli()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *acc1018Env) redeem(id, secret string) *httptest.ResponseRecorder {
	return e.req("POST", "/api/account/session", `{"requestId":"`+id+`","redeemSecret":"`+secret+`"}`)
}

func sessionCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == account.CookieName {
			return c
		}
	}
	return nil
}

// signedIn redeems a proven sign-in and returns the session token.
func (e *acc1018Env) signedIn(pk string) string {
	e.t.Helper()
	e.addNode(pk, "Anna T-Echo", "companion", false)
	e.proven("e000000000000001", pk, e.now)
	w := e.redeem("e000000000000001", accSecret)
	c := sessionCookie(w)
	if w.Code != http.StatusOK || c == nil || c.Value == "" {
		e.t.Fatalf("redeem: %d %s", w.Code, w.Body)
	}
	return c.Value
}

func TestAccounts1018_GateOffByDefault(t *testing.T) {
	pk := pk789("a")
	for name, tc := range map[string]struct {
		key string
		ac  *account.Config
	}{
		"no config":         {accountAdminKey, nil},
		"enabled, weak key": {"changeme", accountsOn(nil)},
		"not set":           {accountAdminKey, &account.Config{}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newAcc1018Env(t, tc.key, tc.ac)
			e.addNode(pk, "Anna", "companion", false)
			if w := e.req("POST", "/api/account/login", `{"pubkey":"`+pk+`"}`); w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", w.Code)
			}
			var cfg AccountConfigResponse
			json.Unmarshal(e.req("GET", "/api/account/config", "").Body.Bytes(), &cfg)
			if cfg.Enabled || cfg.Available {
				t.Fatalf("config %+v", cfg)
			}
		})
	}
}

func TestAccounts1018_IssueLogin(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	pk, pkPos := pk789("a"), pk789("b")
	e.addNode(pk, "Anna T-Echo", "companion", false)
	e.addNode(pkPos, "Bo", "companion", true)
	w := e.req("POST", "/api/account/login", `{"pubkey":"`+strings.ToUpper(pk)+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var got AccountLoginIssuedResponse
	json.Unmarshal(w.Body.Bytes(), &got)
	if !account.ValidRedeemSecret(got.RedeemSecret) || len(got.Code) != 6 || got.NameBudgetBytes != 31 ||
		got.SuggestedName != "Anna T-Echo "+got.Code || got.ExpiresAt != e.now.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("issued %+v", got)
	}
	cmds := e.queued()
	if len(cmds) != 1 || cmds[0].Op != account.OpLogin || cmds[0].PubKey != pk || cmds[0].Code != got.Code ||
		cmds[0].RedeemHash != account.HashRedeemSecret(got.RedeemSecret) || cmds[0].RequestID != got.RequestID {
		t.Fatalf("queued %+v", cmds)
	}
	raw, _ := json.Marshal(cmds[0])
	if strings.Contains(string(raw), got.RedeemSecret) {
		t.Fatal("the redeem secret must never reach the queue")
	}
	// A companion that shares its position has the 23-byte budget.
	w = e.req("POST", "/api/account/login", `{"pubkey":"`+pkPos+`"}`)
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.NameBudgetBytes != 23 {
		t.Fatalf("budget with a position %d", got.NameBudgetBytes)
	}
}

// Owner decisions 2 and 10: only a companion signs in; a blacklisted key is
// refused like an unknown node; a 🚫-prefix name may sign in.
func TestAccounts1018_WhoMaySignIn(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	repeater, blacklisted, hiddenName, unknown := pk789("1"), pk789("2"), pk789("3"), pk789("4")
	e.addNode(repeater, "Roof", "repeater", true)
	e.addNode(blacklisted, "Anna", "companion", false)
	e.addNode(hiddenName, "🚫 Anna", "companion", false)
	e.srv.cfg.SetNodeBlacklist([]string{blacklisted})
	e.srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	ref := e.req("POST", "/api/account/login", `{"pubkey":"`+unknown+`"}`)
	if ref.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", ref.Code)
	}
	for name, pk := range map[string]string{"repeater": repeater, "blacklisted": blacklisted} {
		w := e.req("POST", "/api/account/login", `{"pubkey":"`+pk+`"}`)
		if w.Code != ref.Code || w.Body.String() != ref.Body.String() {
			t.Errorf("%s: %d %s, want the unknown node's %d %s", name, w.Code, w.Body, ref.Code, ref.Body)
		}
	}
	if w := e.req("POST", "/api/account/login", `{"pubkey":"`+hiddenName+`"}`); w.Code != http.StatusAccepted {
		t.Fatalf("a hidden-prefix companion may sign in: %d %s", w.Code, w.Body)
	}
	if n := len(e.queued()); n != 1 {
		t.Fatalf("%d commands queued, want 1", n)
	}
}

func TestAccounts1018_CSRFGuard(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	pk := pk789("a")
	e.addNode(pk, "Anna", "companion", false)
	body := `{"pubkey":"` + pk + `"}`
	for name, mod := range map[string]func(*http.Request){
		"no origin":        func(r *http.Request) { r.Header.Del("Origin") },
		"other origin":     func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"form post":        func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		"cross-site fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		if w := e.req("POST", "/api/account/login", body, mod); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, w.Code)
		}
	}
	if w := e.req("POST", "/api/account/login", body, func(r *http.Request) {
		r.Header.Set("Origin", "https://proxy.example")
		r.Header.Set("X-Forwarded-Host", "proxy.example")
	}); w.Code != http.StatusAccepted {
		t.Errorf("behind a proxy: %d", w.Code)
	}
}

// The global cap holds even where per-IP limits cannot (no trustedProxies:
// httptest's 192.0.2.1 peer is not told apart).
func TestAccounts1018_GlobalLoginCapAndQueueFull(t *testing.T) {
	// The per-IP limit is set out of the way, so only the global window
	// can refuse; each request also comes from its own address.
	e := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.LoginsPerHour = 2; c.LoginsPerMinutePerIP = 1000 }))
	pk := pk789("a")
	e.addNode(pk, "Anna", "companion", false)
	body := `{"pubkey":"` + pk + `"}`
	from := func(i int) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = "203.0.113." + string(rune('1'+i)) + ":4000" }
	}
	for i := 0; i < 2; i++ {
		if w := e.req("POST", "/api/account/login", body, from(i)); w.Code != http.StatusAccepted {
			t.Fatalf("login %d: %d", i, w.Code)
		}
	}
	w := e.req("POST", "/api/account/login", body, from(2))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "too many sign-ins") {
		t.Fatalf("third, from a new address: %d %s", w.Code, w.Body)
	}

	// The per-IP limit, where clients can be told apart (a public peer).
	ip := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.LoginsPerMinutePerIP = 1 }))
	ip.addNode(pk, "Anna", "companion", false)
	if w := ip.req("POST", "/api/account/login", body, from(0)); w.Code != http.StatusAccepted {
		t.Fatalf("per-IP first: %d", w.Code)
	}
	if w := ip.req("POST", "/api/account/login", body, from(0)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("per-IP second: %d", w.Code)
	}
	if w := ip.req("POST", "/api/account/login", body, from(1)); w.Code != http.StatusAccepted {
		t.Fatalf("another address: %d", w.Code)
	}

	e2 := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.MaxQueuedRequests = 1; c.LoginsPerHour = 2 }))
	e2.addNode(pk, "Anna", "companion", false)
	e2.req("POST", "/api/account/login", body)
	if w := e2.req("POST", "/api/account/login", body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("queue full: %d", w.Code)
	}
	// The refused request gave its budget back.
	e2.svc.queue = channelregistry.NewQueue(filepath.Join(t.TempDir(), "q"))
	if w := e2.req("POST", "/api/account/login", body); w.Code != http.StatusAccepted {
		t.Fatalf("after a refund: %d", w.Code)
	}
}

func TestAccounts1018_NoKeyNoSignIn(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	e.svc.readKey = func() ([]byte, error) { return nil, account.ErrNoKey }
	pk := pk789("a")
	e.addNode(pk, "Anna", "companion", false)
	if w := e.req("POST", "/api/account/login", `{"pubkey":"`+pk+`"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no key: %d", w.Code)
	}
	var cfg AccountConfigResponse
	json.Unmarshal(e.req("GET", "/api/account/config", "").Body.Bytes(), &cfg)
	if !cfg.Enabled || cfg.Available {
		t.Fatalf("config %+v", cfg)
	}
}

func TestAccounts1018_RedeemAndCookie(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	pk := pk789("a")
	e.addNode(pk, "Anna T-Echo", "companion", false)
	e.proven("e000000000000001", pk, e.now)
	if w := e.redeem("e000000000000001", strings.Repeat("6", 32)); w.Code != http.StatusUnauthorized || sessionCookie(w) != nil {
		t.Fatalf("wrong secret: %d", w.Code)
	}
	w := e.redeem("e000000000000001", accSecret)
	c := sessionCookie(w)
	if w.Code != http.StatusOK || c == nil {
		t.Fatalf("redeem: %d %s", w.Code, w.Body)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" ||
		c.MaxAge != 30*24*3600 || !strings.HasPrefix(c.Name, "__Host-") {
		t.Fatalf("cookie flags %+v", c)
	}
	var got AccountSessionResponse
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.IdentityPrefix != pk[:8] || got.IdentityName != "Anna T-Echo" || strings.Contains(w.Body.String(), pk) {
		t.Fatalf("session body %s", w.Body)
	}
	// Past the 10-minute redeem window.
	e.now = e.now.Add(11 * time.Minute)
	if w := e.redeem("e000000000000001", accSecret); w.Code != http.StatusUnauthorized {
		t.Fatalf("late redeem: %d", w.Code)
	}
}

func TestAccounts1018_RedeemRefusesPendingAndBlacklisted(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	pk := pk789("a")
	e.addNode(pk, "Anna", "companion", false)
	e.srv.db.conn.Exec(`INSERT INTO account_proofs (request_id, kind, pubkey, code, redeem_hash, status, created_at, expires_at)
		VALUES ('e000000000000002', 'login', ?, '3HQ7ZB', ?, 'pending', ?, ?)`, pk, account.HashRedeemSecret(accSecret), e.now.UnixMilli(), e.now.Add(time.Hour).UnixMilli())
	if w := e.redeem("e000000000000002", accSecret); w.Code != http.StatusUnauthorized {
		t.Fatalf("pending: %d", w.Code)
	}
	e.proven("e000000000000001", pk, e.now)
	e.srv.cfg.SetNodeBlacklist([]string{pk})
	if w := e.redeem("e000000000000001", accSecret); w.Code != http.StatusUnauthorized {
		t.Fatalf("blacklisted after the proof: %d", w.Code)
	}
}

func TestAccounts1018_SessionChecks(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	tok := e.signedIn(pk)
	w := e.req("GET", "/api/account", "", withCookie(tok))
	if w.Code != http.StatusOK {
		t.Fatalf("account: %d %s", w.Code, w.Body)
	}
	var acc AccountResponse
	json.Unmarshal(w.Body.Bytes(), &acc)
	if acc.IdentityPrefix != pk[:8] || acc.IdentityName != "Anna T-Echo" || acc.Limits.MaxMyNodes != 200 || strings.Contains(w.Body.String(), pk) {
		t.Fatalf("account body %s", w.Body)
	}

	// Forged: a valid token for the same account under another key.
	claims, _ := account.NewClaims(accAID, 1, e.now.UnixMilli(), e.now.UnixMilli(), e.svc.limits)
	forged, _ := account.SignSession(accOtherKey, claims)
	for name, bad := range map[string]string{"forged": forged, "garbage": "abc.def", "truncated": tok[:len(tok)-2]} {
		w := e.req("GET", "/api/account", "", withCookie(bad))
		if c := sessionCookie(w); w.Code != http.StatusUnauthorized || c == nil || c.MaxAge >= 0 {
			t.Errorf("%s: %d, want 401 and the cookie cleared", name, w.Code)
		}
	}
	if w := e.req("GET", "/api/account", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", w.Code)
	}

	// Renewal: a two-day-old token is re-issued on use.
	e.now = e.now.Add(48 * time.Hour)
	w = e.req("GET", "/api/account", "", withCookie(tok))
	if c := sessionCookie(w); w.Code != http.StatusOK || c == nil || c.Value == tok || c.MaxAge != 30*24*3600 {
		t.Fatalf("renewal: %d %+v", w.Code, c)
	}

	// Expired: past its 30 days.
	e.now = e.now.Add(29 * 24 * time.Hour)
	if w := e.req("GET", "/api/account", "", withCookie(tok)); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired: %d", w.Code)
	}
}

// Revocation: once the ingestor applied sign-out-everywhere (session_gen +1)
// or the delete, every earlier token is refused (a mutant that skips the
// generation check keeps it valid).
func TestAccounts1018_RevokedByGenerationAndDelete(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	tok := e.signedIn(pk)
	w := e.req("POST", "/api/account/sign-out-all", "", withCookie(tok))
	if w.Code != http.StatusAccepted {
		t.Fatalf("sign-out-all: %d %s", w.Code, w.Body)
	}
	if c := sessionCookie(w); c == nil || c.MaxAge >= 0 {
		t.Fatal("sign-out-all clears this browser's cookie")
	}
	cmds := e.queued()
	if len(cmds) != 1 || cmds[0].Op != account.OpSignOutAll || cmds[0].AccountID != accAID || cmds[0].Gen != 1 {
		t.Fatalf("queued %+v", cmds)
	}
	// The ingestor applies it (test fixture).
	e.srv.db.conn.Exec(`UPDATE accounts SET session_gen = session_gen + 1 WHERE account_id = ?`, accAID)
	if w := e.req("GET", "/api/account", "", withCookie(tok)); w.Code != http.StatusUnauthorized {
		t.Fatalf("old generation: %d", w.Code)
	}

	e2 := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	tok2 := e2.signedIn(pk)
	if w := e2.req("DELETE", "/api/account", "", withCookie(tok2)); w.Code != http.StatusAccepted {
		t.Fatalf("delete: %d", w.Code)
	}
	e2.srv.db.conn.Exec(`DELETE FROM account_proofs`)
	e2.srv.db.conn.Exec(`DELETE FROM accounts`)
	if w := e2.req("GET", "/api/account", "", withCookie(tok2)); w.Code != http.StatusUnauthorized {
		t.Fatalf("deleted account: %d", w.Code)
	}
}

func TestAccounts1018_BlacklistedAfterSignIn(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	tok := e.signedIn(pk)
	e.srv.cfg.SetNodeBlacklist([]string{pk})
	if w := e.req("GET", "/api/account", "", withCookie(tok)); w.Code != http.StatusUnauthorized {
		t.Fatalf("blacklisted companion's session: %d", w.Code)
	}
}

func TestAccounts1018_MyNodesWrites(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(func(c *account.Config) { c.MaxMyNodes = 2; c.WritesPerHour = 3 }))
	tok := e.signedIn(pk)
	// Each write gets its own createdAt, so the queue keeps their order.
	e.svc.now = func() time.Time { e.now = e.now.Add(time.Millisecond); return e.now }
	n1, n2, n3 := pk789("b"), pk789("c"), pk789("d")
	if w := e.req("POST", "/api/account/my-nodes", `{"op":"set","nodes":[{"pubkey":"`+n1+`"},{"pubkey":"`+n2+`"},{"pubkey":"`+n3+`"}]}`, withCookie(tok)); w.Code != http.StatusBadRequest {
		t.Fatalf("set past the cap: %d", w.Code)
	}
	if w := e.req("POST", "/api/account/my-nodes", `{"op":"set","nodes":[{"pubkey":"`+strings.ToUpper(n1)+`","name":"Roof"}]}`, withCookie(tok)); w.Code != http.StatusAccepted {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if w := e.req("POST", "/api/account/my-nodes", `{"op":"remove","pubkey":"`+n1+`"}`, withCookie(tok)); w.Code != http.StatusAccepted {
		t.Fatalf("remove: %d", w.Code)
	}
	name := "Anna"
	if w := e.req("POST", "/api/account/profile", `{"displayName":"`+name+`"}`, withCookie(tok)); w.Code != http.StatusAccepted {
		t.Fatalf("profile: %d", w.Code)
	}
	if w := e.req("POST", "/api/account/my-nodes", `{"op":"add","nodes":[{"pubkey":"`+n2+`"}]}`, withCookie(tok)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("write cap: %d", w.Code)
	}
	cmds := e.queued()
	if len(cmds) != 3 {
		t.Fatalf("%d commands", len(cmds))
	}
	for _, c := range cmds {
		if c.AccountID != accAID || c.Gen != 1 {
			t.Fatalf("command without the session's account and generation: %+v", c)
		}
	}
	if cmds[0].Op != account.OpMyNodesSet || cmds[0].Nodes[0].PubKey != n1 || cmds[1].Op != account.OpMyNodeRemove || *cmds[2].DisplayName != name {
		t.Fatalf("ops %+v", cmds)
	}
	if w := e.req("POST", "/api/account/my-nodes", `{"op":"add","nodes":[{"pubkey":"`+n2+`"}]}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: %d", w.Code)
	}
	if w := e.req("POST", "/api/account/my-nodes", `{"op":"add","nodes":[{"pubkey":"`+n2+`"}]}`, withCookie(tok), func(r *http.Request) { r.Header.Del("Origin") }); w.Code != http.StatusForbidden {
		t.Fatalf("a write without Origin: %d", w.Code)
	}
}

func TestAccounts1018_MyNodesServedWithoutHidden(t *testing.T) {
	pk := pk789("a")
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	tok := e.signedIn(pk)
	shown, hidden, gone := pk789("b"), pk789("c"), pk789("d")
	e.addNode(shown, "Roof", "repeater", true)
	e.addNode(hidden, "Secret", "repeater", true)
	for i, k := range []string{shown, hidden, gone} {
		e.srv.db.conn.Exec(`INSERT INTO account_my_nodes (account_id, pubkey, name_hint, added_at) VALUES (?, ?, ?, ?)`, accAID, k, "hint", i+1)
	}
	e.srv.cfg.SetNodeBlacklist([]string{hidden})
	var acc AccountResponse
	json.Unmarshal(e.req("GET", "/api/account", "", withCookie(tok)).Body.Bytes(), &acc)
	if len(acc.MyNodes) != 2 || acc.MyNodes[0].PubKey != shown || acc.MyNodes[0].Name != "Roof" || acc.MyNodes[1].Name != "hint" {
		t.Fatalf("my nodes %+v", acc.MyNodes)
	}
}

func TestAccounts1018_LoginStatus(t *testing.T) {
	e := newAcc1018Env(t, accountAdminKey, accountsOn(nil))
	pk := pk789("a")
	e.addNode(pk, "Anna", "companion", false)
	var got AccountLoginIssuedResponse
	json.Unmarshal(e.req("POST", "/api/account/login", `{"pubkey":"`+pk+`"}`).Body.Bytes(), &got)
	var st AccountRequestStatusResponse
	json.Unmarshal(e.req("GET", "/api/account/login/"+got.RequestID, "").Body.Bytes(), &st)
	if st.Status != account.RequestQueued {
		t.Fatalf("status %+v", st)
	}
	e.proven("e000000000000001", pk, e.now)
	w := e.req("GET", "/api/account/login/e000000000000001", "")
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Status != account.StatusProven || strings.Contains(w.Body.String(), accAID) || strings.Contains(w.Body.String(), "3HQ7ZB") {
		t.Fatalf("proven status %s", w.Body)
	}
	if w := e.req("GET", "/api/account/login/0123456789abcdef", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", w.Code)
	}
}

// No account response type has a field for the full companion key or any
// contact: reflection over every type the account routes encode.
func TestAccounts1018_ResponsesCarryNoKeyOrContact(t *testing.T) {
	for _, v := range []interface{}{AccountConfigResponse{}, AccountLoginIssuedResponse{}, AccountRequestStatusResponse{},
		AccountSessionResponse{}, AccountResponse{}, AccountNode{}, AccountLimits{}, AccountAcceptedResponse{}} {
		tp := reflect.TypeOf(v)
		for i := 0; i < tp.NumField(); i++ {
			tag := strings.ToLower(tp.Field(i).Tag.Get("json"))
			if strings.Contains(tag, "contact") || strings.HasPrefix(tag, "identitypubkey") || strings.HasPrefix(tag, "accountid") {
				t.Errorf("%s.%s (%s) must not be served", tp.Name(), tp.Field(i).Name, tag)
			}
		}
	}
}

func BenchmarkAccounts1018_SessionAndAccountRead(b *testing.B) {
	t := &testing.T{}
	_ = t
	srv, router := setupTestServerWithAPIKey(&testing.T{}, accountAdminKey)
	srv.cfg.Accounts = accountsOn(nil)
	dbschema.EnsureAccounts(srv.db.conn)
	svc := newAccountService(srv.cfg, srv.db.conn, filepath.Join(b.TempDir(), "x.db"))
	svc.readKey = func() ([]byte, error) { return accKey, nil }
	srv.accounts = svc
	now := time.Now()
	pk := pk789("a")
	srv.db.conn.Exec(`INSERT INTO accounts (account_id, identity_pubkey, created_at, last_login_at) VALUES (?, ?, ?, ?)`, accAID, pk, now.UnixMilli(), now.UnixMilli())
	for i := 0; i < 200; i++ {
		k := strings.Repeat("0", 61) + string("0123456789abcdef"[i%16]) + string("0123456789abcdef"[i/16]) + "0"
		srv.db.conn.Exec(`INSERT INTO account_my_nodes (account_id, pubkey, added_at) VALUES (?, ?, ?)`, accAID, k, i)
	}
	claims, _ := account.NewClaims(accAID, 1, now.UnixMilli(), now.UnixMilli(), svc.limits)
	tok, _ := account.SignSession(accKey, claims)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest("GET", "/api/account", nil)
		r.AddCookie(&http.Cookie{Name: account.CookieName, Value: tok})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			b.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}
