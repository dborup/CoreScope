package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// Global region → members index for /api/nodes?region= (#488 M2, #618).
//
// A region set is served as the union of per-code member lists cut from ONE
// index per region rule, kept by one shared scan of the ADVERT observation
// history grouped by observer IATA. Region sets used to be built one by one
// in the heard rule, each a full history scan of its own: a minute per set
// on a large database, paid again for every new combination or quick pick.
//
// An index keeps the machinery #488 M2 introduced for placement: a
// regionEvidenceAgg with an observations.id watermark, delta scans past it,
// a full rebuild every nodeRegionRebuildInterval (retention pruning,
// observer region edits and late from_pubkey backfills change rows below
// the watermark), and an immutable snapshot readers load without a lock.
// The two rules differ only in their scan and in how a node's per-region
// evidence becomes member regions (regionIndexSpec).
//
// Refreshes of an index are deduplicated by its singleflight and serialised
// by its mutex. A caller that waits (refreshNodeRegion with waitIndex) shares
// the flight in progress; a caller a stale but usable snapshot can serve
// (younger than nodeRegionMaxStale) is served from it at once and only kicks
// a refresh, so no request queues behind a full rebuild once a snapshot
// exists. StartRegionIndexWarmer builds the index right after startup and
// keeps refreshing it, so the full scans run there and not under a request.
// A request that arrives before the first snapshot waits for the build
// already in flight, so it waits at most the rest of the warmup; it runs a
// per-set build of its own only if that build fails (coldNodeRegion).
//
// Everything is read from SQLite on the server's read-only handle; nothing
// touches the in-memory packet store or its lock.

// regionIndexKeepInterval is how often the warmer refreshes the index: a
// delta scan past the watermark (milliseconds), or the periodic full
// rebuild once it is due.
const regionIndexKeepInterval = nodeRegionFreshTTL

// errRegionIndexFlightResult means a regionIndex flight returned something
// other than a *regionMembershipSnapshot, which only a programming error
// can cause.
var errRegionIndexFlightResult = errors.New("unexpected region index flight result")

// regionIndex is one global index. agg and built are owned by the refresh
// holding mu; readers only ever see snap.
type regionIndex struct {
	sf    singleflight.Group
	mu    sync.Mutex
	agg   *regionEvidenceAgg
	built time.Time // start of agg's last full scan
	snap  atomic.Pointer[regionMembershipSnapshot]
	// scanHook (test seam, nil in production) runs at the end of every
	// scan, full or delta, after its rows were folded in; a non-nil error
	// fails that scan the way an error part-way through the rows does.
	scanHook func(full bool) error
}

// regionIndexSpec is what one region rule scans and how it derives a node's
// member regions from its per-region evidence.
type regionIndexSpec struct {
	label   string
	scan    func(ctx context.Context, lo, hi int64, delta bool, agg *regionEvidenceAgg) (int, error)
	regions func(perRegion map[string]*regionEvidenceStats) []string
	// perSet builds one region set on its own, the fallback for a request
	// that arrives before the index's first snapshot when the index build
	// fails (coldNodeRegion); nil when the rule has no per-set form
	// (placement needs every region's evidence).
	perSet func(ctx context.Context, codes []string) (*nodeRegionEntry, error)
}

// heardRegions is the heard rule: a node is a member of every region that
// heard any of its adverts.
func heardRegions(perRegion map[string]*regionEvidenceStats) []string {
	out := make([]string, 0, len(perRegion))
	for r := range perRegion {
		out = append(out, r)
	}
	return out
}

func (db *DB) heardRegionSpec() regionIndexSpec {
	return regionIndexSpec{label: "heard", scan: db.scanHeardRegions, regions: heardRegions, perSet: db.buildNodeRegionCold}
}

func (db *DB) placedRegionSpec() regionIndexSpec {
	p := db.regionPlacement.Params
	return regionIndexSpec{
		label: "placement",
		scan:  db.scanRegionEvidence,
		regions: func(perRegion map[string]*regionEvidenceStats) []string {
			return placeNode(perRegion, p).Regions
		},
	}
}

// activeRegionIndex is the index the configured mode serves from.
func (db *DB) activeRegionIndex() (*regionIndex, regionIndexSpec) {
	if db.regionPlacement.Mode == regionModePlaced {
		return &db.regionPlaced, db.placedRegionSpec()
	}
	return &db.regionHeard, db.heardRegionSpec()
}

// regionMembershipSnapshot is the immutable result of one index refresh:
// the region → members lists the per-set cache entries are cut from.
type regionMembershipSnapshot struct {
	byRegion  map[string][]string // sorted pubkeys that are members of each region
	watermark int64
	refreshed time.Time
	built     time.Time // the index's last full build
}

// newRegionMembershipSnapshot derives every node's member regions in agg.
// It shares the pubkey strings with agg, so the lists cost one slice slot
// per membership (at most agg.pairs).
func newRegionMembershipSnapshot(agg *regionEvidenceAgg, spec regionIndexSpec, built time.Time) *regionMembershipSnapshot {
	byRegion := make(map[string][]string)
	for pk, per := range agg.nodes {
		for _, r := range spec.regions(per) {
			byRegion[r] = append(byRegion[r], pk)
		}
	}
	for _, keys := range byRegion {
		sort.Strings(keys)
	}
	return &regionMembershipSnapshot{byRegion: byRegion, watermark: agg.watermark, refreshed: time.Now(), built: built}
}

// members is the set of nodes that are members of any of codes.
func (s *regionMembershipSnapshot) members(codes []string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, c := range codes {
		for _, pk := range s.byRegion[c] {
			set[pk] = struct{}{}
		}
	}
	return set
}

// entry cuts the per-set cache entry for codes. It carries the snapshot's
// own ages, so an entry cut from a stale snapshot is stale too and the
// fresh/stale ages nodeRegionKeysJSON promises are those of the data.
func (s *regionMembershipSnapshot) entry(codes []string) (*nodeRegionEntry, error) {
	return newNodeRegionEntry(s.members(codes), s.watermark, s.refreshed, s.built)
}

// currentRegionIndex returns a snapshot of ix to serve from. A fresh one is
// returned as is. With wait set, a stale one is refreshed first (sharing the
// refresh in flight, if any). Without it, a snapshot younger than
// nodeRegionMaxStale is returned at once and one refresh is kicked in the
// background; only a missing or over-stale snapshot is waited for.
func (db *DB) currentRegionIndex(ctx context.Context, ix *regionIndex, spec regionIndexSpec, wait bool) (*regionMembershipSnapshot, error) {
	s := ix.snap.Load()
	if s != nil {
		age := time.Since(s.refreshed)
		if age < nodeRegionFreshTTL {
			return s, nil
		}
		if !wait && age < nodeRegionMaxStale {
			// DoChan dedups against a refresh in flight, and its buffered
			// result channel may be left unread.
			ix.sf.DoChan("", func() (any, error) {
				ctx, cancel := context.WithTimeout(context.Background(), nodeRegionRebuildInterval)
				defer cancel()
				return db.refreshRegionIndexLogged(ctx, ix, spec)
			})
			return s, nil
		}
	}
	return db.regionIndexFlight(ctx, ix, spec)
}

// coldNodeRegion serves a region set that a caller waits for before ix has
// published its first snapshot (the warmup still running, or failing). It
// waits for the index build — the warmer's, or one it starts — so a cold
// request waits at most the rest of the warmup and adds no scan that would
// slow the one build every region set is served from. Only when that build
// fails does it fall back to the set's own per-set build, the scan every
// cold request ran before #618, so a failing index does not turn
// /api/nodes?region= into a 500. Fallbacks are serialised by
// nodeRegionColdMu, so a burst of cold sets holds one pooled connection for
// a full scan, and one that queued while a retry (the warmer's next tick, or
// another request) published the index is cut from it instead.
func (db *DB) coldNodeRegion(ctx context.Context, ix *regionIndex, spec regionIndexSpec, codes []string) (*nodeRegionEntry, error) {
	s, err := db.regionIndexFlight(ctx, ix, spec)
	if err == nil {
		return s.entry(codes)
	}
	db.nodeRegionColdMu.Lock()
	defer db.nodeRegionColdMu.Unlock()
	if s := ix.snap.Load(); s != nil {
		return s.entry(codes)
	}
	e, setErr := spec.perSet(ctx, codes)
	if setErr != nil {
		return nil, errors.Join(err, setErr)
	}
	return e, nil
}

// regionIndexFlight runs one refresh of ix, or waits for the one in flight.
// The flight runs under the context of whoever started it, so a caller
// whose own ctx ends stops waiting rather than sit out a scan it cannot
// cancel (the warmer's stop at shutdown).
func (db *DB) regionIndexFlight(ctx context.Context, ix *regionIndex, spec regionIndexSpec) (*regionMembershipSnapshot, error) {
	ch := ix.sf.DoChan("", func() (any, error) {
		return db.refreshRegionIndexLogged(ctx, ix, spec)
	})
	var res singleflight.Result
	select {
	case res = <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if res.Err != nil {
		return nil, res.Err
	}
	v := res.Val
	s, ok := v.(*regionMembershipSnapshot)
	if !ok {
		return nil, fmt.Errorf("nodes-region %s: %w: %T", spec.label, errRegionIndexFlightResult, v)
	}
	return s, nil
}

// refreshRegionIndexLogged is refreshRegionIndex for a flight: a background
// refresh has no caller to report to, so a failure is logged here.
func (db *DB) refreshRegionIndexLogged(ctx context.Context, ix *regionIndex, spec regionIndexSpec) (*regionMembershipSnapshot, error) {
	s, err := db.refreshRegionIndex(ctx, ix, spec)
	if err != nil {
		err = fmt.Errorf("nodes-region %s: %w", spec.label, err)
		log.Printf("[nodes-region] index refresh failed: %v", err)
		return nil, err
	}
	return s, nil
}

// refreshRegionIndex brings ix up to date — a full build when there is none
// or one is due, else a delta past the watermark scanned into a separate
// aggregate and merged only on success — and publishes a new snapshot. A
// failed refresh leaves the aggregate and the published snapshot as they
// were.
func (db *DB) refreshRegionIndex(ctx context.Context, ix *regionIndex, spec regionIndexSpec) (*regionMembershipSnapshot, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.agg == nil || time.Since(ix.built) >= nodeRegionRebuildInterval {
		// Full scans of either index share nodeRegionFullMu, so at most one
		// holds a pooled connection for the whole history.
		db.nodeRegionFullMu.Lock()
		defer db.nodeRegionFullMu.Unlock()
		wm, err := db.maxObservationID(ctx)
		if err != nil {
			return nil, err
		}
		start := time.Now()
		agg := newRegionEvidenceAgg()
		if err := ix.scan(ctx, spec, 0, wm, false, agg); err != nil {
			return nil, err
		}
		ix.agg, ix.built = agg, start
		s := newRegionMembershipSnapshot(agg, spec, start)
		ix.snap.Store(s)
		log.Printf("[nodes-region] %s: full build, %d nodes, %d (node, region) pairs, %d regions in %v (obs id <= %d)",
			spec.label, len(agg.nodes), agg.pairs, len(s.byRegion), time.Since(start).Round(time.Millisecond), wm)
		return s, nil
	}
	wm, err := db.maxObservationID(ctx)
	if err != nil {
		return nil, err
	}
	agg := ix.agg
	if prev := ix.snap.Load(); prev != nil && wm <= agg.watermark {
		s := *prev
		s.refreshed = time.Now()
		ix.snap.Store(&s)
		return &s, nil
	}
	if wm > agg.watermark {
		delta := newRegionEvidenceAgg()
		if err := ix.scan(ctx, spec, agg.watermark, wm, true, delta); err != nil {
			return nil, err
		}
		agg.merge(delta)
	}
	s := newRegionMembershipSnapshot(agg, spec, ix.built)
	ix.snap.Store(s)
	return s, nil
}

// scan runs spec's scan and then the test seam.
func (ix *regionIndex) scan(ctx context.Context, spec regionIndexSpec, lo, hi int64, delta bool, agg *regionEvidenceAgg) error {
	if _, err := spec.scan(ctx, lo, hi, delta, agg); err != nil {
		return err
	}
	if ix.scanHook != nil {
		return ix.scanHook(!delta)
	}
	return nil
}

// scanHeardRegions folds every ADVERT observation with lo < id <= hi, heard
// by an observer with a region, into agg as a (node, region) pair, and
// raises agg's watermark to hi. It is the heard rule's per-set filter
// (UPPER(TRIM(iata)) IN (codes), scanNodeRegionKeys) run once for every
// code: normalised codes are never empty, so the blank regions it skips
// match no set.
//
// The join order is forced (CROSS JOIN) both ways. A delta scan drives from
// the observations rowid range; left to the planner, sqlite_stat1 (#2058)
// makes it start from the observers and walk their whole history. A full
// scan drives from the ADVERT transmissions (idx_transmissions_payload_type,
// then idx_observations_transmission_id, both created by the ingestor's
// schema): with no region filter the planner walks every observation by
// rowid and looks up its transmission, most of them not ADVERTs. Rows are
// not DISTINCT-ed in SQL; agg.pair dedupes them, which measured cheaper
// than SQLite's temp B-tree (#618 PR).
func (db *DB) scanHeardRegions(ctx context.Context, lo, hi int64, delta bool, agg *regionEvidenceAgg) (int, error) {
	joinCond := "obs.rowid = o.observer_idx"
	if !db.isV3() {
		joinCond = "obs.id = o.observer_id"
	}
	from := "observations o CROSS JOIN transmissions t ON t.id = o.transmission_id"
	if !delta {
		from = "transmissions t CROSS JOIN observations o ON o.transmission_id = t.id"
	}
	// Only fixed fragments are formatted in; every value is bound.
	//
	// #1143 / PR #38: from_pubkey is a dedicated column the ingestor fills at
	// write time for ADVERT rows, so this lookup does not have to
	// JSON_EXTRACT and parse decoded_json for every candidate row. Measured
	// against a live 4.9GB database, 9 interleaved rounds so concurrent
	// ingest cannot bias one variant: 934ms -> 835ms median (1.12x) for a
	// single-region filter, identical result set both ways. The win is purely
	// the avoided JSON parse.
	//
	// THE TRAP, for whoever touches this next: from_pubkey is only written
	// for ADVERTs that actually carry a pubkey — see the guard in
	// cmd/ingestor/db.go. A NULL here drops out silently, so a write path
	// that fills decoded_json but forgets from_pubkey would cost nodes from
	// the region filter with no error to notice. That is safe today only
	// because it is measured to be: on live staging 25 of 62104 ADVERTs have
	// NULL from_pubkey, and all 25 have no pubKey in decoded_json either —
	// nothing is lost, because there was never a pubkey to record. A COALESCE
	// fallback to JSON_EXTRACT was measured and rejected: it rescued 0 rows,
	// and it parses decoded_json again for every NULL row, so one corrupt
	// ADVERT that the backfill has not reached yet fails the whole scan with
	// "malformed JSON" (a 500 on /api/nodes?region=). Behind this index that
	// is worse, not better: the failed refresh leaves the index stale for
	// every region set. If you add a new write path for transmissions,
	// populate from_pubkey there rather than reintroducing a per-row JSON
	// parse here. nodes_region_from_pubkey_test.go locks the result set
	// against the JSON_EXTRACT expression; TestBackfillFromPubkey_* in
	// cmd/ingestor locks the backfill's half of the contract.
	q := fmt.Sprintf(`SELECT t.from_pubkey, UPPER(TRIM(obs.iata))
		FROM %[1]s
		CROSS JOIN observers obs ON %[2]s
		WHERE t.payload_type = ?
		AND t.from_pubkey IS NOT NULL
		AND obs.iata IS NOT NULL AND TRIM(obs.iata) != ''
		AND o.id > ? AND o.id <= ?`, from, joinCond)
	rows, err := db.conn.QueryContext(ctx, q, payloadTypeAdvert, lo, hi)
	if err != nil {
		return 0, fmt.Errorf("scan heard regions: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var pk, region string
		if err := rows.Scan(&pk, &region); err != nil {
			return n, fmt.Errorf("scan heard regions: %w", err)
		}
		agg.pair(pk, region)
		n++
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("scan heard regions: %w", err)
	}
	agg.watermark = max(agg.watermark, hi)
	return n, nil
}

// StartRegionIndexWarmer builds the active region index in the background
// and keeps refreshing it every regionIndexKeepInterval, so the first
// /api/nodes?region= request after startup does not pay for the full scan,
// the index never ages past nodeRegionMaxStale, and its periodic rebuild
// runs here rather than under a request. The returned stop cancels a scan
// in flight and waits for the goroutine; it is safe to call more than once.
func (db *DB) StartRegionIndexWarmer() (stop func()) {
	return db.startRegionIndexWarmer(regionIndexKeepInterval)
}

func (db *DB) startRegionIndexWarmer(every time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			ix, spec := db.activeRegionIndex()
			// Failures are logged by the flight; the next tick retries.
			_, _ = db.regionIndexFlight(ctx, ix, spec)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
