package main

// #730 M2 — buildings versus trees in GET /api/terrain/profile, with
// map.dhm.profile.buildings.enabled (off by default).
//
// Where the surface model (DSM) stands more than T above the terrain
// (DTM), a sample inside an INSPIRE – Bygninger footprint is a building,
// anything else trees or other (internal/buildings). The lookup is one
// phase of the profile fetch, after the rasters:
//
//   - It runs inside the same Flight call, the same admission slot and the
//     same per-IP charge as the rasters: a cache miss is charged once, as
//     before, and a hit (or a request that joins a running fetch) makes no
//     building call at all. There is no new charge path.
//   - Only model=both that DHM covers end to end is classified (not a
//     partial one, nor one the foreign fill completed, #738: neither has a
//     surface verdict), and only the segments with an elevated sample are
//     fetched: a path where nothing stands above T costs no request.
//   - The phase has its own deadline (terrainBuildingsDeadline, inside the
//     profile's 25 s), and the footprints come through one shared
//     buildings.Fetcher: at most 4 upstream requests at once server-wide,
//     keyless, cached per segment, coalesced and admitted.
//   - A failure never fails the profile: the classification is
//     "unavailable" (or "partial" when only some segments failed), the
//     answer is 200, and the failure is logged by category. Such a profile
//     is cached for terrainBuildingsRetryTTL only, so the lookup is retried
//     soon after; a deterministic answer (nothing elevated, a truncated
//     segment) is cached like any profile.
//   - The classes are one byte per sample in the profile's cache entry,
//     behind a flag bit, so an entry of another format is rejected and
//     fetched again rather than misread.
//
// The response gains named fields only: classification{state, truncated},
// per-sample cls and verdict.surface.cause, plus the extended credit. With
// the classification off none of them appear and the answer is byte for
// byte the old one (TestTerrainProfileGolden_BuildingsOff).

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/dhm"
)

const (
	// terrainBuildingsDeadline bounds the building phase of one profile
	// fetch (#730 research §6).
	terrainBuildingsDeadline = 5 * time.Second
	// terrainBuildingsRetryTTL is how long a profile whose building lookup
	// failed is reused before a request fetches it again.
	terrainBuildingsRetryTTL = 10 * time.Minute
)

// terrainBuildingsMaxSegments is the most segments one profile's corridor
// has: 80.2 km in 10 km segments.
var terrainBuildingsMaxSegments = int(math.Ceil(float64(buildings.MaxCorridorM) / buildings.DefaultSegmentM))

// terrainBuildingSource fetches one corridor segment's footprints:
// *buildings.Fetcher, or a fake in tests.
type terrainBuildingSource interface {
	Fetch(ctx context.Context, s buildings.Segment) (buildings.Page, error)
}

// newTerrainBuildingSource is the production source on baseURL
// (buildings.DefaultBaseURL outside tests): the keyless INSPIRE client
// behind a Fetcher whose in-flight bound covers every admitted profile's
// segments, so the profile admission (maxInFlight) is the bound that
// bites. At most 4 requests run upstream at once (the Fetcher's
// Concurrency).
func newTerrainBuildingSource(set terrainSettings, baseURL string) terrainBuildingSource {
	co := buildings.DefaultClientOptions()
	co.BaseURL = baseURL
	o := buildings.DefaultFetcherOptions()
	o.MaxInFlight = set.maxInFlight * terrainBuildingsMaxSegments
	return buildings.NewFetcher(buildings.NewClient(co), o)
}

// The states of a classification, as cached (one byte) and as served.
const (
	terrainClassFull        byte = 1 // every elevated sample has a class
	terrainClassPartial     byte = 2 // some elevated samples have none
	terrainClassUnavailable byte = 3 // none could be classified
)

var terrainClassStates = map[byte]string{
	terrainClassFull:        "full",
	terrainClassPartial:     "partial",
	terrainClassUnavailable: "unavailable",
}

// terrainClasses is the classification of one profile: its state, whether
// a segment had more buildings than one page holds, and one
// buildings.Class per sample. degraded (never cached) says a segment's
// lookup failed, so the profile is kept for terrainBuildingsRetryTTL only.
type terrainClasses struct {
	state     byte
	truncated bool
	cls       []buildings.Class
	degraded  bool
}

// The causes of verdict.surface.cause.
const (
	terrainCauseBuilding   = "building"
	terrainCauseVegetation = "vegetation"
	terrainCauseUnknown    = "unknown"
)

// unclassified is the classification of n samples that could not be
// classified.
func unclassified(n int) *terrainClasses {
	return &terrainClasses{state: terrainClassUnavailable, cls: make([]buildings.Class, n)}
}

// classifyBuildings is the building phase of a profile fetch. ctx is the
// fetch's: a profile whose requesters have all gone returns its error, so
// nothing is cached; anything else, failure included, is a
// classification.
func (p *terrainProfiler) classifyBuildings(ctx context.Context, plan *dhm.Plan, e terrainElevation) (*terrainClasses, error) {
	n := len(plan.E)
	out := unclassified(n)
	if e.dsm == nil || n < 2 || plan.Covered() < n {
		// Not wholly inside DHM: no surface verdict (#689 N1, #738), so
		// nothing to tell apart, and no request.
		return out, nil
	}
	for _, v := range e.dtm {
		if math.IsNaN(float64(v)) {
			return out, nil
		}
	}
	c, err := buildings.NewCorridor([2]float64{plan.E[0], plan.N[0]}, [2]float64{plan.E[n-1], plan.N[n-1]},
		buildings.DefaultRadiusM, buildings.DefaultSegmentM)
	if err != nil {
		return out, nil
	}
	s := buildings.Samples{E: plan.E, N: plan.N, DTM: e.dtm, DSM: e.dsm, StepM: plan.StepM}
	o := buildings.DefaultOptions()
	need, err := c.Needed(s, o)
	if err != nil {
		return out, nil
	}
	segs := c.Segments()
	pages := make([]*buildings.Page, len(segs))
	errs := make([]error, len(segs))
	bctx, cancel := context.WithTimeout(ctx, p.buildingsDeadline)
	defer cancel()
	var wg sync.WaitGroup
	asked := 0
	for k := range segs {
		if !need[k] {
			continue
		}
		asked++
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			page, err := p.buildings.Fetch(bctx, segs[k])
			if err != nil {
				errs[k] = err
				return
			}
			pages[k] = &page
		}(k)
	}
	wg.Wait()
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}
	if asked == 0 {
		// Nothing stands above T: every elevated sample (none) is
		// classified, without a request.
		out.state = terrainClassFull
		return out, nil
	}
	answered := 0
	for k, err := range errs {
		switch {
		case err != nil:
			if !out.degraded && p.logFailure != nil {
				p.logFailure("terrain profile buildings", buildingsFailure(err))
			}
			out.degraded = true
		case pages[k] != nil:
			answered++
			out.truncated = out.truncated || pages[k].Truncated
		}
	}
	if answered == 0 {
		return out, nil
	}
	cls, err := buildings.Classify(c, s, pages, o)
	if err != nil {
		return out, nil
	}
	out.cls, out.state = cls, terrainClassFull
	for i, cl := range cls {
		if cl == buildings.Unclassified && s.Elevated(i, o.ThresholdM) {
			out.state = terrainClassPartial
			break
		}
	}
	return out, nil
}

// buildingsFailure is err as a logged category: the phase's own deadline
// is a timeout.
func buildingsFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return dhm.FetchError{Reason: "timeout"}
	}
	return err
}

// ttl is how long the profile is cached: briefly when its building
// lookup failed, else the profile's TTL.
func (c *terrainClasses) ttl(profileTTL time.Duration) time.Duration {
	if c != nil && c.degraded && terrainBuildingsRetryTTL < profileTTL {
		return terrainBuildingsRetryTTL
	}
	return profileTTL
}

// used reports whether any sample was classified, i.e. whether footprints
// went into the answer and are to be credited.
func (c *terrainClasses) used() bool {
	for _, cl := range c.cls {
		if cl != buildings.Unclassified {
			return true
		}
	}
	return false
}

// response is the classification as served.
func (c *terrainClasses) response() *TerrainClassification {
	return &TerrainClassification{State: terrainClassStates[c.state], Truncated: c.truncated}
}

// surfaceCause says what stands where the surface verdict is decided
// (dhm.Link.Obstructions): "building" when any of those samples that is
// elevated is a building, else "unknown" when any is unclassified, else
// "vegetation" when any is trees/other. It is "" (absent) when the
// surface is clear or unknown, or when only the terrain itself decides
// it. The verdict is not changed (#681 owner decision 6).
func (c *terrainClasses) surfaceCause(obstructions []int, e terrainElevation) string {
	t := buildings.DefaultOptions().ThresholdM
	s := buildings.Samples{DTM: e.dtm, DSM: e.dsm}
	unknown, vegetation := false, false
	for _, i := range obstructions {
		if !s.Elevated(i, t) {
			continue
		}
		switch c.cls[i] {
		case buildings.Building:
			return terrainCauseBuilding
		case buildings.Vegetation:
			vegetation = true
		default:
			unknown = true
		}
	}
	switch {
	case unknown:
		return terrainCauseUnknown
	case vegetation:
		return terrainCauseVegetation
	}
	return ""
}

// sampleClass is sample i's class as served: "b", "v", or "" (absent).
func (c *terrainClasses) sampleClass(i int) string {
	if cl := c.cls[i]; cl != buildings.Unclassified {
		return string(rune(cl))
	}
	return ""
}

// The cache entry (terrainElevation.encode): byte 4 holds flags, bit 0 a
// DSM and bits 1-3 the foreign fill (#738); terrainEntryClassified (bit 4)
// marks a classified entry, whose elevations are followed by the state,
// the truncated flag and one class byte per sample. Before #730 M2 bit 4
// had to be 0, so an older reader refuses a classified entry too.
const (
	terrainEntryClassified byte = 1 << 4
	terrainEntryHeader          = 5
)

// terrainEntryOK checks an entry's DSM and classification flags and its
// length against what the profiler expects for n samples: an entry of
// another format is rejected, never misread. decodeTerrainElevation checks
// the fill bits.
func terrainEntryOK(b []byte, n int, both, classified bool) bool {
	if len(b) < terrainEntryHeader {
		return false
	}
	models := 1
	if both {
		models = 2
	}
	size := terrainEntryHeader + 4*n*models
	if classified {
		size += 2 + n
	}
	return b[4]&1 == 1 == both && (b[4]&terrainEntryClassified != 0) == classified && len(b) == size
}

// appendTo writes c after an entry's elevations.
func (c *terrainClasses) appendTo(out []byte) []byte {
	truncated := byte(0)
	if c.truncated {
		truncated = 1
	}
	out = append(out, c.state, truncated)
	for _, cl := range c.cls {
		out = append(out, byte(cl))
	}
	return out
}

var errTerrainEntry = errors.New("terrain cache entry does not match its key")

// decodeTerrainClasses reads the trailer appendTo wrote for n samples.
func decodeTerrainClasses(b []byte, n int) (*terrainClasses, error) {
	if len(b) != 2+n || terrainClassStates[b[0]] == "" || b[1] > 1 {
		return nil, errTerrainEntry
	}
	c := &terrainClasses{state: b[0], truncated: b[1] == 1, cls: make([]buildings.Class, n)}
	for i, v := range b[2:] {
		switch cl := buildings.Class(v); cl {
		case buildings.Unclassified, buildings.Building, buildings.Vegetation:
			c.cls[i] = cl
		default:
			return nil, errTerrainEntry
		}
	}
	return c, nil
}
