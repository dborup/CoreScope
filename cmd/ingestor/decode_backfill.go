package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
// so the watcher compares those snapshots against the key names a completed
// run has already covered (decode_backfill_keys). A revoked or removed key
// arms nothing: a resolved row is never revisited.
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

// decodeBackfillKeyHash is the identity a covered key is recorded under. A
// hashtag channel key *is* derived from its name (sha256 of "#name"), so this
// table must not become a new place that holds key material: only the digest
// of the name is stored, and log lines report kinds and counts, never names.
func decodeBackfillKeyHash(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

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
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS decode_backfill_keys (
		kind      TEXT NOT NULL,
		name_hash TEXT NOT NULL,
		state     TEXT NOT NULL,
		PRIMARY KEY (kind, name_hash)
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

// coveredKeyHashes returns the name digests already recorded for kind, with
// the value reporting whether the digest is "covered" (a run that used it
// reached its ceiling) rather than merely "running".
func (s *Store) coveredKeyHashes(kind string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name_hash, state FROM decode_backfill_keys WHERE kind = ?`, kind)
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
// name that no completed run has covered. Growth only: losing a name never
// arms a run, and no resolved row is ever revisited.
func (s *Store) decodeBackfillArmedKinds(live map[string][]string) ([]string, error) {
	var armed []string
	for _, kind := range []string{decodeBackfillKindChannel, decodeBackfillKindScope} {
		known, err := s.coveredKeyHashes(kind)
		if err != nil {
			return nil, err
		}
		for _, name := range live[kind] {
			if !known[decodeBackfillKeyHash(name)] {
				armed = append(armed, kind)
				break
			}
		}
	}
	return armed, nil
}

// forgetGoneKeys drops the record of a key that is no longer effective, so
// re-adding it later arms a fresh run. Without this, a key that was covered,
// then revoked (or removed from the config), then approved again would leave
// the packets received during the gap unresolved forever: they were ingested
// after the run that credited the key, so no later run would be armed for it.
// Nothing is rewritten here — only the bookkeeping that decides whether a run
// is needed. A reload that cannot read its config keeps every previous key
// (hotKeys.reload bails before storing), so a bad config cannot make this
// forget anything.
//
// It also applies to a run that is resumed after a key it started with was
// revoked: the resumed run reads its snapshot from these rows, so it stops
// using that key for the rest of the table. That is the revocation taking
// effect, not a loss — a later re-approval forgets nothing and arms a fresh
// pass over the whole table.
func (s *Store) forgetGoneKeys(live map[string][]string) error {
	for _, kind := range []string{decodeBackfillKindChannel, decodeBackfillKindScope} {
		known, err := s.coveredKeyHashes(kind)
		if err != nil {
			return err
		}
		if len(known) == 0 {
			continue
		}
		liveHashes := make(map[string]bool, len(live[kind]))
		for _, name := range live[kind] {
			liveHashes[decodeBackfillKeyHash(name)] = true
		}
		var gone []string
		for h := range known {
			if !liveHashes[h] {
				gone = append(gone, h)
			}
		}
		if len(gone) == 0 {
			continue
		}
		sort.Strings(gone)
		if err := s.WriterTx(decodeBackfillComponent, func(tx *sql.Tx) error {
			for _, h := range gone {
				if _, err := tx.Exec(`DELETE FROM decode_backfill_keys WHERE kind = ? AND name_hash = ?`, kind, h); err != nil {
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

// decodeBackfillCandidateSQL is the unresolved-row predicate for the armed
// kinds. Channel candidates are GRP_TXT rows still carrying the `enc_<HEX>`
// routing placeholder (BuildPacketData writes channel_hash only for
// payload_type 5, so GRP_DATA has no placeholder to upgrade and is out of
// scope). Scope candidates are transport-scoped rows whose region is unknown:
// scopeNameForDB writes NULL for "not transport-scoped" and ” for
// "transport-scoped, region unknown".
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
		var ceiling int64
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM transmissions`).Scan(&ceiling); err != nil {
			return nil, fmt.Errorf("read ceiling: %w", err)
		}
		run.ceiling = ceiling
		now := time.Now().UTC().Format(time.RFC3339)
		kinds := run.kinds()
		if err := s.WriterTx(decodeBackfillComponent, func(tx *sql.Tx) error {
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
				for _, name := range liveKeyNames(kind, channelKeys, regionKeys) {
					if _, err := tx.Exec(`INSERT OR IGNORE INTO decode_backfill_keys (kind, name_hash, state) VALUES (?, ?, 'running')`,
						kind, decodeBackfillKeyHash(name)); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("record run: %w", err)
		}
	}

	// Decode with the run's own snapshot: the live names whose digest this
	// run recorded. On a resume that is the set the run started with.
	if run.channel {
		known, err := s.coveredKeyHashes(decodeBackfillKindChannel)
		if err != nil {
			return nil, err
		}
		run.channelKeys = make(map[string]string, len(channelKeys))
		for name, key := range channelKeys {
			if _, ok := known[decodeBackfillKeyHash(name)]; ok {
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
			if _, ok := known[decodeBackfillKeyHash(name)]; ok {
				run.regionKeys[name] = key
			}
		}
	}
	return run, nil
}

// liveKeyNames returns the sorted live key names of one kind, so the recorded
// snapshot does not depend on Go's map iteration order.
func liveKeyNames(kind string, channelKeys map[string]string, regionKeys map[string][]byte) []string {
	var out []string
	switch kind {
	case decodeBackfillKindChannel:
		for name := range channelKeys {
			out = append(out, name)
		}
	case decodeBackfillKindScope:
		for name := range regionKeys {
			out = append(out, name)
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

// decodeBackfillBatch re-decodes one bounded id window and commits the
// rewrites, the new watermark and the counters in a single transaction.
func (s *Store) decodeBackfillBatch(ctx context.Context, run *decodeBackfillRun, set DecodeBackfillSettings) (decodeBackfillBatchResult, error) {
	var b decodeBackfillBatchResult
	b.Next = run.watermark
	windowEnd := run.watermark + set.IDWindow
	if windowEnd > run.ceiling {
		windowEnd = run.ceiling
	}
	pred := decodeBackfillCandidateSQL(run.channel, run.scope)
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
	// rewritten row is byte-identical to the same packet ingested fresh.
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
		if run.channel && r.payloadType == PayloadGRP_TXT && strings.HasPrefix(r.channelHash.String, "enc_") {
			// Same rule as BuildPacketData: a decrypted GRP_TXT is keyed by
			// its channel name, an undecryptable one keeps enc_<HEX>.
			if decoded.Payload.Type == "CHAN" && decoded.Payload.Channel != "" {
				chanUpdates = append(chanUpdates, chanUpdate{r.id, decoded.Payload.Channel, PayloadJSON(&decoded.Payload)})
			}
		}
		if run.scope && r.scopeName.Valid && r.scopeName.String == "" {
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

// RunDecodeBackfill runs one pass for the armed kinds (or resumes an
// unfinished one) and returns what it did. A nil-run (nothing armed and
// nothing to resume) returns a zero result and no error.
func (s *Store) RunDecodeBackfill(ctx context.Context, hk *hotKeys, set DecodeBackfillSettings) (DecodeBackfillResult, error) {
	var res DecodeBackfillResult
	if err := ensureDecodeBackfillTables(s.db); err != nil {
		return res, err
	}
	channelKeys, regionKeys := hk.Channels(), hk.Regions()
	live := map[string][]string{
		decodeBackfillKindChannel: liveKeyNames(decodeBackfillKindChannel, channelKeys, nil),
		decodeBackfillKindScope:   liveKeyNames(decodeBackfillKindScope, nil, regionKeys),
	}
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
	res.Kinds, res.From, res.Ceiling = run.kinds(), run.watermark, run.ceiling
	start := time.Now()
	log.Printf("[%s] starting: kinds=%s ids (%d, %d], %d channel key(s), %d region key(s), batch %d, window %d, pause %s",
		decodeBackfillComponent, res.Kinds, run.watermark, run.ceiling,
		len(run.channelKeys), len(run.regionKeys), set.BatchSize, set.IDWindow, set.Pause)
	for {
		b, err := s.decodeBackfillBatch(ctx, run, set)
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

// StartDecodeBackfill watches the effective key set and runs one pass per
// growth, in the background. The returned stop function cancels the current
// pass and waits for it; the watermark of the last committed batch is kept
// and the next start resumes from it.
func (s *Store) StartDecodeBackfill(hk *hotKeys, set DecodeBackfillSettings) func() {
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
