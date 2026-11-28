package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #473, phase 1: the export's ingest-backlog flag. It is computed
// from observer activity inside the export; only a boolean per event is
// written.

const (
	blSilence = 10 * time.Minute
	blSpan    = 30 * time.Minute
)

// Observers of the backlog fixture (observations.observer_idx).
const (
	obsLive      = 1001 // active all along; hears a real storm
	obsReconnect = 1002 // active, silent, then a backlog burst
	obsLate      = 1003 // first heard in the middle of the range
	obsBoundary  = 1004 // gaps of exactly the silence and one second more
)

// blTx is one transmission of the backlog fixture: its observations (by
// observer, offset from at) and whether its event must be flagged.
type blTx struct {
	at   time.Time
	pt   int
	obs  []blObs
	flag bool
}

type blObs struct {
	observer int // 0: NULL
	d        time.Duration
}

func blAt(d time.Duration) time.Time { return fxT.Add(d) }

// backlogScenario lists the fixture. Times are distinct per transmission,
// so a line's time names its transmission.
func backlogScenario() []blTx {
	var txs []blTx
	one := func(at time.Time, pt, observer int, flag bool) {
		txs = append(txs, blTx{at: at, pt: pt, obs: []blObs{{observer, 0}}, flag: flag})
	}
	// live: an ADVERT every 2 minutes from an hour before Start to the end
	for k := -30; k < 60; k++ {
		one(blAt(time.Duration(k)*2*time.Minute+37*time.Second), 4, obsLive, false)
	}
	// a GRP_TXT just after Start: an observer active since long before is
	// no reconnect at Start (the lookback covers silence + span)
	one(blAt(20*time.Second), 5, obsLive, false)
	// a real storm: 30 GRP_TXT in 30 s, first heard by the live observer,
	// then by the reconnecting one (inside its backlog span): not flagged
	for i := 0; i < 30; i++ {
		txs = append(txs, blTx{at: blAt(40*time.Minute + time.Duration(i)*time.Second), pt: 5,
			obs: []blObs{{obsLive, 0}, {obsReconnect, 2 * time.Second}}})
	}
	// reconnect: active until 4m13s, silent for 15m47s, then 30 GRP_TXT in
	// 30 s heard by it alone (the backlog), then live traffic; the span is
	// [20m, 50m)
	for k := -30; k <= 2; k++ {
		one(blAt(time.Duration(k)*2*time.Minute+13*time.Second), 4, obsReconnect, false)
	}
	for i := 0; i < 30; i++ {
		one(blAt(20*time.Minute+time.Duration(i)*time.Second), 5, obsReconnect, true)
	}
	for _, d := range []time.Duration{25*time.Minute + 17*time.Second, 30*time.Minute + 17*time.Second,
		35*time.Minute + 17*time.Second, 40*time.Minute + 45*time.Second, 45*time.Minute + 17*time.Second,
		50*time.Minute - time.Second} {
		one(blAt(d), 4, obsReconnect, true)
	}
	one(blAt(50*time.Minute), 4, obsReconnect, false)
	one(blAt(55*time.Minute+17*time.Second), 4, obsReconnect, false)
	// late: first heard at 61m2s, as a re-hearing of a transmission the
	// live observer heard first (not flagged); then it is first (flagged)
	txs = append(txs, blTx{at: blAt(61 * time.Minute), pt: 5, obs: []blObs{{obsLive, 0}, {obsLate, 2 * time.Second}}},
		blTx{at: blAt(62 * time.Minute), pt: 5, obs: []blObs{{obsLate, 0}, {obsLive, 3 * time.Second}}, flag: true})
	// no observer: never flagged, also when a flagged observer follows
	one(blAt(70*time.Minute), 5, 0, false)
	txs = append(txs, blTx{at: blAt(71 * time.Minute), pt: 5, obs: []blObs{{0, 0}, {obsLate, time.Second}}})
	// boundary: every 5 minutes, then a gap of exactly the silence (not a
	// reconnect), then a gap one second longer (a reconnect)
	for k := -12; k <= 16; k++ {
		one(blAt(time.Duration(k)*5*time.Minute+51*time.Second), 4, obsBoundary, false)
	}
	one(blAt(90*time.Minute+51*time.Second), 4, obsBoundary, false)
	one(blAt(100*time.Minute+52*time.Second), 4, obsBoundary, true)
	return txs
}

func buildBacklogFixture(t *testing.T) *fixtureDB { return buildFixtureOf(t, backlogScenario()) }

// buildFixtureOf inserts each transmission as a one-hop flood GRP_TXT or
// ADVERT with its observations.
func buildFixtureOf(t *testing.T, txs []blTx) *fixtureDB {
	f := newFixtureDB(t, false)
	grp := grpPayload(0x0a, 20)
	adv := make([]byte, 100)
	for i, x := range txs {
		payload := grp
		if x.pt == 4 {
			payload = adv
		}
		var os []fxObs
		for _, o := range x.obs {
			fo := relay(x.at.Add(o.d), 1, x.pt, 1, []byte{0x0b}, payload)
			fo.observer, fo.noObserver = o.observer, o.observer == 0
			os = append(os, fo)
		}
		f.tx(fxHash(1000+i), frameHex(1, x.pt, 1, []byte{0x0b}, payload), x.pt, os...)
	}
	return f
}

func blOptions(db string) exportOptions {
	opt := fxOptions(db)
	opt.BacklogSilence, opt.BacklogSpan = blSilence, blSpan
	return opt
}

func readRecords(t *testing.T, out []byte) []anomaly.ReplayRecord {
	t.Helper()
	var rs []anomaly.ReplayRecord
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		var r anomaly.ReplayRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %s: %v", sc.Text(), err)
		}
		rs = append(rs, r)
	}
	return rs
}

// The flag follows the definition: an event is flagged exactly when its
// first observation lies in [r, r + span) of a reconnect r of that
// observer, a reconnect being an observation with no observation of the
// same observer in the silence before it.
func TestExportBacklogFlagsAReconnectBurstOnly(t *testing.T) {
	f := buildBacklogFixture(t)
	got, sum := runExportTo(t, blOptions(f.path))
	want := map[time.Time]bool{}
	nflag := 0
	for _, x := range backlogScenario() {
		if !x.at.Before(fxT) && x.at.Before(fxAt(2*time.Hour)) {
			if _, dup := want[x.at]; dup {
				t.Fatalf("scenario: two transmissions at %v", x.at.Sub(fxT))
			}
			want[x.at] = x.flag
			if x.flag {
				nflag++
			}
		}
	}
	rs := readRecords(t, got)
	if len(rs) != len(want) {
		t.Fatalf("%d lines, want %d", len(rs), len(want))
	}
	var wrong []string
	for _, r := range rs {
		w, ok := want[r.Time]
		if !ok {
			t.Fatalf("unexpected line at %v", r.Time.Sub(fxT))
		}
		if r.IngestBacklog != w {
			wrong = append(wrong, fmt.Sprintf("%v: flag %v, want %v", r.Time.Sub(fxT), r.IngestBacklog, w))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d events flagged wrongly:\n%s", len(wrong), strings.Join(wrong, "\n"))
	}
	if sum.IngestBacklog == nil || sum.IngestBacklog.EventsFlagged != nflag || nflag != 38 {
		t.Fatalf("summary %+v, want %d flagged events", sum.IngestBacklog, nflag)
	}
}

// The lookback reaches silence + span before the range (minus settle), so
// an observer active before Start is not a reconnect at Start, and the
// counts in the summary are the rows of that scan.
func TestExportBacklogLooksBackSilencePlusSpan(t *testing.T) {
	f := buildBacklogFixture(t)
	_, sum := runExportTo(t, blOptions(f.path))
	lo := fxT.Add(-fxSettle - blSilence - blSpan).Unix()
	hi := fxAt(2*time.Hour + fxSettle).Unix()
	var rows, null int
	if err := f.db.QueryRow(`SELECT COUNT(*), COUNT(*) - COUNT(observer_idx) FROM observations WHERE timestamp >= ? AND timestamp < ?`,
		lo, hi).Scan(&rows, &null); err != nil {
		t.Fatal(err)
	}
	if b := sum.IngestBacklog; b == nil || b.Rows != rows || b.RowsWithoutObserver != null || null != 2 {
		t.Fatalf("summary %+v, want rows %d, without observer %d", sum.IngestBacklog, rows, null)
	}
}

func stripFlag(b []byte) []byte { return bytes.ReplaceAll(b, []byte(`,"ingest_backlog":true`), nil) }

// Key-free: the flag is the only difference to the export without it. With
// the flag removed every line is byte-identical, the only new key is
// "ingest_backlog", its only value is true, and no sentinel, observer
// index or other column value reaches the output.
func TestExportBacklogAddsOnlyTheBoolean(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *fixtureDB{
		"backlog fixture": buildBacklogFixture,
		"export fixture": func(t *testing.T) *fixtureDB {
			f := buildExportFixture(t, false)
			// one row an hour before Start, so the lookback has data
			grp := grpPayload(0x0a, 20)
			f.tx(fxHash(999), frameHex(1, 5, 1, nil, grp), 5, relay(fxAt(-time.Hour), 1, 5, 1, nil, grp))
			return f
		},
	} {
		f := build(t)
		plain, _ := runExportTo(t, fxOptions(f.path))
		got, sum := runExportTo(t, blOptions(f.path))
		if !bytes.Equal(stripFlag(got), plain) {
			t.Fatalf("%s: the flagged export differs from the plain one in more than the flag", name)
		}
		if n := bytes.Count(got, []byte(`"ingest_backlog"`)); n == 0 || n != sum.IngestBacklog.EventsFlagged ||
			n != bytes.Count(got, []byte(`,"ingest_backlog":true}`+"\n")) {
			t.Fatalf("%s: %d flags, summary %+v", name, n, sum.IngestBacklog)
		}
		for _, s := range []string{sentinelText, sentinelChannel, sentinelPubkey, sentinelObserver, sentinelObsID,
			sentinelResolved, "SENTINEL", "-7.5", "-101", `"rx"`, "observer"} {
			if bytes.Contains(got, []byte(s)) {
				t.Fatalf("%s: export output contains %q", name, s)
			}
		}
		if bytes.Contains(got, []byte("1001")) || bytes.Contains(got, []byte("1002")) || bytes.Contains(got, []byte("1003")) {
			t.Fatalf("%s: an observer index reaches the output", name)
		}
		keys := map[string]bool{}
		sc := bufio.NewScanner(bytes.NewReader(got))
		for sc.Scan() {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatal(err)
			}
			for k, v := range m {
				keys[k] = true
				if k == "ingest_backlog" && string(v) != "true" {
					t.Fatalf("%s: ingest_backlog = %s", name, v)
				}
			}
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		if got, want := strings.Join(ks, ","), "channel,decryption,final_at,first_hop,full_route,id,ingest_backlog,"+
			"payload_bytes,payload_type,route_class,route_kind,time,traffic,wire_bytes"; got != want {
			t.Fatalf("%s: keys %s, want %s", name, got, want)
		}
	}
}

// The output depends on observers only through their activity: renaming
// every observer (any bijection of observer_idx) gives a byte-identical
// export, so no observer identity can be encoded in it.
func TestExportBacklogIsInvariantUnderObserverRenaming(t *testing.T) {
	f := buildBacklogFixture(t)
	before, _ := runExportTo(t, blOptions(f.path))
	for _, q := range []string{
		`UPDATE observations SET observer_idx = 900000 - observer_idx WHERE observer_idx IS NOT NULL`,
		`UPDATE observations SET observer_idx = CASE observer_idx WHEN 899000 THEN 7 WHEN 898999 THEN 3 ELSE observer_idx END`,
	} {
		if _, err := f.db.Exec(q); err != nil {
			t.Fatal(err)
		}
		after, _ := runExportTo(t, blOptions(f.path))
		if !bytes.Equal(after, before) {
			t.Fatalf("renaming observers changed the export (%s)", q)
		}
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE observer_idx IN (3, 7)`).Scan(&n)
	if n == 0 {
		t.Fatal("the renaming touched no rows")
	}
}

// Without the two options the export is unchanged: no flag, no backlog
// summary, and the query is the #457 one (it never names an observer
// column). The options come as a pair.
func TestExportBacklogIsOptIn(t *testing.T) {
	f := buildBacklogFixture(t)
	got, sum := runExportTo(t, fxOptions(f.path))
	if bytes.Contains(got, []byte("ingest_backlog")) || sum.IngestBacklog != nil {
		t.Fatalf("flag or summary without the options: %+v", sum.IngestBacklog)
	}
	b, _ := json.Marshal(sum)
	if bytes.Contains(b, []byte("ingest_backlog")) {
		t.Fatalf("summary names the backlog: %s", b)
	}
	for _, c := range []struct{ s, b time.Duration }{
		{blSilence, 0}, {0, blSpan}, {-time.Second, blSpan}, {blSilence, 1500 * time.Millisecond},
		{25 * time.Hour, blSpan}, {blSilence, 25 * time.Hour},
	} {
		opt := fxOptions(f.path)
		opt.BacklogSilence, opt.BacklogSpan = c.s, c.b
		if err := opt.validate(); err == nil {
			t.Fatalf("silence %v span %v accepted", c.s, c.b)
		}
	}
}

// The export query reads observations.observer_idx only with the flag, and
// the activity scan reads only observer_idx, time and row id; neither
// touches the observers table or any other identifying column.
func TestExportBacklogQueriesReadOnlyTheObserverIndex(t *testing.T) {
	strip := func(q string) string {
		return strings.ReplaceAll(strings.ToLower(q), "o.observer_idx", "")
	}
	for _, q := range []string{exportQuery(true, true), exportQuery(false, true), backlogQuery} {
		if !strings.Contains(q, "o.observer_idx") {
			t.Fatalf("query lacks the observer index:\n%s", q)
		}
		s := strip(q)
		for _, col := range []string{"decoded_json", "from_pubkey", "channel_hash", "observer", "snr", "rssi",
			"score", "direction", "resolved_path", "name", "first_seen", "hash", "*", "iata"} {
			if strings.Contains(s, col) {
				t.Fatalf("query reads %q:\n%s", col, q)
			}
		}
	}
	if s := strings.ToLower(backlogQuery); strings.Contains(s, "join") || strings.Contains(s, "raw_hex") ||
		strings.Contains(s, "path_json") || strings.Contains(s, "transmission") {
		t.Fatalf("activity scan reads more than activity:\n%s", backlogQuery)
	}
	for _, own := range []bool{true, false} {
		if exportQuery(own, false) == exportQuery(own, true) || strings.Contains(exportQuery(own, false), "observer") {
			t.Fatal("the export query without the flag names the observer")
		}
	}
}

// The activity scan is one index search on observations.timestamp in time
// order (no scan, no sort), also when the (observer_idx, timestamp) index
// of newer databases exists; the export query with the observer column
// keeps its #457 plan.
func TestExportBacklogQueryPlansAreBounded(t *testing.T) {
	f := buildBacklogFixture(t)
	if _, err := f.db.Exec(`CREATE INDEX idx_observations_observer_idx_timestamp ON observations(observer_idx, timestamp)`); err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plan := func(q string, args ...interface{}) string {
		rows, err := db.Query(`EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var p []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			p = append(p, detail)
		}
		return strings.Join(p, "\n")
	}
	for q, wants := range map[string][]string{
		plan(backlogQuery, 0, 1, 1): {"SEARCH o USING INDEX idx_observations_timestamp (timestamp>? AND timestamp<?)"},
		plan(firstObservationQuery): {"SEARCH observations USING COVERING INDEX idx_observations_timestamp"},
		plan(exportQuery(true, true), 0, 1, 1, 1): {
			"SEARCH o USING INDEX idx_observations_timestamp (timestamp>? AND timestamp<?)",
			"SEARCH t USING INTEGER PRIMARY KEY (rowid=?)",
			"SEARCH p USING COVERING INDEX idx_observations_tx_ts (transmission_id=? AND timestamp<?)"},
	} {
		for _, w := range wants {
			if !strings.Contains(q, w) {
				t.Fatalf("plan lacks %q:\n%s", w, q)
			}
		}
		for _, bad := range []string{"SCAN", "TEMP B-TREE"} {
			if strings.Contains(q, bad) {
				t.Fatalf("plan has %q:\n%s", bad, q)
			}
		}
	}
}

// Data that does not reach back over the whole lookback would make every
// observer look reconnected at its first row, so the export refuses it.
func TestExportBacklogNeedsTheWholeLookback(t *testing.T) {
	f := buildBacklogFixture(t)
	var first int64
	if err := f.db.QueryRow(`SELECT MIN(timestamp) FROM observations`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	lo := func(opt exportOptions) int64 {
		return opt.Start.Add(-opt.Settle - opt.BacklogSilence - opt.BacklogSpan).Unix()
	}
	opt := blOptions(f.path)
	opt.BacklogSpan = time.Duration(lo(opt)-first)*time.Second + opt.BacklogSpan // lookback starts at the first row
	if lo(opt) != first {
		t.Fatalf("lookback %d, first row %d", lo(opt), first)
	}
	runExportTo(t, opt)
	opt.BacklogSpan += time.Second
	if _, err := exportRecords(opt, io.Discard); err == nil || !strings.Contains(err.Error(), "first observation") {
		t.Fatalf("a lookback one second before the first observation was accepted: %v", err)
	}
	empty := newFixtureDB(t, false)
	if _, err := exportRecords(blOptions(empty.path), io.Discard); err == nil {
		t.Fatal("a database without observations was accepted")
	}
}

// -max-rows also caps the activity scan, which covers the lookback.
func TestExportBacklogScanIsCappedByMaxRows(t *testing.T) {
	f := buildBacklogFixture(t)
	_, sum := runExportTo(t, blOptions(f.path))
	opt := blOptions(f.path)
	opt.MaxRows = sum.IngestBacklog.Rows
	runExportTo(t, opt)
	opt.MaxRows--
	if _, err := exportRecords(opt, io.Discard); err == nil || !strings.Contains(err.Error(), "max-rows") {
		t.Fatalf("activity scan of %d rows allowed past a cap of %d: %v", sum.IngestBacklog.Rows, opt.MaxRows, err)
	}
}

// The tracker holds at most maxObservers observers and maxSpans spans;
// one more fails the export instead of growing.
func TestBacklogTrackerIsBounded(t *testing.T) {
	b := newBacklogTracker(int64(blSilence/time.Second), int64(blSpan/time.Second), 2, 3)
	for _, o := range []int64{1, 2, 1, 2} {
		if err := b.see(o, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.see(3, 100); err == nil {
		t.Fatal("a third observer was accepted")
	}
	if err := b.see(1, 5000); err != nil { // a reconnect after [100, 1900): the third span
		t.Fatal(err)
	}
	if err := b.see(2, 9000); err == nil { // a fourth span
		t.Fatal("a fourth span was accepted")
	}
}

// Spans of one observer that overlap are merged, and a lookup walks them
// forward: every second of [r, r + span) is flagged and nothing else.
func TestBacklogTrackerSpans(t *testing.T) {
	b := newBacklogTracker(600, 1800, 10, 10)
	for _, ts := range []int64{1000, 1500, 2200, 4000, 4500, 9000} {
		if err := b.see(7, ts); err != nil {
			t.Fatal(err)
		}
	}
	// reconnects: 1000 (first seen), 2200 (gap 700 > 600; merged: [1000, 4000)),
	// 4000 (gap 1800; merged: [1000, 5800)), 9000 (gap 4500): [9000, 10800)
	for _, c := range []struct {
		ts   int64
		want bool
	}{{999, false}, {1000, true}, {5799, true}, {5800, false}, {8999, false}, {9000, true}, {10799, true}, {10800, false}} {
		if got := b.flagged(sql.NullInt64{Int64: 7, Valid: true}, c.ts); got != c.want {
			t.Fatalf("t=%d: flagged %v, want %v", c.ts, got, c.want)
		}
	}
	if b.flagged(sql.NullInt64{Int64: 8, Valid: true}, 1000) || b.flagged(sql.NullInt64{}, 1000) {
		t.Fatal("an unknown or NULL observer was flagged")
	}
}

func blArgs(db, out string, extra ...string) []string {
	return append(exportArgs(db, out, "-backlog-silence", "10m", "-backlog-span", "30m"), extra...)
}

// The command takes the options as a pair of explicit flags and prints
// the backlog counts in its summary.
func TestExportCommandBacklogFlags(t *testing.T) {
	f := buildBacklogFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "events.jsonl")
	var stdout, stderr bytes.Buffer
	if code := run(blArgs(f.path, out), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want, _ := runExportTo(t, blOptions(f.path))
	if got, err := os.ReadFile(out); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("output differs from exportRecords (%v)", err)
	}
	var sum exportSummary
	if err := json.Unmarshal(stderr.Bytes(), &sum); err != nil || sum.IngestBacklog == nil || sum.IngestBacklog.EventsFlagged != 38 {
		t.Fatalf("summary %q: %v", stderr.String(), err)
	}
	if !regexp.MustCompile(`"ingest_backlog": \{\s*"rows": \d+,\s*"rows_without_observer": 2,\s*"events_flagged": 38\s*\}`).Match(stderr.Bytes()) {
		t.Fatalf("summary shape:\n%s", stderr.String())
	}
	for name, args := range map[string][]string{
		"silence only": exportArgs(f.path, filepath.Join(dir, "a"), "-backlog-silence", "10m"),
		"span only":    exportArgs(f.path, filepath.Join(dir, "b"), "-backlog-span", "30m"),
		"zero silence": exportArgs(f.path, filepath.Join(dir, "c"), "-backlog-silence", "0s", "-backlog-span", "30m"),
		"span 25h":     exportArgs(f.path, filepath.Join(dir, "d"), "-backlog-silence", "10m", "-backlog-span", "25h"),
		"fractional":   exportArgs(f.path, filepath.Join(dir, "e"), "-backlog-silence", "10m", "-backlog-span", "1.5s"),
	} {
		var so, se bytes.Buffer
		if code := run(args, nil, &so, &se); code != 2 {
			t.Fatalf("%s: exit %d, want 2 (%s)", name, code, se.String())
		}
	}
}

// End to end: the reconnect burst alone fires the unguarded rate rule but
// not the guarded one; the real storm fires both.
func TestExportThenGuardedReplayIgnoresTheBacklog(t *testing.T) {
	f := buildBacklogFixture(t)
	got, _ := runExportTo(t, blOptions(f.path))
	cfg := func(guard bool) anomaly.Config {
		g := ""
		if guard {
			g = `, "exclude_ingest_backlog": true`
		}
		c, err := anomaly.ParseReplayConfig([]byte(`{
 "limits": {"max_keys_per_scope": 1000, "state_ttl": "48h", "dedup_horizon": "6h", "max_dedup_ids": 100000,
  "reorder_delay": "0s", "max_reorder_buffer": 64, "max_window_entries": 4096,
  "max_candidates_per_call": 1000, "max_pending_candidates": 10000},
 "expected": "include",
 "rate": [{"name": "grp", "scope": "global", "payload_types": [5], "window": "5m", "threshold": 20` + g + `,
  "state": {"confirm_after": "0s", "end_after_quiet": "5m", "cooldown": "0s"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	activations := func(guard bool) []time.Duration {
		rep, err := anomaly.Replay(bytes.NewReader(got), cfg(guard), anomaly.ReplayOptions{Start: fxT, End: fxAt(2 * time.Hour), MaxLineBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		var as []time.Duration
		for _, c := range rep.Candidates {
			if c.To == anomaly.StateActive {
				as = append(as, c.At.Sub(fxT))
			}
		}
		return as
	}
	if a := activations(false); len(a) != 2 || a[0] != 20*time.Minute+19*time.Second || a[1] != 40*time.Minute+19*time.Second {
		t.Fatalf("unguarded activations %v, want the burst (20m19s) and the storm (40m19s)", a)
	}
	if a := activations(true); len(a) != 1 || a[0] != 40*time.Minute+19*time.Second {
		t.Fatalf("guarded activations %v, want the storm only (40m19s)", a)
	}
}

// The checked-in E2E fixture (older schema) exports with the flag; the
// flagged output differs from the plain one in the flag only. The fixture
// holds 16:01-18:09 of one day, so a lookback into the day before is
// refused, and one of 40m30s from 17:00 fits.
func TestExportBacklogE2EFixture(t *testing.T) {
	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	if _, err := os.Stat(src); err != nil {
		t.Skip("e2e fixture not present")
	}
	before := fileSum(t, src)
	day := exportOptions{DB: src, Start: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC), Settle: 30 * time.Second, MaxRows: 100000,
		BacklogSilence: 10 * time.Minute, BacklogSpan: 30 * time.Minute}
	if _, err := exportRecords(day, io.Discard); err == nil {
		t.Fatal("a lookback from before the fixture's first observation was accepted")
	}
	opt := exportOptions{DB: src, Start: time.Date(2026, 5, 17, 17, 0, 0, 0, time.UTC),
		End: time.Date(2026, 5, 17, 18, 0, 0, 0, time.UTC), Settle: 30 * time.Second, MaxRows: 100000}
	plain, _ := runExportTo(t, opt)
	opt.BacklogSilence, opt.BacklogSpan = day.BacklogSilence, day.BacklogSpan
	got, sum := runExportTo(t, opt)
	if fileSum(t, src) != before {
		t.Fatal("e2e fixture changed")
	}
	if !bytes.Equal(stripFlag(got), plain) || sum.IngestBacklog == nil || sum.IngestBacklog.Rows == 0 || sum.EventsWritten < 100 {
		t.Fatalf("flagged export differs beyond the flag, or summary %+v (%d events)", sum.IngestBacklog, sum.EventsWritten)
	}
	t.Logf("e2e fixture: %d of %d events flagged", sum.IngestBacklog.EventsFlagged, sum.EventsWritten)
}
