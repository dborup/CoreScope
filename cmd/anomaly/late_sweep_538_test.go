package main

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

// Issue #538: the late-delivery flag on randomised synthetic networks,
// through the real export. Each seed builds 8 observers, two of them with
// clocks 3-10 minutes behind; ADVERT background every 30 s; two real
// GRP_TXT storms of 30-150 transmissions over 20 s - 5 min with a random
// hearing probability; and one observer outage of 5-40 minutes, after
// which that observer delivers its queue (its rows of the outage plus 10-40
// transmissions it alone heard) stamped with the delivery time.
//
// Each transmission's class is known from the generator and written into
// its payload size, so a line's payload_bytes tells whether its event is
// live (first observation on time; skewed clocks included) or late (its
// first observation is a delivered queue row). A live event whose first
// observation is the draining observer's, within two windows of its
// delivery, has a class of its own: the flag marks whole bursts, so it may
// mark those.

const (
	swLiveAdvert = 100
	swNearAdvert = 95
	swLateAdvert = 90
	swLiveGrp    = 23
	swNearGrp    = 28
	swLateGrp    = 33
)

type swObs struct {
	observer int
	at       int64 // seconds from fxT
	late     bool  // a queue row stamped at delivery
}

type swTx struct {
	pt   int
	obs  []swObs
	late bool // the event's first observation is a queue row
	near bool // live, first observed by the draining observer near its delivery
}

type swCase struct {
	outage, queue int // minutes, unique queued transmissions
	txs           []swTx
}

func sweepCase(seed uint64) swCase {
	r := rand.New(rand.NewPCG(seed, 538))
	const nObs = 8
	offset := make([]int64, nObs)
	offset[0] = -int64(180 + r.IntN(421))
	offset[1] = -int64(180 + r.IntN(421))
	drain := 2 + r.IntN(nObs-2)
	s0 := int64(300 + r.IntN(3301))
	c := swCase{outage: 5 + r.IntN(36), queue: 10 + r.IntN(31)}
	rc := s0 + int64(c.outage)*60
	var queued []*swObs
	hear := func(pt int, at int64, p float64) {
		var obs []swObs
		for len(obs) == 0 {
			for o := 0; o < nObs; o++ {
				if r.Float64() < p {
					obs = append(obs, swObs{observer: o, at: at + int64(r.IntN(21)) + offset[o]})
				}
			}
		}
		c.txs = append(c.txs, swTx{pt: pt, obs: obs})
	}
	for at := int64(-150 * 60); at < 140*60; at += 30 {
		hear(4, at+int64(r.IntN(30)), 0.5)
	}
	for s := 0; s < 2; s++ {
		start, n, span, p := int64(600+r.IntN(6001)), 30+r.IntN(121), 20+r.IntN(281), 0.1+0.8*r.Float64()
		for i := 0; i < n; i++ {
			hear(5, start+int64(r.IntN(span)), p)
		}
	}
	for i := 0; i < c.queue; i++ {
		c.txs = append(c.txs, swTx{pt: 5, obs: []swObs{{observer: drain, at: rc}}})
	}
	for i := range c.txs {
		for j := range c.txs[i].obs {
			if o := &c.txs[i].obs[j]; o.observer == drain && o.at >= s0 && o.at <= rc {
				queued = append(queued, o)
			}
		}
	}
	r.Shuffle(len(queued), func(i, j int) { queued[i], queued[j] = queued[j], queued[i] })
	for j, o := range queued {
		o.at, o.late = rc+int64(j/2), true
	}
	for i := range c.txs {
		x := &c.txs[i]
		sort.SliceStable(x.obs, func(a, b int) bool { return x.obs[a].at < x.obs[b].at })
		x.late = x.obs[0].late
		x.near = !x.late && x.obs[0].observer == drain && x.obs[0].at >= rc-2*60 && x.obs[0].at < rc+int64(c.queue+len(queued))/2+3*60
	}
	return c
}

func buildSweepFixture(t *testing.T, c swCase) *fixtureDB {
	f := newFixtureDB(t, false)
	if _, err := f.db.Exec(`PRAGMA synchronous = OFF`); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i, x := range c.txs {
		size := map[[2]bool]int{{false, false}: swLiveAdvert, {false, true}: swLateAdvert,
			{true, false}: swLiveGrp, {true, true}: swLateGrp}[[2]bool{x.pt == 5, x.late}]
		if x.near {
			size = map[bool]int{false: swNearAdvert, true: swNearGrp}[x.pt == 5]
		}
		payload := make([]byte, size)
		if x.pt == 5 {
			payload = grpPayload(0x0a, size-3)
		}
		raw := frameHex(1, x.pt, 1, []byte{0x0b}, payload)
		res, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, payload_type) VALUES (?, ?, '', ?)`,
			raw, fxHash(100000+i), x.pt)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		for _, o := range x.obs {
			if _, err := tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
				VALUES (?, ?, '["0B"]', ?, ?)`, id, 82001+o.observer, fxT.Unix()+o.at, raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

// swCount is flagged/all events of the live, near and late classes.
type swCount struct{ liveFlag, live, nearFlag, near, lateFlag, late int }

func countSweep(t *testing.T, out []byte) swCount {
	t.Helper()
	var c swCount
	for _, r := range readRecords(t, out) {
		if r.PayloadBytes == nil {
			t.Fatal("line without payload_bytes")
		}
		switch *r.PayloadBytes {
		case swLiveAdvert, swLiveGrp:
			c.live++
			if r.IngestBacklog {
				c.liveFlag++
			}
		case swNearAdvert, swNearGrp:
			c.near++
			if r.IngestBacklog {
				c.nearFlag++
			}
		case swLateAdvert, swLateGrp:
			c.late++
			if r.IngestBacklog {
				c.lateFlag++
			}
		default:
			t.Fatalf("payload_bytes %d", *r.PayloadBytes)
		}
	}
	return c
}

// On every seed the late-delivery flag marks no live event, also with two
// skewed clocks, and it marks most delivered-queue events; the #473 flag
// marks live events on every seed.
func TestLateDeliverySyntheticSweep(t *testing.T) {
	const seeds = 20
	var late, old swCount
	for seed := uint64(1); seed <= seeds; seed++ {
		c := sweepCase(seed)
		f := buildSweepFixture(t, c)
		opt := ltOptions(f.path)
		opt.Settle, opt.MaxRows = 30*time.Second, 100000
		got, _ := runExportTo(t, opt)
		l := countSweep(t, got)
		opt = fxOptions(f.path)
		opt.Settle, opt.MaxRows = 30*time.Second, 100000
		opt.BacklogSilence, opt.BacklogSpan = 10*time.Minute, 2*time.Hour
		got, _ = runExportTo(t, opt)
		o := countSweep(t, got)
		t.Logf("seed %2d  outage %2dm  queue %2d | live %4d: late-delivery %d, #473 %3d | near %d: %d, %d | late %3d: late-delivery %3d, #473 %3d",
			seed, c.outage, c.queue, l.live, l.liveFlag, o.liveFlag, l.near, l.nearFlag, o.nearFlag, l.late, l.lateFlag, o.lateFlag)
		if l.liveFlag != 0 {
			t.Errorf("seed %d: %d live events flagged", seed, l.liveFlag)
		}
		if o.liveFlag == 0 || l.live != o.live || l.late != o.late || l.late < c.queue {
			t.Errorf("seed %d: counts %+v / %+v", seed, l, o)
		}
		for _, p := range []*swCount{&late, &old} {
			q := l
			if p == &old {
				q = o
			}
			p.liveFlag, p.live, p.lateFlag, p.late = p.liveFlag+q.liveFlag, p.live+q.live, p.lateFlag+q.lateFlag, p.late+q.late
			p.nearFlag, p.near = p.nearFlag+q.nearFlag, p.near+q.near
		}
	}
	share := func(a, b int) string { return fmt.Sprintf("%d/%d (%.1f %%)", a, b, 100*float64(a)/float64(b)) }
	t.Logf("all seeds: live flagged: late-delivery %s, #473 %s", share(late.liveFlag, late.live), share(old.liveFlag, old.live))
	t.Logf("all seeds: near flagged: late-delivery %s, #473 %s", share(late.nearFlag, late.near), share(old.nearFlag, old.near))
	t.Logf("all seeds: late flagged: late-delivery %s, #473 %s", share(late.lateFlag, late.late), share(old.lateFlag, old.late))
	if 2*late.lateFlag < late.late {
		t.Errorf("late-delivery flag marks only %s of the delivered-queue events", share(late.lateFlag, late.late))
	}
}
