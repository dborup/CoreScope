// Package dhm is CoreScope's server-side access to Danmarks Højdemodel (DHM),
// served by Dataforsyningen / Klimadatastyrelsen.
//
// DHM needs a personal access token, so browsers never talk to it. The
// server validates every request against an exact allow-list and fetches
// upstream through one shared Client that adds the token. The package holds
// what every DHM feature needs exactly once:
//
//   - Config: the operator's `map.dhm` block (enabled + token).
//   - Client: the upstream HTTP client. The token goes in the `token`
//     request header (Dataforsyningen's recommended method), never in a
//     URL, so no upstream URL can leak it. The client also enforces the
//     timeout, the global concurrency limit, request coalescing and
//     response checks, and it never follows redirects.
//   - ExactQuery: the allow-list validator for client query strings.
//   - Cache: a bounded LRU (entries, bytes, TTL) for upstream responses.
//   - Attribution: the one CC BY 4.0 credit line for DHM data.
//
// The hillshade tile shape (#680) lives in wms.go. The elevation raster
// shape for the terrain profile (#681) lives in wcs.go, behind the
// ElevationSource interface, which returns a decoded Grid (grid.go) read by
// a stdlib-only float32 GeoTIFF decoder (geotiff.go); utm.go projects
// lat/lon to the rasters' EPSG:25832. Further features add their own
// request shape next to them and reuse the rest.
package dhm

import "strings"

// Attribution is the credit line for DHM data, which Klimadatastyrelsen has
// licensed under CC BY 4.0 since 16 May 2024. It is plain text because it is
// rendered by Leaflet's attribution control. Every DHM-derived view must
// show it.
const Attribution = "Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, " + LicenseName

// LicenseName and LicenseURL identify the licence of DHM data.
const (
	LicenseName = "CC BY 4.0"
	LicenseURL  = "https://creativecommons.org/licenses/by/4.0/"
)

// Config is config.json `map.dhm`.
type Config struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
}

// Active reports whether DHM features may be offered: enabled with a token.
func (c Config) Active() bool {
	return c.Enabled && strings.TrimSpace(c.Token) != ""
}

// Bounds is a geographic extent in degrees (WGS84).
type Bounds struct{ South, West, North, East float64 }

// Coverage is the EX_GeographicBoundingBox of the DHM layers in the public
// dhm_DAF capabilities.
var Coverage = Bounds{South: 54.4265, West: 7.99125, North: 57.778, East: 15.5995}
