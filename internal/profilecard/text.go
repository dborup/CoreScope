package profilecard

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
	"golang.org/x/text/unicode/norm"
)

// Text is drawn from the Go fonts, which cover Latin (æøå and the rest of
// Europe), Greek and Cyrillic, but not CJK or emoji (plan §2.1).
//
// Glyphs are rasterised without x/image/vector's QuadTo: its lerp compiles
// to a fused multiply-add on arm64 and not on amd64, which would make the
// golden PNGs architecture-dependent. sfnt hands out outlines in 26.6
// fixed point; they are flattened here in integer arithmetic and fed to
// the rasteriser as exactly representable line segments. Glyph rasters
// stay under vector's 512 px floating-point threshold, so it runs its
// fixed-point path, which is integer throughout.

type fontSet struct {
	regular, bold *sfnt.Font
	buf           sfnt.Buffer
	glyphs        map[glyphKey]*glyphMask // per render; never shared
}

type glyphKey struct {
	bold bool
	size int
	gi   sfnt.GlyphIndex
}

// glyphMask is a rendered glyph: its coverage and where its top-left
// pixel sits relative to the pen position on the baseline.
type glyphMask struct {
	mask   *image.Alpha
	dx, dy int
}

// loadFonts parses the embedded Go fonts. sfnt parses lazily, so this is
// cheap enough to do per render, which keeps Render free of package state.
func loadFonts() (*fontSet, error) {
	r, err := sfnt.Parse(goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("profilecard: regular font: %w", err)
	}
	b, err := sfnt.Parse(gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("profilecard: bold font: %w", err)
	}
	return &fontSet{regular: r, bold: b, glyphs: map[glyphKey]*glyphMask{}}, nil
}

func (fs *fontSet) font(bold bool) *sfnt.Font {
	if bold {
		return fs.bold
	}
	return fs.regular
}

func ppem(size int) fixed.Int26_6 { return fixed.Int26_6(size << 6) }

// has reports whether the font maps r to a real glyph (not .notdef).
func (fs *fontSet) has(bold bool, r rune) bool {
	gi, err := fs.font(bold).GlyphIndex(&fs.buf, r)
	return err == nil && gi != 0
}

// measure is the advance width of s in pixels, rounded.
func (fs *fontSet) measure(s string, size int, bold bool) int {
	f := fs.font(bold)
	var w fixed.Int26_6
	prev := sfnt.GlyphIndex(0)
	for i, r := range s {
		gi, err := f.GlyphIndex(&fs.buf, r)
		if err != nil {
			continue
		}
		if i > 0 {
			if k, err := f.Kern(&fs.buf, prev, gi, ppem(size), font.HintingNone); err == nil {
				w += k
			}
		}
		if adv, err := f.GlyphAdvance(&fs.buf, gi, ppem(size), font.HintingNone); err == nil {
			w += adv
		}
		prev = gi
	}
	return w.Round()
}

// fit returns s, or s cut at a rune boundary with "…" appended, so that it
// is at most maxW pixels wide. It may return "" when not even "…" fits.
func (fs *fontSet) fit(s string, size int, bold bool, maxW int) string {
	if fs.measure(s, size, bold) <= maxW {
		return s
	}
	runes := []rune(s)
	// Binary search on the rune count that still fits with the ellipsis.
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fs.measure(strings.TrimRight(string(runes[:mid]), " ")+"…", size, bold) <= maxW {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	out := strings.TrimRight(string(runes[:lo]), " ") + "…"
	if fs.measure(out, size, bold) > maxW {
		return ""
	}
	return out
}

// draw paints s with its baseline origin at (x, y) and returns the pixel
// width drawn. Glyph origins snap to whole pixels.
func (fs *fontSet) draw(cv *canvas, s string, x, y, size int, bold bool, c color.NRGBA) int {
	f := fs.font(bold)
	pen := fixed.I(x)
	prev := sfnt.GlyphIndex(0)
	for i, r := range s {
		gi, err := f.GlyphIndex(&fs.buf, r)
		if err != nil {
			continue
		}
		if i > 0 {
			if k, err := f.Kern(&fs.buf, prev, gi, ppem(size), font.HintingNone); err == nil {
				pen += k
			}
		}
		if g := fs.glyph(bold, size, gi); g != nil {
			ox, oy := pen.Round()+g.dx, y+g.dy
			b := g.mask.Bounds()
			for my := b.Min.Y; my < b.Max.Y; my++ {
				for mx := b.Min.X; mx < b.Max.X; mx++ {
					if a := g.mask.AlphaAt(mx, my).A; a != 0 {
						cv.blend(ox+mx, oy+my, c, uint32(a))
					}
				}
			}
		}
		if adv, err := f.GlyphAdvance(&fs.buf, gi, ppem(size), font.HintingNone); err == nil {
			pen += adv
		}
		prev = gi
	}
	return (pen - fixed.I(x)).Round()
}

// glyph rasterises one glyph at the pen origin, or returns it from the
// per-render cache. nil for an empty glyph (a space) or one too large for
// the fixed-point rasteriser.
func (fs *fontSet) glyph(bold bool, size int, gi sfnt.GlyphIndex) *glyphMask {
	key := glyphKey{bold, size, gi}
	if g, ok := fs.glyphs[key]; ok {
		return g
	}
	g := fs.rasterise(bold, size, gi)
	fs.glyphs[key] = g
	return g
}

func (fs *fontSet) rasterise(bold bool, size int, gi sfnt.GlyphIndex) *glyphMask {
	segs, err := fs.font(bold).LoadGlyph(&fs.buf, gi, ppem(size), nil)
	if err != nil || len(segs) == 0 {
		return nil
	}
	// Bounds in 26.6, from every control point (a hull of the outline).
	minX, minY := fixed.Int26_6(math.MaxInt32), fixed.Int26_6(math.MaxInt32)
	maxX, maxY := fixed.Int26_6(math.MinInt32), fixed.Int26_6(math.MinInt32)
	for _, s := range segs {
		for _, p := range s.Args[:argCount(s.Op)] {
			minX, maxX = min(minX, p.X), max(maxX, p.X)
			minY, maxY = min(minY, p.Y), max(maxY, p.Y)
		}
	}
	x0, y0 := minX.Floor(), minY.Floor()
	w, h := maxX.Ceil()-x0, maxY.Ceil()-y0
	if w <= 0 || h <= 0 || w > 500 || h > 500 {
		return nil
	}
	ox, oy := fixed.I(x0), fixed.I(y0)
	// pt is a 26.6 point relative to the raster origin, as float32 pixels:
	// an integer over 64, exact in float32 for any glyph that fits.
	pt := func(p fixed.Point26_6) (float32, float32) {
		return float32(p.X-ox) / 64, float32(p.Y-oy) / 64
	}
	z := vector.NewRasterizer(w, h)
	z.DrawOp = draw.Src
	var cur fixed.Point26_6
	open := false
	for _, s := range segs {
		switch s.Op {
		case sfnt.SegmentOpMoveTo:
			if open {
				z.ClosePath()
			}
			z.MoveTo(pt(s.Args[0]))
			cur, open = s.Args[0], true
		case sfnt.SegmentOpLineTo:
			z.LineTo(pt(s.Args[0]))
			cur = s.Args[0]
		case sfnt.SegmentOpQuadTo:
			flattenQuad(z, cur, s.Args[0], s.Args[1], ox, oy)
			cur = s.Args[1]
		case sfnt.SegmentOpCubeTo:
			flattenCube(z, cur, s.Args[0], s.Args[1], s.Args[2], ox, oy)
			cur = s.Args[2]
		}
	}
	if open {
		z.ClosePath()
	}
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	return &glyphMask{mask: mask, dx: x0, dy: y0}
}

// argCount is how many points a segment of this op carries.
func argCount(op sfnt.SegmentOp) int {
	switch op {
	case sfnt.SegmentOpQuadTo:
		return 2
	case sfnt.SegmentOpCubeTo:
		return 3
	}
	return 1
}

// steps is how many line segments approximate a curve whose control
// polygon deviates by dev (26.6 units) from its chord: about a tenth of a
// pixel of error, 1–16 segments. Integer square root, so it is exact.
func steps(dev int64) int64 {
	n := int64(1)
	for n < 16 && n*n*51 < dev {
		n++
	}
	return n
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// flattenQuad emits the quadratic Bézier p0→p2 (control p1) as n line
// segments. Each point is (Σ binomial·point) / n², evaluated in int64 and
// divided once in float32: exact numerator, one correctly rounded divide.
func flattenQuad(z *vector.Rasterizer, p0, p1, p2 fixed.Point26_6, ox, oy fixed.Int26_6) {
	ax, ay := int64(p0.X-ox), int64(p0.Y-oy)
	bx, by := int64(p1.X-ox), int64(p1.Y-oy)
	cx, cy := int64(p2.X-ox), int64(p2.Y-oy)
	n := steps(max(abs64(ax-2*bx+cx), abs64(ay-2*by+cy)))
	den := float32(n * n * 64)
	for i := int64(1); i <= n; i++ {
		j := n - i
		x := j*j*ax + 2*i*j*bx + i*i*cx
		y := j*j*ay + 2*i*j*by + i*i*cy
		z.LineTo(float32(x)/den, float32(y)/den)
	}
}

// flattenCube is flattenQuad for a cubic (CFF outlines; the Go fonts are
// TrueType and never use it).
func flattenCube(z *vector.Rasterizer, p0, p1, p2, p3 fixed.Point26_6, ox, oy fixed.Int26_6) {
	ax, ay := int64(p0.X-ox), int64(p0.Y-oy)
	bx, by := int64(p1.X-ox), int64(p1.Y-oy)
	cx, cy := int64(p2.X-ox), int64(p2.Y-oy)
	dx, dy := int64(p3.X-ox), int64(p3.Y-oy)
	dev := max(abs64(ax-2*bx+cx), abs64(ay-2*by+cy), abs64(bx-2*cx+dx), abs64(by-2*cy+dy))
	n := steps(dev)
	den := float32(n * n * n * 64)
	for i := int64(1); i <= n; i++ {
		j := n - i
		x := j*j*j*ax + 3*i*j*j*bx + 3*i*i*j*cx + i*i*i*dx
		y := j*j*j*ay + 3*i*j*j*by + 3*i*i*j*cy + i*i*i*dy
		z.LineTo(float32(x)/den, float32(y)/den)
	}
}

// ── Names ────────────────────────────────────────────────────────────────

const maxNameRunes = 48

// sanitize drops invalid UTF-8, control and format characters (bidi
// overrides, line separators, zero-width joiners) and collapses runs of
// whitespace. What is left is plain text: the card draws text, never
// markup, so nothing needs escaping.
func sanitize(s string) string {
	var b strings.Builder
	space := false
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		s = s[n:]
		switch {
		case r == utf8.RuneError && n <= 1:
			continue
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == ' ', r == ' ':
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cardName is a node name as the card can draw it, or "" when it cannot:
// a name whose glyphs mostly fall outside the Go fonts (CJK, emoji) would
// print as a row of boxes, so the caller falls back to "Node A". A few
// unmappable runes in an otherwise drawable name are dropped instead.
//
// The name is composed (NFC) first: in NFD a letter and its accent are two
// runes, and the Go fonts have no glyph for most combining marks, so
// "Åbenrå" typed on a keyboard that decomposes would lose its rings.
func (fs *fontSet) cardName(name string) string {
	s := norm.NFC.String(sanitize(name))
	total, missing := 0, 0
	for _, r := range s {
		if r == ' ' {
			continue
		}
		total++
		if !fs.has(true, r) {
			missing++
		}
	}
	if total == 0 || missing*5 > total {
		return ""
	}
	if missing > 0 {
		var b strings.Builder
		for _, r := range s {
			if r == ' ' || fs.has(true, r) {
				b.WriteRune(r)
			}
		}
		s = sanitize(b.String())
	}
	if utf8.RuneCountInString(s) > maxNameRunes {
		s = strings.TrimRight(string([]rune(s)[:maxNameRunes-1]), " ") + "…"
	}
	return s
}

// endpointLabel is what the card calls endpoint i (0 = A): a map point by
// its coordinates (it has no name, whatever the caller passes), a node by
// its drawable name, or "Node A"/"Node B" when names are off or the name
// cannot be drawn.
func (fs *fontSet) endpointLabel(end TerrainEndpoint, name string, i int, hide bool) string {
	tag := "AB"[i : i+1]
	if end.Kind == "point" {
		if !finite(end.Lat) || !finite(end.Lon) {
			return "Point " + tag
		}
		return fmt.Sprintf("%.4f, %.4f", end.Lat, end.Lon)
	}
	if !hide {
		if n := fs.cardName(name); n != "" {
			return n
		}
	}
	return "Node " + tag
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
