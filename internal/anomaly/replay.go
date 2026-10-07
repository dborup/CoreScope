package anomaly

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Offline replay (EXPERIMENTAL). Replay reads normalized events from a
// neutral JSON Lines format, runs one Detector over an explicit [Start, End)
// data-time range and returns a deterministic report. It never opens a
// database, never touches the network and never reads the wall clock. There
// is deliberately no command-line tool; see TestOfflineReplay for running it
// from `go test` with local files.
//
// Input line format (one final event per line; unknown fields and trailing
// data after the JSON object are rejected):
//
//	{"id":"tx-1","time":"2030-01-07T12:00:00Z","final_at":"2030-01-07T12:00:05Z",
//	 "payload_type":5,"channel":"ab","route_class":"flood","route_kind":"relay",
//	 "first_hop":"1:5c","full_route":"1:5c11","wire_bytes":60,"payload_bytes":35,
//	 "traffic":"unclassified","decryption":"unknown","truncated":false}
//
// "channel" is plain hex ("" = no channel); hops and routes are
// "<width>:<hex>" ("" or "-" = none); "payload_bytes" may be omitted
// (unknown). Lines must be in data-time order within the config's
// ReorderDelay; older lines are counted as late (see Limits.ReorderDelay for
// lines with equal times).
type ReplayRecord struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	FinalAt      time.Time `json:"final_at,omitempty"`
	PayloadType  int       `json:"payload_type"`
	Channel      string    `json:"channel"`
	RouteClass   string    `json:"route_class"`
	RouteKind    string    `json:"route_kind"`
	FirstHop     string    `json:"first_hop"`
	FullRoute    string    `json:"full_route"`
	WireBytes    int       `json:"wire_bytes"`
	PayloadBytes *int      `json:"payload_bytes,omitempty"`
	Traffic      string    `json:"traffic"`
	Decryption   string    `json:"decryption"`
	Truncated    bool      `json:"truncated,omitempty"`
}

// maxReplayLineBytes caps ReplayOptions.MaxLineBytes.
const maxReplayLineBytes = 1 << 30

// onlyValue reports whether the decoder consumed all of b but whitespace.
func onlyValue(dec *json.Decoder, b []byte) bool {
	return len(bytes.TrimSpace(b[dec.InputOffset():])) == 0
}

// ReplayOptions bound the replay explicitly.
type ReplayOptions struct {
	// Start and End are required and must lie in the valid event time range
	// (End plus the config's ReorderDelay included): events outside
	// [Start, End) are skipped and counted.
	Start, End time.Time
	// MaxLineBytes bounds one input line, excluding its line terminator
	// (required, > 0).
	MaxLineBytes int
}

// ReplayInputStats counts input lines by outcome.
type ReplayInputStats struct {
	Lines, Accepted, Duplicates, Late, Invalid, OutOfRange int
}

// ReplayReport is the deterministic output of Replay.
type ReplayReport struct {
	Experimental bool
	Start, End   time.Time
	Input        ReplayInputStats
	Candidates   []Candidate
	Stats        Stats
	// CoverageReduced is true if any capacity limit cut evidence.
	CoverageReduced bool
}

func parseEnum(s string, names []string, what string) (int, error) {
	for i, n := range names {
		if n == s {
			return i, nil
		}
	}
	return 0, fmt.Errorf("anomaly: unknown %s", what)
}

func parseWidthHex(s string) (int, []byte, error) {
	if s == "" || s == "-" {
		return 0, nil, nil
	}
	i := strings.IndexByte(s, ':')
	if i < 1 {
		return 0, nil, errors.New("anomaly: expected <width>:<hex>")
	}
	w, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, nil, errors.New("anomaly: bad width")
	}
	b, err := hex.DecodeString(s[i+1:])
	if err != nil {
		return 0, nil, errors.New("anomaly: bad hex")
	}
	return w, b, nil
}

var trafficNames = []string{"unclassified", "expected", "monitored"}
var decryptNames = []string{"unknown", "decrypted", "not_decrypted"}

// Event converts a record. Errors never echo the record's values.
func (r ReplayRecord) Event() (Event, error) {
	e := Event{ID: TxID(r.ID), Time: r.Time.UTC(), WireBytes: r.WireBytes, Truncated: r.Truncated}
	if !r.FinalAt.IsZero() {
		e.FinalAt = r.FinalAt.UTC()
	}
	if r.PayloadType < 0 || r.PayloadType > int(MaxPayloadType) {
		return Event{}, errors.New("anomaly: payload_type out of range")
	}
	e.PayloadType = PayloadType(r.PayloadType)
	if r.Channel != "" {
		b, err := hex.DecodeString(r.Channel)
		if err != nil {
			return Event{}, errors.New("anomaly: bad channel hex")
		}
		if e.Channel, err = NewChannelHash(b); err != nil {
			return Event{}, err
		}
	}
	rc, err := parseEnum(r.RouteClass, routeClassNames[:], "route_class")
	if err != nil {
		return Event{}, err
	}
	rk, err := parseEnum(r.RouteKind, routeKindNames[:], "route_kind")
	if err != nil {
		return Event{}, err
	}
	e.RouteClass, e.RouteKind = RouteClass(rc), RouteKind(rk)
	// Width 0 is valid only with no bytes (an empty hop or route); NewRoute
	// enforces the same, so nonempty bytes never vanish silently.
	if w, b, err := parseWidthHex(r.FirstHop); err != nil {
		return Event{}, err
	} else if w != 0 || len(b) != 0 {
		if w != len(b) {
			return Event{}, errors.New("anomaly: first_hop width mismatch")
		}
		if e.FirstHop, err = NewHop(b); err != nil {
			return Event{}, err
		}
	}
	if w, b, err := parseWidthHex(r.FullRoute); err != nil {
		return Event{}, err
	} else if e.FullRoute, err = NewRoute(w, b); err != nil {
		return Event{}, err
	}
	if r.PayloadBytes != nil {
		e.PayloadBytes, e.PayloadKnown = *r.PayloadBytes, true
	}
	tc, err := parseEnum(orDefault(r.Traffic, "unclassified"), trafficNames, "traffic")
	if err != nil {
		return Event{}, err
	}
	dc, err := parseEnum(orDefault(r.Decryption, "unknown"), decryptNames, "decryption")
	if err != nil {
		return Event{}, err
	}
	e.Traffic, e.Decryption = TrafficClass(tc), DecryptStatus(dc)
	return e, e.Validate()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Replay runs cfg over the JSON Lines in r. Invalid lines are counted, not
// fatal; a line longer than MaxLineBytes, a read error or an invalid config
// is fatal.
func Replay(r io.Reader, cfg Config, opt ReplayOptions) (ReplayReport, error) {
	var rep ReplayReport
	if opt.Start.IsZero() || opt.End.IsZero() || !opt.End.After(opt.Start) {
		return rep, errors.New("anomaly: replay needs an explicit Start < End")
	}
	if opt.MaxLineBytes <= 0 || opt.MaxLineBytes > maxReplayLineBytes {
		return rep, errors.New("anomaly: replay needs MaxLineBytes in 1..1 GiB")
	}
	if cfg.HistoryStart.IsZero() {
		cfg.HistoryStart = opt.Start
	}
	d, err := New(cfg)
	if err != nil {
		return rep, err
	}
	// the final Advance must be inside the valid range, or deadlines after
	// the last event would silently not run
	if !validTime(opt.Start) || !validTime(opt.End.Add(cfg.Limits.ReorderDelay)) {
		return rep, errors.New("anomaly: replay Start/End outside the valid time range")
	}
	rep.Experimental = true
	rep.Start, rep.End = opt.Start.UTC(), opt.End.UTC()
	sc := bufio.NewScanner(r)
	// The scanner's limit is max(cap(buf), max) and includes the terminator
	// ("\r\n"): keep the buffer within it and check the line length below.
	limit := opt.MaxLineBytes + 2
	initial := 4096
	if limit < initial {
		initial = limit
	}
	sc.Buffer(make([]byte, 0, initial), limit)
	collect := func(res Result) {
		rep.Candidates = append(rep.Candidates, res.Candidates...)
		rep.CoverageReduced = rep.CoverageReduced || res.CoverageReduced
	}
	// drain after every call, so the per-call cap never overflows the
	// detector's candidate buffer because of the harness
	drain := func(res Result) {
		for collect(res); res.More; {
			res = d.Drain()
			collect(res)
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) > opt.MaxLineBytes {
			return rep, errors.New("anomaly: replay input: line longer than MaxLineBytes")
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		rep.Input.Lines++
		var rec ReplayRecord
		dec := json.NewDecoder(strings.NewReader(string(line)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil || !onlyValue(dec, line) {
			rep.Input.Invalid++
			continue
		}
		e, err := rec.Event()
		if err != nil {
			rep.Input.Invalid++
			continue
		}
		if e.Time.Before(opt.Start) || !e.Time.Before(opt.End) {
			rep.Input.OutOfRange++
			continue
		}
		res := d.Observe(e)
		switch res.Status {
		case StatusAccepted:
			rep.Input.Accepted++
		case StatusDuplicate:
			rep.Input.Duplicates++
		case StatusLate:
			rep.Input.Late++
		default:
			rep.Input.Invalid++
		}
		drain(res)
	}
	if err := sc.Err(); err != nil {
		return rep, fmt.Errorf("anomaly: replay input: %w", err)
	}
	drain(d.Flush())
	// input is complete: run deadlines up to (not including) End
	drain(d.Advance(opt.End.Add(cfg.Limits.ReorderDelay - time.Nanosecond)))
	rep.Stats = d.Snapshot()
	rep.CoverageReduced = rep.CoverageReduced || rep.Stats.CoverageReduced()
	return rep, nil
}

// ---- JSON config for the replay harness ----

// Duration is a JSON-friendly duration ("90s", "5m").
type Duration time.Duration

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("anomaly: durations are strings such as \"5m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ReplayConfig is the JSON form of Config used by the offline harness.
// Every field is required exactly as in Config; there are no defaults.
type ReplayConfig struct {
	Limits struct {
		MaxKeysPerScope      int      `json:"max_keys_per_scope"`
		StateTTL             Duration `json:"state_ttl"`
		DedupHorizon         Duration `json:"dedup_horizon"`
		MaxDedupIDs          int      `json:"max_dedup_ids"`
		ReorderDelay         Duration `json:"reorder_delay"`
		MaxReorderBuffer     int      `json:"max_reorder_buffer"`
		MaxWindowEntries     int      `json:"max_window_entries"`
		MaxCandidatesPerCall int      `json:"max_candidates_per_call"`
		MaxPendingCandidates int      `json:"max_pending_candidates"`
	} `json:"limits"`
	Expected string `json:"expected"` // "include" or "suppress"
	Rate     []struct {
		Name         string        `json:"name"`
		Scope        string        `json:"scope"`
		Window       Duration      `json:"window"`
		Threshold    int           `json:"threshold"`
		PayloadTypes []PayloadType `json:"payload_types"`
		State        stateJSON     `json:"state"`
	} `json:"rate"`
	NewStream []struct {
		Name         string        `json:"name"`
		Scope        string        `json:"scope"`
		Window       Duration      `json:"window"`
		MinCount     int           `json:"min_count"`
		CensorWindow Duration      `json:"censor_window"`
		QuietPeriod  Duration      `json:"quiet_period"`
		PayloadTypes []PayloadType `json:"payload_types"`
		State        stateJSON     `json:"state"`
	} `json:"new_stream"`
	Periodic []struct {
		Name              string        `json:"name"`
		Scope             string        `json:"scope"`
		PayloadTypes      []PayloadType `json:"payload_types"`
		MinPeriod         Duration      `json:"min_period"`
		MaxPeriod         Duration      `json:"max_period"`
		BurstGap          Duration      `json:"burst_gap"`
		JitterAbs         Duration      `json:"jitter_abs"`
		JitterRel         float64       `json:"jitter_rel"`
		MaxMissing        int           `json:"max_missing"`
		MinPulses         int           `json:"min_pulses"`
		HistoryLen        int           `json:"history_len"`
		MinCoverage       float64       `json:"min_coverage"`
		MaxJitterFraction float64       `json:"max_jitter_fraction"`
		MaxChance         float64       `json:"max_chance"`
		State             stateJSON     `json:"state"`
	} `json:"periodic"`
}

type stateJSON struct {
	ConfirmAfter  Duration `json:"confirm_after"`
	EndAfterQuiet Duration `json:"end_after_quiet"`
	Cooldown      Duration `json:"cooldown"`
}

func (s stateJSON) params() StateParams {
	return StateParams{ConfirmAfter: time.Duration(s.ConfirmAfter), EndAfterQuiet: time.Duration(s.EndAfterQuiet), Cooldown: time.Duration(s.Cooldown)}
}

// ParseReplayConfig decodes a JSON config (unknown fields and trailing data
// rejected) and validates it.
func ParseReplayConfig(b []byte) (Config, error) {
	var rc ReplayConfig
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rc); err != nil {
		return Config{}, fmt.Errorf("anomaly: replay config: %w", err)
	}
	if !onlyValue(dec, b) {
		return Config{}, errors.New("anomaly: replay config: trailing data")
	}
	scope := func(s string) (Scope, error) {
		i, err := parseEnum(s, scopeNames[:], "scope")
		if err != nil || i == 0 {
			return 0, errors.New("anomaly: unknown scope")
		}
		return Scope(i), nil
	}
	l := rc.Limits
	cfg := Config{Limits: Limits{MaxKeysPerScope: l.MaxKeysPerScope, StateTTL: time.Duration(l.StateTTL),
		DedupHorizon: time.Duration(l.DedupHorizon), MaxDedupIDs: l.MaxDedupIDs, ReorderDelay: time.Duration(l.ReorderDelay),
		MaxReorderBuffer: l.MaxReorderBuffer, MaxWindowEntries: l.MaxWindowEntries,
		MaxCandidatesPerCall: l.MaxCandidatesPerCall, MaxPendingCandidates: l.MaxPendingCandidates}}
	switch rc.Expected {
	case "include":
		cfg.Expected.Mode = ExpectedInclude
	case "suppress":
		cfg.Expected.Mode = ExpectedSuppress
	}
	for _, r := range rc.Rate {
		s, err := scope(r.Scope)
		if err != nil {
			return Config{}, err
		}
		cfg.Rate = append(cfg.Rate, RateRule{Name: r.Name, Scope: s, Window: time.Duration(r.Window), Threshold: r.Threshold,
			PayloadTypes: r.PayloadTypes, State: r.State.params()})
	}
	for _, r := range rc.NewStream {
		s, err := scope(r.Scope)
		if err != nil {
			return Config{}, err
		}
		cfg.NewStream = append(cfg.NewStream, NewStreamRule{Name: r.Name, Scope: s, Window: time.Duration(r.Window),
			MinCount: r.MinCount, CensorWindow: time.Duration(r.CensorWindow), QuietPeriod: time.Duration(r.QuietPeriod),
			PayloadTypes: r.PayloadTypes, State: r.State.params()})
	}
	for _, r := range rc.Periodic {
		s, err := scope(r.Scope)
		if err != nil {
			return Config{}, err
		}
		cfg.Periodic = append(cfg.Periodic, PeriodicRule{Name: r.Name, Scope: s, PayloadTypes: r.PayloadTypes,
			MinPeriod: time.Duration(r.MinPeriod), MaxPeriod: time.Duration(r.MaxPeriod), BurstGap: time.Duration(r.BurstGap),
			JitterAbs: time.Duration(r.JitterAbs), JitterRel: r.JitterRel, MaxMissing: r.MaxMissing, MinPulses: r.MinPulses,
			HistoryLen: r.HistoryLen, MinCoverage: r.MinCoverage, MaxJitterFraction: r.MaxJitterFraction,
			MaxChance: r.MaxChance, State: r.State.params()})
	}
	return cfg, cfg.Validate()
}
