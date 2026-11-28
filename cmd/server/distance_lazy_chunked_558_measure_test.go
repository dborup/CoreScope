package main

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDistanceLazyChunked558_Measure loads the large-instance shape of #534
// (about 150K transmissions, 2K nodes, 2.5M observations; nodes spread over
// about 1° so hops have a distance) with the production LoadChunked and
// prints the startup cost and what the first distance request pays:
//
//   - LoadChunked wall time,
//   - the longest and the last s.mu wait a reader saw during it (the last one
//     is the post-load section, where the eager build ran),
//   - distance-index builds during startup and until the first request is
//     ready (counted from the build's log line),
//   - the cold lazy build the first request triggers,
//   - one isolated buildDistanceIndex under s.mu on the loaded store.
//
// The file compiles against the commit before the fix too, which gives the
// "before" numbers on the same database:
//
//	CORESCOPE_558_MEASURE=1 CORESCOPE_558_DB=/path/m558.db go test -run DistanceLazyChunked558_Measure -v -timeout 60m
//	CORESCOPE_558_MEASURE=30000,600,500000 ... (tx,nodes,obs)
func TestDistanceLazyChunked558_Measure(t *testing.T) {
	if os.Getenv("CORESCOPE_558_MEASURE") == "" {
		t.Skip("set CORESCOPE_558_MEASURE=1 to run")
	}
	numTx, numNodes, numObs := bn534Scale(t, "CORESCOPE_558_MEASURE")
	dbPath := os.Getenv("CORESCOPE_558_DB")
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "m558.db")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t0 := time.Now()
		bn534WriteDB(t, dbPath, bn534Data(numTx, numNodes, numObs, 534), numNodes)
		conn, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(`UPDATE nodes SET lat = 55.0 + (rowid % 50) * 0.02, lon = 12.0 + (rowid / 50) * 0.025`); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		t.Logf("wrote %s in %s", dbPath, time.Since(t0).Round(time.Millisecond))
	}
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.conn.Close()

	// Count "Built distance index" log lines; keep the rest of the log.
	var builds atomic.Int32
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("Built distance index")) {
			builds.Add(1)
		}
		return prev.Write(p)
	}))
	defer log.SetOutput(prev)

	store := NewPacketStore(db, &PacketStoreConfig{})

	// A reader that takes s.mu.RLock in a loop sees how long the loader
	// holds the write lock at a stretch.
	var maxWait, lastLongWait time.Duration
	stopProbe := make(chan struct{})
	var probe sync.WaitGroup
	probe.Add(1)
	go func() {
		defer probe.Done()
		for {
			select {
			case <-stopProbe:
				return
			default:
			}
			t0 := time.Now()
			store.mu.RLock()
			store.mu.RUnlock()
			d := time.Since(t0)
			if d > maxWait {
				maxWait = d
			}
			if d > 20*time.Millisecond {
				lastLongWait = d
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	t0 := time.Now()
	if err := store.LoadChunked(0); err != nil {
		t.Fatal(err)
	}
	loadDur := time.Since(t0)
	close(stopProbe)
	probe.Wait()
	startupBuilds := builds.Load()
	hopsAtStartup, _ := distIndexLen(store)

	t1 := time.Now()
	store.TriggerDistanceIndexBuild()
	for !store.DistanceIndexBuilt() {
		time.Sleep(5 * time.Millisecond)
	}
	firstReady := time.Since(t1)
	buildsUntilReady := builds.Load()
	hops, paths := distIndexLen(store)

	nodes := store.snapshotPathNodes()
	t2 := time.Now()
	store.mu.Lock()
	store.buildDistanceIndex(nodes)
	store.mu.Unlock()
	isolated := time.Since(t2)

	store.mu.RLock()
	nTx, nObs := len(store.packets), store.totalObs
	store.mu.RUnlock()
	t.Logf("data: %d tx, %d obs, %d nodes; distance index %d hops, %d paths; GOMAXPROCS=%d %s/%s %s",
		nTx, nObs, numNodes, hops, paths, runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH, runtime.Version())
	t.Logf("LoadChunked: %s; longest s.mu wait %s, last long (post-load) wait %s",
		loadDur.Round(time.Millisecond), maxWait.Round(time.Millisecond), lastLongWait.Round(time.Millisecond))
	t.Logf("distance builds: %d during startup (index held %d hops after load), %d until the first request was ready",
		startupBuilds, hopsAtStartup, buildsUntilReady)
	t.Logf("first request: cold lazy build ready after %s; isolated buildDistanceIndex under s.mu: %s",
		firstReady.Round(time.Millisecond), isolated.Round(time.Millisecond))
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
