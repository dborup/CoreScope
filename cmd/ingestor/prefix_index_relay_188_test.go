package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Issue #188 point 2: the prefix index holds relay roles only, with the same
// definition as the server's canAppearInPath (cmd/server/store.go). The
// cases mirror cmd/server/prefix_map_role_test.go TestCanAppearInPath.
func TestIsRelayRole_MatchesServerCanAppearInPath_188(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{"repeater", true},
		{"Repeater", true},
		{"REPEATER", true},
		{"room_server", true},
		{"Room_Server", true},
		{"room", true},
		{"companion", false},
		{"sensor", false},
		{"", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := isRelayRole(tc.role); got != tc.want {
			t.Errorf("isRelayRole(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}

func openRelayTestStore188(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.backfillWg.Wait()
	return store
}

func seedNodes188(t *testing.T, store *Store, nodes ...[2]string) {
	t.Helper()
	for _, n := range nodes {
		var role interface{} = n[1]
		if n[1] == "" {
			role = nil
		}
		if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, ?, ?)`, n[0], n[0][:4], role); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildPrefixIndex_RelayRolesOnly_188(t *testing.T) {
	store := openRelayTestStore188(t)
	const (
		rep  = "aa110000000000000000000000000000000000000000000000000000000000aa"
		comp = "aa220000000000000000000000000000000000000000000000000000000000bb"
		room = "bb330000000000000000000000000000000000000000000000000000000000aa"
		sens = "cc440000000000000000000000000000000000000000000000000000000000aa"
		none = "dd550000000000000000000000000000000000000000000000000000000000aa"
	)
	seedNodes188(t, store, [2]string{rep, "repeater"}, [2]string{comp, "companion"}, [2]string{room, "room"},
		[2]string{sens, "sensor"}, [2]string{none, ""})
	idx, err := buildPrefixIndex(store.db)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx["aa"]; len(got) != 1 || got[0] != rep {
		t.Errorf(`idx["aa"] = %v, want only the repeater (the companion sharing the prefix cannot relay)`, got)
	}
	if got := idx["bb"]; len(got) != 1 || got[0] != room {
		t.Errorf(`idx["bb"] = %v, want the room`, got)
	}
	for _, p := range []string{"aa22", "cc", "dd"} {
		if got := idx[p]; len(got) != 0 {
			t.Errorf("idx[%q] = %v, want empty (companion, sensor and role-less nodes are not relays)", p, got)
		}
	}
}

// A companion is never stored as a relay hop, even when its prefix is
// unique in the node table; a prefix it shares with one repeater now
// resolves to that repeater.
func TestInsertTransmission_CompanionNeverStoredAsRelay_188(t *testing.T) {
	store := openRelayTestStore188(t)
	const (
		rep  = "aa110000000000000000000000000000000000000000000000000000000000aa"
		comp = "aa220000000000000000000000000000000000000000000000000000000000bb"
		lone = "ee660000000000000000000000000000000000000000000000000000000000cc"
	)
	seedNodes188(t, store, [2]string{rep, "repeater"}, [2]string{comp, "companion"}, [2]string{lone, "companion"})
	if err := store.UpsertObserver("obs-188", "observer", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshPrefixIndex(); err != nil {
		t.Fatal(err)
	}
	insert := func(hash, path string) sql.NullString {
		t.Helper()
		pkt := &PacketData{RawHex: "c0ffee", Timestamp: "2026-06-01T00:00:00Z", ObserverID: "obs-188", Hash: hash,
			RouteType: 1, PayloadType: 5, PathJSON: path, DecodedJSON: "{}"}
		if _, err := store.InsertTransmission(pkt); err != nil {
			t.Fatal(err)
		}
		var rp sql.NullString
		if err := store.db.QueryRow(`SELECT resolved_path FROM observations WHERE transmission_id = (SELECT id FROM transmissions WHERE hash = ?)`, hash).Scan(&rp); err != nil {
			t.Fatal(err)
		}
		return rp
	}
	if rp := insert("h-188-lone", `["ee"]`); rp.Valid {
		t.Errorf("a hop whose only match is a companion was stored as %s, want NULL", rp.String)
	}
	rp := insert("h-188-shared", `["aa"]`)
	if !rp.Valid {
		t.Fatal(`hop "aa" (one repeater + one companion) stayed NULL, want the repeater`)
	}
	if got := unmarshalResolvedPathLocal(rp.String); len(got) != 1 || got[0] == nil || *got[0] != rep {
		t.Errorf("resolved_path = %s, want [%s]", rp.String, rep)
	}
}
