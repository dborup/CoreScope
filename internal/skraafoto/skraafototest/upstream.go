// Package skraafototest is a fake Skråfoto upstream for the #737 tests in
// internal/skraafoto and cmd/server: the STAC API (which needs the token)
// and the image CDN (which does not), shaped like the documented v2
// responses and the owner's token checks. Both are reached through one
// RoundTripper, so the clients under test still address the real hosts:
// nothing here, and nothing using it, ever reaches the network.
package skraafototest

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// STACHost and CDNHost are the hosts the Transport serves.
	STACHost = "api.dataforsyningen.dk"
	CDNHost  = "skraafoto-cdn.dataforsyningen.dk"
	// STACPrefix is the API root's path.
	STACPrefix = "/rest/skraafoto_api/v2"
	// Token is the only token the fake STAC accepts: a sentinel that
	// tests search every response, header and log line for.
	Token = "SENTINEL-skraafoto-token-737"
)

// Request is one request either fake saw.
type Request struct {
	Method, Host, Path, RawQuery string
	Header                       http.Header
}

// Upstream is the fake STAC API and CDN.
type Upstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	log []Request

	// Collections are the ids /collections lists; nil: DefaultCollections.
	Collections []string
	// Directions are the directions a search finds a photo for; nil:
	// all five.
	Directions []string
	// Features, when set, are what a search of a direction finds instead
	// of Feature: raw STAC items, in order, at most the query's limit
	// (#737 M3a: real items with their camera models).
	Features map[string][]string
	// Href makes a photo's asset href; nil: DefaultHref.
	Href func(id string) string
	// Files are the CDN objects by path. COG, when set, makes the object
	// at a path that was not put (nil: 404).
	Files map[string][]byte
	COG   func(path string) []byte
	// STACHook and CDNHook answer a request instead of the fake when
	// they return true: errors, redirects, a slow upstream.
	STACHook, CDNHook func(w http.ResponseWriter, r *http.Request) bool

	// STACRequests and CDNRequests count requests that reached each
	// fake; CDNBytes counts the body bytes the CDN served.
	STACRequests, CDNRequests, CDNBytes atomic.Int64
}

// DefaultCollections are the collections of the owner's token check
// (2026-11-15) plus a test collection the client must drop.
var DefaultCollections = []string{"skraafotos2017", "skraafotos2019", "skraafotos2021", "skraafotos2023", "skraafotos2025", "skraafotos2025test"}

// Directions are the five photo directions in search order.
var Directions = []string{"north", "east", "south", "west", "nadir"}

// New starts the fakes; Close stops them.
func New() *Upstream {
	u := &Upstream{Files: map[string][]byte{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	return u
}

func (u *Upstream) Close() { u.srv.Close() }

// Log is a copy of every request so far.
func (u *Upstream) Log() []Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Request(nil), u.log...)
}

// Put stores a CDN object.
func (u *Upstream) Put(path string, data []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Files[path] = data
}

var errHost = errors.New("skraafototest: request for a host other than the STAC API or the CDN")

// Transport sends requests for https://STACHost and https://CDNHost to
// the fakes. Any other scheme or host fails, and is logged.
func (u *Upstream) Transport() http.RoundTripper {
	target, _ := url.Parse(u.srv.URL)
	inner := u.srv.Client().Transport
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		u.mu.Lock()
		u.log = append(u.log, Request{Method: r.Method, Host: r.URL.Host, Path: r.URL.Path,
			RawQuery: r.URL.RawQuery, Header: r.Header.Clone()})
		u.mu.Unlock()
		if r.URL.Scheme != "https" || (r.URL.Host != STACHost && r.URL.Host != CDNHost) {
			return nil, errHost
		}
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
		out.Host = r.URL.Host // the fake tells the two hosts apart by it
		return inner.RoundTrip(out)
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (u *Upstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.Host == CDNHost {
		u.CDNRequests.Add(1)
		if u.CDNHook != nil && u.CDNHook(w, r) {
			return
		}
		u.serveCDN(w, r)
		return
	}
	u.STACRequests.Add(1)
	if u.STACHook != nil && u.STACHook(w, r) {
		return
	}
	u.serveSTAC(w, r)
}

// ── STAC ────────────────────────────────────────────────────────────────────

var (
	itemsPathRe = regexp.MustCompile(`^` + STACPrefix + `/collections/(skraafotos[0-9]{4})/items$`)
	itemPathRe  = regexp.MustCompile(`^` + STACPrefix + `/collections/(skraafotos[0-9]{4})/items/([0-9A-Za-z_]+)$`)
	directionRe = regexp.MustCompile(`\{"eq":\[\{"property":"direction"\},"([a-z]+)"\]\}`)
)

func (u *Upstream) serveSTAC(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("token") != Token {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"Authentication token must only be lowercase (token) or uppercase (TOKEN)!"}`)
		return
	}
	switch p := r.URL.Path; {
	case p == STACPrefix+"/collections":
		cols := u.Collections
		if cols == nil {
			cols = DefaultCollections
		}
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = fmt.Sprintf(`{"id":%q,"type":"Collection","license":"various","links":[]}`, c)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"collections":[%s],"links":[]}`, strings.Join(parts, ","))
	case itemsPathRe.MatchString(p):
		c := itemsPathRe.FindStringSubmatch(p)[1]
		m := directionRe.FindStringSubmatch(r.URL.Query().Get("filter"))
		var features []string
		switch {
		case m != nil && u.Features != nil:
			features = u.Features[m[1]]
			if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n < len(features) {
				features = features[:n]
			}
		case m != nil && u.finds(m[1]):
			features = append(features, u.Feature(c, m[1]))
		}
		w.Header().Set("Content-Type", "application/geo+json")
		fmt.Fprintf(w, `{"type":"FeatureCollection","features":[%s],"links":[],"numberReturned":%d}`, strings.Join(features, ","), len(features))
	case itemPathRe.MatchString(p):
		m := itemPathRe.FindStringSubmatch(p)
		for _, d := range Directions {
			if ItemID(m[1], d) == m[2] {
				w.Header().Set("Content-Type", "application/geo+json")
				fmt.Fprint(w, u.Feature(m[1], d))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"NotFound"}`)
	default:
		http.Error(w, "404 page not found", http.StatusNotFound)
	}
}

func (u *Upstream) finds(dir string) bool {
	ds := u.Directions
	if ds == nil {
		ds = Directions
	}
	for _, d := range ds {
		if d == dir {
			return true
		}
	}
	return false
}

// ItemID is the id of the fake's photo of collection c facing dir.
func ItemID(c, dir string) string {
	for i, d := range Directions {
		if d == dir {
			return fmt.Sprintf("%s_83_36_%d_0020_00003045", strings.TrimPrefix(c, "skraafotos"), i+1)
		}
	}
	return ""
}

// DefaultHref is a documented-shape asset href on the CDN.
func DefaultHref(id string) string {
	return "https://" + CDNHost + "/skraafoto_server/COG_oblique_" + id[:4] + "/10km_614_57/1km_6145_573/" + id + ".tif"
}

// Path is the CDN path of DefaultHref(id).
func Path(id string) string {
	h, _ := url.Parse(DefaultHref(id))
	return h.Path
}

// Feature is the fake's STAC item of collection c facing dir, in the v2
// shape: one asset, data, plus fields a client must ignore.
func (u *Upstream) Feature(c, dir string) string {
	id := ItemID(c, dir)
	href := DefaultHref(id)
	if u.Href != nil {
		href = u.Href(id)
	}
	return fmt.Sprintf(`{"type":"Feature","stac_version":"1.0.0","id":%q,"collection":%q,`+
		`"geometry":{"type":"Polygon","coordinates":[[[574000,6224000],[575000,6224000],[575000,6225000],[574000,6224000]]]},`+
		`"properties":{"datetime":"2025-05-07T10:21:33Z","direction":%q,"gsd":0.1,"license":"various","proj:shape":[14016,20544],"pers:omega":1.5},`+
		`"assets":{"data":{"href":%q,"type":"image/tiff; application=geotiff; profile=cloud-optimized","roles":["data"]}},`+
		`"links":[{"rel":"license","href":"https://dataforsyningen.dk/Vilkaar"}]}`, id, c, dir, href)
}

// ── CDN ─────────────────────────────────────────────────────────────────────

var rangeRe = regexp.MustCompile(`^bytes=([0-9]+)-([0-9]+)$`)

func (u *Upstream) object(path string) ([]byte, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if data, ok := u.Files[path]; ok {
		return data, true
	}
	if u.COG == nil {
		return nil, false
	}
	data := u.COG(path)
	if data != nil {
		u.Files[path] = data
	}
	return data, data != nil
}

func (u *Upstream) serveCDN(w http.ResponseWriter, r *http.Request) {
	data, ok := u.object(r.URL.Path)
	if !ok {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Accept-Ranges", "bytes")
	m := rangeRe.FindStringSubmatch(r.Header.Get("Range"))
	if m == nil {
		w.Header().Set("Content-Type", "image/tiff")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(data)
		u.CDNBytes.Add(int64(n))
		return
	}
	from, _ := strconv.ParseInt(m[1], 10, 64)
	to, _ := strconv.ParseInt(m[2], 10, 64)
	size := int64(len(data))
	if from >= size || to < from {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	to = min(to, size-1)
	WriteRange(w, data[from:to+1], from, size)
	u.CDNBytes.Add(to - from + 1)
}

// WriteRange answers 206 with part, the bytes at from of a size-byte
// object; hooks use it to answer with a different range.
func WriteRange(w http.ResponseWriter, part []byte, from, size int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, from+int64(len(part))-1, size))
	w.Header().Set("Content-Length", strconv.Itoa(len(part)))
	w.Header().Set("Content-Type", "image/tiff")
	w.WriteHeader(http.StatusPartialContent)
	w.Write(part)
}
