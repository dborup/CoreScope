package main

// #740 M2 — DAGI in the API: filter params, the code lists and the
// polygons. No UI (M3).
//
// Params, one per layer, each single-select, ANDed with each other and
// with area and region:
//
//	?kommune=<kommunekode>   e.g. 0751
//	?landsdel=<id_lokalid>   e.g. 218546 (the asset has no NUTS code)
//	?dkregion=<regionskode>  e.g. 1082 (region is already the observer IATA filter)
//	?postnr=<postnummer>     e.g. 8000; a street postcode selects its group
//
// They filter by the sender's position, like area: nodes by their own,
// packets by the ADVERT sender's (packets without one are excluded), and
// the analytics through the same node set area uses. Membership comes from
// the per-node cache (dagi_membership.go), refreshed when the store
// publishes a node snapshot in which a node moved; a request reads one
// snapshot and does one set lookup per node or packet, never a
// point-in-polygon test.
//
// The selection travels with the area through the existing area plumbing:
// nodeScopeKey appends it to the area key, so every analytics cache key
// tells selections apart and every computation filters through the one
// resolveAreaNodes. A request without DAGI params carries the area key
// unchanged.
//
// Unknown codes behave like an unknown area on each endpoint: the
// analytics endpoints answer 400, /api/nodes matches nothing, and
// /api/packets, bulk-health and the fleet clock skew drop the filter. A
// known code nobody is in matches nothing everywhere (area's "an area with
// no members applies no filter" on packets is not copied: a DAGI code is
// never empty by mistake). Nodes without a usable position match no DAGI
// filter; /api/nodes and /api/packets report how many visible nodes that
// hid (dagi.unpositionedHidden), for M3's notice.
//
// Config (AGENTS.md rule 8): dagi.enabled, canonical key only, default on.
// Customizer items for M3: show the DAGI control at all, and the
// "N nodes without position hidden" notice.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// DagiConfig is config.json's "dagi" block.
type DagiConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// DagiEnabled reports whether dagi.enabled is on (missing: on). The
// filters also need the embedded asset to load (dagiIndexOrNil).
func (c *Config) DagiEnabled() bool {
	return c == nil || c.Dagi == nil || c.Dagi.Enabled == nil || *c.Dagi.Enabled
}

// dagiParams is each layer's query parameter.
var dagiParams = [dagiLayerCount]string{"kommune", "landsdel", "dkregion", "postnr"}

// dagiSelection is a request's DAGI filter: the filter key per layer, ""
// for none.
type dagiSelection [dagiLayerCount]string

func (d dagiSelection) active() bool { return d != dagiSelection{} }

// dagiRequest is a request's DAGI params against the index.
type dagiRequest struct {
	sel     dagiSelection // filter keys of the known codes
	asked   bool          // at least one DAGI param is non-empty
	unknown string        // the first param whose code is unknown, "" when none
}

// parseDagiRequest reads the DAGI params of q. With idx nil (DAGI off or
// the asset unusable) every code is unknown.
func parseDagiRequest(idx *dagiIndex, q url.Values) dagiRequest {
	var d dagiRequest
	for l, p := range dagiParams {
		code := q.Get(p)
		if code == "" {
			continue
		}
		d.asked = true
		var key string
		ok := false
		if idx != nil {
			key, ok = idx.layers[l].filterKey(code)
		}
		if !ok {
			if d.unknown == "" {
				d.unknown = p
			}
			continue
		}
		d.sel[l] = key
	}
	return d
}

// dagiRequest is parseDagiRequest against the server's index. A request
// without DAGI params never touches the index.
func (s *Server) dagiRequest(q url.Values) dagiRequest {
	for _, p := range dagiParams {
		if q.Get(p) != "" {
			return parseDagiRequest(s.dagiIndex(), q)
		}
	}
	return dagiRequest{}
}

// nodeScopeSep separates the area key from the DAGI selection in a node
// scope key. An area param holding it is no configured area; nodeScopeKey
// replaces it, so it can never be read as a selection.
const nodeScopeSep = "\x00"

// nodeScopeKey is the key the area plumbing carries for area plus sel: the
// area key itself when sel is empty, else area, then the four filter keys,
// separated by nodeScopeSep.
func nodeScopeKey(area string, sel dagiSelection) string {
	area = strings.ReplaceAll(area, nodeScopeSep, "\uFFFD")
	if !sel.active() {
		return area
	}
	return area + nodeScopeSep + strings.Join(sel[:], nodeScopeSep)
}

// splitNodeScopeKey undoes nodeScopeKey; ok is false for a plain area key.
func splitNodeScopeKey(key string) (area string, sel dagiSelection, ok bool) {
	if !strings.Contains(key, nodeScopeSep) {
		return key, sel, false
	}
	parts := strings.Split(key, nodeScopeSep)
	if len(parts) != 1+int(dagiLayerCount) {
		return key, sel, false
	}
	copy(sel[:], parts[1:])
	return parts[0], sel, sel.active()
}

// matching is the set of pubkeys every layer of sel matches: never nil, so
// a known code nobody is in filters everything out.
func (s *dagiMembershipSnapshot) matching(sel dagiSelection) map[string]bool {
	var sets []map[string]struct{}
	for l, key := range sel {
		if key != "" {
			sets = append(sets, s.Members(DagiLayer(l), key))
		}
	}
	out := map[string]bool{}
	if len(sets) == 0 {
		return out
	}
	sort.Slice(sets, func(i, j int) bool { return len(sets[i]) < len(sets[j]) })
next:
	for pk := range sets[0] {
		for _, other := range sets[1:] {
			if _, ok := other[pk]; !ok {
				continue next
			}
		}
		out[pk] = true
	}
	return out
}

// unpositionedVisible is how many nodes of the snapshot have no usable
// position, leaving out the identities the node list never shows
// (blacklisted keys, hidden name prefixes).
func (s *dagiMembershipSnapshot) unpositionedVisible(cfg *Config) int {
	n := 0
	for _, u := range s.unpositioned {
		if cfg != nil && (cfg.IsBlacklisted(u.pubkey) || cfg.IsNameHidden(u.name)) {
			continue
		}
		n++
	}
	return n
}

// DagiFilterInfo is reported beside a DAGI-filtered node or packet list.
type DagiFilterInfo struct {
	// UnpositionedHidden is how many nodes of the node list have no usable
	// position and so match no DAGI filter. It counts the whole list, not
	// only the nodes the request's other filters (role, search, area, …)
	// would keep.
	UnpositionedHidden int `json:"unpositionedHidden"`
}

var dagiLoadLog sync.Once

// dagiIndexOrNil is the embedded index, or nil when it does not load
// (logged once): then DAGI is off.
func dagiIndexOrNil() *dagiIndex {
	idx, err := dagiDefault()
	if err != nil {
		dagiLoadLog.Do(func() { log.Printf("[dagi] the embedded DAGI asset does not load, DAGI filters are off: %v", err) })
		return nil
	}
	return idx
}

// dagiIndex is the index the request's codes are checked against: nil when
// DAGI is off.
func (s *Server) dagiIndex() *dagiIndex {
	if !s.cfg.DagiEnabled() {
		return nil
	}
	return dagiIndexOrNil()
}

// dagiMembership is the store's cache, created on first use; nil when DAGI
// is off. Once created, every node snapshot the store publishes refreshes
// it (getCachedNodesAndPMOK).
func (s *PacketStore) dagiMembership() *dagiMembership {
	if !s.config.DagiEnabled() {
		return nil
	}
	s.dagiOnce.Do(func() {
		if idx := dagiIndexOrNil(); idx != nil {
			s.dagiPtr.Store(newDagiMembership(idx))
		}
	})
	return s.dagiPtr.Load()
}

// dagiSnapshot is the membership the filters read, as fresh as the node
// snapshot (30 s); nil when DAGI is off. Must not be called with s.mu held.
func (s *PacketStore) dagiSnapshot() *dagiMembershipSnapshot {
	m := s.dagiMembership()
	if m == nil {
		return nil
	}
	if s.db != nil {
		nodes, _, ok := s.getCachedNodesAndPMOK() // a rebuild refreshes m
		if ok && !m.done.Load() {
			// The node snapshot was published before the cache existed.
			m.RefreshIfMoved(nodes)
		}
	}
	return m.Snapshot()
}

// warmDagi builds the membership once at startup, so the first filtered
// request does not pay for the asset load and the first refresh.
func (s *PacketStore) warmDagi() {
	s.dagiSnapshot()
}

// refreshDagiOnPublish is the node snapshot's hook: a published snapshot
// refreshes an existing cache when a node moved. It reads no config, so
// background rebuilds race nothing.
func (s *PacketStore) refreshDagiOnPublish(nodes []nodeInfo) {
	if m := s.dagiPtr.Load(); m != nil {
		m.RefreshIfMoved(nodes)
	}
}

// scopeNodes resolves a node scope key's parts: the area's set (nil for an
// unknown or empty area, which then filters nothing, as before) ANDed with
// the selection's set from snap.
func (s *PacketStore) scopeNodes(area string, sel dagiSelection, snap *dagiMembershipSnapshot) map[string]bool {
	var areaSet map[string]bool
	if area != "" {
		areaSet = s.resolveAreaNodes(area)
	}
	if snap == nil {
		return areaSet
	}
	set := snap.matching(sel)
	if areaSet == nil {
		return set
	}
	for pk := range set {
		if !areaSet[pk] {
			delete(set, pk)
		}
	}
	return set
}

// packetAreaNodes is the node set a packet query filters senders by, from
// the membership snapshot the handler pinned (q.dagiSnap) when there is
// one, so the list and its hidden count come from one state.
func (s *PacketStore) packetAreaNodes(q PacketQuery) map[string]bool {
	if area, sel, ok := splitNodeScopeKey(q.Area); ok && q.dagiSnap != nil {
		return s.scopeNodes(area, sel, q.dagiSnap)
	}
	return s.resolveAreaNodes(q.Area)
}

// lenientScope is the node scope key of an endpoint that drops an unknown
// area or code (packets, bulk-health, the fleet clock skew).
func (s *Server) lenientScope(q url.Values) string {
	return nodeScopeKey(q.Get("area"), s.dagiRequest(q).sel)
}

// analyticsScope is rejectAnalyticsFilters for an analytics request with
// its DAGI params: it writes the 400 (unknown area first, then an unknown
// code) and returns false, or returns the node scope key to compute with.
func (s *Server) analyticsScope(w http.ResponseWriter, q url.Values, region, area string) (string, bool) {
	if s.rejectAnalyticsFilters(w, region, area) {
		return "", false
	}
	d := s.dagiRequest(q)
	if d.unknown != "" {
		writeError(w, http.StatusBadRequest, "unknown "+d.unknown)
		return "", false
	}
	return nodeScopeKey(area, d.sel), true
}

// DagiConfigResponse is GET /api/config/dagi: what M3's control lists.
type DagiConfigResponse struct {
	Enabled bool `json:"enabled"`
	// Credit must be shown with anything drawn from the data.
	Credit string            `json:"credit,omitempty"`
	Source *DagiSource       `json:"source,omitempty"`
	Layers []DagiConfigLayer `json:"layers"`
}

// DagiConfigLayer is one layer and its filter param.
type DagiConfigLayer struct {
	Layer string           `json:"layer"`
	Param string           `json:"param"`
	Areas []DagiConfigArea `json:"areas"`
}

// DagiConfigArea is one selectable value, sorted by code (labels are for
// the client to sort with Danish collation).
type DagiConfigArea struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// OutsideKommune marks Christiansø (0411), an area outside the kommune
	// division.
	OutsideKommune bool `json:"outsideKommune,omitempty"`
	// Codes is every postnummer a grouped entry covers, sorted; each
	// selects the group.
	Codes []string `json:"codes,omitempty"`
}

func (idx *dagiIndex) configResponse() DagiConfigResponse {
	src := idx.source
	resp := DagiConfigResponse{Enabled: true, Credit: idx.credit, Source: &src, Layers: make([]DagiConfigLayer, 0, dagiLayerCount)}
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		li := &idx.layers[l]
		byKey := make(map[string]*DagiConfigArea, len(li.features))
		areas := make([]DagiConfigArea, 0, len(li.features))
		codes := map[string][]string{}
		for i := range li.features {
			f := &li.features[i]
			codes[f.key] = append(codes[f.key], f.area.Code)
			if f.key == f.area.Code {
				areas = append(areas, DagiConfigArea{Code: f.key, Name: f.area.Name, OutsideKommune: f.outsideKommune})
			}
		}
		sort.Slice(areas, func(i, j int) bool { return areas[i].Code < areas[j].Code })
		for i := range areas {
			byKey[areas[i].Code] = &areas[i]
		}
		for key, cs := range codes {
			if len(cs) > 1 {
				sort.Strings(cs)
				byKey[key].Codes = cs
			}
		}
		resp.Layers = append(resp.Layers, DagiConfigLayer{Layer: l.String(), Param: dagiParams[l], Areas: areas})
	}
	return resp
}

func (s *Server) handleConfigDagi(w http.ResponseWriter, r *http.Request) {
	idx := s.dagiIndex()
	if idx == nil {
		writeJSON(w, DagiConfigResponse{Layers: []DagiConfigLayer{}})
		return
	}
	writeJSON(w, idx.configResponse())
}

// DagiPolygonsResponse is GET /api/config/dagi/polygons: boundaries to
// draw, in the asset's own compact encoding.
type DagiPolygonsResponse struct {
	Layer    string               `json:"layer"`
	Param    string               `json:"param"`
	Credit   string               `json:"credit"`
	Quantum  float64              `json:"quantum"`
	Encoding string               `json:"encoding"`
	Features []DagiPolygonFeature `json:"features"`
}

// DagiPolygonFeature is one feature's rings.
type DagiPolygonFeature struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Group is the filter key of a grouped postnummer.
	Group string    `json:"group,omitempty"`
	Rings [][]int64 `json:"rings"`
}

const dagiRingEncoding = "rings: [lon0, lat0, dlon, dlat, ...] in quantum degrees, closing vertex implicit, even-odd"

// dagiPolygonsMaxAge is the browser cache lifetime: the asset only changes
// with a new binary, and the ETag revalidates after.
const dagiPolygonsMaxAge = "86400"

// handleConfigDagiPolygons serves ?layer=<kommune|landsdel|region|postnummer>
// and optionally &code= (any code of the layer; a grouped postnummer gives
// every member).
func (s *Server) handleConfigDagiPolygons(w http.ResponseWriter, r *http.Request) {
	idx := s.dagiIndex()
	if idx == nil {
		writeError(w, http.StatusNotFound, "DAGI is off")
		return
	}
	q := r.URL.Query()
	layer := DagiLayer(-1)
	for l := DagiLayer(0); l < dagiLayerCount; l++ {
		if q.Get("layer") == l.String() {
			layer = l
		}
	}
	if layer < 0 {
		writeError(w, http.StatusBadRequest, "layer must be kommune, landsdel, region or postnummer")
		return
	}
	li := &idx.layers[layer]
	key := ""
	if _, given := q["code"]; given {
		k, ok := li.filterKey(q.Get("code"))
		if q.Get("code") == "" {
			writeError(w, http.StatusBadRequest, "code is empty")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "unknown code")
			return
		}
		key = k
	}
	resp := DagiPolygonsResponse{Layer: layer.String(), Param: dagiParams[layer], Credit: idx.credit, Quantum: idx.quantum,
		Encoding: dagiRingEncoding, Features: []DagiPolygonFeature{}}
	for i := range li.features {
		f := &li.features[i]
		if key != "" && f.key != key {
			continue
		}
		pf := DagiPolygonFeature{Code: f.area.Code, Name: f.area.Name, Rings: f.rings}
		if f.grouped {
			pf.Group = f.key
		}
		resp.Features = append(resp.Features, pf)
	}
	sort.Slice(resp.Features, func(i, j int) bool { return resp.Features[i].Code < resp.Features[j].Code })
	body, err := json.Marshal(resp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256(body)
	tag := `"` + hex.EncodeToString(sum[:12]) + `"`
	h := w.Header()
	h.Set("Cache-Control", "public, max-age="+dagiPolygonsMaxAge)
	h.Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	w.Write(append(body, '\n'))
}
