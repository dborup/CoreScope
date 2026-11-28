package main

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// neighbor_edges.count decay (#544).
//
// Before decay, an edge's count was a lifetime total that only ever grew,
// so credit that should never have been given (bridge TX copies before
// #540) stayed on active edges for good. Decay turns the count into an
// exponentially weighted credit total: once per whole day every count
// above neighborEdgeDecayFloor is multiplied by 0.5^(1/halfLifeDays) and
// rounded. Old credit, inflated or not, halves every half-life. An edge
// credited r times a day settles near r * f/(1-f), about 9.6 r at a 7-day
// half-life (the same half-life as the server's affinity score,
// affinityHalfLifeHours). Rounding keeps that within about 0.5/(1-f),
// about 5 credits, of the exact value.
//
// Decay is opt-in (neighborEdgeCountHalfLifeDays > 0) and irreversible:
// disabling it stops further decay but cannot restore the lifetime counts.
// It changes hop disambiguation, because affinity then follows recent
// traffic: an edge with large old credit can lose a hop to one with a
// recent burst (PR #574 review F1). scripts/neighbor-edge-decay-replay.sh
// measures that on a copy of a real database.
//
// Decay changes weights only. It never deletes an edge and never touches
// last_seen (the builder's delta watermark), so retention still decides
// which edges exist and the builder's scan resumes where it was.

// neighborEdgeDecayFloor is the lowest count decay produces. It is the
// server's affinityMinObservations: the hop resolver (resolveWithContext)
// only resolves a hop by affinity when the best edge has at least 3
// credits, the one absolute threshold any reader applies to persisted
// counts. Every other reader of neighbor_edges uses counts as relative
// weights (affinity score ratios, bridge/coverage scores, the
// count-weighted position estimate, GPS sanity) or only needs the edge to
// exist (reach degree, seenViaPackets, area bridges, the ingestor's own
// path resolver). The neighbour list and neighbour graph APIs are built
// from in-memory packets and do not read the table. At the default
// half-life, rounding already leaves counts of 5 or less unchanged.
const neighborEdgeDecayFloor = 3

// neighborEdgeDecayClockTolerance is how far the stored clock may be ahead
// of now before it is reset. Within it the check is a no-op; beyond it the
// host clock was once wrong, and waiting for real time to catch up would
// stall decay for that long.
const neighborEdgeDecayClockTolerance = 24 * time.Hour

// neighborEdgeDecayStep is the decay period. Integer counts cannot take
// finer steps: rounding at, say, hourly factors would stop decay entirely.
const neighborEdgeDecayStep = 24 * time.Hour

// NeighborEdgeDecayCheckInterval is how often the ingestor checks whether
// a whole day has passed since the last applied step.
const NeighborEdgeDecayCheckInterval = time.Hour

// neighborEdgeDecay reports one DecayNeighborEdgeCounts call.
type neighborEdgeDecay struct {
	Steps      int   // whole days applied
	Lowered    int64 // edge-count updates over all steps
	ClockReset bool  // the stored clock was more than a day ahead and was reset to now
}

func ensureNeighborEdgeDecayState(db *sql.DB) error {
	// PREFLIGHT: async=true reason="CREATE TABLE IF NOT EXISTS for a one-row state table; constant cost at any scale"
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS neighbor_edge_decay_state (
		id              INTEGER PRIMARY KEY CHECK (id = 1),
		decayed_through INTEGER NOT NULL,
		updated_at      TEXT NOT NULL
	)`)
	return err
}

// DecayNeighborEdgeCounts applies every whole day of decay due at now.
//
// The clock (neighbor_edge_decay_state.decayed_through, unix seconds)
// survives restarts. The first call only starts it, so nothing decays
// until a day after decay was first enabled. Each step runs in its own
// writer transaction that also advances the clock by exactly one day, so
// an interrupted catch-up resumes where it stopped, checks never drift,
// and a catch-up after downtime gives the same counts as daily checks.
// Once a step lowers nothing, no count can go lower (the floor, or the
// rounding plateau) and the remaining days only move the clock. A clock
// more than neighborEdgeDecayClockTolerance ahead of now is reset to now
// without decaying, so the next step is one day later.
//
// halfLifeDays <= 0 disables decay: no count changes and the clock is
// forgotten, so enabling decay again starts fresh instead of catching up
// the disabled period.
func (s *Store) DecayNeighborEdgeCounts(now time.Time, halfLifeDays int) (neighborEdgeDecay, error) {
	var d neighborEdgeDecay
	if err := ensureNeighborEdgeDecayState(s.db); err != nil {
		return d, fmt.Errorf("decay state: %w", err)
	}
	if halfLifeDays <= 0 {
		_, err := s.WriterExec("neighbor_decay", `DELETE FROM neighbor_edge_decay_state`)
		return d, err
	}
	var through int64
	err := s.db.QueryRow(`SELECT decayed_through FROM neighbor_edge_decay_state WHERE id = 1`).Scan(&through)
	if err == sql.ErrNoRows {
		_, err = s.WriterExec("neighbor_decay", `INSERT INTO neighbor_edge_decay_state (id, decayed_through, updated_at) VALUES (1, ?, ?)`,
			now.Unix(), now.UTC().Format(time.RFC3339))
		return d, err
	}
	if err != nil {
		return d, fmt.Errorf("read decay state: %w", err)
	}
	if through > now.Add(neighborEdgeDecayClockTolerance).Unix() {
		_, err := s.WriterExec("neighbor_decay", `UPDATE neighbor_edge_decay_state SET decayed_through = ?, updated_at = ? WHERE id = 1`,
			now.Unix(), now.UTC().Format(time.RFC3339))
		d.ClockReset = err == nil
		return d, err
	}
	step := int64(neighborEdgeDecayStep / time.Second)
	due := (now.Unix() - through) / step
	if due <= 0 {
		return d, nil
	}
	factor := math.Pow(0.5, 1/float64(halfLifeDays))
	for d.Steps < int(due) {
		var lowered int64
		err := s.WriterTx("neighbor_decay", func(tx *sql.Tx) error {
			res, err := tx.Exec(`UPDATE neighbor_edges
				SET count = MAX(?1, CAST(ROUND(count * ?2) AS INTEGER))
				WHERE count > ?1 AND CAST(ROUND(count * ?2) AS INTEGER) < count`,
				neighborEdgeDecayFloor, factor)
			if err != nil {
				return fmt.Errorf("decay counts: %w", err)
			}
			lowered, _ = res.RowsAffected()
			next := through + int64(d.Steps+1)*step
			if lowered == 0 {
				next = through + due*step // nothing left to lower: skip to the last whole day
			}
			if _, err := tx.Exec(`UPDATE neighbor_edge_decay_state SET decayed_through = ?, updated_at = ? WHERE id = 1`,
				next, now.UTC().Format(time.RFC3339)); err != nil {
				return fmt.Errorf("advance decay state: %w", err)
			}
			return nil
		})
		if err != nil {
			return d, err
		}
		d.Lowered += lowered
		if lowered == 0 {
			d.Steps = int(due)
			break
		}
		d.Steps++
	}
	return d, nil
}

// StartNeighborEdgeDecay checks for due decay once at start and then every
// interval until the returned stop function is called. With
// halfLifeDays <= 0 the single check forgets the clock and no ticker runs.
func (s *Store) StartNeighborEdgeDecay(halfLifeDays int, interval time.Duration) func() {
	if interval <= 0 {
		interval = NeighborEdgeDecayCheckInterval
	}
	check := func() {
		d, err := s.DecayNeighborEdgeCounts(time.Now(), halfLifeDays)
		if err != nil {
			log.Printf("[neighbor-decay] error: %v", err)
		} else if d.ClockReset {
			log.Printf("[neighbor-decay] stored clock was more than %s ahead of now; reset it, next decay in one day", neighborEdgeDecayClockTolerance)
		} else if d.Steps > 0 {
			log.Printf("[neighbor-decay] applied %d day(s) at a %d-day half-life: %d edge counts lowered", d.Steps, halfLifeDays, d.Lowered)
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		check()
		if halfLifeDays <= 0 {
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				check()
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

// startNeighborEdgeDecayFromConfig is the ingestor's decay wiring: it
// starts the background check at the configured half-life and logs the
// mode. It returns the stop function and the half-life in use (0 when
// disabled).
func startNeighborEdgeDecayFromConfig(s *Store, cfg *Config, interval time.Duration) (func(), int) {
	halfLife := cfg.NeighborEdgeCountHalfLifeDaysOrDefault()
	stop := s.StartNeighborEdgeDecay(halfLife, interval)
	if halfLife > 0 {
		log.Printf("[neighbor-decay] enabled: edge counts halve every %d days, never below %d", halfLife, neighborEdgeDecayFloor)
	} else {
		log.Printf("[neighbor-decay] disabled (opt-in: set neighborEdgeCountHalfLifeDays > 0): edge counts only grow")
	}
	return stop, halfLife
}
