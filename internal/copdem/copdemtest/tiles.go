package copdemtest

import (
	"regexp"
	"strconv"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// Layout is a synthetic GLO-30 tile: W × H pixels over the 1° square in
// Block-pixel blocks, with the ½ overview. The real layout between 50°
// and 60° N is {2400, 3600, 1024}.
type Layout struct{ W, H, Block int }

// GLO30 is the real layout at Danish latitudes.
var GLO30 = Layout{2400, 3600, 1024}

// TileCOG is the tile whose south-west corner is lat, lon at layout l,
// with value at every pixel centre (PixelIsPoint, as GLO-30).
func TileCOG(lat, lon int, l Layout, value func(lon, lat float64) float32) []byte {
	return dhmtest.COG(dhmtest.COGOptions{
		Width: l.W, Height: l.H, Tile: l.Block,
		Lon0: float64(lon), Lat0: float64(lat + 1), PixelLon: 1 / float64(l.W), PixelLat: 1 / float64(l.H),
		Value: value, Overview: true,
	})
}

var tilePathRe = regexp.MustCompile(`^/Copernicus_DSM_COG_10_N([0-9]{2})_00_E([0-9]{3})_00_DEM/Copernicus_DSM_COG_10_N[0-9]{2}_00_E[0-9]{3}_00_DEM\.tif$`)

// Synthetic is a Bucket.Generate that serves every tile path asked for at
// layout l. It answers for any tile, sea included: which tiles exist is
// the client's tile list's business, and the client never asks for one
// it does not list.
func Synthetic(l Layout, value func(lon, lat float64) float32) func(path string) []byte {
	return func(path string) []byte {
		m := tilePathRe.FindStringSubmatch(path)
		if m == nil {
			return nil
		}
		lat, _ := strconv.Atoi(m[1])
		lon, _ := strconv.Atoi(m[2])
		return TileCOG(lat, lon, l, value)
	}
}
