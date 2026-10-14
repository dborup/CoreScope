package anomaly

// Train tracking for active periodic episodes (issue #130, phase B).
//
// Detection needs a full clean chain (MinPulses pulses), so after any break
// it takes MinPulses periods to signal again, which is longer than a
// practical EndAfterQuiet. While an episode is active and tracking is on
// (PeriodicRule.EndAfterMissedPulses > 0), the tracker instead follows the
// established pulse train: a pulse that lands on the expected grid keeps the
// episode alive, and the episode ends when EndAfterMissedPulses consecutive
// expected pulses are missing. Tracking never creates an episode: it is armed
// by a detection signal and only runs while that episode is active.
//
// Pulses are matched against the last matched pulse (the anchor), not against
// the previous pulse. A later part of a multi-part pulse is 0..~1 minute
// after the anchor, so it rounds to zero periods and is ignored: it neither
// matches nor counts as a break, and it cannot move the anchor.

// maxMissedPulses bounds PeriodicRule.EndAfterMissedPulses.
const maxMissedPulses = 64

type trackResult uint8

const (
	trackIgnored trackResult = iota // not on the grid: neither a match nor a break
	trackMatched                    // on the grid: the train continues
)

// pulseTracker is the per-key, per-rule tracking state: O(1) and fixed size.
type pulseTracker struct {
	armed   bool
	anchor  int64 // estimated phase: time of the last matched pulse, smoothed
	period  int64 // current estimate, within period0 +- tol/2
	period0 int64 // estimate from the detection that armed the tracker
	tol     int64
}

func (tr *pulseTracker) arm(anchor, period, tol int64) {
	*tr = pulseTracker{armed: true, anchor: anchor, period: period, period0: period, tol: tol}
}

// pulse feeds a newly started pulse at time t (t > anchor).
func (tr *pulseTracker) pulse(t int64, r *PeriodicRule) trackResult {
	d := t - tr.anchor
	k := (d + tr.period/2) / tr.period // nearest number of periods; 0 = stray
	if k < 1 || k > int64(r.MaxMissing)+1 {
		return trackIgnored
	}
	diff := d - k*tr.period // signed error of the observation against the grid
	if diff > tr.tol || -diff > tr.tol {
		return trackIgnored
	}
	// Conservative update. The phase moves halfway to the observation, so
	// the jitter of one pulse does not shift the grid that the following
	// pulses are matched against (matching against the raw previous pulse
	// would make one late pulse reject its successors too); the period moves
	// a quarter of the observed error per period, never further than half a
	// tolerance from the activation estimate.
	p := tr.period + diff/(4*k)
	if lo := tr.period0 - tr.tol/2; p < lo {
		p = lo
	}
	if hi := tr.period0 + tr.tol/2; p > hi {
		p = hi
	}
	tr.anchor += k*tr.period + diff/2
	tr.period = p
	return trackMatched
}

// quiet is how long after the anchor the episode ends: the last of
// EndAfterMissedPulses expected pulses plus its tolerance, capped by the
// outer bound State.EndAfterQuiet.
func (tr *pulseTracker) quiet(r *PeriodicRule) int64 {
	q := int64(r.EndAfterMissedPulses)*tr.period + tr.tol
	if o := int64(r.State.EndAfterQuiet); q > o {
		q = o
	}
	return q
}
