package main

// #737 M3a — the point and the line of sight in each photo of GET
// /api/skraafoto/search, against the real STAC items of Dokk1 in Aarhus
// (internal/skraafoto/testdata, public data, no token) served by the
// skraafototest fake, and a fake DHM. Nothing reaches the network.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
	"github.com/meshcore-analyzer/skraafoto"
)

const (
	skraafotoFixture = "../../internal/skraafoto/testdata/stac-aarhus-dokk1-2025.json"
	// Dokk1: inside all four photos.
	skraafotoDokk1Lat, skraafotoDokk1Lon = 56.15370, 10.21395
	skraafotoDokk1                       = "lat=56.15370&lon=10.21395"
)

// skraafotoFixtureFeatures are the fixture's items by direction.
func skraafotoFixtureFeatures(t testing.TB) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(skraafotoFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fc struct {
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(b, &fc); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, f := range fc.Features {
		var d struct {
			Properties struct {
				Direction string `json:"direction"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(f, &d); err != nil {
			t.Fatal(err)
		}
		out[d.Properties.Direction] = append(out[d.Properties.Direction], string(f))
	}
	return out
}

// skraafotoFixtureCamera is the camera of a raw fixture item.
func skraafotoFixtureCamera(t testing.TB, raw, dir string) *skraafoto.Camera {
	t.Helper()
	items, _, err := skraafoto.ParseSearch([]byte(`{"features":[`+raw+`]}`), 2025, skraafoto.Direction(dir))
	if err != nil || len(items) != 1 || items[0].Camera == nil {
		t.Fatalf("%s: %v %v", dir, items, err)
	}
	return items[0].Camera
}

// newSkraafotoM3aEnv serves the fixture's items, and, with elev, a
// terrain profile on a fake DHM.
func newSkraafotoM3aEnv(t *testing.T, elev *fakeElevation, pc dhm.ProfileConfig) *skraafotoEnv {
	t.Helper()
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	e.up.Features = skraafotoFixtureFeatures(t)
	if elev != nil {
		pc.Enabled = true
		e.srv.terrain = newTerrainProfiler(pc, nil, terrainProfilerDeps{src: elev, logf: e.logs.logf})
		if e.srv.terrain == nil {
			t.Fatal("no terrain profiler")
		}
	}
	return e
}

func photoByDirection(t *testing.T, resp SkraafotoSearchResponse, dir string) SkraafotoPhoto {
	t.Helper()
	for _, ph := range resp.Photos {
		if ph.Direction == dir {
			return ph
		}
	}
	t.Fatalf("no %s photo in %+v", dir, resp.Photos)
	return SkraafotoPhoto{}
}

func nearPx(a SkraafotoPixel, b skraafoto.Pixel, tol float64) bool {
	return math.Abs(a.X-b.X) <= tol && math.Abs(a.Y-b.Y) <= tol
}

// The point lands where the camera puts it at the DHM height, in each
// preview's own frame, and the answer is cached by the point.
func TestSkraafoto737M3a_PointAtDHMHeight(t *testing.T) {
	elev := newFakeElevation(t, flatElev(3))
	e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{})
	w := e.search(skraafotoDokk1)
	resp := decodeSkraafotoSearch(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("cache %q", w.Header().Get("X-CoreScope-Cache"))
	}
	var dirs []string
	for _, ph := range resp.Photos {
		dirs = append(dirs, ph.Direction)
	}
	if strings.Join(dirs, ",") != "north,east,south,nadir" {
		t.Fatalf("directions %v", dirs)
	}
	pe, pn := dhm.ToUTM32(skraafotoDokk1Lat, skraafotoDokk1Lon)
	feats := skraafotoFixtureFeatures(t)
	frames := map[string]SkraafotoFrame{"north": {1024, 765}, "east": {765, 1024}, "south": {1024, 765}, "nadir": {1024, 699}}
	for _, ph := range resp.Photos {
		c := skraafotoFixtureCamera(t, feats[ph.Direction][0], ph.Direction)
		want, _ := c.At(skraafoto.Ground{E: pe, N: pn, H: 3})
		if ph.Frame == nil || *ph.Frame != frames[ph.Direction] || ph.Point == nil || !ph.InFrame || ph.Approx || ph.Path != nil {
			t.Fatalf("%s: %+v", ph.Direction, ph)
		}
		if !nearPx(*ph.Point, want, 0.05) {
			t.Fatalf("%s: point %+v, want %+v", ph.Direction, *ph.Point, want)
		}
		// At the footprint height instead, an oblique photo would put it
		// elsewhere: the DHM height is what was used. (Near the nadir
		// photo's centre, a few metres of height move it under a pixel.)
		if fp, _ := c.At(skraafoto.Ground{E: pe, N: pn, H: c.GroundM}); ph.Direction != "nadir" && nearPx(*ph.Point, fp, 1) {
			t.Fatalf("%s: point at the footprint height", ph.Direction)
		}
	}
	// One 2 × 2 terrain raster, no surface model.
	if elev.calls.Load() != 1 || elev.model[dhm.ModelTerrain] != 1 {
		t.Fatalf("%d DHM calls %v", elev.calls.Load(), elev.model)
	}
	// Again: the answer is cached; neither upstream is asked.
	s0, _ := e.upstream()
	if w := e.search(skraafotoDokk1); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" {
		t.Fatalf("again: %d %q", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
	// A point two metres away, in the same 10 m search cell: its own
	// answer and height, the cell's photos.
	// DHM was asked for it, so it is a miss although the photos were
	// cached (#827 review finding 8).
	w2 := e.search("lat=56.15372&lon=10.21395")
	resp2 := decodeSkraafotoSearch(t, w2)
	if s, _ := e.upstream(); s != s0 || elev.calls.Load() != 2 {
		t.Fatalf("%d STAC, %d DHM calls", s-s0, elev.calls.Load())
	}
	if c := w2.Header().Get("X-CoreScope-Cache"); c != "miss" {
		t.Fatalf("a height fetched from DHM: cache %q", c)
	}
	// The nadir photo is turned half round (κ ≈ −180°: its top-left
	// corner is the footprint's south-east one), so north is down.
	if p1, p2 := photoByDirection(t, resp, "nadir").Point, photoByDirection(t, resp2, "nadir").Point; p2.Y-p1.Y < 0.5 {
		t.Fatalf("a point 2 m north is not lower in the nadir preview: %+v %+v", p1, p2)
	}
}

// The frame is the preview the preview route serves: the oblique
// pyramid's preview decodes to the size the search reports.
func TestSkraafoto737M3a_FrameIsThePreview(t *testing.T) {
	e := newSkraafotoM3aEnv(t, nil, dhm.ProfileConfig{})
	var levels [][2]int
	for l := [2]int{14144, 10560}; ; l = [2]int{(l[0] + 1) / 2, (l[1] + 1) / 2} {
		levels = append(levels, l)
		if max(l[0], l[1]) < 512 {
			break
		}
	}
	cog := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: levels, Encode: func(l int) bool { return l == 3 }, Pad: 1 << 20})
	e.up.COG = func(string) []byte { return cog }
	ph := photoByDirection(t, decodeSkraafotoSearch(t, e.search(skraafotoDokk1)), "north")
	w := e.get(ph.PreviewURL)
	cfg, err := jpeg.DecodeConfig(w.Body)
	if err != nil || ph.Frame == nil || cfg.Width != ph.Frame.Width || cfg.Height != ph.Frame.Height {
		t.Fatalf("preview %+v (%v), frame %+v", cfg, err, ph.Frame)
	}
}

// Without DHM each photo's footprint height stands in, flagged approx,
// and the answer is cached: a retry would not do better.
func TestSkraafoto737M3a_FootprintHeightWithoutDHM(t *testing.T) {
	e := newSkraafotoM3aEnv(t, nil, dhm.ProfileConfig{})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoDokk1))
	pe, pn := dhm.ToUTM32(skraafotoDokk1Lat, skraafotoDokk1Lon)
	feats := skraafotoFixtureFeatures(t)
	for _, ph := range resp.Photos {
		c := skraafotoFixtureCamera(t, feats[ph.Direction][0], ph.Direction)
		want, _ := c.At(skraafoto.Ground{E: pe, N: pn, H: c.GroundM})
		if !ph.Approx || ph.Point == nil || !ph.InFrame || !nearPx(*ph.Point, want, 0.05) {
			t.Fatalf("%s: %+v, want %+v", ph.Direction, ph, want)
		}
	}
	if _, ok := e.p.answers.Get(skraafotoTestAnswerKey(t, e, skraafotoDokk1), e.clock.now()); !ok {
		t.Fatal("an answer without DHM was not cached")
	}
}

func skraafotoTestAnswerKey(t *testing.T, e *skraafotoEnv, query string) skraafotoAnswerKey {
	t.Helper()
	sq, err := parseSkraafotoSearch(query)
	if err != nil {
		t.Fatal(err)
	}
	pe, pn := dhm.ToUTM32(sq.lat, sq.lon)
	k, ok := e.srv.skraafotoAnswerKey(sq, skraafotoSearchKey{year: sq.year, e: snapSkraafoto(pe), n: snapSkraafoto(pn)})
	if !ok {
		t.Fatal("answer key refused")
	}
	return k
}

// A DHM that fails, is busy or is over the client's budget leaves the
// footprint height, approx, and nothing cached: the next request asks
// DHM again. A request refused by admission gets its budget back.
func TestSkraafoto737M3a_DHMUnavailableNotCached(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		elev := newFakeElevation(t, flatElev(3))
		fail := true
		elev.hook = func(context.Context, dhm.RasterRequest) error {
			if fail {
				return dhm.FetchError{Reason: "status 500"}
			}
			return nil
		}
		e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{})
		for i := 0; i < 2; i++ {
			if ph := decodeSkraafotoSearch(t, e.search(skraafotoDokk1)).Photos[0]; !ph.Approx || !ph.InFrame {
				t.Fatalf("%+v", ph)
			}
		}
		if elev.calls.Load() != 2 {
			t.Fatalf("%d DHM calls: a failed height was cached", elev.calls.Load())
		}
		fail = false
		if ph := decodeSkraafotoSearch(t, e.search(skraafotoDokk1)).Photos[0]; ph.Approx {
			t.Fatalf("after recovery: %+v", ph)
		}
	})
	t.Run("budget", func(t *testing.T) {
		elev := newFakeElevation(t, flatElev(3))
		e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{RateLimitPerMin: intp(1)})
		decodeSkraafotoSearch(t, e.search(skraafotoDokk1))
		ph := decodeSkraafotoSearch(t, e.search("lat=56.15390&lon=10.21395")).Photos[0]
		if !ph.Approx || elev.calls.Load() != 1 {
			t.Fatalf("over budget: %+v, %d DHM calls", ph, elev.calls.Load())
		}
		if _, ok := e.p.answers.Get(skraafotoTestAnswerKey(t, e, "lat=56.15390&lon=10.21395"), e.clock.now()); ok {
			t.Fatal("an answer over budget was cached")
		}
	})
	t.Run("admission", func(t *testing.T) {
		elev := newFakeElevation(t, flatElev(3))
		e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{MaxInFlight: intp(1), RateLimitPerMin: intp(1)})
		tp := e.srv.terrain
		hold, release := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			tp.flight.DoAdmitted(context.Background(), "hold", time.Minute, tp.admit, func(context.Context) ([]byte, error) {
				close(hold)
				<-release
				return nil, errors.New("held")
			})
		}()
		<-hold
		ph := decodeSkraafotoSearch(t, e.search(skraafotoDokk1)).Photos[0]
		close(release)
		wg.Wait()
		if !ph.Approx || elev.calls.Load() != 0 || !strings.Contains(e.logs.String(), "admission") {
			t.Fatalf("busy: %+v, %d DHM calls, logs %q", ph, elev.calls.Load(), e.logs.String())
		}
		// The refused charge came back: the one-a-minute budget still
		// allows the retry.
		if ph := decodeSkraafotoSearch(t, e.search(skraafotoDokk1)).Photos[0]; ph.Approx || elev.calls.Load() != 1 {
			t.Fatalf("retry: %+v, %d DHM calls", ph, elev.calls.Load())
		}
	})
	// Only some of the heights refused (here B is answered, the point and
	// A are refused as admission refuses them): DHM did work for the
	// request, so its charge stays (#827 review finding 5).
	t.Run("partial admission", func(t *testing.T) {
		elev := newFakeElevation(t, flatElev(3))
		bE, _ := dhm.ToUTM32(56.15370, 10.21230)
		elev.hook = func(_ context.Context, r dhm.RasterRequest) error {
			if math.Abs((r.BBox[0]+r.BBox[2])/2-bE) > 10 {
				return dhm.ErrAdmissionFull
			}
			return nil
		}
		e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{RateLimitPerMin: intp(1)})
		ph := decodeSkraafotoSearch(t, e.search(skraafotoDokk1+"&path=56.15370,10.21560,56.15370,10.21230")).Photos[0]
		if !ph.Approx || elev.calls.Load() != 3 || strings.Contains(e.logs.String(), "admission") {
			t.Fatalf("partly refused: %+v, %d DHM calls, logs %q", ph, elev.calls.Load(), e.logs.String())
		}
		if _, ok := e.p.grounds.Get(skraafotoGroundKey(snapLatLon(dhm.LatLon{Lat: 56.15370, Lon: 10.21230})), e.clock.now()); !ok {
			t.Fatal("the height DHM gave was not kept")
		}
		// The one-a-minute budget is spent: the next point is not asked.
		if ph := decodeSkraafotoSearch(t, e.search("lat=56.15390&lon=10.21395")).Photos[0]; !ph.Approx || elev.calls.Load() != 3 {
			t.Fatalf("after a partial refusal: %+v, %d DHM calls: its charge was given back", ph, elev.calls.Load())
		}
	})
}

// skraafotoShifted is a fixture item renamed from id to newID (its asset
// too) with its camera moved by dE metres east: the same view, the point
// off its centre.
func skraafotoShifted(t *testing.T, raw, id, newID string, dE float64) string {
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.ReplaceAll(raw, id, newID)), &f); err != nil {
		t.Fatal(err)
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(f["properties"], &props); err != nil {
		t.Fatal(err)
	}
	var c [3]float64
	if err := json.Unmarshal(props["pers:perspective_center"], &c); err != nil {
		t.Fatal(err)
	}
	c[0] += dE
	props["pers:perspective_center"], _ = json.Marshal(c)
	f["properties"], _ = json.Marshal(props)
	b, _ := json.Marshal(f)
	return string(b)
}

// Of a direction's candidates the photo shown is the one whose centre is
// nearest the point, whatever the API's order.
func TestSkraafoto737M3a_PicksBestCentred(t *testing.T) {
	feats := skraafotoFixtureFeatures(t)
	orig := feats["north"][0]
	const origID, offID = "2025_82_24_2_0138_00000126_100mm", "2025_82_24_2_0138_00000125_100mm"
	off := skraafotoShifted(t, orig, origID, offID, 450)
	for _, order := range [][]string{{off, orig}, {orig, off}} {
		e := newSkraafotoM3aEnv(t, newFakeElevation(t, flatElev(3)), dhm.ProfileConfig{})
		e.up.Features["north"] = order
		ph := photoByDirection(t, decodeSkraafotoSearch(t, e.search(skraafotoDokk1)), "north")
		if ph.ID != origID || !ph.InFrame {
			t.Fatalf("picked %s %+v", ph.ID, ph.Point)
		}
		// The off-centre photo still has the point in frame: it was a
		// real choice. Both were remembered for previews.
		pe, pn := dhm.ToUTM32(skraafotoDokk1Lat, skraafotoDokk1Lon)
		c := skraafotoFixtureCamera(t, off, "north")
		if p, ok := c.At(skraafoto.Ground{E: pe, N: pn, H: 3}); !ok || !c.Frame().Contains(p) {
			t.Fatalf("the shifted photo does not see the point: %+v", p)
		}
		if _, ok := e.p.paths.Get(skraafotoPhotoKey{"skraafotos2025", offID}, e.clock.now()); !ok {
			t.Fatal("a candidate's CDN path was not kept")
		}
	}
	// A point no candidate sees: the API's first, as before M3a.
	e := newSkraafotoM3aEnv(t, nil, dhm.ProfileConfig{})
	e.up.Features["north"] = []string{off, orig}
	ph := photoByDirection(t, decodeSkraafotoSearch(t, e.search("lat=56.19000&lon=10.21395")), "north")
	if ph.ID != offID || ph.InFrame {
		t.Fatalf("outside every frame: picked %s %+v", ph.ID, ph)
	}
}

// A point 5 km away is reported outside the frame.
func TestSkraafoto737M3a_PointOutsideFrame(t *testing.T) {
	e := newSkraafotoM3aEnv(t, newFakeElevation(t, flatElev(3)), dhm.ProfileConfig{})
	resp := decodeSkraafotoSearch(t, e.search("lat=56.19900&lon=10.21395"))
	if len(resp.Photos) != 4 {
		t.Fatalf("%d photos", len(resp.Photos))
	}
	for _, ph := range resp.Photos {
		if ph.InFrame || ph.Path != nil {
			t.Fatalf("%s: %+v", ph.Direction, ph)
		}
		if ph.Point != nil && ph.Point.X >= 0 && ph.Point.X <= float64(ph.Frame.Width) && ph.Point.Y >= 0 && ph.Point.Y <= float64(ph.Frame.Height) {
			t.Fatalf("%s: point %+v inside %+v but not in frame", ph.Direction, ph.Point, ph.Frame)
		}
	}
}

// The line of sight from Dokk1 (antenna 10 m) to a point 3 km west
// (antenna 20 m): 32 points in each frame, from A at its antenna height
// to where the line leaves the frame; a different path, or antenna
// height, is a different answer.
func TestSkraafoto737M3a_PathInFrame(t *testing.T) {
	elev := newFakeElevation(t, flatElev(3))
	e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{})
	const path = "&path=56.15370,10.21395,56.15370,10.16500"
	resp := decodeSkraafotoSearch(t, e.search(skraafotoDokk1+path+"&ha=10&hb=20"))
	pe, pn := dhm.ToUTM32(skraafotoDokk1Lat, skraafotoDokk1Lon)
	feats := skraafotoFixtureFeatures(t)
	for _, ph := range resp.Photos {
		if len(ph.Path) != 32 || ph.Approx {
			t.Fatalf("%s: %d points, approx %v", ph.Direction, len(ph.Path), ph.Approx)
		}
		c := skraafotoFixtureCamera(t, feats[ph.Direction][0], ph.Direction)
		a, _ := c.At(skraafoto.Ground{E: pe, N: pn, H: 3 + 10})
		if !nearPx(ph.Path[0], a, 0.05) {
			t.Fatalf("%s: path starts at %+v, A's antenna is at %+v", ph.Direction, ph.Path[0], a)
		}
		f := ph.Frame
		for i, q := range ph.Path {
			if q.X < 0 || q.X > float64(f.Width) || q.Y < 0 || q.Y > float64(f.Height) {
				t.Fatalf("%s: point %d %+v outside %+v", ph.Direction, i, q, *f)
			}
		}
		last := ph.Path[len(ph.Path)-1]
		if last.X > 0.1 && last.X < float64(f.Width)-0.1 && last.Y > 0.1 && last.Y < float64(f.Height)-0.1 {
			t.Fatalf("%s: the line ends inside the frame at %+v; B is 3 km away", ph.Direction, last)
		}
	}
	nadir := photoByDirection(t, resp, "nadir")
	// Each changed input changes the answer: none is served from the
	// cache of another.
	for _, q := range []string{
		skraafotoDokk1 + path + "&ha=30&hb=20",
		skraafotoDokk1 + path + "&ha=10&hb=20&year=2025",
		skraafotoDokk1 + "&path=56.15370,10.21395,56.18000,10.21395&ha=10&hb=20",
		skraafotoDokk1 + path,
	} {
		got := photoByDirection(t, decodeSkraafotoSearch(t, e.search(q)), "nadir")
		same := len(got.Path) == len(nadir.Path)
		for i := 0; same && i < len(got.Path); i++ {
			same = got.Path[i] == nadir.Path[i]
		}
		if same && !strings.Contains(q, "year=") {
			t.Errorf("%s: the same path as another query's", q)
		}
	}
	// Without a path: no line.
	if ph := photoByDirection(t, decodeSkraafotoSearch(t, e.search(skraafotoDokk1)), "nadir"); ph.Path != nil {
		t.Fatalf("a path without one: %+v", ph.Path)
	}
	// The ends' heights come from DHM like the point's: unknown, the
	// footprint's, flagged.
	e2 := newSkraafotoM3aEnv(t, nil, dhm.ProfileConfig{})
	if ph := photoByDirection(t, decodeSkraafotoSearch(t, e2.search(skraafotoDokk1+path)), "nadir"); len(ph.Path) == 0 || !ph.Approx {
		t.Fatalf("without DHM: %+v", ph)
	}
}

// Each end of the line of sight stands on its own terrain, not the
// point's, and the ends are not swapped: the point, A (100 m east) and B
// (100 m west) are on ground 3, 20 and 40 m high (#827 review finding
// 4). Changing only an antenna height asks no upstream again: hit.
func TestSkraafoto737M3a_PathEndsAtTheirOwnTerrain(t *testing.T) {
	const aLat, aLon, bLat, bLon = 56.15370, 10.21560, 56.15370, 10.21230
	pe, pn := dhm.ToUTM32(skraafotoDokk1Lat, skraafotoDokk1Lon)
	ae, an := dhm.ToUTM32(aLat, aLon)
	be, bn := dhm.ToUTM32(bLat, bLon)
	elev := newFakeElevation(t, func(e, _ float64) float32 {
		switch {
		case e > pe+50:
			return 20
		case e < pe-50:
			return 40
		}
		return 3
	})
	e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{})
	const path = "&path=56.15370,10.21560,56.15370,10.21230"
	w := e.search(skraafotoDokk1 + path + "&ha=10&hb=5")
	resp := decodeSkraafotoSearch(t, w)
	if c := w.Header().Get("X-CoreScope-Cache"); c != "miss" || elev.calls.Load() != 3 {
		t.Fatalf("cache %q, %d DHM calls", c, elev.calls.Load())
	}
	feats := skraafotoFixtureFeatures(t)
	for _, ph := range resp.Photos {
		c := skraafotoFixtureCamera(t, feats[ph.Direction][0], ph.Direction)
		pt, _ := c.At(skraafoto.Ground{E: pe, N: pn, H: 3})
		a, okA := c.At(skraafoto.Ground{E: ae, N: an, H: 20 + 10})
		b, okB := c.At(skraafoto.Ground{E: be, N: bn, H: 40 + 5})
		if !okA || !okB || !c.Frame().Contains(a) || !c.Frame().Contains(b) {
			t.Fatalf("%s: the fixture does not see both ends: %+v %+v", ph.Direction, a, b)
		}
		if ph.Approx || ph.Point == nil || !nearPx(*ph.Point, pt, 0.06) || len(ph.Path) != skraafotoPathPoints {
			t.Fatalf("%s: %+v, point want %+v", ph.Direction, ph, pt)
		}
		// Both ends in frame: the path runs from A to B.
		if first, last := ph.Path[0], ph.Path[len(ph.Path)-1]; !nearPx(first, a, 0.06) || !nearPx(last, b, 0.06) {
			t.Fatalf("%s: path %+v … %+v, want A %+v … B %+v", ph.Direction, first, last, a, b)
		}
		// At the point's height A would be elsewhere: the test tells
		// the heights apart.
		if wrong, _ := c.At(skraafoto.Ground{E: ae, N: an, H: 3 + 10}); nearPx(ph.Path[0], wrong, 0.2) {
			t.Fatalf("%s: A at the point's height is indistinguishable", ph.Direction)
		}
	}
	w = e.search(skraafotoDokk1 + path + "&ha=30&hb=5")
	if c := w.Header().Get("X-CoreScope-Cache"); w.Code != 200 || c != "hit" || elev.calls.Load() != 3 {
		t.Fatalf("another antenna height: %d %q, %d DHM calls", w.Code, c, elev.calls.Load())
	}
}

// One unusable candidate after a direction's first leaves the search
// standing: it is left out and logged by count at most once a minute.
// An unusable first candidate still fails the search (#827 review
// finding 6).
func TestSkraafoto737M3a_BadLaterCandidateSkipped(t *testing.T) {
	feats := skraafotoFixtureFeatures(t)
	bad := strings.Replace(feats["east"][0], `"direction": "east"`, `"direction": "west"`, 1)
	if bad == feats["east"][0] {
		t.Fatal("the mutation did not apply")
	}
	e := newSkraafotoM3aEnv(t, nil, dhm.ProfileConfig{})
	e.up.Features["east"] = []string{feats["east"][0], bad}
	resp := decodeSkraafotoSearch(t, e.search(skraafotoDokk1))
	if ph := photoByDirection(t, resp, "east"); ph.Point == nil || len(resp.Photos) != 4 {
		t.Fatalf("%+v", resp.Photos)
	}
	const line = "left out 1 search candidate(s)"
	if n := strings.Count(e.logs.String(), line); n != 1 {
		t.Fatalf("%d log lines: %q", n, e.logs.String())
	}
	// Another cell within the minute: counted, not logged; after it, the
	// count since the last line.
	decodeSkraafotoSearch(t, e.search("lat=56.15470&lon=10.21395"))
	e.clock.add(time.Minute)
	decodeSkraafotoSearch(t, e.search("lat=56.15570&lon=10.21395"))
	if logs := e.logs.String(); strings.Count(logs, line) != 1 || !strings.Contains(logs, "left out 2 search candidate(s)") {
		t.Fatalf("logs %q", logs)
	}
	e.up.Features["east"] = []string{bad, feats["east"][0]}
	if w := e.search("lat=56.15670&lon=10.21395"); w.Code != 502 {
		t.Fatalf("a bad first candidate: %d", w.Code)
	}
}

// The search cache's capacity in Server's constants: a cell of five
// directions of five candidates fits about 1000 times in
// skraafotoSearchBytes, an answer 2800 times or more (#827 review
// finding 7).
func TestSkraafoto737M3a_CacheEntrySizes(t *testing.T) {
	cands := skraafotoBenchCandidates(t)
	cell := len(mustMarshal(cands))
	sq, err := parseSkraafotoSearch(skraafotoDokk1 + "&path=56.15370,10.21395,56.15370,10.16500&ha=10&hb=20")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	k, _ := s.skraafotoAnswerKey(sq, skraafotoSearchKey{})
	g := skraafotoGround{pt: 3, a: 3, b: 3, final: true}
	withPath := len(mustMarshal(buildSkraafotoAnswer(cands, k, g, s.skraafotoLineFor(k, sq))))
	k.hasPath = false
	plain := len(mustMarshal(buildSkraafotoAnswer(cands, k, g, nil)))
	t.Logf("cell %d B, answer %d B, with a line of sight %d B", cell, plain, withPath)
	if cell > 16<<10 || skraafotoSearchBytes/cell < 1000 {
		t.Errorf("a cell is %d B: the search cache holds %d, not about 1000", cell, skraafotoSearchBytes/cell)
	}
	if withPath > 6<<10 || plain > 2560 || skraafotoSearchBytes/withPath < 2800 {
		t.Errorf("answers are %d and %d B", plain, withPath)
	}
}

// The path keys are the profile's grammar, bounded like its paths, each
// at most once, ha and hb only with a path. A refusal costs nothing.
func TestSkraafoto737M3a_QueryAllowList(t *testing.T) {
	elev := newFakeElevation(t, flatElev(3))
	e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{MaxPathKm: floatp(20)})
	const ok = "&path=56.15370,10.21395,56.15370,10.16500"
	for _, q := range []string{
		"&ha=10", "&hb=10", "&path=56.1,10.2,56.2", "&path=56.1,10.2,56.2,10.3,1", "&path=", "&path=56.1,10.2,,10.3",
		"&path=NaN,10.2,56.2,10.3", "&path=56.1,10.2,56.2,1e1", "&path=56.1,10.2,56.2,+10.3", "&path=91,10.2,56.2,10.3",
		"&path=56.1;10.2;56.2;10.3", "&path=" + strings.Repeat("a", 64), "&path=56.1,%2010.2,56.2,10.3",
		ok + "&ha=101", ok + "&ha=-1", ok + "&ha=1e1", ok + "&ha=NaN", ok + "&ha=", ok + "&ha=10&ha=11", ok + ok,
		ok + "&HA=10", "&PATH=56.15370,10.21395,56.15370,10.16500",
		"&path=56.15370,10.21395,56.15370,10.21395",  // under a metre
		"&path=56.15370,10.21395,56.15370,10.50000",  // 18 km: allowed
		"&path=56.15370,10.21395,56.15370,10.60000x", // garbage
		"&path=56.15370,10.21395,56.35370,10.21395",  // 22 km: over maxPathKm 20
	} {
		w := e.search(skraafotoDokk1 + q)
		if strings.Contains(q, "10.50000") {
			if w.Code != 200 {
				t.Errorf("%q: %d", q, w.Code)
			}
			continue
		}
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid skraafoto request") {
			t.Errorf("%q: %d %s", q, w.Code, w.Body.String())
		}
	}
	if s, _ := e.upstream(); s != 6 || elev.calls.Load() != 2 {
		t.Fatalf("%d STAC and %d DHM requests: refused queries went upstream", s, elev.calls.Load())
	}
	// A path is checked before the point's coverage: outside Denmark a
	// bad path is still refused.
	if w := e.search(skraafotoMalmo + "&path=55.60500,13.00380,55.60500,13.00380"); w.Code != 400 {
		t.Fatalf("Malmö with an empty path: %d", w.Code)
	}
}

func FuzzParseSkraafotoSearch(f *testing.F) {
	for _, s := range []string{
		skraafotoDokk1, skraafotoDokk1 + "&year=2025", skraafotoDokk1 + "&path=56.15370,10.21395,56.15370,10.16500&ha=10&hb=20.5",
		skraafotoDokk1 + "&path=56.1,10.2,56.2,10.3&hb=0", skraafotoDokk1 + "&path=56.1,10.2,56.2,10.3&ha=100&hb=99.9",
		// Just out of bounds: the properties below must hold for them too.
		skraafotoDokk1 + "&path=56.1,10.2,56.2,10.3&ha=100.000001", skraafotoDokk1 + "&path=56.1,10.2,56.2,10.3&hb=101",
		skraafotoDokk1 + "&hb=10", skraafotoDokk1 + "&path=56.1,10.2,56.2,180.1", "lat=1&lon=2&ha=3", "path=1,2,3,4&lat=0&lon=0", "lat=-90&lon=180&path=-90,-180,90,180",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		sq, err := parseSkraafotoSearch(raw)
		if err != nil {
			return
		}
		in := func(v, lim float64) bool { return v >= -lim && v <= lim && !math.IsNaN(v) }
		if !in(sq.lat, 90) || !in(sq.lon, 180) {
			t.Fatalf("%q: point %v,%v", raw, sq.lat, sq.lon)
		}
		if sq.year != 0 {
			if _, ok := skraafoto.CollectionYear(skraafoto.Collection(sq.year)); !ok {
				t.Fatalf("%q: year %d", raw, sq.year)
			}
		}
		if sq.path == nil {
			if q, _ := url.ParseQuery(raw); q.Has("ha") || q.Has("hb") {
				t.Fatalf("%q: an antenna height without a path", raw)
			}
			return
		}
		p := sq.path
		if !in(p.a.Lat, 90) || !in(p.a.Lon, 180) || !in(p.b.Lat, 90) || !in(p.b.Lon, 180) {
			t.Fatalf("%q: path %+v", raw, p)
		}
		for _, h := range []*float64{p.ha, p.hb} {
			if h != nil && !validAntennaM(*h) {
				t.Fatalf("%q: antenna %v", raw, *h)
			}
		}
		// The answer key of an accepted query is total: snapping never
		// fails or panics, and its key differs from the pathless one.
		s := &Server{}
		k, ok := s.skraafotoAnswerKey(sq, skraafotoSearchKey{})
		if ok && !k.hasPath {
			t.Fatalf("%q: path lost from the key", raw)
		}
		_ = fmt.Sprint(k)
	})
}

// BenchmarkSkraafotoAnswer is the work an answer adds to a search: five
// directions of five candidates each picked by projection, and a line
// of sight scanned and sampled in each photo. It runs once per uncached
// answer, never on a hot path.
func BenchmarkSkraafotoAnswer(b *testing.B) {
	cands := skraafotoBenchCandidates(b)
	sq, err := parseSkraafotoSearch(skraafotoDokk1 + "&path=56.15370,10.21395,56.15370,10.16500&ha=10&hb=20")
	if err != nil {
		b.Fatal(err)
	}
	s := &Server{}
	k, _ := s.skraafotoAnswerKey(sq, skraafotoSearchKey{})
	g := skraafotoGround{pt: 3, a: 3, b: 3, final: true}
	line := s.skraafotoLineFor(k, sq)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := buildSkraafotoAnswer(cands, k, g, line)
		if len(resp.Photos) != 5 || len(resp.Photos[0].Path) != 32 {
			b.Fatalf("%d photos", len(resp.Photos))
		}
	}
}

// skraafotoBenchCandidates is a full cell: five directions of
// skraafoto.SearchLimit fixture photos each (west is north's).
func skraafotoBenchCandidates(tb testing.TB) skraafotoCandidates {
	tb.Helper()
	feats := skraafotoFixtureFeatures(tb)
	cands := skraafotoCandidates{Collection: "skraafotos2025", Year: 2025, Years: []int{2025}}
	for _, d := range skraafoto.Directions {
		raw := feats[string(d)]
		if len(raw) == 0 {
			raw = feats["north"]
		}
		var items []skraafoto.Item
		for len(items) < skraafoto.SearchLimit {
			parsed, _, err := skraafoto.ParseSearch([]byte(`{"features":[`+raw[0]+`]}`), 2025, skraafoto.Direction(strings.Replace(string(d), "west", "north", 1)))
			if err != nil {
				tb.Fatal(err)
			}
			items = append(items, parsed...)
		}
		cands.Photos = append(cands.Photos, items)
	}
	return cands
}
