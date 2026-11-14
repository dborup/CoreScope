/* masts-layer.js — #724 M3: antenna sites from Mastedatabasen on the map.
 *
 * Two overlays in the layer control (public/map-tile-providers.js owns the
 * toggles, the hash/storage preference and the credit):
 *   - "Antenna sites (mast, tower or rooftop)": every existing site, on by
 *     default when the server offers the layer (owner decision, option A:
 *     the source does not say whether a site is a mast, a tower or a
 *     rooftop, so the label does not pretend to know);
 *   - "Nearby transmitters (interference)": only the sites with a 700/800/
 *     900 MHz (or GSM-R) entry, off by default. The rule is the server's
 *     (mastedb.InterferenceRelevant); this file only reads the flag.
 *
 * Data comes from GET /api/masts?cell=E,N, one 2 km EPSG:25832 grid cell
 * per request, which the server proxies on demand with a shared cache
 * (cmd/server/masts_proxy.go). The browser:
 *   - fetches nothing unless one of the two overlays is on and the map is
 *     at or above the zoom gate (cfg.minZoom, 13);
 *   - enumerates the cells of the viewport (cellsForView), at most
 *     VIEW_CELL_CAP of them, centre first, and never one outside the
 *     server's extent (cfg.cellExtent);
 *   - paces them: at most CONCURRENCY requests in flight per map, queued
 *     cells that leave the view before they start are dropped, a 503 is
 *     retried after its Retry-After and a 429 pauses the queue (#753 review
 *     N4: one cold z13 viewport is ~80 cells, the server admits 16 fetches);
 *   - diffs per cell: a cell's markers are built once and attached or
 *     detached as the cell enters or leaves the view; nothing is rebuilt;
 *   - keeps at most KEEP_CELL_CAP cells (LRU, the visible cells never go),
 *     so memory is bounded however far the viewer pans;
 *   - draws on one canvas renderer per map, with colours from the CSS
 *     variables --mast-site and --mast-interference (re-read on a theme
 *     change).
 *
 * Popups show published fields only (address, station names, technology,
 * band, service, commissioning date), every one escaped with the shared
 * escapeHtml (app.js). There is no operator, height or structure type,
 * because the source publishes none.
 */
(function () {
  'use strict';

  // Cells one view may need. At most the server's default per-IP budget
  // (mastsCellRateLimitDefault, 120 cells a minute), so a cold view the
  // layer draws at all is served inside one minute instead of leaving its
  // last cells on 429 (#758 review F2). A 1920×1080 z13 view is ~80; a
  // wider one asks for one more zoom step (statusText).
  var VIEW_CELL_CAP = 120;
  var KEEP_CELL_CAP = 320;  // cells kept in memory, visible ones included
  var CONCURRENCY = 4;      // cell requests in flight per map
  var BUSY_RETRIES = 3;     // 503s per cell before it counts as failed
  var FAIL_RETRY_MS = 60000; // a failed cell is not asked again for this long
  var MAX_WAIT_MS = 60000;  // longest Retry-After honoured
  var DEBOUNCE_MS = 250;
  // #724 owner decision 2: the distance a cellular transmitter in these
  // bands may matter at, and the strong tier. Also used by the M4 hint
  // (public/masts-hint.js). The customizer exposes them in M5.
  var HINT_DISTANCE_M = 500;
  var HINT_STRONG_M = 100;

  // ── WGS84 → EPSG:25832 (ETRS89 / UTM 32N) ───────────────────────────────
  // The Krüger series to fourth order, as internal/dhm/utm.go, which is
  // what the server's cell grid is defined in. Only the forward direction
  // is needed here.
  var TM = (function () {
    var a = 6378137, f = 1 / 298.257222101, k0 = 0.9996;
    var n = f / (2 - f), n2 = n * n, n3 = n2 * n, n4 = n3 * n;
    return {
      lon0: 9 * Math.PI / 180,
      e: Math.sqrt(f * (2 - f)),
      kA: k0 * a / (1 + n) * (1 + n2 / 4 + n4 / 64),
      alpha: [
        n / 2 - 2 * n2 / 3 + 5 * n3 / 16 + 41 * n4 / 180,
        13 * n2 / 48 - 3 * n3 / 5 + 557 * n4 / 1440,
        61 * n3 / 240 - 103 * n4 / 140,
        49561 * n4 / 161280
      ]
    };
  })();

  function atanh(x) { return 0.5 * Math.log((1 + x) / (1 - x)); }
  function sinh(x) { return (Math.exp(x) - Math.exp(-x)) / 2; }
  function cosh(x) { return (Math.exp(x) + Math.exp(-x)) / 2; }

  function toUTM32(lat, lon) {
    var phi = lat * Math.PI / 180, dl = lon * Math.PI / 180 - TM.lon0;
    var t = sinh(atanh(Math.sin(phi)) - TM.e * atanh(TM.e * Math.sin(phi)));
    var xi = Math.atan2(t, Math.cos(dl));
    var eta = atanh(Math.sin(dl) / Math.sqrt(1 + t * t));
    var x = eta, y = xi;
    for (var j = 0; j < 4; j++) {
      var k = 2 * (j + 1);
      x += TM.alpha[j] * Math.cos(k * xi) * sinh(k * eta);
      y += TM.alpha[j] * Math.sin(k * xi) * cosh(k * eta);
    }
    return { e: 500000 + TM.kA * x, n: TM.kA * y };
  }

  // ── Cells of a view ─────────────────────────────────────────────────────

  function cellKey(e, n) { return e + ',' + n; }

  /**
   * The grid cells a lat/lon box touches, nearest the centre first, or
   * null when there are more than `cap` (the caller treats that like the
   * zoom gate). Cells outside cfg.cellExtent are left out.
   *
   * A box of constant latitudes and longitudes is not a box in UTM: the
   * eastings are extreme at its corners, and a parallel's northing is
   * lowest where it crosses the central meridian (9° E), so that point is
   * sampled on both edges as well.
   *
   * @param {{south:number, west:number, north:number, east:number}} b
   * @param {{cellSizeM:number, cellExtent:number[]}} cfg
   * @param {number} [cap]
   */
  function cellsForView(b, cfg, cap) {
    cap = cap || VIEW_CELL_CAP;
    var mid = Math.max(b.west, Math.min(b.east, 9));
    var pts = [[b.south, b.west], [b.south, b.east], [b.north, b.west], [b.north, b.east], [b.south, mid], [b.north, mid]];
    var minE = Infinity, maxE = -Infinity, minN = Infinity, maxN = -Infinity;
    for (var i = 0; i < pts.length; i++) {
      var p = toUTM32(pts[i][0], pts[i][1]);
      if (p.e < minE) minE = p.e;
      if (p.e > maxE) maxE = p.e;
      if (p.n < minN) minN = p.n;
      if (p.n > maxN) maxN = p.n;
    }
    var s = cfg.cellSizeM, x = cfg.cellExtent;
    var e0 = Math.max(Math.floor(minE / s), x[0]), e1 = Math.min(Math.floor(maxE / s), x[2]);
    var n0 = Math.max(Math.floor(minN / s), x[1]), n1 = Math.min(Math.floor(maxN / s), x[3]);
    if (e1 < e0 || n1 < n0) return [];
    if ((e1 - e0 + 1) * (n1 - n0 + 1) > cap) return null;
    var c = toUTM32((b.south + b.north) / 2, (b.west + b.east) / 2);
    var ce = c.e / s - 0.5, cn = c.n / s - 0.5;
    var out = [];
    for (var e = e0; e <= e1; e++) {
      for (var n = n0; n <= n1; n++) out.push({ e: e, n: n, key: cellKey(e, n), d: (e - ce) * (e - ce) + (n - cn) * (n - cn) });
    }
    out.sort(function (p, q) { return p.d - q.d; });
    return out;
  }

  // ── Paced, bounded cell loader ──────────────────────────────────────────

  /**
   * Loads the cells a view asks for, at most `concurrency` at a time, and
   * keeps at most `keepCap` of them.
   *
   * opts.fetchCell(key) → Promise<{status, retryAfterS, data}>; onLoad(key,
   * data) when a cell arrives that is still kept; onEvict(key, hadData)
   * when a cell is dropped; onChange() after any state change. now,
   * setTimeout and clearTimeout are injectable for tests.
   */
  function createCellLoader(opts) {
    var concurrency = opts.concurrency || CONCURRENCY;
    var keepCap = opts.keepCap || KEEP_CELL_CAP;
    var now = opts.now || function () { return Date.now(); };
    var setT = opts.setTimeout || function (fn, ms) { return setTimeout(fn, ms); };
    var clearT = opts.clearTimeout || function (t) { clearTimeout(t); };
    var noop = function () {};
    var onLoad = opts.onLoad || noop, onEvict = opts.onEvict || noop, onChange = opts.onChange || noop;

    // key → { state: 'loading'|'loaded'|'waiting'|'failed', data, retryAt, busy }
    // in LRU order: the most recently wanted cells are last.
    var entries = new Map();
    var wanted = [];
    var wantedSet = new Set();
    var inFlight = 0;
    var pausedUntil = 0;
    var timer = null, timerAt = 0;
    var destroyed = false;

    function schedule(at) {
      if (timer && timerAt <= at) return;
      if (timer) clearT(timer);
      timerAt = at;
      timer = setT(function () { timer = null; pump(); }, Math.max(0, at - now()));
    }

    function evict() {
      if (entries.size <= keepCap) return;
      var it = entries.keys();
      while (entries.size > keepCap) {
        var r = it.next();
        if (r.done) break;
        if (wantedSet.has(r.value)) continue;
        var e = entries.get(r.value);
        entries.delete(r.value);
        onEvict(r.value, e.state === 'loaded');
      }
    }

    function due(e, t) {
      return !e || ((e.state === 'waiting' || e.state === 'failed') && e.retryAt <= t);
    }

    function pump() {
      if (destroyed) return;
      var t = now();
      if (t < pausedUntil) { schedule(pausedUntil); return; }
      var next = Infinity;
      for (var i = 0; i < wanted.length && inFlight < concurrency; i++) {
        var key = wanted[i], e = entries.get(key);
        if (due(e, t)) start(key, e);
        else if (e.state === 'waiting' || e.state === 'failed') next = Math.min(next, e.retryAt);
      }
      if (next !== Infinity) schedule(next);
    }

    function start(key, prev) {
      var e = prev || { busy: 0 };
      e.state = 'loading';
      entries.delete(key);
      entries.set(key, e);
      inFlight++;
      evict();
      var p;
      try { p = Promise.resolve(opts.fetchCell(key)); } catch (err) { p = Promise.reject(err); }
      p.then(function (res) { finish(key, e, res || { status: 0 }); },
             function () { finish(key, e, { status: 0 }); });
    }

    function waitMs(res) {
      var s = Number(res.retryAfterS);
      return Math.min(MAX_WAIT_MS, (isFinite(s) && s > 0 ? s : 5) * 1000);
    }

    function finish(key, e, res) {
      inFlight--;
      if (destroyed) return;
      var kept = entries.get(key) === e;
      var t = now();
      if (res.status === 200 && res.data) {
        e.state = 'loaded';
        e.data = res.data;
        if (kept) onLoad(key, res.data);
      } else if (res.status === 429) {
        e.state = 'waiting';
        e.retryAt = t + waitMs(res);
        pausedUntil = Math.max(pausedUntil, e.retryAt);
      } else if (res.status === 503 && ++e.busy <= BUSY_RETRIES) {
        e.state = 'waiting';
        e.retryAt = t + waitMs(res);
      } else {
        e.state = 'failed';
        e.retryAt = t + FAIL_RETRY_MS;
      }
      onChange();
      pump();
    }

    return {
      /** The cells the view needs now, in fetch order. */
      view: function (keys) {
        wanted = keys.slice();
        wantedSet = new Set(wanted);
        // Touch: the wanted cells move to the LRU tail.
        for (var i = 0; i < wanted.length; i++) {
          var e = entries.get(wanted[i]);
          if (e) { entries.delete(wanted[i]); entries.set(wanted[i], e); }
        }
        evict();
        pump();
        onChange();
      },
      data: function (key) { var e = entries.get(key); return e && e.state === 'loaded' ? e.data : null; },
      /** 'loaded' | 'loading' | 'waiting' | 'failed', or '' for an unknown cell. */
      state: function (key) { var e = entries.get(key); return e ? e.state : ''; },
      stats: function () {
        var s = { kept: entries.size, inFlight: inFlight, wanted: wanted.length, loaded: 0, pending: 0, failed: 0, truncated: 0 };
        for (var i = 0; i < wanted.length; i++) {
          var e = entries.get(wanted[i]);
          if (!e || e.state === 'loading' || e.state === 'waiting') s.pending++;
          else if (e.state === 'failed') s.failed++;
          else { s.loaded++; if (e.data && e.data.truncated) s.truncated++; }
        }
        return s;
      },
      destroy: function () {
        destroyed = true;
        if (timer) clearT(timer);
        timer = null;
        entries.clear();
      }
    };
  }

  // ── Popup ───────────────────────────────────────────────────────────────

  var SITE_LABEL = 'Antenna sites (mast, tower or rooftop)';
  var INTERFERENCE_LABEL = 'Nearby transmitters (interference)';

  /**
   * The popup of one site: published fields only, each escaped.
   * @param {object} site  one entry of MastsCellResponse.sites
   * @param {{sourceUrl:string}} cfg
   */
  function popupHTML(site, cfg) {
    var esc = escapeHtml;
    var street = [site.street, site.houseNumber].filter(Boolean).join(' ');
    var place = [site.postcode, site.city].filter(Boolean).join(' ');
    var addr = [street, place].filter(Boolean).join(', ');
    var h = '<div class="mc-masts-popup-body">';
    h += '<div class="mc-masts-popup-title">Antenna site</div>';
    if (addr) h += '<div class="mc-masts-popup-addr">' + esc(addr) + '</div>';
    if (site.stations && site.stations.length) {
      h += '<div class="mc-masts-popup-stations">Station' + (site.stations.length > 1 ? 's' : '') + ': ' +
        site.stations.map(esc).join(', ') + '</div>';
    }
    var entries = site.entries || [];
    if (entries.length) {
      h += '<table class="mc-masts-popup-table"><thead><tr><th>Technology</th><th>Band (MHz)</th><th>Service</th><th>Commissioned</th></tr></thead><tbody>';
      for (var i = 0; i < entries.length; i++) {
        var en = entries[i];
        h += '<tr' + (en.interference ? ' class="mc-masts-int"' : '') + '><td>' + esc(en.technology) + '</td><td>' +
          (en.bandMHz ? esc(en.bandMHz) : '—') + '</td><td>' + esc(en.service || '—') + '</td><td>' +
          esc(en.commissioned || '—') + '</td></tr>';
      }
      h += '</tbody></table>';
    }
    if (site.interference) {
      h += '<p class="mc-masts-popup-hint">Has a 700/800/900 MHz entry. Within about ' + HINT_DISTANCE_M +
        ' m, especially within ' + HINT_STRONG_M + ' m, a cellular transmitter like this may desensitise an unfiltered ' +
        '868 MHz receiver; a band-pass filter or another site may help. Based on distance only: the source ' +
        'publishes no height or power.</p>';
    }
    if (site.likelyMast) {
      h += '<p class="mc-masts-popup-note">Broadcast technology: likely a tall dedicated mast (a heuristic, not from the source).</p>';
    }
    h += '<p class="mc-masts-popup-note">The source does not say whether this is a mast, a tower or a rooftop. ' +
      'Data reported by permit holders to Mastedatabasen (Digitaliseringsstyrelsen, CC0); accuracy not guaranteed. ' +
      '<a href="' + esc(cfg.sourceUrl) + '" target="_blank" rel="noopener noreferrer">mastedatabasen.dk</a></p>';
    return h + '</div>';
  }

  // ── Leaflet glue (browser only) ─────────────────────────────────────────

  function cssColor(name) {
    try {
      return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    } catch (_) { return ''; }
  }

  function styles() {
    var site = cssColor('--mast-site'), int = cssColor('--mast-interference');
    return {
      sites: { radius: 5, weight: 1, color: site, fillColor: site, fillOpacity: 0.85 },
      interference: { radius: 10, weight: 2.5, color: int, fillColor: int, fillOpacity: 0.15 }
    };
  }

  function statusText(z, cfg, active, stats, tooWide) {
    if (!active) return '';
    if (z < cfg.minZoom) return 'Zoom in to ' + cfg.minZoom + '+ to see antenna positions';
    // Over VIEW_CELL_CAP at or above the gate (a large display): the map
    // already is at the gate zoom, so asking for it would be wrong (#758 F1).
    if (tooWide) return 'This view is too wide for antenna positions; zoom in one more step';
    if (stats.pending) return 'Loading antenna sites…';
    if (stats.failed) return 'Antenna data unavailable for some areas';
    if (stats.truncated) return 'Some areas hit the source\'s 500-row limit; sites may be missing';
    return '';
  }

  // One controller per map: one loader, one renderer, two layer groups.
  function controllerFor(map, cfg) {
    if (map._mcMasts) return map._mcMasts;
    var renderer = L.canvas({ padding: 0.3 });
    var groups = {
      sites: L.layerGroup([], { attribution: cfg.attribution }),
      interference: L.layerGroup([], { attribution: cfg.attribution })
    };
    var cells = new Map(); // key → { sites: LayerGroup, interference: LayerGroup }
    var attached = new Set();
    var visible = new Set();
    var statusEl = null;
    var tooWide = false;
    // The view's cells, centre first, and the cells pinned for the profile
    // hint (#724 M4, public/masts-hint.js). Pinned cells are fetched and
    // kept like any other through the same loader, whether or not an
    // overlay is on, but only drawn when they are also in the view.
    var viewKeys = [];
    var pinned = [];
    var pinListener = null, pinSig = '';
    var st = styles();
    var debounceT = null;

    function popupFor(site) { return function () { return popupHTML(site, cfg); }; }

    function build(key) {
      var data = loader.data(key);
      if (!data) return null;
      var c = { sites: L.layerGroup(), interference: L.layerGroup() };
      (data.sites || []).forEach(function (site) {
        var ll = [site.lat, site.lon];
        // 280, like every other map popup: it fits a 375 px phone (#758 F3).
        var popupOpts = { maxWidth: 280, className: 'mc-masts-popup' };
        c.sites.addLayer(L.circleMarker(ll, Object.assign({ renderer: renderer }, st.sites)).bindPopup(popupFor(site), popupOpts));
        if (site.interference) {
          c.interference.addLayer(L.circleMarker(ll, Object.assign({ renderer: renderer }, st.interference)).bindPopup(popupFor(site), popupOpts));
        }
      });
      cells.set(key, c);
      return c;
    }

    function attach(key) {
      if (attached.has(key)) return;
      var c = cells.get(key) || build(key);
      if (!c) return;
      groups.sites.addLayer(c.sites);
      groups.interference.addLayer(c.interference);
      attached.add(key);
    }

    function detach(key) {
      if (!attached.has(key)) return;
      var c = cells.get(key);
      groups.sites.removeLayer(c.sites);
      groups.interference.removeLayer(c.interference);
      attached.delete(key);
    }

    function active() { return map.hasLayer(groups.sites) || map.hasLayer(groups.interference); }

    // The loader's wanted list: the pinned cells first (a handful, the
    // viewer is waiting on them), then the view's, each once.
    function wantedKeys() {
      if (!pinned.length) return viewKeys;
      var seen = new Set(pinned);
      return pinned.concat(viewKeys.filter(function (k) { return !seen.has(k); }));
    }

    // Tells the pin listener when one of its cells settled, not on every
    // state change of the view's cells.
    function notifyPinned() {
      if (!pinListener) return;
      var sig = pinned.map(function (k) { return loader.state(k); }).join(',');
      if (sig === pinSig) return;
      pinSig = sig;
      pinListener();
    }

    function renderStatus() {
      notifyPinned();
      if (!statusEl) return;
      var text = statusText(map.getZoom(), cfg, active(), loader.stats(), tooWide);
      statusEl.textContent = text;
      statusEl.hidden = !text;
    }

    var loader = createCellLoader({
      fetchCell: function (key) {
        return fetch(cfg.url + '?cell=' + encodeURIComponent(key), { credentials: 'same-origin' }).then(function (r) {
          var ra = r.headers.get('Retry-After');
          if (!r.ok) return { status: r.status, retryAfterS: ra };
          return r.json().then(function (data) { return { status: 200, data: data }; });
        });
      },
      onLoad: function (key) { if (visible.has(key)) attach(key); },
      onEvict: function (key) { detach(key); cells.delete(key); },
      onChange: renderStatus
    });

    function update() {
      debounceT = null;
      var want = [];
      tooWide = false;
      if (active() && map.getZoom() >= cfg.minZoom) {
        var b = map.getBounds();
        var list = cellsForView({ south: b.getSouth(), west: b.getWest(), north: b.getNorth(), east: b.getEast() }, cfg);
        if (list) want = list.map(function (c) { return c.key; });
        else tooWide = true;
      }
      visible = new Set(want);
      viewKeys = want;
      attached.forEach(function (key) { if (!visible.has(key)) detach(key); });
      want.forEach(function (key) { if (loader.data(key)) attach(key); });
      loader.view(wantedKeys());
      renderStatus();
    }

    function schedule() {
      if (debounceT) clearTimeout(debounceT);
      debounceT = setTimeout(update, DEBOUNCE_MS);
    }

    function restyle() {
      st = styles();
      cells.forEach(function (c) {
        c.sites.eachLayer(function (m) { m.setStyle(st.sites); });
        c.interference.eachLayer(function (m) { m.setStyle(st.interference); });
      });
    }

    var themeObs = null;
    try {
      themeObs = new MutationObserver(restyle);
      themeObs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    } catch (_) {}
    window.addEventListener('theme-refresh', restyle);

    map.on('moveend zoomend', schedule);
    groups.sites.on('add remove', schedule);
    groups.interference.on('add remove', schedule);
    map.on('unload', function () {
      if (debounceT) clearTimeout(debounceT);
      loader.destroy();
      if (themeObs) themeObs.disconnect();
      window.removeEventListener('theme-refresh', restyle);
      map._mcMasts = null;
    });

    map._mcMasts = {
      groups: groups,
      setStatusElement: function (el) { statusEl = el; renderStatus(); },
      /* Keeps `keys` loaded (and fetches the missing ones, paced like the
       * view's) until the next pin; onChange runs when one of them settles.
       * pin([]) lets them go back to the LRU. */
      pin: function (keys, onChange) {
        pinned = (keys || []).slice();
        pinListener = onChange || null;
        pinSig = '';
        loader.view(wantedKeys());
      },
      /** A pinned or viewed cell's data, or null while it is not loaded. */
      data: function (key) { return loader.data(key); },
      // For tests and diagnostics only.
      stats: function () { var s = loader.stats(); s.attached = attached.size; s.built = cells.size; return s; }
    };
    return map._mcMasts;
  }

  window.MastsLayer = {
    SITE_LABEL: SITE_LABEL,
    INTERFERENCE_LABEL: INTERFERENCE_LABEL,
    VIEW_CELL_CAP: VIEW_CELL_CAP,
    KEEP_CELL_CAP: KEEP_CELL_CAP,
    CONCURRENCY: CONCURRENCY,
    HINT_DISTANCE_M: HINT_DISTANCE_M,
    HINT_STRONG_M: HINT_STRONG_M,
    toUTM32: toUTM32,
    cellsForView: cellsForView,
    createCellLoader: createCellLoader,
    popupHTML: popupHTML,
    statusText: statusText,
    /** The overlay layer of one category ('sites' | 'interference'). */
    layer: function (map, cfg, kind) { return controllerFor(map, cfg).groups[kind]; },
    /** The map's one controller: pin() and data() for the profile hint. */
    controller: controllerFor,
    /** Puts the status line under the overlay's label. */
    decorate: function (map, cfg, labelEl) {
      var doc = labelEl.ownerDocument || document;
      var el = doc.createElement('div');
      el.className = 'mc-masts-status';
      el.setAttribute('role', 'status');
      el.setAttribute('aria-live', 'polite');
      el.hidden = true;
      if (labelEl.parentNode) labelEl.parentNode.insertBefore(el, labelEl.nextSibling);
      controllerFor(map, cfg).setStatusElement(el);
    }
  };
})();
