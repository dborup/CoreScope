package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// #898 M1 — station details on the combined claim, the writer side: per-part
// ops, all-or-nothing approval, replacement of only the carried parts, and
// the contacts' lifecycle. Adverts are signed with the TEST key of
// antenna_claims_test.go; no real node key is used.

const (
	adminContact898  = "ZZADMIN9"
	publicContact898 = "ZZPUBLIC7"
)

func details898() *antennaclaim.DetailsInput {
	g := 5.8
	return &antennaclaim.DetailsInput{
		StationDetails: antennaclaim.StationDetails{AntennaType: "omni", GainDBi: &g, Power: []string{"battery", "solar"},
			Installation: "mast", ContactPublic: publicContact898},
		ContactAdmin: adminContact898,
	}
}

// claim898 queues a claim for the rig's key and applies it; mod sets its parts.
func (r *claimRig) claim898(id, code string, mod func(*antennaclaim.Command)) antennaclaim.Result {
	r.t.Helper()
	c := antennaclaim.Command{RequestID: id, Op: antennaclaim.OpClaim, PubKey: r.pk, Code: code}
	mod(&c)
	r.enqueue(c)
	r.runner.RunOnce(context.Background())
	return r.result(id)
}

// prove898 sends an advert carrying code, then moves the clock on a minute
// so the next proof is newer.
func (r *claimRig) prove898(code string, withPos bool) {
	r.t.Helper()
	r.advert(r.pub, r.priv, "Roof "+code, withPos)
	r.clock = r.clock.Add(time.Minute)
}

func (r *claimRig) decide898(op, claimID string) antennaclaim.Result {
	r.t.Helper()
	r.ts++
	return r.decide(fmt.Sprintf("e%015x", r.ts), op, claimID)
}

type parts898 struct {
	status          string
	hIn, dIn        bool
	approvedBy      string
	adminC, publicC bool
}

func (r *claimRig) parts898(id string) parts898 {
	r.t.Helper()
	var p parts898
	var by sql.NullString
	err := r.store.db.QueryRow(`SELECT c.status, c.height_in_force, c.details_in_force, c.approved_by,
		ac.request_id IS NOT NULL, c.contact_public IS NOT NULL
		FROM node_antenna_claims c LEFT JOIN node_claim_admin_contacts ac ON ac.request_id = c.request_id
		WHERE c.request_id = ?`, id).Scan(&p.status, &p.hIn, &p.dIn, &by, &p.adminC, &p.publicC)
	if err != nil {
		r.t.Fatalf("parts %s: %v", id, err)
	}
	p.approvedBy = by.String
	return p
}

func setHeight(h float64) func(*antennaclaim.Command) {
	return func(c *antennaclaim.Command) { c.HeightM = &h }
}

func setDetails(c *antennaclaim.Command) { c.Details = details898() }

// T2: one advert proves a claim with a height and details; it waits for an
// administrator even with autoApproveProven (N3, mutant M15), and approval
// puts both parts in force (T7, mutant M8).
func TestAntennaClaim898_CombinedProof(t *testing.T) {
	r := newClaimRig(t, &antennaclaim.Config{AutoApproveProven: true})
	if res := r.claim898(claimA, claimCode, func(c *antennaclaim.Command) { setHeight(12)(c); setDetails(c) }); res.Status != antennaclaim.StatusPending {
		t.Fatalf("claim: %+v", res)
	}
	r.prove898(claimCode, true)
	if p := r.parts898(claimA); p.status != antennaclaim.StatusProven || p.hIn || p.dIn {
		t.Fatalf("combined claim after proof: %+v, want proven, nothing in force", p)
	}
	if res := r.decide898(antennaclaim.OpApprove, claimA); res.Status != antennaclaim.StatusApproved {
		t.Fatalf("approve: %+v", res)
	}
	if p := r.parts898(claimA); !p.hIn || !p.dIn || p.approvedBy != antennaclaim.ApprovedByAdmin || !p.adminC || !p.publicC {
		t.Fatalf("approved combined claim: %+v, want both parts in force and both contacts kept", p)
	}
	var typ, inst string
	var gain float64
	var mask int
	if err := r.store.db.QueryRow(`SELECT antenna_type, gain_dbi, power_mask, installation FROM node_antenna_claims WHERE request_id = ?`, claimA).
		Scan(&typ, &gain, &mask, &inst); err != nil || typ != "omni" || gain != 5.8 || mask != 6 || inst != "mast" {
		t.Fatalf("stored details %s %v %d %s (%v)", typ, gain, mask, inst, err)
	}
}

// T3: clear is explicit. Over every part combination, only the clear-only
// claims are approved at proof (by the owner) and need no position; a claim
// that sets details waits even with autoApproveProven, though its height_m
// is NULL (mutants M1, M2, M15).
func TestAntennaClaim898_ClearIsExplicit(t *testing.T) {
	type combo struct{ h, d string }
	want := map[combo]struct {
		noPos      string // status after an advert without a position
		withPos    string // status after an advert with one
		approvedBy string
	}{
		{"set", ""}:        {antennaclaim.StatusPending, antennaclaim.StatusApproved, antennaclaim.ApprovedByAuto},
		{"clear", ""}:      {antennaclaim.StatusApproved, "", antennaclaim.ApprovedByOwner},
		{"", "set"}:        {antennaclaim.StatusPending, antennaclaim.StatusProven, ""},
		{"", "clear"}:      {antennaclaim.StatusApproved, "", antennaclaim.ApprovedByOwner},
		{"set", "set"}:     {antennaclaim.StatusPending, antennaclaim.StatusProven, ""},
		{"set", "clear"}:   {antennaclaim.StatusPending, antennaclaim.StatusApproved, antennaclaim.ApprovedByAuto},
		{"clear", "set"}:   {antennaclaim.StatusPending, antennaclaim.StatusProven, ""},
		{"clear", "clear"}: {antennaclaim.StatusApproved, "", antennaclaim.ApprovedByOwner},
	}
	for c, w := range want {
		t.Run(c.h+"/"+c.d, func(t *testing.T) {
			r := newClaimRig(t, &antennaclaim.Config{AutoApproveProven: true})
			r.claim898(claimA, claimCode, func(cmd *antennaclaim.Command) {
				switch c.h {
				case "set":
					setHeight(12)(cmd)
				case "clear":
					cmd.Clear = true
				}
				switch c.d {
				case "set":
					setDetails(cmd)
				case "clear":
					cmd.ClearDetails = true
				}
			})
			r.prove898(claimCode, false)
			p := r.parts898(claimA)
			if p.status != w.noPos {
				t.Fatalf("without a position: %s, want %s", p.status, w.noPos)
			}
			if w.withPos != "" {
				r.prove898(claimCode, true)
				p = r.parts898(claimA)
				if p.status != w.withPos {
					t.Fatalf("with a position: %s, want %s", p.status, w.withPos)
				}
			}
			if p.approvedBy != w.approvedBy {
				t.Fatalf("approved_by %q, want %q", p.approvedBy, w.approvedBy)
			}
		})
	}
}

// T3 (source scan): no Go file of the server, the ingestor or antennaclaim
// derives a clear from a NULL height; only the dbschema backfill may.
func TestAntennaClaim898_NoNullHeightAsClear(t *testing.T) {
	pat := regexp.MustCompile(`(?i)height_m\s+IS\s+NULL`)
	for _, dir := range []string{".", "../server", "../../internal/channelregistry/antennaclaim"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("glob %s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if loc := pat.FindIndex(b); loc != nil {
				t.Errorf("%s:%d derives a clear from height_m IS NULL", f, 1+bytes.Count(b[:loc[0]], []byte("\n")))
			}
		}
	}
}

// T4 + T5: a new claim replaces only the parts it carries, and a revoke
// brings nothing back (mutants M3, M4).
func TestAntennaClaim898_ReplaceOnlyCarriedParts(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim898(claimA, "AAAAAA", func(c *antennaclaim.Command) { setHeight(10)(c); setDetails(c) })
	r.prove898("AAAAAA", true)
	r.decide898(antennaclaim.OpApprove, claimA)

	// B = details Y: A keeps its height, loses its details and contacts.
	r.claim898(claimB, "BBBBBB", func(c *antennaclaim.Command) {
		c.Details = &antennaclaim.DetailsInput{StationDetails: antennaclaim.StationDetails{AntennaType: "yagi"}}
	})
	r.prove898("BBBBBB", true)
	r.decide898(antennaclaim.OpApprove, claimB)
	if p := r.parts898(claimA); p.status != antennaclaim.StatusApproved || !p.hIn || p.dIn || p.adminC || p.publicC {
		t.Fatalf("A after B: %+v, want approved, height only, no contacts", p)
	}
	if p := r.parts898(claimB); !p.dIn || p.hIn {
		t.Fatalf("B: %+v", p)
	}

	// C = clear the height: A has nothing left in force and is superseded;
	// B's details stay.
	r.claim898(claimC, "CCCCCC", func(c *antennaclaim.Command) { c.Clear = true })
	r.prove898("CCCCCC", false)
	if p := r.parts898(claimA); p.status != antennaclaim.StatusSuperseded || p.hIn || p.dIn {
		t.Fatalf("A after C: %+v, want superseded", p)
	}
	if p := r.parts898(claimB); p.status != antennaclaim.StatusApproved || !p.dIn {
		t.Fatalf("B after C: %+v", p)
	}
	if p := r.parts898(claimC); p.status != antennaclaim.StatusApproved || !p.hIn || p.dIn {
		t.Fatalf("C: %+v", p)
	}

	// T5: revoking B leaves no details in force anywhere (not A's X).
	r.decide898(antennaclaim.OpRevoke, claimB)
	var inForce int
	r.store.db.QueryRow(`SELECT COUNT(*) FROM node_antenna_claims WHERE status = 'approved' AND details_in_force = 1`).Scan(&inForce)
	if inForce != 0 {
		t.Fatalf("%d details part(s) in force after revoking B, want none", inForce)
	}
	if p := r.parts898(claimC); p.status != antennaclaim.StatusApproved || !p.hIn {
		t.Fatalf("height clear after revoking B: %+v", p)
	}
}

// T6: the newer-approved refusal is per part (mutant M7).
func TestAntennaClaim898_PerPartNewerApproved(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim898(claimA, "AAAAAA", setHeight(10)) // older height proof
	r.prove898("AAAAAA", true)
	r.claim898(claimB, "BBBBBB", setDetails) // newer details proof, approved first
	r.prove898("BBBBBB", true)
	r.decide898(antennaclaim.OpApprove, claimB)
	if res := r.decide898(antennaclaim.OpApprove, claimA); res.Status != antennaclaim.StatusApproved {
		t.Fatalf("older height with no overlapping newer claim refused: %+v", res)
	}
	if p := r.parts898(claimB); p.status != antennaclaim.StatusApproved || !p.dIn || !p.adminC {
		t.Fatalf("the height approval touched the details: %+v", p)
	}

	r.claim898(claimC, "CCCCCC", setDetails) // older details proof
	r.prove898("CCCCCC", true)
	r.claim898(claimD, "DDDDDD", func(c *antennaclaim.Command) { setHeight(20)(c); c.ClearDetails = true })
	r.prove898("DDDDDD", true)
	r.decide898(antennaclaim.OpApprove, claimD)
	res := r.decide898(antennaclaim.OpApprove, claimC)
	if res.Status != antennaclaim.RequestError || res.Error != antennaclaim.ErrNewerApproved.Error() {
		t.Fatalf("older details over a newer details clear: %+v", res)
	}
}

// contactsOf reports which contacts of claim id still exist.
func (r *claimRig) contactsOf(id string) (admin, public bool) {
	p := r.parts898(id)
	return p.adminC, p.publicC
}

// T8: the contacts' lifecycle, each field checked on its own (mutants M9,
// M10). Present while pending, proven and approved; gone after every other
// transition; kept when only the height is replaced.
func TestAntennaClaim898_ContactLifecycle(t *testing.T) {
	both := func(t *testing.T, r *claimRig, id, when string, want bool) {
		t.Helper()
		a, p := r.contactsOf(id)
		if a != want || p != want {
			t.Fatalf("%s: admin contact %v, public contact %v, want %v", when, a, p, want)
		}
	}
	withDetails := func(t *testing.T, cfg *antennaclaim.Config) *claimRig {
		r := newClaimRig(t, cfg)
		r.claim898(claimA, "AAAAAA", setDetails)
		both(t, r, claimA, "pending", true)
		return r
	}
	t.Run("reject pending", func(t *testing.T) {
		r := withDetails(t, nil)
		r.decide898(antennaclaim.OpReject, claimA)
		both(t, r, claimA, "rejected while pending", false)
	})
	t.Run("reject proven", func(t *testing.T) {
		r := withDetails(t, nil)
		r.prove898("AAAAAA", true)
		both(t, r, claimA, "proven", true)
		r.decide898(antennaclaim.OpReject, claimA)
		both(t, r, claimA, "rejected while proven", false)
	})
	t.Run("revoke", func(t *testing.T) {
		r := withDetails(t, nil)
		r.prove898("AAAAAA", true)
		r.decide898(antennaclaim.OpApprove, claimA)
		both(t, r, claimA, "approved", true)
		r.decide898(antennaclaim.OpRevoke, claimA)
		both(t, r, claimA, "revoked", false)
		if p := r.parts898(claimA); p.hIn || p.dIn {
			t.Fatalf("revoked claim still in force: %+v", p)
		}
	})
	t.Run("expire sweep", func(t *testing.T) {
		r := withDetails(t, nil)
		r.clock = r.clock.Add(25 * time.Hour)
		r.runner.RunOnce(context.Background())
		if p := r.parts898(claimA); p.status != antennaclaim.StatusExpired {
			t.Fatalf("not expired: %+v", p)
		}
		both(t, r, claimA, "expired", false)
	})
	t.Run("supersede by details set", func(t *testing.T) {
		r := withDetails(t, nil)
		r.prove898("AAAAAA", true)
		r.decide898(antennaclaim.OpApprove, claimA)
		r.claim898(claimB, "BBBBBB", func(c *antennaclaim.Command) {
			c.Details = &antennaclaim.DetailsInput{StationDetails: antennaclaim.StationDetails{Installation: "roof"}}
		})
		r.prove898("BBBBBB", true)
		r.decide898(antennaclaim.OpApprove, claimB)
		both(t, r, claimA, "superseded by details", false)
	})
	t.Run("supersede by clearDetails", func(t *testing.T) {
		r := withDetails(t, nil)
		r.prove898("AAAAAA", true)
		r.decide898(antennaclaim.OpApprove, claimA)
		r.claim898(claimB, "BBBBBB", func(c *antennaclaim.Command) { c.ClearDetails = true })
		r.prove898("BBBBBB", false)
		if p := r.parts898(claimB); p.status != antennaclaim.StatusApproved || p.approvedBy != antennaclaim.ApprovedByOwner {
			t.Fatalf("clearDetails: %+v", p)
		}
		both(t, r, claimA, "superseded by clearDetails", false)
	})
	t.Run("kept after a height-only supersede", func(t *testing.T) {
		r := newClaimRig(t, nil)
		r.claim898(claimA, "AAAAAA", func(c *antennaclaim.Command) { setHeight(10)(c); setDetails(c) })
		r.prove898("AAAAAA", true)
		r.decide898(antennaclaim.OpApprove, claimA)
		r.claim898(claimB, "BBBBBB", setHeight(15))
		r.prove898("BBBBBB", true)
		r.decide898(antennaclaim.OpApprove, claimB)
		both(t, r, claimA, "after a height-only supersede", true)
		if p := r.parts898(claimA); p.status != antennaclaim.StatusApproved || p.hIn || !p.dIn {
			t.Fatalf("A: %+v, want approved with only its details in force", p)
		}
	})
	t.Run("retention cascade", func(t *testing.T) {
		r := withDetails(t, &antennaclaim.Config{RetentionDays: 1})
		r.prove898("AAAAAA", true) // proven never expires; retention removes it
		r.clock = r.clock.Add(3 * 24 * time.Hour)
		r.runner.Prune(context.Background())
		var claims, contacts int
		r.store.db.QueryRow(`SELECT COUNT(*) FROM node_antenna_claims`).Scan(&claims)
		r.store.db.QueryRow(`SELECT COUNT(*) FROM node_claim_admin_contacts`).Scan(&contacts)
		if claims != 0 || contacts != 0 {
			t.Fatalf("after retention: %d claim(s), %d admin contact(s), want none", claims, contacts)
		}
	})
	t.Run("orphan sweep", func(t *testing.T) {
		r := withDetails(t, nil)
		r.decide898(antennaclaim.OpReject, claimA)
		// Plant what a missed delete would leave: both contacts on a
		// rejected claim, still inside the retention window.
		if _, err := r.store.db.Exec(`UPDATE node_antenna_claims SET contact_public = 'orphan' WHERE request_id = ?`, claimA); err != nil {
			t.Fatal(err)
		}
		if _, err := r.store.db.Exec(`INSERT INTO node_claim_admin_contacts (request_id, contact) VALUES (?, 'orphan')`, claimA); err != nil {
			t.Fatal(err)
		}
		both(t, r, claimA, "planted orphans", true)
		r.runner.Prune(context.Background())
		both(t, r, claimA, "after the sweep", false)
		if p := r.parts898(claimA); p.status != antennaclaim.StatusRejected {
			t.Fatalf("the sweep removed the claim itself: %+v", p)
		}
	})
}

// T10 (ingestor): hand-written queue files are validated again.
func TestAntennaClaim898_QueueFileRevalidated(t *testing.T) {
	h := 12.0
	for name, mod := range map[string]func(*antennaclaim.Command){
		"details and clearDetails": func(c *antennaclaim.Command) { setDetails(c); c.ClearDetails = true },
		"empty details":            func(c *antennaclaim.Command) { c.Details = &antennaclaim.DetailsInput{} },
		"bidi admin contact": func(c *antennaclaim.Command) {
			c.Details = &antennaclaim.DetailsInput{ContactAdmin: "a\u202Eb"}
		},
		"gain off the grid": func(c *antennaclaim.Command) {
			g := 5.85
			c.Details = &antennaclaim.DetailsInput{StationDetails: antennaclaim.StationDetails{GainDBi: &g}}
		},
		"height and clear with details": func(c *antennaclaim.Command) { c.HeightM, c.Clear = &h, true; setDetails(c) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newClaimRig(t, nil)
			res := r.claim898(claimA, claimCode, mod)
			if res.Status != antennaclaim.RequestError || res.Error != antennaclaim.ErrInvalidCommand.Error() {
				t.Fatalf("accepted: %+v", res)
			}
			var n int
			r.store.db.QueryRow(`SELECT COUNT(*) FROM node_antenna_claims`).Scan(&n)
			if n != 0 {
				t.Fatal("row inserted")
			}
		})
	}
}

// T13: no log line, queue command or result file carries a contact.
func TestAntennaClaim898_ContactsNeverLoggedOrQueued(t *testing.T) {
	var buf bytes.Buffer
	prev, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	defer func() { log.SetOutput(prev); log.SetFlags(prevFlags) }()

	r := newClaimRig(t, &antennaclaim.Config{RetentionDays: 1})
	r.claim898(claimA, "AAAAAA", setDetails)
	r.prove898("AAAAAA", true)
	r.decide898(antennaclaim.OpApprove, claimA)
	r.claim898(claimB, "BBBBBB", setDetails)
	r.decide898(antennaclaim.OpReject, claimB)
	r.claim898(claimC, "CCCCCC", setDetails)
	r.prove898("CCCCCC", true)
	r.decide898(antennaclaim.OpApprove, claimC) // supersedes A's details
	r.decide898(antennaclaim.OpRevoke, claimC)
	r.claim898(claimD, "DDDDDD", setDetails)
	r.clock = r.clock.Add(3 * 24 * time.Hour)
	r.runner.RunOnce(context.Background()) // expires D
	r.runner.Prune(context.Background())

	for _, c := range []string{adminContact898, publicContact898} {
		if strings.Contains(buf.String(), c) {
			t.Errorf("a log line carries %s:\n%s", c, buf.String())
		}
	}
	entries, err := os.ReadDir(r.queue.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cmd-") {
			t.Errorf("command file %s left after it was applied", e.Name())
		}
		b, _ := os.ReadFile(filepath.Join(r.queue.Dir(), e.Name()))
		if bytes.Contains(b, []byte(adminContact898)) || bytes.Contains(b, []byte(publicContact898)) {
			t.Errorf("%s carries a contact: %s", e.Name(), b)
		}
	}
}

// BenchmarkAntennaClaim898_MatchCombined is the ingest hook with 256
// combined claims pending (T14): the same map lookup and token scan as
// #789's, since a combined claim is one pending entry.
func BenchmarkAntennaClaim898_MatchCombined(b *testing.B) {
	r := &antennaClaimRunner{pending: map[string][]pendingClaim{}, now: time.Now, limits: (*antennaclaim.Config)(nil).Limits()}
	exp := time.Now().Add(time.Hour).UnixMilli()
	for i := 0; i < 256; i++ {
		pk := fmt.Sprintf("%064x", i+1)
		r.pending[pk] = []pendingClaim{{id: "a000000000000001", code: "AAAAAA", expiresAt: exp, clearOnly: i%3 == 0}}
	}
	pk := fmt.Sprintf("%064x", 0)
	r.pending[pk] = []pendingClaim{
		{id: "a000000000000001", code: "AAAAAA", expiresAt: exp},
		{id: "b000000000000002", code: "BBBBBB", expiresAt: exp, clearOnly: true},
		{id: "c000000000000003", code: claimCode, expiresAt: exp},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.match(pk, "Roof Repeater Abcd x", 23, true)
	}
}
