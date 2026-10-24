package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Read-only replay of hop resolution over lifetime vs decayed
// neighbor_edges counts (#544, PR #574 review F2).
//
// Decay is opt-in until its effect on hop disambiguation has been measured
// on real data. This replay is that measurement. It resolves every
// ambiguous hop of the retained packets twice, once against each
// snapshot, through the server's own resolvers:
//   - live: resolvePathForObs, the per-observation resolver used at
//     ingest, which chains the previous resolved hop into the context;
//   - analytics: resolveWithContext with buildHopContextPubkeys, the
//     per-transmission context the distance, topology and RF analytics use.
// Each ambiguous hop is kept (same node), flipped (another node), lost
// (resolved before, not after) or gained (the reverse). Hops with a unique
// prefix cannot change and are only counted.
//
// The two snapshots come from cmd/ingestor's
// TestNeighborEdgeDecayReplay_Prepare (see scripts/neighbor-edge-decay-replay.sh),
// which builds both from one copy of a database: identical edges and
// last_seen, counts with and without decay. Any two databases with the
// same nodes work, for example a copy taken after decay ran for real.
// Both are opened read-only.

type decayReplayTally struct {
	Unique, Ambiguous, Kept, Flipped, Lost, Gained, Unresolved int
}

func (t *decayReplayTally) add(pm *prefixMap, hop string, before, after *string) {
	if c := pm.m[strings.ToLower(hop)]; len(c) < 2 {
		t.Unique++
		return
	}
	t.Ambiguous++
	switch {
	case before == nil && after == nil:
		t.Unresolved++
	case before == nil:
		t.Gained++
	case after == nil:
		t.Lost++
	case *before == *after:
		t.Kept++
	default:
		t.Flipped++
	}
}

func (t decayReplayTally) String() string {
	pct := func(n int) string {
		if t.Ambiguous == 0 {
			return "-"
		}
		return fmt.Sprintf("%.2f%%", 100*float64(n)/float64(t.Ambiguous))
	}
	return fmt.Sprintf("ambiguous hops %d (unique %d): kept %d, flipped %d (%s), lost %d (%s), gained %d (%s), unresolved both %d",
		t.Ambiguous, t.Unique, t.Kept, t.Flipped, pct(t.Flipped), t.Lost, pct(t.Lost), t.Gained, pct(t.Gained), t.Unresolved)
}

type decayReplayReport struct {
	Transmissions, Observations int
	Live, Analytics             decayReplayTally
}

func lowerPK(n *nodeInfo) *string {
	if n == nil {
		return nil
	}
	pk := strings.ToLower(n.PublicKey)
	return &pk
}

// replayNeighborEdgeDecay resolves the hops of txs against both graphs.
func replayNeighborEdgeDecay(txs []*StoreTx, pm *prefixMap, before, after *NeighborGraph) decayReplayReport {
	var r decayReplayReport
	for _, tx := range txs {
		r.Transmissions++
		hops := txGetParsedPath(tx)
		if len(hops) > 0 {
			ctx := buildHopContextPubkeys(tx, pm)
			for _, hop := range hops {
				b, _, _ := pm.resolveWithContext(hop, ctx, before)
				a, _, _ := pm.resolveWithContext(hop, ctx, after)
				r.Analytics.add(pm, hop, lowerPK(b), lowerPK(a))
			}
		}
		for _, obs := range tx.Observations {
			r.Observations++
			obsHops := parsePathJSON(obs.PathJSON)
			if len(obsHops) == 0 {
				continue
			}
			b := resolvePathForObs(obs.PathJSON, obs.ObserverID, tx, pm, before)
			a := resolvePathForObs(obs.PathJSON, obs.ObserverID, tx, pm, after)
			for i, hop := range obsHops {
				r.Live.add(pm, hop, b[i], a[i])
			}
		}
	}
	return r
}

// The replay counts a flip on the review's F1 scenario, where recent
// traffic outranks old lifetime credit, and leaves unique hops and hops
// without affinity alone.
func TestNeighborEdgeDecayReplay_CountsRecentTrafficFlip(t *testing.T) {
	target, a, b, solo := pk64("7a7a"), pk64("b001"), pk64("b002"), pk64("c3c3")
	pm := buildPrefixMap([]nodeInfo{
		{PublicKey: target, Name: "T", Role: "repeater"},
		{PublicKey: a, Name: "A", Role: "repeater"},
		{PublicKey: b, Name: "B", Role: "repeater"},
		{PublicKey: solo, Name: "S", Role: "repeater"},
	})
	graph := func(countA, countB int) *NeighborGraph {
		return persistedNeighborGraph(t, []persistedEdge{
			{target, a, countA, 97 * time.Hour},
			{target, b, countB, time.Hour},
		})
	}
	lifetime, decayed := graph(401, 20), graph(5, 17)
	txs := []*StoreTx{
		// Heard by the target over "b0", then the unique "c3".
		{ObserverID: target, PathJSON: `["b0","c3"]`, DecodedJSON: `{}`,
			Observations: []*StoreObs{{ObserverID: target, PathJSON: `["b0","c3"]`}}},
		// Heard by S, which has no edges: no affinity on either side.
		{ObserverID: solo, PathJSON: `["b0"]`, DecodedJSON: `{}`,
			Observations: []*StoreObs{{ObserverID: solo, PathJSON: `["b0"]`}}},
	}
	r := replayNeighborEdgeDecay(txs, pm, lifetime, decayed)
	want := decayReplayTally{Unique: 1, Ambiguous: 2, Flipped: 1}
	for name, got := range map[string]decayReplayTally{"live": r.Live, "analytics": r.Analytics} {
		// The hop heard by S resolves by a lower tier, identically on both
		// sides, or not at all.
		if got.Kept+got.Unresolved != 1 {
			t.Errorf("%s: %s, want the hop without affinity kept or unresolved", name, got)
		}
		got.Kept, got.Unresolved = 0, 0
		if got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	if r.Transmissions != 2 || r.Observations != 2 {
		t.Errorf("replayed %d transmissions, %d observations; want 2, 2", r.Transmissions, r.Observations)
	}
	if same := replayNeighborEdgeDecay(txs, pm, lifetime, lifetime); same.Live.Flipped+same.Live.Lost+same.Live.Gained+same.Analytics.Flipped+same.Analytics.Lost+same.Analytics.Gained != 0 {
		t.Errorf("identical snapshots reported changes: live %s; analytics %s", same.Live, same.Analytics)
	}
}

// TestNeighborEdgeDecayReplay_DBCopy runs the replay on two database
// copies. It is skipped unless CORESCOPE_DECAY_REPLAY_LIFETIME_DB and
// CORESCOPE_DECAY_REPLAY_DECAYED_DB are set; packets come from the
// lifetime copy (CORESCOPE_DECAY_REPLAY_HOURS of them, default 168).
// Both databases are opened read-only. Never point it at a live database.
func TestNeighborEdgeDecayReplay_DBCopy(t *testing.T) {
	lifetimePath, decayedPath := os.Getenv("CORESCOPE_DECAY_REPLAY_LIFETIME_DB"), os.Getenv("CORESCOPE_DECAY_REPLAY_DECAYED_DB")
	if lifetimePath == "" || decayedPath == "" {
		t.Skip("set CORESCOPE_DECAY_REPLAY_LIFETIME_DB and CORESCOPE_DECAY_REPLAY_DECAYED_DB (see scripts/neighbor-edge-decay-replay.sh)")
	}
	hours := 168.0
	if v := os.Getenv("CORESCOPE_DECAY_REPLAY_HOURS"); v != "" {
		h, err := strconv.ParseFloat(v, 64)
		if err != nil || h < 0 {
			t.Fatalf("CORESCOPE_DECAY_REPLAY_HOURS=%q: want hours >= 0 (0 = all)", v)
		}
		hours = h
	}
	lifetimeDB, err := OpenDB(lifetimePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lifetimeDB.Close()
	decayedDB, err := OpenDB(decayedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer decayedDB.Close()

	before, after := loadNeighborEdgesFromDB(lifetimeDB.conn), loadNeighborEdgesFromDB(decayedDB.conn)
	store := NewPacketStore(lifetimeDB, &PacketStoreConfig{RetentionHours: hours})
	store.graph.Store(before) // main.go loads the graph before the packets (#1643)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	_, pm := store.getCachedNodesAndPM()
	store.mu.RLock()
	txs := append([]*StoreTx(nil), store.packets...)
	store.mu.RUnlock()

	edgeStats := func(g *NeighborGraph) string {
		edges := g.AllEdges()
		eligible, saturated := 0, 0
		for _, e := range edges {
			if e.Count >= affinityMinObservations {
				eligible++
			}
			if e.Count >= 100 {
				saturated++
			}
		}
		return fmt.Sprintf("%d edges, %d with count >= %d, %d with count >= 100", len(edges), eligible, affinityMinObservations, saturated)
	}
	start := time.Now()
	r := replayNeighborEdgeDecay(txs, pm, before, after)
	t.Logf("lifetime snapshot: %s", edgeStats(before))
	t.Logf("decayed snapshot:  %s", edgeStats(after))
	window := "all retained packets"
	if hours > 0 {
		window = fmt.Sprintf("last %.0f h", hours)
	}
	t.Logf("replayed %d transmissions, %d observations (%s) in %s", r.Transmissions, r.Observations, window, time.Since(start).Round(time.Millisecond))
	t.Logf("live (resolvePathForObs):       %s", r.Live)
	t.Logf("analytics (resolveWithContext): %s", r.Analytics)
	if r.Transmissions == 0 {
		t.Fatal("no packets in the replay window; the measurement would be empty")
	}
}
