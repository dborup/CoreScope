package main

import (
	"log"
	"slices"
	"time"
)

// Content-hash migration, server half (#215).
//
// The DB half (rehash, merge the rows that collide, delete the duplicate) is
// done once by the ingestor (cmd/ingestor/hash_migrate.go): cmd/server is the
// read path and opens SQLite with mode=ro, so it must not write. Everything
// here happens in memory and is safe to repeat: a store loaded from a DB the
// ingestor has already converged finds nothing to do.
//
// What it still does matters for a server that loaded rows before the ingestor
// got to them. Packets whose hash differs from the current formula are rehashed
// in memory. When two of them collide, or one collides with a transmission that
// already has the hash, they are merged the way the ingestor merges the DB
// rows, so the two agree on which transmission is the survivor: the lowest ID.
// The duplicate's observations move to it (an observation the survivor already
// has, same observer and path, is dropped, as the DB's dedup index drops it),
// and the duplicate leaves every index.
//
// Relay indexes are the one thing not carried over. A merged observation's
// resolved relays are not re-derived for the survivor (that needs resolved_path
// from the DB, and the survivor is not re-indexed in memory); they come back at
// the next load, when the DB holds the merged row. Until then they are
// attributed to nothing, where before the merge they were attributed to a
// ghost transmission that counted the same packet twice.

type hashUpdate struct {
	tx      *StoreTx
	oldHash string
	newHash string
}

// migrateContentHashesAsync recomputes content hashes in batches after the
// server is already serving HTTP. It only reads and rewrites in-memory state.
func migrateContentHashesAsync(store *PacketStore, batchSize int, yieldDuration time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[hash-migrate] panic recovered: %v", r)
		}
		store.hashMigrationComplete.Store(true)
	}()

	// Walk a snapshot, not the live slice: a merge removes elements from
	// s.packets and eviction trims its head, either of which would shift an
	// offset into it. Transmissions that are gone by the time their batch is
	// applied are skipped.
	store.mu.RLock()
	snapshot := slices.Clone(store.packets)
	store.mu.RUnlock()

	if batchSize < 1 {
		batchSize = 1
	}
	rehashed, merged := 0, 0
	indexesReady := false
	for offset := 0; offset < len(snapshot); offset += batchSize {
		end := min(offset+batchSize, len(snapshot))

		store.mu.RLock()
		updates := staleContentHashes(store, snapshot[offset:end])
		store.mu.RUnlock()
		if len(updates) == 0 {
			continue
		}

		// A merge edits the subpath and path-hop indexes: let their
		// background build finish first, or it could index a duplicate that
		// was just removed.
		if !indexesReady {
			store.WaitIndexesReady(30 * time.Second)
			indexesReady = true
		}
		store.mu.Lock()
		r, m := store.applyContentHashUpdates(updates)
		store.mu.Unlock()
		rehashed += r
		merged += m

		// Yield to let HTTP handlers run.
		time.Sleep(yieldDuration)
	}

	if rehashed > 0 {
		log.Printf("[hash-migrate] Rehashed %d transmissions in memory to the current formula, merged %d duplicates", rehashed, merged)
	}
}

// staleContentHashes returns the transmissions of batch whose hash differs from
// the current formula. Must hold s.mu (read).
func staleContentHashes(s *PacketStore, batch []*StoreTx) []hashUpdate {
	var updates []hashUpdate
	for _, tx := range batch {
		if tx.RawHex == "" || s.byTxID[tx.ID] != tx {
			continue
		}
		if newHash := ComputeContentHash(tx.RawHex); newHash != tx.Hash {
			updates = append(updates, hashUpdate{tx: tx, oldHash: tx.Hash, newHash: newHash})
		}
	}
	return updates
}

// applyContentHashUpdates rehashes the transmissions of updates and merges the
// ones that collide. Returns how many it rehashed and how many duplicates it
// merged away. Must hold s.mu for writing.
func (s *PacketStore) applyContentHashUpdates(updates []hashUpdate) (rehashed, merged int) {
	// The batch was chosen under a read lock: drop what changed since
	// (eviction, a concurrent rehash).
	live := updates[:0]
	renames := make(map[string]string, len(updates))
	for _, u := range updates {
		if s.byTxID[u.tx.ID] != u.tx || u.tx.Hash != u.oldHash {
			continue
		}
		live = append(live, u)
		renames[u.oldHash] = u.newHash
	}
	if len(live) == 0 {
		return 0, 0
	}
	s.rekeyNodeHashes(renames)

	m := newHashMerge()
	for _, u := range live {
		tx := u.tx
		if s.byHash[u.oldHash] == tx {
			delete(s.byHash, u.oldHash)
		}
		tx.Hash = u.newHash
		// The hash length is part of the charge (estimateStoreTxBytes).
		s.trackedBytes += rechargeTx(tx)

		holder := s.byHash[u.newHash]
		if holder == nil || holder == tx {
			s.byHash[u.newHash] = tx
			continue
		}
		// A collision. The lowest ID survives, as in the ingestor's merge of
		// the DB rows, so later polls by transmission_id find the same row.
		winner, loser := tx, holder
		if holder.ID < tx.ID {
			winner, loser = holder, tx
		}
		s.byHash[u.newHash] = winner
		s.moveObservations(winner, loser, m)
		m.add(winner, loser)
	}
	merged = s.finishHashMerge(m)
	return len(live), merged
}

// rekeyNodeHashes renames the hash keys of s.nodeHashes. nodeHashes[pubkey] is
// the set of hashes of the transmissions byNode[pubkey] holds; a transmission
// that changes hash must change its key in every set it is in, or a later
// observation of it is indexed a second time and eviction, which removes the
// key by the transmission's current hash, leaves the old one behind.
//
// The pubkeys a transmission is indexed under are not recorded per
// transmission (resolved relays are only ever read back from the DB), so this
// walks the whole index once per batch, a lookup per entry and no allocation
// for the entries that do not change.
func (s *PacketStore) rekeyNodeHashes(renames map[string]string) {
	var moved []string
	for _, hashes := range s.nodeHashes {
		moved = moved[:0]
		for h := range hashes {
			if _, ok := renames[h]; ok {
				moved = append(moved, h)
			}
		}
		for _, h := range moved {
			delete(hashes, h)
			hashes[renames[h]] = true
		}
	}
}

// moveObservations moves loser's observations to winner, dropping the ones
// winner already has (same observer and path). The observations keep their
// place in byObsID and byObserver and their charge; a dropped one is credited
// here. loser keeps its observations list until finishHashMerge has removed it
// from the path-hop index, which reads it; the index entries of loser itself
// are removed there.
func (s *PacketStore) moveObservations(winner, loser *StoreTx, m *hashMerge) {
	if winner.obsKeys == nil {
		winner.obsKeys = make(map[string]bool)
		winner.observerSet = make(map[string]bool)
	}
	for _, o := range loser.Observations {
		dk := o.ObserverID + "|" + o.PathJSON
		if winner.obsKeys[dk] {
			delete(s.byObsID, o.ID)
			s.totalObs--
			s.trackedBytes -= estimateStoreObsBytes(o)
			m.dropped[o.ID] = struct{}{}
			if o.ObserverID != "" {
				m.droppedObservers[o.ObserverID] = struct{}{}
			}
			continue
		}
		o.TransmissionID = winner.ID
		winner.Observations = append(winner.Observations, o)
		winner.obsKeys[dk] = true
		if o.ObserverID != "" && !winner.observerSet[o.ObserverID] {
			winner.observerSet[o.ObserverID] = true
			winner.UniqueObserverCount++
		}
		winner.ObservationCount++
		if o.Timestamp > winner.LatestSeen {
			winner.LatestSeen = o.Timestamp
		}
	}
	if loser.LatestSeen > winner.LatestSeen {
		winner.LatestSeen = loser.LatestSeen
	}
	winner.pathHashSizeMask |= loser.pathHashSizeMask
	if loser.routeMaskKnown {
		winner.routeMask |= loser.routeMask
		winner.routeMaskKnown = true
	}
}

// hashMerge collects what one batch of collisions removed and touched.
type hashMerge struct {
	losers           []*StoreTx
	loserSet         map[*StoreTx]bool
	winners          map[*StoreTx]string // survivor → its best path before the first merge
	dropped          map[int]struct{}    // observation ids dropped as duplicates
	droppedObservers map[string]struct{}
}

func newHashMerge() *hashMerge {
	return &hashMerge{
		loserSet:         make(map[*StoreTx]bool),
		winners:          make(map[*StoreTx]string),
		dropped:          make(map[int]struct{}),
		droppedObservers: make(map[string]struct{}),
	}
}

// add records a merge. winner's best path is saved before moveObservations'
// caller lets it change; add must run after moveObservations, which does not
// touch PathJSON, so the saved path is still the pre-merge one.
func (m *hashMerge) add(winner, loser *StoreTx) {
	if _, ok := m.winners[winner]; !ok {
		m.winners[winner] = winner.PathJSON
	}
	m.losers = append(m.losers, loser)
	m.loserSet[loser] = true
}

// finishHashMerge removes every merged-away transmission from every index and
// from trackedBytes, then refreshes the survivors (best observation, charge,
// path-derived indexes). Returns the number of duplicates removed.
func (s *PacketStore) finishHashMerge(m *hashMerge) int {
	if len(m.losers) == 0 {
		return 0
	}
	affectedPayloadTypes := make(map[int]struct{})
	for _, l := range m.losers {
		delete(s.byTxID, l.ID)
		// Observations moved or were credited above: this is the tx alone.
		s.trackedBytes -= s.txChargedBytes(l)
		s.untrackAdvertPubkey(l)
		s.removeFromResolvedPubkeyIndex(l.ID)
		if removeTxFromSubpathIndexFull(s.spIndex, s.spTxIndex, l) {
			s.spTotalPaths--
		}
		delete(s.pathHopResolved, l)
		delete(s.fallbackByNode, l)
		if l.PayloadType != nil {
			affectedPayloadTypes[*l.PayloadType] = struct{}{}
		}
	}
	// The resolved relay keys of a duplicate in byPathHop are found through the
	// hops of its observed paths, so it must still hold its observations here:
	// they were moved, not detached. A duplicate that was itself a survivor
	// earlier in the batch holds the ones it took over too.
	evictFromPathHopIndex(s.byPathHop, m.loserSet)
	for _, l := range m.losers {
		l.Observations = nil
		l.ObservationCount = 0
	}
	s.removeTxsFromByNode(m.loserSet)
	for pt := range affectedPayloadTypes {
		kept := slices.DeleteFunc(s.byPayloadType[pt], func(t *StoreTx) bool { return m.loserSet[t] })
		if len(kept) == 0 {
			delete(s.byPayloadType, pt)
		} else {
			s.byPayloadType[pt] = kept
		}
	}
	for obsID := range m.droppedObservers {
		kept := slices.DeleteFunc(s.byObserver[obsID], func(o *StoreObs) bool {
			_, dropped := m.dropped[o.ID]
			return dropped
		})
		if len(kept) == 0 {
			delete(s.byObserver, obsID)
		} else {
			s.byObserver[obsID] = kept
		}
	}
	s.compactDistIndex(m.loserSet)
	s.packets = slices.DeleteFunc(s.packets, func(t *StoreTx) bool { return m.loserSet[t] })

	// A survivor now holds observations it did not have: its best one may be a
	// different path, which moves it in the path-derived indexes, and its
	// charge follows. A survivor that was itself merged into another one later
	// in the batch is gone.
	var changed []*StoreTx
	for w, oldPath := range m.winners {
		if m.loserSet[w] {
			continue
		}
		pickBestObservation(w)
		s.trackedBytes += rechargeTx(w)
		if w.PathJSON != oldPath {
			s.reindexTxPath(w, oldPath)
			changed = append(changed, w)
		}
	}
	if len(changed) > 0 {
		s.updateDistanceIndexForTxs(changed)
	}

	s.invalidateRelayStatsCache()
	s.invalidateCachesFor(cacheInvalidation{eviction: true})
	s.hashSizeInfoMu.Lock()
	s.hashSizeInfoCache = nil
	s.hashSizeInfoMu.Unlock()
	return len(m.losers)
}

// removeTxsFromByNode removes the given transmissions from every byNode list
// and rebuilds nodeHashes for each list it changed, from the transmissions the
// list still holds: a duplicate and its survivor share one hash after the
// merge, so the key cannot be deleted by hash without also dropping the
// survivor's, and a key the duplicate alone held must go. One pass over the
// lists, which only does work for lists that contain a removed transmission.
func (s *PacketStore) removeTxsFromByNode(remove map[*StoreTx]bool) {
	for pk, list := range s.byNode {
		hit := false
		for _, t := range list {
			if remove[t] {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		kept := slices.DeleteFunc(list, func(t *StoreTx) bool { return remove[t] })
		if len(kept) == 0 {
			delete(s.byNode, pk)
			delete(s.nodeHashes, pk)
			continue
		}
		s.byNode[pk] = kept
		hashes := make(map[string]bool, len(kept))
		for _, t := range kept {
			hashes[t.Hash] = true
		}
		s.nodeHashes[pk] = hashes
	}
}
