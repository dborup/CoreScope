package dhmtest

import (
	"bytes"
	"testing"
)

// The decoder round trip is covered in package dhm (profile tests decode
// these bytes); this only pins the header so a broken writer fails here.
func TestGeoTIFFHeader(t *testing.T) {
	nd := float32(-9999)
	for _, deflate := range []bool{false, true} {
		b := GeoTIFF(3, 2, []float32{1, 2, 3, 4, 5, 6}, &nd, deflate)
		if !bytes.HasPrefix(b, []byte("II*\x00\x08\x00\x00\x00")) {
			t.Fatalf("deflate=%v: header %x", deflate, b[:8])
		}
	}
}
