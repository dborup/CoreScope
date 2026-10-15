package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #474, phase 3: rank the stream keys of an exported background by row
// count, so an injection panel can be placed on keys real traffic actually
// uses instead of on keys of its own.
//
// Why this exists. The stream key is payload type + channel hash + route
// class + route kind + first hop. A bot on a channel byte and a first hop
// that no real transmission carries has that key to itself, so the detector
// sees a clean train and recall measured on it says nothing about finding a
// bot inside real traffic: a real spam bot on a busy channel shares its key
// with everyone else entering through the same repeater.
//
// Blind by construction. This command reads the export only and counts rows.
// It never runs the detector, reads no report, no labels and no metrics, and
// has no idea which keys would alarm: the ranking is row counts and nothing
// else, and ties break on the canonical key encoding. So a panel built from
// its output is chosen mechanically and before any detection result exists.
//
// Offline and file-only, like the rest of the tool: no database, no -db
// flag, no network, no clock.

// maxKeySizeBuckets bounds the distinct payload and wire sizes tracked per
// key, and maxKeyRouteVariants the distinct full routes. Both are counting
// bounds only: a key that exceeds one keeps its count and its times, and the
// overflow is reported per key.
const (
	maxKeySizeBuckets   = 512
	maxKeyRouteVariants = 64
	// maxKeySizePairs bounds the distinct (wire, payload) pairs tracked per
	// key. It is the distribution an injected bot draws from, so it is kept
	// wider than the per-dimension bucket bound; overflow is reported.
	maxKeySizePairs = 2048
)

// sizePair is one observed (wire_bytes, payload_bytes) pair and the number
// of background lines that carried it. PayloadBytes is absent for the lines
// whose payload length the export did not know.
//
// Why the pair and not two separate figures: the two sizes are not
// independent. A frame is header plus path plus payload, so wire - payload
// is the route's own overhead, while the payload itself comes in cipher
// blocks. Drawing the two separately produces frames real traffic never
// carries - and, when the drawn payload passes wire - 2, frames the event
// validator refuses outright.
type sizePair struct {
	WireBytes    int  `json:"wire_bytes"`
	PayloadBytes *int `json:"payload_bytes,omitempty"`
	Count        int  `json:"count"`
}

// keyStat is one ranked stream key. Every field is derived from the
// background lines on that key: hashes, hop prefixes, sizes, counts and
// times, which is what the export itself carries. No ids, observers, names
// or text.
type keyStat struct {
	Rank        int    `json:"rank"`
	Key         string `json:"key"`
	PayloadType int    `json:"payload_type"`
	Channel     string `json:"channel"`
	RouteClass  string `json:"route_class"`
	RouteKind   string `json:"route_kind"`
	FirstHop    string `json:"first_hop"`
	// FullRoute is the most common whole route seen on the key, ties broken
	// by the smallest encoding.
	FullRoute string `json:"full_route"`
	// WireBytesMedian and the payload figures are the realistic sizes an
	// injected bot on this key should use. PayloadBytesSpread is
	// (p90 - p10) / 2, so payload_bytes +/- spread covers the middle of the
	// observed spread; it is clamped below PayloadBytesMedian.
	WireBytesMedian    int `json:"wire_bytes_median"`
	PayloadBytesMedian int `json:"payload_bytes_median,omitempty"`
	PayloadBytesSpread int `json:"payload_bytes_spread"`
	// SizeMix is the key's whole observed (wire, payload) distribution, one
	// entry per distinct pair, ordered by wire size then payload size. This
	// is what an injected bot on this key draws its sizes from, so its lines
	// cannot be told from the key's real ones by size.
	SizeMix        []sizePair `json:"size_mix"`
	SizeMixDropped int        `json:"size_mix_dropped,omitempty"`
	Events         int        `json:"events"`
	// SizeBucketsDropped and RouteVariantsDropped count values that did not
	// fit the per-key bounds above; they affect the medians and the route
	// mode only, never Events.
	SizeBucketsDropped   int       `json:"size_buckets_dropped,omitempty"`
	RouteVariantsDropped int       `json:"route_variants_dropped,omitempty"`
	First                time.Time `json:"first"`
	Last                 time.Time `json:"last"`
}

// keysResult is the output file.
type keysResult struct {
	Experimental bool `json:"experimental"`
	// PayloadType, ExcludeChannels and the filter below are the mechanical
	// selection rule, restated in the output so a panel can be audited
	// against it.
	PayloadType      int       `json:"payload_type"`
	ExcludeChannels  []string  `json:"exclude_channels"`
	Filter           string    `json:"filter"`
	InputLines       int       `json:"input_lines"`
	Considered       int       `json:"considered"`
	SkippedType      int       `json:"skipped_payload_type"`
	SkippedNoChannel int       `json:"skipped_no_channel"`
	SkippedNotRelay  int       `json:"skipped_not_relay"`
	SkippedExcluded  int       `json:"skipped_excluded_channel"`
	DistinctKeys     int       `json:"distinct_keys"`
	Top              []keyStat `json:"top"`
}

// keyAgg accumulates one key's lines.
type keyAgg struct {
	k             anomaly.Key
	events        int
	first, last   time.Time
	payload, wire map[int]int
	// pairs counts the observed (wire, payload) pairs; a payload of -1 is a
	// line whose payload length the export did not know.
	pairs        map[[2]int]int
	routes       map[string]int
	sizeDropped  int
	pairDropped  int
	routeDropped int
}

func runKeys(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "exported ReplayRecord JSON Lines, or - for stdin (required)")
	payloadType := fs.Int("payload-type", -1, "payload type to rank (required)")
	top := fs.Int("top", 0, "how many keys to report (required)")
	exclude := fs.String("exclude-channels", "", "comma-separated channel hashes to leave out, lower-case hex")
	maxKeys := fs.Int("max-keys", 0, "most distinct keys allowed (required, a hard bound)")
	maxLine := fs.Int("max-line-bytes", 0, "longest accepted input line (required)")
	out := fs.String("out", "", "output file (must not exist); default stdout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(err error) int {
		fmt.Fprintf(stderr, "keys: %v\n", err)
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
	if *top <= 0 || *maxKeys <= 0 || *maxLine <= 0 {
		return usageErr(errors.New("-top, -max-keys and -max-line-bytes must be > 0"))
	}
	excl, err := parseExcludeList(*exclude)
	if err != nil {
		return usageErr(err)
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "keys: %v\n", err)
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
	res, err := rankKeys(in, *payloadType, excl, *top, *maxKeys, *maxLine)
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

// parseExcludeList parses the -exclude-channels list: lower-case hex hashes,
// no duplicates, nothing empty. The same canonical form the rule's
// exclude_channels takes.
func parseExcludeList(s string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.ToLower(v) != v {
			return nil, errors.New("-exclude-channels wants lower-case hex hashes")
		}
		b, err := hex.DecodeString(v)
		if err != nil || len(b) == 0 {
			return nil, fmt.Errorf("-exclude-channels: %q is not a channel hash", v)
		}
		if _, err := anomaly.NewChannelHash(b); err != nil {
			return nil, fmt.Errorf("-exclude-channels: %q: %w", v, err)
		}
		if seen[v] {
			return nil, fmt.Errorf("-exclude-channels: %q twice", v)
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

// rankKeys counts the background's stream keys and returns the top ones.
//
// The filter is mechanical and fixed: the line's payload type must be the
// requested one, it must carry a channel hash (the periodic rule's target is
// a channel bot), its route kind must be relay (only then is the first hop
// part of the stream key, so only then can a bot share the key), and its
// channel must not be excluded.
func rankKeys(r io.Reader, payloadType int, exclude []string, top, maxKeys, maxLine int) (keysResult, error) {
	res := keysResult{Experimental: true, PayloadType: payloadType, ExcludeChannels: exclude,
		Filter: "payload_type == -payload-type, channel hash present, route_kind == relay, channel not excluded",
		Top:    []keyStat{}}
	excluded := map[string]bool{}
	for _, v := range exclude {
		excluded[v] = true
	}
	aggs := map[anomaly.Key]*keyAgg{}
	sc := bufio.NewScanner(r)
	limit := maxLine + 2
	initial := 4096
	if limit < initial {
		initial = limit
	}
	sc.Buffer(make([]byte, 0, initial), limit)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) > maxLine {
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
		if rec.PayloadType != payloadType {
			res.SkippedType++
			continue
		}
		if rec.Channel == "" {
			res.SkippedNoChannel++
			continue
		}
		if rec.RouteKind != "relay" {
			res.SkippedNotRelay++
			continue
		}
		if excluded[rec.Channel] {
			res.SkippedExcluded++
			continue
		}
		ev, err := rec.Event()
		if err != nil {
			return res, fmt.Errorf("line %d: %w", res.InputLines, err)
		}
		k := anomaly.StreamKeyOf(ev)
		a := aggs[k]
		if a == nil {
			if len(aggs) >= maxKeys {
				return res, errors.New("more than -max-keys distinct keys; raise the bound")
			}
			a = &keyAgg{k: k, payload: map[int]int{}, wire: map[int]int{},
				pairs: map[[2]int]int{}, routes: map[string]int{}}
			aggs[k] = a
		}
		res.Considered++
		a.events++
		if a.first.IsZero() || rec.Time.Before(a.first) {
			a.first = rec.Time.UTC()
		}
		if rec.Time.After(a.last) {
			a.last = rec.Time.UTC()
		}
		bump(a.wire, rec.WireBytes, maxKeySizeBuckets, &a.sizeDropped)
		pb := -1
		if rec.PayloadBytes != nil {
			pb = *rec.PayloadBytes
			bump(a.payload, pb, maxKeySizeBuckets, &a.sizeDropped)
		}
		bumpPair(a.pairs, [2]int{rec.WireBytes, pb}, maxKeySizePairs, &a.pairDropped)
		bumpStr(a.routes, rec.FullRoute, maxKeyRouteVariants, &a.routeDropped)
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("input: %w", err)
	}
	res.DistinctKeys = len(aggs)
	ranked := make([]*keyAgg, 0, len(aggs))
	for _, a := range aggs {
		ranked = append(ranked, a)
	}
	// Row count, then the canonical key encoding: a total order that does
	// not depend on map iteration.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].events != ranked[j].events {
			return ranked[i].events > ranked[j].events
		}
		return ranked[i].k.String() < ranked[j].k.String()
	})
	if len(ranked) > top {
		ranked = ranked[:top]
	}
	for i, a := range ranked {
		res.Top = append(res.Top, a.stat(i+1))
	}
	return res, nil
}

func bump(m map[int]int, v, max int, dropped *int) {
	if _, ok := m[v]; !ok && len(m) >= max {
		*dropped++
		return
	}
	m[v]++
}

func bumpPair(m map[[2]int]int, v [2]int, max int, dropped *int) {
	if _, ok := m[v]; !ok && len(m) >= max {
		*dropped++
		return
	}
	m[v]++
}

func bumpStr(m map[string]int, v string, max int, dropped *int) {
	if _, ok := m[v]; !ok && len(m) >= max {
		*dropped++
		return
	}
	m[v]++
}

func (a *keyAgg) stat(rank int) keyStat {
	s := keyStat{Rank: rank, Key: a.k.String(), PayloadType: int(a.k.PayloadType),
		Channel: hexChannel(a.k), RouteClass: a.k.RouteClass.String(), RouteKind: a.k.RouteKind.String(),
		FirstHop: encodeHop(a.k.FirstHop), Events: a.events, First: a.first, Last: a.last,
		SizeBucketsDropped: a.sizeDropped, RouteVariantsDropped: a.routeDropped,
		SizeMix: sizeMixOf(a.pairs), SizeMixDropped: a.pairDropped}
	s.FullRoute = modeString(a.routes)
	s.WireBytesMedian = int(histPercentile(a.wire, 0.5))
	if len(a.payload) > 0 {
		med := int(histPercentile(a.payload, 0.5))
		p10 := int(histPercentile(a.payload, 0.1))
		p90 := int(histPercentile(a.payload, 0.9))
		s.PayloadBytesMedian = med
		spread := (p90 - p10) / 2
		if spread >= med {
			spread = med - 1
		}
		if spread < 0 {
			spread = 0
		}
		s.PayloadBytesSpread = spread
	}
	return s
}

// sizeMixOf renders a pair histogram in a fixed order: wire size, then
// payload size, with the unknown payload (-1) first for a given wire size.
// The order is a pure function of the histogram, so it does not depend on
// map iteration.
func sizeMixOf(m map[[2]int]int) []sizePair {
	out := make([]sizePair, 0, len(m))
	for v, n := range m {
		p := sizePair{WireBytes: v[0], Count: n}
		if v[1] >= 0 {
			pb := v[1]
			p.PayloadBytes = &pb
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WireBytes != out[j].WireBytes {
			return out[i].WireBytes < out[j].WireBytes
		}
		return payloadOrUnknown(out[i]) < payloadOrUnknown(out[j])
	})
	return out
}

func payloadOrUnknown(p sizePair) int {
	if p.PayloadBytes == nil {
		return -1
	}
	return *p.PayloadBytes
}

// encodeHop renders a first hop in the "width:hex" form a replay record and
// an injection spec use.
func encodeHop(h anomaly.Hop) string {
	b := h.Bytes()
	if len(b) == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%x", len(b), b)
}

// modeString returns the most common value, ties broken by the smallest.
func modeString(m map[string]int) string {
	best, bestN := "", -1
	for v, n := range m {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

// histPercentile is the nearest-rank percentile of a value histogram: the
// value at index ceil(q*n)-1 of the sorted values, clamped into range. It is
// a pure function of the histogram, so it does not depend on map order.
func histPercentile(m map[int]int, q float64) int64 {
	n := 0
	vals := make([]int, 0, len(m))
	for v, c := range m {
		n += c
		vals = append(vals, v)
	}
	if n == 0 {
		return 0
	}
	sort.Ints(vals)
	idx := int(math.Ceil(q*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx > n-1 {
		idx = n - 1
	}
	seen := 0
	for _, v := range vals {
		seen += m[v]
		if seen > idx {
			return int64(v)
		}
	}
	return int64(vals[len(vals)-1])
}
