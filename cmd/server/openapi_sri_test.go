package main

import (
	"crypto/sha512"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Swagger UI page at /api/docs is the one HTML document CoreScope serves
// from Go rather than from public/. It used to pull swagger-ui-dist through a
// floating "@5" specifier with no integrity attribute, so whatever upstream
// published next would execute on an authenticated operator's browser,
// unverified (#336). #442 additionally vendors the bytes into
// public/vendor/swagger-ui-dist/<version>/ and serves them from there instead
// of unpkg.com, so the browser makes no third-party request for this page at
// all. These tests pin the page (exact version, exact digest, crossorigin so
// the digest can actually be compared), assert no CDN reference survives, and
// hash the committed vendored bytes against the same reviewed digests.
//
// The digests below are sha384 over the exact bytes of
// https://unpkg.com/swagger-ui-dist@5.33.2/<file> (the version this page was
// pinned to in #336, now vendored rather than fetched live). Recompute with
//
//	curl -sL <url> | openssl dgst -sha384 -binary | openssl base64 -A
//
// whenever the pin moves, and re-run `sh tools/fetch-browser-deps.sh`; see
// docs/browser-dependencies.md and tools/browser-deps-manifest.json, which
// hold the same values for the Node-side test and the fetch script.
//
// Relates to #336, #442.

const swaggerUIDistVersion = "5.33.2"

var wantSwaggerUIAssets = []struct {
	tag       string // "link" or "script"
	url       string // root-relative path served by the static file handler
	dest      string // filename under public/vendor/swagger-ui-dist/<version>/
	integrity string
}{
	{
		tag:       "link",
		url:       "/vendor/swagger-ui-dist/" + swaggerUIDistVersion + "/swagger-ui.css",
		dest:      "swagger-ui.css",
		integrity: "sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW",
	},
	{
		tag:       "script",
		url:       "/vendor/swagger-ui-dist/" + swaggerUIDistVersion + "/swagger-ui-bundle.js",
		dest:      "swagger-ui-bundle.js",
		integrity: "sha384-gNnUp++d7WqR9OKX4OMoUmV2w0JxEs1uu6NPXgcynGClIX5DOu6ol4Lp+lINyZcJ",
	},
}

// anyTagRe finds every <script>/<link> tag, regardless of where its src/href
// points -- classification into off-origin vs. local-vendor happens in Go
// code afterwards, the same split test-issue-336-external-asset-pinning.js
// makes on the Node side.
var anyTagRe = regexp.MustCompile(`(?is)<(script|link)\b([^>]*)>`)

var attrRe = regexp.MustCompile(`([a-zA-Z-]+)\s*=\s*"([^"]*)"`)

// scanTags returns the attributes of every <script>/<link> tag in html, with
// the tag name under "__tag".
func scanTags(html string) []map[string]string {
	var out []map[string]string
	for _, m := range anyTagRe.FindAllStringSubmatch(html, -1) {
		attrs := map[string]string{"__tag": strings.ToLower(m[1])}
		for _, a := range attrRe.FindAllStringSubmatch(m[2], -1) {
			attrs[strings.ToLower(a[1])] = a[2]
		}
		out = append(out, attrs)
	}
	return out
}

func tagURL(attrs map[string]string) string {
	if attrs["__tag"] == "link" {
		return attrs["href"]
	}
	return attrs["src"]
}

func isOffOrigin(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "//")
}

func isLocalVendor(url string) bool {
	return strings.HasPrefix(url, "/vendor/")
}

// linkLoadsScriptOrStyle reports whether a <link> fetches something that can
// carry integrity. preconnect, dns-prefetch, canonical, icon and friends load
// nothing executable, so demanding SRI from them would be unsatisfiable.
func linkLoadsScriptOrStyle(attrs map[string]string) bool {
	rels := strings.Fields(strings.ToLower(attrs["rel"]))
	for _, rel := range rels {
		if rel == "stylesheet" || rel == "modulepreload" {
			return true
		}
	}
	as := strings.ToLower(attrs["as"])
	for _, rel := range rels {
		if rel == "preload" && (as == "script" || as == "style") {
			return true
		}
	}
	return false
}

// externalSubresourceTags returns the attributes of every off-origin
// subresource tag in html, with the tag name under "__tag". Kept as its own
// helper (rather than inlining the filter) because
// TestExternalSubresourceTagsScopesLinkByRel exercises it directly.
func externalSubresourceTags(html string) []map[string]string {
	var out []map[string]string
	for _, attrs := range scanTags(html) {
		if attrs["__tag"] == "link" && !linkLoadsScriptOrStyle(attrs) {
			continue
		}
		if !isOffOrigin(tagURL(attrs)) {
			continue
		}
		out = append(out, attrs)
	}
	return out
}

// localVendorSubresourceTags returns the attributes of every <script>/<link>
// tag in html that loads a locally vendored dependency (an absolute
// "/vendor/<name>/<version>/<file>" path), the #442 equivalent of
// externalSubresourceTags.
func localVendorSubresourceTags(html string) []map[string]string {
	var out []map[string]string
	for _, attrs := range scanTags(html) {
		if attrs["__tag"] == "link" && !linkLoadsScriptOrStyle(attrs) {
			continue
		}
		if !isLocalVendor(tagURL(attrs)) {
			continue
		}
		out = append(out, attrs)
	}
	return out
}

func externalSwaggerTags(t *testing.T) []map[string]string {
	t.Helper()
	return externalSubresourceTags(swaggerUIHTML)
}

func localSwaggerTags(t *testing.T) []map[string]string {
	t.Helper()
	return localVendorSubresourceTags(swaggerUIHTML)
}

// TestSwaggerUIHasNoCDNReference: #442's whole point for this page. Nothing
// in swaggerUIHTML may reference a third party any more, pinned or not.
func TestSwaggerUIHasNoCDNReference(t *testing.T) {
	if tags := externalSwaggerTags(t); len(tags) != 0 {
		t.Errorf("swaggerUIHTML still references an off-origin subresource: %v", tags)
	}
	if strings.Contains(swaggerUIHTML, "unpkg.com") {
		t.Error("swaggerUIHTML still mentions unpkg.com; swagger-ui-dist must be served from public/vendor/")
	}
}

// TestSwaggerUILocalAssetsArePinnedAndHashed is the structural rule: no
// vendored subresource on this page may lack an exact version, an integrity
// digest, or the crossorigin attribute that makes the digest enforceable.
func TestSwaggerUILocalAssetsArePinnedAndHashed(t *testing.T) {
	tags := localSwaggerTags(t)
	if len(tags) == 0 {
		t.Fatal("found no locally vendored <script>/<link> in swaggerUIHTML; the scanner is broken or the page changed shape")
	}
	exactVersion := regexp.MustCompile(`/vendor/[^/]+/\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?/`)
	sri := regexp.MustCompile(`^sha(?:256|384|512)-[A-Za-z0-9+/]+={0,2}$`)
	for _, attrs := range tags {
		url := tagURL(attrs)
		if !exactVersion.MatchString(url) {
			t.Errorf("%s %q: no exact major.minor.patch version; a floating range resolves at request time", attrs["__tag"], url)
		}
		if got := attrs["integrity"]; !sri.MatchString(got) {
			t.Errorf("%s %q: integrity = %q, want a sha256/384/512 SRI digest", attrs["__tag"], url, got)
		}
		if got := attrs["crossorigin"]; !strings.EqualFold(got, "anonymous") {
			t.Errorf("%s %q: crossorigin = %q, want \"anonymous\"", attrs["__tag"], url, got)
		}
	}
}

// TestExternalSubresourceTagsScopesLinkByRel: only a <link> that loads a
// script or stylesheet can carry integrity. A preconnect, dns-prefetch or
// canonical hint pointing off-origin must not count as an unpinned
// subresource (or bump the asset count below), while every rel that does
// load script or style must still be checked. Kept against a generic
// off-origin fixture (not unpkg.com specifically) since #442 removed every
// real off-origin reference from this package.
func TestExternalSubresourceTagsScopesLinkByRel(t *testing.T) {
	ignored := `<link rel="preconnect" href="https://example.com">
<link rel="dns-prefetch" href="//example.com">
<link rel="canonical" href="https://example.org/">
<link rel="icon" href="https://example.org/favicon.ico">
<link href="https://example.org/no-rel">`
	if got := externalSubresourceTags(ignored); len(got) != 0 {
		t.Errorf("non-subresource <link> tags treated as subresources: %v", got)
	}
	checked := `<link rel="stylesheet" href="https://example.com/a@1/a.css">
<link rel="alternate stylesheet" href="https://example.com/a@1/b.css">
<link rel="modulepreload" href="https://example.com/a@1/c.mjs">
<link rel="preload" as="script" href="https://example.com/a@1/d.js">
<link rel="preload" as="style" href="https://example.com/a@1/e.css">
<link rel="PRELOAD" as="Script" href="//example.com/a@1/f.js">
<script src="https://example.com/a@1/g.js"></script>`
	if got := externalSubresourceTags(checked); len(got) != 7 {
		t.Errorf("got %d checked subresources, want 7: %v", len(got), got)
	}
}

// TestSwaggerUIAssetsMatchReviewedPins pins the exact bytes that were reviewed,
// so moving the version or editing a digest is a deliberate, visible change.
func TestSwaggerUIAssetsMatchReviewedPins(t *testing.T) {
	tags := localSwaggerTags(t)
	if len(tags) != len(wantSwaggerUIAssets) {
		t.Fatalf("swaggerUIHTML has %d vendored subresources, want %d (update wantSwaggerUIAssets together with the page)", len(tags), len(wantSwaggerUIAssets))
	}
	// Matched by URL, not by position: reordering <head> is a legitimate edit
	// and must not fail this test, while a changed version or digest must.
	byURL := map[string]map[string]string{}
	for _, attrs := range tags {
		byURL[tagURL(attrs)] = attrs
	}
	for _, want := range wantSwaggerUIAssets {
		attrs, ok := byURL[want.url]
		if !ok {
			t.Errorf("swaggerUIHTML does not load %s (it loads %v)", want.url, sortedKeys(byURL))
			continue
		}
		if attrs["__tag"] != want.tag {
			t.Errorf("%s: tag = %q, want %q", want.url, attrs["__tag"], want.tag)
		}
		if attrs["integrity"] != want.integrity {
			t.Errorf("%s: integrity = %q, want %q", want.url, attrs["integrity"], want.integrity)
		}
	}
}

// TestSwaggerUIVendoredAssetsMatchReviewedDigest hashes the actual committed
// bytes under public/vendor/swagger-ui-dist/ and compares them to the
// reviewed digests above -- the Go-side analogue of the Node test's
// manifest-vs-disk check, so this package does not merely trust what the tag
// claims.
func TestSwaggerUIVendoredAssetsMatchReviewedDigest(t *testing.T) {
	for _, want := range wantSwaggerUIAssets {
		// cmd/server is the package directory; the repo root is two levels up.
		path := filepath.Join("..", "..", "public", "vendor", "swagger-ui-dist", swaggerUIDistVersion, want.dest)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		sum := sha512.Sum384(data)
		got := "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
		if got != want.integrity {
			t.Errorf("%s: sha384 = %q, want %q (reviewed value in wantSwaggerUIAssets)", path, got, want.integrity)
		}
	}
}

func sortedKeys(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
