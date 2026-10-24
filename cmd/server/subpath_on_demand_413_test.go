package main

// Issue #413: Route Patterns counts subpaths on demand from the packets
// instead of keeping the permanent spIndex/spTxIndex up to date on every
// ingest, path change, merge and eviction.
//
// The tests compare the on-demand endpoints with legacy413Index, a frozen copy
// of the index the store kept up to master 0350322e (addTxToSubpathIndexFull,
// removeTxFromSubpathIndexFull, the unbounded aggregation of
// GetAnalyticsSubpathsBulk, the sort of rankSubpaths and SubpathDetail),
// maintained through the same ingest, path-change and eviction steps. Counts,
// rawHops, ranking, tie-breaks, the 8-hop cap, case folding, totalPaths and
// the detail must come out identical.
//
// The legacy copy differs from master in two places, each on purpose:
//   - total is decremented on eviction. Master's eviction removed the
//     subpaths but never decremented spTotalPaths, so totalPaths (and every
//     pct) drifted upward on a long-running server; TestSubpathTotalPaths
//     FollowsEviction_413 pins the fix.
//   - the detail takes the matched transmissions in packet order. The index
//     held them in insertion order, which a path change rearranged; nothing
//     the detail reports depends on that order except the float summation
//     order of the signal averages.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Legacy reference (frozen copy of the removed index) ────────────────────

type legacy413Index struct {
	counts map[string]int
	txs    map[string][]*StoreTx
	total  int
}

func newLegacy413Index(packets []*StoreTx) *legacy413Index {
	x := &legacy413Index{counts: map[string]int{}, txs: map[string][]*StoreTx{}}
	for _, tx := range packets {
		x.add(tx, txGetParsedPath(tx))
	}
	return x
}

// add is addTxToSubpathIndexFull plus the spTotalPaths++ of its callers.
func (x *legacy413Index) add(tx *StoreTx, hops []string) {
	if len(hops) < 2 {
		return
	}
	maxL := min(8, len(hops))
	for l := 2; l <= maxL; l++ {
		for start := 0; start <= len(hops)-l; start++ {
			key := strings.ToLower(strings.Join(hops[start:start+l], ","))
			x.counts[key]++
			x.txs[key] = append(x.txs[key], tx)
		}
	}
	x.total++
}

// remove is removeTxFromSubpathIndexFull plus spTotalPaths--.
func (x *legacy413Index) remove(tx *StoreTx, hops []string) {
	if len(hops) < 2 {
		return
	}
	maxL := min(8, len(hops))
	for l := 2; l <= maxL; l++ {
		for start := 0; start <= len(hops)-l; start++ {
			key := strings.ToLower(strings.Join(hops[start:start+l], ","))
			x.counts[key]--
			if x.counts[key] <= 0 {
				delete(x.counts, key)
			}
			txs := x.txs[key]
			for i, t := range txs {
				if t == tx {
					x.txs[key] = append(txs[:i], txs[i+1:]...)
					break
				}
			}
			if len(x.txs[key]) == 0 {
				delete(x.txs, key)
			}
		}
	}
	x.total--
}

func legacy413Resolver(s *PacketStore, ctxTxs []*StoreTx) (func(string) string, *prefixMap) {
	_, pm := s.getCachedNodesAndPM()
	contextPubkeys := buildAggregateHopContextPubkeys(ctxTxs, pm)
	hopCache := make(map[string]*nodeInfo)
	graph := s.graph.Load()
	return func(hop string) string {
		if cached, ok := hopCache[hop]; ok {
			if cached != nil {
				return cached.Name
			}
			return hop
		}
		r, _, _ := pm.resolveWithContext(hop, contextPubkeys, graph)
		hopCache[hop] = r
		if r != nil {
			return r.Name
		}
		return hop
	}, pm
}

// legacy413Bulk is the unbounded aggregation over spIndex
// (GetAnalyticsSubpathsBulk, and computeAnalyticsSubpaths for one group).
func legacy413Bulk(s *PacketStore, x *legacy413Index, groups []subpathGroup) []map[string]interface{} {
	resolveHop, _ := legacy413Resolver(s, s.packets)
	perGroup := make([]map[string]*subpathAccum, len(groups))
	for i := range groups {
		perGroup[i] = make(map[string]*subpathAccum)
	}
	for rawKey, count := range x.counts {
		hops := strings.Split(rawKey, ",")
		var namedKey string
		resolved := false
		for gi, g := range groups {
			if len(hops) < g.MinLen || len(hops) > g.MaxLen {
				continue
			}
			if !resolved {
				named := make([]string, len(hops))
				for i, h := range hops {
					named[i] = resolveHop(h)
				}
				namedKey = strings.Join(named, " → ")
				resolved = true
			}
			entry := perGroup[gi][namedKey]
			if entry == nil {
				entry = &subpathAccum{}
				perGroup[gi][namedKey] = entry
			}
			entry.add(rawKey, count)
		}
	}
	results := make([]map[string]interface{}, len(groups))
	for i, g := range groups {
		results[i] = legacy413Rank(perGroup[i], x.total, g.Limit)
	}
	return results
}

// legacy413Windowed is computeAnalyticsSubpathsWindowed/computeSubpathsSlow:
// per transmission, named keys, original-case rawHops, no 8-hop cap.
func legacy413Windowed(s *PacketStore, regionObs map[string]bool, minLen, maxLen, limit int, window TimeWindow) map[string]interface{} {
	resolveHop, _ := legacy413Resolver(s, s.packets)
	subpathCounts := make(map[string]*subpathAccum)
	totalPaths := 0
	for _, tx := range s.packets {
		if !window.Includes(tx.FirstSeen) {
			continue
		}
		hops := txGetParsedPath(tx)
		if len(hops) < 2 {
			continue
		}
		if regionObs != nil {
			match := false
			for _, obs := range tx.Observations {
				if regionObs[obs.ObserverID] {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		totalPaths++
		named := make([]string, len(hops))
		for i, h := range hops {
			named[i] = resolveHop(h)
		}
		for l := minLen; l <= maxLen && l <= len(named); l++ {
			for start := 0; start <= len(named)-l; start++ {
				sub := strings.Join(named[start:start+l], " → ")
				raw := strings.Join(hops[start:start+l], ",")
				entry := subpathCounts[sub]
				if entry == nil {
					entry = &subpathAccum{}
					subpathCounts[sub] = entry
				}
				entry.add(raw, 1)
			}
		}
	}
	return legacy413Rank(subpathCounts, totalPaths, limit)
}

// legacy413Rank is rankSubpaths with its full sort.
func legacy413Rank(counts map[string]*subpathAccum, totalPaths, limit int) map[string]interface{} {
	type subpathEntry struct {
		path  string
		count int
		raw   string
	}
	ranked := make([]subpathEntry, 0, len(counts))
	for path, data := range counts {
		ranked = append(ranked, subpathEntry{path, data.count, data.raw})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].path < ranked[j].path
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	subpaths := make([]map[string]interface{}, 0, len(ranked))
	for _, e := range ranked {
		subpaths = append(subpaths, map[string]interface{}{
			"path":    e.path,
			"rawHops": strings.Split(e.raw, ","),
			"count":   e.count,
			"hops":    len(strings.Split(e.path, " → ")),
			"pct":     subpathPct(e.count, totalPaths),
		})
	}
	return map[string]interface{}{"subpaths": subpaths, "totalPaths": totalPaths}
}

// legacy413Detail is SubpathDetail over spTxIndex, matched transmissions in
// packet order (see the file comment).
func legacy413Detail(s *PacketStore, x *legacy413Index, rawHops []string) SubpathDetailResponse {
	pos := make(map[*StoreTx]int, len(s.packets))
	for i, tx := range s.packets {
		pos[tx] = i
	}
	matchedTxs := append([]*StoreTx(nil), x.txs[strings.ToLower(strings.Join(rawHops, ","))]...)
	sort.SliceStable(matchedTxs, func(i, j int) bool { return pos[matchedTxs[i]] < pos[matchedTxs[j]] })

	_, pm := s.getCachedNodesAndPM()
	contextPubkeys := buildAggregateHopContextPubkeys(matchedTxs, pm)
	graph := s.graph.Load()
	p := &subpathDetailParts{
		rawHops: rawHops, nodes: make([]*nodeInfo, len(rawHops)), pm: pm, graph: graph,
		hopContext: contextPubkeys, matchCount: len(matchedTxs), hourBuckets: make([]int, 24),
	}
	for i, hop := range rawHops {
		if r, _, _ := pm.resolveWithContext(hop, contextPubkeys, graph); r != nil {
			n := *r
			p.nodes[i] = &n
		}
	}
	parents := map[string]*subpathDetailParent{}
	observers := map[[2]string]int{}
	for _, tx := range matchedTxs {
		ts := tx.FirstSeen
		if ts != "" {
			if p.firstSeen == "" || ts < p.firstSeen {
				p.firstSeen = ts
			}
			if p.lastSeen == "" || ts > p.lastSeen {
				p.lastSeen = ts
			}
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				t, err = time.Parse("2006-01-02 15:04:05", ts)
			}
			if err == nil {
				p.hourBuckets[t.Hour()]++
			}
		}
		if tx.SNR != nil {
			p.snrSum += *tx.SNR
			p.snrCount++
		}
		if tx.RSSI != nil {
			p.rssiSum += *tx.RSSI
			p.rssiCount++
		}
		if tx.ObserverName != "" {
			observers[[2]string{tx.ObserverID, tx.ObserverName}]++
		}
		txCtx := buildHopContextPubkeys(tx, pm)
		hops := txGetParsedPath(tx)
		pathHops := make([]subpathDetailHop, len(hops))
		key := make([]string, 0, 2*len(hops))
		for i, h := range hops {
			pathHops[i].raw = h
			if r, _, _ := pm.resolveWithContext(h, txCtx, graph); r != nil {
				pathHops[i].pubkey, pathHops[i].name = r.PublicKey, r.Name
			}
			key = append(key, h, pathHops[i].pubkey)
		}
		k := strings.Join(key, "\x00")
		if pp := parents[k]; pp != nil {
			pp.count++
		} else {
			parents[k] = &subpathDetailParent{hops: pathHops, count: 1}
		}
	}
	for _, pp := range parents {
		p.parents = append(p.parents, *pp)
	}
	for o, count := range observers {
		p.observers = append(p.observers, subpathDetailObserver{id: o[0], name: o[1], count: count})
	}
	return p.response(nil)
}

// ─── Shared data ─────────────────────────────────────────────────────────────

// subpath413Mesh is subpath413Data plus the cases the index treated
// specially: lower- and mixed-case hops of the same prefix (the index folded
// case), a path that repeats a subpath (each occurrence counts), paths far
// beyond 8 hops (the index stopped at 8), two nodes sharing a name (several
// raw keys behind one path, rawHops the smallest), hops that resolve to no
// node, one observation per packet for the region filter, and a few packets
// without a path.
func subpath413Mesh(numTx, numNodes int, seed int64) ([]nodeInfo, []*StoreTx) {
	nodes, packets := subpath413Data(numTx, numNodes, seed)
	rng := rand.New(rand.NewSource(seed * 7919))
	nodes[4].Name = nodes[8].Name // two repeaters, one name
	for i, tx := range packets {
		hops := parsePathJSON(tx.PathJSON)
		switch i % 11 {
		case 1:
			for h := range hops {
				hops[h] = strings.ToLower(hops[h])
			}
		case 2:
			if len(hops) > 0 {
				hops[0] = strings.ToLower(hops[0][:1]) + hops[0][1:]
			}
		case 3:
			if len(hops) >= 2 {
				hops = append(hops, hops[0], hops[1], hops[0], hops[1])
			}
		case 4:
			for len(hops) < 40 {
				hops = append(hops, hops...)
				if len(hops) == 0 {
					hops = []string{"7F", "80"}
				}
			}
		case 5:
			if len(hops) >= 2 {
				hops[1] = fmt.Sprintf("Z%d", rng.Intn(3)) // matches no node
			}
		}
		pj, _ := json.Marshal(hops)
		tx.PathJSON = string(pj)
		tx.parsedPath, tx.pathParsed = nil, false
		tx.Observations = []*StoreObs{{ObserverID: tx.ObserverID, ObserverName: tx.ObserverName}}
	}
	return nodes, packets
}

// subpath413Region gives region "R1" every other repeater as its observers.
func subpath413Region(s *PacketStore, nodes []nodeInfo) map[string]bool {
	obs := map[string]bool{}
	for i, n := range nodes {
		if n.Role == "repeater" && i%8 == 0 {
			obs[n.PublicKey] = true
		}
	}
	s.regionObsMu.Lock()
	s.regionObsCache = map[string]map[string]bool{"R1": obs}
	s.regionObsCacheTime = time.Now().Add(time.Hour)
	s.regionObsMu.Unlock()
	return obs
}

func clearSubpathCache413(s *PacketStore) {
	s.cacheMu.Lock()
	s.subpathCache = make(map[string]*cachedResult)
	s.cacheMu.Unlock()
}

var subpath413Groups = []subpathGroup{
	{2, 2, 50}, {3, 3, 30}, {4, 4, 20}, {5, 8, 15}, // the Route Patterns tab
	{2, 8, 100}, {2, 63, 1000}, {3, 3, 7}, {9, 63, 100}, {8, 8, 1},
}

// ─── Equivalence: on-demand == legacy index ─────────────────────────────────

func TestSubpathsOnDemandMatchLegacyIndex_413(t *testing.T) {
	for _, seed := range []int64{1, 2} {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			nodes, all := subpath413Mesh(1200, 320, seed)
			rng := rand.New(rand.NewSource(seed))
			s := newSubpath413Store(nodes, append([]*StoreTx(nil), all[:800]...))
			region := subpath413Region(s, nodes)
			x := newLegacy413Index(s.packets)
			check413(t, "loaded", s, x, region)

			// Ingest.
			for _, tx := range all[800:] {
				s.packets = append(s.packets, tx)
				x.add(tx, txGetParsedPath(tx))
			}
			// Best path changes (reindexTxPath): another packet's path.
			for i := 0; i < 100; i++ {
				tx := s.packets[rng.Intn(len(s.packets))]
				x.remove(tx, txGetParsedPath(tx))
				tx.PathJSON = s.packets[rng.Intn(len(s.packets))].PathJSON
				tx.parsedPath, tx.pathParsed = parsePathJSON(tx.PathJSON), true
				x.add(tx, txGetParsedPath(tx))
			}
			// Eviction of the oldest.
			for _, tx := range s.packets[:300] {
				x.remove(tx, txGetParsedPath(tx))
			}
			s.packets = s.packets[300:]
			check413(t, "after ingest, path changes and eviction", s, x, region)
		})
	}
}

func check413(t *testing.T, stage string, s *PacketStore, x *legacy413Index, region map[string]bool) {
	t.Helper()
	t.Run("counts and ranking", func(t *testing.T) {
		for _, g := range subpath413Groups {
			clearSubpathCache413(s)
			got := s.GetAnalyticsSubpaths("", g.MinLen, g.MaxLen, g.Limit)
			want := legacy413Bulk(s, x, []subpathGroup{g})[0]
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: group %v differs from the index\n%s", stage, g, diff413(got, want))
			}
		}
	})
	t.Run("bulk", func(t *testing.T) {
		clearSubpathCache413(s)
		got := s.GetAnalyticsSubpathsBulk("", subpath413Groups)
		want := legacy413Bulk(s, x, subpath413Groups)
		for i := range want {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Errorf("%s: bulk group %v differs from the index\n%s", stage, subpath413Groups[i], diff413(got[i], want[i]))
			}
		}
	})
	t.Run("window", func(t *testing.T) {
		since := s.packets[len(s.packets)/2].FirstSeen
		w := TimeWindow{Since: since, Label: "custom"}
		for _, g := range []subpathGroup{{2, 8, 100}, {3, 40, 25}} {
			clearSubpathCache413(s)
			got := s.GetAnalyticsSubpathsWithWindow("", g.MinLen, g.MaxLen, g.Limit, w)
			want := legacy413Windowed(s, nil, g.MinLen, g.MaxLen, g.Limit, w)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: window %v group %v differs\n%s", stage, w, g, diff413(got, want))
			}
		}
	})
	t.Run("region", func(t *testing.T) {
		clearSubpathCache413(s)
		got := s.GetAnalyticsSubpaths("R1", 2, 12, 60)
		want := legacy413Windowed(s, region, 2, 12, 60, TimeWindow{})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: region differs\n%s", stage, diff413(got, want))
		}
	})
	t.Run("detail", func(t *testing.T) {
		hot, cold := subpath413DetailKeys(s.packets)
		var long []string
		for _, tx := range s.packets {
			if hops := txGetParsedPath(tx); len(hops) >= 9 {
				long = hops[:9] // beyond the 8-hop cap: no match, as with the index
				break
			}
		}
		upper := make([]string, len(hot))
		for i, h := range hot {
			upper[i] = strings.ToUpper(h)
		}
		for _, q := range [][]string{hot, cold, upper, long, long[:8], hot[:1], {"zz", "yy"}} {
			got := s.GetSubpathDetail(q)
			want := legacy413Detail(s, x, q)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: detail %v differs\n got  %+v\n want %+v", stage, q, got, want)
			}
		}
	})
}

func diff413(got, want map[string]interface{}) string {
	if got["totalPaths"] != want["totalPaths"] {
		return fmt.Sprintf("totalPaths got %v want %v", got["totalPaths"], want["totalPaths"])
	}
	g, _ := got["subpaths"].([]map[string]interface{})
	w, _ := want["subpaths"].([]map[string]interface{})
	if len(g) != len(w) {
		return fmt.Sprintf("len got %d want %d", len(g), len(w))
	}
	for i := range w {
		if !reflect.DeepEqual(g[i], w[i]) {
			return fmt.Sprintf("entry %d:\n got  %v\n want %v", i, g[i], w[i])
		}
	}
	return "types differ"
}

// ─── totalPaths after eviction ──────────────────────────────────────────────

// The index's eviction removed a packet's subpaths but left spTotalPaths
// alone, so every pct shrank as the server ran. Counted on demand, totalPaths
// is the packets with a 2+ hop path that are still held.
func TestSubpathTotalPathsFollowsEviction_413(t *testing.T) {
	db := setupSubpathWindowDB(t)
	defer db.Close()
	// A recent single-hop packet: held, but not a path Route Patterns count.
	recent := time.Now().UTC().Add(-time.Hour)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES ('03', 'recent_single_hop_003', ?, 1, 4, '{}')`, recent.Format(time.RFC3339))
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (3, 1, 10.0, -90, '["ee"]', ?)`, recent.Unix())
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes not ready")
	}
	if n := len(store.packets); n != 3 {
		t.Fatalf("setup: %d packets loaded, want 3", n)
	}
	store.retentionHours = 7 * 24
	store.mu.Lock()
	evicted := store.EvictStale()
	store.mu.Unlock()
	if evicted != 1 {
		t.Fatalf("evicted %d, want the 30-day-old packet", evicted)
	}

	got := store.GetAnalyticsSubpaths("", 2, 8, 100)
	if tp := got["totalPaths"]; tp != 1 {
		t.Errorf("totalPaths = %v after evicting one of two paths, want 1", tp)
	}
	subs := got["subpaths"].([]map[string]interface{})
	if len(subs) != 1 || subs[0]["path"] != "aa → bb" || subs[0]["pct"] != 100.0 {
		t.Errorf("subpaths = %v, want only aa → bb at 100%%", subs)
	}
}

// ─── No permanent index ─────────────────────────────────────────────────────

func TestSubpathCountsKeepNoPermanentIndex_413(t *testing.T) {
	typ := reflect.TypeOf(PacketStore{})
	for _, f := range []string{"spIndex", "spTxIndex", "spTotalPaths"} {
		if _, ok := typ.FieldByName(f); ok {
			t.Errorf("PacketStore still has %s: Route Patterns must count on demand (#413)", f)
		}
	}

	// The memory budget charged O(hops²) per transmission for the index. With
	// the index gone a transmission's path costs the same per hop at any length.
	tx := func(n int) *StoreTx {
		hops := make([]string, n)
		for i := range hops {
			hops[i] = fmt.Sprintf("%02X", i)
		}
		pj, _ := json.Marshal(hops)
		return &StoreTx{Hash: "h", PathJSON: string(pj)}
	}
	short, long := tx(4), tx(8)
	want := int64(len(long.PathJSON)-len(short.PathJSON)) + 4*perPathHopBytes
	if got := estimateStoreTxBytes(long) - estimateStoreTxBytes(short); got != want {
		t.Errorf("8-hop minus 4-hop estimate = %d bytes, want %d (no per-subpath charge)", got, want)
	}
}

// ─── Ranking: bounded top-N == full sort ────────────────────────────────────

// rankSubpaths keeps the limit best entries in a bounded heap instead of
// sorting all of them. Ties break on the path BEFORE the cut, so the heap must
// select exactly the entries a full sort cut at limit selects, in the same
// order, on every run (#256).
func TestSubpathRankTopNMatchesFullSort_413(t *testing.T) {
	s := &PacketStore{}
	rng := rand.New(rand.NewSource(413))
	for round := 0; round < 12; round++ {
		counts := map[string]*subpathAccum{}
		n := 1 + rng.Intn(300)
		for i := 0; i < n; i++ {
			a := &subpathAccum{}
			for k := 1 + rng.Intn(2); k > 0; k-- {
				a.add(fmt.Sprintf("%02x,%02x", rng.Intn(256), rng.Intn(256)), 1+rng.Intn(3))
			}
			counts[fmt.Sprintf("N%d → N%d", rng.Intn(60), rng.Intn(60))] = a
		}
		total := rng.Intn(1000)
		for _, limit := range []int{0, 1, 2, 7, 50, n - 1, n, n + 5} {
			if limit < 0 {
				continue
			}
			want := legacy413Rank(counts, total, limit)
			assertSameEveryRun(t, fmt.Sprintf("round %d limit %d", round, limit), want, func() interface{} {
				return s.rankSubpaths(counts, total, limit)
			})
		}
	}
}

// ─── Lock: count outside PacketStore.mu ─────────────────────────────────────

// Only the copy of the hop lists (and the hop context) happens under
// PacketStore.mu; enumerating, resolving and ranking happen after it is
// released. The test parks the request inside hop resolution (the resolver
// reads the neighbour graph, whose lock the test holds) and requires a store
// writer to get the lock meanwhile. With the counting under the read lock the
// writer waits until the request is done.
func TestSubpathCountingReleasesStoreLock_413(t *testing.T) {
	nodes := []nodeInfo{
		{PublicKey: "aa11000000000000", Name: "A1", Role: "repeater"},
		{PublicKey: "aa22000000000000", Name: "A2", Role: "repeater"},
		{PublicKey: "bb33000000000000", Name: "B3", Role: "repeater"},
		{PublicKey: "cc44000000000000", Name: "C4", Role: "companion"},
	}
	pt := PayloadTXT_MSG
	var packets []*StoreTx
	for i := 0; i < 3; i++ {
		packets = append(packets, &StoreTx{
			ID: i + 1, Hash: fmt.Sprintf("lk%d", i), PayloadType: &pt,
			FirstSeen: time.Now().UTC().Format(time.RFC3339), PathJSON: `["AA","BB"]`,
			DecodedJSON: `{"pubKey":"cc44000000000000"}`, ObserverID: "obs1", ObserverName: "Obs",
			Observations: []*StoreObs{{ObserverID: "obs1"}},
		})
	}
	since := TimeWindow{Since: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Label: "1h"}
	cases := map[string]func(s *PacketStore){
		"subpaths":       func(s *PacketStore) { s.GetAnalyticsSubpaths("", 2, 8, 10) },
		"subpaths-bulk":  func(s *PacketStore) { s.GetAnalyticsSubpathsBulk("", []subpathGroup{{2, 2, 5}}) },
		"window":         func(s *PacketStore) { s.GetAnalyticsSubpathsWithWindow("", 2, 8, 10, since) },
		"region":         func(s *PacketStore) { s.GetAnalyticsSubpaths("R1", 2, 8, 10) },
		"subpath-detail": func(s *PacketStore) { s.GetSubpathDetail([]string{"aa", "bb"}) },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSubpath413Store(nodes, packets)
			s.regionObsCache = map[string]map[string]bool{"R1": {"obs1": true}}
			s.regionObsCacheTime = time.Now().Add(time.Hour)
			g := NewNeighborGraph()
			s.graph.Store(g)

			g.mu.Lock()
			graphLocked := true
			unlockGraph := func() {
				if graphLocked {
					graphLocked = false
					g.mu.Unlock()
				}
			}
			defer unlockGraph()
			done := make(chan struct{})
			go func() { defer close(done); call(s) }()
			defer func() { unlockGraph(); <-done }()

			// Wait until the request is parked on the graph lock.
			deadline := time.Now().Add(10 * time.Second)
			for !strings.Contains(allStacks413(), "(*NeighborGraph).Neighbors") {
				if time.Now().After(deadline) {
					t.Fatal("the request never reached hop resolution")
				}
				runtime.Gosched()
			}
			deadline = time.Now().Add(2 * time.Second)
			for !s.mu.TryLock() {
				if time.Now().After(deadline) {
					t.Fatal("a store writer could not take PacketStore.mu while the request resolved hops: counting runs under the lock")
				}
				runtime.Gosched()
			}
			s.mu.Unlock()
		})
	}

	// The detail matches the subpath against every packet; that scan must
	// not hold the lock either (PR #529 review F2), only the copy of the hop
	// lists before it and of the matched transmissions after it.
	t.Run("subpath-detail-match", func(t *testing.T) {
		s := newSubpath413Store(nodes, packets)
		parked, release := make(chan struct{}), make(chan struct{})
		s.subpathHook = func(stage string) {
			if stage == "match" {
				close(parked)
				<-release
			}
		}
		done := make(chan struct{})
		go func() { defer close(done); s.GetSubpathDetail([]string{"aa", "bb"}) }()
		defer func() { <-done }()
		defer close(release)
		select {
		case <-parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the detail never reached matching")
		}
		deadline := time.Now().Add(2 * time.Second)
		for !s.mu.TryLock() {
			if time.Now().After(deadline) {
				t.Fatal("a store writer could not take PacketStore.mu while the detail matched: matching runs under the lock")
			}
			runtime.Gosched()
		}
		s.mu.Unlock()
	})
}

func allStacks413() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

// ─── Concurrency: snapshot reads vs. writers (run with -race) ───────────────

// Writers change paths, signal and observers in place, append and evict
// under PacketStore.mu while requests run, as ingest and eviction do. Under
// -race any read of a transmission after the lock is released is reported.
func TestSubpathOnDemandConcurrentWriters_413(t *testing.T) {
	nodes, all := subpath413Mesh(900, 200, 9)
	s := newSubpath413Store(nodes, append([]*StoreTx(nil), all[:600]...))
	subpath413Region(s, nodes)
	hot, _ := subpath413DetailKeys(s.packets)
	since := TimeWindow{Since: s.packets[300].FirstSeen, Label: "custom"}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(1))
		next := 600
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			tx := s.packets[rng.Intn(len(s.packets))]
			tx.PathJSON = s.packets[rng.Intn(len(s.packets))].PathJSON
			tx.parsedPath, tx.pathParsed = parsePathJSON(tx.PathJSON), true
			snr := float64(i % 20)
			tx.SNR, tx.ObserverName, tx.FirstSeen = &snr, fmt.Sprint("obs", i%5), tx.FirstSeen+""
			if next < len(all) {
				txGetParsedPath(all[next])
				s.packets = append(s.packets, all[next])
				next++
				s.packets = s.packets[1:]
			}
			s.mu.Unlock()
			runtime.Gosched()
		}
	}()
	for i := 0; i < 15; i++ {
		clearSubpathCache413(s)
		s.GetAnalyticsSubpaths("", 2, 8, 50)
		s.GetAnalyticsSubpathsBulk("", subpath413Groups[:4])
		s.GetAnalyticsSubpathsWithWindow("", 2, 8, 50, since)
		s.GetAnalyticsSubpaths("R1", 2, 8, 50)
		s.GetSubpathDetail(hot)
	}
	close(stop)
	wg.Wait()
}
