package main

import (
	"encoding/json"
	"math"

	"github.com/meshcore-analyzer/configkey"
)

// MomentsDisplayConfig is config.json moments.display (#969): the
// operator's defaults for the Live page's Moments viewer settings, the
// same keys as the customizer's `moments` section and
// LiveMoments.SETTINGS in public/live-moments.js. Only the keys the
// operator set are served; the page fills the rest with its built-ins,
// and a viewer's own customizer value or a ?moments= link still wins.
type MomentsDisplayConfig struct {
	Enabled        *bool `json:"enabled,omitempty"`
	VisibleCards   *int  `json:"visibleCards,omitempty"`
	DwellSeconds   *int  `json:"dwellSeconds,omitempty"`
	ShowNewNode    *bool `json:"showNewNode,omitempty"`
	ShowBackOnline *bool `json:"showBackOnline,omitempty"`
	ShowRecordLink *bool `json:"showRecordLink,omitempty"`
	ShowBusiest    *bool `json:"showBusiest,omitempty"`
}

// momentsDisplayField is one key of moments.display: a switch (flag) or a
// whole number in lo..hi (num). The order and the bounds are
// SETTINGS.KEYS and SETTINGS.LIMITS in public/live-moments.js
// (TestOperatorDisplay969_BoundsMatchTheClient pins them).
type momentsDisplayField struct {
	key    string
	flag   func(*MomentsDisplayConfig) **bool
	num    func(*MomentsDisplayConfig) **int
	lo, hi int
}

func momentsDisplayFields() []momentsDisplayField {
	return []momentsDisplayField{
		{key: "enabled", flag: func(d *MomentsDisplayConfig) **bool { return &d.Enabled }},
		{key: "visibleCards", num: func(d *MomentsDisplayConfig) **int { return &d.VisibleCards }, lo: 1, hi: 5},
		{key: "dwellSeconds", num: func(d *MomentsDisplayConfig) **int { return &d.DwellSeconds }, lo: 3, hi: 30},
		{key: "showNewNode", flag: func(d *MomentsDisplayConfig) **bool { return &d.ShowNewNode }},
		{key: "showBackOnline", flag: func(d *MomentsDisplayConfig) **bool { return &d.ShowBackOnline }},
		{key: "showRecordLink", flag: func(d *MomentsDisplayConfig) **bool { return &d.ShowRecordLink }},
		{key: "showBusiest", flag: func(d *MomentsDisplayConfig) **bool { return &d.ShowBusiest }},
	}
}

// readMomentsDisplay reads moments.display by its exact keys (#884). A
// number out of range is clamped, a value of the wrong kind is dropped,
// each with one log line. Nil when no key is usable.
func readMomentsDisplay(keys *configkey.Doc, logf func(string, ...interface{})) *MomentsDisplayConfig {
	if keys.Get("moments", "display") == nil {
		return nil
	}
	d := &MomentsDisplayConfig{}
	set := false
	for _, f := range momentsDisplayFields() {
		raw := keys.Get("moments", "display", f.key)
		if raw == nil {
			continue
		}
		if f.flag != nil {
			b := keys.Bool("moments", "display", f.key)
			if b == nil {
				logf("[config] moments.display.%s=%s is not true or false; ignored", f.key, raw)
				continue
			}
			*f.flag(d) = b
			set = true
			continue
		}
		var v float64
		if json.Unmarshal(raw, &v) != nil || v != math.Trunc(v) {
			logf("[config] moments.display.%s=%s is not a whole number; ignored", f.key, raw)
			continue
		}
		// Compared as a float, so a huge value never overflows an int.
		n := f.lo
		switch {
		case v < float64(f.lo):
			logf("[config] moments.display.%s=%s is outside %d-%d; using %d", f.key, raw, f.lo, f.hi, f.lo)
		case v > float64(f.hi):
			n = f.hi
			logf("[config] moments.display.%s=%s is outside %d-%d; using %d", f.key, raw, f.lo, f.hi, f.hi)
		default:
			n = int(v)
		}
		*f.num(d) = &n
		set = true
	}
	if !set {
		return nil
	}
	return d
}
