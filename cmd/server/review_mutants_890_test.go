package main

import (
	"testing"
	"time"
)

// #890 — tests that pin mutants which survived the v0.2.10 reviews. Each
// test names its mutant; none changes behaviour.

// ── Antenna-sites breaker (#878) ───────────────────────────────────────────

// openBreaker890 is a breaker with threshold 1 that one down outcome opens at
// clk, and the generation its probe will run in.
func openBreaker890(t *testing.T, clk time.Time, cooldown time.Duration) (*mastsBreaker, uint64) {
	t.Helper()
	b := newMastsBreaker(1, cooldown)
	gen, ok := b.start(false)
	if !ok {
		t.Fatal("a closed breaker refused a fetch")
	}
	if got := b.record(clk, gen, mastsOutcomeDown); got != "open" {
		t.Fatalf("record = %q, want open", got)
	}
	return b, b.gen
}

// #878 review K1, mutant: remove `b.gen++` from the re-open branch of
// record(). An abandoned probe (it freed the slot, see endProbe) and its
// successor both ran in the open generation. The successor fails and
// re-opens the breaker; the abandoned probe's late success must then
// count for nothing. Without the bump it carries the current generation
// and closes the breaker in the middle of the fresh cool-down.
func TestMastsBreakerReopenStartsNewGeneration890(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const cooldown = 100 * time.Second
	b, openGen := openBreaker890(t, t0, cooldown)

	t1 := t0.Add(cooldown) // cool-down over: a probe is let through
	probeA, _, ok := b.admit(t1)
	if !ok || !probeA {
		t.Fatal("no probe after the cool-down")
	}
	genA, ok := b.start(true)
	if !ok || genA != openGen {
		t.Fatalf("probe A gen %d ok %v, want %d", genA, ok, openGen)
	}
	b.endProbe() // A's client left: the probe slot is free again
	probeB, _, ok := b.admit(t1)
	if !ok || !probeB {
		t.Fatal("no second probe after the first was abandoned")
	}
	genB, _ := b.start(true)
	if got := b.record(t1, genB, mastsOutcomeDown); got != "reopen" {
		t.Fatalf("probe B failure = %q, want reopen", got)
	}
	if b.gen == openGen {
		t.Fatalf("re-open kept generation %d; it must start a new one", openGen)
	}

	t2 := t1.Add(10 * time.Second) // inside the fresh cool-down
	if got := b.record(t2, genA, mastsOutcomeOK); got != "" {
		t.Fatalf("abandoned probe's late success = %q, want no state change", got)
	}
	if _, retry, ok := b.admit(t2); ok || retry != 90*time.Second {
		t.Fatalf("admit after the late success: ok=%v retry=%s, want refused for the remaining 90s", ok, retry)
	}
}

// #878 review K4, mutants: admit() or retryAfter() answers the full
// cool-down instead of what is left of it. A test that checks the answer
// at the instant of opening cannot tell the two apart, so this one lets
// part of the cool-down pass first.
func TestMastsBreakerRetryAfterIsRemainingCooldown890(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const cooldown = 300 * time.Second
	b, _ := openBreaker890(t, t0, cooldown)

	for _, elapsed := range []time.Duration{0, 10 * time.Second, 299 * time.Second} {
		now := t0.Add(elapsed)
		want := cooldown - elapsed
		if _, retry, ok := b.admit(now); ok || retry != want {
			t.Errorf("admit after %s: ok=%v retry=%s, want refused for %s", elapsed, ok, retry, want)
		}
		if got := b.retryAfter(now); got != want {
			t.Errorf("retryAfter after %s = %s, want %s", elapsed, got, want)
		}
	}
	// Once the cool-down is over the probe is out: busy retry, not a negative or full cool-down.
	now := t0.Add(cooldown)
	if probe, _, ok := b.admit(now); !ok || !probe {
		t.Fatal("no probe after the cool-down")
	}
	if got := b.retryAfter(now); got != mastsBusyRetry {
		t.Errorf("retryAfter with the probe out = %s, want %s", got, mastsBusyRetry)
	}
}

// ── Config gates (#889) ────────────────────────────────────────────────────

// #889 review, mutant: delete the `c.keys == nil` early return of
// extractDHMConfig. A Config built in code carries no loaded file, so the
// canonical-key gates have nothing to read; the contract is that the typed
// values decoded from map.dhm then stand. Without the guard every gate is
// recomputed from a nil key doc and silently turns false.
func TestExtractDHMConfigWithoutKeysKeepsDecodedGates890(t *testing.T) {
	c := &Config{Map: map[string]interface{}{"dhm": map[string]interface{}{
		"enabled": true,
		"ortho":   map[string]interface{}{"enabled": true},
		"profile": map[string]interface{}{"enabled": true},
	}}}
	if c.keys != nil {
		t.Fatal("test setup: the Config must have no loaded keys")
	}
	c.extractDHMConfig()
	if _, left := c.Map["dhm"]; left {
		t.Error("map.dhm still in the client-visible Map")
	}
	if !c.DHM.Enabled || !c.DHM.Ortho.Enabled || !c.DHM.Profile.Enabled {
		t.Errorf("gates without keys: enabled=%v ortho=%v profile=%v, want the decoded true values to stand",
			c.DHM.Enabled, c.DHM.Ortho.Enabled, c.DHM.Profile.Enabled)
	}
}
