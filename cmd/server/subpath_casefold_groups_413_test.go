package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Hops are hex prefixes in practice, but the counting folds case like
// strings.ToLower for any hop, as the removed index did: a non-ASCII hop
// takes the fallback in appendLowerHop and matchLowerHop (PR #529 review N1).
// "ẞ" lower-cases to "ß", one byte shorter, so the match position must
// follow the folded length, not the hop's.
func TestSubpathNonASCIIHopsFoldCase_413(t *testing.T) {
	for _, hop := range []string{"ÄÖ", "äö", "ẞx", "aÄ", "AB", ""} {
		want := strings.ToLower(hop)
		if got := string(appendLowerHop([]byte("p,"), hop)); got != "p,"+want {
			t.Errorf("appendLowerHop(%q) = %q, want %q", hop, got, "p,"+want)
		}
		key := "p," + want + ",10"
		if end, ok := matchLowerHop(key, 2, hop); !ok || end != 2+len(want) {
			t.Errorf("matchLowerHop(%q, 2, %q) = %d, %v, want %d, true", key, hop, end, ok, 2+len(want))
		}
	}
	if _, ok := matchLowerHop("öx,10", 0, "ÄX"); ok {
		t.Error(`matchLowerHop matched "ÄX" against "öx"`)
	}

	pt := PayloadTXT_MSG
	var packets []*StoreTx
	for i, path := range []string{`["ÄÖ","10"]`, `["äö","10"]`, `["Äö","10"]`, `["ẞX","10"]`, `["ßx","10"]`} {
		packets = append(packets, &StoreTx{
			ID: i + 1, Hash: fmt.Sprintf("cf%d", i), PayloadType: &pt,
			FirstSeen: time.Now().UTC().Format(time.RFC3339), PathJSON: path,
		})
	}
	s := newSubpath413Store([]nodeInfo{{PublicKey: "ee99000000000000", Name: "E9", Role: "repeater"}}, packets)

	counts := map[string]int{}
	for _, row := range s.GetAnalyticsSubpaths("", 2, 2, 10)["subpaths"].([]map[string]interface{}) {
		counts[row["path"].(string)] = row["count"].(int)
	}
	if want := map[string]int{"äö → 10": 3, "ßx → 10": 2}; !reflect.DeepEqual(counts, want) {
		t.Errorf("subpaths = %v, want %v", counts, want)
	}
	for hops, want := range map[string]int{"ÄÖ,10": 3, "äÖ,10": 3, "ẞx,10": 2, "ßX,10": 2} {
		if got := s.GetSubpathDetail(strings.Split(hops, ",")).TotalMatches; got != want {
			t.Errorf("subpath-detail %s: totalMatches = %d, want %d", hops, got, want)
		}
	}
}

// The bulk count runs once to the longest group's maxLen, whatever order the
// caller lists the groups in (PR #529 review N2): every group must equal the
// same group asked for on its own.
func TestSubpathsBulkGroupOrderDoesNotCapLength_413(t *testing.T) {
	nodes, packets := subpath413Data(1500, 150, 7)
	s := newSubpath413Store(nodes, packets)
	for _, groups := range [][]subpathGroup{
		{{5, 8, 15}, {4, 4, 20}, {3, 3, 30}, {2, 2, 50}},
		{{2, 2, 50}, {5, 8, 15}, {3, 3, 30}},
		{{3, 3, 30}, {2, 8, 40}, {2, 2, 10}},
	} {
		clearSubpathCache413(s)
		bulk := s.GetAnalyticsSubpathsBulk("", groups)
		for i, g := range groups {
			clearSubpathCache413(s)
			want := s.GetAnalyticsSubpaths("", g.MinLen, g.MaxLen, g.Limit)
			if len(want["subpaths"].([]map[string]interface{})) == 0 {
				t.Fatalf("setup: group %v has no subpaths", g)
			}
			if !reflect.DeepEqual(bulk[i], want) {
				t.Errorf("groups %v: group %v differs from the same group on its own", groups, g)
			}
		}
	}
}
