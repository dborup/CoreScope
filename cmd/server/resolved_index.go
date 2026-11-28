package main

// Lock ordering contract (MUST be followed everywhere):
//
//   s.mu  →  s.lruMu   (s.mu is the outer lock, lruMu is the inner lock)
//
// • Never acquire s.lruMu while holding s.mu.
// • fetchResolvedPathForObs takes lruMu independently — callers under s.mu
//   must NOT call it directly; instead collect IDs under s.mu, release, then
//   do LRU ops under lruMu separately.

import (
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

type resolvedPathObservation struct {
	id       int
	pathJSON string
}

// A job owns immutable lookup inputs and a response setter, not a StoreTx or
// a shared observation slice. Setters only mutate this request's response.
type resolvedPathJob struct {
	forTx        bool
	txID         int
	observations []resolvedPathObservation
	obsID        int
	set          func([]*string)
}

func (s *PacketStore) applyResolvedPathJobs(jobs []resolvedPathJob) {
	for _, job := range jobs {
		var rp []*string
		if job.forTx {
			rp = s.resolvedPathForSnapshot(job.txID, job.observations)
		} else {
			rp = s.fetchResolvedPathForObs(job.obsID)
		}
		if rp != nil {
			job.set(rp)
		}
	}
}

// Same longest-present-path choice and LRU behavior as the live-store helper,
// but all inputs were copied under s.mu before SQL is attempted.
func (s *PacketStore) resolvedPathForSnapshot(txID int, observations []resolvedPathObservation) []*string {
	return s.bestResolvedPathAt(txID, len(observations), func(i int) resolvedPathObservation { return observations[i] }, s.lruNow())
}

func (s *PacketStore) bestResolvedPathAt(txID, count int, at func(int) resolvedPathObservation, now int64) []*string {
	if count == 0 {
		return nil
	}
	longest := at(0)
	for i := 1; i < count; i++ {
		obs := at(i)
		if pathLen(obs.pathJSON) > pathLen(longest.pathJSON) {
			longest = obs
		}
	}
	if rp := s.fetchResolvedPathForObsAt(longest.id, now); rp != nil {
		return rp
	}
	paths := s.fetchResolvedPathsForTx(txID)
	best, bestID := longestStoredResolvedPath(count, at, paths)
	if bestID != 0 {
		s.lruMu.Lock()
		s.lruPut(bestID, best)
		s.lruMu.Unlock()
	}
	return best
}

// longestStoredResolvedPath preserves the scalar helper's first-in-snapshot
// tie rule. Rows added after the snapshot never become candidates.
func longestStoredResolvedPath(count int, at func(int) resolvedPathObservation, stored map[int][]*string) ([]*string, int) {
	var best []*string
	bestLen, bestID := -1, 0
	for i := 0; i < count; i++ {
		obs := at(i)
		if rp := stored[obs.id]; rp != nil && pathLen(obs.pathJSON) > bestLen {
			best, bestLen, bestID = rp, pathLen(obs.pathJSON), obs.id
		}
	}
	return best, bestID
}

const resolvedPathBatchSize = 500 // below SQLite's older 999-bound-variable limit

// loadCanonicalResolvedPathsAt is the batched form of BestAt for /paths.
// The observation slices are immutable append-only snapshots captured under
// s.mu. SQL and LRU access happen only after that lock has been released.
// The supplied clock preserves the existing once-per-request TTL decision.
func (s *PacketStore) loadCanonicalResolvedPathsAt(snapshots map[int][]*StoreObs, now int64) map[int][]*string {
	out := make(map[int][]*string, len(snapshots))
	if s.db == nil || s.db.conn == nil {
		return out
	}
	// obs id -> every tx whose longest observation it is; the same id can
	// be the longest of several txs, and each must be answered.
	longestTx := make(map[int][]int, len(snapshots))
	missing := make([]int, 0, len(snapshots))
	fallback := make([]int, 0)
	for txID, observations := range snapshots {
		if len(observations) == 0 {
			continue
		}
		longest, longestLen := observations[0], pathLen(observations[0].PathJSON)
		for _, obs := range observations[1:] {
			if l := pathLen(obs.PathJSON); l > longestLen {
				longest, longestLen = obs, l
			}
		}
		s.lruMu.RLock()
		rp, cached := s.lruGet(longest.ID, now)
		s.lruMu.RUnlock()
		if cached {
			if rp != nil {
				out[txID] = rp
			} else {
				fallback = append(fallback, txID)
			}
			continue
		}
		if _, queued := longestTx[longest.ID]; !queued {
			missing = append(missing, longest.ID)
		}
		longestTx[longest.ID] = append(longestTx[longest.ID], txID)
	}
	// Ascending ids make chunk membership and LRU insert order independent of
	// map iteration: when one request inserts past lruMaxSize, FIFO eviction
	// drops the lowest ids and the highest survive (#548).
	sort.Ints(missing)
	for start := 0; start < len(missing); start += resolvedPathBatchSize {
		chunk := missing[start:min(start+resolvedPathBatchSize, len(missing))]
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.conn.Query(`SELECT id, resolved_path FROM observations WHERE id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		stored := make(map[int][]*string, len(chunk))
		if err == nil {
			for rows.Next() {
				var id int
				var raw sql.NullString
				if scanErr := rows.Scan(&id, &raw); scanErr != nil {
					err = scanErr
					break
				}
				if raw.Valid {
					stored[id] = unmarshalResolvedPath(raw.String)
				}
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
		putIDs := make([]int, 0, len(chunk))
		putRPs := make([][]*string, 0, len(chunk))
		for _, id := range chunk {
			rp, present := stored[id]
			// Only a row that exists with a non-NULL resolved_path is cached;
			// an absent row or NULL must stay a miss (#548).
			if err == nil && present {
				putIDs = append(putIDs, id)
				putRPs = append(putRPs, rp)
			}
			for _, txID := range longestTx[id] {
				if err != nil {
					// Retain the scalar error/recovery behavior for a failed batch.
					if rp := s.fetchResolvedPathForTxBestAt(txID, snapshots[txID], now); rp != nil {
						out[txID] = rp
					}
					continue
				}
				if rp != nil {
					out[txID] = rp
				} else {
					fallback = append(fallback, txID)
				}
			}
		}
		s.lruPutBatch(putIDs, putRPs)
	}
	s.loadFallbackResolvedPathsAt(fallback, snapshots, out, now)
	return out
}

func (s *PacketStore) loadFallbackResolvedPathsAt(txIDs []int, snapshots map[int][]*StoreObs, out map[int][]*string, now int64) {
	sort.Ints(txIDs)
	for start := 0; start < len(txIDs); start += resolvedPathBatchSize {
		chunk := txIDs[start:min(start+resolvedPathBatchSize, len(txIDs))]
		args := make([]interface{}, len(chunk))
		wanted := make(map[int]int)
		for i, id := range chunk {
			args[i] = id
			for _, obs := range snapshots[id] {
				wanted[obs.ID] = id
			}
		}
		rows, err := s.db.conn.Query(`SELECT id, transmission_id, resolved_path FROM observations
			WHERE resolved_path IS NOT NULL AND transmission_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		stored := make(map[int]map[int][]*string, len(chunk))
		if err == nil {
			for rows.Next() {
				var obsID, txID int
				var raw sql.NullString
				if scanErr := rows.Scan(&obsID, &txID, &raw); scanErr != nil {
					err = scanErr
					break
				}
				if !raw.Valid || raw.String == "" {
					continue
				}
				// Historical rows absent from the immutable snapshot cannot
				// win. Do not decode or retain their paths in this request.
				if owner, ok := wanted[obsID]; !ok || owner != txID {
					continue
				}
				if stored[txID] == nil {
					stored[txID] = make(map[int][]*string)
				}
				stored[txID][obsID] = unmarshalResolvedPath(raw.String)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
		putIDs := make([]int, 0, len(chunk))
		putRPs := make([][]*string, 0, len(chunk))
		for _, txID := range chunk {
			if err != nil {
				if rp := s.fetchResolvedPathForTxBestAt(txID, snapshots[txID], now); rp != nil {
					out[txID] = rp
				}
				continue
			}
			obs := snapshots[txID]
			rp, id := longestStoredResolvedPath(len(obs), func(i int) resolvedPathObservation {
				return resolvedPathObservation{id: obs[i].ID, pathJSON: obs[i].PathJSON}
			}, stored[txID])
			if rp != nil {
				out[txID] = rp
				if id != 0 {
					putIDs = append(putIDs, id)
					putRPs = append(putRPs, rp)
				}
			}
		}
		s.lruPutBatch(putIDs, putRPs)
	}
}

// resolvedPubkeyHash computes a fast 64-bit hash for membership index keying.
// Uses FNV-1a from stdlib — good distribution, no external dependency.
func resolvedPubkeyHash(pk string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(strings.ToLower(pk)))
	return h.Sum64()
}

// addToResolvedPubkeyIndex adds a txID under each resolved pubkey hash.
// Deduplicates both within a single call AND across calls — won't add the
// same (hash, txID) pair twice even when called multiple times for the same tx.
// Must be called under s.mu write lock.
func (s *PacketStore) addToResolvedPubkeyIndex(txID int, resolvedPubkeys []string) {
	if !s.useResolvedPathIndex {
		return
	}
	seen := make(map[uint64]bool, len(resolvedPubkeys))
	for _, pk := range resolvedPubkeys {
		if pk == "" {
			continue
		}
		h := resolvedPubkeyHash(pk)
		if seen[h] {
			continue
		}
		seen[h] = true

		// Cross-call dedup: check if (h, txID) already exists in forward index.
		existing := s.resolvedPubkeyIndex[h]
		alreadyPresent := false
		for _, id := range existing {
			if id == txID {
				alreadyPresent = true
				break
			}
		}
		if alreadyPresent {
			continue
		}

		s.resolvedPubkeyIndex[h] = append(existing, txID)
		s.resolvedPubkeyReverse[txID] = append(s.resolvedPubkeyReverse[txID], h)
	}
}

// removeFromResolvedPubkeyIndex removes all index entries for a txID using the reverse map.
// Must be called under s.mu write lock.
func (s *PacketStore) removeFromResolvedPubkeyIndex(txID int) {
	if !s.useResolvedPathIndex {
		return
	}
	hashes := s.resolvedPubkeyReverse[txID]
	for _, h := range hashes {
		list := s.resolvedPubkeyIndex[h]
		// Remove ALL occurrences of txID (not just the first) to prevent orphans.
		filtered := list[:0]
		for _, id := range list {
			if id != txID {
				filtered = append(filtered, id)
			}
		}
		if len(filtered) == 0 {
			delete(s.resolvedPubkeyIndex, h)
		} else {
			s.resolvedPubkeyIndex[h] = filtered
		}
	}
	delete(s.resolvedPubkeyReverse, txID)
}

// extractResolvedPubkeys extracts all non-nil, non-empty pubkeys from a resolved path.
func extractResolvedPubkeys(rp []*string) []string {
	if len(rp) == 0 {
		return nil
	}
	result := make([]string, 0, len(rp))
	for _, p := range rp {
		if p != nil && *p != "" {
			result = append(result, *p)
		}
	}
	return result
}

// mergeResolvedPubkeys collects unique non-empty pubkeys from multiple resolved paths.
func mergeResolvedPubkeys(paths ...[]*string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, rp := range paths {
		for _, p := range rp {
			if p != nil && *p != "" && !seen[*p] {
				seen[*p] = true
				result = append(result, *p)
			}
		}
	}
	return result
}

// nodeInResolvedPathViaIndex checks whether a transmission is associated with
// a target pubkey using the membership index + collision-safety SQL check.
// Must be called under s.mu RLock at minimum.
func (s *PacketStore) nodeInResolvedPathViaIndex(tx *StoreTx, targetPK string) bool {
	if !s.useResolvedPathIndex {
		// Flag off: can't disambiguate, keep candidate (conservative)
		return true
	}

	// If this tx has no indexed pubkeys at all, we can't disambiguate —
	// keep the candidate (same as old behavior for NULL resolved_path).
	if _, hasReverse := s.resolvedPubkeyReverse[tx.ID]; !hasReverse {
		return true
	}

	h := resolvedPubkeyHash(targetPK)
	txIDs := s.resolvedPubkeyIndex[h]

	// Check if this tx's ID is in the candidate list
	for _, id := range txIDs {
		if id == tx.ID {
			// Found in index. Collision-safety: verify with SQL.
			if s.db != nil && s.db.conn != nil {
				return s.confirmResolvedPathContains(tx.ID, targetPK)
			}
			return true // no DB, trust the index
		}
	}

	return false
}

// confirmResolvedPathContains verifies an exact pubkey match in resolved_path
// via SQL. This is the collision-safety fallback for the membership index.
func (s *PacketStore) confirmResolvedPathContains(txID int, pubkey string) bool {
	if s.db == nil || s.db.conn == nil {
		return true
	}
	s.confirmResolvedPathQueries.Add(1)
	// Use INSTR with surrounding quotes for exact match — avoids LIKE escape issues.
	// resolved_path format: ["pubkey1","pubkey2",...]
	needle := `"` + strings.ToLower(pubkey) + `"`
	var count int
	err := s.db.conn.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE transmission_id = ? AND INSTR(LOWER(resolved_path), ?) > 0`,
		txID, needle,
	).Scan(&count)
	if err != nil {
		return true // on error, keep the candidate
	}
	return count > 0
}

// fetchResolvedPathsForTx fetches resolved_path from SQLite for all observations
// of a transmission. Used for on-demand API responses and eviction cleanup.
func (s *PacketStore) fetchResolvedPathsForTx(txID int) map[int][]*string {
	if s.db == nil || s.db.conn == nil {
		return nil
	}
	rows, err := s.db.conn.Query(
		`SELECT id, resolved_path FROM observations WHERE transmission_id = ? AND resolved_path IS NOT NULL`,
		txID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	result := make(map[int][]*string)
	for rows.Next() {
		var obsID int
		var rpJSON sql.NullString
		if err := rows.Scan(&obsID, &rpJSON); err != nil {
			continue
		}
		if rpJSON.Valid && rpJSON.String != "" {
			result[obsID] = unmarshalResolvedPath(rpJSON.String)
		}
	}
	return result
}

// fetchResolvedPathsForObsIDs reads resolved_path for many observations in
// one query (#165, grouped packet pages of up to 50K rows): the ids go in as
// one JSON array parameter, so there is no bound-variable limit and one
// statement to prepare. Values come back as the stored JSON, unparsed
// (resolvedPathRaw), because the caller only re-encodes them. Id 0 is
// skipped. It bypasses the LRU on purpose: a page is far larger than
// lruMaxSize and would evict every entry other endpoints rely on.
func (s *PacketStore) fetchResolvedPathsForObsIDs(ids []int) map[int]json.RawMessage {
	out := make(map[int]json.RawMessage)
	if s.db == nil || s.db.conn == nil || !s.db.hasResolvedPath() || len(ids) == 0 {
		return out
	}
	var b strings.Builder
	b.Grow(len(ids) * 8)
	b.WriteByte('[')
	n := 0
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(id))
		n++
	}
	b.WriteByte(']')
	if n == 0 {
		return out
	}
	rows, err := s.db.conn.Query(`SELECT o.id, o.resolved_path FROM observations o
		WHERE o.id IN (SELECT value FROM json_each(?)) AND o.resolved_path IS NOT NULL`, b.String())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var rpJSON sql.NullString
		if rows.Scan(&id, &rpJSON) == nil && rpJSON.Valid {
			if rp := resolvedPathRaw(rpJSON.String); rp != nil {
				out[id] = rp
			}
		}
	}
	return out
}

// resolvedPathRaw returns a stored resolved_path as JSON to embed in a
// response, or nil when it is empty or not a JSON array (an invalid
// RawMessage would fail the whole response's encoding).
func resolvedPathRaw(s string) json.RawMessage {
	if len(s) < 2 || s[0] != '[' || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

// fetchResolvedPathForObs fetches resolved_path for a single observation,
// using the LRU cache.
func (s *PacketStore) fetchResolvedPathForObs(obsID int) []*string {
	return s.fetchResolvedPathForObsAt(obsID, s.lruNow())
}

// fetchResolvedPathForObsAt is fetchResolvedPathForObs with the LRU clock
// read by the caller (lruNow), so a loop over many candidates reads it once.
func (s *PacketStore) fetchResolvedPathForObsAt(obsID int, now int64) []*string {
	if s.db == nil || s.db.conn == nil {
		return nil
	}

	// Check LRU cache first
	s.lruMu.RLock()
	if rp, ok := s.lruGet(obsID, now); ok {
		s.lruMu.RUnlock()
		return rp
	}
	s.lruMu.RUnlock()

	var rpJSON sql.NullString
	err := s.db.conn.QueryRow(
		`SELECT resolved_path FROM observations WHERE id = ?`, obsID,
	).Scan(&rpJSON)
	if err != nil || !rpJSON.Valid {
		return nil
	}
	rp := unmarshalResolvedPath(rpJSON.String)

	// Store in LRU
	s.lruMu.Lock()
	s.lruPut(obsID, rp)
	s.lruMu.Unlock()

	return rp
}

// fetchResolvedPathForTxBest returns the best observation's resolved_path for a tx.
//
// "Best" = the longest path_json among observations that actually have a stored
// resolved_path. Earlier versions picked the longest-path obs unconditionally
// and queried SQL for that single ID — if the longest-path obs had NULL
// resolved_path while a shorter sibling had one, the call returned nil and
// callers (e.g. /api/nodes/{pk}/health.recentPackets) lost the field. Fixes
// #810 by checking all observations and falling back to the longest sibling
// that has a stored path.
func (s *PacketStore) fetchResolvedPathForTxBest(tx *StoreTx) []*string {
	if tx == nil {
		return nil
	}
	return s.fetchResolvedPathForTxBestAt(tx.ID, tx.Observations, s.lruNow())
}

// fetchResolvedPathForTxBestAt is fetchResolvedPathForTxBest for a tx given by
// its id and observations, with the LRU clock read by the caller. /paths and
// /hop_analytics call it once per candidate; reading the clock once per
// request keeps a cache hit as cheap as it was before entries had an age
// (#277). They call it outside s.mu (it runs SQL and takes lruMu, #1278), so
// they pass the tx's observations as read under s.mu: ingest appends to
// tx.Observations under that lock (#470). Ingest only appends, so the
// elements of such a snapshot never change.
func (s *PacketStore) fetchResolvedPathForTxBestAt(txID int, observations []*StoreObs, now int64) []*string {
	return s.bestResolvedPathAt(txID, len(observations), func(i int) resolvedPathObservation {
		return resolvedPathObservation{id: observations[i].ID, pathJSON: observations[i].PathJSON}
	}, now)
}

// --- Simple LRU cache for resolved paths ---

const lruMaxSize = 10000

// resolvedPathLRUTTL bounds how long a cached resolved_path is served without
// re-reading the row (#277). The ingestor's observation upsert can replace a
// stored resolved_path in place (same row id), and the server's poll loop only
// reads new ids, so it cannot invalidate the entry when that happens. An
// expired entry is a cache miss: the next lookup costs one primary-key read.
const resolvedPathLRUTTL = 60 * time.Second

type resolvedPathLRUEntry struct {
	rp       []*string
	storedAt int64 // lruNow() at insert or refresh
}

// lruEpoch anchors lruNow. time.Since reads the monotonic clock only, so
// entry ages are immune to wall-clock steps and cost one clock read.
var lruEpoch = time.Now()

// lruNow is the LRU clock: nanoseconds since lruEpoch. The lruClock test hook
// stands in for time.Now and is measured from the same epoch, so installing it
// while entries exist keeps their ages.
func (s *PacketStore) lruNow() int64 {
	if s.lruClock != nil {
		return int64(s.lruClock().Sub(lruEpoch))
	}
	return int64(time.Since(lruEpoch))
}

// lruGet returns the cached path unless the entry is older than
// resolvedPathLRUTTL at now. now may be read before the lookup (once per
// request), so an entry stored after it is fresh. Must be called under
// s.lruMu (read or write).
func (s *PacketStore) lruGet(obsID int, now int64) ([]*string, bool) {
	e, ok := s.apiResolvedPathLRU[obsID]
	if !ok || now-e.storedAt > int64(resolvedPathLRUTTL) {
		return nil, false
	}
	return e.rp, true
}

// lruPut adds an entry, or refreshes an existing one in place (keeping its
// FIFO slot, so lruOrder gets no duplicate). Must be called under s.lruMu
// write lock.
func (s *PacketStore) lruPut(obsID int, rp []*string) {
	if s.apiResolvedPathLRU == nil {
		return
	}
	entry := resolvedPathLRUEntry{rp: rp, storedAt: s.lruNow()}
	if _, exists := s.apiResolvedPathLRU[obsID]; exists {
		s.apiResolvedPathLRU[obsID] = entry
		return
	}
	// Compact lruOrder if stale entries exceed 50% of capacity.
	// This prevents effective capacity degradation after bulk deletions.
	if len(s.lruOrder) >= lruMaxSize && len(s.apiResolvedPathLRU) < lruMaxSize/2 {
		compacted := make([]int, 0, len(s.apiResolvedPathLRU))
		for _, id := range s.lruOrder {
			if _, ok := s.apiResolvedPathLRU[id]; ok {
				compacted = append(compacted, id)
			}
		}
		s.lruOrder = compacted
	}
	if len(s.lruOrder) >= lruMaxSize {
		// Evict oldest, skipping stale entries
		for len(s.lruOrder) > 0 {
			evictID := s.lruOrder[0]
			s.lruOrder = s.lruOrder[1:]
			if _, ok := s.apiResolvedPathLRU[evictID]; ok {
				delete(s.apiResolvedPathLRU, evictID)
				break
			}
			// stale entry — skip and continue
		}
	}
	s.apiResolvedPathLRU[obsID] = entry
	s.lruOrder = append(s.lruOrder, obsID)
}

// lruPutBatch adds the entries in order under one lruMu hold, so a chunk costs
// one lock round-trip instead of one per id. It takes lruMu itself; callers
// must not hold it.
func (s *PacketStore) lruPutBatch(ids []int, rps [][]*string) {
	if len(ids) == 0 {
		return
	}
	s.lruMu.Lock()
	for i, id := range ids {
		s.lruPut(id, rps[i])
	}
	s.lruMu.Unlock()
}

// lruDelete removes an entry. Must be called under s.lruMu write lock.
func (s *PacketStore) lruDelete(obsID int) {
	if s.apiResolvedPathLRU == nil {
		return
	}
	delete(s.apiResolvedPathLRU, obsID)
	// Don't scan lruOrder — eviction handles stale entries naturally.
}

// initResolvedPathIndex initializes the resolved path index data structures.
func (s *PacketStore) initResolvedPathIndex() {
	s.resolvedPubkeyIndex = make(map[uint64][]int, 4096)
	s.resolvedPubkeyReverse = make(map[int][]uint64, 4096)
	s.apiResolvedPathLRU = make(map[int]resolvedPathLRUEntry, lruMaxSize)
	s.lruOrder = make([]int, 0, lruMaxSize)
}

// CompactResolvedPubkeyIndex reclaims memory from the resolved pubkey index maps
// after eviction. It removes empty forward-index entries (shouldn't exist if
// removeFromResolvedPubkeyIndex is correct, but defense in depth) and clips
// oversized slice backing arrays where cap > 2*len.
// Must be called under s.mu write lock.
func (s *PacketStore) CompactResolvedPubkeyIndex() {
	if !s.useResolvedPathIndex {
		return
	}
	for h, ids := range s.resolvedPubkeyIndex {
		if len(ids) == 0 {
			delete(s.resolvedPubkeyIndex, h)
			continue
		}
		// Clip oversized backing arrays: if cap > 2*len, reallocate.
		if cap(ids) > 2*len(ids)+8 {
			clipped := make([]int, len(ids))
			copy(clipped, ids)
			s.resolvedPubkeyIndex[h] = clipped
		}
	}
	for txID, hashes := range s.resolvedPubkeyReverse {
		if len(hashes) == 0 {
			delete(s.resolvedPubkeyReverse, txID)
			continue
		}
		if cap(hashes) > 2*len(hashes)+8 {
			clipped := make([]uint64, len(hashes))
			copy(clipped, hashes)
			s.resolvedPubkeyReverse[txID] = clipped
		}
	}
}

// defaultMaxResolvedPubkeyIndexEntries is the default hard cap for the forward
// index. When exceeded, a warning is logged. No auto-eviction — that's the
// eviction ticker's job.
const defaultMaxResolvedPubkeyIndexEntries = 5_000_000

// CheckResolvedPubkeyIndexSize logs a warning if the resolved pubkey forward
// index exceeds the configured maximum entries. Must be called under s.mu
// read lock at minimum.
func (s *PacketStore) CheckResolvedPubkeyIndexSize() {
	if !s.useResolvedPathIndex {
		return
	}
	maxEntries := s.maxResolvedPubkeyIndexEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxResolvedPubkeyIndexEntries
	}
	fwdLen := len(s.resolvedPubkeyIndex)
	revLen := len(s.resolvedPubkeyReverse)
	if fwdLen > maxEntries || revLen > maxEntries {
		log.Printf("[store] WARNING: resolvedPubkeyIndex size exceeds limit — forward=%d reverse=%d limit=%d",
			fwdLen, revLen, maxEntries)
	}
}
