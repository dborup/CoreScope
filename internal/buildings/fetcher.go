package buildings

import (
	"context"
	"encoding/json"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// SegmentSource fetches one segment's footprints: the Client, or a fake.
type SegmentSource interface {
	FetchSegment(ctx context.Context, s Segment) (Page, error)
}

// FetcherOptions are the Fetcher's bounds. They are hard-coded defaults
// for now; config and the customizer come with the server (#730 M2,
// AGENTS.md rule 8).
type FetcherOptions struct {
	CacheEntries int           // pages kept
	CacheBytes   int           // their total encoded size
	CacheTTL     time.Duration // how long a page is reused
	// MaxInFlight bounds the distinct segment calls that exist at once,
	// running or waiting for Concurrency (dhm.Admission); beyond it Fetch
	// refuses with dhm.ErrAdmissionFull, at once and without upstream work.
	MaxInFlight int
	// Concurrency bounds the upstream requests running at once.
	Concurrency int
	// Timeout bounds one shared call, whoever waits on it; 0 is none.
	Timeout time.Duration
	// Now is the cache's clock; nil is time.Now.
	Now func() time.Time
}

// DefaultFetcherOptions: a profile has at most 9 segments, so 16 calls in
// flight serve one profile and part of a second, 4 of them upstream at
// once (#730 research: 8 segments at concurrency 4 ≈ 1 s). The cache
// keeps 256 pages within 16 MiB for a day; GeoDanmark buildings change
// over weeks, and a page is ~0.2 MB on average (1.5 MB per 80 km).
func DefaultFetcherOptions() FetcherOptions {
	return FetcherOptions{
		CacheEntries: 256,
		CacheBytes:   16 << 20,
		CacheTTL:     24 * time.Hour,
		MaxInFlight:  16,
		Concurrency:  4,
		Timeout:      5 * time.Second,
		Now:          time.Now,
	}
}

// Fetcher fetches segments through a bounded cache keyed by segment (a
// path and its reverse share entries), coalescing identical concurrent
// calls (dhm.Flight) and bounding the calls in flight (dhm.Admission) and
// those running upstream. It is safe for concurrent use.
type Fetcher struct {
	src     SegmentSource
	cache   *dhm.Cache[segmentKey]
	flight  dhm.Flight[Page]
	admit   *dhm.Admission
	sem     chan struct{}
	timeout time.Duration
	now     func() time.Time
}

// NewFetcher returns a Fetcher over src with o's bounds; a Concurrency
// under 1 is 1, and a nil Now is time.Now.
func NewFetcher(src SegmentSource, o FetcherOptions) *Fetcher {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Fetcher{
		src:     src,
		cache:   dhm.NewCache[segmentKey](o.CacheEntries, o.CacheBytes, o.CacheTTL),
		admit:   dhm.NewAdmission(o.MaxInFlight),
		sem:     make(chan struct{}, max(1, o.Concurrency)),
		timeout: o.Timeout,
		now:     o.Now,
	}
}

// Fetch returns segment s's page: from the cache, by joining a running
// call for it, or by a new upstream call. It returns ErrBadSegment for an
// invalid s, dhm.ErrAdmissionFull when MaxInFlight calls exist, the
// caller's ctx.Err() when ctx ends first, or the source's error (a
// dhm.FetchError from the Client). Failures are not cached. The page may
// be shared with other callers: treat it as read-only.
func (f *Fetcher) Fetch(ctx context.Context, s Segment) (Page, error) {
	if !s.Valid() {
		return Page{}, ErrBadSegment
	}
	key := s.key()
	if data, ok := f.cache.Get(key, f.now()); ok {
		var p Page
		if json.Unmarshal(data, &p) == nil {
			return p, nil
		}
	}
	p, _, err := f.flight.DoAdmitted(ctx, key.String(), f.timeout, f.admit, func(work context.Context) (Page, error) {
		select {
		case f.sem <- struct{}{}:
			defer func() { <-f.sem }()
		case <-work.Done():
			return Page{}, dhm.FetchError{Reason: "busy"}
		}
		p, err := f.src.FetchSegment(work, s)
		if err != nil {
			return Page{}, err
		}
		if enc, err := json.Marshal(p); err == nil {
			f.cache.Put(key, enc, f.now())
		}
		return p, nil
	})
	return p, err
}
