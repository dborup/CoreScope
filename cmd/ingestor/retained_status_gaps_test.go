package main

import (
	"strings"
	"testing"
	"time"
)

// Gap-closing tests for #1885. An independent mutation run against the
// original seven tests left nine mutants alive; these kill the seven that
// matter. Each test names the mutation it kills so a future reader can tell
// what it is load-bearing for. Six tests kill seven mutants because
// TestRetainedStatusPreservesCanRelaySeen covers both directions of
// can_relay_seen in two independent scenarios.
//
// The two remaining survivors are deliberate: one concerns the 24h constant's
// exact value (any value between "a few hours" and "months" passes, and the
// constant is documented as a generous margin rather than a threshold), and
// one concerns Stats.ObserverUpserts, which is a counter with no behavioural
// consequence on either path.

// Kills: re-adding `packet_count = packet_count + 1` to UpsertObserverRetained.
//
// The count is the observer's real traffic volume. A replay storm that bumped
// it would inflate every dead observer's count by one per ingestor restart,
// and nothing else in the suite looks at the column on this path.
func TestRetainedStatusDoesNotBumpPacketCount(t *testing.T) {
	store := newTestStore(t)
	seedObserver(t, store, "obs-count", 60)
	if _, err := store.db.Exec(`UPDATE observers SET packet_count = 7 WHERE id = ?`, "obs-count"); err != nil {
		t.Fatal(err)
	}

	handleMessage(store, "test", MQTTSource{Name: "test"},
		retainedStatusMsg("meshcore/LAX/obs-count/status", `{"origin":"obs-count","noise_floor":-95.5}`),
		nil, nil, &Config{})

	var got int
	if err := store.db.QueryRow(`SELECT packet_count FROM observers WHERE id = ?`, "obs-count").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Errorf("packet_count = %d, want 7 (a replay is not a packet)", got)
	}

	// The live path must still bump it, or the guard above is just a bug.
	handleMessage(store, "test", MQTTSource{Name: "test"},
		liveStatusMsg("meshcore/LAX/obs-count/status",
			statusPayload("obs-count", "online", time.Now().UTC())),
		nil, nil, &Config{})
	if err := store.db.QueryRow(`SELECT packet_count FROM observers WHERE id = ?`, "obs-count").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Errorf("packet_count = %d after live status, want 8 (live traffic still counts)", got)
	}
}

// Kills: making can_relay_seen regress 1 -> 0, and never setting it to 1.
//
// can_relay_seen is the tristate's "we have actually observed this" bit
// (#1290). A retained replay carrying no `repeat` field must not erase an
// answer the live path already recorded.
func TestRetainedStatusPreservesCanRelaySeen(t *testing.T) {
	store := newTestStore(t)
	seedObserver(t, store, "obs-relay", 60)
	if _, err := store.db.Exec(
		`UPDATE observers SET can_relay = 1, can_relay_seen = 1 WHERE id = ?`, "obs-relay"); err != nil {
		t.Fatal(err)
	}

	// No `repeat` key: meta.CanRelay stays nil, so the CASE must leave the
	// flag alone rather than reset it.
	handleMessage(store, "test", MQTTSource{Name: "test"},
		retainedStatusMsg("meshcore/LAX/obs-relay/status", `{"origin":"obs-relay","noise_floor":-95.5}`),
		nil, nil, &Config{})

	var seen, canRelay int
	if err := store.db.QueryRow(
		`SELECT can_relay_seen, can_relay FROM observers WHERE id = ?`, "obs-relay").
		Scan(&seen, &canRelay); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("can_relay_seen = %d, want 1 (a replay without `repeat` must not un-observe it)", seen)
	}
	if canRelay != 1 {
		t.Errorf("can_relay = %d, want 1 (unchanged by a replay that does not report it)", canRelay)
	}

	// A replay that DOES carry `repeat` still records the observation: the
	// metadata half of the retained path is supposed to keep working.
	store2 := newTestStore(t)
	seedObserver(t, store2, "obs-relay2", 60)
	if _, err := store2.db.Exec(
		`UPDATE observers SET can_relay = NULL, can_relay_seen = 0 WHERE id = ?`, "obs-relay2"); err != nil {
		t.Fatal(err)
	}
	handleMessage(store2, "test", MQTTSource{Name: "test"},
		retainedStatusMsg("meshcore/LAX/obs-relay2/status",
			`{"origin":"obs-relay2","repeat":"true"}`),
		nil, nil, &Config{})
	if err := store2.db.QueryRow(
		`SELECT can_relay_seen, can_relay FROM observers WHERE id = ?`, "obs-relay2").
		Scan(&seen, &canRelay); err != nil {
		t.Fatal(err)
	}
	if seen != 1 || canRelay != 1 {
		t.Errorf("can_relay_seen/can_relay = %d/%d, want 1/1 (a reported value is still recorded)", seen, canRelay)
	}
}

// Kills: replacing strings.EqualFold with a case-sensitive ==.
//
// The whole guard turns on one string comparison. If it became
// case-sensitive, any firmware publishing "ONLINE" would have its liveness
// silently suppressed — the observer would age out despite being alive, which
// is the opposite of the bug this change fixes and far worse than it.
func TestStatusOnlineComparisonIsCaseInsensitive(t *testing.T) {
	now := time.Now().UTC()
	for _, s := range []string{"online", "ONLINE", "OnLiNe", "Online"} {
		if !statusIsLiveness(map[string]interface{}{"status": s}, now) {
			t.Errorf("statusIsLiveness(status=%q) = false, want true", s)
		}
	}
	for _, s := range []string{"offline", "OFFLINE", "OffLine"} {
		if statusIsLiveness(map[string]interface{}{"status": s}, now) {
			t.Errorf("statusIsLiveness(status=%q) = true, want false", s)
		}
	}
}

// Kills: dropping the `s != ""` guard.
//
// An empty status string is not a claim of being offline — it is the absence
// of a claim, and the guard exists so a payload that carries the key but no
// value keeps the previous behaviour instead of being silently discarded.
func TestEmptyStatusStringIsNotAnOfflineClaim(t *testing.T) {
	now := time.Now().UTC()
	if !statusIsLiveness(map[string]interface{}{"status": ""}, now) {
		t.Error(`statusIsLiveness(status="") = false, want true (no claim is not an offline claim)`)
	}
	// A non-string status is likewise unjudgeable and must not suppress.
	if !statusIsLiveness(map[string]interface{}{"status": 1}, now) {
		t.Error("statusIsLiveness(status=1) = false, want true (unjudgeable, keep previous behaviour)")
	}
}

// Kills: flipping `!t.Before(cutoff)` to `t.After(cutoff)`.
//
// The two differ only exactly at the cutoff. The existing tests use 4-month
// staleness and "now", so nothing pinned which side of the boundary is
// inclusive.
func TestStatusLivenessAgeBoundaryIsInclusive(t *testing.T) {
	// Truncate to whole seconds: the payload timestamp round-trips through
	// RFC3339, which carries no sub-second part, so an untruncated `now`
	// would put the "exactly at the cutoff" case a few hundred nanoseconds
	// on the wrong side of the comparison and test the formatter instead of
	// the boundary.
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-statusLivenessMaxAge)

	cases := []struct {
		name string
		ts   time.Time
		want bool
	}{
		{"exactly at the cutoff", cutoff, true},
		{"one second inside", cutoff.Add(time.Second), true},
		{"one second outside", cutoff.Add(-time.Second), false},
	}
	for _, c := range cases {
		msg := map[string]interface{}{
			"status":    "online",
			"timestamp": c.ts.Format(time.RFC3339),
		}
		if got := statusIsLiveness(msg, now); got != c.want {
			t.Errorf("%s: statusIsLiveness = %v, want %v", c.name, got, c.want)
		}
	}
}

// Kills: dropping the TrimSpace/ToUpper normalisation of iata in
// UpsertObserverRetained.
//
// The live path normalises, so a replay writing a raw topic segment would
// make the same observer's region disagree with itself depending on which
// path last touched it.
func TestRetainedStatusNormalizesIATA(t *testing.T) {
	store := newTestStore(t)
	// Seed through the normal path so the row exists, then blank the column
	// so the COALESCE has something to overwrite.
	seedObserver(t, store, "obs-iata", 60)
	if _, err := store.db.Exec(`UPDATE observers SET iata = 'XXX' WHERE id = ?`, "obs-iata"); err != nil {
		t.Fatal(err)
	}

	handleMessage(store, "test", MQTTSource{Name: "test"},
		retainedStatusMsg("meshcore/lax/obs-iata/status", `{"origin":"obs-iata"}`),
		nil, nil, &Config{})

	var got string
	if err := store.db.QueryRow(`SELECT iata FROM observers WHERE id = ?`, "obs-iata").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != strings.ToUpper(got) || got != "LAX" {
		t.Errorf("iata = %q, want %q (retained path must normalise like the live path)", got, "LAX")
	}
}
