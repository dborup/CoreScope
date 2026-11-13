package dhm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// Every test runs against a fake upstream (httptest); none reaches
// Dataforsyningen.

const testToken = "tok-dhm-SECRET-do-not-leak"

// pngStub is a real 256×256 PNG tile: FetchHillshade decodes every tile
// before returning it (#695), so a signature-only stand-in would fail.
var pngStub = dhmtest.PNG(TileSize, TileSize, 1)

type upstream struct {
	srv      *httptest.Server
	hits     atomic.Int64
	mu       sync.Mutex
	paths    []string
	rawQuery []string
	tokens   []string
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.rawQuery = append(u.rawQuery, r.URL.RawQuery)
		u.tokens = append(u.tokens, r.Header.Get("token"))
		u.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngStub)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) logf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newTestClient(t *testing.T, u *upstream, logs *logSink) *Client {
	t.Helper()
	o := DefaultClientOptions()
	o.BaseURL = u.srv.URL + "/"
	o.Timeout = 2 * time.Second
	o.Logf = logs.logf
	o.LogEvery = 0
	c := NewClient(Config{Enabled: true, Token: testToken}, o)
	if c == nil {
		t.Fatal("NewClient returned nil for an active config")
	}
	return c
}

func someTile() TileKey { return TileKey{Z: 10, X: 540, Y: 316} } // Aarhus

func TestAttributionNamesSourceAndLicence(t *testing.T) {
	for _, want := range []string{"Klimadatastyrelsen", "Danmarks Højdemodel", "CC BY 4.0"} {
		if !strings.Contains(Attribution, want) {
			t.Fatalf("Attribution %q lacks %q", Attribution, want)
		}
	}
	if strings.ContainsAny(Attribution, "<>&\"") {
		t.Fatalf("Attribution must be plain text: %q", Attribution)
	}
}

func TestNewClientInactiveIsNil(t *testing.T) {
	for _, cfg := range []Config{{}, {Enabled: true}, {Enabled: true, Token: "  "}, {Token: testToken}} {
		if NewClient(cfg, DefaultClientOptions()) != nil {
			t.Fatalf("NewClient(%+v) is not nil", cfg)
		}
	}
}

// Dataforsyningen recommends the `token` header; the token must never be
// part of an upstream URL.
func TestClientSendsTokenInHeaderNotURL(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	if _, err := c.FetchHillshadeTile(someTile()); err != nil {
		t.Fatal(err)
	}
	if u.tokens[0] != testToken {
		t.Fatalf("token header %q", u.tokens[0])
	}
	if strings.Contains(u.rawQuery[0], testToken) || strings.Contains(strings.ToLower(u.rawQuery[0]), "token") {
		t.Fatalf("token in upstream URL: %s", u.rawQuery[0])
	}
	if u.paths[0] != "/"+WMSService {
		t.Fatalf("upstream path %q", u.paths[0])
	}
}

func TestClientGetOnlyAllowListedServices(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	for _, svc := range []string{"", "dhm", "orto_foraar", "../dhm_DAF", "dhm_DAF/../x", "https://evil.example/"} {
		if _, err := c.Get(svc, url.Values{}, "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("service %q: err %v, want ErrBadRequest", svc, err)
		}
	}
	for _, k := range []string{"token", "TOKEN", "Token"} {
		if _, err := c.Get(WMSService, url.Values{k: {"x"}}, "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("query key %q: err %v, want ErrBadRequest", k, err)
		}
	}
	if u.hits.Load() != 0 {
		t.Fatalf("rejected requests reached upstream")
	}
}

// A missing or wrong token is answered with 403: as an OGC exception on the
// WMS (verified against dhm_DAF) and as the bare text `User not authorized`
// elsewhere. Neither body may come back to the caller or the log.
func TestClient403IsCategoryOnly(t *testing.T) {
	bodies := map[string]string{
		"text/plain": "User not authorized",
		"text/xml":   `<ServiceExceptionReport><ServiceException>User not authorized: User not authorized</ServiceException></ServiceExceptionReport>`,
	}
	for ct, body := range bodies {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, body)
		})
		logs := &logSink{}
		c := newTestClient(t, u, logs)
		_, err := c.FetchHillshadeTile(someTile())
		var fe FetchError
		if !errors.As(err, &fe) || fe.Reason != "status 403" {
			t.Fatalf("%s: err %v, want FetchError status 403", ct, err)
		}
		c.LogFailure("hillshade tile", err)
		if strings.Contains(logs.String(), "not authorized") || strings.Contains(logs.String(), testToken) {
			t.Fatalf("%s: upstream text or token logged: %q", ct, logs.String())
		}
		if !strings.Contains(logs.String(), "status 403") {
			t.Fatalf("%s: failure category not logged: %q", ct, logs.String())
		}
	}
}

// The body must match the content type the caller asked for, whatever its
// bytes look like (a feature without a signature check relies on this).
func TestClientRejectsUnexpectedContentType(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(pngStub)
	})
	c := newTestClient(t, u, &logSink{})
	_, err := c.FetchHillshadeTile(someTile())
	var fe FetchError
	if !errors.As(err, &fe) || fe.Reason != "unexpected content type" {
		t.Fatalf("err %v, want FetchError unexpected content type", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	elsewhere := newUpstream(t, nil)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/steal", http.StatusFound)
	})
	c := newTestClient(t, u, &logSink{})
	if _, err := c.FetchHillshadeTile(someTile()); err == nil {
		t.Fatal("redirect response accepted")
	}
	if elsewhere.hits.Load() != 0 {
		t.Fatal("redirect followed with the token header")
	}
}

func TestClientLogThrottle(t *testing.T) {
	logs := &logSink{}
	now := time.Unix(1000, 0)
	o := DefaultClientOptions()
	o.Logf = logs.logf
	o.Now = func() time.Time { return now }
	c := NewClient(Config{Enabled: true, Token: testToken}, o)
	for i := 0; i < 5; i++ {
		c.LogFailure("x", FetchError{"timeout"})
	}
	now = now.Add(2 * time.Minute)
	c.LogFailure("x", FetchError{"timeout"})
	if n := strings.Count(logs.String(), "\n"); n != 2 {
		t.Fatalf("%d log lines, want 2 (one per minute)", n)
	}
}

func TestExactQuery(t *testing.T) {
	fixed := map[string]string{"a": "1", "b": ""}
	ok := url.Values{"a": {"1"}, "b": {""}, "f": {"v"}}
	got, err := ExactQuery(ok, fixed, "f")
	if err != nil || got["f"] != "v" {
		t.Fatalf("valid query: %v %v", got, err)
	}
	bad := []url.Values{
		{"a": {"1"}, "b": {""}},                         // free key missing
		{"a": {"1"}, "b": {""}, "f": {""}},              // free key empty
		{"a": {"2"}, "b": {""}, "f": {"v"}},             // fixed value differs
		{"A": {"1"}, "b": {""}, "f": {"v"}},             // key case
		{"a": {"1"}, "b": {""}, "f": {"v", "w"}},        // repeated free key
		{"a": {"1", "1"}, "b": {""}, "f": {"v"}},        // repeated fixed key
		{"a": {"1"}, "b": {""}, "f": {"v"}, "x": {"y"}}, // extra key
		{"a": {"1"}, "f": {"v"}, "x": {""}},             // swapped key, same count
		{"a": {"1"}, "f": {"v"}},                        // fixed key missing (#682 review F2/MX1)
		{"f": {"v"}},                                    // every fixed key missing
	}
	for i, q := range bad {
		if _, err := ExactQuery(q, fixed, "f"); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("case %d %v accepted", i, q)
		}
	}
}

func TestCacheBounds(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewCache[int](2, 10, time.Minute)
	c.Put(1, []byte("aaaa"), now)
	c.Put(2, []byte("bbbb"), now)
	c.Get(1, now) // 2 is now least recently used
	c.Put(3, []byte("cc"), now)
	if _, ok := c.Get(2, now); ok || c.Len() != 2 {
		t.Fatalf("LRU entry 2 not evicted (len %d)", c.Len())
	}
	c.Put(4, []byte("dddddddd"), now)
	if c.Size() > 10 {
		t.Fatalf("size %d over byte bound", c.Size())
	}
	c.Put(5, []byte("this is longer than ten"), now)
	if _, ok := c.Get(5, now); ok {
		t.Fatal("entry larger than the byte bound cached")
	}
	if _, ok := c.Get(4, now.Add(time.Minute)); ok {
		t.Fatal("expired entry served")
	}
}

// lonLatTile is the test's own lon/lat → tile formula, independent of wms.go.
func lonLatTile(z int, lon, lat float64) TileKey {
	n := math.Exp2(float64(z))
	r := lat * math.Pi / 180
	return TileKey{
		Z: z,
		X: int((lon + 180) / 360 * n),
		Y: int((1 - math.Log(math.Tan(r)+1/math.Cos(r))/math.Pi) / 2 * n),
	}
}

// #682 review F2/MX2: tiles outside DHM coverage must never go upstream, so
// the module pins IntersectsCoverage itself.
func TestTileIntersectsCoverage(t *testing.T) {
	cases := []struct {
		name     string
		lon, lat float64
		z        int
		want     bool
	}{
		{"Aarhus", 10.2039, 56.1629, 10, true},
		{"Copenhagen z19", 12.5683, 55.6761, 19, true},
		{"Bornholm", 14.9, 55.1, 12, true},
		{"Skagen tip", 10.6, 57.74, 14, true},
		{"Paris", 2.3522, 48.8566, 10, false},
		{"Oslo", 10.75, 59.91, 10, false},
		{"just west of coverage", 7.9, 55.5, 14, false},
		{"just east of coverage", 15.7, 55.1, 14, false},
		{"just north of coverage", 10.5, 57.85, 14, false},
		{"just south of coverage", 10.0, 54.35, 14, false},
	}
	for _, tc := range cases {
		k := lonLatTile(tc.z, tc.lon, tc.lat)
		if got := k.IntersectsCoverage(); got != tc.want {
			t.Errorf("%s (%v): IntersectsCoverage = %v, want %v", tc.name, k, got, tc.want)
		}
	}
	// A z7 tile that only partly overlaps coverage still intersects.
	if k := lonLatTile(7, 7.0, 55.0); !k.IntersectsCoverage() {
		t.Errorf("z7 tile straddling the west edge (%v) does not intersect", k)
	}
}

// #682 review N3: LogEvery == 0 documents "0 logs all", including
// concurrent failures.
func TestClientLogEveryZeroLogsEveryFailure(t *testing.T) {
	logs := &logSink{}
	var tick atomic.Int64
	o := DefaultClientOptions()
	o.Logf = logs.logf
	o.LogEvery = 0
	o.Now = func() time.Time { return time.Unix(0, tick.Add(1)) }
	c := NewClient(Config{Enabled: true, Token: testToken}, o)
	const n = 400
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.LogFailure("x", FetchError{"timeout"}) }()
	}
	wg.Wait()
	if got := strings.Count(logs.String(), "\n"); got != n {
		t.Fatalf("%d of %d failures logged with LogEvery == 0", got, n)
	}
}

// #682 review N4: a non-positive bound must not make Put dereference an
// empty list.
func TestCacheNonPositiveBoundDoesNotPanic(t *testing.T) {
	now := time.Unix(0, 0)
	for _, maxEntries := range []int{0, -1} {
		c := NewCache[int](maxEntries, 10, time.Minute)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Put with maxEntries=%d panicked: %v", maxEntries, r)
				}
			}()
			c.Put(1, []byte("a"), now)
		}()
		if c.Len() != 0 {
			t.Fatalf("maxEntries=%d cached %d entries", maxEntries, c.Len())
		}
	}
}
