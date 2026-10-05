package main

import (
	"log"
	"slices"
	"strings"
	"sync/atomic"
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
// A merge keeps what the duplicate knew, as the ingestor does: the earliest
// first_seen (the survivor stays where it is in s.packets and the byPayloadType
// and byNode lists it is in, which are then put back in order), and the scope and
// route type the survivor has no value for. decoded_json and payload_type are not
// filled in memory: the content hash includes the payload type and the payload,
// so the duplicates agree on them, and filling would change the survivor's
// charge and its index membership. Observations that collide on observer and
// path keep the survivor's copy (see the ingestor); the earliest first_seen is
// what carries the earliest time.
//
// Scope fill. The ingestor fills a NULL scope_name and keeps a value, "" (a
// transport-scoped packet whose region is unknown) included. In memory NULL
// and "" are both "" (nullStrVal), and StoreTx has no spare byte for a flag
// (it stays 320 bytes), so a survivor whose ScopeName is "" is filled from the
// duplicate. The two agree for a NULL survivor, the usual one: a row written
// before scope_name existed, merged with the same packet heard since. They
// differ for a survivor with "" and a duplicate with a region, which needs the
// same packet heard twice, once with a region key configured that matched its
// transport code and once without (the keys were changed in between). There
// the DB keeps "" and memory shows the region until the server reloads, which
// the deploy plan requires after the migration anyway. Filling is the more
// informative of the two and is never wrong about a region that was matched.
//
// Known gap. A reception stored on a row that the in-memory merge has already
// removed is not picked up until the server reloads: IngestNewObservations
// finds no transmission for it, and the poller's cursor moves on. It happens in
// the window between this pass and the ingestor's batch for that row, so the
// server must be restarted once after the ingestor's migration is done (see
// the PR's deploy plan). A bounded loser-to-survivor id alias would close it,
// but it has to live until the ingestor finishes, which the read-only server
// cannot see, so it is left out.
//
// Relay indexes are the one thing not carried over. A merged observation's
// resolved relays are not re-derived for the survivor (that needs resolved_path
// from the DB, and the survivor is not re-indexed in memory); they come back at
// the next load, when the DB holds the merged row. Until then they are
// attributed to nothing, where before the merge they were attributed to a
// ghost transmission that counted the same packet twice.

// hashRekeySweeps counts the passes over all of nodeHashes or byNode that a
// batch needed. Test observability only: with the resolved-pubkey index on, the
// keys of a transmission are found through the transmission and none is needed.
var hashRekeySweeps atomic.Int64

// lockHold accumulates how long the migration held the store's write lock, one
// sample per batch (NEW-1 of the #215 review: a batch that merges costs time
// that grows with the store, and staging should be able to read it off the log).
type lockHold struct {
	max, total time.Duration
	batches    int
}

func (h *lockHold) record(d time.Duration) {
	h.max = max(h.max, d)
	h.total += d
	h.batches++
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

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
	var hold lockHold
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
		held := time.Now()
		r, m := store.applyContentHashUpdates(updates)
		hold.record(time.Since(held))
		store.mu.Unlock()
		rehashed += r
		merged += m

		// Yield to let HTTP handlers run.
		time.Sleep(yieldDuration)
	}

	if rehashed > 0 {
		log.Printf("[hash-migrate] Rehashed %d transmissions in memory to the current formula, merged %d duplicates; max write-lock hold %.1f ms over %d batches (total %.1f ms)",
			rehashed, merged, millis(hold.max), hold.batches, millis(hold.total))
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
	for _, u := range updates {
		if s.byTxID[u.tx.ID] != u.tx || u.tx.Hash != u.oldHash {
			continue
		}
		live = append(live, u)
	}
	if len(live) == 0 {
		return 0, 0
	}
	keys := &nodeKeyFinder{s: s}
	s.rekeyNodeHashes(live, keys)

	m := newHashMerge()
	m.keys = keys
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

// nodeKeyFinder finds the pubkeys a transmission is indexed under in byNode and
// nodeHashes without walking those indexes. A transmission is indexed under its
// decoded pubkeys, the relays the path_json fallback resolved (recorded in
// fallbackByNode) and the relays its observations' resolved_path names; the
// last are recorded only as hashes, in the resolved-pubkey index, so they are
// mapped back to pubkeys through the pubkeys nodeHashes holds. That map costs
// one pass over the keys of nodeHashes (the nodes, not their transmissions),
// built on first use and only when a transmission of the batch has resolved
// relays.
type nodeKeyFinder struct {
	s      *PacketStore
	byHash map[uint64][]string
}

// usable reports whether the keys can be found this way. With the
// resolved-pubkey index off nothing records a transmission's resolved relays.
func (f *nodeKeyFinder) usable() bool { return f.s.useResolvedPathIndex }

// keys returns the pubkeys tx is indexed under. Must hold s.mu.
func (f *nodeKeyFinder) keys(tx *StoreTx) []string {
	var out []string
	if decoded := tx.ParsedDecoded(); decoded != nil {
		for _, field := range [...]string{"pubKey", "destPubKey", "srcPubKey"} {
			if v, ok := decoded[field].(string); ok && v != "" {
				out = append(out, v)
			}
		}
	}
	out = append(out, f.s.fallbackByNode[tx]...)
	if rev := f.s.resolvedPubkeyReverse[tx.ID]; len(rev) > 0 {
		if f.byHash == nil {
			f.byHash = make(map[uint64][]string, len(f.s.nodeHashes))
			for pk := range f.s.nodeHashes {
				h := resolvedPubkeyHash(pk)
				f.byHash[h] = append(f.byHash[h], pk)
			}
		}
		for _, h := range rev {
			out = append(out, f.byHash[h]...)
		}
	}
	return out
}

// rekeyNodeHashes renames the hash keys of s.nodeHashes. nodeHashes[pubkey] is
// the set of hashes of the transmissions byNode[pubkey] holds; a transmission
// that changes hash must change its key in every set it is in, or a later
// observation of it is indexed a second time and eviction, which removes the
// key by the transmission's current hash, leaves the old one behind.
//
// The keys of each transmission are found through the transmission
// (nodeKeyFinder), so a batch costs what the batch holds. Only with the
// resolved-pubkey index off, where a transmission's resolved relays are
// recorded nowhere, does it fall back to one pass over all of nodeHashes.
func (s *PacketStore) rekeyNodeHashes(updates []hashUpdate, keys *nodeKeyFinder) {
	if !keys.usable() {
		renames := make(map[string]string, len(updates))
		for _, u := range updates {
			renames[u.oldHash] = u.newHash
		}
		s.rekeyNodeHashesSweep(renames)
		return
	}
	for _, u := range updates {
		for _, pk := range keys.keys(u.tx) {
			if hashes := s.nodeHashes[pk]; hashes[u.oldHash] {
				delete(hashes, u.oldHash)
				hashes[u.newHash] = true
			}
		}
	}
}

// rekeyNodeHashesSweep is the fallback: one pass over all of nodeHashes.
func (s *PacketStore) rekeyNodeHashesSweep(renames map[string]string) {
	hashRekeySweeps.Add(1)
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
	if loser.FirstSeen != "" && loser.FirstSeen < winner.FirstSeen {
		winner.FirstSeen = loser.FirstSeen
		m.firstSeenMoved[winner] = struct{}{}
	}
	// The ingestor's COALESCE fills a NULL scope_name and keeps any value, ""
	// (transport-scoped, region unknown) included. StoreTx.ScopeName cannot tell
	// the two apart (nullStrVal reads NULL as ""), and StoreTx has no padding left
	// for a flag, so this fills on "": see "Scope fill"
	// in the file header.
	if winner.ScopeName == "" {
		winner.ScopeName = loser.ScopeName
	}
	if winner.RouteType == nil {
		winner.RouteType = loser.RouteType
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
	firstSeenMoved   map[*StoreTx]struct{} // survivors that took an earlier first_seen
	keys             *nodeKeyFinder
}

func newHashMerge() *hashMerge {
	return &hashMerge{
		loserSet:         make(map[*StoreTx]bool),
		winners:          make(map[*StoreTx]string),
		dropped:          make(map[int]struct{}),
		droppedObservers: make(map[string]struct{}),
		firstSeenMoved:   make(map[*StoreTx]struct{}),
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
	// The pubkeys the duplicates are indexed under, read before their records
	// (fallback relays, resolved-pubkey index) are removed below.
	var nodePks map[string]struct{}
	if m.keys != nil && m.keys.usable() {
		nodePks = make(map[string]struct{})
		for _, l := range m.losers {
			for _, pk := range m.keys.keys(l) {
				nodePks[pk] = struct{}{}
			}
		}
	}
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
	s.removeTxsFromByNode(m.loserSet, nodePks)
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
	s.reorderAfterFirstSeenMoved(m)

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

// removeTxsFromByNode removes the given transmissions from the byNode lists
// and rebuilds nodeHashes for each list it changed, from the transmissions the
// list still holds: a duplicate and its survivor share one hash after the
// merge, so the key cannot be deleted by hash without also dropping the
// survivor's, and a key the duplicate alone held must go. pks are the pubkeys
// the transmissions are indexed under (nodeKeyFinder); nil means unknown, and
// every list is looked at.
func (s *PacketStore) removeTxsFromByNode(remove map[*StoreTx]bool, pks map[string]struct{}) {
	one := func(pk string) {
		list := s.byNode[pk]
		hit := false
		for _, t := range list {
			if remove[t] {
				hit = true
				break
			}
		}
		if !hit {
			return
		}
		kept := slices.DeleteFunc(list, func(t *StoreTx) bool { return remove[t] })
		if len(kept) == 0 {
			delete(s.byNode, pk)
			delete(s.nodeHashes, pk)
			return
		}
		s.byNode[pk] = kept
		hashes := make(map[string]bool, len(kept))
		for _, t := range kept {
			hashes[t.Hash] = true
		}
		s.nodeHashes[pk] = hashes
	}
	if pks != nil {
		for pk := range pks {
			one(pk)
		}
		return
	}
	hashRekeySweeps.Add(1)
	for pk := range s.byNode {
		one(pk)
	}
}

// reorderAfterFirstSeenMoved puts the lists that are ordered by first_seen back
// in order after a survivor took an earlier one: s.packets (eviction cuts from
// its head), the byPayloadType lists and the byNode lists the survivor is in.
// Stable, so equal times keep their load order. Only runs when it happened.
func (s *PacketStore) reorderAfterFirstSeenMoved(m *hashMerge) {
	if len(m.firstSeenMoved) == 0 {
		return
	}
	byFirstSeen := func(a, b *StoreTx) int { return strings.Compare(a.FirstSeen, b.FirstSeen) }
	slices.SortStableFunc(s.packets, byFirstSeen)
	types := make(map[int]struct{})
	pks := make(map[string]struct{})
	for w := range m.firstSeenMoved {
		if w.PayloadType != nil {
			types[*w.PayloadType] = struct{}{}
		}
		if m.keys != nil && m.keys.usable() {
			for _, pk := range m.keys.keys(w) {
				pks[pk] = struct{}{}
			}
		}
	}
	for pt := range types {
		slices.SortStableFunc(s.byPayloadType[pt], byFirstSeen)
	}
	if m.keys != nil && m.keys.usable() {
		for pk := range pks {
			slices.SortStableFunc(s.byNode[pk], byFirstSeen)
		}
		return
	}
	hashRekeySweeps.Add(1)
	for _, list := range s.byNode {
		slices.SortStableFunc(list, byFirstSeen)
	}
}
