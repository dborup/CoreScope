package skraafoto

import (
	"context"
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

// patchSOF sets the frame size in the SOF of a tile's JPEG stream.
func patchSOF(t testing.TB, b []byte, w, h int) []byte {
	b = append([]byte(nil), b...)
	for p := 2; p+9 <= len(b) && b[p] == 0xFF; {
		m := b[p+1]
		if m >= 0xC0 && m <= 0xC2 {
			binary.BigEndian.PutUint16(b[p+5:], uint16(h))
			binary.BigEndian.PutUint16(b[p+7:], uint16(w))
			return b
		}
		p += 2 + int(binary.BigEndian.Uint16(b[p+2:]))
	}
	t.Fatal("no SOF in tile")
	return nil
}

// A tile whose SOF is not the tile size is refused before the decoder
// allocates its frame (#800 F1, F3): a hostile 60000² SOF in a few
// hundred bytes would otherwise allocate gigabytes, and a 16² SOF in a
// 4-sample tile would index past the decoded frame in cmykToRGB.
func TestPreview_TileSOFMustBeTheTileSize(t *testing.T) {
	for _, c := range []struct {
		name    string
		w, h    int
		samples int
	}{
		{"huge", 60000, 60000, 4},
		{"wide", 60000, 512, 3},
		{"tall", 512, 60000, 3},
		{"small-cmyk", 16, 16, 4},
		{"small-rgb", 16, 16, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := skraafototest.New()
			defer u.Close()
			u.Put(previewPath, photoCOG(4, func(o *dhmtest.ImageCOGOptions) {
				o.Samples = c.samples
				o.Block = func(level, i int, b []byte) []byte {
					if level == 4 && i == 0 {
						return patchSOF(t, b, c.w, c.h)
					}
					return b
				}
			}))
			cdn := newTestCDN(t, u, nil)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := cdn.Preview(context.Background(), previewPath)
			runtime.ReadMemStats(&after)
			if dhm.FailureReason(err) != "bad tile" {
				t.Fatalf("%v", err)
			}
			// A whole correct preview allocates about 30 MiB.
			if a := after.TotalAlloc - before.TotalAlloc; a > 64<<20 {
				t.Fatalf("allocated %d MiB for a refused tile", a>>20)
			}
		})
	}
}

// Every byte of a preview, header and tiles, fits in MaxPreviewBytes
// (#800 F4): IFD4's six tiles padded past the budget are refused after
// the header read alone; padded to just inside it, the preview is served.
func TestPreview_ByteBudget(t *testing.T) {
	for _, c := range []struct {
		name string
		pad  int
		ok   bool
	}{
		{"over", 750 << 10, false},
		{"inside", 600 << 10, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := skraafototest.New()
			defer u.Close()
			u.Put(previewPath, photoCOG(4, func(o *dhmtest.ImageCOGOptions) {
				o.Block = func(level, i int, b []byte) []byte {
					if level == 4 {
						// Bytes after EOI, which the decoder never reads.
						return append(b, make([]byte, c.pad)...)
					}
					return b
				}
			}))
			_, err := newTestCDN(t, u, nil).Preview(context.Background(), previewPath)
			read := u.CDNBytes.Load()
			if c.ok {
				if err != nil {
					t.Fatalf("%v", err)
				}
			} else {
				if dhm.FailureReason(err) != "preview too large" {
					t.Fatalf("%v", err)
				}
				if len(u.Log()) != 1 {
					t.Fatalf("%d reads, want the header only", len(u.Log()))
				}
			}
			if read > MaxPreviewBytes {
				t.Fatalf("read %d bytes", read)
			}
		})
	}
}

// FuzzPreviewTile fuzzes the stored bytes of one IFD4 tile in a valid
// photo: whatever the tile holds, a preview never panics and allocates
// at most 128 MiB (#800 F1).
func FuzzPreviewTile(f *testing.F) {
	var tile []byte
	photoCOG(4, func(o *dhmtest.ImageCOGOptions) {
		o.Block = func(level, i int, b []byte) []byte {
			if level == 4 && i == 0 {
				tile = b
			}
			return b
		}
	})
	f.Add(tile)
	f.Add(patchSOF(f, tile, 60000, 60000))
	f.Add(patchSOF(f, tile, 16, 16))
	u := skraafototest.New()
	f.Cleanup(u.Close)
	cdn := NewCDN(Options{Transport: u.Transport()})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 256<<10 {
			return
		}
		u.Put(previewPath, photoCOG(4, func(o *dhmtest.ImageCOGOptions) {
			o.Block = func(level, i int, raw []byte) []byte {
				if level == 4 && i == 0 {
					return b
				}
				return raw
			}
		}))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		cdn.Preview(context.Background(), previewPath)
		runtime.ReadMemStats(&after)
		if a := after.TotalAlloc - before.TotalAlloc; a > 128<<20 {
			t.Fatalf("allocated %d MiB", a>>20)
		}
	})
}
