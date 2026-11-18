// Package main: "Moments" (#881 M1) — server-side detection of a few
// noteworthy mesh events, broadcast on the WebSocket hub as {"type":"moment"}
// and kept in a small ring buffer served by GET /api/moments.
//
// Design (M0 plan on #881, approved as written):
//   - Zero work on the per-packet ingest and broadcast path. Every detector
//     piggybacks on data the server already refreshes on its own timer:
//     new_node / back_online on an indexed nodes.last_seen delta query every
//     15 s, record_link on the 60 s neighbor-graph snapshot swap, and
//     busiest_repeater on the repeater-relay snapshot recompute.
//   - Read-only: the detector only issues SELECTs on the server's mode=ro
//     connection (#1283).
//   - Silent seed: the first look at each source after the store has loaded
//     only records a baseline; a restart never replays old events.
//   - Bounded: the node map, the cooldown map and the history ring all have
//     caps and evict.
//   - Privacy: companions are never a moment; every node in a moment passes
//     identityHidden (node/observer blacklist, hidden-name prefixes) and the
//     node-list geofilter, at emit time, and identityHidden again when the
//     history is read. Positions are advertised GPS only, never (0,0).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Moment kinds, as sent in Moment.Kind.
const (
	MomentKindNewNode    = "new_node"
	MomentKindBackOnline = "back_online"
	MomentKindRecordLink = "record_link"
	MomentKindBusiest    = "busiest_repeater"
)

const (
	// momentsNodeTickInterval is the cadence of the nodes.last_seen delta.
	momentsNodeTickInterval = 15 * time.Second
	// momentsDeltaOverlap re-reads a little before the previous tick so a
	// row written while the previous query ran is not missed. Rows seen
	// twice are harmless: an unchanged last_seen is not an event.
	momentsDeltaOverlap = time.Minute
	// momentsFreshWindow: a new node's first_seen and a returning node's
	// last_seen must be this close to now. Keeps a restart, a late ingest or
	// a clock-skewed row from announcing something that happened long ago.
	momentsFreshWindow = 10 * time.Minute
	// momentsNodeMapMin is the node map's minimum cap; the cap is
	// max(2 × nodes at seed, this).
	momentsNodeMapMin = 20000
	// momentsCooldownCap bounds the per-(kind, key) cooldown map.
	momentsCooldownCap = 10000
	// momentsGlobalBurst is the global token bucket's burst.
	momentsGlobalBurst = 3
	// momentsNewNodeCooldown keeps a key from being announced as new twice.
	// first_seen must also be fresh, so in practice a key is new once.
	momentsNewNodeCooldown = 7 * 24 * time.Hour
	// momentsBackOnlineCooldown: the same key is not back online twice
	// within this.
	momentsBackOnlineCooldown = 24 * time.Hour
	// momentsBusyLeaderCooldown suppresses the same busiest repeater.
	momentsBusyLeaderCooldown = 60 * time.Minute
	// A new record must beat the old one by max(0.5 km, 1 %).
	momentsRecordEpsKm   = 0.5
	momentsRecordEpsFrac = 0.01
	// momentsRecordMinCount: one packet is not a link.
	momentsRecordMinCount = 2
	// momentsDropLogEvery throttles the "moment dropped" log line.
	momentsDropLogEvery = 10 * time.Minute
)

// momentKindCap is a per-kind rate cap: at most n moments per window.
type momentKindCap struct {
	n   int
	per time.Duration
}

var momentKindCaps = map[string]momentKindCap{
	MomentKindNewNode:    {3, time.Minute},
	MomentKindBackOnline: {2, time.Minute},
	MomentKindRecordLink: {1, 10 * time.Minute},
	MomentKindBusiest:    {1, 15 * time.Minute},
}

// MomentNode is a node in a moment. Lat/Lon are the node's advertised GPS,
// omitted when it has none.
type MomentNode struct {
	PublicKey string   `json:"publicKey"`
	Name      string   `json:"name,omitempty"`
	Role      string   `json:"role"`
	Lat       *float64 `json:"lat,omitempty"`
	Lon       *float64 `json:"lon,omitempty"`
}

// MomentLink is the record_link pair and its great-circle distance.
type MomentLink struct {
	A          MomentNode `json:"a"`
	B          MomentNode `json:"b"`
	DistanceKm float64    `json:"distanceKm"`
}

// Moment is one event, as broadcast in {"type":"moment","data":…} and listed
// by GET /api/moments.
type Moment struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	At   string `json:"at"`
	// Node: new_node, back_online and busiest_repeater.
	Node *MomentNode `json:"node,omitempty"`
	// Link: record_link.
	Link *MomentLink `json:"link,omitempty"`
	// SilentHours: back_online, how long the repeater was silent.
	SilentHours *float64 `json:"silentHours,omitempty"`
	// Relays and WindowMinutes: busiest_repeater, distinct non-advert
	// packets relayed in the last WindowMinutes.
	Relays        *int `json:"relays,omitempty"`
	WindowMinutes *int `json:"windowMinutes,omitempty"`
	// PreviousKm: record_link, the record it beat (absent for the first).
	PreviousKm *float64 `json:"previousKm,omitempty"`
	// More: how many further nodes of the same kind this moment stands for,
	// when one tick found more than the per-kind cap allows.
	More int `json:"more,omitempty"`
}

// MomentsResponse is the GET /api/moments body.
type MomentsResponse struct {
	Moments []Moment `json:"moments"`
}

// momentNodeRow is one nodes row as the delta query reads it.
type momentNodeRow struct {
	PublicKey string
	Name      string
	Role      string
	Lat, Lon  float64
	HasPos    bool
	LastSeen  string
	FirstSeen string
	Foreign   bool
	// Retired: the key has an inactive_nodes row (see
	// inactiveNodeLastSeenSQL); RetiredLastSeen is that row's last_seen.
	Retired         bool
	RetiredLastSeen string
}

// momentsNodeReader reads nodes rows: all of them when full, else the rows
// whose last_seen is after since.
type momentsNodeReader func(ctx context.Context, since time.Time, full bool) ([]momentNodeRow, error)

// momentNodeState is what the detector remembers per node.
type momentNodeState struct {
	lastSeen time.Time
	name     string
	role     string
	lat, lon float64
	hasGPS   bool
	foreign  bool
}

// momentRecord is the current longest qualifying link; a < b, and the
// pair is its key.
type momentRecord struct {
	a, b  string
	km    float64
	valid bool
}

func (r momentRecord) same(a, b string) bool { return r.valid && r.a == a && r.b == b }

// before orders equal-length candidates deterministically.
func (r momentRecord) before(a, b string) bool { return a < r.a || (a == r.a && b < r.b) }

// momentsDeps are the detector's injected dependencies.
type momentsDeps struct {
	now        func() time.Time
	cfg        *Config
	readNodes  momentsNodeReader
	storeReady func() bool
	// geo returns the geofilter to apply, or nil for none.
	geo       func() *GeoFilterConfig
	maxEdgeKm float64
	broadcast func(Moment)
}

// momentsDetector detects moments; see the file comment.
type momentsDetector struct {
	set momentsSettings
	momentsDeps

	// mu serializes the detectors (node ticker, graph swap, relay
	// recompute) and guards their state. API reads never take it.
	mu          sync.Mutex
	nodes       map[string]*momentNodeState
	nodeCap     int
	nodesSeeded bool
	lastTick    time.Time
	cooldown    map[string]time.Time // kind|key → until
	kindLog     map[string][]time.Time
	tokens      float64
	tokensAt    time.Time
	record      momentRecord
	recSeeded   bool
	busySeeded  bool
	seq         uint64
	dropped     int64
	dropLog     logThrottle

	ringMu sync.Mutex
	ring   []Moment // oldest first, at most set.historySize
}

func newMomentsDetector(set momentsSettings, deps momentsDeps) *momentsDetector {
	if deps.now == nil {
		deps.now = time.Now
	}
	return &momentsDetector{
		set:         set,
		momentsDeps: deps,
		nodes:       make(map[string]*momentNodeState),
		nodeCap:     momentsNodeMapMin,
		cooldown:    make(map[string]time.Time),
		kindLog:     make(map[string][]time.Time),
		tokens:      momentsGlobalBurst,
	}
}

// ── node delta: new_node and back_online ──────────────────────────────────

// momentCandidate is a node-kind event before rate limiting.
type momentCandidate struct {
	pk     string
	at     time.Time // ordering: first_seen (new) or last_seen (back)
	silent time.Duration
}

// tickNodes runs one node delta. The first run after the store has loaded
// reads every node and emits nothing.
func (d *momentsDetector) tickNodes(ctx context.Context) {
	now := d.now()
	d.mu.Lock()
	seeded, since := d.nodesSeeded, d.lastTick.Add(-momentsDeltaOverlap)
	d.mu.Unlock()
	if !seeded && d.storeReady != nil && !d.storeReady() {
		return
	}
	rows, err := d.readNodes(ctx, since, !seeded)
	if err != nil {
		log.Printf("[moments] node delta: %v", err)
		return
	}
	d.mu.Lock()
	if !seeded {
		if c := 2 * len(rows); c > d.nodeCap {
			d.nodeCap = c
		}
	}
	var fresh, back []momentCandidate
	for i := range rows {
		kind, c := d.observe(&rows[i], now, seeded)
		switch kind {
		case MomentKindNewNode:
			fresh = append(fresh, c)
		case MomentKindBackOnline:
			back = append(back, c)
		}
	}
	d.nodesSeeded, d.lastTick = true, now
	out := d.emitNodeKind(MomentKindNewNode, fresh, now)
	out = append(out, d.emitNodeKind(MomentKindBackOnline, back, now)...)
	d.mu.Unlock()
	d.publish(out)
}

func absDuration(x time.Duration) time.Duration {
	if x < 0 {
		return -x
	}
	return x
}

// observe folds one row into the node map and, when detect is set, says
// which node kind (if any) the row is. Must hold d.mu.
func (d *momentsDetector) observe(r *momentNodeRow, now time.Time, detect bool) (string, momentCandidate) {
	pk := strings.ToLower(r.PublicKey)
	if pk == "" {
		return "", momentCandidate{}
	}
	cur := parseNodeTimestamp(r.LastSeen)
	st, known := d.nodes[pk]
	if !known {
		if len(d.nodes) >= d.nodeCap {
			d.evictNodes()
		}
		st = &momentNodeState{}
		d.nodes[pk] = st
	}
	prev := st.lastSeen
	st.name, st.role, st.foreign = r.Name, strings.ToLower(r.Role), r.Foreign
	st.lat, st.lon = r.Lat, r.Lon
	st.hasGPS = r.HasPos && !(r.Lat == 0 && r.Lon == 0)
	if cur.After(st.lastSeen) {
		st.lastSeen = cur
	}
	if !detect || cur.IsZero() || absDuration(now.Sub(cur)) > momentsFreshWindow {
		return "", momentCandidate{}
	}
	if !known {
		if r.Retired {
			// The shared returning-node rule: an inactive_nodes row means
			// this key was seen before, so it is back, never new.
			prev = parseNodeTimestamp(r.RetiredLastSeen)
		} else {
			first := parseNodeTimestamp(r.FirstSeen)
			if !first.IsZero() && absDuration(now.Sub(first)) <= momentsFreshWindow && momentsNewNodeRole(st.role) {
				return MomentKindNewNode, momentCandidate{pk: pk, at: first}
			}
			return "", momentCandidate{}
		}
	}
	if st.role == "repeater" && !prev.IsZero() && cur.Sub(prev) >= d.set.backOnline {
		return MomentKindBackOnline, momentCandidate{pk: pk, at: cur, silent: cur.Sub(prev)}
	}
	return "", momentCandidate{}
}

// momentsNewNodeRole: infrastructure roles only. Companions are personal
// devices and never a moment; an unknown role is not announced either.
func momentsNewNodeRole(role string) bool {
	return role == "repeater" || role == "room" || role == "sensor"
}

// emitNodeKind turns one tick's candidates of a kind into moments: one each
// while the per-kind cap allows, or a single moment with More when there are
// more candidates than the cap has room for. Must hold d.mu.
func (d *momentsDetector) emitNodeKind(kind string, cands []momentCandidate, now time.Time) []Moment {
	ok := cands[:0]
	for _, c := range cands {
		st := d.nodes[c.pk]
		if st != nil && d.visible(c.pk, st) && !d.onCooldown(kind, c.pk, now) {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return nil
	}
	sort.Slice(ok, func(i, j int) bool {
		if !ok[i].at.Equal(ok[j].at) {
			return ok[i].at.Before(ok[j].at)
		}
		return ok[i].pk < ok[j].pk
	})
	cooldown := momentsNewNodeCooldown
	if kind == MomentKindBackOnline {
		cooldown = momentsBackOnlineCooldown
	}
	build := func(c momentCandidate, more int) Moment {
		m := d.newMoment(kind, now)
		m.Node = d.momentNode(c.pk)
		if kind == MomentKindBackOnline {
			h := math.Round(c.silent.Hours()*10) / 10
			m.SilentHours = &h
		}
		m.More = more
		return m
	}
	var out []Moment
	if len(ok) > d.kindBudget(kind, now) {
		if !d.allow(kind, now) {
			d.noteDropped(int64(len(ok)), now)
			return nil
		}
		for _, c := range ok {
			d.setCooldown(kind, c.pk, now.Add(cooldown))
		}
		return []Moment{build(ok[0], len(ok)-1)}
	}
	for i, c := range ok {
		if !d.allow(kind, now) {
			d.noteDropped(int64(len(ok)-i), now)
			break
		}
		d.setCooldown(kind, c.pk, now.Add(cooldown))
		out = append(out, build(c, 0))
	}
	return out
}

// ── record_link: on each neighbor-graph snapshot swap ─────────────────────

// onGraph checks the swapped-in neighbor graph for a new longest direct
// link. Only confident (resolved, unambiguous) edges between two visible,
// non-companion nodes with GPS, seen at least twice, inside the record
// window and no longer than MaxEdgeKm qualify.
func (d *momentsDetector) onGraph(g *NeighborGraph) {
	if g == nil {
		return
	}
	edges := g.AllEdges()
	now := d.now()
	d.mu.Lock()
	out := d.checkRecord(edges, now)
	d.mu.Unlock()
	d.publish(out)
}

// checkRecord must hold d.mu.
func (d *momentsDetector) checkRecord(edges []*NeighborEdge, now time.Time) []Moment {
	if !d.nodesSeeded {
		return nil
	}
	cutoff := now.Add(-d.set.recordWindow)
	var top momentRecord
	recordKm, recordFound := 0.0, false
	for _, e := range edges {
		if e == nil || e.NodeB == "" || e.Ambiguous || e.Count < momentsRecordMinCount || e.LastSeen.Before(cutoff) {
			continue
		}
		a, b := strings.ToLower(e.NodeA), strings.ToLower(e.NodeB)
		if a == b {
			continue
		}
		if b < a {
			a, b = b, a
		}
		sa, sb := d.nodes[a], d.nodes[b]
		if !d.linkEndpoint(a, sa) || !d.linkEndpoint(b, sb) {
			continue
		}
		km := haversineKm(sa.lat, sa.lon, sb.lat, sb.lon)
		if d.maxEdgeKm > 0 && km > d.maxEdgeKm {
			continue
		}
		if km < d.set.recordMinKm {
			continue
		}
		if d.record.same(a, b) {
			recordKm, recordFound = km, true
		}
		if !top.valid || km > top.km || (km == top.km && top.before(a, b)) {
			top = momentRecord{a: a, b: b, km: km, valid: true}
		}
	}
	if !d.recSeeded {
		d.record, d.recSeeded = top, true
		return nil
	}
	if !top.valid {
		d.record = momentRecord{} // the record left the window: silent
		return nil
	}
	if d.record.valid && !recordFound {
		d.record = top // silent drop to the current top
		return nil
	}
	if d.record.same(top.a, top.b) {
		d.record.km = recordKm
		return nil
	}
	if d.record.valid {
		eps := math.Max(momentsRecordEpsKm, momentsRecordEpsFrac*d.record.km)
		if top.km <= d.record.km+eps {
			return nil
		}
	}
	if !d.allow(MomentKindRecordLink, now) {
		// Keep the old record: the latest top fires once the cap allows.
		d.noteDropped(1, now)
		return nil
	}
	m := d.newMoment(MomentKindRecordLink, now)
	m.Link = &MomentLink{A: *d.momentNode(top.a), B: *d.momentNode(top.b), DistanceKm: math.Round(top.km*10) / 10}
	if d.record.valid {
		p := math.Round(d.record.km*10) / 10
		m.PreviousKm = &p
	}
	d.record = top
	return []Moment{m}
}

// linkEndpoint: a record endpoint is a visible node (never a companion)
// with a known role and real GPS. Must hold d.mu.
func (d *momentsDetector) linkEndpoint(pk string, st *momentNodeState) bool {
	return st != nil && st.hasGPS && st.role != "" && d.visible(pk, st)
}

// ── busiest_repeater: on each repeater-relay snapshot recompute ───────────

// onRelay picks the visible repeater with the most distinct non-advert
// relays in the busy window (relayCountRecent) at or above the floor.
func (d *momentsDetector) onRelay(snap *repeaterRelaySnapshot) {
	if snap == nil || snap.loading {
		return
	}
	now := d.now()
	d.mu.Lock()
	out := d.checkBusiest(snap.info, now)
	d.mu.Unlock()
	d.publish(out)
}

// checkBusiest must hold d.mu.
func (d *momentsDetector) checkBusiest(info map[string]RepeaterRelayInfo, now time.Time) []Moment {
	if !d.nodesSeeded {
		return nil
	}
	best, bestN := "", 0
	for key, ri := range info {
		n := ri.relayCountRecent
		if n < d.set.busyMinRelays || len(key) != 64 {
			continue
		}
		pk := strings.ToLower(key)
		st := d.nodes[pk]
		if st == nil || st.role != "repeater" || !d.visible(pk, st) {
			continue
		}
		if n > bestN || (n == bestN && pk < best) {
			best, bestN = pk, n
		}
	}
	if !d.busySeeded {
		d.busySeeded = true
		if best != "" {
			d.setCooldown(MomentKindBusiest, best, now.Add(momentsBusyLeaderCooldown))
		}
		return nil
	}
	if best == "" || d.onCooldown(MomentKindBusiest, best, now) {
		return nil
	}
	if !d.allow(MomentKindBusiest, now) {
		d.noteDropped(1, now)
		return nil
	}
	d.setCooldown(MomentKindBusiest, best, now.Add(momentsBusyLeaderCooldown))
	m := d.newMoment(MomentKindBusiest, now)
	m.Node = d.momentNode(best)
	relays, win := bestN, int(d.set.busyWindow/time.Minute)
	m.Relays, m.WindowMinutes = &relays, &win
	return []Moment{m}
}

// ── shared: visibility, limits, cooldowns, bounded maps ───────────────────

// visible is the emit-time privacy rule. Must hold d.mu.
func (d *momentsDetector) visible(pk string, st *momentNodeState) bool {
	if st.role == "companion" || identityHidden(d.cfg, pk, st.name) {
		return false
	}
	if st.hasGPS && !st.foreign && d.geo != nil {
		if gf := d.geo(); gf != nil && !NodePassesGeoFilter(st.lat, st.lon, gf) {
			return false
		}
	}
	return true
}

func (d *momentsDetector) momentNode(pk string) *MomentNode {
	st := d.nodes[pk]
	n := &MomentNode{PublicKey: pk}
	if st == nil {
		return n
	}
	n.Name, n.Role = st.name, st.role
	if st.hasGPS {
		lat, lon := st.lat, st.lon
		n.Lat, n.Lon = &lat, &lon
	}
	return n
}

func (d *momentsDetector) newMoment(kind string, now time.Time) Moment {
	d.seq++
	return Moment{
		ID:   strconv.FormatInt(now.UnixMilli(), 36) + "-" + strconv.FormatUint(d.seq, 36),
		Kind: kind,
		At:   now.UTC().Format(time.RFC3339),
	}
}

// kindBudget is how many more moments of kind the per-kind cap allows now.
func (d *momentsDetector) kindBudget(kind string, now time.Time) int {
	c := momentKindCaps[kind]
	kept := d.kindLog[kind][:0]
	for _, t := range d.kindLog[kind] {
		if now.Sub(t) < c.per {
			kept = append(kept, t)
		}
	}
	d.kindLog[kind] = kept
	if n := c.n - len(kept); n > 0 {
		return n
	}
	return 0
}

// allow charges one moment of kind against its per-kind cap and the global
// token bucket (maxPerMinute per minute, burst momentsGlobalBurst).
func (d *momentsDetector) allow(kind string, now time.Time) bool {
	if d.kindBudget(kind, now) == 0 {
		return false
	}
	if !d.tokensAt.IsZero() && now.After(d.tokensAt) {
		d.tokens += now.Sub(d.tokensAt).Minutes() * float64(d.set.maxPerMinute)
		if d.tokens > momentsGlobalBurst {
			d.tokens = momentsGlobalBurst
		}
	}
	d.tokensAt = now
	if d.tokens < 1 {
		return false
	}
	d.tokens--
	d.kindLog[kind] = append(d.kindLog[kind], now)
	return true
}

func (d *momentsDetector) noteDropped(n int64, now time.Time) {
	d.dropped += n
	if grew, ok := d.dropLog.note(now, d.dropped, momentsDropLogEvery); ok {
		log.Printf("[moments] rate limit: dropped %d moment(s) since the last note (total %d)", grew, d.dropped)
	}
}

func (d *momentsDetector) onCooldown(kind, pk string, now time.Time) bool {
	until, ok := d.cooldown[kind+"|"+pk]
	return ok && now.Before(until)
}

func (d *momentsDetector) setCooldown(kind, pk string, until time.Time) {
	key := kind + "|" + pk
	if _, ok := d.cooldown[key]; !ok && len(d.cooldown) >= momentsCooldownCap {
		evictOldest(d.cooldown, momentsCooldownCap*9/10, func(t time.Time) time.Time { return t })
	}
	d.cooldown[key] = until
}

// evictNodes drops the longest-silent nodes down to 90 % of the cap. Only a
// node map at its cap pays this; age alone never evicts, since back_online
// needs exactly the silent nodes.
func (d *momentsDetector) evictNodes() {
	evictOldest(d.nodes, d.nodeCap*9/10, func(s *momentNodeState) time.Time { return s.lastSeen })
}

// evictOldest deletes the entries with the smallest at(v) until len(m) ≤
// keep. O(n log n), run only when a map reaches its cap.
func evictOldest[V any](m map[string]V, keep int, at func(V) time.Time) {
	if len(m) <= keep {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, at(v)})
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].t.Equal(all[j].t) {
			return all[i].t.Before(all[j].t)
		}
		return all[i].k < all[j].k
	})
	for _, e := range all[:len(m)-keep] {
		delete(m, e.k)
	}
}

// ── history ring and publish ──────────────────────────────────────────────

func (d *momentsDetector) publish(ms []Moment) {
	if len(ms) == 0 {
		return
	}
	d.ringMu.Lock()
	for _, m := range ms {
		if len(d.ring) >= d.set.historySize {
			copy(d.ring, d.ring[1:])
			d.ring = d.ring[:len(d.ring)-1]
		}
		d.ring = append(d.ring, m)
	}
	d.ringMu.Unlock()
	if d.broadcast != nil {
		for _, m := range ms {
			d.broadcast(m)
		}
	}
}

// momentNodes lists every node a moment names.
func momentNodes(m *Moment) []*MomentNode {
	if m.Link != nil {
		return []*MomentNode{&m.Link.A, &m.Link.B}
	}
	if m.Node != nil {
		return []*MomentNode{m.Node}
	}
	return nil
}

// history returns up to limit moments, newest first, re-checked against the
// current blacklists and hidden-name prefixes so a node hidden after its
// moment fired drops out of the history too.
func (d *momentsDetector) history(limit int) []Moment {
	d.ringMu.Lock()
	defer d.ringMu.Unlock()
	out := make([]Moment, 0, min(limit, len(d.ring)))
	for i := len(d.ring) - 1; i >= 0 && len(out) < limit; i-- {
		hidden := false
		for _, n := range momentNodes(&d.ring[i]) {
			if identityHidden(d.cfg, n.PublicKey, n.Name) {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, d.ring[i])
		}
	}
	return out
}

// ── SQL: the nodes.last_seen delta ────────────────────────────────────────

// momentNodesQuery builds the node read. The delta matches last_seen after
// since in both formats nodes.last_seen holds (parseNodeTimestamp): RFC3339,
// which the ingestor writes, and SQLite's "2006-01-02 15:04:05" from older
// rows. The two do not compare as strings ('T' > ' '), so:
//   - branch 1, last_seen > "YYYY-MM-DDTHH:MM:SS": every RFC3339 row after
//     since (with or without fractional seconds or Z), and every SQLite-format
//     row on a later date;
//   - branch 2, "YYYY-MM-DD HH:MM:SS" < last_seen < "YYYY-MM-DD!": the
//     SQLite-format rows later on since's own date ('!' sorts right after
//     ' ' and before 'T', so same-date RFC3339 rows stay out).
//
// Both branches are range scans of idx_nodes_last_seen (MULTI-INDEX OR, see
// TestMomentsDeltaQueryPlanUsesIndex). The returning-node rule is the shared
// inactiveNodeLastSeenSQL.
func momentNodesQuery(since time.Time, full, withInactive bool) (string, []interface{}) {
	retired := "NULL"
	if withInactive {
		retired = inactiveNodeLastSeenSQL("n")
	}
	q := `SELECT n.public_key, COALESCE(n.name, ''), COALESCE(n.role, ''), n.lat, n.lon,
	       COALESCE(n.last_seen, ''), COALESCE(n.first_seen, ''), COALESCE(n.foreign_advert, 0), ` + retired + `
	FROM nodes n`
	if full {
		return q, nil
	}
	s := since.UTC()
	return q + `
	WHERE n.last_seen > ? OR (n.last_seen > ? AND n.last_seen < ?)`,
		[]interface{}{s.Format("2006-01-02T15:04:05"), s.Format("2006-01-02 15:04:05"), s.Format("2006-01-02") + "!"}
}

// readMomentNodes is the server's momentsNodeReader.
func (s *Server) readMomentNodes(ctx context.Context, since time.Time, full bool) ([]momentNodeRow, error) {
	if s.db == nil || s.db.conn == nil {
		return nil, fmt.Errorf("no database")
	}
	withInactive, err := s.hasInactiveNodesTable(ctx)
	if err != nil {
		return nil, err
	}
	q, args := momentNodesQuery(since, full, withInactive)
	rows, err := s.db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []momentNodeRow
	for rows.Next() {
		var r momentNodeRow
		var lat, lon sql.NullFloat64
		var foreign int64
		var retired sql.NullString
		if err := rows.Scan(&r.PublicKey, &r.Name, &r.Role, &lat, &lon, &r.LastSeen, &r.FirstSeen, &foreign, &retired); err != nil {
			return nil, err
		}
		r.HasPos = lat.Valid && lon.Valid
		r.Lat, r.Lon = lat.Float64, lon.Float64
		r.Foreign = foreign != 0
		r.Retired, r.RetiredLastSeen = retired.Valid, retired.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── wiring ────────────────────────────────────────────────────────────────

// momentsStoreReady is the store's "loaded" signal, the same one
// computeRepeaterRelaySnapshot reports as loading.
func (s *PacketStore) momentsStoreReady() bool {
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	return loaded && (s.backgroundLoadDone.Load() || s.backgroundLoadFailed.Load())
}

// startMoments starts the detector when moments.enabled is set and returns
// its stop function. Disabled, it does nothing at all: no ticker, no hooks,
// no relay window, and GET /api/moments answers an empty list. Idempotent.
func (s *Server) startMoments(interval time.Duration) func() {
	set := s.cfg.momentsSettings()
	if !set.enabled || s.store == nil || s.moments.Load() != nil {
		return func() {}
	}
	if interval <= 0 {
		interval = momentsNodeTickInterval
	}
	geo := func() *GeoFilterConfig {
		if s.cfg.GeoFilterExemptNodeList {
			return nil
		}
		return s.getGeoFilter()
	}
	d := newMomentsDetector(set, momentsDeps{
		cfg:        s.cfg,
		readNodes:  s.readMomentNodes,
		storeReady: s.store.momentsStoreReady,
		geo:        geo,
		maxEdgeKm:  s.cfg.NeighborMaxEdgeKm(),
		broadcast: func(m Moment) {
			if s.hub != nil {
				s.hub.Broadcast(WSMessage{Type: "moment", Data: m})
			}
		},
	})
	if !s.moments.CompareAndSwap(nil, d) {
		return func() {}
	}
	s.store.relayRecentWindow.Store(int64(set.busyWindow))
	graphHook, relayHook := d.onGraph, d.onRelay
	s.store.momentsGraphHook.Store(&graphHook)
	s.store.momentsRelayHook.Store(&relayHook)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[moments] node tick panic: %v", r)
						}
					}()
					d.tickNodes(ctx)
				}()
			case <-ctx.Done():
				return
			}
		}
	}()
	log.Printf("[moments] enabled (node delta every %s; history %d)", interval, set.historySize)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.store.momentsGraphHook.Store(nil)
			s.store.momentsRelayHook.Store(nil)
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		})
	}
}

// handleMoments serves GET /api/moments: the newest moments first, at most
// limit (default and maximum moments.historySize). An empty list when
// moments are disabled.
func (s *Server) handleMoments(w http.ResponseWriter, r *http.Request) {
	resp := MomentsResponse{Moments: []Moment{}}
	if d := s.moments.Load(); d != nil {
		limit := d.set.historySize
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v < limit {
			limit = v
		}
		resp.Moments = d.history(limit)
	}
	writeJSON(w, resp)
}
