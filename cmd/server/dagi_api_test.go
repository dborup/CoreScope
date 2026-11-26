package main

// #740 M2 — the DAGI filters in the API, /api/config/dagi and the polygon
// endpoint.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

func pkDagi(c string) string { return strings.Repeat(c, 64) }

// The seeded nodes. Positions are checked against the lookup in
// TestDagiSeedPlaces, so a test failing below is about the API, not the
// seed.
var dagiSeedNodes = []struct {
	pk, name string
	lat, lon interface{}
	obs      int // the observer (rowid) that hears its advert: 1 = AAR, 2 = CPH
}{
	{pkDagi("1"), "Aarhus C", 56.1567, 10.2108, 1},          // 0751, Østjylland, 1082, 8000
	{pkDagi("2"), "Aarhus N", 56.1800, 10.2000, 2},          // 0751, 8200
	{pkDagi("3"), "Kbh K", 55.6804, 12.5857, 2},             // 0101, Byen København, 1084, 1050 (street group)
	{pkDagi("4"), "Frederiksberg C", 55.68003, 12.54256, 2}, // 0147, 1084, 1800 group
	{pkDagi("5"), "Odense", 55.3959, 10.3883, 1},            // 0461, Fyn, 1083, 5000
	{pkDagi("6"), "Off Hvide Sande", 56.0, 7.0, 1},          // only postnummer 6960 (sea)
	{pkDagi("7"), "Hvide Sande", 56.0045, 8.1294, 1},        // 0760, 6960
	{pkDagi("8"), "No GPS", nil, nil, 1},                    // unpositioned
	{pkDagi("9"), "Zero", 0.0, 0.0, 1},                      // unpositioned (0/0)
	{pkDagi("a"), "Blacklisted no GPS", nil, nil, 1},        // unpositioned, blacklisted: not counted
	{pkDagi("b"), "zzHidden no GPS", nil, nil, 1},           // unpositioned, hidden name: not counted
	{pkDagi("c"), "Flensburg", 54.7833, 9.4333, 2},          // abroad
}

// dagiSeedArea is a configured area around Aarhus C only (not Aarhus N).
var dagiSeedArea = AreaEntry{Label: "Aarhus C box", Polygon: [][2]float64{{56.14, 10.19}, {56.17, 10.19}, {56.17, 10.23}, {56.14, 10.23}}}

func dagiSeedConfig() *Config {
	return &Config{
		Areas:              map[string]AreaEntry{"AARC": dagiSeedArea},
		NodeBlacklist:      []string{pkDagi("a")},
		HiddenNamePrefixes: []string{"zzHidden"},
	}
}

// seedDagi writes the nodes, two observers and one ADVERT per node (plus a
// GRP_TXT without a sender). Packet i has hash %016x of i+1.
func seedDagi(t *testing.T, db *DB) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.conn.Exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen) VALUES ('obsA', 'A', 'AAR', ?, '2026-01-01T00:00:00Z'), ('obsB', 'B', 'CPH', ?, '2026-01-01T00:00:00Z')`,
		now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for i, n := range dagiSeedNodes {
		seen := now.Add(-time.Duration(i+1) * time.Minute)
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?, ?, 'repeater', ?, ?, ?, '2026-01-01T00:00:00Z')`,
			n.pk, n.name, n.lat, n.lon, seen.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		dagiSeedPacket(t, db, i, fmt.Sprintf(`{"type":"ADVERT","pubKey":%q,"name":%q}`, n.pk, n.name), 4, n.pk, n.obs, seen)
	}
	dagiSeedPacket(t, db, len(dagiSeedNodes), `{"type":"GRP_TXT","channelHashHex":"AA"}`, 5, "", 1, now)
}

func dagiSeedPacket(t *testing.T, db *DB, i int, decoded string, typ int, from string, obs int, ts time.Time) {
	t.Helper()
	res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey) VALUES ('AA', ?, ?, 1, ?, ?, ?)`,
		fmt.Sprintf("%016x", i+1), ts.Format(time.RFC3339), typ, decoded, from)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?, ?, 5, -90, '[]', ?)`, id, obs, ts.Unix()); err != nil {
		t.Fatal(err)
	}
}

// dagiHash is the hash of the advert of seed node i.
func dagiHash(i int) string { return fmt.Sprintf("%016x", i+1) }

// newDagiServer is a server over the seed. mode "memory" serves packets
// from the store, "fallback" through the store's SQL history fallback.
func newDagiServer(t *testing.T, cfg *Config, mode string) (*Server, *mux.Router) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	seedDagi(t, db)
	srv := NewServer(db, cfg, NewHub())
	srv.store = NewPacketStore(db, nil)
	srv.store.config = cfg
	if err := srv.store.Load(); err != nil {
		t.Fatal(err)
	}
	if !srv.store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes not ready")
	}
	if mode == "fallback" {
		srv.store.oldestLoaded = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	} else {
		srv.store.oldestLoaded = ""
	}
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return srv, router
}

func dagiGet(t *testing.T, router http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func dagiGetJSON(t *testing.T, router http.Handler, path string, out interface{}) {
	t.Helper()
	w := dagiGet(t, router, path)
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

type dagiNodesPage struct {
	Nodes []struct {
		PublicKey string `json:"public_key"`
	} `json:"nodes"`
	Dagi *DagiFilterInfo `json:"dagi"`
}

func (p dagiNodesPage) keys() []string {
	out := []string{}
	for _, n := range p.Nodes {
		out = append(out, n.PublicKey)
	}
	sort.Strings(out)
	return out
}

func dagiKeys(idx ...int) []string {
	out := []string{}
	for _, i := range idx {
		out = append(out, dagiSeedNodes[i].pk)
	}
	sort.Strings(out)
	return out
}

type dagiPacketsPage struct {
	Total   int `json:"total"`
	Packets []struct {
		Hash string `json:"hash"`
	} `json:"packets"`
	Dagi *DagiFilterInfo `json:"dagi"`
}

func (p dagiPacketsPage) hashes() []string {
	out := []string{}
	for _, x := range p.Packets {
		out = append(out, x.Hash)
	}
	sort.Strings(out)
	return out
}

func dagiHashes(idx ...int) []string {
	out := []string{}
	for _, i := range idx {
		out = append(out, dagiHash(i))
	}
	sort.Strings(out)
	return out
}

func dagiAllHashes() []string {
	out := []string{}
	for i := 0; i <= len(dagiSeedNodes); i++ {
		out = append(out, dagiHash(i))
	}
	sort.Strings(out)
	return out
}

// The seed's positions are where the tests below assume they are.
func TestDagiSeedPlaces(t *testing.T) {
	idx := dagiTestIndex(t)
	want := []dagiCodes{
		{"0751", dagiLandsdelOestjylland, "1082", "8000"},
		{"0751", dagiLandsdelOestjylland, "1082", "8200"},
		{"0101", dagiLandsdelByenKbh, "1084", "1050"},
		{"0147", dagiLandsdelByenKbh, "1084", ""}, // postnummer: a Frederiksberg C street code, checked below
		{"0461", dagiLandsdelFyn, "1083", "5000"},
		{"", "", "", "6960"},
		{"0760", dagiLandsdelVestjylland, "1082", "6960"},
	}
	for i, w := range want {
		n := dagiSeedNodes[i]
		got := dagiCodesOf(idx.Lookup(n.lat.(float64), n.lon.(float64)))
		if i == 3 {
			key, ok := idx.layers[DagiPostnummer].filterKey(got.postnummer)
			if !ok || key != "1800" {
				t.Errorf("%s: postnummer %q has filter key %q, want the Frederiksberg C group 1800", n.name, got.postnummer, key)
			}
			got.postnummer = ""
		}
		if got != w {
			t.Errorf("%s: %+v, want %+v", n.name, got, w)
		}
	}
}

// The params are named after the DAGI entities and clash with no other
// query parameter the server reads (in particular not with region, the
// observer IATA filter).
func TestDagiParamNames(t *testing.T) {
	if dagiParams != [dagiLayerCount]string{"kommune", "landsdel", "dkregion", "postnr"} {
		t.Fatalf("dagiParams = %v", dagiParams)
	}
	used := map[string][]string{} // param → files reading it via .Get("…")
	fset := token.NewFileSet()
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "dagi") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Get" {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					used[v] = append(used[v], name)
				}
			}
			return true
		})
	}
	if len(used["region"]) == 0 || len(used["area"]) == 0 {
		t.Fatal("the scan found no region/area param: it is broken")
	}
	for _, p := range dagiParams {
		if files := used[p]; len(files) > 0 {
			t.Errorf("param %q is already read in %v", p, files)
		}
	}
}

// A request without DAGI params carries the plain area key, byte for byte;
// with them, the selection travels with the area and comes back intact.
func TestNodeScopeKey(t *testing.T) {
	for _, area := range []string{"", "BEL", "a b|c", "ÆØÅ"} {
		if got := nodeScopeKey(area, dagiSelection{}); got != area {
			t.Errorf("nodeScopeKey(%q, none) = %q", area, got)
		}
		if _, _, ok := splitNodeScopeKey(area); ok {
			t.Errorf("plain %q splits as a scope key", area)
		}
		for _, sel := range []dagiSelection{{"0751", "", "", ""}, {"", dagiLandsdelFyn, "", ""}, {"", "", "1084", ""}, {"", "", "", "8000"}, {"0751", "", "1082", "8000"}} {
			key := nodeScopeKey(area, sel)
			gotArea, gotSel, ok := splitNodeScopeKey(key)
			if !ok || gotArea != area || gotSel != sel {
				t.Errorf("split(nodeScopeKey(%q, %v)) = %q, %v, %v", area, sel, gotArea, gotSel, ok)
			}
		}
	}
	// Only nodeScopeKey's shape splits.
	for _, k := range []string{"a\x00b", "a\x00b\x00c\x00d", "a\x00b\x00c\x00d\x00e\x00f", "a\x00\x00\x00\x00"} {
		if _, _, ok := splitNodeScopeKey(k); ok {
			t.Errorf("%q splits", k)
		}
	}
	// Every selection has its own key (the analytics caches key on it).
	seen := map[string]dagiSelection{}
	for _, sel := range []dagiSelection{{"0751", "", "", ""}, {"", "0751", "", ""}, {"", "", "0751", ""}, {"", "", "", "0751"}, {"0751", "", "", "8000"}} {
		k := nodeScopeKey("", sel)
		if prev, dup := seen[k]; dup {
			t.Errorf("%v and %v share key %q", prev, sel, k)
		}
		seen[k] = sel
	}
	// An area param that holds the separator cannot smuggle a selection in:
	// it stays one (unknown) area.
	for _, raw := range []string{"\x00kommune=0751", "BEL\x00", "\x00"} {
		key := nodeScopeKey(raw, dagiSelection{})
		if _, _, ok := splitNodeScopeKey(key); ok {
			t.Errorf("area %q became a scope key %q", raw, key)
		}
		if key == raw {
			t.Errorf("area %q passed through with its separator", raw)
		}
	}
}

func TestDagiFilterKeys(t *testing.T) {
	idx := dagiTestIndex(t)
	cases := []struct {
		l          DagiLayer
		code, want string
		ok         bool
	}{
		{DagiKommune, "0751", "0751", true},
		{DagiKommune, "751", "", false},
		{DagiLandsdel, dagiLandsdelFyn, dagiLandsdelFyn, true},
		{DagiRegion, "1084", "1084", true},
		{DagiPostnummer, "8000", "8000", true},
		{DagiPostnummer, "1050", "1050", true},
		{DagiPostnummer, "1051", "1050", true},
		{DagiPostnummer, "1473", "1050", true},
		{DagiPostnummer, "1550", "1550", true},
		{DagiPostnummer, "1799", "1550", true}, // the regular København V postcode joins its street postcodes
		{DagiPostnummer, "1974", "1800", true},
		{DagiPostnummer, "2000", "2000", true}, // Frederiksberg: not a street postcode
		{DagiPostnummer, "9999", "", false},
		{DagiPostnummer, "", "", false},
	}
	for _, c := range cases {
		got, ok := idx.layers[c.l].filterKey(c.code)
		if got != c.want || ok != c.ok {
			t.Errorf("%s %q: %q, %v; want %q, %v", c.l, c.code, got, ok, c.want, c.ok)
		}
	}
	groups := map[string]int{}
	for _, f := range idx.layers[DagiPostnummer].features {
		if f.key != f.area.Code {
			groups[f.key]++
		}
	}
	if !reflect.DeepEqual(groups, map[string]int{"1050": 231, "1550": 149, "1800": 104}) {
		t.Errorf("grouped postcodes (besides each group's key) = %v", groups)
	}
}

// The grouping rule on its own: every postcode named like a street
// postcode joins the group, a regular one too, and the lowest code is the
// key even when it is the regular one (generation 271 has no such case).
func TestDagiGroupStreetPostcodesRule(t *testing.T) {
	li := dagiLayerIndex{}
	for _, f := range []struct {
		code, name string
		street     bool
	}{{"0900", "By C", false}, {"0901", "By C", true}, {"0902", "By C", true}, {"0950", "Andet", false}, {"0960", "By V", true}} {
		li.features = append(li.features, dagiFeature{area: DagiArea{Code: f.code, Name: f.name}, key: f.code, street: f.street})
	}
	dagiGroupStreetPostcodes(&li)
	got := map[string]string{}
	for _, f := range li.features {
		got[f.area.Code] = f.key
		if f.grouped != (f.area.Name != "Andet") {
			t.Errorf("%s grouped %v", f.area.Code, f.grouped)
		}
	}
	if !reflect.DeepEqual(got, map[string]string{"0900": "0900", "0901": "0900", "0902": "0900", "0950": "0950", "0960": "0960"}) {
		t.Errorf("keys %v", got)
	}
}

// The postnummer filter matches a node only when it is also in a kommune:
// the sea node keeps 6960 as its place, but not in the filter.
func TestDagiMembershipPostnrNeedsKommune(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	m.Refresh([]nodeInfo{dagiTestNode("sea", 56.0, 7.0), dagiTestNode("land", 56.0045, 8.1294)})
	snap := m.Snapshot()
	if p, ok := snap.Place("sea"); !ok || p.Postnummer == nil || p.Postnummer.Code != "6960" || p.Kommune != nil {
		t.Errorf("Place(sea) = %+v, %v", dagiCodesOf(p), ok)
	}
	if snap.Contains(DagiPostnummer, "6960", "sea") || !snap.Contains(DagiPostnummer, "6960", "land") {
		t.Errorf("postnummer 6960 members = %v, want only land", snap.Members(DagiPostnummer, "6960"))
	}
}

// A street postcode's node is in its group's set, under the group key.
func TestDagiMembershipGroupsStreetPostcodes(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	m.Refresh([]nodeInfo{dagiTestNode("kbh", 55.6804, 12.5857), dagiTestNode("frb", 55.68003, 12.54256)})
	snap := m.Snapshot()
	if !snap.Contains(DagiPostnummer, "1050", "kbh") || !snap.Contains(DagiPostnummer, "1800", "frb") {
		t.Errorf("group sets: 1050 %v, 1800 %v", snap.Members(DagiPostnummer, "1050"), snap.Members(DagiPostnummer, "1800"))
	}
	if p, _ := snap.Place("frb"); p.Postnummer == nil || p.Postnummer.Code == "1800" || snap.Members(DagiPostnummer, p.Postnummer.Code) != nil {
		t.Errorf("frb's own street code %+v has a set of its own", p.Postnummer)
	}
}

// One snapshot is one state: a later refresh publishes a new one and
// leaves the held one as it was.
func TestDagiMembershipSnapshotIsOneState(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	m.Refresh([]nodeInfo{dagiTestNode("a", 56.1567, 10.2108), {PublicKey: "x", Name: "x"}})
	held := m.Snapshot()
	m.Refresh([]nodeInfo{dagiTestNode("a", 55.3959, 10.3883)})
	if !held.Contains(DagiKommune, "0751", "a") || held.Contains(DagiKommune, "0461", "a") || held.Unpositioned() != 1 {
		t.Error("the held snapshot changed")
	}
	if now := m.Snapshot(); now == held || !now.Contains(DagiKommune, "0461", "a") || now.Unpositioned() != 0 {
		t.Error("the refresh was not published")
	}
}

func TestDagiMembershipRefreshIfMoved(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	base := []nodeInfo{dagiTestNode("a", 56.1567, 10.2108), dagiTestNode("b", 55.3959, 10.3883), {PublicKey: "c", Name: "c"}}
	base[0].Name, base[1].Name = "A", "B"
	clone := func() []nodeInfo { return append([]nodeInfo(nil), base...) }
	if _, did := m.RefreshIfMoved(clone()); !did {
		t.Fatal("the first call must refresh")
	}
	first := m.Snapshot()
	if _, did := m.RefreshIfMoved(clone()); did {
		t.Error("refreshed with nothing changed")
	}
	rev := clone()
	rev[0], rev[2] = rev[2], rev[0]
	if _, did := m.RefreshIfMoved(rev); did || m.Snapshot() != first {
		t.Error("refreshed on a reorder")
	}
	changes := map[string]func([]nodeInfo) []nodeInfo{
		"moved":      func(n []nodeInfo) []nodeInfo { n[1].Lat += 1e-9; return n },
		"lon moved":  func(n []nodeInfo) []nodeInfo { n[1].Lon += 1e-9; return n },
		"renamed":    func(n []nodeInfo) []nodeInfo { n[2].Name = "zz"; return n },
		"lost GPS":   func(n []nodeInfo) []nodeInfo { n[0].HasGPS = false; return n },
		"added":      func(n []nodeInfo) []nodeInfo { return append(n, dagiTestNode("d", 55.0, 9.0)) },
		"removed":    func(n []nodeInfo) []nodeInfo { return n[:2] },
		"duplicated": func(n []nodeInfo) []nodeInfo { return append(n, n[0], n[0]) },
		"rekeyed":    func(n []nodeInfo) []nodeInfo { n[0].PublicKey = "a2"; return n },
	}
	for name, change := range changes {
		m.Refresh(clone())
		if _, did := m.RefreshIfMoved(change(clone())); !did {
			t.Errorf("%s: not refreshed", name)
		}
	}
	// Refresh records the fingerprint too.
	m.Refresh(clone())
	if _, did := m.RefreshIfMoved(clone()); did {
		t.Error("RefreshIfMoved after Refresh of the same nodes refreshed again")
	}
}

// The store refreshes the cache when it publishes a node snapshot, and
// only when a node moved: a move between kommuner moves the node's
// membership; an unchanged rebuild keeps the published snapshot.
func TestDagiStoreRefreshesOnNodeSnapshot(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	mustExecDB(t, db, `INSERT INTO nodes (public_key, name, lat, lon) VALUES ('mover', 'mover', 56.1567, 10.2108), ('still', 'still', 55.3959, 10.3883)`)
	store := NewPacketStore(db, nil)
	store.warmDagi() // startup: the cache is built before any request
	m := store.dagiMembership()
	if m == nil {
		t.Fatal("no membership with DAGI on by default")
	}
	first := m.Snapshot()
	if !first.Contains(DagiKommune, "0751", "mover") || !first.Contains(DagiKommune, "0461", "still") {
		t.Fatalf("startup refresh missing: 0751 %v, 0461 %v", first.Members(DagiKommune, "0751"), first.Members(DagiKommune, "0461"))
	}
	store.getCachedNodesAndPM() // cached snapshot: nothing rebuilt
	store.InvalidateNodeCache()
	store.getCachedNodesAndPM() // rebuilt, nothing moved
	if m.Snapshot() != first {
		t.Error("an unchanged node snapshot refreshed the cache")
	}
	mustExecDB(t, db, `UPDATE nodes SET lat = 55.3959, lon = 10.3883 WHERE public_key = 'mover'`)
	store.getCachedNodesAndPM()
	if !m.Snapshot().Contains(DagiKommune, "0751", "mover") {
		t.Error("the cached node snapshot (30 s) was bypassed")
	}
	store.InvalidateNodeCache()
	store.getCachedNodesAndPM()
	now := m.Snapshot()
	if now.Contains(DagiKommune, "0751", "mover") || !now.Contains(DagiKommune, "0461", "mover") {
		t.Errorf("after the move: 0751 %v, 0461 %v", now.Members(DagiKommune, "0751"), now.Members(DagiKommune, "0461"))
	}
	// dagiSnapshot (what the filters read) is that state.
	if store.dagiSnapshot() != now {
		t.Error("dagiSnapshot is not the published snapshot")
	}
}

// The response shape M3 builds on (AGENTS.md rule 4).
func TestDagiConfigEndpointShape(t *testing.T) {
	_, router := newDagiServer(t, dagiSeedConfig(), "memory")
	w := dagiGet(t, router, "/api/config/dagi")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"credit", "enabled", "layers", "source"}) {
		t.Errorf("top-level keys %v", keys)
	}
	var got DagiConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	idx := dagiTestIndex(t)
	if !got.Enabled || got.Credit != idx.credit || got.Credit == "" {
		t.Errorf("enabled %v credit %q", got.Enabled, got.Credit)
	}
	if got.Source.License != "CC BY 4.0" || got.Source.TermsURL != "https://dataforsyningen.dk/Vilkaar" ||
		got.Source.Generation != 271 || got.Source.Downloaded != "2026-11-26" || got.Source.Owner != "Klimadatastyrelsen" {
		t.Errorf("source %+v", got.Source)
	}
	if len(got.Layers) != 4 {
		t.Fatalf("%d layers", len(got.Layers))
	}
	wantLayers := []struct {
		layer, param string
		n            int
	}{{"kommune", "kommune", 99}, {"landsdel", "landsdel", 11}, {"region", "dkregion", 5}, {"postnummer", "postnr", 603 - 1 + 3}}
	for i, wl := range wantLayers {
		l := got.Layers[i]
		if l.Layer != wl.layer || l.Param != wl.param || len(l.Areas) != wl.n {
			t.Errorf("layer %d: %s ?%s= %d entries, want %s ?%s= %d", i, l.Layer, l.Param, len(l.Areas), wl.layer, wl.param, wl.n)
		}
		if !sort.SliceIsSorted(l.Areas, func(a, b int) bool { return l.Areas[a].Code < l.Areas[b].Code }) {
			t.Errorf("%s: not sorted by code", l.Layer)
		}
		for _, a := range l.Areas {
			if a.Code == "" || a.Name == "" {
				t.Errorf("%s: entry %+v", l.Layer, a)
			}
		}
	}
	byCode := func(l int, code string) DagiConfigArea {
		for _, a := range got.Layers[l].Areas {
			if a.Code == code {
				return a
			}
		}
		t.Fatalf("layer %d has no %s", l, code)
		return DagiConfigArea{}
	}
	if a := byCode(0, "0751"); a.Name != "Aarhus" || a.OutsideKommune || a.Codes != nil {
		t.Errorf("0751 %+v", a)
	}
	if a := byCode(0, "0411"); !a.OutsideKommune {
		t.Errorf("Christiansø %+v", a)
	}
	if a := byCode(1, dagiLandsdelFyn); a.Name != "Fyn" {
		t.Errorf("Fyn %+v", a)
	}
	for code, want := range map[string]struct {
		name  string
		n     int
		has   string
		first string
	}{"1050": {"København K", 232, "1473", "1050"}, "1550": {"København V", 150, "1799", "1550"}, "1800": {"Frederiksberg C", 105, "1974", "1800"}} {
		a := byCode(3, code)
		if a.Name != want.name || len(a.Codes) != want.n || a.Codes[0] != want.first || !sort.StringsAreSorted(a.Codes) {
			t.Errorf("%s: %s with %d codes", code, a.Name, len(a.Codes))
		}
		found := false
		for _, c := range a.Codes {
			found = found || c == want.has
		}
		if !found {
			t.Errorf("%s: %s missing", code, want.has)
		}
	}
	if a := byCode(3, "8000"); a.Name != "Aarhus C" || a.Codes != nil {
		t.Errorf("8000 %+v", a)
	}
	for _, a := range got.Layers[3].Areas {
		if a.Code == "1051" || a.Code == "1799" {
			t.Errorf("grouped code %s listed on its own", a.Code)
		}
	}
}

func dagiOff() *Config {
	cfg := dagiSeedConfig()
	off := false
	cfg.Dagi = &DagiConfig{Enabled: &off}
	return cfg
}

// dagi.enabled=false: no lists, no polygons, every code unknown.
func TestDagiDisabled(t *testing.T) {
	srv, router := newDagiServer(t, dagiOff(), "memory")
	w := dagiGet(t, router, "/api/config/dagi")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":false,"layers":[]}` {
		t.Errorf("config: %d %s", w.Code, w.Body.String())
	}
	if w := dagiGet(t, router, "/api/config/dagi/polygons?layer=kommune"); w.Code != 404 {
		t.Errorf("polygons: %d", w.Code)
	}
	if srv.store.dagiMembership() != nil || srv.store.dagiPtr.Load() != nil {
		t.Error("the membership cache was built with DAGI off")
	}
	var nodes dagiNodesPage
	dagiGetJSON(t, router, "/api/nodes?kommune=0751", &nodes)
	if len(nodes.Nodes) != 0 {
		t.Errorf("nodes: %v", nodes.keys())
	}
	var pkts dagiPacketsPage
	dagiGetJSON(t, router, "/api/packets?kommune=0751&limit=100", &pkts)
	if !reflect.DeepEqual(pkts.hashes(), dagiAllHashes()) || pkts.Dagi != nil {
		t.Errorf("packets: %v %+v", pkts.hashes(), pkts.Dagi)
	}
	if w := dagiGet(t, router, "/api/analytics/rf?kommune=0751"); w.Code != 400 {
		t.Errorf("rf: %d", w.Code)
	}
}

func TestDagiConfigEnabledKey(t *testing.T) {
	cases := []struct {
		cfg  string
		want bool
	}{
		{`{}`, true},
		{`{"dagi":{}}`, true},
		{`{"dagi":{"enabled":true}}`, true},
		{`{"dagi":{"enabled":false}}`, false},
		{`{"Dagi":{"enabled":false}}`, true},   // not the canonical key: ignored
		{`{"dagi":{"Enabled":false}}`, true},   // not the canonical key: ignored
		{`{"dagi":{"enabled":"false"}}`, true}, // not a boolean
	}
	for _, c := range cases {
		cfg, _ := loadConfigLogged(t, c.cfg)
		if got := cfg.DagiEnabled(); got != c.want {
			t.Errorf("%s: DagiEnabled() = %v, want %v", c.cfg, got, c.want)
		}
	}
	var nilCfg *Config
	if !nilCfg.DagiEnabled() {
		t.Error("a nil config must default to on")
	}
}

func TestDagiPolygonsEndpoint(t *testing.T) {
	_, router := newDagiServer(t, dagiSeedConfig(), "memory")
	idx := dagiTestIndex(t)
	var one DagiPolygonsResponse
	w := dagiGet(t, router, "/api/config/dagi/polygons?layer=kommune&code=0751")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	aarhus := idx.layers[DagiKommune].features[idx.layers[DagiKommune].byCode["0751"]]
	if one.Layer != "kommune" || one.Param != "kommune" || one.Credit != idx.credit || one.Quantum != idx.quantum ||
		len(one.Features) != 1 || one.Features[0].Code != "0751" || one.Features[0].Name != "Aarhus" ||
		!reflect.DeepEqual(one.Features[0].Rings, aarhus.rings) || one.Features[0].Group != "" || one.Encoding == "" {
		t.Errorf("one kommune: %+v", one)
	}
	cc, tag := w.Header().Get("Cache-Control"), w.Header().Get("ETag")
	if cc != "public, max-age=86400" || !strings.HasPrefix(tag, `"`) || len(tag) < 10 {
		t.Errorf("Cache-Control %q ETag %q", cc, tag)
	}
	if w2 := dagiGet(t, router, "/api/config/dagi/polygons?layer=kommune&code=0751", "If-None-Match", tag); w2.Code != 304 || w2.Body.Len() != 0 {
		t.Errorf("revalidation: %d, %d bytes", w2.Code, w2.Body.Len())
	}
	if w2 := dagiGet(t, router, "/api/config/dagi/polygons?layer=kommune&code=0101"); w2.Header().Get("ETag") == tag {
		t.Error("two boundaries share an ETag")
	}
	// A postcode group: every member, each with its group; an alias asks
	// for the same group.
	var grp DagiPolygonsResponse
	dagiGetJSON(t, router, "/api/config/dagi/polygons?layer=postnummer&code=1051", &grp)
	if grp.Param != "postnr" || len(grp.Features) != 232 {
		t.Fatalf("group 1050: %d features", len(grp.Features))
	}
	for _, f := range grp.Features {
		if f.Group != "1050" || f.Name != "København K" {
			t.Errorf("member %+v", f.Code)
		}
	}
	var whole DagiPolygonsResponse
	dagiGetJSON(t, router, "/api/config/dagi/polygons?layer=region", &whole)
	if len(whole.Features) != 5 || whole.Param != "dkregion" {
		t.Errorf("region layer: %d", len(whole.Features))
	}
	for _, bad := range []struct {
		q    string
		code int
	}{{"", 400}, {"layer=", 400}, {"layer=dkregion", 400}, {"layer=Kommune", 400}, {"layer=kommune&code=9999", 404}, {"layer=kommune&code=", 400}} {
		if w := dagiGet(t, router, "/api/config/dagi/polygons?"+bad.q); w.Code != bad.code {
			t.Errorf("%q: %d, want %d", bad.q, w.Code, bad.code)
		}
	}
	// gzip-friendly: the whole kommune layer is under the M1 wire budget.
	gz := gzipMiddleware(router)
	wz := dagiGet(t, gz, "/api/config/dagi/polygons?layer=kommune", "Accept-Encoding", "gzip")
	if wz.Code != 200 || wz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip: %d %q", wz.Code, wz.Header().Get("Content-Encoding"))
	}
	if wz.Body.Len() > 150*1024 {
		t.Errorf("kommune layer: %d bytes gzipped, budget 150 KiB", wz.Body.Len())
	}
	zr, err := gzip.NewReader(bytes.NewReader(wz.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	var all DagiPolygonsResponse
	if err := json.Unmarshal(body, &all); err != nil || len(all.Features) != 99 {
		t.Errorf("kommune layer: %v, %d features", err, len(all.Features))
	}
	t.Logf("kommune layer: %d bytes, %d gzipped", len(body), wz.Body.Len())
}

func TestDagiNodesFilter(t *testing.T) {
	_, router := newDagiServer(t, dagiSeedConfig(), "memory")
	cases := []struct {
		q    string
		want []string
	}{
		{"kommune=0751", dagiKeys(0, 1)},
		{"landsdel=" + dagiLandsdelOestjylland, dagiKeys(0, 1)},
		{"dkregion=1084", dagiKeys(2, 3)},
		{"dkregion=1082", dagiKeys(0, 1, 6)},
		{"postnr=8000", dagiKeys(0)},
		{"postnr=1051", dagiKeys(2)}, // alias of the København K group
		{"postnr=1050", dagiKeys(2)},
		{"postnr=1974", dagiKeys(3)},
		{"postnr=6960", dagiKeys(6)}, // not the sea node
		{"kommune=0751&postnr=8200", dagiKeys(1)},
		{"kommune=0751&dkregion=1084", nil},
		{"kommune=0860", nil}, // known, nobody there
		{"kommune=9999", nil}, // unknown: matches nothing, as an unknown area does here
		{"kommune=0751&postnr=9999", nil},
		{"kommune=0751,0101", nil}, // single-select: a list is no code
		{"area=AARC&kommune=0751", dagiKeys(0)},
		{"area=AARC&kommune=0101", nil},
		{"area=AARC", dagiKeys(0)},
		{"area=NOPE&kommune=0751", nil},
		{"role=companion&kommune=0751", nil},
		{"area=%000751%00%00%00", nil}, // shaped like a scope key: one unknown area
	}
	for _, c := range cases {
		var page dagiNodesPage
		dagiGetJSON(t, router, "/api/nodes?limit=100&"+c.q, &page)
		want := c.want
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(page.keys(), want) {
			t.Errorf("%s: %v, want %v", c.q, page.keys(), want)
		}
		hasDagi := strings.Contains(c.q, "kommune=") || strings.Contains(c.q, "postnr=") || strings.Contains(c.q, "dkregion=") || strings.Contains(c.q, "landsdel=")
		if hasDagi != (page.Dagi != nil) {
			t.Errorf("%s: dagi info %+v", c.q, page.Dagi)
		}
		// No position: 8 and 9; a and b are hidden identities, never counted.
		if page.Dagi != nil && page.Dagi.UnpositionedHidden != 2 {
			t.Errorf("%s: unpositionedHidden = %d, want 2", c.q, page.Dagi.UnpositionedHidden)
		}
	}
	// Without a DAGI filter the unpositioned nodes are listed.
	var all dagiNodesPage
	dagiGetJSON(t, router, "/api/nodes?limit=100", &all)
	if !reflect.DeepEqual(all.keys(), dagiKeys(0, 1, 2, 3, 4, 5, 6, 7, 8, 11)) || all.Dagi != nil {
		t.Errorf("unfiltered: %v %+v", all.keys(), all.Dagi)
	}
	// An empty param is no filter.
	var empty dagiNodesPage
	dagiGetJSON(t, router, "/api/nodes?limit=100&kommune=&postnr=", &empty)
	if !reflect.DeepEqual(empty.keys(), all.keys()) || empty.Dagi != nil {
		t.Errorf("empty params: %v", empty.keys())
	}
}

// With no other post-filter configured (no blacklist, no hidden prefix, no
// geo filter) the DAGI filter alone must still take the filtering path.
func TestDagiNodesFilterAlone(t *testing.T) {
	_, router := newDagiServer(t, &Config{}, "memory")
	var page dagiNodesPage
	dagiGetJSON(t, router, "/api/nodes?limit=100&kommune=0751", &page)
	if !reflect.DeepEqual(page.keys(), dagiKeys(0, 1)) || page.Dagi == nil || page.Dagi.UnpositionedHidden != 4 {
		t.Errorf("kommune=0751 alone: %v %+v", page.keys(), page.Dagi)
	}
}

// The hidden count is the visible nodes of one membership snapshot that
// have no usable position.
func TestDagiUnpositionedVisible(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	m.Refresh([]nodeInfo{
		{PublicKey: "nogps", Name: "n"},
		{PublicKey: "zero", Name: "z", HasGPS: true},
		{PublicKey: "NoGPSBlack", Name: "b"},
		{PublicKey: "hidden", Name: "zzHidden x"},
		dagiTestNode("ok", 56.1567, 10.2108),
	})
	cfg := &Config{NodeBlacklist: []string{"nogpsblack"}, HiddenNamePrefixes: []string{"zzHidden"}}
	if got := m.Snapshot().unpositionedVisible(cfg); got != 2 {
		t.Errorf("unpositionedVisible = %d, want 2", got)
	}
	if got := m.Snapshot().unpositionedVisible(nil); got != 4 {
		t.Errorf("unpositionedVisible(nil config) = %d, want 4", got)
	}
}

func TestDagiPacketsFilter(t *testing.T) {
	for _, mode := range []string{"memory", "fallback"} {
		_, router := newDagiServer(t, dagiSeedConfig(), mode)
		window := ""
		if mode == "fallback" {
			window = "&since=2000-01-01T00:00:00Z"
		}
		cases := []struct {
			q    string
			want []string
			info bool
		}{
			{"kommune=0751", dagiHashes(0, 1), true},
			{"dkregion=1084", dagiHashes(2, 3), true},
			{"landsdel=" + dagiLandsdelFyn, dagiHashes(4), true},
			{"postnr=1473", dagiHashes(2), true},
			{"postnr=6960", dagiHashes(6), true},
			{"kommune=0860", []string{}, true}, // known, nobody there: nothing, not everything
			{"kommune=0751&region=AAR", dagiHashes(0), true},
			{"kommune=0751&region=CPH", dagiHashes(1), true},
			{"kommune=0751&area=AARC", dagiHashes(0), true},
			{"kommune=0101&area=AARC", []string{}, true},
			{"kommune=0751&area=NOPE", dagiHashes(0, 1), true}, // unknown area: no area filter, as without DAGI
			{"kommune=9999", dagiAllHashes(), false},           // unknown code: no filter, as an unknown area
			{"kommune=9999&postnr=8000", dagiHashes(0), true},
			{"area=AARC", dagiHashes(0), false},
			{"area=NOPE", dagiAllHashes(), false},
			{"kommune=", dagiAllHashes(), false},
			// An area param shaped like a scope key is one unknown area.
			{"area=%000751%00%00%00", dagiAllHashes(), false},
		}
		for _, grouped := range []bool{false, true} {
			for _, c := range cases {
				q := fmt.Sprintf("/api/packets?limit=100&groupByHash=%t%s&%s", grouped, window, c.q)
				var page dagiPacketsPage
				dagiGetJSON(t, router, q, &page)
				if !reflect.DeepEqual(page.hashes(), c.want) || page.Total != len(c.want) {
					t.Errorf("%s %s: total %d %v, want %v", mode, q, page.Total, page.hashes(), c.want)
				}
				if c.info != (page.Dagi != nil) || (page.Dagi != nil && page.Dagi.UnpositionedHidden != 2) {
					t.Errorf("%s %s: dagi info %+v", mode, q, page.Dagi)
				}
			}
		}
	}
}

// Without DAGI params, area behaves exactly as before: the same rows for
// every area key, DAGI on or off.
func TestDagiAreaUnchangedWithoutDagiParams(t *testing.T) {
	_, on := newDagiServer(t, dagiSeedConfig(), "memory")
	_, off := newDagiServer(t, dagiOff(), "memory")
	for _, area := range []string{"", "AARC", "NOPE"} {
		for _, path := range []string{"/api/packets?limit=100", "/api/packets?limit=100&groupByHash=true", "/api/nodes?limit=100", "/api/analytics/rf", "/api/analytics/hash-sizes"} {
			q := path
			if area != "" {
				q += "&area=" + area
				if !strings.Contains(path, "?") {
					q = path + "?area=" + area
				}
			}
			a, b := dagiGet(t, on, q), dagiGet(t, off, q)
			if a.Code != b.Code || dagiStripVolatile(a.Body.String()) != dagiStripVolatile(b.Body.String()) {
				t.Errorf("%s: DAGI on %d / off %d differ:\n%s\n%s", q, a.Code, b.Code, a.Body.String(), b.Body.String())
			}
			if strings.Contains(a.Body.String(), `"dagi"`) {
				t.Errorf("%s: a dagi block without a DAGI param", q)
			}
		}
	}
	// The area keys the store sees are the param itself.
	if nodeScopeKey("AARC", dagiSelection{}) != "AARC" {
		t.Error("the area key changed")
	}
	// Requests without DAGI params do no DAGI work: the cache is never
	// built.
	srv, fresh := newDagiServer(t, dagiSeedConfig(), "memory")
	for _, q := range []string{"/api/packets?limit=5", "/api/packets?groupByHash=true", "/api/nodes", "/api/analytics/rf", "/api/nodes/bulk-health", "/api/packets?area=AARC&kommune="} {
		if w := dagiGet(t, fresh, q); w.Code != 200 {
			t.Fatalf("%s: %d", q, w.Code)
		}
	}
	if srv.store.dagiPtr.Load() != nil {
		t.Error("a request without DAGI params built the DAGI cache")
	}
}

// A packet query filters with the snapshot the handler pinned, not a
// newer one: the list and its hidden count come from one state.
func TestDagiPacketQueryUsesPinnedSnapshot(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	mustExecDB(t, db, `INSERT INTO nodes (public_key, name, lat, lon) VALUES ('mover', 'mover', 56.1567, 10.2108)`)
	store := NewPacketStore(db, nil)
	pinned := store.dagiSnapshot()
	mustExecDB(t, db, `UPDATE nodes SET lat = 55.3959, lon = 10.3883 WHERE public_key = 'mover'`)
	store.InvalidateNodeCache()
	if store.dagiSnapshot() == pinned {
		t.Fatal("the move was not published")
	}
	q := PacketQuery{Area: nodeScopeKey("", dagiSelection{"0751"}), dagiSnap: pinned}
	if got := store.packetAreaNodes(q); !got["mover"] {
		t.Errorf("pinned snapshot: %v, want mover in 0751", got)
	}
	q.dagiSnap = nil
	if got := store.packetAreaNodes(q); got["mover"] || got == nil {
		t.Errorf("live snapshot: %v, want an empty set", got)
	}
}

// dagiStripVolatile drops what differs between two servers by time alone.
func dagiStripVolatile(s string) string {
	for _, k := range []string{`"generated_at":"`, `"computedAt":"`, `"cached_at":"`} {
		for {
			i := strings.Index(s, k)
			if i < 0 {
				break
			}
			j := strings.Index(s[i+len(k):], `"`)
			s = s[:i] + s[i+len(k)+j+1:]
		}
	}
	return s
}

// Every endpoint that takes area takes the DAGI params too. The analytics
// endpoints answer an unknown code with 400, as they answer an unknown
// area; bulk-health and the fleet clock skew ignore it, as they ignore an
// unknown area.
func TestDagiAnalyticsEndpoints(t *testing.T) {
	_, router := newDagiServer(t, dagiSeedConfig(), "memory")
	strict := []string{
		"/api/analytics/rf",
		"/api/analytics/topology",
		"/api/analytics/channels",
		"/api/analytics/distance",
		"/api/analytics/hash-sizes",
		"/api/analytics/hash-collisions",
		"/api/analytics/advert-intervals",
		"/api/analytics/advert-intervals/bucket?class=flood&interval_s=3600&snapped=true",
	}
	for _, p := range strict {
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		for param, code := range map[string]string{"kommune": "9999", "landsdel": "1", "dkregion": "0751", "postnr": "0000"} {
			w := dagiGet(t, router, p+sep+param+"="+code)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "unknown "+param) {
				t.Errorf("%s ?%s=%s: %d %s", p, param, code, w.Code, w.Body.String())
			}
		}
		// The area check comes first, as before.
		if w := dagiGet(t, router, p+sep+"area=NOPE&kommune=9999"); w.Code != 400 || !strings.Contains(w.Body.String(), "unknown area") {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
		if w := dagiGet(t, router, p+sep+"kommune=0751&postnr=1051"); w.Code == 400 {
			t.Errorf("%s known codes: %d %s", p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/api/nodes/bulk-health", "/api/nodes/clock-skew"} {
		if w := dagiGet(t, router, p+"?kommune=9999"); w.Code != 200 {
			t.Errorf("%s unknown code: %d", p, w.Code)
		}
	}
	// The filter reaches the computation and its cache key: RF counts the
	// observations of the matching senders' adverts only.
	rfCount := func(q string) int {
		var rf struct {
			TotalPackets int `json:"totalPackets"`
		}
		dagiGetJSON(t, router, "/api/analytics/rf"+q, &rf)
		return rf.TotalPackets
	}
	all, aarhus, kbh, empty := rfCount(""), rfCount("?kommune=0751"), rfCount("?dkregion=1084"), rfCount("?kommune=0860")
	if aarhus != 2 || kbh != 2 || empty != 0 || all <= aarhus {
		t.Errorf("rf totalPackets: all %d, 0751 %d, 1084 %d, 0860 %d", all, aarhus, kbh, empty)
	}
	if again := rfCount("?kommune=0751"); again != aarhus {
		t.Errorf("cached 0751: %d", again)
	}
	if both := rfCount("?kommune=0751&area=AARC"); both != 1 {
		t.Errorf("rf 0751 ∩ AARC: %d", both)
	}
	// bulk-health filters by the same membership.
	var health []struct {
		PublicKey string `json:"public_key"`
	}
	dagiGetJSON(t, router, "/api/nodes/bulk-health?limit=100&kommune=0751", &health)
	got := []string{}
	for _, h := range health {
		got = append(got, h.PublicKey)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, dagiKeys(0, 1)) {
		t.Errorf("bulk-health 0751: %v", got)
	}
}

// Perf (AGENTS.md rule 0): /api/packets with a kommune filter costs what
// the area filter costs, on 30K packets from 2K nodes: both are one set
// lookup per packet's sender.
func BenchmarkDagiPacketsFilter30K(b *testing.B) {
	srv := dagiPerfServer(b)
	for _, q := range []string{"area=AAR", "kommune=0751", "dkregion=1082", "postnr=8000"} {
		b.Run(q, func(b *testing.B) {
			req := httptest.NewRequest("GET", "/api/packets?limit=50&"+q, nil)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				srv.handlePackets(w, req)
				if w.Code != 200 {
					b.Fatal(w.Code)
				}
			}
		})
	}
}

// TestDagiPacketsFilterNoSlowerThanArea enforces it with a margin: the best
// of 5 kommune requests within 1.5× the best of 5 area requests (plus 2 ms
// for timer noise). A per-packet point-in-polygon test would cost about
// 30K × 2 µs = 60 ms and fail it.
func TestDagiPacketsFilterNoSlowerThanArea(t *testing.T) {
	if testing.Short() {
		t.Skip("perf")
	}
	srv := dagiPerfServer(t)
	best := func(q string) time.Duration {
		req := httptest.NewRequest("GET", "/api/packets?limit=50&"+q, nil)
		var min time.Duration
		for i := 0; i < 6; i++ {
			w := httptest.NewRecorder()
			start := time.Now()
			srv.handlePackets(w, req)
			d := time.Since(start)
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			if i > 0 && (min == 0 || d < min) { // the first run warms the caches
				min = d
			}
		}
		return min
	}
	// Both filters must match a real share of the packets, or the timings
	// compare nothing.
	totals := map[string]int{}
	for _, q := range []string{"area=AAR", "kommune=0751"} {
		w := httptest.NewRecorder()
		srv.handlePackets(w, httptest.NewRequest("GET", "/api/packets?limit=1&"+q, nil))
		var page dagiPacketsPage
		json.Unmarshal(w.Body.Bytes(), &page)
		if page.Total < 1000 || page.Total > 10000 {
			t.Fatalf("%s matches %d of 30000 packets", q, page.Total)
		}
		totals[q] = page.Total
	}
	area, kommune := best("area=AAR"), best("kommune=0751")
	t.Logf("/api/packets 30K: area %v (%d match), kommune %v (%d match)", area, totals["area=AAR"], kommune, totals["kommune=0751"])
	if kommune > area*3/2+2*time.Millisecond {
		t.Errorf("kommune %v vs area %v", kommune, area)
	}
}

// dagiPerfServer is 2K nodes across Denmark (a tenth of them in an
// Aarhus-sized box that is both the area AAR and mostly kommune 0751) and
// 30K ADVERT packets from them, in the store.
func dagiPerfServer(t testing.TB) *Server {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	nodes := dagiSyntheticNodes(2000, 11)
	for i := range nodes {
		if i%10 == 0 {
			nodes[i].Lat, nodes[i].Lon = 56.12+float64(i%7)*0.01, 10.12+float64(i%9)*0.02
		}
	}
	tx, _ := db.conn.Begin()
	for _, n := range nodes {
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, lat, lon) VALUES (?, ?, ?, ?)`, n.PublicKey, n.PublicKey[:8], n.Lat, n.Lon); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	cfg := &Config{Areas: map[string]AreaEntry{"AAR": {Label: "Aarhus", Polygon: [][2]float64{{56.11, 10.11}, {56.19, 10.11}, {56.19, 10.29}, {56.11, 10.29}}}}}
	srv := NewServer(db, cfg, NewHub())
	store := NewPacketStore(db, nil)
	store.config = cfg
	typ := 4
	for i := 0; i < 30000; i++ {
		pk := nodes[i%len(nodes)].PublicKey
		stamp := fmt.Sprintf("2026-01-01T%08d", i)
		tx := &StoreTx{ID: i + 1, Hash: fmt.Sprintf("%016x", i+1), FirstSeen: stamp, LatestSeen: stamp, PayloadType: &typ, PathJSON: "[]",
			DecodedJSON: fmt.Sprintf(`{"type":"ADVERT","pubKey":%q}`, pk)}
		store.packets = append(store.packets, tx)
		store.byHash[tx.Hash] = tx
	}
	srv.store = store
	return srv
}
