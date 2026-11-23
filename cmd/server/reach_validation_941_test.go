package main

// #941 M2 — the predicted-reach validation harness (M0 §3.4), test-only:
// it ships in no binary. It reads a database copy read-only (OpenDB,
// mode=ro), scores the predicted path loss of candidate node pairs against
// the links the mesh has actually observed, and writes a JSON and a
// markdown report per model (free space, Bullington, delta-Bullington)
// and distance band.
//
// Run against a real instance's database as docs/predicted-reach-validation.md
// describes (CORESCOPE_REACH_VALIDATION_DB etc.); without those variables
// TestReachValidation941_RealDB skips. In CI the mechanics are proven on
// test-fixtures/e2e-fixture.db, whose Californian positions are moved
// rigidly to Jutland, over a synthetic terrain from a fake
// ElevationSource (TestReachValidation941_FixtureMechanics).
//
// What is scored:
//   - Nodes: repeaters and rooms (the reach's own roles, owner decision 5)
//     with a valid position from their adverts, seen within the window
//     before the newest node in the database, not hidden by the operator's
//     blacklists or hidden-name prefixes, through any of its names (the
//     serve path's alias-aware rule: hiddenIdentityNames, then
//     identityHidden). Antenna heights are the reach's
//     (coverageNodeHeight: operator per node, approved owner height, role).
//   - Candidate pairs: two such nodes at most the radius apart.
//   - Positives: an observer_neighbors row (the firmware's own zero-hop
//     neighbour table, in either direction), or a neighbor_edges row with
//     both ends resolved to full keys of candidate nodes and seen at least
//     the minimum count (default the neighbour graph's own
//     affinityMinObservations, 3). An edge with an unresolved (prefix or
//     empty) end is ambiguous and dropped; a resolved edge seen fewer
//     times is weak and dropped from both sides.
//   - Weak negatives: every other candidate pair. Not having seen a link
//     is not proof there is none, so the false-reach rate is an upper
//     bound.
//   - Each pair's loss is computed with the same code as nodes in reach
//     (dhm.ReachLosses over a disk grid around one end, gathered through
//     the reach's own tile cache and upstream budget), for every model.
//
// The margin of a pair is the operator's link budget (config radio) minus
// the loss; "reached" is a margin of at least the marginal class (3 dB).
// Metrics: hit rate (positives reached), false-reach rate (negatives
// reached), ROC-AUC of the margin, and against the median SNR of
// observer_neighbor_metrics the Spearman ρ and the median offset between
// observed and predicted SNR (predicted: received power minus the
// observer's noise floor, or minus sensitivity + 10 dB, SF8's SNR limit,
// when the observer reports none). The median offset is the calibration
// constant M2 is to fit once the report has run on Danish links.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
	"github.com/meshcore-analyzer/dhm"
)

const (
	// reachValidationNoiseOffsetDb is the SF8 demodulation limit: without a
	// reported noise floor the noise is sensitivity + 10 dB (M0 §3.5, A).
	reachValidationNoiseOffsetDb = 10
	// reachValidationReferences is how many reference repeaters the report
	// details: the observers with the most firmware neighbours.
	reachValidationReferences = 5
)

// reachValidationModels are the scored models, baseline first.
var reachValidationModels = []dhm.PathModel{dhm.PathFreeSpace, dhm.PathBullington, dhm.PathDelta}

// reachValidationBands are the distance bands, in km: [lo, hi).
var reachValidationBands = []struct {
	name   string
	lo, hi float64
}{{"0-5 km", 0, 5}, {"5-15 km", 5, 15}, {"15-30 km", 15, 30}, {"30+ km", 30, math.Inf(1)}}

var updateReachGolden = flag.Bool("update-941-golden", false, "rewrite testdata/reach_validation_941_shape.json")

// ── Input ──────────────────────────────────────────────────────────────────

type valNode struct {
	pk, name, role string
	pt             dhm.LatLon
	heightM        float64
	heightSource   string
}

type valPair struct {
	a, b      *valNode // a.pk < b.pk; a is the transmitter of the evaluation
	positive  bool
	firmware  bool // from observer_neighbors
	edge      bool // from a confident neighbor_edges row
	snr       *float64
	noiseDbm  *float64 // the reporting observer's noise floor
	distanceM float64
	loss      map[dhm.PathModel]float64
}

// reachValidationInput is what the harness read from the database.
type reachValidationInput struct {
	asOf          time.Time
	nodes         map[string]*valNode
	hidden        int
	firmware      map[[2]string]bool // unordered pair keys
	firmwareCount map[string]int     // per observer
	edges         map[[2]string]bool
	ambiguous     int
	weak          map[[2]string]bool
	snr           map[[2]string]float64 // median per unordered pair
	snrNoise      map[[2]string]float64 // the reporting observer's noise floor
	noise         map[string]float64
}

func pairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// loadReachValidationInput reads the nodes and the observed links from a
// read-only connection. Missing optional tables (an older schema) read as
// empty.
func loadReachValidationInput(ctx context.Context, srv *Server, c *terrainCoverage, conn *sql.DB, window time.Duration, minEdgeCount int) (*reachValidationInput, error) {
	in := &reachValidationInput{nodes: map[string]*valNode{}, firmware: map[[2]string]bool{}, firmwareCount: map[string]int{},
		edges: map[[2]string]bool{}, weak: map[[2]string]bool{}, snr: map[[2]string]float64{}, snrNoise: map[[2]string]float64{},
		noise: map[string]float64{}}
	var newest sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT MAX(last_seen) FROM nodes`).Scan(&newest); err != nil {
		return nil, err
	}
	in.asOf = parseNodeTimestamp(newest.String)
	rows, err := conn.QueryContext(ctx, `SELECT public_key, COALESCE(name, ''), COALESCE(role, ''), lat, lon, COALESCE(last_seen, '') FROM nodes`)
	if err != nil {
		return nil, err
	}
	var active []*valNode
	for rows.Next() {
		var pk, name, role, seen string
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&pk, &name, &role, &lat, &lon, &seen); err != nil {
			rows.Close()
			return nil, err
		}
		pk = strings.ToLower(pk)
		if !coverageReachRole(role) || !isHexPubkey(pk) || !lat.Valid || !lon.Valid || !isValidNodePosition(lat.Float64, lon.Float64) {
			continue
		}
		if t := parseNodeTimestamp(seen); t.IsZero() || t.Before(in.asOf.Add(-window)) {
			continue
		}
		active = append(active, &valNode{pk: pk, name: name, role: role, pt: dhm.LatLon{Lat: lat.Float64, Lon: lon.Float64}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The serve path's visibility rule (coverageReachView): one bulk,
	// alias-aware lookup (observer and inactive names), then identityHidden.
	pks := make([]string, len(active))
	for i, n := range active {
		pks[i] = n.pk
	}
	aliases, err := srv.hiddenIdentityNames(ctx, pks)
	if err != nil {
		return nil, err
	}
	for _, n := range active {
		if identityHidden(srv.cfg, n.pk, append([]string{n.name}, aliases[n.pk]...)...) {
			in.hidden++
			continue
		}
		n.heightM, n.heightSource = srv.coverageNodeHeight(ctx, c, n.pk, n.role, n.pt)
		in.nodes[n.pk] = n
	}

	if err := optionalQuery(ctx, conn, `SELECT id, noise_floor FROM observers WHERE noise_floor IS NOT NULL`, func(r *sql.Rows) error {
		var id string
		var nf float64
		if err := r.Scan(&id, &nf); err != nil {
			return err
		}
		if nf < -60 && nf > -160 { // a plausible floor in dBm
			in.noise[strings.ToLower(id)] = nf
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := optionalQuery(ctx, conn, `SELECT observer_id, neighbor_pubkey FROM observer_neighbors`, func(r *sql.Rows) error {
		var o, n string
		if err := r.Scan(&o, &n); err != nil {
			return err
		}
		o, n = strings.ToLower(o), strings.ToLower(n)
		if o != n && in.nodes[o] != nil && in.nodes[n] != nil {
			k := pairKey(o, n)
			if !in.firmware[k] {
				in.firmware[k] = true
			}
			in.firmwareCount[o]++
		}
		return nil
	}); err != nil {
		return nil, err
	}
	snrs := map[[2]string][]float64{}
	reporter := map[[2]string]string{}
	if err := optionalQuery(ctx, conn, `SELECT observer_id, neighbor_pubkey, snr FROM observer_neighbor_metrics WHERE snr IS NOT NULL`, func(r *sql.Rows) error {
		var o, n string
		var snr float64
		if err := r.Scan(&o, &n, &snr); err != nil {
			return err
		}
		o, n = strings.ToLower(o), strings.ToLower(n)
		if in.nodes[o] != nil && in.nodes[n] != nil && o != n && !math.IsNaN(snr) {
			k := pairKey(o, n)
			snrs[k] = append(snrs[k], snr)
			reporter[k] = o
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for k, v := range snrs {
		if m := medianOf(v); m != nil {
			in.snr[k] = *m
			if nf, ok := in.noise[reporter[k]]; ok {
				in.snrNoise[k] = nf
			}
		}
	}
	if err := optionalQuery(ctx, conn, `SELECT node_a, node_b, count FROM neighbor_edges`, func(r *sql.Rows) error {
		var a, b string
		var n int
		if err := r.Scan(&a, &b, &n); err != nil {
			return err
		}
		a, b = strings.ToLower(a), strings.ToLower(b)
		if !isHexPubkey(a) || !isHexPubkey(b) || a == b {
			in.ambiguous++
			return nil
		}
		if in.nodes[a] == nil || in.nodes[b] == nil {
			return nil // not a candidate: hidden, inactive, unpositioned or a companion
		}
		if n < minEdgeCount {
			in.weak[pairKey(a, b)] = true
			return nil
		}
		in.edges[pairKey(a, b)] = true
		return nil
	}); err != nil {
		return nil, err
	}
	return in, nil
}

// optionalQuery runs a query and hands each row to scan; a missing table
// is no rows.
func optionalQuery(ctx context.Context, conn *sql.DB, q string, scan func(*sql.Rows) error) error {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") || strings.Contains(err.Error(), "no such column") {
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// candidatePairs is every pair of nodes at most radiusM apart, labelled;
// weak edges are left out of both sides.
func (in *reachValidationInput) candidatePairs(radiusM float64) (pairs []*valPair, beyond, weak int) {
	keys := make([]string, 0, len(in.nodes))
	for k := range in.nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, ka := range keys {
		a := in.nodes[ka]
		for _, kb := range keys[i+1:] {
			b := in.nodes[kb]
			k := [2]string{ka, kb}
			d := dhm.GroundDistance(a.pt, b.pt)
			if d > radiusM {
				if in.firmware[k] || in.edges[k] {
					beyond++
				}
				continue
			}
			if in.weak[k] && !in.firmware[k] {
				weak++
				continue
			}
			p := &valPair{a: a, b: b, firmware: in.firmware[k], edge: in.edges[k], distanceM: d}
			p.positive = p.firmware || p.edge
			if v, ok := in.snr[k]; ok {
				v := v
				p.snr = &v
				if nf, ok := in.snrNoise[k]; ok {
					nf := nf
					p.noiseDbm = &nf
				}
			}
			pairs = append(pairs, p)
		}
	}
	return pairs, beyond, weak
}

// ── Evaluation ─────────────────────────────────────────────────────────────

// evaluatePairs computes every pair's loss under every model: one disk
// grid per transmitting end (its farthest partner's distance), gathered
// through the reach's tile cache and upstream budget, waiting when the
// budget is spent. Ends are visited in tile order so neighbouring disks
// reuse cached tiles.
func evaluatePairs(ctx context.Context, c *terrainCoverage, pairs []*valPair, sleep func(time.Duration)) (tiles int, partial bool, err error) {
	byA := map[string][]*valPair{}
	for _, p := range pairs {
		byA[p.a.pk] = append(byA[p.a.pk], p)
	}
	type origin struct {
		pk   string
		x, y int
	}
	span := float64(dhm.CoverageTilePx) * float64(c.set.resolutionM)
	var order []origin
	for pk, ps := range byA {
		e, n := dhm.ToUTM32(ps[0].a.pt.Lat, ps[0].a.pt.Lon)
		order = append(order, origin{pk, int(math.Floor(e / span)), int(math.Floor(n / span))})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].y != order[j].y {
			return order[i].y < order[j].y
		}
		if order[i].x != order[j].x {
			return order[i].x < order[j].x
		}
		return order[i].pk < order[j].pk
	})
	res := float64(c.set.resolutionM)
	freq, _ := c.link.frequency(nil)
	k, _ := c.link.kFactor(nil)
	for _, o := range order {
		ps := byA[o.pk]
		a := ps[0].a
		r := 0.0
		for _, p := range ps {
			r = math.Max(r, p.distanceM)
		}
		// The projection's scale: a little room past the farthest partner.
		r = r*1.01 + 2*res
		e, n := dhm.ToUTM32(a.pt.Lat, a.pt.Lon)
		plan := dhm.PlanCoverageTiles(e, n, r, res)
		grid := dhm.NewCoverageGrid(e, n, r, res)
		for {
			_, part, gerr := c.gather(ctx, plan, grid)
			var budget errCoverageBudget
			if errors.As(gerr, &budget) {
				sleep(max(time.Second, budget.retry))
				continue
			}
			if gerr != nil {
				return tiles, partial, gerr
			}
			partial = partial || part
			break
		}
		tiles += len(plan)
		targets := make([]dhm.ReachTarget, len(ps))
		for i, p := range ps {
			targets[i] = dhm.ReachTarget{Lat: p.b.pt.Lat, Lon: p.b.pt.Lon, HeightM: p.b.heightM}
		}
		for _, p := range ps {
			p.loss = map[dhm.PathModel]float64{}
		}
		for _, m := range reachValidationModels {
			spec := dhm.CoverageSpec{Center: a.pt, RadiusM: r, ResolutionM: res, TxHeightM: a.heightM,
				FreqMHz: freq, K: k, Model: m}
			losses, err := dhm.ReachLosses(ctx, grid, spec, targets)
			if err != nil {
				return tiles, partial, err
			}
			for i, l := range losses {
				ps[i].loss[m] = l.LossDb // NaN without terrain; Inside by construction
			}
		}
	}
	return tiles, partial, nil
}

// ── Report ─────────────────────────────────────────────────────────────────

type reachValidationReport struct {
	Version    int                        `json:"version"`
	Issue      string                     `json:"issue"`
	DataAsOf   string                     `json:"dataAsOf"`
	Params     reachValidationParams      `json:"params"`
	Terrain    reachValidationTerrain     `json:"terrain"`
	Pairs      reachValidationPairCounts  `json:"pairs"`
	Models     []reachValidationModel     `json:"models"`
	References []reachValidationReference `json:"references"`
	Notes      []string                   `json:"notes"`
}

type reachValidationParams struct {
	RadiusKm         float64 `json:"radiusKm"`
	ResolutionM      int     `json:"resolutionM"`
	FreqMHz          float64 `json:"freqMHz"`
	K                float64 `json:"k"`
	BudgetDb         float64 `json:"budgetDb"`
	ReachedMarginDb  float64 `json:"reachedMarginDb"`
	WindowDays       float64 `json:"windowDays"`
	MinEdgeCount     int     `json:"minEdgeCount"`
	NoiseFallbackDbm float64 `json:"noiseFallbackDbm"`
	TxPowerDbm       float64 `json:"txPowerDbm"`
	GainsDbi         float64 `json:"gainsDbi"`
	LossDb           float64 `json:"cableLossDb"`
}

type reachValidationTerrain struct {
	Source   string `json:"source"` // what supplied the elevation
	Tiles    int    `json:"tilesGathered"`
	Upstream int64  `json:"upstreamCalls"` // -1: not counted
	Partial  bool   `json:"partial"`
}

type reachValidationPairCounts struct {
	Nodes             int `json:"nodes"`
	HiddenNodes       int `json:"hiddenNodes"`
	Candidates        int `json:"candidates"`
	Scored            int `json:"scored"`
	Positives         int `json:"positives"`
	FromFirmware      int `json:"fromObserverNeighbors"`
	FromEdges         int `json:"fromNeighborEdges"`
	Negatives         int `json:"negatives"`
	AmbiguousEdges    int `json:"droppedAmbiguousEdges"`
	WeakEdges         int `json:"droppedWeakEdges"`
	PositivesBeyond   int `json:"positivesBeyondRadius"`
	NoTerrain         int `json:"noTerrain"`
	WithSNR           int `json:"withSNR"`
	WithReportedNoise int `json:"withReportedNoise"`
}

type reachValidationMetrics struct {
	Positives         int      `json:"positives"`
	Negatives         int      `json:"negatives"`
	HitRate           *float64 `json:"hitRate"`
	FalseReachRate    *float64 `json:"falseReachRate"`
	AUC               *float64 `json:"auc"`
	SNRPairs          int      `json:"snrPairs"`
	Spearman          *float64 `json:"spearman"`
	MedianSNROffsetDb *float64 `json:"medianSnrOffsetDb"`
}

type reachValidationBand struct {
	Band string `json:"band"`
	reachValidationMetrics
}

type reachValidationModel struct {
	Model   string                 `json:"model"`
	ID      string                 `json:"id"`
	Overall reachValidationMetrics `json:"overall"`
	Bands   []reachValidationBand  `json:"bands"`
}

type reachValidationReference struct {
	PubKey    string                        `json:"pubkey"`
	Name      string                        `json:"name"`
	Neighbors int                           `json:"firmwareNeighbors"`
	Models    []reachValidationReferenceRow `json:"models"`
}

type reachValidationReferenceRow struct {
	Model     string   `json:"model"`
	Pairs     int      `json:"pairs"`
	Positives int      `json:"positives"`
	HitRate   *float64 `json:"hitRate"`
	AUC       *float64 `json:"auc"`
}

func round4(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	r := math.Round(v*1e4) / 1e4
	return &r
}

// medianOf is the median, nil for none.
func medianOf(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return &m
}

// ranks are 1-based ranks, ties averaged.
func ranks(v []float64) []float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })
	out := make([]float64, len(v))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		r := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = r
		}
		i = j + 1
	}
	return out
}

// rocAUC is the probability a positive scores above a negative, ties half
// (Mann–Whitney); NaN without both classes.
func rocAUC(pos, neg []float64) float64 {
	if len(pos) == 0 || len(neg) == 0 {
		return math.NaN()
	}
	all := append(append([]float64(nil), pos...), neg...)
	r := ranks(all)
	sum := 0.0
	for i := range pos {
		sum += r[i]
	}
	np, nn := float64(len(pos)), float64(len(neg))
	return (sum - np*(np+1)/2) / (np * nn)
}

// spearman is the rank correlation; NaN under three pairs or without
// spread.
func spearman(x, y []float64) float64 {
	if len(x) < 3 || len(x) != len(y) {
		return math.NaN()
	}
	rx, ry := ranks(x), ranks(y)
	var mx, my float64
	for i := range rx {
		mx += rx[i]
		my += ry[i]
	}
	mx /= float64(len(rx))
	my /= float64(len(ry))
	var sxy, sxx, syy float64
	for i := range rx {
		dx, dy := rx[i]-mx, ry[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return math.NaN()
	}
	return sxy / math.Sqrt(sxx*syy)
}

func rateOf(k, n int) *float64 {
	if n == 0 {
		return nil
	}
	return round4(float64(k) / float64(n))
}

// scoreMetrics scores the pairs under one model.
func scoreMetrics(ps []*valPair, m dhm.PathModel, radio CoverageRadio) reachValidationMetrics {
	budget := radio.budgetDb()
	reached := coverageClasses[1].minDb
	var out reachValidationMetrics
	var pos, neg, snrObs, snrPred, offs []float64
	hits, falses := 0, 0
	for _, p := range ps {
		margin := budget - p.loss[m]
		if p.positive {
			out.Positives++
			pos = append(pos, margin)
			if margin >= reached {
				hits++
			}
		} else {
			out.Negatives++
			neg = append(neg, margin)
			if margin >= reached {
				falses++
			}
		}
		if p.snr != nil && p.positive {
			noise := radio.SensitivityDbm + reachValidationNoiseOffsetDb
			if p.noiseDbm != nil {
				noise = *p.noiseDbm
			}
			pred := radio.TxPowerDbm + radio.AntennaGainDbi + radio.RxGainDbi - radio.LossDb - p.loss[m] - noise
			snrObs = append(snrObs, *p.snr)
			snrPred = append(snrPred, pred)
			offs = append(offs, *p.snr-pred)
		}
	}
	out.HitRate, out.FalseReachRate = rateOf(hits, out.Positives), rateOf(falses, out.Negatives)
	out.AUC = round4(rocAUC(pos, neg))
	out.SNRPairs = len(snrObs)
	out.Spearman = round4(spearman(snrPred, snrObs))
	if med := medianOf(offs); med != nil {
		out.MedianSNROffsetDb = round4(*med)
	}
	return out
}

// buildReachValidationReport scores the evaluated pairs.
func buildReachValidationReport(in *reachValidationInput, c *terrainCoverage, all []*valPair, o reachValidationOptions, beyond, weak int) reachValidationReport {
	radiusM, window := o.radiusM, o.window
	freq, _ := c.link.frequency(nil)
	k, _ := c.link.kFactor(nil)
	rep := reachValidationReport{Version: 1, Issue: "941", DataAsOf: in.asOf.UTC().Format(time.RFC3339),
		Params: reachValidationParams{RadiusKm: radiusM / 1000, ResolutionM: c.set.resolutionM, FreqMHz: freq, K: k,
			BudgetDb: c.radio.budgetDb(), ReachedMarginDb: coverageClasses[1].minDb, WindowDays: window.Hours() / 24,
			MinEdgeCount: o.minEdgeCount, NoiseFallbackDbm: c.radio.SensitivityDbm + reachValidationNoiseOffsetDb,
			TxPowerDbm: c.radio.TxPowerDbm, GainsDbi: c.radio.AntennaGainDbi + c.radio.RxGainDbi, LossDb: c.radio.LossDb},
		Models: []reachValidationModel{}, References: []reachValidationReference{}}
	rep.Pairs = reachValidationPairCounts{Nodes: len(in.nodes), HiddenNodes: in.hidden, Candidates: len(all),
		AmbiguousEdges: in.ambiguous, WeakEdges: weak, PositivesBeyond: beyond}
	var ps []*valPair
	for _, p := range all {
		ok := true
		for _, m := range reachValidationModels {
			if math.IsNaN(p.loss[m]) {
				ok = false
			}
		}
		if !ok {
			rep.Pairs.NoTerrain++
			continue
		}
		ps = append(ps, p)
		if p.positive {
			rep.Pairs.Positives++
			if p.firmware {
				rep.Pairs.FromFirmware++
			}
			if p.edge {
				rep.Pairs.FromEdges++
			}
			if p.snr != nil {
				rep.Pairs.WithSNR++
				if p.noiseDbm != nil {
					rep.Pairs.WithReportedNoise++
				}
			}
		} else {
			rep.Pairs.Negatives++
		}
	}
	rep.Pairs.Scored = len(ps)
	for _, m := range reachValidationModels {
		mr := reachValidationModel{Model: string(m), ID: m.ID(), Overall: scoreMetrics(ps, m, c.radio)}
		for _, b := range reachValidationBands {
			var in []*valPair
			for _, p := range ps {
				if km := p.distanceM / 1000; km >= b.lo && km < b.hi {
					in = append(in, p)
				}
			}
			mr.Bands = append(mr.Bands, reachValidationBand{Band: b.name, reachValidationMetrics: scoreMetrics(in, m, c.radio)})
		}
		rep.Models = append(rep.Models, mr)
	}
	// Reference repeaters: the observers with the most firmware neighbours
	// among the scored nodes (owner decision 6).
	type ref struct {
		pk string
		n  int
	}
	var refs []ref
	for pk, n := range in.firmwareCount {
		if node := in.nodes[pk]; node != nil && node.role == "repeater" {
			refs = append(refs, ref{pk, n})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].n != refs[j].n {
			return refs[i].n > refs[j].n
		}
		return refs[i].pk < refs[j].pk
	})
	if len(refs) > reachValidationReferences {
		refs = refs[:reachValidationReferences]
	}
	for _, r := range refs {
		var mine []*valPair
		for _, p := range ps {
			if p.a.pk == r.pk || p.b.pk == r.pk {
				mine = append(mine, p)
			}
		}
		rr := reachValidationReference{PubKey: r.pk, Name: in.nodes[r.pk].name, Neighbors: r.n}
		for _, m := range reachValidationModels {
			s := scoreMetrics(mine, m, c.radio)
			rr.Models = append(rr.Models, reachValidationReferenceRow{Model: string(m), Pairs: len(mine), Positives: s.Positives, HitRate: s.HitRate, AUC: s.AUC})
		}
		rep.References = append(rep.References, rr)
	}
	rep.Notes = []string{
		"Negatives are pairs never observed while both nodes were active: weak evidence, so the false-reach rate is an upper bound.",
		"Reached: a margin of at least reachedMarginDb at the operator's link budget (config radio).",
		"Predicted SNR: received power minus the reporting observer's noise floor, or minus noiseFallbackDbm when it reports none; the median offset (observed minus predicted) is the calibration constant to fit.",
	}
	return rep
}

func fmtPtr(v *float64, pct bool) string {
	if v == nil {
		return "–"
	}
	if pct {
		return strconv.FormatFloat(100**v, 'f', 1, 64) + " %"
	}
	return strconv.FormatFloat(*v, 'f', 3, 64)
}

// markdown renders the report for a PR or an issue comment.
func (r reachValidationReport) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Predicted reach validation (#%s)\n\n", r.Issue)
	fmt.Fprintf(&b, "Data as of %s. Radius %.0f km, %d m grid, %.3f MHz, k %.2f, link budget %.0f dB (reached: margin ≥ %.0f dB), window %.0f days. Terrain: %s, %d tiles gathered, %d upstream calls%s.\n\n",
		r.DataAsOf, r.Params.RadiusKm, r.Params.ResolutionM, r.Params.FreqMHz, r.Params.K, r.Params.BudgetDb, r.Params.ReachedMarginDb,
		r.Params.WindowDays, r.Terrain.Source, r.Terrain.Tiles, r.Terrain.Upstream, map[bool]string{true: " (partial)", false: ""}[r.Terrain.Partial])
	b.WriteString("## Pairs\n\n| Nodes | Hidden | Candidates | Scored | Positives | from observer_neighbors | from neighbor_edges | Negatives | Ambiguous edges | Weak edges | Positives beyond radius | No terrain | With SNR |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	p := r.Pairs
	fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d |\n\n", p.Nodes, p.HiddenNodes, p.Candidates, p.Scored,
		p.Positives, p.FromFirmware, p.FromEdges, p.Negatives, p.AmbiguousEdges, p.WeakEdges, p.PositivesBeyond, p.NoTerrain, p.WithSNR)
	row := func(label string, m reachValidationMetrics) string {
		return fmt.Sprintf("| %s | %d | %d | %s | %s | %s | %d | %s | %s |\n", label, m.Positives, m.Negatives, fmtPtr(m.HitRate, true),
			fmtPtr(m.FalseReachRate, true), fmtPtr(m.AUC, false), m.SNRPairs, fmtPtr(m.Spearman, false), fmtPtr(m.MedianSNROffsetDb, false))
	}
	head := "| %s | Positives | Negatives | Hit rate | False-reach rate | ROC-AUC | SNR pairs | Spearman ρ | Median SNR offset (dB) |\n|---|---|---|---|---|---|---|---|---|\n"
	b.WriteString("## Per model\n\n")
	fmt.Fprintf(&b, head, "Model")
	for _, m := range r.Models {
		b.WriteString(row(m.Model, m.Overall))
	}
	b.WriteString("\n## Per distance band\n\n")
	fmt.Fprintf(&b, head, "Model, band")
	for _, m := range r.Models {
		for _, band := range m.Bands {
			b.WriteString(row(m.Model+", "+band.Band, band.reachValidationMetrics))
		}
	}
	b.WriteString("\n## Reference repeaters\n\nThe observers with the most firmware neighbours.\n\n| Repeater | Firmware neighbours | Model | Pairs | Positives | Hit rate | ROC-AUC |\n|---|---|---|---|---|---|---|\n")
	for _, ref := range r.References {
		for _, m := range ref.Models {
			fmt.Fprintf(&b, "| %s (%s…) | %d | %s | %d | %d | %s | %s |\n", strings.ReplaceAll(ref.Name, "|", "/"), ref.PubKey[:8], ref.Neighbors, m.Model, m.Pairs, m.Positives,
				fmtPtr(m.HitRate, true), fmtPtr(m.AUC, false))
		}
	}
	b.WriteString("\n## Notes\n\n")
	for _, n := range r.Notes {
		b.WriteString("- " + n + "\n")
	}
	return b.String()
}

// reachValidationOptions are one run's settings.
type reachValidationOptions struct {
	radiusM       float64
	window        time.Duration // before the newest node in the database
	minEdgeCount  int           // a confident neighbor_edges row
	terrainSource string        // for the report
	upstream      func() int64  // upstream calls so far; -1 when not counted
	outDir        string        // "" writes nothing
	sleep         func(time.Duration)
}

func defaultReachValidationOptions(radiusM float64) reachValidationOptions {
	return reachValidationOptions{radiusM: radiusM, window: 7 * 24 * time.Hour, minEdgeCount: affinityMinObservations,
		upstream: func() int64 { return -1 }, sleep: time.Sleep}
}

// reachValidationLabels are the harness's input and labelled candidate
// pairs, before any terrain is read.
type reachValidationLabels struct {
	in           *reachValidationInput
	pairs        []*valPair
	beyond, weak int
}

// labelReachValidation reads the database and labels the candidate pairs:
// everything of a run but the terrain.
func labelReachValidation(ctx context.Context, srv *Server, c *terrainCoverage, conn *sql.DB, o reachValidationOptions) (reachValidationLabels, error) {
	in, err := loadReachValidationInput(ctx, srv, c, conn, o.window, o.minEdgeCount)
	if err != nil {
		return reachValidationLabels{}, err
	}
	pairs, beyond, weak := in.candidatePairs(o.radiusM)
	return reachValidationLabels{in: in, pairs: pairs, beyond: beyond, weak: weak}, nil
}

// report scores the labelled pairs as they are (losses evaluated or not).
func (l reachValidationLabels) report(c *terrainCoverage, o reachValidationOptions) reachValidationReport {
	return buildReachValidationReport(l.in, c, l.pairs, o, l.beyond, l.weak)
}

// runReachValidation is the harness: read, pair, evaluate, score, write.
func runReachValidation(ctx context.Context, srv *Server, c *terrainCoverage, conn *sql.DB, o reachValidationOptions) (reachValidationReport, error) {
	l, err := labelReachValidation(ctx, srv, c, conn, o)
	if err != nil {
		return reachValidationReport{}, err
	}
	tiles, partial, err := evaluatePairs(ctx, c, l.pairs, o.sleep)
	if err != nil {
		return reachValidationReport{}, err
	}
	rep := l.report(c, o)
	rep.Terrain = reachValidationTerrain{Source: o.terrainSource, Tiles: tiles, Upstream: o.upstream(), Partial: partial}
	outDir := o.outDir
	if outDir != "" {
		js, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(filepath.Join(outDir, "reach-validation-941.json"), append(js, '\n'), 0o644); err != nil {
			return rep, err
		}
		if err := os.WriteFile(filepath.Join(outDir, "reach-validation-941.md"), []byte(rep.markdown()), 0o644); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// ── Real run ───────────────────────────────────────────────────────────────

// TestReachValidation941_RealDB runs the harness against an instance's
// database copy; see docs/predicted-reach-validation.md. It skips unless
// CORESCOPE_REACH_VALIDATION_DB is set.
func TestReachValidation941_RealDB(t *testing.T) {
	dbPath := os.Getenv("CORESCOPE_REACH_VALIDATION_DB")
	if dbPath == "" {
		t.Skip("CORESCOPE_REACH_VALIDATION_DB not set: the real-data run is manual (docs/predicted-reach-validation.md)")
	}
	cfgDir := os.Getenv("CORESCOPE_REACH_VALIDATION_CONFIG")
	outDir := os.Getenv("CORESCOPE_REACH_VALIDATION_OUT")
	if cfgDir == "" || outDir == "" {
		t.Fatal("set CORESCOPE_REACH_VALIDATION_CONFIG (the directory of config.json) and CORESCOPE_REACH_VALIDATION_OUT")
	}
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	// The harness's own reach: on, with room for every tile of the run.
	cfg.Coverage.Enabled = true
	if cfg.Coverage.ElevationCacheMB == nil {
		cfg.Coverage.ElevationCacheMB = intp(1024)
	}
	db, err := OpenDB(dbPath) // mode=ro
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	proxy := newDHMProxyFromConfig(cfg)
	terrain := newTerrainProfilerFromConfig(cfg, proxy, db)
	c := newTerrainCoverageFromConfig(cfg, proxy, terrain, db)
	if c == nil {
		t.Fatal("no terrain: the config needs map.dhm with a token, or map.dhm.profile.foreignFill.enabled")
	}
	srv := &Server{cfg: cfg, db: db}
	srv.claims = newAntennaClaimService(cfg, db.conn, "") // reads approved heights; no queue
	o := defaultReachValidationOptions(c.set.maxRadiusM)
	if v, err := strconv.ParseFloat(os.Getenv("CORESCOPE_REACH_VALIDATION_RADIUS_KM"), 64); err == nil && v > 0 {
		o.radiusM = math.Min(v*1000, c.set.maxRadiusM)
	}
	if v, err := strconv.ParseFloat(os.Getenv("CORESCOPE_REACH_VALIDATION_WINDOW_DAYS"), 64); err == nil && v > 0 {
		o.window = time.Duration(v * 24 * float64(time.Hour))
	}
	if v, err := strconv.Atoi(os.Getenv("CORESCOPE_REACH_VALIDATION_MIN_EDGE_COUNT")); err == nil && v > 0 {
		o.minEdgeCount = v
	}
	o.terrainSource = map[[2]bool]string{{true, false}: "dhm", {true, true}: "dhm+glo30", {false, true}: "glo30"}[[2]bool{c.dhm != nil, c.fill != nil}]
	o.outDir = outDir
	rep, err := runReachValidation(context.Background(), srv, c, db.conn, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("report in %s: %d scored pairs (%d positives)", outDir, rep.Pairs.Scored, rep.Pairs.Positives)
}

// ── Mechanics on the fixture ───────────────────────────────────────────────

// reachFixtureOrigin is where the fixture's network is moved: inland
// Jutland.
var reachFixtureOrigin = dhm.LatLon{Lat: 56.15, Lon: 9.55}

// reachFullFixtureEnv opts in to the full fixture run.
const reachFullFixtureEnv = "CORESCOPE_REACH_VALIDATION_FULL_FIXTURE"

// reachFixtureScale is the size of the fixture's terrain run. CI runs the
// small one: every stage, model and band through the same code, on the
// nodes within 25 km of the network's centre, a 10 km radius and a 60 m
// grid, a few seconds under -race. With
// CORESCOPE_REACH_VALIDATION_FULL_FIXTURE=1 it is the full one: every node,
// 40 km on the 30 m grid, seconds without -race and minutes under it, so CI
// never sets it (docs/predicted-reach-validation.md).
type reachFixtureScale struct {
	full        bool
	radiusM     float64
	resolutionM int
	keepM       float64 // the nodes kept: this far from the network's centre
	seeds       int     // firmware links seeded within the radius
	minNodes    int     // candidate nodes the run must have
}

func reachFixtureScaleFromEnv() reachFixtureScale {
	if os.Getenv(reachFullFixtureEnv) == "1" {
		return reachFixtureScale{full: true, radiusM: 40000, resolutionM: 30, keepM: math.Inf(1), seeds: 12, minNodes: 100}
	}
	return reachFixtureScale{radiusM: 10000, resolutionM: 60, keepM: 25000, seeds: 8, minNodes: 50}
}

// prepareReachFixture copies e2e-fixture.db, brings it to the current
// schema (dbschema.Apply, as CI prepares it), moves its nodes rigidly to
// Jutland (equirectangular about the network's centre, so pair distances
// keep within a fraction of a percent), keeps the positioned nodes within
// sc.keepM of its centre and, since the fixture has no firmware neighbour
// reports, seeds observer_neighbors and their SNR history from its own
// resolved neighbor_edges: the first sc.seeds of them within sc.radiusM,
// in key order. Test setup only: the harness opens the copy read-only.
func prepareReachFixture(t *testing.T, sc reachFixtureScale) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	src, err := os.Open(filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst.Close()
	rw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	var lat0, lon0 float64
	if err := rw.QueryRow(`SELECT AVG(lat), AVG(lon) FROM nodes WHERE lat IS NOT NULL AND lat != 0`).Scan(&lat0, &lon0); err != nil {
		t.Fatal(err)
	}
	c0, c1 := math.Cos(lat0*math.Pi/180), math.Cos(reachFixtureOrigin.Lat*math.Pi/180)
	if _, err := rw.Exec(`UPDATE nodes SET lat = ? + (lat - ?), lon = ? + (lon - ?) * ? / ? WHERE lat IS NOT NULL AND lat != 0`,
		reachFixtureOrigin.Lat, lat0, reachFixtureOrigin.Lon, lon0, c0, c1); err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(sc.keepM, 1) {
		rows, err := rw.Query(`SELECT public_key, lat, lon FROM nodes WHERE lat IS NOT NULL AND lat != 0`)
		if err != nil {
			t.Fatal(err)
		}
		var far []string
		for rows.Next() {
			var pk string
			var lat, lon float64
			if err := rows.Scan(&pk, &lat, &lon); err != nil {
				t.Fatal(err)
			}
			if dhm.GroundDistance(reachFixtureOrigin, dhm.LatLon{Lat: lat, Lon: lon}) > sc.keepM {
				far = append(far, pk)
			}
		}
		rows.Close()
		for _, pk := range far {
			if _, err := rw.Exec(`DELETE FROM nodes WHERE public_key = ?`, pk); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := rw.Query(`SELECT e.node_a, e.node_b, a.lat, a.lon, b.lat, b.lon FROM neighbor_edges e
		JOIN nodes a ON a.public_key = e.node_a JOIN nodes b ON b.public_key = e.node_b
		WHERE a.lat != 0 AND b.lat != 0 AND a.role IN ('repeater', 'room') AND b.role IN ('repeater', 'room')
		ORDER BY e.node_a, e.node_b`)
	if err != nil {
		t.Fatal(err)
	}
	type edge struct {
		a, b string
		d    float64
	}
	var es []edge
	for rows.Next() {
		var e edge
		var la, oa, lb, ob float64
		if err := rows.Scan(&e.a, &e.b, &la, &oa, &lb, &ob); err != nil {
			t.Fatal(err)
		}
		e.d = dhm.GroundDistance(dhm.LatLon{Lat: la, Lon: oa}, dhm.LatLon{Lat: lb, Lon: ob})
		es = append(es, e)
	}
	rows.Close()
	n := 0
	for _, e := range es {
		if n == sc.seeds {
			break
		}
		if e.d > sc.radiusM {
			continue
		}
		// Observer ids arrive upper-case from MQTT topics.
		if _, err := rw.Exec(`INSERT OR IGNORE INTO observer_neighbors (observer_id, neighbor_pubkey, status, reported_at) VALUES (?, ?, 'responded', '2026-05-17T18:00:00Z')`,
			strings.ToUpper(e.a), e.b); err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 3; k++ {
			snr := 12 - e.d/2500 + float64(k) // falls with distance
			if _, err := rw.Exec(`INSERT INTO observer_neighbor_metrics (observer_id, neighbor_pubkey, timestamp, snr, heard_secs_ago) VALUES (?, ?, ?, ?, 60)`,
				strings.ToUpper(e.a), e.b, fmt.Sprintf("2026-05-17T1%d:00:00Z", k), snr); err != nil {
				t.Fatal(err)
			}
		}
		n++
	}
	if n < sc.seeds {
		t.Fatalf("only %d fixture edges to seed from", n)
	}
	// The first seeded observer reports its noise floor.
	if _, err := rw.Exec(`INSERT INTO observers (id, name, noise_floor) VALUES ((SELECT MIN(observer_id) FROM observer_neighbors), 'seeded observer', -112)
		ON CONFLICT(id) DO UPDATE SET noise_floor = -112`); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// rollingElev is a synthetic Danish-like terrain anywhere: rolling hills
// of 0-90 m, a function of the coordinates alone.
func rollingElev(e, n float64) float32 {
	x, y := e/1000, n/1000
	return float32(math.Max(0, 35+20*math.Sin(x/4.7)*math.Cos(y/6.1)+12*math.Sin(x/1.3+y/2.2)+5*math.Cos(x/0.41-y/0.67)))
}

// reachValidationCoverage is the harness's reach on a fake
// ElevationSource, everywhere covered, on a grid of resolutionM.
func reachValidationCoverage(t *testing.T, resolutionM int) (*terrainCoverage, *fakeElevation) {
	t.Helper()
	src := newFakeElevation(t, rollingElev)
	c := newTerrainCoverage(CoverageConfig{Enabled: true, ElevationCacheMB: intp(512), ResolutionM: intp(resolutionM)}, nil,
		terrainCoverageDeps{src: src, logf: func(string, ...interface{}) {}, observers: func() ([]Observer, error) { return nil, nil }})
	c.covers = func(e, n float64) bool { return true }
	return c, src
}

// reachValidationEnv is the harness on a prepared fixture copy.
func reachValidationEnv(t *testing.T, path string, resolutionM int) (*Server, *terrainCoverage, *DB, *fakeElevation) {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &Config{}
	c, src := reachValidationCoverage(t, resolutionM)
	srv := &Server{cfg: cfg, db: db}
	srv.claims = newAntennaClaimService(cfg, db.conn, "")
	return srv, c, db, src
}

// fixtureValidationOptions: the scale's radius, a week, and a minimum edge
// count of one, since every resolved edge in the fixture was seen once.
func fixtureValidationOptions(t *testing.T, src *fakeElevation, radiusM float64) reachValidationOptions {
	o := defaultReachValidationOptions(radiusM)
	o.minEdgeCount = 1
	o.terrainSource = "fake (synthetic terrain)"
	o.upstream = func() int64 { return src.calls.Load() }
	o.sleep = func(time.Duration) { t.Fatal("the upstream budget ran out") }
	return o
}

// jsonShape is a report's structure: object keys with their shapes, an
// array's first element, the type of a scalar.
func jsonShape(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, e := range x {
			out[k] = jsonShape(e)
		}
		return out
	case []interface{}:
		if len(x) == 0 {
			return []interface{}{}
		}
		return []interface{}{jsonShape(x[0])}
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// TestReachValidation941_FixtureMechanics runs the whole harness once on
// the fixture and asserts on that one report: it reads the copy read-only
// (the file is unchanged), labels positives from observer_neighbors and
// resolved neighbor_edges and drops the ambiguous ones, scores every model
// and band, names the reference repeaters, fetches each elevation tile
// once, and writes a JSON report of the golden shape and its markdown. CI
// runs it at the small scale (reachFixtureScale); the bands beyond the
// radius are then empty, and every band holds pairs only at the full one.
func TestReachValidation941_FixtureMechanics(t *testing.T) {
	sc := reachFixtureScaleFromEnv()
	path := prepareReachFixture(t, sc)
	before := fileSHA(t, path)
	srv, c, db, src := reachValidationEnv(t, path, sc.resolutionM)
	out := t.TempDir()
	if keep := os.Getenv("CORESCOPE_REACH_VALIDATION_FIXTURE_OUT"); keep != "" {
		out = keep // to look at the fixture's report
	}
	o := fixtureValidationOptions(t, src, sc.radiusM)
	o.outDir = out
	rep, err := runReachValidation(context.Background(), srv, c, db.conn, o)
	if err != nil {
		t.Fatal(err)
	}
	if fileSHA(t, path) != before {
		t.Fatal("the harness changed the database copy")
	}
	if _, err := db.conn.Exec(`DELETE FROM nodes`); err == nil {
		t.Fatal("the harness's connection can write")
	}

	p := rep.Pairs
	t.Logf("scale %+v; pairs %+v; terrain %+v", sc, p, rep.Terrain)
	if rep.Params.RadiusKm != sc.radiusM/1000 || rep.Params.ResolutionM != sc.resolutionM {
		t.Fatalf("params %+v", rep.Params)
	}
	if p.Nodes < sc.minNodes || p.Positives < sc.seeds || p.FromFirmware < sc.seeds || p.FromEdges < sc.seeds || p.Negatives < 100 || p.AmbiguousEdges < 50 ||
		p.WithSNR < sc.seeds || p.WithReportedNoise < 1 || p.Scored+p.NoTerrain != p.Candidates || p.Positives+p.Negatives != p.Scored {
		t.Fatalf("pair counts %+v", p)
	}
	if p.PositivesBeyond == 0 {
		t.Fatal("the fixture's implausible long edges are not reported beyond the radius")
	}
	// Each tile fetched once, however many disks share it.
	if rep.Terrain.Upstream <= 0 || rep.Terrain.Upstream > int64(rep.Terrain.Tiles) || rep.Terrain.Upstream > 400 {
		t.Fatalf("terrain %+v", rep.Terrain)
	}
	if len(rep.Models) != 3 || rep.Models[0].Model != "fspl" || rep.Models[1].Model != "bullington" || rep.Models[2].Model != "delta" {
		t.Fatalf("models %+v", rep.Models)
	}
	for _, m := range rep.Models {
		o := m.Overall
		if o.Positives != p.Positives || o.Negatives != p.Negatives || o.AUC == nil || *o.AUC < 0 || *o.AUC > 1 ||
			o.HitRate == nil || o.FalseReachRate == nil || o.SNRPairs != p.WithSNR || o.Spearman == nil || o.MedianSNROffsetDb == nil {
			t.Fatalf("%s overall %+v", m.Model, o)
		}
		sumP, sumN := 0, 0
		for i, b := range m.Bands {
			if b.Band != reachValidationBands[i].name {
				t.Fatalf("band %d %q", i, b.Band)
			}
			sumP += b.Positives
			sumN += b.Negatives
			// A band that starts at or past the radius holds nothing; at
			// the full scale every band holds pairs (the network spans
			// about 80 km).
			switch {
			case reachValidationBands[i].lo*1000 >= sc.radiusM && b.Positives+b.Negatives > 0:
				t.Fatalf("%s: band %s past the radius holds %+v", m.Model, b.Band, b)
			case sc.full && b.Negatives == 0:
				t.Fatalf("%s: band %s is empty", m.Model, b.Band)
			}
		}
		if sumP != o.Positives || sumN != o.Negatives {
			t.Fatalf("%s: bands hold %d/%d of %d/%d", m.Model, sumP, sumN, o.Positives, o.Negatives)
		}
		// The seeded firmware links are short: the short band holds the
		// most positives.
		for _, b := range m.Bands[1:] {
			if b.Positives >= m.Bands[0].Positives || m.Bands[0].Negatives == 0 {
				t.Fatalf("%s: bands %+v", m.Model, m.Bands)
			}
		}
		if sc.full && m.Bands[0].Negatives >= m.Bands[2].Negatives {
			t.Fatalf("%s: bands %+v", m.Model, m.Bands)
		}
	}
	// Diffraction only ever adds loss: free space reaches at least as many
	// pairs as Bullington, and Bullington as many as delta.
	fr := func(i int) float64 { return *rep.Models[i].Overall.FalseReachRate + *rep.Models[i].Overall.HitRate }
	if !(fr(0) >= fr(1) && fr(1) >= fr(2)) {
		t.Fatalf("reach does not shrink with the models: %v %v %v", fr(0), fr(1), fr(2))
	}
	if len(rep.References) == 0 || rep.References[0].Neighbors < 1 || len(rep.References[0].Models) != 3 {
		t.Fatalf("references %+v", rep.References)
	}

	js, err := os.ReadFile(filepath.Join(out, "reach-validation-941.json"))
	if err != nil {
		t.Fatal(err)
	}
	var generic interface{}
	if err := json.Unmarshal(js, &generic); err != nil {
		t.Fatal(err)
	}
	shape, _ := json.MarshalIndent(jsonShape(generic), "", "  ")
	golden := filepath.Join("testdata", "reach_validation_941_shape.json")
	if *updateReachGolden {
		if err := os.WriteFile(golden, append(shape, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(shape)) {
		t.Fatalf("report shape differs from %s (rerun with -update-941-golden after a deliberate change):\n%s", golden, shape)
	}
	md, err := os.ReadFile(filepath.Join(out, "reach-validation-941.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"# Predicted reach validation (#941)", "## Pairs", "## Per model", "## Per distance band", "## Reference repeaters", "| delta, 30+ km |", "| fspl |"} {
		if !strings.Contains(string(md), h) {
			t.Fatalf("markdown lacks %q:\n%s", h, md)
		}
	}
}

// TestReachValidation941_HiddenAndInactiveNodesAreLeftOut: a node hidden
// by the operator's config is not scored and never named in the report,
// whether it is hidden by the node blacklist or only through an observer
// alias with a hidden-name prefix (the serve path's alias-aware rule); a
// node not seen within the window is not a candidate, and the default
// minimum edge count finds no confident edge in the fixture. The labels
// are all these depend on, so the report is scored without terrain.
func TestReachValidation941_HiddenAndInactiveNodesAreLeftOut(t *testing.T) {
	sc := reachFixtureScaleFromEnv()
	path := prepareReachFixture(t, sc)
	srv, c, db, src := reachValidationEnv(t, path, sc.resolutionM)
	o := fixtureValidationOptions(t, src, sc.radiusM)
	ctx := context.Background()
	labels := func(srv *Server, o reachValidationOptions) reachValidationReport {
		t.Helper()
		l, err := labelReachValidation(ctx, srv, c, db.conn, o)
		if err != nil {
			t.Fatal(err)
		}
		return l.report(c, o)
	}
	full := labels(srv, o)
	if len(full.References) < 2 {
		t.Fatalf("references %+v", full.References)
	}
	// The top reference repeater is hidden only through its observer row's
	// name; another firmware observer by the node blacklist.
	alias := full.References[0]
	var blk, blkName string
	if err := db.conn.QueryRow(`SELECT lower(o.observer_id), n.name FROM observer_neighbors o JOIN nodes n ON n.public_key = lower(o.observer_id)
		WHERE lower(o.observer_id) != ? ORDER BY o.observer_id LIMIT 1`, alias.PubKey).Scan(&blk, &blkName); err != nil {
		t.Fatal(err)
	}
	rw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`INSERT INTO observers (id, name) VALUES (?, 'private-relay') ON CONFLICT(id) DO UPDATE SET name = 'private-relay'`,
		strings.ToUpper(alias.PubKey)); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	if got := labels(srv, o); got.Pairs.HiddenNodes != 0 || got.References[0].PubKey != alias.PubKey {
		t.Fatalf("an alias hides nothing without a hidden prefix: %+v", got.Pairs)
	}
	srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	srv.cfg.SetNodeBlacklist([]string{blk})
	rep := labels(srv, o)
	if rep.Pairs.HiddenNodes != 2 || rep.Pairs.Nodes != full.Pairs.Nodes-2 || rep.Pairs.Candidates >= full.Pairs.Candidates {
		t.Fatalf("hidden: %+v vs %+v", rep.Pairs, full.Pairs)
	}
	js, _ := json.Marshal(rep)
	low := strings.ToLower(string(js) + rep.markdown())
	for _, h := range []struct{ pk, name string }{{alias.PubKey, alias.Name}, {blk, blkName}} {
		if strings.Contains(low, h.pk[:8]) || (h.name != "" && strings.Contains(low, strings.ToLower(h.name))) || strings.Contains(low, "private-relay") {
			t.Fatalf("the hidden node %s (%q) is in the report:\n%s", h.pk[:8], h.name, rep.markdown())
		}
	}
	// A one-minute window before the newest node keeps almost nobody.
	o.window = time.Minute
	short := labels(&Server{cfg: &Config{}, db: db, claims: srv.claims}, o)
	if short.Pairs.Nodes >= full.Pairs.Nodes/2 {
		t.Fatalf("window: %d nodes of %d", short.Pairs.Nodes, full.Pairs.Nodes)
	}
	// The default minimum count (the neighbour graph's 3) finds no
	// confident edge in the fixture: every resolved one is weak.
	o.window = 7 * 24 * time.Hour
	o.minEdgeCount = affinityMinObservations
	def := labels(&Server{cfg: &Config{}, db: db, claims: srv.claims}, o)
	if def.Pairs.FromEdges != 0 || def.Pairs.WeakEdges == 0 || def.Pairs.FromFirmware != full.Pairs.FromFirmware || def.Params.MinEdgeCount != 3 {
		t.Fatalf("default edge count: %+v", def.Pairs)
	}
	if src.calls.Load() != 0 {
		t.Fatal("labelling read terrain")
	}
}

// TestReachValidation941_ReferencesAndBands on hand-made pairs: the
// reference set is the repeaters with the most firmware neighbours, never
// a room however many it has (owner decision 6), and a pair belongs to the
// band [lo, hi) of its distance: a band's lower edge is in it, its upper
// edge in the next.
func TestReachValidation941_ReferencesAndBands(t *testing.T) {
	c, _ := reachValidationCoverage(t, 60)
	in := &reachValidationInput{nodes: map[string]*valNode{}, firmwareCount: map[string]int{}}
	add := func(k byte, role string, neighbours int) *valNode {
		n := &valNode{pk: reachPK(k), name: "Node " + string(k), role: role}
		in.nodes[n.pk] = n
		in.firmwareCount[n.pk] = neighbours
		return n
	}
	room := add('a', "room", 9)
	var reps []*valNode
	for i, k := range []byte("123456") {
		reps = append(reps, add(k, "repeater", 6-i))
	}
	loss := map[dhm.PathModel]float64{dhm.PathFreeSpace: 100, dhm.PathBullington: 110, dhm.PathDelta: 120}
	var pairs []*valPair
	for i, d := range []float64{0, 4999.9, 5000, 14999.9, 15000, 29999.9, 30000, 80000} {
		pairs = append(pairs, &valPair{a: reps[i%len(reps)], b: room, positive: i%2 == 0, distanceM: d, loss: loss})
	}
	rep := buildReachValidationReport(in, c, pairs, defaultReachValidationOptions(40000), 0, 0)
	var refs []string
	for _, r := range rep.References {
		refs = append(refs, r.PubKey[:1])
	}
	if strings.Join(refs, "") != "12345" {
		t.Fatalf("references %v", refs)
	}
	for _, m := range rep.Models {
		for i, b := range m.Bands {
			// Two pairs per band: the lower edge (or 0) and one just
			// below the upper edge; each band one positive.
			if b.Positives != 1 || b.Negatives != 1 {
				t.Fatalf("%s: band %s holds %d/%d, want 1/1 (%v)", m.Model, b.Band, b.Positives, b.Negatives, reachValidationBands[i])
			}
		}
	}
}

// TestReachValidation941_Metrics pins the scoring maths on hand-made pairs.
func TestReachValidation941_Metrics(t *testing.T) {
	if v := rocAUC([]float64{3, 4, 5}, []float64{0, 1, 2}); v != 1 {
		t.Fatalf("separated AUC %v", v)
	}
	if v := rocAUC([]float64{0, 1, 2}, []float64{3, 4, 5}); v != 0 {
		t.Fatalf("inverted AUC %v", v)
	}
	if v := rocAUC([]float64{1, 1}, []float64{1, 1}); v != 0.5 {
		t.Fatalf("tied AUC %v", v)
	}
	if v := rocAUC([]float64{2, 4}, []float64{1, 3}); v != 0.75 {
		t.Fatalf("AUC %v", v)
	}
	if !math.IsNaN(rocAUC(nil, []float64{1})) {
		t.Fatal("AUC without positives")
	}
	if v := spearman([]float64{1, 2, 3, 4}, []float64{10, 20, 30, 40}); math.Abs(v-1) > 1e-12 {
		t.Fatalf("ρ %v", v)
	}
	if v := spearman([]float64{1, 2, 3, 4}, []float64{4, 3, 2, 1}); math.Abs(v+1) > 1e-12 {
		t.Fatalf("ρ %v", v)
	}
	if v := spearman([]float64{1, 2, 3, 4, 5}, []float64{2, 1, 4, 3, 5}); math.Abs(v-0.8) > 1e-12 {
		t.Fatalf("ρ %v", v)
	}
	if !math.IsNaN(spearman([]float64{1, 2}, []float64{1, 2})) || !math.IsNaN(spearman([]float64{1, 1, 1}, []float64{1, 2, 3})) {
		t.Fatal("ρ defined without enough data")
	}
	if m := medianOf([]float64{5, 1, 3}); *m != 3 {
		t.Fatalf("median %v", *m)
	}
	if m := medianOf([]float64{4, 1, 3, 2}); *m != 2.5 {
		t.Fatalf("median %v", *m)
	}
	if medianOf(nil) != nil {
		t.Fatal("median of nothing")
	}
	if r := ranks([]float64{10, 20, 10, 30}); !reflect.DeepEqual(r, []float64{1.5, 3, 1.5, 4}) {
		t.Fatalf("ranks %v", r)
	}

	// Scoring: budget 158 dB, reached at a 3 dB margin.
	radio := CoverageRadio{TxPowerDbm: 22, AntennaGainDbi: 3, RxGainDbi: 3, SensitivityDbm: -130}
	mk := func(pos bool, loss float64) *valPair {
		return &valPair{positive: pos, loss: map[dhm.PathModel]float64{dhm.PathBullington: loss}}
	}
	snr := func(v float64) *float64 { return &v }
	ps := []*valPair{mk(true, 150), mk(true, 156), mk(true, 140), mk(false, 160), mk(false, 154)}
	ps[0].snr, ps[2].snr = snr(-2), snr(5)
	ps[0].noiseDbm = snr(-115)
	m := scoreMetrics(ps, dhm.PathBullington, radio)
	// Margins: 8, 2, 18 (positives); -2, 4 (negatives).
	if m.Positives != 3 || m.Negatives != 2 || *m.HitRate != 0.6667 || *m.FalseReachRate != 0.5 {
		t.Fatalf("metrics %+v", m)
	}
	// Five of the six positive-negative pairs are in order.
	if *m.AUC != 0.8333 {
		t.Fatalf("AUC %v", *m.AUC)
	}
	// Predicted SNR: 28 − 150 − (−115) = −7 (reported floor), 28 − 140 −
	// (−120) = 8 (fallback); offsets 5 and −3, median 1.
	if m.SNRPairs != 2 || *m.MedianSNROffsetDb != 1 || m.Spearman != nil {
		t.Fatalf("SNR %+v", m)
	}
}
