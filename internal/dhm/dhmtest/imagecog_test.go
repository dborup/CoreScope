package dhmtest

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

// splice joins tables and an abbreviated stream into one JPEG, with an
// Adobe APP14 marker of transform 0 (RGB or CMYK) when adobe is set.
func splice(tables, stream []byte, adobe bool) []byte {
	out := []byte{0xFF, 0xD8}
	if adobe {
		out = append(out, 0xFF, 0xEE, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0)
	}
	out = append(out, tables[2:len(tables)-2]...)
	return append(out, stream[2:]...)
}

func TestDCJPEGDecodesExactly(t *testing.T) {
	px := func(x, y int) [4]uint8 { return [4]uint8{uint8(x * 3), uint8(y * 5), uint8(x + y), uint8(255 - x)} }
	for _, n := range []int{1, 3, 4} {
		tables, stream := DCJPEG(40, 24, n, px)
		img, err := jpeg.Decode(bytes.NewReader(splice(tables, stream, n > 1)))
		if err != nil {
			t.Fatalf("%d components: %v", n, err)
		}
		if img.Bounds() != image.Rect(0, 0, 40, 24) {
			t.Fatalf("%d components: bounds %v", n, img.Bounds())
		}
		for _, at := range [][2]int{{0, 0}, {13, 9}, {39, 23}} {
			want := px(at[0]/8*8, at[1]/8*8)
			var got [4]uint8
			switch m := img.(type) {
			case *image.Gray:
				got[0] = m.GrayAt(at[0], at[1]).Y
			case *image.RGBA:
				c := m.RGBAAt(at[0], at[1])
				got = [4]uint8{c.R, c.G, c.B, want[3]}
			case *image.CMYK:
				// Adobe transform 0: Go stores 255 − v.
				c := m.CMYKAt(at[0], at[1])
				got = [4]uint8{255 - c.C, 255 - c.M, 255 - c.Y, 255 - c.K}
			default:
				t.Fatalf("%d components: %T", n, img)
			}
			for c := 0; c < n; c++ {
				if d := int(got[c]) - int(want[c]); d < -1 || d > 1 {
					t.Fatalf("%d components at %v: got %v, want %v", n, at, got, want)
				}
			}
		}
	}
}

func TestGoJPEGTileSplits(t *testing.T) {
	tables, stream := GoJPEGTile(64, 32, func(x, y int) [4]uint8 { return [4]uint8{200, 50, 50, 255} })
	if bytes.Contains(stream, []byte{0xFF, 0xDB}) && bytes.Index(stream, []byte{0xFF, 0xDB}) < bytes.Index(stream, []byte{0xFF, 0xDA}) {
		t.Fatal("DQT left in the stream")
	}
	img, err := jpeg.Decode(bytes.NewReader(splice(tables, stream, false)))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(10, 10).RGBA()
	if r>>8 < 180 || g>>8 > 80 || b>>8 > 80 {
		t.Fatalf("colour %d %d %d", r>>8, g>>8, b>>8)
	}
}
