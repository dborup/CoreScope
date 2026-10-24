package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #474, round 4: ground truth for REAL traffic in arm A. `label` reads
// an export only and writes (channel hash, start, stop, P, size share)
// tuples for runs that carry the aggregate signature of an automated sender:
// a #457 §4 periodic run at channel-hash level whose lines share one payload
// size. Every channel byte, hop and time here is synthetic.

// r4Line is one synthetic background line. payload < 0 means the export did
// not know the payload length.
type r4Line struct {
	at      time.Time
	channel string
	hop     string
	payload int
	pt      int // 0 means GRP_TXT (5)
}

// r4JSONL renders lines in time order (stable), numbered tx1, tx2, ... as an
// export numbers them. The wire size leaves room for header and path.
func r4JSONL(t *testing.T, lines []r4Line) string {
	t.Helper()
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })
	var b strings.Builder
	for i, l := range lines {
		pt := l.pt
		if pt == 0 {
			pt = 5
		}
		hop := l.hop
		if hop == "" {
			hop = "1:0b"
		}
		rec := anomaly.ReplayRecord{ID: fmt.Sprintf("tx%d", i+1), Time: l.at, FinalAt: l.at.Add(30 * time.Second),
			PayloadType: pt, Channel: l.channel, RouteClass: "flood", RouteKind: "relay",
			FirstHop: hop, FullRoute: hop + "00", WireBytes: 40, Traffic: "unclassified", Decryption: "unknown"}
		if l.payload >= 0 {
			pb := l.payload
			rec.PayloadBytes = &pb
			rec.WireBytes = pb + 10
		}
		j, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

// r4BotOpts is one synthetic automated sender: a pulse every period with a
// non-cumulative per-pulse jitter (sd, clamped at max), missed pulses and
// multi-part pulses whose later parts follow within min(55 s, P/4) - the
// #130 shape the injection panel uses.
type r4BotOpts struct {
	channel   string
	start     time.Time
	period    time.Duration
	pulses    int
	jitterSD  time.Duration
	jitterMax time.Duration
	missed    float64
	multipart float64
	hops      []string // rotated per hopEvery pulses
	hopEvery  int
	sizes     []int // drawn uniformly per line; one value = constant size
	seed      int64
}

func r4Bot(o r4BotOpts) []r4Line {
	r := rand.New(rand.NewSource(o.seed))
	var out []r4Line
	mpd := o.period / 4
	if mpd > 55*time.Second {
		mpd = 55 * time.Second
	}
	hopEvery := o.hopEvery
	if hopEvery <= 0 {
		hopEvery = 1
	}
	for i := 0; i < o.pulses; i++ {
		if i > 0 && r.Float64() < o.missed {
			continue
		}
		j := time.Duration(r.NormFloat64() * float64(o.jitterSD))
		if j > o.jitterMax {
			j = o.jitterMax
		}
		if j < -o.jitterMax {
			j = -o.jitterMax
		}
		at := o.start.Add(time.Duration(i)*o.period + j).Truncate(time.Second)
		hop := ""
		if len(o.hops) > 0 {
			hop = o.hops[(i/hopEvery)%len(o.hops)]
		}
		parts := 1
		if r.Float64() < o.multipart {
			parts += 1 + r.Intn(2)
		}
		for p := 0; p < parts; p++ {
			pat := at
			if p > 0 {
				pat = at.Add(time.Duration(r.Int63n(int64(mpd)))).Truncate(time.Second)
			}
			out = append(out, r4Line{at: pat, channel: o.channel, hop: hop, payload: o.sizes[r.Intn(len(o.sizes))]})
		}
	}
	return out
}

// r4Chatter is human-like chatter on one channel: conversations arrive as a
// Poisson process (convPerHour), each lasts 5-40 min, and messages inside it
// follow exponential gaps of mean meanGap. Sizes are drawn from mix, a
// block-quantised payload histogram of the kind the e2e fixture's human
// channels carry (modal share 0.32-0.38 there).
func r4Chatter(channel string, start time.Time, span time.Duration, convPerHour float64, meanGap time.Duration,
	mix map[int]int, seed int64) []r4Line {
	r := rand.New(rand.NewSource(seed))
	var vals []int
	for v, n := range mix {
		for k := 0; k < n; k++ {
			vals = append(vals, v)
		}
	}
	sort.Ints(vals)
	var out []r4Line
	t := 0.0
	for {
		t += r.ExpFloat64() / convPerHour * 3600
		if t > span.Seconds() {
			return out
		}
		end := t + (5+35*r.Float64())*60
		for u := t; u < end && u < span.Seconds(); u += r.ExpFloat64() * meanGap.Seconds() {
			out = append(out, r4Line{at: start.Add(time.Duration(u * float64(time.Second))).Truncate(time.Second),
				channel: channel, payload: vals[r.Intn(len(vals))]})
		}
	}
}

// r4HumanMix is the d9 channel of the e2e fixture (2026-05-17): 37 GRP_TXT
// lines, modal payload size 51 on 12 of them (share 0.32).
var r4HumanMix = map[int]int{35: 12, 51: 12, 67: 7, 83: 3, 99: 3}

// r4Week is the label window the tests use: seven whole days from injT.
var r4Week = 7 * 24 * time.Hour

// r4Labels mirrors the label file. It is declared here, not taken from the
// tool, so the test pins the published shape.
type r4Labels struct {
	Experimental     bool     `json:"experimental"`
	Real             bool     `json:"real"`
	PayloadType      int      `json:"payload_type"`
	ExcludeChannels  []string `json:"exclude_channels"`
	Start            string   `json:"start"`
	End              string   `json:"end"`
	MinSizeShare     float64  `json:"min_size_share"`
	InputLines       int      `json:"input_lines"`
	Considered       int      `json:"considered"`
	SkippedType      int      `json:"skipped_payload_type"`
	SkippedNoChannel int      `json:"skipped_no_channel"`
	SkippedExcluded  int      `json:"skipped_excluded_channel"`
	SkippedWindow    int      `json:"skipped_out_of_window"`
	Channels         int      `json:"channels"`
	Segments         int      `json:"segments"`
	TimingRuns       int      `json:"timing_runs"`
	SizeRejected     int      `json:"size_rejected"`
	Senders          []struct {
		Channel   string    `json:"channel"`
		Start     time.Time `json:"start"`
		Stop      time.Time `json:"stop"`
		Period    dur       `json:"period"`
		SizeShare float64   `json:"size_share"`
	} `json:"senders"`
}

// r4LabelArgs are the round-4 values: the #457 §4 recipe and a 0.9 size
// share, over the test week.
func r4LabelArgs(input string, extra ...string) []string {
	a := []string{"label", "-input", input, "-payload-type", "5",
		"-start", injAt(0), "-end", injAt(r4Week),
		"-pulse-merge-gap", "60s", "-min-period", "1m", "-max-period", "1h", "-min-pulses", "8",
		"-max-harmonic", "3", "-tolerance-abs", "30s", "-tolerance-rel", "0.1",
		"-min-size-share", "0.9", "-max-lines", "1000000", "-max-line-bytes", "4096"}
	return append(a, extra...)
}

// r4Label writes background into a file, runs label and parses the result.
func r4Label(t *testing.T, background string, extra ...string) (r4Labels, []byte) {
	t.Helper()
	in := filepath.Join(t.TempDir(), "bg.jsonl")
	if err := os.WriteFile(in, []byte(background), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(r4LabelArgs(in, extra...), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("label exit %d: %s", code, stderr.String())
	}
	var l r4Labels
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := dec.Decode(&l); err != nil {
		t.Fatalf("label output %q: %v", stdout.String(), err)
	}
	return l, stdout.Bytes()
}

// periodNear reports whether got is within max(30 s, 0.1 P) of P - the
// matching tolerance score applies at k = 1.
func periodNear(got, p time.Duration) bool {
	tol := p / 10
	if tol < 30*time.Second {
		tol = 30 * time.Second
	}
	d := got - p
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// Real-bot shapes must be labelled: one tuple per train, at the train's own
// period, starting at its first pulse. The periods span the detector's
// range from its 1-minute floor, and the noisy shape is #130's (5 % missed,
// 30 % multi-part). The 60 s train is the case a fixed 60 s pulse merge
// cannot label: its pulses are 51-69 s apart, so half of them would be
// merged into the previous one.
func TestLabelRealBotShapesAreLabelled(t *testing.T) {
	for _, p := range []time.Duration{time.Minute, 90 * time.Second, 5 * time.Minute, 15 * time.Minute} {
		for _, noisy := range []bool{false, true} {
			o := r4BotOpts{channel: "56", start: injT.Add(26 * time.Hour), period: p,
				pulses: int(12 * time.Hour / p), jitterSD: 3 * time.Second, jitterMax: 9 * time.Second,
				sizes: []int{35}, seed: int64(p) + 1}
			if noisy {
				o.missed, o.multipart = 0.05, 0.3
			}
			lines := r4Bot(o)
			l, _ := r4Label(t, r4JSONL(t, lines))
			name := fmt.Sprintf("P=%v noisy=%v", p, noisy)
			if len(l.Senders) != 1 {
				t.Errorf("%s: %d senders, want 1 (%+v)", name, len(l.Senders), l)
				continue
			}
			s := l.Senders[0]
			if s.Channel != "56" || !periodNear(time.Duration(s.Period), p) {
				t.Errorf("%s: sender %+v, want channel 56 at about %v", name, s, p)
			}
			if d := s.Start.Sub(lines[0].at); d < 0 || d > p+9*time.Second {
				t.Errorf("%s: starts %v after the first pulse", name, d)
			}
			if s.SizeShare != 1 {
				t.Errorf("%s: size share %v, want 1", name, s.SizeShare)
			}
			if covered := s.Stop.Sub(s.Start); covered < 11*time.Hour {
				t.Errorf("%s: covers only %v of a 12 h train", name, covered)
			}
		}
	}
}

// Human-like chatter must not be labelled, however busy. Its timing alone
// does form §4 runs - the recipe's tolerance max(30 s, 0.1 P) over k = 1..3
// is wide at short P - so it is the size test, a signal the detector never
// reads, that keeps it out. timing_runs proves the size test was reached.
func TestLabelHumanChatterIsNotLabelled(t *testing.T) {
	timing := 0
	for i, c := range []struct {
		conv float64
		gap  time.Duration
	}{{0.5, 45 * time.Second}, {2, time.Minute}, {4, 2 * time.Minute}, {6, 30 * time.Second}} {
		lines := r4Chatter("d9", injT, r4Week, c.conv, c.gap, r4HumanMix, int64(100+i))
		l, _ := r4Label(t, r4JSONL(t, lines))
		if len(l.Senders) != 0 {
			t.Errorf("chatter %v/h, gap %v: %d senders labelled: %+v", c.conv, c.gap, len(l.Senders), l.Senders)
		}
		timing += l.TimingRuns
		if l.SizeRejected != l.TimingRuns {
			t.Errorf("chatter %v/h: size_rejected %d != timing_runs %d", c.conv, l.SizeRejected, l.TimingRuns)
		}
	}
	if timing == 0 {
		t.Fatal("no chatter formed a timing run, so the size test was never exercised")
	}
}

// The circularity guard: a perfectly regular train the periodic detector
// would fire on is NOT a real sender unless its lines also share one size.
// A bot that posts varied text (the September verse bot's kind) stays a
// false alarm in arm A, so the label can never be satisfied by timing - the
// thing the rule measures - alone.
func TestLabelRegularButVariedSizeTrainIsNotLabelled(t *testing.T) {
	lines := r4Bot(r4BotOpts{channel: "a2", start: injT.Add(time.Hour), period: 5 * time.Minute, pulses: 144,
		jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, sizes: []int{35, 51, 67, 83}, seed: 7})
	l, _ := r4Label(t, r4JSONL(t, lines))
	if l.TimingRuns != 1 || l.SizeRejected != 1 || len(l.Senders) != 0 {
		t.Fatalf("varied-size train: timing_runs %d, size_rejected %d, senders %d; want 1, 1, 0",
			l.TimingRuns, l.SizeRejected, len(l.Senders))
	}
}

// A channel silent for days (one line a day) that suddenly carries a steady
// one-minute constant-size burst is labelled from the burst's first pulse.
// The lone lines before it are not part of the sender.
func TestLabelSilentThenBurst(t *testing.T) {
	var lines []r4Line
	for d := 0; d < 4; d++ {
		lines = append(lines, r4Line{at: injT.Add(time.Duration(d)*24*time.Hour + 7*time.Hour), channel: "56", payload: 51})
	}
	burst := injT.Add(4*24*time.Hour + 20*time.Hour)
	lines = append(lines, r4Bot(r4BotOpts{channel: "56", start: burst, period: time.Minute, pulses: 600,
		jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, missed: 0.05, multipart: 0.3,
		sizes: []int{35}, seed: 56})...)
	l, _ := r4Label(t, r4JSONL(t, lines))
	if len(l.Senders) != 1 {
		t.Fatalf("%d senders, want 1: %+v", len(l.Senders), l.Senders)
	}
	s := l.Senders[0]
	if s.Start.Before(burst.Add(-9*time.Second)) || s.Start.After(burst.Add(time.Minute+9*time.Second)) {
		t.Errorf("sender starts %v, the burst at %v", s.Start, burst)
	}
	if !periodNear(time.Duration(s.Period), time.Minute) {
		t.Errorf("period %v, want about 1m", time.Duration(s.Period))
	}
}

// The same timing with constant size is labelled, with a varied size it is
// not, and the 0.9 bar is inclusive. A line whose payload length the export
// did not know counts in the run but never as the common size.
func TestLabelConstantVersusVariedSize(t *testing.T) {
	train := func(sizes []int) []r4Line {
		var out []r4Line
		for i, s := range sizes {
			out = append(out, r4Line{at: injT.Add(time.Hour + time.Duration(i)*5*time.Minute), channel: "7f", payload: s})
		}
		return out
	}
	rep := func(v, n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	for _, c := range []struct {
		name  string
		sizes []int
		share float64 // 0 = not labelled
	}{
		{"constant", rep(35, 20), 1},
		{"exactly 0.9", append(rep(35, 18), 51, 67), 0.9},
		{"0.85", append(rep(35, 17), 51, 67, 83), 0},
		{"varied", []int{35, 51, 67, 83, 35, 51, 67, 83, 35, 51, 67, 83, 35, 51, 67, 83, 35, 51, 67, 83}, 0},
		{"0.9 with one unknown", append(rep(35, 18), -1, 51), 0.9},
		{"unknown sizes are not the common size", append(rep(35, 17), -1, -1, 51), 0},
	} {
		l, _ := r4Label(t, r4JSONL(t, train(c.sizes)))
		if l.TimingRuns != 1 {
			t.Errorf("%s: timing_runs %d, want 1", c.name, l.TimingRuns)
			continue
		}
		switch {
		case c.share == 0 && len(l.Senders) != 0:
			t.Errorf("%s: labelled %+v", c.name, l.Senders)
		case c.share != 0 && (len(l.Senders) != 1 || l.Senders[0].SizeShare != c.share):
			t.Errorf("%s: senders %+v, want one at share %v", c.name, l.Senders, c.share)
		}
	}
}

// Channel-hash level, not route group: a sender whose lines enter through
// changing first hops (route groups) is one sender at its own period. Keyed
// by route group, each hop would see a third of the pulses at 3P.
func TestLabelIsChannelLevelAcrossRouteGroups(t *testing.T) {
	lines := r4Bot(r4BotOpts{channel: "56", start: injT.Add(time.Hour), period: 2 * time.Minute, pulses: 300,
		jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, sizes: []int{35},
		hops: []string{"1:a1", "2:b2c3", "1:d4"}, seed: 3})
	l, _ := r4Label(t, r4JSONL(t, lines))
	if len(l.Senders) != 1 || !periodNear(time.Duration(l.Senders[0].Period), 2*time.Minute) {
		t.Fatalf("senders %+v, want one at about 2m", l.Senders)
	}
}

// The period is estimated per activity segment, so a sender is labelled at
// its own period even when the same hash carried days of chatter earlier.
// A segment ends at a gap no §4 run could bridge (more than max_harmonic *
// max_period plus the tolerance), so splitting there never cuts a run.
func TestLabelEstimatesThePeriodPerSegment(t *testing.T) {
	lines := r4Chatter("c3", injT, 3*24*time.Hour, 2, time.Minute, r4HumanMix, 9)
	lines = append(lines, r4Bot(r4BotOpts{channel: "c3", start: injT.Add(4 * 24 * time.Hour), period: 5 * time.Minute,
		pulses: 144, jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, sizes: []int{51}, seed: 11})...)
	l, _ := r4Label(t, r4JSONL(t, lines))
	if len(l.Senders) != 1 || !periodNear(time.Duration(l.Senders[0].Period), 5*time.Minute) {
		t.Fatalf("senders %+v, want one at about 5m", l.Senders)
	}
	if l.Segments < 2 {
		t.Errorf("segments %d, want the chatter and the sender apart", l.Segments)
	}
}

// A run is never split by the segmentation: a 1-hour sender with missed
// pulses (gaps of 2P and 3P) stays one sender.
func TestLabelSegmentsNeverCutARun(t *testing.T) {
	var lines []r4Line
	k := 0
	for _, step := range []int{1, 1, 3, 1, 1, 2, 1, 1, 1, 3, 1} {
		k += step
		lines = append(lines, r4Line{at: injT.Add(time.Duration(k) * time.Hour), channel: "7f", payload: 35})
	}
	l, _ := r4Label(t, r4JSONL(t, lines))
	if len(l.Senders) != 1 || l.Segments != 1 {
		t.Fatalf("segments %d, senders %+v; want one segment, one sender", l.Segments, l.Senders)
	}
}

// Only the requested payload type, lines with a channel hash, channels not
// excluded and lines inside [-start, -end) are labelled; every skip is
// counted.
func TestLabelFilterIsMechanicalAndCounted(t *testing.T) {
	bot := func(ch string, start time.Time, pt int) []r4Line {
		var out []r4Line
		for i := 0; i < 20; i++ {
			out = append(out, r4Line{at: start.Add(time.Duration(i) * 5 * time.Minute), channel: ch, payload: 35, pt: pt})
		}
		return out
	}
	var lines []r4Line
	lines = append(lines, bot("56", injT.Add(time.Hour), 0)...)
	lines = append(lines, bot("81", injT.Add(time.Hour), 0)...)        // excluded
	lines = append(lines, bot("6f", injT.Add(time.Hour), 6)...)        // another payload type
	lines = append(lines, bot("", injT.Add(time.Hour), 0)...)          // no channel hash
	lines = append(lines, bot("5a", injT.Add(-3*time.Hour), 0)...)     // before -start
	lines = append(lines, bot("5b", injT.Add(r4Week+time.Hour), 0)...) // at or after -end
	l, _ := r4Label(t, r4JSONL(t, lines), "-exclude-channels", "81")
	if len(l.Senders) != 1 || l.Senders[0].Channel != "56" {
		t.Fatalf("senders %+v, want only 56", l.Senders)
	}
	if l.InputLines != 120 || l.Considered != 20 || l.SkippedExcluded != 20 || l.SkippedType != 20 ||
		l.SkippedNoChannel != 20 || l.SkippedWindow != 40 || l.Channels != 1 {
		t.Errorf("counters %+v", l)
	}
	if len(l.ExcludeChannels) != 1 || l.ExcludeChannels[0] != "81" || !l.Real || !l.Experimental ||
		l.PayloadType != 5 || l.MinSizeShare != 0.9 {
		t.Errorf("provenance %+v", l)
	}
}

// The label tool reads real traffic only: a line carrying an injected id is
// refused, so arm A's ground truth can never come from the panel.
func TestLabelRefusesInjectedLines(t *testing.T) {
	bg := r4JSONL(t, []r4Line{{at: injT.Add(time.Hour), channel: "56", payload: 35}})
	bg = strings.Replace(bg, `"id":"tx1"`, `"id":"inj-b0-1-0"`, 1)
	in := filepath.Join(t.TempDir(), "bg.jsonl")
	if err := os.WriteFile(in, []byte(bg), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(r4LabelArgs(in), nil, &stdout, &stderr); code != exitFail || !strings.Contains(stderr.String(), "injected") {
		t.Fatalf("exit %d, stderr %q; want a refusal", code, stderr.String())
	}
}

// The output holds tuples only: per sender exactly the channel hash, start,
// stop, period and size share. No hop, route, size value, id or count of a
// line is published.
func TestLabelOutputHoldsTuplesOnly(t *testing.T) {
	lines := r4Bot(r4BotOpts{channel: "56", start: injT.Add(time.Hour), period: 5 * time.Minute, pulses: 40,
		jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, sizes: []int{77}, hops: []string{"3:fdfdfd"}, seed: 5})
	_, raw := r4Label(t, r4JSONL(t, lines))
	var out struct {
		Senders []map[string]json.RawMessage `json:"senders"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Senders) != 1 {
		t.Fatalf("senders %d", len(out.Senders))
	}
	var keys []string
	for k := range out.Senders[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "channel,period,size_share,start,stop" {
		t.Errorf("sender fields %s", got)
	}
	for _, leak := range []string{"fdfdfd", "tx1", `"77"`, ":77", "relay", "flood"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("output carries %q", leak)
		}
	}
}

// The result is a pure function of the lines: the same input gives the same
// bytes, and so does the same input in another line order.
func TestLabelIsDeterministic(t *testing.T) {
	lines := r4Chatter("d9", injT, 2*24*time.Hour, 2, time.Minute, r4HumanMix, 4)
	lines = append(lines, r4Bot(r4BotOpts{channel: "56", start: injT.Add(5 * time.Hour), period: 90 * time.Second,
		pulses: 200, jitterSD: 3 * time.Second, jitterMax: 9 * time.Second, sizes: []int{35}, seed: 8})...)
	bg := r4JSONL(t, lines)
	_, a := r4Label(t, bg)
	_, b := r4Label(t, bg)
	ls := strings.Split(strings.TrimSpace(bg), "\n")
	for i, j := 0, len(ls)-1; i < j; i, j = i+1, j-1 {
		ls[i], ls[j] = ls[j], ls[i]
	}
	_, c := r4Label(t, strings.Join(ls, "\n")+"\n")
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatalf("label output depends on the run or the line order")
	}
}

// Every value is an explicit flag; there are no defaults. A missing or
// invalid value is a usage error, -max-lines is a hard bound, and -out is
// never overwritten.
func TestLabelRequiresExplicitFlags(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "bg.jsonl")
	bg := r4JSONL(t, []r4Line{{at: injT.Add(time.Hour), channel: "56", payload: 35},
		{at: injT.Add(2 * time.Hour), channel: "56", payload: 35}})
	if err := os.WriteFile(in, []byte(bg), 0o644); err != nil {
		t.Fatal(err)
	}
	full := r4LabelArgs(in)
	for i := 1; i < len(full); i += 2 {
		args := append(append([]string{}, full[:i]...), full[i+2:]...)
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != exitUsage {
			t.Errorf("without %s: exit %d, want %d", full[i], code, exitUsage)
		}
	}
	for _, bad := range [][2]string{{"-min-size-share", "0"}, {"-min-size-share", "1.5"}, {"-min-pulses", "1"},
		{"-tolerance-rel", "-0.1"}, {"-max-period", "30s"}, {"-pulse-merge-gap", "0s"}, {"-payload-type", "99"},
		{"-exclude-channels", "8G"}, {"-end", injAt(0)}} {
		args := append([]string{}, full...)
		for i := range args {
			if args[i] == bad[0] {
				args[i+1] = bad[1]
			}
		}
		var stdout, stderr bytes.Buffer
		if bad[0] == "-exclude-channels" {
			args = append(args, bad[0], bad[1])
		}
		if code := run(args, nil, &stdout, &stderr); code != exitUsage {
			t.Errorf("%s %s: exit %d, want %d", bad[0], bad[1], code, exitUsage)
		}
	}
	var stdout, stderr bytes.Buffer
	args := append([]string{}, full...)
	for i := range args {
		if args[i] == "-max-lines" {
			args[i+1] = "1"
		}
	}
	if code := run(args, nil, &stdout, &stderr); code != exitFail {
		t.Errorf("over -max-lines: exit %d, want %d", code, exitFail)
	}
	out := filepath.Join(dir, "labels-real.json")
	if code := run(append(r4LabelArgs(in), "-out", out), nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("first write: exit %d %s", code, stderr.String())
	}
	if code := run(append(r4LabelArgs(in), "-out", out), nil, &stdout, &stderr); code != exitFail {
		t.Errorf("overwrite: exit %d, want %d", code, exitFail)
	}
}
