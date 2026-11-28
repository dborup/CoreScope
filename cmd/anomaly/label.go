package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #474, round 4: ground truth for REAL traffic, so arm A can tell a
// false alarm from a real automated sender.
//
// Round 3 counted every arm-A episode as a false alarm. All 17 of them fell
// on one channel hash that, by aggregate signals alone, very likely carried
// a real automated sender - which is the rule's target, not a false alarm.
// This command labels such senders from aggregate signals that are fixed in
// advance, need no decryption (so they work on channels nobody holds a key
// for) and use no detector output at all:
//
//  1. Channel-hash level. Every line of the payload type on one channel
//     hash, through any route group. A sender that enters through changing
//     first hops is still one series here, where the rule's stream key
//     would split it.
//  2. The #457 §4 periodic recipe (periodicRuns), per activity segment, with
//     two corrections to its pulse merge and its period estimate:
//     - a segment ends at a gap no run can bridge (more than max_harmonic *
//     max_period plus the tolerance), so P is estimated from the segment's
//     own traffic rather than from a week of everything on the hash, and
//     splitting there never cuts a run;
//     - the pulse merge is min(pulse_merge_gap, P0/2), where P0 is the
//     median raw gap of at least min_period/2 in the segment. A fixed 60 s
//     merge folds every other pulse of a one-minute train into its
//     neighbour, so the recipe could not label the bottom of its own
//     period range; for any cadence of 2 min or more the cap is inactive
//     and the merge is pulse_merge_gap, exactly as in §4.
//  3. Constant size. A run is kept only when one payload size carries at
//     least min_size_share of its lines (every line of its pulses). A line
//     whose payload length the export did not know counts in the run but
//     never as the common size.
//
// Why the size test is what makes this a ground truth and not a copy of the
// detector: the timing recipe alone is looser than the rule (its tolerance
// over k = 1..3 is nearly continuous at short P), so on its own it would
// label almost anything the rule fires on and arm A would pass trivially.
// The rule never reads a payload size; human chat on a channel mixes sizes
// (block-quantised text of different lengths and sender names), while an
// automated sender repeating one message format does not. A periodic train
// of varied size - a verse bot, say - is therefore NOT labelled and stays a
// false alarm: the label can only excuse an episode when evidence the
// detector does not use agrees with it.
//
// Offline and file-only, like the rest of the tool: no database, no -db
// flag, no network, no clock. It reads an export only and refuses injected
// lines, so arm A's ground truth cannot come from the panel. Its output
// holds (channel hash, start, stop, P, size share) tuples and counts: no
// ids, hops, routes, size values, names or text.

// realLabelsRecipe restates the recipe in the output, so a labels file can
// be audited against it.
const realLabelsRecipe = "lines of -payload-type with a channel hash, not excluded, in [-start, -end); " +
	"per channel hash over every route group; segments split at gaps > max_harmonic*max_period + tolerance(max_period); " +
	"per segment: pulse merge = min(pulse_merge_gap, P0/2), P0 = median raw gap >= min_period/2; " +
	"#457 §4 runs (P = median pulse gap in [min_period, max_period], >= min_pulses pulses, every gap within " +
	"max(tolerance_abs, tolerance_rel*P) of k*P, k in 1..max_harmonic); " +
	"kept when one known payload size carries >= min_size_share of the run's lines"

// realSender is one labelled real automated sender: a channel hash and a
// time range with its period and the share of its lines that carry the
// common payload size. Nothing else is published.
type realSender struct {
	Channel   string    `json:"channel"`
	Start     time.Time `json:"start"`
	Stop      time.Time `json:"stop"`
	Period    dur       `json:"period"`
	SizeShare float64   `json:"size_share"`
}

// realLabels is the label file.
type realLabels struct {
	Experimental bool `json:"experimental"`
	// Real tells this file apart from inject's labels (synthetic): score
	// takes each through its own flag only.
	Real            bool          `json:"real"`
	PayloadType     int           `json:"payload_type"`
	ExcludeChannels []string      `json:"exclude_channels"`
	Start           time.Time     `json:"start"`
	End             time.Time     `json:"end"`
	Labelling       labellingSpec `json:"labelling"`
	MinSizeShare    float64       `json:"min_size_share"`
	Recipe          string        `json:"recipe"`
	InputLines      int           `json:"input_lines"`
	Considered      int           `json:"considered"`
	SkippedType     int           `json:"skipped_payload_type"`
	SkippedNoChan   int           `json:"skipped_no_channel"`
	SkippedExcluded int           `json:"skipped_excluded_channel"`
	SkippedWindow   int           `json:"skipped_out_of_window"`
	// Channels, Segments, TimingRuns and SizeRejected count the steps:
	// distinct hashes considered, activity segments, runs that passed the
	// timing recipe, and those of them the size test rejected.
	Channels     int          `json:"channels"`
	Segments     int          `json:"segments"`
	TimingRuns   int          `json:"timing_runs"`
	SizeRejected int          `json:"size_rejected"`
	Senders      []realSender `json:"senders"`
}

// labelOptions are the label command's explicit values.
type labelOptions struct {
	payloadType       int
	exclude           []string
	start, end        time.Time
	labelling         labellingSpec
	minSizeShare      float64
	maxLines, maxLine int
}

// labelLine is what the recipe needs of one line: its time and its payload
// size (-1 when the export did not know it).
type labelLine struct {
	t    int64
	size int
}

func runLabel(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("label", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "exported ReplayRecord JSON Lines, or - for stdin (required)")
	payloadType := fs.Int("payload-type", -1, "payload type to label (required)")
	exclude := fs.String("exclude-channels", "", "comma-separated channel hashes to leave out, lower-case hex")
	start := fs.String("start", "", "RFC 3339 start of the label window, inclusive (required)")
	end := fs.String("end", "", "RFC 3339 end of the label window, exclusive (required)")
	merge := fs.String("pulse-merge-gap", "", "events closer than this are one pulse, before the cadence cap (required)")
	minPeriod := fs.String("min-period", "", "shortest period P (required)")
	maxPeriod := fs.String("max-period", "", "longest period P (required)")
	minPulses := fs.Int("min-pulses", 0, "fewest pulses in a run (required)")
	maxHarmonic := fs.Int("max-harmonic", 0, "a gap may fit k*P for k in 1..this (required)")
	tolAbs := fs.String("tolerance-abs", "", "absolute per-gap tolerance (required)")
	tolRel := fs.String("tolerance-rel", "", "per-gap tolerance as a fraction of P (required)")
	minShare := fs.String("min-size-share", "", "least share of a run's lines on its common payload size, in (0, 1] (required)")
	maxLines := fs.Int("max-lines", 0, "most considered lines allowed (required, a hard bound)")
	maxLine := fs.Int("max-line-bytes", 0, "longest accepted input line (required)")
	out := fs.String("out", "", "labels file (must not exist); default stdout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(err error) int {
		fmt.Fprintf(stderr, "label: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("unexpected arguments"))
	}
	if *input == "" {
		return usageErr(errors.New("-input is required"))
	}
	if *payloadType < 0 || *payloadType > int(anomaly.MaxPayloadType) {
		return usageErr(fmt.Errorf("-payload-type is required and must be in 0..%d", anomaly.MaxPayloadType))
	}
	if *maxLines <= 0 || *maxLine <= 0 {
		return usageErr(errors.New("-max-lines and -max-line-bytes must be > 0"))
	}
	opt := labelOptions{payloadType: *payloadType, maxLines: *maxLines, maxLine: *maxLine}
	var err error
	if opt.exclude, err = parseExcludeList(*exclude); err != nil {
		return usageErr(err)
	}
	if opt.start, err = parseTime("start", *start); err != nil {
		return usageErr(err)
	}
	if opt.end, err = parseTime("end", *end); err != nil {
		return usageErr(err)
	}
	if !opt.end.After(opt.start) {
		return usageErr(errors.New("-end must be after -start"))
	}
	l := &opt.labelling
	l.MinPulses, l.MaxHarmonic = *minPulses, *maxHarmonic
	for _, d := range []struct {
		name, v string
		dst     *dur
	}{{"pulse-merge-gap", *merge, &l.PulseMergeGap}, {"min-period", *minPeriod, &l.MinPeriod},
		{"max-period", *maxPeriod, &l.MaxPeriod}, {"tolerance-abs", *tolAbs, &l.ToleranceAbs}} {
		if d.v == "" {
			return usageErr(fmt.Errorf("-%s is required", d.name))
		}
		v, err := time.ParseDuration(d.v)
		if err != nil {
			return usageErr(fmt.Errorf("-%s: %w", d.name, err))
		}
		*d.dst = dur(v)
	}
	if *tolRel == "" || *minShare == "" {
		return usageErr(errors.New("-tolerance-rel and -min-size-share are required"))
	}
	if l.ToleranceRel, err = strconv.ParseFloat(*tolRel, 64); err != nil {
		return usageErr(fmt.Errorf("-tolerance-rel: %w", err))
	}
	if opt.minSizeShare, err = strconv.ParseFloat(*minShare, 64); err != nil {
		return usageErr(fmt.Errorf("-min-size-share: %w", err))
	}
	if !(opt.minSizeShare > 0 && opt.minSizeShare <= 1) {
		return usageErr(errors.New("-min-size-share must be in (0, 1]"))
	}
	if err := l.validate(); err != nil {
		return usageErr(err)
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "label: %v\n", err)
		return exitFail
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
	res, err := labelReal(in, opt)
	if err != nil {
		return fail(err)
	}
	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		return fail(err)
	}
	b = append(b, '\n')
	if *out == "" {
		_, err = stdout.Write(b)
	} else {
		err = writeNew(*out, b)
	}
	if err != nil {
		return fail(err)
	}
	return exitOK
}

// labelReal reads the export and labels its real automated senders.
func labelReal(r io.Reader, opt labelOptions) (realLabels, error) {
	res := realLabels{Experimental: true, Real: true, PayloadType: opt.payloadType,
		ExcludeChannels: opt.exclude, Start: opt.start, End: opt.end, Labelling: opt.labelling,
		MinSizeShare: opt.minSizeShare, Recipe: realLabelsRecipe, Senders: []realSender{}}
	excluded := map[string]bool{}
	for _, v := range opt.exclude {
		excluded[v] = true
	}
	byChannel := map[string][]labelLine{}
	sc := bufio.NewScanner(r)
	limit := opt.maxLine + 2
	initial := 4096
	if limit < initial {
		initial = limit
	}
	sc.Buffer(make([]byte, 0, initial), limit)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) > opt.maxLine {
			return res, errors.New("input line longer than -max-line-bytes")
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		res.InputLines++
		var rec anomaly.ReplayRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return res, fmt.Errorf("line %d: %w", res.InputLines, err)
		}
		if strings.HasPrefix(rec.ID, injPrefix) {
			return res, fmt.Errorf("line %d carries an injected id (%s...): label reads real traffic only", res.InputLines, injPrefix)
		}
		switch {
		case rec.PayloadType != opt.payloadType:
			res.SkippedType++
			continue
		case rec.Channel == "":
			res.SkippedNoChan++
			continue
		case excluded[rec.Channel]:
			res.SkippedExcluded++
			continue
		case rec.Time.Before(opt.start) || !rec.Time.Before(opt.end):
			res.SkippedWindow++
			continue
		}
		if _, err := rec.Event(); err != nil {
			return res, fmt.Errorf("line %d: %w", res.InputLines, err)
		}
		if res.Considered >= opt.maxLines {
			return res, errors.New("more than -max-lines considered lines; raise the bound")
		}
		res.Considered++
		size := -1
		if rec.PayloadBytes != nil {
			size = *rec.PayloadBytes
		}
		byChannel[rec.Channel] = append(byChannel[rec.Channel], labelLine{t: rec.Time.UnixNano(), size: size})
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("input: %w", err)
	}
	channels := make([]string, 0, len(byChannel))
	for ch := range byChannel {
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	res.Channels = len(channels)
	for _, ch := range channels {
		lines := byChannel[ch]
		// Time, then size: a total order, so the result does not depend on
		// the input's line order.
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].t != lines[j].t {
				return lines[i].t < lines[j].t
			}
			return lines[i].size < lines[j].size
		})
		for _, seg := range segments(lines, opt.labelling) {
			res.Segments++
			for _, s := range labelSegment(seg, opt.labelling) {
				res.TimingRuns++
				if s.SizeShare < opt.minSizeShare {
					res.SizeRejected++
					continue
				}
				s.Channel = ch
				res.Senders = append(res.Senders, s)
			}
		}
	}
	sort.SliceStable(res.Senders, func(i, j int) bool {
		if !res.Senders[i].Start.Equal(res.Senders[j].Start) {
			return res.Senders[i].Start.Before(res.Senders[j].Start)
		}
		return res.Senders[i].Channel < res.Senders[j].Channel
	})
	return res, nil
}

// segments splits one channel's sorted lines at every gap no §4 run can
// bridge: a run's gaps fit k*P + tolerance(P) with P <= max_period and
// k <= max_harmonic, so a longer gap always ends a run, and splitting there
// changes nothing but where P is estimated from.
func segments(lines []labelLine, l labellingSpec) [][]labelLine {
	maxP := int64(time.Duration(l.MaxPeriod))
	bridge := int64(l.MaxHarmonic)*maxP + l.tolerance(maxP)
	var out [][]labelLine
	from := 0
	for i := 1; i < len(lines); i++ {
		if lines[i].t-lines[i-1].t > bridge {
			out = append(out, lines[from:i])
			from = i
		}
	}
	if len(lines) > 0 {
		out = append(out, lines[from:])
	}
	return out
}

// labelSegment runs the recipe over one segment and returns every timing
// run with its size share; the caller applies the size bar.
func labelSegment(seg []labelLine, l labellingSpec) []realSender {
	times := make([]int64, len(seg))
	for i, ln := range seg {
		times[i] = ln.t
	}
	r := periodicRuns(times, l, pulseMerge(times, l))
	out := make([]realSender, 0, len(r.runs))
	for _, run := range r.runs {
		// The run's lines are every line of its pulses.
		lo, hi := r.first[run[0]], len(seg)
		if run[1]+1 < len(r.first) {
			hi = r.first[run[1]+1]
		}
		out = append(out, realSender{Start: time.Unix(0, r.pulses[run[0]]).UTC(),
			Stop: time.Unix(0, r.pulses[run[1]]).UTC(), Period: dur(r.period), SizeShare: sizeShare(seg[lo:hi])})
	}
	return out
}

// pulseMerge is the segment's pulse merge gap: pulse_merge_gap, capped at
// half the segment's raw cadence P0 so that two consecutive pulses of a
// train are never merged. P0 is the median raw gap of at least
// min_period/2; shorter gaps are multi-part parts or interleaved lines, not
// a cadence the recipe could label.
func pulseMerge(times []int64, l labellingSpec) time.Duration {
	merge := time.Duration(l.PulseMergeGap)
	floor := int64(time.Duration(l.MinPeriod)) / 2
	var raw []int64
	for i := 1; i < len(times); i++ {
		if g := times[i] - times[i-1]; g >= floor {
			raw = append(raw, g)
		}
	}
	if len(raw) == 0 {
		return merge
	}
	if half := time.Duration(medianInt64(raw) / 2); half < merge {
		return half
	}
	return merge
}

// sizeShare is the share of lines carrying the most common known payload
// size. Unknown sizes count in the denominator only.
func sizeShare(lines []labelLine) float64 {
	if len(lines) == 0 {
		return 0
	}
	counts := map[int]int{}
	best := 0
	for _, ln := range lines {
		if ln.size < 0 {
			continue
		}
		counts[ln.size]++
		if counts[ln.size] > best {
			best = counts[ln.size]
		}
	}
	return float64(best) / float64(len(lines))
}
