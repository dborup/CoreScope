package anomaly

import (
	"testing"
	"time"
)

// Issue #474, phase 2: a periodic rule can ignore expected-automation
// channels (for example #wardriving) through PeriodicRule.ExcludeChannels.
// The option is opt-in: an empty list excludes nothing, which is the
// behaviour every other test in this package pins.
//
// All channel bytes here are synthetic test values. 0x81 is used because the
// #457 round-4 results name it, but nothing in the library knows what a
// channel hash means.

// perfectTrain is a perfect 12-pulse 5-minute train on one stream, which
// experimentalPeriodic activates on at the 8th pulse.
func perfectTrain(t testing.TB, prefix string, ch byte) []Event {
	t.Helper()
	return series(t, prefix, t0, 12, 5*time.Minute, nil, ch, 0x22)
}

// excludingRule returns experimentalPeriodic with the given channel hashes
// excluded.
func excludingRule(t testing.TB, chans ...ChannelHash) PeriodicRule {
	t.Helper()
	r := experimentalPeriodic()
	r.ExcludeChannels = chans
	return r
}

// A train on an excluded channel produces no periodic candidate at all,
// while the same train on another channel still activates.
func TestPeriodicExcludedChannelProducesNoCandidate(t *testing.T) {
	rule := excludingRule(t, mustChan(t, 0x81))
	d := newDet(t, Config{Periodic: []PeriodicRule{rule}})
	if cs := run(t, d, perfectTrain(t, "x", 0x81)); len(cs) != 0 {
		t.Fatalf("excluded channel produced candidates:\n%s", describe(cs))
	}
	d = newDet(t, Config{Periodic: []PeriodicRule{rule}})
	cs := run(t, d, perfectTrain(t, "y", 0x11))
	if len(filter(cs, "per", StateActive)) != 1 {
		t.Fatalf("a non-excluded channel must still activate:\n%s", describe(cs))
	}
}

// The exclusion matches the whole channel hash, length included: excluding
// the 1-byte hash 0x81 does not exclude the 2-byte hash 0x81ab, exactly as
// Key.String keeps a 1-byte value distinct from the prefix of a wider one.
func TestPeriodicExclusionMatchesTheWholeHash(t *testing.T) {
	rule := excludingRule(t, mustChan(t, 0x81))
	wide := perfectTrain(t, "w", 0x81)
	for i := range wide {
		wide[i].Channel = mustChan(t, 0x81, 0xab)
	}
	cs := run(t, newDet(t, Config{Periodic: []PeriodicRule{rule}}), wide)
	if len(filter(cs, "per", StateActive)) != 1 {
		t.Fatalf("a 2-byte hash must not be excluded by a 1-byte entry:\n%s", describe(cs))
	}
	// The reverse: a 2-byte entry does not exclude the 1-byte hash.
	rule = excludingRule(t, mustChan(t, 0x81, 0xab))
	cs = run(t, newDet(t, Config{Periodic: []PeriodicRule{rule}}), perfectTrain(t, "n", 0x81))
	if len(filter(cs, "per", StateActive)) != 1 {
		t.Fatalf("a 1-byte hash must not be excluded by a 2-byte entry:\n%s", describe(cs))
	}
	// The length is part of the match even when the bytes alone look equal:
	// the 2-byte hash 0x8100 is not the 1-byte hash 0x81 followed by
	// padding, so a 0x81 entry does not exclude it.
	rule = excludingRule(t, mustChan(t, 0x81))
	padded := perfectTrain(t, "z", 0x81)
	for i := range padded {
		padded[i].Channel = mustChan(t, 0x81, 0x00)
	}
	cs = run(t, newDet(t, Config{Periodic: []PeriodicRule{rule}}), padded)
	if len(filter(cs, "per", StateActive)) != 1 {
		t.Fatalf("the 2-byte hash 0x8100 was excluded by the 1-byte entry 0x81:\n%s", describe(cs))
	}
}

// Every listed channel is excluded, not only the first one.
func TestPeriodicExclusionCoversEveryListedChannel(t *testing.T) {
	rule := excludingRule(t, mustChan(t, 0x81), mustChan(t, 0x62))
	for _, ch := range []byte{0x81, 0x62} {
		d := newDet(t, Config{Periodic: []PeriodicRule{rule}})
		if cs := run(t, d, perfectTrain(t, "m", ch)); len(cs) != 0 {
			t.Fatalf("channel %#x produced candidates:\n%s", ch, describe(cs))
		}
	}
}

// The exclusion belongs to the periodic rule only: rate and new-stream
// rules on the same events are untouched, and so is a second periodic rule
// without the exclusion.
func TestPeriodicExclusionLeavesOtherRulesAlone(t *testing.T) {
	other := experimentalPeriodic()
	other.Name = "per2"
	cfg := Config{
		Rate: []RateRule{rateRule("r", ScopeStream, time.Hour, 8)},
		NewStream: []NewStreamRule{{Name: "ns", Scope: ScopeStream, Window: time.Hour, MinCount: 6,
			CensorWindow: 24 * time.Hour, QuietPeriod: 24 * time.Hour, State: quickState()}},
		Periodic:     []PeriodicRule{excludingRule(t, mustChan(t, 0x81)), other},
		HistoryStart: t0.Add(-48 * time.Hour),
	}
	cs := run(t, newDet(t, cfg), perfectTrain(t, "o", 0x81))
	for _, rule := range []string{"r", "ns", "per2"} {
		if len(filter(cs, rule, StateActive)) == 0 {
			t.Errorf("rule %s did not activate on an excluded channel:\n%s", rule, describe(cs))
		}
	}
	if a := filter(cs, "per", StateActive); len(a) != 0 {
		t.Fatalf("the excluding rule activated:\n%s", describe(cs))
	}
}

// With only an excluding periodic rule, an event on an excluded channel
// creates no key state at all, so the exclusion also keeps the scope's key
// budget free.
func TestPeriodicExclusionKeepsNoKeyState(t *testing.T) {
	d := newDet(t, Config{Periodic: []PeriodicRule{excludingRule(t, mustChan(t, 0x81))}})
	run(t, d, perfectTrain(t, "k", 0x81))
	if n := d.Snapshot().Keys[ScopeStream]; n != 0 {
		t.Fatalf("excluded channel kept %d stream keys, want 0", n)
	}
	d = newDet(t, Config{Periodic: []PeriodicRule{excludingRule(t, mustChan(t, 0x81))}})
	run(t, d, perfectTrain(t, "k2", 0x11))
	if n := d.Snapshot().Keys[ScopeStream]; n != 1 {
		t.Fatalf("a non-excluded channel kept %d stream keys, want 1", n)
	}
}

// New copies ExcludeChannels: changing the caller's slice afterwards cannot
// change what the detector excludes, and the detector never writes to it.
func TestPeriodicExclusionIsCopiedFromTheCallerConfig(t *testing.T) {
	rule := excludingRule(t, mustChan(t, 0x81))
	cfg := Config{Periodic: []PeriodicRule{rule}, Expected: ExpectedPolicy{Mode: ExpectedInclude},
		Limits: experimentalLimits()}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Periodic[0].ExcludeChannels[0] = mustChan(t, 0x11)
	if cs := run(t, d, perfectTrain(t, "c", 0x81)); len(cs) != 0 {
		t.Fatalf("the caller's slice changed what the detector excludes:\n%s", describe(cs))
	}
	if got := cfg.Periodic[0].ExcludeChannels[0]; got != mustChan(t, 0x11) {
		t.Fatalf("the detector wrote to the caller's slice: %v", got.Bytes())
	}
}

// An event without a channel (a payload type that carries none) is never
// excluded: the zero "no channel" value is not a 1-byte hash and is not a
// valid list entry.
func TestPeriodicExclusionNeverMatchesNoChannel(t *testing.T) {
	rule := excludingRule(t, mustChan(t, 0x00))
	rule.PayloadTypes = []PayloadType{4}
	var evs []Event
	for i := 0; i < 12; i++ {
		evs = append(evs, Event{ID: TxID("a" + string(rune('a'+i))), Time: at(time.Duration(i) * 5 * time.Minute),
			PayloadType: 4, RouteClass: RouteClassFlood, RouteKind: RouteKindRelay,
			FirstHop: mustHop(t, 0x22), FullRoute: mustRoute(t, 1, 0x22, 0x00), WireBytes: 40})
	}
	cs := run(t, newDet(t, Config{Periodic: []PeriodicRule{rule}}), evs)
	if len(filter(cs, "per", StateActive)) != 1 {
		t.Fatalf("a channel-less train must not be excluded by a 1-byte 0x00 entry:\n%s", describe(cs))
	}
}

// The exclusion works at channel scope too, where the key is the channel.
func TestPeriodicExclusionAtChannelScope(t *testing.T) {
	rule := excludingRule(t, mustChan(t, 0x81))
	rule.Scope = ScopeChannel
	d := newDet(t, Config{Periodic: []PeriodicRule{rule}})
	if cs := run(t, d, perfectTrain(t, "s", 0x81)); len(cs) != 0 {
		t.Fatalf("channel scope ignored the exclusion:\n%s", describe(cs))
	}
}

func TestExcludeChannelsValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		list []ChannelHash
		ok   bool
	}{
		{"unset", nil, true},
		{"empty", []ChannelHash{}, true},
		{"one byte", []ChannelHash{mustChan(t, 0x81)}, true},
		{"two entries", []ChannelHash{mustChan(t, 0x81), mustChan(t, 0x62)}, true},
		{"wider hash", []ChannelHash{mustChan(t, 0x81, 0xab, 0xcd, 0xef)}, true},
		{"same byte twice", []ChannelHash{mustChan(t, 0x81), mustChan(t, 0x81)}, false},
		{"no channel", []ChannelHash{{}}, false},
		{"non-canonical", []ChannelHash{nonCanonicalChan(0x81)}, false},
	} {
		r := experimentalPeriodic()
		r.ExcludeChannels = c.list
		err := (&Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude},
			Periodic: []PeriodicRule{r}}).Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, want ok=%v", c.name, err, c.ok)
		}
	}
	// Too many entries are rejected; the whole 1-byte space is not.
	full := make([]ChannelHash, 0, maxExcludedChannels+1)
	for i := 0; i <= maxExcludedChannels; i++ {
		c, err := NewChannelHash([]byte{byte(i % 256), byte(i / 256)}[:1+i/256])
		if err != nil {
			t.Fatal(err)
		}
		full = append(full, c)
	}
	for _, c := range []struct {
		n  int
		ok bool
	}{{maxExcludedChannels, true}, {maxExcludedChannels + 1, false}} {
		r := experimentalPeriodic()
		r.ExcludeChannels = full[:c.n]
		err := (&Config{Limits: experimentalLimits(), Expected: ExpectedPolicy{Mode: ExpectedInclude},
			Periodic: []PeriodicRule{r}}).Validate()
		if (err == nil) != c.ok {
			t.Errorf("%d entries: err %v, want ok=%v", c.n, err, c.ok)
		}
	}
}

// nonCanonicalChan builds a 1-byte hash with a stray byte beyond its length,
// which Key.Validate rejects elsewhere and the exclusion list rejects here.
func nonCanonicalChan(b byte) ChannelHash {
	c := ChannelHash{n: 1}
	c.b[0], c.b[1] = b, 0xff
	return c
}

// ---- replay JSON config ----

func TestReplayConfigParsesExcludeChannels(t *testing.T) {
	cfg, err := ParseReplayConfig([]byte(excludeConfigJSON(`"exclude_channels": ["81", "62ab"],`)))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Periodic[0].ExcludeChannels
	if len(got) != 2 || got[0] != mustChan(t, 0x81) || got[1] != mustChan(t, 0x62, 0xab) {
		t.Fatalf("exclude_channels parsed as %v", got)
	}
}

// Omitting exclude_channels means no exclusion: the field is opt-in and has
// no default.
func TestReplayConfigWithoutExcludeChannelsExcludesNothing(t *testing.T) {
	cfg, err := ParseReplayConfig([]byte(excludeConfigJSON("")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Periodic[0].ExcludeChannels; got != nil {
		t.Fatalf("exclude_channels defaulted to %v", got)
	}
}

func TestReplayConfigRejectsBadExcludeChannels(t *testing.T) {
	for name, v := range map[string]string{
		"odd hex":      `["8"]`,
		"not hex":      `["zz"]`,
		"empty string": `[""]`,
		"too long":     `["0102030405"]`,
		"upper case":   `["AB"]`,
		"with prefix":  `["0x81"]`,
		"duplicate":    `["81", "81"]`,
		"not a string": `[129]`,
	} {
		if _, err := ParseReplayConfig([]byte(excludeConfigJSON(`"exclude_channels": ` + v + `,`))); err == nil {
			t.Errorf("%s: accepted %s", name, v)
		}
	}
}

// excludeConfigJSON is a minimal valid replay config with one periodic rule,
// into which a line such as `"exclude_channels": ["81"],` is spliced.
func excludeConfigJSON(line string) string {
	return `{
 "limits": {"max_keys_per_scope": 1000, "state_ttl": "48h", "dedup_horizon": "6h", "max_dedup_ids": 1000,
  "reorder_delay": "0s", "max_reorder_buffer": 64, "max_window_entries": 1000,
  "max_candidates_per_call": 100, "max_pending_candidates": 1000},
 "expected": "include",
 "periodic": [{"name": "p474", "scope": "stream", "payload_types": [5],
  "min_period": "1m", "max_period": "1h", "burst_gap": "15s", "jitter_abs": "10s", "jitter_rel": 0.02,
  "max_missing": 2, "min_pulses": 8, "history_len": 32, "min_coverage": 0.6, "max_jitter_fraction": 0.5,
  "max_chance": 0.01, "end_after_missed_pulses": 3, ` + line + `
  "state": {"confirm_after": "0s", "end_after_quiet": "60m", "cooldown": "0s"}}]
}`
}
