package dhm

import "math"

// #681 M3 — link physics for the terrain profile (plan §4).

const (
	SpeedOfLight = 299792458.0 // m/s
	EarthRadiusM = 6371000.0   // mean radius
	// FirmwareFreqMHz is MeshCore's shipped LORA_FREQ build flag
	// (firmware/platformio.ini), the last fallback for the frequency.
	FirmwareFreqMHz = 869.618
	// DefaultKFactor is the standard-atmosphere effective earth factor.
	DefaultKFactor = 4.0 / 3
	// Criterion is the fraction of the first Fresnel zone that must stay
	// clear for a link to count as clear by the usual engineering rule.
	Criterion = 0.6
)

// Wavelength is λ = c / f in metres for f in MHz.
func Wavelength(freqMHz float64) float64 { return SpeedOfLight / (freqMHz * 1e6) }

// FresnelRadius is the first Fresnel zone radius at a point d1 and d2
// metres from the two ends: √(λ·d1·d2 / (d1+d2)).
func FresnelRadius(lambda, d1, d2 float64) float64 {
	if d1 <= 0 || d2 <= 0 {
		return 0
	}
	return math.Sqrt(lambda * d1 * d2 / (d1 + d2))
}

// EarthBulge is the height the earth's curvature adds to the ground d1 and
// d2 metres from the two ends, for an effective earth factor k:
// d1·d2 / (2·k·R).
func EarthBulge(d1, d2, k float64) float64 { return d1 * d2 / (2 * k * EarthRadiusM) }

// VerdictState is the clearance class of a link.
type VerdictState string

const (
	VerdictClear      VerdictState = "clear"      // the whole first Fresnel zone is clear
	VerdictFresnel100 VerdictState = "fresnel100" // F1 obstructed, but ≥ 60 % of it clear: the criterion holds
	VerdictFresnel60  VerdictState = "fresnel60"  // line of sight clear, but < 60 % of F1: the criterion fails
	VerdictBlocked    VerdictState = "blocked"    // the line of sight itself is obstructed
	VerdictUnknown    VerdictState = "unknown"    // missing elevation: no verdict
)

// ClassifyClearance classes the worst clearance ratio along a path: the
// clearance below the line of sight divided by the F1 radius there. The
// boundaries belong to the better class: exactly grazing (0) is not
// blocked, exactly 60 % meets the criterion, exactly 100 % is clear.
func ClassifyClearance(ratio float64) VerdictState {
	switch {
	case math.IsNaN(ratio):
		return VerdictUnknown
	case ratio >= 1:
		return VerdictClear
	case ratio >= Criterion:
		return VerdictFresnel100
	case ratio >= 0:
		return VerdictFresnel60
	}
	return VerdictBlocked
}

// Link is the geometry of one path: evenly spaced samples over DistanceM,
// antenna heights above sea level at both ends (ground + antenna), and
// the radio parameters.
type Link struct {
	DistanceM        float64
	HeightA, HeightB float64
	LambdaM          float64
	K                float64
}

// Assessment is the worst point of a path against one surface.
type Assessment struct {
	State           VerdictState
	WorstIndex      int     // -1 when State is unknown
	WorstAtM        float64 // distance from A
	WorstClearanceM float64 // line of sight minus (surface + bulge); negative = above the LoS
	WorstRatio      float64 // WorstClearanceM / F1 radius there
	// AntennaBelowA and AntennaBelowB say the surface is above the antenna
	// at that end (#745).
	AntennaBelowA, AntennaBelowB bool
}

// Assess finds the interior sample with the smallest clearance ratio.
// The line of sight is kept straight and the earth bulge is added to the
// surface, the conventional path-profile rendering (plan §4.3). Any
// missing value (NaN), at any sample of surface, endpoints included, or
// in an endpoint height, makes the verdict unknown: a partial profile
// never reports "clear". The endpoint heights come from the DTM, so the
// series' own endpoints are checked too: a DSM gap at an antenna is
// otherwise invisible (#696).
//
// A surface above the antenna at an endpoint obstructs the line of sight
// at the antenna itself (#745): the verdict is blocked and AntennaBelowA
// or AntennaBelowB says where. Exactly at the antenna is grazing, not
// blocked. The worst point stays the worst interior sample when that
// obstructs the line of sight too, however much shallower the endpoint
// is, so the real obstruction is the one named and kept in the chart
// (#745 review F1). Otherwise the endpoint is the worst point, with a
// ratio of −Inf since F1 has no radius there; the deeper one if both
// ends are below the surface, A on a tie. Against the DTM this never
// fires: the endpoint heights are the DTM plus an antenna of at least 0 m.
func (l Link) Assess(surface []float32) Assessment {
	unknown := Assessment{State: VerdictUnknown, WorstIndex: -1}
	n := len(surface)
	if n < 3 || math.IsNaN(l.HeightA) || math.IsNaN(l.HeightB) ||
		math.IsNaN(float64(surface[0])) || math.IsNaN(float64(surface[n-1])) {
		return unknown
	}
	step := l.DistanceM / float64(n-1)
	worst := Assessment{WorstIndex: -1, WorstRatio: math.Inf(1)}
	for i := 1; i < n-1; i++ {
		s := float64(surface[i])
		if math.IsNaN(s) {
			return unknown
		}
		d1 := float64(i) * step
		d2 := l.DistanceM - d1
		los := l.HeightA + (l.HeightB-l.HeightA)*d1/l.DistanceM
		clearance := los - (s + EarthBulge(d1, d2, l.K))
		ratio := clearance / FresnelRadius(l.LambdaM, d1, d2)
		if ratio < worst.WorstRatio {
			worst = Assessment{WorstIndex: i, WorstAtM: d1, WorstClearanceM: clearance, WorstRatio: ratio}
		}
	}
	// Endpoints last, so missing data above still wins.
	end := Assessment{WorstIndex: -1}
	clearanceA := l.HeightA - float64(surface[0])
	clearanceB := l.HeightB - float64(surface[n-1])
	if clearanceA < 0 {
		end = Assessment{WorstIndex: 0, WorstClearanceM: clearanceA}
	}
	if clearanceB < 0 && (end.WorstIndex < 0 || clearanceB < end.WorstClearanceM) {
		end = Assessment{WorstIndex: n - 1, WorstAtM: l.DistanceM, WorstClearanceM: clearanceB}
	}
	if end.WorstIndex >= 0 && worst.WorstRatio >= 0 {
		end.WorstRatio = math.Inf(-1)
		worst = end
	}
	worst.AntennaBelowA, worst.AntennaBelowB = clearanceA < 0, clearanceB < 0
	worst.State = ClassifyClearance(worst.WorstRatio)
	return worst
}

// Decimate picks at most max indices of n evenly spaced samples, always
// including the first and the last. The keep indices come first, in the
// order given, while they fit (out-of-range ones are ignored), so
// thinning a profile for the chart never hides its worst point or a data
// gap (#745); evenly spaced indices fill the room that is left. The
// result is strictly increasing. A max below 2 has no room for both ends:
// max 1 is the first index alone, max 0 or less none.
func Decimate(n, max int, keep ...int) []int {
	if n <= max {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	switch {
	case max == 1:
		return []int{0}
	case max < 1:
		return []int{}
	}
	picked := make([]bool, n)
	picked[0], picked[n-1] = true, true
	count := 2
	for _, k := range keep {
		if count == max {
			break
		}
		if k > 0 && k < n-1 && !picked[k] {
			picked[k] = true
			count++
		}
	}
	// The evenly spaced picks include the two endpoints, already counted:
	// at most max−count new ones.
	slots := max - count + 2
	for j := 1; j < slots-1; j++ {
		picked[int(math.Round(float64(j)*float64(n-1)/float64(slots-1)))] = true
	}
	out := make([]int, 0, max)
	for i, p := range picked {
		if p {
			out = append(out, i)
		}
	}
	return out
}
