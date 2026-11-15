package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #773 review finding 3: with the foreign fill off, GET
// /api/terrain/profile is byte-identical to the response before #738.
// The goldens were written by this test on master, before the fill
// existed (CORESCOPE_UPDATE_GOLDEN=1 rewrites them).
func TestTerrainFillOff_GoldenBytes(t *testing.T) {
	cases := []struct{ name, query string }{
		// Øresund: the Swedish end is outside DHM, so the profile is partial.
		{"copenhagen-malmo", pathQuery("55.67610,12.56830", "55.60500,13.00380")},
		// Fully inside DHM, both models.
		{"silkeborg-aarhus-both", pathQuery("56.16970,9.54510", "56.15670,10.21070", "model=both")},
		// Into Germany: partial, and the end is past DHM's raster extent.
		{"sonderborg-kiel", pathQuery("54.90980,9.79220", "54.32330,10.12280")},
	}
	// A terrain that varies along every path, so the golden pins real
	// sample values, not one flat height.
	elev := func(e, n float64) float32 { return float32(int64(e/7+n/13)%97) / 4 }
	env := newTerrainEnv(t, newFakeElevation(t, elev), dhm.ProfileConfig{}, nil)
	for _, c := range cases {
		w := env.get(c.query)
		if w.Code != 200 {
			t.Fatalf("%s: status %d: %s", c.name, w.Code, w.Body.String())
		}
		path := filepath.Join("testdata", "terrain_fill_off_773", c.name+".json")
		if os.Getenv("CORESCOPE_UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, w.Body.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(w.Body.Bytes(), want) {
			t.Fatalf("%s: the response with the fill off differs from master\n got %s\nwant %s", c.name, w.Body.String(), want)
		}
	}
}
