package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// TestChannelEndpointsServeMessagesOlderThanPacketDays verifies the server
// half of #296: a channel message the ingestor kept past packetDays (because
// retention.channelDays is longer) is still listed by GET /api/channels and
// returned by /api/channels/{hash}/messages, including under a region filter,
// which needs the observations the prune keeps with it. Both endpoints read
// SQLite with no age bound, so no server change is needed; this pins that.
func TestChannelEndpointsServeMessagesOlderThanPacketDays(t *testing.T) {
	srv, router := setupTestServer(t)
	for _, q := range []string{`DELETE FROM observations`, `DELETE FROM transmissions`} {
		if _, err := srv.db.conn.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// 30 days old: past a packetDays of 14, inside a channelDays of 90.
	aged := time.Now().UTC().AddDate(0, 0, -30)
	res, err := srv.db.conn.Exec(
		`INSERT INTO transmissions (raw_hex,hash,first_seen,route_type,payload_type,channel_hash,decoded_json) VALUES (?,?,?,0,5,'#history',?)`,
		"aa", "chmsg-aged-296", aged.Format(time.RFC3339), `{"sender":"Old","text":"Old: still here"}`,
	)
	if err != nil {
		t.Fatalf("insert tx: %v", err)
	}
	txID, _ := res.LastInsertId()
	obsRes, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?,?,?)`, "obs296", "Obs296", "CPH")
	if err != nil {
		t.Fatalf("insert observer: %v", err)
	}
	obsIdx, _ := obsRes.LastInsertId()
	if _, err := srv.db.conn.Exec(
		`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?,?,?,?,?,?)`,
		txID, obsIdx, 1.0, -90.0, `[]`, aged.Unix(),
	); err != nil {
		t.Fatalf("insert observation: %v", err)
	}

	get := func(url string, into interface{}) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code != 200 {
			t.Fatalf("%s: status=%d body=%s", url, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			t.Fatalf("%s: decode: %v", url, err)
		}
	}

	for _, region := range []string{"", "CPH"} {
		var list ChannelListResponse
		get("/api/channels?region="+region, &list)
		found := false
		for _, ch := range list.Channels {
			if ch["hash"] == "#history" {
				found = true
			}
		}
		if !found {
			t.Errorf("region %q: /api/channels does not list the 30-day-old channel: %+v", region, list.Channels)
		}

		var msgs struct {
			Messages []map[string]interface{} `json:"messages"`
			Total    int                      `json:"total"`
		}
		get("/api/channels/%23history/messages?limit=10&region="+region, &msgs)
		if len(msgs.Messages) != 1 || msgs.Total != 1 {
			t.Errorf("region %q: messages = %d (total %d), want the 30-day-old message", region, len(msgs.Messages), msgs.Total)
		}
	}
}
