package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// Opt-in retention for the tables nothing else prunes (#329): inactive_nodes,
// node_changes and soft-deleted observers. Each window is in days and 0
// disables it, so an instance that sets none keeps every row, as before. Like
// pruneBatches, every delete runs in bounded WriterTx batches, so ingest waits
// for at most one batch. ping_triggers is deliberately left out and kept
// forever: the all-time Ping Scores records join it with the history sidecar
// (#349, #241), and an entry leaves them once its trigger is gone.

// retentionBatchRows bounds the rows one table-retention transaction deletes.
// These rows are small and their indexes few, so a batch is a few
// milliseconds. A var so tests can exercise several batches.
var retentionBatchRows = 1000

// deleteInBatches runs del, a DELETE whose last parameter is the batch LIMIT,
// in WriterTx batches tagged tag until one deletes fewer rows than the limit.
// args are del's parameters before the LIMIT. On error the rows of the
// already-committed batches are returned alongside it.
func (s *Store) deleteInBatches(tag, del string, args ...any) (int64, error) {
	params := append(append(make([]any, 0, len(args)+1), args...), retentionBatchRows)
	var total int64
	for {
		var n int64
		err := s.WriterTx(tag, func(tx *sql.Tx) error {
			res, err := tx.Exec(del, params...)
			if err != nil {
				return err
			}
			n, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("%s: %w", tag, err)
		}
		total += n
		if n < int64(retentionBatchRows) {
			return total, nil
		}
	}
}

func retentionCutoff(days int) string {
	return time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
}

// pruneInactiveNodesBatch deletes the next batch of inactive_nodes rows whose
// last advert is older than the cutoff (?1). It skips a node that came back:
// its old row stays next to the new nodes row (MoveStaleNodes does not delete
// it), still carries the node's confirmed default_scope evidence
// (UpdateNodeDefaultScope) and keeps the node out of New Nodes. It also skips
// the node of an observer seen since the cutoff, which MoveStaleNodes would
// not retire (#199; see staleNodesWhere). The walk is over
// idx_inactive_nodes_last_seen; the rows it steps over are bounded by the
// nodes and observers tables.
const pruneInactiveNodesBatch = `DELETE FROM inactive_nodes WHERE public_key IN (
	SELECT i.public_key FROM inactive_nodes i
	WHERE i.last_seen < ?1
	  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.public_key = i.public_key)
	  AND lower(i.public_key) NOT IN (
	      SELECT lower(id) FROM observers WHERE id IS NOT NULL AND last_seen >= ?1)
	LIMIT ?2)`

// PruneInactiveNodes deletes inactive_nodes rows not seen for days
// (retention.inactiveNodeDays, #329); days <= 0 disables it. A deleted node
// that adverts again later is a new node: no "resurrected" node_changes row,
// and it shows in New Nodes.
func (s *Store) PruneInactiveNodes(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	n, err := s.deleteInBatches("prune_inactive_nodes", pruneInactiveNodesBatch, retentionCutoff(days))
	if n > 0 {
		log.Printf("[prune] deleted %d inactive_nodes not seen in %d days", n, days)
	}
	return n, err
}

// PruneNodeChanges deletes node_changes rows detected more than days ago
// (retention.nodeChangeDays, #329), oldest first over
// idx_node_changes_detected_at; days <= 0 disables it.
func (s *Store) PruneNodeChanges(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	n, err := s.deleteInBatches("prune_node_changes",
		`DELETE FROM node_changes WHERE id IN (
			SELECT id FROM node_changes WHERE detected_at < ? ORDER BY detected_at LIMIT ?)`,
		retentionCutoff(days))
	if n > 0 {
		log.Printf("[prune] deleted %d node_changes older than %d days", n, days)
	}
	return n, err
}

// purgeObserverCandidates selects the next batch of observers to hard-delete:
// soft-deleted (inactive = 1) by RemoveStaleObservers, not seen since the
// cutoff (?1), and referenced by no row that outlives them by design.
//
// observations.observer_idx is a bare rowid with no foreign key, so deleting a
// still-referenced observer silently orphans history: packets_v stops
// resolving the observer and the packets are mis-attributed. observer_metrics,
// observer_neighbor_metrics and dropped_packets are keyed by observer id and
// age out by metricsDays. These four guards are what make the delete safe;
// each is an index seek per candidate (observers is O(100) rows), so a batch
// stays cheap. observer_neighbors is not a guard: it is a current-only
// snapshot that only the observer itself replaces, so it would keep a dead
// observer forever. PurgeStaleObservers deletes it with the observer instead.
const purgeObserverCandidates = `SELECT id FROM observers
	WHERE inactive = 1
	  AND last_seen < ?1
	  AND id IS NOT NULL
	  AND NOT EXISTS (SELECT 1 FROM observations o WHERE o.observer_idx = observers.rowid)
	  AND NOT EXISTS (SELECT 1 FROM observer_metrics m WHERE m.observer_id = observers.id)
	  AND NOT EXISTS (SELECT 1 FROM observer_neighbor_metrics nm WHERE nm.observer_id = observers.id)
	  AND NOT EXISTS (SELECT 1 FROM dropped_packets d WHERE d.observer_id = observers.id)
	LIMIT ?2`

// PurgeStaleObservers hard-deletes observers RemoveStaleObservers already
// soft-deleted, once they are older than purgeDays and nothing references them
// any more (retention.observerPurgeDays; upstream#1886, #329). It is the
// second stage of observer retention: the soft-delete hides the observer, this
// reclaims the row after its packets and metrics have aged out. purgeDays <= 0
// disables it (the default).
//
// Each batch reads its candidates once and deletes exactly those ids, with
// their observer_neighbors rows, in the same transaction.
func (s *Store) PurgeStaleObservers(purgeDays int) (int64, error) {
	if purgeDays <= 0 {
		return 0, nil
	}
	cutoff := retentionCutoff(purgeDays)
	var total int64
	for {
		var n int
		// Tagged for /api/perf writer-lock visibility (#1340).
		err := s.WriterTx("purge_observers", func(tx *sql.Tx) error {
			ids, err := queryStrings(tx, purgeObserverCandidates, cutoff, retentionBatchRows)
			if err != nil {
				return fmt.Errorf("select observers: %w", err)
			}
			n = len(ids)
			if n == 0 {
				return nil
			}
			in := "?" + strings.Repeat(",?", n-1)
			args := make([]any, n)
			for i, id := range ids {
				args[i] = id
			}
			if _, err := tx.Exec(`DELETE FROM observer_neighbors WHERE observer_id IN (`+in+`)`, args...); err != nil {
				return fmt.Errorf("purge observer_neighbors: %w", err)
			}
			if _, err := tx.Exec(`DELETE FROM observers WHERE id IN (`+in+`)`, args...); err != nil {
				return fmt.Errorf("purge observers: %w", err)
			}
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("purge stale observers: %w", err)
		}
		total += int64(n)
		if n < retentionBatchRows {
			break
		}
	}
	if total > 0 {
		log.Printf("[prune] purged %d inactive observer(s) with no remaining data (not seen in %d days)", total, purgeDays)
	}
	return total, nil
}

// queryStrings returns the single string column of q's rows.
func queryStrings(tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// runTableRetention runs one pass of the opt-in table retention (#329). when
// names the pass in the log ("startup", "daily"). The observer purge runs
// last, after the caller's RemoveStaleObservers, so a row crossing both
// thresholds is finalised in one pass. Each prune logs its own count; an
// error is logged and does not stop the others.
func runTableRetention(store *Store, r TableRetention, when string) {
	for _, p := range []struct {
		days  int
		prune func(int) (int64, error)
	}{
		{r.InactiveNodeDays, store.PruneInactiveNodes},
		{r.NodeChangeDays, store.PruneNodeChanges},
		{r.ObserverPurgeDays, store.PurgeStaleObservers},
	} {
		if _, err := p.prune(p.days); err != nil {
			log.Printf("[prune] %s table retention error: %v", when, err)
		}
	}
}
