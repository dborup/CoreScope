package main

// #730 M2 — with map.dhm.profile.buildings off (the default), the terrain
// profile answers byte for byte what it answered before the building
// classification existed. testdata/terrain_profile_730_off.golden holds
// those answers, captured from the server before #730 M2. Regenerate it
// only for a deliberate change to the response, with
// TERRAIN_730_WRITE_GOLDEN=1.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

const terrain730Golden = "terrain_profile_730_off.golden"

// dokk1E, dokk1N is the centre of Dokk1, Aarhus, in EPSG:25832: the #729
// reference case.
const dokk1E, dokk1N = 575424.0, 6223838.0

// utmPt is the EPSG:25832 position e, n as a request point.
func utmPt(e, n float64) string {
	la, lo := dhm.FromUTM32(e, n)
	return fmt.Sprintf("%.5f,%.5f", la, lo)
}

// dokk1Footprint is a 40 m square around Dokk1's centre; dokk1Ground is
// flat ground at 5 m, dokk1Surface the same with a 28 m roof inside the
// square.
func inDokk1(e, n float64) bool {
	return e >= dokk1E-20 && e <= dokk1E+20 && n >= dokk1N-20 && n <= dokk1N+20
}

func dokk1Ground(e, n float64) float32 { return 5 }

func dokk1Surface(e, n float64) float32 {
	if inDokk1(e, n) {
		return 5 + 28
	}
	return 5
}

// dokk1Path is a 237 m path due north through Dokk1, its centre 138 m
// from A.
var dokk1A, dokk1B = utmPt(dokk1E, dokk1N-138), utmPt(dokk1E, dokk1N+99)

// The forest path is 5 km due east in mid-Jutland, over flat ground at
// 50 m with 18 m of trees from 1 to 3 km.
const forestE, forestN = 530000.0, 6230000.0

var forestA, forestB = utmPt(forestE, forestN), utmPt(forestE+5000, forestN)

func forestGround(e, n float64) float32 { return 50 }

func forestSurface(e, n float64) float32 {
	if e > forestE+1000 && e < forestE+3000 {
		return 50 + 18
	}
	return 50
}

// The partial path crosses Øresund from Helsingør onto Swedish land.
var partialA, partialB = "56.03600,12.61300", "56.04600,12.69400"

type terrainGoldenCase struct {
	Status int    `json:"status"`
	Cache  string `json:"cache"`
	Body   string `json:"body"`
}

// terrainGoldenRuns are the requests the golden holds, in order, each
// against a fresh profiler on its own fake elevation.
var terrainGoldenRuns = []struct {
	name     string
	dtm, dsm func(e, n float64) float32
	queries  []string // one request each; the second of a pair is a hit
}{
	{"dokk1", dokk1Ground, dokk1Surface, []string{pathQuery(dokk1A, dokk1B), pathQuery(dokk1A, dokk1B)}},
	{"dokk1-dtm", dokk1Ground, dokk1Surface, []string{pathQuery(dokk1A, dokk1B, "model=dtm")}},
	{"dokk1-roof-antenna", dokk1Ground, dokk1Surface, []string{pathQuery(dokk1A, dokk1B, "ha=30", "hb=30")}},
	{"forest", forestGround, forestSurface, []string{pathQuery(forestA, forestB)}},
	{"partial", forestGround, forestSurface, []string{pathQuery(partialA, partialB)}},
}

// terrainGoldenAnswers runs terrainGoldenRuns on profilers that newEnv
// builds, keyed "name/i".
func terrainGoldenAnswers(t *testing.T, newEnv func(src *fakeElevation) *terrainEnv) map[string]terrainGoldenCase {
	t.Helper()
	out := map[string]terrainGoldenCase{}
	for _, run := range terrainGoldenRuns {
		src := newFakeElevation(t, run.dtm)
		src.surface = run.dsm
		env := newEnv(src)
		for i, q := range run.queries {
			w := env.get(q)
			out[fmt.Sprintf("%s/%d", run.name, i)] = terrainGoldenCase{
				Status: w.Code, Cache: w.Header().Get("X-CoreScope-Cache"), Body: w.Body.String()}
		}
	}
	return out
}

func readTerrainGolden(t *testing.T, got map[string]terrainGoldenCase) map[string]terrainGoldenCase {
	t.Helper()
	path := filepath.Join("testdata", terrain730Golden)
	if os.Getenv("TERRAIN_730_WRITE_GOLDEN") == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]terrainGoldenCase
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	return want
}

func compareTerrainGolden(t *testing.T, got, want map[string]terrainGoldenCase) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%d answers, golden has %d", len(got), len(want))
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("%s: missing", k)
			continue
		}
		if g.Status != w.Status || g.Cache != w.Cache {
			t.Errorf("%s: status %d cache %q, golden %d %q", k, g.Status, g.Cache, w.Status, w.Cache)
		}
		if g.Body != w.Body {
			t.Errorf("%s: body differs from the golden\n got: %s\nwant: %s", k, g.Body, w.Body)
		}
	}
}

// TestTerrainProfileGolden_BuildingsOff: the default configuration answers
// exactly as before #730 M2.
func TestTerrainProfileGolden_BuildingsOff(t *testing.T) {
	got := terrainGoldenAnswers(t, func(src *fakeElevation) *terrainEnv {
		return newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	})
	compareTerrainGolden(t, got, readTerrainGolden(t, got))
	// The golden is not vacuous: the surface sees Dokk1 and the trees
	// through the line of sight, the terrain does not.
	want := readTerrainGolden(t, got)
	for _, k := range []string{"dokk1/0", "forest/0"} {
		var resp TerrainProfileResponse
		if err := json.Unmarshal([]byte(want[k].Body), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Verdict.State == "blocked" || resp.Verdict.Surface == nil || resp.Verdict.Surface.State != "blocked" {
			t.Fatalf("%s: verdict %s, surface %+v; want the surface blocked and the terrain not", k, resp.Verdict.State, resp.Verdict.Surface)
		}
	}
}
