package dhmtest

import (
	"bytes"
	"image/jpeg"
	"testing"
)

// JPEG must be a real, decodable image of the asked size, deterministic per
// seed and different across seeds, within the #728 estimate at 256 × 256.
func TestJPEG(t *testing.T) {
	a, b := JPEG(256, 256, 1), JPEG(256, 256, 2)
	if !bytes.Equal(a, JPEG(256, 256, 1)) || bytes.Equal(a, b) {
		t.Fatal("JPEG is not deterministic per seed, or seeds collide")
	}
	img, err := jpeg.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 256 || r.Dy() != 256 {
		t.Fatalf("bounds %v", r)
	}
	t.Logf("256x256 tile: %d bytes", len(a))
	if len(a) < 15_000 || len(a) > 40_000 {
		t.Fatalf("tile is %d bytes, want the #728 estimate of 15-40 KB", len(a))
	}
}
