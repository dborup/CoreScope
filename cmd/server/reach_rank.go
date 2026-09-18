package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// reach_rank.go — the neighbour-degree snapshot shared by the Reach page's
// Rank card (/api/nodes/{pubkey}/reach) and the Reach leaderboard
// (/api/reach-rank). One definition, one snapshot:
//
//   - Neighbours: distinct neighbours of a pubkey in neighbor_edges (all-time,
//     i.e. within the ingestor's edge retention), counted over every edge.
//   - Ranked population: pubkeys with at least one edge that have a node or
//     observer record (so they have a Reach page) and are not node-blacklisted,
//     observer-blacklisted, or hidden by node/observer name prefix. Hidden
//     nodes never occupy a placement, so no rank gap or total reveals them.
//   - Rank: 1 + the number of ranked pubkeys with strictly more neighbours
//     (competition ranking: 1, 1, 3). Ties are listed in pubkey order.
//   - Total: the size of the ranked population.
//
// It is NOT a measure of radio quality, range or traffic.

const (
	reachDegreeTTL          = 60 * time.Second
	reachDegreeQueryTimeout = 15 * time.Second

	reachRankDefaultLimit = 50
	reachRankMaxLimit     = 100
	reachRankMaxQueryLen  = 64 // runes

	reachRankRanked      = "ranked"
	reachRankUnranked    = "unranked"
	reachRankUnavailable = "unavailable"
)

var errReachRankNoDB = errors.New("reach rank: no database")

// onDegreeSnapshotLoad runs at the start of every snapshot DB load. A no-op in
// production; tests swap it to count loads and to hold one in flight.
var onDegreeSnapshotLoad = func() {}

// degreeSnapshot is one complete read of the neighbour graph plus the node /
// observer records of its endpoints. Immutable once published.
type degreeSnapshot struct {
	at    time.Time
	deg   map[string]int       // lowercase pubkey → distinct neighbour count, every edge endpoint
	ident map[string]rankIdent // endpoints that have a node or observer record
}

type rankIdent struct {
	hasNode  bool
	nodeName string
	obsName  string
}

// displayName mirrors buildNodeInfoMap: a node row wins over an observer row
// even when its name is empty, so the leaderboard and the Reach page header
// name a node the same way.
func (id rankIdent) displayName() string {
	if id.hasNode {
		return id.nodeName
	}
	return id.obsName
}

// reachRankRow is one leaderboard placement. The unexported nameLower is the
// precomputed search key; it is never serialised.
type reachRankRow struct {
	Rank      int    `json:"rank"`
	Pubkey    string `json:"pubkey"`
	Name      string `json:"name"`
	Neighbors int    `json:"neighbors"`
	nameLower string
}

// reachRankView is the ranked, visibility-filtered projection of one snapshot
// under one blacklist / hidden-prefix generation. Immutable once published.
type reachRankView struct {
	id     uint64 // unique per build, lets cached Reach bodies detect a stale rank
	snap   *degreeSnapshot
	blGen  uint64
	hidGen uint64
	rows   []reachRankRow // neighbours desc, pubkey asc
	pos    map[string]int // pubkey → index into rows
}

func (v *reachRankView) viewID() uint64 {
	if v == nil {
		return 0
	}
	return v.id
}

// lookup returns a pubkey's neighbour count, placement and the ranked total.
// degree comes from the full snapshot, so an unranked-but-visible node still
// shows its neighbour count.
func (v *reachRankView) lookup(pubkey string) (degree, rank, total int, ranked bool) {
	degree = v.snap.deg[pubkey]
	if i, ok := v.pos[pubkey]; ok {
		return degree, v.rows[i].Rank, len(v.rows), true
	}
	return degree, 0, len(v.rows), false
}

// page returns the rows matching q (case-insensitive substring of name or
// pubkey; "" matches all) in [offset, offset+limit), plus the match count.
// Placements are the global ones — a search never renumbers.
func (v *reachRankView) page(q string, offset, limit int) ([]reachRankRow, int) {
	if q == "" {
		n := len(v.rows)
		if offset >= n {
			return []reachRankRow{}, n
		}
		return v.rows[offset:min(offset+limit, n)], n
	}
	q = strings.ToLower(q)
	out := make([]reachRankRow, 0, min(limit, 16))
	matched := 0
	for i := range v.rows {
		r := &v.rows[i]
		if !strings.Contains(r.Pubkey, q) && !strings.Contains(r.nameLower, q) {
			continue
		}
		if matched >= offset && len(out) < limit {
			out = append(out, *r)
		}
		matched++
	}
	return out, matched
}

// reachRankVisible reports whether a pubkey may occupy a placement. Mirrors the
// per-pubkey 404 rules (IsBlacklisted, isPubkeyHidden on the node name) and
// also drops observer-blacklisted pubkeys and hidden observer names.
func reachRankVisible(cfg *Config, pubkey string, id rankIdent) bool {
	if cfg == nil {
		return true
	}
	if cfg.IsBlacklisted(pubkey) || cfg.IsObserverBlacklisted(pubkey) {
		return false
	}
	return !cfg.IsNameHidden(id.nodeName) && !cfg.IsNameHidden(id.obsName)
}

func buildReachRankView(snap *degreeSnapshot, cfg *Config, id, blGen, hidGen uint64) *reachRankView {
	rows := make([]reachRankRow, 0, len(snap.ident))
	for pk, ident := range snap.ident {
		n := snap.deg[pk]
		if n <= 0 || !reachRankVisible(cfg, pk, ident) {
			continue
		}
		name := ident.displayName()
		rows = append(rows, reachRankRow{Pubkey: pk, Name: name, Neighbors: n, nameLower: strings.ToLower(name)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Neighbors != rows[j].Neighbors {
			return rows[i].Neighbors > rows[j].Neighbors
		}
		return rows[i].Pubkey < rows[j].Pubkey
	})
	pos := make(map[string]int, len(rows))
	for i := range rows {
		if i > 0 && rows[i].Neighbors == rows[i-1].Neighbors {
			rows[i].Rank = rows[i-1].Rank
		} else {
			rows[i].Rank = i + 1
		}
		pos[rows[i].Pubkey] = i
	}
	return &reachRankView{id: id, snap: snap, blGen: blGen, hidGen: hidGen, rows: rows, pos: pos}
}

// reachRankView returns the current ranked view, rebuilding it when the
// snapshot or a visibility generation moved. The generations are read before
// filtering, so a concurrent blacklist / prefix change at worst tags the view
// with the older generation and forces one more rebuild on the next request.
// The rebuild is CPU-only (no DB) and runs outside the lock.
func (s *Server) reachRankView(ctx context.Context) (*reachRankView, error) {
	snap, err := s.getDegreeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var blGen, hidGen uint64
	if s.cfg != nil {
		blGen, hidGen = s.cfg.BlacklistGeneration(), s.cfg.HiddenNamePrefixesGeneration()
	}
	s.reach.degreeMu.Lock()
	cur := s.reach.rankView
	if cur != nil && cur.snap == snap && cur.blGen == blGen && cur.hidGen == hidGen {
		s.reach.degreeMu.Unlock()
		return cur, nil
	}
	s.reach.rankViewSeq++
	id := s.reach.rankViewSeq
	s.reach.degreeMu.Unlock()

	v := buildReachRankView(snap, s.cfg, id, blGen, hidGen)

	s.reach.degreeMu.Lock()
	// Publish unless a newer snapshot landed meanwhile; an equivalent view
	// built concurrently is harmless (last writer wins).
	if s.reach.degreeSnap == snap {
		s.reach.rankView = v
	}
	s.reach.degreeMu.Unlock()
	return v, nil
}

// getDegreeSnapshot serves the snapshot while fresh; otherwise one caller
// rebuilds it (singleflight) and concurrent callers share that result. The
// rebuild runs on a context detached from the triggering request (bounded by
// reachDegreeQueryTimeout) so one client disconnecting cannot fail every
// waiter. On a failed rebuild the previous complete snapshot is served —
// its timestamp tells the reader its age; with no previous snapshot the error
// is returned. A failed or partial read is never published.
func (s *Server) getDegreeSnapshot(ctx context.Context) (*degreeSnapshot, error) {
	s.reach.degreeMu.Lock()
	cur := s.reach.degreeSnap
	s.reach.degreeMu.Unlock()
	if cur != nil && time.Since(cur.at) < reachDegreeTTL {
		return cur, nil
	}
	ch := s.reach.degreeSF.DoChan("degree", func() (interface{}, error) {
		s.reach.degreeMu.Lock()
		c := s.reach.degreeSnap
		s.reach.degreeMu.Unlock()
		if c != nil && time.Since(c.at) < reachDegreeTTL {
			return c, nil // refreshed while this caller queued
		}
		onDegreeSnapshotLoad()
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reachDegreeQueryTimeout)
		defer cancel()
		snap, err := s.loadDegreeSnapshot(qctx)
		if err != nil {
			return nil, err
		}
		s.reach.degreeMu.Lock()
		s.reach.degreeSnap = snap
		s.reach.degreeMu.Unlock()
		return snap, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			if cur != nil {
				log.Printf("[reach] degree snapshot rebuild failed: %v (serving snapshot from %s)", res.Err, cur.at.UTC().Format(time.RFC3339))
				return cur, nil
			}
			log.Printf("[reach] degree snapshot rebuild failed: %v (no snapshot to serve)", res.Err)
			return nil, res.Err
		}
		return res.Val.(*degreeSnapshot), nil
	case <-ctx.Done():
		if cur != nil {
			return cur, nil
		}
		return nil, ctx.Err()
	}
}

// loadDegreeSnapshot reads the neighbour degrees and the node / observer
// records of every edge endpoint: three bulk queries, no per-row lookups. Any
// query, scan or iteration error fails the whole load. The node list is read
// here rather than via getCachedNodesAndPM because that cache swallows DB
// errors — an empty node list would silently drop every node from the ranking
// (or skip their hidden-name check).
func (s *Server) loadDegreeSnapshot(ctx context.Context) (*degreeSnapshot, error) {
	if s.db == nil || s.db.conn == nil {
		return nil, errReachRankNoDB
	}
	at := time.Now()
	deg := make(map[string]int)
	err := s.scanRankRows(ctx, "degree", `
		SELECT pk, COUNT(*) FROM (
			SELECT node_a pk FROM neighbor_edges
			UNION ALL SELECT node_b FROM neighbor_edges
		) GROUP BY pk`, func(scan func(...interface{}) error) error {
		var pk string
		var n int
		if err := scan(&pk, &n); err != nil {
			return err
		}
		// The ingestor writes lowercase pubkeys; fold case defensively.
		deg[strings.ToLower(pk)] += n
		return nil
	})
	if err != nil {
		return nil, err
	}
	ident := make(map[string]rankIdent)
	err = s.scanRankRows(ctx, "nodes", `SELECT COALESCE(public_key,''), COALESCE(name,'') FROM nodes`,
		func(scan func(...interface{}) error) error {
			var pk, name string
			if err := scan(&pk, &name); err != nil {
				return err
			}
			pk = strings.ToLower(pk)
			if deg[pk] > 0 {
				id := ident[pk]
				id.hasNode, id.nodeName = true, name
				ident[pk] = id
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = s.scanRankRows(ctx, "observers", `SELECT COALESCE(id,''), COALESCE(name,'') FROM observers`,
		func(scan func(...interface{}) error) error {
			var pk, name string
			if err := scan(&pk, &name); err != nil {
				return err
			}
			pk = strings.ToLower(pk) // observer ids are stored upper-case
			if deg[pk] > 0 {
				id, seen := ident[pk]
				if !seen || id.obsName == "" {
					id.obsName = name
				}
				ident[pk] = id
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return &degreeSnapshot{at: at, deg: deg, ident: ident}, nil
}

// scanRankRows runs one snapshot query and feeds each row to fn, turning any
// query / scan / iteration error into a load failure.
func (s *Server) scanRankRows(ctx context.Context, what, q string, fn func(scan func(...interface{}) error) error) error {
	rows, err := s.db.conn.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("reach rank %s query: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return fmt.Errorf("reach rank %s scan: %w", what, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reach rank %s rows: %w", what, err)
	}
	return nil
}

// applyReachRank fills the snapshot-derived importance fields of a Reach
// response from v (nil = no snapshot could be read).
func applyReachRank(imp *NodeReachImportance, pubkey string, v *reachRankView) {
	if v == nil {
		imp.NeighborDegree, imp.DegreeRank, imp.NodesWithEdges = 0, 0, 0
		imp.RankStatus, imp.RankSnapshotAt = reachRankUnavailable, ""
		return
	}
	degree, rank, total, ranked := v.lookup(pubkey)
	imp.NeighborDegree, imp.DegreeRank, imp.NodesWithEdges = degree, rank, total
	imp.RankStatus = reachRankUnranked
	if ranked {
		imp.RankStatus = reachRankRanked
	}
	imp.RankSnapshotAt = v.snap.at.UTC().Format(time.RFC3339)
}

// ReachRankResponse is the /api/reach-rank body.
type ReachRankResponse struct {
	SnapshotAt string         `json:"snapshot_at"` // when the neighbour graph was read (RFC3339, UTC)
	Total      int            `json:"total"`       // ranked nodes in the whole leaderboard
	Matched    int            `json:"matched"`     // rows matching q (== total without q)
	Offset     int            `json:"offset"`
	Limit      int            `json:"limit"`
	Query      string         `json:"q"`
	Rows       []reachRankRow `json:"rows"`
}

// handleReachRank serves one page of the Reach leaderboard from the cached
// ranked view: O(1) for an unfiltered page, one linear pass for a search.
func (s *Server) handleReachRank(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := strings.TrimSpace(qv.Get("q"))
	if !utf8.ValidString(q) || utf8.RuneCountInString(q) > reachRankMaxQueryLen {
		writeError(w, 400, "invalid q: expected at most "+strconv.Itoa(reachRankMaxQueryLen)+" characters")
		return
	}
	limit := clampLimit(qv.Get("limit"), reachRankDefaultLimit, reachRankMaxLimit)
	offset := 0
	if raw := qv.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, 400, "invalid offset: expected a non-negative integer")
			return
		}
		offset = n
	}
	view, err := s.reachRankView(r.Context())
	if err != nil {
		// 500, not 503: the SPA's api() treats 503 as server warm-up and
		// retries for ~a minute. A failed read is never an empty leaderboard.
		writeError(w, 500, "reach rank unavailable")
		return
	}
	rows, matched := view.page(q, offset, limit)
	writeJSON(w, ReachRankResponse{
		SnapshotAt: view.snap.at.UTC().Format(time.RFC3339),
		Total:      len(view.rows),
		Matched:    matched,
		Offset:     offset,
		Limit:      limit,
		Query:      q,
		Rows:       rows,
	})
}
