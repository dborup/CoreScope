package anomaly

import (
	"container/heap"
	"time"
)

func nsTime(ns int64) time.Time { return time.Unix(0, ns).UTC() }

// Status is the outcome of Detector.Observe.
type Status uint8

const (
	StatusAccepted  Status = iota + 1 // accepted (it may still wait in the reorder buffer)
	StatusDuplicate                   // ID already counted within the dedup horizon
	StatusLate                        // older than the processed frontier
	StatusInvalid                     // failed Event.Validate
)

var statusNames = [...]string{"", "accepted", "duplicate", "late", "invalid"}

func (s Status) String() string {
	if int(s) < len(statusNames) && s > 0 {
		return statusNames[s]
	}
	return "status(?)"
}

// Result is returned by Observe, Advance, Flush and Drain.
type Result struct {
	Status Status // zero for Advance, Flush and Drain
	Err    error  // set for StatusInvalid
	// Candidates are transitions in the order they happened in data time.
	Candidates []Candidate
	// More is true when further candidates wait in the buffer (Drain).
	More bool
	// CoverageReduced is true when a capacity limit cut evidence or dropped
	// candidates during this call.
	CoverageReduced bool
}

// Stats are cumulative counters and current sizes.
type Stats struct {
	Accepted, Duplicates, DuplicateConflicts, Late, Invalid uint64
	Processed                                               uint64
	// TruncatedInputs counts accepted events with Event.Truncated set.
	TruncatedInputs uint64
	// InvalidAdvance counts Advance calls with a time outside the valid range.
	InvalidAdvance    uint64
	Buffered          int
	DedupIDs          int
	DedupEvictedEarly uint64
	// EffectiveDedupHorizon is the data-time span the dedup set currently
	// covers; below Limits.DedupHorizon when capacity forced evictions.
	EffectiveDedupHorizon time.Duration
	Keys                  [numScopes]int
	CapacityEvictions     [numScopes]uint64
	TTLEvictions          [numScopes]uint64
	EpisodesEvicted       uint64
	WindowMerges          uint64
	ReorderForced         uint64
	CandidatesEmitted     uint64
	CandidatesDropped     uint64
	PendingCandidates     int
	MaxCandidateBurst     int
	Frontier              time.Time
}

// CoverageReduced reports whether any capacity limit has cut evidence.
func (s Stats) CoverageReduced() bool {
	if s.DedupEvictedEarly > 0 || s.EpisodesEvicted > 0 || s.WindowMerges > 0 || s.ReorderForced > 0 || s.CandidatesDropped > 0 ||
		s.TruncatedInputs > 0 {
		return true
	}
	for _, n := range s.CapacityEvictions {
		if n > 0 {
			return true
		}
	}
	return false
}

// Detector is a deterministic, single-goroutine anomaly detector. Separate
// Detectors share nothing and may run on separate goroutines. It holds no
// timers and never reads the wall clock: time moves only with event times
// and with Advance.
type Detector struct {
	cfg        Config
	scopes     [numScopes]*keyTable
	historySet bool
	history    int64

	maxSeen  int64
	frontier int64 // latest data time processed (events and deadlines)
	started  bool
	// clock is the decision clock: the latest FinalAt of any valid event
	// offered, or the latest Advance time. Candidates are stamped with it.
	clock int64
	// the last processed event, so a tie at the frontier is ordered exactly
	// as if both events had waited in the reorder buffer
	lastEvT  int64
	lastEvID TxID
	evDone   bool

	dedup     map[TxID]int64
	dedupLog  []dedupRec
	dedupHead int

	reorder   eventHeap
	deadlines deadlineHeap

	pending     []Candidate
	pendingHead int

	lastDedupEarly, lastReorderForced, lastTruncated int64
	stats                                            Stats
	trBuf                                            []transition
	perScratch                                       periodicScratch
	callReduced                                      bool // a capacity limit cut evidence during the current call
}

type dedupRec struct {
	id TxID
	t  int64
}

// New validates cfg and returns a Detector. The config is copied.
func New(cfg Config) (*Detector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := cfg
	c.Rate = append([]RateRule(nil), cfg.Rate...)
	c.NewStream = append([]NewStreamRule(nil), cfg.NewStream...)
	c.Periodic = append([]PeriodicRule(nil), cfg.Periodic...)
	d := &Detector{cfg: c, dedup: make(map[TxID]int64)}
	for i := range c.Rate {
		d.table(c.Rate[i].Scope).addRate(i, c.Rate[i].Window)
	}
	for i := range c.NewStream {
		d.table(c.NewStream[i].Scope).rules.newStream = append(d.table(c.NewStream[i].Scope).rules.newStream, i)
	}
	for i := range c.Periodic {
		d.table(c.Periodic[i].Scope).rules.periodic = append(d.table(c.Periodic[i].Scope).rules.periodic, i)
	}
	if !c.HistoryStart.IsZero() {
		d.history, d.historySet = c.HistoryStart.UnixNano(), true
	}
	return d, nil
}

func (d *Detector) table(s Scope) *keyTable {
	if d.scopes[s] == nil {
		d.scopes[s] = newKeyTable(s)
	}
	return d.scopes[s]
}

// Observe offers one final event. Events must arrive in data-time order, or
// out of order by at most Limits.ReorderDelay; anything older than the
// processed frontier is rejected as late. Candidates may be returned by a
// later call because of the reorder delay and the per-call cap.
func (d *Detector) Observe(e Event) Result {
	var res Result
	if err := e.Validate(); err != nil {
		d.stats.Invalid++
		res.Status, res.Err = StatusInvalid, err
		return res
	}
	t := e.Time.UnixNano()
	if f := e.finalAt().UnixNano(); f > d.clock {
		d.clock = f
	}
	if prev, ok := d.dedup[e.ID]; ok {
		d.stats.Duplicates++
		if prev != t {
			d.stats.DuplicateConflicts++
		}
		res.Status = StatusDuplicate
		return d.finish(res)
	}
	// With ReorderDelay > 0, a tie at the frontier that was already processed
	// (e.g. by Advance) would be reordered by accepting a smaller ID; with 0
	// every event is processed on arrival, so ties keep arrival order.
	tieLate := d.cfg.Limits.ReorderDelay > 0 && d.evDone && t == d.lastEvT && e.ID <= d.lastEvID
	if d.started && (t < d.frontier || tieLate) {
		d.stats.Late++
		res.Status = StatusLate
		return d.finish(res)
	}
	d.started = true
	if !d.historySet {
		d.history, d.historySet = t, true
	}
	d.rememberID(e.ID, t)
	d.stats.Accepted++
	if e.Truncated {
		d.stats.TruncatedInputs++
		d.lastTruncated = maxInt64(d.lastTruncated, t)
		d.callReduced = true
	}
	res.Status = StatusAccepted
	// the event may wait in the reorder buffer: never alias caller memory
	e.FullRoute = Route{w: e.FullRoute.w, hops: append([]byte(nil), e.FullRoute.hops...)}
	d.reorder.push(e)
	if t > d.maxSeen {
		d.maxSeen = t
	}
	if d.reorder.Len() > d.cfg.Limits.MaxReorderBuffer {
		// force the oldest out; it is still processed in order
		d.stats.ReorderForced++
		d.lastReorderForced = maxInt64(d.lastReorderForced, t)
		d.callReduced = true
		d.process(d.reorder[0].Time.UnixNano(), true, false)
	}
	d.process(d.maxSeen-int64(d.cfg.Limits.ReorderDelay), false, false)
	return d.finish(res)
}

// Advance tells the detector that data time has reached t: events earlier
// than t-ReorderDelay will not arrive any more, so held events and due
// deadlines up to that point are processed. Time is taken to have passed
// continuously: an item at data time x is decided at the earliest time an
// Advance could have released it, max(clock, x+ReorderDelay), and the
// decision clock ends at t. t earlier than already known is ignored; t
// outside the valid time range is ignored and counted in
// Stats.InvalidAdvance.
func (d *Detector) Advance(t time.Time) Result {
	var res Result
	if !validTime(t) {
		d.stats.InvalidAdvance++
	} else {
		ns := t.UnixNano()
		if ns > d.maxSeen {
			d.maxSeen = ns
		}
		if d.started {
			d.process(d.maxSeen-int64(d.cfg.Limits.ReorderDelay), false, true)
		}
		if ns > d.clock {
			d.clock = ns
		}
	}
	return d.finish(res)
}

// Flush processes every held event (end of input) and the deadlines up to
// the latest event time. It does not invent time beyond the data. Like
// Advance, it stamps each released item no earlier than its data time +
// ReorderDelay, the earliest time a live detector could have released it.
func (d *Detector) Flush() Result {
	var res Result
	if d.started {
		d.process(d.maxSeen, false, true)
	}
	return d.finish(res)
}

// Drain returns buffered candidates without processing anything.
func (d *Detector) Drain() Result { return d.finish(Result{}) }

// Snapshot returns counters and current sizes.
func (d *Detector) Snapshot() Stats {
	s := d.stats
	s.Buffered = d.reorder.Len()
	s.DedupIDs = len(d.dedup)
	if d.dedupHead < len(d.dedupLog) {
		s.EffectiveDedupHorizon = time.Duration(d.maxSeen - d.dedupLog[d.dedupHead].t)
	}
	for sc, tb := range d.scopes {
		if tb != nil {
			s.Keys[sc] = len(tb.m)
			s.CapacityEvictions[sc] = tb.capacityEvictions
			s.TTLEvictions[sc] = tb.ttlEvictions
		}
	}
	s.PendingCandidates = len(d.pending) - d.pendingHead
	if d.started {
		s.Frontier = nsTime(d.frontier)
	}
	return s
}

func (d *Detector) finish(res Result) Result {
	res.CoverageReduced = res.CoverageReduced || d.callReduced
	d.callReduced = false
	n := len(d.pending) - d.pendingHead
	if n > d.cfg.Limits.MaxCandidatesPerCall {
		n = d.cfg.Limits.MaxCandidatesPerCall
	}
	if n > 0 {
		res.Candidates = append(res.Candidates, d.pending[d.pendingHead:d.pendingHead+n]...)
		for i := d.pendingHead; i < d.pendingHead+n; i++ {
			d.pending[i] = Candidate{}
		}
		d.pendingHead += n
	}
	res.More = d.pendingHead < len(d.pending)
	if d.pendingHead == len(d.pending) {
		d.pending, d.pendingHead = d.pending[:0], 0
	}
	if len(res.Candidates) > d.stats.MaxCandidateBurst {
		d.stats.MaxCandidateBurst = len(res.Candidates)
	}
	return res
}

func (d *Detector) rememberID(id TxID, t int64) {
	lim := &d.cfg.Limits
	// expire by horizon
	lo := d.maxSeen - int64(lim.DedupHorizon)
	if t > d.maxSeen {
		lo = t - int64(lim.DedupHorizon)
	}
	for d.dedupHead < len(d.dedupLog) && d.dedupLog[d.dedupHead].t < lo {
		d.dropDedupHead()
	}
	d.dedup[id] = t
	d.dedupLog = append(d.dedupLog, dedupRec{id: id, t: t})
	for len(d.dedup) > lim.MaxDedupIDs {
		d.stats.DedupEvictedEarly++
		d.callReduced = true
		d.lastDedupEarly = maxInt64(d.lastDedupEarly, t)
		d.dropDedupHead()
	}
	if d.dedupHead >= 1024 && d.dedupHead*2 >= len(d.dedupLog) {
		d.dedupLog = append([]dedupRec(nil), d.dedupLog[d.dedupHead:]...)
		d.dedupHead = 0
	}
}

func (d *Detector) dropDedupHead() {
	r := d.dedupLog[d.dedupHead]
	d.dedupLog[d.dedupHead] = dedupRec{}
	d.dedupHead++
	if t, ok := d.dedup[r.id]; ok && t == r.t {
		delete(d.dedup, r.id)
	}
}

// process handles held events and deadlines in data-time order up to
// horizon. Deadlines at or before an event's time run before the event.
// With forceOne it releases exactly the oldest held event (and the
// deadlines at or before it), whatever the horizon. With tick (Advance and
// Flush) the decision clock moves with the items: to at least
// x+ReorderDelay for an item at data time x.
func (d *Detector) process(horizon int64, forceOne, tick bool) {
	for {
		d.dropStaleDeadlines()
		hasEv, hasDl := d.reorder.Len() > 0, d.deadlines.Len() > 0
		var evT, dlT int64
		if hasEv {
			evT = d.reorder[0].Time.UnixNano()
			if forceOne {
				horizon = evT
			}
		}
		if hasDl {
			dlT = d.deadlines[0].at
		}
		if hasDl && dlT <= horizon && (!hasEv || dlT <= evT) {
			dl := heap.Pop(&d.deadlines).(deadline)
			if tick {
				d.tickTo(dl.at)
			}
			d.advanceFrontier(dl.at)
			d.runDeadline(dl)
			continue
		}
		if hasEv && evT <= horizon {
			e := d.reorder.pop()
			if tick {
				d.tickTo(evT)
			}
			d.advanceFrontier(evT)
			d.handle(&e)
			if forceOne {
				return
			}
			continue
		}
		return
	}
}

// tickTo moves the decision clock to the earliest time an Advance could
// have released an item at data time x.
func (d *Detector) tickTo(x int64) {
	if c := x + int64(d.cfg.Limits.ReorderDelay); c > d.clock {
		d.clock = c
	}
}

func (d *Detector) advanceFrontier(t int64) {
	if t > d.frontier {
		d.frontier = t
	}
	d.expireKeys(d.frontier)
}

// ---- key tables ----

type scopeRules struct {
	rate      []int   // indices into Config.Rate
	widthOf   []int   // per rate rule: index into widths
	widths    []int64 // distinct window widths (ns)
	newStream []int
	periodic  []int
}

func (r *scopeRules) slots() int { return len(r.rate) + len(r.newStream) + len(r.periodic) }

type keyState struct {
	key        Key
	lastSeen   int64
	prev, next *keyState
	win        *windowSet
	ns         []newStreamState
	per        []*periodicState
	eps        []episode
}

func (k *keyState) openEpisodes() bool {
	for i := range k.eps {
		if k.eps[i].open() {
			return true
		}
	}
	return false
}

type keyTable struct {
	scope                Scope
	rules                scopeRules
	m                    map[Key]*keyState
	root                 keyState // LRU sentinel: root.next = most recent
	capacityEvictions    uint64
	ttlEvictions         uint64
	lastCapacityEviction int64
	everCapacityEvicted  bool
}

func newKeyTable(s Scope) *keyTable {
	t := &keyTable{scope: s, m: make(map[Key]*keyState)}
	t.root.next, t.root.prev = &t.root, &t.root
	return t
}

func (t *keyTable) addRate(ruleIdx int, w time.Duration) {
	wi := -1
	for i, x := range t.rules.widths {
		if x == int64(w) {
			wi = i
		}
	}
	if wi < 0 {
		wi = len(t.rules.widths)
		t.rules.widths = append(t.rules.widths, int64(w))
	}
	t.rules.rate = append(t.rules.rate, ruleIdx)
	t.rules.widthOf = append(t.rules.widthOf, wi)
}

func (t *keyTable) unlink(k *keyState) { k.prev.next, k.next.prev = k.next, k.prev }

func (t *keyTable) pushFront(k *keyState) {
	k.next, k.prev = t.root.next, &t.root
	t.root.next.prev = k
	t.root.next = k
}

// expireKeys drops keys idle for StateTTL (from the LRU tail).
func (d *Detector) expireKeys(now int64) {
	ttl := int64(d.cfg.Limits.StateTTL)
	for _, t := range d.scopes {
		if t == nil {
			continue
		}
		// strict: a key idle exactly StateTTL is kept, so a quiet deadline at
		// that same instant still ends its episode normally
		for tail := t.root.prev; tail != &t.root && tail.lastSeen < now-ttl; tail = t.root.prev {
			d.dropKey(t, tail, now, false)
			t.ttlEvictions++
		}
	}
}

func (d *Detector) dropKey(t *keyTable, k *keyState, now int64, capacity bool) {
	if k.openEpisodes() {
		for slot := range k.eps {
			if !k.eps[slot].open() {
				continue
			}
			d.trBuf = k.eps[slot].evict(now, d.trBuf[:0])
			d.stats.EpisodesEvicted++
			var cov CoverageFlags
			if capacity {
				cov = CoverageKeyEvictions
			}
			d.emit(t, k, slot, d.trBuf, emitCtx{at: now, coverage: cov})
		}
	}
	t.unlink(k)
	delete(t.m, k.key)
	if capacity {
		d.callReduced = true
		t.capacityEvictions++
		t.lastCapacityEviction = now
		t.everCapacityEvicted = true
	}
}

func (d *Detector) getKey(t *keyTable, key Key, now int64) *keyState {
	if k, ok := t.m[key]; ok {
		t.unlink(k)
		t.pushFront(k)
		return k
	}
	if len(t.m) >= d.cfg.Limits.MaxKeysPerScope {
		// prefer the least recent key without an open episode (bounded scan)
		victim := t.root.prev
		for i, c := 0, t.root.prev; i < 8 && c != &t.root; i, c = i+1, c.prev {
			if !c.openEpisodes() {
				victim = c
				break
			}
		}
		d.dropKey(t, victim, now, true)
	}
	k := &keyState{key: key}
	r := &t.rules
	if len(r.rate) > 0 {
		k.win = newWindowSet(len(r.widths))
	}
	if len(r.newStream) > 0 {
		k.ns = make([]newStreamState, len(r.newStream))
	}
	if len(r.periodic) > 0 {
		k.per = make([]*periodicState, len(r.periodic))
		for i, ri := range r.periodic {
			k.per[i] = newPeriodicState(d.cfg.Periodic[ri].HistoryLen)
		}
	}
	k.eps = make([]episode, r.slots())
	t.m[key] = k
	t.pushFront(k)
	return k
}

func appliesTo(types []PayloadType, pt PayloadType) bool {
	if len(types) == 0 {
		return true
	}
	for _, x := range types {
		if x == pt {
			return true
		}
	}
	return false
}

// ---- event handling ----

type emitCtx struct {
	at       int64
	trigger  *Event
	label    TrafficLabel
	rate     *RateEvidence
	ns       *NewStreamEvidence
	per      *PeriodicEvidence
	coverage CoverageFlags
}

func (d *Detector) handle(e *Event) {
	d.stats.Processed++
	t := e.Time.UnixNano()
	d.lastEvT, d.lastEvID, d.evDone = t, e.ID, true
	for s := Scope(1); s < numScopes; s++ {
		tb := d.scopes[s]
		if tb == nil {
			continue
		}
		key, ok := keyOf(s, *e)
		if !ok {
			continue
		}
		r := &tb.rules
		applies := false
		for _, i := range r.rate {
			applies = applies || appliesTo(d.cfg.Rate[i].PayloadTypes, e.PayloadType)
		}
		for _, i := range r.newStream {
			applies = applies || appliesTo(d.cfg.NewStream[i].PayloadTypes, e.PayloadType)
		}
		for _, i := range r.periodic {
			applies = applies || appliesTo(d.cfg.Periodic[i].PayloadTypes, e.PayloadType)
		}
		if !applies {
			continue
		}
		k := d.getKey(tb, key, t)
		k.lastSeen = t
		cov := d.coverageFor(tb, k, t)
		slot := 0
		if k.win != nil {
			if k.win.add(t, e.Traffic, r.widths, d.cfg.Limits.MaxWindowEntries) {
				d.stats.WindowMerges++
				d.callReduced = true
				cov |= CoverageWindowSaturated
			}
			for ri, i := range r.rate {
				rule := &d.cfg.Rate[i]
				if appliesTo(rule.PayloadTypes, e.PayloadType) {
					sum := k.win.sums[r.widthOf[ri]]
					if sum.n >= uint64(rule.Threshold) {
						ev := &RateEvidence{Window: rule.Window, Threshold: rule.Threshold, Count: sum.n,
							Expected: sum.exp, Monitored: sum.mon,
							Saturated: k.win.lastMerge != 0 && k.win.lastMerge > t-int64(rule.Window)}
						c := cov
						if ev.Saturated {
							c |= CoverageWindowSaturated
						}
						d.hot(tb, k, slot, &rule.State, emitCtx{at: t, trigger: e,
							label: labelFromCounts(sum.n, sum.exp, sum.mon), rate: ev, coverage: c})
					}
				}
				slot++
			}
		}
		for ni, i := range r.newStream {
			rule := &d.cfg.NewStream[i]
			if appliesTo(rule.PayloadTypes, e.PayloadType) {
				if ev, lbl, ok := k.ns[ni].observe(t, e.Traffic, d.history, rule); ok {
					d.hot(tb, k, slot, &rule.State, emitCtx{at: t, trigger: e, label: lbl, ns: ev, coverage: cov})
				}
			}
			slot++
		}
		for pi, i := range r.periodic {
			rule := &d.cfg.Periodic[i]
			if appliesTo(rule.PayloadTypes, e.PayloadType) {
				if sig, ok := k.per[pi].observe(t, e.Traffic, rule, &d.perScratch); ok {
					ev := sig.ev
					d.hot(tb, k, slot, &rule.State, emitCtx{at: t, trigger: e, label: sig.label, per: &ev, coverage: cov})
				}
			}
			slot++
		}
	}
}

func (d *Detector) coverageFor(tb *keyTable, k *keyState, t int64) CoverageFlags {
	var c CoverageFlags
	lim := &d.cfg.Limits
	if tb.everCapacityEvicted && t-tb.lastCapacityEviction < int64(lim.StateTTL) {
		c |= CoverageKeyEvictions
	}
	if d.lastDedupEarly != 0 && t-d.lastDedupEarly < int64(lim.DedupHorizon) {
		c |= CoverageDedupEvictedEarly
	}
	if d.lastReorderForced != 0 && t-d.lastReorderForced < int64(lim.DedupHorizon) {
		c |= CoverageReorderForced
	}
	if d.lastTruncated != 0 && t-d.lastTruncated < int64(lim.DedupHorizon) {
		c |= CoverageInputTruncated
	}
	return c
}

func (d *Detector) params(tb *keyTable, slot int) (*StateParams, string, Kind) {
	r := &tb.rules
	if slot < len(r.rate) {
		x := &d.cfg.Rate[r.rate[slot]]
		return &x.State, x.Name, KindRate
	}
	slot -= len(r.rate)
	if slot < len(r.newStream) {
		x := &d.cfg.NewStream[r.newStream[slot]]
		return &x.State, x.Name, KindNewStream
	}
	slot -= len(r.newStream)
	x := &d.cfg.Periodic[r.periodic[slot]]
	return &x.State, x.Name, KindPeriodic
}

func (d *Detector) hot(tb *keyTable, k *keyState, slot int, p *StateParams, ctx emitCtx) {
	ep := &k.eps[slot]
	d.trBuf = ep.onHot(ctx.at, p, d.trBuf[:0])
	ep.note(ctx.label, ctx.coverage)
	d.emit(tb, k, slot, d.trBuf, ctx)
	d.schedule(tb, k, slot, p)
}

func (d *Detector) schedule(tb *keyTable, k *keyState, slot int, p *StateParams) {
	ep := &k.eps[slot]
	if at := ep.deadline(p); at != 0 {
		heap.Push(&d.deadlines, deadline{at: at, scope: tb.scope, key: k.key, slot: slot, version: ep.version})
	}
}

func (d *Detector) runDeadline(dl deadline) {
	tb := d.scopes[dl.scope]
	k, ok := tb.m[dl.key]
	if !ok || k.eps[dl.slot].version != dl.version {
		return
	}
	p, _, _ := d.params(tb, dl.slot)
	ep := &k.eps[dl.slot]
	d.trBuf = ep.onTime(dl.at, p, d.trBuf[:0])
	// capacity events near the deadline (e.g. a forced reorder release that
	// ran it early) are flagged on top of the episode's own coverage
	d.emit(tb, k, dl.slot, d.trBuf, emitCtx{at: dl.at, coverage: d.coverageFor(tb, k, dl.at)})
	d.schedule(tb, k, dl.slot, p)
}

func (d *Detector) emit(tb *keyTable, k *keyState, slot int, trs []transition, ctx emitCtx) {
	if len(trs) == 0 {
		return
	}
	_, name, kind := d.params(tb, slot)
	for _, tr := range trs {
		c := Candidate{
			EventID:      episodeID(name, k.key, tr.start),
			Rule:         name,
			Kind:         kind,
			Key:          k.key,
			From:         tr.from,
			To:           tr.to,
			Reason:       tr.reason,
			At:           nsTime(tr.at),
			DecidedAt:    nsTime(maxInt64(tr.at, d.clock)),
			EpisodeStart: nsTime(tr.start),
			Confidence:   k.key.confidence(),
			Traffic:      ctx.label,
			Coverage:     ctx.coverage,
			Summary:      tr.summary,
		}
		if ctx.trigger == nil {
			// time-driven or eviction: the episode's own label and coverage
			c.Traffic = tr.label
			c.Coverage = tr.cov | ctx.coverage
		} else {
			c.TriggerID = ctx.trigger.ID
			c.TriggerRoute = Route{w: ctx.trigger.FullRoute.w, hops: append([]byte(nil), ctx.trigger.FullRoute.hops...)}
			if ctx.rate != nil {
				ev := *ctx.rate
				c.Rate = &ev
			}
			if ctx.ns != nil {
				ev := *ctx.ns
				c.NewStream = &ev
				if ev.Censored {
					c.Confidence = ConfidenceLow
				}
			}
			if ctx.per != nil {
				ev := *ctx.per
				ev.Alternatives = append([]PeriodAlternative(nil), ctx.per.Alternatives...)
				c.Periodic = &ev
			}
		}
		c.Suppressed = d.cfg.Expected.Mode == ExpectedSuppress && c.Traffic == LabelExpected
		d.push(c)
	}
}

func (d *Detector) push(c Candidate) {
	d.stats.CandidatesEmitted++
	if len(d.pending)-d.pendingHead >= d.cfg.Limits.MaxPendingCandidates {
		d.pending[d.pendingHead] = Candidate{}
		d.pendingHead++
		d.stats.CandidatesDropped++
		d.callReduced = true
	}
	d.pending = append(d.pending, c)
	if d.pendingHead >= 256 && d.pendingHead*2 >= len(d.pending) {
		d.pending = append([]Candidate(nil), d.pending[d.pendingHead:]...)
		d.pendingHead = 0
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---- heaps ----

// eventHeap is a typed min-heap on (Time, ID). It avoids container/heap so
// events are not boxed into interfaces (no allocation per event).
type eventHeap []Event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) less(i, j int) bool {
	if !h[i].Time.Equal(h[j].Time) {
		return h[i].Time.Before(h[j].Time)
	}
	return h[i].ID < h[j].ID
}

func (h *eventHeap) push(e Event) {
	*h = append(*h, e)
	a := *h
	for i := len(a) - 1; i > 0; {
		p := (i - 1) / 2
		if !a.less(i, p) {
			break
		}
		a[i], a[p] = a[p], a[i]
		i = p
	}
}

func (h *eventHeap) pop() Event {
	a := *h
	top := a[0]
	last := len(a) - 1
	a[0] = a[last]
	a[last] = Event{}
	a = a[:last]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(a) && a.less(l, m) {
			m = l
		}
		if r < len(a) && a.less(r, m) {
			m = r
		}
		if m == i {
			break
		}
		a[i], a[m] = a[m], a[i]
		i = m
	}
	*h = a
	return top
}

type deadline struct {
	at      int64
	scope   Scope
	key     Key
	slot    int
	version uint32
}

type deadlineHeap []deadline

func (h deadlineHeap) Len() int { return len(h) }
func (h deadlineHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.at != b.at {
		return a.at < b.at
	}
	if a.scope != b.scope {
		return a.scope < b.scope
	}
	if a.key != b.key {
		return a.key.less(b.key)
	}
	if a.slot != b.slot {
		return a.slot < b.slot
	}
	return a.version < b.version
}
func (h deadlineHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *deadlineHeap) Push(x interface{}) { *h = append(*h, x.(deadline)) }
func (h *deadlineHeap) Pop() interface{} {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// dropStaleDeadlines pops stale entries at the top and compacts the heap
// when stale entries dominate, so it stays bounded by live episodes.
func (d *Detector) dropStaleDeadlines() {
	for d.deadlines.Len() > 0 && !d.deadlineLive(d.deadlines[0]) {
		heap.Pop(&d.deadlines)
	}
	if d.deadlines.Len() > 1024 && d.deadlines.Len() > 4*d.liveEpisodes()+1024 {
		kept := d.deadlines[:0]
		for _, x := range d.deadlines {
			if d.deadlineLive(x) {
				kept = append(kept, x)
			}
		}
		d.deadlines = kept
		heap.Init(&d.deadlines)
	}
}

func (d *Detector) deadlineLive(x deadline) bool {
	tb := d.scopes[x.scope]
	if tb == nil {
		return false
	}
	k, ok := tb.m[x.key]
	return ok && k.eps[x.slot].version == x.version
}

func (d *Detector) liveEpisodes() int {
	n := 0
	for _, tb := range d.scopes {
		if tb != nil {
			n += len(tb.m) * tb.rules.slots()
		}
	}
	return n
}
