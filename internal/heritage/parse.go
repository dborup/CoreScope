package heritage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
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
	Polygons     [][][][2]float64 `json:"polygons"`
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

// The plausible EPSG:25832 window around Denmark, wide enough for every
// zone near the cell extent. Coordinates outside it mean the service
// answered in another CRS or axis order (lat/lon degrees, or northing
// first), which must not be measured as metres.
const (
	minEasting, maxEasting   = 100000, 1100000
	minNorthing, maxNorthing = 5900000, 6600000
)

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
	vertices := 0
	for i := range doc.Features {
		f := &doc.Features[i]
		if f.Type != "Feature" {
			return Page{}, errShape
		}
		if f.Geometry == nil {
			continue
		}
		polys, n, err := polygons(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return Page{}, err
		}
		if vertices += n; vertices > MaxVertices {
			return Page{}, errShape
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

// polygons decodes Polygon or MultiPolygon coordinates and validates every
// position; it returns the vertex count too.
func polygons(typ string, raw json.RawMessage) ([][][][2]float64, int, error) {
	var multi [][][][]float64
	switch typ {
	case "MultiPolygon":
		if err := json.Unmarshal(raw, &multi); err != nil {
			return nil, 0, err
		}
	case "Polygon":
		var poly [][][]float64
		if err := json.Unmarshal(raw, &poly); err != nil {
			return nil, 0, err
		}
		multi = [][][][]float64{poly}
	default:
		return nil, 0, errShape
	}
	out := make([][][][2]float64, 0, len(multi))
	n := 0
	for _, poly := range multi {
		if len(poly) == 0 {
			return nil, 0, errShape
		}
		rings := make([][][2]float64, 0, len(poly))
		for _, ring := range poly {
			if len(ring) < 4 {
				return nil, 0, errShape
			}
			pts := make([][2]float64, len(ring))
			for i, pos := range ring {
				// A third value (height) is allowed and ignored.
				if len(pos) < 2 || pos[0] < minEasting || pos[0] > maxEasting || pos[1] < minNorthing || pos[1] > maxNorthing {
					return nil, 0, errShape
				}
				pts[i] = [2]float64{pos[0], pos[1]}
			}
			n += len(pts)
			rings = append(rings, pts)
		}
		out = append(out, rings)
	}
	return out, n, nil
}

// text is a property as display text: a string, cleaned, or a number in
// its JSON form; null or absent is "". Any other JSON type is an error.
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
		return n.String(), nil
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
