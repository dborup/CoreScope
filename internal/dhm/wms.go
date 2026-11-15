package dhm

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// #680 — the hillshade tile request shape.
//
// Upstream, verified against the public GetCapabilities of dhm_DAF (WMS
// 1.3.0): layer dhm_terraen_skyggekort, image/png, EPSG:3857 inherited from
// the root layer, MaxWidth/MaxHeight 10000.

const (
	WMSService     = "dhm_DAF"
	HillshadeLayer = "dhm_terraen_skyggekort"
	TileSize       = 256
	MinZoom        = 7
	MaxZoom        = 19

	webMercatorHalf  = math.Pi * 6378137 // 20037508.342789244
	bboxTolerance    = 1e-6              // fraction of a tile edge
	maxBBoxComponent = 64                // longest accepted bbox number
)

// tileParams is the exact browser request shape (Leaflet 1.9.4
// L.tileLayer.wms with the overlay options in map-tile-providers.js),
// apart from layers, which names the overlay (#734), and bbox.
var tileParams = map[string]string{
	"service":     "WMS",
	"request":     "GetMap",
	"version":     "1.3.0",
	"styles":      "",
	"format":      "image/png",
	"transparent": "true",
	"width":       strconv.Itoa(TileSize),
	"height":      strconv.Itoa(TileSize),
	"crs":         "EPSG:3857",
}

// TileParams returns a copy of the browser query parameters (every key
// except bbox) as the Map page sends them, e.g. for API documentation.
// Every key but layers must have exactly its value; layers is the #680
// value, the terrain id's legacy alias (#734 Q7), and may be any
// OverlayIDs id instead.
func TileParams() map[string]string {
	out := make(map[string]string, len(tileParams)+1)
	for k, v := range tileParams {
		out[k] = v
	}
	out["layers"] = legacyTerrainLayers
	return out
}

// TileKey is one Web Mercator tile.
type TileKey struct{ Z, X, Y int }

func (k TileKey) String() string { return fmt.Sprintf("%d/%d/%d", k.Z, k.X, k.Y) }

// ParseTileRequest validates a browser tile query and returns the overlay
// and the tile it names (#734): layers is an overlay id or the legacy
// terrain alias, and the tile's zoom is inside that overlay's range. Any
// deviation is ErrBadRequest.
func ParseTileRequest(q url.Values) (Overlay, TileKey, error) {
	free, err := ExactQuery(q, tileParams, "layers", "bbox")
	if err != nil {
		return Overlay{}, TileKey{}, err
	}
	o, ok := parseOverlayID(free["layers"])
	if !ok {
		return Overlay{}, TileKey{}, ErrBadRequest
	}
	k, err := TileFromBBox(free["bbox"])
	if err != nil {
		return Overlay{}, TileKey{}, err
	}
	if k.Z < o.MinZoom || k.Z > o.MaxZoom {
		return Overlay{}, TileKey{}, ErrBadRequest
	}
	return o, k, nil
}

// TileFromBBox accepts a bbox only if it is exactly one Web Mercator tile
// (within float noise) at an allowed zoom.
func TileFromBBox(bbox string) (TileKey, error) {
	parts := strings.Split(bbox, ",")
	if len(parts) != 4 {
		return TileKey{}, ErrBadRequest
	}
	var v [4]float64
	for i, s := range parts {
		if len(s) == 0 || len(s) > maxBBoxComponent {
			return TileKey{}, ErrBadRequest
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return TileKey{}, ErrBadRequest
		}
		v[i] = f
	}
	minx, miny, maxx, maxy := v[0], v[1], v[2], v[3]
	span := maxx - minx
	if !(span > 0) || !(maxy-miny > 0) {
		return TileKey{}, ErrBadRequest
	}
	z := int(math.Round(math.Log2(2 * webMercatorHalf / span)))
	if z < MinZoom || z > MaxZoom {
		return TileKey{}, ErrBadRequest
	}
	size := 2 * webMercatorHalf / math.Exp2(float64(z))
	x := int(math.Round((minx + webMercatorHalf) / size))
	y := int(math.Round((webMercatorHalf - maxy) / size))
	n := 1 << z
	if x < 0 || y < 0 || x >= n || y >= n {
		return TileKey{}, ErrBadRequest
	}
	key := TileKey{z, x, y}
	want := key.BBox()
	tol := size * bboxTolerance
	for i := range v {
		if math.Abs(v[i]-want[i]) > tol {
			return TileKey{}, ErrBadRequest
		}
	}
	return key, nil
}

// BBox is the tile's exact EPSG:3857 extent: minx, miny, maxx, maxy.
func (k TileKey) BBox() [4]float64 {
	size := 2 * webMercatorHalf / math.Exp2(float64(k.Z))
	return [4]float64{
		float64(k.X)*size - webMercatorHalf,
		webMercatorHalf - float64(k.Y+1)*size,
		float64(k.X+1)*size - webMercatorHalf,
		webMercatorHalf - float64(k.Y)*size,
	}
}

func mercatorToLonLat(x, y float64) (lon, lat float64) {
	lon = x / webMercatorHalf * 180
	lat = (2*math.Atan(math.Exp(y/6378137)) - math.Pi/2) * 180 / math.Pi
	return
}

// IntersectsCoverage reports whether the tile overlaps DHM Coverage.
func (k TileKey) IntersectsCoverage() bool {
	b := k.BBox()
	west, south := mercatorToLonLat(b[0], b[1])
	east, north := mercatorToLonLat(b[2], b[3])
	return east > Coverage.West && west < Coverage.East &&
		north > Coverage.South && south < Coverage.North
}

// upstreamQuery is the terrain hillshade's GetMap query for the tile,
// rebuilt from the validated key: nothing is copied from the browser's
// query string. OverlayQuery sets another overlay's layer and style.
func (k TileKey) upstreamQuery() url.Values {
	b := k.BBox()
	q := url.Values{}
	for key, v := range tileParams {
		q.Set(key, v)
	}
	q.Set("layers", HillshadeLayer)
	// WMS 1.3.0 spells the boolean TRUE; Leaflet sends `true`.
	q.Set("transparent", "TRUE")
	q.Set("bbox", strings.Join([]string{
		strconv.FormatFloat(b[0], 'f', -1, 64), strconv.FormatFloat(b[1], 'f', -1, 64),
		strconv.FormatFloat(b[2], 'f', -1, 64), strconv.FormatFloat(b[3], 'f', -1, 64),
	}, ","))
	return q
}

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// FetchHillshadeTile fetches one #680 terrain hillshade tile. Only a real
// PNG is returned; anything else is a FetchError.
func (c *Client) FetchHillshadeTile(k TileKey) ([]byte, error) {
	return c.FetchHillshade(HillshadeTerrain, k)
}
