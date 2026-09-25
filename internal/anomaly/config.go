package anomaly

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Config configures a Detector. There are no defaults: every limit and every
// threshold must be set explicitly, New rejects the zero value, and a config
// that enables no rule is rejected rather than silently doing nothing.
//
// None of the thresholds are validated for production use. Phase 1 of the
// study had only three validated incidents; any threshold must first pass a
// blind validation on a snapshot that starts no earlier than 2026-10-08.
type Config struct {
	Limits    Limits
	Rate      []RateRule
	NewStream []NewStreamRule
	Periodic  []PeriodicRule
	Expected  ExpectedPolicy
	// HistoryStart is the earliest data time the caller's history covers
	// (for example the start of a replay, or of the DB retention window).
	// Streams first seen within a NewStreamRule's CensorWindow after it are
	// left-censored, never "new". Zero means: the time of the first event
	// the Detector processes.
	HistoryStart time.Time
}

// Limits bound every piece of state. All fields are required (> 0), except
// ReorderDelay, where 0 means "events must arrive in data-time order".
type Limits struct {
	// MaxKeysPerScope caps tracked keys per scope. When full, the least
	// recently seen key without an open episode is evicted (counted).
	MaxKeysPerScope int
	// StateTTL evicts keys idle this long (data time).
	StateTTL time.Duration
	// DedupHorizon: an ID is guaranteed to be counted at most once if its
	// repeat arrives within this data-time horizon (and MaxDedupIDs was not
	// exhausted). Beyond it, a repeat is counted again. Unbounded dedup of
	// arbitrary IDs cannot be combined with bounded memory.
	DedupHorizon time.Duration
	// MaxDedupIDs caps remembered IDs. Evicting an ID before DedupHorizon is
	// counted and flags reduced coverage.
	MaxDedupIDs int
	// ReorderDelay is how long events are held so that events arriving out
	// of data-time order within this delay are still processed in order.
	// Events older than the processed frontier are rejected as late. With a
	// delay > 0, events with equal times are processed in ID order, and one
	// arriving after its tie was already processed (by Advance, Flush or a
	// later event) is late if its ID sorts first; with 0, every event is
	// processed on arrival and equal times keep arrival order.
	ReorderDelay time.Duration
	// MaxReorderBuffer caps held events; overflow force-releases the oldest.
	MaxReorderBuffer int
	// MaxWindowEntries caps distinct timestamps kept per key for rate
	// windows. Overflow merges the two oldest entries (counts may be
	// overstated until they expire) and is flagged on candidates.
	MaxWindowEntries int
	// MaxCandidatesPerCall caps candidates returned by one call; the rest
	// wait in a buffer returned by later calls or Drain.
	MaxCandidatesPerCall int
	// MaxPendingCandidates caps that buffer; overflow drops the oldest
	// candidate (counted, flagged).
	MaxPendingCandidates int
}

// ExpectedMode says how caller-labelled expected traffic is reported.
type ExpectedMode uint8

const (
	expectedModeUnset ExpectedMode = iota
	// ExpectedInclude: expected traffic counts toward thresholds; candidates
	// carry a TrafficLabel.
	ExpectedInclude
	// ExpectedSuppress: as Include, but candidates whose label is
	// LabelExpected are marked Suppressed. They are still returned and all
	// counters still include them; nothing is deleted.
	ExpectedSuppress
)

// ExpectedPolicy must be set explicitly.
type ExpectedPolicy struct {
	Mode ExpectedMode
}

// StateParams drive the per-episode state machine
// normal -> suspicious -> active -> ended.
type StateParams struct {
	// ConfirmAfter: the rule must stay hot this long after it first fires
	// before the episode becomes active (minimum duration). 0 confirms on
	// the first signal.
	ConfirmAfter time.Duration
	// EndAfterQuiet: an episode with no hot signal for this long ends (or,
	// if never confirmed, returns to normal).
	EndAfterQuiet time.Duration
	// Cooldown: a hot signal within this long after the end reopens the
	// same episode (same EventID) instead of starting a new one.
	Cooldown time.Duration
}

// RateRule fires when a key's count in a sliding window (t-Window, t]
// reaches Threshold.
type RateRule struct {
	Name         string
	Scope        Scope
	Window       time.Duration
	Threshold    int
	PayloadTypes []PayloadType // empty = all payload types
	State        StateParams
}

// NewStreamRule fires when a key that was not seen before (or was silent for
// QuietPeriod) reaches MinCount events within Window of its first event.
type NewStreamRule struct {
	Name         string
	Scope        Scope
	Window       time.Duration
	MinCount     int
	CensorWindow time.Duration
	QuietPeriod  time.Duration
	PayloadTypes []PayloadType
	State        StateParams
}

// PeriodicRule detects a regular pulse train on a key. All evaluation is
// causal: a signal at time t uses only events at or before t.
type PeriodicRule struct {
	Name         string
	Scope        Scope
	PayloadTypes []PayloadType
	// MinPeriod and MaxPeriod bound the fundamental period searched.
	MinPeriod, MaxPeriod time.Duration
	// BurstGap: events within this gap of the previous event belong to the
	// same pulse (the pulse time is its first event). Must be < MinPeriod/2.
	BurstGap time.Duration
	// Tolerance for one gap: max(JitterAbs, JitterRel*period).
	JitterAbs time.Duration
	JitterRel float64
	// MaxMissing is the number of consecutive missing pulses one gap may
	// bridge (a gap may span up to MaxMissing+1 periods).
	MaxMissing int
	// MinPulses is the minimum number of pulses in the current chain.
	MinPulses int
	// HistoryLen is the pulse ring size (bounds memory and CPU).
	HistoryLen int
	// MinCoverage: explained gaps / periods spanned, in (0,1].
	MinCoverage float64
	// MaxJitterFraction: median residual must be <= this fraction of the
	// tolerance.
	MaxJitterFraction float64
	// MaxChance bounds PeriodicEvidence.Chance: a union bound on the expected
	// number of equally long chains under a Poisson null with the key's
	// recent (local) pulse rate, over the period search and every pulse
	// evaluated on the key.
	MaxChance float64
	State     StateParams
}

const (
	maxWindow       = 24 * time.Hour
	maxHistoryLen   = 256
	minHistoryLen   = 8
	maxMissingLimit = 8
	maxRuleNameLen  = 64
	maxKeysLimit    = 10_000_000
)

var errNoRules = errors.New("anomaly: config enables no rule")

func validName(s string) bool {
	if len(s) == 0 || len(s) > maxRuleNameLen {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (p StateParams) validate(name string) error {
	if p.ConfirmAfter < 0 || p.Cooldown < 0 {
		return fmt.Errorf("anomaly: rule %s: ConfirmAfter and Cooldown must be >= 0", name)
	}
	if p.EndAfterQuiet <= 0 {
		return fmt.Errorf("anomaly: rule %s: State.EndAfterQuiet must be > 0", name)
	}
	if p.ConfirmAfter > maxWindow || p.EndAfterQuiet > maxWindow || p.Cooldown > 7*maxWindow {
		return fmt.Errorf("anomaly: rule %s: state durations out of range", name)
	}
	return nil
}

func validPayloadTypes(ts []PayloadType) bool {
	for _, t := range ts {
		if t > MaxPayloadType {
			return false
		}
	}
	return true
}

func finiteIn(v, lo, hi float64) bool { return !math.IsNaN(v) && v >= lo && v <= hi }

// Validate checks the whole config.
func (c *Config) Validate() error {
	l := c.Limits
	switch {
	case l.MaxKeysPerScope <= 0 || l.MaxKeysPerScope > maxKeysLimit:
		return errors.New("anomaly: Limits.MaxKeysPerScope must be in 1..10000000")
	case l.StateTTL <= 0:
		return errors.New("anomaly: Limits.StateTTL must be > 0")
	case l.DedupHorizon <= 0:
		return errors.New("anomaly: Limits.DedupHorizon must be > 0")
	case l.MaxDedupIDs <= 0:
		return errors.New("anomaly: Limits.MaxDedupIDs must be > 0")
	case l.ReorderDelay < 0 || l.ReorderDelay > time.Hour:
		return errors.New("anomaly: Limits.ReorderDelay must be in 0..1h")
	case l.MaxReorderBuffer <= 0:
		return errors.New("anomaly: Limits.MaxReorderBuffer must be > 0")
	case l.MaxWindowEntries < 2:
		return errors.New("anomaly: Limits.MaxWindowEntries must be >= 2")
	case l.MaxCandidatesPerCall <= 0:
		return errors.New("anomaly: Limits.MaxCandidatesPerCall must be > 0")
	case l.MaxPendingCandidates < l.MaxCandidatesPerCall:
		return errors.New("anomaly: Limits.MaxPendingCandidates must be >= MaxCandidatesPerCall")
	}
	switch c.Expected.Mode {
	case ExpectedInclude, ExpectedSuppress:
	default:
		return errors.New("anomaly: Expected.Mode must be set explicitly")
	}
	if len(c.Rate)+len(c.NewStream)+len(c.Periodic) == 0 {
		return errNoRules
	}
	if !c.HistoryStart.IsZero() && !validTime(c.HistoryStart) {
		return errors.New("anomaly: HistoryStart out of range")
	}
	names := map[string]bool{}
	checkName := func(n string) error {
		if !validName(n) {
			return errors.New("anomaly: rule names must be 1-64 chars of [a-z0-9_.-]")
		}
		if names[n] {
			return fmt.Errorf("anomaly: duplicate rule name %s", n)
		}
		names[n] = true
		return nil
	}
	var needTTL, maxRateWindow time.Duration
	need := func(d time.Duration) {
		if d > needTTL {
			needTTL = d
		}
	}
	for _, r := range c.Rate {
		if err := checkName(r.Name); err != nil {
			return err
		}
		if !r.Scope.valid() {
			return fmt.Errorf("anomaly: rule %s: invalid scope", r.Name)
		}
		if r.Window < time.Second || r.Window > maxWindow {
			return fmt.Errorf("anomaly: rule %s: Window must be in 1s..24h", r.Name)
		}
		if r.Threshold < 1 {
			return fmt.Errorf("anomaly: rule %s: Threshold must be >= 1", r.Name)
		}
		if !validPayloadTypes(r.PayloadTypes) {
			return fmt.Errorf("anomaly: rule %s: invalid payload type", r.Name)
		}
		if err := r.State.validate(r.Name); err != nil {
			return err
		}
		if r.Window > maxRateWindow {
			maxRateWindow = r.Window
		}
		need(r.Window)
		need(r.State.EndAfterQuiet + r.State.Cooldown)
	}
	for _, r := range c.NewStream {
		if err := checkName(r.Name); err != nil {
			return err
		}
		if r.Scope != ScopeStream && r.Scope != ScopeChannel && r.Scope != ScopeRouteGroup {
			return fmt.Errorf("anomaly: rule %s: new-stream scope must be stream, channel or route", r.Name)
		}
		if r.Window <= 0 || r.Window > maxWindow || r.MinCount < 1 {
			return fmt.Errorf("anomaly: rule %s: Window must be in (0,24h] and MinCount >= 1", r.Name)
		}
		if r.CensorWindow <= 0 || r.QuietPeriod <= 0 {
			return fmt.Errorf("anomaly: rule %s: CensorWindow and QuietPeriod must be > 0", r.Name)
		}
		// A key silent for less than QuietPeriod before the history start
		// would still be a continuing stream; censoring must cover that.
		if r.CensorWindow < r.QuietPeriod {
			return fmt.Errorf("anomaly: rule %s: CensorWindow must be >= QuietPeriod", r.Name)
		}
		// The rule signals once per episode, so it could never be confirmed.
		if r.State.ConfirmAfter != 0 {
			return fmt.Errorf("anomaly: rule %s: new-stream rules signal once per episode; State.ConfirmAfter must be 0", r.Name)
		}
		if !validPayloadTypes(r.PayloadTypes) {
			return fmt.Errorf("anomaly: rule %s: invalid payload type", r.Name)
		}
		if err := r.State.validate(r.Name); err != nil {
			return err
		}
		need(r.Window)
		need(r.QuietPeriod)
		need(r.State.EndAfterQuiet + r.State.Cooldown)
	}
	for _, r := range c.Periodic {
		if err := checkName(r.Name); err != nil {
			return err
		}
		if r.Scope != ScopeStream && r.Scope != ScopeChannel {
			return fmt.Errorf("anomaly: rule %s: periodic scope must be stream or channel", r.Name)
		}
		switch {
		case r.MinPeriod <= 0 || r.MaxPeriod <= r.MinPeriod || r.MaxPeriod > maxWindow:
			return fmt.Errorf("anomaly: rule %s: need 0 < MinPeriod < MaxPeriod <= 24h", r.Name)
		case r.BurstGap < 0 || r.BurstGap >= r.MinPeriod-r.BurstGap: // 2*BurstGap >= MinPeriod, without overflow
			return fmt.Errorf("anomaly: rule %s: BurstGap must be in [0, MinPeriod/2)", r.Name)
		case r.JitterAbs < 0 || !finiteIn(r.JitterRel, 0, 0.25) || (r.JitterAbs == 0 && r.JitterRel == 0):
			return fmt.Errorf("anomaly: rule %s: need JitterAbs >= 0, JitterRel in [0,0.25], not both 0", r.Name)
		case r.MaxMissing < 0 || r.MaxMissing > maxMissingLimit:
			return fmt.Errorf("anomaly: rule %s: MaxMissing must be in 0..%d", r.Name, maxMissingLimit)
		case r.HistoryLen < minHistoryLen || r.HistoryLen > maxHistoryLen:
			return fmt.Errorf("anomaly: rule %s: HistoryLen must be in %d..%d", r.Name, minHistoryLen, maxHistoryLen)
		case r.MinPulses < 4 || r.MinPulses > r.HistoryLen:
			return fmt.Errorf("anomaly: rule %s: MinPulses must be in 4..HistoryLen", r.Name)
		case !finiteIn(r.MinCoverage, 0, 1) || r.MinCoverage == 0:
			return fmt.Errorf("anomaly: rule %s: MinCoverage must be in (0,1]", r.Name)
		case !finiteIn(r.MaxJitterFraction, 0, 1) || r.MaxJitterFraction == 0:
			return fmt.Errorf("anomaly: rule %s: MaxJitterFraction must be in (0,1]", r.Name)
		case !finiteIn(r.MaxChance, 0, 1) || r.MaxChance == 0:
			return fmt.Errorf("anomaly: rule %s: MaxChance must be in (0,1]", r.Name)
		}
		// A tolerance at or above half the period would let one gap match two
		// multiples; refuse it.
		if tolFor(r, r.MinPeriod) >= r.MinPeriod/2 {
			return fmt.Errorf("anomaly: rule %s: tolerance must stay below MinPeriod/2", r.Name)
		}
		if !validPayloadTypes(r.PayloadTypes) {
			return fmt.Errorf("anomaly: rule %s: invalid payload type", r.Name)
		}
		if err := r.State.validate(r.Name); err != nil {
			return err
		}
		need(time.Duration(r.MaxMissing+1)*r.MaxPeriod + r.State.EndAfterQuiet)
		need(r.State.EndAfterQuiet + r.State.Cooldown)
	}
	if l.StateTTL < needTTL {
		return fmt.Errorf("anomaly: Limits.StateTTL must be >= %s (longest window, quiet period or episode memory)", needTTL)
	}
	if l.DedupHorizon < maxRateWindow+l.ReorderDelay {
		return errors.New("anomaly: Limits.DedupHorizon must cover the longest rate window plus ReorderDelay")
	}
	return nil
}

func tolFor(r PeriodicRule, period time.Duration) time.Duration {
	rel := time.Duration(r.JitterRel * float64(period))
	if r.JitterAbs > rel {
		return r.JitterAbs
	}
	return rel
}
