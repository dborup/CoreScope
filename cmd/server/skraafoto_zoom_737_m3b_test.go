package main

// #737 M3b — GET /api/skraafoto/preview/{c}/{id}?zoom=1&x=&y=: a crop of
// at most 1024 px around preview point (x, y) from a finer overview, with
// the point in the crop's pixels in a header. Against the skraafototest
// fakes only: no network, no token but the fakes' sentinel.

import (
	"fmt"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
	"github.com/meshcore-analyzer/skraafoto"
	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

// newSkraafotoZoomEnv serves photos of the real pyramid whose IFD2 (the
// crop level) and IFD4 (the preview's) are real JPEG.
func newSkraafotoZoomEnv(t *testing.T, sc SkraafotoConfig) *skraafotoEnv {
	t.Helper()
	e := newSkraafotoEnv(t, sc)
	cog := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: dhmtest.SkraafotoLevels, Pad: 1 << 20,
		Encode: func(l int) bool { return l == 2 || l == 4 }})
	e.up.COG = func(string) []byte { return cog }
	return e
}

func skraafotoZoomURL(c, id string, x, y float64) string {
	return fmt.Sprintf("/api/skraafoto/preview/%s/%s?zoom=1&x=%s&y=%s", c, id,
		strconv.FormatFloat(x, 'f', -1, 64), strconv.FormatFloat(y, 'f', -1, 64))
}

// parseSkraafotoFloats reads a comma-separated header of n numbers.
func parseSkraafotoFloats(t *testing.T, h string, n int) []float64 {
	t.Helper()
	parts := strings.Split(h, ",")
	if len(parts) != n {
		t.Fatalf("header %q: want %d numbers", h, n)
	}
	out := make([]float64, n)
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			t.Fatalf("header %q: %v", h, err)
		}
		out[i] = v
	}
	return out
}

func wantSkraafotoFloats(t *testing.T, what, h string, want ...float64) {
	t.Helper()
	got := parseSkraafotoFloats(t, h, len(want))
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.006 {
			t.Fatalf("%s %q, want %v", what, h, want)
		}
	}
}

// A zoom is a 1024 × 699 JPEG cut from IFD2 around the point, the marker
// at the point in the crop's pixels and the crop's box in the preview's.
// A point that snaps to the same box is a cache hit with its own marker;
// another box, and the plain preview, are entries of their own.
func TestSkraafoto737M3b_ZoomCrop(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	ph := resp.Photos[2]
	s0, _ := e.upstream()
	w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 560, 300))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" || w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("%d %q %q %s", w.Code, w.Header().Get("Content-Type"), w.Header().Get("X-CoreScope-Cache"), w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "public, max-age=604800" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", w.Header())
	}
	// Level pixel (2808.75, 1503.86) in the box at (2304, 1280).
	wantSkraafotoFloats(t, "marker", w.Header().Get("X-CoreScope-Skraafoto-Marker"), 504.75, 300*3504.0/699-1280)
	wantSkraafotoFloats(t, "crop", w.Header().Get("X-CoreScope-Skraafoto-Crop"),
		2304*1024.0/5136, 1280*699.0/3504, 3328*1024.0/5136, 1979*699.0/3504)
	cfg, err := jpeg.DecodeConfig(w.Body)
	if err != nil || cfg.Width != 1024 || cfg.Height != 699 {
		t.Fatalf("%+v %v", cfg, err)
	}
	// The search cached where the photo is: no STAC call; the header and
	// one read per tile row of the box (two).
	if s, c := e.upstream(); s != s0 || c < 2 || c > 3 {
		t.Fatalf("%d STAC, %d CDN requests", s-s0, c)
	}
	_, c0 := e.upstream()

	w = e.get(skraafotoZoomURL(ph.Collection, ph.ID, 565, 305))
	if w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" {
		t.Fatalf("same box: %d %q", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
	wantSkraafotoFloats(t, "marker", w.Header().Get("X-CoreScope-Skraafoto-Marker"), 565*5136.0/1024-2304, 305*3504.0/699-1280)
	if _, c := e.upstream(); c != c0 {
		t.Fatal("a cached crop went upstream")
	}
	if n := e.p.previews.Len(); n != 1 {
		t.Fatalf("%d cache entries, want 1", n)
	}

	if w := e.get(ph.PreviewURL); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "miss" || w.Header().Get("X-CoreScope-Skraafoto-Marker") != "" {
		t.Fatalf("plain preview: %d %q %v", w.Code, w.Header().Get("X-CoreScope-Cache"), w.Header())
	}
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 900, 100)); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("another box: %d %q", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
	if n := e.p.previews.Len(); n != 3 {
		t.Fatalf("%d cache entries, want the two crops and the preview", n)
	}
	for _, r := range e.up.Log() {
		if r.Host == skraafoto.CDNHost && r.Header.Get("token") != "" {
			t.Fatal("a token went to the CDN")
		}
	}
	for _, w := range e.seen {
		if strings.Contains(w.Body.String(), skraafototest.Token) || strings.Contains(fmt.Sprint(w.Header()), skraafototest.Token) {
			t.Fatal("the token is in a response")
		}
	}
}

// The crop's cache key is the photo, the level and the box: a plain
// preview never answers a zoom, nor a crop of one box another.
func TestSkraafoto737M3b_CacheKey(t *testing.T) {
	photo := skraafotoPhotoKey{"skraafotos2025", "2025_1"}
	f := skraafoto.ZoomFrame{PreviewWidth: 1024, PreviewHeight: 699, Level: 2, Width: 5136, Height: 3504, TileWidth: 512, TileHeight: 512}
	b, _ := f.Box(560, 300)
	k := skraafotoCropKey(photo, f, b)
	for name, other := range map[string]skraafotoImageKey{
		"plain preview": {photo: photo},
		"other photo":   skraafotoCropKey(skraafotoPhotoKey{"skraafotos2025", "2025_2"}, f, b),
		"other level":   skraafotoCropKey(photo, skraafoto.ZoomFrame{PreviewWidth: 1024, PreviewHeight: 699, Level: 3, Width: 5136, Height: 3504, TileWidth: 512, TileHeight: 512}, b),
		"other x":       skraafotoCropKey(photo, f, skraafoto.CropBox{X: b.X + 256, Y: b.Y, W: b.W, H: b.H}),
		"other y":       skraafotoCropKey(photo, f, skraafoto.CropBox{X: b.X, Y: b.Y + 256, W: b.W, H: b.H}),
	} {
		if other == k {
			t.Errorf("%s shares the key %+v", name, k)
		}
	}
	if b2, _ := f.Box(561, 301); skraafotoCropKey(photo, f, b2) != k {
		t.Error("a point in the same box has another key")
	}
}

// Concurrent cold zooms of one photo whose boxes share their X but not
// their Y are two flights, not one: each gets its own crop, with its
// marker inside it (v0.2.9 review nit, #829 N2).
func TestSkraafoto737M3b_ZoomFlightKeyedByTheWholeBox(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{DailyCap: intPtr(10)})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	ph := resp.Photos[2]
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 900, 100)); w.Code != 200 {
		t.Fatalf("priming zoom: %d", w.Code) // caches how the photo zooms
	}
	f, ok := e.p.zoomFrame(skraafotoPhotoKey{ph.Collection, ph.ID})
	if !ok {
		t.Fatal("no cached zoom frame")
	}
	pts := [2][2]float64{{560, 300}, {560, 100}}
	b0, _ := f.Box(pts[0][0], pts[0][1])
	b1, _ := f.Box(pts[1][0], pts[1][1])
	if b0.X != b1.X || b0.Y == b1.Y {
		t.Fatalf("setup: boxes %+v and %+v must share X only", b0, b1)
	}
	gate, open := skraafotoGate(t)
	e.up.CDNHook = func(w http.ResponseWriter, r *http.Request) bool {
		<-gate
		return false
	}
	var wg sync.WaitGroup
	got := make([]*httptest.ResponseRecorder, 2)
	for i, pt := range pts {
		wg.Add(1)
		go func(i int, x, y float64) {
			defer wg.Done()
			got[i] = e.get(skraafotoZoomURL(ph.Collection, ph.ID, x, y), skraafotoFrom(fmt.Sprintf("198.51.100.%d", i+1)))
		}(i, pt[0], pt[1])
	}
	skraafotoWait(t, func() bool { return e.p.flight.Pending() == 2 })
	open()
	wg.Wait()
	for i, w := range got {
		if w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "miss" {
			t.Fatalf("zoom %d: %d %q", i, w.Code, w.Header().Get("X-CoreScope-Cache"))
		}
		m := parseSkraafotoFloats(t, w.Header().Get("X-CoreScope-Skraafoto-Marker"), 2)
		if m[0] < 0 || m[1] < 0 || m[0] > float64(b0.W) || m[1] > float64(b0.H) {
			t.Fatalf("zoom %d: marker %v outside its crop", i, m)
		}
	}
	if got[0].Header().Get("X-CoreScope-Skraafoto-Crop") == got[1].Header().Get("X-CoreScope-Skraafoto-Crop") {
		t.Fatal("two boxes answered with one crop")
	}
	if n := e.p.previews.Len(); n != 3 {
		t.Fatalf("%d cache entries, want the priming crop and the two new ones", n)
	}
}

// zoom=1, x and y, each once, plain decimals within the preview's 1024
// px; anything else is the fixed 400 before any upstream call.
func TestSkraafoto737M3b_ZoomQueryAllowList(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{})
	base := "/api/skraafoto/preview/skraafotos2025/" + skraafototest.ItemID("skraafotos2025", "north") + "?"
	for _, q := range []string{
		"zoom=1", "zoom=1&x=10", "zoom=1&y=10", "x=10&y=10", "zoom=2&x=10&y=10", "zoom=0&x=10&y=10", "zoom=true&x=10&y=10",
		"zoom=1&x=10&y=10&x=11", "zoom=1&zoom=1&x=10&y=10", "zoom=1&x=10&y=10&token=x", "zoom=1&x=10&y=10&",
		"zoom=1&&x=10&y=10", "zoom=1&x=-1&y=10", "zoom=1&x=10&y=-0", "zoom=1&x=1e2&y=10", "zoom=1&x=0x10&y=10",
		"zoom=1&x=NaN&y=10", "zoom=1&x=Inf&y=10", "zoom=1&x=1024.5&y=10", "zoom=1&x=10&y=1025", "zoom=1&x=+10&y=10",
		"zoom=1&x=%2010&y=10", "zoom=1&x=10.&y=10", "zoom=1&x=.5&y=10", "zoom=1&x=10.1234567&y=10", "Zoom=1&x=10&y=10",
		"zoom=1&X=10&y=10", "zoom=1&x=10;y=10", "zoom=1&x=&y=10",
	} {
		if w := e.get(base + q); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid skraafoto request") {
			t.Errorf("%q: %d %s", q, w.Code, w.Body.String())
		}
	}
	if s, c := e.upstream(); s != 0 || c != 0 {
		t.Fatalf("a refused zoom went upstream: %d STAC, %d CDN", s, c)
	}
	if used := e.p.daily.usedNow(e.clock.now()); used != 0 {
		t.Fatalf("daily budget %d for refused zooms", used)
	}
}

// A point outside the preview (y past its 699 px) is the fixed 400:
// after one header read while the photo's frame is unknown, then from
// the cached frame with no upstream call and no charge.
func TestSkraafoto737M3b_ZoomOutsideFrame(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	ph := resp.Photos[0]
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 500, 1000)); w.Code != 400 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	_, c0 := e.upstream()
	if c0 != 1 {
		t.Fatalf("%d CDN requests, want the header", c0)
	}
	used := e.p.daily.usedNow(e.clock.now())
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 1000, 700)); w.Code != 400 {
		t.Fatalf("again: %d %s", w.Code, w.Body.String())
	}
	if _, c := e.upstream(); c != c0 {
		t.Fatal("a point outside a known frame went upstream")
	}
	if u := e.p.daily.usedNow(e.clock.now()); u != used {
		t.Fatalf("daily budget %d → %d", used, u)
	}
	// The frame's edge is in.
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 1024, 699)); w.Code != 200 {
		t.Fatalf("the corner: %d %s", w.Code, w.Body.String())
	}
}

// A zoom is a preview for fair use: a miss is charged to the client's
// preview budget and the daily budget, a hit to neither.
func TestSkraafoto737M3b_ZoomBudgets(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{PreviewRateLimitPerMin: intPtr(2), SearchRateLimitPerMin: intPtr(100)})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	ph := resp.Photos[1]
	day0 := e.p.daily.usedNow(e.clock.now())
	for i, c := range []struct {
		x, y  float64
		code  int
		state string
	}{
		{560, 300, 200, "miss"}, {565, 305, 200, "hit"}, {100, 100, 200, "miss"}, {562, 302, 200, "hit"}, {900, 600, 429, ""},
	} {
		w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, c.x, c.y))
		if w.Code != c.code || w.Header().Get("X-CoreScope-Cache") != c.state {
			t.Fatalf("request %d: %d %q", i, w.Code, w.Header().Get("X-CoreScope-Cache"))
		}
		if c.code == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
	}
	if used := e.p.daily.usedNow(e.clock.now()) - day0; used != 2 {
		t.Fatalf("daily budget %d for two cold zooms", used)
	}
	// The searches have their own budget.
	if w := e.search("lat=56.16&lon=10.21"); w.Code != 200 {
		t.Fatalf("search after the zooms: %d", w.Code)
	}
}

// The daily cap holds for zooms as for previews.
func TestSkraafoto737M3b_ZoomDailyCap(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{DailyCap: intPtr(2)})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus)) // 1
	ph := resp.Photos[3]
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 500, 300)); w.Code != 200 { // 2
		t.Fatalf("%d", w.Code)
	}
	w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 100, 600))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the cap: %d %v", w.Code, w.Header())
	}
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 502, 301)); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" {
		t.Fatalf("a cached crop over the cap: %d", w.Code)
	}
}

// A photo no search has cached is looked up as for a preview: the year
// list, the item, then the header and the box's tiles.
func TestSkraafoto737M3b_ZoomDeepLink(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{})
	id := skraafototest.ItemID("skraafotos2023", "west")
	if w := e.get(skraafotoZoomURL("skraafotos2023", id, 512, 349)); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	log := e.up.Log()
	if len(log) < 4 || log[0].Path != "/rest/skraafoto_api/v2/collections" ||
		log[1].Path != "/rest/skraafoto_api/v2/collections/skraafotos2023/items/"+id || log[2].Host != skraafoto.CDNHost {
		t.Fatalf("%+v", log)
	}
	if w := e.get(skraafotoZoomURL("skraafotos2023", "2023_83_36_9_0020_00003045", 10, 10)); w.Code != 404 {
		t.Fatalf("unknown photo: %d %s", w.Code, w.Body.String())
	}
}

// A failed crop (a 200 for a tile range read) is the fixed 502 and is
// not cached; the next request reads it again.
func TestSkraafoto737M3b_ZoomFailureNotCached(t *testing.T) {
	e := newSkraafotoZoomEnv(t, SkraafotoConfig{})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	ph := resp.Photos[4]
	e.up.CDNHook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			return false // the header is served
		}
		w.Write(make([]byte, 64))
		return true
	}
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 500, 300)); w.Code != 502 || !strings.Contains(w.Body.String(), "skraafoto preview unavailable") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if n := e.p.previews.Len(); n != 0 {
		t.Fatalf("%d cached after a failure", n)
	}
	e.up.CDNHook = nil
	if w := e.get(skraafotoZoomURL(ph.Collection, ph.ID, 500, 300)); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("after the failure: %d %q", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
}

// FuzzParseSkraafotoZoom: any query either is refused or gives a point
// inside [0, 1024]² that its own canonical query gives again.
func FuzzParseSkraafotoZoom(f *testing.F) {
	for _, s := range []string{"zoom=1&x=560&y=300", "zoom=1&x=0&y=1024", "zoom=1&x=1e3&y=1", "zoom=1&x=10&y=10&x=1", "x=1&y=2&zoom=1", "zoom=1&x=1023.999999&y=0.5", "%zz"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		x, y, err := parseSkraafotoZoom(raw)
		if err != nil {
			return
		}
		if !(x >= 0 && x <= skraafoto.PreviewMaxPx && y >= 0 && y <= skraafoto.PreviewMaxPx) {
			t.Fatalf("%q: (%v, %v)", raw, x, y)
		}
		canon := "zoom=1&x=" + strconv.FormatFloat(x, 'f', -1, 64) + "&y=" + strconv.FormatFloat(y, 'f', -1, 64)
		if x2, y2, err := parseSkraafotoZoom(canon); err != nil || x2 != x || y2 != y {
			t.Fatalf("%q → %q: (%v, %v) %v", raw, canon, x2, y2, err)
		}
	})
}
