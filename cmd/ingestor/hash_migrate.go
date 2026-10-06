package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/meshcore-analyzer/packetpath"
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
//     already has (same observer and path, which idx_observations_dedup rejects)
//     is dropped instead, and the survivor's copy wins, as it does when ingest
//     meets a repeat reception. The earlier copy could win only by copying every
//     observation column, the optional ones (resolved_path, raw_hex) included,
//     over the survivor's row, and by the same in the server's in-memory
//     observation; what is lost is the dropped copy's SNR, RSSI and time (same
//     observer, same path), while the transmission keeps the
//     earliest first_seen;
//   - the survivor takes the earliest first_seen, the later last_seen and the
//     route_mask union (a grown mask is logged in route_mask_changes for running
//     servers). A NULL route_mask is "not computed yet", not "no routes": when
//     either side is NULL the merge cannot take a plain OR, so it recomputes the
//     mask inline (#287) from both sides' stored masks (keeping a bit no
//     surviving observation can rebuild) plus the #89 lower bound the backfill
//     would produce (route_type and the surviving observation headers). The
//     result is non-NULL, so the backfill leaves it alone and the change is
//     logged like any grown mask, except that a survivor whose mask was NULL
//     always logs one row, even for a known 0 (one per both-NULL merge on a
//     pre-#89 DB); and every nullable column it has no value for
//     (scope_name, channel_hash, from_pubkey, ...) from the duplicate; a value
//     it has stays;
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
	fill, err := fillableColumns(d)
	if err != nil {
		return fmt.Errorf("columns: %w", err)
	}
	ex.fillCols = fill
	var scanned, rehashed, merged int64
	var lastID int64
	for {
		// A cancel (shutdown) is an error, not a normal end: the run must be
		// recorded as unfinished so the next start resumes it.
		if err := ctx.Err(); err != nil {
			return err
		}
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
	// fillCols are the columns of fillColumns the table has: a merge fills them
	// from the duplicate when the survivor's value is NULL.
	fillCols []string
}

// fillColumns are the nullable columns of transmissions a merge fills with
// COALESCE: the ones that describe the packet and are NULL only because the
// row that was ingested first did not have the value (an older schema, a copy
// that was not transport-scoped). It is an allow-list on purpose: a nullable
// column added later may use NULL for "pending" (a backfill that has not run
// yet), and filling it from a duplicate would pass that off as a value. A new
// column that should be filled is added here, with a test.
var fillColumns = []string{
	"route_type", "payload_type", "payload_version", "decoded_json",
	"from_pubkey", "channel_hash", "scope_name",
}

// fillableColumns returns the columns of fillColumns that transmissions has, in
// the order of fillColumns: an older schema lacks some of them.
func fillableColumns(d *sql.DB) ([]string, error) {
	rows, err := d.Query(`SELECT name FROM pragma_table_info('transmissions')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var cols []string
	for _, c := range fillColumns {
		if have[c] {
			cols = append(cols, c)
		}
	}
	return cols, nil
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

	var oldMask, loserMask sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT route_mask FROM transmissions WHERE id = ?`, winner).Scan(&oldMask); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT route_mask FROM transmissions WHERE id = ?`, loser).Scan(&loserMask); err != nil {
		return err
	}
	// A NULL route_mask means "not computed yet", not "no routes".
	var mergedMask int64
	if oldMask.Valid && loserMask.Valid {
		// Both computed: each stored mask already encodes that side's route_type
		// and every observation it ever held, so their OR is the exact union.
		mergedMask = oldMask.Int64 | loserMask.Int64
	} else {
		// At least one side was never computed. Leaving the result non-NULL via
		// COALESCE(route_mask, 0) would make the backfill skip the row and lose
		// the uncomputed side's bits for good (#287). Setting it back to NULL and
		// deferring to the backfill instead would lose a bit the computed side
		// stored but no surviving observation can rebuild (an observation
		// idx_observations_dedup dropped in the move above, or a frame a later
		// reception overwrote). So recompute inline, in this transaction: keep
		// every bit either side stored, then OR the #89 lower bound the backfill
		// would produce (both sides' route_type plus the surviving observation
		// headers). The result is non-NULL, so the backfill leaves the row alone
		// and the route_mask_changes row below tells running servers.
		m, err := recomputeMergedRouteMask(ctx, tx, winner, loser)
		if err != nil {
			return err
		}
		if oldMask.Valid {
			m |= oldMask.Int64
		}
		if loserMask.Valid {
			m |= loserMask.Int64
		}
		mergedMask = m
	}
	set := "first_seen = MIN(first_seen, (SELECT first_seen FROM transmissions WHERE id = ?)),\n" +
		"last_seen = MAX(last_seen, (SELECT last_seen FROM transmissions WHERE id = ?)),\n" +
		"route_mask = ?"
	args := []interface{}{loser, loser, mergedMask}
	for _, col := range ex.fillCols {
		set += fmt.Sprintf(",\n\"%s\" = COALESCE(\"%s\", (SELECT \"%s\" FROM transmissions WHERE id = ?))", col, col, col)
		args = append(args, loser)
	}
	args = append(args, winner)
	if _, err := tx.ExecContext(ctx, "UPDATE transmissions SET "+set+" WHERE id = ?", args...); err != nil {
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
		if !oldMask.Valid || mergedMask != oldMask.Int64 {
			// Same record InsertTransmission leaves when an observation adds a
			// route bit: servers that hold the survivor pick up the full mask.
			// The merged mask is always known now, so an uncomputed survivor
			// (oldMask NULL) that the merge gives a mask is announced too.
			if _, err := tx.ExecContext(ctx, `INSERT INTO route_mask_changes (transmission_id, route_mask, created_at) VALUES (?, ?, ?)`,
				winner, mergedMask, time.Now().Unix()); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM transmissions WHERE id = ?`, loser)
	return err
}

// recomputeMergedRouteMask rebuilds the #89 route-mask lower bound for a merged
// transmission whose mask cannot be a plain OR because a side was never
// computed. It mirrors backfillTxRouteMaskBatch for the one merged row: the bit
// of each side's stored route_type (the survivor keeps its own route_type or
// takes the loser's, and a route the survivor was never heard on still matters,
// so both are ORed) plus the header bit of every observation now parented on the
// winner (the move above already folded in the loser's surviving observations).
// The caller ORs in both stored masks, so a bit either side recorded from a
// frame that no surviving observation can rebuild is kept too. Unlike the
// backfill it runs once per merge, so a per-row read is fine.
func recomputeMergedRouteMask(ctx context.Context, tx *sql.Tx, winner, loser int64) (int64, error) {
	var mask int64
	for _, id := range [...]int64{winner, loser} {
		var routeType sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT route_type FROM transmissions WHERE id = ?`, id).Scan(&routeType); err != nil {
			return 0, err
		}
		if routeType.Valid {
			mask |= packetpath.RouteMaskBit(int(routeType.Int64))
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT substr(raw_hex, 1, 2) FROM observations WHERE transmission_id = ? AND raw_hex IS NOT NULL`, winner)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var header sql.NullString
		if err := rows.Scan(&header); err != nil {
			return 0, err
		}
		mask |= routeMaskBitFromHeader(header.String)
	}
	return mask, rows.Err()
}
