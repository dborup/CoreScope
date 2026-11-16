package dhm

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// #860 — how Danish land is distributed over height: the measurement the
// ElevationRamp stops are chosen from, and the source of
// testdata/dk_land_hist_860.txt.
//
// No DHM token is used here, nor any network: it reads Copernicus DEM
// GLO-90 tiles (the public AWS Open Data bucket copernicus-dem-90m,
// Copernicus_DSM_COG_30_N<lat>_00_E<lon>_00_DEM.tif, the 20 tiles that
// hold Danish land) from the directory in DHM_860_GLO90_DIR, and is
// skipped without it. A pixel is Danish land when Covers (the DHM extent
// minus foreign land, #692) holds at its centre and its height is not
// exactly 0 (GLO's sea), weighted by its area (cos latitude).
// DHM_860_WRITE_HIST=1 rewrites the histogram.
//
// GLO-90 is a 90 m surface model (trees and buildings included), not
// DHM/Terræn, so forests read some metres high; at the scale of a ramp
// band that does not move a stop.
func TestElevation860_MeasureLandDistribution(t *testing.T) {
	dir := os.Getenv("DHM_860_GLO90_DIR")
	if dir == "" {
		t.Skip("DHM_860_GLO90_DIR is not set")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.tif"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no tiles in %s: %v", dir, err)
	}
	type sample struct{ h, w float64 }
	var land []sample
	// The views of the issue's before/after comparison, lon/lat boxes.
	views := []struct {
		name string
		box  [4]float64
		s    []sample
	}{
		{name: "Vestjylland", box: [4]float64{8.1, 55.9, 8.9, 56.4}},
		{name: "Silkeborg/Himmelbjerget", box: [4]float64{9.4, 56.0, 9.9, 56.25}},
		{name: "Mols", box: [4]float64{10.45, 56.15, 10.75, 56.3}},
	}
	var area float64 // km², a sanity check against Denmark's 42,951 km²
	for _, f := range files {
		g := readGLO90(t, f)
		for y := 0; y < g.Height; y++ {
			lat := g.Georef.OriginN - (float64(y)+0.5)*g.Georef.PixelN
			w := math.Cos(lat * math.Pi / 180)
			for x := 0; x < g.Width; x++ {
				lon := g.Georef.OriginE + (float64(x)+0.5)*g.Georef.PixelE
				h := float64(g.Values[y*g.Width+x])
				if h == 0 || math.IsNaN(h) || (g.HasNoData && float32(h) == g.NoData) {
					continue
				}
				if e, n := ToUTM32(lat, lon); !Covers(e, n) {
					continue
				}
				land = append(land, sample{h, w})
				area += w * g.Georef.PixelE * 111.32 * g.Georef.PixelN * 111.32
				for i := range views {
					b := views[i].box
					if lon >= b[0] && lon <= b[2] && lat >= b[1] && lat <= b[3] {
						views[i].s = append(views[i].s, sample{h, w})
					}
				}
			}
		}
	}
	report := func(name string, s []sample) []float64 {
		sort.Slice(s, func(i, j int) bool { return s[i].h < s[j].h })
		var total float64
		for _, v := range s {
			total += v.w
		}
		pct := []float64{1, 5, 10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 98, 99, 99.9}
		var acc float64
		var line strings.Builder
		j := 0
		for _, v := range s {
			acc += v.w
			for j < len(pct) && acc >= total*pct[j]/100 {
				fmt.Fprintf(&line, " p%g=%.1f", pct[j], v.h)
				j++
			}
		}
		t.Logf("%s: %d px;%s; max %.1f m", name, len(s), line.String(), s[len(s)-1].h)
		hist := make([]float64, 201)
		for _, v := range s {
			hist[int(math.Min(math.Max(math.Round(v.h), 0), 200))] += v.w / total
		}
		line.Reset()
		for i, sh := range bandShares(hist, ElevationRamp) {
			fmt.Fprintf(&line, " %d-%d:%.1f%%", ElevationRamp[i].M, ElevationRamp[i+1].M, 100*sh)
		}
		t.Logf("%s, land per band:%s", name, line.String())
		return hist
	}
	t.Logf("Danish land: %.0f km²", area)
	hist := report("Denmark", land)
	for _, v := range views {
		report(v.name, v.s)
	}
	if os.Getenv("DHM_860_WRITE_HIST") == "1" {
		var b strings.Builder
		b.WriteString("# #860: the share of Danish land per whole metre of height. Index 0 is\n" +
			"# every height that rounds to 0 m or below, 200 every height from 200 m.\n" +
			"# Measured by TestElevation860_MeasureLandDistribution from Copernicus\n" +
			"# DEM GLO-90 (see testdata/README.md).\n")
		for m, sh := range hist {
			fmt.Fprintf(&b, "%d %.6g\n", m, sh)
		}
		if err := os.WriteFile("testdata/dk_land_hist_860.txt", []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readGLO90 reads a GLO-90 tile: one 2048 px block per file, which
// ParseTiledGeoTIFF (made for GLO-30's 1024 px blocks) refuses, so the
// IFD is read with the package's TIFF reader directly.
func readGLO90(t *testing.T, path string) Grid {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &tiffReader{buf: raw, size: uint64(len(raw)), crs: crsWGS84}
	first, err := r.readHeader()
	if err == nil {
		_, err = r.readIFD(first)
	}
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	geo, err := r.georef()
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var dims [4]uint32
	for i, tag := range []uint16{tagImageWidth, tagImageLength, tagTileWidth, tagTileLength} {
		if dims[i], err = r.uint(tag, 0, false); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if dims[0] > dims[2] || dims[1] > dims[3] {
		t.Fatalf("%s: more than one block", path)
	}
	comp, pred, err := r.coding()
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	off, _ := r.at(r.fields[tagTileOffsets], 0)
	n, _ := r.at(r.fields[tagTileByteCounts], 0)
	buf := make([]float32, int(dims[2])*int(dims[3]))
	if err := newRowDecoder(&tiffReader{order: r.order}, comp, pred, int(dims[2])).decode(raw[off:off+n], buf); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	nd, hasND, _ := r.noData()
	g := Grid{Width: int(dims[0]), Height: int(dims[1]), Georef: geo, NoData: nd, HasNoData: hasND,
		Values: make([]float32, int(dims[0])*int(dims[1]))}
	for y := 0; y < g.Height; y++ {
		copy(g.Values[y*g.Width:(y+1)*g.Width], buf[y*int(dims[2]):])
	}
	return g
}
