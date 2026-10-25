package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// Issue #289 M2 — repair observations.resolved_path rows written with the
// transmission's first route type.
//
// Why those rows exist. DIRECT and FLOOD receptions of the same payload share
// one transmissions row, because the content hash does not cover the header
// route bits (MeshCore src/Packet.cpp calculatePacketHash). Until #308 the
// resolved-path backfill read t.route_type — the route type of the *first*
// reception — for every observation under that row, so a reception whose own
// frame carries the other route family was resolved with the wrong rule:
//
//   - a DIRECT reception under a FLOOD transmission was anchored on the
//     observer, which stored a relay the observer never heard the packet from;
//   - a FLOOD reception under a DIRECT transmission got no observer anchor, so
//     its row stayed NULL. The #188 backfill moves its watermark past every row
//     it scans, whether or not the row resolved, so the corrected backfill
//     never revisits it. Those NULL rows are candidates here too, which is why
//     this pass scans by id from 0 rather than from the backfill watermark.
//
// #308 fixed the readers. Rows they had already written stay wrong until this
// pass rewrites them, which is what M2 of the plan on #289 asks for.
//
// What a candidate is. A row is a candidate when the #308 helper
// (observationRouteType: the observation's own frame header first, then the
// route_mask guard, then t.route_type) disagrees with t.route_type about
// flood-ness — flood-ness is the only thing resolveObservationPath reads. That
// covers both bug directions above and the rows whose frame is gone but whose
// route_mask holds both families (the #308 guard now reports those as unknown,
// where the pre-#308 backfill used t.route_type). Rows the two agree on are
// never read into a batch.
//
// What it writes. For each candidate the pass resolves the hops twice with the
// current graph and index: once with the observation's own route type (`want`)
// and once with t.route_type (`had`, what the pre-#308 reader would produce
// now). It writes only when
//
//  1. the two differ — so a row whose answer does not depend on the route type
//     is left alone even if its stored value is stale for some other reason
//     (that is not #289, and rewriting it would turn this pass into an
//     unbounded re-resolve of history); and
//  2. the stored value is exactly `had` — the signature of a row the pre-#308
//     rule wrote. A family mismatch on its own is not that signature: live
//     ingest has always resolved with the frame's own route type, so most
//     mismatched rows hold a correct ingest value, which differs from today's
//     `had` as soon as the graph or the prefix index has drifted. Those rows
//     are read and left alone, and so is a row an earlier run of this pass
//     already repaired, whose stored value is `want` (PR #590 review F1).
//
// `want` may be empty, and then the row is set back to NULL: that is the
// repair for a DIRECT reception stored with a wrong relay. Every UPDATE is
// guarded on the value the batch read, so a row live ingest rewrote in the
// meantime keeps ingest's value and a repeated batch is a no-op.
//
// The residual the two conditions cannot cover: a row whose stored value is
// both a correct ingest value and exactly what the pre-#308 rule produces
// today. Nothing in the database distinguishes the two writers, so such a row
// is rewritten. It is also the case where the rewrite is least harmful — the
// stored value is the one the buggy rule gives — but it is not free, and it is
// why the rollback section asks for a snapshot.
//
// Bounds and resume (the #89 / #380 shape):
//   - the pass needs the primed prefix index and a post-build neighbour graph
//     with at least one edge (resolvedPathBackfillReady) — resolving against a
//     cold graph would overwrite correct rows with NULL — and waits for the
//     next post-build graph when that is not yet true;
//   - each batch scans a bounded observation-id window and reads at most
//     batchSize candidates, so a window with no candidates cannot become one
//     unbounded range scan on the single SQLite connection;
//   - resolving happens in Go outside any transaction (#267); one WriterTx per
//     batch holds writerMu, so a live insert queues behind a batch and not
//     behind the run;
//   - the row rewrites and the new watermark commit in one transaction, so a
//     crash or a SIGTERM repeats at most the batch that did not commit. The
//     ceiling is pinned on the first batch and persisted: rows above it are
//     written by live ingest, which has always used the frame's own route type.
//
// It runs once per database, recorded in _async_migrations. Server visibility
// is restart-only: the server indexes an observation's resolved_path when it
// first polls or loads the row, and a later rewrite reaches its indexes on the
// next Load. cmd/server is not touched.
const (
	resolvedPathRouteRepairComponent = "resolved_path_route_repair"
	resolvedPathRouteRepairMigration = "resolved_path_route_type_repair_v1"
)

// ResolvedPathRouteRepairConfig is the opt-in switch for the repair
// (PR #590 review F3). The pass overwrites observations.resolved_path in
// place and keeps no copy of the previous value, so it must not run on the
// first boot of a build that happens to contain it: an operator turns it on
// deliberately, after taking a database snapshot. Rows it cleared to NULL are
// below the #188 backfill's watermark and are never revisited, so the
// snapshot is the only way back.
//
// AGENTS.md rule 8: a customizer entry for this switch, and for the three
// bounds below, is a follow-up and not part of this PR.
type ResolvedPathRouteRepairConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// ResolvedPathRouteRepairEnabled reports whether the one-off #289 M2 repair
// may run. Default false: an absent or empty block leaves it off.
func (c *Config) ResolvedPathRouteRepairEnabled() bool {
	return c != nil && c.ResolvedPathRouteRepair != nil && c.ResolvedPathRouteRepair.Enabled
}

// Bounds. Package vars so tests can drive the multi-batch loop with a few
// rows; customizer candidates later (AGENTS.md rule 8), like the #188
// backfill's batchSize/pauseMs.
var (
	resolvedPathRouteRepairBatchSize = 500
	resolvedPathRouteRepairIDWindow  = int64(20000)
	resolvedPathRouteRepairPause     = 250 * time.Millisecond

	// resolvedPathRouteRepairResolve is the resolver a batch calls. A
	// variable so the #267 test can check that no row is resolved while the
	// write transaction holds writerMu.
	resolvedPathRouteRepairResolve = resolveObservationPath

	// resolvedPathRouteRepairPreWriteHook is a test seam, nil in production:
	// called after a batch has resolved its rows and before it writes them.
	resolvedPathRouteRepairPreWriteHook func()

	// resolvedPathRouteRepairBatchReady is the batch's own readiness check,
	// re-run because the loop's gate can pass and the snapshot stop being
	// usable before the batch reads it. A variable so the #590 F5 test can
	// make one batch see a cold snapshot without winning that race for real.
	resolvedPathRouteRepairBatchReady = func(s *Store, idx prefixIndex, graph *NeighborGraph) bool {
		return s.resolvedPathBackfillReady(idx, graph)
	}
)

// resolvedPathRouteRepairProgressEvery logs progress every N batches.
const resolvedPathRouteRepairProgressEvery = 50

// StartResolvedPathRouteRepair schedules the repair as an async migration when
// the operator has opted in (resolvedPathRouteRepair.enabled). Call it after
// the neighbour-edges builder is running, next to the #188 backfill: the pass
// needs that builder's graph and must not delay boot. Cancelling ctx stops it
// at the next statement; it is then recorded as unfinished and resumes from
// its watermark on the next start.
//
// With enabled false nothing is scheduled and nothing is recorded in
// _async_migrations, so turning the switch on later still runs the pass.
func (s *Store) StartResolvedPathRouteRepair(ctx context.Context, enabled bool) {
	if !enabled {
		log.Printf("[%s] disabled: set resolvedPathRouteRepair.enabled to run the one-off #289 repair (take a database snapshot first: it overwrites resolved_path in place)",
			resolvedPathRouteRepairComponent)
		return
	}
	// PREFLIGHT: async=true reason="#289 M2 one-off repair: windowed scan of observations joined to transmissions with batched rewrites; must not block ingestor boot"
	if err := s.RunAsyncMigration(ctx, resolvedPathRouteRepairMigration, s.repairResolvedPathRouteTypes); err != nil {
		log.Printf("[%s] scheduling %s failed: %v", resolvedPathRouteRepairComponent, resolvedPathRouteRepairMigration, err)
	}
}

func ensureResolvedPathRouteRepairState(db *sql.DB) error {
	// PREFLIGHT: async=true reason="CREATE TABLE IF NOT EXISTS for a one-row state table; constant cost at any scale"
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS resolved_path_route_repair_state (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		watermark  INTEGER NOT NULL,
		ceiling    INTEGER NOT NULL,
		scanned    INTEGER NOT NULL DEFAULT 0,
		candidates INTEGER NOT NULL DEFAULT 0,
		repaired   INTEGER NOT NULL DEFAULT 0,
		cleared    INTEGER NOT NULL DEFAULT 0,
		started_at TEXT    NOT NULL,
		updated_at TEXT    NOT NULL
	)`)
	return err
}

// ResolvedPathRouteRepairState is the persisted progress of the pass.
type ResolvedPathRouteRepairState struct {
	Watermark  int64
	Ceiling    int64
	Scanned    int64 // candidate rows the SQL prefilter returned
	Candidates int64 // of those, the rows #308 and t.route_type really disagree on
	Repaired   int64 // rows rewritten to a resolved path
	Cleared    int64 // rows set back to NULL (a wrong relay removed)
	StartedAt  string
	UpdatedAt  string
}

// ResolvedPathRouteRepairState reads the persisted state, or nil when the
// pass has never run.
func (s *Store) ResolvedPathRouteRepairState() (*ResolvedPathRouteRepairState, error) {
	if err := ensureResolvedPathRouteRepairState(s.db); err != nil {
		return nil, err
	}
	var st ResolvedPathRouteRepairState
	err := s.db.QueryRow(`SELECT watermark, ceiling, scanned, candidates, repaired, cleared, started_at, updated_at
		FROM resolved_path_route_repair_state WHERE id = 1`).Scan(&st.Watermark, &st.Ceiling,
		&st.Scanned, &st.Candidates, &st.Repaired, &st.Cleared, &st.StartedAt, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// repairResolvedPathRouteTypes is the resolved_path_route_type_repair_v1
// migration body: it runs batches from the persisted watermark to the pinned
// ceiling.
func (s *Store) repairResolvedPathRouteTypes(ctx context.Context, _ *sql.DB) error {
	if err := ensureResolvedPathRouteRepairState(s.db); err != nil {
		return fmt.Errorf("ensure state table: %w", err)
	}
	st, err := s.resolvedPathRouteRepairStart()
	if err != nil {
		return err
	}
	if st.Watermark >= st.Ceiling {
		log.Printf("[%s] nothing to do (watermark %d, ceiling %d)", resolvedPathRouteRepairComponent, st.Watermark, st.Ceiling)
		return nil
	}
	start := time.Now()
	log.Printf("[%s] starting: ids (%d, %d], batch %d, window %d, pause %s",
		resolvedPathRouteRepairComponent, st.Watermark, st.Ceiling, resolvedPathRouteRepairBatchSize,
		resolvedPathRouteRepairIDWindow, resolvedPathRouteRepairPause)
	var maxHold time.Duration
	for batches := 0; st.Watermark < st.Ceiling; {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Resolving against a cold index or graph would rewrite correct rows
		// as NULL, so a pass that is not ready waits instead of writing.
		if err := s.waitResolvedPathRouteRepairReady(ctx); err != nil {
			return err
		}
		b, err := s.resolvedPathRouteRepairBatch(ctx, st)
		if errors.Is(err, errResolvedPathBackfillNotReady) {
			// The snapshot stopped being usable between the gate above and
			// the batch. Nothing was written: wait for the next post-build
			// graph and retry the same watermark.
			continue
		}
		if err != nil {
			return err
		}
		batches++
		if b.Hold > maxHold {
			maxHold = b.Hold
		}
		if batches%resolvedPathRouteRepairProgressEvery == 0 {
			log.Printf("[%s] progress: watermark %d of %d, %d candidates, %d repaired, %d cleared",
				resolvedPathRouteRepairComponent, st.Watermark, st.Ceiling, st.Candidates, st.Repaired, st.Cleared)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(resolvedPathRouteRepairPause):
		}
	}
	log.Printf("[%s] done: %d prefilter rows read, %d candidates, %d repaired, %d cleared, max write hold %s, in %s",
		resolvedPathRouteRepairComponent, st.Scanned, st.Candidates, st.Repaired, st.Cleared,
		maxHold.Round(time.Microsecond), time.Since(start).Round(time.Millisecond))
	return nil
}

// resolvedPathRouteRepairStart reads the persisted state, or pins a new
// ceiling at the current MAX(observations.id) and records it. Rows above the
// ceiling were written by live ingest, which has always used the frame's own
// route type.
func (s *Store) resolvedPathRouteRepairStart() (*ResolvedPathRouteRepairState, error) {
	st, err := s.ResolvedPathRouteRepairState()
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if st != nil {
		return st, nil
	}
	var ceiling int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM observations`).Scan(&ceiling); err != nil {
		return nil, fmt.Errorf("read ceiling: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	st = &ResolvedPathRouteRepairState{Ceiling: ceiling, StartedAt: now, UpdatedAt: now}
	if _, err := s.WriterExec(resolvedPathRouteRepairComponent,
		`INSERT OR IGNORE INTO resolved_path_route_repair_state (id, watermark, ceiling, started_at, updated_at)
			VALUES (1, 0, ?, ?, ?)`, ceiling, now, now); err != nil {
		return nil, fmt.Errorf("record state: %w", err)
	}
	// A concurrent start may have won the INSERT; re-read so both agree.
	st2, err := s.ResolvedPathRouteRepairState()
	if err != nil || st2 == nil {
		return st, err
	}
	return st2, nil
}

// waitResolvedPathRouteRepairReady blocks until the prefix index and a
// post-build neighbour graph with at least one edge are available, the same
// readiness gate the #188 backfill uses.
func (s *Store) waitResolvedPathRouteRepairReady(ctx context.Context) error {
	for logged := false; ; {
		// Take the wake-up channel before the check, so a graph published in
		// between is not missed.
		_, next := s.neighborGraph.buildState()
		if s.resolvedPathBackfillReady(s.prefixIdx.load(), s.neighborGraph.load()) {
			return nil
		}
		if !logged {
			log.Printf("[%s] waiting: %v", resolvedPathRouteRepairComponent, errResolvedPathBackfillNotReady)
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-next:
		}
	}
}

// resolvedPathRouteRepairBatchResult reports one batch.
type resolvedPathRouteRepairBatchResult struct {
	Scanned    int           // candidate rows read (the SQL prefilter's rows)
	Candidates int           // rows the #308 helper really disagrees on
	Repaired   int           // rows rewritten to a resolved path
	Cleared    int           // rows set back to NULL
	Hold       time.Duration // duration of the write transaction
}

type resolvedPathRouteRepairRow struct {
	id          int64
	pathJSON    string
	stored      string // the stored resolved_path, "" for NULL
	header      string
	txRoute     int
	mask        sql.NullInt64
	payloadType int
	fromPubkey  string
	observerID  string
}

// resolvedPathRouteRepairCandidateSQL is the prefilter: a superset of the
// candidates, cheap enough to run over a bounded id window. The precise test
// is observationRouteType in Go (the SQL cannot call the #308 helper), so this
// only has to avoid missing a candidate.
//
//   - first branch: the row has a frame, and the route family in its header
//     (bit 1 of the low nibble of the header byte, i.e. of raw_hex's second
//     character) differs from t.route_type's family. A character that is not
//     hex makes instr() return 0 and the expression -1, whose bit 1 is set, so
//     an unparseable header is never dropped here when t.route_type is a flood
//     type — Go then falls through to the route_mask rule, as #308 does.
//   - second branch: the row has no usable frame, its route_mask holds both
//     families (so #308 reports the route type as unknown: no observer anchor)
//     and t.route_type is a flood type, which is what the pre-#308 backfill
//     used instead.
//
// Rows with no hops are skipped: resolveObservationPath returns nothing for
// them under either route type.
var resolvedPathRouteRepairCandidateSQL = fmt.Sprintf(`(
		(length(o.raw_hex) >= 2
			AND ((instr('0123456789abcdef', lower(substr(o.raw_hex, 2, 1))) - 1) & 2) != (COALESCE(t.route_type, -1) & 2))
		OR (COALESCE(length(o.raw_hex), 0) < 2
			AND t.route_mask IS NOT NULL
			AND (t.route_mask & %d) != 0
			AND (t.route_mask & %d) != 0
			AND COALESCE(t.route_type, -1) IN (%d, %d))
	)`, packetpath.RouteMaskFlood, packetpath.RouteMaskDirect, packetpath.RouteTransportFlood, packetpath.RouteFlood)

// resolvedPathRouteRepairBatch reads one bounded window of candidates,
// re-resolves them outside any lock and rewrites the rows that the route type
// changes. It advances st in place after the batch commits.
func (s *Store) resolvedPathRouteRepairBatch(ctx context.Context, st *ResolvedPathRouteRepairState) (resolvedPathRouteRepairBatchResult, error) {
	var b resolvedPathRouteRepairBatchResult
	graph, idx := s.neighborGraph.load(), s.prefixIdx.load()
	if !resolvedPathRouteRepairBatchReady(s, idx, graph) {
		return b, errResolvedPathBackfillNotReady
	}
	windowEnd := st.Watermark + resolvedPathRouteRepairIDWindow
	if windowEnd > st.Ceiling {
		windowEnd = st.Ceiling
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, COALESCE(o.path_json, ''), COALESCE(o.resolved_path, ''),
			COALESCE(substr(o.raw_hex, 1, 2), ''), COALESCE(t.route_type, -1), t.route_mask,
			COALESCE(t.payload_type, -1), COALESCE(t.from_pubkey, ''), COALESCE(obs.id, '')
		FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id
		LEFT JOIN observers obs ON obs.rowid = o.observer_idx
		WHERE o.id > ? AND o.id <= ?
			AND COALESCE(o.path_json, '') NOT IN ('', '[]')
			AND `+resolvedPathRouteRepairCandidateSQL+`
		ORDER BY o.id
		LIMIT ?`, st.Watermark, windowEnd, resolvedPathRouteRepairBatchSize)
	if err != nil {
		return b, fmt.Errorf("select batch: %w", err)
	}
	batch := make([]resolvedPathRouteRepairRow, 0, resolvedPathRouteRepairBatchSize)
	for rows.Next() {
		var r resolvedPathRouteRepairRow
		if err := rows.Scan(&r.id, &r.pathJSON, &r.stored, &r.header, &r.txRoute, &r.mask,
			&r.payloadType, &r.fromPubkey, &r.observerID); err != nil {
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
	next := windowEnd
	if len(batch) == resolvedPathRouteRepairBatchSize {
		next = batch[len(batch)-1].id
	}

	// Resolve outside any transaction (#267), with the same inputs
	// InsertTransmission uses (fromPubkey only for ADVERTs).
	type update struct {
		id   int64
		want string // "" means set the row back to NULL
		had  string // the value the batch read, the UPDATE's guard
	}
	var updates []update
	for _, r := range batch {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		rt := observationRouteType(r.header, r.txRoute, r.mask)
		if isFloodRoute(rt) == isFloodRoute(r.txRoute) {
			continue // the prefilter's superset: #308 and t.route_type agree
		}
		hops := parsePathArray(r.pathJSON)
		if len(hops) == 0 {
			continue
		}
		b.Candidates++
		from := ""
		if r.payloadType == int(payloadADVERT) {
			from = strings.ToLower(r.fromPubkey)
		}
		want := marshalResolvedPath(resolvedPathRouteRepairResolve(hops, from, r.observerID, rt, graph, idx))
		// What the pre-#308 reader would produce for this row with today's
		// graph. Two conditions have to hold before the row is rewritten:
		//
		//   - want != had: the route type actually changes the answer. A
		//     candidate whose answer is route-type-independent is not a #289
		//     defect, and rewriting it would turn this into an unbounded
		//     re-resolve of history.
		//   - r.stored == had: the stored value is exactly what the pre-#308
		//     rule produces, which is the signature of the defect. A family
		//     mismatch alone is not: live ingest has always used the frame's
		//     own route type, so most mismatched rows were written correctly
		//     by ingest, and when the graph or the index has drifted since,
		//     today's answer differs from theirs. Requiring the stored value
		//     to match `had` leaves those rows — and a row an earlier run
		//     already repaired, whose stored value is `want` — alone
		//     (PR #590 review F1).
		had := marshalResolvedPath(resolvedPathRouteRepairResolve(hops, from, r.observerID, r.txRoute, graph, idx))
		if want == had || r.stored != had {
			continue
		}
		updates = append(updates, update{r.id, want, r.stored})
	}

	if resolvedPathRouteRepairPreWriteHook != nil {
		resolvedPathRouteRepairPreWriteHook()
	}
	scanned, candidates := st.Scanned+int64(b.Scanned), st.Candidates+int64(b.Candidates)
	holdStart := time.Now()
	err = s.WriterTx(resolvedPathRouteRepairComponent, func(tx *sql.Tx) error {
		b.Repaired, b.Cleared = 0, 0
		if len(updates) > 0 {
			// The guard keeps the value live ingest wrote after this batch
			// read the row, and makes a repeated batch a no-op.
			stmt, err := tx.Prepare(`UPDATE observations SET resolved_path = ?
				WHERE id = ? AND COALESCE(resolved_path, '') = ?`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, u := range updates {
				res, err := stmt.Exec(nilIfEmpty(u.want), u.id, u.had)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					if u.want == "" {
						b.Cleared++
					} else {
						b.Repaired++
					}
				}
			}
		}
		_, err := tx.Exec(`UPDATE resolved_path_route_repair_state
			SET watermark = ?, scanned = ?, candidates = ?, repaired = repaired + ?, cleared = cleared + ?, updated_at = ?
			WHERE id = 1`, next, scanned, candidates, b.Repaired, b.Cleared, time.Now().UTC().Format(time.RFC3339))
		return err
	})
	b.Hold = time.Since(holdStart)
	if err != nil {
		return b, fmt.Errorf("write batch: %w", err)
	}
	st.Watermark, st.Scanned, st.Candidates = next, scanned, candidates
	st.Repaired += int64(b.Repaired)
	st.Cleared += int64(b.Cleared)
	return b, nil
}
