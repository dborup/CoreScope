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
 * profile, with the height inputs debounced. With the server's heritage
 * block, a resolved profile then looks up each end once in the protected
 * monuments register (#723 M3), remembering the answers.
 */
(function (global) {
  'use strict';

  /* Local escaper for the chart's SVG string sinks. One definition only —
   * test-xss-escape-sinks.js lifts it and would refuse a second. */
  function esc(s) { return s == null ? '' : String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }

  /* #765 — the chart footer's CC BY 4.0 credit comes from the server's dhm
   * block (cmd/server/dhm_proxy.go, from internal/dhm): no credit string is
   * kept here. footerCredit below builds it. */
  // The footer's own test ids for the two links creditLinks names, and
  // for the footprints' two in a classified profile's credit (#730 M3).
  var CREDIT_TESTIDS = { dataset: 'tp-credit-dataset', license: 'tp-credit-link',
    buildingsDataset: 'tp-credit-buildings-dataset', buildingsSource: 'tp-credit-buildings-source' };
  // #738 M2: the same two links in the Copernicus credit of a mixed profile.
  var FILL_CREDIT_TESTIDS = { dataset: 'tp-credit-fill-dataset', license: 'tp-credit-fill-license' };
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
  /* #789 M1: the server floors its surface-model estimate at the role
   * height that would otherwise apply, max(role, surface + 2). The
   * customizer role height is this viewer's role height, so it gets the
   * same floor: it replaces an estimate only when it is higher. */
  var FLOORED_SOURCES = { 'dsm-estimate': true };

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

  /* themeHeightOf reads one customizer height by the customizer's own rule
   * (public/antenna-height.js), so the profile uses exactly the value the
   * customizer shows. Unlike clampHeight it refuses rather than clamps, and
   * it refuses hex, exponents and padding: "0x1F4" is no default (review
   * PR#714). Without the rule on the page there are no customizer heights. */
  function themeHeightOf(v) {
    return global.MCAntennaHeight ? global.MCAntennaHeight.parse(v) : null;
  }

  /* themeDefaults resolves the customizer's terrain section. Pass the raw
   * section to resolve a specific one; omit it to read the live config.
   * Junk is not a default: an unusable value falls through to the server.
   * shareLinks is the viewer's preview-link switch (#727 M4): only an
   * explicit false turns it off. */
  function themeDefaults(section) {
    var src = (section === undefined)
      ? (global.SITE_CONFIG && global.SITE_CONFIG.terrain)
      : section;
    var out = { model: DEFAULT_MODEL, heights: {}, shareLinks: true };
    if (!src || typeof src !== 'object') return out;
    if (src.shareLinks === false) out.shareLinks = false;
    if (validModel(src.model)) out.model = src.model;
    Object.keys(THEME_HEIGHT_KEYS).forEach(function (role) {
      var h = themeHeightOf(src[THEME_HEIGHT_KEYS[role]]);
      if (h !== null) out.heights[role] = h;
    });
    var pt = themeHeightOf(src[THEME_POINT_KEY]);
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
   * the default instead, so one bad control never costs the whole link.
   * modelExplicit records whether the link stated the model at all: a
   * stated model is a decision that a copied link and a later customizer
   * change both keep, whatever the default is (#746). */
  function parseState(input, defaults) {
    var p = toParams(input);
    if (!p) return null;
    var refs = parseProfilePair(p.get(HASH_PARAM));
    if (!refs) return null;
    return {
      a: refs[0],
      b: refs[1],
      model: modelOf(p.get('model'), defaults),
      modelExplicit: validModel(p.get('model')),
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

  /* invalidLink is true when a link names a profile that neither reader
   * accepts (#839 §7): /p?profile=garbage hands off to #/map?profile=garbage,
   * and that must say so rather than open a plain map. */
  function invalidLink(input) {
    var p = toParams(input);
    if (!p || p.get(HASH_PARAM) === null) return false;
    return !parseState(p) && !parseStart(p);
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

  // ── Endpoints from the map (#721) ────────────────────────────────────────

  /* pointRef is a free map point as an endpoint: five decimals (~1 m), the
   * precision the link carries, or null for anything that is not a real
   * coordinate. */
  function pointRef(lat, lon) {
    var la = Number(lat), lo = Number(lon);
    if (lat === null || lon === null || !isFinite(la) || !isFinite(lo)
      || la < -90 || la > 90 || lo < -180 || lo > 180) return null;
    return { kind: 'point', lat: round(la, 5), lon: round(lo, 5) };
  }

  /* markerRef is the one rule for "which endpoint is this marker": a node
   * key the node store knows becomes that node, anything else (an
   * observer-only marker, a marker without a key) the point it stands on.
   * The server resolves a key against the node store only, so an observer
   * id sent as a node would come back 404. */
  function markerRef(key, lat, lon, isNode) {
    var k = key ? String(key).toLowerCase() : '';
    if (PUBKEY_RE.test(k) && typeof isNode === 'function' && isNode(k)) return { kind: 'node', pubkey: k };
    return pointRef(lat, lon);
  }

  /* snapEndpoint resolves a drop: onto the nearest marker within radiusPx
   * (by markerRef), else the free point under the drop. drop and every
   * candidate carry container pixels {x, y} and their {lat, lon}. One pass,
   * once per drop — never per pointer move. */
  function snapEndpoint(drop, candidates, radiusPx, isNode) {
    if (!drop) return null;
    var best = null, bestD = radiusPx * radiusPx;
    for (var i = 0; candidates && i < candidates.length; i++) {
      var c = candidates[i];
      var dx = c.x - drop.x, dy = c.y - drop.y, d = dx * dx + dy * dy;
      if (d <= bestD) { best = c; bestD = d; }
    }
    return best ? markerRef(best.key, best.lat, best.lon, isNode) : pointRef(drop.lat, drop.lon);
  }

  /* serializeState writes only what was decided, so a plain two-node link
   * stays short and the round trip is exact. */
  function serializeState(state) {
    var out = {};
    if (!state || !state.a || !state.b) return out;
    out[HASH_PARAM] = refToParam(state.a) + ',' + refToParam(state.b);
    /* The model rides the link whenever it is not the standard "both",
     * whatever this viewer's own default is: the recipient may not share it,
     * so a terrain-only link must say so (review PR#714). It also rides
     * whenever it was stated — by the link this state came from or by the
     * viewer's own pick — so a recipient on another default sees the same
     * profile (#746). Presence, not a comparison with this viewer's
     * default, which the recipient does not share either. */
    if (validModel(state.model) && (state.model !== DEFAULT_MODEL || state.modelExplicit === true)) {
      out.model = state.model;
    }
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
    var ser = serializeState(state);
    ['ha', 'hb', 'k', 'freq'].forEach(function (key) { if (ser[key] !== undefined) p.set(key, ser[key]); });
    return ENDPOINT + '?' + p.toString();
  }

  /* hashParams is what the address bar carries for a state: the full pair
   * once both ends are set, and while B is still being picked the half link
   * profile=<A> that parseStart reads back, so a reload keeps A. A point as
   * A has no half link: parseStart only takes a node key. */
  function hashParams(state) {
    if (state && state.a && state.b) return serializeState(state);
    var out = {};
    if (state && state.a && !state.b && state.a.kind === 'node') out[HASH_PARAM] = refToParam(state.a);
    return out;
  }

  var PROFILE_KEYS = [HASH_PARAM, 'model', 'ha', 'hb', 'k', 'freq'];

  /* withProfileParams is the one place a profile link is written into a
   * hash: the tool's own keys are replaced, every other key is kept. */
  function withProfileParams(hash, ser) {
    hash = String(hash || '');
    var qi = hash.indexOf('?');
    var base = qi >= 0 ? hash.slice(0, qi) : hash;
    var params = new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '');
    PROFILE_KEYS.forEach(function (key) { params.delete(key); });
    Object.keys(ser || {}).forEach(function (key) { params.set(key, ser[key]); });
    var qs = params.toString();
    return qs ? (base || '#') + '?' + qs : base;
  }

  /* profileURL is the one URL builder: the address bar (writeHash) and the
   * copy button (shareURL) both keep the page's path and query. */
  function profileURL(loc, hash, ser, absolute) {
    return (absolute ? String(loc.origin || '') : '') + String(loc.pathname || '/') + String(loc.search || '')
      + withProfileParams(hash, ser);
  }

  /* SHARE_PATH is the server's share route (#727, cmd/server/share_profile.go):
   * one path segment next to index.html, so its relative assets resolve. */
  var SHARE_PATH = 'p';

  /* #873 — the page's base map rides on the #/map link: "?base=<id>" when
   * the address bar holds a plain style id, else nothing. */
  function baseQuery(hash) {
    var h = String(hash || ''), qi = h.indexOf('?');
    var v = qi >= 0 ? new URLSearchParams(h.slice(qi + 1)).get('base') : null;
    return v && /^[A-Za-z0-9_-]{1,64}$/.test(v) ? '?base=' + v : '';
  }

  /* shareURL is the link the panel's copy button hands out: built from the
   * state, not read off the address bar, so it is the same whether or not
   * the host lets the panel write the hash. With `route` (the server serves
   * /p, see shareOffered) it is a /p link that previews the profile in a
   * chat; otherwise the plain #/map link, exactly as before #727. Both carry
   * the same serializeState decisions, so a customizer default never rides
   * either one. */
  function shareURL(state, loc, route) {
    if (!state || !state.a || !state.b || !loc) return null;
    var ser = serializeState(state);
    if (route !== true) return profileURL(loc, '#/map' + baseQuery(loc.hash), ser, true);
    var p = new URLSearchParams();
    Object.keys(ser).forEach(function (key) { p.set(key, ser[key]); });
    var path = String(loc.pathname || '/');
    return String(loc.origin || '') + path.slice(0, path.lastIndexOf('/') + 1) + SHARE_PATH + '?' + p.toString();
  }

  /* shareOffered: the server advertises the /p route as dhm.share only when
   * the profile is served and map.dhm.profile.share.enabled is set. */
  function shareOffered(cfg) {
    return offered(cfg) && cfg.share === true;
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
    return withProfileParams('#/map', hashParams({ a: { kind: 'node', pubkey: pk }, b: null }));
  }

  /* nodeEntryAction is the node-detail button, built as DOM so no node
   * field ever passes through an HTML string. */
  function nodeEntryAction(node, cfg) {
    var entryHref = nodeEntryHash(node, cfg);
    if (!entryHref) return null;
    var a = el('a', 'btn-primary node-terrain-action');
    // Not HTML-escaped: an attribute set through the DOM is not HTML,
    // and escaping would turn a second parameter's & into &amp;. It is
    // safe as built: '#/map?profile=' plus a PUBKEY_RE-checked key.
    a.setAttribute('href', entryHref);
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

  // ── Oblique photo points (#737 M2) ───────────────────────────────────────

  /* endPoint is end 'a' or 'b' of a measured profile as the server
   * snapped it, or null. */
  function endPoint(data, side) {
    var e = (side === 'a' || side === 'b') && data ? data[side] : null;
    var r = e ? pointRef(e.lat, e.lon) : null;
    return r ? { lat: r.lat, lon: r.lon } : null;
  }

  /* sightLine is a measured profile's line A→B with the antenna heights
   * it was drawn at, for the oblique photo's line of sight (#737 M3c),
   * or null. */
  function sightLine(data) {
    var a = endPoint(data, 'a'), b = endPoint(data, 'b');
    if (!a || !b) return null;
    return { a: a, b: b, ha: clampHeight(data.a.antennaM), hb: clampHeight(data.b.antennaM) };
  }

  /* dhmAt answers whether DHM itself measured the profile at d metres
   * from A: not in a Copernicus fill range (#738) and not where the
   * nearest sample has no data. The server decides that per sample with
   * its foreign-land polygon (internal/dhm boundary.go), which the
   * Skråfoto block's bounds rectangle cannot: it takes in Skåne and
   * Flensburg (#839). A profile without samples rests on its coverage. */
  function dhmAt(data, d) {
    var cov = (data && data.coverage) || {};
    if (cov.state === 'none' || !isFinite(d)) return false;
    var fill = Array.isArray(cov.fill) ? cov.fill : [];
    for (var i = 0; i < fill.length; i++) {
      var r = fill[i];
      if (r && d >= Number(r.fromM) && d <= Number(r.toM)) return false;
    }
    var s = data.samples;
    if (!Array.isArray(s) || !s.length) return true;
    // The samples run from A to B: the nearest one by bisection.
    var lo = 0, hi = s.length - 1;
    while (hi - lo > 1) {
      var mid = (lo + hi) >> 1;
      if (Number(s[mid].d) <= d) lo = mid; else hi = mid;
    }
    var near = Math.abs(Number(s[hi].d) - d) < Math.abs(d - Number(s[lo].d)) ? s[hi] : s[lo];
    return near.dtm !== null && near.dtm !== undefined;
  }

  /* dhmStretch: some sample of the profile was measured by DHM, so a
   * chart point can have photos. */
  function dhmStretch(data) {
    var s = data && data.samples;
    if (!Array.isArray(s)) return false;
    for (var i = 0; i < s.length; i++) if (dhmAt(data, Number(s[i].d))) return true;
    return false;
  }

  /* photoCovered: the oblique photos have point p, d metres from A along
   * the measured profile data: inside the Skråfoto block's bounds (the
   * #818 node-action test, Skraafoto.covered) and measured by DHM there
   * (dhmAt). Elsewhere the search always answers empty (#839). */
  function photoCovered(data, cfg, p, d) {
    var SF = global.Skraafoto;
    return !!(p && SF && typeof SF.covered === 'function' && SF.covered(cfg, p) && dhmAt(data, d));
  }

  /* pointAt is chart sample i as a map point: the #781 cursor's own
   * mapping (its fraction of the path, interpolated as the map draws the
   * line), so the photo is of the point the cursor showed. */
  function pointAt(data, g, i) {
    var C = global.TerrainProfileCursor;
    var a = endPoint(data, 'a'), b = endPoint(data, 'b');
    if (!C || !g || !g.x || !a || !b || !(i >= 0 && i < g.x.length) || i % 1) return null;
    var ll = C.latLonAt([a.lat, a.lon], [b.lat, b.lon], C.fraction(g, i));
    var r = pointRef(ll[0], ll[1]);
    return r ? { lat: r.lat, lon: r.lon, distanceM: round(Number(g.d[i]), 2) } : null;
  }

  /* chartIndexAt is the sample under a click at clientX on the plot drawn
   * in rect: the #781 nearest-sample search. -1 when there is none. */
  function chartIndexAt(g, rect, clientX) {
    var C = global.TerrainProfileCursor;
    if (!C || !g || !g.x || !rect || !(rect.width > 0)) return -1;
    return C.nearestIndex(g.x, (clientX - rect.left) / rect.width * g.view.w);
  }

  /* fmtDistance is a distance along the path (#839 §8): whole metres under
   * 1 km, km with two decimals to 10 km and one from there — never a
   * decimetre over kilometres. The chart, its title, the hover readout and
   * the share card all use it; fmtMetres is for heights and clearances. */
  function fmtDistance(m) {
    var n = Number(m);
    if (!isFinite(n)) return '—';
    if (Math.round(n) < 1000) return Math.round(n) + ' m';
    var km = n / 1000;
    return km.toFixed(Math.round(km * 100) < 1000 ? 2 : 1) + ' km';
  }

  // ── Fitting a linked path into view (#746) ───────────────────────────────

  var FIT_MARGIN_PX = 32;
  // Below this much map between the panel and the top edge there is no
  // useful strip to fit a path into.
  var FIT_MIN_FREE_PX = 120;

  /* fitPadding is the Leaflet fitBounds padding that puts a path in the
   * part of a mapH-tall map the panel leaves free. The panel is anchored to
   * the bottom at full width, so it costs bottom padding only, and it is
   * reserved at panelMaxH (its CSS cap, --tp-panel-max-h) even while it is
   * shorter: it grows to the cap when the chart arrives. With no useful
   * strip left (a short landscape screen) the panel is not reserved, and
   * the path fits the whole map rather than a sliver. */
  function fitPadding(mapH, panelH, panelMaxH, left, right) {
    var m = FIT_MARGIN_PX;
    var reserve = Math.max(Number(panelH) || 0, isFinite(panelMaxH) ? Number(panelMaxH) : 0);
    var bottom = (reserve > 0 && mapH - reserve - 2 * m >= FIT_MIN_FREE_PX) ? reserve + m : m;
    return { paddingTopLeft: [insetPad(left), m], paddingBottomRight: [insetPad(right), bottom] };
  }

  /* fitPaddingBeside is fitPadding for a panel docked at the right edge,
   * full height (a short landscape screen, #839 §2): it costs right padding
   * only, and is not reserved when the map beside it is too narrow. */
  function fitPaddingBeside(mapW, panelW, left) {
    var m = FIT_MARGIN_PX, l = insetPad(left), w = Number(panelW) || 0;
    var right = (w > 0 && mapW - w - l - m >= FIT_MIN_FREE_PX) ? w + m : m;
    return { paddingTopLeft: [l, m], paddingBottomRight: [right, m] };
  }

  /* endsVisible is true when every point (map container pixels) can be
   * seen (#839 §3): on the map, outside the part `pad` reserves for the
   * panel, and not under an obstacle box such as the zoom buttons. The
   * fit's own margin is not required: an end near an edge is in view. */
  function endsVisible(pts, size, pad, obstacles) {
    var m = FIT_MARGIN_PX;
    var right = size.x - Math.max(0, pad.paddingBottomRight[0] - m);
    var bottom = size.y - Math.max(0, pad.paddingBottomRight[1] - m);
    return pts.every(function (p) {
      if (!(p.x >= 0 && p.y >= 0 && p.x <= right && p.y <= bottom)) return false;
      return !(obstacles || []).some(function (o) {
        return p.x >= o.left && p.x <= o.right && p.y >= o.top && p.y <= o.bottom;
      });
    });
  }

  /* insetPad keeps a fitted end clear of the map's own buttons and panels
   * along an edge (the zoom buttons on the left, the map controls on the
   * right, #839 §2): `inset` is how far they reach in from that edge. */
  function insetPad(inset) {
    var l = Number(inset);
    return isFinite(l) && l > 0 ? Math.max(FIT_MARGIN_PX, Math.round(l) + FIT_MARGIN_PX / 2) : FIT_MARGIN_PX;
  }

  /* panelCap is the height the panel may grow to in a mapH-tall map: its
   * CSS cap, but never so tall that less than `keep` px of map is left
   * above it (#839 §2), and never under `floor`. It is the same rule as
   * the .tp-panel max-height in style.css, which reads the same three
   * variables. */
  function panelCap(mapH, cap, keep, floor) {
    var c = isFinite(cap) ? Number(cap) : Infinity;
    if (!isFinite(keep) || !(mapH > 0)) return isFinite(c) ? c : NaN;
    return Math.min(c, Math.max(isFinite(floor) ? Number(floor) : 0, mapH - Number(keep)));
  }

  // ── Refusals the viewer can act on (#839 §6) ─────────────────────────────

  /* refusal turns a server refusal the viewer can fix into words, else
   * null. "path too long" is /api/terrain/profile's fixed message for a
   * path over map.dhm.profile.maxPathKm, and the refusal carries that limit
   * (err.maxPathKm), so the panel can state it. */
  function refusal(err) {
    if (!err || err.message !== 'path too long') return null;
    var km = Number(err.maxPathKm);
    var limit = isFinite(km) && km > 0 ? ' The terrain profile covers paths up to ' + round(km, 1) + ' km.' : '';
    return { label: 'Too long', text: 'This path is too long to measure.' + limit + ' Pick two ends closer together.' };
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

  /* `detail` speaks for the deciding terrain model (DTM); `surface` is the
   * same state read off the surface model (DSM), which carries buildings
   * and trees. One table, so the two chips can never drift apart. */
  var VERDICTS = {
    clear: { label: 'Clear', tone: 'ok', detail: 'The whole first Fresnel zone is clear of terrain.',
      surface: 'Buildings and trees stay clear of the whole first Fresnel zone.' },
    fresnel100: { label: 'Usable', tone: 'ok', detail: 'The first Fresnel zone is partly obstructed, but at least 60 % of it stays clear.',
      surface: 'Buildings or trees reach into the first Fresnel zone, but at least 60 % of it stays clear.' },
    fresnel60: { label: 'Marginal', tone: 'warn', detail: 'Line of sight is clear, but less than 60 % of the first Fresnel zone is.',
      surface: 'Line of sight clears the buildings and trees, but less than 60 % of the first Fresnel zone does.' },
    blocked: { label: 'Blocked', tone: 'bad', detail: 'Terrain rises through the line of sight.',
      surface: 'Buildings or trees rise through the line of sight.' }
  };

  /* #729. The server's verdict is the terrain one and only the terrain one:
   * the DTM decides and the DSM informs (#681 owner decision 6), because a
   * forest canopy in the DSM would mark most rural links blocked. That is
   * the right call for the verdict and the wrong one for the words, so a
   * link straight through a 28 m building used to read "Clear" with the
   * building visible only as a hatched band in the chart.
   *
   * So: the main chip says which model it speaks for, and the surface gets
   * a chip of its own whenever it is WORSE than the terrain — never a
   * second verdict (it keeps the warn tone whatever it says, and it can
   * only ever add a caveat, never soften a bad terrain verdict). */
  var TERRAIN_PREFIX = 'Terrain: ';
  var SURFACE_PREFIX = 'Buildings/trees: ';
  var SURFACE_NOTE = 'Buildings block the signal. Trees mostly attenuate it, so a link through forest can still work.';
  var SURFACE_UNKNOWN_DETAIL = 'No surface verdict: the path was not measured end to end.';
  // Worst first. Only states on this ladder can be compared; "unknown" is
  // deliberately absent, so an unjudged surface is never ranked at all.
  var SURFACE_RANK = { blocked: 0, fresnel60: 1, fresnel100: 2, clear: 3 };

  /* #745 (owner decision on #750): a surface above the antenna at an
   * endpoint is rarely a building in the link. Far more often the node is
   * on a roof and its antenna height, measured from the ground, is a role
   * default or too low. The server names the case (verdict.surface.reason
   * and .endpoint), so the chip says what to check instead of "Blocked". */
  var REASON_ANTENNA_BELOW = 'antennaBelowSurface';
  var BELOW_AT = { A: 'A', B: 'B', both: 'A and B' };

  /* #730 M3 — buildings versus trees. With map.dhm.profile.buildings on,
   * the server tells the elevated surface samples of a model=both profile
   * DHM covers end to end apart (cmd/server/terrain_buildings.go):
   * classification.state (full, partial or unavailable), samples[].cls
   * ("b": inside a building footprint, "v": trees or other) and
   * verdict.surface.cause, read at the samples that decide the surface
   * verdict. The band is coloured by class wherever a sample has one, and
   * the chip names the cause on a full or a partial classification: the
   * server says "building" only where a classified building obstructs,
   * and "vegetation" only when every obstructing raised sample is
   * classified trees/other, so its cause is exact on a partial one too
   * (#797 review finding 4); anything else keeps the generic chip. A
   * building caveats in the warn tone, trees in a milder one: they mostly
   * attenuate. A partial or unavailable classification says so in a
   * muted note. The verdicts never change (#681 owner decision 6). */
  var CLASS_STATES = { full: true, partial: true, unavailable: true };
  var CLASSES = {
    b: { legend: 'Building', readout: 'building', fill: 'var(--terrain-building)' },
    v: { legend: 'Trees/other', readout: 'trees/other', fill: 'var(--terrain-vegetation)' }
  };
  /* The chip by cause (#730 research §6): "Blocked by building" and
   * "Through trees" when the surface blocks, else the surface's own word
   * with what stands there. Never a second verdict: each label names the
   * surface state and no other, and the tone stays a caveat. */
  var CAUSES = {
    building: { blocked: 'Blocked by building', suffix: ' — building', tone: 'warn',
      detail: 'Where it does, a building footprint stands: it is a building.' },
    vegetation: { blocked: 'Through trees', suffix: ' — through trees', tone: 'mild',
      detail: 'Where it does, no building footprint stands: it is trees or other.' }
  };
  var CLASS_NOTES = {
    partial: 'Buildings could not be told apart from trees on part of this path.',
    unavailable: 'Buildings could not be told apart from trees on this path.'
  };
  // The legend's word for the clutter colour on a partial classification.
  var CLASS_NONE_LEGEND = 'Not told apart';
  var CLASS_TITLE = 'In the surface band, buildings are told apart from trees and other objects by building footprints.';

  /* The verdict states that assert terrain inside the criterion zone, and
   * so the only ones whose chart may carry intrusion bands. */
  var INTRUDING_VERDICTS = { fresnel60: true, blocked: true };

  /* #738 M2 — with map.dhm.profile.foreignFill on, the samples DHM does
   * not cover are filled from Copernicus DEM GLO-30 and the response marks
   * the profile mixed (coverage.mixed, coverage.fill[].resolutionM). The
   * verdict then rests on a coarser, surface-influenced model outside
   * Denmark, and there is no surface model there at all, so the panel says
   * both. The fill's nominal resolution is 30 m, or this on the ½ overview
   * (cmd/server fillResolutionM[fillOverview]; #738 decision 7). */
  var FILL_OVERVIEW_M = 60;

  function isMixed(data) { return !!(data && data.coverage && data.coverage.mixed === true); }

  // The coarsest resolution the fill ranges report, or null without a
  // usable one: only a positive finite number is a resolution.
  function fillResolution(cov) {
    var best = null;
    (Array.isArray(cov.fill) ? cov.fill : []).forEach(function (r) {
      var m = r && typeof r.resolutionM === 'number' ? r.resolutionM : NaN;
      if (isFinite(m) && m > 0 && (best === null || m > best)) best = m;
    });
    return best;
  }

  /* mixedNote is the note a mixed profile carries, or null for any other
   * profile — a failed fill included: that one is partial, with no fill
   * fields, and keeps the partial detail of verdictChip. */
  function mixedNote(data) {
    if (!isMixed(data)) return null;
    var res = fillResolution(data.coverage);
    var model = res === null ? 'Copernicus, coarser'
      : 'Copernicus ' + Math.round(res) + ' m' + (res >= FILL_OVERVIEW_M ? ' (½ overview)' : '');
    return 'Mixed: outside Denmark ' + model + ', surface-influenced; surface unknown there.';
  }

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
    /* #729: with the surface model in play the bare word is ambiguous —
     * "Clear" next to a building is what sent us here — so the chip names
     * the model it speaks for. With "Terrain only" there is nothing to
     * disambiguate and the bare label stays. */
    if (known) {
      var prefix = (data && data.model === 'both') ? TERRAIN_PREFIX : '';
      return { state: v.state, label: prefix + known.label, tone: known.tone, detail: known.detail };
    }
    var detail;
    if (cov.state === 'partial') {
      detail = 'No verdict, path leaves Denmark. The profile covers ' + fmtPct(cov.pct) + ' % of the path.';
    } else if (cov.state === 'none') {
      // #839: a cross-border path gets the Copernicus fill (#738); a path
      // with no DHM sample gets no data at all (the card's mainChip).
      detail = 'This path lies entirely outside Denmark, so there is no terrain data for it.';
    } else {
      detail = 'No elevation data for this path, so there is no clearance verdict.';
    }
    return { state: 'unknown', label: 'No verdict', tone: 'unknown', detail: detail };
  }

  /* surfaceState is the DSM state the panel is allowed to speak, which is
   * not always the one the response carries: #689 review N1 forces the
   * surface to "unknown" on anything but full coverage, and this file holds
   * the same line locally (see covered()) so a stale cache or a future
   * server can never paint a confident surface verdict on a path that was
   * never measured end to end. */
  function surfaceState(data) {
    var s = data && data.verdict && data.verdict.surface;
    if (!s) return null;
    if (!covered(data) || !Object.prototype.hasOwnProperty.call(VERDICTS, s.state)) return 'unknown';
    return s.state;
  }

  /* classificationState is the classification the panel may speak of:
   * full, partial or unavailable, or null with none — "Terrain only", a
   * profile with a gap (the server never classifies one, and this file
   * holds the same line, see covered()) or a state it does not know. */
  function classificationState(data) {
    var c = data && data.model === 'both' && covered(data) ? data.classification : null;
    return c && Object.prototype.hasOwnProperty.call(CLASS_STATES, c.state) ? c.state : null;
  }

  // A sample's class, "b" or "v", or null for anything else.
  function sampleClass(s) {
    var c = s && s.cls;
    return c === 'b' || c === 'v' ? c : null;
  }

  // The cause the chip may name: on a full or partial classification.
  function surfaceCause(data) {
    var cs = classificationState(data);
    if (cs !== 'full' && cs !== 'partial') return null;
    var c = data.verdict.surface.cause;
    return c === 'building' || c === 'vegetation' ? c : null;
  }

  /* classNote is the muted note of a partial or unavailable
   * classification, else null. Unavailable means the lookup failed: say
   * that nothing was told apart rather than nothing at all (#797 review
   * finding 1). */
  function classNote(data) {
    var cs = classificationState(data);
    return Object.prototype.hasOwnProperty.call(CLASS_NOTES, cs) ? CLASS_NOTES[cs] : null;
  }

  /* surfaceChip maps verdict.surface to the second chip. It returns null
   * when there is no surface to speak of at all — "Terrain only", or a
   * response without the block — and otherwise a chip whose `show` says
   * whether it adds anything: it is shown only when the surface is strictly
   * worse than the terrain, so two agreeing models stay one chip. An
   * antenna below the surface is the exception (antennaBelowChip). */
  function surfaceChip(data) {
    if (!data || data.model !== 'both') return null;
    var state = surfaceState(data);
    if (state === null) return null;
    if (state === 'blocked') {
      var below = antennaBelowChip(data);
      if (below) return below;
    }
    var known = state === 'unknown' ? null : VERDICTS[state];
    var terrain = verdictChip(data);
    var show = !!known
      && Object.prototype.hasOwnProperty.call(SURFACE_RANK, terrain.state)
      && SURFACE_RANK[state] < SURFACE_RANK[terrain.state];
    // #730 M3: what stands there, when the classification says.
    var cause = known ? surfaceCause(data) : null;
    var c = cause ? CAUSES[cause] : null;
    return {
      state: state,
      label: c ? (state === 'blocked' ? c.blocked : known.label + c.suffix)
        : SURFACE_PREFIX + (known ? known.label : 'No verdict'),
      // Never a second verdict: the surface informs, so it caveats in the
      // warn tone (trees in the milder one) whatever state it is in,
      // rather than going red beside a terrain chip that is green.
      tone: c ? c.tone : 'warn',
      detail: known ? known.surface + (c ? ' ' + c.detail : '') : SURFACE_UNKNOWN_DETAIL,
      note: true,
      show: show
    };
  }

  /* antennaBelowChip is the surface chip for an antenna below the surface
   * at an endpoint, or null when the response names no such endpoint. It
   * is shown whatever the terrain says: it is about the input, not a
   * second verdict. The worst point is inside the path only when buildings
   * or trees also rise through the line of sight (the server keeps the
   * deepest interior obstruction as the worst point), and only then does
   * the detail add that, and the blocking/attenuation line appears. */
  function antennaBelowChip(data) {
    var s = data.verdict.surface;
    if (s.reason !== REASON_ANTENNA_BELOW || !Object.prototype.hasOwnProperty.call(BELOW_AT, s.endpoint)) return null;
    var at = BELOW_AT[s.endpoint];
    var worstAt = Number(s.worstAtM), distance = Number(data.distanceM);
    var inside = s.worstAtM !== null && s.worstAtM !== undefined && isFinite(worstAt) && isFinite(distance)
      && worstAt > 0 && worstAt < distance;
    var detail = 'The buildings or trees at ' + at + ' are higher than the antenna'
      + (s.endpoint === 'both' ? 's there' : ' there')
      + '. If the node is on a roof or a mast, set its antenna height above the ground.';
    if (inside) detail += ' ' + VERDICTS.blocked.surface;
    return {
      state: 'blocked',
      label: SURFACE_PREFIX + 'antenna below the surface at ' + at + ' — check the antenna height',
      tone: 'warn',
      detail: detail,
      note: inside,
      show: true
    };
  }

  // ── Protected ancient monuments (#723 M3) ────────────────────────────────

  /* After a profile resolves, each end is looked up in the protected
   * ancient monuments register through the server
   * (GET /api/terrain/heritage, cmd/server/heritage_query.go). An end
   * inside a protection zone, or within the server's radius of one, gets a
   * warn-tone chip and a note under the detail line. Both are advisory.
   * The verdict is the server's and stays as it is.
   *
   * The wording is neutral on purpose. No public source we could read
   * says, for this exact layer, that it is the statutory 100 m
   * fortidsmindebeskyttelseslinje (naturbeskyttelsesloven § 18), so the
   * note makes no legal claim (owner decision on #723). Silence is never
   * an all-clear: nothing near, a failed lookup and a disabled block all
   * show nothing, and no text says that an end is free of monuments. */
  var HERITAGE_ENDPOINT = '/api/terrain/heritage';
  var HERITAGE_WARNING = 'Protected ancient monument / protection zone nearby — check before building';
  // Answers kept per create(), so a height edit or a model switch re-asks
  // nothing, and moving one end asks only for that end.
  var HERITAGE_KEEP = 8;

  /* heritageOffered: the server sends the heritage block only while
   * map.heritage is enabled, and the same switch serves the endpoint. */
  function heritageOffered(cfg) { return !!(cfg && cfg.enabled === true); }

  /* #723 M4 — this viewer's monument settings from the customizer, through
   * the one rule in public/map-tile-providers.js (MC_HERITAGE_SETTINGS):
   * profileWarning (whether to look up and show the warning at all) and
   * warnDistanceM (list an end outside a zone only up to this distance;
   * null lists everything the server reports). Without the rule on the
   * page: the warning on, unfiltered, as in M3. */
  var HERITAGE_VIEWER_DEFAULTS = { profileWarning: true, warnDistanceM: null };
  function heritageViewerSettings() {
    var R = global.MC_HERITAGE_SETTINGS;
    return R && typeof R.resolve === 'function' ? R.resolve() : HERITAGE_VIEWER_DEFAULTS;
  }

  /* heritageQuery is the lookup's query string for one end of a response
   * (the server's own snapped coordinates), or null without a real one. */
  function heritageQuery(end) {
    var lat = end && end.lat, lon = end && end.lon;
    if (typeof lat !== 'number' || typeof lon !== 'number' || !isFinite(lat) || !isFinite(lon)
      || lat < -90 || lat > 90 || lon < -180 || lon > 180) return null;
    return 'lat=' + fmtCoord(lat) + '&lon=' + fmtCoord(lon);
  }

  /* heritageFinding reads one answer: the zone the end is in or near, or
   * null for no answer, nothing near, or a shape we do not know. Cast at
   * the boundary: only a real boolean is "inside" and only a non-negative
   * number is a distance. */
  function heritageFinding(resp) {
    var n = resp && typeof resp === 'object' ? resp.nearest : null;
    if (!n || typeof n !== 'object') return null;
    var inside = n.insideZone === true;
    var d = !inside && typeof n.distanceM === 'number' && isFinite(n.distanceM) && n.distanceM >= 0 ? Math.round(n.distanceM) : null;
    return {
      type: typeof n.type === 'string' && n.type ? n.type : 'Protected monument',
      inside: inside,
      distanceM: d,
      credit: typeof resp.attribution === 'string' ? resp.attribution : ''
    };
  }

  /* An end outside a zone by less than half a metre (distanceM rounds to
   * 0; the server sends whole metres) is on its edge, not "about 0 m". */
  function heritageLine(side, f) {
    var where = f.inside ? 'inside its protection zone'
      : f.distanceM === 0 ? 'at the edge of its protection zone'
      : f.distanceM !== null ? 'about ' + f.distanceM + ' m from its protection zone' : 'near its protection zone';
    return side + ': ' + f.type + ', ' + where + '.';
  }

  /* heritageNote builds the chip and note for both ends' answers
   * ({ a, b }: a response body, or null for a lookup that failed or was
   * never made), or returns null when there is nothing to show.
   * { label, text, html, lead }: the chip label, the plain text (the chip
   * title), the note markup with every dataset value through esc(), and the
   * end that leads the chip (inside first, then the nearer). */
  function heritageNote(answers, cfg, set) {
    if (!heritageOffered(cfg) || !answers) return null;
    set = set || HERITAGE_VIEWER_DEFAULTS;
    if (set.profileWarning === false) return null;
    var found = { a: heritageFinding(answers.a), b: heritageFinding(answers.b) };
    // The viewer's distance narrows the list. An end inside a zone carries
    // no distance (heritageFinding), so it is always listed, and so is one
    // whose distance is unknown: the server still placed it in its radius.
    var limit = typeof set.warnDistanceM === 'number' ? set.warnDistanceM : null;
    ['a', 'b'].forEach(function (side) {
      var f = found[side];
      if (f && limit !== null && f.distanceM !== null && f.distanceM > limit) found[side] = null;
    });
    if (!found.a && !found.b) return null;
    var lead = 'a';
    if (!found.a) lead = 'b';
    else if (found.b && !found.a.inside && (found.b.inside
      || (found.b.distanceM !== null && (found.a.distanceM === null || found.b.distanceM < found.a.distanceM)))) lead = 'b';
    var f = found[lead];
    var credit = (found.a && found.a.credit) || (found.b && found.b.credit)
      || (typeof cfg.attribution === 'string' ? cfg.attribution : '');
    var lines = [], html = ['<strong class="tp-heritage-warning">' + esc(HERITAGE_WARNING) + '</strong>'];
    [['a', 'A'], ['b', 'B']].forEach(function (s) {
      if (!found[s[0]]) return;
      var line = heritageLine(s[1], found[s[0]]);
      lines.push(line);
      html.push('<span class="tp-heritage-end" data-side="' + s[0] + '">' + esc(line) + '</span>');
    });
    if (credit) html.push('<span class="tp-heritage-credit">' + esc(credit) + '</span>');
    return {
      label: f.inside ? 'In monument zone' : f.distanceM === 0 ? 'At monument zone edge'
        : 'Monument zone' + (f.distanceM !== null ? ' ~' + f.distanceM + ' m' : ''),
      text: [HERITAGE_WARNING + '.'].concat(lines, credit ? [credit] : []).join(' '),
      html: html.join(' '),
      lead: lead
    };
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

  /* runsWhere lists the contiguous runs of indices 0..n-1 for which has(i)
   * holds, as { from, to }, inclusive. */
  function runsWhere(n, has) {
    var out = [], run = null;
    for (var i = 0; i < n; i++) {
      if (!has(i)) { run = null; continue; }
      if (run) run.to = i;
      else { run = { from: i, to: i }; out.push(run); }
    }
    return out;
  }

  /* bandRuns splits each clutter segment (both edges present, two samples
   * or more) into runs of one class, cls[i] or null for none (#730 M3),
   * each a polygon from the surface down to the terrain. Where a class
   * meets a sample without one, the class claims the slope between them:
   * that sample stands below the classification's threshold (or, on a
   * partial classification, was not classified), so the drawn slope is
   * the classified object's edge, not something else. Two classes meet
   * halfway between their samples. Either way the runs tile the band
   * exactly, and a run left with no width is not drawn. Without classes a
   * segment is one run: the very polygon drawn before. x, sY and tY are
   * the rounded plot coordinates. */
  function bandRuns(segments, cls, x, sY, tY) {
    // A boundary is a position along the samples: i (on sample i) or
    // i + 0.5 (halfway to the next).
    function at(a, f) { return f % 1 ? r2((a[f - 0.5] + a[f + 0.5]) / 2) : a[f]; }
    function point(a, f) { return at(x, f) + ',' + at(a, f); }
    var out = [];
    segments.forEach(function (seg) {
      if (seg.to === seg.from) return;
      var runs = [], run = null, i;
      for (i = seg.from; i <= seg.to; i++) {
        var c = cls ? cls[i] : null;
        if (run && run.cls === c) run.to = i;
        else { run = { from: i, to: i, cls: c }; runs.push(run); }
      }
      // edge[k] is where run k ends and run k + 1 begins.
      var edges = [];
      for (var k = 0; k + 1 < runs.length; k++) {
        var l = runs[k], r = runs[k + 1];
        edges.push(l.cls === null ? l.to : r.cls === null ? r.from : l.to + 0.5);
      }
      runs.forEach(function (r, k) {
        var lo = k ? edges[k - 1] : seg.from, hi = k < edges.length ? edges[k] : seg.to;
        if (hi <= lo) return;
        var top = [point(sY, lo)], down = [point(tY, hi)];
        for (var j = Math.floor(lo) + 1; j < hi; j++) top.push(point(sY, j));
        top.push(point(sY, hi));
        for (j = Math.ceil(hi) - 1; j > lo; j--) down.push(point(tY, j));
        down.push(point(tY, lo));
        out.push({ from: r.from, to: r.to, cls: r.cls, points: top.join(' ') + ' ' + down.join(' ') });
      });
    });
    return out;
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
    var segments = runsWhere(n, function (j) { return terrain[j] !== null; });
    var gaps = runsWhere(n, function (j) { return terrain[j] === null; }).map(function (r) {
      return { fromM: d[r.from], toM: d[r.to], x1: r2(xs[r.from]), x2: r2(xs[r.to]) };
    });
    /* The surface has gaps of its own (#746): its line follows the surface
     * samples alone, and the clutter band, which needs both edges, the
     * samples with both. Neither may join two valid points across a
     * missing one. */
    var surfaceSegments = both ? runsWhere(n, function (j) { return surface[j] !== null; }) : [];
    var clutterSegments = both ? runsWhere(n, function (j) { return surface[j] !== null && terrain[j] !== null; }) : [];
    // #730 M3: the classes, when there is a classification with any.
    var cstate = classificationState(data);
    var cls = cstate === 'full' || cstate === 'partial' ? samples.map(sampleClass) : null;
    if (cls && !cls.some(function (c) { return c !== null; })) cls = null;

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

    var xR = xs.map(r2);
    var terrainY = terrain.map(function (v) { return v === null ? null : yOf(v); });
    var surfaceY = surface ? surface.map(function (v) { return v === null ? null : yOf(v); }) : null;
    return {
      view: { w: VIEW.w, h: VIEW.h },
      plot: plot,
      domain: { dMax: D, zMin: zMin, zMax: zMax },
      d: d,
      x: xR,
      terrainZ: terrain.map(function (v) { return v === null ? null : r2(v); }),
      terrainY: terrainY,
      surfaceY: surfaceY,
      // #781: metres for the hover readout's clearance, bulge included
      // like terrainZ.
      surfaceZ: surface ? surface.map(function (v) { return v === null ? null : r2(v); }) : null,
      losZ: los.map(r2),
      losY: los.map(yOf),
      f1: f1.map(r2),
      f1TopY: los.map(function (l, j) { return yOf(l + f1[j]); }),
      f1BotY: los.map(function (l, j) { return yOf(l - f1[j]); }),
      f60TopY: los.map(function (l, j) { return yOf(l + criterion * f1[j]); }),
      f60BotY: los.map(function (l, j) { return yOf(l - criterion * f1[j]); }),
      baseY: r2(plot.y + plot.h),
      segments: segments,
      surfaceSegments: surfaceSegments,
      clutterSegments: clutterSegments,
      // #730 M3: the clutter band in runs of one class, and the hover
      // readout's word for each sample's class (null: none).
      bands: bandRuns(clutterSegments, cls, xR, surfaceY, terrainY),
      surfaceClass: cls ? cls.map(function (c) { return c ? CLASSES[c].readout : null; }) : null,
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

  /* chartTitle is the words a screen reader gets instead of the picture.
   * Both verdicts belong in it (#729): the hatched surface band is the one
   * part of this chart that carries no text at all. */
  function chartTitle(data, chip, names, surf, classified) {
    var a = names.nameA || 'A', b = names.nameB || 'B';
    var txt = 'Terrain profile from ' + a + ' to ' + b + ', ' + fmtDistance(data.distanceM) + '. ' + chip.label + '. ' + chip.detail;
    var note = mixedNote(data);
    if (note) txt += ' ' + note;
    // The verdict's worst point is the smallest Fresnel ratio, not
    // always the least clearance on the path (#839): named for what it
    // is, as the share card's AltText names it.
    var worst = data.verdict && data.verdict.worstClearanceM;
    if (worst !== null && worst !== undefined && isFinite(Number(worst))) {
      txt += ' Tightest point for the Fresnel zone: clearance ' + fmtMetres(worst) + ' at ' + fmtDistance(data.verdict.worstAtM) + ' along the path.';
    }
    if (surf && surf.show) txt += ' ' + surf.label + '. ' + surf.detail + (surf.note ? ' ' + SURFACE_NOTE : '');
    // The note rides with the classified band; an unavailable
    // classification draws none, and its note is the panel's own line.
    var cn = classNote(data);
    if (classified) txt += ' ' + CLASS_TITLE + (cn ? ' ' + cn : '');
    return txt;
  }

  /* chartLabel is the chart's accessible NAME. role="img" + aria-label puts
   * the label ahead of <title>, so a verdict that lives only in the title
   * is never announced; both verdicts go here as well (#729). */
  function chartLabel(chip, surf) {
    return 'Terrain profile chart. ' + chip.label + ((surf && surf.show) ? '. ' + surf.label : '');
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
    var surf = surfaceChip(data);
    if (!g) {
      return '<div class="tp-plot tp-plot-empty">'
        + '<svg viewBox="0 0 ' + VIEW.w + ' ' + VIEW.h + '" class="tp-svg" role="img" aria-label="No terrain profile">'
        + '<title>' + esc(chip.detail) + '</title></svg>'
        + '<p class="tp-plot-msg">' + esc(chip.detail) + '</p></div>';
    }
    var p = g.plot;
    var labels = [];
    var out = ['<div class="tp-plot">'];
    // Sink: a quoted SVG attribute. Both pieces are this file's own fixed
    // strings, but they go through esc() like every other interpolation.
    out.push('<svg viewBox="0 0 ' + VIEW.w + ' ' + VIEW.h + '" class="tp-svg" preserveAspectRatio="none" role="img" aria-label="' + esc(chartLabel(chip, surf)) + '">');
    var classified = g.bands.some(function (b) { return b.cls !== null; });
    out.push('<title>' + esc(chartTitle(data, chip, names, surf, classified)) + '</title>');

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

    // 2. DSM: the clutter band between terrain and surface, plus its line,
    //    each split on its own gaps; the band in runs of one class (#730
    //    M3), a run without one in the clutter colour as before.
    g.bands.forEach(function (b) {
      out.push(b.cls
        ? '<polygon class="tp-band tp-band-' + b.cls + '" points="' + b.points + '" fill="' + CLASSES[b.cls].fill + '" stroke="none"/>'
        : '<polygon points="' + b.points + '" fill="var(--terrain-clutter)" stroke="none"/>');
    });
    g.surfaceSegments.forEach(function (seg) {
      if (seg.to === seg.from) return;
      out.push('<polyline points="' + polyline(g.x, g.surfaceY, seg.from, seg.to) + '" fill="none" stroke="var(--terrain-clutter)" stroke-width="1.2"/>');
    });

    // 3/4. Fresnel bands: 60 % inside the full first zone.
    out.push('<polyline points="' + polyline(g.x, g.f60TopY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-60)" stroke-width="1" stroke-dasharray="4 3"/>');
    out.push('<polyline points="' + polyline(g.x, g.f60BotY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-60)" stroke-width="1" stroke-dasharray="4 3"/>');
    out.push('<polyline points="' + polyline(g.x, g.f1TopY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-100)" stroke-width="1.2"/>');
    out.push('<polyline points="' + polyline(g.x, g.f1BotY, 0, g.x.length - 1) + '" fill="none" stroke="var(--fresnel-100)" stroke-width="1.2"/>');

    // 5. Line of sight. Over a classified band, which is solid, it is
    //    cased in the panel colour: on the building or the trees colour
    //    alone it is under 2:1 (#730 M3).
    var losPts = 'x1="' + g.x[0] + '" y1="' + g.losY[0] + '" x2="' + g.x[g.x.length - 1] + '" y2="' + g.losY[g.losY.length - 1] + '"';
    if (classified) out.push('<line class="tp-los-casing" ' + losPts + ' stroke="var(--card-bg)" stroke-width="4"/>');
    out.push('<line ' + losPts + ' stroke="var(--los-line)" stroke-width="1.6"/>');

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
    labels.push(label('tp-ax tp-ax-x1', p.x + p.w, VIEW.h - 14, fmtDistance(data.distanceM)));
    out.push('</svg>');
    out.push(labels.join(''));
    out.push('</div>');
    // #730 M3: the band's colours in words, never colour alone. Outside
    // .tp-plot, whose span children are the positioned chart labels.
    // On a partial classification the clutter colour that is left may be
    // either, and says so.
    if (classified) {
      var items = [['b', CLASSES.b.legend], ['v', CLASSES.v.legend]];
      if (classificationState(data) === 'partial') items.push(['none', CLASS_NONE_LEGEND]);
      out.push('<div class="tp-legend" data-testid="tp-legend">');
      items.forEach(function (it) {
        out.push('<span class="tp-legend-item"><span class="tp-swatch tp-swatch-' + it[0] + '" aria-hidden="true"></span>' + esc(it[1]) + '</span>');
      });
      out.push('</div>');
    }
    return out.join('');
  }

  // ── Panel ────────────────────────────────────────────────────────────────

  function el(tag, cls, text) {
    var e = global.document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }

  /* footerCredit fills the chart footer from the server's dhm block
   * (#733, #765). The profile is computed from DHM — sampled, interpolated
   * and assessed — so the line is the block's derived credit, which says
   * the data was changed; the hillshade tiles are the agency's own
   * rendering and keep the plain one (public/map-tile-providers.js). The
   * dataset and licence marks become links through the one credit walker,
   * MC_appendCredit. A block without the derived credit falls back to the
   * plain one, which still names the owner and the licence, and without the
   * walker (map-tile-providers.js did not load) the credit is still shown,
   * as plain text. */
  /* #730 M3: a profile classified with the building footprints says so in
   * its attribution, which is then the server's dhm.buildings credit
   * (internal/buildings AttributionDerived). The footer shows that line
   * instead, with the footprints' dataset and source linked between the
   * DHM dataset and the licence, in the order they stand in the text (the
   * walker's ordering contract). Without the block nothing here names the
   * footprints. The footer is rebuilt for every answer. */
  function footerCredit(parent, cfg, data) {
    cfg = cfg || {};
    parent.textContent = '';
    var b = cfg.buildings;
    if (b && typeof b.credit === 'string' && b.credit && data && data.attribution === b.credit) {
      linkedCredit(parent, b.credit, buildingsCreditLinks(cfg, b), CREDIT_TESTIDS);
      return parent;
    }
    var text = typeof cfg.attributionDerived === 'string' && cfg.attributionDerived ? cfg.attributionDerived : cfg.attribution;
    linkedCredit(parent, typeof text === 'string' ? text : '', creditLinksOf(cfg), CREDIT_TESTIDS);
    return parent;
  }

  // The dataset and licence links of a credit block, or null without the
  // walker (map-tile-providers.js did not load).
  function creditLinksOf(cfg) {
    return typeof global.MC_creditLinks === 'function' ? global.MC_creditLinks(cfg) : null;
  }

  // The four links of the footprints' credit, in text order.
  function buildingsCreditLinks(cfg, b) {
    var dhm = creditLinksOf(cfg);
    if (!dhm) return null;
    var by = {};
    dhm.forEach(function (l) { by[l.key] = l; });
    return [by.dataset,
      { key: 'buildingsDataset', mark: b.datasetName, href: b.datasetUrl, rel: 'noopener noreferrer' },
      { key: 'buildingsSource', mark: b.sourceName, href: b.sourceUrl, rel: 'noopener noreferrer' },
      by.license];
  }

  /* linkedCredit appends `text` to `parent` with `links` (creditLinks
   * entries) as anchors, through the one credit walker, each anchor in the
   * panel's link style with its test id from `testids`; without the walker
   * the text is still shown, plain. */
  function linkedCredit(parent, text, links, testids) {
    if (typeof global.MC_appendCredit !== 'function' || !links) {
      parent.textContent = text;
      return;
    }
    global.MC_appendCredit(parent, text, links, function (a, link) {
      a.className = 'tp-credit-link';
      if (testids[link.key]) a.setAttribute('data-testid', testids[link.key]);
    });
  }

  /* fillCredit fills `parent` with the Copernicus DEM credit of a mixed
   * profile and hides it for any other (#738 M2, decision 2). The short
   * line is the server's dhm.fill block (internal/copdem Credit), its
   * dataset and licence marks linked as in the DHM footer; the full Art.
   * 6(b) line is the response's own attributionFill, one click away in a
   * disclosure rather than inline. A mixed profile without the block
   * still shows the full line: the credit is a licence obligation. */
  function fillCredit(parent, cfg, data) {
    parent.textContent = '';
    parent.hidden = !isMixed(data);
    if (parent.hidden) return parent;
    cfg = cfg || {};
    var full = typeof data.attributionFill === 'string' ? data.attributionFill : '';
    var short = typeof cfg.credit === 'string' ? cfg.credit : '';
    var line = el('p', 'tp-credit-line');
    line.setAttribute('data-testid', 'tp-credit-fill-line');
    parent.appendChild(line);
    if (!short) {
      line.textContent = full;
      return parent;
    }
    linkedCredit(line, short, creditLinksOf(cfg), FILL_CREDIT_TESTIDS);
    if (full) {
      var more = el('details', 'tp-credit-full');
      more.setAttribute('data-testid', 'tp-credit-fill-full');
      more.appendChild(el('summary', 'tp-credit-more', 'Full credit'));
      var text = el('p', 'tp-credit-line', full);
      text.setAttribute('data-testid', 'tp-credit-fill-text');
      more.appendChild(text);
      parent.appendChild(more);
    }
    return parent;
  }

  function endpointLabel(ref, name) {
    if (!ref) return 'not picked';
    if (ref.kind === 'node') return name || (ref.pubkey.slice(0, 8) + '…');
    return Number(ref.lat).toFixed(4) + ', ' + Number(ref.lon).toFixed(4);
  }

  var SOURCE_LABELS = {
    request: 'from this link',
    'operator-node': 'set by the operator for this node',
    /* #789 M1 — the server's estimate: the surface (roof, treetop) around
     * a repeater or room plus 2 m, never below the role default. */
    'dsm-estimate': 'estimated from surface model',
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
    estimate: { label: 'estimate', title: 'Estimated from the surface model: the roof or treetop around the node plus 2 m, never below the role default. A mast does not show in the surface model.' },
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
    'operator-role': 'operator',
    'dsm-estimate': 'estimate'
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

  /* #790: what the panel waits for, else null. While an end is still to be
   * picked — A, B, or the new position of a Move A / Move B (#721) — the
   * panel is a compact bar (title, this step, Cancel, Close), so on a
   * phone the map above it stays free to tap. With both ends set and no
   * move pending it is the full panel. */
  var PHASE_STATUS = {
    'pick-a': 'Pick the first point on the map.',
    'pick-b': 'Pick the second point on the map.',
    'move-a': 'Pick the new position for A on the map.',
    'move-b': 'Pick the new position for B on the map.'
  };
  function compactPhase(st, moving) {
    if (!st || !st.a) return 'pick-a';
    if (!st.b) return 'pick-b';
    if (moving === 'a' || moving === 'b') return 'move-' + moving;
    return null;
  }

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
          var err = new Error(b && b.error ? b.error : 'Request failed (' + r.status + ')');
          // #839 §6: a refusal's limit, for refusal().
          if (b && isFinite(Number(b.maxPathKm))) err.maxPathKm = Number(b.maxPathKm);
          throw err;
        });
        return r.json();
      });
    };
    var syncHash = opts.syncHash !== false;
    /* #721: the host owns picking, so Move A / Move B exist only when it can
     * arm a picker for one end. Swap needs nothing from the host. */
    var canPick = typeof opts.onMoveStart === 'function';

    var state = { a: null, b: null, model: themeDefaults().model, modelExplicit: false, ha: null, hb: null, k: null, freq: null };
    var open = false, seq = 0, debounceTimer = null, panel = null, nodes = { a: null, b: null };
    // The end whose Move button is armed ('a' | 'b'), else null (#721).
    var moving = null;
    /* #790: the phase syncCompact last drew (compactPhase), and whether the
     * viewer folded the finished profile down to its head (the ▾/▴ toggle). */
    var lastPhase = null, folded = false;
    // Where each height in `state` came from on this side of the wire:
    // 'yours' (typed, or remembered in localStorage), 'link', 'theme' (the
    // customizer's default for this kind of endpoint), or null.
    var origin = { a: null, b: null };
    /* The customizer's defaults applied to this measurement. Deliberately
     * NOT in `state`: a theme default is nobody's decision, so it must not
     * travel in a shared link, and after a reload the badge has to read
     * "default" again rather than "link" (#681 M6). */
    var themeH = { a: null, b: null };
    // #723 M3: monument lookups by query string (heritageAnswer).
    var heritageAnswers = new Map();
    // #723 M4: the resolved profile the note was last asked for, and its seq.
    var heritageShown = null;
    var ui = {};
    // #781: the hover cursor on the chart now drawn, else null.
    var cursor = null;
    /* #737 M2: the oblique photo row, only while the server offers the
     * photos. `drawn` is the profile on the chart with its geometry, and
     * `picked` the chart point clicked on it; both go with the chart. */
    var SF = global.Skraafoto;
    var sfOffered = !!(SF && SF.offered(opts.skraafoto));
    var drawn = null, picked = null;

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

    /* adoptThemeDefault applies a customizer height to one end the server
     * resolves out of its own built-in default — `end` is that resolution:
     * kind, role, antennaM and antennaSource — and reports whether anything
     * moved. An operator value, a remembered height and a link are all more
     * specific and are left alone; a customizer value equal to the server's
     * own is not worth sending. A surface-model estimate is replaced only by
     * a higher customizer value (FLOORED_SOURCES). */
    function adoptThemeDefault(side, end, defs) {
      if (!end || themeH[side] !== null) return false;
      if ((side === 'a' ? state.ha : state.hb) !== null) return false;
      var floored = FLOORED_SOURCES[end.antennaSource];
      if (!PROMOTABLE_SOURCES[end.antennaSource] && !floored) return false;
      var h = themeHeight(end, defs);
      if (h === null || h === clampHeight(end.antennaM)) return false;
      if (floored && !(h > clampHeight(end.antennaM))) return false;
      themeH[side] = h;
      origin[side] = 'theme';
      return true;
    }

    /* adoptThemeDefaults applies adoptThemeDefault to both ends of an
     * answer. Called once per measurement, so it cannot loop. */
    function adoptThemeDefaults(data) {
      var defs = themeDefaults();
      var a = adoptThemeDefault('a', data && data.a, defs);
      var b = adoptThemeDefault('b', data && data.b, defs);
      return a || b;
    }

    /* presetPointDefaults decides a free point's customizer height before
     * asking (#746). A point has no per-node key, so the server's whole
     * rule for it is known up front (dhm.pointAntenna, passed in as
     * opts.pointAntenna), and the first request can carry the height
     * instead of a second one after the answer. A node can have an operator
     * height of its own, so it still waits for the answer. */
    function presetPointDefaults() {
      var rule = opts.pointAntenna;
      if (!rule || typeof rule !== 'object') return;
      var defs = themeDefaults();
      ['a', 'b'].forEach(function (side) {
        if (!state[side] || state[side].kind !== 'point') return;
        adoptThemeDefault(side, { kind: 'point', antennaM: rule.m, antennaSource: rule.source }, defs);
      });
    }

    /* The customizer can move these defaults while the panel is open. Drop
     * what was adopted from the old values and measure again. */
    function onDefaultsChanged() {
      var defs = themeDefaults();
      if (!state.modelExplicit && state.model !== defs.model) {
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
      /* #790 F4: on a finished profile the title is the ▾/▴ fold toggle
       * (a disclosure button inside the heading), so the title row spends
       * no extra 48 px on it — the 640 px landscape row has no room left
       * for one (#784). In the compact bar the title is plain text. */
      var title = el('h3', 'tp-title');
      ui.titleText = el('span', '', 'Path profile');
      title.appendChild(ui.titleText);
      ui.fold = el('button', 'tp-fold', 'Path profile');
      ui.fold.type = 'button';
      // #796 N2: not the same name as the map's "Path profile" toggle.
      ui.fold.setAttribute('aria-label', 'Path profile details');
      ui.fold.hidden = true;
      ui.fold.setAttribute('aria-expanded', 'true');
      ui.fold.setAttribute('data-testid', 'tp-fold');
      ui.foldGlyph = el('span', 'tp-fold-glyph', '\u25BE');
      ui.foldGlyph.setAttribute('aria-hidden', 'true');
      ui.fold.appendChild(ui.foldGlyph);
      ui.fold.addEventListener('click', function () {
        folded = !folded;
        syncCompact();
        // #839 §2: the panel changed size; the host keeps the path in view.
        if (typeof opts.onLayoutChange === 'function') opts.onLayoutChange();
      });
      title.appendChild(ui.fold);
      head.appendChild(title);
      /* #796 F5: the chips have a row of their own inside the title row. It
       * takes the space between the title and Copy/Close and wraps inside
       * itself, so no chip combination (or font) can push Close onto a
       * second line and the chart under the bottom navigation. */
      var chips = el('div', 'tp-chips');
      head.appendChild(chips);
      ui.chip = el('span', 'tp-chip');
      ui.chip.setAttribute('data-testid', 'tp-verdict');
      // #790: an empty chip is a grey pill that reads as a drag handle.
      ui.chip.hidden = true;
      chips.appendChild(ui.chip);
      /* #729: the surface (buildings and trees) verdict, when it is worse
       * than the terrain one. It rides beside the verdict rather than under
       * the chart because "Clear" beside an obvious building is read in the
       * title row and nowhere else. */
      /* #746 N2: the chips are separate statements, but a screen reader
       * reads adjacent inline text as one ("Terrain: ClearBuildings/trees:
       * Blocked"). A visually hidden full stop between them, shown and
       * hidden with the surface chip, makes it two sentences. aria-label
       * on a plain span is not reliably announced, so it is text. */
      ui.surfaceSep = el('span', 'sr-only', '. ');
      ui.surfaceSep.hidden = true;
      chips.appendChild(ui.surfaceSep);
      ui.surface = el('span', 'tp-chip tp-chip-surface');
      ui.surface.setAttribute('data-testid', 'tp-surface-chip');
      ui.surface.hidden = true;
      chips.appendChild(ui.surface);
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
      chips.appendChild(ui.assumed);
      /* #790: Cancel, shown only in the compact bar. During a Move it is
       * the Escape of #721 (the end stays); while picking it resets the
       * profile, exactly as Close does, on the same page. */
      ui.cancel = el('button', 'tp-cancel', 'Cancel');
      ui.cancel.type = 'button';
      ui.cancel.hidden = true;
      ui.cancel.setAttribute('data-testid', 'tp-cancel');
      // Focus goes back to the Move button in syncCompact, for this click
      // and for Escape alike (#790 F1).
      ui.cancel.addEventListener('click', function () {
        if (moving) api.cancelMove(); else api.close();
      });
      head.appendChild(ui.cancel);
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
      // #790: the compact bar's one line — what to do next. Outside the
      // body, so collapsing the body can never hide it.
      ui.pickStatus = el('p', 'tp-pick-status');
      ui.pickStatus.setAttribute('role', 'status');
      ui.pickStatus.setAttribute('data-testid', 'tp-pick-status');
      ui.pickStatus.hidden = true;
      panel.appendChild(ui.pickStatus);
      ui.copyStatus = el('p', 'tp-copy-status');
      ui.copyStatus.setAttribute('role', 'status');
      ui.copyStatus.setAttribute('data-testid', 'tp-copy-status');
      panel.appendChild(ui.copyStatus);

      var body = el('div', 'tp-body');
      body.setAttribute('data-testid', 'tp-body');
      ui.body = body;
      ui.chart = el('div', 'tp-chart');
      ui.chart.setAttribute('data-testid', 'tp-chart');
      body.appendChild(ui.chart);
      if (sfOffered) body.appendChild(skraafotoRow());

      // The server's derived credit, with the dataset and the licence marks
      // as links to the dataset page and the licence itself: the CC BY 4.0
      // obligations, spelled out once (#680, #733, #765).
      ui.credit = el('p', 'tp-credit-line');
      footerCredit(ui.credit, opts.dhm);
      body.appendChild(ui.credit);
      // #738 M2: the Copernicus credit, only while the profile is mixed.
      ui.fillCredit = el('div', 'tp-credit-fill');
      ui.fillCredit.setAttribute('data-testid', 'tp-credit-fill');
      ui.fillCredit.hidden = true;
      body.appendChild(ui.fillCredit);

      ui.detail = el('p', 'tp-detail');
      ui.detail.setAttribute('data-testid', 'tp-detail');
      body.appendChild(ui.detail);
      // #738 M2: what a mixed profile's verdict rests on outside Denmark.
      ui.mixedNote = el('p', 'tp-mixed-note');
      ui.mixedNote.setAttribute('data-testid', 'tp-mixed-note');
      ui.mixedNote.hidden = true;
      body.appendChild(ui.mixedNote);

      /* #729: what a surface verdict actually means for a radio link. It
       * only appears with the chip it explains, so a profile where the two
       * models agree is not padded with a line about nothing. */
      ui.surfaceNote = el('p', 'tp-surface-note', SURFACE_NOTE);
      ui.surfaceNote.setAttribute('data-testid', 'tp-surface-note');
      ui.surfaceNote.hidden = true;
      body.appendChild(ui.surfaceNote);
      // #730 M3: a partial or unavailable classification says so, muted.
      ui.classNote = el('p', 'tp-class-note');
      ui.classNote.setAttribute('data-testid', 'tp-class-note');
      ui.classNote.hidden = true;
      body.appendChild(ui.classNote);

      /* #723 M3: a protected ancient monument at an end — a chip, then
       * its note: the warning, each end's zone type and distance, and the
       * register's credit. Not in the title row: with the surface chip and
       * "assumed heights" there, one more chip wrapped Copy and Close onto
       * a second row and pushed the chart off a short landscape screen
       * (#784 review). Under the chart it moves nothing. */
      ui.heritage = el('span', 'tp-chip tp-chip-warn tp-chip-heritage');
      ui.heritage.setAttribute('data-testid', 'tp-heritage-chip');
      ui.heritage.hidden = true;
      body.appendChild(ui.heritage);
      ui.heritageNote = el('p', 'tp-heritage-note');
      ui.heritageNote.setAttribute('data-testid', 'tp-heritage-note');
      ui.heritageNote.hidden = true;
      body.appendChild(ui.heritageNote);

      var controls = el('div', 'tp-controls');
      ui.endA = endpointControl('a', 'A');
      ui.endB = endpointControl('b', 'B');
      controls.appendChild(ui.endA.wrap);
      controls.appendChild(ui.endB.wrap);
      controls.appendChild(swapControl());

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
          // The viewer's own pick: a later customizer change leaves it
          // alone, and it rides the link.
          state.modelExplicit = true;
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
      var move = null;
      if (canPick) {
        move = el('button', 'tp-seg-btn tp-move', 'Move ' + label);
        move.type = 'button';
        move.disabled = true;
        move.title = 'Pick a new position for ' + label + ' on the map: a node marker or any point. Escape, or this button again, cancels.';
        move.setAttribute('aria-pressed', 'false');
        move.setAttribute('data-testid', 'tp-move-' + side);
        move.addEventListener('click', function () {
          if (moving === side) api.cancelMove(); else api.beginMove(side);
        });
        wrap.appendChild(move);
      }

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
      return { wrap: wrap, name: name, input: input, source: source, badge: badge, move: move };
    }

    /* #737 M2: "Oblique photo" at A, at B, and at the point last clicked
     * on the chart (or picked with Enter at the keyboard cursor). Under
     * the chart, not in the head: the title row must not wrap (#784
     * review), and the chart itself does not move. Hidden until a chart
     * is drawn, so a panel still waiting for B stays as compact as before
     * (#790). Asks nothing until a button is pressed. */
    function skraafotoRow() {
      ui.sfRow = el('div', 'tp-sf-row');
      ui.sfRow.setAttribute('role', 'group');
      ui.sfRow.setAttribute('aria-label', 'Oblique photo');
      ui.sfRow.setAttribute('data-testid', 'tp-sf-row');
      ui.sfRow.hidden = true;
      var lab = el('span', 'tp-sf-label', 'Oblique photo');
      ui.sfRow.appendChild(lab);
      function end(side, label) {
        return function () {
          var p = endPoint(drawn && drawn.data, side);
          var ref = state[side];
          return p && { lat: p.lat, lon: p.lon, label: label + ': ' + endpointLabel(ref, nodes[side] && nodes[side].name),
            line: sightLine(drawn && drawn.data) };
        };
      }
      ui.sfEnds = {};
      [['a', 'A'], ['b', 'B']].forEach(function (s) {
        var b = SF.actionButton({ cls: 'tp-seg-btn tp-sf-btn', testid: 'tp-sf-' + s[0], text: s[1], icon: false, cfg: opts.skraafoto,
          title: 'Oblique aerial photo at ' + s[1], getPoint: end(s[0], s[1]) });
        b.setAttribute('aria-label', 'Oblique photo at ' + s[1]);
        ui.sfEnds[s[0]] = b;
        ui.sfRow.appendChild(b);
      });
      ui.sfPoint = SF.actionButton({ cls: 'tp-seg-btn tp-sf-btn', testid: 'tp-sf-point', text: 'Chart point', icon: false, cfg: opts.skraafoto,
        getPoint: function () {
          return picked && { lat: picked.lat, lon: picked.lon, label: 'Profile point, ' + fmtDistance(picked.distanceM) + ' from A',
            line: sightLine(drawn && drawn.data) };
        } });
      ui.sfRow.appendChild(ui.sfPoint);
      /* #839 §12: why Chart point is off, in words a touch screen shows.
       * The button's title says it too, but only a mouse ever sees that. */
      ui.sfHint = el('span', 'tp-sf-hint', 'Tap the chart to choose a point.');
      ui.sfHint.setAttribute('data-testid', 'tp-sf-hint');
      ui.sfRow.appendChild(ui.sfHint);
      setPicked(null);
      ui.chart.addEventListener('click', function (e) {
        var plot = drawn && e.target && e.target.closest ? e.target.closest('.tp-plot') : null;
        var svg = plot && plot.querySelector('svg');
        if (svg) setPicked(pointAt(drawn.data, drawn.g, chartIndexAt(drawn.g, svg.getBoundingClientRect(), e.clientX)));
      });
      ui.chart.addEventListener('keydown', function (e) {
        if (e.key !== 'Enter' || !drawn || !cursor || cursor.index() === null) return;
        setPicked(pointAt(drawn.data, drawn.g, cursor.index()));
      });
      return ui.sfRow;
    }

    function setPicked(p) {
      picked = p || null;
      if (!ui.sfPoint) return;
      ui.sfPoint.disabled = !picked;
      ui.sfHint.hidden = !!picked;
      ui.sfPoint.querySelector('.sf-action-text').textContent = picked ? 'Chart point · ' + fmtDistance(picked.distanceM) : 'Chart point';
      ui.sfPoint.title = picked
        ? 'Oblique aerial photo at the chart point ' + fmtDistance(picked.distanceM) + ' from A'
        : 'Click or tap the chart to pick a point; Enter picks the keyboard cursor\'s point.';
      ui.sfPoint.setAttribute('aria-label', picked ? 'Oblique photo at the chart point, ' + fmtDistance(picked.distanceM) + ' from A'
        : 'Oblique photo at a chart point: click or tap the chart first');
      syncPhotoRow();
    }

    /* syncPhotoRow offers a photo button only where the photos are
     * (photoCovered, #839): A and B each by their end, "Chart point" while
     * DHM measured some of the path and the point picked, if any, is on
     * it. The row goes with its last button, and with the chart. */
    function syncPhotoRow() {
      var data = drawn && drawn.data;
      var cfg = opts.skraafoto;
      var a = !!data && photoCovered(data, cfg, endPoint(data, 'a'), 0);
      var b = !!data && photoCovered(data, cfg, endPoint(data, 'b'), Number(data.distanceM));
      ui.sfEnds.a.hidden = !a;
      ui.sfEnds.b.hidden = !b;
      // dhmStretch walks the samples: once per drawn profile, not per pick.
      if (data && drawn.stretch === undefined) drawn.stretch = dhmStretch(data);
      ui.sfPoint.hidden = !data || !(a || b || drawn.stretch)
        || !!(picked && !photoCovered(data, cfg, picked, picked.distanceM));
      ui.sfRow.hidden = !data || (!a && !b && ui.sfPoint.hidden);
    }

    /* setDrawn records the profile now on the chart (null: none), which
     * drops a point picked on the one before. */
    function setDrawn(data) {
      drawn = data ? { data: data, g: chartGeometry(data) } : null;
      if (!ui.sfRow) return;
      setPicked(null);
    }

    function swapControl() {
      var wrap = el('div', 'tp-field tp-swap-field');
      ui.swap = el('button', 'tp-seg-btn tp-swap', 'Swap A/B');
      ui.swap.type = 'button';
      ui.swap.disabled = true;
      ui.swap.setAttribute('aria-label', 'Swap A and B');
      ui.swap.setAttribute('data-testid', 'tp-swap');
      ui.swap.addEventListener('click', function () { api.swap(); });
      wrap.appendChild(ui.swap);
      return wrap;
    }

    function syncMoveButtons() {
      var both = !!(state.a && state.b);
      [['a', ui.endA], ['b', ui.endB]].forEach(function (pair) {
        var b = pair[1] && pair[1].move;
        if (!b) return;
        b.disabled = !both;
        b.classList.toggle('active', moving === pair[0]);
        b.setAttribute('aria-pressed', moving === pair[0] ? 'true' : 'false');
      });
      if (ui.swap) ui.swap.disabled = !both;
      syncCompact();
    }

    /* syncCompact shows the full panel or the compact bar for the phase
     * (#790). Only visibility changes: the chart, the cursor (#781) and
     * every control stay built, so a cancelled Move comes back as it was. */
    function syncCompact() {
      if (!panel) return;
      var phase = compactPhase(state, moving);
      var compact = phase !== null;
      var shut = compact || folded;
      var active = doc.activeElement;
      // Focus inside the body would be stranded once it is hidden.
      var stranded = shut && active && ui.body.contains && ui.body.contains(active);
      /* F1: a Move that ends (Cancel, Escape, a pick) hides Cancel; if it
       * held focus, focus goes back to the Move button the move began at
       * instead of falling to <body>. */
      var back = null;
      if (!compact && active && active === ui.cancel && (lastPhase === 'move-a' || lastPhase === 'move-b')) {
        back = (lastPhase === 'move-a' ? ui.endA : ui.endB).move;
      }
      lastPhase = phase;
      panel.classList.toggle('tp-compact', compact);
      panel.classList.toggle('tp-folded', !compact && folded);
      panel.setAttribute('data-phase', phase || 'ready');
      ui.body.hidden = shut;
      ui.copy.hidden = compact;
      ui.cancel.hidden = !compact;
      /* Folded, a finished profile keeps its head: the title, the verdict
       * chips and the verdict as the status line; a real button, not a
       * drag gesture. */
      ui.fold.hidden = compact;
      ui.titleText.hidden = !compact;
      ui.fold.setAttribute('aria-expanded', folded ? 'false' : 'true');
      ui.fold.title = folded ? 'Expand the path profile' : 'Collapse the path profile';
      ui.foldGlyph.textContent = folded ? '\u25B4' : '\u25BE';
      ui.pickStatus.hidden = !shut;
      // Folded, the status line carries the verdict the body would show.
      ui.pickStatus.textContent = compact ? PHASE_STATUS[phase] : (folded ? (ui.chip.title || '') : '');
      if (compact && stranded && ui.cancel.focus) ui.cancel.focus();
      else if (stranded && ui.fold.focus) ui.fold.focus();
      else if (back && back.focus) back.focus();
    }

    /* endpointsChanged is the shared tail of a move and a swap: the panel
     * names, the hash at once (replaceState), and one debounced request. An
     * answer still in flight belongs to the old ends, so scheduleRequest
     * drops it. */
    function endpointsChanged() {
      renderNames();
      setHeritage(null);
      setStatus('Measuring the terrain along the path…', 'tp-loading');
      setChip({ state: 'pending', tone: 'unknown', label: '…', detail: 'Waiting for the terrain profile.' });
      setSurfaceChip(null);
      setMixed(null);
      setClassified(null);
      if (ui.assumed) ui.assumed.hidden = true;
      scheduleRequest();
      if (opts.onEndpointsChange) opts.onEndpointsChange();
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
      ui.chip.hidden = !chip.label;
      ui.chip.setAttribute('data-state', chip.state);
      ui.chip.title = chip.detail;
      // A folded panel shows the verdict as its status line (#790 F4).
      if (folded) syncCompact();
    }

    /* setSurfaceChip shows the second chip and its explanatory line, or
     * takes both away together: the line exists to explain the chip. */
    function setSurfaceChip(surf) {
      var show = !!(surf && surf.show);
      if (ui.surface) {
        ui.surface.hidden = !show;
        ui.surface.className = 'tp-chip tp-chip-surface tp-chip-' + (show ? surf.tone : 'unknown');
        ui.surface.textContent = show ? surf.label : '';
        ui.surface.setAttribute('data-state', show ? surf.state : '');
        ui.surface.title = show ? surf.detail : '';
      }
      if (ui.surfaceSep) ui.surfaceSep.hidden = !show;
      if (ui.surfaceNote) ui.surfaceNote.hidden = !(show && surf.note);
    }

    /* setMixed shows the mixed-resolution note and the Copernicus credit
     * for data, or takes both away (data null: no profile on screen). */
    function setMixed(data) {
      var note = mixedNote(data);
      if (ui.mixedNote) {
        ui.mixedNote.hidden = !note;
        ui.mixedNote.textContent = note || '';
      }
      if (ui.fillCredit) fillCredit(ui.fillCredit, opts.dhm && opts.dhm.fill, data);
    }

    /* setClassified shows the classification's note and the credit for
     * data, or takes the note away and puts the DHM credit back (data
     * null: no profile on screen). */
    function setClassified(data) {
      var note = classNote(data);
      if (ui.classNote) {
        ui.classNote.hidden = !note;
        ui.classNote.textContent = note || '';
      }
      if (ui.credit) footerCredit(ui.credit, opts.dhm, data);
    }

    function writeHash() {
      if (!syncHash) return;
      try {
        var loc = global.location;
        var next = profileURL(loc, loc.hash, open ? hashParams(state) : {}, false);
        global.history.replaceState(global.history.state, '', next);
      } catch (_) { /* a hash we cannot write is not worth breaking the tool for */ }
    }

    /* The /p link needs the server's route (opts.shareRoute) and the
     * viewer's customizer switch, read now so a change needs no reload. */
    function onCopy() {
      var url = shareURL(state, global.location, opts.shareRoute === true && themeDefaults().shareLinks);
      if (!url) return;
      copyLink(url, opts.copyText || global.copyToClipboard).then(function (ok) {
        if (!ui.copyStatus) return;
        ui.copyStatus.textContent = ok ? 'Link copied' : 'Could not copy the link. It is in the address bar.';
      });
    }

    /* setHeritage shows a heritageNote result, or takes the chip and its
     * note away together (null). */
    function setHeritage(note) {
      if (!ui.heritage) return;
      var show = !!note;
      ui.heritage.hidden = ui.heritageNote.hidden = !show;
      ui.heritage.textContent = show ? note.label : '';
      ui.heritage.title = show ? note.text : '';
      ui.heritage.setAttribute('data-lead', show ? note.lead : '');
      // Sink: heritageNote's markup, every dataset value through esc().
      ui.heritageNote.innerHTML = show ? note.html : '';
    }

    /* heritageAnswer is one end's lookup as a promise of its body, or of
     * null when it fails or there is nothing to ask. Answers are kept by
     * query, most recently used last, at most HERITAGE_KEEP; a failure is
     * not kept, so the next profile may ask again. */
    function heritageAnswer(end) {
      var key = heritageQuery(end);
      if (key === null) return Promise.resolve(null);
      var p = heritageAnswers.get(key);
      if (p) {
        heritageAnswers.delete(key);
      } else {
        p = fetchJSON(HERITAGE_ENDPOINT + '?' + key).then(function (body) {
          return body && typeof body === 'object' ? body : null;
        }, function () {
          if (heritageAnswers.get(key) === p) heritageAnswers.delete(key);
          return null;
        });
      }
      heritageAnswers.set(key, p);
      if (heritageAnswers.size > HERITAGE_KEEP) heritageAnswers.delete(heritageAnswers.keys().next().value);
      return p;
    }

    /* lookupHeritage asks about both ends of the profile `mine` drew. It
     * runs once per resolved profile, so the profile's own debounce
     * coalesces it too; an answer for ends that have moved since is
     * dropped. */
    function lookupHeritage(data, mine) {
      if (!heritageOffered(opts.heritage)) return;
      heritageShown = { data: data, seq: mine };
      // #723 M4: the warning switched off asks the server nothing.
      if (!heritageViewerSettings().profileWarning) { setHeritage(null); return; }
      Promise.all([heritageAnswer(data.a), heritageAnswer(data.b)]).then(function (r) {
        if (mine !== seq || !open) return;
        setHeritage(heritageNote({ a: r[0], b: r[1] }, opts.heritage, heritageViewerSettings()));
      });
    }

    /* #723 M4 — the customizer moved a monument setting: the profile on
     * screen redraws its note from the remembered answers (an end not
     * asked about yet, with the warning just switched on, is asked once).
     * Nothing is re-measured. */
    function onHeritageSettingsChanged() {
      if (!open || !heritageShown || heritageShown.seq !== seq) return;
      lookupHeritage(heritageShown.data, seq);
    }

    /* scheduleRequest debounces the request for an edit. Whatever is still
     * in flight was asked for the state before the edit, so it is dropped
     * now, not when the debounced request goes out: otherwise an old
     * answer landing inside the window overwrites the height being edited,
     * the chart and the verdict (#746). */
    function scheduleRequest() {
      seq++;
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
      syncMoveButtons();
    }

    /* #781: the hover cursor lives in public/terrain-profile-cursor.js and
     * only ever adds to the chart drawn here; the host (the Map page) gets
     * the point along the path through opts.onCursor. Detached before the
     * chart is replaced or the panel shuts, so no listener outlives it. */
    function detachCursor() {
      if (cursor) cursor.detach();
      cursor = null;
    }

    function attachCursor(data, g) {
      detachCursor();
      var C = global.TerrainProfileCursor;
      if (!C || !opts.onCursor || !ui.chart || !ui.chart.querySelector) return;
      cursor = C.attach({
        plot: ui.chart.querySelector('.tp-plot'),
        geometry: g || chartGeometry(data),
        data: data,
        onMove: opts.onCursor
      });
    }

    function setStatus(text, cls) {
      if (!ui.chart) return;
      detachCursor();
      setDrawn(null);
      ui.chart.textContent = '';
      var p = el('p', 'tp-status ' + (cls || ''), text);
      p.setAttribute('data-testid', 'tp-status');
      ui.chart.appendChild(p);
    }

    function render(data) {
      var chip = verdictChip(data);
      setChip(chip);
      setSurfaceChip(surfaceChip(data));
      setMixed(data);
      setClassified(data);
      var names = { nameA: nodes.a && nodes.a.name, nameB: nodes.b && nodes.b.name };
      var hasSamples = !!(data.samples && data.samples.length);
      /* The credit is a licence obligation, so it is one HTML line under
       * the chart rather than text inside the SVG: the chart box is
       * stretched to the panel width and in-SVG text came out ~3 px tall
       * on a 360 px phone, which is not an attribution anybody can read. */
      // ...and only when DHM data is shown: a path wholly outside Denmark
      // used none (#839).
      ui.credit.hidden = !!(data.coverage && data.coverage.state === 'none');
      // Either end on a default height caveats the whole verdict — a
      // customizer default included (#681 M6).
      ui.assumed.hidden = !(assumedHeight(data.a, origin.a) || assumedHeight(data.b, origin.b));
      if (!hasSamples) {
        setStatus(chip.detail, 'tp-empty');
      } else {
        // Sink: one SVG string from buildChart, where every node-controlled
        // name went through esc() at interpolation time.
        detachCursor();
        ui.chart.innerHTML = buildChart(data, names);
        setDrawn(data);
        attachCursor(data, drawn.g);
      }
      // Without a plot the status line or the empty plot already says it
      // (#839): a path wholly abroad has samples, all without data.
      ui.detail.textContent = drawn && drawn.g ? chip.detail : '';
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
      setHeritage(null);
      setStatus('Measuring the terrain along the path…', 'tp-loading');
      setChip({ state: 'pending', tone: 'unknown', label: '…', detail: 'Waiting for the terrain profile.' });
      setSurfaceChip(null);
      setMixed(null);
      setClassified(null);
      if (ui.assumed) ui.assumed.hidden = true;
      presetPointDefaults();
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
        lookupHeritage(data, mine);
      }).catch(function (err) {
        if (mine !== seq || !open) return;
        var why = refusal(err);
        setChip({ state: 'error', tone: 'unknown', label: why ? why.label : 'No profile', detail: why ? why.text : String(err && err.message || err) });
        setSurfaceChip(null);
        setMixed(null);
        setClassified(null);
        if (ui.assumed) ui.assumed.hidden = true;
        setStatus(why ? why.text : String((err && err.message) || 'Could not load the terrain profile.'), 'tp-error');
        // A refusal the viewer can fix says so once, in the chart's place.
        ui.detail.textContent = why ? '' : 'Could not load the terrain profile.';
      });
    }

    /* shut closes the panel and drops every answer still in flight. Only
     * an explicit Close takes the profile out of the hash. A stated model
     * belonged to the profile that is closing: the next path is the
     * viewer's own and follows the customizer again (#765, #772 F2). */
    function shut(rewriteHash) {
      if (!open) return;
      open = false;
      seq++;
      detachCursor();
      setDrawn(null);
      if (debounceTimer) { global.clearTimeout(debounceTimer); debounceTimer = null; }
      state.a = null;
      state.b = null;
      state.model = themeDefaults().model;
      state.modelExplicit = false;
      moving = null;
      themeH.a = null;
      themeH.b = null;
      setHeritage(null);
      if (panel) panel.classList.remove('is-open');
      if (rewriteHash) writeHash();
      if (opts.onClose) opts.onClose();
    }

    var api = {
      el: function () { return panel; },
      isOpen: function () { return open; },
      getState: function () {
        return { a: state.a, b: state.b, model: state.model, modelExplicit: state.modelExplicit,
          ha: state.ha, hb: state.hb, k: state.k, freq: state.freq };
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
         * customizer change must not quietly override it. parseState says
         * whether it did; comparing with the default cannot, since a stated
         * model can equal it (#746). */
        state.modelExplicit = next.modelExplicit === true;
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
      /* moveEndpoint replaces one end of an open measurement and keeps the
       * other end, the model, k, freq and the heights (#721). The moved end
       * keeps a height the viewer or the link set, unless this browser
       * remembers one for the new end, which is more specific; a default
       * height stays a default, now the new end's own. Returns false, and
       * costs nothing, for a move onto either end or without both ends. */
      moveEndpoint: function (side, ref) {
        if ((side !== 'a' && side !== 'b') || !ref || !open || !state.a || !state.b) return false;
        var other = side === 'a' ? 'b' : 'a';
        if (sameRef(ref, state[side]) || sameRef(ref, state[other])) return false;
        moving = null;
        var hk = side === 'a' ? 'ha' : 'hb';
        var recalled = recallHeight(ref);
        if (recalled !== null) {
          state[hk] = recalled;
          origin[side] = 'yours';
        } else if (origin[side] !== 'yours' && origin[side] !== 'link') {
          state[hk] = null;
          origin[side] = null;
        }
        state[side] = ref;
        // A default the old end adopted says nothing about the new one.
        themeH[side] = null;
        if (panel) (side === 'a' ? ui.endA : ui.endB).input.value = state[hk] === null ? '' : String(state[hk]);
        endpointsChanged();
        return true;
      },
      /* swap exchanges A and B with everything that belongs to each end. */
      swap: function () {
        if (!open || !state.a || !state.b) return false;
        var t = state.a; state.a = state.b; state.b = t;
        t = state.ha; state.ha = state.hb; state.hb = t;
        t = origin.a; origin.a = origin.b; origin.b = t;
        t = themeH.a; themeH.a = themeH.b; themeH.b = t;
        if (panel) {
          t = ui.endA.input.value; ui.endA.input.value = ui.endB.input.value; ui.endB.input.value = t;
        }
        if (moving) api.cancelMove();
        endpointsChanged();
        return true;
      },
      /* beginMove arms the host's picker for one end; the panel only shows
       * which. Nothing changes until the host calls moveEndpoint. */
      beginMove: function (side) {
        if ((side !== 'a' && side !== 'b') || !canPick || !open || !state.a || !state.b) return;
        moving = side;
        syncMoveButtons();
        opts.onMoveStart(side);
      },
      /* cancelMove is Escape: the end stays where it was, nothing is
       * fetched, the hash is untouched, and the host disarms. */
      cancelMove: function () {
        if (!moving) return;
        moving = null;
        syncMoveButtons();
        if (opts.onMoveCancel) opts.onMoveCancel();
      },
      movingSide: function () { return moving; },
      open: function () {
        if (open) return;
        open = true;
        // A new profile opens unfolded (#790 F4).
        folded = false;
        lastPhase = null;
        if (!panel) mount.appendChild(buildPanel());
        panel.classList.add('is-open');
        renderNames();
        writeHash();
      },
      close: function () { shut(true); },
      /* destroy is teardown, not a Close: the router updates the address
       * bar before it destroys the old page (public/app.js navigate), so
       * the hash already belongs to the destination and is left alone
       * (#746). */
      destroy: function () {
        shut(false);
        if (global.removeEventListener) {
          global.removeEventListener('terrain-defaults-changed', onDefaultsChanged);
          global.removeEventListener('heritage-defaults-changed', onHeritageSettingsChanged);
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
      global.addEventListener('heritage-defaults-changed', onHeritageSettingsChanged);
    }
    return api;
  }

  global.TerrainProfile = {
    footerCredit: footerCredit,
    fillCredit: fillCredit,
    mixedNote: mixedNote,
    FILL_OVERVIEW_M: FILL_OVERVIEW_M,
    ASSUMED_LABEL: ASSUMED_LABEL,
    ENDPOINT: ENDPOINT,
    // #790: the compact bar's phase, read by the unit tests.
    compactPhase: compactPhase,
    HASH_PARAM: HASH_PARAM,
    MAX_HEIGHT_M: MAX_HEIGHT_M,
    parseState: parseState,
    parseStart: parseStart,
    invalidLink: invalidLink,
    withProfileParams: withProfileParams,
    pointRef: pointRef,
    markerRef: markerRef,
    snapEndpoint: snapEndpoint,
    shareURL: shareURL,
    shareOffered: shareOffered,
    fitPadding: fitPadding,
    fitPaddingBeside: fitPaddingBeside,
    panelCap: panelCap,
    endsVisible: endsVisible,
    refusal: refusal,
    fmtDistance: fmtDistance,
    parseProfilePair: parseProfilePair,
    offered: offered,
    nodeEntryHash: nodeEntryHash,
    nodeEntryAction: nodeEntryAction,
    // #737 M2: the node-detail fix rule and the photo points.
    hasFix: hasFix,
    endPoint: endPoint,
    // #839: where the profile's oblique photos are.
    dhmAt: dhmAt,
    dhmStretch: dhmStretch,
    photoCovered: photoCovered,
    sightLine: sightLine,
    pointAt: pointAt,
    chartIndexAt: chartIndexAt,
    // #772 F4: the fit margin, read back by tests instead of copied in.
    FIT_MARGIN_PX: FIT_MARGIN_PX,
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
    surfaceChip: surfaceChip,
    HERITAGE_WARNING: HERITAGE_WARNING,
    heritageNote: heritageNote,
    SURFACE_NOTE: SURFACE_NOTE,
    classNote: classNote,
    CLASS_PARTIAL_NOTE: CLASS_NOTES.partial,
    chartGeometry: chartGeometry,
    buildChart: buildChart,
    create: create,
    // Test seam only: lets the unit tests inspect the very storage the
    // module uses, instead of reaching for a global that may not exist.
    storageForTests: storage
  };
})(typeof window !== 'undefined' ? window : this);
