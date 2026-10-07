package channelregistry

// Proposal statuses, as stored in channel_proposals.status.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	// StatusRevoked marks a previously approved channel an administrator has
	// undone. The row is kept (not deleted) so the audit trail and retention
	// rules apply the same way as for rejected rows; see PruneChannelProposals.
	StatusRevoked = "revoked"
)

// ValidStatus reports whether s is a stored proposal status.
func ValidStatus(s string) bool {
	return s == StatusPending || s == StatusApproved || s == StatusRejected || s == StatusRevoked
}

// Request states reported by GET /api/channel-proposals/requests/{id}.
// "queued" means the ingestor has not picked the request up yet; the other
// states are the outcome it wrote back.
const (
	RequestQueued   = "queued"
	RequestPending  = StatusPending
	RequestApproved = StatusApproved
	RequestRejected = StatusRejected
	RequestRevoked  = StatusRevoked
	RequestError    = "error"
)

// Command operations.
const (
	OpSubmit  = "submit"
	OpApprove = "approve"
	OpReject  = "reject"
	// OpRevoke undoes a previous approval, moving an approved channel back to
	// revoked. It reuses Command.ProposalID, same as OpApprove/OpReject.
	OpRevoke = "revoke"
)

// Proposal is one suggested channel. Timestamps are Unix epoch milliseconds.
type Proposal struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"createdAt"`
	ReviewedAt *int64 `json:"reviewedAt,omitempty"`
}

// Command is what the server writes into the queue. For OpSubmit, Name is set
// and the new proposal gets RequestID as its id, which is what makes replaying
// a command after a crash idempotent. For OpApprove/OpReject, ProposalID is set.
type Command struct {
	RequestID  string `json:"requestId"`
	Op         string `json:"op"`
	Name       string `json:"name,omitempty"`
	ProposalID string `json:"proposalId,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
}

// Result is what the ingestor writes back once a command has been applied.
type Result struct {
	RequestID   string    `json:"requestId"`
	Status      string    `json:"status"`
	Proposal    *Proposal `json:"proposal,omitempty"`
	Error       string    `json:"error,omitempty"`
	CompletedAt int64     `json:"completedAt"`
}

// RequestStatus is the public view of a request: the API response body of
// GET /api/channel-proposals/requests/{id}.
type RequestStatus struct {
	Status   string    `json:"status"`
	Proposal *Proposal `json:"proposal,omitempty"`
	Error    string    `json:"error,omitempty"`
	// CompletedAt is when the ingestor wrote the result (Unix ms; 0 while
	// queued). Not part of the API response: the server uses it to tell a
	// result it has not seen yet from one it is merely asked about again.
	CompletedAt int64 `json:"-"`
}
