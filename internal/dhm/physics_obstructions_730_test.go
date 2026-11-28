package dhm

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
)

// #730 M2 — Link.Obstructions: the samples that decide a verdict, so the
// terrain profile can tell whether a building or trees stand there.

// lowLink is flatLink over ground so low that only the obstacles placed
// on it matter; obstacle sets sample i at clearance c below the line of
// sight (negative: above it), in metres.
func lowLink() (Link, []float32, func(s []float32, i int, c float64)) {
	l, s := flatLink()
	for i := range s {
		s[i] = -500
	}
	s[0], s[len(s)-1] = 0, 0
	step := l.DistanceM / float64(len(s)-1)
	return l, s, func(s []float32, i int, c float64) {
		d1 := float64(i) * step
		s[i] = float32(10 - EarthBulge(d1, l.DistanceM-d1, l.K) - c)
	}
}

func TestObstructions(t *testing.T) {
	l, base, obstacle := lowLink()
	n := len(base)
	r := func(i int) float64 { // F1 radius at sample i
		d1 := float64(i) * 20
		return FresnelRadius(l.LambdaM, d1, l.DistanceM-d1)
	}
	for _, c := range []struct {
		name      string
		obstacles map[int]float64 // sample → clearance as a share of F1 there
		ends      [2]float32      // surface at A and B
		state     VerdictState
		want      []int
	}{
		{"nothing in the zone", nil, [2]float32{0, 0}, VerdictClear, nil},
		{"one tree in F1", map[int]float64{300: 0.8}, [2]float32{0, 0}, VerdictFresnel100, []int{300}},
		{"two in F1, one deeper", map[int]float64{300: 0.8, 700: 0.9}, [2]float32{0, 0}, VerdictFresnel100, []int{300, 700}},
		{"below 60 % beats F1", map[int]float64{300: 0.8, 400: 0.3}, [2]float32{0, 0}, VerdictFresnel60, []int{400}},
		{"through the line of sight", map[int]float64{200: -0.01, 300: -0.05, 400: 0.3}, [2]float32{0, 0}, VerdictBlocked, []int{200, 300}},
		{"surface above antenna A", nil, [2]float32{12, 0}, VerdictBlocked, []int{0}},
		{"above both antennas and the line", map[int]float64{250: -0.1}, [2]float32{12, 11}, VerdictBlocked, []int{0, 250, n - 1}},
	} {
		s := append([]float32(nil), base...)
		for i, share := range c.obstacles {
			obstacle(s, i, share*r(i))
		}
		s[0], s[n-1] = c.ends[0], c.ends[1]
		a := l.Assess(s)
		if a.State != c.state {
			t.Fatalf("%s: state %q, want %q", c.name, a.State, c.state)
		}
		if got := l.Obstructions(s, a); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: obstructions %v, want %v", c.name, got, c.want)
		}
	}

	// A path without a verdict has none.
	s := append([]float32(nil), base...)
	obstacle(s, 300, -1)
	s[500] = float32(math.NaN())
	if a := l.Assess(s); a.State != VerdictUnknown || l.Obstructions(s, a) != nil {
		t.Fatalf("unknown path: %+v, obstructions %v", a, l.Obstructions(s, a))
	}
}

// TestObstructionsHoldTheWorstPoint: on random paths, the worst point is
// always among the obstructions, every obstruction is in the verdict's
// class, and no other sample is.
func TestObstructionsHoldTheWorstPoint(t *testing.T) {
	rng := rand.New(rand.NewSource(730))
	l, _ := flatLink()
	n := 1001
	step := l.DistanceM / float64(n-1)
	seen := map[VerdictState]int{}
	for trial := 0; trial < 400; trial++ {
		// A floor far below the zone, with a few spikes up to a random
		// top: the top decides how far into the zone the worst reaches.
		s := make([]float32, n)
		top := -60 + rng.Float64()*75
		for i := range s {
			s[i] = float32(-80 + rng.Float64()*4)
			if rng.Intn(50) == 0 {
				s[i] = float32(top - rng.Float64()*20)
			}
		}
		if rng.Intn(5) == 0 {
			s[0] = 11 // above antenna A
		}
		a := l.Assess(s)
		seen[a.State]++
		if a.AntennaBelowA {
			seen["antenna below A"]++
		}
		got := l.Obstructions(s, a)
		if a.State == VerdictClear || a.State == VerdictUnknown {
			if got != nil {
				t.Fatalf("trial %d: %s path with obstructions %v", trial, a.State, got)
			}
			continue
		}
		in := map[int]bool{}
		for _, i := range got {
			in[i] = true
		}
		if !in[a.WorstIndex] {
			t.Fatalf("trial %d: worst point %d not among %v", trial, a.WorstIndex, got)
		}
		for i := 1; i < n-1; i++ {
			_, _, ratio := l.clearanceAt(float64(s[i]), i, step)
			if (ClassifyClearance(ratio) == a.State) != in[i] {
				t.Fatalf("trial %d: sample %d in class %s, listed %v", trial, i, ClassifyClearance(ratio), in[i])
			}
		}
		if in[0] != a.AntennaBelowA || in[n-1] != a.AntennaBelowB {
			t.Fatalf("trial %d: ends listed %v %v, antenna below %v %v", trial, in[0], in[n-1], a.AntennaBelowA, a.AntennaBelowB)
		}
	}
	for _, st := range []VerdictState{VerdictClear, VerdictFresnel100, VerdictFresnel60, VerdictBlocked, "antenna below A"} {
		if seen[st] == 0 {
			t.Errorf("no random path came out %s: %v", st, seen)
		}
	}
}
