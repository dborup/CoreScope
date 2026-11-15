// Package dhm is CoreScope's server-side access to Danmarks Højdemodel (DHM),
// served by Dataforsyningen / Klimadatastyrelsen.
//
// DHM needs a personal access token, so browsers never talk to it. The
// server validates every request against an exact allow-list and fetches
// upstream through one shared Client that adds the token. The package holds
// what every DHM feature needs exactly once:
//
//   - Config: the operator's `map.dhm` block (enabled, token, layer,
//     tile rate limit, terrain profile, orthophoto).
//   - Client: the upstream HTTP client. The token goes in the `token`
//     request header (Dataforsyningen's recommended method), never in a
//     URL, so no upstream URL can leak it. The client also enforces the
//     timeout, the global concurrency limit, request coalescing and
//     response checks, and it never follows redirects.
//   - ExactQuery: the allow-list validator for client query strings.
//   - Cache: a bounded LRU (entries, bytes, TTL) for upstream responses.
//   - Attribution: the one CC BY 4.0 credit line for DHM data, with
//     AttributionDerived and the parts it is built from (Owner,
//     DatasetName, DatasetURL, LicenseName, LicenseURL, DerivedNote) for
//     the views that render the dataset and licence links (#733).
//
// The hillshade tile shape (#680) lives in wms.go, the exaggerated
// hillshade service (#687) in hillshade.go, and the orthophoto base-map
// tile (#728, a WMTS on the same token) in ortho.go. The elevation raster
// shape for the terrain profile (#681) lives in wcs.go, behind the
// ElevationSource interface, which returns a decoded Grid (grid.go) read by
// a stdlib-only float32 GeoTIFF decoder (geotiff.go), which also reads the
// tiled EPSG:4326 GeoTIFFs of the Copernicus GLO-30 foreign fill block by
// block (geotiff_tiled.go, #738; the fill itself is internal/copdem); utm.go projects
// lat/lon to the rasters' EPSG:25832. profile.go plans the samples and
// rasters of a terrain profile, physics.go assesses the link, and flight.go
// coalesces calls with cancellation. get.go is the bounded, no-redirect
// GET of the keyless upstreams (internal/mastedb, internal/heritage), and
// polygon.go the shared GeoJSON polygon reading, vertex cap and
// point-in-polygon maths those features measure a point with (#730 M1a).
// Further features add their own request shape next to them and reuse the
// rest.
package dhm

import "strings"

// Attribution is the credit line for DHM data, which Klimadatastyrelsen has
// licensed under CC BY 4.0 since 16 May 2024. It is plain text because it is
// rendered by Leaflet's attribution control. Every DHM-derived view must
// show it.
const Attribution = "Indeholder data fra " + Owner + ", " + DatasetName + ", " + LicenseName

// AttributionDerived is the credit for a view CoreScope computes from DHM
// rather than shows as served: Attribution plus the statement that the data
// was changed, which clause 4 of the terms requires (#733). The terrain
// profile and the profile share card (#727) use it; the hillshade tiles are
// the agency's own rendering and use Attribution.
const AttributionDerived = Attribution + " — " + DerivedNote

// The parts of the credit, each a separate constant because each view
// renders them differently: the map credit and the profile footer link
// DatasetName and LicenseName, and only a derived view adds DerivedNote.
//
// Dataforsyningen's terms for Klimadatastyrelsen data under CC BY 4.0
// ("Vilkår for brug af data", v3.1, August 2024,
// https://dataforsyningen.dk/Vilkaar) require the credit to name the data
// owner, name the licence and link to it, link to the specific dataset (or
// to the data overview), and state whether the data was changed (#733).
const (
	// Owner is the data owner the credit must name.
	Owner = "Klimadatastyrelsen"
	// DatasetName is the dataset the credit must name, and link through
	// DatasetURL.
	DatasetName = "Danmarks Højdemodel"
	// DatasetURL is that dataset on Dataforsyningen: DHM/Terræn, the
	// terrain model behind both the hillshade and the profile's DTM.
	// DHM/Overflade (the profile's surface model, /data/928) is the same
	// product family under the same licence and the same owner; the terms
	// also accept a link to the data overview (/data) instead.
	DatasetURL = "https://dataforsyningen.dk/data/930"

	// LicenseName and LicenseURL identify the licence of DHM data. The
	// name must be shown and must link to the licence itself.
	LicenseName = "CC BY 4.0"
	LicenseURL  = "https://creativecommons.org/licenses/by/4.0/"

	// DerivedNote states that the data shown was changed. It is Danish
	// like the rest of the line, which is Dataforsyningen's own recommended
	// wording.
	DerivedNote = "data bearbejdet"
)

// Config is config.json `map.dhm`.
type Config struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
	// Layer is the hillshade upstream (#687): "exaggerated" (default) or
	// "terrain"; see ParseHillshade.
	Layer string `json:"layer"`
	// TileRateLimitPerMin caps the hillshade tiles one client may pull
	// from upstream per minute (#682 review F3). nil: the server default;
	// 0: off.
	TileRateLimitPerMin *int `json:"tileRateLimitPerMin"`
	// Profile is the terrain profile endpoint (#681).
	Profile ProfileConfig `json:"profile"`
	// Ortho is the orthophoto base layer (#728).
	Ortho OrthoConfig `json:"ortho"`
}

// OrthoConfig is config.json `map.dhm.ortho` (#728). Off unless Enabled;
// it uses map.dhm's token, so it is also off while map.dhm is inactive.
type OrthoConfig struct {
	Enabled bool `json:"enabled"`
}

// ProfileConfig is config.json `map.dhm.profile`. A nil field takes the
// server default; cmd/server resolves and bounds every value.
type ProfileConfig struct {
	Enabled           bool     `json:"enabled"`
	MaxPathKm         *float64 `json:"maxPathKm"`
	MaxSamples        *int     `json:"maxSamples"`
	ChunkKm           *float64 `json:"chunkKm"`
	CacheEntries      *int     `json:"cacheEntries"`
	CacheTTLHours     *float64 `json:"cacheTTLHours"`
	UpstreamTimeoutMs *int     `json:"upstreamTimeoutMs"`
	RateLimitPerMin   *int     `json:"rateLimitPerMin"`
	MaxInFlight       *int     `json:"maxInFlight"`
	// Share is the /p share route with per-profile link previews (#727).
	Share ShareConfig `json:"share"`
	// ForeignFill fills the samples DHM does not cover from Copernicus
	// DEM GLO-30 (#738).
	ForeignFill ForeignFillConfig `json:"foreignFill"`
}

// ForeignFillConfig is config.json `map.dhm.profile.foreignFill` (#738).
// Off unless Enabled: it sends requests to a third party (the AWS Open
// Data bucket of Copernicus DEM). A nil field takes the server default;
// cmd/server resolves and bounds every value.
type ForeignFillConfig struct {
	Enabled      bool `json:"enabled"`
	BlockCacheMB *int `json:"blockCacheMB"`
	TimeoutMs    *int `json:"timeoutMs"`
}

// ShareConfig is config.json `map.dhm.profile.share` (#727). Off unless
// Enabled; a nil UpstreamPerMin takes the server default.
type ShareConfig struct {
	Enabled bool `json:"enabled"`
	// UpstreamPerMin is the plan's shareUpstreamPerMin: the server-wide
	// budget of upstream profile fetches that sharing may cause.
	UpstreamPerMin *int `json:"upstreamPerMin"`
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
