/* #681 M4 — Path profile: the terrain / line-of-sight / Fresnel chart and
 * its panel, for the "Path profile" measure tool on the Map page.
 *
 * Self-contained on purpose. It knows nothing about map.js: the host passes
 * in a mount point and a node resolver, and gets back a controller that owns
 * the panel DOM, the request to GET /api/terrain/profile, the chart and the
 * #/map?profile=… deep link. M5 can therefore open the same tool from node
 * detail without duplicating any of it.
 *
 * The server decides the verdict (cmd/server/terrain_profile.go). This file
 * only draws what the response says and never re-judges it — a partial
 * profile has verdict.state "unknown" upstream and must stay unjudged here.
 *
 * Nothing is fetched, and no listener is attached, until the tool is armed:
 * the panel DOM is built on first open and one request is issued per
 * profile, with the height inputs debounced.
 */
(function (global) {
  'use strict';

  /* Local escaper for the chart's SVG string sinks. One definition only —
   * test-xss-escape-sinks.js lifts it and would refuse a second. */
  function esc(s) { return s == null ? '' : String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }

  // The single CC BY 4.0 credit line, same text as internal/dhm (#680).
  var ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
  var LICENCE_MARK = 'CC BY 4.0';
  var ATTRIBUTION_URL = 'https://creativecommons.org/licenses/by/4.0/';
  var ASSUMED_LABEL = 'assumed heights';
  var ENDPOINT = '/api/terrain/profile';
  var HASH_PARAM = 'profile';
  var MAX_HEIGHT_M = 100;
  var EARTH_RADIUS_M = 6371000;      // internal/dhm/physics.go EarthRadiusM
  var DEFAULT_K = 4 / 3;
  var STORAGE_PREFIX = 'mc-terrain-h:';
  var POINT_STORAGE_KEY = 'point';
  var HEIGHT_DEBOUNCE_MS = 350;
  // A profile parameter is two references; 64-hex keys and 5-decimal
  // coordinates fit well inside this, and anything longer is junk.
  var MAX_PROFILE_PARAM = 200;

  // Mirrors the server grammar (cmd/server/terrain_profile.go): plain
  // decimals only, so NaN, Infinity, hex floats and exponents never parse.
  var PUBKEY_RE = /^[0-9a-f]{64}$/;
  var COORD_RE = /^-?[0-9]{1,3}(\.[0-9]{1,10})?$/;
  // k and freq ride the same link, so they get the same grammar as the
  // coordinates rather than Number()'s: "0x5", "1e1", "  5" and "Infinity"
  // are link junk, not values. Four integer digits cover freq up to 2500.
  var DECIMAL_RE = /^-?[0-9]{1,6}(\.[0-9]{1,10})?$/;

  // ── Customizer defaults (#681 M6) ────────────────────────────────────────

  /* The customizer owns a default antenna height per endpoint kind and a
   * default elevation model (public/customize-v2.js, section `terrain`).
   * They reach us on window.SITE_CONFIG — server config merged with this
   * viewer's own customizer overrides — and they are DEFAULTS: the server's
   * own built-in role heights are all they replace.
   *
   * Height precedence, most specific first:
   *   1. the deep link (?ha=/?hb=)                 badge "link"
   *   2. the height this browser remembers          badge "yours"
   *   3. radio.antennaHeightsM in server config     badge "operator"
   *   4. this section                               badge "default"
   *   5. the server's built-in role defaults        badge "default"
   *
   * A customizer height therefore never overrules an operator who named a
   * real height for a node or a role, and it is never written into a shared
   * link: a link carries decisions, not somebody else's ambient defaults.
   */
  var THEME_HEIGHT_KEYS = {
    repeater: 'heightRepeater', room: 'heightRoom',
    companion: 'heightCompanion', sensor: 'heightSensor'
  };
  var THEME_POINT_KEY = 'heightPoint';
  var DEFAULT_MODEL = 'both';
  /* The antenna sources the server reports when it had nothing but its own
   * built-in default — the only ones a customizer default may replace. */
  var PROMOTABLE_SOURCES = { 'role-default': true, 'point-default': true };

  function validModel(m) { return m === 'dtm' || m === 'both'; }

  // ── Numbers ──────────────────────────────────────────────────────────────

  function plainNumber(s) { return typeof s === 'string' && COORD_RE.test(s) ? Number(s) : null; }

  function round(v, places) { var f = Math.pow(10, places); return Math.round(v * f) / f; }
  function r2(v) { return round(v, 2); }

  /* clampHeight folds any input into the 0-100 m antenna range, or null when
   * it is not a number at all. Objects are rejected outright: Number([]) is
   * 0, which would silently become a valid 0 m antenna. */
  function clampHeight(v) {
    if (v === null || v === undefined || v === '' || typeof v === 'object' || typeof v === 'boolean') return null;
    var n = Number(v);
    if (!isFinite(n)) return null;
    if (n < 0) n = 0;
    if (n > MAX_HEIGHT_M) n = MAX_HEIGHT_M;
    return round(n, 1);
  }

  /* themeDefaults resolves the customizer's terrain section. Pass the raw
   * section to resolve a specific one; omit it to read the live config.
   * Junk is not a default: an unusable value falls through to the server. */
  function themeDefaults(section) {
    var src = (section === undefined)
      ? (global.SITE_CONFIG && global.SITE_CONFIG.terrain)
      : section;
    var out = { model: DEFAULT_MODEL, heights: {} };
    if (!src || typeof src !== 'object') return out;
    if (validModel(src.model)) out.model = src.model;
    Object.keys(THEME_HEIGHT_KEYS).forEach(function (role) {
      var h = clampHeight(src[THEME_HEIGHT_KEYS[role]]);
      if (h !== null) out.heights[role] = h;
    });
    var pt = clampHeight(src[THEME_POINT_KEY]);
    if (pt !== null) out.heights[POINT_STORAGE_KEY] = pt;
    return out;
  }

  /* modelOf picks the elevation model for a state or a link: an explicit
   * value, else the customizer's default, else terrain + surface. */
  function modelOf(raw, defaults) {
    if (validModel(raw)) return raw;
    if (defaults && validModel(defaults.model)) return defaults.model;
    return themeDefaults().model;
  }

  /* themeHeight is the customizer's default for one endpoint of a response,
   * or null when it has none. An unrecognised role takes the handheld
   * height, exactly as cmd/server/terrain_profile.go does. */
  function themeHeight(end, defaults) {
    if (!end || !defaults || !defaults.heights) return null;
    var role = POINT_STORAGE_KEY;
    if (end.kind === 'node') {
      role = Object.prototype.hasOwnProperty.call(THEME_HEIGHT_KEYS, end.role) ? end.role : 'companion';
    }
    var v = defaults.heights[role];
    return v === undefined ? null : v;
  }

  function boundedNumber(v, lo, hi, places) {
    if (v === null || v === undefined || v === '' || typeof v === 'object' || typeof v === 'boolean') return null;
    if (typeof v === 'string' && !DECIMAL_RE.test(v)) return null;
    var n = Number(v);
    if (!isFinite(n) || n < lo || n > hi) return null;
    return round(n, places);
  }

  // ── Deep link ────────────────────────────────────────────────────────────

  /* parseProfilePair reads the profile parameter left to right: a 64-hex
   * public key is one endpoint, anything else must be a lat,lon pair. That
   * makes "<key>,<lat>,<lon>" and "<lat>,<lon>,<lat>,<lon>" unambiguous
   * without a second separator. Anything that does not consume exactly two
   * endpoints is rejected whole — a half-parsed path is not a path. */
  function parseProfilePair(raw) {
    if (typeof raw !== 'string' || raw === '' || raw.length > MAX_PROFILE_PARAM) return null;
    var tok = raw.split(',');
    var refs = [];
    var i = 0;
    while (i < tok.length && refs.length < 3) {
      var pk = String(tok[i]).toLowerCase();
      if (PUBKEY_RE.test(pk)) { refs.push({ kind: 'node', pubkey: pk }); i += 1; continue; }
      if (i + 1 >= tok.length) return null;
      var lat = plainNumber(tok[i]);
      var lon = plainNumber(tok[i + 1]);
      if (lat === null || lon === null) return null;
      if (lat < -90 || lat > 90 || lon < -180 || lon > 180) return null;
      refs.push({ kind: 'point', lat: lat, lon: lon });
      i += 2;
    }
    return (refs.length === 2 && i === tok.length) ? refs : null;
  }

  function toParams(input) {
    if (!input && input !== '') return null;
    if (typeof input === 'string') return new URLSearchParams(input.charAt(0) === '?' ? input.slice(1) : input);
    if (typeof input.get === 'function') return input;
    return null;
  }

  /* parseState returns null when the pair itself is unusable — that is the
   * one thing the link cannot do without. A junk model/k/freq falls back to
   * the default instead, so one bad control never costs the whole link. */
  function parseState(input, defaults) {
    var p = toParams(input);
    if (!p) return null;
    var refs = parseProfilePair(p.get(HASH_PARAM));
    if (!refs) return null;
    return {
      a: refs[0],
      b: refs[1],
      model: modelOf(p.get('model'), defaults),
      ha: clampHeight(p.get('ha')),
      hb: clampHeight(p.get('hb')),
      k: boundedNumber(p.get('k'), 0.5, 10, 4),
      freq: boundedNumber(p.get('freq'), 150, 2500, 4)
    };
  }

  /* parseStart reads the half link node detail opens the tool with:
   * profile=<one public key> means "A is this node, pick B". It is a
   * separate reader on purpose, so a lone key can never pass parseState
   * and be measured as half a path. */
  function parseStart(input) {
    var p = toParams(input);
    if (!p) return null;
    var raw = p.get(HASH_PARAM);
    if (typeof raw !== 'string') return null;
    var pk = raw.toLowerCase();
    return PUBKEY_RE.test(pk) ? { kind: 'node', pubkey: pk } : null;
  }

  function fmtCoord(v) { return Number(v).toFixed(5); }

  function refToParam(ref) {
    if (!ref) return null;
    if (ref.kind === 'node') return ref.pubkey;
    return fmtCoord(ref.lat) + ',' + fmtCoord(ref.lon);
  }

  function refKey(ref) {
    if (!ref) return null;
    return ref.kind === 'node' ? ref.pubkey : POINT_STORAGE_KEY;
  }

  function sameRef(x, y) {
    if (!x || !y || x.kind !== y.kind) return false;
    return x.kind === 'node' ? x.pubkey === y.pubkey : (x.lat === y.lat && x.lon === y.lon);
  }

  /* serializeState writes only what differs from the defaults, so a plain
   * two-node link stays short and the round trip is exact. */
  function serializeState(state, defaults) {
    var out = {};
    if (!state || !state.a || !state.b) return out;
    out[HASH_PARAM] = refToParam(state.a) + ',' + refToParam(state.b);
    /* The model rides the link whenever it differs from the default this
     * viewer would otherwise get, so a link is exact in both directions:
     * with a customizer default of "dtm", choosing terrain + surface has to
     * be written down or reopening the link would silently undo it. */
    if (validModel(state.model) && state.model !== modelOf(null, defaults)) out.model = state.model;
    if (state.ha !== null && state.ha !== undefined) out.ha = String(state.ha);
    if (state.hb !== null && state.hb !== undefined) out.hb = String(state.hb);
    if (state.k !== null && state.k !== undefined) out.k = String(state.k);
    if (state.freq !== null && state.freq !== undefined) out.freq = String(state.freq);
    return out;
  }

  /* requestURL is the only place a terrain request is built. It carries no
   * credential of any kind: the DHM token lives on the server. */
  function requestURL(state) {
    var p = new URLSearchParams();
    p.set('a', refToParam(state.a));
    p.set('b', refToParam(state.b));
    /* Always explicit: the server's own default is "both", so a customizer
     * default of "dtm" has to be stated or the request would quietly come
     * back with a surface model nobody asked for. */
    p.set('model', state.model === 'dtm' ? 'dtm' : 'both');
    var ser = serializeState(state, { model: state.model });
    ['ha', 'hb', 'k', 'freq'].forEach(function (key) { if (ser[key] !== undefined) p.set(key, ser[key]); });
    return ENDPOINT + '?' + p.toString();
  }

  /* shareURL is the link the panel's copy button hands out: built from the
   * state, not read off the address bar, so it is the same whether or not
   * the host lets the panel write the hash. */
  function shareURL(state, loc, defaults) {
    if (!state || !state.a || !state.b || !loc) return null;
    return String(loc.origin || '') + String(loc.pathname || '/') + '#/map?' + new URLSearchParams(serializeState(state, defaults)).toString();
  }

  // ── Node-detail entry point ──────────────────────────────────────────────

  /* offered is the one gate for the whole tool: the server only sends a dhm
   * block with profile: true when map.dhm.profile.enabled is set and a
   * token is configured. The Map page and node detail both ask it. */
  function offered(cfg) {
    return !!(cfg && cfg.enabled === true && cfg.profile === true);
  }

  /* hasFix is the node-detail "real fix" rule (public/nodes.js): both
   * coordinates present and in range, and not the 0,0 no-lock sentinel. An
   * estimated position does not count — the server measures from the
   * node's own lat/lon. */
  function hasFix(node) {
    if (!node || node.lat === null || node.lat === undefined || node.lat === ''
      || node.lon === null || node.lon === undefined || node.lon === '') return false;
    var lat = Number(node.lat), lon = Number(node.lon);
    if (!isFinite(lat) || !isFinite(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180) return false;
    return !(lat === 0 && lon === 0);
  }

  /* nodeEntryHash is the deep link node detail offers: the Map tool with A
   * filled in and the picker waiting for B. null means "offer nothing". */
  function nodeEntryHash(node, cfg) {
    if (!offered(cfg) || !hasFix(node)) return null;
    var pk = String(node.public_key || '').toLowerCase();
    if (!PUBKEY_RE.test(pk)) return null;
    var p = new URLSearchParams();
    p.set(HASH_PARAM, refToParam({ kind: 'node', pubkey: pk }));
    return '#/map?' + p.toString();
  }

  /* nodeEntryAction is the node-detail button, built as DOM so no node
   * field ever passes through an HTML string. */
  function nodeEntryAction(node, cfg) {
    var hash = nodeEntryHash(node, cfg);
    if (!hash) return null;
    var a = el('a', 'btn-primary node-terrain-action');
    // hash is '#/map?profile=' + 64 hex, so esc() changes nothing; it keeps
    // the href sink provably clean for the XSS preflight.
    a.setAttribute('href', esc(hash));
    a.setAttribute('data-testid', 'node-terrain-profile');
    a.setAttribute('title', 'Path profile from this node: terrain, line of sight and Fresnel zone. Pick the other end on the map.');
    // Static markup, no interpolation: the Phosphor sprite icon.
    var icon = el('span', 'node-terrain-icon');
    icon.setAttribute('aria-hidden', 'true');
    icon.innerHTML = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-ruler"/></svg>';
    a.appendChild(icon);
    a.appendChild(el('span', null, 'Terrain profile to…'));
    return a;
  }

  // ── Per-viewer height memory ─────────────────────────────────────────────

  function storage() {
    try { return global.localStorage || null; } catch (_) { return null; }
  }

  function recallHeight(ref) {
    var key = refKey(ref);
    if (!key) return null;
    try {
      var s = storage();
      return s ? clampHeight(s.getItem(STORAGE_PREFIX + key)) : null;
    } catch (_) { return null; }
  }

  function rememberHeight(ref, value) {
    var key = refKey(ref);
    var h = clampHeight(value);
    if (!key || h === null) return;
    try {
      var s = storage();
      if (s) s.setItem(STORAGE_PREFIX + key, String(h));
    } catch (_) { /* private mode, quota, disabled storage */ }
  }

  // ── Verdict ──────────────────────────────────────────────────────────────

  var VERDICTS = {
    clear: { label: 'Clear', tone: 'ok', detail: 'The whole first Fresnel zone is clear of terrain.' },
    fresnel100: { label: 'Usable', tone: 'ok', detail: 'The first Fresnel zone is partly obstructed, but at least 60 % of it stays clear.' },
    fresnel60: { label: 'Marginal', tone: 'warn', detail: 'Line of sight is clear, but less than 60 % of the first Fresnel zone is.' },
    blocked: { label: 'Blocked', tone: 'bad', detail: 'Terrain rises through the line of sight.' }
  };

  /* The verdict states that assert terrain inside the criterion zone, and
   * so the only ones whose chart may carry intrusion bands. */
  var INTRUDING_VERDICTS = { fresnel60: true, blocked: true };

  function fmtPct(v) {
    var n = Number(v);
    return isFinite(n) ? String(round(n, 1)) : '0';
  }

  /* covered answers "is this profile judgeable at all". Coverage outranks
   * verdict.state: a path with a hole in it was never measured end to end,
   * so no verdict on it can be trusted, whatever the response says. The
   * server already forces unknown in that case (internal/dhm), and holding
   * the same line here means a stale cache, a proxy or a future server
   * cannot make this panel show a confident green on an unmeasured path.
   * A response with no coverage block at all is not evidence of a gap. */
  function covered(data) {
    var cov = (data && data.coverage) || {};
    return !cov.state || cov.state === 'full';
  }

  /* verdictChip maps the server's verdict to the chip. Anything the server
   * may add later lands on "No verdict" rather than on a confident tone:
   * guessing green on an unknown state is the one harmful failure here. */
  function verdictChip(data) {
    var v = (data && data.verdict) || {};
    var cov = (data && data.coverage) || {};
    var known = covered(data) && Object.prototype.hasOwnProperty.call(VERDICTS, v.state) ? VERDICTS[v.state] : null;
    if (known) return { state: v.state, label: known.label, tone: known.tone, detail: known.detail };
    var detail;
    if (cov.state === 'partial') {
      detail = 'No verdict, path leaves Denmark. The profile covers ' + fmtPct(cov.pct) + ' % of the path.';
    } else if (cov.state === 'none') {
      detail = 'Terrain data is available for Denmark only. This path lies outside coverage.';
    } else {
      detail = 'No elevation data for this path, so there is no clearance verdict.';
    }
    return { state: 'unknown', label: 'No verdict', tone: 'unknown', detail: detail };
  }

  // ── Physics, for drawing only ────────────────────────────────────────────

  function earthBulge(d1, d2, k) { return d1 * d2 / (2 * k * EARTH_RADIUS_M); }

  function fresnelRadius(lambda, d1, d2) {
    if (!(lambda > 0) || d1 <= 0 || d2 <= 0) return 0;
    return Math.sqrt(lambda * d1 * d2 / (d1 + d2));
  }

  // ── Chart geometry ───────────────────────────────────────────────────────

  var VIEW = { w: 820, h: 300 };
  var PAD = { l: 46, r: 16, t: 14, b: 30 };
  var Y_PAD_FRACTION = 0.08;

  function endpointGround(end, samples, from, step) {
    if (end && end.groundM !== null && end.groundM !== undefined && isFinite(Number(end.groundM))) return Number(end.groundM);
    for (var i = from; i >= 0 && i < samples.length; i += step) {
      var g = samples[i].dtm;
      if (g !== null && g !== undefined && isFinite(Number(g))) return Number(g);
    }
    return null;
  }

  /* chartGeometry turns one /api/terrain/profile response into plot
   * coordinates. Elevations are drawn with the earth bulge added, which is
   * how the server assesses them, so what the eye reads off the chart and
   * what the verdict says can never disagree.
   *
   * All arithmetic runs on raw metres; only the values handed back are
   * rounded to two decimals, and the y axis uses the same rounded domain it
   * reports, so a caller can reproduce every coordinate from the output. */
  function chartGeometry(data) {
    if (!data || !data.samples || !data.samples.length) return null;
    var D = Number(data.distanceM);
    if (!isFinite(D) || D <= 0) return null;
    var samples = data.samples;
    var n = samples.length;
    var phys = data.physics || {};
    var lambda = Number(phys.lambdaM) > 0 ? Number(phys.lambdaM) : 0;
    var k = Number(phys.k) > 0 ? Number(phys.k) : DEFAULT_K;
    var both = data.model === 'both';
    var plot = { x: PAD.l, y: PAD.t, w: VIEW.w - PAD.l - PAD.r, h: VIEW.h - PAD.t - PAD.b };

    var zA = endpointGround(data.a, samples, 0, 1);
    var zB = endpointGround(data.b, samples, n - 1, -1);
    if (zA === null || zB === null) return null;
    zA += Number((data.a && data.a.antennaM) || 0);
    zB += Number((data.b && data.b.antennaM) || 0);

    var d = [], xs = [], terrain = [], surface = both ? [] : null, f1 = [], los = [];
    var lo = Infinity, hi = -Infinity;
    for (var i = 0; i < n; i++) {
      var di = Number(samples[i].d);
      var bulge = earthBulge(di, D - di, k);
      var g = samples[i].dtm;
      var t = (g === null || g === undefined) ? null : Number(g) + bulge;
      var s = null;
      if (both) {
        var sv = samples[i].dsm;
        s = (sv === null || sv === undefined) ? null : Number(sv) + bulge;
        surface.push(s);
      }
      var r = fresnelRadius(lambda, di, D - di);
      var l = zA + (zB - zA) * (di / D);
      d.push(di);
      xs.push(plot.x + plot.w * (di / D));
      terrain.push(t);
      f1.push(r);
      los.push(l);
      if (t !== null) { lo = Math.min(lo, t); hi = Math.max(hi, t); }
      if (s !== null) { lo = Math.min(lo, s); hi = Math.max(hi, s); }
      lo = Math.min(lo, l - r);
      hi = Math.max(hi, l + r);
    }
    if (!isFinite(lo) || !isFinite(hi)) return null;
    var pad = Math.max(1, (hi - lo) * Y_PAD_FRACTION);
    var zMin = r2(lo - pad), zMax = r2(hi + pad);
    var span = zMax - zMin || 1;
    function yOf(z) { return r2(plot.y + plot.h * (zMax - z) / span); }

    // Contiguous runs of samples with elevation, and the gaps between them.
    var segments = [], gaps = [], run = null, gap = null;
    for (i = 0; i < n; i++) {
      if (terrain[i] === null) {
        if (run) { segments.push(run); run = null; }
        if (!gap) gap = { fromM: d[i], toM: d[i], x1: r2(xs[i]), x2: r2(xs[i]) };
        else { gap.toM = d[i]; gap.x2 = r2(xs[i]); }
      } else {
        if (gap) { gaps.push(gap); gap = null; }
        if (!run) run = { from: i, to: i };
        else run.to = i;
      }
    }
    if (run) segments.push(run);
    if (gap) gaps.push(gap);

    /* Intrusion runs: where the clearance below the line of sight is under
     * the server's criterion fraction of the Fresnel radius.
     *
     * These runs are the server's verdict made visible, never a second
     * opinion on it. The server judged the profile at full resolution;
     * `samples` is decimated for transport, so a verdict recomputed from
     * it could disagree with the chip. The drawing is therefore gated on
     * the two states that already assert an intrusion into the criterion
     * zone: "clear" and "fresnel100" assert the opposite, "unknown"
     * asserts nothing, and a profile with a gap is unjudgeable. Bands and
     * chip tone can only appear together. */
    var intrusions = [];
    var verdict = data.verdict || {};
    var criterion = Number(verdict.criterion);
    if (!isFinite(criterion)) criterion = 0.6;
    if (covered(data) && INTRUDING_VERDICTS[verdict.state] === true) {
      var cur = null;
      for (i = 0; i < n; i++) {
        var hit = terrain[i] !== null && f1[i] > 0 && (los[i] - terrain[i]) < criterion * f1[i];
        if (hit) {
          if (!cur) cur = { fromM: d[i], toM: d[i], x1: r2(xs[i]), x2: r2(xs[i]) };
          else { cur.toM = d[i]; cur.x2 = r2(xs[i]); }
        } else if (cur) { intrusions.push(cur); cur = null; }
      }
      if (cur) intrusions.push(cur);
    }

    return {
      view: { w: VIEW.w, h: VIEW.h },
      plot: plot,
      domain: { dMax: D, zMin: zMin, zMax: zMax },
      d: d,
      x: xs.map(r2),
      terrainZ: terrain.map(function (v) { return v === null ? null : r2(v); }),
      terrainY: terrain.map(function (v) { return v === null ? null : yOf(v); }),
      surfaceY: surface ? surface.map(function (v) { return v === null ? null : yOf(v); }) : null,
      losY: los.map(yOf),
      f1: f1.map(r2),
      f1TopY: los.map(function (l, j) { return yOf(l + f1[j]); }),
      f1BotY: los.map(function (l, j) { return yOf(l - f1[j]); }),
      f60TopY: los.map(function (l, j) { return yOf(l + criterion * f1[j]); }),
      f60BotY: los.map(function (l, j) { return yOf(l - criterion * f1[j]); }),
      baseY: r2(plot.y + plot.h),
      segments: segments,
      gaps: gaps,
      intrusions: intrusions,
      criterion: criterion
    };
  }

  // ── Chart SVG ────────────────────────────────────────────────────────────

  function fmtMetres(v) {
    var n = Number(v);
    if (!isFinite(n)) return '—';
    return (Math.abs(n) >= 10000 ? round(n / 1000, 1) + ' km' : round(n, 1) + ' m');
  }

  /* The chart box is a fixed 820x300 viewBox stretched to whatever width
   * the panel has. SVG <text> inside it is stretched with everything else,
   * so on a 360 px phone the axis labels came out about 3 px tall and
   * squashed sideways. Every label is therefore HTML, absolutely
   * positioned over the plot in percentages of the very same box: real
   * text at a real font size, selectable, and honouring the viewer's font
   * scaling. The percentages are exact because preserveAspectRatio="none"
   * maps the viewBox onto the box linearly in each axis. */
  function pctX(x) { return round(x / VIEW.w * 100, 3); }
  function pctY(y) { return round(y / VIEW.h * 100, 3); }

  function label(cls, x, y, text) {
    return '<span class="' + cls + '" style="left:' + pctX(x) + '%;top:' + pctY(y) + '%">' + esc(text) + '</span>';
  }

  function polyline(xs, ys, from, to) {
    var pts = [];
    for (var i = from; i <= to; i++) pts.push(xs[i] + ',' + ys[i]);
    return pts.join(' ');
  }

  /* chartTitle is the words a screen reader gets instead of the picture. */
  function chartTitle(data, chip, names) {
    var a = names.nameA || 'A', b = names.nameB || 'B';
    var txt = 'Terrain profile from ' + a + ' to ' + b + ', ' + fmtMetres(data.distanceM) + '. ' + chip.label + ': ' + chip.detail;
    var worst = data.verdict && data.verdict.worstClearanceM;
    if (worst !== null && worst !== undefined && isFinite(Number(worst))) {
      txt += ' Worst clearance ' + fmtMetres(worst) + ' at ' + fmtMetres(data.verdict.worstAtM) + ' along the path.';
    }
    return txt;
  }

  /* buildChart renders the profile as one inline SVG string, following the
   * public/analytics.js convention (no chart library). Layers, bottom to
   * top: DTM fill → DSM clutter band and line → 60 % Fresnel → full Fresnel
   * → line of sight → intrusion markers → endpoint pins. Every colour is a
   * CSS variable so the customizer can reach them later (M6). */
  function buildChart(data, names) {
    names = names || {};
    var g = chartGeometry(data);
    var chip = verdictChip(data);
    if (!g) {
      return '<div class="tp-plot tp-plot-empty">'
        + '<svg viewBox="0 0 ' + VIEW.w + ' ' + VIEW.h + '" class="tp-svg" role="img" aria-label="No terrain profile">'
        + '<title>' + esc(chip.detail) + '</title></svg>'
        + '<p class="tp-plot-msg">' + esc(chip.detail) + '</p></div>';
    }
    var p = g.plot;
    var labels = [];
    var out = ['<div class="tp-plot">'];
    out.push('<svg viewBox="0 0 ' + VIEW.w + ' ' + VIEW.h + '" class="tp-svg" preserveAspectRatio="none" role="img" aria-label="Terrain profile chart">');
    out.push('<title>' + esc(chartTitle(data, chip, names)) + '</title>');

    // Horizontal grid + elevation ticks.
    for (var t = 0; t <= 4; t++) {
      var z = g.domain.zMax - (g.domain.zMax - g.domain.zMin) * t / 4;
      var y = r2(p.y + p.h * t / 4);
      out.push('<line x1="' + p.x + '" y1="' + y + '" x2="' + (p.x + p.w) + '" y2="' + y + '" stroke="var(--border)" stroke-width="0.5" stroke-dasharray="2 3"/>');
      labels.push(label('tp-ax tp-ax-y', p.x, y, String(Math.round(z))));
    }

    // 1. DTM fill, one polygon per covered stretch so a gap stays a gap.
    g.segments.forEach(function (seg) {
      var pts = polyline(g.x, g.terrainY, seg.from, seg.to);
      out.push('<polygon points="' + g.x[seg.from] + ',' + g.baseY + ' ' + pts + ' ' + g.x[seg.to] + ',' + g.baseY
        + '" fill="var(--terrain-fill)" stroke="var(--terrain-fill)" stroke-width="1"/>');
    });

    // 2. DSM: the clutter band between terrain and surface, plus its line.
    if (g.surfaceY) {
      g.segments.forEach(function (seg) {
        var up = [], down = [];
        for (var i = seg.from; i <= seg.to; i++) {
          if (g.surfaceY[i] === null) continue;
          up.push(g.x[i] + ',' + g.surfaceY[i]);
          down.unshift(g.x[i] + ',' + g.terrainY[i]);
        }
        if (up.length < 2) return;
        out.push('<polygon points="' + up.join(' ') + ' ' + down.join(' ') + '" fill="var(--terrain-clutter)" stroke="none"/>');
        out.push('<polyline points="' + up.join(' ') + '" fill="none" stroke="var(--terrain-clutter)" stroke-width="1.2"/>');
      });
    }

    // 3/4. Fresnel bands: 60 % inside the full first zone.
    out.push('<polyline points="' + polyline(g.x, g.f60TopY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-60)" stroke-width="1" stroke-dasharray="4 3"/>');
    out.push('<polyline points="' + polyline(g.x, g.f60BotY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-60)" stroke-width="1" stroke-dasharray="4 3"/>');
    out.push('<polyline points="' + polyline(g.x, g.f1TopY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-100)" stroke-width="1.2"/>');
    out.push('<polyline points="' + polyline(g.x, g.f1BotY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-100)" stroke-width="1.2"/>');

    // 5. Line of sight.
    out.push('<line x1="' + g.x[0] + '" y1="' + g.losY[0] + '" x2="' + g.x[g.x.length - 1] + '" y2="' + g.losY[g.losY.length - 1]
      + '" stroke="var(--los-line)" stroke-width="1.6"/>');

    // 6. Intrusion markers: a band on the baseline per obstructed stretch,
    //    and a ring on the worst sample the server reported.
    g.intrusions.forEach(function (run) {
      var w = Math.max(2, r2(run.x2 - run.x1));
      out.push('<rect x="' + run.x1 + '" y="' + r2(g.baseY - 5) + '" width="' + w + '" height="5" fill="var(--profile-blocked)" class="tp-intrusion"/>');
    });
    var worstAt = data.verdict && data.verdict.worstAtM;
    if (g.intrusions.length && worstAt !== null && worstAt !== undefined && isFinite(Number(worstAt))) {
      var wx = r2(p.x + p.w * Math.min(1, Math.max(0, Number(worstAt) / g.domain.dMax)));
      out.push('<line class="tp-worst" x1="' + wx + '" y1="' + p.y + '" x2="' + wx + '" y2="' + g.baseY + '" stroke="var(--profile-blocked)" stroke-width="1" stroke-dasharray="3 3"/>');
    }

    // 7. Out-of-coverage stretches: left blank, labelled once.
    g.gaps.forEach(function (gp) {
      var mid = r2((gp.x1 + gp.x2) / 2);
      out.push('<rect x="' + gp.x1 + '" y="' + p.y + '" width="' + Math.max(2, r2(gp.x2 - gp.x1)) + '" height="' + p.h + '" fill="none" stroke="var(--border)" stroke-width="0.8" stroke-dasharray="3 4"/>');
      labels.push(label('tp-ax tp-ax-gap', mid, r2(g.baseY - 8), 'no data'));
    });

    // 8. Endpoint pins: the mast from ground to antenna, with the label.
    [{ x: g.x[0], y: g.losY[0], g: g.terrainY[0], tag: 'A', cls: 'tp-pin-a', dx: 3 },
     { x: g.x[g.x.length - 1], y: g.losY[g.losY.length - 1], g: g.terrainY[g.terrainY.length - 1], tag: 'B', cls: 'tp-pin-b', dx: -3 }]
      .forEach(function (pin) {
        var foot = pin.g === null ? g.baseY : pin.g;
        out.push('<line x1="' + pin.x + '" y1="' + foot + '" x2="' + pin.x + '" y2="' + pin.y + '" stroke="var(--los-line)" stroke-width="1.6"/>');
        out.push('<circle cx="' + pin.x + '" cy="' + pin.y + '" r="3" fill="var(--los-line)"/>');
        labels.push(label('tp-pin-lab ' + pin.cls, r2(pin.x + pin.dx), r2(pin.y - 7), pin.tag));
      });

    // Distance axis, as HTML for the same reason as the elevation ticks.
    labels.push(label('tp-ax tp-ax-x0', p.x, VIEW.h - 14, '0'));
    labels.push(label('tp-ax tp-ax-x1', p.x + p.w, VIEW.h - 14, fmtMetres(data.distanceM)));
    out.push('</svg>');
    out.push(labels.join(''));
    out.push('</div>');
    return out.join('');
  }

  // ── Panel ────────────────────────────────────────────────────────────────

  function el(tag, cls, text) {
    var e = global.document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }

  function endpointLabel(ref, name) {
    if (!ref) return 'not picked';
    if (ref.kind === 'node') return name || (ref.pubkey.slice(0, 8) + '…');
    return Number(ref.lat).toFixed(4) + ', ' + Number(ref.lon).toFixed(4);
  }

  var SOURCE_LABELS = {
    request: 'from this link',
    'operator-node': 'set by the operator for this node',
    'operator-role': 'operator default for the role',
    'role-default': 'role default for this node type',
    'point-default': 'default for a map point'
  };

  /* A "request" height is one the browser sent, and only the browser knows
   * why: the viewer typed or remembered it, the customizer supplied it, or
   * it arrived on a link. The server cannot tell these apart, so the
   * wording is resolved here alongside the badge. */
  var ORIGIN_LABELS = {
    yours: 'remembered in this browser',
    theme: 'default for this endpoint, from Display settings'
  };

  function sourceLabel(source, origin) {
    if (source === 'request' && ORIGIN_LABELS[origin]) return ORIGIN_LABELS[origin];
    return SOURCE_LABELS[source] || '';
  }

  /* assumedHeight answers "is this height still a guess". The server says
   * so for its own defaults, but a height the customizer supplied comes
   * back as a plain request value — and a class-wide default is exactly as
   * much of an assumption as the server's own. Keeping it flagged is what
   * stops the M5 caveat disappearing the moment an operator edits the
   * customizer. */
  function assumedHeight(end, origin) {
    return !!(end && end.assumed) || origin === 'theme';
  }

  /* Where an antenna height comes from, as a one-word badge. The server
   * knows defaults and operator values; a "request" height is one the
   * browser sent, and only the browser knows whether the viewer typed or
   * remembered it ("yours") or it arrived on somebody else's link. */
  var HEIGHT_BADGES = {
    default: { label: 'default', title: 'A built-in default for this kind of endpoint, not a measured height.' },
    operator: { label: 'operator', title: 'Set by the operator of this instance (radio.antennaHeightsM).' },
    yours: { label: 'yours', title: 'Your own height for this endpoint, remembered in this browser.' },
    link: { label: 'link', title: 'Taken from the link you opened.' },
    /* #681 M6 — the customizer's default for this kind of endpoint. Still a
     * default, so it reads "default" like the server's own: it is an
     * assumption about a class of node, not a measured height. */
    theme: { label: 'default', title: 'The default for this kind of endpoint, from this site\u2019s Display settings.' }
  };
  var SOURCE_BADGE = {
    'role-default': 'default',
    'point-default': 'default',
    'operator-node': 'operator',
    'operator-role': 'operator'
  };

  function heightBadge(source, origin) {
    var key = null;
    if (source === 'request') key = (origin === 'yours' || origin === 'theme') ? origin : 'link';
    else if (Object.prototype.hasOwnProperty.call(SOURCE_BADGE, source)) key = SOURCE_BADGE[source];
    if (!key) return null;
    return { key: key, label: HEIGHT_BADGES[key].label, title: HEIGHT_BADGES[key].title };
  }

  /* copyLink resolves true or false and never throws: a clipboard that
   * refuses, throws or does not exist is a message, not an error. */
  function copyLink(text, copier) {
    return new Promise(function (resolve) {
      if (typeof copier !== 'function') { resolve(false); return; }
      try {
        copier(String(text), function () { resolve(true); }, function () { resolve(false); });
      } catch (_) { resolve(false); }
    });
  }

  var FREQ_SOURCES = {
    request: 'from this link',
    config: 'operator configuration',
    observers: 'most common on this mesh',
    firmware: 'MeshCore firmware default'
  };

  /* create builds the tool. The host owns picking and the map; this owns the
   * panel, the request and the deep link. Nothing runs until open(). */
  function create(opts) {
    opts = opts || {};
    var doc = global.document;
    var mount = opts.mount || doc.body;
    var resolveNode = opts.resolveNode || function () { return null; };
    var fetchJSON = opts.fetchJSON || function (url) {
      return global.fetch(url).then(function (r) {
        if (!r.ok) return r.json().catch(function () { return {}; }).then(function (b) {
          throw new Error(b && b.error ? b.error : 'Request failed (' + r.status + ')');
        });
        return r.json();
      });
    };
    var syncHash = opts.syncHash !== false;

    var state = { a: null, b: null, model: themeDefaults().model, ha: null, hb: null, k: null, freq: null };
    var open = false, seq = 0, debounceTimer = null, panel = null, nodes = { a: null, b: null };
    // Where each height in `state` came from on this side of the wire:
    // 'yours' (typed, or remembered in localStorage), 'link', 'theme' (the
    // customizer's default for this kind of endpoint), or null.
    var origin = { a: null, b: null };
    /* The customizer's defaults applied to this measurement. Deliberately
     * NOT in `state`: a theme default is nobody's decision, so it must not
     * travel in a shared link, and after a reload the badge has to read
     * "default" again rather than "link" (#681 M6). */
    var themeH = { a: null, b: null };
    // True once the viewer, or a link, has stated the model outright — a
    // later customizer change then leaves it alone.
    var modelPinned = false;
    var ui = {};

    /* effectiveState is what we ask the server for: `state` with the
     * customizer's defaults filled into the heights it left empty. */
    function effectiveState() {
      return {
        a: state.a, b: state.b, model: state.model,
        ha: state.ha !== null ? state.ha : themeH.a,
        hb: state.hb !== null ? state.hb : themeH.b,
        k: state.k, freq: state.freq
      };
    }

    /* adoptThemeDefaults applies a customizer height to an endpoint the
     * server answered out of its own built-in default, and reports whether
     * anything moved. An operator value, a remembered height and a link are
     * all more specific and are left alone; a customizer value equal to what
     * the server already returned is not worth a second request. Called once
     * per measurement, so it cannot loop. */
    function adoptThemeDefaults(data) {
      var defs = themeDefaults();
      var changed = false;
      ['a', 'b'].forEach(function (side) {
        var end = (data && data[side]) || null;
        if (!end || themeH[side] !== null) return;
        if ((side === 'a' ? state.ha : state.hb) !== null) return;
        if (!PROMOTABLE_SOURCES[end.antennaSource]) return;
        var h = themeHeight(end, defs);
        if (h === null || h === clampHeight(end.antennaM)) return;
        themeH[side] = h;
        origin[side] = 'theme';
        changed = true;
      });
      return changed;
    }

    /* The customizer can move these defaults while the panel is open. Drop
     * what was adopted from the old values and measure again. */
    function onDefaultsChanged() {
      var defs = themeDefaults();
      if (!modelPinned && state.model !== defs.model) {
        state.model = defs.model;
        syncModelButtons();
      }
      ['a', 'b'].forEach(function (side) {
        if (themeH[side] === null) return;
        themeH[side] = null;
        if (origin[side] === 'theme') origin[side] = null;
      });
      if (open && state.a && state.b) request();
    }

    function buildPanel() {
      panel = el('section', 'tp-panel');
      panel.setAttribute('role', 'region');
      panel.setAttribute('aria-label', 'Path profile');
      panel.setAttribute('data-testid', 'tp-panel');

      var head = el('header', 'tp-head');
      head.appendChild(el('h3', 'tp-title', 'Path profile'));
      ui.chip = el('span', 'tp-chip');
      ui.chip.setAttribute('data-testid', 'tp-verdict');
      head.appendChild(ui.chip);
      /* Plan §3.2: the verdict must read "assumed heights" until the viewer
       * has confirmed both ends. A 10 m error in an assumed antenna height
       * swamps the 0.05 m DHM accuracy and can flip the verdict outright,
       * so the caveat sits next to the verdict rather than only beside the
       * height inputs. It clears itself: as soon as a height is typed the
       * request carries it and the server answers assumed:false. */
      ui.assumed = el('span', 'tp-chip tp-chip-assumed', ASSUMED_LABEL);
      ui.assumed.setAttribute('data-testid', 'tp-assumed-chip');
      ui.assumed.title = 'At least one antenna height is a default, not a measured value. Set both heights to confirm them.';
      ui.assumed.hidden = true;
      head.appendChild(ui.assumed);
      ui.copy = el('button', 'tp-close tp-copy');
      ui.copy.type = 'button';
      ui.copy.disabled = true;
      ui.copy.title = 'Copy a link to this profile';
      ui.copy.setAttribute('aria-label', 'Copy link to this profile');
      ui.copy.setAttribute('data-testid', 'tp-copy-link');
      // Static markup, no interpolation: the Phosphor sprite icon.
      ui.copy.innerHTML = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-link"/></svg>';
      ui.copy.addEventListener('click', onCopy);
      head.appendChild(ui.copy);
      var close = el('button', 'tp-close');
      close.type = 'button';
      close.setAttribute('aria-label', 'Close path profile');
      close.setAttribute('data-testid', 'tp-close');
      // Static markup, no interpolation: the Phosphor sprite, like every
      // other close affordance on the Map page (#1648 M6).
      close.innerHTML = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-x"/></svg>';
      close.addEventListener('click', function () { api.close(); });
      head.appendChild(close);
      panel.appendChild(head);
      ui.copyStatus = el('p', 'tp-copy-status');
      ui.copyStatus.setAttribute('role', 'status');
      ui.copyStatus.setAttribute('data-testid', 'tp-copy-status');
      panel.appendChild(ui.copyStatus);

      var body = el('div', 'tp-body');
      body.setAttribute('data-testid', 'tp-body');
      ui.chart = el('div', 'tp-chart');
      ui.chart.setAttribute('data-testid', 'tp-chart');
      body.appendChild(ui.chart);

      // The shared credit text, with its licence marker as a link to the
      // licence itself: the CC BY 4.0 obligation, spelled out once.
      ui.credit = el('p', 'tp-credit-line');
      ui.credit.appendChild(el('span', null, ATTRIBUTION.slice(0, ATTRIBUTION.length - LICENCE_MARK.length)));
      var licence = el('a', 'tp-credit-link', LICENCE_MARK);
      licence.setAttribute('href', ATTRIBUTION_URL);
      licence.setAttribute('target', '_blank');
      licence.setAttribute('rel', 'noopener noreferrer license');
      licence.setAttribute('data-testid', 'tp-credit-link');
      ui.credit.appendChild(licence);
      body.appendChild(ui.credit);

      ui.detail = el('p', 'tp-detail');
      ui.detail.setAttribute('data-testid', 'tp-detail');
      body.appendChild(ui.detail);

      var controls = el('div', 'tp-controls');
      ui.endA = endpointControl('a', 'A');
      ui.endB = endpointControl('b', 'B');
      controls.appendChild(ui.endA.wrap);
      controls.appendChild(ui.endB.wrap);

      var modelWrap = el('div', 'tp-field');
      modelWrap.appendChild(el('span', 'tp-field-label', 'Model'));
      var group = el('div', 'tp-seg');
      group.setAttribute('role', 'group');
      group.setAttribute('aria-label', 'Elevation model');
      ui.modelBtns = {};
      [['both', 'Terrain + surface'], ['dtm', 'Terrain only']].forEach(function (m) {
        var b = el('button', 'tp-seg-btn', m[1]);
        b.type = 'button';
        b.setAttribute('data-model', m[0]);
        b.setAttribute('data-testid', 'tp-model-' + m[0]);
        b.addEventListener('click', function () {
          if (state.model === m[0]) return;
          state.model = m[0];
          modelPinned = true;
          syncModelButtons();
          request();
        });
        ui.modelBtns[m[0]] = b;
        group.appendChild(b);
      });
      modelWrap.appendChild(group);
      controls.appendChild(modelWrap);

      ui.meta = el('p', 'tp-meta');
      ui.meta.setAttribute('data-testid', 'tp-meta');
      controls.appendChild(ui.meta);
      body.appendChild(controls);

      panel.appendChild(body);
      syncModelButtons();
      return panel;
    }

    function endpointControl(side, label) {
      var wrap = el('div', 'tp-field tp-end');
      wrap.setAttribute('data-testid', 'tp-end-' + side);
      var head = el('div', 'tp-end-head');
      head.appendChild(el('span', 'tp-end-tag', label));
      var name = el('span', 'tp-end-name', 'not picked');
      name.setAttribute('data-testid', 'tp-name-' + side);
      head.appendChild(name);
      wrap.appendChild(head);

      var id = 'tpHeight' + label;
      var lab = el('label', 'tp-field-label', 'Antenna height (m)');
      lab.setAttribute('for', id);
      wrap.appendChild(lab);

      var stepper = el('div', 'tp-stepper');
      var minus = el('button', 'tp-step', '−');
      minus.type = 'button';
      minus.setAttribute('aria-label', 'Lower antenna height ' + label);
      minus.setAttribute('data-testid', 'tp-h-' + side + '-minus');
      var input = el('input', 'tp-height');
      input.type = 'number';
      input.id = id;
      input.min = '0';
      input.max = String(MAX_HEIGHT_M);
      input.step = '1';
      input.inputMode = 'numeric';
      input.setAttribute('data-testid', 'tp-h-' + side);
      var plus = el('button', 'tp-step', '+');
      plus.type = 'button';
      plus.setAttribute('aria-label', 'Raise antenna height ' + label);
      plus.setAttribute('data-testid', 'tp-h-' + side + '-plus');
      stepper.appendChild(minus);
      stepper.appendChild(input);
      stepper.appendChild(plus);
      wrap.appendChild(stepper);

      var srcRow = el('div', 'tp-src-row');
      var badge = el('span', 'tp-hbadge');
      badge.setAttribute('data-testid', 'tp-hsrc-' + side);
      badge.hidden = true;
      srcRow.appendChild(badge);
      var source = el('span', 'tp-assumed');
      source.setAttribute('data-testid', 'tp-assumed-' + side);
      srcRow.appendChild(source);
      wrap.appendChild(srcRow);

      function commit(v) {
        var h = clampHeight(v);
        if (h === null) return;
        input.value = String(h);
        state[side === 'a' ? 'ha' : 'hb'] = h;
        origin[side] = 'yours';
        themeH[side] = null;
        rememberHeight(state[side], h);
        scheduleRequest();
      }
      minus.addEventListener('click', function () { commit((clampHeight(input.value) || 0) - 1); });
      plus.addEventListener('click', function () { commit((clampHeight(input.value) || 0) + 1); });
      input.addEventListener('input', function () { commit(input.value); });
      input.addEventListener('change', function () { commit(input.value); });
      return { wrap: wrap, name: name, input: input, source: source, badge: badge };
    }

    function syncModelButtons() {
      if (!ui.modelBtns) return;
      Object.keys(ui.modelBtns).forEach(function (m) {
        var b = ui.modelBtns[m];
        var on = state.model === m;
        b.classList.toggle('active', on);
        b.setAttribute('aria-pressed', on ? 'true' : 'false');
      });
    }

    function setChip(chip) {
      if (!ui.chip) return;
      ui.chip.className = 'tp-chip tp-chip-' + chip.tone;
      ui.chip.textContent = chip.label;
      ui.chip.setAttribute('data-state', chip.state);
      ui.chip.title = chip.detail;
    }

    function writeHash() {
      if (!syncHash) return;
      try {
        var hash = (global.location && global.location.hash) || '';
        var qi = hash.indexOf('?');
        var base = qi >= 0 ? hash.slice(0, qi) : hash;
        var params = new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '');
        ['profile', 'model', 'ha', 'hb', 'k', 'freq'].forEach(function (key) { params.delete(key); });
        if (open && state.a && state.b) {
          var ser = serializeState(state, themeDefaults());
          Object.keys(ser).forEach(function (key) { params.set(key, ser[key]); });
        }
        var qs = params.toString();
        var next = global.location.pathname + global.location.search + (qs ? (base || '#') + '?' + qs : base);
        global.history.replaceState(global.history.state, '', next);
      } catch (_) { /* a hash we cannot write is not worth breaking the tool for */ }
    }

    function onCopy() {
      var url = shareURL(state, global.location, themeDefaults());
      if (!url) return;
      copyLink(url, opts.copyText || global.copyToClipboard).then(function (ok) {
        if (!ui.copyStatus) return;
        ui.copyStatus.textContent = ok ? 'Link copied' : 'Could not copy the link. It is in the address bar.';
      });
    }

    function scheduleRequest() {
      if (debounceTimer) global.clearTimeout(debounceTimer);
      debounceTimer = global.setTimeout(function () { debounceTimer = null; request(); }, HEIGHT_DEBOUNCE_MS);
      writeHash();
    }

    function renderNames() {
      if (!panel) return;
      [['a', ui.endA], ['b', ui.endB]].forEach(function (pair) {
        var side = pair[0], c = pair[1];
        var ref = state[side];
        nodes[side] = (ref && ref.kind === 'node') ? resolveNode(ref.pubkey) : null;
        c.name.textContent = endpointLabel(ref, nodes[side] && nodes[side].name);
        c.input.disabled = !ref;
      });
      ui.copy.disabled = !(state.a && state.b);
      ui.copyStatus.textContent = '';
    }

    function setStatus(text, cls) {
      if (!ui.chart) return;
      ui.chart.textContent = '';
      var p = el('p', 'tp-status ' + (cls || ''), text);
      p.setAttribute('data-testid', 'tp-status');
      ui.chart.appendChild(p);
    }

    function render(data) {
      var chip = verdictChip(data);
      setChip(chip);
      var names = { nameA: nodes.a && nodes.a.name, nameB: nodes.b && nodes.b.name };
      var drawn = !!(data.samples && data.samples.length);
      /* The credit is a licence obligation, so it is one HTML line under
       * the chart rather than text inside the SVG: the chart box is
       * stretched to the panel width and in-SVG text came out ~3 px tall
       * on a 360 px phone, which is not an attribution anybody can read. */
      ui.credit.hidden = false;
      // Either end on a default height caveats the whole verdict — a
      // customizer default included (#681 M6).
      ui.assumed.hidden = !(assumedHeight(data.a, origin.a) || assumedHeight(data.b, origin.b));
      if (!drawn) {
        setStatus(chip.detail, 'tp-empty');
      } else {
        // Sink: one SVG string from buildChart, where every node-controlled
        // name went through esc() at interpolation time.
        ui.chart.innerHTML = buildChart(data, names);
      }
      ui.detail.textContent = chip.detail;
      var phys = data.physics || {};
      var parts = [];
      if (isFinite(Number(phys.freqMHz))) {
        parts.push('Frequency ' + round(Number(phys.freqMHz), 3) + ' MHz (' + (FREQ_SOURCES[phys.freqSource] || 'default') + ')');
      }
      if (isFinite(Number(phys.k))) parts.push('k = ' + round(Number(phys.k), 2));
      if (isFinite(Number(phys.f1MaxM))) parts.push('widest Fresnel radius ' + round(Number(phys.f1MaxM), 1) + ' m');
      ui.meta.textContent = parts.join(' · ');
      [['a', ui.endA], ['b', ui.endB]].forEach(function (pair) {
        var side = pair[0], c = pair[1];
        var end = data[side] || {};
        var h = clampHeight(end.antennaM);
        // Shown, not adopted: copying the server's default into `state`
        // would send it back as a "request" height on the next measure and
        // quietly turn an assumed height into a confirmed one.
        if (h !== null && doc.activeElement !== c.input) c.input.value = String(h);
        var badge = heightBadge(end.antennaSource, origin[side]);
        c.badge.hidden = !badge;
        c.badge.textContent = badge ? badge.label : '';
        c.badge.title = badge ? badge.title : '';
        c.badge.className = 'tp-hbadge' + (badge ? ' tp-hbadge-' + badge.key : '');
        c.badge.setAttribute('data-source', badge ? badge.key : '');
        var src = sourceLabel(end.antennaSource, origin[side]);
        var guess = assumedHeight(end, origin[side]);
        c.source.textContent = guess ? ('Assumed — ' + src) : src;
        c.source.classList.toggle('is-assumed', guess);
      });
    }

    /* request issues exactly one call per profile. A newer call wins: an
     * older response is dropped rather than racing into the panel. */
    function request(adopted) {
      if (!open || !state.a || !state.b) return;
      if (debounceTimer) { global.clearTimeout(debounceTimer); debounceTimer = null; }
      renderNames();
      writeHash();
      var mine = ++seq;
      setStatus('Measuring the terrain along the path…', 'tp-loading');
      setChip({ state: 'pending', tone: 'unknown', label: '…', detail: 'Waiting for the terrain profile.' });
      if (ui.assumed) ui.assumed.hidden = true;
      return fetchJSON(requestURL(effectiveState())).then(function (data) {
        if (mine !== seq || !open) return;
        /* The server answers which height it used and where it came from,
         * so a customizer default can be applied without ever overruling an
         * operator's own value. The repeat carries the heights and lands on
         * the elevation cache (heights are not part of its key), so it costs
         * no upstream request; the first answer is not drawn, so the viewer
         * sees one chart rather than two. */
        if (!adopted && adoptThemeDefaults(data)) { request(true); return; }
        render(data);
      }).catch(function (err) {
        if (mine !== seq || !open) return;
        setChip({ state: 'error', tone: 'unknown', label: 'No profile', detail: String(err && err.message || err) });
        if (ui.assumed) ui.assumed.hidden = true;
        setStatus(String((err && err.message) || 'Could not load the terrain profile.'), 'tp-error');
        ui.detail.textContent = 'Could not load the terrain profile.';
      });
    }

    var api = {
      el: function () { return panel; },
      isOpen: function () { return open; },
      getState: function () {
        return { a: state.a, b: state.b, model: state.model, ha: state.ha, hb: state.hb, k: state.k, freq: state.freq };
      },
      /* setEndpoint fills one end and, when both are set, measures. A node
       * or point with a remembered height adopts it immediately. */
      setEndpoint: function (side, ref) {
        if (side !== 'a' && side !== 'b') return;
        state[side] = ref || null;
        var h = recallHeight(ref);
        state[side === 'a' ? 'ha' : 'hb'] = h;
        origin[side] = h === null ? null : 'yours';
        // A new endpoint invalidates whatever default the old one adopted.
        themeH[side] = null;
        if (panel) {
          var c = side === 'a' ? ui.endA : ui.endB;
          c.input.value = h === null ? '' : String(h);
        }
        api.open();
        renderNames();
        if (state.a && state.b) request(); else { writeHash(); setStatus('Pick the second point on the map.', 'tp-empty'); }
      },
      setState: function (next) {
        if (!next) return;
        state.a = next.a || null;
        state.b = next.b || null;
        state.model = validModel(next.model) ? next.model : themeDefaults().model;
        /* A link that spells the model out is a decision, and a later
         * customizer change must not quietly override it. parseState has
         * already resolved an absent one to the default, so the difference
         * from the default is what the link actually said. */
        modelPinned = state.model !== themeDefaults().model;
        state.ha = next.ha === undefined ? null : next.ha;
        state.hb = next.hb === undefined ? null : next.hb;
        state.k = next.k === undefined ? null : next.k;
        state.freq = next.freq === undefined ? null : next.freq;
        origin.a = state.ha === null ? null : 'link';
        origin.b = state.hb === null ? null : 'link';
        themeH.a = null;
        themeH.b = null;
        if (state.ha === null) { state.ha = recallHeight(state.a); if (state.ha !== null) origin.a = 'yours'; }
        if (state.hb === null) { state.hb = recallHeight(state.b); if (state.hb !== null) origin.b = 'yours'; }
        api.open();
        syncModelButtons();
        if (panel) {
          ui.endA.input.value = state.ha === null ? '' : String(state.ha);
          ui.endB.input.value = state.hb === null ? '' : String(state.hb);
        }
        renderNames();
        if (state.a && state.b) request();
      },
      open: function () {
        if (open) return;
        open = true;
        if (!panel) mount.appendChild(buildPanel());
        panel.classList.add('is-open');
        renderNames();
        writeHash();
      },
      close: function () {
        if (!open) return;
        open = false;
        seq++;
        if (debounceTimer) { global.clearTimeout(debounceTimer); debounceTimer = null; }
        state.a = null;
        state.b = null;
        themeH.a = null;
        themeH.b = null;
        if (panel) panel.classList.remove('is-open');
        writeHash();
        if (opts.onClose) opts.onClose();
      },
      destroy: function () {
        api.close();
        if (global.removeEventListener) {
          global.removeEventListener('terrain-defaults-changed', onDefaultsChanged);
        }
        if (panel && panel.parentNode) panel.parentNode.removeChild(panel);
        panel = null;
        ui = {};
      },
      // Test seam only: the customizer's event, delivered directly.
      defaultsChangedForTests: onDefaultsChanged
    };
    /* The customizer fires this when an antenna default or the model moves,
     * and only then — a colour pick is pure CSS and needs no re-measure. */
    if (global.addEventListener) {
      global.addEventListener('terrain-defaults-changed', onDefaultsChanged);
    }
    return api;
  }

  global.TerrainProfile = {
    ATTRIBUTION: ATTRIBUTION,
    ATTRIBUTION_URL: ATTRIBUTION_URL,
    ASSUMED_LABEL: ASSUMED_LABEL,
    ENDPOINT: ENDPOINT,
    HASH_PARAM: HASH_PARAM,
    MAX_HEIGHT_M: MAX_HEIGHT_M,
    parseState: parseState,
    parseStart: parseStart,
    shareURL: shareURL,
    offered: offered,
    nodeEntryHash: nodeEntryHash,
    nodeEntryAction: nodeEntryAction,
    heightBadge: heightBadge,
    copyLink: copyLink,
    serializeState: serializeState,
    requestURL: requestURL,
    clampHeight: clampHeight,
    themeDefaults: themeDefaults,
    themeHeight: themeHeight,
    sourceLabel: sourceLabel,
    assumedHeight: assumedHeight,
    recallHeight: recallHeight,
    rememberHeight: rememberHeight,
    verdictChip: verdictChip,
    chartGeometry: chartGeometry,
    buildChart: buildChart,
    create: create,
    // Test seam only: lets the unit tests inspect the very storage the
    // module uses, instead of reaching for a global that may not exist.
    storageForTests: storage
  };
})(typeof window !== 'undefined' ? window : this);
