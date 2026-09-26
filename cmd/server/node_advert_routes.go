package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// Node-detail advert route breakdown: a port/extension of upstream
// `Kpa-clawbot/CoreScope#2073`. recentAdvertsByRoute lists a node's newest
// adverts per route class and advertCounts counts them per class over 24h
// and 7d. Both classify exactly like Relay Airtime Share (#89,
// classifyAdvertRoute): route_mask when the ingestor has set it, else the
// first-inserted route_type. The no-usable-route class is "unknown" here and
// "legacy" in Relay Airtime Share's route_class (its historical plain ADVERT
// row); both name the same relayAirtimeAdvertUnknown bucket.
//
// The breakdown is opt-in: GET /api/nodes/{pubkey} computes it only with
// include=advertRoutes, which only the node page sends.

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

// nodeDetailIncludeAdvertRoutes is the include value of GET
// /api/nodes/{pubkey} that opts in to the breakdown.
const nodeDetailIncludeAdvertRoutes = "advertRoutes"

// wantsNodeAdvertRoutes reports whether the request opted in with
// include=advertRoutes (the include parameter may repeat and holds a
// comma-separated list).
func wantsNodeAdvertRoutes(r *http.Request) bool {
	for _, v := range r.URL.Query()["include"] {
		for _, item := range strings.Split(v, ",") {
			if strings.TrimSpace(item) == nodeDetailIncludeAdvertRoutes {
				return true
			}
		}
	}
	return false
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

// nodeAdvertScanSQL reads a node's ADVERT rows newest id first, in the order
// of idx_transmissions_from_pubkey (no sort, no window). Beside the class
// inputs it returns the two predicates of CountFloodAdvertsForNode as SQL
// evaluates them - first_seen >= floor and route_type = 1 - so the flood
// count derived from this scan is that function's bit for bit, whatever the
// stored types. The class inputs are normalized to what the SQL classifier
// (and TestAdvertRouteClassSQL_MatchesGoClassifier) saw: route_type only as
// one of the four route values, route_mask only in its route bits. The unary
// + on payload_type keeps it off idx_transmissions_payload_type, so the
// planner always walks idx_transmissions_from_pubkey: without ANALYZE
// statistics (neither binary runs it) the choice between the two otherwise
// depends on index creation order and SQLite version, and the payload_type
// index would visit every ADVERT in the database. For an INTEGER column
// "+payload_type = ?" matches exactly the rows "payload_type = ?" does.
// Parameters: floor, flood route type, pubkey, payload type.
func nodeAdvertScanSQL(hasRouteMask bool) string {
	mask := "NULL"
	if hasRouteMask {
		mask = fmt.Sprintf("route_mask & %d", packetpath.RouteMaskAll)
	}
	return fmt.Sprintf(`SELECT id,
			CASE WHEN route_type IN (%d, %d, %d, %d) THEN CAST(route_type AS INTEGER) END,
			%s,
			COALESCE(first_seen, ''), COALESCE(hash, ''),
			COALESCE(first_seen >= ?, 0), COALESCE(route_type = ?, 0)
		FROM transmissions WHERE from_pubkey = ? AND +payload_type = ?
		ORDER BY id DESC`,
		RouteTransportFlood, RouteFlood, RouteDirect, RouteTransportDirect, mask)
}

// nodeAdvertScan is what one pass over a node's ADVERT rows yields: the ids
// to list (newest first) with their class, the per-class counts and
// flood_advert_count_7d.
type nodeAdvertScan struct {
	ids     []int
	classes []string // classes[i] is the class ids[i] is listed under
	counts  NodeAdvertCounts
	flood7d int
}

// scanNodeAdvertRoutes is the pass behind GetNodeAdvertRoutes (see there).
func (db *DB) scanNodeAdvertRoutes(pubkey string, perClass int, now time.Time, rowCap int) (nodeAdvertScan, error) {
	var sc nodeAdvertScan
	const floodWindowHours = 7 * 24
	floor := advertDateFloor(now, floodWindowHours)
	rows, err := db.conn.Query(nodeAdvertScanSQL(db.hasRouteMask()), floor, advertRouteTypeFlood, pubkey, payloadTypeAdvert)
	if err != nil {
		return sc, err
	}
	defer rows.Close()
	perClassSeen := map[string]int{}
	var entries []advertRouteEntry
	var floods []floodAdvertEntry
	for rows.Next() {
		var id, inFloor, isFlood int
		var routeType, mask sql.NullInt64
		var ts, hash string
		if err := rows.Scan(&id, &routeType, &mask, &ts, &hash, &inFloor, &isFlood); err != nil {
			return sc, err
		}
		cls := advertRouteClassName(classifyAdvertRouteRow(mask, routeType))
		if perClassSeen[cls] < perClass {
			perClassSeen[cls]++
			sc.ids = append(sc.ids, id)
			sc.classes = append(sc.classes, cls)
		}
		if inFloor == 0 {
			continue
		}
		if len(entries) < rowCap {
			entries = append(entries, advertRouteEntry{ts: ts, hash: hash, class: cls})
		} else {
			sc.counts.Truncated = true
		}
		if isFlood != 0 && len(floods) < rowCap {
			floods = append(floods, floodAdvertEntry{ts: ts, rt: advertRouteTypeFlood, hash: hash})
		}
	}
	if err := rows.Err(); err != nil {
		return sc, err
	}
	sc.counts.H24 = countAdvertRoutes(entries, now, 24)
	sc.counts.D7 = countAdvertRoutes(entries, now, floodWindowHours)
	sc.flood7d = countFloodAdverts(floods, now, floodWindowHours)
	return sc, nil
}

// GetNodeAdvertRoutes returns pubkey's newest perClass adverts per route
// class, its per-class advert counts for 24h and 7d, and its
// flood_advert_count_7d.
//
// One index-ordered pass over the node's ADVERT rows (nodeAdvertScanSQL)
// serves all three, because visiting the rows - a table lookup each after
// the from_pubkey index - is the cost on an advert-spamming node. Go
// classifies each row (classifyAdvertRoute) and keeps the first perClass rows
// of every class, so the class filter runs before the limit; every row at or
// after the 7d date floor also feeds the counts. The counts window is
// first_seen - when the advert was first heard, the same axis as
// flood_advert_count_7d - not the ingest id that orders the lists (#1345), so
// a late buffered upload counts in the window it was sent in.
//
// rowCap (> 0) bounds the count entries held per request like
// CountFloodAdvertsForNode (newest rows win, Truncated reports it); the
// lists are unaffected. flood7d equals CountFloodAdvertsForNode(pubkey, 7*24,
// rowCap) evaluated at now on the same rows: its own rowCap over the rows
// with route_type = 1 inside the floor, then countFloodAdverts
// (TestNodeAdvertRoutes_FloodCountMatchesCountFloodAdvertsForNode). The full
// rows of the listed ids are read in one batch afterwards. RouteMaskBackfill
// is left for the caller (Server.nodeAdvertRoutes, which also caches the
// lists and counts, never flood7d).
func (db *DB) GetNodeAdvertRoutes(pubkey string, perClass int, now time.Time, rowCap int) (NodeAdvertsByRoute, NodeAdvertCounts, int, error) {
	byRoute := NodeAdvertsByRoute{Limit: perClass, Flood: NodeAdvertRows{}, ZeroHop: NodeAdvertRows{}, Mixed: NodeAdvertRows{}}
	sc, err := db.scanNodeAdvertRoutes(pubkey, perClass, now, rowCap)
	if err != nil || len(sc.ids) == 0 {
		return byRoute, sc.counts, sc.flood7d, err
	}
	// Lean rows (no observation arrays): they are cached and sit next to
	// the full recentAdverts rows. route_class is the class the rows were
	// selected by, so a row never disagrees with its list even if the
	// ingestor widens its route_mask meanwhile.
	listed := make(map[int]string, len(sc.ids))
	args := make([]interface{}, len(sc.ids))
	for i, id := range sc.ids {
		listed[id] = sc.classes[i]
		args[i] = id
	}
	full, err := db.queryNodeAdvertRows(false, false, "t.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+") ORDER BY t.id DESC", args...)
	if err != nil {
		return byRoute, sc.counts, 0, err
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
	return byRoute, sc.counts, sc.flood7d, nil
}
