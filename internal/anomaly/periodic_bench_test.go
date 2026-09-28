package anomaly

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Cost of the periodic candidate search per pulse, at the fixture rule and at
// the largest allowed ring and MaxMissing, for the traffic shapes that drive
// the search differently.

func maxPeriodicRule() PeriodicRule {
	r := experimentalPeriodic()
	r.HistoryLen, r.MaxMissing, r.JitterAbs = maxHistoryLen, maxMissingLimit, 6*time.Second
	return r
}

// worstPeriodicRule never lets a chain meet the criteria, so no hypothesis is
// kept and the full search runs on every pulse, over long chains.
func worstPeriodicRule() PeriodicRule {
	r := maxPeriodicRule()
	r.MinCoverage, r.MaxJitterFraction = 1, 0.01
	return r
}

var benchShapes = []string{"periodic", "alternating", "jitter", "missing", "bursty", "noise"}

// pulseTimes returns n increasing pulse times (ns) of one traffic shape.
func pulseTimes(shape string, n int) []int64 {
	rng := rand.New(rand.NewSource(1))
	const p = int64(5 * time.Minute)
	out := make([]int64, 0, n)
	t := t0.UnixNano()
	for len(out) < n {
		i := int64(len(out))
		switch shape {
		case "periodic":
			out = append(out, t+i*p)
		case "alternating":
			d := int64(0)
			if i%2 == 1 {
				d = -int64(6 * time.Second)
			}
			out = append(out, t+i*p+d)
		case "jitter":
			out = append(out, t+i*p+rng.Int63n(int64(6*time.Second))-int64(3*time.Second))
		case "missing":
			if rng.Intn(2) == 0 {
				t += p
			}
			t += p
			out = append(out, t)
		case "bursty":
			t += int64(10*time.Second) + rng.Int63n(int64(10*time.Minute))
			out = append(out, t)
		case "noise":
			t += int64(10*time.Second) + int64(rng.ExpFloat64()*float64(p))
			out = append(out, t)
		default:
			panic(shape)
		}
	}
	return out
}

// pulseFeeder feeds a shape's pulse times into one state per rule, cycling
// through the times with a growing offset so time keeps increasing.
type pulseFeeder struct {
	rules  []PeriodicRule
	states []*periodicState
	times  []int64
	span   int64
	i      int
	sc     periodicScratch
}

func newPulseFeeder(rules []PeriodicRule, shape string) *pulseFeeder {
	f := &pulseFeeder{rules: rules, times: pulseTimes(shape, 4096)}
	for i := range rules {
		f.states = append(f.states, newPeriodicState(rules[i].HistoryLen))
	}
	f.span = f.times[len(f.times)-1] - f.times[0] + int64(time.Hour)
	return f
}

func (f *pulseFeeder) feed(pulses int) {
	for n := 0; n < pulses; n++ {
		t := f.times[f.i%len(f.times)] + int64(f.i/len(f.times))*f.span
		f.i++
		for j := range f.rules {
			f.states[j].observe(t, TrafficUnclassified, &f.rules[j], &f.sc)
		}
	}
}

var benchConfigs = []struct {
	name  string
	rules []PeriodicRule
}{
	{"fixture", []PeriodicRule{experimentalPeriodic()}},
	{"max", []PeriodicRule{maxPeriodicRule()}},
	{"max-4rules", []PeriodicRule{maxPeriodicRule(), maxPeriodicRule(), maxPeriodicRule(), maxPeriodicRule()}},
	{"worst", []PeriodicRule{worstPeriodicRule()}},
}

// periodicWorkBound is the most work one pulse evaluation may do: chain
// measurements for the kept hypothesis and its MaxMissing multiples, at most
// two per seed (the seed's pass and, for a refined cell, its period), the
// final refinement and four alternatives, each visiting at most HistoryLen-1
// gaps; plus one verification walk of recentGaps gaps per seed.
func periodicWorkBound(r *PeriodicRule) (chains, steps uint64) {
	seeds := uint64(recentGaps * (r.MaxMissing + 1))
	chains = uint64(1+r.MaxMissing) + 2*seeds + 1 + 4
	return chains, chains*uint64(r.HistoryLen-1) + seeds*recentGaps
}

// workGuardRules are the rules TestPeriodicSearchWorkIsBounded runs: the
// fixture rule of BenchmarkPeriodicSearch's "fixture/..." cases, and the
// largest allowed ring and MaxMissing ("max"), also where no chain ever meets
// the criteria ("worst").
var workGuardRules = []struct {
	name string
	rule PeriodicRule
}{
	{"fixture", experimentalPeriodic()},
	{"max", maxPeriodicRule()},
	{"worst", worstPeriodicRule()},
}

// measuredSteps is the total number of gaps visited by the search over the
// deterministic runs of TestPeriodicSearchWorkIsBounded (4*maxHistoryLen
// pulses per rule and shape). The test allows 25% either way. More means the
// search visits markedly more chains or gaps, even though it may stay within
// the loose analytic periodicWorkBound. Less means it visits markedly fewer:
// lost search coverage (a range or seed dropped too early, say), unless it
// is a deliberate optimization, which then updates these values.
//
// The counters count visited gaps only. Work per visited gap is not caught:
// removing the periodRange memo, for example, leaves every count unchanged
// (TestRangeMemoIsNeverStale covers the memo), and only
// BenchmarkPeriodicSearch measures the CPU.
var measuredSteps = map[string]uint64{
	"fixture/periodic": 33289, "fixture/alternating": 104271, "fixture/jitter": 54979,
	"fixture/missing": 34850, "fixture/bursty": 82025, "fixture/noise": 67588,
	"max/periodic": 236631, "max/alternating": 1466835, "max/jitter": 247953,
	"max/missing": 243414, "max/bursty": 199882, "max/noise": 147811,
	"worst/periodic": 236631, "worst/alternating": 1466835, "worst/jitter": 6434483,
	"worst/missing": 1174373, "worst/bursty": 199882, "worst/noise": 147811,
}

// Every single pulse stays within periodicWorkBound, for every guard rule and
// traffic shape, including the configuration where no chain ever meets the
// criteria and the search runs on every pulse; and the total work stays
// within 25% of measuredSteps, either way.
func TestPeriodicSearchWorkIsBounded(t *testing.T) {
	for _, g := range workGuardRules {
		r := g.rule
		maxChains, maxSteps := periodicWorkBound(&r)
		for _, shape := range benchShapes {
			key := g.name + "/" + shape
			f := newPulseFeeder([]PeriodicRule{r}, shape)
			var peakC, peakS uint64
			for i := 0; i < 4*maxHistoryLen; i++ {
				c0, s0 := f.sc.chains, f.sc.steps
				f.feed(1)
				dc, ds := f.sc.chains-c0, f.sc.steps-s0
				if dc > maxChains || ds > maxSteps {
					t.Fatalf("%s: pulse %d did %d chains and %d steps, bound %d and %d", key, i, dc, ds, maxChains, maxSteps)
				}
				peakC, peakS = max(peakC, dc), max(peakS, ds)
			}
			switch m, ok := measuredSteps[key]; {
			case !ok:
				t.Errorf("%s: %d steps in total, no measuredSteps entry", key, f.sc.steps)
			case f.sc.steps > m+m/4:
				t.Errorf("%s: %d steps in total, more than measured %d +25%%: the search does markedly more work", key, f.sc.steps, m)
			case f.sc.steps < m-m/4:
				t.Errorf("%s: %d steps in total, less than measured %d -25%%: the search visits markedly fewer gaps, which means lost search coverage; if this is a deliberate optimization, update measuredSteps", key, f.sc.steps, m)
			}
			t.Logf("%-19s peak %d chains (bound %d), %d steps (bound %d), total %d steps",
				key, peakC, maxChains, peakS, maxSteps, f.sc.steps)
		}
	}
}

func BenchmarkPeriodicSearch(b *testing.B) {
	for _, cfg := range benchConfigs {
		for _, shape := range benchShapes {
			b.Run(fmt.Sprintf("%s/%s", cfg.name, shape), func(b *testing.B) {
				f := newPulseFeeder(cfg.rules, shape)
				f.feed(2 * maxHistoryLen) // full rings and grown scratch buffers
				c0, s0 := f.sc.chains, f.sc.steps
				b.ReportAllocs()
				b.ResetTimer()
				f.feed(b.N)
				b.StopTimer()
				n := float64(b.N * len(cfg.rules))
				b.ReportMetric(float64(f.sc.chains-c0)/n, "chains/pulse")
				b.ReportMetric(float64(f.sc.steps-s0)/n, "steps/pulse")
			})
		}
	}
}
