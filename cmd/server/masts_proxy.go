package main

// #724 M2 — GET /api/masts?cell=E,N: the antenna sites of one 2 km × 2 km
// EPSG:25832 grid cell from Mastedatabasen (Digitaliseringsstyrelsen).
//
// The browser asks for fixed grid cells, never for a bbox, so one cached
// cell serves every viewer. The server fetches a cell from the documented
// WFS only when a viewer's map needs it and it is not cached: there is no
// prefetch, no background walk and no way to request anything but one
// cell, because the catalogue record forbids scraping (#724 plan, owner
// decisions). The upstream client, the cell math and the GML parser are
// internal/mastedb (M1).
//
// Properties, each pinned by masts_proxy_724_test.go:
//   - Not an open proxy. The only query key is `cell`, two plain decimal
//     integers inside the Danish extent (mastedb.Cell.Valid); anything else
//     is 400 with a fixed body, before any cache, budget or upstream work.
//     The upstream URL and query are fixed in internal/mastedb; nothing an
//     operator or a client sets can point it elsewhere.
//   - Bounded LRU cache of encoded responses (entries, bytes, 7-day TTL,
//     short against the quarterly updates). Only successes are cached.
//   - Concurrent misses for one cell share one fetch (dhm.Flight), which
//     stops once every requester has gone or after mastsDeadline.
//   - A miss is charged to the client's per-IP budget
//     (map.masts.cellRateLimitPerMin, ipRateLimiter, #690/#702); a hit is
//     free. At most map.masts.maxInFlight distinct cell fetches are
//     admitted server-wide (dhm.Admission, #744); beyond that a miss is
//     503 with Retry-After before any upstream work, and its charge is
//     refunded (#705). Of the admitted fetches, at most mastsUpstreamSlots
//     talk to the WFS at once; one that gets no slot before mastsDeadline
//     did no upstream work either, so it is the same refunded 503 (#753
//     review N2). The browser paces its cell requests (public/masts-layer.js)
//     so one viewer stays well inside maxInFlight.
//   - Fail closed: an upstream failure or anything but a well-formed
//     MasteIBrug page is 502 (504 on a timeout) with a fixed body and no
//     upstream text, logged by category at most once per minute.
//
// The endpoint never touches SQLite.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/mastedb"
)

const (
	mastsClientPath = "/api/masts"
	// mastsMinZoom is the plan's zoom gate: a desktop viewport at z13 is
	// about 70 cells. The browser enforces it (M3); the client config
	// carries it so the browser does not hard-code it. It is also the
	// lowest map.masts.minZoom: the browser's view cap is paced against
	// the per-IP budget at this zoom (#758 review F2), so an operator may
	// only raise it, up to mastsMinZoomMax (#724 M5).
	mastsMinZoom    = 13
	mastsMinZoomMax = 18
	// mastsHintDistanceDefault and mastsHintStrongDefault are the profile
	// hint's distance and strong tier (#724 owner decision 2), carried in
	// the client block as map.masts.hintDistanceM / hintStrongM (#724 M5).
	// The distance stays within half a 2 km cell less the browser's 10 m
	// pad (public/masts-hint.js CELL_PAD_M), so each end of a profile pins
	// at most four cells (public/masts-layer.js PIN_CELL_CAP). The strong
	// tier is 0 (off) up to the distance.
	mastsHintDistanceDefault = 500
	mastsHintDistanceMin     = 100
	mastsHintDistanceMax     = 990
	mastsHintStrongDefault   = 100
	// mastsCacheTTL: 7 days, short against the quarterly upstream updates.
	mastsCacheTTL = 7 * 24 * time.Hour
	// mastsBrowserMaxAge is how long a browser may reuse a cell.
	mastsBrowserMaxAge = 24 * time.Hour
	// mastsUpstreamSlots is the plan's global cap on requests open towards
	// the WFS at once. Admitted fetches beyond it wait for a slot.
	mastsUpstreamSlots = 2
	// mastsDeadline bounds one fetch, waiting for an upstream slot
	// included: a few upstream timeouts (mastedb: 15 s each).
	mastsDeadline = 30 * time.Second
	// mastsBusyRetry is the Retry-After of a 503 admission refusal.
	mastsBusyRetry = 5 * time.Second
	// mastsLogEvery throttles the failure and refusal log lines.
	mastsLogEvery = time.Minute

	// mastsCellRateLimitDefault is the plan's per-client budget of cells
	// that miss the cache, per minute. A first look at z13 is about 70
	// cells, at z14 about 18; cached cells are free.
	mastsCellRateLimitDefault = 120
	// mastsCellRateLimitMax bounds map.masts.cellRateLimitPerMin like
	// profile.rateLimitPerMin (0-6000).
	mastsCellRateLimitMax = 6000
	// mastsMaxInFlightDefault is how many distinct cell fetches may be
	// admitted (waiting for or holding an upstream slot) server-wide: one
	// z14 viewport's worth. Each costs a goroutine and at most one parsed
	// page while it waits.
	mastsMaxInFlightDefault = 16
	mastsMaxInFlightCap     = 64
)

// errMastsBadRequest is the single 400 body. It never carries client input.
var errMastsBadRequest = errors.New("invalid masts request")

// errMastsBusy is the 503 body: the fetch was refused admission, or was
// admitted but never got an upstream slot. Neither reached upstream.
var errMastsBusy = errors.New("antenna data busy")

// errMastsStarved is an admitted fetch that waited for an upstream slot
// until its deadline.
var errMastsStarved = errors.New("masts: no upstream slot before the deadline")

// mastsCellRe is "E,N" in plain decimal: no sign, no leading zero, no
// spaces, no exponent. Six digits is ample for the extent and keeps Atoi
// from overflowing.
var mastsCellRe = regexp.MustCompile(`^[1-9][0-9]{0,5},[1-9][0-9]{0,5}$`)

// MastsConfig is config.json `map.masts` (Config.Masts). Default off. It
// has no upstream setting: the WFS is fixed in internal/mastedb.
type MastsConfig struct {
	Enabled bool `json:"enabled"`
	// CellRateLimitPerMin is the per-client budget of uncached cells per
	// minute; nil is the default, 0 is off.
	CellRateLimitPerMin *int `json:"cellRateLimitPerMin,omitempty"`
	// MaxInFlight bounds the distinct cell fetches admitted server-wide.
	MaxInFlight *int `json:"maxInFlight,omitempty"`
	// MinZoom, HintDistanceM and HintStrongM are the operator's values of
	// the browser settings the customizer exposes (#724 M5); nil is the
	// default. See resolveMastsClientSettings.
	MinZoom       *int `json:"minZoom,omitempty"`
	HintDistanceM *int `json:"hintDistanceM,omitempty"`
	HintStrongM   *int `json:"hintStrongM,omitempty"`
}

// MastsClientConfig is the `masts` block of /api/config/client. Present
// only while map.masts.enabled is true; it carries fixed values only, no
// limit and no upstream address.
type MastsClientConfig struct {
	Enabled     bool   `json:"enabled"`
	URL         string `json:"url"`
	MinZoom     int    `json:"minZoom"`
	CellSizeM   int    `json:"cellSizeM"`
	Attribution string `json:"attribution"`
	SourceURL   string `json:"sourceUrl"`
	// CellExtent is the valid cell index range [minE, minN, maxE, maxN]
	// (mastedb.Cell.Valid), so the browser never asks for a cell the
	// server would refuse.
	CellExtent [4]int `json:"cellExtent"`
	// HintDistanceM and HintStrongM are the profile hint's distance and
	// strong tier in metres (#724 M5), the customizer's starting point.
	HintDistanceM int `json:"hintDistanceM"`
	HintStrongM   int `json:"hintStrongM"`
}

// MastsCellResponse is the body of GET /api/masts.
type MastsCellResponse struct {
	Cell mastedb.Cell `json:"cell"`
	// Truncated is set when upstream returned its page limit (500 rows) or
	// said more matched, so the cell may be missing sites.
	Truncated bool           `json:"truncated"`
	Sites     []mastedb.Site `json:"sites"`
}

// extractMastsConfig moves map.masts into the typed Masts field, like
// map.dhm and map.heritage, so the browser receives only the fixed client
// block and never the raw config.
func (c *Config) extractMastsConfig() {
	liftMapBlock(c, "masts", "antenna sites layer", &c.Masts)
}

// mastsCellRateLimit is map.masts.cellRateLimitPerMin: unset is the
// default, 0 is off, and anything outside [0, mastsCellRateLimitMax] warns
// and takes the default, like map.dhm.tileRateLimitPerMin.
func mastsCellRateLimit(c MastsConfig, logf func(string, ...interface{})) int {
	if c.CellRateLimitPerMin == nil {
		return mastsCellRateLimitDefault
	}
	if v := *c.CellRateLimitPerMin; v >= 0 && v <= mastsCellRateLimitMax {
		return v
	}
	logf("[masts] config map.masts.cellRateLimitPerMin is out of range (0-%d); using %d", mastsCellRateLimitMax, mastsCellRateLimitDefault)
	return mastsCellRateLimitDefault
}

// mastsMaxInFlight is map.masts.maxInFlight, 1-mastsMaxInFlightCap.
func mastsMaxInFlight(c MastsConfig, logf func(string, ...interface{})) int {
	if c.MaxInFlight == nil {
		return mastsMaxInFlightDefault
	}
	if v := *c.MaxInFlight; v >= 1 && v <= mastsMaxInFlightCap {
		return v
	}
	logf("[masts] config map.masts.maxInFlight is out of range (1-%d); using %d", mastsMaxInFlightCap, mastsMaxInFlightDefault)
	return mastsMaxInFlightDefault
}

// mastsClientSettings are the browser-side values of map.masts, resolved
// and bounded: the zoom gate and the profile hint's two distances.
type mastsClientSettings struct {
	minZoom       int
	hintDistanceM int
	hintStrongM   int
}

func defaultMastsClientSettings() mastsClientSettings {
	return mastsClientSettings{minZoom: mastsMinZoom, hintDistanceM: mastsHintDistanceDefault, hintStrongM: mastsHintStrongDefault}
}

// resolveMastsClientSettings applies the defaults and bounds, like
// map.dhm.profile.rateLimitPerMin: an out-of-range value warns and takes
// the default. A strong tier past the (resolved) distance is out of range
// too; the default strong tier is within every allowed distance.
func resolveMastsClientSettings(c MastsConfig, logf func(string, ...interface{})) mastsClientSettings {
	k := configKnobs{tag: "[masts]", block: "map.masts", logf: logf}
	s := mastsClientSettings{
		minZoom:       k.integer("minZoom", c.MinZoom, mastsMinZoom, mastsMinZoom, mastsMinZoomMax),
		hintDistanceM: k.integer("hintDistanceM", c.HintDistanceM, mastsHintDistanceDefault, mastsHintDistanceMin, mastsHintDistanceMax),
	}
	s.hintStrongM = k.integer("hintStrongM", c.HintStrongM, mastsHintStrongDefault, 0, s.hintDistanceM)
	return s
}

// ── Proxy ──────────────────────────────────────────────────────────────────

type mastsProxyOptions struct {
	client          mastedb.ClientOptions
	cacheMaxEntries int
	cacheMaxBytes   int
	cacheTTL        time.Duration
	rateLimitPerMin int // 0: off
	maxInFlight     int
	view            mastsClientSettings
	trustedProxies  []*net.IPNet
	logf            func(string, ...interface{})
	now             func() time.Time
}

// defaultMastsProxyOptions are the production limits. A full 500-row cell
// encodes to about 75 KB (TestMastsDefaultCacheBounds), a typical one to a
// few KB, so 4096 cells fit in the 32 MiB byte bound in practice and the
// byte bound holds the worst case.
func defaultMastsProxyOptions() mastsProxyOptions {
	return mastsProxyOptions{
		client:          mastedb.DefaultClientOptions(),
		cacheMaxEntries: 4096,
		cacheMaxBytes:   32 << 20,
		cacheTTL:        mastsCacheTTL,
		rateLimitPerMin: mastsCellRateLimitDefault,
		maxInFlight:     mastsMaxInFlightDefault,
		view:            defaultMastsClientSettings(),
		logf:            log.Printf,
		now:             time.Now,
	}
}

type mastsProxy struct {
	client   *mastedb.Client
	cache    *dhm.Cache[mastedb.Cell]
	flight   dhm.Flight[[]byte]
	limiter  *ipRateLimiter // nil: no per-client limit
	admit    *dhm.Admission // admitted fetches (maxInFlight)
	upstream chan struct{}  // one token per open upstream request
	deadline time.Duration
	view     mastsClientSettings
	now      func() time.Time
	logf     func(string, ...interface{})

	failLog    logThrottle
	refuseLog  logThrottle
	starveLog  logThrottle
	refusedAll atomic.Int64 // admission refusals (503) since start
	starvedAll atomic.Int64 // starved fetches (503) since start
}

// newMastsProxy returns nil unless map.masts is enabled: the layer is then
// not offered and the endpoint is 404.
func newMastsProxy(cfg MastsConfig, opts mastsProxyOptions) *mastsProxy {
	if !cfg.Enabled {
		return nil
	}
	if opts.logf == nil {
		opts.logf = log.Printf
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return &mastsProxy{
		client:   mastedb.NewClient(opts.client),
		cache:    dhm.NewCache[mastedb.Cell](opts.cacheMaxEntries, opts.cacheMaxBytes, opts.cacheTTL),
		limiter:  newIPRateLimiter("masts", "map.masts.cellRateLimitPerMin", opts.rateLimitPerMin, opts.trustedProxies, opts.logf),
		admit:    dhm.NewAdmission(opts.maxInFlight),
		upstream: make(chan struct{}, mastsUpstreamSlots),
		deadline: mastsDeadline,
		view:     opts.view,
		now:      opts.now,
		logf:     opts.logf,
	}
}

func newMastsProxyFromConfig(cfg *Config) *mastsProxy {
	if cfg == nil || !cfg.Masts.Enabled {
		return nil
	}
	opts := defaultMastsProxyOptions()
	opts.rateLimitPerMin = mastsCellRateLimit(cfg.Masts, opts.logf)
	opts.maxInFlight = mastsMaxInFlight(cfg.Masts, opts.logf)
	opts.view = resolveMastsClientSettings(cfg.Masts, opts.logf)
	opts.trustedProxies = parseCIDRListQuiet(cfg.WSTrustedProxies())
	return newMastsProxy(cfg.Masts, opts)
}

// parseMastsQuery accepts exactly one key, `cell`, holding a valid cell.
func parseMastsQuery(q url.Values) (mastedb.Cell, error) {
	free, err := dhm.ExactQuery(q, nil, "cell")
	if err != nil || !mastsCellRe.MatchString(free["cell"]) {
		return mastedb.Cell{}, errMastsBadRequest
	}
	es, ns, _ := strings.Cut(free["cell"], ",")
	e, errE := strconv.Atoi(es)
	n, errN := strconv.Atoi(ns)
	c := mastedb.Cell{E: e, N: n}
	if errE != nil || errN != nil || !c.Valid() {
		return mastedb.Cell{}, errMastsBadRequest
	}
	return c, nil
}

// encodeMastsCell is the response body for a parsed page.
func encodeMastsCell(cell mastedb.Cell, page mastedb.Page) ([]byte, error) {
	return json.Marshal(MastsCellResponse{Cell: cell, Truncated: page.Truncated, Sites: mastedb.Sites(page.Rows)})
}

func (p *mastsProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cell, err := parseMastsQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, errMastsBadRequest.Error())
		return
	}
	if data, ok := p.cache.Get(cell, p.now()); ok {
		writeMastsCell(w, data, "hit")
		return
	}
	// Only a cell that will go upstream is charged to the client.
	if ok, retry := p.limiter.allow(r); !ok {
		writeRateLimited(w, retry)
		return
	}
	key := strconv.Itoa(cell.E) + "," + strconv.Itoa(cell.N)
	// Admission is per fetch, not per request: callers that join a
	// running fetch add no work and take no slot.
	data, _, err := p.flight.DoAdmitted(r.Context(), key, p.deadline, p.admit, func(ctx context.Context) ([]byte, error) {
		select {
		case p.upstream <- struct{}{}:
			defer func() { <-p.upstream }()
		case <-ctx.Done():
			return nil, errMastsStarved
		}
		page, err := p.client.FetchCell(ctx, cell)
		if err != nil {
			return nil, err
		}
		enc, err := encodeMastsCell(cell, page)
		if err != nil {
			return nil, dhm.FetchError{Reason: "encode"}
		}
		p.cache.Put(cell, enc, p.now())
		return enc, nil
	})
	if err != nil {
		p.writeFetchError(w, r, err)
		return
	}
	writeMastsCell(w, data, "miss")
}

func writeMastsCell(w http.ResponseWriter, data []byte, state string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(mastsBrowserMaxAge/time.Second)))
	h.Set("X-CoreScope-Cache", state)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// writeFetchError maps a failed fetch: a client that left gets nothing, a
// refusal is 503, a timeout 504 and anything else 502. None carries
// upstream text; the log line carries the category only.
func (p *mastsProxy) writeFetchError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, dhm.ErrAdmissionFull) || errors.Is(err, errMastsStarved) {
		// Neither did upstream work: give the per-IP charge back, so a
		// client that retries as told never reaches 429 through 503s.
		p.limiter.refund(r)
		p.logBusy(errors.Is(err, errMastsStarved))
		setRetryAfter(w, mastsBusyRetry)
		writeError(w, http.StatusServiceUnavailable, errMastsBusy.Error())
		return
	}
	timeout := dhm.FailureReason(err) == "timeout" || errors.Is(err, context.DeadlineExceeded)
	reason := dhm.FailureReason(err)
	if timeout {
		reason = "timeout"
	}
	if p.failLog.allow(p.now(), mastsLogEvery) {
		p.logf("[masts] upstream cell failed (%s)", reason)
	}
	if timeout {
		writeError(w, http.StatusGatewayTimeout, "antenna data timed out")
		return
	}
	writeError(w, http.StatusBadGateway, "antenna data unavailable")
}

// logBusy reports 503s at most once per interval per cause. The two
// causes have different knobs (#758 review F5): a refusal means
// map.masts.maxInFlight fetches were admitted; a starved fetch was
// admitted with room to spare but waited out its deadline behind the
// mastsUpstreamSlots open WFS requests, which raising maxInFlight would
// only lengthen.
func (p *mastsProxy) logBusy(starved bool) {
	if starved {
		if n, ok := p.starveLog.note(p.now(), p.starvedAll.Add(1), mastsLogEvery); ok {
			p.logf("[masts] busy: answered %d request(s) with 503 since the last report because admitted fetches waited %s for one of the %d upstream slots; upstream is slow, and raising maxInFlight would not help", n, p.deadline, mastsUpstreamSlots)
		}
		return
	}
	if n, ok := p.refuseLog.note(p.now(), p.refusedAll.Add(1), mastsLogEvery); ok {
		p.logf("[masts] busy: answered %d request(s) with 503 since the last report because map.masts.maxInFlight (%d) fetches were admitted; raise maxInFlight if this persists", n, p.admit.Cap())
	}
}

func (s *Server) mastsClientConfig() *MastsClientConfig {
	if s.masts == nil {
		return nil
	}
	return &MastsClientConfig{
		Enabled:       true,
		URL:           mastsClientPath,
		MinZoom:       s.masts.view.minZoom,
		CellSizeM:     mastedb.CellSizeM,
		Attribution:   mastedb.Attribution,
		SourceURL:     mastedb.SourceURL,
		CellExtent:    [4]int{mastedb.MinCellE, mastedb.MinCellN, mastedb.MaxCellE, mastedb.MaxCellN},
		HintDistanceM: s.masts.view.hintDistanceM,
		HintStrongM:   s.masts.view.hintStrongM,
	}
}

func (s *Server) handleMasts(w http.ResponseWriter, r *http.Request) {
	if s.masts == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.masts.ServeHTTP(w, r)
}
