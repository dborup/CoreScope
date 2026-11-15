package copdem

import (
	"context"
	"math"
	"testing"

	"github.com/meshcore-analyzer/copdem/copdemtest"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #738 perf proof (AGENTS.md rule 0): the fill of a typical 80 km
// cross-border path at the real GLO-30 layout (2400 × 3600 pixels per
// tile, 1024² blocks, DEFLATE + predictor 3), served by the fake bucket on
// loopback. It reports the blocks, requests and bytes one profile costs,
// and the time cold (empty caches: headers, blocks, decode) and with the
// blocks cached (decode and sampling only). The network is loopback, so
// the cold time is the CPU and the in-process HTTP, not S3's latency.

// benchTerrain compresses about as badly as real GLO-30: smooth hills
// plus centimetre noise, ≈ 2.8 MB per 4 MiB block, the largest real block
// measured in N55E012 (#738 research §3.2: 38 KB to 2.78 MB, ≈ 0.97 MB
// on average). So the bytes below are an upper bound.
func benchTerrain(lon, lat float64) float32 {
	h := 40 + 30*math.Sin(lon*40)*math.Cos(lat*55) + 8*math.Sin(lon*310+lat*170)
	noise := math.Mod(math.Abs(math.Sin(lon*91733+lat*48611))*1e4, 1)
	return float32(h + 0.37*noise)
}

// benchPath is Copenhagen City Hall to 80 km east-north-east, across
// Øresund into Skåne: the DHM part is Copenhagen and the sea, the fill
// part about 60 km of Swedish land.
func benchPath() dhm.Plan { return plan(copenhagen, toward(copenhagen, 80000, 70)) }

func glo30Bucket(b *testing.B) *copdemtest.Bucket {
	bk := copdemtest.New()
	b.Cleanup(bk.Close)
	bk.Generate = func(path string) []byte {
		for _, tl := range []Tile{{55, 12}, {55, 13}, {56, 12}, {56, 13}} {
			if tl.Path() == path {
				return dhmtest.COG(dhmtest.GLO30Tile(tl.Lat, tl.Lon, benchTerrain))
			}
		}
		return nil
	}
	return bk
}

func benchClient(bk *copdemtest.Bucket) *Client {
	o := DefaultOptions()
	o.Transport = bk.Transport()
	return NewClient(o)
}

func reportFill(b *testing.B, r Result, requests, bytes int64, n int) {
	b.ReportMetric(float64(r.Calls), "calls")
	b.ReportMetric(float64(r.Level), "level")
	b.ReportMetric(float64(r.Filled), "filled-samples")
	b.ReportMetric(float64(requests)/float64(n), "requests/op")
	b.ReportMetric(float64(bytes)/float64(n), "upstream-bytes/op")
}

func BenchmarkFill80kmCrossBorderCold(b *testing.B) {
	bk := glo30Bucket(b)
	p := benchPath()
	// Build the tiles outside the timing.
	if _, err := benchClient(bk).Fill(context.Background(), &p); err != nil {
		b.Fatal(err)
	}
	reqs0, bytes0 := bk.Requests.Load(), bk.Bytes.Load()
	var r Result
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := benchClient(bk) // empty caches
		var err error
		if r, err = c.Fill(context.Background(), &p); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	reportFill(b, r, bk.Requests.Load()-reqs0, bk.Bytes.Load()-bytes0, b.N)
}

func BenchmarkFill80kmCrossBorderWarmBlocks(b *testing.B) {
	bk := glo30Bucket(b)
	p := benchPath()
	c := benchClient(bk)
	if _, err := c.Fill(context.Background(), &p); err != nil {
		b.Fatal(err)
	}
	reqs0, bytes0 := bk.Requests.Load(), bk.Bytes.Load()
	var r Result
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		if r, err = c.Fill(context.Background(), &p); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	reportFill(b, r, bk.Requests.Load()-reqs0, bk.Bytes.Load()-bytes0, b.N)
}
