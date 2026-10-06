package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Client-RX source allowlist (#265): startup validation and the throttled,
// bounded warning for coverage dropped because it arrived on a source that is
// not in clientRxCoverage.sources.
//
// Dropping in silence fails in the dangerous direction — an operator who
// mistypes a source name would lose all coverage with nothing in the log — but
// a line per message is not an option either: an unlisted broker publishing
// meshcore/client/# produces mesh-rate traffic. So each source is logged on its
// first drop, re-logged at most every clientRxSourceWarnInterval while drops
// continue, and never more than clientRxSourceWarnMax times for the lifetime of
// the process (the last line says so). Mirrors the observerIATAWhitelist
// warning in iata_drop_warn.go; the throttle table needs no size cap here
// because the key is a configured source name, not a publisher-controlled
// topic segment.

const (
	// clientRxSourceWarnInterval is how long a source stays quiet after a
	// logged drop.
	clientRxSourceWarnInterval = 10 * time.Minute
	// clientRxSourceWarnMax caps how many lines one source may ever emit, so a
	// permanently misconfigured deployment cannot fill the log over weeks.
	clientRxSourceWarnMax = 10
)

// clientRxSourceDropThrottle is the per-source throttle state. The zero value
// is ready for use.
type clientRxSourceDropThrottle struct {
	mu    sync.Mutex
	state map[string]*clientRxSourceDropEntry
}

type clientRxSourceDropEntry struct {
	last  time.Time // when this source was last logged
	lines int       // lines emitted for this source so far
}

// shouldWarn reports whether a drop from source name should be logged at now,
// and whether this is the final line allowed for that source.
func (t *clientRxSourceDropThrottle) shouldWarn(name string, now time.Time) (warn, final bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		t.state = make(map[string]*clientRxSourceDropEntry)
	}
	e := t.state[key]
	if e == nil {
		e = &clientRxSourceDropEntry{}
		t.state[key] = e
	}
	if e.lines >= clientRxSourceWarnMax {
		return false, false
	}
	if e.lines > 0 && now.Sub(e.last) < clientRxSourceWarnInterval {
		return false, false
	}
	e.lines++
	e.last = now
	return true, e.lines == clientRxSourceWarnMax
}

// warnClientRxSourceDrop logs a throttled warning for a client-namespace
// message dropped because its MQTT source is not in clientRxCoverage.sources.
// The source name is quoted (%q) so an odd config value cannot forge log lines.
func (c *Config) warnClientRxSourceDrop(tag, name string, now time.Time) {
	if c == nil {
		return
	}
	warn, final := c.clientRxSrcDrop.shouldWarn(name, now)
	if !warn {
		return
	}
	if final {
		log.Printf("MQTT [%s] [client-rx] dropping client coverage: source %q not in clientRxCoverage.sources; line cap reached (%d), further drops from this source are silent",
			tag, name, clientRxSourceWarnMax)
		return
	}
	log.Printf("MQTT [%s] [client-rx] dropping client coverage: source %q not in clientRxCoverage.sources; further drops from this source suppressed for %s",
		tag, name, clientRxSourceWarnInterval)
}

// checkClientRxSources reports, once at startup, every clientRxCoverage.sources
// entry that matches no configured mqttSources[].name — an allowlist of names
// that can never match would silently drop all coverage. It also logs the
// effective restriction so the gate is visible in the boot log, and warns when
// one entry matches more than one configured source (#278). Returns the
// unknown names (nil when there is no allowlist or every name matches).
func checkClientRxSources(cfg *Config, sources []MQTTSource) []string {
	allow := cfg.ClientRxCoverageSources()
	if len(allow) == 0 {
		return nil
	}
	var unknown, ambiguous []string
	for _, want := range allow {
		var matched []string
		for _, src := range sources {
			if strings.EqualFold(strings.TrimSpace(src.Name), want) {
				matched = append(matched, fmt.Sprintf("%q", src.Name))
			}
		}
		switch {
		case len(matched) == 0:
			unknown = append(unknown, want)
		case len(matched) > 1:
			// mqttSources[].name is not required to be unique and matching is
			// case-insensitive, so one entry can admit several brokers (#278).
			ambiguous = append(ambiguous, fmt.Sprintf("%s matches %d sources (%s)", want, len(matched), strings.Join(matched, ", ")))
		}
	}
	state := ""
	if !cfg.ClientRxCoverageEnabled() {
		// The allowlist is inert while the feature is off; say so rather than
		// implying coverage is being ingested from the listed sources.
		state = " (clientRxCoverage.enabled is false, so no coverage is ingested at all)"
	}
	log.Printf("[client-rx] coverage restricted to %d MQTT source(s): %s%s", len(allow), strings.Join(allow, ", "), state)
	if len(unknown) > 0 {
		log.Printf("[client-rx] WARNING: %d clientRxCoverage.sources name(s) match no configured mqttSources[].name: %s — coverage will never be accepted for those names; check the spelling",
			len(unknown), strings.Join(unknown, ", "))
	}
	if len(ambiguous) > 0 {
		log.Printf("[client-rx] WARNING: clientRxCoverage.sources name(s) match more than one configured mqttSources[].name (case-insensitive): %s — coverage is accepted from every one of them; give each source a unique name",
			strings.Join(ambiguous, "; "))
	}
	return unknown
}
