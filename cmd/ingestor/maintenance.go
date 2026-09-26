package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// pruneBatchTransmissions bounds how many transmissions (and their child
// observations) one prune transaction deletes before it commits and
// releases the writer lock.
//
// Releasing between batches is the entire point. writerMu serialises every
// wrapped writer call, so while the prune holds it the MQTT ingest path is
// blocked. Go's sync.Mutex switches to FIFO handoff once a waiter has been
// blocked for 1ms (starvation mode), so an ingest goroutine queued behind
// the prune is served at the next batch boundary instead of after the whole
// retention day.
//
// The bound is on transmissions, but hold time scales with the rows actually
// deleted, and each transmission carries an unbounded number of observations.
// At ~16 observations per transmission a batch is ~4k row deletes and a few
// hundred milliseconds; an instance with a denser observation ratio gets a
// proportionally longer hold from the same batch size. 250 also keeps the
// commit count low (a 16k-transmission day is 64 transactions, not 16k).
const pruneBatchTransmissions = 250

// pruneAgedTransmissionIDs selects the next batch of transmissions older than
// the cutoff. Both statements of a batch embed it, so they resolve the same
// set: nothing modifies `transmissions` between them inside the transaction.
//
// The ORDER BY must be satisfiable from idx_transmissions_first_seen. That
// index carries the rowid as its tiebreaker, so "first_seen, id" is walked
// straight off it and the LIMIT stays deterministic even when timestamps tie.
// Ordering by id alone looks equivalent but makes SQLite abandon the index for
// a rowid SCAN. That is harmless while rows are being deleted — the oldest
// rows have the lowest rowids and match at once — but the batch that finds
// nothing, which is the steady state whenever nothing has aged out, walks the
// whole table under writerMu. TestPruneAgedTransmissionIDsUsesFirstSeenIndex
// pins the plan.
const pruneAgedTransmissionIDs = `SELECT id FROM transmissions WHERE first_seen < ? ORDER BY first_seen, id LIMIT ?`

// The statements of one prune batch. Child rows go first (no CASCADE in
// SQLite): observations, then the batch's route_mask_changes rows (#89; via
// idx_route_mask_changes_tx), then the transmissions themselves.
const (
	pruneObservationsBatch     = `DELETE FROM observations WHERE transmission_id IN (` + pruneAgedTransmissionIDs + `)`
	pruneRouteMaskChangesBatch = `DELETE FROM route_mask_changes WHERE transmission_id IN (` + pruneAgedTransmissionIDs + `)`
	pruneTransmissionsBatch    = `DELETE FROM transmissions WHERE id IN (` + pruneAgedTransmissionIDs + `)`
)

// PruneOldPackets deletes transmissions (and their child observations)
// older than `days`. Returns count of transmissions deleted.
//
// Owned by the ingestor per #1283: the writer process is the only one
// allowed to hold the DB write lock; previously this lived in
// cmd/server/db.go and raced ingestor INSERTs (SQLITE_BUSY).
//
// Deletion is chunked into bounded transactions so ingest is never blocked
// for longer than a single batch. Deleting a whole retention day in one
// transaction held the writer lock for ~35s on a mesh doing ~260k
// observations/day, stalling MQTT ingest for the same duration.
//
// On error the transmissions deleted by already-committed batches are
// returned alongside it — those rows are gone, so reporting 0 would be
// wrong.
func (s *Store) PruneOldPackets(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	var total int64
	for {
		var batch int64
		// Tagged for writer-perf visibility (#1340).
		err := s.WriterTx("prune_packets", func(tx *sql.Tx) error {
			if _, err := tx.Exec(pruneObservationsBatch, cutoff, pruneBatchTransmissions); err != nil {
				return fmt.Errorf("prune observations: %w", err)
			}
			if _, err := tx.Exec(pruneRouteMaskChangesBatch, cutoff, pruneBatchTransmissions); err != nil {
				return fmt.Errorf("prune route_mask_changes: %w", err)
			}
			res, err := tx.Exec(pruneTransmissionsBatch, cutoff, pruneBatchTransmissions)
			if err != nil {
				return fmt.Errorf("prune transmissions: %w", err)
			}
			batch, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			return total, err
		}
		total += batch
		// A short batch proves nothing is left below the cutoff: the subquery
		// found fewer rows than it was allowed to take. Only a batch that came
		// back exactly full needs another pass.
		if batch < pruneBatchTransmissions {
			break
		}
	}
	if total > 0 {
		log.Printf("[prune] deleted %d transmissions older than %d days", total, days)
	}
	return total, nil
}

// routeMaskOrphanPruneBatch bounds one orphan-prune transaction. A var so
// tests can exercise several batches.
var routeMaskOrphanPruneBatch = 1000

// PruneOrphanRouteMaskChanges deletes route_mask_changes rows whose
// transmission no longer exists. PruneOldPackets already deletes a pruned
// transmission's rows in the same batch; this catches any other deletion path
// so the log never outlives its transmission. It never deletes a row of an
// existing transmission: the log carries no time-based expiry, so a slow
// server can never miss a change of a transmission it still holds.
//
// Each writer transaction examines the next routeMaskOrphanPruneBatch ids
// (a primary-key range, one transmissions primary-key probe per row) and
// deletes the orphans among them, so its work is bounded however far apart
// the orphans are. The table is small in practice (22 rows on a
// 496,798-transmission staging copy; at most three per transmission, one per
// route bit it can gain). Returns the number of rows deleted.
func (s *Store) PruneOrphanRouteMaskChanges() (int64, error) {
	batch := routeMaskOrphanPruneBatch
	if batch <= 0 {
		batch = 1000
	}
	var total, after int64
	for {
		var examined, deleted int64
		err := s.WriterTx("prune_route_mask_changes", func(tx *sql.Tx) error {
			var last sql.NullInt64
			if err := tx.QueryRow(`SELECT MAX(id), COUNT(*) FROM (
				SELECT id FROM route_mask_changes WHERE id > ? ORDER BY id LIMIT ?)`,
				after, batch).Scan(&last, &examined); err != nil {
				return fmt.Errorf("scan route_mask_changes: %w", err)
			}
			if examined == 0 {
				return nil
			}
			res, err := tx.Exec(`DELETE FROM route_mask_changes WHERE id > ? AND id <= ?
				AND NOT EXISTS (SELECT 1 FROM transmissions t WHERE t.id = route_mask_changes.transmission_id)`,
				after, last.Int64)
			if err != nil {
				return fmt.Errorf("prune orphan route_mask_changes: %w", err)
			}
			deleted, _ = res.RowsAffected()
			after = last.Int64
			return nil
		})
		if err != nil {
			return total, err
		}
		total += deleted
		if examined < int64(batch) {
			break
		}
	}
	if total > 0 {
		log.Printf("[prune] deleted %d route_mask_changes rows of deleted transmissions", total)
	}
	return total, nil
}

// PruneOldClientReceptions deletes mobile client-RX coverage rows older than
// `days` (by rx_at), and client_observers (companion names) whose last_seen has
// aged out. This bounds the otherwise-unbounded client_receptions table the
// opt-in coverage feature feeds. 0 disables. Owned by the ingestor writer
// (#1283). Returns the number of client_receptions rows deleted.
func (s *Store) PruneOldClientReceptions(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	var n int64
	err := s.WriterTx("prune_client_receptions", func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM client_receptions WHERE rx_at < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("prune client_receptions: %w", err)
		}
		n, _ = res.RowsAffected()
		// Drop companion name rows not refreshed within the window.
		if _, err := tx.Exec(`DELETE FROM client_observers WHERE last_seen < ?`, cutoff); err != nil {
			return fmt.Errorf("prune client_observers: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("[prune] deleted %d client_receptions older than %d days", n, days)
	}
	return n, nil
}

// SoftDeleteBlacklistedObservers marks observers in the blacklist as
// inactive=1 so they are hidden from API responses. Owned by ingestor
// per #1287. Runs once at startup.
func (s *Store) SoftDeleteBlacklistedObservers(blacklist []string) {
	n, err := dbschema.SoftDeleteBlacklistedObservers(s.db, blacklist)
	if err != nil {
		log.Printf("[observer-blacklist] warning: soft-delete failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("[observer-blacklist] soft-deleted %d blacklisted observer(s)", n)
	}
}

// PruneNeighborEdges deletes rows older than maxAgeDays from
// neighbor_edges. Owned by the ingestor per #1287 (was in cmd/server).
// Returns DB rows deleted.
func (s *Store) PruneNeighborEdges(maxAgeDays int) (int64, error) {
	if maxAgeDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(maxAgeDays) * 24 * time.Hour).Format(time.RFC3339)
	res, err := s.db.Exec("DELETE FROM neighbor_edges WHERE last_seen < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune neighbor_edges: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("[neighbor-prune] removed %d DB rows older than %d days", n, maxAgeDays)
	}
	return n, nil
}

// ─── from_pubkey backfill (#1143) ──────────────────────────────────────────
//
// Moved from cmd/server/from_pubkey_migration.go in #1287. Runs from the
// ingestor's maintenance loop. Populates transmissions.from_pubkey for
// ADVERT rows whose value is still NULL, by parsing decoded_json.pubKey.

// FromPubkeyBackfillStats holds progress for /api/healthz exposure.
// The ingestor exposes these via stats_file.go so the server can read
// them without writing.
type FromPubkeyBackfillStats struct {
	Total     int64 `json:"total"`
	Processed int64 `json:"processed"`
	Done      bool  `json:"done"`
}

// BackfillFromPubkey scans transmissions where from_pubkey IS NULL and
// payload_type = 4 (ADVERT) and populates from_pubkey from decoded_json.
// Chunked + yields between batches. Safe to call repeatedly; once a row
// is set to either "" or hex it never matches the WHERE clause again.
func (s *Store) BackfillFromPubkey(chunkSize int, yieldDuration time.Duration, progress func(total, processed int64, done bool)) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[backfill] from_pubkey panic recovered: %v", r)
		}
		if progress != nil {
			progress(0, 0, true) // signal done; values overwritten below if collected
		}
	}()
	if chunkSize <= 0 {
		chunkSize = 5000
	}

	var total int64
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM transmissions WHERE from_pubkey IS NULL AND payload_type = 4",
	).Scan(&total); err != nil {
		log.Printf("[backfill] from_pubkey count error: %v", err)
		return
	}
	if total == 0 {
		log.Println("[backfill] from_pubkey: nothing to do")
		if progress != nil {
			progress(0, 0, true)
		}
		return
	}
	if progress != nil {
		progress(total, 0, false)
	}
	log.Printf("[backfill] from_pubkey starting: %d ADVERT rows", total)

	stmt, err := s.db.Prepare("UPDATE transmissions SET from_pubkey = ? WHERE id = ?")
	if err != nil {
		log.Printf("[backfill] from_pubkey prepare: %v", err)
		return
	}
	defer stmt.Close()

	var processed int64
	for {
		rows, err := s.db.Query(
			"SELECT id, decoded_json FROM transmissions WHERE from_pubkey IS NULL AND payload_type = 4 LIMIT ?",
			chunkSize)
		if err != nil {
			log.Printf("[backfill] from_pubkey select: %v", err)
			return
		}
		type row struct {
			id int64
			pk string
		}
		batch := make([]row, 0, chunkSize)
		for rows.Next() {
			var id int64
			var dj sql.NullString
			if err := rows.Scan(&id, &dj); err != nil {
				continue
			}
			batch = append(batch, row{id: id, pk: extractPubkeyFromAdvertJSON(dj.String)})
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}

		tx, err := s.db.Begin()
		if err != nil {
			log.Printf("[backfill] from_pubkey begin tx: %v", err)
			return
		}
		txStmt := tx.Stmt(stmt)
		for _, b := range batch {
			// Sentinel: "" = scanned-no-pubkey (so the WHERE clause
			// won't keep rescanning this row). hex = real pubkey.
			var val interface{} = ""
			if b.pk != "" {
				val = b.pk
			}
			if _, err := txStmt.Exec(val, b.id); err != nil {
				log.Printf("[backfill] from_pubkey update id=%d: %v", b.id, err)
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("[backfill] from_pubkey commit: %v", err)
			return
		}
		processed += int64(len(batch))
		if progress != nil {
			progress(total, processed, false)
		}
		if len(batch) < chunkSize {
			break
		}
		if yieldDuration > 0 {
			time.Sleep(yieldDuration)
		}
	}
	log.Printf("[backfill] from_pubkey complete: %d rows processed", processed)
	if progress != nil {
		progress(total, processed, true)
	}
}

// extractPubkeyFromAdvertJSON parses an ADVERT decoded_json blob and
// returns the pubKey field, or "" if absent/invalid.
func extractPubkeyFromAdvertJSON(s string) string {
	if s == "" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return ""
	}
	if v, ok := m["pubKey"].(string); ok {
		return v
	}
	return ""
}
