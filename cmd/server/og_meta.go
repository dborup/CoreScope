package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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

// ogPlaceholders are every __OG_*__ placeholder index.html may carry. Both
// prepare (the site document) and renderProfile (the /p document, #727)
// substitute every one of them; a test pins this list to the shipped file.
var ogPlaceholders = []string{
	"__OG_TITLE__", "__OG_SITE_NAME__", "__OG_DESCRIPTION__",
	"__OG_IMAGE__", "__OG_IMAGE_DIMS__", "__OG_PAGE_META__",
}

type ogMeta struct {
	replacer *strings.Replacer // static placeholders, applied once
	// siteName is the escaped og:site_name, which a per-profile document
	// (#727) keeps while its title changes.
	siteName string
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
	// imageVer is a #719 cache-bust value for imagePath: the first 12 hex
	// chars of the SHA-256 of the file's bytes, computed once at startup.
	// Empty when imagePath could not be read.
	imageVer string
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
		} else {
			log.Printf("[og] WARNING: branding.publicBaseUrl %q is not an absolute http(s) URL, ignored", base)
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
		if spec != "" {
			log.Printf("[og] WARNING: branding.ogImageUrl %q is unusable (github host, bad path or missing file under public/), using %s", spec, ogDefaultImage)
		}
		o.imagePath = ogDefaultImage
	}
	if o.imagePath != "" {
		o.imageNeedsOrigin = true
		if data, err := os.ReadFile(filepath.Join(publicDir, filepath.FromSlash(o.imagePath))); err == nil {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
				o.dims = fmt.Sprintf(`<meta property="og:image:width" content="%d">`+"\n  "+`<meta property="og:image:height" content="%d">`, cfg.Width, cfg.Height)
			}
			o.imageVer = ogContentHashHex(data)
		}
	}

	e := html.EscapeString
	o.siteName = e(siteName)
	o.replacer = strings.NewReplacer(
		"__OG_TITLE__", o.siteName,
		"__OG_SITE_NAME__", o.siteName,
		"__OG_DESCRIPTION__", e(desc),
		"__OG_IMAGE_DIMS__", o.dims,
		// Page-specific tags (og:url, robots) exist on /p only (#727).
		"__OG_PAGE_META__", "",
	)
	return o
}

// ogContentHashHex returns the first 12 hex characters of the SHA-256 of
// data — a short, content-stable cache-bust value for a local og:image
// (#719): it changes only when the image itself changes, not on every
// deploy or restart, unlike the __BUST__ asset cache-buster in main.go.
func ogContentHashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
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
		return strings.ReplaceAll(doc, "__OG_IMAGE__", ogOriginPlaceholder+"/"+o.localImageRef())
	}
	return strings.ReplaceAll(doc, "__OG_IMAGE__", html.EscapeString(o.imageAbs))
}

// localImageRef is the escaped path and #719 version of a local og:image.
func (o *ogMeta) localImageRef() string {
	imagePath := o.imagePath
	if o.imageVer != "" {
		imagePath += "?v=" + o.imageVer
	}
	return html.EscapeString(imagePath)
}

// imageFor is the escaped og:image value for one request: what prepare and
// render produce between them.
func (o *ogMeta) imageFor(r *http.Request) string {
	if !o.imageNeedsOrigin {
		return html.EscapeString(o.imageAbs)
	}
	return html.EscapeString(o.origin(r)) + "/" + o.localImageRef()
}

// ── Per-profile documents (#727) ───────────────────────────────────────────

// ogPage is the page-specific part of a per-profile document, unescaped:
// renderProfile is the only place it is escaped.
type ogPage struct {
	Title       string
	Description string
	// Path is the page's own path and query ("/p?…"). The caller builds it
	// from parsed, validated values only, never from the raw request; with
	// the request origin it becomes og:url.
	Path string
}

// Rune limits of the per-profile tags (#727 plan §1.3).
const (
	ogTitleMaxRunes       = 90
	ogDescriptionMaxRunes = 200
)

// renderProfile fills a per-profile document: doc is index.html with only
// __BUST__ substituted, so every OG placeholder is still in place. One
// strings.Replacer pass fills all of them; substituted values are never
// scanned again, so a value that happens to contain a placeholder stays
// text. The image stays the site card until #727 M3. og:url (re-added for
// /p only, owner decision 5; #1890 keeps it off every other page) and
// robots noindex (owner decision 8) ride __OG_PAGE_META__.
func (o *ogMeta) renderProfile(doc string, r *http.Request, pg ogPage) []byte {
	meta := `<meta name="robots" content="noindex">`
	if origin := o.origin(r); origin != "" {
		meta = `<meta property="og:url" content="` + html.EscapeString(origin+pg.Path) + `">` + "\n  " + meta
	}
	return []byte(strings.NewReplacer(
		"__OG_TITLE__", ogText(pg.Title, ogTitleMaxRunes),
		"__OG_SITE_NAME__", o.siteName,
		"__OG_DESCRIPTION__", ogText(pg.Description, ogDescriptionMaxRunes),
		"__OG_IMAGE__", o.imageFor(r),
		"__OG_IMAGE_DIMS__", o.dims,
		"__OG_PAGE_META__", meta,
	).Replace(doc))
}

// ogText is the one chokepoint for request-derived text in a meta tag:
// sanitise and truncate (ogClip), then html.EscapeString, in that order, so
// truncation can never cut an entity in half. Every value lands in a
// double-quoted attribute (or <title>) of a document whose markup is fixed
// at build time, so attribute escaping is sufficient.
func ogText(s string, maxRunes int) string {
	return html.EscapeString(ogClip(s, maxRunes))
}

// ogClip drops what has no business in a preview — C0/C1 controls, the
// U+2028/U+2029 separators, bidi overrides and isolates, and angle
// brackets — collapses whitespace runs to one space, trims, and cuts the
// result to maxRunes runes, ending in "…" when it was longer.
func ogClip(s string, maxRunes int) string {
	var b strings.Builder
	pendingSpace := false
	for _, r := range s {
		switch {
		case r == '<' || r == '>' || r == '\u2028' || r == '\u2029' ||
			(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069'):
			continue
		case unicode.IsSpace(r):
			pendingSpace = true
			continue
		case unicode.IsControl(r):
			continue
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteRune(r)
	}
	out := b.String()
	if maxRunes <= 0 || utf8.RuneCountInString(out) <= maxRunes {
		return out
	}
	rs := []rune(out)[:maxRunes-1]
	return strings.TrimRight(string(rs), " ") + "…"
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
// themeDir is where theme.json lives (LoadTheme also checks themeDir/data).
func ogMetaFromConfig(cfg *Config, publicDir, themeDir string) *ogMeta {
	theme := LoadTheme(themeDir)
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
		// Quiet: the ws limiter parses the same list and already warns.
		trusted = parseCIDRListQuiet(cfg.WSTrustedProxies())
	}
	return newOGMeta(pick, publicDir, trusted)
}
