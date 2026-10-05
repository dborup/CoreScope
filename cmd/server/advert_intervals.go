package main

import (
	"encoding/json"
	"math"
	"sort"
	"time"
)

// Estimated advert intervals per node (#245 M1): how often a node sends
// flood and zero-hop adverts, estimated from the gaps between the adverts
// CoreScope heard, for the node detail page.
//
// What the firmware does (MeshCore source; AGENTS.md: read it, don't guess):
//   - flood.advert.interval: flood_advert_interval, whole hours, 0 = off,
//     else 3-168 (src/helpers/CommonCLI.h:34, src/helpers/CommonCLI.cpp:486-495).
//     Default 47 h on repeaters and room servers
//     (examples/simple_repeater/MyMesh.cpp:904), off on sensors.
//   - advert.interval: advert_interval, stored as minutes / 2, 0 = off, else
//     60-240 minutes, so even minutes (src/helpers/CommonCLI.h:33,
//     src/helpers/CommonCLI.cpp:496-505). A new install starts at 2 minutes
//     (examples/simple_repeater/MyMesh.cpp:903) until the first savePrefs
//     turns anything below 60 off (src/helpers/CommonCLI.cpp:160-165).
//   - Both timers re-arm with futureMillis(interval) when they fire, and the
//     flood advert re-arms the zero-hop timer too
//     (examples/simple_repeater/MyMesh.cpp:1044-1058, 1294-1306): the
//     zero-hop gap across a flood advert is irregular.
//   - Extra adverts: the advert / advert.zerohop CLI commands do not re-arm
//     a timer (src/helpers/CommonCLI.cpp:191-198), and a boot sends a
//     zero-hop advert (examples/simple_repeater/main.cpp:119).
//   - The advert carries the sender's RTC time (src/Mesh.cpp:418), which
//     can be years off or jump (clkreboot, src/helpers/CommonCLI.cpp:187-190).
//
// The estimate uses the adverts the Flood and Zero-hop panels list (the
// newest nodeAdvertRouteLimit per class, GetNodeAdvertRoutes), so it costs
// no extra query; mixed and unknown adverts are left out as their class is
// ambiguous. transmissions.hash is unique, so the rows are distinct adverts.
//
// The thresholds below are hardcoded; with the minimum samples they are
// candidates for the customizer (#245 "Later", AGENTS.md rule 8).

type advertIntervalClass int

const (
	advertIntervalFlood advertIntervalClass = iota
	advertIntervalZeroHop
)

const (
	advertConfidenceHigh   = "high"
	advertConfidenceMedium = "medium"
	advertConfidenceLow    = "low"
	advertConfidenceNone   = "none"
)

const (
	// advertIntervalMinSamples is the fewest adverts (two gaps) estimated.
	advertIntervalMinSamples = 3
	// advertIntervalTol is how far a gap may be from k x interval, as a
	// fraction of the interval, and how far outside the firmware range an
	// estimate may be and still snap to it.
	advertIntervalTol = 0.10
	// advertIntervalMaxMultiple is the longest run of missed adverts a gap
	// may stand for (k - 1).
	advertIntervalMaxMultiple = 4
	// advertIntervalRaisedRun is how many of the newest gaps in a row at the
	// same multiple of the interval read as a raised setting rather than
	// adverts missed in a row.
	advertIntervalRaisedRun = 3
	// advertClockSlackS is how far the sender clock may be ahead of
	// first_seen, and the least disagreement between a sender gap and the
	// first_seen gap that rejects the sender gap (else 10 % of the gap).
	advertClockSlackS = 600
)

// AdvertIntervalEstimate is one route class of NodeAdvertIntervals.
type AdvertIntervalEstimate struct {
	// IntervalS is the estimate in seconds, snapped to the nearest value
	// the firmware can be set to when it is close to one; null when there
	// is no estimate (confidence none).
	IntervalS *int64 `json:"interval_s"`
	// RawIntervalS is the median before snapping.
	RawIntervalS *int64 `json:"raw_interval_s"`
	Snapped      bool   `json:"snapped"`
	// Samples is the number of adverts used, GapsUsed the gaps between them
	// that fit a whole multiple of the interval.
	Samples    int     `json:"samples"`
	GapsUsed   int     `json:"gaps_used"`
	Confidence string  `json:"confidence"`
	LastAdvert *string `json:"last_advert"`
}

// NodeAdvertIntervals is advertIntervals on node detail (include=advertRoutes).
type NodeAdvertIntervals struct {
	Window  int                    `json:"window"`
	Flood   AdvertIntervalEstimate `json:"flood"`
	ZeroHop AdvertIntervalEstimate `json:"zero_hop"`
}

// advertIntervalSample is one advert: the sender's clock (0 when unknown)
// and when it was first heard.
type advertIntervalSample struct {
	senderTS int64
	heard    time.Time
}

// nodeAdvertIntervals estimates both classes from the per-class lists.
func nodeAdvertIntervals(byRoute NodeAdvertsByRoute) NodeAdvertIntervals {
	return NodeAdvertIntervals{
		Window:  byRoute.Limit,
		Flood:   estimateAdvertInterval(advertIntervalSamples(byRoute.Flood), advertIntervalFlood),
		ZeroHop: estimateAdvertInterval(advertIntervalSamples(byRoute.ZeroHop), advertIntervalZeroHop),
	}
}

// advertIntervalSamples reads first_seen and the decoded advert timestamp
// of each row. Rows without a parseable first_seen are skipped; a missing
// or malformed timestamp leaves senderTS 0.
func advertIntervalSamples(rows NodeAdvertRows) []advertIntervalSample {
	out := make([]advertIntervalSample, 0, len(rows))
	for _, r := range rows {
		fs, _ := r["first_seen"].(string)
		heard, ok := parseRelayTS(fs)
		if !ok {
			continue
		}
		s := advertIntervalSample{heard: heard}
		if dj, _ := r["decoded_json"].(string); dj != "" {
			var d struct {
				Timestamp int64 `json:"timestamp"`
			}
			if json.Unmarshal([]byte(dj), &d) == nil {
				s.senderTS = d.Timestamp
			}
		}
		out = append(out, s)
	}
	return out
}

// advertIntervalFloorS is the shortest interval the class's timer can run
// at, less the tolerance: shorter gaps are never a candidate interval (a
// burst of manual adverts).
func advertIntervalFloorS(class advertIntervalClass) float64 {
	if class == advertIntervalZeroHop {
		return 120 * (1 - advertIntervalTol)
	}
	return 3 * 3600 * (1 - advertIntervalTol)
}

// estimateAdvertInterval estimates one class's interval:
//  1. Gaps between consecutive adverts (by first_seen). The sender's clock
//     gives the gap when both adverts have a plausible one - not ahead of
//     first_seen by more than advertClockSlackS - and its gap is positive
//     and agrees with first_seen's (a clock jump does not); else first_seen.
//  2. A candidate interval is a gap seen directly (within the tolerance) in
//     at least two gaps and a quarter of them, and not shorter than the
//     class's timer allows. The one explaining the most gaps as 1-4 x
//     itself wins (ties: the longer, fewer missed adverts). k x interval
//     is k - 1 missed adverts; shorter gaps are extra adverts, dropped.
//  3. A raised setting: a new interval of 2-4x the old one is explained by
//     the old one as missed adverts in every gap. When the newest
//     advertIntervalRaisedRun gaps are all the same multiple k > 1, that is
//     the setting, not adverts missed in a row: only the adverts since the
//     newest gap at the old interval are used, and step 2 runs on them.
//     Setting an interval re-arms its timer at once
//     (src/helpers/CommonCLI.cpp:491-492, 500-501). A lowered setting needs
//     nothing: the new interval explains the old gaps as multiples once it
//     is a candidate.
//  4. The interval is the median of gap/k over the gaps it explains, then
//     snapped to the firmware's values (snapAdvertInterval).
//
// The candidate search is O(gaps^2) with gaps < nodeAdvertRouteLimit, run
// at most once per raised setting found.
func estimateAdvertInterval(samples []advertIntervalSample, class advertIntervalClass) AdvertIntervalEstimate {
	est := AdvertIntervalEstimate{Samples: len(samples), Confidence: advertConfidenceNone}
	if len(samples) == 0 {
		return est
	}
	s := append([]advertIntervalSample(nil), samples...)
	sort.Slice(s, func(i, j int) bool {
		if !s[i].heard.Equal(s[j].heard) {
			return s[i].heard.Before(s[j].heard)
		}
		return s[i].senderTS < s[j].senderTS
	})
	last := s[len(s)-1].heard.UTC().Format(time.RFC3339)
	est.LastAdvert = &last
	if len(s) < advertIntervalMinSamples {
		return est
	}

	gaps, from := advertGaps(s)
	best, start := advertIntervalCandidate(gaps, class), 0
	for best > 0 {
		cut := advertIntervalRaised(gaps, best)
		if cut == 0 {
			break
		}
		start = from[cut]
		gaps, from = gaps[cut:], from[cut:]
		best = advertIntervalCandidate(gaps, class)
	}
	if best == 0 {
		return est
	}
	est.Samples = len(s) - start

	var units []float64
	for _, g := range gaps {
		if k := advertGapMultiple(g, best); k > 0 {
			units = append(units, g/float64(k))
		}
	}
	interval := medianFloat(units)

	used, irregular := 0, 0
	for _, g := range gaps {
		switch {
		case advertGapMultiple(g, interval) > 0:
			used++
		case g >= interval*(1-advertIntervalTol):
			irregular++
		}
	}
	if used < 2 {
		return est
	}
	raw := int64(math.Round(interval))
	snapped, ok := snapAdvertInterval(interval, class)
	est.RawIntervalS, est.IntervalS, est.Snapped, est.GapsUsed = &raw, &snapped, ok, used
	ratio := float64(used) / float64(used+irregular)
	switch {
	case used >= 6 && ratio >= 0.75:
		est.Confidence = advertConfidenceHigh
	case used >= 3 && ratio >= 0.5:
		est.Confidence = advertConfidenceMedium
	default:
		est.Confidence = advertConfidenceLow
	}
	return est
}

// advertGaps is the positive gaps in seconds between consecutive samples
// (sorted by heard), each from the sender's clock when it is plausible, and
// for each gap the index of the sample it starts at.
func advertGaps(s []advertIntervalSample) (gaps []float64, from []int) {
	plausible := func(a advertIntervalSample) bool {
		return a.senderTS > 0 && a.senderTS <= a.heard.Unix()+advertClockSlackS
	}
	gaps, from = make([]float64, 0, len(s)-1), make([]int, 0, len(s)-1)
	for i := 1; i < len(s); i++ {
		g := s[i].heard.Sub(s[i-1].heard).Seconds()
		if plausible(s[i]) && plausible(s[i-1]) {
			ds := float64(s[i].senderTS - s[i-1].senderTS)
			if ds > 0 && math.Abs(ds-g) <= math.Max(advertClockSlackS, advertIntervalTol*g) {
				g = ds
			}
		}
		if g > 0 {
			gaps, from = append(gaps, g), append(from, i-1)
		}
	}
	return gaps, from
}

// advertIntervalCandidate picks the interval candidate (step 2 of
// estimateAdvertInterval), or 0 when no gap qualifies.
func advertIntervalCandidate(gaps []float64, class advertIntervalClass) float64 {
	best, bestExplained := 0.0, 0
	for _, c := range gaps {
		if c < advertIntervalFloorS(class) {
			continue
		}
		direct, explained := 0, 0
		for _, g := range gaps {
			k := advertGapMultiple(g, c)
			if k == 1 {
				direct++
			}
			if k > 0 {
				explained++
			}
		}
		if direct < 2 || direct*4 < len(gaps) {
			continue
		}
		if explained > bestExplained || (explained == bestExplained && c > best) {
			best, bestExplained = c, explained
		}
	}
	return best
}

// advertIntervalRaised spots a raised setting (step 3 of
// estimateAdvertInterval): the newest advertIntervalRaisedRun gaps that are
// a multiple of interval are all the same k > 1. Gaps that are no multiple
// (extra adverts, the gap at the change) are skipped. It returns the index
// of the first gap after the newest one at interval itself, where the new
// setting starts, or 0 when there is no such run.
func advertIntervalRaised(gaps []float64, interval float64) int {
	run, runK := 0, 0
	for i := len(gaps) - 1; i >= 0; i-- {
		k := advertGapMultiple(gaps[i], interval)
		switch {
		case k == 0:
		case run < advertIntervalRaisedRun:
			if k == 1 || (runK > 0 && k != runK) {
				return 0
			}
			run, runK = run+1, k
		case k == 1:
			return i + 1
		}
	}
	return 0
}

// advertGapMultiple is k when gap is k x interval within the tolerance
// (k = 1..advertIntervalMaxMultiple), else 0.
func advertGapMultiple(gap, interval float64) int {
	k := int(math.Round(gap / interval))
	if k < 1 || k > advertIntervalMaxMultiple || math.Abs(gap-float64(k)*interval) > advertIntervalTol*interval {
		return 0
	}
	return k
}

func medianFloat(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// snapAdvertInterval rounds seconds to the nearest value the class's setting
// can take (see the file comment): flood whole hours 3-168, zero-hop even
// minutes 60-240 or the 2-minute new-install default. Estimates further
// than the tolerance outside that range are returned rounded to a second
// and not snapped.
func snapAdvertInterval(seconds float64, class advertIntervalClass) (int64, bool) {
	unit, lo, hi := 3600.0, 3.0, 168.0 // flood: hours
	if class == advertIntervalZeroHop {
		if math.Abs(seconds-120) <= advertIntervalTol*120 {
			return 120, true
		}
		unit, lo, hi = 120.0, 30.0, 120.0 // zero-hop: 2-minute steps, 60-240 min
	}
	if seconds < lo*unit*(1-advertIntervalTol) || seconds > hi*unit*(1+advertIntervalTol) {
		return int64(math.Round(seconds)), false
	}
	steps := math.Min(hi, math.Max(lo, math.Round(seconds/unit)))
	return int64(steps * unit), true
}
