package main

import (
	"context"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestReachServe941_Manual serves the whole app with the predicted reach
// on, for browser validation and screenshots (#941 M2): the production
// router, the real coverage endpoint and nodes in reach, over a synthetic
// terrain from a fake ElevationSource (no DHM token, no network). The
// database is the e2e fixture prepared as the validation harness does
// (migrated, its network moved to Jutland). It skips unless
// CORESCOPE_941_SERVE_ADDR is set, and serves for
// CORESCOPE_941_SERVE_SECONDS (default 1800):
//
//	cd cmd/server
//	CORESCOPE_941_SERVE_ADDR=127.0.0.1:13592 go test -run 'TestReachServe941_Manual$' -timeout 1h -v .
func TestReachServe941_Manual(t *testing.T) {
	addr := os.Getenv("CORESCOPE_941_SERVE_ADDR")
	if addr == "" {
		t.Skip("CORESCOPE_941_SERVE_ADDR not set: manual browser validation only")
	}
	secs := 1800
	if v, err := strconv.Atoi(os.Getenv("CORESCOPE_941_SERVE_SECONDS")); err == nil && v > 0 {
		secs = v
	}
	path := prepareReachFixture(t, reachFixtureScale{radiusM: 40000, keepM: math.Inf(1), seeds: 12})
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewPacketStore(db, nil)
	store.graph.Store(loadNeighborEdgesFromDB(db.conn)) // main.go order: graph before packets
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	hub := NewHub()
	srv := NewServer(db, cfg, hub)
	srv.store = store
	srv.coverage = newTerrainCoverage(CoverageConfig{Enabled: true, RateLimitPerMin: intp(0)}, nil,
		terrainCoverageDeps{src: newFakeElevation(t, rollingElev), logf: t.Logf, observers: func() ([]Observer, error) { return nil, nil }})
	srv.coverage.covers = func(e, n float64) bool { return true }
	public, _ := filepath.Abs(filepath.Join("..", "..", "public"))
	hs := &http.Server{Addr: addr, Handler: newHTTPRouter(srv, hub, public)}
	go func() {
		time.Sleep(time.Duration(secs) * time.Second)
		hs.Shutdown(context.Background())
	}()
	t.Logf("serving %s on http://%s for %d s (synthetic terrain, fake ElevationSource)", public, addr, secs)
	if err := hs.ListenAndServe(); err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
