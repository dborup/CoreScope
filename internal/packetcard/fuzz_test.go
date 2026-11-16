package packetcard

import (
	"math"
	"strings"
	"testing"
	"time"
)

// FuzzRender drives the renderer with arbitrary coordinates (NaN, ±Inf,
// ±180°, the antimeridian, identical points, the poles), names, types,
// counts and times. It must always return a 1200×630 card whose runs stay
// in the safe area, never draw a number that is negative or NaN (no value has a
// dash), and never
// draw the first observer's name without First nor the origin's for
// anything but an advert. CI runs the seeds; run longer with
//
//	go test -run '^$' -fuzz FuzzRender -fuzztime 60s
func FuzzRender(f *testing.F) {
	f.Add(56.15, 10.2, 56.2, 10.1, 56.3, 9.9, uint8(5), uint8(1), uint16(3), int64(6), 4.2, 38.0, 1234.0, "Aarhus-Obs", "Rpt", "mesh.example", int64(1_763_212_841))
	f.Add(math.NaN(), math.Inf(1), math.Inf(-1), -180.0, 180.0, 180.0, uint8(2), uint8(2), uint16(0), int64(-1), math.NaN(), -1.0, math.Inf(1), "", "", "", int64(0))
	f.Add(-17.7, 179.9, -17.8, -179.9, -17.75, 180.0, uint8(4), uint8(0), uint16(1), int64(64), 0.0, 0.0, 0.0, "東京", "‮\x00x", "\n", int64(math.MinInt64))
	f.Add(56.0, 10.0, 56.0, 10.0, 56.0, 10.0, uint8(255), uint8(255), uint16(65535), int64(math.MaxInt64), 1e300, 1e300, 1e300, strings.Repeat("Ω", 300), strings.Repeat("Å", 200), strings.Repeat("x", 500), int64(math.MaxInt64))
	f.Add(90.0, -180.0, -90.0, 180.0, 0.0, 0.0, uint8(9), uint8(3), uint16(200), int64(2), 59.96, 0.04, -5.0, "a", "b", "c", int64(-62135596800))

	f.Fuzz(func(t *testing.T, la1, lo1, la2, lo2, la3, lo3 float64, pt, flags uint8, nb uint16, hops int64, spread, km, air float64, obsName, advName, site string, heard int64) {
		n := int(nb % 300)
		b := Branch{Hops: int(hops), Points: []LatLon{{la1, lo1}, {la2, lo2}}, Observer: &LatLon{la3, lo3},
			SecondsAfterFirst: &spread, DistanceFromFirstKm: &km}
		in := PacketCardInput{
			PayloadType: int(pt % 17), RouteType: int(flags % 5),
			FirstObserverName: obsName, AdvertName: advName,
			Areas:      []Area{{Ring: []LatLon{{la1, lo1}, {la2, lo2}, {la3, lo3}}}, {Ring: BoxRing(la1, la2, lo1, lo2)}},
			FirstHeard: time.Unix(heard, 0), AirtimeMs: &air,
		}
		for i := 0; i < n; i++ {
			in.Branches = append(in.Branches, b)
		}
		if flags&8 == 0 {
			in.First = &b
		}
		img, tr, err := render(in, Options{Site: site, HideNames: flags&16 != 0})
		if err != nil {
			t.Fatal(err)
		}
		if bd := img.Bounds(); bd.Dx() != Width || bd.Dy() != Height {
			t.Fatalf("bounds %v", bd)
		}
		for _, r := range tr.runs {
			if r.X < safeMargin || r.X+r.W > Width-safeMargin {
				t.Fatalf("%s run %q at x=%d w=%d leaves the safe area", r.Role, r.Text, r.X, r.W)
			}
			if r.Role == "value" && (strings.Contains(r.Text, "-") || strings.Contains(r.Text, "NaN") || strings.Contains(r.Text, "Inf")) {
				t.Fatalf("value %q", r.Text)
			}
			if r.Role == "heard" && strings.Contains(r.Text, "First heard by") && in.First == nil {
				t.Fatalf("first observer named without First: %q", r.Text)
			}
			if r.Role == "heard" && strings.Contains(r.Text, "Advert from") && in.PayloadType != 4 {
				t.Fatalf("origin named for payload type %d: %q", in.PayloadType, r.Text)
			}
		}
		if tr.points > MaxPoints || tr.branches > MaxBranches+1 {
			t.Fatalf("%d points, %d branches drawn", tr.points, tr.branches)
		}
	})
}
