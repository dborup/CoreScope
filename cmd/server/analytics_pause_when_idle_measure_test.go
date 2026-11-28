package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sort"
	"testing"
	"time"
)

// measurePauseWhenIdle backs TestPauseWhenIdle_Measure (#492): on a copy of
// test-fixtures/e2e-fixture.db it runs every analytics recomputer at a
// short interval for a fixed window, once per (pauseWhenIdle, traffic)
// pair, and prints compute passes, allocations, process CPU and read
// latency. "visible" reads every default-shape snapshot once per interval
// (an open analytics page polling); "sparse" reads them once every ten
// intervals (each read is a first read after idle); "idle" reads nothing.
func measurePauseWhenIdle(t *testing.T) {
	interval := 100 * time.Millisecond
	window := 5 * time.Second
	if v := os.Getenv("PAUSE_IDLE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			window = d
		}
	}
	src := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	fmt.Printf("| pauseWhenIdle | traffic | compute passes | alloc MB | mallocs (k) | CPU s | read p50 | read p99 | read max |\n")
	fmt.Printf("|---|---|---|---|---|---|---|---|---|\n")
	for _, pause := range []bool{false, true} {
		for _, mode := range []struct {
			name      string
			readEvery int
		}{{"idle", 0}, {"sparse", 10}, {"visible", 1}} {
			dbPath := filepath.Join(t.TempDir(), "fixture.db")
			copyFileForMeasure(t, src, dbPath)
			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			s := NewPacketStore(db, nil)
			s.graph.Store(loadNeighborEdgesFromDB(db.conn)) // main.go order: graph before packets
			if err := s.Load(); err != nil {
				t.Fatal(err)
			}
			s.WaitIndexesReady(30 * time.Second)
			s.signalStartupLoadDone()
			stop := s.StartAnalyticsRecomputers(interval, AnalyticsRecomputeIntervals{PauseWhenIdle: pause})
			rcs := storeRecomputerFields(t, s)
			for _, rc := range rcs {
				rc.RecomputeNow() // settle past the post-load pass before measuring
			}
			runtime.GC()
			runs0 := totalRuns(rcs)
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			cpu0 := processCPUSeconds()

			var lat []time.Duration
			deadline := time.Now().Add(window)
			tk := time.NewTicker(interval)
			for n := 1; time.Now().Before(deadline); n++ {
				<-tk.C
				if mode.readEvery > 0 && n%mode.readEvery == 0 {
					t0 := time.Now()
					readAllDefaultSnapshots(s)
					lat = append(lat, time.Since(t0))
				}
			}
			tk.Stop()

			cpu1 := processCPUSeconds()
			runtime.ReadMemStats(&m1)
			runs := totalRuns(rcs) - runs0
			stop()
			db.Close()

			p50, p99, pmax := "-", "-", "-"
			if len(lat) > 0 {
				sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
				p50 = lat[len(lat)/2].String()
				p99 = lat[len(lat)*99/100].String()
				pmax = lat[len(lat)-1].String()
			}
			fmt.Printf("| %v | %s | %d | %.1f | %.1f | %.2f | %s | %s | %s |\n",
				pause, mode.name, runs,
				float64(m1.TotalAlloc-m0.TotalAlloc)/1e6,
				float64(m1.Mallocs-m0.Mallocs)/1e3,
				cpu1-cpu0, p50, p99, pmax)
		}
	}
	fmt.Printf("\ninterval=%s window=%s recomputers=9 GOMAXPROCS=%d\n", interval, window, runtime.GOMAXPROCS(0))
}

func totalRuns(rcs map[string]*analyticsRecomputer) int64 {
	var n int64
	for _, rc := range rcs {
		n += rc.ComputeRuns()
	}
	return n
}

// readAllDefaultSnapshots is what an open analytics page reads per refresh.
func readAllDefaultSnapshots(s *PacketStore) {
	s.GetAnalyticsRF("", "")
	s.GetAnalyticsTopology("", "")
	s.GetAnalyticsChannels("", "")
	s.GetAnalyticsDistance("", "")
	s.GetAnalyticsHashSizes("", "")
	s.GetAnalyticsHashCollisions("", "")
	s.GetAnalyticsRoles()
	s.GetObserverCalibrations()
	s.GetFleetClockSkew("")
}

// processCPUSeconds is the runtime's estimate of total CPU time used by the
// process (user + GC + idle-excluded), portable across CI platforms.
func processCPUSeconds() float64 {
	samples := []metrics.Sample{{Name: "/cpu/classes/total:cpu-seconds"}, {Name: "/cpu/classes/idle:cpu-seconds"}}
	metrics.Read(samples)
	return samples[0].Value.Float64() - samples[1].Value.Float64()
}

func copyFileForMeasure(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
