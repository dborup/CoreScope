package dhm

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
)

// #687 — the exaggerated hillshade.
//
// Dataforsyningen serves an exaggerated terrain hillshade from a separate
// WMS, wms/dhm_temp, that shows relief much more clearly than #680's
// dhm_terraen_skyggekort. Verified with a token: the service requires the
// token for GetCapabilities too, lists EPSG:3857, and answers the #680
// GetMap shape (1.3.0, 256×256 PNG) with a valid PNG. "temp" and "quick"
// suggest it may disappear without notice, so callers keep the terrain
// layer as a fallback.
//
// The browser request shape does not change: browsers always ask for the
// #680 tile, and the server chooses the upstream.

const (
	ExaggeratedWMSService     = "wms/dhm_temp"
	ExaggeratedHillshadeLayer = "quickDHM_skyggekort_terraen_overdrevet"
)

// Hillshade is an upstream hillshade layer.
type Hillshade int

const (
	HillshadeTerrain     Hillshade = iota // dhm_DAF / dhm_terraen_skyggekort (#680)
	HillshadeExaggerated                  // wms/dhm_temp / quickDHM_skyggekort_terraen_overdrevet (#687)
)

// ParseHillshade reads config `map.dhm.layer`: "exaggerated" (also the
// default for an empty value) or "terrain". Any other value returns
// HillshadeTerrain and false; the caller warns.
func ParseHillshade(s string) (Hillshade, bool) {
	switch s {
	case "", "exaggerated":
		return HillshadeExaggerated, true
	case "terrain":
		return HillshadeTerrain, true
	}
	return HillshadeTerrain, false
}

func (h Hillshade) String() string {
	if h == HillshadeExaggerated {
		return "exaggerated"
	}
	return "terrain"
}

func (h Hillshade) service() string {
	if h == HillshadeExaggerated {
		return ExaggeratedWMSService
	}
	return WMSService
}

func (h Hillshade) layer() string {
	if h == HillshadeExaggerated {
		return ExaggeratedHillshadeLayer
	}
	return HillshadeLayer
}

// hillshadeQuery is the tile's GetMap query for layer h: #680's upstream
// query with only the layer changed.
func (k TileKey) hillshadeQuery(h Hillshade) url.Values {
	q := k.upstreamQuery()
	q.Set("layers", h.layer())
	return q
}

// hillshadeQueryCheck accepts q only if it is exactly the hillshadeQuery of
// the tile its bbox names, so Client.Get cannot send any other request to
// h's service: no other layer, format or parameter.
func hillshadeQueryCheck(h Hillshade) func(url.Values) error {
	return func(q url.Values) error {
		if len(q["bbox"]) != 1 {
			return ErrBadRequest
		}
		k, err := TileFromBBox(q["bbox"][0])
		if err != nil {
			return err
		}
		if q.Encode() != k.hillshadeQuery(h).Encode() {
			return ErrBadRequest
		}
		return nil
	}
}

// FetchHillshade fetches one tile of layer h. Only a real PNG is returned;
// anything else is a FetchError.
func (c *Client) FetchHillshade(h Hillshade, k TileKey) ([]byte, error) {
	data, err := c.Get(h.service(), k.hillshadeQuery(h), "image/png")
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, FetchError{"non-image response"}
	}
	return data, nil
}

// FallbackEligible reports whether err from the exaggerated layer may be
// answered with the terrain layer (#687): the upstream answered with a
// non-200 status, a wrong content type or an invalid PNG, or the connection
// failed. A timeout or a full concurrency budget (busy) is not eligible: a
// second attempt would wait on the same budget for another Timeout (#688
// review F1). Neither is an oversize response or a request we refused.
func FallbackEligible(err error) bool {
	var fe FetchError
	if !errors.As(err, &fe) {
		return false
	}
	switch fe.Reason {
	case "transport", "read", "unexpected content type", "non-image response":
		return true
	}
	code, ok := strings.CutPrefix(fe.Reason, "status ")
	return ok && code != ""
}

// LogFallback logs, throttled like LogFailure and by category only, that
// what failed and the terrain layer was served instead.
func (c *Client) LogFallback(what string, err error) {
	c.logThrottled("[dhm] upstream %s failed (%s); served terrain fallback", what, FailureReason(err))
}
