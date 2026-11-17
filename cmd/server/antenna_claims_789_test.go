package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
	"github.com/meshcore-analyzer/dhm"
)

// #789 M2a — owner antenna-height claims, the read-only server side: the
// gate, issuance (identical 404 for hidden and unknown nodes, height
// bounds, the global rate limit, the queue bound), the request status, the
// admin routes, and the owner source in the profile's precedence.

// Test fixture only: the production server never writes this table; the
// ingestor creates it (internal/dbschema) and is its only writer.
const claimTableDDL = `CREATE TABLE node_antenna_claims (
	request_id TEXT PRIMARY KEY, pubkey TEXT NOT NULL, height_m REAL NULL, code TEXT NOT NULL,
	status TEXT NOT NULL, needs_review INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL, proven_at INTEGER NULL, proof_hash TEXT NULL, proof_lat REAL NULL,
	proof_lon REAL NULL, reviewed_at INTEGER NULL, approved_by TEXT NULL)`

const claimAdminKey = "antenna-admin-key-strong-enough"

type claim789Env struct {
	*est789Env
	svc   *antennaClaimService
	queue *channelregistry.Queue
	now   time.Time
}

func newClaim789Env(t *testing.T, apiKey string, cc *antennaclaim.Config, radio *RadioConfig) *claim789Env {
	t.Helper()
	est := newEst789Env(t, &surfaceFake{surfaceM: 20}, radio)
	est.srv.cfg.APIKey = apiKey
	est.srv.cfg.AntennaClaims = cc
	if _, err := est.srv.db.conn.Exec(claimTableDDL); err != nil {
		t.Fatal(err)
	}
	env := &claim789Env{est789Env: est, now: time.UnixMilli(1_800_000_000_000)}
	env.svc = newAntennaClaimService(est.srv.cfg, est.srv.db.conn, filepath.Join(t.TempDir(), antennaclaim.QueueDirName))
	env.svc.now = func() time.Time { return env.now }
	est.srv.claims = env.svc
	env.queue = env.svc.queue
	return env
}

func claimsOn(mod func(*antennaclaim.Config)) *antennaclaim.Config {
	on := true
	c := &antennaclaim.Config{Enabled: &on}
	if mod != nil {
		mod(c)
	}
	return c
}

func (e *claim789Env) do(method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// post submits a claim, then advances the clock 1 ms so queued commands
// keep their submission order.
func (e *claim789Env) post(pk, body string) *httptest.ResponseRecorder {
	w := e.do("POST", "/api/nodes/"+pk+"/antenna-claims", body, "")
	e.now = e.now.Add(time.Millisecond)
	return w
}

func (e *claim789Env) queued(t *testing.T) []antennaclaim.Command {
	t.Helper()
	raw, err := e.queue.PendingValues()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]antennaclaim.Command, 0, len(raw))
	for _, rc := range raw {
		var c antennaclaim.Command
		if err := json.Unmarshal(rc.Data, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// insertClaim writes a claim row as the ingestor would (test fixture).
func (e *claim789Env) insertClaim(t *testing.T, id, pk string, h *float64, status string, lat, lon float64) {
	t.Helper()
	if _, err := e.srv.db.conn.Exec(`INSERT INTO node_antenna_claims
		(request_id, pubkey, height_m, code, status, created_at, expires_at, proven_at, proof_hash, proof_lat, proof_lon, reviewed_at, approved_by)
		VALUES (?, ?, ?, '7KX9QM', ?, ?, ?, ?, 'abcd', ?, ?, ?, CASE WHEN ? = 'approved' THEN 'admin' END)`,
		id, pk, h, status, e.now.UnixMilli(), e.now.Add(24*time.Hour).UnixMilli(), e.now.UnixMilli(), lat, lon, e.now.UnixMilli(), status); err != nil {
		t.Fatal(err)
	}
	e.svc.invalidateOwners()
}

// ── Gate ──────────────────────────────────────────────────────────────────

func TestAntennaClaims789_GateOffByDefault(t *testing.T) {
	pk := pk789("a")
	for name, tc := range map[string]struct {
		key string
		cc  *antennaclaim.Config
	}{
		"no config":           {claimAdminKey, nil},
		"enabled, no key":     {"", claimsOn(nil)},
		"enabled, weak key":   {"changeme", claimsOn(nil)},
		"enabled, short key":  {"short-key", claimsOn(nil)},
		"strong key, not set": {claimAdminKey, &antennaclaim.Config{}},
	} {
		t.Run(name, func(t *testing.T) {
			env := newClaim789Env(t, tc.key, tc.cc, nil)
			env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
			if w := env.post(pk, `{"heightM":18}`); w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", w.Code)
			}
			if n := len(env.queued(t)); n != 0 {
				t.Fatalf("%d commands queued while closed", n)
			}
		})
	}
	env := newClaim789Env(t, claimAdminKey, claimsOn(nil), nil)
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	if w := env.post(pk, `{"heightM":18}`); w.Code != http.StatusAccepted {
		t.Fatalf("open gate: %d %s", w.Code, w.Body)
	}
}

// ── Issuance ──────────────────────────────────────────────────────────────

func TestAntennaClaims789_IssueCode(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Aarhus Roof Repeater West", "repeater", est789Lat, est789Lon)
	t0 := env.now
	w := env.post(strings.ToUpper(pk), `{"heightM":18.5}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	var resp AntennaClaimIssuedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !channelregistry.ValidID(resp.RequestID) || !antennaclaim.ValidCode(resp.Code) {
		t.Fatalf("response %+v", resp)
	}
	if resp.ExpiresAt != t0.Add(24*time.Hour).UnixMilli() || resp.NameBudgetBytes != 23 {
		t.Fatalf("response %+v", resp)
	}
	if resp.SuggestedName != "Aarhus Roof Repe "+resp.Code || !antennaclaim.ContainsCode(resp.SuggestedName, 23, resp.Code) {
		t.Fatalf("suggested name %q", resp.SuggestedName)
	}
	cmds := env.queued(t)
	if len(cmds) != 1 {
		t.Fatalf("%d commands", len(cmds))
	}
	c := cmds[0]
	if c.Op != antennaclaim.OpClaim || c.RequestID != resp.RequestID || c.PubKey != pk || c.Code != resp.Code ||
		c.HeightM == nil || *c.HeightM != 18.5 || c.Clear || c.CreatedAt != t0.UnixMilli() {
		t.Fatalf("command %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("queued command does not validate: %v", err)
	}
	// A clear claim.
	if w := env.post(pk, `{"clear":true}`); w.Code != http.StatusAccepted {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if cmds := env.queued(t); len(cmds) != 2 || !cmds[1].Clear || cmds[1].HeightM != nil {
		t.Fatalf("clear command %+v", cmds)
	}
}

func TestAntennaClaims789_HiddenNodeIdenticalToUnknown(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(nil), nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	hidden, nogps, unknown := pk789("a"), pk789("b"), pk789("c")
	env.addNode(t, hidden, "private-roof", "repeater", est789Lat, est789Lon)
	env.addNode(t, nogps, "No position", "repeater", 0, 0)
	ref := env.post(unknown, `{"heightM":18}`)
	if ref.Code != http.StatusNotFound {
		t.Fatalf("unknown node: %d", ref.Code)
	}
	for name, pk := range map[string]string{"hidden": hidden, "no position": nogps} {
		w := env.post(pk, `{"heightM":18}`)
		if w.Code != ref.Code || w.Body.String() != ref.Body.String() || w.Header().Get("Content-Type") != ref.Header().Get("Content-Type") {
			t.Fatalf("%s: %d %q differs from unknown %d %q", name, w.Code, w.Body, ref.Code, ref.Body)
		}
	}
	if n := len(env.queued(t)); n != 0 {
		t.Fatalf("%d commands queued for hidden/unknown nodes", n)
	}
	if n := len(env.svc.recent); n != 0 {
		t.Fatalf("a 404 charged the budget (%d)", n)
	}
}

func TestAntennaClaims789_BodyAndHeightBounds(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	for _, body := range []string{
		`{"heightM":-0.1}`, `{"heightM":100.1}`, `{"heightM":150}`, `{"heightM":12.55}`,
		`{"heightM":"12"}`, `{"heightM":12,"clear":true}`, `{}`, `{"clear":false}`,
		`{"heightM":12,"note":"x"}`, `{"heightM":12} {"heightM":13}`, `not json`,
		`{"heightM":12,"pad":"` + strings.Repeat("x", 1100) + `"}`,
	} {
		if w := env.post(pk, body); w.Code != http.StatusBadRequest {
			t.Errorf("body %.40q: %d, want 400", body, w.Code)
		}
	}
	if w := env.post("not-a-key", `{"heightM":12}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad pubkey: %d", w.Code)
	}
	if n := len(env.svc.recent); n != 0 {
		t.Fatalf("bad bodies charged the budget (%d)", n)
	}
	for _, body := range []string{`{"heightM":0}`, `{"heightM":100}`, `{"heightM":0.1}`, `{"heightM":99.9}`} {
		if w := env.post(pk, body); w.Code != http.StatusAccepted {
			t.Errorf("body %s: %d %s", body, w.Code, w.Body)
		}
	}
}

func TestAntennaClaims789_RateLimit429(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(func(c *antennaclaim.Config) { c.SubmissionsPerHour = 2 }), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	for i := 0; i < 2; i++ {
		if w := env.post(pk, `{"heightM":18}`); w.Code != http.StatusAccepted {
			t.Fatalf("claim %d: %d", i, w.Code)
		}
	}
	w := env.post(pk, `{"heightM":18}`)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "3600" {
		t.Fatalf("third claim: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	env.now = env.now.Add(time.Hour + time.Second)
	if w := env.post(pk, `{"heightM":18}`); w.Code != http.StatusAccepted {
		t.Fatalf("after the window: %d", w.Code)
	}
}

func TestAntennaClaims789_QueueFull503Refunds(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(func(c *antennaclaim.Config) { c.MaxQueuedRequests = 1 }), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	if w := env.post(pk, `{"heightM":18}`); w.Code != http.StatusAccepted {
		t.Fatalf("first: %d", w.Code)
	}
	w := env.post(pk, `{"heightM":18}`)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("full queue: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if n := len(env.svc.recent); n != 1 {
		t.Fatalf("a refused enqueue kept its budget: %d charged, want 1", n)
	}
}

func TestAntennaClaims789_NeedsReviewFromCachedSurface(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	// Nothing cached yet: never flagged, and issuing never measures.
	env.post(pk, `{"heightM":95}`)
	if n := env.surface.calls.Load(); n != 0 {
		t.Fatalf("issuing a claim measured the surface (%d rasters)", n)
	}
	env.endA(t, pk) // a profile caches the 20 m surface
	env.post(pk, `{"heightM":95}`)
	env.post(pk, `{"heightM":22}`)
	env.post(pk, `{"heightM":10}`)
	cmds := env.queued(t)
	want := []bool{false, true, false, true}
	for i, c := range cmds {
		if c.NeedsReview != want[i] {
			t.Errorf("claim %d (%v m): needsReview %v, want %v", i, *c.HeightM, c.NeedsReview, want[i])
		}
	}
}

// ── Request status ────────────────────────────────────────────────────────

func TestAntennaClaims789_RequestStatus(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(nil), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	var issued AntennaClaimIssuedResponse
	json.Unmarshal(env.post(pk, `{"heightM":18}`).Body.Bytes(), &issued)
	get := func(id string) (int, AntennaClaimStatusResponse, string) {
		w := env.do("GET", "/api/antenna-claims/requests/"+id, "", "")
		var st AntennaClaimStatusResponse
		json.Unmarshal(w.Body.Bytes(), &st)
		return w.Code, st, w.Body.String()
	}
	if code, st, _ := get(issued.RequestID); code != 200 || st.Status != antennaclaim.RequestQueued {
		t.Fatalf("queued: %d %+v", code, st)
	}
	// The ingestor refused it.
	env.queue.CompleteValue(issued.RequestID, antennaclaim.Result{RequestID: issued.RequestID, Status: antennaclaim.RequestError, Error: "too many"})
	if code, st, _ := get(issued.RequestID); code != 200 || st.Status != antennaclaim.RequestError || st.Error != "too many" {
		t.Fatalf("error: %d %+v", code, st)
	}
	// Stored and proven: the row is the truth, not the result file.
	h := 18.0
	env.insertClaim(t, issued.RequestID, pk, &h, antennaclaim.StatusProven, est789Lat, est789Lon)
	if code, st, body := get(issued.RequestID); code != 200 || st.Status != antennaclaim.StatusProven || st.ExpiresAt == 0 || strings.Contains(body, "7KX9QM") {
		t.Fatalf("proven: %d %+v %s", code, st, body)
	}
	_, _, unknownBody := get("ffffffffffffffff")
	if code, _, _ := get("ffffffffffffffff"); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	if code, _, _ := get("nope"); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
	// The node becomes hidden: its claim is answered like an unknown one.
	env.srv.cfg.SetHiddenNamePrefixes([]string{"Roo"})
	if code, _, body := get(issued.RequestID); code != 404 || body != unknownBody {
		t.Fatalf("hidden node's claim: %d %q, want 404 %q", code, body, unknownBody)
	}
}

// ── Admin ─────────────────────────────────────────────────────────────────

func TestAntennaClaims789_AdminRoutes(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil) // public claims off: admin still works
	pk, hiddenPK := pk789("a"), pk789("b")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.addNode(t, hiddenPK, "private-x", "repeater", est789Lat, est789Lon)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	h := 18.0
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusProven, est789Lat, est789Lon)
	env.insertClaim(t, "a000000000000002", pk, &h, antennaclaim.StatusApproved, est789Lat+0.001, est789Lon)
	env.insertClaim(t, "a000000000000003", hiddenPK, &h, antennaclaim.StatusProven, est789Lat, est789Lon)

	for _, key := range []string{"", "wrong-key-wrong-key-wrong"} {
		if w := env.do("GET", "/api/admin/antenna-claims", "", key); w.Code != http.StatusUnauthorized {
			t.Fatalf("key %q: %d", key, w.Code)
		}
		if w := env.do("POST", "/api/admin/antenna-claims/a000000000000001/approve", "", key); w.Code != http.StatusUnauthorized {
			t.Fatalf("approve with key %q: %d", key, w.Code)
		}
	}
	w := env.do("GET", "/api/admin/antenna-claims", "", claimAdminKey)
	var list AdminAntennaClaimListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 {
		t.Fatalf("list %d %v", w.Code, err)
	}
	if len(list.Claims) != 2 || list.Enabled || list.AutoApprove {
		t.Fatalf("list %+v (the hidden node's claim must not be listed)", list)
	}
	byID := map[string]AdminAntennaClaim{}
	for _, c := range list.Claims {
		byID[c.ID] = c
	}
	a := byID["a000000000000002"]
	if a.Name != "Roof" || a.Role != "repeater" || a.PubKey != pk || a.HeightM == nil || *a.HeightM != 18 || a.MovedM == nil || *a.MovedM < 100 || *a.MovedM > 120 || !a.Stale {
		t.Fatalf("approved row %+v", a)
	}
	if strings.Contains(w.Body.String(), "7KX9QM") {
		t.Fatal("codes must not be listed")
	}
	if w := env.do("GET", "/api/admin/antenna-claims?status=proven", "", claimAdminKey); !strings.Contains(w.Body.String(), "a000000000000001") || strings.Contains(w.Body.String(), "a000000000000002") {
		t.Fatalf("status filter: %s", w.Body)
	}
	if w := env.do("GET", "/api/admin/antenna-claims?status=bogus", "", claimAdminKey); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}

	for _, tc := range []struct {
		id, op string
		code   int
	}{
		{"a000000000000001", "approve", 202},
		{"a000000000000001", "reject", 202},
		{"a000000000000001", "revoke", 409},  // not approved
		{"a000000000000002", "approve", 409}, // not proven
		{"a000000000000002", "revoke", 202},
		{"a000000000000003", "approve", 404}, // hidden: like unknown
		{"ffffffffffffffff", "approve", 404},
		{"nope", "approve", 400},
	} {
		w := env.do("POST", "/api/admin/antenna-claims/"+tc.id+"/"+tc.op, "", claimAdminKey)
		if w.Code != tc.code {
			t.Errorf("%s %s: %d, want %d (%s)", tc.op, tc.id, w.Code, tc.code, w.Body)
		}
	}
	cmds := env.queued(t)
	if len(cmds) != 3 {
		t.Fatalf("%d decisions queued, want 3", len(cmds))
	}
	for _, c := range cmds {
		if err := c.Validate(); err != nil || c.ClaimID == "" {
			t.Fatalf("decision %+v: %v", c, err)
		}
	}
}

// ── Precedence and the profile ────────────────────────────────────────────

func TestAntennaClaims789_PrecedenceWithOwner(t *testing.T) {
	pk := pk789("a")
	plain := &terrainProfiler{radio: resolveTerrainRadio(nil, func(string, ...interface{}) {})}
	op := &terrainProfiler{radio: resolveTerrainRadio(&RadioConfig{AntennaHeightsM: map[string]float64{
		pk: 18, "repeater": 15,
	}}, func(string, ...interface{}) {})}
	opRole := &terrainProfiler{radio: resolveTerrainRadio(&RadioConfig{AntennaHeightsM: map[string]float64{
		"repeater": 15,
	}}, func(string, ...interface{}) {})}
	node := func(role string, owner, s *float64) terrainEnd {
		return terrainEnd{kind: "node", role: role, pubkey: pk, owner: owner, surfaceM: s}
	}
	for _, tc := range []struct {
		name string
		p    *terrainProfiler
		e    terrainEnd
		ov   *float64
		h    float64
		src  string
	}{
		{"request beats owner", plain, node("repeater", floatp(30), nil), floatp(3), 3, "request"},
		{"operator-node beats owner", op, node("repeater", floatp(30), nil), nil, 18, "operator-node"},
		{"owner beats the estimate", plain, node("repeater", floatp(6), floatp(40)), nil, 6, "owner"},
		{"owner below the role default stands", plain, node("repeater", floatp(4), nil), nil, 4, "owner"},
		{"owner beats operator-role", opRole, node("repeater", floatp(8), nil), nil, 8, "owner"},
		{"owner for a companion", plain, node("companion", floatp(12), nil), nil, 12, "owner"},
		{"owner 0 m is a height", plain, node("room", floatp(0), nil), nil, 0, "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, src, assumed := tc.p.antenna(tc.e, tc.ov)
			if h != tc.h || src != tc.src || assumed {
				t.Fatalf("antenna = %v, %q, %v; want %v, %q, false", h, src, assumed, tc.h, tc.src)
			}
		})
	}
	// An owner height means no estimate is wanted.
	est := &terrainProfiler{estimate: terrainEstimator{src: &surfaceFake{}}, radio: plain.radio}
	if est.wantsEstimate(node("repeater", floatp(6), nil), nil) {
		t.Fatal("wantsEstimate with an owner height")
	}
	if !est.wantsEstimate(node("repeater", nil, nil), nil) {
		t.Fatal("wantsEstimate without an owner height")
	}
}

func TestAntennaClaims789_ProfileUsesOwnerHeight(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	h := 31.5
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusApproved, est789Lat, est789Lon)
	a := env.endA(t, pk)
	if a.AntennaM != 31.5 || a.AntennaSource != "owner" || a.Assumed {
		t.Fatalf("endpoint %+v, want 31.5 m owner", a)
	}
	if n := env.surface.calls.Load(); n != 0 {
		t.Fatalf("%d surface rasters for a node with an owner height", n)
	}
	// The card and /p resolve the same request: same height.
	req, rerr := env.srv.resolveTerrainRequest(context.Background(), env.p, terrainQuery{a: terrainRef{pubkey: pk}, b: terrainRef{pt: offsetPtLL(3000)}})
	if rerr != nil {
		t.Fatal(rerr.msg)
	}
	if got, src, _ := env.p.antenna(req.ends[0], nil); got != 31.5 || src != "owner" {
		t.Fatalf("shared request path: %v %q", got, src)
	}
}

func offsetPtLL(eastM float64) dhm.LatLon {
	e, n := dhm.ToUTM32(est789Lat, est789Lon)
	la, lo := dhm.FromUTM32(e+eastM, n)
	return dhm.LatLon{Lat: la, Lon: lo}
}

func TestAntennaClaims789_MovedNodeIsStale(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	h := 31.5
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusApproved, est789Lat, est789Lon)
	// 30 m north: still the owner's height.
	env.move(t, pk, est789Lat+30/111320.0, est789Lon)
	if a := env.endA(t, pk); a.AntennaSource != "owner" {
		t.Fatalf("after 30 m %+v, want owner", a)
	}
	// 60 m north: stale; the estimate (20 m surface + 2) is used instead.
	env.move(t, pk, est789Lat+60/111320.0, est789Lon)
	if a := env.endA(t, pk); a.AntennaSource != "dsm-estimate" || a.AntennaM != 22 {
		t.Fatalf("after 60 m %+v, want the 22 m estimate", a)
	}
}

func TestAntennaClaims789_OnlyApprovedHeightsServed(t *testing.T) {
	pk := pk789("a")
	h := 31.5
	for _, status := range []string{antennaclaim.StatusPending, antennaclaim.StatusProven, antennaclaim.StatusRejected,
		antennaclaim.StatusRevoked, antennaclaim.StatusExpired, antennaclaim.StatusSuperseded} {
		t.Run(status, func(t *testing.T) {
			env := newClaim789Env(t, claimAdminKey, nil, nil)
			env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
			env.insertClaim(t, "a000000000000001", pk, &h, status, est789Lat, est789Lon)
			if a := env.endA(t, pk); a.AntennaSource == "owner" {
				t.Fatalf("a %s claim was served: %+v", status, a)
			}
		})
	}
	// An approved clear (NULL height) is no height.
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.insertClaim(t, "a000000000000001", pk, nil, antennaclaim.StatusApproved, est789Lat, est789Lon)
	if a := env.endA(t, pk); a.AntennaSource == "owner" {
		t.Fatalf("a clear was served as a height: %+v", a)
	}
}

func TestAntennaClaims789_RevokeFallsBackAfterRefresh(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	h := 31.5
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusApproved, est789Lat, est789Lon)
	if a := env.endA(t, pk); a.AntennaSource != "owner" {
		t.Fatalf("%+v", a)
	}
	// The ingestor revokes it; the server sees it at the next refresh.
	env.srv.db.conn.Exec(`UPDATE node_antenna_claims SET status = 'revoked' WHERE request_id = 'a000000000000001'`)
	if a := env.endA(t, pk); a.AntennaSource != "owner" {
		t.Fatalf("snapshot refreshed before its TTL: %+v", a)
	}
	env.now = env.now.Add(ownerHeightsTTL + time.Second)
	if a := env.endA(t, pk); a.AntennaSource != "dsm-estimate" {
		t.Fatalf("after revoke and refresh %+v, want the estimate", a)
	}
}

func TestAntennaClaims789_HiddenNodeNoOwnerHeight(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	pk := pk789("a")
	env.addNode(t, pk, "private-roof", "repeater", est789Lat, est789Lon)
	h := 31.5
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusApproved, est789Lat, est789Lon)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	wantError(t, env.get(pathQuery(pk, offsetPt(est789Lat, est789Lon, 3000, 0))), 404, "node not found")
}

func TestAntennaClaims789_MissingTableIsEmpty(t *testing.T) {
	est := newEst789Env(t, &surfaceFake{surfaceM: 20}, nil)
	svc := newAntennaClaimService(est.srv.cfg, est.srv.db.conn, "")
	if h := svc.ownerHeights(context.Background()); len(h) != 0 {
		t.Fatalf("no table: %v", h)
	}
}

func TestAntennaClaims789_HeightCapIsTheProfileCap(t *testing.T) {
	if antennaclaim.MaxHeightM != terrainMaxAntennaM {
		t.Fatalf("claim cap %v != profile cap %v", antennaclaim.MaxHeightM, terrainMaxAntennaM)
	}
}

// F2: the server refuses at once (409) to approve a proof older than the
// claim already in force for the key; an equal or newer proof is accepted.
func TestAntennaClaims789_ApproveOlderProofConflict(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	pk, other := pk789("a"), pk789("c")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.addNode(t, other, "Mast", "repeater", est789Lat, est789Lon)
	h := 18.0
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusProven, est789Lat, est789Lon)
	env.insertClaim(t, "a000000000000002", pk, nil, antennaclaim.StatusApproved, est789Lat, est789Lon) // the owner's clear
	env.insertClaim(t, "a000000000000003", pk, &h, antennaclaim.StatusProven, est789Lat, est789Lon)
	env.insertClaim(t, "a000000000000004", other, &h, antennaclaim.StatusProven, est789Lat, est789Lon)
	base := env.now.UnixMilli()
	for id, at := range map[string]int64{"a000000000000001": base - 60_000, "a000000000000002": base, "a000000000000003": base + 60_000, "a000000000000004": base - 120_000} {
		if _, err := env.srv.db.conn.Exec(`UPDATE node_antenna_claims SET proven_at = ? WHERE request_id = ?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	w := env.do("POST", "/api/admin/antenna-claims/a000000000000001/approve", "", claimAdminKey)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), antennaclaim.ErrNewerApproved.Error()) {
		t.Fatalf("older proof: %d %s", w.Code, w.Body)
	}
	for _, id := range []string{"a000000000000003", "a000000000000004"} { // newer proof; another key
		if w := env.do("POST", "/api/admin/antenna-claims/"+id+"/approve", "", claimAdminKey); w.Code != http.StatusAccepted {
			t.Fatalf("approve %s: %d %s", id, w.Code, w.Body)
		}
	}
	if cmds := env.queued(t); len(cmds) != 2 {
		t.Fatalf("%d decisions queued, want 2", len(cmds))
	}
}
