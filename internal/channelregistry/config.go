package channelregistry

import "time"

// Defaults for the limits. Every limit is bounded so the queue, the table and
// the per-request status files cannot grow without bound.
const (
	DefaultMaxPending         = 100
	DefaultMaxApproved        = 128
	DefaultMaxQueuedRequests  = 256
	DefaultRetentionDays      = 30
	DefaultSubmissionsPerHour = 20
	DefaultRequestTTL         = 24 * time.Hour

	// maxResultFactor bounds the number of kept result files relative to the
	// queue size, so a burst of finished requests cannot fill the directory.
	maxResultFactor = 4
)

// Config is the "channelProposals" block in config.json. The server and the
// ingestor read the same block. Zero or negative values fall back to the
// defaults.
type Config struct {
	// Enabled opens public submissions. It only takes effect when a strong
	// admin apiKey is configured too. Approved channels stay active and
	// visible when this is later turned off.
	Enabled            *bool `json:"enabled,omitempty"`
	MaxPending         int   `json:"maxPending,omitempty"`
	MaxApproved        int   `json:"maxApproved,omitempty"`
	MaxQueuedRequests  int   `json:"maxQueuedRequests,omitempty"`
	RetentionDays      int   `json:"retentionDays,omitempty"`
	SubmissionsPerHour int   `json:"submissionsPerHour,omitempty"`
}

// Limits are the resolved limits, with defaults applied.
type Limits struct {
	MaxPending         int
	MaxApproved        int
	MaxQueuedRequests  int
	MaxResults         int
	RetentionDays      int
	SubmissionsPerHour int
	RequestTTL         time.Duration
}

// SubmissionsRequested reports whether the operator switched submissions on.
// Callers must additionally require a strong admin API key.
func (c *Config) SubmissionsRequested() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

// Limits resolves the configured limits. Safe on a nil *Config.
func (c *Config) Limits() Limits {
	var in Config
	if c != nil {
		in = *c
	}
	l := Limits{
		MaxPending:         orDefault(in.MaxPending, DefaultMaxPending),
		MaxApproved:        orDefault(in.MaxApproved, DefaultMaxApproved),
		MaxQueuedRequests:  orDefault(in.MaxQueuedRequests, DefaultMaxQueuedRequests),
		RetentionDays:      orDefault(in.RetentionDays, DefaultRetentionDays),
		SubmissionsPerHour: orDefault(in.SubmissionsPerHour, DefaultSubmissionsPerHour),
		RequestTTL:         DefaultRequestTTL,
	}
	l.MaxResults = l.MaxQueuedRequests * maxResultFactor
	return l
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
