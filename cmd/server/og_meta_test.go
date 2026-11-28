package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #646 — server-rendered Open Graph / Twitter tags from branding config.

const ogTestIndex = `<!DOCTYPE html><html><head>
<title>__OG_TITLE__</title>
<meta property="og:title" content="__OG_TITLE__">
<meta property="og:site_name" content="__OG_TITLE__">
<meta property="og:description" content="__OG_DESCRIPTION__">
<meta property="og:image" content="__OG_IMAGE__">
__OG_IMAGE_DIMS__
<meta name="twitter:title" content="__OG_TITLE__">
<meta name="twitter:description" content="__OG_DESCRIPTION__">
<meta name="twitter:image" content="__OG_IMAGE__">
<script src="app.js?v=__BUST__"></script>
</head><body></body></html>`

func ogTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(ogTestIndex), 0644); err != nil {
		t.Fatal(err)
	}
	// real 1200x630 PNG header is enough for DecodeConfig only if complete; copy the bundled one
	src, err := os.ReadFile(filepath.Join("..", "..", "public", "og-image.png"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"og-image.png", "og-image-meshview.png"} {
		os.WriteFile(filepath.Join(dir, n), src, 0644)
	}
	return dir
}

func ogGet(t *testing.T, dir string, pick func(string) any, trusted []*net.IPNet, mod func(*http.Request)) string {
	t.Helper()
	h := spaHandler(dir, http.FileServer(http.Dir(dir)), newOGMeta(pick, dir, trusted))
	req := httptest.NewRequest("GET", "http://example.test/", nil)
	if mod != nil {
		mod(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Body.String()
}

func brandPick(kv map[string]string) func(string) any {
	return func(k string) any {
		if v, ok := kv[k]; ok {
			return v
		}
		return nil
	}
}

func TestOGMetaReflectsBranding(t *testing.T) {
	dir := ogTestDir(t)
	body := ogGet(t, dir, brandPick(map[string]string{"siteName": "MeshView DK", "tagline": "Dansk MeshCore overblik"}), nil, nil)
	for _, want := range []string{
		`<title>MeshView DK</title>`,
		`<meta property="og:title" content="MeshView DK">`,
		`<meta property="og:site_name" content="MeshView DK">`,
		`<meta property="og:description" content="Dansk MeshCore overblik">`,
		`<meta name="twitter:title" content="MeshView DK">`,
		`<meta name="twitter:description" content="Dansk MeshCore overblik">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestOGMetaFallbacksWhenUnset(t *testing.T) {
	dir := ogTestDir(t)
	body := ogGet(t, dir, brandPick(nil), nil, nil)
	if !strings.Contains(body, `<title>CoreScope</title>`) || !strings.Contains(body, `og:site_name" content="CoreScope"`) {
		t.Errorf("siteName fallback missing:\n%s", body)
	}
	if !strings.Contains(body, "Real-time MeshCore LoRa mesh network analyzer") {
		t.Errorf("description fallback missing:\n%s", body)
	}
	if strings.Contains(body, "__OG_") {
		t.Errorf("unreplaced placeholder leaked:\n%s", body)
	}
	// whitespace-only values count as unset
	body = ogGet(t, dir, brandPick(map[string]string{"siteName": "   ", "tagline": ""}), nil, nil)
	if !strings.Contains(body, `<title>CoreScope</title>`) {
		t.Errorf("blank siteName must fall back:\n%s", body)
	}
}

func TestOGMetaEscapesHostileValues(t *testing.T) {
	dir := ogTestDir(t)
	evil := `"><script>alert(1)</script><x y='`
	body := ogGet(t, dir, brandPick(map[string]string{"siteName": evil, "tagline": evil, "ogImageUrl": evil}), nil, nil)
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatalf("hostile value broke out:\n%s", body)
	}
	if strings.Count(body, "<script") != 1 { // only the app.js tag
		t.Errorf("extra script element injected:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected escaped form:\n%s", body)
	}
}

var ogImageRe = regexp.MustCompile(`property="og:image" content="([^"]*)"`)
var twImageRe = regexp.MustCompile(`name="twitter:image" content="([^"]*)"`)

// ogVersionedImageRe matches a local og:image URL with the #719 content-hash
// query param: <path>?v=<12 lowercase hex chars>, nothing else after it.
var ogVersionedImageRe = regexp.MustCompile(`^(https?://[^?]+)\?v=([0-9a-f]{12})$`)

func TestOGImageAbsoluteAndNeverGithub(t *testing.T) {
	dir := ogTestDir(t)
	body := ogGet(t, dir, brandPick(nil), nil, nil)
	m := ogImageRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("default og:image = %v", m)
	}
	vm := ogVersionedImageRe.FindStringSubmatch(m[1])
	if vm == nil || vm[1] != "http://example.test/og-image.png" {
		t.Fatalf("default og:image = %q, want http://example.test/og-image.png?v=<hash>", m[1])
	}
	if tw := twImageRe.FindStringSubmatch(body); tw == nil || tw[1] != m[1] {
		t.Errorf("twitter:image %v != og:image", tw)
	}
	if !strings.Contains(body, `og:image:width" content="1200"`) || !strings.Contains(body, `og:image:height" content="630"`) {
		t.Errorf("dimensions missing for bundled image:\n%s", body)
	}
	// path under public/
	body = ogGet(t, dir, brandPick(map[string]string{"ogImageUrl": "og-image-meshview.png"}), nil, nil)
	if m := ogImageRe.FindStringSubmatch(body); m == nil {
		t.Fatalf("path ogImageUrl -> %v", m)
	} else if vm := ogVersionedImageRe.FindStringSubmatch(m[1]); vm == nil || vm[1] != "http://example.test/og-image-meshview.png" {
		t.Errorf("path ogImageUrl -> %q, want .../og-image-meshview.png?v=<hash>", m[1])
	}
	// absolute URL used verbatim, no dims claimed (unknown)
	body = ogGet(t, dir, brandPick(map[string]string{"ogImageUrl": "https://cdn.example.org/p.png"}), nil, nil)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || m[1] != "https://cdn.example.org/p.png" {
		t.Errorf("absolute ogImageUrl -> %v", m)
	}
	if strings.Contains(body, "og:image:width") {
		t.Errorf("must not claim dimensions for a remote image:\n%s", body)
	}
	// github hosts and traversal/missing files fall back to the bundled default
	for _, bad := range []string{
		"https://raw.githubusercontent.com/x/y/master/og.png",
		"https://github.com/x/y/raw/og.png",
		"../../etc/passwd",
		"missing.png",
		"sub/../og-image-meshview.png", // resolves inside public/, but ".." is refused outright
		"javascript:alert(1)",
		"//evil.example/x.png",
	} {
		body = ogGet(t, dir, brandPick(map[string]string{"ogImageUrl": bad}), nil, nil)
		m := ogImageRe.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("ogImageUrl %q -> %v, want bundled default", bad, m)
			continue
		}
		if vm := ogVersionedImageRe.FindStringSubmatch(m[1]); vm == nil || vm[1] != "http://example.test/og-image.png" {
			t.Errorf("ogImageUrl %q -> %q, want bundled default with ?v=<hash>", bad, m[1])
		}
	}
	if strings.Contains(strings.ToLower(body), "githubusercontent") {
		t.Errorf("github leaked")
	}
}

func TestOGImageBaseURL(t *testing.T) {
	dir := ogTestDir(t)
	// configured public base wins over Host
	body := ogGet(t, dir, brandPick(map[string]string{"publicBaseUrl": "https://meshview.example/"}), nil, nil)
	m := ogImageRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("publicBaseUrl -> %v", m)
	}
	if vm := ogVersionedImageRe.FindStringSubmatch(m[1]); vm == nil || vm[1] != "https://meshview.example/og-image.png" {
		t.Errorf("publicBaseUrl -> %q, want .../og-image.png?v=<hash>", m[1])
	}
	// X-Forwarded-Proto ignored from an untrusted peer
	fwd := func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1234"; r.Header.Set("X-Forwarded-Proto", "https") }
	body = ogGet(t, dir, brandPick(nil), nil, fwd)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "http://example.test/") {
		t.Errorf("untrusted XFP honoured: %v", m)
	}
	// honoured from a trusted proxy
	_, nw, _ := net.ParseCIDR("203.0.113.0/24")
	body = ogGet(t, dir, brandPick(nil), []*net.IPNet{nw}, fwd)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "https://example.test/") {
		t.Errorf("trusted XFP not honoured: %v", m)
	}
	// a trusted-proxy list that does not contain the peer must not unlock XFP
	_, other := ogCIDR("198.51.100.0/24")
	body = ogGet(t, dir, brandPick(nil), []*net.IPNet{other}, fwd)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "http://example.test/") {
		t.Errorf("XFP honoured from a peer outside trustedProxies: %v", m)
	}
	// hostile Host header cannot inject markup
	body = ogGet(t, dir, brandPick(nil), nil, func(r *http.Request) { r.Host = `a"><script>x</script>` })
	if strings.Contains(body, "<script>x") {
		t.Errorf("Host header injected markup:\n%s", body)
	}
}

func ogCIDR(c string) (net.IP, *net.IPNet) {
	ip, n, _ := net.ParseCIDR(c)
	return ip, n
}

func TestOGMetaKeepsBustReplacement(t *testing.T) {
	dir := ogTestDir(t)
	body := ogGet(t, dir, brandPick(nil), nil, nil)
	if strings.Contains(body, "__BUST__") || !regexp.MustCompile(`app\.js\?v=\d+`).MatchString(body) {
		t.Errorf("__BUST__ not replaced:\n%s", body)
	}
}

func TestOGMetaServedOnSPAFallbackToo(t *testing.T) {
	dir := ogTestDir(t)
	h := spaHandler(dir, http.FileServer(http.Dir(dir)), newOGMeta(brandPick(map[string]string{"siteName": "X1"}), dir, nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/nodes/abc", nil))
	if !strings.Contains(w.Body.String(), "<title>X1</title>") {
		t.Errorf("deep link did not get rendered tags:\n%s", w.Body.String())
	}
	m := ogImageRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("deep link og:image not absolute: %v", m)
	}
	if vm := ogVersionedImageRe.FindStringSubmatch(m[1]); vm == nil || vm[1] != "http://example.test/og-image.png" {
		t.Errorf("deep link og:image -> %q, want .../og-image.png?v=<hash>", m[1])
	}
}

// The shipped index.html must carry placeholders, never a hardcoded external
// image host.
func TestShippedIndexHasNoExternalOGImage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "public", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, re := range []string{`property="og:image" content="__OG_IMAGE__"`, `name="twitter:image" content="__OG_IMAGE__"`, `<title>__OG_TITLE__</title>`} {
		if !regexp.MustCompile(re).MatchString(s) {
			t.Errorf("index.html missing %s", re)
		}
	}
	if regexp.MustCompile(`(?i)(og|twitter):image"\s+content="https?://`).MatchString(s) {
		t.Errorf("index.html hardcodes an external image host")
	}
	if strings.Contains(s, "githubusercontent") {
		t.Errorf("index.html still references githubusercontent")
	}
}

// #719 — a local og:image gets a ?v=<content hash> so link-preview caches
// bust on a real image change, not on every deploy or restart.

func TestOGImageLocalHasVersionParam(t *testing.T) {
	dir := ogTestDir(t)
	body := ogGet(t, dir, brandPick(nil), nil, nil)
	m := ogImageRe.FindStringSubmatch(body)
	if m == nil || !ogVersionedImageRe.MatchString(m[1]) {
		t.Fatalf("og:image = %v, want <url>?v=<12-hex>", m)
	}
}

func TestOGImageExternalURLUnchanged(t *testing.T) {
	dir := ogTestDir(t)
	body := ogGet(t, dir, brandPick(map[string]string{"ogImageUrl": "https://cdn.example.org/p.png"}), nil, nil)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || m[1] != "https://cdn.example.org/p.png" {
		t.Errorf("absolute ogImageUrl must stay unchanged, got %v", m)
	}
}

func TestOGImageVersionTracksContent(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "og-image.png")
	write := func(content string) {
		if err := os.WriteFile(imgPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	write("content-a")
	verA1 := newOGMeta(brandPick(nil), dir, nil).imageVer
	verA2 := newOGMeta(brandPick(nil), dir, nil).imageVer
	if verA1 == "" {
		t.Fatal("expected a non-empty content hash for a local og:image")
	}
	if verA1 != verA2 {
		t.Errorf("hash changed across two reads of identical content: %q != %q", verA1, verA2)
	}

	write("content-b")
	verB := newOGMeta(brandPick(nil), dir, nil).imageVer
	if verB == "" {
		t.Fatal("expected a non-empty content hash after the file changed")
	}
	if verB == verA1 {
		t.Errorf("hash did not change when file content changed: still %q", verB)
	}
}

func TestOGImageServedWithVersionQueryString(t *testing.T) {
	dir := ogTestDir(t)
	o := newOGMeta(brandPick(nil), dir, nil)
	if o.imageVer == "" {
		t.Fatal("expected a content hash for the bundled og-image.png")
	}
	h := spaHandler(dir, http.FileServer(http.Dir(dir)), o)
	req := httptest.NewRequest("GET", "http://example.test/"+o.imagePath+"?v="+o.imageVer, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /%s?v=%s = %d, want 200", o.imagePath, o.imageVer, w.Code)
	}
	want, err := os.ReadFile(filepath.Join(dir, o.imagePath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Error("served body does not match the file on disk when the query string is present")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("unexpected Cache-Control on a versioned image response: %q", cc)
	}
}
