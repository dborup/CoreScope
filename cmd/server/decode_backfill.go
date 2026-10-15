package main

import (
	"database/sql"
	"log"
	"strings"
)

// Read-only view of the ingestor's historical decode backfill (#380).
//
// The ingestor re-decodes stored `enc_<HEX>` GRP_TXT rows and unknown-scoped
// transport rows when the effective key set grows, rewriting channel_hash,
// decoded_json and scope_name (cmd/ingestor/decode_backfill.go). Every write
// stays there: this file only reads, and the server keeps its mode=ro handle
// (#1283).
//
// What the server has to do about it is invalidate caches. The surfaces that
// show channel decoding and region scopes are SQL-backed:
//
//   - GET /api/channels and GET /api/channels/{hash}/messages read the
//     database (handleChannelMessages prefers s.db over the in-memory store),
//     through DB.channelsCache / DB.encChannelsCache;
//   - the analytics channel tabs read it through PacketStore.chanCache and
//     channelsCacheRes.
//
// So one signal is enough — "rows were rewritten" — and the ingestor provides
// it as a monotonic revision on its one-row state table. The poller reads
// that single row per tick and drops the caches above when it grows. No
// per-row change log, deliberately: route_mask_changes needs per-row ids and
// therefore its own retention and orphan sweeps, and nothing here does.
//
// What a rewrite does NOT change without a restart is the in-memory StoreTx
// copy of a transmission the server loaded BEFORE the rewrite: its
// DecodedJSON and ScopeName keep their load-time values. That is deliberate.
// Analytics read transmissions from snapshots taken outside s.mu (see
// distance_snapshot_race_test.go, which parks a compute inside
// tx.ParsedDecoded()), so writing those string fields in place would be a
// data race, not a refresh. It affects only the in-memory fallbacks — the
// packets-page channel filter and the Audio Lab buckets — never the Channels
// page, and the next server start loads the rewritten columns. This is the
// same boundary the #188 resolved_path backfill already documents.

// decodeBackfillStateTable is the one-row progress table the ingestor writes.
const decodeBackfillStateTable = "decode_backfill_state"

// decodeBackfillAbsentRecheck is how many poller ticks pass between probes
// once the state table was found missing. The poller ticks about once a
// second, so this is roughly a minute — slow enough that a database no #380
// ingestor will ever touch costs one failed query a minute, soon enough that
// an ingestor started after this server is picked up without a restart.
const decodeBackfillAbsentRecheck = 60

// DecodeBackfillStatus is the read-only view reported on /api/healthz.
// Status is "absent" when no ingestor has ever run the backfill against this
// database (or the ingestor is older than #380).
type DecodeBackfillStatus struct {
	Status         string `json:"status"`
	Kinds          string `json:"kinds,omitempty"`
	Watermark      int64  `json:"watermark"`
	Ceiling        int64  `json:"ceiling"`
	Revision       int64  `json:"revision"`
	Scanned        int64  `json:"scanned"`
	ChannelUpdated int64  `json:"channelUpdated"`
	ScopeUpdated   int64  `json:"scopeUpdated"`
	Ambiguous      int64  `json:"ambiguous"`
	DecodeErrors   int64  `json:"decodeErrors"`
	UpdatedAt      string `json:"updatedAt,omitempty"`
}

// decodeBackfillStatus reads the state row. It is deliberately not cached:
// unlike the route_mask status (a COUNT over transmissions) this is one
// primary-key lookup of a single row, and a cache would make /api/healthz
// report a finished pass seconds late — the one thing an operator watching a
// backfill is reading it for. A missing table or row is "absent", not an
// error: a database written by a pre-#380 ingestor has neither.
func (db *DB) decodeBackfillStatus() DecodeBackfillStatus {
	out := DecodeBackfillStatus{Status: "absent"}
	if db == nil || db.conn == nil {
		return out
	}
	var st DecodeBackfillStatus
	err := db.conn.QueryRow(`SELECT kinds, watermark, ceiling, status, revision, scanned,
		channel_updated, scope_updated, ambiguous, decode_errors, updated_at
		FROM `+decodeBackfillStateTable+` WHERE id = 1`).Scan(&st.Kinds, &st.Watermark,
		&st.Ceiling, &st.Status, &st.Revision, &st.Scanned, &st.ChannelUpdated,
		&st.ScopeUpdated, &st.Ambiguous, &st.DecodeErrors, &st.UpdatedAt)
	switch {
	case err == nil:
		return st
	case err == sql.ErrNoRows, strings.Contains(err.Error(), "no such table"):
		return out
	default:
		log.Printf("[decode-backfill] reading %s failed: %v", decodeBackfillStateTable, err)
		return out
	}
}

// decodeBackfillRevision reads just the revision counter. -1 means the table
// is absent or unreadable, which is reported once and then stops polling.
func (db *DB) decodeBackfillRevision() int64 {
	if db == nil || db.conn == nil {
		return -1
	}
	var rev int64
	err := db.conn.QueryRow(`SELECT revision FROM ` + decodeBackfillStateTable + ` WHERE id = 1`).Scan(&rev)
	switch {
	case err == nil:
		return rev
	case err == sql.ErrNoRows:
		// Table exists, no run recorded yet: keep polling, nothing changed.
		return 0
	case strings.Contains(err.Error(), "no such table"):
		return -1
	default:
		log.Printf("[decode-backfill] reading the revision failed: %v", err)
		return -1
	}
}

// RefreshDecodeBackfill polls the ingestor's decode-backfill revision and, on
// growth, drops every cache that could still serve a pre-rewrite channel or
// scope row. The first call only records the baseline, so a server that
// starts next to a running backfill does not invalidate on its first tick.
// Returns whether it invalidated. Read-only; the poller calls it every tick.
func (s *PacketStore) RefreshDecodeBackfill() bool {
	if s == nil || s.db == nil || s.db.conn == nil {
		return false
	}
	s.decodeBackfillMu.Lock()
	defer s.decodeBackfillMu.Unlock()
	if s.decodeBackfillState < 0 {
		// The table was missing. It can still appear while this server runs:
		// the ingestor creates it lazily on its first pass, which on a joint
		// deploy can land after this server's first tick. So re-probe on a
		// slow cadence instead of latching off, and keep the common case
		// (a database no #380 ingestor will ever touch) down to one failed
		// query per decodeBackfillAbsentRecheck ticks.
		s.decodeBackfillAbsentTicks++
		if s.decodeBackfillAbsentTicks < decodeBackfillAbsentRecheck {
			return false
		}
		s.decodeBackfillAbsentTicks = 0
	}
	rev := s.db.decodeBackfillRevision()
	if rev < 0 {
		s.decodeBackfillState = -1
		return false
	}
	if s.decodeBackfillState < 0 {
		// It just appeared: take its revision as the baseline rather than
		// treating every row the ingestor had already rewritten as news.
		s.decodeBackfillState = 1
		s.decodeBackfillRev = rev
		return false
	}
	if s.decodeBackfillState == 0 {
		s.decodeBackfillState = 1
		s.decodeBackfillRev = rev
		return false
	}
	if rev <= s.decodeBackfillRev {
		return false
	}
	s.decodeBackfillRev = rev
	s.invalidateDecodeBackfillCaches()
	log.Printf("[decode-backfill] ingestor rewrote decoded rows (revision %d); channel and scope caches dropped", rev)
	return true
}

// invalidateDecodeBackfillCaches drops the channel/scope caches on both
// levels: the DB-level channel list caches that back /api/channels, and the
// store-level analytics caches.
func (s *PacketStore) invalidateDecodeBackfillCaches() {
	s.db.channelsCache.reset()
	s.db.encChannelsCache.reset()
	// hasChannelData clears the store-level channel caches (channelsCacheRes,
	// which backs the no-database fallback of GET /api/channels, and
	// chanCache). The in-memory analytics caches that read a transmission's
	// decoded payload are deliberately not in this set: those values do not
	// change under a running server (see the note at the top of this file).
	s.invalidateCachesFor(cacheInvalidation{hasChannelData: true})
}
