package main

import (
	"errors"
	"math"
	"net/http"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/heritage"
	"github.com/meshcore-analyzer/mastedb"
	"github.com/meshcore-analyzer/skraafoto"
)

// site is where every probe asks: Copenhagen's City Hall Square, inside
// the coverage of every service.
var site = dhm.LatLon{Lat: 55.6761, Lon: 12.5683}

// siteZoom is the tile zoom of the WMS and WMTS probes: inside every
// overlay's range, contours (from zoom 14) included, and the ortho's.
const siteZoom = 14

// skaermkortSkip is why Skærmkort is not probed: #741 builds its request
// in the browser only, and the smoke check sends no hand-written URL.
const skaermkortSkip = "no Go request builder: the Skærmkort WMS URL is built in the browser " +
	"(public/map-tile-providers.js, #741), with map.tiles.providers.dataforsyningen.token; " +
	"probe it here once a Go builder exists"

// naturSkip is why Natur- og Friluftskort is not probed, for the same
// reason as Skærmkort (#741 M2).
const naturSkip = "no Go request builder: the Natur- og Friluftskort WMS URL is built in the browser " +
	"(public/map-tile-providers.js, #741 M2), with map.tiles.providers.dataforsyningen.token; " +
	"probe it here once a Go builder exists"

// settings are the command's knobs.
type settings struct {
	year int // Skråfoto collection the search asks
	// cdnPath is the photo the CDN read asks for when the search did not
	// run or found none; "" skips the read. No path is built in: photos
	// are only known from a search, and the documented example photo
	// answered 404 on 2026-11-15.
	cdnPath string
}

func defaultSettings() settings {
	return settings{year: 2025}
}

// errNoCDNPath skips the CDN read: no photo to read.
var errNoCDNPath = skipError("no photo to read: needs the STAC search (" + tokenDHM + ") or -skraafoto-cdn-path")

// siteTile is the Web Mercator tile holding site at siteZoom.
func siteTile() dhm.TileKey { return siteTileAt(siteZoom) }

// siteTileAt is the Web Mercator tile holding site at zoom z.
func siteTileAt(z int) dhm.TileKey {
	n := math.Exp2(float64(z))
	lat := site.Lat * math.Pi / 180
	return dhm.TileKey{
		Z: z,
		X: int((site.Lon + 180) / 360 * n),
		Y: int((1 - math.Log(math.Tan(lat)+1/math.Cos(lat))/math.Pi) / 2 * n),
	}
}

// siteUTM is site in EPSG:25832 metres, floored to whole metres.
func siteUTM() (e, n int) {
	fe, fn := dhm.ToUTM32(site.Lat, site.Lon)
	return int(fe), int(fn)
}

// siteSegment is a 1 km east-west building query line from site.
func siteSegment() buildings.Segment {
	e, n := siteUTM()
	return buildings.Segment{
		Line:    [4]int64{int64(e) * 10, int64(n) * 10, int64(e+1000) * 10, int64(n) * 10},
		RadiusM: buildings.DefaultRadiusM,
	}
}

// siteRaster is a 1 km square of model at 10 m, snapped to 100 m.
func siteRaster(m dhm.Model) dhm.RasterRequest {
	e, n := siteUTM()
	e0, n0 := float64(e/100*100), float64(n/100*100)
	return dhm.RasterRequest{Model: m, BBox: [4]float64{e0, n0, e0 + 1000, n0 + 1000}, Width: 100, Height: 100}
}

// buildProbes is every upstream's allow-listed request shape, in the
// order they run.
func buildProbes(s settings) []probe {
	k := siteTile()
	e, n := siteUTM()
	var ps []probe

	// DHM WMS: every overlay id, as the server fetches it under each
	// map.dhm.layer value; the terrain id is two upstream layers. A
	// rendered id (#792) is no WMS layer: its WCS raster is probed below.
	for _, id := range dhm.OverlayIDs() {
		if base, _ := dhm.LookupOverlay(id); base.Rendered() {
			continue
		}
		seen := map[string]bool{}
		for _, h := range []dhm.Hillshade{dhm.HillshadeExaggerated, dhm.HillshadeTerrain} {
			base, _ := dhm.LookupOverlay(id)
			o := base.Resolve(h)
			name := "dhm-wms-" + id
			if o != base {
				name += "-" + h.String()
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			ps = append(ps, probe{
				name:   name,
				token:  tokenDHM,
				url:    func() (string, error) { return dhm.RequestURL(dhm.DefaultBaseURL, o.Service, k.OverlayQuery(o)), nil },
				status: http.StatusOK,
				check:  checkImage("png", dhm.TileSize),
			})
		}
	}
	// DHM WCS: the terrain profile's raster, both models.
	for _, m := range []dhm.Model{dhm.ModelTerrain, dhm.ModelSurface} {
		r := siteRaster(m)
		ps = append(ps, probe{
			name:  "dhm-wcs-" + string(m),
			token: tokenDHM,
			url: func() (string, error) {
				if err := r.Validate(); err != nil {
					return "", err
				}
				return dhm.RequestURL(dhm.DefaultBaseURL, dhm.WCSService, r.CoverageQuery()), nil
			},
			status: http.StatusOK,
			check:  checkTIFF,
		})
	}
	// #792: the colour-by-elevation tile's raster, at its top zoom.
	if er, ok := dhm.ElevationRaster(siteTileAt(dhm.ElevationMaxZoom)); ok {
		ps = append(ps, probe{
			name:  "dhm-wcs-elevation",
			token: tokenDHM,
			url: func() (string, error) {
				if err := er.Validate(); err != nil {
					return "", err
				}
				return dhm.RequestURL(dhm.DefaultBaseURL, dhm.WCSService, er.CoverageQuery()), nil
			},
			status: http.StatusOK,
			check:  checkTIFF,
		})
	}
	ps = append(ps, probe{
		name:  "dhm-ortho-wmts",
		token: tokenDHM,
		url: func() (string, error) {
			return dhm.RequestURL(dhm.DefaultBaseURL, dhm.OrthoWMTSService, k.OrthoQuery()), nil
		},
		status: http.StatusOK,
		check:  checkImage("jpeg", dhm.TileSize),
	})

	ps = append(ps, probe{
		name: "buildings-inspire-ogcapi",
		url: func() (string, error) {
			seg := siteSegment()
			if !seg.Valid() {
				return "", buildings.ErrBadSegment
			}
			return buildings.NewClient(buildings.DefaultClientOptions()).SegmentURL(seg), nil
		},
		status: http.StatusOK,
		check:  checkFeatures,
	})
	ps = append(ps, probe{
		name: "heritage-wfs",
		url: func() (string, error) {
			cell, ok := heritage.CellOf(float64(e), float64(n))
			if !ok {
				return "", heritage.ErrBadCell
			}
			c, err := heritage.NewClient(heritage.DefaultClientOptions())
			if err != nil {
				return "", err
			}
			return c.CellURL(cell), nil
		},
		status: http.StatusOK,
		check:  checkFeatures,
	})
	ps = append(ps, probe{
		name: "mastedb-wfs",
		url: func() (string, error) {
			cell := mastedb.Cell{E: e / mastedb.CellSizeM, N: n / mastedb.CellSizeM}
			if !cell.Valid() {
				return "", mastedb.ErrBadCell
			}
			return mastedb.NewClient(mastedb.DefaultClientOptions()).CellURL(cell), nil
		},
		status: http.StatusOK,
		check:  checkGML,
	})

	copdemRange := dhm.Range{From: 0, To: copdem.HeaderBytes - 1, MaxBody: copdem.HeaderBytes}
	ps = append(ps, probe{
		name: "copdem-cog-range",
		url: func() (string, error) {
			t, ok := copdem.TileAt(site.Lat, site.Lon)
			if !ok || !t.Listed() {
				return "", errors.New("site tile not in the Copernicus tile list")
			}
			return t.URL(), nil
		},
		rng:    &copdemRange,
		status: http.StatusPartialContent,
		check:  checkRange(copdemRange, checkTIFF),
	})

	// Skråfoto: the search, then a read of the photo it found (or the
	// -skraafoto-cdn-path photo without a token; SKIP without either).
	cdnPath := s.cdnPath
	ps = append(ps, probe{
		name:   "skraafoto-stac-search",
		token:  tokenDHM,
		url:    func() (string, error) { return skraafoto.SearchURL(s.year, e, n, skraafoto.North) },
		status: http.StatusOK,
		check:  checkFeatures,
		then: func(r *reply) {
			// ParseSearch returns the candidates nearest the footprint
			// centroid first (#737 M3a); the first with a CDN path is read.
			if items, _, err := skraafoto.ParseSearch(r.body, s.year, skraafoto.North); err == nil {
				for _, item := range items {
					if item.Path != "" {
						cdnPath = item.Path
						break
					}
				}
			}
		},
	})
	cdnRange := dhm.Range{From: 0, To: skraafoto.HeaderBytes - 1, MaxBody: skraafoto.HeaderBytes}
	ps = append(ps, probe{
		name: "skraafoto-cdn-range",
		url: func() (string, error) {
			if cdnPath == "" {
				return "", errNoCDNPath
			}
			return skraafoto.CDNURL(cdnPath)
		},
		rng:    &cdnRange,
		status: http.StatusPartialContent,
		check:  checkRange(cdnRange, checkTIFF),
	})

	ps = append(ps, probe{name: "skaermkort-wms", skip: skaermkortSkip})
	ps = append(ps, probe{name: "natur-friluft-wms", skip: naturSkip})
	return ps
}
