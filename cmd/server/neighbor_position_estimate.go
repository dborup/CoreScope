package main

import (
	"math"
	"sort"
	"time"
)

// Initial heuristic limits, not calibrated probabilities or RF range bounds.
// Keep the SQL candidate limit and the fixed-size distance matrix in sync.
const neighborPositionCandidateLimit = 20
const neighborPositionHalfLife = 7 * 24 * time.Hour
const neighborPositionAmbiguityRatio = 0.8
const neighborPositionMinimumRelativeWeight = 0.1

// NeighborPositionEstimate describes evidence sufficiency, not positional
// accuracy. SpreadKm is the maximum separation of contributing neighbors;
// it is NOT an error radius. Persisted edges have no source/prefix confidence.
type NeighborPositionEstimate struct {
	Status                string   `json:"status"`
	Method                string   `json:"method"`
	ContributorCount      int      `json:"contributor_count"`
	CandidateCount        int      `json:"candidate_count"`
	SpreadKm              float64  `json:"spread_km"`
	Lat                   *float64 `json:"lat,omitempty"`
	Lon                   *float64 `json:"lon,omitempty"`
	NewestSeen            string   `json:"newest_seen,omitempty"`
	OldestSeen            string   `json:"oldest_seen,omitempty"`
	UnknownFreshnessCount int      `json:"unknown_freshness_count"`
}

type neighborPositionCandidate struct {
	Pubkey, Name string
	Lat, Lon     float64
	Count        float64
	LastSeen     time.Time
}

type neighborPositionResult struct {
	Estimate NeighborPositionEstimate
	// A single-neighbor path proxy remains available for legacy approx
	// markers, but never as a supported node position or an ambiguity bypass.
	Legacy   neighborEstimate
	LegacyOK bool
}

func validNeighborPosition(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsInf(lat, 0) && !math.IsNaN(lon) && !math.IsInf(lon, 0) &&
		lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 && !(lat == 0 && lon == 0)
}

func neighborPositionWeight(c neighborPositionCandidate, now time.Time) float64 {
	count := c.Count
	if math.IsNaN(count) || count < 1 {
		count = 1
	}
	// Traffic is only a weak tie-break: one edge has at most twice the
	// support of another equally fresh edge, regardless of lifetime volume.
	w := 1 + math.Log1p(math.Min(count, 20))/math.Log(21)
	if c.LastSeen.IsZero() || c.LastSeen.After(now.Add(5*time.Minute)) {
		return w * 0.25
	}
	age := now.Sub(c.LastSeen)
	if age < 0 {
		age = 0
	}
	return w * math.Exp2(-float64(age)/float64(neighborPositionHalfLife))
}

// estimateNeighborPosition is shared by single-node and bulk path lookups.
// Inputs are bounded to 20 before distance work. Each candidate seeds a
// radius neighborhood; evaluating all seeds avoids highest-count anchoring.
// The distance matrix and neighborhood support take O(20²) bounded work.
// Two disjoint, similarly supported neighborhoods cause abstention. This
// heuristic does not prove physical adjacency or independently known sources.
// maxEdgeKm<=0 retains the legacy opt-out: all valid candidates contribute.
func estimateNeighborPosition(input []neighborPositionCandidate, maxEdgeKm float64, now time.Time) neighborPositionResult {
	r := neighborPositionResult{Estimate: NeighborPositionEstimate{Status: "unavailable", Method: "neighbor_cluster_v1"}}
	// SQL already supplies a deterministically ranked top 20. Defensively
	// cap callers before copying/sorting as well; this is not an all-node API.
	if len(input) > neighborPositionCandidateLimit {
		input = input[:neighborPositionCandidateLimit]
	}
	candidates := append([]neighborPositionCandidate(nil), input...)
	for i := range candidates {
		if math.IsNaN(candidates[i].Count) || candidates[i].Count < 1 {
			candidates[i].Count = 1
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Count != candidates[j].Count {
			return candidates[i].Count > candidates[j].Count
		}
		if candidates[i].Pubkey != candidates[j].Pubkey {
			return candidates[i].Pubkey < candidates[j].Pubkey
		}
		return candidates[i].LastSeen.After(candidates[j].LastSeen)
	})
	if len(candidates) > neighborPositionCandidateLimit {
		candidates = candidates[:neighborPositionCandidateLimit]
	}
	valid := candidates[:0]
	seen := make(map[string]bool, len(candidates))
	strongestWeight := 0.0
	for _, c := range candidates {
		if !seen[c.Pubkey] && validNeighborPosition(c.Lat, c.Lon) && neighborPositionWeight(c, now) > 0 {
			valid = append(valid, c)
			seen[c.Pubkey] = true
			strongestWeight = math.Max(strongestWeight, neighborPositionWeight(c, now))
		}
	}
	// A negligible stale edge must not turn one meaningful neighbor into a
	// supposedly supported pair. This relative evidence floor is a heuristic,
	// not an absolute freshness guarantee: equally old evidence remains old,
	// and its timestamps remain visible to callers.
	candidates = valid[:0]
	for _, c := range valid {
		if neighborPositionWeight(c, now) >= strongestWeight*neighborPositionMinimumRelativeWeight {
			candidates = append(candidates, c)
		}
	}
	n := len(candidates)
	r.Estimate.CandidateCount = n
	if n == 0 {
		return r
	}
	var distance [neighborPositionCandidateLimit][neighborPositionCandidateLimit]float64
	var weights [neighborPositionCandidateLimit]float64
	var groups [neighborPositionCandidateLimit]uint32
	var support [neighborPositionCandidateLimit]float64
	var size [neighborPositionCandidateLimit]int
	for i, c := range candidates {
		weights[i] = neighborPositionWeight(c, now)
		for j := 0; j < i; j++ {
			distance[i][j] = haversineKm(c.Lat, c.Lon, candidates[j].Lat, candidates[j].Lon)
			distance[j][i] = distance[i][j]
		}
	}
	best := 0
	for i := range candidates {
		for j := range candidates {
			if maxEdgeKm <= 0 || distance[i][j] <= maxEdgeKm {
				groups[i] |= 1 << j
				support[i] += weights[j]
				size[i]++
			}
		}
		if support[i] > support[best] || (support[i] == support[best] && size[i] > size[best]) {
			best = i
		}
	}
	if support[best] <= 0 {
		return r
	}
	for i := range candidates {
		if groups[i]&groups[best] == 0 && support[i] >= neighborPositionAmbiguityRatio*support[best] {
			r.Estimate.Status = "ambiguous"
			return r
		}
	}
	var latSum, lonSum, totalWeight, spread float64
	var newest, oldest time.Time
	name := ""
	// Unwrap longitudes around a selected contributor to avoid averaging
	// +179.9 and -179.9 into Greenwich. This remains a heuristic centroid.
	referenceLon := candidates[best].Lon
	for i, c := range candidates {
		if groups[best]&(1<<i) == 0 {
			continue
		}
		if name == "" {
			name = c.Name
		}
		latSum += c.Lat * weights[i]
		delta := math.Mod(c.Lon-referenceLon+540, 360) - 180
		lonSum += (referenceLon + delta) * weights[i]
		totalWeight += weights[i]
		if c.LastSeen.IsZero() || c.LastSeen.After(now.Add(5*time.Minute)) {
			r.Estimate.UnknownFreshnessCount++
		} else {
			if newest.IsZero() || c.LastSeen.After(newest) {
				newest = c.LastSeen
			}
			if oldest.IsZero() || c.LastSeen.Before(oldest) {
				oldest = c.LastSeen
			}
		}
		for j := 0; j < i; j++ {
			if groups[best]&(1<<j) != 0 && distance[i][j] > spread {
				spread = distance[i][j]
			}
		}
	}
	lat := latSum / totalWeight
	lon := math.Mod(lonSum/totalWeight+540, 360) - 180
	// Preserve the exact proxy coordinate for legacy one-neighbor callers.
	if size[best] == 1 {
		lat, lon = candidates[best].Lat, candidates[best].Lon
	}
	r.Legacy = neighborEstimate{Name: name, Lat: lat, Lon: lon, ContributorCount: size[best], SpreadKm: spread}
	r.LegacyOK = true
	r.Estimate.ContributorCount = size[best]
	r.Estimate.SpreadKm = spread
	if !newest.IsZero() {
		r.Estimate.NewestSeen = newest.UTC().Format(time.RFC3339)
	}
	if !oldest.IsZero() {
		r.Estimate.OldestSeen = oldest.UTC().Format(time.RFC3339)
	}
	if size[best] < 2 {
		r.Estimate.Status = "insufficient"
		return r
	}
	r.Estimate.Status = "estimated"
	r.Estimate.Lat, r.Estimate.Lon = &lat, &lon
	return r
}
