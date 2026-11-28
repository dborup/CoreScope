package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #289 M1: the resolved-path backfill and the neighbour builder take
// each observation's route type from its own frame header
// (observations.raw_hex, header bits 1-0: firmware src/Packet.h
// PH_ROUTE_MASK, docs/packet_format.md), not from t.route_type, the route
// type of the transmission's first reception. The content hash does not
// cover the route bits (src/Packet.cpp calculatePacketHash), so DIRECT and
// FLOOD receptions of one payload share a transmission row.

// Frames whose header byte is FLOOD (0x15) or DIRECT (0x16). The second
// byte is chosen so that a header read from the wrong offset ("60"/"50",
// route 0) is a flood type: a DIRECT frame read that way is mis-attributed.
const (
	floodFrame289  = "1501c3aabb"
	directFrame289 = "1601c3aabb"
	mixedMask289   = int64(0b0110) // routes 1 (FLOOD) and 2 (DIRECT)
	floodMask289   = int64(0b0010) // route 1 only
)

func mask289(m int64) sql.NullInt64 { return sql.NullInt64{Int64: m, Valid: true} }

// The helper reads the header when it parses, gives up on a mixed mask when
// it does not, and otherwise keeps t.route_type.
func TestObservationRouteType_289(t *testing.T) {
	null := sql.NullInt64{}
	for _, c := range []struct {
		name    string
		header  string
		txRoute int
		mask    sql.NullInt64
		want    int
	}{
		{"header transport flood", "14", 2, mask289(mixedMask289), 0},
		{"header flood under DIRECT tx", "15", 2, mask289(mixedMask289), 1},
		{"header DIRECT under FLOOD tx", "16", 1, mask289(mixedMask289), 2},
		{"header transport direct", "17", 1, null, 3},
		{"upper-case header", "1A", 1, null, 2},
		{"lower-case header", "1a", 1, null, 2},
		{"header wins over a NULL route_type", "15", -1, null, 1},
		{"no header, mixed mask", "", 1, mask289(mixedMask289), -1},
		{"no header, mixed mask 0b0101", "", 0, mask289(0b0101), -1},
		{"no header, mixed mask 0b1010", "", 3, mask289(0b1010), -1},
		{"no header, full mask", "", 2, mask289(0b1111), -1},
		{"no header, flood-only mask", "", 1, mask289(0b0011), 1},
		{"no header, direct-only mask", "", 2, mask289(0b1100), 2},
		{"no header, NULL mask", "", 1, null, 1},
		{"no header, NULL mask, NULL route_type", "", -1, null, -1},
		{"short header", "1", 1, null, 1},
		{"short header, mixed mask", "6", 1, mask289(mixedMask289), -1},
		{"invalid header, mixed mask", "zz", 1, mask289(mixedMask289), -1},
		{"invalid header, single-family mask", "g6", 2, mask289(0b0100), 2},
	} {
		if got := observationRouteType(c.header, c.txRoute, c.mask); got != c.want {
			t.Errorf("%s: observationRouteType(%q, %d, %+v) = %d, want %d", c.name, c.header, c.txRoute, c.mask, got, c.want)
		}
	}
}

// seedObservation289 writes one transmission (route type txRoute, route_mask
// mask, NULL when !mask.Valid) with one observation from the obs188 observer.
// rawHex "" stores a NULL frame, as rows written before #881 have.
func seedObservation289(t *testing.T, store *Store, txRoute int, mask sql.NullInt64, rawHex, pathJSON string, ts int64) int64 {
	t.Helper()
	var obsIdx int64
	if err := store.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, strings.ToUpper(obs188)).Scan(&obsIdx); err != nil {
		t.Fatal(err)
	}
	res, err := store.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, route_mask) VALUES ('00', ?, '2026-06-01T00:00:00Z', ?, 5, '{}', ?)`,
		fmt.Sprintf("h289-%d-%s-%d", txRoute, rawHex, ts), txRoute, mask)
	if err != nil {
		t.Fatal(err)
	}
	txID, _ := res.LastInsertId()
	var frame sql.NullString
	if rawHex != "" {
		frame = sql.NullString{String: rawHex, Valid: true}
	}
	res, err = store.db.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex) VALUES (?, ?, ?, ?, ?)`,
		txID, obsIdx, pathJSON, ts, frame)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

type observationCase289 struct {
	name    string
	txRoute int
	mask    sql.NullInt64
	rawHex  string
	flood   bool // the observation must be treated as a flood reception
}

var observationCases289 = []observationCase289{
	// #289 direction 1: DIRECT anchored as FLOOD.
	{"DIRECT frame under a FLOOD transmission", routeFlood188, mask289(mixedMask289), directFrame289, false},
	// #289 direction 2: FLOOD treated as DIRECT.
	{"FLOOD frame under a DIRECT transmission", routeDirect188, mask289(mixedMask289), floodFrame289, true},
	// No frame and a mask with both families: the route type is unknown.
	{"no frame, mixed mask", routeFlood188, mask289(mixedMask289), "", false},
	// No frame, one family or a NULL mask: today's result (t.route_type).
	{"no frame, flood-only mask", routeFlood188, mask289(floodMask289), "", true},
	{"no frame, NULL mask", routeFlood188, sql.NullInt64{}, "", true},
	{"no frame, direct-only mask", routeDirect188, mask289(0b0100), "", false},
}

// The backfill anchors the last hop on the observer only for a flood
// reception. The fixture's only observer edge is obs188 <-> c3a, so a flood
// ["c3"] resolves to c3a and anything else leaves the row NULL (the 1-byte
// prefix is shared by c3a and c3b).
func TestResolvedPathBackfill_UsesObservationRouteType_289(t *testing.T) {
	for _, c := range observationCases289 {
		t.Run(c.name, func(t *testing.T) {
			store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
			defer store.Close()
			id := seedObservation289(t, store, c.txRoute, c.mask, c.rawHex, `["c3"]`, ts190)
			primeIndexAndGraph188(t, store)
			if _, err := store.RunResolvedPathBackfill(context.Background(), 100, 0); err != nil {
				t.Fatal(err)
			}
			rp := resolvedPathOf188(t, store, id)
			if !c.flood {
				if rp.Valid {
					t.Fatalf("resolved_path = %s, want NULL: the observer anchor was used for a non-flood reception", rp.String)
				}
				return
			}
			if !rp.Valid {
				t.Fatal("resolved_path is NULL, want the observer-anchored c3a")
			}
			wantPath188(t, unmarshalResolvedPathLocal(rp.String), c3a)
		})
	}
}

// The builder adds the observer <-> last-hop edge only for a flood
// reception. ["c366"] names c3b uniquely.
func TestNeighborEdgesBuilder_UsesObservationRouteType_289(t *testing.T) {
	for _, c := range observationCases289 {
		t.Run(c.name, func(t *testing.T) {
			store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
			defer store.Close()
			clearEdges188(t, store)
			seedObservation289(t, store, c.txRoute, c.mask, c.rawHex, `["c366"]`, ts190+1)
			if _, err := store.buildAndPersistNeighborEdges(); err != nil {
				t.Fatal(err)
			}
			g, err := loadNeighborGraph(store.db)
			if err != nil {
				t.Fatal(err)
			}
			if got := g.IsAdjacent(obs188, c3b); got != c.flood {
				t.Fatalf("observer<->c3b edge = %v, want %v", got, c.flood)
			}
		})
	}
}
