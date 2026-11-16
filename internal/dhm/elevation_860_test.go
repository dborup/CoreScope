package dhm

import (
	"bufio"
	"errors"
	"fmt"
	"image/color"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// #860 — the colour-by-elevation ramp tuned to Danish land. These tests
// pin what the ramp is for: its stops share out the land (from a measured
// histogram, testdata/dk_land_hist_860.txt), the low-lying ground stands
// apart from the lowland, the hills are warm, every colour shows on the
// light and the dark base maps, and the tile URL changes with the ramp.

// The stops in metres, chosen from the measured distribution (see
// ElevationRamp).
func TestElevation860_StopsAreTheMeasuredOnes(t *testing.T) {
	want := []int{0, 2, 5, 10, 15, 20, 25, 30, 40, 50, 65, 80, 100, 130, 175}
	got := make([]int, len(ElevationRamp))
	for i, s := range ElevationRamp {
		got[i] = s.M
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stops %v, want %v", got, want)
	}
	if ElevationRampMaxM != want[len(want)-1] {
		t.Errorf("ElevationRampMaxM = %d, want the top stop %d", ElevationRampMaxM, want[len(want)-1])
	}
}

// landHist860 is the measured share of Danish land per whole metre:
// index 0 is every height that rounds to 0 m or below, the last index
// every height at or above it.
func landHist860(t *testing.T) []float64 {
	t.Helper()
	f, err := os.Open("testdata/dk_land_hist_860.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var m int
		var share float64
		if _, err := fmt.Sscanf(line, "%d %g", &m, &share); err != nil || m != len(out) {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, share)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, s := range out {
		sum += s
	}
	if len(out) < 176 || math.Abs(sum-1) > 1e-3 {
		t.Fatalf("histogram: %d metres, sum %v", len(out), sum)
	}
	return out
}

// bandShares is the share of land whose whole-metre height falls in each
// band [stop i, stop i+1) of ramp; the last band runs to the top.
func bandShares(hist []float64, ramp []RampStop) []float64 {
	out := make([]float64, len(ramp)-1)
	for m, s := range hist {
		k := 0
		for k+1 < len(out) && m >= ramp[k+1].M {
			k++
		}
		out[k] += s
	}
	return out
}

// Every band below 80 m holds a meaningful share of Danish land, 4-15 %,
// so no colour is spent on ground that is not there and no band is a
// third of the country; the bands from 80 m are kept for the hills
// (about 5 % of the land) and are each narrower still.
func TestElevation860_BandsShareTheLand(t *testing.T) {
	hist := landHist860(t)
	shares := bandShares(hist, ElevationRamp)
	for i, s := range shares {
		lo, hi := ElevationRamp[i].M, ElevationRamp[i+1].M
		t.Logf("%3d-%3d m: %5.1f %%", lo, hi, 100*s)
		if lo < 80 && (s < 0.04 || s > 0.15) {
			t.Errorf("band %d-%d m holds %.1f %% of the land, want 4-15 %%", lo, hi, 100*s)
		}
		if lo >= 80 && s > 0.05 {
			t.Errorf("hill band %d-%d m holds %.1f %%, want at most 5 %%", lo, hi, 100*s)
		}
	}
}

// oklab is c in the OKLab space, where a Euclidean distance is a
// perceived colour difference (about 0.02 is just noticeable).
func oklab(c color.NRGBA) [3]float64 {
	lin := func(v uint8) float64 {
		x := float64(v) / 255
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	r, g, b := lin(c.R), lin(c.G), lin(c.B)
	l := math.Cbrt(0.4122214708*r + 0.5363325363*g + 0.0514459929*b)
	m := math.Cbrt(0.2119034982*r + 0.6806995451*g + 0.1073969566*b)
	s := math.Cbrt(0.0883024619*r + 0.2817188376*g + 0.6299787005*b)
	return [3]float64{
		0.2104542553*l + 0.7936177850*m - 0.0040720468*s,
		1.9779984951*l - 2.4285922050*m + 0.4505937099*s,
		0.0259040371*l + 0.7827717662*m - 0.8086757660*s,
	}
}

func deltaE(a, b color.NRGBA) float64 {
	x, y := oklab(a), oklab(b)
	return math.Sqrt((x[0]-y[0])*(x[0]-y[0]) + (x[1]-y[1])*(x[1]-y[1]) + (x[2]-y[2])*(x[2]-y[2]))
}

// metreColour is the tile colour of a whole-metre height.
func metreColour(m int) color.NRGBA {
	return elevationPalette[elevationIndex(float64(m)+0.1)].(color.NRGBA)
}

func stopColour(s RampStop) color.NRGBA { return color.NRGBA{s.R, s.G, s.B, 255} }

// Neighbouring stops are told apart: at least about two just-noticeable
// differences, so no stop is spent on a colour its neighbour already has.
func TestElevation860_NeighbouringStopsDiffer(t *testing.T) {
	for i := 1; i < len(ElevationRamp); i++ {
		a, b := ElevationRamp[i-1], ElevationRamp[i]
		if d := deltaE(stopColour(a), stopColour(b)); d < 0.035 {
			t.Errorf("stops %d m and %d m differ by %.3f, want >= 0.035", a.M, b.M, d)
		}
	}
}

// hue is c's OKLCH hue in degrees, 0-360: green is about 140, teal 180.
func hue(c color.NRGBA) float64 {
	l := oklab(c)
	return math.Mod(math.Atan2(l[2], l[1])*180/math.Pi+360, 360)
}

// Low-lying ground, 0-4 m (marsh, reclaimed land, coast), is clearly apart
// from the 6-20 m lowland: it is teal, every hue at least 150 degrees,
// where the lowland is green, every hue at most 145, and no colour of the
// one is within 0.04 (OKLab, two just-noticeable differences) of a colour
// of the other. The 5 m stop between them is where the hue turns. The
// #792 ramp gave all of 0-20 m one green (hues 151 to 139, 4 m and 6 m
// 0.011 apart).
func TestElevation860_LowLyingStandsApartFromTheLowland(t *testing.T) {
	for m := 0; m <= 4; m++ {
		if h := hue(metreColour(m)); h < 150 {
			t.Errorf("%d m has hue %.0f, want teal (>= 150)", m, h)
		}
	}
	for m := 6; m <= 20; m++ {
		if h := hue(metreColour(m)); h > 145 {
			t.Errorf("%d m has hue %.0f, want green (<= 145)", m, h)
		}
	}
	worst, wa, wb := math.Inf(1), 0, 0
	for a := 0; a <= 4; a++ {
		for b := 6; b <= 20; b++ {
			if d := deltaE(metreColour(a), metreColour(b)); d < worst {
				worst, wa, wb = d, a, b
			}
		}
	}
	if worst < 0.04 {
		t.Errorf("%d m and %d m differ by %.3f, want >= 0.04", wa, wb, worst)
	}
}

// The hills are warm: from 80 m every colour is red-dominated (red at
// least 60 above green, and not below blue), so Himmelbjerget, Ejer
// Bavnehøj and Mols Bjerge stand out from the yellow and orange lowland.
func TestElevation860_HillsAreWarm(t *testing.T) {
	var cold []string
	for m := 80; m <= ElevationRampMaxM; m++ {
		if c := metreColour(m); int(c.R) < int(c.G)+60 || c.R < c.B {
			cold = append(cold, fmt.Sprintf("%d m %v", m, c))
		}
	}
	if len(cold) > 0 {
		t.Errorf("%d hill heights not red-dominated, first %s", len(cold), cold[0])
	}
	// Below the hills the ramp is not yet red: 50 m is still orange.
	if c := metreColour(50); int(c.G) < int(c.R)/2 {
		t.Errorf("50 m is %v, already red", c)
	}
}

// Every colour shows on the base maps at the overlay's default opacity
// (60 %, DHM_ELEVATION_OPACITY in public/map-tile-providers.js): over a
// light land colour (OSM Standard #f2efe9, Carto Positron #fafaf8) and a
// dark one (Carto Dark Matter #0e0e0e, a dimmed dark map #2b2b2b) the
// overlay changes the colour by at least 0.08 (OKLab), about four
// just-noticeable differences. The #792 ramp's light top (#f2eee9) all
// but vanished on a light map.
func TestElevation860_ReadableOnLightAndDarkBaseMaps(t *testing.T) {
	const opacity = 0.6
	bases := []color.NRGBA{{0xf2, 0xef, 0xe9, 255}, {0xfa, 0xfa, 0xf8, 255}, {0x0e, 0x0e, 0x0e, 255}, {0x2b, 0x2b, 0x2b, 255}}
	over := func(c, bg color.NRGBA) color.NRGBA {
		mix := func(x, y uint8) uint8 { return uint8(math.Round(opacity*float64(x) + (1-opacity)*float64(y))) }
		return color.NRGBA{mix(c.R, bg.R), mix(c.G, bg.G), mix(c.B, bg.B), 255}
	}
	for _, bg := range bases {
		worst, at := math.Inf(1), 0
		for m := 0; m <= ElevationRampMaxM; m++ {
			if d := deltaE(over(metreColour(m), bg), bg); d < worst {
				worst, at = d, m
			}
		}
		if worst < 0.08 {
			t.Errorf("on %v: %d m changes the map by %.3f, want >= 0.08", bg, at, worst)
		}
	}
}

// The ramp's id, the `styles` value of an elevation tile URL, is derived
// from every stop: any change to a height or a colour is a new URL, so no
// browser or shared cache serves a tile of an older ramp (#860).
func TestElevation860_StyleFollowsTheRamp(t *testing.T) {
	if !regexp.MustCompile(`^ramp-[0-9a-f]{8}$`).MatchString(ElevationStyle) {
		t.Fatalf("ElevationStyle %q", ElevationStyle)
	}
	if ElevationStyle != rampStyle(ElevationRamp) {
		t.Fatalf("ElevationStyle %q is not the ramp's %q", ElevationStyle, rampStyle(ElevationRamp))
	}
	seen := map[string]string{ElevationStyle: "the ramp"}
	for i := range ElevationRamp {
		for f, edit := range map[string]func(*RampStop){
			"M": func(s *RampStop) { s.M++ }, "R": func(s *RampStop) { s.R ^= 1 },
			"G": func(s *RampStop) { s.G ^= 1 }, "B": func(s *RampStop) { s.B ^= 1 },
		} {
			r := append([]RampStop(nil), ElevationRamp...)
			edit(&r[i])
			id := rampStyle(r)
			what := "stop " + strconv.Itoa(i) + " " + f
			if prev, dup := seen[id]; dup {
				t.Errorf("%s gives %q, the same as %s", what, id, prev)
			}
			seen[id] = what
		}
	}
	// A stop moved to another index is another ramp too.
	r := append([]RampStop(nil), ElevationRamp...)
	r[1], r[2] = r[2], r[1]
	if rampStyle(r) == ElevationStyle {
		t.Error("swapped stops give the same id")
	}
}

// The palette is the ramp: each stop's metre has the stop's colour.
func TestElevation860_PaletteHitsEveryStop(t *testing.T) {
	for _, s := range ElevationRamp {
		if got := metreColour(s.M); got != stopColour(s) {
			t.Errorf("%d m is %v, want %v", s.M, got, stopColour(s))
		}
	}
}

// An elevation tile URL names its ramp in `styles` (#860), so the browser
// asks for a new URL when the ramp changes. The proxy takes exactly the
// current ramp's id, or the empty `styles` of a page loaded before the
// deploy (one release, like the #680 terrain alias); any other id, a
// repeated key, and a ramp id on an upstream overlay are refused.
func TestElevation860_TileRequestNamesTheRamp(t *testing.T) {
	withStyles := func(layers string, k TileKey, styles ...string) url.Values {
		q := overlayTileQuery(layers, k)
		q["styles"] = styles
		return q
	}
	for _, c := range []struct {
		name string
		q    url.Values
		ok   bool
	}{
		{"elevation, the ramp's id", withStyles("elevation", elevTileZ13, ElevationStyle), true},
		{"elevation, empty (a page from before #860)", withStyles("elevation", elevTileZ13, ""), true},
		{"elevation, another ramp's id", withStyles("elevation", elevTileZ13, "ramp-00000000"), false},
		{"elevation, the id twice", withStyles("elevation", elevTileZ13, ElevationStyle, ElevationStyle), false},
		{"elevation, the id and empty", withStyles("elevation", elevTileZ13, ElevationStyle, ""), false},
		{"elevation, the id in upper case", withStyles("elevation", elevTileZ13, strings.ToUpper(ElevationStyle)), false},
		{"terrain, the ramp's id", withStyles("terrain", elevTileZ13, ElevationStyle), false},
		{"surface, the ramp's id", withStyles("surface", elevTileZ13, ElevationStyle), false},
		{"contours, the ramp's id", withStyles("contours", TileKey{Z: 14, X: 8764, Y: 5120}, ElevationStyle), false},
		{"contours, its upstream style", withStyles("contours", TileKey{Z: 14, X: 8764, Y: 5120}, ContourStyle), false},
		{"terrain, empty", withStyles("terrain", elevTileZ13, ""), true},
	} {
		o, k, err := ParseTileRequest(c.q)
		if c.ok && (err != nil || k.Z == 0 || o.ID == "") {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: accepted (%v)", c.name, err)
		}
	}
	// The query the caller passed is not changed.
	q := withStyles("elevation", elevTileZ13, ElevationStyle)
	ParseTileRequest(q)
	if q.Get("styles") != ElevationStyle {
		t.Errorf("styles became %q", q.Get("styles"))
	}
}

// The tile parse runs on every elevation request, cache hits included.
// Naming the ramp costs one copy of the 11-key query (maps.Clone); the
// empty styles of every other overlay takes the old path.
func BenchmarkElevation860_ParseTileRequest(b *testing.B) {
	for _, c := range []struct{ name, layers, styles string }{
		{"terrain", "terrain", ""},
		{"elevation-ramp-id", "elevation", ElevationStyle},
	} {
		q := overlayTileQuery(c.layers, elevTileZ13)
		q.Set("styles", c.styles)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := ParseTileRequest(q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
