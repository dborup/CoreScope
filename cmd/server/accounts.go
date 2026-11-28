package main

// Owner accounts (#1018 M1) — the read-only server side.
//
// Sign-in (option A of the #1018 research, owner decision 1): the browser
// names its companion; the server issues a random single-use code bound to
// that key and a redeem secret only that browser holds; the owner puts the
// code in the companion's name and sends a flood advert; the ingestor proves
// it at advert ingest (cmd/ingestor/accounts.go) and creates the account;
// the browser redeems the proven sign-in for a session cookie.
//
// This file only:
//   - issues sign-in codes and queues every account write as a command file
//     in the account queue next to the database (#1283: no SQLite writes);
//   - mints and checks the stateless HMAC session token
//     (internal/channelregistry/account), with one primary-key read of the
//     account row per authenticated request for revocation (session_gen);
//   - serves the sign-in status, the session and the account from read-only
//     queries.
//
// Identities: only a known companion may sign in (owner decision 2). A
// blacklisted key is refused exactly like an unknown node; a node hidden only
// by a hidden-name prefix may sign in, because nothing here makes its key
// public (owner decision 10). The companion key is never in a public
// response: the account sees only its first 8 hex characters.
//
// Bounds: sign-in opens only with accounts.enabled, a strong apiKey, a
// database and the session key file; a global sliding window of
// loginsPerHour sign-ins (per-IP limits are skipped without trustedProxies,
// so the global cap is what holds) plus loginsPerMinutePerIP; a global window
// of writesPerHour account writes; MaxQueuedRequests command files; the
// ingestor enforces the pending caps and maxMyNodes.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/account"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

const (
	// maxAccountBodyBytes caps a small POST body; maxMyNodesBodyBytes the
	// bulk list of the migration (HardMaxMyNodes entries of ~200 bytes).
	maxAccountBodyBytes = 1024
	maxMyNodesBodyBytes = 256 << 10
	// accountKeyRecheck is how often a missing session key is looked for
	// again: the ingestor creates it at its start.
	accountKeyRecheck = 10 * time.Second
	// identityPrefixLen is how much of the companion key the account sees.
	identityPrefixLen = 8
)

// AccountConfigResponse is GET /api/account/config. Enabled: the operator
// opened sign-in (with a strong apiKey and a database); Available: the
// session key exists too, so a sign-in can complete now.
type AccountConfigResponse struct {
	Enabled          bool `json:"enabled"`
	Available        bool `json:"available"`
	SessionDays      int  `json:"sessionDays"`
	LoginCodeMinutes int  `json:"loginCodeMinutes"`
	MaxMyNodes       int  `json:"maxMyNodes"`
}

// AccountLoginRequest is the body of POST /api/account/login.
type AccountLoginRequest struct {
	PubKey string `json:"pubkey"`
}

// AccountLoginIssuedResponse is the 202 body of a queued sign-in. The code
// and the redeem secret are shown only here, to the requester.
type AccountLoginIssuedResponse struct {
	RequestID    string `json:"requestId"`
	Code         string `json:"code"`
	RedeemSecret string `json:"redeemSecret"`
	ExpiresAt    int64  `json:"expiresAt"`
	// NameBudgetBytes is the companion's name on air: 31 bytes without a
	// position, 23 with one or when unknown.
	NameBudgetBytes int    `json:"nameBudgetBytes"`
	SuggestedName   string `json:"suggestedName"`
}

// AccountRequestStatusResponse is GET /api/account/login/{id} and
// GET /api/account/requests/{id}.
type AccountRequestStatusResponse struct {
	Status    string `json:"status"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AccountSessionRequest is the body of POST /api/account/session.
type AccountSessionRequest struct {
	RequestID    string `json:"requestId"`
	RedeemSecret string `json:"redeemSecret"`
}

// AccountSessionResponse answers a redeemed sign-in: who signed in, for the
// "This is me / Not me" step.
type AccountSessionResponse struct {
	IdentityPrefix string `json:"identityPrefix"`
	IdentityName   string `json:"identityName,omitempty"`
	ExpiresAt      int64  `json:"expiresAt"`
}

// AccountNode is one entry of the synced My nodes list.
type AccountNode struct {
	PubKey  string `json:"pubkey"`
	Name    string `json:"name"`
	Role    string `json:"role,omitempty"`
	AddedAt int64  `json:"addedAt"`
}

// AccountLimits are the limits the page shows and enforces early.
type AccountLimits struct {
	MaxMyNodes int `json:"maxMyNodes"`
}

// AccountResponse is GET /api/account, for the account itself only. It has
// no field for the full companion key nor for any admin contact.
type AccountResponse struct {
	DisplayName    string        `json:"displayName,omitempty"`
	IdentityPrefix string        `json:"identityPrefix"`
	IdentityName   string        `json:"identityName,omitempty"`
	SignedInAt     int64         `json:"signedInAt"`
	MyNodes        []AccountNode `json:"myNodes"`
	Limits         AccountLimits `json:"limits"`
}

// AccountMyNodesRequest is POST /api/account/my-nodes: op "set" replaces the
// list with nodes (the migration), "add" adds nodes[0], "remove" removes
// pubkey.
type AccountMyNodesRequest struct {
	Op     string           `json:"op"`
	Nodes  []account.MyNode `json:"nodes,omitempty"`
	PubKey string           `json:"pubkey,omitempty"`
}

// AccountProfileRequest is POST /api/account/profile; "" clears the name.
type AccountProfileRequest struct {
	DisplayName string `json:"displayName"`
}

// AccountAcceptedResponse is the 202 body of a queued account write.
type AccountAcceptedResponse struct {
	RequestID string `json:"requestId"`
}

type accountService struct {
	enabled bool
	limits  account.Limits
	queue   *channelregistry.Queue // nil without a database path
	db      *sql.DB                // read-only; nil without a database
	now     func() time.Time
	readKey func() ([]byte, error)

	logins    submissionWindow // global sign-in cap
	writes    submissionWindow // global account-write cap
	ipLimiter *ipRateLimiter   // per-IP sign-ins; a no-op without trustedProxies

	keyMu        sync.Mutex
	key          []byte
	keyCheckedAt time.Time
}

func newAccountService(cfg *Config, db *sql.DB, dbPath string) *accountService {
	var ac *account.Config
	strongKey := false
	var trusted []string
	if cfg != nil {
		ac = cfg.Accounts
		strongKey = cfg.APIKey != "" && !IsWeakAPIKey(cfg.APIKey)
		trusted = cfg.WSTrustedProxies()
	}
	svc := &accountService{limits: ac.Limits(), db: db, now: time.Now}
	if dbPath != "" {
		svc.queue = channelregistry.NewQueue(account.QueueDir(dbPath))
		svc.readKey = func() ([]byte, error) { return account.ReadKey(dbPath) }
	}
	svc.enabled = ac.Requested() && strongKey && svc.queue != nil && db != nil
	if ac.Requested() && !svc.enabled {
		log.Printf("[accounts] WARNING: accounts.enabled is set but sign-in stays closed: " +
			"it needs a strong apiKey (16+ characters, not a placeholder) and a database")
	}
	if svc.enabled {
		svc.ipLimiter = newIPRateLimiter("accounts", "accounts.loginsPerMinutePerIP", svc.limits.LoginsPerMinutePerIP,
			parseCIDRListQuiet(trusted), log.Printf)
	}
	return svc
}

// accountsSvc returns the service, building it on first use.
func (s *Server) accountsSvc() *accountService {
	s.accountsOnce.Do(func() {
		if s.accounts != nil {
			return
		}
		var conn *sql.DB
		path := ""
		if s.db != nil {
			conn, path = s.db.conn, s.db.path
		}
		s.accounts = newAccountService(s.cfg, conn, path)
	})
	return s.accounts
}

// sessionKey returns the signing key, or nil while the ingestor has not
// created it. A missing key is looked for again every accountKeyRecheck.
func (a *accountService) sessionKey() []byte {
	a.keyMu.Lock()
	defer a.keyMu.Unlock()
	if a.key != nil || a.readKey == nil {
		return a.key
	}
	now := a.now()
	if !a.keyCheckedAt.IsZero() && now.Sub(a.keyCheckedAt) < accountKeyRecheck {
		return nil
	}
	a.keyCheckedAt = now
	k, err := a.readKey()
	if err != nil {
		if !errors.Is(err, account.ErrNoKey) {
			log.Printf("[accounts] the session key cannot be used: %v", err) // never the key itself
		}
		return nil
	}
	a.key = k
	return k
}

func isMissingAccountTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, t := range []string{"accounts", "account_proofs", "account_my_nodes"} {
		if strings.Contains(msg, "no such table: "+t) {
			return true
		}
	}
	return false
}

// ── CSRF and cookies ──────────────────────────────────────────────────────

// sameOriginWrite is the CSRF defence in depth under the SameSite=Strict
// cookie: a state-changing request must name this site in Origin and, when
// it has a body, send JSON. A form post from another site has neither.
func sameOriginWrite(r *http.Request, withBody bool) bool {
	if withBody && !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	return fwd != "" && strings.EqualFold(u.Host, strings.TrimSpace(fwd))
}

func setSessionCookie(w http.ResponseWriter, tok string, expMs, nowMs int64) {
	http.SetCookie(w, &http.Cookie{
		Name: account.CookieName, Value: tok, Path: "/",
		MaxAge: int((expMs - nowMs) / 1000), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: account.CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// ── Session ───────────────────────────────────────────────────────────────

// accountSession is a checked session.
type accountSession struct {
	claims         account.SessionClaims
	identityPubKey string
	displayName    string
}

var errNoSession = errors.New("not signed in")

// session checks the request's cookie: the signature and lifetime, then the
// account row (one primary-key read): it must exist, carry the token's
// generation and not belong to a blacklisted companion. A token older than a
// day is re-issued (sliding, never past the hard cap).
func (s *Server) session(w http.ResponseWriter, r *http.Request) (accountSession, error) {
	a := s.accountsSvc()
	var sess accountSession
	c, err := r.Cookie(account.CookieName)
	if err != nil || c.Value == "" || a.db == nil {
		return sess, errNoSession
	}
	key := a.sessionKey()
	if key == nil {
		return sess, errNoSession
	}
	nowMs := a.now().UnixMilli()
	claims, err := account.VerifySession(key, c.Value, nowMs, a.limits.SessionMax.Milliseconds())
	if err != nil {
		return sess, errNoSession
	}
	var gen int64
	var dn sql.NullString
	err = a.db.QueryRowContext(r.Context(), `SELECT session_gen, identity_pubkey, display_name FROM accounts WHERE account_id = ?`,
		claims.AccountID).Scan(&gen, &sess.identityPubKey, &dn)
	if err == sql.ErrNoRows || isMissingAccountTable(err) {
		return sess, errNoSession
	}
	if err != nil {
		return sess, err
	}
	if gen != claims.Gen || identityHidden(s.cfg, sess.identityPubKey) {
		return sess, errNoSession
	}
	sess.claims, sess.displayName = claims, dn.String
	if claims.NeedsRenewal(nowMs, a.limits) {
		if next, err := account.NewClaims(claims.AccountID, claims.Gen, claims.AuthAt, nowMs, a.limits); err == nil {
			if tok, err := account.SignSession(key, next); err == nil {
				setSessionCookie(w, tok, next.ExpiresAt, nowMs)
				sess.claims = next
			}
		}
	}
	return sess, nil
}

// requireSession answers 401 (and clears a stale cookie) unless signed in.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (accountSession, bool) {
	sess, err := s.session(w, r)
	if err == nil {
		return sess, true
	}
	if errors.Is(err, errNoSession) {
		if _, cerr := r.Cookie(account.CookieName); cerr == nil {
			clearSessionCookie(w)
		}
		writeError(w, http.StatusUnauthorized, "not signed in")
		return sess, false
	}
	log.Printf("[accounts] session check failed: %v", err)
	writeError(w, http.StatusInternalServerError, "could not check the session")
	return sess, false
}

// ── Routes ────────────────────────────────────────────────────────────────

// GET /api/account/config
func (s *Server) handleAccountConfig(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	a := s.accountsSvc()
	writeJSON(w, AccountConfigResponse{
		Enabled:          a.enabled,
		Available:        a.enabled && a.sessionKey() != nil,
		SessionDays:      int(a.limits.SessionTTL / (24 * time.Hour)),
		LoginCodeMinutes: int(a.limits.LoginTTL / time.Minute),
		MaxMyNodes:       a.limits.MaxMyNodes,
	})
}

// companionForLogin reads the node a sign-in names. ok is false for an
// unknown node, a node that is not a companion and a blacklisted key: one
// identical 404 for all three.
func (s *Server) companionForLogin(ctx context.Context, pk string) (name string, budget int, ok bool, err error) {
	if identityHidden(s.cfg, pk) {
		return "", 0, false, nil
	}
	var role sql.NullString
	var lat, lon sql.NullFloat64
	var nm sql.NullString
	err = s.accountsSvc().db.QueryRowContext(ctx, `SELECT name, role, lat, lon FROM nodes WHERE public_key = ?`, pk).Scan(&nm, &role, &lat, &lon)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	if strings.ToLower(role.String) != "companion" {
		return "", 0, false, nil
	}
	budget = account.UnknownBudget
	if !lat.Valid && !lon.Valid {
		budget = antennaclaim.NameBudget(false, false, false)
	}
	return nm.String, budget, true, nil
}

// POST /api/account/login
func (s *Server) handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	a := s.accountsSvc()
	noStore(w)
	if !a.enabled {
		writeError(w, http.StatusForbidden, "sign-in is disabled")
		return
	}
	if !sameOriginWrite(r, true) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	var body AccountLoginRequest
	if err := decodeSingleJSONObject(http.MaxBytesReader(w, r.Body, maxAccountBodyBytes), &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	pk := strings.ToLower(strings.TrimSpace(body.PubKey))
	if !antennaclaim.ValidPubKey(pk) {
		writeError(w, http.StatusBadRequest, "invalid public key")
		return
	}
	name, budget, ok, err := s.companionForLogin(r.Context(), pk)
	if err != nil {
		log.Printf("[accounts] companion lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read the node")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	if a.sessionKey() == nil {
		writeError(w, http.StatusServiceUnavailable, "sign-in is not available yet, please try again later")
		return
	}
	if ok, wait := a.ipLimiter.allow(r); !ok {
		writeRateLimited(w, wait)
		return
	}
	if ok, wait := a.logins.allow(a.now(), a.limits.LoginsPerHour); !ok {
		a.ipLimiter.refund(r)
		setRetryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many sign-ins right now, please try again later")
		return
	}
	refund := func() { a.logins.refund(); a.ipLimiter.refund(r) }
	code, err := antennaclaim.NewCode()
	if err != nil {
		refund()
		writeError(w, http.StatusInternalServerError, "could not issue a code")
		return
	}
	secret, hash, err := account.NewRedeemSecret()
	if err != nil {
		refund()
		writeError(w, http.StatusInternalServerError, "could not issue a code")
		return
	}
	now := a.now()
	cmd := account.Command{RequestID: channelregistry.NewID(), Op: account.OpLogin, PubKey: pk, Code: code, RedeemHash: hash, CreatedAt: now.UnixMilli()}
	if !a.enqueue(w, cmd) {
		refund()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(AccountLoginIssuedResponse{
		RequestID: cmd.RequestID, Code: code, RedeemSecret: secret,
		ExpiresAt:       now.Add(a.limits.LoginTTL).UnixMilli(),
		NameBudgetBytes: budget,
		SuggestedName:   antennaclaim.SuggestedName(name, code, budget),
	})
}

// enqueue writes cmd, answering 503 when the queue is full and 500 on other
// errors; it writes nothing on success.
func (a *accountService) enqueue(w http.ResponseWriter, cmd account.Command) bool {
	if a.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "accounts are unavailable")
		return false
	}
	if err := cmd.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if err := a.queue.EnqueueValue(cmd.RequestID, cmd, a.limits.MaxQueuedRequests); err != nil {
		if errors.Is(err, channelregistry.ErrQueueFull) {
			setRetryAfter(w, time.Minute)
			writeError(w, http.StatusServiceUnavailable, "the account queue is full, please try again later")
			return false
		}
		log.Printf("[accounts] enqueue %s failed: %v", cmd.Op, err)
		writeError(w, http.StatusInternalServerError, "could not queue the request")
		return false
	}
	return true
}

// GET /api/account/login/{requestId}: the unguessable request id is the
// capability; the answer is the status only, never the code or the account.
func (s *Server) handleAccountLoginStatus(w http.ResponseWriter, r *http.Request) {
	a := s.accountsSvc()
	noStore(w)
	id := mux.Vars(r)["requestId"]
	if !channelregistry.ValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	if a.db != nil {
		var status string
		var exp int64
		err := a.db.QueryRowContext(r.Context(), `SELECT status, expires_at FROM account_proofs WHERE request_id = ? AND kind = 'login'`, id).Scan(&status, &exp)
		switch {
		case err == nil:
			if status == account.StatusPending && a.now().UnixMilli() > exp {
				status = account.StatusExpired // the sweep has not run yet
			}
			writeJSON(w, AccountRequestStatusResponse{Status: status, ExpiresAt: exp})
			return
		case err != sql.ErrNoRows && !isMissingAccountTable(err):
			log.Printf("[accounts] status %s failed: %v", id, err)
			writeError(w, http.StatusInternalServerError, "could not read the request status")
			return
		}
	}
	s.writeQueuedAccountStatus(w, id)
}

// writeQueuedAccountStatus answers from the queue alone: queued, refused, or
// unknown (404).
func (s *Server) writeQueuedAccountStatus(w http.ResponseWriter, id string) {
	a := s.accountsSvc()
	if a.queue == nil {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	var res account.Result
	queued, err := a.queue.LookupValue(id, &res)
	switch {
	case errors.Is(err, channelregistry.ErrUnknownRequest):
		writeError(w, http.StatusNotFound, "Not found")
	case err != nil:
		log.Printf("[accounts] lookup %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "could not read the request status")
	case queued:
		writeJSON(w, AccountRequestStatusResponse{Status: account.RequestQueued})
	default:
		writeJSON(w, AccountRequestStatusResponse{Status: res.Status, Error: res.Error})
	}
}

// GET /api/account/requests/{requestId}: an account write's status.
func (s *Server) handleAccountRequestStatus(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	id := mux.Vars(r)["requestId"]
	if !channelregistry.ValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	s.writeQueuedAccountStatus(w, id)
}

// POST /api/account/session: redeem a proven sign-in. Read-only: the
// sign-in must be proven, redeemed within RedeemWindow of its proof, with the
// secret whose hash was queued, and its account must exist. Redeeming twice
// inside the window is harmless: only the secret's holder can.
func (s *Server) handleAccountSession(w http.ResponseWriter, r *http.Request) {
	a := s.accountsSvc()
	noStore(w)
	if !a.enabled {
		writeError(w, http.StatusForbidden, "sign-in is disabled")
		return
	}
	if !sameOriginWrite(r, true) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	var body AccountSessionRequest
	if err := decodeSingleJSONObject(http.MaxBytesReader(w, r.Body, maxAccountBodyBytes), &body); err != nil ||
		!channelregistry.ValidID(body.RequestID) || !account.ValidRedeemSecret(body.RedeemSecret) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	key := a.sessionKey()
	if key == nil {
		writeError(w, http.StatusServiceUnavailable, "sign-in is not available yet, please try again later")
		return
	}
	var (
		status, hash, aid, pk string
		provenAt, gen         sql.NullInt64
		identityName          sql.NullString
	)
	err := a.db.QueryRowContext(r.Context(), `SELECT p.status, p.redeem_hash, p.proven_at, COALESCE(a.account_id, ''),
		COALESCE(a.session_gen, 0), COALESCE(a.identity_pubkey, ''), n.name
		FROM account_proofs p LEFT JOIN accounts a ON a.account_id = p.account_id
		LEFT JOIN nodes n ON n.public_key = a.identity_pubkey
		WHERE p.request_id = ? AND p.kind = 'login'`, body.RequestID).Scan(&status, &hash, &provenAt, &aid, &gen, &pk, &identityName)
	refused := func() { writeError(w, http.StatusUnauthorized, "this sign-in cannot be used") }
	if err == sql.ErrNoRows || isMissingAccountTable(err) {
		refused()
		return
	}
	if err != nil {
		log.Printf("[accounts] redeem %s failed: %v", body.RequestID, err)
		writeError(w, http.StatusInternalServerError, "could not read the sign-in")
		return
	}
	nowMs := a.now().UnixMilli()
	if subtle.ConstantTimeCompare([]byte(account.HashRedeemSecret(body.RedeemSecret)), []byte(hash)) != 1 {
		refused()
		return
	}
	if status != account.StatusProven || !provenAt.Valid || aid == "" || nowMs-provenAt.Int64 > a.limits.RedeemWindow.Milliseconds() ||
		identityHidden(s.cfg, pk) {
		refused()
		return
	}
	claims, err := account.NewClaims(aid, gen.Int64, provenAt.Int64, nowMs, a.limits)
	if err != nil {
		refused()
		return
	}
	tok, err := account.SignSession(key, claims)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start the session")
		return
	}
	setSessionCookie(w, tok, claims.ExpiresAt, nowMs)
	writeJSON(w, AccountSessionResponse{IdentityPrefix: pk[:identityPrefixLen], IdentityName: identityName.String, ExpiresAt: claims.ExpiresAt})
}

// DELETE /api/account/session: sign out in this browser. A copy of the token
// stays valid until it expires unless the owner signs out everywhere.
func (s *Server) handleAccountSignOut(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !sameOriginWrite(r, false) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/account
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	sess, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	resp, err := s.readAccount(r.Context(), sess)
	if err != nil {
		log.Printf("[accounts] read failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read the account")
		return
	}
	writeJSON(w, resp)
}

// readAccount is two indexed reads: the companion's name, and the account's
// list (at most maxMyNodes rows, by primary key) joined to the nodes it
// names. Hidden nodes (#743) are left out, never written; one batched
// name lookup for the whole list.
func (s *Server) readAccount(ctx context.Context, sess accountSession) (AccountResponse, error) {
	a := s.accountsSvc()
	resp := AccountResponse{
		DisplayName:    sess.displayName,
		IdentityPrefix: sess.identityPubKey[:identityPrefixLen],
		SignedInAt:     sess.claims.AuthAt,
		MyNodes:        []AccountNode{},
		Limits:         AccountLimits{MaxMyNodes: a.limits.MaxMyNodes},
	}
	var nm sql.NullString
	if err := a.db.QueryRowContext(ctx, `SELECT name FROM nodes WHERE public_key = ?`, sess.identityPubKey).Scan(&nm); err != nil && err != sql.ErrNoRows {
		return resp, err
	}
	resp.IdentityName = nm.String
	rows, err := a.db.QueryContext(ctx, `SELECT m.pubkey, COALESCE(n.name, m.name_hint, ''), COALESCE(n.role, ''), m.added_at
		FROM account_my_nodes m LEFT JOIN nodes n ON n.public_key = m.pubkey
		WHERE m.account_id = ? ORDER BY m.added_at, m.pubkey LIMIT ?`, sess.claims.AccountID, a.limits.MaxMyNodes)
	if err != nil {
		return resp, err
	}
	defer rows.Close()
	var list []AccountNode
	keys := []string{}
	for rows.Next() {
		var n AccountNode
		if err := rows.Scan(&n.PubKey, &n.Name, &n.Role, &n.AddedAt); err != nil {
			return resp, err
		}
		list = append(list, n)
		keys = append(keys, n.PubKey)
	}
	if err := rows.Err(); err != nil {
		return resp, err
	}
	names, err := s.hiddenIdentityNames(ctx, keys)
	if err != nil {
		return resp, err
	}
	for _, n := range list {
		if !identityHidden(s.cfg, n.PubKey, append(names[n.PubKey], n.Name)...) {
			resp.MyNodes = append(resp.MyNodes, n)
		}
	}
	return resp, nil
}

// accountWrite is the shared front of every account write: the gate, the
// CSRF check, the session, the global write window and the queue. build
// fills the op-specific fields; a false return means it answered already.
// endSession clears this browser's cookie once the write is queued.
func (s *Server) accountWrite(w http.ResponseWriter, r *http.Request, withBody, endSession bool, build func(*account.Command) bool) (accountSession, bool) {
	a := s.accountsSvc()
	noStore(w)
	if !sameOriginWrite(r, withBody) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return accountSession{}, false
	}
	sess, ok := s.requireSession(w, r)
	if !ok {
		return sess, false
	}
	cmd := account.Command{RequestID: channelregistry.NewID(), AccountID: sess.claims.AccountID, Gen: sess.claims.Gen, CreatedAt: a.now().UnixMilli()}
	if !build(&cmd) {
		return sess, false
	}
	if ok, wait := a.writes.allow(a.now(), a.limits.WritesPerHour); !ok {
		setRetryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many account changes right now, please try again later")
		return sess, false
	}
	if !a.enqueue(w, cmd) {
		a.writes.refund()
		return sess, false
	}
	if endSession {
		clearSessionCookie(w)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(AccountAcceptedResponse{RequestID: cmd.RequestID})
	return sess, true
}

// POST /api/account/my-nodes
func (s *Server) handleAccountMyNodes(w http.ResponseWriter, r *http.Request) {
	a := s.accountsSvc()
	s.accountWrite(w, r, true, false, func(cmd *account.Command) bool {
		var body AccountMyNodesRequest
		if err := decodeSingleJSONObject(http.MaxBytesReader(w, r.Body, maxMyNodesBodyBytes), &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return false
		}
		for i := range body.Nodes {
			body.Nodes[i].PubKey = strings.ToLower(body.Nodes[i].PubKey)
		}
		switch body.Op {
		case "set":
			cmd.Op, cmd.Nodes = account.OpMyNodesSet, body.Nodes
			if len(body.Nodes) > a.limits.MaxMyNodes {
				writeError(w, http.StatusBadRequest, "too many nodes")
				return false
			}
		case "add":
			cmd.Op, cmd.Nodes = account.OpMyNodeAdd, body.Nodes
		case "remove":
			cmd.Op, cmd.PubKey = account.OpMyNodeRemove, strings.ToLower(body.PubKey)
		default:
			writeError(w, http.StatusBadRequest, "op must be set, add or remove")
			return false
		}
		if cmd.Validate() != nil {
			writeError(w, http.StatusBadRequest, "invalid node list")
			return false
		}
		return true
	})
}

// POST /api/account/profile
func (s *Server) handleAccountProfile(w http.ResponseWriter, r *http.Request) {
	s.accountWrite(w, r, true, false, func(cmd *account.Command) bool {
		var body AccountProfileRequest
		if err := decodeSingleJSONObject(http.MaxBytesReader(w, r.Body, maxAccountBodyBytes), &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return false
		}
		name := strings.TrimSpace(body.DisplayName)
		if name != "" && !account.ValidDisplayName(name) {
			writeError(w, http.StatusBadRequest, "the display name must be 1-32 printable characters")
			return false
		}
		cmd.Op, cmd.DisplayName = account.OpProfileSet, &name
		return true
	})
}

// POST /api/account/sign-out-all: every session of the account ends once
// the ingestor applies it (about 2 s); this browser's cookie goes at once.
func (s *Server) handleAccountSignOutAll(w http.ResponseWriter, r *http.Request) {
	s.accountWriteEndingSession(w, r, account.OpSignOutAll)
}

// DELETE /api/account: the account and its list are deleted; station data
// is untouched (M1 has no account-owned station data).
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	s.accountWriteEndingSession(w, r, account.OpDelete)
}

func (s *Server) accountWriteEndingSession(w http.ResponseWriter, r *http.Request, op string) {
	s.accountWrite(w, r, false, true, func(cmd *account.Command) bool {
		cmd.Op = op
		return true
	})
}
