// Package main: "Node Changes" audit log -- second piece of dborup's
// requested network-changes tooling (2026-07-29), after New Nodes
// (new_nodes.go). Reads node_changes, written by cmd/ingestor's
// detectAndLogNodeChange as ADVERTs arrive (role/name/position changes,
// plus "returned after being pruned to inactive_nodes"). A dedicated
// audit table rather than a periodic snapshot diff, so nothing is missed
// between polls.
package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// NodeChangeEntry is one row in the node-change audit log.
type NodeChangeEntry struct {
	ID        int64  `json:"id"`
	PublicKey string `json:"publicKey"`
	// Name is the node's CURRENT display name (resolved via a bulk nodes
	// lookup, same as ping_scores.go's relay-name resolution) -- not a
	// historical snapshot at the time of the change.
	Name string `json:"name,omitempty"`
	// Role is the node's CURRENT role (same resolved-live convention as
	// Name), for the Tools > Node Changes role filter.
	Role string `json:"role,omitempty"`
	// Foreign mirrors nodes.foreign_advert for the node's CURRENT state
	// (node_changes rows don't store their own snapshot of it) -- same
	// All/Domestic/Foreign vocabulary as Tools > New Nodes' toggle. Not
	// omitempty: false is a real, meaningful value here (domestic).
	Foreign    bool   `json:"foreign"`
	ChangeType string `json:"changeType"` // "role" | "name" | "position" | "resurrected"
	OldValue   string `json:"oldValue,omitempty"`
	NewValue   string `json:"newValue,omitempty"`
	DetectedAt string `json:"detectedAt"`
	// DistanceKm is set only for changeType=="position" -- parsed from
	// the "lat,lon" old/new value strings cmd/ingestor writes.
	DistanceKm *float64 `json:"distanceKm,omitempty"`
}

// nodeChangesSQLFetchCap bounds how many rows are ever pulled from SQL
// before identity filtering (identityHidden) and truncating to the caller's
// limit in Go -- filtering after a tight SQL LIMIT would silently undercount.
const nodeChangesSQLFetchCap = 500

// computeNodeChanges returns the most recent node_changes rows, newest
// first, capped at limit (clamped to [1, nodeChangesSQLFetchCap]).
func (s *Server) computeNodeChanges(limit int) ([]NodeChangeEntry, error) {
	entries, _, _, err := s.fetchNodeChanges(limit)
	return entries, err
}

// fetchNodeChanges is computeNodeChanges plus what SQL returned before
// identity filtering: scanned is the number of rows read (up to the point
// the loop stopped at limit; with limit = nodeChangesSQLFetchCap that is
// every row SQL returned) and oldestScanned the detected_at of the last
// row read. The Network Digest needs both to
// tell whether the SQL LIMIT cut off rows -- the filtered list alone
// undercounts when hidden rows were dropped (#1015, same as #1009 F1).
//
// Visibility follows the node list's identity rule (#1015): a row is left
// out when identityHidden holds for its pubkey with the node's CURRENT name
// (node blacklist, observer blacklist, hidden name prefix), and a "name" row
// is also left out when its old or new value is a hidden name -- the rename
// that hid a node, or that later un-hid it, would otherwise publish the
// hidden name. Config is read live on every call (there is no response
// cache), so a blacklist or prefix change applies to the next request.
func (s *Server) fetchNodeChanges(limit int) (out []NodeChangeEntry, scanned int, oldestScanned string, err error) {
	if limit <= 0 || limit > nodeChangesSQLFetchCap {
		limit = nodeChangesSQLFetchCap
	}
	// The current name and role come from the same query (primary-key join),
	// so the hidden-name check runs per row before the limit, with no second
	// lookup that could fail on its own and let a hidden node through.
	rows, err := s.db.conn.Query(`
		SELECT c.id, c.public_key, c.change_type, c.old_value, c.new_value, c.detected_at,
		       n.name, n.role
		FROM node_changes c
		LEFT JOIN nodes n ON n.public_key = c.public_key
		ORDER BY c.id DESC
		LIMIT ?`, nodeChangesSQLFetchCap)
	if err != nil {
		return nil, 0, "", err
	}

	out = make([]NodeChangeEntry, 0, limit)
	for rows.Next() {
		var e NodeChangeEntry
		var oldValue, newValue, name, role sql.NullString
		// A scan failure here aborts the whole fetch (see computeNewNodes
		// for the same reasoning) -- rows must still be closed on this
		// path since it isn't deferred (see the comment below).
		if err := rows.Scan(&e.ID, &e.PublicKey, &e.ChangeType, &oldValue, &newValue, &e.DetectedAt, &name, &role); err != nil {
			rows.Close()
			return nil, 0, "", fmt.Errorf("computeNodeChanges: scan row: %w", err)
		}
		scanned++
		oldestScanned = e.DetectedAt
		e.OldValue = oldValue.String
		e.NewValue = newValue.String
		hidden := identityHidden(s.cfg, e.PublicKey, name.String)
		if !hidden && e.ChangeType == "name" {
			hidden = identityHidden(s.cfg, e.PublicKey, e.OldValue, e.NewValue)
		}
		if hidden {
			continue
		}
		e.Name = name.String
		e.Role = role.String
		if e.ChangeType == "position" {
			e.DistanceKm = positionChangeDistanceKm(e.OldValue, e.NewValue)
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	// Explicitly closed (not deferred) BEFORE the nested query below --
	// issuing a second query while this cursor is still open (the loop
	// above may `break` without draining every row) deadlocks the
	// single-connection SQLite pool used in tests. Recurring lesson in
	// this codebase; see ping_scores.go/observer neighbor code for the
	// same pattern. rows.Err() must be checked AFTER Close() (both are
	// valid to call post-Close, and Close() itself doesn't surface a
	// mid-iteration cursor error the way Err() does).
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, "", fmt.Errorf("computeNodeChanges: row iteration: %w", err)
	}

	if len(out) > 0 {
		pubkeys := make([]string, 0, len(out))
		for _, e := range out {
			pubkeys = append(pubkeys, e.PublicKey)
		}
		foreignFlags := s.db.foreignFlagsForPubkeys(pubkeys)
		for i := range out {
			out[i].Foreign = foreignFlags[out[i].PublicKey]
		}
	}
	return out, scanned, oldestScanned, nil
}

// positionChangeDistanceKm parses cmd/ingestor's "lat,lon" old/new value
// strings and returns the haversine distance between them, or nil if
// either fails to parse.
func positionChangeDistanceKm(oldValue, newValue string) *float64 {
	oldLat, oldLon, ok1 := parseLatLonPair(oldValue)
	newLat, newLon, ok2 := parseLatLonPair(newValue)
	if !ok1 || !ok2 {
		return nil
	}
	d := haversineKm(oldLat, oldLon, newLat, newLon)
	return &d
}

func parseLatLonPair(s string) (lat, lon float64, ok bool) {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(parts[0], 64)
	lon, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return lat, lon, true
}

// handleNodeChanges serves Tools > Node Changes: the most recent audited
// role/name/position changes and pruned-node returns, network-wide. Empty
// list (not an error) when nothing has been logged yet.
func (s *Server) handleNodeChanges(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := s.computeNodeChanges(limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"nodeChanges": entries})
}
