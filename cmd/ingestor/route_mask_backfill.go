package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/meshcore-analyzer/dbschema"
	"github.com/meshcore-analyzer/packetpath"
)

// Issue #89 backfill of transmissions.route_mask for rows written before the
// column existed (route_mask IS NULL). Each row gets the bit of its stored
// route_type plus every bit readable from its surviving observations.raw_hex
// headers. That is a lower bound: a route variant whose frame no longer exists
// anywhere (e.g. overwritten twice by the observation upsert) is not invented.
//
// Sizing (issue #89 cold-cache measurement on a 4.95 GB staging copy, 496,798
// tx / 6,526,537 obs, staging-class host):
//   - building the pending index: 18-19.5 s cold, one statement. It runs here,
//     after MQTT subscribe, where IngestBuffer covers the write stall.
//   - batches of 100 tx: writer hold p99 161 ms (max 316 ms), a concurrent
//     live write waited p99 121 ms, 9.3 min in total including yields.
//
// routeMaskBackfillBatchSize and routeMaskBackfillYield are vars so tests can
// exercise multi-batch runs.
var (
	routeMaskBackfillBatchSize = 100
	routeMaskBackfillYield     = 50 * time.Millisecond
)

// StartRouteMaskBackfill schedules the backfill as an async migration. Call it
// after the MQTT subscription and ingestBuffer.Ready(): it runs next to live
// ingest and must not delay draining the startup buffer. Cancelling ctx stops
// it at the next statement; it is then recorded as failed and resumes on the
// next start.
//
// A completed migration is re-run when NULL rows exist again, which happens
// when an older ingestor (after a rollback) inserted rows without a mask.
func (s *Store) StartRouteMaskBackfill(ctx context.Context) {
	name := dbschema.RouteMaskBackfillMigration
	var hasIndex int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, dbschema.RouteMaskPendingIndex).Scan(&hasIndex)
	if hasIndex > 0 {
		var pending int
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM transmissions WHERE route_mask IS NULL)`).Scan(&pending); err == nil && pending == 1 {
			if res, err := s.db.Exec(`UPDATE _async_migrations SET status = 'pending_async' WHERE name = ? AND status = 'done'`, name); err == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					log.Printf("[route-mask] rows without route_mask found after completion; re-running %s", name)
				}
			}
		}
	}
	if err := s.RunAsyncMigration(ctx, name, s.backfillTxRouteMask); err != nil {
		log.Printf("[route-mask] scheduling %s failed: %v", name, err)
	}
}

// backfillTxRouteMask builds the pending index and then fills NULL rows in
// bounded batches until none are left.
func (s *Store) backfillTxRouteMask(ctx context.Context, d *sql.DB) error {
	start := time.Now()
	if err := createRouteMaskPendingIndex(ctx, d); err != nil {
		return err
	}
	log.Printf("[route-mask] pending index ready in %s; backfilling route_mask", time.Since(start).Round(time.Millisecond))
	var total int64
	for batches := 1; ; batches++ {
		n, err := s.backfillTxRouteMaskBatch(ctx, d, routeMaskBackfillBatchSize)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		total += n
		if batches%100 == 0 {
			log.Printf("[route-mask] backfilled %d rows so far", total)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(routeMaskBackfillYield):
		}
	}
	log.Printf("[route-mask] backfill complete: %d rows in %s", total, time.Since(start).Round(time.Second))
	return nil
}

// createRouteMaskPendingIndex builds the partial index the backfill and the
// server's status query use. It holds writerMu like every other writer, so
// the stall it causes is recorded as route_mask_index instead of showing up as
// wait time of whichever writer queues behind it on the single connection.
func createRouteMaskPendingIndex(ctx context.Context, d *sql.DB) error {
	waitStart := time.Now()
	writerMu.Lock()
	wait := time.Since(waitStart)
	holdStart := time.Now()
	defer func() {
		hold := time.Since(holdStart)
		writerMu.Unlock()
		recordWriterTiming("route_mask_index", wait, hold, dbschema.CreateRouteMaskPendingIndexSQL)
	}()
	// PREFLIGHT: async=true reason="partial index over transmissions; one full-table scan, measured 18-19.5 s cold on a 4.95 GB staging copy; runs after MQTT subscribe so IngestBuffer absorbs the write stall"
	if _, err := d.ExecContext(ctx, dbschema.CreateRouteMaskPendingIndexSQL); err != nil {
		return fmt.Errorf("create %s: %w", dbschema.RouteMaskPendingIndex, err)
	}
	return nil
}

// backfillTxRouteMaskBatch fills up to limit NULL rows in one write
// transaction and returns how many it updated. It holds writerMu like
// InsertTransmission, so a live observation is either fully visible to the
// batch (its frame is read here) or arrives after it (and its OR extends the
// now-known mask).
func (s *Store) backfillTxRouteMaskBatch(ctx context.Context, d *sql.DB, limit int) (int64, error) {
	waitStart := time.Now()
	writerMu.Lock()
	wait := time.Since(waitStart)
	holdStart := time.Now()
	defer func() {
		hold := time.Since(holdStart)
		writerMu.Unlock()
		recordWriterTiming("route_mask_backfill", wait, hold, "backfillTxRouteMaskBatch")
	}()

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after Commit

	rows, err := tx.QueryContext(ctx, `SELECT id, route_type FROM transmissions WHERE route_mask IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	masks := map[int64]int64{}
	var ids []interface{}
	for rows.Next() {
		var id int64
		var routeType sql.NullInt64
		if err := rows.Scan(&id, &routeType); err != nil {
			rows.Close()
			return 0, err
		}
		var mask int64
		if routeType.Valid {
			mask = packetpath.RouteMaskBit(int(routeType.Int64))
		}
		masks[id] = mask
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	orows, err := tx.QueryContext(ctx, `SELECT transmission_id, substr(raw_hex, 1, 2) FROM observations
		WHERE transmission_id IN (`+placeholders+`) AND raw_hex IS NOT NULL`, ids...)
	if err != nil {
		return 0, err
	}
	for orows.Next() {
		var id int64
		var header sql.NullString
		if err := orows.Scan(&id, &header); err != nil {
			orows.Close()
			return 0, err
		}
		// Unparseable or truncated frames contribute nothing; they never stop
		// the backfill.
		if rt, ok := packetpath.RouteTypeFromRawHex(header.String); ok {
			masks[id] |= packetpath.RouteMaskBit(rt)
		}
	}
	if err := orows.Err(); err != nil {
		orows.Close()
		return 0, err
	}
	orows.Close()

	stmt, err := tx.PrepareContext(ctx, `UPDATE transmissions SET route_mask = ? WHERE id = ? AND route_mask IS NULL`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var updated int64
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, masks[id.(int64)], id)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		updated += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}
