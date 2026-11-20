package main

// #941 M1 — GET /api/terrain/coverage: the predicted reach of a radio at a
// node or a map point ("Predicted reach"), and GET
// /api/terrain/coverage/raster, its loss raster.
//
// The browser asks once per geometry. The server plans the aligned
// elevation tiles of the disk (internal/dhm coverage.go), takes each from
// its tile cache or fetches it (one allow-listed DTM raster from the DHM
// WCS, and the Copernicus GLO-30 foreign fill for the cells DHM does not
// cover), sweeps the radials with free-space loss plus Bullington
// diffraction, and keeps the result as a gzip-compressed byte raster of
// path loss in dB. The JSON answer says where the raster is; the raster
// route serves cache hits only.
//
// Properties, each pinned by terrain_coverage_941_test.go:
//   - Off unless map.coverage.enabled (its canonical key only, #884) and
//     there is terrain: DHM active (map.dhm with a token) or the foreign
//     fill on (map.dhm.profile.foreignFill.enabled). The block is lifted
//     out of the client-visible map.
//   - Every parameter is validated against the profile's grammar; errors
//     are fixed strings that echo nothing. Power, gains, cable loss and
//     sensitivity are not parameters and not in the cache key: they only
//     shift the margin, so the browser applies them to the cached loss and
//     a slider never asks the server again.
//   - A point no source covers, an over-large radius, a hidden or unknown
//     node and a cache hit make no upstream call.
//   - A cache miss is charged to the client's per-IP budget
//     (map.coverage.rateLimitPerMin); at most map.coverage.maxInFlight
//     computations run at once, server-wide (503 + Retry-After beyond
//     that); identical requests share one computation (dhm.Flight), which
//     runs to its end or its 30 s deadline even if every requester leaves,
//     so a retry finds it cached.
//   - DHM rasters go through a sub-budget of two of the shared client's
//     four upstream slots (dhm.LimitSource), so hillshade tiles and
//     profiles keep the rest, and through a server-wide budget of
//     map.coverage.upstreamPerMin calls a minute; a cold computation the
//     budget cannot pay is 503 + Retry-After before any call.
//   - Tiles are cached as int16 decimetres in a byte-bounded LRU
//     (map.coverage.elevationCacheMB), results in their own LRU
//     (map.coverage.cacheEntries, bounded by bytes too).
//   - Upstream failures are 502, timeouts 504; neither carries upstream
//     text, and the token never appears in a response or a log line. A
//     failed foreign fill leaves the reach partial and is kept only
//     coveragePartialTTL, so a later request tries it again.
//
// The endpoint reads node positions from the in-memory node snapshot and
// observer radios through the profile's read-only query; it writes
// nothing to the database.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
	"golang.org/x/sync/errgroup"
)

const (
	coverageRoute       = "/api/terrain/coverage"
	coverageRasterRoute = "/api/terrain/coverage/raster"

	// coverageDeadline bounds one computation, fetches included.
	coverageDeadline = 30 * time.Second
	// coverageBusyRetry is the Retry-After of a full admission.
	coverageBusyRetry = 5 * time.Second
	// coverageUpstreamSlots is the reach's share of the shared DHM client's
	// four upstream slots (#941 owner decision 2).
	coverageUpstreamSlots = 2
	// coveragePerCall bounds one tile's upstream fetch: the shared
	// client's own per-request timeout.
	coveragePerCall = 10 * time.Second
	// coverageTileWorkers is how many tiles are gathered at once; each
	// holds at most two decoded tiles (2 MB).
	coverageTileWorkers = 4
	// coverageFillSlots bounds the foreign fills running at once,
	// server-wide.
	coverageFillSlots = 2
	// coverageSnap is the centre's snap in the cache key: 4 decimals,
	// about 11 m, against a 100 m raster pixel (M0 §3.3).
	coverageSnap = 1e4
	// coverageDefaultRadiusKm is the radius when the request names none,
	// capped at map.coverage.maxRadiusKm.
	coverageDefaultRadiusKm = 40
	// coverageRxDefaultM is the receiver height of the raster: a handheld
	// (#941 owner decision 7).
	coverageRxDefaultM = 2
	// coveragePartialTTL is how long a reach with a failed foreign fill is
	// kept: briefly, so a later request tries the fill again.
	coveragePartialTTL = 5 * time.Minute
	// coverageResultMaxBytes bounds one cached result: the largest raster
	// (1008², stored gzip) plus its metadata.
	coverageResultMaxBytes = 1008*1008 + 64<<10
	// coverageRefusalLogEvery throttles the refusal and fill-failure logs.
	coverageRefusalLogEvery = time.Minute
	// coverageMaxInFlightCap bounds map.coverage.maxInFlight: each
	// computation uses half the CPUs (dhm.CoverageWorkers) and up to
	// ~64 MB.
	coverageMaxInFlightCap = 4
)

// CoverageConfig is config.json `map.coverage` (#941): the predicted
// reach. A nil field takes the server default; every value is bounded.
type CoverageConfig struct {
	Enabled bool `json:"enabled"`
	// MaxRadiusKm is the largest radius a request may ask for (5–60).
	MaxRadiusKm *float64 `json:"maxRadiusKm"`
	// ResolutionM is the elevation grid and radial step (20–60 m).
	ResolutionM *int `json:"resolutionM"`
	// MaxInFlight is how many computations may run at once (1–4).
	MaxInFlight *int `json:"maxInFlight"`
	// RateLimitPerMin is each client's budget of cache misses; 0 is off.
	RateLimitPerMin *int `json:"rateLimitPerMin"`
	// UpstreamPerMin is the server-wide budget of DHM rasters a minute.
	UpstreamPerMin *int `json:"upstreamPerMin"`
	// CacheEntries and CacheTTLHours bound the result cache.
	CacheEntries  *int     `json:"cacheEntries"`
	CacheTTLHours *float64 `json:"cacheTTLHours"`
	// ElevationCacheMB bounds the elevation tile cache.
	ElevationCacheMB *int `json:"elevationCacheMB"`
}

// coverageSettings are map.coverage, resolved and bounded.
type coverageSettings struct {
	maxRadiusM          float64
	resolutionM         int
	maxInFlight         int
	rateLimitPerMin     int
	upstreamPerMin      int
	cacheEntries        int
	cacheTTL            time.Duration
	elevationCacheBytes int
}

// resolveCoverageSettings applies the defaults and bounds; an
// out-of-range value logs a warning and takes the default.
func resolveCoverageSettings(cc CoverageConfig, logf func(string, ...interface{})) coverageSettings {
	k := configKnobs{tag: "[coverage]", block: "map.coverage", logf: logf}
	return coverageSettings{
		maxRadiusM:      1000 * k.num("maxRadiusKm", cc.MaxRadiusKm, 40, 5, 60, false),
		resolutionM:     k.integer("resolutionM", cc.ResolutionM, 30, 20, 60),
		maxInFlight:     k.integer("maxInFlight", cc.MaxInFlight, 1, 1, coverageMaxInFlightCap),
		rateLimitPerMin: k.integer("rateLimitPerMin", cc.RateLimitPerMin, 2, 0, 600),
		upstreamPerMin:  k.integer("upstreamPerMin", cc.UpstreamPerMin, 240, 0, 6000),
		cacheEntries:    k.integer("cacheEntries", cc.CacheEntries, 32, 1, 256),
		cacheTTL:        time.Duration(k.num("cacheTTLHours", cc.CacheTTLHours, 24, 0, 168, true) * float64(time.Hour)),
		// 64 MB holds 128 tiles: about three 40 km disks.
		elevationCacheBytes: k.integer("elevationCacheMB", cc.ElevationCacheMB, 64, 8, 1024) << 20,
	}
}

// CoverageRadio is the link budget the browser colours the loss with:
// config `radio`, resolved. The server never uses it in a computation.
type CoverageRadio struct {
	TxPowerDbm     float64 `json:"txPowerDbm"`
	AntennaGainDbi float64 `json:"antennaGainDbi"`
	RxGainDbi      float64 `json:"rxGainDbi"`
	LossDb         float64 `json:"lossDb"`
	SensitivityDbm float64 `json:"sensitivityDbm"`
}

// resolveCoverageRadio applies the link-budget defaults (#941 M0 §3.5,
// owner decision 7): 22 dBm, the firmware's most common LORA_TX_POWER;
// 3 dBi at both ends; no cable loss; −130 dBm, SF8 at 62.5 kHz.
func resolveCoverageRadio(rc *RadioConfig, logf func(string, ...interface{})) CoverageRadio {
	if rc == nil {
		rc = &RadioConfig{}
	}
	k := configKnobs{tag: "[coverage]", block: "radio", logf: logf}
	return CoverageRadio{
		TxPowerDbm:     k.num("txPowerDbm", rc.TxPowerDbm, 22, -10, 30, false),
		AntennaGainDbi: k.num("antennaGainDbi", rc.AntennaGainDbi, 3, -10, 20, false),
		RxGainDbi:      k.num("rxGainDbi", rc.RxGainDbi, 3, -10, 20, false),
		LossDb:         k.num("lossDb", rc.LossDb, 0, 0, 30, false),
		SensitivityDbm: k.num("sensitivityDbm", rc.SensitivityDbm, -130, -150, -80, false),
	}
}

// CoverageClientConfig is the `coverage` block of /api/config/client:
// present only while the reach is served. It carries what the panel
// needs and none of the server's limits beyond the radius.
type CoverageClientConfig struct {
	Enabled     bool    `json:"enabled"`
	MaxRadiusKm float64 `json:"maxRadiusKm"`
	RadiusKm    float64 `json:"radiusKm"` // the radius a request without r gets
	ResolutionM int     `json:"resolutionM"`
	RxHeightM   float64 `json:"rxHeightM"` // the receiver height without hr
	// PointAntenna is a map point's transmitter height without ha.
	PointAntenna *DHMPointAntenna `json:"pointAntenna,omitempty"`
	// Radio is the operator's link budget, the panel's starting values.
	Radio CoverageRadio `json:"radio"`
	// DHM says the reach uses DHM: its credit is the dhm block's.
	DHM bool `json:"dhm"`
	// Fill is the Copernicus credit while the foreign fill is used.
	Fill *DHMFillClientConfig `json:"fill,omitempty"`
}

// ── Service ────────────────────────────────────────────────────────────────

// terrainCoverage serves the predicted reach.
type terrainCoverage struct {
	set   coverageSettings
	radio CoverageRadio
	// link resolves the frequency, k and antenna heights by the profile's
	// own rules (terrain_profile.go): the profile itself when it is on.
	link *terrainProfiler
	dhm  dhm.ElevationSource // the DHM WCS behind its sub-budget; nil: no DHM
	fill terrainFiller       // the GLO-30 foreign fill; nil: off

	tiles      *dhm.Cache[coverageTileKey]
	results    *dhm.Cache[string]
	flight     dhm.Flight[[]byte] // computations
	tileFlight dhm.Flight[[]byte] // tile fetches shared across computations
	admit      *dhm.Admission
	budget     *upstreamBudget
	limiter    *ipRateLimiter
	fillSlots  chan struct{}
	covers     func(e, n float64) bool
	deadline   time.Duration
	workers    int

	refused    atomic.Int64
	refuseLog  logThrottle
	fillFailed atomic.Int64
	fillLog    logThrottle

	now        func() time.Time
	logf       func(string, ...interface{})
	logFailure func(what string, err error)
}

type terrainCoverageDeps struct {
	src        dhm.ElevationSource // the DHM WCS; nil: DHM is not active
	fill       terrainFiller       // the foreign fill; nil: off
	link       *terrainProfiler    // nil: built from the radio config
	logf       func(string, ...interface{})
	logFailure func(what string, err error)
	observers  func() ([]Observer, error)
	trusted    []*net.IPNet
}

// coverageTileKey is one cached tile of one source.
type coverageTileKey struct {
	Res, X, Y int
	Fill      bool
}

// newTerrainCoverage returns nil unless the reach is enabled and has
// terrain to use.
func newTerrainCoverage(cc CoverageConfig, rc *RadioConfig, d terrainCoverageDeps) *terrainCoverage {
	if d.logf == nil {
		d.logf = log.Printf
	}
	if !cc.Enabled {
		return nil
	}
	if d.src == nil && d.fill == nil {
		d.logf("[coverage] map.coverage.enabled needs terrain: map.dhm enabled with a token, or map.dhm.profile.foreignFill.enabled; predicted reach off")
		return nil
	}
	set := resolveCoverageSettings(cc, d.logf)
	link := d.link
	if link == nil {
		if d.observers == nil {
			d.observers = func() ([]Observer, error) { return nil, nil }
		}
		// The profile's rules without the profile: its frequency, k and
		// antenna resolution read only these fields.
		link = &terrainProfiler{radio: resolveTerrainRadio(rc, d.logf), observers: d.observers, now: time.Now, logf: d.logf}
	}
	c := &terrainCoverage{
		set:   set,
		radio: resolveCoverageRadio(rc, d.logf),
		link:  link,
		fill:  d.fill,
		tiles: dhm.NewCache[coverageTileKey](set.elevationCacheBytes/dhm.CoverageTileBytes, set.elevationCacheBytes, set.cacheTTL),
		results: dhm.NewCache[string](set.cacheEntries, set.cacheEntries*coverageResultMaxBytes,
			set.cacheTTL),
		admit:      dhm.NewAdmission(set.maxInFlight),
		limiter:    newIPRateLimiter("coverage", "map.coverage.rateLimitPerMin", set.rateLimitPerMin, d.trusted, d.logf),
		fillSlots:  make(chan struct{}, coverageFillSlots),
		covers:     dhm.Covers,
		deadline:   coverageDeadline,
		workers:    dhm.CoverageWorkers(),
		now:        time.Now,
		logf:       d.logf,
		logFailure: d.logFailure,
	}
	c.budget = newUpstreamBudget(set.upstreamPerMin, func() time.Time { return c.now() })
	if d.src != nil {
		c.dhm = dhm.LimitSource(d.src, coverageUpstreamSlots)
	}
	return c
}

// newTerrainCoverageFromConfig wires the reach to the DHM client (when
// active) and, with map.dhm.profile.foreignFill.enabled, to a Copernicus
// client of its own, so the profile's fill budget is never spent here.
func newTerrainCoverageFromConfig(cfg *Config, proxy *dhmProxy, terrain *terrainProfiler, db *DB) *terrainCoverage {
	if cfg == nil || !cfg.Coverage.Enabled {
		return nil
	}
	d := terrainCoverageDeps{link: terrain, trusted: parseCIDRListQuiet(cfg.WSTrustedProxies())}
	if proxy != nil && proxy.client != nil {
		d.src = dhm.NewWCSSource(proxy.client)
		d.logFailure = proxy.client.LogFailure
	}
	if db != nil {
		d.observers = db.GetObservers
	}
	if cfg.DHM.Profile.ForeignFill.Enabled {
		fs := resolveTerrainSettings(cfg.DHM.Profile, quietLogf)
		if terrain != nil {
			fs = terrain.set
		}
		o := fillOptions(fs)
		set := resolveCoverageSettings(cfg.Coverage, quietLogf)
		// Every admitted computation may run its fills, each within the
		// fill's own call budget.
		o.MaxPending = set.maxInFlight * coverageFillSlots * dhm.MaxCallsPerModel
		d.fill = copdem.NewClient(o)
	}
	return newTerrainCoverage(cfg.Coverage, cfg.Radio, d)
}

// quietLogf drops a line: for settings another feature already resolved
// and warned about.
func quietLogf(string, ...interface{}) {}

// extractCoverageConfig lifts map.coverage out of the client-visible map;
// only its canonical `enabled` switches it on (#884).
func (c *Config) extractCoverageConfig() {
	liftMapBlock(c, "coverage", "predicted reach", &c.Coverage, func(cc *CoverageConfig) *bool { return &cc.Enabled })
}

// ── Upstream budget ────────────────────────────────────────────────────────

// upstreamBudget is a server-wide token bucket of upstream calls: perMin a
// minute, holding at most perMin. A batch is admitted when the bucket
// holds it, or when it is full: a computation larger than the whole
// budget then runs alone and leaves the bucket in debt, so the long-run
// rate stays at perMin.
type upstreamBudget struct {
	mu     sync.Mutex
	perMin float64
	tokens float64
	at     time.Time
	now    func() time.Time
}

func newUpstreamBudget(perMin int, now func() time.Time) *upstreamBudget {
	return &upstreamBudget{perMin: float64(perMin), tokens: float64(perMin), now: now}
}

// take spends n calls, or says how long until it could.
func (b *upstreamBudget) take(n int) (bool, time.Duration) {
	if n <= 0 {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.perMin <= 0 {
		return false, time.Minute
	}
	t := b.now()
	if !b.at.IsZero() {
		b.tokens = math.Min(b.perMin, b.tokens+t.Sub(b.at).Minutes()*b.perMin)
	}
	b.at = t
	need := math.Min(float64(n), b.perMin)
	if b.tokens >= need {
		b.tokens -= float64(n)
		return true, 0
	}
	return false, time.Duration((need - b.tokens) / b.perMin * float64(time.Minute))
}

// errCoverageBudget: the upstream budget cannot pay a cold computation.
type errCoverageBudget struct{ retry time.Duration }

func (e errCoverageBudget) Error() string { return "predicted reach busy" }

// ── Request ────────────────────────────────────────────────────────────────

// coverageParams are the only accepted query keys.
var coverageParams = map[string]bool{"at": true, "ha": true, "hr": true, "r": true, "freq": true}

type coverageQuery struct {
	at       terrainRef
	ha, hr   *float64
	radiusKm *float64
	freq     *float64
}

func parseCoverageQuery(q url.Values) (coverageQuery, error) {
	var cq coverageQuery
	for k, vs := range q {
		if !coverageParams[k] || len(vs) != 1 || len(vs[0]) > 64 {
			return cq, errTerrainQuery
		}
	}
	var err error
	if cq.at, err = parseTerrainRef(q.Get("at")); err != nil {
		return cq, err
	}
	for _, f := range []struct {
		key   string
		dst   **float64
		valid func(float64) bool
	}{
		{"ha", &cq.ha, validAntennaM},
		{"hr", &cq.hr, validAntennaM},
		// The upper bound is the operator's, checked after parsing so the
		// refusal can name it.
		{"r", &cq.radiusKm, func(v float64) bool { return v >= 1 }},
		{"freq", &cq.freq, validFreqMHz},
	} {
		vs, ok := q[f.key]
		if !ok {
			continue
		}
		v, ok := parseTerrainNumber(vs[0], terrainNumberRe)
		if !ok || !f.valid(v) {
			return cq, errTerrainQuery
		}
		*f.dst = &v
	}
	return cq, nil
}

// coverageKey is the result cache key: everything the loss depends on,
// and nothing the browser applies (power, gains, cable loss,
// sensitivity).
type coverageKey struct {
	Lat, Lon    int64 // snapped to coverageSnap
	RadiusDm    int64 // radius in 100 m steps
	TxHalfM     int64 // transmitter height in 0.5 m steps
	RxHalfM     int64
	FreqKHz     int64
	KMilli      int64
	ResolutionM int
	Model       string
	DHM, Fill   bool // the sources the computation may use
}

// hash is the key's raster id: 32 hex digits of its SHA-256.
func (k coverageKey) hash() string {
	s := fmt.Sprintf("%d,%d|%d|%d|%d|%d|%d|%d|%s|%t|%t", k.Lat, k.Lon, k.RadiusDm, k.TxHalfM, k.RxHalfM, k.FreqKHz, k.KMilli, k.ResolutionM, k.Model, k.DHM, k.Fill)
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// snapHalf rounds a height to the cache key's 0.5 m.
func snapHalf(h float64) float64 { return math.Round(h*2) / 2 }

// coverageRequest is a validated, resolved request.
type coverageRequest struct {
	end                 terrainEnd
	center              dhm.LatLon // snapped
	spec                dhm.CoverageSpec
	txSource, rxSource  string
	txAssumed           bool
	freqSource, kSource string
	key                 string
}

// coverageRefusal is a refused request's body: the error, and the limit
// an over-large radius broke.
type coverageRefusal struct {
	Error       string  `json:"error"`
	MaxRadiusKm float64 `json:"maxRadiusKm,omitempty"`
}

// resolveCoverageRequest resolves the centre (hidden, unknown and
// unpositioned nodes are 404), the radius, heights, frequency and k, and
// gates on the sources. No upstream call, no charge.
func (s *Server) resolveCoverageRequest(ctx context.Context, c *terrainCoverage, q coverageQuery) (coverageRequest, int, *coverageRefusal) {
	var req coverageRequest
	end, ok := s.resolveTerrainEnd(ctx, q.at)
	if !ok {
		return req, http.StatusNotFound, &coverageRefusal{Error: "node not found"}
	}
	radiusM := math.Min(coverageDefaultRadiusKm*1000, c.set.maxRadiusM)
	if q.radiusKm != nil {
		if *q.radiusKm*1000 > c.set.maxRadiusM {
			return req, http.StatusBadRequest, &coverageRefusal{Error: "radius too large", MaxRadiusKm: c.set.maxRadiusM / 1000}
		}
		radiusM = math.Round(*q.radiusKm*10) * 100
	}
	req.center = dhm.LatLon{Lat: math.Round(end.pt.Lat*coverageSnap) / coverageSnap, Lon: math.Round(end.pt.Lon*coverageSnap) / coverageSnap}
	if !c.servable(req.center) {
		return req, http.StatusBadRequest, &coverageRefusal{Error: "point not covered"}
	}
	// The transmitter's height by the profile's rules: the request, the
	// operator's node or role height, the owner's, a cached surface
	// estimate, the role default. Nothing is measured for the reach.
	ends := [2]terrainEnd{end, {kind: "point"}}
	s.attachOwnerHeights(ctx, &ends)
	c.link.cachedEstimates(&ends, [2]*float64{q.ha, nil})
	req.end = ends[0]
	tx, txSource, assumed := c.link.antenna(ends[0], q.ha)
	rx, rxSource := float64(coverageRxDefaultM), "default"
	if q.hr != nil {
		rx, rxSource = *q.hr, "request"
	}
	freq, freqSource := c.link.frequency(q.freq)
	k, kSource := c.link.kFactor(nil)
	req.spec = dhm.CoverageSpec{Center: req.center, RadiusM: radiusM, ResolutionM: float64(c.set.resolutionM),
		TxHeightM: snapHalf(tx), RxHeightM: snapHalf(rx), FreqMHz: freq, K: k,
		OutPixelM: dhm.CoverageOutPixelFor(radiusM), Workers: c.workers}
	req.txSource, req.txAssumed, req.rxSource = txSource, assumed, rxSource
	req.freqSource, req.kSource = freqSource, kSource
	req.key = coverageKey{
		Lat: int64(math.Round(req.center.Lat * coverageSnap)), Lon: int64(math.Round(req.center.Lon * coverageSnap)),
		RadiusDm: int64(math.Round(radiusM / 100)), TxHalfM: int64(req.spec.TxHeightM * 2), RxHalfM: int64(req.spec.RxHeightM * 2),
		FreqKHz: int64(math.Round(freq * 1000)), KMilli: int64(math.Round(k * 1000)), ResolutionM: c.set.resolutionM,
		Model: dhm.CoverageModel, DHM: c.dhm != nil, Fill: c.fill != nil,
	}.hash()
	return req, 0, nil
}

// servable reports whether some source has terrain at the centre: DHM
// covers it, or it is in the foreign fill's region.
func (c *terrainCoverage) servable(at dhm.LatLon) bool {
	if c.dhm != nil {
		if e, n := dhm.ToUTM32(at.Lat, at.Lon); c.covers(e, n) {
			return true
		}
	}
	if c.fill != nil {
		t, ok := copdem.TileAt(at.Lat, at.Lon)
		return ok && t.InRegion()
	}
	return false
}

// ── Handlers ───────────────────────────────────────────────────────────────

func (s *Server) handleTerrainCoverage(w http.ResponseWriter, r *http.Request) {
	c := s.coverage
	if c == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	q, err := parseCoverageQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	req, status, refusal := s.resolveCoverageRequest(r.Context(), c, q)
	if refusal != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(refusal)
		return
	}
	state := "hit"
	data, hit := c.results.Get(req.key, c.now())
	if !hit {
		state = "miss"
		if ok, retry := c.limiter.allow(r); !ok {
			writeRateLimited(w, retry)
			return
		}
		data, _, err = c.flight.DoAdmittedHeld(r.Context(), req.key, c.deadline, c.admit, c.computeCall(req))
		if err != nil {
			c.writeFetchError(w, r, err)
			return
		}
	}
	entry, err := decodeCoverageEntry(data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("X-CoreScope-Cache", state)
	writeJSON(w, c.buildResponse(req, entry))
}

// handleTerrainCoverageRaster serves a cached reach's raster: the bytes
// as stored, gzip, or plain to a client that does not take gzip.
func (s *Server) handleTerrainCoverageRaster(w http.ResponseWriter, r *http.Request) {
	c := s.coverage
	if c == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	q := r.URL.Query()
	keys := q["key"]
	if len(q) != 1 || len(keys) != 1 || !isRasterKey(keys[0]) {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	data, ok := c.results.Get(keys[0], c.now())
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	entry, err := decodeCoverageEntry(data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		w.Write(entry.raster)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(entry.raster))
	if err == nil {
		var raw []byte
		if raw, err = io.ReadAll(zr); err == nil {
			w.Write(raw)
			return
		}
	}
	writeError(w, http.StatusInternalServerError, "internal error")
}

// isRasterKey accepts exactly coverageKey.hash's 32 lower-case hex digits.
func isRasterKey(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// writeFetchError maps a failed computation: a client that left gets
// nothing, a refusal is 503 with its charge given back, a timeout 504,
// anything else upstream 502. None carries upstream text.
func (c *terrainCoverage) writeFetchError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	var budget errCoverageBudget
	switch {
	case errors.Is(err, dhm.ErrAdmissionFull):
		c.limiter.refund(r)
		c.noteRefusal("map.coverage.maxInFlight computations are running")
		setRetryAfter(w, coverageBusyRetry)
		writeError(w, http.StatusServiceUnavailable, "predicted reach busy")
		return
	case errors.As(err, &budget):
		c.limiter.refund(r)
		c.noteRefusal("map.coverage.upstreamPerMin is spent")
		setRetryAfter(w, max(time.Second, budget.retry))
		writeError(w, http.StatusServiceUnavailable, "predicted reach busy")
		return
	}
	if c.logFailure != nil {
		c.logFailure("predicted reach raster", err)
	}
	if dhm.FailureReason(err) == "timeout" || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "terrain data timed out")
		return
	}
	writeError(w, http.StatusBadGateway, "terrain data unavailable")
}

// noteRefusal counts a refusal and reports it at most once per
// coverageRefusalLogEvery.
func (c *terrainCoverage) noteRefusal(why string) {
	n, ok := c.refuseLog.note(c.now(), c.refused.Add(1), coverageRefusalLogEvery)
	if ok {
		c.logf("[coverage] predicted reach: refused %d request(s) since the last report because %s; no upstream request was made", n, why)
	}
}

// noteFillFailure counts a failed tile fill and reports it at most once
// per coverageRefusalLogEvery, by category only.
func (c *terrainCoverage) noteFillFailure(err error) {
	n, ok := c.fillLog.note(c.now(), c.fillFailed.Add(1), coverageRefusalLogEvery)
	if ok {
		c.logf("[coverage] predicted reach foreign fill failed (%s): %d tile(s) left without terrain since the last report", dhm.FailureReason(err), n)
	}
}

// ── Computation ────────────────────────────────────────────────────────────

// computeCall is one shared computation: gather the tiles, sweep, encode,
// cache.
func (c *terrainCoverage) computeCall(req coverageRequest) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		spec := req.spec
		e0, n0 := dhm.ToUTM32(spec.Center.Lat, spec.Center.Lon)
		grid := dhm.NewCoverageGrid(e0, n0, spec.RadiusM, spec.ResolutionM)
		used, partial, err := c.gather(ctx, dhm.PlanCoverageTiles(e0, n0, spec.RadiusM, spec.ResolutionM), grid)
		if err != nil {
			return nil, err
		}
		res, err := dhm.ComputeCoverage(ctx, grid, spec)
		if err != nil {
			return nil, dhm.ContextFailure(err)
		}
		entry := coverageEntry{meta: coverageEntryMeta{Width: res.Width, Height: res.Height, Bounds: res.Bounds,
			CoveredPct: res.CoveredPct, DHM: used.dhm, Fill: used.fill}}
		if !math.IsNaN(res.GroundM) {
			g := round2(res.GroundM)
			entry.meta.GroundM = &g
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(res.Loss)
		zw.Close()
		entry.raster = buf.Bytes()
		enc := entry.encode()
		ttl := c.set.cacheTTL
		if partial {
			ttl = coveragePartialTTL
		}
		c.results.PutFor(req.key, enc, c.now(), ttl)
		return enc, nil
	}
}

// coverageUsed says which sources gave terrain to a computation.
type coverageUsed struct{ dhm, fill bool }

// coverageTileJob is one tile's state while gathering: its cached
// entries, and what still has to be fetched or filled.
type coverageTileJob struct {
	t                  dhm.CoverageTile
	dhmData, fillData  []byte // cached; nil: not cached, empty: nothing
	needDHM, needFill  bool
	anyCovered, allCov bool // DHM covers some, every cell
}

// gather pastes every tile into grid: DHM where it covers, the fill
// elsewhere. A DHM failure fails the computation; a fill failure only
// leaves gaps (partial).
func (c *terrainCoverage) gather(ctx context.Context, tiles []dhm.CoverageTile, grid *dhm.CoverageGrid) (coverageUsed, bool, error) {
	res := grid.Res
	resM := int(res)
	jobs := make([]coverageTileJob, len(tiles))
	now := c.now()
	calls := 0
	for i, t := range tiles {
		j := coverageTileJob{t: t}
		if c.dhm != nil {
			if _, ok := t.Raster(res); ok {
				j.dhmData, j.needDHM = c.cachedTile(coverageTileKey{Res: resM, X: t.X, Y: t.Y}, now)
			}
		}
		if c.fill != nil {
			j.fillData, j.needFill = c.cachedTile(coverageTileKey{Res: resM, X: t.X, Y: t.Y, Fill: true}, now)
		}
		if j.needDHM || j.needFill {
			j.anyCovered, j.allCov = c.tileCoverage(t, res)
			if j.needDHM && !j.anyCovered {
				// Nothing of the tile is Danish: no call, remembered.
				c.tiles.Put(coverageTileKey{Res: resM, X: t.X, Y: t.Y}, []byte{}, now)
				j.needDHM, j.dhmData = false, []byte{}
			}
			if j.needFill && c.dhm != nil && j.allCov {
				c.tiles.Put(coverageTileKey{Res: resM, X: t.X, Y: t.Y, Fill: true}, []byte{}, now)
				j.needFill, j.fillData = false, []byte{}
			}
		}
		if j.needDHM {
			calls++
		}
		jobs[i] = j
	}
	if ok, retry := c.budget.take(calls); !ok {
		return coverageUsed{}, false, errCoverageBudget{retry: retry}
	}
	var usedDHM, usedFill, partial atomic.Bool
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(coverageTileWorkers)
	for i := range jobs {
		j := &jobs[i]
		g.Go(func() error {
			dv, err := c.dhmTile(gctx, j, res)
			if err != nil {
				return err
			}
			fv, failed := c.fillTile(gctx, j, res)
			if failed {
				partial.Store(true)
			}
			merged, fromDHM, fromFill := mergeCoverageTile(dv, fv)
			if merged != nil {
				grid.Paste(j.t, merged)
			}
			if fromDHM {
				usedDHM.Store(true)
			}
			if fromFill {
				usedFill.Store(true)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return coverageUsed{}, false, err
	}
	return coverageUsed{dhm: usedDHM.Load(), fill: usedFill.Load()}, partial.Load(), nil
}

// cachedTile is a cached tile's bytes, or nil and true when it must be
// fetched.
func (c *terrainCoverage) cachedTile(k coverageTileKey, now time.Time) ([]byte, bool) {
	if data, ok := c.tiles.Get(k, now); ok {
		return data, false
	}
	return nil, true
}

// tileCoverage reports whether DHM covers any and every cell of t;
// without DHM it covers none.
func (c *terrainCoverage) tileCoverage(t dhm.CoverageTile, res float64) (someCovered, allCovered bool) {
	if c.dhm == nil {
		return false, false
	}
	allCovered = true
	for row := 0; row < dhm.CoverageTilePx; row++ {
		for col := 0; col < dhm.CoverageTilePx; col++ {
			if c.covers(t.Cell(res, col, row)) {
				someCovered = true
			} else {
				allCovered = false
			}
			if someCovered && !allCovered {
				return
			}
		}
	}
	return
}

// dhmTile is tile j's DHM heights: cached, or fetched once (shared with
// any computation fetching it too) and cached. nil: none.
func (c *terrainCoverage) dhmTile(ctx context.Context, j *coverageTileJob, res float64) ([]float32, error) {
	data := j.dhmData
	if j.needDHM {
		k := coverageTileKey{Res: int(res), X: j.t.X, Y: j.t.Y}
		var err error
		data, _, err = c.tileFlight.Do(ctx, fmt.Sprint(k), coveragePerCall, func(work context.Context) ([]byte, error) {
			r, _ := j.t.Raster(res)
			g, err := c.dhm.FetchRaster(work, r)
			if err != nil {
				return nil, err
			}
			enc := dhm.EncodeCoverageTile(dhm.CoverageTileFromGrid(&g, j.t, res, c.covers))
			c.tiles.Put(k, enc, c.now())
			return enc, nil
		})
		if err != nil {
			return nil, dhm.ContextFailure(err)
		}
	}
	if len(data) == 0 {
		return nil, nil
	}
	return dhm.DecodeCoverageTile(data)
}

// fillTile is tile j's foreign-fill heights for the cells DHM does not
// give: cached, or filled and cached. A failure is logged and leaves the
// cells without terrain (failed true); it is not cached.
func (c *terrainCoverage) fillTile(ctx context.Context, j *coverageTileJob, res float64) ([]float32, bool) {
	data := j.fillData
	if j.needFill {
		select {
		case c.fillSlots <- struct{}{}:
		case <-ctx.Done():
			return nil, true
		}
		vals, err := c.fillCells(ctx, j.t, res)
		<-c.fillSlots
		if err != nil {
			if ctx.Err() == nil {
				c.noteFillFailure(err)
			}
			return nil, true
		}
		data = dhm.EncodeCoverageTile(vals)
		c.tiles.Put(coverageTileKey{Res: int(res), X: j.t.X, Y: j.t.Y, Fill: true}, data, c.now())
	}
	if len(data) == 0 {
		return nil, false
	}
	vals, err := dhm.DecodeCoverageTile(data)
	return vals, err != nil
}

// fillCells fills the cells of t that DHM does not cover, all of them
// without DHM, through one foreign-fill call.
func (c *terrainCoverage) fillCells(ctx context.Context, t dhm.CoverageTile, res float64) ([]float32, error) {
	var idx []int
	var es, ns []float64
	for row := 0; row < dhm.CoverageTilePx; row++ {
		for col := 0; col < dhm.CoverageTilePx; col++ {
			e, n := t.Cell(res, col, row)
			if c.dhm != nil && c.covers(e, n) {
				continue
			}
			idx = append(idx, row*dhm.CoverageTilePx+col)
			es, ns = append(es, e), append(ns, n)
		}
	}
	plan := dhm.PointsPlan(es, ns)
	r, err := c.fill.Fill(ctx, &plan)
	if err != nil {
		return nil, err
	}
	if len(r.Values) != len(idx) {
		return nil, dhm.FetchError{Reason: "fill mismatch"}
	}
	vals := make([]float32, dhm.CoverageTileCells)
	nan := float32(math.NaN())
	for i := range vals {
		vals[i] = nan
	}
	for k, i := range idx {
		vals[i] = r.Values[k]
	}
	return vals, nil
}

// mergeCoverageTile is DHM where it has a height, the fill elsewhere, and
// whether each gave any.
func mergeCoverageTile(dv, fv []float32) (out []float32, fromDHM, fromFill bool) {
	switch {
	case dv == nil && fv == nil:
		return nil, false, false
	case dv == nil:
		dv = make([]float32, len(fv))
		nan := float32(math.NaN())
		for i := range dv {
			dv[i] = nan
		}
	}
	for i, v := range dv {
		if !math.IsNaN(float64(v)) {
			fromDHM = true
			continue
		}
		if fv != nil && !math.IsNaN(float64(fv[i])) {
			dv[i] = fv[i]
			fromFill = true
		}
	}
	return dv, fromDHM, fromFill
}

// ── Cached result ──────────────────────────────────────────────────────────

// coverageEntryMeta is the cached part of a response that depends on the
// computation, not on how the centre was named.
type coverageEntryMeta struct {
	Width      int           `json:"w"`
	Height     int           `json:"h"`
	Bounds     [2][2]float64 `json:"b"`
	GroundM    *float64      `json:"g"`
	CoveredPct float64       `json:"c"`
	DHM        bool          `json:"d"`
	Fill       bool          `json:"f"`
}

// coverageEntry is one cached result: the metadata and the gzip raster.
type coverageEntry struct {
	meta   coverageEntryMeta
	raster []byte
}

var errCoverageEntry = errors.New("malformed coverage entry")

// encode is the metadata's JSON length (uint32 LE), the JSON, the raster.
func (e coverageEntry) encode() []byte {
	meta, _ := json.Marshal(e.meta)
	out := make([]byte, 4, 4+len(meta)+len(e.raster))
	binary.LittleEndian.PutUint32(out, uint32(len(meta)))
	out = append(out, meta...)
	return append(out, e.raster...)
}

func decodeCoverageEntry(b []byte) (coverageEntry, error) {
	if len(b) < 4 {
		return coverageEntry{}, errCoverageEntry
	}
	n := int(binary.LittleEndian.Uint32(b))
	if n > len(b)-4 {
		return coverageEntry{}, errCoverageEntry
	}
	var e coverageEntry
	if err := json.Unmarshal(b[4:4+n], &e.meta); err != nil {
		return coverageEntry{}, errCoverageEntry
	}
	e.raster = b[4+n:]
	return e, nil
}

// ── Response ───────────────────────────────────────────────────────────────

// CoverageResponse is the body of GET /api/terrain/coverage.
type CoverageResponse struct {
	// Key names the raster: GET /api/terrain/coverage/raster?key=….
	Key    string         `json:"key"`
	Center CoverageCenter `json:"center"`
	Params CoverageParams `json:"params"`
	Raster CoverageRaster `json:"raster"`
	// Sources are the terrain sources the prediction used.
	Sources []CoverageSource `json:"sources"`
	// Attribution is the DHM credit when DHM was used (data bearbejdet);
	// AttributionFill the Copernicus Art. 6(b) line when the fill was.
	Attribution     string `json:"attribution,omitempty"`
	AttributionFill string `json:"attributionFill,omitempty"`
}

// CoverageCenter is the transmitter's position, snapped to 4 decimals.
type CoverageCenter struct {
	Lat     float64  `json:"lat"`
	Lon     float64  `json:"lon"`
	Kind    string   `json:"kind"` // "node" or "point"
	Role    string   `json:"role,omitempty"`
	GroundM *float64 `json:"groundM"` // null without terrain there
}

// CoverageParams are the computation's parameters as resolved.
type CoverageParams struct {
	RadiusKm       float64 `json:"radiusKm"`
	ResolutionM    int     `json:"resolutionM"`
	TxHeightM      float64 `json:"txHeightM"`
	TxHeightSource string  `json:"txHeightSource"`
	TxAssumed      bool    `json:"txAssumed"`
	RxHeightM      float64 `json:"rxHeightM"`
	RxHeightSource string  `json:"rxHeightSource"` // "request" or "default"
	FreqMHz        float64 `json:"freqMHz"`
	FreqSource     string  `json:"freqSource"`
	K              float64 `json:"k"`
	KSource        string  `json:"kSource"`
	Model          string  `json:"model"`
}

// CoverageRaster describes the loss raster.
type CoverageRaster struct {
	URL    string        `json:"url"`
	Width  int           `json:"width"`
	Height int           `json:"height"`
	Bounds [2][2]float64 `json:"bounds"` // [[south, west], [north, east]]: Web Mercator aligned
	// Encoding: one byte per pixel, row 0 north; NoData, or the path
	// loss LossBaseDb + value × LossStepDb.
	Encoding   string  `json:"encoding"`
	LossBaseDb float64 `json:"lossBaseDb"`
	LossStepDb float64 `json:"lossStepDb"`
	NoData     int     `json:"noData"`
	CoveredPct float64 `json:"coveredPct"` // share of the disk with a prediction
	Bytes      int     `json:"bytes"`      // gzip size of the raster
}

// CoverageSource is one terrain source of a prediction.
type CoverageSource struct {
	ID          string `json:"id"`
	ResolutionM int    `json:"resolutionM"`
	Model       string `json:"model"` // "dtm": bare ground; "dsm": surface
}

func (c *terrainCoverage) buildResponse(req coverageRequest, e coverageEntry) CoverageResponse {
	spec := req.spec
	resp := CoverageResponse{
		Key:    req.key,
		Center: CoverageCenter{Lat: req.center.Lat, Lon: req.center.Lon, Kind: req.end.kind, Role: req.end.role, GroundM: e.meta.GroundM},
		Params: CoverageParams{RadiusKm: spec.RadiusM / 1000, ResolutionM: c.set.resolutionM,
			TxHeightM: spec.TxHeightM, TxHeightSource: req.txSource, TxAssumed: req.txAssumed,
			RxHeightM: spec.RxHeightM, RxHeightSource: req.rxSource,
			FreqMHz: spec.FreqMHz, FreqSource: req.freqSource, K: spec.K, KSource: req.kSource, Model: dhm.CoverageModel},
		Raster: CoverageRaster{URL: coverageRasterRoute + "?key=" + req.key, Width: e.meta.Width, Height: e.meta.Height,
			Bounds: e.meta.Bounds, Encoding: "loss-u8", LossBaseDb: dhm.CoverageLossBaseDb, LossStepDb: dhm.CoverageLossStepDb,
			NoData: dhm.CoverageNoData, CoveredPct: e.meta.CoveredPct, Bytes: len(e.raster)},
		Sources: []CoverageSource{},
	}
	if e.meta.DHM {
		resp.Sources = append(resp.Sources, CoverageSource{ID: "dhm-dtm", ResolutionM: c.set.resolutionM, Model: "dtm"})
		resp.Attribution = dhm.AttributionDerived
	}
	if e.meta.Fill {
		// GLO-30 is a surface model (#738 decision 1).
		resp.Sources = append(resp.Sources, CoverageSource{ID: copdem.Source, ResolutionM: 30, Model: "dsm"})
		resp.AttributionFill = copdem.AttributionDerived
	}
	return resp
}

// coverageClientConfig is /api/config/client's coverage block; nil while
// the reach is off.
func (s *Server) coverageClientConfig() *CoverageClientConfig {
	c := s.coverage
	if c == nil {
		return nil
	}
	h, src, _ := c.link.antenna(terrainEnd{kind: "point"}, nil)
	out := &CoverageClientConfig{
		Enabled:      true,
		MaxRadiusKm:  c.set.maxRadiusM / 1000,
		RadiusKm:     math.Min(coverageDefaultRadiusKm, c.set.maxRadiusM/1000),
		ResolutionM:  c.set.resolutionM,
		RxHeightM:    coverageRxDefaultM,
		PointAntenna: &DHMPointAntenna{M: h, Source: src},
		Radio:        c.radio,
		DHM:          c.dhm != nil,
	}
	if c.fill != nil {
		out.Fill = &DHMFillClientConfig{
			Credit:      copdem.Credit,
			DatasetName: copdem.DatasetName,
			DatasetURL:  copdem.DatasetURL,
			LicenseName: copdem.LicenseName,
			LicenseURL:  copdem.LicenseURL,
		}
	}
	return out
}
