package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/packetcard"
)

// #812 M3 — View Path share links: /p?packet=<hash>&viewPath=1, the link
// for the View Path modal's #/packets/<hash>?viewPath=1. It is the packet
// share (packet_share.go) with one more allow-listed key, so these tests
// pin that the link takes M2's gate, grammar, visibility rule, tags and
// card, and differs only in og:url.

// vpGet is the View Path link of hash.
func (e *packetShareEnv) vpGet(hash string) string {
	return e.get("/p?packet=" + hash + "&viewPath=1").Body.String()
}

// sameButURL reports whether the View Path document of hash is the packet
// link's document but for og:url, which must carry viewPath=1.
func sameButURL(t *testing.T, e *packetShareEnv, hash string) {
	t.Helper()
	vp, plain := e.vpGet(hash), e.get("/p?packet="+hash).Body.String()
	want := "http://example.test/p?packet=" + hash + "&viewPath=1"
	if got := mustMeta(t, vp, "og:url"); got != want {
		t.Fatalf("og:url %q, want %q", got, want)
	}
	rawVP, _ := metaContent(vp, "og:url")
	rawPlain, _ := metaContent(plain, "og:url")
	if strings.Replace(vp, rawVP, rawPlain, -1) != plain {
		t.Error("the View Path document differs from the packet link's in more than og:url")
	}
}

func TestViewPathShare_OffByDefault(t *testing.T) {
	e := newPacketShareEnv(t, false, nil)
	site := e.siteDocument(t)
	for _, q := range []string{"packet=" + psFlood + "&viewPath=1", "viewPath=1"} {
		if w := e.get("/p?" + q); w.Code != 200 || w.Body.String() != site {
			t.Errorf("off: /p?%s is %d, site document %t", q, w.Code, w.Body.String() == site)
		}
	}
}

func TestViewPathShare_ParseTable(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	site := e.siteDocument(t)
	for _, c := range []struct {
		name, query string
		code        int
	}{
		{"view path", "packet=" + psFlood + "&viewPath=1", 200},
		{"key order", "viewPath=1&packet=" + psFlood, 200},
		{"uppercase hash", "packet=" + strings.ToUpper(psFlood) + "&viewPath=1", 200},
		{"v is ignored", "packet=" + psFlood + "&viewPath=1&v=abc", 200},
		{"tracking keys are ignored", "packet=" + psFlood + "&viewPath=1&fbclid=x&utm_medium=chat", 200},
		{"viewPath=0", "packet=" + psFlood + "&viewPath=0", 400},
		{"viewPath=true", "packet=" + psFlood + "&viewPath=true", 400},
		{"viewPath empty", "packet=" + psFlood + "&viewPath=", 400},
		{"viewPath=01", "packet=" + psFlood + "&viewPath=01", 400},
		{"viewPath with markup", "packet=" + psFlood + "&viewPath=%22%3E%3Cscript%3Ealert(1)%3C%2Fscript%3E", 400},
		{"a duplicated viewPath", "packet=" + psFlood + "&viewPath=1&viewPath=1", 400},
		{"another case", "packet=" + psFlood + "&viewpath=1", 400},
		{"the ping-score record", "packet=" + psFlood + "&viewPath=1&record=allTime.farthestPing", 400},
		{"an observation", "packet=" + psFlood + "&viewPath=1&obs=1", 400},
		{"a bad hash", "packet=" + psFlood[:15] + "&viewPath=1", 400},
		{"a numeric id", "packet=12345&viewPath=1", 400},
		{"an unknown hash", "packet=" + psUnknown + "&viewPath=1", 404},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := e.get("/p?" + c.query)
			if w.Code != c.code {
				t.Fatalf("status %d, want %d", w.Code, c.code)
			}
			if c.code != 200 {
				if w.Body.String() != site {
					t.Error("a refused request is not the plain site document")
				}
				return
			}
			// Nothing of the request but the hash and viewPath=1 is echoed.
			if w.Body.String() != e.vpGet(psFlood) {
				t.Error("the document differs from the plain View Path link's: something was echoed")
			}
		})
	}
	// viewPath alone is no packet link: /p keeps its old answer (terrain
	// sharing is off here, so the site document).
	if w := e.get("/p?viewPath=1"); w.Code != 200 || w.Body.String() != site {
		t.Errorf("viewPath without a packet: %d", w.Code)
	}
	// The card route does not take viewPath: the link shares the card.
	if w := e.get("/api/packets/" + psFlood + "/card.png?viewPath=1"); w.Code != 400 {
		t.Errorf("card with viewPath: %d", w.Code)
	}
}

func TestViewPathShare_Tags(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	sameButURL(t, e, psFlood)
	sameButURL(t, e, psAdvert)
	body := e.vpGet(psFlood)
	if got := mustMeta(t, body, "og:title"); got != "GRP_TXT · Flood · 2 hops · heard by 3" {
		t.Errorf("og:title %q", got)
	}
	img := mustMeta(t, body, "og:image")
	if want := "http://example.test/api/packets/" + psFlood + "/card.png?v=" + e.c.version(e.share(t, psFlood).in); img != want {
		t.Errorf("og:image %q, want %q", img, want)
	}
	if w := e.get(strings.TrimPrefix(img, "http://example.test")); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Errorf("card %d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

// ── Privacy: the visibility rule is M2's, hop and observer alike ───────────

func TestViewPathShare_HiddenHopsAndObservers(t *testing.T) {
	for _, c := range []struct {
		name      string
		edit      func(*Config)
		title     string
		branches  int
		forbidden []string
	}{
		{"blacklisted relay", func(c *Config) { c.SetNodeBlacklist([]string{psR2}) }, "GRP_TXT · Flood · 1 hop · heard by 2", 2, []string{"Viborg", "Relay Two"}},
		{"observer blacklist", func(c *Config) { c.ObserverBlacklist = []string{psO3} }, "GRP_TXT · Flood · 2 hops · heard by 2", 2, []string{"Silkeborg"}},
		{"hidden observer prefix", func(c *Config) { c.SetHiddenNamePrefixes([]string{"Obs Silke"}) }, "GRP_TXT · Flood · 2 hops · heard by 2", 2, []string{"Silkeborg"}},
		{"hidden first observer", func(c *Config) { c.ObserverBlacklist = []string{psO1} }, "GRP_TXT · Flood · 2 hops · heard by 2", 2, []string{"Aarhus", "First heard", "Spread", "Farthest"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newPacketShareEnv(t, true, c.edit)
			in := e.share(t, psFlood).in
			if len(in.Branches) != c.branches || packetcard.Observers(in) != c.branches {
				t.Fatalf("branches %d observers %d", len(in.Branches), packetcard.Observers(in))
			}
			sameButURL(t, e, psFlood)
			body := e.vpGet(psFlood)
			if got := mustMeta(t, body, "og:title"); got != c.title {
				t.Errorf("og:title %q, want %q", got, c.title)
			}
			for _, k := range []string{"og:title", "og:description", "og:image:alt"} {
				for _, s := range c.forbidden {
					if v := mustMeta(t, body, k); strings.Contains(v, s) {
						t.Errorf("%s carries the hidden %q: %q", k, s, v)
					}
				}
			}
		})
	}
	// A first observer whose only branch is hidden is not named (#852
	// review, finding 5).
	e := newPacketShareEnv(t, true, func(c *Config) { c.SetHiddenNamePrefixes([]string{"Secret"}) })
	sameButURL(t, e, psFirstUncounted)
	if body := e.vpGet(psFirstUncounted); strings.Contains(body, "Aarhus") || mustMeta(t, body, "og:title") != "GRP_TXT · Flood · 1 hop · heard by 1" {
		t.Errorf("uncounted first: og:title %q, Aarhus named %t", mustMeta(t, body, "og:title"), strings.Contains(body, "Aarhus"))
	}
	// A hidden advert origin is a 404, as for the packet link.
	e = newPacketShareEnv(t, true, func(c *Config) { c.SetNodeBlacklist([]string{psRH}) })
	if w := e.get("/p?packet=" + psHiddenAdvert + "&viewPath=1"); w.Code != 404 || w.Body.String() != e.siteDocument(t) {
		t.Errorf("hidden advert origin: %d", w.Code)
	}
	// No message text, channel or sender, for a flood or a direct packet.
	e = newPacketShareEnv(t, true, nil)
	for _, h := range []string{psFlood, psDirect, psFirstUncounted} {
		body := e.vpGet(h)
		for _, s := range psSecrets {
			if strings.Contains(body, s) {
				t.Errorf("%s: %q reaches the View Path preview", h, s)
			}
		}
	}
}

func TestViewPathShare_XSSNamesAreInert(t *testing.T) {
	evil := `<script>alert(1)</script>"><img src=x onerror=alert(2)>&amp;`
	e := newPacketShareEnv(t, true, nil)
	for _, q := range []string{`UPDATE observers SET name = ? WHERE id = '` + psO1 + `'`,
		`UPDATE transmissions SET decoded_json = json_set(decoded_json, '$.name', ?) WHERE hash = '` + psAdvert + `'`} {
		if _, err := e.srv.db.conn.Exec(q, evil); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{psFlood, psAdvert} {
		body := e.vpGet(h)
		for _, bad := range []string{"<script>alert", "<img", `"><img`, "onerror=alert(2)>"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: %q survives in the document", h, bad)
			}
		}
		if alt := mustMeta(t, body, "og:image:alt"); !strings.Contains(alt, "scriptalert(1)/script") {
			t.Errorf("%s: alt %q does not carry the name as inert text", h, alt)
		}
	}
	// A hostile viewPath value is refused, never echoed.
	w := e.get("/p?packet=" + psFlood + "&viewPath=1%22%3E%3Cscript%3Ealert(3)%3C%2Fscript%3E")
	if w.Code != 400 || strings.Contains(w.Body.String(), "alert(3)") {
		t.Errorf("hostile viewPath: %d", w.Code)
	}
}

// ── v and the cache ────────────────────────────────────────────────────────

// The View Path link and the packet link share one v and one cached card,
// and v follows an added observation on the View Path link too.
func TestViewPathShare_VersionAndCache(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	var renders atomic.Int32
	e.c.render = func(in packetcard.PacketCardInput, _ packetcard.Options) ([]byte, error) {
		renders.Add(1)
		return []byte(fmt.Sprintf("png:%d", len(in.Branches))), nil
	}
	plainImg := mustMeta(t, e.get("/p?packet="+psFlood).Body.String(), "og:image")
	vpImg := mustMeta(t, e.vpGet(psFlood), "og:image")
	if vpImg != plainImg {
		t.Fatalf("og:image differs: %q vs %q", vpImg, plainImg)
	}
	if w := e.get(strings.TrimPrefix(plainImg, "http://example.test")); w.Header().Get("X-CoreScope-Card") != "miss" {
		t.Fatalf("first: %s", w.Header().Get("X-CoreScope-Card"))
	}
	if w := e.get(strings.TrimPrefix(vpImg, "http://example.test")); w.Header().Get("X-CoreScope-Card") != "hit" || renders.Load() != 1 {
		t.Errorf("View Path card: %s, %d renders", w.Header().Get("X-CoreScope-Card"), renders.Load())
	}

	fresh := "f800000000000008"
	seedSharePacket(t, e.srv.db, fresh, 5, 1, `{"type":"CHAN","text":"x"}`, psNow.Add(-time.Minute))
	seedShareObservation(t, e.srv.db, fresh, psO1, nil, psNow.Add(-time.Minute))
	v0 := mustMeta(t, e.vpGet(fresh), "og:image")
	if v := mustMeta(t, e.vpGet(fresh), "og:image"); v != v0 {
		t.Fatalf("v moved without a change: %s → %s", v0, v)
	}
	seedShareObservation(t, e.srv.db, fresh, psO2, []string{psR1}, psNow.Add(-50*time.Second))
	e.now = psNow.Add(2*time.Minute + time.Second)
	v1 := mustMeta(t, e.vpGet(fresh), "og:image")
	if v1 == v0 {
		t.Error("v did not move after an added observation")
	}
	if v1 != mustMeta(t, e.get("/p?packet="+fresh).Body.String(), "og:image") {
		t.Error("the links' v diverge after an added observation")
	}
	if w := e.get(strings.TrimPrefix(v1, "http://example.test")); w.Header().Get("X-CoreScope-Card") != "miss" || w.Body.String() != "png:2" || w.Header().Get("Cache-Control") != "public, max-age=120" {
		t.Errorf("changed card: %s %q %s", w.Header().Get("X-CoreScope-Card"), w.Body.String(), w.Header().Get("Cache-Control"))
	}
}
