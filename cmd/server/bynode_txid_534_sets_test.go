package main

import "sort"

// The only place the #534 tests read byNode's de-dup sets directly; every
// other check goes through byNode, the store's entry points and its readers.
// Must hold s.mu. Master keys the sets by hash: each is mapped back to the ID
// of the live transmission that holds it, -1 when none does.

// bn534SetIDs returns the transmission IDs the de-dup set of pk holds, sorted.
func bn534SetIDs(s *PacketStore, pk string) []int {
	set := s.nodeHashes[pk]
	ids := make([]int, 0, len(set))
	for h := range set {
		if tx := s.byHash[h]; tx != nil {
			ids = append(ids, tx.ID)
		} else {
			ids = append(ids, -1)
		}
	}
	sort.Ints(ids)
	return ids
}

// bn534SetKeys returns the pubkeys that have a de-dup set.
func bn534SetKeys(s *PacketStore) []string {
	pks := make([]string, 0, len(s.nodeHashes))
	for pk := range s.nodeHashes {
		pks = append(pks, pk)
	}
	return pks
}

// bn534Unlist removes tx from the de-dup set of pk, as eviction does.
func bn534Unlist(s *PacketStore, pk string, tx *StoreTx) {
	if hashes, ok := s.nodeHashes[pk]; ok {
		delete(hashes, tx.Hash)
		if len(hashes) == 0 {
			delete(s.nodeHashes, pk)
		}
	}
}
