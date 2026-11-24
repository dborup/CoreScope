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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meshcore-analyzer/dhm"
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

// gatherBoth963 is a disk's DTM and DSM grids; the DSM is the DTM without
// a surface coverage.
func gatherBoth963(ctx context.Context, c, cs *terrainCoverage, d reachDisk, sleep func(time.Duration)) (tg, sg *dhm.CoverageGrid, tiles, surfTiles int, partial bool, err error) {
	tg, tiles, partial, err = gatherReachDisk(ctx, c, d, sleep)
	if err != nil || cs == nil {
		return tg, tg, tiles, 0, partial, err
	}
	sg, surfTiles, p2, err := gatherReachDisk(ctx, cs, d, sleep)
	return tg, sg, tiles, surfTiles, partial || p2, err
}

// evaluateCandidates963 computes every candidate's loss for every pair
// into p.loss (keyed by the candidate id), sampling each path exactly as
// dhm.ReachLosses does, and returns each scored node's clutter class.
func evaluateCandidates963(ctx context.Context, c, cs *terrainCoverage, pairs []*valPair, sleep func(time.Duration)) (tiles, surfTiles int, partial bool, classes map[string]clutterClass963, err error) {
	res := float64(c.set.resolutionM)
	freq, _ := c.link.frequency(nil)
	k, _ := c.link.kFactor(nil)
	classes = map[string]clutterClass963{}
	for _, d := range reachDisks(c, pairs) {
		tg, sg, n, ns, part, gerr := gatherBoth963(ctx, c, cs, d, sleep)
		if gerr != nil {
			return tiles, surfTiles, partial, classes, gerr
		}
		tiles, surfTiles, partial = tiles+n, surfTiles+ns, partial || part
		e0, n0 := dhm.ToUTM32(d.a.pt.Lat, d.a.pt.Lon)
		txClass := clutterAt963(tg, sg, e0, n0, res)
		classes[d.a.pk] = txClass
		for _, p := range d.pairs {
			if err := ctx.Err(); err != nil {
				return tiles, surfTiles, partial, classes, err
			}
			e1, n1 := dhm.ToUTM32(p.b.pt.Lat, p.b.pt.Lon)
			dist := math.Hypot(e1-e0, n1-n0)
			rxClass := clutterAt963(tg, sg, e1, n1, res)
			classes[p.b.pk] = rxClass
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
	return tiles, surfTiles, partial, classes, nil
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
func runReachValidation963(ctx context.Context, srv *Server, c, cs *terrainCoverage, conn *sql.DB, o reachValidationOptions) (reachValidation963Report, error) {
	l, err := labelReachValidation(ctx, srv, c, conn, o)
	if err != nil {
		return reachValidation963Report{}, err
	}
	tiles, surfTiles, partial, classes, err := evaluateCandidates963(ctx, c, cs, l.pairs, o.sleep)
	if err != nil {
		return reachValidation963Report{}, err
	}
	rep := buildReachValidation963Report(l, c, o, classes)
	rep.Terrain = reachValidation963Terrain{Source: o.terrainSource, Tiles: tiles, SurfaceTiles: surfTiles, Upstream: o.upstream(), Partial: partial,
		SurfaceSource: "none (the DTM grid; GLO-30 is a surface model)"}
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
func reachRadiusAt963(ctx context.Context, c, cs *terrainCoverage, pt dhm.LatLon, txH, rxH, radiusM float64, bearings int, sleep func(time.Duration)) (reachRadius963Report, error) {
	res := float64(c.set.resolutionM)
	tg, sg, _, _, _, err := gatherBoth963(ctx, c, cs, reachDisk{a: &valNode{pt: pt}, radiusM: radiusM*1.01 + 2*res}, sleep)
	if err != nil {
		return reachRadius963Report{}, err
	}
	freq, _ := c.link.frequency(nil)
	k, _ := c.link.kFactor(nil)
	e0, n0 := dhm.ToUTM32(pt.Lat, pt.Lon)
	rr := reachLosses963(tg, sg, e0, n0, txH, rxH, freq, k, radiusM, res, bearings)
	return reachRadius963Report{Lat: pt.Lat, Lon: pt.Lon, TxHeightM: txH, RxHeightM: rxH, RadiusKm: radiusM / 1000, ResolutionM: c.set.resolutionM,
		Bearings: bearings, BudgetsDb: reachRadius963Budgets, TxClass: clutterNames963[clutterAt963(tg, sg, e0, n0, res)],
		Rows: reachRadiusRows963(rr, res)}, nil
}

func (r reachRadius963Report) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Reach radius (#963 M0)\n\nPoint %.4f, %.4f (clutter class %s), transmitter %.0f m, receiver %.0f m above the ground, %d bearings to %.0f km on a %d m grid. Terrain: %s. Reached: margin ≥ 3 dB.\n\n",
		r.Lat, r.Lon, r.TxClass, r.TxHeightM, r.RxHeightM, r.Bearings, r.RadiusKm, r.ResolutionM, r.Terrain)
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
