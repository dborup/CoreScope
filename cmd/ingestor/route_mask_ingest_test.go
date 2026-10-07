package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/meshcore-analyzer/packetpath"
)

// Issue #89: transmissions.route_mask records every raw route type (0..3)
// observed for a content hash. The first insert sets the bit of its own route;
// every later observation ORs its bit in atomically in SQL. The mask is
// monotonic, so the final value must not depend on insert order, observer,
// redelivery, restarts or concurrency. route_type keeps its legacy
// first-inserted meaning.

var (
	// ROUTE_TYPE_TRANSPORT_FLOOD heard at 0 hops (header 0x10, codes 0000 0000,
	// path_len 0): the variant staging tx 1000443 kept after the observation
	// upsert overwrote a zero-hop frame from the same observer.
	routeMaskFlood0HopRaw = "10" + "00000000" + "00" + firstIngestedAdvertPayload
	routeMaskT0           = "2026-09-24T10:00:00Z"
	routeMaskT5           = "2026-09-24T10:05:00Z"
)

type routeMaskObs struct{ raw, observer, rx string }

func routeMaskStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.backfillWg.Wait()
	for _, id := range []string{"obs-a", "obs-b"} {
		if err := s.UpsertObserver(id, id, "TST", nil); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func routeMaskInsert(t *testing.T, s *Store, o routeMaskObs) string {
	t.Helper()
	decoded, err := DecodePacket(o.raw, nil, false)
	if err != nil {
		t.Fatalf("decode %s: %v", o.raw[:4], err)
	}
	pd := BuildPacketData(&MQTTPacketMessage{Raw: o.raw}, decoded, o.observer, "", nil)
	pd.Timestamp = o.rx
	if _, err := s.InsertTransmission(pd); err != nil {
		t.Fatal(err)
	}
	return pd.Hash
}

func routeMaskOf(t *testing.T, s *Store, hash string) (mask sql.NullInt64, routeType int) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT route_mask, route_type FROM transmissions WHERE hash = ?`, hash).Scan(&mask, &routeType); err != nil {
		t.Fatal(err)
	}
	return mask, routeType
}

func permutations(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := append(append(append([]int{}, p[:i]...), n-1), p[i:]...)
			out = append(out, q)
		}
	}
	return out
}

func TestInsertTransmission_RouteMaskIsOrderIndependent(t *testing.T) {
	flood := routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0}
	zeroHop := routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT5}
	scenarios := []struct {
		name string
		obs  []routeMaskObs
		want int64
	}{
		{"flood and zero-hop, different observers", []routeMaskObs{flood, zeroHop}, 0b1010},
		{"zero-hop with the later rx time", []routeMaskObs{flood, {firstIngestedZeroHopRaw, "obs-b", routeMaskT0}}, 0b1010},
		{"same observer and path: zero-hop and 0-hop transport flood",
			[]routeMaskObs{{firstIngestedZeroHopRaw, "obs-a", routeMaskT0}, {routeMaskFlood0HopRaw, "obs-a", routeMaskT5}}, 0b1001},
		{"redelivery of both variants",
			[]routeMaskObs{flood, flood, zeroHop, zeroHop}, 0b1010},
		{"three route values", []routeMaskObs{flood, zeroHop, {routeMaskFlood0HopRaw, "obs-b", routeMaskT0}}, 0b1011},
		{"single flood only", []routeMaskObs{flood}, 0b0010},
		{"single zero-hop only", []routeMaskObs{zeroHop}, 0b1000},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			for _, perm := range permutations(len(sc.obs)) {
				s := routeMaskStore(t, filepath.Join(t.TempDir(), "rm.db"))
				var hash string
				for _, i := range perm {
					hash = routeMaskInsert(t, s, sc.obs[i])
				}
				mask, routeType := routeMaskOf(t, s, hash)
				first, _ := DecodePacket(sc.obs[perm[0]].raw, nil, false)
				s.Close()
				if !mask.Valid || mask.Int64 != sc.want {
					t.Fatalf("order %v: route_mask = %v, want %04b", perm, mask, sc.want)
				}
				if routeType != first.Header.RouteType {
					t.Fatalf("order %v: route_type = %d, want the first inserted route %d (legacy contract)", perm, routeType, first.Header.RouteType)
				}
			}
		})
	}
}

func TestInsertTransmission_RouteMaskSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	s := routeMaskStore(t, path)
	hash := routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT0})
	s.Close()
	s = routeMaskStore(t, path) // ingestor restart
	routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT5})
	routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT0}) // redelivery
	mask, _ := routeMaskOf(t, s, hash)
	s.Close()
	if !mask.Valid || mask.Int64 != 0b1010 {
		t.Fatalf("route_mask after restart = %v, want 1010", mask)
	}
}

// Both variants of each fresh payload race for writerMu; the mask must be the
// same whichever wins.
func TestInsertTransmission_RouteMaskUnderParallelIngest(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "parallel.db"))
	defer s.Close()
	const n = 200
	hashes := make([]string, n)
	for i := 0; i < n; i++ {
		payload := fmt.Sprintf("%064x", i) + firstIngestedAdvertPayload[64:]
		frames := []routeMaskObs{
			{"11" + "02" + "a1b2" + payload, "obs-a", routeMaskT0},
			{"13" + "00000000" + "00" + payload, "obs-b", routeMaskT0},
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, f := range frames {
			wg.Add(1)
			go func(f routeMaskObs) {
				defer wg.Done()
				<-start
				h := routeMaskInsert(t, s, f)
				mu.Lock()
				hashes[i] = h
				mu.Unlock()
			}(f)
		}
		close(start)
		wg.Wait()
	}
	var wrong int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_mask IS NOT 10`).Scan(&wrong); err != nil {
		t.Fatal(err)
	}
	if wrong != 0 {
		t.Fatalf("%d of %d transmissions ended with a mask other than 1010 under parallel ingest", wrong, n)
	}
}

// A route outside 0..3 sets no bit; the first insert still records a known
// (non-NULL) mask so it is not mistaken for an un-backfilled legacy row.
func TestInsertTransmission_InvalidRouteSetsNoBit(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "invalid.db"))
	defer s.Close()
	decoded, err := DecodePacket(firstIngestedFloodRaw, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	pd := BuildPacketData(&MQTTPacketMessage{Raw: firstIngestedFloodRaw}, decoded, "obs-a", "", nil)
	pd.Timestamp = routeMaskT0
	pd.RouteType = 7
	if _, err := s.InsertTransmission(pd); err != nil {
		t.Fatal(err)
	}
	mask, _ := routeMaskOf(t, s, pd.Hash)
	if !mask.Valid || mask.Int64 != 0 {
		t.Fatalf("route_mask for route 7 = %v, want 0", mask)
	}
	pd.RouteType = -1
	pd.ObserverID = "obs-b"
	if _, err := s.InsertTransmission(pd); err != nil {
		t.Fatal(err)
	}
	if mask, _ = routeMaskOf(t, s, pd.Hash); mask.Int64 != 0 {
		t.Fatalf("route_mask after route -1 = %v, want 0", mask)
	}
}

// PATH (payload 8) seen with and without transport codes keeps both raw bits
// (staging had one such hash in a 7-day census).
func TestInsertTransmission_PathKeepsBothFloodRouteBits(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "path.db"))
	defer s.Close()
	payload := "aabb" + strings.Repeat("cd", 20)
	var hash string
	for _, o := range []routeMaskObs{
		{"21" + "00" + payload, "obs-a", routeMaskT0},              // PATH, ROUTE_TYPE_FLOOD
		{"20" + "11112222" + "00" + payload, "obs-b", routeMaskT5}, // PATH, ROUTE_TYPE_TRANSPORT_FLOOD
	} {
		hash = routeMaskInsert(t, s, o)
	}
	mask, _ := routeMaskOf(t, s, hash)
	if !mask.Valid || mask.Int64 != 0b0011 {
		t.Fatalf("PATH route_mask = %v, want 0011", mask)
	}
}

// Un-backfilled legacy rows (NULL) are left to the backfill: the live path
// only ORs into known masks, and never clears bits of a known one.
func TestInsertTransmission_RouteMaskLeavesLegacyNullAndNeverClears(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "legacy.db"))
	defer s.Close()
	hash := routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0})
	if _, err := s.db.Exec(`UPDATE transmissions SET route_mask = NULL WHERE hash = ?`, hash); err != nil {
		t.Fatal(err)
	}
	routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT5})
	if mask, _ := routeMaskOf(t, s, hash); mask.Valid {
		t.Fatalf("live ingest wrote %v into an un-backfilled row; the backfill owns NULL rows", mask)
	}
	if _, err := s.db.Exec(`UPDATE transmissions SET route_mask = 15 WHERE hash = ?`, hash); err != nil {
		t.Fatal(err)
	}
	routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-b", routeMaskT5})
	if mask, _ := routeMaskOf(t, s, hash); mask.Int64 != 15 {
		t.Fatalf("route_mask = %v after a later observation, want 1111 (bits are never cleared)", mask)
	}
}

// A server poll that sees an observation row must also see its route bit in
// transmissions.route_mask; otherwise the server can store the observation
// with a mask that lacks the bit and never re-read it. A trigger records the
// mask at the moment each observation row is written (insert or upsert).
func TestInsertTransmission_ObservationIsVisibleOnlyWithItsRouteBit(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "visible.db"))
	defer s.Close()
	for _, q := range []string{
		`CREATE TABLE obs_mask_seen (obs_id INTEGER, raw_hex TEXT, mask INTEGER)`,
		`CREATE TRIGGER obs_mask_probe_ins AFTER INSERT ON observations BEGIN
			INSERT INTO obs_mask_seen SELECT NEW.id, NEW.raw_hex, route_mask FROM transmissions WHERE id = NEW.transmission_id; END`,
		`CREATE TRIGGER obs_mask_probe_upd AFTER UPDATE ON observations BEGIN
			INSERT INTO obs_mask_seen SELECT NEW.id, NEW.raw_hex, route_mask FROM transmissions WHERE id = NEW.transmission_id; END`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []routeMaskObs{
		{firstIngestedFloodRaw, "obs-a", routeMaskT0},
		{firstIngestedZeroHopRaw, "obs-b", routeMaskT5},
		{firstIngestedZeroHopRaw, "obs-a", routeMaskT0},
		{routeMaskFlood0HopRaw, "obs-a", routeMaskT5}, // upserts the previous row
	} {
		routeMaskInsert(t, s, o)
	}
	rows, err := s.db.Query(`SELECT obs_id, raw_hex, mask FROM obs_mask_seen ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var id int64
		var raw string
		var mask sql.NullInt64
		if err := rows.Scan(&id, &raw, &mask); err != nil {
			t.Fatal(err)
		}
		n++
		rt, ok := packetpath.RouteTypeFromRawHex(raw)
		if !ok {
			t.Fatalf("obs %d: unparseable raw_hex %q", id, raw)
		}
		if bit := packetpath.RouteMaskBit(rt); !mask.Valid || mask.Int64&bit == 0 {
			t.Fatalf("obs %d (route %d) became visible while route_mask was %v, without its bit", id, rt, mask)
		}
	}
	if n < 4 {
		t.Fatalf("probe saw %d observation writes, want at least 4", n)
	}
}

// Recording a new route bit and writing the observation that carries it
// happen together or not at all. If the route_mask update fails, the
// observation must not become visible: a known mask is never backfilled
// again, so an observation stored without its bit would leave the mask
// permanently short. If the observation write fails after the update, the
// update is rolled back too. Test-only triggers make one of the two writes
// fail deterministically.
func TestInsertTransmission_FailedRouteBitKeepsObservationOut(t *testing.T) {
	type obsRow struct {
		observer string
		raw      string
	}
	observations := func(t *testing.T, s *Store, hash string) []obsRow {
		t.Helper()
		rows, err := s.db.Query(`SELECT obs.id, o.raw_hex FROM observations o
			JOIN transmissions t ON t.id = o.transmission_id
			JOIN observers obs ON obs.rowid = o.observer_idx
			WHERE t.hash = ? ORDER BY obs.id`, hash)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []obsRow
		for rows.Next() {
			var r obsRow
			if err := rows.Scan(&r.observer, &r.raw); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	faults := []struct {
		name     string
		triggers []string
	}{
		{"route_mask update fails", []string{
			`CREATE TRIGGER fault_route_mask BEFORE UPDATE OF route_mask ON transmissions
				BEGIN SELECT RAISE(ABORT, 'injected route_mask failure'); END`,
		}},
		{"observation write fails after the update", []string{
			`CREATE TRIGGER fault_obs_insert BEFORE INSERT ON observations
				BEGIN SELECT RAISE(ABORT, 'injected observation failure'); END`,
			`CREATE TRIGGER fault_obs_update BEFORE UPDATE ON observations
				BEGIN SELECT RAISE(ABORT, 'injected observation failure'); END`,
		}},
		{"change event insert fails after both writes", []string{
			`CREATE TRIGGER fault_change BEFORE INSERT ON route_mask_changes
				BEGIN SELECT RAISE(ABORT, 'injected change event failure'); END`,
		}},
	}
	paths := []struct {
		name  string
		first routeMaskObs
		next  routeMaskObs // brings a new route bit; fails, then succeeds
		want  int64        // mask after the successful retry
	}{
		{"new observation row (insert)",
			routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0},
			routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT5}, 0b1010},
		{"same observer and path (upsert)",
			routeMaskObs{firstIngestedZeroHopRaw, "obs-a", routeMaskT0},
			routeMaskObs{routeMaskFlood0HopRaw, "obs-a", routeMaskT5}, 0b1001},
	}
	for _, fault := range faults {
		for _, tc := range paths {
			t.Run(fault.name+"/"+tc.name, func(t *testing.T) {
				s := routeMaskStore(t, filepath.Join(t.TempDir(), "fail.db"))
				defer s.Close()
				hash := routeMaskInsert(t, s, tc.first)
				wantMask, _ := routeMaskOf(t, s, hash)
				wantObs := observations(t, s, hash)

				for _, q := range fault.triggers {
					if _, err := s.db.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
				writeErrs, inserted := s.Stats.WriteErrors.Load(), s.Stats.ObservationsInserted.Load()
				decoded, err := DecodePacket(tc.next.raw, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				pd := BuildPacketData(&MQTTPacketMessage{Raw: tc.next.raw}, decoded, tc.next.observer, "", nil)
				pd.Timestamp = tc.next.rx
				isNew, err := s.InsertTransmission(pd)
				if err != nil || isNew {
					t.Fatalf("InsertTransmission = (%v, %v), want the non-fatal (false, nil) of a failed observation write", isNew, err)
				}
				if got := s.Stats.WriteErrors.Load() - writeErrs; got != 1 {
					t.Fatalf("WriteErrors grew by %d, want exactly 1", got)
				}
				if got := s.Stats.ObservationsInserted.Load() - inserted; got != 0 {
					t.Fatalf("ObservationsInserted grew by %d, want 0", got)
				}
				if got, _ := routeMaskOf(t, s, hash); got != wantMask {
					t.Fatalf("route_mask = %v after the failed update, want unchanged %v", got, wantMask)
				}
				if got := observations(t, s, hash); !reflect.DeepEqual(got, wantObs) {
					t.Fatalf("observations after the failed update = %v, want unchanged %v", got, wantObs)
				}
				if got := routeMaskChanges(t, s); len(got) != 0 {
					t.Fatalf("change rows after the failed update = %+v, want none", got)
				}

				// Once the failure is gone, a redelivery stores both.
				for _, name := range []string{"fault_route_mask", "fault_obs_insert", "fault_obs_update", "fault_change"} {
					if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
						t.Fatal(err)
					}
				}
				routeMaskInsert(t, s, tc.next)
				if got, _ := routeMaskOf(t, s, hash); !got.Valid || got.Int64 != tc.want {
					t.Fatalf("route_mask after the redelivery = %v, want %04b", got, tc.want)
				}
				found := false
				for _, o := range observations(t, s, hash) {
					found = found || (o.observer == tc.next.observer && strings.EqualFold(o.raw, tc.next.raw))
				}
				if !found {
					t.Fatalf("redelivered observation missing: %v", observations(t, s, hash))
				}
				if got := routeMaskChanges(t, s); len(got) != 1 || got[0].mask != tc.want {
					t.Fatalf("change rows after the redelivery = %+v, want one with mask %04b", got, tc.want)
				}
			})
		}
	}
}
