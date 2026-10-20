package main

import (
	"log"
	"sort"
	"time"
)

// retainedHistoryVerifyAttempts bounds the retries when an eviction moves
// the verification edge between the SQL count and the in-memory count.
const retainedHistoryVerifyAttempts = 3

// retainedHistoryEdgeLocked returns the instant from which every transmission
// is expected in memory: the load floor, moved past every row an eviction
// (retention or memory cap) removed since the load began and up to the
// retention floor at now, rounded up to a whole second. maxID is the highest
// transmission id the store has reached; rows above it are not ingested yet
// and are no evidence either way. ok is false when nothing can be verified:
// not loaded, empty, capped or failed.
//
// The retention floor matters with hotStartupHours=0, where the load floor
// is s.packets[0].FirstSeen: a loaded re-heard row (old first_seen, recent
// last_seen) puts it below the floor, and the DB rows between the two were
// never meant to be loaded. A row with first_seen at or after the floor has
// last_seen after it too, so the load (whose cutoff was taken earlier)
// selected it.
func (s *PacketStore) retainedHistoryEdgeLocked(now time.Time) (from time.Time, maxID int, ok bool) {
	h := &s.analyticsHistory
	if !s.loaded || len(s.packets) == 0 || h.cap || h.failed {
		return time.Time{}, 0, false
	}
	from, err := time.Parse(time.RFC3339Nano, s.oldestLoaded)
	if err != nil {
		return time.Time{}, 0, false
	}
	if h.evictedFrom.After(from) {
		from = h.evictedFrom
	}
	if s.retentionHours > 0 {
		if floor := now.Add(-time.Duration(s.retentionHours * float64(time.Hour))); floor.After(from) {
			from = floor
		}
	}
	from = from.UTC()
	if whole := from.Truncate(time.Second); whole.Before(from) {
		from = whole.Add(time.Second)
	}
	return from, s.maxTxID, true
}

// countRetainedSinceLocked counts the in-memory transmissions with
// FirstSeen >= bound and ID <= maxID. s.packets is sorted by FirstSeen, so
// they are a suffix; the per-row check keeps an out-of-order row from being
// counted, which could otherwise hide a missing one.
func (s *PacketStore) countRetainedSinceLocked(bound string, maxID int) int {
	i := sort.Search(len(s.packets), func(i int) bool { return s.packets[i].FirstSeen >= bound })
	n := 0
	for _, tx := range s.packets[i:] {
		if tx.ID <= maxID && tx.FirstSeen >= bound {
			n++
		}
	}
	return n
}

// verifyRetainedHistory returns the start of the retained history that is
// proven complete, or zero when it cannot be proven (#509). It counts the DB
// rows with first_seen at or after the edge and compares them with the rows
// in memory. The old gate required loadCoverageRatio == 1, which divides the
// rows in memory after loading by a count taken before it: rows that
// retention or eviction removed in between, or that the ingestor pruned from
// the DB, read as missing until the next restart. Here evicted rows lie below
// the edge, and pruned rows are no longer counted.
//
// first_seen is compared as a string on both sides. The bound is the whole
// second formatted without its zone suffix, which sorts at or below every
// canonical UTC stamp of that second ("…:05Z", "…:05.123Z") and above every
// stamp of the second before, so the string test equals the instant test
// (see #351 F6).
//
// The SQL count runs without s.mu (#495). An eviction between the two counts
// moves the edge; the check is then repeated against the new edge. now is
// taken once, so only an eviction moves the edge between the two reads.
func (s *PacketStore) verifyRetainedHistory() time.Time {
	now := time.Now().UTC()
	for attempt := 0; attempt < retainedHistoryVerifyAttempts; attempt++ {
		s.mu.RLock()
		from, maxID, ok := s.retainedHistoryEdgeLocked(now)
		s.mu.RUnlock()
		if !ok {
			return time.Time{}
		}
		bound := from.Format("2006-01-02T15:04:05")
		var inDB int
		if err := s.db.conn.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE first_seen >= ? AND id <= ?`,
			bound, maxID).Scan(&inDB); err != nil {
			log.Printf("[store] retained history unverified: count failed: %v", err)
			return time.Time{}
		}
		if s.retainedHistoryCountedHook != nil {
			s.retainedHistoryCountedHook()
		}
		s.mu.RLock()
		again, _, ok := s.retainedHistoryEdgeLocked(now)
		inMemory := 0
		if ok && again.Equal(from) {
			inMemory = s.countRetainedSinceLocked(bound, maxID)
		}
		s.mu.RUnlock()
		if !ok {
			return time.Time{}
		}
		if !again.Equal(from) {
			continue
		}
		if inMemory != inDB {
			log.Printf("[store] retained history unverified: %d of %d transmissions since %s in memory", inMemory, inDB, bound)
			return time.Time{}
		}
		return from
	}
	log.Printf("[store] retained history unverified: eviction moved the edge %d times", retainedHistoryVerifyAttempts)
	return time.Time{}
}

// publishRetainedHistory verifies the retained history and records the
// result for the coverage readers. It returns whether it was verified.
func (s *PacketStore) publishRetainedHistory() bool {
	from := s.verifyRetainedHistory()
	s.mu.Lock()
	s.retainedHistoryFrom = from
	s.mu.Unlock()
	return !from.IsZero()
}
