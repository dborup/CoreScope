package nodecard

import "time"

// asOf is the fixtures' "now": the footer and every relative time are
// computed against it, never against the wall clock.
var asOf = time.Date(2026, 11, 15, 13, 20, 41, 0, time.UTC)

const fxKey = "3f2a91c0d4e5b6a798c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3"

func ip(v int) *int                 { return &v }
func fp(v float64) *float64         { return &v }
func ago(d time.Duration) time.Time { return asOf.Add(-d) }

func fxRepeater() NodeCardInput {
	return NodeCardInput{
		PubKey: fxKey, Name: "DK_7800_Kjeldbjerg", Role: "repeater", Area: "Midtjylland",
		LastHeard: ago(3*time.Hour + 12*time.Minute),
		Relay:     &RelayStats{Relayed24h: ip(1234), Relayed1h: ip(56), AirtimePct24h: fp(3.24), AirtimePeakPct: fp(8.1)},
		Neighbors: ip(12), AsOf: asOf,
	}
}

func fxRoom() NodeCardInput {
	return NodeCardInput{
		PubKey: fxKey, Name: "Aarhus Room", Role: "room", Area: "Midtjylland",
		LastHeard: ago(4 * time.Minute),
		Relay:     &RelayStats{Relayed24h: ip(87), Relayed1h: ip(0), AirtimePct24h: fp(0.04), AirtimePeakPct: fp(0.6)},
		Neighbors: ip(3), AsOf: asOf,
	}
}

func fxSensor() NodeCardInput {
	return NodeCardInput{
		PubKey: fxKey, Name: "Weather Sensor Ry", Role: "sensor",
		LastHeard: ago(2 * 24 * time.Hour),
		// A sensor never shows a relay block, even when it is passed one.
		Relay:     &RelayStats{Relayed24h: ip(9)},
		Neighbors: ip(1), AsOf: asOf,
	}
}

func fxCompanion() NodeCardInput {
	in := fxSensor()
	in.Name, in.Role, in.Relay = "Phone", "companion", nil
	return in
}

// The #857 fields on the room and sensor fixtures. Rooms carry transported
// scopes like repeaters; a sensor has no relay block and no scopes row.
func fxRoomFull() NodeCardInput {
	in := fxRoom()
	in.FirstSeen = ago(36 * 24 * time.Hour)
	in.AvgHops = ip(2)
	in.OwnScope = sp("")
	in.TransportedScopes = []string{"#dk-aarhus"}
	in.FloodInterval = &AdvertInterval{Seconds: 12 * 3600, Confidence: "medium", Snapped: true}
	return in
}

func fxSensorFull() NodeCardInput {
	in := fxSensor()
	in.FirstSeen = ago(3*24*time.Hour + 5*time.Hour)
	in.AvgHops = ip(1)
	in.OwnScope = sp("*")
	// 300 min is past the settable 60–240 min (+10 %), so the estimator
	// leaves it unsnapped (snapAdvertInterval): the note is honest (#861).
	in.ZeroHopInterval = &AdvertInterval{Seconds: 300 * 60, Confidence: "high", Snapped: false}
	return in
}
