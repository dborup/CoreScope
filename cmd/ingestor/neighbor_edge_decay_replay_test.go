package main

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Replay preparation for the decay measurement (#544, PR #574 review F2).
//
// prepareNeighborEdgeDecayReplay turns one copy of a database into two
// that differ only in neighbor_edges.count, for cmd/server's
// TestNeighborEdgeDecayReplay_DBCopy to compare hop resolution on:
//   - lifetime.db: decay off (the default). The stored counts plus `days`
//     days of new credits.
//   - decayed.db: the same credits, with the real daily decay at
//     halfLifeDays applied after each day.
//
// New credits are the edge's recent daily rate. The rate comes from the
// builder's own rebuild over the retained observations (a scratch copy
// with neighbor_edges emptied), divided by the observation window (the
// number of days that have observations), and is
// credited through the builder's write path at the edge's rebuilt
// last_seen. last_seen therefore stays the same in both outputs, edges the
// rebuild finds that are not stored yet appear in both, and only the
// counts differ.
//
// The source is opened read-only and copied with VACUUM INTO; all writes go
// to files in outDir. Never point it at a live database: use a copy.

type decayReplayPrep struct {
	Days, HalfLifeDays        int
	WindowDays                float64
	StoredEdges, Rebuilt      int
	Credits                   int64
	LifetimePath, DecayedPath string
}

// copyDBReadOnly writes a consistent copy of src to dst without writing to
// src.
func copyDBReadOnly(src, dst string) error {
	ro, err := sql.Open("sqlite", "file:"+src+"?mode=ro")
	if err != nil {
		return err
	}
	defer ro.Close()
	_, err = ro.Exec(`VACUUM INTO ?`, dst)
	return err
}

func prepareNeighborEdgeDecayReplay(src, outDir string, days, halfLifeDays int) (decayReplayPrep, error) {
	p := decayReplayPrep{Days: days, HalfLifeDays: halfLifeDays,
		LifetimePath: filepath.Join(outDir, "lifetime.db"), DecayedPath: filepath.Join(outDir, "decayed.db")}
	if days <= 0 || halfLifeDays <= 0 {
		return p, fmt.Errorf("days %d and half-life %d must be positive", days, halfLifeDays)
	}
	ratesPath := filepath.Join(outDir, "rates.db")
	for _, f := range []string{p.LifetimePath, p.DecayedPath, ratesPath} {
		if _, err := os.Stat(f); err == nil {
			return p, fmt.Errorf("%s exists; use an empty output directory", f)
		}
		if err := copyDBReadOnly(src, f); err != nil {
			return p, fmt.Errorf("copy to %s: %w", f, err)
		}
	}
	defer os.Remove(ratesPath)

	// Recent credits per edge: the builder's rebuild over the retained
	// observations.
	rates, err := OpenStore(ratesPath)
	if err != nil {
		return p, err
	}
	rates.WaitForAsyncMigrations()
	type credit struct {
		edge  edgeRow
		count int
	}
	var rebuilt []credit
	err = func() error {
		defer rates.Close()
		if err := rates.db.QueryRow(`SELECT COUNT(*) FROM neighbor_edges`).Scan(&p.StoredEdges); err != nil {
			return err
		}
		// The window is the number of UTC days that have observations, so
		// a few stray old rows add a day each instead of diluting the rate,
		// and days without traffic (downtime) do not count.
		var days int
		if err := rates.db.QueryRow(`SELECT COUNT(DISTINCT timestamp / 86400) FROM observations WHERE direction IS NOT 'tx'`).Scan(&days); err != nil {
			return err
		}
		if days == 0 {
			return fmt.Errorf("no observations to derive edge rates from")
		}
		p.WindowDays = float64(days)
		if _, err := rates.db.Exec(`DELETE FROM neighbor_edges`); err != nil {
			return err
		}
		for {
			b, err := rates.buildNeighborEdges()
			if err != nil {
				return err
			}
			if b.caughtUp() || b.edges == 0 {
				break
			}
		}
		rows, err := rates.db.Query(`SELECT node_a, node_b, count, last_seen FROM neighbor_edges ORDER BY node_a, node_b`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c credit
			if err := rows.Scan(&c.edge.a, &c.edge.b, &c.count, &c.edge.ts); err != nil {
				return err
			}
			rebuilt = append(rebuilt, c)
		}
		return rows.Err()
	}()
	if err != nil {
		return p, fmt.Errorf("rebuild rates: %w", err)
	}
	p.Rebuilt = len(rebuilt)

	lifetime, err := OpenStore(p.LifetimePath)
	if err != nil {
		return p, err
	}
	defer lifetime.Close()
	decayed, err := OpenStore(p.DecayedPath)
	if err != nil {
		return p, err
	}
	defer decayed.Close()
	lifetime.WaitForAsyncMigrations()
	decayed.WaitForAsyncMigrations()

	// Simulated days end now, so the decay clock in decayed.db is current.
	start := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	if _, err := lifetime.DecayNeighborEdgeCounts(start, 0); err != nil {
		return p, err
	}
	if _, err := decayed.DecayNeighborEdgeCounts(start, halfLifeDays); err != nil {
		return p, err
	}
	for d := 1; d <= days; d++ {
		// Spread each edge's rate evenly: floor(d*r) - floor((d-1)*r) credits on day d.
		var credits []edgeRow
		for _, c := range rebuilt {
			rate := float64(c.count) / p.WindowDays
			for i := int(math.Floor(float64(d)*rate)) - int(math.Floor(float64(d-1)*rate)); i > 0; i-- {
				credits = append(credits, c.edge)
			}
		}
		p.Credits += int64(len(credits))
		for _, s := range []*Store{lifetime, decayed} {
			if len(credits) > 0 {
				if err := s.persistNeighborEdges(credits); err != nil {
					return p, err
				}
			}
		}
		if _, err := decayed.DecayNeighborEdgeCounts(start.Add(time.Duration(d)*24*time.Hour), halfLifeDays); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (p decayReplayPrep) String() string {
	return fmt.Sprintf("%d stored edges; rebuild over %.0f days with observations found %d edges; %d days of credits (%d in total) at a %d-day half-life -> %s, %s",
		p.StoredEdges, p.WindowDays, p.Rebuilt, p.Days, p.Credits, p.HalfLifeDays, p.LifetimePath, p.DecayedPath)
}

func decayReplayEnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", name, v)
	}
	return n
}

// TestNeighborEdgeDecayReplay_Prepare is the preparation step of the
// replay. It is skipped unless CORESCOPE_DECAY_REPLAY_SRC (a copy of a
// database) and CORESCOPE_DECAY_REPLAY_OUT (an empty directory) are set.
// CORESCOPE_DECAY_REPLAY_DAYS (default 30) and
// CORESCOPE_DECAY_REPLAY_HALF_LIFE (default 7) set the simulation. See
// scripts/neighbor-edge-decay-replay.sh.
func TestNeighborEdgeDecayReplay_Prepare(t *testing.T) {
	src, out := os.Getenv("CORESCOPE_DECAY_REPLAY_SRC"), os.Getenv("CORESCOPE_DECAY_REPLAY_OUT")
	if src == "" || out == "" {
		t.Skip("set CORESCOPE_DECAY_REPLAY_SRC and CORESCOPE_DECAY_REPLAY_OUT (see scripts/neighbor-edge-decay-replay.sh)")
	}
	days := decayReplayEnvInt(t, "CORESCOPE_DECAY_REPLAY_DAYS", 30)
	halfLife := decayReplayEnvInt(t, "CORESCOPE_DECAY_REPLAY_HALF_LIFE", 7)
	p, err := prepareNeighborEdgeDecayReplay(src, out, days, halfLife)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(p)
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// On a copy of the e2e fixture the preparation leaves the source
// byte-identical and writes two databases with the same edges and
// last_seen, whose counts differ only by decay: the lifetime counts are
// the stored counts plus every credit, and no decayed count is higher.
func TestNeighborEdgeDecayReplay_PrepareOnFixtureCopy(t *testing.T) {
	fixture := filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("e2e fixture not available: %v", err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	if err := copyDBReadOnly(fixture, src); err != nil {
		t.Fatal(err)
	}
	srcSum := sumNeighborEdgeCounts(t, src)
	srcHash := fileSHA256(t, src)
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := prepareNeighborEdgeDecayReplay(src, out, 30, 7)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(p)
	if got := fileSHA256(t, src); got != srcHash {
		t.Fatal("the source database changed")
	}
	if p.Credits == 0 || p.Rebuilt == 0 {
		t.Fatalf("no credits replayed (%s); the check would be vacuous", p)
	}
	if _, err := prepareNeighborEdgeDecayReplay(src, out, 30, 7); err == nil {
		t.Error("a second run overwrote the outputs")
	}
	lifetime, decayed := readReplayEdges(t, p.LifetimePath), readReplayEdges(t, p.DecayedPath)
	if len(lifetime) != len(decayed) || len(lifetime) < p.StoredEdges {
		t.Fatalf("lifetime %d edges, decayed %d, stored %d", len(lifetime), len(decayed), p.StoredEdges)
	}
	lower := 0
	for k, l := range lifetime {
		d, ok := decayed[k]
		if !ok || d.lastSeen != l.lastSeen {
			t.Fatalf("edge %s: lifetime %+v, decayed %+v", k, l, d)
		}
		if d.count > l.count {
			t.Errorf("edge %s: decayed count %d above lifetime %d", k, d.count, l.count)
		}
		if d.count < l.count {
			lower++
		}
	}
	if lower == 0 {
		t.Error("decay lowered no count")
	}
	if got := sumNeighborEdgeCounts(t, p.LifetimePath); got != srcSum+p.Credits {
		t.Errorf("lifetime count sum %d, want stored %d + credits %d", got, srcSum, p.Credits)
	}
}

type replayEdge struct {
	count    int64
	lastSeen string
}

func readReplayEdges(t *testing.T, path string) map[string]replayEdge {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT node_a, node_b, COALESCE(count, 0), COALESCE(last_seen, '') FROM neighbor_edges`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]replayEdge{}
	for rows.Next() {
		var a, b string
		var e replayEdge
		if err := rows.Scan(&a, &b, &e.count, &e.lastSeen); err != nil {
			t.Fatal(err)
		}
		out[a+"--"+b] = e
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sumNeighborEdgeCounts(t *testing.T, path string) int64 {
	t.Helper()
	var sum int64
	for _, e := range readReplayEdges(t, path) {
		sum += e.count
	}
	return sum
}
