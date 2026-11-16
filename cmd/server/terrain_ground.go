package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/meshcore-analyzer/dhm"
)

// groundAt is the terrain (DTM) height at pt, NaN where DHM has none, for
// the #737 M3a projection of a point into an oblique photo. It is the
// profile's own sampling, a dhm.Plan of the one point read by Plan.Fetch
// (one raster of 2 × 2 px), coalesced in the profile's Flight and
// admitted by its maxInFlight, within its upstream timeout and deadline.
// A point DHM does not cover costs no call. The caller snaps pt, caches
// the height and charges the per-IP budget; a refusal is
// dhm.ErrAdmissionFull (isTerrainBusy).
func (p *terrainProfiler) groundAt(ctx context.Context, pt dhm.LatLon) (float64, error) {
	plan := dhm.PlanProfile(pt, pt, p.set.plan)
	if plan.Calls() == 0 {
		return math.NaN(), nil
	}
	key := fmt.Sprintf("ground|%d,%d", terrainSnapCoord(pt.Lat), terrainSnapCoord(pt.Lon))
	b, _, err := p.flight.DoAdmitted(ctx, key, p.deadline, p.admit, func(ctx context.Context) ([]byte, error) {
		dtm, err := plan.Fetch(ctx, p.src, dhm.ModelTerrain, p.set.upstreamTimeout)
		if err != nil {
			return nil, err
		}
		return encodeGround(float64(dtm[0])), nil
	})
	if err != nil {
		if !isTerrainBusy(err) && ctx.Err() == nil && p.logFailure != nil {
			p.logFailure("skraafoto ground height", err)
		}
		return 0, err
	}
	return decodeGround(b), nil
}

// encodeGround and decodeGround are a height as a cache entry: float32,
// NaN for none.
func encodeGround(h float64) []byte {
	return binary.LittleEndian.AppendUint32(nil, math.Float32bits(float32(h)))
}

func decodeGround(b []byte) float64 {
	if len(b) != 4 {
		return math.NaN()
	}
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
}
