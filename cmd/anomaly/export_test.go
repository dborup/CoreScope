package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/anomaly"
)

// Issue #457, phase 2: a read-only, bounded, key-free export of
// anomaly.ReplayRecord lines from a CoreScope database.

var fxT = time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)

func fxAt(d time.Duration) time.Time { return fxT.Add(d) }

const fxSettle = 10 * time.Second

// buildExportFixture fills the scenario used by most export tests. Each
// transmission exercises one rule of the export; see the expected records
// in TestExportSyntheticFixture.
func buildExportFixture(t *testing.T, wal bool) *fixtureDB {
	f := newFixtureDB(t, wal)
	grp := grpPayload(0x0a, 20) // 23 payload bytes
	// 0..19: a 5-minute "bot" on channel 0a. Three observers: the earliest
	// heard it via relays 0b,01; one heard it with no path; one via 0c.
	for i := 0; i < 20; i++ {
		at := fxAt(10*time.Minute + time.Duration(i)*5*time.Minute)
		f.tx(fxHash(i), frameHex(1, 5, 1, []byte{0x0b, 0x01}, grp), 5,
			relay(at, 1, 5, 1, []byte{0x0b, 0x01}, grp),
			relay(at.Add(time.Second), 1, 5, 1, nil, grp),
			relay(at.Add(3*time.Second), 1, 5, 1, []byte{0x0c}, grp))
	}
	// 100: relay evidence 30 s after the first observation is past the
	// settle window: excluded, so the event stays no-path.
	f.tx(fxHash(100), frameHex(1, 5, 1, nil, grp), 5,
		relay(fxAt(time.Minute), 1, 5, 1, nil, grp),
		relay(fxAt(time.Minute+30*time.Second), 1, 5, 1, []byte{0xee}, grp))
	// 101: evidence exactly at the settle deadline is included.
	f.tx(fxHash(101), frameHex(1, 5, 1, nil, grp), 5,
		relay(fxAt(2*time.Minute), 1, 5, 1, nil, grp),
		relay(fxAt(2*time.Minute+fxSettle), 1, 5, 1, []byte{0xdd}, grp))
	// 102: first heard 3 s before Start: not exported, and its later
	// observation must not make an in-range event.
	f.tx(fxHash(102), frameHex(1, 5, 1, nil, grp), 5,
		relay(fxAt(-3*time.Second), 1, 5, 1, nil, grp),
		relay(fxAt(2*time.Second), 1, 5, 1, []byte{0xcb}, grp))
	// 103: first heard a minute before Start (before the lead-in): its
	// in-range observation is past the settle window and must be dropped.
	f.tx(fxHash(103), frameHex(1, 5, 1, nil, grp), 5,
		relay(fxAt(-time.Minute), 1, 5, 1, nil, grp),
		relay(fxAt(5*time.Second), 1, 5, 1, []byte{0xcc}, grp))
	// 104: transport flood with 2-byte hops on channel 0b.
	g2 := grpPayload(0x0b, 20)
	f.tx(fxHash(104), frameHex(0, 5, 2, []byte{0xfd, 0x12, 0x00, 0x01}, g2), 5,
		relay(fxAt(3*time.Minute), 0, 5, 2, []byte{0xfd, 0x12, 0x00, 0x01}, g2))
	// 105: a direct TXT_MSG: no channel, no first hop.
	txt := make([]byte, 20)
	f.tx(fxHash(105), frameHex(2, 2, 1, []byte{0x77}, txt), 2,
		relay(fxAt(4*time.Minute), 2, 2, 1, []byte{0x77}, txt))
	// 106: TRACE: header path bytes are SNR values, path_json holds payload
	// hops; neither is route evidence.
	trace := make([]byte, 12)
	f.tx(fxHash(106), frameHex(1, 9, 1, []byte{0x10, 0x20}, trace), 9,
		fxObs{at: fxAt(5 * time.Minute), raw: frameHex(1, 9, 1, []byte{0x10, 0x20}, trace), path: `["AB","CD"]`})
	// 113: a TRACE row without its own raw_hex: path_json holds payload
	// hops and the transmission frame's SNR bytes belong to another
	// observation, so the frame size is unknown.
	f.tx(fxHash(113), frameHex(1, 9, 1, []byte{0x10, 0x20}, trace), 9,
		fxObs{at: fxAt(5*time.Minute + 30*time.Second), path: `["AB","CD","EF"]`})
	// 107: a row from before observations.raw_hex (#881): the path comes
	// from path_json, the header and payload from the transmission frame.
	g3 := grpPayload(0x0c, 20)
	f.tx(fxHash(107), frameHex(1, 5, 1, []byte{0x01}, g3), 5,
		fxObs{at: fxAt(6 * time.Minute), path: `["AA","BB"]`})
	// 108: no raw_hex and no path_json: the route is unknown.
	f.tx(fxHash(108), frameHex(1, 5, 1, []byte{0x01}, g3), 5, fxObs{at: fxAt(7 * time.Minute)})
	// 109: an undecodable frame.
	f.tx(fxHash(109), "ZZ", 5, fxObs{at: fxAt(8 * time.Minute), path: `[]`})
	// 110: an ADVERT with 3-byte hops.
	adv := make([]byte, 100)
	f.tx(fxHash(110), frameHex(1, 4, 3, []byte{0xaa, 0xbb, 0xcc, 0x11, 0x22, 0x33}, adv), 4,
		relay(fxAt(9*time.Minute), 1, 4, 3, []byte{0xaa, 0xbb, 0xcc, 0x11, 0x22, 0x33}, adv))
	// 111 / 112: the last second before End is in, End itself is out.
	f.tx(fxHash(111), frameHex(1, 5, 1, nil, grp), 5, relay(fxAt(2*time.Hour-time.Second), 1, 5, 1, nil, grp))
	f.tx(fxHash(112), frameHex(1, 5, 1, nil, grp), 5, relay(fxAt(2*time.Hour), 1, 5, 1, nil, grp))
	return f
}

func fxOptions(db string) exportOptions {
	return exportOptions{DB: db, Start: fxT, End: fxAt(2 * time.Hour), Settle: fxSettle, MaxRows: 1000}
}

func runExportTo(t *testing.T, opt exportOptions) ([]byte, exportSummary) {
	t.Helper()
	var buf bytes.Buffer
	sum, err := exportRecords(opt, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), sum
}

func rec(id string, at time.Time, pt int, ch, rc, rk, hop, route string, wire int, payload *int) anomaly.ReplayRecord {
	return anomaly.ReplayRecord{ID: id, Time: at, FinalAt: at.Add(fxSettle), PayloadType: pt, Channel: ch,
		RouteClass: rc, RouteKind: rk, FirstHop: hop, FullRoute: route, WireBytes: wire, PayloadBytes: payload,
		Traffic: "unclassified", Decryption: "unknown"}
}

func intp(v int) *int { return &v }

func TestExportSyntheticFixture(t *testing.T) {
	f := buildExportFixture(t, false)
	got, sum := runExportTo(t, fxOptions(f.path))

	var want []anomaly.ReplayRecord
	p23 := intp(23)
	want = append(want,
		rec(fxHash(100), fxAt(time.Minute), 5, "0a", "flood", "nopath", "", "", 25, p23),
		rec(fxHash(101), fxAt(2*time.Minute), 5, "0a", "flood", "relay", "1:dd", "1:dd", 26, p23),
		rec(fxHash(104), fxAt(3*time.Minute), 5, "0b", "tflood", "relay", "2:fd12", "2:fd120001", 33, p23),
		rec(fxHash(105), fxAt(4*time.Minute), 2, "", "direct", "direct", "", "", 23, intp(20)),
		rec(fxHash(106), fxAt(5*time.Minute), 9, "", "flood", "unknown", "", "", 16, intp(12)),
		rec(fxHash(113), fxAt(5*time.Minute+30*time.Second), 9, "", "flood", "unknown", "", "", 0, intp(12)),
		rec(fxHash(107), fxAt(6*time.Minute), 5, "0c", "flood", "relay", "1:aa", "1:aabb", 27, p23),
		rec(fxHash(108), fxAt(7*time.Minute), 5, "0c", "unknown", "unknown", "", "", 0, p23),
		rec(fxHash(110), fxAt(9*time.Minute), 4, "", "flood", "relay", "3:aabbcc", "3:aabbcc112233", 108, intp(100)))
	for i := 0; i < 20; i++ {
		at := fxAt(10*time.Minute + time.Duration(i)*5*time.Minute)
		want = append(want, rec(fxHash(i), at, 5, "0a", "flood", "relay", "1:0b", "1:0b01", 27, p23))
	}
	want = append(want, rec(fxHash(111), fxAt(2*time.Hour-time.Second), 5, "0a", "flood", "nopath", "", "", 25, p23))
	var wb bytes.Buffer
	for _, r := range want {
		b, _ := json.Marshal(r)
		wb.Write(append(b, '\n'))
	}
	if !bytes.Equal(got, wb.Bytes()) {
		gl, wl := strings.Split(string(got), "\n"), strings.Split(wb.String(), "\n")
		for i := range wl {
			if i >= len(gl) || gl[i] != wl[i] {
				g := "<missing>"
				if i < len(gl) {
					g = gl[i]
				}
				t.Fatalf("line %d:\n got %s\nwant %s", i+1, g, wl[i])
			}
		}
		t.Fatalf("extra output lines:\n%s", strings.Join(gl[len(wl):], "\n"))
	}

	wantSum := exportSummary{Rows: 75, FrameFromTransmission: 4, BadFrame: 1, PathUnknown: 1,
		EventsWritten: 30, EventsOutOfRange: 2}
	gotSum := sum
	gotSum.Normalizer = anomaly.NormalizerStats{}
	if gotSum != wantSum {
		t.Fatalf("summary %+v, want %+v", gotSum, wantSum)
	}
	ns := sum.Normalizer
	if ns.Accepted != 74 || ns.Late != 0 || ns.Conflicts != 0 || ns.Invalid != 0 || ns.OutOfArrivalOrder != 0 ||
		ns.EventsFinalized != 32 || ns.CoverageReduced() || sum.CoverageReduced {
		t.Fatalf("normalizer stats %+v", ns)
	}
}

// The output carries only on-wire, key-free fields: none of the decrypted
// text, channel names, public keys, observer names/IDs or resolved paths
// stored next to the frames, and no field outside ReplayRecord.
func TestExportOutputIsKeyFree(t *testing.T) {
	f := buildExportFixture(t, false)
	got, _ := runExportTo(t, fxOptions(f.path))
	for _, s := range []string{sentinelText, sentinelChannel, sentinelPubkey, sentinelObserver, sentinelObsID,
		sentinelResolved, "SENTINEL", "-7.5", "-101", `"rx"`} {
		if bytes.Contains(got, []byte(s)) {
			t.Fatalf("export output contains %q", s)
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(got))
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		var r anomaly.ReplayRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %s: %v", sc.Text(), err)
		}
	}
}

// The query names only the frame, its path, its time and its content hash.
func TestExportQueryReadsOnlyOnWireColumns(t *testing.T) {
	for _, own := range []bool{true, false} {
		q := strings.ToLower(exportQuery(own))
		for _, col := range []string{"decoded_json", "from_pubkey", "channel_hash", "observer", "snr", "rssi",
			"score", "direction", "resolved_path", "name", "first_seen", "*"} {
			if strings.Contains(q, col) {
				t.Fatalf("export query reads %q:\n%s", col, q)
			}
		}
	}
}

// Parity with the replay contract: every exported line passes the same
// validation Replay applies (strict decode, ReplayRecord.Event), and a
// replay of the export accepts every line.
func TestExportParityWithReplayRecordValidation(t *testing.T) {
	f := buildExportFixture(t, false)
	got, sum := runExportTo(t, fxOptions(f.path))
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(got))
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		var r anomaly.ReplayRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Event(); err != nil {
			t.Fatalf("line %s: %v", sc.Text(), err)
		}
		n++
	}
	cfg, err := anomaly.ParseReplayConfig([]byte(parityConfig))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := anomaly.Replay(bytes.NewReader(got), cfg, anomaly.ReplayOptions{Start: fxT, End: fxAt(2 * time.Hour), MaxLineBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	in := rep.Input
	if n != sum.EventsWritten || in.Lines != n || in.Accepted != n || in.Invalid+in.Late+in.Duplicates+in.OutOfRange != 0 {
		t.Fatalf("lines %d, written %d, replay input %+v", n, sum.EventsWritten, in)
	}
}

// parityConfig: one global rate rule, only so Replay has a valid config.
const parityConfig = `{
 "limits": {"max_keys_per_scope": 1000, "state_ttl": "48h", "dedup_horizon": "6h", "max_dedup_ids": 100000,
  "reorder_delay": "0s", "max_reorder_buffer": 64, "max_window_entries": 4096,
  "max_candidates_per_call": 1000, "max_pending_candidates": 10000},
 "expected": "include",
 "rate": [{"name": "parity", "scope": "global", "window": "5m", "threshold": 1000,
  "state": {"confirm_after": "0s", "end_after_quiet": "10m", "cooldown": "0s"}}]
}`

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestExportOpensTheDatabaseReadOnly(t *testing.T) {
	f := buildExportFixture(t, false)
	db, err := openReadOnly(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{`CREATE TABLE x (y INTEGER)`, `DELETE FROM observations`, `PRAGMA user_version = 7`} {
		if _, err := db.Exec(stmt); err == nil {
			t.Fatalf("%s succeeded on the export connection", stmt)
		}
	}
	for _, bad := range []string{"", "a?mode=rw", "a#b"} {
		if _, err := openReadOnly(bad); err == nil {
			t.Fatalf("path %q accepted", bad)
		}
	}
}

// The export changes no byte of the database, in rollback-journal mode and
// in WAL mode with a writer connection open (as with a running ingestor).
func TestExportLeavesTheDatabaseUnchanged(t *testing.T) {
	for _, wal := range []bool{false, true} {
		f := buildExportFixture(t, wal)
		if !wal {
			f.db.Close()
		}
		files := []string{f.path}
		if wal {
			files = append(files, f.path+"-wal")
		}
		before := make([][32]byte, len(files))
		for i, p := range files {
			before[i] = fileSum(t, p)
		}
		runExportTo(t, fxOptions(f.path))
		for i, p := range files {
			if fileSum(t, p) != before[i] {
				t.Fatalf("wal=%v: %s changed", wal, filepath.Base(p))
			}
		}
		if !wal {
			for _, sfx := range []string{"-journal", "-wal", "-shm"} {
				if _, err := os.Stat(f.path + sfx); err == nil {
					t.Fatalf("export created %s", sfx)
				}
			}
		}
	}
}

// Cost is bounded by the rows in the time range: the range is an index
// search on observations.timestamp in output order (no scan, no sort), the
// transmission is a primary-key lookup and the settle check an index search
// per row.
func TestExportQueryPlanIsBounded(t *testing.T) {
	f := buildExportFixture(t, false)
	db, err := openReadOnly(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+exportQuery(true), 0, 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	all := strings.Join(plan, "\n")
	for _, want := range []string{
		"SEARCH o USING INDEX idx_observations_timestamp (timestamp>? AND timestamp<?)",
		"SEARCH t USING INTEGER PRIMARY KEY (rowid=?)",
		"SEARCH p USING COVERING INDEX idx_observations_tx_ts (transmission_id=? AND timestamp<?)",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("plan lacks %q:\n%s", want, all)
		}
	}
	for _, bad := range []string{"SCAN", "TEMP B-TREE"} {
		if strings.Contains(all, bad) {
			t.Fatalf("plan has %q:\n%s", bad, all)
		}
	}
}

func TestExportMaxRowsIsAHardCap(t *testing.T) {
	f := buildExportFixture(t, false)
	opt := fxOptions(f.path)
	opt.MaxRows = 75 // exactly the rows in range
	runExportTo(t, opt)
	opt.MaxRows = 74
	if _, err := exportRecords(opt, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "max-rows") {
		t.Fatalf("75 rows allowed past a cap of 74: %v", err)
	}
}

func exportArgs(db, out string, extra ...string) []string {
	args := []string{"export", "-db", db, "-start", "2030-03-01T00:00:00Z", "-end", "2030-03-01T02:00:00Z",
		"-settle", "10s", "-max-rows", "1000", "-out", out}
	return append(args, extra...)
}

func TestExportCommandWritesTheFileAndASummary(t *testing.T) {
	f := buildExportFixture(t, false)
	out := filepath.Join(t.TempDir(), "events.jsonl")
	var stdout, stderr bytes.Buffer
	if code := run(exportArgs(f.path, out), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want, _ := runExportTo(t, fxOptions(f.path))
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("output file differs from exportRecords (%v)", err)
	}
	var sum exportSummary
	if err := json.Unmarshal(stderr.Bytes(), &sum); err != nil || sum.EventsWritten != 30 {
		t.Fatalf("summary %q: %v", stderr.String(), err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout not empty: %q", stdout.String())
	}
	// an existing output is never overwritten
	if code := run(exportArgs(f.path, out), nil, &stdout, &stderr); code == 0 {
		t.Fatal("existing output overwritten")
	}
}

func TestExportCommandLeavesNoFileOnFailure(t *testing.T) {
	f := buildExportFixture(t, false)
	dir := t.TempDir()
	out := filepath.Join(dir, "events.jsonl")
	args := exportArgs(f.path, out)
	args[len(args)-3] = "74" // -max-rows
	var stdout, stderr bytes.Buffer
	if code := run(args, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("files left after a failed export: %v", ents)
	}
}

func TestExportCommandRequiresExplicitBounds(t *testing.T) {
	f := buildExportFixture(t, false)
	out := filepath.Join(t.TempDir(), "events.jsonl")
	full := exportArgs(f.path, out)
	// drop each flag in turn (flag name and value)
	for i := 1; i < len(full); i += 2 {
		args := append(append([]string{}, full[:i]...), full[i+2:]...)
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != 2 {
			t.Fatalf("without %s: exit %d, want 2", full[i], code)
		}
	}
	bad := map[string][]string{
		"end before start":   {"-end", "2030-02-28T00:00:00Z"},
		"empty range":        {"-end", "2030-03-01T00:00:00Z"},
		"range over 31 days": {"-end", "2030-04-01T00:00:01Z"},
		"settle zero":        {"-settle", "0s"},
		"settle over 1h":     {"-settle", "61m"},
		"max-rows zero":      {"-max-rows", "0"},
		"max-rows too large": {"-max-rows", "100000001"},
		"out is the db":      {"-out", f.path},
		"out is the wal":     {"-out", f.path + "-wal"},
		"positional args":    {"extra"},
		"bad time":           {"-start", "yesterday"},
	}
	for name, extra := range bad {
		var stdout, stderr bytes.Buffer
		if code := run(append(append([]string{}, full...), extra...), nil, &stdout, &stderr); code != 2 {
			t.Fatalf("%s: exit %d, want 2 (%s)", name, code, stderr.String())
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("output written by a rejected command")
	}
}

// The checked-in E2E fixture (a staging capture with the older schema: no
// observations.raw_hex) exports without invalid lines and is not modified.
func TestExportE2EFixture(t *testing.T) {
	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	if _, err := os.Stat(src); err != nil {
		t.Skip("e2e fixture not present")
	}
	before := fileSum(t, src)
	opt := exportOptions{DB: src, Start: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC), Settle: 30 * time.Second, MaxRows: 100000}
	got, sum := runExportTo(t, opt)
	if fileSum(t, src) != before {
		t.Fatal("e2e fixture changed")
	}
	if sum.EventsWritten < 100 || sum.Rows == 0 || sum.Normalizer.Late != 0 || sum.Normalizer.Conflicts != 0 {
		t.Fatalf("summary %+v", sum)
	}
	cfg, err := anomaly.ParseReplayConfig([]byte(parityConfig))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := anomaly.Replay(bytes.NewReader(got), cfg, anomaly.ReplayOptions{Start: opt.Start, End: opt.End, MaxLineBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if in := rep.Input; in.Accepted != sum.EventsWritten || in.Invalid+in.Late+in.Duplicates+in.OutOfRange != 0 {
		t.Fatalf("replay input %+v, written %d", in, sum.EventsWritten)
	}
	for _, s := range []string{"#wardriving", "#chat", "enc_"} {
		if bytes.Contains(got, []byte(s)) {
			t.Fatalf("output contains channel name %q", s)
		}
	}
}
