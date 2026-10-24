package main

import (
	"strings"
	"sync"
	"sync/atomic"
)

// Bounded sharing of the observer metadata strings (#489).
//
// Every observation the store holds carries its observer's ID (a 64-hex
// pubkey), name, IATA code and direction. All four come out of a SQL join, so
// a row scan allocates a fresh copy of each for every observation. The large
// fork instance holds about 2.5M observations from about 65 observers, so the
// same ~140 distinct values were stored ten million times: 207 MiB of logical
// string content against 5.3 KiB of distinct values (measured -- see
// TestObsStrings489_Measure and the PR description).
//
// obsStringPool hands out one canonical copy of each value, so equal values
// share immutable storage. It is owned by the PacketStore, so each store
// (and each test) has its own, and it has its own lock: no load or ingest
// path waits for the pool while holding s.mu. The two live paths intern
// while scanning rows, before they take s.mu at all
// (TestObsStringPoolNeverBlocksStoreWriters_489).
//
// Policy, and why:
//
//   - The canonical copies are held for the life of the store. A strongly
//     held copy is what makes the sharing survive a garbage collection: a
//     weak table (unique.Make) hands back a second copy for a value interned
//     again after a GC.
//   - A new value is cloned, so a short string scanned out of a large buffer
//     never pins that buffer.
//   - The table is bounded (obsStringPoolMax). Past the cap a value is
//     returned exactly as it came in: the observation is correct and simply
//     unshared. A full pool never rejects an observation.
//   - Nothing is ever replaced or mutated in place. A renamed observer is a
//     different value, which gets its own entry, so observations recorded
//     before the rename keep the name they were heard with.
const (
	// obsStringPoolMax bounds the table. The steady state is one entry per
	// distinct value: 65 observer IDs + 65 names + 8 IATA codes + 2
	// directions = 140 on the large instance, 48 on the e2e fixture. The cap
	// leaves 58x headroom for observer growth and for renames (a rename adds
	// one entry, and the old name stays so observations heard before it keep
	// sharing their own), and bounds the pool's own retention to 2.2 MB of
	// charge (about 0.7 MB in fact, see obsStringEntryBytes).
	obsStringPoolMax = 8192

	// obsStringEntryBytes is one entry's cost on top of the canonical copy's
	// own bytes: two string headers in the slot, the slot's share of the
	// group control word and the table's slack. Measured on Go swiss maps
	// (TestObsStringPoolRetainedBytes_489): a table filled to its capacity
	// costs 80 B per entry, and the steady state -- 140 entries in a table
	// sized for 256 -- costs 208 B per entry because the table's footprint
	// is fixed below the hint. The higher figure is charged, so the charge
	// is an upper bound at every fill: at the cap it holds 2.2 MB for about
	// 0.7 MB of real retention, and over-charging can only make eviction
	// trigger earlier, never later.
	obsStringEntryBytes = 208
)

// Bits of StoreObs.sharedStrs: set when that field's value is a canonical
// copy the pool holds, i.e. when the observation does not own those bytes.
// estimateStoreObsBytes reads them, so the charge of an observation stays a
// pure function of the observation and eviction credits exactly what ingest
// charged.
const (
	obsSharedObserverID uint8 = 1 << iota
	obsSharedObserverName
	obsSharedObserverIATA
	obsSharedDirection
)

// obsStringPool is the store's bounded canonical table of observer metadata
// strings. A nil pool is a valid disabled pool (every value stays unshared),
// which is what a test store built as &PacketStore{} gets.
type obsStringPool struct {
	mu    sync.RWMutex
	table map[string]string
	// bytes is the pool's own retention: per entry the canonical copy's
	// content plus obsStringEntryBytes. Written under mu, read without it
	// by the accounting path, which runs under s.mu.
	bytes atomic.Int64
	// interned counts successful shares; full counts the calls the cap sent
	// back unshared. Diagnostics only (and the saturation test).
	interned atomic.Int64
	full     atomic.Int64
}

func newObsStringPool() *obsStringPool {
	return &obsStringPool{table: make(map[string]string, 256)}
}

// retainedBytes is what the pool itself holds. It only grows, and no eviction
// credits it: the canonical copies outlive the observations that share them.
func (p *obsStringPool) retainedBytes() int64 {
	if p == nil {
		return 0
	}
	return p.bytes.Load()
}

// entries is the number of canonical copies held.
func (p *obsStringPool) entries() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.table)
}

// intern returns the canonical copy of s and whether it is shared. A hit
// takes only the read lock and allocates nothing. The empty string is never
// interned: it holds no bytes and costs nothing to repeat.
func (p *obsStringPool) intern(s string) (string, bool) {
	if p == nil || s == "" {
		return s, false
	}
	p.mu.RLock()
	c, ok := p.table[s]
	p.mu.RUnlock()
	if ok {
		p.interned.Add(1)
		return c, true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.table[s]; ok {
		p.interned.Add(1)
		return c, true
	}
	if len(p.table) >= obsStringPoolMax {
		p.full.Add(1)
		return s, false
	}
	// Clone so a value scanned out of a larger buffer does not pin it.
	c = strings.Clone(s)
	p.table[c] = c
	p.bytes.Add(int64(len(c)) + obsStringEntryBytes)
	p.interned.Add(1)
	return c, true
}

// obsStrings is one observation's observer metadata after interning, with the
// mask of the fields the pool now holds. Carried from a row scan to the
// StoreObs the scan builds.
type obsStrings struct {
	observerID, observerName, observerIATA, direction string
	shared                                            uint8
}

// internObs interns the four observer metadata strings of one observation.
// Call it where the row is scanned -- for the live ingest paths that is
// before s.mu is taken, so the pool is never waited on under the store lock.
func (p *obsStringPool) internObs(observerID, observerName, observerIATA, direction string) obsStrings {
	var out obsStrings
	var shared bool
	if out.observerID, shared = p.intern(observerID); shared {
		out.shared |= obsSharedObserverID
	}
	if out.observerName, shared = p.intern(observerName); shared {
		out.shared |= obsSharedObserverName
	}
	if out.observerIATA, shared = p.intern(observerIATA); shared {
		out.shared |= obsSharedObserverIATA
	}
	if out.direction, shared = p.intern(direction); shared {
		out.shared |= obsSharedDirection
	}
	return out
}

// apply copies the interned strings and their shared mask onto obs. Used by
// the two live ingest paths, which intern before the lock and build the
// StoreObs under it.
func (m obsStrings) apply(obs *StoreObs) {
	obs.ObserverID, obs.ObserverName = m.observerID, m.observerName
	obs.ObserverIATA, obs.Direction = m.observerIATA, m.direction
	obs.sharedStrs = m.shared
}

// ownedObsStringBytes is the observer metadata content estimateStoreObsBytes
// charges obs for. A field the pool holds costs the observation only its
// string header, which storeObsBaseBytes already covers, so only the fields
// obs owns outright are charged. The pool's own retention is charged once,
// separately (accountedBytes).
func ownedObsStringBytes(obs *StoreObs) int64 {
	var n int64
	if obs.sharedStrs&obsSharedObserverID == 0 {
		n += int64(len(obs.ObserverID))
	}
	if obs.sharedStrs&obsSharedObserverName == 0 {
		n += int64(len(obs.ObserverName))
	}
	if obs.sharedStrs&obsSharedObserverIATA == 0 {
		n += int64(len(obs.ObserverIATA))
	}
	if obs.sharedStrs&obsSharedDirection == 0 {
		n += int64(len(obs.Direction))
	}
	return n
}
