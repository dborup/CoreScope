package anomaly

import (
	"fmt"
	"math/rand"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// Benchmarks use synthetic events only. Thresholds are high so that the
// measurement is dominated by state updates, not by candidate emission
// (except in the burst benchmark).

func benchLimits(maxKeys int) Limits {
	return Limits{MaxKeysPerScope: maxKeys, StateTTL: 48 * time.Hour, DedupHorizon: 2 * time.Hour, MaxDedupIDs: 4_000_000,
		ReorderDelay: 0, MaxReorderBuffer: 1024, MaxWindowEntries: 4096, MaxCandidatesPerCall: 1 << 20, MaxPendingCandidates: 1 << 22}
}

func benchConfig(maxKeys int, periodic, allScopes bool) Config {
	st := StateParams{EndAfterQuiet: 10 * time.Minute, Cooldown: 30 * time.Minute}
	cfg := Config{Limits: benchLimits(maxKeys), Expected: ExpectedPolicy{Mode: ExpectedInclude}}
	for _, w := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
		cfg.Rate = append(cfg.Rate, RateRule{Name: "s" + strconv.Itoa(int(w/time.Minute)), Scope: ScopeStream, Window: w, Threshold: 1 << 20, State: st})
		if allScopes {
			for _, s := range []Scope{ScopeChannel, ScopeRouteGroup, ScopeGlobal} {
				cfg.Rate = append(cfg.Rate, RateRule{Name: fmt.Sprintf("%s%d", s, int(w/time.Minute)), Scope: s, Window: w, Threshold: 1 << 20, State: st})
			}
		}
	}
	if periodic {
		cfg.Periodic = []PeriodicRule{{Name: "p", Scope: ScopeStream, MinPeriod: time.Minute, MaxPeriod: time.Hour, BurstGap: 5 * time.Second,
			JitterAbs: 5 * time.Second, JitterRel: 0.02, MaxMissing: 2, MinPulses: 8, HistoryLen: 32, MinCoverage: 0.6,
			MaxJitterFraction: 0.5, MaxChance: 0.01, State: st}}
	}
	return cfg
}

func streamEvent(i, stream int, when time.Time) Event {
	hop := [3]byte{byte(stream), byte(stream >> 8), byte(stream >> 16)}
	w := 1 + stream%3
	h, _ := NewHop(hop[:w])
	c, _ := NewChannelHash([]byte{byte(stream % 251)})
	return Event{ID: TxID("e" + strconv.Itoa(i)), Time: when, PayloadType: 5, Channel: c,
		RouteClass: RouteClassFlood, RouteKind: RouteKindRelay, FirstHop: h, WireBytes: 60}
}

func BenchmarkObserve(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		for _, per := range []bool{false, true} {
			b.Run(fmt.Sprintf("streams=%d/periodic=%v", n, per), func(b *testing.B) {
				d, err := New(benchConfig(2*n, per, false))
				if err != nil {
					b.Fatal(err)
				}
				// warm up: every stream exists with a few events
				clock := t0
				id := 0
				for r := 0; r < 3; r++ {
					for s := 0; s < n; s++ {
						clock = clock.Add(time.Millisecond)
						d.Observe(streamEvent(id, s, clock))
						id++
					}
				}
				evs := make([]Event, b.N)
				for i := range evs {
					clock = clock.Add(10 * time.Millisecond)
					evs[i] = streamEvent(id+i, i%n, clock)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := range evs {
					d.Observe(evs[i])
				}
			})
		}
	}
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// BenchmarkBytesPerStream reports steady-state heap per stream with full
// 1/5/15/60-minute windows (4 entries each) and optionally periodicity.
func BenchmarkBytesPerStream(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		for _, per := range []bool{false, true} {
			b.Run(fmt.Sprintf("streams=%d/periodic=%v", n, per), func(b *testing.B) {
				for it := 0; it < b.N; it++ {
					b.StopTimer()
					before := heapInUse()
					d, _ := New(benchConfig(2*n, per, false))
					clock := t0
					id := 0
					for r := 0; r < 4; r++ {
						for s := 0; s < n; s++ {
							clock = clock.Add(time.Millisecond)
							d.Observe(streamEvent(id, s, clock))
							id++
						}
					}
					// the dedup set is bounded separately (MaxDedupIDs); drop it so
					// only per-stream state is measured
					d.dedup, d.dedupLog, d.dedupHead = map[TxID]int64{}, nil, 0
					after := heapInUse()
					b.ReportMetric(float64(after-before)/float64(n), "B/stream")
					runtime.KeepAlive(d)
					b.StartTimer()
				}
			})
		}
	}
}

// zipf picks streams with a skewed distribution.
func zipfStreams(n, count int, seed int64) []int {
	r := rand.New(rand.NewSource(seed))
	z := rand.NewZipf(r, 1.1, 1, uint64(n-1))
	out := make([]int, count)
	for i := range out {
		out[i] = int(z.Uint64())
	}
	return out
}

// BenchmarkMillionEvents runs 1M events over 10k streams, multi-scale windows
// at all four scopes, with and without periodicity. It reports ns/event,
// allocs/event, the peak heap during the run and the largest burst.
func BenchmarkMillionEvents(b *testing.B) {
	const events, streams = 1_000_000, 10_000
	pick := zipfStreams(streams, events, 1)
	for _, per := range []bool{false, true} {
		b.Run(fmt.Sprintf("periodic=%v", per), func(b *testing.B) {
			var peak uint64
			for it := 0; it < b.N; it++ {
				b.StopTimer()
				d, _ := New(benchConfig(4*streams, per, true))
				evs := make([]Event, events)
				clock := t0
				for i := range evs {
					clock = clock.Add(50 * time.Millisecond)
					evs[i] = streamEvent(i, pick[i], clock)
				}
				runtime.GC()
				var base runtime.MemStats
				runtime.ReadMemStats(&base)
				b.StartTimer()
				start := time.Now()
				for i := range evs {
					d.Observe(evs[i])
					if i%100_000 == 0 {
						b.StopTimer()
						var m runtime.MemStats
						runtime.ReadMemStats(&m)
						// detector growth only: the input slice is in the baseline
						if m.HeapInuse-base.HeapInuse > peak {
							peak = m.HeapInuse - base.HeapInuse
						}
						b.StartTimer()
					}
				}
				el := time.Since(start)
				b.ReportMetric(float64(el.Nanoseconds())/float64(events), "ns/event")
				s := d.Snapshot()
				b.ReportMetric(float64(s.MaxCandidateBurst), "max-burst")
			}
			b.ReportMetric(float64(peak)/(1<<20), "detector-peak-heap-MiB")
		})
	}
}

// BenchmarkAllocsPerEvent isolates allocations in steady state.
func BenchmarkAllocsPerEvent(b *testing.B) {
	d, _ := New(benchConfig(40_000, true, true))
	pick := zipfStreams(10_000, 200_000, 2)
	clock := t0
	for i, s := range pick {
		clock = clock.Add(50 * time.Millisecond)
		d.Observe(streamEvent(i, s, clock))
	}
	evs := make([]Event, b.N)
	for i := range evs {
		clock = clock.Add(50 * time.Millisecond)
		evs[i] = streamEvent(200_000+i, pick[i%len(pick)], clock)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range evs {
		d.Observe(evs[i])
	}
}

// BenchmarkExpiration: every event creates a new stream; old ones expire by
// TTL, so eviction runs continuously.
func BenchmarkExpiration(b *testing.B) {
	cfg := benchConfig(1_000_000, false, false)
	cfg.Limits.StateTTL = time.Hour
	cfg.Rate = cfg.Rate[:1]
	cfg.Rate[0].State.Cooldown = 0
	d, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	evs := make([]Event, b.N)
	clock := t0
	for i := range evs {
		clock = clock.Add(time.Second)
		evs[i] = streamEvent(i, i%16_000_000, clock)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range evs {
		d.Observe(evs[i])
	}
	b.ReportMetric(float64(d.Snapshot().TTLEvictions[ScopeStream])/float64(b.N), "ttl-evictions/event")
}

// BenchmarkCapacityEviction: a flood of unique keys against a 10k cap.
func BenchmarkCapacityEviction(b *testing.B) {
	d, _ := New(benchConfig(10_000, false, false))
	evs := make([]Event, b.N)
	clock := t0
	for i := range evs {
		clock = clock.Add(time.Millisecond)
		evs[i] = streamEvent(i, 10_000+i%16_000_000, clock)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range evs {
		d.Observe(evs[i])
	}
	b.StopTimer()
	if d.Snapshot().Keys[ScopeStream] > 10_000 {
		b.Fatal("cap exceeded")
	}
}

// BenchmarkDuplicateFanout: 20 observations per transmission through the
// Normalizer, each final event offered twice to the Detector.
func BenchmarkDuplicateFanout(b *testing.B) {
	n, _ := NewNormalizer(NormalizerConfig{SettleWindow: 30 * time.Second, MaxPendingTransmissions: 100_000,
		MaxObservationsPerTransmission: 64, FinalizedHorizon: time.Hour, MaxFinalizedIDs: 1_000_000})
	d, _ := New(benchConfig(100_000, true, true))
	route, _ := NewRoute(1, []byte{1, 2, 3, 4})
	ch, _ := NewChannelHash([]byte{0x42})
	obs := make([]Observation, 0, b.N)
	clock := t0
	for i := 0; len(obs) < b.N; i++ {
		clock = clock.Add(2 * time.Second)
		for j := 0; j < 20 && len(obs) < b.N; j++ {
			obs = append(obs, Observation{TxID: TxID("t" + strconv.Itoa(i)), ObsID: strconv.Itoa(j), Time: clock.Add(time.Duration(j) * 100 * time.Millisecond),
				PayloadType: 5, Channel: ch, RouteClass: RouteClassFlood, Path: route, WireBytes: 60})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range obs {
		for _, e := range n.Add(obs[i]).Events {
			d.Observe(e)
			d.Observe(e)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(d.Snapshot().Duplicates), "duplicates")
}

// BenchmarkWorstCandidateBurst: 10k streams become quiet at the same instant;
// one Advance emits 10k 'ended' transitions.
func BenchmarkWorstCandidateBurst(b *testing.B) {
	const n = 10_000
	for it := 0; it < b.N; it++ {
		b.StopTimer()
		cfg := benchConfig(2*n, false, false)
		cfg.Rate = []RateRule{{Name: "hot", Scope: ScopeStream, Window: time.Minute, Threshold: 1,
			State: StateParams{EndAfterQuiet: 10 * time.Minute}}}
		d, _ := New(cfg)
		for s := 0; s < n; s++ {
			d.Observe(streamEvent(s, s, t0))
		}
		for r := d.Drain(); r.More; r = d.Drain() {
		}
		b.StartTimer()
		start := time.Now()
		r := d.Advance(t0.Add(time.Hour))
		el := time.Since(start)
		b.StopTimer()
		b.ReportMetric(float64(len(r.Candidates)), "burst-candidates")
		b.ReportMetric(float64(el.Microseconds()), "burst-us")
		b.StartTimer()
	}
}
