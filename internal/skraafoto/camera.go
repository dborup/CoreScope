package skraafoto

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/meshcore-analyzer/dhm"
)

// #737 M3a — where a ground point lies in a photo. Every STAC item carries
// its camera model (the perspective-imagery extension, pers:*), so a point
// in EPSG:25832 with a DVR90 height projects into the photo by the
// collinearity equations, with no upstream call.
//
// The conventions, fixed against the real items in
// testdata/stac-aarhus-dokk1-2025.json (camera_737_m3a_test.go), not
// assumed:
//
//   - pers:rotation_matrix, row-major, turns world offsets into camera
//     coordinates: (u, v, w) = R · (X − C), where C is
//     pers:perspective_center. It equals (Rx(ω)·Ry(φ)·Rz(κ))ᵀ of
//     pers:omega, pers:phi and pers:kappa in degrees to 1e-15 in all four
//     items, and an item where it does not is given no camera.
//   - The camera looks down its −w axis: a point is in front when w < 0.
//   - Image coordinates in mm, x right and y up from the image centre:
//     x = x₀ − f·u/w, y = y₀ − f·v/w, where (x₀, y₀) is
//     principal_point_offset. Its sign comes from the east photo, whose
//     y₀ is 6.68 mm (1777 px): subtracting it instead misses an image
//     corner by 3377 px at worst, each corner's height free in −2…40 m
//     as in the corner test (3230 px over −10…60 m). x₀ is 0 in every
//     item seen, so its sign is taken to follow y₀'s.
//   - Pixels: column = cols/2 + x/sx, row = rows/2 − y/sy, with (sx, sy)
//     pixel_spacing, and (0, 0) the top-left corner of the top-left
//     pixel. proj:shape is [rows, cols] and sensor_array_dimensions
//     [cols, rows]; an item where they disagree is given no camera.
//
// With these, each footprint corner lands within 9.3 full-resolution
// pixels (about 1 m) of its image corner at the height it was draped at,
// which is under 0.7 pixel of a preview.

// Camera is a photo's camera model, validated.
type Camera struct {
	// Center is the perspective centre: E and N in EPSG:25832, H in
	// DVR90, in metres.
	Center [3]float64
	// Rotation is the world-to-camera rotation, row-major.
	Rotation [9]float64
	// FocalMM, PixelMM (x, y) and PrincipalMM (x₀, y₀) are the interior
	// orientation.
	FocalMM     float64
	PixelMM     [2]float64
	PrincipalMM [2]float64
	// Cols and Rows are the full-resolution size.
	Cols, Rows int
	// GroundM is the mean height of the footprint corners
	// (footprintHeight), the fallback for a point whose terrain height is
	// not known; valid only when GroundOK.
	GroundM  float64
	GroundOK bool
}

// Ground is a point in EPSG:25832 with a DVR90 height, in metres.
type Ground struct{ E, N, H float64 }

// Pixel is a position in a preview in pixels: x to the right and y down
// from its top-left corner.
type Pixel struct{ X, Y float64 }

// Project is the full-resolution pixel position of (e, n, h). ok is false
// for a point that is not in front of the camera.
func (c *Camera) Project(e, n, h float64) (col, row float64, ok bool) {
	r := &c.Rotation
	dx, dy, dz := e-c.Center[0], n-c.Center[1], h-c.Center[2]
	u := r[0]*dx + r[1]*dy + r[2]*dz
	v := r[3]*dx + r[4]*dy + r[5]*dz
	w := r[6]*dx + r[7]*dy + r[8]*dz
	if !(w < 0) {
		return 0, 0, false
	}
	x := c.PrincipalMM[0] - c.FocalMM*u/w
	y := c.PrincipalMM[1] - c.FocalMM*v/w
	return float64(c.Cols)/2 + x/c.PixelMM[0], float64(c.Rows)/2 - y/c.PixelMM[1], true
}

// ray is the world direction of the full-resolution pixel position (col,
// row): Project's inverse up to the distance.
func (c *Camera) ray(col, row float64) [3]float64 {
	x := (col-float64(c.Cols)/2)*c.PixelMM[0] - c.PrincipalMM[0]
	y := (float64(c.Rows)/2-row)*c.PixelMM[1] - c.PrincipalMM[1]
	z := -c.FocalMM
	r := &c.Rotation
	// Rᵀ · (x, y, −f)
	return [3]float64{r[0]*x + r[3]*y + r[6]*z, r[1]*x + r[4]*y + r[7]*z, r[2]*x + r[5]*y + r[8]*z}
}

// Frame is the preview this camera's photo is served at.
func (c *Camera) Frame() Frame { return PreviewFrame(c.Cols, c.Rows) }

// At is g in the preview; ok is false when g is not in front of the
// camera. A point in front may still be outside the frame (Contains).
func (c *Camera) At(g Ground) (Pixel, bool) {
	col, row, ok := c.Project(g.E, g.N, g.H)
	if !ok {
		return Pixel{}, false
	}
	return c.Frame().fromFull(col, row), true
}

// lineScan is the number of steps Line scans a line in for the part
// inside the frame: 78 m apart on an 80 km path, against a frame about a
// kilometre across.
const lineScan = 1024

// Line samples the straight line from a to b, lowered by sag(t) metres at
// t ∈ [0, 1] from a (the earth's bulge), where it is in the preview: n
// points (at least 2), evenly spaced from where it enters the frame to
// where it leaves, each inside. nil when no part of it is in frame. The
// ends of the visible part are found to a hundredth of a pixel.
func (c *Camera) Line(a, b Ground, sag func(t float64) float64, n int) []Pixel {
	f := c.Frame()
	at := func(t float64) (Pixel, bool) {
		col, row, ok := c.Project(a.E+(b.E-a.E)*t, a.N+(b.N-a.N)*t, a.H+(b.H-a.H)*t-sag(t))
		p := f.fromFull(col, row)
		return p, ok && f.Contains(p)
	}
	first, last := -1, -1
	for i := 0; i <= lineScan; i++ {
		if _, in := at(float64(i) / lineScan); in {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}
	// edge narrows (out, in] to where the line crosses the frame.
	edge := func(out, in float64) float64 {
		for k := 0; k < 40; k++ {
			mid := (out + in) / 2
			if _, ok := at(mid); ok {
				in = mid
			} else {
				out = mid
			}
		}
		return in
	}
	t0, t1 := float64(first)/lineScan, float64(last)/lineScan
	if first > 0 {
		t0 = edge(float64(first-1)/lineScan, t0)
	}
	if last < lineScan {
		t1 = edge(float64(last+1)/lineScan, t1)
	}
	n = max(2, n)
	out := make([]Pixel, 0, n)
	for i := 0; i < n; i++ {
		if p, ok := at(t0 + (t1-t0)*float64(i)/float64(n-1)); ok {
			out = append(out, p)
		}
	}
	return out
}

// Frame is the size of a photo's preview and the scale to it from full
// resolution.
type Frame struct {
	Width, Height int
	cols, rows    int
}

// PreviewFrame is the preview of a photo of cols × rows px, as Preview
// cuts it: the level previewLevel picks of a pyramid whose overviews
// halve the size (⌈n/2⌉ each, as GDAL builds them and as the real photos
// are laid out: 20544 → 10272 → … → 1284 → 642 → 321), scaled by
// previewSize. GDAL resamples each overview, and downscale the level, by
// the plain ratio of the sizes, so a full-resolution position maps to the
// preview by the ratio of the full and the preview size alone.
func PreviewFrame(cols, rows int) Frame {
	levels := [][2]int{{cols, rows}}
	for l := levels[0]; max(l[0], l[1]) > PreviewMaxPx && l[0] > 1 && l[1] > 1; {
		l = [2]int{(l[0] + 1) / 2, (l[1] + 1) / 2}
		levels = append(levels, l)
	}
	l := levels[previewLevelIndex(len(levels), func(i int) (int, int) { return levels[i][0], levels[i][1] })]
	w, h := previewSize(l[0], l[1])
	return Frame{Width: w, Height: h, cols: cols, rows: rows}
}

// fromFull is the full-resolution position (col, row) in the preview.
func (f Frame) fromFull(col, row float64) Pixel {
	return Pixel{X: col * float64(f.Width) / float64(f.cols), Y: row * float64(f.Height) / float64(f.rows)}
}

// Contains reports whether p is inside the preview, its edges included.
func (f Frame) Contains(p Pixel) bool {
	return p.X >= 0 && p.X <= float64(f.Width) && p.Y >= 0 && p.Y <= float64(f.Height)
}

// ── From the STAC item ──────────────────────────────────────────────────────

// The bounds a camera model must be inside. Anything else is not a photo
// this code understands, and gets no camera rather than a guess.
const (
	pixelCRS        = 25832 // pers:crs: EPSG:25832
	heightCRS       = 5799  // pers:vertical_crs: DVR90
	maxImageSide    = 1 << 17
	maxFocalMM      = 1000
	maxPixelMM      = 1
	maxPrincipalMM  = 100
	maxCameraHeight = 20000
	// rotationTol is how far pers:rotation_matrix may be from the one of
	// ω, φ and κ; the real items agree to 1e-15.
	rotationTol = 1e-6
	// footprintMissM is how far a corner ray may pass beside its
	// footprint corner; footprintMinM and footprintMaxM bound the height.
	footprintMissM = 25
	footprintMinM  = -50
	footprintMaxM  = 500
)

// stacCamera is the part of an item's properties the camera is read
// from. Every field is a pointer, so an absent one is told from a zero.
type stacCamera struct {
	Shape    *[2]float64 `json:"proj:shape"`
	Omega    *float64    `json:"pers:omega"`
	Phi      *float64    `json:"pers:phi"`
	Kappa    *float64    `json:"pers:kappa"`
	Center   *[3]float64 `json:"pers:perspective_center"`
	CRS      *int        `json:"pers:crs"`
	Vertical *int        `json:"pers:vertical_crs"`
	Rotation *[9]float64 `json:"pers:rotation_matrix"`
	Interior *struct {
		Focal     *float64    `json:"focal_length"`
		Spacing   *[2]float64 `json:"pixel_spacing"`
		Principal *[2]float64 `json:"principal_point_offset"`
		Sensor    *[2]float64 `json:"sensor_array_dimensions"`
	} `json:"pers:interior_orientation"`
	// CRSName is the item's crs member, the CRS of its geometry.
	CRSName *struct {
		Properties struct {
			Name string `json:"name"`
		} `json:"properties"`
	} `json:"crs"`
}

// stacPolygon is an item's footprint.
type stacPolygon struct {
	Type        string        `json:"type"`
	Coordinates [][][]float64 `json:"coordinates"`
}

// parseCamera is the camera of an item from its raw properties and
// geometry, or nil when the model is absent or does not hold together. A
// footprint it cannot read only leaves GroundOK false.
func parseCamera(props, geometry json.RawMessage) *Camera {
	var s stacCamera
	if json.Unmarshal(props, &s) != nil || s.Shape == nil || s.Omega == nil || s.Phi == nil || s.Kappa == nil ||
		s.Center == nil || s.CRS == nil || s.Vertical == nil || s.Rotation == nil || s.Interior == nil ||
		s.Interior.Focal == nil || s.Interior.Spacing == nil || s.Interior.Principal == nil || s.Interior.Sensor == nil {
		return nil
	}
	in := s.Interior
	rows, okR := wholeSide(s.Shape[0])
	cols, okC := wholeSide(s.Shape[1])
	c := &Camera{Center: *s.Center, Rotation: *s.Rotation, FocalMM: *in.Focal, PixelMM: *in.Spacing,
		PrincipalMM: *in.Principal, Cols: cols, Rows: rows}
	switch {
	case *s.CRS != pixelCRS || *s.Vertical != heightCRS || !okR || !okC,
		in.Sensor[0] != float64(cols) || in.Sensor[1] != float64(rows),
		!(c.FocalMM > 0 && c.FocalMM <= maxFocalMM),
		!(c.PixelMM[0] > 0 && c.PixelMM[0] <= maxPixelMM && c.PixelMM[1] > 0 && c.PixelMM[1] <= maxPixelMM),
		!(math.Abs(c.PrincipalMM[0]) <= maxPrincipalMM && math.Abs(c.PrincipalMM[1]) <= maxPrincipalMM),
		!(c.Center[2] > 0 && c.Center[2] <= maxCameraHeight) || !finite(c.Center[0]) || !finite(c.Center[1]),
		!rotationMatches(c.Rotation, *s.Omega, *s.Phi, *s.Kappa):
		return nil
	}
	if corners, ok := footprintCorners(geometry, s.CRSName != nil, crsName(&s)); ok {
		c.GroundM, c.GroundOK = c.footprintHeight(corners)
	}
	return c
}

func crsName(s *stacCamera) string {
	if s.CRSName == nil {
		return ""
	}
	return s.CRSName.Properties.Name
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// wholeSide is v as an image side: a whole number of pixels in range.
func wholeSide(v float64) (int, bool) {
	if !(v >= 1 && v <= maxImageSide) || v != math.Trunc(v) {
		return 0, false
	}
	return int(v), true
}

// rotationMatches reports whether r is (Rx(ω)·Ry(φ)·Rz(κ))ᵀ, the angles in
// degrees, entry by entry within rotationTol. An orthonormal r follows.
func rotationMatches(r [9]float64, omega, phi, kappa float64) bool {
	const rad = math.Pi / 180
	so, co := math.Sincos(omega * rad)
	sp, cp := math.Sincos(phi * rad)
	sk, ck := math.Sincos(kappa * rad)
	// Rx(ω)·Ry(φ)·Rz(κ), transposed: its columns are the rows here.
	want := [9]float64{
		cp * ck, co*sk + so*sp*ck, so*sk - co*sp*ck,
		-cp * sk, co*ck - so*sp*sk, so*ck + co*sp*sk,
		sp, -so * cp, co * cp,
	}
	for i := range r {
		if !(math.Abs(r[i]-want[i]) <= rotationTol) {
			return false
		}
	}
	return true
}

// footprintCorners are the footprint polygon's outer corners in
// EPSG:25832, without the closing repeat. The geometry is in the item's
// crs: CRS84 (lon, lat) or EPSG:25832, which the server's searches ask
// for. Without a crs member the coordinates' range decides: no point in
// Denmark is within ±180 in EPSG:25832.
func footprintCorners(geometry json.RawMessage, hasCRS bool, name string) ([][2]float64, bool) {
	var g stacPolygon
	if json.Unmarshal(geometry, &g) != nil || g.Type != "Polygon" || len(g.Coordinates) == 0 {
		return nil, false
	}
	ring := g.Coordinates[0]
	if len(ring) < 4 || len(ring) > 64 {
		return nil, false
	}
	lonLat := true
	for _, p := range ring {
		if len(p) < 2 || !finite(p[0]) || !finite(p[1]) {
			return nil, false
		}
		lonLat = lonLat && math.Abs(p[0]) <= 180 && math.Abs(p[1]) <= 90
	}
	switch {
	case !hasCRS:
	case strings.HasSuffix(name, "/OGC/1.3/CRS84"):
		lonLat = true
	case strings.HasSuffix(name, "/EPSG/0/25832"):
		lonLat = false
	default:
		return nil, false
	}
	out := make([][2]float64, 0, len(ring)-1)
	for _, p := range ring[:len(ring)-1] {
		e, n := p[0], p[1]
		if lonLat {
			e, n = dhm.ToUTM32(p[1], p[0])
		}
		out = append(out, [2]float64{e, n})
	}
	return out, true
}

// footprintHeight is the mean height at which the image corner rays pass
// over the footprint corners: for each footprint corner, the image corner
// whose ray passes nearest it, and the height of the ray there. Each must
// pass within footprintMissM, at a height in range.
func (c *Camera) footprintHeight(corners [][2]float64) (float64, bool) {
	cols, rows := float64(c.Cols), float64(c.Rows)
	var rays [4][3]float64
	for k, ic := range [4][2]float64{{0, 0}, {cols, 0}, {cols, rows}, {0, rows}} {
		rays[k] = c.ray(ic[0], ic[1])
	}
	sum := 0.0
	for _, fc := range corners {
		best, bestH := math.Inf(1), 0.0
		for _, d := range rays {
			dxy := d[0]*d[0] + d[1]*d[1]
			if !(dxy > 0) || !(d[2] < 0) {
				continue
			}
			// The ray's nearest approach to fc in plan.
			s := (d[0]*(fc[0]-c.Center[0]) + d[1]*(fc[1]-c.Center[1])) / dxy
			if !(s > 0) {
				continue
			}
			miss := math.Hypot(c.Center[0]+s*d[0]-fc[0], c.Center[1]+s*d[1]-fc[1])
			if miss < best {
				best, bestH = miss, c.Center[2]+s*d[2]
			}
		}
		if !(best <= footprintMissM && bestH >= footprintMinM && bestH <= footprintMaxM) {
			return 0, false
		}
		sum += bestH
	}
	return sum / float64(len(corners)), true
}
