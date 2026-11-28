package anomaly

import (
	"container/heap"
	"errors"
	"fmt"
	"sort"
	"time"
)

// NormalizerConfig bounds the Normalizer. Every field is required.
//
// Finality policy: a transmission is final at (first arrival + SettleWindow).
// Observations that arrive at or before that deadline are included, even if
// their data timestamp is earlier than observations already seen (which can
// change the chosen first hop while the event is still pending). The event
// is released only once the arrival watermark passes the deadline. An
// observation of a transmission that is already final is rejected as late
// and never changes the released event, so a replay can never use route
// evidence that arrived after the event was decided.
type NormalizerConfig struct {
	// SettleWindow is how long after its first observation arrives a
	// transmission may still collect observations.
	SettleWindow time.Duration
	// MaxPendingTransmissions caps transmissions waiting for their deadline.
	// When full, the pending transmission with the earliest deadline is
	// finalized early (counted as a forced finalization).
	MaxPendingTransmissions int
	// MaxObservationsPerTransmission caps the observation fan-out kept per
	// transmission. Further observations are dropped and counted.
	MaxObservationsPerTransmission int
	// FinalizedHorizon is how long (in arrival time) finalized IDs are
	// remembered to reject late observations. After it, a late observation
	// would start a new pending transmission with the same ID, which the
	// Detector's own dedup must then catch.
	FinalizedHorizon time.Duration
	// MaxFinalizedIDs caps the remembered finalized IDs. Evicting one before
	// its horizon is counted as reduced coverage.
	MaxFinalizedIDs int
}

// Bounds on the Normalizer's durations (they also keep nanosecond
// arithmetic far from overflow).
const (
	maxSettleWindow     = 24 * time.Hour
	maxFinalizedHorizon = 30 * 24 * time.Hour
)

// Validate checks that every limit is set explicitly.
func (c NormalizerConfig) Validate() error {
	switch {
	case c.SettleWindow <= 0 || c.SettleWindow > maxSettleWindow:
		return errors.New("anomaly: NormalizerConfig.SettleWindow must be in (0, 24h]")
	case c.FinalizedHorizon > maxFinalizedHorizon:
		return errors.New("anomaly: NormalizerConfig.FinalizedHorizon must be <= 30 days")
	case c.MaxPendingTransmissions <= 0:
		return errors.New("anomaly: NormalizerConfig.MaxPendingTransmissions must be > 0")
	case c.MaxObservationsPerTransmission <= 0:
		return errors.New("anomaly: NormalizerConfig.MaxObservationsPerTransmission must be > 0")
	case c.FinalizedHorizon < c.SettleWindow:
		return errors.New("anomaly: NormalizerConfig.FinalizedHorizon must be >= SettleWindow")
	case c.MaxFinalizedIDs <= 0:
		return errors.New("anomaly: NormalizerConfig.MaxFinalizedIDs must be > 0")
	}
	return nil
}

// ObsStatus is the outcome of Normalizer.Add.
type ObsStatus uint8

const (
	ObsAccepted          ObsStatus = iota + 1
	ObsDuplicate                   // identical observation already held
	ObsConflict                    // same TxID/ObsID with different content, or payload mismatch with any held observation
	ObsLate                        // the transmission is already final
	ObsInvalid                     // failed Observation.Validate
	ObsOutOfArrivalOrder           // ArrivedAt earlier than the arrival watermark
	ObsDroppedFanout               // MaxObservationsPerTransmission reached
)

var obsStatusNames = [...]string{"", "accepted", "duplicate", "conflict", "late", "invalid", "out_of_arrival_order", "dropped_fanout"}

func (s ObsStatus) String() string {
	if int(s) < len(obsStatusNames) && s > 0 {
		return obsStatusNames[s]
	}
	return fmt.Sprintf("obsstatus(%d)", uint8(s))
}

// NormalizeResult is returned by Normalizer.Add, Advance and Flush.
type NormalizeResult struct {
	Status ObsStatus // zero for Advance/Flush
	Err    error     // set for ObsInvalid
	// Events are the transmissions finalized by this call, in deadline order.
	Events []Event
	// CoverageReduced is true when a capacity limit dropped or truncated
	// evidence during this call.
	CoverageReduced bool
}

// NormalizerStats are cumulative counters.
type NormalizerStats struct {
	Accepted, Duplicates, Conflicts, Late, Invalid, OutOfArrivalOrder uint64
	DroppedFanout                                                     uint64
	ForcedFinalizations                                               uint64
	FinalizedIDsEvictedEarly                                          uint64
	EventsFinalized                                                   uint64
	Pending, FinalizedIDs                                             int
}

// CoverageReduced reports whether any capacity limit has cut evidence.
func (s NormalizerStats) CoverageReduced() bool {
	return s.DroppedFanout > 0 || s.ForcedFinalizations > 0 || s.FinalizedIDsEvictedEarly > 0
}

type pendingTx struct {
	id           TxID
	firstArrival int64
	deadline     int64
	obs          []*Observation
	idx          int // heap index
	// payload is the first observation with a known payload size; every
	// later known size must agree with it
	payload *Observation
	// minWire is the smallest known frame size held; a known payload must
	// fit every held frame
	minWire   int
	dropped   int  // observations dropped by the fan-out cap
	truncated bool // fan-out drop or forced early finalization
}

func (p *pendingTx) compatible(o *Observation) bool {
	if !compatible(p.obs[0], o) || !fitsFrame(o, p.minWire) {
		return false
	}
	return p.payload == nil || compatible(p.payload, o)
}

func (p *pendingTx) add(o *Observation) {
	p.obs = append(p.obs, o)
	if p.payload == nil && o.PayloadKnown {
		p.payload = o
	}
	if o.WireBytes > 0 && (p.minWire == 0 || o.WireBytes < p.minWire) {
		p.minWire = o.WireBytes
	}
}

type pendingHeap []*pendingTx

func (h pendingHeap) Len() int { return len(h) }
func (h pendingHeap) Less(i, j int) bool {
	if h[i].deadline != h[j].deadline {
		return h[i].deadline < h[j].deadline
	}
	return h[i].id < h[j].id
}
func (h pendingHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i]; h[i].idx = i; h[j].idx = j }
func (h *pendingHeap) Push(x interface{}) { p := x.(*pendingTx); p.idx = len(*h); *h = append(*h, p) }
func (h *pendingHeap) Pop() interface{} {
	old := *h
	p := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return p
}

type finalizedRec struct {
	id TxID
	at int64
}

// Normalizer turns observations, fed in arrival order, into final Events. It
// is deterministic, holds no goroutines or timers and never reads the wall
// clock. It is not safe for concurrent use; use one Normalizer per goroutine.
type Normalizer struct {
	cfg          NormalizerConfig
	watermark    int64
	started      bool
	pending      map[TxID]*pendingTx
	deadlines    pendingHeap
	finalized    map[TxID]int64
	finalizedLog []finalizedRec // FIFO by finalization time; head at finalizedHead
	finalizedHd  int
	stats        NormalizerStats
}

// NewNormalizer validates cfg and returns an empty Normalizer.
func NewNormalizer(cfg NormalizerConfig) (*Normalizer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Normalizer{cfg: cfg, pending: make(map[TxID]*pendingTx), finalized: make(map[TxID]int64)}, nil
}

// Add ingests one observation. ArrivedAt (or Time when ArrivedAt is zero)
// must not be earlier than the latest arrival already seen.
func (n *Normalizer) Add(o Observation) NormalizeResult {
	var res NormalizeResult
	if err := o.Validate(); err != nil {
		n.stats.Invalid++
		res.Status, res.Err = ObsInvalid, err
		return res
	}
	a := o.arrived().UnixNano()
	if n.started && a < n.watermark {
		n.stats.OutOfArrivalOrder++
		res.Status = ObsOutOfArrivalOrder
		return res
	}
	n.advanceTo(a, &res)
	if _, done := n.finalized[o.TxID]; done {
		n.stats.Late++
		res.Status = ObsLate
		return res
	}
	p := n.pending[o.TxID]
	if p == nil {
		if len(n.pending) >= n.cfg.MaxPendingTransmissions {
			oldest := heap.Pop(&n.deadlines).(*pendingTx)
			oldest.truncated = true
			n.finalize(oldest, n.watermark, &res)
			n.stats.ForcedFinalizations++
			res.CoverageReduced = true
		}
		p = &pendingTx{id: o.TxID, firstArrival: a, deadline: a + int64(n.cfg.SettleWindow)}
		n.pending[o.TxID] = p
		heap.Push(&n.deadlines, p)
	} else {
		if !p.compatible(&o) {
			n.stats.Conflicts++
			res.Status = ObsConflict
			return res
		}
		for _, h := range p.obs {
			if h.ObsID == o.ObsID {
				if sameObservation(h, &o) {
					n.stats.Duplicates++
					res.Status = ObsDuplicate
				} else {
					n.stats.Conflicts++
					res.Status = ObsConflict
				}
				return res
			}
		}
		if len(p.obs) >= n.cfg.MaxObservationsPerTransmission {
			p.dropped++
			p.truncated = true
			n.stats.DroppedFanout++
			res.Status = ObsDroppedFanout
			res.CoverageReduced = true
			return res
		}
	}
	oc := o
	oc.Path = Route{w: o.Path.w, hops: append([]byte(nil), o.Path.hops...)}
	p.add(&oc)
	n.stats.Accepted++
	res.Status = ObsAccepted
	return res
}

// Advance moves the arrival watermark to t (if later) and releases every
// transmission whose deadline has passed.
func (n *Normalizer) Advance(t time.Time) NormalizeResult {
	var res NormalizeResult
	if !validTime(t) {
		return res
	}
	a := t.UnixNano()
	if n.started && a < n.watermark {
		return res
	}
	n.advanceTo(a, &res)
	return res
}

// Flush finalizes every pending transmission at its deadline (end of input).
func (n *Normalizer) Flush() NormalizeResult {
	var res NormalizeResult
	for n.deadlines.Len() > 0 {
		p := heap.Pop(&n.deadlines).(*pendingTx)
		n.finalize(p, p.deadline, &res)
	}
	return res
}

// Stats returns cumulative counters.
func (n *Normalizer) Stats() NormalizerStats {
	s := n.stats
	s.Pending = len(n.pending)
	s.FinalizedIDs = len(n.finalized)
	return s
}

func (n *Normalizer) advanceTo(a int64, res *NormalizeResult) {
	n.started = true
	if a > n.watermark {
		n.watermark = a
	}
	for n.deadlines.Len() > 0 && n.deadlines[0].deadline < n.watermark {
		p := heap.Pop(&n.deadlines).(*pendingTx)
		n.finalize(p, p.deadline, res)
	}
	horizon := n.watermark - int64(n.cfg.FinalizedHorizon)
	for n.finalizedHd < len(n.finalizedLog) && n.finalizedLog[n.finalizedHd].at < horizon {
		n.dropFinalizedHead()
	}
	n.compactFinalized()
}

func (n *Normalizer) finalize(p *pendingTx, at int64, res *NormalizeResult) {
	delete(n.pending, p.id)
	ev := buildEvent(p.obs, time.Unix(0, at).UTC(), p.dropped)
	ev.Truncated = p.truncated
	res.Events = append(res.Events, ev)
	n.stats.EventsFinalized++
	n.finalized[p.id] = at
	n.finalizedLog = append(n.finalizedLog, finalizedRec{id: p.id, at: at})
	for len(n.finalized) > n.cfg.MaxFinalizedIDs {
		if n.finalizedLog[n.finalizedHd].at >= n.watermark-int64(n.cfg.FinalizedHorizon) {
			n.stats.FinalizedIDsEvictedEarly++
			res.CoverageReduced = true
		}
		n.dropFinalizedHead()
	}
}

func (n *Normalizer) dropFinalizedHead() {
	r := n.finalizedLog[n.finalizedHd]
	n.finalizedLog[n.finalizedHd] = finalizedRec{}
	n.finalizedHd++
	if at, ok := n.finalized[r.id]; ok && at == r.at {
		delete(n.finalized, r.id)
	}
}

func (n *Normalizer) compactFinalized() {
	if n.finalizedHd > 1024 && n.finalizedHd*2 > len(n.finalizedLog) {
		n.finalizedLog = append([]finalizedRec(nil), n.finalizedLog[n.finalizedHd:]...)
		n.finalizedHd = 0
	}
}

// BatchReport describes what NormalizeTransmission included.
type BatchReport struct {
	Included, ExcludedLate, Duplicates int
}

// NormalizeTransmission applies the Normalizer's finality policy to all
// observations of ONE transmission at once (offline use). Observations are
// taken in arrival order (ArrivedAt, then ObsID); those arriving after
// (first arrival + settle) are excluded, exactly as the streaming Normalizer
// would reject them as late, before any conflict check. It returns an error
// if the observations belong to different transmissions, or if included
// observations conflict (where the streaming Normalizer reports
// ObsConflict).
func NormalizeTransmission(obs []Observation, settle time.Duration) (Event, BatchReport, error) {
	var rep BatchReport
	if len(obs) == 0 {
		return Event{}, rep, errors.New("anomaly: no observations")
	}
	if settle <= 0 {
		return Event{}, rep, errors.New("anomaly: settle window must be > 0")
	}
	ptrs := make([]*Observation, len(obs))
	for i := range obs {
		if err := obs[i].Validate(); err != nil {
			return Event{}, rep, fmt.Errorf("observation %d: %w", i, err)
		}
		ptrs[i] = &obs[i]
	}
	sort.Slice(ptrs, func(i, j int) bool {
		ai, aj := ptrs[i].arrived(), ptrs[j].arrived()
		if !ai.Equal(aj) {
			return ai.Before(aj)
		}
		return ptrs[i].ObsID < ptrs[j].ObsID
	})
	first := ptrs[0]
	deadline := first.arrived().Add(settle)
	p := &pendingTx{id: first.TxID}
	for i, o := range ptrs {
		if o.TxID != first.TxID {
			return Event{}, rep, fmt.Errorf("anomaly: observation %d belongs to another transmission", i)
		}
		if o.arrived().After(deadline) {
			rep.ExcludedLate++
			continue
		}
		if len(p.obs) > 0 && !p.compatible(o) {
			return Event{}, rep, fmt.Errorf("anomaly: observation %d conflicts", i)
		}
		dup := false
		for _, h := range p.obs {
			if h.ObsID == o.ObsID {
				if !sameObservation(h, o) {
					return Event{}, rep, fmt.Errorf("anomaly: observation %d reuses an ObsID with different content", i)
				}
				dup = true
			}
		}
		if dup {
			rep.Duplicates++
			continue
		}
		p.add(o)
	}
	rep.Included = len(p.obs)
	return buildEvent(p.obs, deadline, rep.ExcludedLate), rep, nil
}
