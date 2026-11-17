package dbschema

import "testing"

// TestNodeAntennaClaimsTable789 checks the #789 M2a table: idempotent, the
// status/approved_by/height CHECKs, the clear form (NULL height) and the
// lookup indexes. The server never writes it (#1283).
func TestNodeAntennaClaimsTable789(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	for i := 0; i < 2; i++ {
		if err := Apply(db, nil); err != nil {
			t.Fatalf("Apply #%d: %v", i+1, err)
		}
	}
	ins := func(id string, h interface{}, status, by string) error {
		var approvedBy interface{}
		if by != "" {
			approvedBy = by
		}
		_, err := db.Exec(`INSERT INTO node_antenna_claims
			(request_id, pubkey, height_m, code, status, created_at, expires_at, approved_by)
			VALUES (?, 'ab', ?, '7KX9QM', ?, 1, 2, ?)`, id, h, status, approvedBy)
		return err
	}
	for _, st := range []string{"pending", "proven", "approved", "rejected", "revoked", "expired", "superseded"} {
		if err := ins("id-"+st, 12.5, st, ""); err != nil {
			t.Fatalf("status %s: %v", st, err)
		}
	}
	if err := ins("clear", nil, "approved", "owner"); err != nil {
		t.Fatalf("clear claim (NULL height, owner): %v", err)
	}
	for name, err := range map[string]error{
		"bad status":      ins("x1", 1, "bogus", ""),
		"bad approved_by": ins("x2", 1, "approved", "someone"),
		"negative height": ins("x3", -1, "pending", ""),
		"height over cap": ins("x4", 100.5, "pending", ""),
		"duplicate id":    ins("clear", 1, "pending", ""),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, idx := range []string{"idx_node_antenna_claims_pubkey_status", "idx_node_antenna_claims_status"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s missing (%d, %v)", idx, n, err)
		}
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE node_antenna_claims`); err != nil {
		t.Fatal(err)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady must not require node_antenna_claims: %v", err)
	}
}
