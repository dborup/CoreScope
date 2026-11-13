package dhmtest

import (
	"bytes"
	"image/png"
	"testing"
)

// PNG must be a real, decodable image of the asked size, deterministic per
// seed and different across seeds, near a real tile's size at 256 × 256.
func TestPNG(t *testing.T) {
	a, b := PNG(256, 256, 1), PNG(256, 256, 2)
	if !bytes.Equal(a, PNG(256, 256, 1)) || bytes.Equal(a, b) {
		t.Fatal("PNG is not deterministic per seed, or seeds collide")
	}
	img, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 256 || r.Dy() != 256 {
		t.Fatalf("bounds %v", r)
	}
	t.Logf("256x256 tile: %d bytes", len(a))
	if len(a) < 30_000 || len(a) > 90_000 {
		t.Fatalf("tile is %d bytes, want a real tile's ~50-60 KB", len(a))
	}
}
