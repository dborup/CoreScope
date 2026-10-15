package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #474, phase 3: overlay synthetic channel-bot trains on a background
// of real exported events, so recall can be measured when real spam is rare.
//
// The tool is offline and file-only. It opens no database, takes no -db
// flag, touches no network and reads no wall clock: it reads a spec and a
// background JSONL file and writes a new JSONL file plus a labels file.
// Nothing it produces is ever written back to a CoreScope database.
//
// Every injected event is marked: its id is "inj-<bot>-<pulse>-<part>", a
// value no export can produce (export ids are tx1, tx2, ...), and the tool
// refuses a background line whose id already starts with that prefix. A
// replay report therefore names injected traffic in its TriggerIDs, and the
// labels file says exactly which incidents were injected.

// injPrefix marks every injected event id.
const injPrefix = "inj-"

// maxInjectBots and maxInjectHoles bound one spec.
const (
	maxInjectBots  = 64
	maxInjectHoles = 64
	// maxInjectSizeMix bounds one bot's size distribution, and
	// maxInjectSizeCount one entry's weight, so the cumulative weights
	// cannot overflow.
	maxInjectSizeMix   = 4096
	maxInjectSizeCount = 1 << 32
)

// injectSpec is the JSON spec of one injection run. There are no defaults:
// every field must be present, and fields whose zero value is meaningful
// (seed, payload type, jitter, fractions) are pointers so that leaving one
// out is an error rather than a silent zero.
type injectSpec struct {
	Seed *uint64 `json:"seed"`
	// FinalAfter is added to each injected event time to give its final_at,
	// as the export's -settle does for real events.
	FinalAfter *dur          `json:"final_after"`
	Labelling  labellingSpec `json:"labelling"`
	Bots       []injectBot   `json:"bots"`
}

// labellingSpec is the ground-truth recipe applied to the injected pulses
// (#457 §4, periodic): events closer than PulseMergeGap are one pulse, P is
// the median pulse gap inside [MinPeriod, MaxPeriod], and an incident is a
// maximal run of at least MinPulses pulses whose every gap is within
// max(ToleranceAbs, ToleranceRel*P) of k*P for some k in 1..MaxHarmonic.
type labellingSpec struct {
	PulseMergeGap dur     `json:"pulse_merge_gap"`
	MinPeriod     dur     `json:"min_period"`
	MaxPeriod     dur     `json:"max_period"`
	MinPulses     int     `json:"min_pulses"`
	MaxHarmonic   int     `json:"max_harmonic"`
	ToleranceAbs  dur     `json:"tolerance_abs"`
	ToleranceRel  float64 `json:"tolerance_rel"`
}

// injectBot describes one synthetic bot. Times are data times, as in the
// export; the fields that describe the wire are exactly the fields of a
// replay record, so an injected line carries nothing a real line does not.
type injectBot struct {
	Name        string `json:"name"`
	Channel     string `json:"channel"`
	PayloadType *int   `json:"payload_type"`
	RouteClass  string `json:"route_class"`
	RouteKind   string `json:"route_kind"`
	FirstHop    string `json:"first_hop"`
	FullRoute   string `json:"full_route"`
	// The sizes are described in exactly one of two ways, and the spec must
	// pick one.
	//
	// SizeMix is the realistic one, and what `keys` produces: the key's own
	// observed (wire_bytes, payload_bytes) pairs with a count each. One
	// whole pair is drawn per event, weighted by count. Because every pair
	// came off a real line, it is valid by construction and carries the
	// route's own wire-minus-payload overhead, so an injected line cannot be
	// told from a real one on size.
	//
	// WireBytes with PayloadBytes and PayloadBytesSpread is the older,
	// scalar description, kept for hand-written specs: a fixed wire size and
	// a payload drawn uniformly from payload_bytes +/- the spread. It cannot
	// reproduce a real distribution - the payload comes in cipher blocks
	// while wire - payload is header plus path - so the whole envelope is
	// required to fit the frame (payload_bytes + spread <= wire_bytes - 2);
	// a spec that could overflow is refused here rather than aborting the
	// run on its first wide draw.
	WireBytes          int        `json:"wire_bytes"`
	PayloadBytes       *int       `json:"payload_bytes"`
	PayloadBytesSpread *int       `json:"payload_bytes_spread"`
	SizeMix            []sizePair `json:"size_mix"`
	Start              time.Time  `json:"start"`
	Stop               time.Time  `json:"stop"`
	Period             dur        `json:"period"`
	// JitterSD is the standard deviation of the per-pulse offset from the
	// nominal grid time; JitterMax clamps it. The offset is not cumulative,
	// so the train does not drift.
	JitterSD  *dur `json:"jitter_sd"`
	JitterMax dur  `json:"jitter_max"`
	// MissedFraction is the probability that a pulse other than the first is
	// dropped (one missed pulse, as a real bot's).
	MissedFraction *float64 `json:"missed_fraction"`
	// MultipartFraction is the probability that a pulse is multi-part;
	// 1..MultipartMaxExtra later parts then follow it within
	// MultipartMaxDelay.
	MultipartFraction *float64 `json:"multipart_fraction"`
	MultipartMaxExtra *int     `json:"multipart_max_extra"`
	MultipartMaxDelay dur      `json:"multipart_max_delay"`
	// Holes are data-time ranges where the bot sends nothing (an outage).
	Holes []injectHole `json:"holes"`
}

type injectHole struct {
	Start    time.Time `json:"start"`
	Duration dur       `json:"duration"`
}

// dur is a JSON duration written and read as a string ("5m0s"), so a spec
// and the labels file it produces round-trip through the same form.
type dur time.Duration

func (d dur) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *dur) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("durations are strings such as \"5m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = dur(v)
	return nil
}

// prng is splitmix64: a fixed, self-contained generator, so an injection is
// reproducible from the spec alone and independent of the Go version's
// math/rand.
type prng struct{ s uint64 }

func (p *prng) next() uint64 {
	p.s += 0x9e3779b97f4a7c15
	z := p.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// float returns a uniform value in [0,1).
func (p *prng) float() float64 { return float64(p.next()>>11) / (1 << 53) }

// normal returns a standard normal value (Box-Muller, cosine branch).
func (p *prng) normal() float64 {
	u1 := 1 - p.float() // in (0,1]
	u2 := p.float()
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

// botPRNG seeds one bot's stream from the spec seed and the bot's name, so
// each bot's output depends only on the seed and its own fields: adding,
// removing or reordering bots never changes another bot's events.
func botPRNG(seed uint64, name string) *prng {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	p := &prng{s: seed ^ h.Sum64()}
	p.next() // discard one value, so a zero seed is not a degenerate start
	return p
}

// botStats counts what one bot produced.
type botStats struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	// Key is the stream key every one of this bot's events falls on, in the
	// library's canonical encoding. The evaluation needs it: a bot placed on
	// a key real traffic uses shares that key with the background, and
	// scoring on the channel hash alone could then credit a real train to
	// the bot (or the other way round).
	Key             string    `json:"key"`
	PulsesScheduled int       `json:"pulses_scheduled"`
	PulsesEmitted   int       `json:"pulses_emitted"`
	PulsesMissed    int       `json:"pulses_missed"`
	PulsesInHole    int       `json:"pulses_in_hole"`
	MultipartPulses int       `json:"multipart_pulses"`
	Events          int       `json:"events"`
	First           time.Time `json:"first,omitempty"`
	Last            time.Time `json:"last,omitempty"`
	Incidents       int       `json:"incidents"`
}

// injectSummary is printed on stderr (counts and times only).
type injectSummary struct {
	Experimental         bool       `json:"experimental"`
	BackgroundLines      int        `json:"background_lines"`
	BackgroundOutOfOrder int        `json:"background_out_of_order"`
	InjectedEvents       int        `json:"injected_events"`
	LinesWritten         int        `json:"lines_written"`
	Bots                 []botStats `json:"bots"`
}

// Incident is one labelled ground-truth incident: a channel hash and a time
// range, with the bot's period. It holds no key material, no observer
// identity and no text.
type Incident struct {
	Bot     string `json:"bot"`
	Channel string `json:"channel"`
	// Key is the bot's stream key (see botStats.Key). score matches on it
	// when it is present, so an episode of real traffic on the same channel
	// but a different route group is never mistaken for the bot.
	Key    string    `json:"key"`
	Start  time.Time `json:"start"`
	Stop   time.Time `json:"stop"`
	Period dur       `json:"period"`
	Pulses int       `json:"pulses"`
}

// InjectLabels is the labels file: the ground truth of one injection run.
type InjectLabels struct {
	Experimental bool          `json:"experimental"`
	Synthetic    bool          `json:"synthetic"`
	Seed         uint64        `json:"seed"`
	Labelling    labellingSpec `json:"labelling"`
	Bots         []botStats    `json:"bots"`
	Incidents    []Incident    `json:"incidents"`
}

// injEvent is one generated event before it becomes a line.
type injEvent struct {
	rec   anomaly.ReplayRecord
	t     int64 // rec.Time in nanoseconds, for sorting
	pulse int
}

func runInject(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	fs.SetOutput(stderr)
	specPath := fs.String("spec", "", "injection spec JSON (required)")
	input := fs.String("input", "", "background ReplayRecord JSON Lines, or - for stdin (required; /dev/null for none)")
	out := fs.String("out", "", "merged JSON Lines output (required, must not exist)")
	labels := fs.String("labels", "", "ground-truth labels JSON (required, must not exist)")
	maxInjected := fs.Int("max-injected", 0, "most injected events allowed (required)")
	maxLine := fs.Int("max-line-bytes", 0, "longest accepted background line (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(err error) int {
		fmt.Fprintf(stderr, "inject: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("unexpected arguments"))
	}
	if *specPath == "" || *input == "" || *out == "" || *labels == "" {
		return usageErr(errors.New("-spec, -input, -out and -labels are required"))
	}
	if *maxInjected <= 0 || *maxLine <= 0 {
		return usageErr(errors.New("-max-injected and -max-line-bytes must be > 0"))
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "inject: %v\n", err)
		return exitFail
	}
	sb, err := os.ReadFile(*specPath)
	if err != nil {
		return fail(err)
	}
	spec, err := parseInjectSpec(sb)
	if err != nil {
		return fail(err)
	}
	evs, stats, incidents, err := generate(spec, *maxInjected)
	if err != nil {
		return fail(err)
	}
	in := stdin
	if *input != "-" {
		f, err := os.Open(*input)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		in = f
	}
	// Both outputs must be new files, and neither appears unless the whole
	// run succeeds.
	outF, err := createNew(*out)
	if err != nil {
		return fail(err)
	}
	sum, err := mergeLines(in, outF, evs, *maxLine)
	if cerr := outF.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(*out)
		return fail(err)
	}
	sum.Bots = stats
	lb, err := json.MarshalIndent(InjectLabels{Experimental: true, Synthetic: true, Seed: *spec.Seed,
		Labelling: spec.Labelling, Bots: stats, Incidents: incidents}, "", " ")
	if err == nil {
		err = writeNew(*labels, append(lb, '\n'))
	}
	if err != nil {
		os.Remove(*out)
		return fail(err)
	}
	b, err := json.Marshal(sum)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stderr, "%s\n", b)
	return exitOK
}

// parseInjectSpec decodes and validates a spec (unknown fields and trailing
// data rejected).
func parseInjectSpec(b []byte) (injectSpec, error) {
	var s injectSpec
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return injectSpec{}, fmt.Errorf("inject spec: %w", err)
	}
	if !onlyJSONValue(dec, b) {
		return injectSpec{}, errors.New("inject spec: trailing data")
	}
	if s.Seed == nil {
		return injectSpec{}, errors.New("inject spec: seed is required")
	}
	if s.FinalAfter == nil || *s.FinalAfter < 0 || time.Duration(*s.FinalAfter) > time.Hour ||
		time.Duration(*s.FinalAfter)%time.Second != 0 {
		return injectSpec{}, errors.New("inject spec: final_after is required and must be whole seconds in 0..1h")
	}
	if err := s.Labelling.validate(); err != nil {
		return injectSpec{}, err
	}
	if len(s.Bots) == 0 || len(s.Bots) > maxInjectBots {
		return injectSpec{}, fmt.Errorf("inject spec: need 1..%d bots", maxInjectBots)
	}
	seen := map[string]bool{}
	for i := range s.Bots {
		if err := s.Bots[i].validate(); err != nil {
			return injectSpec{}, err
		}
		if seen[s.Bots[i].Name] {
			return injectSpec{}, fmt.Errorf("inject spec: duplicate bot name %q", s.Bots[i].Name)
		}
		seen[s.Bots[i].Name] = true
		// Every size the bot can draw has to make a valid event, and the
		// check belongs here: a spec that could overflow the frame is
		// refused outright rather than aborting a run after thousands of
		// lines, which is what round 2 did on a realistic key.
		if _, err := botStreamKey(&s.Bots[i], time.Duration(*s.FinalAfter)); err != nil {
			return injectSpec{}, err
		}
	}
	return s, nil
}

func (l labellingSpec) validate() error {
	switch {
	case time.Duration(l.PulseMergeGap) <= 0:
		return errors.New("inject spec: labelling.pulse_merge_gap must be > 0")
	case time.Duration(l.MinPeriod) <= 0 || time.Duration(l.MaxPeriod) <= time.Duration(l.MinPeriod):
		return errors.New("inject spec: labelling needs 0 < min_period < max_period")
	case l.MinPulses < 2:
		return errors.New("inject spec: labelling.min_pulses must be >= 2")
	case l.MaxHarmonic < 1 || l.MaxHarmonic > 16:
		return errors.New("inject spec: labelling.max_harmonic must be in 1..16")
	case time.Duration(l.ToleranceAbs) < 0:
		return errors.New("inject spec: labelling.tolerance_abs must be >= 0")
	case !(l.ToleranceRel >= 0 && l.ToleranceRel <= 1):
		return errors.New("inject spec: labelling.tolerance_rel must be in 0..1")
	case time.Duration(l.ToleranceAbs) == 0 && l.ToleranceRel == 0:
		return errors.New("inject spec: labelling needs a tolerance")
	}
	return nil
}

// validBotName allows the characters a rule name allows, so an injected id
// stays printable and greppable.
func validBotName(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (b *injectBot) validate() error {
	name := b.Name
	bad := func(what string) error { return fmt.Errorf("inject spec: bot %q: %s", name, what) }
	if !validBotName(b.Name) {
		return fmt.Errorf("inject spec: bot names must be 1-32 chars of [a-z0-9_.-], got %q", b.Name)
	}
	if b.Channel == "" || strings.ToLower(b.Channel) != b.Channel {
		return bad("channel must be lower-case hex")
	}
	if _, err := hex.DecodeString(b.Channel); err != nil {
		return bad("channel must be hex")
	}
	switch {
	case b.PayloadType == nil:
		return bad("payload_type is required")
	case b.MissedFraction == nil || *b.MissedFraction < 0 || *b.MissedFraction > 1:
		return bad("missed_fraction is required and must be in 0..1")
	case b.MultipartFraction == nil || *b.MultipartFraction < 0 || *b.MultipartFraction > 1:
		return bad("multipart_fraction is required and must be in 0..1")
	case b.MultipartMaxExtra == nil || *b.MultipartMaxExtra < 0 || *b.MultipartMaxExtra > 8:
		return bad("multipart_max_extra is required and must be in 0..8")
	case b.JitterSD == nil || *b.JitterSD < 0:
		return bad("jitter_sd is required and must be >= 0")
	case b.SizeMix != nil:
		if err := b.validateSizeMix(bad); err != nil {
			return err
		}
	case b.WireBytes <= 0:
		return bad("size_mix or wire_bytes is required, and wire_bytes must be > 0")
	case b.PayloadBytes != nil && *b.PayloadBytes <= 0:
		return bad("payload_bytes must be > 0 when present")
	case b.PayloadBytesSpread == nil || *b.PayloadBytesSpread < 0:
		return bad("payload_bytes_spread is required and must be >= 0")
	case *b.PayloadBytesSpread > 0 && b.PayloadBytes == nil:
		return bad("payload_bytes_spread needs payload_bytes")
	case b.PayloadBytes != nil && *b.PayloadBytesSpread >= *b.PayloadBytes:
		return bad("payload_bytes_spread must be < payload_bytes")
	case b.PayloadBytes != nil && *b.PayloadBytes+*b.PayloadBytesSpread > b.WireBytes-2:
		// The top of the draw range, not just payload_bytes, has to fit the
		// frame: Event.Validate refuses PayloadBytes > WireBytes - 2.
		return bad("payload_bytes + payload_bytes_spread must fit the frame (<= wire_bytes - 2)")
	}
	switch {
	case b.Start.IsZero() || b.Stop.IsZero() || !b.Stop.After(b.Start):
		return bad("start and stop are required, with stop after start")
	case time.Duration(b.Period) <= 0:
		return bad("period must be > 0")
	case time.Duration(b.JitterMax) < 0 || time.Duration(b.JitterMax) >= time.Duration(b.Period)/2:
		return bad("jitter_max must be in [0, period/2)")
	case time.Duration(b.MultipartMaxDelay) < 0:
		return bad("multipart_max_delay must be >= 0")
	case *b.MultipartFraction > 0 && (*b.MultipartMaxExtra == 0 || time.Duration(b.MultipartMaxDelay) == 0):
		return bad("multipart_fraction needs multipart_max_extra and multipart_max_delay")
	case len(b.Holes) > maxInjectHoles:
		return bad("too many holes")
	}
	for _, h := range b.Holes {
		if h.Start.IsZero() || time.Duration(h.Duration) <= 0 {
			return bad("every hole needs a start and a duration > 0")
		}
	}
	return nil
}

// validateSizeMix checks a bot's size distribution. The pairs themselves are
// checked against the event validator in botStreamKey, which sees the bot's
// whole wire shape; here only the shape of the list is checked, plus the
// mutual exclusion with the scalar size fields.
func (b *injectBot) validateSizeMix(bad func(string) error) error {
	if b.WireBytes != 0 || b.PayloadBytes != nil || b.PayloadBytesSpread != nil {
		return bad("size_mix is the whole size description: leave wire_bytes, payload_bytes and payload_bytes_spread out")
	}
	if len(b.SizeMix) == 0 || len(b.SizeMix) > maxInjectSizeMix {
		return bad(fmt.Sprintf("size_mix needs 1..%d entries", maxInjectSizeMix))
	}
	seen := map[[2]int]bool{}
	for _, sp := range b.SizeMix {
		if sp.Count <= 0 || sp.Count > maxInjectSizeCount {
			return bad(fmt.Sprintf("every size_mix entry needs a count in 1..%d", maxInjectSizeCount))
		}
		if sp.WireBytes <= 0 {
			return bad("every size_mix entry needs wire_bytes > 0")
		}
		if sp.PayloadBytes != nil && *sp.PayloadBytes < 0 {
			return bad("a size_mix payload_bytes must be >= 0")
		}
		k := [2]int{sp.WireBytes, payloadOrUnknown(sp)}
		if seen[k] {
			return bad(fmt.Sprintf("size_mix carries the pair (%d, %d) twice", k[0], k[1]))
		}
		seen[k] = true
	}
	return nil
}

// sizeDraw turns one bot's size description into a draw. For a size mix it
// holds the cumulative weights, computed once per bot, so a draw is a binary
// search rather than a walk over the whole distribution.
type sizeDraw struct {
	mix   []sizePair
	cum   []int64
	total int64
	// the scalar description
	wire    int
	payload *int
	spread  int
}

func newSizeDraw(b *injectBot) sizeDraw {
	d := sizeDraw{mix: b.SizeMix, wire: b.WireBytes, payload: b.PayloadBytes}
	if b.PayloadBytesSpread != nil {
		d.spread = *b.PayloadBytesSpread
	}
	for _, sp := range b.SizeMix {
		d.total += int64(sp.Count)
		d.cum = append(d.cum, d.total)
	}
	return d
}

// random reports whether a draw consumes a value from the generator. A
// single-entry mix and a zero spread are constant, so they consume none and
// leave every other draw of the train where it was.
func (d sizeDraw) random() bool {
	if len(d.mix) > 0 {
		return len(d.mix) > 1
	}
	return d.payload != nil && d.spread > 0
}

// draw returns one event's wire and payload size. It takes exactly one value
// from p when random() is true, and none otherwise.
func (d sizeDraw) draw(p *prng) (int, *int) {
	if len(d.mix) > 0 {
		i := 0
		if len(d.mix) > 1 {
			r := int64(p.float() * float64(d.total))
			i = sort.Search(len(d.cum), func(j int) bool { return d.cum[j] > r })
			if i >= len(d.mix) {
				i = len(d.mix) - 1
			}
		}
		sp := d.mix[i]
		if sp.PayloadBytes == nil {
			return sp.WireBytes, nil
		}
		pb := *sp.PayloadBytes
		return sp.WireBytes, &pb
	}
	if d.payload == nil {
		return d.wire, nil
	}
	pb := *d.payload
	if d.spread > 0 {
		pb += int(p.float()*float64(2*d.spread+1)) - d.spread
	}
	return d.wire, &pb
}

// generate builds every injected event, the per-bot counts and the labelled
// incidents. It fails, without producing anything, if the spec would emit
// more than maxInjected events.
func generate(s injectSpec, maxInjected int) ([]injEvent, []botStats, []Incident, error) {
	var evs []injEvent
	stats := make([]botStats, 0, len(s.Bots))
	var incidents []Incident
	for i := range s.Bots {
		b := &s.Bots[i]
		botEvs, st, err := generateBot(b, *s.Seed, time.Duration(*s.FinalAfter), maxInjected-len(evs))
		if err != nil {
			return nil, nil, nil, err
		}
		inc := labelIncidents(b.Name, b.Channel, st.Key, botEvs, s.Labelling)
		st.Incidents = len(inc)
		evs = append(evs, botEvs...)
		stats = append(stats, st)
		incidents = append(incidents, inc...)
	}
	// One deterministic order for the output: time, then id.
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].t != evs[j].t {
			return evs[i].t < evs[j].t
		}
		return evs[i].rec.ID < evs[j].rec.ID
	})
	return evs, stats, incidents, nil
}

// generateBot draws one bot's pulse train. Draw order per scheduled pulse is
// fixed: the missed-pulse draw (except for the first pulse), the jitter draw,
// the multi-part draw, then one draw per extra part. A pulse dropped by an
// earlier draw consumes no later draw.
func generateBot(b *injectBot, seed uint64, finalAfter time.Duration, budget int) ([]injEvent, botStats, error) {
	p := botPRNG(seed, b.Name)
	st := botStats{Name: b.Name, Channel: b.Channel}
	period := time.Duration(b.Period)
	// Every one of a bot's events carries the same wire fields, so the bot
	// has exactly one stream key. Derive it once, from the same Event the
	// detector would see.
	key, err := botStreamKey(b, finalAfter)
	if err != nil {
		return nil, botStats{}, err
	}
	st.Key = key
	sizes := newSizeDraw(b)
	var evs []injEvent
	emit := func(pulse, part int, when time.Time) error {
		if len(evs) >= budget {
			return fmt.Errorf("inject: more than -max-injected events; raise the bound or shorten the spec")
		}
		// Parity with the export, which times every line from a whole-second
		// observation time (time.Unix(ts, 0)) and takes a whole-second
		// -settle: an injected line must not be recognisable by a fractional
		// second. Truncation moves a time by less than one second towards
		// the past, so the effective jitter is in [-(jitter_max + 1s),
		// jitter_max].
		when = when.UTC().Truncate(time.Second)
		// One size draw per emitted event: a whole (wire, payload) pair from
		// the key's observed distribution, or the scalar payload spread.
		wire, payload := sizes.draw(p)
		rec := anomaly.ReplayRecord{
			ID: fmt.Sprintf("%s%s-%d-%d", injPrefix, b.Name, pulse, part), Time: when,
			FinalAt: when.Add(finalAfter), PayloadType: *b.PayloadType, Channel: b.Channel,
			RouteClass: b.RouteClass, RouteKind: b.RouteKind, FirstHop: b.FirstHop, FullRoute: b.FullRoute,
			WireBytes: wire, PayloadBytes: payload, Traffic: "unclassified", Decryption: "unknown",
		}
		// An injected line must be exactly as valid as an exported one. Every
		// size the draw can produce was checked by botStreamKey before any
		// event was built, so this cannot fire on a validated spec.
		if _, err := rec.Event(); err != nil {
			return fmt.Errorf("inject: bot %q: %w", b.Name, err)
		}
		evs = append(evs, injEvent{rec: rec, t: rec.Time.UnixNano(), pulse: pulse})
		return nil
	}
	for i := 0; ; i++ {
		nominal := b.Start.Add(time.Duration(i) * period)
		if !nominal.Before(b.Stop) {
			break
		}
		st.PulsesScheduled++
		if i > 0 && p.float() < *b.MissedFraction {
			st.PulsesMissed++
			continue
		}
		if inHole(b.Holes, nominal) {
			st.PulsesInHole++
			continue
		}
		off := time.Duration(p.normal() * float64(time.Duration(*b.JitterSD)))
		if lim := time.Duration(b.JitterMax); off > lim {
			off = lim
		} else if off < -lim {
			off = -lim
		}
		when := nominal.Add(off)
		if err := emit(i, 0, when); err != nil {
			return nil, botStats{}, err
		}
		st.PulsesEmitted++
		if p.float() < *b.MultipartFraction {
			extra := 1 + int(p.float()*float64(*b.MultipartMaxExtra))
			st.MultipartPulses++
			for part := 1; part <= extra; part++ {
				d := time.Duration(p.float() * float64(time.Duration(b.MultipartMaxDelay)))
				if err := emit(i, part, when.Add(d)); err != nil {
					return nil, botStats{}, err
				}
			}
		}
	}
	st.Events = len(evs)
	if len(evs) > 0 {
		first, last := evs[0].rec.Time, evs[0].rec.Time
		for _, e := range evs {
			if e.rec.Time.Before(first) {
				first = e.rec.Time
			}
			if e.rec.Time.After(last) {
				last = e.rec.Time
			}
		}
		st.First, st.Last = first, last
	}
	return evs, st, nil
}

// botStreamKey returns the canonical stream key of a bot's events, and
// checks that every size the bot can draw makes a valid event. It builds the
// same ReplayRecord emit builds, so a spec whose wire fields do not make a
// valid event fails here - before any event, line or file is produced -
// rather than aborting the run halfway through or silently labelling nothing.
//
// Sizes are not part of a stream key, so every size gives the same key; the
// loop is there to validate, and the equality is asserted rather than
// assumed.
func botStreamKey(b *injectBot, finalAfter time.Duration) (string, error) {
	build := func(wire int, payload *int) (string, error) {
		rec := anomaly.ReplayRecord{ID: injPrefix + b.Name + "-key", Time: b.Start.UTC().Truncate(time.Second),
			PayloadType: *b.PayloadType, Channel: b.Channel, RouteClass: b.RouteClass, RouteKind: b.RouteKind,
			FirstHop: b.FirstHop, FullRoute: b.FullRoute, WireBytes: wire, PayloadBytes: payload,
			Traffic: "unclassified", Decryption: "unknown"}
		rec.FinalAt = rec.Time.Add(finalAfter)
		ev, err := rec.Event()
		if err != nil {
			return "", fmt.Errorf("inject: bot %q: %w", b.Name, err)
		}
		return anomaly.StreamKeyOf(ev).String(), nil
	}
	var sizes [][2]*int
	if len(b.SizeMix) > 0 {
		for _, sp := range b.SizeMix {
			wire := sp.WireBytes
			sizes = append(sizes, [2]*int{&wire, sp.PayloadBytes})
		}
	} else {
		// The whole envelope of the scalar draw: the smallest and the largest
		// payload it can reach.
		wire := b.WireBytes
		if b.PayloadBytes == nil {
			sizes = append(sizes, [2]*int{&wire, nil})
		} else {
			lo, hi := *b.PayloadBytes-*b.PayloadBytesSpread, *b.PayloadBytes+*b.PayloadBytesSpread
			sizes = append(sizes, [2]*int{&wire, &lo}, [2]*int{&wire, &hi})
		}
	}
	key := ""
	for _, s := range sizes {
		var payload *int
		if s[1] != nil {
			pb := *s[1]
			payload = &pb
		}
		k, err := build(*s[0], payload)
		if err != nil {
			return "", err
		}
		if key == "" {
			key = k
		} else if k != key {
			return "", fmt.Errorf("inject: bot %q: its sizes do not share one stream key", b.Name)
		}
	}
	return key, nil
}

func inHole(holes []injectHole, t time.Time) bool {
	for _, h := range holes {
		if !t.Before(h.Start) && t.Before(h.Start.Add(time.Duration(h.Duration))) {
			return true
		}
	}
	return false
}

// labelIncidents applies the labelling recipe to one bot's emitted events.
func labelIncidents(bot, channel, key string, evs []injEvent, l labellingSpec) []Incident {
	if len(evs) == 0 {
		return nil
	}
	times := make([]int64, 0, len(evs))
	for _, e := range evs {
		times = append(times, e.t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	// Events closer than the merge gap are one pulse, timed at its first
	// event (as the detector's BurstGap does).
	pulses := []int64{times[0]}
	for _, t := range times[1:] {
		if t-pulses[len(pulses)-1] >= int64(time.Duration(l.PulseMergeGap)) {
			pulses = append(pulses, t)
		}
	}
	if len(pulses) < 2 {
		return nil
	}
	gaps := make([]int64, 0, len(pulses)-1)
	inRange := make([]int64, 0, len(pulses)-1)
	for i := 1; i < len(pulses); i++ {
		g := pulses[i] - pulses[i-1]
		gaps = append(gaps, g)
		if g >= int64(time.Duration(l.MinPeriod)) && g <= int64(time.Duration(l.MaxPeriod)) {
			inRange = append(inRange, g)
		}
	}
	if len(inRange) == 0 {
		return nil
	}
	period := medianInt64(inRange)
	tol := int64(float64(period) * l.ToleranceRel)
	if a := int64(time.Duration(l.ToleranceAbs)); a > tol {
		tol = a
	}
	fits := func(g int64) bool {
		for k := 1; k <= l.MaxHarmonic; k++ {
			d := g - int64(k)*period
			if d < 0 {
				d = -d
			}
			if d <= tol {
				return true
			}
		}
		return false
	}
	var out []Incident
	// A run is a maximal stretch of pulses whose every gap fits.
	runStart := 0
	flush := func(end int) { // pulses[runStart:end+1] is the run
		if end-runStart+1 >= l.MinPulses {
			out = append(out, Incident{Bot: bot, Channel: channel, Key: key, Start: time.Unix(0, pulses[runStart]).UTC(),
				Stop: time.Unix(0, pulses[end]).UTC(), Period: dur(period), Pulses: end - runStart + 1})
		}
	}
	for i, g := range gaps {
		if !fits(g) {
			flush(i)
			runStart = i + 1
		}
	}
	flush(len(pulses) - 1)
	return out
}

// medianInt64 returns the median of a non-empty slice: the middle value, or
// the mean of the two middle values (truncated) for an even count. It sorts
// a copy, so the caller's slice order is kept.
func medianInt64(v []int64) int64 {
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return s[n/2-1] + (s[n/2]-s[n/2-1])/2
}

// backgroundLine is the minimum a background line must tell us: its time, to
// merge on, and its id, to keep the injected marker unambiguous. Unknown
// fields are allowed here: the line is copied through byte for byte.
type backgroundLine struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
}

// mergeLines writes the injected events and the background lines to w in
// data-time order. Background lines are copied verbatim, so the background
// is never reformatted; an injected event goes out before the first
// background line that is not earlier than it.
func mergeLines(r io.Reader, w io.Writer, evs []injEvent, maxLine int) (injectSummary, error) {
	sum := injectSummary{Experimental: true, InjectedEvents: len(evs)}
	bw := bufio.NewWriter(w)
	next := 0
	writeInjected := func(upto int64, all bool) error {
		for next < len(evs) && (all || evs[next].t <= upto) {
			b, err := json.Marshal(evs[next].rec)
			if err != nil {
				return err
			}
			if _, err := bw.Write(append(b, '\n')); err != nil {
				return err
			}
			next++
			sum.LinesWritten++
		}
		return nil
	}
	sc := bufio.NewScanner(r)
	limit := maxLine + 2
	initial := 4096
	if limit < initial {
		initial = limit
	}
	sc.Buffer(make([]byte, 0, initial), limit)
	var prev int64
	havePrev := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) > maxLine {
			return sum, errors.New("inject: background line longer than -max-line-bytes")
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		sum.BackgroundLines++
		var bl backgroundLine
		if err := json.Unmarshal(line, &bl); err != nil || bl.Time.IsZero() {
			return sum, fmt.Errorf("inject: background line %d: need a JSON object with an RFC 3339 \"time\"", sum.BackgroundLines)
		}
		if strings.HasPrefix(bl.ID, injPrefix) {
			return sum, fmt.Errorf("inject: background line %d already carries an injected id (%s...): refusing to mix", sum.BackgroundLines, injPrefix)
		}
		t := bl.Time.UnixNano()
		if havePrev && t < prev {
			sum.BackgroundOutOfOrder++
		}
		prev, havePrev = t, true
		if err := writeInjected(t, false); err != nil {
			return sum, err
		}
		if _, err := bw.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			return sum, err
		}
		sum.LinesWritten++
	}
	if err := sc.Err(); err != nil {
		return sum, fmt.Errorf("inject: background input: %w", err)
	}
	if err := writeInjected(0, true); err != nil {
		return sum, err
	}
	return sum, bw.Flush()
}

// onlyJSONValue reports whether the decoder consumed all of b but trailing
// whitespace.
func onlyJSONValue(dec *json.Decoder, b []byte) bool {
	return len(strings.TrimSpace(string(b[dec.InputOffset():]))) == 0
}
