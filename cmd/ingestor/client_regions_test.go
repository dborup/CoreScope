package main

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Declared-region reports on meshcore/client/{collector}/regions (#126). The
// protocol these tests pin is docs/client-regions.md.

const (
	testRegionsTarget    = "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddaa"
	testRegionsCollector = testCompanionPK
	testRegionsOther     = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
)

// regionsNow is the fixed ingest clock of the parse-level tests.
var regionsNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// regionsMsg is a valid report as a decoded MQTT payload, stamped one minute
// before at. Tests delete or overwrite single fields.
func regionsMsg(at time.Time) map[string]interface{} {
	return map[string]interface{}{
		"timestamp":      at.Add(-time.Minute).Format(time.RFC3339Nano),
		"target":         testRegionsTarget,
		"regions":        []interface{}{"*", "be", "be-vlg"},
		"truncated":      false,
		"repeater_clock": float64(1760443140),
		"gps":            map[string]interface{}{"lat": 51.2, "lon": 4.4, "acc_m": 8.0},
	}
}

// regionsMQTT wraps a payload on the regions topic of collector.
func regionsMQTT(t *testing.T, collector string, payload map[string]interface{}) *mockMessage {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &mockMessage{topic: "meshcore/client/" + collector + "/regions", payload: b}
}

func regionsOnCfg() *Config {
	return &Config{ClientRegions: &ClientRegionsConfig{Enabled: true}}
}

func parseRegions(t *testing.T, msg map[string]interface{}) (*ClientDeclaredRegions, string) {
	t.Helper()
	return parseClientRegionsReport(testRegionsCollector, msg, regionsNow, 30*24*time.Hour)
}

func mustParseRegions(t *testing.T, msg map[string]interface{}) *ClientDeclaredRegions {
	t.Helper()
	r, reason := parseRegions(t, msg)
	if r == nil {
		t.Fatalf("report rejected (%s), want accepted", reason)
	}
	return r
}

func declaredRegionsRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM client_declared_regions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type storedRegionsRow struct {
	collector, target, regions, reportedAt, ingestedAt, source string
	truncated                                                  int
	clock                                                      sql.NullInt64
	lat, lon, acc                                              sql.NullFloat64
}

func onlyRegionsRow(t *testing.T, s *Store) storedRegionsRow {
	t.Helper()
	var r storedRegionsRow
	if err := s.db.QueryRow(`SELECT collector_pubkey, target_pubkey, regions_csv, reported_at, ingested_at,
			mqtt_source, truncated, repeater_clock, lat, lon, pos_acc_m FROM client_declared_regions`).Scan(
		&r.collector, &r.target, &r.regions, &r.reportedAt, &r.ingestedAt, &r.source, &r.truncated,
		&r.clock, &r.lat, &r.lon, &r.acc); err != nil {
		t.Fatalf("read the single report: %v", err)
	}
	return r
}

// --- Gate -----------------------------------------------------------------

func TestClientRegionsEnabledDefault(t *testing.T) {
	if (&Config{}).ClientRegionsEnabled() {
		t.Fatal("absent clientRegions must be off")
	}
	if (&Config{ClientRegions: &ClientRegionsConfig{}}).ClientRegionsEnabled() {
		t.Fatal("enabled:false must be off")
	}
	if !regionsOnCfg().ClientRegionsEnabled() {
		t.Fatal("enabled:true must be on")
	}
	var nilCfg *Config
	if nilCfg.ClientRegionsEnabled() {
		t.Fatal("a nil config must be off")
	}
}

func TestClientRegionsGateOnWritesOneReport(t *testing.T) {
	s := newTestStore(t)
	handleMessage(s, "t", MQTTSource{Name: "trusted"}, regionsMQTT(t, testRegionsCollector, regionsMsg(time.Now())), nil, nil, regionsOnCfg())
	if n := declaredRegionsRows(t, s); n != 1 {
		t.Fatalf("gate on: want 1 report, got %d", n)
	}
	if got := onlyRegionsRow(t, s).source; got != "trusted" {
		t.Fatalf("mqtt_source = %q, want the delivering source name", got)
	}
}

// Gate off: nothing stored, and the message never falls through to the
// observer path (parts[1]="client" read as a region, the collector as an
// observer id).
func TestClientRegionsGateOffDropsWithoutFallThrough(t *testing.T) {
	s := newTestStore(t)
	payload := regionsMsg(time.Now())
	payload["raw"] = "11451000D818206D3AAC152C8A91F89957E6D30CA51F36E28790228971C473B755F244F718754CF5EE4A2FD58D944466E42CDED140C66D0CC590183E32BAF40F112BE8F3F2BDF6012B4B2793C52F1D36F69EE054D9A05593286F78453E56C0EC4A3EB95DDA2A7543FCCC00B939CACC009278603902FC12BCF84B706120526F6F6620536F6C6172"
	handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, testRegionsCollector, payload), nil, nil, &Config{})
	if n := declaredRegionsRows(t, s); n != 0 {
		t.Fatalf("gate off: want 0 reports, got %d", n)
	}
	var obs, tx int
	s.db.QueryRow(`SELECT COUNT(*) FROM observers`).Scan(&obs)
	s.db.QueryRow(`SELECT COUNT(*) FROM transmissions`).Scan(&tx)
	if obs != 0 || tx != 0 {
		t.Fatalf("gate off fell through to the observer path: observers=%d transmissions=%d", obs, tx)
	}
}

// clientRegions is its own gate: clientRxCoverage neither enables nor
// disables it.
func TestClientRegionsGateIndependentOfCoverage(t *testing.T) {
	s := newTestStore(t)
	coverageOnly := &Config{ClientRxCoverage: &ClientRxCoverageConfig{Enabled: true}}
	handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, testRegionsCollector, regionsMsg(time.Now())), nil, nil, coverageOnly)
	if n := declaredRegionsRows(t, s); n != 0 {
		t.Fatalf("clientRxCoverage alone must not accept reports, got %d", n)
	}
	handleMessage(s, "t", MQTTSource{Name: "t"}, clientCoverageMsg(), nil, nil, regionsOnCfg())
	if n := clientReceptionCount(t, s); n != 0 {
		t.Fatalf("clientRegions alone must not accept coverage, got %d", n)
	}
	handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, testRegionsCollector, regionsMsg(time.Now())), nil, nil, regionsOnCfg())
	if n := declaredRegionsRows(t, s); n != 1 {
		t.Fatalf("clientRegions alone must accept reports, got %d", n)
	}
}

// --- Topic identity, blacklist, source allowlist ---------------------------

func TestClientRegionsBlacklistedCollectorDropped(t *testing.T) {
	s := newTestStore(t)
	cfg := regionsOnCfg()
	cfg.ObserverBlacklist = []string{strings.ToUpper(testRegionsCollector)}
	handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, testRegionsCollector, regionsMsg(time.Now())), nil, nil, cfg)
	if n := declaredRegionsRows(t, s); n != 0 {
		t.Fatalf("blacklisted collector: want 0 reports, got %d", n)
	}
}

func TestClientRegionsSourceAllowlistApplies(t *testing.T) {
	s := newTestStore(t)
	cfg := regionsOnCfg()
	cfg.ClientRxCoverage = &ClientRxCoverageConfig{Sources: []string{"device-auth"}}
	handleMessage(s, "t", MQTTSource{Name: "legacy"}, regionsMQTT(t, testRegionsCollector, regionsMsg(time.Now())), nil, nil, cfg)
	if n := declaredRegionsRows(t, s); n != 0 {
		t.Fatalf("unlisted source: want 0 reports, got %d", n)
	}
	handleMessage(s, "t", MQTTSource{Name: "device-auth"}, regionsMQTT(t, testRegionsCollector, regionsMsg(time.Now())), nil, nil, cfg)
	if n := declaredRegionsRows(t, s); n != 1 {
		t.Fatalf("listed source: want 1 report, got %d", n)
	}
}

// Identity is the topic key, never a payload field.
func TestClientRegionsCollectorIsTopicKeyNotPayload(t *testing.T) {
	s := newTestStore(t)
	payload := regionsMsg(time.Now())
	payload["origin_id"] = testRegionsOther
	payload["collector"] = testRegionsOther
	handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, strings.ToUpper(testRegionsCollector), payload), nil, nil, regionsOnCfg())
	if got := onlyRegionsRow(t, s).collector; got != testRegionsCollector {
		t.Fatalf("collector = %q, want the lowercased topic key %q", got, testRegionsCollector)
	}
}

// The collector must be a full key: delete-client-rx erases by exact 64 hex,
// so a shorter key would store rows the erasure path cannot reach.
func TestClientRegionsCollectorMustBeFullKey(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{testRegionsCollector[:8], testRegionsCollector + "00", "zz" + testRegionsCollector[2:], "!@#$"} {
		handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, bad, regionsMsg(time.Now())), nil, nil, regionsOnCfg())
	}
	if n := declaredRegionsRows(t, s); n != 0 {
		t.Fatalf("non-64-hex collector keys must be dropped, got %d reports", n)
	}
}

func TestClientRegionsTopicMustHaveFourSegments(t *testing.T) {
	s := newTestStore(t)
	m := regionsMQTT(t, testRegionsCollector, regionsMsg(time.Now()))
	m.topic += "/extra"
	handleMessage(s, "t", MQTTSource{Name: "t"}, m, nil, nil, regionsOnCfg())
	if n := declaredRegionsRows(t, s); n != 0 {
		t.Fatalf("a five-segment topic must be dropped, got %d reports", n)
	}
}

// --- Target ----------------------------------------------------------------

func TestClientRegionsTargetValidation(t *testing.T) {
	for name, target := range map[string]interface{}{
		"missing": nil, "short prefix": testRegionsTarget[:12], "too long": testRegionsTarget + "aa",
		"non-hex": "gg" + testRegionsTarget[2:], "number": float64(12),
	} {
		msg := regionsMsg(regionsNow)
		if target == nil {
			delete(msg, "target")
		} else {
			msg["target"] = target
		}
		if r, _ := parseRegions(t, msg); r != nil {
			t.Errorf("%s target: accepted %q, want dropped", name, r.Target)
		}
	}
	msg := regionsMsg(regionsNow)
	msg["target"] = "  " + strings.ToUpper(testRegionsTarget) + " "
	if got := mustParseRegions(t, msg).Target; got != testRegionsTarget {
		t.Fatalf("target = %q, want trimmed lowercase %q", got, testRegionsTarget)
	}
}

// --- Region normalisation ----------------------------------------------------

func TestClientRegionsNormalisation(t *testing.T) {
	cases := []struct {
		name string
		in   []interface{}
		want []string
	}{
		{"hash added, wildcard kept", []interface{}{"*", "be", "#be-vlg"}, []string{"*", "#be", "#be-vlg"}},
		{"private region keeps $", []interface{}{"$ops", "be"}, []string{"$ops", "#be"}},
		{"NUL padding trimmed", []interface{}{"be", "belml\x00\x00\x00"}, []string{"#be", "#belml"}},
		{"padding-only entry skipped", []interface{}{"be", "\x00\x00"}, []string{"#be"}},
		{"whitespace trimmed", []interface{}{" be ", "\tnl\n"}, []string{"#be", "#nl"}},
		{"duplicates dropped, first order kept", []interface{}{"nl", "be", "#nl", "be"}, []string{"#nl", "#be"}},
		{"case preserved", []interface{}{"BE", "be"}, []string{"#BE", "#be"}},
		{"30-byte name accepted", []interface{}{strings.Repeat("a", 30)}, []string{"#" + strings.Repeat("a", 30)}},
	}
	for _, c := range cases {
		got, reason := normalizeDeclaredRegions(c.in)
		if reason != "" || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %q (%s), want %q", c.name, got, reason, c.want)
		}
	}
}

func TestClientRegionsNormalisationRejects(t *testing.T) {
	many := make([]interface{}, 87)
	for i := range many {
		many[i] = "r"
	}
	long := make([]interface{}, 0, 6)
	for i := 0; i < 6; i++ { // 6×29 + 5 commas = 179 > 172
		long = append(long, strings.Repeat(string(rune('a'+i)), 29))
	}
	for name, in := range map[string][]interface{}{
		"non-string entry":  {"be", float64(3)},
		"null entry":        {"be", nil},
		"comma in name":     {"be,nl"},
		"control character": {"be\x01"},
		"invalid UTF-8":     {"be\xff"},
		"31-byte name":      {strings.Repeat("a", 31)},
		"too many entries":  many,
		"list over 172 B":   long,
	} {
		if got, reason := normalizeDeclaredRegions(in); reason == "" {
			t.Errorf("%s: accepted %q, want rejected", name, got)
		}
	}
}

func TestClientRegionsMissingOrMalformedListDropped(t *testing.T) {
	for name, v := range map[string]interface{}{"missing": nil, "string": "be,nl", "object": map[string]interface{}{}, "null": "null"} {
		msg := regionsMsg(regionsNow)
		switch {
		case v == nil:
			delete(msg, "regions")
		case v == "null":
			msg["regions"] = nil
		default:
			msg["regions"] = v
		}
		if r, _ := parseRegions(t, msg); r != nil {
			t.Errorf("%s regions: accepted as %q, want dropped (never an empty answer)", name, r.Regions)
		}
	}
}

// An explicit empty list is an answer and is stored.
func TestClientRegionsExplicitEmptyListStored(t *testing.T) {
	s := newTestStore(t)
	for i, list := range [][]interface{}{{}, {"\x00\x00\x00"}} {
		msg := regionsMsg(time.Now())
		msg["regions"] = list
		msg["timestamp"] = time.Now().Add(-time.Duration(i+1) * time.Minute).UTC().Format(time.RFC3339Nano)
		handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, testRegionsCollector, msg), nil, nil, regionsOnCfg())
	}
	var n, empty int
	s.db.QueryRow(`SELECT COUNT(*), SUM(regions_csv = '') FROM client_declared_regions`).Scan(&n, &empty)
	if n != 2 || empty != 2 {
		t.Fatalf("empty answers: rows=%d empty=%d, want 2/2", n, empty)
	}
}

// --- Position and accuracy ---------------------------------------------------

func TestClientRegionsPositionValidation(t *testing.T) {
	type pos struct{ lat, lon, acc *float64 }
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		gps  interface{}
		want pos
	}{
		{"valid", map[string]interface{}{"lat": 51.2, "lon": 4.4, "acc_m": 8.0}, pos{f(51.2), f(4.4), f(8)}},
		{"no accuracy keeps position", map[string]interface{}{"lat": 51.2, "lon": 4.4}, pos{f(51.2), f(4.4), nil}},
		{"accuracy at bound", map[string]interface{}{"lat": 51.2, "lon": 4.4, "acc_m": 10000.0}, pos{f(51.2), f(4.4), f(10000)}},
		{"no gps", nil, pos{}},
		{"lat out of range", map[string]interface{}{"lat": 91.0, "lon": 4.4, "acc_m": 8.0}, pos{}},
		{"lon out of range", map[string]interface{}{"lat": 51.2, "lon": -180.5, "acc_m": 8.0}, pos{}},
		{"null island", map[string]interface{}{"lat": 0.0, "lon": 0.0, "acc_m": 8.0}, pos{}},
		{"lat as string", map[string]interface{}{"lat": "51.2", "lon": 4.4}, pos{}},
		{"lon missing", map[string]interface{}{"lat": 51.2}, pos{}},
		{"negative accuracy drops the fix", map[string]interface{}{"lat": 51.2, "lon": 4.4, "acc_m": -1.0}, pos{}},
		{"coarse accuracy drops the fix", map[string]interface{}{"lat": 51.2, "lon": 4.4, "acc_m": 10000.5}, pos{}},
		{"string accuracy drops the fix", map[string]interface{}{"lat": 51.2, "lon": 4.4, "acc_m": "8"}, pos{}},
		{"gps not an object", "51.2,4.4", pos{}},
	}
	eq := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	for _, c := range cases {
		msg := regionsMsg(regionsNow)
		if c.gps == nil {
			delete(msg, "gps")
		} else {
			msg["gps"] = c.gps
		}
		r := mustParseRegions(t, msg)
		if !eq(r.Lat, c.want.lat) || !eq(r.Lon, c.want.lon) || !eq(r.PosAccM, c.want.acc) {
			t.Errorf("%s: got lat=%v lon=%v acc=%v, want %v %v %v", c.name, r.Lat, r.Lon, r.PosAccM, c.want.lat, c.want.lon, c.want.acc)
		}
	}
}

// --- Three clocks ------------------------------------------------------------

func TestClientRegionsThreeClocksStayDistinct(t *testing.T) {
	msg := regionsMsg(regionsNow)
	msg["timestamp"] = "2026-10-14T13:55:30.120+02:00" // 11:55:30.120Z
	msg["repeater_clock"] = float64(1700000000)        // a repeater whose RTC is a year behind
	r := mustParseRegions(t, msg)
	if r.ReportedAt != "2026-10-14T11:55:30.120Z" {
		t.Errorf("reported_at = %q, want the payload timestamp in UTC ms", r.ReportedAt)
	}
	if r.IngestedAt != "2026-10-14T12:00:00.000Z" {
		t.Errorf("ingested_at = %q, want the ingest clock", r.IngestedAt)
	}
	if r.RepeaterClock == nil || *r.RepeaterClock != 1700000000 {
		t.Errorf("repeater_clock = %v, want 1700000000 as reported", r.RepeaterClock)
	}
}

func TestClientRegionsRepeaterClockValidation(t *testing.T) {
	for name, v := range map[string]interface{}{
		"missing": nil, "negative": float64(-1), "fractional": 1.5, "above uint32": float64(math.MaxUint32 + 1),
		"string": "1760443140", "bool": true,
	} {
		msg := regionsMsg(regionsNow)
		if v == nil {
			delete(msg, "repeater_clock")
		} else {
			msg["repeater_clock"] = v
		}
		if r := mustParseRegions(t, msg); r.RepeaterClock != nil {
			t.Errorf("%s repeater_clock: stored %d, want NULL (never 0, row kept)", name, *r.RepeaterClock)
		}
	}
	for _, v := range []float64{0, math.MaxUint32} {
		msg := regionsMsg(regionsNow)
		msg["repeater_clock"] = v
		if r := mustParseRegions(t, msg); r.RepeaterClock == nil || *r.RepeaterClock != int64(v) {
			t.Errorf("repeater_clock %v: got %v, want kept", v, r.RepeaterClock)
		}
	}
}

func TestClientRegionsReportedTimeRequired(t *testing.T) {
	for name, ts := range map[string]interface{}{
		"missing":          nil,
		"unparseable":      "yesterday",
		"zone-less":        "2026-10-14T11:59:00",
		"number":           float64(1760443140),
		"16 min in future": regionsNow.Add(16 * time.Minute).Format(time.RFC3339),
		"expired":          regionsNow.Add(-30*24*time.Hour - time.Second).Format(time.RFC3339),
	} {
		msg := regionsMsg(regionsNow)
		if ts == nil {
			delete(msg, "timestamp")
		} else {
			msg["timestamp"] = ts
		}
		if r, _ := parseRegions(t, msg); r != nil {
			t.Errorf("%s timestamp: accepted with reported_at=%q, want dropped (never replaced by ingest time)", name, r.ReportedAt)
		}
	}
	for name, ts := range map[string]string{
		"15 min in future":   regionsNow.Add(15 * time.Minute).Format(time.RFC3339),
		"just inside window": regionsNow.Add(-30*24*time.Hour + time.Second).Format(time.RFC3339),
		"explicit offset":    "2026-10-14T13:00:00+02:00",
		"second precision Z": "2026-10-14T11:00:00Z",
	} {
		msg := regionsMsg(regionsNow)
		msg["timestamp"] = ts
		if r, reason := parseRegions(t, msg); r == nil {
			t.Errorf("%s timestamp: dropped (%s), want accepted", name, reason)
		}
	}
}

// Expiry uses the configured retention window, not a fixed 30 days.
func TestClientRegionsExpiryFollowsRetentionWindow(t *testing.T) {
	s := newTestStore(t)
	cfg := regionsOnCfg()
	cfg.Retention = &RetentionConfig{ClientRegionsDays: 2}
	msg := regionsMsg(time.Now())
	msg["timestamp"] = time.Now().Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	handleMessage(s, "t", MQTTSource{Name: "t"}, regionsMQTT(t, testRegionsCollector, msg), nil, nil, cfg)
	if n := declaredRegionsRows(t, s); n != 0 {
		t.Fatalf("a report older than clientRegionsDays must be dropped, got %d", n)
	}
}

func TestClientRegionsTruncatedOnlyOnTrue(t *testing.T) {
	for v, want := range map[interface{}]bool{true: true, false: false, "true": false, float64(1): false} {
		msg := regionsMsg(regionsNow)
		msg["truncated"] = v
		if got := mustParseRegions(t, msg).Truncated; got != want {
			t.Errorf("truncated=%v: got %v, want %v", v, got, want)
		}
	}
}

// --- Storage: history, provenance, duplicates, out of order -------------------

func TestClientRegionsStoredRowRoundTrip(t *testing.T) {
	s := newTestStore(t)
	r := mustParseRegions(t, regionsMsg(regionsNow))
	r.Source = "src-a"
	if ins, err := s.InsertClientDeclaredRegions(r); err != nil || !ins {
		t.Fatalf("insert: ins=%v err=%v", ins, err)
	}
	got := onlyRegionsRow(t, s)
	if got.collector != testRegionsCollector || got.target != testRegionsTarget || got.regions != "*,#be,#be-vlg" ||
		got.reportedAt != "2026-10-14T11:59:00.000Z" || got.ingestedAt != "2026-10-14T12:00:00.000Z" ||
		got.source != "src-a" || got.truncated != 0 || !got.clock.Valid || got.clock.Int64 != 1760443140 ||
		!got.lat.Valid || got.lat.Float64 != 51.2 || !got.acc.Valid || got.acc.Float64 != 8 {
		t.Fatalf("stored row = %+v", got)
	}
}

func TestClientRegionsProvenanceKeepsCollectorsAndTimes(t *testing.T) {
	s := newTestStore(t)
	insert := func(collector, ts string, regions ...interface{}) bool {
		msg := regionsMsg(regionsNow)
		msg["timestamp"] = ts
		msg["regions"] = regions
		r, reason := parseClientRegionsReport(collector, msg, regionsNow, 30*24*time.Hour)
		if r == nil {
			t.Fatalf("rejected: %s", reason)
		}
		ins, err := s.InsertClientDeclaredRegions(r)
		if err != nil {
			t.Fatal(err)
		}
		return ins
	}
	const t1, t2 = "2026-10-14T10:00:00Z", "2026-10-14T11:00:00Z"
	insert(testRegionsCollector, t1, "be")
	insert(testRegionsOther, t1, "nl") // a second collector disagrees at the same moment
	insert(testRegionsCollector, t2, "be", "nl")
	if insert(testRegionsCollector, t2, "be", "nl") {
		t.Fatal("an exact redelivery must be a no-op")
	}
	if n := declaredRegionsRows(t, s); n != 3 {
		t.Fatalf("want 3 reports (2 collectors, 2 times, 1 duplicate), got %d", n)
	}
	var collectors int
	s.db.QueryRow(`SELECT COUNT(DISTINCT collector_pubkey) FROM client_declared_regions WHERE target_pubkey=?`, testRegionsTarget).Scan(&collectors)
	if collectors != 2 {
		t.Fatalf("distinct collectors = %d, want 2", collectors)
	}
}

// A buffered report that arrives late is history, not the latest answer.
func TestClientRegionsOutOfOrderKeepsLatestByReportedTime(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []struct{ ts, region string }{{"2026-10-14T11:00:00Z", "new"}, {"2026-10-12T11:00:00Z", "old"}} {
		msg := regionsMsg(regionsNow)
		msg["timestamp"], msg["regions"] = c.ts, []interface{}{c.region}
		if _, err := s.InsertClientDeclaredRegions(mustParseRegions(t, msg)); err != nil {
			t.Fatal(err)
		}
	}
	// The read path's order (cmd/server declared_regions.go): reported_at,
	// never id or ingested_at.
	var latest string
	if err := s.db.QueryRow(`SELECT regions_csv FROM client_declared_regions WHERE target_pubkey = ?
		ORDER BY reported_at DESC LIMIT 1`, testRegionsTarget).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if latest != "#new" {
		t.Fatalf("latest by reported_at = %q, want #new: the late report must keep its own reported_at", latest)
	}
	if n := declaredRegionsRows(t, s); n != 2 {
		t.Fatalf("the late report must still be kept as history, got %d rows", n)
	}
}

// --- Schema: legacy DB, indexes ------------------------------------------------

func TestClientRegionsSchemaOnLegacyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE client_observers (pubkey TEXT PRIMARY KEY, name TEXT, last_seen TEXT)`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore on a legacy DB: %v", err)
	}
	defer s.Close()
	for _, name := range []string{"client_declared_regions", "idx_cdr_reported", "idx_cdr_collector"} {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name).Scan(&n)
		if n != 1 {
			t.Errorf("%s missing after OpenStore on a legacy DB", name)
		}
	}
}

// --- Retention -----------------------------------------------------------------

func TestClientRegionsDaysFallbacks(t *testing.T) {
	cases := []struct {
		name string
		r    *RetentionConfig
		want int
	}{
		{"unset", nil, 30},
		{"zero", &RetentionConfig{}, 30},
		{"follows clientRxDays", &RetentionConfig{ClientRxDays: 7}, 7},
		{"own window wins", &RetentionConfig{ClientRxDays: 7, ClientRegionsDays: 3}, 3},
		{"negative is unset", &RetentionConfig{ClientRegionsDays: -1}, 30},
	}
	for _, c := range cases {
		if got := (&Config{Retention: c.r}).ClientRegionsDays(); got != c.want {
			t.Errorf("%s: ClientRegionsDays() = %d, want %d", c.name, got, c.want)
		}
	}
}

func seedRegionsReport(t *testing.T, s *Store, collector, reportedAt string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO client_declared_regions
		(collector_pubkey, target_pubkey, regions_csv, reported_at, ingested_at, mqtt_source)
		VALUES (?, ?, '#be', ?, '2026-10-14T12:00:00.000Z', 't')`, collector, testRegionsTarget, reportedAt); err != nil {
		t.Fatalf("seed report: %v", err)
	}
}

func TestClientRegionsPruneByReportedTime(t *testing.T) {
	s := newTestStore(t)
	orig := retentionBatchRows
	retentionBatchRows = 2
	t.Cleanup(func() { retentionBatchRows = orig })
	for i := 0; i < 5; i++ {
		seedRegionsReport(t, s, testRegionsCollector, time.Date(2026, 9, 1, 0, 0, i, 0, time.UTC).Format(clientRegionsTimeLayout))
	}
	seedRegionsReport(t, s, testRegionsCollector, "2026-10-13T23:59:59.999Z") // just inside a 1-day window
	n, err := s.pruneClientDeclaredRegionsBefore(regionsNow.AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || declaredRegionsRows(t, s) != 1 {
		t.Fatalf("pruned %d, left %d; want 5 pruned across batches, 1 kept", n, declaredRegionsRows(t, s))
	}
}

func TestClientRegionsPruneUsesIndex(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+clientRegionsPruneSQL, "2026-10-01T00:00:00.000Z", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		plan = append(plan, detail)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "idx_cdr_reported") {
		t.Fatalf("retention must seek idx_cdr_reported, plan:\n%s", strings.Join(plan, "\n"))
	}
}

// config.example.json documents the gate, off, and the retention key.
func TestConfigExampleDocumentsClientRegions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ClientRegions == nil || cfg.ClientRegions.Enabled {
		t.Fatalf("config.example.json must carry clientRegions with enabled:false, got %+v", cfg.ClientRegions)
	}
	text := string(data)
	for _, want := range []string{`"_comment_clientRegions"`, "clientRegionsDays", "docs/client-regions.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("config.example.json does not mention %s", want)
		}
	}
}
