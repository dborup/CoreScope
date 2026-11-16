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
}

// Carded reports whether role has a node card: repeaters, rooms and
// sensors (owner decision Q3). Companions are usually people's phones and
// get the plain site card; so does anything unknown.
func Carded(role string) bool {
	return role == "repeater" || role == "room" || role == "sensor"
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
}

// Clauses are the stat clauses Render draws for in, in drawing order.
func Clauses(in NodeCardInput) []Clause {
	ts := tiles(in)
	out := make([]Clause, len(ts))
	for i, t := range ts {
		out[i] = Clause{Label: t.label, Value: t.value, Detail: t.detail, Over: t.over}
	}
	return out
}

// tiles are the stat clauses for in, in drawing order. An absent or
// invalid value (nil, negative, NaN, an airtime over 100 %) drops its
// clause; it is never drawn as 0.
func tiles(in NodeCardInput) []tile {
	var ts []tile
	if !in.LastHeard.IsZero() {
		ts = append(ts, tile{label: "Last heard", value: RelativeTime(in.LastHeard, in.AsOf), detail: absUTC(in.LastHeard)})
	}
	if rs := in.Relay; rs != nil && (in.Role == "repeater" || in.Role == "room") {
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
	return ts
}

func count(p *int) (string, bool) {
	if p == nil || *p < 0 {
		return "", false
	}
	return fmtCount(*p), true
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
	s := toFixed1(pct) + "%"
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

// toFixed1 is JS Number.prototype.toFixed(1): the exact binary value
// rounded to one decimal, an exact tie (x.25, x.75) rounded up, where Go's
// formatter would round it to even.
func toFixed1(v float64) string {
	if q := float64(v * 4); q == math.Trunc(q) && float64(v*2) != math.Trunc(float64(v*2)) {
		v = math.Ceil(float64(v*10)) / 10
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// fmtCount writes v with a comma between thousands, as toLocaleString does
// in English.
func fmtCount(v int) string {
	s := strconv.Itoa(v)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
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

func (r *renderer) tiles(ts []tile) {
	for i, t := range ts {
		if i >= 2*tileCols {
			break // six clauses at most; tiles() never makes more
		}
		x := safeMargin + (i%tileCols)*tileW
		y := tileTop + (i/tileCols)*tileRowH
		maxW := tileW - tileGap
		valueC, detailC := r.pal.Text, r.pal.TextMuted
		if t.over {
			valueC, detailC = r.pal.StatusRed, r.pal.StatusRed
		}
		r.text("label", r.fs.Fit(t.label, labelSize, false, maxW), x, y, labelSize, false, cardkit.Opaque(r.pal.TextMuted))
		size, v := r.fit(t.value, valueSize, minValueSize, true, maxW)
		r.text("value", v, x, y+valueDrop, size, true, cardkit.Opaque(valueC))
		r.text("detail", r.fs.Fit(t.detail, detailSize, false, maxW), x, y+detailDrop, detailSize, false, cardkit.Opaque(detailC))
	}
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
