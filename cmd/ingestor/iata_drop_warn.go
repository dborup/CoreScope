package main

import (
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Throttled warning for observerIATAWhitelist drops (#110).
//
// Dropping in silence fails in the dangerous direction: a legitimate region
// missing from the allow-list loses all its traffic with nothing in the log.
// Logging every drop is not an option either (a foreign feed is thousands of
// messages a day), so each region is logged once and re-logged at most every
// IATAWarnInterval while it keeps arriving; a strict log-once would scroll
// out of any log window and leave an active drop looking healthy.
//
// The region is a topic segment the publisher controls, so the per-region
// state is strictly bounded: at most iataWarnMaxTracked keys of at most
// iataWarnMaxKeyLen bytes. Past the cap the drop is still logged, on one
// shared overflow throttle. Entries older than the interval are reclaimed
// when the table is full, so the first codes ever seen cannot own it.

const (
	// iataWarnMaxTracked bounds the per-region throttle table. Far above any
	// real deployment's region count, small enough to be harmless when a
	// hostile publisher sends a new region per message.
	iataWarnMaxTracked = 512
	// iataWarnMaxKeyLen caps the stored and logged region code. IATA codes
	// are three letters; longer segments are truncated (and may share a key).
	iataWarnMaxKeyLen = 32
	// defaultIATAWarnIntervalSec is the default re-log interval (6h).
	defaultIATAWarnIntervalSec = 6 * 60 * 60
	// maxIATAWarnIntervalSec caps the configured interval (24h). A larger
	// value buys nothing for a warning meant to stay visible, and a huge one
	// overflows time.Duration into a negative interval that logs every drop.
	maxIATAWarnIntervalSec = 24 * 60 * 60
)

// iataDropThrottle is the per-Config throttle state. The zero value is ready.
type iataDropThrottle struct {
	mu           sync.Mutex
	last         map[string]time.Time
	overflowLast time.Time
	// oldest is a lower bound on the oldest time in last: a sweep can free
	// nothing until it has expired, so a full table of fresh entries costs
	// O(1) per drop, not a full scan.
	oldest time.Time
	sweeps int // full-table sweeps, for tests
}

// IATAWarnInterval returns how often a dropped region is re-logged:
// iataWarnIntervalSec capped at 24 hours, or the default for 0 or less.
func (c *Config) IATAWarnInterval() time.Duration {
	if c == nil || c.IATAWarnIntervalSec <= 0 {
		return defaultIATAWarnIntervalSec * time.Second
	}
	if c.IATAWarnIntervalSec > maxIATAWarnIntervalSec {
		return maxIATAWarnIntervalSec * time.Second
	}
	return time.Duration(c.IATAWarnIntervalSec) * time.Second
}

// normalizeIATAForWarn is the key and the logged form of a region segment:
// trimmed, upper-cased and cut to iataWarnMaxKeyLen bytes on a rune boundary.
func normalizeIATAForWarn(iata string) string {
	code := strings.ToUpper(strings.TrimSpace(iata))
	if len(code) <= iataWarnMaxKeyLen {
		return code
	}
	cut := iataWarnMaxKeyLen
	for cut > 0 && !utf8.RuneStart(code[cut]) {
		cut--
	}
	return code[:cut]
}

// shouldWarn reports whether a drop of code (already normalized) should be
// logged at now, and whether it goes through the shared overflow throttle.
func (t *iataDropThrottle) shouldWarn(code string, now time.Time, interval time.Duration) (warn, overflow bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[code]; ok {
		if now.Sub(last) < interval {
			return false, false
		}
		t.last[code] = now
		return true, false
	}
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	if len(t.last) >= iataWarnMaxTracked && now.Sub(t.oldest) >= interval {
		t.sweeps++
		oldest := now
		for k, ts := range t.last {
			if now.Sub(ts) >= interval {
				delete(t.last, k)
			} else if ts.Before(oldest) {
				oldest = ts
			}
		}
		t.oldest = oldest
	}
	if len(t.last) >= iataWarnMaxTracked {
		if !t.overflowLast.IsZero() && now.Sub(t.overflowLast) < interval {
			return false, true
		}
		t.overflowLast = now
		return true, true
	}
	if len(t.last) == 0 || now.Before(t.oldest) {
		t.oldest = now
	}
	t.last[code] = now
	return true, false
}

// warnIATADrop logs a throttled warning for a message dropped by
// observerIATAWhitelist. The region is quoted (%q), so control characters in
// the publisher-controlled segment cannot break or forge log lines.
func (c *Config) warnIATADrop(tag, iata string, now time.Time) {
	if c == nil {
		return
	}
	code := normalizeIATAForWarn(iata)
	interval := c.IATAWarnInterval()
	warn, overflow := c.iataDropWarn.shouldWarn(code, now, interval)
	if !warn {
		return
	}
	if overflow {
		log.Printf("MQTT [%s] [region-filter] dropping region %q: not in observerIATAWhitelist; throttle table full (%d regions), further untracked regions suppressed for %s",
			tag, code, iataWarnMaxTracked, interval)
		return
	}
	log.Printf("MQTT [%s] [region-filter] dropping region %q: not in observerIATAWhitelist; further messages from this region suppressed for %s",
		tag, code, interval)
}
