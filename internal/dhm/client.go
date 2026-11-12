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

	"golang.org/x/sync/singleflight"
)

// DefaultBaseURL is Dataforsyningen's API root. Services are paths below it.
const DefaultBaseURL = "https://api.dataforsyningen.dk/"

// allowedServices are the only upstream services the Client calls. A
// feature that needs another DHM service adds it here, next to its own
// request shape.
var allowedServices = map[string]serviceShape{
	// The tile proxy rebuilds every WMS query from a validated TileKey;
	// the check holds any other caller to that shape too.
	WMSService: {check: checkTileQuery},
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
	flight  singleflight.Group
	lastLog atomic.Int64 // unix nanos of the last failure log line
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
func (c *Client) Get(service string, q url.Values, contentType string) ([]byte, error) {
	return c.get(service, q, []string{contentType})
}

// get is Get accepting any of contentTypes.
func (c *Client) get(service string, q url.Values, contentTypes []string) ([]byte, error) {
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
	v, err, _ := c.flight.Do(strings.Join(contentTypes, ",")+" "+target, func() (interface{}, error) {
		return c.fetch(target, contentTypes, maxBody)
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func (c *Client) fetch(target string, contentTypes []string, maxBody int) ([]byte, error) {
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
	req.Header.Set("Accept", strings.Join(contentTypes, ", "))
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, FetchError{"timeout"}
		}
		return nil, FetchError{"transport"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, int64(maxBody)))
		return nil, FetchError{"status " + strconv.Itoa(resp.StatusCode)}
	}
	if !hasContentType(resp.Header.Get("Content-Type"), contentTypes) {
		return nil, FetchError{"unexpected content type"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, FetchError{"timeout"}
		}
		return nil, FetchError{"read"}
	}
	if len(data) > maxBody {
		return nil, FetchError{"oversize response"}
	}
	return data, nil
}

// hasContentType reports whether the media type of got is exactly one of
// want; parameters such as `; subtype=geotiff` are allowed.
func hasContentType(got string, want []string) bool {
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

func isTimeout(err error) bool {
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

// logThrottled logs at most once per LogEvery, across all callers.
func (c *Client) logThrottled(format string, args ...interface{}) {
	if c.opts.LogEvery > 0 {
		now := c.opts.Now().UnixNano()
		last := c.lastLog.Load()
		if now-last < int64(c.opts.LogEvery) || !c.lastLog.CompareAndSwap(last, now) {
			return
		}
	}
	c.opts.Logf(format, args...)
}
