package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestNeighborGraph952_MeasureFixture builds the in-memory neighbour graph
// from a fixture DB and writes its edges, the route-type share of the
// observations with a path, and the build time to a JSON file, so the effect
// of #952 can be compared before and after. It compiles against master too.
//
//	CORESCOPE_952_MEASURE=out.json [CORESCOPE_952_DB=path.db] go test -run NeighborGraph952_MeasureFixture -v
func TestNeighborGraph952_MeasureFixture(t *testing.T) {
	out := os.Getenv("CORESCOPE_952_MEASURE")
	if out == "" {
		t.Skip("set CORESCOPE_952_MEASURE=<output.json> to run")
	}
	src := os.Getenv("CORESCOPE_952_DB")
	if src == "" {
		src = filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	}
	db, err := OpenDB(rpm2CopyFile(t, src))
	if err != nil {
		t.Fatal(err)
	}
	defer db.conn.Close()
	store := NewPacketStore(db, &PacketStoreConfig{})
	store.graph.Store(loadNeighborEdgesFromDB(db.conn))
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	store.WaitIndexesReady(60 * time.Second)

	type edgeOut struct {
		A, B, Prefix string // Prefix tells ambiguous edges (B == "") apart
		Count        int
		Observer     bool
	}
	type result struct {
		Transmissions, Observations, WithPath int
		WithPathByRoute                       map[string]int
		Edges, ObserverEdges                  int
		BuildMedianMs                         float64
		BuildRuns                             int
		EdgeList                              []edgeOut
	}
	res := result{WithPathByRoute: map[string]int{}}
	observers := map[string]bool{}
	store.mu.RLock()
	res.Transmissions = len(store.packets)
	for _, tx := range store.packets {
		for _, o := range tx.Observations {
			if o.Direction == "tx" {
				continue
			}
			res.Observations++
			observers[strings.ToLower(o.ObserverID)] = true
			if len(parsePathJSON(o.PathJSON)) == 0 {
				continue
			}
			res.WithPath++
			name := "unknown"
			if rt, ok := observationRouteType(o, tx); ok {
				name = map[int]string{RouteTransportFlood: "transport_flood", RouteFlood: "flood", RouteDirect: "direct", RouteTransportDirect: "transport_direct"}[rt]
			}
			res.WithPathByRoute[name]++
		}
	}
	store.mu.RUnlock()

	var g *NeighborGraph
	var times []time.Duration
	deadline := time.Now().Add(5 * time.Second)
	for len(times) < 21 || (time.Now().Before(deadline) && len(times) < 201) {
		start := time.Now()
		g = BuildFromStore(store)
		times = append(times, time.Since(start))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	res.BuildRuns = len(times)
	res.BuildMedianMs = float64(times[len(times)/2].Microseconds()) / 1000

	for _, e := range g.AllEdges() {
		obs := observers[e.NodeA] || observers[e.NodeB]
		res.EdgeList = append(res.EdgeList, edgeOut{e.NodeA, e.NodeB, e.Prefix, e.Count, obs})
		if obs {
			res.ObserverEdges++
		}
	}
	res.Edges = len(res.EdgeList)
	sort.Slice(res.EdgeList, func(i, j int) bool {
		x, y := res.EdgeList[i], res.EdgeList[j]
		if x.A != y.A {
			return x.A < y.A
		}
		if x.B != y.B {
			return x.B < y.B
		}
		return x.Prefix < y.Prefix
	})
	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("tx=%d obs=%d withPath=%d byRoute=%v edges=%d observerEdges=%d build median %.2f ms over %d runs",
		res.Transmissions, res.Observations, res.WithPath, res.WithPathByRoute, res.Edges, res.ObserverEdges, res.BuildMedianMs, res.BuildRuns)
}
