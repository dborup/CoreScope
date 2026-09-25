package anomaly

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// Kind is the detector family that produced a candidate.
type Kind uint8

const (
	KindRate Kind = iota + 1
	KindNewStream
	KindPeriodic
)

var kindNames = [...]string{"", "rate", "new_stream", "periodic"}

func (k Kind) String() string {
	if int(k) < len(kindNames) && k > 0 {
		return kindNames[k]
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// State is an episode state.
type State uint8

const (
	StateNormal State = iota
	StateSuspicious
	StateActive
	StateEnded
)

var stateNames = [...]string{"normal", "suspicious", "active", "ended"}

func (s State) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "state(" + strconv.Itoa(int(s)) + ")"
}

// Reason is a machine-readable transition reason.
type Reason uint8

const (
	// ReasonThresholdCrossed: normal -> suspicious, the rule became hot.
	ReasonThresholdCrossed Reason = iota + 1
	// ReasonConfirmed: suspicious -> active after State.ConfirmAfter.
	ReasonConfirmed
	// ReasonNotConfirmed: suspicious -> normal, quiet before confirmation.
	ReasonNotConfirmed
	// ReasonQuiet: active -> ended after State.EndAfterQuiet without a hot
	// signal.
	ReasonQuiet
	// ReasonReopened: ended -> active, hot again within State.Cooldown.
	ReasonReopened
	// ReasonEvicted: the key's state was evicted by a capacity limit while
	// the episode was open; the episode is closed without a quiet period.
	ReasonEvicted
)

var reasonNames = [...]string{"", "threshold_crossed", "confirmed", "not_confirmed", "quiet", "reopened", "evicted"}

func (r Reason) String() string {
	if int(r) < len(reasonNames) && r > 0 {
		return reasonNames[r]
	}
	return "reason(" + strconv.Itoa(int(r)) + ")"
}

// legalTransition is the only place that defines which transitions exist.
func legalTransition(from, to State, r Reason) bool {
	switch {
	case from == StateNormal && to == StateSuspicious && r == ReasonThresholdCrossed:
	case from == StateSuspicious && to == StateActive && r == ReasonConfirmed:
	case from == StateSuspicious && to == StateNormal && (r == ReasonNotConfirmed || r == ReasonEvicted):
	case from == StateActive && to == StateEnded && (r == ReasonQuiet || r == ReasonEvicted):
	case from == StateEnded && to == StateActive && r == ReasonReopened:
	default:
		return false
	}
	return true
}

// CoverageFlags mark evidence that capacity limits may have cut.
type CoverageFlags uint16

const (
	// CoverageWindowSaturated: the key's window merged entries (counts may
	// be overstated).
	CoverageWindowSaturated CoverageFlags = 1 << iota
	// CoverageKeyEvictions: the scope evicted keys for capacity within the
	// last StateTTL, so a "new" key may have existed before.
	CoverageKeyEvictions
	// CoverageDedupEvictedEarly: dedup IDs were evicted before the horizon
	// within the last DedupHorizon; a repeat may have been counted twice.
	CoverageDedupEvictedEarly
	// CoverageReorderForced: the reorder buffer overflowed recently; late
	// events may have been rejected.
	CoverageReorderForced
	// CoverageInputTruncated: an event within the last DedupHorizon had
	// Truncated set (its normalization dropped observations for capacity),
	// so its first hop may rest on incomplete route evidence.
	CoverageInputTruncated
)

// PeriodAlternative is another period that explains a comparable chain.
type PeriodAlternative struct {
	Period   time.Duration
	Support  int
	Coverage float64
}

// RateEvidence is attached to rate candidates.
type RateEvidence struct {
	Window    time.Duration
	Threshold int
	Count     uint64 // events in (t-Window, t], including expected ones
	Expected  uint64
	Monitored uint64
	Saturated bool
}

// NewStreamEvidence is attached to new-stream candidates.
type NewStreamEvidence struct {
	Window      time.Duration
	MinCount    int
	Count       int
	Age         time.Duration // time from the episode's first event
	Censored    bool          // first seen within CensorWindow of HistoryStart: may predate the data
	Reactivated bool          // seen before, silent for QuietPeriod, active again
}

// PeriodicEvidence is attached to periodic candidates.
type PeriodicEvidence struct {
	// Period is the hypothesis period. It is fixed when the hypothesis is
	// formed (a recent gap divided by k, replaced by the phase estimate
	// chain span / periods spanned when that explains at least as much) and
	// kept while it explains every new gap within Tolerance; it is not
	// re-fitted on every pulse.
	Period time.Duration
	// Tolerance is the per-gap tolerance max(JitterAbs, JitterRel*Period).
	Tolerance time.Duration
	// JitterMedian is the median |gap - k*Period| over the chain. It is a
	// gap measure: independent per-pulse jitter shows up roughly doubled.
	JitterMedian time.Duration
	Support      int // pulses in the chain
	Events       int // events merged into those pulses
	Coverage     float64
	// Chance is an upper bound (union bound) on the expected number of
	// equally long chains under a Poisson null with the key's local pulse
	// rate, over all pulses evaluated on the key so far and every candidate
	// period the search may try; one gap is discounted for fitting Period.
	Chance       float64
	SufficientAt time.Time // first time the evidence met every criterion
	Alternatives []PeriodAlternative
}

// EpisodeSummary is attached to ended / not-confirmed / evicted candidates.
type EpisodeSummary struct {
	ActiveSince time.Time // zero if never active
	LastHot     time.Time
	HotSignals  uint32
}

// Candidate is one state transition of one episode. Only transitions are
// emitted: repeated hot signals inside an episode are counted, not emitted.
type Candidate struct {
	// EventID is stable across replays: it depends only on the rule name,
	// the key encoding and the episode start time.
	EventID  string
	Rule     string
	Kind     Kind
	Key      Key
	From, To State
	Reason   Reason
	At       time.Time // data time of the transition
	// DecidedAt is the detector's decision clock when the transition was
	// made: the latest FinalAt of every event offered so far, or the latest
	// Advance time (and >= At). It is therefore >= the FinalAt of every
	// event behind the decision. When events are offered in the order they
	// became final (as Normalizer releases them), it is the time a live
	// detector would have made the decision.
	DecidedAt    time.Time
	EpisodeStart time.Time
	Confidence   Confidence
	Traffic      TrafficLabel
	Suppressed   bool
	Coverage     CoverageFlags
	// TriggerID is the caller's ID of the event that caused the transition
	// (empty for time-driven transitions).
	TriggerID TxID
	// TriggerRoute is the full route of that event, as separate evidence.
	// For channel and global scope it only describes the event that crossed
	// the threshold; it is not an attribution of the anomaly.
	TriggerRoute Route
	Rate         *RateEvidence      `json:",omitempty"`
	NewStream    *NewStreamEvidence `json:",omitempty"`
	Periodic     *PeriodicEvidence  `json:",omitempty"`
	Summary      *EpisodeSummary    `json:",omitempty"`
}

func episodeID(rule string, k Key, start int64) string {
	h := sha256.New()
	h.Write([]byte(rule))
	h.Write([]byte{0})
	h.Write([]byte(k.String()))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(start))
	h.Write([]byte{0})
	h.Write(b[:])
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// ---- deterministic JSON ----

func (k Kind) MarshalJSON() ([]byte, error)         { return json.Marshal(k.String()) }
func (s State) MarshalJSON() ([]byte, error)        { return json.Marshal(s.String()) }
func (r Reason) MarshalJSON() ([]byte, error)       { return json.Marshal(r.String()) }
func (s Scope) MarshalJSON() ([]byte, error)        { return json.Marshal(s.String()) }
func (c Confidence) MarshalJSON() ([]byte, error)   { return json.Marshal(c.String()) }
func (l TrafficLabel) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }
func (k Key) MarshalJSON() ([]byte, error)          { return json.Marshal(k.String()) }

// MarshalJSON encodes a route as "<width>:<hex>" ("-" when empty).
func (r Route) MarshalJSON() ([]byte, error) {
	if r.w == 0 {
		return json.Marshal("-")
	}
	return json.Marshal(strconv.Itoa(int(r.w)) + ":" + hex.EncodeToString(r.hops))
}

// MarshalJSON lists the set flags by name.
func (f CoverageFlags) MarshalJSON() ([]byte, error) {
	names := []string{}
	for i, n := range []string{"window_saturated", "key_evictions", "dedup_evicted_early", "reorder_forced", "input_truncated"} {
		if f&(1<<i) != 0 {
			names = append(names, n)
		}
	}
	return json.Marshal(names)
}
