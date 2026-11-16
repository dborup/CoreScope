package main

// #680 — DHM (Danmarks Højdemodel) terrain hillshade overlay proxy.
//
// The Map and Live pages can draw the DHM terrain hillshade as an optional
// overlay. The browser requests WMS tiles from /api/map/dhm/wms; this
// handler validates the request against one fixed, allow-listed shape and
// fetches the tile through the shared DHM client (internal/dhm), which
// adds the token server-side in the `token` request header.
//
// #687: the server chooses the upstream layer (map.dhm.layer). By default
// it is the exaggerated hillshade (wms/dhm_temp); if that fails for a
// tile, the #680 terrain hillshade (dhm_DAF) is served instead. Browsers
// keep requesting the #680 shape either way.
//
// #734: the browser names the overlay by id in `layers` (terrain, surface
// or contours; internal/dhm/overlay.go), and the server maps it to the
// upstream layer. terrain is the #687 chain above, and the #680 layer name
// stays its alias for one release. surface (the surface hillshade) and
// contours (2.5 m contour lines, from z14) have no fallback. Every
// overlay keeps its own cache entries and shares the one per-client
// budget and admission bound.
//
// Security properties, each pinned by dhm_proxy_test.go and the
// internal/dhm tests:
//   - Not an open proxy. The upstream service is a constant, every client
//     parameter is checked against an exact allow-list, unknown parameters
//     are rejected, and the upstream query is rebuilt from the validated
//     tile coordinates rather than copied from the client.
//   - The token is never written to a response, an error, a URL or a log
//     line. Failures are logged by fixed category only.
//   - Upstream bodies are never forwarded unless they are a PNG within the
//     size cap; anything else becomes a transparent tile marked no-store.
//   - Bounded LRU cache (entries + bytes + TTL), a per-request timeout,
//     a concurrency limit towards upstream and coalescing of identical
//     in-flight tiles.
//   - A global admission bound on distinct cold tiles (#744): at most
//     dhmTileMaxInFlight fetch workers exist, running or queued for an
//     upstream slot. Beyond it a tile is refused at once (transparent,
//     no-store, X-CoreScope-Tile: busy) and the per-client charge is given
//     back. A client that leaves stops waiting, and the fetch stops once
//     no client waits for it, so queued work does not outlive its callers.
//   - A per-client budget for tiles that would go upstream
//     (map.dhm.tileRateLimitPerMin, #682 review F3): 429 with Retry-After
//     beyond it. Cached tiles and tiles outside Denmark are not charged.
//
// #728: with map.dhm.ortho.enabled, the same proxy also serves the Danish
// spring orthophoto as a base-map tile at /api/map/dhm/ortho/{z}/{x}/{y}
// (WMTS orto_foraar_webm_DAF, internal/dhm/ortho.go). It holds to every
// property above: the path is validated to a TileKey and nothing else, any
// query string is refused, the upstream query is rebuilt, only a decoded
// 256×256 JPEG is served, and the same coverage gate, per-client budget
// and admission bound apply. Its JPEGs live in their own cache instance,
// so they never evict hillshade tiles.
//
// The endpoints never touch SQLite.

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
)

const (
	dhmClientPath = "/api/map/dhm/wms"
	// dhmOrthoRoute is the ortho tile route (#728) and, as the browser's
	// Leaflet URL template, the url of dhm.ortho in /api/config/client.
	dhmOrthoRoute    = "/api/map/dhm/ortho/{z}/{x}/{y}"
	dhmBrowserMaxAge = 24 * time.Hour
	// dhmFallbackMaxAge is the browser cache lifetime of a terrain tile
	// served in place of a failed exaggerated one (#687), so the browser
	// asks again soon rather than keeping a mixed map for a day.
	dhmFallbackMaxAge = 10 * time.Minute
)

// DHMConfig is config.json `map.dhm` (Config.DHM).
type DHMConfig = dhm.Config

// DHMClientConfig is the `dhm` block of /api/config/client. Present only
// while the proxy is active; it carries what the overlay needs and nothing
// secret.
type DHMClientConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Layer   string `json:"layer"`
	MinZoom int    `json:"minZoom"`
	MaxZoom int    `json:"maxZoom"`
	// Profile reports whether GET /api/terrain/profile is served (#681 M4).
	// The overlay only needs map.dhm; the Path profile tool also needs
	// map.dhm.profile.enabled, so the Map page cannot infer it from
	// Enabled and must be told. No limit or parameter is published with
	// it: the endpoint validates its own request.
	Profile bool `json:"profile"`
	// Share reports whether the /p share route serves per-profile link
	// previews (#727): the profile is served and map.dhm.profile.share.
	// enabled is set. Without it the copy button hands out the plain
	// #/map?profile= link, exactly as before.
	Share bool `json:"share"`
	// Card is the share card's look as the operator set it (#727 M4),
	// so the customizer can say what a shared link's preview shows; a
	// viewer's own settings never reach the card. Present only with Share.
	Card *DHMCardClientConfig `json:"card,omitempty"`
	// PointAntenna is how the profile resolves a free map point's antenna
	// height when the request names none (#746): radio.antennaHeightsM.point,
	// else the built-in default. A point has no per-node key, so this is the
	// whole rule, and the Map page can apply a customizer point default in
	// the one request a drop makes instead of asking twice. Present only
	// with Profile.
	PointAntenna *DHMPointAntenna `json:"pointAntenna,omitempty"`
	Bounds       [2][2]float64    `json:"bounds"` // [[south, west], [north, east]]
	Attribution  string           `json:"attribution"`
	// The credit's parts (#733). Dataforsyningen's terms require the
	// credit to link both the dataset and the licence, and Leaflet's
	// attribution control is the page's, not the server's, so the link
	// targets have to reach the browser. Attribution stays the plain text
	// it always was: the page turns these two marks inside it into
	// anchors, which is why each mark travels with its URL.
	DatasetName string `json:"datasetName"`
	DatasetURL  string `json:"datasetUrl"`
	LicenseName string `json:"licenseName"`
	LicenseURL  string `json:"licenseUrl"`
	// AttributionDerived is the credit of a view computed from DHM
	// (dhm.AttributionDerived): the profile chart footer shows it, with the
	// same two marks as links. Sent so the page keeps no copy of any
	// credit string (#765).
	AttributionDerived string `json:"attributionDerived"`
	// Overlays are the DHM overlays the page may offer, by the id it puts
	// in a tile request's `layers` (#734). Layer stays the #680 value for
	// pages that predate them.
	Overlays []DHMOverlayClientConfig `json:"overlays"`
	// Ortho is present only while map.dhm.ortho.enabled is set (#728).
	Ortho *DHMOrthoClientConfig `json:"ortho,omitempty"`
	// Fill is the foreign fill's credit (#738 M2), present only while the
	// profile is served with map.dhm.profile.foreignFill.enabled.
	Fill *DHMFillClientConfig `json:"fill,omitempty"`
	// Buildings is the building footprints' credit (#730 M3), present
	// only while the profile is served with map.dhm.profile.buildings.
	// enabled.
	Buildings *DHMBuildingsClientConfig `json:"buildings,omitempty"`
}

// DHMBuildingsClientConfig is the credit of a profile classified with
// the INSPIRE – Bygninger footprints (#730 M3), in parts, from
// internal/buildings: the profile footer shows Credit (the response's
// attribution then says the same) with the DHM dataset, the footprints'
// dataset, their source (linked to GeoDanmark's terms, which ask for it)
// and the licence as links, through the same walker as the DHM credit,
// so the page keeps no copy of it. The DHM dataset and the licence are
// the dhm block's own fields. The @geodanmark handle stays text.
type DHMBuildingsClientConfig struct {
	Credit      string `json:"credit"`
	DatasetName string `json:"datasetName"`
	DatasetURL  string `json:"datasetUrl"`
	SourceName  string `json:"sourceName"`
	SourceURL   string `json:"sourceUrl"`
}

func buildingsClientConfig() *DHMBuildingsClientConfig {
	return &DHMBuildingsClientConfig{
		Credit:      buildings.AttributionDerived,
		DatasetName: buildings.DatasetName,
		DatasetURL:  buildings.DatasetURL,
		SourceName:  buildings.Source,
		SourceURL:   buildings.SourceTermsURL,
	}
}

// DHMFillClientConfig is the Copernicus DEM credit of a mixed profile
// (#738 M2), in parts, from internal/copdem: the profile panel shows
// Credit with the dataset and licence marks as links, through the same
// walker as the DHM credit, so the page keeps no copy of it. The long
// Art. 6(b) line is the profile response's attributionFill.
type DHMFillClientConfig struct {
	Credit      string `json:"credit"`
	DatasetName string `json:"datasetName"`
	DatasetURL  string `json:"datasetUrl"`
	LicenseName string `json:"licenseName"`
	LicenseURL  string `json:"licenseUrl"`
}

// DHMOverlayClientConfig is one entry of `dhm.overlays` in
// /api/config/client (#734): an overlay id, the zoom range the proxy
// serves it at, and the dataset its credit links (#733, #734 Q5). The
// credit text and the licence are the block's own: every overlay is
// Klimadatastyrelsen's rendering of Danmarks Højdemodel under CC BY 4.0,
// shown as served.
type DHMOverlayClientConfig struct {
	ID          string `json:"id"`
	MinZoom     int    `json:"minZoom"`
	MaxZoom     int    `json:"maxZoom"`
	DatasetName string `json:"datasetName"`
	DatasetURL  string `json:"datasetUrl"`
}

// dhmClientOverlayIDs are the overlays offered to the page: every id the
// proxy serves (#734 M2 adds contours, from z14).
var dhmClientOverlayIDs = []string{"terrain", "surface", "contours"}

func dhmOverlayClientConfigs() []DHMOverlayClientConfig {
	out := make([]DHMOverlayClientConfig, 0, len(dhmClientOverlayIDs))
	for _, id := range dhmClientOverlayIDs {
		o, ok := dhm.LookupOverlay(id)
		if !ok {
			continue // TestDHM734_OfferedOverlaysAreTableIDs pins that none is skipped
		}
		out = append(out, DHMOverlayClientConfig{
			ID:          o.ID,
			MinZoom:     o.MinZoom,
			MaxZoom:     o.MaxZoom,
			DatasetName: dhm.DatasetName,
			DatasetURL:  o.DatasetURL,
		})
	}
	return out
}

// DHMOrthoClientConfig is `dhm.ortho` of /api/config/client (#728): what
// the orthophoto base layer needs, built from constants only. Its credit
// parts follow #733, like the hillshade's.
type DHMOrthoClientConfig struct {
	URL         string        `json:"url"` // Leaflet URL template
	MinZoom     int           `json:"minZoom"`
	MaxZoom     int           `json:"maxZoom"`
	Bounds      [2][2]float64 `json:"bounds"` // [[south, west], [north, east]]
	Attribution string        `json:"attribution"`
	DatasetName string        `json:"datasetName"`
	DatasetURL  string        `json:"datasetUrl"`
	LicenseName string        `json:"licenseName"`
	LicenseURL  string        `json:"licenseUrl"`
	// ForeignMask marks the z12 tiles inside Bounds that DHM does not
	// cover: Skåne, Schleswig and Rügen away from the coast (M3). The
	// base layer requests no ortho tile there, so the Esri imagery under
	// it shows and no upstream fetch is spent on another country.
	ForeignMask *dhm.TileMask `json:"foreignMask"`
}

// dhmCoverageBounds is dhm.Coverage as Leaflet bounds.
func dhmCoverageBounds() [2][2]float64 {
	c := dhm.Coverage
	return [2][2]float64{{c.South, c.West}, {c.North, c.East}}
}

func dhmOrthoClientConfig(mask *dhm.TileMask) *DHMOrthoClientConfig {
	return &DHMOrthoClientConfig{
		URL:         dhmOrthoRoute,
		MinZoom:     dhm.OrthoMinZoom,
		MaxZoom:     dhm.OrthoMaxZoom,
		Bounds:      dhmCoverageBounds(),
		Attribution: dhm.OrthoAttribution,
		DatasetName: dhm.OrthoDatasetName,
		DatasetURL:  dhm.OrthoDatasetURL,
		LicenseName: dhm.LicenseName,
		LicenseURL:  dhm.LicenseURL,
		ForeignMask: mask,
	}
}

// DHMCardClientConfig is the client config's dhm.card (#727 M4): the two
// display choices of map.dhm.profile.card, resolved. Nothing else of the
// card block (its cache size) reaches the browser.
type DHMCardClientConfig struct {
	Theme string `json:"theme"` // "dark" or "light"
	Names bool   `json:"names"` // node names on the card
}

func cardClientConfig(set cardSettings) *DHMCardClientConfig {
	theme := "light"
	if set.dark {
		theme = "dark"
	}
	return &DHMCardClientConfig{Theme: theme, Names: set.names}
}

// DHMPointAntenna is a free point's antenna height without a request
// height, and its source: "operator-role" or "point-default", as in a
// profile response's antennaSource.
type DHMPointAntenna struct {
	M      float64 `json:"m"`
	Source string  `json:"source"`
}

func dhmClientConfig() *DHMClientConfig {
	return &DHMClientConfig{
		Enabled:            true,
		URL:                dhmClientPath,
		Layer:              dhm.HillshadeLayer,
		MinZoom:            dhm.MinZoom,
		MaxZoom:            dhm.MaxZoom,
		Bounds:             dhmCoverageBounds(),
		Attribution:        dhm.Attribution,
		DatasetName:        dhm.DatasetName,
		DatasetURL:         dhm.DatasetURL,
		LicenseName:        dhm.LicenseName,
		LicenseURL:         dhm.LicenseURL,
		AttributionDerived: dhm.AttributionDerived,
		Overlays:           dhmOverlayClientConfigs(),
	}
}

// dhmTileQueryParams documents the exact tile request shape for OpenAPI:
// every fixed key with its only accepted value, layers as the enum of
// overlay ids (#734), plus bbox, all required.
func dhmTileQueryParams() []paramMeta {
	fixed := dhm.TileParams()
	names := make([]string, 0, len(fixed))
	for k := range fixed {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]paramMeta, 0, len(names)+1)
	for _, k := range names {
		if k == "layers" {
			// TileParams' layers is the #680 value, the terrain alias.
			out = append(out, paramMeta{Name: k, Type: "string", Required: true,
				Enum: append(dhm.OverlayIDs(), fixed[k]),
				Description: "the overlay id: terrain, surface or contours; " + strconv.Quote(fixed[k]) +
					" is the #680 alias of terrain, accepted for one release"})
			continue
		}
		desc := "must be exactly " + strconv.Quote(fixed[k])
		if fixed[k] == "" {
			desc = "must be present and empty"
		}
		out = append(out, paramMeta{Name: k, Description: desc, Type: "string", Required: true})
	}
	return append(out, paramMeta{Name: "bbox", Description: "minx,miny,maxx,maxy of one EPSG:3857 tile at zoom 7-19", Type: "string", Required: true})
}

// extractDHMConfig moves map.dhm into the typed DHM field, because the
// free-form Map block is shipped verbatim by /api/config/client. The raw
// block is deleted even when it is malformed: it must never reach clients.
func (c *Config) extractDHMConfig() {
	liftMapBlock(c, "dhm", "terrain overlay", &c.DHM)
}

// extractTypedMapBlocks lifts every typed block out of the client-visible
// Map: map.dhm (#680), map.heritage (#723), map.masts (#724) and
// map.skraafoto (#737), and drops the tokens the client must not get (#741).
func (c *Config) extractTypedMapBlocks() {
	c.extractDHMConfig()
	c.dropDHMTokenFromTiles()
	c.dropUngatedTileTokens()
	c.extractHeritageConfig()
	c.extractMastsConfig()
	c.extractSkraafotoConfig()
}

// dropDHMTokenFromTiles keeps the DHM token out of the tile config, which
// /api/config/client ships verbatim (map.tiles, and the legacy top-level
// tiles and mapDarkTileProvider). The Dataforsyningen base maps (#741)
// take a token of their own, map.tiles.providers.dataforsyningen.token,
// client-visible while that provider is on (dropUngatedTileTokens);
// it comes from the same Dataforsyningen account as map.dhm.token, so the
// likeliest leak is the DHM token pasted there as well. Every string value
// in those blocks that contains the DHM token is deleted (an array element
// is blanked), wherever and however its key is spelled, and the key is
// logged, never the value. The DHM proxy keeps its token. Runs once, at
// load, on a config of a few kilobytes.
func (c *Config) dropDHMTokenFromTiles() {
	token := strings.TrimSpace(c.DHM.Token)
	if token == "" {
		return
	}
	var dropped []string
	dropString(c.Map["tiles"], token, "map.tiles", &dropped)
	dropString(c.Tiles, token, "tiles", &dropped)
	if strings.Contains(c.MapDarkTileProvider, token) {
		c.MapDarkTileProvider = ""
		dropped = append(dropped, "mapDarkTileProvider")
	}
	for _, p := range dropped {
		log.Printf("[dhm] config %s holds the map.dhm.token value and was removed from the client config: map.dhm.token is server-only, so the Dataforsyningen base maps need a token of their own in map.tiles.providers.dataforsyningen.token", p)
	}
}

// dropString deletes from a decoded JSON value every string that contains
// s, appending the path of each to dropped.
func dropString(v interface{}, s, path string, dropped *[]string) {
	if m := jsonObjectAt(v); m != nil {
		for k, e := range m {
			if str, ok := e.(string); ok && strings.Contains(str, s) {
				delete(m, k)
				*dropped = append(*dropped, path+"."+k)
				continue
			}
			dropString(e, s, path+"."+k, dropped)
		}
		return
	}
	a, _ := v.([]interface{})
	for i, e := range a {
		if str, ok := e.(string); ok && strings.Contains(str, s) {
			a[i] = ""
			*dropped = append(*dropped, path+"["+strconv.Itoa(i)+"]")
			continue
		}
		dropString(e, s, path+"["+strconv.Itoa(i)+"]", dropped)
	}
}

// ── Proxy ──────────────────────────────────────────────────────────────────

type dhmProxyOptions struct {
	client          dhm.ClientOptions
	cacheMaxEntries int
	cacheMaxBytes   int
	cacheTTL        time.Duration
	// rateLimitPerMin is the per-client budget of upstream-bound tiles;
	// 0 is off. newDHMProxyFromConfig sets it from the config.
	rateLimitPerMin int
	trustedProxies  []*net.IPNet
	// tileMaxInFlight bounds distinct cold-tile fetches, running or
	// queued (#744); defaultDHMProxyOptions sets dhmTileMaxInFlight.
	tileMaxInFlight int
	// The ortho cache (#728), apart from the hillshade cache so large
	// JPEGs never evict hillshade tiles. Used only with map.dhm.ortho.
	orthoCacheMaxEntries int
	orthoCacheMaxBytes   int
	orthoCacheTTL        time.Duration
}

// dhmTileMaxInFlight is the shipped bound on cold-tile fetch workers
// (#744): 24x the upstream concurrency of 4, like the profile's
// maxInFlight cap is a multiple of the same budget. #734 M2 raised it from
// 64, measured with test-issue-734-dhm-overlay-budget-e2e.js: a cold
// 1920x1080 Map view with a hillshade and the contour lines on (z15) is 90
// tiles, 45 per overlay, so the usual two-overlay view fits; rare
// alignments (up to 2x54 = 108 at 1920x1080) and larger screens (2560x1440:
// up to 154) lean on the busy retry (#728 N3). Meanwhile a
// flood holds at most 96 goroutines that read no body until they have an
// upstream slot. An admitted tile waits behind at most 23 others for one
// of the 4 slots: at the slowest cold tile measured on staging (0.35 s,
// #734 T9) that is 8.4 s, inside the 10 s Timeout that bounds the wait.
const dhmTileMaxInFlight = 96

// dhmTileRateLimitDefault is the shipped per-client budget of tiles that
// miss the server cache, per minute, shared by every overlay. Sized from
// the tile ceilings of the scripted sessions in tests/dhm-tile-budget.js
// (a 1920×1080 Map, HTTP cache off, so upper bounds; every step bounded by
// the tiles its view change can touch, at a person's pace of 2.5 s per
// view, times 1.3 for flick glides the scripts leave out). One overlay
// from z8 (test-issue-681-dhm-tile-budget-e2e.js): a normal session —
// load, three zoom steps in, four pans, two out — has a ceiling of 384
// tiles in 25 s, 1198/min with the headroom: the earlier 1200 fitted one
// overlay. Two overlays, a hillshade and the contour lines, from z15
// (#734 M2, test-issue-734-dhm-overlay-budget-e2e.js): a normal session —
// two zoom steps in, four pans, two out — has a ceiling of 660 tiles in
// 22.5 s, 2288/min with the headroom, and an aggressive one 15→19→15 with
// sixteen pans a ceiling of 1692 as one burst. 2400 is the next round
// step over 2288, so a person using the map never meets it, while a
// script walking the ~97 M tiles of DHM coverage is held to 40 per second
// per address.
const dhmTileRateLimitDefault = 2400

// dhmTileRateLimitMax bounds map.dhm.tileRateLimitPerMin like
// profile.rateLimitPerMin bounds its own key (0-6000, 600x its default of
// 10): 25x the shipped tile default, comfortably under the point
// (perMin above ~6e10, where time.Minute/perMin truncates to zero) where
// the limiter would silently stop limiting (#690 review N2).
const dhmTileRateLimitMax = 60000

// dhmTileRateLimit is map.dhm.tileRateLimitPerMin: unset is the default, 0
// is off, and anything outside [0, dhmTileRateLimitMax] warns and falls
// back to the default too, like profile.rateLimitPerMin.
func dhmTileRateLimit(c dhm.Config, logf func(string, ...interface{})) int {
	if logf == nil {
		logf = log.Printf
	}
	if c.TileRateLimitPerMin == nil {
		return dhmTileRateLimitDefault
	}
	v := *c.TileRateLimitPerMin
	if v < 0 || v > dhmTileRateLimitMax {
		logf("[dhm] config map.dhm.tileRateLimitPerMin is out of range; using %d", dhmTileRateLimitDefault)
		return dhmTileRateLimitDefault
	}
	return v
}

func defaultDHMProxyOptions() dhmProxyOptions {
	return dhmProxyOptions{
		client:          dhm.DefaultClientOptions(),
		cacheMaxEntries: 2048,
		cacheMaxBytes:   64 << 20,
		cacheTTL:        24 * time.Hour,
		tileMaxInFlight: dhmTileMaxInFlight,
		// The #728 plan's proposal at ~25 KB a tile; staging measures real
		// ortho tile sizes before these are final. Aerial photos change
		// about once a year, hence the long TTL.
		orthoCacheMaxEntries: 2048,
		orthoCacheMaxBytes:   96 << 20,
		orthoCacheTTL:        7 * 24 * time.Hour,
	}
}

type dhmProxy struct {
	client *dhm.Client
	layer  dhm.Hillshade // configured upstream layer
	cache  *dhm.Cache[dhmCacheKey]
	// orthoCache holds ortho tiles (#728); nil while map.dhm.ortho is off,
	// and the ortho route then answers 404.
	orthoCache *dhm.Cache[dhmCacheKey]
	// orthoMask is dhm.OrthoForeignMask, built when the proxy starts so no
	// client config request pays for it (#728 M3); nil with orthoCache.
	orthoMask *dhm.TileMask
	// limiter and admit are shared by every tile product: one token, one
	// fair-use budget.
	limiter *ipRateLimiter // nil: no per-client limit
	admit   *dhm.Admission // bound on cold-tile fetch workers (#744)
	now     func() time.Time
}

// dhmProduct is what a cached tile shows (#728).
type dhmProduct uint8

const (
	dhmProductTerrain     dhmProduct = iota // #680 hillshade
	dhmProductExaggerated                   // #687 hillshade
	dhmProductOrtho                         // #728 orthophoto
	dhmProductSurface                       // #734 surface hillshade
	dhmProductContours                      // #734 contour lines
)

// overlayProduct is the product an upstream overlay's tiles are, by its
// upstream layer: the terrain id is two products, one per #687 layer.
// Every overlay in internal/dhm's table has one (TestDHM734_
// EveryOverlayHasItsOwnProduct); one without would be a programming
// error, and must not share another product's cache entries.
func overlayProduct(o dhm.Overlay) dhmProduct {
	switch o.Layer {
	case dhm.HillshadeLayer:
		return dhmProductTerrain
	case dhm.ExaggeratedHillshadeLayer:
		return dhmProductExaggerated
	case dhm.SurfaceLayer:
		return dhmProductSurface
	case dhm.ContourLayer:
		return dhmProductContours
	}
	panic("dhm: overlay without a cache product")
}

// name is the product in a log line that names two of them.
func (pr dhmProduct) name() string {
	switch pr {
	case dhmProductExaggerated:
		return "exaggerated"
	case dhmProductOrtho:
		return "ortho"
	case dhmProductSurface:
		return "surface"
	case dhmProductContours:
		return "contours"
	}
	return "terrain"
}

// tileLabel is what a log line says failed or was refused.
func (pr dhmProduct) tileLabel() string {
	switch pr {
	case dhmProductExaggerated:
		return "exaggerated hillshade tile"
	case dhmProductOrtho:
		return "ortho tile"
	case dhmProductSurface:
		return "surface hillshade tile"
	case dhmProductContours:
		return "contour tile"
	}
	return "hillshade tile"
}

func hillshadeProduct(h dhm.Hillshade) dhmProduct {
	if h == dhm.HillshadeExaggerated {
		return dhmProductExaggerated
	}
	return dhmProductTerrain
}

// dhmCacheKey is a tile of the product it actually is, so a terrain
// fallback is never served as an exaggerated tile (#687), nor any tile as
// another product's.
type dhmCacheKey struct {
	Product dhmProduct
	Tile    dhm.TileKey
}

// newDHMProxy returns nil unless map.dhm is enabled with a token: an
// inactive proxy is not offered to clients and its endpoint returns 404.
func newDHMProxy(cfg dhm.Config, opts dhmProxyOptions) *dhmProxy {
	client := dhm.NewClient(cfg, opts.client)
	if client == nil {
		return nil
	}
	layer, ok := dhm.ParseHillshade(cfg.Layer)
	if !ok {
		// The value is not echoed: it could be a misplaced token.
		opts.client.Logf(`[dhm] config map.dhm.layer must be "exaggerated" or "terrain"; using "terrain"`)
	}
	p := &dhmProxy{
		client: client,
		layer:  layer,
		cache:  dhm.NewCache[dhmCacheKey](opts.cacheMaxEntries, opts.cacheMaxBytes, opts.cacheTTL),
		limiter: newIPRateLimiter("dhm", "map.dhm.tileRateLimitPerMin", opts.rateLimitPerMin,
			opts.trustedProxies, opts.client.Logf),
		admit: dhm.NewAdmission(opts.tileMaxInFlight),
		now:   opts.client.Now,
	}
	if cfg.Ortho.Enabled {
		p.orthoCache = dhm.NewCache[dhmCacheKey](opts.orthoCacheMaxEntries, opts.orthoCacheMaxBytes, opts.orthoCacheTTL)
		mask := dhm.OrthoForeignMask()
		p.orthoMask = &mask
	}
	return p
}

// dhmTransparentPNG is served outside coverage and on any upstream failure,
// so the overlay stays silent instead of drawing broken tiles.
var dhmTransparentPNG = func() []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// writeDHMTile writes a PNG tile the browser may cache for maxAge; 0 is
// no-store.
func writeDHMTile(w http.ResponseWriter, data []byte, maxAge time.Duration, state string) {
	writeTileImage(w, "image/png", data, maxAge, state)
}

// writeTileImage is writeDHMTile for any image type.
func writeTileImage(w http.ResponseWriter, contentType string, data []byte, maxAge time.Duration, state string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-CoreScope-Tile", state)
	if maxAge > 0 {
		h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(maxAge/time.Second)))
	} else {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (p *dhmProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	overlay, key, err := dhm.ParseTileRequest(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	if !key.IntersectsCoverage() {
		writeDHMTile(w, dhmTransparentPNG, dhmBrowserMaxAge, "outside")
		return
	}
	src := overlay.Resolve(p.layer) // the terrain id is the configured chain (#687)
	// Only a tile that will go upstream is charged to the client.
	if _, cached := p.cache.Get(dhmCacheKey{Product: overlayProduct(src), Tile: key}, p.now()); !cached {
		if ok, retry := p.limiter.allow(r); !ok {
			writeRateLimited(w, retry)
			return
		}
	}
	ctx := r.Context()
	data, hit, err := p.layerTile(ctx, src, key)
	if err == nil {
		writeDHMTile(w, data, dhmBrowserMaxAge, tileState("", hit))
		return
	}
	if ctx.Err() != nil {
		return // the client left: nobody to answer, nothing to log
	}
	if errors.Is(err, dhm.ErrAdmissionFull) {
		// #744: refused before any upstream work, so the charge is given
		// back, as for a refused profile (#705/#707).
		p.limiter.refund(r)
		p.client.LogRefusal(overlayProduct(overlay).tileLabel())
		writeDHMTile(w, dhmTransparentPNG, 0, "busy")
		return
	}
	fallback, hasFallback := src.Fallback()
	if !hasFallback || !dhm.FallbackEligible(err) {
		// No fallback (#734: only the exaggerated terrain has one), or a
		// timeout or a full budget: retrying on the fallback would only
		// wait again (#688 review F1).
		p.client.LogFailure(overlayProduct(src).tileLabel(), err)
		writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
		return
	}
	// #687: the exaggerated tile failed; serve the terrain tile instead.
	data, hit, fallbackErr := p.layerTile(ctx, fallback, key)
	if ctx.Err() != nil {
		return
	}
	if fallbackErr != nil {
		first, second := overlayProduct(src), overlayProduct(fallback)
		p.client.LogFailure(second.tileLabel(), dhm.FetchError{Reason: first.name() + " " + dhm.FailureReason(err) +
			", " + second.name() + " " + dhm.FailureReason(fallbackErr)})
		writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
		return
	}
	p.client.LogFallback(overlayProduct(src).tileLabel(), err)
	writeDHMTile(w, data, dhmFallbackMaxAge, tileState("fallback", hit))
}

// layerTile returns the tile of one upstream overlay, from the cache or
// upstream, and whether it was a cache hit. Only successes are cached. A
// cold tile is admitted against p.admit and waited for until ctx is done.
func (p *dhmProxy) layerTile(ctx context.Context, o dhm.Overlay, k dhm.TileKey) ([]byte, bool, error) {
	ck := dhmCacheKey{Product: overlayProduct(o), Tile: k}
	if data, ok := p.cache.Get(ck, p.now()); ok {
		return data, true, nil
	}
	data, err := p.client.FetchOverlayAdmitted(ctx, p.admit, o, k)
	if err != nil {
		return nil, false, err
	}
	p.cache.Put(ck, data, p.now())
	return data, false, nil
}

// serveOrtho serves one orthophoto tile (#728) of
// GET /api/map/dhm/ortho/{z}/{x}/{y}, with the hillshade's coverage gate,
// per-client budget and admission bound. Failures are a transparent PNG,
// which a base layer shows as the map background.
func (p *dhmProxy) serveOrtho(w http.ResponseWriter, r *http.Request) {
	if p.orthoCache == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	v := mux.Vars(r)
	key, err := dhm.ParseOrthoTile(v["z"], v["x"], v["y"])
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	if !key.IntersectsCoverage() {
		writeDHMTile(w, dhmTransparentPNG, dhmBrowserMaxAge, "outside")
		return
	}
	ck := dhmCacheKey{Product: dhmProductOrtho, Tile: key}
	if data, ok := p.orthoCache.Get(ck, p.now()); ok {
		writeTileImage(w, "image/jpeg", data, dhmBrowserMaxAge, "hit")
		return
	}
	// Only a tile that will go upstream is charged, from the hillshade's
	// budget: one client, one token.
	if ok, retry := p.limiter.allow(r); !ok {
		writeRateLimited(w, retry)
		return
	}
	ctx := r.Context()
	data, err := p.client.FetchOrthoAdmitted(ctx, p.admit, key)
	if err == nil {
		p.orthoCache.Put(ck, data, p.now())
		writeTileImage(w, "image/jpeg", data, dhmBrowserMaxAge, "miss")
		return
	}
	if ctx.Err() != nil {
		return // the client left: nobody to answer, nothing to log
	}
	if errors.Is(err, dhm.ErrAdmissionFull) {
		p.limiter.refund(r)
		p.client.LogRefusal("ortho tile")
		writeDHMTile(w, dhmTransparentPNG, 0, "busy")
		return
	}
	p.client.LogFailure("ortho tile", err)
	writeDHMTile(w, dhmTransparentPNG, 0, "unavailable")
}

// tileState is the X-CoreScope-Tile value: hit/miss, or with a prefix
// fallback/fallback-hit.
func tileState(prefix string, hit bool) string {
	switch {
	case prefix == "" && hit:
		return "hit"
	case prefix == "":
		return "miss"
	case hit:
		return prefix + "-hit"
	}
	return prefix
}

func newDHMProxyFromConfig(cfg *Config) *dhmProxy {
	if cfg == nil {
		return nil
	}
	opts := defaultDHMProxyOptions()
	opts.rateLimitPerMin = dhmTileRateLimit(cfg.DHM, opts.client.Logf)
	opts.trustedProxies = parseCIDRListQuiet(cfg.WSTrustedProxies())
	return newDHMProxy(cfg.DHM, opts)
}

func (s *Server) dhmClientConfig() *DHMClientConfig {
	if s.dhm == nil {
		return nil
	}
	c := dhmClientConfig()
	c.Profile = s.terrain != nil
	c.Share = s.terrain != nil && s.terrain.set.share.enabled
	if c.Share {
		c.Card = cardClientConfig(s.terrain.set.card)
	}
	if s.terrain != nil {
		h, src, _ := s.terrain.antenna(terrainEnd{kind: "point"}, nil)
		c.PointAntenna = &DHMPointAntenna{M: h, Source: src}
	}
	if s.dhm.orthoCache != nil {
		c.Ortho = dhmOrthoClientConfig(s.dhm.orthoMask)
	}
	if s.terrain != nil && s.terrain.fill != nil {
		c.Fill = &DHMFillClientConfig{
			Credit:      copdem.Credit,
			DatasetName: copdem.DatasetName,
			DatasetURL:  copdem.DatasetURL,
			LicenseName: copdem.LicenseName,
			LicenseURL:  copdem.LicenseURL,
		}
	}
	if s.terrain != nil && s.terrain.buildings != nil {
		c.Buildings = buildingsClientConfig()
	}
	return c
}

func (s *Server) handleDHMTile(w http.ResponseWriter, r *http.Request) {
	if s.dhm == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.dhm.ServeHTTP(w, r)
}

// handleDHMOrtho is GET /api/map/dhm/ortho/{z}/{x}/{y} (#728): 404 unless
// map.dhm is active and map.dhm.ortho.enabled.
func (s *Server) handleDHMOrtho(w http.ResponseWriter, r *http.Request) {
	if s.dhm == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.dhm.serveOrtho(w, r)
}
