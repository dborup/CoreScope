package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Issue #256: analytics lists are built by ranging over a map and were sorted
// on one key only. Go map iteration order is random, so entries with equal
// values came out in a different order on each recompute, and in lists capped
// at N it was random which tied entries made the cut. These tests compute each
// list many times and require the same, fully specified order every time.

const tieOrderRuns = 50

// newTieOrderStore builds an in-memory store over packets with an empty node
// cache, so hops stay unresolved and no DB is needed.
func newTieOrderStore(packets []*StoreTx) *PacketStore {
	s := newChannelTestStore(packets)
	s.nodeCache = []nodeInfo{}
	s.nodePM = buildPrefixMap(nil)
	s.nodeCacheTime = time.Now()
	return s
}

// assertSameEveryRun calls compute tieOrderRuns times and fails unless every
// result equals want.
func assertSameEveryRun(t *testing.T, name string, want interface{}, compute func() interface{}) {
	t.Helper()
	for run := 0; run < tieOrderRuns; run++ {
		if got := compute(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s, run %d:\n got  %v\n want %v", name, run, got, want)
			return
		}
	}
}

// mapField projects one field of each entry of a []map[string]interface{}.
func mapField(list interface{}, field string) []string {
	out := []string{}
	for _, e := range list.([]map[string]interface{}) {
		out = append(out, fmt.Sprint(e[field]))
	}
	return out
}

func makeHashSizeAdvert(pubkey, name, firstSeen string) *StoreTx {
	pt := PayloadADVERT
	d, _ := json.Marshal(map[string]interface{}{"pubKey": pubkey, "name": name})
	return &StoreTx{
		// Header 0x11: flood route, ADVERT. Path byte 0x41: 2-byte hashes, 1 hop.
		RawHex:      "1141aabbccdd",
		Hash:        "adv-" + pubkey + "-" + firstSeen,
		FirstSeen:   firstSeen,
		PayloadType: &pt,
		DecodedJSON: string(d),
	}
}

func TestHashSizesMultiByteNodesTieOrder_256(t *testing.T) {
	// pubkey → adverts. Six nodes tie on 3, inserted in an order that is not
	// the expected one.
	counts := []struct {
		pk string
		n  int
	}{
		{"f6000000", 3}, {"a1000000", 3}, {"d4000000", 3}, {"0b000000", 5},
		{"c3000000", 3}, {"b2000000", 3}, {"e5000000", 3}, {"99000000", 1},
	}
	var packets []*StoreTx
	for _, c := range counts {
		for i := 0; i < c.n; i++ {
			packets = append(packets, makeHashSizeAdvert(c.pk, "N-"+c.pk, fmt.Sprintf("2026-05-01T12:%02d:00Z", i)))
		}
	}
	s := newTieOrderStore(packets)

	want := []string{"0b000000", "a1000000", "b2000000", "c3000000", "d4000000", "e5000000", "f6000000", "99000000"}
	assertSameEveryRun(t, "multiByteNodes pubkeys", want, func() interface{} {
		return mapField(s.computeAnalyticsHashSizes("", "")["multiByteNodes"], "pubkey")
	})
}

func TestHashSizesTopHopsTieOrder_256(t *testing.T) {
	// 60 distinct 2-byte hops, each seen twice, plus one seen three times.
	// The list is capped at 50, so the tie also decides which hops are listed.
	var packets []*StoreTx
	var hops []string
	for i := 0; i < 60; i++ {
		hops = append(hops, fmt.Sprintf("%04x", (i*37)%256+0x1000))
	}
	add := func(hop string) {
		pt := PayloadTXT_MSG
		packets = append(packets, &StoreTx{
			RawHex: "0941aabb", Hash: fmt.Sprintf("h%d", len(packets)),
			FirstSeen: "2026-05-01T12:00:00Z", PayloadType: &pt,
			PathJSON: `["` + hop + `"]`,
		})
	}
	for _, h := range hops {
		add(h)
		add(h)
	}
	add("ffff")
	add("ffff")
	add("ffff")
	s := newTieOrderStore(packets)

	sorted := append([]string(nil), hops...)
	sort.Strings(sorted)
	want := append([]string{"ffff"}, sorted[:49]...)
	assertSameEveryRun(t, "topHops hex", want, func() interface{} {
		return mapField(s.computeAnalyticsHashSizes("", "")["topHops"], "hex")
	})
}

func TestChannelAnalyticsTieOrder_256(t *testing.T) {
	var packets []*StoreTx
	add := func(chHash int, channel, sender, firstSeen string) {
		tx := makeGrpTxWithStatus(chHash, channel, "hi", sender, "decrypted")
		tx.ID = len(packets) + 1
		tx.FirstSeen = firstSeen
		packets = append(packets, tx)
	}
	// Six channels with 2 messages each, one with 4. Each message has its own
	// sender (20 senders with 1 message, capped at 15), plus one sender with 3.
	sender := 0
	nextSender := func() string { sender++; return fmt.Sprintf("s%02d", (sender*7)%23) }
	for _, ch := range []int{60, 10, 50, 20, 40, 30} {
		for i := 0; i < 2; i++ {
			add(ch, fmt.Sprintf("#c%d", ch), nextSender(), "2026-05-01T12:00:00Z")
		}
	}
	for i := 0; i < 4; i++ {
		add(70, "#c70", "zz-top", "2026-05-01T13:00:00Z")
	}
	for i := 0; i < 8; i++ {
		add(80, "#c80", nextSender(), "2026-05-01T14:00:00Z")
	}
	s := newTieOrderStore(packets)
	s.byPayloadType[5] = packets
	compute := func() map[string]interface{} { return s.computeAnalyticsChannels("", "", TimeWindow{}) }

	// channels: messages desc, then hash asc.
	assertSameEveryRun(t, "channels hash", []string{"80", "70", "10", "20", "30", "40", "50", "60"}, func() interface{} {
		return mapField(compute()["channels"], "hash")
	})

	// topSenders: count desc, then name asc, capped at 15.
	var names []string
	for k := range senderSet(packets) {
		if k != "zz-top" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	wantSenders := append([]string{"zz-top"}, names[:14]...)
	assertSameEveryRun(t, "topSenders name", wantSenders, func() interface{} {
		return mapField(compute()["topSenders"], "name")
	})

	// channelTimeline: hour asc, then channel asc.
	wantTL := []string{
		"2026-05-01T12|#c10", "2026-05-01T12|#c20", "2026-05-01T12|#c30",
		"2026-05-01T12|#c40", "2026-05-01T12|#c50", "2026-05-01T12|#c60",
		"2026-05-01T13|#c70", "2026-05-01T14|#c80",
	}
	assertSameEveryRun(t, "channelTimeline hour|channel", wantTL, func() interface{} {
		out := []string{}
		for _, e := range compute()["channelTimeline"].([]map[string]interface{}) {
			out = append(out, e["hour"].(string)+"|"+e["channel"].(string))
		}
		return out
	})
}

func senderSet(packets []*StoreTx) map[string]bool {
	out := map[string]bool{}
	for _, tx := range packets {
		var d struct {
			Sender string `json:"sender"`
		}
		json.Unmarshal([]byte(tx.DecodedJSON), &d)
		out[d.Sender] = true
	}
	return out
}

func TestRFAnalyticsTieOrder_256(t *testing.T) {
	var packets []*StoreTx
	snr := 5.0
	// Payload types 0..6 each with 2 packets (one observation each), type 8
	// with 3.
	add := func(ptype int) {
		pt := ptype
		id := len(packets) + 1
		packets = append(packets, &StoreTx{
			ID: id, Hash: fmt.Sprintf("rf%d", id), RawHex: "0000",
			FirstSeen: "2026-05-01T12:00:00Z", PayloadType: &pt,
			Observations: []*StoreObs{{ID: id, ObserverID: "obs1", SNR: &snr, Timestamp: "2026-05-01T12:00:00Z"}},
		})
	}
	for _, p := range []int{6, 2, 4, 0, 5, 1, 3} {
		add(p)
		add(p)
	}
	add(8)
	add(8)
	add(8)
	s := newTieOrderStore(packets)
	compute := func() map[string]interface{} { return s.computeAnalyticsRF("", "", TimeWindow{}) }

	assertSameEveryRun(t, "payloadTypes type", []int{8, 0, 1, 2, 3, 4, 5, 6}, func() interface{} {
		v := reflect.ValueOf(compute()["payloadTypes"])
		out := []int{}
		for i := 0; i < v.Len(); i++ {
			out = append(out, int(v.Index(i).FieldByName("Type").Int()))
		}
		return out
	})

	// snrByType: count desc, then type name asc.
	var tied []string
	for _, p := range []int{0, 1, 2, 3, 4, 5, 6} {
		tied = append(tied, payloadTypeNames[p])
	}
	sort.Strings(tied)
	wantSNR := append([]string{payloadTypeNames[8]}, tied...)
	assertSameEveryRun(t, "snrByType name", wantSNR, func() interface{} {
		v := reflect.ValueOf(compute()["snrByType"])
		out := []string{}
		for i := 0; i < v.Len(); i++ {
			out = append(out, v.Index(i).FieldByName("Name").String())
		}
		return out
	})
}

func TestTopologyAnalyticsTieOrder_256(t *testing.T) {
	var packets []*StoreTx
	add := func(obsID, path string) {
		id := len(packets) + 1
		packets = append(packets, &StoreTx{
			ID: id, Hash: fmt.Sprintf("tp%d", id), FirstSeen: "2026-05-01T12:00:00Z",
			ObserverID: obsID, ObserverName: "Name-" + obsID, PathJSON: path,
		})
	}
	// Six observers each hear the same six 2-hop paths once, so every hop,
	// every pair, every ring entry and every cross-observer entry ties.
	observers := []string{"obs-f", "obs-a", "obs-d", "obs-c", "obs-e", "obs-b"}
	paths := []string{`["f1","e1"]`, `["a1","b1"]`, `["d1","c1"]`}
	for _, o := range observers {
		for _, p := range paths {
			add(o, p)
		}
	}
	s := newTieOrderStore(packets)
	compute := func() map[string]interface{} { return s.computeAnalyticsTopology("", "", TimeWindow{}) }

	assertSameEveryRun(t, "topRepeaters hop", []string{"a1", "b1", "c1", "d1", "e1", "f1"}, func() interface{} {
		return mapField(compute()["topRepeaters"], "hop")
	})
	assertSameEveryRun(t, "topPairs hopA|hopB", []string{"a1|b1", "c1|d1", "e1|f1"}, func() interface{} {
		out := []string{}
		for _, e := range compute()["topPairs"].([]map[string]interface{}) {
			out = append(out, e["hopA"].(string)+"|"+e["hopB"].(string))
		}
		return out
	})
	// observers had no sort at all; the first one is the default tab.
	assertSameEveryRun(t, "observers id", []string{"obs-a", "obs-b", "obs-c", "obs-d", "obs-e", "obs-f"}, func() interface{} {
		return mapField(compute()["observers"], "id")
	})
	assertSameEveryRun(t, "perObserverReach obs-a rings", []string{"1:b1,c1,e1", "2:a1,d1,f1"}, func() interface{} {
		reach := compute()["perObserverReach"].(map[string]interface{})["obs-a"].(map[string]interface{})
		out := []string{}
		for _, r := range reach["rings"].([]map[string]interface{}) {
			out = append(out, fmt.Sprintf("%d:%s", r["hops"], strings.Join(mapField(r["nodes"], "hop"), ",")))
		}
		return out
	})
	wantObs := strings.Join([]string{"obs-a", "obs-b", "obs-c", "obs-d", "obs-e", "obs-f"}, ",")
	assertSameEveryRun(t, "multiObsNodes hop:observers", []string{
		"a1:" + wantObs, "b1:" + wantObs, "c1:" + wantObs, "d1:" + wantObs, "e1:" + wantObs, "f1:" + wantObs,
	}, func() interface{} {
		out := []string{}
		for _, e := range compute()["multiObsNodes"].([]map[string]interface{}) {
			out = append(out, e["hop"].(string)+":"+strings.Join(mapField(e["observers"], "observer_id"), ","))
		}
		return out
	})
	// bestPathList: minDist asc, then hop asc; among observers tied on
	// minDist the lowest observer id is reported.
	assertSameEveryRun(t, "bestPathList hop@observer", []string{
		"b1@obs-a", "c1@obs-a", "e1@obs-a", "a1@obs-a", "d1@obs-a", "f1@obs-a",
	}, func() interface{} {
		out := []string{}
		for _, e := range compute()["bestPathList"].([]map[string]interface{}) {
			out = append(out, e["hop"].(string)+"@"+e["observer_id"].(string))
		}
		return out
	})
}

func TestDistanceTopHopsTieOrder_256(t *testing.T) {
	// Six pairs at exactly the same distance, one farther, one nearer.
	var hops []distHopRecord
	for _, p := range [][2]string{{"F", "f"}, {"A", "a"}, {"D", "d"}, {"C", "c"}, {"E", "e"}, {"B", "b"}} {
		hops = append(hops, distHopRecord{FromPk: p[0], ToPk: p[1], Dist: 42, Hash: "h" + p[0]})
	}
	hops = append(hops, distHopRecord{FromPk: "Z", ToPk: "z", Dist: 99, Hash: "hZ"})
	hops = append(hops, distHopRecord{FromPk: "Y", ToPk: "y", Dist: 1, Hash: "hY"})

	// limit 5 also checks which tied pairs make the cut.
	assertSameEveryRun(t, "distance topHops fromPk", []string{"Z", "A", "B", "C", "D"}, func() interface{} {
		return mapField(dedupeHopsByPair(hops, 5), "fromPk")
	})
}

func TestRankSubpathsTieOrder_256(t *testing.T) {
	counts := map[string]*subpathAccum{}
	for _, p := range []string{"F → G", "A → B", "D → E", "C → D", "E → F", "B → C"} {
		counts[p] = &subpathAccum{count: 4, raw: strings.ToLower(strings.ReplaceAll(p, " → ", ","))}
	}
	counts["X → Y"] = &subpathAccum{count: 9, raw: "x,y"}
	s := newTieOrderStore(nil)

	assertSameEveryRun(t, "subpaths path", []string{"X → Y", "A → B", "B → C", "C → D"}, func() interface{} {
		return mapField(s.rankSubpaths(counts, 100, 4)["subpaths"], "path")
	})
}
