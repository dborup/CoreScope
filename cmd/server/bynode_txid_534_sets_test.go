package main

import "sort"

// The only place the #534 tests read byNode's de-dup sets directly; every
// other check goes through byNode, the store's entry points and its readers.
// Must hold s.mu.

// bn534SetIDs returns the transmission IDs the de-dup set of pk holds, sorted.
func bn534SetIDs(s *PacketStore, pk string) []int {
	var ids []int
	if set := s.nodeTxIDs[pk]; set != nil {
		set.any(func(id int) bool {
			ids = append(ids, id)
			return false
		})
	}
	sort.Ints(ids)
	return ids
}

// bn534SetKeys returns the pubkeys that have a de-dup set.
func bn534SetKeys(s *PacketStore) []string {
	pks := make([]string, 0, len(s.nodeTxIDs))
	for pk := range s.nodeTxIDs {
		pks = append(pks, pk)
	}
	return pks
}

// bn534Unlist removes tx from the de-dup set of pk, as eviction does.
func bn534Unlist(s *PacketStore, pk string, tx *StoreTx) { s.unlistNodeTx(pk, tx) }
