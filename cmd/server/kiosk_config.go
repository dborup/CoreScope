package main

import (
	"encoding/json"
	"strings"

	"github.com/meshcore-analyzer/configkey"
)

// #968 M1 — the operator kiosk preset: config.json "kiosk". It fills a
// #/live?kiosk=1 link that does not say otherwise (the link's own tour=,
// every= and theme= win) and never turns kiosk on. The browser receives it
// as the `kiosk` block of /api/config/client, resolved: every member is
// present with its default when unset. Absent block ⇒ not served.
//
// Bounds are public/live-kiosk.js's (LiveKiosk.SETTINGS); a value outside
// them logs one warning and takes the default.
const (
	kioskMaxSteps        = 12 // LiveKiosk.MAX_STEPS, the regionGroups cap
	kioskEveryMin        = 15
	kioskEveryMax        = 3600
	kioskEveryDefault    = 60
	kioskRemountMinH     = 1
	kioskRemountMaxH     = 24
	kioskRemountDefaultH = 6
	kioskThemeDefault    = "dark"
)

var (
	kioskThemes = []string{"dark", "light", "auto"}
	kioskTiles  = []string{"rate", "active", "records"} // the HUD order
)

// KioskConfig is the resolved kiosk preset, as served to the browser.
type KioskConfig struct {
	// Tour: area keys (/api/config/areas/polygons) for a link with no tour=.
	Tour []string `json:"tour"`
	// EverySeconds: seconds per tour step for a link with no every=.
	EverySeconds int `json:"everySeconds"`
	// Theme: dark, light or auto, for a link with no theme=.
	Theme string `json:"theme"`
	// Tiles: the big-number tiles shown, in HUD order.
	Tiles []string `json:"tiles"`
	// RemountHours: the in-place remount interval.
	RemountHours float64 `json:"remountHours"`
}

func kioskDefaultConfig() KioskConfig {
	return KioskConfig{
		Tour:         []string{},
		EverySeconds: kioskEveryDefault,
		Theme:        kioskThemeDefault,
		Tiles:        append([]string(nil), kioskTiles...),
		RemountHours: kioskRemountDefaultH,
	}
}

func kioskOneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// readKioskConfig reads the kiosk block by canonical keys only (#884).
// nil when the block is missing or not an object.
func readKioskConfig(keys *configkey.Doc, logf func(string, ...interface{})) *KioskConfig {
	raw := keys.Get("kiosk")
	if raw == nil {
		return nil
	}
	var obj struct{}
	if json.Unmarshal(raw, &obj) != nil {
		logf("[config] kiosk.(block) is not an object; ignored, using the default (no preset)")
		return nil
	}
	k := kioskDefaultConfig()
	warn := func(key, want, def string) {
		logf("[config] kiosk.%s is not %s; using the default %s", key, want, def)
	}

	if v := keys.Get("kiosk", "tour"); v != nil {
		var tour []string
		if json.Unmarshal(v, &tour) != nil {
			warn("tour", "a list of area keys", "(no tour)")
		} else {
			for _, s := range tour {
				if s = strings.TrimSpace(s); s != "" {
					k.Tour = append(k.Tour, s)
				}
			}
			if len(k.Tour) > kioskMaxSteps {
				logf("[config] kiosk.tour has %d steps, more than %d; using the default cap: the first %d", len(k.Tour), kioskMaxSteps, kioskMaxSteps)
				k.Tour = k.Tour[:kioskMaxSteps]
			}
		}
	}
	if v := keys.Get("kiosk", "everySeconds"); v != nil {
		var n int
		if json.Unmarshal(v, &n) != nil || n < kioskEveryMin || n > kioskEveryMax {
			warn("everySeconds", "a whole number of seconds in 15-3600", "60")
		} else {
			k.EverySeconds = n
		}
	}
	if v := keys.Get("kiosk", "theme"); v != nil {
		var s string
		if json.Unmarshal(v, &s) != nil || !kioskOneOf(s, kioskThemes) {
			warn("theme", `"dark", "light" or "auto"`, kioskThemeDefault)
		} else {
			k.Theme = s
		}
	}
	if v := keys.Get("kiosk", "tiles"); v != nil {
		var list []string
		if json.Unmarshal(v, &list) != nil {
			warn("tiles", `a list of "rate", "active", "records"`, "(all tiles)")
		} else {
			want := map[string]bool{}
			var unknown []string
			for _, s := range list {
				if kioskOneOf(s, kioskTiles) {
					want[s] = true
				} else {
					unknown = append(unknown, s)
				}
			}
			if len(unknown) > 0 {
				logf("[config] kiosk.tiles: unknown tile(s) %q dropped (known: rate, active, records); using the default (all tiles) if none is left", unknown)
			}
			if len(want) > 0 {
				k.Tiles = k.Tiles[:0]
				for _, s := range kioskTiles {
					if want[s] {
						k.Tiles = append(k.Tiles, s)
					}
				}
			}
		}
	}
	if v := keys.Get("kiosk", "remountHours"); v != nil {
		var h float64
		if json.Unmarshal(v, &h) != nil || h < kioskRemountMinH || h > kioskRemountMaxH {
			warn("remountHours", "a number of hours in 1-24", "6")
		} else {
			k.RemountHours = h
		}
	}
	return &k
}

// kioskClientConfig is the /api/config/client block, nil when unset.
func (c *Config) kioskClientConfig() *KioskConfig {
	if c == nil || c.Kiosk == nil {
		return nil
	}
	k := *c.Kiosk
	k.Tour = append([]string{}, k.Tour...)
	k.Tiles = append([]string{}, k.Tiles...)
	return &k
}
