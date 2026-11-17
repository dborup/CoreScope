package main

// #872 — circuit breaker in front of Mastedatabasen.
//
// When the WFS is down or hangs, every uncached cell used to wait up to
// mastsDeadline for one of the upstream slots, and each try timed out
// on its own, so viewers saw "Loading antenna sites…" for minutes and the
// log filled with timeouts. The breaker stops that:
//
//   - closed: fetches go upstream. breakerFailures consecutive "down"
//     outcomes (a timeout, a transport error or a 5xx; see
//     mastsOutcomeOf) open it. A success resets the count; any other
//     answer (a 4xx, a malformed page) neither counts nor resets.
//   - open: for breakerCooldownSec no fetch goes upstream. An uncached
//     cell answers at once with the distinct "source down" 503 and
//     Retry-After = the remaining cool-down, before the per-IP charge,
//     admission or an upstream slot. Cached cells are still served.
//   - half-open: after the cool-down exactly one request is let through as
//     the probe. Success closes the breaker, a down outcome re-opens it for
//     a full cool-down, anything else (the client left, no slot) frees the
//     probe for the next request. While the probe runs, others are refused
//     with Retry-After mastsBusyRetry.
//
// The state is a few scalars under one mutex: no goroutine, no per-request
// allocation, nothing that grows. Each state change logs one line.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

const (
	// mastsBreakerFailuresDefault consecutive down outcomes open the
	// breaker (map.masts.breakerFailures, 1-20).
	mastsBreakerFailuresDefault = 3
	mastsBreakerFailuresMax     = 20
	// mastsBreakerCooldownDefault is how long an open breaker keeps
	// upstream untouched (map.masts.breakerCooldownSec, 30-3600 s).
	mastsBreakerCooldownDefault = 5 * time.Minute
	mastsBreakerCooldownMinSec  = 30
	mastsBreakerCooldownMaxSec  = 3600

	// mastsSourceHeader marks the "source down" 503 so the browser can
	// tell it from a busy 503 without parsing the body.
	mastsSourceHeader = "X-CoreScope-Masts-Source"
)

// errMastsSourceDown is the body of the breaker's 503.
var errMastsSourceDown = errors.New("antenna data source unavailable")

// mastsBreakerSettings resolves map.masts.breakerFailures and
// breakerCooldownSec; out of range warns and takes the default.
func mastsBreakerSettings(c MastsConfig, logf func(string, ...interface{})) (int, time.Duration) {
	k := configKnobs{tag: "[masts]", block: "map.masts", logf: logf}
	n := k.integer("breakerFailures", c.BreakerFailures, mastsBreakerFailuresDefault, 1, mastsBreakerFailuresMax)
	s := k.integer("breakerCooldownSec", c.BreakerCooldownSec, int(mastsBreakerCooldownDefault/time.Second),
		mastsBreakerCooldownMinSec, mastsBreakerCooldownMaxSec)
	return n, time.Duration(s) * time.Second
}

// mastsOutcome is what one fetch says about upstream's health.
type mastsOutcome int

const (
	mastsOutcomeNeutral mastsOutcome = iota // says nothing: client left, no slot, a 4xx, a bad page
	mastsOutcomeOK                          // upstream answered a well-formed page
	mastsOutcomeDown                        // timeout, transport error or 5xx
)

// mastsOutcomeOf classifies a fetch error (nil is OK).
func mastsOutcomeOf(err error) mastsOutcome {
	if err == nil {
		return mastsOutcomeOK
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return mastsOutcomeDown
	}
	switch r := dhm.FailureReason(err); {
	case r == "timeout", r == "transport", strings.HasPrefix(r, "status 5"):
		return mastsOutcomeDown
	}
	return mastsOutcomeNeutral
}

type mastsBreaker struct {
	threshold int
	cooldown  time.Duration

	mu        sync.Mutex
	fails     int       // consecutive down outcomes while closed
	openUntil time.Time // zero: closed
	probing   bool      // half-open and the probe is out
}

func newMastsBreaker(threshold int, cooldown time.Duration) *mastsBreaker {
	return &mastsBreaker{threshold: threshold, cooldown: cooldown}
}

// admit says whether a fetch may go upstream now. probe is set for the one
// request let through after a cool-down; its caller must call endProbe.
// When refused, retry is how long to wait.
func (b *mastsBreaker) admit(now time.Time) (probe bool, retry time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return false, 0, true
	}
	if now.Before(b.openUntil) {
		return false, b.openUntil.Sub(now), false
	}
	if b.probing {
		return false, mastsBusyRetry, false
	}
	b.probing = true
	return true, 0, true
}

// blocked reports whether a non-probe fetch must not go upstream: an
// admitted fetch that waited for a slot while the breaker opened.
func (b *mastsBreaker) blocked() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.openUntil.IsZero()
}

// endProbe frees the probe. If its fetch said nothing (neutral), the
// breaker stays half-open and the next request probes.
func (b *mastsBreaker) endProbe() {
	b.mu.Lock()
	b.probing = false
	b.mu.Unlock()
}

// record notes one fetch outcome and returns the state change to log, if
// any: "open", "reopen" or "closed".
func (b *mastsBreaker) record(now time.Time, o mastsOutcome) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	open := !b.openUntil.IsZero()
	switch o {
	case mastsOutcomeOK:
		b.fails = 0
		if open {
			b.openUntil = time.Time{}
			return "closed"
		}
	case mastsOutcomeDown:
		if open {
			// Only the probe's failure (after the cool-down) re-opens; a
			// late failure of a fetch started before opening changes
			// nothing.
			if !now.Before(b.openUntil) {
				b.openUntil = now.Add(b.cooldown)
				return "reopen"
			}
			return ""
		}
		b.fails++
		if b.fails >= b.threshold {
			b.fails = 0
			b.openUntil = now.Add(b.cooldown)
			return "open"
		}
	}
	return ""
}
