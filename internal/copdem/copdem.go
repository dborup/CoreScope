// Package copdem is CoreScope's foreign fill for the terrain profile
// (#738): elevations outside Danmarks Højdemodel's coverage, from
// Copernicus DEM GLO-30, read as public Cloud-Optimized GeoTIFFs from the
// AWS Open Data bucket copernicus-dem-30m. No token and no account.
//
// DHM covers Denmark only (internal/dhm Covers), so a profile across
// Øresund or into Germany used to stop at "partial, no verdict". The fill
// gives exactly the samples DHM does not cover (the existing foreign-land
// gate, and everything outside DHM's raster extent) a GLO-30 terrain
// height, and nothing else. GLO-30 is a surface model: forests and towns
// read high, a bias towards a false "blocked", never towards a false
// "clear" (#738 decision 1).
//
// The package holds:
//
//   - the tile list (tiles.go): which 1°×1° tiles exist in the region a
//     profile can reach. A tile not in the bucket is sea, 0 m, per the
//     bucket's readme; a 404 for a listed tile is an error, never 0 m.
//   - the Client (client.go): range reads of one fixed host and of tile
//     paths that pass a strict allow-list, never a redirect, never a DHM
//     token, and only a 206 whose Content-Range is the range asked for,
//     with a capped body. Its own small concurrency limit, coalescing
//     (dhm.Flight) and admission bound (dhm.Admission), bounded header and
//     block caches (dhm.Cache).
//   - the fill (fill.go): which blocks a profile's uncovered samples need,
//     within the per-model budget of dhm.MaxCallsPerModel upstream calls,
//     falling back to the ½ overview (#738 decision 7), and the bilinear
//     sample of each block (dhm.TiledGeoTIFF, dhm.Grid).
//
// The tiled GeoTIFF reader is internal/dhm's geotiff_tiled.go, which shares
// its decoder with the DHM rasters. This package is stdlib-only beyond
// internal/dhm.
package copdem

const (
	// Source names the fill in a profile response.
	Source = "copdem-glo30"

	// DatasetName is the dataset, for a short credit line.
	DatasetName = "Copernicus DEM GLO-30"

	// AttributionDerived is Art. 6(b) of the Copernicus DEM licence,
	// verbatim: the notice for adapted or modified data, which a sampled
	// and interpolated profile is. A profile with a filled sample must
	// carry it (#738 research §4, decision 2).
	AttributionDerived = "produced using Copernicus WorldDEM-30 © DLR e.V. 2010-2014 and © Airbus Defence and Space GmbH 2014-2018 provided under COPERNICUS by the European Union and ESA; all rights reserved"

	// LiabilityNotice is Art. 6(c), verbatim, which redistributors must
	// add to their licence or legal notice: it lives in the README and in
	// config.example.json (#738 decision 2).
	LiabilityNotice = "The organisations in charge of the Copernicus programme by law or by delegation do not incur any liability for any use of the Copernicus WorldDEM-30"

	// LicenseURL is the Copernicus DEM licence.
	LicenseURL = "https://documentation.dataspace.copernicus.eu/APIs/SentinelHub/Data/DEM/resources/license/License-COPDEM-30.pdf"
)
