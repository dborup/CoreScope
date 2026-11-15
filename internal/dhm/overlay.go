package dhm

import (
	"context"
	"net/url"
)

// #734 — the DHM tile overlays, by id.
//
// The browser names an overlay by id in the tile request's `layers` key;
// the server maps the id to the upstream service, layer and style, so no
// upstream layer name ever comes from a client. Verified against the
// public GetCapabilities of dhm_DAF (WMS 1.3.0, #734 M0 research): the
// surface hillshade has exactly the terrain hillshade's entry (same bbox,
// scale range, no styles), so it answers the #680 request shape with only
// `layers` changed; the contour layer lists the styles hvid, gul and roed
// and a MaxScaleDenominator of 30000, which Web Mercator first reaches at
// z14 (72 dpi) or z15 (0.28 mm pixels).

const (
	SurfaceLayer = "dhm_overflade_skyggekort"
	ContourLayer = "dhm_kurve_traditionel" // 2.5 m interval, made for 1:25,000
	// ContourStyle is the contour layer's one accepted style (#734 M2).
	// The layer lists hvid, gul and roed (white, yellow, red) besides the
	// unnamed default; only red is a named colour that stands out from
	// both the light and the dark base maps, and the contour pane
	// (mc-contour-pane) is outside the dark-theme invert filter, so the
	// colour is shown as served. The hillshade pane (mcTerrainPane) is not:
	// it gets --terrain-overlay-filter in dark, so contours must not move
	// into it (#798 review F3). One
	// style for both themes keeps one contour cache and no refetch on a
	// theme switch. Any other style is refused: upstream an unknown style
	// is a ServiceException with status 200 (owner check T3).
	ContourStyle = "roed"
	// ContourMinZoom is the first zoom the contour layer draws at: z13
	// tiles are empty, z14 tiles carry lines (owner check T3 on staging,
	// #734). Below it the upstream would answer a blank tile, so the proxy
	// refuses the request and the page never asks.
	ContourMinZoom = 14

	// The dataset pages the overlays' credits link (#733, #734 Q5): the
	// overlays are the agency's own rendering of DHM/Overflade and
	// DHM/Højdekurver, the same owner, dataset family and licence as the
	// terrain hillshade's DHM/Terræn (DatasetURL).
	SurfaceDatasetURL = "https://dataforsyningen.dk/data/928"
	ContourDatasetURL = "https://dataforsyningen.dk/data/926"

	// legacyTerrainLayers is the #680 `layers` value. For one release
	// (#734 Q7) it stays a fixed alias of the terrain id, so pages loaded
	// before the deploy keep working. It is never forwarded upstream.
	legacyTerrainLayers = HillshadeLayer
)

// Overlay is one DHM tile overlay: the id the browser sends, the upstream
// WMS layer that answers it, and the zoom range it is served at.
type Overlay struct {
	ID         string // the browser's `layers` value
	Service    string // upstream WMS service (an allowedServices key)
	Layer      string // upstream layer
	Style      string // upstream `styles` value
	MinZoom    int
	MaxZoom    int
	DatasetURL string // the dataset page its credit links (#733)
	// fallback is served for a tile this overlay fails with a
	// FallbackEligible error (#687); nil: none. It is unexported, so the
	// table cannot be changed through a copy.
	fallback *Overlay
}

// Fallback returns the overlay served when o fails, if it has one.
func (o Overlay) Fallback() (Overlay, bool) {
	if o.fallback == nil {
		return Overlay{}, false
	}
	return *o.fallback, true
}

var (
	overlayTerrain = Overlay{ID: "terrain", Service: WMSService, Layer: HillshadeLayer,
		MinZoom: MinZoom, MaxZoom: MaxZoom, DatasetURL: DatasetURL}
	// overlayExaggerated is how the terrain id is fetched under
	// map.dhm.layer "exaggerated" (#687): not an id of its own.
	overlayExaggerated = Overlay{ID: "terrain", Service: ExaggeratedWMSService, Layer: ExaggeratedHillshadeLayer,
		MinZoom: MinZoom, MaxZoom: MaxZoom, DatasetURL: DatasetURL, fallback: &overlayTerrain}
	overlaySurface = Overlay{ID: "surface", Service: WMSService, Layer: SurfaceLayer,
		MinZoom: MinZoom, MaxZoom: MaxZoom, DatasetURL: SurfaceDatasetURL}
	overlayContours = Overlay{ID: "contours", Service: WMSService, Layer: ContourLayer, Style: ContourStyle,
		MinZoom: ContourMinZoom, MaxZoom: MaxZoom, DatasetURL: ContourDatasetURL}

	// overlayTable is the browser allow-list, in display order.
	overlayTable = []Overlay{overlayTerrain, overlaySurface, overlayContours}
	overlayByID  = func() map[string]Overlay {
		m := make(map[string]Overlay, len(overlayTable))
		for _, o := range overlayTable {
			m[o.ID] = o
		}
		return m
	}()
	// upstreamOverlays is every way an overlay is fetched: what
	// overlayQueryCheck lets through.
	upstreamOverlays = append([]Overlay{overlayExaggerated}, overlayTable...)
)

// LookupOverlay returns the overlay with id; only the ids themselves are
// known (no upstream layer name, case variant or alias).
func LookupOverlay(id string) (Overlay, bool) {
	o, ok := overlayByID[id]
	return o, ok
}

// OverlayIDs returns every overlay id, in display order.
func OverlayIDs() []string {
	out := make([]string, len(overlayTable))
	for i, o := range overlayTable {
		out[i] = o.ID
	}
	return out
}

// parseOverlayID maps a browser `layers` value to its overlay: an id, or
// the legacy terrain alias.
func parseOverlayID(layers string) (Overlay, bool) {
	if layers == legacyTerrainLayers {
		return overlayTerrain, true
	}
	return LookupOverlay(layers)
}

// Resolve returns o as the server fetches it: the terrain id is the
// configured hillshade chain (map.dhm.layer, #687), every other id itself.
func (o Overlay) Resolve(terrain Hillshade) Overlay {
	if o.ID == overlayTerrain.ID && terrain == HillshadeExaggerated {
		return overlayExaggerated
	}
	return o
}

// overlayQuery is the tile's GetMap query for overlay o: #680's upstream
// query with o's layer and style.
func (k TileKey) overlayQuery(o Overlay) url.Values {
	q := k.upstreamQuery()
	q.Set("layers", o.Layer)
	q.Set("styles", o.Style)
	return q
}

// overlayQueryCheck accepts q only if it is exactly the overlayQuery of one
// of service's overlays, for the tile its bbox names, at a zoom inside that
// overlay's range. So Client.Get cannot send any other request to service:
// no other layer, style, format, zoom or parameter. A service has at most
// one overlay per upstream layer, so the check is one map lookup and one
// rebuilt query whatever the number of overlays.
func overlayQueryCheck(service string) func(url.Values) error {
	byLayer := map[string]Overlay{}
	for _, o := range upstreamOverlays {
		if o.Service == service {
			byLayer[o.Layer] = o // a copy: later edits to the table's vars do not reach it
		}
	}
	return func(q url.Values) error {
		if len(q["bbox"]) != 1 || len(q["layers"]) != 1 {
			return ErrBadRequest
		}
		o, ok := byLayer[q["layers"][0]]
		if !ok {
			return ErrBadRequest
		}
		k, err := TileFromBBox(q["bbox"][0])
		if err != nil {
			return err
		}
		if k.Z < o.MinZoom || k.Z > o.MaxZoom || q.Encode() != k.overlayQuery(o).Encode() {
			return ErrBadRequest
		}
		return nil
	}
}

// FetchOverlayAdmitted fetches one tile of overlay o as
// FetchHillshadeAdmitted does (#744): a new upstream call takes an adm
// slot first, or fails at once with ErrAdmissionFull, and the caller stops
// waiting when ctx is done. Only a complete 256×256 PNG is returned
// (#695). o is not trusted: the service's check refuses any overlay that
// is not in the table.
func (c *Client) FetchOverlayAdmitted(ctx context.Context, adm *Admission, o Overlay, k TileKey) ([]byte, error) {
	data, err := c.get(ctx, adm, o.Service, k.overlayQuery(o), []string{"image/png"})
	if err != nil {
		return nil, err
	}
	if err := checkTilePNG(data); err != nil {
		return nil, err
	}
	return data, nil
}
