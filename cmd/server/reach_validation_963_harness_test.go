package main

// #963 M0 — the candidate-model harness: the #941 harness's pairs and
// labels (labelReachValidation), each pair's DTM and DSM profiles from two
// reach coverages (the DSM one over DHM's dhm_overflade), every candidate of
// reach_models_963_test.go scored per distance band and per lower antenna
// height, and the reach radius of a point. Test-only research code, read-
// only like #941's: OpenDB (mode=ro), no write anywhere.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"golang.org/x/sync/errgroup"
)

// reachValidation963Heights split the pairs by the lower antenna above
// the ground: role defaults are 10 m (repeater) and 5 m (room).
var reachValidation963Heights = []struct {
	name   string
	lo, hi float64
}{{"< 5 m", 0, 5}, {"5-10 m", 5, 10}, {">= 10 m", 10, math.Inf(1)}}

// reachRadius963Budgets are the reach-radius table's link budgets: today's
// default (158 dB) and the same less today's 25 dB environment loss.
var reachRadius963Budgets = [2]float64{158, 158 - envLoss963Db}

// surfaceSource963 asks its source for the surface model (DSM) instead.
type surfaceSource963 struct{ inner dhm.ElevationSource }

func (s surfaceSource963) FetchRaster(ctx context.Context, r dhm.RasterRequest) (dhm.Grid, error) {
	r.Model = dhm.ModelSurface
	return s.inner.FetchRaster(ctx, r)
}

// surfaceCoverage963 is c's twin over DHM's surface model: its own tile
// cache, but c's DHM sub-budget (the same limited source) and c's
// per-minute upstream budget, so the pair never exceeds the reach's own
// limits; the same foreign fill. nil without DHM: the GLO-30 fill is a
// surface model already, so the DTM grid then doubles as the DSM (no
// canopy, no clutter).
func surfaceCoverage963(c *terrainCoverage, cc CoverageConfig) *terrainCoverage {
	if c == nil || c.dhm == nil {
		return nil
	}
	s := newTerrainCoverage(cc, nil, terrainCoverageDeps{src: surfaceSource963{c.dhm}, fill: c.fill, link: c.link, logf: quietLogf})
	s.dhm = surfaceSource963{c.dhm}
	s.budget = c.budget
	s.covers = c.covers
	return s
}

// countReach963Upstream counts both coverages' upstream requests.
func countReach963Upstream(c, cs *terrainCoverage) func() int64 {
	a := countReachUpstream(c)
	if cs == nil {
		return a
	}
	b := countReachUpstream(cs)
	return func() int64 { return a() + b() }
}

// ── Gathering: the harness's own tile fetch ───────────────────────────────

// The harness's stages, as errors, logs and the report name them. The
// clutter class (land cover) has no stage of its own: it is computed from
// the DTM and DSM grids, with no upstream call.
const (
	stageDTM963 = "DTM"
	stageDSM963 = "DSM"
)

// gather963 is how the harness gathers terrain. A long run must not end
// on one slow tile, and the reach's own gather would: it bounds each tile
// at coveragePerCall (10 s, the wait for one of the reach's 2 upstream
// slots included) and fails the whole disk on one failure. So the harness
// fetches each DHM tile itself first (fetchTiles963) and gather then
// reads them all from the coverage's cache.
type gather963 struct {
	timeout time.Duration                // one tile attempt
	retries int                          // attempts after the first
	backoff time.Duration                // before the first retry; doubles per retry
	sleep   func(time.Duration)          // waits for the upstream budget and the backoff
	logf    func(string, ...interface{}) // progress, and each tile given up; nil: silent
	every   int                          // a progress line every this many disks; 0: none
	// upstream counts the upstream calls so far, for the progress line;
	// nil: not shown.
	upstream func() int64
}

const (
	fetchTimeout963  = 60 * time.Second
	fetchRetries963  = 3
	fetchBackoff963  = 5 * time.Second
	progressEvery963 = 25
)

// defaultGather963 is the harness's gathering with the default policy,
// waiting with sleep and silent.
func defaultGather963(sleep func(time.Duration)) gather963 {
	return gather963{timeout: fetchTimeout963, retries: fetchRetries963, backoff: fetchBackoff963, sleep: sleep}
}

// gather963FromEnv is defaultGather963(time.Sleep) with the opt-in runs'
// settings: CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT (a Go duration
// such as 90s, or seconds), _FETCH_RETRIES and _PROGRESS_EVERY (disks).
func gather963FromEnv(logf func(string, ...interface{})) (gather963, error) {
	g := defaultGather963(time.Sleep)
	g.logf, g.every = logf, progressEvery963
	if v := os.Getenv("CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			s, ferr := strconv.ParseFloat(v, 64)
			if ferr != nil {
				return g, fmt.Errorf("CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT %q: want a duration such as 90s, or seconds", v)
			}
			d = time.Duration(s * float64(time.Second))
		}
		if d <= 0 {
			return g, fmt.Errorf("CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT %q: must be positive", v)
		}
		g.timeout = d
	}
	for _, e := range []struct {
		name string
		to   *int
	}{{"CORESCOPE_REACH_VALIDATION_963_FETCH_RETRIES", &g.retries}, {"CORESCOPE_REACH_VALIDATION_963_PROGRESS_EVERY", &g.every}} {
		if v := os.Getenv(e.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return g, fmt.Errorf("%s %q: want a whole number ≥ 0", e.name, v)
			}
			*e.to = n
		}
	}
	return g, nil
}

func (g gather963) log(format string, args ...interface{}) {
	if g.logf != nil {
		g.logf(format, args...)
	}
}

// reachFetch963Stats counts one source's tile fetches by the harness.
type reachFetch963Stats struct {
	Fetched int `json:"tilesFetched"` // on the first attempt or a retry
	Retried int `json:"retries"`      // failed attempts tried again
	Failed  int `json:"tilesFailed"`  // given up: no terrain there
	// Reasons counts every failed attempt by its category (timeout,
	// status 503, ...).
	Reasons map[string]int `json:"failureReasons,omitempty"`
}

func (s *reachFetch963Stats) add(o reachFetch963Stats) {
	s.Fetched += o.Fetched
	s.Retried += o.Retried
	s.Failed += o.Failed
	for k, v := range o.Reasons {
		if s.Reasons == nil {
			s.Reasons = map[string]int{}
		}
		s.Reasons[k] += v
	}
}

func (s reachFetch963Stats) text() string {
	out := fmt.Sprintf("%d fetched, %d retries, %d failed", s.Fetched, s.Retried, s.Failed)
	if len(s.Reasons) == 0 {
		return out
	}
	keys := make([]string, 0, len(s.Reasons))
	for k := range s.Reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = fmt.Sprintf("%s ×%d", k, s.Reasons[k])
	}
	return out + " (" + strings.Join(keys, ", ") + ")"
}

// reachFetch963 is the harness's tile fetches per source.
type reachFetch963 struct {
	DTM reachFetch963Stats `json:"dtm"`
	DSM reachFetch963Stats `json:"dsm"`
}

func (f *reachFetch963) add(o reachFetch963) {
	f.DTM.add(o.DTM)
	f.DSM.add(o.DSM)
}

func (f reachFetch963) failed() bool { return f.DTM.Failed+f.DSM.Failed > 0 }

func (f reachFetch963) text() string {
	return "DTM " + f.DTM.text() + "; DSM " + f.DSM.text()
}

// tileError963 is a fatal tile fetch, with where it happened.
type tileError963 struct {
	stage    string
	tile     dhm.CoverageTile
	bbox     [4]float64 // EPSG:25832
	attempts int
	err      error
}

func (e tileError963) Error() string {
	return fmt.Sprintf("%s tile %d,%d (EPSG:25832 bbox %.0f,%.0f,%.0f,%.0f), attempt %d: %v",
		e.stage, e.tile.X, e.tile.Y, e.bbox[0], e.bbox[1], e.bbox[2], e.bbox[3], e.attempts, e.err)
}

func (e tileError963) Unwrap() error { return e.err }

// retryable963 reports whether a failed tile fetch may be retried and,
// failing that, left without terrain: every upstream failure but a
// refused token. A request the client refuses (dhm.ErrBadRequest) is a
// harness bug and fatal.
func retryable963(err error) bool {
	var fe dhm.FetchError
	if !errors.As(err, &fe) {
		return false
	}
	return fe.Reason != "status 401" && fe.Reason != "status 403"
}

// fetchTiles963 fetches the plan's DHM tiles that cov's gather would
// fetch (in the raster extent, partly Danish, not cached) and caches them,
// so that gather finds every one in the cache. Each attempt has
// g.timeout, through cov's DHM sub-budget and per-minute upstream budget,
// with at most coverageUpstreamSlots tiles at once (the sub-budget's
// slots, so an attempt never waits for one). A failed attempt is retried
// up to g.retries times, the backoff doubling each time. A tile that
// still fails is cached empty, as a tile outside Denmark is: no terrain,
// so the pairs over it count as without terrain, as #941 counts a gap.
// Fatal, with stage and tile: the run's context ending, or a failure no
// retry mends (retryable963).
func fetchTiles963(ctx context.Context, cov *terrainCoverage, stage string, plan []dhm.CoverageTile, res float64, g gather963) (reachFetch963Stats, error) {
	var st reachFetch963Stats
	if cov.dhm == nil {
		return st, nil
	}
	now := cov.now()
	var todo []dhm.CoverageTile
	for _, t := range plan {
		if _, ok := t.Raster(res); !ok {
			continue
		}
		if _, need := cov.cachedTile(coverageTileKey{Res: int(res), X: t.X, Y: t.Y}, now); !need {
			continue
		}
		if some, _ := cov.tileCoverage(t, res); some {
			todo = append(todo, t)
		}
	}
	var mu sync.Mutex
	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(coverageUpstreamSlots)
	for _, t := range todo {
		eg.Go(func() error {
			one, err := fetchTile963(ectx, cov, stage, t, res, g)
			mu.Lock()
			st.add(one)
			mu.Unlock()
			return err
		})
	}
	err := eg.Wait()
	if err == nil {
		err = ctx.Err()
	}
	return st, err
}

// fetchTile963 is one tile of fetchTiles963.
func fetchTile963(ctx context.Context, cov *terrainCoverage, stage string, t dhm.CoverageTile, res float64, g gather963) (reachFetch963Stats, error) {
	var st reachFetch963Stats
	k := coverageTileKey{Res: int(res), X: t.X, Y: t.Y}
	r, _ := t.Raster(res)
	for attempt := 1; ; attempt++ {
		for {
			ok, retry := cov.budget.take(1)
			if ok {
				break
			}
			g.sleep(max(time.Second, retry))
		}
		actx, cancel := context.WithTimeout(ctx, g.timeout)
		grid, err := cov.dhm.FetchRaster(actx, r)
		cancel()
		if err == nil {
			cov.tiles.Put(k, dhm.EncodeCoverageTile(dhm.CoverageTileFromGrid(&grid, t, res, cov.covers)), cov.now())
			st.Fetched++
			return st, nil
		}
		fatal := tileError963{stage: stage, tile: t, bbox: r.BBox, attempts: attempt, err: err}
		if ctx.Err() != nil {
			fatal.err = fmt.Errorf("run stopped (%v) while fetching: %w", ctx.Err(), err)
			return st, fatal
		}
		if !retryable963(err) {
			return st, fatal
		}
		if st.Reasons == nil {
			st.Reasons = map[string]int{}
		}
		st.Reasons[err.Error()]++
		if attempt > g.retries {
			cov.tiles.Put(k, []byte{}, cov.now())
			st.Failed++
			g.log("963: %s tile %d,%d given up after %d attempts (%v): no terrain there", stage, t.X, t.Y, attempt, err)
			return st, nil
		}
		st.Retried++
		g.sleep(g.backoff << (attempt - 1))
	}
}

// diskError963 is a fatal error gathering a disk, with stage and where.
func diskError963(stage string, d reachDisk, res float64, err error) error {
	e, n := dhm.ToUTM32(d.a.pt.Lat, d.a.pt.Lon)
	m := d.radiusM + 2*res
	return fmt.Errorf("%s gather, disk of %.0f km around %s at %.5f,%.5f (EPSG:25832 bbox %.0f,%.0f,%.0f,%.0f): %w",
		stage, d.radiusM/1000, pkPrefix963(d.a.pk), d.a.pt.Lat, d.a.pt.Lon, e-m, n-m, e+m, n+m, err)
}

// pkPrefix963 is a node key's prefix for errors and logs; "point" for the
// reach-radius point, which has none.
func pkPrefix963(pk string) string {
	switch {
	case pk == "":
		return "point"
	case len(pk) > 8:
		return pk[:8]
	}
	return pk
}

// gathered963 is what gathering a disk's DTM and DSM took.
type gathered963 struct {
	tiles, surfTiles int
	partial          bool
	fetch            reachFetch963
}

// gatherBoth963 is a disk's DTM and DSM grids, each fetched by
// fetchTiles963 and gathered through its coverage; the DSM is the DTM
// without a surface coverage.
func gatherBoth963(ctx context.Context, c, cs *terrainCoverage, d reachDisk, g gather963) (tg, sg *dhm.CoverageGrid, out gathered963, err error) {
	one := func(cov *terrainCoverage, stage string, st *reachFetch963Stats) (*dhm.CoverageGrid, int, bool, error) {
		res := float64(cov.set.resolutionM)
		e, n := dhm.ToUTM32(d.a.pt.Lat, d.a.pt.Lon)
		f, err := fetchTiles963(ctx, cov, stage, dhm.PlanCoverageTiles(e, n, d.radiusM, res), res, g)
		st.add(f)
		if err != nil {
			return nil, 0, false, fmt.Errorf("%s fetch, disk of %s: %w", stage, pkPrefix963(d.a.pk), err)
		}
		grid, tiles, part, err := gatherReachDisk(ctx, cov, d, g.sleep)
		if err != nil {
			return nil, 0, false, diskError963(stage, d, res, err)
		}
		return grid, tiles, part, nil
	}
	tg, out.tiles, out.partial, err = one(c, stageDTM963, &out.fetch.DTM)
	if err != nil || cs == nil {
		return tg, tg, out, err
	}
	sg, n, part, err := one(cs, stageDSM963, &out.fetch.DSM)
	out.surfTiles, out.partial = n, out.partial || part
	return tg, sg, out, err
}

// evaluated963 is what evaluating the candidates gathered and found.
type evaluated963 struct {
	gathered963
	classes map[string]clutterClass963 // each scored node's clutter class
}

// evaluateCandidates963 computes every candidate's loss for every pair
// into p.loss (keyed by the candidate id), sampling each path exactly as
// dhm.ReachLosses does, and returns each scored node's clutter class. It
// logs progress every g.every disks.
func evaluateCandidates963(ctx context.Context, c, cs *terrainCoverage, pairs []*valPair, g gather963) (ev evaluated963, err error) {
	res := float64(c.set.resolutionM)
	freq, _ := c.link.frequency(nil)
	k, _ := c.link.kFactor(nil)
	ev.classes = map[string]clutterClass963{}
	disks := reachDisks(c, pairs)
	start, donePairs := time.Now(), 0
	for di, d := range disks {
		tg, sg, got, gerr := gatherBoth963(ctx, c, cs, d, g)
		ev.fetch.add(got.fetch)
		if gerr != nil {
			return ev, fmt.Errorf("disk %d/%d: %w", di+1, len(disks), gerr)
		}
		ev.tiles, ev.surfTiles, ev.partial = ev.tiles+got.tiles, ev.surfTiles+got.surfTiles, ev.partial || got.partial || got.fetch.failed()
		donePairs += len(d.pairs)
		if g.every > 0 && ((di+1)%g.every == 0 || di+1 == len(disks)) {
			up := ""
			if g.upstream != nil {
				up = ", " + upstreamText(g.upstream())
			}
			g.log("963: disk %d/%d, %d/%d pairs, %d + %d tiles; tile fetches %s%s; %v elapsed",
				di+1, len(disks), donePairs, len(pairs), ev.tiles, ev.surfTiles, ev.fetch.text(), up, time.Since(start).Round(time.Second))
		}
		e0, n0 := dhm.ToUTM32(d.a.pt.Lat, d.a.pt.Lon)
		txClass := clutterAt963(tg, sg, e0, n0, res)
		ev.classes[d.a.pk] = txClass
		for _, p := range d.pairs {
			if err := ctx.Err(); err != nil {
				return ev, err
			}
			e1, n1 := dhm.ToUTM32(p.b.pt.Lat, p.b.pt.Lon)
			dist := math.Hypot(e1-e0, n1-n0)
			rxClass := clutterAt963(tg, sg, e1, n1, res)
			ev.classes[p.b.pk] = rxClass
			p.loss = map[dhm.PathModel]float64{}
			if dist < 1 {
				for _, id := range candidates963 {
					p.loss[dhm.PathModel(id)] = math.NaN()
				}
				continue
			}
			ns := max(1, int(math.Round(dist/res)))
			dtm, dsm := make([]float32, ns+1), make([]float32, ns+1)
			for i := 0; i <= ns; i++ {
				f := float64(i) / float64(ns)
				e, n := e0+f*(e1-e0), n0+f*(n1-n0)
				dtm[i], dsm[i] = tg.Sample(e, n), sg.Sample(e, n)
			}
			l := candidateLosses963(path963{dtm: dtm, dsm: dsm, distanceM: dist, txAGL: d.a.heightM, rxAGL: p.b.heightM,
				freqMHz: freq, k: k, endClass: [2]clutterClass963{txClass, rxClass}})
			for _, id := range candidates963 {
				p.loss[dhm.PathModel(id)] = l[id]
			}
		}
	}
	return ev, nil
}

// ── Report ─────────────────────────────────────────────────────────────────

type reachValidation963Report struct {
	Version    int                           `json:"version"`
	Issue      string                        `json:"issue"`
	DataAsOf   string                        `json:"dataAsOf"`
	Params     reachValidationParams         `json:"params"`
	Terrain    reachValidation963Terrain     `json:"terrain"`
	Pairs      reachValidationPairCounts     `json:"pairs"`
	EndClasses reachValidation963Classes     `json:"endClasses"`
	SNR        observedSNR963Stats           `json:"observedSnr"`
	Candidates []reachValidation963Candidate `json:"candidates"`
	Notes      []string                      `json:"notes"`
}

type reachValidation963Terrain struct {
	Source        string `json:"source"`
	SurfaceSource string `json:"surfaceSource"`
	Tiles         int    `json:"tilesGathered"`
	SurfaceTiles  int    `json:"surfaceTilesGathered"`
	Upstream      int64  `json:"upstreamCalls"`
	Partial       bool   `json:"partial"`
	// Fetch is the harness's DHM tile fetches per source; a failed tile
	// has no terrain, and its pairs count as without terrain.
	Fetch         reachFetch963 `json:"fetch"`
	FetchTimeoutS float64       `json:"fetchTimeoutS"`
	FetchRetries  int           `json:"fetchRetries"`
}

// reachValidation963Classes counts the scored nodes by clutter class.
type reachValidation963Classes struct {
	Open     int `json:"open"`
	Suburban int `json:"suburban"`
	Urban    int `json:"urbanOrForest"`
}

type reachValidation963Candidate struct {
	ID      string                 `json:"id"`
	Overall reachValidationMetrics `json:"overall"`
	// OverallCappedOffsetDb is the median SNR offset with both SNRs capped
	// at the receiver's saturation (cappedSNROffset963).
	OverallCappedOffsetDb *float64                 `json:"medianSnrOffsetCappedDb"`
	Bands                 []reachValidation963Band `json:"bands"`
	Heights               []reachValidation963Band `json:"heights"` // by the lower antenna above the ground
}

type reachValidation963Band struct {
	reachValidationBand
	CappedOffsetDb *float64 `json:"medianSnrOffsetCappedDb"`
}

// snrCap963 is the SNR a receiver reports at most (A: SX126x packet SNR
// saturates near the top of its range; the report's observed SNR summary
// shows where), CORESCOPE_REACH_VALIDATION_963_SNR_CAP or 10 dB.
func snrCap963() float64 {
	if v, err := strconv.ParseFloat(os.Getenv("CORESCOPE_REACH_VALIDATION_963_SNR_CAP"), 64); err == nil {
		return v
	}
	return 10
}

// cappedSNROffset963 is the median of observed minus predicted SNR over
// the positives with an SNR history, both capped at capDb: a censored
// comparison, so a link the receiver reports at its saturation is not
// scored as tens of dB optimistic. nil without such pairs.
func cappedSNROffset963(ps []*valPair, m dhm.PathModel, radio CoverageRadio, capDb float64) *float64 {
	var offs []float64
	for _, p := range ps {
		if p.snr != nil && p.positive {
			offs = append(offs, math.Min(*p.snr, capDb)-math.Min(predictedSNR(p, m, radio), capDb))
		}
	}
	if med := medianOf(offs); med != nil {
		return round4(*med)
	}
	return nil
}

// observedSNR963Stats summarises the observed SNR of the positives.
type observedSNR963Stats struct {
	Pairs        int      `json:"pairs"`
	P50Db        *float64 `json:"p50Db"`
	P95Db        *float64 `json:"p95Db"`
	MaxDb        *float64 `json:"maxDb"`
	CapDb        float64  `json:"capDb"`
	AtOrAboveCap int      `json:"atOrAboveCap"`
}

func observedSNR963(ps []*valPair, capDb float64) observedSNR963Stats {
	out := observedSNR963Stats{CapDb: capDb}
	var v []float64
	for _, p := range ps {
		if p.snr != nil && p.positive {
			v = append(v, *p.snr)
			if *p.snr >= capDb {
				out.AtOrAboveCap++
			}
		}
	}
	out.Pairs = len(v)
	if len(v) == 0 {
		return out
	}
	sort.Float64s(v)
	out.P50Db = medianOf(v)
	out.P95Db = round4(v[int(math.Ceil(0.95*float64(len(v))))-1])
	out.MaxDb = round4(v[len(v)-1])
	return out
}

func (in *reachValidationInput) count963(classes map[string]clutterClass963) reachValidation963Classes {
	var out reachValidation963Classes
	for _, cl := range classes {
		switch cl {
		case clutterSuburban963:
			out.Suburban++
		case clutterUrban963:
			out.Urban++
		default:
			out.Open++
		}
	}
	return out
}

// buildReachValidation963Report scores every candidate over the pairs
// every candidate has a loss for.
func buildReachValidation963Report(l reachValidationLabels, c *terrainCoverage, o reachValidationOptions, classes map[string]clutterClass963) reachValidation963Report {
	base := l.report(c, o) // #941's params and counts; its models are not used
	rep := reachValidation963Report{Version: 1, Issue: "963", DataAsOf: base.DataAsOf, Params: base.Params,
		EndClasses: l.in.count963(classes), Candidates: []reachValidation963Candidate{}}
	rep.Pairs = reachValidationPairCounts{Nodes: base.Pairs.Nodes, HiddenNodes: base.Pairs.HiddenNodes, Candidates: base.Pairs.Candidates,
		AmbiguousEdges: base.Pairs.AmbiguousEdges, WeakEdges: base.Pairs.WeakEdges, PositivesBeyond: base.Pairs.PositivesBeyond}
	models := make([]dhm.PathModel, len(candidates963))
	for i, id := range candidates963 {
		models[i] = dhm.PathModel(id)
	}
	ps := scoredPairs(l.pairs, models, &rep.Pairs)
	capDb := snrCap963()
	rep.SNR = observedSNR963(ps, capDb)
	split := func(m dhm.PathModel, of func(*valPair) float64, bands []struct {
		name   string
		lo, hi float64
	}) []reachValidation963Band {
		out := make([]reachValidation963Band, 0, len(bands))
		for _, b := range bands {
			var in []*valPair
			for _, p := range ps {
				if v := of(p); v >= b.lo && v < b.hi {
					in = append(in, p)
				}
			}
			out = append(out, reachValidation963Band{reachValidationBand: reachValidationBand{Band: b.name, reachValidationMetrics: scoreMetrics(in, m, c.radio)},
				CappedOffsetDb: cappedSNROffset963(in, m, c.radio, capDb)})
		}
		return out
	}
	km := func(p *valPair) float64 { return p.distanceM / 1000 }
	lower := func(p *valPair) float64 { return math.Min(p.a.heightM, p.b.heightM) }
	for _, m := range models {
		rep.Candidates = append(rep.Candidates, reachValidation963Candidate{ID: string(m), Overall: scoreMetrics(ps, m, c.radio),
			OverallCappedOffsetDb: cappedSNROffset963(ps, m, c.radio, capDb),
			Bands:                 split(m, km, reachValidationBands), Heights: split(m, lower, reachValidation963Heights)})
	}
	rep.Notes = []string{
		"Candidates are test-only prototypes (cmd/server/reach_models_963_test.go); delta is today's model, delta+env25 today's model with the panel's default 25 dB environment loss.",
		"Clutter classes come from the canopy (DSM − DTM) within 100 m of each end: open/rural (P.2108 R = 10 m, eq. 2b), suburban (R = 10 m, eq. 2a) or urban/forest (R = 15 m, eq. 2a). +p2108 raises a terminal below R to R for the path and adds the correction (P.526-16 §4.5); delta+p2108c does so only at suburban and urban/forest ends and leaves open land to delta. Without a DHM surface model (GLO-30 only) every end is open.",
		"mm-proxy follows map.meshcore.io's approach (forest as 25 m obstacles, P.2108 eq. 2a at the antennas' own heights with no loss in open land, a 10 dB fade margin) with DHM canopy instead of OpenStreetMap land cover and delta-Bullington instead of Deygout.",
		"Negatives are pairs never observed while both nodes were active: weak evidence, so the false-reach rate is an upper bound.",
		"Median SNR offset: observed minus predicted SNR; negative means the candidate is optimistic. The capped offset caps both at the SNR a receiver reports at most (observedSnr.capDb), so links at the receiver's saturation do not count as optimistic.",
	}
	return rep
}

func (r reachValidation963Report) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Predicted reach candidates (#963 M0)\n\nData as of %s. Radius %.0f km, %d m grid, %.3f MHz, k %.2f, link budget %.0f dB (reached: margin ≥ %.0f dB), window %.0f days. Terrain: %s, surface: %s, %d + %d tiles gathered, %s%s.\n\n",
		r.DataAsOf, r.Params.RadiusKm, r.Params.ResolutionM, r.Params.FreqMHz, r.Params.K, r.Params.BudgetDb, r.Params.ReachedMarginDb, r.Params.WindowDays,
		r.Terrain.Source, r.Terrain.SurfaceSource, r.Terrain.Tiles, r.Terrain.SurfaceTiles, upstreamText(r.Terrain.Upstream),
		map[bool]string{true: " (partial)", false: ""}[r.Terrain.Partial])
	p := r.Pairs
	fmt.Fprintf(&b, "DHM tile fetches (%g s timeout, up to %d retries; a failed tile has no terrain): %s.\n\n", r.Terrain.FetchTimeoutS, r.Terrain.FetchRetries, r.Terrain.Fetch.text())
	fmt.Fprintf(&b, "Pairs: %d nodes, %d scored (%d positives, %d negatives), %d with SNR, %d without terrain.\n\n", p.Nodes, p.Scored, p.Positives, p.Negatives, p.WithSNR, p.NoTerrain)
	fmt.Fprintf(&b, "## Clutter classes at the ends\n\n| Open | Suburban | Urban / forest |\n|---|---|---|\n| %d | %d | %d |\n\n", r.EndClasses.Open, r.EndClasses.Suburban, r.EndClasses.Urban)
	sn := r.SNR
	fmt.Fprintf(&b, "## Observed SNR\n\nPositives with an SNR history: %d; median %s dB, 95th percentile %s dB, max %s dB; %d at or above the cap of %.1f dB.\n\n",
		sn.Pairs, fmtPtr(sn.P50Db, false), fmtPtr(sn.P95Db, false), fmtPtr(sn.MaxDb, false), sn.AtOrAboveCap, sn.CapDb)
	b.WriteString("## Overall\n\n| Candidate | Hit rate | False-reach rate | ROC-AUC | SNR pairs | Spearman ρ | Median SNR offset (dB) | Capped offset (dB) |\n|---|---|---|---|---|---|---|---|\n")
	for _, c := range r.Candidates {
		m := c.Overall
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %s | %s | %s |\n", c.ID, fmtPtr(m.HitRate, true), fmtPtr(m.FalseReachRate, true), fmtPtr(m.AUC, false),
			m.SNRPairs, fmtPtr(m.Spearman, false), fmtPtr(m.MedianSNROffsetDb, false), fmtPtr(c.OverallCappedOffsetDb, false))
	}
	matrix := func(title string, bands func(reachValidation963Candidate) []reachValidation963Band, cell func(reachValidation963Band) string) {
		if len(r.Candidates) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n| Candidate |", title)
		head := bands(r.Candidates[0])
		for _, band := range head {
			fmt.Fprintf(&b, " %s (%d/%d) |", band.Band, band.Positives, band.Negatives)
		}
		b.WriteString("\n|---|" + strings.Repeat("---|", len(head)) + "\n")
		for _, c := range r.Candidates {
			fmt.Fprintf(&b, "| %s |", c.ID)
			for _, band := range bands(c) {
				fmt.Fprintf(&b, " %s |", cell(band))
			}
			b.WriteString("\n")
		}
	}
	dist := func(c reachValidation963Candidate) []reachValidation963Band { return c.Bands }
	hgt := func(c reachValidation963Candidate) []reachValidation963Band { return c.Heights }
	hit := func(m reachValidation963Band) string { return fmtPtr(m.HitRate, true) }
	fr := func(m reachValidation963Band) string { return fmtPtr(m.FalseReachRate, true) }
	auc := func(m reachValidation963Band) string { return fmtPtr(m.AUC, false) }
	off := func(m reachValidation963Band) string {
		return fmt.Sprintf("%s / %s (n %d)", fmtPtr(m.MedianSNROffsetDb, false), fmtPtr(m.CappedOffsetDb, false), m.SNRPairs)
	}
	b.WriteString("\nBand headers give positives/negatives.\n")
	matrix("Hit rate per distance band", dist, hit)
	matrix("False-reach rate per distance band", dist, fr)
	matrix("ROC-AUC per distance band", dist, auc)
	matrix("Median SNR offset per distance band (raw / capped)", dist, off)
	matrix("Hit rate per lower antenna height", hgt, hit)
	matrix("False-reach rate per lower antenna height", hgt, fr)
	matrix("ROC-AUC per lower antenna height", hgt, auc)
	matrix("Median SNR offset per lower antenna height (raw / capped)", hgt, off)
	b.WriteString("\n## Notes\n\n")
	for _, n := range r.Notes {
		b.WriteString("- " + n + "\n")
	}
	return b.String()
}

// runReachValidation963 is the candidate harness: read and label (#941),
// evaluate every candidate, score, write reach-validation-963.{json,md}.
// A tile that fails after g's retries leaves its pairs without terrain;
// a fatal error names its stage and where.
func runReachValidation963(ctx context.Context, srv *Server, c, cs *terrainCoverage, conn *sql.DB, o reachValidationOptions, g gather963) (reachValidation963Report, error) {
	l, err := labelReachValidation(ctx, srv, c, conn, o)
	if err != nil {
		return reachValidation963Report{}, fmt.Errorf("labels (database): %w", err)
	}
	ev, err := evaluateCandidates963(ctx, c, cs, l.pairs, g)
	if err != nil {
		return reachValidation963Report{}, err
	}
	rep := buildReachValidation963Report(l, c, o, ev.classes)
	rep.Terrain = reachValidation963Terrain{Source: o.terrainSource, Tiles: ev.tiles, SurfaceTiles: ev.surfTiles, Upstream: o.upstream(), Partial: ev.partial,
		SurfaceSource: "none (the DTM grid; GLO-30 is a surface model)", Fetch: ev.fetch, FetchTimeoutS: g.timeout.Seconds(), FetchRetries: g.retries}
	if cs != nil {
		rep.Terrain.SurfaceSource = "dhm surface (dhm_overflade)"
	}
	return rep, writeReport963(o.outDir, "reach-validation-963", rep, rep.markdown())
}

// writeReport963 writes name.json and name.md into dir; "" writes nothing.
func writeReport963(dir, name string, v interface{}, md string) error {
	if dir == "" {
		return nil
	}
	js, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(js, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".md"), []byte(md), 0o644)
}

// ── Reach radius at a point ────────────────────────────────────────────────

// reachRadius963Row is one candidate's reach radius over the bearings, at
// each of reachRadius963Budgets.
type reachRadius963Row struct {
	Candidate string     `json:"candidate"`
	MedianKm  [2]float64 `json:"medianKm"`
	MinKm     [2]float64 `json:"minKm"`
	MaxKm     [2]float64 `json:"maxKm"`
}

type reachRadius963Report struct {
	Lat         float64             `json:"lat"`
	Lon         float64             `json:"lon"`
	TxHeightM   float64             `json:"txHeightM"`
	RxHeightM   float64             `json:"rxHeightM"`
	RadiusKm    float64             `json:"radiusKm"`
	ResolutionM int                 `json:"resolutionM"`
	Bearings    int                 `json:"bearings"`
	BudgetsDb   [2]float64          `json:"budgetsDb"`
	Terrain     string              `json:"terrain"`
	TxClass     string              `json:"txClass"`
	Upstream    int64               `json:"upstreamCalls"`
	Fetch       reachFetch963       `json:"fetch"`
	Rows        []reachRadius963Row `json:"rows"`
}

var clutterNames963 = [...]string{"open", "suburban", "urban/forest"}

// reachRadiusRows963 summarises reachLosses963's output per candidate.
func reachRadiusRows963(rr map[string][][]float64, res float64) []reachRadius963Row {
	var rows []reachRadius963Row
	for _, id := range candidates963 {
		row := reachRadius963Row{Candidate: id}
		for i, budget := range reachRadius963Budgets {
			r := reachRadius963(rr[id], res, budget)
			sort.Float64s(r)
			if len(r) == 0 {
				continue
			}
			row.MinKm[i], row.MaxKm[i] = r[0]/1000, r[len(r)-1]/1000
			row.MedianKm[i] = r[len(r)/2] / 1000
			if len(r)%2 == 0 {
				row.MedianKm[i] = (r[len(r)/2-1] + r[len(r)/2]) / 2000
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// reachRadiusAt963 is the reach radius of every candidate from pt, with
// antennas txH and rxH above the ground, over the coverages' terrain.
func reachRadiusAt963(ctx context.Context, c, cs *terrainCoverage, pt dhm.LatLon, txH, rxH, radiusM float64, bearings int, g gather963) (reachRadius963Report, error) {
	res := float64(c.set.resolutionM)
	tg, sg, got, err := gatherBoth963(ctx, c, cs, reachDisk{a: &valNode{pt: pt}, radiusM: radiusM*1.01 + 2*res}, g)
	if err != nil {
		return reachRadius963Report{}, fmt.Errorf("reach radius: %w", err)
	}
	freq, _ := c.link.frequency(nil)
	k, _ := c.link.kFactor(nil)
	e0, n0 := dhm.ToUTM32(pt.Lat, pt.Lon)
	rr := reachLosses963(tg, sg, e0, n0, txH, rxH, freq, k, radiusM, res, bearings)
	return reachRadius963Report{Lat: pt.Lat, Lon: pt.Lon, TxHeightM: txH, RxHeightM: rxH, RadiusKm: radiusM / 1000, ResolutionM: c.set.resolutionM,
		Bearings: bearings, BudgetsDb: reachRadius963Budgets, TxClass: clutterNames963[clutterAt963(tg, sg, e0, n0, res)],
		Fetch: got.fetch, Rows: reachRadiusRows963(rr, res)}, nil
}

func (r reachRadius963Report) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Reach radius (#963 M0)\n\nPoint %.4f, %.4f (clutter class %s), transmitter %.0f m, receiver %.0f m above the ground, %d bearings to %.0f km on a %d m grid. Terrain: %s. Reached: margin ≥ 3 dB.\n\n",
		r.Lat, r.Lon, r.TxClass, r.TxHeightM, r.RxHeightM, r.Bearings, r.RadiusKm, r.ResolutionM, r.Terrain)
	fmt.Fprintf(&b, "DHM tile fetches of this run (tiles cached by the validation run are not counted; a failed tile has no terrain): %s.\n\n", r.Fetch.text())
	fmt.Fprintf(&b, "| Candidate | median km at %.0f dB | min–max km | median km at %.0f dB | min–max km |\n|---|---|---|---|---|\n", r.BudgetsDb[0], r.BudgetsDb[1])
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "| %s | %.2f | %.2f–%.2f | %.2f | %.2f–%.2f |\n", row.Candidate, row.MedianKm[0], row.MinKm[0], row.MaxKm[0], row.MedianKm[1], row.MinKm[1], row.MaxKm[1])
	}
	return b.String()
}

// reachPoint963 is CORESCOPE_REACH_VALIDATION_963_POINT, or open Jutland
// (the point #963's ground-reflection comment names).
func reachPoint963() (dhm.LatLon, error) {
	s := os.Getenv("CORESCOPE_REACH_VALIDATION_963_POINT")
	if s == "" {
		return dhm.LatLon{Lat: 56.136, Lon: 8.975}, nil
	}
	var pt dhm.LatLon
	if _, err := fmt.Sscanf(s, "%f,%f", &pt.Lat, &pt.Lon); err != nil {
		return pt, fmt.Errorf("CORESCOPE_REACH_VALIDATION_963_POINT %q: want lat,lon", s)
	}
	return pt, nil
}
