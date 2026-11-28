package main

import "time"

// lastHeardWindow is one entry of the Map page's "Last Heard" menu: the
// `lastHeard` query key GetNodes accepts and the window it filters on (#662).
type lastHeardWindow struct {
	Key string
	Dur time.Duration
}

// lastHeardWindows is the single key → window table behind GetNodes's
// `lastHeard` filter. Keep public/map.js's #mcLastHeard options in sync
// (test-map-last-heard-options.js asserts every option is a key here).
// Hardcoded for now; exposing it in the customizer is a follow-up
// (AGENTS.md rule 8).
var lastHeardWindows = []lastHeardWindow{
	{"1h", time.Hour},
	{"3h", 3 * time.Hour},
	{"6h", 6 * time.Hour},
	{"12h", 12 * time.Hour},
	{"24h", 24 * time.Hour},
	{"2d", 48 * time.Hour},
	{"3d", 72 * time.Hour},
	{"4d", 96 * time.Hour},
	{"5d", 120 * time.Hour},
	{"6d", 144 * time.Hour},
	{"7d", 168 * time.Hour},
	{"30d", 720 * time.Hour},
}

// lastHeardDuration returns the window for key; ok is false for an unknown
// key, which means "no filter".
func lastHeardDuration(key string) (time.Duration, bool) {
	for _, w := range lastHeardWindows {
		if w.Key == key {
			return w.Dur, true
		}
	}
	return 0, false
}
