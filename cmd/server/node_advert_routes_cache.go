package main

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// The #2073 advert route breakdown scans all of a node's ADVERT rows: a
// table lookup per row, ~130 ms for a node with 50,000 adverts on a
// 2,000,000-transmission database (node_advert_routes_bench_test.go). The
// result is cached per pubkey so repeated views do not rescan:
//   - nodeAdvertRouteTTL bounds staleness from changes that do not add a
//     row - a route_mask promoted to mixed, the backfill, the windows
//     sliding;
//   - a newer transmission from the node invalidates its entry alone (read
//     from the covering from_pubkey index), debounced so a node that adverts
//     every few seconds is rescanned at most once per nodeAdvertRouteDebounce;
//   - at most nodeAdvertRouteCacheMax entries, expired ones dropped on every
//     put and the oldest evicted first. An entry holds up to 4 x 20 lean
//     rows (no observation arrays), a few KB each;
//   - concurrent misses for one pubkey share a single scan (singleflight).
//
// These are hardcoded for now; like the other cache TTLs they are candidates
// for config.
const (
	nodeAdvertRouteTTL      = 30 * time.Second
	nodeAdvertRouteDebounce = 5 * time.Second
	nodeAdvertRouteCacheMax = 128
)

type nodeAdvertRouteEntry struct {
	byRoute  NodeAdvertsByRoute
	counts   NodeAdvertCounts
	latestID int64 // the node's newest transmission id when computed
	at       time.Time
}

// nodeAdvertRouteCache is usable as a zero value. Cached rows are shared
// between responses and never mutated after put.
type nodeAdvertRouteCache struct {
	mu      sync.Mutex
	entries map[string]nodeAdvertRouteEntry
	flight  singleflight.Group
	onScan  func() // test hook: called once per scan
}

func (c *nodeAdvertRouteCache) get(pubkey string, latestID int64, now time.Time) (nodeAdvertRouteEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[pubkey]
	if !ok {
		return e, false
	}
	age := now.Sub(e.at)
	if age >= nodeAdvertRouteTTL || (e.latestID != latestID && age >= nodeAdvertRouteDebounce) {
		delete(c.entries, pubkey)
		return e, false
	}
	return e, true
}

// put stores e, dropping expired entries first and then, if still full, the
// oldest one. O(nodeAdvertRouteCacheMax) under the lock, no I/O.
func (c *nodeAdvertRouteCache) put(pubkey string, e nodeAdvertRouteEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]nodeAdvertRouteEntry)
	}
	for k, v := range c.entries {
		if now.Sub(v.at) >= nodeAdvertRouteTTL {
			delete(c.entries, k)
		}
	}
	if _, ok := c.entries[pubkey]; !ok && len(c.entries) >= nodeAdvertRouteCacheMax {
		oldest := ""
		for k, v := range c.entries {
			if oldest == "" || v.at.Before(c.entries[oldest].at) {
				oldest = k
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[pubkey] = e
}

func (c *nodeAdvertRouteCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// latestTransmissionIDForNode is the node's newest transmission id (0 when
// none), an index-only read of idx_transmissions_from_pubkey.
func (db *DB) latestTransmissionIDForNode(pubkey string) (int64, error) {
	var id int64
	err := db.conn.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM transmissions WHERE from_pubkey = ?`, pubkey).Scan(&id)
	return id, err
}

// nodeAdvertRoutes is GetNodeAdvertRoutes through the per-pubkey cache; the
// route_mask backfill status is read fresh (it has its own short cache).
func (s *Server) nodeAdvertRoutes(pubkey string, now time.Time) (NodeAdvertsByRoute, NodeAdvertCounts, error) {
	latestID, err := s.db.latestTransmissionIDForNode(pubkey)
	if err != nil {
		return NodeAdvertsByRoute{}, NodeAdvertCounts{}, err
	}
	c := &s.advertRoutes
	e, ok := c.get(pubkey, latestID, now)
	if !ok {
		v, err, _ := c.flight.Do(pubkey, func() (interface{}, error) {
			if c.onScan != nil {
				c.onScan()
			}
			byRoute, counts, err := s.db.GetNodeAdvertRoutes(pubkey, nodeAdvertRouteLimit, now, floodAdvertRowCap)
			if err != nil {
				return nil, err
			}
			fresh := nodeAdvertRouteEntry{byRoute: byRoute, counts: counts, latestID: latestID, at: now}
			c.put(pubkey, fresh, now)
			return fresh, nil
		})
		if err != nil {
			return NodeAdvertsByRoute{}, NodeAdvertCounts{}, err
		}
		e = v.(nodeAdvertRouteEntry)
	}
	e.counts.RouteMaskBackfill = s.db.routeMaskBackfillStatus()
	return e.byRoute, e.counts, nil
}
