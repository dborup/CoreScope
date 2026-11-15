package buildings

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// testdata/dokk1_cell.json is one tokenless sample of the real service,
// fetched once on 2026-11-15 to build this fixture:
//
//	GET .../ogcapi/features/building_inspire/collections/building/items
//	    ?bbox=575250,6223750,575500,6224000&bbox-crs=<EPSG:25832 URI>
//	    &crs=<EPSG:25832 URI>&limit=100&f=application/geo%2Bjson
//
// It is the Dokk1 cell of the #730 research: 25 features, 282 vertices,
// answered as application/json, 31,199 bytes. The fixture keeps every
// feature, its id and geometry exactly, and 5 of its 25 properties (14,528
// bytes); the parser reads no property. Feature 19 is Dokk1 itself.
func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const dokk1Index = 19

func TestParseDokk1Fixture(t *testing.T) {
	page, err := Parse(readFixture(t, "dokk1_cell.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Footprints) != 25 || page.Truncated || page.Skipped != 0 {
		t.Fatalf("%d footprints, truncated %v, skipped %d", len(page.Footprints), page.Truncated, page.Skipped)
	}
	vertices := 0
	for _, fp := range page.Footprints {
		for _, poly := range fp {
			for _, ring := range poly {
				vertices += len(ring)
			}
		}
	}
	if vertices != 282 {
		t.Fatalf("%d vertices, want 282", vertices)
	}
	if got := page.Footprints[0][0][0][0]; got != [2]float64{575268.91, 6223852.76} {
		t.Fatalf("first vertex %v: easting, northing order", got)
	}
	if in, _ := dhm.Locate(page.Footprints[dokk1Index], 575424, 6223838); !in {
		t.Fatal("Dokk1's centre is not inside footprint 19")
	}
}

// fc is a FeatureCollection around features with the given extra
// top-level members.
func fc(features, extra string) []byte {
	return []byte(`{"type":"FeatureCollection","features":[` + features + `]` + extra + `}`)
}

func featureJSON(typ, coords string) string {
	return `{"type":"Feature","id":"building.1","geometry":{"type":"` + typ + `","coordinates":` + coords + `},"properties":{"inspireid":"1"}}`
}

// sq is a 10 m square ring at (e, n), closed.
func sq(e, n int) string {
	return fmt.Sprintf(`[[%d,%d],[%d,%d],[%d,%d],[%d,%d],[%d,%d]]`, e, n, e+10, n, e+10, n+10, e, n+10, e, n)
}

var goodFeature = featureJSON("Polygon", `[`+sq(575400, 6223800)+`]`)

// TestParseEmptyCollection: the service's answer for a corridor without
// buildings (147 bytes in the research) is an empty, complete page.
func TestParseEmptyCollection(t *testing.T) {
	body := []byte(`{"type":"FeatureCollection","features":[],"totalFeatures":0,"numberMatched":0,"numberReturned":0,"timeStamp":"2026-11-15T00:05:12.345Z","crs":null}`)
	page, err := Parse(body)
	if err != nil || len(page.Footprints) != 0 || page.Truncated || page.Skipped != 0 {
		t.Fatalf("%+v, %v", page, err)
	}
}

// TestParseTruncated: numberMatched above what came back, a next link, or
// a full page that does not say how many matched, each mean the segment's
// answer is incomplete, so its samples stay unclassified.
func TestParseTruncated(t *testing.T) {
	full := strings.TrimSuffix(strings.Repeat(goodFeature+",", MaxFeatures), ",")
	for name, tc := range map[string]struct {
		body []byte
		want bool
	}{
		"complete":                     {fc(goodFeature, `,"numberMatched":1,"numberReturned":1`), false},
		"more matched":                 {fc(goodFeature, `,"numberMatched":2,"numberReturned":1`), true},
		"next link":                    {fc(goodFeature, `,"links":[{"href":"https://x/items?startIndex=1","rel":"next"}]`), true},
		"other links only":             {fc(goodFeature, `,"numberMatched":1,"links":[{"href":"https://x/","rel":"self"},{"rel":"alternate"}]`), false},
		"full page, count given":       {fc(full, fmt.Sprintf(`,"numberMatched":%d`, MaxFeatures)), false},
		"full page, count missing":     {fc(full, ``), true},
		"full page, count null":        {fc(full, `,"numberMatched":null`), true},
		"short page, count missing":    {fc(goodFeature, ``), false},
		"skipped features still count": {fc(featureJSON("Point", `[575400,6223800]`), `,"numberMatched":1,"numberReturned":1`), false},
	} {
		page, err := Parse(tc.body)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if page.Truncated != tc.want {
			t.Errorf("%s: truncated %v, want %v", name, page.Truncated, tc.want)
		}
	}
}

// TestParseRejects: anything that is not this collection's GeoJSON is an
// error, so the segment stays unclassified rather than classified against
// nonsense. That includes the OGC API exception document, which GeoServer
// sends as JSON, and a page in another CRS or axis order.
func TestParseRejects(t *testing.T) {
	over := strings.TrimSuffix(strings.Repeat(goodFeature+",", MaxFeatures+1), ",")
	for name, body := range map[string][]byte{
		"empty body":            nil,
		"not JSON":              []byte(`<html>Service Unavailable</html>`),
		"OGC API exception":     []byte(`{"code":"InvalidParameterValue","description":"Invalid filter"}`),
		"OGC exception, typed":  []byte(`{"type":"InvalidParameterValue","title":"Bad filter","detail":"x"}`),
		"a single Feature":      []byte(goodFeature),
		"features missing":      []byte(`{"type":"FeatureCollection"}`),
		"features null":         []byte(`{"type":"FeatureCollection","features":null}`),
		"features an object":    []byte(`{"type":"FeatureCollection","features":{}}`),
		"trailing data":         append(fc(goodFeature, ``), []byte(` {"type":"FeatureCollection","features":[]}`)...),
		"trailing garbage":      append(fc(goodFeature, ``), 'x'),
		"too many features":     fc(over, ``),
		"numberReturned wrong":  fc(goodFeature, `,"numberReturned":2`),
		"numberMatched too low": fc(goodFeature+","+goodFeature, `,"numberMatched":1`),
		"numberMatched string":  fc(goodFeature, `,"numberMatched":"1"`),
		"numberMatched float":   fc(goodFeature, `,"numberMatched":1.5`),
		"numberMatched < 0":     fc(``, `,"numberMatched":-1`),
		"numberReturned < 0":    fc(``, `,"numberReturned":-1`),
		"feature type":          fc(`{"type":"Fature","geometry":null}`, ``),
		"geometry a string":     fc(`{"type":"Feature","geometry":"POLYGON((0 0))"}`, ``),
		"lat/lon page":          fc(featureJSON("Polygon", `[[[10.21,56.15],[10.22,56.15],[10.22,56.16],[10.21,56.15]]]`), ``),
		"swapped axes":          fc(featureJSON("Polygon", `[[[6223800,575400],[6223800,575410],[6223810,575410],[6223800,575400]]]`), ``),
		"one footprint lat/lon": fc(goodFeature+","+featureJSON("Polygon", `[[[10.21,56.15],[10.22,56.15],[10.22,56.16],[10.21,56.15]]]`), ``),
	} {
		if page, err := Parse(body); err == nil {
			t.Errorf("%s: accepted as %+v", name, page)
		}
	}
}

// TestParseSkipsBadFeatures: a feature without a usable footprint (no
// geometry, a point, a ring of three, coordinates that are not numbers) is
// skipped and counted, and the rest of the page is kept. An empty
// MultiPolygon is valid GeoJSON but no footprint either.
func TestParseSkipsBadFeatures(t *testing.T) {
	body := fc(strings.Join([]string{
		`{"type":"Feature","id":"building.2","geometry":null,"properties":{}}`,
		`{"type":"Feature","id":"building.3","properties":{}}`,
		featureJSON("Point", `[575400,6223800]`),
		featureJSON("Polygon", `[[[575400,6223800],[575410,6223800],[575400,6223800]]]`),
		featureJSON("Polygon", `[[["575400",6223800],[575410,6223800],[575410,6223810],[575400,6223800]]]`),
		featureJSON("Polygon", `[]`),
		featureJSON("MultiPolygon", `[]`), // empty geometry: no footprint (found by FuzzParse)
		goodFeature,
	}, ","), `,"numberMatched":8,"numberReturned":8`)
	page, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Footprints) != 1 || page.Skipped != 7 || page.Truncated {
		t.Fatalf("%d footprints, %d skipped, truncated %v", len(page.Footprints), page.Skipped, page.Truncated)
	}
}

// TestParseSkippedFeatureCostsNoBudget (#771 review): dhm.PolygonReader
// used to charge a refused geometry's validated rings to the budget. A bad
// feature that is skipped must leave the whole budget to the good ones.
// The bad feature here has an outer ring of budget-4 valid vertices and a
// hole of three positions; the good one after it has 5 vertices, which fit
// only if the bad one was refunded.
func TestParseSkippedFeatureCostsNoBudget(t *testing.T) {
	const budget = 100
	pts := make([]string, 0, budget-4)
	for i := 0; i < budget-5; i++ {
		pts = append(pts, fmt.Sprintf(`[%d,6223800]`, 575400+i))
	}
	pts = append(pts, `[575400,6223800]`)
	bad := featureJSON("Polygon", `[[`+strings.Join(pts, ",")+`],[[575401,6223801],[575402,6223801],[575401,6223801]]]`)
	page, err := parse(fc(bad+","+goodFeature, ``), budget)
	if err != nil {
		t.Fatalf("good feature after a skipped one refused: %v", err)
	}
	if len(page.Footprints) != 1 || page.Skipped != 1 {
		t.Fatalf("%d footprints, %d skipped", len(page.Footprints), page.Skipped)
	}
}

// TestParseVertexBudget: the budget is the page's, MaxVertices by default;
// overrunning it refuses the page rather than skipping every feature after
// the overrun, which would read as "no buildings here".
func TestParseVertexBudget(t *testing.T) {
	if _, err := parse(fc(goodFeature+","+goodFeature, ``), 10); err != nil {
		t.Fatalf("exactly the budget refused: %v", err)
	}
	if _, err := parse(fc(goodFeature+","+goodFeature+","+goodFeature, ``), 10); !errors.Is(err, dhm.ErrVertexBudget) {
		t.Fatalf("over the budget: err %v, want dhm.ErrVertexBudget", err)
	}
	if MaxVertices < 20*MaxFeatures {
		t.Fatalf("MaxVertices %d leaves a full page under 20 vertices per footprint", MaxVertices)
	}
}

// TestParseMultiPolygonWithHole: a courtyard is a hole, and a point in it
// is outside the footprint.
func TestParseMultiPolygonWithHole(t *testing.T) {
	outer := `[[575400,6223800],[575440,6223800],[575440,6223840],[575400,6223840],[575400,6223800]]`
	hole := `[[575410,6223810],[575430,6223810],[575430,6223830],[575410,6223830],[575410,6223810]]`
	page, err := Parse(fc(featureJSON("MultiPolygon", `[[`+outer+`,`+hole+`],[`+sq(575500, 6223800)+`]]`), ``))
	if err != nil {
		t.Fatal(err)
	}
	fp := page.Footprints[0]
	if len(fp) != 2 || len(fp[0]) != 2 {
		t.Fatalf("shape %d polygons, %d rings", len(fp), len(fp[0]))
	}
	for _, c := range []struct {
		e, n float64
		in   bool
	}{{575405, 6223805, true}, {575420, 6223820, false}, {575505, 6223805, true}} {
		if in, _ := dhm.Locate(fp, c.e, c.n); in != c.in {
			t.Errorf("(%v, %v): inside %v, want %v", c.e, c.n, in, c.in)
		}
	}
}

// FuzzParse: no input panics, and whatever is accepted is bounded and in
// the window, so the classifier can measure it.
func FuzzParse(f *testing.F) {
	f.Add(readFixture(f, "dokk1_cell.json"))
	f.Add([]byte(`{"type":"FeatureCollection","features":[],"numberMatched":0,"numberReturned":0}`))
	f.Add(fc(goodFeature, `,"numberMatched":3,"links":[{"rel":"next"}]`))
	f.Add(fc(featureJSON("MultiPolygon", `[[`+sq(575400, 6223800)+`,`+sq(575402, 6223802)+`]]`)+`,{"type":"Feature","geometry":null}`, ``))
	f.Add(fc(featureJSON("Polygon", `[[[10.2,56.1],[10.3,56.1],[10.3,56.2],[10.2,56.1]]]`), ``))
	f.Fuzz(func(t *testing.T, body []byte) {
		page, err := Parse(body)
		if err != nil {
			return
		}
		if len(page.Footprints)+page.Skipped > MaxFeatures || page.Skipped < 0 {
			t.Fatalf("%d footprints + %d skipped", len(page.Footprints), page.Skipped)
		}
		w, vertices := coordWindow, 0
		for _, fp := range page.Footprints {
			if len(fp) == 0 {
				t.Fatal("empty footprint")
			}
			for _, poly := range fp {
				for _, ring := range poly {
					if len(ring) < 4 {
						t.Fatalf("ring of %d", len(ring))
					}
					for _, p := range ring {
						if !(p[0] >= w[0] && p[0] <= w[2] && p[1] >= w[1] && p[1] <= w[3]) {
							t.Fatalf("position %v outside the window", p)
						}
					}
					vertices += len(ring)
				}
			}
		}
		if vertices > MaxVertices {
			t.Fatalf("%d vertices", vertices)
		}
	})
}
