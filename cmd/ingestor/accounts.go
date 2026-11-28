package main

// Owner accounts (#1018 M1) — the writer side.
//
// The read-only server issues a sign-in code bound to one companion public
// key as a command file in the account queue, and queues every account write
// the same way. This file stores them, proves sign-ins at advert ingest,
// creates the session key file and applies the writes.
//
// Sign-in proof: an advert that is new (not a re-received copy: same content
// hash), whose ed25519 signature verified, and whose type is ADV_TYPE_CHAT (a
// companion, firmware src/helpers/AdvertDataHelpers.h:8; BaseChatMesh builds
// its self advert with that type, src/helpers/BaseChatMesh.cpp:21-41) is
// looked up by its full public key in a bounded in-memory map; a pending code
// found as a whole token in the on-air part of its name proves the sign-in
// and is used up. A repeater, room or sensor advert never signs in (owner
// decision 2), whatever its name carries. The on-air part is
// antennaclaim.NameBudget of the advert's own flags: 31 bytes for a
// companion without a position, 23 with one (firmware
// src/helpers/AdvertDataHelpers.cpp:4-29, MAX_ADVERT_DATA_SIZE in
// src/MeshCore.h:12).
//
// Why nobody else can prove it, and why seeing the code on air is useless:
// the signature covers the name (src/Mesh.cpp:404-437); the code is drawn from
// crypto/rand after it was issued; a replayed advert is never new; and the
// session is only handed to the browser that holds the redeem secret, whose
// hash alone is stored here.
//
// Writes carry the session generation of the token that asked for them; a
// command whose generation is no longer the account's is refused, so nothing
// queued before "sign out everywhere" or a delete applies after it.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/account"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// User-facing outcomes, returned verbatim in the request status.
var (
	errTooManyLogins     = errors.New("too many sign-ins are waiting, please try again later")
	errTooManyKeyLogins  = errors.New("this companion already has the maximum number of sign-ins waiting")
	errAccountNotFound   = errors.New("account not found")
	errSessionEnded      = errors.New("this session has ended, please sign in again")
	errTooManyMyNodes    = errors.New("the list has reached its maximum number of nodes")
	errAccountReqExpired = errors.New("the request expired before it could be processed")
	errAccountStorage    = errors.New("the request could not be saved")
)

type pendingLogin struct {
	id        string
	code      string
	expiresAt int64 // Unix ms
}

type accountRunner struct {
	store  *Store
	queue  *channelregistry.Queue
	limits account.Limits
	now    func() time.Time

	mu sync.Mutex
	// pending maps a companion key to its pending sign-ins: at most
	// MaxPendingPerKey each and MaxPendingLogins in all, both enforced at
	// insert. Rebuilt from the table after every change.
	pending map[string][]pendingLogin
	loaded  bool
}

func newAccountRunner(store *Store, cfg *account.Config) *accountRunner {
	return &accountRunner{
		store:   store,
		queue:   channelregistry.NewQueue(account.QueueDir(store.path)),
		limits:  cfg.Limits(),
		now:     time.Now,
		pending: map[string][]pendingLogin{},
	}
}

// accountLoadOrCreateKey makes sure the session key file exists next to the
// database: 32 bytes from crypto/rand, mode 0600, written atomically. An
// existing file is kept, and tightened to 0600 if it was looser. The key is
// never logged.
func accountLoadOrCreateKey(dbPath string) error {
	p := account.KeyPath(dbPath)
	info, err := os.Stat(p)
	if err == nil {
		if info.Size() < account.KeyLen {
			return account.ErrKeyTooShort
		}
		if info.Mode().Perm() != 0o600 {
			return os.Chmod(p, 0o600)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	key := make([]byte, account.KeyLen)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	return writeFileAtomic0600(p, key)
}

// observeAdvert is the ingest hook, called for every advert after it was
// stored. Cost on the hot path: a nil check, the advert's type and, only
// for a companion advert while sign-ins wait, one O(1) map lookup and a scan
// of at most MaxPendingPerKey codes over at most 32 bytes.
func (r *accountRunner) observeAdvert(p *Payload, hash string, isNew bool) {
	if r == nil || !isNew || p == nil || p.SignatureValid == nil || !*p.SignatureValid || p.Flags == nil || !p.Flags.Chat {
		return
	}
	pubkey := strings.ToLower(p.PubKey)
	budget := antennaclaim.NameBudget(p.Flags.HasLocation, p.Flags.HasFeat1, p.Flags.HasFeat2)
	pl, ok := r.match(pubkey, p.Name, budget)
	if !ok {
		return
	}
	proved, err := r.store.proveLogin(context.Background(), pl.id, pubkey, hash, r.now().UnixMilli())
	if err != nil {
		r.restore(pubkey, pl)
		log.Printf("[accounts] proving sign-in %s failed: %v", pl.id, err)
		return
	}
	if proved {
		log.Printf("[accounts] sign-in %s proven", pl.id)
	}
}

// match finds and removes the pending sign-in of pubkey whose code is a
// whole token within the first budget bytes of name.
func (r *accountRunner) match(pubkey, name string, budget int) (pendingLogin, bool) {
	nowMs := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.pending[pubkey]
	for i, pl := range list {
		if pl.expiresAt < nowMs {
			continue
		}
		if antennaclaim.ContainsCode(name, budget, pl.code) {
			rest := append(list[:i:i], list[i+1:]...)
			if len(rest) == 0 {
				delete(r.pending, pubkey)
			} else {
				r.pending[pubkey] = rest
			}
			return pl, true
		}
	}
	return pendingLogin{}, false
}

func (r *accountRunner) restore(pubkey string, pl pendingLogin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, have := range r.pending[pubkey] {
		if have.id == pl.id {
			return
		}
	}
	r.pending[pubkey] = append(r.pending[pubkey], pl)
}

// proveLogin records the proof of a pending sign-in and creates the account
// of the companion, or refreshes its last sign-in. Returns false when the
// sign-in was no longer pending or had expired.
func (s *Store) proveLogin(ctx context.Context, id, pubkey, hash string, nowMs int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT pubkey FROM account_proofs
		WHERE request_id = ? AND kind = 'login' AND status = 'pending' AND expires_at >= ?`, id, nowMs).Scan(&stored)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored != pubkey { // the map and the table disagree: never cross keys
		return false, nil
	}
	var aid string
	err = tx.QueryRowContext(ctx, `SELECT account_id FROM accounts WHERE identity_pubkey = ?`, pubkey).Scan(&aid)
	switch {
	case err == sql.ErrNoRows:
		if aid, err = account.NewID(); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (account_id, identity_pubkey, created_at, last_login_at)
			VALUES (?, ?, ?, ?)`, aid, pubkey, nowMs, nowMs); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET last_login_at = ? WHERE account_id = ?`, nowMs, aid); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_proofs SET status = 'proven', proven_at = ?, proof_hash = ?, account_id = ?
		WHERE request_id = ? AND status = 'pending'`, nowMs, hash, aid, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// insertLogin stores a new pending sign-in, or reports the stored status of
// a replayed command. A sign-in over a cap is refused; it never evicts
// another, so a stranger cannot cancel the owner's.
func (s *Store) insertLogin(ctx context.Context, cmd account.Command, l account.Limits, nowMs int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM account_proofs WHERE request_id = ?`, cmd.RequestID).Scan(&status)
	if err == nil {
		return status, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	var total, forKey int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(pubkey = ?), 0) FROM account_proofs
		WHERE kind = 'login' AND status = 'pending' AND expires_at >= ?`, cmd.PubKey, nowMs).Scan(&total, &forKey); err != nil {
		return "", err
	}
	if total >= l.MaxPendingLogins {
		return "", errTooManyLogins
	}
	if forKey >= l.MaxPendingPerKey {
		return "", errTooManyKeyLogins
	}
	created := cmd.CreatedAt
	if created <= 0 {
		created = nowMs
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_proofs
		(request_id, kind, pubkey, code, redeem_hash, status, created_at, expires_at)
		VALUES (?, 'login', ?, ?, ?, 'pending', ?, ?)`,
		cmd.RequestID, cmd.PubKey, cmd.Code, cmd.RedeemHash, created, created+l.LoginTTL.Milliseconds()); err != nil {
		return "", err
	}
	return account.StatusPending, tx.Commit()
}

// applyAccountWrite runs one account write in a transaction, after checking
// that the account exists and that the command's generation is current.
func (s *Store) applyAccountWrite(ctx context.Context, cmd account.Command, l account.Limits, nowMs int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var gen int64
	err = tx.QueryRowContext(ctx, `SELECT session_gen FROM accounts WHERE account_id = ?`, cmd.AccountID).Scan(&gen)
	if err == sql.ErrNoRows {
		return errAccountNotFound
	}
	if err != nil {
		return err
	}
	if gen != cmd.Gen {
		return errSessionEnded
	}
	aid := cmd.AccountID
	switch cmd.Op {
	case account.OpMyNodesSet:
		if len(cmd.Nodes) > l.MaxMyNodes {
			return errTooManyMyNodes
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_my_nodes WHERE account_id = ?`, aid); err != nil {
			return err
		}
		for _, n := range cmd.Nodes {
			if err := insertMyNode(ctx, tx, aid, n, nowMs); err != nil {
				return err
			}
		}
	case account.OpMyNodeAdd:
		n := cmd.Nodes[0]
		var have, count int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(pubkey = ?), 0), COUNT(*) FROM account_my_nodes WHERE account_id = ?`,
			n.PubKey, aid).Scan(&have, &count); err != nil {
			return err
		}
		if have > 0 {
			break
		}
		if count >= l.MaxMyNodes {
			return errTooManyMyNodes
		}
		if err := insertMyNode(ctx, tx, aid, n, nowMs); err != nil {
			return err
		}
	case account.OpMyNodeRemove:
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_my_nodes WHERE account_id = ? AND pubkey = ?`, aid, cmd.PubKey); err != nil {
			return err
		}
	case account.OpProfileSet:
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET display_name = ? WHERE account_id = ?`,
			nullString(*cmd.DisplayName), aid); err != nil {
			return err
		}
	case account.OpSignOutAll:
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET session_gen = session_gen + 1 WHERE account_id = ?`, aid); err != nil {
			return err
		}
	case account.OpDelete:
		if err := deleteAccounts(ctx, tx, `account_id = ?`, aid); err != nil {
			return err
		}
	default:
		return account.ErrInvalidCommand
	}
	return tx.Commit()
}

func insertMyNode(ctx context.Context, tx *sql.Tx, aid string, n account.MyNode, nowMs int64) error {
	added := n.AddedAt
	if added <= 0 || added > nowMs {
		added = nowMs
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO account_my_nodes (account_id, pubkey, name_hint, added_at) VALUES (?, ?, ?, ?)`,
		aid, n.PubKey, nullString(n.Name), added)
	return err
}

// deleteAccounts removes the accounts matching where (on accounts) with
// their lists and proofs, inside tx. The deletes are explicit rather than
// left to ON DELETE CASCADE alone. Returns the number of accounts removed.
func deleteAccounts(ctx context.Context, tx *sql.Tx, where string, args ...interface{}) error {
	sel := `SELECT account_id FROM accounts WHERE ` + where
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_my_nodes WHERE account_id IN (`+sel+`)`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_proofs WHERE account_id IN (`+sel+`)`, args...); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE `+where, args...)
	return err
}

// expireLogins marks pending sign-ins whose code ran out as expired.
func (s *Store) expireLogins(ctx context.Context, nowMs int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_proofs SET status = 'expired' WHERE status = 'pending' AND expires_at < ?`, nowMs)
	return err
}

// PruneAccounts deletes finished sign-in rows ProofRetention after their
// code ran out, and the accounts without a sign-in since inactiveCutoffMs
// (24 months by default, owner decision 9), with their lists. One
// transaction. Returns the number of accounts removed.
func (s *Store) PruneAccounts(ctx context.Context, proofCutoffMs, inactiveCutoffMs int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_proofs WHERE status != 'pending' AND expires_at < ?`, proofCutoffMs); err != nil {
		return 0, err
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE last_login_at < ?`, inactiveCutoffMs).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		if err := deleteAccounts(ctx, tx, `last_login_at < ?`, inactiveCutoffMs); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// RunOnce applies the waiting commands, expires sign-ins whose code ran out
// and rebuilds the pending map after any change. With nothing to do it lists
// the queue directory and writes nothing.
func (r *accountRunner) RunOnce(ctx context.Context) {
	changed := !r.loaded
	cmds, err := r.queue.PendingValues()
	if err != nil {
		log.Printf("[accounts] list queue failed: %v", err)
	}
	for _, rc := range cmds {
		if rc.Err != nil {
			log.Printf("[accounts] discarding unreadable command %s: %v", rc.Path, rc.Err)
			if err := r.queue.Discard(rc.Path); err != nil {
				log.Printf("[accounts] discard %s failed: %v", rc.Path, err)
			}
			continue
		}
		res := r.apply(ctx, rc.Data)
		res.RequestID = rc.RequestID
		if err := r.queue.CompleteValue(rc.RequestID, res); err != nil {
			log.Printf("[accounts] write result %s failed: %v", rc.RequestID, err)
		}
		changed = true
	}
	nowMs := r.now().UnixMilli()
	if !r.loaded || r.hasExpired(nowMs) {
		if err := r.store.expireLogins(ctx, nowMs); err != nil {
			log.Printf("[accounts] expiry failed: %v", err)
		}
		changed = true
	}
	if changed {
		r.reload(ctx, nowMs)
	}
}

func (r *accountRunner) hasExpired(nowMs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.pending {
		for _, pl := range l {
			if pl.expiresAt < nowMs {
				return true
			}
		}
	}
	return false
}

// reload rebuilds the pending map from the table, bounded by
// MaxPendingLogins.
func (r *accountRunner) reload(ctx context.Context, nowMs int64) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT request_id, pubkey, code, expires_at FROM account_proofs
		WHERE kind = 'login' AND status = 'pending' AND expires_at >= ?
		ORDER BY created_at LIMIT ?`, nowMs, r.limits.MaxPendingLogins)
	if err != nil {
		log.Printf("[accounts] loading pending sign-ins failed: %v", err)
		return
	}
	defer rows.Close()
	next := map[string][]pendingLogin{}
	for rows.Next() {
		var pl pendingLogin
		var pubkey string
		if err := rows.Scan(&pl.id, &pubkey, &pl.code, &pl.expiresAt); err != nil {
			log.Printf("[accounts] loading pending sign-ins failed: %v", err)
			return
		}
		next[pubkey] = append(next[pubkey], pl)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[accounts] loading pending sign-ins failed: %v", err)
		return
	}
	r.mu.Lock()
	r.pending, r.loaded = next, true
	r.mu.Unlock()
}

func (r *accountRunner) apply(ctx context.Context, data []byte) account.Result {
	nowMs := r.now().UnixMilli()
	res := account.Result{CompletedAt: nowMs}
	fail := func(err error) account.Result {
		res.Status, res.Error = account.RequestError, userFacingAccountError(err)
		return res
	}
	var cmd account.Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return fail(account.ErrInvalidCommand)
	}
	if err := cmd.Validate(); err != nil {
		return fail(err)
	}
	ttl := r.limits.ResultTTL
	if cmd.Op == account.OpLogin {
		ttl = r.limits.LoginTTL
	}
	if cmd.CreatedAt > 0 && nowMs-cmd.CreatedAt > ttl.Milliseconds() {
		return fail(errAccountReqExpired)
	}
	if cmd.Op == account.OpLogin {
		status, err := r.store.insertLogin(ctx, cmd, r.limits, nowMs)
		if err != nil {
			return fail(err)
		}
		res.Status = status
		return res
	}
	if err := r.store.applyAccountWrite(ctx, cmd, r.limits, nowMs); err != nil {
		return fail(err)
	}
	res.Status = account.RequestDone
	return res
}

// userFacingAccountError keeps known outcomes readable and hides storage
// details, which are logged instead.
func userFacingAccountError(err error) string {
	switch {
	case errors.Is(err, errTooManyLogins), errors.Is(err, errTooManyKeyLogins), errors.Is(err, errAccountNotFound),
		errors.Is(err, errSessionEnded), errors.Is(err, errTooManyMyNodes), errors.Is(err, errAccountReqExpired),
		errors.Is(err, account.ErrInvalidCommand):
		return err.Error()
	}
	log.Printf("[accounts] command failed: %v", err)
	return errAccountStorage.Error()
}

// Prune applies retention to the tables and to this queue's result files.
func (r *accountRunner) Prune(ctx context.Context) {
	now := r.now()
	proofCutoff := now.Add(-account.ProofRetention).UnixMilli()
	inactiveCutoff := now.Add(-r.limits.InactiveAfter).UnixMilli()
	if n, err := r.store.PruneAccounts(ctx, proofCutoff, inactiveCutoff); err != nil {
		log.Printf("[accounts] retention failed: %v", err)
	} else if n > 0 {
		log.Printf("[accounts] retention removed %d inactive account(s)", n)
	}
	if _, err := r.queue.Prune(now, r.limits.ResultTTL, r.limits.MaxResults); err != nil {
		log.Printf("[accounts] queue prune failed: %v", err)
	}
}

// Start runs RunOnce every tick and Prune every pruneEvery. The returned func
// stops the loop and waits for it.
func (r *accountRunner) Start(tick, pruneEvery time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(tick)
		defer t.Stop()
		lastPrune := time.Time{}
		for {
			r.RunOnce(ctx)
			if time.Since(lastPrune) >= pruneEvery {
				r.Prune(ctx)
				lastPrune = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
