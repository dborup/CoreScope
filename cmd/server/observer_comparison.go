package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// GET /api/analytics/observer-comparison?ref=<id>&hours=1|6|24&observers=<ids> (#122).
//
// Reference-to-many observer comparison, descriptive only (no SNR, no health
// score -- see docs/api-spec.md). Fixes the browser-sample problem the
// existing pairwise compare.js overlap has (public/compare.js, #129/#1646):
// that mode requests at most 10,000 packets per observer over a hardcoded
// 24h window and intersects them client-side, so a busy observer is compared
// on an incomplete subset. This endpoint instead aggregates every eligible
// RX observation server-side, over the caller's chosen window, via a single
// indexed scan (scanObserverComparisonPairs) -- O(matched observations), not
// O(observers x packets) and not bounded by any packet-list page size.
const (
	// observerComparisonMaxObservers bounds how many "other" observers one
	// request may compare against the reference. The UI drives this from
	// the eligible-observer dropdown (a few dozen entries in practice); this
	// is a hard ceiling against a request asking for an unbounded comparison
	// set. Exceeding it is a 400, not a silent partial comparison (#122:
	// "never silently truncate the aggregate population").
	observerComparisonMaxObservers = 40

	// observerComparisonObserversParamMaxLen caps the raw "observers" query
	// value's length, rejected before it is split or looked up, so a
	// pathological string cannot drive any work at all.
	observerComparisonObserversParamMaxLen = 4096

	// observerComparisonIDMaxLen caps any single observer id (ref or one
	// entry of observers), rejected before lookup -- the same bound on a
	// hostile value as the raw-list cap above.
	observerComparisonIDMaxLen = 128

	observerComparisonCacheTTL     = 30 * time.Second
	observerComparisonCacheMaxKeys = 64
)

// observerComparisonScanRowLimit hard-caps the number of distinct (observer,
// transmission) RX pairs one request's scan may return. The window (<=24h)
// and observer count (<=observerComparisonMaxObservers) already bound the
// scan to recent, requested traffic rather than full table history; this is
// a backstop against one pathologically busy window. Hitting it fails the
// request closed with an explicit error (#122) instead of returning
// truncated counts that would look exact.
//
// var (not const), mirroring reachScanRowLimit (node_reach.go): tests lower
// it to exercise the limit without inserting a million rows.
var observerComparisonScanRowLimit = 1_000_000

// observerComparisonAllowedHours is the MVP's fixed window whitelist (#122
// "Offer 1-hour, 6-hour, and 24-hour windows initially").
var observerComparisonAllowedHours = map[int]bool{1: true, 6: true, 24: true}

// ObserverComparisonWindow is the exact UTC [Start, End) interval the
// comparison was computed over, based on observations.timestamp -- not
// transmissions.first_seen -- so a transmission re-observed inside the
// window counts even when it was first seen before it (#122).
type ObserverComparisonWindow struct {
	Hours int    `json:"hours"`
	Start string `json:"start"` // RFC3339 UTC, inclusive
	End   string `json:"end"`   // RFC3339 UTC, exclusive
}

// ObserverComparisonReference is the chosen reference observer and the size
// of its own RX set in the window (|R|).
type ObserverComparisonReference struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Total int    `json:"total"`
}

// ObserverComparisonRow is one other observer's overlap with the reference.
// *CoveragePct is nil when its denominator is zero -- #122: "return an
// unavailable percentage ... rather than presenting a misleading 0%".
type ObserverComparisonRow struct {
	ObserverID           string   `json:"observerId"`
	ObserverName         string   `json:"observerName"`
	Total                int      `json:"total"`                // |O|
	Shared               int      `json:"shared"`                // |S| = |R ∩ O|
	ReferenceOnly        int      `json:"referenceOnly"`         // |R|-|S|
	OtherOnly            int      `json:"otherOnly"`             // |O|-|S|
	ReferenceCoveragePct *float64 `json:"referenceCoveragePct"`  // 100*|S|/|R|
	OtherCoveragePct     *float64 `json:"otherCoveragePct"`      // 100*|S|/|O|
}

// ObserverComparisonResponse is the full response body. No map[string]any:
// every field is named (#122).
type ObserverComparisonResponse struct {
	Reference ObserverComparisonReference `json:"reference"`
	Window    ObserverComparisonWindow    `json:"window"`
	Rows      []ObserverComparisonRow     `json:"rows"`
}

// observerIdentity is one resolved, currently-active observers row.
type observerIdentity struct {
	RowID int64
	ID    string
	Name  string
}

// observationPair is one distinct (observer rowid, transmission id) RX
// observation inside a window -- the only shape the comparison needs.
type observationPair struct {
	ObserverRowID  int64
	TransmissionID int64
}

// resolveObserverIdentities looks up rowid/name for the given observer ids,
// restricted to active observers (inactive IS NULL OR inactive = 0 -- the
// same visibility GetObservers already applies). An id that does not exist
// or is inactive is simply absent from the result; callers decide what an
// absence means for the reference vs. the compared set.
func (db *DB) resolveObserverIdentities(ids []string) (map[string]observerIdentity, error) {
	if len(ids) == 0 {
		return map[string]observerIdentity{}, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.conn.Query(
		`SELECT rowid, id, name FROM observers WHERE id IN (`+placeholders+`) AND (inactive IS NULL OR inactive = 0)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]observerIdentity, len(ids))
	for rows.Next() {
		var rowID int64
		var id string
		var name sql.NullString
		if err := rows.Scan(&rowID, &id, &name); err != nil {
			return nil, err
		}
		displayName := id
		if name.Valid && name.String != "" {
			displayName = name.String
		}
		out[id] = observerIdentity{RowID: rowID, ID: id, Name: displayName}
	}
	return out, rows.Err()
}

// observerComparisonScanSQL is the window scan's statement, shared with the
// query-plan test so the plan that is asserted is the plan that runs.
func observerComparisonScanSQL(placeholders string) string {
	return `SELECT DISTINCT observer_idx, transmission_id FROM observations
		 WHERE observer_idx IN (` + placeholders + `)
		   AND timestamp >= ? AND timestamp < ?
		   AND UPPER(direction) = 'RX'`
}

// scanObserverComparisonPairs returns every distinct (observer_idx,
// transmission_id) RX observation for the given observer rowids inside
// [start, end). It reads the composite (observer_idx, timestamp) index
// (migration obs_observer_ts_idx_v1), so the scan is bounded by the
// requested window and observer set, not by total table size (#122 perf).
//
// "RX" is matched case-insensitively; NULL or any other value (including
// "TX") is excluded -- #122: an unknown direction must never silently count
// as RX.
func (db *DB) scanObserverComparisonPairs(rowIDs []int64, start, end time.Time) ([]observationPair, error) {
	placeholders := strings.Repeat("?,", len(rowIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, 0, len(rowIDs)+2)
	for _, id := range rowIDs {
		args = append(args, id)
	}
	args = append(args, start.Unix(), end.Unix())

	rows, err := db.conn.Query(observerComparisonScanSQL(placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]observationPair, 0, 1024)
	for rows.Next() {
		if len(out) >= observerComparisonScanRowLimit {
			return nil, fmt.Errorf(
				"observer comparison window too large: more than %d matching observations; narrow the window or reduce the compared observers",
				observerComparisonScanRowLimit)
		}
		var p observationPair
		if err := rows.Scan(&p.ObserverRowID, &p.TransmissionID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// computeObserverComparison aggregates scanned RX pairs into per-observer
// overlap rows against the reference. Two linear passes over pairs --
// O(len(pairs)), independent of how many "other" observers are compared.
// There is no per-observer re-scan, so this is not O(observers x packets).
func computeObserverComparison(refRowID int64, others []observerIdentity, pairs []observationPair) (refTotal int, rows []ObserverComparisonRow) {
	refSet := make(map[int64]struct{}, len(pairs))
	for _, p := range pairs {
		if p.ObserverRowID == refRowID {
			refSet[p.TransmissionID] = struct{}{}
		}
	}
	refTotal = len(refSet)

	total := make(map[int64]int, len(others))
	shared := make(map[int64]int, len(others))
	for _, p := range pairs {
		if p.ObserverRowID == refRowID {
			continue
		}
		total[p.ObserverRowID]++
		if _, ok := refSet[p.TransmissionID]; ok {
			shared[p.ObserverRowID]++
		}
	}

	rows = make([]ObserverComparisonRow, 0, len(others))
	for _, o := range others {
		s := shared[o.RowID]
		t := total[o.RowID]
		row := ObserverComparisonRow{
			ObserverID:    o.ID,
			ObserverName:  o.Name,
			Total:         t,
			Shared:        s,
			ReferenceOnly: refTotal - s,
			OtherOnly:     t - s,
		}
		if refTotal > 0 {
			v := roundPct(float64(s) / float64(refTotal))
			row.ReferenceCoveragePct = &v
		}
		if t > 0 {
			v := roundPct(float64(s) / float64(t))
			row.OtherCoveragePct = &v
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if ni, nj := strings.ToLower(rows[i].ObserverName), strings.ToLower(rows[j].ObserverName); ni != nj {
			return ni < nj
		}
		return rows[i].ObserverID < rows[j].ObserverID
	})
	return refTotal, rows
}

// roundPct rounds a 0..1 fraction to a percentage with one decimal place,
// matching the existing pairwise compare.js computeOverlapStats rounding.
func roundPct(frac float64) float64 {
	return math.Round(frac*1000) / 10
}

// observerComparisonCacheEntry is one cached response, with its own expiry.
type observerComparisonCacheEntry struct {
	resp    *ObserverComparisonResponse
	expires time.Time
}

// observerComparisonCache is usable as a zero value. Keyed by
// observerComparisonCacheKey (ref|hours|sorted other ids); bounded to
// observerComparisonCacheMaxKeys entries following the #430 pattern
// (evict expired first, then the one nearest expiry), with concurrent
// misses for one key sharing a single computation (singleflight). A stored
// response is never mutated after put, so it is safe to hand the same
// pointer to every caller of a cache hit.
type observerComparisonCache struct {
	mu      sync.Mutex
	entries map[string]*observerComparisonCacheEntry
	flights singleflight.Group
}

func (c *observerComparisonCache) get(key string, now time.Time) (*ObserverComparisonResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || !now.Before(e.expires) {
		return nil, false
	}
	return e.resp, true
}

func (c *observerComparisonCache) put(key string, resp *ObserverComparisonResponse, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*observerComparisonCacheEntry)
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= observerComparisonCacheMaxKeys {
		for k, old := range c.entries {
			if !now.Before(old.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= observerComparisonCacheMaxKeys {
			victim, first := "", true
			var soonest time.Time
			for k, old := range c.entries {
				switch {
				case first, old.expires.Before(soonest):
					victim, soonest, first = k, old.expires, false
				case old.expires.Equal(soonest) && k < victim:
					victim = k
				}
			}
			delete(c.entries, victim)
		}
	}
	c.entries[key] = &observerComparisonCacheEntry{resp: resp, expires: now.Add(observerComparisonCacheTTL)}
}

// len reports the current entry count (tests).
func (c *observerComparisonCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// reset drops every entry (tests).
func (c *observerComparisonCache) reset() {
	c.mu.Lock()
	c.entries = nil
	c.mu.Unlock()
}

// load returns the cached response for key or, on a miss, the result of one
// computation shared by every concurrent caller of key (#122 singleflight).
// The cache is re-checked inside the flight so a caller that missed just
// before another flight stored a fresh entry does not recompute.
func (c *observerComparisonCache) load(key string, now time.Time, compute func() (*ObserverComparisonResponse, error)) (*ObserverComparisonResponse, error) {
	if resp, ok := c.get(key, now); ok {
		return resp, nil
	}
	v, err, _ := c.flights.Do(key, func() (interface{}, error) {
		if resp, ok := c.get(key, time.Now()); ok {
			return resp, nil
		}
		resp, err := compute()
		if err != nil {
			return nil, err
		}
		c.put(key, resp, time.Now())
		return resp, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*ObserverComparisonResponse), nil
}

// observerComparisonCacheKey builds the cache/flight key for one request.
// Bounded to observerComparisonCacheMaxKeyBytes: a longer key is stored (and
// used as the flight key) as its SHA-256 digest, mirroring
// channelListStoreKey (channels_list_cache.go).
const observerComparisonCacheMaxKeyBytes = 512

func observerComparisonCacheKey(refID string, hours int, others []observerIdentity) string {
	ids := make([]string, len(others))
	for i, o := range others {
		ids[i] = o.ID
	}
	sort.Strings(ids)
	key := refID + "|" + strconv.Itoa(hours) + "|" + strings.Join(ids, ",")
	if len(key) <= observerComparisonCacheMaxKeyBytes {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// parseObserverComparisonObserverIDs splits and de-duplicates the
// "observers" query value. Returns an error if refID appears in the list
// (the reference must not compare against itself -- the same invariant
// public/compare.js already enforces for the two-observer mode via
// isComparisonReady).
func parseObserverComparisonObserverIDs(raw, refID string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	seen := make(map[string]bool, 8)
	out := make([]string, 0, 8)
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if len(id) > observerComparisonIDMaxLen {
			return nil, errors.New("observer id too long")
		}
		if id == refID {
			return nil, errors.New("observers must not include the reference observer")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// handleObserverComparison implements GET /api/analytics/observer-comparison
// (#122). All parameter validation happens before any cache lookup, DB
// query, or singleflight -- the #430 "reject before work" pattern.
func (s *Server) handleObserverComparison(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || !s.db.isV3() {
		writeError(w, 400, "observer comparison requires the v3 observations schema")
		return
	}

	refID := strings.TrimSpace(r.URL.Query().Get("ref"))
	if refID == "" {
		writeError(w, 400, "ref is required")
		return
	}
	if len(refID) > observerComparisonIDMaxLen {
		writeError(w, 400, "ref too long")
		return
	}

	hours, err := strconv.Atoi(r.URL.Query().Get("hours"))
	if err != nil || !observerComparisonAllowedHours[hours] {
		writeError(w, 400, "hours must be one of 1, 6, 24")
		return
	}

	observersRaw := r.URL.Query().Get("observers")
	if len(observersRaw) > observerComparisonObserversParamMaxLen {
		writeError(w, 400, "observers filter too long")
		return
	}
	otherIDs, err := parseObserverComparisonObserverIDs(observersRaw, refID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if len(otherIDs) == 0 {
		writeError(w, 400, "observers is required (comma-separated observer ids)")
		return
	}
	if len(otherIDs) > observerComparisonMaxObservers {
		writeError(w, 400, fmt.Sprintf("too many observers: %d requested, %d max", len(otherIDs), observerComparisonMaxObservers))
		return
	}

	if s.cfg != nil && s.cfg.IsObserverBlacklisted(refID) {
		writeError(w, 404, "observer not found")
		return
	}

	allIDs := make([]string, 0, len(otherIDs)+1)
	allIDs = append(allIDs, refID)
	allIDs = append(allIDs, otherIDs...)
	identities, err := s.db.resolveObserverIdentities(allIDs)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ref, ok := identities[refID]
	if !ok {
		writeError(w, 404, "observer not found")
		return
	}

	others := make([]observerIdentity, 0, len(otherIDs))
	for _, id := range otherIDs {
		o, ok := identities[id]
		if !ok {
			continue // unknown or inactive -- silently excluded (visibility policy, not an error)
		}
		if s.cfg != nil && s.cfg.IsObserverBlacklisted(id) {
			continue
		}
		others = append(others, o)
	}
	if len(others) == 0 {
		writeError(w, 400, "no eligible observers to compare")
		return
	}

	cacheKey := observerComparisonCacheKey(refID, hours, others)
	resp, err := s.obsComparison.load(cacheKey, time.Now(), func() (*ObserverComparisonResponse, error) {
		return s.computeObserverComparisonResponse(ref, others, hours)
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, resp)
}

// computeObserverComparisonResponse runs the DB scan and builds the typed
// response for one (reference, others, hours) request. Not cached itself --
// handleObserverComparison wraps it in s.obsComparison.load.
func (s *Server) computeObserverComparisonResponse(ref observerIdentity, others []observerIdentity, hours int) (*ObserverComparisonResponse, error) {
	end := time.Now().UTC().Truncate(time.Second)
	start := end.Add(-time.Duration(hours) * time.Hour)

	rowIDs := make([]int64, 0, len(others)+1)
	rowIDs = append(rowIDs, ref.RowID)
	for _, o := range others {
		rowIDs = append(rowIDs, o.RowID)
	}

	pairs, err := s.db.scanObserverComparisonPairs(rowIDs, start, end)
	if err != nil {
		return nil, err
	}

	refTotal, rows := computeObserverComparison(ref.RowID, others, pairs)

	return &ObserverComparisonResponse{
		Reference: ObserverComparisonReference{ID: ref.ID, Name: ref.Name, Total: refTotal},
		Window: ObserverComparisonWindow{
			Hours: hours,
			Start: start.Format(time.RFC3339),
			End:   end.Format(time.RFC3339),
		},
		Rows: rows,
	}, nil
}
