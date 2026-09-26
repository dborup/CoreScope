package anomaly

// episode is the per-(rule, key) state machine
//
//	normal -> suspicious -> active -> ended -> (cooldown) -> normal
//
// with every transition checked by legalTransition. It only reacts to hot
// signals and to deadlines in data time; it never reads the wall clock.
type episode struct {
	state           State
	start           int64 // first hot signal of the episode (stable ID basis)
	suspiciousSince int64
	activeSince     int64
	lastHot         int64
	endedAt         int64
	hot             uint32
	version         uint32 // bumped on every change; invalidates old deadlines
	// label and cov summarize the hot signals of the episode, so that
	// time-driven and eviction transitions carry the same traffic label and
	// coverage flags as the transitions that opened it.
	label    TrafficLabel
	labelSet bool
	cov      CoverageFlags
}

// note merges one hot signal's label and coverage into the episode.
func (ep *episode) note(l TrafficLabel, c CoverageFlags) {
	ep.cov |= c
	if !ep.labelSet {
		ep.label, ep.labelSet = l, true
		return
	}
	ep.label = mergeLabel(ep.label, l)
}

type transition struct {
	from, to State
	reason   Reason
	at       int64
	start    int64
	summary  *EpisodeSummary
	label    TrafficLabel
	cov      CoverageFlags
}

func (ep *episode) summary() *EpisodeSummary {
	s := &EpisodeSummary{LastHot: nsTime(ep.lastHot), HotSignals: ep.hot}
	if ep.activeSince != 0 {
		s.ActiveSince = nsTime(ep.activeSince)
	}
	return s
}

func (ep *episode) move(to State, r Reason, at int64, out []transition, withSummary bool) []transition {
	if !legalTransition(ep.state, to, r) {
		panic("anomaly: illegal state transition " + ep.state.String() + "->" + to.String() + " (" + r.String() + ")")
	}
	tr := transition{from: ep.state, to: to, reason: r, at: at, start: ep.start, label: ep.label, cov: ep.cov}
	if withSummary {
		tr.summary = ep.summary()
	}
	ep.state = to
	ep.version++
	return append(out, tr)
}

// onHot applies a hot signal at data time t. Due deadlines must already have
// been applied (the Detector processes deadlines at or before t first).
func (ep *episode) onHot(t int64, p *StateParams, out []transition) []transition {
	if ep.state == StateEnded {
		if t-ep.endedAt < int64(p.Cooldown) {
			ep.lastHot = t
			ep.hot++
			return ep.move(StateActive, ReasonReopened, t, out, false)
		}
		// cooldown over: a silent return to normal, then a new episode
		*ep = episode{version: ep.version + 1}
	}
	switch ep.state {
	case StateNormal:
		*ep = episode{state: StateNormal, start: t, suspiciousSince: t, lastHot: t, hot: 1, version: ep.version}
		out = ep.move(StateSuspicious, ReasonThresholdCrossed, t, out, false)
		if p.ConfirmAfter == 0 {
			ep.activeSince = t
			out = ep.move(StateActive, ReasonConfirmed, t, out, false)
		}
	case StateSuspicious:
		ep.lastHot = t
		ep.hot++
		if t-ep.suspiciousSince >= int64(p.ConfirmAfter) {
			ep.activeSince = t
			out = ep.move(StateActive, ReasonConfirmed, t, out, false)
		} else {
			ep.version++
		}
	case StateActive:
		ep.lastHot = t
		ep.hot++
		ep.version++
	}
	return out
}

// deadline returns the next data time at which the episode changes by
// itself, or 0 if none.
func (ep *episode) deadline(p *StateParams) int64 {
	switch ep.state {
	case StateSuspicious, StateActive:
		return ep.lastHot + int64(p.EndAfterQuiet)
	case StateEnded:
		return ep.endedAt + int64(p.Cooldown)
	}
	return 0
}

// onTime applies every time-driven change due at or before now. Transitions
// are stamped with the time the condition became true, not with now, so the
// output does not depend on when the caller advanced time.
func (ep *episode) onTime(now int64, p *StateParams, out []transition) []transition {
	for {
		d := ep.deadline(p)
		if d == 0 || d > now {
			return out
		}
		switch ep.state {
		case StateSuspicious:
			out = ep.move(StateNormal, ReasonNotConfirmed, d, out, true)
			*ep = episode{version: ep.version}
		case StateActive:
			out = ep.move(StateEnded, ReasonQuiet, d, out, true)
			ep.endedAt = d
		case StateEnded:
			// cooldown expired: silent return to normal
			*ep = episode{version: ep.version + 1}
		}
	}
}

// evict closes an open episode because its key state is being dropped.
func (ep *episode) evict(now int64, out []transition) []transition {
	switch ep.state {
	case StateSuspicious:
		out = ep.move(StateNormal, ReasonEvicted, now, out, true)
	case StateActive:
		out = ep.move(StateEnded, ReasonEvicted, now, out, true)
	}
	return out
}

func (ep *episode) open() bool { return ep.state == StateSuspicious || ep.state == StateActive }
