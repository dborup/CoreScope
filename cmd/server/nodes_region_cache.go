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
// Membership only grows as observations arrive, so it is cached per region
// set with an observations.id watermark:
//   - fresh (< nodeRegionFreshTTL): served as-is;
//   - stale: served as-is while ONE background refresh scans only
//     observations with id > watermark (a rowid range, milliseconds);
//   - every nodeRegionRebuildInterval the refresh is a full rebuild instead,
//     so retention pruning, observer IATA changes and late from_pubkey
//     backfills are picked up.
//
// Only the first request for a region set waits on a full scan, and
// concurrent first requests share it (the #1910 singleflight pattern, as
// channelsSF).
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
	// which is a full rebuild at this age, so the documented bounds hold:
	// without the wait a refresh that keeps failing serves membership of
	// unbounded age with only a log line to show it. The cost is that the
	// first region request after nodeRegionMaxStale with no region traffic
	// pays one full scan, exactly as a cold start does.
	nodeRegionMaxStale = nodeRegionRebuildInterval
)

// errNodeRegionFlightResult means nodeRegionSF returned something other than
// a *nodeRegionEntry, which only a programming error can cause.
var errNodeRegionFlightResult = errors.New("unexpected node region flight result")

// nodeRegionEntry is immutable once stored: refreshes build a new entry, so
// readers can use one without holding nodeRegionCacheMu.
type nodeRegionEntry struct {
	keys      []string // sorted from_pubkeys heard in the region set
	keysJSON  string   // keys as a JSON array, bound to json_each(?)
	watermark int64    // max observations.id covered by keys
	refreshed time.Time
	built     time.Time // last full rebuild
}

func (e *nodeRegionEntry) fresh() bool {
	return time.Since(e.refreshed) < nodeRegionFreshTTL
}

func (e *nodeRegionEntry) rebuildDue() bool {
	return time.Since(e.built) >= nodeRegionRebuildInterval
}

// newNodeRegionEntry snapshots set into an entry.
func newNodeRegionEntry(set map[string]struct{}, watermark int64, built time.Time) (*nodeRegionEntry, error) {
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
		refreshed: time.Now(),
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
				return db.refreshNodeRegion(key, codes)
			})
			return e.keysJSON, nil
		}
		// Past nodeRegionMaxStale the entry is too old to serve blind, so
		// wait for the refresh (a full rebuild at this age). A refresh that
		// fails still falls back to the stale entry: an outage of the
		// observation scan must not turn /api/nodes?region= into a 500.
		v, err, _ := db.nodeRegionSF.Do(key, func() (any, error) {
			return db.refreshNodeRegion(key, codes)
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
		return db.refreshNodeRegion(key, codes)
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

// refreshNodeRegion brings the cache entry for key up to date and stores it:
// a delta scan past the watermark when an entry exists and is not due a
// rebuild, a full scan otherwise. It runs inside nodeRegionSF, so at most one
// refresh per key is in flight. Failures are logged here because a background
// refresh has no caller to report to; the previous entry stays in place.
func (db *DB) refreshNodeRegion(key string, codes []string) (*nodeRegionEntry, error) {
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

	var e *nodeRegionEntry
	var err error
	if prev == nil || prev.rebuildDue() {
		e, err = db.buildNodeRegion(ctx, key, codes)
	} else {
		e, err = db.extendNodeRegion(ctx, key, codes, prev)
	}
	if err != nil {
		err = fmt.Errorf("nodes-region %s: %w", key, err)
		log.Printf("[nodes-region] refresh failed: %v", err)
		return nil, err
	}
	db.setNodeRegionEntry(key, e)
	return e, nil
}

// buildNodeRegion scans the full observation history for the region set.
// Full builds are serialised across region sets: each holds a pooled
// connection for seconds on a large database, and entries built together at
// startup fall due together.
func (db *DB) buildNodeRegion(ctx context.Context, key string, codes []string) (*nodeRegionEntry, error) {
	db.nodeRegionFullMu.Lock()
	defer db.nodeRegionFullMu.Unlock()

	wm, err := db.maxObservationID(ctx)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	set := make(map[string]struct{})
	if _, err := db.scanNodeRegionKeys(ctx, codes, 0, wm, false, set); err != nil {
		return nil, err
	}
	log.Printf("[nodes-region] %s: full build, %d nodes in %v (obs id <= %d)",
		key, len(set), time.Since(start).Round(time.Millisecond), wm)
	return newNodeRegionEntry(set, wm, start)
}

// extendNodeRegion adds nodes heard since prev's watermark.
func (db *DB) extendNodeRegion(
	ctx context.Context, key string, codes []string, prev *nodeRegionEntry,
) (*nodeRegionEntry, error) {
	wm, err := db.maxObservationID(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(prev.keys))
	for _, k := range prev.keys {
		set[k] = struct{}{}
	}
	start := time.Now()
	added := 0
	if wm > prev.watermark {
		added, err = db.scanNodeRegionKeys(ctx, codes, prev.watermark, wm, true, set)
		if err != nil {
			return nil, err
		}
	}
	if added == 0 {
		// Same membership: keep the encoded keys, advance the watermark.
		e := *prev
		e.watermark = max(wm, prev.watermark)
		e.refreshed = time.Now()
		return &e, nil
	}
	log.Printf("[nodes-region] %s: +%d nodes from obs %d..%d in %v",
		key, added, prev.watermark+1, wm, time.Since(start).Round(time.Millisecond))
	return newNodeRegionEntry(set, wm, prev.built)
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

// scanNodeRegionKeys adds to set every ADVERT from_pubkey observed by an
// observer in codes with lo < observations.id <= hi, returning how many were
// new. For a delta scan the join order is forced (CROSS JOIN) to drive from
// the observations rowid range; left to the planner, sqlite_stat1 (#2058)
// makes it start from the region's observers and walk their whole history.
func (db *DB) scanNodeRegionKeys(
	ctx context.Context, codes []string, lo, hi int64, delta bool, set map[string]struct{},
) (int, error) {
	placeholders := make([]string, len(codes))
	args := []any{payloadTypeAdvert}
	for i, c := range codes {
		placeholders[i] = "?"
		args = append(args, c)
	}
	args = append(args, lo, hi)

	joinCond := "obs.rowid = o.observer_idx"
	if !db.isV3() {
		joinCond = "obs.id = o.observer_id"
	}
	join := "JOIN"
	if delta {
		join = "CROSS JOIN"
	}
	// Only fixed fragments and "?" placeholders are formatted in; every value
	// is bound.
	//
	// #1143 / PR #38: from_pubkey is a dedicated column the ingestor fills at
	// write time for ADVERT rows, so this lookup does not have to
	// JSON_EXTRACT and parse decoded_json for every candidate row. Measured
	// against a live 4.9GB database, 9 interleaved rounds so concurrent
	// ingest cannot bias one variant: 934ms -> 835ms median (1.12x) for a
	// single-region filter, identical result set both ways. The win is purely
	// the avoided JSON parse — EXPLAIN QUERY PLAN is byte-identical before
	// and after, both driving off idx_transmissions_payload_type. Despite the
	// column being indexed, idx_transmissions_from_pubkey does not
	// participate in this plan at all.
	//
	// THE TRAP, for whoever touches this next: from_pubkey is only written
	// for ADVERTs that actually carry a pubkey — see the guard in
	// cmd/ingestor/db.go. A NULL here drops out of IN (...) silently, so a
	// write path that fills decoded_json but forgets from_pubkey would cost
	// nodes from this filter with no error to notice. That is safe today only
	// because it is measured to be: on live staging 25 of 62104 ADVERTs have
	// NULL from_pubkey, and all 25 have no pubKey in decoded_json either —
	// nothing is lost, because there was never a pubkey to record. A COALESCE
	// fallback to JSON_EXTRACT was measured and rejected: it rescued 0 rows,
	// and it parses decoded_json again for every NULL row, so one corrupt
	// ADVERT that the backfill has not reached yet fails the whole query with
	// "malformed JSON" (a 500 on /api/nodes?region=). Behind this cache that
	// is worse, not better: the failed refresh leaves the cached entry stale
	// for every later request too. Its speed is not the reason: on a 4.3GB
	// synthetic DB it times within noise of from_pubkey. If you add a new
	// write path for transmissions, populate from_pubkey there rather than
	// reintroducing a per-row JSON parse here. nodes_region_from_pubkey_test.go
	// locks the result set against the JSON_EXTRACT expression;
	// TestBackfillFromPubkey_* in cmd/ingestor locks the backfill's half of
	// the contract.
	q := fmt.Sprintf(`SELECT DISTINCT t.from_pubkey
		FROM observations o
		%[1]s transmissions t ON t.id = o.transmission_id
		%[1]s observers obs ON %[2]s
		WHERE t.payload_type = ?
		AND t.from_pubkey IS NOT NULL
		AND UPPER(TRIM(obs.iata)) IN (%[3]s)
		AND o.id > ? AND o.id <= ?`, join, joinCond, strings.Join(placeholders, ","))

	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("scan region keys: %w", err)
	}
	defer rows.Close()
	added := 0
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			return added, fmt.Errorf("scan region keys: %w", err)
		}
		if _, ok := set[pk]; !ok {
			set[pk] = struct{}{}
			added++
		}
	}
	if err := rows.Err(); err != nil {
		return added, fmt.Errorf("scan region keys: %w", err)
	}
	return added, nil
}
