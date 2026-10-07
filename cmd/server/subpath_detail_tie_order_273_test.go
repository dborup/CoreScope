package main

import (
	"fmt"
	"sort"
	"testing"
)

// Issue #273: GetSubpathDetail is a sibling of the #256-fixed rankSubpaths.
// Its parentPaths and observers lists are built by ranging over a map and were
// sorted on the count only. Go map iteration order is random, so entries with
// equal counts came out in a different order on each call, and in lists capped
// at N it was random which tied entries made the cut. These tests compute the
// detail many times and require the same, fully specified order every time.

// newSubpathDetailStore builds a tie-order store and feeds every packet through
// the same subpath index that production uses, so GetSubpathDetail resolves its
// matched transmissions exactly as it would at runtime.
func newSubpathDetailStore(packets []*StoreTx) *PacketStore {
	s := newTieOrderStore(packets)
	for _, tx := range packets {
		addTxToSubpathIndexFull(s.spIndex, s.spTxIndex, tx)
	}
	return s
}

func TestSubpathDetailParentPathsTieOrder_273(t *testing.T) {
	// Query the 2-hop subpath "a1 → b1". Each parent path extends it with a
	// distinct third hop. 18 parent paths tie on 2, one is seen 5 times. The
	// list is capped at 15, so the tie also decides which parents are listed.
	rawHops := []string{"a1", "b1"}
	var packets []*StoreTx
	id := 0
	add := func(path string) {
		id++
		pt := PayloadTXT_MSG
		packets = append(packets, &StoreTx{
			RawHex: "0941aabb", Hash: fmt.Sprintf("pp%d", id),
			FirstSeen: "2026-05-01T12:00:00Z", PayloadType: &pt,
			ObserverName: "obs", PathJSON: path,
		})
	}
	var tiedPaths []string
	for i := 0; i < 18; i++ {
		suffix := fmt.Sprintf("%04x", (i*37)%256+0x2000)
		add(fmt.Sprintf(`["a1","b1","%s"]`, suffix))
		add(fmt.Sprintf(`["a1","b1","%s"]`, suffix))
		tiedPaths = append(tiedPaths, "a1 → b1 → "+suffix)
	}
	const hiSuffix = "ffff"
	for i := 0; i < 5; i++ {
		add(fmt.Sprintf(`["a1","b1","%s"]`, hiSuffix))
	}
	s := newSubpathDetailStore(packets)

	sort.Strings(tiedPaths)
	want := append([]string{"a1 → b1 → " + hiSuffix}, tiedPaths[:14]...)
	assertSameEveryRun(t, "parentPaths path", want, func() interface{} {
		return mapField(s.GetSubpathDetail(rawHops)["parentPaths"], "path")
	})
}

func TestSubpathDetailObserversTieOrder_273(t *testing.T) {
	// Every transmission carries the same 2-hop path "c1 → d1" but a distinct
	// observer. 14 observers tie on 2, one is seen 5 times. The observers list
	// is capped at 10, so the tie also decides which observers are listed.
	rawHops := []string{"c1", "d1"}
	var packets []*StoreTx
	id := 0
	add := func(observer string) {
		id++
		pt := PayloadTXT_MSG
		packets = append(packets, &StoreTx{
			RawHex: "0941aabb", Hash: fmt.Sprintf("ob%d", id),
			FirstSeen: "2026-05-01T12:00:00Z", PayloadType: &pt,
			ObserverName: observer, PathJSON: `["c1","d1"]`,
		})
	}
	var tiedObs []string
	for i := 0; i < 14; i++ {
		name := fmt.Sprintf("obs-%04x", (i*37)%256+0x3000)
		add(name)
		add(name)
		tiedObs = append(tiedObs, name)
	}
	const hiObs = "obs-zzzz"
	for i := 0; i < 5; i++ {
		add(hiObs)
	}
	s := newSubpathDetailStore(packets)

	sort.Strings(tiedObs)
	want := append([]string{hiObs}, tiedObs[:9]...)
	assertSameEveryRun(t, "observers name", want, func() interface{} {
		return mapField(s.GetSubpathDetail(rawHops)["observers"], "name")
	})
}
