/* terrain-profile-cursor.js — #781: the Path profile's hover cursor.
 *
 * Pointing at the profile chart draws a dashed vertical line and a small
 * readout at that distance (distance from A, terrain, surface where the
 * profile has it, with its class where the profile tells buildings from
 * trees (#730 M3), clearance under the line of sight), and tells the host
 * which fraction of the path that is, so the Map page can put a dot on its
 * profile line at the same point.
 *
 * Kept apart from terrain-profile.js on purpose: the chart there is built
 * as one SVG string, and this only adds two nodes to it after the fact and
 * moves them. It never redraws the chart.
 *
 * Performance: input is coalesced to one update per animation frame; an
 * update is one binary search over the sample x positions (≤ 1001 of
 * them), a few attribute writes on one existing <line> and one existing
 * readout, and one host callback. A move that lands on the sample already
 * shown does nothing at all.
 *
 * Exposes window.TerrainProfileCursor; no dependencies. */
(function (global) {
  'use strict';

  var SVG_NS = 'http://www.w3.org/2000/svg';
  // Shift + ←/→ moves this many samples at a time.
  var BIG_STEP = 10;
  var R = 6378137; // EPSG:3857 sphere, the projection Leaflet draws in
  var DEG = Math.PI / 180;

  /* nearestIndex is the sample whose x is nearest to x, by binary search
   * over xs (ascending). Outside the plot it clamps to A or B. -1 when
   * there are no samples. */
  function nearestIndex(xs, x) {
    var n = xs ? xs.length : 0;
    if (!n) return -1;
    if (!(x > xs[0])) return 0;
    if (!(x < xs[n - 1])) return n - 1;
    var lo = 0, hi = n - 1;
    while (hi - lo > 1) {
      var mid = (lo + hi) >> 1;
      if (xs[mid] <= x) lo = mid; else hi = mid;
    }
    return (x - xs[lo] <= xs[hi] - x) ? lo : hi;
  }

  function mercY(lat) { return R * Math.log(Math.tan(Math.PI / 4 + lat * DEG / 2)); }
  function mercLat(y) { return (2 * Math.atan(Math.exp(y / R)) - Math.PI / 2) / DEG; }

  /* latLonAt is the point at fraction t of the straight line from a to b
   * ([lat, lon] each) as the map draws it: Leaflet projects both ends to
   * Web Mercator and joins them there, so the point is interpolated in the
   * same projection and lies exactly on the drawn line. The server samples
   * a straight line in UTM 32; over the ≤ 80 km a profile may span the two
   * lines are well under a metre apart at Danish latitudes. */
  function latLonAt(a, b, t) {
    if (t <= 0) return [a[0], a[1]];
    if (t >= 1) return [b[0], b[1]];
    var ya = mercY(a[0]), yb = mercY(b[0]);
    return [mercLat(ya + (yb - ya) * t), a[1] + (b[1] - a[1]) * t];
  }

  /* fraction is how far along the path sample i is, 0 at A and 1 at B. */
  function fraction(g, i) {
    var D = g.domain.dMax;
    return D > 0 ? Math.min(1, Math.max(0, g.d[i] / D)) : 0;
  }

  function num(v) { return (v === null || v === undefined || !isFinite(Number(v))) ? null : Number(v); }
  function signed(v) { return (v < 0 ? '−' : '') + Math.abs(v).toFixed(1); }
  // #839 §8: one rule for distances along the path, terrain-profile.js's.
  function fmtDistance(m) { return global.TerrainProfile.fmtDistance(m); }

  /* readout builds the words for sample i: heights are the DHM values
   * above sea level as the server sent them; clearance is the line of
   * sight above the ground (or the surface) with the earth bulge added,
   * which is how the chart draws it and how the server judges it. */
  function readout(g, data, i) {
    var s = (data.samples && data.samples[i]) || {};
    var both = data.model === 'both';
    var out = {
      distanceM: Math.round(Number(g.d[i]) * 100) / 100,
      terrainM: num(s.dtm),
      surfaceM: both ? num(s.dsm) : null,
      clearanceM: null,
      surfaceClearanceM: null,
      lines: [],
      text: ''
    };
    var los = g.losZ[i];
    if (out.terrainM !== null && g.terrainZ[i] !== null) out.clearanceM = Math.round((los - g.terrainZ[i]) * 100) / 100;
    if (out.surfaceM !== null && g.surfaceZ && g.surfaceZ[i] !== null) out.surfaceClearanceM = Math.round((los - g.surfaceZ[i]) * 100) / 100;

    out.lines.push(fmtDistance(out.distanceM) + ' from A');
    out.lines.push(out.terrainM === null ? 'Terrain: no data' : 'Terrain ' + out.terrainM.toFixed(1) + ' m');
    // #730 M3: the class at the sample, in terrain-profile.js's words.
    var cls = both && g.surfaceClass ? g.surfaceClass[i] : null;
    if (both) out.lines.push(out.surfaceM === null ? 'Surface: no data' : 'Surface ' + out.surfaceM.toFixed(1) + ' m' + (cls ? ' (' + cls + ')' : ''));
    var c = out.clearanceM === null ? 'Clearance: unknown' : 'Clearance ' + signed(out.clearanceM) + ' m';
    if (out.clearanceM !== null && out.surfaceClearanceM !== null) c += ' (surface ' + signed(out.surfaceClearanceM) + ' m)';
    out.lines.push(c);
    out.text = out.lines.join('. ');
    return out;
  }

  /* step is the keyboard: one sample per press, BIG_STEP with Shift,
   * clamped to the path. From no position → starts at A and ← at B. */
  function step(i, n, dir, shift) {
    if (i === null || i === undefined || i < 0) return dir > 0 ? 0 : n - 1;
    var next = i + dir * (shift ? BIG_STEP : 1);
    return Math.max(0, Math.min(n - 1, next));
  }

  /* attach wires one rendered chart. opts:
   *   plot      the .tp-plot element (holds the <svg> and the HTML labels)
   *   geometry  chartGeometry() of the profile drawn in it
   *   data      the profile response itself
   *   onMove    host callback: { index, t, distanceM } or null when hidden
   *   doc, raf, caf  seams for tests; default to the browser's
   * Returns { detach, index }, or null when there is nothing to point at. */
  function attach(opts) {
    var g = opts && opts.geometry;
    var plot = opts && opts.plot;
    if (!g || !plot || !g.x || !g.x.length) return null;
    var svg = plot.querySelector('svg');
    if (!svg) return null;
    var doc = opts.doc || global.document;
    var raf = opts.raf || function (fn) { return global.requestAnimationFrame(fn); };
    var caf = opts.caf || function (id) { global.cancelAnimationFrame(id); };
    var onMove = opts.onMove || function () {};
    var data = opts.data || {};
    var n = g.x.length;

    var line = doc.createElementNS(SVG_NS, 'line');
    line.setAttribute('class', 'tp-cursor-line');
    line.setAttribute('y1', String(g.plot.y));
    line.setAttribute('y2', String(g.baseY));
    line.setAttribute('x1', String(g.x[0]));
    line.setAttribute('x2', String(g.x[0]));
    line.setAttribute('stroke', 'var(--tp-cursor)');
    line.setAttribute('stroke-width', '1.5');
    line.setAttribute('stroke-dasharray', '4 3');
    // The viewBox is stretched to the panel; keep the dash 1.5 px wide.
    line.setAttribute('vector-effect', 'non-scaling-stroke');
    line.setAttribute('pointer-events', 'none');
    line.setAttribute('visibility', 'hidden');
    line.setAttribute('data-testid', 'tp-cursor-line');
    svg.appendChild(line);

    // The readout: one box with a fixed set of rows, written with
    // textContent only. Polite, so a screen reader hears where the
    // keyboard put the cursor without being interrupted.
    var box = doc.createElement('div');
    box.setAttribute('class', 'tp-cursor-readout');
    box.setAttribute('data-testid', 'tp-cursor-readout');
    box.setAttribute('aria-live', 'polite');
    box.setAttribute('aria-atomic', 'true');
    box.hidden = true;
    var rows = [];
    for (var r = 0; r < 4; r++) { rows.push(box.appendChild(doc.createElement('div'))); }
    plot.appendChild(box);

    plot.setAttribute('tabindex', '0');
    plot.setAttribute('role', 'group');
    plot.setAttribute('aria-label', 'Profile cursor. Point at the chart, or use the left and right arrow keys, to see that point on the map.');

    var cur = null;       // the sample shown, or null
    var frame = null;     // the pending animation frame
    var pending = null;   // { x } | { keys: [{ dir, shift }] } | 'hide'
    var dragging = false; // a touch is down on the chart
    var gone = false;

    function schedule() { if (frame === null) frame = raf(flush); }

    function flush() {
      frame = null;
      var p = pending; pending = null;
      if (gone || p === null) return;
      if (p === 'hide') { hide(); return; }
      if (p.keys) {
        var i = cur;
        for (var j = 0; j < p.keys.length; j++) i = step(i, n, p.keys[j].dir, p.keys[j].shift);
        show(i);
        return;
      }
      var rect = svg.getBoundingClientRect();
      if (!(rect.width > 0)) return;
      show(nearestIndex(g.x, (p.x - rect.left) / rect.width * g.view.w));
    }

    function show(i) {
      if (i === cur || i < 0) return;
      cur = i;
      var x = String(g.x[i]);
      line.setAttribute('x1', x);
      line.setAttribute('x2', x);
      if (line.getAttribute('visibility') !== 'visible') line.setAttribute('visibility', 'visible');
      var ro = readout(g, data, i);
      for (var j = 0; j < rows.length; j++) {
        rows[j].textContent = ro.lines[j] || '';
        rows[j].hidden = j >= ro.lines.length;
      }
      // Keep the box on the plot: right of the line in the left half, left
      // of it in the right half.
      var pct = g.x[i] / g.view.w * 100;
      box.style.left = pct.toFixed(3) + '%';
      box.style.transform = pct > 50 ? 'translateX(calc(-100% - 6px))' : 'translateX(6px)';
      box.hidden = false;
      onMove({ index: i, t: fraction(g, i), distanceM: ro.distanceM });
    }

    function hide() {
      if (cur === null) return;
      cur = null;
      line.setAttribute('visibility', 'hidden');
      box.hidden = true;
      onMove(null);
    }

    function onPointerMove(e) {
      if (e.pointerType === 'touch' && !dragging) return;
      pending = { x: e.clientX };
      schedule();
    }
    function onPointerDown(e) {
      if (e.pointerType === 'touch') dragging = true;
      pending = { x: e.clientX };
      schedule();
    }
    function onPointerUp() { dragging = false; }
    // A touch keeps the cursor where the finger lifted; the browser sends
    // a leave right after the lift, so only a mouse or pen leave hides it.
    function onPointerLeave(e) {
      if (e.pointerType === 'touch') return;
      pending = 'hide';
      schedule();
    }
    function onKeyDown(e) {
      var dir = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
      if (!dir) return;
      e.preventDefault();
      if (!pending || !pending.keys) pending = { keys: [] };
      pending.keys.push({ dir: dir, shift: !!e.shiftKey });
      schedule();
    }
    function onBlur() { pending = 'hide'; schedule(); }

    var handlers = [
      ['pointermove', onPointerMove], ['pointerdown', onPointerDown],
      ['pointerup', onPointerUp], ['pointercancel', onPointerUp],
      ['pointerleave', onPointerLeave], ['keydown', onKeyDown], ['blur', onBlur]
    ];
    handlers.forEach(function (h) { plot.addEventListener(h[0], h[1]); });

    return {
      index: function () { return cur; },
      detach: function () {
        if (gone) return;
        gone = true;
        if (frame !== null) { caf(frame); frame = null; }
        pending = null;
        handlers.forEach(function (h) { plot.removeEventListener(h[0], h[1]); });
        if (line.parentNode) line.parentNode.removeChild(line);
        if (box.parentNode) box.parentNode.removeChild(box);
        if (cur !== null) { cur = null; onMove(null); }
      }
    };
  }

  global.TerrainProfileCursor = {
    nearestIndex: nearestIndex,
    latLonAt: latLonAt,
    fraction: fraction,
    readout: readout,
    step: step,
    attach: attach,
    BIG_STEP: BIG_STEP
  };
})(typeof window !== 'undefined' ? window : this);
