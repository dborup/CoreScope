package main

// #740 M1 — the checked-in DAGI asset: counts, provenance and credit, valid
// rings, size budget, and the topology the build promises (no overlaps, no
// gaps between neighbours, every kommune inside its region).

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// dagiAssetSHA256 pins the generated files. Regenerating them with
// scripts/build-dagi.py is the only way they change; update these then.
var dagiAssetSHA256 = map[DagiLayer]string{
	DagiKommune:    "0e924c9d3593d01a5096785740bc33484c3806f0a996e0139745826e8895f33e",
	DagiLandsdel:   "2306f181f479dee897de93628bb19e4b309adb1e892e2c832a8c9af9b510a9dc",
	DagiRegion:     "e17bbca24185bb529c51df90cc1481bece046951f4a470c141dbe2a8248402d6",
	DagiPostnummer: "1e1a544ffb6845eddb08aa442dd44f998a93318858f8d8ebc7df214f1533c4fa",
}

// The #740 budget: per layer at most 400 KB raw and 150 KB gzipped on the
// wire (the server compresses at gzip.DefaultCompression, compress.go).
const (
	dagiMaxRawBytes  = 400 * 1024
	dagiMaxGzipBytes = 150 * 1024
)

func TestDagiAssetCountsAndProvenance(t *testing.T) {
	want := map[DagiLayer]struct {
		entity   string
		features int
	}{
		DagiKommune:    {"Kommuneinddeling", 99}, // 98 kommuner + Christiansø
		DagiLandsdel:   {"Landsdel", 11},
		DagiRegion:     {"Regionsinddeling", 5},
		DagiPostnummer: {"Postnummerinddeling", 1089},
	}
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		f := dagiTestAsset(t, l)
		w := want[l]
		if len(f.Features) != w.features || f.Source.Features != w.features {
			t.Errorf("%s: %d features (source %d), want %d", l, len(f.Features), f.Source.Features, w.features)
		}
		src := f.Source
		if src.Register != "DAGI" || src.Entity != w.entity || src.Generation != 271 || src.TypeOfData != "Current" ||
			src.Downloaded != "2026-11-26" || len(src.SHA256) != 64 ||
			src.File != "DAGI_V4_"+w.entity+"_TotalDownload_gpkg_Current_271.gpkg" {
			t.Errorf("%s: source = %+v", l, src)
		}
		if f.Format != dagiFormat || f.Layer != l.String() || f.Quantum != 1e-5 {
			t.Errorf("%s: format %q layer %q quantum %v", l, f.Format, f.Layer, f.Quantum)
		}
		// The recorded accuracy: 60 m for the shared kommune/landsdel/
		// region topology, 225 m for postnummer, never exceeded.
		tol := 60.0
		if l == DagiPostnummer {
			tol = 225
		}
		if f.Simplify.ToleranceM != tol || !(f.Simplify.MaxDeviationM > 0) || f.Simplify.MaxDeviationM > tol {
			t.Errorf("%s: simplify %+v, want tolerance %v m and a deviation within it", l, f.Simplify, tol)
		}
	}
	// Exactly one area outside the kommune division: Christiansø, in Region
	// Hovedstaden. The 98 others are the kommuner.
	var outside []string
	for _, f := range dagiTestAsset(t, DagiKommune).Features {
		if f.OutsideKommune {
			outside = append(outside, f.Code+" "+f.Name+" "+f.Region)
		}
	}
	if len(outside) != 1 || outside[0] != "0411 Christiansø 1084" {
		t.Errorf("outside the kommune division: %v, want [0411 Christiansø 1084]", outside)
	}
	street := 0
	for _, f := range dagiTestAsset(t, DagiPostnummer).Features {
		if f.Street {
			street++
		}
	}
	if street != 486 {
		t.Errorf("street postcodes = %d, want 486", street)
	}
}

// The credit Dataforsyningen's terms require (#733 wording: owner, dataset,
// licence, changed), the fetch month, and the links, in every file.
func TestDagiAssetCredit(t *testing.T) {
	const credit = "Indeholder data fra Klimadatastyrelsen, DAGI, november 2026, CC BY 4.0 — data bearbejdet"
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		f := dagiTestAsset(t, l)
		if f.Credit != credit {
			t.Errorf("%s: credit %q, want %q", l, f.Credit, credit)
		}
		if f.Owner != "Klimadatastyrelsen" || f.License != "CC BY 4.0" ||
			f.LicenseURL != "https://creativecommons.org/licenses/by/4.0/" ||
			f.DatasetURL != "https://dataforsyningen.dk/data/3901" || f.TermsURL != "https://dataforsyningen.dk/Vilkaar" ||
			!strings.HasPrefix(f.Dataset, "DAGI") {
			t.Errorf("%s: owner %q dataset %q %q licence %q %q terms %q", l, f.Owner, f.Dataset, f.DatasetURL, f.License, f.LicenseURL, f.TermsURL)
		}
	}
	if idx := dagiTestIndex(t); idx.credit != credit {
		t.Errorf("index credit = %q", idx.credit)
	}
}

// Every ring has at least three distinct vertices, no repeated consecutive
// vertex (closing included), and lies in Denmark's area: kommune, landsdel
// and region on land, postnummer out to its sea limits.
func TestDagiAssetRingsValid(t *testing.T) {
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		minX, maxX, minY, maxY := int64(800000), int64(1530000), int64(5450000), int64(5800000)
		if l == DagiPostnummer {
			minX, maxX, minY, maxY = 250000, 1700000, 5400000, 5850000
		}
		for _, f := range dagiTestAsset(t, l).Features {
			for ri, r := range f.Rings {
				var x, y int64
				var pts [][2]int64
				seen := map[[2]int64]bool{}
				for i := 0; i+1 < len(r); i += 2 {
					x, y = x+r[i], y+r[i+1]
					pts = append(pts, [2]int64{x, y})
					seen[[2]int64{x, y}] = true
					if x < minX || x > maxX || y < minY || y > maxY {
						t.Errorf("%s %s ring %d: vertex (%d, %d) outside Denmark", l, f.Code, ri, x, y)
					}
				}
				if len(r)%2 != 0 || len(seen) < 3 {
					t.Errorf("%s %s ring %d: %d values, %d distinct vertices", l, f.Code, ri, len(r), len(seen))
				}
				for i := range pts {
					if pts[i] == pts[(i+1)%len(pts)] {
						t.Errorf("%s %s ring %d: vertex %d repeated", l, f.Code, ri, i)
					}
				}
			}
			if !(f.AreaM2 > 0) {
				t.Errorf("%s %s: area %v", l, f.Code, f.AreaM2)
			}
		}
	}
}

// The size budget, measured as the server would send it, and the pinned
// checksums.
func TestDagiAssetSizeBudget(t *testing.T) {
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		raw, err := dagiAssets.ReadFile("dagi/dagi-" + l.String() + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
		zw.Write(raw)
		zw.Close()
		t.Logf("%-10s %7d B raw, %6d B gzip", l, len(raw), buf.Len())
		if len(raw) > dagiMaxRawBytes || buf.Len() > dagiMaxGzipBytes {
			t.Errorf("%s: %d B raw / %d B gzip, budget %d / %d", l, len(raw), buf.Len(), dagiMaxRawBytes, dagiMaxGzipBytes)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != dagiAssetSHA256[l] {
			t.Errorf("%s: sha256 %s, pinned %s (regenerated? update dagiAssetSHA256)", l, got, dagiAssetSHA256[l])
		}
	}
}

// dagiTestMatches is every feature of the layer that holds (x, y).
func dagiTestMatches(li *dagiLayerIndex, x, y float64) []int32 {
	cx, cy := dagiCell(x)-li.minCX, dagiCell(y)-li.minCY
	if cx < 0 || cy < 0 || cx >= li.nx || cy >= li.ny {
		return nil
	}
	var out []int32
	for _, i := range li.cells[cy*li.nx+cx] {
		if li.features[i].contains(x, y) {
			out = append(out, i)
		}
	}
	return out
}

// dagiTestOnBoundary reports whether (x, y) is on an edge of f.
func dagiTestOnBoundary(f *dagiFeature, x, y float64) bool {
	for i := range f.segs {
		if dagiOnSegment(&f.segs[i], x, y) {
			return true
		}
	}
	return false
}

// Exact: no vertex of a feature lies strictly inside another feature of the
// same layer (on its border is how neighbours meet), so the simplification
// swallowed no islet and no stretch of coast.
func TestDagiAssetNoVertexInsideNeighbour(t *testing.T) {
	idx := dagiTestIndex(t)
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		li := &idx.layers[l]
		bad := 0
		for i := range li.features {
			f := &li.features[i]
			for _, s := range f.segs {
				for _, j := range dagiTestMatches(li, s.x0, s.y0) {
					if j != int32(i) && !dagiTestOnBoundary(&li.features[j], s.x0, s.y0) {
						if bad++; bad <= 5 {
							t.Errorf("%s: vertex (%v, %v) of %s lies inside %s", l, s.x0, s.y0, f.area.Code, li.features[j].area.Code)
						}
					}
				}
			}
		}
		if bad > 0 {
			t.Errorf("%s: %d vertices inside a neighbour", l, bad)
		}
	}
}

// Exact: every vertex of a kommune is in its own region (border included).
// With the shared arcs and no vertex inside a neighbour, the kommune lies
// in its region.
func TestDagiAssetKommuneInsideItsRegion(t *testing.T) {
	idx := dagiTestIndex(t)
	regions := &idx.layers[DagiRegion]
	for _, k := range idx.layers[DagiKommune].features {
		ri, ok := regions.byCode[k.region]
		if !ok {
			t.Errorf("kommune %s: region %q unknown", k.area.Code, k.region)
			continue
		}
		r := &regions.features[ri]
		out := 0
		for _, s := range k.segs {
			if !r.contains(s.x0, s.y0) {
				out++
			}
		}
		if out > 0 {
			t.Errorf("kommune %s %s: %d of %d vertices outside region %s", k.area.Code, k.area.Name, out, len(k.segs), k.region)
		}
	}
}

// Sampled on a grid over Denmark (0.02° x 0.01°, about 1.39 km² a point):
// within a layer no point is in two features (no overlap), and the three
// nested layers have no gap against each other (a point in a region is in
// a kommune and a landsdel, and back). Each kommune's points share one
// landsdel and its own region. Postnummer is its own simplification, so
// against the kommuner its gap is measured and bounded instead.
func TestDagiAssetSampledOverlapAndGaps(t *testing.T) {
	idx := dagiTestIndex(t)
	const (
		step         = 2000.0 // units of x (0.02°); y uses half
		maxPostnrGap = 3      // sample points in a kommune with no postnummer
	)
	overlaps := [dagiLayerCount]int{}
	nestedGaps, regionMismatch, postnrGap, land := 0, 0, 0, 0
	kommuneLandsdel := map[int32]int32{}
	for y := 5450000.0 + step/4; y < 5800000; y += step / 2 {
		for x := 800000.0 + step/2; x < 1530000; x += step {
			var at [dagiLayerCount][]int32
			for l := range idx.layers {
				at[l] = dagiTestMatches(&idx.layers[l], x, y)
				if len(at[l]) > 1 {
					overlaps[l]++
				}
			}
			k, ld, r := len(at[DagiKommune]) > 0, len(at[DagiLandsdel]) > 0, len(at[DagiRegion]) > 0
			if k != ld || k != r {
				nestedGaps++
				continue
			}
			if !k {
				continue
			}
			land++
			kf := &idx.layers[DagiKommune].features[at[DagiKommune][0]]
			if idx.layers[DagiRegion].features[at[DagiRegion][0]].area.Code != kf.region {
				regionMismatch++
			}
			if prev, ok := kommuneLandsdel[at[DagiKommune][0]]; ok && prev != at[DagiLandsdel][0] {
				t.Errorf("kommune %s spans landsdele %s and %s", kf.area.Code,
					idx.layers[DagiLandsdel].features[prev].area.Code, idx.layers[DagiLandsdel].features[at[DagiLandsdel][0]].area.Code)
			}
			kommuneLandsdel[at[DagiKommune][0]] = at[DagiLandsdel][0]
			if len(at[DagiPostnummer]) == 0 {
				postnrGap++
			}
		}
	}
	t.Logf("land samples %d; overlaps %v; nested gaps %d; region mismatches %d; land without postnummer %d",
		land, overlaps, nestedGaps, regionMismatch, postnrGap)
	if land < 25000 {
		t.Errorf("only %d land samples: the grid missed Denmark", land)
	}
	for l, n := range overlaps {
		if n > 0 {
			t.Errorf("%s: %d sample points in two features", DagiLayer(l), n)
		}
	}
	if nestedGaps > 0 || regionMismatch > 0 {
		t.Errorf("nested layers: %d gaps, %d points in another region than their kommune's", nestedGaps, regionMismatch)
	}
	if postnrGap > maxPostnrGap {
		t.Errorf("%d land samples have no postnummer, tolerance %d", postnrGap, maxPostnrGap)
	}
	if len(kommuneLandsdel) < 98 {
		t.Errorf("only %d kommuner sampled", len(kommuneLandsdel))
	}
}

// dagiTestEdges counts every undirected edge of a layer.
func dagiTestEdges(t testing.TB, l DagiLayer) map[[4]int64]int {
	t.Helper()
	edges := map[[4]int64]int{}
	for _, f := range dagiTestAsset(t, l).Features {
		for _, r := range f.Rings {
			var x, y int64
			var pts [][2]int64
			for i := 0; i+1 < len(r); i += 2 {
				x, y = x+r[i], y+r[i+1]
				pts = append(pts, [2]int64{x, y})
			}
			for i, a := range pts {
				b := pts[(i+1)%len(pts)]
				if b[0] < a[0] || b[0] == a[0] && b[1] < a[1] {
					a, b = b, a
				}
				edges[[4]int64{a[0], a[1], b[0], b[1]}]++
			}
		}
	}
	return edges
}

// Exact: neighbours share their border edge for edge (no edge is used by
// more than two features), and kommune, landsdel and region have the same
// outer edges (coast and land border): an edge that only one kommune has
// is an edge that only one region has. A border simplified differently on
// its two sides, or a kommune border that does not meet its region's,
// leaves an unshared edge in one layer and not the other: a gap or an
// overlap of any size, which the sampled test below can miss.
func TestDagiAssetSharedBorders(t *testing.T) {
	outer := map[DagiLayer]map[[4]int64]bool{}
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		outer[l] = map[[4]int64]bool{}
		for e, n := range dagiTestEdges(t, l) {
			if n > 2 {
				t.Errorf("%s: edge %v used %d times", l, e, n)
			}
			if n == 1 {
				outer[l][e] = true
			}
		}
	}
	for _, l := range []DagiLayer{DagiLandsdel, DagiRegion} {
		diff := 0
		for e := range outer[DagiKommune] {
			if !outer[l][e] {
				diff++
			}
		}
		for e := range outer[l] {
			if !outer[DagiKommune][e] {
				diff++
			}
		}
		if diff > 0 {
			t.Errorf("kommune and %s outer edges differ by %d (of %d)", l, diff, len(outer[DagiKommune]))
		}
	}
	// Postnummer tiles Denmark with its sea: its outer edges are the sea
	// limit and the land border with Germany (54.80–54.91 N). One on land
	// anywhere else is a border its two postcodes do not share.
	idx := dagiTestIndex(t)
	for e := range outer[DagiPostnummer] {
		x, y := float64(e[0]+e[2])/2, float64(e[1]+e[3])/2
		if y > 5492000 && idx.layers[DagiKommune].find(x, y) >= 0 {
			t.Errorf("postnummer outer edge at (%v, %v) is on land, away from the border", y/1e5, x/1e5)
		}
	}
	t.Logf("outer edges: kommune %d, landsdel %d, region %d, postnummer %d",
		len(outer[DagiKommune]), len(outer[DagiLandsdel]), len(outer[DagiRegion]), len(outer[DagiPostnummer]))
}
