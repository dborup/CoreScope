package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const channel379Old = "2020-01-01T12:00:00Z"
const channel379New = "2026-10-15T12:00:00Z"

// The old packet intentionally predates every packet-store window: lists
// retain their SQL/full-history contract, not the RAM-retention contract.
func seedChannelRegion379(t testing.TB, db *DB, v3 bool) {
	t.Helper()
	for _, o := range []struct{ id, iata string }{{"region379-a1", " aar "}, {"region379-a2", "AAR"}, {"region379-b", "CPH"}} {
		if _, err := db.conn.Exec(`INSERT INTO observers(id,name,iata) VALUES (?,?,?)`, o.id, o.id, o.iata); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range []struct {
		id, payload          int
		channel, at, decoded string
	}{
		{37901, 5, "#region379", channel379Old, `{"text":"Alpha: regional older message","sender":"Alpha"}`},
		{37902, 5, "#region379", channel379New, `{"text":"Beta: other region newer message","sender":"Beta"}`},
		{37903, 5, "enc_D1", channel379Old, `{}`},
		{37904, 5, "enc_D1", channel379New, `{}`},
		{37905, 5, "#no-observations379", channel379New, `{}`},
		{37906, 5, "#unknown-observer379", channel379New, `{}`},
		{37907, 5, "#null-observer379", channel379New, `{}`},
		{37908, 4, "#region379", "2026-10-16T12:00:00Z", `{"text":"Not a channel packet"}`},
	} {
		if _, err := db.conn.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,channel_hash,decoded_json) VALUES (?,'0500',?,?,?,?,?)`, x.id, fmt.Sprintf("channel379-%d", x.id), x.at, x.payload, x.channel, x.decoded); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []struct {
		tx       int
		observer interface{}
	}{
		{37901, "region379-a1"}, {37901, "region379-a2"}, {37901, "region379-a1"}, {37901, "region379-b"},
		{37902, "region379-b"},
		{37903, "region379-a1"}, {37903, "region379-a2"}, {37903, "region379-a1"},
		{37904, "region379-b"}, {37906, "missing379"}, {37907, nil}, {37908, "region379-a1"},
	} {
		query := `INSERT INTO observations(transmission_id,observer_id,path_json,timestamp) VALUES (?,?,'[]',1)`
		if v3 {
			query = `INSERT INTO observations(transmission_id,observer_idx,path_json,timestamp) VALUES (?,(SELECT rowid FROM observers WHERE id=?),'[]',1)`
		}
		if _, err := db.conn.Exec(query, o.tx, o.observer); err != nil {
			t.Fatal(err)
		}
	}
}

func channel379ByHash(t testing.TB, rows []map[string]interface{}, hash string) map[string]interface{} {
	t.Helper()
	for _, row := range rows {
		if row["hash"] == hash {
			return row
		}
	}
	t.Fatalf("missing channel %q: %v", hash, rows)
	return nil
}

func TestChannelRegion379CountsDistinctTransmissions(t *testing.T) {
	for _, v3 := range []bool{true, false} {
		t.Run(fmt.Sprintf("v3=%v", v3), func(t *testing.T) {
			db := pinTestDB(t, v3, true)
			seedChannelRegion379(t, db, v3)
			for _, c := range []struct {
				region           string
				plain, encrypted int
			}{{"AAR", 1, 1}, {" aar , AAR ", 1, 1}, {"CPH", 2, 1}, {"AAR,CPH", 2, 2}, {"", 2, 2}} {
				plain, err := db.GetChannels(c.region)
				if err != nil {
					t.Fatal(err)
				}
				enc, err := db.GetEncryptedChannels(c.region)
				if err != nil {
					t.Fatal(err)
				}
				if got := channel379ByHash(t, plain, "#region379")["messageCount"]; got != c.plain {
					t.Errorf("%q plain count=%v want%d", c.region, got, c.plain)
				}
				if got := channel379ByHash(t, enc, "enc_D1")["messageCount"]; got != c.encrypted {
					t.Errorf("%q encrypted count=%v want%d", c.region, got, c.encrypted)
				}
				if c.region == "AAR" && (len(plain) != 1 || len(enc) != 1) {
					t.Errorf("regional rows include unknown/unheard/non-channel packets: plain=%v enc=%v", plain, enc)
				}
			}
			for _, call := range []func() ([]map[string]interface{}, error){func() ([]map[string]interface{}, error) { return db.GetChannels("ZZZ") }, func() ([]map[string]interface{}, error) { return db.GetEncryptedChannels("ZZZ") }} {
				rows, err := call()
				if err != nil || len(rows) != 0 {
					t.Fatalf("unknown region rows=%v err=%v", rows, err)
				}
			}
		})
	}
}

func TestChannelRegion379SampleMatchesRegionalPopulation(t *testing.T) {
	for _, v3 := range []bool{true, false} {
		t.Run(fmt.Sprintf("v3=%v", v3), func(t *testing.T) {
			db := pinTestDB(t, v3, true)
			seedChannelRegion379(t, db, v3)
			for _, c := range []struct{ region, at, message, sender string }{{"AAR", channel379Old, "regional older message", "Alpha"}, {"CPH", channel379New, "other region newer message", "Beta"}, {"AAR,CPH", channel379New, "other region newer message", "Beta"}, {"", channel379New, "other region newer message", "Beta"}} {
				rows, err := db.GetChannels(c.region)
				if err != nil {
					t.Fatal(err)
				}
				row := channel379ByHash(t, rows, "#region379")
				if row["lastActivity"] != c.at || row["lastMessage"] != c.message || row["lastSender"] != c.sender {
					t.Errorf("%q incoherent regional sample: %v; want%s/%s/%s", c.region, row, c.at, c.message, c.sender)
				}
			}
		})
	}
}

func TestChannelRegion379HTTPIncludesEncryptedWithoutMutatingCache(t *testing.T) {
	srv, router := setupTestServer(t)
	seedChannelRegion379(t, srv.db, true)
	before, err := srv.db.GetChannels("AAR")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/channels?region=AAR&includeEncrypted=true", nil))
	if w.Code != 200 {
		t.Fatalf("status%d body%s", w.Code, w.Body.String())
	}
	var body struct {
		Channels []map[string]interface{} `json:"channels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"#region379", "enc_D1"} {
		if got := channel379ByHash(t, body.Channels, hash)["messageCount"]; got != float64(1) {
			t.Errorf("API %s count=%v want1", hash, got)
		}
	}
	after, err := srv.db.GetChannels("AAR")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(baseline) {
		t.Fatal("includeEncrypted changed cached decrypted channels")
	}
}

func TestChannelRegion379PlansKeepIndexedMembership(t *testing.T) {
	for _, v3 := range []bool{true, false} {
		for _, stats := range []bool{false, true} {
			t.Run(fmt.Sprintf("v3=%v/stats=%v", v3, stats), func(t *testing.T) {
				db := pinTestDB(t, v3, true)
				pinTestSeed(t, db, v3, 6000, 379)
				if stats {
					if _, err := db.conn.Exec("ANALYZE"); err != nil {
						t.Fatal(err)
					}
				}
				plan := explainPlan(t, db.conn, channelsSQL(v3, "?,?", true), "AAR", "CPH")
				assertChannelsPlan(t, "regional EXISTS", plan)
				found := false
				for _, line := range plan {
					if strings.Contains(line, "SEARCH o USING INDEX idx_observations_") && strings.Contains(line, "transmission_id=?") {
						found = true
					}
				}
				if !found {
					t.Fatalf("regional membership lost its transmission lookup index: %v", plan)
				}
				t.Logf("%s", strings.Join(plan, " | "))
			})
		}
	}
}
