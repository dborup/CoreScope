package nodecard

import (
	"bytes"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func nanF() float64 { return math.NaN() }

var coordRe = regexp.MustCompile(`-?\d{1,3}\.\d{3,}\s*,\s*-?\d{1,3}\.\d{3,}`)

// NodeCardInput carries no position, map or packet field: the card cannot
// draw what it is never given. A field added with such a name fails here.
func TestInputHasNoPositionOrPacketFields(t *testing.T) {
	bad := regexp.MustCompile(`(?i)lat|lon|coord|pos|map|tile|packet|payload|raw|hex|path`)
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if bad.MatchString(f.Name) {
				t.Errorf("%s.%s: the node card takes no position, map or packet input", rt.Name(), f.Name)
			}
			if f.Type.PkgPath() == "" || strings.HasSuffix(f.Type.PkgPath(), "nodecard") {
				walk(f.Type)
			}
		}
	}
	walk(reflect.TypeOf(NodeCardInput{}))
}

// Every text run stays inside the card's safe area (24 px from each edge,
// which Facebook and LinkedIn may crop), whatever the name.
func TestRunsInsideSafeArea(t *testing.T) {
	for _, c := range goldenCases() {
		_, runs, err := render(c.in, Options{Site: "a-very-long-site-name.example.org"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.X < safeMargin || r.X+r.W > Width-safeMargin || r.Y > Height-safeMargin {
				t.Errorf("%s: %s run %q at x=%d w=%d y=%d leaves the safe area", c.name, r.Role, r.Text, r.X, r.W, r.Y)
			}
		}
		// Runs in one row never overlap.
		rows := map[int][]textRun{}
		for _, r := range runs {
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

// The long name shrinks, then is shortened with "…" at a rune boundary; the
// title never runs into the badge.
func TestLongNameTruncated(t *testing.T) {
	in := fxRepeater()
	in.Name = strings.Repeat("W", 200)
	_, runs, err := render(in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var title, badge textRun
	for _, r := range runs {
		switch r.Role {
		case "title":
			title = r
		case "badge":
			badge = r
		}
	}
	if !strings.HasSuffix(title.Text, "…") || title.Size != minTitleSize || !utf8.ValidString(title.Text) {
		t.Errorf("title %+v", title)
	}
	if title.X+title.W > badge.X-badgePad-safeMargin {
		t.Errorf("title (to %d) runs into the badge (from %d)", title.X+title.W, badge.X-badgePad)
	}
}

// Every card is 1200×630 and under the size bound: 64 KiB for a card of up
// to six clauses (#837: below a terrain card, ~80 KB, so the ~64 cached
// node cards of #811 M3 stay under 4 MiB), and maxDenseCardBytes for the
// dense grid of #857, still under the server's 128 KiB per cached card
// (#755, cardBytesPerEntry), which bounds the cache's memory either way.
func TestCardSizeBound(t *testing.T) {
	largest, largestDense := 0, 0
	for _, c := range goldenCases() {
		dense := len(tiles(c.in)) > 2*tileCols
		bound := maxCardBytes
		if dense {
			bound = maxDenseCardBytes
		}
		for _, dark := range []bool{true, false} {
			out, err := Render(c.in, optFor(t, c, dark))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(out))
			if err != nil || cfg.Width != Width || cfg.Height != Height {
				t.Fatalf("%s: %dx%d %v", c.name, cfg.Width, cfg.Height, err)
			}
			if dense {
				largestDense = max(largestDense, len(out))
			} else {
				largest = max(largest, len(out))
			}
			if len(out) > bound {
				t.Errorf("%s: %d bytes, bound %d", c.name, len(out), bound)
			}
		}
	}
	t.Logf("largest card %d bytes (bound %d), largest dense card %d bytes (bound %d)", largest, maxCardBytes, largestDense, maxDenseCardBytes)
}

const (
	maxCardBytes      = 64 << 10
	maxDenseCardBytes = 96 << 10
)

// The per-card budget, the mechanism of #755: the target is logged
// against, the test fails at twice it, so a loaded shared runner cannot
// fail it while a renderer doing real extra work still does. Skipped under
// -race; CI runs it once more without (deploy.yml).
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
	budgetTarget = 15 * time.Millisecond
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
	if !strings.Contains(wf, "(cd internal/nodecard && go test -race ./...)") {
		t.Error("deploy.yml does not run internal/nodecard under -race")
	}
	if !regexp.MustCompile(`\(cd internal/nodecard && go test -count=1 -run '[^']*TestRenderWithinBudget`).MatchString(wf) {
		t.Error("deploy.yml has no non-race step running TestRenderWithinBudget for internal/nodecard")
	}
}

// Render is a pure function of its input: the same bytes twice, and the
// wall clock does not enter (only AsOf does).
func TestDeterministic(t *testing.T) {
	a, err := Render(fxRepeater(), Options{Site: "x"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	b, _ := Render(fxRepeater(), Options{Site: "x"})
	if !bytes.Equal(a, b) {
		t.Fatal("two renders of the same input differ")
	}
}
