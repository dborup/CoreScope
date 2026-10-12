package main

import (
	"regexp"
	"strings"
	"testing"
)

// The Swagger UI page at /api/docs is the one HTML document CoreScope serves
// from Go rather than from public/. It used to pull swagger-ui-dist through a
// floating "@5" specifier with no integrity attribute, so whatever upstream
// published next would execute on an authenticated operator's browser,
// unverified. These tests pin that page: exact version, exact digest,
// crossorigin so the digest can actually be compared.
//
// The digests below are sha384 over the exact bytes served by
// https://unpkg.com/swagger-ui-dist@5.33.2/<file>. Recompute with
//
//	curl -sL <url> | openssl dgst -sha384 -binary | openssl base64 -A
//
// whenever the pin moves; see docs/browser-dependencies.md.
//
// Relates to #336.

const swaggerUIDistVersion = "5.33.2"

var wantSwaggerUIAssets = []struct {
	tag       string // "link" or "script"
	url       string
	integrity string
}{
	{
		tag:       "link",
		url:       "https://unpkg.com/swagger-ui-dist@" + swaggerUIDistVersion + "/swagger-ui.css",
		integrity: "sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW",
	},
	{
		tag:       "script",
		url:       "https://unpkg.com/swagger-ui-dist@" + swaggerUIDistVersion + "/swagger-ui-bundle.js",
		integrity: "sha384-gNnUp++d7WqR9OKX4OMoUmV2w0JxEs1uu6NPXgcynGClIX5DOu6ol4Lp+lINyZcJ",
	},
}

// externalTagRe finds every <script>/<link> tag whose URL leaves the origin.
var externalTagRe = regexp.MustCompile(`(?is)<(script|link)\b([^>]*\b(?:src|href)\s*=\s*"(?:https?:)?//[^"]*"[^>]*)>`)

var attrRe = regexp.MustCompile(`([a-zA-Z-]+)\s*=\s*"([^"]*)"`)

func externalSwaggerTags(t *testing.T) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, m := range externalTagRe.FindAllStringSubmatch(swaggerUIHTML, -1) {
		attrs := map[string]string{"__tag": strings.ToLower(m[1])}
		for _, a := range attrRe.FindAllStringSubmatch(m[2], -1) {
			attrs[strings.ToLower(a[1])] = a[2]
		}
		out = append(out, attrs)
	}
	return out
}

// TestSwaggerUIExternalAssetsArePinnedAndHashed is the structural rule: no
// off-origin subresource on this page may lack an exact version, an integrity
// digest, or the crossorigin attribute that makes the digest enforceable.
func TestSwaggerUIExternalAssetsArePinnedAndHashed(t *testing.T) {
	tags := externalSwaggerTags(t)
	if len(tags) == 0 {
		t.Fatal("found no external <script>/<link> in swaggerUIHTML; the scanner is broken or the page changed shape")
	}
	exactVersion := regexp.MustCompile(`@\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?/`)
	sri := regexp.MustCompile(`^sha(?:256|384|512)-[A-Za-z0-9+/]+={0,2}$`)
	for _, attrs := range tags {
		url := attrs["src"]
		if attrs["__tag"] == "link" {
			url = attrs["href"]
		}
		if !exactVersion.MatchString(url) {
			t.Errorf("%s %q: no exact major.minor.patch version; a floating range resolves at request time", attrs["__tag"], url)
		}
		if got := attrs["integrity"]; !sri.MatchString(got) {
			t.Errorf("%s %q: integrity = %q, want a sha256/384/512 SRI digest", attrs["__tag"], url, got)
		}
		// Without crossorigin the response is opaque and the browser skips
		// the integrity comparison instead of failing closed.
		if got := attrs["crossorigin"]; !strings.EqualFold(got, "anonymous") {
			t.Errorf("%s %q: crossorigin = %q, want \"anonymous\"", attrs["__tag"], url, got)
		}
	}
}

// TestSwaggerUIAssetsMatchReviewedPins pins the exact bytes that were reviewed,
// so moving the version or editing a digest is a deliberate, visible change.
func TestSwaggerUIAssetsMatchReviewedPins(t *testing.T) {
	tags := externalSwaggerTags(t)
	if len(tags) != len(wantSwaggerUIAssets) {
		t.Fatalf("swaggerUIHTML has %d external subresources, want %d (update wantSwaggerUIAssets together with the page)", len(tags), len(wantSwaggerUIAssets))
	}
	for i, want := range wantSwaggerUIAssets {
		attrs := tags[i]
		if attrs["__tag"] != want.tag {
			t.Errorf("subresource %d: tag = %q, want %q", i, attrs["__tag"], want.tag)
			continue
		}
		gotURL := attrs["src"]
		if want.tag == "link" {
			gotURL = attrs["href"]
		}
		if gotURL != want.url {
			t.Errorf("subresource %d: url = %q, want %q", i, gotURL, want.url)
		}
		if attrs["integrity"] != want.integrity {
			t.Errorf("subresource %d (%s): integrity = %q, want %q", i, want.url, attrs["integrity"], want.integrity)
		}
	}
}

// TestSwaggerUIHTMLHasNoUnpinnedFloatingSpecifier is a cheap belt-and-braces
// check: it catches a floating specifier anywhere in the page, including in a
// place the tag scanner would not look (an inline URL, a preload hint).
func TestSwaggerUIHTMLHasNoUnpinnedFloatingSpecifier(t *testing.T) {
	floating := regexp.MustCompile(`unpkg\.com/[^"'\s]*@\d+(?:\.\d+)?/`)
	if m := floating.FindAllString(swaggerUIHTML, -1); len(m) > 0 {
		t.Errorf("floating CDN specifier(s) in swaggerUIHTML: %v", m)
	}
}
