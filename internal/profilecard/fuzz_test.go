package profilecard

import (
	"encoding/binary"
	"math"
	"testing"
)

// FuzzRender drives Render with arbitrary responses: the elevations come
// from raw float32 bit patterns (so NaN, ±Inf, subnormals and 3e38 all
// occur), the distances and physics from arbitrary float64s, the strings
// from arbitrary bytes. Whatever it gets, Render must return a 1200×630
// PNG with the credit on it. CI runs the seeds below; run longer with
//
//	go test -run '^$' -fuzz FuzzRender -fuzztime 60s
func FuzzRender(f *testing.F) {
	clear := fxClear()
	f.Add(clear.DistanceM, clear.Physics.LambdaM, clear.Physics.K, 18.0, 12.0, 0.6, "both", "full", "clear", "Søby", "东京", []byte{0, 0, 0x20, 0x42, 0, 0, 0x30, 0x42, 0, 0, 0x28, 0x42}, uint8(0))
	f.Add(0.0, 0.0, 0.0, 0.0, 0.0, 0.0, "", "", "", "", "", []byte{}, uint8(255))
	f.Add(math.NaN(), math.Inf(1), -1.0, math.NaN(), math.Inf(-1), math.NaN(), "dtm", "partial", "blocked", "\x00", "‮", []byte{0, 0, 0xc0, 0x7f, 0, 0, 0x80, 0x7f, 0, 0, 0x80, 0xff}, uint8(7))
	f.Add(1e-300, 1e300, 1e-9, 1e308, -1e308, 2.0, "both", "none", "fresnel60", "a", "b", []byte{0xff, 0xff, 0x7f, 0x7f, 1, 0, 0, 0}, uint8(3))
	f.Add(80000.0, 0.345, 4.0/3, 10.0, 2.0, 0.6, "both", "full", "fresnel100", "North Hub", "South Hub", make([]byte, 4*3200), uint8(1))

	f.Fuzz(func(t *testing.T, dist, lambda, k, antA, antB, criterion float64, model, cov, state, nameA, nameB string, elev []byte, flags uint8) {
		if len(elev) > 4*4096 {
			elev = elev[:4*4096]
		}
		n := len(elev) / 4
		resp := TerrainProfileResponse{
			DistanceM: dist, SampleCount: n, Model: model,
			Coverage: TerrainCoverage{State: cov, Pct: dist},
			A:        TerrainEndpoint{Kind: []string{"node", "point", ""}[int(flags)%3], AntennaM: antA, Assumed: flags&8 != 0},
			B:        TerrainEndpoint{Kind: []string{"point", "node", "?"}[int(flags>>2)%3], AntennaM: antB, Lat: antA, Lon: antB},
			Physics:  TerrainPhysics{LambdaM: lambda, K: k, FreqMHz: lambda},
			Verdict:  TerrainVerdict{State: state, Criterion: criterion},
		}
		if flags&16 != 0 {
			resp.Verdict.WorstAtM, resp.Verdict.WorstClearanceM = &dist, &antA
		}
		if flags&32 != 0 {
			resp.Verdict.Surface = &TerrainSurfaceInfo{State: state}
		}
		if flags&64 != 0 {
			resp.A.GroundM = &antB
		}
		for i := 0; i < n; i++ {
			v := float64(math.Float32frombits(binary.LittleEndian.Uint32(elev[4*i:])))
			s := TerrainSample{D: float64(i) * dist / math.Max(1, float64(n-1))}
			if elev[4*i]&1 == 0 {
				s.DTM = &v
			}
			if elev[4*i]&2 == 0 {
				w := v + float64(elev[4*i+1])
				s.DSM = &w
			}
			resp.Samples = append(resp.Samples, s)
		}
		img, runs, err := render(resp, [2]string{nameA, nameB}, DefaultPalette(flags&128 != 0), Options{HideNames: flags&4 != 0, Site: nameB})
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != Width || b.Dy() != Height {
			t.Fatalf("bounds %v", b)
		}
		ok := false
		for _, r := range runs {
			ok = ok || r.Role == "attribution" && r.Size == attributionSize
		}
		if !ok {
			t.Fatal("no attribution")
		}
	})
}
