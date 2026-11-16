package packetcard

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustRender(t testing.TB, in PacketCardInput, opt Options) (*image.RGBA, *trace) {
	t.Helper()
	img, tr, err := render(in, opt)
	if err != nil {
		t.Fatal(err)
	}
	return img, tr
}

func allText(tr *trace) string {
	var s []string
	for _, r := range tr.runs {
		s = append(s, r.Text)
	}
	return strings.Join(s, "\n")
}

// Owner decision Q2 (revised): every payload type gets a card, the
// direct-routed TXT_MSG, REQ and RESPONSE too, named by the firmware
// name and nothing from the payload.
func TestEveryPayloadTypeCarded(t *testing.T) {
	for pt := 0; pt < 16; pt++ {
		in := fxDirect()
		in.PayloadType = pt
		out, err := Render(in, Options{})
		if err != nil || len(out) == 0 {
			t.Fatalf("payload type %d: %v", pt, err)
		}
		_, tr := mustRender(t, in, Options{})
		if got, want := runText(tr.runs, "title"), PayloadName(pt)+" · Direct · 2 hops"; got != want {
			t.Errorf("payload type %d: title %q, want %q", pt, got, want)
		}
	}
	for pt, want := range map[int]string{-1: "Unknown type", 12: "Unknown type", 16: "Unknown type", 1 << 40: "Unknown type"} {
		if got := PayloadName(pt); got != want {
			t.Errorf("PayloadName(%d) = %q, want %q", pt, got, want)
		}
	}
}

// The card names payload types with the frontend's firmware names
// (payload-labels.js, which packet-filter.js reads) and routes with
// packet-filter.js's ROUTE_TYPES, in words.
func TestNamesMatchFrontend(t *testing.T) {
	root := filepath.Join("..", "..", "public")
	labels, err := os.ReadFile(filepath.Join(root, "payload-labels.js"))
	if err != nil {
		t.Fatal(err)
	}
	ms := regexp.MustCompile(`enumName: '([A-Z_]+)'[^}]*enumId: (\d+)`).FindAllStringSubmatch(string(labels), -1)
	if len(ms) < 13 {
		t.Fatalf("found %d payload labels", len(ms))
	}
	seen := map[int]bool{}
	for _, m := range ms {
		id := 0
		for _, c := range m[2] {
			id = id*10 + int(c-'0')
		}
		seen[id] = true
		if got := PayloadName(id); got != m[1] {
			t.Errorf("payload %d: card %q, frontend %q", id, got, m[1])
		}
	}
	for pt := 0; pt < 16; pt++ {
		if !seen[pt] && PayloadName(pt) != "Unknown type" {
			t.Errorf("payload %d: card names it %q, the frontend does not", pt, PayloadName(pt))
		}
	}
	filter, err := os.ReadFile(filepath.Join(root, "packet-filter.js"))
	if err != nil {
		t.Fatal(err)
	}
	rm := regexp.MustCompile(`ROUTE_TYPES = \{ 0: '(\w+)', 1: '(\w+)', 2: '(\w+)', 3: '(\w+)' \}`).FindStringSubmatch(string(filter))
	if rm == nil {
		t.Fatal("ROUTE_TYPES not found in packet-filter.js")
	}
	for rt := 0; rt < 4; rt++ {
		want := strings.ToLower(strings.ReplaceAll(rm[rt+1], "_", " "))
		if got := strings.ToLower(RouteName(rt)); got != want {
			t.Errorf("route %d: card %q, frontend %q", rt, got, want)
		}
	}
	if RouteName(4) != "" || RouteName(-1) != "" {
		t.Error("an unknown route must have no name")
	}
	in := fxFlood()
	in.RouteType = 9
	if _, tr := mustRender(t, in, Options{}); runText(tr.runs, "title") != "GRP_TXT · 6 hops" {
		t.Errorf("unknown route: title %q", runText(tr.runs, "title"))
	}
}

// PacketCardInput is named structs only, with no map or interface
// anywhere, and no field that could carry message text, a channel, a key,
// a sender, a hash or any name other than the two the rules allow
// (#812 privacy rules 1–4).
func TestInputHasNoContentFields(t *testing.T) {
	bad := regexp.MustCompile(`(?i)text|msg|message|channel|key|secret|psk|sender|decrypt|raw|hex|hash|pub|label|payload$|data|body|name`)
	allowed := map[string]bool{"FirstObserverName": true, "AdvertName": true, "PayloadType": true}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(rt.Elem(), path)
			return
		case reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
			t.Errorf("%s: %v; the input is named structs only", path, rt)
			return
		case reflect.Struct:
		default:
			return
		}
		if rt.PkgPath() != "" && !strings.HasSuffix(rt.PkgPath(), "packetcard") {
			return // time.Time
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if bad.MatchString(f.Name) && !allowed[f.Name] {
				t.Errorf("%s.%s: the packet card takes no content, key or extra name", rt.Name(), f.Name)
			}
			walk(f.Type, rt.Name()+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(PacketCardInput{}), "PacketCardInput")
}

// Owner decision Q3 (revised): the first observer is named, "First heard
// by <name> · heard by N".
func TestFirstObserverNamed(t *testing.T) {
	_, tr := mustRender(t, fxFlood(), Options{})
	if got := runText(tr.runs, "heard"); got != "First heard by Aarhus-Obs · heard by 3 observers" {
		t.Errorf("heard %q", got)
	}
}

// When the first observation is hidden (First nil), the card falls back to
// counts only: the name is not drawn even when a caller leaves it set, no
// other observer is promoted in its place, and the spread and distance,
// measured from the hidden origin, are not drawn either.
func TestHiddenFirstFallsBackToCounts(t *testing.T) {
	_, tr := mustRender(t, fxHiddenFirst(), Options{})
	if got := runText(tr.runs, "heard"); got != "Heard by 2 observers" {
		t.Errorf("heard %q, want counts only", got)
	}
	all := allText(tr)
	for _, no := range []string{fxObserver, "First heard by", "Spread", "Farthest", "4.2s", "25.3 km"} {
		if strings.Contains(all, no) {
			t.Errorf("hidden first: %q drawn:\n%s", no, all)
		}
	}
	if tr.first {
		t.Error("hidden first: the first-heard ring is drawn")
	}
}

// The first observer's name is drawn only when it can be drawn: a name of
// boxes falls back to counts, never to another observer.
func TestUndrawableFirstNameFallsBackToCounts(t *testing.T) {
	in := fxFlood()
	in.FirstObserverName = "东京中继站"
	if _, tr := mustRender(t, in, Options{}); runText(tr.runs, "heard") != "Heard by 3 observers" {
		t.Errorf("heard %q", runText(tr.runs, "heard"))
	}
}

// Privacy rule 2: only an advert names its origin.
func TestAdvertNameOnlyForAdverts(t *testing.T) {
	for pt := 0; pt < 16; pt++ {
		in := fxAdvert()
		in.PayloadType = pt
		_, tr := mustRender(t, in, Options{})
		if got := strings.Contains(allText(tr), "Kjeldbjerg"); got != (pt == 4) {
			t.Errorf("payload type %d: origin drawn = %v", pt, got)
		}
	}
}

// HideNames draws neither name.
func TestHideNames(t *testing.T) {
	_, tr := mustRender(t, fxAdvert(), Options{HideNames: true})
	all := allText(tr)
	if strings.Contains(all, "Kjeldbjerg") || strings.Contains(all, fxObserver) {
		t.Errorf("names drawn with HideNames:\n%s", all)
	}
}

// Every value is computed from what the card was given: heard-by counts
// the visible branches, hops is the deepest of them, spread and distance
// the largest from the first.
func TestCountsFromInput(t *testing.T) {
	_, tr := mustRender(t, fxFlood(), Options{})
	if got := runText(tr.runs, "label"); got != "Observers | Spread | Farthest from first | Est. flood airtime" {
		t.Errorf("labels %q", got)
	}
	if got := runText(tr.runs, "value"); got != "3 | 4.2s | 38.0 km | ≥1.2s" {
		t.Errorf("values %q", got)
	}
	in := fxFlood()
	in.Branches = in.Branches[1:]
	if _, tr := mustRender(t, in, Options{}); !strings.HasSuffix(runText(tr.runs, "title"), "· 4 hops") || !strings.HasSuffix(runText(tr.runs, "heard"), "heard by 2 observers") {
		t.Errorf("after dropping the deepest branch: %q / %q", runText(tr.runs, "title"), runText(tr.runs, "heard"))
	}
}

// An absent or invalid value drops its clause; it is never drawn as 0.
func TestAbsentDropsClause(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*PacketCardInput)
		gone string
	}{
		"no airtime":       {func(in *PacketCardInput) { in.AirtimeMs = nil }, "Est. flood airtime"},
		"NaN airtime":      {func(in *PacketCardInput) { in.AirtimeMs = fp(math.NaN()) }, "Est. flood airtime"},
		"negative airtime": {func(in *PacketCardInput) { in.AirtimeMs = fp(-1) }, "Est. flood airtime"},
		"no spread": {func(in *PacketCardInput) {
			for i := range in.Branches {
				in.Branches[i].SecondsAfterFirst = nil
			}
		}, "Spread"},
		"Inf spread": {func(in *PacketCardInput) {
			for i := range in.Branches {
				in.Branches[i].SecondsAfterFirst = fp(math.Inf(1))
			}
		}, "Spread"},
		"no distance": {func(in *PacketCardInput) {
			for i := range in.Branches {
				in.Branches[i].DistanceFromFirstKm = fp(0)
			}
		}, "Farthest"},
		"no first heard": {func(in *PacketCardInput) { in.FirstHeard = time.Time{} }, "First heard 2"},
		"no branches":    {func(in *PacketCardInput) { in.Branches = nil; in.First = nil }, "hops"},
	} {
		in := fxFlood()
		c.edit(&in)
		_, tr := mustRender(t, in, Options{})
		if all := allText(tr); strings.Contains(all, c.gone) {
			t.Errorf("%s: %q still drawn:\n%s", name, c.gone, all)
		}
	}
	in := fxFlood()
	in.Branches, in.First = nil, nil
	if _, tr := mustRender(t, in, Options{}); runText(tr.runs, "heard") != "No visible observers" {
		t.Errorf("no branches: heard %q", runText(tr.runs, "heard"))
	}
}

func TestFormatting(t *testing.T) {
	for s, want := range map[float64]string{0.5: "0.5s", 4.2: "4.2s", 59.96: "60.0s", 61: "1m 1s", 125.5: "2m 6s"} {
		if got := fmtDuration(s); got != want {
			t.Errorf("fmtDuration(%v) = %q, want %q", s, got, want)
		}
	}
	for ms, want := range map[float64]string{0: "0ms", 312.4: "312ms", 999.4: "999ms", 1000: "1.0s", 1234: "1.2s", 8840: "8.8s"} {
		if got := fmtAirtime(ms); got != want {
			t.Errorf("fmtAirtime(%v) = %q, want %q", ms, got, want)
		}
	}
}

// Owner decision Q4 (revised): the touched areas are drawn as faint
// outlines behind the path, clipped to the map.
func TestAreaOutlinesDrawn(t *testing.T) {
	with, tr := mustRender(t, fxFlood(), Options{})
	in := fxFlood()
	in.Areas = nil
	without, _ := mustRender(t, in, Options{})
	if tr.areas != 2 {
		t.Errorf("%d areas drawn, want 2", tr.areas)
	}
	pal := DefaultPalette(true)
	n, full := 0, 0
	for y := mapTop; y < mapBottom; y++ {
		for x := safeMargin; x < Width-safeMargin; x++ {
			a, b := with.RGBAAt(x, y), without.RGBAAt(x, y)
			if a == b {
				continue
			}
			n++
			if a.R == pal.Accent.R && a.G == pal.Accent.G && a.B == pal.Accent.B {
				full++
			}
		}
	}
	if n < 200 {
		t.Errorf("areas changed %d pixels, want outlines", n)
	}
	if full != 0 {
		t.Errorf("%d area pixels in full accent; the outlines are faint", full)
	}
	// Nothing outside the map changes: the outlines are clipped to it.
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			if (y < mapTop || y >= mapBottom) && with.RGBAAt(x, y) != without.RGBAAt(x, y) {
				t.Fatalf("area outline outside the map at (%d,%d)", x, y)
			}
		}
	}
}

// An area larger than the map, or with a vertex on the far side of the
// world, is clipped, not dropped; a ring with an invalid vertex or fewer
// than three is dropped.
func TestAreaClippingAndValidation(t *testing.T) {
	in := fxFlood()
	in.Areas = []Area{{Ring: BoxRing(-80, 80, -170, 170)}}
	if _, tr := mustRender(t, in, Options{}); tr.areas != 0 {
		t.Errorf("an area around the whole map has no edge in it: %d drawn", tr.areas)
	}
	in.Areas = []Area{{Ring: []LatLon{{56.1, 10.1}, {56.2, 10.2}, {-60, -170}}}}
	if _, tr := mustRender(t, in, Options{}); tr.areas != 1 {
		t.Errorf("an area reaching off the map: %d drawn, want 1 (clipped)", tr.areas)
	}
	// A strip taller than the map: every edge leaves it, two cross it.
	in.Areas = []Area{{Ring: BoxRing(55.0, 57.5, 10.1, 10.15)}}
	if _, tr := mustRender(t, in, Options{}); tr.areas != 1 {
		t.Errorf("a strip across the map: %d drawn, want 1 (clipped)", tr.areas)
	}
	for name, ring := range map[string][]LatLon{
		"NaN":    {{56.1, 10.1}, {math.NaN(), 10.2}, {56.2, 10.0}, {56.15, 10.3}},
		"lat 91": {{56.1, 10.1}, {91, 10.2}, {56.2, 10.0}, {56.15, 10.3}},
		"two":    {{56.1, 10.1}, {56.2, 10.2}},
	} {
		in.Areas = []Area{{Ring: ring}}
		if _, tr := mustRender(t, in, Options{}); tr.areas != 0 {
			t.Errorf("%s: %d drawn, want 0", name, tr.areas)
		}
	}
}

// Privacy rule 5: with no visible positioned point the card is text only,
// type, route and counts, never an empty map; the areas are not drawn.
func TestTextOnlyWhenNothingPositioned(t *testing.T) {
	in := fxNoPositions()
	in.Areas = []Area{midtjylland()}
	img, tr := mustRender(t, in, Options{})
	if tr.mapped || tr.areas != 0 || tr.branches != 0 {
		t.Errorf("mapped %v, %d areas, %d branches drawn", tr.mapped, tr.areas, tr.branches)
	}
	if runText(tr.runs, "empty") == "" || hasRole(tr.runs, "note") {
		t.Errorf("runs:\n%s", formatRuns(tr.runs))
	}
	bg := DefaultPalette(true).Background
	for y := mapTop; y < mapBottom; y += 3 {
		for x := safeMargin; x < Width-safeMargin; x += 3 {
			if c := img.RGBAAt(x, y); (c.R != bg.R || c.G != bg.G || c.B != bg.B) && !nearRun(tr.runs, x, y) {
				t.Fatalf("map area drawn at (%d,%d) with nothing positioned", x, y)
			}
		}
	}
}

func nearRun(runs []textRun, x, y int) bool {
	for _, r := range runs {
		if x >= r.X-2 && x <= r.X+r.W+2 && y >= r.Y-r.Size && y <= r.Y+r.Size/2 {
			return true
		}
	}
	return false
}

// Bounds: at most MaxBranches branches are drawn ("+N more" for the rest,
// all of them counted) and at most MaxPoints points, decimated with each
// branch's ends kept. The work is bounded whatever the input size.
func TestBounds(t *testing.T) {
	in := fxFanout(MaxBranches + 72)
	_, tr := mustRender(t, in, Options{})
	if tr.branches != MaxBranches+1 { // + the first, drawn on top
		t.Errorf("%d branches drawn, want %d", tr.branches, MaxBranches+1)
	}
	if got := runText(tr.runs, "more"); got != "+72 more not drawn" {
		t.Errorf("more %q", got)
	}
	huge := make([]LatLon, 1_000_000)
	for i := range huge {
		huge[i] = LatLon{56 + float64(i)*1e-7, 10 + float64(i)*1e-7}
	}
	in = PacketCardInput{PayloadType: 5, RouteType: 1}
	for i := 0; i < 5000; i++ {
		in.Branches = append(in.Branches, Branch{Hops: 3, Points: huge, Observer: at(56.2, 10.2)})
	}
	in.Areas = make([]Area, 10_000)
	for i := range in.Areas {
		in.Areas[i] = Area{Ring: huge}
	}
	start := time.Now()
	_, tr = mustRender(t, in, Options{})
	if tr.points > MaxPoints {
		t.Errorf("%d points drawn, bound %d", tr.points, MaxPoints)
	}
	if tr.areas > MaxAreas || tr.areaVertices > MaxAreaVertices {
		t.Errorf("%d areas, %d vertices drawn", tr.areas, tr.areaVertices)
	}
	if got := runText(tr.runs, "heard"); got != "Heard by 5,000 observers" {
		t.Errorf("heard %q", got)
	}
	if d := time.Since(start); !raceEnabled && d > 2*time.Second {
		t.Errorf("a huge input took %v", d)
	}
	if got := decimate(huge, 10); len(got) != 10 || got[0] != huge[0] || got[9] != huge[len(huge)-1] {
		t.Errorf("decimate kept %d, ends %v %v", len(got), got[0], got[len(got)-1])
	}
}

// ── Projection ───────────────────────────────────────────────────────────

func projected(t *testing.T, pts ...LatLon) []image.Point {
	t.Helper()
	var us []upt
	for _, p := range pts {
		u, ok := toMicro(p)
		if !ok {
			t.Fatalf("%v is not drawable", p)
		}
		us = append(us, u)
	}
	pr := newProjection(us)
	out := make([]image.Point, len(us))
	for i, u := range us {
		out[i] = pr.pixel(pr.local(u))
	}
	return out
}

func dist(a, b image.Point) float64 {
	return math.Hypot(float64(a.X-b.X), float64(a.Y-b.Y)) / 256
}

// Two points 10 m apart are drawn close together inside a ~2 km span,
// not stretched across the card; one point sits in the middle.
func TestProjectionMinimumSpan(t *testing.T) {
	for _, pair := range [][2]LatLon{{{56.15, 10.2}, {56.15009, 10.2}}, {{56.15, 10.2}, {56.15, 10.20016}}} {
		p := projected(t, pair[0], pair[1])
		if d := dist(p[0], p[1]); d < 0.5 || d > float64(innerH())*0.01/minSpanKm*1.2 {
			t.Errorf("10 m (%v) drawn %.1f px apart", pair, d)
		}
	}
	one := projected(t, pAarhus)[0]
	if cx, cy := one.X>>8, one.Y>>8; abs(cx-Width/2) > 1 || abs(cy-(mapTop+mapBottom)/2) > 1 {
		t.Errorf("a single point at (%d,%d), want the map's centre", cx, cy)
	}
}

// Equirectangular: a degree of longitude is cos(latitude) of a degree of
// latitude, and north is up.
func TestProjectionAspect(t *testing.T) {
	p := projected(t, LatLon{56, 10}, LatLon{57, 10}, LatLon{56, 11})
	ns, ew := dist(p[0], p[1]), dist(p[0], p[2])
	if r := ew / ns; math.Abs(r-math.Cos(56.5*math.Pi/180)) > 0.01 {
		t.Errorf("east-west / north-south = %.3f, want cos 56.5° = %.3f", r, math.Cos(56.5*math.Pi/180))
	}
	if p[1].Y >= p[0].Y || p[2].X <= p[0].X {
		t.Error("north must be up and east right")
	}
}

// Points either side of the antimeridian are drawn next to each other,
// not at opposite edges of the card: two 0.2° apart across it, a third
// 0.9° further west, and the pair is closer than the third.
func TestProjectionAntimeridian(t *testing.T) {
	p := projected(t, LatLon{-17.7, 179.9}, LatLon{-17.8, -179.9}, LatLon{-17.75, 179.0})
	if pair, third := dist(p[0], p[1]), dist(p[0], p[2]); pair >= third {
		t.Errorf("across the antimeridian: the pair %.0f px apart, the third %.0f px", pair, third)
	}
	if p[1].X <= p[0].X || p[2].X >= p[0].X {
		t.Error("-179.9° is east of 179.9° across the antimeridian, 179.0° west of it")
	}
	if q := projected(t, LatLon{0, -180}, LatLon{0, 180}); dist(q[0], q[1]) > 1 {
		t.Errorf("±180° are the same meridian: %.1f px apart", dist(q[0], q[1]))
	}
}

// NaN, infinite and out-of-range positions are not drawn and do not move
// the drawable ones.
func TestInvalidPositionsDropped(t *testing.T) {
	for _, p := range []LatLon{{math.NaN(), 10}, {56, math.NaN()}, {math.Inf(1), 10}, {91, 10}, {-90.0001, 0}, {0, 180.0001}, {0, -181}} {
		if _, ok := toMicro(p); ok {
			t.Errorf("%v accepted", p)
		}
	}
	for _, p := range []LatLon{{90, 180}, {-90, -180}, {0, 0}} {
		if _, ok := toMicro(p); !ok {
			t.Errorf("%v refused", p)
		}
	}
	in := fxFlood()
	base, _ := mustRender(t, in, Options{})
	in.Branches[0].Points = append([]LatLon{{math.NaN(), 0}, {95, 200}}, in.Branches[0].Points...)
	got, _ := mustRender(t, in, Options{})
	if !bytes.Equal(base.Pix, got.Pix) {
		t.Error("invalid positions changed the card")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ── Layout, size, time ───────────────────────────────────────────────────

// Every text run stays inside the safe area, and runs in a row never
// overlap.
func TestRunsInsideSafeArea(t *testing.T) {
	for _, c := range goldenCases() {
		_, tr := mustRender(t, c.in, Options{Site: "a-very-long-site-name.example.org"})
		for _, r := range tr.runs {
			if r.X < safeMargin || r.X+r.W > Width-safeMargin || r.Y > Height-safeMargin {
				t.Errorf("%s: %s run %q at x=%d w=%d y=%d leaves the safe area", c.name, r.Role, r.Text, r.X, r.W, r.Y)
			}
		}
		rows := map[int][]textRun{}
		for _, r := range tr.runs {
			rows[r.Y] = append(rows[r.Y], r)
		}
		for y, rs := range rows {
			sort.Slice(rs, func(i, j int) bool { return rs[i].X < rs[j].X })
			for i := 1; i < len(rs); i++ {
				if rs[i-1].X+rs[i-1].W > rs[i].X {
					t.Errorf("%s: runs %q and %q overlap on row %d", c.name, rs[i-1].Text, rs[i].Text, y)
				}
			}
		}
	}
}

// Every card is 1200×630 and under the size bound: 128 KiB, so the ~64
// cached packet cards of #812 M2 stay under 8 MiB. The schematic's
// anti-aliased lines compress less than the node card's text (64 KiB).
func TestCardSizeBound(t *testing.T) {
	largest := 0
	for _, c := range goldenCases() {
		for _, dark := range []bool{true, false} {
			out, err := Render(c.in, optFor(t, c, dark))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(out))
			if err != nil || cfg.Width != Width || cfg.Height != Height {
				t.Fatalf("%s: %dx%d %v", c.name, cfg.Width, cfg.Height, err)
			}
			if len(out) > largest {
				largest = len(out)
			}
			if len(out) > maxCardBytes {
				t.Errorf("%s: %d bytes, bound %d", c.name, len(out), maxCardBytes)
			}
		}
	}
	t.Logf("largest card %d bytes (bound %d)", largest, maxCardBytes)
}

const maxCardBytes = 128 << 10

// The per-card budget, the mechanism of #755 and #837: the target is
// logged against, the test fails at twice it. Skipped under -race; CI runs
// it once more without (deploy.yml).
func TestRenderWithinBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("timing is meaningless under -race")
	}
	var times []time.Duration
	for _, c := range goldenCases() {
		best := time.Duration(math.MaxInt64)
		for i := 0; i < 7; i++ {
			start := time.Now()
			if _, err := Render(c.in, Options{Site: "mesh.example"}); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		times = append(times, best)
		switch {
		case best > budgetLimit:
			t.Errorf("%s: %v per card, limit %v (target %v)", c.name, best, budgetLimit, budgetTarget)
		case best > budgetTarget:
			t.Logf("%s: %v per card, over the %v target (limit %v)", c.name, best, budgetTarget, budgetLimit)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("per card: fastest %v, slowest %v (target %v, limit %v)", times[0], times[len(times)-1], budgetTarget, budgetLimit)
}

const (
	budgetTarget = 25 * time.Millisecond
	budgetLimit  = 2 * budgetTarget
)

func TestRenderBudgetHasHeadroom(t *testing.T) {
	if budgetLimit != 2*budgetTarget || budgetTarget > 25*time.Millisecond {
		t.Fatalf("target %v, limit %v; want the limit twice the target, the target no looser than the terrain card's 25ms", budgetTarget, budgetLimit)
	}
}

// CI enforces the budget: the non-race step in deploy.yml names it.
func TestBudgetRunsInCI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	if !strings.Contains(wf, "(cd internal/packetcard && go test -race ./...)") {
		t.Error("deploy.yml does not run internal/packetcard under -race")
	}
	if !regexp.MustCompile(`\(cd internal/packetcard && go test -count=1 -run '[^']*TestRenderWithinBudget`).MatchString(wf) {
		t.Error("deploy.yml has no non-race step running TestRenderWithinBudget for internal/packetcard")
	}
}

// Render is a pure function of its input.
func TestDeterministic(t *testing.T) {
	a, err := Render(fxFanout(30), Options{Site: "x"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Render(fxFanout(30), Options{Site: "x"})
	if !bytes.Equal(a, b) {
		t.Fatal("two renders of the same input differ")
	}
}

// Stats and Observers are exported for the share preview's text (#812 M2):
// they must be exactly what the card draws, tile for tile.
func TestStatsAreTheDrawnTiles(t *testing.T) {
	for name, in := range map[string]PacketCardInput{
		"flood": fxFlood(), "direct": fxDirect(), "fan-out": fxFanout(30), "advert": fxAdvert(), "no positions": fxNoPositions(),
	} {
		_, tr, err := render(in, Options{Site: "example.test"})
		if err != nil {
			t.Fatal(err)
		}
		var drawn []Stat
		for _, r := range tr.runs {
			switch r.Role {
			case "label":
				drawn = append(drawn, Stat{Label: r.Text})
			case "value":
				drawn[len(drawn)-1].Value = r.Text
			}
		}
		if got := Stats(in); !reflect.DeepEqual(got, drawn) {
			t.Errorf("%s: Stats %q, drawn %q", name, got, drawn)
		}
		if Stats(in)[0].Value != strconv.Itoa(Observers(in)) {
			t.Errorf("%s: the Observers tile %q is not Observers %d", name, Stats(in)[0].Value, Observers(in))
		}
	}
}
