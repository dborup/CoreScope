package main

import (
	"database/sql"
	"math"
	"strings"
	"time"
)

// Observer credit (#918): the server-side health buckets (GetNetworkStatus
// and the area density) apply the same rule as the client's
// public/roles.js getNodeHealth (#902). A node whose public key is also an
// observer that reported within healthThresholds.observerOnlineMinutes is
// running right now, so it counts as active whatever its role. The rule is
// shared here so both computations cannot drift; the client/server parity
// test runs test-fixtures/observer-credit-parity.json through both.
const (
	// observerReportToleranceMs and observerFutureToleranceMs match
	// OBSERVER_REPORT_TOLERANCE_MS and NODE_FRESHNESS_FUTURE_TOLERANCE_MS
	// in public/roles.js.
	observerReportToleranceMs = 30000
	observerFutureToleranceMs = 30000
)

// ObserverReports maps a normalised (lowercase, full 64-hex) observer id to
// the unix-ms time of its last status report (observers.last_seen).
type ObserverReports map[string]int64

// normalizeObserverKey mirrors MyMeshObservers.normalizeId: only a full
// 64-character hex key is an identity; prefixes never match.
func normalizeObserverKey(id string) (string, bool) {
	k := strings.ToLower(strings.TrimSpace(id))
	if len(k) != 64 {
		return "", false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return k, true
}

// add records one observer report; ids or timestamps that do not parse are
// ignored (no credit can come from them).
func (o ObserverReports) add(id, lastSeen string) {
	k, ok := normalizeObserverKey(id)
	if !ok {
		return
	}
	if ms, ok := parseHealthTimestamp(sql.NullString{String: lastSeen, Valid: true}); ok {
		o[k] = ms
	}
}

// LoadObserverReports reads the observer roster once, for one computation.
// It is the roster /api/observers serves (GetListedObservers: no
// soft-deleted rows, no blacklisted ids), so the server credits exactly the
// observers the client can index.
func (db *DB) LoadObserverReports(blacklisted func(id string) bool) (ObserverReports, error) {
	observers, err := db.GetListedObservers(blacklisted)
	if err != nil {
		return nil, err
	}
	out := make(ObserverReports, len(observers))
	for _, o := range observers {
		if o.LastSeen != nil {
			out.add(o.ID, *o.LastSeen)
		}
	}
	return out, nil
}

// parseHealthTimestamp parses a nodes/observers last_seen in the two
// formats the database holds.
func parseHealthTimestamp(s sql.NullString) (int64, bool) {
	if !s.Valid {
		return 0, false
	}
	if t, err := time.Parse(time.RFC3339, s.String); err == nil {
		return t.UnixMilli(), true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s.String); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

// hasObserverCredit reports whether pubkey is an observer whose last report
// is recent enough (same window as getObserverReportCredit in roles.js).
func (o ObserverReports) hasObserverCredit(pubkey string, nowMs int64, th HealthThresholds) bool {
	k, ok := normalizeObserverKey(pubkey)
	if !ok {
		return false
	}
	at, found := o[k]
	if !found || at > nowMs+observerFutureToleranceMs {
		return false
	}
	onlineMs := int64(th.ObserverOnlineMinutes) * 60000
	if onlineMs <= 0 {
		onlineMs = 3600000
	}
	return nowMs-at < onlineMs+observerReportToleranceMs
}

// classifyNodeHealth returns "active", "degraded" or "silent" for one node:
// its own last_seen against the role thresholds, lifted to active by an
// observer report. The one place both health buckets classify a node.
func classifyNodeHealth(role, pubkey string, lastSeen sql.NullString, nowMs int64, th HealthThresholds, obs ObserverReports) string {
	age := int64(math.MaxInt64)
	if ms, ok := parseHealthTimestamp(lastSeen); ok {
		age = nowMs - ms
	}
	degradedMs, silentMs := th.GetHealthMs(role)
	if age < int64(degradedMs) {
		return "active"
	}
	if obs.hasObserverCredit(pubkey, nowMs, th) {
		return "active"
	}
	if age < int64(silentMs) {
		return "degraded"
	}
	return "silent"
}
