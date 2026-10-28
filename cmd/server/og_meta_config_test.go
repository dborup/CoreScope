package main

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #646 follow-up — the production path (ogMetaFromConfig: config.json branding
// overlaid by theme.json, plus webSocket.trustedProxies) was only covered by
// tests that inject `pick` straight into newOGMeta.

func ogWriteTheme(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "theme.json"), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ogServe(t *testing.T, publicDir string, o *ogMeta, mod func(*http.Request)) string {
	t.Helper()
	h := spaHandler(publicDir, http.FileServer(http.Dir(publicDir)), o)
	req := httptest.NewRequest("GET", "http://example.test/", nil)
	if mod != nil {
		mod(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Body.String()
}

func TestOGMetaFromConfigUsesConfigBranding(t *testing.T) {
	pub := ogTestDir(t)
	cfg := &Config{Branding: map[string]interface{}{
		"siteName": "Cfg Name", "tagline": "Cfg tagline", "publicBaseUrl": "https://cfg.example",
	}}
	body := ogServe(t, pub, ogMetaFromConfig(cfg, pub, ogWriteTheme(t, "")), nil)
	for _, want := range []string{"<title>Cfg Name</title>", `content="Cfg tagline"`, `content="https://cfg.example/og-image.png"`} {
		if !strings.Contains(body, want) {
			t.Errorf("config.json branding not reflected, missing %q:\n%s", want, body)
		}
	}
}

func TestOGMetaFromConfigThemeOverridesConfig(t *testing.T) {
	pub := ogTestDir(t)
	cfg := &Config{Branding: map[string]interface{}{"siteName": "Cfg Name", "tagline": "Cfg tagline"}}
	theme := ogWriteTheme(t, `{"branding":{"siteName":"Theme Name"}}`)
	body := ogServe(t, pub, ogMetaFromConfig(cfg, pub, theme), nil)
	if !strings.Contains(body, "<title>Theme Name</title>") {
		t.Errorf("theme.json siteName ignored:\n%s", body)
	}
	// keys theme.json does not set still come from config.json
	if !strings.Contains(body, `content="Cfg tagline"`) {
		t.Errorf("config.json tagline lost when theme.json sets only siteName:\n%s", body)
	}
	// theme.json "null" falls through to config.json
	theme = ogWriteTheme(t, `{"branding":{"siteName":null}}`)
	body = ogServe(t, pub, ogMetaFromConfig(cfg, pub, theme), nil)
	if !strings.Contains(body, "<title>Cfg Name</title>") {
		t.Errorf("null theme.json siteName should fall through to config.json:\n%s", body)
	}
}

func TestOGMetaFromConfigWiresTrustedProxies(t *testing.T) {
	pub := ogTestDir(t)
	cfg := &Config{WebSocket: &WebSocketConfig{TrustedProxies: []string{"203.0.113.0/24"}}}
	fwd := func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1234"; r.Header.Set("X-Forwarded-Proto", "https") }
	body := ogServe(t, pub, ogMetaFromConfig(cfg, pub, ogWriteTheme(t, "")), fwd)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "https://example.test/") {
		t.Errorf("X-Forwarded-Proto not honoured from a configured trusted proxy: %v", m)
	}
	// without the config entry the same request stays http
	body = ogServe(t, pub, ogMetaFromConfig(&Config{}, pub, ogWriteTheme(t, "")), fwd)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "http://example.test/") {
		t.Errorf("X-Forwarded-Proto honoured without trustedProxies: %v", m)
	}
	// nil config must not panic and falls back to defaults
	body = ogServe(t, pub, ogMetaFromConfig(nil, pub, ogWriteTheme(t, "")), nil)
	if !strings.Contains(body, "<title>CoreScope</title>") {
		t.Errorf("nil config should give defaults:\n%s", body)
	}
}

func TestOGMetaFromConfigWarnsOnceOnBadTrustedProxy(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	pub := ogTestDir(t)
	cfg := &Config{WebSocket: &WebSocketConfig{TrustedProxies: []string{"not-an-ip"}}}
	ogMetaFromConfig(cfg, pub, ogWriteTheme(t, ""))
	if n := strings.Count(buf.String(), "not-an-ip"); n != 0 {
		t.Errorf("og_meta must not repeat the trustedProxies warning the ws limiter already logs, got %d:\n%s", n, buf.String())
	}
}

func TestOGXForwardedProtoUsesFirstElement(t *testing.T) {
	pub := ogTestDir(t)
	_, nw := ogCIDR("203.0.113.0/24")
	fwd := func(r *http.Request) {
		r.RemoteAddr = "203.0.113.9:1234"
		r.Header.Set("X-Forwarded-Proto", "https, http")
	}
	body := ogGet(t, pub, brandPick(nil), []*net.IPNet{nw}, fwd)
	if m := ogImageRe.FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "https://") {
		t.Errorf("comma list must use its first element: %v", m)
	}
}

func TestOGPublicBaseURLNeedsHTTPScheme(t *testing.T) {
	pub := ogTestDir(t)
	for _, bad := range []string{"ftp://meshview.example", "meshview.example", "javascript://x/%0aalert(1)"} {
		body := ogGet(t, pub, brandPick(map[string]string{"publicBaseUrl": bad}), nil, nil)
		if m := ogImageRe.FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "http://example.test/") {
			t.Errorf("publicBaseUrl %q should be ignored, got %v", bad, m)
		}
	}
}

func TestOGWarnsOnUnusableOperatorValues(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	pub := ogTestDir(t)
	newOGMeta(brandPick(map[string]string{"publicBaseUrl": "meshview.example", "ogImageUrl": "missing.png"}), pub, nil)
	out := buf.String()
	for _, key := range []string{"publicBaseUrl", "ogImageUrl"} {
		if !strings.Contains(out, key) {
			t.Errorf("no startup warning naming %s:\n%s", key, out)
		}
	}
	buf.Reset()
	newOGMeta(brandPick(nil), pub, nil)
	if buf.Len() != 0 {
		t.Errorf("unset values must not warn:\n%s", buf.String())
	}
}
