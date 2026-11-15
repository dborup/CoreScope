package main

// #730 M2 — buildings versus trees in GET /api/terrain/profile. Every test
// runs the real buildings.Client and Fetcher against stubOGC, an httptest
// stand-in for the INSPIRE OGC API; nothing reaches Dataforsyningen.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/dhm"
)

// stubOGC answers the corridor queries with the footprints whose bounding
// box comes within the radius of the query's line, as a complete page,
// unless answer says otherwise. Every request is counted and kept.
type stubOGC struct {
	srv        *httptest.Server
	footprints [][][2]float64 // one closed ring each, EPSG:25832
	// answer, if set, replaces the answer for a line; ok false keeps the
	// default.
	answer  func(line [4]float64) (status int, body []byte, ok bool)
	delay   time.Duration
	hold    chan struct{} // if set, requests wait for it to close
	calls   atomic.Int32
	running atomic.Int32
	peak    atomic.Int32
	mu      sync.Mutex
	reqs    []*http.Request
}

var stubLineRe = regexp.MustCompile(`^DWITHIN\(geometry,LINESTRING\(([0-9.]+) ([0-9.]+),([0-9.]+) ([0-9.]+)\),([0-9]+),meters\)$`)

func newStubOGC(t *testing.T, footprints ...[][2]float64) *stubOGC {
	t.Helper()
	s := &stubOGC{footprints: footprints}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	// Registered after Close, so it runs first: held requests return.
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.hold != nil {
			select {
			case <-s.hold:
			default:
				close(s.hold)
			}
		}
	})
	return s
}

func (s *stubOGC) serve(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	n := s.running.Add(1)
	defer s.running.Add(-1)
	for p := s.peak.Load(); n > p && !s.peak.CompareAndSwap(p, n); p = s.peak.Load() {
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Clone(context.Background()))
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	m := stubLineRe.FindStringSubmatch(r.URL.Query().Get("filter"))
	if m == nil {
		http.Error(w, "bad filter", http.StatusBadRequest)
		return
	}
	var line [4]float64
	for i := range line {
		line[i], _ = strconv.ParseFloat(m[i+1], 64)
	}
	radius, _ := strconv.ParseFloat(m[5], 64)
	if s.answer != nil {
		if status, body, ok := s.answer(line); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write(body)
			return
		}
	}
	var features []string
	for i, ring := range s.footprints {
		if ringNearLine(ring, line, radius) {
			features = append(features, stubFeature(i, ring))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"type":"FeatureCollection","features":[%s],"numberMatched":%d,"numberReturned":%d}`,
		strings.Join(features, ","), len(features), len(features))
}

// ringNearLine: the ring's bounding box meets the line's grown by r, a
// superset of DWITHIN's answer that is exact for the axis-aligned lines
// these tests use.
func ringNearLine(ring [][2]float64, line [4]float64, r float64) bool {
	minE, maxE := math.Inf(1), math.Inf(-1)
	minN, maxN := math.Inf(1), math.Inf(-1)
	for _, v := range ring {
		minE, maxE = math.Min(minE, v[0]), math.Max(maxE, v[0])
		minN, maxN = math.Min(minN, v[1]), math.Max(maxN, v[1])
	}
	return maxE >= math.Min(line[0], line[2])-r && minE <= math.Max(line[0], line[2])+r &&
		maxN >= math.Min(line[1], line[3])-r && minN <= math.Max(line[1], line[3])+r
}

func stubFeature(id int, ring [][2]float64) string {
	pts := make([]string, len(ring))
	for i, v := range ring {
		pts[i] = fmt.Sprintf("[%.2f,%.2f]", v[0], v[1])
	}
	return fmt.Sprintf(`{"type":"Feature","id":"building.%d","geometry":{"type":"Polygon","coordinates":[[%s]]},"properties":{"inspireid":"%d"}}`,
		id, strings.Join(pts, ","), id)
}

// square is a closed axis-aligned ring of half-width h around (e, n).
func square(e, n, h float64) [][2]float64 {
	return [][2]float64{{e - h, n - h}, {e + h, n - h}, {e + h, n + h}, {e - h, n + h}, {e - h, n - h}}
}

// dokk1Footprint is Dokk1 as the stub serves it: the square the fake DSM
// raises by 28 m.
var dokk1Footprint = square(dokk1E, dokk1N, 20)

func (s *stubOGC) requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.reqs...)
}

// source is the production chain on the stub (newTerrainBuildingSource):
// the keyless client behind the server's Fetcher.
func (s *stubOGC) source(set terrainSettings) terrainBuildingSource {
	return newTerrainBuildingSource(set, s.srv.URL+"/ogcapi/features/building_inspire/collections/building/items")
}

type buildingsEnv struct {
	*terrainEnv
	ogc *stubOGC
	src *fakeElevation
}

// newBuildingsEnv is a profiler with the classification on, on fake
// elevations and ogc, with no per-client limit unless pc sets one.
func newBuildingsEnv(t *testing.T, dtm, dsm func(e, n float64) float32, ogc *stubOGC, pc dhm.ProfileConfig) *buildingsEnv {
	t.Helper()
	src := newFakeElevation(t, dtm)
	src.surface = dsm
	pc.Buildings.Enabled = true
	if pc.RateLimitPerMin == nil {
		pc.RateLimitPerMin = intp(0)
	}
	set := resolveTerrainSettings(pc, func(string, ...interface{}) {})
	env := newTerrainEnvWith(t, src, pc, nil, func(d *terrainProfilerDeps) { d.buildings = ogc.source(set) })
	return &buildingsEnv{terrainEnv: env, ogc: ogc, src: src}
}

// elevated is whether a served sample's surface stands above T.
func elevated(s TerrainSample) bool {
	return s.DTM != nil && s.DSM != nil && *s.DSM-*s.DTM > buildings.DefaultOptions().ThresholdM
}

// classesOf checks that exactly the elevated samples are classified when
// want is "full", and returns the served samples' classes by distance.
func classesOf(t *testing.T, resp TerrainProfileResponse) map[float64]string {
	t.Helper()
	out := map[float64]string{}
	for _, s := range resp.Samples {
		switch {
		case s.Cls != "" && !elevated(s):
			t.Fatalf("sample at %.2f m is %q but not elevated (dtm %v dsm %v)", s.D, s.Cls, *s.DTM, *s.DSM)
		case s.Cls != "" && s.Cls != "b" && s.Cls != "v":
			t.Fatalf("sample at %.2f m has class %q", s.D, s.Cls)
		case s.Cls == "" && elevated(s) && resp.Classification != nil && resp.Classification.State == "full":
			t.Fatalf("elevated sample at %.2f m unclassified in a full classification", s.D)
		}
		out[s.D] = s.Cls
	}
	return out
}

func wantClassification(t *testing.T, resp TerrainProfileResponse, state string, truncated bool) {
	t.Helper()
	c := resp.Classification
	if c == nil || c.State != state || c.Truncated != truncated {
		t.Fatalf("classification %+v, want {%s %v}", c, state, truncated)
	}
}

func wantCause(t *testing.T, resp TerrainProfileResponse, cause string) {
	t.Helper()
	if resp.Verdict.Surface == nil || resp.Verdict.Surface.Cause != cause {
		t.Fatalf("surface %+v, want cause %q", resp.Verdict.Surface, cause)
	}
}

// offAnswer is the golden answer with the classification off for a run
// (TestTerrainProfileGolden_BuildingsOff), decoded.
func offAnswer(t *testing.T, key string) (TerrainProfileResponse, string) {
	t.Helper()
	golden := readTerrainGolden(t)
	c, ok := golden[key]
	if !ok {
		t.Fatalf("golden has no %s", key)
	}
	var resp TerrainProfileResponse
	if err := json.Unmarshal([]byte(c.Body), &resp); err != nil {
		t.Fatal(err)
	}
	return resp, c.Body
}

// sameVerdictAsOff: apart from the classification's own fields and the
// credit, the answer is the golden answer without the classification, so
// neither verdict moved (#681 owner decision 6).
func sameVerdictAsOff(t *testing.T, resp TerrainProfileResponse, key string) {
	t.Helper()
	_, want := offAnswer(t, key)
	resp.Classification = nil
	resp.Attribution = dhm.AttributionDerived
	if resp.Verdict.Surface != nil {
		resp.Verdict.Surface.Cause = ""
	}
	for i := range resp.Samples {
		resp.Samples[i].Cls = ""
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(got)+"\n" != want { // writeJSON's encoder ends with a newline
		t.Fatalf("answer moved beyond the classification\n got: %s\nwant: %s", got, want)
	}
}

// ── The reference cases ───────────────────────────────────────────────────

// TestTerrainBuildings_Dokk1IsABuilding: the #729 reference path. The
// samples on Dokk1's roof are buildings, the surface verdict's cause is
// "building", the verdicts are unchanged, the credit names the
// footprints, and the one corridor request carries no token.
func TestTerrainBuildings_Dokk1IsABuilding(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
	w := env.get(pathQuery(dokk1A, dokk1B))
	resp := decodeProfile(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("cache %q", w.Header().Get("X-CoreScope-Cache"))
	}
	wantClassification(t, resp, "full", false)
	wantCause(t, resp, "building")
	cls := classesOf(t, resp)
	nearest, roof := math.Inf(1), ""
	for d, c := range cls {
		if c == "b" && (d < 118-10 || d > 158+10) {
			t.Fatalf("building at %.2f m, off Dokk1's 118-158 m", d)
		}
		if math.Abs(d-138) < nearest {
			nearest, roof = math.Abs(d-138), c
		}
	}
	if roof != "b" {
		t.Fatalf("the sample nearest 138 m is %q, want a building", roof)
	}
	if resp.Attribution != buildings.AttributionDerived {
		t.Fatalf("attribution %q", resp.Attribution)
	}
	sameVerdictAsOff(t, resp, "dokk1/0")

	reqs := ogc.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d corridor requests, want 1", len(reqs))
	}
	for _, r := range reqs {
		for k := range r.URL.Query() {
			if strings.EqualFold(k, "token") {
				t.Fatal("token in the corridor query")
			}
		}
		for _, h := range []string{"Token", "Authorization", "Cookie"} {
			if r.Header.Get(h) != "" {
				t.Fatalf("%s header sent to the footprint service", h)
			}
		}
	}
	if env.src.calls.Load() != 2 {
		t.Fatalf("%d rasters, want 2", env.src.calls.Load())
	}
}

// TestTerrainBuildings_ForestIsVegetation: 2 km of trees and no footprint
// are trees/other, sample for sample, and the cause is "vegetation".
func TestTerrainBuildings_ForestIsVegetation(t *testing.T) {
	ogc := newStubOGC(t)
	env := newBuildingsEnv(t, forestGround, forestSurface, ogc, dhm.ProfileConfig{})
	resp := decodeProfile(t, env.get(pathQuery(forestA, forestB)))
	wantClassification(t, resp, "full", false)
	wantCause(t, resp, "vegetation")
	trees := 0
	for _, c := range classesOf(t, resp) {
		switch c {
		case "b":
			t.Fatal("a building without a footprint")
		case "v":
			trees++
		}
	}
	if trees < 190 {
		t.Fatalf("%d samples of trees, want the ~200 of 2 km at 10 m", trees)
	}
	// No footprint is still data from the service: it is credited.
	if resp.Attribution != buildings.AttributionDerived {
		t.Fatalf("attribution %q", resp.Attribution)
	}
	sameVerdictAsOff(t, resp, "forest/0")
	if n := ogc.calls.Load(); n != 1 {
		t.Fatalf("%d corridor requests, want 1", n)
	}
}

// ── Failure, truncation, nothing to do ────────────────────────────────────

// TestTerrainBuildings_UpstreamFailureIsUnavailable: a failing footprint
// service never fails the profile. The answer is 200 with the same
// verdicts, the classification is unavailable, no sample has a class, the
// cause is "unknown", the credit is DHM's alone, and the failure is
// logged by category.
func TestTerrainBuildings_UpstreamFailureIsUnavailable(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func([4]float64) (int, []byte, bool)
		hold   bool
		reason string
	}{
		{"500", func([4]float64) (int, []byte, bool) { return 500, []byte("<h1>secret</h1>"), true }, false, "status 500"},
		{"oversize", func([4]float64) (int, []byte, bool) {
			return 200, []byte(`{"type":"FeatureCollection","features":[],"x":"` + strings.Repeat("a", 2<<20) + `"}`), true
		}, false, "oversize response"},
		{"not GeoJSON", func([4]float64) (int, []byte, bool) { return 200, []byte(`{"code":"InvalidParameterValue"}`), true }, false, "bad response"},
		{"timeout", nil, true, "timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ogc := newStubOGC(t, dokk1Footprint)
			ogc.answer = c.answer
			if c.hold {
				ogc.hold = make(chan struct{})
			}
			env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
			env.p.buildingsDeadline = 300 * time.Millisecond
			start := time.Now()
			resp := decodeProfile(t, env.get(pathQuery(dokk1A, dokk1B)))
			if took := time.Since(start); took > 3*time.Second {
				t.Fatalf("took %v: the building phase's deadline did not hold", took)
			}
			wantClassification(t, resp, "unavailable", false)
			wantCause(t, resp, "unknown")
			for d, cl := range classesOf(t, resp) {
				if cl != "" {
					t.Fatalf("sample at %.2f m classified %q without footprints", d, cl)
				}
			}
			if resp.Attribution != dhm.AttributionDerived {
				t.Fatalf("attribution %q credits footprints that were not used", resp.Attribution)
			}
			sameVerdictAsOff(t, resp, "dokk1/0")
			want := "[dhm] upstream terrain profile buildings failed (" + c.reason + ")"
			if logs := env.logs.String(); !strings.Contains(logs, want) || strings.Contains(logs, "secret") {
				t.Fatalf("log %q, want %q and no upstream text", logs, want)
			}
		})
	}
}

// fifteenKm is a 15 km path due east in mid-Jutland, two segments of
// 7.5 km, with 18 m of trees over all of it but the last 500 m at each end.
var fifteenA, fifteenB = utmPt(forestE, forestN), utmPt(forestE+15000, forestN)

func fifteenSurface(e, n float64) float32 {
	if e > forestE+500 && e < forestE+14500 {
		return 50 + 18
	}
	return 50
}

// firstSegment is whether a query line belongs to the path's first
// segment, the one that starts at A.
func firstSegment(line [4]float64) bool { return math.Min(line[0], line[2]) < forestE+100 }

// TestTerrainBuildings_TruncatedSegmentIsPartial: a segment with more
// buildings than one page holds leaves its samples unclassified, and the
// classification partial and truncated, whether the page says so
// (numberMatched) or simply holds more than the limit (#776 review N4);
// the other segment is classified.
func TestTerrainBuildings_TruncatedSegmentIsPartial(t *testing.T) {
	over := make([]string, buildings.MaxFeatures+1)
	for i := range over {
		over[i] = stubFeature(i, square(forestE+3000, forestN+100+float64(i), 0.4))
	}
	for name, page := range map[string]string{
		"numberMatched":    `{"type":"FeatureCollection","features":[],"numberMatched":1500,"numberReturned":0}`,
		"over MaxFeatures": `{"type":"FeatureCollection","features":[` + strings.Join(over, ",") + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ogc := newStubOGC(t)
			ogc.answer = func(line [4]float64) (int, []byte, bool) { return 200, []byte(page), firstSegment(line) }
			env := newBuildingsEnv(t, forestGround, fifteenSurface, ogc, dhm.ProfileConfig{})
			resp := decodeProfile(t, env.get(pathQuery(fifteenA, fifteenB)))
			wantClassification(t, resp, "partial", true)
			// Trees block in both segments, but those of the first are
			// not classified: there may be a building among them.
			wantCause(t, resp, "unknown")
			if n := ogc.calls.Load(); n != 2 {
				t.Fatalf("%d corridor requests, want 2", n)
			}
			for _, s := range resp.Samples {
				if !elevated(s) {
					continue
				}
				first := s.D < resp.DistanceM/2-50
				second := s.D > resp.DistanceM/2+50
				if first && s.Cls != "" || second && s.Cls != "v" {
					t.Fatalf("sample at %.0f m of %.0f: %q", s.D, resp.DistanceM, s.Cls)
				}
			}
		})
	}
}

// TestTerrainBuildings_NothingElevatedMakesNoCall: a surface that never
// stands above T needs no footprint: no request, a full classification,
// no class, no cause and DHM's credit alone.
func TestTerrainBuildings_NothingElevatedMakesNoCall(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	low := func(e, n float64) float32 {
		if inDokk1(e, n) {
			return 5 + 2.5 // T itself is not above T
		}
		return 5
	}
	env := newBuildingsEnv(t, dokk1Ground, low, ogc, dhm.ProfileConfig{})
	resp := decodeProfile(t, env.get(pathQuery(dokk1A, dokk1B)))
	wantClassification(t, resp, "full", false)
	for d, c := range classesOf(t, resp) {
		if c != "" {
			t.Fatalf("sample at %.2f m classified %q", d, c)
		}
	}
	if resp.Verdict.Surface.Cause != "" || resp.Attribution != dhm.AttributionDerived {
		t.Fatalf("cause %q, attribution %q", resp.Verdict.Surface.Cause, resp.Attribution)
	}
	if n := ogc.calls.Load(); n != 0 {
		t.Fatalf("%d corridor requests, want 0", n)
	}
}

// TestTerrainBuildings_OnlyFullSurfaceProfiles: model=dtm has no surface
// to classify (no classification at all), and a partly covered profile
// has no verdict to explain (unavailable); neither asks the service.
func TestTerrainBuildings_OnlyFullSurfaceProfiles(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	env := newBuildingsEnv(t, forestGround, forestSurface, ogc, dhm.ProfileConfig{})
	w := env.get(pathQuery(dokk1A, dokk1B, "model=dtm"))
	if strings.Contains(w.Body.String(), "classification") || strings.Contains(w.Body.String(), `"cls"`) {
		t.Fatalf("model=dtm classified: %s", w.Body.String())
	}
	resp := decodeProfile(t, env.get(pathQuery(partialA, partialB)))
	if resp.Coverage.State != "partial" {
		t.Fatalf("coverage %s", resp.Coverage.State)
	}
	wantClassification(t, resp, "unavailable", false)
	if resp.Verdict.Surface.Cause != "" {
		t.Fatalf("cause %q on an unknown surface", resp.Verdict.Surface.Cause)
	}
	// A path wholly outside DHM makes no call at all, and says the same.
	rasters := env.src.calls.Load()
	resp = decodeProfile(t, env.get(pathQuery("57.70000,11.95000", "57.71000,11.97000")))
	if resp.Coverage.State != "none" || env.src.calls.Load() != rasters {
		t.Fatalf("coverage %s, %d rasters", resp.Coverage.State, env.src.calls.Load()-rasters)
	}
	wantClassification(t, resp, "unavailable", false)
	if n := ogc.calls.Load(); n != 0 {
		t.Fatalf("%d corridor requests, want 0", n)
	}
}

// ── Budgets: cache, charge, admission, concurrency ────────────────────────

// TestTerrainBuildings_CacheHitMakesNoCall: a hit answers the same bytes
// without a corridor request, a raster or a per-IP charge, and a miss is
// charged once, building phase included.
func TestTerrainBuildings_CacheHitMakesNoCall(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{RateLimitPerMin: intp(2)})
	q := pathQuery(dokk1A, dokk1B)
	first := env.get(q)
	for i := 0; i < 3; i++ {
		w := env.get(q)
		if w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" || w.Body.String() != first.Body.String() {
			t.Fatalf("hit %d: %d %q, same body %v", i, w.Code, w.Header().Get("X-CoreScope-Cache"), w.Body.String() == first.Body.String())
		}
	}
	if n, r := ogc.calls.Load(), env.src.calls.Load(); n != 1 || r != 2 {
		t.Fatalf("%d corridor requests and %d rasters, want 1 and 2", n, r)
	}
	// The budget of 2 took one charge for the first miss: one more miss
	// fits, a third does not.
	if w := env.get(pathQuery(forestA, forestB)); w.Code != 200 {
		t.Fatalf("second miss: %d %s", w.Code, w.Body.String())
	}
	if w := env.get(pathQuery(fifteenA, fifteenB)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("third miss: %d, want 429", w.Code)
	}
}

// TestTerrainBuildings_SameFlightAndSlot: the building phase holds the
// profile's admission slot (maxInFlight 1 refuses another profile while it
// runs), and a request that joins the running fetch gets the same answer
// from the one corridor request.
func TestTerrainBuildings_SameFlightAndSlot(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	ogc.hold = make(chan struct{})
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{MaxInFlight: intp(1)})
	q := pathQuery(dokk1A, dokk1B)
	bodies := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() { bodies <- env.get(q).Body.String() }()
	}
	waitUntil(t, "the building phase and a joiner", func() bool { return ogc.running.Load() == 1 && env.p.flight.Pending() == 2 })
	if w := env.get(pathQuery(forestA, forestB)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("another profile during the building phase: %d, want 503", w.Code)
	}
	ogc.mu.Lock()
	close(ogc.hold)
	ogc.mu.Unlock()
	a, b := <-bodies, <-bodies
	if a != b || !strings.Contains(a, `"cause":"building"`) {
		t.Fatalf("answers differ or unclassified:\n%s\n%s", a, b)
	}
	if n := ogc.calls.Load(); n != 1 {
		t.Fatalf("%d corridor requests, want 1", n)
	}
}

// TestTerrainBuildings_LongPathAtMostFourAtOnce: a 79 km path with trees
// all along asks one request per 10 km segment, at most four at once, and
// classifies every elevated sample among the 1024 served.
func TestTerrainBuildings_LongPathAtMostFourAtOnce(t *testing.T) {
	ogc := newStubOGC(t)
	ogc.delay = 100 * time.Millisecond
	const e0, e1 = 480000.0, 559000.0
	trees := func(e, n float64) float32 {
		if e > e0+500 && e < e1-500 {
			return 50 + 18
		}
		return 50
	}
	env := newBuildingsEnv(t, forestGround, trees, ogc, dhm.ProfileConfig{})
	resp := decodeProfile(t, env.get(pathQuery(utmPt(e0, forestN), utmPt(e1, forestN))))
	if resp.Coverage.State != "full" || resp.SampleCount < 3000 || len(resp.Samples) != terrainMaxReturned {
		t.Fatalf("coverage %s, %d samples", resp.Coverage.State, resp.SampleCount)
	}
	wantClassification(t, resp, "full", false)
	wantCause(t, resp, "vegetation")
	trees2 := 0
	for _, c := range classesOf(t, resp) {
		if c == "v" {
			trees2++
		}
	}
	if trees2 < 900 {
		t.Fatalf("%d of %d served samples are trees", trees2, len(resp.Samples))
	}
	if n, p := ogc.calls.Load(), ogc.peak.Load(); n != 8 || p != 4 {
		t.Fatalf("%d corridor requests, %d at once; want 8, 4", n, p)
	}
}

// ── The cache entry ───────────────────────────────────────────────────────

// TestTerrainBuildings_EntryFormat: a classified entry decodes to what was
// encoded; an entry of another format, a bad state or a bad class byte is
// rejected.
func TestTerrainBuildings_EntryFormat(t *testing.T) {
	const n = 4
	nan := float32(math.NaN())
	e := terrainElevation{dtm: []float32{1, 2, 3, nan}, dsm: []float32{1, 9, 30, nan},
		classes: &terrainClasses{state: terrainClassPartial, truncated: true,
			cls: []buildings.Class{0, buildings.Vegetation, buildings.Building, 0}}}
	enc := e.encode()
	if len(enc) != 5+8*n+2+n || enc[4] != 1|terrainEntryClassified {
		t.Fatalf("entry %d bytes, format %d", len(enc), enc[4])
	}
	got, err := decodeTerrainElevation(enc, n, true, true)
	if err != nil || got.classes == nil {
		t.Fatalf("round trip: %v", err)
	}
	if c := got.classes; c.state != terrainClassPartial || !c.truncated || c.degraded ||
		string(classBytes(c.cls)) != string(classBytes(e.classes.cls)) || got.dsm[2] != 30 || !math.IsNaN(float64(got.dtm[3])) {
		t.Fatalf("round trip: %+v, classes %+v", got, *c)
	}
	// A degraded classification (#730 M3) round-trips, so a hit can
	// retry it.
	e.classes.degraded = true
	if got, err := decodeTerrainElevation(e.encode(), n, true, true); err != nil || !got.classes.degraded || !got.classes.truncated {
		t.Fatalf("degraded round trip: %+v, %v", got.classes, err)
	}
	e.classes.degraded = false
	legacy := terrainElevation{dtm: e.dtm, dsm: e.dsm}.encode()
	for name, c := range map[string]struct {
		b                []byte
		both, classified bool
	}{
		"legacy entry, classified profiler": {legacy, true, true},
		"classified entry, legacy profiler": {enc, true, false},
		"dtm entry, classified profiler":    {terrainElevation{dtm: e.dtm}.encode(), true, true},
		"wrong format byte":                 {withByte(enc, 4, 1), true, true},
		"bad state":                         {withByte(enc, 5+8*n, 9), true, true},
		"bad flags":                         {withByte(enc, 5+8*n+1, 4), true, true},
		"bad class":                         {withByte(enc, 5+8*n+2+1, 'x'), true, true},
		"short":                             {enc[:len(enc)-1], true, true},
		// #786 review F3: the flag bits above the classification's are
		// unknown, so an entry with one set is refused, never misread.
		"flag bit 5": {withByte(enc, 4, enc[4]|1<<5), true, true},
		"flag bit 6": {withByte(enc, 4, enc[4]|1<<6), true, true},
		"flag bit 7": {withByte(enc, 4, enc[4]|1<<7), true, true},
	} {
		if _, err := decodeTerrainElevation(c.b, n, c.both, c.classified); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The fill flags (#738) and the classification share the byte, and
	// both survive a round trip.
	mixed := e
	mixed.fill, mixed.fillFailed = fillNone, true
	if got, err := decodeTerrainElevation(mixed.encode(), n, true, true); err != nil || !got.fillFailed || got.classes == nil || got.classes.cls[2] != buildings.Building {
		t.Fatalf("fill flags with classes: %+v, %v", got, err)
	}
	mixed.fill, mixed.fillFailed = fillOverview, false
	if got, err := decodeTerrainElevation(mixed.encode(), n, true, true); err != nil || got.fill != fillOverview || got.classes == nil {
		t.Fatalf("fill level with classes: %+v, %v", got, err)
	}
	// The legacy formats still read as before.
	if got, err := decodeTerrainElevation(legacy, n, true, false); err != nil || got.classes != nil || got.dsm[2] != 30 {
		t.Fatalf("legacy entry: %+v, %v", got, err)
	}
}

func classBytes(cls []buildings.Class) []byte {
	out := make([]byte, len(cls))
	for i, c := range cls {
		out[i] = byte(c)
	}
	return out
}

func withByte(b []byte, i int, v byte) []byte {
	out := append([]byte(nil), b...)
	out[i] = v
	return out
}

// TestTerrainBuildings_OtherFormatIsFetchedAgain: an entry in the old
// format under a classified profile's key is not served as a hit, nor
// misread: the profile is fetched and classified again, and the entry
// replaced.
func TestTerrainBuildings_OtherFormatIsFetchedAgain(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
	q := pathQuery(dokk1A, dokk1B)
	vals, _ := url.ParseQuery(q)
	tq, err := parseTerrainQuery(vals)
	if err != nil {
		t.Fatal(err)
	}
	req, rerr := env.srv.resolveTerrainRequest(context.Background(), env.p, tq)
	if rerr != nil {
		t.Fatal(rerr)
	}
	n := len(req.plan.E)
	key := terrainKey{A: req.ka, B: req.kb, Both: true, Samples: n, StepCm: int64(math.Round(req.plan.StepM * 100))}
	env.p.cache.Put(key, terrainElevation{dtm: make([]float32, n), dsm: make([]float32, n)}.encode(), env.p.now())

	w := env.get(q)
	resp := decodeProfile(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("old entry served as %q", w.Header().Get("X-CoreScope-Cache"))
	}
	wantCause(t, resp, "building")
	data, ok := env.p.cache.Get(key, env.p.now())
	if !ok || data[4]&terrainEntryClassified == 0 {
		t.Fatal("old entry not replaced by a classified one")
	}
	if ogc.calls.Load() != 1 || env.src.calls.Load() != 2 {
		t.Fatalf("%d corridor requests, %d rasters", ogc.calls.Load(), env.src.calls.Load())
	}
}

// TestTerrainProfileGolden_BuildingsOffWithASource: off is off even with a
// footprint source at hand: the answers are the golden's, and the source
// is never asked.
func TestTerrainProfileGolden_BuildingsOffWithASource(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	got := terrainGoldenAnswers(t, func(src *fakeElevation) *terrainEnv {
		return newTerrainEnvWith(t, src, dhm.ProfileConfig{}, nil, func(d *terrainProfilerDeps) {
			d.buildings = ogc.source(resolveTerrainSettings(dhm.ProfileConfig{}, func(string, ...interface{}) {}))
		})
	})
	compareTerrainGolden(t, got, readTerrainGolden(t))
	if n := ogc.calls.Load(); n != 0 {
		t.Fatalf("%d corridor requests with the classification off", n)
	}
}

// TestTerrainBuildings_ConfigIsOffByDefault: map.dhm.profile.buildings is
// off unless enabled is true, and config.example.json ships it off.
func TestTerrainBuildings_ConfigIsOffByDefault(t *testing.T) {
	if resolveTerrainSettings(dhm.ProfileConfig{}, func(string, ...interface{}) {}).buildings {
		t.Fatal("on by default")
	}
	var pc dhm.ProfileConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"buildings":{"enabled":true}}`), &pc); err != nil || !resolveTerrainSettings(pc, func(string, ...interface{}) {}).buildings {
		t.Fatalf("enabled not read: %v", err)
	}
	// Enabled, the profiler builds the INSPIRE Fetcher itself (no request
	// is made until a profile needs one); off, it has no source at all.
	deps := terrainProfilerDeps{src: newFakeElevation(t, flatElev(0)), logf: func(string, ...interface{}) {}}
	if p := newTerrainProfiler(pc, nil, deps); p == nil || p.buildings == nil {
		t.Fatal("enabled classification without a source")
	} else if _, ok := p.buildings.(*buildings.Fetcher); !ok {
		t.Fatalf("source %T, want the INSPIRE Fetcher", p.buildings)
	}
	pc.Buildings.Enabled = false
	deps.buildings = newStubOGC(t).source(terrainSettings{maxInFlight: 1})
	if p := newTerrainProfiler(pc, nil, deps); p == nil || p.buildings != nil || p.classifies(true) {
		t.Fatal("classification off, but a source is wired")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Map struct {
			DHM struct {
				Profile struct {
					Buildings *struct {
						Enabled *bool `json:"enabled"`
					} `json:"buildings"`
				} `json:"profile"`
			} `json:"dhm"`
		} `json:"map"`
	}
	if err := json.Unmarshal(data, &ex); err != nil {
		t.Fatal(err)
	}
	if b := ex.Map.DHM.Profile.Buildings; b == nil || b.Enabled == nil || *b.Enabled {
		t.Fatalf("config.example.json map.dhm.profile.buildings.enabled must be present and false: %+v", b)
	}
}
