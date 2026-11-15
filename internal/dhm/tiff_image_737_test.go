package dhm

// #737 M1 — ParseTiledImage, the layout of a JPEG-tiled image COG (the
// Skråfoto oblique photos). Every file is synthetic (dhmtest.ImageCOG).

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

func skraafotoCOG(edit func(level int, es []dhmtest.COGEntry) []dhmtest.COGEntry) []byte {
	return dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: dhmtest.SkraafotoLevels, Edit: edit, Pad: 1 << 20})
}

func TestTiledImage737_RealPyramid(t *testing.T) {
	file := skraafotoCOG(nil)
	img, err := ParseTiledImage(file[:64<<10], int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Levels) != len(dhmtest.SkraafotoLevels) {
		t.Fatalf("%d levels, want %d", len(img.Levels), len(dhmtest.SkraafotoLevels))
	}
	for i, l := range img.Levels {
		want := dhmtest.SkraafotoLevels[i]
		if l.Width != want[0] || l.Height != want[1] || l.TileWidth != 512 || l.TileHeight != 512 || l.Samples != 4 || l.Photometric != 2 {
			t.Fatalf("level %d: %+v", i, l)
		}
		if len(l.JPEGTables) == 0 {
			t.Fatalf("level %d: no JPEGTables", i)
		}
		for k := 0; k < l.NumTiles(); k++ {
			off, n := l.TileRange(k)
			if off < 8 || n != 16 || off+n > int64(len(file)) {
				t.Fatalf("level %d tile %d: %d+%d", i, k, off, n)
			}
		}
	}
	// IFD4 is 1284 × 876: 3 × 2 tiles of 512.
	if l := img.Levels[4]; l.TilesAcross() != 3 || l.TilesDown() != 2 || l.NumTiles() != 6 {
		t.Fatalf("IFD4: %d × %d tiles", l.TilesAcross(), l.TilesDown())
	}
}

func TestTiledImage737_SkipsMaskAndReadsYCbCr(t *testing.T) {
	file := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{1024, 512}, {512, 256}, {256, 128}}, Mask: 1})
	img, err := ParseTiledImage(file, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Levels) != 3 || img.Levels[2].Width != 256 {
		t.Fatalf("mask not skipped: %d levels", len(img.Levels))
	}
	file = dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{512, 512}}, Samples: 3, Photometric: 6})
	img, err = ParseTiledImage(file, int64(len(file)))
	if err != nil || img.Levels[0].Samples != 3 || img.Levels[0].Photometric != 6 {
		t.Fatalf("YCbCr: %+v %v", img, err)
	}
}

func TestTiledImage737_SparseTile(t *testing.T) {
	zero := uint32(0)
	file := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{512, 512}}, Edit: func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
		for i := range es {
			if es[i].Tag == 324 || es[i].Tag == 325 {
				es[i].Raw = &zero // one tile: the value is inline
			}
		}
		return es
	}})
	img, err := ParseTiledImage(file, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if off, n := img.Levels[0].TileRange(0); off != 0 || n != 0 {
		t.Fatalf("sparse tile %d+%d", off, n)
	}
}

func replaceEntry(es []dhmtest.COGEntry, e dhmtest.COGEntry) []dhmtest.COGEntry {
	for i := range es {
		if es[i].Tag == e.Tag {
			es[i] = e
			return es
		}
	}
	return append(es, e)
}

func dropEntry(es []dhmtest.COGEntry, tag uint16) []dhmtest.COGEntry {
	out := es[:0]
	for _, e := range es {
		if e.Tag != tag {
			out = append(out, e)
		}
	}
	return out
}

func TestTiledImage737_Refuses(t *testing.T) {
	far := uint32(1 << 30)
	for name, edit := range map[string]func(int, []dhmtest.COGEntry) []dhmtest.COGEntry{
		"deflate": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(259, 8))
		},
		"old-style jpeg": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(259, 6))
		},
		"16-bit": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(258, 8, 8, 8, 16))
		},
		"bits count": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(258, 8, 8, 8))
		},
		"2 samples": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(replaceEntry(es, dhmtest.ShortsEntry(277, 2)), dhmtest.ShortsEntry(258, 8, 8))
		},
		"planar": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(284, 2))
		},
		"ycbcr 4 samples": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(262, 6))
		},
		"cmyk": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(262, 5))
		},
		"tile 500": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(322, 500))
		},
		"tile 2048": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(323, 2048))
		},
		"no tile length": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry { return dropEntry(es, 323) },
		"width 0": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.LongsEntry(256, 0))
		},
		"width 70000": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.LongsEntry(256, 70000))
		},
		"strips": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry { return append(es, dhmtest.LongsEntry(273, 8)) },
		"tile count entries": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.LongsEntry(325, 16, 16))
		},
		"tile past file": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.COGEntry{Tag: 324, Type: 4, Count: 1, Raw: &far})
		},
		"empty count, real offset": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.LongsEntry(325, 0))
		},
		"tables as shorts": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.ShortsEntry(347, 1, 2, 3))
		},
		"tables too large": func(_ int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			return replaceEntry(es, dhmtest.COGEntry{Tag: 347, Type: 7, Count: MaxJPEGTables + 1, Data: make([]byte, MaxJPEGTables+1)})
		},
		"first is overview": func(level int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			if level == 0 {
				return replaceEntry(es, dhmtest.LongsEntry(254, 1))
			}
			return es
		},
		"overview not smaller": func(level int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			if level == 1 {
				return replaceEntry(replaceEntry(es, dhmtest.LongsEntry(256, 512)), dhmtest.LongsEntry(257, 256))
			}
			return es
		},
	} {
		file := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{512, 256}, {256, 128}}, Edit: edit})
		if _, err := ParseTiledImage(file, int64(len(file))); err == nil || errors.Is(err, ErrShortHeader) {
			t.Errorf("%s: err %v, want malformed", name, err)
		}
	}
}

func TestTiledImage737_ShortHeaderAndMagic(t *testing.T) {
	file := skraafotoCOG(nil)
	if _, err := ParseTiledImage(file[:4096], int64(len(file))); !errors.Is(err, ErrShortHeader) {
		t.Fatalf("4 KiB prefix: %v, want ErrShortHeader", err)
	}
	if _, err := ParseTiledImage(file[:4096], 4000); err == nil || errors.Is(err, ErrShortHeader) {
		t.Fatalf("prefix longer than the file: %v", err)
	}
	big := append([]byte(nil), file[:64<<10]...)
	binary.LittleEndian.PutUint16(big[2:], 43)
	if _, err := ParseTiledImage(big, int64(len(file))); err == nil {
		t.Fatal("BigTIFF accepted")
	}
}

func TestTiledImage737_IFDCap(t *testing.T) {
	var levels [][2]int
	for i := 0; i < MaxImageIFDs+4; i++ {
		levels = append(levels, [2]int{300 - 10*i, 200 - 10*i})
	}
	file := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: levels, Tile: 16})
	img, err := ParseTiledImage(file, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) <= MaxImageIFDs || len(img.Levels) != MaxImageIFDs {
		t.Fatalf("%d IFDs in the file, %d read; cap %d", len(levels), len(img.Levels), MaxImageIFDs)
	}
}

func TestTiledImage737_IFDLoopEnds(t *testing.T) {
	file := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{512, 256}}})
	// Point IFD0's next-IFD offset at itself.
	first := binary.LittleEndian.Uint32(file[4:])
	n := binary.LittleEndian.Uint16(file[first:])
	binary.LittleEndian.PutUint32(file[first+2+12*uint32(n):], first)
	if _, err := ParseTiledImage(file, int64(len(file))); err == nil {
		t.Fatal("a self-referencing IFD chain was accepted")
	}
}

func FuzzParseTiledImage(f *testing.F) {
	f.Add(skraafotoCOG(nil)[:64<<10], int64(1<<30))
	small := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{512, 256}, {256, 128}}, Mask: 1})
	f.Add(small, int64(len(small)))
	ycc := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: [][2]int{{64, 64}}, Tile: 32, Samples: 3, Photometric: 6, Encode: func(int) bool { return true }})
	f.Add(ycc, int64(len(ycc)))
	f.Fuzz(func(t *testing.T, prefix []byte, size int64) {
		img, err := ParseTiledImage(prefix, size)
		if err != nil {
			return
		}
		for i, l := range img.Levels {
			if l.Width < 1 || l.Height < 1 || l.Width > MaxImageEdge || l.Height > MaxImageEdge || len(l.JPEGTables) > MaxJPEGTables {
				t.Fatalf("level %d: %+v", i, l)
			}
			if p := img.Levels[max(i-1, 0)]; i > 0 && (l.Width > p.Width || l.Height > p.Height || l.Width*l.Height >= p.Width*p.Height) {
				t.Fatalf("level %d not smaller", i)
			}
			for k := 0; k < l.NumTiles(); k++ {
				off, n := l.TileRange(k)
				if off < 0 || n < 0 || off+n > size || n > int64(l.MaxTileBytes()) {
					t.Fatalf("level %d tile %d: %d+%d of %d", i, k, off, n, size)
				}
			}
		}
		if len(img.Levels) > MaxImageIFDs {
			t.Fatalf("%d levels", len(img.Levels))
		}
	})
}
