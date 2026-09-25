package main

// Shared channel proposals — the writer side.
//
// The read-only server validates public suggestions and admin decisions and
// writes them as command files into the channelregistry queue next to the
// database. This file is where they become rows: the ingestor applies each
// command in a transaction, writes a result file the server reads back, and
// adds newly approved channels to the live channel keys so their traffic is
// decrypted from then on.
//
// Every command is idempotent, because a crash can land between the commit
// and the result write (the command is then applied again on the next tick):
//   - submit: the new proposal's id IS the request id, so a replay finds its
//     own row and reports it instead of inserting twice. A different request
//     for an existing name is a duplicate and reports the existing proposal.
//   - approve/reject: repeating the decision that is already stored reports
//     it unchanged; only a *conflicting* repeat (reject after approve) fails.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

// User-facing outcomes. Their text is returned verbatim in the request status.
var (
	errTooManyPending   = errors.New("too many suggestions are waiting for review, please try again later")
	errTooManyApproved  = errors.New("the limit of shared channels has been reached")
	errProposalNotFound = errors.New("suggestion not found")
	errRequestExpired   = errors.New("the request expired before it could be processed")
	errProposalStorage  = errors.New("the suggestion could not be saved")
)

// errProposalConflict reports a decision that contradicts the stored one.
type errProposalConflict struct{ status string }

func (e errProposalConflict) Error() string {
	return "suggestion was already " + e.status
}

const proposalColumns = `id, name, status, created_at, reviewed_at`

func scanProposalRow(row *sql.Row) (channelregistry.Proposal, bool, error) {
	var p channelregistry.Proposal
	var reviewed sql.NullInt64
	err := row.Scan(&p.ID, &p.Name, &p.Status, &p.CreatedAt, &reviewed)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if reviewed.Valid {
		v := reviewed.Int64
		p.ReviewedAt = &v
	}
	return p, true, nil
}

// submitChannelProposal stores a new pending proposal, or reports the existing
// one for a replayed request or an already-proposed name.
func (s *Store) submitChannelProposal(ctx context.Context, cmd channelregistry.Command, maxPending int, nowMs int64) (channelregistry.Proposal, error) {
	// Never trust the queue file: validate again with the shared rules.
	name, err := channelregistry.NormalizeName(cmd.Name)
	if err != nil {
		return channelregistry.Proposal{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return channelregistry.Proposal{}, err
	}
	defer tx.Rollback()

	// Prefer our own row (replay) over another request's row (duplicate).
	existing, found, err := scanProposalRow(tx.QueryRowContext(ctx,
		`SELECT `+proposalColumns+` FROM channel_proposals WHERE id = ? OR name = ?
		 ORDER BY (id = ?) DESC LIMIT 1`, cmd.RequestID, name, cmd.RequestID))
	if err != nil {
		return channelregistry.Proposal{}, err
	}
	if found {
		return existing, nil
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_proposals WHERE status = 'pending'`).Scan(&pending); err != nil {
		return channelregistry.Proposal{}, err
	}
	if pending >= maxPending {
		return channelregistry.Proposal{}, errTooManyPending
	}
	created := cmd.CreatedAt
	if created <= 0 {
		created = nowMs
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channel_proposals (id, name, status, created_at) VALUES (?, ?, 'pending', ?)`,
		cmd.RequestID, name, created); err != nil {
		return channelregistry.Proposal{}, err
	}
	if err := tx.Commit(); err != nil {
		return channelregistry.Proposal{}, err
	}
	return channelregistry.Proposal{ID: cmd.RequestID, Name: name, Status: channelregistry.StatusPending, CreatedAt: created}, nil
}

// reviewChannelProposal moves a pending proposal to approved or rejected.
func (s *Store) reviewChannelProposal(ctx context.Context, id, target string, maxApproved int, nowMs int64) (channelregistry.Proposal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return channelregistry.Proposal{}, err
	}
	defer tx.Rollback()
	p, found, err := scanProposalRow(tx.QueryRowContext(ctx,
		`SELECT `+proposalColumns+` FROM channel_proposals WHERE id = ?`, id))
	if err != nil {
		return channelregistry.Proposal{}, err
	}
	if !found {
		return channelregistry.Proposal{}, errProposalNotFound
	}
	if p.Status == target {
		return p, nil // repeated decision: nothing to do
	}
	if p.Status != channelregistry.StatusPending {
		return p, errProposalConflict{p.Status}
	}
	if target == channelregistry.StatusApproved {
		var approved int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_proposals WHERE status = 'approved'`).Scan(&approved); err != nil {
			return channelregistry.Proposal{}, err
		}
		if approved >= maxApproved {
			return p, errTooManyApproved
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE channel_proposals SET status = ?, reviewed_at = ? WHERE id = ? AND status = 'pending'`,
		target, nowMs, id); err != nil {
		return channelregistry.Proposal{}, err
	}
	if err := tx.Commit(); err != nil {
		return channelregistry.Proposal{}, err
	}
	p.Status = target
	p.ReviewedAt = &nowMs
	return p, nil
}

// PruneChannelProposals deletes rejected proposals reviewed before cutoffMs
// and pending ones submitted before it. Approved channels are kept: they are
// bounded by maxApproved and must stay active.
func (s *Store) PruneChannelProposals(ctx context.Context, cutoffMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM channel_proposals
		 WHERE (status = 'rejected' AND reviewed_at < ?)
		    OR (status = 'pending' AND created_at < ?)`, cutoffMs, cutoffMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// channelProposalRunner drains the queue and keeps the live keys in step.
type channelProposalRunner struct {
	store  *Store
	queue  *channelregistry.Queue
	keys   *hotKeys
	limits channelregistry.Limits
	now    func() time.Time
}

func newChannelProposalRunner(store *Store, keys *hotKeys, cfg *channelregistry.Config) *channelProposalRunner {
	return &channelProposalRunner{
		store:  store,
		queue:  channelregistry.NewQueue(channelregistry.QueueDir(store.path)),
		keys:   keys,
		limits: cfg.Limits(),
		now:    time.Now,
	}
}

// LoadApproved adds every approved channel to the live keys. Called at
// startup, before MQTT traffic is handled.
func (r *channelProposalRunner) LoadApproved(ctx context.Context) (int, error) {
	names, err := channelregistry.ListApprovedNames(ctx, r.store.db, channelregistry.MaxListed)
	if err != nil {
		return 0, err
	}
	return r.keys.AddApproved(names...), nil
}

// RunOnce applies every waiting command in submission order.
func (r *channelProposalRunner) RunOnce(ctx context.Context) {
	cmds, err := r.queue.Pending()
	if err != nil {
		log.Printf("[channel-proposals] list queue failed: %v", err)
		return
	}
	for _, qc := range cmds {
		if qc.Err != nil {
			log.Printf("[channel-proposals] discarding unreadable command %s: %v", qc.Path, qc.Err)
			if err := r.queue.Discard(qc.Path); err != nil {
				log.Printf("[channel-proposals] discard %s failed: %v", qc.Path, err)
			}
			continue
		}
		res := r.apply(ctx, qc.Command)
		// Keys first: the row is committed, so the channel is approved even
		// if writing the result fails below (it is retried next tick).
		if qc.Command.Op == channelregistry.OpApprove && res.Status == channelregistry.RequestApproved && res.Proposal != nil {
			if r.keys.AddApproved(res.Proposal.Name) > 0 {
				log.Printf("[channel-proposals] approved %q — added to channel keys", res.Proposal.Name)
			}
		}
		if err := r.queue.Complete(res); err != nil {
			log.Printf("[channel-proposals] write result %s failed: %v", res.RequestID, err)
		}
	}
}

func (r *channelProposalRunner) apply(ctx context.Context, cmd channelregistry.Command) channelregistry.Result {
	now := r.now()
	nowMs := now.UnixMilli()
	res := channelregistry.Result{RequestID: cmd.RequestID, CompletedAt: nowMs}
	if cmd.CreatedAt > 0 && nowMs-cmd.CreatedAt > r.limits.RequestTTL.Milliseconds() {
		res.Status, res.Error = channelregistry.RequestError, errRequestExpired.Error()
		return res
	}
	var (
		p   channelregistry.Proposal
		err error
	)
	switch cmd.Op {
	case channelregistry.OpSubmit:
		p, err = r.store.submitChannelProposal(ctx, cmd, r.limits.MaxPending, nowMs)
	case channelregistry.OpApprove:
		p, err = r.store.reviewChannelProposal(ctx, cmd.ProposalID, channelregistry.StatusApproved, r.limits.MaxApproved, nowMs)
	case channelregistry.OpReject:
		p, err = r.store.reviewChannelProposal(ctx, cmd.ProposalID, channelregistry.StatusRejected, r.limits.MaxApproved, nowMs)
	default:
		err = channelregistry.ErrInvalidCommand
	}
	if err != nil {
		res.Status, res.Error = channelregistry.RequestError, userFacingProposalError(cmd, err)
		if p.ID != "" {
			res.Proposal = &p
		}
		return res
	}
	res.Status = p.Status
	res.Proposal = &p
	return res
}

// userFacingProposalError keeps known outcomes readable and hides storage
// details, which are logged instead.
func userFacingProposalError(cmd channelregistry.Command, err error) string {
	var conflict errProposalConflict
	switch {
	case errors.As(err, &conflict),
		errors.Is(err, errTooManyPending), errors.Is(err, errTooManyApproved),
		errors.Is(err, errProposalNotFound), errors.Is(err, channelregistry.ErrInvalidCommand),
		errors.Is(err, channelregistry.ErrNameEmpty), errors.Is(err, channelregistry.ErrNameTooLong),
		errors.Is(err, channelregistry.ErrNameInvalidUTF8), errors.Is(err, channelregistry.ErrNameControl),
		errors.Is(err, channelregistry.ErrNameSpace):
		return err.Error()
	}
	log.Printf("[channel-proposals] %s %s failed: %v", cmd.Op, cmd.RequestID, err)
	return errProposalStorage.Error()
}

// Prune applies retention to the table and the queue's result files.
func (r *channelProposalRunner) Prune(ctx context.Context) {
	now := r.now()
	cutoff := now.Add(-time.Duration(r.limits.RetentionDays) * 24 * time.Hour).UnixMilli()
	if n, err := r.store.PruneChannelProposals(ctx, cutoff); err != nil {
		if !channelregistry.IsMissingTable(err) {
			log.Printf("[channel-proposals] retention failed: %v", err)
		}
	} else if n > 0 {
		log.Printf("[channel-proposals] retention removed %d old suggestion(s)", n)
	}
	if _, err := r.queue.Prune(now, r.limits.RequestTTL, r.limits.MaxResults); err != nil {
		log.Printf("[channel-proposals] queue prune failed: %v", err)
	}
}

// Start runs RunOnce every tick and Prune every pruneEvery. The returned func
// stops the loop and waits for it.
func (r *channelProposalRunner) Start(tick, pruneEvery time.Duration) (stop func()) {
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
