package copdem

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/copdem/copdemtest"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #738 M1 — the fill: which samples, which blocks, which level, and the
// sampled heights. The tiles are synthetic, served by copdemtest.

// testHeight is the synthetic terrain: a plane in lon/lat, so bilinear
// sampling is exact away from the clamped half pixel at tile edges.
func testHeight(lon, lat float64) float32 {
	return float32(100 + 50*(lon-12) + 30*(lat-55))
}

var (
	// tenth: a tenth of GLO-30's resolution in 64-pixel blocks (4 × 6;
	// overview 2 × 3): fast, and a path still crosses several blocks.
	tenth = copdemtest.Layout{W: 240, H: 360, Block: 64}
	// dense: a fifth, in 16-pixel blocks (2.1 × 2.5 km at 55° N; 30 × 45
	// blocks, overview 15 × 23), so a 40 km path is over budget at full
	// resolution and within it at the overview.
	dense = copdemtest.Layout{W: 480, H: 720, Block: 16}
)

// fillBucket serves every tile at layout l, each built when first asked
// for.
func fillBucket(t *testing.T, l copdemtest.Layout, value func(lon, lat float64) float32) *copdemtest.Bucket {
	b := newBucket(t)
	b.Generate = copdemtest.Synthetic(l, value)
	return b
}

// tol is the most a filled sample may differ from testHeight at layout
// l and level: the edge hold of up to one pixel at a tile or block edge
// (dhm.TiledLevel.Locate), at testHeight's slope, plus rounding.
func tol(l copdemtest.Layout, level int) float64 {
	px := float64(int(1) << level)
	return 1.05*px*(50/float64(l.W)+30/float64(l.H)) + 1e-3
}

// putTiles serves the given tiles at layout l.
func putTiles(b *copdemtest.Bucket, l copdemtest.Layout, value func(lon, lat float64) float32, tiles ...Tile) {
	for _, tl := range tiles {
		b.Put(tl.Path(), copdemtest.TileCOG(tl.Lat, tl.Lon, l, value))
	}
}

// Malmö is just east of 13° E: the path spans N55E012 and N55E013.
var cphMalmoTiles = []Tile{{55, 12}, {55, 13}}

var defaultPlan = dhm.PlanOptions{MaxSamples: 3200, ChunkM: 2500}

func plan(a, b dhm.LatLon) dhm.Plan { return dhm.PlanProfile(a, b, defaultPlan) }

var (
	copenhagen  = dhm.LatLon{Lat: 55.67610, Lon: 12.56830}
	malmo       = dhm.LatLon{Lat: 55.60500, Lon: 13.00380}
	sonderborg  = dhm.LatLon{Lat: 54.90980, Lon: 9.79220}
	kiel        = dhm.LatLon{Lat: 54.32330, Lon: 10.12280}
	aarhus      = dhm.LatLon{Lat: 56.16290, Lon: 10.20390}
	silkeborg   = dhm.LatLon{Lat: 56.16970, Lon: 9.54510}
	christianso = dhm.LatLon{Lat: 55.32040, Lon: 15.18730}
)

// toward is the point d metres from p on a bearing, in UTM 32.
func toward(p dhm.LatLon, d, bearingDeg float64) dhm.LatLon {
	e, n := dhm.ToUTM32(p.Lat, p.Lon)
	b := bearingDeg * math.Pi / 180
	lat, lon := dhm.FromUTM32(e+d*math.Sin(b), n+d*math.Cos(b))
	return dhm.LatLon{Lat: lat, Lon: lon}
}

// sampleLatLon is plan sample i in degrees.
func sampleLatLon(p *dhm.Plan, i int) (lat, lon float64) { return dhm.FromUTM32(p.E[i], p.N[i]) }

// checkFill: covered samples are never filled; every uncovered one has
// the terrain's height (within the half-pixel edge hold), 0 on sea tiles.
func checkFill(t *testing.T, p *dhm.Plan, r Result, value func(lon, lat float64) float32, tol float64) {
	t.Helper()
	filled := 0
	for i := range p.E {
		v := r.Values[i]
		if p.Inside(i) {
			if !math.IsNaN(float64(v)) {
				t.Fatalf("sample %d is covered by DHM but filled with %v", i, v)
			}
			continue
		}
		lat, lon := sampleLatLon(p, i)
		tl, _ := TileAt(lat, lon)
		want := float64(0)
		if tl.Listed() {
			want = float64(value(lon, lat))
		}
		if math.IsNaN(float64(v)) || math.Abs(float64(v)-want) > tol {
			t.Fatalf("sample %d (%.5f, %.5f, %v): %v, want %v", i, lat, lon, tl, v, want)
		}
		filled++
	}
	if filled != r.Filled {
		t.Fatalf("Filled %d, %d samples have a value", r.Filled, filled)
	}
}

// Copenhagen → Malmö: the Swedish end (and the coastal buffer) is filled,
// every DHM sample is left to DHM, and the fill is at full resolution.
func TestFillCopenhagenMalmo(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	p := plan(copenhagen, malmo)
	r, err := c.Fill(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Filled == 0 || r.Filled == len(p.E) || r.Level != 0 {
		t.Fatalf("filled %d of %d at level %d", r.Filled, len(p.E), r.Level)
	}
	checkFill(t, &p, r, testHeight, tol(tenth, 0))
	// The two tiles' headers and the blocks the uncovered samples are
	// in: no more.
	if got := b.Requests.Load(); got != int64(r.Calls) || r.Calls < 3 || r.Calls > dhm.MaxCallsPerModel {
		t.Fatalf("%d requests, Calls %d", got, r.Calls)
	}
	for _, req := range b.Log() {
		if req.Path != cphMalmoTiles[0].Path() && req.Path != cphMalmoTiles[1].Path() {
			t.Fatalf("request for %s", req.Path)
		}
	}
}

// A path DHM covers end to end makes no request at all.
func TestFillPureDHMPathMakesNoRequest(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	p := plan(aarhus, silkeborg)
	if p.Covered() != len(p.E) {
		t.Fatalf("Aarhus–Silkeborg: %d of %d covered", p.Covered(), len(p.E))
	}
	r, err := c.Fill(context.Background(), &p)
	if err != nil || r.Filled != 0 || r.Calls != 0 || b.Requests.Load() != 0 {
		t.Fatalf("%+v %v, %d requests", r, err, b.Requests.Load())
	}
	for i, v := range r.Values {
		if !math.IsNaN(float64(v)) {
			t.Fatalf("sample %d filled", i)
		}
	}
}

// Sønderborg → Kiel: Kiel is south of DHM's raster extent, not just on
// foreign land, and is filled; the path spans tiles N54E009 and N54E010.
func TestFillOutOfExtent(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	p := plan(sonderborg, kiel)
	r, err := c.Fill(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if e, n := p.E[len(p.E)-1], p.N[len(p.E)-1]; n >= dhm.RasterExtent[1] || p.Inside(len(p.E)-1) {
		t.Fatalf("Kiel at %v,%v should be south of the extent", e, n)
	}
	checkFill(t, &p, r, testHeight, tol(tenth, 0))
	paths := map[string]bool{}
	for _, req := range b.Log() {
		paths[req.Path] = true
	}
	if !paths[(Tile{54, 9}).Path()] || !paths[(Tile{54, 10}).Path()] || len(paths) != 2 {
		t.Fatalf("tiles requested: %v", paths)
	}
}

// A sample on a tile the bucket does not have is sea: 0 m, and that tile
// is never requested.
func TestFillSeaTileIsZeroWithoutRequest(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	end := toward(christianso, 62000, 90) // east of Christiansø, past 16° E
	if tl, _ := TileAt(end.Lat, end.Lon); tl != (Tile{55, 16}) || tl.Listed() {
		t.Fatalf("end in %v", tl)
	}
	p := plan(christianso, end)
	r, err := c.Fill(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	checkFill(t, &p, r, testHeight, tol(tenth, 0))
	if v := r.Values[len(p.E)-1]; v != 0 {
		t.Fatalf("end on the sea tile: %v, want 0", v)
	}
	for _, req := range b.Log() {
		if strings.Contains(req.Path, "E016") {
			t.Fatalf("sea tile requested: %s", req.Path)
		}
	}
}

// A listed tile that is missing upstream fails the fill: no value at all,
// never 0 m (#738 decision 4: the profile degrades to partial).
func TestFillListedTile404IsError(t *testing.T) {
	b := newBucket(t) // empty
	c := newTestClient(t, b)
	p := plan(copenhagen, malmo)
	r, err := c.Fill(context.Background(), &p)
	if dhm.FailureReason(err) != "status 404" {
		t.Fatalf("err %v, want status 404", err)
	}
	if r.Values != nil || r.Filled != 0 {
		t.Fatalf("a failed fill returned values: %+v", r)
	}
}

// A block that fails to decode fails the fill.
func TestFillCorruptBlockIsError(t *testing.T) {
	b := newBucket(t)
	for _, tl := range cphMalmoTiles {
		b.Put(tl.Path(), dhmtest.COG(dhmtest.COGOptions{
			Width: 240, Height: 360, Tile: 64, Lon0: float64(tl.Lon), Lat0: float64(tl.Lat + 1),
			PixelLon: 1.0 / 240, PixelLat: 1.0 / 360, Value: testHeight, Overview: true,
			Block: func(level, i int, raw []byte) []byte {
				out := append([]byte{}, raw...)
				out[len(out)-1] ^= 0xff // break every checksum
				return out
			},
		}))
	}
	p := plan(copenhagen, malmo)
	r, err := newTestClient(t, b).Fill(context.Background(), &p)
	if dhm.FailureReason(err) != "invalid block" || r.Values != nil {
		t.Fatalf("corrupt block: %+v %v", r, err)
	}
}

// Decoding a block inflates 4 MiB: at most Concurrency blocks are decoded
// at once, across every fill of the client.
func TestFillBoundsConcurrentDecodes(t *testing.T) {
	var mu sync.Mutex
	running, peak := 0, 0
	orig := decodeBlock
	decodeBlock = func(tt *dhm.TiledGeoTIFF, level, i int, raw []byte, buf []float32) (dhm.Grid, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		defer func() { mu.Lock(); running--; mu.Unlock() }()
		time.Sleep(5 * time.Millisecond)
		return orig(tt, level, i, raw, buf)
	}
	t.Cleanup(func() { decodeBlock = orig })
	b := fillBucket(t, dense, testHeight)
	c := newTestClient(t, b, func(o *Options) { o.Concurrency = 2 })
	var wg sync.WaitGroup
	for _, a := range []dhm.LatLon{{Lat: 55.40, Lon: 12.80}, {Lat: 55.60, Lon: 12.70}, {Lat: 55.90, Lon: 12.60}} {
		wg.Add(1)
		go func(a dhm.LatLon) {
			defer wg.Done()
			p := plan(a, toward(a, 40000, 80))
			if _, err := c.Fill(context.Background(), &p); err != nil {
				t.Error(err)
			}
		}(a)
	}
	wg.Wait()
	if peak < 2 || peak > 2 {
		t.Fatalf("peak %d concurrent decodes, want 2 (the client's Concurrency)", peak)
	}
}

// GLO-30 voids are filled, but a nodata pixel must still read as a gap,
// never as a height: here every pixel of N55E013 (Malmö) is NaN.
func TestFillNaNPixelIsAGap(t *testing.T) {
	hole := func(lon, lat float64) float32 {
		if lon >= 13 {
			return float32(math.NaN())
		}
		return testHeight(lon, lat)
	}
	b := newBucket(t)
	putTiles(b, tenth, hole, cphMalmoTiles...)
	p := plan(copenhagen, malmo)
	r, err := newTestClient(t, b).Fill(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	gaps, values := 0, 0
	for i := range p.E {
		if p.Inside(i) {
			continue
		}
		lat, lon := sampleLatLon(&p, i)
		got := float64(r.Values[i])
		switch tl, _ := TileAt(lat, lon); {
		case tl == (Tile{55, 13}) && !math.IsNaN(got):
			t.Fatalf("sample %d at %.4f,%.4f over the hole: %v", i, lat, lon, got)
		case tl == (Tile{55, 13}):
			gaps++
		case math.IsNaN(got):
			t.Fatalf("sample %d at %.4f,%.4f in N55E012 has no value", i, lat, lon)
		default:
			values++
		}
	}
	if gaps == 0 || values == 0 || r.Filled != values {
		t.Fatalf("%d gaps, %d values, Filled %d", gaps, values, r.Filled)
	}
}

// With small blocks a path needs more than the budget at full resolution:
// the fill takes the ½ overview (#738 decision 7), stays within budget,
// and its heights are still the terrain's.
func TestFillFallsBackToOverview(t *testing.T) {
	b := fillBucket(t, dense, testHeight)
	c := newTestClient(t, b)
	// Off Falsterbo into Skåne: 23 calls at full resolution, 13 at the
	// overview (measured at this layout).
	a := dhm.LatLon{Lat: 55.40, Lon: 12.80}
	p := plan(a, toward(a, 40000, 80))
	r, err := c.Fill(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != 1 || r.Calls > dhm.MaxCallsPerModel || b.Requests.Load() > dhm.MaxCallsPerModel {
		t.Fatalf("level %d, calls %d, %d requests", r.Level, r.Calls, b.Requests.Load())
	}
	checkFill(t, &p, r, testHeight, tol(dense, 1))
	if cost := fullResolutionCost(t, c, &p); cost <= dhm.MaxCallsPerModel {
		t.Fatalf("full resolution would cost %d: the case does not exercise the fallback", cost)
	}
}

// fullResolutionCost is what level 0 would cost for p.
func fullResolutionCost(t *testing.T, c *Client, p *dhm.Plan) int {
	t.Helper()
	pts, tiles, _ := c.points(p, make([]float32, len(p.E)))
	hs, err := c.readHeaders(context.Background(), tiles)
	if err != nil {
		t.Fatal(err)
	}
	cost, _ := levelCost(0, pts, tiles, hs)
	return cost
}

// Beyond the budget at both levels the fill refuses, after reading only
// the headers.
func TestFillOverBudget(t *testing.T) {
	b := newBucket(t)
	for _, tl := range cphMalmoTiles {
		b.Put(tl.Path(), dhmtest.COG(dhmtest.COGOptions{
			Width: 1024, Height: 1024, Tile: 16, Lon0: float64(tl.Lon), Lat0: float64(tl.Lat + 1),
			PixelLon: 1.0 / 1024, PixelLat: 1.0 / 1024, Value: testHeight, Overview: true, Uncompressed: true,
		}))
	}
	c := newTestClient(t, b)
	a := dhm.LatLon{Lat: 55.60, Lon: 12.70}
	p := plan(a, toward(a, 40000, 80))
	r, err := c.Fill(context.Background(), &p)
	if !errors.Is(err, ErrOverBudget) || r.Values != nil {
		t.Fatalf("%+v %v, want ErrOverBudget", r, err)
	}
	// The two headers, each 41 KiB of block tables: read in two parts.
	if got := b.Requests.Load(); got != 4 {
		t.Fatalf("%d requests, want the 2 headers in 2 reads each", got)
	}
}

// Whatever the path, a fill never sends more than the budget, and the
// cost it reports is an upper bound on what it sent.
func TestFillNeverExceedsBudget(t *testing.T) {
	b := fillBucket(t, dense, testHeight)
	rng := rand.New(rand.NewSource(738))
	for k := 0; k < 40; k++ {
		c := newTestClient(t, b) // cold caches
		a := dhm.LatLon{Lat: 54.6 + 3*rng.Float64(), Lon: 8.2 + 7*rng.Float64()}
		p := plan(a, toward(a, 1000+79000*rng.Float64(), 360*rng.Float64()))
		before := b.Requests.Load()
		r, err := c.Fill(context.Background(), &p)
		sent := b.Requests.Load() - before
		if sent > dhm.MaxCallsPerModel || (err == nil && sent > int64(r.Calls)) {
			t.Fatalf("path %d: %d requests, Calls %d, %v", k, sent, r.Calls, err)
		}
		if err != nil && !errors.Is(err, ErrOverBudget) {
			t.Fatalf("path %d: %v", k, err)
		}
	}
}

// Two concurrent fills that share blocks fetch each block once (dhm.Flight
// while both wait, the block cache after), and a warm fill makes no
// request and gives the same answer at the same level.
func TestFillSharesBlocksAndCaches(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	release, open := gate(t)
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Range") != "bytes=0-16383" {
			<-release
		}
		return false
	}
	p1 := plan(copenhagen, malmo)
	p2 := plan(dhm.LatLon{Lat: 55.68, Lon: 12.60}, malmo)
	var wg sync.WaitGroup
	var r1, r2 Result
	var e1, e2 error
	wg.Add(2)
	go func() { defer wg.Done(); r1, e1 = c.Fill(context.Background(), &p1) }()
	go func() { defer wg.Done(); r2, e2 = c.Fill(context.Background(), &p2) }()
	waitFor(t, func() bool { return c.flight.Pending() >= 2 })
	open()
	wg.Wait()
	if e1 != nil || e2 != nil {
		t.Fatal(e1, e2)
	}
	blocks := map[string]int{}
	for _, req := range b.Log() {
		blocks[req.Path+" "+req.Header.Get("Range")]++
	}
	for rg, n := range blocks {
		if n != 1 {
			t.Fatalf("range %s requested %d times", rg, n)
		}
	}
	before := b.Requests.Load()
	warm, err := c.Fill(context.Background(), &p1)
	if err != nil || b.Requests.Load() != before || warm.Level != r1.Level || warm.Calls != r1.Calls {
		t.Fatalf("warm fill: %+v %v, %d requests", warm, err, b.Requests.Load()-before)
	}
	for i := range warm.Values {
		if math.Float32bits(warm.Values[i]) != math.Float32bits(r1.Values[i]) {
			t.Fatalf("warm fill differs at %d", i)
		}
	}
	_ = r2
}

// A cancelled fill returns at once; the shared calls stop.
func TestFillCancel(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	}
	c := newTestClient(t, b, func(o *Options) { o.Concurrency = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	p := plan(copenhagen, malmo)
	go func() { _, err := c.Fill(ctx, &p); done <- err }()
	waitFor(t, func() bool { return b.Requests.Load() == 1 })
	cancel()
	if err := <-done; dhm.FailureReason(err) != "canceled" {
		t.Fatalf("err %v", err)
	}
	waitFor(t, func() bool { return c.adm.InUse() == 0 && len(c.sem) == 0 })
}

// A straight line in UTM 32 can leave a tile and come back: parallels
// curve in UTM, so a 60 km path between two points at 53.9996° N bows
// about 0.0008° north, into N54, in its middle. The tile it leaves and
// re-enters is read and counted once.
func TestFillTileVisitedTwiceCountsOnce(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	p := plan(dhm.LatLon{Lat: 53.9996, Lon: 11.05}, dhm.LatLon{Lat: 53.9996, Lon: 11.95})
	var seq []Tile
	for i := range p.E {
		lat, lon := sampleLatLon(&p, i)
		if tl, _ := TileAt(lat, lon); len(seq) == 0 || seq[len(seq)-1] != tl {
			seq = append(seq, tl)
		}
	}
	if len(seq) != 3 || seq[0] != seq[2] || seq[0] != (Tile{53, 11}) || seq[1] != (Tile{54, 11}) {
		t.Fatalf("tiles along the path %v, want N53E011, N54E011, N53E011", seq)
	}
	r, err := c.Fill(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	checkFill(t, &p, r, testHeight, tol(tenth, 0))
	if got := b.Requests.Load(); got != int64(r.Calls) {
		t.Fatalf("%d requests, Calls %d: a tile counted twice", got, r.Calls)
	}
}

// A plan over more tiles than half the budget is refused before any
// header is read: a header can cost two reads, so the headers alone could
// exceed it.
func TestFillTooManyTilesNoRequest(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	p := plan(dhm.LatLon{Lat: 53.1, Lon: 6.1}, dhm.LatLon{Lat: 58.9, Lon: 16.9}) // ≈ 900 km across the region
	_, tiles, _ := c.points(&p, make([]float32, len(p.E)))
	if 2*len(tiles) <= dhm.MaxCallsPerModel {
		t.Fatalf("%d tiles: the case does not exceed half the budget", len(tiles))
	}
	if _, err := c.Fill(context.Background(), &p); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("err %v", err)
	}
	if got := b.Requests.Load(); got != 0 {
		t.Fatalf("%d requests", got)
	}
}

// Samples outside the tile list's region get no value: unknown, not sea.
func TestFillOutsideRegionIsUnknown(t *testing.T) {
	b := fillBucket(t, tenth, testHeight)
	c := newTestClient(t, b)
	a := dhm.LatLon{Lat: 52.6, Lon: 9.0} // south of N53: no DHM sample either
	p := plan(a, toward(a, 20000, 0))
	r, err := c.Fill(context.Background(), &p)
	if err != nil || r.Filled != 0 || b.Requests.Load() != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	for i := range r.Values {
		if lat, _ := sampleLatLon(&p, i); lat < 53 && !math.IsNaN(float64(r.Values[i])) {
			t.Fatalf("sample %d at %.3f N filled", i, lat)
		}
	}
}
