package dhm

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// #734 M1 — the browser names a DHM overlay by id; the server maps the id
// to the upstream service, layer and style. Every test runs against a fake
// upstream (httptest); none reaches Dataforsyningen.

// wantOverlayQuery is the upstream GetMap query for one tile of an
// overlay's upstream layer: #680's, with the layer's style. The styles are
// spelled out here, not taken from the table, so a changed style fails.
func wantOverlayQuery(layer string, k TileKey) url.Values {
	q := wantHillshadeQuery(layer, k)
	if layer == "dhm_kurve_traditionel" {
		q.Set("styles", "roed") // #734 M2
	}
	return q
}

// overlayTileQuery is the browser's tile query (Leaflet 1.9.4,
// L.tileLayer.wms) for tile k with layers = layers.
func overlayTileQuery(layers string, k TileKey) url.Values {
	q := url.Values{}
	for key, v := range map[string]string{
		"service": "WMS", "request": "GetMap", "version": "1.3.0", "layers": layers,
		"styles": "", "format": "image/png", "transparent": "true",
		"width": "256", "height": "256", "crs": "EPSG:3857",
	} {
		q.Set(key, v)
	}
	q.Set("bbox", formatBBox(k.BBox()))
	return q
}

// Every id maps to exactly its service, layer, style, zoom range, dataset
// and fallback. The values are spelled out here, not taken from the
// package, so a changed table fails.
func TestOverlay734_TableIsExact(t *testing.T) {
	cases := []struct {
		id, service, layer, style string
		minZ, maxZ                int
		datasetURL                string
	}{
		{"terrain", "dhm_DAF", "dhm_terraen_skyggekort", "", 7, 19, "https://dataforsyningen.dk/data/930"},
		{"surface", "dhm_DAF", "dhm_overflade_skyggekort", "", 7, 19, "https://dataforsyningen.dk/data/928"},
		// #734 M2: roed, the one named style that reads on both themes.
		{"contours", "dhm_DAF", "dhm_kurve_traditionel", "roed", 14, 19, "https://dataforsyningen.dk/data/926"},
		// #792: rendered here from the WCS DTM, z9-13 (elevation_792_test.go).
		{"elevation", "dhm_wcs_DAF", "dhm_terraen", "", 9, 13, "https://dataforsyningen.dk/data/930"},
	}
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, c.id)
		o, ok := LookupOverlay(c.id)
		if !ok {
			t.Fatalf("%s: not in the table", c.id)
		}
		if o.ID != c.id || o.Service != c.service || o.Layer != c.layer || o.Style != c.style ||
			o.MinZoom != c.minZ || o.MaxZoom != c.maxZ || o.DatasetURL != c.datasetURL {
			t.Errorf("%s = %+v", c.id, o)
		}
		if _, has := o.Fallback(); has {
			t.Errorf("%s has a fallback; only the exaggerated terrain upstream does", c.id)
		}
	}
	if got := OverlayIDs(); !reflect.DeepEqual(got, ids) {
		t.Errorf("OverlayIDs() = %v, want %v", got, ids)
	}
	// The ids are the only names LookupOverlay knows: no upstream layer,
	// no case variant, no legacy alias (that is ParseTileRequest's).
	for _, name := range []string{
		"", "Terrain", "SURFACE", " surface", "Elevation", "exaggerated", "hillshade",
		"dhm_terraen_skyggekort", "dhm_overflade_skyggekort", "dhm_kurve_traditionel",
		"quickDHM_skyggekort_terraen_overdrevet", "quickDHM_skyggekort_overflade",
	} {
		if o, ok := LookupOverlay(name); ok {
			t.Errorf("LookupOverlay(%q) = %+v", name, o)
		}
	}
}

// The terrain id is the configured map.dhm.layer chain (#687), exactly as
// before #734: exaggerated with the terrain layer as its fallback, or
// terrain alone. The other ids ignore the setting.
func TestOverlay734_TerrainIsTheConfiguredChain(t *testing.T) {
	terrain, _ := LookupOverlay("terrain")
	surface, _ := LookupOverlay("surface")
	contours, _ := LookupOverlay("contours")

	ex := terrain.Resolve(HillshadeExaggerated)
	if ex.ID != "terrain" || ex.Service != "wms/dhm_temp" || ex.Layer != "quickDHM_skyggekort_terraen_overdrevet" ||
		ex.Style != "" || ex.MinZoom != 7 || ex.MaxZoom != 19 || ex.DatasetURL != terrain.DatasetURL {
		t.Fatalf("exaggerated terrain = %+v", ex)
	}
	fb, ok := ex.Fallback()
	if !ok || !reflect.DeepEqual(fb, terrain) {
		t.Fatalf("exaggerated fallback = %+v, %v; want the terrain overlay", fb, ok)
	}
	if got := terrain.Resolve(HillshadeTerrain); !reflect.DeepEqual(got, terrain) {
		t.Fatalf("terrain with map.dhm.layer terrain = %+v", got)
	}
	for _, h := range []Hillshade{HillshadeTerrain, HillshadeExaggerated} {
		if got := surface.Resolve(h); !reflect.DeepEqual(got, surface) {
			t.Errorf("surface under %v = %+v", h, got)
		}
		if got := contours.Resolve(h); !reflect.DeepEqual(got, contours) {
			t.Errorf("contours under %v = %+v", h, got)
		}
	}
	// The #687 helpers are the same chain.
	if HillshadeExaggerated.service() != ex.Service || HillshadeExaggerated.layer() != ex.Layer ||
		HillshadeTerrain.service() != terrain.Service || HillshadeTerrain.layer() != terrain.Layer {
		t.Fatal("Hillshade service/layer disagree with the overlay table")
	}
}

// A caller cannot widen the allow-list by editing what it was handed.
func TestOverlay734_CopiesCannotWidenTheAllowList(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	terrain, _ := LookupOverlay("terrain")
	ex := terrain.Resolve(HillshadeExaggerated)
	fb, _ := ex.Fallback()
	fb.Layer = "dhm_kurve_0_25_m"
	terrain.Layer = "dhm_kurve_0_25_m"
	if o, _ := LookupOverlay("terrain"); o.Layer != "dhm_terraen_skyggekort" {
		t.Fatalf("table changed through a copy: %+v", o)
	}
	if again, _ := terrain.Resolve(HillshadeExaggerated).Fallback(); again.Layer != "dhm_terraen_skyggekort" {
		t.Fatalf("fallback changed through a copy: %+v", again)
	}
	if _, err := c.FetchOverlayAdmitted(context.Background(), nil, fb, someTile()); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("edited overlay fetched: %v", err)
	}
	if u.hits.Load() != 0 {
		t.Fatal("edited overlay reached upstream")
	}
}

func TestParseTileRequest734_AcceptsIdsAndTheLegacyAlias(t *testing.T) {
	cases := []struct {
		layers, id string
		minZ       int
	}{
		{"terrain", "terrain", 7},
		{"surface", "surface", 7},
		{"contours", "contours", 14},
		// #734 Q7: pages loaded before the deploy send the #680 layer
		// name; for one release it is a fixed alias of terrain.
		{"dhm_terraen_skyggekort", "terrain", 7},
	}
	for _, c := range cases {
		want, _ := LookupOverlay(c.id)
		for z := c.minZ; z <= MaxZoom; z++ {
			k := lonLatTile(z, 10.2, 56.16)
			o, got, err := ParseTileRequest(overlayTileQuery(c.layers, k))
			if err != nil || got != k || !reflect.DeepEqual(o, want) {
				t.Fatalf("layers=%s z%d: %+v %v %v; want %+v %v", c.layers, z, o, got, err, want, k)
			}
		}
	}
}

func TestParseTileRequest734_RejectsEverythingElse(t *testing.T) {
	k := lonLatTile(15, 10.2, 56.16)
	layers := func(v ...string) func(url.Values) {
		return func(q url.Values) { q["layers"] = v }
	}
	cases := map[string]func(url.Values){
		"unknown id":            layers("elevation"),
		"exaggerated is no id":  layers("exaggerated"),
		"surface layer name":    layers("dhm_overflade_skyggekort"),
		"contour layer name":    layers("dhm_kurve_traditionel"),
		"finer contours":        layers("dhm_kurve_0_5_m"),
		"quick DHM surface":     layers("quickDHM_skyggekort_overflade"),
		"exaggerated layer":     layers("quickDHM_skyggekort_terraen_overdrevet"),
		"case variant":          layers("Surface"),
		"upper case":            layers("TERRAIN"),
		"alias case variant":    layers("DHM_TERRAEN_SKYGGEKORT"),
		"leading space":         layers(" surface"),
		"trailing space":        layers("surface "),
		"comma list":            layers("terrain,surface"),
		"comma list with alias": layers("dhm_terraen_skyggekort,dhm_kurve_traditionel"),
		"repeated layers":       layers("surface", "surface"),
		"two ids":               layers("terrain", "surface"),
		"empty layers":          layers(""),
		"missing layers":        func(q url.Values) { q.Del("layers") },
		"Layers key":            func(q url.Values) { q["Layers"] = q["layers"]; q.Del("layers") },
		"style for surface":     func(q url.Values) { q.Set("layers", "surface"); q.Set("styles", "gul") },
		"jpeg surface":          func(q url.Values) { q.Set("layers", "surface"); q.Set("format", "image/jpeg") },
		"extra key":             func(q url.Values) { q.Set("layers", "surface"); q.Set("sld_body", "<x/>") },
		"token key":             func(q url.Values) { q.Set("layers", "surface"); q.Set("token", "x") },
		"TOKEN key":             func(q url.Values) { q.Set("layers", "surface"); q.Set("TOKEN", "x") },
	}
	for name, mut := range cases {
		q := overlayTileQuery("surface", k)
		mut(q)
		if o, got, err := ParseTileRequest(q); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: %+v %v %v, want ErrBadRequest", name, o, got, err)
		}
	}
	// A zoom outside the overlay's own range is refused, even where
	// another overlay accepts it.
	for z := MinZoom; z < 14; z++ {
		q := overlayTileQuery("contours", lonLatTile(z, 10.2, 56.16))
		if _, _, err := ParseTileRequest(q); !errors.Is(err, ErrBadRequest) {
			t.Errorf("contours z%d: err %v, want ErrBadRequest", z, err)
		}
		if _, _, err := ParseTileRequest(overlayTileQuery("surface", lonLatTile(z, 10.2, 56.16))); err != nil {
			t.Errorf("surface z%d: %v", z, err)
		}
	}
	for _, id := range []string{"terrain", "surface", "contours"} {
		for _, z := range []int{MinZoom - 1, MaxZoom + 1} {
			q := overlayTileQuery(id, lonLatTile(z, 10.2, 56.16))
			if _, _, err := ParseTileRequest(q); !errors.Is(err, ErrBadRequest) {
				t.Errorf("%s z%d: err %v, want ErrBadRequest", id, z, err)
			}
		}
	}
}

// overlayQueryCheck holds every caller of Client.get to the rebuilt query
// of one of the service's own overlays, at a zoom inside its range.
func TestOverlayQueryCheck734_DAF(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	k := lonLatTile(15, 10.2, 56.16)
	for _, layer := range []string{"dhm_terraen_skyggekort", "dhm_overflade_skyggekort", "dhm_kurve_traditionel"} {
		if _, err := c.Get(WMSService, wantOverlayQuery(layer, k), "image/png"); err != nil {
			t.Fatalf("%s: exact query rejected: %v", layer, err)
		}
	}
	if u.hits.Load() != 3 {
		t.Fatalf("upstream hits %d, want 3", u.hits.Load())
	}
	mutate := func(layer string, k TileKey, f func(url.Values)) url.Values {
		q := wantOverlayQuery(layer, k)
		f(q)
		return q
	}
	same := func(url.Values) {}
	bad := map[string]url.Values{
		"surface style gul": mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("styles", "gul") }),
		// #734 M2: the exact-style allow-list. An unknown style is a 200
		// ServiceException upstream (owner check T3), and every other
		// named style is another rendering.
		"contours no style":    mutate("dhm_kurve_traditionel", k, func(q url.Values) { q.Set("styles", "") }),
		"contours style gul":   mutate("dhm_kurve_traditionel", k, func(q url.Values) { q.Set("styles", "gul") }),
		"contours style hvid":  mutate("dhm_kurve_traditionel", k, func(q url.Values) { q.Set("styles", "hvid") }),
		"contours style ROED":  mutate("dhm_kurve_traditionel", k, func(q url.Values) { q.Set("styles", "ROED") }),
		"contours bad style":   mutate("dhm_kurve_traditionel", k, func(q url.Values) { q.Set("styles", "nosuchstyle") }),
		"surface style roed":   mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("styles", "roed") }),
		"surface jpeg":         mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("format", "image/jpeg") }),
		"surface extra key":    mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("exceptions", "XML") }),
		"surface token":        mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("token", testToken) }),
		"surface TOKEN":        mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("TOKEN", testToken) }),
		"surface browser TRUE": mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("transparent", "true") }),
		"surface 512 px":       mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("width", "512") }),
		"contours at z13":      mutate("dhm_kurve_traditionel", lonLatTile(13, 10.2, 56.16), same),
		"contours at z7":       mutate("dhm_kurve_traditionel", lonLatTile(7, 10.2, 56.16), same),
		"finer contours":       mutate("dhm_kurve_0_5_m", k, same),
		"spot heights":         mutate("dhm_kote_2_5_m", k, same),
		"two layers":           mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Set("layers", "dhm_kurve_traditionel,dhm_kote_2_5_m") }),
		"layers twice":         mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Add("layers", "dhm_overflade_skyggekort") }),
		"no layers":            mutate("dhm_overflade_skyggekort", k, func(q url.Values) { q.Del("layers") }),
		"an overlay id":        mutate("surface", k, same),
		"exaggerated layer":    mutate("quickDHM_skyggekort_terraen_overdrevet", k, same),
		"quick DHM surface":    mutate("quickDHM_skyggekort_overflade", k, same),
	}
	for name, q := range bad {
		if _, err := c.Get(WMSService, q, "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", name, err)
		}
	}
	if u.hits.Load() != 3 {
		t.Fatalf("%d off-shape queries reached upstream", u.hits.Load()-3)
	}
}

// The DAF overlays never reach the exaggerated service, nor its layer the
// DAF.
func TestOverlayQueryCheck734_ServicesStayApart(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	k := someTile()
	for _, layer := range []string{"dhm_terraen_skyggekort", "dhm_overflade_skyggekort", "dhm_kurve_traditionel", "quickDHM_skyggekort_overflade"} {
		if _, err := c.Get(ExaggeratedWMSService, wantHillshadeQuery(layer, lonLatTile(15, 10.2, 56.16)), "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s on wms/dhm_temp: err %v", layer, err)
		}
	}
	if _, err := c.Get(ExaggeratedWMSService, wantHillshadeQuery(ExaggeratedHillshadeLayer, k), "image/png"); err != nil {
		t.Fatalf("exaggerated query rejected: %v", err)
	}
	if u.hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1", u.hits.Load())
	}
}

// FetchOverlayAdmitted sends exactly the overlay's upstream query, with the
// token in the header and never in the URL.
func TestFetchOverlay734_UpstreamShape(t *testing.T) {
	for _, c := range []struct {
		id, path, layer string
		z               int
	}{
		{"terrain", "/dhm_DAF", "dhm_terraen_skyggekort", 10},
		{"surface", "/dhm_DAF", "dhm_overflade_skyggekort", 10},
		{"contours", "/dhm_DAF", "dhm_kurve_traditionel", 14},
	} {
		t.Run(c.id, func(t *testing.T) {
			u := newUpstream(t, nil)
			o, _ := LookupOverlay(c.id)
			k := lonLatTile(c.z, 10.2, 56.16)
			data, err := newTestClient(t, u, &logSink{}).FetchOverlayAdmitted(context.Background(), NewAdmission(4), o, k)
			if err != nil || len(data) != len(pngStub) {
				t.Fatalf("fetch: %d bytes, %v", len(data), err)
			}
			if u.paths[0] != c.path {
				t.Fatalf("path %q", u.paths[0])
			}
			got, _ := url.ParseQuery(u.rawQuery[0])
			if want := wantOverlayQuery(c.layer, k); got.Encode() != want.Encode() {
				t.Fatalf("upstream query\n got %s\nwant %s", got.Encode(), want.Encode())
			}
			if u.tokens[0] != testToken || strings.Contains(strings.ToLower(u.rawQuery[0]), "token") {
				t.Fatalf("token header %q, query %q", u.tokens[0], u.rawQuery[0])
			}
		})
	}
}

// #734 Q5: one dataset link per overlay, in the #733 credit's parts. The
// credit text itself is unchanged, so its dataset mark is the same.
func TestOverlay734_CreditLinks(t *testing.T) {
	want := map[string]string{
		"terrain":  "https://dataforsyningen.dk/data/930", // DHM/Terræn, as #733
		"surface":  "https://dataforsyningen.dk/data/928", // DHM/Overflade
		"contours": "https://dataforsyningen.dk/data/926", // DHM/Højdekurver
	}
	for id, u := range want {
		o, _ := LookupOverlay(id)
		if o.DatasetURL != u {
			t.Errorf("%s DatasetURL = %q, want %q", id, o.DatasetURL, u)
		}
		if strings.ContainsAny(o.DatasetURL, "<>&\"' ") || !strings.HasPrefix(o.DatasetURL, "https://") {
			t.Errorf("%s DatasetURL %q is not a plain https URL", id, o.DatasetURL)
		}
	}
	if terrain, _ := LookupOverlay("terrain"); terrain.DatasetURL != DatasetURL {
		t.Errorf("terrain's link %q left the #733 DatasetURL %q", terrain.DatasetURL, DatasetURL)
	}
	if !strings.Contains(Attribution, DatasetName) || strings.Contains(Attribution, DerivedNote) {
		t.Errorf("Attribution %q changed", Attribution)
	}
}

// The check costs one map lookup and one Encode per call, whatever the
// number of overlays on the service: it is on every cold tile's path.
func BenchmarkOverlayQueryCheck734(b *testing.B) {
	check := overlayQueryCheck(WMSService)
	q := wantHillshadeQuery("dhm_kurve_traditionel", lonLatTile(15, 10.2, 56.16))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := check(q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseTileRequest734(b *testing.B) {
	q := overlayTileQuery("surface", lonLatTile(15, 10.2, 56.16))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := ParseTileRequest(q); err != nil {
			b.Fatal(err)
		}
	}
}
