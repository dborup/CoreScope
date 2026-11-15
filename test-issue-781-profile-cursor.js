/**
 * Unit tests (#781): the Path profile hover cursor —
 * public/terrain-profile-cursor.js, with chartGeometry from
 * public/terrain-profile.js.
 *
 * Both real modules are loaded with vm.createContext, as the other
 * terrain-profile suites do; nothing here copies production logic.
 *
 * Covered:
 *   1. x → sample: nearest sample by binary search, at both ends, the
 *      middle, outside the plot and inside a gap in the terrain;
 *   2. sample → lat/lon: the distance fraction interpolated in Web
 *      Mercator, the projection the map draws its straight line in, so the
 *      dot lies on that line; exact at A and B;
 *   3. the readout: distance, terrain, surface where present, clearance
 *      under the line of sight; a missing DSM, a terrain gap and a profile
 *      with no verdict;
 *   4. keyboard stepping: ←/→ one sample, Shift ten, clamped, and a first
 *      press from no position;
 *   5. the controller: one SVG line and one readout made once, moved in
 *      place; mouse leave hides, touch lift keeps; blur hides;
 *   6. perf: a burst of 500 moves inside one frame costs one frame request
 *      and at most one update, and never rebuilds the chart;
 *   7. teardown: detach removes every listener, the line, the readout and
 *      the pending frame, and tells the host the cursor is gone.
 *
 * Usage: node test-issue-781-profile-cursor.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
function near(a, b, tol, msg) {
  assert.ok(Math.abs(a - b) <= tol, (msg || '') + ' expected ' + b + ' ± ' + tol + ', got ' + a);
}

function load() {
  const win = { addEventListener() {}, removeEventListener() {} };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise, encodeURIComponent, decodeURIComponent,
    setTimeout, clearTimeout,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/antenna-height.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile-cursor.js', 'utf8'), ctx);
  return { TP: win.TerrainProfile, C: win.TerrainProfileCursor, win };
}

/* A profile of n samples over D metres. opts.gap = [from, to] nulls the
 * terrain there; opts.dsmGap the surface; opts.model 'dtm' drops the DSM. */
function profile(opts) {
  opts = opts || {};
  const n = opts.n || 11, D = opts.D || 1000;
  const samples = [];
  for (let i = 0; i < n; i++) {
    const d = D * i / (n - 1);
    const inGap = opts.gap && i >= opts.gap[0] && i <= opts.gap[1];
    const inDsmGap = opts.dsmGap && i >= opts.dsmGap[0] && i <= opts.dsmGap[1];
    const s = { d: d, dtm: inGap ? null : 20 + i };
    if (opts.model !== 'dtm') s.dsm = (inGap || inDsmGap) ? null : 26 + i;
    samples.push(s);
  }
  const data = {
    a: { lat: 56, lon: 10, groundM: 20, antennaM: 10 },
    b: { lat: 56.1, lon: 10.1, groundM: 20 + n - 1, antennaM: 10 },
    distanceM: D, model: opts.model || 'both',
    coverage: { state: opts.gap ? 'partial' : 'full', pct: 100 },
    samples: samples,
    physics: { lambdaM: 0.34474, k: 4 / 3 },
  };
  if (!opts.noVerdict) data.verdict = { state: 'clear', criterion: 0.6, worstClearanceM: 5, worstAtM: 500 };
  return data;
}

// ── A fake plot: the .tp-plot div with its SVG, recording every change ──

function fakePlot(rect) {
  const log = { adds: 0, removes: 0, setAttr: 0, rebuilds: 0 };
  const listeners = {};
  function node(tag) {
    const n = {
      tagName: tag, attrs: {}, children: [], parentNode: null, style: {}, hidden: false,
      _text: '',
      get textContent() { return this._text; },
      set textContent(v) { this._text = String(v); },
      setAttribute(k, v) { log.setAttr++; this.attrs[k] = String(v); },
      getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
      removeAttribute(k) { delete this.attrs[k]; },
      appendChild(c) { this.children.push(c); c.parentNode = this; return c; },
      removeChild(c) { this.children = this.children.filter(x => x !== c); c.parentNode = null; return c; },
      set innerHTML(v) { log.rebuilds++; }, // the cursor must never rebuild
    };
    return n;
  }
  const svg = node('svg');
  svg.appendChild(node('title')); // the chart's own content
  svg.getBoundingClientRect = () => rect || { left: 100, top: 0, width: 820, height: 180 };
  const plot = node('div');
  plot.appendChild(svg);
  plot.querySelector = (sel) => (sel === 'svg' ? svg : null);
  plot.addEventListener = (t, fn) => { log.adds++; (listeners[t] || (listeners[t] = [])).push(fn); };
  plot.removeEventListener = (t, fn) => {
    const l = listeners[t] || [];
    const i = l.indexOf(fn);
    if (i >= 0) { l.splice(i, 1); log.removes++; }
  };
  plot.fire = (t, ev) => (listeners[t] || []).slice().forEach(fn => fn(Object.assign({ preventDefault() { ev && (ev.prevented = true); } }, ev)));
  plot.live = () => Object.keys(listeners).reduce((s, k) => s + listeners[k].length, 0);
  const doc = {
    createElementNS: (ns, tag) => Object.assign(node(tag), { ns: ns }),
    createElement: (tag) => node(tag),
  };
  return { plot, svg, doc, log };
}

function fakeFrames() {
  let next = 1;
  const queue = new Map();
  return {
    raf: (fn) => { const id = next++; queue.set(id, fn); return id; },
    caf: (id) => { queue.delete(id); },
    pending: () => queue.size,
    requested: () => next - 1,
    flush: () => { const fns = Array.from(queue.values()); queue.clear(); fns.forEach(fn => fn(0)); },
  };
}

function attachOn(C, TP, data, extra) {
  const g = TP.chartGeometry(data);
  const f = fakePlot(extra && extra.rect);
  const frames = fakeFrames();
  const moves = [];
  const ctl = C.attach(Object.assign({
    plot: f.plot, doc: f.doc, geometry: g, data: data,
    onMove: (p) => moves.push(p), raf: frames.raf, caf: frames.caf,
  }, extra || {}));
  return { ctl, f, frames, moves, g };
}

const { TP, C } = load();
console.log('\n=== #781 Path profile hover cursor ===\n');

// ── 1. x → sample ──────────────────────────────────────────────────────

test('nearestIndex: ends, middle, outside the plot and between samples', () => {
  const xs = [46, 121.8, 197.6, 273.4, 349.2, 425];
  assert.strictEqual(C.nearestIndex(xs, 46), 0);
  assert.strictEqual(C.nearestIndex(xs, 425), 5);
  assert.strictEqual(C.nearestIndex(xs, -50), 0, 'left of the plot clamps to A');
  assert.strictEqual(C.nearestIndex(xs, 9999), 5, 'right of the plot clamps to B');
  assert.strictEqual(C.nearestIndex(xs, 197.6), 2);
  assert.strictEqual(C.nearestIndex(xs, 159), 1, 'just under halfway rounds down');
  assert.strictEqual(C.nearestIndex(xs, 160), 2, 'just over halfway rounds up');
  assert.strictEqual(C.nearestIndex([], 5), -1);
  assert.strictEqual(C.nearestIndex([7], 500), 0);
});

test('nearestIndex agrees with a linear scan on 1001 samples', () => {
  const g = TP.chartGeometry(profile({ n: 1001, D: 80000 }));
  for (let x = 30; x <= 830; x += 0.37) {
    let best = 0;
    for (let i = 1; i < g.x.length; i++) if (Math.abs(g.x[i] - x) < Math.abs(g.x[best] - x)) best = i;
    const got = C.nearestIndex(g.x, x);
    assert.ok(Math.abs(g.x[got] - x) === Math.abs(g.x[best] - x), 'x=' + x + ': ' + got + ' vs ' + best);
  }
});

test('a pointer over a terrain gap lands on the gap sample', () => {
  const data = profile({ n: 11, gap: [4, 6] });
  const g = TP.chartGeometry(data);
  const i = C.nearestIndex(g.x, g.x[5]);
  assert.strictEqual(i, 5);
  assert.strictEqual(C.readout(g, data, i).terrainM, null);
});

// ── 2. sample → lat/lon ─────────────────────────────────────────────────

test('latLonAt is exact at A and B', () => {
  const a = [56.15467, 10.2137], b = [56.15265, 10.21471];
  assert.deepStrictEqual(Array.from(C.latLonAt(a, b, 0)), a);
  assert.deepStrictEqual(Array.from(C.latLonAt(a, b, 1)), b);
});

test('latLonAt interpolates in Web Mercator, on the line the map draws', () => {
  // Reference values from the spherical-Mercator inverse (EPSG:3857),
  // computed independently: the midpoint of 0°…60° is asin(1/√3) = 35.264°,
  // not the arithmetic 30°.
  const m = C.latLonAt([0, 0], [60, 20], 0.5);
  near(m[0], 35.26438968, 1e-7, 'lat');
  near(m[1], 10, 1e-12, 'lon is linear');
  near(C.latLonAt([56, 10], [57, 11], 0.5)[0], 56.50329638, 1e-7, 'Danish midpoint');
  near(C.latLonAt([56, 10], [57, 11], 0.25)[0], 56.25247533, 1e-7, 'quarter');
  near(C.latLonAt([56, 10], [57, 11], 0.25)[1], 10.25, 1e-12);
});

test('fraction is the sample distance over the path length', () => {
  const data = profile({ n: 5, D: 1200 });
  const g = TP.chartGeometry(data);
  assert.strictEqual(C.fraction(g, 0), 0);
  assert.strictEqual(C.fraction(g, 2), 0.5);
  assert.strictEqual(C.fraction(g, 4), 1);
});

// ── 3. readout ─────────────────────────────────────────────────────────

test('readout: distance, terrain, surface and clearance', () => {
  const data = profile({ n: 11, D: 1000 });
  const g = TP.chartGeometry(data);
  const r = C.readout(g, data, 5);
  assert.strictEqual(r.distanceM, 500);
  assert.strictEqual(r.terrainM, 25);
  assert.strictEqual(r.surfaceM, 31);
  // LOS at 500 m: 30 + (40 - 30) / 2 = 35 m, terrain plus bulge = 25 + 0.0147.
  near(r.clearanceM, 35 - 25 - 500 * 500 / (2 * (4 / 3) * 6371000), 0.02, 'terrain clearance');
  near(r.surfaceClearanceM, 35 - 31 - 500 * 500 / (2 * (4 / 3) * 6371000), 0.02, 'surface clearance');
  assert.deepStrictEqual(Array.from(r.lines), ['500 m from A', 'Terrain 25.0 m', 'Surface 31.0 m', 'Clearance 10.0 m (surface 4.0 m)']);
  assert.strictEqual(r.text, '500 m from A. Terrain 25.0 m. Surface 31.0 m. Clearance 10.0 m (surface 4.0 m)');
});

test('readout: kilometres past 1 km, a negative clearance carries a minus sign', () => {
  const data = profile({ n: 11, D: 20000 });
  data.samples[3].dtm = 200;
  data.samples[3].dsm = 210;
  const g = TP.chartGeometry(data);
  const r = C.readout(g, data, 3);
  assert.strictEqual(r.lines[0], '6.00 km from A');
  assert.ok(r.clearanceM < 0);
  assert.ok(/^Clearance −\d+\.\d m \(surface −\d+\.\d m\)$/.test(r.lines[3]), r.lines[3]);
});

test('readout: a terrain-only profile has no surface line', () => {
  const data = profile({ n: 11, model: 'dtm' });
  const g = TP.chartGeometry(data);
  const r = C.readout(g, data, 5);
  assert.strictEqual(r.surfaceM, null);
  assert.strictEqual(r.surfaceClearanceM, null);
  assert.strictEqual(r.lines.length, 3);
  assert.strictEqual(r.lines[2], 'Clearance 10.0 m');
});

test('readout: a missing DSM sample says so; a terrain gap has no clearance', () => {
  const data = profile({ n: 11, dsmGap: [5, 5], gap: [8, 8] });
  const g = TP.chartGeometry(data);
  const r = C.readout(g, data, 5);
  assert.strictEqual(r.surfaceM, null);
  assert.strictEqual(r.lines[2], 'Surface: no data');
  assert.strictEqual(r.lines[3], 'Clearance 10.0 m');
  const gap = C.readout(g, data, 8);
  assert.strictEqual(gap.terrainM, null);
  assert.strictEqual(gap.clearanceM, null);
  assert.deepStrictEqual(Array.from(gap.lines), ['800 m from A', 'Terrain: no data', 'Surface: no data', 'Clearance: unknown']);
});

test('readout: a profile with no verdict still reads every sample', () => {
  const data = profile({ n: 11, noVerdict: true });
  const g = TP.chartGeometry(data);
  const r = C.readout(g, data, 2);
  assert.strictEqual(r.terrainM, 22);
  assert.ok(isFinite(r.clearanceM));
});

// ── 4. keyboard stepping ────────────────────────────────────────────────

test('step: one sample, ten with Shift, clamped; first press from nothing', () => {
  assert.strictEqual(C.step(5, 100, 1, false), 6);
  assert.strictEqual(C.step(5, 100, -1, false), 4);
  assert.strictEqual(C.step(5, 100, 1, true), 15);
  assert.strictEqual(C.step(5, 100, -1, true), 0);
  assert.strictEqual(C.step(98, 100, 1, true), 99);
  assert.strictEqual(C.step(0, 100, -1, false), 0);
  assert.strictEqual(C.step(null, 100, 1, false), 0, '→ from nothing starts at A');
  assert.strictEqual(C.step(null, 100, -1, false), 99, '← from nothing starts at B');
});

// ── 5. controller ──────────────────────────────────────────────────────

test('attach makes one dashed line and one readout, hidden, and a focusable plot', () => {
  const t = attachOn(C, TP, profile());
  const line = t.f.svg.children.filter(c => c.attrs.class === 'tp-cursor-line');
  assert.strictEqual(line.length, 1);
  assert.strictEqual(line[0].ns, 'http://www.w3.org/2000/svg');
  assert.ok(line[0].attrs['stroke-dasharray'], 'dashed');
  assert.strictEqual(line[0].attrs.stroke, 'var(--tp-cursor)');
  assert.strictEqual(line[0].attrs.visibility, 'hidden');
  const ro = t.f.plot.children.filter(c => c.attrs.class === 'tp-cursor-readout');
  assert.strictEqual(ro.length, 1);
  assert.strictEqual(ro[0].attrs['aria-live'], 'polite');
  assert.strictEqual(ro[0].hidden, true);
  assert.strictEqual(t.f.plot.attrs.tabindex, '0');
  assert.strictEqual(t.f.log.rebuilds, 0);
});

test('mouse move: moves the line to the sample and reports the fraction', () => {
  const t = attachOn(C, TP, profile({ n: 11 }));
  // Sample 5 sits at viewBox x = 46 + 758 * 0.5 = 425; the svg is 820 px
  // wide from left 100, so the client x is 100 + 425.
  t.f.plot.fire('pointermove', { clientX: 525, pointerType: 'mouse' });
  assert.strictEqual(t.moves.length, 0, 'nothing before the frame');
  t.frames.flush();
  const line = t.f.svg.children[1];
  assert.strictEqual(line.attrs.x1, String(t.g.x[5]));
  assert.strictEqual(line.attrs.x2, String(t.g.x[5]));
  assert.strictEqual(line.attrs.visibility, 'visible');
  assert.strictEqual(t.moves.length, 1);
  assert.strictEqual(t.moves[0].index, 5);
  assert.strictEqual(t.moves[0].t, 0.5);
  assert.strictEqual(t.ctl.index(), 5);
  const ro = t.f.plot.children[1];
  assert.strictEqual(ro.hidden, false);
  assert.ok(/^500 m from A/.test(ro.attrs['aria-label'] || ro.children.map(c => c.textContent).join('. ')));
});

test('a pointer in a stretched svg maps through its real width', () => {
  // The viewBox is stretched to a 360 px phone: viewBox x 425 is 186.6 px in.
  const t = attachOn(C, TP, profile({ n: 11 }), { rect: { left: 10, top: 0, width: 360, height: 180 } });
  t.f.plot.fire('pointermove', { clientX: 10 + 425 * 360 / 820, pointerType: 'mouse' });
  t.frames.flush();
  assert.strictEqual(t.ctl.index(), 5);
});

test('mouse leave hides the line, the readout and the map dot', () => {
  const t = attachOn(C, TP, profile());
  t.f.plot.fire('pointermove', { clientX: 525, pointerType: 'mouse' });
  t.frames.flush();
  t.f.plot.fire('pointerleave', { pointerType: 'mouse' });
  t.frames.flush();
  assert.strictEqual(t.f.svg.children[1].attrs.visibility, 'hidden');
  assert.strictEqual(t.f.plot.children[1].hidden, true);
  assert.strictEqual(t.moves[t.moves.length - 1], null);
  assert.strictEqual(t.ctl.index(), null);
});

test('touch: a drag moves the cursor and it stays where the finger lifts', () => {
  const t = attachOn(C, TP, profile({ n: 11 }));
  t.f.plot.fire('pointerdown', { clientX: 100 + t.g.x[2], pointerType: 'touch' });
  t.frames.flush();
  assert.strictEqual(t.ctl.index(), 2);
  t.f.plot.fire('pointermove', { clientX: 100 + t.g.x[7], pointerType: 'touch' });
  t.frames.flush();
  assert.strictEqual(t.ctl.index(), 7);
  t.f.plot.fire('pointerup', { pointerType: 'touch' });
  t.f.plot.fire('pointerleave', { pointerType: 'touch' }); // follows a lift on touch
  t.frames.flush();
  assert.strictEqual(t.ctl.index(), 7, 'kept after the lift');
  assert.strictEqual(t.f.svg.children[1].attrs.visibility, 'visible');
  // The next tap moves it.
  t.f.plot.fire('pointerdown', { clientX: 100 + t.g.x[9], pointerType: 'touch' });
  t.frames.flush();
  assert.strictEqual(t.ctl.index(), 9);
});

test('touch: a cancelled drag (the panel scrolled) keeps the cursor', () => {
  const t = attachOn(C, TP, profile({ n: 11 }));
  t.f.plot.fire('pointerdown', { clientX: 100 + t.g.x[3], pointerType: 'touch' });
  t.f.plot.fire('pointercancel', { pointerType: 'touch' });
  t.frames.flush();
  assert.strictEqual(t.ctl.index(), 3);
});

test('keyboard: ←/→ step one sample, Shift ten, and default scrolling is stopped', () => {
  const t = attachOn(C, TP, profile({ n: 31 }));
  const ev = { key: 'ArrowRight' };
  t.f.plot.fire('keydown', ev);
  t.frames.flush();
  assert.ok(ev.prevented, 'preventDefault');
  assert.strictEqual(t.ctl.index(), 0);
  t.f.plot.fire('keydown', { key: 'ArrowRight' }); t.frames.flush();
  assert.strictEqual(t.ctl.index(), 1);
  t.f.plot.fire('keydown', { key: 'ArrowRight', shiftKey: true }); t.frames.flush();
  assert.strictEqual(t.ctl.index(), 11);
  t.f.plot.fire('keydown', { key: 'ArrowLeft' }); t.frames.flush();
  assert.strictEqual(t.ctl.index(), 10);
  // Two presses inside one frame both count.
  t.f.plot.fire('keydown', { key: 'ArrowRight' });
  t.f.plot.fire('keydown', { key: 'ArrowRight' });
  t.frames.flush();
  assert.strictEqual(t.ctl.index(), 12);
  const other = { key: 'a' };
  t.f.plot.fire('keydown', other);
  assert.ok(!other.prevented, 'other keys pass through');
});

test('blur hides the cursor', () => {
  const t = attachOn(C, TP, profile());
  t.f.plot.fire('keydown', { key: 'ArrowRight' }); t.frames.flush();
  t.f.plot.fire('blur', {}); t.frames.flush();
  assert.strictEqual(t.ctl.index(), null);
  assert.strictEqual(t.moves[t.moves.length - 1], null);
});

// ── 6. perf ─────────────────────────────────────────────────────────────

test('perf: 500 moves in one frame → one frame request, ≤ 1 update, no rebuild', () => {
  const t = attachOn(C, TP, profile({ n: 1001, D: 80000 }));
  const setBefore = t.f.log.setAttr;
  for (let i = 0; i < 500; i++) t.f.plot.fire('pointermove', { clientX: 100 + 46 + i * 1.5, pointerType: 'mouse' });
  assert.strictEqual(t.frames.requested(), 1, 'frames requested during the burst');
  assert.strictEqual(t.moves.length, 0);
  t.frames.flush();
  assert.strictEqual(t.moves.length, 1, 'updates in that frame');
  assert.ok(t.f.log.setAttr - setBefore <= 4, 'attribute writes for the one update: ' + (t.f.log.setAttr - setBefore));
  assert.strictEqual(t.f.log.rebuilds, 0, 'chart rebuilds');
  assert.strictEqual(t.f.svg.children.length, 2, 'no new SVG nodes');
  // The same sample again costs nothing at all.
  t.f.plot.fire('pointermove', { clientX: 100 + 46 + 499 * 1.5, pointerType: 'mouse' });
  t.frames.flush();
  assert.strictEqual(t.moves.length, 1, 'no update for an unchanged sample');
});

test('perf: 10 frames of 50 moves each → exactly one update per frame', () => {
  const t = attachOn(C, TP, profile({ n: 1001, D: 80000 }));
  for (let f = 0; f < 10; f++) {
    for (let i = 0; i < 50; i++) t.f.plot.fire('pointermove', { clientX: 150 + f * 60 + i, pointerType: 'mouse' });
    t.frames.flush();
  }
  assert.strictEqual(t.frames.requested(), 10);
  assert.strictEqual(t.moves.length, 10);
});

// ── 7. teardown ─────────────────────────────────────────────────────────

test('detach removes every listener, node and pending frame, and clears the dot', () => {
  const t = attachOn(C, TP, profile());
  assert.ok(t.f.plot.live() > 0);
  t.f.plot.fire('pointermove', { clientX: 525, pointerType: 'mouse' });
  t.frames.flush();
  t.f.plot.fire('pointermove', { clientX: 600, pointerType: 'mouse' }); // a frame in flight
  assert.strictEqual(t.frames.pending(), 1);
  t.ctl.detach();
  assert.strictEqual(t.f.plot.live(), 0, 'listeners left');
  assert.strictEqual(t.f.log.adds, t.f.log.removes);
  assert.strictEqual(t.frames.pending(), 0, 'frame cancelled');
  assert.strictEqual(t.f.svg.children.length, 1, 'line removed');
  assert.strictEqual(t.f.plot.children.length, 1, 'readout removed');
  assert.strictEqual(t.moves[t.moves.length - 1], null, 'host told to hide the dot');
  t.ctl.detach(); // idempotent
  assert.strictEqual(t.f.log.adds, t.f.log.removes);
});

test('detach without a shown cursor does not call the host', () => {
  const t = attachOn(C, TP, profile());
  t.ctl.detach();
  assert.strictEqual(t.moves.length, 0);
});

test('attach refuses a plot without geometry', () => {
  const f = fakePlot();
  assert.strictEqual(C.attach({ plot: f.plot, doc: f.doc, geometry: null, data: {} }), null);
  assert.strictEqual(f.log.adds, 0);
});

// ── terrain-profile.js hands out the metres the readout needs ───────────

test('chartGeometry exposes the line of sight and the surface in metres', () => {
  const data = profile({ n: 11 });
  const g = TP.chartGeometry(data);
  assert.strictEqual(g.losZ.length, 11);
  assert.strictEqual(g.losZ[0], 30);
  assert.strictEqual(g.losZ[10], 40);
  assert.strictEqual(g.surfaceZ.length, 11);
  assert.ok(Math.abs(g.surfaceZ[5] - 31) < 0.02);
  assert.strictEqual(TP.chartGeometry(profile({ model: 'dtm' })).surfaceZ, null);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
process.exit(failed ? 1 : 0);
