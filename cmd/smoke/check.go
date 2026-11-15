package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig of the ortho tile
	_ "image/png"  // image.DecodeConfig of the WMS tiles
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/meshcore-analyzer/dhm"
)

// maxBody caps a body read; no allow-listed shape answers more than about
// 2 MB (a full Mastedatabasen cell).
const maxBody = 8 << 20

type verdict string

const (
	pass verdict = "PASS"
	fail verdict = "FAIL"
	skip verdict = "SKIP"
)

// probe is one upstream request shape.
type probe struct {
	name string
	// token is the config key of the token the request needs in the
	// dhm.TokenHeader header; "" for none.
	token string
	// url builds the request URL with the client's own builder.
	url func() (string, error)
	// rng, when set, is sent as the Range header.
	rng *dhm.Range
	// status is the only status that passes.
	status int
	// check is the cheap shape check of a reply with that status: it
	// returns a short note for the output.
	check func(*reply) (string, error)
	// then runs after a PASS: a later probe may use what this one found.
	then func(*reply)
	// skip, when set, is why the probe cannot run at all.
	skip string
}

// skipError, returned by a probe's url, makes it SKIP: an input it needs
// is missing. It is not a failure.
type skipError string

func (e skipError) Error() string { return string(e) }

// reply is what an upstream answered.
type reply struct {
	status      int
	contentType string
	header      http.Header
	body        []byte
}

// result is one output line.
type result struct {
	verdict     verdict
	name, url   string
	status      int
	contentType string
	size        int
	note, err   string
}

func (r result) String() string {
	head := fmt.Sprintf("%-4s  %-28s", r.verdict, r.name)
	switch {
	case r.verdict == skip:
		return head + "  reason=" + strconv.Quote(r.err)
	case r.status == 0:
		return head + "  error=" + strconv.Quote(r.err) + "  url=" + r.url
	}
	tail := fmt.Sprintf("  status=%d  type=%q  bytes=%d", r.status, r.contentType, r.size)
	if r.verdict == pass {
		tail += "  check=" + strconv.Quote(r.note)
	} else {
		tail += "  error=" + strconv.Quote(r.err)
	}
	return head + tail + "  url=" + r.url
}

// execute sends p once and checks the reply.
func execute(ctx context.Context, hc *http.Client, p probe, toks tokens) result {
	res := result{name: p.name}
	if p.skip != "" {
		res.verdict, res.err = skip, p.skip
		return res
	}
	if p.token != "" && toks[p.token] == "" {
		res.verdict, res.err = skip, "no "+p.token+" in the config"
		return res
	}
	res.verdict = fail
	target, err := p.url()
	var se skipError
	if errors.As(err, &se) {
		res.verdict, res.err = skip, se.Error()
		return res
	}
	if err != nil {
		res.err = "building the request: " + err.Error()
		return res
	}
	res.url = target
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.err = "building the request: " + err.Error()
		return res
	}
	if p.token != "" {
		req.Header.Set(dhm.TokenHeader, toks[p.token])
	}
	if p.rng != nil {
		req.Header.Set("Range", p.rng.Header())
	}
	resp, err := hc.Do(req)
	if err != nil {
		res.err = "transport: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	rep := &reply{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), header: resp.Header, body: body}
	res.status, res.contentType, res.size = rep.status, rep.contentType, len(body)
	switch {
	case err != nil:
		res.err = "reading the body: " + err.Error()
	case len(body) > maxBody:
		res.err = fmt.Sprintf("body over %d bytes", maxBody)
	case rep.status != p.status:
		res.err = fmt.Sprintf("status %d, want %d", rep.status, p.status)
	default:
		if res.note, err = p.check(rep); err != nil {
			res.err = err.Error()
			return res
		}
		res.verdict = pass
		if p.then != nil {
			p.then(rep)
		}
	}
	return res
}

// mediaType is the media type of a Content-Type, lower case, without
// parameters.
func mediaType(ct string) string {
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// checkImage accepts an image/<format> reply that decodes as a format
// image of edge × edge pixels: a WMS or WMTS tile.
func checkImage(format string, edge int) func(*reply) (string, error) {
	return func(r *reply) (string, error) {
		if mt := mediaType(r.contentType); mt != "image/"+format {
			return "", fmt.Errorf("content type %s, want image/%s", mt, format)
		}
		cfg, got, err := image.DecodeConfig(bytes.NewReader(r.body))
		if err != nil || got != format {
			return "", fmt.Errorf("body is not a %s image", format)
		}
		if cfg.Width != edge || cfg.Height != edge {
			return "", fmt.Errorf("%s is %dx%d, want %dx%d", format, cfg.Width, cfg.Height, edge, edge)
		}
		return fmt.Sprintf("%s %dx%d", format, cfg.Width, cfg.Height), nil
	}
}

// checkTIFF accepts a body that starts with a TIFF magic number, either
// byte order. The content type is not checked: the S3 bucket and the CDN
// label their COGs differently, and an error document fails the magic.
func checkTIFF(r *reply) (string, error) {
	switch {
	case bytes.HasPrefix(r.body, []byte("II*\x00")):
		return "tiff II*", nil
	case bytes.HasPrefix(r.body, []byte("MM\x00*")):
		return "tiff MM*", nil
	}
	return "", errors.New("body has no TIFF magic number")
}

// checkFeatures accepts a JSON reply with a features array, which may be
// empty: OGC API Features, a WFS GeoJSON answer and a STAC search.
func checkFeatures(r *reply) (string, error) {
	if mt := mediaType(r.contentType); !strings.Contains(mt, "json") {
		return "", fmt.Errorf("content type %s, want JSON", mt)
	}
	var doc struct {
		Features *[]json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		return "", errors.New("body is not JSON")
	}
	if doc.Features == nil {
		return "", errors.New("JSON has no features array")
	}
	return fmt.Sprintf("json features=%d", len(*doc.Features)), nil
}

// checkGML accepts an XML reply whose root element is a FeatureCollection:
// a WFS GetFeature answer in GML, not an exception report.
func checkGML(r *reply) (string, error) {
	if mt := mediaType(r.contentType); !strings.Contains(mt, "xml") {
		return "", fmt.Errorf("content type %s, want XML", mt)
	}
	d := xml.NewDecoder(bytes.NewReader(r.body))
	for {
		tok, err := d.Token()
		if err != nil {
			return "", errors.New("body has no XML root element")
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "FeatureCollection" {
				return "", fmt.Errorf("XML root is %s, want FeatureCollection", se.Name.Local)
			}
			return "gml FeatureCollection", nil
		}
	}
}

var contentRangeRe = regexp.MustCompile(`^bytes ([0-9]{1,15})-([0-9]{1,15})/([0-9]{1,15})$`)

// checkRange accepts a 206 reply to rng as dhm.RangeRead does: its
// Content-Range starts at rng.From and ends at rng.To, or at the object's
// last byte when that comes first, and the body is that range. next then
// checks the body.
func checkRange(rng dhm.Range, next func(*reply) (string, error)) func(*reply) (string, error) {
	return func(r *reply) (string, error) {
		cr := r.header.Get("Content-Range")
		m := contentRangeRe.FindStringSubmatch(cr)
		if m == nil {
			return "", fmt.Errorf("Content-Range %q", cr)
		}
		a, _ := strconv.ParseInt(m[1], 10, 64)
		b, _ := strconv.ParseInt(m[2], 10, 64)
		total, _ := strconv.ParseInt(m[3], 10, 64)
		if a != rng.From || b < a || b >= total || (b != rng.To && !(b == total-1 && b < rng.To)) {
			return "", fmt.Errorf("Content-Range %q for %s", cr, rng.Header())
		}
		if int64(len(r.body)) != b-a+1 {
			return "", fmt.Errorf("body %d bytes for Content-Range %q", len(r.body), cr)
		}
		note, err := next(r)
		if err != nil {
			return "", err
		}
		return "content-range " + cr + ", " + note, nil
	}
}
