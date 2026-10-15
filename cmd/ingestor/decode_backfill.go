package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Historical channel and region decoding (#380).
//
// CoreScope resolves channel keys and region scopes when a packet is
// ingested. Adding a key later — a hashChannels/channelKeys entry plus a
// SIGHUP or restart, an approved shared channel (internal/channelregistry),
// or an approved region scope (#375) — helps future packets only: stored
// `enc_<HEX>` GRP_TXT rows and transport-scoped rows with an unknown region
// stay unresolved even though their raw frame is still in the database.
//
// This pass re-decodes exactly those rows from transmissions.raw_hex through
// the normal decode path and rewrites channel_hash, decoded_json and
// scope_name so a backfilled row is byte-identical to the same packet
// ingested fresh. It is owned by the ingestor: cmd/server stays read-only
// (#1283).
//
// Trigger: growth of the effective key set. hotKeys already publishes both
// effective sets with every lifecycle layer merged (config base ∪ approved),
// so the watcher compares those snapshots against the keys a completed run
// has already covered (decode_backfill_keys). A key is its name *and* its
// value (decodeBackfillKeyIdentity): a new value under a covered name is a new
// key and arms a pass. Losing a key arms nothing and rewrites nothing — it
// only forgets that key, so that re-adding it later starts a fresh pass over
// the whole table (see forgetDecodeBackfillKeys for why that is needed, and
// why it is driven by the removal itself rather than by the next poll).
//
// A revocation reaches a pass that is already running, not only the next one:
// the batch loop re-reads the effective set before every batch and drops a
// revoked or replaced key from its snapshot (dropRevokedRunKeys), and a batch
// re-checks the set inside its write transaction and starts over without the
// key if it changed after the batch decoded. So no row is persisted from a
// decryption with a key that was no longer effective when the batch wrote,
// and the pass is not credited with it. Rows written before the revocation
// keep their plaintext, which is exactly what an ordinary revocation does for
// rows decrypted at ingest before it.
//
// Side effects are not replayed: the pass rewrites channel_hash, decoded_json
// and scope_name and nothing else. Live ingest additionally records a
// ping_triggers row for a decrypted GRP_TXT whose text is a ping trigger
// (db.go); a backfilled row deliberately does not get one, so adding a key
// does not retroactively change the ping leaderboard. The message itself is
// fully readable; only that leaderboard ignores backfilled history.
//
// Bounds (ingestion stays responsive):
//   - one run at a time, with one watermark, resumed from the database;
//   - each batch covers a bounded id window and at most batchSize matching
//     rows, so a batch with sparse matches cannot become one unbounded range
//     scan on the single SQLite connection;
//   - decoding happens in Go outside any transaction; one WriterTx per batch,
//     so a live insert queues behind a batch, not behind the run;
//   - a cancellable pause between batches.
//
// Resume and idempotence: the row rewrites, the new watermark and the
// counters commit in one transaction, so a crash or a SIGTERM repeats at most
// the batch that did not commit. Every UPDATE carries its own guard
// (channel_hash still `enc_%`, scope_name still ''), so a repeat is a no-op
// and an already-resolved row is never touched — idempotence does not depend
// on the watermark being exact.
//
// Server visibility: the channel and scope surfaces that matter are
// SQL-backed on the server (/api/channels, /api/channels/{hash}/messages,
// the encrypted-channel list, the scope_name queries), so they serve
// rewritten rows as soon as their caches are dropped. A batch that changed
// a row bumps decode_backfill_state.revision, which the server polls (one
// row per tick) to invalidate those caches. See cmd/server/decode_backfill.go
// for what stays until the next server start, and why.

const decodeBackfillComponent = "decode_backfill"

// Kind names, used in decode_backfill_state.kinds and decode_backfill_keys.
const (
	decodeBackfillKindChannel = "channel"
	decodeBackfillKindScope   = "scope"
)

// Defaults; DecodeBackfillConfig overrides them (AGENTS.md rule 8: customizer
// candidates later).
const (
	defaultDecodeBackfillBatchSize = 300
	defaultDecodeBackfillIDWindow  = 20000
	defaultDecodeBackfillPause     = 250 * time.Millisecond
	defaultDecodeBackfillPoll      = 30 * time.Second
	// decodeBackfillProgressEvery logs progress every N batches.
	decodeBackfillProgressEvery = 50
)

// DecodeBackfillConfig is the "decodeBackfill" config block (documented in
// config.example.json). 0 or a negative value means the default for every
// numeric field, so the pause between batches cannot be turned off.
type DecodeBackfillConfig struct {
	Disabled   bool `json:"disabled,omitempty"`
	BatchSize  int  `json:"batchSize,omitempty"`   // matching rows per batch; default 300
	IDWindow   int  `json:"idWindow,omitempty"`    // transmissions.id span scanned per batch; default 20000
	PauseMs    int  `json:"pauseMs,omitempty"`     // pause between batches; default 250
	PollSecond int  `json:"pollSeconds,omitempty"` // key-set poll interval; default 30
}

// DecodeBackfillSettings is the effective, validated configuration.
type DecodeBackfillSettings struct {
	Enabled   bool
	BatchSize int
	IDWindow  int64
	Pause     time.Duration
	Poll      time.Duration
}

// DecodeBackfillSettings returns the effective settings for this config.
func (c *Config) DecodeBackfillSettings() DecodeBackfillSettings {
	s := DecodeBackfillSettings{
		Enabled:   true,
		BatchSize: defaultDecodeBackfillBatchSize,
		IDWindow:  defaultDecodeBackfillIDWindow,
		Pause:     defaultDecodeBackfillPause,
		Poll:      defaultDecodeBackfillPoll,
	}
	if b := c.DecodeBackfill; b != nil {
		s.Enabled = !b.Disabled
		if b.BatchSize > 0 {
			s.BatchSize = b.BatchSize
		}
		if b.IDWindow > 0 {
			s.IDWindow = int64(b.IDWindow)
		}
		if b.PauseMs > 0 {
			s.Pause = time.Duration(b.PauseMs) * time.Millisecond
		}
		if b.PollSecond > 0 {
			s.Poll = time.Duration(b.PollSecond) * time.Second
		}
	}
	return s
}

// decodeBackfillKeyIdentity is the identity a key is recorded under: a
// sha256 over the length-prefixed name followed by the key bytes. The value
// is part of it because channelKeys and the rainbow file map a name to an
// explicit key, so an operator can replace the key behind a name; that is a
// different key with different history to decode, and the old value must stop
// being used. Only the digest is stored — never the name or the key in clear,
// and log lines report kinds and counts only. For a hashtag channel the key is
// sha256 of the name, so the digest reveals nothing the name did not; for a
// pre-shared key it is a one-way digest of a 128-bit secret.
func decodeBackfillKeyIdentity(name string, key []byte) string {
	var n [binary.MaxVarintLen64]byte
	h := sha256.New()
	h.Write(n[:binary.PutUvarint(n[:], uint64(len(name)))])
	h.Write([]byte(name))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))
}

// channelKeyBytes is the key a channel-key string stands for. Channel keys are
// hex, in either case; a value that is not hex (which decrypts nothing) is
// identified by its raw bytes.
func channelKeyBytes(hexKey string) []byte {
	if b, err := hex.DecodeString(hexKey); err == nil {
		return b
	}
	return []byte(hexKey)
}

// sameChannelKey reports whether two channel-key strings are the same key.
func sameChannelKey(a, b string) bool {
	return a == b || bytes.Equal(channelKeyBytes(a), channelKeyBytes(b))
}

// removedKey is a key that left the effective set, or whose value under its
// name was replaced: hotKeys reports one per removal (onRemoved).
type removedKey struct {
	kind string // decodeBackfillKindChannel or decodeBackfillKindScope
	name string
	key  []byte
}

func (k removedKey) identity() string { return decodeBackfillKeyIdentity(k.name, k.key) }

// DecodeBackfillState is the persisted progress of the current or last run.
type DecodeBackfillState struct {
	Kinds          string
	Watermark      int64
	Ceiling        int64
	Status         string // "running" | "done"
	Revision       int64
	Scanned        int64
	ChannelUpdated int64
	ScopeUpdated   int64
	Ambiguous      int64
	DecodeErrors   int64
	StartedAt      string
	UpdatedAt      string
}

func ensureDecodeBackfillTables(db *sql.DB) error {
	// PREFLIGHT: async=true reason="CREATE TABLE IF NOT EXISTS for a one-row state table and a key-digest table bounded by the configured key count; constant cost at any scale"
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS decode_backfill_state (
		id              INTEGER PRIMARY KEY CHECK (id = 1),
		kinds           TEXT    NOT NULL,
		watermark       INTEGER NOT NULL,
		ceiling         INTEGER NOT NULL,
		status          TEXT    NOT NULL,
		revision        INTEGER NOT NULL DEFAULT 0,
		scanned         INTEGER NOT NULL DEFAULT 0,
		channel_updated INTEGER NOT NULL DEFAULT 0,
		scope_updated   INTEGER NOT NULL DEFAULT 0,
		ambiguous       INTEGER NOT NULL DEFAULT 0,
		decode_errors   INTEGER NOT NULL DEFAULT 0,
		started_at      TEXT    NOT NULL,
		updated_at      TEXT    NOT NULL
	)`); err != nil {
		return fmt.Errorf("create decode_backfill_state: %w", err)
	}
	// An earlier development build recorded sha256(name) under name_hash.
	// Those digests match no key identity, so the rows are useless; dropping
	// the table only means the next poll arms one fresh pass.
	var legacy int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('decode_backfill_keys') WHERE name = 'name_hash'`).Scan(&legacy); err != nil {
		return fmt.Errorf("inspect decode_backfill_keys: %w", err)
	}
	if legacy > 0 {
		// PREFLIGHT: async=true reason="drops a bookkeeping table bounded by the configured key count; constant cost at any scale"
		if _, err := db.Exec(`DROP TABLE decode_backfill_keys`); err != nil {
			return fmt.Errorf("drop legacy decode_backfill_keys: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS decode_backfill_keys (
		kind     TEXT NOT NULL,
		key_hash TEXT NOT NULL,
		state    TEXT NOT NULL,
		PRIMARY KEY (kind, key_hash)
	)`); err != nil {
		return fmt.Errorf("create decode_backfill_keys: %w", err)
	}
	return nil
}

// DecodeBackfillState reads the persisted state, or nil when no run has ever
// been recorded.
func (s *Store) DecodeBackfillState() (*DecodeBackfillState, error) {
	var st DecodeBackfillState
	err := s.db.QueryRow(`SELECT kinds, watermark, ceiling, status, revision, scanned,
		channel_updated, scope_updated, ambiguous, decode_errors, started_at, updated_at
		FROM decode_backfill_state WHERE id = 1`).Scan(&st.Kinds, &st.Watermark, &st.Ceiling,
		&st.Status, &st.Revision, &st.Scanned, &st.ChannelUpdated, &st.ScopeUpdated,
		&st.Ambiguous, &st.DecodeErrors, &st.StartedAt, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// decodeBackfillTablesExist reports whether a pass has ever created the
// bookkeeping tables. A disabled ingestor and the removal hook use it so they
// never create them.
func (s *Store) decodeBackfillTablesExist() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'decode_backfill_keys'`).Scan(&n)
	return n > 0, err
}

// coveredKeyHashes returns the key identities already recorded for kind, with
// the value reporting whether the identity is "covered" (a run that used it
// reached its ceiling) rather than merely "running".
func (s *Store) coveredKeyHashes(kind string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT key_hash, state FROM decode_backfill_keys WHERE kind = ?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h, state string
		if err := rows.Scan(&h, &state); err != nil {
			return nil, err
		}
		out[h] = state == "covered"
	}
	return out, rows.Err()
}

// decodeBackfillArmedKinds reports which kinds have at least one live key
// identity that no completed run has covered. Growth only: losing a key never
// arms a run, and no resolved row is ever revisited.
func (s *Store) decodeBackfillArmedKinds(live map[string][]string) ([]string, error) {
	var armed []string
	for _, kind := range []string{decodeBackfillKindChannel, decodeBackfillKindScope} {
		known, err := s.coveredKeyHashes(kind)
		if err != nil {
			return nil, err
		}
		for _, id := range live[kind] {
			if !known[id] {
				armed = append(armed, kind)
				break
			}
		}
	}
	return armed, nil
}

// newIngestorHotKeys builds the live key sets with the decode backfill's
// removal hook wired, so a key leaving an effective set is forgotten on the
// event itself, whether or not the pass is enabled. main and the tests build
// their key sets here, so the wiring cannot be dropped without a test failing.
func newIngestorHotKeys(s *Store, channelKeys map[string]string, regionKeys map[string][]byte) *hotKeys {
	hk := newHotKeys(channelKeys, regionKeys)
	hk.onRemoved(s.forgetDecodeBackfillKeys)
	return hk
}

// forgetDecodeBackfillKeys is the removal hook main wires to hotKeys.onRemoved:
// it drops the record of every key that just left the effective set (or whose
// value was replaced), so re-adding it later arms a fresh pass. Without this,
// a key that was covered, then revoked (or removed from the config), then
// approved again would leave the packets received during the gap unresolved
// forever: they were ingested after the run that credited the key, so no
// later run would be armed for it.
//
// It runs on the removal itself, synchronously, not on the watcher's next
// look at the key set: a revoke followed by a re-approve between two looks —
// within one batch, within one poll, across two SIGHUPs, or for as long as
// decodeBackfill.disabled keeps the watcher from looking at all — would
// otherwise never be seen. It also runs when the pass is disabled, but only
// ever deletes rows from a table a previous pass created; it never creates
// one. Revocations and reloads are administrator events, so this is one small
// write per event, never per packet.
//
// Nothing is rewritten here — only the bookkeeping that decides whether a run
// is needed. Deleting the "running" row of a key the current pass still uses
// only means the pass is not credited with it, and the next poll arms a fresh
// pass: conservative, never a loss.
func (s *Store) forgetDecodeBackfillKeys(removed []removedKey) {
	n, err := s.deleteDecodeBackfillKeys(removed)
	if err != nil {
		log.Printf("[%s] could not forget %d removed key(s): %v", decodeBackfillComponent, len(removed), err)
		return
	}
	if n > 0 {
		log.Printf("[%s] %d recorded key(s) left the effective set; forgotten, so re-adding one starts a new pass", decodeBackfillComponent, n)
	}
}

// deleteDecodeBackfillKeys deletes the bookkeeping rows of removed, if the
// table exists, and returns how many rows it deleted.
func (s *Store) deleteDecodeBackfillKeys(removed []removedKey) (int64, error) {
	if len(removed) == 0 {
		return 0, nil
	}
	var deleted int64
	err := s.WriterTx(decodeBackfillComponent, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'decode_backfill_keys'`).Scan(&n); err != nil || n == 0 {
			return err
		}
		for _, k := range removed {
			res, err := tx.Exec(`DELETE FROM decode_backfill_keys WHERE kind = ? AND key_hash = ?`, k.kind, k.identity())
			if err != nil {
				return err
			}
			if a, _ := res.RowsAffected(); a > 0 {
				deleted += a
			}
		}
		return nil
	})
	return deleted, err
}

// forgetGoneKeys drops the record of every key that is not effective now. It
// covers what no in-process removal event can: a key removed from the config
// across a restart. RunDecodeBackfill calls it on every poll and a disabled
// ingestor once at start-up. A reload that cannot read its config keeps every
// previous key (hotKeys.reload bails before storing), so a bad config cannot
// make this forget anything.
//
// It also applies to a run that is resumed after a key it started with was
// revoked: the resumed run reads its snapshot from these rows, so it stops
// using that key for the rest of the table. That is the revocation taking
// effect, not a loss — a later re-approval arms a fresh pass over the whole
// table.
func (s *Store) forgetGoneKeys(live map[string][]string) error {
	for _, kind := range []string{decodeBackfillKindChannel, decodeBackfillKindScope} {
		known, err := s.coveredKeyHashes(kind)
		if err != nil {
			return err
		}
		if len(known) == 0 {
			continue
		}
		liveIDs := make(map[string]bool, len(live[kind]))
		for _, id := range live[kind] {
			liveIDs[id] = true
		}
		var gone []string
		for h := range known {
			if !liveIDs[h] {
				gone = append(gone, h)
			}
		}
		if len(gone) == 0 {
			continue
		}
		sort.Strings(gone)
		if err := s.WriterTx(decodeBackfillComponent, func(tx *sql.Tx) error {
			for _, h := range gone {
				if _, err := tx.Exec(`DELETE FROM decode_backfill_keys WHERE kind = ? AND key_hash = ?`, kind, h); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		log.Printf("[%s] %d %s key(s) are no longer effective; forgotten, so re-adding one starts a new pass", decodeBackfillComponent, len(gone), kind)
	}
	return nil
}

// decodeBackfillRun is one pass: the kinds it rewrites, the key snapshot it
// decodes with, and its id bounds.
type decodeBackfillRun struct {
	channel     bool
	scope       bool
	channelKeys map[string]string
	regionKeys  map[string][]byte
	watermark   int64
	ceiling     int64
}

func (r *decodeBackfillRun) kinds() string {
	var out []string
	if r.channel {
		out = append(out, decodeBackfillKindChannel)
	}
	if r.scope {
		out = append(out, decodeBackfillKindScope)
	}
	return strings.Join(out, ",")
}

// usableChannel and usableScope report which armed kinds still have a key the
// run may decode with. They differ from channel/scope only after
// dropRevokedRunKeys emptied a snapshot mid-run (or a resumed run found every
// key of one kind gone): the kind stays armed for the bookkeeping, but the
// candidate predicate narrows so the remaining pass does not keep selecting
// rows nothing can resolve. The crediting in finishDecodeBackfillRun uses
// channel/scope, not these — a kind whose keys were dropped has no key rows
// left to credit.
func (r *decodeBackfillRun) usableChannel() bool { return r.channel && len(r.channelKeys) > 0 }
func (r *decodeBackfillRun) usableScope() bool   { return r.scope && len(r.regionKeys) > 0 }

// decodeBackfillCandidateSQL is the unresolved-row predicate for the armed
// kinds. Channel candidates are GRP_TXT rows still carrying the `enc_<HEX>`
// routing placeholder (BuildPacketData writes channel_hash only for
// payload_type 5, so GRP_DATA has no placeholder to upgrade and is out of
// scope). Scope candidates are transport-scoped rows whose region is unknown:
// scopeNameForDB writes NULL for "not transport-scoped" and the empty string
// for "transport-scoped, region unknown".
func decodeBackfillCandidateSQL(channel, scope bool) string {
	var preds []string
	if channel {
		preds = append(preds, `(payload_type = 5 AND channel_hash LIKE 'enc_%')`)
	}
	if scope {
		preds = append(preds, `(scope_name = '')`)
	}
	if len(preds) == 0 {
		return ""
	}
	return "(" + strings.Join(preds, " OR ") + ")"
}

// startDecodeBackfillRun records a new run (or resumes an unfinished one) and
// returns it with the key snapshot it must decode with. A run's snapshot is
// persisted as the "running" rows of decode_backfill_keys, so a resume after
// a crash uses the keys the run started with — a key added mid-run stays
// uncovered and arms the next run instead of being silently credited.
func (s *Store) startDecodeBackfillRun(armed []string, channelKeys map[string]string, regionKeys map[string][]byte) (*decodeBackfillRun, error) {
	st, err := s.DecodeBackfillState()
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	run := &decodeBackfillRun{}
	if st != nil && st.Status == "running" {
		for _, k := range strings.Split(st.Kinds, ",") {
			switch k {
			case decodeBackfillKindChannel:
				run.channel = true
			case decodeBackfillKindScope:
				run.scope = true
			}
		}
		run.watermark, run.ceiling = st.Watermark, st.Ceiling
		if !run.channel && !run.scope {
			// A state row with no usable kinds cannot make progress; close it
			// so the watcher can arm a fresh run.
			if err := s.finishDecodeBackfillRun(run); err != nil {
				return nil, err
			}
			return nil, nil
		}
	} else {
		if len(armed) == 0 {
			return nil, nil
		}
		for _, k := range armed {
			switch k {
			case decodeBackfillKindChannel:
				run.channel = true
			case decodeBackfillKindScope:
				run.scope = true
			}
		}
		now := time.Now().UTC().Format(time.RFC3339)
		kinds := run.kinds()
		if decodeBackfillPreSnapshotHook != nil {
			decodeBackfillPreSnapshotHook()
		}
		if err := s.WriterTx(decodeBackfillComponent, func(tx *sql.Tx) error {
			// The ceiling is read under the writer lock, in the transaction
			// that records the snapshot (#467). A removal hook that ran before
			// this point has already deleted its row, and any row it let
			// through is at or below the ceiling, so the pass resolves it. A
			// removal after it finds the "running" rows and deletes them.
			// Read earlier, a row ingested between a revoke and a re-approve
			// that both land before the snapshot would sit above the ceiling
			// of a pass that is nevertheless credited with the key.
			var ceiling int64
			if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM transmissions`).Scan(&ceiling); err != nil {
				return fmt.Errorf("read ceiling: %w", err)
			}
			run.ceiling = ceiling
			if _, err := tx.Exec(`INSERT INTO decode_backfill_state
				(id, kinds, watermark, ceiling, status, revision, scanned, channel_updated, scope_updated, ambiguous, decode_errors, started_at, updated_at)
				VALUES (1, ?, 0, ?, 'running', COALESCE((SELECT revision FROM decode_backfill_state WHERE id = 1), 0), 0, 0, 0, 0, 0, ?, ?)
				ON CONFLICT(id) DO UPDATE SET kinds = excluded.kinds, watermark = 0, ceiling = excluded.ceiling,
					status = 'running', scanned = 0, channel_updated = 0, scope_updated = 0, ambiguous = 0,
					decode_errors = 0, started_at = excluded.started_at, updated_at = excluded.updated_at`,
				kinds, ceiling, now, now); err != nil {
				return err
			}
			// The snapshot: every live name of an armed kind. An existing
			// "covered" row is left alone by OR IGNORE.
			for _, kind := range []string{decodeBackfillKindChannel, decodeBackfillKindScope} {
				if (kind == decodeBackfillKindChannel && !run.channel) || (kind == decodeBackfillKindScope && !run.scope) {
					continue
				}
				for _, id := range liveKeyIdentities(kind, channelKeys, regionKeys) {
					if _, err := tx.Exec(`INSERT OR IGNORE INTO decode_backfill_keys (kind, key_hash, state) VALUES (?, ?, 'running')`,
						kind, id); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("record run: %w", err)
		}
	}

	// Decode with the run's own snapshot: the live keys whose identity this
	// run recorded. On a resume that is the set the run started with, minus
	// any key revoked or replaced since.
	if run.channel {
		known, err := s.coveredKeyHashes(decodeBackfillKindChannel)
		if err != nil {
			return nil, err
		}
		run.channelKeys = make(map[string]string, len(channelKeys))
		for name, key := range channelKeys {
			if _, ok := known[decodeBackfillKeyIdentity(name, channelKeyBytes(key))]; ok {
				run.channelKeys[name] = key
			}
		}
	}
	if run.scope {
		known, err := s.coveredKeyHashes(decodeBackfillKindScope)
		if err != nil {
			return nil, err
		}
		run.regionKeys = make(map[string][]byte, len(regionKeys))
		for name, key := range regionKeys {
			if _, ok := known[decodeBackfillKeyIdentity(name, key)]; ok {
				run.regionKeys[name] = key
			}
		}
	}
	return run, nil
}

// liveKeyIdentities returns the sorted identities of the live keys of one
// kind, so the recorded snapshot does not depend on Go's map iteration order.
func liveKeyIdentities(kind string, channelKeys map[string]string, regionKeys map[string][]byte) []string {
	var out []string
	switch kind {
	case decodeBackfillKindChannel:
		for name, key := range channelKeys {
			out = append(out, decodeBackfillKeyIdentity(name, channelKeyBytes(key)))
		}
	case decodeBackfillKindScope:
		for name, key := range regionKeys {
			out = append(out, decodeBackfillKeyIdentity(name, key))
		}
	}
	sort.Strings(out)
	return out
}

// finishDecodeBackfillRun marks the run done and promotes its key snapshot to
// "covered" in the same transaction, so a crash can never credit a key whose
// run did not reach the ceiling.
func (s *Store) finishDecodeBackfillRun(run *decodeBackfillRun) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.WriterTx(decodeBackfillComponent, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE decode_backfill_state SET status = 'done', updated_at = ? WHERE id = 1`, now); err != nil {
			return err
		}
		for _, kind := range []string{decodeBackfillKindChannel, decodeBackfillKindScope} {
			if (kind == decodeBackfillKindChannel && !run.channel) || (kind == decodeBackfillKindScope && !run.scope) {
				continue
			}
			if _, err := tx.Exec(`UPDATE decode_backfill_keys SET state = 'covered' WHERE kind = ?`, kind); err != nil {
				return err
			}
		}
		return nil
	})
}

// revokedKeys lists the keys of the run's snapshot that are no longer
// effective with the value the run decodes with: revoked, removed from the
// config, or replaced by a new value under the same name. It does not change
// the run. One map probe per snapshot key, on the published pointer hotKeys
// hands out without locking.
func (r *decodeBackfillRun) revokedKeys(hk *hotKeys) []removedKey {
	var out []removedKey
	if len(r.channelKeys) > 0 {
		live := hk.Channels()
		for name, key := range r.channelKeys {
			if lk, ok := live[name]; !ok || !sameChannelKey(lk, key) {
				out = append(out, removedKey{decodeBackfillKindChannel, name, channelKeyBytes(key)})
			}
		}
	}
	if len(r.regionKeys) > 0 {
		live := hk.Regions()
		for name, key := range r.regionKeys {
			if lk, ok := live[name]; !ok || !bytes.Equal(lk, key) {
				out = append(out, removedKey{decodeBackfillKindScope, name, key})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].name < out[j].name
	})
	return out
}

// dropRevokedRunKeys removes every revokedKeys entry from the run's snapshot
// and returns how many it dropped. The batch loop calls it before every batch
// and once more before finishing, and a batch calls it when its in-transaction
// re-check found the key set changed, so a revocation (or a replaced value)
// that lands while a pass is walking the table takes effect within one batch
// instead of at the end of the pass.
//
// It also deletes the key's bookkeeping row, which the removal hook
// (forgetDecodeBackfillKeys) has normally done already. Two consequences, both
// wanted: finishDecodeBackfillRun cannot credit a key that was revoked under
// it, and re-approving the key later arms a fresh pass over the whole table,
// so the rows this pass walked past with it are not lost. A replaced value is
// the same: the old value is never credited, and the new one — which the run
// never recorded — arms the next pass.
//
// Rows the pass had already rewritten keep their plaintext. That is the same
// answer an ordinary revocation gives for rows decrypted at ingest before it
// ("Historical messages already decoded and stored are NOT deleted",
// cmd/server/openapi.go): a revocation stops future decryption, it does not
// re-encrypt history. Clearing them would also mean rewriting rows the pass
// already committed, which the "never touch a resolved row" guard forbids.
func (s *Store) dropRevokedRunKeys(run *decodeBackfillRun, hk *hotKeys) (int, error) {
	gone := run.revokedKeys(hk)
	if len(gone) == 0 {
		return 0, nil
	}
	for _, g := range gone {
		if g.kind == decodeBackfillKindChannel {
			delete(run.channelKeys, g.name)
		} else {
			delete(run.regionKeys, g.name)
		}
	}
	if _, err := s.deleteDecodeBackfillKeys(gone); err != nil {
		return 0, err
	}
	log.Printf("[%s] %d key(s) were revoked or replaced while the pass was running; dropped from the pass, %d channel and %d region key(s) left",
		decodeBackfillComponent, len(gone), len(run.channelKeys), len(run.regionKeys))
	return len(gone), nil
}

// decodeBackfillRow is one candidate row.
type decodeBackfillRow struct {
	id          int64
	rawHex      string
	payloadType int
	channelHash sql.NullString
	scopeName   sql.NullString
}

// decodeBackfillBatchResult reports one batch.
type decodeBackfillBatchResult struct {
	Scanned        int
	ChannelUpdated int
	ScopeUpdated   int
	Ambiguous      int
	DecodeErrors   int
	Next           int64 // the new watermark
	Done           bool  // the window reached the ceiling
	Hold           time.Duration
}

// DecodeBackfillResult reports one run.
type DecodeBackfillResult struct {
	Kinds          string
	From, Ceiling  int64
	Batches        int
	Scanned        int
	ChannelUpdated int
	ScopeUpdated   int
	Ambiguous      int
	DecodeErrors   int
	MaxHold        time.Duration
}

// decodeBackfillValidateSignatures mirrors the ingest decode flag. It is a
// var so a test can pin the value the backfill decodes with; production sets
// it once from the config at start-up.
var decodeBackfillValidateSignatures bool

// errDecodeBackfillKeysChanged aborts a batch whose write transaction found a
// key of the run's snapshot revoked or replaced after the batch decoded.
var errDecodeBackfillKeysChanged = errors.New("key set changed under the batch")

// decodeBackfillBatch re-decodes one bounded id window and commits the
// rewrites, the new watermark and the counters in a single transaction.
//
// The write transaction first re-checks the run's keys against hk. If one was
// revoked or replaced after the batch decoded its rows, the transaction is
// rolled back, the key leaves the snapshot (dropRevokedRunKeys) and the batch
// is redone from the same watermark. That is the filter on the updates, done
// by redoing rather than by picking updates apart: the decoder reports a
// decrypted channel under its normalised name (normalizeChannelName), so an
// update cannot always be traced back to the snapshot key that produced it.
// It costs one more decode of at most batchSize rows, only in a batch a
// revocation actually landed in; the snapshot only shrinks, so it terminates.
func (s *Store) decodeBackfillBatch(ctx context.Context, run *decodeBackfillRun, hk *hotKeys, set DecodeBackfillSettings) (decodeBackfillBatchResult, error) {
	for {
		b, err := s.decodeBackfillBatchOnce(ctx, run, hk, set)
		if !errors.Is(err, errDecodeBackfillKeysChanged) {
			return b, err
		}
		if _, err := s.dropRevokedRunKeys(run, hk); err != nil {
			return decodeBackfillBatchResult{Next: run.watermark}, fmt.Errorf("re-read key set: %w", err)
		}
	}
}

func (s *Store) decodeBackfillBatchOnce(ctx context.Context, run *decodeBackfillRun, hk *hotKeys, set DecodeBackfillSettings) (decodeBackfillBatchResult, error) {
	var b decodeBackfillBatchResult
	b.Next = run.watermark
	windowEnd := run.watermark + set.IDWindow
	if windowEnd > run.ceiling {
		windowEnd = run.ceiling
	}
	pred := decodeBackfillCandidateSQL(run.usableChannel(), run.usableScope())
	if pred == "" {
		b.Next, b.Done = run.ceiling, true
		return b, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(raw_hex, ''), COALESCE(payload_type, -1), channel_hash, scope_name
		FROM transmissions WHERE id > ? AND id <= ? AND `+pred+` ORDER BY id LIMIT ?`,
		run.watermark, windowEnd, set.BatchSize)
	if err != nil {
		return b, fmt.Errorf("select batch: %w", err)
	}
	batch := make([]decodeBackfillRow, 0, set.BatchSize)
	for rows.Next() {
		var r decodeBackfillRow
		if err := rows.Scan(&r.id, &r.rawHex, &r.payloadType, &r.channelHash, &r.scopeName); err != nil {
			rows.Close()
			return b, fmt.Errorf("scan batch: %w", err)
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return b, fmt.Errorf("read batch: %w", err)
	}
	b.Scanned = len(batch)

	// The LIMIT truncated the window: continue from the last row read. Only a
	// window read to its end moves the watermark to the window end.
	if len(batch) == set.BatchSize {
		b.Next = batch[len(batch)-1].id
	} else {
		b.Next = windowEnd
	}
	b.Done = b.Next >= run.ceiling

	// Decode outside any transaction, through the normal decode path, so a
	// rewritten row is byte-identical to the same packet ingested fresh in the
	// three columns this pass owns. Ingest's one side effect on a decrypted
	// GRP_TXT — the ping_triggers row — is deliberately not replayed; see the
	// note at the top of this file.
	type chanUpdate struct {
		id          int64
		channelHash string
		decodedJSON string
	}
	type scopeUpdate struct {
		id   int64
		name string
	}
	var chanUpdates []chanUpdate
	var scopeUpdates []scopeUpdate
	for _, r := range batch {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		decoded, err := DecodePacket(r.rawHex, run.channelKeys, decodeBackfillValidateSignatures)
		if err != nil {
			b.DecodeErrors++
			continue
		}
		if run.usableChannel() && r.payloadType == PayloadGRP_TXT && strings.HasPrefix(r.channelHash.String, "enc_") {
			// Same rule as BuildPacketData: a decrypted GRP_TXT is keyed by
			// its channel name, an undecryptable one keeps enc_<HEX>.
			if decoded.Payload.Type == "CHAN" && decoded.Payload.Channel != "" {
				chanUpdates = append(chanUpdates, chanUpdate{r.id, decoded.Payload.Channel, PayloadJSON(&decoded.Payload)})
			}
		}
		if run.usableScope() && r.scopeName.Valid && r.scopeName.String == "" {
			if decoded.TransportCodes != nil && decoded.TransportCodes.Code1 != "0000" {
				name, matches := matchScopeDetail(run.regionKeys, byte(decoded.Header.PayloadType), decoded.payloadRaw, decoded.TransportCodes.Code1)
				switch {
				case matches > 1:
					// Abstain: code1 is 16 bits, so two unrelated regions can
					// collide on it. Reporting unknown-scoped is the same
					// answer matchScope gives at ingest.
					b.Ambiguous++
				case name != "":
					scopeUpdates = append(scopeUpdates, scopeUpdate{r.id, name})
				}
			}
		}
	}

	if decodeBackfillPreWriteHook != nil {
		decodeBackfillPreWriteHook()
	}
	holdStart := time.Now()
	changed := 0
	err = s.WriterTx(decodeBackfillComponent, func(tx *sql.Tx) error {
		// Checked under the writer lock, so a revocation published before
		// this point cannot be persisted by this batch.
		if len(run.revokedKeys(hk)) > 0 {
			return errDecodeBackfillKeysChanged
		}
		if len(chanUpdates) > 0 {
			// The guard keeps an already-resolved row untouched, whoever
			// resolved it, and makes a repeated batch a no-op.
			stmt, err := tx.Prepare(`UPDATE transmissions SET channel_hash = ?, decoded_json = ?
				WHERE id = ? AND payload_type = 5 AND channel_hash LIKE 'enc_%'`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, u := range chanUpdates {
				res, err := stmt.Exec(u.channelHash, u.decodedJSON, u.id)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					b.ChannelUpdated++
				}
			}
		}
		if len(scopeUpdates) > 0 {
			stmt, err := tx.Prepare(`UPDATE transmissions SET scope_name = ? WHERE id = ? AND scope_name = ''`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, u := range scopeUpdates {
				res, err := stmt.Exec(u.name, u.id)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					b.ScopeUpdated++
				}
			}
		}
		changed = b.ChannelUpdated + b.ScopeUpdated
		// revision only grows when a row actually changed, so a run over
		// rows that stay unresolved does not make running servers drop
		// their caches once per batch.
		_, err := tx.Exec(`UPDATE decode_backfill_state SET watermark = ?, scanned = scanned + ?,
			channel_updated = channel_updated + ?, scope_updated = scope_updated + ?,
			ambiguous = ambiguous + ?, decode_errors = decode_errors + ?,
			revision = revision + ?, updated_at = ? WHERE id = 1`,
			b.Next, b.Scanned, b.ChannelUpdated, b.ScopeUpdated, b.Ambiguous, b.DecodeErrors,
			min(changed, 1), time.Now().UTC().Format(time.RFC3339))
		return err
	})
	b.Hold = time.Since(holdStart)
	if errors.Is(err, errDecodeBackfillKeysChanged) {
		return decodeBackfillBatchResult{Next: run.watermark}, err
	}
	if err != nil {
		b.ChannelUpdated, b.ScopeUpdated = 0, 0
		return b, fmt.Errorf("write batch: %w", err)
	}
	// Counted only after the commit, so a rolled-back batch cannot inflate
	// the perf-page counters that make a runaway backfill obvious (#1119).
	for i := 0; i < b.ChannelUpdated; i++ {
		s.Stats.IncBackfill("decode_channel")
	}
	for i := 0; i < b.ScopeUpdated; i++ {
		s.Stats.IncBackfill("decode_scope")
	}
	run.watermark = b.Next
	if decodeBackfillBatchHook != nil {
		decodeBackfillBatchHook(b)
	}
	return b, nil
}

// decodeBackfillBatchHook is a test seam, nil in production. It runs after a
// batch committed, so a test can cancel a run between batches.
var decodeBackfillBatchHook func(decodeBackfillBatchResult)

// decodeBackfillPreWriteHook is a test seam, nil in production. It runs after
// a batch decoded its rows and before it writes them, which is the window in
// which live ingest or another writer can resolve a row the batch is about to
// rewrite. The per-UPDATE guards close that window; this seam is how a test
// opens it.
var decodeBackfillPreWriteHook func()

// decodeBackfillPreSnapshotHook is a test seam, nil in production. It runs in
// startDecodeBackfillRun after the key set was read and before the transaction
// that records the run, which is the window a revoke and a re-approve can hide
// in (#467).
var decodeBackfillPreSnapshotHook func()

// RunDecodeBackfill runs one pass for the armed kinds (or resumes an
// unfinished one) and returns what it did. A nil-run (nothing armed and
// nothing to resume) returns a zero result and no error.
func (s *Store) RunDecodeBackfill(ctx context.Context, hk *hotKeys, set DecodeBackfillSettings) (DecodeBackfillResult, error) {
	var res DecodeBackfillResult
	if err := ensureDecodeBackfillTables(s.db); err != nil {
		return res, err
	}
	channelKeys, regionKeys := hk.Channels(), hk.Regions()
	live := liveDecodeBackfillKeys(channelKeys, regionKeys)
	if err := s.forgetGoneKeys(live); err != nil {
		return res, fmt.Errorf("forget gone keys: %w", err)
	}
	armed, err := s.decodeBackfillArmedKinds(live)
	if err != nil {
		return res, fmt.Errorf("arm kinds: %w", err)
	}
	run, err := s.startDecodeBackfillRun(armed, channelKeys, regionKeys)
	if err != nil || run == nil {
		return res, err
	}
	if len(run.channelKeys) == 0 && len(run.regionKeys) == 0 {
		// Nothing in the snapshot can resolve a row, so scanning the table
		// would be pure cost. Reachable after every key a resumed run started
		// with was revoked while it was parked.
		log.Printf("[%s] run for kinds=%s has no effective key left; closing it without a scan", decodeBackfillComponent, run.kinds())
		if err := s.finishDecodeBackfillRun(run); err != nil {
			return res, fmt.Errorf("finish run: %w", err)
		}
		return res, nil
	}
	res.Kinds, res.From, res.Ceiling = run.kinds(), run.watermark, run.ceiling
	start := time.Now()
	log.Printf("[%s] starting: kinds=%s ids (%d, %d], %d channel key(s), %d region key(s), batch %d, window %d, pause %s",
		decodeBackfillComponent, res.Kinds, run.watermark, run.ceiling,
		len(run.channelKeys), len(run.regionKeys), set.BatchSize, set.IDWindow, set.Pause)
	for {
		// Re-read the effective key set before every batch so a revocation
		// mid-pass is honoured (see dropRevokedRunKeys), not only by the next
		// pass.
		if _, err := s.dropRevokedRunKeys(run, hk); err != nil {
			return res, fmt.Errorf("re-read key set: %w", err)
		}
		if !run.usableChannel() && !run.usableScope() {
			log.Printf("[%s] run for kinds=%s has no effective key left; stopping at watermark %d of %d",
				decodeBackfillComponent, run.kinds(), run.watermark, run.ceiling)
			break
		}
		b, err := s.decodeBackfillBatch(ctx, run, hk, set)
		if err != nil {
			return res, err
		}
		res.Batches++
		res.Scanned += b.Scanned
		res.ChannelUpdated += b.ChannelUpdated
		res.ScopeUpdated += b.ScopeUpdated
		res.Ambiguous += b.Ambiguous
		res.DecodeErrors += b.DecodeErrors
		if b.Hold > res.MaxHold {
			res.MaxHold = b.Hold
		}
		if res.Batches%decodeBackfillProgressEvery == 0 {
			log.Printf("[%s] progress: watermark %d of %d, %d scanned, %d channel, %d scope, %d ambiguous",
				decodeBackfillComponent, run.watermark, run.ceiling, res.Scanned, res.ChannelUpdated, res.ScopeUpdated, res.Ambiguous)
		}
		if b.Done {
			break
		}
		if err := decodeBackfillWait(ctx, set.Pause); err != nil {
			return res, err
		}
	}
	// One last re-read: a key revoked after the last batch committed must not
	// be credited by the finish below.
	if _, err := s.dropRevokedRunKeys(run, hk); err != nil {
		return res, fmt.Errorf("re-read key set: %w", err)
	}
	if err := s.finishDecodeBackfillRun(run); err != nil {
		return res, fmt.Errorf("finish run: %w", err)
	}
	log.Printf("[%s] done: kinds=%s %d scanned, %d channel row(s), %d scope row(s), %d ambiguous, %d undecodable, %d batches, max write hold %s, in %s",
		decodeBackfillComponent, res.Kinds, res.Scanned, res.ChannelUpdated, res.ScopeUpdated,
		res.Ambiguous, res.DecodeErrors, res.Batches, res.MaxHold.Round(time.Microsecond), time.Since(start).Round(time.Millisecond))
	return res, nil
}

// decodeBackfillWait is the pause between batches. Cancellation is checked
// before the pause, so a stop is honoured even when the configured pause is
// zero (a plain select over a ready ctx.Done() and a ready zero-length timer
// would pick either at random, and could start one more batch after a stop).
func decodeBackfillWait(ctx context.Context, pause time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pause):
		return nil
	}
}

// liveDecodeBackfillKeys is the live key identities of both kinds.
func liveDecodeBackfillKeys(channelKeys map[string]string, regionKeys map[string][]byte) map[string][]string {
	return map[string][]string{
		decodeBackfillKindChannel: liveKeyIdentities(decodeBackfillKindChannel, channelKeys, nil),
		decodeBackfillKindScope:   liveKeyIdentities(decodeBackfillKindScope, nil, regionKeys),
	}
}

// StartDecodeBackfill watches the effective key set and runs one pass per
// growth, in the background. The returned stop function cancels the current
// pass and waits for it; the watermark of the last committed batch is kept
// and the next start resumes from it.
//
// With set.Enabled false no watcher starts and no pass runs. If a previous
// pass left bookkeeping behind, the keys that are not effective at this start
// are forgotten once (forgetGoneKeys), and main keeps the removal hook wired,
// so turning the pass back on later re-reads every gap a removal left. No
// table is created.
func (s *Store) StartDecodeBackfill(hk *hotKeys, set DecodeBackfillSettings) func() {
	if !set.Enabled {
		if ok, err := s.decodeBackfillTablesExist(); err != nil {
			log.Printf("[%s] error: %v", decodeBackfillComponent, err)
		} else if ok {
			if err := s.forgetGoneKeys(liveDecodeBackfillKeys(hk.Channels(), hk.Regions())); err != nil {
				log.Printf("[%s] error: %v", decodeBackfillComponent, err)
			}
		}
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[%s] panic recovered: %v", decodeBackfillComponent, r)
			}
		}()
		for {
			if _, err := s.RunDecodeBackfill(ctx, hk, set); err != nil && ctx.Err() == nil {
				log.Printf("[%s] error: %v", decodeBackfillComponent, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(set.Poll):
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
