package main

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Follow-ups from #541 (issue #545).

func followupNodes() []nodeInfo {
	return []nodeInfo{
		{Role: "repeater", PublicKey: "aaaa1111", Name: "A_SF", Lat: 37.77, Lon: -122.41, HasGPS: true},
		{Role: "repeater", PublicKey: "cccc3333", Name: "C_SJ", Lat: 37.34, Lon: -121.89, HasGPS: true}, // ~68 km from A
		{Role: "repeater", PublicKey: "obs00001", Name: "Observer", Lat: 37.77, Lon: -122.41, HasGPS: true},
	}
}

func followupStore() *PacketStore {
	// Enough adverts to pass the default-shape filter (minCount=5, minScore=0.1).
	var txs []*StoreTx
	for i := 1; i <= 20; i++ {
		txs = append(txs, ngMakeTx(i, PayloadADVERT, ngFromNodeJSON("aaaa1111"), []*StoreObs{
			ngMakeObs("obs00001", `["cccc"]`, nowStr, ngFloatPtr(-10)),
		}))
	}
	return ngTestStore(followupNodes(), txs)
}

// F1: the rebuild cadence is derived from the TTL, not the response-cache interval.
func TestNeighborGraphRebuildEvery(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     int
	}{
		{0, 1},
		{-time.Second, 1},
		{neighborGraphCacheInterval, 1}, // default behaves as before
		{neighborGraphTTL, 1},
		{2 * neighborGraphTTL, 1}, // slower than the TTL: every tick
		{time.Minute, 5},
		{90 * time.Second, 4}, // ceil(300/90): never rebuilds more often than the TTL
		{10 * time.Second, 30},
	}
	for _, c := range cases {
		if got := neighborGraphRebuildEvery(c.interval); got != c.want {
			t.Errorf("neighborGraphRebuildEvery(%v) = %d, want %d", c.interval, got, c.want)
		}
	}
}

// F1: a fast response-cache cadence must not rebuild the graph on every tick.
func TestNeighborGraphFastCacheCadenceDoesNotRebuildGraphEveryTick(t *testing.T) {
	srv := &Server{store: followupStore()}
	stop := make(chan struct{})
	defer close(stop)
	srv.startNeighborGraphRecomputer(5*time.Millisecond, stop)

	deadline := time.Now().Add(5 * time.Second)
	for srv.neighborGraphCache.ptr.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("first recompute never published")
		}
		time.Sleep(time.Millisecond)
	}
	first := srv.neighborGraph.Load()
	if first == nil {
		t.Fatal("first tick did not publish a graph")
	}
	seen := map[*neighborGraphCacheEntry]bool{}
	for len(seen) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only saw %d cache ticks", len(seen))
		}
		seen[srv.neighborGraphCache.ptr.Load()] = true
		time.Sleep(time.Millisecond)
	}
	if srv.neighborGraph.Load() != first {
		t.Fatal("graph was rebuilt on a cache tick well inside neighborGraphTTL")
	}
}

// F2: the cached response is built from the graph the same tick just produced.
func TestNeighborGraphCacheBuiltFromSameTickGraph(t *testing.T) {
	stale := NewNeighborGraph() // empty, as a previous tick's graph would be before new traffic
	stale.builtAt = time.Now()
	srv := &Server{store: followupStore()}
	srv.neighborGraph.Store(stale)
	srv.recomputeNeighborGraphCache()

	entry := srv.neighborGraphCache.unfilteredPtr.Load()
	if entry == nil {
		t.Fatal("no unfiltered cache entry published")
	}
	if len(entry.resp.Edges) == 0 {
		t.Fatal("cached response was built from the previous graph, not the one this tick produced")
	}
	def := srv.neighborGraphCache.ptr.Load()
	if def == nil || len(def.resp.Edges) == 0 {
		t.Fatal("default-shape cached response was built from the previous graph")
	}
}

// F2 (skipped rebuild): a tick that skips the graph rebuild still builds from the published graph.
func TestNeighborGraphCacheTickWithoutRebuildKeepsGraph(t *testing.T) {
	g := BuildFromStore(followupStore())
	srv := &Server{store: followupStore()}
	srv.neighborGraph.Store(g)
	srv.recomputeNeighborGraphCacheTick(false)
	if srv.neighborGraph.Load() != g {
		t.Fatal("tick with rebuild=false replaced the graph")
	}
	if e := srv.neighborGraphCache.unfilteredPtr.Load(); e == nil || len(e.resp.Edges) == 0 {
		t.Fatal("response not built from the published graph")
	}
}

// F3/F7: concurrent publishers and readers are race-free. The previous plain
// pointer field raced here under -race when the mutex was dropped; the
// atomic.Pointer field makes that mutant unrepresentable.
func TestNeighborGraphPublishIsRaceFree(t *testing.T) {
	srv := &Server{store: followupStore()}
	srv.neighborGraph.Store(NewNeighborGraph())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				srv.rebuildNeighborGraph()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if srv.getNeighborGraph() == nil {
					t.Error("nil graph")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// F6: Config.NeighborMaxEdgeKm semantics.
func TestConfigNeighborMaxEdgeKm(t *testing.T) {
	var nilCfg *Config
	cases := []struct {
		name string
		cfg  *Config
		want float64
	}{
		{"nil config", nilCfg, DefaultMaxEdgeKm},
		{"no neighborGraph block", &Config{}, DefaultMaxEdgeKm},
		{"zero = default", &Config{NeighborGraph: &NeighborGraphConfig{}}, DefaultMaxEdgeKm},
		{"explicit", &Config{NeighborGraph: &NeighborGraphConfig{MaxEdgeKm: 42}}, 42},
		{"negative disables", &Config{NeighborGraph: &NeighborGraphConfig{MaxEdgeKm: -1}}, 0},
	}
	for _, c := range cases {
		if got := c.cfg.NeighborMaxEdgeKm(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// F6: Server.buildNeighborGraph passes the configured maxEdgeKm into the build.
func TestBuildNeighborGraphWiresMaxEdgeKm(t *testing.T) {
	edges := func(cfg *Config) (int, uint64) {
		srv := &Server{store: followupStore(), cfg: cfg}
		g := srv.buildNeighborGraph()
		return len(g.AllEdges()), g.RejectedEdgesGeoFar
	}
	if n, rej := edges(&Config{NeighborGraph: &NeighborGraphConfig{MaxEdgeKm: 10}}); n != 0 || rej == 0 {
		t.Errorf("maxEdgeKm=10: edges=%d rejected=%d, want 0 edges and >0 rejected", n, rej)
	}
	if n, rej := edges(&Config{}); n == 0 || rej != 0 {
		t.Errorf("default: edges=%d rejected=%d, want edges kept", n, rej)
	}
	if n, rej := edges(&Config{NeighborGraph: &NeighborGraphConfig{MaxEdgeKm: -1}}); n == 0 || rej != 0 {
		t.Errorf("filter disabled: edges=%d rejected=%d, want edges kept", n, rej)
	}
	if n, _ := edges(nil); n == 0 {
		t.Error("nil cfg: edges dropped")
	}
}

// F6: Config.DebugAffinity enables the build's disambiguation log.
func TestBuildNeighborGraphWiresDebugAffinity(t *testing.T) {
	on := &Server{store: followupStore(), cfg: &Config{DebugAffinity: true}}
	if on.buildNeighborGraph().logFn == nil {
		t.Error("DebugAffinity=true did not enable build logging")
	}
	off := &Server{store: followupStore(), cfg: &Config{}}
	if off.buildNeighborGraph().logFn != nil {
		t.Error("DebugAffinity=false enabled build logging")
	}
}

// F3/F7: the publish pointer is an atomic.Pointer (house pattern, like
// PacketStore.graph and neighborGraphCache.ptr), so a mutex-less plain-pointer
// publish cannot be written.
func TestNeighborGraphFieldIsAtomicPointer(t *testing.T) {
	f, ok := reflect.TypeOf(Server{}).FieldByName("neighborGraph")
	if !ok || f.Type != reflect.TypeOf(atomic.Pointer[NeighborGraph]{}) {
		t.Fatalf("Server.neighborGraph must be atomic.Pointer[NeighborGraph], got %v", f.Type)
	}
}
