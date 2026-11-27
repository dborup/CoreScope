package main

// #740 M1 — DAGI: which kommune, landsdel, region and postnummer a point is
// in, from Klimadatastyrelsen's administrative boundaries, offline.
//
// The polygons are simplified, checked-in derivatives of the owner's one-off
// Datafordeler download (DAGI V4, generation 271), made by
// scripts/build-dagi.py, one JSON file per layer in dagi/. Every file
// carries its source, licence and credit in its header; the credit must go
// wherever the data is shown (M2/M3).
//
// Kommune, landsdel and region were simplified as one topology, so their
// shared borders are the same polylines and a kommune lies exactly in its
// region; postnummer is its own topology. A point exactly on a border is in
// both neighbours, and the layer's precedence decides: the feature with the
// smaller source area wins, then the lower code. Street postcodes
// (ergadepostnummer) tile with the others in the source, so the same rule
// is all they need.
//
// The index is read-only and in memory; nothing here touches the database
// (#1283). M1 adds no API: M2 wires the per-node cache (dagi_membership.go)
// into the filters. Config for later (AGENTS.md rule 8): a dagi.enabled
// switch, and an asset override path (loadDagiIndex takes any fs.FS).

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"sync"
)

//go:embed dagi/dagi-*.json
var dagiAssets embed.FS

// DagiLayer is one DAGI entity the lookup reports.
type DagiLayer int

const (
	DagiKommune DagiLayer = iota
	DagiLandsdel
	DagiRegion
	DagiPostnummer
	dagiLayerCount
)

// dagiLayerNames are the layers' names in the asset files (dagi-<name>.json).
var dagiLayerNames = [dagiLayerCount]string{"kommune", "landsdel", "region", "postnummer"}

func (l DagiLayer) String() string {
	if l < 0 || l >= dagiLayerCount {
		return fmt.Sprintf("DagiLayer(%d)", int(l))
	}
	return dagiLayerNames[l]
}

// dagiFormat is the asset format scripts/build-dagi.py writes.
const dagiFormat = "corescope-dagi/1"

// DagiArea is one DAGI feature as the lookup reports it: its stable code
// (kommunekode, landsdel id_lokalid, regionskode, postnummer) and its name.
type DagiArea struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// DagiPlace is the DAGI membership of one point. A nil layer means the point
// is in no feature of it: abroad, at sea, or without a position. Postnummer
// polygons reach into the sea (as DAGI draws them), so a point offshore can
// have a postnummer and no kommune. The pointers are shared and read-only.
type DagiPlace struct {
	Kommune    *DagiArea `json:"kommune,omitempty"`
	Landsdel   *DagiArea `json:"landsdel,omitempty"`
	Region     *DagiArea `json:"region,omitempty"`
	Postnummer *DagiArea `json:"postnummer,omitempty"`
}

// dagiAssetFile is one layer file as scripts/build-dagi.py writes it.
type dagiAssetFile struct {
	Format     string             `json:"format"`
	Layer      string             `json:"layer"`
	Source     dagiAssetSource    `json:"source"`
	Credit     string             `json:"credit"`
	Owner      string             `json:"owner"`
	Dataset    string             `json:"dataset"`
	DatasetURL string             `json:"datasetUrl"`
	License    string             `json:"license"`
	LicenseURL string             `json:"licenseUrl"`
	TermsURL   string             `json:"termsUrl"`
	Quantum    float64            `json:"quantum"`
	Simplify   dagiAssetSimplify  `json:"simplify"`
	Features   []dagiAssetFeature `json:"features"`
}

type dagiAssetSource struct {
	Register   string `json:"register"`
	Entity     string `json:"entity"`
	Generation int    `json:"generation"`
	TypeOfData string `json:"typeOfData"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	LastChange string `json:"lastChange"`
	Features   int    `json:"features"`
	Downloaded string `json:"downloaded"`
}

// dagiAssetSimplify is the accuracy the build recorded: the Douglas-Peucker
// tolerance and the largest measured distance of a dropped source vertex
// from the simplified border, in metres (plus at most 0.56 m quantisation).
type dagiAssetSimplify struct {
	ToleranceM    float64 `json:"toleranceM"`
	MaxDeviationM float64 `json:"maxDeviationM"`
}

type dagiAssetFeature struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Region is a kommune's regionskode.
	Region string `json:"region,omitempty"`
	// OutsideKommune marks Christiansø, which DAGI lists as an area outside
	// the kommune division (udenforkommuneinddeling).
	OutsideKommune bool `json:"outsideKommune,omitempty"`
	// Street marks a street postcode (ergadepostnummer).
	Street bool    `json:"street,omitempty"`
	AreaM2 float64 `json:"areaM2"`
	// Rings are [lon0, lat0, dlon, dlat, ...] in Quantum degrees: the first
	// vertex absolute, then deltas, the closing vertex implicit.
	Rings [][]int64 `json:"rings"`
}

// dagiSeg is one polygon edge in asset units (Quantum degrees).
type dagiSeg struct{ x0, y0, x1, y1 float64 }

// dagiFeature is one feature and its point-in-polygon index: the edges of
// all its rings (even-odd), bucketed in horizontal bands so a test reads
// only the edges whose latitude range meets the point's band.
type dagiFeature struct {
	area                   DagiArea
	region                 string
	areaM2                 float64
	minX, minY, maxX, maxY float64
	bandH                  float64
	bands                  [][]int32
	segs                   []dagiSeg
}

// dagiSegsPerBand is the average number of edges a band is sized for.
const dagiSegsPerBand = 4

// dagiCellUnits is the edge of a layer grid cell (0.1°): a cell lists the
// features whose bbox meets it, so a lookup bbox-tests only those.
const dagiCellUnits = 10000

// dagiLayerIndex is one layer: its features in precedence order (smaller
// source area first, then the lower code) and the grid of candidates.
type dagiLayerIndex struct {
	features []dagiFeature
	byCode   map[string]int32
	minCX    int
	minCY    int
	nx, ny   int
	cells    [][]int32
}

// dagiIndex is all four layers, loaded once and never changed.
type dagiIndex struct {
	layers  [dagiLayerCount]dagiLayerIndex
	quantum float64
	credit  string
}

// dagiDefault is the embedded asset, parsed on first use.
var dagiDefault = sync.OnceValues(func() (*dagiIndex, error) {
	return loadDagiIndex(dagiAssets, "dagi")
})

// loadDagiIndex reads dagi-<layer>.json for every layer from dir in fsys.
// The four files must come from one build of one download: the same credit
// (it names the fetch month), DAGI generation and download date.
func loadDagiIndex(fsys fs.FS, dir string) (*dagiIndex, error) {
	idx := &dagiIndex{}
	var first dagiAssetSource
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		name := dir + "/dagi-" + l.String() + ".json"
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		var file dagiAssetFile
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if file.Format != dagiFormat || file.Layer != l.String() {
			return nil, fmt.Errorf("%s: format %q layer %q, want %q %q", name, file.Format, file.Layer, dagiFormat, l)
		}
		if !(file.Quantum > 0) || (idx.quantum != 0 && file.Quantum != idx.quantum) {
			return nil, fmt.Errorf("%s: quantum %v", name, file.Quantum)
		}
		idx.quantum = file.Quantum
		if file.Credit == "" {
			return nil, fmt.Errorf("%s: no credit", name)
		}
		if l == 0 {
			idx.credit, first = file.Credit, file.Source
		} else if file.Credit != idx.credit || file.Source.Generation != first.Generation ||
			file.Source.Downloaded != first.Downloaded {
			return nil, fmt.Errorf("%s: credit %q, generation %d, downloaded %q; %s has %q, %d, %q", name,
				file.Credit, file.Source.Generation, file.Source.Downloaded,
				DagiLayer(0), idx.credit, first.Generation, first.Downloaded)
		}
		li, err := buildDagiLayer(file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		idx.layers[l] = li
	}
	for _, f := range idx.layers[DagiKommune].features {
		if _, ok := idx.layers[DagiRegion].byCode[f.region]; !ok {
			return nil, fmt.Errorf("kommune %s: unknown region %q", f.area.Code, f.region)
		}
	}
	return idx, nil
}

func buildDagiLayer(file dagiAssetFile) (dagiLayerIndex, error) {
	li := dagiLayerIndex{byCode: make(map[string]int32, len(file.Features))}
	if len(file.Features) == 0 {
		return li, fmt.Errorf("no features")
	}
	li.features = make([]dagiFeature, 0, len(file.Features))
	for _, af := range file.Features {
		f, err := buildDagiFeature(af)
		if err != nil {
			return li, fmt.Errorf("feature %s: %w", af.Code, err)
		}
		li.features = append(li.features, f)
	}
	sort.Slice(li.features, func(i, j int) bool {
		return dagiPrecedes(&li.features[i], &li.features[j])
	})
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for i := range li.features {
		f := &li.features[i]
		if _, dup := li.byCode[f.area.Code]; dup {
			return li, fmt.Errorf("duplicate code %s", f.area.Code)
		}
		li.byCode[f.area.Code] = int32(i)
		minX, minY = math.Min(minX, f.minX), math.Min(minY, f.minY)
		maxX, maxY = math.Max(maxX, f.maxX), math.Max(maxY, f.maxY)
	}
	li.minCX, li.minCY = dagiCell(minX), dagiCell(minY)
	li.nx, li.ny = dagiCell(maxX)-li.minCX+1, dagiCell(maxY)-li.minCY+1
	li.cells = make([][]int32, li.nx*li.ny)
	for i := range li.features {
		f := &li.features[i]
		for cy := dagiCell(f.minY); cy <= dagiCell(f.maxY); cy++ {
			for cx := dagiCell(f.minX); cx <= dagiCell(f.maxX); cx++ {
				c := (cy-li.minCY)*li.nx + (cx - li.minCX)
				li.cells[c] = append(li.cells[c], int32(i)) // in precedence order
			}
		}
	}
	return li, nil
}

// dagiPrecedes is the layer's precedence for a point in more than one
// feature (on a shared border): the smaller source area, then the lower
// code. It is a total order, so the answer never depends on file order.
func dagiPrecedes(a, b *dagiFeature) bool {
	if a.areaM2 != b.areaM2 {
		return a.areaM2 < b.areaM2
	}
	return a.area.Code < b.area.Code
}

func dagiCell(v float64) int { return int(math.Floor(v / dagiCellUnits)) }

func buildDagiFeature(af dagiAssetFeature) (dagiFeature, error) {
	f := dagiFeature{
		area:   DagiArea{Code: af.Code, Name: af.Name},
		region: af.Region,
		areaM2: af.AreaM2,
	}
	if af.Code == "" || len(af.Rings) == 0 {
		return f, fmt.Errorf("no code or no rings")
	}
	f.minX, f.minY, f.maxX, f.maxY = math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for ri, ring := range af.Rings {
		if len(ring)%2 != 0 || len(ring) < 6 {
			return f, fmt.Errorf("ring %d: %d values, want an even count of at least 6", ri, len(ring))
		}
		var x, y int64
		var pts [][2]float64
		for i := 0; i < len(ring); i += 2 {
			x, y = x+ring[i], y+ring[i+1]
			pts = append(pts, [2]float64{float64(x), float64(y)})
		}
		for i, p := range pts {
			q := pts[(i+1)%len(pts)]
			f.segs = append(f.segs, dagiSeg{p[0], p[1], q[0], q[1]})
			f.minX, f.minY = math.Min(f.minX, p[0]), math.Min(f.minY, p[1])
			f.maxX, f.maxY = math.Max(f.maxX, p[0]), math.Max(f.maxY, p[1])
		}
	}
	nb := len(f.segs) / dagiSegsPerBand
	nb = max(1, min(nb, 4096))
	f.bandH = math.Max((f.maxY-f.minY)/float64(nb), 1)
	f.bands = make([][]int32, nb)
	for i, s := range f.segs {
		lo, hi := f.band(math.Min(s.y0, s.y1)), f.band(math.Max(s.y0, s.y1))
		for b := lo; b <= hi; b++ {
			f.bands[b] = append(f.bands[b], int32(i))
		}
	}
	return f, nil
}

// band is the band of latitude y (asset units), clamped to the feature.
func (f *dagiFeature) band(y float64) int {
	return max(0, min(int((y-f.minY)/f.bandH), len(f.bands)-1))
}

// contains reports whether (x, y) in asset units is in the feature, its
// boundary included: even-odd over the edges of the point's band.
func (f *dagiFeature) contains(x, y float64) bool {
	// Written so NaN fails: every comparison with NaN is false.
	if !(x >= f.minX && x <= f.maxX && y >= f.minY && y <= f.maxY) {
		return false
	}
	in := false
	for _, i := range f.bands[f.band(y)] {
		s := &f.segs[i]
		if dagiOnSegment(s, x, y) {
			return true
		}
		if (s.y0 > y) != (s.y1 > y) && x < s.x0+(y-s.y0)*(s.x1-s.x0)/(s.y1-s.y0) {
			in = !in
		}
	}
	return in
}

// dagiOnSegment reports whether (x, y) is within 1e-3 units (about a
// millimetre) of s.
func dagiOnSegment(s *dagiSeg, x, y float64) bool {
	const eps = 1e-3
	if x < math.Min(s.x0, s.x1)-eps || x > math.Max(s.x0, s.x1)+eps ||
		y < math.Min(s.y0, s.y1)-eps || y > math.Max(s.y0, s.y1)+eps {
		return false
	}
	dx, dy := s.x1-s.x0, s.y1-s.y0
	return math.Abs(dx*(y-s.y0)-dy*(x-s.x0)) <= eps*math.Hypot(dx, dy)
}

// find is the index of the feature (x, y) is in, by precedence, or -1.
func (li *dagiLayerIndex) find(x, y float64) int32 {
	cx, cy := dagiCell(x)-li.minCX, dagiCell(y)-li.minCY
	if cx < 0 || cy < 0 || cx >= li.nx || cy >= li.ny {
		return -1
	}
	for _, i := range li.cells[cy*li.nx+cx] {
		if li.features[i].contains(x, y) {
			return i
		}
	}
	return -1
}

// units converts a WGS84 position to asset units, and reports whether it
// can be in any feature at all: a real position (isValidNodePosition: not
// NaN, not ±Inf, not the 0/0 "no position" sentinel, in range).
func (idx *dagiIndex) units(lat, lon float64) (x, y float64, ok bool) {
	if !isValidNodePosition(lat, lon) {
		return 0, 0, false
	}
	return lon / idx.quantum, lat / idx.quantum, true
}

// locate is the feature index per layer of a WGS84 position, -1 for none.
func (idx *dagiIndex) locate(lat, lon float64) [dagiLayerCount]int32 {
	out := [dagiLayerCount]int32{-1, -1, -1, -1}
	x, y, ok := idx.units(lat, lon)
	if !ok {
		return out
	}
	for l := range idx.layers {
		out[l] = idx.layers[l].find(x, y)
	}
	return out
}

// place turns feature indices into the reported membership.
func (idx *dagiIndex) place(at [dagiLayerCount]int32) DagiPlace {
	ref := func(l DagiLayer) *DagiArea {
		if at[l] < 0 {
			return nil
		}
		return &idx.layers[l].features[at[l]].area
	}
	return DagiPlace{
		Kommune:    ref(DagiKommune),
		Landsdel:   ref(DagiLandsdel),
		Region:     ref(DagiRegion),
		Postnummer: ref(DagiPostnummer),
	}
}

// Lookup is the DAGI membership of a WGS84 position.
func (idx *dagiIndex) Lookup(lat, lon float64) DagiPlace {
	return idx.place(idx.locate(lat, lon))
}
