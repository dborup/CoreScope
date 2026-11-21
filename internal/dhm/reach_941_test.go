package dhm

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// #941 M2 — nodes in reach (reach.go): point-to-point losses from the
// reach's centre over the raster's own elevation grid.

// reachAt is a target d metres from the centre on a bearing (degrees from
// north), in EPSG:25832 like the radials.
func reachAt(e0, n0, d, bearing, h float64) ReachTarget {
	s, c := math.Sincos(bearing * math.Pi / 180)
	lat, lon := FromUTM32(e0+d*s, n0+d*c)
	return ReachTarget{Lat: lat, Lon: lon, HeightM: h}
}

func reachSpec(r float64, m PathModel) CoverageSpec {
	return CoverageSpec{Center: LatLon{Lat: covAarhusLat, Lon: covAarhusLon}, RadiusM: r, ResolutionM: 30,
		TxHeightM: 10, RxHeightM: 2, FreqMHz: FirmwareFreqMHz, K: DefaultKFactor, Model: m}
}

// TestReachLosses941_OrderAndShadow on synthetic DEMs: over flat ground the
// loss grows with distance whatever the bearing, and a higher antenna at
// the same distance loses less; a ridge shadows the node behind it, which
// then loses more than a farther node in the open.
func TestReachLosses941_OrderAndShadow(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	for _, m := range []PathModel{PathBullington, PathDelta} {
		g := covGridFrom(e0, n0, 30000, 30, func(float64, float64) float32 { return 3 })
		var targets []ReachTarget
		for i, km := range []float64{29, 2, 15, 7, 22, 0.5} {
			targets = append(targets, reachAt(e0, n0, km*1000, float64(i)*61, 10))
		}
		targets = append(targets, reachAt(e0, n0, 15000, 200, 30)) // 15 km, higher mast
		got, err := ReachLosses(context.Background(), g, reachSpec(30000, m), targets)
		if err != nil {
			t.Fatal(err)
		}
		idx := []int{0, 1, 2, 3, 4, 5}
		sort.Slice(idx, func(a, b int) bool { return got[idx[a]].LossDb < got[idx[b]].LossDb })
		for k := 1; k < len(idx); k++ {
			if got[idx[k]].DistanceM <= got[idx[k-1]].DistanceM {
				t.Fatalf("%s: loss order is not distance order: %+v", m, got)
			}
		}
		if !(got[6].LossDb < got[2].LossDb) {
			t.Fatalf("%s: a 30 m mast at 15 km (%.1f dB) does not beat 10 m (%.1f dB)", m, got[6].LossDb, got[2].LossDb)
		}
		for i, r := range got {
			if !r.Inside || math.IsNaN(r.LossDb) || math.Abs(r.DistanceM-[]float64{29, 2, 15, 7, 22, 0.5, 15}[i]*1000) > 1 {
				t.Fatalf("%s: target %d: %+v", m, i, r)
			}
		}

		// A 120 m ridge 4 km east of the centre.
		ridge := func(e, n float64) float32 {
			if x := e - e0; x > 3950 && x < 4050 {
				return 120
			}
			return 3
		}
		g = covGridFrom(e0, n0, 30000, 30, ridge)
		got, err = ReachLosses(context.Background(), g, reachSpec(30000, m), []ReachTarget{
			reachAt(e0, n0, 8000, 90, 10),   // behind the ridge
			reachAt(e0, n0, 12000, 270, 10), // farther, open ground
		})
		if err != nil {
			t.Fatal(err)
		}
		if !(got[0].LossDb > got[1].LossDb+10) {
			t.Fatalf("%s: shadowed node %.1f dB, open farther node %.1f dB", m, got[0].LossDb, got[1].LossDb)
		}
	}
}

// TestReachLosses941_MatchesTheRadial: a node on a radial's receiver gets
// the raster's own number: the sweep (radialSweep.run) and the path
// (Link.PathLoss) are the same model over the same samples.
func TestReachLosses941_MatchesTheRadial(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	elev := covSyntheticDEM(e0, n0)
	g := covGridFrom(e0, n0, 20000, 30, elev)
	for _, m := range []PathModel{PathBullington, PathDelta} {
		const nS = 600
		h := make([]float32, nS+1)
		for i := range h {
			h[i] = g.Sample(e0, n0+float64(i)*30) // bearing 0: north
		}
		radial := sweepForModel(h, 30, 10, 4, FirmwareFreqMHz, DefaultKFactor, 1, m)
		for _, j := range []int{3, 50, 333, 600} {
			lat, lon := FromUTM32(e0, n0+float64(j)*30)
			got, err := ReachLosses(context.Background(), g, reachSpec(20000, m), []ReachTarget{{Lat: lat, Lon: lon, HeightM: 4}})
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(got[0].LossDb-float64(radial[j-1])) > 0.01 {
				t.Fatalf("%s at %d m: node %.4f dB, radial %.4f dB", m, j*30, got[0].LossDb, radial[j-1])
			}
		}
	}
}

// TestReachLosses941_OutsideAndGaps: beyond the radius and at the centre
// a target is outside, with no loss; a gap on the path gives no loss but
// is inside.
func TestReachLosses941_OutsideAndGaps(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	hole := func(e, n float64) float32 {
		if n-n0 > 5000 && n-n0 < 5100 {
			return float32(math.NaN())
		}
		return 2
	}
	g := covGridFrom(e0, n0, 10000, 30, hole)
	got, err := ReachLosses(context.Background(), g, reachSpec(10000, PathDelta), []ReachTarget{
		reachAt(e0, n0, 10500, 90, 10),
		{Lat: covAarhusLat, Lon: covAarhusLon, HeightM: 10},
		reachAt(e0, n0, 8000, 0, 10),
		reachAt(e0, n0, 8000, 180, 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Inside || got[1].Inside || !math.IsNaN(got[0].LossDb) || !math.IsNaN(got[1].LossDb) {
		t.Fatalf("outside targets: %+v %+v", got[0], got[1])
	}
	if !got[2].Inside || !math.IsNaN(got[2].LossDb) {
		t.Fatalf("across the gap: %+v", got[2])
	}
	if !got[3].Inside || math.IsNaN(got[3].LossDb) {
		t.Fatalf("clear path: %+v", got[3])
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReachLosses(ctx, g, reachSpec(10000, PathDelta), []ReachTarget{reachAt(e0, n0, 100, 0, 1)}); err == nil {
		t.Fatal("a cancelled evaluation finished")
	}
}

// reachTargets2000 is 2000 nodes spread over a 40 km disk.
func reachTargets2000(e0, n0 float64) []ReachTarget {
	rng := rand.New(rand.NewSource(2000))
	out := make([]ReachTarget, 2000)
	for i := range out {
		out[i] = reachAt(e0, n0, 40000*math.Sqrt(rng.Float64()), rng.Float64()*360, 2+rng.Float64()*20)
	}
	return out
}

// BenchmarkReachLosses2000 is nodes in reach at the M2 scale: 2000 nodes
// in a 40 km / 30 m disk, Bullington and the complete method.
func BenchmarkReachLosses2000(b *testing.B) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	g := covGridFrom(e0, n0, 40000, 30, covSyntheticDEM(e0, n0))
	targets := reachTargets2000(e0, n0)
	for _, m := range []PathModel{PathBullington, PathDelta} {
		b.Run(string(m), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := ReachLosses(context.Background(), g, reachSpec(40000, m), targets); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
