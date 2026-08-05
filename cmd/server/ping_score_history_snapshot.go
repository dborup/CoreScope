// Package main: Ping Scores snapshot-from-history builder -- Phase 4C of
// the recompute redesign (see reviews/CoreScope-code-review-2026-08-04.md).
//
// buildPingScoresSnapshotFromHistory produces the same PingScoresSnapshot
// shape computeAllPingScores does, but from already-persisted history
// entries instead of a live GetPacketPath call per trigger. It is NOT yet
// wired into the production recomputer (Phase 4D+ cuts over) -- it exists
// now so it can be validated against computeAllPingScores' own output via
// equivalence tests (ping_score_history_snapshot_test.go) before any
// cutover happens.
package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PingScoreHistoryMismatchError means a persisted entry's hash or timestamp
// no longer matches the live ping_triggers row for the same tx_id --
// reconciliation (planPingScoreHistoryReconcile) hasn't caught up with an
// invalidation yet, so the entry may describe a different packet entirely.
// buildPingScoresSnapshotFromHistory returns this instead of ever showing
// the mismatched entry as if it were current.
type PingScoreHistoryMismatchError struct {
	TxID                             int64
	TriggerHash, EntryHash           string
	TriggerTimestamp, EntryTimestamp string
}

func (e *PingScoreHistoryMismatchError) Error() string {
	return fmt.Sprintf(
		"ping score history: tx_id=%d trigger/entry mismatch (trigger hash=%q ts=%q vs entry hash=%q ts=%q) -- reconciliation required before building a snapshot",
		e.TxID, e.TriggerHash, e.TriggerTimestamp, e.EntryHash, e.EntryTimestamp,
	)
}

// observerNamesByPubkey bulk-resolves observer display names for a set of
// pubkeys -- v3 schema only.
//
// IMPORTANT, investigated for this phase: this mirrors the name SOURCE
// GetPacketPath's live Observer.Name field actually uses (b.observerName,
// read directly off the observers table via the observer_idx join in
// GetPacketPath's own query) -- it is observers.name, NOT nodes.name.
// These are two independently maintained fields (an observer's own
// self-reported name at packet-hearing time vs. a node's self-reported
// name at ADVERT time) that usually agree for the same physical device but
// are not guaranteed to. namesAndRolesForPubkeys (used below, unchanged,
// for RelayLeaderboard names) queries nodes.name instead, matching
// computeAllPingScores' own existing relay-name lookup exactly. Neither
// path consults inactive_nodes anywhere -- grep confirms nothing in
// GetPacketPath, resolveNodesByPubkey/resolveNodesByName, or
// namesAndRolesForPubkeys ever references that table, so this function
// doesn't either; today's ping-score name resolution has never fallen back
// to it, and this preserves that.
//
// Case-insensitive on purpose (`LOWER(id) IN (...)`): DeepestPubkey/
// FarthestPubkey/FirstPubkey are always persisted lowercased (see
// parsePacketPathObsRow), but observers.id's on-disk casing is not
// something this function assumes -- LOWER(id) normalizes both sides
// regardless. This means SQLite can't use observers.id's own index for
// this lookup, but the observers table is small and this runs at most
// once per snapshot build, not per-ping -- an accepted tradeoff for
// correctness here.
//
// Legacy (non-v3) schema has no separate observers table at all --
// GetPacketPath instead reads observer_name directly off each OBSERVATION
// row for that schema, a value that can vary per observation and isn't
// otherwise indexed for a bulk historical lookup like this one. Returns an
// empty map for that case (a known limitation, not silently pretended
// away) rather than attempting to reconstruct it -- callers already treat
// a missing name as a pubkey-fallback, best-effort case (see
// buildPingScoresSnapshotFromHistory's nameOrFallback), so this just means
// legacy-schema snapshots show pubkeys instead of names for these three
// fields; a cosmetic degradation, not a score-state one.
func (db *DB) observerNamesByPubkey(pubkeys []string) map[string]string {
	names := make(map[string]string, len(pubkeys))
	if len(pubkeys) == 0 || !db.isV3() {
		return names
	}
	unique := dedupPacketPathStrings(pubkeys)
	for i := 0; i < len(unique); i += packetPathNodeLookupChunkSize {
		end := i + packetPathNodeLookupChunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[i:end]
		placeholders := make([]byte, 0, len(chunk)*2)
		args := make([]interface{}, len(chunk))
		for j, pk := range chunk {
			if j > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args[j] = pk
		}
		rows, err := db.conn.Query("SELECT id, name FROM observers WHERE LOWER(id) IN ("+string(placeholders)+")", args...)
		if err != nil {
			continue // best-effort: a name-lookup failure must not break score-state
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var id string
				var name sql.NullString
				if rows.Scan(&id, &name) != nil {
					return // best-effort, as above: a failed chunk just leaves those names unresolved
				}
				if name.String != "" {
					names[strings.ToLower(id)] = name.String
				}
			}
		}()
	}
	return names
}

// buildPingScoresSnapshotFromHistory builds a PingScoresSnapshot from
// triggers (a fresh ping_triggers read, e.g. fetchPingTriggers' result)
// and entries (persisted history rows, e.g. PingScoreHistoryStore.LoadAll's
// result or a pingScoreHistoryIndex's Entries()) -- reusing the exact same
// record-selection (pingRecordSet.consider), leaderboard-ranking
// (topPingLeaderboard), and cutoff (7-day ThisWeek, 30-day
// SenderLeaderboard) logic computeAllPingScores itself uses, so the two
// can never silently diverge in selection or tie-break rules.
//
// now is injected rather than read via time.Now() deep in this function --
// callers (and tests) control the ThisWeek/SenderLeaderboard cutoffs and
// GeneratedAt precisely, and repeated calls with the same inputs+now are
// byte-for-byte reproducible.
//
// Only entries whose tx_id appears in triggers are considered (a stale
// entry for a tx_id ping_triggers no longer has is silently excluded, not
// an error -- reconciling that away is planPingScoreHistoryReconcile's
// job, not this function's). A trigger with no corresponding entry yet is
// likewise just excluded (nothing to show for it this cycle), matching
// computeAllPingScores' own "score == nil -> continue" behavior for a
// trigger GetPacketPath couldn't resolve. TotalPings is len(triggers) --
// the fresh, live count -- never len(entries) or an index's Len().
//
// A trigger whose hash or timestamp doesn't match its history entry's own
// (same tx_id, different content -- reconciliation hasn't caught up with
// an invalidation yet) is a HARD failure: (nil, *PingScoreHistoryMismatchError).
// This function never shows a known-stale/mismatched entry as if it were
// current -- that would silently publish wrong data with no signal.
// Reconciling this away (recomputing the tx_id under its new hash/
// timestamp) is planPingScoreHistoryReconcile's + the engine's job; this
// function only refuses to paper over the gap.
//
// A materialization failure (invalid persisted relay_pubkeys_json -- see
// materializePingScoreFromHistoryEntry) aborts the WHOLE build with an
// error rather than silently skipping that one entry: skipping would
// permanently and invisibly drop a historical ping from the leaderboards
// with no visible signal, exactly what item 2's error contract exists to
// prevent. Name-lookup failures are a completely different, deliberately
// best-effort case (see observerNamesByPubkey/nameOrFallback below) --
// cosmetic, never a reason to fail the build.
func (s *Server) buildPingScoresSnapshotFromHistory(
	triggers []pingTriggerRow,
	entries []PingScoreHistoryEntry,
	now time.Time,
) (*PingScoresSnapshot, error) {
	byTxID := make(map[int64]PingScoreHistoryEntry, len(entries))
	for _, e := range entries {
		byTxID[e.TxID] = e
	}

	snap := &PingScoresSnapshot{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		TotalPings:  len(triggers),
	}

	relayCounts := map[string]*PingLeaderboardEntry{}
	observerCounts := map[string]*PingLeaderboardEntry{}
	senderCutoff := now.AddDate(0, 0, -30) // matches computeAllPingScores' 30-day SenderLeaderboard window exactly
	senderCounts := map[string]*PingLeaderboardEntry{}
	weekCutoff := now.AddDate(0, 0, -7) // matches computeAllPingScores' 7-day ThisWeek window exactly
	allTime := &pingRecordSet{}
	week := &pingRecordSet{}

	// Collected for the name-enrichment pass below. Each score here is a
	// freshly allocated *PingScore from materializePingScoreFromHistoryEntry
	// -- never the same object as anything in `entries`, so mutating a
	// score's name fields below never mutates the input entries.
	var scores []*PingScore

	for _, trigger := range triggers {
		entry, ok := byTxID[trigger.txID]
		if !ok {
			continue // no history entry for this trigger (yet)
		}
		if entry.Hash != trigger.hash || entry.Timestamp != trigger.firstSeen {
			// A persisted entry whose hash/timestamp no longer matches the
			// live trigger row means reconciliation (planPingScoreHistoryReconcile)
			// hasn't caught up with this tx_id yet -- the entry may describe
			// a completely different packet. Showing it anyway (a "known
			// stale score") would silently publish wrong data with no
			// signal; failing the whole build instead makes reconciliation
			// a HANDHÆVET invariant rather than just a comment's promise.
			return nil, &PingScoreHistoryMismatchError{
				TxID:        trigger.txID,
				TriggerHash: trigger.hash, EntryHash: entry.Hash,
				TriggerTimestamp: trigger.firstSeen, EntryTimestamp: entry.Timestamp,
			}
		}
		score, err := materializePingScoreFromHistoryEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("buildPingScoresSnapshotFromHistory: %w", err)
		}
		if score == nil {
			continue // Unscorable
		}
		scores = append(scores, score)

		allTime.consider(score)
		ts, tsErr := time.Parse(time.RFC3339, score.Timestamp)
		if tsErr == nil && ts.After(weekCutoff) {
			week.consider(score)
		}
		for _, pk := range score.relayPubkeys {
			e := relayCounts[pk]
			if e == nil {
				e = &PingLeaderboardEntry{Pubkey: pk}
				relayCounts[pk] = e
			}
			e.Count++
		}
		if score.firstPubkey != "" {
			e := observerCounts[score.firstPubkey]
			if e == nil {
				e = &PingLeaderboardEntry{Pubkey: score.firstPubkey}
				observerCounts[score.firstPubkey] = e
			}
			e.Count++
		}
		if score.Sender != "" && tsErr == nil && ts.After(senderCutoff) {
			e := senderCounts[score.Sender]
			if e == nil {
				e = &PingLeaderboardEntry{Name: score.Sender}
				senderCounts[score.Sender] = e
			}
			e.Count++
		}
	}

	snap.FarthestPing = allTime.Farthest
	snap.MostHopsPing = allTime.MostHops
	snap.WidestSpreadPing = allTime.WidestSpread
	snap.FastestSpreadPing = allTime.FastestSpread
	snap.MostEfficientPing = allTime.MostEfficient
	if week.Farthest != nil || week.MostHops != nil || week.WidestSpread != nil || week.FastestSpread != nil || week.MostEfficient != nil {
		snap.ThisWeek = &WeeklyPingRecords{
			FarthestPing:      week.Farthest,
			MostHopsPing:      week.MostHops,
			WidestSpreadPing:  week.WidestSpread,
			FastestSpreadPing: week.FastestSpread,
			MostEfficientPing: week.MostEfficient,
		}
	}

	// --- name enrichment: best-effort, cosmetic only. A failed/partial
	// lookup just leaves the pubkey-fallback already in place -- it never
	// aborts the build or otherwise touches score-state (StationCount,
	// DeepestHops, FarthestKm, etc. above are already final at this point).

	observerPubkeySet := map[string]bool{}
	for _, score := range scores {
		if score.DeepestPubkey != "" {
			observerPubkeySet[score.DeepestPubkey] = true
		}
		if score.FarthestPubkey != "" {
			observerPubkeySet[score.FarthestPubkey] = true
		}
		if score.firstPubkey != "" {
			observerPubkeySet[score.firstPubkey] = true
		}
	}
	observerPubkeys := make([]string, 0, len(observerPubkeySet))
	for pk := range observerPubkeySet {
		observerPubkeys = append(observerPubkeys, pk)
	}
	observerNames := s.db.observerNamesByPubkey(observerPubkeys)
	nameOrFallback := func(pk string) string {
		if name := observerNames[pk]; name != "" {
			return name
		}
		return pk
	}
	for _, score := range scores {
		if score.DeepestPubkey != "" {
			score.DeepestName = nameOrFallback(score.DeepestPubkey)
		}
		if score.FarthestPubkey != "" {
			score.FarthestName = nameOrFallback(score.FarthestPubkey)
		}
		if score.firstPubkey != "" {
			score.firstName = nameOrFallback(score.firstPubkey)
		}
	}
	// ObserverLeaderboard's Name matches computeAllPingScores' own source
	// exactly (score.firstName, no separate bulk query for it) -- derived
	// here now that firstName has just been enriched above.
	for pk, e := range observerCounts {
		e.Name = nameOrFallback(pk)
	}

	// RelayLeaderboard names: unchanged from computeAllPingScores -- same
	// helper (nodes.name, not observers.name), same fallback-to-pubkey rule.
	if len(relayCounts) > 0 {
		pubkeys := make([]string, 0, len(relayCounts))
		for pk := range relayCounts {
			pubkeys = append(pubkeys, pk)
		}
		names, _ := s.db.namesAndRolesForPubkeys(pubkeys)
		for pk, e := range relayCounts {
			if name := names[pk]; name != "" {
				e.Name = name
			} else {
				e.Name = pk
			}
		}
	}

	snap.RelayLeaderboard = topPingLeaderboard(relayCounts, 10)
	snap.ObserverLeaderboard = topPingLeaderboard(observerCounts, 10)
	snap.SenderLeaderboard = topPingLeaderboard(senderCounts, 10)
	return snap, nil
}
