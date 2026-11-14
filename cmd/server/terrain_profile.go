package main

// #681 M3 — GET /api/terrain/profile: the terrain between two nodes or map
// points from Danmarks Højdemodel, with line of sight, the first Fresnel
// zone and earth curvature.
//
// One browser request per profile. The server plans the samples and the
// rasters (internal/dhm: profile.go), fetches at most 16 WCS rasters per
// elevation model through the shared DHM client (token header, global
// concurrency budget, no redirects), samples and releases them, and keeps
// only the elevations in a bounded TTL cache. The physics are computed per
// request from the cached elevations, so changing antenna heights, k or
// the frequency never goes upstream again.
//
// Properties, each pinned by terrain_profile_test.go:
//   - Every parameter is validated against a strict grammar and range;
//     errors are fixed strings that echo nothing back.
//   - An over-length path and a path outside Denmark make no upstream
//     call, and neither does a cache hit. A cache miss is charged to the
//     client's per-IP budget (map.dhm.profile.rateLimitPerMin, see
//     ip_rate_limit.go) before it goes upstream.
//   - A sample on Swedish, German, Polish or Norwegian land (dhm.Covers)
//     has no data, on both models: upstream answers 0.0 there, not nodata,
//     so a path onto foreign land is partial with no verdict (#692). The
//     sea is covered, Danish or not: 0.0 is its true height, except within
//     about 1.25 km of a foreign coast, where the coarse coastline could
//     put foreign land (#693 review round 2).
//   - At most map.dhm.profile.maxInFlight distinct profile fetches run at
//     once, server-wide; beyond that a request is 503 with Retry-After
//     before any raster work starts (#696). The per-IP budget alone does
//     not bound the total across addresses.
//   - Identical concurrent requests share one fetch (dhm.Flight), which
//     stops when every requester has gone or after the 25 s profile
//     deadline, releasing the client's concurrency slots (#683 review N3).
//   - Upstream failures are 502, timeouts 504; neither carries upstream
//     text, and the token never appears in a response or a log line.
//   - The verdict comes from the terrain model (DTM); the surface model
//     (DSM) is assessed for information only (#681 owner decision 6).
//     A profile with any gap gets no verdict, on either model: the DSM
//     can be a separate, complete coverage while the DTM has a hole, so
//     a partial profile forces the surface verdict to "unknown" too
//     (#689 review N1).
//
// The endpoint reads node positions from the in-memory node snapshot and
// observer radios through a read-only query; it writes nothing.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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

	"github.com/meshcore-analyzer/dhm"
	"golang.org/x/sync/errgroup"
)

const (
	// terrainDeadline bounds a whole profile fetch (plan §7).
	terrainDeadline = 25 * time.Second
	// terrainBusyRetry is the Retry-After of a 503 when maxInFlight
	// profiles are already being fetched: a few cold profiles' time.
	terrainBusyRetry = 5 * time.Second
	// terrainMaxInFlightCap is the top of the maxInFlight range: 4x the
	// shared upstream budget of 4 requests (#707). Each admitted profile
	// holds ~20 goroutines while blocked, so the old cap of 64 meant ~1280 above baseline.
	terrainMaxInFlightCap = 16
	// terrainRefusalLogEvery throttles the admission-refusal log line.
	terrainRefusalLogEvery = time.Minute
	// terrainMaxReturned is the most samples a response carries; the
	// worst points are always among them (dhm.Decimate).
	terrainMaxReturned = 1024
	// terrainObserverWindow is how recently an observer must have been
	// seen for its radio to count towards the modal frequency.
	terrainObserverWindow = 7 * 24 * time.Hour
	// terrainObserverFreqTTL is how long the modal frequency is reused.
	terrainObserverFreqTTL = 10 * time.Minute
	// terrainSnap is the coordinate snap of the cache key: 5 decimals,
	// about a metre, so advert jitter still hits the cache (plan §7).
	terrainSnap = 1e5
	// terrainMaxAntennaM bounds an antenna height above ground.
	terrainMaxAntennaM = 100
)

// terrainPointKey is the antennaHeightsM key of a free map point.
const terrainPointKey = "point"

// terrainRoleAntennaM are the default antenna heights above ground (#681
// owner decision 5). They are assumptions, and the response says so.
var terrainRoleAntennaM = map[string]float64{
	"repeater":      10,
	"room":          5,
	"companion":     2,
	"sensor":        2,
	terrainPointKey: 2,
}

// RadioConfig is config.json `radio`: the link-physics defaults of the
// terrain profile. Every field is optional.
type RadioConfig struct {
	// DefaultFreqMHz overrides the frequency observed from the mesh.
	DefaultFreqMHz *float64 `json:"defaultFreqMHz,omitempty"`
	// KFactor is the effective earth factor; default 4/3.
	KFactor *float64 `json:"kFactor,omitempty"`
	// AntennaHeightsM are antenna heights above ground in metres, keyed by
	// role ("repeater", "room", "companion", "sensor", "point") or by a
	// node's 64-hex public key.
	AntennaHeightsM map[string]float64 `json:"antennaHeightsM,omitempty"`
}

// ── Settings ───────────────────────────────────────────────────────────────

// terrainSettings are map.dhm.profile, resolved and bounded.
type terrainSettings struct {
	maxPathM        float64
	plan            dhm.PlanOptions
	cacheEntries    int
	cacheTTL        time.Duration
	upstreamTimeout time.Duration
	rateLimitPerMin int
	maxInFlight     int
}

// resolveTerrainSettings applies the defaults and bounds. An out-of-range
// value logs a warning and takes the default.
func resolveTerrainSettings(pc dhm.ProfileConfig, logf func(string, ...interface{})) terrainSettings {
	num := func(key string, v *float64, def, lo, hi float64, loOpen bool) float64 {
		if v == nil {
			return def
		}
		ok := *v <= hi && (*v > lo || !loOpen && *v == lo) // NaN fails both
		if !ok {
			logf("[dhm] config map.dhm.profile.%s is out of range; using %v", key, def)
			return def
		}
		return *v
	}
	integer := func(key string, v *int, def, lo, hi int) int {
		if v == nil {
			return def
		}
		if *v < lo || *v > hi {
			logf("[dhm] config map.dhm.profile.%s is out of range; using %d", key, def)
			return def
		}
		return *v
	}
	return terrainSettings{
		// The planner's rasters are at most dhm.MaxRasterSpanM (80 km).
		maxPathM: 1000 * num("maxPathKm", pc.MaxPathKm, 80, 0, dhm.MaxRasterSpanM/1000, true),
		plan: dhm.PlanOptions{
			MaxSamples: integer("maxSamples", pc.MaxSamples, 3200, 16, 3200),
			ChunkM:     1000 * num("chunkKm", pc.ChunkKm, 2.5, 0, dhm.MaxRasterSpanM/1000, false),
		},
		cacheEntries: integer("cacheEntries", pc.CacheEntries, 256, 1, 4096),
		cacheTTL:     time.Duration(num("cacheTTLHours", pc.CacheTTLHours, 24, 0, 168, true) * float64(time.Hour)),
		// Capped at the shared client's own 10 s per-request timeout.
		upstreamTimeout: time.Duration(integer("upstreamTimeoutMs", pc.UpstreamTimeoutMs, 10000, 500, 10000)) * time.Millisecond,
		rateLimitPerMin: integer("rateLimitPerMin", pc.RateLimitPerMin, 10, 0, 6000),
		// 4 matches the shared DHM client's upstream budget: one model=both
		// profile has up to 32 rasters, so it can fill all 4 slots on its
		// own, and more profiles in flight would only queue goroutines.
		maxInFlight: integer("maxInFlight", pc.MaxInFlight, 4, 1, terrainMaxInFlightCap),
	}
}

// terrainRadio is RadioConfig, validated.
type terrainRadio struct {
	freqMHz float64 // 0: not configured
	k       float64 // 0: not configured
	heights map[string]float64
}

func resolveTerrainRadio(rc *RadioConfig, logf func(string, ...interface{})) terrainRadio {
	out := terrainRadio{heights: map[string]float64{}}
	if rc == nil {
		return out
	}
	if v := rc.DefaultFreqMHz; v != nil {
		if validFreqMHz(*v) {
			out.freqMHz = *v
		} else {
			logf("[dhm] config radio.defaultFreqMHz is outside 150-2500 MHz; ignored")
		}
	}
	if v := rc.KFactor; v != nil {
		if validKFactor(*v) {
			out.k = *v
		} else {
			logf("[dhm] config radio.kFactor is outside 0.5-10; ignored")
		}
	}
	bad := 0
	for key, h := range rc.AntennaHeightsM {
		k := strings.ToLower(key)
		_, isRole := terrainRoleAntennaM[k]
		if (!isRole && !isHexPubkey(k)) || !validAntennaM(h) {
			bad++
			continue
		}
		out.heights[k] = h
	}
	if bad > 0 {
		// The keys are not echoed: a config typo should not land in logs.
		logf("[dhm] config radio.antennaHeightsM: %d entries ignored (keys must be a role or a 64-hex public key, heights 0-%d m)", bad, terrainMaxAntennaM)
	}
	return out
}

func validFreqMHz(f float64) bool  { return f >= 150 && f <= 2500 } // firmware CommonCLI clamp
func validKFactor(k float64) bool  { return k >= 0.5 && k <= 10 }
func validAntennaM(h float64) bool { return h >= 0 && h <= terrainMaxAntennaM }

// ── Profiler ───────────────────────────────────────────────────────────────

type terrainProfiler struct {
	src        dhm.ElevationSource
	set        terrainSettings
	radio      terrainRadio
	cache      *dhm.Cache[terrainKey]
	flight     dhm.Flight[[]byte]
	limiter    *ipRateLimiter // nil: no per-client limit
	admit      chan struct{}  // one token per profile fetch in flight (maxInFlight)
	refused    atomic.Int64   // admission refusals (503) since start
	logMu      sync.Mutex     // guards the throttle below
	logAt      time.Time      // last refusal report
	logCount   int64          // refusals already reported
	deadline   time.Duration
	now        func() time.Time
	logf       func(string, ...interface{})
	logFailure func(what string, err error)
	observers  func() ([]Observer, error)

	freqMu  sync.Mutex
	freqAt  time.Time
	freqVal float64
	freqOK  bool
}

type terrainProfilerDeps struct {
	src        dhm.ElevationSource
	logf       func(string, ...interface{})
	logFailure func(what string, err error)
	observers  func() ([]Observer, error)
	trusted    []*net.IPNet
}

// terrainCacheMaxBytes is the cache's byte bound: one entry holds n DTM + n
// DSM float32 plus a 5-byte header, n at most maxSamples. dhm.Cache evicts
// on entry count OR total bytes; by construction every entry here is at
// most 1/cacheEntries of this bound, so the byte condition can never fire
// before the entry count does (#689 review N3). It is kept anyway,
// deliberately redundant, as defense-in-depth against a future change
// decoupling entry size from maxSamples, and because dhm.Cache is shared
// with the DHM tile cache (dhm_proxy.go), where variable tile sizes make
// the byte bound load-bearing — the two call sites should stay consistent.
// At the documented maxima (cacheEntries 4096, maxSamples 3200) this is
// ~105 MB; see config.example.json's _comment_cacheEntries.
func terrainCacheMaxBytes(cacheEntries, maxSamples int) int {
	return cacheEntries * (5 + 8*maxSamples)
}

// newTerrainProfiler returns nil unless the profile is enabled.
func newTerrainProfiler(pc dhm.ProfileConfig, rc *RadioConfig, d terrainProfilerDeps) *terrainProfiler {
	if !pc.Enabled || d.src == nil {
		return nil
	}
	if d.logf == nil {
		d.logf = log.Printf
	}
	if d.observers == nil {
		d.observers = func() ([]Observer, error) { return nil, nil }
	}
	set := resolveTerrainSettings(pc, d.logf)
	maxBytes := terrainCacheMaxBytes(set.cacheEntries, set.plan.MaxSamples)
	return &terrainProfiler{
		src:        d.src,
		set:        set,
		radio:      resolveTerrainRadio(rc, d.logf),
		cache:      dhm.NewCache[terrainKey](set.cacheEntries, maxBytes, set.cacheTTL),
		limiter:    newIPRateLimiter("map.dhm.profile.rateLimitPerMin", set.rateLimitPerMin, d.trusted, d.logf),
		admit:      make(chan struct{}, set.maxInFlight),
		deadline:   terrainDeadline,
		now:        time.Now,
		logf:       d.logf,
		logFailure: d.logFailure,
		observers:  d.observers,
	}
}

// newTerrainProfilerFromConfig offers the profile only when the DHM client
// is active (map.dhm enabled with a token); newTerrainProfiler checks
// map.dhm.profile.enabled.
func newTerrainProfilerFromConfig(cfg *Config, proxy *dhmProxy, db *DB) *terrainProfiler {
	if cfg == nil || proxy == nil || proxy.client == nil {
		return nil
	}
	d := terrainProfilerDeps{
		src:        dhm.NewWCSSource(proxy.client),
		logFailure: proxy.client.LogFailure,
		trusted:    parseCIDRListQuiet(cfg.WSTrustedProxies()),
	}
	if db != nil {
		d.observers = db.GetObservers
	}
	return newTerrainProfiler(cfg.DHM.Profile, cfg.Radio, d)
}

// ── Request grammar ────────────────────────────────────────────────────────

var (
	// Plain decimals only: no exponent, hex float, Inf, NaN or spaces.
	terrainCoordRe  = regexp.MustCompile(`^-?[0-9]{1,3}(\.[0-9]{1,10})?$`)
	terrainNumberRe = regexp.MustCompile(`^[0-9]{1,4}(\.[0-9]{1,6})?$`)
)

// terrainParams are the only accepted query keys.
var terrainParams = map[string]bool{"a": true, "b": true, "model": true, "ha": true, "hb": true, "k": true, "freq": true}

// terrainRef is one requested endpoint: a node or a point.
type terrainRef struct {
	pubkey string // lower-case; "" for a point
	pt     dhm.LatLon
}

type terrainQuery struct {
	a, b   terrainRef
	both   bool // model=both (default) rather than dtm
	ha, hb *float64
	k      *float64
	freq   *float64
}

var errTerrainQuery = dhm.ErrBadRequest

// errTerrainBusy: maxInFlight profile fetches are already running.
var errTerrainBusy = errors.New("terrain profiles busy")

func parseTerrainQuery(q url.Values) (terrainQuery, error) {
	var tq terrainQuery
	for k, vs := range q {
		if !terrainParams[k] || len(vs) != 1 || len(vs[0]) > 64 {
			return tq, errTerrainQuery
		}
	}
	var err error
	if tq.a, err = parseTerrainRef(q.Get("a")); err != nil {
		return tq, err
	}
	if tq.b, err = parseTerrainRef(q.Get("b")); err != nil {
		return tq, err
	}
	switch m, ok := q["model"]; {
	case !ok || m[0] == "both":
		tq.both = true
	case m[0] == "dtm":
	default:
		return tq, errTerrainQuery
	}
	for _, f := range []struct {
		key   string
		dst   **float64
		valid func(float64) bool
	}{
		{"ha", &tq.ha, validAntennaM},
		{"hb", &tq.hb, validAntennaM},
		{"k", &tq.k, validKFactor},
		{"freq", &tq.freq, validFreqMHz},
	} {
		vs, ok := q[f.key]
		if !ok {
			continue
		}
		v, ok := parseTerrainNumber(vs[0], terrainNumberRe)
		if !ok || !f.valid(v) {
			return tq, errTerrainQuery
		}
		*f.dst = &v
	}
	return tq, nil
}

func parseTerrainNumber(s string, re *regexp.Regexp) (float64, bool) {
	if !re.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

// parseTerrainRef accepts a 64-hex public key or "lat,lon".
func parseTerrainRef(s string) (terrainRef, error) {
	if pk := strings.ToLower(s); isHexPubkey(pk) {
		return terrainRef{pubkey: pk}, nil
	}
	lat, lon, ok := strings.Cut(s, ",")
	if !ok {
		return terrainRef{}, errTerrainQuery
	}
	la, ok1 := parseTerrainNumber(lat, terrainCoordRe)
	lo, ok2 := parseTerrainNumber(lon, terrainCoordRe)
	if !ok1 || !ok2 || la < -90 || la > 90 || lo < -180 || lo > 180 {
		return terrainRef{}, errTerrainQuery
	}
	return terrainRef{pt: dhm.LatLon{Lat: la, Lon: lo}}, nil
}

// ── Response ───────────────────────────────────────────────────────────────

// TerrainProfileResponse is the body of GET /api/terrain/profile.
type TerrainProfileResponse struct {
	A           TerrainEndpoint `json:"a"`
	B           TerrainEndpoint `json:"b"`
	DistanceM   float64         `json:"distanceM"`
	StepM       float64         `json:"stepM"`
	SampleCount int             `json:"sampleCount"` // computed; samples holds at most 1024 of them
	Model       string          `json:"model"`       // "dtm" or "both"
	Coverage    TerrainCoverage `json:"coverage"`
	Samples     []TerrainSample `json:"samples"`
	Physics     TerrainPhysics  `json:"physics"`
	Verdict     TerrainVerdict  `json:"verdict"`
	Attribution string          `json:"attribution"`
}

// TerrainEndpoint is one end of the path, snapped to 5 decimals.
type TerrainEndpoint struct {
	Lat           float64  `json:"lat"`
	Lon           float64  `json:"lon"`
	Kind          string   `json:"kind"` // "node" or "point"
	Role          string   `json:"role,omitempty"`
	GroundM       *float64 `json:"groundM"` // DTM; null outside coverage
	AntennaM      float64  `json:"antennaM"`
	AntennaSource string   `json:"antennaSource"` // request, operator-node, operator-role, role-default, point-default
	Assumed       bool     `json:"assumed"`       // the height is a default, not a known value
}

type TerrainCoverage struct {
	State     string   `json:"state"` // full, partial, none
	Pct       float64  `json:"pct"`
	FirstGapM *float64 `json:"firstGapM"` // distance of the first sample without data
}

// TerrainSample is one point along the path; null/absent where there is
// no elevation data.
type TerrainSample struct {
	D   float64  `json:"d"`
	DTM *float64 `json:"dtm"`
	DSM *float64 `json:"dsm,omitempty"`
}

type TerrainPhysics struct {
	FreqMHz    float64 `json:"freqMHz"`
	FreqSource string  `json:"freqSource"` // request, config, observers, firmware
	LambdaM    float64 `json:"lambdaM"`
	K          float64 `json:"k"`
	KSource    string  `json:"kSource"` // request, config, default
	F1MaxM     float64 `json:"f1MaxM"`  // first Fresnel radius at mid-path
}

type TerrainVerdict struct {
	State     string  `json:"state"` // clear, fresnel100, fresnel60, blocked, unknown
	Model     string  `json:"model"` // always "dtm": the terrain model decides
	Criterion float64 `json:"criterion"`
	TerrainClearance
	// Surface is the same assessment against the DSM (trees, buildings),
	// for information; present with model=both.
	Surface *TerrainSurfaceInfo `json:"surface,omitempty"`
}

// TerrainClearance is the worst point of an assessment; null without a
// verdict.
type TerrainClearance struct {
	WorstClearanceM *float64 `json:"worstClearanceM"`
	WorstAtM        *float64 `json:"worstAtM"`
	WorstF1Fraction *float64 `json:"worstF1Fraction"`
}

// TerrainSurfaceInfo is forced to "unknown" with null clearance numbers
// whenever Coverage.State is not "full", even though it is assessed
// against the DSM, not the deciding DTM: the two models can have
// different coverage, and a partial profile must never report a
// confident verdict on either (#689 review N1).
type TerrainSurfaceInfo struct {
	State string `json:"state"`
	TerrainClearance
}

// ── Handler ────────────────────────────────────────────────────────────────

// terrainKey is the cache key (plan §7): endpoints snapped to 5 decimals,
// model, sample count and step. The physics parameters are not part of
// it: they do not change the elevations, so a height or k change is a hit.
type terrainKey struct {
	A, B    [2]int64
	Both    bool
	Samples int
	StepCm  int64
}

func (k terrainKey) String() string {
	return fmt.Sprintf("%d,%d|%d,%d|%t|%d|%d", k.A[0], k.A[1], k.B[0], k.B[1], k.Both, k.Samples, k.StepCm)
}

func terrainSnapCoord(v float64) int64 { return int64(math.Round(v * terrainSnap)) }

// terrainEnd is a resolved endpoint before elevation.
type terrainEnd struct {
	pt     dhm.LatLon
	kind   string
	role   string
	pubkey string
}

// terrainRequest is a validated, resolved profile request: everything a
// handler needs before any elevation work. One path builds it
// (resolveTerrainRequest), whichever route the query came in on.
type terrainRequest struct {
	q      terrainQuery
	ends   [2]terrainEnd
	ka, kb [2]int64   // the snapped endpoints of the cache key
	a, b   dhm.LatLon // the same, as coordinates
	plan   dhm.Plan
}

// terrainRequestError is a refusal: its status and a fixed message that
// echoes nothing from the request.
type terrainRequestError struct {
	status int
	msg    string
}

// resolveTerrainRequest is the shared prelude after parsing: resolve both
// ends (hidden, unknown and unpositioned nodes are 404), snap them to the
// cache key's 5 decimals, gate the distance and plan the samples. It makes
// no upstream call and charges nothing.
func (s *Server) resolveTerrainRequest(p *terrainProfiler, tq terrainQuery) (terrainRequest, *terrainRequestError) {
	req := terrainRequest{q: tq}
	for i, ref := range [2]terrainRef{tq.a, tq.b} {
		e, ok := s.resolveTerrainEnd(ref)
		if !ok {
			return req, &terrainRequestError{http.StatusNotFound, "node not found"}
		}
		req.ends[i] = e
	}
	req.ka = [2]int64{terrainSnapCoord(req.ends[0].pt.Lat), terrainSnapCoord(req.ends[0].pt.Lon)}
	req.kb = [2]int64{terrainSnapCoord(req.ends[1].pt.Lat), terrainSnapCoord(req.ends[1].pt.Lon)}
	req.a = dhm.LatLon{Lat: float64(req.ka[0]) / terrainSnap, Lon: float64(req.ka[1]) / terrainSnap}
	req.b = dhm.LatLon{Lat: float64(req.kb[0]) / terrainSnap, Lon: float64(req.kb[1]) / terrainSnap}
	d := dhm.GroundDistance(req.a, req.b)
	if !(d >= 1) {
		return req, &terrainRequestError{http.StatusBadRequest, dhm.ErrBadRequest.Error()}
	}
	if d > p.set.maxPathM {
		return req, &terrainRequestError{http.StatusBadRequest, "path too long"}
	}
	req.plan = dhm.PlanProfile(req.a, req.b, p.set.plan)
	return req, nil
}

func (s *Server) handleTerrainProfile(w http.ResponseWriter, r *http.Request) {
	p := s.terrain
	if p == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	tq, err := parseTerrainQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, dhm.ErrBadRequest.Error())
		return
	}
	req, rerr := s.resolveTerrainRequest(p, tq)
	if rerr != nil {
		writeError(w, rerr.status, rerr.msg)
		return
	}
	plan := req.plan
	n := len(plan.E)

	var elev terrainElevation
	state := "none"
	if plan.Calls() == 0 {
		// Nothing of the path is inside DHM: no upstream call, no charge.
		elev = terrainElevation{dtm: nanSlice(n)}
		if tq.both {
			elev.dsm = nanSlice(n)
		}
	} else {
		key := terrainKey{A: req.ka, B: req.kb, Both: tq.both, Samples: n, StepCm: int64(math.Round(plan.StepM * 100))}
		data, hit := p.cache.Get(key, p.now())
		state = "hit"
		if !hit {
			state = "miss"
			if ok, retry := p.limiter.allow(r); !ok {
				writeRateLimited(w, retry)
				return
			}
			data, _, err = p.flight.Do(r.Context(), key.String(), p.deadline, func(ctx context.Context) ([]byte, error) {
				// Admission is per fetch, not per request: callers that
				// join a running fetch add no work and are not counted.
				select {
				case p.admit <- struct{}{}:
					defer func() { <-p.admit }()
				default:
					return nil, errTerrainBusy
				}
				e, err := p.fetch(ctx, &plan, tq.both)
				if err != nil {
					return nil, err
				}
				enc := e.encode()
				p.cache.Put(key, enc, p.now())
				return enc, nil
			})
			if err != nil {
				p.writeFetchError(w, r, err)
				return
			}
		}
		if elev, err = decodeTerrainElevation(data, n, tq.both); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	w.Header().Set("X-CoreScope-Cache", state)
	writeJSON(w, p.buildResponse(tq, req.ends, req.a, req.b, &plan, elev))
}

// writeFetchError maps a failed fetch: a client that left gets nothing,
// a timeout is 504, anything else upstream is 502. Neither carries
// upstream text; the log line carries the category only.
func (p *terrainProfiler) writeFetchError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(err, errTerrainBusy) {
		// A refusal did no upstream work: give the per-IP charge back, so
		// a client that retries as told never reaches 429 through 503s.
		p.limiter.refund(r)
		p.noteRefusal()
		setRetryAfter(w, terrainBusyRetry)
		writeError(w, http.StatusServiceUnavailable, errTerrainBusy.Error())
		return
	}
	if p.logFailure != nil {
		p.logFailure("terrain profile raster", err)
	}
	if dhm.FailureReason(err) == "timeout" || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "terrain data timed out")
		return
	}
	writeError(w, http.StatusBadGateway, "terrain data unavailable")
}

// noteRefusal counts an admission refusal and reports it at most once per
// terrainRefusalLogEvery: the first at once, then the number since the last
// report. Neither the token nor any URL is involved.
func (p *terrainProfiler) noteRefusal() {
	total := p.refused.Add(1)
	now := p.now()
	p.logMu.Lock()
	if !p.logAt.IsZero() && now.Sub(p.logAt) < terrainRefusalLogEvery {
		p.logMu.Unlock()
		return
	}
	n := total - p.logCount
	p.logAt, p.logCount = now, total
	p.logMu.Unlock()
	p.logf("[dhm] terrain profile admission: refused %d request(s) since the last report because map.dhm.profile.maxInFlight (%d) fetches are running; raise it if this persists", n, cap(p.admit))
}

// resolveTerrainEnd resolves a public key to a positioned, visible node,
// or passes a point through. Hidden, unknown and unpositioned nodes all
// look the same: not found.
func (s *Server) resolveTerrainEnd(ref terrainRef) (terrainEnd, bool) {
	if ref.pubkey == "" {
		return terrainEnd{pt: ref.pt, kind: "point"}, true
	}
	if s.store == nil {
		return terrainEnd{}, false
	}
	nodes, _ := s.store.getCachedNodesAndPM()
	for i := range nodes {
		n := &nodes[i]
		if !strings.EqualFold(n.PublicKey, ref.pubkey) {
			continue
		}
		if !n.HasGPS || !isValidNodePosition(n.Lat, n.Lon) || identityHidden(s.cfg, ref.pubkey, n.Name) {
			return terrainEnd{}, false
		}
		return terrainEnd{pt: dhm.LatLon{Lat: n.Lat, Lon: n.Lon}, kind: "node", role: n.Role, pubkey: ref.pubkey}, true
	}
	return terrainEnd{}, false
}

// terrainElevation is the cached part of a profile: elevations per
// sample, NaN without data; dsm is nil for model=dtm.
type terrainElevation struct{ dtm, dsm []float32 }

func nanSlice(n int) []float32 {
	out := make([]float32, n)
	nan := float32(math.NaN())
	for i := range out {
		out[i] = nan
	}
	return out
}

// fetch gets both models concurrently; the first failure cancels the other.
func (p *terrainProfiler) fetch(ctx context.Context, plan *dhm.Plan, both bool) (terrainElevation, error) {
	var e terrainElevation
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		e.dtm, err = plan.Fetch(gctx, p.src, dhm.ModelTerrain, p.set.upstreamTimeout)
		return err
	})
	if both {
		g.Go(func() (err error) {
			e.dsm, err = plan.Fetch(gctx, p.src, dhm.ModelSurface, p.set.upstreamTimeout)
			return err
		})
	}
	return e, g.Wait()
}

// encode is n, a DSM flag, then the float32s little-endian.
func (e terrainElevation) encode() []byte {
	n := len(e.dtm)
	out := make([]byte, 5, 5+8*n)
	binary.LittleEndian.PutUint32(out, uint32(n))
	if e.dsm != nil {
		out[4] = 1
	}
	for _, s := range [][]float32{e.dtm, e.dsm} {
		for _, v := range s {
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(v))
		}
	}
	return out
}

func decodeTerrainElevation(b []byte, n int, both bool) (terrainElevation, error) {
	models := 1
	if both {
		models = 2
	}
	if len(b) != 5+4*n*models || int(binary.LittleEndian.Uint32(b)) != n || (b[4] == 1) != both {
		return terrainElevation{}, errors.New("terrain cache entry does not match its key")
	}
	read := func(off int) []float32 {
		out := make([]float32, n)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[off+4*i:]))
		}
		return out
	}
	e := terrainElevation{dtm: read(5)}
	if both {
		e.dsm = read(5 + 4*n)
	}
	return e, nil
}

// ── Physics + response ─────────────────────────────────────────────────────

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func optional(v float32) *float64 {
	if math.IsNaN(float64(v)) {
		return nil
	}
	r := round2(float64(v))
	return &r
}

func (p *terrainProfiler) buildResponse(tq terrainQuery, ends [2]terrainEnd, a, b dhm.LatLon, plan *dhm.Plan, e terrainElevation) TerrainProfileResponse {
	n := len(e.dtm)
	resp := TerrainProfileResponse{
		DistanceM: round2(plan.DistanceM), StepM: round2(plan.StepM), SampleCount: n,
		Model: "dtm", Attribution: dhm.Attribution, Samples: []TerrainSample{},
	}
	if tq.both {
		resp.Model = "both"
	}

	// Coverage, by the model that decides.
	valid := 0
	for i, v := range e.dtm {
		if !math.IsNaN(float64(v)) {
			valid++
		} else if resp.Coverage.FirstGapM == nil {
			gap := round2(float64(i) * plan.StepM)
			resp.Coverage.FirstGapM = &gap
		}
	}
	resp.Coverage.Pct = math.Round(1000*float64(valid)/float64(n)) / 10
	switch valid {
	case n:
		resp.Coverage.State = "full"
	case 0:
		resp.Coverage.State = "none"
	default:
		resp.Coverage.State = "partial"
	}

	// Endpoints.
	overrides := [2]*float64{tq.ha, tq.hb}
	heights := [2]float64{}
	for i, end := range ends {
		pt := []dhm.LatLon{a, b}[i]
		ep := TerrainEndpoint{Lat: pt.Lat, Lon: pt.Lon, Kind: end.kind, Role: end.role,
			GroundM: optional(e.dtm[[]int{0, n - 1}[i]])}
		ep.AntennaM, ep.AntennaSource, ep.Assumed = p.antenna(end, overrides[i])
		heights[i] = math.NaN()
		if ep.GroundM != nil {
			heights[i] = float64(e.dtm[[]int{0, n - 1}[i]]) + ep.AntennaM
		}
		if i == 0 {
			resp.A = ep
		} else {
			resp.B = ep
		}
	}

	// Physics.
	ph := TerrainPhysics{K: dhm.DefaultKFactor, KSource: "default"}
	ph.FreqMHz, ph.FreqSource = p.frequency(tq.freq)
	switch {
	case tq.k != nil:
		ph.K, ph.KSource = *tq.k, "request"
	case p.radio.k != 0:
		ph.K, ph.KSource = p.radio.k, "config"
	}
	lambda := dhm.Wavelength(ph.FreqMHz)
	ph.LambdaM = math.Round(lambda*1e6) / 1e6
	ph.F1MaxM = round2(dhm.FresnelRadius(lambda, plan.DistanceM/2, plan.DistanceM/2))
	resp.Physics = ph

	// Verdict: the DTM decides, the DSM informs.
	link := dhm.Link{DistanceM: plan.DistanceM, HeightA: heights[0], HeightB: heights[1], LambdaM: lambda, K: ph.K}
	terrain := link.Assess(e.dtm)
	resp.Verdict = TerrainVerdict{State: string(terrain.State), Model: "dtm", Criterion: dhm.Criterion,
		TerrainClearance: clearanceOf(terrain)}
	keep := []int{terrain.WorstIndex}
	if e.dsm != nil {
		surface := link.Assess(e.dsm)
		// A partial profile never reports a confident verdict, even on the
		// model that only informs: the DSM can be a separate, complete
		// coverage while the deciding DTM has a gap (#689 review N1).
		if resp.Coverage.State != "full" {
			surface = dhm.Assessment{State: dhm.VerdictUnknown, WorstIndex: -1}
		}
		resp.Verdict.Surface = &TerrainSurfaceInfo{State: string(surface.State), TerrainClearance: clearanceOf(surface)}
		keep = append(keep, surface.WorstIndex)
	}

	if resp.Coverage.State != "none" {
		idx := dhm.Decimate(n, terrainMaxReturned, keep...)
		resp.Samples = make([]TerrainSample, len(idx))
		for j, i := range idx {
			s := TerrainSample{D: round2(float64(i) * plan.StepM), DTM: optional(e.dtm[i])}
			if e.dsm != nil {
				s.DSM = optional(e.dsm[i])
			}
			resp.Samples[j] = s
		}
	}
	return resp
}

func clearanceOf(a dhm.Assessment) TerrainClearance {
	if a.State == dhm.VerdictUnknown {
		return TerrainClearance{}
	}
	c, at, f := round2(a.WorstClearanceM), round2(a.WorstAtM), math.Round(a.WorstRatio*1000)/1000
	return TerrainClearance{WorstClearanceM: &c, WorstAtM: &at, WorstF1Fraction: &f}
}

// antenna is the antenna height above ground for an endpoint, most
// specific first: the request, the operator's per-node value, the
// operator's role value, the built-in role default.
func (p *terrainProfiler) antenna(e terrainEnd, override *float64) (h float64, source string, assumed bool) {
	if override != nil {
		return *override, "request", false
	}
	if e.pubkey != "" {
		if v, ok := p.radio.heights[e.pubkey]; ok {
			return v, "operator-node", false
		}
	}
	key, def := terrainPointKey, "point-default"
	if e.kind == "node" {
		key, def = e.role, "role-default"
		if _, ok := terrainRoleAntennaM[key]; !ok || key == terrainPointKey {
			key = "companion" // an unknown role gets the handheld height
		}
	}
	if v, ok := p.radio.heights[key]; ok {
		return v, "operator-role", true
	}
	return terrainRoleAntennaM[key], def, true
}

// frequency resolves the Fresnel frequency: the request, then the
// operator's config, then the modal frequency of recently seen observers,
// then the firmware build default (plan §4.1).
func (p *terrainProfiler) frequency(req *float64) (float64, string) {
	if req != nil {
		return *req, "request"
	}
	if p.radio.freqMHz != 0 {
		return p.radio.freqMHz, "config"
	}
	if f, ok := p.observerFreq(); ok {
		return f, "observers"
	}
	return dhm.FirmwareFreqMHz, "firmware"
}

// observerFreq is modalObserverFreq over the observers table, refreshed at
// most every terrainObserverFreqTTL.
func (p *terrainProfiler) observerFreq() (float64, bool) {
	p.freqMu.Lock()
	defer p.freqMu.Unlock()
	now := p.now()
	if !p.freqAt.IsZero() && now.Sub(p.freqAt) < terrainObserverFreqTTL {
		return p.freqVal, p.freqOK
	}
	p.freqVal, p.freqOK = 0, false
	if obs, err := p.observers(); err == nil {
		p.freqVal, p.freqOK = modalObserverFreq(obs, now)
	}
	p.freqAt = now
	return p.freqVal, p.freqOK
}

// radioFreqRe is the first field of observers.radio ("freq,bw,sf,cr"):
// a plain decimal, as public/observer-detail.js reads it.
var radioFreqRe = regexp.MustCompile(`^[0-9]{1,4}(\.[0-9]{1,9})?$`)

// modalObserverFreq is the most common radio frequency among observers
// seen within terrainObserverWindow, rounded to 1 kHz so 910.5250244 and
// 910.525 count together. Junk (empty, non-numeric, out of the firmware's
// 150-2500 MHz range, a trailing \r) is skipped. A tie goes to the lower
// frequency.
func modalObserverFreq(obs []Observer, now time.Time) (float64, bool) {
	counts := map[int64]int{}
	for _, o := range obs {
		if o.Radio == nil || o.LastSeen == nil {
			continue
		}
		seen, ok := parseObserverTime(*o.LastSeen)
		if !ok || now.Sub(seen) > terrainObserverWindow {
			continue
		}
		first, _, _ := strings.Cut(*o.Radio, ",")
		f, ok := parseTerrainNumber(strings.TrimSpace(first), radioFreqRe)
		if !ok || !validFreqMHz(f) {
			continue
		}
		counts[int64(math.Round(f*1000))]++
	}
	best, bestN := int64(0), 0
	for khz, c := range counts {
		if c > bestN || c == bestN && khz < best {
			best, bestN = khz, c
		}
	}
	return float64(best) / 1000, bestN > 0
}

func parseObserverTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
