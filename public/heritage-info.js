/* heritage-info.js — click-for-info on the protected ancient monuments
 * overlay (#723 M2), shared by the Map and Live pages.
 *
 * The overlay itself (map-tile-providers.js, #723 M1) is a WMS image with
 * no feature data. When it is on and the map is at its minZoom (12) or
 * closer, a click on the map asks GET /api/terrain/heritage for that point
 * and shows what the register says in a small popup: the protection zone
 * holding the point, or the nearest one within the server's radius, with
 * the dataset's own type, dating, status and numbers, and whether the
 * point is inside the zone. No advice is given (the profile warning is
 * #723 M3), and there is no link: no stable public URL for a monument
 * could be documented.
 *
 * Click precedence — anything else on the map wins:
 *   - nothing happens while the overlay is off, below its minZoom,
 *     outside its bounds (Denmark) or without the server's heritage block,
 *     so no request is made then;
 *   - the host's own modes win (Map: profile picking and dragging A/B,
 *     passed in as isBusy); busy is read at 'preclick' too, so a pick that
 *     disarms the picker in the same click still wins;
 *   - a click on a marker, a path, a popup or a control, or one a layer
 *     stopped, belongs to that layer.
 *
 * One request per click; a new click aborts the previous one and only the
 * latest answer is rendered. Turning the overlay off closes the popup.
 *
 * The popup is built with createElement + textContent only: dataset text
 * is never parsed as HTML. Colours come from style.css variables.
 */
(function (global) {
  'use strict';

  var ENDPOINT = '/api/terrain/heritage';
  // Clicks on these belong to something else on the map.
  var OWNED = '.leaflet-interactive, .leaflet-marker-icon, .leaflet-popup, .leaflet-control';

  function defaultConfig() {
    return typeof global.MC_getHeritageConfig === 'function' ? global.MC_getHeritageConfig() : null;
  }

  // bounds is the overlay's [[south, west], [north, east]]; none: no limit.
  // Also the Skråfoto node action's coverage test (skraafoto.js, #818).
  function inBounds(bounds, lat, lon) {
    if (!Array.isArray(bounds) || bounds.length !== 2) return true;
    return lat >= bounds[0][0] && lat <= bounds[1][0] && lon >= bounds[0][1] && lon <= bounds[1][1];
  }

  function ownedTarget(ev) {
    if (!ev) return false;
    if (ev._stopped) return true;
    var t = ev.target;
    return !!(t && typeof t.closest === 'function' && t.closest(OWNED));
  }

  // ── Popup content ────────────────────────────────────────────────────────

  function el(doc, tag, cls, text) {
    var e = doc.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = String(text);
    return e;
  }

  function wrap(doc) { return el(doc, 'div', 'mc-heritage-info'); }

  function message(doc, text) {
    var w = wrap(doc);
    w.appendChild(el(doc, 'div', 'mc-heritage-msg', text));
    return w;
  }

  function row(doc, dl, label, value) {
    if (value == null || value === '') return;
    dl.appendChild(el(doc, 'dt', null, label));
    dl.appendChild(el(doc, 'dd', null, value));
  }

  function str(v) { return typeof v === 'string' ? v : ''; }

  /* buildContent turns one /api/terrain/heritage answer into the popup's
   * element. Every value is set as text. */
  function buildContent(doc, resp) {
    if (!resp || typeof resp !== 'object') return message(doc, 'Monument information is unavailable right now.');
    var w = wrap(doc);
    var n = resp.nearest;
    if (!n || typeof n !== 'object') {
      var r = Number(resp.radiusM);
      w.appendChild(el(doc, 'div', 'mc-heritage-msg', 'No protection zone within ' + (isFinite(r) ? Math.round(r) : '') + ' m of this point.'));
    } else {
      w.appendChild(el(doc, 'div', 'mc-heritage-title', str(n.type) || 'Protection zone'));
      var dl = el(doc, 'dl', 'mc-heritage-fields');
      row(doc, dl, 'Dating', str(n.dating));
      row(doc, dl, 'Status', str(n.status));
      row(doc, dl, 'Protection no.', str(n.protectionNumber));
      row(doc, dl, 'System no.', str(n.systemNumber));
      row(doc, dl, 'Inside protection zone', n.insideZone === true ? 'Yes' : 'No');
      var d = n.distanceM;
      if (n.insideZone !== true && typeof d === 'number' && isFinite(d)) row(doc, dl, 'Distance to zone', Math.round(d) + ' m');
      w.appendChild(dl);
    }
    if (resp.truncated === true) w.appendChild(el(doc, 'div', 'mc-heritage-note', 'Many zones here: this answer may be incomplete.'));
    if (str(resp.attribution)) w.appendChild(el(doc, 'div', 'mc-heritage-credit', resp.attribution));
    return w;
  }

  function buildFailure(doc, status, retryAfter) {
    if (status === 429) {
      var s = parseInt(retryAfter, 10);
      return message(doc, 'Too many lookups' + (s > 0 ? ': try again in ' + s + ' s.' : '; try again shortly.'));
    }
    return message(doc, 'Monument information is unavailable right now.');
  }

  // ── Map wiring ───────────────────────────────────────────────────────────

  /* attach wires click-for-info to a Leaflet map. opts (all optional):
   *   isBusy()  — true while the host owns map clicks (Map: profile tool);
   *   config()  — the heritage client block (default MC_getHeritageConfig);
   *   layer()   — the overlay layer (default map._mcHeritageLayer, which
   *               map-tile-providers.js keeps);
   *   fetch, document — injected for tests.
   * Returns { detach }. */
  function attach(map, opts) {
    opts = opts || {};
    var isBusy = opts.isBusy || function () { return false; };
    var config = opts.config || defaultConfig;
    var layer = opts.layer || function () { return map._mcHeritageLayer; };
    var doFetch = opts.fetch || function (u, i) { return global.fetch(u, i); };
    var doc = opts.document || global.document;
    var busyAtPress = false;
    var seq = 0;
    var ctrl = null;
    var popup = null;

    function overlayOn() {
      var l = layer();
      return !!(l && map.hasLayer(l));
    }

    function abort() {
      seq++;
      if (ctrl) { try { ctrl.abort(); } catch (_) {} }
      ctrl = null;
    }

    function show(latlng, content) {
      if (!popup) {
        popup = global.L.popup({ className: 'mc-heritage-popup', maxWidth: 260, autoPanPadding: [16, 16] });
        popup.on('remove', abort);
      }
      popup.setLatLng(latlng).setContent(content);
      if (!popup.isOpen()) popup.openOn(map);
    }

    function onPreclick(e) {
      busyAtPress = !!isBusy() || ownedTarget(e && e.originalEvent);
    }

    function onClick(e) {
      var pressedBusy = busyAtPress;
      busyAtPress = false;
      if (pressedBusy || isBusy() || !e || !e.latlng || ownedTarget(e.originalEvent)) return;
      var cfg = config();
      if (!cfg || !overlayOn() || map.getZoom() < cfg.minZoom) return;
      var lat = Number(e.latlng.lat), lon = Number(e.latlng.lng);
      if (!inBounds(cfg.bounds, lat, lon)) return;
      abort();
      var mine = seq;
      ctrl = typeof global.AbortController === 'function' ? new global.AbortController() : null;
      var url = ENDPOINT + '?lat=' + lat.toFixed(6) + '&lon=' + lon.toFixed(6);
      show(e.latlng, message(doc, 'Looking up protected monuments…'));
      doFetch(url, { signal: ctrl ? ctrl.signal : undefined, headers: { Accept: 'application/json' } })
        .then(function (r) {
          if (!r.ok) return { fail: buildFailure(doc, r.status, r.headers && r.headers.get('Retry-After')) };
          return r.json().then(function (body) { return { body: body }; });
        })
        .then(function (res) {
          if (!res || mine !== seq) return;
          show(e.latlng, res.fail || buildContent(doc, res.body));
        })
        .catch(function () {
          if (mine === seq) show(e.latlng, buildFailure(doc, 0));
        });
    }

    function onOverlayRemove(e) {
      if (!e || e.layer !== layer()) return;
      abort();
      if (popup) map.closePopup(popup);
    }

    map.on('preclick', onPreclick);
    map.on('click', onClick);
    map.on('overlayremove', onOverlayRemove);
    return {
      detach: function () {
        abort();
        map.off('preclick', onPreclick);
        map.off('click', onClick);
        map.off('overlayremove', onOverlayRemove);
        if (popup) map.closePopup(popup);
      }
    };
  }

  global.HeritageInfo = { attach: attach, buildContent: buildContent, inBounds: inBounds };
})(typeof window !== 'undefined' ? window : this);
