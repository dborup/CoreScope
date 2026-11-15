package main

import (
	"encoding/json"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #746 N-a (#750 delta review): verdict.surface.reason and .endpoint exist
// only for an antenna below the surface on a fully covered profile. They
// are absent — not empty strings — on a partial or unknown surface verdict,
// whose antenna flags the partial→unknown forcing must drop, and on a
// verdict decided inside the path or not obstructed at all.
func TestTerrainProfile746_SurfaceReasonOnlyForAnEndpoint(t *testing.T) {
	x, _ := dhm.ToUTM32(56.1629, 10.2039)
	const distance = 1000
	q := pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, distance, 0), "ha=10", "hb=10")
	dtmHole := func(e, n float64) float32 { // no terrain data around mid-path
		if e > x+400 && e < x+600 {
			return -9999
		}
		return 0
	}
	aboveA := func(e, n float64) float32 { // 40 m of surface at A, 10 m antennas
		if e < x {
			return 40
		}
		return 0
	}
	midBuilding := func(e, n float64) float32 { // 200 m at mid-path only
		if e > x+450 && e < x+550 {
			return 200
		}
		return 0
	}
	// surfaceKeys is the verdict.surface object as sent, key by key, so an
	// empty "reason" is told apart from an absent one.
	surfaceKeys := func(t *testing.T, body []byte) map[string]json.RawMessage {
		t.Helper()
		var raw struct {
			Verdict struct {
				Surface map[string]json.RawMessage `json:"surface"`
			} `json:"verdict"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if raw.Verdict.Surface == nil {
			t.Fatal("no verdict.surface with model=both")
		}
		return raw.Verdict.Surface
	}

	// The control: the same antenna below the surface at A on a fully
	// covered path does carry the reason, so the cases below would carry it
	// too if the flags or the empty fields leaked.
	t.Run("control: full coverage, A below the surface", func(t *testing.T) {
		env := newTerrainEnv(t, modelSplit{dtm: newFakeElevation(t, flatElev(0)), dsm: newFakeElevation(t, aboveA)}, dhm.ProfileConfig{}, nil)
		r := decodeProfile(t, env.get(q))
		if s := r.Verdict.Surface; r.Coverage.State != "full" || s == nil || s.Reason != "antennaBelowSurface" || s.Endpoint != "A" {
			t.Fatalf("fixture: coverage %+v surface %+v, want antennaBelowSurface at A", r.Coverage, s)
		}
	})

	for _, c := range []struct {
		name      string
		dtm, dsm  func(e, n float64) float32
		coverage  string
		wantState string
	}{
		// MV2: the forcing to unknown must not carry AntennaBelowA through.
		{"partial coverage, A below the surface", dtmHole, aboveA, "partial", "unknown"},
		// MV1: an interior obstruction has no reason, and no empty one.
		{"interior obstruction only", flatElev(0), midBuilding, "full", "blocked"},
		{"clear surface", flatElev(0), flatElev(0), "full", "clear"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTerrainEnv(t, modelSplit{dtm: newFakeElevation(t, c.dtm), dsm: newFakeElevation(t, c.dsm)}, dhm.ProfileConfig{}, nil)
			w := env.get(q)
			r := decodeProfile(t, w)
			if r.Coverage.State != c.coverage || r.Verdict.Surface == nil || r.Verdict.Surface.State != c.wantState {
				t.Fatalf("fixture: coverage %+v surface %+v, want %s and %s", r.Coverage, r.Verdict.Surface, c.coverage, c.wantState)
			}
			keys := surfaceKeys(t, w.Body.Bytes())
			for _, k := range []string{"reason", "endpoint"} {
				if v, ok := keys[k]; ok {
					t.Errorf("verdict.surface.%s is present (%s); want it absent", k, v)
				}
			}
		})
	}
}
