package main

import (
	"database/sql"
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

// mergeObservationRoute ORs the route bit of a newly ingested observation's
// frame into a known mask. The ingestor ORs the same bit into the database
// right after inserting the observation, so a poll that lands in between
// would otherwise miss it. Unknown (legacy NULL) masks stay unknown so the
// live view never claims more than a cold load of the same database.
func (tx *StoreTx) mergeObservationRoute(rawHex string) {
	if !tx.routeMaskKnown {
		return
	}
	if rt, ok := packetpath.RouteTypeFromRawHex(rawHex); ok {
		tx.routeMask |= uint8(packetpath.RouteMaskBit(rt))
	}
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
