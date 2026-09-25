package anomaly

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Numerical stability of the periodic null model and the canonical JSON of
// PeriodicEvidence.Chance. Chance comes from Exp/Expm1/Pow, whose last bits
// differ between CPU architectures; these tests pin what must not differ.

// stateWithRate returns a pulse ring of n pulses spread evenly over span, so
// chance() sees the local rate (n-1)/span.
func stateWithRate(n int, span time.Duration, evaluated uint64) *periodicState {
	p := newPeriodicState(maxHistoryLen)
	for i := 0; i < n; i++ {
		p.times[i] = t0.UnixNano() + int64(span)*int64(i)/int64(n-1)
	}
	p.n, p.evaluated = n, evaluated
	return p
}

// directChance is the textbook form with the subtraction
// exp(-l*lo) - exp(-l*hi): accurate only when the tolerance is not small.
func directChance(p *periodicState, c chainInfo, r *PeriodicRule) float64 {
	lambda := float64(p.n-1) / float64(p.at(p.n-1)-p.at(0))
	var pg float64
	for k := 1; k <= r.MaxMissing+1; k++ {
		lo := math.Max(0, float64(int64(k)*c.period-c.tol))
		hi := float64(int64(k)*c.period + c.tol)
		pg += math.Exp(-lambda*lo) - math.Exp(-lambda*hi)
	}
	pg = math.Min(pg, 1)
	return float64(p.evaluated) * float64(searchCells(r)) * math.Pow(pg, float64(c.gaps-1))
}

func relDiff(a, b float64) float64 {
	if a == b {
		return 0
	}
	return math.Abs(a-b) / math.Max(math.Abs(a), math.Abs(b))
}

// Where the direct form is well conditioned, the expm1 form gives the same
// value (to within rounding).
func TestChanceExpm1FormMatchesTheDirectFormWhereWellConditioned(t *testing.T) {
	period := int64(5 * time.Minute)
	for _, rateTimesP := range []float64{0.2, 0.5, 1, 2, 5} {
		span := time.Duration(float64(30*period) / rateTimesP)
		p := stateWithRate(31, span, 1000)
		for _, tolFrac := range []float64{0.01, 0.05, 0.1, 0.25} {
			for missing := 0; missing <= 3; missing++ {
				for _, gaps := range []int{3, 10, 30} {
					r := PeriodicRule{MaxMissing: missing}
					c := chainInfo{period: period, tol: int64(tolFrac * float64(period)), gaps: gaps}
					got, want := p.chance(c, &r), directChance(p, c, &r)
					if d := relDiff(got, want); d > 1e-12 {
						t.Fatalf("lambda*P=%g tol=%g missing=%d gaps=%d: expm1 form %g, direct %g (rel %g)",
							rateTimesP, tolFrac, missing, gaps, got, want, d)
					}
				}
			}
		}
	}
}

// For small (valid) tolerances the probability of one gap falling in
// [lo, hi] is exp(-l*lo) * x * (1 - x/2 + x^2/6 - ...) with x = l*(hi-lo).
// The expm1 form must stay at full precision there, even after Pow raises it
// to the longest chain; the direct form does not.
func TestChanceIsAccurateForSmallTolerances(t *testing.T) {
	period := int64(time.Hour)
	p := stateWithRate(2, time.Duration(period), 1) // local rate 1/P
	lambda := 1 / float64(period)
	r := PeriodicRule{MaxMissing: 0}
	cells := float64(searchCells(&r))
	for _, x := range []float64{1e-12, 1e-9, 1e-6, 1e-4} {
		tol := int64(x / lambda / 2)
		if tol < 1 {
			tol = 1
		}
		xa := lambda * float64(2*tol) // the exact width after integer rounding
		lo := float64(period - tol)
		pgRef := math.Exp(-lambda*lo) * xa * (1 - xa/2 + xa*xa/6 - xa*xa*xa/24)
		// the longest chain whose chance stays a normal float: Pow amplifies
		// the relative error of pg by the exponent
		longest := 1 + int(250/-math.Log10(pgRef))
		if longest > maxHistoryLen-1 {
			longest = maxHistoryLen - 1
		}
		for _, gaps := range []int{2, 3, longest} {
			c := chainInfo{period: period, tol: tol, gaps: gaps}
			want := cells * math.Pow(pgRef, float64(gaps-1))
			if !(want >= 1e-250) {
				t.Fatalf("x=%g gaps=%d: reference %g left the normal range", x, gaps, want)
			}
			if d := relDiff(p.chance(c, &r), want); d > 1e-12 {
				t.Fatalf("x=%g gaps=%d: chance off by %g relative", x, gaps, d)
			}
		}
	}
	// the oracle is sharp enough to see the cancellation it guards against
	c := chainInfo{period: period, tol: int64(1e-9 / lambda / 2), gaps: 2}
	xa := lambda * float64(2*c.tol)
	pgRef := math.Exp(-lambda*float64(period-c.tol)) * xa * (1 - xa/2 + xa*xa/6)
	if d := relDiff(directChance(p, c, &r), cells*pgRef); d < 1e-9 {
		t.Fatalf("direct form unexpectedly accurate (rel %g): the oracle cannot tell the forms apart", d)
	}
}

// The detector decides on the full Chance, not on its rounded JSON form.
// Thresholds just below, at and just above the value give the same decision
// on every architecture: the literals below sit 1e-10 relative away from the
// value, far outside the measured cross-architecture spread (about 5e-13).
func TestMaxChanceDecisionAroundTheBoundary(t *testing.T) {
	const (
		canonical = 4.59387e-08            // the value rounded to ChanceDigits
		justBelow = 4.5938728007910717e-08 // value * (1 - 1e-10)
		justAbove = 4.5938728017098456e-08 // value * (1 + 1e-10)
	)
	signalAt := func(maxChance float64) (PeriodicEvidence, bool) {
		r := experimentalPeriodic()
		r.MaxChance = maxChance
		p := newPeriodicState(r.HistoryLen)
		var sc periodicScratch
		var sig periodicSignal
		var ok bool
		for i := 0; i < r.MinPulses; i++ {
			sig, ok = p.observe(at(time.Duration(i)*5*time.Minute).UnixNano(), TrafficUnclassified, &r, &sc)
		}
		return sig.ev, ok
	}
	ev, ok := signalAt(1)
	if !ok {
		t.Fatal("fixture: no signal at the first sufficient pulse")
	}
	raw := ev.Chance
	if got := canonicalFloat(raw, ChanceDigits); got != canonical {
		t.Fatalf("canonical Chance %v, want %v (raw %.17g)", got, canonical, raw)
	}
	for _, c := range []struct {
		name      string
		maxChance float64
		fires     bool
	}{
		{"just below (1e-10)", justBelow, false},
		{"one ulp below", math.Nextafter(raw, 0), false},
		{"at the value", raw, true},
		{"one ulp above", math.Nextafter(raw, 1), true},
		{"just above (1e-10)", justAbove, true},
	} {
		if _, fired := signalAt(c.maxChance); fired != c.fires {
			t.Errorf("MaxChance %s (%.17g): fired=%v, want %v", c.name, c.maxChance, fired, c.fires)
		}
	}
	// rounding does not leak into the decision: the rounded value lies on one
	// side of raw, and a threshold between them decides by the raw value
	mid := (raw + canonical) / 2
	if _, fired := signalAt(mid); fired != (raw <= mid) {
		t.Errorf("threshold between raw %.17g and canonical %v decided by the rounded value", raw, canonical)
	}
}

func TestCanonicalFloat(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0, 0},
		{1.2345649999999, 1.23456},   // just below a rounding boundary
		{1.2345650000001, 1.23457},   // just above it
		{1234565, 1.23456e6},         // exact binary tie: half to even
		{1234575, 1.23458e6},         // exact binary tie: half to even
		{-1.2345650000001, -1.23457}, // negative
		{-0.000241965270782, -0.000241965},
		{4.593872801250516e-8, 4.59387e-8},
		{4.593872801250437e-8, 4.59387e-8},
		{1e-310, 1e-310}, // subnormal
		{2.459010123456e-164, 2.45901e-164},
		{144867615, 1.44868e8}, // large
		{math.MaxFloat64, 1.79769e308},
	} {
		if got := canonicalFloat(c.in, ChanceDigits); got != c.want {
			t.Errorf("canonicalFloat(%.17g) = %.17g, want %.17g", c.in, got, c.want)
		}
	}
	if got := canonicalFloat(math.Copysign(0, -1), ChanceDigits); !math.Signbit(got) || got != 0 {
		t.Error("negative zero not kept")
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := canonicalFloat(v, ChanceDigits)
		if !(math.IsNaN(v) && math.IsNaN(got)) && got != v {
			t.Errorf("canonicalFloat(%v) = %v", v, got)
		}
	}
}

// Values that differ only in their last bits (as on amd64 vs arm64) give
// byte-identical JSON; every other field is written unchanged.
func TestPeriodicEvidenceJSONIsCanonical(t *testing.T) {
	base := PeriodicEvidence{Period: 5 * time.Minute, Tolerance: 6 * time.Second, JitterMedian: time.Second,
		Support: 8, Events: 9, Coverage: 1.0 / 3, SufficientAt: t0,
		Alternatives: []PeriodAlternative{{Period: 10 * time.Minute, Support: 4, Coverage: 0.5}}}
	for _, pair := range [][2]float64{
		{4.593872801250516e-8, 4.593872801250437e-8},     // the two sides of the CI diff
		{0.00024196527078255232, 0.00024196527078255262}, // likewise
		{1.2345612345678, math.Nextafter(1.2345612345678, 2)},
	} {
		a, b := base, base
		a.Chance, b.Chance = pair[0], pair[1]
		ja, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		jb, _ := json.Marshal(b)
		if !bytes.Equal(ja, jb) {
			t.Fatalf("JSON differs for %.17g and %.17g:\n%s\n%s", pair[0], pair[1], ja, jb)
		}
	}
	e := base
	e.Chance = 4.593872801250516e-8
	got, _ := json.Marshal(e)
	type plain PeriodicEvidence
	p := plain(base)
	p.Chance = 4.59387e-8
	want, _ := json.Marshal(p)
	if !bytes.Equal(got, want) {
		t.Fatalf("only Chance may change:\n got %s\nwant %s", got, want)
	}
	if !bytes.Contains(got, []byte(`"Chance":4.59387e-8`)) {
		t.Fatalf("Chance not written with %d significant digits: %s", ChanceDigits, got)
	}
}

// goldenWithoutChance is the SHA-256 of testdata/replay_golden.json with every
// "Chance" member removed (numbers kept verbatim, keys sorted), taken from
// the golden file before Chance was canonicalized. Timestamps, transitions,
// reason codes, coverage, counters and the number of candidates are therefore
// unchanged; only the documented Chance values changed.
const goldenWithoutChance = "2538014b83e1976a614da59c3db2eeb8d403ba76de63f3f58d55aad221eaf959"

func stripChance(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		delete(x, "Chance")
		for k, c := range x {
			x[k] = stripChance(c)
		}
	case []interface{}:
		for i, c := range x {
			x[i] = stripChance(c)
		}
	}
	return v
}

func collectChance(v interface{}, out *[]string) {
	switch x := v.(type) {
	case map[string]interface{}:
		if c, ok := x["Chance"]; ok {
			*out = append(*out, string(c.(json.Number)))
		}
		for _, c := range x {
			collectChance(c, out)
		}
	case []interface{}:
		for _, c := range x {
			collectChance(c, out)
		}
	}
}

func TestGoldenChangedOnlyInTheDocumentedChanceValues(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "replay_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	decode := func() interface{} {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	var chances []string
	collectChance(decode(), &chances)
	n := map[string]int{}
	for _, c := range chances {
		n[c]++
	}
	if len(chances) != 4 || n["4.59387e-8"] != 2 || n["0.000241965"] != 2 {
		t.Fatalf("golden Chance values %v, want 4.59387e-8 x2 and 0.000241965 x2", chances)
	}
	b, err := json.Marshal(stripChance(decode()))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != goldenWithoutChance {
		t.Fatalf("golden changed outside Chance: sha256 %s, want %s", got, goldenWithoutChance)
	}
}
