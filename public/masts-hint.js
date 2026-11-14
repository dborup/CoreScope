/* masts-hint.js — #724 M4: the interference hint in the terrain profile.
 *
 * When an end of the measured path lies within HINT_DISTANCE_M (500 m) of
 * an antenna site with a 700, 800 or 900 MHz entry, the profile panel gets
 * one more chip beside the verdict, and a line under the detail text that
 * says what it means. Within HINT_STRONG_M (100 m) it says the transmitter
 * may be on the same building or mast. The wording is a hint and nothing
 * more: the source publishes no height or power, so the check is distance
 * only, and it never says that interference will happen (owner decision 2).
 *
 * Only a published band of 700, 800 or 900 counts. An entry without a band
 * never triggers the hint, even where the server flags it (GSM-R with no
 * band): the hint names the band it is about.
 *
 * Data: no request of its own. The cells around each end (cellsNear, at
 * most four 2 km cells per end) are pinned in the map's antenna-sites
 * controller (public/masts-layer.js), so they come from the same
 * /api/masts?cell= grid, the same paced loader, the same bounded cell
 * cache and the same per-IP budget as the layer. A cell the layer already
 * holds costs nothing; an unchanged end asks for nothing.
 *
 * Self-contained so the profile panel (public/terrain-profile.js) needs no
 * change: map.js attaches it and calls update(a, b) where it draws the
 * path. The chip and the note are text only (textContent / title); no site
 * field is ever shown in them.
 */
(function () {
  'use strict';

  var EARTH_RADIUS_M = 6371000;   // as public/terrain-profile.js
  var BANDS_MHZ = [700, 800, 900];

  function constants() {
    var ML = window.MastsLayer || {};
    return { far: ML.HINT_DISTANCE_M || 500, strong: ML.HINT_STRONG_M || 100 };
  }

  /** Great-circle distance in metres (haversine). */
  function distanceM(lat1, lon1, lat2, lon2) {
    var r = Math.PI / 180;
    var dLat = (lat2 - lat1) * r, dLon = (lon2 - lon1) * r;
    var h = Math.sin(dLat / 2) * Math.sin(dLat / 2) +
      Math.cos(lat1 * r) * Math.cos(lat2 * r) * Math.sin(dLon / 2) * Math.sin(dLon / 2);
    return 2 * EARTH_RADIUS_M * Math.asin(Math.min(1, Math.sqrt(h)));
  }

  /** The 700/800/900 MHz bands of a site, sorted and distinct. */
  function relevantBands(site) {
    var out = [];
    var entries = (site && site.entries) || [];
    for (var i = 0; i < entries.length; i++) {
      var b = entries[i] && entries[i].bandMHz;
      if (typeof b === 'number' && BANDS_MHZ.indexOf(b) >= 0 && out.indexOf(b) < 0) out.push(b);
    }
    return out.sort(function (x, y) { return x - y; });
  }

  /** 'strong' | 'near' | null for a distance in whole metres. */
  function tierOf(d) {
    var c = constants();
    if (d <= c.strong) return 'strong';
    if (d <= c.far) return 'near';
    return null;
  }

  /**
   * The nearest site with a relevant band within the hint distance of
   * (lat, lon): { distanceM (whole metres), bands, tier }, or null.
   */
  function nearestFor(lat, lon, sites) {
    var best = null;
    for (var i = 0; i < (sites || []).length; i++) {
      var s = sites[i];
      if (!s || typeof s.lat !== 'number' || typeof s.lon !== 'number' || !isFinite(s.lat) || !isFinite(s.lon)) continue;
      var bands = relevantBands(s);
      if (!bands.length) continue;
      var d = Math.round(distanceM(lat, lon, s.lat, s.lon));
      if (best && d >= best.distanceM) continue;
      if (!tierOf(d)) continue;
      best = { distanceM: d, bands: bands, tier: tierOf(d) };
    }
    return best;
  }

  /**
   * The grid cells within the hint distance of (lat, lon), its own cell
   * first, inside cfg.cellExtent. 500 m against 2 km cells: at most four.
   */
  function cellsNear(lat, lon, cfg) {
    var p = window.MastsLayer.toUTM32(lat, lon);
    var s = cfg.cellSizeM, x = cfg.cellExtent, r = constants().far;
    var own = Math.floor(p.e / s) + ',' + Math.floor(p.n / s);
    var out = [];
    for (var e = Math.floor((p.e - r) / s); e <= Math.floor((p.e + r) / s); e++) {
      for (var n = Math.floor((p.n - r) / s); n <= Math.floor((p.n + r) / s); n++) {
        if (e < x[0] || e > x[2] || n < x[1] || n > x[3]) continue;
        var key = e + ',' + n;
        if (key === own) out.unshift(key); else out.push(key);
      }
    }
    return out;
  }

  function endLine(side, f) {
    var bands = f.bands.join('/');
    var line = side + ': ' + (bands.charAt(0) === '8' ? 'an ' : 'a ') + bands + ' MHz cellular transmitter about ' + f.distanceM + ' m away';
    return line + (f.tier === 'strong' ? ', possibly on the same building or mast.' : '.');
  }

  /**
   * The hint for both ends ({ a, b } of nearestFor results), or null.
   * { tier, label, detail }: the nearer end leads the chip.
   */
  function hintFor(found) {
    var a = found && found.a, b = found && found.b;
    if (!a && !b) return null;
    var lead = !b || (a && a.distanceM <= b.distanceM) ? a : b;
    var c = constants();
    var lines = [];
    if (a) lines.push(endLine('A', a));
    if (b) lines.push(endLine('B', b));
    lines.push('Within about ' + c.far + ' m, especially within ' + c.strong + ' m, a transmitter in these bands ' +
      'may desensitise an unfiltered 868 MHz receiver; a band-pass filter or another site may help. ' +
      'Based on distance only: the source publishes no height or power.');
    return { tier: lead.tier, label: 'Cell transmitter ~' + lead.distanceM + ' m', detail: lines.join(' ') };
  }

  /**
   * Shows `hint` in the profile panel, or hides it (null). The chip goes
   * into the title row right before the copy button, after the other
   * chips; the note right after the detail line. Built once per panel.
   */
  function render(panel, hint) {
    if (!panel) return;
    var head = panel.querySelector('.tp-head');
    var detail = panel.querySelector('.tp-detail');
    if (!head || !detail) return;
    var doc = panel.ownerDocument || document;
    var ui = panel._mcMastHint;
    if (!ui) {
      ui = panel._mcMastHint = {
        // A screen reader would read the chip and its neighbour as one
        // sentence; the profile's own chips use the same hidden stop.
        sep: doc.createElement('span'),
        chip: doc.createElement('span'),
        note: doc.createElement('p')
      };
      ui.sep.className = 'sr-only';
      ui.sep.textContent = '. ';
      ui.chip.setAttribute('data-testid', 'tp-mast-hint');
      ui.note.className = 'tp-mast-note';
      ui.note.setAttribute('data-testid', 'tp-mast-note');
      var copy = panel.querySelector('.tp-copy');
      head.insertBefore(ui.sep, copy);
      head.insertBefore(ui.chip, copy);
      detail.parentNode.insertBefore(ui.note, detail.nextSibling);
    }
    var show = !!hint;
    ui.sep.hidden = ui.chip.hidden = ui.note.hidden = !show;
    ui.chip.className = 'tp-chip tp-chip-mast tp-chip-mast-' + (show ? hint.tier : 'none');
    ui.chip.setAttribute('data-tier', show ? hint.tier : '');
    ui.chip.textContent = show ? hint.label : '';
    ui.chip.title = show ? hint.detail : '';
    ui.note.textContent = show ? hint.detail : '';
  }

  /**
   * Binds the hint to one map's profile tool. opts: { map, cfg (the
   * validated masts block), panel: () => the profile panel or null }.
   * Returns { update(a, b), clear() } with a and b as [lat, lon], or null
   * when the layer is not offered.
   */
  function attach(opts) {
    var ML = window.MastsLayer;
    if (!opts || !opts.map || !opts.cfg || !ML || typeof ML.controller !== 'function') return null;
    var map = opts.map, cfg = opts.cfg;
    var ends = null, key = '', cells = { a: [], b: [] };

    function sitesOf(keys) {
      var ctl = ML.controller(map, cfg), out = [];
      for (var i = 0; i < keys.length; i++) {
        var d = ctl.data(keys[i]);
        if (d && d.sites) out = out.concat(d.sites);
      }
      return out;
    }

    function recompute() {
      var panel = opts.panel();
      if (!ends) { render(panel, null); return; }
      render(panel, hintFor({
        a: nearestFor(ends.a[0], ends.a[1], sitesOf(cells.a)),
        b: nearestFor(ends.b[0], ends.b[1], sitesOf(cells.b))
      }));
    }

    return {
      update: function (a, b) {
        if (!a || !b) { this.clear(); return; }
        var k = a[0] + ',' + a[1] + ';' + b[0] + ',' + b[1];
        if (k === key) { recompute(); return; }
        key = k;
        ends = { a: a, b: b };
        cells = { a: cellsNear(a[0], a[1], cfg), b: cellsNear(b[0], b[1], cfg) };
        ML.controller(map, cfg).pin(cells.a.concat(cells.b), recompute);
        recompute();
      },
      clear: function () {
        if (key) ML.controller(map, cfg).pin([], null);
        key = '';
        ends = null;
        render(opts.panel(), null);
      }
    };
  }

  window.MastsHint = {
    BANDS_MHZ: BANDS_MHZ,
    get HINT_DISTANCE_M() { return constants().far; },
    get HINT_STRONG_M() { return constants().strong; },
    distanceM: distanceM,
    relevantBands: relevantBands,
    nearestFor: nearestFor,
    cellsNear: cellsNear,
    hintFor: hintFor,
    render: render,
    attach: attach
  };
})();
