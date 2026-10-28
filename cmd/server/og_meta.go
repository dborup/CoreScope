package main

import (
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// #646 — link-preview crawlers do not run JavaScript, so the Open Graph and
// Twitter tags in index.html are filled in by the server from the branding
// config (config.json + theme.json, same precedence as /api/config/theme).
//
// index.html carries __OG_*__ placeholders. Everything except the origin of a
// relative og:image is resolved once at startup; per request we only swap in
// the origin, so the cost is one strings.Replace on a ~10 KB document.

const (
	ogDefaultSiteName    = "CoreScope"
	ogDefaultDescription = "Real-time MeshCore LoRa mesh network analyzer — live packet visualization, node tracking, channel decryption, route analysis, and deep mesh analytics."
	ogDefaultImage       = "og-image.png"
	ogOriginPlaceholder  = "__OG_ORIGIN__"
)

// ogHostRe limits what may be echoed back from the Host header into an
// absolute URL. Anything else yields a relative path rather than markup.
var ogHostRe = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]{1,5})?$|^\[[0-9A-Fa-f:.]+\](:[0-9]{1,5})?$`)

type ogMeta struct {
	replacer *strings.Replacer // static placeholders, applied once
	trusted  []*net.IPNet
	// publicBase, when set, is used instead of the request origin.
	publicBase string
	// dims is the pre-rendered og:image:width/height markup ("" if unknown).
	dims string
	// A local image (imagePath, relative to public/) needs the request
	// origin; a configured absolute URL (imageAbs) does not.
	imageNeedsOrigin bool
	imagePath        string
	imageAbs         string
}

func ogString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// newOGMeta resolves branding once. pick returns the raw value of a branding
// key (nil when unset); publicDir is where a relative ogImageUrl must live;
// trusted lists the reverse proxies whose X-Forwarded-Proto is believed.
func newOGMeta(pick func(string) any, publicDir string, trusted []*net.IPNet) *ogMeta {
	if pick == nil {
		pick = func(string) any { return nil }
	}
	o := &ogMeta{trusted: trusted}

	siteName := ogString(pick("siteName"))
	if siteName == "" {
		siteName = ogDefaultSiteName
	}
	desc := ogString(pick("tagline"))
	if desc == "" {
		desc = ogDefaultDescription
	}

	if base := ogString(pick("publicBaseUrl")); base != "" {
		if u, err := url.Parse(base); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			o.publicBase = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
		}
	}

	spec := ogString(pick("ogImageUrl"))
	switch {
	case ogIsAbsoluteURL(spec) && !ogIsGithubHost(spec):
		o.imageAbs = spec
	case spec != "" && !ogIsAbsoluteURL(spec) && ogLocalImage(publicDir, spec) != "":
		o.imagePath = ogLocalImage(publicDir, spec)
	default:
		// Unset, or unusable (github host, traversal, missing file): fall
		// back to the bundled image rather than emit a broken URL.
		o.imagePath = ogDefaultImage
	}
	if o.imagePath != "" {
		o.imageNeedsOrigin = true
		if f, err := os.Open(filepath.Join(publicDir, filepath.FromSlash(o.imagePath))); err == nil {
			if cfg, _, err := image.DecodeConfig(f); err == nil && cfg.Width > 0 && cfg.Height > 0 {
				o.dims = fmt.Sprintf(`<meta property="og:image:width" content="%d">`+"\n  "+`<meta property="og:image:height" content="%d">`, cfg.Width, cfg.Height)
			}
			f.Close()
		}
	}

	e := html.EscapeString
	o.replacer = strings.NewReplacer(
		"__OG_TITLE__", e(siteName),
		"__OG_DESCRIPTION__", e(desc),
		"__OG_IMAGE_DIMS__", o.dims,
	)
	return o
}

func ogIsAbsoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func ogIsGithubHost(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return true
	}
	h := strings.ToLower(u.Hostname())
	return h == "github.com" || strings.HasSuffix(h, ".github.com") || strings.HasSuffix(h, ".githubusercontent.com")
}

// ogLocalImage returns the slash-separated path (relative to publicDir) of an
// existing image file, or "" when spec is not a safe path under publicDir.
func ogLocalImage(publicDir, spec string) string {
	if strings.Contains(spec, "\\") || strings.HasPrefix(spec, "//") || strings.Contains(spec, ":") || strings.Contains(spec, "?") || strings.Contains(spec, "#") {
		return ""
	}
	clean := path.Clean("/" + spec)
	if clean == "/" || strings.Contains(spec, "..") {
		return ""
	}
	switch strings.ToLower(path.Ext(clean)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return ""
	}
	rel := strings.TrimPrefix(clean, "/")
	st, err := os.Stat(filepath.Join(publicDir, filepath.FromSlash(rel)))
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return rel
}

// origin returns scheme://host for the request. Scheme comes from
// X-Forwarded-Proto only when the TCP peer is a configured trusted proxy.
func (o *ogMeta) origin(r *http.Request) string {
	if o.publicBase != "" {
		return o.publicBase
	}
	if !ogHostRe.MatchString(r.Host) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if len(o.trusted) > 0 {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ipInAny(net.ParseIP(strings.TrimSpace(host)), o.trusted) {
			switch strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])) {
			case "https":
				scheme = "https"
			case "http":
				scheme = "http"
			}
		}
	}
	return scheme + "://" + r.Host
}

// prepare applies the request-independent substitutions. The image is left as
// a placeholder only when it needs the request origin.
func (o *ogMeta) prepare(doc string) string {
	doc = o.replacer.Replace(doc)
	if o.imageNeedsOrigin {
		return strings.ReplaceAll(doc, "__OG_IMAGE__", ogOriginPlaceholder+"/"+html.EscapeString(o.imagePath))
	}
	return strings.ReplaceAll(doc, "__OG_IMAGE__", html.EscapeString(o.imageAbs))
}

// render returns the final document for one request.
func (o *ogMeta) render(prepared []byte, r *http.Request) []byte {
	if !o.imageNeedsOrigin {
		return prepared
	}
	return []byte(strings.ReplaceAll(string(prepared), ogOriginPlaceholder, html.EscapeString(o.origin(r))))
}

// ogMetaFromConfig builds the link-preview renderer from config.json branding
// overlaid by theme.json (same precedence as handleConfigTheme).
func ogMetaFromConfig(cfg *Config, publicDir string) *ogMeta {
	theme := LoadTheme(".")
	pick := func(key string) any {
		if v, ok := theme.Branding[key]; ok && v != nil {
			return v
		}
		if cfg == nil {
			return nil
		}
		return cfg.Branding[key]
	}
	var trusted []*net.IPNet
	if cfg != nil {
		trusted = parseCIDRList(cfg.WSTrustedProxies(), "trustedProxies")
	}
	return newOGMeta(pick, publicDir, trusted)
}
