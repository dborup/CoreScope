package dhm

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// DefaultBaseURL is Dataforsyningen's API root. Services are paths below it.
const DefaultBaseURL = "https://api.dataforsyningen.dk/"

// allowedServices are the only upstream services the Client calls. A
// feature that needs another DHM service (for example the WCS in #681)
// adds it here, next to its own request shape.
var allowedServices = map[string]bool{
	WMSService: true,
}

// tokenHeader is the request header Dataforsyningen reads the token from.
// Its documentation recommends the header over the `token` query parameter
// as the more secure method; it also keeps the token out of every URL, and
// so out of net/http's *url.Error text.
const tokenHeader = "token"

// ClientOptions are the limits of the upstream client.
type ClientOptions struct {
	BaseURL      string        // DefaultBaseURL outside tests
	Timeout      time.Duration // per upstream request, including the body
	Concurrency  int           // upstream requests in flight, all features together
	MaxBodyBytes int           // larger responses are rejected
	Now          func() time.Time
	Logf         func(format string, args ...interface{})
	LogEvery     time.Duration // throttle for failure log lines; 0 logs all
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		BaseURL:      DefaultBaseURL,
		Timeout:      10 * time.Second,
		Concurrency:  4,
		MaxBodyBytes: 1 << 20,
		Now:          time.Now,
		Logf:         log.Printf,
		LogEvery:     time.Minute,
	}
}

// Client fetches from DHM services with the token added server-side.
type Client struct {
	token   string
	opts    ClientOptions
	http    *http.Client
	sem     chan struct{}
	flight  singleflight.Group
	lastLog atomic.Int64 // unix nanos of the last failure log line
}

// NewClient returns nil unless cfg is Active.
func NewClient(cfg Config, opts ClientOptions) *Client {
	if !cfg.Active() {
		return nil
	}
	return &Client{
		token: strings.TrimSpace(cfg.Token),
		opts:  opts,
		http: &http.Client{
			Timeout: opts.Timeout,
			// Never follow a redirect: it would resend the token header
			// to wherever upstream points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sem: make(chan struct{}, opts.Concurrency),
	}
}

// FetchError is an upstream failure reduced to a fixed category. It holds
// no URL and no upstream text, so logging or returning it cannot leak the
// token or anything upstream said.
type FetchError struct{ Reason string }

func (e FetchError) Error() string { return e.Reason }

// Get fetches service?q and returns the body of a 200 response whose
// Content-Type starts with contentType. Every other outcome, including
// error bodies in any format (OGC XML, plain text, JSON), is a FetchError.
// Identical concurrent requests share one upstream call.
func (c *Client) Get(service string, q url.Values, contentType string) ([]byte, error) {
	if !allowedServices[service] {
		return nil, ErrBadRequest
	}
	for k := range q {
		if strings.EqualFold(k, tokenHeader) {
			return nil, ErrBadRequest
		}
	}
	target := c.opts.BaseURL + service + "?" + q.Encode()
	v, err, _ := c.flight.Do(contentType+" "+target, func() (interface{}, error) {
		return c.fetch(target, contentType)
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func (c *Client) fetch(target, contentType string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, FetchError{"busy"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, FetchError{"request"}
	}
	req.Header.Set(tokenHeader, c.token)
	req.Header.Set("Accept", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, FetchError{"timeout"}
		}
		return nil, FetchError{"transport"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, int64(c.opts.MaxBodyBytes)))
		return nil, FetchError{"status " + strconv.Itoa(resp.StatusCode)}
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), contentType) {
		return nil, FetchError{"unexpected content type"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.opts.MaxBodyBytes)+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, FetchError{"timeout"}
		}
		return nil, FetchError{"read"}
	}
	if len(data) > c.opts.MaxBodyBytes {
		return nil, FetchError{"oversize response"}
	}
	return data, nil
}

func isTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// LogFailure logs an upstream failure by category only, at most once per
// LogEvery.
func (c *Client) LogFailure(what string, err error) {
	var fe FetchError
	reason := "unknown"
	if errors.As(err, &fe) {
		reason = fe.Reason
	}
	now := c.opts.Now().UnixNano()
	last := c.lastLog.Load()
	if c.opts.LogEvery > 0 && now-last < int64(c.opts.LogEvery) {
		return
	}
	if !c.lastLog.CompareAndSwap(last, now) {
		return
	}
	c.opts.Logf("[dhm] upstream %s failed (%s)", what, reason)
}
