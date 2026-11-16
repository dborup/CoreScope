/* skraafoto.js — #737 M2: the Skråfoto viewer, oblique aerial photos of a
 * map point from Klimadatastyrelsen, through this server's proxy (#737 M1,
 * cmd/server/skraafoto_proxy.go).
 *
 * Offered only while /api/config/client carries an enabled `skraafoto`
 * block (window.MC_SKRAAFOTO_CFG, set by roles.js): the server sends it
 * only when map.skraafoto.enabled is on and a token is configured, and it
 * never carries the token. Without it this module shows nothing at all.
 *
 * Entry points: a node with a fix inside the block's bounds, Denmark
 * (node page and side pane, nodeAction), and the Path profile's ends and
 * clicked chart point where DHM measured them (terrain-profile.js,
 * actionButton, photoCovered, #839). Each opens one dialog:
 *   - one search per point and year (GET searchUrl?lat=&lon=[&year=]),
 *     the newest year first, with a selector for the older ones;
 *   - a direction switcher for the five photos (N, E, S, W, nadir);
 *   - only the photo shown is fetched: no prefetch of the other four, and
 *     a photo seen before is not fetched again while the dialog is open;
 *   - the photo date, the server's privacy note, its #733 credit with the
 *     dataset and licence links, and "Full resolution" in the official
 *     viewer;
 *   - loading, error (429, 503, other) and empty states;
 *   - Escape and Close return focus to the button that opened it; Tab
 *     stays inside; Escape is taken before the page's own Escape handlers
 *     (node page "back", Map "close the profile").
 *
 * Deep link (AGENTS.md "Deep Linking"): the point, direction and year
 * ride the hash as skraafoto=<lat>,<lon>,<dir>[,<year>] next to whatever
 * the page keeps there, written with replaceState, removed on close; a
 * cold load or a hash change that carries it opens the viewer there.
 *
 * #737 M3c, the marks: an SVG layer over the photo, in the photo's own
 * pixels (viewBox) and scaled with it (xMidYMid meet, the image's
 * object-fit: contain), so it stays on the point through resizes and
 * rotations:
 *   - a crosshair at the search's `point` (M3a, preview pixels), only when
 *     `inFrame`, dashed when `approx` (a footprint height, not DHM);
 *   - opened from a Path profile, the line of sight A→B (the search is
 *     asked for the profile's `path`, `ha` and `hb`; the answer's `path`);
 *   - "Zoom in" / "Zoom out", a button with aria-pressed: the M3b crop
 *     around the point (preview ?zoom=1&x=&y=), fetched on the first click
 *     only, kept like a preview; the crosshair then sits at the crop's
 *     X-CoreScope-Skraafoto-Marker, the line is moved into the crop by
 *     X-CoreScope-Skraafoto-Crop. Another direction or year turns it off;
 *   - from a profile the first photo is the oblique most perpendicular to
 *     A–B, which sees the line from the side; otherwise north, as before;
 *   - the layer's aria-label says where the point is and that a line is
 *     drawn (markText).
 *
 * #737 customizer (AGENTS rule 8): the viewer's defaults, section
 * `skraafoto` of the customizer (public/customize-v2.js), by the one rule
 * here (SETTINGS), read when a viewer opens:
 *   - direction: 'auto' is the above (perpendicular from a profile,
 *     otherwise north); a direction is the first photo from anywhere. A
 *     deep-linked direction still wins;
 *   - zoom: 'overview', or 'zoomed': each new photo (direction or year)
 *     opens on its crop when its point is in frame; the button still wins;
 *   - lineOfSight: false neither asks for the line nor draws it; the side
 *     view of A–B is still the 'auto' direction.
 * The server's map.skraafoto config stays server-only.
 *
 * Everything shown is built with createElement(NS) + textContent and
 * setAttribute; nothing from the server or a node passes through an HTML
 * string.
 *
 * Exposes window.Skraafoto. */
(function (global) {
  'use strict';

  // The five photos, in the order the server returns them (#737 Q5).
  var DIRECTIONS = ['north', 'east', 'south', 'west', 'nadir'];
  var DIR_SHORT = { north: 'N', east: 'E', south: 'S', west: 'W', nadir: 'Nadir' };
  var DIR_TEXT = {
    north: 'Looking north', east: 'Looking east', south: 'Looking south', west: 'Looking west',
    nadir: 'Straight down (nadir)'
  };
  // internal/skraafoto MinYear: 2017 is left out (#737 Q4).
  var MIN_YEAR = 2019;
  var MAX_YEAR = 2100;
  var HASH_PARAM = 'skraafoto';
  // The official viewer, the "Full resolution" link (#737 Q9).
  var VIEWER_PREFIX = 'https://skraafoto.dataforsyningen.dk/?';
  // Same-origin API paths only: a block cannot point the viewer elsewhere.
  var SEARCH_PATH_RE = /^\/api\/[A-Za-z0-9_\/-]+$/;
  // The server's own preview path of one photo (internal/skraafoto ids).
  var PREVIEW_RE = /^\/api\/skraafoto\/preview\/skraafotos20[0-9]{2}\/[0-9]{4}_[0-9A-Za-z_]{1,64}$/;
  var COORD_RE = /^-?[0-9]{1,3}(\.[0-9]{1,10})?$/;
  // Photos kept as object URLs while a dialog is open: five a year.
  var MAX_KEPT = 10;

  // ── Viewer defaults (#737 customizer) ──────────────────────────────────────
  // The customizer's section `skraafoto` and the viewer share this rule,
  // so the customizer stores exactly what the viewer reads. The defaults
  // are M3c's behaviour.
  var SETTING_KEYS = ['direction', 'zoom', 'lineOfSight'];
  var SETTING_DEFAULTS = { direction: 'auto', zoom: 'overview', lineOfSight: true };
  var DIRECTION_SETTINGS = ['auto'].concat(DIRECTIONS);
  var ZOOM_SETTINGS = ['overview', 'zoomed'];
  // What the customizer calls each value.
  var SETTING_LABELS = {
    direction: {
      auto: 'Auto: across the line from a Path profile, otherwise north',
      north: 'Looking north', east: 'Looking east', south: 'Looking south', west: 'Looking west',
      nadir: 'Straight down (nadir)'
    },
    zoom: { overview: 'The whole photo', zoomed: 'Zoomed in around the point' }
  };

  /* validSetting: whether v is a usable value of viewer setting key. */
  function validSetting(key, v) {
    if (key === 'direction') return typeof v === 'string' && DIRECTION_SETTINGS.indexOf(v) >= 0;
    if (key === 'zoom') return typeof v === 'string' && ZOOM_SETTINGS.indexOf(v) >= 0;
    if (key === 'lineOfSight') return typeof v === 'boolean';
    return false;
  }

  /* settings is the viewer's defaults: section (default: the live
   * window.SITE_CONFIG.skraafoto, the customizer's effective config) over
   * the built-ins, valid values only, key by key. */
  function settings(section) {
    var src = section;
    if (src === undefined) {
      try { src = global.SITE_CONFIG && global.SITE_CONFIG.skraafoto; } catch (_) { src = null; }
    }
    var ok = !!src && typeof src === 'object';
    var out = {};
    SETTING_KEYS.forEach(function (k) {
      out[k] = ok && Object.prototype.hasOwnProperty.call(src, k) && validSetting(k, src[k]) ? src[k] : SETTING_DEFAULTS[k];
    });
    return out;
  }

  // ── Pure helpers ───────────────────────────────────────────────────────────

  function offered(cfg) {
    return !!(cfg && cfg.enabled === true && typeof cfg.searchUrl === 'string' && SEARCH_PATH_RE.test(cfg.searchUrl));
  }

  function round5(v) { return Math.round(v * 1e5) / 1e5; }

  /* point is a coordinate pair the search accepts, rounded to ~1 m. */
  function point(lat, lon) {
    if (lat === null || lon === null || lat === undefined || lon === undefined || lat === '' || lon === '') return null;
    var la = Number(lat), lo = Number(lon);
    if (!isFinite(la) || !isFinite(lo) || la < -90 || la > 90 || lo < -180 || lo > 180) return null;
    return { lat: round5(la), lon: round5(lo) };
  }

  /* covered: the point is inside the block's bounds, Denmark (#818), by
   * the heritage overlay's bounds test (HeritageInfo.inBounds, loaded on
   * every page). A block without bounds, an older server's, sets no
   * limit. The bounds are a rectangle, so they take in Skåne and northern
   * Schleswig too: the Path profile adds its own DHM coverage on top
   * (terrain-profile.js photoCovered, #839). */
  function covered(cfg, p) {
    var hi = global.HeritageInfo;
    return !hi || typeof hi.inBounds !== 'function' || hi.inBounds(cfg && cfg.bounds, p.lat, p.lon);
  }

  /* nodePoint is where a node's photos are: its own position, by the
   * node-detail "real fix" rule (TerrainProfile.hasFix), else null. */
  function nodePoint(node) {
    var tp = global.TerrainProfile;
    if (!tp || typeof tp.hasFix !== 'function' || !tp.hasFix(node)) return null;
    return point(node.lat, node.lon);
  }

  function validYear(y) {
    return typeof y === 'number' && y % 1 === 0 && y >= MIN_YEAR && y <= MAX_YEAR;
  }

  /* searchURL is the search at a point and year; with line (sightLine)
   * the line of sight is asked for too: path=latA,lonA,latB,lonB, and ha
   * and hb when known (#737 M3a). */
  function searchURL(cfg, lat, lon, year, line) {
    var p = point(lat, lon);
    if (!p || !offered(cfg)) return null;
    var u = cfg.searchUrl + '?lat=' + p.lat.toFixed(5) + '&lon=' + p.lon.toFixed(5);
    if (validYear(year)) u += '&year=' + year;
    if (!line) return u;
    u += '&path=' + [line.a.lat, line.a.lon, line.b.lat, line.b.lon].map(function (v) { return v.toFixed(5); }).join(',');
    if (line.ha !== null) u += '&ha=' + line.ha;
    if (line.hb !== null) u += '&hb=' + line.hb;
    return u;
  }

  function previewURL(photo) {
    var u = photo && photo.previewUrl;
    return typeof u === 'string' && PREVIEW_RE.test(u) ? u : null;
  }

  function viewerLink(photo) {
    var u = photo && photo.viewerUrl;
    return typeof u === 'string' && u.indexOf(VIEWER_PREFIX) === 0 && !/[\s"'<>\\]/.test(u) ? u : null;
  }

  /* orderPhotos is the answer's photos in DIRECTIONS order, one per
   * direction (the first wins), unknown directions dropped. */
  function orderPhotos(photos) {
    var by = {};
    (Array.isArray(photos) ? photos : []).forEach(function (p) {
      if (p && DIRECTIONS.indexOf(p.direction) >= 0 && !by[p.direction]) by[p.direction] = p;
    });
    return DIRECTIONS.filter(function (d) { return by[d]; }).map(function (d) { return by[d]; });
  }

  function pickDirection(ordered, wanted) {
    if (!ordered || !ordered.length) return null;
    for (var i = 0; i < ordered.length; i++) if (ordered[i].direction === wanted) return wanted;
    return ordered[0].direction;
  }

  function yearChoices(years) {
    var out = [];
    (Array.isArray(years) ? years : []).forEach(function (y) {
      if (validYear(y) && out.indexOf(y) < 0) out.push(y);
    });
    return out.sort(function (a, b) { return b - a; });
  }

  function selectedYear(resp) {
    var ys = yearChoices(resp && resp.years);
    if (!ys.length) return 0;
    return ys.indexOf(resp.year) >= 0 ? resp.year : ys[0];
  }

  function photoDate(dt) {
    var m = /^([0-9]{4}-[0-9]{2}-[0-9]{2})T/.exec(typeof dt === 'string' ? dt : '');
    return m ? m[1] : '';
  }

  /* errorText words a failed search or preview. retrySec is Retry-After. */
  function errorText(status, retrySec) {
    var wait = retrySec > 0 ? ' Try again in about ' + Math.ceil(retrySec) + ' s.' : ' Try again in a minute.';
    if (status === 429) return 'Too many photo requests from you just now.' + wait;
    if (status === 503) return 'Oblique photos are busy, or this server\'s daily allowance is used up.' + (retrySec > 0 ? wait : ' Try again later.');
    if (status === 404) return 'This photo was not found.';
    return 'Oblique photos are unavailable right now.';
  }

  function formatLink(s) {
    var p = s && point(s.lat, s.lon);
    if (!p || DIRECTIONS.indexOf(s.dir) < 0) return null;
    return p.lat.toFixed(5) + ',' + p.lon.toFixed(5) + ',' + s.dir + (validYear(s.year) ? ',' + s.year : '');
  }

  function parseLink(v) {
    if (typeof v !== 'string' || !v) return null;
    var f = v.split(',');
    if (f.length < 2 || f.length > 4 || !COORD_RE.test(f[0]) || !COORD_RE.test(f[1])) return null;
    var p = point(f[0], f[1]);
    if (!p) return null;
    var dir = null, year = 0;
    if (f.length > 2) {
      if (DIRECTIONS.indexOf(f[2]) < 0) return null;
      dir = f[2];
    }
    if (f.length > 3) {
      year = /^[0-9]{4}$/.test(f[3]) ? Number(f[3]) : NaN;
      if (!validYear(year)) return null;
    }
    return { lat: p.lat, lon: p.lon, dir: dir, year: year };
  }

  /* withParam writes (or, with value null, removes) the viewer's key in
   * a hash and keeps every other key, as withProfileParams does for the
   * profile's. */
  function withParam(hash, value) {
    hash = String(hash || '');
    var qi = hash.indexOf('?');
    var base = qi >= 0 ? hash.slice(0, qi) : hash;
    var params = new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '');
    params.delete(HASH_PARAM);
    if (value) params.set(HASH_PARAM, value);
    var qs = params.toString();
    return qs ? (base || '#') + '?' + qs : base;
  }

  function linkFromHash(hash) {
    var h = String(hash || '');
    var qi = h.indexOf('?');
    if (qi < 0) return null;
    return parseLink(new URLSearchParams(h.slice(qi + 1)).get(HASH_PARAM));
  }

  /* creditLinks: the #733 marks, in text order (the #765 contract). */
  function creditLinks(c) {
    if (!c) return [];
    return [
      { key: 'dataset', mark: c.datasetName, href: c.datasetUrl, rel: 'noopener noreferrer' },
      { key: 'license', mark: c.licenseName, href: c.licenseUrl, rel: 'noopener noreferrer license' }
    ];
  }

  // ── Marks (#737 M3c) ───────────────────────────────────────────────────────

  var SVG_NS = 'http://www.w3.org/2000/svg';
  // The search's oblique directions and the bearing each looks along.
  var VIEW_BEARING = { north: 0, east: 90, south: 180, west: 270 };
  // cmd/server/skraafoto_zoom.go: x and y at most skraafoto.PreviewMaxPx.
  var ZOOM_MAX_PX = 1024;
  var MARKER_HEADER = 'X-CoreScope-Skraafoto-Marker';
  var CROP_HEADER = 'X-CoreScope-Skraafoto-Crop';
  // The M3b headers: plain non-negative decimals, as the server writes them.
  var HEADER_NUM_RE = /^[0-9]{1,5}(\.[0-9]{1,6})?$/;
  // The crosshair, in screen pixels: arm length and the gap at its centre.
  var CROSS_ARM_PX = 14;
  var CROSS_GAP_PX = 4;

  function num(v) { return typeof v === 'number' && isFinite(v); }
  function xy(p) { return p && num(p.x) && num(p.y) ? { x: p.x, y: p.y } : null; }
  function size(s) {
    var w = s && (s.width !== undefined ? s.width : s.w), h = s && (s.height !== undefined ? s.height : s.h);
    return num(w) && num(h) && w > 0 && h > 0 ? { width: w, height: h } : null;
  }
  function round2(v) { var r = Math.round(v * 100) / 100; return r === 0 ? 0 : r; }

  /* displayMap is how a photo of size space is shown in a box of size box
   * under object-fit: contain, the image's CSS, and so also under the SVG
   * layer's xMidYMid meet: one scale, centred, letterboxed along one axis.
   * Display pixels are relative to the box's top-left corner. */
  function displayMap(space, box) {
    var s = size(space), b = size(box);
    if (!s || !b) return null;
    var k = Math.min(b.width / s.width, b.height / s.height);
    return { scale: k, x: (b.width - s.width * k) / 2, y: (b.height - s.height * k) / 2 };
  }

  function toDisplay(p, m) {
    return p && m ? { x: m.x + p.x * m.scale, y: m.y + p.y * m.scale } : null;
  }

  function headerNums(v, n) {
    if (typeof v !== 'string') return null;
    var f = v.split(',');
    if (f.length !== n) return null;
    for (var i = 0; i < n; i++) if (!HEADER_NUM_RE.test(f[i])) return null;
    return f.map(Number);
  }

  /* parseMarker reads X-CoreScope-Skraafoto-Marker: the point in crop pixels. */
  function parseMarker(v) {
    var f = headerNums(v, 2);
    return f ? { x: f[0], y: f[1] } : null;
  }

  /* parseCropBox reads X-CoreScope-Skraafoto-Crop: the crop's box in
   * preview pixels, x0,y0,x1,y1, of some size. */
  function parseCropBox(v) {
    var f = headerNums(v, 4);
    return f && f[2] > f[0] && f[3] > f[1] ? { x0: f[0], y0: f[1], x1: f[2], y1: f[3] } : null;
  }

  /* toCrop moves preview pixel p into a crop: { box, width, height }, the
   * box in preview pixels and the crop's own size. */
  function toCrop(p, crop) {
    var q = xy(p), b = crop && crop.box, s = size(crop);
    if (!q || !b || !s) return null;
    return { x: (q.x - b.x0) * s.width / (b.x1 - b.x0), y: (q.y - b.y0) * s.height / (b.y1 - b.y0) };
  }

  function pathOf(path) {
    if (!Array.isArray(path) || path.length < 2) return null;
    var out = [];
    for (var i = 0; i < path.length; i++) {
      var q = xy(path[i]);
      if (!q) return null;
      out.push(q);
    }
    return out;
  }

  /* marks is what to draw on photo p: { width, height } is the space (the
   * preview's frame, or zoomed the crop's size), point the crosshair
   * (null: none), path the line of sight (null: none). crop, when zoomed:
   * { box, width, height, marker }, from the M3b headers; zoomed is then
   * true. null when the photo has no camera model (no frame). */
  function marks(p, crop) {
    var frame = size(p && p.frame);
    if (!frame) return null;
    var line = pathOf(p.path);
    var approx = p.approx === true;
    if (crop) {
      var cs = size(crop);
      if (!cs) return null;
      var mk = xy(crop.marker);
      if (mk && (mk.x < 0 || mk.y < 0 || mk.x > cs.width || mk.y > cs.height)) mk = null;
      var moved = line && crop.box ? line.map(function (q) { return toCrop(q, crop); }) : null;
      return { width: cs.width, height: cs.height, point: mk, path: moved, approx: approx, zoomed: true };
    }
    return { width: frame.width, height: frame.height, point: p.inFrame === true ? xy(p.point) : null, path: line, approx: approx };
  }

  /* zoomURL is the M3b crop around photo p's point, or null when there is
   * no point in frame to zoom on. */
  function zoomURL(p) {
    var u = previewURL(p), q = p && p.inFrame === true ? xy(p.point) : null;
    if (!u || !q) return null;
    var x = round2(q.x), y = round2(q.y);
    if (x < 0 || y < 0 || x > ZOOM_MAX_PX || y > ZOOM_MAX_PX) return null;
    return u + '?zoom=1&x=' + x + '&y=' + y;
  }

  /* sightLine is a profile's line A→B as the search takes it: two points,
   * and the antenna heights when they are in the server's 0–100 m range
   * (otherwise the server's default height). null without two points. */
  function sightLine(l) {
    var a = l && l.a && point(l.a.lat, l.a.lon), b = l && l.b && point(l.b.lat, l.b.lon);
    if (!a || !b) return null;
    function h(v) { return num(v) && v >= 0 && v <= 100 ? round2(v) : null; }
    return { a: a, b: b, ha: h(l.ha), hb: h(l.hb) };
  }

  /* perpendicularDirection is the oblique photo among photos that looks
   * most nearly at right angles to the line A→B, so the line runs across
   * it: by ground bearing (longitude shrunk by cos latitude), the first in
   * DIRECTIONS order on a tie. null without a line or an oblique photo. */
  function perpendicularDirection(l, photos) {
    var s = sightLine(l);
    if (!s) return null;
    var k = Math.cos((s.a.lat + s.b.lat) / 2 * Math.PI / 180);
    var dx = (s.b.lon - s.a.lon) * k, dy = s.b.lat - s.a.lat;
    if (dx === 0 && dy === 0) return null;
    var bearing = Math.atan2(dx, dy);
    var best = null, score = -1;
    orderPhotos(photos).forEach(function (p) {
      if (!(p.direction in VIEW_BEARING)) return;
      var v = Math.abs(Math.sin(VIEW_BEARING[p.direction] * Math.PI / 180 - bearing));
      if (v > score + 1e-9) { score = v; best = p.direction; }
    });
    return best;
  }

  /* placeWords names where p lies in a frame of w × h, on a 3 × 3 grid. */
  function placeWords(p, w, h) {
    var col = p.x < w / 3 ? 'left' : p.x > w * 2 / 3 ? 'right' : 'centre';
    var row = p.y < h / 3 ? 'top' : p.y > h * 2 / 3 ? 'bottom' : 'centre';
    if (row === 'centre') return col === 'centre' ? 'centre' : 'centre-' + col;
    return row + '-' + col;
  }

  /* markText is the marks' text equivalent. */
  function markText(m) {
    if (!m) return '';
    var t = m.point
      ? 'Point marked at the ' + placeWords(m.point, m.width, m.height) + (m.approx ? ', position approximate.' : '.')
      : m.zoomed ? 'Not marked in this view.' : 'The point is outside this photo.';
    return m.path ? t + ' Line of sight from A to B drawn on the photo.' : t;
  }

  // ── DOM ────────────────────────────────────────────────────────────────────

  function el(tag, cls, text) {
    var e = global.document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }

  // Static markup, no interpolation: a Phosphor sprite icon, the camera
  // or the close cross.
  function icon(camera) {
    var s = el('span', 'sf-icon');
    s.setAttribute('aria-hidden', 'true');
    s.innerHTML = camera
      ? '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-camera"/></svg>'
      : '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-x"/></svg>';
    return s;
  }

  var current = null; // the open viewer, if any
  var clipSeq = 0; // numbers each viewer's clipPath id

  /* open shows the viewer at a point. o: { lat, lon, label, opener, cfg,
   * year, dir, line, fetch }. cfg defaults to the server's block; line is
   * a Path profile's A→B (sightLine), drawn on the photo; fetch is a seam
   * for tests. Returns the viewer, or null when not offered. */
  function open(o) {
    o = o || {};
    var cfg = o.cfg || global.MC_SKRAAFOTO_CFG;
    var at = point(o.lat, o.lon);
    if (!offered(cfg) || !at) return null;
    if (current) current.close(false);
    var doc = global.document;
    var fetcher = o.fetch || function (u, init) { return global.fetch(u, init); };
    var opener = o.opener || null;
    // A page may re-render while the viewer is open (the node page draws
    // its card twice), replacing the opener: then its successor, the
    // element with the same test id, gets the focus back.
    var openerId = opener && opener.getAttribute ? opener.getAttribute('data-testid') : null;
    var line = sightLine(o.line);
    // The customizer's defaults, as they are now (#737 customizer).
    var pref = settings();
    var linkDir = DIRECTIONS.indexOf(o.dir) >= 0 ? o.dir : null;
    var prefDir = pref.direction !== 'auto' ? pref.direction : null;
    var zoomFirst = pref.zoom === 'zoomed';
    var drawLine = pref.lineOfSight;
    // A deep link's direction, then the customizer's, then north.
    var state = { year: validYear(o.year) ? o.year : 0, dir: linkDir || prefDir || 'north', resp: null, photos: [], zoom: false };
    // From a profile, the first answer picks the side view of the line,
    // unless a direction was asked for (a deep link or the customizer).
    var autoDir = !!line && !linkDir && !prefDir;
    var seq = 0, pseq = 0, previewAbort = null, searchAbort = null;
    var kept = []; // [{ url, objectURL, crop }], newest last; crop: a zoom's headers
    var shown = null; // the object URL that is the image's source
    var ui = {};
    var gone = false;

    var overlay = el('div', 'sf-overlay');
    overlay.setAttribute('data-testid', 'sf-overlay');
    var dialog = el('div', 'sf-dialog');
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');
    dialog.setAttribute('aria-labelledby', 'sfTitle');
    dialog.setAttribute('data-testid', 'sf-dialog');
    dialog.setAttribute('tabindex', '-1');
    overlay.appendChild(dialog);

    var head = el('header', 'sf-head');
    var titles = el('div', 'sf-titles');
    var title = el('h2', 'sf-title', 'Oblique photo');
    title.id = 'sfTitle';
    titles.appendChild(title);
    ui.where = el('p', 'sf-where', o.label ? String(o.label) : at.lat.toFixed(5) + ', ' + at.lon.toFixed(5));
    ui.where.setAttribute('data-testid', 'sf-where');
    titles.appendChild(ui.where);
    head.appendChild(titles);
    var closeBtn = el('button', 'sf-close');
    closeBtn.type = 'button';
    closeBtn.setAttribute('aria-label', 'Close oblique photo');
    closeBtn.setAttribute('data-testid', 'sf-close');
    closeBtn.appendChild(icon(false));
    closeBtn.addEventListener('click', function () { api.close(true); });
    head.appendChild(closeBtn);
    dialog.appendChild(head);

    var controls = el('div', 'sf-controls');
    var dirs = el('div', 'sf-dirs');
    dirs.setAttribute('role', 'group');
    dirs.setAttribute('aria-label', 'Direction');
    ui.dirs = {};
    DIRECTIONS.forEach(function (d) {
      var b = el('button', 'sf-dir', DIR_SHORT[d]);
      b.type = 'button';
      b.title = DIR_TEXT[d];
      b.setAttribute('aria-label', DIR_TEXT[d]);
      b.setAttribute('aria-pressed', 'false');
      b.setAttribute('data-dir', d);
      b.setAttribute('data-testid', 'sf-dir-' + d);
      b.disabled = true;
      b.addEventListener('click', function () { choose(d); });
      ui.dirs[d] = b;
      dirs.appendChild(b);
    });
    controls.appendChild(dirs);
    var yearWrap = el('label', 'sf-year-wrap');
    yearWrap.setAttribute('data-testid', 'sf-year-wrap');
    ui.yearWrap = yearWrap;
    yearWrap.appendChild(el('span', 'sf-year-label', 'Year'));
    ui.year = el('select', 'sf-year');
    ui.year.setAttribute('data-testid', 'sf-year');
    ui.year.disabled = true;
    ui.year.addEventListener('change', function () {
      var y = Number(ui.year.value);
      if (validYear(y) && y !== state.year) { state.year = y; search(); }
    });
    yearWrap.appendChild(ui.year);
    controls.appendChild(yearWrap);
    ui.zoom = el('button', 'sf-zoom', 'Zoom in');
    ui.zoom.type = 'button';
    ui.zoom.setAttribute('aria-pressed', 'false');
    ui.zoom.setAttribute('data-testid', 'sf-zoom');
    ui.zoom.title = 'A closer view around the point';
    ui.zoom.disabled = true;
    ui.zoom.addEventListener('click', function () {
      if (ui.zoom.disabled) return;
      state.zoom = !state.zoom;
      syncZoom();
      show();
    });
    controls.appendChild(ui.zoom);
    dialog.appendChild(controls);

    var stage = el('div', 'sf-stage');
    stage.setAttribute('data-testid', 'sf-stage');
    ui.img = el('img', 'sf-img');
    ui.img.setAttribute('data-testid', 'sf-img');
    ui.img.alt = '';
    ui.img.hidden = true;
    // The image and its marks share one box: the SVG layer covers the
    // image element and letterboxes as it does.
    var frame = el('div', 'sf-frame');
    frame.appendChild(ui.img);
    ui.marks = doc.createElementNS(SVG_NS, 'svg');
    ui.marks.setAttribute('class', 'sf-marks');
    ui.marks.setAttribute('data-testid', 'sf-marks');
    ui.marks.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    ui.marks.setAttribute('focusable', 'false');
    // hidden is an HTML attribute only: on SVG the property does nothing,
    // so the attribute itself is set (and .sf-marks[hidden] hides it).
    ui.marks.setAttribute('hidden', '');
    frame.appendChild(ui.marks);
    stage.appendChild(frame);
    ui.status = el('p', 'sf-status');
    ui.status.setAttribute('role', 'status');
    ui.status.setAttribute('data-testid', 'sf-status');
    stage.appendChild(ui.status);
    dialog.appendChild(stage);

    var meta = el('p', 'sf-meta');
    ui.date = el('span', 'sf-date');
    ui.date.setAttribute('data-testid', 'sf-date');
    meta.appendChild(ui.date);
    ui.full = el('a', 'sf-full', 'Full resolution');
    ui.full.setAttribute('data-testid', 'sf-full');
    ui.full.setAttribute('target', '_blank');
    ui.full.setAttribute('rel', 'noopener noreferrer');
    ui.full.hidden = true;
    meta.appendChild(ui.full);
    dialog.appendChild(meta);

    // The privacy note (#737 Q10) and the credit are the server's words,
    // from its block: no copy of either lives here (#733 guard).
    var note = el('p', 'sf-note', typeof cfg.note === 'string' ? cfg.note : '');
    note.setAttribute('data-testid', 'sf-note');
    note.hidden = !note.textContent;
    dialog.appendChild(note);
    ui.credit = el('p', 'sf-credit');
    ui.credit.setAttribute('data-testid', 'sf-credit');
    dialog.appendChild(ui.credit);
    setCredit(cfg.credit);

    overlay.addEventListener('click', function (e) { if (e.target === overlay) api.close(true); });

    function setCredit(c) {
      ui.credit.textContent = '';
      var text = c && typeof c.attribution === 'string' ? c.attribution : '';
      if (typeof global.MC_appendCredit === 'function') {
        global.MC_appendCredit(ui.credit, text, creditLinks(c), function (a, l) { a.setAttribute('data-credit', l.key); });
      } else {
        ui.credit.textContent = text;
      }
    }

    function setState(s, text) {
      dialog.setAttribute('data-state', s);
      stage.setAttribute('aria-busy', s === 'loading' ? 'true' : 'false');
      ui.status.textContent = text || '';
      ui.status.hidden = !text;
    }

    function writeHash() {
      try {
        var v = formatLink({ lat: at.lat, lon: at.lon, dir: state.dir, year: state.year });
        var loc = global.location;
        global.history.replaceState(global.history.state, '', loc.pathname + loc.search + withParam(loc.hash, gone ? null : v));
      } catch (_) { /* a hash we cannot write is not worth breaking the viewer for */ }
    }

    function syncControls() {
      var have = {};
      state.photos.forEach(function (p) { have[p.direction] = true; });
      DIRECTIONS.forEach(function (d) {
        var b = ui.dirs[d];
        b.disabled = !have[d];
        b.setAttribute('aria-pressed', d === state.dir && have[d] ? 'true' : 'false');
        b.classList.toggle('active', d === state.dir && !!have[d]);
      });
      var years = yearChoices(state.resp && state.resp.years);
      ui.year.textContent = '';
      years.forEach(function (y) {
        var opt = el('option', null, String(y));
        opt.value = String(y);
        ui.year.appendChild(opt);
      });
      ui.year.value = String(state.year);
      ui.year.disabled = years.length < 2;
      // No year to show (an empty answer, #839): no empty select either.
      ui.yearWrap.hidden = !years.length;
      syncZoom();
    }

    /* syncZoom: the toggle is offered for a photo with its point in
     * frame, pressed while the crop is (or is being) shown. */
    function syncZoom() {
      var can = !!zoomURL(photoOf(state.dir));
      if (!can) state.zoom = false;
      ui.zoom.disabled = !can;
      ui.zoom.setAttribute('aria-pressed', state.zoom ? 'true' : 'false');
      ui.zoom.textContent = state.zoom ? 'Zoom out' : 'Zoom in';
    }

    function svg(tag, cls, parent) {
      var e = doc.createElementNS(SVG_NS, tag);
      if (cls) e.setAttribute('class', cls);
      parent.appendChild(e);
      return e;
    }

    var clipId = 'sf-marks-clip-' + (++clipSeq); // unique per viewer
    var drawn = null; // { p, crop }: the marks on screen, redrawn on resize

    /* drawMarks puts photo p's marks over the image: the crosshair and the
     * line, in the image's own pixels. Called once the image has loaded,
     * and again when it is resized (the crosshair keeps its screen size). */
    function drawMarks(p, crop) {
      drawn = { p: p, crop: crop };
      var c = crop ? { box: crop.box, marker: crop.marker, width: ui.img.naturalWidth, height: ui.img.naturalHeight } : null;
      var m = marks(p, c);
      if (!m) { hideMarks(); return; }
      if (!drawLine) m.path = null;
      var root = ui.marks;
      root.textContent = '';
      root.setAttribute('viewBox', '0 0 ' + m.width + ' ' + m.height);
      root.setAttribute('role', 'img');
      root.setAttribute('aria-label', markText(m));
      // Clipped to the photo itself: the outer svg clips only to its box,
      // which takes in the letterbox bars, and zoomed the line's points
      // lie far outside the crop.
      var clip = svg('clipPath', null, svg('defs', null, root));
      clip.setAttribute('id', clipId);
      var r = svg('rect', null, clip);
      r.setAttribute('x', '0');
      r.setAttribute('y', '0');
      r.setAttribute('width', String(m.width));
      r.setAttribute('height', String(m.height));
      var g = svg('g', 'sf-marks-clip', root);
      g.setAttribute('clip-path', 'url(#' + clipId + ')');
      g.setAttribute('data-testid', 'sf-marks-clip');
      var dm = displayMap(m, { width: ui.img.clientWidth, height: ui.img.clientHeight });
      var unit = dm && dm.scale > 0 ? 1 / dm.scale : m.width / 400;
      if (m.path) {
        var pts = m.path.map(function (q) { return round2(q.x) + ',' + round2(q.y); }).join(' ');
        svg('polyline', 'sf-los-halo', g).setAttribute('points', pts);
        var los = svg('polyline', 'sf-los', g);
        los.setAttribute('points', pts);
        los.setAttribute('data-testid', 'sf-los');
      }
      if (m.point) {
        var x = round2(m.point.x), y = round2(m.point.y);
        var cross = svg('g', 'sf-cross' + (m.approx ? ' sf-cross-approx' : ''), g);
        cross.setAttribute('data-testid', 'sf-cross');
        cross.setAttribute('data-x', String(x));
        cross.setAttribute('data-y', String(y));
        cross.setAttribute('transform', 'translate(' + x + ' ' + y + ')');
        var arm = round2(CROSS_ARM_PX * unit), gap = round2(CROSS_GAP_PX * unit);
        ['sf-cross-halo', 'sf-cross-line'].forEach(function (cls) {
          [[gap, 0, arm, 0], [-gap, 0, -arm, 0], [0, gap, 0, arm], [0, -gap, 0, -arm]].forEach(function (l) {
            var e = svg('line', cls, cross);
            e.setAttribute('x1', String(l[0]));
            e.setAttribute('y1', String(l[1]));
            e.setAttribute('x2', String(l[2]));
            e.setAttribute('y2', String(l[3]));
          });
        });
      }
      root.removeAttribute('hidden');
    }

    /* hideMarks takes the layer away while a photo loads or fails: hidden,
     * emptied, and without the last photo's text equivalent. */
    function hideMarks() {
      drawn = null;
      ui.marks.textContent = '';
      ui.marks.removeAttribute('role');
      ui.marks.removeAttribute('aria-label');
      ui.marks.setAttribute('hidden', '');
    }

    // The crosshair is sized in screen pixels: redrawn when the image is
    // resized (a window resize, a phone rotated). Coalesced by the
    // observer, one redraw per frame at most.
    var resizer = typeof global.ResizeObserver === 'function'
      ? new global.ResizeObserver(function () { if (drawn && !gone && !ui.img.hidden) drawMarks(drawn.p, drawn.crop); })
      : null;
    if (resizer) resizer.observe(ui.img);

    function retryAfter(r) {
      var v = r && r.headers && r.headers.get ? Number(r.headers.get('Retry-After')) : 0;
      return isFinite(v) && v > 0 ? v : 0;
    }

    function search() {
      var mine = ++seq;
      pseq++;
      if (previewAbort) previewAbort.abort();
      if (searchAbort) searchAbort.abort();
      searchAbort = typeof AbortController === 'function' ? new AbortController() : null;
      ui.img.hidden = true;
      ui.full.hidden = true;
      ui.date.textContent = '';
      setState('loading', 'Looking up photos…');
      hideMarks();
      state.zoom = zoomFirst;
      var url = searchURL(cfg, at.lat, at.lon, state.year, drawLine ? line : null);
      fetcher(url, searchAbort ? { signal: searchAbort.signal } : undefined).then(function (r) {
        if (!r.ok) {
          var err = new Error('status ' + r.status);
          err.status = r.status;
          err.retry = retryAfter(r);
          throw err;
        }
        return r.json();
      }).then(function (resp) {
        if (mine !== seq || gone) return;
        state.resp = resp || {};
        state.photos = orderPhotos(state.resp.photos);
        state.year = selectedYear(state.resp);
        if (state.resp.credit) setCredit(state.resp.credit);
        if (!state.photos.length) {
          syncControls();
          setState('empty', 'No oblique photos at this point.');
          writeHash();
          return;
        }
        if (autoDir) state.dir = perpendicularDirection(line, state.photos) || state.dir;
        autoDir = false;
        state.dir = pickDirection(state.photos, state.dir);
        syncControls();
        writeHash();
        show();
      }).catch(function (err) {
        if (mine !== seq || gone || (err && err.name === 'AbortError')) return;
        state.photos = [];
        syncControls();
        setState('error', errorText(err && err.status ? err.status : 0, err && err.retry));
      });
    }

    function photoOf(d) {
      for (var i = 0; i < state.photos.length; i++) if (state.photos[i].direction === d) return state.photos[i];
      return null;
    }

    function keptOf(u) {
      for (var i = 0; i < kept.length; i++) if (kept[i].url === u) return kept[i];
      return null;
    }

    function isKept(objectURL) {
      for (var i = 0; i < kept.length; i++) if (kept[i].objectURL === objectURL) return true;
      return false;
    }

    /* keep holds a fetched photo for the dialog. A copy pushed out is
     * revoked, unless it is the image's source: display() revokes that
     * one when the next photo replaces it, close() when the dialog goes. */
    function keep(u, objectURL, crop) {
      kept.push({ url: u, objectURL: objectURL, crop: crop || null });
      while (kept.length > MAX_KEPT) {
        var old = kept.shift();
        if (old.objectURL !== shown) revoke(old.objectURL);
      }
    }

    function revoke(u) {
      try { global.URL.revokeObjectURL(u); } catch (_) {}
    }

    function display(objectURL, p, crop) {
      ui.img.alt = 'Oblique aerial photo, ' + DIR_TEXT[p.direction].toLowerCase() + (photoDate(p.datetime) ? ', ' + photoDate(p.datetime) : '')
        + (crop ? ', zoomed in around the point' : '');
      ui.img.onload = function () { if (!gone) { ui.img.hidden = false; setState('ready', ''); drawMarks(p, crop); } };
      ui.img.onerror = function () { if (!gone) { ui.img.hidden = true; hideMarks(); setState('error', errorText(0)); } };
      var was = shown;
      shown = objectURL;
      ui.img.src = objectURL;
      if (was && was !== objectURL && !isKept(was)) revoke(was);
    }

    /* show puts the chosen direction's photo up: the one preview it
     * needs, or zoomed its crop, fetched now, or the copy this dialog
     * already holds. */
    function show() {
      var p = photoOf(state.dir);
      if (!p) return;
      var mine = ++pseq;
      if (previewAbort) previewAbort.abort();
      previewAbort = null;
      var date = photoDate(p.datetime);
      ui.date.textContent = DIR_TEXT[p.direction] + (date ? ' · photographed ' + date : '');
      var link = viewerLink(p);
      ui.full.hidden = !link;
      if (link) ui.full.setAttribute('href', link); else ui.full.removeAttribute('href');
      ui.img.hidden = true;
      hideMarks();
      var zoomed = state.zoom;
      var u = zoomed ? zoomURL(p) : previewURL(p);
      if (!u) { setState('error', 'No preview of this photo here; open it in full resolution.'); return; }
      var have = keptOf(u);
      if (have) { setState('loading', zoomed ? 'Zooming in…' : 'Loading photo…'); display(have.objectURL, p, have.crop); return; }
      setState('loading', zoomed ? 'Zooming in…' : 'Loading photo…');
      var crop = null;
      previewAbort = typeof AbortController === 'function' ? new AbortController() : null;
      fetcher(u, previewAbort ? { signal: previewAbort.signal } : undefined).then(function (r) {
        if (!r.ok) {
          var err = new Error('status ' + r.status);
          err.status = r.status;
          err.retry = retryAfter(r);
          throw err;
        }
        if (zoomed) {
          var h = r.headers && r.headers.get ? r.headers : { get: function () { return null; } };
          crop = { marker: parseMarker(h.get(MARKER_HEADER)), box: parseCropBox(h.get(CROP_HEADER)) };
        }
        return r.blob();
      }).then(function (blob) {
        if (gone) return;
        var objectURL = global.URL.createObjectURL(blob);
        keep(u, objectURL, crop);
        if (mine !== pseq) return;
        display(objectURL, p, crop);
      }).catch(function (err) {
        if (mine !== pseq || gone || (err && err.name === 'AbortError')) return;
        setState('error', errorText(err && err.status ? err.status : 0, err && err.retry));
      });
    }

    function choose(d) {
      if (d === state.dir && dialog.getAttribute('data-state') !== 'error') return;
      if (!photoOf(d)) return;
      state.dir = d;
      state.zoom = zoomFirst;
      syncControls();
      writeHash();
      show();
    }

    function focusables() {
      return Array.prototype.filter.call(dialog.querySelectorAll('button, select, a[href], [tabindex]:not([tabindex="-1"])'),
        function (e) { return !e.disabled && !e.hidden && e.offsetParent !== null; });
    }

    /* Capture phase, so the page's own Escape (node page back, Map closes
     * the profile) never sees the one that closes the viewer. */
    function onKey(e) {
      if (gone) return;
      if (e.key === 'Escape' || e.key === 'Esc') {
        e.preventDefault();
        e.stopImmediatePropagation();
        api.close(true);
        return;
      }
      if (e.key !== 'Tab') return;
      var f = focusables();
      if (!f.length) { e.preventDefault(); dialog.focus(); return; }
      var first = f[0], last = f[f.length - 1], a = doc.activeElement;
      if (!dialog.contains(a)) { e.preventDefault(); first.focus(); }
      else if (e.shiftKey && (a === first || a === dialog)) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && a === last) { e.preventDefault(); first.focus(); }
    }

    var api = {
      el: function () { return dialog; },
      state: function () { return { lat: at.lat, lon: at.lon, dir: state.dir, year: state.year }; },
      /* close(true) is the viewer's own Close or Escape: focus goes back
       * to the opener and the hash drops the viewer's key. close(false)
       * is a page change or another viewer taking over. */
      close: function (byUser) {
        if (gone) return;
        gone = true;
        seq++;
        pseq++;
        if (previewAbort) previewAbort.abort();
        if (searchAbort) searchAbort.abort();
        doc.removeEventListener('keydown', onKey, true);
        if (resizer) resizer.disconnect();
        drawn = null;
        if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
        ui.img.removeAttribute('src');
        if (shown && !isKept(shown)) revoke(shown);
        shown = null;
        kept.forEach(function (k) { revoke(k.objectURL); });
        kept = [];
        if (current === api) current = null;
        if (byUser) {
          writeHash();
          var back = opener && opener.isConnected ? opener
            : (openerId ? doc.querySelector('[data-testid="' + openerId + '"]') : null);
          if (back && typeof back.focus === 'function') back.focus();
        }
      }
    };

    current = api;
    doc.addEventListener('keydown', onKey, true);
    doc.body.appendChild(overlay);
    closeBtn.focus();
    search();
    return api;
  }

  /* actionButton is an "Oblique photo" button; getPoint() is asked at
   * click time for { lat, lon, label, line } (null: nothing to show; line,
   * from a profile, the A→B drawn on the photo). */
  function actionButton(o) {
    var b = el('button', o.cls || 'btn-primary');
    b.type = 'button';
    if (o.testid) b.setAttribute('data-testid', o.testid);
    if (o.title) b.title = o.title;
    if (o.icon !== false) b.appendChild(icon(true));
    b.appendChild(el('span', 'sf-action-text', o.text || 'Oblique photo'));
    b.addEventListener('click', function () {
      var p = o.getPoint ? o.getPoint() : null;
      if (!p) return;
      open({ lat: p.lat, lon: p.lon, label: p.label, line: p.line, opener: b, cfg: o.cfg });
    });
    return b;
  }

  /* nodeAction is the node-detail button, or null when the photos are not
   * offered, the node has no fix or its fix is outside the block's bounds
   * (there the search always answers empty). Asks nothing until it is
   * clicked. */
  function nodeAction(node, cfg) {
    var at = nodePoint(node);
    if (!offered(cfg) || !at || !covered(cfg, at)) return null;
    var name = node.name || node.advert_name || '';
    return actionButton({
      cls: 'btn-primary node-terrain-action sf-node-action', testid: 'node-skraafoto', cfg: cfg,
      title: 'Oblique aerial photos of this node\'s position, from four sides and straight down',
      getPoint: function () { return { lat: at.lat, lon: at.lon, label: name ? String(name) : null }; }
    });
  }

  /* fromHash opens, follows or closes the viewer for the hash's key. */
  function fromHash() {
    var cfg = global.MC_SKRAAFOTO_CFG;
    var link = null;
    try { link = linkFromHash(global.location && global.location.hash); } catch (_) {}
    if (!link || !offered(cfg)) { if (current) current.close(false); return; }
    if (current) {
      var s = current.state();
      if (s.lat === link.lat && s.lon === link.lon && (!link.dir || s.dir === link.dir) && (!link.year || s.year === link.year)) return;
    }
    open({ lat: link.lat, lon: link.lon, dir: link.dir, year: link.year, cfg: cfg });
  }

  /* customizerReady resolves once the customizer has applied this
   * browser's overrides to window.SITE_CONFIG (its pipeline ends with
   * theme-changed, then initDone), so a cold deep link opens with the
   * viewer's defaults (#737 customizer). At once without the customizer;
   * after CUSTOMIZER_WAIT_MS at the latest, so a theme config that never
   * comes cannot keep the link from opening. */
  var CUSTOMIZER_WAIT_MS = 4000;
  function customizerReady() {
    var c = global._customizerV2;
    if (!c || c.initDone || !global.addEventListener) return global.Promise.resolve();
    return new global.Promise(function (resolve) {
      var timer = null;
      function done() {
        global.removeEventListener('theme-changed', onTheme);
        if (timer !== null) global.clearTimeout(timer);
        resolve();
      }
      // initDone is set right after the pipeline's theme-changed.
      function onTheme() { global.setTimeout(function () { if (c.initDone) done(); }, 0); }
      global.addEventListener('theme-changed', onTheme);
      timer = global.setTimeout(done, CUSTOMIZER_WAIT_MS);
    });
  }

  if (global.addEventListener) global.addEventListener('hashchange', fromHash);
  if (global.Promise) global.Promise.resolve(global.MeshConfigReady).catch(function () {}).then(customizerReady).then(fromHash);

  global.Skraafoto = {
    DIRECTIONS: DIRECTIONS,
    // #737 customizer — the viewer defaults' one rule, shared with
    // public/customize-v2.js (section `skraafoto`).
    SETTINGS: {
      KEYS: SETTING_KEYS, DEFAULTS: SETTING_DEFAULTS,
      DIRECTIONS: DIRECTION_SETTINGS, ZOOMS: ZOOM_SETTINGS, LABELS: SETTING_LABELS,
      valid: validSetting, resolve: settings
    },
    MIN_YEAR: MIN_YEAR,
    HASH_PARAM: HASH_PARAM,
    offered: offered,
    covered: covered,
    nodePoint: nodePoint,
    searchURL: searchURL,
    previewURL: previewURL,
    viewerLink: viewerLink,
    orderPhotos: orderPhotos,
    pickDirection: pickDirection,
    yearChoices: yearChoices,
    selectedYear: selectedYear,
    photoDate: photoDate,
    errorText: errorText,
    formatLink: formatLink,
    parseLink: parseLink,
    withParam: withParam,
    linkFromHash: linkFromHash,
    creditLinks: creditLinks,
    displayMap: displayMap,
    toDisplay: toDisplay,
    parseMarker: parseMarker,
    parseCropBox: parseCropBox,
    toCrop: toCrop,
    marks: marks,
    zoomURL: zoomURL,
    sightLine: sightLine,
    perpendicularDirection: perpendicularDirection,
    placeWords: placeWords,
    markText: markText,
    open: open,
    actionButton: actionButton,
    nodeAction: nodeAction,
    current: function () { return current; }
  };
})(typeof window !== 'undefined' ? window : this);
