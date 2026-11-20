/* #941 M1 — Predicted reach: where a radio at a node or a map point is
 * predicted to be heard, as a coloured layer on the Map page.
 *
 * Self-contained, like terrain-profile.js: map.js passes in the map, a
 * mount point and a node resolver, and gets back a controller that owns
 * the toggle button, the panel, the requests to GET /api/terrain/coverage
 * and its raster, the canvas overlay and the #/map?reach=… deep link.
 *
 * The server computes the path loss in dB (cmd/server/terrain_coverage.go)
 * and sends it as one byte per pixel. Everything that only shifts the
 * margin — transmit power, both antenna gains, cable loss, sensitivity —
 * is applied here, through a 256-entry lookup table built from CSS
 * variables, so moving those sliders recolours the same pixels without a
 * request. Heights, radius and frequency change the loss and ask again.
 *
 * Nothing is fetched until the tool is armed or a link opens it. All DOM is
 * built with createElement/textContent: no server or node field ever passes
 * through an HTML string.
 */
(function (global) {
  'use strict';

  var ENDPOINT = '/api/terrain/coverage';
  var HASH_PARAM = 'reach';
  // The other keys of a reach link. They are namespaced (r…) because the
  // profile's link already uses ha and freq on the same #/map hash.
  var KEYS = {
    tx: 'rtx', rx: 'rrx', radius: 'rkm', freq: 'rmhz',
    power: 'rpwr', txGain: 'rgtx', rxGain: 'rgrx', loss: 'rloss', sens: 'rsens'
  };
  // [min, max, decimals]. Request bounds mirror cmd/server's grammar;
  // budget bounds mirror resolveCoverageRadio.
  var BOUNDS = {
    tx: [0, 100, 1], rx: [0, 100, 1], radius: [1, 60, 1], freq: [150, 2500, 3],
    power: [-10, 30, 1], txGain: [-10, 20, 1], rxGain: [-10, 20, 1], loss: [0, 30, 1], sens: [-150, -80, 1]
  };
  var REQUEST_FIELDS = ['tx', 'rx', 'radius', 'freq'];
  var BUDGET_FIELDS = ['power', 'txGain', 'rxGain', 'loss', 'sens'];
  var RADIO_KEYS = { power: 'txPowerDbm', txGain: 'antennaGainDbi', rxGain: 'rxGainDbi', loss: 'lossDb', sens: 'sensitivityDbm' };
  var DEFAULT_RADIO = { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 };

  // The margin classes of M0 §3.5 (owner decision 7), in dB, best first.
  // Their colours are the CSS variables; the customizer gets them in M4.
  var CLASSES = [
    { id: 'good', min: 10, label: 'Good', range: '≥ 10 dB', cssVar: '--reach-good' },
    { id: 'marginal', min: 3, label: 'Marginal', range: '3–10 dB', cssVar: '--reach-marginal' },
    { id: 'weak', min: 0, label: 'Weak', range: '0–3 dB', cssVar: '--reach-weak' }
  ];
  var REQUEST_DEBOUNCE_MS = 500;
  var HASH_DEBOUNCE_MS = 250;
  var OPACITY_KEY = 'mc-reach-opacity';
  var DEFAULT_OPACITY = 0.6;
  var MAX_PARAM = 200;

  // The profile's grammars (terrain-profile.js; the server mirrors them).
  var PUBKEY_RE = /^[0-9a-f]{64}$/;
  var COORD_RE = /^-?[0-9]{1,3}(\.[0-9]{1,10})?$/;
  var DECIMAL_RE = /^-?[0-9]{1,6}(\.[0-9]{1,10})?$/;

  function round(v, places) { var f = Math.pow(10, places); return Math.round(v * f) / f; }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = String(text);
    return e;
  }

  // ── Deep link ────────────────────────────────────────────────────────────

  /* parseAt reads the centre: a 64-hex public key or "lat,lon". */
  function parseAt(s) {
    if (typeof s !== 'string' || !s || s.length > MAX_PARAM) return null;
    var pk = s.toLowerCase();
    if (PUBKEY_RE.test(pk)) return { kind: 'node', pubkey: pk };
    var parts = s.split(',');
    if (parts.length !== 2 || !COORD_RE.test(parts[0]) || !COORD_RE.test(parts[1])) return null;
    var lat = Number(parts[0]), lon = Number(parts[1]);
    if (!(lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180)) return null;
    return { kind: 'point', lat: round(lat, 5), lon: round(lon, 5) };
  }

  function atString(at) {
    return at.kind === 'node' ? at.pubkey : round(at.lat, 5) + ',' + round(at.lon, 5);
  }

  /* boundedNumber is a field's value from a link or an input: the plain
   * decimal grammar, inside its bounds, rounded; null otherwise. */
  function boundedNumber(field, v) {
    var b = BOUNDS[field];
    if (!b || v === null || v === undefined || v === '' || typeof v === 'object' || typeof v === 'boolean') return null;
    if (typeof v === 'string' && !DECIMAL_RE.test(v)) return null;
    var n = Number(v);
    if (!isFinite(n) || n < b[0] || n > b[1]) return null;
    return round(n, b[2]);
  }

  /* parseState reads a #/map query string. null unless it names a valid
   * centre; an invalid optional value is dropped, the rest is kept. */
  function parseState(qs) {
    var params = new URLSearchParams(String(qs || ''));
    var at = parseAt(params.get(HASH_PARAM));
    if (!at) return null;
    var st = { at: at };
    Object.keys(KEYS).forEach(function (f) {
      var v = boundedNumber(f, params.get(KEYS[f]));
      if (v !== null) st[f] = v;
    });
    return st;
  }

  /* invalidLink: the query has a reach parameter that cannot be read. */
  function invalidLink(qs) {
    var params = new URLSearchParams(String(qs || ''));
    return params.has(HASH_PARAM) && !parseState(qs);
  }

  /* serializeState is the link's parameters, in a fixed order; a field the
   * state does not set stays out, so the server's default applies. */
  function serializeState(st) {
    var out = {};
    if (!st || !st.at) return out;
    out[HASH_PARAM] = atString(st.at);
    Object.keys(KEYS).forEach(function (f) {
      if (typeof st[f] === 'number' && isFinite(st[f])) out[KEYS[f]] = String(st[f]);
    });
    return out;
  }

  /* withReachParams replaces the reach parameters of a hash, keeping the
   * others (the view, the base map, …). */
  function withReachParams(hash, ser) {
    hash = String(hash || '');
    var qi = hash.indexOf('?');
    var base = qi >= 0 ? hash.slice(0, qi) : hash;
    var params = new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '');
    params.delete(HASH_PARAM);
    Object.keys(KEYS).forEach(function (f) { params.delete(KEYS[f]); });
    Object.keys(ser || {}).forEach(function (k) { params.set(k, ser[k]); });
    var qs = params.toString();
    return qs ? (base || '#') + '?' + qs : base;
  }

  /* requestURL is the computation's request: the centre and the fields
   * that change the loss. The budget never reaches the server. */
  function requestURL(st) {
    var q = 'at=' + encodeURIComponent(atString(st.at));
    var server = { tx: 'ha', rx: 'hr', radius: 'r', freq: 'freq' };
    REQUEST_FIELDS.forEach(function (f) {
      if (typeof st[f] === 'number') q += '&' + server[f] + '=' + encodeURIComponent(String(st[f]));
    });
    return ENDPOINT + '?' + q;
  }

  // ── Link budget and colours ──────────────────────────────────────────────

  /* budgetOf is the maximum path loss the link tolerates, in dB: power +
   * both gains − cable loss − sensitivity, each from the state, else the
   * operator's radio block, else the shipped defaults. */
  function budgetOf(st, radio) {
    var v = budgetValues(st, radio);
    return v.power + v.txGain + v.rxGain - v.loss - v.sens;
  }

  function budgetValues(st, radio) {
    var out = {};
    BUDGET_FIELDS.forEach(function (f) {
      var r = radio && typeof radio[RADIO_KEYS[f]] === 'number' ? radio[RADIO_KEYS[f]] : DEFAULT_RADIO[RADIO_KEYS[f]];
      out[f] = st && typeof st[f] === 'number' ? st[f] : r;
    });
    return out;
  }

  /* marginClass is the class of a margin in dB; a boundary belongs to the
   * better class. 'none' below 0 dB. */
  function marginClass(m) {
    if (!(typeof m === 'number' && isFinite(m))) return 'none';
    for (var i = 0; i < CLASSES.length; i++) if (m >= CLASSES[i].min) return CLASSES[i].id;
    return 'none';
  }

  /* parseColor reads a computed CSS colour: #rgb, #rrggbb or rgb()/rgba().
   * null for anything else. */
  function parseColor(s) {
    s = String(s || '').trim();
    var m = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(s);
    if (m) {
      var h = m[1];
      if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
      return [parseInt(h.slice(0, 2), 16), parseInt(h.slice(2, 4), 16), parseInt(h.slice(4, 6), 16)];
    }
    m = /^rgba?\(\s*([0-9.]+)[\s,]+([0-9.]+)[\s,]+([0-9.]+)/i.exec(s);
    if (m) return [Math.round(Number(m[1])), Math.round(Number(m[2])), Math.round(Number(m[3]))];
    return null;
  }

  /* themeColors reads the class colours from the CSS variables on the
   * page (getVar defaults to the document's computed style). */
  function themeColors(getVar) {
    getVar = getVar || function (name) {
      return global.getComputedStyle(global.document.documentElement).getPropertyValue(name);
    };
    var out = {};
    CLASSES.forEach(function (c) { out[c.id] = parseColor(getVar(c.cssVar)); });
    return out;
  }

  /* byteClass is the class of raster byte v at a budget: byte v is the loss
   * meta.lossBaseDb + v·meta.lossStepDb, its margin budget − loss. null for
   * meta.noData (no prediction). The byte below noData is every loss at or
   * above its value (the server clamps there), so its margin is unknown and
   * it is 'none' at every budget. */
  function byteClass(v, budget, meta) {
    var base = Number(meta && meta.lossBaseDb), step = Number(meta && meta.lossStepDb);
    var noData = meta && typeof meta.noData === 'number' ? meta.noData : 255;
    if (v === noData) return null;
    if (v === noData - 1 || !isFinite(base) || !isFinite(step)) return 'none';
    return marginClass(budget - (base + v * step));
  }

  /* buildLut is the 256-entry RGBA table of a raster, coloured by
   * byteClass; no reach, no data and the clamped byte are transparent. */
  function buildLut(budget, meta, colors) {
    var lut = new Uint8ClampedArray(256 * 4);
    for (var v = 0; v < 256; v++) {
      var c = colors && colors[byteClass(v, budget, meta)];
      if (!c) continue;
      lut[4 * v] = c[0];
      lut[4 * v + 1] = c[1];
      lut[4 * v + 2] = c[2];
      lut[4 * v + 3] = 255;
    }
    return lut;
  }

  /* paint fills an RGBA pixel buffer from the raster through the table,
   * four bytes at a time. */
  function paint(rgba, raster, lut) {
    var out = new Uint32Array(rgba.buffer, rgba.byteOffset, raster.length);
    var table = new Uint32Array(lut.buffer, lut.byteOffset, 256);
    for (var i = 0; i < raster.length; i++) out[i] = table[raster[i]];
    return rgba;
  }

  /* classShares counts the raster's pixels per class at a budget, for the
   * legend's percentages: one pass, a 256-entry histogram. */
  function classShares(raster, budget, meta) {
    var hist = new Uint32Array(256);
    for (var i = 0; i < raster.length; i++) hist[raster[i]]++;
    var out = { good: 0, marginal: 0, weak: 0, none: 0, total: 0 };
    for (var v = 0; v < 256; v++) {
      var cls = hist[v] ? byteClass(v, budget, meta) : null;
      if (!cls) continue;
      out[cls] += hist[v];
      out.total += hist[v];
    }
    return out;
  }

  // ── Gates ────────────────────────────────────────────────────────────────

  function offered(cfg) { return !!(cfg && cfg.enabled === true); }

  /* hasFix: both coordinates present, in range, and not the 0,0 no-lock
   * sentinel (the node-detail rule, terrain-profile.js). */
  function hasFix(node) {
    if (!node || node.lat === null || node.lat === undefined || node.lat === ''
      || node.lon === null || node.lon === undefined || node.lon === '') return false;
    var lat = Number(node.lat), lon = Number(node.lon);
    if (!isFinite(lat) || !isFinite(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180) return false;
    return !(lat === 0 && lon === 0);
  }

  /* nodeEntryHash is node detail's link: the Map page with the reach of
   * this node. null means "offer nothing". */
  function nodeEntryHash(node, cfg) {
    if (!offered(cfg) || !hasFix(node)) return null;
    var pk = String(node.public_key || '').toLowerCase();
    if (!PUBKEY_RE.test(pk)) return null;
    return '#/map?' + HASH_PARAM + '=' + pk;
  }

  /* nodeEntryAction is the node-detail chip, built as DOM. */
  function nodeEntryAction(node, cfg) {
    var href = nodeEntryHash(node, cfg);
    if (!href) return null;
    var a = el('a', 'btn-primary node-action-chip');
    a.setAttribute('href', href);
    a.setAttribute('data-testid', 'node-predicted-reach');
    a.setAttribute('title', 'Predicted reach: where this node is predicted to be heard, from the terrain. A prediction, not a measurement.');
    var icon = el('span', 'node-terrain-icon');
    icon.setAttribute('aria-hidden', 'true');
    // Static markup, no interpolation: the Phosphor sprite icon.
    icon.innerHTML = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-broadcast"/></svg>';
    a.appendChild(icon);
    a.appendChild(el('span', null, 'Predicted reach'));
    return a;
  }

  // ── Messages ─────────────────────────────────────────────────────────────

  /* refusal is the panel's words for a failed request: a status and the
   * server's fixed error. Nothing from the body is shown verbatim except
   * the radius limit, as a number. */
  function refusal(status, body, retryAfter, cfg) {
    var err = body && typeof body.error === 'string' ? body.error : '';
    var wait = Number(retryAfter);
    var later = isFinite(wait) && wait > 0 ? ' Try again in ' + Math.ceil(wait) + ' s.' : ' Try again shortly.';
    if (status === 400 && err === 'radius too large') {
      var lim = Number(body.maxRadiusKm);
      return 'The radius is limited to ' + (isFinite(lim) ? lim : '?') + ' km on this server.';
    }
    if (status === 400 && err === 'point not covered') {
      return cfg && cfg.fill ? 'No terrain data there. Pick a point in Denmark or near it.' : 'No terrain data there. Pick a point in Denmark.';
    }
    if (status === 404) return 'That node is unknown, hidden or has no position.';
    if (status === 429) return 'Too many new predictions from here.' + later;
    if (status === 503) return 'The server is busy predicting another reach.' + later;
    if (status === 504) return 'The terrain data timed out. The tiles that arrived are kept, so trying again is faster.';
    if (status === 502) return 'The terrain data is unavailable right now.';
    return 'The prediction failed.';
  }

  function fmt(v, places) { return Number(v).toFixed(places); }

  /* fillShortCredit is the Copernicus credit line of a reach that used
   * the fill. The server's short credit says "Outside Denmark"; without
   * DHM the fill is the terrain everywhere, so it names the dataset and
   * its licence alone (the marks the credit walker links). */
  function fillShortCredit(cfg) {
    var fc = cfg && cfg.fill;
    if (!fc) return '';
    return cfg.dhm ? String(fc.credit || '') : 'Terrain: ' + fc.datasetName + ' (' + fc.licenseName + ')';
  }

  // ── Controller ───────────────────────────────────────────────────────────

  var CanvasOverlay = null;

  /* canvasOverlay is an L.imageOverlay drawn on a canvas, so recolouring
   * is a putImageData on the same element: no encode, no request. */
  function canvasOverlay(L, canvas, bounds, opts) {
    if (!CanvasOverlay) {
      CanvasOverlay = L.ImageOverlay.extend({
        _initImage: function () {
          var img = this._image = this._url;
          L.DomUtil.addClass(img, 'leaflet-image-layer');
          if (this._zoomAnimated) L.DomUtil.addClass(img, 'leaflet-zoom-animated');
          if (this.options.className) L.DomUtil.addClass(img, this.options.className);
          img.onselectstart = L.Util.falseFn;
          img.onmousemove = L.Util.falseFn;
        }
      });
    }
    return new CanvasOverlay(canvas, bounds, opts);
  }

  function storage() {
    try { return global.localStorage || null; } catch (_) { return null; }
  }

  /* create builds the tool. opts: map, mount (the map's wrapper), cfg (the
   * client config's coverage block), dhm (its dhm block, for the credit),
   * resolveNode(pubkey) → {name, lat, lon}, fetchFn (tests), onArm and
   * onOpen (the host puts its other map tools away), linkMove(move) (the
   * host runs a link's fit without saving it as the viewer's view). */
  function create(opts) {
    var map = opts.map, L = global.L, cfg = opts.cfg || {};
    var fetchFn = opts.fetchFn || (global.fetch ? global.fetch.bind(global) : null);
    var state = null, armed = false, panel = null, ui = {}, btn = null, hint = null;
    var overlay = null, canvas = null, centreDot = null, raster = null, meta = null, data = null;
    var generation = 0, aborter = null, requestTimer = null, hashTimer = null, fitPending = false;
    var opacity = DEFAULT_OPACITY;
    try {
      var saved = Number(storage() && storage().getItem(OPACITY_KEY));
      if (saved >= 0.1 && saved <= 1) opacity = saved;
    } catch (_) { /* the default stands */ }

    function buildToggle() {
      if (!opts.mount || btn) return;
      btn = el('button', 'map-reach-toggle');
      btn.type = 'button';
      btn.setAttribute('data-testid', 'map-reach-toggle');
      btn.setAttribute('aria-pressed', 'false');
      btn.setAttribute('aria-label', 'Predicted reach');
      btn.title = 'Predicted reach — where a radio here is predicted to be heard';
      // Static markup, no interpolation.
      btn.innerHTML = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-broadcast"/></svg>';
      btn.addEventListener('click', function () { if (armed) disarm(); else arm(); });
      opts.mount.appendChild(btn);
      hint = el('div', 'map-profile-hint reach-hint');
      hint.setAttribute('data-testid', 'reach-hint');
      hint.setAttribute('role', 'status');
      hint.hidden = true;
      opts.mount.appendChild(hint);
    }

    function syncButton() {
      if (!btn) return;
      btn.classList.toggle('active', armed);
      btn.setAttribute('aria-pressed', armed ? 'true' : 'false');
    }

    function showHint(text, isError) {
      if (!hint) return;
      hint.textContent = text;
      hint.classList.toggle('is-error', !!isError);
      hint.hidden = false;
    }

    function arm() {
      if (armed || !map) return;
      if (opts.onArm) opts.onArm();
      armed = true;
      map.getContainer().classList.add('reach-picking');
      map.on('click', onMapClick);
      syncButton();
      showHint('Click the map where the radio is. Escape cancels.');
    }

    function disarm() {
      if (!armed) return;
      armed = false;
      if (map) {
        map.off('click', onMapClick);
        map.getContainer().classList.remove('reach-picking');
      }
      if (hint) hint.hidden = true;
      syncButton();
    }

    function onMapClick(e) {
      if (!armed || !e || !e.latlng) return;
      disarm();
      var at = parseAt(round(e.latlng.lat, 5) + ',' + round(e.latlng.lng, 5));
      if (at) open(Object.assign({}, state || {}, { at: at }));
    }

    function onKey(e) {
      if (e.key !== 'Escape') return;
      if (armed) disarm();
    }

    // ── Panel ──

    function field(parent, f, label, unit, kind, testid) {
      var row = el('label', 'reach-field');
      row.appendChild(el('span', 'reach-field-label', label));
      var input = el('input', 'reach-input');
      var b = BOUNDS[f];
      input.type = kind;
      input.min = String(f === 'radius' ? 1 : b[0]);
      input.max = String(f === 'radius' ? (cfg.maxRadiusKm || b[1]) : b[1]);
      input.step = f === 'freq' ? '0.001' : (kind === 'range' ? '1' : '0.5');
      input.setAttribute('data-testid', testid);
      input.setAttribute('aria-label', label + (unit ? ' (' + unit + ')' : ''));
      row.appendChild(input);
      var out = el('span', 'reach-field-value');
      out.setAttribute('data-testid', testid + '-value');
      row.appendChild(out);
      parent.appendChild(row);
      ui[f] = input;
      ui[f + 'Out'] = out;
      ui[f + 'Unit'] = unit;
      return input;
    }

    function buildPanel() {
      if (panel) return;
      panel = el('section', 'reach-panel');
      panel.setAttribute('data-testid', 'reach-panel');
      panel.setAttribute('aria-label', 'Predicted reach');
      var head = el('div', 'reach-head');
      head.appendChild(el('h2', 'reach-title', 'Predicted reach'));
      var close = el('button', 'reach-close', '×');
      close.type = 'button';
      close.setAttribute('aria-label', 'Close predicted reach');
      close.setAttribute('data-testid', 'reach-close');
      close.addEventListener('click', function () { closePanel(); });
      head.appendChild(close);
      panel.appendChild(head);

      ui.centre = el('p', 'reach-centre');
      ui.centre.setAttribute('data-testid', 'reach-centre');
      panel.appendChild(ui.centre);
      ui.status = el('p', 'reach-status');
      ui.status.setAttribute('data-testid', 'reach-status');
      ui.status.setAttribute('role', 'status');
      panel.appendChild(ui.status);

      ui.legend = el('div', 'reach-legend');
      ui.legend.setAttribute('data-testid', 'reach-legend');
      CLASSES.forEach(function (c) {
        var row = el('div', 'reach-legend-row');
        row.setAttribute('data-class', c.id);
        var sw = el('span', 'reach-swatch reach-swatch-' + c.id);
        sw.setAttribute('aria-hidden', 'true');
        row.appendChild(sw);
        row.appendChild(el('span', 'reach-legend-label', c.label + ' ' + c.range));
        var pct = el('span', 'reach-legend-pct');
        pct.setAttribute('data-testid', 'reach-share-' + c.id);
        row.appendChild(pct);
        ui.legend.appendChild(row);
      });
      ui.legend.appendChild(el('p', 'reach-legend-note', 'Uncoloured: below 0 dB margin, outside the radius, or no terrain data.'));
      panel.appendChild(ui.legend);

      var geo = el('fieldset', 'reach-group');
      geo.appendChild(el('legend', null, 'Geometry — recomputes'));
      field(geo, 'tx', 'Antenna height', 'm', 'number', 'reach-tx');
      field(geo, 'rx', 'Receiver height', 'm', 'number', 'reach-rx');
      field(geo, 'radius', 'Radius', 'km', 'number', 'reach-radius');
      field(geo, 'freq', 'Frequency', 'MHz', 'number', 'reach-freq');
      REQUEST_FIELDS.forEach(function (f) { ui[f].addEventListener('input', onGeometryInput); });
      panel.appendChild(geo);

      var bud = el('fieldset', 'reach-group');
      bud.appendChild(el('legend', null, 'Link budget — instant'));
      field(bud, 'power', 'TX power', 'dBm', 'range', 'reach-power');
      field(bud, 'txGain', 'TX antenna gain', 'dBi', 'range', 'reach-txgain');
      field(bud, 'rxGain', 'RX antenna gain', 'dBi', 'range', 'reach-rxgain');
      field(bud, 'loss', 'Cable loss', 'dB', 'range', 'reach-loss');
      field(bud, 'sens', 'Sensitivity', 'dBm', 'range', 'reach-sens');
      BUDGET_FIELDS.forEach(function (f) { ui[f].addEventListener('input', onBudgetInput); });
      ui.budget = el('p', 'reach-budget');
      ui.budget.setAttribute('data-testid', 'reach-budget');
      bud.appendChild(ui.budget);
      panel.appendChild(bud);

      var op = el('label', 'reach-field');
      op.appendChild(el('span', 'reach-field-label', 'Opacity'));
      ui.opacity = el('input', 'reach-input');
      ui.opacity.type = 'range';
      ui.opacity.min = '0.1';
      ui.opacity.max = '1';
      ui.opacity.step = '0.05';
      ui.opacity.value = String(opacity);
      ui.opacity.setAttribute('data-testid', 'reach-opacity');
      ui.opacity.setAttribute('aria-label', 'Layer opacity');
      ui.opacity.addEventListener('input', function () {
        opacity = Number(ui.opacity.value);
        if (overlay) overlay.setOpacity(opacity);
        try { storage() && storage().setItem(OPACITY_KEY, String(opacity)); } catch (_) { /* per-viewer only */ }
      });
      op.appendChild(ui.opacity);
      panel.appendChild(op);

      ui.model = el('p', 'reach-model');
      ui.model.setAttribute('data-testid', 'reach-model');
      panel.appendChild(ui.model);
      ui.credit = el('div', 'reach-credit');
      ui.credit.setAttribute('data-testid', 'reach-credit');
      panel.appendChild(ui.credit);
      opts.mount.appendChild(panel);
    }

    function setStatus(text, isError) {
      if (!ui.status) return;
      ui.status.textContent = text || '';
      ui.status.hidden = !text;
      ui.status.classList.toggle('is-error', !!isError);
    }

    /* syncInputs shows the state, or what the server resolved for a field
     * the state leaves to it. */
    function syncInputs() {
      if (!panel || !state) return;
      var p = data && data.params;
      var resolved = { tx: p && p.txHeightM, rx: p && p.rxHeightM, radius: p && p.radiusKm, freq: p && p.freqMHz };
      if (!p) resolved = { tx: null, rx: cfg.rxHeightM, radius: cfg.radiusKm, freq: null };
      REQUEST_FIELDS.forEach(function (f) {
        var v = typeof state[f] === 'number' ? state[f] : resolved[f];
        if (global.document.activeElement !== ui[f]) ui[f].value = v === null || v === undefined ? '' : String(v);
      });
      var bv = budgetValues(state, cfg.radio);
      BUDGET_FIELDS.forEach(function (f) {
        if (global.document.activeElement !== ui[f]) ui[f].value = String(bv[f]);
        ui[f + 'Out'].textContent = fmt(bv[f], 0) + ' ' + ui[f + 'Unit'];
      });
      ui.budget.textContent = 'Maximum path loss ' + fmt(budgetOf(state, cfg.radio), 0) + ' dB';
    }

    function syncCentre() {
      if (!ui.centre || !state) return;
      var at = state.at, label;
      if (at.kind === 'node') {
        var n = opts.resolveNode ? opts.resolveNode(at.pubkey) : null;
        label = 'From ' + ((n && n.name) || (at.pubkey.slice(0, 8) + '…'));
      } else {
        label = 'From ' + fmt(at.lat, 4) + ', ' + fmt(at.lon, 4);
      }
      if (data && data.center && typeof data.center.groundM === 'number') label += ' · ground ' + fmt(data.center.groundM, 0) + ' m';
      if (data && data.params) {
        label += ' · antenna ' + data.params.txHeightM + ' m' + (data.params.txAssumed ? ' (assumed)' : '');
      }
      ui.centre.textContent = label;
    }

    function syncModel() {
      if (!ui.model) return;
      if (!data) { ui.model.textContent = ''; return; }
      var p = data.params || {};
      var dsm = (data.sources || []).some(function (s) { return s && s.model === 'dsm'; });
      ui.model.textContent = 'Free-space loss plus Bullington diffraction over the terrain (ITU-R P.526), '
        + p.resolutionM + ' m grid, k ' + fmt(p.k, 2) + ', ' + fmt(p.freqMHz, 3) + ' MHz. '
        + (dsm ? 'Outside Denmark the terrain includes trees and buildings. ' : '')
        + 'No buildings or trees in Denmark yet: a prediction, not a measurement.';
    }

    function syncCredit() {
      if (!ui.credit) return;
      ui.credit.textContent = '';
      if (!data) return;
      var dhm = opts.dhm;
      if (data.attribution && dhm && typeof global.MC_appendCredit === 'function' && typeof global.MC_creditLinks === 'function') {
        var p = el('p', 'reach-credit-line');
        p.setAttribute('data-testid', 'reach-credit-dhm');
        global.MC_appendCredit(p, data.attribution, global.MC_creditLinks(dhm), function (a) { a.className = 'tp-credit-link'; });
        ui.credit.appendChild(p);
      } else if (data.attribution) {
        var plain = el('p', 'reach-credit-line', data.attribution);
        plain.setAttribute('data-testid', 'reach-credit-dhm');
        ui.credit.appendChild(plain);
      }
      if (data.attributionFill) {
        var f = el('p', 'reach-credit-line');
        f.setAttribute('data-testid', 'reach-credit-fill');
        var fc = cfg.fill;
        if (fc && typeof fc.credit === 'string' && typeof global.MC_appendCredit === 'function' && typeof global.MC_creditLinks === 'function') {
          global.MC_appendCredit(f, fillShortCredit(cfg), global.MC_creditLinks(fc), function (a) { a.className = 'tp-credit-link'; });
          var more = el('details', 'tp-credit-full');
          more.appendChild(el('summary', 'tp-credit-more', 'Full credit'));
          more.appendChild(el('p', 'tp-credit-line', data.attributionFill));
          f.appendChild(more);
        } else {
          f.textContent = data.attributionFill;
        }
        ui.credit.appendChild(f);
      }
    }

    // ── Drawing ──

    function recolor() {
      if (!canvas || !raster || !meta) return;
      var ctx = canvas.getContext('2d');
      var img = ctx.createImageData(meta.width, meta.height);
      var budget = budgetOf(state, cfg.radio);
      paint(img.data, raster, buildLut(budget, meta, themeColors()));
      ctx.putImageData(img, 0, 0);
      var share = classShares(raster, budget, meta);
      CLASSES.forEach(function (c) {
        var cell = ui.legend && ui.legend.querySelector('[data-testid="reach-share-' + c.id + '"]');
        if (cell) cell.textContent = share.total ? Math.round(100 * share[c.id] / share.total) + ' %' : '';
      });
    }

    function draw() {
      if (!map || !L || !meta) return;
      if (!canvas) {
        canvas = global.document.createElement('canvas');
        canvas.setAttribute('data-testid', 'reach-canvas');
      }
      canvas.width = meta.width;
      canvas.height = meta.height;
      var b = meta.bounds;
      var bounds = L.latLngBounds([b[0][0], b[0][1]], [b[1][0], b[1][1]]);
      if (!overlay) {
        overlay = canvasOverlay(L, canvas, bounds, { opacity: opacity, interactive: false, className: 'reach-layer' });
        overlay.addTo(map);
      } else {
        overlay.setBounds(bounds);
        if (!map.hasLayer(overlay)) overlay.addTo(map);
      }
      recolor();
      var c = data && data.center;
      if (c) {
        if (!centreDot) centreDot = L.circleMarker([c.lat, c.lon], { className: 'reach-centre-dot', radius: 6, weight: 2, fillOpacity: 1, interactive: false });
        else centreDot.setLatLng([c.lat, c.lon]);
        if (!map.hasLayer(centreDot)) centreDot.addTo(map);
      }
      if (fitPending) {
        fitPending = false;
        var fit = function () { map.fitBounds(bounds, { padding: [20, 20], animate: false }); };
        // The host keeps a link's fit out of the viewer's saved view.
        if (opts.linkMove) opts.linkMove(fit); else fit();
      }
    }

    function clearLayer() {
      if (map && overlay && map.hasLayer(overlay)) map.removeLayer(overlay);
      if (map && centreDot && map.hasLayer(centreDot)) map.removeLayer(centreDot);
      raster = null;
      meta = null;
    }

    // ── Requests ──

    function request() {
      if (!state || !fetchFn) return;
      var gen = ++generation;
      if (aborter) try { aborter.abort(); } catch (_) { /* gone */ }
      aborter = typeof global.AbortController === 'function' ? new global.AbortController() : null;
      var signal = aborter ? aborter.signal : undefined;
      setStatus('Predicting the reach… a cold area can take up to half a minute.');
      panel && panel.classList.add('is-busy');
      fetchFn(requestURL(state), { signal: signal, credentials: 'same-origin' }).then(function (resp) {
        if (gen !== generation) return null;
        return resp.json().catch(function () { return null; }).then(function (body) {
          if (gen !== generation) return null;
          if (!resp.ok) {
            var retry = resp.headers && resp.headers.get ? resp.headers.get('Retry-After') : null;
            throw { shown: refusal(resp.status, body, retry, cfg) };
          }
          return body;
        });
      }).then(function (body) {
        if (!body || gen !== generation) return null;
        var r = body.raster;
        if (!r || typeof r.url !== 'string' || r.url.indexOf(ENDPOINT + '/raster?key=') !== 0
          || !(r.width > 0 && r.height > 0 && r.width * r.height <= 4e6)) {
          throw { shown: 'The prediction failed.' };
        }
        return fetchFn(r.url, { signal: signal, credentials: 'same-origin' }).then(function (resp) {
          if (gen !== generation) return null;
          if (!resp.ok) throw { shown: 'The prediction expired before it arrived. Try again.' };
          return resp.arrayBuffer();
        }).then(function (buf) {
          if (!buf || gen !== generation) return;
          var bytes = new Uint8Array(buf);
          if (bytes.length !== r.width * r.height) throw { shown: 'The prediction failed.' };
          data = body;
          raster = bytes;
          meta = { width: r.width, height: r.height, bounds: r.bounds, lossBaseDb: r.lossBaseDb, lossStepDb: r.lossStepDb, noData: r.noData };
          setStatus(r.coveredPct < 100 ? 'Terrain data covers ' + Math.round(r.coveredPct) + ' % of the area.' : '');
          panel && panel.classList.remove('is-busy');
          draw();
          syncInputs();
          syncCentre();
          syncModel();
          syncCredit();
        });
      }).catch(function (e) {
        if (gen !== generation) return;
        if (e && e.name === 'AbortError') return;
        panel && panel.classList.remove('is-busy');
        setStatus(e && e.shown ? e.shown : 'The prediction failed: network error.', true);
      });
    }

    function scheduleRequest() {
      clearTimeout(requestTimer);
      requestTimer = setTimeout(request, REQUEST_DEBOUNCE_MS);
    }

    function writeHash() {
      clearTimeout(hashTimer);
      hashTimer = setTimeout(function () {
        if (!global.location || !global.history) return;
        if (String(global.location.hash || '').indexOf('#/map') !== 0) return;
        try {
          global.history.replaceState(global.history.state, '', global.location.pathname + global.location.search
            + withReachParams(global.location.hash, serializeState(state)));
        } catch (_) { /* the view still works */ }
      }, HASH_DEBOUNCE_MS);
    }

    function onGeometryInput(e) {
      var f = REQUEST_FIELDS.filter(function (k) { return ui[k] === e.target; })[0];
      var v = boundedNumber(f, e.target.value);
      if (f === 'radius' && v !== null && cfg.maxRadiusKm && v > cfg.maxRadiusKm) v = null;
      e.target.classList.toggle('is-invalid', v === null && e.target.value !== '');
      if (v === null) return;
      state[f] = v;
      writeHash();
      scheduleRequest();
    }

    var recolorFrame = 0;
    function onBudgetInput(e) {
      var f = BUDGET_FIELDS.filter(function (k) { return ui[k] === e.target; })[0];
      var v = boundedNumber(f, e.target.value);
      if (v === null) return;
      state[f] = v;
      syncInputs();
      writeHash();
      // One repaint per frame, however fast the slider moves.
      if (recolorFrame) return;
      var raf = global.requestAnimationFrame || function (fn) { return setTimeout(fn, 16); };
      recolorFrame = raf(function () { recolorFrame = 0; recolor(); });
    }

    // ── Open / close ──

    /* open shows the reach of st (a parseState result): the panel, one
     * request, the hash. fit: bring the disk into view when it arrives. */
    function open(st, o) {
      if (!st || !st.at) return;
      if (opts.onOpen) opts.onOpen();
      buildPanel();
      var sameGeometry = state && atString(state.at) === atString(st.at)
        && REQUEST_FIELDS.every(function (f) { return state[f] === st[f]; });
      state = Object.assign({}, st);
      fitPending = !!(o && o.fit);
      panel.hidden = false;
      if (!sameGeometry) data = null;
      syncInputs();
      syncCentre();
      writeHash();
      if (sameGeometry && raster) { recolor(); return; }
      request();
    }

    function closePanel() {
      generation++;
      if (aborter) try { aborter.abort(); } catch (_) { /* gone */ }
      clearTimeout(requestTimer);
      disarm();
      clearLayer();
      state = null;
      data = null;
      if (panel) panel.hidden = true;
      writeHash();
    }

    function onThemeChange() { recolor(); }

    buildToggle();
    global.document.addEventListener('keydown', onKey);
    global.addEventListener('theme-refresh', onThemeChange);

    return {
      arm: arm,
      disarm: disarm,
      isArmed: function () { return armed; },
      open: open,
      close: closePanel,
      isOpen: function () { return !!(panel && !panel.hidden && state); },
      getState: function () { return state ? Object.assign({}, state) : null; },
      el: function () { return panel; },
      destroy: function () {
        closePanel();
        clearTimeout(hashTimer);
        global.document.removeEventListener('keydown', onKey);
        global.removeEventListener('theme-refresh', onThemeChange);
        if (panel && panel.parentNode) panel.parentNode.removeChild(panel);
        if (btn && btn.parentNode) btn.parentNode.removeChild(btn);
        if (hint && hint.parentNode) hint.parentNode.removeChild(hint);
        panel = btn = hint = null;
        overlay = canvas = centreDot = null;
        ui = {};
      },
      // Test seam: repaint now, as a slider would on the next frame.
      recolorForTests: recolor
    };
  }

  global.PredictedReach = {
    ENDPOINT: ENDPOINT,
    HASH_PARAM: HASH_PARAM,
    KEYS: KEYS,
    CLASSES: CLASSES,
    parseState: parseState,
    invalidLink: invalidLink,
    serializeState: serializeState,
    withReachParams: withReachParams,
    requestURL: requestURL,
    budgetOf: budgetOf,
    marginClass: marginClass,
    parseColor: parseColor,
    themeColors: themeColors,
    buildLut: buildLut,
    paint: paint,
    classShares: classShares,
    refusal: refusal,
    fillShortCredit: fillShortCredit,
    offered: offered,
    hasFix: hasFix,
    nodeEntryHash: nodeEntryHash,
    nodeEntryAction: nodeEntryAction,
    create: create
  };
})(typeof window !== 'undefined' ? window : this);
