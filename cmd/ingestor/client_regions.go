package main

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/meshcore-analyzer/regions"
)

// Declared-region reports (#126): a mobile companion (the collector) asks a
// repeater in direct range which regions it lets flood and relays the answer
// on meshcore/client/{collector}/regions. docs/client-regions.md is the
// protocol; this file implements it and nothing more.

// clientRegionsTimeLayout stores reported_at and ingested_at as UTC with
// millisecond precision, so text comparison (ORDER BY, the retention cutoff)
// is chronological.
const clientRegionsTimeLayout = "2006-01-02T15:04:05.000Z"

const (
	// maxDeclaredRegionNameBytes: RegionEntry.name is char[31] in the firmware
	// (src/helpers/RegionMap.h), so a name holds at most 30 bytes.
	maxDeclaredRegionNameBytes = 30
	// maxDeclaredRegionsCSVBytes: handleAnonRegionsReq gives exportNamesTo
	// sizeof(reply_data) - 12 = MAX_PACKET_PAYLOAD - 12 bytes for the list.
	maxDeclaredRegionsCSVBytes = maxPacketPayload - 12
	// maxDeclaredRegionEntries: the most one-byte names that fit, comma-separated.
	maxDeclaredRegionEntries = maxDeclaredRegionsCSVBytes / 2
	// maxDeclaredPosAccM: a fix coarser than this cannot place a direct-range
	// exchange, so the whole position is dropped.
	maxDeclaredPosAccM = 10000.0
)

// ClientDeclaredRegions is one accepted report: one repeater's answer, as one
// collector relayed it at one reported time. Never per-target state.
type ClientDeclaredRegions struct {
	Collector     string   // topic key (ACL-bound), 64 hex
	Target        string   // the repeater that answered, 64 hex
	Regions       []string // normalised; empty = "answered, floods nothing"
	Truncated     bool     // the app's hint, as sent
	ReportedAt    string   // payload timestamp (collector's clock), clientRegionsTimeLayout
	IngestedAt    string   // ingestor clock at acceptance, clientRegionsTimeLayout
	RepeaterClock *int64   // repeater RTC seconds as reported; nil = absent/invalid
	Lat, Lon      *float64 // collector position; nil = none/rejected
	PosAccM       *float64 // only ever set together with Lat/Lon
	Source        string   // mqttSources[].name that delivered it
}

// handleClientRegions validates and stores one report. collector is the raw
// topic segment. The caller has already applied the gate, the observer
// blacklist and the source allowlist.
func handleClientRegions(store *Store, cfg *Config, tag, source, collector string, msg map[string]interface{}, now time.Time) {
	maxAge := time.Duration(cfg.ClientRegionsDays()) * 24 * time.Hour
	r, reason := parseClientRegionsReport(collector, msg, now, maxAge)
	if r == nil {
		log.Printf("MQTT [%s] regions from %.8q dropped: %s", tag, collector, reason)
		return
	}
	r.Source = source
	if _, err := store.InsertClientDeclaredRegions(r); err != nil {
		log.Printf("MQTT [%s] client_declared_regions insert: %v", tag, err)
	}
}

// parseClientRegionsReport applies the protocol to one decoded payload.
// It returns the report, or nil and the reason the whole message is dropped.
// An invalid position or repeater_clock only drops that field. now is the
// ingest clock; maxAge is the retention window, past which a report is
// expired.
func parseClientRegionsReport(collector string, msg map[string]interface{}, now time.Time, maxAge time.Duration) (*ClientDeclaredRegions, string) {
	// Identity is the topic key, never a payload field, and a full key so
	// that admin delete-client-rx (exact 64 hex) can always reach the rows.
	collector = strings.ToLower(strings.TrimSpace(collector))
	if !clientRxPubkeyRe.MatchString(collector) {
		return nil, "collector is not a 64-hex key"
	}
	target, _ := msg["target"].(string)
	target = strings.ToLower(strings.TrimSpace(target))
	if !clientRxPubkeyRe.MatchString(target) {
		return nil, "target is not a 64-hex key"
	}
	rawList, ok := msg["regions"].([]interface{})
	if !ok {
		return nil, "regions missing or not an array"
	}
	list, reason := normalizeDeclaredRegions(rawList)
	if reason != "" {
		return nil, reason
	}
	ts, _ := msg["timestamp"].(string)
	reported, ok := parseReportTS(ts)
	if !ok {
		return nil, "timestamp missing or not RFC3339 with a zone"
	}
	if reported.After(now.Add(maxReportFutureSkew)) {
		return nil, "timestamp too far in the future"
	}
	if reported.Before(now.Add(-maxAge)) {
		return nil, "timestamp older than the retention window"
	}
	truncated, _ := msg["truncated"].(bool)
	lat, lon, acc := declaredRegionsPosition(msg["gps"])
	return &ClientDeclaredRegions{
		Collector:     collector,
		Target:        target,
		Regions:       list,
		Truncated:     truncated,
		ReportedAt:    reported.UTC().Format(clientRegionsTimeLayout),
		IngestedAt:    now.UTC().Format(clientRegionsTimeLayout),
		RepeaterClock: declaredRegionsRepeaterClock(msg["repeater_clock"]),
		Lat:           lat,
		Lon:           lon,
		PosAccM:       acc,
	}, ""
}

// normalizeDeclaredRegions turns the payload's region list into the stored
// form, or returns why the list is malformed. NUL padding (the reply is
// block-cipher padded) and surrounding whitespace are trimmed and
// padding-only entries skipped. "*" and private "$name" regions stay as
// they are, and any other name gets the "#" convention of
// regions.Normalize. Case is kept (the firmware matches with strcmp), and
// duplicates go, first order kept. An empty result is a valid answer.
func normalizeDeclaredRegions(raw []interface{}) ([]string, string) {
	if len(raw) > maxDeclaredRegionEntries {
		return nil, "too many regions"
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	csvBytes, names := 0, 0
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, "non-string region entry"
		}
		s = strings.TrimSpace(strings.TrimRight(s, "\x00\t\n\v\f\r "))
		if s == "" {
			continue
		}
		bare := strings.TrimPrefix(s, "#")
		if bare == "" || !utf8.ValidString(bare) || len(bare) > maxDeclaredRegionNameBytes {
			return nil, "region name empty, invalid UTF-8 or over 30 bytes"
		}
		if strings.IndexFunc(bare, func(r rune) bool { return r == ',' || unicode.IsControl(r) }) >= 0 {
			return nil, "region name contains a comma or control character"
		}
		csvBytes += len(bare)
		names++
		name := bare
		if bare != "*" && !strings.HasPrefix(bare, "$") {
			name, _ = regions.Normalize(bare)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if names > 0 && csvBytes+names-1 > maxDeclaredRegionsCSVBytes {
		return nil, "region list longer than a reply can carry"
	}
	return out, ""
}

// declaredRegionsPosition validates the optional gps object. It returns no
// position for anything invalid, and an accuracy only together with the
// position it qualifies.
func declaredRegionsPosition(v interface{}) (lat, lon, acc *float64) {
	gps, ok := v.(map[string]interface{})
	if !ok {
		return nil, nil, nil
	}
	la, laOK := gps["lat"].(float64)
	lo, loOK := gps["lon"].(float64)
	if !laOK || !loOK || la < -90 || la > 90 || lo < -180 || lo > 180 || (la == 0 && lo == 0) {
		return nil, nil, nil
	}
	if raw, present := gps["acc_m"]; present {
		a, ok := raw.(float64)
		if !ok || math.IsNaN(a) || a < 0 || a > maxDeclaredPosAccM {
			return nil, nil, nil
		}
		acc = &a
	}
	return &la, &lo, acc
}

// declaredRegionsRepeaterClock keeps the repeater's RTC reading (uint32
// seconds) as reported. Anything else, absent included, is nil, never 0.
func declaredRegionsRepeaterClock(v interface{}) *int64 {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < 0 || f > math.MaxUint32 {
		return nil
	}
	n := int64(f)
	return &n
}

// InsertClientDeclaredRegions stores one report. A redelivery of the same
// (target, reported_at, collector) is a no-op (ins=false); every other
// report is kept as history. All writes live in the ingestor (#1283).
func (s *Store) InsertClientDeclaredRegions(r *ClientDeclaredRegions) (bool, error) {
	truncated := 0
	if r.Truncated {
		truncated = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO client_declared_regions
			(collector_pubkey, target_pubkey, regions_csv, truncated, reported_at, ingested_at,
			 repeater_clock, lat, lon, pos_acc_m, mqtt_source)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(target_pubkey, reported_at, collector_pubkey) DO NOTHING`,
		r.Collector, r.Target, strings.Join(r.Regions, ","), truncated, r.ReportedAt, r.IngestedAt,
		r.RepeaterClock, r.Lat, r.Lon, r.PosAccM, r.Source)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// clientRegionsPruneSQL deletes one batch of reports older than the cutoff,
// seeking idx_cdr_reported (pinned by an EXPLAIN test).
const clientRegionsPruneSQL = `DELETE FROM client_declared_regions
	WHERE id IN (SELECT id FROM client_declared_regions WHERE reported_at < ? LIMIT ?)`

// PruneOldClientDeclaredRegions deletes reports whose reported_at is more
// than days old (Config.ClientRegionsDays, never 0 in production).
func (s *Store) PruneOldClientDeclaredRegions(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	n, err := s.pruneClientDeclaredRegionsBefore(time.Now().UTC().AddDate(0, 0, -days))
	if n > 0 {
		log.Printf("[prune] deleted %d client_declared_regions older than %d days", n, days)
	}
	return n, err
}

func (s *Store) pruneClientDeclaredRegionsBefore(cutoff time.Time) (int64, error) {
	return s.deleteInBatchesOf("prune_client_declared_regions", clientRegionsPruneSQL, retentionBatchRows,
		cutoff.UTC().Format(clientRegionsTimeLayout))
}

// runClientRegionsRetention prunes client_declared_regions, logging any error.
// Runs whether or not the gate is on, so stored reports age out after the
// feature is switched off.
func runClientRegionsRetention(store *Store, days int) int64 {
	n, err := store.PruneOldClientDeclaredRegions(days)
	if err != nil {
		log.Printf("[prune] client_declared_regions: %v", err)
	}
	return n
}

// clientRegionsDeleteSQL deletes one batch of a collector's reports for
// admin delete-client-rx, seeking idx_cdr_collector with `=` (never a prefix).
const clientRegionsDeleteSQL = `DELETE FROM client_declared_regions
	WHERE id IN (SELECT id FROM client_declared_regions WHERE collector_pubkey = ? LIMIT ?)`

// DeleteClientDeclaredRegionsByCollector erases every report key collected.
// Reports where key is only the target are kept: they describe a repeater's
// configuration, not the contributor. key must already be validated.
func (s *Store) DeleteClientDeclaredRegionsByCollector(key string) (int64, error) {
	return s.deleteInBatchesOf(deleteClientRxComponent, clientRegionsDeleteSQL, deleteClientRxBatchSize, key)
}

// clientDeclaredRegionsCount is the dry-run count of key's reports over a
// read-only handle. A legacy DB that no ingestor of this version has opened
// has no table yet, and holds no reports.
func clientDeclaredRegionsCount(db *sql.DB, key string) (int64, error) {
	var present int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'client_declared_regions'`).Scan(&present); err != nil {
		return 0, fmt.Errorf("probe client_declared_regions: %w", err)
	}
	if present == 0 {
		return 0, nil
	}
	var n int64
	err := db.QueryRow(`SELECT COUNT(*) FROM client_declared_regions WHERE collector_pubkey = ?`, key).Scan(&n)
	return n, err
}
