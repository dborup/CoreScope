package skraafoto

// #737 M3a — the camera model of a photo, against real STAC items: the
// four photos of 2025 (east, north, nadir, south) that contain Dokk1 in
// Aarhus, as the API served them (testdata/stac-aarhus-dokk1-2025.json,
// public data, no token). Their geometry is in CRS84, as a search without
// crs=EPSG:25832 answers.

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

const fixturePath = "testdata/stac-aarhus-dokk1-2025.json"

// Dokk1, the library on the Aarhus harbour front: inside all four photos.
const dokk1Lat, dokk1Lon = 56.15370, 10.21395

// fixtureFeatures are the fixture's features by direction, as raw JSON.
func fixtureFeatures(t testing.TB) map[Direction]string {
	t.Helper()
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fc struct {
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(b, &fc); err != nil {
		t.Fatal(err)
	}
	out := map[Direction]string{}
	for _, f := range fc.Features {
		var d struct {
			Properties struct {
				Direction Direction `json:"direction"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(f, &d); err != nil {
			t.Fatal(err)
		}
		out[d.Properties.Direction] = string(f)
	}
	if len(out) != 4 {
		t.Fatalf("fixture directions %d", len(out))
	}
	return out
}

func fixtureItem(t testing.TB, d Direction) Item {
	t.Helper()
	items, _, err := ParseSearch([]byte(`{"type":"FeatureCollection","features":[`+fixtureFeatures(t)[d]+`]}`), 2025, d)
	if err != nil || len(items) != 1 {
		t.Fatalf("%s: %v %v", d, items, err)
	}
	if items[0].Camera == nil {
		t.Fatalf("%s: no camera", d)
	}
	return items[0]
}

// fixtureCorners are the footprint polygon's corners of photo d in
// EPSG:25832, without the closing repeat.
func fixtureCorners(t testing.TB, d Direction) [][2]float64 {
	var f struct {
		Geometry struct {
			Coordinates [][][2]float64 `json:"coordinates"`
		} `json:"geometry"`
	}
	if err := json.Unmarshal([]byte(fixtureFeatures(t)[d]), &f); err != nil {
		t.Fatal(err)
	}
	ring := f.Geometry.Coordinates[0]
	var out [][2]float64
	for _, c := range ring[:len(ring)-1] {
		e, n := dhm.ToUTM32(c[1], c[0])
		out = append(out, [2]float64{e, n})
	}
	return out
}

var fixtureDirections = []Direction{East, North, Nadir, South}

// The footprint corners project onto the image corners. The footprints
// were draped on the terrain, so each corner has its own height: for
// each corner a height in the plausible range for Aarhus C (−2 to 40 m
// DVR90) must put it within cornerTolPx full-resolution pixels of an
// image corner, and the four must reach four different corners. This is
// what fixes the conventions (Camera's doc); any other choice of matrix
// direction, principal point sign or axes misses by hundreds of pixels.
func TestCamera737_FootprintCornersProjectToImageCorners(t *testing.T) {
	const cornerTolPx = 10 // achieved: 9.3 px (south), at 0.1 m GSD about 1 m
	worst := 0.0
	for _, d := range fixtureDirections {
		misses, used := cornerMisses(t, fixtureItem(t, d).Camera, d, -2, 40)
		for i, m := range misses {
			if m > cornerTolPx {
				t.Errorf("%s: footprint corner %d is %.1f px from the nearest image corner", d, i, m)
			}
			worst = math.Max(worst, m)
		}
		if len(used) != 4 {
			t.Errorf("%s: the footprint corners reach image corners %v", d, used)
		}
	}
	t.Logf("worst corner residual %.2f px at full resolution", worst)
}

// cornerMisses is, for each footprint corner of photo d, how far camera
// c puts it from the nearest image corner at its best height in lo…hi
// metres, in full-resolution pixels, and which image corners they reach.
func cornerMisses(t *testing.T, c *Camera, d Direction, lo, hi float64) ([]float64, map[int]bool) {
	cols, rows := float64(c.Cols), float64(c.Rows)
	imageCorners := [][2]float64{{0, 0}, {cols, 0}, {cols, rows}, {0, rows}}
	used := map[int]bool{}
	var misses []float64
	for _, fc := range fixtureCorners(t, d) {
		best, bestK := math.Inf(1), -1
		for h := lo; h <= hi; h += 0.1 {
			col, row, ok := c.Project(fc[0], fc[1], h)
			if !ok {
				continue
			}
			for k, ic := range imageCorners {
				if dist := math.Hypot(col-ic[0], row-ic[1]); dist < best {
					best, bestK = dist, k
				}
			}
		}
		used[bestK] = true
		misses = append(misses, best)
	}
	return misses, used
}

// The principal point's sign, as Camera's doc gives it: subtracting the
// east photo's y₀ instead misses a corner by 3377 px over −2…40 m and
// 3230 px over −10…60 m (#827 review finding 9).
func TestCamera737_PrincipalPointSignMiss(t *testing.T) {
	c := *fixtureItem(t, East).Camera
	c.PrincipalMM[1] = -c.PrincipalMM[1]
	for _, r := range []struct{ lo, hi, want float64 }{{-2, 40, 3377}, {-10, 60, 3230}} {
		misses, _ := cornerMisses(t, &c, East, r.lo, r.hi)
		worst := 0.0
		for _, m := range misses {
			worst = math.Max(worst, m)
		}
		if math.Abs(worst-r.want) > 1 {
			t.Errorf("%g…%g m: worst miss %.1f px, the doc says %g", r.lo, r.hi, worst, r.want)
		}
	}
}

// A camera does not mirror: the footprint ring seen in the image (rows
// down) turns the other way from the ring on the map (north up). The
// corner test cannot tell an image from its mirror; this can.
func TestCamera737_ImageNotMirrored(t *testing.T) {
	area := func(ps [][2]float64) float64 {
		a := 0.0
		for i := range ps {
			j := (i + 1) % len(ps)
			a += ps[i][0]*ps[j][1] - ps[j][0]*ps[i][1]
		}
		return a
	}
	for _, d := range fixtureDirections {
		c := fixtureItem(t, d).Camera
		ground := fixtureCorners(t, d)
		var image [][2]float64
		for _, g := range ground {
			col, row, ok := c.Project(g[0], g[1], c.GroundM)
			if !ok {
				t.Fatalf("%s: corner behind the camera", d)
			}
			image = append(image, [2]float64{col, row})
		}
		if area(ground)*area(image) >= 0 {
			t.Errorf("%s: the image is mirrored (areas %.0f, %.0f)", d, area(ground), area(image))
		}
	}
}

// The ground point straight below the nadir camera lands on the
// principal point, near the image centre: an independent check of the
// axes on the one photo that looks straight down.
func TestCamera737_NadirLooksDown(t *testing.T) {
	c := fixtureItem(t, Nadir).Camera
	col, row, ok := c.Project(c.Center[0], c.Center[1], 5)
	// ω and φ are about 0.1°: at 2100 m that is a few metres, 0.1 m GSD.
	if !ok || math.Abs(col-float64(c.Cols)/2) > 100 || math.Abs(row-float64(c.Rows)/2) > 100 {
		t.Fatalf("nadir point at %.1f,%.1f of %dx%d (ok %v)", col, row, c.Cols, c.Rows, ok)
	}
	// A point above the camera is behind it.
	if _, _, ok := c.Project(c.Center[0], c.Center[1], c.Center[2]+100); ok {
		t.Fatal("a point behind the camera projected")
	}
}

// The oblique photos look the way they are named: the camera stands on
// the opposite side of the point.
func TestCamera737_DirectionsMatchCameraPositions(t *testing.T) {
	e, n := dhm.ToUTM32(dokk1Lat, dokk1Lon)
	for d, want := range map[Direction][2]float64{East: {-1, 0}, North: {0, -1}, South: {0, 1}} {
		c := fixtureItem(t, d).Camera
		de, dn := c.Center[0]-e, c.Center[1]-n
		if de*want[0]+dn*want[1] < 1000 {
			t.Errorf("%s: camera at %.0f,%.0f from the point", d, de, dn)
		}
	}
}

// Dokk1 is inside all four previews; a point 5 km north is in none.
func TestCamera737_PointInAndOutOfFrame(t *testing.T) {
	e, n := dhm.ToUTM32(dokk1Lat, dokk1Lon)
	fe, fn := dhm.ToUTM32(56.2000, 10.2100)
	for _, d := range fixtureDirections {
		c := fixtureItem(t, d).Camera
		f := c.Frame()
		p, ok := c.At(Ground{E: e, N: n, H: 5})
		if !ok || !f.Contains(p) {
			t.Errorf("%s: Dokk1 at %+v of %dx%d", d, p, f.Width, f.Height)
		}
		if p, ok := c.At(Ground{E: fe, N: fn, H: 5}); ok && f.Contains(p) {
			t.Errorf("%s: a point 5 km away in frame at %+v", d, p)
		}
	}
}

// The preview is cut as Preview cuts it: the smallest overview at least
// PreviewMaxPx on the long edge, scaled to PreviewMaxPx. For the 20544 ×
// 14016 nadir photo that is IFD4, 1284 × 876, and 1024 × 699, the size
// the CDN preview of a real pyramid has (skraafoto_proxy_737_test.go); a
// portrait oblique photo of 10560 × 14144 is cut from 1320 × 1768.
func TestCamera737_PreviewFrame(t *testing.T) {
	for _, c := range []struct{ cols, rows, w, h int }{
		{20544, 14016, 1024, 699}, {10560, 14144, 765, 1024}, {14144, 10560, 1024, 765},
		{800, 600, 800, 600}, {1024, 10, 1024, 10}, {2048, 2048, 1024, 1024}, {2049, 1, 1024, 1},
		// Odd sizes: the level is ⌈n/2⌉ (1025 × 512), not the photo
		// scaled, which would round to 511.
		{2049, 1023, 1024, 512},
	} {
		f := PreviewFrame(c.cols, c.rows)
		if f.Width != c.w || f.Height != c.h {
			t.Errorf("%dx%d: %dx%d, want %dx%d", c.cols, c.rows, f.Width, f.Height, c.w, c.h)
		}
	}
	// The image corners are the preview corners.
	c := fixtureItem(t, East).Camera
	f := c.Frame()
	if p := f.fromFull(float64(c.Cols), float64(c.Rows)); p != (Pixel{X: 765, Y: 1024}) {
		t.Fatalf("%+v", p)
	}
}

// previewLevel and PreviewFrame choose the same level of the pyramid.
func TestCamera737_PreviewFrameMatchesPreviewLevel(t *testing.T) {
	levels := [][2]int{{20544, 14016}, {10272, 7008}, {5136, 3504}, {2568, 1752}, {1284, 876}, {642, 438}, {321, 219}}
	ti := &dhm.TiledImage{}
	for _, l := range levels {
		ti.Levels = append(ti.Levels, dhm.ImageLevel{Width: l[0], Height: l[1], TileWidth: 512, TileHeight: 512})
	}
	l, err := previewLevel(ti)
	if err != nil {
		t.Fatal(err)
	}
	w, h := previewSize(l.Width, l.Height)
	if f := PreviewFrame(20544, 14016); f.Width != w || f.Height != h {
		t.Fatalf("frame %dx%d, preview %dx%d", f.Width, f.Height, w, h)
	}
}

// The footprint height is where the corner rays cross the footprint
// corners: the mean of the heights the footprint was draped at.
func TestCamera737_GroundFromFootprint(t *testing.T) {
	for _, d := range fixtureDirections {
		c := fixtureItem(t, d).Camera
		if !c.GroundOK || c.GroundM < 0 || c.GroundM > 25 {
			t.Errorf("%s: footprint height %.2f (ok %v)", d, c.GroundM, c.GroundOK)
		}
	}
	// The same footprint in EPSG:25832, as a search with crs=EPSG:25832
	// answers, gives the same height.
	raw := fixtureFeatures(t)[East]
	want := fixtureItem(t, East).Camera.GroundM
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	ring := fixtureCorners(t, East)
	ring = append(ring, ring[0])
	geom, _ := json.Marshal(struct {
		Type        string         `json:"type"`
		Coordinates [][][2]float64 `json:"coordinates"`
	}{"Polygon", [][][2]float64{ring}})
	f["geometry"] = geom
	props := strings.Replace(string(f["properties"]), "http://www.opengis.net/def/crs/OGC/1.3/CRS84", "http://www.opengis.net/def/crs/EPSG/0/25832", 1)
	f["properties"] = json.RawMessage(props)
	b, _ := json.Marshal(f)
	items, _, err := ParseSearch([]byte(`{"features":[`+string(b)+`]}`), 2025, East)
	if err != nil || items[0].Camera == nil || !items[0].Camera.GroundOK || math.Abs(items[0].Camera.GroundM-want) > 1e-3 {
		t.Fatalf("EPSG:25832 footprint: %+v %v, want %.3f", items, err, want)
	}
	// Without the crs member the coordinates' range tells them apart.
	props = strings.Replace(props, `"crs"`, `"crs_unused"`, 1)
	f["properties"] = json.RawMessage(props)
	b, _ = json.Marshal(f)
	items, _, err = ParseSearch([]byte(`{"features":[`+string(b)+`]}`), 2025, East)
	if err != nil || !items[0].Camera.GroundOK || math.Abs(items[0].Camera.GroundM-want) > 1e-3 {
		t.Fatalf("footprint without crs: %+v %v", items, err)
	}
}

// A camera model that does not hold together is dropped, not guessed at:
// the photo is still listed, without a camera.
func TestCamera737_InvalidCameraDropped(t *testing.T) {
	raw := fixtureFeatures(t)[North]
	for name, mod := range map[string]func(string) string{
		"kappa off": func(s string) string { return strings.Replace(s, `"pers:kappa": 0.07946`, `"pers:kappa": 1.07946`, 1) },
		"no omega":  func(s string) string { return strings.Replace(s, `"pers:omega": 45.082571, `, ``, 1) },
		"other crs": func(s string) string { return strings.Replace(s, `"pers:crs": 25832`, `"pers:crs": 4326`, 1) },
		"other height": func(s string) string {
			return strings.Replace(s, `"pers:vertical_crs": 5799`, `"pers:vertical_crs": 4979`, 1)
		},
		"crs as text": func(s string) string { return strings.Replace(s, `"pers:crs": 25832`, `"pers:crs": "EPSG:25832"`, 1) },
		"sensor mismatch": func(s string) string {
			return strings.Replace(s, `"sensor_array_dimensions": [14144.0, 10560.0]`, `"sensor_array_dimensions": [10560.0, 14144.0]`, 1)
		},
		"zero focal": func(s string) string { return strings.Replace(s, `"focal_length": 123.38`, `"focal_length": 0`, 1) },
		"no shape":   func(s string) string { return strings.Replace(s, `"proj:shape": [10560.0, 14144.0], `, ``, 1) },
		"fractional shape": func(s string) string {
			return strings.Replace(s, `"proj:shape": [10560.0, 14144.0]`, `"proj:shape": [10560.5, 14144.0]`, 1)
		},
		"short matrix": func(s string) string { return strings.Replace(s, `0.9999971016957945, `, ``, 1) },
		"camera underground": func(s string) string {
			return strings.Replace(s, `[575179.53, 6221491.66, 2117.26]`, `[575179.53, 6221491.66, -2117.26]`, 1)
		},
	} {
		body := mod(raw)
		if body == raw {
			t.Fatalf("%s: the mutation did not apply", name)
		}
		items, _, err := ParseSearch([]byte(`{"features":[`+body+`]}`), 2025, North)
		if err != nil || len(items) != 1 {
			t.Errorf("%s: the photo itself was refused: %v", name, err)
			continue
		}
		if items[0].Camera != nil {
			t.Errorf("%s: camera kept", name)
		}
	}
	// A footprint that is not a polygon only loses the footprint height.
	body := strings.Replace(raw, `"type": "Polygon"`, `"type": "MultiPolygon"`, 1)
	items, _, err := ParseSearch([]byte(`{"features":[`+body+`]}`), 2025, North)
	if err != nil || items[0].Camera == nil || items[0].Camera.GroundOK {
		t.Fatalf("multipolygon footprint: %+v %v", items, err)
	}
}

// The line from a to b in the preview: n points from where it enters the
// frame to where it leaves, each inside; nil when no part is inside.
func TestCamera737_LineClippedToFrame(t *testing.T) {
	c := fixtureItem(t, North).Camera
	f := c.Frame()
	e, n := dhm.ToUTM32(dokk1Lat, dokk1Lon)
	flat := func(float64) float64 { return 0 }

	// From Dokk1 to 5 km west: starts at Dokk1, leaves through the left
	// edge.
	a := Ground{E: e, N: n, H: 20}
	b := Ground{E: e - 5000, N: n, H: 20}
	line := c.Line(a, b, flat, 32)
	if len(line) != 32 {
		t.Fatalf("%d points", len(line))
	}
	start, _ := c.At(a)
	if math.Hypot(line[0].X-start.X, line[0].Y-start.Y) > 1e-6 {
		t.Fatalf("starts at %+v, not at A %+v", line[0], start)
	}
	if last := line[31]; last.X > 0.5 {
		t.Fatalf("does not reach the left edge: %+v", last)
	}
	for i, p := range line {
		if !f.Contains(p) {
			t.Fatalf("point %d %+v outside %dx%d", i, p, f.Width, f.Height)
		}
		if i > 0 && p.X >= line[i-1].X {
			t.Fatalf("not monotone westward at %d", i)
		}
	}
	// Both ends outside, across the frame: enters and leaves at an edge.
	line = c.Line(Ground{E: e + 5000, N: n, H: 20}, b, flat, 32)
	if len(line) != 32 || line[0].X < float64(f.Width)-0.5 || line[31].X > 0.5 {
		t.Fatalf("across: %d points, %+v … %+v", len(line), line[0], line[len(line)-1])
	}
	// Entirely outside: nil.
	if line := c.Line(Ground{E: e, N: n + 9000, H: 20}, Ground{E: e + 1000, N: n + 9000, H: 20}, flat, 32); line != nil {
		t.Fatalf("outside: %+v", line)
	}
	// The sag lowers the line: in a north-looking photo, lower is
	// further down the image.
	sag := func(t float64) float64 { return 30 * 4 * t * (1 - t) }
	straight := c.Line(a, Ground{E: e - 400, N: n, H: 20}, flat, 33)
	sagged := c.Line(a, Ground{E: e - 400, N: n, H: 20}, sag, 33)
	if len(straight) != 33 || len(sagged) != 33 || !(sagged[16].Y > straight[16].Y+5) || math.Abs(sagged[0].Y-straight[0].Y) > 1e-6 {
		t.Fatalf("sag: %+v vs %+v", sagged[16], straight[16])
	}
}
