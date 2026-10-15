package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #380 sizing. The backfill walks transmissions in id order, so its
// cost is (one bounded pass over the table) + (decode + one UPDATE per
// candidate). This benchmark builds a synthetic table of the shape a real
// deployment has — a few hundred thousand transmissions of which a small
// share are undecryptable GRP_TXT rows — and times one complete run, so the
// per-row cost can be extrapolated to a multi-GB database before any staging
// run. It reports rows/s over the whole table and over the candidates.
//
// Sizes are vars so a CI run can shrink them; the defaults are what the
// numbers quoted in the PR body were measured with.
var (
	decodeBenchRows       = 200000
	decodeBenchCandidates = 20000
)

// seedDecodeBenchDB inserts decodeBenchRows transmissions, of which
// decodeBenchCandidates are enc_<HEX> GRP_TXT rows carrying a real encrypted
// frame for channel. The rest are ADVERT-shaped rows the pass must skip.
func seedDecodeBenchDB(tb testing.TB, s *Store, channelKeyHex string) {
	tb.Helper()
	frames := make([]string, 16)
	for i := range frames {
		// One shared channel-hash byte: what a single busy channel looks like.
		frames[i] = decodeBFGrpTxt(tb, channelKeyHex, fmt.Sprintf("Alice: bench %d", i), 0x3A)
	}
	tx, err := s.db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, channel_hash, decoded_json)
		VALUES (?, ?, '2026-01-01T00:00:00Z', 1, ?, ?, ?)`)
	if err != nil {
		tb.Fatal(err)
	}
	every := decodeBenchRows / decodeBenchCandidates
	for i := 0; i < decodeBenchRows; i++ {
		if every > 0 && i%every == 0 {
			f := frames[i%len(frames)]
			if _, err := stmt.Exec(f, fmt.Sprintf("bench-c-%d", i), 5, "enc_3A", `{"type":"GRP_TXT","decryptionStatus":"no_key"}`); err != nil {
				tb.Fatal(err)
			}
			continue
		}
		if _, err := stmt.Exec("1101AB00", fmt.Sprintf("bench-a-%d", i), 4, nil, `{"type":"ADVERT"}`); err != nil {
			tb.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

func BenchmarkDecodeBackfillRun(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	s, err := OpenStore(path)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	s.backfillWg.Wait()

	key := deriveHashtagChannelKey(decodeBFChannel)
	seedDecodeBenchDB(b, s, key)
	hk := newHotKeys(decodeBFChannelKeys(decodeBFChannel), nil)
	// Pause 0: the benchmark measures the work, not the deliberate idle time
	// between batches (the production default adds 250 ms per batch).
	set := DecodeBackfillSettings{Enabled: true, BatchSize: defaultDecodeBackfillBatchSize,
		IDWindow: defaultDecodeBackfillIDWindow, Pause: 0, Poll: time.Hour}

	var updated int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetDecodeBenchDB(b, s)
		b.StartTimer()
		res, err := s.RunDecodeBackfill(context.Background(), hk, set)
		if err != nil {
			b.Fatal(err)
		}
		updated = res.ChannelUpdated
	}
	b.StopTimer()
	if updated == 0 {
		b.Fatalf("benchmark resolved nothing; the fixture is wrong")
	}
	perRun := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	b.ReportMetric(float64(decodeBenchRows)/(perRun/1e9), "table_rows/s")
	b.ReportMetric(float64(updated)/(perRun/1e9), "rewritten_rows/s")
}

// resetDecodeBenchDB puts the candidates back to their unresolved state and
// clears the run bookkeeping, so every iteration does the same work.
func resetDecodeBenchDB(tb testing.TB, s *Store) {
	tb.Helper()
	for _, q := range []string{
		`UPDATE transmissions SET channel_hash = 'enc_3A', decoded_json = '{"type":"GRP_TXT","decryptionStatus":"no_key"}' WHERE payload_type = 5`,
		`DELETE FROM decode_backfill_keys`,
		`DELETE FROM decode_backfill_state`,
	} {
		if _, err := s.db.Exec(q); err != nil && !strings.Contains(err.Error(), "no such table") {
			tb.Fatal(err)
		}
	}
}
