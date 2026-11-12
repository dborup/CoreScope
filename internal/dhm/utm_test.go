package dhm

import (
	"math"
	"testing"
)

// #681 M2 — the EPSG:25832 projection against published control points.

// tmControl is one published Transverse Mercator point.
type tmControl struct{ lat, lon, x, y float64 }

// karneyTM are points from C. F. F. Karney, "Test data for the transverse
// Mercator projection" (2009, doi:10.5281/zenodo.32470, TMcoords.dat):
// WGS84, k0 = 0.9996, central meridian 0, no false origin, exact to 0.1 nm.
// The points were chosen to span Denmark's latitudes (54.4°–57.7° N) and
// its offsets from the UTM 32 central meridian (−1° to +6.6°, where +6.6°
// is Bornholm); x is negated for a western offset, which the projection's
// symmetry makes exact.
var karneyTM = []tmControl{
	{54.434432840308, 3.856377623194, 250083.4547802976174, 6038709.069266305645},
	{54.460106930766, 6.646935337563, 430570.2083068554164, 6055076.5039844791765},
	{54.514333461744, 2.664056056485, 172447.7365409540253, 6044014.5601575392288},
	{54.971413373124, .619375524124, 39648.4440291124595, 6091785.8252028847776},
	{55.753535712331, 3.471798114123, 217855.5460780085389, 6184108.1225681774287},
	{55.953495801345, .54862938926, 34257.6968084381878, 6201039.7176673973882},
	{56.091164741569, 6.02914488772, 374876.8960673754116, 6232620.003311624468},
	{56.177928637885, 2.655723054873, 164847.4357248308983, 6229057.6731173943061},
	{56.920232117969, 4.869110363634, 296276.3103349849527, 6319065.2883705954572},
	{57.058155920114, 3.00130233273, 182004.250940038539, 6327861.5723938387266},
	{57.300051282812, 6.113073497242, 368070.7697824580546, 6367335.308059842569},
	{57.68228672093, 1.06465201437, 63485.5902590520328, 6393837.5361613212161},
	{54.971413373124, -.619375524124, -39648.4440291124595, 6091785.8252028847776},
	{57.68228672093, -1.06465201437, -63485.5902590520328, 6393837.5361613212161},
}

// metresBetween is the planar distance in metres between two lat/lon points
// a few millimetres apart.
func metresBetween(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	dn := (lat2 - lat1) * math.Pi / 180 * r
	de := (lon2 - lon1) * math.Pi / 180 * r * math.Cos(lat1*math.Pi/180)
	return math.Hypot(dn, de)
}

func checkTM(t *testing.T, name string, tm *transverseMercator, p tmControl, tolM float64) {
	t.Helper()
	x, y := tm.forward(p.lat, p.lon)
	if d := math.Hypot(x-p.x, y-p.y); !(d <= tolM) {
		t.Errorf("%s forward (%.9f, %.9f): got %.4f %.4f, want %.4f %.4f (off %.4f m)", name, p.lat, p.lon, x, y, p.x, p.y, d)
	}
	lat, lon := tm.inverse(p.x, p.y)
	if d := metresBetween(lat, lon, p.lat, p.lon); !(d <= tolM) {
		t.Errorf("%s inverse (%.4f, %.4f): got %.10f %.10f, want %.10f %.10f (off %.4f m)", name, p.x, p.y, lat, lon, p.lat, p.lon, d)
	}
}

// The Krüger series on the WGS84 ellipsoid reproduces Karney's exact
// values to a millimetre in both directions over Denmark's span.
func TestTransverseMercatorKarneyPoints(t *testing.T) {
	wgs84TM := newTransverseMercator(6378137, 298.257223563, 0.9996, 0, 0, 0, 0)
	for _, p := range karneyTM {
		checkTM(t, "karney", wgs84TM, p, 0.001)
	}
}

// EPSG Guidance Note 7-2 §1.4.6.1 worked example: OSGB 1936 / British
// National Grid on Airy 1830, with a non-zero latitude of origin and a
// negative false northing. It is published to the centimetre, from the
// USGS series, so the bound is 2 cm (measured: 6 mm E, 8 mm N forward,
// 10 mm inverse).
func TestTransverseMercatorEPSGExample(t *testing.T) {
	bng := newTransverseMercator(6377563.396, 299.3249646, 0.9996012717, 49, -2, 400000, -100000)
	checkTM(t, "EPSG GN7-2", bng, tmControl{50.5, 0.5, 577274.99, 69740.50}, 0.02)
}

// The exported EPSG:25832 projection against the same published points,
// moved to zone 32 (central meridian 9° E, false easting 500 000 m). GRS80
// and WGS84 differ only in the 11th digit of the flattening, which moves a
// point by well under a millimetre here; the bound is one centimetre.
func TestUTM32PublishedPointsBothWays(t *testing.T) {
	for _, p := range karneyTM {
		e, n := ToUTM32(p.lat, p.lon+9)
		if d := math.Hypot(e-(p.x+500000), n-p.y); !(d <= 0.01) {
			t.Errorf("ToUTM32(%.9f, %.9f) = %.4f %.4f, want %.4f %.4f", p.lat, p.lon+9, e, n, p.x+500000, p.y)
		}
		lat, lon := FromUTM32(p.x+500000, p.y)
		if d := metresBetween(lat, lon, p.lat, p.lon+9); !(d <= 0.01) {
			t.Errorf("FromUTM32(%.4f, %.4f) = %.10f %.10f, want %.10f %.10f", p.x+500000, p.y, lat, lon, p.lat, p.lon+9)
		}
	}
	// On the central meridian the easting is exactly the false easting.
	if e, _ := ToUTM32(56, 9); math.Abs(e-500000) > 1e-6 {
		t.Errorf("central meridian easting %.9f", e)
	}
	// The equator at the central meridian is the false origin.
	if e, n := ToUTM32(0, 9); math.Abs(e-500000) > 1e-6 || math.Abs(n) > 1e-6 {
		t.Errorf("false origin %.9f %.9f", e, n)
	}
}

// Round trips stay under a millimetre at every corner of the DHM raster
// extent and across a lat/lon grid over the DHM coverage, both directions.
func TestUTM32RoundTrip(t *testing.T) {
	x := RasterExtent
	for _, c := range [][2]float64{{x[0], x[1]}, {x[0], x[3]}, {x[2], x[1]}, {x[2], x[3]}, {(x[0] + x[2]) / 2, (x[1] + x[3]) / 2}} {
		lat, lon := FromUTM32(c[0], c[1])
		if !Coverage.containsApprox(lat, lon, 0.2) {
			t.Errorf("corner %.0f %.0f → %.5f %.5f, far outside the DHM coverage", c[0], c[1], lat, lon)
		}
		e, n := ToUTM32(lat, lon)
		if d := math.Hypot(e-c[0], n-c[1]); !(d < 0.001) {
			t.Errorf("corner %.0f %.0f round trip off %.6f m", c[0], c[1], d)
		}
	}
	for lat := Coverage.South; lat <= Coverage.North; lat += 0.25 {
		for lon := Coverage.West; lon <= Coverage.East; lon += 0.25 {
			e, n := ToUTM32(lat, lon)
			lat2, lon2 := FromUTM32(e, n)
			if d := metresBetween(lat, lon, lat2, lon2); !(d < 0.001) {
				t.Errorf("%.4f %.4f round trip off %.6f m", lat, lon, d)
			}
		}
	}
}

func (b Bounds) containsApprox(lat, lon, tolDeg float64) bool {
	return lat >= b.South-tolDeg && lat <= b.North+tolDeg && lon >= b.West-tolDeg && lon <= b.East+tolDeg
}
