package buildings

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// DefaultBaseURL is the items of the building collection of INSPIRE –
// Bygninger on Dataforsyningen's OGC API Features (GeoServer). It needs no
// token: Klimadatastyrelsen exempts the INSPIRE OGC services (#730
// research §2.1), and every call of the research was made without one.
const DefaultBaseURL = "https://api.dataforsyningen.dk/ogcapi/features/building_inspire/collections/building/items"

// CRS is the only coordinate reference system the service is asked in,
// for the filter and for the returned geometry: EPSG:25832, easting first.
const CRS = "http://www.opengis.net/def/crs/EPSG/0/25832"

// fixedQuery is every query parameter but the filter: GeoJSON, EPSG:25832
// in and out, ECQL text, at most MaxFeatures footprints. The service is a
// GeoServer: its cql2-text parser has no DWITHIN ("Function not found",
// HTTP 400, seen on staging 2026-11-15), while ecql-text accepts
// DWITHIN(geometry, LINESTRING(...), r, meters) with the same filter-crs. The limit is
// written from MaxFeatures, so the two cannot drift apart (#776 review
// N2). The values hold only characters that need no escaping in a query
// string, "+" written as %2B (TestSegmentURLDecodesToTheFixedQuery).
var fixedQuery = "f=application/geo%2Bjson" +
	"&crs=" + CRS +
	"&filter-lang=ecql-text" +
	"&filter-crs=" + CRS +
	"&limit=" + strconv.Itoa(MaxFeatures)

// acceptedContentTypes: the service answers f=application/geo+json with
// application/json (#730 research). An OGC exception in XML or an HTML
// error page is refused by type.
var acceptedContentTypes = []string{"application/geo+json", "application/json"}

// ErrBadSegment is returned, without any request, for a Segment that is
// not Valid. It holds no caller input.
var ErrBadSegment = errors.New("buildings: segment outside the allowed shape")

// ClientOptions are the limits of the upstream client.
type ClientOptions struct {
	BaseURL      string        // DefaultBaseURL outside tests
	Timeout      time.Duration // per request, including the body
	MaxBodyBytes int           // larger responses are rejected
}

// DefaultClientOptions are the production limits. The research measured
// 1.22 KB per footprint, so a full page of MaxFeatures is about 1.2 MB;
// the 2 MiB cap leaves room for complex footprints. The densest 3 km of
// Aarhus C answered in 0.31 s.
func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		BaseURL:      DefaultBaseURL,
		Timeout:      5 * time.Second,
		MaxBodyBytes: 2 << 20,
	}
}

// Client fetches one segment at a time. It holds no cache and no
// concurrency limit; Fetcher adds those.
type Client struct {
	opts ClientOptions
	http *http.Client
}

// NewClient returns a client with opts. It never follows a redirect.
func NewClient(opts ClientOptions) *Client {
	return &Client{opts: opts, http: dhm.NewNoRedirectClient(opts.Timeout)}
}

// segmentURL is the items URL for a valid segment. Only the line's four
// decimetre integers and the allow-listed radius vary, and the filter is
// escaped by hand: digits and "." need no escaping, and "(", ",", " " and
// ")" are written as %28, %2C, %20 and %29.
func (c *Client) segmentURL(s Segment) string {
	l := s.Line
	return c.opts.BaseURL + "?" + fixedQuery +
		"&filter=DWITHIN%28geometry%2CLINESTRING%28" +
		fmtDM(l[0]) + "%20" + fmtDM(l[1]) + "%2C" + fmtDM(l[2]) + "%20" + fmtDM(l[3]) +
		"%29%2C" + strconv.Itoa(s.RadiusM) + "%2Cmeters%29"
}

// fmtDM writes non-negative decimetres as metres with one decimal.
func fmtDM(v int64) string {
	return strconv.FormatInt(v/10, 10) + "." + strconv.FormatInt(v%10, 10)
}

// FetchSegment fetches and parses the footprints within the segment's
// radius of its line. Every failure is ErrBadSegment or a dhm.FetchError
// with a fixed category ("canceled", "timeout", "status 500", "bad
// response", ...), never upstream text.
func (c *Client) FetchSegment(ctx context.Context, s Segment) (Page, error) {
	if !s.Valid() {
		return Page{}, ErrBadSegment
	}
	body, err := dhm.GetBody(ctx, c.http, c.segmentURL(s), "application/geo+json", acceptedContentTypes, c.opts.MaxBodyBytes)
	if err != nil {
		return Page{}, err
	}
	page, err := Parse(body)
	if err != nil {
		return Page{}, dhm.FetchError{Reason: "bad response"}
	}
	return page, nil
}
