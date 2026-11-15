package heritage

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// fixedQuery is every query parameter except bbox: a WFS 2.0 GetFeature
// on the zone layer, GeoJSON, in EPSG:25832, at most MaxFeatures zones.
// The values hold only characters that need no escaping in a query string
// (TestFetchCellNeverSendsAFilter).
//
// There is deliberately no filter: cql_filter=DWITHIN(...) does not work
// on this service (#723 research: a lat/lon point returned nonsensical
// reprojected coordinates, a native point returned nothing in either axis
// order). The client asks for a bbox and Nearest tests the geometry.
const fixedQuery = "service=WFS&version=2.0.0&request=GetFeature" +
	"&typeNames=" + TypeName +
	"&outputFormat=application/json&srsName=" + CRS +
	"&count=100" // MaxFeatures; TestFetchCellNeverSendsAFilter pins the two together

// acceptedContentTypes: GeoServer answers outputFormat=application/json
// with application/json;charset=UTF-8. An OGC exception report is XML and
// is refused by type.
var acceptedContentTypes = []string{"application/json", "application/geo+json"}

// ErrBadCell is returned for a cell outside the extent. It holds no caller
// input.
var ErrBadCell = errors.New("heritage: cell outside the extent")

// ClientOptions are the limits of the upstream client.
type ClientOptions struct {
	BaseURL      string        // DefaultBaseURL outside tests
	Timeout      time.Duration // per request, including the body
	MaxBodyBytes int           // larger responses are rejected
	// MarginM grows every cell's query box (Cell.QueryBox): the server's
	// query radius, 0..MaxMarginM.
	MarginM int
}

// DefaultClientOptions are the production limits. One zone is about
// 2 KB of GeoJSON (#723 research), so a full page of MaxFeatures is about
// 0.2 MB; 1 MiB leaves room for zones with many vertices.
func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		BaseURL:      DefaultBaseURL,
		Timeout:      10 * time.Second,
		MaxBodyBytes: 1 << 20,
		MarginM:      100,
	}
}

// Client fetches one cell at a time from the WFS. It holds no cache and no
// concurrency limit: the server route adds those.
type Client struct {
	opts ClientOptions
	http *http.Client
}

// NewClient returns a client with opts, or an error for a margin outside
// 0..MaxMarginM.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.MarginM < 0 || opts.MarginM > MaxMarginM {
		return nil, errors.New("heritage: margin out of range")
	}
	return &Client{opts: opts, http: dhm.NewNoRedirectClient(opts.Timeout)}, nil
}

// CellURL is the GetFeature URL for a valid cell. Nothing but the cell's
// integer bbox varies. The live smoke check
// (#803) sends it too.
func (c *Client) CellURL(cell Cell) string {
	b := cell.QueryBox(c.opts.MarginM)
	return c.opts.BaseURL + "?" + fixedQuery + "&bbox=" +
		strconv.Itoa(b[0]) + "," + strconv.Itoa(b[1]) + "," +
		strconv.Itoa(b[2]) + "," + strconv.Itoa(b[3]) + "," + CRS
}

// FetchCell fetches and parses the zones intersecting one cell's query
// box. Every failure is ErrBadCell or a dhm.FetchError with a fixed
// category ("canceled", "timeout", "status 500", "bad response", ...),
// never upstream text.
func (c *Client) FetchCell(ctx context.Context, cell Cell) (Page, error) {
	if !cell.Valid() {
		return Page{}, ErrBadCell
	}
	body, err := dhm.GetBody(ctx, c.http, c.CellURL(cell), "application/json", acceptedContentTypes, c.opts.MaxBodyBytes)
	if err != nil {
		return Page{}, err
	}
	page, err := Parse(body)
	if err != nil {
		return Page{}, dhm.FetchError{Reason: "bad response"}
	}
	return page, nil
}
