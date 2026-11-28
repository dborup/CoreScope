// Package mastedb is CoreScope's server-side access to Mastedatabasen, the
// Danish register of antenna positions run by Digitaliseringsstyrelsen
// (#724).
//
// Licence and terms: the data is CC0 1.0, but the catalogue record also says
// "Data Scraping af mastedatabasen er forbudt". The package therefore only
// ever asks for one fixed 2 km × 2 km EPSG:25832 cell at a time (cell.go),
// on behalf of someone viewing the map. It has no way to request an
// arbitrary bbox, page through results or walk the country, and nothing in
// it should be added that does.
//
// The package holds:
//
//   - Cell: the grid-snapped cell math and the Danish extent (cell.go).
//   - Client: the WFS client. Every request has the same fixed shape (type
//     name, property list, CRS); only the cell's bbox varies. It never
//     follows redirects, bounds the body, has a timeout and honours ctx
//     (client.go).
//   - Parse: a strict GML 3.2 parser into typed rows (gml.go).
//   - Sites: rows grouped into sites by position, with the derived
//     interference and likelyMast flags (site.go).
//
// Coordinates are projected with the internal/dhm UTM 32N code, so there is
// one implementation of EPSG:25832 in the repository. Upstream failures are
// dhm.FetchError, a fixed category with no URL and no upstream text, so the
// server can log and report both upstreams the same way.
//
// The route, cache, coalescing and rate limits are milestone M2; the map
// layer is M3.
package mastedb

// Attribution is the courtesy credit line for Mastedatabasen data. CC0 needs
// none, but the plan in #724 credits the source anyway.
const Attribution = "Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)"

// SourceURL is the public Mastedatabasen site, for the popup link.
const SourceURL = "https://mastedatabasen.dk/"
