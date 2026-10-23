package main

// Observation dedup per transmission (#411). Red stub: the current eager
// maps behind the helpers the tests call; the next commit replaces it.

const obsIndexThreshold = 32

type obsDedupKey struct {
	observerID, pathJSON string
}

const (
	obsIndexRecordBytes   = 96
	obsIndexKeyBytes      = 88
	obsIndexObserverBytes = 40
)

func obsIndexBytes(tx *StoreTx) int64 { return 0 }

func (tx *StoreTx) lookupObservation(observerID, pathJSON string) (dup, knownObserver bool) {
	return tx.obsKeys[observerID+"|"+pathJSON], tx.observerSet[observerID]
}

func (tx *StoreTx) hasObservation(observerID, pathJSON string) bool {
	dup, _ := tx.lookupObservation(observerID, pathJSON)
	return dup
}

func (tx *StoreTx) addObservation(obs *StoreObs, knownObserver bool) int64 {
	if tx.obsKeys == nil {
		tx.obsKeys = make(map[string]bool)
		tx.observerSet = make(map[string]bool)
	}
	tx.obsKeys[obs.ObserverID+"|"+obs.PathJSON] = true
	if obs.ObserverID != "" && !knownObserver {
		tx.observerSet[obs.ObserverID] = true
		tx.UniqueObserverCount++
	}
	tx.Observations = append(tx.Observations, obs)
	tx.ObservationCount++
	return 0
}
