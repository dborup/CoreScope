package main

import (
	"fmt"
	"testing"
)

// Pool micro-benchmarks for #489: the steady state is a hit, which must take
// only the read lock and allocate nothing. The parallel variant is the
// contention case (background fill, live ingest and a chunk merge can all
// intern at once).
//
//	go test -run '^$' -bench ObsStringPool -benchtime 2s .

func benchPoolKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%064x", i)
	}
	return keys
}

func BenchmarkObsStringPoolHit(b *testing.B) {
	p := newObsStringPool()
	keys := benchPoolKeys(140)
	for _, k := range keys {
		p.intern(k)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.intern(keys[i%len(keys)])
	}
}

func BenchmarkObsStringPoolHitParallel(b *testing.B) {
	p := newObsStringPool()
	keys := benchPoolKeys(140)
	for _, k := range keys {
		p.intern(k)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			p.intern(keys[i%len(keys)])
			i++
		}
	})
}

// BenchmarkObsStringInternObs is one observation's worth of interning: the
// four fields a row scan hands over.
func BenchmarkObsStringInternObs(b *testing.B) {
	p := newObsStringPool()
	id, name, iata, dir := obs489ObserverID(7), obs489ObserverName(7), obs489IATA(7), "RX"
	p.internObs(id, name, iata, dir)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.internObs(id, name, iata, dir)
	}
}

// BenchmarkObsStringPoolMiss is the cold-load case: a value the pool does not
// hold yet, which takes the write lock and clones.
func BenchmarkObsStringPoolMiss(b *testing.B) {
	keys := benchPoolKeys(obsStringPoolMax)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := newObsStringPool()
		b.StartTimer()
		for _, k := range keys {
			p.intern(k)
		}
	}
	b.ReportMetric(float64(len(keys)), "values/op")
}
