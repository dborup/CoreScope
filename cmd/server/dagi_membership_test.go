package main

// #740 M1 — the per-node DAGI membership cache.

import (
	"math"
	"sync"
	"testing"
)

func dagiTestNode(pk string, lat, lon float64) nodeInfo {
	return nodeInfo{PublicKey: pk, Lat: lat, Lon: lon, HasGPS: !(lat == 0 && lon == 0)}
}

func TestDagiMembershipRefreshOnePass(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	st := m.Refresh([]nodeInfo{
		dagiTestNode("aarhus", 56.1567, 10.2108),
		dagiTestNode("kbh", 55.6804, 12.5857),
		dagiTestNode("sea", 56.0, 7.0),
		dagiTestNode("flensburg", 54.7833, 9.4333),
	})
	if st != (dagiRefreshStats{Nodes: 4, Located: 4}) {
		t.Errorf("stats = %+v", st)
	}
	p, ok := m.Place("aarhus")
	if !ok || dagiCodesOf(p) != (dagiCodes{"0751", dagiLandsdelOestjylland, "1082", "8000"}) {
		t.Errorf("Place(aarhus) = %+v, %v", dagiCodesOf(p), ok)
	}
	if !m.Contains(DagiKommune, "0751", "aarhus") || m.Contains(DagiKommune, "0751", "kbh") ||
		!m.Contains(DagiRegion, "1084", "kbh") || !m.Contains(DagiPostnummer, "1050", "kbh") ||
		!m.Contains(DagiLandsdel, dagiLandsdelByenKbh, "kbh") {
		t.Error("Contains disagrees with Place")
	}
	// At sea: a postnummer, no kommune. Abroad: in the cache, in no set.
	if p, ok := m.Place("sea"); !ok || dagiCodesOf(p) != (dagiCodes{"", "", "", "6960"}) {
		t.Errorf("Place(sea) = %+v, %v", dagiCodesOf(p), ok)
	}
	if p, ok := m.Place("flensburg"); !ok || dagiCodesOf(p) != (dagiCodes{}) {
		t.Errorf("Place(flensburg) = %+v, %v", dagiCodesOf(p), ok)
	}
	if got := len(m.Members(DagiRegion, "1082")); got != 1 {
		t.Errorf("Members(region 1082) = %d nodes, want 1", got)
	}
	if m.Members(DagiKommune, "9999") != nil || m.Members(DagiLayer(-1), "0751") != nil || m.Members(dagiLayerCount, "0751") != nil {
		t.Error("an unknown code or layer has members")
	}
}

// Nodes without a position get no membership and are counted: no GPS, the
// 0/0 sentinel, NaN, ±Inf, out of range.
func TestDagiMembershipUnpositioned(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	nodes := []nodeInfo{
		{PublicKey: "nogps", Lat: 56.1567, Lon: 10.2108, HasGPS: false},
		dagiTestNode("zero", 0, 0),
		{PublicKey: "zero-flagged", Lat: 0, Lon: 0, HasGPS: true},
		{PublicKey: "nan", Lat: math.NaN(), Lon: 10.2, HasGPS: true},
		{PublicKey: "inf", Lat: 56.1, Lon: math.Inf(1), HasGPS: true},
		{PublicKey: "range", Lat: 156.1, Lon: 10.2, HasGPS: true},
		dagiTestNode("ok", 56.1567, 10.2108),
	}
	st := m.Refresh(nodes)
	if st != (dagiRefreshStats{Nodes: 7, Located: 1, Unpositioned: 6}) || m.Unpositioned() != 6 {
		t.Errorf("stats = %+v, Unpositioned() = %d", st, m.Unpositioned())
	}
	for _, n := range nodes[:6] {
		if _, ok := m.Place(n.PublicKey); ok {
			t.Errorf("%s has a place", n.PublicKey)
		}
	}
	if got := m.Members(DagiKommune, "0751"); len(got) != 1 {
		t.Errorf("Members(0751) = %v, want only ok", got)
	}
}

// A node whose position did not change keeps its membership without a
// lookup; a moved node is located again and changes sets; a node gone from
// the list is gone from the cache.
func TestDagiMembershipRefreshOnlyOnMove(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	m.Refresh([]nodeInfo{
		dagiTestNode("a", 56.1567, 10.2108), // Aarhus
		dagiTestNode("b", 55.0, 9.0),
		dagiTestNode("gone", 55.6804, 12.5857),
	})
	st := m.Refresh([]nodeInfo{
		dagiTestNode("a", 56.1567, 10.2108),   // unchanged
		dagiTestNode("b", 57.0488, 9.9217),    // moved to Aalborg
		dagiTestNode("new", 55.3959, 10.3883), // Odense
	})
	if st != (dagiRefreshStats{Nodes: 3, Located: 2, Reused: 1}) {
		t.Errorf("stats = %+v, want 1 reused, 2 located", st)
	}
	if !m.Contains(DagiKommune, "0851", "b") || m.Contains(DagiRegion, "1083", "b") {
		t.Error("the moved node kept its old membership")
	}
	if _, ok := m.Place("gone"); ok || m.Contains(DagiKommune, "0101", "gone") {
		t.Error("a node gone from the list is still cached")
	}
	if !m.Contains(DagiKommune, "0751", "a") || !m.Contains(DagiKommune, "0461", "new") {
		t.Error("membership lost")
	}
	// A node that loses its position loses its membership.
	m.Refresh([]nodeInfo{dagiTestNode("a", 0, 0)})
	if _, ok := m.Place("a"); ok || m.Contains(DagiKommune, "0751", "a") {
		t.Error("a node without a position kept its membership")
	}
}

// Reuse is keyed on the exact position: the cached place is the one the
// lookup gives for that position, never one of a neighbouring position.
func TestDagiMembershipReuseNeedsSamePosition(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	m.Refresh([]nodeInfo{dagiTestNode("a", 56.1567, 10.2108)})
	next := math.Nextafter(10.2108, 11)
	if st := m.Refresh([]nodeInfo{dagiTestNode("a", 56.1567, next)}); st.Located != 1 || st.Reused != 0 {
		t.Errorf("a one-ulp move was reused: %+v", st)
	}
	if !samePosition(dagiNodePlace{lat: math.NaN(), lon: 1}, math.NaN(), 1) || samePosition(dagiNodePlace{lat: 1, lon: 2}, 1, 3) {
		t.Error("samePosition")
	}
}

// Readers never block on, or see half of, a refresh (run with -race).
func TestDagiMembershipConcurrentReaders(t *testing.T) {
	m := newDagiMembership(dagiTestIndex(t))
	nodes := dagiSyntheticNodes(500, 7)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, n := range nodes[:50] {
					if p, ok := m.Place(n.PublicKey); ok && p.Kommune != nil && !m.Contains(DagiKommune, p.Kommune.Code, n.PublicKey) {
						// Place and Contains read two snapshots; both
						// must hold the node, as positions never change here.
						t.Errorf("%s: in %s by Place, not by Contains", n.PublicKey, p.Kommune.Code)
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		m.Refresh(nodes)
	}
	close(stop)
	wg.Wait()
}
