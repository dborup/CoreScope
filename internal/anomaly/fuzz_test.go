package anomaly

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// Fuzz targets. Seeds are added in code only; no corpus files are kept.

func keyFromBytes(b []byte) Key {
	get := func(i int) byte {
		if i < len(b) {
			return b[i]
		}
		return 0
	}
	k := Key{Scope: Scope(get(0)%4 + 1), PayloadType: PayloadType(get(1) & 15)}
	if n := int(get(2) % 5); n > 0 {
		cb := make([]byte, n)
		for i := range cb {
			cb[i] = get(3 + i)
		}
		k.Channel, _ = NewChannelHash(cb)
	}
	k.RouteClass = RouteClass(get(8) % 6)
	k.RouteKind = RouteKind(get(9) % 4)
	if w := int(get(10) % 4); w > 0 {
		k.FirstHop, _ = NewHop([]byte{get(11), get(12), get(13)}[:w])
	}
	// make the key canonical for its scope
	switch k.Scope {
	case ScopeStream:
		if k.RouteKind == RouteKindRelay && k.FirstHop.IsZero() {
			k.FirstHop, _ = NewHop([]byte{get(11)})
		}
		if k.RouteKind != RouteKindRelay {
			k.FirstHop = Hop{}
		}
		if !kindFitsClass(k.RouteKind, k.RouteClass) {
			k.RouteClass = RouteClassMixed // allowed for every kind
		}
	case ScopeChannel:
		k.RouteClass, k.RouteKind, k.FirstHop = 0, 0, Hop{}
	case ScopeRouteGroup:
		k.Channel, k.RouteClass, k.RouteKind = ChannelHash{}, 0, RouteKindRelay
		if k.FirstHop.IsZero() {
			k.FirstHop, _ = NewHop([]byte{get(11)})
		}
	case ScopeGlobal:
		k.Channel, k.RouteClass, k.RouteKind, k.FirstHop = ChannelHash{}, 0, 0, Hop{}
	}
	return k
}

func FuzzKeyEncoding(f *testing.F) {
	f.Add([]byte{0, 5, 1, 0xab, 0, 0, 0, 0, 1, 1, 1, 0x5c}, []byte{0, 5, 1, 0xab, 0, 0, 0, 0, 1, 1, 2, 0x5c, 0x12})
	f.Add([]byte{3, 5}, []byte{2, 5, 1, 0})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		k1, k2 := keyFromBytes(a), keyFromBytes(b)
		for _, k := range []Key{k1, k2} {
			if err := k.Validate(); err != nil {
				t.Fatalf("generated key invalid: %v", err)
			}
			back, err := ParseKey(k.String())
			if err != nil || back != k {
				t.Fatalf("round trip failed for %s: %v", k, err)
			}
		}
		if (k1 == k2) != (k1.String() == k2.String()) {
			t.Fatalf("encoding not injective: %s vs %s", k1, k2)
		}
	})
}

func FuzzParseKeyIsCanonical(f *testing.F) {
	f.Add("v1|s=stream|pt=5|ch=1:ab|rc=flood|rk=relay|hop=1:5c")
	f.Add("v1|s=global|pt=0|ch=-|rc=unknown|rk=unknown|hop=-")
	f.Add("v1|s=route|pt=5|ch=-|rc=unknown|rk=relay|hop=3:000000")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := ParseKey(s)
		if err == nil && k.String() != s {
			t.Fatalf("accepted non-canonical %q (canonical %q)", s, k.String())
		}
	})
}

// fuzzEvent decodes 8 bytes into an event near a moving clock.
func fuzzEvent(b []byte, i int, clock *time.Time) Event {
	g := func(j int) byte {
		if j < len(b) {
			return b[j]
		}
		return 0
	}
	// time step in [-10 s, +245 s]: negative steps create late / reordered events
	step := time.Duration(int(g(0))-10) * time.Second
	*clock = clock.Add(step)
	e := Event{ID: TxID(fmt.Sprintf("f%d-%d", g(7)%16, i)), Time: *clock, PayloadType: PayloadType(g(1) % 16), Traffic: TrafficClass(g(6) % 3)}
	if g(1)%3 == 0 {
		e.ID = TxID(fmt.Sprintf("dup%d", g(7)%8)) // frequent duplicate IDs
	}
	if e.PayloadType == 5 || e.PayloadType == 6 {
		e.Channel, _ = NewChannelHash([]byte{g(2) % 8})
	}
	switch g(3) % 4 {
	case 0:
		e.RouteClass, e.RouteKind = RouteClassFlood, RouteKindNoPath
	case 1:
		e.RouteClass, e.RouteKind = RouteClassDirect, RouteKindDirect
	default:
		e.RouteClass, e.RouteKind = RouteClassFlood, RouteKindRelay
		if e.PayloadType == PayloadTypeTrace {
			e.RouteKind = RouteKindUnknown
		} else {
			w := int(g(4)%3) + 1
			e.FirstHop, _ = NewHop([]byte{g(5) % 4, g(5), g(4)}[:w])
		}
	}
	e.WireBytes = int(g(6))
	return e
}

func fuzzConfig() Config {
	lim := Limits{MaxKeysPerScope: 8, StateTTL: 7 * time.Hour, DedupHorizon: 2 * time.Hour, MaxDedupIDs: 16,
		ReorderDelay: 20 * time.Second, MaxReorderBuffer: 4, MaxWindowEntries: 6, MaxCandidatesPerCall: 3, MaxPendingCandidates: 12}
	st := StateParams{ConfirmAfter: time.Minute, EndAfterQuiet: 10 * time.Minute, Cooldown: 20 * time.Minute}
	return Config{Limits: lim, Expected: ExpectedPolicy{Mode: ExpectedSuppress},
		Rate: []RateRule{
			{Name: "s1", Scope: ScopeStream, Window: time.Minute, Threshold: 2, State: st},
			{Name: "c5", Scope: ScopeChannel, Window: 5 * time.Minute, Threshold: 3, State: st},
			{Name: "r15", Scope: ScopeRouteGroup, Window: 15 * time.Minute, Threshold: 3, State: st},
			{Name: "g60", Scope: ScopeGlobal, Window: time.Hour, Threshold: 5, State: st},
		},
		NewStream: []NewStreamRule{{Name: "ns", Scope: ScopeStream, Window: time.Hour, MinCount: 2, CensorWindow: time.Hour, QuietPeriod: time.Hour,
			State: StateParams{EndAfterQuiet: 10 * time.Minute, Cooldown: 20 * time.Minute}}},
		Periodic: []PeriodicRule{{Name: "p", Scope: ScopeStream, MinPeriod: time.Minute, MaxPeriod: 2 * time.Hour, BurstGap: 2 * time.Second,
			JitterAbs: 5 * time.Second, JitterRel: 0.05, MaxMissing: 2, MinPulses: 4, HistoryLen: 8, MinCoverage: 0.5,
			MaxJitterFraction: 1, MaxChance: 1, State: st}},
	}
}

func checkInvariants(t *testing.T, d *Detector, r Result) {
	t.Helper()
	lim := d.cfg.Limits
	for _, c := range r.Candidates {
		if !legalTransition(c.From, c.To, c.Reason) {
			t.Fatalf("illegal transition %v->%v %v", c.From, c.To, c.Reason)
		}
		if c.DecidedAt.Before(c.At) {
			t.Fatal("DecidedAt before At")
		}
		if c.Key.Validate() != nil {
			t.Fatal("candidate key invalid")
		}
		if c.Key.Scope == ScopeGlobal && (!c.Key.FirstHop.IsZero() || !c.Key.Channel.IsZero()) {
			t.Fatal("global candidate attributed")
		}
	}
	if len(r.Candidates) > lim.MaxCandidatesPerCall {
		t.Fatal("per-call cap exceeded")
	}
	s := d.Snapshot()
	for sc, n := range s.Keys {
		if n > lim.MaxKeysPerScope {
			t.Fatalf("scope %d holds %d keys", sc, n)
		}
	}
	if s.DedupIDs > lim.MaxDedupIDs || s.Buffered > lim.MaxReorderBuffer || s.PendingCandidates > lim.MaxPendingCandidates {
		t.Fatalf("bounded structure exceeded: %+v", s)
	}
	for _, tb := range d.scopes {
		if tb == nil {
			continue
		}
		for _, k := range tb.m {
			if k.win == nil {
				continue
			}
			if k.win.live() > lim.MaxWindowEntries {
				t.Fatal("window entries exceed cap")
			}
			for _, sm := range k.win.sums {
				if sm.n > s.Accepted || sm.exp > sm.n || sm.mon > sm.n {
					t.Fatalf("window sum out of range: %+v accepted=%d", sm, s.Accepted)
				}
			}
		}
	}
	if d.deadlines.Len() > 4*d.liveEpisodes()+1024 {
		t.Fatal("deadline heap unbounded")
	}
}

func FuzzDetectorNoPanicAndBounded(f *testing.F) {
	f.Add([]byte{10, 5, 1, 2, 0, 7, 0, 1, 70, 5, 1, 2, 0, 7, 0, 2, 0, 5, 1, 2, 0, 7, 0, 3})
	f.Add([]byte{255, 4, 0, 0, 1, 2, 1, 9, 0, 9, 3, 3, 2, 2, 2, 2})
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := New(fuzzConfig())
		if err != nil {
			t.Fatal(err)
		}
		clock := t0
		for i := 0; i+8 <= len(data) && i < 8*400; i += 8 {
			chunk := data[i : i+8]
			var r Result
			if chunk[7]%11 == 0 {
				r = d.Advance(clock.Add(time.Duration(chunk[0]) * time.Minute))
			} else {
				r = d.Observe(fuzzEvent(chunk, i, &clock))
			}
			checkInvariants(t, d, r)
		}
		checkInvariants(t, d, d.Flush())
		for r := d.Drain(); ; r = d.Drain() {
			checkInvariants(t, d, r)
			if !r.More {
				break
			}
		}
	})
}

func collectAll(t *testing.T, cfg Config, evs []Event) []Candidate {
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out []Candidate
	for _, e := range evs {
		out = append(out, d.Observe(e).Candidates...)
	}
	out = append(out, d.Flush().Candidates...)
	for r := d.Drain(); ; r = d.Drain() {
		out = append(out, r.Candidates...)
		if !r.More {
			break
		}
	}
	return out
}

func FuzzDedupIdempotent(f *testing.F) {
	f.Add([]byte{10, 5, 1, 2, 0, 7, 0, 1, 20, 5, 1, 2, 0, 7, 0, 2, 30, 5, 1, 2, 0, 7, 0, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := fuzzConfig()
		cfg.Limits.MaxDedupIDs = 100_000
		cfg.Limits.MaxReorderBuffer = 100_000
		cfg.Limits.MaxCandidatesPerCall = 100_000
		cfg.Limits.MaxPendingCandidates = 100_000
		clock := t0
		var once, twice []Event
		for i := 0; i+8 <= len(data) && i < 8*300; i += 8 {
			e := fuzzEvent(data[i:i+8], i, &clock)
			if e.Validate() != nil {
				continue
			}
			once = append(once, e)
			twice = append(twice, e, e)
		}
		a, b := collectAll(t, cfg, once), collectAll(t, cfg, twice)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("duplicate events changed the output: %d vs %d candidates", len(a), len(b))
		}
	})
}

func FuzzNormalizePermutation(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, rot uint8) {
		var os []Observation
		for i := 0; i+4 <= len(data) && len(os) < 32; i += 4 {
			o := Observation{TxID: "tx", ObsID: fmt.Sprintf("o%02d", i/4), Time: t0.Add(time.Duration(data[i]) * time.Second),
				ArrivedAt: t0.Add(time.Duration(data[i+1]) * time.Second), PayloadType: 5, Channel: mustChan(t, 0x42), WireBytes: 50}
			switch data[i+2] % 3 {
			case 0:
				o.RouteClass = RouteClassFlood
				w := int(data[i+3]%3) + 1
				hops := make([]byte, w*int(data[i+3]%4+1))
				for j := range hops {
					hops[j] = data[i+3] + byte(j)
				}
				o.Path, _ = NewRoute(w, hops)
			case 1:
				o.RouteClass = RouteClassFlood
			default:
				o.RouteClass = RouteClassDirect
			}
			os = append(os, o)
		}
		if len(os) == 0 {
			return
		}
		a, ra, errA := NormalizeTransmission(os, 60*time.Second)
		p := make([]Observation, len(os))
		for i := range os {
			p[(i+int(rot))%len(os)] = os[i]
		}
		for i, j := 0, len(p)-1; i < j && rot%2 == 1; i, j = i+1, j-1 {
			p[i], p[j] = p[j], p[i]
		}
		b, rb, errB := NormalizeTransmission(p, 60*time.Second)
		if (errA == nil) != (errB == nil) || !reflect.DeepEqual(a, b) || ra != rb {
			t.Fatalf("observation order changed the event: %+v vs %+v", a, b)
		}
		if errA == nil {
			if err := a.Validate(); err != nil {
				t.Fatalf("normalized event invalid: %v", err)
			}
		}
	})
}

func FuzzStateMachineLegal(f *testing.F) {
	f.Add([]byte{0, 1, 1, 5, 2, 200, 0, 9})
	f.Fuzz(func(t *testing.T, ops []byte) {
		p := StateParams{ConfirmAfter: time.Duration(len(ops)%3) * time.Minute, EndAfterQuiet: 5 * time.Minute, Cooldown: time.Duration(len(ops)%4) * 10 * time.Minute}
		var ep episode
		now := t0.UnixNano()
		for i := 0; i+1 < len(ops) && i < 2000; i += 2 {
			now += int64(ops[i+1]) * int64(15*time.Second)
			var trs []transition
			switch ops[i] % 3 {
			case 0:
				trs = ep.onTime(now, &p, nil)
				trs = ep.onHot(now, &p, trs)
			case 1:
				trs = ep.onTime(now, &p, nil)
			case 2:
				trs = ep.evict(now, nil)
			}
			for _, tr := range trs {
				if !legalTransition(tr.from, tr.to, tr.reason) || tr.at > now {
					t.Fatalf("illegal or future transition %+v", tr)
				}
			}
		}
	})
}
