package main

// Owner antenna-height claims (#789 M2a) — the writer side.
//
// The read-only server issues a claim (a random code bound to one public key
// and one height, or a clear) as a command file in the antenna-claim queue.
// This file turns it into a node_antenna_claims row and keeps the pending
// claims in a bounded in-memory map. At advert ingest, an advert that is new
// (not a re-received copy: same content hash) and whose ed25519 signature
// verified is looked up in that map by its full public key, and a pending
// code found as a whole token in the on-air part of its name proves the claim.
// The code is then used up.
//
// Why nobody else can prove it: the signature covers the name, so the advert
// must come from the key holder or the node's admin CLI; the code is drawn
// from crypto/rand after the claim was issued, so no earlier advert carries
// it; a re-received or replayed advert has the same content hash and is never
// new. No advert timestamp or observer time is trusted.
//
// The ingestor knows nothing about hidden identities (#743): the server
// refuses to issue claims for hidden nodes and never serves their heights.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// User-facing outcomes, returned verbatim in the request status.
var (
	errTooManyClaims     = errors.New("too many antenna height claims are waiting, please try again later")
	errTooManyNodeClaims = errors.New("this node already has the maximum number of claims waiting for an advert")
	errClaimNotFound     = errors.New("claim not found")
	errClaimExpired      = errors.New("the request expired before it could be processed")
	errClaimStorage      = errors.New("the claim could not be saved")
)

// errClaimConflict is a decision on a claim that is not in a status it
// applies to.
type errClaimConflict struct{ status string }

func (e errClaimConflict) Error() string { return "claim is " + e.status }

// pendingClaim is one claim waiting for its advert.
type pendingClaim struct {
	id        string
	code      string
	expiresAt int64 // Unix ms
	clear     bool
}

type antennaClaimRunner struct {
	store       *Store
	queue       *channelregistry.Queue
	limits      antennaclaim.Limits
	autoApprove bool
	now         func() time.Time

	mu sync.Mutex
	// pending maps a public key to its pending claims: at most
	// MaxPendingPerNode each and MaxPending in all, both enforced when a
	// claim is inserted. Rebuilt from the table after every change.
	pending map[string][]pendingClaim
	loaded  bool
}

func newAntennaClaimRunner(store *Store, cfg *antennaclaim.Config) *antennaClaimRunner {
	return &antennaClaimRunner{
		store:       store,
		queue:       channelregistry.NewQueue(antennaclaim.QueueDir(store.path)),
		limits:      cfg.Limits(),
		autoApprove: cfg.AutoApprove(),
		now:         time.Now,
		pending:     map[string][]pendingClaim{},
	}
}

func (r *antennaClaimRunner) pendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.pending {
		n += len(l)
	}
	return n
}

// match finds and removes the pending claim of pubkey whose code is a whole
// token within the first budget bytes of name. hasPos is whether the advert
// carries a position: a height is bound to where it was proven, so only a
// clear claim may be proven without one. O(1) map lookup plus a scan of at
// most MaxPendingPerNode codes over at most 32 bytes.
func (r *antennaClaimRunner) match(pubkey, name string, budget int, hasPos bool) (pendingClaim, bool) {
	nowMs := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.pending[pubkey]
	for i, pc := range list {
		if pc.expiresAt < nowMs || (!pc.clear && !hasPos) {
			continue
		}
		if antennaclaim.ContainsCode(name, budget, pc.code) {
			rest := append(list[:i:i], list[i+1:]...)
			if len(rest) == 0 {
				delete(r.pending, pubkey)
			} else {
				r.pending[pubkey] = rest
			}
			return pc, true
		}
	}
	return pendingClaim{}, false
}

// observeAdvert is the ingest hook, called for every advert after it was
// stored. Only a new transmission with a verified signature can prove a
// claim; with signature validation off, nothing is ever proven.
func (r *antennaClaimRunner) observeAdvert(p *Payload, hash string, isNew bool) {
	if r == nil || !isNew || p == nil || p.SignatureValid == nil || !*p.SignatureValid || p.Flags == nil {
		return
	}
	pubkey := strings.ToLower(p.PubKey)
	budget := antennaclaim.NameBudget(p.Flags.HasLocation, p.Flags.HasFeat1, p.Flags.HasFeat2)
	hasPos := p.Lat != nil && p.Lon != nil
	pc, ok := r.match(pubkey, p.Name, budget, hasPos)
	if !ok {
		return
	}
	status, err := r.store.proveAntennaClaim(context.Background(), pc.id, hash, p.Lat, p.Lon, r.autoApprove, r.now().UnixMilli())
	if err != nil {
		log.Printf("[antenna-claims] proving %s failed: %v", pc.id, err)
		return
	}
	if status != "" {
		log.Printf("[antenna-claims] claim %s for %s… is now %s", pc.id, pubkey[:16], status)
	}
}

// proveAntennaClaim records the proof of a pending claim. A clear claim takes
// effect at once (the owner withdraws their own height, owner decision 4); a
// height is approved automatically only with autoApprove and no review flag,
// otherwise it waits as proven. Returns the new status, or "" when the claim
// was no longer pending or had expired (a racing match, a reject).
func (s *Store) proveAntennaClaim(ctx context.Context, id, hash string, lat, lon *float64, autoApprove bool, nowMs int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var (
		pubkey      string
		isClear     bool
		needsReview bool
	)
	err = tx.QueryRowContext(ctx, `SELECT pubkey, height_m IS NULL, needs_review FROM node_antenna_claims
		WHERE request_id = ? AND status = 'pending' AND expires_at >= ?`, id, nowMs).Scan(&pubkey, &isClear, &needsReview)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	status, approvedBy := antennaclaim.StatusProven, sql.NullString{}
	var reviewedAt sql.NullInt64
	switch {
	case isClear:
		status, approvedBy = antennaclaim.StatusApproved, sql.NullString{String: antennaclaim.ApprovedByOwner, Valid: true}
	case autoApprove && !needsReview:
		status, approvedBy = antennaclaim.StatusApproved, sql.NullString{String: antennaclaim.ApprovedByAuto, Valid: true}
	}
	if status == antennaclaim.StatusApproved {
		reviewedAt = sql.NullInt64{Int64: nowMs, Valid: true}
		if err := supersedeApproved(ctx, tx, pubkey, id, nowMs); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE node_antenna_claims
		SET status = ?, proven_at = ?, proof_hash = ?, proof_lat = ?, proof_lon = ?, reviewed_at = ?, approved_by = ?
		WHERE request_id = ? AND status = 'pending'`,
		status, nowMs, hash, lat, lon, reviewedAt, approvedBy, id); err != nil {
		return "", err
	}
	return status, tx.Commit()
}

// supersedeApproved retires every other approved claim of pubkey: one claim
// per key is in force.
func supersedeApproved(ctx context.Context, tx *sql.Tx, pubkey, keep string, nowMs int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE node_antenna_claims SET status = 'superseded', reviewed_at = ?
		WHERE pubkey = ? AND status = 'approved' AND request_id != ?`, nowMs, pubkey, keep)
	return err
}

// insertAntennaClaim stores a new pending claim, or reports the stored status
// of a replayed command (the claim's id is its request id). The caps are
// checked here, inside the transaction: a claim over a cap is refused, it
// never evicts another (a stranger's request must not cancel the owner's).
func (s *Store) insertAntennaClaim(ctx context.Context, cmd antennaclaim.Command, l antennaclaim.Limits, nowMs int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM node_antenna_claims WHERE request_id = ?`, cmd.RequestID).Scan(&status)
	if err == nil {
		return status, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	var total, forKey int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(pubkey = ?), 0) FROM node_antenna_claims WHERE status = 'pending'`, cmd.PubKey).Scan(&total, &forKey); err != nil {
		return "", err
	}
	if total >= l.MaxPending {
		return "", errTooManyClaims
	}
	if forKey >= l.MaxPendingPerNode {
		return "", errTooManyNodeClaims
	}
	created := cmd.CreatedAt
	if created <= 0 {
		created = nowMs
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_antenna_claims
		(request_id, pubkey, height_m, code, status, needs_review, created_at, expires_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)`,
		cmd.RequestID, cmd.PubKey, cmd.HeightM, cmd.Code, cmd.NeedsReview, created, created+l.RequestTTL.Milliseconds()); err != nil {
		return "", err
	}
	return antennaclaim.StatusPending, tx.Commit()
}

// reviewAntennaClaim applies an administrator's decision: a claim in one of
// sources moves to target. Repeating a decision is idempotent.
func (s *Store) reviewAntennaClaim(ctx context.Context, id string, sources []string, target string, nowMs int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var status, pubkey string
	err = tx.QueryRowContext(ctx, `SELECT status, pubkey FROM node_antenna_claims WHERE request_id = ?`, id).Scan(&status, &pubkey)
	if err == sql.ErrNoRows {
		return "", errClaimNotFound
	}
	if err != nil {
		return "", err
	}
	if status == target {
		return status, nil
	}
	allowed := false
	for _, src := range sources {
		allowed = allowed || status == src
	}
	if !allowed {
		return status, errClaimConflict{status: status}
	}
	var approvedBy sql.NullString
	if target == antennaclaim.StatusApproved {
		approvedBy = sql.NullString{String: antennaclaim.ApprovedByAdmin, Valid: true}
		if err := supersedeApproved(ctx, tx, pubkey, id, nowMs); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE node_antenna_claims SET status = ?, reviewed_at = ?, approved_by = ?
		WHERE request_id = ? AND status = ?`, target, nowMs, approvedBy, id, status); err != nil {
		return "", err
	}
	return target, tx.Commit()
}

// PruneAntennaClaims deletes finished claims decided before cutoffMs and
// claims still pending or proven that were issued before it. The claim in
// force (approved) is kept: at most one per key.
func (s *Store) PruneAntennaClaims(ctx context.Context, cutoffMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM node_antenna_claims
		WHERE (status IN ('rejected', 'revoked', 'expired', 'superseded') AND COALESCE(reviewed_at, created_at) < ?)
		   OR (status IN ('pending', 'proven') AND created_at < ?)`, cutoffMs, cutoffMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RunOnce applies the waiting commands, expires claims whose code ran out,
// and rebuilds the pending map after any change. With nothing to do it
// lists the queue directory and writes nothing.
func (r *antennaClaimRunner) RunOnce(ctx context.Context) {
	changed := !r.loaded
	cmds, err := r.queue.PendingValues()
	if err != nil {
		log.Printf("[antenna-claims] list queue failed: %v", err)
	}
	for _, rc := range cmds {
		if rc.Err != nil {
			log.Printf("[antenna-claims] discarding unreadable command %s: %v", rc.Path, rc.Err)
			if err := r.queue.Discard(rc.Path); err != nil {
				log.Printf("[antenna-claims] discard %s failed: %v", rc.Path, err)
			}
			continue
		}
		res := r.apply(ctx, rc.Data)
		res.RequestID = rc.RequestID
		if err := r.queue.CompleteValue(rc.RequestID, res); err != nil {
			log.Printf("[antenna-claims] write result %s failed: %v", rc.RequestID, err)
		}
		changed = true
	}
	nowMs := r.now().UnixMilli()
	if r.hasExpired(nowMs) {
		if _, err := r.store.db.ExecContext(ctx, `UPDATE node_antenna_claims SET status = 'expired'
			WHERE status = 'pending' AND expires_at < ?`, nowMs); err != nil {
			log.Printf("[antenna-claims] expiry failed: %v", err)
		}
		changed = true
	}
	if changed {
		r.reload(ctx, nowMs)
	}
}

func (r *antennaClaimRunner) hasExpired(nowMs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.pending {
		for _, pc := range l {
			if pc.expiresAt < nowMs {
				return true
			}
		}
	}
	return false
}

// reload rebuilds the pending map from the table, bounded by MaxPending.
func (r *antennaClaimRunner) reload(ctx context.Context, nowMs int64) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT request_id, pubkey, code, expires_at, height_m IS NULL
		FROM node_antenna_claims WHERE status = 'pending' AND expires_at >= ?
		ORDER BY created_at LIMIT ?`, nowMs, r.limits.MaxPending)
	if err != nil {
		log.Printf("[antenna-claims] loading pending claims failed: %v", err)
		return
	}
	defer rows.Close()
	next := map[string][]pendingClaim{}
	for rows.Next() {
		var pc pendingClaim
		var pubkey string
		if err := rows.Scan(&pc.id, &pubkey, &pc.code, &pc.expiresAt, &pc.clear); err != nil {
			log.Printf("[antenna-claims] loading pending claims failed: %v", err)
			return
		}
		next[pubkey] = append(next[pubkey], pc)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[antenna-claims] loading pending claims failed: %v", err)
		return
	}
	r.mu.Lock()
	r.pending, r.loaded = next, true
	r.mu.Unlock()
}

func (r *antennaClaimRunner) apply(ctx context.Context, data []byte) antennaclaim.Result {
	nowMs := r.now().UnixMilli()
	res := antennaclaim.Result{CompletedAt: nowMs}
	fail := func(err error) antennaclaim.Result {
		res.Status, res.Error = antennaclaim.RequestError, userFacingClaimError(err)
		return res
	}
	var cmd antennaclaim.Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return fail(antennaclaim.ErrInvalidCommand)
	}
	// Never trust the queue file: validate again with the shared rules.
	if err := cmd.Validate(); err != nil {
		return fail(err)
	}
	if cmd.CreatedAt > 0 && nowMs-cmd.CreatedAt > r.limits.RequestTTL.Milliseconds() {
		return fail(errClaimExpired)
	}
	var (
		status string
		err    error
	)
	switch cmd.Op {
	case antennaclaim.OpClaim:
		res.ClaimID = cmd.RequestID
		status, err = r.store.insertAntennaClaim(ctx, cmd, r.limits, nowMs)
	case antennaclaim.OpApprove:
		res.ClaimID = cmd.ClaimID
		status, err = r.store.reviewAntennaClaim(ctx, cmd.ClaimID, []string{antennaclaim.StatusProven}, antennaclaim.StatusApproved, nowMs)
	case antennaclaim.OpReject:
		res.ClaimID = cmd.ClaimID
		status, err = r.store.reviewAntennaClaim(ctx, cmd.ClaimID, []string{antennaclaim.StatusPending, antennaclaim.StatusProven}, antennaclaim.StatusRejected, nowMs)
	case antennaclaim.OpRevoke:
		res.ClaimID = cmd.ClaimID
		status, err = r.store.reviewAntennaClaim(ctx, cmd.ClaimID, []string{antennaclaim.StatusApproved}, antennaclaim.StatusRevoked, nowMs)
	}
	if err != nil {
		if cmd.Op == antennaclaim.OpClaim {
			res.ClaimID = ""
		}
		return fail(err)
	}
	res.Status = status
	return res
}

// userFacingClaimError keeps known outcomes readable and hides storage
// details, which are logged instead.
func userFacingClaimError(err error) string {
	var conflict errClaimConflict
	switch {
	case errors.As(err, &conflict), errors.Is(err, errTooManyClaims), errors.Is(err, errTooManyNodeClaims),
		errors.Is(err, errClaimNotFound), errors.Is(err, errClaimExpired), errors.Is(err, antennaclaim.ErrInvalidCommand):
		return err.Error()
	}
	log.Printf("[antenna-claims] command failed: %v", err)
	return errClaimStorage.Error()
}

// Prune applies retention to the table and to this queue's result files.
func (r *antennaClaimRunner) Prune(ctx context.Context) {
	now := r.now()
	cutoff := now.Add(-time.Duration(r.limits.RetentionDays) * 24 * time.Hour).UnixMilli()
	if n, err := r.store.PruneAntennaClaims(ctx, cutoff); err != nil {
		log.Printf("[antenna-claims] retention failed: %v", err)
	} else if n > 0 {
		log.Printf("[antenna-claims] retention removed %d old claim(s)", n)
	}
	if _, err := r.queue.Prune(now, r.limits.RequestTTL, r.limits.MaxResults); err != nil {
		log.Printf("[antenna-claims] queue prune failed: %v", err)
	}
}

// Start runs RunOnce every tick and Prune every pruneEvery. The returned func
// stops the loop and waits for it.
func (r *antennaClaimRunner) Start(tick, pruneEvery time.Duration) (stop func()) {
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
