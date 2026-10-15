package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
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
	// 114: a GRP_DATA (0x06): the channel hash is its first payload byte
	// too (firmware docs/payloads.md).
	gd := grpPayload(0x0d, 20)
	f.tx(fxHash(114), frameHex(1, 6, 1, []byte{0x0e}, gd), 6,
		relay(fxAt(8*time.Minute+30*time.Second), 1, 6, 1, []byte{0x0e}, gd))
	// 110: an ADVERT with 3-byte hops.
	adv := make([]byte, 100)
	f.tx(fxHash(110), frameHex(1, 4, 3, []byte{0xaa, 0xbb, 0xcc, 0x11, 0x22, 0x33}, adv), 4,
		relay(fxAt(9*time.Minute), 1, 4, 3, []byte{0xaa, 0xbb, 0xcc, 0x11, 0x22, 0x33}, adv))
	// 115: observations.raw_hex is '' rather than NULL: like 107, the frame
	// comes from the transmission and the path from path_json.
	f.tx(fxHash(115), frameHex(1, 5, 1, []byte{0x01}, g3), 5,
		fxObs{at: fxAt(9*time.Minute + 30*time.Second), emptyRaw: true, path: `["AA","BB"]`})
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

	// IDs number the transmissions in the order their first row is read
	// (tx1 is 102 in the lead-in, tx10 the bad frame 109, tx35 is 112 in
	// the lead-out).
	var want []anomaly.ReplayRecord
	p23 := intp(23)
	want = append(want,
		rec("tx2", fxAt(time.Minute), 5, "0a", "flood", "nopath", "", "", 25, p23),
		rec("tx3", fxAt(2*time.Minute), 5, "0a", "flood", "relay", "1:dd", "1:dd", 26, p23),
		rec("tx4", fxAt(3*time.Minute), 5, "0b", "tflood", "relay", "2:fd12", "2:fd120001", 33, p23),
		rec("tx5", fxAt(4*time.Minute), 2, "", "direct", "direct", "", "", 23, intp(20)),
		rec("tx6", fxAt(5*time.Minute), 9, "", "flood", "unknown", "", "", 16, intp(12)),
		rec("tx7", fxAt(5*time.Minute+30*time.Second), 9, "", "flood", "unknown", "", "", 0, intp(12)),
		rec("tx8", fxAt(6*time.Minute), 5, "0c", "flood", "relay", "1:aa", "1:aabb", 27, p23),
		rec("tx9", fxAt(7*time.Minute), 5, "0c", "unknown", "unknown", "", "", 0, p23),
		rec("tx11", fxAt(8*time.Minute+30*time.Second), 6, "0d", "flood", "relay", "1:0e", "1:0e", 26, p23),
		rec("tx12", fxAt(9*time.Minute), 4, "", "flood", "relay", "3:aabbcc", "3:aabbcc112233", 108, intp(100)),
		rec("tx13", fxAt(9*time.Minute+30*time.Second), 5, "0c", "flood", "relay", "1:aa", "1:aabb", 27, p23))
	for i := 0; i < 20; i++ {
		at := fxAt(10*time.Minute + time.Duration(i)*5*time.Minute)
		want = append(want, rec(fmt.Sprintf("tx%d", 14+i), at, 5, "0a", "flood", "relay", "1:0b", "1:0b01", 27, p23))
	}
	want = append(want, rec("tx34", fxAt(2*time.Hour-time.Second), 5, "0a", "flood", "nopath", "", "", 25, p23))
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

	wantSum := exportSummary{Rows: 77, FrameFromTransmission: 5, BadFrame: 1, PathUnknown: 1,
		EventsWritten: 32, EventsOutOfRange: 2}
	gotSum := sum
	gotSum.Normalizer = anomaly.NormalizerStats{}
	if gotSum != wantSum {
		t.Fatalf("summary %+v, want %+v", gotSum, wantSum)
	}
	ns := sum.Normalizer
	if ns.Accepted != 76 || ns.Late != 0 || ns.Conflicts != 0 || ns.Invalid != 0 || ns.OutOfArrivalOrder != 0 ||
		ns.EventsFinalized != 34 || ns.CoverageReduced() || sum.CoverageReduced {
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

// The query names only the frame, its path, its time and the row IDs that
// join an observation to its transmission; not even the content hash.
func TestExportQueryReadsOnlyOnWireColumns(t *testing.T) {
	for _, own := range []bool{true, false} {
		q := strings.ToLower(exportQuery(own, false))
		for _, col := range []string{"decoded_json", "from_pubkey", "channel_hash", "observer", "snr", "rssi",
			"score", "direction", "resolved_path", "name", "first_seen", "hash", "*"} {
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

// Both ways of opening the database (live file and -immutable snapshot)
// refuse writes, and a missing file is an error that creates no file.
func TestExportOpensTheDatabaseReadOnly(t *testing.T) {
	f := buildExportFixture(t, false)
	for name, open := range map[string]func(string) (*sql.DB, error){
		"openReadOnly": openReadOnly, "openImmutable": openImmutable,
	} {
		t.Run(name, func(t *testing.T) {
			db, err := open(f.path)
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
				if _, err := open(bad); err == nil {
					t.Fatalf("path %q accepted", bad)
				}
			}
			missing := filepath.Join(t.TempDir(), "typo.db")
			if db, err := open(missing); err == nil {
				db.Close()
				t.Fatal("a missing database file was opened")
			}
			for _, sfx := range []string{"", "-wal", "-shm", "-journal"} {
				if _, err := os.Lstat(missing + sfx); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("opening a missing database left %s%s (%v)", missing, sfx, err)
				}
			}
		})
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
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+exportQuery(true, false), 0, 1, 1, 1)
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
	opt.MaxRows = 77 // exactly the rows in range
	runExportTo(t, opt)
	opt.MaxRows = 76
	if _, err := exportRecords(opt, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "max-rows") {
		t.Fatalf("77 rows allowed past a cap of 76: %v", err)
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
	if err := json.Unmarshal(stderr.Bytes(), &sum); err != nil || sum.EventsWritten != 32 {
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
	args[len(args)-3] = "76" // -max-rows
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
		"out is the shm":     {"-out", f.path + "-shm"},
		"out is the journal": {"-out", f.path + "-journal"},
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

// Review F1: an exported id is the export's own number. It is not the
// content hash, which joins a line to every CoreScope instance that heard
// the packet (and on a database not rehashed by #215 is raw_hex[:16], for an
// ADVERT 6 bytes of the advertiser's public key), and no part of a frame.
func TestExportIDsAreOpaque(t *testing.T) {
	f := buildExportFixture(t, false)
	got, _ := runExportTo(t, fxOptions(f.path))
	assertOpaqueIDs(t, got, f.path)

	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	if _, err := os.Stat(src); err != nil {
		t.Skip("e2e fixture not present")
	}
	got, _ = runExportTo(t, exportOptions{DB: src, Start: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC), Settle: 30 * time.Second, MaxRows: 100000})
	assertOpaqueIDs(t, got, src)
}

// assertOpaqueIDs checks that every line's id is a fresh per-export number
// and that no id equals a transmission hash or occurs in any stored frame,
// and that no hash or 6-byte public key prefix occurs anywhere in out.
func assertOpaqueIDs(t *testing.T, out []byte, dbPath string) {
	t.Helper()
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	strs := func(q string) []string {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var vs []string
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v.Valid && v.String != "" {
				vs = append(vs, strings.ToLower(v.String))
			}
		}
		return vs
	}
	hashes := strs(`SELECT hash FROM transmissions`)
	frames := strs(`SELECT raw_hex FROM transmissions`)
	if own, _ := hasColumn(db, "observations", "raw_hex"); own {
		frames = append(frames, strs(`SELECT raw_hex FROM observations`)...)
	}
	var keys []string
	if ok, _ := hasColumn(db, "nodes", "public_key"); ok {
		for _, k := range strs(`SELECT public_key FROM nodes`) {
			if len(k) >= 12 {
				keys = append(keys, k[:12])
			}
		}
	}
	if len(hashes) == 0 || len(frames) == 0 {
		t.Fatal("no hashes or frames to check against")
	}
	lower := bytes.ToLower(out)
	for _, s := range append(append([]string{}, hashes...), keys...) {
		if bytes.Contains(lower, []byte(s)) {
			t.Fatalf("output contains the stored hash or key prefix %q", s)
		}
	}
	idRe := regexp.MustCompile(`^tx[1-9][0-9]*$`)
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var r anomaly.ReplayRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		id := strings.ToLower(r.ID)
		if !idRe.MatchString(r.ID) || seen[id] {
			t.Fatalf("id %q is not a fresh per-export number", r.ID)
		}
		seen[id] = true
		for _, h := range hashes {
			if id == h {
				t.Fatalf("id %q is a transmission hash", r.ID)
			}
		}
		for _, fr := range frames {
			if strings.Contains(fr, id) {
				t.Fatalf("id %q occurs in a stored frame", r.ID)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no lines exported")
	}
}

// The numbering keeps a transmission's id for every row up to its settle
// deadline (the export query admits no later row), forgets it after, and
// holds at most max transmissions at a time.
func TestTxNumberingIsPerExportAndBounded(t *testing.T) {
	n := newTxNumbering(10, 3)
	must := func(tx, ts int64) anomaly.TxID {
		t.Helper()
		id, err := n.id(tx, ts)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if a, b := must(7, 100), must(9, 105); a != "tx1" || b != "tx2" {
		t.Fatalf("ids %q %q", a, b)
	}
	// row id 7 again exactly at its settle deadline: the same transmission
	if id := must(7, 110); id != "tx1" {
		t.Fatalf("same transmission at its deadline got %q", id)
	}
	if id := must(5, 111); id != "tx3" || n.live() != 2 {
		t.Fatalf("id %q, %d live; 7 must be forgotten after its deadline", id, n.live())
	}
	if id := must(9, 115); id != "tx2" {
		t.Fatalf("same transmission got %q", id)
	}
	must(6, 115)
	if _, err := n.id(8, 115); err == nil {
		t.Fatal("a fourth live transmission was accepted with max 3")
	}
	// a refused row takes no number, and numbers are never reused
	if id := must(4, 200); id != "tx5" {
		t.Fatalf("id %q after eviction", id)
	}
}

// Review F5: mode=ro is not free of side effects on a WAL database: on a
// cleanly closed WAL file SQLite creates -wal and -shm beside it, and a
// live database's read snapshot is held for the whole export. Immutable
// reads a static snapshot copy with no locks and no side files, also from
// a read-only directory, and refuses a copy that has a -wal or -journal
// file beside it (immutable mode would ignore its contents).
func TestExportImmutableSnapshotHasNoSideEffects(t *testing.T) {
	f := buildExportFixture(t, true)
	f.db.Close() // a clean close checkpoints and removes -wal and -shm
	side := func() []string {
		var s []string
		for _, sfx := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Stat(f.path + sfx); err == nil {
				s = append(s, sfx)
			}
		}
		return s
	}
	if s := side(); len(s) != 0 {
		t.Fatalf("snapshot has side files %v before the export", s)
	}
	want, _ := runExportTo(t, fxOptions(f.path))
	// the behaviour the README documents for mode=ro on a WAL file
	if s := side(); len(s) != 2 {
		t.Fatalf("mode=ro on a closed WAL database left %v; the README says -wal and -shm", s)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if err := os.Remove(f.path + sfx); err != nil {
			t.Fatal(err)
		}
	}
	before := fileSum(t, f.path)
	dir := filepath.Dir(f.path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	opt := fxOptions(f.path)
	opt.Immutable = true
	got, _ := runExportTo(t, opt)
	if !bytes.Equal(got, want) {
		t.Fatal("immutable export differs from the mode=ro export")
	}
	if s := side(); len(s) != 0 {
		t.Fatalf("immutable export created %v", s)
	}
	if fileSum(t, f.path) != before {
		t.Fatal("immutable export changed the database")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sfx := range []string{"-wal", "-journal"} {
		if err := os.WriteFile(f.path+sfx, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := exportRecords(opt, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), sfx) {
			t.Fatalf("immutable export beside a %s file: %v", sfx, err)
		}
		os.Remove(f.path + sfx)
	}

	// the command line: -immutable is opt-in and gives the same lines
	out := filepath.Join(t.TempDir(), "events.jsonl")
	var stdout, stderr bytes.Buffer
	if code := run(exportArgs(f.path, out, "-immutable"), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if b, err := os.ReadFile(out); err != nil || !bytes.Equal(b, want) {
		t.Fatalf("-immutable output differs (%v)", err)
	}
	if s := side(); len(s) != 0 {
		t.Fatalf("-immutable command created %v", s)
	}
}

// The README's snapshot recipe is one VACUUM INTO statement on a read-only
// connection: a single read transaction, so it finishes while a writer
// commits continuously (the backup API's .backup restarts on every write and
// may never finish), and its copy is one self-contained file for -immutable.
func TestReadmeSnapshotRecipeFinishesUnderAWriter(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^sqlite3 .*\.backup`).Match(readme) {
		t.Fatal("README still recommends .backup, which may never finish on a live database")
	}
	recipe := regexp.MustCompile(`(?m)^sqlite3 -readonly \S+ "VACUUM INTO '[^']+'"`).FindAll(readme, -1)
	if len(recipe) != 1 {
		t.Fatalf("README has %d `sqlite3 -readonly <db> \"VACUUM INTO '<snap>'\"` lines, want 1", len(recipe))
	}

	f := buildExportFixture(t, true) // the writer connection stays open, frames stay in the WAL
	if st, err := os.Stat(f.path + "-wal"); err != nil || st.Size() == 0 {
		t.Fatalf("fixture has no uncheckpointed WAL frames (%v)", err)
	}
	want, _ := runExportTo(t, fxOptions(f.path))

	if _, err := f.db.Exec(`CREATE TABLE ingest (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := f.db.Exec(`INSERT INTO ingest (n) VALUES (1)`); err == nil {
				commits.Add(1)
			}
		}
	}()
	for commits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	snap := filepath.Join(t.TempDir(), "meshcore.db")
	src, err := openReadOnly(f.path) // sqlite3 -readonly
	if err != nil {
		t.Fatal(err)
	}
	before := commits.Load()
	finished := make(chan error, 1)
	go func() { _, err := src.Exec(`VACUUM INTO ?`, snap); finished <- err }()
	select {
	case err = <-finished:
	case <-time.After(60 * time.Second):
		err = errors.New("VACUUM INTO did not finish within 60s under a writer")
	}
	during := commits.Load() - before
	close(stop)
	<-done
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("writer commits while the snapshot ran: %d", during)

	for _, sfx := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(snap + sfx); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot has a %s file", sfx)
		}
	}
	opt := fxOptions(snap)
	opt.Immutable = true
	got, _ := runExportTo(t, opt)
	if !bytes.Equal(got, want) {
		t.Fatal("-immutable export of the VACUUM INTO snapshot differs from the export of the live file")
	}
	db, err := openImmutable(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check: %q (%v)", check, err)
	}
}
