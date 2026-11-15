package heritage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"

	"github.com/meshcore-analyzer/dhm"
)

// Zone is one protection zone: the dataset fields the server reports and
// the geometry, polygons → rings → (easting, northing) in EPSG:25832
// metres, the first ring of each polygon its outer edge. The JSON tags are
// the cache encoding only; the server builds its own response.
type Zone struct {
	Type         string           `json:"type,omitempty"`     // anlaegstype
	Dating       string           `json:"dating,omitempty"`   // datering
	Status       string           `json:"status,omitempty"`   // fred_status
	ProtectionNo string           `json:"frednr,omitempty"`   // frednr
	SystemNo     string           `json:"systemnr,omitempty"` // systemnr
	Polygons     dhm.MultiPolygon `json:"polygons"`
}

// Page is one cell's zones. Truncated means the WFS had more zones in the
// box than MaxFeatures, so the nearest one may be missing.
type Page struct {
	Zones     []Zone `json:"zones"`
	Truncated bool   `json:"truncated,omitempty"`
}

var errShape = errors.New("heritage: unexpected response shape")

type featureCollection struct {
	Type          string          `json:"type"`
	Features      []feature       `json:"features"`
	NumberMatched json.RawMessage `json:"numberMatched"`
	CRS           *struct {
		Properties struct {
			Name string `json:"name"`
		} `json:"properties"`
	} `json:"crs"`
}

type feature struct {
	Type     string `json:"type"`
	Geometry *struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	} `json:"geometry"`
	Properties struct {
		Anlaegstype json.RawMessage `json:"anlaegstype"`
		Datering    json.RawMessage `json:"datering"`
		FredStatus  json.RawMessage `json:"fred_status"`
		Frednr      json.RawMessage `json:"frednr"`
		Systemnr    json.RawMessage `json:"systemnr"`
	} `json:"properties"`
}

// crsNames are the spellings GeoServer may give EPSG:25832 in a GeoJSON
// crs member. A response in any other CRS is refused rather than measured.
var crsNames = map[string]bool{
	CRS:                            true,
	"EPSG:25832":                   true,
	"urn:x-ogc:def:crs:EPSG:25832": true,
	"http://www.opengis.net/def/crs/EPSG/0/25832":   true,
	"http://www.opengis.net/gml/srs/epsg.xml#25832": true,
}

// coordWindow is the plausible EPSG:25832 window around Denmark, minE,
// minN, maxE, maxN, wide enough for every zone near the cell extent.
// Coordinates outside it mean the service answered in another CRS or axis
// order (lat/lon degrees, or northing first), which must not be measured as
// metres. dhm.PolygonReader enforces it (#730 M1a).
var coordWindow = [4]float64{100000, 5900000, 1100000, 6600000}

// Parse reads a WFS GeoJSON FeatureCollection of the zone layer. Anything
// off-shape is an error: another document type, trailing data, more than
// MaxFeatures features, a non-polygon geometry, a ring under four
// positions, coordinates outside the EPSG:25832 window, more than
// MaxVertices vertices, or a property that is an object, array or boolean.
// A feature without geometry is skipped.
func Parse(body []byte) (Page, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var doc featureCollection
	if err := dec.Decode(&doc); err != nil {
		return Page{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Page{}, errShape
	}
	if doc.Type != "FeatureCollection" || len(doc.Features) > MaxFeatures {
		return Page{}, errShape
	}
	if doc.CRS != nil && !crsNames[doc.CRS.Properties.Name] {
		return Page{}, errShape
	}
	page := Page{Zones: make([]Zone, 0, len(doc.Features)), Truncated: len(doc.Features) == MaxFeatures}
	var matched json.Number
	if json.Unmarshal(doc.NumberMatched, &matched) == nil {
		if n, err := matched.Int64(); err == nil && n > int64(len(doc.Features)) {
			page.Truncated = true
		}
	}
	geom := dhm.PolygonReader{Window: coordWindow, MaxVertices: MaxVertices}
	for i := range doc.Features {
		f := &doc.Features[i]
		if f.Type != "Feature" {
			return Page{}, errShape
		}
		if f.Geometry == nil {
			continue
		}
		polys, err := geom.Read(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return Page{}, err
		}
		z := Zone{Polygons: polys}
		for _, p := range []struct {
			dst *string
			raw json.RawMessage
		}{
			{&z.Type, f.Properties.Anlaegstype},
			{&z.Dating, f.Properties.Datering},
			{&z.Status, f.Properties.FredStatus},
			{&z.ProtectionNo, f.Properties.Frednr},
			{&z.SystemNo, f.Properties.Systemnr},
		} {
			if *p.dst, err = text(p.raw); err != nil {
				return Page{}, err
			}
		}
		page.Zones = append(page.Zones, z)
	}
	return page, nil
}

// text is a property as display text: a string or a number in its JSON
// form, cleaned and capped either way; null or absent is "". Any other
// JSON type is an error.
func text(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	switch c := raw[0]; {
	case c == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return cleanText(s), nil
	case c == '-' || c >= '0' && c <= '9':
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", err
		}
		// Capped like a string: a number may be any length in JSON.
		return cleanText(n.String()), nil
	}
	return "", errShape
}

// cleanText collapses whitespace, drops control and format characters
// (bidi overrides included) and keeps at most MaxTextRunes runes.
// json.Unmarshal has already replaced invalid UTF-8 with U+FFFD. The text
// is still escaped wherever it is shown; this only keeps it tidy.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > MaxTextRunes {
		s = string(r[:MaxTextRunes])
	}
	return s
}
