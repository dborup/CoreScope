package dhm

import (
	"net/url"
	"strconv"
	"strings"
)

// #681 — the elevation raster request shape (WCS GetCoverage).
//
// Upstream, verified against the public GetCapabilities of dhm_wcs_DAF (WCS
// 1.0.0): exactly two coverages, dhm_terraen (DTM) and dhm_overflade (DSM).
// The DAF WCS guide states UTM zone 32N (EPSG:25832) and GeoTIFF only.

const (
	WCSService      = "dhm_wcs_DAF"
	TerrainCoverage = "dhm_terraen"
	SurfaceCoverage = "dhm_overflade"
	RasterCRS       = "EPSG:25832"

	// MaxRasterSize is the largest raster edge in pixels. The plan's
	// default 2.5 km chunk needs at most 179 px at a 10 m step, and the
	// single-call mode (chunkKm = 0) about 362 px; 512 covers both.
	MaxRasterSize = 512
	// MaxRasterSpanM is the largest bbox edge in metres: the plan's 80 km
	// maximum path length, so an axis-aligned path fits one request.
	MaxRasterSpanM = 80000

	// maxRasterBodyBytes is one MaxRasterSize² raster of uncompressed
	// float32 (DEFLATE does not reliably shrink float elevations, and
	// upstream may not compress at all), plus 64 KiB for the TIFF header,
	// IFD, GeoTIFF keys and strip tables (512 strips × 2 LONGs × 4 bytes
	// = 4 KiB). That is 1 114 112 bytes, above the 1 MiB tile cap.
	maxRasterBodyBytes = MaxRasterSize*MaxRasterSize*4 + 64<<10
)

// RasterExtent is the DHM layer bbox in EPSG:25832 metres from the public
// dhm_DAF capabilities: minE, minN, maxE, maxN. A raster request must lie
// inside it; the caller clips a path that leaves Denmark.
var RasterExtent = [4]float64{440000, 6048000, 894000, 6404000}

// Model is the elevation model of a raster: bare ground or surface.
type Model string

const (
	ModelTerrain Model = "dtm" // terrain: vegetation and buildings removed
	ModelSurface Model = "dsm" // surface: includes trees and buildings
)

var modelCoverage = map[Model]string{
	ModelTerrain: TerrainCoverage,
	ModelSurface: SurfaceCoverage,
}

// RasterRequest is one elevation raster: a bbox in EPSG:25832 metres
// (minE, minN, maxE, maxN) sampled at Width × Height pixels.
type RasterRequest struct {
	Model         Model
	BBox          [4]float64
	Width, Height int
}

// ElevationSource fetches elevation rasters, so a replacement for the DHM
// WCS is a drop-in (#681 owner decision 8). FetchRaster returns a decoded,
// georeferenced Grid in EPSG:25832 that covers r.BBox at r's resolution,
// up to one pixel of snapping; the wire format is the source's business.
type ElevationSource interface {
	FetchRaster(r RasterRequest) (Grid, error)
}

// Validate accepts r only if every field is inside the allow-list.
func (r RasterRequest) Validate() error {
	if _, ok := modelCoverage[r.Model]; !ok {
		return ErrBadRequest
	}
	if r.Width < 1 || r.Width > MaxRasterSize || r.Height < 1 || r.Height > MaxRasterSize {
		return ErrBadRequest
	}
	// Written so NaN and ±Inf fail too: every comparison with NaN is
	// false, and an infinite edge exceeds MaxRasterSpanM.
	b := r.BBox
	if !(b[2] > b[0]) || !(b[3] > b[1]) || b[2]-b[0] > MaxRasterSpanM || b[3]-b[1] > MaxRasterSpanM {
		return ErrBadRequest
	}
	e := RasterExtent
	if b[0] < e[0] || b[1] < e[1] || b[2] > e[2] || b[3] > e[3] {
		return ErrBadRequest
	}
	return nil
}

// coverageParams are the fixed GetCoverage keys and their only values.
var coverageParams = map[string]string{
	"service":      "WCS",
	"version":      "1.0.0",
	"request":      "GetCoverage",
	"crs":          RasterCRS,
	"response_crs": RasterCRS,
	"format":       "GTiff",
}

// coverageQuery is the GetCoverage query for a validated request.
func (r RasterRequest) coverageQuery() url.Values {
	q := url.Values{}
	for k, v := range coverageParams {
		q.Set(k, v)
	}
	q.Set("coverage", modelCoverage[r.Model])
	q.Set("bbox", strings.Join([]string{
		strconv.FormatFloat(r.BBox[0], 'f', -1, 64), strconv.FormatFloat(r.BBox[1], 'f', -1, 64),
		strconv.FormatFloat(r.BBox[2], 'f', -1, 64), strconv.FormatFloat(r.BBox[3], 'f', -1, 64),
	}, ","))
	q.Set("width", strconv.Itoa(r.Width))
	q.Set("height", strconv.Itoa(r.Height))
	return q
}

// parseCoverageQuery accepts only the exact GetCoverage shape that
// coverageQuery builds. Client.Get runs it on every WCS call.
func parseCoverageQuery(q url.Values) (RasterRequest, error) {
	free, err := ExactQuery(q, coverageParams, "coverage", "bbox", "width", "height")
	if err != nil {
		return RasterRequest{}, err
	}
	var r RasterRequest
	for m, cov := range modelCoverage {
		if free["coverage"] == cov {
			r.Model = m
		}
	}
	parts := strings.Split(free["bbox"], ",")
	if len(parts) != 4 {
		return RasterRequest{}, ErrBadRequest
	}
	for i, s := range parts {
		if len(s) > maxBBoxComponent {
			return RasterRequest{}, ErrBadRequest
		}
		if r.BBox[i], err = strconv.ParseFloat(s, 64); err != nil {
			return RasterRequest{}, ErrBadRequest
		}
	}
	if r.Width, err = parseSize(free["width"]); err != nil {
		return RasterRequest{}, err
	}
	if r.Height, err = parseSize(free["height"]); err != nil {
		return RasterRequest{}, err
	}
	return r, r.Validate()
}

// parseSize accepts a plain decimal pixel count, nothing else.
func parseSize(s string) (int, error) {
	if len(s) == 0 || len(s) > 4 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, ErrBadRequest
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, ErrBadRequest
	}
	return n, nil
}

// checkCoverageQuery accepts q only in the exact spelling coverageQuery
// builds for the request it parses to. A numerically valid but
// non-canonical bbox or size (0x1p19, 5.75e5, 575000., 0179) would
// otherwise reach upstream in a shape this package never sends.
func checkCoverageQuery(q url.Values) error {
	r, err := parseCoverageQuery(q)
	if err != nil {
		return err
	}
	if q.Encode() != r.coverageQuery().Encode() {
		return ErrBadRequest
	}
	return nil
}

// geoTIFFContentTypes are the media types accepted for a raster.
var geoTIFFContentTypes = []string{"image/tiff", "image/geotiff"}

// WCSSource is the DHM WCS ElevationSource.
type WCSSource struct{ c *Client }

// NewWCSSource returns the DHM WCS source on c.
func NewWCSSource(c *Client) *WCSSource { return &WCSSource{c} }

// FetchRaster fetches and decodes one raster. A request outside the
// allow-list is ErrBadRequest before any upstream call (Client.Get checks
// the query); every upstream or decoding failure is a FetchError.
func (s *WCSSource) FetchRaster(r RasterRequest) (Grid, error) {
	data, err := s.c.get(WCSService, r.coverageQuery(), geoTIFFContentTypes)
	if err != nil {
		return Grid{}, err
	}
	g, err := decodeGeoTIFF(data)
	if err != nil {
		return Grid{}, FetchError{"invalid raster"}
	}
	if g.Georef == (Georef{}) {
		// No georeferencing in the file: the grid is the request.
		if g.Width != r.Width || g.Height != r.Height {
			return Grid{}, FetchError{"raster mismatch"}
		}
		g.Georef = Georef{
			OriginE: r.BBox[0],
			OriginN: r.BBox[3],
			PixelE:  (r.BBox[2] - r.BBox[0]) / float64(r.Width),
			PixelN:  (r.BBox[3] - r.BBox[1]) / float64(r.Height),
		}
	} else if !g.fits(r) {
		return Grid{}, FetchError{"raster mismatch"}
	}
	return g, nil
}
