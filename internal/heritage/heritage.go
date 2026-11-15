// Package heritage is CoreScope's server-side access to the protection
// zones of the Slots- og Kulturstyrelsen register of protected ancient
// monuments (Fund og Fortidsminder, #723).
//
// The register's GeoServer serves the zones as the WFS feature type
// public:fundogfortidsminder_areal_beskyttelse: polygons (EPSG:25832)
// carrying anlaegstype, datering, fred_status, frednr and systemnr among
// other fields. The service needs no token; it is free under the agency's
// terms of use, which also say that harvesting the content is not
// permitted. The package therefore only ever asks for one fixed
// 250 m × 250 m EPSG:25832 cell at a time (grown by the configured
// radius), on behalf of someone who clicked the map. It has no way to
// request an arbitrary bbox, page through results, filter or walk the
// country, and nothing should be added that does.
//
// The package holds:
//
//   - Cell: the grid-snapped cell math and the Danish extent (cell.go).
//   - Client: the WFS client. Every request has the same fixed shape;
//     only the cell's bbox varies. It uses the shared keyless GET of
//     internal/dhm: no redirects, a timeout, a bounded body, and failures
//     reduced to a dhm.FetchError category (client.go).
//   - Parse: a strict reader of the GeoJSON FeatureCollection into Zones,
//     keeping only the dataset fields the server reports (parse.go). The
//     geometry itself is read by dhm.PolygonReader.
//   - Nearest: which zone a point belongs to — the one that holds it, or
//     else the nearest one within a radius — over dhm.Locate's
//     point-in-polygon and edge distance in EPSG:25832 metres (nearest.go).
//
// Coordinates are projected with the internal/dhm UTM 32N code, and the
// polygon reading and maths are internal/dhm's too (polygon.go, #730 M1a),
// so there is one implementation of EPSG:25832 in the repository and one of
// the geometry it carries. The route, cache,
// coalescing and rate limits live in cmd/server (heritage_query.go).
package heritage

// DefaultBaseURL is the WFS of the agency's GeoServer. Only the
// /ffgeoserver/public/ prefix answers over HTTPS; neighbouring paths on the
// host redirect to plain http:// (#723 research), so this exact prefix is
// used and no redirect is followed.
const DefaultBaseURL = "https://www.kulturarv.dk/ffgeoserver/public/wfs"

// TypeName is the only feature type the client asks for: the protection
// zones. The monument points (fundogfortidsminder_punkt_fredet) are drawn
// by the M1 overlay but not queried.
const TypeName = "public:fundogfortidsminder_areal_beskyttelse"

// CRS is the only coordinate reference system the WFS is asked in, both
// for the bbox and for the returned geometry. EPSG:25832's axis order is
// easting, northing.
const CRS = "urn:ogc:def:crs:EPSG::25832"

// MaxFeatures is the WFS count: the most zones one cell request returns.
// The capabilities declare no CountDefault, so without it an unbounded
// bbox would return all ~32,000 zones. A full page is marked Truncated.
const MaxFeatures = 100

// MaxVertices bounds the vertices of one page, so a hostile or broken
// response cannot make the point-in-polygon work unbounded.
const MaxVertices = 50000

// MaxTextRunes bounds every text field taken from the dataset.
const MaxTextRunes = 120
