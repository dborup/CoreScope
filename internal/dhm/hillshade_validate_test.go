package dhm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"runtime"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #695 — FetchHillshade returns a tile only if it decodes as a 256×256 PNG.
// The upstream is known to deliver truncated bodies (#684); a body that
// merely starts with the PNG signature must not pass. Every test runs
// against a fake upstream (httptest); none reaches Dataforsyningen.

// withIHDRSize returns p with its IHDR width and height set to w × h and
// the chunk CRC fixed, so only the declared size is wrong.
func withIHDRSize(p []byte, w, h uint32) []byte {
	q := append([]byte{}, p...)
	// signature(8) length(4) "IHDR"(4) width(4) height(4) … crc at 29.
	binary.BigEndian.PutUint32(q[16:], w)
	binary.BigEndian.PutUint32(q[20:], h)
	binary.BigEndian.PutUint32(q[29:], crc32.ChecksumIEEE(q[12:29]))
	return q
}

func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serveTile(t *testing.T, body []byte) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(body)
	})
}

func TestFetchHillshadeRejectsInvalidPNG(t *testing.T) {
	corrupt := append([]byte{}, pngStub...)
	corrupt[len(corrupt)/2] ^= 0xff // inside IDAT: the chunk CRC no longer matches
	cases := map[string]struct {
		body []byte
		want string
	}{
		// The body from the issue: the signature, then anything.
		"signature then text": {[]byte("\x89PNG\r\n\x1a\nnot a PNG"), "invalid image"},
		"signature only":      {pngSignature, "invalid image"},
		// Truncations as #684 produced them: the IHDR alone passes a
		// header-only check, so these need the full decode.
		"truncated after IHDR":  {pngStub[:33], "invalid image"},
		"truncated in IDAT":     {pngStub[:len(pngStub)/2], "invalid image"},
		"truncated before IEND": {pngStub[:len(pngStub)-12], "invalid image"},
		"truncated in IEND":     {pngStub[:len(pngStub)-1], "invalid image"},
		"corrupt IDAT":          {corrupt, "invalid image"},
		// Real PNGs of the wrong size.
		"255x256":   {dhmtest.PNG(255, 256, 1), "invalid image"},
		"256x255":   {dhmtest.PNG(256, 255, 1), "invalid image"},
		"512x512":   {dhmtest.PNG(512, 512, 1), "invalid image"},
		"1x1":       {dhmtest.PNG(1, 1, 1), "invalid image"},
		"8192 IHDR": {withIHDRSize(dhmtest.PNG(1, 1, 1), 8192, 8192), "invalid image"},
		// No signature at all stays the #680 category.
		"gif":   {[]byte("GIF89a\x01\x00\x01\x00"), "non-image response"},
		"empty": {nil, "non-image response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, h := range []Hillshade{HillshadeExaggerated, HillshadeTerrain} {
				data, err := newTestClient(t, serveTile(t, tc.body), &logSink{}).FetchHillshade(h, someTile())
				var fe FetchError
				if !errors.As(err, &fe) || fe.Reason != tc.want || data != nil {
					t.Fatalf("%v: %d bytes, err %v; want FetchError %q", h, len(data), err, tc.want)
				}
				if !FallbackEligible(err) {
					t.Fatalf("%v: %q is not fallback-eligible", h, fe.Reason)
				}
			}
		})
	}
}

// Every valid 256×256 PNG passes unchanged, whatever its colour model.
func TestFetchHillshadeAcceptsValidPNG(t *testing.T) {
	r := image.Rect(0, 0, TileSize, TileSize)
	gray := image.NewGray(r)
	pal := image.NewPaletted(r, color.Palette{color.Transparent, color.Black, color.White})
	nrgba := image.NewNRGBA(r) // partly transparent, like transparent=TRUE tiles
	for i := range gray.Pix {
		gray.Pix[i] = uint8(i)
		pal.Pix[i] = uint8(i % 3)
	}
	for i := range nrgba.Pix {
		nrgba.Pix[i] = uint8(i * 7)
	}
	cases := map[string][]byte{
		"fixture":   pngStub,
		"gray":      encodePNG(t, gray),
		"paletted":  encodePNG(t, pal),
		"nrgba":     encodePNG(t, nrgba),
		"other key": dhmtest.PNG(TileSize, TileSize, 7),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := newTestClient(t, serveTile(t, body), &logSink{}).FetchHillshade(HillshadeExaggerated, someTile())
			if err != nil || !bytes.Equal(data, body) {
				t.Fatalf("%d bytes, err %v; want the %d-byte tile", len(data), err, len(body))
			}
		})
	}
}

// The size is checked from the IHDR before any pixel buffer exists: a body
// declaring 8192×8192 must not make the decoder allocate the 256 MB image.
func TestCheckTilePNGBoundsAllocation(t *testing.T) {
	bomb := withIHDRSize(dhmtest.PNG(1, 1, 1), 8192, 8192)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := checkTilePNG(bomb)
	runtime.ReadMemStats(&after)
	if FailureReason(err) != "invalid image" {
		t.Fatalf("err %v", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 16<<20 {
		t.Fatalf("rejecting an 8192×8192 IHDR allocated %d MB", got>>20)
	}
}

// BenchmarkCheckTilePNG is the validation cost per upstream tile miss
// (#695), on a ~52 KB tile shaped like a real hillshade tile. Cache hits
// never validate.
func BenchmarkCheckTilePNG(b *testing.B) {
	tile := dhmtest.PNG(TileSize, TileSize, 1)
	b.SetBytes(int64(len(tile)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := checkTilePNG(tile); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeConfigOnly is the header-only check alone, for comparison:
// cheap, but it cannot see a truncated IDAT (see "truncated after IHDR").
func BenchmarkDecodeConfigOnly(b *testing.B) {
	tile := dhmtest.PNG(TileSize, TileSize, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := png.DecodeConfig(bytes.NewReader(tile)); err != nil {
			b.Fatal(err)
		}
	}
}
