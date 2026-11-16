package nodecard

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// #857: own scope, transported scopes, first seen, avg hops and the two
// estimated advert intervals, with the node page's wording.

func sp(s string) *string { return &s }

func fxRepeaterFull() NodeCardInput {
	in := fxRepeater()
	in.FirstSeen = ago(412 * 24 * time.Hour)
	in.AvgHops = ip(3)
	in.OwnScope, in.OwnScopeConfirmed = sp("#dk-midtjylland"), true
	in.TransportedScopes = []string{"#dk", "#dk-aarhus", "#dk-midtjylland"}
	in.FloodInterval = &AdvertInterval{Seconds: 47 * 3600, Confidence: "high", Snapped: true}
	in.ZeroHopInterval = &AdvertInterval{Seconds: 120 * 60, Confidence: "medium", Snapped: true}
	return in
}

const fullLabels = "Last heard, Relayed 24 h, Relayed 1 h, Seen airtime 24 h, Busiest hour, Neighbors, " +
	"First Seen, Avg Hops, Own scope, Transported scopes, Estimated flood interval, Estimated zero-hop interval"

func TestTiles857Order(t *testing.T) {
	if got := labels(tiles(fxRepeaterFull())); got != fullLabels {
		t.Errorf("repeater tiles\n %q\nwant\n %q", got, fullLabels)
	}
	room := fxRepeaterFull()
	room.Role = "room"
	if got := labels(tiles(room)); got != fullLabels {
		t.Errorf("room tiles %q", got)
	}
	// A sensor never shows transported scopes (the node page shows them
	// for repeaters and rooms only), even when it is passed some.
	sensor := fxRepeaterFull()
	sensor.Role, sensor.Relay = "sensor", nil
	want := "Last heard, Neighbors, First Seen, Avg Hops, Own scope, Estimated flood interval, Estimated zero-hop interval"
	if got := labels(tiles(sensor)); got != want {
		t.Errorf("sensor tiles %q, want %q", got, want)
	}
}

// Every new clause drops when its value is absent; nothing is ever drawn
// as 0 or "—".
func TestNew857FieldsAbsentDrop(t *testing.T) {
	cases := []struct {
		name string
		edit func(*NodeCardInput)
		gone string
	}{
		{"no first seen", func(in *NodeCardInput) { in.FirstSeen = time.Time{} }, "First Seen"},
		{"no avg hops (empty window)", func(in *NodeCardInput) { in.AvgHops = nil }, "Avg Hops"},
		{"avg hops 0 (the node page hides a falsy avgHops)", func(in *NodeCardInput) { in.AvgHops = ip(0) }, "Avg Hops"},
		{"negative avg hops", func(in *NodeCardInput) { in.AvgHops = ip(-2) }, "Avg Hops"},
		{"own scope NULL", func(in *NodeCardInput) { in.OwnScope = nil }, "Own scope"},
		{"own scope of control characters only", func(in *NodeCardInput) { in.OwnScope = sp("‮\x00") }, "Own scope"},
		{"no transported scopes", func(in *NodeCardInput) { in.TransportedScopes = nil }, "Transported scopes"},
		{"empty transported scopes", func(in *NodeCardInput) { in.TransportedScopes = []string{"", " "} }, "Transported scopes"},
		{"no flood interval", func(in *NodeCardInput) { in.FloodInterval = nil }, "Estimated flood interval"},
		{"low-confidence flood interval", func(in *NodeCardInput) { in.FloodInterval.Confidence = "low" }, "Estimated flood interval"},
		{"no-confidence flood interval", func(in *NodeCardInput) { in.FloodInterval.Confidence = "none" }, "Estimated flood interval"},
		{"zero flood interval", func(in *NodeCardInput) { in.FloodInterval.Seconds = 0 }, "Estimated flood interval"},
		{"no zero-hop interval", func(in *NodeCardInput) { in.ZeroHopInterval = nil }, "Estimated zero-hop interval"},
		{"low-confidence zero-hop interval", func(in *NodeCardInput) { in.ZeroHopInterval.Confidence = "low" }, "Estimated zero-hop interval"},
		{"negative zero-hop interval", func(in *NodeCardInput) { in.ZeroHopInterval.Seconds = -60 }, "Estimated zero-hop interval"},
	}
	for _, c := range cases {
		in := fxRepeaterFull()
		c.edit(&in)
		ts := tiles(in)
		if strings.Contains(labels(ts), c.gone) {
			t.Errorf("%s: %q still drawn: %s", c.name, c.gone, labels(ts))
		}
		if len(ts) != 11 {
			t.Errorf("%s: %d tiles, want the other 11", c.name, len(ts))
		}
		for _, tl := range ts {
			if tl.value == "0" && tl.label != "Relayed 1 h" || tl.value == "—" || tl.value == "" {
				t.Errorf("%s: %s drawn as %q", c.name, tl.label, tl.value)
			}
		}
	}
}

// Own scope follows the node page (nodes.js row-own-scope): NULL drops the
// row, "" is "unknown scope", anything else is the scope as stored; the
// confirmed mark is the detail line.
func TestOwnScopeClause(t *testing.T) {
	for _, c := range []struct {
		scope     string
		confirmed bool
		value     string
		detail    string
	}{
		{"#dk", false, "#dk", ""},
		{"#dk", true, "#dk", "confirmed"},
		{"", false, "unknown scope", ""},
		{"", true, "unknown scope", "confirmed"},
		{"*", false, "*", ""},
		{"  #dk‮ ", false, "#dk", ""},
	} {
		in := fxSensor()
		in.OwnScope, in.OwnScopeConfirmed = sp(c.scope), c.confirmed
		tl, ok := tileByLabel(tiles(in), "Own scope")
		if !ok || tl.value != c.value || tl.detail != c.detail {
			t.Errorf("scope %q confirmed %v: tile %+v (present %v), want %q / %q", c.scope, c.confirmed, tl, ok, c.value, c.detail)
		}
	}
}

// The interval text is node-adverts.js fmtInterval: flood in hours (minutes
// under 1 h), zero-hop in minutes, one decimal at most, after "≈"; the
// detail is the confidence, or the node page's note when the estimate is
// outside the settable range (both do not fit a tile; the note matters
// more, and the confidence is medium or high whenever a tile is drawn). The sample count is left out: it moves with
// every advert and would churn v (#857: bucket intervals).
func TestIntervalClause(t *testing.T) {
	for _, c := range []struct {
		flood  bool
		iv     AdvertInterval
		value  string
		detail string
	}{
		{true, AdvertInterval{47 * 3600, "high", true}, "≈ 47 h", "high confidence"},
		{true, AdvertInterval{5400, "medium", true}, "≈ 1.5 h", "medium confidence"},
		{true, AdvertInterval{2700, "high", false}, "≈ 45 min", "outside the settable 3–168 h"},
		{true, AdvertInterval{3*3600 + 179, "high", true}, "≈ 3 h", "high confidence"},
		{true, AdvertInterval{3*3600 + 180, "high", true}, "≈ 3.1 h", "high confidence"},
		{false, AdvertInterval{120 * 60, "medium", true}, "≈ 120 min", "medium confidence"},
		{false, AdvertInterval{120, "high", true}, "≈ 2 min", "high confidence"},
		{false, AdvertInterval{18030, "high", false}, "≈ 300.5 min", "outside the settable 60–240 min"},
	} {
		in := fxSensor()
		label := "Estimated zero-hop interval"
		iv := c.iv
		if c.flood {
			in.FloodInterval, label = &iv, "Estimated flood interval"
		} else {
			in.ZeroHopInterval = &iv
		}
		tl, ok := tileByLabel(tiles(in), label)
		if !ok || tl.value != c.value || tl.detail != c.detail {
			t.Errorf("%s %+v: tile %+v (present %v), want %q / %q", label, c.iv, tl, ok, c.value, c.detail)
		}
	}
}

// First seen is drawn like last heard: the timeAgo wording against AsOf,
// the absolute UTC time under it.
func TestFirstSeenClause(t *testing.T) {
	in := fxSensor()
	in.FirstSeen = ago(412*24*time.Hour + time.Hour)
	tl, ok := tileByLabel(tiles(in), "First Seen")
	if !ok || tl.value != "412d ago" || tl.detail != "2025-09-29 12:20 UTC" || !tl.absTime {
		t.Errorf("first seen tile %+v (present %v)", tl, ok)
	}
	if lh, _ := tileByLabel(tiles(in), "Last heard"); !lh.absTime {
		t.Error("last heard's detail is an absolute time too")
	}
	for _, l := range []string{"Own scope", "Avg Hops", "Estimated flood interval", "Neighbors"} {
		if tl, ok := tileByLabel(tiles(fxRepeaterFull()), l); !ok || tl.absTime {
			t.Errorf("%s: absTime %v (present %v)", l, tl.absTime, ok)
		}
	}
}

func TestAvgHopsClause(t *testing.T) {
	in := fxSensor()
	in.AvgHops = ip(4)
	if tl, ok := tileByLabel(tiles(in), "Avg Hops"); !ok || tl.value != "4" || tl.detail != "" {
		t.Errorf("avg hops tile %+v (present %v)", tl, ok)
	}
}

// The transported-scopes clause text lists at most three scopes, then
// "+N"; the card draws as many as fit (TestTransportedScopesDrawn).
func TestTransportedScopesClause(t *testing.T) {
	for _, c := range []struct {
		scopes []string
		value  string
	}{
		{[]string{"#dk"}, "#dk"},
		{[]string{"#dk", "#se"}, "#dk, #se"},
		{[]string{"#a", "#b", "#c"}, "#a, #b, #c"},
		{[]string{"#a", "#b", "#c", "#d", "#e"}, "#a, #b, #c +2"},
		{[]string{"#a", "", "#b\x00"}, "#a, #b"},
	} {
		in := fxRepeater()
		in.TransportedScopes = c.scopes
		tl, ok := tileByLabel(tiles(in), "Transported scopes")
		if !ok || tl.value != c.value {
			t.Errorf("%q: tile %+v (present %v), want %q", c.scopes, tl, ok, c.value)
		}
	}
}

// On the card a long scope list is cut to what fits, ending "+N" with N
// the scopes not drawn; a short one is drawn whole.
func TestTransportedScopesDrawn(t *testing.T) {
	in := fxRepeaterFull()
	many := []string{}
	for _, s := range []string{"dk", "dk-aarhus", "dk-midtjylland", "dk-nordjylland", "se", "no", "de-nord", "dk-fyn"} {
		many = append(many, "#"+s)
	}
	in.TransportedScopes = many
	_, runs, err := render(in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := drawnValue(t, runs, "Transported scopes")
	i := strings.LastIndex(v, " +")
	if i < 0 {
		t.Fatalf("long list drawn as %q, want a +N suffix", v)
	}
	shown := strings.Split(v[:i], ", ")
	if want := " +" + itoa(len(many)-len(shown)); v[i:] != want {
		t.Errorf("drawn %q: suffix %q, want %q", v, v[i:], want)
	}
	for k, s := range shown {
		if s != many[k] {
			t.Errorf("drawn %q: scope %d is %q, want %q", v, k, s, many[k])
		}
	}
	short := fxRepeaterFull()
	_, runs, _ = render(short, Options{})
	if v := drawnValue(t, runs, "Transported scopes"); v != "#dk, #dk-aarhus, #dk-midtjylland" && !strings.HasSuffix(v, " +1") {
		t.Errorf("three scopes drawn as %q", v)
	}
}

// One scope too long for its tile is cut with "…" and has no "+0" after
// it: there is no other scope to count (#861 review F2). Firmware region
// names run to 30 characters (RegionEntry.name[31]).
func TestTransportedScopeSingleLongNoPlusZero(t *testing.T) {
	in := fxRepeaterFull()
	in.TransportedScopes = []string{"#dk-sydjylland-vestkyst-esbjerg-nord"}
	_, runs, err := render(in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := drawnValue(t, runs, "Transported scopes")
	if strings.Contains(v, " +") || !strings.HasSuffix(v, "…") || !strings.HasPrefix(v, "#dk-") {
		t.Errorf("one long scope drawn as %q, want it cut with \"…\" and no \"+N\"", v)
	}
	// Two long scopes still count the one not drawn.
	in.TransportedScopes = append(in.TransportedScopes, "#dk-nordjylland-vendsyssel-hjoerring")
	_, runs, _ = render(in, Options{})
	if v := drawnValue(t, runs, "Transported scopes"); !strings.HasSuffix(v, "… +1") {
		t.Errorf("two long scopes drawn as %q, want a \"… +1\" suffix", v)
	}
}

// Clauses carries every scope the card may draw, not only the 3 the text
// names, so the server's v can hash them (#861 review F1).
func TestClauseCarriesDrawnScopes(t *testing.T) {
	in := fxRepeaterFull()
	in.TransportedScopes = []string{"#a", " ", "#b", "#c", "#d"}
	for _, cl := range Clauses(in) {
		if cl.Label == "Transported scopes" {
			if got := strings.Join(cl.Scopes, ","); got != "#a,#b,#c,#d" {
				t.Errorf("clause scopes %q, want the sanitised list", got)
			}
			return
		}
	}
	t.Fatal("no transported scopes clause")
}

// Up to six clauses keep the M2 layout (3 columns, two rows, large values),
// so a card without the new fields is unchanged; seven or eight use four
// columns on the same rows, more a 4×3 grid, and none is ever dropped for
// lack of room.
func TestLayoutDensity(t *testing.T) {
	_, runs, _ := render(fxRepeater(), Options{})
	if xs := labelXs(runs); len(xs) != 6 || xs[1] != safeMargin+tileW || xs[3] != safeMargin {
		t.Errorf("six clauses: label x %v, want the 3-column grid", xs)
	}
	_, runs, _ = render(fxRepeaterFull(), Options{})
	xs := labelXs(runs)
	if len(xs) != 12 {
		t.Fatalf("twelve clauses drew %d labels", len(xs))
	}
	if xs[1] != safeMargin+denseTileW || xs[4] != safeMargin || xs[8] != safeMargin {
		t.Errorf("twelve clauses: label x %v, want the 4-column grid", xs)
	}
	// Seven or eight clauses: four columns on the M2 grid's two rows.
	eight := fxRepeater()
	eight.FirstSeen, eight.AvgHops = ago(243*24*time.Hour), ip(8)
	_, mid, _ := render(eight, Options{})
	var ys []int
	for _, r := range mid {
		if r.Role == "label" {
			ys = append(ys, r.Y)
		}
	}
	if xs := labelXs(mid); len(xs) != 8 || xs[1] != safeMargin+denseTileW || xs[4] != safeMargin || ys[0] != tileTop || ys[4] != tileTop+tileRowH {
		t.Errorf("eight clauses: label x %v y %v, want 4 columns on the 2-row grid", xs, ys)
	}
	for n, want := range map[int]grid{0: wideGrid, 6: wideGrid, 7: midGrid, 8: midGrid, 9: denseGrid, 12: denseGrid} {
		if gridFor(n) != want {
			t.Errorf("gridFor(%d) = %+v", n, gridFor(n))
		}
	}
	for _, r := range runs {
		if r.Role == "value" && r.Size > denseValueSize {
			t.Errorf("dense value %q at %d px", r.Text, r.Size)
		}
		if (r.Role == "label" || r.Role == "value" || r.Role == "detail") && r.Y >= footerRule-8 {
			t.Errorf("%s %q at y=%d reaches the footer rule", r.Role, r.Text, r.Y)
		}
	}
}

// v (cmd/server) leaves out only the absolute-time details; the clause says
// which ones they are.
func TestClauseAbsTime(t *testing.T) {
	for _, c := range Clauses(fxRepeaterFull()) {
		want := c.Label == "Last heard" || c.Label == "First Seen"
		if c.AbsTime != want {
			t.Errorf("%s: AbsTime %v", c.Label, c.AbsTime)
		}
	}
}

func tileByLabel(ts []tile, label string) (tile, bool) {
	for _, t := range ts {
		if t.label == label {
			return t, true
		}
	}
	return tile{}, false
}

func labelXs(runs []textRun) []int {
	var xs []int
	for _, r := range runs {
		if r.Role == "label" {
			xs = append(xs, r.X)
		}
	}
	return xs
}

// drawnValue is the value run drawn under the label run with text label.
func drawnValue(t *testing.T, runs []textRun, label string) string {
	t.Helper()
	for i, r := range runs {
		if r.Role == "label" && r.Text == label && i+1 < len(runs) && runs[i+1].Role == "value" {
			return runs[i+1].Text
		}
	}
	t.Fatalf("no %q tile drawn", label)
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
