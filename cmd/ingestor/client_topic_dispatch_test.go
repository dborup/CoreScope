package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/packetpath"
)

// Regression tests for the meshcore/client/... dispatch: a client-namespace
// topic must never reach the ordinary observer path, whatever the
// clientRxCoverage gate says. On the observer path parts[1] ("client") would be
// read as a region and the companion pubkey as an observer id, registering the
// phone as an observer and ingesting its packets as observer traffic.

// clientTopicPayload is a client-shaped payload that the observer path WOULD
// ingest if it ever got there: a decodable relayed advert (raw) plus GPS.
const clientTopicPayload = `{"raw":"11451000D818206D3AAC152C8A91F89957E6D30CA51F36E28790228971C473B755F244F718754CF5EE4A2FD58D944466E42CDED140C66D0CC590183E32BAF40F112BE8F3F2BDF6012B4B2793C52F1D36F69EE054D9A05593286F78453E56C0EC4A3EB95DDA2A7543FCCC00B939CACC009278603902FC12BCF84B706120526F6F6620536F6C6172","direction":"rx","origin":"MyMob","SNR":-7.0,"RSSI":-92.0,"gps":{"lat":51.05,"lon":3.72,"acc_m":8.0}}`

// Same two path bytes ("1a2b", hash_size 2, hash_count 1) on a FLOOD route;
// only the payload type differs. For TRACE the header path bytes are per-hop
// SNR values, not node hashes, so they must never become a heard_key.
const (
	floodGrpTxtHopRaw = "15" + "41" + "1a2b" + "3333333333333333" + "3333333333333333" // header 0x15: FLOOD, GRP_TXT
	floodTraceRaw     = "25" + "41" + "1a2b" + "aabbccdd1122334400"                    // header 0x25: FLOOD, TRACE
)

var (
	gateOffCfg = func() *Config { return &Config{} }
	gateOnCfg  = func() *Config { return &Config{ClientRxCoverage: &ClientRxCoverageConfig{Enabled: true}} }
)

// dataTableCounts returns the row count of every data table in the store,
// skipping SQLite internals and migration bookkeeping (_migrations,
// _async_migrations, _meta, schema_version). Async startup migrations are
// drained first so the snapshot is deterministic.
func dataTableCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	s.WaitForAsyncMigrations()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '\_%' ESCAPE '\'
		  AND name != 'schema_version' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if len(names) == 0 {
		t.Fatal("no data tables found")
	}
	counts := make(map[string]int, len(names))
	for _, n := range names {
		var c int
		if err := s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, n)).Scan(&c); err != nil {
			t.Fatal(err)
		}
		counts[n] = c
	}
	return counts
}

// assertOnlyDeltas fails unless every table's row count changed by exactly the
// given delta (tables not listed must be unchanged).
func assertOnlyDeltas(t *testing.T, before, after map[string]int, want map[string]int) {
	t.Helper()
	got := map[string]int{}
	for name, a := range after {
		if d := a - before[name]; d != 0 {
			got[name] = d
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("table %s disappeared", name)
		}
	}
	if len(want) == 0 {
		want = map[string]int{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("row-count deltas = %v, want %v", got, want)
	}
}

func dispatch(t *testing.T, s *Store, cfg *Config, topic, payload string) (before, after map[string]int) {
	t.Helper()
	before = dataTableCounts(t, s)
	handleMessage(s, "test", MQTTSource{Name: "test"}, &mockMessage{topic: topic, payload: []byte(payload)}, nil, nil, cfg)
	after = dataTableCounts(t, s)
	return before, after
}

func clientMsgJSON(raw string) string {
	return `{"raw":"` + raw + `","direction":"rx","SNR":4.5,"RSSI":-101.0,"gps":{"lat":51.2,"lon":4.4,"acc_m":8.0}}`
}

// Gate OFF + valid client packet: nothing may be written anywhere — no
// coverage, no observer, no transmission/observation, no node.
func TestClientTopicGateOffWritesNothing(t *testing.T) {
	s := newTestStore(t)
	before, after := dispatch(t, s, gateOffCfg(), "meshcore/client/"+testCompanionPK+"/packets", clientTopicPayload)
	assertOnlyDeltas(t, before, after, nil)
}

// Gate ON + valid client packet: coverage (client_receptions + the
// self-reported name in client_observers) is written, and the companion is
// NOT registered as an ordinary observer.
func TestClientTopicGateOnWritesCoverageOnly(t *testing.T) {
	s := newTestStore(t)
	before, after := dispatch(t, s, gateOnCfg(), "meshcore/client/"+testCompanionPK+"/packets", clientTopicPayload)
	assertOnlyDeltas(t, before, after, map[string]int{"client_receptions": 1, "client_observers": 1})

	var observers int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observers WHERE lower(id) = ? OR lower(iata) = 'client'`, testCompanionPK).Scan(&observers); err != nil {
		t.Fatal(err)
	}
	if observers != 0 {
		t.Fatalf("companion registered as observer: %d rows", observers)
	}
}

// A FLOOD-routed hop packet on the client topic still yields coverage for the
// directly-heard forwarder (path[last]).
func TestClientTopicFloodHopPacketStillWritesCoverage(t *testing.T) {
	decoded, err := DecodePacket(floodGrpTxtHopRaw, nil, false)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	if decoded.Header.RouteType != packetpath.RouteFlood || decoded.Header.PayloadTypeName != "GRP_TXT" ||
		len(decoded.Path.Hops) != 1 || !strings.EqualFold(decoded.Path.Hops[0], "1a2b") {
		t.Fatalf("fixture sanity: route=%d type=%s hops=%v", decoded.Header.RouteType, decoded.Header.PayloadTypeName, decoded.Path.Hops)
	}

	s := newTestStore(t)
	before, after := dispatch(t, s, gateOnCfg(), "meshcore/client/"+testCompanionPK+"/packets", clientMsgJSON(floodGrpTxtHopRaw))
	assertOnlyDeltas(t, before, after, map[string]int{"client_receptions": 1})

	var heardKey, src string
	var keylen int
	if err := s.db.QueryRow(`SELECT heard_key, heard_keylen, src FROM client_receptions`).Scan(&heardKey, &keylen, &src); err != nil {
		t.Fatal(err)
	}
	if heardKey != "1a2b" || keylen != 2 || src != "rxlog" {
		t.Fatalf("reception = (%q, %d, %q), want (\"1a2b\", 2, \"rxlog\")", heardKey, keylen, src)
	}
}

// A FLOOD-routed TRACE carries per-hop SNR bytes in the header path, not node
// hashes. The same "1a2b" bytes that make a hop above must NOT become coverage.
func TestClientTopicFloodTraceWritesNoCoverage(t *testing.T) {
	decoded, err := DecodePacket(floodTraceRaw, nil, false)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	if decoded.Header.RouteType != packetpath.RouteFlood || decoded.Header.PayloadTypeName != "TRACE" ||
		len(decoded.Path.Hops) != 1 || !strings.EqualFold(decoded.Path.Hops[0], "1a2b") {
		t.Fatalf("fixture sanity: route=%d type=%s hops=%v", decoded.Header.RouteType, decoded.Header.PayloadTypeName, decoded.Path.Hops)
	}

	s := newTestStore(t)
	before, after := dispatch(t, s, gateOnCfg(), "meshcore/client/"+testCompanionPK+"/packets", clientMsgJSON(floodTraceRaw))
	assertOnlyDeltas(t, before, after, nil)
}

// Unsupported client sub-topics (and a client topic with no sub-topic) are
// dropped with the gate both off and on, even when the payload is a packet the
// observer path would ingest.
func TestClientTopicUnsupportedSubtopicsWriteNothing(t *testing.T) {
	topics := []string{
		"meshcore/client/" + testCompanionPK + "/rf",
		"meshcore/client/" + testCompanionPK + "/foo",
		"meshcore/client/" + testCompanionPK + "/status",
		"meshcore/client/" + testCompanionPK + "/neighbors",
		"meshcore/client/" + testCompanionPK,
		"meshcore/client",
	}
	rfSample := `{"type":"RF_SAMPLE","timestamp":"2026-08-17T10:00:00.000Z","gps":{"lat":51.2,"lon":4.4},"uptime_secs":84213,"noise_floor":-119,"recv_errors":5}`
	for _, gate := range []struct {
		name string
		cfg  func() *Config
	}{{"gateOff", gateOffCfg}, {"gateOn", gateOnCfg}} {
		for _, topic := range topics {
			for _, payload := range []string{clientTopicPayload, rfSample} {
				name := fmt.Sprintf("%s/%s/%d", gate.name, strings.Replace(topic, testCompanionPK, "PK", 1), len(payload))
				t.Run(name, func(t *testing.T) {
					s := newTestStore(t)
					before, after := dispatch(t, s, gate.cfg(), topic, payload)
					assertOnlyDeltas(t, before, after, nil)
				})
			}
		}
	}
}

// A blacklisted companion writes nothing on any client topic, gate off or on.
func TestClientTopicBlacklistedCompanionWritesNothing(t *testing.T) {
	for _, gateOn := range []bool{false, true} {
		for _, sub := range []string{"packets", "rf"} {
			t.Run(fmt.Sprintf("gateOn=%v/%s", gateOn, sub), func(t *testing.T) {
				cfg := &Config{ObserverBlacklist: []string{strings.ToUpper(testCompanionPK)}}
				if gateOn {
					cfg.ClientRxCoverage = &ClientRxCoverageConfig{Enabled: true}
				}
				s := newTestStore(t)
				before, after := dispatch(t, s, cfg, "meshcore/client/"+testCompanionPK+"/"+sub, clientTopicPayload)
				assertOnlyDeltas(t, before, after, nil)
			})
		}
	}
}

// The observer blacklist keeps its meaning on the ordinary observer path:
// a blacklisted observer's packets and status write nothing, while an
// unlisted observer on the same config is ingested normally.
func TestObserverTopicBlacklistUnchanged(t *testing.T) {
	cfg := &Config{ObserverBlacklist: []string{"blockedobs"}}
	for _, sub := range []string{"packets", "status"} {
		t.Run("blacklisted/"+sub, func(t *testing.T) {
			s := newTestStore(t)
			before, after := dispatch(t, s, cfg, "meshcore/SJC/BlockedObs/"+sub, clientTopicPayload)
			assertOnlyDeltas(t, before, after, nil)
		})
	}
	t.Run("unlisted/packets", func(t *testing.T) {
		s := newTestStore(t)
		before, after := dispatch(t, s, cfg, "meshcore/SJC/obs1/packets", clientTopicPayload)
		if after["transmissions"]-before["transmissions"] != 1 || after["observers"]-before["observers"] != 1 {
			t.Fatalf("unlisted observer not ingested: before=%v after=%v", before, after)
		}
	})
}

// meshcore/<iata>/<observer>/packets is unaffected by the client dispatch and
// by the coverage gate: the observer path ingests it exactly as before and the
// GPS field is ignored (no coverage row).
func TestObserverTopicPacketsUnchanged(t *testing.T) {
	for _, gate := range []struct {
		name string
		cfg  func() *Config
	}{{"gateOff", gateOffCfg}, {"gateOn", gateOnCfg}} {
		t.Run(gate.name, func(t *testing.T) {
			s := newTestStore(t)
			before, after := dispatch(t, s, gate.cfg(), "meshcore/SJC/obs1/packets", clientTopicPayload)
			assertOnlyDeltas(t, before, after, map[string]int{"transmissions": 1, "observations": 1, "observers": 1, "nodes": 1})

			var iata string
			if err := s.db.QueryRow(`SELECT iata FROM observers WHERE id = 'obs1'`).Scan(&iata); err != nil {
				t.Fatal(err)
			}
			if iata != "SJC" {
				t.Fatalf("observer iata = %q, want SJC", iata)
			}
		})
	}
}
