package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/meshcore-analyzer/nodecard"
)

// #857 — own scope, transported scopes, first seen, avg hops and the
// estimated advert intervals on the node share card, each from the source
// the node page reads.

func i64(v int64) *int64 { return &v }

// addScopeColumns gives the test DB the nodes.default_scope columns the
// production schema has (#899, #1865 follow-up).
func (e *nodeShareEnv) addScopeColumns(t *testing.T) {
	t.Helper()
	for _, c := range []string{"default_scope TEXT", "default_scope_confirmed_at TEXT"} {
		if _, err := e.srv.db.conn.Exec("ALTER TABLE nodes ADD COLUMN " + c); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.db.hasDefaultScopeFlag.forceTrue()
	e.srv.db.hasDefaultScopeConfirmedAtFlag.forceTrue()
}

func (e *nodeShareEnv) setScope(t *testing.T, pk string, scope, confirmedAt any) {
	t.Helper()
	if _, err := e.srv.db.conn.Exec(`UPDATE nodes SET default_scope = ?, default_scope_confirmed_at = ? WHERE public_key = ?`, scope, confirmedAt, pk); err != nil {
		t.Fatal(err)
	}
}

// setPaths puts transmissions with these paths in pk's in-memory window
// (byNode), as ingest does.
func (e *nodeShareEnv) setPaths(pk string, paths ...string) {
	st := e.srv.store
	st.mu.Lock()
	defer st.mu.Unlock()
	var txs []*StoreTx
	for _, p := range paths {
		tx := &StoreTx{FirstSeen: nsHeard.Format(time.RFC3339)}
		setTxPath(tx, p) // parsed at write time, as ingest does (#470)
		txs = append(txs, tx)
	}
	st.byNode[pk] = txs
}

// publishIntervals installs a published advert-interval snapshot holding
// rows, as the background recomputer does.
func (e *nodeShareEnv) publishIntervals(rows ...advertIntervalNodeRow) *analyticsRecomputer {
	snap := &advertIntervalSnapshot{rows: rows}
	rc := newAnalyticsRecomputer("advert-intervals", time.Hour, func() interface{} { return snap })
	rc.runOnce()
	st := e.srv.store
	st.analyticsRecomputerMu.Lock()
	st.recompAdvertIntervals = rc
	st.analyticsRecomputerMu.Unlock()
	return rc
}

func estimate(sec int64, conf string) AdvertIntervalEstimate {
	return AdvertIntervalEstimate{IntervalS: i64(sec), Samples: 12, Confidence: conf, Status: advertIntervalEstimated, Snapped: true}
}

// newFullEnv is a repeater with every #857 source filled.
func newFullEnv(t *testing.T) *nodeShareEnv {
	t.Helper()
	e := newNodeShareEnv(t, true, nil)
	e.addScopeColumns(t)
	e.setScope(t, nsRepeater, "#dk-midtjylland", "2026-11-15T10:00:00Z")
	e.setRelay(map[string]RepeaterRelayInfo{nsRepeater: {RelayCount24h: 1234, RelayCount1h: 56, TransportedScopes: []string{"#dk", "#dk-aarhus"}}})
	e.setPaths(nsRepeater, `["aa","bb"]`, `["cc"]`, ``, `["dd","ee","ff"]`)
	e.publishIntervals(
		advertIntervalNodeRow{pubkey: nsRepeater, flood: estimate(47*3600, "high"), zeroHop: estimate(120*60, "medium")},
		advertIntervalNodeRow{pubkey: nsRoom, flood: estimate(12*3600, "low")},
	)
	return e
}

func TestNodeShare857_Sources(t *testing.T) {
	e := newFullEnv(t)
	in := e.input(t, nsRepeater)
	if in.OwnScope == nil || *in.OwnScope != "#dk-midtjylland" || !in.OwnScopeConfirmed {
		t.Errorf("own scope %v confirmed %v", in.OwnScope, in.OwnScopeConfirmed)
	}
	if strings.Join(in.TransportedScopes, ",") != "#dk,#dk-aarhus" {
		t.Errorf("transported scopes %v", in.TransportedScopes)
	}
	if !in.FirstSeen.Equal(nsHeard) {
		t.Errorf("first seen %v, want %v", in.FirstSeen, nsHeard)
	}
	if in.AvgHops == nil || *in.AvgHops != 2 { // (2+1+3)/3
		t.Errorf("avg hops %v, want 2", nilOr(in.AvgHops))
	}
	if f := in.FloodInterval; f == nil || f.Seconds != 47*3600 || f.Confidence != "high" || !f.Snapped {
		t.Errorf("flood interval %+v", f)
	}
	if z := in.ZeroHopInterval; z == nil || z.Seconds != 7200 || z.Confidence != "medium" {
		t.Errorf("zero-hop interval %+v", z)
	}
	desc := strings.Join(clauses(in), " · ")
	for _, want := range []string{"First Seen: 3h ago", "Avg Hops: 2", "Own scope: #dk-midtjylland", "Transported scopes: #dk, #dk-aarhus",
		"Estimated flood interval: ≈ 47 h", "Estimated zero-hop interval: ≈ 120 min"} {
		if !strings.Contains(desc, want) {
			t.Errorf("clauses %q miss %q", desc, want)
		}
	}
	// The dense card is served and fits the per-entry cache bound (#755).
	w := e.get("/api/nodes/" + nsRepeater + "/card.png")
	if w.Code != 200 || w.Body.Len() > cardBytesPerEntry {
		t.Errorf("card: %d, %d bytes (cache bound %d)", w.Code, w.Body.Len(), cardBytesPerEntry)
	}
}

// The values are the node page's: own scope and first seen as
// GET /api/nodes/{pk} returns them, avg hops as /health's stats.avgHops,
// the intervals as /api/analytics/advert-intervals estimates them.
func TestNodeShare857_SameAsNodePage(t *testing.T) {
	e := newFullEnv(t)
	for _, c := range []struct {
		scope, at any
	}{{"#dk", nil}, {"", "2026-11-15T10:00:00Z"}, {"*", ""}, {nil, nil}} {
		e.setScope(t, nsRepeater, c.scope, c.at)
		var detail struct {
			Node map[string]any `json:"node"`
		}
		if err := json.Unmarshal(e.get("/api/nodes/"+nsRepeater).Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		in := e.input(t, nsRepeater)
		page, _ := detail.Node["default_scope"].(string)
		pageConfirmed, _ := detail.Node["default_scope_confirmed_at"].(string)
		if (detail.Node["default_scope"] == nil) != (in.OwnScope == nil) || in.OwnScope != nil && *in.OwnScope != page || in.OwnScopeConfirmed != (pageConfirmed != "") {
			t.Errorf("scope %v/%v: card %v %v, node page %v %q", c.scope, c.at, in.OwnScope, in.OwnScopeConfirmed, detail.Node["default_scope"], pageConfirmed)
		}
		if fs, _ := detail.Node["first_seen"].(string); !in.FirstSeen.Equal(parseNodeTimestamp(fs)) {
			t.Errorf("first seen: card %v, node page %q", in.FirstSeen, fs)
		}
	}
	var health struct {
		Stats NodeHealthStats `json:"stats"`
	}
	if err := json.Unmarshal(e.get("/api/nodes/"+nsRepeater+"/health").Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if in := e.input(t, nsRepeater); health.Stats.AvgHops == nil || in.AvgHops == nil || *in.AvgHops != *health.Stats.AvgHops {
		t.Errorf("avg hops: card %v, health %v", nilOr(in.AvgHops), nilOr(health.Stats.AvgHops))
	}
}

// An absent source drops its value; it is never computed inline.
func TestNodeShare857_AbsentSources(t *testing.T) {
	e := newNodeShareEnv(t, true, nil) // no scope columns, no snapshots
	e.setPaths(nsRepeater, ``, `[]`)   // a window with no hop data
	in := e.input(t, nsRepeater)
	if in.OwnScope != nil || in.AvgHops != nil || in.FloodInterval != nil || in.ZeroHopInterval != nil || in.TransportedScopes != nil {
		t.Errorf("absent sources gave %+v", in)
	}
	e = newFullEnv(t)
	e.setScope(t, nsRepeater, nil, "2026-11-15T10:00:00Z") // NULL scope: no row on the node page
	e.setPaths(nsRepeater)
	e.setRelay(map[string]RepeaterRelayInfo{})
	e.publishIntervals() // published, but without this node
	in = e.input(t, nsRepeater)
	if in.OwnScope != nil || in.AvgHops != nil || in.FloodInterval != nil || in.TransportedScopes != nil {
		t.Errorf("absent values gave %+v", in)
	}
	for _, cl := range clauses(in) {
		for _, gone := range []string{"Own scope", "Avg Hops", "interval", "Transported"} {
			if strings.Contains(cl, gone) {
				t.Errorf("clause %q drawn without a value", cl)
			}
		}
	}
	// A low-confidence estimate is passed and dropped by the card; a
	// non-estimate is not passed at all.
	room := newFullEnv(t).input(t, nsRoom)
	if room.FloodInterval == nil || room.FloodInterval.Confidence != "low" || strings.Contains(strings.Join(clauses(room), " "), "interval") {
		t.Errorf("room: interval %+v, clauses %v", room.FloodInterval, clauses(room))
	}
	for _, est := range []AdvertIntervalEstimate{
		{Status: advertIntervalTooFew, Samples: 2, Confidence: advertConfidenceNone},
		{Status: advertIntervalIrregular, Samples: 9},
		{Status: advertIntervalNoneObserved},
		{Status: advertIntervalEstimated},
		{Status: advertIntervalEstimated, IntervalS: i64(0), Confidence: "high"},
		{Status: advertIntervalIrregular, IntervalS: i64(3600), Confidence: "high"}, // only an estimate is drawn
	} {
		if got := cardAdvertInterval(est); got != nil {
			t.Errorf("%+v passed as %+v", est, got)
		}
	}
}

// Reading the interval snapshot never wakes a paused recomputer: a crawler
// fetching cards must not keep the background pass running (#492).
func TestNodeShare857_IntervalsPeekNotLoad(t *testing.T) {
	e := newFullEnv(t)
	rc := e.srv.store.recomputer(&e.srv.store.recompAdvertIntervals)
	rc.pauseWhenIdle = true
	rc.readSinceCompute.Store(false)
	if in := e.input(t, nsRepeater); in.FloodInterval == nil {
		t.Fatal("no interval read")
	}
	if rc.readSinceCompute.Load() {
		t.Error("a card read counted as an analytics read")
	}
	rc.Load()
	if !rc.readSinceCompute.Load() {
		t.Error("the check is vacuous: Load does not mark a read here")
	}
}

// v changes with every displayed #857 value, and stays put while only the
// clock or an undisplayed value moves.
func TestNodeShare857_Version(t *testing.T) {
	e := newFullEnv(t)
	base := e.input(t, nsRepeater)
	v0 := e.c.version(base)
	same := func(name string, edit func(*nodecard.NodeCardInput), want bool) {
		t.Helper()
		in := e.input(t, nsRepeater)
		edit(&in)
		if got := e.c.version(in) == v0; got != want {
			t.Errorf("%s: v unchanged = %v, want %v", name, got, want)
		}
	}
	same("as of 20 min later (first seen still 3h ago)", func(in *nodecard.NodeCardInput) { in.AsOf = in.AsOf.Add(20 * time.Minute) }, true)
	same("first seen a minute earlier, same wording", func(in *nodecard.NodeCardInput) { in.FirstSeen = in.FirstSeen.Add(-time.Minute) }, true)
	same("first seen a day earlier", func(in *nodecard.NodeCardInput) { in.FirstSeen = in.FirstSeen.Add(-24 * time.Hour) }, false)
	same("avg hops", func(in *nodecard.NodeCardInput) { *in.AvgHops = 3 }, false)
	same("own scope", func(in *nodecard.NodeCardInput) { s := "#dk"; in.OwnScope = &s }, false)
	same("confirmed mark", func(in *nodecard.NodeCardInput) { in.OwnScopeConfirmed = false }, false)
	same("transported scopes", func(in *nodecard.NodeCardInput) { in.TransportedScopes = append(in.TransportedScopes, "#se") }, false)
	// The text names 3 scopes, but the tile draws as many as fit: a change
	// past the third must move v too (#861 review F1).
	four := func(last string) func(*nodecard.NodeCardInput) {
		return func(in *nodecard.NodeCardInput) { in.TransportedScopes = []string{"#a", "#b", "#c", last} }
	}
	in4 := e.input(t, nsRepeater)
	four("#d")(&in4)
	v4 := e.c.version(in4)
	in4e := e.input(t, nsRepeater)
	four("#e")(&in4e)
	if e.c.version(in4e) == v4 {
		t.Error("a 4th drawn scope changed (#d → #e) but v did not")
	}
	if a, b := nodeCardBytes(t, in4), nodeCardBytes(t, in4e); bytes.Equal(a, b) {
		t.Error("the check is vacuous: the 4th scope is not drawn")
	}
	same("flood interval", func(in *nodecard.NodeCardInput) { in.FloodInterval.Seconds = 48 * 3600 }, false)
	same("flood confidence (drawn detail)", func(in *nodecard.NodeCardInput) { in.FloodInterval.Confidence = "medium" }, false)
	same("zero-hop interval dropped (low)", func(in *nodecard.NodeCardInput) { in.ZeroHopInterval.Confidence = "low" }, false)
	same("flood interval a few seconds off, same wording", func(in *nodecard.NodeCardInput) { in.FloodInterval.Seconds += 60 }, true)
	// The served page and card agree on it.
	body := e.get("/p?node=" + nsRepeater).Body.String()
	if !strings.Contains(body, nodeCardPath(nsRepeater, v0)) {
		t.Errorf("page does not point at v=%s", v0)
	}
}

// og:description: the area, then the clauses in the card's order; when they
// do not fit the 200 runes, the lowest-priority clauses (the last) go
// first, whole, never cut with "…". The alt text follows the same rule.
func TestNodeShare857_DescriptionOrderAndLength(t *testing.T) {
	e := newFullEnv(t)
	in := e.input(t, nsRepeater)
	all := clauses(in)
	pg := e.c.page(in)
	if !strings.HasPrefix(pg.Description, "Midtjylland · Last heard: 3h ago") {
		t.Errorf("description starts %q", pg.Description)
	}
	if n := utf8.RuneCountInString(ogClip(pg.Description, 0)); n > ogDescriptionMaxRunes || strings.HasSuffix(ogText(pg.Description, ogDescriptionMaxRunes), "…") {
		t.Errorf("description %d runes: %q", n, pg.Description)
	}
	kept := strings.Split(pg.Description, " · ")
	if len(kept) >= len(all) {
		t.Fatalf("the full repeater should not fit: %d of %d clauses, %q", len(kept), len(all), pg.Description)
	}
	for i, k := range kept {
		if all[i] != k {
			t.Errorf("clause %d: %q, want %q (a prefix of the priority order)", i, k, all[i])
		}
	}
	if utf8.RuneCountInString(ogClip(pg.ImageAlt, 0)) > ogImageAltMaxRunes || strings.HasSuffix(ogText(pg.ImageAlt, ogImageAltMaxRunes), "…") {
		t.Errorf("alt %q", pg.ImageAlt)
	}
	// A short card keeps every clause.
	sensor := e.input(t, nsSensor)
	if got := e.c.page(sensor).Description; got != strings.Join(clauses(sensor), " · ") {
		t.Errorf("sensor description %q", got)
	}
	// One clause too long on its own is kept and cut by ogText.
	if got := joinWithin("", []string{strings.Repeat("x", 300), "y"}, " · ", 200); got != strings.Repeat("x", 300) {
		t.Errorf("joinWithin kept %q", got)
	}
	t.Logf("%d of %d clauses, %d runes: %s", len(kept), len(all), utf8.RuneCountInString(pg.Description), pg.Description)
}

// avgHopsOf is health's formula: the rounded mean over transmissions with
// a path; no path data is "no value", not 0.
func TestAvgHopsOf(t *testing.T) {
	tx := func(p string) *StoreTx { t := &StoreTx{}; setTxPath(t, p); return t }
	for _, c := range []struct {
		txs  []*StoreTx
		want int
		ok   bool
	}{
		{nil, 0, false},
		{[]*StoreTx{tx(""), tx("[]")}, 0, false},
		{[]*StoreTx{tx(`["aa"]`)}, 1, true},
		{[]*StoreTx{tx(`["aa","bb"]`), tx(`["aa"]`)}, 2, true}, // 1.5 rounds up
		{[]*StoreTx{tx(`["aa","bb"]`), tx(`["aa"]`), tx(`["a"]`), tx("")}, 1, true},
	} {
		if got, ok := avgHopsOf(c.txs); got != c.want || ok != c.ok {
			t.Errorf("%d txs: %d %v, want %d %v", len(c.txs), got, ok, c.want, c.ok)
		}
	}
}

func TestParseNodeTimestamp(t *testing.T) {
	want := time.Date(2026, 11, 15, 13, 20, 0, 0, time.UTC)
	for s, w := range map[string]time.Time{
		"2026-11-15T13:20:00Z": want, "2026-11-15 13:20:00": want, "": {}, "yesterday": {},
	} {
		if got := parseNodeTimestamp(s); !got.Equal(w) {
			t.Errorf("%q: %v", s, got)
		}
	}
}

// BenchmarkNodeShare857_Fields is the #857 work one card request adds
// (addNodePageFields): avg hops over a busy repeater's in-memory window,
// one primary-key read of the node row, and a binary search of a published
// interval snapshot holding every node.
func BenchmarkNodeShare857_Fields(b *testing.B) {
	for _, txs := range []int{1000, 20000} {
		b.Run(fmt.Sprintf("window=%d", txs), func(b *testing.B) {
			e := benchNodeShareEnv(b, 20000)
			for _, c := range []string{"default_scope TEXT", "default_scope_confirmed_at TEXT"} {
				if _, err := e.srv.db.conn.Exec("ALTER TABLE nodes ADD COLUMN " + c); err != nil {
					b.Fatal(err)
				}
			}
			e.srv.db.hasDefaultScopeFlag.forceTrue()
			e.srv.db.hasDefaultScopeConfirmedAtFlag.forceTrue()
			paths := make([]string, txs)
			for i := range paths {
				paths[i] = `["aa","bb","cc"]`
			}
			e.setPaths(nsRepeater, paths...)
			rows := make([]advertIntervalNodeRow, 0, 20001)
			for i := 0; i < 20000; i++ {
				rows = append(rows, advertIntervalNodeRow{pubkey: fmt.Sprintf("%064x", i+1)})
			}
			rows = append(rows, advertIntervalNodeRow{pubkey: nsRepeater, flood: estimate(47*3600, "high")})
			sort.Slice(rows, func(i, j int) bool { return rows[i].pubkey < rows[j].pubkey })
			e.publishIntervals(rows...)
			nodes, _ := e.srv.store.getCachedNodesAndPM()
			n, ok := e.c.index.find(nodes, nsRepeater)
			if !ok {
				b.Fatal("no node")
			}
			in := nodecard.NodeCardInput{PubKey: nsRepeater, Role: "repeater"}
			e.srv.addNodePageFields(&in, n)
			if in.AvgHops == nil || in.FloodInterval == nil {
				b.Fatalf("fields missing: %+v", in)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				in := nodecard.NodeCardInput{PubKey: nsRepeater, Role: "repeater"}
				e.srv.addNodePageFields(&in, n)
			}
		})
	}
}

// nodeCardBytes is the PNG the card renderer draws for in.
func nodeCardBytes(t *testing.T, in nodecard.NodeCardInput) []byte {
	t.Helper()
	b, err := nodecard.Render(in, nodecard.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
