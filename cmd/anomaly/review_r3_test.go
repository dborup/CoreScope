package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Round-3 review of PR #478 (#474). The round-2 panel could not be injected
// at all on a realistic key: make_spec.py held wire_bytes at the key's
// median while letting the payload size spread, and the event validator
// requires payload <= wire - 2, so the first oversized draw aborted the run
// (G1). The fix draws each event's (wire, payload) pair from the key's own
// observed distribution (G2), which is valid by construction and leaves no
// size an injected line can be recognised by. The rest of this file pins the
// boundaries round 2's mutants survived (G4), the timing statistic that now
// penalises a missed bot (G3) and the per-incident baseline flag (G5).
//
// Every channel byte, hop, size and time here is synthetic.

// r3Rec is one background line with explicit sizes.
func r3Rec(id string, at time.Time, channel, hop, route string, wire int, payload *int) anomaly.ReplayRecord {
	return anomaly.ReplayRecord{ID: id, Time: at, FinalAt: at.Add(30 * time.Second), PayloadType: 5,
		Channel: channel, RouteClass: "flood", RouteKind: "relay", FirstHop: hop, FullRoute: route,
		WireBytes: wire, PayloadBytes: payload, Traffic: "unclassified", Decryption: "unknown"}
}

func r3Line(t *testing.T, rec anomaly.ReplayRecord) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// r3Sized writes one line per payload size on one series, with the wire size
// a fixed number of bytes above the payload (header plus path, as a real
// frame has). Times step by a minute, so the lines are in order.
func r3Sized(t *testing.T, channel, hop, route string, payloads []int, headroom int) string {
	t.Helper()
	var b strings.Builder
	for i, pb := range payloads {
		pb := pb
		b.WriteString(r3Line(t, r3Rec(fmt.Sprintf("tx%d", i+1), injT.Add(time.Duration(i)*time.Minute),
			channel, hop, route, pb+headroom, &pb)))
	}
	return b.String()
}

// G2/G1: the ranking carries the key's whole observed (wire, payload)
// distribution, with a count per distinct pair. That distribution is what an
// injected bot draws its sizes from, so it has to be in the output, exact,
// and in a fixed order.
func TestKeysReportsTheObservedSizeDistribution(t *testing.T) {
	var b strings.Builder
	// 35 bytes four times, 51 twice, 67 once, each with a 3-byte frame
	// overhead. Two lines share the 38-byte frame with a shorter payload and
	// with a payload the export did not know, which is what fixes the order
	// inside one wire size.
	add := func(i int, wire int, payload *int) {
		b.WriteString(r3Line(t, r3Rec(fmt.Sprintf("tx%d", i+1), injT.Add(time.Duration(i)*time.Minute),
			"0b", "1:5c", "1:5c0a", wire, payload)))
	}
	for i, pb := range []int{35, 35, 35, 35, 51, 51, 67} {
		pb := pb
		add(i, pb+3, &pb)
	}
	add(7, 40, nil)
	add(8, 38, nil)
	add(9, 38, intp(30))
	res := keysOn(t, b.String())
	if len(res.Top) != 1 {
		t.Fatalf("one key expected, got %d", len(res.Top))
	}
	got := res.Top[0].SizeMix
	// Ordered by wire size, then by payload size, with the unknown payload
	// first inside one wire size.
	want := []sizePair{{WireBytes: 38, Count: 1}, {WireBytes: 38, PayloadBytes: intp(30), Count: 1},
		{WireBytes: 38, PayloadBytes: intp(35), Count: 4}, {WireBytes: 40, Count: 1},
		{WireBytes: 54, PayloadBytes: intp(51), Count: 2}, {WireBytes: 70, PayloadBytes: intp(67), Count: 1}}
	if len(got) != len(want) {
		t.Fatalf("size mix %+v", got)
	}
	for i := range want {
		if got[i].WireBytes != want[i].WireBytes || got[i].Count != want[i].Count ||
			(got[i].PayloadBytes == nil) != (want[i].PayloadBytes == nil) ||
			(want[i].PayloadBytes != nil && *got[i].PayloadBytes != *want[i].PayloadBytes) {
			t.Fatalf("size mix entry %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if res.Top[0].Events != 10 {
		t.Fatalf("events %d", res.Top[0].Events)
	}
}

// r3SizeMix renders a size mix as the JSON an injection spec carries.
func r3SizeMix(pairs ...sizePair) string {
	b, err := json.Marshal(pairs)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// r3MixSpec is one bot described by a size mix instead of by scalar sizes.
func r3MixSpec(seed uint64, mix string, stopAfter time.Duration) string {
	return fmt.Sprintf(`{
 "seed": %d,
 "final_after": "30s",
 "labelling": {"pulse_merge_gap": "60s", "min_period": "1m", "max_period": "1h",
  "min_pulses": 8, "max_harmonic": 3, "tolerance_abs": "30s", "tolerance_rel": 0.1},
 "bots": [{
  "name": "bot-a", "channel": "7f", "payload_type": 5, "route_class": "flood", "route_kind": "relay",
  "first_hop": "1:fd", "full_route": "1:fd0a", "size_mix": %s,
  "start": %q, "stop": %q, "period": "5m", "jitter_sd": "3s", "jitter_max": "9s",
  "missed_fraction": 0, "multipart_fraction": 0, "multipart_max_extra": 2,
  "multipart_max_delay": "55s", "holes": []
 }]
}`, seed, mix, injAt(0), injAt(stopAfter))
}

// G1/G2: a bot that draws from the key's observed pairs emits only sizes the
// key really carried, in roughly the observed proportions, and every line is
// a valid event - including on a block-quantised distribution whose spread
// is far wider than the frame's own margin, which is what aborted round 2.
func TestInjectSizeMixDrawsOnlyObservedPairs(t *testing.T) {
	mix := r3SizeMix(sizePair{WireBytes: 38, PayloadBytes: intp(35), Count: 70},
		sizePair{WireBytes: 54, PayloadBytes: intp(51), Count: 20},
		sizePair{WireBytes: 70, PayloadBytes: intp(67), Count: 10},
		sizePair{WireBytes: 40, Count: 10})
	r := injectInto(t, r3MixSpec(5, mix, 24*time.Hour), backgroundJSONL(40))
	seen := map[string]int{}
	lines := injectedLines(t, r)
	if len(lines) < 200 {
		t.Fatalf("only %d injected lines", len(lines))
	}
	for _, rec := range lines {
		pb := "none"
		if rec.PayloadBytes != nil {
			pb = fmt.Sprint(*rec.PayloadBytes)
		}
		key := fmt.Sprintf("%d/%s", rec.WireBytes, pb)
		switch key {
		case "38/35", "54/51", "70/67", "40/none":
		default:
			t.Fatalf("%s carries the unobserved size pair %s", rec.ID, key)
		}
		seen[key]++
	}
	if len(seen) != 4 {
		t.Fatalf("only %d of the four observed pairs were drawn: %v", len(seen), seen)
	}
	// The commonest pair is 70 % of the weight and must dominate; the three
	// others are 10-20 % each and must stay well below it.
	if seen["38/35"] <= 2*seen["54/51"] || seen["54/51"] <= seen["70/67"] {
		t.Fatalf("draw does not follow the observed weights: %v", seen)
	}
}

// G1: a size mix is the whole size description. Mixing it with the scalar
// size fields would leave two sources of truth, and an entry the frame
// cannot hold must be refused by the spec rather than abort the run halfway
// through, which is what round 2 did.
func TestInjectSizeMixIsValidatedBeforeAnythingRuns(t *testing.T) {
	for _, tc := range []struct{ name, mix, extra string }{
		{"a pair the frame cannot hold",
			r3SizeMix(sizePair{WireBytes: 38, PayloadBytes: intp(37), Count: 1}), ""},
		{"a zero count", r3SizeMix(sizePair{WireBytes: 38, PayloadBytes: intp(35), Count: 0}), ""},
		{"a negative count", r3SizeMix(sizePair{WireBytes: 38, PayloadBytes: intp(35), Count: -1}), ""},
		{"no wire size", r3SizeMix(sizePair{PayloadBytes: intp(35), Count: 1}), ""},
		{"an empty mix", "[]", ""},
		{"beside wire_bytes", r3SizeMix(sizePair{WireBytes: 38, PayloadBytes: intp(35), Count: 1}),
			`"wire_bytes": 60,`},
		{"beside payload_bytes", r3SizeMix(sizePair{WireBytes: 38, PayloadBytes: intp(35), Count: 1}),
			`"payload_bytes": 35, "payload_bytes_spread": 0,`},
	} {
		spec := strings.Replace(r3MixSpec(5, tc.mix, time.Hour), `"size_mix":`, tc.extra+` "size_mix":`, 1)
		if _, err := parseInjectSpec([]byte(spec)); err == nil {
			t.Fatalf("%s: want an error", tc.name)
		}
	}
	ok := r3MixSpec(5, r3SizeMix(sizePair{WireBytes: 38, PayloadBytes: intp(36), Count: 1}), time.Hour)
	if _, err := parseInjectSpec([]byte(ok)); err != nil {
		t.Fatalf("a payload exactly wire-2 long is legal: %v", err)
	}
}

// G1: the scalar path keeps its envelope inside the frame too. payload_bytes
// +/- payload_bytes_spread is the whole range a draw can reach, so the top of
// that range - not just payload_bytes - has to fit wire_bytes - 2. Round 2
// checked the spread against the payload only, so a legal-looking spec
// aborted on its first wide draw.
func TestInjectSpreadEnvelopeMustFitTheFrame(t *testing.T) {
	spec := func(payload, spread, wire int) string {
		return fmt.Sprintf(`{"seed":7,"final_after":"30s",
 "labelling":{"pulse_merge_gap":"60s","min_period":"1m","max_period":"1h","min_pulses":8,
  "max_harmonic":3,"tolerance_abs":"30s","tolerance_rel":0.1},
 "bots":[{"name":"bot-a","channel":"7f","payload_type":5,"route_class":"flood","route_kind":"relay",
  "first_hop":"1:fd","full_route":"1:fd0a","wire_bytes":%d,"payload_bytes":%d,"payload_bytes_spread":%d,
  "start":%q,"stop":%q,"period":"5m","jitter_sd":"0s",
  "jitter_max":"10s","missed_fraction":0,"multipart_fraction":0,"multipart_max_extra":2,
  "multipart_max_delay":"55s","holes":[]}]}`, wire, payload, spread, injAt(0), injAt(4*time.Hour))
	}
	// The real shape of the abort: a key whose median wire is 47 and whose
	// payload spread is 24, as the fixture's rank-1 key is. The diagnostic
	// names the two fields, because round 2's failure mode was an opaque
	// validator error thousands of lines into a run.
	_, err := parseInjectSpec([]byte(spec(35, 24, 47)))
	if err == nil {
		t.Fatal("a spread that can overflow the frame must be refused by the spec")
	}
	if !strings.Contains(err.Error(), "payload_bytes + payload_bytes_spread") {
		t.Fatalf("the error must name the envelope: %v", err)
	}
	// One byte inside the margin is legal, one byte outside is not.
	if _, err := parseInjectSpec([]byte(spec(35, 23, 60))); err != nil {
		t.Fatalf("payload_bytes + spread == wire_bytes - 2 is legal: %v", err)
	}
	if _, err := parseInjectSpec([]byte(spec(35, 24, 60))); err == nil {
		t.Fatal("payload_bytes + spread one past wire_bytes - 2 must be refused")
	}
	// The envelope must clear every bound the event validator has, not only
	// the frame: a payload of 180 +/- 10 fits a 255-byte frame but its top
	// passes MaxPayloadBytes (184).
	if _, err := parseInjectSpec([]byte(spec(180, 10, 255))); err == nil {
		t.Fatal("a spread whose top passes MaxPayloadBytes must be refused")
	}
	if _, err := parseInjectSpec([]byte(spec(180, 4, 255))); err != nil {
		t.Fatalf("a spread whose top is exactly MaxPayloadBytes is legal: %v", err)
	}
	// And no run of a legal spec may ever produce an invalid line.
	r := injectInto(t, spec(35, 23, 60), backgroundJSONL(20))
	for _, rec := range injectedLines(t, r) {
		if _, err := rec.Event(); err != nil {
			t.Fatalf("%s: %v", rec.ID, err)
		}
	}
}

// r3BlockBackground is a background shaped like real GRP_TXT traffic: the
// payload comes in 16-byte cipher blocks and the frame adds only a header
// and a one-hop path, so wire - payload is 3 on every line and the size
// spread is many times the frame's own margin. nKeys series, the i-th
// carrying one line more than the next, so the ranking is unambiguous.
func r3BlockBackground(t *testing.T, nKeys, perKey int) string {
	t.Helper()
	var b strings.Builder
	at := injT
	n := 0
	for i := 0; i < nKeys; i++ {
		hop := fmt.Sprintf("1:%02x", 0x20+i)
		for j := 0; j < perKey+(nKeys-i); j++ {
			pb := []int{35, 51, 67, 35, 83, 35, 51}[j%7]
			at = at.Add(41 * time.Second)
			n++
			b.WriteString(r3Line(t, r3Rec(fmt.Sprintf("tx%d", n), at, "0b", hop, hop+"0a", pb+3, &pb)))
		}
	}
	return b.String()
}

// r3MakeSpec runs the pre-registered make_spec.py over a keys ranking and
// returns the spec it printed. The script is the pre-registration's own, so
// the test exercises what the round will actually run.
func r3MakeSpec(t *testing.T, keysJSON []byte) []byte {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed; make_spec.py cannot be exercised here")
	}
	dir := t.TempDir()
	kp := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(kp, keysJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "make_spec.py", injT.UTC().Format("2006-01-02T15:04:05Z"), kp)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("make_spec.py: %v: %s", err, errb.String())
	}
	return out.Bytes()
}

// G1: the pre-registered pipeline has to run on realistic sizes. keys ->
// make_spec.py -> inject, on a background whose payloads come in 16-byte
// blocks, is exactly the step that aborted in round 2.
func TestPipelineKeysSpecInjectOnBlockQuantisedSizes(t *testing.T) {
	bg := r3BlockBackground(t, 10, 40)
	dir := t.TempDir()
	bgPath := filepath.Join(dir, "bg.jsonl")
	if err := os.WriteFile(bgPath, []byte(bg), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"keys", "-input", bgPath, "-payload-type", "5", "-top", "16",
		"-max-keys", "5000", "-max-line-bytes", "4096"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("keys exit %d: %s", code, stderr.String())
	}
	spec := r3MakeSpec(t, stdout.Bytes())
	specPath, outPath, labPath := filepath.Join(dir, "spec.json"), filepath.Join(dir, "inj.jsonl"), filepath.Join(dir, "labels.json")
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	var istdout, istderr bytes.Buffer
	code := run([]string{"inject", "-spec", specPath, "-input", bgPath, "-out", outPath,
		"-labels", labPath, "-max-injected", "2000000", "-max-line-bytes", "65536"}, nil, &istdout, &istderr)
	if code != 0 {
		t.Fatalf("inject exit %d: %s", code, istderr.String())
	}
	// Every injected line must be a valid event, and no injected size pair
	// may be one the background never carried.
	bgPairs := map[string]bool{}
	for _, l := range strings.Split(strings.TrimRight(bg, "\n"), "\n") {
		var rec anomaly.ReplayRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}
		bgPairs[r3Pair(rec)] = true
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	injected := 0
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		var rec anomaly.ReplayRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(rec.ID, injPrefix) {
			continue
		}
		injected++
		if _, err := rec.Event(); err != nil {
			t.Fatalf("%s: %v", rec.ID, err)
		}
		if !bgPairs[r3Pair(rec)] {
			t.Fatalf("%s carries size pair %s, which the background never had", rec.ID, r3Pair(rec))
		}
	}
	if injected < 1000 {
		t.Fatalf("only %d injected lines", injected)
	}
}

func r3Pair(rec anomaly.ReplayRecord) string {
	if rec.PayloadBytes == nil {
		return fmt.Sprintf("%d/none", rec.WireBytes)
	}
	return fmt.Sprintf("%d/%d", rec.WireBytes, *rec.PayloadBytes)
}

// G1: the whole pre-registered chain, from a database. export -> keys ->
// make_spec.py -> inject on the package's own synthetic CoreScope fixture,
// whose GRP_TXT payloads come in real 16-byte cipher blocks over a one-hop
// path - the size structure that made round 2 abort.
func TestPipelineExportKeysSpecInjectOnTheFixture(t *testing.T) {
	f := newFixtureDB(t, false)
	// Ten route groups, each a different first hop on one channel byte, with
	// 12 to 21 transmissions apiece so the ranking is strict. Payload sizes
	// are 3 + 16k bytes, as a GRP_TXT payload is.
	n := 0
	for i := 0; i < 10; i++ {
		hop := byte(0x20 + i)
		for j := 0; j < 12+(10-i); j++ {
			n++
			blocks := []int{32, 48, 64, 32, 80, 32, 48}[j%7]
			pl := grpPayload(0x0a, blocks)
			at := fxAt(time.Duration(n) * 37 * time.Second)
			f.tx(fxHash(n), frameHex(1, 5, 1, []byte{hop}, pl), 5,
				relay(at, 1, 5, 1, []byte{hop}, pl))
		}
	}
	dir := t.TempDir()
	bgPath := filepath.Join(dir, "bg.jsonl")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"export", "-db", f.path, "-start", fxT.UTC().Format(time.RFC3339),
		"-end", fxAt(24 * time.Hour).UTC().Format(time.RFC3339), "-settle", "30s",
		"-max-rows", "100000", "-out", bgPath}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("export exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"keys", "-input", bgPath, "-payload-type", "5", "-top", "16",
		"-max-keys", "5000000", "-max-line-bytes", "4096"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("keys exit %d: %s", code, stderr.String())
	}
	var ranking keysResult
	if err := json.Unmarshal(stdout.Bytes(), &ranking); err != nil {
		t.Fatal(err)
	}
	if len(ranking.Top) < 9 {
		t.Fatalf("the fixture must offer at least the panel's nine keys, got %d", len(ranking.Top))
	}
	// The size structure that broke round 2 must really be there: the key's
	// payload spread has to exceed the frame's own margin.
	k := ranking.Top[0]
	if k.PayloadBytesSpread <= k.WireBytesMedian-2-k.PayloadBytesMedian {
		t.Fatalf("rank 1 is not block-quantised: wire %d, payload %d +/- %d",
			k.WireBytesMedian, k.PayloadBytesMedian, k.PayloadBytesSpread)
	}
	spec := r3MakeSpec(t, stdout.Bytes())
	specPath := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	var istdout, istderr bytes.Buffer
	code := run([]string{"inject", "-spec", specPath, "-input", bgPath,
		"-out", filepath.Join(dir, "inj.jsonl"), "-labels", filepath.Join(dir, "labels.json"),
		"-max-injected", "2000000", "-max-line-bytes", "65536"}, nil, &istdout, &istderr)
	if code != 0 {
		t.Fatalf("inject exit %d: %s", code, istderr.String())
	}
	var sum injectSummary
	if err := json.Unmarshal(istderr.Bytes(), &sum); err != nil {
		t.Fatalf("summary %q: %v", istderr.String(), err)
	}
	if sum.InjectedEvents < 1000 || len(sum.Bots) != 18 {
		t.Fatalf("summary: %d events, %d bots", sum.InjectedEvents, len(sum.Bots))
	}
}

// G4 (mutant N3): the tie-break is part of the mechanical assignment rule,
// so it has to be pinned. Two keys with the same row count rank by the
// smaller canonical encoding first; reversing that order silently moves
// every panel position.
func TestKeysTieBreakIsTheSmallerCanonicalKey(t *testing.T) {
	res := keysOn(t, r2Background([]r2Series{
		{channel: "0b", firstHop: "1:11", fullRoute: "1:11", weight: 3, payload: 30},
		{channel: "0b", firstHop: "1:22", fullRoute: "1:22", weight: 3, payload: 30},
		{channel: "0b", firstHop: "1:33", fullRoute: "1:33", weight: 3, payload: 30},
	}, 4))
	if len(res.Top) != 3 {
		t.Fatalf("three tied keys expected, got %d", len(res.Top))
	}
	for i := 1; i < len(res.Top); i++ {
		if res.Top[i-1].Events != res.Top[i].Events {
			t.Fatalf("the counts must tie: %+v", res.Top)
		}
		if !(res.Top[i-1].Key < res.Top[i].Key) {
			t.Fatalf("tie %d: %q must sort before %q", i, res.Top[i-1].Key, res.Top[i].Key)
		}
	}
	if res.Top[0].FirstHop != "1:11" || res.Top[2].FirstHop != "1:33" {
		t.Fatalf("tie order: %q .. %q", res.Top[0].FirstHop, res.Top[2].FirstHop)
	}
}

// G4 (mutant N11): -top reports exactly that many keys. The full ranking
// (-top large) is what the pre-registration's absence check for the isolated
// keys reads, so an off-by-one there silently drops a key from the check.
func TestKeysTopReportsExactlyTopKeys(t *testing.T) {
	bg := r3BlockBackground(t, 6, 10)
	for _, tc := range []struct{ top, want int }{{1, 1}, {3, 3}, {6, 6}, {9, 6}} {
		res := keysOn(t, bg, "-top", fmt.Sprint(tc.top))
		if len(res.Top) != tc.want {
			t.Fatalf("-top %d reported %d keys, want %d", tc.top, len(res.Top), tc.want)
		}
		if res.DistinctKeys != 6 {
			t.Fatalf("-top %d: distinct keys %d", tc.top, res.DistinctKeys)
		}
		for i, k := range res.Top {
			if k.Rank != i+1 {
				t.Fatalf("-top %d: entry %d has rank %d", tc.top, i, k.Rank)
			}
		}
	}
}

// G4 (mutant N6): the percentiles are nearest-rank over the value
// histogram - the value at index ceil(q*n)-1 - and the boundary of that
// scan is off by one count if the comparison is not strict. Ten distinct
// sizes, one line each, make every percentile a different value.
func TestKeysPercentilesAreNearestRank(t *testing.T) {
	payloads := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	res := keysOn(t, r3Sized(t, "0b", "1:5c", "1:5c0a", payloads, 5))
	if len(res.Top) != 1 {
		t.Fatalf("one key expected, got %d", len(res.Top))
	}
	k := res.Top[0]
	// n = 10: the median is index 4 (50), p10 index 0 (10) and p90 index 8
	// (90), so the spread is (90 - 10) / 2 = 40. A non-strict scan returns
	// the value one count earlier: median 40 and p90 80, spread 35.
	if k.PayloadBytesMedian != 50 {
		t.Fatalf("payload median %d, want 50", k.PayloadBytesMedian)
	}
	if k.PayloadBytesSpread != 40 {
		t.Fatalf("payload spread %d, want 40", k.PayloadBytesSpread)
	}
	if k.WireBytesMedian != 55 {
		t.Fatalf("wire median %d, want 55", k.WireBytesMedian)
	}
}

// G4 (mutant N4): the spread is clamped strictly below the median, because
// payload_bytes +/- spread must stay positive. A raw spread exactly equal to
// the median is the boundary, and only a clamp at equality catches it.
func TestKeysSpreadIsClampedStrictlyBelowTheMedian(t *testing.T) {
	// Ten sizes, one line each: p10 = 1, median = 16, p90 = 33, so the raw
	// spread is (33 - 1) / 2 = 16, exactly the median.
	res := keysOn(t, r3Sized(t, "0b", "1:5c", "1:5c0a", []int{1, 2, 3, 4, 16, 20, 24, 28, 33, 40}, 5))
	k := res.Top[0]
	if k.PayloadBytesMedian != 16 {
		t.Fatalf("payload median %d, want 16", k.PayloadBytesMedian)
	}
	if k.PayloadBytesSpread != 15 {
		t.Fatalf("spread %d, want the median minus one (15)", k.PayloadBytesSpread)
	}
}

// G4 (mutant N7): first and last are the key's earliest and latest line. The
// export is ordered, but a merged or concatenated background need not be, and
// a first that tracks the maximum reports a window the key never had.
func TestKeysFirstAndLastAreTheExtremeTimes(t *testing.T) {
	var b strings.Builder
	pb := 35
	for i, d := range []time.Duration{5 * time.Hour, time.Hour, 9 * time.Hour, 3 * time.Hour} {
		b.WriteString(r3Line(t, r3Rec(fmt.Sprintf("tx%d", i+1), injT.Add(d), "0b", "1:5c", "1:5c0a", 40, &pb)))
	}
	k := keysOn(t, b.String()).Top[0]
	if !k.First.Equal(injT.Add(time.Hour)) {
		t.Fatalf("first %s, want %s", k.First, injT.Add(time.Hour))
	}
	if !k.Last.Equal(injT.Add(9 * time.Hour)) {
		t.Fatalf("last %s, want %s", k.Last, injT.Add(9*time.Hour))
	}
}

// G4 (mutant N8): an injected time is truncated, not rounded. The doc says
// truncation moves a time by less than one second towards the past, and the
// effective jitter window stated in the code depends on it; rounding would
// move half the events into the future instead.
func TestInjectTruncatesTowardsThePast(t *testing.T) {
	// A start three quarters of a second past the second, with no jitter:
	// truncation gives the whole second below it, rounding the one above.
	start := injT.Add(750 * time.Millisecond)
	spec := fmt.Sprintf(`{"seed":7,"final_after":"30s",
 "labelling":{"pulse_merge_gap":"60s","min_period":"1m","max_period":"1h","min_pulses":8,
  "max_harmonic":3,"tolerance_abs":"30s","tolerance_rel":0.1},
 "bots":[{"name":"bot-a","channel":"7f","payload_type":5,"route_class":"flood","route_kind":"relay",
  "first_hop":"1:fd","full_route":"1:fd0a","wire_bytes":60,"payload_bytes":35,"payload_bytes_spread":0,
  "start":%q,"stop":%q,"period":"5m","jitter_sd":"0s",
  "jitter_max":"10s","missed_fraction":0,"multipart_fraction":0,"multipart_max_extra":2,
  "multipart_max_delay":"55s","holes":[]}]}`,
		start.UTC().Format(time.RFC3339Nano), injT.Add(time.Hour).UTC().Format(time.RFC3339Nano))
	r := injectInto(t, spec, backgroundJSONL(10))
	lines := injectedLines(t, r)
	if len(lines) == 0 {
		t.Fatal("no injected lines")
	}
	for _, rec := range lines {
		if rec.Time.Nanosecond() != 0 {
			t.Fatalf("%s is not a whole second: %s", rec.ID, rec.Time)
		}
	}
	if !lines[0].Time.Equal(injT) {
		t.Fatalf("first event at %s, want %s (truncated towards the past)", lines[0].Time, injT)
	}
	if !lines[0].Time.Before(start) {
		t.Fatalf("truncation must move the time towards the past: %s is not before %s", lines[0].Time, start)
	}
}

// r3Episode is one arm's episode, written out in full.
func r3Episode(key string, act, end time.Duration, period time.Duration) episode {
	return episode{channel: "7f", key: key, activation: injT.Add(act), end: injT.Add(end),
		hasPeriod: period > 0, period: period}
}

// G4 (mutants N1/N2): the baseline overlap is closed at both ends. An
// episode of the background that starts exactly when the injected arm's
// episode ends, or ends exactly when it starts, still overlaps it: a
// half-open test would call such an episode the injection's doing.
func TestScoreBaselineOverlapIsClosedAtBothEnds(t *testing.T) {
	const p = 300 * time.Second
	for _, tc := range []struct {
		name             string
		baseAct, baseEnd time.Duration
	}{
		{"baseline starts exactly when the episode ends", 2 * time.Hour, 3 * time.Hour},
		{"baseline ends exactly when the episode starts", 0, time.Hour},
	} {
		eps := []episode{r3Episode(scoreKey, time.Hour, 2*time.Hour, p)}
		markBaseline(eps, []episode{r3Episode(scoreKey, tc.baseAct, tc.baseEnd, p)})
		if !eps[0].baseline {
			t.Fatalf("%s: a touching baseline episode must still mark it", tc.name)
		}
	}
	// One nanosecond clear of the episode at either end is not an overlap.
	for _, tc := range []struct {
		name             string
		baseAct, baseEnd time.Duration
	}{
		{"after", 2*time.Hour + 1, 3 * time.Hour},
		{"before", 0, time.Hour - 1},
	} {
		eps := []episode{r3Episode(scoreKey, time.Hour, 2*time.Hour, p)}
		markBaseline(eps, []episode{r3Episode(scoreKey, tc.baseAct, tc.baseEnd, p)})
		if eps[0].baseline {
			t.Fatalf("%s: a baseline episode clear of the window must not mark it", tc.name)
		}
	}
}

// r3Panel builds n incidents on n keys, with a detection at the given time
// to detect per incident; a negative time to detect means the incident is
// missed (no episode at all).
func r3Panel(p time.Duration, ttds []float64) (InjectLabels, scoreReport) {
	var incs []Incident
	var cands []scoreCandidate
	for i, mult := range ttds {
		start := injT.Add(time.Duration(i) * 12 * time.Hour)
		key := fmt.Sprintf("v1|s=stream|pt=5|ch=1:7f|rc=flood|rk=relay|hop=1:%02x", 0xa0+i)
		incs = append(incs, Incident{Bot: fmt.Sprintf("b%d", i), Channel: "7f", Key: key,
			Start: start, Stop: start.Add(60 * p), Period: dur(p), Pulses: 8})
		if mult < 0 {
			continue
		}
		at := start.Add(time.Duration(mult * float64(p)))
		cands = append(cands, r2Cand(fmt.Sprintf("e%d", i), "active", key, at, p),
			r2Cand(fmt.Sprintf("e%d", i), "ended", key, at.Add(p), 0))
	}
	return InjectLabels{Synthetic: true, Labelling: r2Labelling(8), Incidents: incs},
		scoreReport{Experimental: true, Start: injT, End: injT.Add(240 * time.Hour), Candidates: cands}
}

// G3: a timing bar over the detected incidents only cannot fail on a missed
// bot. Four clean shapes detected at the floor and four realistic ones missed
// gives recall 0.50 and a p75 of 1 P over the detected subset - a pass of
// every timing bar round 2 had. The selected tail ranks a miss above every
// finite value, so the same panel's p75 is infinite and the bar fails.
func TestScoreSelectedTailCountsAMissAsInfinite(t *testing.T) {
	const p = 300 * time.Second
	lab, rep := r3Panel(p, []float64{1, 1, 1, 1, -1, -1, -1, -1})
	res, err := score(rep, lab, "p474", "labels.json", "", nil, nil, injT, injT.Add(240*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.IncidentsSelected != 8 || res.IncidentsDetected != 4 || res.IncidentsMissed != 4 {
		t.Fatalf("selected %d, detected %d, missed %d", res.IncidentsSelected, res.IncidentsDetected, res.IncidentsMissed)
	}
	if res.P75TTDPeriods != 1 {
		t.Fatalf("the detected-only p75 is %.1f P; the statistic round 2 had cannot fail", res.P75TTDPeriods)
	}
	// n = 8 selected: the nearest-rank p75 is the sixth value, and values
	// five to eight are misses, so the p75 is infinite.
	if !res.P75TTDSelectedInfinite || res.P75TTDSelected != nil || res.P75TTDPeriodsSelected != 0 {
		t.Fatalf("p75 over the selected incidents must be infinite: %+v", res)
	}
	// The median is the fourth value, which is still a detection.
	if res.MedianTTDSelectedInfinite || res.MedianTTDSelected == nil ||
		time.Duration(*res.MedianTTDSelected) != p || res.MedianTTDPeriodsSelected != 1 {
		t.Fatalf("median over the selected incidents: %+v", res)
	}
	// With every incident detected the selected tail is finite and equals
	// the detected-only tail.
	fullLab, fullRep := r3Panel(p, []float64{1, 1, 1, 1, 10, 20, 30, 40})
	full, err := score(fullRep, fullLab, "p474", "labels.json", "", nil, nil, injT, injT.Add(240*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if full.IncidentsMissed != 0 || full.P75TTDSelectedInfinite || full.MedianTTDSelectedInfinite {
		t.Fatalf("a complete panel has a finite tail: %+v", full)
	}
	if full.P75TTDPeriodsSelected != full.P75TTDPeriods || full.MedianTTDPeriodsSelected != 1 {
		t.Fatalf("selected %.1f P vs detected %.1f P", full.P75TTDPeriodsSelected, full.P75TTDPeriods)
	}
	// A miss outside the -bots selection must not enter the statistic.
	sub, err := score(rep, lab, "p474", "labels.json", "", nil, []string{"b0", "b1", "b2", "b3"},
		injT, injT.Add(240*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if sub.IncidentsMissed != 0 || sub.P75TTDSelectedInfinite || *sub.Recall != 1 {
		t.Fatalf("-bots subset: missed %d, infinite %v, recall %.2f",
			sub.IncidentsMissed, sub.P75TTDSelectedInfinite, *sub.Recall)
	}
}

// G5: when the only episode of an incident's own series is absorbed by the
// baseline arm, the per-incident row reads as a plain miss. A no-go caused by
// background contamination has to be distinguishable from a detector miss, so
// the row carries the count and a flag.
func TestScoreReportsBaselineMaskingPerIncident(t *testing.T) {
	const p = 300 * time.Second
	l := r2Labelling(8)
	key := scoreKey
	other := "v1|s=stream|pt=5|ch=1:7f|rc=flood|rk=relay|hop=1:ee"
	lab := InjectLabels{Synthetic: true, Labelling: l, Incidents: []Incident{
		{Bot: "masked", Channel: "7f", Key: key, Start: injT, Stop: injT.Add(60 * p), Period: dur(p), Pulses: 8},
		{Bot: "plain-miss", Channel: "7f", Key: other, Start: injT, Stop: injT.Add(60 * p), Period: dur(p), Pulses: 8},
	}}
	rep := scoreReport{Experimental: true, Start: injT, End: injT.Add(48 * time.Hour), Candidates: []scoreCandidate{
		r2Cand("e1", "active", key, injT.Add(10*time.Minute), p),
		r2Cand("e1", "ended", key, injT.Add(2*time.Hour), 0),
	}}
	base := []episode{r3Episode(key, 0, 24*time.Hour, p)}
	res, err := score(rep, lab, "p474", "labels.json", "report-a.json", base, nil, injT, injT.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.BaselineEpisodes != 1 || res.IncidentsDetected != 0 {
		t.Fatalf("baseline episodes %d, detected %d", res.BaselineEpisodes, res.IncidentsDetected)
	}
	if res.IncidentsBaselineMasked != 1 {
		t.Fatalf("incidents_baseline_masked %d, want 1", res.IncidentsBaselineMasked)
	}
	byBot := map[string]incidentScore{}
	for _, is := range res.PerIncident {
		byBot[is.Bot] = is
	}
	if m := byBot["masked"]; !m.BaselineMasked || m.BaselineEpisodes != 1 || m.Detected {
		t.Fatalf("the masked incident's row: %+v", m)
	}
	if pm := byBot["plain-miss"]; pm.BaselineMasked || pm.BaselineEpisodes != 0 || pm.Detected {
		t.Fatalf("a plain miss must not be flagged as masked: %+v", pm)
	}
}
