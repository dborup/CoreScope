package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Region-scoped node membership cache for GetNodes (#2101).
//
// `/api/nodes?region=X` restricts the node list to nodes whose ADVERTs were
// heard by an observer in region X. That used to be an inline
// `public_key IN (SELECT DISTINCT from_pubkey FROM transmissions ⋈
// observations ⋈ observers ...)` subquery, uncached, evaluated twice per
// request (COUNT(*) and the page). On a 1.4 GB database (1.47M observations)
// each evaluation took ~10s, and fetchAllNodes() pages at 500, so a single
// open Nodes or Map tab kept a 2-vCPU host busy.
//
// Since #618 an entry is cut from the global heard index (region_index.go):
// one shared scan of the observation history grouped by observer IATA, so a
// region set is the union of its codes' member lists, not a full scan of its
// own. Entries keep their ages and the stale-while-revalidate contract:
//   - fresh (< nodeRegionFreshTTL): served as-is;
//   - stale: served as-is while ONE background refresh re-cuts it from the
//     index, refreshing the index first — a delta scan of observations with
//     id > watermark (a rowid range, milliseconds);
//   - every nodeRegionRebuildInterval the index refresh is a full rebuild
//     instead, so retention pruning, observer IATA changes and late
//     from_pubkey backfills are picked up.
//
// An entry carries the ages of the snapshot it was cut from, so those are
// the ages of the data. A request that has to wait for an entry (a new
// region set, or one past nodeRegionMaxStale) is cut from any snapshot
// younger than nodeRegionMaxStale at once. Only a cold start waits — for
// the index's first build or the set's own per-set build, whichever
// finishes first — and concurrent requests share both (the #1910
// singleflight pattern, as channelsSF). StartRegionIndexWarmer runs the
// index build right after startup and keeps the index refreshed.
//
// Two bounds make that safe to expose to a query parameter (AGENTS.md rule 0,
// "no unbounded data structures"):
//
//   - at most nodeRegionMaxEntries entries, kept by evicting the least
//     recently used one (setNodeRegionEntry), and a stored key longer than
//     nodeRegionMaxKeyBytes is replaced by its digest (nodeRegionKey);
//   - an entry older than nodeRegionMaxStale is not served blind: the caller
//     waits for its refresh, so the ages promised above are the ages
//     actually served even when refreshes keep failing.
//
// The index itself is bounded by the distinct (node, region) pairs in the
// retained history: a few thousand nodes times the regions each was heard
// in, never by the number of hearings.

const (
	nodeRegionFreshTTL        = 30 * time.Second
	nodeRegionRebuildInterval = 30 * time.Minute
	// nodeRegionMaxEntries bounds the cache. Entries hold up to a few thousand
	// pubkeys each (one per node in the region set, so the nodes table caps
	// an entry), so this is far below the shared maxCacheEntries; in practice
	// an instance sees a handful of distinct region sets.
	nodeRegionMaxEntries = 32
	// nodeRegionMaxKeyBytes caps a stored key, as channelListMaxKeyBytes does
	// for the channel list cache: ?region= is caller-controlled and
	// normalizeRegionCodes does not limit how many codes it accepts, so a
	// long set is stored under "sha256:" + 64 hex digits instead. Normalized
	// codes are upper-case, so no short key can collide with a digest.
	nodeRegionMaxKeyBytes = 256
	// nodeRegionMaxStale bounds the age of a served entry. Below it a stale
	// entry is served while one refresh runs in the background — the fast
	// path every page uses. At or above it the caller waits for the refresh,
	// so the documented bounds hold: without the wait a refresh that keeps
	// failing serves membership of unbounded age with only a log line to
	// show it. The wait is a re-cut from the index, which the warmer keeps
	// younger than this; only an index that is itself this old (its
	// refreshes failing) makes the caller wait for a scan.
	nodeRegionMaxStale = nodeRegionRebuildInterval
)

// errNodeRegionFlightResult means nodeRegionSF returned something other than
// a *nodeRegionEntry, which only a programming error can cause.
var errNodeRegionFlightResult = errors.New("unexpected node region flight result")

// nodeRegionEntry is immutable once stored: refreshes build a new entry, so
// readers can use one without holding nodeRegionCacheMu.
type nodeRegionEntry struct {
	keys      []string // sorted from_pubkeys that are members of the region set
	keysJSON  string   // keys as a JSON array, bound to json_each(?)
	watermark int64    // max observations.id covered by keys
	refreshed time.Time
	built     time.Time // the index's last full rebuild
}

func (e *nodeRegionEntry) fresh() bool {
	return time.Since(e.refreshed) < nodeRegionFreshTTL
}

// newNodeRegionEntry snapshots set into an entry.
func newNodeRegionEntry(set map[string]struct{}, watermark int64, refreshed, built time.Time) (*nodeRegionEntry, error) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("encode region keys: %w", err)
	}
	return &nodeRegionEntry{
		keys:      keys,
		keysJSON:  string(buf),
		watermark: watermark,
		refreshed: refreshed,
		built:     built,
	}, nil
}

// nodeRegionKey canonicalises normalised region codes so "EDI,GLA",
// "gla, edi" and "EDI,EDI,GLA" share one cache entry. A set long enough to
// bloat the map (or the log line) is keyed by its digest instead; the codes
// themselves are passed to the scan separately, so the key is only an
// identity.
func nodeRegionKey(codes []string) string {
	seen := make(map[string]struct{}, len(codes))
	uniq := make([]string, 0, len(codes))
	for _, c := range codes {
		if _, ok := seen[c]; !ok {
			seen[c] = struct{}{}
			uniq = append(uniq, c)
		}
	}
	sort.Strings(uniq)
	key := strings.Join(uniq, ",")
	if len(key) <= nodeRegionMaxKeyBytes {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// getNodeRegionEntry returns the entry for key and marks it as the most
// recently used one, so eviction drops the sets nobody is paging through.
func (db *DB) getNodeRegionEntry(key string) *nodeRegionEntry {
	db.nodeRegionCacheMu.Lock()
	defer db.nodeRegionCacheMu.Unlock()
	e := db.nodeRegionCache[key]
	if e != nil {
		db.touchNodeRegionLocked(key)
	}
	return e
}

// setNodeRegionEntry stores e, evicting the least recently used entry when
// the cache is full.
//
// It must evict exactly one entry, never clear the map: ?region= is
// caller-controlled, so a client cycling through more than
// nodeRegionMaxEntries distinct region sets would otherwise drop the entries
// the real pages depend on, and every following request would pay a full
// observation scan — serialised behind nodeRegionFullMu, seconds each on a
// large database. That is the one thing this cache exists to prevent.
func (db *DB) setNodeRegionEntry(key string, e *nodeRegionEntry) {
	db.nodeRegionCacheMu.Lock()
	defer db.nodeRegionCacheMu.Unlock()
	if db.nodeRegionCache == nil {
		db.nodeRegionCache = make(map[string]*nodeRegionEntry, nodeRegionMaxEntries)
		db.nodeRegionUsed = make(map[string]int64, nodeRegionMaxEntries)
	}
	if _, replacing := db.nodeRegionCache[key]; !replacing {
		for len(db.nodeRegionCache) >= nodeRegionMaxEntries {
			victim, oldest, found := "", int64(0), false
			for k, used := range db.nodeRegionUsed {
				if !found || used < oldest {
					victim, oldest, found = k, used, true
				}
			}
			if !found {
				break // cannot happen: the two maps are written together
			}
			delete(db.nodeRegionCache, victim)
			delete(db.nodeRegionUsed, victim)
		}
	}
	db.nodeRegionCache[key] = e
	db.touchNodeRegionLocked(key)
}

// touchNodeRegionLocked records key as the most recent use. Caller holds
// nodeRegionCacheMu. The tick is a counter, not a clock, so entries touched
// inside one coarse clock tick still order.
func (db *DB) touchNodeRegionLocked(key string) {
	db.nodeRegionTick++
	if db.nodeRegionUsed == nil {
		db.nodeRegionUsed = make(map[string]int64, nodeRegionMaxEntries)
	}
	db.nodeRegionUsed[key] = db.nodeRegionTick
}

// nodeRegionKeysJSON returns the JSON array of node public keys heard in the
// given (normalised, non-empty) region codes, for binding to
// `public_key IN (SELECT value FROM json_each(?))`.
func (db *DB) nodeRegionKeysJSON(codes []string) (string, error) {
	key := nodeRegionKey(codes)

	if e := db.getNodeRegionEntry(key); e != nil {
		age := time.Since(e.refreshed)
		switch {
		case age < nodeRegionFreshTTL:
			return e.keysJSON, nil
		case age < nodeRegionMaxStale:
			// Stale-while-revalidate: kick one refresh, don't wait on it.
			// DoChan dedups against an in-flight refresh for the same key,
			// and its buffered result channel may be left unread.
			db.nodeRegionSF.DoChan(key, func() (any, error) {
				return db.refreshNodeRegion(key, codes, true)
			})
			return e.keysJSON, nil
		}
		// Past nodeRegionMaxStale the entry is too old to serve blind, so
		// wait for a re-cut from the index. A refresh that fails still falls
		// back to the stale entry: an outage of the observation scan must
		// not turn /api/nodes?region= into a 500.
		v, err, _ := db.nodeRegionSF.Do(key, func() (any, error) {
			return db.refreshNodeRegion(key, codes, false)
		})
		if err != nil {
			log.Printf("[nodes-region] %s: serving entry %v old, refresh failed: %v",
				key, age.Round(time.Second), err)
			return e.keysJSON, nil
		}
		if fresh, ok := v.(*nodeRegionEntry); ok {
			return fresh.keysJSON, nil
		}
		return "", fmt.Errorf("nodes-region %s: %w: %T", key, errNodeRegionFlightResult, v)
	}

	v, err, _ := db.nodeRegionSF.Do(key, func() (any, error) {
		return db.refreshNodeRegion(key, codes, false)
	})
	if err != nil {
		return "", err
	}
	e, ok := v.(*nodeRegionEntry)
	if !ok {
		return "", fmt.Errorf("nodes-region %s: %w: %T", key, errNodeRegionFlightResult, v)
	}
	return e.keysJSON, nil
}

// refreshNodeRegion re-cuts the cache entry for key from the active region
// index and stores it. With waitIndex (a background refresh, which nobody
// waits on) a stale index is refreshed first; without it (a caller is
// waiting) any snapshot younger than nodeRegionMaxStale is cut at once and
// the index refresh runs in the background (currentRegionIndex), and before
// the first snapshot exists the set's own build races the index build
// (coldNodeRegion). It runs
// inside nodeRegionSF, so at most one refresh per key is in flight.
// Failures are logged here because a background refresh has no caller to
// report to; the previous entry stays in place.
func (db *DB) refreshNodeRegion(key string, codes []string, waitIndex bool) (*nodeRegionEntry, error) {
	prev := db.getNodeRegionEntry(key)
	if prev != nil && prev.fresh() {
		return prev, nil // a flight that finished just before this one got here
	}
	if db.nodeRegionQueryHook != nil {
		if err := db.nodeRegionQueryHook(); err != nil {
			return nil, fmt.Errorf("nodes-region %s: %w", key, err)
		}
	}

	// A scan still running when the next rebuild would be due is abandoned
	// rather than left holding one of the pooled connections.
	ctx, cancel := context.WithTimeout(context.Background(), nodeRegionRebuildInterval)
	defer cancel()

	ix, spec := db.activeRegionIndex()
	var e *nodeRegionEntry
	var err error
	if !waitIndex && spec.perSet != nil && ix.snap.Load() == nil {
		e, err = db.coldNodeRegion(ctx, ix, spec, codes)
	} else {
		var s *regionMembershipSnapshot
		if s, err = db.currentRegionIndex(ctx, ix, spec, waitIndex); err == nil {
			e, err = s.entry(codes)
		}
	}
	if err != nil {
		err = fmt.Errorf("nodes-region %s: %w", key, err)
		log.Printf("[nodes-region] refresh failed: %v", err)
		return nil, err
	}
	db.setNodeRegionEntry(key, e)
	return e, nil
}

// maxObservationID pins the upper bound of a scan. The ingestor is the single
// writer and observations.id is AUTOINCREMENT, so every id <= the returned
// value is committed and visible to a later read; newer rows are left for the
// next delta.
func (db *DB) maxObservationID(ctx context.Context) (int64, error) {
	const q = "SELECT COALESCE(MAX(id), 0) FROM observations"
	// Issue exactly one query: an unscanned *sql.Row keeps its connection
	// checked out, and the pool is 4 (OpenDB).
	row := db.conn.QueryRowContext(ctx, q)
	var wm int64
	if err := row.Scan(&wm); err != nil {
		return 0, fmt.Errorf("max observation id: %w", err)
	}
	return wm, nil
}

// buildNodeRegionCold is the per-set build a request runs while the region
// index has no snapshot yet (coldNodeRegion): the full scan every cold
// region set ran before #618. These builds are serialised, as they were
// then, so a burst of cold sets holds one pooled connection besides the
// index build's. ctx ending means the build is no longer wanted: a build
// still queued gives up, one already scanning finishes under the same
// timeout as the index build rather than be interrupted (an interrupted
// query costs its pooled connection).
func (db *DB) buildNodeRegionCold(ctx context.Context, codes []string) (*nodeRegionEntry, error) {
	db.nodeRegionColdMu.Lock()
	defer db.nodeRegionColdMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err // the index build won while this one queued
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nodeRegionRebuildInterval)
	defer cancel()
	if db.nodeRegionPerSetHook != nil {
		db.nodeRegionPerSetHook()
	}
	wm, err := db.maxObservationID(ctx)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	set := make(map[string]struct{})
	if err := db.scanNodeRegionKeys(ctx, codes, wm, set); err != nil {
		return nil, err
	}
	log.Printf("[nodes-region] %s: per-set build while the index warms up, %d nodes in %v (obs id <= %d)",
		nodeRegionKey(codes), len(set), time.Since(start).Round(time.Millisecond), wm)
	return newNodeRegionEntry(set, wm, time.Now(), start)
}

// scanNodeRegionKeys adds to set every ADVERT from_pubkey observed by an
// observer in codes with 0 < observations.id <= hi: the heard rule for one
// region set, as master built it, left to the planner, which can drive it
// from the set's own observers. Keep both id bounds: without the lower one
// the planner walks observations by rowid instead (2.5x slower on the #176
// perf database). Only fixed fragments and "?" placeholders are formatted
// in; every value is bound. scanHeardRegions carries the from_pubkey
// contract.
func (db *DB) scanNodeRegionKeys(ctx context.Context, codes []string, hi int64, set map[string]struct{}) error {
	placeholders := make([]string, len(codes))
	args := []any{payloadTypeAdvert}
	for i, c := range codes {
		placeholders[i] = "?"
		args = append(args, c)
	}
	args = append(args, int64(0), hi)
	joinCond := "obs.rowid = o.observer_idx"
	if !db.isV3() {
		joinCond = "obs.id = o.observer_id"
	}
	q := fmt.Sprintf(`SELECT DISTINCT t.from_pubkey
		FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id
		JOIN observers obs ON %[1]s
		WHERE t.payload_type = ?
		AND t.from_pubkey IS NOT NULL
		AND UPPER(TRIM(obs.iata)) IN (%[2]s)
		AND o.id > ? AND o.id <= ?`, joinCond, strings.Join(placeholders, ","))
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("scan region keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			return fmt.Errorf("scan region keys: %w", err)
		}
		set[pk] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan region keys: %w", err)
	}
	return nil
}
