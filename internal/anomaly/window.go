package anomaly

// Sliding-window counting for rate rules.
//
// Semantics: count(W, t) is the number of accepted events of the key with
// t-W < time <= t, evaluated at the time t of the event just added. An
// event exactly W older than t is outside the window. Events reach a key in
// non-decreasing data time (the Detector's reorder buffer guarantees it).
//
// Memory: one entry per distinct timestamp in the longest window, capped by
// Limits.MaxWindowEntries. On overflow the two oldest entries are merged;
// the merged counts stay in every window that held the newer entry until it
// expires, so counts can be overstated (never understated). Every merge is
// counted and flagged on candidates of that key.

type winEntry struct {
	t           int64
	n, exp, mon uint32
	_           uint32 // pad to 24 bytes, keeps entries aligned
}

type winSum struct {
	n, exp, mon uint64
}

// windowSet tracks every distinct window width used by the rate rules of one
// scope, for one key.
type windowSet struct {
	entries   []winEntry
	head      int
	starts    []int // per width: absolute index of the first entry inside
	sums      []winSum
	lastMerge int64 // data time of the last overflow merge (0 = never)
}

func newWindowSet(nWidths int) *windowSet {
	return &windowSet{starts: make([]int, nWidths), sums: make([]winSum, nWidths)}
}

// add records one event at time t. widths are in nanoseconds; maxEntries
// bounds the live entries. It returns true if an overflow merge happened.
func (w *windowSet) add(t int64, cls TrafficClass, widths []int64, maxEntries int) (merged bool) {
	var e, m uint32
	switch cls {
	case TrafficExpected:
		e = 1
	case TrafficMonitored:
		m = 1
	}
	if last := len(w.entries) - 1; last >= w.head && w.entries[last].t == t {
		w.entries[last].n++
		w.entries[last].exp += e
		w.entries[last].mon += m
	} else {
		w.entries = append(w.entries, winEntry{t: t, n: 1, exp: e, mon: m})
	}
	for i, width := range widths {
		s := &w.sums[i]
		s.n++
		s.exp += uint64(e)
		s.mon += uint64(m)
		lo := t - width
		for w.starts[i] < len(w.entries) && w.entries[w.starts[i]].t <= lo {
			x := w.entries[w.starts[i]]
			s.n -= uint64(x.n)
			s.exp -= uint64(x.exp)
			s.mon -= uint64(x.mon)
			w.starts[i]++
		}
	}
	// Entries before every window start can go.
	minStart := len(w.entries)
	for _, st := range w.starts {
		if st < minStart {
			minStart = st
		}
	}
	for w.head < minStart {
		w.entries[w.head] = winEntry{}
		w.head++
	}
	for len(w.entries)-w.head > maxEntries {
		w.mergeOldest()
		w.lastMerge = t
		merged = true
	}
	w.compact()
	return merged
}

// mergeOldest folds entries[head] into entries[head+1].
func (w *windowSet) mergeOldest() {
	old := w.entries[w.head]
	nxt := &w.entries[w.head+1]
	nxt.n += old.n
	nxt.exp += old.exp
	nxt.mon += old.mon
	oldIdx := w.head
	w.entries[oldIdx] = winEntry{}
	w.head++
	for i := range w.starts {
		switch {
		case w.starts[i] <= oldIdx:
			// old was inside; its counts now live in nxt, also inside.
			w.starts[i] = w.head
		case w.starts[i] == w.head:
			// old was outside but nxt is inside: nxt now carries old's
			// counts, so the window overstates by them until nxt expires.
			w.sums[i].n += uint64(old.n)
			w.sums[i].exp += uint64(old.exp)
			w.sums[i].mon += uint64(old.mon)
		}
	}
}

func (w *windowSet) compact() {
	if w.head >= 32 && w.head*2 >= len(w.entries) {
		n := copy(w.entries, w.entries[w.head:])
		for i := n; i < len(w.entries); i++ {
			w.entries[i] = winEntry{}
		}
		w.entries = w.entries[:n]
		for i := range w.starts {
			w.starts[i] -= w.head
		}
		w.head = 0
	}
}

func (w *windowSet) live() int { return len(w.entries) - w.head }
