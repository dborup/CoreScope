package main

import "time"

// advertRouteTypeFlood is ROUTE_TYPE_FLOOD from the MeshCore packet header.
// Named distinctly from the equivalent constant in the (still open) unscoped-
// relay PR so the two changes merge independently.
const advertRouteTypeFlood = 1

// floodAdvertEntry is one advert transmission originated by a node, reduced to
// what the windowed flood-advert count needs: first-seen timestamp, route type
// and packet hash (for dedup across re-ingests / multi-observer rows).
type floodAdvertEntry struct {
	ts   string
	rt   int
	hash string
}

// advertWindow is the exact first-seen window shared by the windowed advert
// counters (flood_advert_count_7d and the #2073 advertCounts). Entries with
// unparseable timestamps are skipped, matching relay-liveness behaviour;
// entries without a hash fall back to their timestamp as the dedup key.
type advertWindow struct {
	cutoff time.Time
	seen   map[string]struct{}
}

func newAdvertWindow(now time.Time, windowHours float64) advertWindow {
	return advertWindow{
		cutoff: now.Add(-time.Duration(windowHours * float64(time.Hour))),
		seen:   map[string]struct{}{},
	}
}

// admit reports whether an advert first seen at ts lies inside the window
// and has not been counted yet (by hash), and records it.
func (w advertWindow) admit(ts, hash string) bool {
	t, ok := parseRelayTS(ts)
	if !ok || !t.After(w.cutoff) {
		return false
	}
	key := hash
	if key == "" {
		key = ts
	}
	if _, dup := w.seen[key]; dup {
		return false
	}
	w.seen[key] = struct{}{}
	return true
}

// advertDateFloor is the SQL pre-filter for an advert window: a DATE-ONLY
// string one day before the window start. A date prefix compares lexically
// the same whatever follows it (separator, precision, zone suffix), so the
// floor never drops an in-window row; the exact check is advertWindow.admit
// in Go, which skips what parseRelayTS cannot parse (e.g. a legacy
// 'YYYY-MM-DD HH:MM:SS' value).
func advertDateFloor(now time.Time, windowHours float64) string {
	return now.UTC().Add(-time.Duration(windowHours*float64(time.Hour))).AddDate(0, 0, -1).Format("2006-01-02")
}

// countFloodAdverts counts distinct flood adverts (route_type ==
// advertRouteTypeFlood) whose first-seen lies within the past windowHours
// (see advertWindow).
func countFloodAdverts(entries []floodAdvertEntry, now time.Time, windowHours float64) int {
	w := newAdvertWindow(now, windowHours)
	n := 0
	for _, e := range entries {
		if e.rt == advertRouteTypeFlood && w.admit(e.ts, e.hash) {
			n++
		}
	}
	return n
}

// CountFloodAdvertsForNode returns how many distinct FLOOD adverts pubkey
// originated in the last windowHours - the mesh-wide-airtime kind. Zero-hop
// adverts (route_type DIRECT) are excluded, so a nearby observer hearing a
// node's cheap local adverts does not inflate the number.
//
// route_type is filtered in SQL so an advert-spamming node cannot truncate
// the flood count (review feedback on the earlier LIMIT approach). The time
// floor is advertDateFloor; the exact window check stays in Go.
//
// This is an external contract (the ArcScope advisor reads it) and is kept
// as is, counted fresh on every request and never cached: route_type 1
// only, i.e. the first-inserted route. It therefore differs from
// advertCounts["7d"].flood (node_advert_routes.go), which is route_mask
// based: that one also counts transport flood (route 0) and never counts a
// mixed advert (flood + zero-hop), while this one counts a mixed advert
// whenever its first-inserted route was 1. When a node-detail request scans
// the node for the #2073 breakdown itself, that scan yields this same number
// (GetNodeAdvertRoutes, pinned by a parity test) and this query is skipped.
//
// The row cap is a pure safety valve on per-request allocation: it applies to
// flood adverts inside the floor window only, and 50000 in ~8 days is ~4 per
// minute - any node past it is unambiguously a spammer whether the count
// saturates or not. (An exact COUNT cannot move into SQL because the precise
// window check needs parseRelayTS over the mixed first_seen formats.)
// floodAdvertRowCap is the production row cap; tests pass a smaller cap
// directly, so there is no mutable package state to race on.
const floodAdvertRowCap = 50000

func (db *DB) CountFloodAdvertsForNode(pubkey string, windowHours float64, rowCap int) (int, error) {
	return db.countFloodAdvertsForNodeAt(pubkey, windowHours, rowCap, time.Now())
}

// countFloodAdvertsForNodeAt is CountFloodAdvertsForNode at a given instant,
// for the parity test of the #2073 scan that can stand in for it
// (GetNodeAdvertRoutes).
func (db *DB) countFloodAdvertsForNodeAt(pubkey string, windowHours float64, rowCap int, now time.Time) (int, error) {
	floor := advertDateFloor(now, windowHours)
	rows, err := db.conn.Query(
		"SELECT COALESCE(first_seen, ''), COALESCE(route_type, -1), COALESCE(hash, '') FROM transmissions WHERE from_pubkey = ? AND payload_type = ? AND route_type = ? AND first_seen >= ? ORDER BY id DESC LIMIT ?",
		pubkey, payloadTypeAdvert, advertRouteTypeFlood, floor, rowCap)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var entries []floodAdvertEntry
	for rows.Next() {
		var e floodAdvertEntry
		if err := rows.Scan(&e.ts, &e.rt, &e.hash); err != nil {
			return 0, err
		}
		entries = append(entries, e)
	}
	return countFloodAdverts(entries, now, windowHours), nil
}
