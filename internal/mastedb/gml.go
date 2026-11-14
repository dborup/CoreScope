package mastedb

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

// PageLimit is the documented WFS limit ("only 500 records at a time"). A
// response that reaches it may be missing rows.
const PageLimit = 500

// ErrBadResponse is the single error for a response that is not a
// well-formed MasteIBrug feature collection. It holds no upstream text.
var ErrBadResponse = errors.New("mastedb: unexpected response")

// Row is one WFS feature: one technology and band entry at a position.
// Text fields are as published, trimmed; nothing is inferred.
type Row struct {
	ID           int64   // MasteID
	Station      string  // Unik_Station_navn
	E, N         float64 // UTM_E, UTM_N (EPSG:25832 metres)
	Street       string  // Vejnavn
	HouseNumber  string  // Husnummer
	Postcode     string  // Postnr
	City         string  // By
	Service      string  // TjenesteArt_navn
	Technology   string  // Teknologi_navn
	BandMHz      int     // Frekvensbaand; 0 when missing or not a whole number
	Commissioned string  // Idriftsaettelsesdato, as published
	ObjectType   string  // Objekt_type; empty in every row sampled for #724
}

// Page is one parsed GetFeature response for a cell.
type Page struct {
	Rows []Row
	// Returned is numberReturned: the rows upstream sent, before rows
	// outside the cell were dropped.
	Returned int
	// Truncated is set when upstream may have left rows out: it returned
	// PageLimit rows or more, or said more matched than it returned.
	Truncated bool
}

// gmlCollection is the subset of wfs:FeatureCollection the parser reads.
// encoding/xml matches the namespaced names exactly, so a feature of
// another type or namespace is not read as a MasteIBrug. The feature
// namespace is the bare string the service declares (no scheme).
type gmlCollection struct {
	XMLName        xml.Name    `xml:"http://www.opengis.net/wfs/2.0 FeatureCollection"`
	NumberMatched  string      `xml:"numberMatched,attr"`
	NumberReturned string      `xml:"numberReturned,attr"`
	Members        []gmlMember `xml:"http://www.opengis.net/wfs/2.0 member"`
}

type gmlMember struct {
	Feature *gmlFeature `xml:"www.mastedatabasen.dk MasteIBrug"`
}

type gmlFeature struct {
	MasteID              string `xml:"www.mastedatabasen.dk MasteID"`
	UnikStationNavn      string `xml:"www.mastedatabasen.dk Unik_Station_navn"`
	UTME                 string `xml:"www.mastedatabasen.dk UTM_E"`
	UTMN                 string `xml:"www.mastedatabasen.dk UTM_N"`
	Vejnavn              string `xml:"www.mastedatabasen.dk Vejnavn"`
	Husnummer            string `xml:"www.mastedatabasen.dk Husnummer"`
	Postnr               string `xml:"www.mastedatabasen.dk Postnr"`
	By                   string `xml:"www.mastedatabasen.dk By"`
	TjenesteArtNavn      string `xml:"www.mastedatabasen.dk TjenesteArt_navn"`
	TeknologiNavn        string `xml:"www.mastedatabasen.dk Teknologi_navn"`
	Frekvensbaand        string `xml:"www.mastedatabasen.dk Frekvensbaand"`
	Idriftsaettelsesdato string `xml:"www.mastedatabasen.dk Idriftsaettelsesdato"`
	ObjektType           string `xml:"www.mastedatabasen.dk Objekt_type"`
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Parse reads a GetFeature response for cell. It fails closed: anything
// other than a wfs:FeatureCollection whose numberReturned matches its
// MasteIBrug members, each with an id and finite coordinates, is
// ErrBadResponse. Rows outside cell (the WFS BBOX is inclusive, so edge
// points come back for both neighbours) are dropped.
func Parse(body []byte, cell Cell) (Page, error) {
	var c gmlCollection
	dec := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(body, utf8BOM)))
	if err := dec.Decode(&c); err != nil || !onlyTrailingMisc(dec) {
		return Page{}, ErrBadResponse
	}
	returned, err := strconv.Atoi(c.NumberReturned)
	if err != nil || returned != len(c.Members) {
		return Page{}, ErrBadResponse
	}
	page := Page{Returned: returned, Truncated: returned >= PageLimit}
	// numberMatched may be "unknown" in WFS 2.0; only a number counts.
	if matched, err := strconv.Atoi(c.NumberMatched); err == nil && matched > returned {
		page.Truncated = true
	}
	page.Rows = make([]Row, 0, len(c.Members))
	for _, m := range c.Members {
		if m.Feature == nil {
			return Page{}, ErrBadResponse
		}
		r, ok := m.Feature.row()
		if !ok {
			return Page{}, ErrBadResponse
		}
		if cell.Contains(r.E, r.N) {
			page.Rows = append(page.Rows, r)
		}
	}
	return page, nil
}

// onlyTrailingMisc consumes dec to EOF and reports whether nothing but
// whitespace, comments and processing instructions follows the root.
// Decode stops at the root's end tag, so without this a second root, text
// or an unfinished tag after it would be accepted (#748).
func onlyTrailingMisc(dec *xml.Decoder) bool {
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case xml.Comment, xml.ProcInst:
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return false
			}
		default: // StartElement, EndElement, Directive
			return false
		}
	}
}

// row converts f, reporting false if a required field is unusable.
func (f *gmlFeature) row() (Row, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(f.MasteID), 10, 64)
	if err != nil {
		return Row{}, false
	}
	e, okE := parseCoord(f.UTME)
	n, okN := parseCoord(f.UTMN)
	if !okE || !okN {
		return Row{}, false
	}
	return Row{
		ID:           id,
		Station:      strings.TrimSpace(f.UnikStationNavn),
		E:            e,
		N:            n,
		Street:       strings.TrimSpace(f.Vejnavn),
		HouseNumber:  strings.TrimSpace(f.Husnummer),
		Postcode:     strings.TrimSpace(f.Postnr),
		City:         strings.TrimSpace(f.By),
		Service:      strings.TrimSpace(f.TjenesteArtNavn),
		Technology:   strings.TrimSpace(f.TeknologiNavn),
		BandMHz:      parseBand(f.Frekvensbaand),
		Commissioned: strings.TrimSpace(f.Idriftsaettelsesdato),
		ObjectType:   strings.TrimSpace(f.ObjektType),
	}, true
}

func parseCoord(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// parseBand is the band in MHz, or 0 (unknown) when it is missing, not a
// whole positive number, or absurd. An unknown band never counts as
// interference-relevant.
func parseBand(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v <= 0 || v > 1000000 {
		return 0
	}
	return v
}
