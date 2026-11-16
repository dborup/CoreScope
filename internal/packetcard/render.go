// Package packetcard draws the 1200×630 link-preview card for a shared
// packet (#812 M1): the packet's path as a schematic with no basemap (hop
// points and lines, the observers that heard it, the first to hear it
// ringed, faint outlines of the touched areas), with the payload type,
// route, hop count, who heard it first and how many did, the spread, the
// farthest observer, the estimated flood airtime and when it was first
// heard.
//
// Render is a pure function of its arguments: no I/O, no clock, no package
// state, and the same pixels for the same input on every architecture.
// Every drawing primitive is internal/cardkit's. Looking the packet up,
// the path visibility rule, caching and HTTP are the caller's (cmd/server,
// M2); the input type cannot carry message text, a channel, a key or any
// name but the two the privacy rules allow (types.go).
package packetcard

import (
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"

	"github.com/meshcore-analyzer/cardkit"
)

// Card geometry. Nothing that carries meaning sits within safeMargin px of
// an edge: Facebook and LinkedIn crop a few pixels off a 1.91:1 preview.
const (
	Width, Height = 1200, 630
	safeMargin    = 24

	titleSize     = 44
	minTitleSize  = 28
	heardSize     = 24
	nameMaxW      = 300 // each name in the heard line
	textX         = 44  // header text, right of the accent bar
	titleBaseline = 78
	heardBaseline = 114
	headerRule    = 136

	mapTop, mapBottom = 146, 450
	mapPad            = 24
	innerLeft         = safeMargin + mapPad
	innerTop          = mapTop + mapPad
	noteSize          = 18
	noteBaseline      = 472
	emptySize         = 26

	tileCols   = 4
	tileW      = (Width - 2*safeMargin) / tileCols
	tileGap    = 24
	labelSize  = 20
	labelBase  = 502
	valueSize  = 34
	minValSize = 24
	valueBase  = 540
	footerRule = 556
	footerSize = 22
	footerBase = 596
	footerGap  = 32
)

func innerW() int { return Width - 2*safeMargin - 2*mapPad }
func innerH() int { return mapBottom - mapTop - 2*mapPad }

// Line widths and alphas, after packet-path-map.js: area outlines weight 1
// at opacity 0.35, other branches muted, the first branch emphasised.
var (
	areaWidth   = cardkit.Fx(1.5)
	branchWidth = cardkit.Fx(2)
	firstWidth  = cardkit.Fx(3)
)

const (
	areaAlpha   = 90  // 0.35
	branchAlpha = 150 // 0.6
	relayRadius = 3
	obsRadius   = 5
	ringOuter   = 10
	ringInner   = 7
)

// payloadNames are the firmware payload type names (Packet.h
// PAYLOAD_TYPE_*), as payload-labels.js and packet-filter.js name them.
var payloadNames = [...]string{
	0: "REQ", 1: "RESPONSE", 2: "TXT_MSG", 3: "ACK", 4: "ADVERT", 5: "GRP_TXT", 6: "GRP_DATA",
	7: "ANON_REQ", 8: "PATH", 9: "TRACE", 10: "MULTIPART", 11: "CONTROL", 15: "RAW_CUSTOM",
}

const payloadAdvert = 4

// PayloadName is the firmware name of a payload type, "Unknown type" for
// a value Packet.h does not define. It is the only description of the
// payload the card gives.
func PayloadName(pt int) string {
	if pt >= 0 && pt < len(payloadNames) && payloadNames[pt] != "" {
		return payloadNames[pt]
	}
	return "Unknown type"
}

var routeNames = [...]string{"Transport flood", "Flood", "Direct", "Transport direct"}

// RouteName is the route type in words (Packet.h ROUTE_TYPE_*), "" for an
// unknown one.
func RouteName(rt int) string {
	if rt >= 0 && rt < len(routeNames) {
		return routeNames[rt]
	}
	return ""
}

// textRun is one string the card drew, kept so tests can assert words
// rather than pixels. Y is the baseline; W the drawn width.
type textRun struct {
	Role, Text string
	Size       int
	Bold       bool
	X, Y, W    int
}

// trace is what one render drew, for the tests.
type trace struct {
	runs                                  []textRun
	branches, points, areas, areaVertices int
	mapped, first                         bool
}

// Render draws the card for in and returns it as a PNG.
func Render(in PacketCardInput, opt Options) ([]byte, error) {
	img, _, err := render(in, opt)
	if err != nil {
		return nil, err
	}
	return encode(img)
}

func encode(img *image.RGBA) ([]byte, error) { return cardkit.EncodePNG(img) }

type renderer struct {
	cv  cardkit.Canvas
	fs  *cardkit.Fonts
	pal Palette
	tr  *trace
}

func render(in PacketCardInput, opt Options) (*image.RGBA, *trace, error) {
	pal := DefaultPalette(true)
	if opt.Palette != nil {
		pal = *opt.Palette
	}
	fs, err := cardkit.LoadFonts()
	if err != nil {
		return nil, nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	r := &renderer{cv: cardkit.Canvas{Img: img}, fs: fs, pal: pal, tr: &trace{}}
	r.cv.FillRect(img.Rect, cardkit.Opaque(pal.Background))
	r.header(in, opt)
	r.drawMap(in)
	r.tiles(tiles(in))
	r.footer(in, opt)
	return img, r.tr, nil
}

// text draws s and records it. It returns the drawn width.
func (r *renderer) text(role, s string, x, y, size int, bold bool, c color.NRGBA) int {
	if s == "" {
		return 0
	}
	w := r.fs.Draw(&r.cv, s, x, y, size, bold, c)
	r.tr.runs = append(r.tr.runs, textRun{Role: role, Text: s, Size: size, Bold: bold, X: x, Y: y, W: w})
	return w
}

// ── Header ───────────────────────────────────────────────────────────────

// observers is how many visible observers heard the packet: one per
// branch, or the first alone.
func observers(in PacketCardInput) int {
	if len(in.Branches) == 0 && in.First != nil {
		return 1
	}
	return len(in.Branches)
}

// Title is "GRP_TXT · Flood · 6 hops": the payload type, the route and the
// deepest visible hop count, each dropped when unknown.
func Title(in PacketCardInput) string {
	parts := []string{PayloadName(in.PayloadType)}
	if rt := RouteName(in.RouteType); rt != "" {
		parts = append(parts, rt)
	}
	hops, ok := -1, false
	for _, b := range in.Branches {
		if b.Hops >= 0 && b.Hops > hops {
			hops, ok = b.Hops, true
		}
	}
	if f := in.First; f != nil && f.Hops >= 0 && f.Hops > hops {
		hops, ok = f.Hops, true
	}
	if ok {
		parts = append(parts, plural(hops, "hop"))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return cardkit.FmtCount(n) + " " + word + "s"
}

func (r *renderer) header(in PacketCardInput, opt Options) {
	right := Width - safeMargin
	r.cv.FillRect(image.Rect(safeMargin, 34, safeMargin+6, 122), cardkit.Opaque(r.pal.Accent))
	title := Title(in)
	size := titleSize
	for ; size > minTitleSize && r.fs.Measure(title, size, true) > right-textX; size -= 4 {
	}
	r.text("title", r.fs.Fit(title, size, true, right-textX), textX, titleBaseline, size, true, cardkit.Opaque(r.pal.Text))
	r.text("heard", r.fs.Fit(r.heardLine(in, opt), heardSize, false, right-textX), textX, heardBaseline, heardSize, false, cardkit.Opaque(r.pal.TextMuted))
	r.cv.HLine(safeMargin, right, headerRule, 1, 0, cardkit.Opaque(r.pal.Border))
}

// heardLine is "Advert from <origin> · first heard by <observer> · heard
// by N observers". The origin is named only for an advert (privacy rule
// 2), the observer only with First (owner decision Q3): without either,
// or with a name the card cannot draw, the line falls back to the count
// and no other name takes its place.
func (r *renderer) heardLine(in PacketCardInput, opt Options) string {
	var parts []string
	name := func(s string) string {
		if opt.HideNames {
			return ""
		}
		return r.fs.Fit(r.fs.DrawableName(s), heardSize, false, nameMaxW)
	}
	if in.PayloadType == payloadAdvert {
		if n := name(in.AdvertName); n != "" {
			parts = append(parts, "Advert from "+n)
		}
	}
	if in.First != nil {
		if n := name(in.FirstObserverName); n != "" {
			parts = append(parts, "first heard by "+n)
		}
	}
	if n := observers(in); n > 0 {
		parts = append(parts, "heard by "+plural(n, "observer"))
	} else {
		parts = append(parts, "no visible observers")
	}
	line := strings.Join(parts, " · ")
	return strings.ToUpper(line[:1]) + line[1:]
}

// ── Map ──────────────────────────────────────────────────────────────────

// drawable is a branch's drawable positions: its decimated relays, then
// its observer, each converted or dropped.
func drawable(b Branch, perBranch int) (relays []upt, obs *upt) {
	for _, p := range decimate(b.Points, perBranch) {
		if u, ok := toMicro(p); ok {
			relays = append(relays, u)
		}
	}
	if b.Observer != nil {
		if u, ok := toMicro(*b.Observer); ok {
			obs = &u
		}
	}
	return relays, obs
}

type mapBranch struct {
	relays []upt
	obs    *upt
}

func (r *renderer) drawMap(in PacketCardInput) {
	shown := in.Branches[:min(len(in.Branches), MaxBranches)]
	slots := len(shown)
	if in.First != nil {
		slots++
	}
	perBranch := MaxPoints
	if slots > 0 {
		perBranch = max(MaxPoints/slots-1, 1) // -1: the observer
	}
	var all []upt
	collect := func(b Branch) mapBranch {
		relays, obs := drawable(b, perBranch)
		all = append(all, relays...)
		if obs != nil {
			all = append(all, *obs)
		}
		return mapBranch{relays, obs}
	}
	branches := make([]mapBranch, 0, len(shown))
	for _, b := range shown {
		branches = append(branches, collect(b))
	}
	var first *mapBranch
	if in.First != nil {
		f := collect(*in.First)
		first = &f
	}
	muted := cardkit.Opaque(r.pal.TextMuted)
	if len(all) == 0 {
		msg := "No station on this path has an advertised position"
		w := r.fs.Measure(msg, emptySize, false)
		r.text("empty", r.fs.Fit(msg, emptySize, false, Width-2*safeMargin), max((Width-w)/2, safeMargin), (mapTop+mapBottom)/2+emptySize/3, emptySize, false, muted)
		return
	}
	r.tr.mapped = true
	r.tr.points = len(all)
	pr := newProjection(all)
	r.areas(pr, in.Areas)

	path := r.pal.Accent
	path.A = uint8(uint32(path.A) * branchAlpha / 255)
	for _, b := range branches {
		r.branch(pr, b, branchWidth, path)
	}
	if first != nil {
		r.branch(pr, *first, firstWidth, cardkit.Opaque(r.pal.Accent))
	}
	for _, b := range branches {
		r.dots(pr, b)
	}
	if first != nil {
		r.dots(pr, *first)
		if first.obs != nil {
			_, c := snap(pr.pixel(pr.local(*first.obs)))
			r.cv.Disc(c.X, c.Y, ringOuter, cardkit.Opaque(r.pal.StatusGreen))
			r.cv.Disc(c.X, c.Y, ringInner, cardkit.Opaque(r.pal.Background))
			r.cv.Disc(c.X, c.Y, obsRadius, cardkit.Opaque(r.pal.StatusYellow))
			r.tr.first = true
		}
	}

	r.text("note", "Path is a schematic; positions as advertised", safeMargin, noteBaseline, noteSize, false, muted)
	if n := len(in.Branches) - len(shown); n > 0 {
		s := "+" + cardkit.FmtCount(n) + " more not drawn"
		r.text("more", s, Width-safeMargin-r.fs.Measure(s, noteSize, false), noteBaseline, noteSize, false, muted)
	}
}

// branch draws one branch's line, relays in path order, then its
// observer.
func (r *renderer) branch(pr projection, b mapBranch, w int32, c color.NRGBA) {
	pts := make([]image.Point, 0, len(b.relays)+1)
	for _, u := range b.relays {
		p, _ := snap(pr.pixel(pr.local(u)))
		pts = append(pts, p)
	}
	if b.obs != nil {
		p, _ := snap(pr.pixel(pr.local(*b.obs)))
		pts = append(pts, p)
	}
	if len(pts) > 1 {
		r.cv.Polyline(pts, w, false, c)
	}
	r.tr.branches++
}

// dots draws a branch's relays and its observer.
func (r *renderer) dots(pr projection, b mapBranch) {
	for _, u := range b.relays {
		_, c := snap(pr.pixel(pr.local(u)))
		r.cv.Disc(c.X, c.Y, relayRadius, cardkit.Opaque(r.pal.Accent))
	}
	if b.obs != nil {
		_, c := snap(pr.pixel(pr.local(*b.obs)))
		r.cv.Disc(c.X, c.Y, obsRadius, cardkit.Opaque(r.pal.StatusYellow))
	}
}

// areas draws the touched areas' outlines, clipped to the map, faint and
// behind everything else (owner decision Q4). A ring with a vertex that
// cannot be drawn, or fewer than three, is left out.
func (r *renderer) areas(pr projection, areas []Area) {
	areas = areas[:min(len(areas), MaxAreas)]
	if len(areas) == 0 {
		return
	}
	perArea := max(MaxAreaVertices/len(areas), 3)
	c := r.pal.Accent
	c.A = uint8(uint32(c.A) * areaAlpha / 255)
	x0, y0, x1, y1 := pr.clipRect()
	for _, a := range areas {
		ring := decimate(a.Ring, perArea)
		if len(ring) < 3 {
			continue
		}
		local := make([]upt, 0, len(ring))
		for _, v := range ring {
			u, ok := toMicro(v)
			if !ok {
				local = nil
				break
			}
			local = append(local, pr.local(u))
		}
		if len(local) < 3 {
			continue
		}
		r.tr.areaVertices += len(local)
		// Clipped edges join into runs; each run is one polyline, so a
		// translucent outline is painted once per pixel.
		var run []image.Point
		drew := false
		flush := func() {
			if len(run) > 1 {
				r.cv.Polyline(run, areaWidth, false, c)
				drew = true
			}
			run = run[:0]
		}
		for i := range local {
			a, b, ok := clip(local[i], local[(i+1)%len(local)], x0, y0, x1, y1)
			if !ok {
				flush()
				continue
			}
			pa, pb := pr.pixel(a), pr.pixel(b)
			if len(run) == 0 || run[len(run)-1] != pa {
				flush()
				run = append(run, pa)
			}
			run = append(run, pb)
		}
		flush()
		if drew {
			r.tr.areas++
		}
	}
}

// ── Stat tiles ───────────────────────────────────────────────────────────

type tile struct{ label, value string }

// tiles are the stat clauses for in, in drawing order: observers always;
// the spread and the farthest observer only with a visible First, which
// they are measured from; the airtime estimate when known. An absent or
// invalid value drops its clause; it is never drawn as 0.
func tiles(in PacketCardInput) []tile {
	ts := []tile{{"Observers", cardkit.FmtCount(observers(in))}}
	if in.First != nil {
		spread, far := -1.0, -1.0
		for _, b := range in.Branches {
			if v, ok := positive(b.SecondsAfterFirst); ok && v > spread {
				spread = v
			}
			if v, ok := positive(b.DistanceFromFirstKm); ok && v > far {
				far = v
			}
		}
		if spread > 0 {
			ts = append(ts, tile{"Spread", fmtDuration(spread)})
		}
		if far > 0 {
			ts = append(ts, tile{"Farthest from first", cardkit.ToFixed1(far) + " km"})
		}
	}
	if v, ok := finite(in.AirtimeMs); ok && v >= 0 {
		ts = append(ts, tile{"Est. flood airtime", "≥" + fmtAirtime(v)})
	}
	return ts
}

// finite is *p when it is a finite number, and not too large to format.
func finite(p *float64) (float64, bool) {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || math.Abs(*p) > 1e12 {
		return 0, false
	}
	return *p, true
}

func positive(p *float64) (float64, bool) {
	v, ok := finite(p)
	return v, ok && v > 0
}

// fmtDuration is packet-path-map.js formatDuration: "4.2s" under a
// minute, "2m 6s" from there.
func fmtDuration(s float64) string {
	if s < 60 {
		return cardkit.ToFixed1(s) + "s"
	}
	m := math.Floor(s / 60)
	return strconv.FormatFloat(m, 'f', 0, 64) + "m " + strconv.FormatFloat(math.Floor(math.Mod(s, 60)+0.5), 'f', 0, 64) + "s"
}

// fmtAirtime is packet-path-map.js formatAirtimeMs: whole ms under a
// second, seconds with one decimal from there.
func fmtAirtime(ms float64) string {
	if ms < 1000 {
		return strconv.FormatFloat(math.Floor(ms+0.5), 'f', 0, 64) + "ms"
	}
	return cardkit.ToFixed1(ms/1000) + "s"
}

func (r *renderer) tiles(ts []tile) {
	for i, t := range ts {
		if i >= tileCols {
			break
		}
		x := safeMargin + i*tileW
		maxW := tileW - tileGap
		r.text("label", r.fs.Fit(t.label, labelSize, false, maxW), x, labelBase, labelSize, false, cardkit.Opaque(r.pal.TextMuted))
		size := valueSize
		for ; size > minValSize && r.fs.Measure(t.value, size, true) > maxW; size -= 4 {
		}
		r.text("value", r.fs.Fit(t.value, size, true, maxW), x, valueBase, size, true, cardkit.Opaque(r.pal.Text))
	}
}

// ── Footer ───────────────────────────────────────────────────────────────

func (r *renderer) footer(in PacketCardInput, opt Options) {
	right := Width - safeMargin
	muted := cardkit.Opaque(r.pal.TextMuted)
	r.cv.HLine(safeMargin, right, footerRule, 1, 0, cardkit.Opaque(r.pal.Border))
	w := 0
	if !in.FirstHeard.IsZero() && in.FirstHeard.Year() > 0 && in.FirstHeard.Year() < 10000 {
		w = r.text("footer", "First heard "+in.FirstHeard.UTC().Format("2006-01-02 15:04:05")+" UTC", safeMargin, footerBase, footerSize, false, muted)
	}
	if site := cardkit.Sanitize(opt.Site); site != "" {
		if site = r.fs.Fit(site, footerSize, false, right-(safeMargin+w+footerGap)); site != "" {
			r.text("site", site, right-r.fs.Measure(site, footerSize, false), footerBase, footerSize, false, muted)
		}
	}
}
