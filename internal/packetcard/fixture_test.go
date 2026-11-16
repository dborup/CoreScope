package packetcard

import (
	"math"
	"time"
)

// firstHeard is the fixtures' first-heard time.
var firstHeard = time.Date(2026, 11, 15, 13, 20, 41, 0, time.UTC)

func fp(v float64) *float64 { return &v }
func at(lat, lon float64) *LatLon {
	return &LatLon{lat, lon}
}

// Positions around Aarhus and Silkeborg, as a mesh there advertises them.
var (
	pAarhus    = LatLon{56.1572, 10.2107}
	pSkejby    = LatLon{56.1966, 10.1700}
	pHinnerup  = LatLon{56.2667, 10.0667}
	pSilkeborg = LatLon{56.1697, 9.5451}
	pRanders   = LatLon{56.4607, 10.0364}
	pSkanderb  = LatLon{56.0337, 9.9307}
)

func midtjylland() Area {
	return Area{Ring: []LatLon{{56.60, 9.30}, {56.62, 10.60}, {56.30, 10.95}, {55.95, 10.40}, {55.90, 9.40}}}
}

func aarhusBox() Area { return Area{Ring: BoxRing(56.08, 56.24, 10.05, 10.30)} }

const fxObserver = "Aarhus-Obs"

// fxFlood is a channel message flooded through four relays to the first
// observer, with two more observers, over two touched areas.
func fxFlood() PacketCardInput {
	first := Branch{Hops: 4, Points: []LatLon{pSilkeborg, pHinnerup, pSkejby}, Observer: at(pAarhus.Lat, pAarhus.Lon),
		SecondsAfterFirst: fp(0), DistanceFromFirstKm: fp(0)}
	return PacketCardInput{
		PayloadType: 5, RouteType: 1,
		Branches: []Branch{
			{Hops: 6, Points: []LatLon{pSilkeborg, pHinnerup, pSkejby, pAarhus}, Observer: at(pSkanderb.Lat, pSkanderb.Lon),
				SecondsAfterFirst: fp(4.2), DistanceFromFirstKm: fp(25.31)},
			first,
			{Hops: 2, Points: []LatLon{pSilkeborg}, Observer: at(pRanders.Lat, pRanders.Lon),
				SecondsAfterFirst: fp(1.75), DistanceFromFirstKm: fp(38.04)},
		},
		First: &first, FirstObserverName: fxObserver,
		Areas:      []Area{midtjylland(), aarhusBox()},
		FirstHeard: firstHeard, AirtimeMs: fp(1234),
	}
}

// fxDirect is a direct message along a two-relay route.
func fxDirect() PacketCardInput {
	b := Branch{Hops: 2, Points: []LatLon{pSkejby, pHinnerup}, Observer: at(pRanders.Lat, pRanders.Lon), SecondsAfterFirst: fp(0)}
	return PacketCardInput{PayloadType: 2, RouteType: 2, Branches: []Branch{b}, First: &b, FirstObserverName: "Randers Obs",
		FirstHeard: firstHeard, AirtimeMs: fp(312.4)}
}

// fxFanout is a flood heard by n observers spread in a circle around
// the origin.
func fxFanout(n int) PacketCardInput {
	in := PacketCardInput{PayloadType: 5, RouteType: 1, FirstHeard: firstHeard, AirtimeMs: fp(8840)}
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		r := 0.15 + 0.05*float64(i%3)
		mid := LatLon{56.15 + r/2*math.Sin(a), 10.0 + r*math.Cos(a)}
		obs := LatLon{56.15 + r*math.Sin(a), 10.0 + 2*r*math.Cos(a)}
		in.Branches = append(in.Branches, Branch{Hops: 2 + i%5, Points: []LatLon{{56.15, 10.0}, mid}, Observer: &obs,
			SecondsAfterFirst: fp(float64(i) * 0.9), DistanceFromFirstKm: fp(float64(i) * 1.3)})
	}
	f := in.Branches[n/2]
	f.SecondsAfterFirst, f.DistanceFromFirstKm = fp(0), fp(0)
	in.First, in.FirstObserverName = &f, "Fan Obs 15"
	in.Areas = []Area{midtjylland()}
	return in
}

// fxAdvert is a repeater's advert.
func fxAdvert() PacketCardInput {
	b := Branch{Hops: 1, Points: []LatLon{pHinnerup}, Observer: at(pAarhus.Lat, pAarhus.Lon), SecondsAfterFirst: fp(0)}
	return PacketCardInput{PayloadType: 4, RouteType: 1, Branches: []Branch{b}, First: &b, FirstObserverName: fxObserver,
		AdvertName: "DK_7800_Kjeldbjerg", Areas: []Area{aarhusBox()}, FirstHeard: firstHeard, AirtimeMs: fp(96)}
}

// fxNoPositions is a packet whose visible stations have no position.
func fxNoPositions() PacketCardInput {
	in := fxFlood()
	for i := range in.Branches {
		in.Branches[i].Points, in.Branches[i].Observer = nil, nil
	}
	in.First.Points, in.First.Observer = nil, nil
	in.Areas = nil
	return in
}

// fxSinglePoint is a packet heard by one positioned observer, zero hops.
func fxSinglePoint() PacketCardInput {
	b := Branch{Hops: 0, Observer: at(pAarhus.Lat, pAarhus.Lon), SecondsAfterFirst: fp(0)}
	return PacketCardInput{PayloadType: 4, RouteType: 1, Branches: []Branch{b}, First: &b, FirstObserverName: fxObserver,
		AdvertName: "Aarhus Rpt", FirstHeard: firstHeard}
}

// fxHiddenFirst is fxFlood after the filter hid the first observation:
// First is nil, and the spread and distances measured from it are gone.
// FirstObserverName is still set, as a careless caller might leave it.
func fxHiddenFirst() PacketCardInput {
	in := fxFlood()
	in.Branches = in.Branches[:1]
	in.Branches = append(in.Branches, fxFlood().Branches[2])
	in.First = nil
	in.AirtimeMs = nil
	return in
}
