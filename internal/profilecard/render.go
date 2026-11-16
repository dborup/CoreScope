// Package profilecard draws the 1200×630 link-preview card for a shared
// terrain profile (#727 M2): the profile chart, the verdict in the panel's
// own words, both endpoint names and the DHM credit.
//
// Render is a pure function of its arguments: no I/O, no HTTP, no package
// state, and the same bytes for the same input on every architecture (all
// drawing is integer coverage arithmetic, see raster.go and text.go).
// Fetching, caching, budgets and HTTP are the caller's (cmd/server, M3).
package profilecard

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"strings"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/cardkit"
	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
)

// Card geometry. Nothing that carries meaning sits within safeMargin px of
// an edge: Facebook and LinkedIn crop a few pixels off a 1.91:1 preview.
const (
	Width, Height = 1200, 630
	safeMargin    = 24

	titleSize       = 40
	minTitleSize    = 28
	subtitleSize    = 22
	badgeSize       = 24
	chipSize        = 16
	axisSize        = 16
	pinSize         = 18
	summarySize     = 20
	attributionSize = 18
	messageSize     = 22

	textX           = 44 // title and subtitle, right of the accent bar
	headerRule      = 120
	plotX, plotW    = 96, 1080 // 1080 columns: the renderer's unit of work
	plotY, plotH    = 144, 352
	axisBaseline    = 524
	footerRule      = 540
	summaryBaseline = 574
	creditBaseline  = 604

	// A card with more than one credit line (the Copernicus fill, #738;
	// the building footprints, #846) stacks them up from creditBaseline,
	// creditLeading apart at the same 18 px size, and the summary sits
	// one leading above the first.
	creditLeading = 20
)

// Options are the operator's display choices. The zero value is the
// owner's default (#727): names shown, no host line.
type Options struct {
	// HideNames draws "Node A"/"Node B" instead of node names.
	HideNames bool
	// Site is the host shown after the credit, e.g. "meshview.dk".
	Site string
	// Pending draws the card for a profile whose elevations are not
	// available yet (the server's degraded card, #727 M3): endpoints,
	// distance and heights, no verdict, and pendingMessage in the plot.
	// resp's samples and verdict are ignored.
	Pending bool
}

// pendingMessage is the plot text of a Pending card.
const pendingMessage = "The terrain profile is not ready yet. Open the link to measure it."

// textRun is one string the card drew, kept so tests can assert words
// rather than pixels. Y is the baseline; W the drawn width.
type textRun struct {
	Role, Text string
	Size       int
	Bold       bool
	X, Y, W    int
}

// Credit is the DHM credit every card draws: the derived credit (#733),
// because a card is computed from DHM data rather than shown as served,
// and it is the credit of the profile it pictures.
const Credit = dhm.AttributionDerived

// FillCredit is the Copernicus DEM credit a mixed card (resp.Coverage.Mixed,
// #738) draws under Credit: the profile UI's short line (#779's
// copdem.Credit), built from the copdem parts master has, without its
// trailing "(Copernicus DEM licence)" mark. That mark is a link in the UI;
// a picture cannot link, and with it the line does not fit the card at the
// credit size. The long Art. 6(b) notice (copdem.AttributionDerived) stays
// one step away, on the page the card links to.
const FillCredit = "Outside Denmark: produced using " + copdem.DatasetName +
	", provided under COPERNICUS by the European Union and ESA"

// buildingsCredited reports whether the card credits the INSPIRE building
// footprints (#846): the profile's surface was classified with them, full
// or partial, and the response carries the panel's credit for it, which
// cmd/server sets exactly when a sample was classified (terrainClasses
// used). The panel shows that same line on the same condition (#797).
func buildingsCredited(resp TerrainProfileResponse) bool {
	c := resp.Classification
	return c != nil && (c.State == "full" || c.State == "partial") && resp.Attribution == buildings.AttributionDerived
}

// creditLines is the DHM credit as the card draws it: Credit, or for a
// classified profile the panel's buildings.AttributionDerived. That line
// is too wide for the card at the credit size and is never shortened, so
// it wraps, whole, where the footprints' part begins, after the DHM
// dataset (#779 keeps the fill credit whole on a line of its own too).
func creditLines(resp TerrainProfileResponse) []string {
	if !buildingsCredited(resp) {
		return []string{Credit}
	}
	head, tail, ok := strings.Cut(buildings.AttributionDerived, dhm.DatasetName)
	if !ok {
		return []string{buildings.AttributionDerived}
	}
	return []string{head + dhm.DatasetName, strings.TrimSpace(tail)}
}

// Render draws the card for resp and returns it as a PNG. names are the
// display names of endpoints A and B ("" when unknown); the caller is
// responsible for only passing names of nodes the profile API would
// resolve (hidden identities never reach here). The DHM credit is always
// drawn, whatever resp.Attribution says.
func Render(resp TerrainProfileResponse, names [2]string, pal Palette, opt Options) ([]byte, error) {
	img, _, err := render(resp, names, pal, opt)
	if err != nil {
		return nil, err
	}
	return cardkit.EncodePNG(img)
}

type renderer struct {
	cv   cardkit.Canvas
	fs   *cardkit.Fonts
	pal  Palette
	runs []textRun
}

func render(resp TerrainProfileResponse, names [2]string, pal Palette, opt Options) (*image.RGBA, []textRun, error) {
	fs, err := cardkit.LoadFonts()
	if err != nil {
		return nil, nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Rect, image.NewUniform(cardkit.Opaque(pal.Background)), image.Point{}, draw.Src)
	r := &renderer{cv: cardkit.Canvas{Img: img}, fs: fs, pal: pal}

	if opt.Pending {
		resp = pendingResponse(resp)
	}
	main := verdictChip(resp)
	surf, showSurf := surfaceChip(resp)
	r.header(resp, names, opt, main, surf, showSurf)
	g, drawn := geometry(resp) // a pending response has no samples: not drawn
	if drawn {
		r.plot(resp, g)
	} else {
		r.emptyPlot(main)
	}
	r.footer(resp, main, opt, drawn)
	return img, r.runs, nil
}

// AltText is the card's text alternative (og:image:alt): the Go twin of
// public/terrain-profile.js chartTitle, with the card's own endpoint labels
// and chips, so it says what the picture shows.
func AltText(resp TerrainProfileResponse, names [2]string, opt Options) string {
	fs, err := cardkit.LoadFonts()
	if err != nil {
		return ""
	}
	if opt.Pending {
		resp = pendingResponse(resp)
	}
	main := verdictChip(resp)
	a := endpointLabel(fs, resp.A, names[0], 0, opt.HideNames)
	b := endpointLabel(fs, resp.B, names[1], 1, opt.HideNames)
	txt := "Terrain profile from " + a + " to " + b + ", " + fmtDistance(resp.DistanceM) + ". " + main.label + ". " + main.detail
	wc, okC := value(resp.Verdict.WorstClearanceM)
	at, okA := value(resp.Verdict.WorstAtM)
	// The verdict's worst point is the smallest Fresnel ratio, which need
	// not be the least clearance on the path (#839): named for what it is.
	if main.state != "unknown" && okC && okA {
		txt += " Tightest point for the Fresnel zone: clearance " + fmtMetres(wc) + " at " + fmtDistance(at) + " along the path."
	}
	if surf, show := surfaceChip(resp); show {
		txt += " " + surf.label + "."
	}
	return txt
}

// AltCredit is the sentence og:image:alt ends with when the card credits
// the building footprints (#846), "" otherwise: their dataset, source and
// licence, from internal/buildings' own parts. It is apart from AltText so
// the caller can clip AltText and keep the credit whole.
func AltCredit(resp TerrainProfileResponse, opt Options) string {
	if opt.Pending || !buildingsCredited(resp) {
		return ""
	}
	return "Buildings told apart from trees with " + buildings.DatasetName + " (" + buildings.Source + "), " + dhm.LicenseName + "."
}

// pendingResponse is resp without anything measured: no samples, no
// verdict, no ground heights. What is left (endpoints, distance, radio,
// antenna heights) needs no elevation.
func pendingResponse(resp TerrainProfileResponse) TerrainProfileResponse {
	resp.Samples = nil
	resp.Verdict = TerrainVerdict{State: "unknown", Model: resp.Verdict.Model, Criterion: resp.Verdict.Criterion}
	resp.Coverage = TerrainCoverage{State: "pending"}
	resp.A.GroundM, resp.B.GroundM = nil, nil
	resp.Classification = nil
	return resp
}

// text draws s and records it. It returns the drawn width.
func (r *renderer) text(role, s string, x, y, size int, bold bool, c color.NRGBA) int {
	if s == "" {
		return 0
	}
	w := r.fs.Draw(&r.cv, s, x, y, size, bold, c)
	r.runs = append(r.runs, textRun{Role: role, Text: s, Size: size, Bold: bold, X: x, Y: y, W: w})
	return w
}

// ── Verdict words ────────────────────────────────────────────────────────

// verdictWord is the Go twin of VERDICTS in public/terrain-profile.js: a
// shared card must use the same words as the panel it links to.
type verdictWord struct{ label, tone, detail string }

func verdictWords(state string) (verdictWord, bool) {
	switch state {
	case "clear":
		return verdictWord{"Clear", "ok", "The whole first Fresnel zone is clear of terrain."}, true
	case "fresnel100":
		return verdictWord{"Usable", "ok", "The first Fresnel zone is partly obstructed, but at least 60 % of it stays clear."}, true
	case "fresnel60":
		return verdictWord{"Marginal", "warn", "Line of sight is clear, but less than 60 % of the first Fresnel zone is."}, true
	case "blocked":
		return verdictWord{"Blocked", "bad", "Terrain rises through the line of sight."}, true
	}
	return verdictWord{}, false
}

// surfaceRank orders the states worst first; "unknown" has no rank.
func surfaceRank(state string) (int, bool) {
	switch state {
	case "blocked":
		return 0, true
	case "fresnel60":
		return 1, true
	case "fresnel100":
		return 2, true
	case "clear":
		return 3, true
	}
	return 0, false
}

type chip struct{ state, label, tone, detail string }

// Verdict is the main (terrain) verdict as the card states it: Word is the
// panel's word ("Clear", "Usable", "Marginal", "Blocked", or "No verdict"
// when Known is false) and Detail its sentence. The badge shows Word,
// prefixed "Terrain: " when the surface model is drawn too; the server
// puts the same Word in og:title, so a preview never disagrees with its
// card.
type Verdict struct {
	Word, Detail string
	Known        bool
}

// MainVerdict is the verdict the card's badge shows for resp.
func MainVerdict(resp TerrainProfileResponse) Verdict {
	c := mainChip(resp)
	return Verdict{Word: c.word, Detail: c.detail, Known: c.state != "unknown"}
}

// covered mirrors the panel: a path with a hole in it was never measured
// end to end, so no verdict on it is shown, whatever the response says.
func covered(resp TerrainProfileResponse) bool {
	return resp.Coverage.State == "" || resp.Coverage.State == "full"
}

func verdictChip(resp TerrainProfileResponse) chip {
	c := mainChip(resp)
	label := c.word
	if c.state != "unknown" && resp.Model == "both" {
		label = "Terrain: " + label
	}
	return chip{c.state, label, c.tone, c.detail}
}

// mainChipWords is the main verdict before the badge's model prefix.
type mainChipWords struct{ state, word, tone, detail string }

func mainChip(resp TerrainProfileResponse) mainChipWords {
	if w, ok := verdictWords(resp.Verdict.State); ok && covered(resp) {
		return mainChipWords{resp.Verdict.State, w.label, w.tone, w.detail}
	}
	detail := "No elevation data for this path, so there is no clearance verdict."
	switch resp.Coverage.State {
	case "partial":
		detail = "No verdict, path leaves Denmark. The profile covers " + fmtNum(jsRound(resp.Coverage.Pct, 1)) + " % of the path."
	case "none":
		// #839: a cross-border path gets the Copernicus fill (#738); a
		// path with no DHM sample gets no data at all.
		detail = "This path lies entirely outside Denmark, so there is no terrain data for it."
	case "pending":
		detail = pendingMessage
	}
	return mainChipWords{"unknown", "No verdict", "unknown", detail}
}

// surfaceChip is the second chip, shown only when buildings and trees are
// strictly worse than the terrain (#729), or always when an antenna is
// below the surface (#745, antennaBelowLabel). It never softens the
// terrain verdict and always has the warn tone.
func surfaceChip(resp TerrainProfileResponse) (chip, bool) {
	s := resp.Verdict.Surface
	if resp.Model != "both" || s == nil || !covered(resp) {
		return chip{}, false
	}
	if s.State == "blocked" {
		if label, ok := antennaBelowLabel(s); ok {
			return chip{"blocked", label, "warn", ""}, true
		}
	}
	w, ok := verdictWords(s.State)
	sr, ok1 := surfaceRank(s.State)
	tr, ok2 := surfaceRank(verdictChip(resp).state)
	if !ok || !ok1 || !ok2 || sr >= tr {
		return chip{}, false
	}
	return chip{s.State, "Buildings/trees: " + w.label, "warn", ""}, true
}

// antennaBelowLabel is the panel's antennaBelowChip label: a surface above
// the antenna at an endpoint is far more often a roof-top node with a
// default antenna height than a building in the link, so the chip says
// what to check instead of "Blocked".
func antennaBelowLabel(s *TerrainSurfaceInfo) (string, bool) {
	if s.Reason != "antennaBelowSurface" {
		return "", false
	}
	at := map[string]string{"A": "A", "B": "B", "both": "A and B"}[s.Endpoint]
	if at == "" {
		return "", false
	}
	return "Buildings/trees: antenna below the surface at " + at + " — check the antenna height", true
}

func (r *renderer) toneColor(tone string) color.NRGBA {
	switch tone {
	case "ok":
		return r.pal.StatusGreen
	case "warn":
		return r.pal.StatusYellow
	case "bad":
		return r.pal.ProfileBlocked
	}
	return r.pal.TextMuted
}

// ── Header ───────────────────────────────────────────────────────────────

func (r *renderer) header(resp TerrainProfileResponse, names [2]string, opt Options, main, surf chip, showSurf bool) {
	right := Width - safeMargin
	// Verdict badge, top right.
	bw := r.fs.Measure(main.label, badgeSize, true) + 36
	badge := image.Rect(right-bw, 28, right, 72)
	pillColor := cardkit.Opaque(r.toneColor(main.tone))
	r.cv.Pill(badge, pillColor)
	r.text("badge", main.label, badge.Min.X+18, 58, badgeSize, true, cardkit.Opaque(cardkit.ReadableOn(pillColor, r.pal.Text, r.pal.Background)))
	// The title row has only the badge to its right; the surface chip sits
	// on the subtitle's row and bounds the subtitle alone (#755 review).
	titleW := badge.Min.X - safeMargin - textX
	textRight := badge.Min.X
	if showSurf {
		cw := r.fs.Measure(surf.label, chipSize, false) + 24
		c := image.Rect(right-cw, 80, right, 108)
		cc := cardkit.Opaque(r.toneColor(surf.tone))
		r.cv.Pill(c, cc)
		r.text("surface-badge", surf.label, c.Min.X+12, 100, chipSize, false, cardkit.Opaque(cardkit.ReadableOn(cc, r.pal.Text, r.pal.Background)))
		textRight = min(textRight, c.Min.X)
	}
	maxW := textRight - safeMargin - textX

	// Accent bar and title: "A → B", shrinking before it is shortened.
	r.cv.FillRect(image.Rect(safeMargin, 30, safeMargin+6, 92), cardkit.Opaque(r.pal.LosLine))
	a := endpointLabel(r.fs, resp.A, names[0], 0, opt.HideNames)
	b := endpointLabel(r.fs, resp.B, names[1], 1, opt.HideNames)
	size, title := r.fitTitle(a, b, titleW)
	r.text("title", title, textX, 70, size, true, cardkit.Opaque(r.pal.Text))

	parts := []string{"Path profile"}
	if finite(resp.DistanceM) && resp.DistanceM > 0 {
		parts = append(parts, fmtDistance(resp.DistanceM))
	}
	if f := resp.Physics.FreqMHz; finite(f) && f > 0 {
		parts = append(parts, fmtNum(jsRound(f, 3))+" MHz")
	}
	if k := resp.Physics.K; finite(k) && k > 0 {
		ks := fmtNum(jsRound(k, 2))
		if math.Abs(k-dhm.DefaultKFactor) < 1e-9 {
			ks = "4/3"
		}
		parts = append(parts, "k = "+ks)
	}
	sub := r.fs.Fit(strings.Join(parts, " · "), subtitleSize, false, maxW)
	r.text("subtitle", sub, textX, 104, subtitleSize, false, cardkit.Opaque(r.pal.TextMuted))
	r.cv.HLine(safeMargin, right, headerRule, 1, 0, cardkit.Opaque(r.pal.Border))
}

// fitTitle shrinks the title from titleSize to minTitleSize, then shortens
// the longer name(s) with "…" until "A → B" fits maxW.
func (r *renderer) fitTitle(a, b string, maxW int) (int, string) {
	const arrow = " → "
	for size := titleSize; size >= minTitleSize; size -= 4 {
		if t := a + arrow + b; r.fs.Measure(t, size, true) <= maxW {
			return size, t
		}
	}
	size := minTitleSize
	room := maxW - r.fs.Measure(arrow, size, true)
	wa, wb := r.fs.Measure(a, size, true), r.fs.Measure(b, size, true)
	switch {
	case wa <= room/2:
		b = r.fs.Fit(b, size, true, room-wa)
	case wb <= room/2:
		a = r.fs.Fit(a, size, true, room-wb)
	default:
		a, b = r.fs.Fit(a, size, true, room/2), r.fs.Fit(b, size, true, room/2)
	}
	return size, a + arrow + b
}

// ── Plot ─────────────────────────────────────────────────────────────────

// geom is the profile resampled onto the plot: one value per column for
// the measured surfaces (the highest point inside the column, so a narrow
// ridge is never lost between two columns) and one per column edge for
// the smooth lines. Everything is in metres with the earth bulge added to
// the ground, which is how the server assesses it (dhm.Link.Assess).
type geom struct {
	D                float64
	terr, surf       []float64 // per column; NaN = no data
	los, f1          []float64 // per edge (plotW+1)
	criterion        float64
	zMin, zMax, span float64
	groundA, groundB float64 // terrain at the ends, bulge-free (bulge is 0 there)
	hasSurface       bool
}

// endpointGround is the panel's endpointGround: the endpoint's own DTM
// height, else the nearest sample with one.
func endpointGround(end TerrainEndpoint, samples []TerrainSample, fromEnd bool) (float64, bool) {
	if end.GroundM != nil && finite(*end.GroundM) {
		return *end.GroundM, true
	}
	for j := range samples {
		i := j
		if fromEnd {
			i = len(samples) - 1 - j
		}
		if v := samples[i].DTM; v != nil && finite(*v) {
			return *v, true
		}
	}
	return 0, false
}

func value(p *float64) (float64, bool) {
	if p == nil || !finite(*p) {
		return 0, false
	}
	return *p, true
}

func geometry(resp TerrainProfileResponse) (geom, bool) {
	g := geom{D: resp.DistanceM, hasSurface: resp.Model == "both"}
	smp := resp.Samples
	if !finite(g.D) || g.D <= 0 || len(smp) == 0 {
		return g, false
	}
	lambda := resp.Physics.LambdaM
	if !finite(lambda) || lambda <= 0 {
		lambda = 0
	}
	k := resp.Physics.K
	if !finite(k) || k <= 0 {
		k = dhm.DefaultKFactor
	}
	g.criterion = resp.Verdict.Criterion
	if !finite(g.criterion) {
		g.criterion = dhm.Criterion
	}
	var okA, okB bool
	g.groundA, okA = endpointGround(resp.A, smp, false)
	g.groundB, okB = endpointGround(resp.B, smp, true)
	if !okA || !okB {
		return g, false
	}
	antA, antB := resp.A.AntennaM, resp.B.AntennaM
	if !finite(antA) {
		antA = 0
	}
	if !finite(antB) {
		antB = 0
	}
	zA, zB := g.groundA+antA, g.groundB+antB

	// Edges: the line of sight and the Fresnel radius are smooth functions
	// of distance, evaluated once per column edge.
	g.los = make([]float64, plotW+1)
	g.f1 = make([]float64, plotW+1)
	edgeD := func(e int) float64 { return g.D * float64(e) / plotW }
	for e := 0; e <= plotW; e++ {
		d := edgeD(e)
		g.los[e] = zA + float64((zB-zA)*(d/g.D))
		if lambda > 0 {
			g.f1[e] = dhm.FresnelRadius(lambda, d, g.D-d)
		}
	}

	// Columns: the highest measured point that falls inside, from the
	// samples in the column plus the interpolated values at its two edges.
	// One pass over the samples and one over the columns: O(samples +
	// columns), however many samples the response carries.
	g.terr = nanSlice(plotW)
	g.surf = nanSlice(plotW)
	bulge := func(d float64) float64 { return dhm.EarthBulge(d, g.D-d, k) }
	raise := func(dst []float64, c int, v float64) {
		if c >= 0 && c < plotW && finite(v) && (math.IsNaN(dst[c]) || v > dst[c]) {
			dst[c] = v
		}
	}
	for _, s := range smp {
		if !finite(s.D) || s.D < 0 || s.D > g.D {
			continue
		}
		c := min(int(s.D/g.D*plotW), plotW-1)
		b := bulge(s.D)
		if v, ok := value(s.DTM); ok {
			raise(g.terr, c, v+b)
		}
		if v, ok := value(s.DSM); ok && g.hasSurface {
			raise(g.surf, c, v+b)
		}
	}
	j := 0
	for e := 0; e <= plotW; e++ {
		d := edgeD(e)
		for j+1 < len(smp) && smp[j+1].D <= d {
			j++
		}
		if j+1 >= len(smp) || !(smp[j].D <= d && d <= smp[j+1].D) {
			continue
		}
		s0, s1 := smp[j], smp[j+1]
		t := 0.0
		if s1.D > s0.D {
			t = (d - s0.D) / (s1.D - s0.D)
		}
		b := bulge(d)
		if a, ok := value(s0.DTM); ok {
			if c, ok := value(s1.DTM); ok {
				v := a + float64((c-a)*t) + b
				raise(g.terr, e-1, v)
				raise(g.terr, e, v)
			}
		}
		if a, ok := value(s0.DSM); ok && g.hasSurface {
			if c, ok := value(s1.DSM); ok {
				v := a + float64((c-a)*t) + b
				raise(g.surf, e-1, v)
				raise(g.surf, e, v)
			}
		}
	}

	// Vertical domain, padded like the panel's chart.
	lo, hi := math.Inf(1), math.Inf(-1)
	for c := 0; c < plotW; c++ {
		for _, v := range []float64{g.terr[c], g.surf[c]} {
			if !math.IsNaN(v) {
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	for e := 0; e <= plotW; e++ {
		lo = math.Min(lo, g.los[e]-g.f1[e])
		hi = math.Max(hi, g.los[e]+g.f1[e])
	}
	pad := math.Max(1, float64((hi-lo)*0.08))
	g.zMin, g.zMax = lo-pad, hi+pad
	g.span = g.zMax - g.zMin
	if !finite(g.zMin) || !finite(g.zMax) || !finite(g.span) || g.span <= 0 {
		return g, false
	}
	return g, true
}

func nanSlice(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// y maps an elevation to a fixed-point row.
func (g geom) y(z float64) int32 {
	return cardkit.Fx(plotY + float64(plotH*(g.zMax-z))/g.span)
}

// line draws a function graph given per edge (len plotW+1) or per column
// (len plotW, drawn column centre to column centre), half px either side,
// dashed on/off columns (off = 0: solid).
func (r *renderer) line(g geom, vals []float64, half float64, on, off int, c color.NRGBA) {
	h := cardkit.Fx(half)
	n := len(vals) - 1
	for i := 0; i < n && i < plotW; i++ {
		if off > 0 && i%(on+off) >= on {
			continue
		}
		a, b := vals[i], vals[i+1]
		switch {
		case math.IsNaN(a) && math.IsNaN(b):
			continue
		case math.IsNaN(a):
			a = b
		case math.IsNaN(b):
			b = a
		}
		ya, yb := g.y(a), g.y(b)
		r.cv.VSpan(plotX+i, min(ya, yb)-h, max(ya, yb)+h, plotY, plotY+plotH, c)
	}
}

func (r *renderer) plot(resp TerrainProfileResponse, g geom) {
	base := int32(plotY+plotH) * cardkit.FracOne
	border, muted := cardkit.Opaque(r.pal.Border), cardkit.Opaque(r.pal.TextMuted)

	// Grid and elevation ticks.
	for t := 0; t <= 4; t++ {
		yy := plotY + plotH*t/4
		r.cv.HLine(plotX, plotX+plotW, yy, 2, 3, border)
		z := g.zMax - float64(float64((g.zMax-g.zMin)*float64(t))/4)
		lab := fmtNum(math.Floor(z+0.5)) + " m"
		if w := r.fs.Measure(lab, axisSize, false); w <= plotX-8-safeMargin {
			r.text("axis-y", lab, plotX-8-w, yy+6, axisSize, false, muted)
		}
	}

	// 1. DTM fill; 2. the DSM clutter band and its line.
	for c := 0; c < plotW; c++ {
		if !math.IsNaN(g.terr[c]) {
			r.cv.VSpan(plotX+c, g.y(g.terr[c]), base, plotY, plotY+plotH, r.pal.TerrainFill)
		}
	}
	// The band is drawn per class (#846) where the profile has classes,
	// in the panel's colours; a column without one keeps the clutter
	// colour, so a card without classes draws what it always drew.
	var cls []byte
	if g.hasSurface {
		cls = columnClasses(resp, g)
		for c := 0; c < plotW; c++ {
			if !math.IsNaN(g.surf[c]) && !math.IsNaN(g.terr[c]) {
				r.cv.VSpan(plotX+c, g.y(g.surf[c]), g.y(g.terr[c]), plotY, plotY+plotH, r.bandColor(cls, c))
			}
		}
		r.line(g, g.surf, 0.6, 1, 0, r.pal.TerrainClutter)
	}

	// 3/4. Fresnel zones: 60 % dashed inside the full first zone.
	lambdaOK := false
	f60top, f60bot := make([]float64, plotW+1), make([]float64, plotW+1)
	f1top, f1bot := make([]float64, plotW+1), make([]float64, plotW+1)
	for e := range g.los {
		lambdaOK = lambdaOK || g.f1[e] > 0
		cf := float64(g.criterion * g.f1[e])
		f60top[e], f60bot[e] = g.los[e]+cf, g.los[e]-cf
		f1top[e], f1bot[e] = g.los[e]+g.f1[e], g.los[e]-g.f1[e]
	}
	if lambdaOK {
		r.line(g, f60top, 0.5, 8, 6, r.pal.Fresnel60)
		r.line(g, f60bot, 0.5, 8, 6, r.pal.Fresnel60)
		r.line(g, f1top, 0.7, 1, 0, r.pal.Fresnel100)
		r.line(g, f1bot, 0.7, 1, 0, r.pal.Fresnel100)
	}
	// 5. Line of sight. Over a classified band, which is solid, it is
	// drawn over a casing in the background colour, as the panel does
	// (#797: the line alone was 1.87:1 against the class colours).
	if cls != nil {
		r.line(g, g.los, 2.1, 1, 0, cardkit.Opaque(r.pal.Background))
	}
	r.line(g, g.los, 1.1, 1, 0, r.pal.LosLine)

	// 6. Intrusions: only drawn when the verdict already asserts one, so
	// the band and the badge can never disagree (panel rule, #689).
	intruding := covered(resp) && (resp.Verdict.State == "fresnel60" || resp.Verdict.State == "blocked")
	hit := false
	if intruding {
		for c := 0; c < plotW; c++ {
			mid := float64((g.los[c] + g.los[c+1]) / 2)
			f := float64((g.f1[c] + g.f1[c+1]) / 2)
			if !math.IsNaN(g.terr[c]) && f > 0 && mid-g.terr[c] < float64(g.criterion*f) {
				r.cv.FillRect(image.Rect(plotX+c, plotY+plotH-8, plotX+c+1, plotY+plotH), cardkit.Opaque(r.pal.ProfileBlocked))
				hit = true
			}
		}
	}
	if at, ok := value(resp.Verdict.WorstAtM); ok && hit {
		x := plotX + int(math.Min(1, math.Max(0, at/g.D))*float64(plotW-1))
		r.cv.VLine(x, 1, plotY, plotY+plotH, 4, 4, cardkit.Opaque(r.pal.ProfileBlocked))
	}

	// 7. Out-of-coverage stretches: left blank, outlined, labelled once.
	for c := 0; c < plotW; {
		if !math.IsNaN(g.terr[c]) {
			c++
			continue
		}
		start := c
		for c < plotW && math.IsNaN(g.terr[c]) {
			c++
		}
		x0, x1 := plotX+start, plotX+c-1
		r.cv.HLine(x0, x1+1, plotY, 3, 4, border)
		r.cv.HLine(x0, x1+1, plotY+plotH-1, 3, 4, border)
		r.cv.VLine(x0, 1, plotY, plotY+plotH, 3, 4, border)
		r.cv.VLine(x1, 1, plotY, plotY+plotH, 3, 4, border)
		const lab = "no data"
		if w := r.fs.Measure(lab, axisSize, false); x1-x0 > w+16 {
			r.text("gap", lab, (x0+x1-w)/2, plotY+plotH-14, axisSize, false, muted)
		}
	}

	// 8. Endpoint pins: the mast from the ground to the antenna.
	pins := []struct {
		x, e   int
		ground float64
		tag    string
	}{{plotX, 0, g.groundA, "A"}, {plotX + plotW - 1, plotW, g.groundB, "B"}}
	for _, p := range pins {
		top := g.y(g.los[p.e])
		foot := g.y(p.ground)
		for dx := -1; dx <= 1; dx++ {
			r.cv.VSpan(p.x+dx, top, foot, plotY, plotY+plotH, cardkit.Opaque(r.pal.LosLine))
		}
		yTop := int(min(max(top>>cardkit.FracBits, plotY), plotY+plotH))
		r.cv.Disc(p.x, yTop, 5, cardkit.Opaque(r.pal.LosLine))
		w := r.fs.Measure(p.tag, pinSize, true)
		lx := p.x + 10
		if p.tag == "B" {
			lx = p.x - 10 - w
		}
		ly := min(max(yTop-8, plotY+pinSize), plotY+plotH-4)
		r.text("pin", p.tag, lx, ly, pinSize, true, cardkit.Opaque(r.pal.Text))
	}

	// Distance axis.
	r.text("axis-x", "0", plotX, axisBaseline, axisSize, false, muted)
	mid := fmtDistance(g.D / 2)
	if g.D >= 1000 && g.D < 2000 { // same unit as the end label
		mid = strconv.FormatFloat(jsRound(g.D/2000, 2), 'f', 2, 64) + " km"
	}
	midX := plotX + plotW/2 - r.fs.Measure(mid, axisSize, false)/2
	r.text("axis-x", mid, midX, axisBaseline, axisSize, false, muted)
	end := r.fs.Fit(fmtDistance(g.D), axisSize, false, plotW/2-40)
	r.text("axis-x", end, plotX+plotW-r.fs.Measure(end, axisSize, false), axisBaseline, axisSize, false, muted)
	if cls != nil {
		r.legend(resp, plotX+r.fs.Measure("0", axisSize, false)+legendGap, midX-legendGap)
	}
}

// The panel's words for the band's colours (public/terrain-profile.js
// CLASSES and CLASS_NONE_LEGEND; TestClassLegend pins them).
const (
	legendBuilding   = "Building"
	legendVegetation = "Trees/other"
	legendNone       = "Not told apart"
	legendSwatch     = 12 // px square, then legendGap/4 to its word
	legendGap        = 24 // px between items, and to the axis labels
)

// bandColor is the band's colour in column c: its class's, else clutter.
func (r *renderer) bandColor(cls []byte, c int) color.NRGBA {
	if cls != nil {
		switch cls[c] {
		case 'b':
			return r.pal.TerrainBuilding
		case 'v':
			return r.pal.TerrainVegetation
		}
	}
	return r.pal.TerrainClutter
}

// legend names the band's colours in the axis row, between x0 and x1:
// the panel's legend under its chart (#797), with "Not told apart" for
// the clutter colour a partial classification leaves. The row has room
// for it on every card (the middle distance label is at least 400 px
// away); were it ever too narrow, the legend is left out rather than
// drawn over a label.
func (r *renderer) legend(resp TerrainProfileResponse, x0, x1 int) {
	items := []struct {
		word string
		c    color.NRGBA
	}{{legendBuilding, r.pal.TerrainBuilding}, {legendVegetation, r.pal.TerrainVegetation}}
	if resp.Classification.State == "partial" {
		items = append(items, struct {
			word string
			c    color.NRGBA
		}{legendNone, r.pal.TerrainClutter})
	}
	w := -legendGap
	for _, it := range items {
		w += legendSwatch + legendGap/4 + r.fs.Measure(it.word, axisSize, false) + legendGap
	}
	if x0+w > x1 {
		return
	}
	x := x0
	for _, it := range items {
		r.cv.FillRect(image.Rect(x, axisBaseline-legendSwatch, x+legendSwatch, axisBaseline), it.c)
		x += legendSwatch + legendGap/4
		x += r.text("legend", it.word, x, axisBaseline, axisSize, false, cardkit.Opaque(r.pal.TextMuted)) + legendGap
	}
}

// sampleClasses is each sample's class as the band may draw it, 'b', 'v'
// or 0, or nil when it draws none: the panel's rule (chartGeometry), only
// on a full or partial classification of a fully covered model=both
// profile, and only when some sample is "b" or "v".
func sampleClasses(resp TerrainProfileResponse) []byte {
	c := resp.Classification
	if resp.Model != "both" || !covered(resp) || c == nil || (c.State != "full" && c.State != "partial") {
		return nil
	}
	out := make([]byte, len(resp.Samples))
	some := false
	for i, s := range resp.Samples {
		if s.Cls == "b" || s.Cls == "v" {
			out[i], some = s.Cls[0], true
		}
	}
	if !some {
		return nil
	}
	return out
}

// columnClasses is the class each plot column of the band is drawn in,
// or nil without classes: the Go twin of the panel's bandRuns. Where a
// class meets a sample without one, the class claims the slope between
// them; two classes meet halfway between their samples. A column takes
// the class at its centre. One pass over the samples and one over the
// columns, like geometry.
func columnClasses(resp TerrainProfileResponse, g geom) []byte {
	cls := sampleClasses(resp)
	if cls == nil {
		return nil
	}
	smp := resp.Samples
	// end is where sample i's class gives way to sample i+1's.
	end := func(i int) float64 {
		a, b := cls[i], cls[i+1]
		switch {
		case a == b || b == 0:
			return smp[i+1].D
		case a == 0:
			return smp[i].D
		}
		return (smp[i].D + smp[i+1].D) / 2
	}
	out := make([]byte, plotW)
	i := 0
	for c := range out {
		at := g.D * (float64(c) + 0.5) / plotW
		for i+1 < len(smp) && end(i) <= at {
			i++
		}
		out[c] = cls[i]
	}
	return out
}

// emptyPlot is the plot box when there is nothing to draw: the reason,
// in the panel's words, inside a dashed outline.
func (r *renderer) emptyPlot(main chip) {
	border := cardkit.Opaque(r.pal.Border)
	r.cv.HLine(plotX, plotX+plotW, plotY, 3, 4, border)
	r.cv.HLine(plotX, plotX+plotW, plotY+plotH-1, 3, 4, border)
	r.cv.VLine(plotX, 1, plotY, plotY+plotH, 3, 4, border)
	r.cv.VLine(plotX+plotW-1, 1, plotY, plotY+plotH, 3, 4, border)
	msg := main.detail
	if main.state != "unknown" {
		msg = "No elevation data for this path, so there is no clearance verdict."
	}
	msg = r.fs.Fit(msg, messageSize, false, plotW-48)
	w := r.fs.Measure(msg, messageSize, false)
	r.text("message", msg, plotX+(plotW-w)/2, plotY+plotH/2+8, messageSize, false, cardkit.Opaque(r.pal.TextMuted))
}

// ── Footer ───────────────────────────────────────────────────────────────

// footer is the summary line and the credit. The reason for a missing
// verdict goes in the summary only when the plot did not already say it.
func (r *renderer) footer(resp TerrainProfileResponse, main chip, opt Options, plotted bool) {
	right := Width - safeMargin
	r.cv.HLine(safeMargin, right, footerRule, 1, 0, cardkit.Opaque(r.pal.Border))

	var parts []string
	wc, okC := value(resp.Verdict.WorstClearanceM)
	at, okA := value(resp.Verdict.WorstAtM)
	switch {
	case main.state == "unknown":
		if plotted {
			parts = append(parts, main.detail)
		}
	case okC && okA:
		parts = append(parts, "Tightest Fresnel point: clearance "+fmtMetres(wc)+" at "+fmtDistance(at))
	}
	heights := fmtMetres(resp.A.AntennaM) + " / " + fmtMetres(resp.B.AntennaM)
	if resp.A.Assumed || resp.B.Assumed {
		parts = append(parts, "heights assumed ("+heights+")")
	} else {
		parts = append(parts, "heights "+heights)
	}
	if resp.Model == "both" {
		parts = append(parts, "terrain + surface")
	} else {
		parts = append(parts, "terrain only")
	}
	// The credits are never shortened and never left out (CC BY 4.0 for
	// DHM and the footprints, the Copernicus DEM licence for the fill):
	// stacked up from creditBaseline, the DHM credit first.
	type line struct{ role, text string }
	var credits []line
	for _, l := range creditLines(resp) {
		credits = append(credits, line{"attribution", l})
	}
	if resp.Coverage.Mixed {
		credits = append(credits, line{"attributionFill", FillCredit})
	}
	firstY := creditBaseline - creditLeading*(len(credits)-1)
	summaryY := summaryBaseline
	if len(credits) > 1 {
		summaryY = firstY - creditLeading
	}
	// Three credit lines (a classified mixed card, which the server never
	// draws) leave no room for the summary under the rule: the credits
	// are obligations, the summary is not.
	if summaryY-summarySize >= footerRule {
		summary := r.fs.Fit(strings.Join(parts, " · "), summarySize, false, right-safeMargin)
		r.text("summary", summary, safeMargin, summaryY, summarySize, false, cardkit.Opaque(r.pal.Text))
	}

	muted := cardkit.Opaque(r.pal.TextMuted)
	w := 0
	for i, l := range credits {
		lw := r.text(l.role, l.text, safeMargin, firstY+creditLeading*i, attributionSize, false, muted)
		if i == 0 {
			w = lw
		}
	}
	if site := cardkit.Sanitize(opt.Site); site != "" {
		site = r.fs.Fit(site, attributionSize, false, right-(safeMargin+w+32))
		if site != "" {
			r.text("site", site, right-r.fs.Measure(site, attributionSize, false), firstY, attributionSize, false, muted)
		}
	}
}

// ── Number formatting, as the panel does it ───────────────────────────────

// jsRound is Math.round(v·10^places)/10^places: halves round up, as in JS.
func jsRound(v float64, places int) float64 {
	f := math.Pow(10, float64(places))
	return math.Floor(float64(v*f)+0.5) / f
}

// fmtNum is JS String(n) for the rounded values the card shows.
func fmtNum(v float64) string {
	if v == 0 {
		v = 0 // no "-0"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// fmtDistance is the panel's fmtDistance, for a distance along the path
// (#839 §8): whole metres under 1 km, km with two decimals to 10 km and
// one from there. fmtMetres stays for heights and clearances.
func fmtDistance(v float64) string {
	if !finite(v) {
		return "—"
	}
	if m := jsRound(v, 0); m < 1000 {
		return fmtNum(m) + " m"
	}
	places := 2
	if jsRound(v/1000, 2) >= 10 {
		places = 1
	}
	return strconv.FormatFloat(jsRound(v/1000, places), 'f', places, 64) + " km"
}

// FmtDistance is fmtDistance for the share page's title, so the title and
// the card it previews state a distance the same way.
func FmtDistance(v float64) string { return fmtDistance(v) }

// fmtMetres is the panel's fmtMetres: km with one decimal from 10 km.
func fmtMetres(v float64) string {
	if !finite(v) {
		return "—"
	}
	if math.Abs(v) >= 10000 {
		return fmtNum(jsRound(v/1000, 1)) + " km"
	}
	return fmtNum(jsRound(v, 1)) + " m"
}
