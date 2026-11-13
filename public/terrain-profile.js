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

  function boundedNumber(v, lo, hi, places) {
    if (v === null || v === undefined || v === '' || typeof v === 'object' || typeof v === 'boolean') return null;
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
  function parseState(input) {
    var p = toParams(input);
    if (!p) return null;
    var refs = parseProfilePair(p.get(HASH_PARAM));
    if (!refs) return null;
    return {
      a: refs[0],
      b: refs[1],
      model: p.get('model') === 'dtm' ? 'dtm' : 'both',
      ha: clampHeight(p.get('ha')),
      hb: clampHeight(p.get('hb')),
      k: boundedNumber(p.get('k'), 0.5, 10, 4),
      freq: boundedNumber(p.get('freq'), 150, 2500, 4)
    };
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
  function serializeState(state) {
    var out = {};
    if (!state || !state.a || !state.b) return out;
    out[HASH_PARAM] = refToParam(state.a) + ',' + refToParam(state.b);
    if (state.model === 'dtm') out.model = 'dtm';
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
    var ser = serializeState(state);
    if (ser.model) p.set('model', ser.model);
    ['ha', 'hb', 'k', 'freq'].forEach(function (key) { if (ser[key] !== undefined) p.set(key, ser[key]); });
    return ENDPOINT + '?' + p.toString();
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

  function fmtPct(v) {
    var n = Number(v);
    return isFinite(n) ? String(round(n, 1)) : '0';
  }

  /* verdictChip maps the server's verdict to the chip. Anything the server
   * may add later lands on "No verdict" rather than on a confident tone:
   * guessing green on an unknown state is the one harmful failure here. */
  function verdictChip(data) {
    var v = (data && data.verdict) || {};
    var cov = (data && data.coverage) || {};
    var known = Object.prototype.hasOwnProperty.call(VERDICTS, v.state) ? VERDICTS[v.state] : null;
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
     * the server's criterion fraction of the Fresnel radius. Only drawn
     * when the server actually reached a verdict — a profile with a gap is
     * unknown, and marking "obstructions" on it would invent a judgement. */
    var intrusions = [];
    var verdict = data.verdict || {};
    var criterion = Number(verdict.criterion);
    if (!isFinite(criterion)) criterion = 0.6;
    if (verdict.state && verdict.state !== 'unknown') {
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
      return '<svg viewBox="0 0 ' + VIEW.w + ' ' + VIEW.h + '" class="tp-svg" role="img" aria-label="No terrain profile">'
        + '<title>' + esc(chip.detail) + '</title>'
        + '<text x="' + (VIEW.w / 2) + '" y="' + (VIEW.h / 2) + '" text-anchor="middle" font-size="14" fill="var(--text-muted)">'
        + esc(chip.detail) + '</text></svg>';
    }
    var p = g.plot;
    var out = ['<svg viewBox="0 0 ' + VIEW.w + ' ' + VIEW.h + '" class="tp-svg" preserveAspectRatio="none" role="img" aria-label="Terrain profile chart">'];
    out.push('<title>' + esc(chartTitle(data, chip, names)) + '</title>');

    // Horizontal grid + elevation ticks.
    for (var t = 0; t <= 4; t++) {
      var z = g.domain.zMax - (g.domain.zMax - g.domain.zMin) * t / 4;
      var y = r2(p.y + p.h * t / 4);
      out.push('<line x1="' + p.x + '" y1="' + y + '" x2="' + (p.x + p.w) + '" y2="' + y + '" stroke="var(--border)" stroke-width="0.5" stroke-dasharray="2 3"/>');
      out.push('<text x="' + (p.x - 5) + '" y="' + (y + 3) + '" text-anchor="end" font-size="9" fill="var(--text-muted)">' + Math.round(z) + '</text>');
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
      out.push('<line x1="' + wx + '" y1="' + p.y + '" x2="' + wx + '" y2="' + g.baseY + '" stroke="var(--profile-blocked)" stroke-width="1" stroke-dasharray="3 3"/>');
      out.push('<circle cx="' + wx + '" cy="' + g.losY[0] + '" r="0"/>');
    }

    // 7. Out-of-coverage stretches: left blank, labelled once.
    g.gaps.forEach(function (gp) {
      var mid = r2((gp.x1 + gp.x2) / 2);
      out.push('<rect x="' + gp.x1 + '" y="' + p.y + '" width="' + Math.max(2, r2(gp.x2 - gp.x1)) + '" height="' + p.h + '" fill="none" stroke="var(--border)" stroke-width="0.8" stroke-dasharray="3 4"/>');
      out.push('<text x="' + mid + '" y="' + r2(g.baseY - 8) + '" text-anchor="middle" font-size="9" fill="var(--text-muted)">no data</text>');
    });

    // 8. Endpoint pins: the mast from ground to antenna, with the label.
    [{ x: g.x[0], y: g.losY[0], g: g.terrainY[0], label: 'A', anchor: 'start', dx: 3 },
     { x: g.x[g.x.length - 1], y: g.losY[g.losY.length - 1], g: g.terrainY[g.terrainY.length - 1], label: 'B', anchor: 'end', dx: -3 }]
      .forEach(function (pin) {
        var foot = pin.g === null ? g.baseY : pin.g;
        out.push('<line x1="' + pin.x + '" y1="' + foot + '" x2="' + pin.x + '" y2="' + pin.y + '" stroke="var(--los-line)" stroke-width="1.6"/>');
        out.push('<circle cx="' + pin.x + '" cy="' + pin.y + '" r="3" fill="var(--los-line)"/>');
        out.push('<text x="' + r2(pin.x + pin.dx) + '" y="' + r2(pin.y - 7) + '" text-anchor="' + pin.anchor + '" font-size="11" font-weight="600" fill="var(--text)">' + pin.label + '</text>');
      });

    // Distance axis and the required CC BY 4.0 credit.
    out.push('<text x="' + p.x + '" y="' + (VIEW.h - 14) + '" font-size="9" fill="var(--text-muted)">0</text>');
    out.push('<text x="' + (p.x + p.w) + '" y="' + (VIEW.h - 14) + '" text-anchor="end" font-size="9" fill="var(--text-muted)">' + esc(fmtMetres(data.distanceM)) + '</text>');
    out.push('<text x="' + (VIEW.w / 2) + '" y="' + (VIEW.h - 3) + '" text-anchor="middle" font-size="8" fill="var(--text-muted)" class="tp-credit">' + esc(data.attribution || ATTRIBUTION) + '</text>');
    out.push('</svg>');
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

    var state = { a: null, b: null, model: 'both', ha: null, hb: null, k: null, freq: null };
    var open = false, seq = 0, debounceTimer = null, panel = null, nodes = { a: null, b: null };
    var ui = {};

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

      var body = el('div', 'tp-body');
      body.setAttribute('data-testid', 'tp-body');
      ui.chart = el('div', 'tp-chart');
      ui.chart.setAttribute('data-testid', 'tp-chart');
      body.appendChild(ui.chart);

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

      ui.credit = el('p', 'tp-credit-line', ATTRIBUTION);
      body.appendChild(ui.credit);
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

      var source = el('span', 'tp-assumed');
      source.setAttribute('data-testid', 'tp-assumed-' + side);
      wrap.appendChild(source);

      function commit(v) {
        var h = clampHeight(v);
        if (h === null) return;
        input.value = String(h);
        state[side === 'a' ? 'ha' : 'hb'] = h;
        rememberHeight(state[side], h);
        scheduleRequest();
      }
      minus.addEventListener('click', function () { commit((clampHeight(input.value) || 0) - 1); });
      plus.addEventListener('click', function () { commit((clampHeight(input.value) || 0) + 1); });
      input.addEventListener('input', function () { commit(input.value); });
      input.addEventListener('change', function () { commit(input.value); });
      return { wrap: wrap, name: name, input: input, source: source };
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
          var ser = serializeState(state);
          Object.keys(ser).forEach(function (key) { params.set(key, ser[key]); });
        }
        var qs = params.toString();
        var next = global.location.pathname + global.location.search + (qs ? (base || '#') + '?' + qs : base);
        global.history.replaceState(global.history.state, '', next);
      } catch (_) { /* a hash we cannot write is not worth breaking the tool for */ }
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
      // The credit lives in the chart footer; the paragraph is its stand-in
      // when there is no chart, so it is shown exactly once either way.
      ui.credit.hidden = drawn;
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
        if (h !== null && doc.activeElement !== c.input) c.input.value = String(h);
        if (h !== null) state[side === 'a' ? 'ha' : 'hb'] = h;
        var src = SOURCE_LABELS[end.antennaSource] || '';
        c.source.textContent = end.assumed ? ('Assumed — ' + src) : src;
        c.source.classList.toggle('is-assumed', !!end.assumed);
      });
    }

    /* request issues exactly one call per profile. A newer call wins: an
     * older response is dropped rather than racing into the panel. */
    function request() {
      if (!open || !state.a || !state.b) return;
      if (debounceTimer) { global.clearTimeout(debounceTimer); debounceTimer = null; }
      renderNames();
      writeHash();
      var mine = ++seq;
      setStatus('Measuring the terrain along the path…', 'tp-loading');
      setChip({ state: 'pending', tone: 'unknown', label: '…', detail: 'Waiting for the terrain profile.' });
      return fetchJSON(requestURL(state)).then(function (data) {
        if (mine !== seq || !open) return;
        render(data);
      }).catch(function (err) {
        if (mine !== seq || !open) return;
        setChip({ state: 'error', tone: 'unknown', label: 'No profile', detail: String(err && err.message || err) });
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
        state.model = next.model === 'dtm' ? 'dtm' : 'both';
        state.ha = next.ha === undefined ? null : next.ha;
        state.hb = next.hb === undefined ? null : next.hb;
        state.k = next.k === undefined ? null : next.k;
        state.freq = next.freq === undefined ? null : next.freq;
        if (state.ha === null) state.ha = recallHeight(state.a);
        if (state.hb === null) state.hb = recallHeight(state.b);
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
        if (panel) panel.classList.remove('is-open');
        writeHash();
        if (opts.onClose) opts.onClose();
      },
      destroy: function () {
        api.close();
        if (panel && panel.parentNode) panel.parentNode.removeChild(panel);
        panel = null;
        ui = {};
      }
    };
    return api;
  }

  global.TerrainProfile = {
    ATTRIBUTION: ATTRIBUTION,
    ENDPOINT: ENDPOINT,
    HASH_PARAM: HASH_PARAM,
    MAX_HEIGHT_M: MAX_HEIGHT_M,
    parseState: parseState,
    serializeState: serializeState,
    requestURL: requestURL,
    clampHeight: clampHeight,
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
