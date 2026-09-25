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

// measuredSteps is the total number of gaps visited by the search over the
// deterministic runs of TestPeriodicSearchWorkIsBounded (4*maxHistoryLen
// pulses per shape). The test allows 25% above it, so a change that makes
// the search markedly more expensive fails here even though it stays
// within the loose analytic periodicWorkBound.
var measuredSteps = map[string]uint64{
	"periodic/0.6": 236631, "alternating/0.6": 1434346, "jitter/0.6": 246825,
	"missing/0.6": 243414, "bursty/0.6": 196561, "noise/0.6": 144778,
	"periodic/1": 236631, "alternating/1": 1434346, "jitter/1": 5846059,
	"missing/1": 1174373, "bursty/1": 196561, "noise/1": 144778,
}

// Every single pulse stays within periodicWorkBound, at the largest allowed
// ring and MaxMissing, for every traffic shape, including the configuration
// where no chain ever meets the criteria and the search runs on every pulse;
// and the total work stays within 25% of measuredSteps.
func TestPeriodicSearchWorkIsBounded(t *testing.T) {
	for _, r := range []PeriodicRule{maxPeriodicRule(), worstPeriodicRule()} {
		maxChains, maxSteps := periodicWorkBound(&r)
		for _, shape := range benchShapes {
			f := newPulseFeeder([]PeriodicRule{r}, shape)
			var peakC, peakS uint64
			for i := 0; i < 4*maxHistoryLen; i++ {
				c0, s0 := f.sc.chains, f.sc.steps
				f.feed(1)
				dc, ds := f.sc.chains-c0, f.sc.steps-s0
				if dc > maxChains || ds > maxSteps {
					t.Fatalf("%s MinCoverage=%g: pulse %d did %d chains and %d steps, bound %d and %d",
						shape, r.MinCoverage, i, dc, ds, maxChains, maxSteps)
				}
				peakC, peakS = max(peakC, dc), max(peakS, ds)
			}
			key := fmt.Sprintf("%s/%g", shape, r.MinCoverage)
			if m, ok := measuredSteps[key]; !ok || f.sc.steps > m+m/4 {
				t.Errorf("%s: %d steps in total, measured %d (+25%% allowed)", key, f.sc.steps, m)
			}
			t.Logf("%-11s MinCoverage=%g: peak %d chains (bound %d), %d steps (bound %d), total %d steps",
				shape, r.MinCoverage, peakC, maxChains, peakS, maxSteps, f.sc.steps)
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
