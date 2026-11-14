package dhm

import (
	"bytes"
	"context"
	"image/jpeg"
	"net/url"
	"strconv"
)

// #728 M2 — the Danish spring orthophoto as a base-map tile.
//
// Upstream, from the public GetCapabilities of orto_foraar_webm_DAF (WMTS
// 1.0.0), as recorded in the #728 research: layer orto_foraar_webm, style
// default, tile matrix set DFD_GoogleMapsCompatible (EPSG:3857, 256 px,
// Leaflet's own grid), image/jpeg, tile matrices 0–20. A GetTile without a
// token is refused, so it goes through this Client like the hillshade.
//
// The capabilities' ResourceURL template points at another platform with a
// credential that is not ours; the query is never taken from it. It is
// rebuilt here, KVP against DefaultBaseURL, from a validated TileKey.
//
// The temporary quickorto service (wms/orto_foraar_temp) is deliberately
// not used: unlike #687 it would add nothing.

const (
	OrthoWMTSService   = "orto_foraar_webm_DAF"
	OrthoLayer         = "orto_foraar_webm"
	OrthoTileMatrixSet = "DFD_GoogleMapsCompatible"
	// OrthoMinZoom matches the hillshade's floor; OrthoMaxZoom is the
	// service's last tile matrix.
	OrthoMinZoom = 7
	OrthoMaxZoom = 20
)

// The orthophoto credit (#728), in the style of the DHM credit (#733): the
// tiles are shown as served, so there is no DerivedNote.
//
// UNVERIFIED: the wording is the #728 plan's proposal, after kortoverblik.dk's
// "(CC BY) GeoDanmark". It has not been checked against the product page or
// the terms; that is required before #728 M2 merges.
const (
	// OrthoOwner is the data owner the credit names.
	OrthoOwner = "GeoDanmark"
	// OrthoDatasetName is the dataset the credit names.
	OrthoDatasetName = "Ortofoto forår"
	// OrthoDatasetURL is Dataforsyningen's data overview, which the terms
	// accept in place of the dataset page (see DatasetURL).
	OrthoDatasetURL = "https://dataforsyningen.dk/data"
	// OrthoAttribution is the plain-text credit line of the orthophoto
	// base layer. Its licence is LicenseName / LicenseURL.
	OrthoAttribution = "Indeholder data fra " + OrthoOwner + ", " + OrthoDatasetName + ", " + LicenseName
)

// orthoParams are the fixed keys of the upstream GetTile query; the tile
// keys tilematrix, tilerow and tilecol are added from a TileKey.
var orthoParams = map[string]string{
	"service":       "WMTS",
	"request":       "GetTile",
	"version":       "1.0.0",
	"layer":         OrthoLayer,
	"style":         "default",
	"tilematrixset": OrthoTileMatrixSet,
	"format":        "image/jpeg",
}

// maxTileCoordLen bounds a z/x/y path segment: 2^20 has 7 digits.
const maxTileCoordLen = 7

// ParseOrthoTile validates the z, x and y of a browser ortho tile request
// (#728: the path /api/map/dhm/ortho/{z}/{x}/{y}). Each must be a plain
// decimal number with no sign, space or leading zero, z within
// [OrthoMinZoom, OrthoMaxZoom], and x and y within the zoom's grid. Any
// deviation is ErrBadRequest.
func ParseOrthoTile(z, x, y string) (TileKey, error) {
	var v [3]int
	for i, s := range [3]string{z, x, y} {
		n, ok := tileCoord(s)
		if !ok {
			return TileKey{}, ErrBadRequest
		}
		v[i] = n
	}
	k := TileKey{Z: v[0], X: v[1], Y: v[2]}
	if k.Z < OrthoMinZoom || k.Z > OrthoMaxZoom {
		return TileKey{}, ErrBadRequest
	}
	if n := 1 << k.Z; k.X >= n || k.Y >= n {
		return TileKey{}, ErrBadRequest
	}
	return k, nil
}

// tileCoord parses a canonical non-negative decimal: strconv.Itoa(n) == s.
func tileCoord(s string) (int, bool) {
	if len(s) == 0 || len(s) > maxTileCoordLen {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

// orthoQuery is the tile's upstream GetTile query, rebuilt from the
// validated key.
func (k TileKey) orthoQuery() url.Values {
	q := url.Values{}
	for key, v := range orthoParams {
		q.Set(key, v)
	}
	q.Set("tilematrix", strconv.Itoa(k.Z))
	q.Set("tilerow", strconv.Itoa(k.Y))
	q.Set("tilecol", strconv.Itoa(k.X))
	return q
}

// orthoQueryCheck accepts q only if it is exactly the orthoQuery of a tile
// ParseOrthoTile accepts, so Client.Get cannot send any other request to
// the ortho service: no other layer, format, tile matrix set or parameter.
func orthoQueryCheck(q url.Values) error {
	free, err := ExactQuery(q, orthoParams, "tilematrix", "tilerow", "tilecol")
	if err != nil {
		return err
	}
	_, err = ParseOrthoTile(free["tilematrix"], free["tilecol"], free["tilerow"])
	return err
}

// FetchOrthoAdmitted fetches one orthophoto tile like
// FetchHillshadeAdmitted fetches a hillshade tile (#744): a new upstream
// call takes an adm slot first, or fails at once with ErrAdmissionFull,
// and the caller stops waiting when ctx is done. Only a complete 256×256
// JPEG is returned; anything else is a FetchError, so the proxy, which
// caches only successes, never caches a bad tile.
func (c *Client) FetchOrthoAdmitted(ctx context.Context, adm *Admission, k TileKey) ([]byte, error) {
	data, err := c.get(ctx, adm, OrthoWMTSService, k.orthoQuery(), []string{"image/jpeg"})
	if err != nil {
		return nil, err
	}
	if err := checkTileJPEG(data); err != nil {
		return nil, err
	}
	return data, nil
}

var jpegSOI = []byte{0xff, 0xd8, 0xff}

// checkTileJPEG is checkTilePNG for JPEG (#695): a body without the JPEG
// start-of-image marker is "non-image response"; one with it that does
// not decode completely as a TileSize × TileSize JPEG is "invalid image".
// DecodeConfig runs first so a body that declares a huge image is
// rejected before the decoder allocates its pixels. BenchmarkCheckTileJPEG
// measures about 1.0 ms and 126 KB of short-lived allocations per ~23 KB
// tile (i7-1260P; checkTilePNG is 1.6 ms there), paid only on an upstream
// fetch that already took a network round trip; cache hits are not
// re-checked.
func checkTileJPEG(data []byte) error {
	if !bytes.HasPrefix(data, jpegSOI) {
		return FetchError{"non-image response"}
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != TileSize || cfg.Height != TileSize {
		return FetchError{"invalid image"}
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		return FetchError{"invalid image"}
	}
	return nil
}
