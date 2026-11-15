// Package skraafoto is CoreScope's server-side access to Skråfoto, the
// oblique aerial photos of Denmark from Klimadatastyrelsen (#737 M1).
//
// Two upstreams, kept apart:
//
//   - The STAC API (STAC, stac.go) finds the photos of a point. It needs
//     the operator's Dataforsyningen token, the one map.dhm uses, sent in
//     the `token` header and nowhere else. One query shape only: the
//     items of one collection that contain one EPSG:25832 point and face
//     one direction, as the official viewer asks (limit SearchLimit, not
//     its 1: the server picks the best-centred, #737 M3a).
//   - The image CDN (CDN, cdn.go) holds each photo as a Cloud-Optimized
//     GeoTIFF of about 322 MB. It needs no token, and is never sent one:
//     CDN has no token to send. A preview reads the file's IFDs with one
//     range read, then the tiles of one overview (IFD4, 1284 × 876, of a
//     real photo) and nothing else, and re-encodes them as one JPEG of at
//     most PreviewMaxPx on the long edge.
//
// Every upstream answer is checked before use: STAC JSON is decoded into
// named structs and every field the server keeps is validated (ids.go);
// an asset href is accepted only as a path on the CDN host, rebuilt by
// the server; range reads are dhm.RangeRead (a 206 of exactly the range
// asked for); the TIFF layout is dhm.ParseTiledImage. Neither client
// follows a redirect. Failures are dhm.FetchError with a fixed reason.
//
// Fair use (per-client budgets, the global daily cap, admission and
// coalescing) and the caches are cmd/server's. This package is
// stdlib-only beyond internal/dhm.
package skraafoto

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/meshcore-analyzer/dhm"
)

const (
	// STACHost is the STAC API's host, fixed in code.
	STACHost = "api.dataforsyningen.dk"
	// stacBase is the STAC v2 root (#737 research §2.1: the documented
	// path without /rest is not routed).
	stacBase = "https://" + STACHost + "/rest/skraafoto_api/v2"

	// CDNHost is the image CDN's host, fixed in code; CDNBase is its root.
	CDNHost = "skraafoto-cdn.dataforsyningen.dk"
	CDNBase = "https://" + CDNHost

	// viewerURL is the official viewer, the "full resolution" link (#737
	// Q9).
	viewerURL = "https://skraafoto.dataforsyningen.dk/"

	// MinYear is the oldest collection offered: 2017 is left out (#737
	// Q4).
	MinYear = 2019
	// MaxYears bounds the years a collection list yields.
	MaxYears = 16
)

// The credit, in the #733 parts (#737 Q6): the owner treats Skråfoto as
// Klimadatastyrelsen's free data under CC BY 4.0, credited
// "Klimadatastyrelsen, Skråfotos, CC BY 4.0".
const (
	Owner       = dhm.Owner
	DatasetName = "Skråfotos"
	// DatasetURL is the agency's page about the photos.
	DatasetURL  = "https://www.klimadatastyrelsen.dk/kortlaegning/skraafotos"
	LicenseName = dhm.LicenseName
	LicenseURL  = dhm.LicenseURL
	Attribution = Owner + ", " + DatasetName + ", " + LicenseName
	// PrivacyNote is the line the viewer shows with the photos (#737
	// Q10).
	PrivacyNote = "Public aerial photos from Klimadatastyrelsen, shown at reduced size."
)

// Direction is where a photo looks: the four oblique directions and the
// straight-down nadir (#737 Q5).
type Direction string

const (
	North Direction = "north"
	East  Direction = "east"
	South Direction = "south"
	West  Direction = "west"
	Nadir Direction = "nadir"
)

// Directions are the five, in the order a search returns them.
var Directions = []Direction{North, East, South, West, Nadir}

// Valid reports whether d is one of Directions.
func (d Direction) Valid() bool {
	for _, x := range Directions {
		if d == x {
			return true
		}
	}
	return false
}

var (
	collectionRe = regexp.MustCompile(`^skraafotos(20[0-9]{2})$`)
	// itemRe is a photo id: its collection's year, then the sheet, image
	// and frame numbers (e.g. 2021_83_36_1_0020_00003045). The tail is
	// held to a fixed character set, which is what keeps an id safe in a
	// path, a cache key and a link; its exact grouping is not assumed.
	itemRe = regexp.MustCompile(`^([0-9]{4})_[0-9A-Za-z_]{1,64}$`)
	// cdnPathRe is a COG on the CDN: /skraafoto_server/, one to four
	// directories (COG_oblique_<year>/10km_…/1km_…), the photo's id.
	cdnPathRe = regexp.MustCompile(`^/skraafoto_server/(?:[A-Za-z0-9_-]{1,64}/){1,4}([0-9]{4}_[0-9A-Za-z_]{1,64})\.tif$`)
)

// CollectionYear is the year of a collection offered, skraafotos<year>
// from MinYear on.
func CollectionYear(c string) (int, bool) {
	m := collectionRe.FindStringSubmatch(c)
	if m == nil {
		return 0, false
	}
	y, _ := strconv.Atoi(m[1])
	return y, y >= MinYear
}

// Collection is the collection of year.
func Collection(year int) string { return "skraafotos" + strconv.Itoa(year) }

// ValidItem reports whether id is a photo id of collection c: the
// pattern, and the year it starts with.
func ValidItem(c, id string) bool {
	y, ok := CollectionYear(c)
	m := itemRe.FindStringSubmatch(id)
	return ok && m != nil && m[1] == strconv.Itoa(y)
}

// errHref is the refusal of an asset href that is not the photo on the
// CDN.
var errHref = dhm.FetchError{Reason: "asset not on the CDN"}

// CDNPath accepts an upstream asset href only as the path of photo id on
// the CDN: https, exactly CDNHost (no port, no user), no query or
// fragment, no escapes, and a path that cdnPathRe matches and that names
// id. The server requests CDNBase + path, never the href itself.
func CDNPath(href, id string) (string, error) {
	u, err := url.Parse(href)
	if err != nil || u.Scheme != "https" || u.Host != CDNHost || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" {
		return "", errHref
	}
	m := cdnPathRe.FindStringSubmatch(u.Path)
	if m == nil || m[1] != id || strings.Contains(href, "..") {
		return "", errHref
	}
	return u.Path, nil
}

// ViewerLink is the official viewer opened on photo id.
func ViewerLink(id string) string { return viewerURL + "?item=" + url.QueryEscape(id) }
