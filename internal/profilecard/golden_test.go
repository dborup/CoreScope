package profilecard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
// checked three ways: its decoded pixels must match exactly, its PNG file
// byte for byte, and its .runs.txt — every text run the card drew — line
// for line, so a failing case says which words moved before anyone has to
// open an image.
//
// The pixels are the same on every architecture and Go version (integer
// drawing, TestNoFusedMultiplyAdd). The PNG bytes are not: compress/flate
// changed between go1.26 and go1.27, same pixels, different stream. So
// testdata/golden/toolchain.txt records the Go version that wrote the
// goldens — CI's, from .github/workflows/deploy.yml — and the byte-exact
// comparison runs under that version; under any other the test says so
// and still compares every pixel. Regenerate with CI's toolchain:
//
//	GOTOOLCHAIN=go1.27.1 go test -run TestGolden -update
var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

type goldenCase struct {
	name  string
	resp  TerrainProfileResponse
	names [2]string
	opt   Options
	theme string // "" = shipped palette; "operator" = testdata/operator-theme.json
	// want are text fragments that must appear, by run role.
	want map[string]string
	// lightPNG also keeps a byte-exact PNG of the light theme. Every case
	// keeps its light text runs; the light PNGs are limited to keep the
	// committed goldens under about 1 MB (one card is ~80 KB).
	lightPNG bool
	// state pins the fixture's own verdict, so a fixture change that turns
	// "blocked" into "clear" fails here, not as a mysterious pixel diff.
	state string
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{name: "clear", resp: fxClear(), names: [2]string{"Hill Repeater", "Harbour Mast"}, state: "clear",
			want: map[string]string{"title": "Hill Repeater → Harbour Mast", "badge": "Terrain: Clear", "subtitle": "11.1 km"}},
		{name: "marginal-intrusion", resp: fxMarginal(), names: [2]string{"Ridge North", "Valley Node"}, state: "fresnel60", lightPNG: true,
			want: map[string]string{"badge": "Terrain: Marginal", "summary": "heights assumed"}},
		{name: "blocked", resp: fxBlocked(), names: [2]string{"West Tower", "East Tower"}, state: "blocked",
			want: map[string]string{"badge": "Blocked", "summary": "Worst clearance"}},
		{name: "partial-gap", resp: fxPartial(), names: [2]string{"Coast Relay", "Island Relay"}, state: "unknown",
			want: map[string]string{"badge": "No verdict", "summary": "path leaves Denmark"}},
		{name: "coverage-none", resp: fxNone(), names: [2]string{"Far Away", ""}, state: "unknown",
			want: map[string]string{"badge": "No verdict", "message": "Denmark only", "title": "Far Away → 55.1000, 10.1000"}},
		{name: "model-dtm", resp: fxDTM(), names: [2]string{"Field Node", "Church Mast"}, state: "clear",
			want: map[string]string{"badge": "Clear", "subtitle": "k = 1", "summary": "terrain only"}},
		{name: "short-300m", resp: fxShort(), state: "clear",
			want: map[string]string{"subtitle": "300 m", "title": "55.0000, 10.0000 → 55.1000, 10.1000"}},
		{name: "long-80km", resp: fxLong(), names: [2]string{"North Hub", "South Hub"}, state: "blocked",
			want: map[string]string{"subtitle": "80.0 km"}},
		{name: "danish-name", resp: fxClear(), names: [2]string{"Søby Bakker", "Ørsted Mast — Æblehaven"}, state: "clear",
			want: map[string]string{"title": "Søby Bakker → Ørsted Mast — Æblehaven"}},
		{name: "cjk-fallback", resp: fxClear(), names: [2]string{"东京中继站", "Mast 🛰"}, state: "clear",
			want: map[string]string{"title": "Node A → Mast"}},
		{name: "mixed-fill", resp: fxMixed(), names: [2]string{"Hill Repeater", "Harbour Mast"}, state: "clear",
			opt:  Options{Site: "mesh.example"},
			want: map[string]string{"attributionFill": "Copernicus DEM GLO-30", "attribution": "data bearbejdet", "site": "mesh.example"}},
		{name: "operator-palette", resp: fxMarginal(), names: [2]string{"Ridge North", "Valley Node"}, theme: "operator", state: "fresnel60",
			opt:  Options{Site: "mesh.example"},
			want: map[string]string{"site": "mesh.example"}},
	}
}

// operatorTheme is the operator-theme.json fixture: the two theme.json sections, as the server's
// LoadTheme would hand them over (string values only).
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

func paletteFor(t testing.TB, c goldenCase, dark bool) Palette {
	if c.theme != "operator" {
		return DefaultPalette(dark)
	}
	th := loadOperatorTheme(t)
	section := th.Theme
	if dark {
		section = th.ThemeDark
	}
	pal, _ := ResolvePalette(section, dark)
	return pal
}

func formatRuns(runs []textRun) string {
	var b strings.Builder
	for _, r := range runs {
		w := "regular"
		if r.Bold {
			w = "bold"
		}
		fmt.Fprintf(&b, "%-13s %2dpx %-7s x=%4d y=%3d w=%4d %s\n", r.Role, r.Size, w, r.X, r.Y, r.W, r.Text)
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

func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	b, err := encode(img)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// inputDigest fingerprints a fixture part by part, so a failing golden
// says whether the input or the drawing moved, and which part of the input
// (JSON prints floats exactly, shortest form).
func inputDigest(resp TerrainProfileResponse) string {
	d := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return err.Error()
		}
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:4])
	}
	dtm, dsm, dd := make([]any, len(resp.Samples)), make([]any, len(resp.Samples)), make([]float64, len(resp.Samples))
	for i, s := range resp.Samples {
		dtm[i], dsm[i], dd[i] = s.DTM, s.DSM, s.D
	}
	return fmt.Sprintf("all=%s n=%d d=%s dtm=%s dsm=%s ends=%s physics=%s verdict=%s",
		d(resp), len(resp.Samples), d(dd), d(dtm), d(dsm), d([2]TerrainEndpoint{resp.A, resp.B}), d(resp.Physics), d(resp.Verdict))
}

// samePixels decodes both PNGs and compares their RGBA bytes exactly.
func samePixels(a, b []byte) bool {
	ia, err1 := png.Decode(bytes.NewReader(a))
	ib, err2 := png.Decode(bytes.NewReader(b))
	if err1 != nil || err2 != nil || ia.Bounds() != ib.Bounds() {
		return false
	}
	ra, rb := image.NewRGBA(ia.Bounds()), image.NewRGBA(ib.Bounds())
	draw.Draw(ra, ra.Rect, ia, ia.Bounds().Min, draw.Src)
	draw.Draw(rb, rb.Rect, ib, ib.Bounds().Min, draw.Src)
	return bytes.Equal(ra.Pix, rb.Pix)
}

// pixelDiff says where two PNGs differ: a count, a bounding box, and a
// map of the card in 40 px cells ('#' = some pixel in the cell differs),
// so a CI log shows which layer moved without anyone fetching an image.
func pixelDiff(a, b []byte) string {
	ia, err1 := png.Decode(bytes.NewReader(a))
	ib, err2 := png.Decode(bytes.NewReader(b))
	if err1 != nil || err2 != nil {
		return fmt.Sprintf("decode: %v / %v", err1, err2)
	}
	if ia.Bounds() != ib.Bounds() {
		return fmt.Sprintf("bounds %v vs %v", ia.Bounds(), ib.Bounds())
	}
	const cell = 40
	r := ia.Bounds()
	grid := make([][]byte, (r.Dy()+cell-1)/cell)
	for i := range grid {
		grid[i] = bytes.Repeat([]byte{'.'}, (r.Dx()+cell-1)/cell)
	}
	n := 0
	box := image.Rectangle{}
	var first []string
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			ca, cb := ia.At(x, y), ib.At(x, y)
			if ca != cb {
				n++
				box = box.Union(image.Rect(x, y, x+1, y+1))
				grid[(y-r.Min.Y)/cell][(x-r.Min.X)/cell] = '#'
				if len(first) < 5 {
					first = append(first, fmt.Sprintf("(%d,%d) want %v got %v", x, y, ca, cb))
				}
			}
		}
	}
	var m strings.Builder
	for _, row := range grid {
		m.Write(row)
		m.WriteByte('\n')
	}
	return fmt.Sprintf("%d pixels differ within %v; first %s\n%s", n, box, strings.Join(first, ", "), m.String())
}

func TestGolden(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	toolchainFile := filepath.Join(dir, "toolchain.txt")
	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(toolchainFile, []byte(runtime.Version()+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tc, err := os.ReadFile(toolchainFile)
	if err != nil {
		t.Fatal(err)
	}
	exactBytes := strings.TrimSpace(string(tc)) == runtime.Version()
	if !exactBytes {
		t.Logf("goldens were written by %s, this is %s: comparing pixels exactly, PNG bytes not", strings.TrimSpace(string(tc)), runtime.Version())
	}
	for _, c := range goldenCases() {
		if c.state != "" && c.resp.Verdict.State != c.state {
			t.Fatalf("%s: fixture verdict is %q, the case expects %q", c.name, c.resp.Verdict.State, c.state)
		}
		for _, theme := range []string{"dark", "light"} {
			c, theme := c, theme
			t.Run(c.name+"-"+theme, func(t *testing.T) {
				img, runs, err := render(c.resp, c.names, paletteFor(t, c, theme == "dark"), c.opt)
				if err != nil {
					t.Fatal(err)
				}
				for role, want := range c.want {
					if got := runText(runs, role); !strings.Contains(got, want) {
						t.Errorf("%s text %q does not contain %q", role, got, want)
					}
				}
				got := encodePNG(t, img)
				gotRuns := formatRuns(runs)
				base := filepath.Join(dir, c.name+"-"+theme)
				keepPNG := theme == "dark" || c.lightPNG
				if *update {
					if !keepPNG {
						_ = os.Remove(base + ".png")
					} else if err := os.WriteFile(base+".png", got, 0o644); err != nil {
						t.Fatal(err)
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
				if !samePixels(want, got) {
					t.Errorf("pixels differ from %s.png (input digest %s): %s", base, inputDigest(c.resp), pixelDiff(want, got))
				} else if exactBytes && !bytes.Equal(got, want) {
					t.Errorf("PNG bytes differ from %s.png with identical pixels (same toolchain %s)", base, runtime.Version())
				}
			})
		}
	}
}
