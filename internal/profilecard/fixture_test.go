package profilecard

import (
	"math"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/dhm"
)

// Fixtures build TerrainProfileResponses the way cmd/server's
// buildResponse does (coverage, verdict through dhm.Link.Assess, decimation
// to 1024 samples), from piecewise-linear terrain given in whole metres.
//
// Every product that meets an addition is wrapped in float64(...): the Go
// spec lets arm64 fuse x*y+z into one FMA instruction, amd64 (GOAMD64=v1)
// does not, and a one-ulp difference in a fixture would move a golden
// pixel between a developer's Mac and CI. TestNoFusedMultiplyAdd pins it.

// spec describes one synthetic path.
type spec struct {
	distM            float64
	n                int       // computed samples (SampleCount)
	terrain          []float64 // control points, evenly spaced from A to B
	surface          []float64 // control points of the DSM; nil for model=dtm
	gapFrom, gapTo   float64   // fraction of the path without data; equal = none
	antA, antB       float64
	assumedA         bool
	assumedB         bool
	freqMHz, k       float64
	kindA, kindB     string
	maxReturned      int // 0 = 1024, the server's terrainMaxReturned
	allSamplesNoData bool
	// The building classification (#730 M2), as cmd/server's
	// classifyBuildings leaves it: classState "" is none, else the state
	// served. On a full or partial one every raised sample (DSM − DTM over
	// the classifier's threshold) is a building inside a footprint and
	// trees/other outside, except, on a partial one, those in
	// [unclassFrom, unclassTo], whose footprints did not come back.
	classState             string
	footprints             [][2]float64 // fractions of the path
	unclassFrom, unclassTo float64
}

// classes is s's class per computed sample, and whether any sample has
// one (cmd/server's terrainClasses.used: the footprints are credited).
func (s spec) classes(dtm, dsm []float32) ([]string, bool) {
	out := make([]string, len(dtm))
	if s.classState != "full" && s.classState != "partial" {
		return out, false
	}
	smp := buildings.Samples{DTM: dtm, DSM: dsm}
	used := false
	for i := range out {
		t := float64(i) / float64(len(dtm)-1)
		if !smp.Elevated(i, buildings.DefaultOptions().ThresholdM) ||
			(s.classState == "partial" && t >= s.unclassFrom && t <= s.unclassTo) {
			continue
		}
		out[i] = string(rune(buildings.Vegetation))
		for _, f := range s.footprints {
			if t >= f[0] && t <= f[1] {
				out[i] = string(rune(buildings.Building))
			}
		}
		used = true
	}
	return out, used
}

// lerpPoints is the piecewise-linear value of pts at t in [0, 1].
func lerpPoints(pts []float64, t float64) float64 {
	if len(pts) == 1 {
		return pts[0]
	}
	pos := float64(t * float64(len(pts)-1))
	i := int(pos)
	if i >= len(pts)-1 {
		return pts[len(pts)-1]
	}
	f := pos - float64(i)
	return pts[i] + float64((pts[i+1]-pts[i])*f)
}

func round2(v float64) float64 { return math.Round(float64(v*100)) / 100 }

func ptr(v float64) *float64 { return &v }

func optional(v float32) *float64 {
	if math.IsNaN(float64(v)) {
		return nil
	}
	return ptr(round2(float64(v)))
}

func clearanceOf(a dhm.Assessment) TerrainClearance {
	if a.State == dhm.VerdictUnknown {
		return TerrainClearance{}
	}
	out := TerrainClearance{WorstClearanceM: ptr(round2(a.WorstClearanceM)), WorstAtM: ptr(round2(a.WorstAtM))}
	if !math.IsInf(a.WorstRatio, 0) {
		out.WorstF1Fraction = ptr(math.Round(float64(a.WorstRatio*1000)) / 1000)
	}
	return out
}

// surfaceInfo and gapKeep follow cmd/server's surfaceInfo and
// terrainGapKeep (#745), so a fixture is shaped like a real response.
func surfaceInfo(a dhm.Assessment) *TerrainSurfaceInfo {
	out := &TerrainSurfaceInfo{State: string(a.State), TerrainClearance: clearanceOf(a)}
	switch {
	case a.AntennaBelowA && a.AntennaBelowB:
		out.Endpoint = "both"
	case a.AntennaBelowA:
		out.Endpoint = "A"
	case a.AntennaBelowB:
		out.Endpoint = "B"
	}
	if out.Endpoint != "" {
		out.Reason = "antennaBelowSurface"
	}
	return out
}

func gapKeep(room int, series ...[]float32) []int {
	var starts, edges []int
	for _, s := range series {
		for i := 0; i < len(s); i++ {
			if !math.IsNaN(float64(s[i])) {
				continue
			}
			j := i
			for j+1 < len(s) && math.IsNaN(float64(s[j+1])) {
				j++
			}
			starts = append(starts, i)
			edges = append(edges, j, i-1, j+1)
			i = j
		}
	}
	if len(starts) <= room {
		return append(starts, edges...)
	}
	if room <= 0 {
		return nil
	}
	var missing []int
	for i := range series[0] {
		for _, s := range series {
			if s != nil && math.IsNaN(float64(s[i])) {
				missing = append(missing, i)
				break
			}
		}
	}
	out := make([]int, room)
	for j := range out {
		out[j] = missing[int(math.Round(float64(j)*float64(len(missing)-1)/float64(max(room-1, 1))))]
	}
	return out
}

func build(s spec) TerrainProfileResponse {
	n := s.n
	step := s.distM / float64(n-1)
	nan := float32(math.NaN())
	dtm := make([]float32, n)
	var dsm []float32
	if s.surface != nil {
		dsm = make([]float32, n)
	}
	for i := 0; i < n; i++ {
		t := float64(i) / float64(n-1)
		inGap := s.allSamplesNoData || (s.gapTo > s.gapFrom && t >= s.gapFrom && t <= s.gapTo)
		if inGap {
			dtm[i] = nan
			if dsm != nil {
				dsm[i] = nan
			}
			continue
		}
		g := lerpPoints(s.terrain, t)
		dtm[i] = float32(g)
		if dsm != nil {
			dsm[i] = float32(math.Max(g, lerpPoints(s.surface, t)))
		}
	}
	kindA, kindB := s.kindA, s.kindB
	if kindA == "" {
		kindA = "node"
	}
	if kindB == "" {
		kindB = "node"
	}
	resp := TerrainProfileResponse{
		DistanceM: round2(s.distM), StepM: round2(step), SampleCount: n,
		Model: "dtm", Attribution: dhm.AttributionDerived, Samples: []TerrainSample{},
	}
	if dsm != nil {
		resp.Model = "both"
	}
	valid := 0
	for i, v := range dtm {
		if !math.IsNaN(float64(v)) {
			valid++
		} else if resp.Coverage.FirstGapM == nil {
			resp.Coverage.FirstGapM = ptr(round2(float64(float64(i) * step)))
		}
	}
	resp.Coverage.Pct = math.Round(float64(1000*float64(valid))/float64(n)) / 10
	switch valid {
	case n:
		resp.Coverage.State = "full"
	case 0:
		resp.Coverage.State = "none"
	default:
		resp.Coverage.State = "partial"
	}
	heights := [2]float64{math.NaN(), math.NaN()}
	for i, kind := range []string{kindA, kindB} {
		idx := []int{0, n - 1}[i]
		ant := []float64{s.antA, s.antB}[i]
		ep := TerrainEndpoint{Lat: 55.0 + float64(i)/10, Lon: 10.0 + float64(i)/10, Kind: kind,
			GroundM: optional(dtm[idx]), AntennaM: ant, AntennaSource: "request",
			Assumed: []bool{s.assumedA, s.assumedB}[i]}
		if kind == "node" {
			ep.Role = "repeater"
		}
		if ep.Assumed {
			ep.AntennaSource = "role-default"
		}
		if ep.GroundM != nil {
			heights[i] = float64(dtm[idx]) + ant
		}
		if i == 0 {
			resp.A = ep
		} else {
			resp.B = ep
		}
	}
	freq := s.freqMHz
	if freq == 0 {
		freq = dhm.FirmwareFreqMHz
	}
	k := s.k
	if k == 0 {
		k = dhm.DefaultKFactor
	}
	lambda := dhm.Wavelength(freq)
	resp.Physics = TerrainPhysics{FreqMHz: freq, FreqSource: "request", LambdaM: math.Round(float64(lambda*1e6)) / 1e6,
		K: k, KSource: "request", F1MaxM: round2(dhm.FresnelRadius(lambda, float64(s.distM/2), float64(s.distM/2)))}
	link := dhm.Link{DistanceM: s.distM, HeightA: heights[0], HeightB: heights[1], LambdaM: lambda, K: k}
	terrain := link.Assess(dtm)
	resp.Verdict = TerrainVerdict{State: string(terrain.State), Model: "dtm", Criterion: dhm.Criterion,
		TerrainClearance: clearanceOf(terrain)}
	keep := []int{terrain.WorstIndex}
	if dsm != nil {
		surface := link.Assess(dsm)
		if resp.Coverage.State != "full" {
			surface = dhm.Assessment{State: dhm.VerdictUnknown, WorstIndex: -1}
		}
		resp.Verdict.Surface = surfaceInfo(surface)
		keep = append(keep, surface.WorstIndex)
	}
	var cls []string
	if s.classState != "" {
		var used bool
		cls, used = s.classes(dtm, dsm)
		resp.Classification = &TerrainClassification{State: s.classState}
		if used {
			resp.Attribution = buildings.AttributionDerived
		}
	}
	if resp.Coverage.State != "none" {
		maxRet := s.maxReturned
		if maxRet == 0 {
			maxRet = 1024
		}
		room := maxRet - 2
		for _, k := range keep {
			if k > 0 && k < n-1 {
				room--
			}
		}
		keep = append(keep, gapKeep(room, dtm, dsm)...)
		idx := dhm.Decimate(n, maxRet, keep...)
		resp.Samples = make([]TerrainSample, len(idx))
		for j, i := range idx {
			smp := TerrainSample{D: round2(float64(float64(i) * step)), DTM: optional(dtm[i])}
			if dsm != nil {
				smp.DSM = optional(dsm[i])
			}
			if cls != nil {
				smp.Cls = cls[i]
			}
			resp.Samples[j] = smp
		}
	}
	return resp
}

// Named fixtures, shared by the golden, property and benchmark tests.

func fxClear() TerrainProfileResponse {
	return build(spec{distM: 11100, n: 1024, terrain: []float64{38, 30, 22, 26, 19, 14, 20, 31, 45},
		surface: []float64{38, 41, 30, 27, 33, 26, 22, 34, 45}, antA: 28, antB: 22, freqMHz: 869.618})
}

func fxMarginal() TerrainProfileResponse {
	return build(spec{distM: 14600, n: 1024, terrain: []float64{12, 18, 26, 34, 41, 44, 37, 25, 16, 10},
		surface: []float64{12, 22, 33, 40, 47, 49, 39, 27, 19, 10}, antA: 46, antB: 34, assumedB: true, freqMHz: 869.618})
}

func fxBlocked() TerrainProfileResponse {
	return build(spec{distM: 9200, n: 921, terrain: []float64{8, 14, 22, 61, 74, 52, 20, 9},
		antA: 12, antB: 8, freqMHz: 869.525})
}

func fxPartial() TerrainProfileResponse {
	return build(spec{distM: 21800, n: 1024, terrain: []float64{6, 14, 9, 2, 0, 0, 0, 3, 11},
		surface: []float64{6, 19, 15, 2, 0, 0, 0, 3, 18}, gapFrom: 0.42, gapTo: 0.71, antA: 20, antB: 15})
}

func fxNone() TerrainProfileResponse {
	return build(spec{distM: 31400, n: 1256, terrain: []float64{0}, allSamplesNoData: true,
		antA: 10, antB: 10, kindB: "point"})
}

func fxDTM() TerrainProfileResponse {
	return build(spec{distM: 6400, n: 641, terrain: []float64{24, 31, 28, 17, 22, 35},
		antA: 25, antB: 40, assumedA: true, freqMHz: 869.618, k: 1})
}

func fxShort() TerrainProfileResponse {
	return build(spec{distM: 300, n: 31, terrain: []float64{52, 54, 57, 55},
		surface: []float64{52, 61, 63, 55}, antA: 8, antB: 8, kindA: "point", kindB: "point"})
}

func fxLong() TerrainProfileResponse {
	return build(spec{distM: 80000, n: 3200, terrain: []float64{40, 70, 95, 60, 30, 18, 45, 110, 84, 52, 23, 61},
		surface: []float64{40, 82, 104, 71, 33, 25, 58, 121, 90, 60, 27, 61}, antA: 40, antB: 35, freqMHz: 869.618})
}

// fxMixed is fxClear with its middle filled from Copernicus DEM (#738):
// the server sets Coverage.Mixed, the fill stretches and AttributionFill.
func fxMixed() TerrainProfileResponse {
	r := fxClear()
	r.Coverage.Mixed = true
	r.Coverage.Fill = []TerrainFillRange{{FromM: 4000, ToM: 7000, Source: "copdem-glo30", ResolutionM: 30}}
	r.AttributionFill = "produced using Copernicus WorldDEM-30 (long Art. 6(b) notice)"
	return r
}

// fxTown is a 2.4 km path through a town (#846): trees, a low building,
// a tall pair that rises through the line of sight, more trees and one
// more building, over gentle terrain that stays clear. state is the
// building classification: "full", "partial" (the last 40 % of the path
// not told apart) or "unavailable" (the lookup failed: no classes).
func fxTown(state string) TerrainProfileResponse {
	// 0 is bare ground: the DSM is never below the terrain (build).
	surface := []float64{
		0, 0, 0, 0, 32, 35, 34, 0, // trees at 200–350 m
		0, 44, 44, 0, 0, 0, 0, 0, // a building at 400–550 m
		36, 38, 37, 39, 36, 0, // trees at 800–1050 m
		0, 58, 58, 0, 52, 0, // two buildings at 1100–1350 m
		0, 0, 0, 0, 40, 41, 0, 42, 41, 0, // trees at 1600–1850 m
		0, 50, 50, 0, // a building at 1900–2050 m
		0, 0, 0, 0, 0, 0, 0,
	}
	return build(spec{distM: 2400, n: 961, terrain: []float64{18, 19, 21, 22, 22, 23, 25, 26, 26, 27, 28, 28, 27},
		surface: surface, antA: 26, antB: 20, freqMHz: 869.618, classState: state,
		footprints:  [][2]float64{{400.0 / 2400, 550.0 / 2400}, {1100.0 / 2400, 1350.0 / 2400}, {1900.0 / 2400, 2050.0 / 2400}},
		unclassFrom: 0.6, unclassTo: 1})
}
