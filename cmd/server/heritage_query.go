package main

// #723 M2 — GET /api/terrain/heritage?lat=&lon=: what the protected
// ancient monuments register says about one map point. The Map and Live
// pages ask it when a viewer clicks the map with the monuments overlay on
// (public/heritage-info.js); the terrain profile warning (#723 M3) will
// ask it for its endpoints.
//
// The answer is the protection zone holding the point, or else the
// nearest one within map.heritage.radiusM: its anlaegstype, datering,
// fred_status, frednr and systemnr exactly as the dataset has them, the
// distance to its edge and whether the point lies inside it. Nothing from
// the request is echoed. internal/heritage asks the agency's WFS for one
// fixed 250 m cell grown by the radius and computes the rest locally.
//
// Properties, each pinned by heritage_query_723_test.go:
//   - lat and lon only, plain decimals in range, each exactly once; any
//     other query string, a malformed or empty pair included, is one
//     fixed 400 and makes no upstream call.
//   - A point outside the layers' extent is answered empty without an
//     upstream call or a charge.
//   - Bounded LRU cache per cell (map.heritage.cacheEntries, TTL
//     cacheTTLHours): every click inside one cell shares one upstream call.
//   - Identical concurrent cells share one call (dhm.Flight), which stops
//     when every requester has gone, freeing its slot.
//   - A cache miss is charged to the client's per-IP budget
//     (rateLimitPerMin, ip_rate_limit.go) before it goes upstream: 429
//     with Retry-After beyond it.
//   - At most maxInFlight distinct upstream calls run at once, server-wide
//     (dhm.Admission, #744); beyond that 503 with Retry-After before a
//     worker starts or any upstream call, and the per-IP charge is given
//     back (#696/#707).
//   - Upstream failures are 502, timeouts 504, never with upstream text,
//     and logged by category at most once a minute. Failures are not
//     cached, and every refusal (429, 502, 503, 504) is no-store.
//
// The register is free to use under the agency's terms, which forbid
// harvesting it; the fixed cells, the per-IP budget and the global bound
// are what keep this endpoint from being a way to do that.
//
// The endpoint never touches SQLite.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/heritage"
)

const (
	// heritageBusyRetry is the Retry-After of a 503 when maxInFlight
	// upstream calls are already running.
	heritageBusyRetry = 5 * time.Second
	// heritageLogEvery throttles the refusal and failure log lines.
	heritageLogEvery = time.Minute
	// heritageCacheMaxBytes bounds the cache whatever cacheEntries says.
	// An entry is one cell's zones: a few KB normally, at most about
	// 1.3 MB (heritage.MaxVertices encoded).
	heritageCacheMaxBytes = 32 << 20
)

// ── Settings ───────────────────────────────────────────────────────────────

// heritageSettings are map.heritage, resolved and bounded.
type heritageSettings struct {
	radiusM         int
	cacheEntries    int
	cacheTTL        time.Duration
	upstreamTimeout time.Duration
	rateLimitPerMin int
	maxInFlight     int
}

// resolveHeritageSettings applies the defaults and bounds, with the same
// names and ranges as map.dhm.profile where they mean the same thing. An
// out-of-range value logs a warning and takes the default.
func resolveHeritageSettings(hc HeritageConfig, logf func(string, ...interface{})) heritageSettings {
	if logf == nil {
		logf = log.Printf
	}
	k := configKnobs{tag: "[heritage]", block: "map.heritage", logf: logf}
	return heritageSettings{
		// The query box is the cell grown by the radius, so the radius is
		// bounded by the widest box internal/heritage builds.
		radiusM:      k.integer("radiusM", hc.RadiusM, 100, 0, heritage.MaxMarginM),
		cacheEntries: k.integer("cacheEntries", hc.CacheEntries, 256, 1, 4096),
		// Protection zones change on a legal timescale, so the default is
		// the longest TTL the profile allows.
		cacheTTL:        time.Duration(k.num("cacheTTLHours", hc.CacheTTLHours, 168, 0, 168, true) * float64(time.Hour)),
		upstreamTimeout: time.Duration(k.integer("upstreamTimeoutMs", hc.UpstreamTimeoutMs, 10000, 500, 10000)) * time.Millisecond,
		// One click costs one upstream request, against up to 32 for a
		// profile, so twice the profile's 10: a viewer clicking from
		// monument to monument does not meet it.
		rateLimitPerMin: k.integer("rateLimitPerMin", hc.RateLimitPerMin, 20, 0, 6000),
		maxInFlight:     k.integer("maxInFlight", hc.MaxInFlight, 4, 1, terrainMaxInFlightCap),
	}
}

// ── Query ──────────────────────────────────────────────────────────────────

// heritageSource is the WFS client (heritage.Client), or a fake in tests.
type heritageSource interface {
	FetchCell(ctx context.Context, c heritage.Cell) (heritage.Page, error)
}

type heritageQuery struct {
	src       heritageSource
	set       heritageSettings
	cache     *dhm.Cache[heritage.Cell]
	flight    dhm.Flight[[]byte]
	limiter   *ipRateLimiter // nil: no per-client limit
	admit     *dhm.Admission // maxInFlight upstream calls; refusals are 503
	refused   atomic.Int64   // admission refusals (503) since start
	failures  atomic.Int64   // upstream failures since start
	refuseLog logThrottle
	failLog   logThrottle
	now       func() time.Time
	logf      func(string, ...interface{})
}

type heritageQueryDeps struct {
	src     heritageSource
	logf    func(string, ...interface{})
	trusted []*net.IPNet
}

func newHeritageQuery(set heritageSettings, d heritageQueryDeps) *heritageQuery {
	if d.src == nil {
		return nil
	}
	if d.logf == nil {
		d.logf = log.Printf
	}
	return &heritageQuery{
		src:     d.src,
		set:     set,
		cache:   dhm.NewCache[heritage.Cell](set.cacheEntries, heritageCacheMaxBytes, set.cacheTTL),
		limiter: newIPRateLimiter("heritage", "map.heritage.rateLimitPerMin", set.rateLimitPerMin, d.trusted, d.logf),
		admit:   dhm.NewAdmission(set.maxInFlight),
		now:     time.Now,
		logf:    d.logf,
	}
}

// newHeritageQueryFromConfig returns nil unless map.heritage is enabled:
// the same switch offers the overlay and this endpoint.
func newHeritageQueryFromConfig(cfg *Config) *heritageQuery {
	if cfg == nil || !cfg.Heritage.Enabled {
		return nil
	}
	set := resolveHeritageSettings(cfg.Heritage, log.Printf)
	opts := heritage.DefaultClientOptions()
	opts.Timeout, opts.MarginM = set.upstreamTimeout, set.radiusM
	client, err := heritage.NewClient(opts)
	if err != nil { // radiusM is bounded above, so this cannot happen
		log.Printf("[heritage] query disabled: %v", err)
		return nil
	}
	return newHeritageQuery(set, heritageQueryDeps{src: client, trusted: parseCIDRListQuiet(cfg.WSTrustedProxies())})
}

// ── Request and response ───────────────────────────────────────────────────

var errHeritageQuery = errors.New("invalid heritage request")

// errHeritageBusy is the 503 body: maxInFlight upstream calls are
// already running.
var errHeritageBusy = errors.New("heritage lookups busy")

// parseHeritageQuery accepts exactly lat and lon, each once, as plain
// decimals (the terrain profile's coordinate grammar) in range. raw is the
// whole query string: a pair url.Values would drop (bad escape,
// semicolon, empty) is refused too.
func parseHeritageQuery(raw string) (lat, lon float64, err error) {
	q, err := url.ParseQuery(raw)
	if err != nil || strings.Contains("&"+raw+"&", "&&") {
		return 0, 0, errHeritageQuery
	}
	v, err := dhm.ExactQuery(q, nil, "lat", "lon")
	if err != nil {
		return 0, 0, errHeritageQuery
	}
	lat, ok1 := parseTerrainNumber(v["lat"], terrainCoordRe)
	lon, ok2 := parseTerrainNumber(v["lon"], terrainCoordRe)
	if !ok1 || !ok2 || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, errHeritageQuery
	}
	return lat, lon, nil
}

// HeritageInfoResponse is the body of GET /api/terrain/heritage.
type HeritageInfoResponse struct {
	RadiusM int `json:"radiusM"` // map.heritage.radiusM
	// Count is the zones holding the point or within RadiusM of it.
	Count int `json:"count"`
	// Truncated: the cell held more zones than one upstream page, so the
	// answer may miss one.
	Truncated bool `json:"truncated"`
	// Nearest is null when no zone holds the point or lies within RadiusM.
	Nearest     *HeritageMonument `json:"nearest"`
	Attribution string            `json:"attribution"`
}

// HeritageMonument is one protection zone as the dataset describes it.
// Each text field is the dataset's own value ("" when it has none); the
// server adds only the distance and the inside flag.
type HeritageMonument struct {
	Type             string `json:"type"`                       // anlaegstype
	Dating           string `json:"dating"`                     // datering
	Status           string `json:"status"`                     // fred_status
	ProtectionNumber string `json:"protectionNumber,omitempty"` // frednr
	SystemNumber     string `json:"systemNumber,omitempty"`     // systemnr
	// DistanceM is from the point to the zone's edge in whole metres; 0
	// inside.
	DistanceM  int  `json:"distanceM"`
	InsideZone bool `json:"insideZone"`
}

func (q *heritageQuery) response(page heritage.Page, e, n float64) HeritageInfoResponse {
	resp := HeritageInfoResponse{RadiusM: q.set.radiusM, Truncated: page.Truncated, Attribution: heritageAttribution}
	r := heritage.Nearest(page.Zones, e, n, float64(q.set.radiusM))
	resp.Count = r.Count
	if z := r.Zone; z != nil {
		resp.Nearest = &HeritageMonument{Type: z.Type, Dating: z.Dating, Status: z.Status,
			ProtectionNumber: z.ProtectionNo, SystemNumber: z.SystemNo,
			DistanceM: int(math.Round(r.DistanceM)), InsideZone: r.Inside}
	}
	return resp
}

// inHeritageBounds is the overlay's own extent (heritage_overlay.go).
func inHeritageBounds(lat, lon float64) bool {
	b := heritageBounds
	return lat >= b[0][0] && lat <= b[1][0] && lon >= b[0][1] && lon <= b[1][1]
}

// ── Handler ────────────────────────────────────────────────────────────────

func (s *Server) handleHeritageInfo(w http.ResponseWriter, r *http.Request) {
	q := s.heritage
	if q == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	lat, lon, err := parseHeritageQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, errHeritageQuery.Error())
		return
	}
	e, n := dhm.ToUTM32(lat, lon)
	cell, ok := heritage.CellOf(e, n)
	if !inHeritageBounds(lat, lon) || !ok {
		w.Header().Set("X-CoreScope-Cache", "none")
		writeJSON(w, q.response(heritage.Page{}, e, n))
		return
	}
	data, state, err := q.cellPage(r, cell)
	if err != nil {
		q.writeFetchError(w, r, err)
		return
	}
	var page heritage.Page
	if err := json.Unmarshal(data, &page); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("X-CoreScope-Cache", state)
	writeJSON(w, q.response(page, e, n))
}

// cellPage returns a cell's encoded page from the cache ("hit") or
// upstream ("miss"). Only a miss is charged to the client, and only a
// success is cached.
func (q *heritageQuery) cellPage(r *http.Request, cell heritage.Cell) ([]byte, string, error) {
	if data, hit := q.cache.Get(cell, q.now()); hit {
		return data, "hit", nil
	}
	if ok, retry := q.limiter.allow(r); !ok {
		return nil, "", rateLimited{retry}
	}
	key := strconv.Itoa(cell.E) + "," + strconv.Itoa(cell.N)
	// Admission is per upstream call, not per request: callers that join a
	// running call add no work and take no slot.
	data, _, err := q.flight.DoAdmitted(r.Context(), key, q.set.upstreamTimeout, q.admit, func(ctx context.Context) ([]byte, error) {
		page, err := q.src.FetchCell(ctx, cell)
		if err != nil {
			return nil, err
		}
		enc, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		q.cache.Put(cell, enc, q.now())
		return enc, nil
	})
	return data, "miss", err
}

// rateLimited is a refusal by the per-IP budget.
type rateLimited struct{ retry time.Duration }

func (rateLimited) Error() string { return "rate limit exceeded" }

// writeFetchError maps a failed lookup: a client that left gets nothing,
// the per-IP budget 429, a full gate 503 (charge refunded), a timeout 504,
// anything else upstream 502. None carries upstream text, and none may be
// stored, whatever middleware the route sits behind (#723 M3, as
// masts_proxy.go).
func (q *heritageQuery) writeFetchError(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Cache-Control", "no-store")
	var rl rateLimited
	switch {
	case errors.As(err, &rl):
		writeRateLimited(w, rl.retry)
		return
	case r.Context().Err() != nil:
		return
	case errors.Is(err, dhm.ErrAdmissionFull):
		// A refusal did no upstream work: give the per-IP charge back.
		q.limiter.refund(r)
		if n, ok := q.refuseLog.note(q.now(), q.refused.Add(1), heritageLogEvery); ok {
			q.logf("[heritage] admission: refused %d lookup(s) since the last report because map.heritage.maxInFlight (%d) upstream calls are running; raise it if this persists", n, q.admit.Cap())
		}
		setRetryAfter(w, heritageBusyRetry)
		writeError(w, http.StatusServiceUnavailable, errHeritageBusy.Error())
		return
	}
	if n, ok := q.failLog.note(q.now(), q.failures.Add(1), heritageLogEvery); ok {
		q.logf("[heritage] upstream lookup failed (%s); %d failure(s) since the last report", dhm.FailureReason(err), n)
	}
	if dhm.FailureReason(err) == "timeout" || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "heritage data timed out")
		return
	}
	writeError(w, http.StatusBadGateway, "heritage data unavailable")
}
