package main

import (
	"database/sql"
	"testing"

	"github.com/meshcore-analyzer/channelregistry"
)

// #616: the ingestor records who approved a channel in
// channel_proposals.approved_by — 'auto' when the autoApprove policy approved
// a new name, 'admin' for an admin approval — and clears it whenever the row
// leaves the approved status (reject, revoke, resubmission after a revoke).

// approvedBy reads the stored approved_by of proposal id; "" means NULL.
func (f *proposalFixture) approvedBy(id string) string {
	f.t.Helper()
	var by sql.NullString
	if err := f.store.db.QueryRow(`SELECT approved_by FROM channel_proposals WHERE id = ?`, id).Scan(&by); err != nil {
		f.t.Fatalf("approved_by of %s: %v", id, err)
	}
	return by.String
}

func TestChannelProposalApprovedByAutoApprove(t *testing.T) {
	enabled := true
	f := newProposalFixture(t, &channelregistry.Config{Enabled: &enabled, AutoApprove: true})
	st := f.run(f.enqueue(channelregistry.OpSubmit, "#AutoBy"))
	if st.Status != channelregistry.RequestApproved {
		t.Fatalf("auto-approve = %+v", st)
	}
	if got := f.approvedBy(st.Proposal.ID); got != channelregistry.ApprovedByAuto {
		t.Fatalf("auto-approved approved_by = %q, want %q", got, channelregistry.ApprovedByAuto)
	}
	// A replay of the same submission reports the row unchanged.
	if dup := f.run(f.enqueue(channelregistry.OpSubmit, "#AutoBy")); dup.Proposal.ID != st.Proposal.ID {
		t.Fatalf("duplicate = %+v", dup)
	}
	if got := f.approvedBy(st.Proposal.ID); got != channelregistry.ApprovedByAuto {
		t.Fatalf("approved_by after a duplicate submission = %q", got)
	}

	// Revoke clears it; the resubmission lands pending without it, and an
	// admin approval then records 'admin', not the old 'auto'.
	if r := f.run(f.enqueue(channelregistry.OpRevoke, st.Proposal.ID)); r.Status != channelregistry.RequestRevoked {
		t.Fatalf("revoke = %+v", r)
	}
	if got := f.approvedBy(st.Proposal.ID); got != "" {
		t.Fatalf("revoked approved_by = %q, want NULL", got)
	}
	if r := f.run(f.enqueue(channelregistry.OpSubmit, "#AutoBy")); r.Status != channelregistry.RequestPending {
		t.Fatalf("resubmit = %+v", r)
	}
	if got := f.approvedBy(st.Proposal.ID); got != "" {
		t.Fatalf("resubmitted approved_by = %q, want NULL", got)
	}
	if r := f.run(f.enqueue(channelregistry.OpApprove, st.Proposal.ID)); r.Status != channelregistry.RequestApproved {
		t.Fatalf("admin approve = %+v", r)
	}
	if got := f.approvedBy(st.Proposal.ID); got != channelregistry.ApprovedByAdmin {
		t.Fatalf("re-approved approved_by = %q, want %q", got, channelregistry.ApprovedByAdmin)
	}
}

func TestChannelProposalApprovedByAdminDecisions(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#ByAdmin")).Proposal.ID
	if got := f.approvedBy(id); got != "" {
		t.Fatalf("pending approved_by = %q, want NULL", got)
	}
	if st := f.run(f.enqueue(channelregistry.OpApprove, id)); st.Status != channelregistry.RequestApproved {
		t.Fatalf("approve = %+v", st)
	}
	if got := f.approvedBy(id); got != channelregistry.ApprovedByAdmin {
		t.Fatalf("admin-approved approved_by = %q, want %q", got, channelregistry.ApprovedByAdmin)
	}
	// A repeated approval is a no-op and keeps the value.
	if st := f.run(f.enqueue(channelregistry.OpApprove, id)); st.Status != channelregistry.RequestApproved {
		t.Fatalf("repeat approve = %+v", st)
	}
	if got := f.approvedBy(id); got != channelregistry.ApprovedByAdmin {
		t.Fatalf("approved_by after a repeated approval = %q", got)
	}
	if st := f.run(f.enqueue(channelregistry.OpRevoke, id)); st.Status != channelregistry.RequestRevoked {
		t.Fatalf("revoke = %+v", st)
	}
	if got := f.approvedBy(id); got != "" {
		t.Fatalf("revoked approved_by = %q, want NULL", got)
	}

	// Reject clears a value an older ingestor may have left on a pending
	// row (it did not know the column).
	other := f.run(f.enqueue(channelregistry.OpSubmit, "#Rejected")).Proposal.ID
	if _, err := f.store.db.Exec(`UPDATE channel_proposals SET approved_by = 'auto' WHERE id = ?`, other); err != nil {
		t.Fatal(err)
	}
	if st := f.run(f.enqueue(channelregistry.OpReject, other)); st.Status != channelregistry.RequestRejected {
		t.Fatalf("reject = %+v", st)
	}
	if got := f.approvedBy(other); got != "" {
		t.Fatalf("rejected approved_by = %q, want NULL", got)
	}
}

// A revoked row that still carries a stale value (left by an older
// ingestor) loses it when it is suggested again.
func TestChannelProposalApprovedByResubmitClearsStaleValue(t *testing.T) {
	f := newProposalFixture(t, nil)
	id := f.run(f.enqueue(channelregistry.OpSubmit, "#Stale")).Proposal.ID
	f.run(f.enqueue(channelregistry.OpApprove, id))
	f.run(f.enqueue(channelregistry.OpRevoke, id))
	if _, err := f.store.db.Exec(`UPDATE channel_proposals SET approved_by = 'admin' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if st := f.run(f.enqueue(channelregistry.OpSubmit, "#Stale")); st.Status != channelregistry.RequestPending {
		t.Fatalf("resubmit = %+v", st)
	}
	if got := f.approvedBy(id); got != "" {
		t.Fatalf("resubmitted approved_by = %q, want NULL", got)
	}
}
