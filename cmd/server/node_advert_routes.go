package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// Node-detail advert route breakdown: a port/extension of upstream
// `Kpa-clawbot/CoreScope#2073`. recentAdvertsByRoute lists a node's newest
// adverts per route class and advertCounts counts them per class over 24h
// and 7d. Both classify exactly like Relay Airtime Share (#89,
// classifyAdvertRoute): route_mask when the ingestor has set it, else the
// first-inserted route_type.

// Route class names on the wire (NodeAdvertsByRoute keys, route_class).
const (
	advertClassFlood   = "flood"
	advertClassZeroHop = "zero_hop"
	advertClassMixed   = "mixed"
	advertClassUnknown = "unknown"
)

// nodeAdvertRouteLimit is the per-class list length, the same 20 as the
// chronological recentAdverts list.
const nodeAdvertRouteLimit = 20

func advertRouteClassName(c relayAirtimeAdvertRoute) string {
	switch c {
	case relayAirtimeAdvertFlood:
		return advertClassFlood
	case relayAirtimeAdvertZeroHop:
		return advertClassZeroHop
	case relayAirtimeAdvertMixed:
		return advertClassMixed
	}
	return advertClassUnknown
}

// classifyAdvertRouteRow classifies a transmissions row from its route_mask
// and route_type columns (NULL mask: not backfilled or no column).
func classifyAdvertRouteRow(mask, routeType sql.NullInt64) relayAirtimeAdvertRoute {
	var rt *int
	if routeType.Valid {
		v := int(routeType.Int64)
		rt = &v
	}
	return classifyAdvertRoute(mask.Int64, mask.Valid, rt)
}

// setAdvertRouteClass adds route_class to an ADVERT row from
// scanTransmissionRow; other payload types have no advert route class.
func setAdvertRouteClass(p NodeAdvertRow, mask sql.NullInt64) {
	if pt, ok := p["payload_type"].(int); !ok || pt != payloadTypeAdvert {
		return
	}
	var routeType sql.NullInt64
	if rt, ok := p["route_type"].(int); ok {
		routeType = sql.NullInt64{Int64: int64(rt), Valid: true}
	}
	p["route_class"] = advertRouteClassName(classifyAdvertRouteRow(mask, routeType))
}

// advertRouteClassSQL is classifyAdvertRoute as a SQL expression over the
// unaliased transmissions columns, yielding the route class name. Without
// the route_mask column every row takes the route_type fallback.
// TestAdvertRouteClassSQL_MatchesGoClassifier pins it to the Go rule.
func advertRouteClassSQL(hasRouteMask bool) string {
	fallback := fmt.Sprintf(`WHEN route_type IN (%d, %d) THEN '%s' WHEN route_type IN (%d, %d) THEN '%s' ELSE '%s'`,
		RouteTransportFlood, RouteFlood, advertClassFlood,
		RouteDirect, RouteTransportDirect, advertClassZeroHop, advertClassUnknown)
	if !hasRouteMask {
		return "(CASE " + fallback + " END)"
	}
	flood, direct := packetpath.RouteMaskFlood, packetpath.RouteMaskDirect
	return fmt.Sprintf(`(CASE WHEN route_mask & %d <> 0 THEN (CASE WHEN route_mask & %d <> 0 AND route_mask & %d <> 0 THEN '%s' WHEN route_mask & %d <> 0 THEN '%s' ELSE '%s' END) %s END)`,
		packetpath.RouteMaskAll, flood, direct, advertClassMixed, flood, advertClassFlood, advertClassZeroHop, fallback)
}

// advertRouteEntry is one advert inside the count floor: first-seen, hash
// (dedup key) and route class.
type advertRouteEntry struct {
	ts, hash, class string
}

// countAdvertRoutes counts distinct adverts per class whose first-seen lies
// in the past windowHours (advertWindow: exact parseRelayTS window, dedup by
// hash, as flood_advert_count_7d).
func countAdvertRoutes(entries []advertRouteEntry, now time.Time, windowHours float64) AdvertRouteCounts {
	w := newAdvertWindow(now, windowHours)
	var c AdvertRouteCounts
	for _, e := range entries {
		if !w.admit(e.ts, e.hash) {
			continue
		}
		switch e.class {
		case advertClassFlood:
			c.Flood++
		case advertClassZeroHop:
			c.ZeroHop++
		case advertClassMixed:
			c.Mixed++
		default:
			c.Unknown++
		}
	}
	return c
}

// GetNodeAdvertRoutes returns pubkey's newest perClass adverts per route
// class and its per-class advert counts for 24h and 7d.
//
// One scan of the node's ADVERT rows serves both, because visiting the rows
// (a table lookup each after the from_pubkey index) is the cost on an
// advert-spamming node: the class is computed in SQL and ROW_NUMBER() per
// class keeps the newest perClass ids of every class (the class filter runs
// before the limit), while every row at or after the 7d date floor also
// feeds the counts. The counts window is first_seen - when the advert was
// first heard, the same axis as flood_advert_count_7d - not the ingest id
// that orders the lists (#1345), so a late buffered upload counts in the
// window it was sent in.
//
// rowCap bounds the count entries held per request like
// CountFloodAdvertsForNode (newest rows win, Truncated reports it); the
// lists are unaffected. The full rows of the listed ids are read in one
// batch afterwards. RouteMaskBackfill is left for the caller
// (Server.nodeAdvertRoutes, which also caches the result).
func (db *DB) GetNodeAdvertRoutes(pubkey string, perClass int, now time.Time, rowCap int) (NodeAdvertsByRoute, NodeAdvertCounts, error) {
	byRoute := NodeAdvertsByRoute{Limit: perClass, Flood: NodeAdvertRows{}, ZeroHop: NodeAdvertRows{}, Mixed: NodeAdvertRows{}}
	var counts NodeAdvertCounts
	floor := advertDateFloor(now, 7*24)
	class := advertRouteClassSQL(db.hasRouteMask())
	rows, err := db.conn.Query(`SELECT id, cls, rn, first_seen, hash FROM (
			SELECT id, cls, COALESCE(first_seen, '') AS first_seen, COALESCE(hash, '') AS hash,
				ROW_NUMBER() OVER (PARTITION BY cls ORDER BY id DESC) AS rn
			FROM (SELECT id, first_seen, hash, `+class+` AS cls FROM transmissions WHERE from_pubkey = ? AND payload_type = ?)
		) WHERE rn <= ? OR first_seen >= ?
		ORDER BY id DESC`, pubkey, payloadTypeAdvert, perClass, floor)
	if err != nil {
		return byRoute, counts, err
	}
	listed := map[int]string{}
	var ids []interface{}
	var entries []advertRouteEntry
	for rows.Next() {
		var id, rn int
		var cls, ts, hash string
		if err := rows.Scan(&id, &cls, &rn, &ts, &hash); err != nil {
			rows.Close()
			return byRoute, counts, err
		}
		if rn <= perClass {
			listed[id] = cls
			ids = append(ids, id)
		}
		if ts >= floor {
			if len(entries) < rowCap {
				entries = append(entries, advertRouteEntry{ts: ts, hash: hash, class: cls})
			} else {
				counts.Truncated = true
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return byRoute, counts, err
	}
	counts.H24 = countAdvertRoutes(entries, now, 24)
	counts.D7 = countAdvertRoutes(entries, now, 7*24)

	if len(ids) == 0 {
		return byRoute, counts, nil
	}
	// Lean rows (no observation arrays): they are cached and sit next to
	// the full recentAdverts rows. route_class is the class the rows were
	// selected by, so a row never disagrees with its list even if the
	// ingestor widens its route_mask meanwhile.
	full, err := db.queryNodeAdvertRows(false, "t.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+") ORDER BY t.id DESC", ids...)
	if err != nil {
		return byRoute, counts, err
	}
	for _, p := range full {
		id, _ := p["id"].(int)
		p["route_class"] = listed[id]
		switch listed[id] {
		case advertClassFlood:
			byRoute.Flood = append(byRoute.Flood, p)
		case advertClassZeroHop:
			byRoute.ZeroHop = append(byRoute.ZeroHop, p)
		case advertClassMixed:
			byRoute.Mixed = append(byRoute.Mixed, p)
		default:
			byRoute.Unknown = append(byRoute.Unknown, p)
		}
	}
	return byRoute, counts, nil
}
