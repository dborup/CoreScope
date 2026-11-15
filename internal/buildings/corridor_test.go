package buildings

import (
	"errors"
	"math"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// dokk1 is a point on the Dokk1 library in Aarhus C, the #729 reference
// case, in EPSG:25832 metres.
var dokk1 = [2]float64{575418.0, 6223838.7}

func mustCorridor(t testing.TB, a, b [2]float64) Corridor {
	t.Helper()
	c, err := NewCorridor(a, b, DefaultRadiusM, DefaultSegmentM)
	if err != nil {
		t.Fatalf("NewCorridor(%v, %v): %v", a, b, err)
	}
	return c
}

// TestNewCorridorRejects: every corridor outside the allow-listed shape is
// ErrBadCorridor, so nothing a caller passes can widen the query.
func TestNewCorridorRejects(t *testing.T) {
	ext := dhm.RasterExtent
	nan, inf := math.NaN(), math.Inf(1)
	east := func(m float64) [2]float64 { return [2]float64{dokk1[0] - m, dokk1[1]} }
	for name, tc := range map[string]struct {
		a, b     [2]float64
		radiusM  int
		segmentM int
	}{
		"A west of the extent":  {[2]float64{ext[0] - 1, 6200000}, dokk1, 15, 10000},
		"B east of the extent":  {dokk1, [2]float64{ext[2] + 1, 6200000}, 15, 10000},
		"A south of the extent": {[2]float64{575000, ext[1] - 1}, dokk1, 15, 10000},
		"B north of the extent": {dokk1, [2]float64{575000, ext[3] + 1}, 15, 10000},
		"NaN easting":           {[2]float64{nan, 6200000}, dokk1, 15, 10000},
		"NaN northing":          {dokk1, [2]float64{575000, nan}, 15, 10000},
		"+Inf easting":          {[2]float64{inf, 6200000}, dokk1, 15, 10000},
		"-Inf northing":         {dokk1, [2]float64{575000, -inf}, 15, 10000},
		"zero length":           {dokk1, dokk1, 15, 10000},
		"equal after rounding":  {dokk1, [2]float64{dokk1[0] + 0.04, dokk1[1]}, 15, 10000},
		"one metre too long":    {dokk1, east(MaxCorridorM + 1), 15, 10000},
		"radius 0":              {dokk1, east(1000), 0, 10000},
		"radius 14":             {dokk1, east(1000), 14, 10000},
		"radius 16":             {dokk1, east(1000), 16, 10000},
		"radius 50":             {dokk1, east(1000), 50, 10000},
		"negative radius":       {dokk1, east(1000), -15, 10000},
		"segment 0":             {dokk1, east(1000), 15, 0},
		"segment below 1 km":    {dokk1, east(1000), 15, MinSegmentM - 1},
		"segment above 10 km":   {dokk1, east(1000), 15, MaxSegmentM + 1},
		"negative segment":      {dokk1, east(1000), 15, -10000},
	} {
		if _, err := NewCorridor(tc.a, tc.b, tc.radiusM, tc.segmentM); !errors.Is(err, ErrBadCorridor) {
			t.Errorf("%s: err %v, want ErrBadCorridor", name, err)
		}
	}
	// The limits themselves are inside the shape.
	for name, tc := range map[string]struct{ a, b [2]float64 }{
		"extent corners":  {[2]float64{ext[0], ext[1]}, [2]float64{ext[0] + 50000, ext[1] + 50000}},
		"exactly the max": {dokk1, east(MaxCorridorM)},
		"0.1 m":           {dokk1, east(0.1)},
	} {
		if _, err := NewCorridor(tc.a, tc.b, DefaultRadiusM, DefaultSegmentM); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, r := range AllowedRadiiM {
		if _, err := NewCorridor(dokk1, east(1000), r, DefaultSegmentM); err != nil {
			t.Errorf("allowed radius %d refused: %v", r, err)
		}
	}
}

// TestNewCorridorRoundsToDecimetres: the ends are rounded to 0.1 m, the
// precision the query is written in, before anything is measured.
func TestNewCorridorRoundsToDecimetres(t *testing.T) {
	c := mustCorridor(t, [2]float64{575418.04, 6223838.66}, [2]float64{574537.55, 6224090.549})
	if c.a != [2]float64{575418.0, 6223838.7} || c.b != [2]float64{574537.6, 6224090.5} {
		t.Fatalf("rounded to %v %v", c.a, c.b)
	}
}

// TestMaxCorridorCoversTheLongestProfile: the terrain profile allows
// paths of up to 80 km of ground distance (dhm.MaxRasterSpanM), but UTM 32
// stretches distance away from its central meridian, by about 0.15 % at
// Bornholm. An 80 km profile there is longer than 80 km in EPSG:25832 and
// must still fit.
func TestMaxCorridorCoversTheLongestProfile(t *testing.T) {
	// B on Bornholm near the extent's east edge; A due west, moved until
	// the ground distance is 80 km.
	b := [2]float64{890000, 6125000}
	latB, lonB := dhm.FromUTM32(b[0], b[1])
	lo, hi := 0.0, 5.0 // degrees of longitude west of B
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if dhm.GroundDistance(dhm.LatLon{Lat: latB, Lon: lonB - mid}, dhm.LatLon{Lat: latB, Lon: lonB}) < float64(dhm.MaxRasterSpanM) {
			lo = mid
		} else {
			hi = mid
		}
	}
	ea, na := dhm.ToUTM32(latB, lonB-lo)
	a := [2]float64{ea, na}
	l := math.Hypot(b[0]-a[0], b[1]-a[1])
	if l <= float64(dhm.MaxRasterSpanM) {
		t.Fatalf("an 80 km ground path is %.1f m in UTM; the test no longer shows the stretch", l)
	}
	c, err := NewCorridor(a, b, DefaultRadiusM, DefaultSegmentM)
	if err != nil {
		t.Fatalf("80 km profile at Bornholm (%.1f m in UTM) refused: %v", l, err)
	}
	if n := len(c.Segments()); n != 9 {
		t.Fatalf("%d segments, want 9", n)
	}
}

// TestSegmentsEnumeration cuts N–S, E–W and diagonal corridors into equal
// spans of at most the segment length. Each span's query line is the span
// grown by the radius at both ends, along the track, in whole decimetres.
func TestSegmentsEnumeration(t *testing.T) {
	for name, tc := range map[string]struct {
		a, b [2]float64
		want int
	}{
		"N–S 80 km":                 {[2]float64{560000, 6300000}, [2]float64{560000, 6220000}, 8},
		"E–W 25 km":                 {[2]float64{550000, 6223000}, [2]float64{575000, 6223000}, 3},
		"diagonal 30 km":            {[2]float64{560000, 6200000}, [2]float64{581213.2, 6221213.2}, 3},
		"exactly one segment":       {[2]float64{560000, 6200000}, [2]float64{570000, 6200000}, 1},
		"0.1 m over one segment":    {[2]float64{560000, 6200000}, [2]float64{570000.1, 6200000}, 2},
		"short Dokk1 path, 1 m":     {dokk1, [2]float64{dokk1[0] + 1, dokk1[1]}, 1},
		"reverse diagonal (S→N, W)": {[2]float64{581213.2, 6221213.2}, [2]float64{560000, 6200000}, 3},
		// The query line reaches the radius past the extent's corner.
		"from the extent corner": {[2]float64{dhm.RasterExtent[0], dhm.RasterExtent[1]}, [2]float64{dhm.RasterExtent[0] + 6000, dhm.RasterExtent[1] + 8000}, 1},
		"to the extent corner":   {[2]float64{dhm.RasterExtent[2] - 6000, dhm.RasterExtent[3] - 8000}, [2]float64{dhm.RasterExtent[2], dhm.RasterExtent[3]}, 1},
	} {
		c := mustCorridor(t, tc.a, tc.b)
		segs := c.Segments()
		if len(segs) != tc.want {
			t.Errorf("%s: %d segments, want %d", name, len(segs), tc.want)
			continue
		}
		l := c.LengthM()
		u := [2]float64{(c.b[0] - c.a[0]) / l, (c.b[1] - c.a[1]) / l}
		r := float64(DefaultRadiusM)
		for k, s := range segs {
			if !s.Valid() || s.RadiusM != DefaultRadiusM {
				t.Errorf("%s: segment %d invalid: %+v", name, k, s)
			}
			s0, s1 := c.span(k)
			if s1-s0 > DefaultSegmentM+1e-9 || s1 <= s0 {
				t.Errorf("%s: segment %d spans %.3f..%.3f", name, k, s0, s1)
			}
			if k == 0 && s0 != 0 || k == len(segs)-1 && math.Abs(s1-l) > 1e-6 {
				t.Errorf("%s: spans do not run from A to B", name)
			}
			if k > 0 {
				if p0, _ := c.span(k - 1); p0 >= s0 {
					t.Errorf("%s: segment %d out of order", name, k)
				}
				if _, p1 := c.span(k - 1); p1 != s0 {
					t.Errorf("%s: gap or overlap before segment %d", name, k)
				}
			}
			for i, want := range [2][2]float64{
				{c.a[0] + u[0]*(s0-r), c.a[1] + u[1]*(s0-r)},
				{c.a[0] + u[0]*(s1+r), c.a[1] + u[1]*(s1+r)},
			} {
				got := [2]float64{float64(s.Line[2*i]) / 10, float64(s.Line[2*i+1]) / 10}
				if math.Abs(got[0]-want[0]) > 0.051 || math.Abs(got[1]-want[1]) > 0.051 {
					t.Errorf("%s: segment %d end %d at %v, want %v", name, k, i, got, want)
				}
			}
		}
	}
}

// TestSegmentOfAssignsEverySampleOnce: a sample on the track belongs to
// the segment whose span holds it, a span end to the later segment, B to
// the last; a sample off the track or past an end belongs to none.
func TestSegmentOfAssignsEverySampleOnce(t *testing.T) {
	c := mustCorridor(t, [2]float64{550000, 6223000}, [2]float64{575000, 6223000}) // E–W 25 km, 3 spans
	_, first := c.span(0)
	for name, tc := range map[string]struct {
		e, n float64
		want int
	}{
		"A":                    {550000, 6223000, 0},
		"in the first span":    {551000, 6223000, 0},
		"first span end":       {550000 + first, 6223000, 1},
		"B":                    {575000, 6223000, 2},
		"inside the slack":     {575000 + trackSlackM, 6223000 - trackSlackM, 2},
		"before A":             {550000 - trackSlackM - 0.01, 6223000, -1},
		"past B":               {575000 + trackSlackM + 0.01, 6223000, -1},
		"off the track, left":  {560000, 6223000 + trackSlackM + 0.01, -1},
		"off the track, right": {560000, 6223000 - trackSlackM - 0.01, -1},
		"NaN":                  {math.NaN(), 6223000, -1},
	} {
		s, tt := c.track(tc.e, tc.n)
		if got := c.segmentOf(s, tt); got != tc.want {
			t.Errorf("%s: segment %d, want %d", name, got, tc.want)
		}
	}
	// Every span end, on corridors of many segment counts: exactly at it is
	// the later segment, a hair before is the earlier.
	for _, l := range []float64{1, 333.3, 10000, 29999.9, 70000.7, MaxCorridorM} {
		c := mustCorridor(t, [2]float64{450000, 6100000}, [2]float64{450000 + l, 6100000})
		for k := 1; k < len(c.Segments()); k++ {
			s0, _ := c.span(k)
			if got := c.segmentOf(s0, 0); got != k {
				t.Errorf("L=%.1f: span %d start in segment %d", l, k, got)
			}
			if got := c.segmentOf(math.Nextafter(s0, 0), 0); got != k-1 {
				t.Errorf("L=%.1f: just before span %d in segment %d", l, k, got)
			}
		}
	}
}

// TestRadiusCoversTheWindow: every point of a sample's classification
// window lies within the radius of its own segment's query line, with
// room for the decimetre rounding, for every allowed radius, at the widest
// window (the longest profile step), and for samples at the very ends of a
// span or the edge of the track slack. So the one DWITHIN request of a
// segment returns every footprint its samples' windows can touch.
func TestRadiusCoversTheWindow(t *testing.T) {
	const roundingM = 0.5
	if MaxHalfWindowM+trackSlackM+roundingM > float64(AllowedRadiiM[0]) {
		t.Fatalf("window %.1f m + slack %.1f m exceeds the smallest radius %d m", MaxHalfWindowM, trackSlackM, AllowedRadiiM[0])
	}
	h := MaxHalfWindowM
	for _, r := range AllowedRadiiM {
		for _, ends := range [][2][2]float64{
			{[2]float64{560000, 6300000}, [2]float64{560000, 6220000}},     // N–S
			{[2]float64{560000, 6200000}, [2]float64{581213.2, 6221213.2}}, // diagonal
			{dokk1, [2]float64{dokk1[0] - 3, dokk1[1] + 4}},                // 5 m
		} {
			c, err := NewCorridor(ends[0], ends[1], r, DefaultSegmentM)
			if err != nil {
				t.Fatal(err)
			}
			for k, seg := range c.Segments() {
				s0, s1 := c.span(k)
				for _, s := range []float64{s0 - trackSlackM, s0, s1, s1 + trackSlackM} {
					for _, tt := range []float64{-trackSlackM, 0, trackSlackM} {
						for _, w := range windowOffsets {
							for _, v := range windowOffsets {
								e, n := c.point(s+w*h, tt+v*h)
								if d := lineDist(seg, e, n); d > float64(r)-roundingM {
									t.Fatalf("r=%d segment %d: window point %.1f m from the query line", r, k, d)
								}
							}
						}
					}
				}
			}
		}
	}
}

// lineDist is the distance in metres from (e, n) to the segment's query
// line, read back from its decimetres.
func lineDist(s Segment, e, n float64) float64 {
	a := [2]float64{float64(s.Line[0]) / 10, float64(s.Line[1]) / 10}
	b := [2]float64{float64(s.Line[2]) / 10, float64(s.Line[3]) / 10}
	dx, dy := b[0]-a[0], b[1]-a[1]
	t := math.Max(0, math.Min(1, ((e-a[0])*dx+(n-a[1])*dy)/(dx*dx+dy*dy)))
	return math.Hypot(a[0]+t*dx-e, a[1]+t*dy-n)
}

// TestSegmentValid: a Segment a caller builds by hand is checked again
// before it can reach a URL.
func TestSegmentValid(t *testing.T) {
	good := mustCorridor(t, dokk1, [2]float64{dokk1[0] - 900, dokk1[1] + 250}).Segments()[0]
	if !good.Valid() {
		t.Fatalf("%+v invalid", good)
	}
	w := lineWindowDM
	for name, mut := range map[string]func(*Segment){
		"zero value":           func(s *Segment) { *s = Segment{} },
		"radius 16":            func(s *Segment) { s.RadiusM = 16 },
		"zero length":          func(s *Segment) { s.Line[2], s.Line[3] = s.Line[0], s.Line[1] },
		"easting below window": func(s *Segment) { s.Line[0] = w[0] - 1 },
		"northing above":       func(s *Segment) { s.Line[3] = w[3] + 1 },
		"too long": func(s *Segment) {
			s.Line[2] = s.Line[0] + (MaxSegmentM+2*MaxRadiusM+1)*10 + 1
			s.Line[3] = s.Line[1]
		},
	} {
		s := good
		mut(&s)
		if s.Valid() {
			t.Errorf("%s: %+v valid", name, s)
		}
	}
	// The longest legal line is valid.
	s := good
	s.Line[2], s.Line[3] = s.Line[0]+(MaxSegmentM+2*MaxRadiusM+1)*10, s.Line[1]
	if !s.Valid() {
		t.Fatal("the longest legal line refused")
	}
}

// TestSegmentKeyIgnoresDirection: a path and its reverse ask for the same
// lines, so they share cache entries.
func TestSegmentKeyIgnoresDirection(t *testing.T) {
	a, b := [2]float64{560000, 6200000}, [2]float64{570000, 6200000} // one segment
	fw := mustCorridor(t, a, b).Segments()[0]
	bw := mustCorridor(t, b, a).Segments()[0]
	if fw.Line == bw.Line {
		t.Fatal("test needs lines that differ in direction")
	}
	if fw.key() != bw.key() {
		t.Fatalf("keys %v and %v differ", fw.key(), bw.key())
	}
	other := mustCorridor(t, a, [2]float64{570000, 6200000.1}).Segments()[0]
	if other.key() == fw.key() {
		t.Fatal("different lines share a key")
	}
	r20, _ := NewCorridor(a, b, 20, DefaultSegmentM)
	if r20.Segments()[0].key() == fw.key() {
		t.Fatal("different radii share a key")
	}
}
