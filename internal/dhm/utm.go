package dhm

import "math"

// #681 M2 — WGS84 ↔ ETRS89 / UTM zone 32N (EPSG:25832), the only CRS the
// DHM WCS serves.
//
// ETRS89 and WGS84 have drifted apart by roughly 0.9 m since 1989. Against
// the 0.4 m DHM grid, a 10 m sampling step and 20–60 m Fresnel radii that is
// irrelevant, so lat/lon from MeshCore adverts (WGS84) is used as ETRS89
// directly.
//
// The projection is the Krüger series to fourth order in n (Karney 2011,
// "Transverse Mercator with an accuracy of a few nanometers", eqs. 35–36),
// which is accurate to well under a millimetre within Denmark's
// 6.6° from the central meridian. The inverse turns conformal latitude back
// into geodetic latitude by fixed-point iteration, which is exact.

// transverseMercator is one Transverse Mercator projection on one
// ellipsoid. UTM32 is the only one the package uses; the parameters exist so
// tests can check the formulas against published examples on other
// ellipsoids.
type transverseMercator struct {
	k0, lon0, fe, fn float64 // scale, central meridian (rad), false easting/northing
	e                float64 // first eccentricity
	kA               float64 // k0 × rectifying radius A
	alpha, beta      [4]float64
	n0               float64 // northing of the natural origin before fn
}

func newTransverseMercator(a, invF, k0, lat0Deg, lon0Deg, fe, fn float64) *transverseMercator {
	f := 1 / invF
	n := f / (2 - f)
	n2, n3, n4 := n*n, n*n*n, n*n*n*n
	tm := &transverseMercator{
		k0:   k0,
		lon0: lon0Deg * math.Pi / 180,
		fe:   fe,
		fn:   fn,
		e:    math.Sqrt(f * (2 - f)),
		kA:   k0 * a / (1 + n) * (1 + n2/4 + n4/64),
		alpha: [4]float64{
			n/2 - 2*n2/3 + 5*n3/16 + 41*n4/180,
			13*n2/48 - 3*n3/5 + 557*n4/1440,
			61*n3/240 - 103*n4/140,
			49561 * n4 / 161280,
		},
		beta: [4]float64{
			n/2 - 2*n2/3 + 37*n3/96 - n4/360,
			n2/48 + n3/15 - 437*n4/1440,
			17*n3/480 - 37*n4/840,
			4397 * n4 / 161280,
		},
	}
	_, tm.n0 = tm.forwardRaw(lat0Deg*math.Pi/180, 0)
	return tm
}

// forwardRaw projects latitude phi and longitude dl relative to the central
// meridian (radians) without false origin.
func (tm *transverseMercator) forwardRaw(phi, dl float64) (x, y float64) {
	// t = tan of the conformal latitude.
	t := math.Sinh(math.Atanh(math.Sin(phi)) - tm.e*math.Atanh(tm.e*math.Sin(phi)))
	xi := math.Atan2(t, math.Cos(dl))
	eta := math.Atanh(math.Sin(dl) / math.Sqrt(1+t*t))
	x, y = eta, xi
	for j, a := range tm.alpha {
		k := 2 * float64(j+1)
		x += a * math.Cos(k*xi) * math.Sinh(k*eta)
		y += a * math.Sin(k*xi) * math.Cosh(k*eta)
	}
	return tm.kA * x, tm.kA * y
}

// forward projects latitude/longitude in degrees to easting/northing.
func (tm *transverseMercator) forward(latDeg, lonDeg float64) (e, n float64) {
	x, y := tm.forwardRaw(latDeg*math.Pi/180, lonDeg*math.Pi/180-tm.lon0)
	return tm.fe + x, tm.fn + y - tm.n0
}

// inverse unprojects easting/northing to latitude/longitude in degrees.
func (tm *transverseMercator) inverse(e, n float64) (latDeg, lonDeg float64) {
	xi := (n - tm.fn + tm.n0) / tm.kA
	eta := (e - tm.fe) / tm.kA
	xp, ep := xi, eta
	for j, b := range tm.beta {
		k := 2 * float64(j+1)
		xp -= b * math.Sin(k*xi) * math.Cosh(k*eta)
		ep -= b * math.Cos(k*xi) * math.Sinh(k*eta)
	}
	chi := math.Asin(math.Sin(xp) / math.Cosh(ep))
	// Geodetic from conformal latitude: the fixed point of
	// φ = 2·atan(tan(π/4 + χ/2) · ((1 + e·sinφ)/(1 − e·sinφ))^(e/2)) − π/2,
	// which converges to machine precision in a handful of steps.
	phi := chi
	for i := 0; i < 20; i++ {
		s := tm.e * math.Sin(phi)
		next := 2*math.Atan(math.Tan(math.Pi/4+chi/2)*math.Pow((1+s)/(1-s), tm.e/2)) - math.Pi/2
		if math.Abs(next-phi) < 1e-15 {
			phi = next
			break
		}
		phi = next
	}
	dl := math.Atan2(math.Sinh(ep), math.Cos(xp))
	return phi * 180 / math.Pi, (tm.lon0 + dl) * 180 / math.Pi
}

// utm32 is ETRS89 / UTM zone 32N (EPSG:25832): GRS80, central meridian
// 9° E, k0 = 0.9996, false easting 500 000 m, false northing 0.
var utm32 = newTransverseMercator(6378137, 298.257222101, 0.9996, 0, 9, 500000, 0)

// ToUTM32 projects a WGS84 (treated as ETRS89) latitude/longitude in
// degrees to EPSG:25832 easting/northing in metres.
func ToUTM32(lat, lon float64) (e, n float64) { return utm32.forward(lat, lon) }

// FromUTM32 is the inverse of ToUTM32.
func FromUTM32(e, n float64) (lat, lon float64) { return utm32.inverse(e, n) }
