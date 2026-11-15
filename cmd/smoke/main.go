// Command smoke is the manual live smoke check of every external client's
// request shape (#803).
//
// The upstream clients are unit-tested against httptest fakes only, so a
// request shape the real service rejects passes every test: #802's INSPIRE
// buildings filter (cql2-text, HTTP 400 for DWITHIN) showed only on
// staging. smoke sends each client's one allow-listed request shape once
// to the real service and prints, per upstream, the HTTP status, the
// content type, the body size and a cheap shape check. Every URL comes
// from the client's own builder (internal/dhm, internal/buildings, ...),
// so the check covers exactly what the server sends.
//
// It is run by hand before a release candidate, never in CI or tests:
//
//	scripts/smoke-upstreams.sh [-config path/to/config.json]
//
// The Dataforsyningen token is read from the config's map.dhm.token, as
// the server reads it, sent only in the token header of the requests that
// need it, and never printed: every output line is redacted. A probe whose
// token is missing is SKIP, not FAIL; so is the Skråfoto CDN read with no
// photo (from the search, or -skraafoto-cdn-path). Any FAIL exits 1; a config that
// cannot be read exits 2 before any request.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, nil))
}

// run is the command: it returns the exit code. rt replaces the HTTP
// transport (tests); nil is the default transport without transparent
// compression, as the clients use.
func run(args []string, stdout, stderr io.Writer, rt http.RoundTripper) int {
	s := defaultSettings()
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "server config.json to read map.dhm.token from (none: token probes SKIP)")
	timeout := fs.Duration("timeout", 30*time.Second, "per request, including the body")
	fs.IntVar(&s.year, "skraafoto-year", s.year, "Skråfoto collection year the STAC search asks")
	fs.StringVar(&s.cdnPath, "skraafoto-cdn-path", s.cdnPath,
		"CDN path (/skraafoto_server/...tif) of the photo to read when the STAC search did not run or found none")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	toks := tokens{}
	if *configPath != "" {
		var err error
		if toks, err = loadTokens(*configPath); err != nil {
			fmt.Fprintf(stderr, "smoke: %v\n", err)
			return 2
		}
	}
	out := &redactingWriter{w: stdout, r: newRedactor(toks.secrets()...)}
	hc := newHTTPClient(rt, *timeout)
	fails := 0
	for _, p := range buildProbes(s) {
		res := execute(context.Background(), hc, p, toks)
		out.line(res.String())
		if res.verdict == fail {
			fails++
		}
	}
	if fails > 0 {
		out.line(fmt.Sprintf("smoke: %d upstream(s) failed", fails))
		return 1
	}
	return 0
}

// newHTTPClient is the smoke check's client: like the upstream clients',
// it never follows a redirect and has transparent gzip off.
func newHTTPClient(rt http.RoundTripper, timeout time.Duration) *http.Client {
	hc := dhm.NewNoRedirectClient(timeout)
	if rt != nil {
		hc.Transport = rt
	}
	return hc
}
