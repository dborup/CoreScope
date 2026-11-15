package main

// #737 M1 — Skråfoto: the oblique aerial photos of a map point, server
// side. Two endpoints, both off unless map.skraafoto.enabled and map.dhm
// carries a token:
//
//   - GET /api/skraafoto/search?lat=&lon=[&year=]: the photos of a point,
//     one per direction (north, east, south, west and nadir), from the
//     newest collection or the year asked for (2017 is not offered), with
//     the years there are. internal/skraafoto asks the STAC API with the
//     token, in one fixed query shape per direction.
//   - GET /api/skraafoto/preview/{collection}/{item}: one photo as a JPEG
//     of at most 1024 px on the long edge, cut from one overview of its
//     COG on the CDN by range reads. The CDN needs no token and is never
//     sent one.
//
// Properties, each pinned by skraafoto_proxy_737_test.go:
//   - lat, lon and year only, plain decimals, each at most once; a
//     preview path is a collection and a photo id of that year, with no
//     query string. Anything else is one fixed 400, before any upstream
//     call. Nothing from the request is echoed.
//   - A point outside Denmark (dhm.Covers) is answered empty without an
//     upstream call or a charge.
//   - Bounded caches: the year list and searches (24 h, the search point
//     snapped to 10 m), where each photo is (7 d), and previews within
//     cacheMB (7 d). Only successes are cached.
//   - Identical concurrent requests share one upstream call (dhm.Flight);
//     it stops once every requester has gone.
//   - Fair use of the operator's token: a cache miss is charged to the
//     client's per-IP budget (searchRateLimitPerMin 30, previewRateLimit-
//     PerMin 120) and to the server-wide budget of cold requests per UTC
//     day (dailyCap); 429 or 503 with Retry-After beyond them. At most
//     maxInFlight distinct upstream calls run at once (dhm.Admission);
//     beyond that 503 before any worker starts. A request refused by the
//     daily budget or by admission is given its per-IP charge back, and a
//     request that joins a running call its daily charge.
//   - The token is never in a response, a URL or a log line; failures are
//     logged by fixed category, at most once a minute.
//
// The endpoints never touch SQLite.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/skraafoto"
)

const (
	skraafotoSearchRoute  = "/api/skraafoto/search"
	skraafotoPreviewRoute = "/api/skraafoto/preview/{collection}/{item}"

	// skraafotoDailyCapDefault is the shipped daily budget of cold
	// requests, server-wide: a search is six STAC calls at most, a
	// preview one STAC call and a few CDN reads.
	skraafotoDailyCapDefault = 2000
	// skraafotoSnapM is the grid a search point is snapped to: photos
	// cover hundreds of metres, so clicks a few metres apart share one.
	skraafotoSnapM = 10
	// skraafotoSearchTTL and skraafotoPhotoTTL are the cache lifetimes;
	// the photos of a year do not change.
	skraafotoSearchTTL = 24 * time.Hour
	skraafotoPhotoTTL  = 7 * 24 * time.Hour
	// skraafotoWorkTimeout bounds one upstream call, all its requests
	// together.
	skraafotoWorkTimeout = 30 * time.Second
	skraafotoBusyRetry   = 5 * time.Second
	skraafotoLogEvery    = time.Minute

	skraafotoSearchEntries = 4096
	skraafotoSearchBytes   = 16 << 20
	skraafotoPhotoEntries  = 16384
)

// SkraafotoConfig is config.json `map.skraafoto` (#737), lifted out of Map
// like map.dhm. The token is map.dhm's. A nil field takes the default.
type SkraafotoConfig struct {
	Enabled                bool `json:"enabled"`
	SearchRateLimitPerMin  *int `json:"searchRateLimitPerMin"`
	PreviewRateLimitPerMin *int `json:"previewRateLimitPerMin"`
	// DailyCap is the server-wide budget of cold searches and previews
	// per UTC day; 0 is off.
	DailyCap    *int `json:"dailyCap"`
	MaxInFlight *int `json:"maxInFlight"`
	CacheMB     *int `json:"cacheMB"`
}

// extractSkraafotoConfig moves map.skraafoto into the typed field.
func (c *Config) extractSkraafotoConfig() {
	liftMapBlock(c, "skraafoto", "skraafoto", &c.Skraafoto)
}

// skraafotoSettings are map.skraafoto, resolved and bounded.
type skraafotoSettings struct {
	searchPerMin, previewPerMin int
	dailyCap                    int
	maxInFlight                 int
	cacheBytes                  int
}

func resolveSkraafotoSettings(sc SkraafotoConfig, logf func(string, ...interface{})) skraafotoSettings {
	if logf == nil {
		logf = log.Printf
	}
	k := configKnobs{tag: "[skraafoto]", block: "map.skraafoto", logf: logf}
	return skraafotoSettings{
		// The owner's fair-use budgets (#737 Q7). 0 is off, as for the
		// other per-client budgets.
		searchPerMin:  k.integer("searchRateLimitPerMin", sc.SearchRateLimitPerMin, 30, 0, 6000),
		previewPerMin: k.integer("previewRateLimitPerMin", sc.PreviewRateLimitPerMin, 120, 0, 6000),
		dailyCap:      k.integer("dailyCap", sc.DailyCap, skraafotoDailyCapDefault, 0, 1000000),
		maxInFlight:   k.integer("maxInFlight", sc.MaxInFlight, 8, 1, terrainMaxInFlightCap),
		cacheBytes:    k.integer("cacheMB", sc.CacheMB, 64, 1, 512) << 20,
	}
}

// ── Daily budget ───────────────────────────────────────────────────────────

// dailyBudget is a server-wide count of cold requests per UTC day. A nil
// *dailyBudget allows everything.
type dailyBudget struct {
	cap  int
	mu   sync.Mutex
	day  int64 // days since the epoch, UTC
	used int
}

// newDailyBudget returns nil when cap ≤ 0 (off).
func newDailyBudget(cap int) *dailyBudget {
	if cap <= 0 {
		return nil
	}
	return &dailyBudget{cap: cap}
}

func utcDay(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// take charges one request, or reports how long until the next UTC day.
func (b *dailyBudget) take(now time.Time) (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := utcDay(now); d != b.day {
		b.day, b.used = d, 0
	}
	if b.used >= b.cap {
		return false, time.Unix((b.day+1)*86400, 0).Sub(now)
	}
	b.used++
	return true, 0
}

// refund gives back a charge of the same day.
func (b *dailyBudget) refund(now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if utcDay(now) == b.day && b.used > 0 {
		b.used--
	}
}

// usedNow is the day's count.
func (b *dailyBudget) usedNow(now time.Time) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if utcDay(now) != b.day {
		return 0
	}
	return b.used
}

// ── Proxy ──────────────────────────────────────────────────────────────────

type skraafotoDeps struct {
	stac    *skraafoto.STAC
	cdn     *skraafoto.CDN
	logf    func(string, ...interface{})
	trusted []*net.IPNet
	now     func() time.Time
}

type skraafotoSearchKey struct {
	year int // 0: the newest
	e, n int // the snapped EPSG:25832 point
}

type skraafotoPhotoKey struct{ collection, id string }

type skraafotoProxy struct {
	stac     *skraafoto.STAC
	cdn      *skraafoto.CDN
	years    *dhm.Cache[struct{}]           // the year list, JSON
	searches *dhm.Cache[skraafotoSearchKey] // search responses, JSON
	paths    *dhm.Cache[skraafotoPhotoKey]  // where a photo is on the CDN
	previews *dhm.Cache[skraafotoPhotoKey]  // preview JPEGs
	flight   dhm.Flight[[]byte]

	searchLimiter, previewLimiter *ipRateLimiter // nil: no per-client limit
	daily                         *dailyBudget   // nil: no daily cap
	admit                         *dhm.Admission

	refused, failures  atomic.Int64
	refuseLog, failLog logThrottle
	capLog             logThrottle
	now                func() time.Time
	logf               func(string, ...interface{})
}

func newSkraafotoProxy(set skraafotoSettings, d skraafotoDeps) *skraafotoProxy {
	if d.stac == nil || d.cdn == nil {
		return nil
	}
	if d.logf == nil {
		d.logf = log.Printf
	}
	if d.now == nil {
		d.now = time.Now
	}
	return &skraafotoProxy{
		stac:     d.stac,
		cdn:      d.cdn,
		years:    dhm.NewCache[struct{}](1, 64<<10, skraafotoSearchTTL),
		searches: dhm.NewCache[skraafotoSearchKey](skraafotoSearchEntries, skraafotoSearchBytes, skraafotoSearchTTL),
		paths:    dhm.NewCache[skraafotoPhotoKey](skraafotoPhotoEntries, skraafotoPhotoEntries*160, skraafotoPhotoTTL),
		previews: dhm.NewCache[skraafotoPhotoKey](set.cacheBytes/(16<<10), set.cacheBytes, skraafotoPhotoTTL),
		searchLimiter: newIPRateLimiter("skraafoto", "map.skraafoto.searchRateLimitPerMin", set.searchPerMin,
			d.trusted, d.logf),
		previewLimiter: newIPRateLimiter("skraafoto", "map.skraafoto.previewRateLimitPerMin", set.previewPerMin,
			d.trusted, d.logf),
		daily: newDailyBudget(set.dailyCap),
		admit: dhm.NewAdmission(set.maxInFlight),
		now:   d.now,
		logf:  d.logf,
	}
}

// newSkraafotoProxyFromConfig returns nil unless map.skraafoto.enabled
// and map.dhm has a token.
func newSkraafotoProxyFromConfig(cfg *Config) *skraafotoProxy {
	if cfg == nil || !cfg.Skraafoto.Enabled {
		return nil
	}
	set := resolveSkraafotoSettings(cfg.Skraafoto, log.Printf)
	return newSkraafotoProxy(set, skraafotoDeps{
		stac:    skraafoto.NewSTAC(cfg.DHM.Token, skraafoto.Options{}),
		cdn:     skraafoto.NewCDN(skraafoto.Options{}),
		trusted: parseCIDRListQuiet(cfg.WSTrustedProxies()),
	})
}

// ── Responses ──────────────────────────────────────────────────────────────

// SkraafotoSearchResponse is the body of GET /api/skraafoto/search.
type SkraafotoSearchResponse struct {
	// Collection and Year are what was searched; "" and 0 outside
	// Denmark.
	Collection string `json:"collection"`
	Year       int    `json:"year"`
	// Years are the years offered, newest first.
	Years []int `json:"years"`
	// Photos holds at most one photo per direction, in the order north,
	// east, south, west, nadir; a direction without a photo is absent.
	Photos []SkraafotoPhoto `json:"photos"`
	Credit SkraafotoCredit  `json:"credit"`
}

// SkraafotoPhoto is one photo. No upstream URL is passed on: the preview
// is the server's, the viewer link the official viewer's.
type SkraafotoPhoto struct {
	ID         string `json:"id"`
	Collection string `json:"collection"`
	Direction  string `json:"direction"`
	Datetime   string `json:"datetime"` // RFC 3339, UTC
	// ViewerURL opens the photo at full resolution in the official
	// viewer (#737 Q9).
	ViewerURL string `json:"viewerUrl"`
	// PreviewURL is this server's preview; omitted when the photo's
	// asset is not where previews are read from.
	PreviewURL string `json:"previewUrl,omitempty"`
}

// SkraafotoCredit is the credit in its #733 parts: Attribution is the
// plain line, the dataset and licence marks inside it travel with their
// links.
type SkraafotoCredit struct {
	Attribution string `json:"attribution"`
	Owner       string `json:"owner"`
	DatasetName string `json:"datasetName"`
	DatasetURL  string `json:"datasetUrl"`
	LicenseName string `json:"licenseName"`
	LicenseURL  string `json:"licenseUrl"`
}

func skraafotoCredit() SkraafotoCredit {
	return SkraafotoCredit{
		Attribution: skraafoto.Attribution, Owner: skraafoto.Owner,
		DatasetName: skraafoto.DatasetName, DatasetURL: skraafoto.DatasetURL,
		LicenseName: skraafoto.LicenseName, LicenseURL: skraafoto.LicenseURL,
	}
}

// SkraafotoClientConfig is the `skraafoto` block of /api/config/client,
// present only while the proxy is active. Nothing secret.
type SkraafotoClientConfig struct {
	Enabled    bool   `json:"enabled"`
	SearchURL  string `json:"searchUrl"`
	PreviewURL string `json:"previewUrl"` // with {collection} and {item}
	MaxPx      int    `json:"maxPx"`
	// Note is the line shown with the photos (#737 Q10).
	Note   string          `json:"note"`
	Credit SkraafotoCredit `json:"credit"`
}

func (s *Server) skraafotoClientConfig() *SkraafotoClientConfig {
	if s.skraafoto == nil {
		return nil
	}
	return &SkraafotoClientConfig{
		Enabled: true, SearchURL: skraafotoSearchRoute, PreviewURL: skraafotoPreviewRoute,
		MaxPx: skraafoto.PreviewMaxPx, Note: skraafoto.PrivacyNote, Credit: skraafotoCredit(),
	}
}

// ── Requests ───────────────────────────────────────────────────────────────

var errSkraafotoRequest = errors.New("invalid skraafoto request")

var skraafotoYearRe = regexp.MustCompile(`^20[0-9]{2}$`)

// parseSkraafotoSearch accepts lat and lon, plain decimals in range, and
// optionally year, each exactly once. raw is the whole query string, so
// a pair url.Values would drop is refused too.
func parseSkraafotoSearch(raw string) (lat, lon float64, year int, err error) {
	q, err := url.ParseQuery(raw)
	if err != nil || strings.Contains("&"+raw+"&", "&&") {
		return 0, 0, 0, errSkraafotoRequest
	}
	keys := []string{"lat", "lon"}
	if _, ok := q["year"]; ok {
		keys = append(keys, "year")
	}
	v, err := dhm.ExactQuery(q, nil, keys...)
	if err != nil {
		return 0, 0, 0, errSkraafotoRequest
	}
	lat, ok1 := parseTerrainNumber(v["lat"], terrainCoordRe)
	lon, ok2 := parseTerrainNumber(v["lon"], terrainCoordRe)
	if !ok1 || !ok2 || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, 0, errSkraafotoRequest
	}
	if y, ok := v["year"]; ok {
		year, _ = strconv.Atoi(y)
		if _, valid := skraafoto.CollectionYear(skraafoto.Collection(year)); !skraafotoYearRe.MatchString(y) || !valid {
			return 0, 0, 0, errSkraafotoRequest
		}
	}
	return lat, lon, year, nil
}

func snapSkraafoto(v float64) int {
	return int(math.Round(v/skraafotoSnapM)) * skraafotoSnapM
}

// ── Handlers ───────────────────────────────────────────────────────────────

func (s *Server) handleSkraafotoSearch(w http.ResponseWriter, r *http.Request) {
	p := s.skraafoto
	if p == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	lat, lon, year, err := parseSkraafotoSearch(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, errSkraafotoRequest.Error())
		return
	}
	e, n := dhm.ToUTM32(lat, lon)
	if !dhm.Covers(e, n) {
		writeSkraafotoJSON(w, mustMarshal(SkraafotoSearchResponse{Years: []int{}, Photos: []SkraafotoPhoto{}, Credit: skraafotoCredit()}), "none")
		return
	}
	key := skraafotoSearchKey{year: year, e: snapSkraafoto(e), n: snapSkraafoto(n)}
	if year != 0 {
		if years, ok := p.cachedYears(); ok && !containsInt(years, year) {
			writeError(w, http.StatusBadRequest, errSkraafotoRequest.Error())
			return
		}
	}
	if data, ok := p.searches.Get(key, p.now()); ok {
		writeSkraafotoJSON(w, data, "hit")
		return
	}
	data, err := p.cold(r, p.searchLimiter, "s "+strconv.Itoa(key.year)+" "+strconv.Itoa(key.e)+" "+strconv.Itoa(key.n),
		func(ctx context.Context) ([]byte, error) { return p.search(ctx, key) })
	if err != nil {
		p.writeFetchError(w, r, p.searchLimiter, "search", err)
		return
	}
	writeSkraafotoJSON(w, data, "miss")
}

func (s *Server) handleSkraafotoPreview(w http.ResponseWriter, r *http.Request) {
	p := s.skraafoto
	if p == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	v := mux.Vars(r)
	key := skraafotoPhotoKey{collection: v["collection"], id: v["item"]}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || !skraafoto.ValidItem(key.collection, key.id) {
		writeError(w, http.StatusBadRequest, errSkraafotoRequest.Error())
		return
	}
	if data, ok := p.previews.Get(key, p.now()); ok {
		writeSkraafotoJPEG(w, data, "hit")
		return
	}
	data, err := p.cold(r, p.previewLimiter, "p "+key.collection+" "+key.id,
		func(ctx context.Context) ([]byte, error) { return p.preview(ctx, key) })
	if err != nil {
		p.writeFetchError(w, r, p.previewLimiter, "preview", err)
		return
	}
	writeSkraafotoJPEG(w, data, "miss")
}

func writeSkraafotoJSON(w http.ResponseWriter, data []byte, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-CoreScope-Cache", state)
	w.Write(data)
}

func writeSkraafotoJPEG(w http.ResponseWriter, data []byte, state string) {
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-CoreScope-Cache", state)
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(skraafotoPhotoTTL/time.Second)))
	w.Write(data)
}

// skraafotoCapped is a refusal by the daily budget.
type skraafotoCapped struct{ retry time.Duration }

func (skraafotoCapped) Error() string { return "skraafoto daily budget spent" }

// cold runs fn, a cache miss, as one shared upstream call of key: charged
// to the client's budget in lim and to the daily budget first, then
// admitted. Refusals after the per-client charge give it back; a caller
// that joins a running call gives its daily charge back.
func (p *skraafotoProxy) cold(r *http.Request, lim *ipRateLimiter, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	if ok, retry := lim.allow(r); !ok {
		return nil, rateLimited{retry}
	}
	if ok, retry := p.daily.take(p.now()); !ok {
		lim.refund(r)
		return nil, skraafotoCapped{retry}
	}
	data, shared, err := p.flight.DoAdmitted(r.Context(), key, skraafotoWorkTimeout, p.admit, fn)
	if shared || errors.Is(err, dhm.ErrAdmissionFull) {
		p.daily.refund(p.now())
	}
	return data, err
}

// cachedYears is the cached year list.
func (p *skraafotoProxy) cachedYears() ([]int, bool) {
	data, ok := p.years.Get(struct{}{}, p.now())
	if !ok {
		return nil, false
	}
	var years []int
	return years, json.Unmarshal(data, &years) == nil
}

// yearList is the year list, cached or from STAC; concurrent searches
// share one fetch. It runs inside an admitted search, so it takes no
// admission slot of its own.
func (p *skraafotoProxy) yearList(ctx context.Context) ([]int, error) {
	if years, ok := p.cachedYears(); ok {
		return years, nil
	}
	data, _, err := p.flight.Do(ctx, "years", skraafotoWorkTimeout, func(ctx context.Context) ([]byte, error) {
		years, err := p.stac.Years(ctx)
		if err != nil {
			return nil, err
		}
		data := mustMarshal(years)
		p.years.Put(struct{}{}, data, p.now())
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	var years []int
	if err := json.Unmarshal(data, &years); err != nil {
		return nil, err
	}
	return years, nil
}

// errSkraafotoYear is a year the API does not have.
var errSkraafotoYear = errors.New("unknown year")

// search runs a cold search: the year list, then one STAC search per
// direction, concurrently (the client bounds them). Any failure fails it.
func (p *skraafotoProxy) search(ctx context.Context, key skraafotoSearchKey) ([]byte, error) {
	years, err := p.yearList(ctx)
	if err != nil {
		return nil, err
	}
	year := key.year
	if year == 0 {
		year = years[0]
	} else if !containsInt(years, year) {
		return nil, errSkraafotoYear
	}
	items := make([]*skraafoto.Item, len(skraafoto.Directions))
	errs := make([]error, len(skraafoto.Directions))
	var wg sync.WaitGroup
	for i, d := range skraafoto.Directions {
		wg.Add(1)
		go func(i int, d skraafoto.Direction) {
			defer wg.Done()
			items[i], errs[i] = p.stac.Search(ctx, year, key.e, key.n, d)
		}(i, d)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	resp := SkraafotoSearchResponse{Collection: skraafoto.Collection(year), Year: year, Years: years,
		Photos: make([]SkraafotoPhoto, 0, len(items)), Credit: skraafotoCredit()}
	now := p.now()
	for _, it := range items {
		if it == nil {
			continue
		}
		ph := SkraafotoPhoto{ID: it.ID, Collection: it.Collection, Direction: string(it.Direction),
			Datetime: it.Datetime.Format(time.RFC3339), ViewerURL: skraafoto.ViewerLink(it.ID)}
		if it.Path != "" {
			ph.PreviewURL = "/api/skraafoto/preview/" + it.Collection + "/" + it.ID
			p.paths.Put(skraafotoPhotoKey{it.Collection, it.ID}, []byte(it.Path), now)
		}
		resp.Photos = append(resp.Photos, ph)
	}
	data := mustMarshal(resp)
	p.searches.Put(key, data, now)
	return data, nil
}

// preview runs a cold preview: where the photo is (cached by a search,
// or one STAC lookup), then its preview from the CDN.
func (p *skraafotoProxy) preview(ctx context.Context, key skraafotoPhotoKey) ([]byte, error) {
	var path string
	if b, ok := p.paths.Get(key, p.now()); ok {
		path = string(b)
	} else {
		it, err := p.stac.Item(ctx, key.collection, key.id)
		if err != nil {
			return nil, err
		}
		path = it.Path
		p.paths.Put(key, []byte(path), p.now())
	}
	data, err := p.cdn.Preview(ctx, path)
	if err != nil {
		return nil, err
	}
	p.previews.Put(key, data, p.now())
	return data, nil
}

// writeFetchError maps a failed cold request. None carries upstream text.
func (p *skraafotoProxy) writeFetchError(w http.ResponseWriter, r *http.Request, lim *ipRateLimiter, what string, err error) {
	var rl rateLimited
	var capped skraafotoCapped
	switch {
	case errors.As(err, &rl):
		writeRateLimited(w, rl.retry)
		return
	case errors.As(err, &capped):
		if p.capLog.allow(p.now(), skraafotoLogEvery) {
			p.logf("[skraafoto] daily budget of map.skraafoto.dailyCap cold requests is spent; serving cached photos only until 00:00 UTC")
		}
		setRetryAfter(w, capped.retry)
		writeError(w, http.StatusServiceUnavailable, capped.Error())
		return
	case r.Context().Err() != nil:
		return // the client left
	case errors.Is(err, dhm.ErrAdmissionFull):
		lim.refund(r)
		if n, ok := p.refuseLog.note(p.now(), p.refused.Add(1), skraafotoLogEvery); ok {
			p.logf("[skraafoto] admission: refused %d request(s) since the last report because map.skraafoto.maxInFlight (%d) upstream calls are running", n, p.admit.Cap())
		}
		setRetryAfter(w, skraafotoBusyRetry)
		writeError(w, http.StatusServiceUnavailable, "skraafoto busy")
		return
	case errors.Is(err, errSkraafotoYear):
		writeError(w, http.StatusBadRequest, errSkraafotoRequest.Error())
		return
	case dhm.FailureReason(err) == "status 404":
		writeError(w, http.StatusNotFound, "photo not found")
		return
	}
	if n, ok := p.failLog.note(p.now(), p.failures.Add(1), skraafotoLogEvery); ok {
		p.logf("[skraafoto] upstream %s failed (%s); %d failure(s) since the last report", what, dhm.FailureReason(err), n)
	}
	if dhm.FailureReason(err) == "timeout" || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "skraafoto "+what+" timed out")
		return
	}
	writeError(w, http.StatusBadGateway, "skraafoto "+what+" unavailable")
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// mustMarshal encodes a value whose type always encodes.
func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
