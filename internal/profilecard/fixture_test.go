package profilecard

import (
	"math"

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
	return TerrainClearance{
		WorstClearanceM: ptr(round2(a.WorstClearanceM)),
		WorstAtM:        ptr(round2(a.WorstAtM)),
		WorstF1Fraction: ptr(math.Round(float64(a.WorstRatio*1000)) / 1000),
	}
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
		Model: "dtm", Attribution: dhm.Attribution, Samples: []TerrainSample{},
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
		resp.Verdict.Surface = &TerrainSurfaceInfo{State: string(surface.State), TerrainClearance: clearanceOf(surface)}
		keep = append(keep, surface.WorstIndex)
	}
	if resp.Coverage.State != "none" {
		maxRet := s.maxReturned
		if maxRet == 0 {
			maxRet = 1024
		}
		idx := dhm.Decimate(n, maxRet, keep...)
		resp.Samples = make([]TerrainSample, len(idx))
		for j, i := range idx {
			smp := TerrainSample{D: round2(float64(float64(i) * step)), DTM: optional(dtm[i])}
			if dsm != nil {
				smp.DSM = optional(dsm[i])
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
