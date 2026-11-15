package main

// #737 M3a — the point, and a path's line of sight, in each photo of GET
// /api/skraafoto/search. Each photo's STAC item carries its camera model
// (internal/skraafoto Camera), so the server projects them itself, from
// the items the search already fetched: no other Skråfoto call.
//
//   - The point's height is the terrain (DTM) there, from DHM through the
//     terrain profile's own sampling (terrainProfiler.groundAt), under
//     its coalescing, admission and per-IP budget (map.dhm.profile). The
//     heights are cached per point, snapped like the profile (about a
//     metre). Without the profile, outside DHM, without data there, or
//     when DHM is refused or fails, each photo uses its footprint's mean
//     height instead and says approx; an answer is cached only when that
//     will not change on a retry.
//   - path=latA,lonA,latB,lonB, with ha and hb (antenna heights above the
//     ground, validated as the profile's), adds the line of sight between
//     the antennas: a straight line lowered by the earth's bulge at the
//     profile's k-factor, drawn with skraafotoPathPoints points where it
//     is in the frame. A missing height is the profile's point height.
//   - Per direction the search has up to skraafoto.SearchLimit photos;
//     the one shown is the one whose image centre is nearest the point,
//     in front and in frame first, else the API's first.
//   - Coordinates are preview pixels, x right and y down from the top-left
//     corner, of the preview the preview route serves (skraafoto.Frame);
//     the frame's size is in the answer.

import (
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/skraafoto"
)

const (
	// skraafotoPathPoints is how many points a line of sight is drawn
	// with in a photo.
	skraafotoPathPoints = 32
	// skraafotoGroundEntries bounds the height cache: 4 bytes a point.
	skraafotoGroundEntries = 16384
	skraafotoGroundTTL     = 7 * 24 * time.Hour
	// skraafotoAnswerEntries bounds the answer cache, within
	// skraafotoSearchBytes.
	skraafotoAnswerEntries = 4096
)

// skraafotoPathQuery is a requested path: its ends and, when given, their
// antenna heights.
type skraafotoPathQuery struct {
	a, b   dhm.LatLon
	ha, hb *float64
}

// skraafotoAnswerKey is the cache key of an answer: the search, the point
// and the path's ends snapped to 5 decimals (terrainSnap), the antenna
// heights in centimetres (-1: the default).
type skraafotoAnswerKey struct {
	search  skraafotoSearchKey
	pt      [2]int64
	hasPath bool
	path    [4]int64
	ha, hb  int64
}

// skraafotoGroundKey is a point snapped to 5 decimals.
type skraafotoGroundKey [2]int64

func snapLatLon(ll dhm.LatLon) [2]int64 {
	return [2]int64{terrainSnapCoord(ll.Lat), terrainSnapCoord(ll.Lon)}
}

func unsnapLatLon(k [2]int64) dhm.LatLon {
	return dhm.LatLon{Lat: float64(k[0]) / terrainSnap, Lon: float64(k[1]) / terrainSnap}
}

func antennaKey(h *float64) int64 {
	if h == nil {
		return -1
	}
	return int64(math.Round(*h * 100))
}

// skraafotoMaxPathM is the longest path accepted: the profile's.
func (s *Server) skraafotoMaxPathM() float64 {
	if s.terrain != nil {
		return s.terrain.set.maxPathM
	}
	return 1000 * terrainMaxPathKmDefault
}

// answerKey snaps the query into its answer key. ok is false for a path
// the profile would refuse: shorter than a metre or longer than its
// maxPathKm.
func (s *Server) skraafotoAnswerKey(sq skraafotoQuery, search skraafotoSearchKey) (skraafotoAnswerKey, bool) {
	k := skraafotoAnswerKey{search: search, pt: snapLatLon(dhm.LatLon{Lat: sq.lat, Lon: sq.lon}), ha: -1, hb: -1}
	if sq.path == nil {
		return k, true
	}
	a, b := snapLatLon(sq.path.a), snapLatLon(sq.path.b)
	if d := dhm.GroundDistance(unsnapLatLon(a), unsnapLatLon(b)); !(d >= 1) || d > s.skraafotoMaxPathM() {
		return k, false
	}
	k.hasPath, k.path = true, [4]int64{a[0], a[1], b[0], b[1]}
	k.ha, k.hb = antennaKey(sq.path.ha), antennaKey(sq.path.hb)
	return k, true
}

// skraafotoCandidates are a cold search's photos, as the search cache
// keeps them: per direction (skraafoto.Directions order) the API's
// candidates in its order; a direction without a photo is empty.
type skraafotoCandidates struct {
	Collection string
	Year       int
	Years      []int
	Photos     [][]skraafoto.Item
}

// SkraafotoFrame is a preview's size in pixels: the space of Point and
// Path.
type SkraafotoFrame struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// SkraafotoPixel is a position in a preview, x right and y down from its
// top-left corner, to a tenth of a pixel.
type SkraafotoPixel struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func skraafotoPixel(p skraafoto.Pixel) SkraafotoPixel {
	return SkraafotoPixel{X: math.Round(p.X*10) / 10, Y: math.Round(p.Y*10) / 10}
}

// skraafotoGround is the terrain under the point and the path's ends,
// NaN where it is not known; final is false when DHM was refused or
// failed, so an answer built on it is not cached.
type skraafotoGround struct {
	pt, a, b float64
	final    bool
}

// ground is the terrain at the snapped points, from the cache or DHM,
// each distinct point fetched once. Without the profile (t nil) it is
// unknown, finally. A request that fetches is charged once to the
// profile's per-IP budget, as a profile miss is; refused, its heights are
// unknown this once. A request whose every fetch was refused by admission
// gets its charge back.
func (p *skraafotoProxy) ground(r *http.Request, t *terrainProfiler, pts [][2]int64) ([]float64, bool) {
	hs := make([]float64, len(pts))
	miss := map[skraafotoGroundKey][]int{}
	for i, k := range pts {
		hs[i] = math.NaN()
		if b, ok := p.grounds.Get(skraafotoGroundKey(k), p.now()); ok {
			hs[i] = decodeGround(b)
			continue
		}
		miss[skraafotoGroundKey(k)] = append(miss[skraafotoGroundKey(k)], i)
	}
	if len(miss) == 0 || t == nil {
		return hs, true
	}
	if ok, _ := t.limiter.allow(r); !ok {
		return hs, false
	}
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		failed, busy int
	)
	for k, idx := range miss {
		wg.Add(1)
		go func(k skraafotoGroundKey, idx []int) {
			defer wg.Done()
			h, err := t.groundAt(r.Context(), unsnapLatLon(k))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if isTerrainBusy(err) {
					busy++
				}
				return
			}
			for _, i := range idx {
				hs[i] = h
			}
			p.grounds.Put(k, encodeGround(h), p.now())
		}(k, idx)
	}
	wg.Wait()
	if busy == len(miss) {
		t.limiter.refund(r)
		t.noteRefusal()
	}
	return hs, failed == 0
}

// skraafotoGroundFor is the terrain for an answer.
func (s *Server) skraafotoGroundFor(r *http.Request, k skraafotoAnswerKey) skraafotoGround {
	pts := [][2]int64{k.pt}
	if k.hasPath {
		pts = append(pts, [2]int64{k.path[0], k.path[1]}, [2]int64{k.path[2], k.path[3]})
	}
	hs, final := s.skraafoto.ground(r, s.terrain, pts)
	g := skraafotoGround{pt: hs[0], a: math.NaN(), b: math.NaN(), final: final}
	if k.hasPath {
		g.a, g.b = hs[1], hs[2]
	}
	return g
}

// skraafotoLine is a resolved line of sight.
type skraafotoLine struct {
	a, b   dhm.LatLon
	ha, hb float64 // antenna heights above the ground
	k      float64
}

// skraafotoLineFor resolves the path of k: its snapped ends, the request's
// antenna heights or the profile's point height, and the profile's
// k-factor. nil without a path.
func (s *Server) skraafotoLineFor(k skraafotoAnswerKey, sq skraafotoQuery) *skraafotoLine {
	if !k.hasPath {
		return nil
	}
	l := &skraafotoLine{
		a: unsnapLatLon([2]int64{k.path[0], k.path[1]}), b: unsnapLatLon([2]int64{k.path[2], k.path[3]}),
		k: dhm.DefaultKFactor,
	}
	point := terrainRoleAntennaM[terrainPointKey]
	if s.terrain != nil {
		point, _, _ = s.terrain.roleAntenna(terrainEnd{kind: "point"})
		l.k, _ = s.terrain.kFactor(nil)
	}
	l.ha, l.hb = point, point
	if sq.path.ha != nil {
		l.ha = *sq.path.ha
	}
	if sq.path.hb != nil {
		l.hb = *sq.path.hb
	}
	return l
}

// heightOr is h, or the photo's footprint height when h is not known;
// approx says which, ok whether there is any.
func heightOr(h float64, c *skraafoto.Camera) (v float64, approx, ok bool) {
	switch {
	case !math.IsNaN(h):
		return h, false, true
	case c.GroundOK:
		return c.GroundM, true, true
	}
	return 0, false, false
}

// skraafotoPick is the photo shown of a direction's candidates: the one
// whose image centre is nearest pt (normalised by the frame), among those
// it is in front of and in the frame of; else the API's first.
func skraafotoPick(cands []skraafoto.Item, e, n, ground float64) skraafoto.Item {
	best, bestD := 0, math.Inf(1)
	for i, it := range cands {
		c := it.Camera
		if c == nil {
			continue
		}
		h, _, ok := heightOr(ground, c)
		if !ok {
			continue
		}
		col, row, front := c.Project(e, n, h)
		x, y := col/float64(c.Cols), row/float64(c.Rows)
		if !front || x < 0 || x > 1 || y < 0 || y > 1 {
			continue
		}
		if d := math.Hypot(x-0.5, y-0.5); d < bestD {
			best, bestD = i, d
		}
	}
	return cands[best]
}

// skraafotoProject adds where pt (and line) are in ph's photo, camera c.
func skraafotoProject(ph *SkraafotoPhoto, c *skraafoto.Camera, e, n float64, g skraafotoGround, line *skraafotoLine) {
	if c == nil {
		return
	}
	h, approx, ok := heightOr(g.pt, c)
	if !ok {
		return
	}
	p, front := c.At(skraafoto.Ground{E: e, N: n, H: h})
	if !front || math.IsInf(p.X, 0) || math.IsInf(p.Y, 0) || math.IsNaN(p.X) || math.IsNaN(p.Y) {
		return
	}
	f := c.Frame()
	px := skraafotoPixel(p)
	ph.Frame, ph.Point, ph.InFrame, ph.Approx = &SkraafotoFrame{Width: f.Width, Height: f.Height}, &px, f.Contains(p), approx
	if line == nil {
		return
	}
	ga, apA, okA := heightOr(g.a, c)
	gb, apB, okB := heightOr(g.b, c)
	if !okA || !okB {
		return
	}
	ea, na := dhm.ToUTM32(line.a.Lat, line.a.Lon)
	eb, nb := dhm.ToUTM32(line.b.Lat, line.b.Lon)
	d := dhm.GroundDistance(line.a, line.b)
	sag := func(t float64) float64 { return dhm.EarthBulge(t*d, (1-t)*d, line.k) }
	pts := c.Line(skraafoto.Ground{E: ea, N: na, H: ga + line.ha}, skraafoto.Ground{E: eb, N: nb, H: gb + line.hb}, sag, skraafotoPathPoints)
	if len(pts) == 0 {
		return
	}
	ph.Path = make([]SkraafotoPixel, len(pts))
	for i, q := range pts {
		ph.Path[i] = skraafotoPixel(q)
	}
	ph.Approx = ph.Approx || apA || apB
}

// buildSkraafotoAnswer is the search response for the point at the
// snapped key k: per direction the photo picked, with the point and the
// line projected into it.
func buildSkraafotoAnswer(c skraafotoCandidates, k skraafotoAnswerKey, g skraafotoGround, line *skraafotoLine) SkraafotoSearchResponse {
	resp := SkraafotoSearchResponse{Collection: c.Collection, Year: c.Year, Years: c.Years,
		Photos: make([]SkraafotoPhoto, 0, len(c.Photos)), Credit: skraafotoCredit()}
	pt := unsnapLatLon(k.pt)
	e, n := dhm.ToUTM32(pt.Lat, pt.Lon)
	for _, cands := range c.Photos {
		if len(cands) == 0 {
			continue
		}
		it := skraafotoPick(cands, e, n, g.pt)
		ph := SkraafotoPhoto{ID: it.ID, Collection: it.Collection, Direction: string(it.Direction),
			Datetime: it.Datetime.Format(time.RFC3339), ViewerURL: skraafoto.ViewerLink(it.ID)}
		if it.Path != "" {
			ph.PreviewURL = "/api/skraafoto/preview/" + it.Collection + "/" + it.ID
		}
		skraafotoProject(&ph, it.Camera, e, n, g, line)
		resp.Photos = append(resp.Photos, ph)
	}
	return resp
}
