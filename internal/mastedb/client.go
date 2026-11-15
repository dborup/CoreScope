package mastedb

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// DefaultBaseURL is the documented WFS endpoint ("Data fra databasen" on
// mastedatabasen.dk). It needs no key.
const DefaultBaseURL = "https://mastedatabasen.dk/viskort/map.ashx"

// TypeName is the only feature type the client asks for: existing antenna
// positions. Planned positions (MasteIFremtid) are out of scope (#724).
const TypeName = "mastedatabasen:MasteIBrug"

// PropertyNames are the fields the client asks for: what Row holds, and no
// more. Frekvensbaand must be named; the documented example list leaves it
// out and the service then omits it.
const PropertyNames = "MasteID,Unik_Station_navn,UTM_E,UTM_N,Vejnavn,Husnummer,Postnr,By," +
	"TjenesteArt_navn,Teknologi_navn,Frekvensbaand,Idriftsaettelsesdato,Objekt_type"

// fixedQuery is every query parameter except BBOX, in the documented order.
// The values hold only characters that need no escaping in a query string,
// which TestFixedQueryNeedsNoEscaping checks.
const fixedQuery = "service=wfs&request=GetFeature&version=1.1.1" +
	"&typeName=" + TypeName + "&propertyName=" + PropertyNames

// ErrBadCell is returned for a cell outside the Danish extent. It holds no
// caller input.
var ErrBadCell = errors.New("mastedb: cell outside the extent")

// acceptedContentTypes are the media types a GML response may carry. The
// service sends application/gml+xml; the generic XML types are what
// GeoServer sends in other configurations. Parse checks the content.
var acceptedContentTypes = []string{"application/gml+xml", "application/xml", "text/xml"}

// ClientOptions are the limits of the upstream client.
type ClientOptions struct {
	BaseURL      string        // DefaultBaseURL outside tests
	Timeout      time.Duration // per request, including the body
	MaxBodyBytes int           // larger responses are rejected
}

// DefaultClientOptions are the production limits. One row is about 1.1 KB
// of GML with PropertyNames, so a full 500-row page is about 0.6 MB; 2 MiB
// leaves room for a cell above the documented page limit.
func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		BaseURL:      DefaultBaseURL,
		Timeout:      15 * time.Second,
		MaxBodyBytes: 2 << 20,
	}
}

// Client fetches one cell at a time from the WFS. It holds no cache and no
// concurrency limit: the server route adds those (#724 M2).
type Client struct {
	opts ClientOptions
	http *http.Client
}

// NewClient returns a client with opts.
func NewClient(opts ClientOptions) *Client {
	return &Client{
		opts: opts,
		// Never follows a redirect: the request shape is only checked for
		// the documented endpoint.
		http: dhm.NewNoRedirectClient(opts.Timeout),
	}
}

// CellURL is the GetFeature URL for a valid cell. Nothing but the cell's
// integer bbox varies. The live smoke check (#803) sends it too.
func (c *Client) CellURL(cell Cell) string {
	b := cell.BBox()
	bbox := strconv.Itoa(int(b[0])) + "," + strconv.Itoa(int(b[1])) + "," +
		strconv.Itoa(int(b[2])) + "," + strconv.Itoa(int(b[3])) + "," + CRS
	return c.opts.BaseURL + "?" + fixedQuery + "&BBOX=" + bbox
}

// FetchCell fetches and parses the rows of one cell. Every failure is
// ErrBadCell or a dhm.FetchError with a fixed category ("canceled",
// "timeout", "status 500", "bad response", ...), never upstream text.
func (c *Client) FetchCell(ctx context.Context, cell Cell) (Page, error) {
	if !cell.Valid() {
		return Page{}, ErrBadCell
	}
	body, err := c.fetch(ctx, c.CellURL(cell))
	if err != nil {
		return Page{}, err
	}
	page, err := Parse(body, cell)
	if err != nil {
		return Page{}, dhm.FetchError{Reason: "bad response"}
	}
	return page, nil
}

// fetch runs one upstream GET through the shared keyless fetch: the body
// of a 200 response with an accepted media type, at most MaxBodyBytes long.
func (c *Client) fetch(ctx context.Context, target string) ([]byte, error) {
	return dhm.GetBody(ctx, c.http, target, "application/gml+xml, application/xml;q=0.9, text/xml;q=0.8",
		acceptedContentTypes, c.opts.MaxBodyBytes)
}
