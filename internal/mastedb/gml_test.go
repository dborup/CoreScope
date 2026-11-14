package mastedb

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestParseSmallFixture(t *testing.T) {
	page, err := Parse(readFixture(t, "cell_small.gml"), testCell)
	if err != nil {
		t.Fatal(err)
	}
	if page.Returned != 9 || page.Truncated {
		t.Fatalf("Returned %d Truncated %v, want 9 false", page.Returned, page.Truncated)
	}
	// Row 1000040 lies on the east edge and belongs to the next cell.
	if len(page.Rows) != 8 {
		t.Fatalf("%d rows, want 8 (edge row dropped)", len(page.Rows))
	}
	byID := make(map[int64]Row)
	for _, r := range page.Rows {
		byID[r.ID] = r
	}
	if _, ok := byID[1000040]; ok {
		t.Error("edge row 1000040 not dropped")
	}
	if _, ok := byID[1000030]; !ok {
		t.Error("min-corner row 1000030 dropped")
	}
	want := Row{
		ID: 1000001, Station: "TEST-A1", E: 526400, N: 6254800,
		Street: "Prøvevej", HouseNumber: "12B", Postcode: "8800", City: "Testby Ø",
		Service: "Mobiltelefoni", Technology: "LTE", BandMHz: 800,
		Commissioned: "2019-03-01T23:00:00Z", ObjectType: "",
	}
	if got := byID[1000001]; got != want {
		t.Errorf("row 1000001 = %+v\nwant %+v", got, want)
	}
	// An empty Objekt_type, written either way, is "".
	for _, id := range []int64{1000001, 1000003} {
		if byID[id].ObjectType != "" {
			t.Errorf("row %d ObjectType %q, want empty", id, byID[id].ObjectType)
		}
	}
	// A missing Frekvensbaand element and a non-numeric one are unknown.
	if b := byID[1000010].BandMHz; b != 0 {
		t.Errorf("missing band parsed as %d", b)
	}
	if b := byID[1000021].BandMHz; b != 0 {
		t.Errorf("band \"ukendt\" parsed as %d", b)
	}
	if e := byID[1000020].E; e != 527500.5 {
		t.Errorf("fractional easting parsed as %v", e)
	}
}

func TestParseEmptyCollection(t *testing.T) {
	page, err := Parse(genGML(0, "0"), testCell)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 0 || page.Returned != 0 || page.Truncated {
		t.Fatalf("empty page = %+v", page)
	}
}

func TestParseTruncated(t *testing.T) {
	cases := []struct {
		rows      int
		matched   string
		truncated bool
	}{
		{PageLimit - 1, fmt.Sprint(PageLimit - 1), false},
		{PageLimit, fmt.Sprint(PageLimit), true},
		{PageLimit, "unknown", true},
		{10, "11", true},
		{10, "unknown", false},
		{10, "10", false},
		{10, "", false},
	}
	for _, tc := range cases {
		page, err := Parse(genGML(tc.rows, tc.matched), testCell)
		if err != nil {
			t.Fatalf("%d rows, matched %q: %v", tc.rows, tc.matched, err)
		}
		if page.Truncated != tc.truncated {
			t.Errorf("%d rows, matched %q: Truncated %v, want %v", tc.rows, tc.matched, page.Truncated, tc.truncated)
		}
		if len(page.Rows) != tc.rows {
			t.Errorf("%d rows, matched %q: parsed %d rows", tc.rows, tc.matched, len(page.Rows))
		}
	}
}

func TestParseBOM(t *testing.T) {
	body := append([]byte{0xEF, 0xBB, 0xBF}, readFixture(t, "cell_small.gml")...)
	if _, err := Parse(body, testCell); err != nil {
		t.Fatalf("BOM-prefixed response: %v", err)
	}
}

const oneMember = `<wfs:member><mastedatabasen:MasteIBrug gml:id="x">` +
	`<mastedatabasen:MasteID>%s</mastedatabasen:MasteID>` +
	`<mastedatabasen:UTM_E>%s</mastedatabasen:UTM_E><mastedatabasen:UTM_N>6255000</mastedatabasen:UTM_N>` +
	`</mastedatabasen:MasteIBrug></wfs:member>`

func collection(returned string, members ...string) []byte {
	var b bytes.Buffer
	b.WriteString(`<wfs:FeatureCollection xmlns:wfs="http://www.opengis.net/wfs/2.0" ` +
		`xmlns:mastedatabasen="www.mastedatabasen.dk" xmlns:gml="http://www.opengis.net/gml/3.2"`)
	if returned != "" {
		b.WriteString(` numberReturned="` + returned + `"`)
	}
	b.WriteString(`>`)
	for _, m := range members {
		b.WriteString(m)
	}
	b.WriteString(`</wfs:FeatureCollection>`)
	return b.Bytes()
}

// TestParseRejects: everything that is not a well-formed MasteIBrug
// collection fails closed with ErrBadResponse.
func TestParseRejects(t *testing.T) {
	good := fmt.Sprintf(oneMember, "1", "527000")
	if _, err := Parse(collection("1", good), testCell); err != nil {
		t.Fatalf("control case: %v", err)
	}
	full := readFixture(t, "cell_small.gml")
	cases := map[string][]byte{
		"HTML 500 page": []byte(`<!DOCTYPE html><html><head><title>Runtime Error</title></head>` +
			`<body><h1>Server Error in '/' Application.</h1></body></html>`),
		"XHTML page":                   []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body>error</body></html>`),
		"junk":                         []byte("\x00\x01not xml at all <<<>>>"),
		"empty body":                   nil,
		"plain text":                   []byte("Service unavailable"),
		"JSON":                         []byte(`{"error":"x"}`),
		"OWS exception":                []byte(`<ows:ExceptionReport xmlns:ows="http://www.opengis.net/ows/1.1" version="2.0.0"><ows:Exception exceptionCode="InvalidParameterValue"><ows:ExceptionText>bad</ows:ExceptionText></ows:Exception></ows:ExceptionReport>`),
		"WFS 1.1 namespace":            bytes.ReplaceAll(full, []byte("http://www.opengis.net/wfs/2.0"), []byte("http://www.opengis.net/wfs")),
		"cut off mid-body":             full[:len(full)/2],
		"no numberReturned":            collection("", good),
		"numberReturned text":          collection("ten", good),
		"numberReturned too high":      collection("2", good),
		"numberReturned too low":       collection("0", good),
		"member of another type":       collection("1", `<wfs:member><mastedatabasen:MasteIFremtid gml:id="x"/></wfs:member>`),
		"feature in another namespace": collection("1", `<wfs:member><x:MasteIBrug xmlns:x="urn:other"><x:MasteID>1</x:MasteID></x:MasteIBrug></wfs:member>`),
		"missing MasteID":              collection("1", fmt.Sprintf(oneMember, "", "527000")),
		"non-numeric MasteID":          collection("1", fmt.Sprintf(oneMember, "abc", "527000")),
		"missing easting":              collection("1", fmt.Sprintf(oneMember, "1", "")),
		"non-numeric easting":          collection("1", fmt.Sprintf(oneMember, "1", "527 000")),
		"NaN easting":                  collection("1", fmt.Sprintf(oneMember, "1", "NaN")),
		"infinite easting":             collection("1", fmt.Sprintf(oneMember, "1", "+Inf")),
		"Latin-1 declaration":          append([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?>`), collection("1", good)...),
	}
	for name, body := range cases {
		page, err := Parse(body, testCell)
		if !errors.Is(err, ErrBadResponse) {
			t.Errorf("%s: err %v, want ErrBadResponse", name, err)
		}
		if page.Rows != nil {
			t.Errorf("%s: rows returned alongside the error", name)
		}
	}
}

func TestParseBand(t *testing.T) {
	cases := map[string]int{
		"800": 800, " 900 ": 900, "26000": 26000,
		"": 0, "ukendt": 0, "0": 0, "-800": 0, "800.5": 0, "800 MHz": 0, "99999999": 0,
	}
	for in, want := range cases {
		if got := parseBand(in); got != want {
			t.Errorf("parseBand(%q) = %d, want %d", in, got, want)
		}
	}
}
