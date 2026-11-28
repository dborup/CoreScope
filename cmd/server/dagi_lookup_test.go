package main

// #740 M1 — the DAGI lookup against the checked-in asset: known places per
// layer, islands, coast, sea and abroad, positions that are no position, and
// the border precedence.

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func dagiTestIndex(t testing.TB) *dagiIndex {
	t.Helper()
	idx, err := dagiDefault()
	if err != nil {
		t.Fatalf("embedded DAGI asset: %v", err)
	}
	return idx
}

// dagiCodes is a place as four codes, "" for none, for compact tables.
type dagiCodes struct{ kommune, landsdel, region, postnummer string }

func dagiCodesOf(p DagiPlace) dagiCodes {
	code := func(a *DagiArea) string {
		if a == nil {
			return ""
		}
		return a.Code
	}
	return dagiCodes{code(p.Kommune), code(p.Landsdel), code(p.Region), code(p.Postnummer)}
}

// Landsdel codes are DAGI id_lokalid (the source has no NUTS code).
const (
	dagiLandsdelByenKbh     = "218528"
	dagiLandsdelBornholm    = "218534"
	dagiLandsdelFyn         = "218540"
	dagiLandsdelSydjylland  = "218542"
	dagiLandsdelVestjylland = "218544"
	dagiLandsdelOestjylland = "218546"
	dagiLandsdelNordjylland = "218548"
)

func TestDagiLookupKnownPlaces(t *testing.T) {
	idx := dagiTestIndex(t)
	cases := []struct {
		name     string
		lat, lon float64
		want     dagiCodes
	}{
		{"Aarhus C", 56.1567, 10.2108, dagiCodes{"0751", dagiLandsdelOestjylland, "1082", "8000"}},
		{"København K, Kongens Nytorv", 55.6804, 12.5857, dagiCodes{"0101", dagiLandsdelByenKbh, "1084", "1050"}},
		{"København V, Rådhuspladsen", 55.6759, 12.5691, dagiCodes{"0101", dagiLandsdelByenKbh, "1084", "1550"}},
		{"Rønne", 55.1009, 14.7066, dagiCodes{"0400", dagiLandsdelBornholm, "1084", "3700"}},
		{"Skagen", 57.7209, 10.5839, dagiCodes{"0813", dagiLandsdelNordjylland, "1081", "9990"}},
		{"Odense C", 55.3959, 10.3883, dagiCodes{"0461", dagiLandsdelFyn, "1083", "5000"}},
		{"Aalborg", 57.0488, 9.9217, dagiCodes{"0851", dagiLandsdelNordjylland, "1081", "9000"}},
		{"Esbjerg", 55.4765, 8.4594, dagiCodes{"0561", dagiLandsdelSydjylland, "1083", "6700"}},
		// Frederiksberg is an enclave: a hole in København's polygon.
		{"Frederiksberg Rådhus (enclave)", 55.6789, 12.5323, dagiCodes{"0147", dagiLandsdelByenKbh, "1084", "2000"}},
		{"Valby (around the enclave)", 55.6625, 12.5160, dagiCodes{"0101", dagiLandsdelByenKbh, "1084", "2500"}},
		// Near the land border, on the Danish side.
		{"Padborg", 54.8260, 9.3640, dagiCodes{"0580", dagiLandsdelSydjylland, "1083", "6330"}},
		// On the spit between Ringkøbing Fjord and the North Sea.
		{"Hvide Sande", 56.0045, 8.1294, dagiCodes{"0760", dagiLandsdelVestjylland, "1082", "6960"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dagiCodesOf(idx.Lookup(c.lat, c.lon)); got != c.want {
				t.Errorf("Lookup(%v, %v) = %+v, want %+v", c.lat, c.lon, got, c.want)
			}
		})
	}
}

// The names come along with the codes, and the JSON shape is named fields
// with absent layers left out.
func TestDagiLookupNamesAndJSON(t *testing.T) {
	idx := dagiTestIndex(t)
	p := idx.Lookup(56.1567, 10.2108)
	if p.Kommune == nil || p.Kommune.Name != "Aarhus" || p.Landsdel.Name != "Østjylland" ||
		p.Region.Name != "Region Midtjylland" || p.Postnummer.Name != "Aarhus C" {
		t.Fatalf("names = %+v %+v %+v %+v", p.Kommune, p.Landsdel, p.Region, p.Postnummer)
	}
	raw, err := json.Marshal(idx.Lookup(56.0, 7.0)) // North Sea: postnummer only
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"postnummer":{"code":"6960","name":"Hvide Sande"}}` {
		t.Errorf("JSON = %s", raw)
	}
}

// Small islands survive the simplification (#740: Anholt, Christiansø,
// Læsø, Samsø).
func TestDagiLookupIslands(t *testing.T) {
	idx := dagiTestIndex(t)
	cases := []struct {
		name     string
		lat, lon float64
		want     dagiCodes
	}{
		{"Anholt", 56.7150, 11.5538, dagiCodes{"0707", dagiLandsdelOestjylland, "1082", "8592"}},
		// Christiansø is DAGI's area outside the kommune division; it is in
		// Region Hovedstaden (its regionlokalid) and landsdel Bornholm.
		{"Christiansø", 55.3204, 15.1876, dagiCodes{"0411", dagiLandsdelBornholm, "1084", "3760"}},
		{"Læsø, Byrum", 57.2550, 11.0000, dagiCodes{"0825", dagiLandsdelNordjylland, "1081", "9940"}},
		{"Samsø, Tranebjerg", 55.8366, 10.5964, dagiCodes{"0741", dagiLandsdelOestjylland, "1082", "8305"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dagiCodesOf(idx.Lookup(c.lat, c.lon)); got != c.want {
				t.Errorf("Lookup(%v, %v) = %+v, want %+v", c.lat, c.lon, got, c.want)
			}
		})
	}
	if ch := idx.Lookup(55.3204, 15.1876).Kommune; ch == nil || ch.Name != "Christiansø" {
		t.Errorf("Christiansø: %+v", ch)
	}
}

// Coast, sea and abroad: kommune, landsdel and region are land; a point off
// the coast is in none of them. Postnummer polygons reach into Danish
// waters as DAGI draws them, so offshore can still have a postnummer.
func TestDagiLookupCoastSeaAbroad(t *testing.T) {
	idx := dagiTestIndex(t)
	cases := []struct {
		name     string
		lat, lon float64
		want     dagiCodes
	}{
		{"2.5 km off Hvide Sande", 56.0045, 8.0900, dagiCodes{"", "", "", "6960"}},
		{"Aarhus Bugt", 56.1500, 10.3000, dagiCodes{"", "", "", "8000"}},
		{"North Sea, 56N 7E", 56.0, 7.0, dagiCodes{"", "", "", "6960"}},
		{"Kattegat west of Anholt", 56.6, 12.0, dagiCodes{"", "", "", "8592"}},
		{"Skagerrak, beyond the postnummer sea", 58.5, 9.0, dagiCodes{}},
		{"Flensburg", 54.7833, 9.4333, dagiCodes{}},
		{"Schleswig", 54.5200, 9.5600, dagiCodes{}},
		{"Malmö", 55.6050, 13.0038, dagiCodes{}},
		{"Between Bornholm and Christiansø", 55.2500, 15.1000, dagiCodes{"", "", "", "3760"}},
		{"Null Island", 0.001, 0.001, dagiCodes{}},
		{"Southern hemisphere", -56.1567, 10.2108, dagiCodes{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dagiCodesOf(idx.Lookup(c.lat, c.lon)); got != c.want {
				t.Errorf("Lookup(%v, %v) = %+v, want %+v", c.lat, c.lon, got, c.want)
			}
		})
	}
}

// (0, 0) is MeshCore's "no position", and NaN and ±Inf are no position
// either: no membership, and no panic.
func TestDagiLookupNoPosition(t *testing.T) {
	idx := dagiTestIndex(t)
	nan, inf := math.NaN(), math.Inf(1)
	for _, p := range [][2]float64{{0, 0}, {nan, 10.2}, {56.1, nan}, {nan, nan}, {inf, 10.2}, {56.1, -inf}, {91, 10}, {56, 181}} {
		if got := dagiCodesOf(idx.Lookup(p[0], p[1])); got != (dagiCodes{}) {
			t.Errorf("Lookup(%v, %v) = %+v, want none", p[0], p[1], got)
		}
	}
}

// dagiTestFeatureVertices decodes one feature's vertices from the asset, in
// asset units.
func dagiTestFeatureVertices(t testing.TB, l DagiLayer, code string) map[[2]int64]bool {
	t.Helper()
	file := dagiTestAsset(t, l)
	for _, f := range file.Features {
		if f.Code != code {
			continue
		}
		out := map[[2]int64]bool{}
		for _, r := range f.Rings {
			var x, y int64
			for i := 0; i < len(r); i += 2 {
				x, y = x+r[i], y+r[i+1]
				out[[2]int64{x, y}] = true
			}
		}
		return out
	}
	t.Fatalf("%s %s not in the asset", l, code)
	return nil
}

func dagiTestAsset(t testing.TB, l DagiLayer) dagiAssetFile {
	t.Helper()
	raw, err := dagiAssets.ReadFile("dagi/dagi-" + l.String() + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var file dagiAssetFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// dagiTestSharedVertex is a vertex both features have: a point exactly on
// their common border.
func dagiTestSharedVertex(t testing.TB, l DagiLayer, a, b string) (lat, lon float64) {
	t.Helper()
	va, vb := dagiTestFeatureVertices(t, l, a), dagiTestFeatureVertices(t, l, b)
	var shared [][2]int64
	for v := range va {
		if vb[v] {
			shared = append(shared, v)
		}
	}
	if len(shared) == 0 {
		t.Fatalf("%s %s and %s share no vertex", l, a, b)
	}
	sort.Slice(shared, func(i, j int) bool {
		return shared[i][0] < shared[j][0] || shared[i][0] == shared[j][0] && shared[i][1] < shared[j][1]
	})
	v := shared[len(shared)/2]
	return float64(v[1]) * 1e-5, float64(v[0]) * 1e-5
}

// A point on a shared border is in both neighbours; the smaller source area
// wins, then the lower code, whatever the order in the file. The pairs are
// picked so that area and code disagree (Aarhus 0751 is smaller than
// Favrskov 0710); one is a street postcode against its regular neighbour.
func TestDagiLookupBorderTieBreak(t *testing.T) {
	idx := dagiTestIndex(t)
	cases := []struct {
		layer      DagiLayer
		a, b, want string
	}{
		{DagiKommune, "0751", "0710", "0751"},        // Aarhus 471 km² < Favrskov 540 km²
		{DagiKommune, "0101", "0147", "0147"},        // Frederiksberg, the enclave, is smaller
		{DagiPostnummer, "1433", "2300", "2300"},     // regular København S (48 km²) < street 1433 (103 km², mostly sea)
		{DagiLandsdel, "218546", "218544", "218546"}, // Østjylland 5,916 km² < Vestjylland 7,229 km²
	}
	for _, c := range cases {
		lat, lon := dagiTestSharedVertex(t, c.layer, c.a, c.b)
		li := &idx.layers[c.layer]
		fa, fb := &li.features[li.byCode[c.a]], &li.features[li.byCode[c.b]]
		x, y, _ := idx.units(lat, lon)
		if !fa.contains(x, y) || !fb.contains(x, y) {
			t.Fatalf("%s %s/%s: the shared vertex (%v, %v) is not on both borders", c.layer, c.a, c.b, lat, lon)
		}
		want := c.want
		at := idx.locate(lat, lon)[c.layer]
		if got := li.features[at].area.Code; got != want {
			t.Errorf("%s border %s/%s at (%v, %v) = %s, want %s", c.layer, c.a, c.b, lat, lon, got, want)
		}
		// The same layer built from the features in reverse file order
		// answers the same.
		file := dagiTestAsset(t, c.layer)
		for i, j := 0, len(file.Features)-1; i < j; i, j = i+1, j-1 {
			file.Features[i], file.Features[j] = file.Features[j], file.Features[i]
		}
		rev, err := buildDagiLayer(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := rev.features[rev.find(x, y)].area.Code; got != want {
			t.Errorf("%s border %s/%s, reversed file order = %s, want %s", c.layer, c.a, c.b, got, want)
		}
	}
}

// dagiPrecedes is a strict total order: smaller area first, then the lower
// code.
func TestDagiPrecedence(t *testing.T) {
	f := func(code string, area float64) *dagiFeature {
		return &dagiFeature{area: DagiArea{Code: code}, areaM2: area}
	}
	cases := []struct {
		a, b *dagiFeature
		want bool
	}{
		{f("0710", 1), f("0101", 2), true},  // smaller area wins over a lower code
		{f("0101", 2), f("0710", 1), false}, //
		{f("0101", 5), f("0710", 5), true},  // equal area: lower code
		{f("0710", 5), f("0101", 5), false}, //
		{f("0101", 5), f("0101", 5), false}, // irreflexive
	}
	for _, c := range cases {
		if got := dagiPrecedes(c.a, c.b); got != c.want {
			t.Errorf("dagiPrecedes(%s/%v, %s/%v) = %v, want %v", c.a.area.Code, c.a.areaM2, c.b.area.Code, c.b.areaM2, got, c.want)
		}
	}
}

// The point-in-polygon test on a hand-made feature: inside, outside, on an
// edge and a vertex (inside: the boundary counts), in a hole, and NaN.
func TestDagiFeatureContains(t *testing.T) {
	// A 10 x 10 square with a 4 x 4 hole, in units.
	f, err := buildDagiFeature(dagiAssetFeature{Code: "x", AreaM2: 1, Rings: [][]int64{
		{0, 0, 10, 0, 0, 10, -10, 0},
		{3, 3, 4, 0, 0, 4, -4, 0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		x, y float64
		want bool
	}{
		{1, 1, true}, {9, 9, true}, {5, 1, true},
		{5, 5, false},              // in the hole
		{3, 5, true}, {7, 7, true}, // on the hole's edge / vertex
		{0, 5, true}, {10, 10, true}, // on the outer edge / vertex
		{-1, 5, false}, {11, 5, false}, {5, -0.01, false}, {5, 10.01, false},
		{math.NaN(), 5, false}, {5, math.NaN(), false},
	}
	for _, c := range cases {
		if got := f.contains(c.x, c.y); got != c.want {
			t.Errorf("contains(%v, %v) = %v, want %v", c.x, c.y, got, c.want)
		}
	}
}

// A malformed asset is refused rather than half-loaded.
func TestDagiBuildFeatureRejectsMalformed(t *testing.T) {
	for name, af := range map[string]dagiAssetFeature{
		"no code":      {Rings: [][]int64{{0, 0, 1, 0, 0, 1}}},
		"no rings":     {Code: "x"},
		"odd values":   {Code: "x", Rings: [][]int64{{0, 0, 1, 0, 0}}},
		"two vertices": {Code: "x", Rings: [][]int64{{0, 0, 1, 0}}},
	} {
		if _, err := buildDagiFeature(af); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := buildDagiLayer(dagiAssetFile{}); err == nil {
		t.Error("an empty layer was accepted")
	}
	sq := [][]int64{{0, 0, 1, 0, 0, 1}}
	dup := dagiAssetFile{Features: []dagiAssetFeature{{Code: "a", Rings: sq}, {Code: "a", Rings: sq}}}
	if _, err := buildDagiLayer(dup); err == nil {
		t.Error("a duplicate code was accepted")
	}
}

// units refuses what is no position before any float-to-grid conversion
// (Go leaves int(NaN) to the implementation).
func TestDagiUnitsRefusesNoPosition(t *testing.T) {
	idx := dagiTestIndex(t)
	for _, p := range [][2]float64{{0, 0}, {math.NaN(), 10}, {56, math.Inf(-1)}, {-91, 10}} {
		if _, _, ok := idx.units(p[0], p[1]); ok {
			t.Errorf("units(%v, %v) ok", p[0], p[1])
		}
	}
	if x, y, ok := idx.units(56.1567, 10.2108); !ok || math.Abs(x-1021080) > 1e-6 || math.Abs(y-5615670) > 1e-6 {
		t.Errorf("units(Aarhus) = %v, %v, %v", x, y, ok)
	}
}

// loadDagiIndex refuses a set of files that do not belong together.
func TestLoadDagiIndexRejects(t *testing.T) {
	files := map[DagiLayer]dagiAssetFile{}
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		files[l] = dagiTestAsset(t, l)
	}
	write := func(mut func(map[DagiLayer]*dagiAssetFile)) fstest.MapFS {
		copies := map[DagiLayer]*dagiAssetFile{}
		for l, f := range files {
			c := f
			c.Features = append([]dagiAssetFeature(nil), f.Features...)
			copies[l] = &c
		}
		mut(copies)
		fsys := fstest.MapFS{}
		for l, f := range copies {
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			fsys["d/dagi-"+l.String()+".json"] = &fstest.MapFile{Data: raw}
		}
		return fsys
	}
	if _, err := loadDagiIndex(write(func(map[DagiLayer]*dagiAssetFile) {}), "d"); err != nil {
		t.Fatalf("the unchanged files are refused: %v", err)
	}
	cases := map[string]func(map[DagiLayer]*dagiAssetFile){
		"kommune in an unknown region": func(f map[DagiLayer]*dagiAssetFile) { f[DagiKommune].Features[0].Region = "9999" },
		"wrong format":                 func(f map[DagiLayer]*dagiAssetFile) { f[DagiRegion].Format = "corescope-dagi/0" },
		"layer in the wrong file":      func(f map[DagiLayer]*dagiAssetFile) { f[DagiLandsdel].Layer = "region" },
		"quantum differs":              func(f map[DagiLayer]*dagiAssetFile) { f[DagiPostnummer].Quantum = 1e-4 },
		"no quantum":                   func(f map[DagiLayer]*dagiAssetFile) { f[DagiKommune].Quantum = 0 },
		"empty layer":                  func(f map[DagiLayer]*dagiAssetFile) { f[DagiRegion].Features = nil },
		"credit differs": func(f map[DagiLayer]*dagiAssetFile) {
			f[DagiPostnummer].Credit = strings.Replace(f[DagiPostnummer].Credit, "november", "oktober", 1)
		},
		"credit differs in landsdel": func(f map[DagiLayer]*dagiAssetFile) {
			f[DagiLandsdel].Credit += " "
		},
		"credit differs in region": func(f map[DagiLayer]*dagiAssetFile) {
			f[DagiRegion].Credit += " "
		},
		"generation differs": func(f map[DagiLayer]*dagiAssetFile) { f[DagiLandsdel].Source.Generation++ },
		"downloaded differs": func(f map[DagiLayer]*dagiAssetFile) { f[DagiRegion].Source.Downloaded = "2026-11-25" },
		"first file differs": func(f map[DagiLayer]*dagiAssetFile) { f[DagiKommune].Source.Generation-- },
		"no credit in any file": func(f map[DagiLayer]*dagiAssetFile) {
			for _, file := range f {
				file.Credit = ""
			}
		},
	}
	for name, mut := range cases {
		if _, err := loadDagiIndex(write(mut), "d"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	missing := write(func(map[DagiLayer]*dagiAssetFile) {})
	delete(missing, "d/dagi-postnummer.json")
	if _, err := loadDagiIndex(missing, "d"); err == nil {
		t.Error("a missing layer file was accepted")
	}
	bad := write(func(map[DagiLayer]*dagiAssetFile) {})
	bad["d/dagi-kommune.json"] = &fstest.MapFile{Data: []byte("{")}
	if _, err := loadDagiIndex(bad, "d"); err == nil {
		t.Error("malformed JSON was accepted")
	}
}
