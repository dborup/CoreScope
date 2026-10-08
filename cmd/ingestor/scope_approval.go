package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/regions"
)

const maxApprovedRegionScopes = 128
const scopeQueueDir = "region-scope-requests"

type scopeApprovalRunner struct {
	store *Store
	keys  *hotKeys
	queue *channelregistry.Queue
}

func newScopeApprovalRunner(store *Store, keys *hotKeys) *scopeApprovalRunner {
	return &scopeApprovalRunner{store: store, keys: keys,
		queue: channelregistry.NewQueue(filepath.Join(filepath.Dir(store.path), scopeQueueDir))}
}

func (r *scopeApprovalRunner) LoadApproved(ctx context.Context) error {
	rows, err := r.store.db.QueryContext(ctx, `SELECT name FROM approved_region_scopes WHERE status = 'approved' LIMIT ?`, maxApprovedRegionScopes+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if err := regions.ValidateCandidate(name); err != nil {
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) > maxApprovedRegionScopes {
		return errors.New("approved region scope limit exceeded")
	}
	r.keys.AddApprovedRegions(names...)
	return nil
}

// observedScope rechecks the current OTA neighbor snapshot in the writer.
// This is admin-event work, never packet-path work.
func (r *scopeApprovalRunner) observedScope(ctx context.Context, name string) (bool, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT scopes FROM observer_neighbors WHERE scopes IS NOT NULL`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var scopes string
		if err := rows.Scan(&scopes); err != nil {
			return false, err
		}
		for _, raw := range strings.Split(scopes, ",") {
			s := strings.TrimSpace(raw)
			if s != "" && !strings.HasPrefix(s, "#") {
				s = "#" + s
			}
			if s == name {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func (r *scopeApprovalRunner) apply(ctx context.Context, cmd channelregistry.Command) channelregistry.Result {
	now := time.Now().UnixMilli()
	res := channelregistry.Result{RequestID: cmd.RequestID, CompletedAt: now}
	if err := regions.ValidateCandidate(cmd.Name); err != nil {
		res.Status, res.Error = channelregistry.RequestError, err.Error()
		return res
	}
	if cmd.CreatedAt <= 0 || now-cmd.CreatedAt > 5*60*1000 {
		res.Status, res.Error = channelregistry.RequestError, "request expired"
		return res
	}
	if cmd.Op != channelregistry.OpScopeApprove && cmd.Op != channelregistry.OpScopeReject && cmd.Op != channelregistry.OpScopeRevoke {
		res.Status, res.Error = channelregistry.RequestError, "invalid scope decision"
		return res
	}
	// The result file can fail after the DB commit. Durable request identity
	// must win even if a later decision has since changed this scope or the
	// OTA observation disappeared before this command is replayed.
	var priorName, priorStatus string
	err := r.store.db.QueryRowContext(ctx, `SELECT name,new_status FROM region_scope_audit WHERE request_id = ?`, cmd.RequestID).Scan(&priorName, &priorStatus)
	if err == nil {
		res.Status = priorStatus
		res.Proposal = &channelregistry.Proposal{Name: priorName, Status: priorStatus}
		return res
	}
	if err != sql.ErrNoRows {
		res.Status, res.Error = channelregistry.RequestError, "could not verify decision request"
		return res
	}
	if cmd.Op != channelregistry.OpScopeRevoke {
		found, err := r.observedScope(ctx, cmd.Name)
		if err != nil {
			log.Printf("[scope-approval] scope observation failed: %v", err)
			res.Status, res.Error = channelregistry.RequestError, "could not verify observed scope"
			return res
		}
		if !found {
			res.Status, res.Error = channelregistry.RequestError, "scope is no longer observed"
			return res
		}
	}
	status := map[string]string{
		channelregistry.OpScopeApprove: "approved",
		channelregistry.OpScopeReject:  "rejected",
		channelregistry.OpScopeRevoke:  "revoked",
	}[cmd.Op]
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		res.Status, res.Error = channelregistry.RequestError, "could not save decision"
		return res
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT status FROM approved_region_scopes WHERE name = ?`, cmd.Name).Scan(&old)
	if err != nil && err != sql.ErrNoRows {
		res.Status, res.Error = channelregistry.RequestError, "could not save decision"
		return res
	}
	if cmd.Op == channelregistry.OpScopeRevoke && old != "approved" && old != "revoked" {
		res.Status, res.Error = channelregistry.RequestError, "scope is not approved"
		return res
	}
	if cmd.Op == channelregistry.OpScopeReject && old == "approved" {
		res.Status, res.Error = channelregistry.RequestError, "approved scope must be revoked"
		return res
	}
	if status == "approved" && old != "approved" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approved_region_scopes WHERE status = 'approved'`).Scan(&n); err != nil {
			res.Status, res.Error = channelregistry.RequestError, "could not save decision"
			return res
		}
		if n >= maxApprovedRegionScopes {
			res.Status, res.Error = channelregistry.RequestError, "approved scope limit reached"
			return res
		}
	}
	if old != status {
		_, err = tx.ExecContext(ctx, `INSERT INTO approved_region_scopes (name,status,created_at,reviewed_at) VALUES (?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET status=excluded.status, reviewed_at=excluded.reviewed_at`, cmd.Name, status, now, now)
		if err != nil {
			log.Printf("[scope-approval] save failed: %v", err)
			res.Status, res.Error = channelregistry.RequestError, "could not save decision"
			return res
		}
		// Keep all active approvals (at most 128), but bound old rejection
		// and revocation decisions so an unlimited set of OTA names cannot
		// grow this table forever.
		_, err = tx.ExecContext(ctx, `DELETE FROM approved_region_scopes WHERE name IN (
			SELECT name FROM approved_region_scopes WHERE status IN ('rejected','revoked')
			ORDER BY reviewed_at DESC, name DESC LIMIT -1 OFFSET 3968)`)
		if err != nil {
			res.Status, res.Error = channelregistry.RequestError, "could not bound decisions"
			return res
		}
	}
	// Keep request identities for at least 10 minutes, twice the command TTL.
	// If 10,000 decisions arrive within that period, refuse new decisions
	// rather than evict a still-replayable request ID.
	_, err = tx.ExecContext(ctx, `DELETE FROM region_scope_audit WHERE decided_at < ? AND request_id IN (
		SELECT request_id FROM region_scope_audit ORDER BY decided_at DESC, request_id DESC LIMIT -1 OFFSET 1000)`, now-10*60*1000)
	if err != nil {
		res.Status, res.Error = channelregistry.RequestError, "could not prune audit"
		return res
	}
	var auditCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM region_scope_audit`).Scan(&auditCount); err != nil {
		res.Status, res.Error = channelregistry.RequestError, "could not count audit"
		return res
	}
	if auditCount >= 10000 {
		res.Status, res.Error = channelregistry.RequestError, "scope decision rate limit reached"
		return res
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO region_scope_audit(request_id,name,operation,previous_status,new_status,decided_at) VALUES (?,?,?,?,?,?)`, cmd.RequestID, cmd.Name, cmd.Op, old, status, now)
	if err != nil {
		res.Status, res.Error = channelregistry.RequestError, "could not save audit"
		return res
	}
	if err := tx.Commit(); err != nil {
		res.Status, res.Error = channelregistry.RequestError, "could not save decision"
		return res
	}
	if status == "approved" {
		r.keys.AddApprovedRegions(cmd.Name)
	} else {
		r.keys.RemoveApprovedRegion(cmd.Name)
	}
	res.Status = status
	res.Proposal = &channelregistry.Proposal{Name: cmd.Name, Status: status, ReviewedAt: &now}
	return res
}

func (r *scopeApprovalRunner) RunOnce(ctx context.Context) {
	cmds, err := r.queue.Pending()
	if err != nil {
		log.Printf("[scope-approval] queue read failed: %v", err)
		return
	}
	for _, qc := range cmds {
		if qc.Err != nil {
			_ = r.queue.Discard(qc.Path)
			continue
		}
		// Complete writes the result before removing the command. If a crash
		// leaves both, the committed decision must not be applied again after
		// a later admin action has changed this scope.
		if prior, err := r.queue.Lookup(qc.Command.RequestID); err == nil && prior.Status != channelregistry.RequestQueued {
			_ = r.queue.Discard(qc.Path)
			continue
		}
		res := r.apply(ctx, qc.Command)
		if err := r.queue.Complete(res); err != nil {
			log.Printf("[scope-approval] result failed: %v", err)
		}
	}
}

func (r *scopeApprovalRunner) Start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			r.RunOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
