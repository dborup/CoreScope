package anomaly

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func normCfg() NormalizerConfig {
	return NormalizerConfig{SettleWindow: 30 * time.Second, MaxPendingTransmissions: 1000,
		MaxObservationsPerTransmission: 64, FinalizedHorizon: time.Hour, MaxFinalizedIDs: 100_000}
}

func obs(t testing.TB, tx, id string, when time.Time, class RouteClass, width int, hops ...byte) Observation {
	t.Helper()
	o := Observation{TxID: TxID(tx), ObsID: id, Time: when, PayloadType: 5, Channel: mustChan(t, 0x42),
		RouteClass: class, WireBytes: 40 + len(hops)}
	if len(hops) > 0 {
		o.Path = mustRoute(t, width, hops...)
	}
	return o
}

func TestNormalizeFanoutIsOneTransmission(t *testing.T) {
	var os []Observation
	for i := 0; i < 5; i++ {
		os = append(os, obs(t, "tx", fmt.Sprintf("o%d", i), at(time.Duration(i)*time.Second), RouteClassFlood, 1, byte(0x10+i), 0x99))
	}
	e, rep, err := NormalizeTransmission(os, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if e.Observations != 5 || rep.Included != 5 {
		t.Fatalf("fan-out: %d observations, %d included", e.Observations, rep.Included)
	}
	// feeding the event to a detector counts it once
	d := newDet(t, Config{Rate: []RateRule{rateRule("r", ScopeGlobal, time.Minute, 2)}})
	cs := run(t, d, []Event{e})
	if len(cs) != 0 {
		t.Fatalf("one transmission with five observations must count once:\n%s", describe(cs))
	}
}

func TestNormalizeFirstHopIsEarliestPathKeepingWidth(t *testing.T) {
	os := []Observation{
		obs(t, "tx", "b", at(2*time.Second), RouteClassFlood, 2, 0xAA, 0x01, 0xBB, 0x02), // later, path
		obs(t, "tx", "a", at(1*time.Second), RouteClassFlood, 0),                         // earliest, no path
		obs(t, "tx", "c", at(3*time.Second), RouteClassFlood, 2, 0xCC, 0x03),
	}
	e, _, err := NormalizeTransmission(os, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if e.RouteKind != RouteKindRelay || e.FirstHop != mustHop(t, 0xAA, 0x01) {
		t.Fatalf("first hop %x (kind %v), want 2-byte aa01 from the earliest observation with a path", e.FirstHop.Bytes(), e.RouteKind)
	}
	if e.FullRoute.Hops() != 2 || e.FullRoute.Width() != 2 {
		t.Fatalf("full route %d hops width %d", e.FullRoute.Hops(), e.FullRoute.Width())
	}
	if !e.Time.Equal(at(time.Second)) {
		t.Fatalf("event time %v, want earliest observation", e.Time)
	}
	// tie on time: ObsID breaks it
	tie := []Observation{
		obs(t, "tx2", "z", at(0), RouteClassFlood, 1, 0x02),
		obs(t, "tx2", "y", at(0), RouteClassFlood, 1, 0x01),
	}
	e2, _, _ := NormalizeTransmission(tie, time.Minute)
	if e2.FirstHop != mustHop(t, 0x01) {
		t.Fatalf("tie must break on ObsID, got %x", e2.FirstHop.Bytes())
	}
}

func TestNormalizePermutationInvariant(t *testing.T) {
	base := []Observation{
		obs(t, "tx", "o1", at(3*time.Second), RouteClassFlood, 1, 0x05, 0x06),
		obs(t, "tx", "o2", at(1*time.Second), RouteClassFlood, 1, 0x07),
		obs(t, "tx", "o3", at(1*time.Second), RouteClassFlood, 0),
		obs(t, "tx", "o4", at(2*time.Second), RouteClassTransportFlood, 1, 0x08),
		obs(t, "tx", "o5", at(9*time.Second), RouteClassFlood, 1, 0x09),
	}
	want, _, err := NormalizeTransmission(base, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		p := append([]Observation(nil), base...)
		rng.Shuffle(len(p), func(a, b int) { p[a], p[b] = p[b], p[a] })
		got, _, err := NormalizeTransmission(p, 30*time.Second)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d changed the event: %+v vs %+v (%v)", i, got, want, err)
		}
	}
}

func TestNormalizerLateObservationNeverChangesFinalEvent(t *testing.T) {
	n, _ := NewNormalizer(normCfg())
	a := obs(t, "tx", "a", at(10*time.Second), RouteClassFlood, 1, 0x11)
	if r := n.Add(a); r.Status != ObsAccepted {
		t.Fatal(r.Status)
	}
	// a later-arriving observation with an EARLIER timestamp, after the deadline
	late := obs(t, "tx", "b", at(0), RouteClassFlood, 1, 0x22)
	late.ArrivedAt = at(10*time.Second + 31*time.Second)
	r := n.Add(late)
	if len(r.Events) != 1 {
		t.Fatalf("event should finalize when the watermark passes the deadline, got %d", len(r.Events))
	}
	if r.Status != ObsLate {
		t.Fatalf("status %v, want late", r.Status)
	}
	e := r.Events[0]
	if e.FirstHop != mustHop(t, 0x11) || !e.Time.Equal(at(10*time.Second)) {
		t.Fatalf("late evidence leaked into the final event: hop %x time %v", e.FirstHop.Bytes(), e.Time)
	}
	if !e.FinalAt.Equal(at(40 * time.Second)) {
		t.Fatalf("FinalAt %v, want first arrival + settle", e.FinalAt)
	}
	if n.Stats().Late != 1 {
		t.Fatal("late observation must be counted")
	}
}

func TestNormalizerEarlierTimestampWithinSettleIsIncluded(t *testing.T) {
	n, _ := NewNormalizer(normCfg())
	n.Add(obs(t, "tx", "a", at(10*time.Second), RouteClassFlood, 1, 0x11))
	b := obs(t, "tx", "b", at(0), RouteClassFlood, 1, 0x22)
	b.ArrivedAt = at(20 * time.Second) // within the settle window
	if r := n.Add(b); r.Status != ObsAccepted {
		t.Fatal(r.Status)
	}
	evs := n.Flush().Events
	if len(evs) != 1 || evs[0].FirstHop != mustHop(t, 0x22) || !evs[0].Time.Equal(at(0)) {
		t.Fatalf("pending event must use the earliest observation seen before finality: %+v", evs)
	}
}

func TestNormalizerStreamingEqualsBatch(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 40; trial++ {
		var os []Observation
		for i := 0; i < 1+rng.Intn(8); i++ {
			o := obs(t, "tx", fmt.Sprintf("o%02d", i), at(time.Duration(rng.Intn(20))*time.Second), RouteClassFlood, 1, byte(rng.Intn(4)))
			if rng.Intn(3) == 0 {
				o.Path = Route{}
			}
			o.ArrivedAt = at(time.Duration(rng.Intn(60)) * time.Second)
			os = append(os, o)
		}
		batch, _, err := NormalizeTransmission(os, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := NewNormalizer(normCfg())
		sorted := append([]Observation(nil), os...)
		// arrival order
		for i := 1; i < len(sorted); i++ {
			for j := i; j > 0 && (sorted[j].ArrivedAt.Before(sorted[j-1].ArrivedAt) ||
				sorted[j].ArrivedAt.Equal(sorted[j-1].ArrivedAt) && sorted[j].ObsID < sorted[j-1].ObsID); j-- {
				sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			}
		}
		var evs []Event
		for _, o := range sorted {
			evs = append(evs, n.Add(o).Events...)
		}
		evs = append(evs, n.Flush().Events...)
		if len(evs) != 1 {
			t.Fatalf("trial %d: %d events", trial, len(evs))
		}
		got := evs[0]
		got.ExcludedObservations = batch.ExcludedObservations // streaming cannot know future late arrivals
		if !reflect.DeepEqual(got, batch) {
			t.Fatalf("trial %d: streaming %+v != batch %+v", trial, got, batch)
		}
	}
}

func TestNormalizerDuplicateAndConflict(t *testing.T) {
	n, _ := NewNormalizer(normCfg())
	a := obs(t, "tx", "a", at(0), RouteClassFlood, 1, 0x11)
	n.Add(a)
	if r := n.Add(a); r.Status != ObsDuplicate {
		t.Fatalf("identical observation: %v", r.Status)
	}
	b := a
	b.WireBytes++
	if r := n.Add(b); r.Status != ObsConflict {
		t.Fatalf("same ObsID, different content: %v", r.Status)
	}
	c := obs(t, "tx", "c", at(0), RouteClassFlood, 1, 0x11)
	c.Channel = mustChan(t, 0x43)
	if r := n.Add(c); r.Status != ObsConflict {
		t.Fatalf("different channel for the same TxID: %v", r.Status)
	}
	_, _, err := NormalizeTransmission([]Observation{a, c}, time.Minute)
	if err == nil {
		t.Fatal("batch must reject conflicting observations")
	}
}

func TestNormalizeRouteClassesAndTrace(t *testing.T) {
	mixed := []Observation{
		obs(t, "tx", "a", at(0), RouteClassFlood, 1, 0x11),
		obs(t, "tx", "b", at(time.Second), RouteClassTransportDirect, 0),
	}
	e, _, _ := NormalizeTransmission(mixed, time.Minute)
	if e.RouteClass != RouteClassMixed || StreamKeyOf(e).confidence() != ConfidenceLow {
		t.Fatalf("disagreeing route classes must be mixed / low confidence: %v", e.RouteClass)
	}
	direct := []Observation{obs(t, "d", "a", at(0), RouteClassDirect, 1, 0x33, 0x44)}
	e, _, _ = NormalizeTransmission(direct, time.Minute)
	if e.RouteKind != RouteKindDirect || !e.FirstHop.IsZero() {
		t.Fatalf("a direct path's next hop must not become a first hop: %v %x", e.RouteKind, e.FirstHop.Bytes())
	}
	tr := obs(t, "tr", "a", at(0), RouteClassFlood, 1, 0x55)
	tr.PayloadType = PayloadTypeTrace
	e, _, _ = NormalizeTransmission([]Observation{tr}, time.Minute)
	if e.RouteKind != RouteKindUnknown || !e.FirstHop.IsZero() {
		t.Fatal("TRACE paths hold SNR bytes and must not become route evidence")
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizerBoundsAreCountedAsReducedCoverage(t *testing.T) {
	cfg := normCfg()
	cfg.MaxPendingTransmissions = 2
	cfg.MaxObservationsPerTransmission = 2
	cfg.MaxFinalizedIDs = 2
	n, _ := NewNormalizer(cfg)
	for i := 0; i < 3; i++ {
		r := n.Add(obs(t, "fan", fmt.Sprintf("o%d", i), at(0), RouteClassFlood, 1, 0x01))
		if i == 2 && (r.Status != ObsDroppedFanout || !r.CoverageReduced) {
			t.Fatalf("fan-out cap: %v", r.Status)
		}
	}
	n.Add(obs(t, "p2", "o", at(time.Second), RouteClassFlood, 1, 0x01))
	r := n.Add(obs(t, "p3", "o", at(2*time.Second), RouteClassFlood, 1, 0x01))
	if len(r.Events) != 1 || !r.CoverageReduced {
		t.Fatalf("pending cap must force-finalize the earliest deadline: %d events", len(r.Events))
	}
	n.Flush()
	st := n.Stats()
	if st.DroppedFanout != 1 || st.ForcedFinalizations != 1 || st.FinalizedIDsEvictedEarly == 0 || !st.CoverageReduced() {
		t.Fatalf("stats %+v", st)
	}
	if st.FinalizedIDs > 2 || st.Pending != 0 {
		t.Fatalf("bounded sets exceeded: %+v", st)
	}
}

func TestNormalizerRejectsOutOfArrivalOrder(t *testing.T) {
	n, _ := NewNormalizer(normCfg())
	n.Add(obs(t, "a", "o", at(10*time.Second), RouteClassFlood, 1, 0x01))
	if r := n.Add(obs(t, "b", "o", at(5*time.Second), RouteClassFlood, 1, 0x01)); r.Status != ObsOutOfArrivalOrder {
		t.Fatalf("status %v", r.Status)
	}
}

func TestValidationErrorsDoNotEchoPrivateValues(t *testing.T) {
	secretID := "SECRET-TX-5f3a"
	o := obs(t, secretID, "OBS-SECRET-9c1d", at(0), RouteClassFlood, 1, 0xDE)
	o.WireBytes = 999
	err := o.Validate()
	if err == nil || !errors.Is(err, errInvalidObservation) {
		t.Fatal("expected invalid observation")
	}
	e := relayEvent(t, secretID, t0, 0xAB, 0xDE)
	e.WireBytes = 999
	err2 := e.Validate()
	for _, msg := range []string{err.Error(), err2.Error()} {
		for _, secret := range []string{secretID, "OBS-SECRET", "de", "ab", "DE", "AB"} {
			if strings.Contains(strings.ToLower(msg), strings.ToLower(secret)) && secret != "de" && secret != "ab" {
				t.Fatalf("error leaks %q: %s", secret, msg)
			}
		}
		if strings.Contains(msg, "5f3a") || strings.Contains(msg, "9c1d") {
			t.Fatalf("error leaks id: %s", msg)
		}
	}
}

func TestNormalizerAdvanceReleasesDueTransmissions(t *testing.T) {
	n, _ := NewNormalizer(normCfg())
	n.Add(obs(t, "tx", "a", at(0), RouteClassFlood, 1, 0x11))
	if r := n.Advance(at(29 * time.Second)); len(r.Events) != 0 {
		t.Fatal("released before the deadline")
	}
	if r := n.Advance(at(30 * time.Second)); len(r.Events) != 0 {
		t.Fatal("released at the deadline: an observation may still arrive at that instant")
	}
	r := n.Advance(at(31 * time.Second))
	if len(r.Events) != 1 || !r.Events[0].FinalAt.Equal(at(30*time.Second)) {
		t.Fatalf("advance must release at first arrival + settle: %+v", r.Events)
	}
	if r := n.Advance(at(10 * time.Second)); len(r.Events) != 0 {
		t.Fatal("going back in time must be ignored")
	}
}
