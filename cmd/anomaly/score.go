package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
//   - An episode is a true positive for an incident when the channel hashes
//     are equal, [activation, end] overlaps [incident start, stop + 4P], and
//     the episode's period is within the labelling tolerance of k*P for some
//     k in 1..max_harmonic. An episode whose activation carries no periodic
//     evidence can never be a true positive.
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
	id         string
	channel    string
	activation time.Time
	end        time.Time
	hasPeriod  bool
	period     time.Duration
	matches    int
	// nearMiss: the episode overlapped an incident on the same channel but
	// failed the period check.
	nearMiss bool
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
	Labels       string  `json:"labels"`
	// Bots is the -bots filter, empty when every labelled bot counts.
	Bots     []string `json:"bots"`
	Episodes int      `json:"episodes"`
	// EpisodesWithoutPeriod counts episodes whose activation candidate
	// carried no periodic evidence; they can never be true positives.
	EpisodesWithoutPeriod int     `json:"episodes_without_period"`
	TruePositives         int     `json:"true_positives"`
	FalsePositives        int     `json:"false_positives"`
	FalseAlarmsPerDay     float64 `json:"false_alarms_per_day"`
	// PeriodMismatches counts episodes that overlap an incident on the same
	// channel but fail the period check (#457 "also reported").
	PeriodMismatches int `json:"period_mismatches"`
	// Incidents is every labelled incident; IncidentsSelected is the subset
	// the bars cover (all of them unless -bots narrows it).
	Incidents         int     `json:"incidents"`
	IncidentsSelected int     `json:"incidents_selected"`
	IncidentsDetected int     `json:"incidents_detected"`
	Recall            float64 `json:"recall"`
	// Medians over the detected incidents.
	MedianTimeToDetect        *dur            `json:"median_time_to_detect,omitempty"`
	MedianTTDPeriods          float64         `json:"median_time_to_detect_periods,omitempty"`
	MedianStopLatency         *dur            `json:"median_stop_latency,omitempty"`
	MedianEpisodesPerIncident float64         `json:"median_episodes_per_detected_incident,omitempty"`
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
	var want []string
	for _, b := range strings.Split(*bots, ",") {
		if b = strings.TrimSpace(b); b != "" {
			want = append(want, b)
		}
	}
	res, err := score(rep, lab, *rule, *labels, want, opt.start, opt.end)
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

// score computes the metrics. labelsPath is reported as provenance only;
// bots, when not empty, selects which bots' incidents the bars cover.
func score(rep scoreReport, lab InjectLabels, rule, labelsPath string, bots []string, start, end time.Time) (scoreResult, error) {
	eps, noPeriod, err := episodes(rep, rule, start, end)
	if err != nil {
		return scoreResult{}, err
	}
	days := end.Sub(start).Hours() / 24
	res := scoreResult{Experimental: true, Rule: rule, Start: start.Format(time.RFC3339),
		End: end.Format(time.RFC3339), Days: days, Labels: labelsPath, Episodes: len(eps),
		EpisodesWithoutPeriod: noPeriod, Incidents: len(lab.Incidents), Bots: bots,
		PerIncident: []incidentScore{}, FalsePositivesByChannel: []channelCount{}}
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
			if e.channel != inc.Channel || !overlaps(*e, inc, p) {
				continue
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
		m := dur(medianInt64(ttds))
		res.MedianTimeToDetect = &m
		res.MedianTTDPeriods = medianFloat(ttdPeriods(res.PerIncident))
		s := dur(medianInt64(stops))
		res.MedianStopLatency = &s
		res.MedianEpisodesPerIncident = medianFloat(perInc)
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
			a = &agg{channel: hexChannel(k)}
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
		e := episode{id: id, channel: a.channel, activation: a.act, end: a.endT,
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
