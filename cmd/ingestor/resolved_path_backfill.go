package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Issue #188 point 4: re-resolve observations stored with
// resolved_path = NULL once the prefix index and neighbour graph are primed.
// Those rows come from the resolver before #188 (no observer anchor,
// companions in the index) and from the start-up window in which buffered
// ingest is drained before the index is primed (main.go).
//
// The pass needs a prefix index and a neighbour graph loaded after a
// neighbor_edges build that succeeded and caught up with the observations
// (neighborEdgesBuild.caughtUp), with at least one edge
// (resolvedPathBackfillReady). On anything less it would resolve only
// unique prefixes and still move the watermark past every row, so it does
// not start, and a batch whose snapshot is not usable writes nothing. The
// background pass waits for the next post-build graph and then resumes from
// the persisted watermark.
//
// One pass per ingestor start covers observation ids in (watermark, ceiling],
// where ceiling is MAX(observations.id) when the pass starts and watermark is
// persisted in resolved_path_backfill_state. Each batch:
//   - reads at most batchSize rows by id on the shared connection, without
//     writerMu (a bounded primary-key range scan);
//   - resolves them in Go with resolveObservationPath, holding no lock;
//   - writes in one WriterTx: an UPDATE ... WHERE resolved_path IS NULL per
//     row that now resolves, and the new watermark.
//
// Updating only NULL rows and moving the watermark in the same transaction
// makes the pass idempotent and resumable: a crash or restart repeats at most
// the batch that did not commit. Rows that still do not resolve stay NULL and
// are not retried in a later pass (the watermark has passed them).
//
// Server visibility: the server indexes an observation's resolved_path when
// it first polls the row (live ingest, #182) or loads it at start-up. A value
// backfilled after that reaches the server's indexes only on its next restart
// (Load). This change does not touch the server.

const resolvedPathBackfillComponent = "resolved_path_backfill"

// Defaults; ResolvedPathBackfillConfig overrides them. Candidates for the
// customizer later (AGENTS.md rule 8).
const (
	defaultResolvedPathBackfillBatchSize = 500
	defaultResolvedPathBackfillPause     = 250 * time.Millisecond
)

// ResolvedPathBackfillConfig is the "resolvedPathBackfill" config block
// (documented in config.example.json). For BatchSize and PauseMs, 0 or a
// negative value means the default, so the pause cannot be turned off; the
// smallest pause is 1 ms.
type ResolvedPathBackfillConfig struct {
	Disabled  bool `json:"disabled,omitempty"`
	BatchSize int  `json:"batchSize,omitempty"` // rows per batch; default 500
	PauseMs   int  `json:"pauseMs,omitempty"`   // pause between batches; default 250
}

// ResolvedPathBackfillSettings returns the effective settings.
func (c *Config) ResolvedPathBackfillSettings() (enabled bool, batchSize int, pause time.Duration) {
	enabled, batchSize, pause = true, defaultResolvedPathBackfillBatchSize, defaultResolvedPathBackfillPause
	if b := c.ResolvedPathBackfill; b != nil {
		enabled = !b.Disabled
		if b.BatchSize > 0 {
			batchSize = b.BatchSize
		}
		if b.PauseMs > 0 {
			pause = time.Duration(b.PauseMs) * time.Millisecond
		}
	}
	return enabled, batchSize, pause
}

// resolvedPathBatch reports one batch.
type resolvedPathBatch struct {
	Scanned  int           // observation rows read
	Resolved int           // rows whose resolved_path was set
	Last     int64         // highest id read: the new watermark
	Hold     time.Duration // duration of the write transaction (writerMu + SQLite write lock)
}

// ResolvedPathBackfillResult reports one pass.
type ResolvedPathBackfillResult struct {
	From, Ceiling int64
	Batches       int
	Scanned       int
	Resolved      int
	MaxHold       time.Duration
}

func ensureResolvedPathBackfillState(db *sql.DB) error {
	// PREFLIGHT: async=true reason="CREATE TABLE IF NOT EXISTS for a one-row state table; constant cost at any scale"
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS resolved_path_backfill_state (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		watermark  INTEGER NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	return err
}

func (s *Store) resolvedPathBackfillWatermark() (int64, error) {
	var w int64
	err := s.db.QueryRow(`SELECT watermark FROM resolved_path_backfill_state WHERE id = 1`).Scan(&w)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return w, err
}

// errResolvedPathBackfillNotReady: no prefix index, or no neighbour graph
// from an edge build that caught up, or that graph has no edge.
var errResolvedPathBackfillNotReady = errors.New("prefix index or neighbour graph not ready (no neighbour-edge build has caught up yet, or no edges)")

// resolvedPathBackfillReady reports whether the pass can run on this index
// and graph.
func (s *Store) resolvedPathBackfillReady(idx prefixIndex, graph *NeighborGraph) bool {
	built, _ := s.neighborGraph.buildState()
	return built && len(idx) > 0 && !graph.empty()
}

// StartResolvedPathBackfill runs one backfill pass in the background. Until
// a neighbour graph from an edge build that caught up, with at least one edge, is
// published (StartNeighborEdgesBuilder), it waits and retries on each new
// post-build graph. The returned stop function cancels the pass and waits for
// it; the watermark of the last committed batch is kept.
func (s *Store) StartResolvedPathBackfill(batchSize int, pause time.Duration) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[%s] panic recovered: %v", resolvedPathBackfillComponent, r)
			}
		}()
		for logged := false; ; {
			// Take the wake-up channel before the readiness check, so a
			// graph published in between is not missed.
			_, next := s.neighborGraph.buildState()
			_, err := s.RunResolvedPathBackfill(ctx, batchSize, pause)
			if !errors.Is(err, errResolvedPathBackfillNotReady) {
				if err != nil && ctx.Err() == nil {
					log.Printf("[%s] error: %v", resolvedPathBackfillComponent, err)
				}
				return
			}
			if !logged {
				log.Printf("[%s] waiting: %v", resolvedPathBackfillComponent, err)
				logged = true
			}
			select {
			case <-ctx.Done():
				return
			case <-next:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// RunResolvedPathBackfill runs one pass from the persisted watermark up to the
// current MAX(observations.id).
func (s *Store) RunResolvedPathBackfill(ctx context.Context, batchSize int, pause time.Duration) (ResolvedPathBackfillResult, error) {
	var res ResolvedPathBackfillResult
	if batchSize <= 0 {
		batchSize = defaultResolvedPathBackfillBatchSize
	}
	// Without a usable index and graph most rows would stay NULL while the
	// watermark moved past them for good.
	if !s.resolvedPathBackfillReady(s.prefixIdx.load(), s.neighborGraph.load()) {
		return res, errResolvedPathBackfillNotReady
	}
	if err := ensureResolvedPathBackfillState(s.db); err != nil {
		return res, fmt.Errorf("ensure state table: %w", err)
	}
	from, err := s.resolvedPathBackfillWatermark()
	if err != nil {
		return res, fmt.Errorf("read watermark: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM observations`).Scan(&res.Ceiling); err != nil {
		return res, fmt.Errorf("read ceiling: %w", err)
	}
	res.From = from
	if from >= res.Ceiling {
		log.Printf("[%s] nothing to do (watermark %d, max id %d)", resolvedPathBackfillComponent, from, res.Ceiling)
		return res, nil
	}
	start := time.Now()
	log.Printf("[%s] starting: ids (%d, %d], batch %d, pause %s", resolvedPathBackfillComponent, from, res.Ceiling, batchSize, pause)
	for after := from; after < res.Ceiling; {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		b, err := s.resolvedPathBackfillBatch(ctx, after, res.Ceiling, batchSize)
		if err != nil {
			return res, err
		}
		if b.Scanned == 0 {
			break
		}
		after = b.Last
		res.Batches++
		res.Scanned += b.Scanned
		res.Resolved += b.Resolved
		if b.Hold > res.MaxHold {
			res.MaxHold = b.Hold
		}
		if res.Batches%100 == 0 {
			log.Printf("[%s] progress: watermark %d of %d, %d scanned, %d resolved", resolvedPathBackfillComponent, after, res.Ceiling, res.Scanned, res.Resolved)
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(pause):
		}
	}
	log.Printf("[%s] done: %d rows scanned, %d resolved, %d batches, max write hold %s, in %s",
		resolvedPathBackfillComponent, res.Scanned, res.Resolved, res.Batches, res.MaxHold.Round(time.Microsecond), time.Since(start).Round(time.Millisecond))
	return res, nil
}

// resolvedPathBackfillResolve is the resolver a batch calls. It is a
// variable so the #267 test can check that no batch resolves while it holds
// the write transaction.
var resolvedPathBackfillResolve = resolveObservationPath

type resolvedPathBackfillRow struct {
	id          int64
	pathJSON    string
	isNull      bool
	routeType   int // the observation's own (observationRouteType), -1 unknown
	payloadType int
	fromPubkey  string
	observerID  string
}

// resolvedPathBackfillBatch processes ids in (after, ceiling], at most limit
// rows, and persists the new watermark.
func (s *Store) resolvedPathBackfillBatch(ctx context.Context, after, ceiling int64, limit int) (resolvedPathBatch, error) {
	var b resolvedPathBatch
	// The pass checked readiness when it started; check the snapshot this
	// batch uses as well, and write nothing (not even the watermark) if it
	// is not usable.
	graph, idx := s.neighborGraph.load(), s.prefixIdx.load()
	if !s.resolvedPathBackfillReady(idx, graph) {
		return b, errResolvedPathBackfillNotReady
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, COALESCE(o.path_json, ''), o.resolved_path IS NULL,
			COALESCE(substr(o.raw_hex, 1, 2), ''), COALESCE(t.route_type, -1), t.route_mask,
			COALESCE(t.payload_type, -1), COALESCE(t.from_pubkey, ''), COALESCE(obs.id, '')
		FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id
		LEFT JOIN observers obs ON obs.rowid = o.observer_idx
		WHERE o.id > ? AND o.id <= ?
		ORDER BY o.id
		LIMIT ?`, after, ceiling, limit)
	if err != nil {
		return b, fmt.Errorf("select batch: %w", err)
	}
	batch := make([]resolvedPathBackfillRow, 0, limit)
	for rows.Next() {
		var r resolvedPathBackfillRow
		var header string
		var txRoute int
		var mask sql.NullInt64
		if err := rows.Scan(&r.id, &r.pathJSON, &r.isNull, &header, &txRoute, &mask, &r.payloadType, &r.fromPubkey, &r.observerID); err != nil {
			rows.Close()
			return b, fmt.Errorf("scan batch: %w", err)
		}
		r.routeType = observationRouteType(header, txRoute, mask)
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return b, fmt.Errorf("read batch: %w", err)
	}
	if len(batch) == 0 {
		return b, nil
	}
	b.Scanned = len(batch)
	b.Last = batch[len(batch)-1].id

	// Resolve outside any lock, with the same inputs InsertTransmission uses
	// (fromPubkey only for ADVERTs, as buildPacketData sets it).
	type update struct {
		id int64
		rp string
	}
	var updates []update
	for _, r := range batch {
		if !r.isNull {
			continue
		}
		hops := parsePathArray(r.pathJSON)
		if len(hops) == 0 {
			continue
		}
		from := ""
		if r.payloadType == int(payloadADVERT) {
			from = strings.ToLower(r.fromPubkey)
		}
		if rp := marshalResolvedPath(resolvedPathBackfillResolve(hops, from, r.observerID, r.routeType, graph, idx)); rp != "" {
			updates = append(updates, update{r.id, rp})
		}
	}

	holdStart := time.Now()
	err = s.WriterTx(resolvedPathBackfillComponent, func(tx *sql.Tx) error {
		if len(updates) > 0 {
			stmt, err := tx.Prepare(`UPDATE observations SET resolved_path = ? WHERE id = ? AND resolved_path IS NULL`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, u := range updates {
				res, err := stmt.Exec(u.rp, u.id)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					b.Resolved++
				}
			}
		}
		_, err := tx.Exec(`INSERT INTO resolved_path_backfill_state (id, watermark, updated_at) VALUES (1, ?, ?)
			ON CONFLICT(id) DO UPDATE SET watermark = excluded.watermark, updated_at = excluded.updated_at`,
			b.Last, time.Now().UTC().Format(time.RFC3339))
		return err
	})
	b.Hold = time.Since(holdStart)
	if err != nil {
		b.Resolved = 0
		return b, fmt.Errorf("write batch: %w", err)
	}
	return b, nil
}
