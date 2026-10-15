package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #474, phase 3: score a replay report of the periodic rule against
// the injected ground truth (or against no ground truth at all, which
// measures false alarms on real background traffic).
//
// The matching and the metrics are the #457 §5 periodic rules, with the
// labels coming from `inject` instead of from a hand-labelled channel:
//
//   - An episode is one EventID. Its activation is its first transition to
//     active; its end is its last transition to ended, or the report's end if
//     it never ended. Only episodes activated inside [-start, -end) count.
//   - An episode is a true positive for an incident when they are the same
//     series, [activation, end] overlaps [incident start, stop + 4P], and
//     the episode's period is within the labelling tolerance of k*P for some
//     k in 1..max_harmonic. An episode whose activation carries no periodic
//     evidence can never be a true positive.
//   - Same series means the same stream key when the incident carries one
//     (inject writes it), and the same channel hash otherwise. The key is
//     what makes a panel placed on real traffic's own keys scorable: on a
//     busy channel, several route groups share the channel hash, and
//     matching on the hash alone would credit any of them to the bot.
//   - -baseline takes the report of the same window without the injection
//     (arm A). An episode that already exists there, on the same key and
//     overlapping in time, is attributed to the background: it is neither a
//     true positive nor a false positive, and is counted on its own. That is
//     the differential reading a shared key needs - a detection in arm B
//     counts only when it is absent from arm A.
//   - Every other episode is a false positive. With no labels file, every
//     episode is a false positive, which is the false-alarm arm.
//   - -bots narrows which incidents the recall and the medians cover, for
//     example to leave a non-blind sanity-check bot out of the primary
//     numbers. It never turns a matched episode into a false positive: an
//     episode that matches any labelled incident is a true positive
//     whether or not that incident was selected.
//
// Scoring reads files only: a report, a labels file and nothing else.

// scoreCandidate is the part of a report candidate scoring needs. The report
// is this tool's own output, so unknown fields are ignored rather than
// rejected; the fields below are checked instead.
type scoreCandidate struct {
	EventID  string
	Rule     string
	Kind     string
	Key      string
	To       string
	At       time.Time
	Periodic *struct {
		Period time.Duration
	} `json:",omitempty"`
}

type scoreReport struct {
	Experimental bool
	Start, End   time.Time
	Candidates   []scoreCandidate
}

// episode is one EventID's activation and end.
type episode struct {
	channel    string
	key        string
	activation time.Time
	end        time.Time
	hasPeriod  bool
	period     time.Duration
	matches    int
	// nearMiss: the episode overlapped an incident on the same series but
	// failed the period check.
	nearMiss bool
	// baseline: an episode on the same key, overlapping in time, is already
	// in the -baseline report, so this one is not the injection's doing.
	baseline bool
}

// incidentScore reports one labelled incident's outcome.
type incidentScore struct {
	Bot          string  `json:"bot"`
	Channel      string  `json:"channel"`
	Start        string  `json:"start"`
	Stop         string  `json:"stop"`
	Period       dur     `json:"period"`
	Episodes     int     `json:"episodes"`
	Detected     bool    `json:"detected"`
	TimeToDetect *dur    `json:"time_to_detect,omitempty"`
	TTDPeriods   float64 `json:"time_to_detect_periods,omitempty"`
	StopLatency  *dur    `json:"stop_latency,omitempty"`
}

// scoreResult is the metrics file.
type scoreResult struct {
	Experimental bool    `json:"experimental"`
	Rule         string  `json:"rule"`
	Start        string  `json:"start"`
	End          string  `json:"end"`
	Days         float64 `json:"days"`
	// Labels and Baseline are provenance: the file names only, never the
	// paths they were read from (a path can carry a user or host name).
	Labels   string `json:"labels"`
	Baseline string `json:"baseline"`
	// Bots is the -bots filter, empty when every labelled bot counts.
	Bots     []string `json:"bots"`
	Episodes int      `json:"episodes"`
	// EpisodesWithoutPeriod counts episodes whose activation candidate
	// carried no periodic evidence; they can never be true positives.
	EpisodesWithoutPeriod int `json:"episodes_without_period"`
	// BaselineEpisodes counts episodes the -baseline report already has on
	// the same key, overlapping in time. They are neither true nor false
	// positives here: they are the background's, not the injection's.
	BaselineEpisodes  int     `json:"baseline_episodes"`
	TruePositives     int     `json:"true_positives"`
	FalsePositives    int     `json:"false_positives"`
	FalseAlarmsPerDay float64 `json:"false_alarms_per_day"`
	// PeriodMismatches counts episodes that overlap an incident on the same
	// channel but fail the period check (#457 "also reported").
	PeriodMismatches int `json:"period_mismatches"`
	// Incidents is every labelled incident; IncidentsSelected is the subset
	// the bars cover (all of them unless -bots narrows it).
	Incidents         int     `json:"incidents"`
	IncidentsSelected int     `json:"incidents_selected"`
	IncidentsDetected int     `json:"incidents_detected"`
	Recall            float64 `json:"recall"`
	// Medians over the detected incidents. A median hides the slow tail:
	// clean trains alarm at the earliest pulse the rule allows, so half the
	// panel can sit at that floor while the realistic shapes take many times
	// longer. The p75 and the maximum are reported beside every median so
	// the tail is visible without reading the per-incident table.
	MedianTimeToDetect        *dur            `json:"median_time_to_detect,omitempty"`
	MedianTTDPeriods          float64         `json:"median_time_to_detect_periods,omitempty"`
	P75TimeToDetect           *dur            `json:"p75_time_to_detect,omitempty"`
	P75TTDPeriods             float64         `json:"p75_time_to_detect_periods,omitempty"`
	MaxTimeToDetect           *dur            `json:"max_time_to_detect,omitempty"`
	MaxTTDPeriods             float64         `json:"max_time_to_detect_periods,omitempty"`
	MedianStopLatency         *dur            `json:"median_stop_latency,omitempty"`
	P75StopLatency            *dur            `json:"p75_stop_latency,omitempty"`
	MaxStopLatency            *dur            `json:"max_stop_latency,omitempty"`
	MedianEpisodesPerIncident float64         `json:"median_episodes_per_detected_incident,omitempty"`
	MaxEpisodesPerIncident    float64         `json:"max_episodes_per_detected_incident,omitempty"`
	PerIncident               []incidentScore `json:"per_incident"`
	// FalsePositivesByChannel lists the false positives per channel hash,
	// sorted by hash: channel hashes and counts only.
	FalsePositivesByChannel []channelCount `json:"false_positives_by_channel"`
}

type channelCount struct {
	Channel string `json:"channel"`
	Count   int    `json:"count"`
}

func runScore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("score", flag.ContinueOnError)
	fs.SetOutput(stderr)
	report := fs.String("report", "", "replay report JSON (required)")
	labels := fs.String("labels", "", "injected labels JSON; omitted means no ground truth (false alarms only)")
	baseline := fs.String("baseline", "", "replay report of the same window without the injection; episodes it already has on the same key are the background's")
	rule := fs.String("rule", "", "periodic rule name to score (required)")
	bots := fs.String("bots", "", "comma-separated bot names whose incidents the bars cover; empty means all")
	start := fs.String("start", "", "RFC 3339 start of the evaluation window, inclusive (required)")
	end := fs.String("end", "", "RFC 3339 end of the evaluation window, exclusive (required)")
	out := fs.String("out", "", "metrics file (must not exist); default stdout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(err error) int {
		fmt.Fprintf(stderr, "score: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("unexpected arguments"))
	}
	if *report == "" || *rule == "" {
		return usageErr(errors.New("-report and -rule are required"))
	}
	var opt struct{ start, end time.Time }
	var err error
	if opt.start, err = parseTime("start", *start); err != nil {
		return usageErr(err)
	}
	if opt.end, err = parseTime("end", *end); err != nil {
		return usageErr(err)
	}
	if !opt.end.After(opt.start) {
		return usageErr(errors.New("-end must be after -start"))
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "score: %v\n", err)
		return exitFail
	}
	rb, err := os.ReadFile(*report)
	if err != nil {
		return fail(err)
	}
	rep, err := parseScoreReport(rb)
	if err != nil {
		return fail(err)
	}
	var lab InjectLabels
	if *labels != "" {
		lb, err := os.ReadFile(*labels)
		if err != nil {
			return fail(err)
		}
		if lab, err = parseInjectLabels(lb); err != nil {
			return fail(err)
		}
	}
	var base []episode
	if *baseline != "" {
		bb, err := os.ReadFile(*baseline)
		if err != nil {
			return fail(err)
		}
		brep, err := parseScoreReport(bb)
		if err != nil {
			return fail(fmt.Errorf("baseline %w", err))
		}
		if base, _, err = episodes(brep, *rule, opt.start, opt.end); err != nil {
			return fail(fmt.Errorf("baseline %w", err))
		}
	}
	want := []string{}
	for _, b := range strings.Split(*bots, ",") {
		if b = strings.TrimSpace(b); b != "" {
			want = append(want, b)
		}
	}
	res, err := score(rep, lab, *rule, *labels, *baseline, base, want, opt.start, opt.end)
	if err == nil && len(want) > 0 && res.IncidentsSelected == 0 {
		return fail(errors.New("-bots selected no labelled incident"))
	}
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

func parseScoreReport(b []byte) (scoreReport, error) {
	var r scoreReport
	if err := json.Unmarshal(b, &r); err != nil {
		return scoreReport{}, fmt.Errorf("report: %w", err)
	}
	if !r.Experimental || r.Start.IsZero() || r.End.IsZero() || !r.End.After(r.Start) {
		return scoreReport{}, errors.New("report: not a replay report (Experimental, Start, End)")
	}
	for i, c := range r.Candidates {
		if c.EventID == "" || c.Rule == "" || c.Kind == "" || c.To == "" || c.At.IsZero() {
			return scoreReport{}, fmt.Errorf("report: candidate %d is missing EventID, Rule, Kind, To or At", i)
		}
		if _, err := anomaly.ParseKey(c.Key); err != nil {
			return scoreReport{}, fmt.Errorf("report: candidate %d: %w", i, err)
		}
	}
	return r, nil
}

func parseInjectLabels(b []byte) (InjectLabels, error) {
	var l InjectLabels
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return InjectLabels{}, fmt.Errorf("labels: %w", err)
	}
	if !onlyJSONValue(dec, b) {
		return InjectLabels{}, errors.New("labels: trailing data")
	}
	if !l.Synthetic {
		return InjectLabels{}, errors.New("labels: not an injected labels file (synthetic)")
	}
	if err := l.Labelling.validate(); err != nil {
		return InjectLabels{}, fmt.Errorf("labels: %w", err)
	}
	for i, inc := range l.Incidents {
		if inc.Channel == "" || inc.Start.IsZero() || inc.Stop.IsZero() || inc.Stop.Before(inc.Start) || time.Duration(inc.Period) <= 0 {
			return InjectLabels{}, fmt.Errorf("labels: incident %d is incomplete", i)
		}
	}
	return l, nil
}

// score computes the metrics. labelsPath and baselinePath are reported as
// provenance, by file name only; baseline, when given, are the episodes of
// the same window without the injection; bots, when not empty, selects which
// bots' incidents the bars cover.
func score(rep scoreReport, lab InjectLabels, rule, labelsPath, baselinePath string, baseline []episode,
	bots []string, start, end time.Time) (scoreResult, error) {
	eps, noPeriod, err := episodes(rep, rule, start, end)
	if err != nil {
		return scoreResult{}, err
	}
	markBaseline(eps, baseline)
	days := end.Sub(start).Hours() / 24
	res := scoreResult{Experimental: true, Rule: rule, Start: start.Format(time.RFC3339),
		End: end.Format(time.RFC3339), Days: days, Labels: filepath.Base(labelsPath),
		Baseline: filepath.Base(baselinePath), Episodes: len(eps),
		EpisodesWithoutPeriod: noPeriod, Incidents: len(lab.Incidents), Bots: nonNil(bots),
		PerIncident: []incidentScore{}, FalsePositivesByChannel: []channelCount{}}
	if labelsPath == "" {
		res.Labels = ""
	}
	if baselinePath == "" {
		res.Baseline = ""
	}
	selected := func(bot string) bool {
		if len(bots) == 0 {
			return true
		}
		for _, b := range bots {
			if b == bot {
				return true
			}
		}
		return false
	}
	l := lab.Labelling
	var ttds, stops []int64
	var perInc []float64
	for _, inc := range lab.Incidents {
		p := time.Duration(inc.Period)
		is := incidentScore{Bot: inc.Bot, Channel: inc.Channel, Start: inc.Start.Format(time.RFC3339),
			Stop: inc.Stop.Format(time.RFC3339), Period: inc.Period}
		var firstAct, lastEnd time.Time
		for i := range eps {
			e := &eps[i]
			if !sameSeries(*e, inc) || !overlaps(*e, inc, p) {
				continue
			}
			if e.baseline {
				continue // the background's episode, counted once below
			}
			if !e.hasPeriod {
				continue // counted once, as EpisodesWithoutPeriod
			}
			if !periodFits(e.period, p, l) {
				e.nearMiss = true
				continue
			}
			e.matches++
			is.Episodes++
			if firstAct.IsZero() || e.activation.Before(firstAct) {
				firstAct = e.activation
			}
			if lastEnd.IsZero() || e.end.After(lastEnd) {
				lastEnd = e.end
			}
		}
		if is.Episodes > 0 {
			is.Detected = true
			ttd := dur(firstAct.Sub(inc.Start))
			sl := dur(lastEnd.Sub(inc.Stop))
			is.TimeToDetect, is.StopLatency = &ttd, &sl
			is.TTDPeriods = float64(firstAct.Sub(inc.Start)) / float64(p)
		}
		// Only selected incidents enter the recall and the medians; an
		// episode that matched an unselected incident is still a match, so
		// it never becomes a false positive.
		if !selected(inc.Bot) {
			continue
		}
		res.IncidentsSelected++
		if is.Episodes > 0 {
			res.IncidentsDetected++
			ttds = append(ttds, int64(*is.TimeToDetect))
			stops = append(stops, int64(*is.StopLatency))
			perInc = append(perInc, float64(is.Episodes))
		}
		res.PerIncident = append(res.PerIncident, is)
	}
	byChannel := map[string]int{}
	for _, e := range eps {
		if e.baseline {
			res.BaselineEpisodes++
			continue
		}
		if e.matches == 0 && e.nearMiss {
			res.PeriodMismatches++
		}
		if e.matches > 0 {
			res.TruePositives++
			continue
		}
		res.FalsePositives++
		byChannel[e.channel]++
	}
	if days > 0 {
		res.FalseAlarmsPerDay = float64(res.FalsePositives) / days
	}
	if res.IncidentsSelected > 0 {
		res.Recall = float64(res.IncidentsDetected) / float64(res.IncidentsSelected)
	}
	if len(ttds) > 0 {
		tp := ttdPeriods(res.PerIncident)
		m, p75, mx := dur(medianInt64(ttds)), dur(percentileInt64(ttds, 0.75)), dur(percentileInt64(ttds, 1))
		res.MedianTimeToDetect, res.P75TimeToDetect, res.MaxTimeToDetect = &m, &p75, &mx
		res.MedianTTDPeriods = medianFloat(tp)
		res.P75TTDPeriods = percentileFloat(tp, 0.75)
		res.MaxTTDPeriods = percentileFloat(tp, 1)
		sm, s75, smx := dur(medianInt64(stops)), dur(percentileInt64(stops, 0.75)), dur(percentileInt64(stops, 1))
		res.MedianStopLatency, res.P75StopLatency, res.MaxStopLatency = &sm, &s75, &smx
		res.MedianEpisodesPerIncident = medianFloat(perInc)
		res.MaxEpisodesPerIncident = percentileFloat(perInc, 1)
	}
	for ch, n := range byChannel {
		res.FalsePositivesByChannel = append(res.FalsePositivesByChannel, channelCount{Channel: ch, Count: n})
	}
	sort.Slice(res.FalsePositivesByChannel, func(i, j int) bool {
		return res.FalsePositivesByChannel[i].Channel < res.FalsePositivesByChannel[j].Channel
	})
	return res, nil
}

// ttdPeriods collects the time to detect in units of P over the detected
// incidents.
func ttdPeriods(per []incidentScore) []float64 {
	var out []float64
	for _, p := range per {
		if p.Detected {
			out = append(out, p.TTDPeriods)
		}
	}
	return out
}

// episodes groups a report's candidates for one rule into episodes, keeping
// those activated inside [start, end).
func episodes(rep scoreReport, rule string, start, end time.Time) ([]episode, int, error) {
	type agg struct {
		channel        string
		key            string
		act, endT      time.Time
		actSet, endSet bool
		period         time.Duration
		hasPeriod      bool
	}
	order := []string{}
	m := map[string]*agg{}
	for _, c := range rep.Candidates {
		if c.Rule != rule || c.Kind != "periodic" {
			continue
		}
		k, err := anomaly.ParseKey(c.Key)
		if err != nil {
			return nil, 0, fmt.Errorf("report: %w", err)
		}
		a := m[c.EventID]
		if a == nil {
			// ParseKey accepts only the canonical encoding, so k.String()
			// is c.Key: the key is comparable as a string.
			a = &agg{channel: hexChannel(k), key: k.String()}
			m[c.EventID] = a
			order = append(order, c.EventID)
		}
		switch c.To {
		case "active":
			// The first transition to active is the activation, and its
			// candidate carries the periodic evidence that is scored.
			if !a.actSet || c.At.Before(a.act) {
				a.act, a.actSet = c.At, true
				a.hasPeriod = c.Periodic != nil
				if c.Periodic != nil {
					a.period = c.Periodic.Period
				}
			}
		case "ended":
			if !a.endSet || c.At.After(a.endT) {
				a.endT, a.endSet = c.At, true
			}
		}
	}
	var out []episode
	noPeriod := 0
	for _, id := range order {
		a := m[id]
		if !a.actSet || a.act.Before(start) || !a.act.Before(end) {
			continue
		}
		e := episode{channel: a.channel, key: a.key, activation: a.act, end: a.endT,
			hasPeriod: a.hasPeriod, period: a.period}
		if !a.endSet {
			e.end = rep.End // never ended: the report's end, as #457 §1 says
		}
		if !e.hasPeriod {
			noPeriod++
		}
		out = append(out, e)
	}
	return out, noPeriod, nil
}

// hexChannel returns a key's channel hash as plain lower-case hex, the form
// the labels file uses ("" when the key carries no channel).
func hexChannel(k anomaly.Key) string {
	b := k.Channel.Bytes()
	if len(b) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", b)
}

// sameSeries reports whether an episode and an incident describe the same
// series: the same stream key when the incident carries one, and the same
// channel hash for a labels file written before inject recorded keys.
func sameSeries(e episode, inc Incident) bool {
	if inc.Key != "" {
		return e.key == inc.Key
	}
	return e.channel == inc.Channel
}

// markBaseline flags every episode that the baseline report already has on
// the same key, overlapping in time. Such an episode exists without the
// injection, so it says nothing about the injected bot either way.
func markBaseline(eps, baseline []episode) {
	if len(baseline) == 0 {
		return
	}
	byKey := map[string][]episode{}
	for _, b := range baseline {
		byKey[b.key] = append(byKey[b.key], b)
	}
	for i := range eps {
		for _, b := range byKey[eps[i].key] {
			if !b.activation.After(eps[i].end) && !b.end.Before(eps[i].activation) {
				eps[i].baseline = true
				break
			}
		}
	}
}

// overlaps reports whether an episode's [activation, end] overlaps the
// incident's [start, stop + 4P].
func overlaps(e episode, inc Incident, p time.Duration) bool {
	return !e.activation.After(inc.Stop.Add(4*p)) && !e.end.Before(inc.Start)
}

// periodFits reports whether the episode's period is within the labelling
// tolerance of k*P for some k in 1..MaxHarmonic.
func periodFits(got, p time.Duration, l labellingSpec) bool {
	tol := time.Duration(float64(p) * l.ToleranceRel)
	if a := time.Duration(l.ToleranceAbs); a > tol {
		tol = a
	}
	for k := 1; k <= l.MaxHarmonic; k++ {
		d := got - time.Duration(k)*p
		if d < 0 {
			d = -d
		}
		if d <= tol {
			return true
		}
	}
	return false
}

// nonNil keeps an empty filter out of the JSON as [] rather than null.
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func medianFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// percentileInt64 and percentileFloat are the nearest-rank percentile: the
// value at index ceil(q*n)-1 of the sorted values, so q = 1 is the maximum
// and q = 0.75 of eight values is the sixth. They sort a copy.
func percentileInt64(v []int64, q float64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[rankIndex(len(s), q)]
}

func percentileFloat(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[rankIndex(len(s), q)]
}

func rankIndex(n int, q float64) int {
	i := int(math.Ceil(q*float64(n))) - 1
	if i < 0 {
		return 0
	}
	if i > n-1 {
		return n - 1
	}
	return i
}
