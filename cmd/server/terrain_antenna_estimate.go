package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #789 M1 — an antenna height estimated from the surface model.
//
// A repeater or room on a roof or a tower stands on the surface, not on
// the ground: DSM − DTM in a small window around the node (dhm.SurfaceAt)
// is the evidence. The estimate is surface + terrainEstimateRoofM, and it
// only ever raises a height: a lattice mast is invisible in the DSM
// (research §3), so an estimate of about 0 m must never pull a node on a
// 30 m mast below its role height. It is marked assumed, like a default.
//
// It is computed lazily, for a node endpoint of a profile that has no
// request or operator height for it, and never in a batch. The result is
// kept per node in a bounded LRU keyed by the public key and the position
// rounded to 5 decimals, so a node that moves misses the cache and is
// measured again. A cache hit is O(1); a miss costs one DSM and one DTM
// raster of about 52 × 52 px, coalesced (Flight), admitted like a
// profile fetch, and charged to the client's per-IP limit like an
// elevation miss (one charge per request). Only the profile route
// measures; the share card and /p read the cache alone (cachedEstimates),
// so all three draw the same height once it is known. Hidden nodes never
// get here: resolveTerrainEnd refuses them before any coordinate is read
// (#743).

const (
	// terrainEstimateEntries bounds the estimate cache: about the node
	// count of a large instance. An entry is 8 bytes of value plus the
	// key, well under 1 MB in all.
	terrainEstimateEntries = 4096
	// terrainEstimateTTL is how long an estimate is kept. DHM changes
	// rarely, and a move changes the key.
	terrainEstimateTTL = 30 * 24 * time.Hour
	// terrainEstimateFailTTL is how long a failed measurement is not
	// retried, so a failing upstream costs at most 2 calls per node per
	// hour.
	terrainEstimateFailTTL = time.Hour
	// terrainEstimateRadiusM is the window radius: advertised positions
	// are often a few metres off (research §3).
	terrainEstimateRadiusM = 10
	// terrainEstimateRoofM is the antenna above the surface: a short
	// mast on the roof.
	terrainEstimateRoofM = 2
	// terrainEstimateMinCoverage is the share of the window that must
	// have data in both models for an estimate.
	terrainEstimateMinCoverage = 0.5
)

// terrainEstimateRoles are the roles that get an estimate. Companions and
// sensors keep their role default (#789, coordinator decision on open
// question 4): a handheld under a tree is not on the canopy.
var terrainEstimateRoles = map[string]bool{"repeater": true, "room": true}

// terrainEstimateKey is a node at a position snapped like the profile
// cache key (terrainSnapCoord).
type terrainEstimateKey struct {
	pubkey   string
	lat, lon int64
}

func (k terrainEstimateKey) String() string {
	return fmt.Sprintf("est|%s|%d,%d", k.pubkey, k.lat, k.lon)
}

// terrainEstimator is the estimate's state in a terrainProfiler. A nil
// src switches the estimate off.
type terrainEstimator struct {
	src    dhm.ElevationSource
	cache  *dhm.Cache[terrainEstimateKey] // the surface in metres; NaN: none
	failed *dhm.Cache[terrainEstimateKey] // measurements that failed recently
	flight dhm.Flight[[]byte]
}

func newTerrainEstimator(src dhm.ElevationSource) terrainEstimator {
	if src == nil {
		return terrainEstimator{}
	}
	return terrainEstimator{
		src:    src,
		cache:  dhm.NewCache[terrainEstimateKey](terrainEstimateEntries, terrainEstimateEntries*8, terrainEstimateTTL),
		failed: dhm.NewCache[terrainEstimateKey](terrainEstimateEntries, terrainEstimateEntries, terrainEstimateFailTTL),
	}
}

// estimatedAntennaM is the antenna height for a surface of s metres above
// the ground, bounded by terrainMaxAntennaM.
func estimatedAntennaM(s float64) float64 {
	return math.Min(terrainMaxAntennaM, math.Round((s+terrainEstimateRoofM)*10)/10)
}

// wantsEstimate reports whether endpoint e, with the request's override,
// would fall through to a role height that an estimate may raise: a node
// of an estimated role with neither a request nor an operator height.
func (p *terrainProfiler) wantsEstimate(e terrainEnd, override *float64) bool {
	if p.estimate.src == nil || override != nil || e.kind != "node" || e.pubkey == "" || !terrainEstimateRoles[e.role] {
		return false
	}
	if _, ok := p.radio.heights[e.pubkey]; ok {
		return false
	}
	// #789 M2: a proven owner height ranks above the estimate; skip the
	// measurement for a node that has one.
	return true
}

// estimateKey is e's estimate cache key: its public key and its position
// snapped like the profile cache key.
func estimateKey(e terrainEnd) terrainEstimateKey {
	return terrainEstimateKey{pubkey: e.pubkey, lat: terrainSnapCoord(e.pt.Lat), lon: terrainSnapCoord(e.pt.Lon)}
}

// cachedSurface is the surface under node e from the caches alone, never
// upstream. known is false when a profile would measure it: neither an
// estimate nor a recent failure is remembered.
func (p *terrainProfiler) cachedSurface(e terrainEnd) (s *float64, known bool) {
	key, now := estimateKey(e), p.now()
	if b, ok := p.estimate.cache.Get(key, now); ok {
		return decodeSurface(b), true
	}
	_, failed := p.estimate.failed.Get(key, now)
	return nil, failed
}

// cachedEstimates sets surfaceM on the endpoints that want an estimate
// and have one cached, and reports whether any of them still has to be
// measured. It never calls upstream, so the share card and /p, which
// must not, see the same estimate as the panel once it has been measured
// (#789 review); without one their role height stands.
func (p *terrainProfiler) cachedEstimates(ends *[2]terrainEnd, overrides [2]*float64) (pending bool) {
	for i := range ends {
		if !p.wantsEstimate(ends[i], overrides[i]) {
			continue
		}
		s, known := p.cachedSurface(ends[i])
		ends[i].surfaceM = s
		pending = pending || !known
	}
	return pending
}

// estimateEnds sets surfaceM on the endpoints that want an estimate,
// measuring the ones not cached, concurrently. A failure leaves the
// endpoint without one: its role height stands.
func (p *terrainProfiler) estimateEnds(ctx context.Context, ends *[2]terrainEnd, overrides [2]*float64) {
	var wg sync.WaitGroup
	for i := range ends {
		if !p.wantsEstimate(ends[i], overrides[i]) {
			continue
		}
		wg.Add(1)
		go func(e *terrainEnd) {
			defer wg.Done()
			e.surfaceM = p.surfaceFor(ctx, *e)
		}(&ends[i])
	}
	wg.Wait()
}

// surfaceFor is the cached surface under node e, measuring it on a miss;
// nil when there is none (not covered, too little data, or the
// measurement failed or was refused).
func (p *terrainProfiler) surfaceFor(ctx context.Context, e terrainEnd) *float64 {
	if s, known := p.cachedSurface(e); known {
		return s
	}
	key := estimateKey(e)
	at := dhm.LatLon{Lat: float64(key.lat) / terrainSnap, Lon: float64(key.lon) / terrainSnap}
	// Admitted like a profile fetch: a refusal (dhm.ErrAdmissionFull) did
	// no upstream work, so nothing is remembered and the next profile
	// tries again.
	b, _, err := p.estimate.flight.DoAdmitted(ctx, key.String(), p.deadline, p.admit, func(fctx context.Context) ([]byte, error) {
		if p.set.upstreamTimeout > 0 {
			var cancel context.CancelFunc
			fctx, cancel = context.WithTimeout(fctx, p.set.upstreamTimeout)
			defer cancel()
		}
		v := math.NaN()
		s, err := dhm.SurfaceAt(fctx, p.estimate.src, at, terrainEstimateRadiusM)
		switch {
		case errors.Is(err, dhm.ErrNotCovered):
		case errors.Is(err, context.Canceled):
			return nil, err // every caller left: not a failure of upstream
		case err != nil:
			p.estimate.failed.Put(key, []byte{1}, p.now())
			if p.logFailure != nil {
				p.logFailure("terrain antenna estimate", err)
			}
			return nil, err
		case s.Coverage >= terrainEstimateMinCoverage:
			v = s.P90M
		}
		enc := binary.LittleEndian.AppendUint64(nil, math.Float64bits(v))
		p.estimate.cache.Put(key, enc, p.now())
		return enc, nil
	})
	if err != nil {
		return nil
	}
	return decodeSurface(b)
}

func decodeSurface(b []byte) *float64 {
	if len(b) != 8 {
		return nil
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(b))
	if math.IsNaN(v) {
		return nil
	}
	return &v
}
