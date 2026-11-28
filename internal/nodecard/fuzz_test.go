package nodecard

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// FuzzRender drives Render with arbitrary names, keys, areas, site names
// and numbers: counts of any sign and size, airtimes from any float64 bit
// pattern (NaN, ±Inf, subnormals), times far outside any calendar. For a
// carded role with an AsOf it must return a 1200×630 card whose every run
// stays in the safe area and whose numbers are never negative or "NaN";
// otherwise exactly ErrRoleNotCarded or ErrNoAsOf. CI runs the seeds;
// run longer with
//
//	go test -run '^$' -fuzz FuzzRender -fuzztime 60s
func FuzzRender(f *testing.F) {
	f.Add("DK_7800_Kjeldbjerg", fxKey, "Midtjylland", "mesh.example", uint8(0), int64(1234), int64(56), int64(12), 3.24, 8.1, int64(3600), int64(1_763_212_841))
	f.Add("", "", "", "", uint8(255), int64(0), int64(0), int64(0), 0.0, 0.0, int64(0), int64(0))
	f.Add("‮\x00東京🛰", "zz", "ממסר", "\n", uint8(7), int64(-1), int64(math.MinInt64), int64(math.MaxInt64), math.NaN(), math.Inf(1), int64(math.MinInt64), int64(math.MaxInt64))
	f.Add(strings.Repeat("Ω", 300), strings.Repeat("f", 64), strings.Repeat("Å", 200), strings.Repeat("x", 500), uint8(1), int64(1e15), int64(1), int64(1<<40), 1e300, 9.95, int64(-86400), int64(-62135596800))
	f.Add("áb̈c", fxKey, " ", "x", uint8(2), int64(7), int64(-3), int64(1), 0.04, -1.0, int64(1), int64(253402300799))

	roles := []string{"repeater", "room", "sensor", "companion", "observer", ""}
	f.Fuzz(func(t *testing.T, name, key, area, site string, flags uint8, r24, r1, nb int64, air, peak float64, agoSec, asOfSec int64) {
		in := NodeCardInput{
			PubKey: key, Name: name, Role: roles[int(flags)%len(roles)], Area: area,
			AsOf: time.Unix(asOfSec, 0),
		}
		if flags&8 == 0 {
			in.LastHeard = in.AsOf.Add(-time.Duration(agoSec) * time.Second)
		}
		if flags&16 == 0 {
			a, b := int(r24), int(r1)
			in.Relay = &RelayStats{Relayed24h: &a, Relayed1h: &b, AirtimePct24h: &air, AirtimePeakPct: &peak}
		}
		if flags&32 == 0 {
			n := int(nb)
			in.Neighbors = &n
		}
		if flags&64 != 0 {
			in.AsOf = time.Time{}
		}
		img, runs, err := render(in, Options{Site: site, HideNames: flags&128 != 0})
		switch {
		case !Carded(in.Role):
			if !errors.Is(err, ErrRoleNotCarded) {
				t.Fatalf("role %q: %v", in.Role, err)
			}
			return
		case in.AsOf.IsZero():
			if !errors.Is(err, ErrNoAsOf) {
				t.Fatalf("zero AsOf: %v", err)
			}
			return
		case err != nil:
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != Width || b.Dy() != Height {
			t.Fatalf("bounds %v", b)
		}
		sawTitle, sawAsOf := false, false
		for _, r := range runs {
			sawTitle = sawTitle || r.Role == "title" && r.Text != ""
			sawAsOf = sawAsOf || r.Role == "asof" && strings.HasPrefix(r.Text, "as of ")
			if r.X < safeMargin || r.X+r.W > Width-safeMargin {
				t.Fatalf("%s run %q at x=%d w=%d leaves the safe area", r.Role, r.Text, r.X, r.W)
			}
			if r.Role == "value" && (strings.HasPrefix(r.Text, "-") || strings.Contains(r.Text, "NaN") || strings.Contains(r.Text, "Inf")) {
				t.Fatalf("value %q", r.Text)
			}
		}
		if !sawTitle || !sawAsOf {
			t.Fatalf("title %v, as-of %v: %v", sawTitle, sawAsOf, runs)
		}
	})
}
