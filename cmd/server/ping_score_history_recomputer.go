// Package main: ping-score-history production-wiring layer -- fase 5 of
// the recompute redesign (see reviews/CoreScope-code-review-2026-08-04.md
// and the accompanying fase 5A design discussion). This file is where the
// glue between the already-approved engine/store (fase 4A-4E) and a real
// running server accumulates, phase by phase, per the approved fase 5A
// design's commit breakdown -- NOT wired into main.go yet.
//
// Fase 5C scope: only the retention-configuration bridge below. No
// worker, no lifecycle, no interfaces, no main.go call site.
package main

import (
	"fmt"
	"math"
	"time"
)

// pingScoreHistoryMaxRetentionPacketDays is the largest packetDays value
// that can be multiplied by 24*time.Hour without overflowing
// time.Duration's underlying int64 (nanoseconds). Anything larger would
// silently wrap to an incorrect (possibly negative) duration --
// pingScoreHistoryRetentionDuration rejects it explicitly instead of ever
// computing the overflowed value.
const pingScoreHistoryMaxRetentionPacketDays = math.MaxInt64 / int64(24*time.Hour)

// pingScoreHistoryRetentionDuration converts the server's existing
// retention.packetDays config into the RetentionDuration
// pingScoreHistoryEngineConfig expects, reusing the SAME authoritative
// semantics cmd/ingestor/config.go's own PacketDaysOrZero already
// enforces for the identical field: packetDays is the single source of
// truth for how long the ingestor keeps transmissions/observations
// before PruneOldPackets (cmd/ingestor/main.go) removes them, so the
// history engine's DataPruned/permanent-unreconstructable/gap detection
// must judge age against exactly this window -- never an invented
// default.
//
//   - cfg == nil, or cfg.Retention == nil: returns (0, nil) -- retention
//     is simply not configured. Matches PacketDaysOrZero's own "no
//     Retention block" branch.
//   - cfg.Retention.PacketDays == 0: returns (0, nil) -- PacketDays'
//     own doc comment says "0 disables"; the history engine treats
//     RetentionDuration<=0 as "DataPruned/permanent-unreconstructable/
//     gap detection disabled" (see pingScoreHistoryEngineConfig's own
//     doc comment in ping_score_history_engine.go) -- the exact same
//     meaning, reused unchanged, not a new default.
//   - cfg.Retention.PacketDays > 0 and safely convertible: returns
//     (time.Duration(PacketDays) * 24 * time.Hour, nil) -- the same
//     "N days" interpretation PruneOldPackets itself uses.
//   - cfg.Retention.PacketDays < 0: returns an error. This is a
//     DELIBERATE departure from PacketDaysOrZero's own behavior --
//     PacketDaysOrZero's `> 0` guard silently folds a negative value
//     into "disabled" (identical to zero), with no error and no log.
//     That silent equivalence is a reasonable choice for the ingestor's
//     own prune-or-don't-prune decision, but this bridge exists
//     specifically so a negative value -- almost certainly a config
//     mistake, since nothing legitimately means "prune after -5 days" --
//     never gets a chance to silently masquerade as either "disabled" or
//     a valid duration. The caller (a later production-wiring phase) is
//     expected to fail startup rather than silently run with wrong
//     retention-based detection. (There is no existing config-loader
//     guarantee that rules this out ahead of time -- LoadConfig performs
//     no range validation on retention.packetDays -- so this defensive
//     check is load-bearing, not redundant.)
//   - cfg.Retention.PacketDays > pingScoreHistoryMaxRetentionPacketDays:
//     returns an error rather than ever computing
//     time.Duration(PacketDays)*24*time.Hour, which would overflow
//     time.Duration's int64 and wrap to an incorrect value. (Also not
//     ruled out by the config loader -- packetDays is a plain JSON int
//     with no upper bound enforced anywhere.)
//
// Never mutates cfg or cfg.Retention -- a pure read with no side effects.
func pingScoreHistoryRetentionDuration(cfg *Config) (time.Duration, error) {
	if cfg == nil || cfg.Retention == nil {
		return 0, nil
	}
	packetDays := cfg.Retention.PacketDays
	if packetDays == 0 {
		return 0, nil
	}
	if packetDays < 0 {
		return 0, fmt.Errorf("ping score history: retention.packetDays is negative (%d) -- 0 means disabled, a negative value is not a valid retention window", packetDays)
	}
	if int64(packetDays) > pingScoreHistoryMaxRetentionPacketDays {
		return 0, fmt.Errorf("ping score history: retention.packetDays (%d) is too large to convert to a time.Duration without overflow (max %d)", packetDays, pingScoreHistoryMaxRetentionPacketDays)
	}
	return time.Duration(packetDays) * 24 * time.Hour, nil
}
