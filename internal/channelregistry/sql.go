package channelregistry

import (
	"context"
	"database/sql"
	"strings"
)

// TableName is the table the ingestor creates (internal/dbschema) and owns.
const TableName = "channel_proposals"

// MaxListed caps every read. The table itself is bounded by MaxPending,
// MaxApproved and retention; this is a second guard against an operator
// lowering MaxApproved below what is already approved.
const MaxListed = 1024

// IsMissingTable reports whether err is SQLite's "no such table" for the
// proposals table. That is the normal state while a new server runs against
// a database an older ingestor has not migrated yet, so readers treat it as
// an empty table rather than an error.
func IsMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: "+TableName)
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > MaxListed {
		return MaxListed
	}
	return limit
}

// ListApprovedNames returns the approved channel names, oldest approval
// first. A missing table yields an empty result.
func ListApprovedNames(ctx context.Context, db *sql.DB, limit int) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM channel_proposals WHERE status = 'approved'
		 ORDER BY reviewed_at, created_at, id LIMIT ?`, clampLimit(limit))
	if err != nil {
		if IsMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ListProposals returns proposals, newest first, optionally filtered by
// status ("" for all). A missing table yields an empty result.
func ListProposals(ctx context.Context, db *sql.DB, status string, limit int) ([]Proposal, error) {
	query := `SELECT id, name, status, created_at, reviewed_at FROM channel_proposals`
	args := []interface{}{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id LIMIT ?`
	args = append(args, clampLimit(limit))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		if IsMissingTable(err) {
			return []Proposal{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProposal returns one proposal. found is false when it does not exist or
// the table is missing.
func GetProposal(ctx context.Context, db *sql.DB, id string) (p Proposal, found bool, err error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, name, status, created_at, reviewed_at FROM channel_proposals WHERE id = ?`, id)
	p, err = scanProposal(row)
	if err == sql.ErrNoRows || IsMissingTable(err) {
		return Proposal{}, false, nil
	}
	if err != nil {
		return Proposal{}, false, err
	}
	return p, true, nil
}

type scanner interface {
	Scan(dest ...interface{}) error
}

func scanProposal(s scanner) (Proposal, error) {
	var p Proposal
	var reviewed sql.NullInt64
	if err := s.Scan(&p.ID, &p.Name, &p.Status, &p.CreatedAt, &reviewed); err != nil {
		return Proposal{}, err
	}
	if reviewed.Valid {
		v := reviewed.Int64
		p.ReviewedAt = &v
	}
	return p, nil
}
