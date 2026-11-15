package copdem

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tilesSHA256 pins tiles.txt: regenerate it with the script and update
// this together (see scripts/gen-copdem-tiles.py).
const tilesSHA256 = "f958058b8174ccf8a3d2c8c573c82d04662a415e3d0c681dab1dc55950252828"

func TestTilesTextIsTheGeneratedFile(t *testing.T) {
	sum := sha256.Sum256([]byte(tilesText))
	if got := hex.EncodeToString(sum[:]); got != tilesSHA256 {
		t.Fatalf("tiles.txt sha256 %s, want %s: regenerate with scripts/gen-copdem-tiles.py and update tilesSHA256", got, tilesSHA256)
	}
	if !strings.Contains(tilesText, "# source md5: 637fe75ddf7615ba853dd83caf05cd82, 26450 tiles listed\n") {
		t.Fatal("tiles.txt does not name the tileList.txt it was generated from")
	}
	// The script's region is the Go region.
	if !strings.Contains(tilesText, "# region: south edges N53-N58, west edges E006-E016 (66 tiles, 60 exist)\n") ||
		regionSouth != 53 || regionNorth != 58 || regionWest != 6 || regionEast != 16 {
		t.Fatal("tiles.txt region and the Go region differ")
	}
}

func TestTileList(t *testing.T) {
	n := 0
	for lat := regionSouth; lat <= regionNorth; lat++ {
		for lon := regionWest; lon <= regionEast; lon++ {
			if (Tile{lat, lon}).Listed() {
				n++
			}
		}
	}
	if n != 60 {
		t.Fatalf("%d tiles listed, want 60", n)
	}
	// #738 research §3.2: every border tile N54-N57 / E008-E015 exists
	// (HTTP 200), and N55E012 holds Copenhagen and Malmö.
	for _, tl := range []Tile{{55, 12}, {55, 13}, {54, 8}, {54, 9}, {54, 10}, {54, 11}, {54, 12}, {54, 13}, {54, 14},
		{55, 14}, {55, 15}, {56, 11}, {56, 12}, {57, 11}} {
		if !tl.Listed() {
			t.Errorf("%v not listed", tl)
		}
	}
	// Open sea: no tile.
	for _, tl := range []Tile{{54, 6}, {55, 6}, {55, 7}, {55, 16}, {56, 6}, {56, 7}} {
		if tl.Listed() || !tl.InRegion() {
			t.Errorf("%v: listed %v, in region %v; want sea in the region", tl, tl.Listed(), tl.InRegion())
		}
	}
	// Outside the region nothing is listed, whatever the bucket has.
	for _, tl := range []Tile{{52, 12}, {59, 12}, {55, 5}, {55, 17}, {-55, 12}, {55, -12}} {
		if tl.Listed() || tl.InRegion() {
			t.Errorf("%v outside the region: listed %v, in region %v", tl, tl.Listed(), tl.InRegion())
		}
	}
}

func TestParseTilesRejectsMalformed(t *testing.T) {
	for _, text := range []string{
		"N55E12\n", "N55E0120\n", "S55E012\n", "N55W012\n", "N52E012\n", "N59E012\n", "N55E005\n", "N55E017\n",
		"n55e012\n", " N55E012x\n", "N55E012 N55E013\n", "",
		"N55E012\nN52E012\n", // one valid line does not excuse an out-of-region one
	} {
		if _, err := parseTiles(text); err == nil {
			t.Errorf("%q: accepted", text)
		}
	}
	got, err := parseTiles("# comment\n\nN53E006\nN58E016\n")
	if err != nil || !got.has(Tile{53, 6}) || !got.has(Tile{58, 16}) || got.has(Tile{55, 12}) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestTileAt(t *testing.T) {
	for _, c := range []struct {
		lat, lon float64
		want     Tile
		ok       bool
	}{
		{55.6761, 12.5683, Tile{55, 12}, true},
		{55, 12, Tile{55, 12}, true},
		{54.999999, 11.999999, Tile{54, 11}, true},
		{-0.5, -0.5, Tile{-1, -1}, true},
		{math.NaN(), 12, Tile{}, false},
		{55, math.Inf(1), Tile{}, false},
		{91, 12, Tile{}, false},
		{55, 181, Tile{}, false},
	} {
		got, ok := TileAt(c.lat, c.lon)
		if got != c.want || ok != c.ok {
			t.Errorf("TileAt(%v, %v) = %v, %v; want %v, %v", c.lat, c.lon, got, ok, c.want, c.ok)
		}
	}
}

func TestTilePathAllowList(t *testing.T) {
	for lat := regionSouth; lat <= regionNorth; lat++ {
		for lon := regionWest; lon <= regionEast; lon++ {
			tl := Tile{lat, lon}
			err := checkPath(tl.Path())
			if tl.Listed() != (err == nil) {
				t.Errorf("%v listed %v, checkPath %v", tl, tl.Listed(), err)
			}
		}
	}
	if p := (Tile{55, 12}).Path(); p != "/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif" {
		t.Fatalf("path %q", p)
	}
	for _, p := range []string{
		"",
		"/",
		"/tileList.txt",
		"Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif",
		"/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E013_00_DEM.tif",
		"/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N56_00_E012_00_DEM.tif",
		"/Copernicus_DSM_COG_10_N55_00_E016_00_DEM/Copernicus_DSM_COG_10_N55_00_E016_00_DEM.tif", // sea: not listed
		"/Copernicus_DSM_COG_10_N52_00_E012_00_DEM/Copernicus_DSM_COG_10_N52_00_E012_00_DEM.tif", // outside the region
		"/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif?x=1",
		"/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif/",
		"/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/../Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif",
		"/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E012_00_DEM.TIF",
		"/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/AUXFILES/Copernicus_DSM_COG_10_N55_00_E012_00_DEM_EDM.tif",
		"/Copernicus_DSM_COG_30_N55_00_E012_00_DEM/Copernicus_DSM_COG_30_N55_00_E012_00_DEM.tif",
		"//evil.example/Copernicus_DSM_COG_10_N55_00_E012_00_DEM/Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif",
	} {
		if err := checkPath(p); err == nil {
			t.Errorf("%q: allowed", p)
		}
	}
}

// The path regex alone is the strict allow-list of the hard rule: exactly
// the 66 region tiles' paths, before the tile list narrows it further.
func TestTilePathRegex(t *testing.T) {
	n := 0
	for lat := 50; lat <= 61; lat++ {
		for lon := 3; lon <= 19; lon++ {
			tl := Tile{lat, lon}
			if got := tilePathRe.MatchString(tl.Path()); got != tl.InRegion() {
				t.Errorf("%v: regex %v, in region %v", tl, got, tl.InRegion())
			} else if got {
				n++
			}
		}
	}
	if n != regionTiles {
		t.Fatalf("%d paths match, want %d", n, regionTiles)
	}
	p := (Tile{55, 12}).Path()
	for _, bad := range []string{p + "?x=1", p + "/", p + "\n", "/x" + p, "x" + p, p[:len(p)-1]} {
		if tilePathRe.MatchString(bad) {
			t.Errorf("regex matches %q", bad)
		}
	}
}

// The generator, run on a small test list, writes what parseTiles reads.
// Skipped without python3: the embedded list itself is pinned above.
func TestGeneratorOnTestList(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	cmd := exec.Command(py, filepath.Join("..", "..", "scripts", "gen-copdem-tiles.py"), filepath.Join("testdata", "tileList-sample.txt"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("generator: %v", err)
	}
	got, err := parseTiles(string(out))
	if err != nil {
		t.Fatalf("generator output does not parse: %v\n%s", err, out)
	}
	want := []Tile{{53, 6}, {55, 12}, {58, 16}}
	n := 0
	for lat := regionSouth; lat <= regionNorth; lat++ {
		for lon := regionWest; lon <= regionEast; lon++ {
			if got.has(Tile{lat, lon}) {
				n++
			}
		}
	}
	for _, tl := range want {
		if !got.has(tl) {
			t.Errorf("%v missing from the generated list", tl)
		}
	}
	if n != len(want) {
		t.Errorf("%d tiles generated, want %d (out-of-region and southern/western tiles must be dropped)\n%s", n, len(want), out)
	}
}
