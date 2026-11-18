package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
	"github.com/meshcore-analyzer/dbschema"
)

// #898 M1 — station details, the read-only server side: the submit rules,
// GET /api/nodes/{pubkey}/station, the in-force flags on the read side, the
// admin contact's single read path, and the leak gates.

const (
	adminContact898  = "ZZADMIN9"
	publicContact898 = "ZZPUBLIC7"
)

// claimRow898 is a claim row as the ingestor writes it (test fixture).
type claimRow898 struct {
	id, pk, status       string
	heightM              *float64
	heightOp, detailsOp  string
	hIn, dIn             bool
	antennaType          string
	gain                 *float64
	powerMask            int
	installation         string
	contactPublic        string
	contactAdmin         string
	lat, lon             float64
	provenAt, reviewedAt int64
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func (e *claim789Env) insert898(t *testing.T, c claimRow898) {
	t.Helper()
	insertClaimRow898(t, e.srv.db.conn, e.now, c)
	e.svc.invalidateOwners()
}

// insertClaimRow898 writes c at time now, as the ingestor would.
func insertClaimRow898(tb testing.TB, db *sql.DB, now time.Time, c claimRow898) {
	tb.Helper()
	if c.provenAt == 0 {
		c.provenAt = now.UnixMilli()
	}
	if c.reviewedAt == 0 {
		c.reviewedAt = c.provenAt
	}
	var mask interface{}
	if c.powerMask != 0 {
		mask = c.powerMask
	}
	if _, err := db.Exec(`INSERT INTO node_antenna_claims
		(request_id, pubkey, height_m, height_op, details_op, height_in_force, details_in_force,
		 antenna_type, gain_dbi, power_mask, installation, contact_public,
		 code, status, created_at, expires_at, proven_at, proof_hash, proof_lat, proof_lon, reviewed_at, approved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '7KX9QM', ?, ?, ?, ?, 'abcd', ?, ?, ?, CASE WHEN ? = 'approved' THEN 'admin' END)`,
		c.id, c.pk, c.heightM, nullIfEmpty(c.heightOp), nullIfEmpty(c.detailsOp), c.hIn, c.dIn,
		nullIfEmpty(c.antennaType), c.gain, mask, nullIfEmpty(c.installation), nullIfEmpty(c.contactPublic),
		c.status, now.UnixMilli(), now.Add(24*time.Hour).UnixMilli(), c.provenAt, c.lat, c.lon, c.reviewedAt, c.status); err != nil {
		tb.Fatal(err)
	}
	if c.contactAdmin != "" {
		if _, err := db.Exec(`INSERT INTO node_claim_admin_contacts (request_id, contact) VALUES (?, ?)`, c.id, c.contactAdmin); err != nil {
			tb.Fatal(err)
		}
	}
}

// stationRow is an approved details claim in force at the test position.
func stationRow(id, pk string) claimRow898 {
	g := 5.8
	return claimRow898{id: id, pk: pk, status: antennaclaim.StatusApproved, detailsOp: "set", dIn: true,
		antennaType: "omni", gain: &g, powerMask: 6, installation: "mast",
		contactPublic: publicContact898, contactAdmin: adminContact898, lat: est789Lat, lon: est789Lon}
}

func detailsOn(mod func(*antennaclaim.Config)) *antennaclaim.Config {
	return claimsOn(func(c *antennaclaim.Config) {
		c.DetailsEnabled = true
		if mod != nil {
			mod(c)
		}
	})
}

func (e *claim789Env) station(t *testing.T, pk string) NodeStationResponse {
	t.Helper()
	w := e.do("GET", "/api/nodes/"+pk+"/station", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("station %s…: %d %s", pk[:4], w.Code, w.Body)
	}
	return decode[NodeStationResponse](t, w)
}

// ── /station and the read side ────────────────────────────────────────────

// T2 (read side): an approved combined claim serves its height to profiles
// and its details, contactPublic included, from /station.
func TestAntennaClaims898_StationServesApprovedDetails(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	if got := env.station(t, pk); got.Details != nil || got.Stale || got.ApprovedAt != nil {
		t.Fatalf("no claim: %+v", got)
	}
	row := stationRow("a000000000000001", pk)
	h := 12.5
	row.heightM, row.heightOp, row.hIn = &h, "set", true
	env.insert898(t, row)
	got := env.station(t, pk)
	want := antennaclaim.StationDetails{AntennaType: "omni", GainDBi: row.gain, Power: []string{"solar", "battery"},
		Installation: "mast", ContactPublic: publicContact898}
	if got.Details == nil || !reflect.DeepEqual(*got.Details, want) || got.ApprovedAt == nil || *got.ApprovedAt != env.now.UnixMilli() {
		t.Fatalf("station %+v (details %+v), want %+v", got, got.Details, want)
	}
	if a := env.endA(t, pk); a.AntennaSource != "owner" || a.AntennaM != 12.5 {
		t.Fatalf("combined claim's height: %+v", a)
	}
}

// With detailsEnabled off, /station answers {"details":null} even for
// stored details (N4); it never needs the terrain profile.
func TestAntennaClaims898_StationGateAndTerrainOff(t *testing.T) {
	pk := pk789("a")
	for name, cc := range map[string]*antennaclaim.Config{
		"claims off":       nil,
		"details off":      claimsOn(nil),
		"details, no key":  detailsOn(nil), // weak key below
		"details only key": {DetailsEnabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			key := claimAdminKey
			if name == "details, no key" {
				key = ""
			}
			env := newClaim789Env(t, key, cc, nil)
			env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
			env.insert898(t, stationRow("a000000000000001", pk))
			w := env.do("GET", "/api/nodes/"+pk+"/station", "", "")
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"details":null}` {
				t.Fatalf("%d %s, want {\"details\":null}", w.Code, w.Body)
			}
			cfg := decode[AntennaClaimConfigResponse](t, env.do("GET", "/api/antenna-claims/config", "", ""))
			if cfg.DetailsEnabled {
				t.Fatalf("config says details are enabled: %+v", cfg)
			}
		})
	}
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.insert898(t, stationRow("a000000000000001", pk))
	env.srv.terrain = nil
	if got := env.station(t, pk); got.Details == nil {
		t.Fatalf("terrain off hid the details: %+v", got)
	}
	if cfg := decode[AntennaClaimConfigResponse](t, env.do("GET", "/api/antenna-claims/config", "", "")); !cfg.DetailsEnabled || !cfg.Enabled {
		t.Fatalf("config %+v", cfg)
	}
}

// T4 + T5 (read side): the in-force flags decide what is served, not the
// newest approval (mutants M4, M5, M6).
func TestAntennaClaims898_InForceFlagsDecide(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	t0 := env.now.UnixMilli()
	// A = height 10 + details X, its details since replaced by B.
	a := stationRow("a000000000000001", pk)
	h10 := 10.0
	a.heightM, a.heightOp, a.hIn, a.dIn, a.antennaType, a.contactPublic, a.contactAdmin = &h10, "set", true, false, "panel", "", ""
	a.provenAt = t0
	env.insert898(t, a)
	// B = details Y, approved later, in force.
	b := stationRow("a000000000000002", pk)
	b.antennaType, b.provenAt = "yagi", t0+1000
	env.insert898(t, b)
	if got := env.station(t, pk); got.Details == nil || got.Details.AntennaType != "yagi" {
		t.Fatalf("details: %+v, want B's yagi", got.Details)
	}
	if e := env.endA(t, pk); e.AntennaSource != "owner" || e.AntennaM != 10 {
		t.Fatalf("height after a details-only claim: %+v, want A's 10 m", e)
	}
	// C = height clear, approved last: A's height is out of force.
	if _, err := env.srv.db.conn.Exec(`UPDATE node_antenna_claims SET height_in_force = 0, status = 'superseded' WHERE request_id = ?`, a.id); err != nil {
		t.Fatal(err)
	}
	env.insert898(t, claimRow898{id: "a000000000000003", pk: pk, status: antennaclaim.StatusApproved, heightOp: "clear", hIn: true,
		lat: est789Lat, lon: est789Lon, provenAt: t0 + 2000})
	if e := env.endA(t, pk); e.AntennaSource == "owner" {
		t.Fatalf("a cleared height is still served: %+v", e)
	}
	// M5: an approved row whose height is out of force is never served,
	// even as the newest approval.
	h30 := 30.0
	env.insert898(t, claimRow898{id: "a000000000000004", pk: pk, status: antennaclaim.StatusApproved, heightM: &h30, heightOp: "set",
		hIn: false, detailsOp: "clear", dIn: true, lat: est789Lat, lon: est789Lon, provenAt: t0 + 3000})
	if e := env.endA(t, pk); e.AntennaSource == "owner" {
		t.Fatalf("a height out of force is served: %+v", e)
	}
	// That claim cleared the details: nothing is served, not A's panel.
	if _, err := env.srv.db.conn.Exec(`UPDATE node_antenna_claims SET details_in_force = 0 WHERE request_id = ?`, b.id); err != nil {
		t.Fatal(err)
	}
	env.svc.invalidateOwners()
	if got := env.station(t, pk); got.Details != nil {
		t.Fatalf("details after a clear: %+v", got.Details)
	}
	// T5: B revoked (flags 0) — nothing comes back from A.
	if _, err := env.srv.db.conn.Exec(`UPDATE node_antenna_claims SET status = 'revoked' WHERE request_id = ?`, b.id); err != nil {
		t.Fatal(err)
	}
	env.svc.invalidateOwners()
	if got := env.station(t, pk); got.Details != nil {
		t.Fatalf("revoking B resurrected details: %+v", got.Details)
	}
}

// T11: staleness is per part, from each part's own proof position, and
// honours staleMoveM (mutant M17).
func TestAntennaClaims898_StalenessPerPart(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	h := 12.0
	// The height was proven 40 m north, the details here.
	env.insert898(t, claimRow898{id: "a000000000000001", pk: pk, status: antennaclaim.StatusApproved, heightM: &h, heightOp: "set", hIn: true,
		lat: est789Lat + 40/111320.0, lon: est789Lon})
	env.insert898(t, stationRow("a000000000000002", pk))
	// The node moves 45 m south of the details' proof: 85 m from the
	// height's, 45 m from the details'.
	env.move(t, pk, est789Lat-45/111320.0, est789Lon)
	env.svc.invalidateOwners()
	if got := env.station(t, pk); got.Details == nil || got.Stale {
		t.Fatalf("details 45 m away: %+v, want served", got)
	}
	if e := env.endA(t, pk); e.AntennaSource == "owner" {
		t.Fatalf("height 85 m from its proof is served: %+v", e)
	}
	// 51 m from the details' proof: withheld, stale.
	env.move(t, pk, est789Lat-51/111320.0, est789Lon)
	if got := env.station(t, pk); got.Details != nil || !got.Stale || got.ApprovedAt != nil {
		t.Fatalf("details 51 m away: %+v, want null and stale", got)
	}
	// 49 m: served again.
	env.move(t, pk, est789Lat-49/111320.0, est789Lon)
	if got := env.station(t, pk); got.Details == nil || got.Stale {
		t.Fatalf("details 49 m away: %+v, want served", got)
	}
	// A configured staleMoveM is honoured.
	env2 := newClaim789Env(t, claimAdminKey, detailsOn(func(c *antennaclaim.Config) { c.StaleMoveM = 100 }), nil)
	env2.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env2.insert898(t, stationRow("a000000000000002", pk))
	env2.move(t, pk, est789Lat-80/111320.0, est789Lon)
	if got := env2.station(t, pk); got.Details == nil {
		t.Fatalf("80 m with staleMoveM 100: %+v, want served", got)
	}
}

// T12: hidden, unknown and unpositioned nodes give byte-identical 404s on
// /station and on a details POST; the admin list leaves out hidden nodes'
// claims (mutant M18).
func TestAntennaClaims898_HiddenLikeUnknown(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	hidden, nogps, unknown := pk789("a"), pk789("b"), pk789("c")
	env.addNode(t, hidden, "private-roof", "repeater", est789Lat, est789Lon)
	env.addNode(t, nogps, "No position", "repeater", 0, 0)
	env.insert898(t, stationRow("a000000000000001", hidden))
	body := `{"details":{"antennaType":"omni","contactAdmin":"x"}}`
	refGet := env.do("GET", "/api/nodes/"+unknown+"/station", "", "")
	refPost := env.post(unknown, body)
	if refGet.Code != http.StatusNotFound || refPost.Code != http.StatusNotFound {
		t.Fatalf("unknown: GET %d, POST %d", refGet.Code, refPost.Code)
	}
	for name, pk := range map[string]string{"hidden": hidden, "no position": nogps} {
		g := env.do("GET", "/api/nodes/"+pk+"/station", "", "")
		if g.Code != refGet.Code || g.Body.String() != refGet.Body.String() || g.Header().Get("Content-Type") != refGet.Header().Get("Content-Type") {
			t.Errorf("%s GET: %d %q differs from unknown %d %q", name, g.Code, g.Body, refGet.Code, refGet.Body)
		}
		p := env.post(pk, body)
		if p.Code != refPost.Code || p.Body.String() != refPost.Body.String() {
			t.Errorf("%s POST: %d %q differs from unknown %d %q", name, p.Code, p.Body, refPost.Code, refPost.Body)
		}
	}
	if w := env.do("GET", "/api/nodes/not-a-key/station", "", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad key: %d", w.Code)
	}
	list := env.do("GET", "/api/admin/antenna-claims", "", claimAdminKey)
	if strings.Contains(list.Body.String(), "a000000000000001") || strings.Contains(list.Body.String(), adminContact898) {
		t.Fatalf("admin list shows a hidden node's claim: %s", list.Body)
	}
}

// ── Submit ────────────────────────────────────────────────────────────────

// T10 (server): the request rules per part, the details gate (N4) and the
// gain review flag (Q6).
func TestAntennaClaims898_SubmitRules(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(func(c *antennaclaim.Config) { c.SubmissionsPerHour = 1000 }), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	for _, body := range []string{
		`{}`, `{"clear":false,"clearDetails":false}`,
		`{"heightM":12,"clear":true}`,
		`{"details":{"antennaType":"omni"},"clearDetails":true}`,
		`{"clearDetails":true,"details":{"contactAdmin":"x"}}`,
		`{"details":{}}`, `{"details":null}`,
		`{"details":{"antennaType":"dipole"}}`,
		`{"details":{"gainDbi":20.1}}`, `{"details":{"gainDbi":-5.1}}`, `{"details":{"gainDbi":0.05}}`,
		`{"details":{"power":[]}}`, `{"details":{"power":["solar","solar"]}}`, `{"details":{"power":["wind"]}}`,
		`{"details":{"installation":"tower"}}`,
		`{"details":{"contactAdmin":"` + strings.Repeat("x", 33) + `"}}`,
		`{"details":{"contactPublic":"https://x"}}`,
		`{"details":{"contactPublic":"a\u202eb"}}`, `{"details":{"contactAdmin":"a\u200db"}}`,
		`{"details":{"contactAdmin":"<b>"}}`, `{"details":{"contactPublic":"a\nb"}}`,
		`{"details":{"contactAdmin":" lead"}}`,
		`{"details":{"antennaType":"omni","note":"x"}}`,
		`{"details":{"antennaType":"omni"},"contactAdmin":"x"}`,
	} {
		if w := env.post(pk, body); w.Code != http.StatusBadRequest {
			t.Errorf("body %.70q: %d %s, want 400", body, w.Code, w.Body)
		}
	}
	if n := len(env.queued(t)); n != 0 {
		t.Fatalf("%d bad bodies queued", n)
	}
	ok := map[string]antennaclaim.Parts{
		`{"heightM":12}`:                        {Height: "set"},
		`{"clear":true}`:                        {Height: "clear"},
		`{"clearDetails":true}`:                 {Details: "clear"},
		`{"details":{"contactAdmin":"OZ1ABC"}}`: {Details: "set"},
		`{"heightM":12,"clearDetails":true}`:    {Height: "set", Details: "clear"},
		`{"clear":true,"clearDetails":true}`:    {Height: "clear", Details: "clear"},
		`{"heightM":12.5,"details":{"antennaType":"yagi","gainDbi":-5,"power":["battery","mains"],"installation":"portable","contactPublic":"Søren","contactAdmin":"sms 12345678"}}`: {Height: "set", Details: "set"},
	}
	for body, want := range ok {
		if w := env.post(pk, body); w.Code != http.StatusAccepted {
			t.Fatalf("body %s: %d %s", body, w.Code, w.Body)
		}
		cmds := env.queued(t)
		if got := cmds[len(cmds)-1].Parts(); got != want {
			t.Errorf("body %s queued parts %+v, want %+v", body, got, want)
		}
		if err := cmds[len(cmds)-1].Validate(); err != nil {
			t.Errorf("body %s queued an invalid command: %v", body, err)
		}
	}
	// Gain outside 0–15 dBi is flagged for review; inside it is not.
	for body, want := range map[string]bool{
		`{"details":{"gainDbi":15}}`: false, `{"details":{"gainDbi":0}}`: false,
		`{"details":{"gainDbi":15.1}}`: true, `{"details":{"gainDbi":-0.1}}`: true,
	} {
		env.post(pk, body)
		cmds := env.queued(t)
		if got := cmds[len(cmds)-1].NeedsReview; got != want {
			t.Errorf("%s: needsReview %v, want %v", body, got, want)
		}
	}

	// detailsEnabled off: setting details is 403, clearing stays open.
	off := newClaim789Env(t, claimAdminKey, claimsOn(nil), nil)
	off.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	if w := off.post(pk, `{"details":{"antennaType":"omni"}}`); w.Code != http.StatusForbidden {
		t.Errorf("details with detailsEnabled off: %d", w.Code)
	}
	if w := off.post(pk, `{"heightM":12,"details":{"antennaType":"omni"}}`); w.Code != http.StatusForbidden {
		t.Errorf("height + details with detailsEnabled off: %d", w.Code)
	}
	if w := off.post(pk, `{"clearDetails":true}`); w.Code != http.StatusAccepted {
		t.Errorf("clearDetails with detailsEnabled off: %d %s", w.Code, w.Body)
	}
	if n := len(off.queued(t)); n != 1 {
		t.Errorf("%d queued with details off, want only the clear", n)
	}
}

// ── Admin ─────────────────────────────────────────────────────────────────

// T6 (server): the 409 for an older proof is per part (mutant M7).
func TestAntennaClaims898_PerPartConflict(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	t0 := env.now.UnixMilli()
	h := 12.0
	env.insert898(t, claimRow898{id: "a000000000000001", pk: pk, status: antennaclaim.StatusProven, heightM: &h, heightOp: "set",
		lat: est789Lat, lon: est789Lon, provenAt: t0})
	older := stationRow("a000000000000002", pk)
	older.status, older.dIn, older.provenAt = antennaclaim.StatusProven, false, t0
	env.insert898(t, older)
	newer := stationRow("a000000000000003", pk)
	newer.provenAt = t0 + 1000
	env.insert898(t, newer)
	if w := env.do("POST", "/api/admin/antenna-claims/a000000000000001/approve", "", claimAdminKey); w.Code != http.StatusAccepted {
		t.Errorf("older height under a newer details claim: %d %s, want 202", w.Code, w.Body)
	}
	if w := env.do("POST", "/api/admin/antenna-claims/a000000000000002/approve", "", claimAdminKey); w.Code != http.StatusConflict {
		t.Errorf("older details under newer details: %d %s, want 409", w.Code, w.Body)
	}
}

// T7: approval is all-or-nothing; the only decision routes act on the whole
// claim.
func TestAntennaClaims898_NoPerPartDecisionRoute(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	var got []string
	env.router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		if tpl, err := route.GetPathTemplate(); err == nil && strings.HasPrefix(tpl, "/api/admin/antenna-claims/") {
			got = append(got, tpl)
		}
		return nil
	})
	sort.Strings(got)
	want := []string{"/api/admin/antenna-claims/{id}/approve", "/api/admin/antenna-claims/{id}/reject", "/api/admin/antenna-claims/{id}/revoke"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("admin decision routes %v, want exactly %v", got, want)
	}
}

// The admin list carries the parts, the in-force flags and the details.
func TestAntennaClaims898_AdminListParts(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	h := 10.0
	row := stationRow("a000000000000001", pk)
	row.heightM, row.heightOp, row.hIn, row.dIn = &h, "set", true, false
	env.insert898(t, row)
	env.insert898(t, claimRow898{id: "a000000000000002", pk: pk, status: antennaclaim.StatusApproved, detailsOp: "clear", dIn: true, lat: est789Lat, lon: est789Lon})
	list := decode[AdminAntennaClaimListResponse](t, env.do("GET", "/api/admin/antenna-claims", "", claimAdminKey))
	by := map[string]AdminAntennaClaim{}
	for _, c := range list.Claims {
		by[c.ID] = c
	}
	a, b := by["a000000000000001"], by["a000000000000002"]
	if a.HeightOp != "set" || a.DetailsOp != "set" || !a.HeightInForce || a.DetailsInForce || a.Details == nil || a.Details.AntennaType != "omni" || a.HeightM == nil {
		t.Fatalf("combined row %+v", a)
	}
	if b.HeightOp != "" || b.DetailsOp != "clear" || b.Details != nil || b.HeightM != nil || !b.DetailsInForce {
		t.Fatalf("details clear row %+v", b)
	}
}

// ── Leak gates ────────────────────────────────────────────────────────────

// publicResponseTypes are the response types of every public claim route.
var publicResponseTypes = []interface{}{
	NodeStationResponse{}, AntennaClaimStatusResponse{}, AntennaClaimIssuedResponse{},
	AntennaClaimConfigResponse{}, NodeAntennaResponse{}, antennaclaim.StationDetails{},
}

// jsonTags walks t (embedded structs, pointers, slices and maps included)
// and returns every JSON field name it can marshal.
func jsonTags(t reflect.Type, seen map[reflect.Type]bool) []string {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return nil
	}
	seen[t] = true
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if !f.Anonymous || f.Tag.Get("json") != "" {
			out = append(out, name)
		}
		out = append(out, jsonTags(f.Type, seen)...)
	}
	return out
}

// T9a: no public response type can carry the admin contact (mutant M12).
func TestAntennaClaims898_PublicTypesHaveNoAdminContact(t *testing.T) {
	for _, v := range publicResponseTypes {
		for _, tag := range jsonTags(reflect.TypeOf(v), map[reflect.Type]bool{}) {
			if strings.EqualFold(tag, "contactAdmin") || strings.EqualFold(tag, "ContactAdmin") {
				t.Errorf("%T can carry %s", v, tag)
			}
		}
	}
	// The walk itself must see the admin contact where it is.
	found := false
	for _, tag := range jsonTags(reflect.TypeOf(AdminAntennaClaim{}), map[reflect.Type]bool{}) {
		found = found || tag == "contactAdmin"
	}
	if !found {
		t.Fatal("the reflection walk misses AdminAntennaClaim.contactAdmin: it would not catch a leak")
	}
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// T9b: plant both contacts on an approved claim and GET every public route
// (path parameters filled with the claim's node and ids), the share page
// and the node card: no body carries contactAdmin, and contactPublic
// appears only on /station (mutants M11, M12, M13). The admin list carries
// contactAdmin for proven and approved claims only (M14).
func TestAntennaClaims898_AdminContactNeverPublic(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof Repeater", "repeater", est789Lat, est789Lon)
	approved := stationRow("a000000000000001", pk)
	h := 12.0
	approved.heightM, approved.heightOp, approved.hIn = &h, "set", true
	env.insert898(t, approved)
	pending := stationRow("a000000000000002", pk)
	pending.status, pending.dIn, pending.contactAdmin = antennaclaim.StatusPending, false, "ZZPENDING3"
	env.insert898(t, pending)
	proven := stationRow("a000000000000003", pk)
	proven.status, proven.dIn, proven.contactAdmin = antennaclaim.StatusProven, false, "ZZPROVEN5"
	env.insert898(t, proven)
	// A second node whose approved details carry only the admin contact
	// (N2): a read that falls back to it (M11) has nothing else to show.
	pk2 := pk789("e")
	env.addNode(t, pk2, "Hill Repeater", "repeater", est789Lat, est789Lon)
	adminOnly := stationRow("a000000000000004", pk2)
	adminOnly.contactPublic, adminOnly.contactAdmin = "", "ZZADMINONLY1"
	env.insert898(t, adminOnly)
	// Node shares on, so /p?node= and the card are served too.
	env.srv.cfg.Share.Nodes.Enabled = true
	env.srv.initNodeCards()
	env.srv.registerShareRoutes(env.router, newIndexDoc(sharePublicDir, newOGMeta(brandPick(nil), sharePublicDir, nil)))

	var targets []string
	for _, node := range []struct{ pk, id string }{{pk, approved.id}, {pk2, adminOnly.id}} {
		fill := map[string]string{"pubkey": node.pk, "id": node.id, "requestId": node.id, "hash": "abcd"}
		for key, meta := range routeDescriptions() {
			if meta.Auth || !strings.HasPrefix(key, "GET ") {
				continue
			}
			path := pathParam.ReplaceAllStringFunc(strings.TrimPrefix(key, "GET "), func(m string) string {
				if v, ok := fill[m[1:len(m)-1]]; ok {
					return v
				}
				return "x"
			})
			targets = append(targets, path)
		}
		targets = append(targets, "/p?node="+node.pk, "/api/nodes/"+node.pk+"/card.png", "/api/nodes/"+node.pk)
	}
	targets = append(targets, "/api/nodes", "/api/nodes/bulk-health",
		"/api/antenna-claims/requests/"+pending.id, "/api/antenna-claims/requests/"+proven.id)
	sort.Strings(targets)
	station := "/api/nodes/" + pk + "/station"
	if got := env.station(t, pk2); got.Details == nil || got.Details.AntennaType != "omni" {
		t.Fatalf("the admin-only node's station: %+v", got)
	}
	sawStation := false
	for _, path := range targets {
		w := env.do("GET", path, "", "")
		body := w.Body.String()
		for _, c := range []string{adminContact898, "ZZPENDING3", "ZZPROVEN5", "ZZADMINONLY1"} {
			if strings.Contains(body, c) {
				t.Errorf("GET %s (%d) carries the admin contact %s", path, w.Code, c)
			}
		}
		if strings.Contains(body, publicContact898) {
			if path != station {
				t.Errorf("GET %s (%d) carries contactPublic; only /station may", path, w.Code)
			}
			sawStation = true
		}
	}
	if !sawStation {
		t.Fatal("/station did not serve contactPublic: the walk proves nothing")
	}
	// The share page and the card were really served.
	if w := env.do("GET", "/p?node="+pk, "", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Roof Repeater") {
		t.Fatalf("/p?node= not served (%d): the walk does not cover it", w.Code)
	}

	list := env.do("GET", "/api/admin/antenna-claims", "", claimAdminKey).Body.String()
	if !strings.Contains(list, adminContact898) || !strings.Contains(list, "ZZPROVEN5") {
		t.Errorf("admin list lacks the proven/approved admin contacts: %s", list)
	}
	if strings.Contains(list, "ZZPENDING3") {
		t.Errorf("admin list shows a pending claim's admin contact (Q5)")
	}
}

// T13 (server): submitting and listing never logs a contact.
func TestAntennaClaims898_ContactsNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.post(pk, `{"details":{"contactAdmin":"`+adminContact898+`","contactPublic":"`+publicContact898+`"}}`)
	env.post(pk, `{"details":{"contactAdmin":"bad:`+adminContact898+`"}}`)
	env.insert898(t, stationRow("a000000000000001", pk))
	env.do("GET", "/api/admin/antenna-claims", "", claimAdminKey)
	env.station(t, pk)
	for _, c := range []string{adminContact898, publicContact898} {
		if strings.Contains(buf.String(), c) {
			t.Errorf("a log line carries %s:\n%s", c, buf.String())
		}
	}
}

// T14: /api/nodes keeps its key set: station details are not added to it.
func TestAntennaClaims898_NodesKeySetUnchanged(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	keys := func() string {
		t.Helper()
		var resp struct {
			Nodes []map[string]json.RawMessage `json:"nodes"`
		}
		if err := json.Unmarshal(env.do("GET", "/api/nodes", "", "").Body.Bytes(), &resp); err != nil || len(resp.Nodes) == 0 {
			t.Fatalf("/api/nodes: %v (%d nodes)", err, len(resp.Nodes))
		}
		var ks []string
		for k := range resp.Nodes[0] {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	before := keys()
	env.insert898(t, stationRow("a000000000000001", pk))
	env.station(t, pk)
	if after := keys(); after != before {
		t.Fatalf("/api/nodes keys changed:\n before %s\n after  %s", before, after)
	}
}

// BenchmarkStationSnapshot898 is the station snapshot's read: one SELECT
// of 2K approved details, every ownerHeightsTTL (T14).
func BenchmarkStationSnapshot898(b *testing.B) {
	// The snapshot reads only the claims table: no test server is needed,
	// and every setup step fails through b.
	conn, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "claims.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { conn.Close() })
	if err := dbschema.EnsureNodeAntennaClaims(conn); err != nil {
		b.Fatal(err)
	}
	svc := newAntennaClaimService(nil, conn, "")
	now := time.UnixMilli(1_800_000_000_000)
	for i := 0; i < 2000; i++ {
		insertClaimRow898(b, conn, now, stationRow(fmt.Sprintf("%016x", i+1), fmt.Sprintf("%064x", i+1)))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := svc.readOwnerStations(context.Background())
		if err != nil || len(m) != 2000 {
			b.Fatalf("%d rows, %v", len(m), err)
		}
	}
}
