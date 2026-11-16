package packetcard

import (
	"image"
	"math"

	"github.com/meshcore-analyzer/cardkit"
)

// The map is an equirectangular schematic of the visible positions, with
// no basemap (#812 M1). Positions become integer microdegrees as they
// come in; everything after that is integer arithmetic, so a golden made
// on arm64 matches amd64 CI (cardkit's TestNoFusedMultiplyAdd explains
// why floats are kept out of drawing). The one float step, the cos of the
// middle latitude, is rounded to 1/65536 before use.

const (
	micro = 1_000_000
	// minSpan is the smallest span the map shows either way, about 2 km
	// (a degree of latitude is 111.32 km), so a two-point path is not
	// drawn as a line across the whole card.
	minSpanKm = 2.0
	minSpan   = 17966 // minSpanKm / 111.32 km in microdegrees
	cosOne    = 1 << 16
)

// upt is a position in microdegrees, or after local a point in
// microdegree-sized units east (lon) and south (lat) of the map's corner.
type upt struct{ lat, lon int64 }

// toMicro converts a drawable position; NaN, infinite and out-of-range
// positions are refused.
func toMicro(p LatLon) (upt, bool) {
	if !(p.Lat >= -90 && p.Lat <= 90 && p.Lon >= -180 && p.Lon <= 180) {
		return upt{}, false
	}
	return upt{int64(math.Round(p.Lat * micro)), int64(math.Round(p.Lon * micro))}, true
}

type projection struct {
	// shift moves western longitudes a turn east, for a path across the
	// antimeridian.
	shift bool
	// minLon, maxLat are the corner of the shown span; k is cos(middle
	// latitude) in 1/65536; xOff, yOff centre a span widened to minSpan.
	minLon, maxLat, k, xOff, yOff int64
	// A local unit is num/den fixed-point pixels; (ox, oy) is the corner
	// in fixed-point pixels.
	num, den, ox, oy int64
}

// newProjection fits the positions us (at least one) into the map's inner
// box, centred, north up, keeping their aspect.
func newProjection(us []upt) projection {
	var p projection
	minLon, maxLon, minLat, maxLat := lonBounds(us, false)
	if maxLon-minLon > 180*micro {
		if a, b, _, _ := lonBounds(us, true); b-a < maxLon-minLon {
			p.shift, minLon, maxLon = true, a, b
		}
	}
	mid := float64((minLat+maxLat)/2) / micro
	p.k = max(int64(math.Round(math.Cos(mid*math.Pi/180)*cosOne)), 1)
	p.minLon, p.maxLat = minLon, maxLat
	spanX, spanY := (maxLon-minLon)*p.k/cosOne, maxLat-minLat
	if spanX < minSpan {
		p.xOff, spanX = (minSpan-spanX)/2, minSpan
	}
	if spanY < minSpan {
		p.yOff, spanY = (minSpan-spanY)/2, minSpan
	}
	iw, ih := int64(innerW()), int64(innerH())
	if iw*spanY <= ih*spanX {
		p.num, p.den = iw*cardkit.FracOne, spanX
	} else {
		p.num, p.den = ih*cardkit.FracOne, spanY
	}
	p.ox = int64(innerLeft)*cardkit.FracOne + (iw*cardkit.FracOne-spanX*p.num/p.den)/2
	p.oy = int64(innerTop)*cardkit.FracOne + (ih*cardkit.FracOne-spanY*p.num/p.den)/2
	return p
}

func lonBounds(us []upt, shift bool) (minLon, maxLon, minLat, maxLat int64) {
	minLon, minLat = math.MaxInt64, math.MaxInt64
	maxLon, maxLat = math.MinInt64, math.MinInt64
	for _, u := range us {
		lon := shifted(u.lon, shift)
		minLon, maxLon = min(minLon, lon), max(maxLon, lon)
		minLat, maxLat = min(minLat, u.lat), max(maxLat, u.lat)
	}
	return
}

func shifted(lon int64, shift bool) int64 {
	if shift && lon < 0 {
		return lon + 360*micro
	}
	return lon
}

// local is u in the map's local units: east and south of its corner,
// within ±5.4·10⁸ for any position on Earth.
func (p projection) local(u upt) upt {
	return upt{lon: (shifted(u.lon, p.shift)-p.minLon)*p.k/cosOne + p.xOff, lat: p.maxLat - u.lat + p.yOff}
}

// pixel is a local point in fixed-point pixels. A point inside the shown
// span lands in the inner box; one far outside may not fit an int32 and
// must be clipped first (clip).
func (p projection) pixel(l upt) image.Point {
	return image.Point{int(p.ox + l.lon*p.num/p.den), int(p.oy + l.lat*p.num/p.den)}
}

// snap moves a fixed-point pixel to the centre of its pixel, where
// cardkit's discs are centred, and returns that pixel too.
func snap(fp image.Point) (image.Point, image.Point) {
	px := image.Point{fp.X >> cardkit.FracBits, fp.Y >> cardkit.FracBits}
	return image.Point{px.X<<cardkit.FracBits + cardkit.FracOne/2, px.Y<<cardkit.FracBits + cardkit.FracOne/2}, px
}

// clipRect is the map's rectangle in local units, inset by clipInset px
// so a line along its edge stays inside it.
func (p projection) clipRect() (x0, y0, x1, y1 int64) {
	toLocal := func(px int, o int64) int64 { return (int64(px)*cardkit.FracOne - o) * p.den / p.num }
	return toLocal(safeMargin+clipInset, p.ox), toLocal(mapTop+clipInset, p.oy),
		toLocal(Width-safeMargin-clipInset, p.ox), toLocal(mapBottom-clipInset, p.oy)
}

const clipInset = 2

// clip cuts the segment a–b (local units) to the rectangle, Cohen–
// Sutherland in integers: every product is of two local differences, at
// most about 1.1·10⁹ each, well inside int64.
func clip(a, b upt, x0, y0, x1, y1 int64) (upt, upt, bool) {
	code := func(u upt) int {
		c := 0
		switch {
		case u.lon < x0:
			c |= 1
		case u.lon > x1:
			c |= 2
		}
		switch {
		case u.lat < y0:
			c |= 4
		case u.lat > y1:
			c |= 8
		}
		return c
	}
	ca, cb := code(a), code(b)
	for i := 0; i < 8; i++ {
		switch {
		case ca|cb == 0:
			return a, b, true
		case ca&cb != 0:
			return a, b, false
		}
		out := ca
		if out == 0 {
			out = cb
		}
		var u upt
		switch {
		case out&1 != 0:
			u = upt{lon: x0, lat: a.lat + (b.lat-a.lat)*(x0-a.lon)/(b.lon-a.lon)}
		case out&2 != 0:
			u = upt{lon: x1, lat: a.lat + (b.lat-a.lat)*(x1-a.lon)/(b.lon-a.lon)}
		case out&4 != 0:
			u = upt{lat: y0, lon: a.lon + (b.lon-a.lon)*(y0-a.lat)/(b.lat-a.lat)}
		default:
			u = upt{lat: y1, lon: a.lon + (b.lon-a.lon)*(y1-a.lat)/(b.lat-a.lat)}
		}
		if out == ca {
			a, ca = u, code(u)
		} else {
			b, cb = u, code(u)
		}
	}
	return a, b, false
}

// decimate keeps at most n of pts, evenly spaced, the first and last
// always; it reads only the points it keeps.
func decimate(pts []LatLon, n int) []LatLon {
	if len(pts) <= n {
		return pts
	}
	if n < 2 {
		return pts[:n]
	}
	out := make([]LatLon, n)
	for i := range out {
		out[i] = pts[i*(len(pts)-1)/(n-1)]
	}
	return out
}
