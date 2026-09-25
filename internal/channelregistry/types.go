package channelregistry

// Proposal statuses, as stored in channel_proposals.status.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// ValidStatus reports whether s is a stored proposal status.
func ValidStatus(s string) bool {
	return s == StatusPending || s == StatusApproved || s == StatusRejected
}

// Request states reported by GET /api/channel-proposals/requests/{id}.
// "queued" means the ingestor has not picked the request up yet; the other
// states are the outcome it wrote back.
const (
	RequestQueued   = "queued"
	RequestPending  = StatusPending
	RequestApproved = StatusApproved
	RequestRejected = StatusRejected
	RequestError    = "error"
)

// Command operations.
const (
	OpSubmit  = "submit"
	OpApprove = "approve"
	OpReject  = "reject"
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
}
