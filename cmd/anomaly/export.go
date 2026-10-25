package main

import (
	"bufio"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/meshcore-analyzer/anomaly"
	"github.com/meshcore-analyzer/packetpath"
	_ "modernc.org/sqlite"
)

// Export: CoreScope SQLite -> anomaly.ReplayRecord JSON Lines.
//
// Read-only: the database is opened with SQLite's mode=ro (for a static
// snapshot copy also immutable=1) and only SELECT statements run. Key-free: the
// query reads the frame (raw_hex), the observation's path (path_json), its
// time and the row IDs that join it to its transmission; never the content
// hash, decoded_json, channel names, public keys, observer columns,
// SNR/RSSI or resolved paths (except observer_idx for the opt-in backlog
// and late-delivery flags, which only a boolean leaves). A line's id is the export's own
// number (see txNumbering). Bounded: one index range scan on observations.timestamp
// over [start - settle, end + settle) with a hard -max-rows cap; per row a
// primary-key lookup and one index search; memory is bounded by the caps
// below (Normalizer and ID numbering).
//
// Events are built by anomaly.Normalizer (its reference observation and
// finality policy): an observation is evidence when its time is at most
// -settle after the transmission's first observation. The same rule is
// applied in SQL against every observation of the transmission, also those
// before the range, so a transmission first heard before start never
// becomes an in-range event. Observation time is observations.timestamp
// (whole seconds): the observer's receive time from the MQTT envelope, or
// the ingest time where the ingestor falls back to it (resolveRxTime).
//
// With -backlog-silence and -backlog-span the export also flags events
// whose first observation follows a silence of its observer (backlog.go);
// with the -late-* flags instead, events that an observer delivered in a
// burst of transmissions others heard minutes earlier (late.go).

const (
	maxExportSpan   = 31 * 24 * time.Hour
	maxExportSettle = time.Hour
	maxExportRows   = 100_000_000
	// Normalizer capacity for the export (not detection thresholds).
	// Pending transmissions are those first heard within the last settle
	// window; a forced finalization or fan-out drop is counted in the
	// summary and marks the events truncated. exportMaxPending also caps
	// the transmissions the ID numbering holds; exceeding it fails.
	exportMaxPending  = 1 << 17
	exportMaxObsPerTx = 1024
)

type exportOptions struct {
	DB         string
	Start, End time.Time
	Settle     time.Duration
	MaxRows    int
	// Immutable opens DB as a static snapshot copy (see openImmutable).
	Immutable bool
	// BacklogSilence and BacklogSpan enable the ingest-backlog flag
	// (backlog.go); both or neither, 0 is off.
	BacklogSilence, BacklogSpan time.Duration
	// LateWindow, LateMinRows, LateShare and LateGap enable the
	// late-delivery flag (late.go); all or none, 0 is off.
	LateWindow, LateGap time.Duration
	LateMinRows         int
	LateShare           float64
}

func (o exportOptions) validate() error {
	switch {
	case o.DB == "":
		return errors.New("-db is required")
	case o.Start.IsZero() || o.End.IsZero():
		return errors.New("-start and -end are required")
	case !o.End.After(o.Start):
		return errors.New("-end must be after -start")
	case o.End.Sub(o.Start) > maxExportSpan:
		return errors.New("-end - -start must be at most 31 days")
	case o.Start.Nanosecond() != 0 || o.End.Nanosecond() != 0:
		return errors.New("-start and -end must be whole seconds")
	case o.Settle <= 0 || o.Settle > maxExportSettle || o.Settle%time.Second != 0:
		return errors.New("-settle must be whole seconds in (0, 1h]")
	case o.MaxRows <= 0 || o.MaxRows > maxExportRows:
		return fmt.Errorf("-max-rows must be in 1..%d", maxExportRows)
	}
	if err := o.validateBacklog(); err != nil {
		return err
	}
	return o.validateLate()
}

// exportSummary counts rows and events; it holds no values from the data.
type exportSummary struct {
	// Rows is the observation rows read (range plus settle margins, after
	// the settle filter).
	Rows int `json:"rows"`
	// FrameFromTransmission counts rows without observations.raw_hex
	// (written before #881): header and payload come from the
	// transmission's frame, the path from path_json.
	FrameFromTransmission int `json:"frame_from_transmission"`
	// BadFrame counts rows whose frame does not decode; they are dropped.
	BadFrame int `json:"bad_frame"`
	// PathUnknown counts rows with neither raw_hex nor a usable path_json:
	// their route class is unknown, so they never form a route group.
	PathUnknown      int                     `json:"path_unknown"`
	EventsWritten    int                     `json:"events_written"`
	EventsOutOfRange int                     `json:"events_out_of_range"`
	InvalidEvents    int                     `json:"invalid_events"`
	CoverageReduced  bool                    `json:"coverage_reduced"`
	Normalizer       anomaly.NormalizerStats `json:"normalizer"`
	// IngestBacklog is set only when the backlog flag is enabled.
	IngestBacklog *backlogSummary `json:"ingest_backlog,omitempty"`
	// LateDelivery is set only when the late-delivery flag is enabled.
	LateDelivery *lateSummary `json:"late_delivery,omitempty"`
}

// openReadOnly opens path with SQLite's mode=ro: the connection cannot
// write, and a missing file is an error instead of a new database. On a WAL
// database SQLite still creates the -wal and -shm files if they are missing
// (and fails if the directory is read-only), and the export's single query
// holds one read snapshot for its whole run, so a live writer cannot
// checkpoint past it meanwhile. For a static copy use openImmutable.
func openReadOnly(path string) (*sql.DB, error) {
	return openSQLite(path, "mode=ro&_pragma=busy_timeout(5000)")
}

// openImmutable opens a static snapshot copy with mode=ro and immutable=1:
// SQLite takes no locks and creates no side files. It then assumes nothing
// changes the file and ignores a -wal or hot -journal file beside it, whose
// contents would be lost; a copy with either is refused.
func openImmutable(path string) (*sql.DB, error) {
	for _, sfx := range []string{"-wal", "-journal"} {
		if _, err := os.Lstat(path + sfx); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s%s exists; an immutable snapshot must be one self-contained file", path, sfx)
		}
	}
	return openSQLite(path, "mode=ro&immutable=1")
}

func openSQLite(path, params string) (*sql.DB, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, errors.New("database path must be non-empty and contain no '?' or '#'")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+params)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// exportQuery selects the observations in [?1, ?2) whose time is at most
// ?3 seconds after the first observation of their transmission, in arrival
// order, at most ?4 rows. ownRaw selects observations.raw_hex, which older
// databases lack; observer adds observations.observer_idx for the backlog
// or late-delivery flag.
func exportQuery(ownRaw, observer bool) string {
	raw, obs := "NULL", ""
	if ownRaw {
		raw = "o.raw_hex"
	}
	if observer {
		obs = ", o.observer_idx"
	}
	return `SELECT o.id, o.timestamp, o.path_json, ` + raw + `, o.transmission_id, t.raw_hex` + obs + `
FROM observations o JOIN transmissions t ON t.id = o.transmission_id
WHERE o.timestamp >= ?1 AND o.timestamp < ?2
  AND NOT EXISTS (SELECT 1 FROM observations p
                  WHERE p.transmission_id = o.transmission_id AND p.timestamp < o.timestamp - ?3)
ORDER BY o.timestamp, o.id
LIMIT ?4`
}

func hasColumn(db *sql.DB, table, col string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n)
	return n > 0, err
}

// exportRecords writes the events with data time in [Start, End) to w.
func exportRecords(opt exportOptions, w io.Writer) (exportSummary, error) {
	var sum exportSummary
	if err := opt.validate(); err != nil {
		return sum, err
	}
	open := openReadOnly
	if opt.Immutable {
		open = openImmutable
	}
	db, err := open(opt.DB)
	if err != nil {
		return sum, err
	}
	defer db.Close()
	ownRaw, err := hasColumn(db, "observations", "raw_hex")
	if err != nil {
		return sum, err
	}
	norm, err := anomaly.NewNormalizer(anomaly.NormalizerConfig{SettleWindow: opt.Settle,
		MaxPendingTransmissions: exportMaxPending, MaxObservationsPerTransmission: exportMaxObsPerTx,
		FinalizedHorizon: opt.Settle, MaxFinalizedIDs: exportMaxPending})
	if err != nil {
		return sum, err
	}
	// flag sets Observation.IngestBacklog; flaggedEvents counts the events
	var flag interface {
		flagged(observer sql.NullInt64, ts int64) bool
	}
	var flaggedEvents *int
	switch {
	case opt.backlog():
		sum.IngestBacklog = &backlogSummary{}
		b, err := scanBacklog(db, opt, sum.IngestBacklog)
		if err != nil {
			return sum, err
		}
		flag, flaggedEvents = b, &sum.IngestBacklog.EventsFlagged
	case opt.late():
		sum.LateDelivery = &lateSummary{}
		l, err := scanLate(db, opt, sum.LateDelivery)
		if err != nil {
			return sum, err
		}
		flag, flaggedEvents = l, &sum.LateDelivery.EventsFlagged
	}
	ids := newTxNumbering(int64(opt.Settle/time.Second), exportMaxPending)
	rows, err := db.Query(exportQuery(ownRaw, flag != nil), opt.Start.Add(-opt.Settle).Unix(), opt.End.Add(opt.Settle).Unix(),
		int64(opt.Settle/time.Second), opt.MaxRows+1)
	if err != nil {
		return sum, err
	}
	defer rows.Close()

	bw := bufio.NewWriter(w)
	emit := func(evs []anomaly.Event) error {
		for _, e := range evs {
			if e.Time.Before(opt.Start) || !e.Time.Before(opt.End) {
				sum.EventsOutOfRange++
				continue
			}
			r := anomaly.RecordFromEvent(e)
			// parity with Replay: only lines it would accept are written
			if _, err := r.Event(); err != nil {
				sum.InvalidEvents++
				continue
			}
			b, err := json.Marshal(r)
			if err != nil {
				return err
			}
			if _, err := bw.Write(append(b, '\n')); err != nil {
				return err
			}
			sum.EventsWritten++
			if e.IngestBacklog {
				*flaggedEvents++
			}
		}
		return nil
	}
	for rows.Next() {
		if sum.Rows++; sum.Rows > opt.MaxRows {
			return sum, fmt.Errorf("more than -max-rows %d observation rows in range; output incomplete", opt.MaxRows)
		}
		var r obsRow
		dest := []interface{}{&r.id, &r.ts, &r.pathJSON, &r.ownRaw, &r.tx, &r.txRaw}
		if flag != nil {
			dest = append(dest, &r.observer)
		}
		if err := rows.Scan(dest...); err != nil {
			return sum, err
		}
		id, err := ids.id(r.tx, r.ts)
		if err != nil {
			return sum, err
		}
		o, note, err := observationFrom(r, id)
		if note.fromTx {
			sum.FrameFromTransmission++
		}
		if err != nil {
			sum.BadFrame++
			continue
		}
		if note.pathUnknown {
			sum.PathUnknown++
		}
		if flag != nil {
			o.IngestBacklog = flag.flagged(r.observer, r.ts)
		}
		if err := emit(norm.Add(o).Events); err != nil {
			return sum, err
		}
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}
	if err := emit(norm.Flush().Events); err != nil {
		return sum, err
	}
	sum.Normalizer = norm.Stats()
	sum.CoverageReduced = sum.Normalizer.CoverageReduced()
	return sum, bw.Flush()
}

type obsRow struct {
	id, ts, tx       int64
	pathJSON, ownRaw sql.NullString
	txRaw            string
	observer         sql.NullInt64 // read only for the backlog or late-delivery flag
}

// txNumbering gives each transmission an id of the export's own: "tx" and
// a counter in the order its first row is read. The id is not derived from
// the content hash, a frame or a row ID, so the id does not join a line to a
// CoreScope database or instance; it is only unique within the export. The
// line itself still can: its time, payload type, sizes and route can
// single out one transmission, so the files stay with the coordinator.
// Rows arrive in time order and the export query admits a row only within
// settle of its transmission's first observation, so a transmission is
// forgotten once its first row read is more than settle old. At most max
// transmissions are held; one more fails the export.
type txNumbering struct {
	settle int64
	max    int
	next   int
	ids    map[int64]anomaly.TxID // transmission row ID -> id
	order  []txFirst              // held transmissions, by first row time
}

type txFirst struct{ tx, ts int64 }

func newTxNumbering(settle int64, max int) *txNumbering {
	return &txNumbering{settle: settle, max: max, ids: make(map[int64]anomaly.TxID)}
}

// id returns the id of transmission row tx for a row at ts (Unix seconds,
// non-decreasing across calls).
func (n *txNumbering) id(tx, ts int64) (anomaly.TxID, error) {
	for len(n.order) > 0 && n.order[0].ts < ts-n.settle {
		delete(n.ids, n.order[0].tx)
		n.order = n.order[1:]
	}
	if id, ok := n.ids[tx]; ok {
		return id, nil
	}
	if len(n.ids) >= n.max {
		return "", fmt.Errorf("more than %d transmissions first heard within one -settle window", n.max)
	}
	n.next++
	id := anomaly.TxID("tx" + strconv.Itoa(n.next))
	n.ids[tx] = id
	n.order = append(n.order, txFirst{tx, ts})
	return id, nil
}

func (n *txNumbering) live() int { return len(n.ids) }

type rowNote struct{ fromTx, pathUnknown bool }

// routeClasses maps the header route type (firmware Packet.h) to the
// library's route class.
var routeClasses = [4]anomaly.RouteClass{
	packetpath.RouteTransportFlood:  anomaly.RouteClassTransportFlood,
	packetpath.RouteFlood:           anomaly.RouteClassFlood,
	packetpath.RouteDirect:          anomaly.RouteClassDirect,
	packetpath.RouteTransportDirect: anomaly.RouteClassTransportDirect,
}

// observationFrom builds a key-free Observation from on-wire facts only:
// header (route and payload type), path, payload size and, for group
// payloads, the 1-byte channel hash (firmware docs/payloads.md).
func observationFrom(r obsRow, id anomaly.TxID) (anomaly.Observation, rowNote, error) {
	note := rowNote{fromTx: !r.ownRaw.Valid || r.ownRaw.String == ""}
	raw := r.ownRaw.String
	if note.fromTx {
		raw = r.txRaw
	}
	fr, err := parseFrame(raw)
	if err != nil {
		return anomaly.Observation{}, note, err
	}
	o := anomaly.Observation{TxID: id, ObsID: strconv.FormatInt(r.id, 10),
		Time: time.Unix(r.ts, 0).UTC(), PayloadType: anomaly.PayloadType(fr.payloadType),
		RouteClass: routeClasses[fr.route], PayloadBytes: len(fr.payload), PayloadKnown: true}
	if fr.payloadType == payloadGrpTxt || fr.payloadType == payloadGrpData {
		if o.Channel, err = anomaly.NewChannelHash(fr.payload[:1]); err != nil {
			return anomaly.Observation{}, note, err
		}
	}
	// pathBytes sizes the frame; -1 means the frame size is unknown
	pathBytes := len(fr.path)
	switch {
	case !packetpath.PathBytesAreHops(fr.payloadType):
		// TRACE: the header path holds SNR values, not route evidence
		if note.fromTx {
			pathBytes = -1
		}
	case !note.fromTx:
		if o.Path, err = anomaly.NewRoute(routeWidth(fr), fr.path); err != nil {
			return anomaly.Observation{}, note, err
		}
	default:
		width, hops, ok := parsePathJSON(r.pathJSON)
		if !ok {
			note.pathUnknown = true
			o.RouteClass = anomaly.RouteClassUnknown
			pathBytes = -1
			break
		}
		if o.Path, err = anomaly.NewRoute(width, hops); err != nil {
			return anomaly.Observation{}, note, err
		}
		pathBytes = len(hops)
	}
	if pathBytes >= 0 {
		o.WireBytes = fr.headerBytes() + pathBytes + len(fr.payload)
	}
	return o, note, nil
}

const (
	payloadGrpTxt  = 0x05
	payloadGrpData = 0x06
)

// frame is a decoded MeshCore frame (firmware Packet::readFrom).
type frame struct {
	route         int
	payloadType   byte
	hashSize      int
	path, payload []byte
}

func (f frame) headerBytes() int {
	if packetpath.IsTransportRoute(f.route) {
		return 6 // header, 4 transport code bytes, path length
	}
	return 2
}

func routeWidth(f frame) int {
	if len(f.path) == 0 {
		return 0
	}
	return f.hashSize
}

var errBadFrame = errors.New("frame does not decode")

// parseFrame decodes a frame with the rules of firmware Packet::readFrom:
// path hash size 4 is reserved, the path holds at most MAX_PATH_SIZE bytes
// and the payload is non-empty and at most MAX_PACKET_PAYLOAD bytes.
func parseFrame(rawHex string) (frame, error) {
	buf, err := hex.DecodeString(rawHex)
	if err != nil || len(buf) < 2 {
		return frame{}, errBadFrame
	}
	f := frame{route: packetpath.RouteTypeFromHeader(buf[0]), payloadType: buf[0] >> 2 & 0x0f}
	i := f.headerBytes() - 1
	if i >= len(buf) {
		return frame{}, errBadFrame
	}
	pl := buf[i]
	i++
	f.hashSize = int(pl>>6) + 1
	n := int(pl&63) * f.hashSize
	if f.hashSize == 4 || n > anomaly.MaxPathBytes || i+n >= len(buf) {
		return frame{}, errBadFrame
	}
	f.path, f.payload = buf[i:i+n], buf[i+n:]
	if len(f.payload) > anomaly.MaxPayloadBytes {
		return frame{}, errBadFrame
	}
	return f, nil
}

// parsePathJSON reads observations.path_json (a JSON array of hop hex
// strings of one width, as the ingestor writes it). ok is false when the
// column is NULL/empty or not of that shape.
func parsePathJSON(s sql.NullString) (width int, hops []byte, ok bool) {
	if !s.Valid || s.String == "" {
		return 0, nil, false
	}
	var parts []string
	if json.Unmarshal([]byte(s.String), &parts) != nil {
		return 0, nil, false
	}
	for _, p := range parts {
		b, err := hex.DecodeString(p)
		if err != nil || len(b) < 1 || len(b) > anomaly.MaxHopWidth || (width != 0 && len(b) != width) {
			return 0, nil, false
		}
		width = len(b)
		hops = append(hops, b...)
	}
	if len(hops) > anomaly.MaxPathBytes {
		return 0, nil, false
	}
	return width, hops, true
}

// runExport parses flags, writes the export to -out (a new file, written
// under a temporary name and linked into place only on success) and prints
// the summary as JSON on stderr.
func runExport(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "CoreScope SQLite database, opened read-only (required)")
	immutable := fs.Bool("immutable", false, "-db is a static snapshot copy: open it with immutable=1 (no locks, no side files)")
	start := fs.String("start", "", "RFC 3339 start, inclusive, whole seconds (required)")
	end := fs.String("end", "", "RFC 3339 end, exclusive, at most 31 days after -start (required)")
	settle := fs.Duration("settle", 0, "route evidence window after a transmission's first observation, (0, 1h] (required)")
	maxRows := fs.Int("max-rows", 0, "hard cap on observation rows read; exceeding it fails the export (required)")
	out := fs.String("out", "", "output JSON Lines file; must not exist (required)")
	silence := fs.Duration("backlog-silence", 0, "with -backlog-span: an observation with no observation of its observer in this long before it is a reconnect (optional, no default)")
	spn := fs.Duration("backlog-span", 0, "with -backlog-silence: flag events first heard by an observer within this long after its reconnect (optional, no default)")
	lateWindow := fs.Duration("late-window", 0, "with the other -late-* flags: bin width of an observer's late burst (optional, no default)")
	lateMinRows := fs.Int("late-min-rows", 0, "with the other -late-* flags: rows a late burst holds at least (optional, no default)")
	lateShare := fs.Float64("late-share", 0, "with the other -late-* flags: share of a late burst's rows that are already seen, (0, 1] (optional, no default)")
	lateGap := fs.Duration("late-gap", 0, "with the other -late-* flags: a row is already seen when another observer heard its transmission this long before (optional, no default)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(err error) int {
		fmt.Fprintf(stderr, "export: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("unexpected arguments"))
	}
	opt := exportOptions{DB: *dbPath, Settle: *settle, MaxRows: *maxRows, Immutable: *immutable,
		BacklogSilence: *silence, BacklogSpan: *spn,
		LateWindow: *lateWindow, LateMinRows: *lateMinRows, LateShare: *lateShare, LateGap: *lateGap}
	var err error
	if opt.Start, err = parseTime("start", *start); err != nil {
		return usageErr(err)
	}
	if opt.End, err = parseTime("end", *end); err != nil {
		return usageErr(err)
	}
	if err := opt.validate(); err != nil {
		return usageErr(err)
	}
	if *out == "" {
		return usageErr(errors.New("-out is required"))
	}
	if sameFileFamily(*out, *dbPath) {
		return usageErr(errors.New("-out must not be the database or one of its -wal/-shm/-journal files"))
	}
	if _, err := os.Lstat(*out); err == nil {
		return usageErr(fmt.Errorf("%s exists; refusing to overwrite", *out))
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "export: %v\n", err)
		return exitFail
	}
	tmp := *out + ".partial"
	f, err := createNew(tmp)
	if err != nil {
		return fail(err)
	}
	sum, err := exportRecords(opt, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// Link refuses an existing target, so nothing is overwritten
		err = os.Link(tmp, *out)
	}
	os.Remove(tmp)
	if err != nil {
		return fail(err)
	}
	b, _ := json.MarshalIndent(sum, "", " ")
	fmt.Fprintf(stderr, "%s\n", b)
	return exitOK
}

// sameFileFamily reports whether out names db or one of SQLite's side files.
func sameFileFamily(out, db string) bool {
	ao, err1 := filepath.Abs(out)
	ad, err2 := filepath.Abs(db)
	if err1 != nil || err2 != nil {
		return true
	}
	for _, sfx := range []string{"", "-wal", "-shm", "-journal"} {
		if ao == ad+sfx {
			return true
		}
	}
	return false
}
