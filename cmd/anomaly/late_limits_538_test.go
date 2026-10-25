package main

import (
	"database/sql"
	"testing"
	"time"
)

// Issue #538: the stated limits of the late-delivery flag, pinned so that a
// change to them is a visible decision. Each network below is built with the
// sweep's fixture and classes (late: the event's first observation is a
// delivered queue row; live: everything else).

// limitNet describes a network for the limits: observers 0 and 1 are live
// and hear an ADVERT every period seconds (1 at +1 s); each drainer
// (observers 2, 3, ...) hears it at +2 s, except during the outage, when it
// queues it. queue GRP_TXT heard only by the drainers are spread evenly
// over the outage. On reconnect each drainer delivers its queue in the
// order it heard it (FIFO), rate rows a second, stamped at delivery;
// drainer j starts j seconds after the first.
type limitNet struct {
	period, outage, queue, drainers, rate int // outage in minutes
}

func (n limitNet) build() swCase {
	const s0 = 30 * 60
	rc := int64(s0 + n.outage*60)
	c := swCase{outage: n.outage, queue: n.queue}
	type heard struct {
		tx, at int64 // index into c.txs and time heard
	}
	var queued []heard
	for at := int64(-60 * 60); at < 140*60; at += int64(n.period) {
		obs := []swObs{{observer: 0, at: at}, {observer: 1, at: at + 1}}
		if at >= s0 && at < rc {
			queued = append(queued, heard{int64(len(c.txs)), at})
		} else {
			for j := 0; j < n.drainers; j++ {
				obs = append(obs, swObs{observer: 2 + j, at: at + 2})
			}
		}
		c.txs = append(c.txs, swTx{pt: 4, obs: obs})
	}
	for i := 0; i < n.queue; i++ {
		at := int64(s0) + (2*int64(i)+1)*int64(n.outage)*30/int64(n.queue)
		queued = append(queued, heard{int64(len(c.txs)), at})
		c.txs = append(c.txs, swTx{pt: 5, late: true})
	}
	// FIFO: in the order heard; ties keep the background first
	for i := 1; i < len(queued); i++ {
		for k := i; k > 0 && queued[k].at < queued[k-1].at; k-- {
			queued[k], queued[k-1] = queued[k-1], queued[k]
		}
	}
	for q, h := range queued {
		for j := 0; j < n.drainers; j++ {
			x := &c.txs[h.tx]
			x.obs = append(x.obs, swObs{observer: 2 + j, at: rc + int64(j) + int64(q/n.rate), late: true})
		}
	}
	return c
}

// limitFlags exports the network with the PR's flag values and counts the
// flagged events of each class.
func limitFlags(t *testing.T, n limitNet) swCount {
	t.Helper()
	f := buildSweepFixture(t, n.build())
	opt := ltOptions(f.path)
	opt.Settle, opt.MaxRows = 30*time.Second, 100000
	got, _ := runExportTo(t, opt)
	return countSweep(t, got)
}

// A common-cause outage is never flagged: when two or more observers lose
// the same uplink and drain the same outage together, each queued row has a
// peer (the other draining observer) seconds away, so the latest other
// observation before t + gap is that peer, no row is already seen and
// nothing is flagged. One observer draining the same outage is flagged.
func TestLateDeliveryMissesACommonCauseOutage(t *testing.T) {
	for _, n := range []limitNet{
		{period: 60, outage: 40, queue: 25, rate: 1},
		{period: 10, outage: 20, queue: 40, rate: 1},
		{period: 10, outage: 40, queue: 40, rate: 1},
	} {
		for d := 1; d <= 3; d++ {
			n.drainers = d
			c := limitFlags(t, n)
			t.Logf("background %2ds  outage %2dm  queue %d  drainers %d: late %d/%d, live %d/%d",
				n.period, n.outage, n.queue, d, c.lateFlag, c.late, c.liveFlag, c.live)
			want := 0
			if d == 1 {
				want = n.queue
			}
			if c.late != n.queue || c.lateFlag != want || c.liveFlag != 0 {
				t.Errorf("%+v: late %d/%d, live %d flagged; want %d/%d late, no live",
					n, c.lateFlag, c.late, c.liveFlag, want, n.queue)
			}
		}
	}
}

// Recall at a queue large enough to matter to the GRP_TXT rate rule: the
// share's denominator holds the queue-only rows, so a queue of 40 GRP_TXT
// (delivered FIFO at 5 rows a second) is flagged only when at least as many
// shared rows older than the gap come with it: after a 20- or 40-minute
// outage at 6 shared rows a minute, not after 3-10 minutes, and never at 1
// shared row a minute.
func TestLateDeliveryRecallOfALargeQueue(t *testing.T) {
	for _, c := range []struct {
		period int // seconds between shared rows
		outage []int
		want   int
	}{
		{10, []int{3, 5, 10}, 0},
		{10, []int{20, 40}, 40},
		{60, []int{3, 5, 10, 20, 40}, 0},
	} {
		for _, m := range c.outage {
			n := limitNet{period: c.period, outage: m, queue: 40, drainers: 1, rate: 5}
			got := limitFlags(t, n)
			t.Logf("shared every %2ds  outage %2dm: late %d/%d, live %d/%d", c.period, m, got.lateFlag, got.late, got.liveFlag, got.live)
			if got.late != 40 || got.lateFlag != c.want || got.liveFlag != 0 {
				t.Errorf("%+v: late %d/%d, live %d flagged; want %d/40 late, no live", n, got.lateFlag, got.late, got.liveFlag, c.want)
			}
		}
	}
}

// Share 0.5 does not protect a real storm from two skewed clocks: a storm of
// 40 GRP_TXT heard by O (clock right), a third of them also by an observer
// whose clock runs 3 minutes behind and another third by one 7 minutes
// behind, and by nobody else, gives two thirds of O's rows delays of 3 or 7
// minutes. Those span more than the gap, so the 13 events O heard first
// (the ones neither skewed observer heard) are flagged.
func TestLateDeliveryTwoSkewedClocksDefeatTheShare(t *testing.T) {
	var c swCase
	for at := int64(-60 * 60); at < 140*60; at += 30 {
		c.txs = append(c.txs, swTx{pt: 4, obs: []swObs{{observer: 3, at: at}, {observer: 4, at: at + 1}}})
	}
	const start = 60 * 60
	for i := int64(0); i < 40; i++ {
		obs := []swObs{{observer: 0, at: start + 3*i}}
		switch i % 3 {
		case 0:
			obs = append(obs, swObs{observer: 1, at: start + 3*i - 180})
		case 1:
			obs = append(obs, swObs{observer: 2, at: start + 3*i - 420})
		}
		// a live storm: any flag on it is wrong, so its class is live
		c.txs = append(c.txs, swTx{pt: 5, obs: obs})
	}
	for i := range c.txs {
		x := &c.txs[i]
		for k := 1; k < len(x.obs); k++ {
			for j := k; j > 0 && x.obs[j].at < x.obs[j-1].at; j-- {
				x.obs[j], x.obs[j-1] = x.obs[j-1], x.obs[j]
			}
		}
	}
	f := buildSweepFixture(t, c)
	opt := ltOptions(f.path)
	opt.Settle, opt.MaxRows = 30*time.Second, 100000
	got, _ := runExportTo(t, opt)
	n := countSweep(t, got)
	t.Logf("storm and background: live %d/%d flagged", n.liveFlag, n.live)
	if n.liveFlag != 13 {
		t.Fatalf("%d live events flagged, want the 13 storm events O heard first", n.liveFlag)
	}
}

// The t + gap bound of the elsewhere time is exclusive: a peer exactly at
// t + gap is not "within the gap", so an older peer still makes the row
// already seen; a peer one second earlier is within the gap.
func TestLateQueryPeerAtExactlyTheGapIsNotWithin(t *testing.T) {
	const t0, o, p, q = 60 * 60, 81001, 81002, 81003
	gap := int64(ltGap / time.Second)
	f := buildFixtureOf(t, []blTx{
		{at: fxAt(t0 * time.Second), pt: 5, obs: []blObs{{o, 0}, {p, -3 * time.Minute}, {q, ltGap}}},
		{at: fxAt((t0 + 600) * time.Second), pt: 5, obs: []blObs{{o, 0}, {p, -3 * time.Minute}, {q, ltGap - time.Second}}},
	})
	db, err := openReadOnly(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(lateQuery, fxT.Unix(), fxAt(2*time.Hour).Unix(), 100, gap)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]sql.NullInt64{}
	for rows.Next() {
		var observer, elsewhere sql.NullInt64
		var ts int64
		if err := rows.Scan(&observer, &ts, &elsewhere); err != nil {
			t.Fatal(err)
		}
		if observer.Int64 == o {
			got[ts-fxT.Unix()] = elsewhere
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for ts, want := range map[int64]int64{t0: t0 - 180, t0 + 600: t0 + 600 + gap - 1} {
		if e := got[ts]; !e.Valid || e.Int64-fxT.Unix() != want {
			t.Fatalf("row at %d s: elsewhere %+v, want %d s", ts, e, want)
		}
	}
	l := ltTracker(1, 100)
	for ts, want := range map[int64]bool{t0: true, t0 + 600: false} {
		e := got[ts]
		if seen, _ := l.see(o, ts+fxT.Unix(), e); seen != want {
			t.Fatalf("row at %d s: already seen %v, want %v", ts, seen, want)
		}
	}
}
