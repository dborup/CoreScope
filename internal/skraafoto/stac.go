package skraafoto

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

const (
	// tokenHeader is the header Dataforsyningen reads the token from, as
	// internal/dhm sends it.
	tokenHeader = "token"
	// epsg25832 is the CRS of the search point and of the answer, as the
	// official viewer asks (src/modules/api.js).
	epsg25832 = "http://www.opengis.net/def/crs/EPSG/0/25832"

	maxCollectionsBody = 1 << 20
	maxItemsBody       = 512 << 10
)

// stacTypes are the media types a STAC answer may carry.
var stacTypes = []string{"application/geo+json", "application/json"}

// Options are the limits of a client.
type Options struct {
	// Timeout bounds one upstream request, from sending it to the end of
	// its body. Waiting for a slot is not counted; the caller's context
	// bounds that.
	Timeout time.Duration
	// Concurrency is the number of upstream requests in flight at once.
	Concurrency int
	// MaxOutput caps a preview JPEG (CDN only); 0: MaxPreviewOutput.
	MaxOutput int
	// Transport replaces the HTTP transport (tests: skraafototest). nil
	// is a clone of http.DefaultTransport without transparent
	// compression.
	Transport http.RoundTripper
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Concurrency < 1 {
		o.Concurrency = 4
	}
	if o.MaxOutput < 1 {
		o.MaxOutput = MaxPreviewOutput
	}
	if o.Transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DisableCompression = true
		o.Transport = t
	}
	return o
}

// limiter is a client's bounded upstream access: a no-redirect HTTP
// client, a concurrency slot per request and a per-request timeout.
type limiter struct {
	opts Options
	http *http.Client
	sem  chan struct{}
	sent atomic.Int64
}

func newLimiter(o Options) limiter {
	o = o.withDefaults()
	// Never follow a redirect: the STAC client would resend its token
	// header wherever it points, and a 3xx is not a 206 for the CDN.
	hc := dhm.NewNoRedirectClient(0)
	hc.Transport = o.Transport
	return limiter{opts: o, http: hc, sem: make(chan struct{}, o.Concurrency)}
}

// do runs fn holding a slot, on ctx bounded by the timeout.
func (l *limiter) do(ctx context.Context, fn func(context.Context) error) error {
	select {
	case l.sem <- struct{}{}:
		defer func() { <-l.sem }()
	case <-ctx.Done():
		return dhm.ContextFailure(ctx.Err())
	}
	ctx, cancel := context.WithTimeout(ctx, l.opts.Timeout)
	defer cancel()
	return fn(ctx)
}

// STAC searches the Skråfoto STAC API with the operator's token.
type STAC struct {
	token string
	lim   limiter
}

// NewSTAC returns nil without a token.
func NewSTAC(token string, o Options) *STAC {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return &STAC{token: token, lim: newLimiter(o)}
}

// Requests is the number of upstream requests sent.
func (s *STAC) Requests() int64 { return s.lim.sent.Load() }

// get fetches path (below the v2 root) with query q, if any. path is
// built by the callers from validated parts only.
func (s *STAC) get(ctx context.Context, path string, q url.Values, maxBody int) ([]byte, error) {
	target := stacBase + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	h := http.Header{}
	h.Set(tokenHeader, s.token)
	h.Set("Accept", strings.Join(stacTypes, ", "))
	var body []byte
	err := s.lim.do(ctx, func(ctx context.Context) error {
		s.lim.sent.Add(1)
		var err error
		body, err = dhm.GetBodyWithHeader(ctx, s.lim.http, target, h, stacTypes, maxBody)
		return err
	})
	return body, err
}

// Years are the collections offered, newest first (ParseCollections).
func (s *STAC) Years(ctx context.Context) ([]int, error) {
	body, err := s.get(ctx, "/collections", nil, maxCollectionsBody)
	if err != nil {
		return nil, err
	}
	return ParseCollections(body)
}

// Search returns the best-centred photo of year's collection that
// contains the EPSG:25832 point (e, n) and faces d, or nil when there is
// none: the one query shape the server sends.
func (s *STAC) Search(ctx context.Context, year, e, n int, d Direction) (*Item, error) {
	if _, ok := CollectionYear(Collection(year)); !ok || !d.Valid() {
		return nil, dhm.ErrBadRequest
	}
	// Every value is an integer or a constant: no caller text reaches it.
	filter := fmt.Sprintf(`{"and":[{"contains":[{"property":"geometry"},{"type":"Point","coordinates":[%d,%d]}]},`+
		`{"eq":[{"property":"direction"},"%s"]}]}`, e, n, d)
	q := url.Values{
		"limit": {"1"}, "filter": {filter}, "filter-lang": {"cql-json"},
		"filter-crs": {epsg25832}, "crs": {epsg25832},
	}
	body, err := s.get(ctx, "/collections/"+Collection(year)+"/items", q, maxItemsBody)
	if err != nil {
		return nil, err
	}
	return ParseSearch(body, year, d)
}

// Item looks up photo id of collection c, for a preview of a photo no
// search has cached.
func (s *STAC) Item(ctx context.Context, c, id string) (Item, error) {
	if !ValidItem(c, id) {
		return Item{}, dhm.ErrBadRequest
	}
	body, err := s.get(ctx, "/collections/"+c+"/items/"+id, nil, maxItemsBody)
	if err != nil {
		return Item{}, err
	}
	return ParseItem(body, c, id)
}
