// Package nodecard draws the 1200×630 link-preview card for a shared node
// page (#811 M2): the node's name and role, when it was last heard, the
// relay block for repeaters and rooms, the neighbour count, and an "as of"
// footer with the site name.
//
// Render is a pure function of its arguments: no I/O, no clock, no package
// state, and the same pixels for the same input on every architecture (all
// drawing goes through internal/cardkit). Looking the node up, the
// visibility rule, caching and HTTP are the caller's (cmd/server, M3).
//
// The card has no map, no packet content and no coordinates, and the input
// type has no field that could carry them (#811 plan, Q3).
package nodecard

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/meshcore-analyzer/cardkit"
)

// Card geometry. Nothing that carries meaning sits within safeMargin px of
// an edge: Facebook and LinkedIn crop a few pixels off a 1.91:1 preview.
const (
	Width, Height = 1200, 630
	safeMargin    = 24

	titleSize    = 44
	minTitleSize = 28
	badgeSize    = 24
	badgePad     = 18
	areaSize     = 24
	labelSize    = 22
	valueSize    = 44
	minValueSize = 28
	detailSize   = 20
	footerSize   = 22

	textX         = 44 // title and area, right of the accent bar
	titleBaseline = 78
	areaBaseline  = 114
	headerRule    = 136

	tileCols    = 3
	tileW       = (Width - 2*safeMargin) / tileCols
	tileGap     = 24 // kept free on the right of every tile
	tileTop     = 196
	tileRowH    = 170
	valueDrop   = 56 // label baseline → value baseline
	detailDrop  = 88 // label baseline → detail baseline
	footerRule  = 548
	footerBase  = 594
	footerSpace = 32 // between the as-of line and the site name

	// The dense grid (#857), for more than 2*tileCols clauses: 4 columns
	// by 3 rows, smaller text, the same safe area and footer.
	denseCols       = 4
	denseTileW      = (Width - 2*safeMargin) / denseCols
	denseTop        = 172
	denseRowH       = 128
	denseLabelSize  = 18
	denseValueSize  = 34
	denseMinValue   = 22
	denseDetailSize = 17
	denseValueDrop  = 42
	denseDetailDrop = 66
	maxTiles        = 3 * denseCols

	// The mid grid (#857), for up to two rows of four: the wide grid's
	// rows, text between the two.
	midLabelSize    = 20
	midValueSize    = 40
	midMinValue     = 26
	midDetailSize   = 18
	midValueDrop    = 50
	midDetailDrop   = 78
	maxScopesInText = 3 // transported scopes named in the clause text
	scopeSep        = ", "
)

// warnPct is the duty-cycle limit seen airtime is flagged at: the default
// of ObserverAirtime.isOver (public/observer-airtime.js), which is what
// the busiest-repeaters table uses.
const warnPct = 10

// textRun is one string the card drew, kept so tests can assert words
// rather than pixels. Y is the baseline; W the drawn width.
type textRun struct {
	Role, Text string
	Size       int
	Bold       bool
	X, Y, W    int
}

// tile is one stat clause: a label, the value, and an optional detail line.
// over draws the value and detail in the warning colour.
type tile struct {
	label, value, detail string
	over                 bool
	// absTime: detail is the absolute time of value, which follows
	// value's wording (last heard, first seen).
	absTime bool
	// scopes, when set, is the full list value names (transported
	// scopes); the card draws as many as fit, then "+N".
	scopes []string
}

// Carded reports whether role has a node card: repeaters, rooms and
// sensors (owner decision Q3). Companions are usually people's phones and
// get the plain site card; so does anything unknown.
func Carded(role string) bool {
	return role == "repeater" || role == "room" || role == "sensor"
}

// CardedRoles lists the roles Carded accepts, for the client config
// (#811 M4), so the browser offers /p?node= for exactly these.
func CardedRoles() []string {
	return []string{"repeater", "room", "sensor"}
}

// RoleLabel is the role as the card's badge names it; cmd/server puts the
// same words in og:title.
func RoleLabel(role string) string {
	switch role {
	case "repeater":
		return "Repeater"
	case "room":
		return "Room Server"
	}
	return "Sensor"
}

// Render draws the card for in and returns it as a PNG. It returns
// ErrRoleNotCarded for a role Carded refuses and ErrNoAsOf without an AsOf.
func Render(in NodeCardInput, opt Options) ([]byte, error) {
	img, _, err := render(in, opt)
	if err != nil {
		return nil, err
	}
	return encode(img)
}

func encode(img *image.RGBA) ([]byte, error) { return cardkit.EncodePNG(img) }

type renderer struct {
	cv   cardkit.Canvas
	fs   *cardkit.Fonts
	pal  Palette
	runs []textRun
}

func render(in NodeCardInput, opt Options) (*image.RGBA, []textRun, error) {
	if !Carded(in.Role) {
		return nil, nil, ErrRoleNotCarded
	}
	if in.AsOf.IsZero() {
		return nil, nil, ErrNoAsOf
	}
	pal := DefaultPalette(true)
	if opt.Palette != nil {
		pal = *opt.Palette
	}
	fs, err := cardkit.LoadFonts()
	if err != nil {
		return nil, nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Rect, image.NewUniform(cardkit.Opaque(pal.Background)), image.Point{}, draw.Src)
	r := &renderer{cv: cardkit.Canvas{Img: img}, fs: fs, pal: pal}
	r.header(in, opt)
	r.tiles(tiles(in))
	r.footer(in, opt)
	return img, r.runs, nil
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

// fit shrinks s from size to minSize in steps of 4, then shortens it with
// "…", until it is at most maxW wide.
func (r *renderer) fit(s string, size, minSize int, bold bool, maxW int) (int, string) {
	for ; size > minSize; size -= 4 {
		if r.fs.Measure(s, size, bold) <= maxW {
			return size, s
		}
	}
	return minSize, r.fs.Fit(s, minSize, bold, maxW)
}

// ── Header ───────────────────────────────────────────────────────────────

func (r *renderer) header(in NodeCardInput, opt Options) {
	right := Width - safeMargin
	roleC := cardkit.Opaque(r.pal.roleColor(in.Role))

	label := RoleLabel(in.Role)
	bw := r.fs.Measure(label, badgeSize, true) + 2*badgePad
	badge := image.Rect(right-bw, 34, right, 78)
	r.cv.Pill(badge, roleC)
	r.text("badge", label, badge.Min.X+badgePad, 64, badgeSize, true,
		cardkit.Opaque(cardkit.ReadableOn(roleC, r.pal.Text, r.pal.Background)))

	r.cv.FillRect(image.Rect(safeMargin, 34, safeMargin+6, 122), roleC)
	name := ""
	if !opt.HideNames {
		name = r.fs.DrawableName(in.Name)
	}
	if name == "" {
		name = FallbackTitle(in.PubKey)
	}
	size, title := r.fit(name, titleSize, minTitleSize, true, badge.Min.X-safeMargin-textX)
	r.text("title", title, textX, titleBaseline, size, true, cardkit.Opaque(r.pal.Text))

	if area := r.fs.DrawableName(in.Area); area != "" {
		area = r.fs.Fit(area, areaSize, false, right-textX)
		r.text("area", area, textX, areaBaseline, areaSize, false, cardkit.Opaque(r.pal.TextMuted))
	}
	r.cv.HLine(safeMargin, right, headerRule, 1, 0, cardkit.Opaque(r.pal.Border))
}

// FallbackTitle is the title when the name is hidden or cannot be drawn:
// "Node" and the first 8 characters of the key, only when they are hex.
func FallbackTitle(key string) string {
	if len(key) < 8 {
		return "Node"
	}
	for _, c := range key[:8] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return "Node"
		}
	}
	return "Node " + strings.ToLower(key[:8])
}

// ── Stat clauses ─────────────────────────────────────────────────────────

// Clause is one stat clause as the card draws it, for the text of the
// preview (og:description) and the card's version hash (#811 M3), so the
// text and the picture can never say different things.
type Clause struct {
	Label, Value, Detail string
	// Over is a seen airtime at or over the duty-cycle limit, drawn in the
	// warning colour.
	Over bool
	// AbsTime is true when Detail is the absolute UTC time of Value
	// (last heard, first seen): it follows Value's wording, so the
	// server's v leaves it out and buckets by the wording (#857).
	AbsTime bool
}

// Clauses are the stat clauses Render draws for in, in drawing order.
func Clauses(in NodeCardInput) []Clause {
	ts := tiles(in)
	out := make([]Clause, len(ts))
	for i, t := range ts {
		out[i] = Clause{Label: t.label, Value: t.value, Detail: t.detail, Over: t.over, AbsTime: t.absTime}
	}
	return out
}

// tiles are the stat clauses for in, in drawing order. An absent or
// invalid value (nil, negative, NaN, an airtime over 100 %) drops its
// clause; it is never drawn as 0.
func tiles(in NodeCardInput) []tile {
	var ts []tile
	if !in.LastHeard.IsZero() {
		ts = append(ts, timeTile("Last heard", in.LastHeard, in.AsOf))
	}
	relays := in.Role == "repeater" || in.Role == "room"
	if rs := in.Relay; rs != nil && relays {
		if v, ok := count(rs.Relayed24h); ok {
			ts = append(ts, tile{label: "Relayed 24 h", value: v})
		}
		if v, ok := count(rs.Relayed1h); ok {
			ts = append(ts, tile{label: "Relayed 1 h", value: v})
		}
		if t, ok := airtimeTile("Seen airtime 24 h", rs.AirtimePct24h); ok {
			ts = append(ts, t)
		}
		if t, ok := airtimeTile("Busiest hour", rs.AirtimePeakPct); ok {
			ts = append(ts, t)
		}
	}
	if v, ok := count(in.Neighbors); ok {
		ts = append(ts, tile{label: "Neighbors", value: v})
	}
	// #857, with the node page's labels and its rules for hiding a row.
	if !in.FirstSeen.IsZero() {
		ts = append(ts, timeTile("First seen", in.FirstSeen, in.AsOf))
	}
	if p := in.AvgHops; p != nil && *p > 0 { // nodes.js: ${stats.avgHops ? … : ''}
		ts = append(ts, tile{label: "Avg Hops", value: strconv.Itoa(*p)})
	}
	if t, ok := ownScopeTile(in.OwnScope, in.OwnScopeConfirmed); ok {
		ts = append(ts, t)
	}
	if relays {
		if t, ok := scopesTile(in.TransportedScopes); ok {
			ts = append(ts, t)
		}
	}
	if t, ok := intervalTile("Estimated flood interval", in.FloodInterval, floodUnit); ok {
		ts = append(ts, t)
	}
	if t, ok := intervalTile("Estimated zero-hop interval", in.ZeroHopInterval, zeroHopUnit); ok {
		ts = append(ts, t)
	}
	return ts
}

func timeTile(label string, at, asOf time.Time) tile {
	return tile{label: label, value: RelativeTime(at, asOf), detail: absUTC(at), absTime: true}
}

// ownScopeTile is nodes.js row-own-scope: NULL has no row on the card
// (the node page's "—"), "" is "unknown scope", any other value is the
// scope as stored ("*" included). Confirmed is the page's check mark.
func ownScopeTile(scope *string, confirmed bool) (tile, bool) {
	if scope == nil {
		return tile{}, false
	}
	v := "unknown scope"
	if *scope != "" {
		if v = cardkit.Sanitize(*scope); v == "" {
			return tile{}, false
		}
	}
	t := tile{label: "Own scope", value: v}
	if confirmed {
		t.detail = "confirmed"
	}
	return t, true
}

// scopesTile names at most maxScopesInText scopes, then "+N"; the card
// draws as many of t.scopes as fit.
func scopesTile(raw []string) (tile, bool) {
	var scopes []string
	for _, s := range raw {
		if s = cardkit.Sanitize(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		return tile{}, false
	}
	return tile{label: "Transported scopes", value: scopeList(scopes, min(len(scopes), maxScopesInText)), scopes: scopes}, true
}

// scopeList is the first k scopes and "+N" for the rest.
func scopeList(scopes []string, k int) string {
	s := strings.Join(scopes[:k], scopeSep)
	if k < len(scopes) {
		s += " +" + strconv.Itoa(len(scopes)-k)
	}
	return s
}

// intervalUnit is one class of node-adverts.js INTERVAL_CLASSES.
type intervalUnit struct {
	seconds float64
	label   string
	rng     string
}

var (
	floodUnit   = intervalUnit{3600, "h", "3–168 h"}
	zeroHopUnit = intervalUnit{60, "min", "60–240 min"}
)

// intervalTile is node-adverts.js intervalRow for an estimate, at medium
// or high confidence only (#857: a low-confidence estimate is dropped).
// The sample count is left out: it changes with every advert, the
// interval does not.
func intervalTile(label string, iv *AdvertInterval, u intervalUnit) (tile, bool) {
	if iv == nil || iv.Seconds <= 0 || (iv.Confidence != "high" && iv.Confidence != "medium") {
		return tile{}, false
	}
	t := tile{label: label, value: "≈ " + fmtInterval(iv.Seconds, u), detail: iv.Confidence + " confidence"}
	if !iv.Snapped {
		// Both notes do not fit a tile; this one matters more, and the
		// confidence is medium or high whenever a tile is drawn.
		t.detail = "outside the settable " + u.rng
	}
	return t, true
}

// fmtInterval is node-adverts.js fmtInterval: Math.round to one decimal in
// the class's unit, and flood under an hour in whole minutes.
func fmtInterval(seconds int64, u intervalUnit) string {
	v := float64(seconds) / u.seconds
	if u.seconds == 3600 && v < 1 {
		return strconv.FormatFloat(math.Floor(float64(seconds)/60+0.5), 'f', -1, 64) + " min"
	}
	return strconv.FormatFloat(math.Floor(v*10+0.5)/10, 'f', -1, 64) + " " + u.label
}

func count(p *int) (string, bool) {
	if p == nil || *p < 0 {
		return "", false
	}
	return cardkit.FmtCount(*p), true
}

func airtimeTile(label string, p *float64) (tile, bool) {
	if p == nil || math.IsNaN(*p) || *p < 0 || *p > 100 {
		return tile{}, false
	}
	t := tile{label: label, value: airtimeText(*p), detail: "lower bound", over: airtimeOver(*p)}
	if t.over {
		t.detail = "at or over the " + strconv.Itoa(warnPct) + "% limit"
	}
	return t, true
}

// airtimeText is the busiest-repeaters table's cell text
// (analytics.js _busiestAirtimeCell over ObserverAirtime.format): one
// decimal, "<0.1%" for a trace, and "≥" in front of any other non-zero
// value, since seen airtime is a lower bound.
func airtimeText(pct float64) string {
	if pct > 0 && pct < 0.1 {
		return "<0.1%"
	}
	s := cardkit.ToFixed1(pct) + "%"
	if pct > 0 {
		return "≥" + s
	}
	return s
}

// airtimeOver is ObserverAirtime.isOver at the default limit: compared at
// the one decimal shown, so "10.0%" is never unflagged.
func airtimeOver(pct float64) bool {
	return math.Floor(float64(pct*10)+0.5)/10 >= warnPct
}

// RelativeTime is the Go twin of public/app.js timeAgo, measured against
// asOf instead of the wall clock: "45s ago", "3m ago", "2h ago", "5d ago",
// or "in 2h" for a time after asOf. M3 buckets the card's version hash by
// this wording.
func RelativeTime(last, asOf time.Time) string {
	d := asOf.Sub(last) // saturates instead of overflowing
	s := int64(d / time.Second)
	if d%time.Second < 0 {
		s-- // Math.floor
	}
	abs := s
	if abs < 0 {
		abs = -abs
	}
	var v int64
	var suffix string
	switch {
	case abs < 60:
		v, suffix = abs, "s"
	case abs < 3600:
		v, suffix = abs/60, "m"
	case abs < 86400:
		v, suffix = abs/3600, "h"
	default:
		v, suffix = abs/86400, "d"
	}
	if s < 0 {
		return "in " + strconv.FormatInt(v, 10) + suffix
	}
	return strconv.FormatInt(v, 10) + suffix + " ago"
}

func absUTC(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") + " UTC" }

// grid is one tile layout: the M2 grid for up to six clauses, the dense
// one (#857) for more.
type grid struct {
	cols, w, top, rowH                int
	labelSize, valueSize, minValue    int
	detailSize, valueDrop, detailDrop int
}

var (
	wideGrid  = grid{tileCols, tileW, tileTop, tileRowH, labelSize, valueSize, minValueSize, detailSize, valueDrop, detailDrop}
	midGrid   = grid{denseCols, denseTileW, tileTop, tileRowH, midLabelSize, midValueSize, midMinValue, midDetailSize, midValueDrop, midDetailDrop}
	denseGrid = grid{denseCols, denseTileW, denseTop, denseRowH, denseLabelSize, denseValueSize, denseMinValue, denseDetailSize, denseValueDrop, denseDetailDrop}
)

// gridFor picks the layout for n clauses: the M2 grid up to six, four
// columns on the same two rows up to eight, the dense 4×3 grid beyond.
func gridFor(n int) grid {
	switch {
	case n <= 2*tileCols:
		return wideGrid
	case n <= 2*denseCols:
		return midGrid
	}
	return denseGrid
}

func (r *renderer) tiles(ts []tile) {
	g := gridFor(len(ts))
	for i, t := range ts {
		if i >= maxTiles {
			break // twelve clauses at most; tiles() never makes more
		}
		x := safeMargin + (i%g.cols)*g.w
		y := g.top + (i/g.cols)*g.rowH
		maxW := g.w - tileGap
		valueC, detailC := r.pal.Text, r.pal.TextMuted
		if t.over {
			valueC, detailC = r.pal.StatusRed, r.pal.StatusRed
		}
		r.text("label", r.fs.Fit(t.label, g.labelSize, false, maxW), x, y, g.labelSize, false, cardkit.Opaque(r.pal.TextMuted))
		size, v := r.fit(t.value, g.valueSize, g.minValue, true, maxW)
		if t.scopes != nil {
			size, v = r.fitScopes(t.scopes, g.valueSize, g.minValue, maxW)
		}
		r.text("value", v, x, y+g.valueDrop, size, true, cardkit.Opaque(valueC))
		r.text("detail", r.fs.Fit(t.detail, g.detailSize, false, maxW), x, y+g.detailDrop, g.detailSize, false, cardkit.Opaque(detailC))
	}
}

// fitScopes draws the whole list when it fits at some size down to
// minSize; else, at minSize, as many scopes as fit and "+N" for the rest,
// and at worst the first scope shortened with "…" before "+N".
func (r *renderer) fitScopes(scopes []string, size, minSize, maxW int) (int, string) {
	all := scopeList(scopes, len(scopes))
	for ; size > minSize; size -= 4 {
		if r.fs.Measure(all, size, true) <= maxW {
			return size, all
		}
	}
	for k := len(scopes); k >= 1; k-- {
		if s := scopeList(scopes, k); r.fs.Measure(s, minSize, true) <= maxW {
			return minSize, s
		}
	}
	rest := " +" + strconv.Itoa(len(scopes)-1)
	return minSize, r.fs.Fit(scopes[0], minSize, true, maxW-r.fs.Measure(rest, minSize, true)) + rest
}

// ── Footer ───────────────────────────────────────────────────────────────

func (r *renderer) footer(in NodeCardInput, opt Options) {
	right := Width - safeMargin
	muted := cardkit.Opaque(r.pal.TextMuted)
	r.cv.HLine(safeMargin, right, footerRule, 1, 0, cardkit.Opaque(r.pal.Border))
	w := r.text("asof", "as of "+absUTC(in.AsOf), safeMargin, footerBase, footerSize, false, muted)
	if site := cardkit.Sanitize(opt.Site); site != "" {
		if site = r.fs.Fit(site, footerSize, false, right-(safeMargin+w+footerSpace)); site != "" {
			r.text("site", site, right-r.fs.Measure(site, footerSize, false), footerBase, footerSize, false, muted)
		}
	}
}
