package main

// #723 M1 — optional "Protected ancient monuments" overlay (Denmark).
//
// The Map and Live pages can draw the Slots- og Kulturstyrelsen register of
// protected ancient monuments (fredede fortidsminder) and their protection
// zones as a WMS overlay. Unlike the DHM hillshade (#680) the service needs
// no token and answers with Access-Control-Allow-Origin: *, so the browser
// fetches the tiles directly and the server has no proxy: its only part is
// the opt-in `heritage` block of /api/config/client, which tells the page
// the overlay may be offered and with which fixed parameters.
//
// Every value below is fixed in code. map.heritage only switches the block
// on; an operator cannot point browsers at another service through it.

const (
	// heritageWMSURL is the agency's GeoServer. Only the /ffgeoserver/public/
	// prefix answers over HTTPS; neighbouring paths on the host redirect to
	// plain http:// (#723 research).
	heritageWMSURL = "https://www.kulturarv.dk/ffgeoserver/public/wms"
	// heritageLayers draws the protection-zone polygons first and the
	// protected monument points on top of them, in one GetMap.
	heritageLayers = "fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet"
	// heritageMinZoom: both layers declare MaxScaleDenominator 250000, and
	// z8-z11 tiles come back as one identical blank PNG; z12 is the first
	// zoom with content, so lower zooms are never requested.
	heritageMinZoom = 12
	// heritageMaxZoom is the Map and Live maximum (their base layers stop
	// at 19); the WMS renders any scale below its limit.
	heritageMaxZoom = 19
	// heritageAttribution is the credit SDFI's national map portal shows for
	// this layer. The service is free under the agency's terms of use, not
	// under a CC licence, so no CC mark goes with it.
	heritageAttribution = "© Slots- og Kulturstyrelsen"
)

// heritageBounds is the union of both layers' EX_GeographicBoundingBox in
// the WMS 1.3.0 capabilities (2026-11-14), rounded outward to 1e-4 degrees:
// [[south, west], [north, east]]. Leaflet requests no tile outside it.
var heritageBounds = [2][2]float64{{54.4646, 8.0899}, {57.7289, 15.5689}}

// HeritageConfig is config.json `map.heritage` (Config.Heritage). Default off.
type HeritageConfig struct {
	Enabled bool `json:"enabled"`
}

// HeritageClientConfig is the `heritage` block of /api/config/client.
// Present only while map.heritage.enabled is true.
type HeritageClientConfig struct {
	Enabled     bool          `json:"enabled"`
	URL         string        `json:"url"`
	Layers      string        `json:"layers"`
	MinZoom     int           `json:"minZoom"`
	MaxZoom     int           `json:"maxZoom"`
	Bounds      [2][2]float64 `json:"bounds"` // [[south, west], [north, east]]
	Attribution string        `json:"attribution"`
}

func heritageClientConfig() *HeritageClientConfig {
	return &HeritageClientConfig{
		Enabled:     true,
		URL:         heritageWMSURL,
		Layers:      heritageLayers,
		MinZoom:     heritageMinZoom,
		MaxZoom:     heritageMaxZoom,
		Bounds:      heritageBounds,
		Attribution: heritageAttribution,
	}
}

func (s *Server) heritageClientConfig() *HeritageClientConfig {
	if s.cfg == nil || !s.cfg.Heritage.Enabled {
		return nil
	}
	return heritageClientConfig()
}

// extractHeritageConfig moves map.heritage into the typed Heritage field,
// the same way extractDHMConfig handles map.dhm, so the browser receives
// only the fixed client block and never the raw config.
func (c *Config) extractHeritageConfig() {
	liftMapBlock(c, "heritage", "heritage overlay", &c.Heritage)
}
