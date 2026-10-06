package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// #245 M1: estimated flood / zero-hop advert intervals per node. The
// firmware facts the expectations rest on (MeshCore, see advert_intervals.go):
//   - flood.advert.interval: whole hours, 3-168, 0 = off
//     (src/helpers/CommonCLI.cpp:486-495); default 47 h on repeaters
//     (examples/simple_repeater/MyMesh.cpp:904).
//   - advert.interval: minutes stored as mins/2, 60-240, 0 = off
//     (src/helpers/CommonCLI.cpp:496-505); 2 min on an untouched new install
//     (examples/simple_repeater/MyMesh.cpp:903, turned off by the first
//     savePrefs, src/helpers/CommonCLI.cpp:162-165).

var aiBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// aiSeries builds samples at the given multiples of interval after aiBase.
// The sender clock reads heard - 2 s (send-to-first-heard delay).
func aiSeries(interval time.Duration, at ...float64) []advertIntervalSample {
	out := make([]advertIntervalSample, 0, len(at))
	for _, k := range at {
		heard := aiBase.Add(time.Duration(k * float64(interval)))
		out = append(out, advertIntervalSample{senderTS: heard.Unix() - 2, heard: heard})
	}
	return out
}

// aiGaps builds samples from aiBase separated by the given gaps, oldest
// first, the sender clock as in aiSeries.
func aiGaps(gaps ...time.Duration) []advertIntervalSample {
	heard := aiBase
	out := []advertIntervalSample{{senderTS: heard.Unix() - 2, heard: heard}}
	for _, g := range gaps {
		heard = heard.Add(g)
		out = append(out, advertIntervalSample{senderTS: heard.Unix() - 2, heard: heard})
	}
	return out
}

// aiRepeat is n copies of gap.
func aiRepeat(gap time.Duration, n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = gap
	}
	return out
}

func aiInts(from, to int) []float64 {
	var out []float64
	for i := from; i <= to; i++ {
		out = append(out, float64(i))
	}
	return out
}

func aiCheck(t *testing.T, got AdvertIntervalEstimate, wantInterval int64, wantConf string, wantSamples, wantGaps int) {
	t.Helper()
	if wantInterval == 0 {
		if got.IntervalS != nil {
			t.Fatalf("interval_s = %d, want null (%+v)", *got.IntervalS, got)
		}
	} else if got.IntervalS == nil || *got.IntervalS != wantInterval {
		t.Fatalf("interval_s = %v, want %d (%+v)", ptrStr(got.IntervalS), wantInterval, got)
	}
	if got.Confidence != wantConf || got.Samples != wantSamples || got.GapsUsed != wantGaps {
		t.Fatalf("confidence/samples/gaps_used = %s/%d/%d, want %s/%d/%d (%+v)",
			got.Confidence, got.Samples, got.GapsUsed, wantConf, wantSamples, wantGaps, got)
	}
}

func ptrStr(p *int64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

func TestEstimateAdvertInterval_RegularSeries(t *testing.T) {
	s := aiSeries(12*time.Hour, aiInts(0, 9)...)
	got := estimateAdvertInterval(s, advertIntervalFlood)
	aiCheck(t, got, 12*3600, advertConfidenceHigh, 10, 9)
	if !got.Snapped || got.RawIntervalS == nil || *got.RawIntervalS != 12*3600 {
		t.Fatalf("raw/snapped = %v/%v", ptrStr(got.RawIntervalS), got.Snapped)
	}
	if want := aiBase.Add(9 * 12 * time.Hour).Format(time.RFC3339); got.LastAdvert == nil || *got.LastAdvert != want {
		t.Fatalf("last_advert = %v, want %s", got.LastAdvert, want)
	}
	// Input order does not matter: rows arrive in ingest order.
	rev := make([]advertIntervalSample, len(s))
	for i := range s {
		rev[len(s)-1-i] = s[i]
	}
	aiCheck(t, estimateAdvertInterval(rev, advertIntervalFlood), 12*3600, advertConfidenceHigh, 10, 9)
}

// Missed adverts: a gap of 2x or 3x the interval is the interval with one or
// two adverts nobody heard, not a longer interval. Every gap fits.
func TestEstimateAdvertInterval_MissedAdverts(t *testing.T) {
	// 47 h (the repeater default); adverts 3, 6 and 7 missed: gaps 47, 47,
	// 94, 47, 141, 47, 47, 47 h.
	s := aiSeries(47*time.Hour, 0, 1, 2, 4, 5, 8, 9, 10, 11)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 47*3600, advertConfidenceHigh, 9, 8)

	// Half the gaps doubled or tripled: the median of the raw gaps is 2x,
	// only the multiple handling brings it back to 12 h.
	s = aiSeries(12*time.Hour, 0, 2, 4, 5, 7, 10, 12, 13, 15)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceHigh, 9, 8)
}

// Most adverts missed (a zero-hop advert heard only now and then): the gaps
// cluster at 2x, but enough sit at the interval itself to show it.
func TestEstimateAdvertInterval_MostlyMissed(t *testing.T) {
	// 60 min zero-hop; gaps 120, 120, 60, 180, 120, 60, 120 min.
	s := aiSeries(time.Hour, 0, 2, 4, 5, 8, 10, 11, 13)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalZeroHop), 3600, advertConfidenceHigh, 8, 7)
}

// Manual or extra adverts (the advert / advert.zerohop CLI commands do not
// re-arm the timer, src/helpers/CommonCLI.cpp:191-198; a reboot sends a
// zero-hop advert, examples/simple_repeater/main.cpp:119) split a regular gap
// into two short ones. Those gaps are dropped; the interval stays.
func TestEstimateAdvertInterval_ExtraAdverts(t *testing.T) {
	// 120 min zero-hop, 12 timer adverts plus extras at 2.3 and 7.75.
	s := aiSeries(2*time.Hour, append(aiInts(0, 11), 2.3, 7.75)...)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalZeroHop), 7200, advertConfidenceHigh, 14, 9)

	// A burst of manual adverts (minutes apart, below the 3 h flood minimum,
	// so never a timer interval) must not drag the interval down: 6 timer
	// adverts at 12 h and 6 manual ones 7 min apart after one of them - more
	// "regular" gaps than the timer's own.
	s = aiSeries(12*time.Hour, 0, 1, 2, 3, 4, 5, 3.01, 3.02, 3.03, 3.04, 3.05, 3.06)
	got := estimateAdvertInterval(s, advertIntervalFlood)
	aiCheck(t, got, 12*3600, advertConfidenceMedium, 12, 5)
}

// An outage of many intervals (node off, observers down) is one long gap.
// The median ignores it; a mean would not.
func TestEstimateAdvertInterval_LongOutage(t *testing.T) {
	s := aiSeries(12*time.Hour, 0, 1, 2, 3, 4, 5, 6, 7, 8, 30)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceHigh, 10, 8)
}

// A gap of up to 4x the interval is missed adverts (three in a row); a
// longer one is an outage and irregular.
func TestEstimateAdvertInterval_MultipleLimit(t *testing.T) {
	// 12 h with one 4x gap: all 8 gaps fit.
	s := aiSeries(12*time.Hour, 0, 1, 2, 3, 7, 8, 9, 10, 11)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceHigh, 9, 8)
	// A 5x gap instead: 7 of the 8 gaps fit.
	s = aiSeries(12*time.Hour, 0, 1, 2, 3, 8, 9, 10, 11, 12)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceHigh, 9, 7)
}

// A raised setting: the new interval is 2-4x the old one, so the old one
// explains every new gap as missed adverts. A run of gaps at the same
// multiple is the setting, not adverts missed in a row: once the newest 3
// are, the estimate uses the adverts since the newest gap at the old
// interval. Setting either interval re-arms its timer at once
// (src/helpers/CommonCLI.cpp:491-492, 500-501), so the gap at the change is
// between the new interval and the new plus the old one.
func TestEstimateAdvertInterval_RaisedInterval(t *testing.T) {
	H, M := time.Hour, time.Minute
	series := func(parts ...[]time.Duration) []advertIntervalSample {
		var gaps []time.Duration
		for _, p := range parts {
			gaps = append(gaps, p...)
		}
		return aiGaps(gaps...)
	}
	cases := []struct {
		name     string
		class    advertIntervalClass
		s        []advertIntervalSample
		interval int64
		conf     string
		samples  int
		gaps     int
	}{
		// 10 adverts 12 h apart, then 10 more 24 h apart.
		{"flood 12 h -> 24 h", advertIntervalFlood, series(aiRepeat(12*H, 9), aiRepeat(24*H, 10)), 24 * 3600, advertConfidenceHigh, 11, 10},
		{"flood 12 h -> 47 h", advertIntervalFlood, series(aiRepeat(12*H, 9), aiRepeat(47*H, 10)), 47 * 3600, advertConfidenceHigh, 11, 10},
		{"flood 24 h -> 47 h", advertIntervalFlood, series(aiRepeat(24*H, 9), aiRepeat(47*H, 10)), 47 * 3600, advertConfidenceHigh, 11, 10},
		{"zero-hop 60 -> 120 min", advertIntervalZeroHop, series(aiRepeat(60*M, 9), aiRepeat(120*M, 10)), 120 * 60, advertConfidenceHigh, 11, 10},
		{"zero-hop 120 -> 240 min", advertIntervalZeroHop, series(aiRepeat(120*M, 9), aiRepeat(240*M, 10)), 240 * 60, advertConfidenceHigh, 11, 10},
		// The timer re-armed when set: the gap at the change is 24 h + 6 h,
		// irregular for both intervals.
		{"flood 12 h -> 24 h, re-armed when set", advertIntervalFlood, series(aiRepeat(12*H, 9), []time.Duration{30 * H}, aiRepeat(24*H, 9)), 24 * 3600, advertConfidenceHigh, 11, 9},
		// A missed advert (48 h) and a manual one (10 h + 14 h) after the
		// change: the newest 3 gaps that fit 12 h are still all 2x it.
		{"flood 12 h -> 24 h, missed and manual adverts", advertIntervalFlood, series(aiRepeat(12*H, 9), []time.Duration{24 * H, 24 * H, 48 * H, 24 * H, 10 * H, 14 * H, 24 * H, 24 * H}), 24 * 3600, advertConfidenceHigh, 9, 6},
		// Three in a row at 2x: estimated from those three.
		{"flood 12 h -> 24 h, 3 new gaps", advertIntervalFlood, series(aiRepeat(12*H, 16), aiRepeat(24*H, 3)), 24 * 3600, advertConfidenceMedium, 4, 3},
		// Two in a row are as likely two missed adverts: the interval stays.
		{"flood 12 h, last 2 gaps 2x", advertIntervalFlood, series(aiRepeat(12*H, 17), aiRepeat(24*H, 2)), 12 * 3600, advertConfidenceHigh, 20, 19},
		// Different multiples in a row are missed adverts, not a setting.
		{"flood 12 h, last 3 gaps 2x 3x 2x", advertIntervalFlood, series(aiRepeat(12*H, 10), []time.Duration{24 * H, 36 * H, 24 * H}), 12 * 3600, advertConfidenceHigh, 14, 13},
		// Lowered (47 h -> 12 h): 12 h explains the old 47 h gaps as 4x
		// once it is a candidate, as before.
		{"flood 47 h -> 12 h", advertIntervalFlood, series(aiRepeat(47*H, 14), aiRepeat(12*H, 5)), 12 * 3600, advertConfidenceHigh, 20, 19},
		// A gap at the interval itself among the newest 3 that fit breaks
		// the run (review N1 on #247): 24, 12, 24, 24 h is missed adverts
		// on 12 h, not a raised setting since the 12 h gap.
		{"flood 12 h, last 4 gaps 2x 1x 2x 2x", advertIntervalFlood, series(aiRepeat(12*H, 12), []time.Duration{24 * H, 12 * H, 24 * H, 24 * H}), 12 * 3600, advertConfidenceHigh, 17, 16},
		// Raised twice (review N2 on #247): the cut repeats on the adverts
		// since the first change until no run is left, so the estimate is
		// the final setting, not the one in between.
		{"zero-hop 60 -> 120 -> 240 min", advertIntervalZeroHop, series(aiRepeat(60*M, 6), aiRepeat(120*M, 6), aiRepeat(240*M, 6)), 240 * 60, advertConfidenceHigh, 7, 6},
		{"flood 12 h -> 24 h -> 47 h", advertIntervalFlood, series(aiRepeat(12*H, 6), aiRepeat(24*H, 6), aiRepeat(47*H, 6)), 47 * 3600, advertConfidenceHigh, 7, 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			aiCheck(t, estimateAdvertInterval(c.s, c.class), c.interval, c.conf, c.samples, c.gaps)
		})
	}
}

// Candidates are intervals the class's timer can run at. The zero-hop
// timer runs at 2 min (an untouched new install) or 60-240 min, nothing
// between (src/helpers/CommonCLI.cpp:160-165, 496-505), so a
// zero-hop candidate is 2 min or at least 54 min (60 less the tolerance).
// Manual advert.zerohop every 10-30 min is irregular, not "12 min".
func TestEstimateAdvertInterval_TimerCandidates(t *testing.T) {
	M := time.Minute
	manual := aiGaps(12*M, 12*M, 24*M, 36*M, 12*M, 30*M)
	aiCheck(t, estimateAdvertInterval(manual, advertIntervalZeroHop), 0, advertConfidenceNone, 7, 0)
	aiCheck(t, estimateAdvertInterval(aiGaps(aiRepeat(50*M, 8)...), advertIntervalZeroHop), 0, advertConfidenceNone, 9, 0)
	aiCheck(t, estimateAdvertInterval(aiGaps(aiRepeat(2*M, 19)...), advertIntervalZeroHop), 120, advertConfidenceHigh, 20, 19)
	aiCheck(t, estimateAdvertInterval(aiGaps(aiRepeat(55*M, 8)...), advertIntervalZeroHop), 3600, advertConfidenceHigh, 9, 8)
	// Flood stays at 3 h or more: hourly manual flood adverts are irregular.
	aiCheck(t, estimateAdvertInterval(aiGaps(aiRepeat(60*M, 8)...), advertIntervalFlood), 0, advertConfidenceNone, 9, 0)
}

// The zero-hop 2 min candidate band is 120 s +- 10 % (review N5 on #247),
// the same tolerance the snap uses: gaps of 108-132 s give the 2 min
// default, gaps just outside it are no timer interval. The other edges:
// zero-hop 54 min (60 less 10 %), flood 2.7 h (3 h less 10 %).
func TestEstimateAdvertInterval_CandidateBandEdges(t *testing.T) {
	S, M := time.Second, time.Minute
	for _, c := range []struct {
		gap  time.Duration
		want int64
		conf string
		gaps int
	}{
		{108 * S, 120, advertConfidenceHigh, 19},
		{132 * S, 120, advertConfidenceHigh, 19},
		{107 * S, 0, advertConfidenceNone, 0},
		{133 * S, 0, advertConfidenceNone, 0},
	} {
		t.Run(c.gap.String(), func(t *testing.T) {
			aiCheck(t, estimateAdvertInterval(aiGaps(aiRepeat(c.gap, 19)...), advertIntervalZeroHop), c.want, c.conf, 20, c.gaps)
		})
	}
	for _, c := range []struct {
		class advertIntervalClass
		s     float64
		want  bool
	}{
		{advertIntervalZeroHop, 108, true},
		{advertIntervalZeroHop, 132, true},
		{advertIntervalZeroHop, 107, false},
		{advertIntervalZeroHop, 133, false},
		{advertIntervalZeroHop, (54 * M).Seconds(), true},
		{advertIntervalZeroHop, (54 * M).Seconds() - 1, false},
		{advertIntervalFlood, 2.7 * 3600, true},
		{advertIntervalFlood, 2.7*3600 - 1, false},
	} {
		if got := advertIntervalCandidateOK(c.s, c.class); got != c.want {
			t.Errorf("advertIntervalCandidateOK(%v s, class %d) = %v, want %v", c.s, c.class, got, c.want)
		}
	}
}

// Known trade-off of the raised-interval rule (review N3 on #247): with an
// unchanged interval, when the newest 3 heard gaps are the same multiple k
// (every 2nd or 3rd advert lost), the estimate is k x at medium confidence
// on 4 adverts, until the next gap at the interval itself breaks the run.
// docs/api-spec.md documents it next to the sparse-coverage limitation.
func TestEstimateAdvertInterval_FalseChange(t *testing.T) {
	H, M := time.Hour, time.Minute
	cases := []struct {
		name     string
		class    advertIntervalClass
		gaps     []time.Duration
		interval int64
	}{
		{"zero-hop 60 min, last 3 gaps 2x", advertIntervalZeroHop, append(aiRepeat(60*M, 10), aiRepeat(120*M, 3)...), 120 * 60},
		{"flood 47 h, last 3 gaps 2x", advertIntervalFlood, append(aiRepeat(47*H, 10), aiRepeat(94*H, 3)...), 94 * 3600},
		{"zero-hop 120 min, last 3 gaps 3x", advertIntervalZeroHop, append(aiRepeat(120*M, 10), aiRepeat(360*M, 3)...), 360 * 60},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			aiCheck(t, estimateAdvertInterval(aiGaps(c.gaps...), c.class), c.interval, advertConfidenceMedium, 4, 3)
		})
	}
	// A gap that fits no multiple (an outage over 4x) does not break the run.
	s := aiGaps(append(aiRepeat(60*M, 10), 120*M, 120*M, 300*M, 120*M)...)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalZeroHop), 120*60, advertConfidenceMedium, 5, 3)
	// One more gap at the interval itself ends it.
	s = aiGaps(append(append(aiRepeat(60*M, 10), aiRepeat(120*M, 3)...), 60*M)...)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalZeroHop), 3600, advertConfidenceHigh, 15, 14)
}

// Known limitation (review F2 on #247): when coverage is so sparse that the
// interval itself is never heard twice in a row, it is no candidate and a
// multiple of it is estimated. A 47 h flood heard only 2x and 3x apart
// reads as 94 h or 141 h, with the other multiple's gaps irregular.
// Treating gaps that share a divisor as its multiples would instead read a
// 24 h series with a few 36 h gaps as 12 h; this pins the current reading.
func TestEstimateAdvertInterval_SparseCoverage(t *testing.T) {
	H := time.Hour
	s := aiGaps(94*H, 141*H, 94*H, 141*H, 94*H, 141*H, 94*H)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 94*3600, advertConfidenceMedium, 8, 4)
	s = aiGaps(94*H, 141*H, 47*H, 94*H, 141*H, 94*H, 141*H)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 141*3600, advertConfidenceMedium, 8, 3)
}

// A candidate must be seen directly in a quarter of the gaps, not only
// twice. Two manual adverts that each split a 12 h gap into 4 h + 8 h make
// 4 h a gap seen twice that explains every gap (4, 8 and 12 h are 1-3x
// 4 h), but 2 of 10 gaps is under a quarter, so 12 h stays.
func TestEstimateAdvertInterval_CandidateQuarter(t *testing.T) {
	s := aiSeries(12*time.Hour, 0, 1, 4.0/3, 2, 3, 4, 13.0/3, 5, 6, 7, 8)
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceHigh, 11, 6)
}

// Sender clock: preferred when plausible, first_seen otherwise.
func TestEstimateAdvertInterval_SenderClock(t *testing.T) {
	// first_seen jitters by up to 20 min (late uploads); the sender's
	// clock is exact. The raw median is then exactly the sender's interval.
	jitter := []time.Duration{0, 20 * time.Minute, -15 * time.Minute, 5 * time.Minute, 18 * time.Minute, -20 * time.Minute, 0, 10 * time.Minute}
	steady := func(offset time.Duration) []advertIntervalSample {
		var s []advertIntervalSample
		for i, j := range jitter {
			sent := aiBase.Add(time.Duration(i) * 12 * time.Hour)
			s = append(s, advertIntervalSample{senderTS: sent.Add(offset).Unix(), heard: sent.Add(j)})
		}
		return s
	}
	for _, offset := range []time.Duration{0, -60 * 24 * time.Hour, -2 * 365 * 24 * time.Hour} {
		got := estimateAdvertInterval(steady(offset), advertIntervalFlood)
		if got.RawIntervalS == nil || *got.RawIntervalS != 12*3600 {
			t.Fatalf("offset %v: raw_interval_s = %v, want exactly 43200 from the sender clock", offset, ptrStr(got.RawIntervalS))
		}
	}

	// A clock ahead of when the advert was heard is clearly wrong: the
	// sender's gaps (exactly 12 h) are not used, first_seen's (12 h 1 min) are.
	var ahead []advertIntervalSample
	for i := 0; i < 8; i++ {
		heard := aiBase.Add(time.Duration(i) * (12*time.Hour + time.Minute))
		ahead = append(ahead, advertIntervalSample{senderTS: aiBase.Add(24*time.Hour + time.Duration(i)*12*time.Hour).Unix(), heard: heard})
	}
	got := estimateAdvertInterval(ahead, advertIntervalFlood)
	if got.RawIntervalS == nil || *got.RawIntervalS != 12*3600+60 {
		t.Fatalf("clock ahead: raw_interval_s = %v, want 43260 from first_seen", ptrStr(got.RawIntervalS))
	}
	aiCheck(t, got, 12*3600, advertConfidenceHigh, 8, 7)

	// clkreboot (src/helpers/CommonCLI.cpp:187-190) sets the clock back to
	// 15 May 2024 mid-series: one gap is negative on the sender's clock and
	// falls back to first_seen, the rest still use the sender's clock.
	s := aiSeries(12*time.Hour, aiInts(0, 9)...)
	for i := 5; i < len(s); i++ {
		s[i].senderTS = 1715770351 + int64(i-5)*12*3600
	}
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceHigh, 10, 9)

	// A forward jump (clock sync after running behind): the sender's gap is
	// days longer than first_seen's, so first_seen wins for that gap.
	s = aiSeries(12*time.Hour, aiInts(0, 9)...)
	for i := 4; i < len(s); i++ {
		s[i].senderTS += 9 * 24 * 3600
	}
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceHigh, 10, 9)

	// No sender timestamp at all (decoded_json without one): first_seen.
	s = aiSeries(12*time.Hour, aiInts(0, 5)...)
	for i := range s {
		s[i].senderTS = 0
	}
	aiCheck(t, estimateAdvertInterval(s, advertIntervalFlood), 12*3600, advertConfidenceMedium, 6, 5)
}

func TestEstimateAdvertInterval_TooFewSamples(t *testing.T) {
	for n := 0; n <= 2; n++ {
		got := estimateAdvertInterval(aiSeries(12*time.Hour, aiInts(0, n-1)...), advertIntervalFlood)
		aiCheck(t, got, 0, advertConfidenceNone, n, 0)
		if got.RawIntervalS != nil || got.Snapped {
			t.Fatalf("n=%d: raw/snapped = %v/%v", n, ptrStr(got.RawIntervalS), got.Snapped)
		}
		if (n == 0) != (got.LastAdvert == nil) {
			t.Fatalf("n=%d: last_advert = %v", n, got.LastAdvert)
		}
	}
	// Three adverts are the minimum: two fitting gaps, low confidence.
	aiCheck(t, estimateAdvertInterval(aiSeries(12*time.Hour, 0, 1, 2), advertIntervalFlood), 12*3600, advertConfidenceLow, 3, 2)
	// Same timestamps twice (no positive gap): nothing to estimate from.
	dup := aiSeries(12*time.Hour, 0, 0, 0)
	aiCheck(t, estimateAdvertInterval(dup, advertIntervalFlood), 0, advertConfidenceNone, 3, 0)
}

// status tells the UI why there is no estimate, so it does not repeat the
// minimum of 3 adverts (review F6 on #247).
func TestEstimateAdvertInterval_Status(t *testing.T) {
	cases := []struct {
		at   []float64
		want string
	}{
		{nil, advertIntervalNoneObserved},
		{[]float64{0}, advertIntervalTooFew},
		{[]float64{0, 1}, advertIntervalTooFew},
		{[]float64{0, 0, 0}, advertIntervalIrregular},
		{[]float64{0, 10, 47, 50, 121, 143}, advertIntervalIrregular},
		{[]float64{0, 1, 2}, advertIntervalEstimated},
	}
	for _, c := range cases {
		if got := estimateAdvertInterval(aiSeries(time.Hour*12, c.at...), advertIntervalFlood); got.Status != c.want {
			t.Errorf("%v: status = %q, want %q (%+v)", c.at, got.Status, c.want, got)
		}
	}
}

// Irregular adverts (a companion advertising by hand) have no interval.
func TestEstimateAdvertInterval_Irregular(t *testing.T) {
	s := aiSeries(time.Hour, 0, 10, 47, 50, 121, 143)
	got := estimateAdvertInterval(s, advertIntervalFlood)
	if got.Confidence != advertConfidenceNone || got.IntervalS != nil || got.Samples != 6 {
		t.Fatalf("irregular: %+v", got)
	}
}

// Confidence: high needs >= 6 fitting gaps and >= 75 % of the non-short
// gaps fitting, medium >= 3 and >= 50 %, low anything less. Short gaps
// (extra adverts) are dropped and count neither way.
func TestEstimateAdvertInterval_ConfidenceTiers(t *testing.T) {
	cases := []struct {
		at   []float64
		conf string
		gaps int
	}{
		{aiInts(0, 6), advertConfidenceHigh, 6},                                    // 6 of 6 fit
		{aiInts(0, 5), advertConfidenceMedium, 5},                                  // 5 fit: below the 6 for high
		{[]float64{0, 1, 2, 3, 4, 5, 6, 7.5, 10, 13.5}, advertConfidenceMedium, 6}, // 6 fit, 3 irregular (67 %)
		{[]float64{0, 1, 2, 3, 3.4, 3.6}, advertConfidenceMedium, 3},               // 3 fit, 2 short gaps dropped
		{[]float64{0, 1, 2, 3, 4.5, 7, 10.5, 11.8}, advertConfidenceLow, 3},        // 3 fit, 4 irregular (43 %)
		{[]float64{0, 1, 2, 2.3, 2.5, 2.7, 2.85}, advertConfidenceLow, 2},          // 2 fit
	}
	for i, c := range cases {
		got := estimateAdvertInterval(aiSeries(12*time.Hour, c.at...), advertIntervalFlood)
		if got.Confidence != c.conf || got.GapsUsed != c.gaps {
			t.Errorf("case %d: confidence/gaps_used = %s/%d, want %s/%d (%+v)", i, got.Confidence, got.GapsUsed, c.conf, c.gaps, got)
		}
		if got.IntervalS == nil || *got.IntervalS != 12*3600 {
			t.Errorf("case %d: interval_s = %v", i, ptrStr(got.IntervalS))
		}
	}
}

// The interval is the median of gap/k over the fitting gaps, not their mean: one late
// first_seen (no sender clock) moves a mean, not the median.
func TestEstimateAdvertInterval_MedianNotMean(t *testing.T) {
	s := aiSeries(12*time.Hour, aiInts(0, 7)...)
	s[7].heard = s[7].heard.Add(time.Hour)
	for i := range s {
		s[i].senderTS = 0
	}
	got := estimateAdvertInterval(s, advertIntervalFlood)
	aiCheck(t, got, 12*3600, advertConfidenceHigh, 8, 7)
	if got.RawIntervalS == nil || *got.RawIntervalS != 12*3600 {
		t.Fatalf("raw_interval_s = %v, want the median 43200", ptrStr(got.RawIntervalS))
	}
}

// Snapping follows the firmware's settable values (see the file comment).
func TestSnapAdvertInterval(t *testing.T) {
	h, m := 3600.0, 60.0
	cases := []struct {
		class   advertIntervalClass
		raw     float64
		want    int64
		snapped bool
	}{
		{advertIntervalFlood, 47.2 * h, 47 * 3600, true},
		{advertIntervalFlood, 46.6 * h, 47 * 3600, true},
		{advertIntervalFlood, 12*h + 95, 12 * 3600, true},
		{advertIntervalFlood, 3 * h, 3 * 3600, true},
		{advertIntervalFlood, 2.8 * h, 3 * 3600, true}, // within 10 % below the 3 h minimum
		{advertIntervalFlood, 2.5 * h, 9000, false},    // below 3 h: no settable value
		{advertIntervalFlood, 168.4 * h, 168 * 3600, true},
		{advertIntervalFlood, 180 * h, 168 * 3600, true}, // within 10 % above the 168 h maximum
		{advertIntervalFlood, 200 * h, 720000, false},
		{advertIntervalZeroHop, 120.9 * m, 120 * 60, true}, // even minutes, not whole ones
		{advertIntervalZeroHop, 121.5 * m, 122 * 60, true},
		{advertIntervalZeroHop, 60.4 * m, 60 * 60, true},
		{advertIntervalZeroHop, 56 * m, 60 * 60, true}, // within 10 % below 60 min
		{advertIntervalZeroHop, 50 * m, 3000, false},   // 50 min cannot be set
		{advertIntervalZeroHop, 239 * m, 240 * 60, true},
		{advertIntervalZeroHop, 260 * m, 240 * 60, true}, // within 10 % above 240 min
		{advertIntervalZeroHop, 270 * m, 16200, false},
		{advertIntervalZeroHop, 125, 120, true}, // the 2 min new-install default
		{advertIntervalZeroHop, 10 * m, 600, false},
		{advertIntervalZeroHop, 12 * h, 43200, false}, // a flood-sized interval is not a zero-hop setting
	}
	for _, c := range cases {
		got, snapped := snapAdvertInterval(c.raw, c.class)
		if got != c.want || snapped != c.snapped {
			t.Errorf("snap(%v, class %d) = %d/%v, want %d/%v", c.raw, c.class, got, snapped, c.want, c.snapped)
		}
	}
}

// aiRows builds NodeAdvertRows the way GetNodeAdvertRoutes lists them
// (newest ingest first), each with decoded_json carrying the sender clock.
func aiRows(interval time.Duration, n int, newest time.Time) NodeAdvertRows {
	rows := NodeAdvertRows{}
	for i := 0; i < n; i++ {
		heard := newest.Add(-time.Duration(i) * interval)
		rows = append(rows, NodeAdvertRow{
			"first_seen":   heard.Format(time.RFC3339),
			"decoded_json": fmt.Sprintf(`{"type":"ADVERT","timestamp":%d}`, heard.Unix()-1),
		})
	}
	return rows
}

// Each class is estimated from its own list; mixed and unknown adverts are
// left out (their class is ambiguous). No zero-hop adverts: none, with no
// last_advert.
func TestNodeAdvertIntervals_PerClass(t *testing.T) {
	newest := aiBase.Add(240 * time.Hour)
	byRoute := NodeAdvertsByRoute{
		Limit:   nodeAdvertRouteLimit,
		Flood:   aiRows(12*time.Hour, 10, newest),
		ZeroHop: aiRows(2*time.Hour, 8, newest.Add(-time.Hour)),
		Mixed:   aiRows(time.Hour, 10, newest),
		Unknown: aiRows(3*time.Hour, 10, newest),
	}
	got := nodeAdvertIntervals(byRoute)
	if got.Window != nodeAdvertRouteLimit {
		t.Fatalf("window = %d", got.Window)
	}
	aiCheck(t, got.Flood, 12*3600, advertConfidenceHigh, 10, 9)
	aiCheck(t, got.ZeroHop, 7200, advertConfidenceHigh, 8, 7)
	if !got.Flood.Snapped || !got.ZeroHop.Snapped {
		t.Fatalf("snapped flood/zero_hop = %v/%v: each class snaps to its own setting", got.Flood.Snapped, got.ZeroHop.Snapped)
	}
	if got.ZeroHop.LastAdvert == nil || *got.ZeroHop.LastAdvert != newest.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("zero_hop last_advert = %v", got.ZeroHop.LastAdvert)
	}

	byRoute.ZeroHop = NodeAdvertRows{}
	got = nodeAdvertIntervals(byRoute)
	aiCheck(t, got.ZeroHop, 0, advertConfidenceNone, 0, 0)
	if got.ZeroHop.LastAdvert != nil {
		t.Fatalf("no zero-hop adverts: last_advert = %v", *got.ZeroHop.LastAdvert)
	}
	aiCheck(t, got.Flood, 12*3600, advertConfidenceHigh, 10, 9)
}

// Rows the estimator cannot use: no parseable first_seen is skipped; a
// missing or malformed sender timestamp falls back to first_seen.
func TestAdvertIntervalSamples_Parsing(t *testing.T) {
	rows := NodeAdvertRows{
		{"first_seen": "2026-09-01T12:00:00Z", "decoded_json": `{"timestamp":1788264000}`},
		{"first_seen": "2026-09-01T00:00:00.000Z", "decoded_json": `{"timestamp":"x"}`},
		{"first_seen": "2026-08-31T12:00:00Z", "decoded_json": nil},
		{"first_seen": "garbage", "decoded_json": `{"timestamp":1}`},
		{"first_seen": nil},
	}
	s := advertIntervalSamples(rows)
	if len(s) != 3 {
		t.Fatalf("samples = %d, want 3: %+v", len(s), s)
	}
	if s[0].senderTS != 1788264000 || s[1].senderTS != 0 || s[2].senderTS != 0 {
		t.Fatalf("sender timestamps = %d/%d/%d", s[0].senderTS, s[1].senderTS, s[2].senderTS)
	}
	if !s[1].heard.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("heard = %v", s[1].heard)
	}
}

// API: advertIntervals rides on the include=advertRoutes opt-in, flood from
// the flood list and zero-hop from the zero-hop list.
func TestNodeDetail_AdvertIntervals(t *testing.T) {
	srv, router := setupTestServer(t)
	narAddRouteMask(t, srv.db)
	const pk = "245aa00000000000000000000000000000000000000000000000000000000001"
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen) VALUES (?, 'Interval Node', 'repeater', ?, ?)`,
		pk, narAgo(time.Hour), narAgo(200*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	insert := func(hash string, routeType, mask int, heard time.Time) {
		if _, err := srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey, route_mask)
			VALUES ('1100', ?, ?, ?, 4, ?, ?, ?)`, hash, heard.Format(time.RFC3339), routeType, fmt.Sprintf(`{"type":"ADVERT","timestamp":%d}`, heard.Unix()-3), pk, mask); err != nil {
			t.Fatal(err)
		}
	}
	// Flood every 12 h with one missed; zero-hop every 120 min.
	for _, k := range []int{1, 2, 3, 5, 6, 7, 8} {
		insert(fmt.Sprintf("f%d", k), 1, 0b0010, now.Add(-time.Duration(k)*12*time.Hour))
	}
	for k := 0; k < 6; k++ {
		insert(fmt.Sprintf("z%d", k), 2, 0b0100, now.Add(-time.Duration(30+120*k)*time.Minute))
	}

	req := func(q string) string {
		_, raw := narGetNode(t, router, pk+q, 200)
		return raw
	}
	if raw := req(""); strings.Contains(raw, "advertIntervals") {
		t.Fatalf("advertIntervals without the opt-in: %s", raw)
	}
	var body struct {
		Intervals *NodeAdvertIntervals `json:"advertIntervals"`
	}
	if err := json.Unmarshal([]byte(req(narIncludeQuery)), &body); err != nil {
		t.Fatal(err)
	}
	if body.Intervals == nil {
		t.Fatal("advertIntervals missing with include=advertRoutes")
	}
	aiCheck(t, body.Intervals.Flood, 12*3600, advertConfidenceHigh, 7, 6)
	aiCheck(t, body.Intervals.ZeroHop, 7200, advertConfidenceMedium, 6, 5)
	if body.Intervals.Flood.Status != advertIntervalEstimated || body.Intervals.ZeroHop.Status != advertIntervalEstimated {
		t.Fatalf("status flood/zero_hop = %q/%q, want estimated", body.Intervals.Flood.Status, body.Intervals.ZeroHop.Status)
	}
	if body.Intervals.Window != nodeAdvertRouteLimit {
		t.Fatalf("window = %d", body.Intervals.Window)
	}
}

