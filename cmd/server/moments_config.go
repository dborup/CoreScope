package main

import (
	"log"
	"time"
)

// MomentsConfig is the config.json "moments" block (#881). Off by default.
// Zero or missing values take the defaults below; values outside the valid
// range are clamped, with one log line each at load.
type MomentsConfig struct {
	Enabled bool `json:"enabled"`
	// BackOnlineHours: a repeater silent at least this long is back online
	// when heard again. Default 24 (2 × the firmware's 12 h flood-advert
	// default), range 1-720.
	BackOnlineHours float64 `json:"backOnlineHours,omitempty"`
	// RecordWindowHours: the rolling window for "longest direct link".
	// Default 24, range 1-168.
	RecordWindowHours float64 `json:"recordWindowHours,omitempty"`
	// RecordMinKm: links shorter than this are never a record. Default 5,
	// range 0.1-1000.
	RecordMinKm float64 `json:"recordMinKm,omitempty"`
	// BusyWindowMinutes / BusyMinRelays: busiest repeater = most distinct
	// non-advert relays in the last BusyWindowMinutes, at least
	// BusyMinRelays. Defaults 10 and 20; ranges 1-1440 and 1-1000000.
	BusyWindowMinutes int `json:"busyWindowMinutes,omitempty"`
	BusyMinRelays     int `json:"busyMinRelays,omitempty"`
	// MaxPerMinute: global token-bucket rate across all kinds (burst 3).
	// Default 6, range 1-60.
	MaxPerMinute int `json:"maxPerMinute,omitempty"`
	// HistorySize: moments kept for GET /api/moments. Default 50, range
	// 1-500.
	HistorySize int `json:"historySize,omitempty"`
	// Display is moments.display (#969): the operator's defaults for the
	// Live page's Moments viewer settings. Read only by its exact keys
	// (#884) in readMomentsDisplay, never by the struct decode.
	Display *MomentsDisplayConfig `json:"-"`
}

// momentsSettings is MomentsConfig resolved: defaults applied, ranges
// clamped, durations typed.
type momentsSettings struct {
	enabled       bool
	backOnline    time.Duration
	recordWindow  time.Duration
	recordMinKm   float64
	busyWindow    time.Duration
	busyMinRelays int
	maxPerMinute  int
	historySize   int
}

func clampMomentF(name string, v, def, lo, hi float64, logf func(string, ...interface{})) float64 {
	if v == 0 {
		return def
	}
	if v < lo || v > hi {
		c := v
		if c < lo {
			c = lo
		} else {
			c = hi
		}
		if logf != nil {
			logf("[config] moments.%s=%v is outside %v-%v; using %v", name, v, lo, hi, c)
		}
		return c
	}
	return v
}

func clampMomentI(name string, v, def, lo, hi int, logf func(string, ...interface{})) int {
	return int(clampMomentF(name, float64(v), float64(def), float64(lo), float64(hi), logf))
}

// normalized returns m with defaults and clamps applied, logging each
// clamp through logf (nil: silent).
func (m MomentsConfig) normalized(logf func(string, ...interface{})) MomentsConfig {
	m.BackOnlineHours = clampMomentF("backOnlineHours", m.BackOnlineHours, 24, 1, 720, logf)
	m.RecordWindowHours = clampMomentF("recordWindowHours", m.RecordWindowHours, 24, 1, 168, logf)
	m.RecordMinKm = clampMomentF("recordMinKm", m.RecordMinKm, 5, 0.1, 1000, logf)
	m.BusyWindowMinutes = clampMomentI("busyWindowMinutes", m.BusyWindowMinutes, 10, 1, 1440, logf)
	m.BusyMinRelays = clampMomentI("busyMinRelays", m.BusyMinRelays, 20, 1, 1000000, logf)
	m.MaxPerMinute = clampMomentI("maxPerMinute", m.MaxPerMinute, 6, 1, 60, logf)
	m.HistorySize = clampMomentI("historySize", m.HistorySize, 50, 1, 500, logf)
	return m
}

// normalizeMoments clamps the moments block in place at load.
func (c *Config) normalizeMoments() {
	if c.Moments != nil {
		*c.Moments = c.Moments.normalized(log.Printf)
	}
}

// MomentsEnabled reports moments.enabled; missing block ⇒ off.
func (c *Config) MomentsEnabled() bool {
	return c != nil && c.Moments != nil && c.Moments.Enabled
}

// momentsSettings resolves the moments block.
func (c *Config) momentsSettings() momentsSettings {
	var m MomentsConfig
	if c != nil && c.Moments != nil {
		m = *c.Moments
	}
	m = m.normalized(nil)
	return momentsSettings{
		enabled:       c.MomentsEnabled(),
		backOnline:    time.Duration(m.BackOnlineHours * float64(time.Hour)),
		recordWindow:  time.Duration(m.RecordWindowHours * float64(time.Hour)),
		recordMinKm:   m.RecordMinKm,
		busyWindow:    time.Duration(m.BusyWindowMinutes) * time.Minute,
		busyMinRelays: m.BusyMinRelays,
		maxPerMinute:  m.MaxPerMinute,
		historySize:   m.HistorySize,
	}
}

// MomentsClientConfig is the read-only moments block of
// /api/config/client: whether the server emits moments and how many it
// keeps. Omitted when moments are off.
type MomentsClientConfig struct {
	Enabled     bool                  `json:"enabled"`
	HistorySize int                   `json:"historySize"`
	Display     *MomentsDisplayConfig `json:"display,omitempty"`
}

// momentsClientConfig is the /api/config/client block, nil when off.
func (c *Config) momentsClientConfig() *MomentsClientConfig {
	if !c.MomentsEnabled() {
		return nil
	}
	return &MomentsClientConfig{Enabled: true, HistorySize: c.momentsSettings().historySize, Display: c.Moments.Display}
}
