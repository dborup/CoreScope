package nodecard

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// go test -run TestGolden -update rewrites testdata/golden/. Each case is
// checked three ways, as the terrain card's goldens are: its decoded
// pixels exactly, its PNG byte for byte under the toolchain that wrote it
// (testdata/golden/toolchain.txt, CI's; compress/flate output differs
// between Go versions), and its .runs.txt, every text run the card drew,
// line for line. Regenerate with CI's toolchain:
//
//	GOTOOLCHAIN=go1.27.1 go test -run TestGolden -update
var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

type goldenCase struct {
	name     string
	in       NodeCardInput
	opt      Options
	operator bool // testdata/operator-theme.json instead of the shipped palette
	// want are text fragments that must appear, by run role; absent are
	// fragments no run of that role may contain.
	want, absent map[string]string
	lightPNG     bool
}

func goldenCases() []goldenCase {
	long := fxRepeater()
	long.Name = "DK_7800_Kjeldbjerg_Hilltop_North_Relay_Backbone_Node_Extended_Name"
	rtl := fxRepeater()
	rtl.Name = "ממסר תל אביב"
	emoji := fxRoom()
	emoji.Name = "Mast 🛰 Vest"
	glyph := fxRepeater()
	glyph.Name = "东京中继站"
	noRelay := fxRepeater()
	noRelay.Relay = nil
	over := fxRepeater()
	over.Relay = &RelayStats{Relayed24h: ip(48211), AirtimePct24h: fp(4.5), AirtimePeakPct: fp(9.96)}
	sparse := fxRepeater()
	sparse.Area, sparse.Neighbors, sparse.LastHeard = "", nil, time.Time{}
	sparse.Relay = &RelayStats{Relayed1h: ip(2)}
	// #857: the densest card (all twelve clauses) with a long name and a
	// scope list too long for its tile.
	dense := fxRepeaterFull()
	dense.Name = long.Name
	dense.TransportedScopes = []string{"#dk", "#dk-aarhus", "#dk-midtjylland", "#dk-nordjylland", "#se-skane", "#de-nord", "#dk-fyn"}
	dense.Relay.AirtimePeakPct = fp(12.5)
	// The common #857 card: a repeater whose only new values are first seen
	// and avg hops (no scopes known, no confident interval).
	eight := fxRepeater()
	eight.FirstSeen, eight.AvgHops = ago(243*24*time.Hour+5*time.Hour), ip(8)
	return []goldenCase{
		{name: "repeater-full", in: fxRepeaterFull(), opt: Options{Site: "mesh.example"}, lightPNG: true,
			want: map[string]string{"label": "First seen | Avg Hops | Own scope | Transported scopes | Estimated flood interval | Estimated zero-hop interval",
				"value": "≈ 47 h | ≈ 120 min", "detail": "confirmed | high confidence | medium confidence"}},
		{name: "room-full", in: fxRoomFull(),
			want:   map[string]string{"badge": "Room Server", "value": "unknown scope | #dk-aarhus | ≈ 12 h"},
			absent: map[string]string{"label": "zero-hop"}},
		{name: "sensor-full", in: fxSensorFull(),
			want:   map[string]string{"value": "* | ≈ 60.5 min", "detail": "outside the settable 60–240 min"},
			absent: map[string]string{"label": "Transported"}},
		{name: "dense-long-name", in: dense, opt: Options{Site: "mesh.example"},
			want: map[string]string{"title": "…", "value": " +", "detail": "at or over the 10% limit"}},
		{name: "repeater-eight", in: eight, opt: Options{Site: "mesh.example"},
			want: map[string]string{"label": "First seen | Avg Hops", "value": "243d ago"}},
		{name: "repeater", in: fxRepeater(), opt: Options{Site: "mesh.example"}, lightPNG: true,
			want: map[string]string{"title": "DK_7800_Kjeldbjerg", "badge": "Repeater", "area": "Midtjylland",
				"value": "1,234", "asof": "as of 2026-11-15 13:20 UTC", "site": "mesh.example"}},
		{name: "room", in: fxRoom(),
			want: map[string]string{"badge": "Room Server", "label": "Relayed 1 h", "value": "<0.1%"}},
		{name: "sensor", in: fxSensor(),
			want:   map[string]string{"badge": "Sensor", "value": "2d ago", "label": "Neighbors"},
			absent: map[string]string{"label": "Relayed"}},
		{name: "repeater-no-relay", in: noRelay,
			want:   map[string]string{"label": "Last heard | Neighbors"},
			absent: map[string]string{"label": "Relayed", "value": "≥"}},
		{name: "airtime-over", in: over,
			want:   map[string]string{"value": "≥10.0%", "detail": "at or over the 10% limit"},
			absent: map[string]string{"label": "Relayed 1 h"}},
		{name: "sparse", in: sparse,
			want:   map[string]string{"label": "Relayed 1 h"},
			absent: map[string]string{"label": "Last heard", "area": "", "value": "0"}},
		{name: "long-name", in: long, want: map[string]string{"title": "…"}},
		{name: "rtl-name", in: rtl, want: map[string]string{"title": "Node 3f2a91c0"}},
		{name: "emoji-name", in: emoji, want: map[string]string{"title": "Mast Vest"}},
		{name: "glyph-fail-name", in: glyph, want: map[string]string{"title": "Node 3f2a91c0"}},
		{name: "hidden-name", in: fxRepeater(), opt: Options{HideNames: true},
			want: map[string]string{"title": "Node 3f2a91c0"}, absent: map[string]string{"title": "Kjeldbjerg"}},
		{name: "operator-palette", in: fxRepeater(), operator: true, opt: Options{Site: "mesh.example"},
			want: map[string]string{"site": "mesh.example"}},
	}
}

type operatorTheme struct {
	Theme      map[string]string `json:"theme"`
	ThemeDark  map[string]string `json:"themeDark"`
	NodeColors map[string]string `json:"nodeColors"`
}

func loadOperatorTheme(t testing.TB) operatorTheme {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "operator-theme.json"))
	if err != nil {
		t.Fatal(err)
	}
	var th operatorTheme
	if err := json.Unmarshal(b, &th); err != nil {
		t.Fatal(err)
	}
	return th
}

func optFor(t testing.TB, c goldenCase, dark bool) Options {
	opt := c.opt
	pal := DefaultPalette(dark)
	if c.operator {
		th := loadOperatorTheme(t)
		var bad []string
		pal, bad = ResolveTheme(th.Theme, th.ThemeDark, th.NodeColors, dark)
		if len(bad) != 0 {
			t.Fatalf("operator theme has bad keys %v", bad)
		}
	}
	opt.Palette = &pal
	return opt
}

func formatRuns(runs []textRun) string {
	var b strings.Builder
	for _, r := range runs {
		w := "regular"
		if r.Bold {
			w = "bold"
		}
		fmt.Fprintf(&b, "%-7s %2dpx %-7s x=%4d y=%3d w=%4d %s\n", r.Role, r.Size, w, r.X, r.Y, r.W, r.Text)
	}
	return b.String()
}

func runText(runs []textRun, role string) string {
	var parts []string
	for _, r := range runs {
		if r.Role == role {
			parts = append(parts, r.Text)
		}
	}
	return strings.Join(parts, " | ")
}

func hasRole(runs []textRun, role string) bool {
	for _, r := range runs {
		if r.Role == role {
			return true
		}
	}
	return false
}

func toRGBA(t testing.TB, b []byte) *image.RGBA {
	t.Helper()
	im, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out := image.NewRGBA(im.Bounds())
	draw.Draw(out, out.Rect, im, im.Bounds().Min, draw.Src)
	return out
}

// pixelDiff is a count and bounding box of the differing pixels.
func pixelDiff(a, b *image.RGBA) string {
	if a.Rect != b.Rect {
		return fmt.Sprintf("bounds %v vs %v", a.Rect, b.Rect)
	}
	n, box := 0, image.Rectangle{}
	for y := a.Rect.Min.Y; y < a.Rect.Max.Y; y++ {
		for x := a.Rect.Min.X; x < a.Rect.Max.X; x++ {
			if a.RGBAAt(x, y) != b.RGBAAt(x, y) {
				n++
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return fmt.Sprintf("%d pixels differ within %v", n, box)
}

func TestGolden(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	tcFile := filepath.Join(dir, "toolchain.txt")
	if *update {
		if err := os.WriteFile(tcFile, []byte(runtime.Version()+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tc, err := os.ReadFile(tcFile)
	if err != nil {
		t.Fatal(err)
	}
	exactBytes := strings.TrimSpace(string(tc)) == runtime.Version()
	if !exactBytes {
		t.Logf("goldens were written by %s, this is %s: comparing pixels exactly, PNG bytes not", strings.TrimSpace(string(tc)), runtime.Version())
	}
	for _, c := range goldenCases() {
		for _, theme := range []string{"dark", "light"} {
			c, theme := c, theme
			t.Run(c.name+"-"+theme, func(t *testing.T) {
				img, runs, err := render(c.in, optFor(t, c, theme == "dark"))
				if err != nil {
					t.Fatal(err)
				}
				for role, want := range c.want {
					if got := runText(runs, role); !strings.Contains(got, want) {
						t.Errorf("%s text %q does not contain %q", role, got, want)
					}
				}
				for role, no := range c.absent {
					got := runText(runs, role)
					if no == "" && hasRole(runs, role) || no != "" && strings.Contains(got, no) {
						t.Errorf("%s text %q must not contain %q", role, got, no)
					}
				}
				got, err := encode(img)
				if err != nil {
					t.Fatal(err)
				}
				gotRuns := formatRuns(runs)
				base := filepath.Join(dir, c.name+"-"+theme)
				keepPNG := theme == "dark" || c.lightPNG
				if *update {
					if keepPNG {
						if err := os.WriteFile(base+".png", got, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(base+".runs.txt", []byte(gotRuns), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				wantRuns, err := os.ReadFile(base + ".runs.txt")
				if err != nil {
					t.Fatalf("%v (run with -update to create it)", err)
				}
				if gotRuns != string(wantRuns) {
					t.Errorf("text runs differ:\n--- want\n%s--- got\n%s", wantRuns, gotRuns)
				}
				if !keepPNG {
					return
				}
				want, err := os.ReadFile(base + ".png")
				if err != nil {
					t.Fatalf("%v (run with -update to create it)", err)
				}
				if wi, gi := toRGBA(t, want), toRGBA(t, got); !bytes.Equal(wi.Pix, gi.Pix) {
					t.Errorf("pixels differ from %s.png: %s", base, pixelDiff(wi, gi))
				} else if exactBytes && !bytes.Equal(got, want) {
					t.Errorf("PNG bytes differ from %s.png with identical pixels (same toolchain %s)", base, runtime.Version())
				}
			})
		}
	}
}
