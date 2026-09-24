package main

import (
	"database/sql"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/meshcore-analyzer/dbschema"
	"github.com/meshcore-analyzer/packetpath"
)

// Issue #89: transmissions.route_mask records every raw route type observed
// for a content hash (bit r = route r). The ingestor sets it on insert, ORs
// later observations into it, and backfills legacy rows asynchronously. The
// server only reads it.

// mergeRouteMask ORs a transmissions.route_mask value into the in-memory
// copy. NULL (not backfilled yet) leaves an unknown mask unknown; bits are
// never cleared. Caller holds s.mu for writing.
func (tx *StoreTx) mergeRouteMask(v sql.NullInt64) {
	if !v.Valid {
		return
	}
	tx.routeMask |= uint8(v.Int64 & packetpath.RouteMaskAll)
	tx.routeMaskKnown = true
}

// mergeRouteMaskOrQueue merges the mask of a transmission the store has just
// created and published in s.byTxID and, while that mask is still NULL,
// queues it so RefreshBackfilledRouteMasks reads it once the ingestor has
// backfilled it. Caller holds s.mu for writing.
func (s *PacketStore) mergeRouteMaskOrQueue(tx *StoreTx, v sql.NullInt64) {
	tx.mergeRouteMask(v)
	s.queueUnknownRouteMask(tx)
}

// queueUnknownRouteMask queues tx if its mask is still unknown. The tx must
// already be in s.byTxID: a refresh drops queued ids it cannot find there.
func (s *PacketStore) queueUnknownRouteMask(tx *StoreTx) {
	if tx.routeMaskKnown || s.db == nil || !s.db.hasRouteMask() {
		return
	}
	s.routeMaskInboxMu.Lock()
	s.routeMaskInbox = append(s.routeMaskInbox, tx.ID)
	s.routeMaskInboxMu.Unlock()
	s.routeMaskQueuedCount.Add(1)
}

// routeMaskPendingLen is an upper bound on the queued transmissions whose
// backfilled mask this server has not read yet (it still counts evicted or
// deleted ids until a refresh reaches them).
func (s *PacketStore) routeMaskPendingLen() int {
	return int(s.routeMaskQueuedCount.Load())
}

// routeMaskRefreshLimit and routeMaskRefreshChunk bound one refresh: at most
// limit ids per poller tick, read in IN-lists of chunk ids. 2,000 ids per
// one-second tick stays ahead of the backfill (about 500 rows/s measured on a
// staging copy).
const (
	routeMaskRefreshLimit = 2000
	routeMaskRefreshChunk = 500
)

// RefreshBackfilledRouteMasks reads the route_mask of queued transmissions
// (loaded while their mask was NULL) and merges every value the ingestor's
// backfill has written since. It returns how many in-memory masks changed.
//
// The backfill fills NULL rows in ascending id order, so the queue is kept
// sorted and resolved up to the first id that is still NULL; that id and all
// later ones wait for a later tick. Ids whose row is gone (retention) or
// whose transmission was evicted are dropped. Read-only; the poller calls it
// on every tick.
func (s *PacketStore) RefreshBackfilledRouteMasks() int {
	if s.db == nil || s.db.conn == nil || !s.db.hasRouteMask() {
		return 0
	}
	s.routeMaskRefreshMu.Lock()
	defer s.routeMaskRefreshMu.Unlock()

	s.routeMaskInboxMu.Lock()
	inbox := s.routeMaskInbox
	s.routeMaskInbox = nil
	s.routeMaskInboxMu.Unlock()
	if len(inbox) > 0 {
		// Sorted here, outside s.mu and the inbox lock that load paths take.
		s.routeMaskQueue = append(s.routeMaskQueue, inbox...)
		sort.Ints(s.routeMaskQueue)
	}
	ids := s.routeMaskQueue
	if len(ids) > routeMaskRefreshLimit {
		ids = ids[:routeMaskRefreshLimit]
	}
	if len(ids) == 0 {
		return 0
	}

	masks := make(map[int]sql.NullInt64, len(ids))
	resolved := 0
	for start := 0; start < len(ids) && resolved == start; start += routeMaskRefreshChunk {
		end := start + routeMaskRefreshChunk
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.db.readRouteMasks(ids[start:end], masks); err != nil {
			log.Printf("[route-mask] refresh read failed: %v", err)
			break
		}
		for _, id := range ids[start:end] {
			if v, ok := masks[id]; ok && !v.Valid {
				break
			}
			resolved++
		}
	}
	if resolved == 0 {
		return 0
	}

	changed := 0
	s.mu.Lock()
	for _, id := range ids[:resolved] {
		v, ok := masks[id]
		tx := s.byTxID[id]
		if !ok || tx == nil {
			continue
		}
		known, before := tx.routeMaskKnown, tx.routeMask
		tx.mergeRouteMask(v)
		if !known || tx.routeMask != before {
			changed++
		}
	}
	s.mu.Unlock()

	s.routeMaskQueue = s.routeMaskQueue[resolved:]
	if len(s.routeMaskQueue) == 0 {
		s.routeMaskQueue = nil
	}
	s.routeMaskQueuedCount.Add(-int64(resolved))

	if changed > 0 {
		// Relay Airtime Share lives in rfCache.
		s.invalidateCachesFor(cacheInvalidation{hasNewObservations: true})
	}
	return changed
}

// readRouteMasks adds route_mask for each existing transmission in ids to
// out; ids without a row are left out.
func (db *DB) readRouteMasks(ids []int, out map[int]sql.NullInt64) error {
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.conn.Query(`SELECT id, route_mask FROM transmissions WHERE id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var mask sql.NullInt64
		if err := rows.Scan(&id, &mask); err != nil {
			return err
		}
		out[id] = mask
	}
	return rows.Err()
}

// routeMaskBackfillStatus is the database status, except that "complete"
// becomes "backfilling" while this server still holds transmissions whose
// backfilled mask it has not read yet (remaining = how many).
func (s *PacketStore) routeMaskBackfillStatus() RouteMaskBackfillStatus {
	st := s.db.routeMaskBackfillStatus()
	if st.Status == "complete" {
		if n := int64(s.routeMaskPendingLen()); n > 0 {
			return RouteMaskBackfillStatus{Status: "backfilling", Remaining: &n}
		}
	}
	return st
}

// RouteMaskBackfillStatus is the read-only view of the ingestor's route_mask
// backfill, reported on /api/healthz and the Relay Airtime Share response.
//   - pending: the column or the pending index does not exist yet, or rows
//     still lack a mask while the backfill is not running
//   - backfilling: rows lack a mask and the async migration is running
//   - complete: no row lacks a mask
//
// Remaining is the number of rows without a mask, or null when it cannot be
// counted cheaply (no pending index yet).
type RouteMaskBackfillStatus struct {
	Status    string `json:"status"`
	Remaining *int64 `json:"remaining"`
}

// routeMaskStatusTTL bounds how often the status queries run; the partial
// index keeps them cheap (EXISTS 0.2-0.5 ms, COUNT 53-90 ms cold on a
// 496,798-transmission staging copy), but health endpoints are polled.
var routeMaskStatusTTL = 30 * time.Second

func (db *DB) routeMaskBackfillStatus() RouteMaskBackfillStatus {
	db.routeMaskStatusMu.Lock()
	defer db.routeMaskStatusMu.Unlock()
	if time.Now().Before(db.routeMaskStatusExp) {
		return db.routeMaskStatus
	}
	db.routeMaskStatus = db.computeRouteMaskBackfillStatus()
	db.routeMaskStatusExp = time.Now().Add(routeMaskStatusTTL)
	return db.routeMaskStatus
}

// computeRouteMaskBackfillStatus decides the status from the database itself,
// never from a flag that could claim completion early: "complete" requires
// that no transmission has route_mask IS NULL.
func (db *DB) computeRouteMaskBackfillStatus() RouteMaskBackfillStatus {
	pending := RouteMaskBackfillStatus{Status: "pending"}
	if !db.hasRouteMask() {
		return pending
	}
	var hasIndex int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
		dbschema.RouteMaskPendingIndex).Scan(&hasIndex); err != nil || hasIndex == 0 {
		return pending
	}
	var remaining int64
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_mask IS NULL`).Scan(&remaining); err != nil {
		return pending
	}
	if remaining == 0 {
		return RouteMaskBackfillStatus{Status: "complete", Remaining: &remaining}
	}
	var asyncStatus string
	db.conn.QueryRow(`SELECT status FROM _async_migrations WHERE name = ?`, dbschema.RouteMaskBackfillMigration).Scan(&asyncStatus)
	if asyncStatus == "pending_async" {
		return RouteMaskBackfillStatus{Status: "backfilling", Remaining: &remaining}
	}
	return RouteMaskBackfillStatus{Status: "pending", Remaining: &remaining}
}
