package dhm

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultBaseURL is Dataforsyningen's API root. Services are paths below it.
const DefaultBaseURL = "https://api.dataforsyningen.dk/"

// allowedServices are the only upstream services the Client calls. A
// feature that needs another DHM service adds it here, next to its own
// request shape.
var allowedServices = map[string]serviceShape{
	// The tile proxy rebuilds every WMS query from a validated TileKey;
	// the check holds any other caller to that shape too.
	WMSService: {check: hillshadeQueryCheck(HillshadeTerrain)},
	WCSService: {check: checkCoverageQuery, raster: true},
	// #687: only the exaggerated hillshade's exact tile shape.
	ExaggeratedWMSService: {check: hillshadeQueryCheck(HillshadeExaggerated)},
}

// serviceShape is what the Client enforces per service.
type serviceShape struct {
	check  func(url.Values) error // accepts only the service's exact query shape
	raster bool                   // responses are raster chunks: MaxCoverageBodyBytes applies
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
	MaxBodyBytes int           // larger responses are rejected (WMS tiles)
	// MaxCoverageBodyBytes caps a WCS raster chunk, which can exceed the
	// tile cap; 0 means maxRasterBodyBytes.
	MaxCoverageBodyBytes int
	Now                  func() time.Time
	Logf                 func(format string, args ...interface{})
	LogEvery             time.Duration // throttle for failure log lines; 0 logs all
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		BaseURL:              DefaultBaseURL,
		Timeout:              10 * time.Second,
		Concurrency:          4,
		MaxBodyBytes:         1 << 20,
		MaxCoverageBodyBytes: maxRasterBodyBytes,
		Now:                  time.Now,
		Logf:                 log.Printf,
		LogEvery:             time.Minute,
	}
}

// Client fetches from DHM services with the token added server-side.
type Client struct {
	token   string
	opts    ClientOptions
	http    *http.Client
	sem     chan struct{}
	flight  Flight[[]byte]
	lastLog atomic.Int64 // unix nanos of the last failure log line
	// lastRefusalLog throttles admission refusals on their own (#749
	// review F4), so a refusal flood never hides an upstream failure.
	lastRefusalLog atomic.Int64
}

// disableCompressionTransport clones the default transport with
// compression disabled, so it never adds Accept-Encoding: gzip.
func disableCompressionTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	return t
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
			// Disable transparent gzip: the upstream truncates gzip
			// bodies over HTTP/2 (#684), and PNG tiles and GeoTIFF
			// rasters are already compressed anyway.
			Transport: disableCompressionTransport(),
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
// media type is contentType. Every other outcome, including
// error bodies in any format (OGC XML, plain text, JSON), is a FetchError.
// Identical concurrent requests share one upstream call. A service with a
// request shape (the WCS) only accepts exactly that shape.
//
// Get has no context and no admission bound; the tile proxy uses
// FetchHillshadeAdmitted (#744).
func (c *Client) Get(service string, q url.Values, contentType string) ([]byte, error) {
	return c.get(context.Background(), nil, service, q, []string{contentType})
}

// get is Get accepting any of contentTypes, stopping when ctx is done: a
// cancelled ctx is FetchError "canceled", a passed deadline "timeout".
// The shared upstream call stops once every caller waiting on it has gone
// (#683 review N3), which frees its concurrency slot. A new upstream call
// first takes an adm slot (nil: unbounded); when none is free, get returns
// ErrAdmissionFull without starting one (#744).
func (c *Client) get(ctx context.Context, adm *Admission, service string, q url.Values, contentTypes []string) ([]byte, error) {
	shape, ok := allowedServices[service]
	if !ok {
		return nil, ErrBadRequest
	}
	for k := range q {
		if strings.EqualFold(k, tokenHeader) {
			return nil, ErrBadRequest
		}
	}
	if shape.check != nil {
		if err := shape.check(q); err != nil {
			return nil, err
		}
	}
	maxBody := c.opts.MaxBodyBytes
	if shape.raster {
		maxBody = c.opts.MaxCoverageBodyBytes
		if maxBody <= 0 {
			maxBody = maxRasterBodyBytes
		}
	}
	target := c.opts.BaseURL + service + "?" + q.Encode()
	data, _, err := c.flight.DoAdmitted(ctx, strings.Join(contentTypes, ",")+" "+target, 0, adm, func(work context.Context) ([]byte, error) {
		return c.fetch(work, target, contentTypes, maxBody)
	})
	if err != nil {
		return nil, ctxFailure(err)
	}
	return data, nil
}

// ctxFailure maps the caller's context error, when ctx ended its wait, to
// a FetchError category; any other error passes through.
func ctxFailure(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return FetchError{"canceled"}
	case errors.Is(err, context.DeadlineExceeded):
		return FetchError{"timeout"}
	}
	return err
}

// fetch runs one upstream request. work is cancelled once every caller
// has given up (get maps that for them, so fetch's own result is unread
// then); Timeout bounds the request on top of it.
func (c *Client) fetch(work context.Context, target string, contentTypes []string, maxBody int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(work, c.opts.Timeout)
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
	req.Header.Set("Accept", strings.Join(contentTypes, ", "))
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || IsTimeout(err) {
			return nil, FetchError{"timeout"}
		}
		return nil, FetchError{"transport"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, int64(maxBody)))
		return nil, FetchError{"status " + strconv.Itoa(resp.StatusCode)}
	}
	if !HasContentType(resp.Header.Get("Content-Type"), contentTypes) {
		return nil, FetchError{"unexpected content type"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || IsTimeout(err) {
			return nil, FetchError{"timeout"}
		}
		return nil, FetchError{"read"}
	}
	if len(data) > maxBody {
		return nil, FetchError{"oversize response"}
	}
	return data, nil
}

// HasContentType reports whether the media type of got is exactly one of
// want; parameters such as `; subtype=geotiff` are allowed. It is shared
// with internal/mastedb.
func HasContentType(got string, want []string) bool {
	mt, _, err := mime.ParseMediaType(got)
	if err != nil {
		return false
	}
	for _, w := range want {
		if mt == w {
			return true
		}
	}
	return false
}

// IsTimeout reports whether err is a network timeout. It is shared with
// internal/mastedb.
func IsTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// FailureReason is err's fixed category: the FetchError reason, or
// "unknown". It never holds a URL or upstream text.
func FailureReason(err error) string {
	var fe FetchError
	if errors.As(err, &fe) {
		return fe.Reason
	}
	return "unknown"
}

// LogFailure logs an upstream failure by category only, at most once per
// LogEvery.
func (c *Client) LogFailure(what string, err error) {
	c.logThrottled("[dhm] upstream %s failed (%s)", what, FailureReason(err))
}

// LogRefusal logs that what was refused by a full Admission (#744): no
// upstream request was made. It is throttled per LogEvery like
// LogFailure, but on its own clock (#749 review F4).
func (c *Client) LogRefusal(what string) {
	c.logEvery(&c.lastRefusalLog, "[dhm] %s refused: admission full, no upstream request", what)
}

// logThrottled logs at most once per LogEvery, across all callers.
func (c *Client) logThrottled(format string, args ...interface{}) {
	c.logEvery(&c.lastLog, format, args...)
}

// logEvery logs at most once per LogEvery per clock last.
func (c *Client) logEvery(last *atomic.Int64, format string, args ...interface{}) {
	if c.opts.LogEvery > 0 {
		now := c.opts.Now().UnixNano()
		prev := last.Load()
		if now-prev < int64(c.opts.LogEvery) || !last.CompareAndSwap(prev, now) {
			return
		}
	}
	c.opts.Logf(format, args...)
}
