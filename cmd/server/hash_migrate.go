package main

import (
	"log"
	"time"
)

// migrateContentHashesAsync recomputes content hashes in batches after the
// server is already serving HTTP.  Packets whose hash changes are updated in
// both the DB and the in-memory byHash index.  The migration is idempotent:
// once all hashes match the current formula it completes instantly.
func migrateContentHashesAsync(store *PacketStore, batchSize int, yieldDuration time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[hash-migrate] panic recovered: %v", r)
		}
		store.hashMigrationComplete.Store(true)
	}()

	// Snapshot the packet slice length under lock (packets only grow).
	store.mu.RLock()
	total := len(store.packets)
	store.mu.RUnlock()

	// Keep evidence only for hashes that actually collide during this one
	// migration run. The legacy migration retains duplicate in-memory rows, so
	// a later batch must also update ghosts created by an earlier collision.
	// This registry is bounded by collision members and is discarded when the
	// migration returns.
	type collisionEvidence struct {
		mask    uint8
		members map[*StoreTx]struct{}
	}
	collisionEvidenceByHash := make(map[string]*collisionEvidence)

	migrated := 0
	for offset := 0; offset < total; offset += batchSize {
		end := offset + batchSize
		if end > total {
			end = total
		}

		// Collect stale hashes in this batch under RLock.
		type hashUpdate struct {
			tx      *StoreTx
			oldHash string
			newHash string
		}
		var updates []hashUpdate

		store.mu.RLock()
		for _, tx := range store.packets[offset:end] {
			if tx.RawHex == "" {
				continue
			}
			newHash := ComputeContentHash(tx.RawHex)
			if newHash != tx.Hash {
				updates = append(updates, hashUpdate{tx: tx, oldHash: tx.Hash, newHash: newHash})
			}
		}
		store.mu.RUnlock()

		if len(updates) == 0 {
			continue
		}
		// A UNIQUE collision merges DB observations into one survivor, while
		// this legacy migration intentionally keeps its existing in-memory
		// cardinality/index behaviour. Track the survivor IDs so the observed
		// path-width evidence can nevertheless be made consistent across every
		// same-content in-memory row after the existing update loop.
		collisionSurvivors := make(map[string][]int)

		// Write batch to DB in a single transaction.
		dbTx, err := store.db.conn.Begin()
		if err != nil {
			log.Printf("[hash-migrate] begin tx: %v", err)
			continue
		}
		stmt, err := dbTx.Prepare("UPDATE transmissions SET hash = ? WHERE id = ?")
		if err != nil {
			log.Printf("[hash-migrate] prepare: %v", err)
			dbTx.Rollback()
			continue
		}

		for _, u := range updates {
			if _, err := stmt.Exec(u.newHash, u.tx.ID); err != nil {
				// UNIQUE constraint = two old hashes map to the same new hash (duplicate).
				// Merge observations to the surviving tx, delete the duplicate.
				log.Printf("[hash-migrate] tx %d collides — merging duplicate", u.tx.ID)
				var survID int
				if err2 := dbTx.QueryRow("SELECT id FROM transmissions WHERE hash = ?", u.newHash).Scan(&survID); err2 == nil {
					dbTx.Exec("UPDATE observations SET transmission_id = ? WHERE transmission_id = ?", survID, u.tx.ID)
					dbTx.Exec("DELETE FROM transmissions WHERE id = ?", u.tx.ID)
					collisionSurvivors[u.newHash] = append(collisionSurvivors[u.newHash], survID)
				}
			}
		}
		stmt.Close()

		if err := dbTx.Commit(); err != nil {
			log.Printf("[hash-migrate] commit: %v", err)
			continue
		}

		// Update in-memory index under write lock.
		store.mu.Lock()
		// A merged duplicate stays in memory with its own observations (see
		// collisionSurvivors above): the DB rows were re-parented, the
		// in-memory ones are not, so trackedBytes keeps charging each
		// observation to the one tx that holds it and eviction credits it
		// once (#202). Copying them to the survivor here would credit them
		// twice, as each of the two txs is evicted. A fix that removes the
		// duplicate must move them (see
		// TestHashMigrationMerge_EvictionCreditsEveryObservationOnce_202).
		for _, u := range updates {
			delete(store.byHash, u.oldHash)
			u.tx.Hash = u.newHash
			store.byHash[u.newHash] = u.tx
		}
		for newHash, survivorIDs := range collisionSurvivors {
			evidence := collisionEvidenceByHash[newHash]
			if evidence == nil {
				evidence = &collisionEvidence{members: make(map[*StoreTx]struct{})}
				collisionEvidenceByHash[newHash] = evidence
			}
			addMember := func(tx *StoreTx) {
				if tx == nil {
					return
				}
				evidence.mask |= tx.pathHashSizeMask
				evidence.members[tx] = struct{}{}
			}
			for _, u := range updates {
				if u.newHash == newHash {
					addMember(u.tx)
				}
			}
			for _, survivorID := range survivorIDs {
				addMember(store.byTxID[survivorID])
			}
			addMember(store.byHash[newHash])
			for member := range evidence.members {
				member.pathHashSizeMask |= evidence.mask
			}
		}
		store.mu.Unlock()

		migrated += len(updates)

		// Yield to let HTTP handlers run.
		time.Sleep(yieldDuration)
	}

	if migrated > 0 {
		log.Printf("[hash-migrate] Migrated %d content hashes to new formula", migrated)
	}
}
