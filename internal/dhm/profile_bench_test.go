package dhm

import (
	"context"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #681 M3 perf proof (AGENTS.md rule 0): decoding the DEFLATE GeoTIFF
// rasters of one profile and sampling them, at the 1024-sample (20 km) and
// 3200-sample (80 km) sizes, at the most expensive bearing (45°). The
// network is not part of this; cmd/server measures the latency against a
// fake upstream.

// bytesSource serves pre-encoded GeoTIFFs through the WCS decode path.
type bytesSource map[[4]float64][]byte

func (s bytesSource) FetchRaster(_ context.Context, r RasterRequest) (Grid, error) {
	return rasterGrid(s[r.BBox], r)
}

func benchProfile(b *testing.B, km float64, wantSamples int) {
	p := PlanProfile(aarhus, toward(aarhus, km*1000, 45), defaultPlan())
	if n := len(p.E); n < wantSamples-1 || n > wantSamples+1 {
		b.Fatalf("%v km: %d samples, want %d", km, n, wantSamples)
	}
	src := bytesSource{}
	rasterBytes := 0
	for _, c := range p.chunks {
		r := c.raster
		vals := make([]float32, r.Width*r.Height)
		for i := range vals {
			vals[i] = float32(i%97) * 0.37
		}
		src[r.BBox] = dhmtest.GeoTIFF(r.Width, r.Height, vals, nil, true)
		rasterBytes += len(src[r.BBox])
	}
	b.ReportMetric(float64(p.Calls()), "rasters")
	b.ReportMetric(float64(rasterBytes), "raster-bytes")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Fetch(context.Background(), src, ModelTerrain, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProfileDecodeAndSample1024(b *testing.B) { benchProfile(b, 20, 1025) }
func BenchmarkProfileDecodeAndSample3200(b *testing.B) { benchProfile(b, 80, 3200) }
