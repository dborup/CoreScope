package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #538: the export's late-delivery flag. An event is flagged when its
// first observation lies in a burst of its observer that repeats
// transmissions other observers heard minutes earlier. Only the existing
// ingest_backlog boolean is written.

const (
	ltWindow  = time.Minute
	ltMinRows = 10
	ltShare   = 0.5
	ltGap     = 2 * time.Minute
)

// Observers of the late-delivery fixture (observations.observer_idx).
const (
	ltA        = 81001 // live, hears every background ADVERT first
	ltB        = 81002 // live, one second after A
	ltDrain    = 81003 // live, then a 40-minute outage, then delivers its queue at once
	ltQuiet    = 81004 // silent, then first to hear a real storm (the #473 loss)
	ltSkew     = 81005 // clock 167 s behind: its rows look early, everyone else's late
	ltIsolated = 81006 // silent, then a real storm nobody else hears
	ltAgain    = 81007 // delivers Drain's outage transmissions once more, an hour later
)

// ltGroup names the part of the scenario a transmission belongs to.
type ltGroup string

const (
	ltBackground ltGroup = "background"
	ltDrainQueue ltGroup = "drain backlog"
	ltQuietStorm ltGroup = "storm, quiet observer first"
	ltSkewStorm  ltGroup = "storm, skewed observer"
	ltIsoStorm   ltGroup = "storm, isolated observer"
	ltNoObserver ltGroup = "no observer"
)

type ltTx struct {
	blTx
	group ltGroup
}

func ltMin(m int, s int) time.Duration {
	return time.Duration(m)*time.Minute + time.Duration(s)*time.Second
}

// lateScenario lists the fixture; flag marks the events the late-delivery
// flag must set.
func lateScenario() []ltTx {
	var txs []ltTx
	add := func(g ltGroup, at time.Time, pt int, flag bool, obs ...blObs) {
		txs = append(txs, ltTx{blTx{at: at, pt: pt, obs: obs, flag: flag}, g})
	}
	// background: one ADVERT a minute at m:37, heard by A, then B, then
	// Drain; Drain is out for [5m, 45m) and delivers those rows from 45m00s
	// on, one a second, stamped with its delivery time; Skew hears the ADVERTs
	// of minutes 60..85, stamped 167 s early
	for m := -150; m < 130; m++ {
		at := blAt(ltMin(m, 37))
		obs := []blObs{{ltA, 0}, {ltB, time.Second}}
		switch {
		case m >= 5 && m < 45:
			// a later delivery by another observer does not hide that A
			// and B heard it long before Drain: only observations before
			// t + gap count
			obs = append(obs, blObs{ltDrain, blAt(ltMin(45, m-5)).Sub(at)}, blObs{ltAgain, blAt(ltMin(105, m-5)).Sub(at)})
		case m != 45:
			obs = append(obs, blObs{ltDrain, 2 * time.Second})
		}
		if m >= 60 && m <= 85 {
			obs = append(obs, blObs{ltSkew, -167 * time.Second})
		}
		add(ltBackground, at, 4, false, obs...)
	}
	// the queue also holds 25 GRP_TXT nobody else heard: the backlog storm
	for i := 0; i < 25; i++ {
		add(ltDrainQueue, blAt(ltMin(45, i)), 5, true, blObs{ltDrain, 0})
	}
	// a first observation without an observer inside Drain's burst, and
	// one with Drain second: neither is flagged
	add(ltNoObserver, blAt(ltMin(45, 45)), 4, false, blObs{0, 0}, blObs{ltDrain, time.Second})
	// a real storm of 30 GRP_TXT first heard by an observer that was silent
	// for hours, then by A; the #473 flag marks it
	for i := 0; i < 30; i++ {
		add(ltQuietStorm, blAt(ltMin(55, i)), 5, false, blObs{ltQuiet, 0}, blObs{ltA, 2 * time.Second})
	}
	// a real storm of 40 GRP_TXT heard by A; Skew hears three in four, so
	// A's rows around it are mostly "already seen", but all with the same
	// delay (Skew's clock offset)
	for i := 0; i < 40; i++ {
		obs := []blObs{{ltA, 0}}
		if i%4 != 3 {
			obs = append(obs, blObs{ltSkew, -161 * time.Second})
		}
		add(ltSkewStorm, blAt(ltMin(75, i)), 5, false, obs...)
	}
	// a real storm of 25 GRP_TXT that one silent observer hears alone
	for i := 0; i < 25; i++ {
		add(ltIsoStorm, blAt(ltMin(95, i)), 5, false, blObs{ltIsolated, 0})
	}
	return txs
}

// eventTime is the time of a transmission's event: its first observation.
func (x ltTx) eventTime() time.Time {
	t := x.at.Add(x.obs[0].d)
	for _, o := range x.obs[1:] {
		if at := x.at.Add(o.d); at.Before(t) {
			t = at
		}
	}
	return t
}

func buildLateFixture(t *testing.T) *fixtureDB {
	var txs []blTx
	for _, x := range lateScenario() {
		txs = append(txs, x.blTx)
	}
	return buildFixtureOf(t, txs)
}

func ltOptions(db string) exportOptions {
	opt := fxOptions(db)
	opt.LateWindow, opt.LateMinRows, opt.LateShare, opt.LateGap = ltWindow, ltMinRows, ltShare, ltGap
	return opt
}

// ltWant maps each in-range event time to its scenario transmission.
func ltWant(t *testing.T) map[time.Time]ltTx {
	t.Helper()
	want := map[time.Time]ltTx{}
	for _, x := range lateScenario() {
		at := x.eventTime()
		if at.Before(fxT) || !at.Before(fxAt(2*time.Hour)) {
			continue
		}
		if _, dup := want[at]; dup {
			t.Fatalf("scenario: two events at %v", at.Sub(fxT))
		}
		want[at] = x
	}
	return want
}

// flagsByGroup counts events and flagged events per group.
func flagsByGroup(t *testing.T, out []byte, want map[time.Time]ltTx) map[ltGroup][2]int {
	t.Helper()
	rs := readRecords(t, out)
	if len(rs) != len(want) {
		t.Fatalf("%d lines, want %d", len(rs), len(want))
	}
	g := map[ltGroup][2]int{}
	for _, r := range rs {
		x, ok := want[r.Time]
		if !ok {
			t.Fatalf("unexpected line at %v", r.Time.Sub(fxT))
		}
		c := g[x.group]
		c[0]++
		if r.IngestBacklog {
			c[1]++
		}
		g[x.group] = c
	}
	return g
}

// The flag follows the definition: exactly the backlog storm Drain delivers
// is flagged, and none of the three real storms, while the #473 flag
// (first observations after a 10-minute silence, for 2 h) marks the
// backlog and also every event of the storms a quiet, skewed or isolated
// observer heard first.
func TestExportLateDeliveryFlagsTheDrainBacklogOnly(t *testing.T) {
	f := buildLateFixture(t)
	f.tx(fxHash(9999), frameHex(1, 4, 1, nil, make([]byte, 100)), 4, fxObs{at: fxAt(-3 * time.Hour), observer: ltA,
		raw: frameHex(1, 4, 1, nil, make([]byte, 100))}) // lookback data for the #473 flag
	want := ltWant(t)
	got, sum := runExportTo(t, ltOptions(f.path))
	var wrong []string
	for _, r := range readRecords(t, got) {
		if x := want[r.Time]; r.IngestBacklog != x.flag {
			wrong = append(wrong, fmt.Sprintf("%v (%s): flag %v, want %v", r.Time.Sub(fxT), x.group, r.IngestBacklog, x.flag))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d events flagged wrongly:\n%s", len(wrong), strings.Join(wrong, "\n"))
	}
	if sum.LateDelivery == nil || sum.LateDelivery.EventsFlagged != 25 || sum.IngestBacklog != nil {
		t.Fatalf("summary %+v / %+v, want 25 flagged events under late_delivery only", sum.LateDelivery, sum.IngestBacklog)
	}
	late := flagsByGroup(t, got, want)

	old := fxOptions(f.path)
	old.BacklogSilence, old.BacklogSpan = 10*time.Minute, 2*time.Hour
	oldOut, _ := runExportTo(t, old)
	prev := flagsByGroup(t, oldOut, want)
	groups := []ltGroup{ltBackground, ltDrainQueue, ltQuietStorm, ltSkewStorm, ltIsoStorm, ltNoObserver}
	for _, g := range groups {
		t.Logf("%-30s events %3d   #473 flag %3d   late-delivery flag %3d", g, late[g][0], prev[g][1], late[g][1])
	}
	for g, w := range map[ltGroup]int{ltDrainQueue: 25, ltQuietStorm: 30, ltSkewStorm: 30, ltIsoStorm: 25} {
		if prev[g][1] != w {
			t.Fatalf("#473 flag on %s: %d flagged, want %d", g, prev[g][1], w)
		}
	}
}

// The late-delivery summary counts the activity scan: its rows cover
// [start - settle - 2 window, end + settle + 2 window), rows without an
// observer, and rows whose transmission another observer heard at least the
// gap earlier and none within the gap.
func TestExportLateDeliverySummaryCountsTheScan(t *testing.T) {
	f := buildLateFixture(t)
	_, sum := runExportTo(t, ltOptions(f.path))
	lo := fxT.Add(-fxSettle - 2*ltWindow).Unix()
	hi := fxAt(2*time.Hour + fxSettle + 2*ltWindow).Unix()
	var rows, null, seen int
	if err := f.db.QueryRow(`SELECT COUNT(*), COUNT(*) - COUNT(observer_idx),
		SUM(EXISTS (SELECT 1 FROM observations p WHERE p.transmission_id = o.transmission_id
			AND p.observer_idx IS NOT NULL AND p.observer_idx <> o.observer_idx AND p.timestamp <= o.timestamp - ?1)
		  AND NOT EXISTS (SELECT 1 FROM observations p WHERE p.transmission_id = o.transmission_id
			AND p.observer_idx IS NOT NULL AND p.observer_idx <> o.observer_idx
			AND p.timestamp > o.timestamp - ?1 AND p.timestamp < o.timestamp + ?1))
		FROM observations o WHERE timestamp >= ?2 AND timestamp < ?3`, int64(ltGap/time.Second), lo, hi).Scan(&rows, &null, &seen); err != nil {
		t.Fatal(err)
	}
	if l := sum.LateDelivery; l == nil || l.Rows != rows || l.RowsWithoutObserver != null || l.RowsAlreadySeen != seen ||
		null != 1 || seen == 0 {
		t.Fatalf("summary %+v, want rows %d, without observer %d, already seen %d", sum.LateDelivery, rows, null, seen)
	}
}

// Key-free: the flag is the only difference to the export without it, the
// only new key is "ingest_backlog" with the value true, and no sentinel or
// observer index reaches the output.
func TestExportLateDeliveryAddsOnlyTheBoolean(t *testing.T) {
	f := buildLateFixture(t)
	plain, _ := runExportTo(t, fxOptions(f.path))
	got, sum := runExportTo(t, ltOptions(f.path))
	if !bytes.Equal(stripFlag(got), plain) {
		t.Fatal("the flagged export differs from the plain one in more than the flag")
	}
	if n := bytes.Count(got, []byte(`"ingest_backlog"`)); n == 0 || n != sum.LateDelivery.EventsFlagged ||
		n != bytes.Count(got, []byte(`,"ingest_backlog":true}`+"\n")) {
		t.Fatalf("%d flags, summary %+v", n, sum.LateDelivery)
	}
	for _, s := range []string{sentinelText, sentinelChannel, sentinelPubkey, sentinelObserver, sentinelObsID,
		sentinelResolved, "SENTINEL", "-7.5", "-101", `"rx"`, "observer", "late", "8100"} {
		if bytes.Contains(got, []byte(s)) {
			t.Fatalf("export output contains %q", s)
		}
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
				t.Fatalf("ingest_backlog = %s", v)
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
		t.Fatalf("keys %s, want %s", got, want)
	}
}

// The output depends on observers only through which rows share one:
// renaming every observer (any bijection of observer_idx) gives a
// byte-identical export and summary.
func TestExportLateDeliveryIsInvariantUnderObserverRenaming(t *testing.T) {
	f := buildLateFixture(t)
	before, sumBefore := runExportTo(t, ltOptions(f.path))
	for _, q := range []string{
		`UPDATE observations SET observer_idx = 900000 - observer_idx WHERE observer_idx IS NOT NULL`,
		`UPDATE observations SET observer_idx = CASE observer_idx WHEN 818999 THEN 7 WHEN 818997 THEN 3 ELSE observer_idx END`,
		// a shift by an odd number: every index changes parity
		`UPDATE observations SET observer_idx = observer_idx + 1000001 WHERE observer_idx IS NOT NULL`,
	} {
		if _, err := f.db.Exec(q); err != nil {
			t.Fatal(err)
		}
		after, sumAfter := runExportTo(t, ltOptions(f.path))
		if !bytes.Equal(after, before) || *sumAfter.LateDelivery != *sumBefore.LateDelivery {
			t.Fatalf("renaming observers changed the export (%s)", q)
		}
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE observer_idx IN (1000004, 1000008)`).Scan(&n)
	if n == 0 {
		t.Fatal("the renaming touched no rows")
	}
}

// Without the -late-* options the export is unchanged: no flag and no
// late_delivery summary. The four options come together, each in range,
// and never with the #473 options.
func TestExportLateDeliveryIsOptIn(t *testing.T) {
	f := buildLateFixture(t)
	got, sum := runExportTo(t, fxOptions(f.path))
	if bytes.Contains(got, []byte("ingest_backlog")) || sum.LateDelivery != nil {
		t.Fatalf("flag or summary without the options: %+v", sum.LateDelivery)
	}
	if b, _ := json.Marshal(sum); bytes.Contains(b, []byte("late")) {
		t.Fatalf("summary names the late-delivery flag: %s", b)
	}
	if err := ltOptions(f.path).validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*exportOptions){
		"window only":     func(o *exportOptions) { *o = fxOptions(o.DB); o.LateWindow = ltWindow },
		"min rows only":   func(o *exportOptions) { *o = fxOptions(o.DB); o.LateMinRows = ltMinRows },
		"share only":      func(o *exportOptions) { *o = fxOptions(o.DB); o.LateShare = ltShare },
		"gap only":        func(o *exportOptions) { *o = fxOptions(o.DB); o.LateGap = ltGap },
		"no window":       func(o *exportOptions) { o.LateWindow = 0 },
		"no gap":          func(o *exportOptions) { o.LateGap = 0 },
		"no min rows":     func(o *exportOptions) { o.LateMinRows = 0 },
		"no share":        func(o *exportOptions) { o.LateShare = 0 },
		"negative window": func(o *exportOptions) { o.LateWindow = -time.Minute },
		"window 1h1s":     func(o *exportOptions) { o.LateWindow = time.Hour + time.Second },
		"gap 1h1s":        func(o *exportOptions) { o.LateGap = time.Hour + time.Second },
		"fractional gap":  func(o *exportOptions) { o.LateGap = 1500 * time.Millisecond },
		"min rows 2^20+1": func(o *exportOptions) { o.LateMinRows = maxLateMinRows + 1 },
		"share 1.01":      func(o *exportOptions) { o.LateShare = 1.01 },
		"negative share":  func(o *exportOptions) { o.LateShare = -0.5 },
		"NaN share":       func(o *exportOptions) { o.LateShare = math.NaN() },
		"with #473 flag":  func(o *exportOptions) { o.BacklogSilence, o.BacklogSpan = blSilence, blSpan },
	} {
		opt := ltOptions(f.path)
		mod(&opt)
		if err := opt.validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	for _, ok := range []func(*exportOptions){
		func(o *exportOptions) { o.LateWindow, o.LateGap = time.Hour, time.Second },
		func(o *exportOptions) { o.LateMinRows, o.LateShare = maxLateMinRows, 1 },
	} {
		opt := ltOptions(f.path)
		ok(&opt)
		if err := opt.validate(); err != nil {
			t.Fatalf("a bound value was refused: %v", err)
		}
	}
}

// The activity scan reads only the observer index, time, row id and
// transmission row id; it joins no table and touches no other column.
func TestLateQueryReadsOnlyActivity(t *testing.T) {
	s := strings.NewReplacer("o.observer_idx", "", "p.observer_idx", "").Replace(strings.ToLower(lateQuery))
	for _, col := range []string{"decoded_json", "from_pubkey", "channel_hash", "observer", "snr", "rssi", "score",
		"direction", "resolved_path", "name", "first_seen", "hash", "*", "iata", "join", "raw_hex", "path_json", "transmissions"} {
		if strings.Contains(s, col) {
			t.Fatalf("activity scan reads %q:\n%s", col, lateQuery)
		}
	}
}

// The activity scan is one index search on observations.timestamp in time
// order, and its subquery one index search per row on the transmission's
// observations (no scan, no sort).
func TestLateQueryPlanIsBounded(t *testing.T) {
	f := buildLateFixture(t)
	db, err := openReadOnly(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+lateQuery, 0, 1, 1, 1)
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
	plan := strings.Join(p, "\n")
	for _, w := range []string{"SEARCH o USING INDEX idx_observations_timestamp (timestamp>? AND timestamp<?)",
		"SEARCH p USING INDEX idx_observations_tx_ts (transmission_id=? AND timestamp<?)"} {
		if !strings.Contains(plan, w) {
			t.Fatalf("plan lacks %q:\n%s", w, plan)
		}
	}
	for _, bad := range []string{"SCAN", "TEMP B-TREE"} {
		if strings.Contains(plan, bad) {
			t.Fatalf("plan has %q:\n%s", bad, plan)
		}
	}
}

// -max-rows also caps the activity scan.
func TestExportLateDeliveryScanIsCappedByMaxRows(t *testing.T) {
	f := buildLateFixture(t)
	opt := ltOptions(f.path)
	opt.MaxRows = 100000
	_, sum := runExportTo(t, opt)
	opt.MaxRows = sum.LateDelivery.Rows
	runExportTo(t, opt)
	opt.MaxRows--
	if _, err := exportRecords(opt, io.Discard); err == nil || !strings.Contains(err.Error(), "late-delivery activity scan") {
		t.Fatalf("activity scan of %d rows allowed past a cap of %d: %v", sum.LateDelivery.Rows, opt.MaxRows, err)
	}
}

func ltTracker(minRows int, maxBins int) *lateTracker {
	opt := exportOptions{LateWindow: ltWindow, LateMinRows: minRows, LateShare: ltShare, LateGap: ltGap}
	return newLateTracker(opt, maxBins)
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// A burst needs every condition at its bound: rows >= min rows, already-seen
// share >= share, and already-seen delays spread over >= gap; a row is
// already seen at a delay of exactly the gap. The window is the bin and its
// two neighbours.
func TestLateTrackerBurst(t *testing.T) {
	type row struct {
		ts int64
		d  int64 // < 0: no observation elsewhere
	}
	cases := []struct {
		name string
		rows []row
		want bool
	}{
		{"at every bound", []row{{600, 120}, {601, 240}, {602, -1}, {603, -1}}, true},
		{"one row short", []row{{600, 120}, {601, 240}, {602, -1}}, false},
		{"share just below", []row{{600, 120}, {601, 240}, {602, -1}, {603, -1}, {604, -1}}, false},
		{"delay one second below the gap", []row{{600, 119}, {601, 240}, {602, 240}, {603, -1}}, false},
		{"spread one second short", []row{{600, 121}, {601, 240}, {602, -1}, {603, -1}}, false},
		{"constant offset (skewed clock)", []row{{600, 300}, {601, 300}, {602, 300}, {603, 300}}, false},
		{"neighbour bins count", []row{{540, 120}, {601, 240}, {660, -1}, {719, -1}}, true},
		{"two bins away does not", []row{{540, 120}, {601, 240}, {660, -1}, {720, -1}}, false},
	}
	for _, c := range cases {
		l := ltTracker(4, 100)
		for _, r := range c.rows {
			e := sql.NullInt64{}
			if r.d >= 0 {
				e = nullInt(r.ts - r.d)
			}
			if _, err := l.see(1, r.ts, e); err != nil {
				t.Fatal(err)
			}
		}
		if got := l.flagged(nullInt(1), 601); got != c.want {
			t.Fatalf("%s: flagged %v, want %v", c.name, got, c.want)
		}
	}
	l := ltTracker(4, 100)
	for _, r := range []row{{600, 120}, {601, 240}, {602, -1}, {603, -1}} {
		e := sql.NullInt64{}
		if r.d >= 0 {
			e = nullInt(r.ts - r.d)
		}
		l.see(1, r.ts, e)
	}
	for _, c := range []struct {
		ts   int64
		want bool
	}{{539, false}, {540, true}, {719, true}, {720, false}} {
		if got := l.flagged(nullInt(1), c.ts); got != c.want {
			t.Fatalf("t=%d: flagged %v, want %v", c.ts, got, c.want)
		}
	}
	if l.flagged(nullInt(2), 600) || l.flagged(sql.NullInt64{}, 600) {
		t.Fatal("another or a NULL observer was flagged")
	}
}

// Bins are epoch-aligned also before 1970, and see reports already-seen rows.
func TestLateTrackerBinsAndSeen(t *testing.T) {
	for _, c := range []struct{ a, b, want int64 }{{-1, 60, -1}, {-60, 60, -1}, {-61, 60, -2}, {0, 60, 0}, {59, 60, 0}, {60, 60, 1}} {
		if got := floorDiv(c.a, c.b); got != c.want {
			t.Fatalf("floorDiv(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	l := ltTracker(1, 100)
	if seen, _ := l.see(1, 1000, nullInt(880)); !seen {
		t.Fatal("a delay of the gap is not already seen")
	}
	if seen, _ := l.see(1, 1000, nullInt(881)); seen {
		t.Fatal("a delay below the gap is already seen")
	}
	if seen, _ := l.see(1, 1000, sql.NullInt64{}); seen {
		t.Fatal("a row nobody else heard is already seen")
	}
}

// The tracker holds at most maxBins (observer, bin) entries; one more
// fails the export instead of growing.
func TestLateTrackerIsBounded(t *testing.T) {
	l := ltTracker(1, 2)
	for _, r := range []struct{ o, ts int64 }{{1, 0}, {1, 59}, {2, 0}, {2, 30}} {
		if _, err := l.see(r.o, r.ts, sql.NullInt64{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.see(1, 60, sql.NullInt64{}); err == nil {
		t.Fatal("a third entry was accepted")
	}
}

func ltArgs(db, out string, extra ...string) []string {
	return append(exportArgs(db, out, "-late-window", "1m", "-late-min-rows", "10", "-late-share", "0.5", "-late-gap", "2m"), extra...)
}

// The command takes the four options as explicit flags and prints the
// late-delivery counts in its summary.
func TestExportCommandLateDeliveryFlags(t *testing.T) {
	f := buildLateFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "events.jsonl")
	var stdout, stderr bytes.Buffer
	if code := run(ltArgs(f.path, out), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want, _ := runExportTo(t, ltOptions(f.path))
	if got, err := os.ReadFile(out); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("output differs from exportRecords (%v)", err)
	}
	if !regexp.MustCompile(`"late_delivery": \{\s*"rows": \d+,\s*"rows_without_observer": 1,\s*"rows_already_seen": \d+,\s*"events_flagged": 25\s*\}`).
		Match(stderr.Bytes()) {
		t.Fatalf("summary shape:\n%s", stderr.String())
	}
	for name, args := range map[string][]string{
		"window only": exportArgs(f.path, filepath.Join(dir, "a"), "-late-window", "1m"),
		"no share":    exportArgs(f.path, filepath.Join(dir, "b"), "-late-window", "1m", "-late-min-rows", "10", "-late-gap", "2m"),
		"share 2":     exportArgs(f.path, filepath.Join(dir, "c"), "-late-window", "1m", "-late-min-rows", "10", "-late-share", "2", "-late-gap", "2m"),
		"with #473":   ltArgs(f.path, filepath.Join(dir, "d"), "-backlog-silence", "10m", "-backlog-span", "30m"),
	} {
		var so, se bytes.Buffer
		if code := run(args, nil, &so, &se); code != 2 {
			t.Fatalf("%s: exit %d, want 2 (%s)", name, code, se.String())
		}
	}
}

// End to end: the unguarded rate rule fires on the backlog storm and the
// three real storms. Guarded by the late-delivery flag it loses only the
// backlog storm; guarded by the #473 flag it loses all four, because a
// quiet, a skewed or an isolated observer heard each real storm first.
func TestExportThenGuardedReplayIgnoresTheLateBacklog(t *testing.T) {
	f := buildLateFixture(t)
	f.tx(fxHash(9999), frameHex(1, 4, 1, nil, make([]byte, 100)), 4, fxObs{at: fxAt(-3 * time.Hour), observer: ltA,
		raw: frameHex(1, 4, 1, nil, make([]byte, 100))})
	late, _ := runExportTo(t, ltOptions(f.path))
	oldOpt := fxOptions(f.path)
	oldOpt.BacklogSilence, oldOpt.BacklogSpan = 10*time.Minute, 2*time.Hour
	old, _ := runExportTo(t, oldOpt)
	activations := func(in []byte, guard bool) []time.Duration {
		g := ""
		if guard {
			g = `, "exclude_ingest_backlog": true`
		}
		cfg, err := anomaly.ParseReplayConfig([]byte(`{
 "limits": {"max_keys_per_scope": 1000, "state_ttl": "48h", "dedup_horizon": "6h", "max_dedup_ids": 100000,
  "reorder_delay": "0s", "max_reorder_buffer": 64, "max_window_entries": 4096,
  "max_candidates_per_call": 1000, "max_pending_candidates": 10000},
 "expected": "include",
 "rate": [{"name": "grp", "scope": "global", "payload_types": [5], "window": "5m", "threshold": 20` + g + `,
  "state": {"confirm_after": "0s", "end_after_quiet": "5m", "cooldown": "0s"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		rep, err := anomaly.Replay(bytes.NewReader(in), cfg, anomaly.ReplayOptions{Start: fxT, End: fxAt(2 * time.Hour), MaxLineBytes: 4096})
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
	str := func(as []time.Duration) string { return fmt.Sprint(as) }
	backlog, quiet, skew, iso := ltMin(45, 19), ltMin(55, 19), ltMin(72, 44), ltMin(95, 19)
	if a := activations(late, false); str(a) != str([]time.Duration{backlog, quiet, skew, iso}) {
		t.Fatalf("unguarded activations %v", a)
	}
	if a := activations(old, false); str(a) != str([]time.Duration{backlog, quiet, skew, iso}) {
		t.Fatalf("unguarded activations on the #473 export %v", a)
	}
	if a := activations(late, true); str(a) != str([]time.Duration{quiet, skew, iso}) {
		t.Fatalf("late-delivery guarded activations %v, want the three real storms", a)
	}
	if a := activations(old, true); len(a) != 0 {
		t.Fatalf("#473 guarded activations %v, want none", a)
	}
}

// The checked-in E2E fixture (older schema, one observation per
// transmission) exports with the flag: nothing is already seen, nothing is
// flagged, and the output equals the plain export.
func TestExportLateDeliveryE2EFixture(t *testing.T) {
	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	if _, err := os.Stat(src); err != nil {
		t.Skip("e2e fixture not present")
	}
	before := fileSum(t, src)
	opt := exportOptions{DB: src, Start: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC), Settle: 30 * time.Second, MaxRows: 100000}
	plain, _ := runExportTo(t, opt)
	opt.LateWindow, opt.LateMinRows, opt.LateShare, opt.LateGap = ltWindow, ltMinRows, ltShare, ltGap
	got, sum := runExportTo(t, opt)
	if fileSum(t, src) != before {
		t.Fatal("e2e fixture changed")
	}
	if l := sum.LateDelivery; !bytes.Equal(got, plain) || l == nil || l.Rows < 400 || l.RowsAlreadySeen != 0 ||
		l.EventsFlagged != 0 || sum.EventsWritten < 400 {
		t.Fatalf("export differs, or summary %+v (%d events)", sum.LateDelivery, sum.EventsWritten)
	}
}
