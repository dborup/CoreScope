package nodecard

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func labels(ts []tile) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.label)
	}
	return strings.Join(s, ", ")
}

// The clauses depend on the role (#811 M2): every role has last heard and
// neighbours; only repeaters and rooms have the relay block.
func TestTilesPerRole(t *testing.T) {
	full := "Last heard, Relayed 24 h, Relayed 1 h, Seen airtime 24 h, Busiest hour, Neighbors"
	for _, c := range []struct {
		in   NodeCardInput
		want string
	}{
		{fxRepeater(), full},
		{fxRoom(), full},
		{fxSensor(), "Last heard, Neighbors"},
	} {
		if got := labels(tiles(c.in)); got != c.want {
			t.Errorf("%s: tiles %q, want %q", c.in.Role, got, c.want)
		}
	}
}

// An absent value drops its clause; it is never drawn as 0. A present 0 is
// drawn: it is a real count.
func TestAbsentDropsNeverZero(t *testing.T) {
	cases := []struct {
		name string
		edit func(*NodeCardInput)
		gone string
	}{
		{"no last heard", func(in *NodeCardInput) { in.LastHeard = time.Time{} }, "Last heard"},
		{"no neighbours", func(in *NodeCardInput) { in.Neighbors = nil }, "Neighbors"},
		{"no 24 h relays", func(in *NodeCardInput) { in.Relay.Relayed24h = nil }, "Relayed 24 h"},
		{"no 1 h relays", func(in *NodeCardInput) { in.Relay.Relayed1h = nil }, "Relayed 1 h"},
		{"no airtime", func(in *NodeCardInput) { in.Relay.AirtimePct24h = nil }, "Seen airtime 24 h"},
		{"no peak", func(in *NodeCardInput) { in.Relay.AirtimePeakPct = nil }, "Busiest hour"},
		{"negative count", func(in *NodeCardInput) { in.Neighbors = ip(-1) }, "Neighbors"},
		{"NaN airtime", func(in *NodeCardInput) { in.Relay.AirtimePct24h = fp(nanF()) }, "Seen airtime 24 h"},
		{"negative peak", func(in *NodeCardInput) { in.Relay.AirtimePeakPct = fp(-0.5) }, "Busiest hour"},
	}
	for _, c := range cases {
		in := fxRepeater()
		c.edit(&in)
		ts := tiles(in)
		if strings.Contains(labels(ts), c.gone) {
			t.Errorf("%s: %q still drawn: %s", c.name, c.gone, labels(ts))
		}
		if len(ts) != 5 {
			t.Errorf("%s: %d tiles, want the other 5", c.name, len(ts))
		}
		for _, tl := range ts {
			if tl.value == "0" && (tl.label != "Relayed 1 h" || in.Relay.Relayed1h == nil) {
				t.Errorf("%s: %s drawn as 0", c.name, tl.label)
			}
		}
	}
	in := fxRepeater()
	in.Relay = nil
	if got := labels(tiles(in)); got != "Last heard, Neighbors" {
		t.Errorf("missing relay block: %q", got)
	}
	in = fxRepeater()
	in.Relay.Relayed1h = ip(0)
	if v := tileValue(tiles(in), "Relayed 1 h"); v != "0" {
		t.Errorf("a present 0 must be drawn as 0, got %q", v)
	}
}

func tileValue(ts []tile, label string) string {
	for _, t := range ts {
		if t.label == label {
			return t.value
		}
	}
	return "<absent>"
}

func tileOf(ts []tile, label string) tile {
	for _, t := range ts {
		if t.label == label {
			return t
		}
	}
	return tile{}
}

// Seen airtime is a lower bound, drawn with "≥" as the busiest-repeaters
// table draws it (analytics.js _busiestAirtimeCell), and flagged with the
// rule of ObserverAirtime.isOver at the default 10 % limit: compared at the
// one decimal shown, so "10.0%" is never unflagged.
func TestAirtimeClause(t *testing.T) {
	for _, c := range []struct {
		pct  float64
		text string
		over bool
	}{
		{0, "0.0%", false},
		{0.04, "<0.1%", false},
		{0.1, "≥0.1%", false},
		{3.24, "≥3.2%", false},
		{0.25, "≥0.3%", false}, // JS toFixed rounds an exact tie up
		{9.94, "≥9.9%", false},
		{0.15, "≥0.1%", false}, // the exact value is below the tie
		{0.75, "≥0.8%", false},
		// JS quirk, mirrored: toFixed(1) of 9.95 (exactly 9.9499…) is
		// "9.9", while isOver's Math.round(9.95*10) sees 99.5 and flags it.
		{9.95, "≥9.9%", true},
		{9.96, "≥10.0%", true},
		{10, "≥10.0%", true},
		{57.3, "≥57.3%", true},
	} {
		if got := airtimeText(c.pct); got != c.text {
			t.Errorf("airtimeText(%v) = %q, want %q", c.pct, got, c.text)
		}
		if got := airtimeOver(c.pct); got != c.over {
			t.Errorf("airtimeOver(%v) = %v, want %v", c.pct, got, c.over)
		}
	}
	in := fxRepeater()
	in.Relay.AirtimePeakPct = fp(12.5)
	if tl := tileOf(tiles(in), "Busiest hour"); !tl.over || !strings.Contains(tl.detail, "10% limit") {
		t.Errorf("busiest hour over the limit: %+v", tl)
	}
	if tl := tileOf(tiles(in), "Seen airtime 24 h"); tl.over || tl.detail != "lower bound" {
		t.Errorf("24 h airtime under the limit: %+v", tl)
	}
}

// RelativeTime is the Go twin of public/app.js timeAgo, against AsOf.
func TestRelativeTime(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s ago"},
		{59*time.Second + 999*time.Millisecond, "59s ago"},
		{60 * time.Second, "1m ago"},
		{59 * time.Minute, "59m ago"},
		{3*time.Hour + 12*time.Minute, "3h ago"},
		{23*time.Hour + 59*time.Minute, "23h ago"},
		{24 * time.Hour, "1d ago"},
		{400 * 24 * time.Hour, "400d ago"},
		{-500 * time.Millisecond, "in 1s"}, // Math.floor of a negative fraction
		{-2 * time.Hour, "in 2h"},
	} {
		if got := RelativeTime(asOf.Add(-c.d), asOf); got != c.want {
			t.Errorf("RelativeTime(-%v) = %q, want %q", c.d, got, c.want)
		}
	}
	// Extremes saturate instead of overflowing.
	if got := RelativeTime(time.Unix(-1<<62, 0), time.Unix(1<<62, 0)); !strings.HasSuffix(got, "d ago") {
		t.Errorf("extreme past: %q", got)
	}
	if got := RelativeTime(time.Unix(1<<62, 0), time.Unix(-1<<62, 0)); !strings.HasPrefix(got, "in ") {
		t.Errorf("extreme future: %q", got)
	}
}

// Last heard shows the relative time and the absolute UTC time.
func TestLastHeardTile(t *testing.T) {
	in := fxRepeater()
	in.LastHeard = time.Date(2026, 11, 15, 11, 8, 59, 0, time.FixedZone("CET", 3600))
	tl := tileOf(tiles(in), "Last heard")
	if tl.value != "3h ago" || tl.detail != "2026-11-15 10:08 UTC" {
		t.Errorf("last heard tile %+v", tl)
	}
}

func TestCounts(t *testing.T) {
	for v, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := fmtCount(v); got != want {
			t.Errorf("fmtCount(%d) = %q, want %q", v, got, want)
		}
	}
}

// Owner decision Q3: cards for repeaters, rooms and sensors only. A
// companion, an observer or an unknown role gets no card at all.
func TestRoleGate(t *testing.T) {
	for role, want := range map[string]bool{"repeater": true, "room": true, "sensor": true,
		"companion": false, "observer": false, "": false, "Repeater": false, "room_server": false} {
		if Carded(role) != want {
			t.Errorf("Carded(%q) = %v", role, !want)
		}
		in := fxRepeater()
		in.Role = role
		out, err := Render(in, Options{})
		if want && (err != nil || len(out) == 0) {
			t.Errorf("%q: %v", role, err)
		}
		if !want && (!errors.Is(err, ErrRoleNotCarded) || out != nil) {
			t.Errorf("%q: got %d bytes, err %v; want ErrRoleNotCarded", role, len(out), err)
		}
	}
	if out, err := Render(fxCompanion(), Options{}); !errors.Is(err, ErrRoleNotCarded) || out != nil {
		t.Errorf("companion: %d bytes, %v", len(out), err)
	}
}

func TestAsOfRequired(t *testing.T) {
	in := fxRepeater()
	in.AsOf = time.Time{}
	if _, err := Render(in, Options{}); !errors.Is(err, ErrNoAsOf) {
		t.Fatalf("zero AsOf: %v", err)
	}
}

// The footer: "as of … UTC" on the left, whatever zone AsOf is in, and the
// site name on the right.
func TestFooter(t *testing.T) {
	in := fxRepeater()
	in.AsOf = asOf.In(time.FixedZone("CET", 3600))
	_, runs, err := render(in, Options{Site: "  mesh.example‮ "})
	if err != nil {
		t.Fatal(err)
	}
	if got := runText(runs, "asof"); got != "as of 2026-11-15 13:20 UTC" {
		t.Errorf("as-of %q", got)
	}
	if got := runText(runs, "site"); got != "mesh.example" {
		t.Errorf("site %q", got)
	}
	_, runs, _ = render(fxRepeater(), Options{})
	if hasRole(runs, "site") {
		t.Error("no site configured, but a site run was drawn")
	}
}

// The fallback title never shows anything but the first 8 hex characters
// of the key, and nothing at all from a key that is not hex.
func TestFallbackTitle(t *testing.T) {
	for key, want := range map[string]string{
		fxKey: "Node 3f2a91c0", strings.ToUpper(fxKey): "Node 3f2a91c0",
		"3f2a": "Node", "": "Node", "<script>abcdefgh": "Node", "zz2a91c0d4": "Node",
	} {
		if got := fallbackTitle(key); got != want {
			t.Errorf("fallbackTitle(%q) = %q, want %q", key, got, want)
		}
	}
}

// Nothing positional reaches the card: the input has no field for it, and
// no drawn text looks like a coordinate.
func TestNoCoordinatesOrPackets(t *testing.T) {
	for _, c := range goldenCases() {
		_, runs, err := render(c.in, Options{Site: "mesh.example"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if coordRe.MatchString(r.Text) {
				t.Errorf("%s: %s run %q looks like a coordinate", c.name, r.Role, r.Text)
			}
		}
	}
}
