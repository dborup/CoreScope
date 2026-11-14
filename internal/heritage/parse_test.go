package heritage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestParseFixture: the GeoServer FeatureCollection shape is read into the
// dataset fields we use, and nothing else (url, stednr, ... are dropped).
func TestParseFixture(t *testing.T) {
	page, err := Parse(readFixture(t, "zones_two.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Zones) != 2 || page.Truncated {
		t.Fatalf("%d zones, truncated %v", len(page.Zones), page.Truncated)
	}
	z := page.Zones[0]
	if z.Type != "Rundhøj" || z.Dating != "Oldtid" || z.Status != "Fredet" || z.ProtectionNo != "3624:15" || z.SystemNo != "190001" {
		t.Fatalf("fields %+v", z)
	}
	if len(z.Polygons) != 1 || len(z.Polygons[0]) != 1 || len(z.Polygons[0][0]) != 49 {
		t.Fatalf("geometry %d", len(z.Polygons))
	}
	if z2 := page.Zones[1]; z2.ProtectionNo != "" || z2.Type != "Vandfyldt grav" {
		t.Fatalf("null frednr %+v", z2)
	}
}

func fc(features string, extra string) []byte {
	return []byte(`{"type":"FeatureCollection","features":[` + features + `]` + extra + `}`)
}

func featureJSON(geomType, coords, props string) string {
	return `{"type":"Feature","geometry":{"type":"` + geomType + `","coordinates":` + coords + `},"properties":{` + props + `}}`
}

const square = `[[[526000,6179000],[526100,6179000],[526100,6179100],[526000,6179100],[526000,6179000]]]`

// TestParseRejectsWhatIsNotTheZoneLayer: anything off-shape is an error,
// so the server answers 502 instead of guessing.
func TestParseRejectsWhatIsNotTheZoneLayer(t *testing.T) {
	for name, body := range map[string][]byte{
		"not json":        []byte(`<ows:ExceptionReport>`),
		"exception json":  []byte(`{"exceptions":[{"code":"InvalidParameterValue"}]}`),
		"wrong type":      []byte(`{"type":"Feature","features":[]}`),
		"trailing data":   append(fc("", ""), []byte(`{}`)...),
		"lat/lon coords":  fc(featureJSON("Polygon", `[[[9.42,55.75],[9.43,55.75],[9.43,55.76],[9.42,55.75]]]`, ""), ""),
		"swapped axes":    fc(featureJSON("Polygon", `[[[6179000,526000],[6179000,526100],[6179100,526100],[6179000,526000]]]`, ""), ""),
		"northing off":    fc(featureJSON("Polygon", `[[[526000,55.75],[526100,55.75],[526100,55.76],[526000,55.75]]]`, ""), ""),
		"easting off":     fc(featureJSON("Polygon", `[[[9.42,6179000],[9.43,6179000],[9.43,6179100],[9.42,6179000]]]`, ""), ""),
		"point geometry":  fc(featureJSON("Point", `[526000,6179000]`, ""), ""),
		"short ring":      fc(featureJSON("Polygon", `[[[526000,6179000],[526100,6179000],[526000,6179000]]]`, ""), ""),
		"string coord":    fc(featureJSON("Polygon", `[[["526000",6179000],[526100,6179000],[526100,6179100],[526000,6179000]]]`, ""), ""),
		"one-value coord": fc(featureJSON("Polygon", `[[[526000],[526100,6179000],[526100,6179100],[526000,6179000]]]`, ""), ""),
		"other crs":       fc(featureJSON("Polygon", square, ""), `,"crs":{"type":"name","properties":{"name":"urn:ogc:def:crs:EPSG::4326"}}`),
		"object property": fc(featureJSON("Polygon", square, `"anlaegstype":{"a":1}`), ""),
		"not a feature":   fc(`{"type":"Something","geometry":null,"properties":{}}`, ""),
	} {
		if _, err := Parse(body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseAcceptsPolygonNumbersAndNulls(t *testing.T) {
	body := fc(featureJSON("Polygon", `[[[526000,6179000,0],[526100,6179000,0],[526100,6179100,0],[526000,6179100,0],[526000,6179000,0]]]`,
		`"anlaegstype":"Gravhøj","systemnr":12345,"frednr":null,"fred_status":"Fredet","datering":null`)+","+
		`{"type":"Feature","geometry":null,"properties":{"anlaegstype":"no geometry"}}`,
		`,"crs":{"type":"name","properties":{"name":"EPSG:25832"}},"numberMatched":"unknown"`)
	page, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Zones) != 1 {
		t.Fatalf("%d zones (a feature without geometry is skipped)", len(page.Zones))
	}
	if z := page.Zones[0]; z.SystemNo != "12345" || z.ProtectionNo != "" || z.Dating != "" || z.Type != "Gravhøj" {
		t.Fatalf("%+v", z)
	}
}

// TestParseCleansText: upstream text is bounded, valid UTF-8 and free of
// control characters; it is still escaped by the browser, this only keeps
// the response tidy.
func TestParseCleansText(t *testing.T) {
	long := strings.Repeat("ø", 300)
	body := fc(featureJSON("Polygon", square, `"anlaegstype":"  Rund\u0000høj\n\tmed  <b>tekst</b> ","datering":"`+long+`","fred_status":"`+"\xff"+`"`), "")
	page, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	z := page.Zones[0]
	if z.Type != "Rundhøj med <b>tekst</b>" {
		t.Fatalf("type %q", z.Type)
	}
	if n := len([]rune(z.Dating)); n != MaxTextRunes {
		t.Fatalf("dating %d runes", n)
	}
	if z.Status != "�" {
		t.Fatalf("status %q", z.Status)
	}
}

// TestParseNumericPropertyRuneCap (#762 review finding 3): a number is kept
// in its JSON form, but under the same MaxTextRunes cap as a string, so
// the cache entry and the response stay as small as documented.
func TestParseNumericPropertyRuneCap(t *testing.T) {
	long := strings.Repeat("7", 5000)
	body := fc(featureJSON("Polygon", square, `"frednr":`+long+`,"systemnr":-`+long+`.5e3`), "")
	page, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	z := page.Zones[0]
	if n := len([]rune(z.ProtectionNo)); n != MaxTextRunes {
		t.Fatalf("frednr kept %d chars (cap %d)", n, MaxTextRunes)
	}
	if n := len([]rune(z.SystemNo)); n != MaxTextRunes || !strings.HasPrefix(z.SystemNo, "-777") {
		t.Fatalf("systemnr kept %d chars (cap %d): %.10q", n, MaxTextRunes, z.SystemNo)
	}
}

func TestParseTruncation(t *testing.T) {
	var fs []string
	for i := 0; i < MaxFeatures; i++ {
		fs = append(fs, featureJSON("Polygon", square, ""))
	}
	page, err := Parse(fc(strings.Join(fs, ","), ""))
	if err != nil || !page.Truncated {
		t.Fatalf("a full page is not marked truncated: %v %v", page.Truncated, err)
	}
	page, err = Parse(fc(featureJSON("Polygon", square, ""), `,"numberMatched":7,"numberReturned":1`))
	if err != nil || !page.Truncated {
		t.Fatalf("numberMatched > returned not truncated: %v %v", page.Truncated, err)
	}
	fs = append(fs, featureJSON("Polygon", square, ""))
	if _, err := Parse(fc(strings.Join(fs, ","), "")); err == nil {
		t.Fatal("more features than count accepted")
	}
}

func TestParseBoundsVertices(t *testing.T) {
	var b strings.Builder
	b.WriteString("[[")
	for i := 0; i <= MaxVertices; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("[526000,6179000]")
	}
	b.WriteString("]]")
	if _, err := Parse(fc(featureJSON("Polygon", b.String(), ""), "")); err == nil {
		t.Fatal("over MaxVertices accepted")
	}
}
