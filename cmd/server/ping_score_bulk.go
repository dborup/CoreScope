package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// This file holds additive, read-only bulk-query helpers for Ping Scores
// Phase 4B: batched siblings of GetPacketPath and nearestPositionedNeighbor
// that answer the same question for many inputs in O(chunks) queries
// instead of O(N). None of these are wired into main.go, the recomputer, or
// the public API yet -- that's Phase 4C+. See buildPacketPathResponseFromReduction
// in db.go for the shared branch-assembly core these bulk helpers reuse so
// single-item and bulk logic cannot silently diverge.

// observationFingerprint is a cheap, coarse signal for "did new
// observations arrive for this transmission since we last scored it" --
// COUNT(*) and MAX(id) over that transmission's observations rows. It only
// detects INSERTs; it does NOT detect in-place UPDATEs to
// resolved_path/snr/rssi, nodes.lat/lon changes, or neighbor_edges changes
// -- those are only bounded by the separate deep-sweep mechanism from the
// Phase 4 design, not this fingerprint.
type observationFingerprint struct {
	Count int64
	MaxID int64
}

// observationFingerprintsBulk computes an observationFingerprint for every
// transmission id in txIDs, chunked at 499 bind parameters per query to
// stay under SQLite's default variable limit. A txID with zero observation
// rows is simply absent from the result map -- not an error, and distinct
// from an explicit Count:0 entry (which can't otherwise occur, since a
// transmission with zero rows never appears in the GROUP BY output at all).
func (db *DB) observationFingerprintsBulk(txIDs []int64) (map[int64]observationFingerprint, error) {
	result := make(map[int64]observationFingerprint, len(txIDs))
	if len(txIDs) == 0 {
		return result, nil
	}
	seen := make(map[int64]bool, len(txIDs))
	unique := make([]int64, 0, len(txIDs))
	for _, id := range txIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}

	const chunkSize = 499
	for i := 0; i < len(unique); i += chunkSize {
		end := i + chunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[i:end]
		placeholders := make([]byte, 0, len(chunk)*2)
		args := make([]interface{}, len(chunk))
		for j, id := range chunk {
			if j > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args[j] = id
		}
		query := "SELECT transmission_id, COUNT(*), MAX(id) FROM observations WHERE transmission_id IN (" + string(placeholders) + ") GROUP BY transmission_id"
		if err := func() error {
			rows, err := db.conn.Query(query, args...)
			if err != nil {
				return fmt.Errorf("observation fingerprint query: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var txID, count, maxID int64
				if err := rows.Scan(&txID, &count, &maxID); err != nil {
					return fmt.Errorf("observation fingerprint scan: %w", err)
				}
				result[txID] = observationFingerprint{Count: count, MaxID: maxID}
			}
			return rows.Err()
		}(); err != nil {
			return nil, fmt.Errorf("observation fingerprint iteration: %w", err)
		}
	}
	return result, nil
}

// nearestPositionedNeighborsBulk computes the same weighted-centroid
// position estimate nearestPositionedNeighbor would for each pubkey in
// pubkeys, in O(chunks) neighbor_edges queries instead of O(len(pubkeys)).
//
// Each chunk uses a VALUES-CTE joined by column, not by repeating the
// target list on both sides of the OR: `JOIN targets t ON (ne.node_a = t.pk
// OR ne.node_b = t.pk)` binds the chunk's N pubkeys exactly once (in the
// CTE's own VALUES list) -- the OR condition inside the join reads the
// CTE's already-bound column, it does not re-bind a parameter per
// reference. Verified empirically against modernc.org/sqlite before relying
// on it here (and re-verified against the shipped code by
// TestNearestPositionedNeighborsBulk_ParameterBudgetIsExactlyNPerChunk): a
// query built this way for N targets contains exactly N `?` placeholders
// and errors on any other argument count. Per-target top-20-by-count
// selection via a partitioned ROW_NUMBER() window, ordered `count DESC,
// neighbor ASC`, produces the identical rows a per-target
// `ORDER BY count DESC, neighbor ASC LIMIT 20` would -- the same secondary
// tie-break nearestPositionedNeighbor's own query now uses, so a target
// with more than 20 candidates and ties spanning the 20th-place cutoff
// still resolves to the same top 20 either way. This determinizes
// previously-undefined ordering; it does not preserve any order that was
// ever guaranteed before.
//
// A pubkey with no result (no edges, or no positioned contributor within
// maxEdgeKm of the strongest one) is simply absent from the returned map,
// exactly matching nearestPositionedNeighbor's ok=false -- not a
// zero-value entry.
//
// The candidate-position lookup inside each chunk (nodes matching the up-
// to-20-per-target neighbor pubkeys the ranked query returned) has its own
// independent chunk loop, separate from the targets chunking above: a
// single 499-target chunk can produce up to 499*20 = 9980 distinct
// candidate pubkeys, far past the 499-bind-parameter budget for one query.
func (db *DB) nearestPositionedNeighborsBulk(pubkeys []string, maxEdgeKm float64) (map[string]neighborEstimate, error) {
	result := make(map[string]neighborEstimate, len(pubkeys))
	if len(pubkeys) == 0 {
		return result, nil
	}
	seen := make(map[string]bool, len(pubkeys))
	unique := make([]string, 0, len(pubkeys))
	for _, pk := range pubkeys {
		npk := strings.ToLower(strings.TrimSpace(pk))
		if npk == "" || seen[npk] {
			continue
		}
		seen[npk] = true
		unique = append(unique, npk)
	}
	if len(unique) == 0 {
		return result, nil
	}

	const chunkSize = 499
	for i := 0; i < len(unique); i += chunkSize {
		end := i + chunkSize
		if end > len(unique) {
			end = len(unique)
		}
		if err := db.nearestPositionedNeighborsChunk(unique[i:end], maxEdgeKm, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// nearestPositionedNeighborsChunk resolves one chunk (<=499 targets) of
// nearestPositionedNeighborsBulk, writing results directly into result.
func (db *DB) nearestPositionedNeighborsChunk(targets []string, maxEdgeKm float64, result map[string]neighborEstimate) error {
	placeholders := make([]byte, 0, len(targets)*4)
	args := make([]interface{}, len(targets))
	for i, pk := range targets {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '(', '?', ')')
		args[i] = pk
	}
	query := `
		WITH targets(pk) AS (VALUES ` + string(placeholders) + `),
		edges AS (
			SELECT t.pk AS target,
				CASE WHEN ne.node_a = t.pk THEN ne.node_b ELSE ne.node_a END AS neighbor,
				ne.count AS count
			FROM neighbor_edges ne
			JOIN targets t ON (ne.node_a = t.pk OR ne.node_b = t.pk)
		),
		ranked AS (
			SELECT target, neighbor, count,
				ROW_NUMBER() OVER (PARTITION BY target ORDER BY count DESC, neighbor ASC) AS rn
			FROM edges
		)
		SELECT target, neighbor, count FROM ranked WHERE rn <= 20 ORDER BY target, rn`

	type candidate struct {
		pubkey string
		weight float64
	}
	candidatesByTarget := make(map[string][]candidate, len(targets))

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return fmt.Errorf("neighbor estimate bulk query: %w", err)
	}
	scanErr := func() error {
		defer rows.Close()
		for rows.Next() {
			var target, neighbor string
			var count float64
			if err := rows.Scan(&target, &neighbor, &count); err != nil {
				return fmt.Errorf("neighbor estimate bulk scan: %w", err)
			}
			candidatesByTarget[target] = append(candidatesByTarget[target], candidate{pubkey: neighbor, weight: count})
		}
		return rows.Err()
	}()
	if scanErr != nil {
		return fmt.Errorf("neighbor estimate bulk iteration: %w", scanErr)
	}
	if len(candidatesByTarget) == 0 {
		return nil
	}

	candidatePubkeySet := make(map[string]bool)
	for _, cs := range candidatesByTarget {
		for _, c := range cs {
			candidatePubkeySet[c.pubkey] = true
		}
	}
	candidatePubkeys := make([]string, 0, len(candidatePubkeySet))
	for pk := range candidatePubkeySet {
		candidatePubkeys = append(candidatePubkeys, pk)
	}

	type posInfo struct {
		name     string
		lat, lon float64
	}
	// candidatePubkeys came from a map (candidatePubkeySet), so it's already
	// deduped -- but it can hold up to len(targets)*20 entries, which for a
	// full 499-target chunk is up to 9980, far past one query's
	// packetPathNodeLookupChunkSize budget. Its own independent chunk loop
	// keeps each query's bind-parameter count within budget regardless of
	// how many targets this chunk covered.
	posByPK := make(map[string]posInfo, len(candidatePubkeys))
	for i := 0; i < len(candidatePubkeys); i += packetPathNodeLookupChunkSize {
		end := i + packetPathNodeLookupChunkSize
		if end > len(candidatePubkeys) {
			end = len(candidatePubkeys)
		}
		nodeChunk := candidatePubkeys[i:end]
		nodePlaceholders := make([]byte, 0, len(nodeChunk)*2)
		nodeArgs := make([]interface{}, len(nodeChunk))
		for j, pk := range nodeChunk {
			if j > 0 {
				nodePlaceholders = append(nodePlaceholders, ',')
			}
			nodePlaceholders = append(nodePlaceholders, '?')
			nodeArgs[j] = pk
		}
		nodeRows, err := db.conn.Query(
			"SELECT public_key, name, lat, lon FROM nodes WHERE public_key IN ("+string(nodePlaceholders)+") AND lat IS NOT NULL AND lon IS NOT NULL AND lat != 0 AND lon != 0", nodeArgs...)
		if err != nil {
			return fmt.Errorf("neighbor estimate bulk node query: %w", err)
		}
		scanErr := func() error {
			defer nodeRows.Close()
			for nodeRows.Next() {
				var pk string
				var name sql.NullString
				var lat, lon float64
				if err := nodeRows.Scan(&pk, &name, &lat, &lon); err != nil {
					return fmt.Errorf("neighbor estimate bulk node scan: %w", err)
				}
				posByPK[pk] = posInfo{name: name.String, lat: lat, lon: lon}
			}
			return nodeRows.Err()
		}()
		if scanErr != nil {
			return fmt.Errorf("neighbor estimate bulk node iteration: %w", scanErr)
		}
	}

	type weighted struct {
		posInfo
		weight float64
	}
	for target, candidates := range candidatesByTarget {
		var contributors []weighted
		for _, c := range candidates {
			p, found := posByPK[c.pubkey]
			if !found {
				continue
			}
			w := c.weight
			if w <= 0 {
				w = 1
			}
			contributors = append(contributors, weighted{posInfo: p, weight: w})
		}
		if len(contributors) == 0 {
			continue
		}

		// candidates is count-DESC ordered (via the ORDER BY target, rn
		// clause above, mirroring rn's PARTITION BY target ORDER BY count
		// DESC), so the first resolved contributor is the strongest --
		// both the geo-sanity anchor below and the returned name.
		if maxEdgeKm > 0 && len(contributors) > 1 {
			anchor := contributors[0]
			filtered := contributors[:1:1] // anchor always survives (distance to itself is 0)
			for _, c := range contributors[1:] {
				if haversineKm(anchor.lat, anchor.lon, c.lat, c.lon) <= maxEdgeKm {
					filtered = append(filtered, c)
				}
			}
			contributors = filtered
		}

		var sumLat, sumLon, sumWeight float64
		var strongestName string
		for _, c := range contributors {
			sumLat += c.lat * c.weight
			sumLon += c.lon * c.weight
			sumWeight += c.weight
			if strongestName == "" {
				strongestName = c.name
			}
		}
		var spread float64
		for i := 0; i < len(contributors); i++ {
			for j := i + 1; j < len(contributors); j++ {
				d := haversineKm(contributors[i].lat, contributors[i].lon, contributors[j].lat, contributors[j].lon)
				if d > spread {
					spread = d
				}
			}
		}
		result[target] = neighborEstimate{
			Name: strongestName, Lat: sumLat / sumWeight, Lon: sumLon / sumWeight,
			ContributorCount: len(contributors), SpreadKm: spread,
		}
	}
	return nil
}

// GetPacketPathsBulk computes the same PacketPathResponse GetPacketPath
// would for each hash in hashes. All branch-assembly logic is the single
// shared buildPacketPathResponseFromReduction (db.go), so the two can never
// silently diverge in output shape or field values.
//
// Query count is NOT independent of the input size in every dimension --
// it is O(hash-chunks + pubkey-chunks + name-chunks + neighbor-target-chunks
// + candidate-pubkey-chunks), each dimension chunked independently at
// packetPathNodeLookupChunkSize (nodes lookups) or the matching per-function
// chunk size (hashes, neighbor targets): (1) one query per chunk of hashes
// (<=499 per chunk) for the raw observation rows, grouped into a per-hash
// packetPathReduction as rows are scanned since SQLite returns them in
// query order, not pre-grouped by hash; (2) resolveNodesByPubkey, itself
// chunked, across the pubkey union of every hash's branches; (3)
// resolveNodesByName, itself chunked, across the observer-name-fallback
// union; (4) nearestPositionedNeighborsBulk, itself chunked on BOTH targets
// and (separately) resolved candidate pubkeys, across the union of every
// pubkey step 2/3 left unpositioned. What stays flat regardless of input
// size is the number of DISTINCT LOOKUPS (5, not one per hash/pubkey/name)
// -- see TestGetPacketPathsBulk_QueryCountIndependentOfHashCount, which
// holds each dimension's cardinality below its own chunk size and confirms
// query count doesn't grow with hash count under that condition; growing
// any one dimension past its chunk size adds more chunks for that
// dimension only.
//
// A hash with no observation rows at all (never observed, or unknown to
// this DB) is simply absent from the returned map -- not an error,
// matching GetPacketPath's own contract of returning an empty-Branches
// response rather than erroring for an unknown hash.
func (db *DB) GetPacketPathsBulk(hashes []string, maxEdgeKm float64) (map[string]*PacketPathResponse, error) {
	// result is created and the empty-input check runs BEFORE the
	// hasResolvedPath schema check on purpose: an empty request should
	// short-circuit to an empty, error-free result without touching the
	// database at all, even on a schema that lacks resolved_path entirely
	// (e.g. a legacy DB mid-migration) -- there is nothing to look up, so
	// there is nothing for that schema gap to affect.
	result := make(map[string]*PacketPathResponse, len(hashes))
	if len(hashes) == 0 {
		return result, nil
	}
	if !db.hasResolvedPath() {
		return nil, fmt.Errorf("resolved_path not available on this server")
	}

	seen := make(map[string]bool, len(hashes))
	normalized := make([]string, 0, len(hashes))
	for _, h := range hashes {
		lh := strings.ToLower(h)
		if seen[lh] {
			continue
		}
		seen[lh] = true
		normalized = append(normalized, lh)
	}

	var buildQuery func(placeholders string) string
	if db.isV3() {
		buildQuery = func(placeholders string) string {
			return `SELECT t.hash, obs.rowid, obs.id, obs.name, obs.iata, o.path_json, o.resolved_path, o.snr, o.timestamp, t.id, o.id
				FROM observations o
				JOIN transmissions t ON t.id = o.transmission_id
				LEFT JOIN observers obs ON obs.rowid = o.observer_idx
				WHERE t.hash IN (` + placeholders + `)`
		}
	} else {
		buildQuery = func(placeholders string) string {
			return `SELECT t.hash, o.observer_id, o.observer_id, o.observer_name, NULL, o.path_json, o.resolved_path, o.snr, o.timestamp, t.id, o.id
				FROM observations o
				JOIN transmissions t ON t.id = o.transmission_id
				WHERE t.hash IN (` + placeholders + `)`
		}
	}

	reductions := make(map[string]*packetPathReduction, len(normalized))
	const chunkSize = 499
	for i := 0; i < len(normalized); i += chunkSize {
		end := i + chunkSize
		if end > len(normalized) {
			end = len(normalized)
		}
		chunk := normalized[i:end]
		placeholders := make([]byte, 0, len(chunk)*2)
		args := make([]interface{}, len(chunk))
		for j, h := range chunk {
			if j > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args[j] = h
		}
		rows, err := db.conn.Query(buildQuery(string(placeholders)), args...)
		if err != nil {
			return nil, fmt.Errorf("packet path bulk query: %w", err)
		}
		scanErr := func() error {
			defer rows.Close()
			for rows.Next() {
				var rowHash string
				var obsKey, obsPubkey, obsName, obsIATA, pathJSON, resolvedPathJSON sql.NullString
				var snr sql.NullFloat64
				var ts sql.NullInt64
				var rowTxID sql.NullInt64
				var obsID int64
				if err := rows.Scan(&rowHash, &obsKey, &obsPubkey, &obsName, &obsIATA, &pathJSON, &resolvedPathJSON, &snr, &ts, &rowTxID, &obsID); err != nil {
					return fmt.Errorf("packet path bulk scan: %w", err)
				}
				rowHash = strings.ToLower(rowHash)
				red, ok := reductions[rowHash]
				if !ok {
					red = newPacketPathReduction()
					reductions[rowHash] = red
				}
				if rowTxID.Valid {
					red.txID = rowTxID.Int64
				}
				branch, key, ok := parsePacketPathObsRow(obsKey, obsPubkey, obsName, obsIATA, pathJSON, resolvedPathJSON, snr, ts, obsID)
				if !ok {
					continue
				}
				red.fold(key, branch, ts.Valid, ts.Int64)
			}
			return rows.Err()
		}()
		if scanErr != nil {
			return nil, fmt.Errorf("packet path bulk iteration: %w", scanErr)
		}
	}

	pubkeySet := make(map[string]bool)
	for _, red := range reductions {
		for pk := range collectPacketPathPubkeys(red.first, red.best) {
			pubkeySet[pk] = true
		}
	}
	pubkeys := make([]string, 0, len(pubkeySet))
	for pk := range pubkeySet {
		pubkeys = append(pubkeys, pk)
	}
	nodeByPK, err := db.resolveNodesByPubkey(pubkeys)
	if err != nil {
		return nil, fmt.Errorf("packet path bulk node lookup: %w", err)
	}

	nameFallbackSet := make(map[string]bool)
	for _, red := range reductions {
		for n := range collectPacketPathNameFallback(red.first, red.best, nodeByPK) {
			nameFallbackSet[n] = true
		}
	}
	names := make([]string, 0, len(nameFallbackSet))
	for n := range nameFallbackSet {
		names = append(names, n)
	}
	nodeByName, err := db.resolveNodesByName(names)
	if err != nil {
		return nil, fmt.Errorf("packet path bulk name lookup: %w", err)
	}

	fallbackSet := make(map[string]bool)
	for _, red := range reductions {
		for pk := range collectPacketPathFallbackCandidates(red.first, red.best, nodeByPK, nodeByName) {
			fallbackSet[pk] = true
		}
	}
	fallbackList := make([]string, 0, len(fallbackSet))
	for pk := range fallbackSet {
		fallbackList = append(fallbackList, pk)
	}
	estimates, err := db.nearestPositionedNeighborsBulk(fallbackList, maxEdgeKm)
	if err != nil {
		return nil, fmt.Errorf("packet path bulk neighbor estimate: %w", err)
	}
	neighborLookup := func(pk string) (neighborEstimate, bool) {
		e, ok := estimates[pk]
		return e, ok
	}

	for hash, red := range reductions {
		result[hash] = buildPacketPathResponseFromReduction(hash, red.txID, red.first, red.best, nodeByPK, nodeByName, neighborLookup)
	}
	return result, nil
}
