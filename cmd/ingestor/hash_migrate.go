package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// Content-hash migration (#215).
//
// transmissions.hash is the content hash of the raw packet, and the formula
// has changed over the project's life (#786, #787). Rows written before a
// change carry a stale hash. They are rehashed here, in the process that owns
// the DB, once: this runs as the async migration below and is recorded in
// _async_migrations, so it does not repeat at the next start. It used to run in
// cmd/server on the server's mode=ro handle, where every UPDATE and DELETE
// failed, was logged as a collision, and was retried at every start.
//
// New rows are written with the current formula (InsertTransmission hashes with
// ComputeContentHash), so only rows older than the formula can be stale.
//
// Rehashing can make two rows collide: the same packet stored under two stale
// hashes, or a stale row and a row already written with the current hash.
// transmissions.hash is UNIQUE, so the rows are merged instead:
//
//   - the row with the LOWEST ID survives. cmd/server's in-memory migration
//     applies the same rule, so a server that has not restarted yet and one
//     that has agree on which row stays;
//   - the duplicate's observations move to it. An observation the survivor
//     already has (same observer and path, which idx_observations_dedup
//     rejects) is dropped instead;
//   - last_seen becomes the later of the two, route_mask the union, and
//     first_seen stays the survivor's. A grown route_mask is logged in
//     route_mask_changes for running servers;
//   - rows hung off the duplicate follow the survivor (ping_triggers), or go
//     with it when the survivor already has one (route_mask_changes);
//   - the duplicate row is deleted.
//
// If the formula changes again, bump the migration name: a recorded migration
// is never re-run.
const contentHashMigration = "content_hash_formula_v1"

// StartContentHashMigration schedules the migration. Like the route_mask
// backfill it is started by main once the ingest buffer is draining, not by
// OpenStore: it scans the whole of transmissions and writes in batches, which
// the buffer absorbs, and OpenStore callers (tests, tools) that seed their own
// rows must not have them rehashed under them. Cancelled on shutdown; a
// migration that did not finish resumes at the next start.
func (s *Store) StartContentHashMigration(ctx context.Context) {
	// PREFLIGHT: async=true reason="full-table scan of transmissions.raw_hex (hash recompute) with batched writes; must not block ingestor boot"
	if err := s.RunAsyncMigration(ctx, contentHashMigration, s.migrateContentHashes); err != nil {
		log.Printf("[hash-migrate] scheduling %s failed: %v", contentHashMigration, err)
	}
}

// Package vars, not consts, so tests can drive the multi-batch loop with a few
// rows.
var (
	contentHashMigrationBatchSize = 2000
	contentHashMigrationYield     = 20 * time.Millisecond

	// contentHashMigrationHook is a test seam, nil in production. It is called
	// after a batch was scanned and before it is rewritten ("scanned"), and
	// after the rewrite ("batch"); n is the number of rows scanned so far.
	contentHashMigrationHook func(stage string, n int)
)

type staleContentHash struct {
	id      int64
	oldHash string
	newHash string
}

// migrateContentHashes is the content_hash_formula_v1 async migration body.
// Each batch is read, then rewritten in one transaction under writerMu like
// every other writer, so live inserts queue behind a batch rather than the
// whole migration (the same shape as backfillTxRouteMask).
func (s *Store) migrateContentHashes(ctx context.Context, d *sql.DB) error {
	start := time.Now()
	ex := hashMigrationTables{
		pingTriggers:     tableExists(d, "ping_triggers"),
		routeMaskChanges: tableExists(d, "route_mask_changes"),
	}
	var scanned, rehashed, merged int64
	var lastID int64
	for {
		stale, next, n, err := scanStaleContentHashes(ctx, d, lastID, contentHashMigrationBatchSize)
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if n == 0 {
			break
		}
		scanned += int64(n)
		lastID = next
		if h := contentHashMigrationHook; h != nil {
			h("scanned", int(scanned))
		}
		if len(stale) > 0 {
			r, m, err := s.rewriteContentHashes(ctx, d, stale, ex)
			if err != nil {
				return fmt.Errorf("rewrite: %w", err)
			}
			rehashed += r
			merged += m
		}
		if h := contentHashMigrationHook; h != nil {
			h("batch", int(scanned))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(contentHashMigrationYield):
		}
	}
	if rehashed > 0 || merged > 0 {
		log.Printf("[hash-migrate] rehashed %d of %d transmissions to the current formula, merged %d duplicates, in %s",
			rehashed, scanned, merged, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// tableExists reports whether a table is present. Both optional tables are
// created by dbschema.Apply before this runs; the check keeps the migration
// from failing on a DB that predates them.
func tableExists(d *sql.DB, name string) bool {
	var n int
	return d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n) == nil && n > 0
}

type hashMigrationTables struct {
	pingTriggers     bool
	routeMaskChanges bool
}

// scanStaleContentHashes reads the next batch of rows after afterID and returns
// those whose hash differs from the current formula, the last id scanned, and
// how many rows it scanned (0 at the end of the table).
func scanStaleContentHashes(ctx context.Context, d *sql.DB, afterID int64, limit int) ([]staleContentHash, int64, int, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, raw_hex, hash FROM transmissions WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, afterID, 0, err
	}
	defer rows.Close()
	var stale []staleContentHash
	last := afterID
	n := 0
	for rows.Next() {
		var id int64
		var raw, hash string
		if err := rows.Scan(&id, &raw, &hash); err != nil {
			return nil, afterID, 0, err
		}
		last = id
		n++
		if raw == "" {
			continue
		}
		if want := ComputeContentHash(raw); want != hash {
			stale = append(stale, staleContentHash{id: id, oldHash: hash, newHash: want})
		}
	}
	return stale, last, n, rows.Err()
}

// rewriteContentHashes applies one batch in a single transaction.
func (s *Store) rewriteContentHashes(ctx context.Context, d *sql.DB, stale []staleContentHash, ex hashMigrationTables) (rehashed, merged int64, err error) {
	waitStart := time.Now()
	writerMu.Lock()
	wait := time.Since(waitStart)
	holdStart := time.Now()
	defer func() {
		hold := time.Since(holdStart)
		writerMu.Unlock()
		recordWriterTiming("content_hash_migration", wait, hold, "rewriteContentHashes")
	}()

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() // no-op after Commit

	for _, u := range stale {
		// The scan ran before writerMu was taken: retention may have removed
		// the row since.
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT hash FROM transmissions WHERE id = ?`, u.id).Scan(&current); err == sql.ErrNoRows {
			continue
		} else if err != nil {
			return 0, 0, err
		}
		if current != u.oldHash {
			continue
		}
		var holder int64
		finalID := u.id
		err := tx.QueryRowContext(ctx, `SELECT id FROM transmissions WHERE hash = ?`, u.newHash).Scan(&holder)
		switch {
		case err == sql.ErrNoRows:
			// No collision: the hash is simply rewritten.
		case err != nil:
			return 0, 0, err
		default:
			winner, loser := u.id, holder
			if holder < u.id {
				winner, loser = holder, u.id
			}
			if err := mergeTransmissions(ctx, tx, winner, loser, ex); err != nil {
				return 0, 0, fmt.Errorf("merge tx %d into %d: %w", loser, winner, err)
			}
			merged++
			finalID = winner
		}
		if finalID == u.id {
			// The survivor already holds the new hash when it is the holder;
			// otherwise the duplicate holding it is gone and the hash is free.
			if _, err := tx.ExecContext(ctx, `UPDATE transmissions SET hash = ? WHERE id = ?`, u.newHash, u.id); err != nil {
				return 0, 0, err
			}
		}
		if ex.pingTriggers {
			// ping_triggers keeps a copy of the hash.
			if _, err := tx.ExecContext(ctx, `UPDATE ping_triggers SET hash = ? WHERE tx_id = ?`, u.newHash, finalID); err != nil {
				return 0, 0, err
			}
		}
		rehashed++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return rehashed, merged, nil
}

// mergeTransmissions folds loser into winner and deletes loser. See the file
// comment for the rules. The winner's hash is not touched here: when the loser
// holds the new hash, deleting it frees the hash for the caller's UPDATE.
func mergeTransmissions(ctx context.Context, tx *sql.Tx, winner, loser int64, ex hashMigrationTables) error {
	// OR IGNORE: an observation the survivor already has violates
	// idx_observations_dedup; it stays behind and is deleted just below.
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE observations SET transmission_id = ? WHERE transmission_id = ?`, winner, loser); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM observations WHERE transmission_id = ?`, loser); err != nil {
		return err
	}

	var oldMask sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT route_mask FROM transmissions WHERE id = ?`, winner).Scan(&oldMask); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE transmissions SET
			last_seen = MAX(last_seen, (SELECT last_seen FROM transmissions WHERE id = ?)),
			route_mask = CASE WHEN route_mask IS NULL AND (SELECT route_mask FROM transmissions WHERE id = ?) IS NULL THEN NULL
				ELSE COALESCE(route_mask, 0) | COALESCE((SELECT route_mask FROM transmissions WHERE id = ?), 0) END
		WHERE id = ?`, loser, loser, loser, winner); err != nil {
		return err
	}

	if ex.pingTriggers {
		// A ping is one row per transmission: keep the survivor's if it has one,
		// else it takes over the duplicate's.
		if _, err := tx.ExecContext(ctx, `DELETE FROM ping_triggers WHERE tx_id = ? AND EXISTS (SELECT 1 FROM ping_triggers WHERE tx_id = ?)`, loser, winner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ping_triggers SET tx_id = ? WHERE tx_id = ?`, winner, loser); err != nil {
			return err
		}
	}
	if ex.routeMaskChanges {
		if _, err := tx.ExecContext(ctx, `DELETE FROM route_mask_changes WHERE transmission_id = ?`, loser); err != nil {
			return err
		}
		var newMask sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT route_mask FROM transmissions WHERE id = ?`, winner).Scan(&newMask); err != nil {
			return err
		}
		if newMask.Valid && (!oldMask.Valid || newMask.Int64 != oldMask.Int64) {
			// Same record InsertTransmission leaves when an observation adds a
			// route bit: servers that hold the survivor pick up the full mask.
			if _, err := tx.ExecContext(ctx, `INSERT INTO route_mask_changes (transmission_id, route_mask, created_at) VALUES (?, ?, ?)`,
				winner, newMask.Int64, time.Now().Unix()); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM transmissions WHERE id = ?`, loser)
	return err
}
