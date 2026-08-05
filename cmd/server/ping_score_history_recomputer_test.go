package main

import (
	"testing"
	"time"
)

// 1. nil *Config.
func TestPingScoreHistoryRetentionDuration_NilConfig(t *testing.T) {
	got, err := pingScoreHistoryRetentionDuration(nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != 0 {
		t.Errorf("got %s, want 0", got)
	}
}

// 2. Config with no Retention block at all.
func TestPingScoreHistoryRetentionDuration_NilRetention(t *testing.T) {
	got, err := pingScoreHistoryRetentionDuration(&Config{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != 0 {
		t.Errorf("got %s, want 0", got)
	}
}

// 3. PacketDays == 0 -- matches PacketDaysOrZero's own "0 disables" meaning.
func TestPingScoreHistoryRetentionDuration_ZeroPacketDays(t *testing.T) {
	got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: 0}})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != 0 {
		t.Errorf("got %s, want 0", got)
	}
}

// 4. PacketDays == 1 -> exactly 24 hours, not "approximately a day".
func TestPingScoreHistoryRetentionDuration_OneDay(t *testing.T) {
	got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: 1}})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != 24*time.Hour {
		t.Errorf("got %s, want exactly 24h", got)
	}
}

// 5. A realistic production value.
func TestPingScoreHistoryRetentionDuration_ThirtyDays(t *testing.T) {
	got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: 30}})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != 30*24*time.Hour {
		t.Errorf("got %s, want 30*24h", got)
	}
}

// 6. Negative PacketDays -> a clear error, never a silently-negative
// engine duration. Deliberately stricter than PacketDaysOrZero's own
// silent fold-to-disabled -- see the function's own doc comment.
func TestPingScoreHistoryRetentionDuration_NegativePacketDays(t *testing.T) {
	got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: -1}})
	if err == nil {
		t.Fatal("want an error for a negative PacketDays, got nil")
	}
	if got != 0 {
		t.Errorf("got %s, want 0 alongside the error", got)
	}
	// Some larger negative values too -- not just -1.
	for _, days := range []int{-5, -30, -1000} {
		if _, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: days}}); err == nil {
			t.Errorf("PacketDays=%d: want an error, got nil", days)
		}
	}
}

// 7. The largest PacketDays value that can still be safely converted.
func TestPingScoreHistoryRetentionDuration_MaxSafeValue(t *testing.T) {
	maxDays := int(pingScoreHistoryMaxRetentionPacketDays)
	got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: maxDays}})
	if err != nil {
		t.Fatalf("err = %v, want nil at the exact safe boundary", err)
	}
	want := time.Duration(maxDays) * 24 * time.Hour
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if want < 0 {
		t.Fatalf("sanity check failed: the expected duration itself is negative (%s) -- the boundary constant is wrong", want)
	}
}

// 8. One PacketDays value past the safe boundary -> an error, never an
// overflowed/wrapped (possibly negative) duration.
func TestPingScoreHistoryRetentionDuration_FirstValueOverSafeLimit(t *testing.T) {
	over := int(pingScoreHistoryMaxRetentionPacketDays) + 1
	got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: over}})
	if err == nil {
		t.Fatalf("want an overflow error for PacketDays=%d, got nil (result would have been %s)", over, got)
	}
	if got != 0 {
		t.Errorf("got %s, want 0 alongside the error", got)
	}
}

// 9. The function must not mutate cfg or cfg.Retention.
func TestPingScoreHistoryRetentionDuration_DoesNotMutateConfig(t *testing.T) {
	cfg := &Config{Retention: &RetentionConfig{PacketDays: 30, NodeDays: 7, ObserverDays: 14, MetricsDays: 90}}
	before := *cfg.Retention

	if _, err := pingScoreHistoryRetentionDuration(cfg); err != nil {
		t.Fatal(err)
	}

	if *cfg.Retention != before {
		t.Errorf("cfg.Retention changed: before=%+v after=%+v", before, *cfg.Retention)
	}
}

// 10. The positive-value interpretation matches cmd/ingestor's own
// packet-retention semantics exactly. cmd/ingestor is a separate `package
// main` (can't be imported directly), so this reconstructs the two
// relevant call sites' actual logic inline, cited by file:line, rather
// than asserting against a copy that could silently drift:
//
//   - cmd/ingestor/config.go PacketDaysOrZero(): `if c.Retention != nil
//     && c.Retention.PacketDays > 0 { return c.Retention.PacketDays };
//     return 0` -- 0 and negative both fold to "0 (disabled)", no error.
//   - cmd/ingestor/maintenance.go PruneOldPackets(days): `if days <= 0 {
//     return 0, nil }`, else `cutoff := time.Now().UTC().AddDate(0, 0,
//     -days)` -- i.e. exactly `days` calendar days back, computed in UTC
//     (no DST, so AddDate(0,0,-N) and Add(-N*24*time.Hour) are the same
//     instant).
//
// This test proves: (a) for PacketDays==0, both sides agree on
// "disabled/no window" (0 here, no prune call there); (b) for a positive
// PacketDays, this bridge's RetentionDuration corresponds to EXACTLY the
// same cutoff instant PruneOldPackets would compute from the same "now".
func TestPingScoreHistoryRetentionDuration_PositiveValueMatchesIngestorCutoff(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	for _, days := range []int{1, 7, 30, 90, 365} {
		got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: days}})
		if err != nil {
			t.Fatalf("PacketDays=%d: %v", days, err)
		}

		// This bridge's own cutoff, using the SAME duration-subtraction
		// approach production wiring will use (now.Add(-RetentionDuration)).
		bridgeCutoff := now.Add(-got)

		// PruneOldPackets' own cutoff computation, reconstructed verbatim
		// (maintenance.go: `time.Now().UTC().AddDate(0, 0, -days)`).
		ingestorCutoff := now.AddDate(0, 0, -days)

		if !bridgeCutoff.Equal(ingestorCutoff) {
			t.Errorf("PacketDays=%d: bridge cutoff %s != ingestor cutoff %s", days, bridgeCutoff, ingestorCutoff)
		}
	}

	// PacketDays==0: this bridge returns a disabled (0) duration, and
	// PacketDaysOrZero()/PruneOldPackets' own `> 0`/`<= 0` guards mean no
	// prune call (and thus no cutoff) is ever computed on that side either
	// -- both sides agree "there is no retention window", just expressed
	// differently (a zero Duration here vs. skipping the call there).
	got, err := pingScoreHistoryRetentionDuration(&Config{Retention: &RetentionConfig{PacketDays: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("PacketDays=0: got %s, want 0 (disabled, matching PruneOldPackets' own no-op)", got)
	}
}
