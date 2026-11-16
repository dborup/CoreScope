package packetcard

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
)

// go test -run TestGolden -update rewrites testdata/golden/. Each case is
// checked three ways, as the node and terrain cards' goldens are: its
// decoded pixels exactly, its PNG byte for byte under the toolchain that
// wrote it (testdata/golden/toolchain.txt, CI's), and its .runs.txt, every
// text run the card drew, line for line. Regenerate with CI's toolchain:
//
//	GOTOOLCHAIN=go1.27.1 go test -run TestGolden -update
var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

type goldenCase struct {
	name     string
	in       PacketCardInput
	opt      Options
	operator bool // testdata/operator-theme.json instead of the shipped palette
	// want are text fragments that must appear, by run role; absent are
	// fragments no run of that role may contain ("" = no run of the role).
	want, absent map[string]string
	lightPNG     bool
}

func goldenCases() []goldenCase {
	over := fxFanout(MaxBranches + 72)
	long := fxAdvert()
	long.AdvertName = "DK_7800_Kjeldbjerg_Hilltop_North_Relay_Backbone_Node_Extended_Name"
	long.FirstObserverName = "Observer_With_A_Very_Long_Name_That_Goes_On_And_On_Forever"
	return []goldenCase{
		{name: "flood-areas", in: fxFlood(), opt: Options{Site: "mesh.example"}, lightPNG: true,
			want: map[string]string{"title": "GRP_TXT · Flood · 6 hops", "heard": "First heard by Aarhus-Obs · heard by 3 observers",
				"value": "≥1.2s", "note": "Path is a schematic; positions as advertised", "footer": "First heard 2026-11-15 13:20:41 UTC", "site": "mesh.example"}},
		{name: "direct", in: fxDirect(),
			want:   map[string]string{"title": "TXT_MSG · Direct · 2 hops", "heard": "First heard by Randers Obs · heard by 1 observer", "value": "≥312ms"},
			absent: map[string]string{"label": "Spread"}},
		{name: "fanout-30", in: fxFanout(30),
			want: map[string]string{"heard": "heard by 30 observers", "value": "26.1s"}, absent: map[string]string{"more": ""}},
		{name: "advert", in: fxAdvert(),
			want: map[string]string{"title": "ADVERT · Flood · 1 hop", "heard": "Advert from DK_7800_Kjeldbjerg · first heard by Aarhus-Obs"}},
		{name: "no-positions", in: fxNoPositions(),
			want:   map[string]string{"empty": "No station on this path has an advertised position", "heard": "heard by 3 observers"},
			absent: map[string]string{"note": ""}},
		{name: "single-point", in: fxSinglePoint(),
			want: map[string]string{"title": "ADVERT · Flood · 0 hops", "note": "schematic"}},
		{name: "hidden-first", in: fxHiddenFirst(),
			want:   map[string]string{"heard": "Heard by 2 observers", "label": "Observers"},
			absent: map[string]string{"heard": "Aarhus-Obs", "label": "Spread"}},
		{name: "over-bounds", in: over, want: map[string]string{"more": "+72 more not drawn", "heard": "heard by 200 observers"}},
		{name: "long-names", in: long, want: map[string]string{"heard": "…"}},
		{name: "hidden-names", in: fxAdvert(), opt: Options{HideNames: true},
			want: map[string]string{"heard": "Heard by 1 observer"}, absent: map[string]string{"heard": "Kjeldbjerg"}},
		{name: "operator-palette", in: fxFlood(), operator: true, opt: Options{Site: "mesh.example"},
			want: map[string]string{"site": "mesh.example"}},
	}
}

type operatorTheme struct {
	Theme     map[string]string `json:"theme"`
	ThemeDark map[string]string `json:"themeDark"`
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
		pal, bad = ResolveTheme(th.Theme, th.ThemeDark, dark)
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
				img, tr, err := render(c.in, optFor(t, c, theme == "dark"))
				if err != nil {
					t.Fatal(err)
				}
				runs := tr.runs
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
