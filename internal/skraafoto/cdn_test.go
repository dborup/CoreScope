package skraafoto

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"regexp"
	"strconv"
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

var previewPath = skraafototest.Path(testID)

// photoCOG is a photo with the real pyramid; only the tiles of the level
// at index enc are real JPEG, every other tile is filler that fails to
// decode. Pad puts the tiles past the first header read, as in a real
// file.
func photoCOG(enc int, mod func(*dhmtest.ImageCOGOptions)) []byte {
	o := dhmtest.ImageCOGOptions{Levels: dhmtest.SkraafotoLevels, Encode: func(l int) bool { return l == enc }, Pad: 1 << 20}
	if mod != nil {
		mod(&o)
	}
	return dhmtest.ImageCOG(o)
}

func newTestCDN(t testing.TB, u *skraafototest.Upstream, mod func(*Options)) *CDN {
	t.Helper()
	o := Options{Transport: u.Transport()}
	if mod != nil {
		mod(&o)
	}
	return NewCDN(o)
}

func decodePreview(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

var rangeHdrRe = regexp.MustCompile(`^bytes=([0-9]+)-([0-9]+)$`)

// The owner's check (2026-11-15): a 1024 px preview comes from overview
// IFD4, 1284 × 876, six 512 px tiles. The preview reads the header and
// those tiles only, every request a range, no token, nothing else.
func TestPreview_ReadsOnlyIFD4(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	file := photoCOG(4, nil)
	u.Put(previewPath, file)
	c := newTestCDN(t, u, nil)
	out, err := c.Preview(context.Background(), previewPath)
	if err != nil {
		t.Fatal(err)
	}
	img := decodePreview(t, out)
	if img.Bounds().Dx() != PreviewMaxPx || img.Bounds().Dy() != 699 {
		t.Fatalf("preview %v, want 1024 × 699", img.Bounds())
	}
	layout, err := dhm.ParseTiledImage(file, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	l4 := layout.Levels[4]
	lo, hi := int64(len(file)), int64(0)
	for i := 0; i < l4.NumTiles(); i++ {
		off, n := l4.TileRange(i)
		lo, hi = min(lo, off), max(hi, off+n)
	}
	log := u.Log()
	if len(log) < 2 || len(log) > 1+l4.NumTiles() {
		t.Fatalf("%d requests: one header read and at most one per IFD4 tile", len(log))
	}
	var bytesRead int64
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
		bytesRead += to - from + 1
		if i == 0 && (from != 0 || to != HeaderBytes-1) {
			t.Fatalf("header read %d-%d", from, to)
		}
		if i > 0 && (from < lo || to >= hi) {
			t.Fatalf("read %d-%d outside IFD4's tiles [%d, %d)", from, to, lo, hi)
		}
	}
	if bytesRead > MaxPreviewBytes || u.CDNBytes.Load() > MaxPreviewBytes {
		t.Fatalf("%d bytes read", bytesRead)
	}
	if c.Requests() != int64(len(log)) {
		t.Fatalf("Requests %d, log %d", c.Requests(), len(log))
	}
}

func near(a, b uint8, tol int) bool {
	d := int(a) - int(b)
	return d >= -tol && d <= tol
}

// The preview shows the picture: a pixel of the output is the scaled
// pixel of IFD4 under it, for the 4-sample photos (RGB plus a band, no
// Adobe marker: decoded as inverted CMYK and inverted back), for RGB and
// for YCbCr tiles.
func TestPreview_Colours(t *testing.T) {
	for _, c := range []struct {
		name         string
		samples, pho int
		tol          int
	}{{"rgb+band", 4, 2, 10}, {"rgb", 3, 2, 10}, {"ycbcr", 3, 6, 24}} {
		u := skraafototest.New()
		u.Put(previewPath, photoCOG(4, func(o *dhmtest.ImageCOGOptions) { o.Samples, o.Photometric = c.samples, c.pho }))
		out, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath)
		u.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		img := decodePreview(t, out)
		for _, at := range [][2]int{{100, 100}, {700, 300}, {1000, 650}} {
			want := dhmtest.Gradient(4, at[0]*1284/1024, at[1]*876/699)
			r, g, b, _ := img.At(at[0], at[1]).RGBA()
			if !near(uint8(r>>8), want[0], c.tol) || !near(uint8(g>>8), want[1], c.tol) || !near(uint8(b>>8), want[2], c.tol) {
				t.Errorf("%s at %v: %d %d %d, want %v", c.name, at, r>>8, g>>8, b>>8, want[:3])
			}
		}
	}
}

func TestPreview_StrictRanges(t *testing.T) {
	file := photoCOG(4, nil)
	for name, hook := range map[string]func(w http.ResponseWriter, r *http.Request) bool{
		"200 instead of 206": func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "image/tiff")
			w.Write(file)
			return true
		},
		"200 with a content range": func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Range", "bytes 0-65535/"+strconv.Itoa(len(file)))
			w.Write(file[:HeaderBytes])
			return true
		},
		"other range": func(w http.ResponseWriter, r *http.Request) bool {
			skraafototest.WriteRange(w, file[1:HeaderBytes+1], 1, int64(len(file)))
			return true
		},
		"404": func(w http.ResponseWriter, r *http.Request) bool { http.NotFound(w, r); return true },
	} {
		u := skraafototest.New()
		u.Put(previewPath, file)
		u.CDNHook = hook
		if _, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if u.CDNBytes.Load() != 0 || len(u.Log()) != 1 {
			t.Errorf("%s: read on after a refusal", name)
		}
		u.Close()
	}
}

func TestPreview_RefusesPathsOffTheAllowList(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	c := newTestCDN(t, u, nil)
	for _, p := range []string{"", "/x.tif", "/skraafoto_server/../etc/" + testID + ".tif", "/skraafoto_server/a/" + testID + ".tif?x=1", "//evil.example/skraafoto_server/a/" + testID + ".tif"} {
		if _, err := c.Preview(context.Background(), p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if len(u.Log()) != 0 {
		t.Fatalf("%d requests", len(u.Log()))
	}
}

func TestPreview_HeaderBounds(t *testing.T) {
	// The tile tables of IFD0 lie 2 MiB into the file: past the header
	// read and its one extension.
	far := uint32(2 << 20)
	file := photoCOG(4, func(o *dhmtest.ImageCOGOptions) {
		o.Pad = 4 << 20
		o.Edit = func(level int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			if level == 0 {
				for i := range es {
					if es[i].Tag == 324 {
						es[i].Raw = &far
					}
				}
			}
			return es
		}
	})
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, file)
	_, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath)
	if dhm.FailureReason(err) != "header too large" {
		t.Fatalf("%v", err)
	}
	if n := len(u.Log()); n != 2 {
		t.Fatalf("%d reads, want the header and its one extension", n)
	}
	if u.CDNBytes.Load() > maxHeaderBytes {
		t.Fatalf("%d bytes", u.CDNBytes.Load())
	}
}

func TestPreview_NeverDecodesFullResolution(t *testing.T) {
	for name, levels := range map[string][][2]int{
		"no overview":       {{20544, 14016}},
		"overview too big":  {{20544, 14016}, {4096, 3000}},
		"too many tiles":    {{4096, 1024}, {2048, 512}},
		"oversize tile set": {{3000, 2000}, {1500, 1000}},
		// Four 1024 px tiles and 2 MP, but four times the preview's width.
		"wider than twice": {{4096, 512}},
	} {
		u := skraafototest.New()
		file := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: levels, Tile: map[string]int{"too many tiles": 128, "oversize tile set": 256, "wider than twice": 1024}[name], Pad: 1 << 20})
		u.Put(previewPath, file)
		_, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath)
		if dhm.FailureReason(err) != "no preview level" {
			t.Errorf("%s: %v", name, err)
		}
		if n := len(u.Log()); n != 1 {
			t.Errorf("%s: %d reads, want the header only", name, n)
		}
		u.Close()
	}
}

func TestPreview_SmallPhotoIsNotEnlarged(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{800, 600}, {400, 300}}, Encode: func(int) bool { return true }}))
	out, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath)
	if err != nil {
		t.Fatal(err)
	}
	if b := decodePreview(t, out).Bounds(); b.Dx() != 800 || b.Dy() != 600 {
		t.Fatalf("%v", b)
	}
}

func TestPreview_BadTileAndOutputCap(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, photoCOG(3, nil)) // IFD4's tiles are filler
	if _, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath); dhm.FailureReason(err) != "bad tile" {
		t.Fatalf("filler tiles: %v", err)
	}
	u.Put(previewPath, photoCOG(4, func(o *dhmtest.ImageCOGOptions) {
		o.Block = func(level, i int, b []byte) []byte {
			if level == 4 && i == 2 {
				return b[:len(b)/2] // a truncated JPEG stream
			}
			return b
		}
	}))
	if _, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath); dhm.FailureReason(err) != "bad tile" {
		t.Fatalf("truncated tile: %v", err)
	}
	u.Put(previewPath, photoCOG(4, nil))
	if _, err := newTestCDN(t, u, func(o *Options) { o.MaxOutput = 1000 }).Preview(context.Background(), previewPath); dhm.FailureReason(err) != "oversize preview" {
		t.Fatalf("output cap: %v", err)
	}
}

func TestPreview_SparseTilesAreBlack(t *testing.T) {
	zero := uint32(0)
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{512, 512}}, Encode: func(int) bool { return true },
		Edit: func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			for i := range es {
				if es[i].Tag == 324 || es[i].Tag == 325 {
					es[i].Raw = &zero
				}
			}
			return es
		}}))
	out, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath)
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _, _ := decodePreview(t, out).At(100, 100).RGBA(); r>>8 > 8 {
		t.Fatalf("sparse tile is %d", r>>8)
	}
	if len(u.Log()) != 1 {
		t.Fatalf("%d reads for a sparse photo", len(u.Log()))
	}
}

func TestPreview_Canceled(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.Put(previewPath, photoCOG(4, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestCDN(t, u, nil).Preview(ctx, previewPath); dhm.FailureReason(err) != "canceled" {
		t.Fatalf("%v", err)
	}
}

// FuzzPreviewHeader runs the whole header path — range read, IFD walk,
// level choice — on fuzzed file starts, through the fake CDN.
func FuzzPreviewHeader(f *testing.F) {
	f.Add(photoCOG(4, func(o *dhmtest.ImageCOGOptions) { o.Pad = 0 })[:4096])
	small := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{1100, 600}, {550, 300}}, Encode: func(int) bool { return true }})
	f.Add(small)
	// One fake for the whole run: f.Fuzz calls are sequential per process.
	u := skraafototest.New()
	f.Cleanup(u.Close)
	cdn := NewCDN(Options{Transport: u.Transport()})
	f.Fuzz(func(t *testing.T, file []byte) {
		if len(file) > 1<<20 {
			return
		}
		u.Put(previewPath, file)
		before := u.CDNBytes.Load()
		out, err := cdn.Preview(context.Background(), previewPath)
		if read := u.CDNBytes.Load() - before; read > MaxPreviewBytes {
			t.Fatalf("%d bytes", read)
		}
		if err != nil {
			return
		}
		img, err := jpeg.DecodeConfig(bytes.NewReader(out))
		if err != nil || img.Width > PreviewMaxPx || img.Height > PreviewMaxPx || len(out) > MaxPreviewOutput {
			t.Fatalf("%v %+v %d", err, img, len(out))
		}
	})
}

// BenchmarkSkraafotoPreview is the cold cost of one preview on the real
// pyramid: range reads, upstream bytes and time per preview (#737 §4.8).
func BenchmarkSkraafotoPreview(b *testing.B) {
	for _, c := range []struct {
		name string
		mod  func(*dhmtest.ImageCOGOptions)
	}{
		{"rgb+band", nil},
		// Texture through Go's encoder: tile sizes like a real photo's.
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
			u.Put(previewPath, photoCOG(4, c.mod))
			cdn := newTestCDN(b, u, nil)
			b.ReportAllocs()
			b.ResetTimer()
			var outBytes int
			for i := 0; i < b.N; i++ {
				out, err := cdn.Preview(context.Background(), previewPath)
				if err != nil {
					b.Fatal(err)
				}
				outBytes = len(out)
			}
			b.ReportMetric(float64(cdn.Requests())/float64(b.N), "reads/op")
			b.ReportMetric(float64(u.CDNBytes.Load())/float64(b.N), "upstreamB/op")
			b.ReportMetric(float64(outBytes), "jpegB")
		})
	}
}
