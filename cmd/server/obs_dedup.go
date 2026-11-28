package main

// Observation dedup per transmission (#411).
//
// The store keeps one observation per (observer ID, path_json) pair of a
// transmission and counts the distinct non-empty observer IDs. Most
// transmissions are heard a handful of times, so a scan of tx.Observations
// answers both questions without any per-transmission allocation. A
// transmission that reaches obsIndexThreshold observations builds two maps
// (obsKeys, observerSet) keyed by the strings the observations already hold,
// so a busy transmission keeps O(1) checks and no key is ever concatenated.
//
// The threshold comes from the distributions measured for #411. On a light
// synthetic 30K-transmission set (median 4 distinct pairs, p99 21, p99.9 36)
// about 0.2% of transmissions reach 32. With the production mean (about 18
// observations per transmission: median 15, p90 35, p99 69) about 13% build
// the maps, and the heap still drops by about 19%. A threshold of 64 measured
// about 5% less heap on top of that and is a possible follow-up.
// BenchmarkObsDedup: a missing check scans
// 31 observations in 100-130 ns (180 ns when one observer holds them all),
// against 55-95 ns for the old lookup, which also allocated an 80-100 byte
// key on every check; from 32 on it is a 25 ns map lookup.
const obsIndexThreshold = 32

// obsDedupKey identifies an observation within its transmission.
type obsDedupKey struct {
	observerID, pathJSON string
}

// Self-accounted cost of the maps of a transmission past obsIndexThreshold,
// measured for #411 (Go swiss maps, 32 to 600 entries): about 86 B per
// obsKeys entry and 40 B per observerSet entry including growth slack, plus
// the two map headers.
const (
	obsIndexRecordBytes   = 96
	obsIndexKeyBytes      = 88
	obsIndexObserverBytes = 40
)

// obsIndexBytes is what trackedBytes holds for tx's dedup maps. It is a
// function of the maps alone, so eviction re-derives the charge (see
// txChargedBytes); the maps only grow while tx is in the store.
func obsIndexBytes(tx *StoreTx) int64 {
	if tx.obsKeys == nil {
		return 0
	}
	return obsIndexRecordBytes + int64(len(tx.obsKeys))*obsIndexKeyBytes + int64(len(tx.observerSet))*obsIndexObserverBytes
}

// lookupObservation reports whether tx already holds an observation by
// observerID on pathJSON, and whether observerID (when not empty) already
// observed tx.
func (tx *StoreTx) lookupObservation(observerID, pathJSON string) (dup, knownObserver bool) {
	if tx.obsKeys != nil {
		_, dup = tx.obsKeys[obsDedupKey{observerID, pathJSON}]
		_, knownObserver = tx.observerSet[observerID]
		return dup, knownObserver
	}
	for _, o := range tx.Observations {
		if o.ObserverID != observerID {
			continue
		}
		if o.PathJSON == pathJSON {
			return true, observerID != ""
		}
		knownObserver = observerID != ""
	}
	return false, knownObserver
}

// addObservation appends obs, for which lookupObservation reported no
// duplicate and knownObserver, and keeps ObservationCount,
// UniqueObserverCount and the dedup maps in step. It returns the change in
// obsIndexBytes(tx), for the caller to add to trackedBytes.
func (tx *StoreTx) addObservation(obs *StoreObs, knownObserver bool) int64 {
	before := obsIndexBytes(tx)
	if obs.ObserverID != "" && !knownObserver {
		tx.UniqueObserverCount++
		if tx.observerSet != nil {
			tx.observerSet[obs.ObserverID] = struct{}{}
		}
	}
	tx.Observations = append(tx.Observations, obs)
	tx.ObservationCount++
	if tx.obsKeys != nil {
		tx.obsKeys[obsDedupKey{obs.ObserverID, obs.PathJSON}] = struct{}{}
	} else if len(tx.Observations) >= obsIndexThreshold {
		tx.obsKeys = make(map[obsDedupKey]struct{}, len(tx.Observations))
		tx.observerSet = make(map[string]struct{})
		for _, o := range tx.Observations {
			tx.obsKeys[obsDedupKey{o.ObserverID, o.PathJSON}] = struct{}{}
			if o.ObserverID != "" {
				tx.observerSet[o.ObserverID] = struct{}{}
			}
		}
	}
	return obsIndexBytes(tx) - before
}
