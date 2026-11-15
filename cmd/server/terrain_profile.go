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
//   - With map.dhm.profile.foreignFill.enabled (off by default), the
//     samples DHM does not cover are filled from Copernicus DEM GLO-30
//     (internal/copdem, #738), beside the DHM rasters and within their
//     own budget of 16 calls. Only the DTM is filled: the DSM stays a gap
//     abroad, so a mixed profile's surface verdict is "unknown". The
//     response says which stretches were filled, at which resolution, and
//     carries the Copernicus credit. A failed fill leaves the profile
//     partial with no verdict, as before. Its DHM part is cached as
//     usual; a cache hit tries only the fill again. A path with no DHM
//     sample still makes no upstream call at all.
//   - With map.dhm.profile.buildings.enabled (off by default), the DSM's
//     elevated samples are told apart as buildings or trees/other from
//     INSPIRE building footprints, inside the same fetch, slot and charge
//     (terrain_buildings.go, #730 M2). Off, the answer is unchanged.
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/profilecard"
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
	// worst points are always among them, and the data gaps stay visible
	// (dhm.Decimate, terrainGapKeep).
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
	// share is map.dhm.profile.share (#727), see share_profile.go.
	share shareSettings
	// card is map.dhm.profile.card (#727 M3), see profile_card.go.
	card cardSettings
	// fill is map.dhm.profile.foreignFill (#738).
	fill foreignFillSettings
	// buildings is map.dhm.profile.buildings.enabled (#730 M2).
	buildings bool
}

// foreignFillSettings are map.dhm.profile.foreignFill, resolved and
// bounded.
type foreignFillSettings struct {
	enabled         bool
	blockCacheBytes int
	timeout         time.Duration
}

// shareSettings are map.dhm.profile.share, resolved and bounded.
type shareSettings struct {
	enabled bool
	// upstreamPerMin is the server-wide budget of upstream profile
	// fetches that sharing may cause: the share card spends it on an
	// elevation cache miss (profile_card.go). /p itself never fetches.
	upstreamPerMin int
}

// shareUpstreamPerMinMax bounds share.upstreamPerMin: one fetch a second.
const shareUpstreamPerMinMax = 60

// resolveTerrainSettings applies the defaults and bounds. An out-of-range
// value logs a warning and takes the default.
func resolveTerrainSettings(pc dhm.ProfileConfig, logf func(string, ...interface{})) terrainSettings {
	k := configKnobs{tag: "[dhm]", block: "map.dhm.profile", logf: logf}
	num, integer := k.num, k.integer
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
		// Off unless the operator opts in (#727 owner decision 7); 0 is a
		// legitimate budget: sharing never goes upstream.
		share: shareSettings{
			enabled:        pc.Share.Enabled,
			upstreamPerMin: integer("share.upstreamPerMin", pc.Share.UpstreamPerMin, 2, 0, shareUpstreamPerMinMax),
		},
		card: cardSettings{
			dark:         cardTheme(pc.Card.Theme, logf),
			names:        pc.Card.Names == nil || *pc.Card.Names,
			cacheEntries: integer("card.cacheEntries", pc.Card.CacheEntries, cardCacheEntriesDefault, 1, cardCacheEntriesMax),
		},
		// Off unless the operator opts in (#738 decision 3): it sends
		// requests to a third party.
		fill: foreignFillSettings{
			enabled:         pc.ForeignFill.Enabled,
			blockCacheBytes: integer("foreignFill.blockCacheMB", pc.ForeignFill.BlockCacheMB, 32, 1, 256) << 20,
			timeout:         time.Duration(integer("foreignFill.timeoutMs", pc.ForeignFill.TimeoutMs, 10000, 500, 10000)) * time.Millisecond,
		},
		// Off unless the operator opts in (#730 M2): it asks a second
		// service, so its traffic stays the operator's choice.
		buildings: pc.Buildings.Enabled,
	}
}

// fillOptions are the foreign fill client's limits for set. Its admission
// holds every admitted profile's whole budget, so the profile's own
// admission (maxInFlight) is the bound that bites.
func fillOptions(set terrainSettings) copdem.Options {
	o := copdem.DefaultOptions()
	o.BlockCacheBytes = set.fill.blockCacheBytes
	o.Timeout = set.fill.timeout
	o.MaxPending = set.maxInFlight * dhm.MaxCallsPerModel
	return o
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

// terrainFiller fills the samples DHM does not cover (#738):
// *copdem.Client, or a fake in tests.
type terrainFiller interface {
	Fill(ctx context.Context, plan *dhm.Plan) (copdem.Result, error)
}

type terrainProfiler struct {
	src        dhm.ElevationSource
	fill       terrainFiller // nil: map.dhm.profile.foreignFill is off
	fillFailed atomic.Int64  // failed fills since start
	fillLog    logThrottle   // the fill failure report, once per terrainRefusalLogEvery
	set        terrainSettings
	radio      terrainRadio
	cache      *dhm.Cache[terrainKey]
	flight     dhm.Flight[[]byte]
	limiter    *ipRateLimiter // nil: no per-client limit
	admit      *dhm.Admission // one slot per profile fetch in flight (maxInFlight)
	refused    atomic.Int64   // admission refusals (503) since start
	refuseLog  logThrottle    // the refusal report, once per terrainRefusalLogEvery
	// card is the share card (#727 M3); nil unless sharing is enabled.
	card       *profileCards
	deadline   time.Duration
	now        func() time.Time
	logf       func(string, ...interface{})
	logFailure func(what string, err error)
	observers  func() ([]Observer, error)

	// buildings tells buildings from trees (#730 M2); nil while
	// map.dhm.profile.buildings is off. buildingsDeadline bounds its phase.
	buildings         terrainBuildingSource
	buildingsDeadline time.Duration

	freqMu  sync.Mutex
	freqAt  time.Time
	freqVal float64
	freqOK  bool
}

type terrainProfilerDeps struct {
	src dhm.ElevationSource
	// fill replaces the Copernicus client built from the settings
	// (tests); it is used only when the fill is enabled.
	fill       terrainFiller
	logf       func(string, ...interface{})
	logFailure func(what string, err error)
	observers  func() ([]Observer, error)
	trusted    []*net.IPNet
	// theme is the operator's theme sections for the share card's
	// palette (cardThemeSections); nil means the shipped palette.
	theme func() (light, dark map[string]string)

	// buildings replaces the INSPIRE source built from the settings
	// (tests); it is used only while the classification is enabled.
	buildings terrainBuildingSource
}

// terrainCacheMaxBytes is the cache's byte bound: one entry holds n DTM + n
// DSM float32 plus a 5-byte header, n at most maxSamples, and with the
// building classification (#730 M2) a 2-byte state and one class byte
// per sample. dhm.Cache evicts on entry count OR total bytes; by
// construction every entry here is at most 1/cacheEntries of this bound,
// so the byte condition can never fire before the entry count does (#689
// review N3). It is kept anyway,
// deliberately redundant, as defense-in-depth against a future change
// decoupling entry size from maxSamples, and because dhm.Cache is shared
// with the DHM tile cache (dhm_proxy.go), where variable tile sizes make
// the byte bound load-bearing — the two call sites should stay consistent.
// At the documented maxima (cacheEntries 4096, maxSamples 3200) this is
// ~105 MB, ~118 MB classified; see config.example.json's
// _comment_cacheEntries.
func terrainCacheMaxBytes(cacheEntries, maxSamples int, classified bool) int {
	if classified {
		return cacheEntries * (5 + 8*maxSamples + 2 + maxSamples)
	}
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
	maxBytes := terrainCacheMaxBytes(set.cacheEntries, set.plan.MaxSamples, set.buildings)
	var bsrc terrainBuildingSource
	if set.buildings {
		bsrc = d.buildings
		if bsrc == nil {
			bsrc = newTerrainBuildingSource(set, buildings.DefaultBaseURL)
		}
	}
	var fill terrainFiller
	if set.fill.enabled {
		fill = d.fill
		if fill == nil {
			// Its own client: the DHM token and DHM's upstream slots are
			// never near it.
			fill = copdem.NewClient(fillOptions(set))
		}
	}
	p := &terrainProfiler{
		src:        d.src,
		fill:       fill,
		set:        set,
		radio:      resolveTerrainRadio(rc, d.logf),
		cache:      dhm.NewCache[terrainKey](set.cacheEntries, maxBytes, set.cacheTTL),
		limiter:    newIPRateLimiter("dhm", "map.dhm.profile.rateLimitPerMin", set.rateLimitPerMin, d.trusted, d.logf),
		admit:      dhm.NewAdmission(set.maxInFlight),
		deadline:   terrainDeadline,
		now:        time.Now,
		logf:       d.logf,
		logFailure: d.logFailure,
		observers:  d.observers,

		buildings:         bsrc,
		buildingsDeadline: terrainBuildingsDeadline,
	}
	if set.share.enabled {
		p.card = newProfileCards(set, d.theme, d.logf)
	}
	return p
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
	// The card's palette is read once, like the og: tags (og_meta.go):
	// a theme.json change takes effect at the next start, and the
	// palette fingerprint in the card key and in v makes it visible.
	d.theme = func() (map[string]string, map[string]string) {
		return cardThemeSections(cfg, LoadTheme("."))
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

// The body of GET /api/terrain/profile is internal/profilecard's
// TerrainProfileResponse: the share-card renderer takes the response as it
// is served (#727 M3), so the types live there and are aliased here. The
// field-by-field documentation is in internal/profilecard/types.go;
// profilecard_shape_test.go still pins the JSON shape.
type (
	TerrainProfileResponse = profilecard.TerrainProfileResponse
	TerrainEndpoint        = profilecard.TerrainEndpoint
	TerrainCoverage        = profilecard.TerrainCoverage
	TerrainFillRange       = profilecard.TerrainFillRange
	TerrainSample          = profilecard.TerrainSample
	TerrainPhysics         = profilecard.TerrainPhysics
	TerrainVerdict         = profilecard.TerrainVerdict
	TerrainClearance       = profilecard.TerrainClearance
	TerrainSurfaceInfo     = profilecard.TerrainSurfaceInfo
	TerrainClassification  = profilecard.TerrainClassification
)

// terrainMaxFillRanges bounds Coverage.Fill.
const terrainMaxFillRanges = 64

const terrainReasonAntennaBelowSurface = "antennaBelowSurface"

// ── Handler ────────────────────────────────────────────────────────────────

// terrainKey is the cache key (plan §7): endpoints snapped to 5 decimals,
// model, sample count and step. The physics parameters are not part of
// it: they do not change the elevations, so a height or k change is a hit.
type terrainKey struct {
	A, B    [2]int64
	Both    bool
	Samples int
	StepCm  int64
	// Fill is whether the foreign fill is on (#738), so an entry made
	// without it is never served as a profile with it, or back.
	Fill bool
}

func (k terrainKey) String() string {
	return fmt.Sprintf("%d,%d|%d,%d|%t|%d|%d|%t", k.A[0], k.A[1], k.B[0], k.B[1], k.Both, k.Samples, k.StepCm, k.Fill)
}

func terrainSnapCoord(v float64) int64 { return int64(math.Round(v * terrainSnap)) }

// terrainEnd is a resolved endpoint before elevation.
type terrainEnd struct {
	pt     dhm.LatLon
	kind   string
	role   string
	pubkey string
	// name is the node's display name, for the share preview (#727). It
	// is only ever set for a node that passed the visibility gate in
	// resolveTerrainEnd; the JSON response never carries it.
	name string
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

// elevationKey is the elevation cache key of a resolved request.
func (p *terrainProfiler) elevationKey(req terrainRequest) terrainKey {
	return terrainKey{A: req.ka, B: req.kb, Both: req.q.both, Samples: len(req.plan.E), StepCm: int64(math.Round(req.plan.StepM * 100)), Fill: p.fill != nil}
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
// no upstream call and charges nothing. ctx bounds the identity-visibility
// lookup in resolveTerrainEnd (#743).
func (s *Server) resolveTerrainRequest(ctx context.Context, p *terrainProfiler, tq terrainQuery) (terrainRequest, *terrainRequestError) {
	req := terrainRequest{q: tq}
	for i, ref := range [2]terrainRef{tq.a, tq.b} {
		e, ok := s.resolveTerrainEnd(ctx, ref)
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
	req, rerr := s.resolveTerrainRequest(r.Context(), p, tq)
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
		if p.classifies(tq.both) {
			elev.classes = unclassified(n)
		}
	} else {
		key := p.elevationKey(req)
		data, hit := p.cache.Get(key, p.now())
		// An entry of another format (#730 M2) is fetched afresh, not served.
		hit = hit && terrainEntryOK(data, n, tq.both, p.classifies(tq.both))
		state = "hit"
		if !hit {
			state = "miss"
			if ok, retry := p.limiter.allow(r); !ok {
				writeRateLimited(w, retry)
				return
			}
			// Admission is per fetch, not per request: callers that join
			// a running fetch add no work and take no slot.
			data, _, err = p.flight.DoAdmitted(r.Context(), key.String(), p.deadline, p.admit, p.elevationCall(key, &plan, tq.both))
			if err != nil {
				p.writeFetchError(w, r, err)
				return
			}
		}
		if elev, err = decodeTerrainElevation(data, n, tq.both, p.classifies(tq.both)); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if hit && elev.fillFailed {
			elev = p.retryFill(r, key, &plan, elev)
		}
	}
	w.Header().Set("X-CoreScope-Cache", state)
	writeJSON(w, p.buildResponse(tq, req.ends, req.a, req.b, &plan, elev))
}

// elevationCall is the body of one shared elevation fetch: both models,
// encoded and cached. Every route that fetches elevations runs it through
// p.flight under p.admit, so a card and a panel asking for the same path
// share one fetch and one maxInFlight budget.
func (p *terrainProfiler) elevationCall(key terrainKey, plan *dhm.Plan, both bool) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		e, err := p.fetch(ctx, plan, both)
		if err != nil {
			return nil, err
		}
		if p.classifies(both) {
			if e.classes, err = p.classifyBuildings(ctx, plan, e); err != nil {
				return nil, err
			}
		}
		// A profile left partial by a failed fill is kept too: the DHM
		// part is good, and a hit retries only the fill (#738). One whose
		// building lookup failed is kept briefly (#730).
		enc := e.encode()
		p.cache.PutFor(key, enc, p.now(), e.classes.ttl(p.set.cacheTTL))
		return enc, nil
	}
}

// isTerrainBusy reports an admission refusal: no slot, no upstream work.
func isTerrainBusy(err error) bool {
	return errors.Is(err, errTerrainBusy) || errors.Is(err, dhm.ErrAdmissionFull)
}

// writeFetchError maps a failed fetch: a client that left gets nothing,
// a timeout is 504, anything else upstream is 502. Neither carries
// upstream text; the log line carries the category only.
func (p *terrainProfiler) writeFetchError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	if isTerrainBusy(err) {
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
	n, ok := p.refuseLog.note(p.now(), p.refused.Add(1), terrainRefusalLogEvery)
	if !ok {
		return
	}
	p.logf("[dhm] terrain profile admission: refused %d request(s) since the last report because map.dhm.profile.maxInFlight (%d) fetches are running; raise it if this persists", n, p.admit.Cap())
}

// resolveTerrainEnd resolves a public key to a positioned, visible node,
// or passes a point through. Hidden, unknown and unpositioned nodes all
// look the same: not found. Visibility is the identity policy Reach uses
// (identityHidden over hiddenIdentityNames: blacklists plus the live node,
// inactive-node and observer-alias names, #743), decided before any
// coordinate is read. The lookup runs for every node endpoint, known or
// not, so the work done does not tell a hidden identity apart from a
// missing one; a failed lookup fails closed. Unlike isIdentityHidden it
// does not skip the lookup for a blacklisted key, which would make
// blacklisted endpoints measurably cheaper than unknown ones.
func (s *Server) resolveTerrainEnd(ctx context.Context, ref terrainRef) (terrainEnd, bool) {
	if ref.pubkey == "" {
		return terrainEnd{pt: ref.pt, kind: "point"}, true
	}
	names, err := s.hiddenIdentityNames(ctx, []string{ref.pubkey})
	if err != nil || identityHidden(s.cfg, ref.pubkey, names[strings.ToLower(ref.pubkey)]...) {
		if err != nil && s.terrain != nil {
			s.terrain.logf("[dhm] terrain profile: node visibility lookup failed; endpoint refused")
		}
		return terrainEnd{}, false
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
		return terrainEnd{pt: dhm.LatLon{Lat: n.Lat, Lon: n.Lon}, kind: "node", role: n.Role, pubkey: ref.pubkey, name: n.Name}, true
	}
	return terrainEnd{}, false
}

// terrainElevation is the cached part of a profile: elevations per
// sample, NaN without data; dsm is nil for model=dtm. fill is the
// resolution the foreign fill gave the DTM (#738), fillFailed that it
// was tried and failed, so a cache hit tries it again.
type terrainElevation struct {
	dtm, dsm   []float32
	fill       int
	fillFailed bool
	// classes is the building classification (#730 M2), nil when there
	// is none.
	classes *terrainClasses
}

// classifies reports whether a profile of this model is classified: the
// classification is on, and there is a surface to classify.
func (p *terrainProfiler) classifies(both bool) bool { return p.buildings != nil && both }

// The foreign fill levels of a terrainElevation.
const (
	fillNone     = iota
	fillFull     // GLO-30 at full resolution, about 30 m
	fillOverview // the ½ overview, about 60 m
)

// fillResolutionM is TerrainFillRange.ResolutionM per fill level.
var fillResolutionM = [...]int{fillNone: 0, fillFull: 30, fillOverview: 60}

func nanSlice(n int) []float32 {
	out := make([]float32, n)
	nan := float32(math.NaN())
	for i := range out {
		out[i] = nan
	}
	return out
}

// fetch gets both models concurrently, and the foreign fill beside them
// (#738); the first DHM failure cancels the rest. A failed fill only
// leaves the profile partial (fillFailed).
func (p *terrainProfiler) fetch(ctx context.Context, plan *dhm.Plan, both bool) (terrainElevation, error) {
	var e terrainElevation
	fill := p.startFill(ctx, plan)
	defer fill.stop() // whichever way fetch returns
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
	if err := g.Wait(); err != nil {
		return e, err
	}
	p.applyFill(ctx, &e, plan, fill)
	return e, nil
}

// pendingFill is a foreign fill running beside the DHM rasters.
type pendingFill struct {
	done   chan fillOutcome
	cancel context.CancelFunc
}

type fillOutcome struct {
	res copdem.Result
	err error
}

// startFill starts the fill of plan's uncovered samples; nil when the
// fill is off or DHM covers every sample.
func (p *terrainProfiler) startFill(ctx context.Context, plan *dhm.Plan) *pendingFill {
	if p.fill == nil || plan.Covered() == len(plan.E) {
		return nil
	}
	fctx, cancel := context.WithCancel(ctx)
	f := &pendingFill{done: make(chan fillOutcome, 1), cancel: cancel}
	go func() {
		res, err := p.fill.Fill(fctx, plan)
		f.done <- fillOutcome{res, err}
	}()
	return f
}

// stop cancels the fill without waiting for it.
func (f *pendingFill) stop() {
	if f != nil {
		f.cancel()
	}
}

// applyFill waits for the fill and writes its heights into the DTM, at
// uncovered samples only. A failure is counted and logged by category,
// unless the profile's own context ended.
func (p *terrainProfiler) applyFill(ctx context.Context, e *terrainElevation, plan *dhm.Plan, f *pendingFill) {
	if f == nil {
		return
	}
	o := <-f.done
	if o.err == nil && len(o.res.Values) != len(e.dtm) {
		o.err = dhm.FetchError{Reason: "fill mismatch"}
	}
	if o.err != nil {
		e.fillFailed = true
		if ctx.Err() == nil {
			p.noteFillFailure(o.err)
		}
		return
	}
	n := 0
	for i, v := range o.res.Values {
		if !plan.Inside(i) && !math.IsNaN(float64(v)) {
			e.dtm[i] = v
			n++
		}
	}
	if n > 0 {
		e.fill = fillFull + min(o.res.Level, 1)
	}
}

// retryFill tries the foreign fill again for a cached profile whose fill
// failed. The DHM part comes from the cache, so a failing Copernicus
// never re-fetches DHM (#773 review finding 1). The retry is upstream
// work: it is charged to the per-client budget like a miss and shared by
// concurrent callers. A success replaces the cache entry; whatever stops
// the retry, the cached partial profile e is served as it is.
func (p *terrainProfiler) retryFill(r *http.Request, key terrainKey, plan *dhm.Plan, e terrainElevation) terrainElevation {
	if ok, _ := p.limiter.allow(r); !ok {
		return e
	}
	data, _, err := p.flight.Do(r.Context(), key.String()+"|fill", p.deadline, func(ctx context.Context) ([]byte, error) {
		// The fill writes into its own copy: this caller may leave, and
		// serve e, while the retry still runs for the others.
		f := terrainElevation{dtm: slices.Clone(e.dtm), dsm: e.dsm, classes: e.classes}
		p.applyFill(ctx, &f, plan, p.startFill(ctx, plan))
		enc := f.encode()
		if !f.fillFailed {
			p.cache.Put(key, enc, p.now())
		}
		return enc, nil
	})
	if err != nil {
		return e
	}
	f, err := decodeTerrainElevation(data, len(e.dtm), e.dsm != nil, e.classes != nil)
	if err != nil {
		return e
	}
	return f
}

// noteFillFailure counts a failed fill and reports it at most once per
// terrainRefusalLogEvery, by category: no URL, no upstream text.
func (p *terrainProfiler) noteFillFailure(err error) {
	total := p.fillFailed.Add(1)
	n, ok := p.fillLog.note(p.now(), total, terrainRefusalLogEvery)
	if !ok {
		return
	}
	p.logf("[dhm] terrain profile foreign fill failed (%s): %d profile(s) left partial since the last report", dhm.FailureReason(err), n)
}

// encode is n, a flags byte (bit 0: a DSM; bits 1–2: the fill level;
// bit 3: the fill failed; bit 4: the building classification, #730 M2),
// then the float32s little-endian, and then the classification
// (terrainClasses.appendTo).
func (e terrainElevation) encode() []byte {
	n := len(e.dtm)
	size := 5 + 4*n
	if e.dsm != nil {
		size += 4 * n
	}
	if e.classes != nil {
		size += 2 + n
	}
	out := make([]byte, 5, size)
	binary.LittleEndian.PutUint32(out, uint32(n))
	if e.dsm != nil {
		out[4] = 1
	}
	out[4] |= byte(e.fill) << 1
	if e.fillFailed {
		out[4] |= 1 << 3
	}
	if e.classes != nil {
		out[4] |= terrainEntryClassified
	}
	for _, s := range [][]float32{e.dtm, e.dsm} {
		for _, v := range s {
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(v))
		}
	}
	if e.classes != nil {
		out = e.classes.appendTo(out)
	}
	return out
}

func decodeTerrainElevation(b []byte, n int, both, classified bool) (terrainElevation, error) {
	models := 1
	if both {
		models = 2
	}
	if !terrainEntryOK(b, n, both, classified) || int(binary.LittleEndian.Uint32(b)) != n {
		return terrainElevation{}, errTerrainEntry
	}
	level, failed := int(b[4]>>1&3), b[4]&(1<<3) != 0
	if b[4]>>5 != 0 || level > fillOverview || (failed && level != fillNone) {
		return terrainElevation{}, errTerrainEntry
	}
	read := func(off int) []float32 {
		out := make([]float32, n)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[off+4*i:]))
		}
		return out
	}
	e := terrainElevation{dtm: read(5), fill: level, fillFailed: failed}
	if both {
		e.dsm = read(5 + 4*n)
	}
	if classified {
		var err error
		if e.classes, err = decodeTerrainClasses(b[5+4*n*models:], n); err != nil {
			return terrainElevation{}, err
		}
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
		// The profile is computed from DHM, not shown as served, so the
		// credit is the derived one (#733).
		Model: "dtm", Attribution: dhm.AttributionDerived, Samples: []TerrainSample{},
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
	// #738: an uncovered sample with a height has it from the fill.
	if e.fill != fillNone {
		filled := make([]bool, n)
		for i, v := range e.dtm {
			filled[i] = !plan.Inside(i) && !math.IsNaN(float64(v))
			resp.Coverage.Mixed = resp.Coverage.Mixed || filled[i]
		}
		if resp.Coverage.Mixed {
			resp.Coverage.Fill = terrainFillRanges(filled, plan.StepM, fillResolutionM[e.fill])
			resp.AttributionFill = copdem.AttributionDerived
		}
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
	var ph TerrainPhysics
	ph.FreqMHz, ph.FreqSource, ph.K, ph.KSource = p.linkParams(tq)
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
		// coverage while the deciding DTM has a gap (#689 review N1). Nor
		// does a mixed one: there is no surface model abroad (#738).
		if resp.Coverage.State != "full" || resp.Coverage.Mixed {
			surface = dhm.Assessment{State: dhm.VerdictUnknown, WorstIndex: -1}
		}
		resp.Verdict.Surface = surfaceInfo(surface)
		keep = append(keep, surface.WorstIndex)
		if e.classes != nil {
			resp.Verdict.Surface.Cause = e.classes.surfaceCause(link.Obstructions(e.dsm, surface), e)
		}
	}
	if e.classes != nil {
		resp.Classification = e.classes.response()
		if e.classes.used() {
			resp.Attribution = buildings.AttributionDerived
		}
	}

	if resp.Coverage.State != "none" {
		// The gaps get the places the endpoints and the worst points leave
		// (#745 review N4). The two worst points can only coincide on a
		// profile without gaps, so neither is counted twice.
		room := terrainMaxReturned - 2
		for _, k := range keep {
			if k > 0 && k < n-1 {
				room--
			}
		}
		keep = append(keep, terrainGapKeep(room, e.dtm, e.dsm)...)
		idx := dhm.Decimate(n, terrainMaxReturned, keep...)
		resp.Samples = make([]TerrainSample, len(idx))
		for j, i := range idx {
			s := TerrainSample{D: round2(float64(i) * plan.StepM), DTM: optional(e.dtm[i])}
			if e.dsm != nil {
				s.DSM = optional(e.dsm[i])
			}
			if e.classes != nil {
				s.Cls = e.classes.sampleClass(i)
			}
			resp.Samples[j] = s
		}
	}
	return resp
}

// terrainFillRanges is the stretches of filled samples, at most
// terrainMaxFillRanges: beyond that the stretches with the smallest DHM
// gaps between them are merged first.
func terrainFillRanges(filled []bool, stepM float64, resolutionM int) []TerrainFillRange {
	var runs [][2]int
	for i := 0; i < len(filled); i++ {
		if !filled[i] {
			continue
		}
		j := i
		for j+1 < len(filled) && filled[j+1] {
			j++
		}
		runs = append(runs, [2]int{i, j})
		i = j
	}
	if len(runs) == 0 {
		return nil
	}
	// merge[k] joins run k to run k+1: the smallest gaps, ties to the
	// earlier, until terrainMaxFillRanges remain.
	merge := make([]bool, len(runs))
	if excess := len(runs) - terrainMaxFillRanges; excess > 0 {
		gaps := make([]int, len(runs)-1)
		for k := range gaps {
			gaps[k] = k
		}
		sort.SliceStable(gaps, func(a, b int) bool {
			return runs[gaps[a]+1][0]-runs[gaps[a]][1] < runs[gaps[b]+1][0]-runs[gaps[b]][1]
		})
		for _, k := range gaps[:excess] {
			merge[k] = true
		}
	}
	out := make([]TerrainFillRange, 0, min(len(runs), terrainMaxFillRanges))
	for k := 0; k < len(runs); k++ {
		first := runs[k][0]
		for merge[k] {
			k++
		}
		out = append(out, TerrainFillRange{
			FromM: round2(float64(first) * stepM), ToM: round2(float64(runs[k][1]) * stepM),
			Source: copdem.Source, ResolutionM: resolutionM,
		})
	}
	return out
}

// terrainGapKeep lists the samples that keep the data gaps of the series
// visible through dhm.Decimate (#745), most important first, for room
// places: the first missing sample of each gap, then its last missing
// sample and the samples just outside it, so a chart gap starts and ends
// where the data does. Decimate takes them while they fit, and then has
// no room for evenly spaced samples that could join two valid ones across
// a gap. With more gaps than room, the room goes to missing samples spread
// evenly over all of them: a gap left without a sample of its own merges
// into its neighbours, none is bridged by a line, and no evenly spaced
// sample is left. All series have the same length; a nil
// one after the first is skipped, but series[0] must not be nil, since
// the overflow walks its indices.
func terrainGapKeep(room int, series ...[]float32) []int {
	var starts, edges []int
	for _, s := range series {
		for i := 0; i < len(s); i++ {
			if !math.IsNaN(float64(s[i])) {
				continue
			}
			j := i
			for j+1 < len(s) && math.IsNaN(float64(s[j+1])) {
				j++
			}
			starts = append(starts, i)
			edges = append(edges, j, i-1, j+1)
			i = j
		}
	}
	if len(starts) <= room {
		return append(starts, edges...)
	}
	if room <= 0 {
		return nil
	}
	var missing []int
	for i := range series[0] {
		for _, s := range series {
			if s != nil && math.IsNaN(float64(s[i])) {
				missing = append(missing, i)
				break
			}
		}
	}
	out := make([]int, room)
	for j := range out {
		out[j] = missing[int(math.Round(float64(j)*float64(len(missing)-1)/float64(max(room-1, 1))))]
	}
	return out
}

// surfaceInfo is the informational DSM verdict, with the reason when an
// antenna is below the surface (#745).
func surfaceInfo(a dhm.Assessment) *TerrainSurfaceInfo {
	out := &TerrainSurfaceInfo{State: string(a.State), TerrainClearance: clearanceOf(a)}
	switch {
	case a.AntennaBelowA && a.AntennaBelowB:
		out.Endpoint = "both"
	case a.AntennaBelowA:
		out.Endpoint = "A"
	case a.AntennaBelowB:
		out.Endpoint = "B"
	}
	if out.Endpoint != "" {
		out.Reason = terrainReasonAntennaBelowSurface
	}
	return out
}

func clearanceOf(a dhm.Assessment) TerrainClearance {
	if a.State == dhm.VerdictUnknown {
		return TerrainClearance{}
	}
	c, at := round2(a.WorstClearanceM), round2(a.WorstAtM)
	out := TerrainClearance{WorstClearanceM: &c, WorstAtM: &at}
	// An obstruction at an antenna has no fraction: F1 has no radius
	// there, and the ratio is −Inf (#745), which JSON cannot carry.
	if !math.IsInf(a.WorstRatio, 0) {
		f := math.Round(a.WorstRatio*1000) / 1000
		out.WorstF1Fraction = &f
	}
	return out
}

// linkParams resolves the request's frequency and k-factor, with where
// each came from: the request, the operator's config, or a default.
func (p *terrainProfiler) linkParams(tq terrainQuery) (freq float64, freqSource string, k float64, kSource string) {
	freq, freqSource = p.frequency(tq.freq)
	switch {
	case tq.k != nil:
		return freq, freqSource, *tq.k, "request"
	case p.radio.k != 0:
		return freq, freqSource, p.radio.k, "config"
	}
	return freq, freqSource, dhm.DefaultKFactor, "default"
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
