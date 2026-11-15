package copdem

import (
	"context"
	"testing"

	"github.com/meshcore-analyzer/copdem/copdemtest"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #738 perf proof (AGENTS.md rule 0): the fill of a typical 80 km
// cross-border path at the real GLO-30 layout (2400 × 3600 pixels per
// tile, 1024² blocks, DEFLATE + predictor 3), on dhmtest.Terrain, whose
// blocks are about as large as the largest real ones, served by the fake
// bucket on loopback. It reports the blocks, requests and bytes one
// profile costs, and the time cold (empty caches: headers, blocks,
// decode) and with the blocks cached (decode and sampling only). The
// network is loopback, so the cold time is the CPU and the in-process
// HTTP, not S3's latency.

// benchPath is Copenhagen City Hall to 80 km east-north-east, across
// Øresund into Skåne: the DHM part is Copenhagen and the sea, the fill
// part about 60 km of Swedish land.
func benchPath() dhm.Plan { return plan(copenhagen, toward(copenhagen, 80000, 70)) }

func glo30Bucket(b *testing.B) *copdemtest.Bucket {
	bk := copdemtest.New()
	b.Cleanup(bk.Close)
	bk.Generate = copdemtest.Synthetic(copdemtest.GLO30, dhmtest.Terrain)
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
