package dhm

import (
	"context"
	"math"
)

// #941 M2 — nodes in reach: the path loss from a reach's centre to given
// points, each with its own antenna height, over the elevation grid the
// raster was computed on. The same model as the sweep (Link.PathLoss, the
// point-to-point twin of radialSweep.run); no I/O, so no upstream call.

// ReachTarget is one point evaluated from the centre.
type ReachTarget struct {
	Lat, Lon float64
	HeightM  float64 // its antenna above the ground
}

// ReachLoss is one target's result.
type ReachLoss struct {
	// Inside: within the spec's radius and not at the centre itself. An
	// outside target has no loss.
	Inside    bool
	DistanceM float64 // in EPSG:25832, as the raster's radials
	// LossDb is the basic transmission loss, NaN without complete terrain
	// along the path.
	LossDb float64
}

// ReachLosses evaluates every target from spec's centre over g with
// spec's model: a straight path sampled about every ResolutionM, the
// transmitter TxHeightM and the target HeightM above the ground. It checks
// ctx every few targets. O(targets × samples per path).
func ReachLosses(ctx context.Context, g *CoverageGrid, spec CoverageSpec, targets []ReachTarget) ([]ReachLoss, error) {
	e0, n0 := ToUTM32(spec.Center.Lat, spec.Center.Lon)
	lambda := Wavelength(spec.FreqMHz)
	out := make([]ReachLoss, len(targets))
	var surface []float32
	for k, t := range targets {
		if k%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		e1, n1 := ToUTM32(t.Lat, t.Lon)
		d := math.Hypot(e1-e0, n1-n0)
		r := ReachLoss{DistanceM: d, LossDb: math.NaN(), Inside: d >= 1 && d <= spec.RadiusM}
		if r.Inside {
			n := max(1, int(math.Round(d/spec.ResolutionM)))
			surface = surface[:0]
			for i := 0; i <= n; i++ {
				f := float64(i) / float64(n)
				surface = append(surface, g.Sample(e0+f*(e1-e0), n0+f*(n1-n0)))
			}
			link := Link{DistanceM: d, HeightA: float64(surface[0]) + spec.TxHeightM,
				HeightB: float64(surface[n]) + t.HeightM, LambdaM: lambda, K: spec.K}
			if loss, ok := link.PathLoss(surface, spec.Model); ok {
				r.LossDb = loss
			}
		}
		out[k] = r
	}
	return out, nil
}
