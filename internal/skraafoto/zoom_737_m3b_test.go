package skraafoto

// #737 M3b — a zoomed crop around the point: a box of at most
// PreviewMaxPx cut 1:1 from a finer overview (IFD2 of a real photo,
// about 0.3 m/px against the preview's 1.1), read by range reads of only
// the tiles under the box. Every test runs against the skraafototest
// fake CDN: no network, no token.

import (
	"context"
	"errors"
	"image"
	"math"
	"net/http"
	"strconv"
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

// zoomColor is a smooth field over IFD2 of the real pyramid (5136 ×
// 3504): red across, green down, blue by level. A crop's pixel says
// where in the level it came from to within a few pixels.
func zoomColor(level, x, y int) [4]uint8 {
	return [4]uint8{uint8(x * 255 / 5136), uint8(y * 255 / 3504), uint8(60 + 20*level), 255}
}

// zoomCOG is a photo with the real pyramid whose IFD2 is real JPEG in
// zoomColor, IFD4 (the preview's) too; every other level is filler that
// fails to decode.
func zoomCOG(mod func(*dhmtest.ImageCOGOptions)) []byte {
	o := dhmtest.ImageCOGOptions{Levels: dhmtest.SkraafotoLevels, Color: zoomColor, Pad: 1 << 20,
		Encode: func(l int) bool { return l == 2 || l == 4 }}
	if mod != nil {
		mod(&o)
	}
	return dhmtest.ImageCOG(o)
}

// realZoom is the ZoomFrame of a real photo: its preview is 1024 × 699
// (IFD4 scaled), its crops come from IFD2.
var realZoom = ZoomFrame{PreviewWidth: 1024, PreviewHeight: 699, Level: 2, Width: 5136, Height: 3504, TileWidth: 512, TileHeight: 512}

func openZoom(t testing.TB, u *skraafototest.Upstream) (*CDN, *Zoom) {
	t.Helper()
	c := newTestCDN(t, u, nil)
	z, err := c.Zoom(context.Background(), previewPath)
	if err != nil {
		t.Fatal(err)
	}
	return c, z
}

// The crop level is the smallest at least ZoomFactor times the preview
// level's long edge: IFD2 (5136 px) of a real photo, whose preview level
// is IFD4 (1284 px). The frame is the preview's, as Preview serves it.
func TestZoom_PicksTheLevelFourTimesThePreview(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, zoomCOG(nil))
	_, z := openZoom(t, u)
	if z.Frame != realZoom {
		t.Fatalf("frame %+v, want %+v", z.Frame, realZoom)
	}
	if n := len(u.Log()); n != 1 {
		t.Fatalf("%d reads to open, want the header only", n)
	}
	// A photo without a level that fine is cropped from its largest.
	for name, c := range map[string]struct {
		levels [][2]int
		want   ZoomFrame
	}{
		"small":      {[][2]int{{1500, 1000}, {750, 500}}, ZoomFrame{1024, 683, 0, 1500, 1000, 512, 512}},
		"one level":  {[][2]int{{800, 600}}, ZoomFrame{800, 600, 0, 800, 600, 512, 512}},
		"factor two": {[][2]int{{4000, 2000}, {2000, 1000}, {1000, 500}}, ZoomFrame{1024, 512, 0, 4000, 2000, 512, 512}},
	} {
		u := skraafototest.New()
		u.Put(previewPath, dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: c.levels, Pad: 1 << 16}))
		_, z := openZoom(t, u)
		if z.Frame != c.want {
			t.Errorf("%s: %+v, want %+v", name, z.Frame, c.want)
		}
		u.Close()
	}
}

// Box centres the crop on the point, its origin on the half-tile grid,
// PreviewMaxPx on the long edge in the level's aspect; Marker is the
// point in the crop and InPreview the crop in the preview.
func TestZoomBox_CentresOnThePoint(t *testing.T) {
	// (560, 300) is level pixel (2808.75, 1503.86): the box origin
	// (2296.75, 1154.36) snaps to (2304, 1280).
	b, err := realZoom.Box(560, 300)
	if err != nil {
		t.Fatal(err)
	}
	if b != (CropBox{X: 2304, Y: 1280, W: 1024, H: 699}) {
		t.Fatalf("box %+v", b)
	}
	m := realZoom.Marker(b, 560, 300)
	if math.Abs(m.X-504.75) > 1e-9 || math.Abs(m.Y-(300*3504.0/699-1280)) > 1e-9 {
		t.Fatalf("marker %+v", m)
	}
	lo, hi := realZoom.InPreview(b)
	if math.Abs(lo.X-2304*1024.0/5136) > 1e-9 || math.Abs(lo.Y-1280*699.0/3504) > 1e-9 ||
		math.Abs(hi.X-3328*1024.0/5136) > 1e-9 || math.Abs(hi.Y-1979*699.0/3504) > 1e-9 {
		t.Fatalf("in preview %+v %+v", lo, hi)
	}
	if n := realZoom.tilesIn(b); n != 6 {
		t.Fatalf("%d tiles, want 3 × 2", n)
	}
	// Points that snap to the same origin share the box: the cache key.
	if b2, _ := realZoom.Box(565, 305); b2 != b {
		t.Fatalf("a nearby point: %+v", b2)
	}
	if b3, _ := realZoom.Box(620, 300); b3 == b {
		t.Fatal("a point 300 level px away shares the box")
	}
}

// Near the border the box is moved inside the level, not cut: the point
// is then off centre, up to the frame's edge, and still in the crop.
func TestZoomBox_EdgeClipping(t *testing.T) {
	for _, c := range []struct {
		x, y   float64
		want   CropBox
		marker Pixel
	}{
		{0, 0, CropBox{0, 0, 1024, 699}, Pixel{0, 0}},
		{1024, 699, CropBox{4112, 2805, 1024, 699}, Pixel{1024, 699}},
		{1024, 0, CropBox{4112, 0, 1024, 699}, Pixel{1024, 0}},
		{0, 699, CropBox{0, 2805, 1024, 699}, Pixel{0, 699}},
		{30, 680, CropBox{0, 2805, 1024, 699}, Pixel{30 * 5136.0 / 1024, 680*3504.0/699 - 2805}},
	} {
		b, err := realZoom.Box(c.x, c.y)
		if err != nil || b != c.want {
			t.Errorf("(%v, %v): %+v %v, want %+v", c.x, c.y, b, err, c.want)
			continue
		}
		if m := realZoom.Marker(b, c.x, c.y); math.Abs(m.X-c.marker.X) > 1e-9 || math.Abs(m.Y-c.marker.Y) > 1e-9 {
			t.Errorf("(%v, %v): marker %+v, want %+v", c.x, c.y, m, c.marker)
		}
	}
	for _, p := range [][2]float64{{-0.01, 10}, {10, -0.01}, {1024.01, 10}, {10, 699.01}, {math.NaN(), 10}, {10, math.Inf(1)}} {
		if _, err := realZoom.Box(p[0], p[1]); !errors.Is(err, ErrOutsideFrame) {
			t.Errorf("%v: %v, want ErrOutsideFrame", p, err)
		}
	}
	if _, err := (ZoomFrame{}).Box(0, 0); err == nil {
		t.Error("a zero frame gave a box")
	}
}

// checkCrop decodes a crop of box b and compares it with zoomColor over
// the level, sampling the middle of every 8 × 8 block the source
// (DCJPEG, flat blocks) holds.
func checkCrop(t *testing.T, out []byte, b CropBox, tol int) image.Image {
	t.Helper()
	img := decodePreview(t, out)
	if img.Bounds().Dx() != b.W || img.Bounds().Dy() != b.H {
		t.Fatalf("crop %v, want %d × %d", img.Bounds(), b.W, b.H)
	}
	bad := 0
	for y := 4; y < b.H; y += 37 {
		for x := 4; x < b.W; x += 29 {
			lx, ly := b.X+x, b.Y+y
			if lx%8 < 2 || lx%8 > 5 || ly%8 < 2 || ly%8 > 5 {
				continue // near a block edge, where re-encoding rings
			}
			want := zoomColor(2, lx&^7, ly&^7)
			r, g, bl, _ := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y).RGBA()
			if !near(uint8(r>>8), want[0], tol) || !near(uint8(g>>8), want[1], tol) || !near(uint8(bl>>8), want[2], tol) {
				if bad++; bad < 5 {
					t.Errorf("crop (%d, %d) = level (%d, %d): %d %d %d, want %v", x, y, lx, ly, r>>8, g>>8, bl>>8, want)
				}
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d pixels off", bad)
	}
	return img
}

// The crop shows the level under the box, for the 4-sample photos
// (inverted CMYK), RGB and YCbCr tiles, with the box starting inside a
// tile; and the marker sits on the point.
func TestCrop_Pixels(t *testing.T) {
	for _, c := range []struct {
		name         string
		samples, pho int
		tol          int
	}{{"rgb+band", 4, 2, 6}, {"rgb", 3, 2, 6}, {"ycbcr", 3, 6, 10}} {
		t.Run(c.name, func(t *testing.T) {
			u := skraafototest.New()
			defer u.Close()
			u.Put(previewPath, zoomCOG(func(o *dhmtest.ImageCOGOptions) { o.Samples, o.Photometric = c.samples, c.pho }))
			cdn, z := openZoom(t, u)
			b, err := z.Frame.Box(560, 300)
			if err != nil {
				t.Fatal(err)
			}
			if b.X%512 == 0 || b.Y%512 == 0 {
				t.Fatalf("box %+v starts on a tile edge: the test would not see a tile offset", b)
			}
			out, err := cdn.Crop(context.Background(), z, b)
			if err != nil {
				t.Fatal(err)
			}
			img := checkCrop(t, out, b, c.tol)
			m := z.Frame.Marker(b, 560, 300)
			want := zoomColor(2, int(b.X)+int(m.X), int(b.Y)+int(m.Y))
			r, g, _, _ := img.At(int(m.X), int(m.Y)).RGBA()
			if !near(uint8(r>>8), want[0], 8) || !near(uint8(g>>8), want[1], 8) {
				t.Fatalf("under the marker %d %d, want %v", r>>8, g>>8, want)
			}
		})
	}
}

// A crop at the far corner is moved inside the level and reads its last,
// partial tiles; the pixels are still the level's.
func TestCrop_EdgeClipping(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, zoomCOG(nil))
	cdn, z := openZoom(t, u)
	for _, p := range [][2]float64{{1020, 695}, {0, 0}, {1024, 0}} {
		b, err := z.Frame.Box(p[0], p[1])
		if err != nil {
			t.Fatal(err)
		}
		if b.X < 0 || b.Y < 0 || b.X+b.W > 5136 || b.Y+b.H > 3504 || b.W != 1024 || b.H != 699 {
			t.Fatalf("%v: box %+v", p, b)
		}
		out, err := cdn.Crop(context.Background(), z, b)
		if err != nil {
			t.Fatalf("%v: %v", p, err)
		}
		checkCrop(t, out, b, 6)
	}
}

// tileRanges are the byte ranges of tiles of level l, by index.
func tileRanges(t *testing.T, file []byte, level int) *dhm.ImageLevel {
	t.Helper()
	layout, err := dhm.ParseTiledImage(file, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	return &layout.Levels[level]
}

// A crop reads the header and the tiles under its box: every read a
// range inside the box's tiles, one per tile row at most, no token, the
// preview's byte budget; and it decodes those tiles only (every other
// tile of IFD2 is made undecodable).
func TestCrop_ReadsOnlyTheBoxTiles(t *testing.T) {
	b, _ := realZoom.Box(560, 300)
	cropReadsOnlyBoxTiles(t, b)
}

// A box whose right edge is a tile edge reads and decodes no tile of the
// next column: the tile range ends at the box's last pixel, not one past
// it (v0.2.9 review nit, #829 N1).
func TestCrop_BoxEndingOnATileEdge(t *testing.T) {
	var b CropBox
	found := false
	for x := 0.0; x <= 1024 && !found; x += 0.5 {
		b, _ = realZoom.Box(x, 300)
		found = b.X > 0 && (b.X+b.W)%512 == 0 && b.X+b.W < realZoom.Width
	}
	if !found {
		t.Fatal("no box ends on a tile edge")
	}
	if n := realZoom.tilesIn(b); n != 2*2 {
		t.Fatalf("box %+v: %d tiles, want 2 across × 2 down", b, n)
	}
	cropReadsOnlyBoxTiles(t, b)
}

func cropReadsOnlyBoxTiles(t *testing.T, b CropBox) {
	t.Helper()
	inBox := map[int]bool{}
	for ty := b.Y / 512; ty <= (b.Y+b.H-1)/512; ty++ {
		for tx := b.X / 512; tx <= (b.X+b.W-1)/512; tx++ {
			inBox[ty*11+tx] = true
		}
	}
	file := zoomCOG(func(o *dhmtest.ImageCOGOptions) {
		o.Block = func(level, i int, raw []byte) []byte {
			if level == 2 && !inBox[i] {
				return []byte("not-a-jpeg-tile!")
			}
			return raw
		}
	})
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, file)
	cdn, z := openZoom(t, u)
	out, err := cdn.Crop(context.Background(), z, b)
	if err != nil {
		t.Fatal(err)
	}
	checkCrop(t, out, b, 6)
	l2 := tileRanges(t, file, 2)
	lo, hi := int64(len(file)), int64(0)
	for i := range inBox {
		off, n := l2.TileRange(i)
		lo, hi = min(lo, off), max(hi, off+n)
	}
	log := u.Log()
	if len(log) < 2 || len(log) > 1+2 {
		t.Fatalf("%d requests: the header and one read per tile row (2)", len(log))
	}
	var read int64
	for i, r := range log {
		m := rangeHdrRe.FindStringSubmatch(r.Header.Get("Range"))
		if r.Host != CDNHost || r.Method != "GET" || r.Path != previewPath || r.RawQuery != "" || m == nil {
			t.Fatalf("request %d: %s %s%s?%s Range %q", i, r.Method, r.Host, r.Path, r.RawQuery, r.Header.Get("Range"))
		}
		for k := range r.Header {
			if http.CanonicalHeaderKey(k) == "Token" {
				t.Fatal("a token header went to the CDN")
			}
		}
		from, _ := strconv.ParseInt(m[1], 10, 64)
		to, _ := strconv.ParseInt(m[2], 10, 64)
		read += to - from + 1
		if i > 0 && (from < lo || to >= hi) {
			t.Fatalf("read %d-%d outside the box's tiles [%d, %d)", from, to, lo, hi)
		}
	}
	if read > MaxPreviewBytes || u.CDNBytes.Load() > MaxPreviewBytes {
		t.Fatalf("%d bytes read", read)
	}
}

// The tile budget: a crop never decodes more than MaxZoomTiles tiles. A
// photo whose box could need more (384 px tiles: up to 4 × 3; its
// preview level is still within the preview's bounds) is refused
// after the header, before any tile is read; a box Crop is handed that
// is not one Box gives is refused too.
func TestCrop_TileBudget(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, zoomCOG(func(o *dhmtest.ImageCOGOptions) { o.Tile = 384 }))
	_, err := newTestCDN(t, u, nil).Zoom(context.Background(), previewPath)
	if dhm.FailureReason(err) != "no zoom level" {
		t.Fatalf("384 px tiles: %v", err)
	}
	if n := len(u.Log()); n != 1 {
		t.Fatalf("%d reads, want the header only", n)
	}

	u2 := skraafototest.New()
	defer u2.Close()
	u2.Put(previewPath, zoomCOG(nil))
	cdn, z := openZoom(t, u2)
	for name, b := range map[string]CropBox{
		"wider than the preview": {X: 0, Y: 0, W: 2048, H: 699},
		"outside the level":      {X: 4500, Y: 0, W: 1024, H: 699},
		"negative":               {X: -1, Y: 0, W: 1024, H: 699},
		"empty":                  {X: 0, Y: 0, W: 0, H: 699},
	} {
		if _, err := cdn.Crop(context.Background(), z, b); !errors.Is(err, dhm.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(u2.Log()); n != 1 {
		t.Fatalf("%d reads for refused boxes", n)
	}
}

// The byte budget is the preview's, header included: tiles that would
// take more are refused before they are read.
func TestCrop_ByteBudget(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, zoomCOG(func(o *dhmtest.ImageCOGOptions) {
		o.Block = func(level, i int, raw []byte) []byte {
			if level == 2 {
				return append(raw, make([]byte, 700<<10)...) // 6 × 700 KiB > 4 MiB
			}
			return raw
		}
	}))
	cdn, z := openZoom(t, u)
	b, _ := z.Frame.Box(560, 300)
	if _, err := cdn.Crop(context.Background(), z, b); dhm.FailureReason(err) != "preview too large" {
		t.Fatalf("%v", err)
	}
	if n := len(u.Log()); n != 1 {
		t.Fatalf("%d reads, want the header only", n)
	}
}

// The header's bytes count against a crop's budget: tiles that fit
// MaxPreviewBytes on their own, but not with the header read before
// them, are refused before they are read (v0.2.9 review nit, #829 N3).
func TestCrop_ByteBudgetCountsTheHeader(t *testing.T) {
	b, _ := realZoom.Box(560, 300)
	x0, x1, y0, y1 := realZoom.tileSpan(b)
	padded := func(pad int) []byte {
		return zoomCOG(func(o *dhmtest.ImageCOGOptions) {
			o.Block = func(level, i int, raw []byte) []byte {
				if level == 2 {
					return append(raw, make([]byte, pad)...)
				}
				return raw
			}
		})
	}
	boxBytes := func(file []byte) (n int64) {
		l2 := tileRanges(t, file, 2)
		for ty := y0; ty <= y1; ty++ {
			for tx := x0; tx <= x1; tx++ {
				_, k := l2.TileRange(ty*l2.TilesAcross() + tx)
				n += k
			}
		}
		return n
	}
	tiles := int64((x1 - x0 + 1) * (y1 - y0 + 1))
	raw := boxBytes(padded(0))
	pad := (MaxPreviewBytes - raw) / tiles
	file := padded(int(pad))
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, file)
	cdn, z := openZoom(t, u)
	if got := boxBytes(file); got > MaxPreviewBytes || got <= int64(MaxPreviewBytes-z.used) {
		t.Fatalf("setup: box tiles %d bytes, want in (%d, %d]", got, MaxPreviewBytes-z.used, MaxPreviewBytes)
	}
	if _, err := cdn.Crop(context.Background(), z, b); dhm.FailureReason(err) != "preview too large" {
		t.Fatalf("%v", err)
	}
	if n := len(u.Log()); n != 1 {
		t.Fatalf("%d reads, want the header only", n)
	}
}

// A level only a little larger than a crop has fewer tiles than a crop's
// size alone suggests: maxTiles is clamped to the level's own tile count,
// so a 1100 × 700 level on 384 px tiles (3 × 2 tiles) zooms, where the
// unclamped 4 × 3 would refuse it (v0.2.9 review nit, #829 N4).
func TestZoomFrame_MaxTilesClampedToTheLevel(t *testing.T) {
	f := ZoomFrame{PreviewWidth: 1024, PreviewHeight: 652, Width: 1100, Height: 700, TileWidth: 384, TileHeight: 384}
	if n := f.maxTiles(); n != 3*2 || n > MaxZoomTiles {
		t.Fatalf("maxTiles %d, want 6 (3 × 2, the whole level)", n)
	}
}

// The strict range checks of the preview hold for a crop's tile reads.
func TestCrop_StrictRanges(t *testing.T) {
	file := zoomCOG(nil)
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, file)
	cdn, z := openZoom(t, u)
	u.CDNHook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "image/tiff")
		w.Write(file) // 200, not 206
		return true
	}
	b, _ := z.Frame.Box(560, 300)
	if _, err := cdn.Crop(context.Background(), z, b); err == nil {
		t.Fatal("a 200 for a range read was accepted")
	}
}

func TestZoom_RefusesPathsOffTheAllowList(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	for _, p := range []string{"", "/elsewhere/2025_1.tif", previewPath + "?x=1", "/skraafoto_server/../" + testID + ".tif"} {
		if _, err := newTestCDN(t, u, nil).Zoom(context.Background(), p); !errors.Is(err, dhm.ErrBadRequest) {
			t.Errorf("%q: %v", p, err)
		}
	}
	if len(u.Log()) != 0 {
		t.Fatalf("%d requests", len(u.Log()))
	}
}

// FuzzZoomBox: for any frame and point, a box is inside the level, at
// most PreviewMaxPx on each side, holds the marker, and on 512 px tiles
// or larger touches at most MaxZoomTiles tiles; a point outside the
// preview is ErrOutsideFrame.
func FuzzZoomBox(f *testing.F) {
	f.Add(560.0, 300.0, 5136, 3504, 512)
	f.Add(0.0, 0.0, 800, 600, 512)
	f.Add(1024.0, 699.0, 5136, 3504, 512)
	f.Add(1e300, -1e300, 1, 1, 1)
	f.Fuzz(func(t *testing.T, x, y float64, w, h, tile int) {
		if w < 1 || h < 1 || w > 1<<17 || h > 1<<17 || tile < 16 || tile > 4096 {
			return
		}
		pw, ph := previewSize(w, h)
		fr := ZoomFrame{PreviewWidth: pw, PreviewHeight: ph, Width: w, Height: h, TileWidth: tile, TileHeight: tile}
		b, err := fr.Box(x, y)
		in := x >= 0 && y >= 0 && x <= float64(pw) && y <= float64(ph)
		if err != nil {
			if in {
				t.Fatalf("(%v, %v) in %d × %d: %v", x, y, pw, ph, err)
			}
			return
		}
		if !in {
			t.Fatalf("(%v, %v) outside %d × %d gave %+v", x, y, pw, ph, b)
		}
		if b.X < 0 || b.Y < 0 || b.W < 1 || b.H < 1 || b.X+b.W > w || b.Y+b.H > h || b.W > PreviewMaxPx || b.H > PreviewMaxPx {
			t.Fatalf("box %+v of %d × %d", b, w, h)
		}
		m := fr.Marker(b, x, y)
		const eps = 1e-6
		if m.X < -eps || m.Y < -eps || m.X > float64(b.W)+eps || m.Y > float64(b.H)+eps {
			t.Fatalf("marker %+v outside %+v", m, b)
		}
		if tile >= 512 && fr.tilesIn(b) > MaxZoomTiles {
			t.Fatalf("%d tiles for %+v on %d px tiles", fr.tilesIn(b), b, tile)
		}
	})
}

// BenchmarkSkraafotoCrop is the cold cost of one crop of the real
// pyramid after its header: range reads, upstream bytes, time and
// allocations per crop (#737 §4.8).
func BenchmarkSkraafotoCrop(b *testing.B) {
	for _, c := range []struct {
		name string
		mod  func(*dhmtest.ImageCOGOptions)
	}{
		{"rgb+band", nil},
		{"ycbcr-texture", func(o *dhmtest.ImageCOGOptions) {
			o.Samples, o.Photometric = 3, 6
			o.Color = func(level, x, y int) [4]uint8 {
				v := uint32(x*7919+y*104729) * 2654435761
				return [4]uint8{uint8(90 + v>>27), uint8(110 + (v>>22)&31), uint8(70 + (v>>17)&31), 255}
			}
		}},
	} {
		b.Run(c.name, func(b *testing.B) {
			u := skraafototest.New()
			defer u.Close()
			u.Put(previewPath, zoomCOG(c.mod))
			cdn, z := openZoom(b, u)
			box, _ := z.Frame.Box(560, 300)
			before, bytesBefore := cdn.Requests(), u.CDNBytes.Load()
			b.ReportAllocs()
			b.ResetTimer()
			var outBytes int
			for i := 0; i < b.N; i++ {
				out, err := cdn.Crop(context.Background(), z, box)
				if err != nil {
					b.Fatal(err)
				}
				outBytes = len(out)
			}
			b.ReportMetric(float64(cdn.Requests()-before)/float64(b.N), "reads/op")
			b.ReportMetric(float64(u.CDNBytes.Load()-bytesBefore)/float64(b.N), "upstreamB/op")
			b.ReportMetric(float64(outBytes), "jpegB")
		})
	}
}
