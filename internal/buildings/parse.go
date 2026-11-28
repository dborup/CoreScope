package buildings

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/meshcore-analyzer/dhm"
)

// MaxFeatures is the query's limit: the most footprints one segment's
// request returns. A page that reports more matches, or links a next page,
// is Truncated. The research's densest 10 km at r = 15 m (Aarhus C) had
// 493 buildings.
const MaxFeatures = 1000

// MaxVertices bounds the vertices of one page, so a hostile or broken
// response cannot make the classification work unbounded. The Dokk1 cell
// averages 11 vertices per footprint; a full page of MaxFeatures at that
// rate is about 11,000. The 2 MiB body cap allows ~120,000 at the shortest
// possible position.
const MaxVertices = 50000

// Page is one segment's footprints, each a building's polygons in
// EPSG:25832 metres. Truncated means the service had more buildings in the
// corridor than it returned, so the segment's samples stay unclassified.
// Skipped counts features without a usable footprint (no geometry, not a
// polygon, a ring under four positions, an empty MultiPolygon), which were
// left out; a page that skipped too many is not used either (usable).
//
// A Page may be shared between callers (Fetcher): treat it as read-only.
// The JSON tags are the cache encoding only.
type Page struct {
	Footprints []dhm.MultiPolygon `json:"footprints"`
	Truncated  bool               `json:"truncated,omitempty"`
	Skipped    int                `json:"skipped,omitempty"`
}

// maxSkippedOneIn: a page that skipped more than 1 in maxSkippedOneIn of
// its features is not used.
const maxSkippedOneIn = 20

// usable reports whether the page can classify its segment: complete, and
// with at most 1 in maxSkippedOneIn of its features skipped. A skipped
// feature is a building whose footprint was lost, and its samples would
// read as Vegetation; a page that lost them all would read as a corridor
// without buildings (#776 review N3). Real pages skip none.
func (p *Page) usable() bool {
	return !p.Truncated && p.Skipped*maxSkippedOneIn <= p.Skipped+len(p.Footprints)
}

var errShape = errors.New("buildings: unexpected response shape")

// coordWindow is the plausible EPSG:25832 window of a footprint: the DHM
// extent grown by 5 km, since a building that reaches into a corridor at
// the extent's edge may extend past it. A position outside means the
// service answered in another CRS or axis order; dhm.PolygonReader refuses
// it with dhm.ErrOutsideWindow, and so does Parse, for the whole page.
var coordWindow = func() [4]float64 {
	x := dhm.RasterExtent
	return [4]float64{x[0] - 5000, x[1] - 5000, x[2] + 5000, x[3] + 5000}
}()

// featureCollection is the part of the response Parse reads. The
// properties of a feature are not read at all: the classification needs
// the footprint only, and the INSPIRE attributes that would matter
// (heightaboveground, numberoffloorsaboveground) are null throughout
// (#730 research §2.1).
type featureCollection struct {
	Type           string          `json:"type"`
	Features       *[]feature      `json:"features"`
	NumberMatched  json.RawMessage `json:"numberMatched"`
	NumberReturned json.RawMessage `json:"numberReturned"`
	Links          []struct {
		Rel string `json:"rel"`
	} `json:"links"`
}

type feature struct {
	Type     string `json:"type"`
	Geometry *struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	} `json:"geometry"`
}

// Parse reads the OGC API Features GeoJSON of one segment's request.
// Anything off-shape is an error: another document (an OGC API exception
// is JSON too), trailing data, a missing features array, a numberMatched
// or numberReturned that is not a count or disagrees with the page, a
// member of features that is not a Feature, a position outside
// coordWindow, or more than MaxVertices vertices. More than MaxFeatures
// features is a Truncated page without footprints. A feature without a
// usable footprint is skipped and counted instead, and costs no vertex
// budget.
func Parse(body []byte) (Page, error) { return parse(body, MaxVertices) }

func parse(body []byte, maxVertices int) (Page, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var doc featureCollection
	if err := dec.Decode(&doc); err != nil {
		return Page{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Page{}, errShape
	}
	if doc.Type != "FeatureCollection" || doc.Features == nil {
		return Page{}, errShape
	}
	features := *doc.Features
	matched, hasMatched, err := count(doc.NumberMatched)
	if err != nil || hasMatched && matched < len(features) {
		return Page{}, errShape
	}
	if returned, ok, err := count(doc.NumberReturned); err != nil || ok && returned != len(features) {
		return Page{}, errShape
	}
	if len(features) > MaxFeatures {
		// More buildings than the limit asked for: the corridor holds more
		// than one page, so the segment stays unclassified, as for any
		// truncated page (#776 review N4), and its footprints are not read.
		return Page{Truncated: true}, nil
	}
	page := Page{Footprints: make([]dhm.MultiPolygon, 0, len(features))}
	page.Truncated = matched > len(features) || !hasMatched && len(features) >= MaxFeatures
	for _, l := range doc.Links {
		page.Truncated = page.Truncated || l.Rel == "next"
	}
	geom := dhm.PolygonReader{Window: coordWindow, MaxVertices: maxVertices}
	for i := range features {
		f := &features[i]
		if f.Type != "Feature" {
			return Page{}, errShape
		}
		if f.Geometry == nil {
			page.Skipped++
			continue
		}
		fp, err := geom.Read(f.Geometry.Type, f.Geometry.Coordinates)
		switch {
		case errors.Is(err, dhm.ErrVertexBudget), errors.Is(err, dhm.ErrOutsideWindow):
			// The whole page is off: skipping on would drop every
			// footprint after this one, or all of them.
			return Page{}, err
		case err != nil || len(fp) == 0:
			// One broken footprint (Read refunded its vertices), or an
			// empty MultiPolygon, which is valid GeoJSON but no footprint.
			page.Skipped++
		default:
			page.Footprints = append(page.Footprints, fp)
		}
	}
	return page, nil
}

// count reads an OGC API count member: absent or null is not given,
// anything but an integer is an error. A negative count always disagrees
// with the page, which the caller refuses.
func count(raw json.RawMessage) (n int, ok bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false, errShape
	}
	return n, true, nil
}
